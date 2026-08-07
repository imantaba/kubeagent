package fleet

import (
	"reflect"
	"testing"
)

// ev builds one cluster's evidence from two literal lists. Sets, not counts —
// see TestCorrelateCountsAClusterOnceHoweverLoudItIs in summarize_test.go for
// why that distinction is load-bearing.
func ev(context string, issues, blindspots []string) clusterEvidence {
	e := clusterEvidence{id: context, issues: map[string]bool{}, blindspots: map[string]bool{}}
	for _, i := range issues {
		e.issues[i] = true
	}
	for _, b := range blindspots {
		e.blindspots[b] = true
	}
	return e
}

// A signal in one cluster is not a correlation, and neither is a sweep of one
// cluster however much that cluster reports.
func TestCorrelateNeedsTwoClusters(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []clusterEvidence
	}{
		{"no clusters at all", nil},
		{"one cluster, however much it reports", []clusterEvidence{
			ev("prod-eu", []string{"ImagePullBackOff", "OOMKilled"}, []string{"nodes/proxy"}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := correlate(tc.in); got != nil {
				t.Errorf("correlate() = %+v, want nil", got)
			}
		})
	}
}

// Three clusters, one signal in two of them and two signals in one each. Only
// the shared one survives.
func TestCorrelateKeepsOnlySignalsInTwoOrMoreClusters(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("prod-eu", []string{"ImagePullBackOff", "OOMKilled"}, nil),
		ev("prod-us", []string{"ImagePullBackOff"}, nil),
		ev("staging", []string{"Unschedulable"}, nil),
	})

	want := []Shared{
		{Signal: "ImagePullBackOff", Source: SourceIssue, Clusters: []string{"prod-eu", "prod-us"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Nothing shared at all is nil, not an empty slice, so Report's omitempty drops
// the key rather than encoding [].
func TestCorrelateWithNothingInCommonIsNil(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("prod-eu", []string{"ImagePullBackOff"}, []string{"nodes/proxy"}),
		ev("prod-us", []string{"OOMKilled"}, []string{"pods/log"}),
	})
	if got != nil {
		t.Errorf("correlate() = %+v, want nil so omitempty drops the key", got)
	}
}

// Most widespread first; equal counts break by signal name ascending. Go
// randomizes map iteration, so the tiebreak is not a nicety.
func TestCorrelateOrdersByClusterCountThenName(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"bbb", "zzz", "aaa"}, nil),
		ev("example-b", []string{"bbb", "zzz", "aaa"}, nil),
		ev("example-c", []string{"bbb"}, nil),
	})

	want := []Shared{
		{Signal: "bbb", Source: SourceIssue, Clusters: []string{"example-a", "example-b", "example-c"}},
		{Signal: "aaa", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "zzz", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Source rank beats cluster count: the two-cluster issue below precedes the
// three-cluster blind spot. A plain string compare would invert this, because
// "blindspot" sorts before "issue".
func TestCorrelatePutsEveryIssueBeforeEveryBlindSpot(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"zzz-issue"}, []string{"aaa-resource", "bbb-resource"}),
		ev("example-b", []string{"zzz-issue"}, []string{"aaa-resource", "bbb-resource"}),
		ev("example-c", nil, []string{"aaa-resource"}),
	})

	want := []Shared{
		{Signal: "zzz-issue", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "aaa-resource", Source: SourceBlindspot,
			Clusters: []string{"example-a", "example-b", "example-c"}},
		{Signal: "bbb-resource", Source: SourceBlindspot, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// An issue kind and an API resource name are different vocabularies that happen
// to share a namespace of strings. Folding them together would report one
// signal where there are two.
func TestCorrelateKeepsAnIssueAndABlindSpotOfTheSameNameApart(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("example-a", []string{"events"}, []string{"events"}),
		ev("example-b", []string{"events"}, []string{"events"}),
	})

	want := []Shared{
		{Signal: "events", Source: SourceIssue, Clusters: []string{"example-a", "example-b"}},
		{Signal: "events", Source: SourceBlindspot, Clusters: []string{"example-a", "example-b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("correlate() = %+v, want %+v", got, want)
	}
}

// Context names within an entry are ascending, so two runs over the same fleet
// render identical bytes.
func TestCorrelateNamesClustersAscending(t *testing.T) {
	got := correlate([]clusterEvidence{
		ev("zulu", []string{"OOMKilled"}, nil),
		ev("alpha", []string{"OOMKilled"}, nil),
		ev("mike", []string{"OOMKilled"}, nil),
	})

	want := []string{"alpha", "mike", "zulu"}
	if len(got) != 1 {
		t.Fatalf("correlate() = %+v, want one entry", got)
	}
	if !reflect.DeepEqual(got[0].Clusters, want) {
		t.Errorf("Clusters = %v, want %v", got[0].Clusters, want)
	}
}

// The fold walks two maps, and Go randomizes map iteration order per run. Every
// tiebreak in the sort exists to make this loop pass.
func TestCorrelateIsDeterministicAcrossRuns(t *testing.T) {
	in := []clusterEvidence{
		ev("example-a", []string{"ppp", "qqq", "rrr", "sss"}, []string{"ttt", "uuu"}),
		ev("example-b", []string{"ppp", "qqq", "rrr", "sss"}, []string{"ttt", "uuu"}),
	}

	first := correlate(in)
	if len(first) != 6 {
		t.Fatalf("correlate() = %+v, want six shared signals", first)
	}
	for i := 0; i < 50; i++ {
		if got := correlate(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n got %+v\nwant %+v", i, got, first)
		}
	}
}
