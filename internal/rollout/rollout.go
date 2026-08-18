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
//
// Revision 1 is excluded: it is the Deployment's creation, so there is no prior
// state for it to be a change from, and a workload flagged since it was created
// gains nothing from being told it was created. Suppressing it is what makes the
// annotation's presence mean something — that this workload changed recently.
// The gate is the revision number rather than the absence of a prior ReplicaSet:
// revision 5 with its predecessors garbage-collected still changed, and prints
// without a delta for a different reason.
//
// Suppression happens here rather than in a renderer so every surface agrees:
// the JSON document is the one that gets forwarded, and a rollout key describing
// a creation is the same non-signal there as on a terminal. The field is already
// omitempty and already absent for non-Deployments, unflagged workloads and
// rollouts past recencyWindow, so narrowing when it appears moves no
// schemaVersion.
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
		if revOf(*cur) <= 1 {
			continue // revision 1 is the Deployment's creation; nothing changed
		}
		rc := &inventory.RolloutChange{
			Revision: strconv.Itoa(revOf(*cur)),
			Since:    inventory.HumanSince(cur.CreationTimestamp.Time.UTC().Format(time.RFC3339), now),
		}
		if prev != nil {
			o, n := firstImage(*prev), firstImage(*cur)
			container := ""
			if oi, ni, name, matched := changedContainer(w, *prev, *cur); matched {
				o, n, container = oi, ni, name
			}
			if o != n && o != "" && n != "" {
				rc.OldImage, rc.NewImage = o, n
				if container != "" && container != firstContainerName(*cur) {
					rc.Container = container
				}
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

// firstContainerName is firstImage's sibling for the container's name rather
// than its image — used to decide whether a matched container is the one the
// unqualified delta already names.
func firstContainerName(rs appsv1.ReplicaSet) string {
	cs := rs.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Name
}

// findingImage returns the first non-empty Image among the workload's
// findings (diagnose.Finding.Image, R228's carrier), and whether one was
// found — the failing container's image, in finding order.
func findingImage(w inventory.Workload) (string, bool) {
	for _, f := range w.Findings {
		if f.Image != "" {
			return f.Image, true
		}
	}
	return "", false
}

// containerByImage returns the name of the ReplicaSet's container whose
// image matches, and whether one was found.
func containerByImage(rs appsv1.ReplicaSet, image string) (string, bool) {
	for _, c := range rs.Spec.Template.Spec.Containers {
		if c.Image == image {
			return c.Name, true
		}
	}
	return "", false
}

// imageByName returns the named container's image in the ReplicaSet, and
// whether it was found.
func imageByName(rs appsv1.ReplicaSet, name string) (string, bool) {
	for _, c := range rs.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c.Image, true
		}
	}
	return "", false
}

// changedContainer locates the container the workload's failing finding
// names: it looks up the finding's image in cur's template to get a
// container name, then reads that same name's image out of prev. matched is
// false — and the other three returns are zero — whenever no finding carries
// an image, the image matches no container in cur, or the matched container
// has no same-named counterpart in prev (added by this revision rather than
// changed by it, so there is no delta to describe for it), in which case the
// caller falls back to firstImage's unqualified delta exactly as before this
// helper existed.
func changedContainer(w inventory.Workload, prev, cur appsv1.ReplicaSet) (oldImage, newImage, container string, matched bool) {
	img, ok := findingImage(w)
	if !ok {
		return "", "", "", false
	}
	name, ok := containerByImage(cur, img)
	if !ok {
		return "", "", "", false
	}
	old, ok := imageByName(prev, name)
	if !ok {
		return "", "", "", false
	}
	return old, img, name, true
}
