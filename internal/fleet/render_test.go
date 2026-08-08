package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
)

func sampleReport() Report {
	rep := Report{
		SchemaVersion: "1.2",
		FailOn:        findings.Critical,
		Clusters: []ClusterSummary{
			{Context: "example-staging-2", Verdict: "inconclusive", Warning: 1, Blindspots: 2},
			{Context: "example-eu-1", Verdict: "fail", Critical: 4, Warning: 2,
				TopIssues: []string{"CrashLoopBackOff", "ImagePullBackOff"}},
			{Context: "example-us-3", Verdict: "fail", Critical: 1, Warning: 5, Info: 1,
				TopIssues: []string{"Unschedulable"}},
			{Context: "example-eu-2", Verdict: "pass"},
		},
		Unreachable: []Unreachable{{Context: "example-ap-1", Reason: ReasonUnreachable}},
	}
	rep.Verdict, rep.Code = decide(rep.Clusters, rep.Unreachable)
	return rep
}

func TestRenderTextIsExactBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderText(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  5 clusters, 2 failing, 1 unreachable",
		"",
		"CLUSTER            VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"example-ap-1       unreachable                     connecting to the cluster",
		"example-staging-2  inconclusive     0     1     0  (2 blind spots)",
		"example-eu-1       fail             4     2     0  CrashLoopBackOff, ImagePullBackOff",
		"example-us-3       fail             1     5     1  Unschedulable",
		"example-eu-2       pass             0     0     0",
		"",
		"verdict: inconclusive (exit 2)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%s\nwant\n%s", got, want)
	}
}

// One blind spot, not "1 blind spots".
func TestRenderTextSingularBlindSpot(t *testing.T) {
	rep := Report{
		Verdict:  "inconclusive",
		Code:     2,
		Clusters: []ClusterSummary{{Context: "example-a", Verdict: "inconclusive", Blindspots: 1}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "(1 blind spot)") {
		t.Errorf("output = %q, want a singular blind spot", buf.String())
	}
}

// A blind spot and issue kinds are not exclusive; both belong in the column.
func TestRenderTextShowsIssuesAndBlindSpotsTogether(t *testing.T) {
	rep := Report{
		Verdict: "inconclusive",
		Code:    2,
		Clusters: []ClusterSummary{{
			Context: "example-a", Verdict: "inconclusive", Critical: 1, Blindspots: 3,
			TopIssues: []string{"OOMKilled"},
		}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "OOMKilled (3 blind spots)") {
		t.Errorf("output = %q, want both the issue kind and the blind-spot count", buf.String())
	}
}

// A context name can be far wider than the header, and the columns must still
// line up rather than run into each other.
func TestRenderTextWidensForALongContextName(t *testing.T) {
	long := "default/api-example-com:6443/kube:admin"
	rep := Report{
		Verdict:  "pass",
		Clusters: []ClusterSummary{{Context: long, Verdict: "pass"}, {Context: "example-a", Verdict: "pass"}},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.HasPrefix(line, "CLUSTER") || strings.HasPrefix(line, long) || strings.HasPrefix(line, "example-a") {
			if !strings.Contains(line[len(long):], "  ") {
				t.Errorf("line %q does not keep two spaces after the widest name", line)
			}
		}
	}
}

// TestRenderTextWidensForALongContextName above sets only Context, so
// identity == context there by construction: a width loop that read Context
// instead of the row identity would compute the same number for that
// fixture and this test would not notice. Here Name is longer than every
// Context in the report, so the two computations diverge, and only a width
// computed from the row identity keeps the columns lined up.
func TestRenderTextWidthFollowsRowIdentityNotContext(t *testing.T) {
	rep := Report{
		Verdict: "pass",
		Clusters: []ClusterSummary{
			{Name: "edge-far-west-1", Context: "default", Verdict: "pass"},
			{Context: "prod-eu", Verdict: "pass"},
		},
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  2 clusters, 0 failing, 0 unreachable",
		"",
		"CLUSTER          VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"edge-far-west-1  pass             0     0     0",
		"prod-eu          pass             0     0     0",
		"",
		"verdict: pass (exit 0)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%q\nwant\n%q", got, want)
	}
}

// There is no elision. The spec's sample line is documentation shorthand, not
// program output — three hundred clusters means three hundred rows.
func TestRenderTextElidesNothing(t *testing.T) {
	rep := Report{Verdict: "pass"}
	for i := 0; i < 40; i++ {
		rep.Clusters = append(rep.Clusters, ClusterSummary{
			Context: "example-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Verdict: "pass",
		})
	}
	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if strings.Contains(buf.String(), "more passing") {
		t.Error("output elides passing clusters; every selected cluster gets a row")
	}
	if got := strings.Count(buf.String(), "\n  ") + strings.Count(buf.String(), "example-"); got < 40 {
		t.Errorf("counted %d cluster rows, want 40", got)
	}
}

// The document's consumers are pipelines — jq, a script in another language —
// never a Go program decoding it back into Report: internal/fleet is
// unexported from this module, and kubeagent has no JSON decoder anywhere.
// So this test reads the bytes the way an actual consumer does, generically,
// rather than asserting a typed round trip nothing relies on.
func TestRenderJSONIsAValidDocument(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if doc["verdict"] != "inconclusive" || doc["exitCode"] != float64(2) {
		t.Errorf("verdict/exitCode = %v/%v, want inconclusive/2", doc["verdict"], doc["exitCode"])
	}
	if unreachable, ok := doc["unreachable"].([]any); !ok || len(unreachable) != 1 {
		t.Errorf("unreachable = %#v, want the one unreachable cluster as its own array — a "+
			"consumer filtering clusters[] must not have to know some entries have no counts",
			doc["unreachable"])
	}
	// A passing cluster carries no topIssues key at all.
	if strings.Contains(buf.String(), `"topIssues": []`) {
		t.Error("an empty topIssues array reached the document; omitempty must drop the key")
	}
	// findings.Level.MarshalJSON exists to guarantee the spelling reaches the
	// wire, never the ordinal — a generic decode is the only place in this
	// package that can actually check that promise held.
	if failOn, ok := doc["failOn"].(string); !ok || failOn != "critical" {
		t.Errorf("failOn = %#v, want the string %q, not a number", doc["failOn"], "critical")
	}
}

// flakyWriter fails the nth write and succeeds on every other one. A writer
// that stays failed once it fails would not prove anything here: the final
// write would fail too, so a renderer that checked only its last call would
// still return an error and look correct.
type flakyWriter struct {
	failAt int
	n      int
	err    error
}

func (f *flakyWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n == f.failAt {
		return 0, f.err
	}
	return len(p), nil
}

func TestRenderTextReportsAFailedWrite(t *testing.T) {
	boom := errors.New("disk full")

	for _, tc := range []struct {
		name string
		rep  Report
	}{
		{"without a correlation", sampleReport()},
		{"with both correlation sections", sharedReport()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many writes a full render makes, so the loop below covers every one.
			counter := &flakyWriter{failAt: -1}
			if err := RenderText(counter, tc.rep); err != nil {
				t.Fatalf("counting writes: %v", err)
			}
			if counter.n < 3 {
				t.Fatalf("counted %d writes; the fixture should render a header and several rows",
					counter.n)
			}

			for i := 1; i <= counter.n; i++ {
				w := &flakyWriter{failAt: i, err: boom}
				if err := RenderText(w, tc.rep); !errors.Is(err, boom) {
					t.Errorf("write %d of %d failed: err = %v, want %v", i, counter.n, err, boom)
				}
			}
		})
	}
}

// sharedReport is sampleReport with a correlation. Four judged clusters, so the
// denominator is 4 — the fifth is unreachable and contributed no evidence.
func sharedReport() Report {
	rep := sampleReport()
	rep.Shared = []Shared{
		{Signal: "ImagePullBackOff", Source: SourceIssue, Clusters: []string{
			"example-eu-1", "example-eu-2", "example-staging-2", "example-us-3"}},
		{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{
			"example-eu-1", "example-us-3"}},
		{Signal: "nodes/proxy", Source: SourceBlindspot, Clusters: []string{
			"example-eu-1", "example-staging-2"}},
	}
	return rep
}

func TestRenderTextWithCorrelationIsExactBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderText(&buf, sharedReport()); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  5 clusters, 2 failing, 1 unreachable",
		"",
		"CLUSTER            VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"example-ap-1       unreachable                     connecting to the cluster",
		"example-staging-2  inconclusive     0     1     0  (2 blind spots)",
		"example-eu-1       fail             4     2     0  CrashLoopBackOff, ImagePullBackOff",
		"example-us-3       fail             1     5     1  Unschedulable",
		"example-eu-2       pass             0     0     0",
		"",
		"SHARED ISSUES  in 2 or more of 4 judged clusters",
		"",
		"  4/4  ImagePullBackOff  example-eu-1, example-eu-2, example-staging-2, +1 more",
		"  2/4  OOMKilled         example-eu-1, example-us-3",
		"",
		"SHARED BLIND SPOTS  in 2 or more of 4 judged clusters",
		"",
		"  2/4  nodes/proxy       example-eu-1, example-staging-2",
		"",
		"verdict: inconclusive (exit 2)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%s\nwant\n%s", got, want)
	}
}

// A heading over nothing reads as a failed render, so a section with no entries
// is omitted entirely rather than printed empty.
func TestRenderTextOmitsASharedSectionWithNoEntries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shared      []Shared
		wantPresent string
		wantAbsent  string
	}{
		{
			name: "blind spots only",
			shared: []Shared{{Signal: "nodes/proxy", Source: SourceBlindspot,
				Clusters: []string{"example-a", "example-b"}}},
			wantPresent: "SHARED BLIND SPOTS",
			wantAbsent:  "SHARED ISSUES",
		},
		{
			name: "issues only",
			shared: []Shared{{Signal: "OOMKilled", Source: SourceIssue,
				Clusters: []string{"example-a", "example-b"}}},
			wantPresent: "SHARED ISSUES",
			wantAbsent:  "SHARED BLIND SPOTS",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Report{
				Verdict: "fail", Code: 1,
				Clusters: []ClusterSummary{
					{Context: "example-a", Verdict: "fail", Critical: 1},
					{Context: "example-b", Verdict: "fail", Critical: 1},
				},
				Shared: tc.shared,
			}
			var buf bytes.Buffer
			if err := RenderText(&buf, rep); err != nil {
				t.Fatalf("RenderText() error = %v", err)
			}
			if !strings.Contains(buf.String(), tc.wantPresent) {
				t.Errorf("output = %q, want a %s section", buf.String(), tc.wantPresent)
			}
			if strings.Contains(buf.String(), tc.wantAbsent) {
				t.Errorf("output = %q, want no %s heading over an empty section",
					buf.String(), tc.wantAbsent)
			}
		})
	}
}

// A signpost, not an inventory — the same reasoning that caps TopIssues at
// three. The JSON document carries every name.
func TestRenderTextNamesAtMostThreeClustersThenCountsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clusters []string
		wantLine string
	}{
		{"three are all named", []string{"example-a", "example-b", "example-c"},
			"  3/3  OOMKilled  example-a, example-b, example-c"},
		{"four name three and count one",
			[]string{"example-a", "example-b", "example-c", "example-d"},
			"  4/4  OOMKilled  example-a, example-b, example-c, +1 more"},
		{"six name three and count three",
			[]string{"example-a", "example-b", "example-c", "example-d", "example-e", "example-f"},
			"  6/6  OOMKilled  example-a, example-b, example-c, +3 more"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Report{Verdict: "pass", Shared: []Shared{
				{Signal: "OOMKilled", Source: SourceIssue, Clusters: tc.clusters},
			}}
			for _, c := range tc.clusters {
				rep.Clusters = append(rep.Clusters, ClusterSummary{Context: c, Verdict: "pass"})
			}

			var buf bytes.Buffer
			if err := RenderText(&buf, rep); err != nil {
				t.Fatalf("RenderText() error = %v", err)
			}
			if !strings.Contains(buf.String(), "\n"+tc.wantLine+"\n") {
				t.Errorf("output =\n%s\nwant a line %q", buf.String(), tc.wantLine)
			}
		})
	}
}

// The denominator is judged clusters, not selected ones. An unreachable cluster
// produced no verdict and could not have contributed a signal, so counting it
// would make a 2-of-2 correlation read as 2-of-5 and understate it.
func TestRenderTextSharedDenominatorCountsJudgedClustersOnly(t *testing.T) {
	rep := Report{
		Verdict: "inconclusive", Code: 2,
		Clusters: []ClusterSummary{
			{Context: "example-a", Verdict: "fail", Critical: 1},
			{Context: "example-b", Verdict: "fail", Critical: 1},
		},
		Unreachable: []Unreachable{
			{Context: "example-c", Reason: ReasonUnreachable},
			{Context: "example-d", Reason: ReasonTimedOut},
			{Context: "example-e", Reason: ReasonTimedOut},
		},
		Shared: []Shared{
			{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		},
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "in 2 or more of 2 judged clusters") {
		t.Errorf("output =\n%s\nwant a denominator of 2 — three unreachable clusters "+
			"produced no verdict and could not have contributed a signal", buf.String())
	}
	if !strings.Contains(buf.String(), "  2/2  OOMKilled") {
		t.Errorf("output =\n%s\nwant the count cell measured against judged clusters too", buf.String())
	}
}

func TestRenderJSONOmitsSharedWhenThereIsNoCorrelation(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	if strings.Contains(buf.String(), `"shared"`) {
		t.Errorf("document carries a shared key with no correlation; omitempty must drop it:\n%s",
			buf.String())
	}
}

// The text renderer names at most three clusters. The document names every one:
// a jq filter asking which clusters share a signal must get the answer, not a
// signpost.
func TestRenderJSONCarriesEverySharedClusterName(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sharedReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	shared, ok := doc["shared"].([]any)
	if !ok || len(shared) != 3 {
		t.Fatalf("shared = %#v, want three entries", doc["shared"])
	}
	first, ok := shared[0].(map[string]any)
	if !ok {
		t.Fatalf("shared[0] = %#v, want an object", shared[0])
	}
	if first["signal"] != "ImagePullBackOff" || first["source"] != "issue" {
		t.Errorf("shared[0] = %#v, want the most widespread issue first", first)
	}
	if clusters, ok := first["clusters"].([]any); !ok || len(clusters) != 4 {
		t.Errorf("clusters = %#v, want all four names — the +N more elision belongs to "+
			"the text renderer, not to the document", first["clusters"])
	}
	if strings.Contains(buf.String(), "more") {
		t.Errorf("the document carries the text renderer's elision:\n%s", buf.String())
	}
}

// The table names what the operator called each cluster, and the SHARED section
// names the same thing — an operator who cannot cross-reference the two has two
// reports rather than one. Compared byte for byte, because column padding is
// the whole point of this renderer.
func TestRenderTextNamesTheRowIdentity(t *testing.T) {
	rep := Report{
		SchemaVersion: "1.2",
		Verdict:       "fail",
		Code:          1,
		Clusters: []ClusterSummary{
			{Name: "edge-a", Context: "default", Verdict: "fail", Critical: 2,
				TopIssues: []string{"ImagePullBackOff", "OOMKilled"}},
			{Name: "edge-b", Context: "default", Verdict: "fail", Critical: 1,
				TopIssues: []string{"OOMKilled"}},
			{Context: "prod-eu", Verdict: "pass"},
			{Context: "prod-us", Verdict: "pass"},
		},
		Unreachable: []Unreachable{},
		Shared: []Shared{
			{Signal: "OOMKilled", Source: SourceIssue, Clusters: []string{"edge-a", "edge-b"}},
		},
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, rep); err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}

	want := strings.Join([]string{
		"FLEET  4 clusters, 2 failing, 0 unreachable",
		"",
		"CLUSTER  VERDICT       CRIT  WARN  INFO  TOP ISSUES",
		"edge-a   fail             2     0     0  ImagePullBackOff, OOMKilled",
		"edge-b   fail             1     0     0  OOMKilled",
		"prod-eu  pass             0     0     0",
		"prod-us  pass             0     0     0",
		"",
		"SHARED ISSUES  in 2 or more of 4 judged clusters",
		"",
		"  2/4  OOMKilled  edge-a, edge-b",
		"",
		"verdict: fail (exit 1)",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("RenderText() =\n%q\nwant\n%q", got, want)
	}
}

// An unreachable row is named by its identity too. Rendering "default" for a
// per-cluster kubeconfig would name nothing at all.
func TestRenderTextNamesAnUnreachableClusterByItsIdentity(t *testing.T) {
	var buf bytes.Buffer
	err := RenderText(&buf, Report{
		SchemaVersion: "1.2",
		Verdict:       "inconclusive",
		Code:          2,
		Clusters:      []ClusterSummary{{Context: "prod-eu", Verdict: "pass"}},
		Unreachable:   []Unreachable{{Name: "edge-a", Context: "default", Reason: ReasonUnreachable}},
	})
	if err != nil {
		t.Fatalf("RenderText() error = %v", err)
	}
	if !strings.Contains(buf.String(), "edge-a   unreachable") {
		t.Errorf("rendered =\n%s\nwant an unreachable row named edge-a", buf.String())
	}
	if strings.Contains(buf.String(), "default") {
		t.Errorf("rendered =\n%s\nwant no bare context where an identity was given", buf.String())
	}
}

// A sweep selected from a kubeconfig must encode no name key anywhere, so a
// consumer written against fleet 1.0 or 1.1 sees the document it expects.
func TestRenderJSONOmitsNameWhenNoNameDiffers(t *testing.T) {
	var buf bytes.Buffer
	err := RenderJSON(&buf, Report{
		SchemaVersion: "1.2",
		Verdict:       "inconclusive",
		Code:          2,
		Clusters:      []ClusterSummary{{Context: "prod-eu", Verdict: "pass"}},
		Unreachable:   []Unreachable{{Context: "prod-us", Reason: ReasonUnreachable}},
	})
	if err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}
	if strings.Contains(buf.String(), `"name"`) {
		t.Errorf("document =\n%s\nwant no name key when no name differs from its context", buf.String())
	}
}
