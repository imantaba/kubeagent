package capacity

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// OwnerTemplate is one workload's pod template carried with the workload's own
// identity. RuleLimitNoRequest reads templates rather than Pods because the API
// server copies an unset request from the limit when it admits a Pod, so a stored
// Pod never shows the limit-without-request shape — only the template its author
// wrote does.
type OwnerTemplate struct {
	Kind      string
	Namespace string
	Name      string
	Spec      corev1.PodSpec
}

// Templates collects one OwnerTemplate per workload from the slices
// inventory.Inputs already carries — no new API call, no new RBAC.
//
// Two deliberate exclusions:
//
//   - ReplicaSets are not a source. A Deployment's own template already carries
//     the authored shape; adding its ReplicaSets would report one authored
//     mistake twice under two owner identities.
//   - A Job owned by a CronJob is not a source. Its template is a copy of the
//     CronJob's. Skipping it here (rather than deduplicating downstream) avoids
//     reporting one authored mistake twice under two owner identities, the same
//     reasoning as the ReplicaSet exclusion above. The CronJob's own template is
//     the source; a bare Job (no CronJob owner) is still included.
//
// The returned slice is built by appending across these fixed-order input
// slices, never by ranging a map, so the order is deterministic for identical
// inputs.
func Templates(deployments []appsv1.Deployment, statefulSets []appsv1.StatefulSet,
	daemonSets []appsv1.DaemonSet, jobs []batchv1.Job, cronJobs []batchv1.CronJob) []OwnerTemplate {

	var out []OwnerTemplate
	for _, d := range deployments {
		out = append(out, OwnerTemplate{
			Kind: "Deployment", Namespace: d.Namespace, Name: d.Name,
			Spec: d.Spec.Template.Spec,
		})
	}
	for _, s := range statefulSets {
		out = append(out, OwnerTemplate{
			Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name,
			Spec: s.Spec.Template.Spec,
		})
	}
	for _, ds := range daemonSets {
		out = append(out, OwnerTemplate{
			Kind: "DaemonSet", Namespace: ds.Namespace, Name: ds.Name,
			Spec: ds.Spec.Template.Spec,
		})
	}
	for _, j := range jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			continue
		}
		out = append(out, OwnerTemplate{
			Kind: "Job", Namespace: j.Namespace, Name: j.Name,
			Spec: j.Spec.Template.Spec,
		})
	}
	for _, cj := range cronJobs {
		out = append(out, OwnerTemplate{
			Kind: "CronJob", Namespace: cj.Namespace, Name: cj.Name,
			Spec: cj.Spec.JobTemplate.Spec.Template.Spec,
		})
	}
	return out
}
