// Package pvchealth flags PersistentVolumeClaims stuck Pending because provisioning
// or binding failed. It uses two strategies: a structural cause derived from the cluster
// graph (missing StorageClass, or no matching PV for a static claim), and falling back
// to the PVC's ProvisioningFailed/FailedBinding events. Pure and read-only: the caller
// supplies the PVCs, events, StorageClasses, and PVs.
package pvchealth

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Issue is one PVC stuck Pending because provisioning/binding failed.
type Issue struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`  // "Pending"
	Reason       string `json:"reason"` // "ProvisioningFailed" | "FailedBinding" | "MissingStorageClass" | "NoMatchingPV"
	Detail       string `json:"detail"` // the cause
	StorageClass string `json:"storageClass,omitempty"`
}

// Assess flags each Pending PVC that cannot provision or bind, naming the cause:
// a missing StorageClass or no matching PV (structural, event-independent), else
// the newest ProvisioningFailed/FailedBinding event's message. Pure and read-only.
func Assess(pvcs []corev1.PersistentVolumeClaim, events []corev1.Event, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) []Issue {
	issues := make([]Issue, 0)
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimPending {
			continue
		}
		if reason, detail, ok := structuralCause(c, storageClasses, pvs); ok {
			issues = append(issues, Issue{
				Namespace: c.Namespace, Name: c.Name, Phase: "Pending",
				Reason: reason, Detail: detail, StorageClass: storageClass(c),
			})
			continue
		}
		ev := newestFailureEvent(events, c.Namespace, c.Name)
		if ev == nil {
			continue
		}
		issues = append(issues, Issue{
			Namespace: c.Namespace, Name: c.Name, Phase: "Pending",
			Reason: ev.Reason, Detail: ev.Message, StorageClass: storageClass(c),
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

// structuralCause returns a definitive provisioning cause derived from the cluster
// graph (missing StorageClass, or no matching PV for a static claim), or ok=false
// when none applies (leaving the PVC to the event path).
func structuralCause(c corev1.PersistentVolumeClaim, storageClasses []storagev1.StorageClass, pvs []corev1.PersistentVolume) (reason, detail string, ok bool) {
	sc := c.Spec.StorageClassName
	switch {
	case sc != nil && *sc != "":
		if !classExists(*sc, storageClasses) {
			return "MissingStorageClass", fmt.Sprintf("references StorageClass %q which does not exist", *sc), true
		}
		return "", "", false
	case sc != nil && *sc == "":
		if !anyMatchingPV(c, pvs) {
			return "NoMatchingPV", fmt.Sprintf("no available PersistentVolume matches its request (%s, %s)", requestSize(c), modeList(c)), true
		}
		return "", "", false
	default: // sc == nil (default SC / ambiguous) — leave to the event path
		return "", "", false
	}
}

func classExists(name string, scs []storagev1.StorageClass) bool {
	for _, s := range scs {
		if s.Name == name {
			return true
		}
	}
	return false
}

// anyMatchingPV reports whether some Available, unbound, static PV can satisfy the
// claim's size and access modes.
func anyMatchingPV(c corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) bool {
	req := c.Spec.Resources.Requests[corev1.ResourceStorage]
	for _, pv := range pvs {
		if pv.Status.Phase != corev1.VolumeAvailable || pv.Spec.ClaimRef != nil {
			continue
		}
		if pv.Spec.StorageClassName != "" {
			continue // a dynamic-class PV is not a candidate for a static claim
		}
		pvCap := pv.Spec.Capacity[corev1.ResourceStorage]
		if pvCap.Cmp(req) < 0 {
			continue
		}
		if !modesSatisfied(c.Spec.AccessModes, pv.Spec.AccessModes) {
			continue
		}
		return true
	}
	return false
}

func modesSatisfied(want, have []corev1.PersistentVolumeAccessMode) bool {
	set := make(map[corev1.PersistentVolumeAccessMode]bool, len(have))
	for _, m := range have {
		set[m] = true
	}
	for _, m := range want {
		if !set[m] {
			return false
		}
	}
	return true
}

func requestSize(c corev1.PersistentVolumeClaim) string {
	if q, ok := c.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return q.String()
	}
	return "?"
}

func modeList(c corev1.PersistentVolumeClaim) string {
	parts := make([]string, 0, len(c.Spec.AccessModes))
	for _, m := range c.Spec.AccessModes {
		parts = append(parts, string(m))
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, ",")
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
