package diagnose

import (
	corev1 "k8s.io/api/core/v1"
)

// PodFacts bundles everything a detector needs about one pod.
// Events is populated for forward-compatibility; v1 detectors read Pod only.
type PodFacts struct {
	Pod    *corev1.Pod
	Events []corev1.Event
}

// Finding is one diagnosis: what's wrong with a pod and why.
type Finding struct {
	Pod      string `json:"pod"`      // "namespace/name"
	Issue    string `json:"issue"`    // "CrashLoopBackOff"
	Reason   string `json:"reason"`   // human-readable root cause
	Evidence string `json:"evidence"` // the exact signal observed
}

// Detector inspects one pod's facts and returns a Finding if it matches,
// or nil when the pod does not exhibit this failure mode.
type Detector interface {
	Detect(facts PodFacts) *Finding
}

// Run applies every detector to every pod and collects all findings.
func Run(detectors []Detector, facts []PodFacts) []Finding {
	var findings []Finding
	for _, f := range facts {
		for _, d := range detectors {
			if finding := d.Detect(f); finding != nil {
				findings = append(findings, *finding)
			}
		}
	}
	return findings
}
