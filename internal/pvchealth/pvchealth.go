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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Issue is one PVC stuck Pending because provisioning/binding failed.
type Issue struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`  // "Pending"
	Reason       string `json:"reason"` // "ProvisioningFailed" | "FailedBinding" | "MissingStorageClass" | "NoMatchingPV" | "PVSelectorMismatch"
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
		// The message is free text the API server does not validate, and this is
		// where it becomes a kubeagent value. The reason is not sanitized, and
		// deliberately: newestFailureEvent admits only "ProvisioningFailed" and
		// "FailedBinding", so what reaches an issue is one of two literals this
		// package already knows, not text from the cluster.
		issues = append(issues, Issue{
			Namespace: c.Namespace, Name: c.Name, Phase: "Pending",
			Reason: ev.Reason, Detail: safetext.Line(ev.Message), StorageClass: storageClass(c),
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
		ok, excludedBySelector := anyMatchingPV(c, pvs)
		if !ok {
			if excludedBySelector > 0 {
				return "PVSelectorMismatch", fmt.Sprintf("no available PersistentVolume matches its selector (%d otherwise-suitable volume(s) excluded)", excludedBySelector), true
			}
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
// claim's size, access modes, volume mode, and label selector. When none does, ok is
// false and excludedBySelector counts the PVs that satisfied every other criterion and
// were excluded by the selector alone — the signal that distinguishes "no storage"
// from "your selector is wrong".
func anyMatchingPV(c corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) (ok bool, excludedBySelector int) {
	req := c.Spec.Resources.Requests[corev1.ResourceStorage]
	// A nil selector means the claim named no selector at all — every PV is a
	// candidate. metav1.LabelSelectorAsSelector(nil) returns labels.Nothing()
	// instead, since callers with a NetworkPolicy-style non-pointer selector
	// mean the opposite by nil; a PVC's *LabelSelector has no such convention,
	// so that default must be overridden here.
	sel := labels.Everything()
	if c.Spec.Selector != nil {
		var err error
		sel, err = metav1.LabelSelectorAsSelector(c.Spec.Selector)
		if err != nil {
			sel = labels.Nothing()
		}
	}
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
		if !volumeModeSatisfied(c.Spec.VolumeMode, pv.Spec.VolumeMode) {
			continue
		}
		if !sel.Matches(labels.Set(pv.Labels)) {
			excludedBySelector++
			continue
		}
		return true, 0
	}
	return false, excludedBySelector
}

// volumeModeSatisfied reports whether the PV's volume mode satisfies the claim's.
// A nil VolumeMode on either side defaults to Filesystem, matching the API server's
// own defaulting.
func volumeModeSatisfied(want, have *corev1.PersistentVolumeMode) bool {
	w := corev1.PersistentVolumeFilesystem
	if want != nil {
		w = *want
	}
	h := corev1.PersistentVolumeFilesystem
	if have != nil {
		h = *have
	}
	return w == h
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
// (by LastTimestamp) for the named PVC, or nil. A failure event the controller has
// since spoken past — any strictly newer event on the same PVC, regardless of
// reason — is history rather than a live diagnosis, so it is not returned either.
// A tie (equal LastTimestamp) does not supersede: only a strictly newer event does.
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
	if best == nil {
		return nil
	}
	for i := range events {
		e := &events[i]
		if e.InvolvedObject.Kind != "PersistentVolumeClaim" ||
			e.InvolvedObject.Namespace != namespace || e.InvolvedObject.Name != name {
			continue
		}
		if e.LastTimestamp.After(best.LastTimestamp.Time) {
			return nil
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
