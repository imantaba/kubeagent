package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/findings"
)

func sampleReport() Report {
	rep := Report{
		SchemaVersion: "1.0",
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

func TestRenderJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, sampleReport()); err != nil {
		t.Fatalf("RenderJSON() error = %v", err)
	}

	var got Report
	err := json.Unmarshal(buf.Bytes(), &got)
	// internal/findings.Level defines MarshalJSON (the spelling) but not the
	// matching UnmarshalJSON, so decoding FailOn back into that type is a
	// pre-existing gap in that package, not something a renderer can fix.
	// encoding/json still populates every other field before reporting the one
	// mismatch, so only that named field+type is tolerated here; any other
	// decode failure still fails the test below.
	if err != nil {
		te, ok := err.(*json.UnmarshalTypeError)
		if !ok || te.Field != "failOn" {
			t.Fatalf("unmarshalling: %v", err)
		}
	}
	if got.Verdict != "inconclusive" || got.Code != 2 {
		t.Errorf("verdict = %q/%d, want inconclusive/2", got.Verdict, got.Code)
	}
	if len(got.Unreachable) != 1 {
		t.Errorf("Unreachable = %+v, want the one unreachable cluster as its own array — a "+
			"consumer filtering clusters[] must not have to know some entries have no counts",
			got.Unreachable)
	}
	// A passing cluster carries no topIssues key at all.
	if strings.Contains(buf.String(), `"topIssues": []`) {
		t.Error("an empty topIssues array reached the document; omitempty must drop the key")
	}
}
