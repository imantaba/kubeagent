// Package inventory groups a cluster's pods into workloads (Deployments,
// StatefulSets, DaemonSets, and bare pods), computing replica health and
// restart history, and attaches detector findings to the owning workload.
package inventory

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// PodRow is one pod under a workload.
type PodRow struct {
	Name        string
	Phase       string
	Ready       string // "1/1"
	Restarts    int
	LastRestart string // RFC3339 UTC, "" if none
	Node        string
	IP          string
	Age         string
	Image       string
}

// Workload is one controller (or bare pod) and its aggregated health.
type Workload struct {
	Namespace   string
	Name        string
	Kind        string // Deployment | StatefulSet | DaemonSet | ReplicaSet | Pod
	Desired     int
	Ready       int
	Status      string // Running | Degraded
	Restarts    int
	LastRestart string
	Image       string
	Pods        []PodRow
	Findings    []diagnose.Finding
}

// Flagged reports whether the workload needs attention: it has a detector
// finding or is not fully ready.
func (w Workload) Flagged() bool {
	return len(w.Findings) > 0 || w.Ready < w.Desired
}

func termTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
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
	if desired > 0 && ready >= desired {
		return "Running"
	}
	return "Degraded"
}
