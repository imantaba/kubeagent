// Package rollout annotates flagged Deployments with their most recent rollout —
// what changed (revision, image) and when — so a degraded workload reads as a
// lead ("changed 4d ago") rather than a bare symptom. Pure and read-only; the
// caller supplies workloads, ReplicaSets, and the clock.
package rollout

import (
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const revisionAnno = "deployment.kubernetes.io/revision"

// recencyWindow bounds how old a rollout may be and still be reported as a
// recent change. A flagged Deployment whose current rollout predates this window
// gets no annotation.
const recencyWindow = 7 * 24 * time.Hour

// Annotate sets w.Rollout for each flagged Deployment whose current (highest-
// revision) ReplicaSet was created within recencyWindow of now, recording the
// revision, its age, and the first-container image delta versus the previous
// revision (image left empty when unchanged or when there is no prior revision).
// It mutates the slice elements in place.
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, now time.Time) {
	for i := range workloads {
		w := workloads[i]
		if !w.Flagged() || w.Kind != "Deployment" {
			continue
		}
		cur, prev := currentAndPrevRS(w.Namespace, w.Name, replicaSets)
		if cur == nil {
			continue
		}
		if now.Sub(cur.CreationTimestamp.Time) > recencyWindow {
			continue // rollout too old to be "what changed"
		}
		rc := &inventory.RolloutChange{
			Revision: strconv.Itoa(revOf(*cur)),
			Since:    inventory.HumanSince(cur.CreationTimestamp.Time.UTC().Format(time.RFC3339), now),
		}
		if prev != nil {
			if o, n := firstImage(*prev), firstImage(*cur); o != n && o != "" && n != "" {
				rc.OldImage, rc.NewImage = o, n
			}
		}
		workloads[i].Rollout = rc
	}
}

// currentAndPrevRS returns the ReplicaSets with the highest and second-highest
// revision owned by the named Deployment (prev is nil when only one revision).
func currentAndPrevRS(namespace, deployment string, replicaSets []appsv1.ReplicaSet) (cur, prev *appsv1.ReplicaSet) {
	var owned []appsv1.ReplicaSet
	for _, rs := range replicaSets {
		if rs.Namespace == namespace && ownedBy(rs, deployment) && revOf(rs) > 0 {
			owned = append(owned, rs)
		}
	}
	if len(owned) == 0 {
		return nil, nil
	}
	sort.Slice(owned, func(i, j int) bool { return revOf(owned[i]) > revOf(owned[j]) })
	cur = &owned[0]
	if len(owned) > 1 {
		prev = &owned[1]
	}
	return cur, prev
}

func ownedBy(rs appsv1.ReplicaSet, deployment string) bool {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Name == deployment {
			return true
		}
	}
	return false
}

func revOf(rs appsv1.ReplicaSet) int {
	if v, ok := rs.Annotations[revisionAnno]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func firstImage(rs appsv1.ReplicaSet) string {
	cs := rs.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}
