// Package pvcreclaim lists PersistentVolumeClaims whose bound PersistentVolume
// has reclaimPolicy Delete — the data-loss-prone case where deleting the PVC or
// PV destroys the underlying storage. Pure: the caller supplies the PVCs and
// PVs. Read-only.
package pvcreclaim

import (
	corev1 "k8s.io/api/core/v1"
)

// PVCReclaim is one flagged PVC: it is Bound and its PV reclaims with Delete.
type PVCReclaim struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	PV           string `json:"pv"`
	StorageClass string `json:"storageClass,omitempty"`
	Capacity     string `json:"capacity,omitempty"`
}

// Report is the set of Delete-policy PVCs. Count == len(PVCs).
type Report struct {
	PVCs  []PVCReclaim `json:"pvcs"`
	Count int          `json:"count"`
}

// Assess flags each Bound PVC whose bound PV has reclaimPolicy Delete.
func Assess(pvcs []corev1.PersistentVolumeClaim, pvs []corev1.PersistentVolume) Report {
	policy := make(map[string]corev1.PersistentVolumeReclaimPolicy, len(pvs))
	for _, v := range pvs {
		policy[v.Name] = v.Spec.PersistentVolumeReclaimPolicy
	}

	rep := Report{PVCs: make([]PVCReclaim, 0)}
	for _, c := range pvcs {
		if c.Status.Phase != corev1.ClaimBound || c.Spec.VolumeName == "" {
			continue
		}
		p, ok := policy[c.Spec.VolumeName]
		if !ok || p != corev1.PersistentVolumeReclaimDelete {
			continue
		}
		rep.PVCs = append(rep.PVCs, PVCReclaim{
			Namespace:    c.Namespace,
			Name:         c.Name,
			PV:           c.Spec.VolumeName,
			StorageClass: storageClass(c),
			Capacity:     capacity(c),
		})
	}
	rep.Count = len(rep.PVCs)
	return rep
}

func storageClass(c corev1.PersistentVolumeClaim) string {
	if c.Spec.StorageClassName == nil {
		return ""
	}
	return *c.Spec.StorageClassName
}

func capacity(c corev1.PersistentVolumeClaim) string {
	q, ok := c.Status.Capacity[corev1.ResourceStorage]
	if !ok {
		return ""
	}
	return q.String()
}
