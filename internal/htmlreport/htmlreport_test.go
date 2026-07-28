package htmlreport

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/report"
)

// fixedNow is the clock every test that cares about the timestamp injects.
var fixedNow = time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

// render is the test helper: it renders and fails the test on any error, so the
// assertions below read as assertions about the document, not about plumbing.
func render(t *testing.T, in Input) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// TestRenderEscapesClusterControlledText is the load-bearing security test. A
// container termination message or image-pull error is a free-form string the
// cluster controls, and it lands verbatim in a document someone mails to a
// colleague. This test fails if anyone swaps html/template for text/template.
func TestRenderEscapesClusterControlledText(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{{
			Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
			Issue:  "CrashLoopBackOff",
			Reason: `<script>alert(1)</script> exited with "status 2" & no retry`,
		}},
	})
	if strings.Contains(got, "<script>alert(1)</script>") {
		t.Error("the raw <script> tag reached the document — cluster-controlled text is not being escaped")
	}
	for _, want := range []string{"&lt;script&gt;", "&#34;status 2&#34;", "&amp;"} {
		if !strings.Contains(got, want) {
			t.Errorf("escaped form %q missing from the document", want)
		}
	}
}

// TestRenderCarriesNoScript guards the CSP property: the document must be inert.
func TestRenderCarriesNoScript(t *testing.T) {
	got := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(strings.ToLower(got), "<script") {
		t.Error("the document contains a <script> tag; it must be inert to survive a strict CSP")
	}
}

// TestRenderPreservesFindingOrder pins that Render never re-sorts. findings.Flatten
// already calls findings.Sort; a second ordering here would be free to drift from it.
func TestRenderPreservesFindingOrder(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "zeta-first", Issue: "CrashLoopBackOff"},
			{Level: findings.Warning, Kind: "Service", Namespace: "shop", Name: "alpha-second", Issue: "NoEndpoints"},
			{Level: findings.Info, Kind: "Pod", Namespace: "shop", Name: "mid-third", Issue: "Noted"},
		},
	})
	first, second, third :=
		strings.Index(got, "zeta-first"), strings.Index(got, "alpha-second"), strings.Index(got, "mid-third")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("a finding is missing from the document: %d %d %d", first, second, third)
	}
	if !(first < second && second < third) {
		t.Errorf("Render reordered the findings: positions %d, %d, %d — it must render the slice as handed", first, second, third)
	}
}

// TestRenderCountsBySeverity checks the header tally, which is what a reader
// skims before anything else.
func TestRenderCountsBySeverity(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Name: "a", Issue: "CrashLoopBackOff"},
			{Level: findings.Critical, Kind: "Deployment", Name: "b", Issue: "ImagePullBackOff"},
			{Level: findings.Warning, Kind: "Service", Name: "c", Issue: "NoEndpoints"},
		},
	})
	for _, want := range []string{"2 critical", "1 warning", "0 info"} {
		if !strings.Contains(got, want) {
			t.Errorf("header tally missing %q", want)
		}
	}
}

// TestRenderHeaderCarriesVersionAndScope covers both the all-namespaces and the
// scoped case, since the scope line is the only thing telling a reader how much
// of the cluster this document actually covers.
func TestRenderHeaderCarriesVersionAndScope(t *testing.T) {
	all := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if !strings.Contains(all, "v0.66.0") {
		t.Error("header is missing the kubeagent version")
	}
	if !strings.Contains(all, "all namespaces") {
		t.Error(`an unscoped scan must say "all namespaces"`)
	}
	if !strings.Contains(all, "2026-07-28 09:30:00 UTC") {
		t.Error("header is missing the injected generation timestamp")
	}
	scoped := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0", Namespace: "shop"})
	if !strings.Contains(scoped, "namespace shop") {
		t.Error(`a scan with -n shop must say "namespace shop"`)
	}
	if strings.Contains(scoped, "all namespaces") {
		t.Error("a scoped scan must not claim to cover all namespaces")
	}
}

// TestRenderEmptyClusterStatesItExplicitly: a headless table reads as a broken
// report. A clean cluster must say so.
func TestRenderEmptyClusterStatesItExplicitly(t *testing.T) {
	got := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if !strings.Contains(got, "No findings") {
		t.Error("a scan with no findings must say so explicitly")
	}
	if strings.Contains(got, "<tbody>") {
		t.Error("an empty findings set must not render a headless table")
	}
}

// TestRenderLeaksNoIdentity: the document is shared, so it must disclose nothing
// about how kubeagent reached the cluster. The fixture below is deliberately free
// of paths and URLs, so any hit came from the renderer, not from the data.
func TestRenderLeaksNoIdentity(t *testing.T) {
	got := render(t, Input{
		Report:    report.Input{Now: fixedNow},
		Version:   "v0.66.0",
		Namespace: "shop",
		Findings: []findings.Finding{{
			Level: findings.Critical, Kind: "Deployment", Namespace: "shop", Name: "web",
			Issue: "CrashLoopBackOff", Reason: "container web exited with status 2",
		}},
	})
	for _, banned := range []string{"://", "/home", ".kube", "kubeconfig", "--context"} {
		if strings.Contains(got, banned) {
			t.Errorf("the document contains %q; it must carry no cluster identity and no external reference", banned)
		}
	}
}

// TestRenderZeroClockFallsBackToWallClock mirrors report.Input.Now's documented
// contract, so a caller that forgets the clock gets today, not year 1.
func TestRenderZeroClockFallsBackToWallClock(t *testing.T) {
	got := render(t, Input{Version: "v0.66.0"})
	if strings.Contains(got, "0001-01-01") {
		t.Error("a zero Now rendered a year-1 timestamp; it must fall back to wall-clock")
	}
}

// errWriter fails every write, standing in for `kubeagent scan --output html | head`.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestRenderReturnsWriteErrors: a closed downstream pipe must surface, not be swallowed.
func TestRenderReturnsWriteErrors(t *testing.T) {
	err := Render(errWriter{}, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if err == nil {
		t.Fatal("Render swallowed a write error")
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("Render lost the underlying write error: %v", err)
	}
}
