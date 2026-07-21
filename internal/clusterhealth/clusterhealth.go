// Package clusterhealth derives a one-line cluster verdict from node health
// and a kube-system workload rollup.
package clusterhealth

import (
	"fmt"
	"sort"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const systemNamespace = "kube-system"

// ClusterHealth is the first-line cluster verdict.
type ClusterHealth struct {
	Verdict              string     `json:"verdict"` // Healthy | Degraded
	NodesTotal           int        `json:"nodesTotal"`
	NodesReady           int        `json:"nodesReady"`
	NodesStaleHeartbeat  int        `json:"nodesStaleHeartbeat,omitempty"`
	NodesExpectedAbsent  int        `json:"nodesExpectedAbsent,omitempty"`
	NodeIssues           []string   `json:"nodeIssues,omitempty"`
	SystemIssues         []string   `json:"systemIssues,omitempty"`
	ScopeNote            string     `json:"scopeNote,omitempty"`
	DownNodes            []DownNode `json:"downNodes,omitempty"`
}

// DownNode is a node that is effectively down — NotReady, or Ready but its
// kubelet has stopped heartbeating. Used to attribute workload failures to their
// node (internal/rootcause).
type DownNode struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // "NotReady" | "kubelet not heartbeating"
}

// Heartbeat carries the node-lease inputs for the kubelet-heartbeat-freshness
// check. A Threshold <= 0 disables the check.
type Heartbeat struct {
	Leases    []coordinationv1.Lease
	Now       time.Time
	Threshold time.Duration
}

// Assess computes the verdict from nodes and the assembled workloads. A node is
// unhealthy if not Ready, under Memory/Disk/PID pressure, cordoned, or (when the
// heartbeat check is enabled) Ready but its kubelet lease is stale/missing. When
// expected is non-empty, any declared name with no Node object is also flagged.
// The verdict is Healthy only when there are no node and no system issues.
func Assess(nodes []corev1.Node, hb Heartbeat, expected []string, workloads []inventory.Workload) ClusterHealth {
	ch := ClusterHealth{NodesTotal: len(nodes)}
	leaseByNode := make(map[string]coordinationv1.Lease, len(hb.Leases))
	for _, l := range hb.Leases {
		leaseByNode[l.Name] = l
	}
	present := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		present[n.Name] = true
		ready, issues := nodeHealth(n)
		if !ready {
			ch.DownNodes = append(ch.DownNodes, DownNode{Name: n.Name, Reason: "NotReady"})
		}
		if ready {
			ch.NodesReady++
			if hb.Threshold > 0 {
				if iss, stale := staleHeartbeat(leaseByNode, n.Name, hb.Now, hb.Threshold); stale {
					issues = append(issues, iss)
					ch.NodesStaleHeartbeat++
					if len(issues) == 1 {
						// Only treat as hard-down when the sole issue is the stale heartbeat
						// itself (no pressure, no cordon). A cordoned or pressured node that
						// also has a missing lease is still just "degraded", not "hard-down".
						ch.DownNodes = append(ch.DownNodes, DownNode{Name: n.Name, Reason: "kubelet not heartbeating"})
					}
				}
			}
		}
		for _, iss := range issues {
			ch.NodeIssues = append(ch.NodeIssues, n.Name+" "+iss)
		}
	}
	for _, name := range cleanExpected(expected) {
		if !present[name] {
			ch.NodeIssues = append(ch.NodeIssues, name+" expected but absent from the cluster")
			ch.NodesExpectedAbsent++
		}
	}
	for _, w := range workloads {
		if w.Namespace == systemNamespace && w.Flagged() {
			if w.Kind == "Job" || w.Kind == "CronJob" {
				ch.SystemIssues = append(ch.SystemIssues,
					fmt.Sprintf("%s/%s %s", w.Namespace, w.Name, w.Status))
			} else {
				ch.SystemIssues = append(ch.SystemIssues,
					fmt.Sprintf("%s/%s %d/%d %s", w.Namespace, w.Name, w.Ready, w.Desired, w.Status))
			}
		}
	}
	if len(ch.NodeIssues) == 0 && len(ch.SystemIssues) == 0 {
		ch.Verdict = "Healthy"
	} else {
		ch.Verdict = "Degraded"
	}
	return ch
}

// cleanExpected trims, drops blanks, dedups, and sorts the declared expected
// node names.
func cleanExpected(expected []string) []string {
	seen := make(map[string]bool, len(expected))
	var out []string
	for _, name := range expected {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// staleHeartbeat reports whether a Ready node's kubelet lease is stale (or
// missing/renewTime-less) beyond the threshold, and the issue string to record.
func staleHeartbeat(leaseByNode map[string]coordinationv1.Lease, node string, now time.Time, threshold time.Duration) (string, bool) {
	l, ok := leaseByNode[node]
	if !ok || l.Spec.RenewTime == nil {
		return "no kubelet lease", true
	}
	staleness := now.Sub(l.Spec.RenewTime.Time)
	if staleness > threshold {
		return fmt.Sprintf("kubelet not heartbeating (lease %s stale)", staleness.Round(time.Second)), true
	}
	return "", false
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
// its problems. The NotReady issue is enriched with the NodeReady condition's
// reason and (trimmed) message so the output names the cause, not just "NotReady".
func nodeHealth(n corev1.Node) (ready bool, issues []string) {
	var readyReason, readyMessage string
	for _, c := range n.Status.Conditions {
		switch c.Type {
		case corev1.NodeReady:
			ready = c.Status == corev1.ConditionTrue
			readyReason, readyMessage = c.Reason, c.Message
		case corev1.NodeMemoryPressure, corev1.NodeDiskPressure, corev1.NodePIDPressure:
			if c.Status == corev1.ConditionTrue {
				issues = append(issues, string(c.Type))
			}
		}
	}
	if !ready {
		issues = append(issues, notReadyIssue(readyReason, readyMessage))
	}
	if n.Spec.Unschedulable {
		issues = append(issues, "SchedulingDisabled")
	}
	return ready, issues
}

// notReadyIssue builds the NotReady issue string, adding the NodeReady
// condition's reason and trimmed message when present.
func notReadyIssue(reason, message string) string {
	s := "NotReady"
	m := trimLine(message, 120)
	switch {
	case reason != "" && m != "":
		s += ": " + reason + " — " + m
	case reason != "":
		s += ": " + reason
	case m != "":
		s += ": " + m
	}
	return s
}

// trimLine returns the first line of s, trimmed of surrounding space and
// truncated to max runes with a trailing ellipsis when longer.
func trimLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
