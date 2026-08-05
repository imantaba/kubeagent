package dashboard

import (
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenPath = "testdata/golden-dashboard.html"

// goldenInput is the fixture: two clusters, one of them unreachable, active and
// resolved incidents across both, SLO on, and explanations present — so the
// snapshot exercises every section of the page in one comparison.
//
// Every value is an RFC 2606 name or an obviously synthetic label. Nothing here
// is named like a credential, and no value is a path, a URL or an address.
func goldenInput() Input {
	return Input{
		Version: "v1.2.0",
		Now:     fixedNow,
		Clusters: []Cluster{
			{Name: "example-a", Up: true, LastScan: "2026-08-02T09:29:30Z"},
			{Name: "example-b", Up: false, LastScan: "2026-08-02T09:14:00Z", Error: "connection refused"},
			{Name: "example-c"},
		},
		Active: []Incident{
			{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff",
				Firings: 2, AgeSeconds: 1800},
			{Cluster: "example-a", Kind: "Pod", Namespace: "example-ns", Name: "worker-0", Issue: "CrashLoopBackOff",
				Firings: 7, Flapping: true, AgeSeconds: 3600},
			{Cluster: "example-b", Kind: "Node", Name: "node-1", Issue: "NotReady",
				Firings: 1, AgeSeconds: 300},
		},
		Resolved: []Incident{
			{Cluster: "example-a", Kind: "StatefulSet", Namespace: "example-ns", Name: "cache", Issue: "Degraded",
				Firings: 1, ResolvedAt: "2026-08-02T07:45:00Z", ResolutionSeconds: 2700},
			{Cluster: "example-b", Kind: "Service", Namespace: "example-ns", Name: "api", Issue: "NoEndpoints",
				Firings: 3, ResolvedAt: "2026-08-02T08:20:00Z", ResolutionSeconds: 600},
		},
		Stats: Stats{
			NewTotal: 9, ResolvedTotal: 6, FlapTotal: 2, DroppedTotal: 1,
			ResolutionSecondsSum: 9900, ResolutionSecondsCount: 6,
		},
		SLO: []SLO{{
			Cluster: "example-a",
			Target:  0.999,
			Windows: []SLOWindow{
				{Name: "fast (1h)", Availability: 0.982, BurnRate: 18, Coverage: 0.45},
				{Name: "slow (6h)", Availability: 0.9993, BurnRate: 0.7, Coverage: 0.98},
			},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web",
			Issues:      []string{"ImagePullBackOff"},
			ExplainedAt: "2026-08-02T09:05:00Z",
			Model:       "example-model",
			Text:        "The image tag referenced by the Deployment does not exist in the registry.\nRoll back to the previous tag, or push the missing tag.",
		}},
	}
}

// TestGoldenDashboard snapshots the whole page. The markup is an unstable
// surface — website/docs/compatibility.md says so — and this fixture is a
// regression guard that keeps a change to it deliberate, not a promise to a
// consumer. Anyone who wants a contract parses /issues, which is versioned.
func TestGoldenDashboard(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, goldenInput()); err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("writing %s: %v", goldenPath, err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("dashboard output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/dashboard -run TestGoldenDashboard -update\n"+
			"and review the diff.", firstDiff(string(want), string(got)))
	}
}

// TestGoldenInputCoversEverySection stops the fixture from quietly narrowing.
// A golden test over an input that exercises three of seven sections is a
// regression guard over three of seven sections.
func TestGoldenInputCoversEverySection(t *testing.T) {
	out := render(t, goldenInput())
	for _, section := range []string{
		"<h2>Clusters",
		"<h2>Active incidents",
		"<h2>Resolved incidents",
		"<h2>Totals",
		"<h2>SLO",
		"<h2>Explanations",
	} {
		if !strings.Contains(out, section) {
			t.Errorf("the golden input does not exercise the %q section", section)
		}
	}
	if !strings.Contains(out, "unreachable") {
		t.Error("the golden input has no unreachable cluster")
	}
	if !strings.Contains(out, "not scanned yet") {
		t.Error("the golden input has no cluster in the starting state")
	}
	if !strings.Contains(out, "<th>Cluster</th>") {
		t.Error("the golden input is not multicluster, so the Cluster column is never exercised")
	}
	if !strings.Contains(out, "burn alert suppressed") {
		t.Error("the golden input never exercises the coverage suppression note")
	}
}

// firstDiff names the first line where two renders diverge, so a failure points
// at a line instead of printing two pages.
func firstDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + a + "\n  got:  " + b
		}
	}
	return "(no line differs — check trailing bytes)"
}
