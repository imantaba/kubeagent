package htmlreport

import (
	"bytes"
	"errors"
	htmlescape "html"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
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
		Blind: []scan.ReadFailure{{
			Resource: "pods",
			Reason: redact.Error(&url.Error{
				Op:  "Get",
				URL: "https://198.51.100.7:6443/api/v1/pods",
				Err: errors.New("dial tcp 198.51.100.7:6443: connect: connection refused"),
			}),
		}},
	})
	for _, banned := range []string{"://", "/home", ".kube", "kubeconfig", "--context", "198.51.100.7", "6443"} {
		if strings.Contains(got, banned) {
			t.Errorf("the document contains %q; it must carry no cluster identity and no external reference", banned)
		}
	}
}

// TestBlindReasonIsClassifiedNeverEchoed: the blind-spots block is the one place where
// cluster-produced free text could reach a document meant to be forwarded, and no filter
// over that text is safe. apierrors.NewForbidden interpolates whatever the authorizer
// returned, so an authorization message carries the username — on a real cluster that is
// an IAM ARN, a node's internal DNS name, or an OIDC email — and under webhook
// authorization it carries a third-party backend's words as well. So the reason is never
// echoed: it is read only to pick one of kubeagent's own phrases.
func TestBlindReasonIsClassifiedNeverEchoed(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "an RBAC denial",
			reason: `pods is forbidden: User "system:serviceaccount:shop:default" cannot list resource "pods" in API group "" in the namespace "shop"`,
			want:   reasonForbidden,
		},
		{
			name:   "an RBAC denial naming an IAM principal",
			reason: `pods is forbidden: User "arn:aws:iam::198510000007:user/alice" cannot list resource "pods" in API group "" in the namespace "shop"`,
			want:   reasonForbidden,
		},
		{
			name:   "a webhook authorizer appending its own words",
			reason: `pods is forbidden: User "alice" cannot list resource "pods": denied by policy backend authz.internal.example`,
			want:   reasonForbidden,
		},
		{
			name:   "a resource type the cluster does not serve",
			reason: "the server could not find the requested resource",
			want:   reasonNotServed,
		},
		{
			// kubectl's "doesn't have a resource type" wording is not one of
			// these: it comes from client-side RESTMapper resolution in a
			// package kubeagent does not depend on. It falls through to
			// reasonUnavailable, which is the safe direction.
			name:   "kubectl's missing-resource wording, which this binary never produces",
			reason: `the server doesn't have a resource type "verticalpodautoscalers"`,
			want:   reasonUnavailable,
		},
		{
			name:   "a transport failure",
			reason: "Get https://198.51.100.7:6443: dial tcp 198.51.100.7:6443: connect: connection refused",
			want:   reasonUnavailable,
		},
		{
			name:   "a timeout",
			reason: "the server was unable to return a response in the time allotted, but may still be processing the request",
			want:   reasonUnavailable,
		},
		{
			name:   "an empty reason",
			reason: "",
			want:   reasonUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeReason(tc.reason)
			if got != tc.want {
				t.Errorf("safeReason(%q) = %q, want %q", tc.reason, got, tc.want)
			}
			if got != reasonForbidden && got != reasonNotServed && got != reasonUnavailable {
				t.Errorf("safeReason returned %q, which is not one of kubeagent's own phrases", got)
			}
		})
	}
}

// TestRenderEchoesNoBlindSpotReasonText: the same guarantee asserted on the rendered
// bytes, so a template edit that reaches around safeReason is caught too. Every
// distinctive word of the cluster's message must be absent from the document.
func TestRenderEchoesNoBlindSpotReasonText(t *testing.T) {
	got := render(t, Input{
		Report: report.Input{Now: fixedNow},
		Blind: []scan.ReadFailure{
			{
				Resource: "pods in namespace shop",
				Reason:   `pods is forbidden: User "arn:aws:iam::198510000007:user/alice" cannot list resource "pods": denied by authz.internal.example`,
			},
			{
				Resource: "verticalpodautoscalers",
				Reason:   "the server could not find the requested resource",
			},
		},
	})

	for _, leak := range []string{"arn:aws:iam", "198510000007", "alice", "authz.internal.example", "cannot list resource"} {
		if strings.Contains(got, leak) {
			t.Errorf("rendered document contains %q, which came from the cluster's own error text", leak)
		}
	}
	// html/template escapes the apostrophe in reasonForbidden ("kubeagent's") to
	// &#39;, so the expectation is checked against the same escaped form a reader
	// of the document actually sees, not the raw Go string constant.
	for _, want := range []string{
		"pods in namespace shop", "verticalpodautoscalers",
		htmlescape.EscapeString(reasonForbidden), htmlescape.EscapeString(reasonNotServed),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in the document", want)
		}
	}
}

// TestRenderWithholdsEndpointsFromTheBlindSpotsBlock: the same guarantee, asserted on the
// rendered bytes rather than on the helper, so a template edit that reaches around
// safeReason is caught too.
func TestRenderWithholdsEndpointsFromTheBlindSpotsBlock(t *testing.T) {
	got := render(t, Input{
		Report: report.Input{Now: fixedNow},
		Blind: []scan.ReadFailure{{
			Resource: "pods in namespace shop",
			Reason: redact.Error(&url.Error{
				Op:  "Get",
				URL: "https://198.51.100.7:6443/api/v1/namespaces/shop/pods",
				Err: errors.New("dial tcp 198.51.100.7:6443: connect: connection refused"),
			}),
		}},
	})

	for _, leak := range []string{"198.51.100.7", "6443", "https://", "dial tcp"} {
		if strings.Contains(got, leak) {
			t.Errorf("rendered document contains %q, which identifies the cluster", leak)
		}
	}
	if !strings.Contains(got, "pods in namespace shop") {
		t.Error("the resource that could not be read should still be named")
	}
	if !strings.Contains(got, reasonUnavailable) {
		t.Errorf("want the withheld-reason phrase %q in the document", reasonUnavailable)
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

// TestRenderBlindSpotsBlock: a document that omits what kubeagent could not read
// is green-when-blind, and a rendered file is easier to over-trust than an exit
// code. The block must appear whenever there are partial reads — and only then.
// The two fixture reasons are an authorization message and a resource type the
// cluster does not serve; safeReason classifies each to its own kubeagent phrase
// rather than echoing the original text.
func TestRenderBlindSpotsBlock(t *testing.T) {
	with := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Blind: []scan.ReadFailure{
			{Resource: "pods in namespace restricted", Reason: `forbidden: User cannot list resource "pods"`},
			{Resource: "horizontalpodautoscalers", Reason: "the server could not find the requested resource"},
		},
	})
	if !strings.Contains(with, "Blind spots") {
		t.Error("partial reads did not render a blind-spots block")
	}
	// html/template escapes the apostrophe in reasonForbidden ("kubeagent's") to
	// &#39;, so the expectation is checked against that escaped form.
	for _, want := range []string{
		"pods in namespace restricted", "horizontalpodautoscalers",
		htmlescape.EscapeString(reasonForbidden), htmlescape.EscapeString(reasonNotServed),
	} {
		if !strings.Contains(with, want) {
			t.Errorf("blind-spots block is missing %q", want)
		}
	}
	without := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(without, "Blind spots") {
		t.Error("a scan with no partial reads must not render an empty blind-spots block")
	}
}

// TestRenderSeverityFilterIsPureCSS pins the mechanism, not just the appearance:
// the filter must be :checked sibling selectors, because the document has no
// JavaScript and must keep working where script is blocked.
func TestRenderSeverityFilterIsPureCSS(t *testing.T) {
	got := render(t, Input{
		Report:  report.Input{Now: fixedNow},
		Version: "v0.66.0",
		Findings: []findings.Finding{
			{Level: findings.Critical, Kind: "Deployment", Name: "a", Issue: "CrashLoopBackOff"},
			{Level: findings.Warning, Kind: "Service", Name: "b", Issue: "NoEndpoints"},
		},
	})
	for _, want := range []string{
		`id="f-all"`, `id="f-warn"`, `id="f-crit"`,
		"#f-crit:checked ~ table tr.warning",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("severity filter is missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(got), "<script") {
		t.Error("the severity filter must be pure CSS — the document must stay script-free")
	}
	if !strings.Contains(got, "Critical 1") || !strings.Contains(got, "Warning and above 2") {
		t.Error("the filter controls must be labelled with their counts")
	}

	// The rules above are written as `#f-crit:checked ~ table ...`. `~` is the
	// general-sibling combinator, which only matches elements that come AFTER
	// the reference element in document order. So the three radio inputs must
	// be emitted before the <table> they filter, or the selectors never match
	// anything and every row stays visible in every mode — silently, since the
	// selector text and label text (asserted above) would still be present.
	table := strings.Index(got, "<table")
	if table < 0 {
		t.Fatal("findings table is missing from the document")
	}
	for _, id := range []string{`id="f-all"`, `id="f-warn"`, `id="f-crit"`} {
		pos := strings.Index(got, id)
		if pos < 0 {
			t.Fatalf("radio input %q is missing from the document", id)
		}
		if pos > table {
			t.Errorf("radio input %q is emitted after <table> (input at byte %d, table at byte %d): "+
				"the `~` general-sibling combinator matches only following siblings, so a radio "+
				"placed after the table it filters can never match it via ~, and the CSS-only "+
				"severity filter breaks — every row stays visible regardless of which radio is checked",
				id, pos, table)
		}
	}
}

// TestRenderDetailSections: the detail lives behind collapsed <details> so the
// findings stay above the fold, but it must actually be in the document.
func TestRenderDetailSections(t *testing.T) {
	got := render(t, Input{
		Report: report.Input{
			Now: fixedNow,
			Cluster: clusterhealth.ClusterHealth{
				Verdict: "Degraded", NodesTotal: 3, NodesReady: 2,
				NodeIssues:   []string{"worker-2 NotReady: KubeletNotReady"},
				SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
			},
			Result: inventory.Result{Workloads: []inventory.Workload{{
				Namespace: "shop", Kind: "Deployment", Name: "web", Desired: 3, Ready: 1,
				Status: "Degraded", Image: "busybox:1.36", RootCause: "node worker-2 (NotReady)",
			}}},
		},
		Version: "v0.66.0",
	})
	if strings.Count(got, "<details") < 2 {
		t.Errorf("want at least two <details> sections, got %d", strings.Count(got, "<details"))
	}
	if strings.Contains(got, "<details open") {
		t.Error("detail sections must be collapsed by default so the findings stay above the fold")
	}
	for _, want := range []string{
		"Cluster health", "Degraded", "2/3",
		"worker-2 NotReady: KubeletNotReady", "kube-system/coredns Degraded 1/2",
		"Workload inventory", "busybox:1.36", "node worker-2 (NotReady)", "1/3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detail sections are missing %q", want)
		}
	}
}

// TestRenderExplanationOnlyWhenPresent: --explain produces one plain-English
// paragraph worth carrying into a shared document. Without the flag there is no
// narrative, and an empty section would read as a failure.
func TestRenderExplanationOnlyWhenPresent(t *testing.T) {
	with := render(t, Input{
		Report:  report.Input{Now: fixedNow, Explanation: "The shop namespace is down because worker-2 stopped heartbeating."},
		Version: "v0.66.0",
	})
	if !strings.Contains(with, "worker-2 stopped heartbeating") {
		t.Error("the --explain narrative did not reach the document")
	}
	without := render(t, Input{Report: report.Input{Now: fixedNow}, Version: "v0.66.0"})
	if strings.Contains(without, "Explanation") {
		t.Error("a scan without --explain must not render an empty Explanation section")
	}
}
