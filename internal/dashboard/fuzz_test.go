package dashboard

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// FuzzDashboardRender asserts the renderer's whole postcondition on arbitrary
// input: it never errors, it never panics, it is deterministic, and no angle
// bracket originating in the input reaches the output.
//
// The bracket count is the load-bearing assertion. Comparing the page against
// the same page rendered from input with every '<' and '>' stripped isolates
// the brackets the template itself emits from the ones the input contributed:
// html/template escapes an input bracket to an entity, so the two counts must
// agree exactly. A spelling-based check ("does the output contain <script")
// only catches the payloads someone thought of.
func FuzzDashboardRender(f *testing.F) {
	f.Add("v1.2.0", "example-cluster", "connection refused", "Deployment", "example-ns", "web", "ImagePullBackOff", "the image tag does not exist", int64(1800), int64(600), 9900.0, int64(6), 0.999, 1.5, 0.8)
	f.Add("", "", "", "", "", "", "", "", int64(0), int64(0), 0.0, int64(0), 0.0, 0.0, 0.0)
	f.Add("<script>alert(1)</script>", "<script>", "\"><img src=x onerror=alert(1)>", "&", "'", "</style>", "</pre>", "<!--", int64(-1), int64(-1), -1.0, int64(-1), 1.0, math.Inf(1), math.NaN())
	f.Add("a\x00b", "\x1b[2J", "bad\xffbyte", "before‮after", "tiếng Việt", "é́́", "  ", "\n\n", int64(1)<<62, int64(1)<<62, math.MaxFloat64, int64(1), 1e308, math.Inf(-1), 1e308)
	f.Add("{{ . }}", "{{ template \"x\" }}", "{{/*", "*/}}", "%s", "%!v(PANIC=", "\\", "\"", int64(86400), int64(86400), 1e15, int64(1), 0.5, 0.0, 0.6)

	f.Fuzz(func(t *testing.T,
		version, cluster, clusterErr, kind, namespace, name, issue, text string,
		age, resolution int64,
		sum float64, count int64,
		target, burn, coverage float64,
	) {
		in := fuzzInput(version, cluster, clusterErr, kind, namespace, name, issue, text,
			age, resolution, sum, count, target, burn, coverage)

		var buf bytes.Buffer
		if err := Render(&buf, in); err != nil {
			t.Fatalf("Render returned an error: %v", err)
		}
		got := buf.String()

		// Determinism: a second render of the same value must produce the same
		// bytes. Map iteration is the classic way this fails.
		var again bytes.Buffer
		if err := Render(&again, in); err != nil {
			t.Fatalf("second Render returned an error: %v", err)
		}
		if again.String() != got {
			t.Fatal("Render is not deterministic for this input")
		}

		// No input bracket survives as a bracket.
		clean := fuzzInput(
			stripBrackets(version), stripBrackets(cluster), stripBrackets(clusterErr),
			stripBrackets(kind), stripBrackets(namespace), stripBrackets(name),
			stripBrackets(issue), stripBrackets(text),
			age, resolution, sum, count, target, burn, coverage)
		var base bytes.Buffer
		if err := Render(&base, clean); err != nil {
			t.Fatalf("Render of the bracket-free input returned an error: %v", err)
		}
		for _, b := range []string{"<", ">"} {
			if a, c := strings.Count(got, b), strings.Count(base.String(), b); a != c {
				t.Fatalf("%d %q in the page, want %d — an input bracket reached the output unescaped", a, b, c)
			}
		}

		// Belt and braces on the two spellings that matter most.
		lower := strings.ToLower(got)
		if strings.Contains(lower, "<script") {
			t.Fatal("a <script tag reached the page")
		}
		if strings.Contains(lower, "javascript:") {
			t.Fatal("a javascript: URL reached the page")
		}

		// No arithmetic artifact reaches a reader.
		for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%s reached the page", bad)
			}
		}
	})
}

// fuzzInput threads the fuzzed values through every field the page renders.
func fuzzInput(version, cluster, clusterErr, kind, namespace, name, issue, text string,
	age, resolution int64, sum float64, count int64, target, burn, coverage float64) Input {
	return Input{
		Version:  version,
		Now:      fixedNow,
		Clusters: []Cluster{{Name: cluster, Up: false, LastScan: "2026-08-02T09:29:00Z", Error: clusterErr}},
		Active: []Incident{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name, Issue: issue,
			Firings: 2, Flapping: true, AgeSeconds: age,
		}},
		Resolved: []Incident{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name, Issue: issue,
			Firings:    1,
			ResolvedAt: "2026-08-02T08:30:00Z", ResolutionSeconds: resolution,
		}},
		Stats: Stats{ResolutionSecondsSum: sum, ResolutionSecondsCount: count},
		SLO: []SLO{{
			Cluster: cluster, Target: target,
			Windows: []SLOWindow{{Name: issue, Availability: coverage, BurnRate: burn, Coverage: coverage}},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: cluster, Kind: kind, Namespace: namespace, Name: name,
			Issues: []string{issue}, ExplainedAt: "2026-08-02T09:05:00Z", Model: version, Text: text,
		}},
	}
}

// stripBrackets removes the two characters html/template must never let
// through, so the comparison render carries only the template's own brackets.
func stripBrackets(s string) string {
	return strings.NewReplacer("<", "", ">", "").Replace(s)
}
