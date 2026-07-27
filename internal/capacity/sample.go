package capacity

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// attachSamples records coverage and writes an observed reading onto owners that a
// structural rule ALREADY flagged. It never adds, removes, or reorders an owner.
//
// This is the whole discipline of the feature: metrics-server returns one sample of
// roughly a 30-second average and keeps no history, so a reading cannot justify
// putting a workload on the list — only annotating one that a provable rule put
// there.
//
// A RuleLimitNoRequest row sourced from a CronJob's OwnerTemplate is a known
// exception that never receives a sample: its key is "CronJob/ns/name", but its
// pods are owned by a Job and roll up (via ownerOf, below) to "Job/ns/jobname" —
// the Job that runs the CronJob's schedule, not the CronJob itself. The two keys
// never match. The row is still correct and still renders; it is simply left
// without an Observed reading rather than built into a Job-to-CronJob roll-up.
func attachSamples(rs *RightSizing, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	usage map[string]corev1.ResourceList, namespace string) {
	if rs == nil {
		return
	}
	rsIndex := deploymentIndex(replicaSets)

	type total struct{ cpu, mem int64 }
	byOwner := map[string]*total{}
	for _, p := range pods {
		if terminal(p) || (namespace != "" && p.Namespace != namespace) {
			continue
		}
		rs.PodsTotal++
		u, ok := usage[p.Namespace+"/"+p.Name]
		if !ok {
			continue
		}
		rs.PodsReporting++
		o := ownerOf(p, rsIndex)
		key := o.Kind + "/" + o.Namespace + "/" + o.Name
		if byOwner[key] == nil {
			byOwner[key] = &total{}
		}
		cpu := u[corev1.ResourceCPU]
		mem := u[corev1.ResourceMemory]
		byOwner[key].cpu += cpu.MilliValue()
		byOwner[key].mem += mem.Value()
	}
	rs.MetricsAvailable = rs.PodsReporting > 0
	if !rs.MetricsAvailable {
		return
	}
	for ri := range rs.Rules {
		for oi := range rs.Rules[ri].Owners {
			o := &rs.Rules[ri].Owners[oi]
			t, ok := byOwner[o.Kind+"/"+o.Namespace+"/"+o.Name]
			if !ok {
				continue
			}
			o.Observed = observedFor(o.flaggedResource, t.cpu, t.mem)
		}
	}
}

// observedFor picks the reading that speaks to the rule: the resource the rule
// actually flagged (Owner.flaggedResource — RuleLimitNoRequest can fire on either
// CPU or memory), everything else defaults to CPU via flaggedResource's zero value.
// This never re-derives the resource by parsing Detail's rendered text: a display
// string is not a data channel, and the next wording change would silently break it.
func observedFor(res resourceKind, cpuMilli, memBytes int64) string {
	if res == resourceMemory {
		return formatBytes(memBytes)
	}
	return formatMilliCPU(cpuMilli) + " cores"
}
