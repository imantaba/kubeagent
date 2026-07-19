// Package pvchealth flags PersistentVolumeClaims stuck Pending because provisioning
// or binding failed, reading the PVC's ProvisioningFailed/FailedBinding events. Pure
// and read-only: the caller supplies the PVCs and events. Event-based, like the
// attach-time VolumeAttachError check — a Pending PVC with no failure event (Bound, or
// WaitForFirstConsumer waiting for a pod) is never flagged.
package pvchealth

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// Issue is one PVC stuck Pending because provisioning/binding failed.
type Issue struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`  // "Pending"
	Reason       string `json:"reason"` // "ProvisioningFailed" | "FailedBinding"
	Detail       string `json:"detail"` // the event message (the cause)
	StorageClass string `json:"storageClass,omitempty"`
}

// Assess flags each Pending PVC that has a provisioning/binding failure event.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event) []Issue {
	issues := make([]Issue, 0)
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		ev := newestFailureEvent(events, c.Namespace, c.Name)
		if ev == nil {
			continue
		}
		issues = append(issues, Issue{
			Namespace:    c.Namespace,
			Name:         c.Name,
			Phase:        "Pending",
			Reason:       ev.Reason,
			Detail:       ev.Message,
			StorageClass: storageClass(c),
		})
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Namespace != issues[j].Namespace {
			return issues[i].Namespace < issues[j].Namespace
		}
		return issues[i].Name < issues[j].Name
	})
	return issues
}

// newestFailureEvent returns the most recent ProvisioningFailed/FailedBinding event
// (by LastTimestamp) for the named PVC, or nil.
func newestFailureEvent(events []corev1.Event, namespace, name string) *corev1.Event {
	var best *corev1.Event
	for i := range events {
		e := &events[i]
		if e.InvolvedObject.Kind != "PersistentVolumeClaim" ||
			e.InvolvedObject.Namespace != namespace || e.InvolvedObject.Name != name {
			continue
		}
		if e.Reason != "ProvisioningFailed" && e.Reason != "FailedBinding" {
			continue
		}
		if best == nil || e.LastTimestamp.After(best.LastTimestamp.Time) {
			best = e
		}
	}
	return best
}

func storageClass(c corev1.PersistentVolumeClaim) string {
	if c.Spec.StorageClassName == nil {
		return ""
	}
	return *c.Spec.StorageClassName
}
