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
	"github.com/imantaba/kubeagent/internal/safetext"
)

// PodRow is one pod under a workload.
type PodRow struct {
	Name string `json:"name"`
	// Phase is status.phase verbatim: Pending, Running, Succeeded, Failed or
	// Unknown. State is the kubectl-style display value computed beside it —
	// see podState for why both exist and which one a report should print.
	Phase       string `json:"phase"`
	State       string `json:"state,omitempty"`
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
	Namespace           string             `json:"namespace"`
	Name                string             `json:"name"`
	Kind                string             `json:"kind"` // Deployment | StatefulSet | DaemonSet | ReplicaSet | Job | CronJob | Pod
	Desired             int                `json:"desired"`
	Ready               int                `json:"ready"`
	Status              string             `json:"status"` // Running | Degraded | Scaled Down | Complete | Failed | Pending | Active(N) | Idle | Last run failed (set by batchhealth.Annotate)
	Restarts            int                `json:"restarts"`
	LastRestart         string             `json:"lastRestart,omitempty"`
	Image               string             `json:"image"`
	Pods                []PodRow           `json:"pods"`
	Findings            []diagnose.Finding `json:"findings,omitempty"`
	PodsOmitted         int                `json:"podsOmitted,omitempty"`
	Schedule            string             `json:"schedule,omitempty"`
	Priority            int                `json:"priority,omitempty"`            // 2 problem | 3 restart-only | 4 cron (set by Prioritize)
	NetworkPolicies     []string           `json:"networkPolicies,omitempty"`     // names of NPs selecting this workload's pods (hint; set by netpolicy.Annotate)
	Rollout             *RolloutChange     `json:"rollout,omitempty"`             // recent-rollout correlation (hint; set by rollout.Annotate)
	RootCause           string             `json:"rootCause,omitempty"`           // "node X (reason)", "registry Y (N workloads failing to pull)", or "PVC Z (reason)" — root-cause attribution (hint; set by rootcause.Annotate/AnnotatePVC/AnnotateRegistry)
	RootCauseTrace      []Hypothesis       `json:"rootCauseTrace,omitempty"`      // every root-cause candidate the attribution pass evaluated, whatever the verdict (set by the same three annotators; empty when no candidate existed)
	RootCauseConfidence string             `json:"rootCauseConfidence,omitempty"` // confidence of the RootCause attribution ("high" | "medium"; set by confidence.Annotate, empty when RootCause is)
}

// RolloutChange is a recent-rollout correlation for a flagged Deployment: what
// changed (revision, image) and when. Set by rollout.Annotate; nil when there is
// no recent rollout to report.
type RolloutChange struct {
	Revision  string `json:"revision"`
	Since     string `json:"since"`
	OldImage  string `json:"oldImage,omitempty"`
	NewImage  string `json:"newImage,omitempty"`
	Container string `json:"-"` // set by rollout.Annotate only when the matched container is not the template's first (matching/render only, not serialized)
}

// A Verdict says what the attribution pass concluded about one candidate
// cause. The vocabulary is closed: attributed (this candidate is the
// workload's RootCause), ruled_out (its evidence did not match this
// workload), outranked (its evidence matched, but precedence chose a
// stronger cause).
type Verdict string

const (
	VerdictAttributed Verdict = "attributed"
	VerdictRuledOut   Verdict = "ruled_out"
	VerdictOutranked  Verdict = "outranked"
)

// A Hypothesis is one candidate root cause the attribution pass evaluated for
// a workload, kept whatever the verdict was. The trace exists so an operator
// can see what was considered and rejected, not only what won: "ruled out"
// and "outranked" are answers, not omissions. Kind is one of "node", "pvc",
// "registry".
type Hypothesis struct {
	Cause   string  `json:"cause"`
	Kind    string  `json:"kind"`
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
	// Object is the candidate's bare object name — the node name, PVC name,
	// or registry host inside Cause — for local verdict mode's evidence
	// gather. Never marshalled: the eight JSON documents are a versioned
	// contract and this field is not part of any of them.
	Object string `json:"-"`
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
	return HumanAge(t, now) + " ago"
}

// HumanAge renders the gap between t and now as a compact duration such as
// "3d", "5h" or "12m". A negative gap (a clock skew, or an object stamped in
// the future) reads as zero rather than a negative string.
func HumanAge(t, now time.Time) string {
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

// PodRowFor projects one pod into the row shape a report renders. Assemble
// calls it for every pod it groups, and internal/mcp's inspect handler calls it
// for a pod it looked up directly — one implementation, so a pod row is the
// same shape whichever surface produced it.
//
// now is a parameter rather than a time.Now() call, so a caller holding an
// injected clock (the MCP server has one) gets a deterministic Age.
func PodRowFor(p corev1.Pod, now time.Time) PodRow {
	restarts, last := podRestarts(p)
	return PodRow{
		Name: p.Name, Phase: string(p.Status.Phase), State: podState(p), Ready: podReady(p),
		Restarts: restarts, LastRestart: termTime(last),
		Node: p.Spec.NodeName, IP: p.Status.PodIP,
		Age: HumanAge(p.CreationTimestamp.Time, now), Image: podImage(p),
	}
}

// podState renders what a pod is doing the way `kubectl get pods` does, from the
// containers rather than from status.phase. The two disagree often enough to
// matter: a pod whose container is in CrashLoopBackOff has phase Running, and
// one whose container cannot pull its image has phase Pending, so an operator
// reading a kubeagent row beside a kubectl row sees two different words for the
// same pod and has to work out which tool is wrong. Neither is; they answer
// different questions.
//
// The rule is deliberately smaller than kubectl's own column logic, which
// kubeagent has no reason to reimplement in full. Anything outside it falls
// through to the phase, which is what every row printed before this existed.
//
// A container's waiting and terminated reasons are API text the API server does
// not validate — the kubelet writes them — so this is the point at which they
// become kubeagent values, and safetext.Line is applied here rather than at any
// renderer. The emptiness test that decides whether a reason is worth naming
// runs on the *sanitized* value, not the raw one: a reason with no printable
// content is nothing to say, and testing the raw value would render "Init:"
// with nothing after it.
func podState(p corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "Terminating"
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if r := safetext.Line(containerReason(cs, true)); r != "" {
			return "Init:" + r
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if r := safetext.Line(containerReason(cs, false)); r != "" {
			return r
		}
	}
	return string(p.Status.Phase)
}

// containerReason returns the reason a container status is worth naming, or ""
// when it is not. An init container is worth naming only when it failed —
// exiting zero is how an init container succeeds, and the pod moves on — while a
// main container's terminated reason is worth naming whatever its exit code, so
// a finished pod reads "Completed" rather than "Succeeded".
func containerReason(cs corev1.ContainerStatus, isInit bool) string {
	if w := cs.State.Waiting; w != nil {
		return w.Reason
	}
	if t := cs.State.Terminated; t != nil {
		if isInit && t.ExitCode == 0 {
			return ""
		}
		return t.Reason
	}
	return ""
}

// WorkloadStatus renders a ready-versus-desired pair as the one status
// vocabulary every kubeagent surface uses. Assemble sets it for a workload it
// grouped; internal/mcp's inspect handler calls it for a ReplicaSet it looked
// up directly, so a ReplicaSet's status word means the same thing as a
// Deployment's.
func WorkloadStatus(ready, desired int) string {
	if desired == 0 {
		return "Scaled Down"
	}
	if ready >= desired {
		return "Running"
	}
	return "Degraded"
}

// terminalPodStatus reports the finished status of a pod-derived workload (a bare
// pod, or pods orphaned from a deleted controller) when every pod has reached a
// terminal phase. Such pods are done, not missing — a one-shot pod that exited 0
// is "Complete" and one that exited non-zero is "Failed", so a finished pod isn't
// mistaken for a degraded long-running workload. It returns "" when any pod is
// still live (Running/Pending/Unknown), leaving the ready/desired model to apply.
func terminalPodStatus(pods []PodRow) string {
	if len(pods) == 0 {
		return ""
	}
	anyFailed := false
	for _, p := range pods {
		switch p.Phase {
		case string(corev1.PodSucceeded):
		case string(corev1.PodFailed):
			anyFailed = true
		default:
			return "" // a live pod — not a finished workload
		}
	}
	if anyFailed {
		return "Failed"
	}
	return "Complete"
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

// Owner is the workload a pod rolls up to.
type Owner struct {
	Kind      string
	Namespace string
	Name      string
}

// PodOwners resolves every pod in in.Pods to the workload it belongs to, keyed
// by the pod's "namespace/name". It is kubeagent's single implementation of the
// pod-to-workload rule: Assemble calls it, and so does `kubeagent baseline
// capture`, rather than each keeping its own copy that can drift.
//
// baseline capture cannot read that rule off Assemble's output. Prioritize
// drops healthy-quiet workloads from the report and Assemble truncates a Job's
// or CronJob's pod list at jobPodCap — both right for a report an operator
// reads, both wrong for a baseline, which needs precisely the healthy majority
// and every pod behind it.
//
// The rule: a ReplicaSet-owned pod rolls up to that ReplicaSet's Deployment
// when the ReplicaSet is in in.ReplicaSets and names one, and to the ReplicaSet
// otherwise; a Job-owned pod rolls up to that Job's CronJob when the Job names
// one AND that CronJob is in in.CronJobs, and to the Job otherwise; any other
// controller owner is taken at face value; a pod with no owner is its own
// "Pod" workload.
func PodOwners(in Inputs) map[string]Owner {
	// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).
	rsToDeploy := map[string]string{}
	for _, rs := range in.ReplicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}
	// jobToCronJob resolves Job -> owning CronJob name (namespaced).
	jobToCronJob := map[string]string{}
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			jobToCronJob[j.Namespace+"/"+j.Name] = o.Name
		}
	}
	// cronJobs is the set Assemble seeds as CronJob workloads. Gating the
	// Job -> CronJob promotion on it is exactly Assemble's old
	// controllerKeys[key("CronJob", ns, cj)] check: the only CronJob entries
	// controllerKeys ever held came from in.CronJobs.
	cronJobs := map[string]bool{}
	for _, cj := range in.CronJobs {
		cronJobs[cj.Namespace+"/"+cj.Name] = true
	}

	out := make(map[string]Owner, len(in.Pods))
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
				if cj, ok := jobToCronJob[p.Namespace+"/"+o.Name]; ok && cronJobs[p.Namespace+"/"+cj] {
					kind, name = "CronJob", cj
				} else {
					kind, name = "Job", o.Name
				}
			default:
				kind, name = o.Kind, o.Name
			}
		}
		out[p.Namespace+"/"+p.Name] = Owner{Kind: kind, Namespace: p.Namespace, Name: name}
	}
	return out
}

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
	// CronJob-owned Jobs are NOT seeded as their own workloads (their pods roll
	// up to the CronJob, per PodOwners).
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			continue
		}
		seedJobLike("Job", j.Namespace, j.Name, jobStatus(j), "")
	}

	owners := PodOwners(in)
	podKey := map[string]string{}    // "ns/name" -> workload key
	derivedReady := map[string]int{} // ready-pod count for pod-derived workloads
	for _, p := range in.Pods {
		o := owners[p.Namespace+"/"+p.Name]
		k := key(o.Kind, p.Namespace, o.Name)
		w, ok := workloads[k]
		if !ok {
			w = &Workload{Namespace: p.Namespace, Name: o.Name, Kind: o.Kind}
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
		w.Pods = append(w.Pods, PodRowFor(p, time.Now()))
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
			if s := terminalPodStatus(w.Pods); s != "" {
				// Finished pods aren't "missing" — represent them like a
				// completed Job (0/0) so ready<desired doesn't flag them.
				w.Desired, w.Ready, w.Status = 0, 0, s
			}
		}
		if (w.Kind == "Job" || w.Kind == "CronJob") && len(w.Pods) > jobPodCap {
			w.PodsOmitted = len(w.Pods) - jobPodCap
			w.Pods = w.Pods[:jobPodCap]
		}
		if w.Status == "" {
			w.Status = WorkloadStatus(w.Ready, w.Desired)
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
		if ws[i].Name != ws[j].Name {
			return ws[i].Name < ws[j].Name
		}
		return ws[i].Kind < ws[j].Kind
	})
}

// Opts controls which lower-priority categories Prioritize includes.
type Opts struct {
	IncludeRestarts bool
	IncludeCron     bool
}

// Result is the filtered, prioritized workloads plus counts of what was hidden
// (for the report's footer hint).
type Result struct {
	Workloads      []Workload
	HiddenRestarts int
	HiddenCron     int
	Census         Census `json:"-"`
}

// Census counts long-running workloads before display filtering: Total is every
// one of them, Good those that are not Flagged(). The watch daemon's SLI reads
// this rather than Workloads, which is filtered for display and omits exactly
// the healthy majority an availability figure needs.
//
// Job and CronJob are excluded: neither is expected to be continuously up. A
// CronJob idle between runs is not unavailable, and a Job that failed once
// carries its findings forever, permanently denting a figure it has no business
// influencing.
//
// A Deployment or StatefulSet scaled to zero replicas is counted Good, not
// excluded: Ready(0) < Desired(0) is false and its Status is "Scaled Down", not
// "Failed", so Flagged() is false — deliberately, since an operator-initiated
// scale-down is not an outage and must not dent the SLI.
//
// json:"-" because inventory.Result is serialized by `scan --output json`, whose
// shape is a documented contract. The census feeds the watch SLI, not the report.
type Census struct {
	Good  int
	Total int
}

// Priority tiers (lower = more urgent).
const (
	priorityProblem = 2 // flagged non-cron workload
	priorityRestart = 3 // healthy but restarted
	priorityCron    = 4 // CronJob
)

// Prioritize filters the assembled workloads to what should be shown and tags
// each with a Priority tier. Problems (flagged non-cron) are always kept;
// restart-only and CronJobs are kept only when their opt-in flag is set;
// healthy-quiet workloads are always dropped. The result is sorted by
// (Priority, Namespace, Name, Kind).
func Prioritize(workloads []Workload, opts Opts) Result {
	var res Result
	for _, w := range workloads {
		if w.Kind != "Job" && w.Kind != "CronJob" {
			res.Census.Total++
			if !w.Flagged() {
				res.Census.Good++
			}
		}
		switch {
		case w.Kind == "CronJob":
			switch {
			case w.Flagged():
				w.Priority = priorityProblem
				res.Workloads = append(res.Workloads, w)
			case opts.IncludeCron:
				w.Priority = priorityCron
				res.Workloads = append(res.Workloads, w)
			default:
				res.HiddenCron++
			}
		case w.Flagged():
			w.Priority = priorityProblem
			res.Workloads = append(res.Workloads, w)
		case w.Restarts > 0:
			if opts.IncludeRestarts {
				w.Priority = priorityRestart
				res.Workloads = append(res.Workloads, w)
			} else {
				res.HiddenRestarts++
			}
		default:
			// healthy-quiet — always hidden
		}
	}
	sort.Slice(res.Workloads, func(i, j int) bool {
		a, b := res.Workloads[i], res.Workloads[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
	return res
}
