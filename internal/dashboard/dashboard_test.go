package dashboard

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// fixedNow is the clock every test that compares bytes uses, so no fixture
// holds a time-varying value.
var fixedNow = time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)

// render is the tests' entry point: it fails the test on a render error, which
// no test in this file expects, and returns the page as a string.
func render(t *testing.T, in Input) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, in); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String()
}

// payloadInput puts the same string into every field of the page that carries
// caller-supplied text. Task 3 extends it with the SLO and explanation fields.
func payloadInput(payload string) Input {
	return Input{
		Version: payload,
		Now:     fixedNow,
		Clusters: []Cluster{{
			Name:     payload,
			Up:       false,
			LastScan: "2026-08-02T09:29:00Z",
			Error:    payload,
		}},
		Active: []Incident{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issue: payload, FiringSince: "2026-08-02T09:00:00Z",
			Firings: 3, Flapping: true, AgeSeconds: 1800,
		}},
		Resolved: []Incident{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issue: payload, FiringSince: "2026-08-02T08:00:00Z",
			Firings: 1, ResolvedAt: "2026-08-02T08:30:00Z", ResolutionSeconds: 1800,
		}},
		Stats: Stats{
			NewTotal: 4, ResolvedTotal: 2, FlapTotal: 1, DroppedTotal: 0,
			ResolutionSecondsSum: 3600, ResolutionSecondsCount: 2,
		},
		SLO: []SLO{{
			Cluster: payload,
			Target:  0.999,
			Windows: []SLOWindow{{Name: payload, Availability: 0.9, BurnRate: 2, Coverage: 0.8}},
		}},
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: payload, Kind: payload, Namespace: payload, Name: payload,
			Issues:      []string{payload},
			ExplainedAt: "2026-08-02T09:20:00Z",
			Model:       payload,
			Text:        payload,
		}},
	}
}

// escapePayloads are the strings a hostile or merely unlucky cluster can put
// into a field the API server does not validate. Each must reach the page
// escaped and inert.
var escapePayloads = []struct{ name, payload string }{
	{"script tag", "<script>alert(1)</script>"},
	{"attribute break-out", `"><img src=x onerror=alert(1)>`},
	{"bare ampersand", "a & b"},
	{"single quote", "it's broken"},
	{"combining marks", "é́́"},
}

// TestRenderEscapesEveryStringField is the escaping table. It asserts the whole
// postcondition rather than a spelling: no executable markup survives anywhere
// in the page, and the payload's angle brackets arrive as entities.
func TestRenderEscapesEveryStringField(t *testing.T) {
	for _, tc := range escapePayloads {
		t.Run(tc.name, func(t *testing.T) {
			out := render(t, payloadInput(tc.payload))
			lower := strings.ToLower(out)
			if strings.Contains(lower, "<script") {
				t.Error("a <script tag reached the page")
			}
			// There is deliberately no assertion on the substring "onerror=".
			// Contextual escaping rewrites < > & " ' and nothing else, so in a
			// text node that substring survives verbatim inside
			// &#34;&gt;&lt;img src=x onerror=alert(1)&gt; — inert, because an
			// event handler runs only inside a tag, and the two assertions
			// around this comment are what prove no tag boundary was created.
			// Asserting its absence would fail correct code and could only be
			// satisfied by a second transformation on top of escaping, which
			// this package must not become.
			if strings.Contains(out, "<img") {
				t.Error("an <img element reached the page")
			}
			if strings.Contains(out, tc.payload) && strings.ContainsAny(tc.payload, "<>&") {
				t.Errorf("payload %q reached the page unescaped", tc.payload)
			}
		})
	}
}

// TestRenderEscapesAngleBracketsAsEntities pins the positive half: the payload
// is not merely absent, it is present in escaped form. A renderer that dropped
// the field entirely would pass the negative assertions above.
func TestRenderEscapesAngleBracketsAsEntities(t *testing.T) {
	out := render(t, payloadInput("<script>alert(1)</script>"))
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Error("the payload is neither present escaped nor rendered at all")
	}
}

// TestRenderEmptyInput is the starting state: a daemon that has just come up,
// with no cluster reporting and nothing tracked. It must render a page, not
// panic and not go dark.
func TestRenderEmptyInput(t *testing.T) {
	out := render(t, Input{Now: fixedNow})
	if !strings.Contains(out, "No active incidents") {
		t.Error("an empty page does not say there are no active incidents")
	}
	if strings.Contains(out, "NaN") {
		t.Error("an empty page renders NaN")
	}
}

// TestRenderUnscannedCluster covers the state before the first evaluation
// completes. An empty incident list from a cluster that has never been scanned
// must not read like a healthy one — that distinction is the whole reason the
// cluster strip exists.
func TestRenderUnscannedCluster(t *testing.T) {
	out := render(t, Input{
		Now:      fixedNow,
		Clusters: []Cluster{{Name: "example-cluster"}},
	})
	if !strings.Contains(out, "not scanned yet") {
		t.Error("a cluster with no completed evaluation does not say so")
	}
}

// TestRenderUnreachableCluster is the other half: reachable-and-quiet must not
// look like unreachable.
func TestRenderUnreachableCluster(t *testing.T) {
	out := render(t, Input{
		Now: fixedNow,
		Clusters: []Cluster{{
			Name:     "example-cluster",
			LastScan: "2026-08-02T09:29:00Z",
			Error:    "connection refused",
		}},
	})
	if !strings.Contains(out, "unreachable") {
		t.Error("a down cluster is not reported as unreachable")
	}
	if !strings.Contains(out, "connection refused") {
		t.Error("the cluster's error is not shown")
	}
}

// TestMeanTimeToResolutionWithNoResolutions asserts the tile shows an em dash
// rather than dividing by zero.
func TestMeanTimeToResolutionWithNoResolutions(t *testing.T) {
	out := render(t, Input{Now: fixedNow, Stats: Stats{ResolutionSecondsSum: 0, ResolutionSecondsCount: 0}})
	if strings.Contains(out, "NaN") || strings.Contains(out, "+Inf") {
		t.Error("mean time to resolution divided by zero")
	}
	if !strings.Contains(out, "—") {
		t.Error("mean time to resolution does not render an em dash when nothing has resolved")
	}
}

// TestClusterColumnOnlyWhenMulticluster keeps a single-cluster page from
// carrying a column that says the same thing on every row.
func TestClusterColumnOnlyWhenMulticluster(t *testing.T) {
	one := render(t, Input{
		Now:      fixedNow,
		Clusters: []Cluster{{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:29:00Z"}},
		Active:   []Incident{{Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff", AgeSeconds: 90}},
	})
	if strings.Count(one, "<th>Cluster</th>") != 0 {
		t.Error("a single-cluster page carries a Cluster column in an incident table")
	}
	two := render(t, Input{
		Now: fixedNow,
		Clusters: []Cluster{
			{Name: "example-a", Up: true, LastScan: "2026-08-02T09:29:00Z"},
			{Name: "example-b", Up: true, LastScan: "2026-08-02T09:29:00Z"},
		},
		Active: []Incident{{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "web", Issue: "ImagePullBackOff", AgeSeconds: 90}},
	})
	if !strings.Contains(two, "<th>Cluster</th>") {
		t.Error("a multicluster page omits the Cluster column")
	}
}

// TestHumanDuration pins the duration spelling the incident tables use.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		sec  int64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{90, "1m 30s"},
		{3600, "1h 0m"},
		{7845, "2h 10m"},
		{86400, "1d 0h"},
		{-5, "0s"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.sec); got != tc.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tc.sec, got, tc.want)
		}
	}
}

// sloInput is an SLO section with both windows populated and coverage below the
// suppression floor on the fast window.
func sloInput() Input {
	return Input{
		Now: fixedNow,
		SLO: []SLO{{
			Cluster: "example-cluster",
			Target:  0.999,
			Windows: []SLOWindow{
				{Name: "fast (1h)", Availability: 0.9, BurnRate: 100, Coverage: 0.4},
				{Name: "slow (6h)", Availability: 0.9995, BurnRate: 0.5, Coverage: 0.95},
			},
		}},
	}
}

// TestSLOSectionAbsentWhenNoSLO keeps the section out of a page for a daemon
// running without --slo-target, rather than rendering an empty table.
func TestSLOSectionAbsentWhenNoSLO(t *testing.T) {
	if out := render(t, Input{Now: fixedNow}); strings.Contains(out, "<h2>SLO") {
		t.Error("the SLO section renders with no SLO configured")
	}
}

// TestSLOSectionRenders covers the numbers and the coverage annotation. The
// suppression note matches what the kubeagent_slo_window_coverage_ratio help
// text already documents: below 0.6 the burn alert is suppressed.
func TestSLOSectionRenders(t *testing.T) {
	out := render(t, sloInput())
	for _, want := range []string{"<h2>SLO", "example-cluster", "99.90%", "fast (1h)", "slow (6h)", "burn alert suppressed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the SLO section does not contain %q", want)
		}
	}
	if strings.Contains(out, "NaN") {
		t.Error("the SLO section renders NaN")
	}
}

// TestSLOSectionSurvivesNonFiniteNumbers is the arithmetic boundary. A burn
// rate is a quotient, and a quotient by a target of exactly 1 is infinite.
func TestSLOSectionSurvivesNonFiniteNumbers(t *testing.T) {
	out := render(t, Input{
		Now: fixedNow,
		SLO: []SLO{{
			Cluster: "example-cluster",
			Target:  1,
			Windows: []SLOWindow{{Name: "fast (1h)", Availability: 0.5, BurnRate: math.Inf(1), Coverage: math.NaN()}},
		}},
	})
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Error("a non-finite SLO number reached the page")
	}
}

// TestExplanationsSectionAbsentWhenDisabled keeps the section off a page for a
// daemon running without --explain.
func TestExplanationsSectionAbsentWhenDisabled(t *testing.T) {
	if out := render(t, Input{Now: fixedNow}); strings.Contains(out, "<h2>Explanations") {
		t.Error("the explanations section renders with --explain off")
	}
}

// TestExplanationsSectionEmptyWhenEnabled asserts the section is present but
// empty when --explain is on and nothing has been explained yet. That is a
// distinguishable state an operator paying for the feature needs to see; a
// section that vanished would look identical to the feature being off.
func TestExplanationsSectionEmptyWhenEnabled(t *testing.T) {
	out := render(t, Input{Now: fixedNow, ExplainEnabled: true})
	if !strings.Contains(out, "<h2>Explanations") {
		t.Error("the explanations section is absent with --explain on")
	}
	if !strings.Contains(out, "No incident has been explained yet") {
		t.Error("an enabled but empty explanations section does not say so")
	}
}

// TestExplanationsRender covers the populated case.
func TestExplanationsRender(t *testing.T) {
	out := render(t, Input{
		Now:            fixedNow,
		ExplainEnabled: true,
		Explanations: []Explanation{{
			Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web",
			Issues:      []string{"ImagePullBackOff", "Degraded"},
			ExplainedAt: "2026-08-02T09:20:00Z", Model: "example-model",
			Text: "The image tag does not exist in the registry.",
		}},
	})
	for _, want := range []string{"example-ns/web", "ImagePullBackOff, Degraded", "example-model", "does not exist in the registry"} {
		if !strings.Contains(out, want) {
			t.Errorf("the explanations section does not contain %q", want)
		}
	}
}

// TestExplanationNamesItsClusterOnlyWhenMulticluster mirrors the rule the
// incident tables follow. Explanations are a flat list, not a block per
// cluster, so on a one-cluster page the name is on the header already and
// repeating it on every article is noise — but on a multi-cluster page an
// explanation that does not say which cluster it came from is unreadable.
func TestExplanationNamesItsClusterOnlyWhenMulticluster(t *testing.T) {
	explanations := []Explanation{{
		Cluster: "example-cluster", Kind: "Deployment", Namespace: "example-ns", Name: "web",
		ExplainedAt: "2026-08-02T09:20:00Z", Model: "example-model",
		Text: "The image tag does not exist in the registry.",
	}}
	one := render(t, Input{
		Now: fixedNow, ExplainEnabled: true, Explanations: explanations,
		Clusters: []Cluster{{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:00:00Z"}},
	})
	if strings.Contains(one, "example-cluster · Deployment") {
		t.Error("a single-cluster page repeats the cluster name on an explanation")
	}
	two := render(t, Input{
		Now: fixedNow, ExplainEnabled: true, Explanations: explanations,
		Clusters: []Cluster{
			{Name: "example-cluster", Up: true, LastScan: "2026-08-02T09:00:00Z"},
			{Name: "example-other", Up: true, LastScan: "2026-08-02T09:00:00Z"},
		},
	})
	if !strings.Contains(two, "example-cluster · Deployment") {
		t.Error("a multi-cluster page does not say which cluster an explanation came from")
	}
}

// TestRenderIsDeterministic asserts that the same Input rendered twice produces
// the same bytes. Map iteration order and an unstable sort are the two ways
// this fails, and both would show up as a page that reshuffles itself every
// thirty seconds.
func TestRenderIsDeterministic(t *testing.T) {
	in := goldenInput()
	first := render(t, in)
	for i := 0; i < 20; i++ {
		if got := render(t, in); got != first {
			t.Fatalf("render %d differs from the first render", i+2)
		}
	}
}

// TestRenderIgnoresInputOrder is what actually proves the order is total. If
// any pair of rows compared equal, some permutation would place them in the
// other order and the bytes would differ.
func TestRenderIgnoresInputOrder(t *testing.T) {
	in := goldenInput()
	want := render(t, in)
	for i := 0; i < len(in.Active); i++ {
		shuffled := goldenInput()
		// A rotation by i is a deterministic permutation — no random source, so
		// a failure reproduces exactly.
		shuffled.Active = append(append([]Incident(nil), shuffled.Active[i:]...), shuffled.Active[:i]...)
		shuffled.Resolved = append(append([]Incident(nil), shuffled.Resolved[i%len(shuffled.Resolved):]...), shuffled.Resolved[:i%len(shuffled.Resolved)]...)
		if got := render(t, shuffled); got != want {
			t.Errorf("rotating the input by %d changed the page — the sort order is not total", i)
		}
	}
}

// TestEqualDurationsStillOrderTotally is the case the age-first sort makes
// likely: two incidents firing for exactly the same number of seconds. They
// must fall through to the tiebreaker chain, not to whatever order they
// arrived in.
func TestEqualDurationsStillOrderTotally(t *testing.T) {
	a := Incident{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "alpha", Issue: "Degraded", AgeSeconds: 600}
	b := Incident{Cluster: "example-a", Kind: "Deployment", Namespace: "example-ns", Name: "beta", Issue: "Degraded", AgeSeconds: 600}
	one := render(t, Input{Now: fixedNow, Active: []Incident{a, b}})
	two := render(t, Input{Now: fixedNow, Active: []Incident{b, a}})
	if one != two {
		t.Error("two incidents with equal firing durations render in input order")
	}
	if strings.Index(one, "alpha") > strings.Index(one, "beta") {
		t.Error("the tiebreaker chain did not order equal-duration rows by name")
	}
}
