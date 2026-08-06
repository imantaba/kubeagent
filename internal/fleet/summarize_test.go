package fleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

func finding(level findings.Level, issue string) findings.Finding {
	return findings.Finding{Level: level, Kind: "Pod", Namespace: "ns", Name: "pod", Issue: issue}
}

func TestSummarizeCountsByLevelAcrossFailingAndReported(t *testing.T) {
	v := gate.Verdict{
		Verdict: "fail",
		Failing: []findings.Finding{
			finding(findings.Critical, "CrashLoopBackOff"),
			finding(findings.Critical, "CrashLoopBackOff"),
		},
		Reported: []findings.Finding{
			finding(findings.Warning, "Unschedulable"),
			finding(findings.Info, "Unschedulable"),
		},
		Inconclusive: []gate.Blindspot{{Resource: "nodes", Reason: "forbidden"}},
	}

	got := summarize("example-context", v)

	want := ClusterSummary{
		Context:    "example-context",
		Verdict:    "fail",
		Critical:   2,
		Warning:    1,
		Info:       1,
		Blindspots: 1,
		TopIssues:  []string{"CrashLoopBackOff", "Unschedulable"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summarize() = %+v, want %+v", got, want)
	}
}

// A pass carries no issues at all, and TopIssues must be nil rather than an
// empty slice so `omitempty` drops the key from the JSON document.
func TestSummarizeOfAPassIsAllZeroAndOmitsTopIssues(t *testing.T) {
	got := summarize("example-context", gate.Verdict{Verdict: "pass"})

	if got.TopIssues != nil {
		t.Errorf("TopIssues = %v, want nil so omitempty drops the key", got.TopIssues)
	}
	if got.Critical+got.Warning+got.Info+got.Blindspots != 0 {
		t.Errorf("summarize() = %+v, want every count zero", got)
	}
}

// Three, and the fourth-most-common kind is dropped rather than truncating the
// column at render time — the cap belongs to the document, not to one renderer.
func TestSummarizeCapsTopIssuesAtThreeMostFrequentFirst(t *testing.T) {
	var fs []findings.Finding
	for i := 0; i < 4; i++ {
		fs = append(fs, finding(findings.Critical, "aaa"))
	}
	for i := 0; i < 3; i++ {
		fs = append(fs, finding(findings.Critical, "bbb"))
	}
	for i := 0; i < 2; i++ {
		fs = append(fs, finding(findings.Critical, "ccc"))
	}
	fs = append(fs, finding(findings.Critical, "ddd"))

	got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})

	want := []string{"aaa", "bbb", "ccc"}
	if !reflect.DeepEqual(got.TopIssues, want) {
		t.Errorf("TopIssues = %v, want %v", got.TopIssues, want)
	}
}

// Equal counts must not order by map iteration, which Go randomizes per run.
func TestSummarizeBreaksTopIssueTiesByNameAscending(t *testing.T) {
	fs := []findings.Finding{
		finding(findings.Critical, "zebra"),
		finding(findings.Critical, "alpha"),
		finding(findings.Critical, "mike"),
	}

	for i := 0; i < 50; i++ {
		got := summarize("example-context", gate.Verdict{Verdict: "fail", Failing: fs})
		want := []string{"alpha", "mike", "zebra"}
		if !reflect.DeepEqual(got.TopIssues, want) {
			t.Fatalf("run %d: TopIssues = %v, want %v", i, got.TopIssues, want)
		}
	}
}

// The report names clusters and issue kinds. A namespace, pod, workload or node
// name reaching it would be a credential leak, and the defence is structural:
// summarize reads Level and Issue and nothing else. This test proves the
// structure holds by marking every other field and looking for the markers.
func TestSummarizeCarriesNoObjectName(t *testing.T) {
	const marker = "MARKERVALUE"
	v := gate.Verdict{
		Verdict: "fail",
		Failing: []findings.Finding{{
			Level:     findings.Critical,
			Kind:      marker + "Kind",
			Namespace: marker + "Namespace",
			Name:      marker + "Name",
			Issue:     "CrashLoopBackOff",
			Reason:    marker + "Reason",
			Owner:     marker + "Owner",
		}},
		Inconclusive: []gate.Blindspot{{Resource: marker + "Resource", Reason: marker + "BlindReason"}},
	}

	got := summarize("example-context", v)

	rendered := strings.Join(append([]string{got.Context, got.Verdict}, got.TopIssues...), " ")
	if strings.Contains(rendered, marker) {
		t.Errorf("summary %+v carries a marker; the report must name clusters and issue kinds only", got)
	}
}

// One total order, used by both renderers and by the JSON document: verdict
// rank first, then critical, warning and info counts descending, then context
// name ascending. Context names are unique within a kubeconfig, so the last
// tiebreak makes the order total — two runs over the same fleet cannot differ.
func TestSortSummariesIsWorstFirstAndTotal(t *testing.T) {
	in := []ClusterSummary{
		{Context: "b-pass", Verdict: "pass"},
		{Context: "a-fail-low", Verdict: "fail", Critical: 1},
		{Context: "z-inconclusive", Verdict: "inconclusive", Blindspots: 1},
		{Context: "a-pass", Verdict: "pass"},
		{Context: "b-fail-high", Verdict: "fail", Critical: 4},
		{Context: "c-fail-tie", Verdict: "fail", Critical: 1},
	}

	sortSummaries(in)

	want := []string{"z-inconclusive", "b-fail-high", "a-fail-low", "c-fail-tie", "a-pass", "b-pass"}
	var got []string
	for _, s := range in {
		got = append(got, s.Context)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// inconclusive outranks fail, mirroring gate.Decide's own switch, where the
// blind case is evaluated before the failing case. When kubeagent could not see
// enough, a "fail" may understate what is actually wrong — so the honest fleet
// answer is that the run could not judge. Inverting this would let one
// unreachable cluster hide behind another cluster's failure.
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		clusters    []ClusterSummary
		unreachable []Unreachable
		wantVerdict string
		wantCode    int
	}{
		{"everything passed", []ClusterSummary{{Verdict: "pass"}, {Verdict: "pass"}}, nil, "pass", 0},
		{"one failing", []ClusterSummary{{Verdict: "pass"}, {Verdict: "fail"}}, nil, "fail", 1},
		{"one inconclusive", []ClusterSummary{{Verdict: "pass"}, {Verdict: "inconclusive"}}, nil, "inconclusive", 2},
		{"inconclusive outranks fail", []ClusterSummary{{Verdict: "fail"}, {Verdict: "inconclusive"}}, nil, "inconclusive", 2},
		{"unreachable outranks fail", []ClusterSummary{{Verdict: "fail"}}, []Unreachable{{Context: "x"}}, "inconclusive", 2},
		{"nothing selected is not a pass to invent", nil, nil, "pass", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, code := decide(tc.clusters, tc.unreachable)
			if verdict != tc.wantVerdict || code != tc.wantCode {
				t.Errorf("decide() = %q/%d, want %q/%d", verdict, code, tc.wantVerdict, tc.wantCode)
			}
		})
	}
}
