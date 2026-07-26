package watch

import (
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// issueKeys projects one evaluation into the set of tracked issue instances.
// Pure and deterministic; duplicates collapse, so two broken routes on the same
// Ingress with the same problem yield one key. Sorting is the tracker's job.
//
// Intentionally excluded: intentionally-empty ("Expected") Service and Ingress
// issues, and the advisory config reports (NodeReserve, PVCReclaim,
// SecurityIssues) — those describe standing configuration, not incidents that
// fire and resolve, so tracking them would make MTTR meaningless.
//
// Known gap: nodes declared via --expected-nodes but absent from the cluster
// (Health.NodesExpectedAbsent) are not tracked. ClusterHealth exposes them only
// as a counter and a free-text NodeIssues sentence, with no stable per-node key
// to project. Tracking them needs a structured field on the detector, which is
// a separate change.
func issueKeys(res *scan.Result) []watchstate.Key {
	seen := map[watchstate.Key]bool{}
	var keys []watchstate.Key
	add := func(kind, namespace, name, issue string) {
		k := watchstate.Key{Kind: kind, Namespace: namespace, Name: name, Issue: issue}
		if seen[k] {
			return
		}
		seen[k] = true
		keys = append(keys, k)
	}

	for _, w := range res.Inventory.Workloads {
		if len(w.Findings) > 0 {
			for _, f := range w.Findings {
				add(w.Kind, w.Namespace, w.Name, f.Issue)
			}
			continue
		}
		if w.Flagged() {
			add(w.Kind, w.Namespace, w.Name, "Degraded")
		}
	}
	for _, i := range res.ServiceIssues {
		if !i.Expected {
			add("Service", i.Namespace, i.Name, i.Problem)
		}
	}
	for _, i := range res.IngressIssues {
		if !i.Expected {
			add("Ingress", i.Namespace, i.Ingress, i.Problem)
		}
	}
	for _, i := range res.PVCIssues {
		add("PVC", i.Namespace, i.Name, i.Reason)
	}
	for _, i := range res.StuckTerminating {
		add(i.Kind, i.Namespace, i.Name, "StuckTerminating")
	}
	for _, i := range res.PDBIssues {
		add("PodDisruptionBudget", i.Namespace, i.Name, i.Category)
	}
	for _, i := range res.HPAIssues {
		add("HorizontalPodAutoscaler", i.Namespace, i.Name, i.Category)
	}
	for _, i := range res.WebhookIssues {
		add(i.Kind, "", i.Config+"/"+i.Webhook, i.Problem)
	}
	for _, i := range res.QuotaIssues {
		add("ResourceQuota", i.Namespace, i.Quota+"/"+i.Resource, i.Severity)
	}
	for _, n := range res.Health.DownNodes {
		issue := "KubeletNotHeartbeating"
		if n.Reason == "NotReady" {
			issue = "NotReady"
		}
		add("Node", "", n.Name, issue)
	}
	for _, i := range res.KubeletHealth.Unhealthy {
		add("Node", "", i.Node, "KubeletUnhealthy")
	}
	if res.ControlPlane.Status == "unhealthy" {
		add("Cluster", "", "control-plane", "Unhealthy")
	}
	if res.DNS.Status == "degraded" {
		add("Cluster", "", "coredns", "DNSDegraded")
	}
	if res.Certificates != nil {
		for _, c := range res.Certificates.Expired {
			add("Secret", c.Namespace, c.Name, "CertExpired")
		}
		for _, c := range res.Certificates.Expiring {
			add("Secret", c.Namespace, c.Name, "CertExpiring")
		}
		for _, c := range res.Certificates.Invalid {
			add("Secret", c.Namespace, c.Name, "CertInvalid")
		}
	}
	for _, v := range res.DiskUsage.Over {
		name := v.Name
		if v.Kind == "node" {
			name = v.Node
		}
		add("Volume", v.Namespace, name, "DiskOverThreshold")
	}
	return keys
}

// flaggedWorkloads is the evaluation's flagged workloads, which is the cluster
// context an explanation gets. Same predicate the issue tracker uses.
func flaggedWorkloads(res *scan.Result) []inventory.Workload {
	var out []inventory.Workload
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			out = append(out, w)
		}
	}
	return out
}
