// Package inventory groups a cluster's pods into workloads (Deployments,
// StatefulSets, DaemonSets, and bare pods), computing replica health and
// restart history, and attaches detector findings to the owning workload.
package inventory

import (
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// PodRow is one pod under a workload.
type PodRow struct {
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	Ready       string `json:"ready"` // "1/1"
	Restarts    int    `json:"restarts"`
	LastRestart string `json:"lastRestart,omitempty"` // RFC3339 UTC, "" if none
	Node        string `json:"node"`
	IP          string `json:"ip"`
	Age         string `json:"age"`
	Image       string `json:"image"`
}

// Workload is one controller (or bare pod) and its aggregated health.
type Workload struct {
	Namespace   string             `json:"namespace"`
	Name        string             `json:"name"`
	Kind        string             `json:"kind"` // Deployment | StatefulSet | DaemonSet | ReplicaSet | Job | CronJob | Pod
	Desired     int                `json:"desired"`
	Ready       int                `json:"ready"`
	Status      string             `json:"status"` // Running | Degraded | Scaled Down | Complete | Failed | Pending | Active(N) | Idle
	Restarts    int                `json:"restarts"`
	LastRestart string             `json:"lastRestart,omitempty"`
	Image       string             `json:"image"`
	Pods        []PodRow           `json:"pods"`
	Findings    []diagnose.Finding `json:"findings,omitempty"`
	PodsOmitted int                `json:"podsOmitted,omitempty"`
	Schedule    string             `json:"schedule,omitempty"`
}

// Flagged reports whether the workload needs attention.
func (w Workload) Flagged() bool {
	return len(w.Findings) > 0 || w.Ready < w.Desired || w.Status == "Failed"
}

func termTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// HumanSince formats an RFC3339 timestamp as a relative age like "20d ago".
// Returns "" for an empty or unparseable timestamp.
func HumanSince(rfc3339 string, now time.Time) string {
	if rfc3339 == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return ""
	}
	return humanAge(t, now) + " ago"
}

func humanAge(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

func controllerOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

func podRestarts(p corev1.Pod) (int, metav1.Time) {
	total := 0
	var last metav1.Time
	for _, cs := range p.Status.ContainerStatuses {
		total += int(cs.RestartCount)
		if term := cs.LastTerminationState.Terminated; term != nil {
			if last.IsZero() || term.FinishedAt.After(last.Time) {
				last = term.FinishedAt
			}
		}
	}
	return total, last
}

func podReady(p corev1.Pod) string {
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
}

func podIsReady(p corev1.Pod) bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func podImage(p corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Image
	}
	return ""
}

func workloadStatus(ready, desired int) string {
	if desired == 0 {
		return "Scaled Down"
	}
	if ready >= desired {
		return "Running"
	}
	return "Degraded"
}

// Inputs are the raw lists Assemble consumes.
type Inputs struct {
	Pods         []corev1.Pod
	Deployments  []appsv1.Deployment
	ReplicaSets  []appsv1.ReplicaSet
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
	Jobs         []batchv1.Job
	CronJobs     []batchv1.CronJob
}

// jobStatus maps a Job's conditions/counts to a status string.
func jobStatus(j batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Status == corev1.ConditionTrue {
			switch c.Type {
			case batchv1.JobFailed:
				return "Failed"
			case batchv1.JobComplete:
				return "Complete"
			}
		}
	}
	if j.Status.Active > 0 {
		return "Running"
	}
	return "Pending"
}

// cronJobStatus summarizes a CronJob by its active-job count.
func cronJobStatus(cj batchv1.CronJob) string {
	if n := len(cj.Status.Active); n > 0 {
		return fmt.Sprintf("Active(%d)", n)
	}
	return "Idle"
}

const jobPodCap = 3 // max pod rows shown per Job/CronJob workload

// Assemble groups pods into workloads, reads controller status for ready/desired,
// aggregates restarts, attaches findings, and returns workloads sorted
// flagged-first then by namespace/name.
func Assemble(in Inputs, findings []diagnose.Finding) []Workload {
	key := func(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

	workloads := map[string]*Workload{}
	controllerKeys := map[string]bool{}
	seed := func(kind, ns, name string, desired, ready int) {
		k := key(kind, ns, name)
		workloads[k] = &Workload{Namespace: ns, Name: name, Kind: kind, Desired: desired, Ready: ready}
		controllerKeys[k] = true
	}
	for _, d := range in.Deployments {
		desired := 1
		if d.Spec.Replicas != nil {
			desired = int(*d.Spec.Replicas)
		}
		seed("Deployment", d.Namespace, d.Name, desired, int(d.Status.ReadyReplicas))
	}
	for _, s := range in.StatefulSets {
		desired := 1
		if s.Spec.Replicas != nil {
			desired = int(*s.Spec.Replicas)
		}
		seed("StatefulSet", s.Namespace, s.Name, desired, int(s.Status.ReadyReplicas))
	}
	for _, ds := range in.DaemonSets {
		seed("DaemonSet", ds.Namespace, ds.Name, int(ds.Status.DesiredNumberScheduled), int(ds.Status.NumberReady))
	}

	// seedJobLike seeds a Job/CronJob workload with a controller-derived status
	// (and schedule), keeping Desired/Ready at 0.
	seedJobLike := func(kind, ns, name, status, schedule string) {
		k := key(kind, ns, name)
		workloads[k] = &Workload{Namespace: ns, Name: name, Kind: kind, Status: status, Schedule: schedule}
		controllerKeys[k] = true
	}
	for _, cj := range in.CronJobs {
		seedJobLike("CronJob", cj.Namespace, cj.Name, cronJobStatus(cj), cj.Spec.Schedule)
	}
	// jobToCronJob resolves a Job to its owning CronJob (namespaced); CronJob-owned
	// Jobs are NOT seeded as their own workloads (their pods roll up to the CronJob).
	jobToCronJob := map[string]string{}
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			jobToCronJob[j.Namespace+"/"+j.Name] = o.Name
			continue
		}
		seedJobLike("Job", j.Namespace, j.Name, jobStatus(j), "")
	}

	// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).
	rsToDeploy := map[string]string{}
	for _, rs := range in.ReplicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}

	podKey := map[string]string{}    // "ns/name" -> workload key
	derivedReady := map[string]int{} // ready-pod count for pod-derived workloads
	for _, p := range in.Pods {
		kind, name := "Pod", p.Name
		if o := controllerOwner(p.OwnerReferences); o != nil {
			switch o.Kind {
			case "ReplicaSet":
				if dep, ok := rsToDeploy[p.Namespace+"/"+o.Name]; ok {
					kind, name = "Deployment", dep
				} else {
					kind, name = "ReplicaSet", o.Name
				}
			case "Job":
				if cj, ok := jobToCronJob[p.Namespace+"/"+o.Name]; ok && controllerKeys[key("CronJob", p.Namespace, cj)] {
					kind, name = "CronJob", cj
				} else {
					kind, name = "Job", o.Name
				}
			default:
				kind, name = o.Kind, o.Name
			}
		}
		k := key(kind, p.Namespace, name)
		w, ok := workloads[k]
		if !ok {
			w = &Workload{Namespace: p.Namespace, Name: name, Kind: kind}
			workloads[k] = w
		}
		restarts, last := podRestarts(p)
		w.Restarts += restarts
		if lt := termTime(last); lt != "" && lt > w.LastRestart {
			w.LastRestart = lt
		}
		if w.Image == "" {
			w.Image = podImage(p)
		}
		if podIsReady(p) {
			derivedReady[k]++
		}
		w.Pods = append(w.Pods, PodRow{
			Name: p.Name, Phase: string(p.Status.Phase), Ready: podReady(p),
			Restarts: restarts, LastRestart: termTime(last),
			Node: p.Spec.NodeName, IP: p.Status.PodIP,
			Age: humanAge(p.CreationTimestamp.Time, time.Now()), Image: podImage(p),
		})
		podKey[p.Namespace+"/"+p.Name] = k
	}

	// Pods and findings come from the same scan snapshot, so every finding's
	// pod is present in podKey; an unmatched finding (none today) is dropped.
	for _, f := range findings {
		if k, ok := podKey[f.Pod]; ok {
			workloads[k].Findings = append(workloads[k].Findings, f)
		}
	}

	out := make([]Workload, 0, len(workloads))
	for k, w := range workloads {
		if !controllerKeys[k] {
			w.Desired = len(w.Pods)
			w.Ready = derivedReady[k]
		}
		if (w.Kind == "Job" || w.Kind == "CronJob") && len(w.Pods) > jobPodCap {
			w.PodsOmitted = len(w.Pods) - jobPodCap
			w.Pods = w.Pods[:jobPodCap]
		}
		if w.Status == "" {
			w.Status = workloadStatus(w.Ready, w.Desired)
		}
		out = append(out, *w)
	}
	sortWorkloads(out)
	return out
}

func sortWorkloads(ws []Workload) {
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].Flagged() != ws[j].Flagged() {
			return ws[i].Flagged() // flagged first
		}
		if ws[i].Namespace != ws[j].Namespace {
			return ws[i].Namespace < ws[j].Namespace
		}
		return ws[i].Name < ws[j].Name
	})
}
