// Package createhealth attaches a "FailedCreate" finding to a workload whose
// controller cannot create pods — a ResourceQuota, LimitRange, or admission
// webhook is rejecting them, so the workload sits below its desired replicas
// with no pods to diagnose. Pure and read-only: the caller supplies the
// assembled+prioritized workloads, the ReplicaSets (to resolve a Deployment's
// ReplicaSet events back to the Deployment), and the FailedCreate events.
// Mirrors netpolicy/rollout.Annotate.
package createhealth

import (
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "FailedCreate" finding to each flagged workload that has no
// existing finding and whose controller has a FailedCreate event. It mutates the
// slice elements in place. A Deployment's FailedCreate event lands on its
// ReplicaSet, so replicaSets resolves that back to the Deployment; StatefulSet
// and DaemonSet events are matched directly.
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, events []corev1.Event) {
	rsToDeploy := map[string]string{}
	for _, rs := range replicaSets {
		if name, ok := ownedByDeployment(rs); ok {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = name
		}
	}
	byWorkload := map[string]*corev1.Event{}
	for i := range events {
		e := &events[i]
		if e.Reason != "FailedCreate" {
			continue
		}
		key := workloadKeyForEvent(e, rsToDeploy)
		if key == "" {
			continue
		}
		if best, ok := byWorkload[key]; !ok || e.LastTimestamp.Time.After(best.LastTimestamp.Time) {
			byWorkload[key] = e
		}
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || len(w.Findings) > 0 {
			continue
		}
		e, ok := byWorkload[w.Kind+"/"+w.Namespace+"/"+w.Name]
		if !ok {
			continue
		}
		w.Findings = append(w.Findings, diagnose.Finding{
			Pod:      w.Namespace + "/" + w.Name,
			Issue:    "FailedCreate",
			Reason:   "the controller cannot create pods — " + classifyCreateFailure(e.Message),
			Evidence: e.Message,
		})
	}
}

// ownedByDeployment returns the owning Deployment's name if the ReplicaSet is
// controlled by one.
func ownedByDeployment(rs appsv1.ReplicaSet) (string, bool) {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Controller != nil && *o.Controller {
			return o.Name, true
		}
	}
	return "", false
}

// workloadKeyForEvent maps a FailedCreate event to a workload key
// ("Kind/namespace/name"), resolving a ReplicaSet to its owning Deployment. It
// returns "" for an involved-object kind this check does not track (e.g. a Job,
// which internal/batchhealth owns).
func workloadKeyForEvent(e *corev1.Event, rsToDeploy map[string]string) string {
	io := e.InvolvedObject
	switch io.Kind {
	case "ReplicaSet":
		if dep, ok := rsToDeploy[io.Namespace+"/"+io.Name]; ok {
			return "Deployment/" + io.Namespace + "/" + dep
		}
		return "ReplicaSet/" + io.Namespace + "/" + io.Name
	case "StatefulSet", "DaemonSet":
		return io.Kind + "/" + io.Namespace + "/" + io.Name
	default:
		return ""
	}
}

// classifyCreateFailure names the pod-creation failure mode from the controller's
// FailedCreate event message. Order matters: a quota/LimitRange denial is also
// phrased as "forbidden", so those are matched before the generic forbidden case.
func classifyCreateFailure(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "exceeded quota"):
		return "blocked by a ResourceQuota"
	case strings.Contains(m, "admission webhook"):
		return "rejected by an admission webhook"
	case strings.Contains(m, "limitrange"), strings.Contains(m, "minimum "), strings.Contains(m, "maximum "):
		return "violates a LimitRange"
	case strings.Contains(m, "forbidden"):
		return "forbidden by admission"
	default:
		return "pod creation is failing"
	}
}
