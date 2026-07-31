package htmlreport

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenNow is the fixed clock for the snapshot, so the fixture holds no
// time-varying bytes and the comparison is a plain byte comparison.
var goldenNow = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

const goldenPath = "testdata/golden-report.html"

// goldenInput exercises every rendered part of the document: all three severity
// levels, partial reads, cluster health with both issue lists, a workload table,
// and an --explain narrative.
func goldenInput() Input {
	return Input{
		Version:   "v0.66.0",
		Namespace: "shop",
		Report: report.Input{
			Now: goldenNow,
			Policy: &report.PolicyView{
				Rules: 3,
				Violations: []policy.Violation{
					{RuleID: "registry-allowlist", Level: policy.LevelCritical, Kind: "Pod",
						Namespace: "shop", Name: "checkout-7d9f",
						Message:  "image is not from an allowed registry",
						Evidence: "docker.example.net/checkout:2.1"},
					{RuleID: "pdb-required", Level: policy.LevelWarning, Kind: "Deployment",
						Namespace: "shop", Name: "checkout",
						Message: "no PodDisruptionBudget covers this Deployment"},
				},
				NotEvaluated: []policy.Unevaluated{{
					RuleID: "storage-encrypted", Level: policy.LevelCritical, Kind: "StorageClass",
					Reason: "kubeagent could not read this kind, so the rule was not evaluated",
				}},
			},
			Cluster: clusterhealth.ClusterHealth{
				Verdict: "Degraded", NodesTotal: 4, NodesReady: 2,
				NodeIssues: []string{
					"worker-2 NotReady: KubeletNotReady — container runtime is down",
					"worker-1 kubelet not heartbeating (lease 95s stale)",
				},
				SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
				ScopeNote:    "node health only — re-run without -n (or with -n kube-system) for the system workload check",
			},
			Result: inventory.Result{Workloads: []inventory.Workload{
				{Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 3, Ready: 0,
					Status: "Degraded", Image: "busybox:1.36", RootCause: "node worker-1 (kubelet not heartbeating)"},
				{Namespace: "shop", Kind: "Deployment", Name: "api", Desired: 2, Ready: 0,
					Status: "Degraded", Image: "nginx:9.9.9-nope"},
				{Namespace: "shop", Kind: "StatefulSet", Name: "data", Desired: 1, Ready: 1,
					Status: "Running", Image: "postgres:16"},
			}},
			Explanation: "The shop namespace is degraded because worker-1 stopped heartbeating, " +
				"which stalled web, and api references an image tag that does not exist.",
		},
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
				Issue: "CrashLoopBackOff", Reason: `container "web" repeatedly crashes after starting`,
				Owner: "Deployment/web"},
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "api",
				Issue: "ImagePullBackOff", Reason: `Back-off pulling image "nginx:9.9.9-nope": not found`,
				Owner: "Deployment/api"},
			{Level: findings.Warning, Kind: "Service", Namespace: "shop", Name: "payments",
				Issue: "NoEndpoints", Reason: "no ready endpoints"},
			{Level: findings.Info, Kind: "ResourceQuota", Namespace: "shop", Name: "compute",
				Issue: "nearing", Reason: "requests.cpu 3/4 used"},
		},
		Blind: []scan.ReadFailure{
			{Resource: "horizontalpodautoscalers", Reason: `forbidden: User cannot list resource "horizontalpodautoscalers"`},
		},
	}
}

func TestGoldenHTMLReport(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenInput()); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("HTML report output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/htmlreport -run TestGoldenHTMLReport -update\n"+
			"then re-read the diff: this document is shared outside the cluster, so a "+
			"new line in it is a disclosure decision.",
			firstDiff(string(want), string(got)))
	}
}

// TestGoldenInputCoversEverySection guards against the fixture silently losing a
// part, which would leave the golden a partial snapshot that still passes.
func TestGoldenInputCoversEverySection(t *testing.T) {
	in := goldenInput()
	if len(in.Blind) == 0 || len(in.Report.Cluster.NodeIssues) == 0 ||
		len(in.Report.Cluster.SystemIssues) == 0 || in.Report.Cluster.ScopeNote == "" ||
		len(in.Report.Result.Workloads) == 0 || in.Report.Explanation == "" {
		t.Fatal("goldenInput must populate every section so the golden stays comprehensive")
	}
	if in.Report.Policy == nil || len(in.Report.Policy.Violations) == 0 ||
		len(in.Report.Policy.NotEvaluated) == 0 {
		t.Fatal("goldenInput must populate the policy section, violations and unevaluated rules")
	}
	levels := map[findings.Level]bool{}
	for _, f := range in.Findings {
		levels[f.Level] = true
	}
	if !levels[findings.Critical] || !levels[findings.Warning] || !levels[findings.Info] {
		t.Fatalf("goldenInput must exercise all three severity levels, got %v", levels)
	}
}

// firstDiff returns the first differing line, for a readable failure message.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, wl, gl)
		}
	}
	return "(files differ only in trailing content)"
}
