package baseline

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCaptureComputesPodHourNormalisedRate(t *testing.T) {
	// Two pods, 7200s each = 4 pod-hours; 6 restarts total = 1.5/hour.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 4, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
	}, time.Hour, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", doc.SchemaVersion, SchemaVersion)
	}
	if doc.CapturedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("CapturedAt = %q, want an RFC3339 UTC instant", doc.CapturedAt)
	}
	if doc.MinPodAgeSeconds != 3600 {
		t.Errorf("MinPodAgeSeconds = %v, want 3600", doc.MinPodAgeSeconds)
	}
	if len(doc.Workloads) != 1 {
		t.Fatalf("Workloads = %d entries, want 1", len(doc.Workloads))
	}
	e := doc.Workloads[0]
	if e.RestartsPerHour != 1.5 {
		t.Errorf("RestartsPerHour = %v, want 1.5", e.RestartsPerHour)
	}
	if e.Pods != 2 || e.ObservedSeconds != 14400 {
		t.Errorf("Pods/ObservedSeconds = %d/%v, want 2/14400", e.Pods, e.ObservedSeconds)
	}
}

func TestCaptureExcludesAYoungPodFromBothSides(t *testing.T) {
	// The young pod's 5 restarts and its 60 seconds must BOTH vanish. If only
	// the numerator were filtered the rate would drop; if only the denominator
	// were, it would spike. Either bug leaves 1.0 here.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 5, AgeSeconds: 60},
	}, time.Hour, time.Time{})

	if len(doc.Workloads) != 1 {
		t.Fatalf("Workloads = %d entries, want 1", len(doc.Workloads))
	}
	e := doc.Workloads[0]
	if e.RestartsPerHour != 1 || e.Pods != 1 || e.ObservedSeconds != 7200 {
		t.Errorf("got %.4f/hour over %d pods and %vs, want 1/hour over 1 pod and 7200s",
			e.RestartsPerHour, e.Pods, e.ObservedSeconds)
	}
}

func TestCaptureOmitsAWorkloadWithNoCountedPods(t *testing.T) {
	// Unknown is not zero: an entry at 0/hour would later read as "this
	// workload never restarts", which the sample cannot support.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 60},
	}, time.Hour, time.Time{})
	if len(doc.Workloads) != 0 {
		t.Errorf("Workloads = %+v, want no entry for a workload with no counted pods", doc.Workloads)
	}
}

func TestCaptureSortsByKindNamespaceName(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "StatefulSet", Namespace: "prod", Name: "cache", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "web", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "kube-system", Name: "api", AgeSeconds: 7200},
	}, time.Hour, time.Time{})

	var got []string
	for _, e := range doc.Workloads {
		got = append(got, e.Kind+"/"+e.Namespace+"/"+e.Name)
	}
	want := []string{
		"Deployment/kube-system/api",
		"Deployment/prod/api",
		"Deployment/prod/web",
		"StatefulSet/prod/cache",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestCaptureRefusesAnUnusableAge(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: math.NaN()},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: math.Inf(1)},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: -5},
	}, 0, time.Time{})
	if len(doc.Workloads) != 0 {
		t.Errorf("Workloads = %+v, want no entry — every sample had an unusable age", doc.Workloads)
	}
}

// baseDoc is a one-workload document at the given rate, captured with a
// one-hour floor.
func baseDoc(rate float64) Document {
	return Document{
		SchemaVersion: SchemaVersion, MinPodAgeSeconds: 3600,
		Workloads: []Entry{{Kind: "Deployment", Namespace: "prod", Name: "api",
			RestartsPerHour: rate, Pods: 1, ObservedSeconds: 7200}},
	}
}

// atRate is one 7200-second pod carrying whatever restart count produces rate.
func atRate(rate float64) []PodSample {
	return []PodSample{{Kind: "Deployment", Namespace: "prod", Name: "api",
		Restarts: int(rate * 2), AgeSeconds: 7200}}
}

func TestCompareNeedsBothThresholds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		base, cur float64
		wantFlag  bool
	}{
		// 4x and +3.0/hour: both hold.
		{"clearly worse", 1, 4, true},
		// 20x but only +0.19/hour: the floor suppresses it.
		{"big multiple, tiny absolute change", 0.01, 0.2, false},
		// +2.0/hour but only 2x: the factor suppresses it.
		{"big absolute change, small multiple", 2, 4, false},
		// Baseline zero: the multiplicative test is trivially true, so the
		// floor is the only thing deciding — and 1.0 clears it.
		{"zero baseline above the floor", 0, 1, true},
		// Baseline zero and current below the floor: not reported.
		{"zero baseline below the floor", 0, 0.4, false},
		// Nobody is paged for a thing improving.
		{"improvement", 4, 1, false},
		{"unchanged", 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Compare(baseDoc(tc.base), atRate(tc.cur), CompareOptions{})
			if got := len(rep.Deviations) == 1; got != tc.wantFlag {
				t.Errorf("%.3f -> %.3f flagged = %v, want %v (deviations: %+v)",
					tc.base, tc.cur, got, tc.wantFlag, rep.Deviations)
			}
			if rep.Compared != 1 {
				t.Errorf("Compared = %d, want 1", rep.Compared)
			}
		})
	}
}

func TestCompareHonoursExplicitOptions(t *testing.T) {
	// 2x with a +1.0/hour rise: default Factor 3.0 refuses it, Factor 2.0 takes it.
	if rep := Compare(baseDoc(1), atRate(2), CompareOptions{}); len(rep.Deviations) != 0 {
		t.Errorf("default options flagged 1 -> 2, want no deviation")
	}
	rep := Compare(baseDoc(1), atRate(2), CompareOptions{Factor: 2, Floor: 0.5})
	if len(rep.Deviations) != 1 {
		t.Fatalf("Factor 2 did not flag 1 -> 2: %+v", rep.Deviations)
	}
	d := rep.Deviations[0]
	if d.BaselineRate != 1 || d.CurrentRate != 2 || d.Pods != 1 {
		t.Errorf("deviation = %+v, want baseline 1, current 2, 1 pod", d)
	}
}

func TestCompareCountsNewAndGoneWorkloadsWithoutFlaggingThem(t *testing.T) {
	doc := baseDoc(1)
	doc.Workloads = append(doc.Workloads, Entry{
		Kind: "Deployment", Namespace: "prod", Name: "gone", RestartsPerHour: 1, Pods: 1, ObservedSeconds: 7200})

	rep := Compare(doc, []PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "brand-new", Restarts: 40, AgeSeconds: 7200},
	}, CompareOptions{})

	if rep.Compared != 1 || rep.NotInBaseline != 1 || rep.GoneFromCluster != 1 {
		t.Errorf("compared/new/gone = %d/%d/%d, want 1/1/1", rep.Compared, rep.NotInBaseline, rep.GoneFromCluster)
	}
	if len(rep.Deviations) != 0 {
		t.Errorf("deviations = %+v, want none — a workload absent from the baseline is never flagged", rep.Deviations)
	}
}

func TestCompareAppliesTheCapturedFloorNotTheCallers(t *testing.T) {
	// The document says one hour. A 60-second pod carrying 40 restarts must be
	// excluded on the compare side too, or the asymmetry alone produces an alarm.
	rep := Compare(baseDoc(1), []PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 40, AgeSeconds: 60},
	}, CompareOptions{})
	if len(rep.Deviations) != 0 {
		t.Errorf("deviations = %+v, want none — the young pod must not count", rep.Deviations)
	}
}

func TestCompareAlwaysReturnsANonNilDeviationSlice(t *testing.T) {
	rep := Compare(Document{SchemaVersion: SchemaVersion}, nil, CompareOptions{})
	if rep.Deviations == nil {
		t.Error("Deviations is nil; a run that found nothing must encode \"deviations\": []")
	}
}

func TestMarshalLoadRoundTrip(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 7200},
	}, time.Hour, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if b[len(b)-1] != '\n' {
		t.Error("Marshal output does not end in a newline")
	}
	got, err := Load(b)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CapturedAt != doc.CapturedAt || got.MinPodAgeSeconds != doc.MinPodAgeSeconds {
		t.Errorf("round trip changed the header: %+v vs %+v", got, doc)
	}
	if len(got.Workloads) != 1 || got.Workloads[0] != doc.Workloads[0] {
		t.Errorf("round trip changed the workloads: %+v vs %+v", got.Workloads, doc.Workloads)
	}
}

func TestLoadRejectsADifferentMajor(t *testing.T) {
	_, err := Load([]byte(`{"schemaVersion":"2.0","workloads":[]}`))
	if err == nil {
		t.Fatal("Load accepted a different MAJOR version")
	}
	if !strings.Contains(err.Error(), "2.0") {
		t.Errorf("error %q does not name the version it refused", err)
	}
}

func TestLoadAcceptsAHigherMinor(t *testing.T) {
	// additionalProperties is unset on purpose: a document written by a later
	// MINOR must still load here, unknown keys and all.
	doc, err := Load([]byte(`{"schemaVersion":"1.9","workloads":[],"somethingNew":true}`))
	if err != nil {
		t.Fatalf("Load rejected a higher MINOR: %v", err)
	}
	if doc.Workloads == nil {
		t.Error("Load left Workloads nil")
	}
}

func TestLoadRejectsAMissingOrMalformedVersion(t *testing.T) {
	for _, src := range []string{
		`{"workloads":[]}`,
		`{"schemaVersion":"","workloads":[]}`,
		`{"schemaVersion":"1","workloads":[]}`,
		`{"schemaVersion":"x.y","workloads":[]}`,
		`{"schemaVersion":"1.","workloads":[]}`,
		`not json`,
	} {
		if _, err := Load([]byte(src)); err == nil {
			t.Errorf("Load accepted %q", src)
		}
	}
}

func TestLoadRejectsAnUnusableEntry(t *testing.T) {
	for _, src := range []string{
		`{"schemaVersion":"1.0","workloads":[{"namespace":"prod","name":"api"}]}`,
		`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","namespace":"prod"}]}`,
		`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","name":"api","restartsPerHour":-1}]}`,
		`{"schemaVersion":"1.0","minPodAgeSeconds":-1,"workloads":[]}`,
	} {
		if _, err := Load([]byte(src)); err == nil {
			t.Errorf("Load accepted %q", src)
		}
	}
}
