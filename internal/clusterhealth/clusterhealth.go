// Package clusterhealth derives a one-line cluster verdict from node health
// and a kube-system workload rollup.
package clusterhealth

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const systemNamespace = "kube-system"

// ClusterHealth is the first-line cluster verdict.
type ClusterHealth struct {
	Verdict      string   `json:"verdict"` // Healthy | Degraded
	NodesTotal   int      `json:"nodesTotal"`
	NodesReady   int      `json:"nodesReady"`
	NodeIssues   []string `json:"nodeIssues,omitempty"`
	SystemIssues []string `json:"systemIssues,omitempty"`
	ScopeNote    string   `json:"scopeNote,omitempty"`
}

// Assess computes the verdict from nodes and the assembled workloads. A node is
// unhealthy if not Ready, under Memory/Disk/PID pressure, or cordoned. System
// issues are flagged kube-system workloads. The verdict is Healthy only when
// there are no node and no system issues.
func Assess(nodes []corev1.Node, workloads []inventory.Workload) ClusterHealth {
	ch := ClusterHealth{NodesTotal: len(nodes)}
	for _, n := range nodes {
		ready, issues := nodeHealth(n)
		if ready {
			ch.NodesReady++
		}
		for _, iss := range issues {
			ch.NodeIssues = append(ch.NodeIssues, n.Name+" "+iss)
		}
	}
	for _, w := range workloads {
		if w.Namespace == systemNamespace && w.Flagged() {
			ch.SystemIssues = append(ch.SystemIssues,
				fmt.Sprintf("%s/%s %d/%d %s", w.Namespace, w.Name, w.Ready, w.Desired, w.Status))
		}
	}
	if len(ch.NodeIssues) == 0 && len(ch.SystemIssues) == 0 {
		ch.Verdict = "Healthy"
	} else {
		ch.Verdict = "Degraded"
	}
	return ch
}

// NamespaceScopeNote returns a caveat for the verdict when the scan is scoped to
// a single namespace that excludes kube-system, so the system rollup could not
// run. Returns "" when the rollup was in scope (all namespaces, or -n kube-system).
func NamespaceScopeNote(namespace string) string {
	if namespace != "" && namespace != systemNamespace {
		return "node health only — re-run without -n (or with -n kube-system) for the system workload check"
	}
	return ""
}

// nodeHealth returns whether the node's Ready condition is true and a list of
// its problems ("NotReady", pressure types, "SchedulingDisabled").
func nodeHealth(n corev1.Node) (ready bool, issues []string) {
	for _, c := range n.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			ready = c.Status == corev1.ConditionTrue
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				issues = append(issues, string(c.Type))
			}
		}
	}
	if !ready {
		issues = append(issues, "NotReady")
	}
	if n.Spec.Unschedulable {
		issues = append(issues, "SchedulingDisabled")
	}
	return ready, issues
}
