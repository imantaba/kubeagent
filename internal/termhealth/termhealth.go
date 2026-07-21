// Package termhealth flags resources wedged in Terminating — a Namespace stuck on
// a finalizer or a downstream condition, a Pod stuck past its grace period, a PVC
// held by pvc-protection — and names the blocker. Pure and read-only: the caller
// supplies the namespaces, pods, PVCs, threshold, and clock. Advisory (never
// affects the cluster verdict).
package termhealth

import (
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Issue is one resource stuck Terminating past the threshold.
type Issue struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Age       string `json:"age"`
	PastGrace bool   `json:"pastGrace,omitempty"`
	Reason    string `json:"reason"`
}

// nsConditionOrder lists the blocking namespace conditions in the order to report.
var nsConditionOrder = []corev1.NamespaceConditionType{
	"NamespaceDeletionContentFailure", "NamespaceContentRemaining", "NamespaceFinalizersRemaining",
}

// Assess flags every resource whose deletion has been pending longer than
// threshold, sorted by (Kind, Namespace, Name).
func Assess(namespaces []corev1.Namespace, pods []corev1.Pod, pvcs []corev1.PersistentVolumeClaim, threshold time.Duration, now time.Time) []Issue {
	var out []Issue
	for _, ns := range namespaces {
		if age, ok := stuckFor(ns.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "Namespace", Name: ns.Name, Age: age, Reason: nsReason(ns)})
		}
	}
	for _, p := range pods {
		if age, ok := stuckFor(p.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, Age: age, PastGrace: true, Reason: podReason(p)})
		}
	}
	for _, c := range pvcs {
		if age, ok := stuckFor(c.DeletionTimestamp, threshold, now); ok {
			out = append(out, Issue{Kind: "PersistentVolumeClaim", Namespace: c.Namespace, Name: c.Name, Age: age, Reason: pvcReason(c, pods)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// stuckFor reports the compact age and whether dt is set and older than threshold.
func stuckFor(dt *metav1.Time, threshold time.Duration, now time.Time) (string, bool) {
	if dt == nil {
		return "", false
	}
	d := now.Sub(dt.Time)
	if d <= threshold {
		return "", false
	}
	return compactDur(d), true
}

// compactDur renders a duration as the largest whole unit: Nd / Nh / Nm (min "1m").
func compactDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d >= time.Minute:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return "1m"
	}
}

func podReason(p corev1.Pod) string {
	if len(p.Finalizers) > 0 {
		return "finalizer " + strings.Join(p.Finalizers, ", ")
	}
	return "deletion not confirmed (node gone or kubelet not reporting)"
}

func pvcReason(c corev1.PersistentVolumeClaim, pods []corev1.Pod) string {
	hasProtection := false
	for _, f := range c.Finalizers {
		if f == "kubernetes.io/pvc-protection" {
			hasProtection = true
		}
	}
	if hasProtection {
		if mp := mountingPod(c, pods); mp != "" {
			return "pvc-protection — still mounted by pod " + mp
		}
		return "pvc-protection"
	}
	if len(c.Finalizers) > 0 {
		return "finalizer " + strings.Join(c.Finalizers, ", ")
	}
	return "deletion pending"
}

// mountingPod returns "ns/name" of the first same-namespace pod mounting the PVC.
func mountingPod(c corev1.PersistentVolumeClaim, pods []corev1.Pod) string {
	for _, p := range pods {
		if p.Namespace != c.Namespace {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == c.Name {
				return p.Namespace + "/" + p.Name
			}
		}
	}
	return ""
}

func nsReason(ns corev1.Namespace) string {
	byType := map[corev1.NamespaceConditionType]corev1.NamespaceCondition{}
	for _, c := range ns.Status.Conditions {
		byType[c.Type] = c
	}
	for _, t := range nsConditionOrder {
		if c, ok := byType[t]; ok {
			return string(t) + " — " + trimMsg(c.Message)
		}
	}
	if len(ns.Spec.Finalizers) > 0 {
		fs := make([]string, len(ns.Spec.Finalizers))
		for i, f := range ns.Spec.Finalizers {
			fs[i] = string(f)
		}
		return "finalizers " + strings.Join(fs, ", ")
	}
	return "deletion pending"
}

func trimMsg(s string) string { return strings.TrimRight(strings.TrimSpace(s), ".") }
