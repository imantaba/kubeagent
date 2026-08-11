// Package rollouthealth attaches a "RolloutStuck" finding to a flagged
// Deployment, StatefulSet or DaemonSet whose rollout has wedged — the new pods
// are not becoming available. A Deployment says so in its status conditions; a
// StatefulSet and a DaemonSet carry no conditions at all, so their arms read the
// revision and replica counters instead. Pure and read-only: the caller supplies
// the assembled+prioritized workloads, the controller objects (for their status)
// and the pods (for their age). Mirrors createhealth.Annotate.
package rollouthealth

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// rolloutGrace is how long a rollout is given before "not finished yet" is
// called "wedged". It is 600 seconds because that is Kubernetes' own default
// for a Deployment's spec.progressDeadlineSeconds — the Deployment arm has
// always been exactly this patient, because the API server applies that default
// and the kube-controller-manager is what sets ProgressDeadlineExceeded. A
// StatefulSet and a DaemonSet have no equivalent field, so their arms borrow the
// number rather than inventing one, and all three kinds wait the same length of
// time.
//
// A package constant, not a flag: the value is a restatement of an upstream
// default, and a flag would invite it to drift away from the Deployment arm it
// is meant to match.
const rolloutGrace = 600 * time.Second

// Annotate appends a "RolloutStuck" finding to each flagged Deployment,
// StatefulSet or DaemonSet workload whose controller status shows a stuck
// rollout, and which no pod-level cause already explains — see restarting for
// the second half of that test. It mutates the slice elements in place. Runs
// after createhealth.Annotate so a lingering FailedCreate event wins the "no
// existing finding" gate.
//
// now is injected rather than read from the clock so the grace period is
// testable, matching the detectors' pattern.
func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet, daemonSets []appsv1.DaemonSet, pods []corev1.Pod, now time.Time) {
	byDeployment := make(map[string]*appsv1.Deployment, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		byDeployment[d.Namespace+"/"+d.Name] = d
	}
	byStatefulSet := make(map[string]*appsv1.StatefulSet, len(statefulSets))
	for i := range statefulSets {
		s := &statefulSets[i]
		byStatefulSet[s.Namespace+"/"+s.Name] = s
	}
	byDaemonSet := make(map[string]*appsv1.DaemonSet, len(daemonSets))
	for i := range daemonSets {
		d := &daemonSets[i]
		byDaemonSet[d.Namespace+"/"+d.Name] = d
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || len(w.Findings) > 0 || restarting(w) {
			continue
		}
		key := w.Namespace + "/" + w.Name
		var (
			evidence string
			stuck    bool
			created  time.Time
		)
		// The workload's own kind selects which object list is consulted, so a
		// StatefulSet workload is never answered from a same-named Deployment.
		switch w.Kind {
		case "Deployment":
			dep, ok := byDeployment[key]
			if !ok {
				continue
			}
			// The Deployment arm reads a condition the controller only sets
			// after spec.progressDeadlineSeconds has already elapsed, so it
			// needs no grace period of its own.
			evidence, stuck = stuckCondition(dep)
			if stuck {
				w.Findings = append(w.Findings, finding(w, evidence))
			}
			continue
		case "StatefulSet":
			sts, ok := byStatefulSet[key]
			if !ok {
				continue
			}
			evidence, stuck = stuckStatefulSet(sts)
			created = sts.CreationTimestamp.Time
		case "DaemonSet":
			ds, ok := byDaemonSet[key]
			if !ok {
				continue
			}
			evidence, stuck = stuckDaemonSet(ds)
			created = ds.CreationTimestamp.Time
		default:
			continue
		}
		if !stuck || !settled(w, created, pods, now) {
			continue
		}
		w.Findings = append(w.Findings, finding(w, evidence))
	}
}

// finding builds the RolloutStuck finding for a workload. The reason names the
// workload's own kind — a value from the closed set the switch above admits, so
// it is not cluster text — and renders byte-identically to the long-standing
// wording for a Deployment.
func finding(w *inventory.Workload, evidence string) diagnose.Finding {
	return diagnose.Finding{
		Pod:      w.Namespace + "/" + w.Name,
		Issue:    "RolloutStuck",
		Reason:   "the " + w.Kind + "'s rollout cannot complete — the new pods are not becoming available",
		Evidence: evidence,
	}
}

// restarting reports whether any pod in the workload has restarted enough times
// to be crash-looping rather than to have hit one bad moment.
//
// It sits beside the len(w.Findings) > 0 clause, not in place of it. That clause
// is this package's zero-redundancy rule — RolloutStuck says nothing a pod-level
// detector has already said — but it is evaluated against one momentary sample,
// and a crash-looping container is only in Waiting between restart attempts.
// Forty consecutive scans of one unchanged Deployment reported RolloutStuck 32
// times and CrashLoopBackOff 8, so the issue kind that gate verdicts, SARIF
// results, alert dedup keys and the watch daemon's /issues are keyed on depended
// on which millisecond the scan landed in. The restart count is durable across
// the whole cycle, so it answers the question the instant state cannot.
//
// The identical clause in internal/createhealth was replaced rather than
// extended, and deliberately: there a workload with a pod finding is still
// short of pods and the second cause is real, so suppressing it lost half the
// story. Here the pod finding IS the cause, and a second finding for the same
// thing is the redundancy this package exists to avoid.
//
// diagnose.RestartThreshold is read rather than re-typed so the number this
// package uses and the one RestartLoopDetector uses cannot drift apart.
func restarting(w *inventory.Workload) bool {
	for _, p := range w.Pods {
		if p.Restarts >= diagnose.RestartThreshold {
			return true
		}
	}
	return false
}

// settled reports whether the workload has had the full grace period to finish.
// A rollout that is merely young and a rollout that is wedged look identical in
// a single sample of the counters, so the StatefulSet and DaemonSet arms wait
// before claiming anything.
//
// Two questions, because either alone has a hole. Every not-ready pod the
// workload owns must be older than the grace period — a pod that is still
// starting is the ordinary case and must not be called stuck. And the controller
// itself must be older, because a controller that has created no pods yet has no
// young pod to hold the arm back and would otherwise be called wedged seconds
// after it was applied.
//
// Ownership is read from the pod's own controller reference rather than through
// inventory.PodOwners: a StatefulSet and a DaemonSet own their pods directly,
// with none of the ReplicaSet or Job indirection that rule exists to resolve,
// and this function is never reached for a Deployment. Reading the pods rather
// than w.Pods also means Assemble's truncation of a long pod list cannot make a
// young pod invisible here.
func settled(w *inventory.Workload, created time.Time, pods []corev1.Pod, now time.Time) bool {
	if now.Sub(created) < rolloutGrace {
		return false
	}
	for i := range pods {
		p := &pods[i]
		if p.Namespace != w.Namespace || !controlledBy(p, w.Kind, w.Name) || podReady(p) {
			continue
		}
		if now.Sub(p.CreationTimestamp.Time) < rolloutGrace {
			return false
		}
	}
	return true
}

// controlledBy reports whether the pod's controller reference names this
// workload.
func controlledBy(p *corev1.Pod, kind, name string) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == kind && o.Name == name {
			return true
		}
	}
	return false
}

// podReady reports whether the pod's Ready condition is True. A pod with no
// Ready condition at all has not become ready.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// stuckCondition returns the evidence string and true when the Deployment's
// status shows a wedged rollout. ReplicaFailure (the concrete pod-creation
// blocker) takes precedence over Progressing/ProgressDeadlineExceeded.
func stuckCondition(dep *appsv1.Deployment) (string, bool) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return fmt.Sprintf("ReplicaFailure: %s", c.Message), true
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return fmt.Sprintf("Progressing (ProgressDeadlineExceeded): %s", c.Message), true
		}
	}
	return "", false
}

// stuckStatefulSet returns the evidence string and true when the StatefulSet's
// counters show a wedged rollout. A StatefulSet publishes no conditions, so
// there is nothing to read but the counters: an update that is not reaching the
// replicas (the revisions differ and updatedReplicas has not caught up), or,
// with no update in flight, replicas that are not becoming ready.
//
// The denominator is spec.replicas — what the operator asked for — not
// status.replicas, which counts the pods the controller managed to create and
// would read 0/0 on a StatefulSet wedged because it can create nothing.
//
// Every value in the evidence is an integer the API server owns, so no
// sanitizing is needed. The revision names are compared and never emitted;
// naming them would put a ControllerRevision's identity into a report designed
// to be forwarded, and they answer nothing an operator asked.
func stuckStatefulSet(sts *appsv1.StatefulSet) (string, bool) {
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	if desired <= 0 {
		return "", false // scaled to zero on purpose
	}
	s := sts.Status
	if s.UpdateRevision != s.CurrentRevision {
		if s.UpdatedReplicas < desired {
			return fmt.Sprintf("updatedReplicas %d/%d, update revision pending", s.UpdatedReplicas, desired), true
		}
		return "", false
	}
	if s.ReadyReplicas < desired {
		return fmt.Sprintf("readyReplicas %d/%d, revision unchanged", s.ReadyReplicas, desired), true
	}
	return "", false
}

// stuckDaemonSet returns the evidence string and true when the DaemonSet's
// counters show a wedged rollout. The shortfall is numberReady against
// desiredNumberScheduled; whether the update is also stalled
// (updatedNumberScheduled behind desired) only decides which counters the
// evidence names.
//
// desiredNumberScheduled is the denominator because a DaemonSet has no replica
// count in its spec — the number it wants is however many nodes its selector and
// the nodes' taints admit. Zero means it wants nothing anywhere, which is not a
// shortfall.
//
// Every value in the evidence is an integer the API server owns, so no
// sanitizing is needed, and no node is named.
func stuckDaemonSet(ds *appsv1.DaemonSet) (string, bool) {
	s := ds.Status
	if s.DesiredNumberScheduled <= 0 || s.NumberReady >= s.DesiredNumberScheduled {
		return "", false
	}
	if s.UpdatedNumberScheduled < s.DesiredNumberScheduled {
		return fmt.Sprintf("numberReady %d/%d, updatedNumberScheduled %d/%d",
			s.NumberReady, s.DesiredNumberScheduled, s.UpdatedNumberScheduled, s.DesiredNumberScheduled), true
	}
	return fmt.Sprintf("numberReady %d/%d, all pods updated", s.NumberReady, s.DesiredNumberScheduled), true
}
