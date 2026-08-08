package fleet

import "sort"

// minShared is how many clusters a signal must appear in before it is a
// correlation. Two, and not configurable: one cluster is not a pattern, and
// every number above two is an arbitrary line an operator would have to learn.
const minShared = 2

// Shared is one signal that appeared in minShared or more judged clusters.
//
// There is no count field on purpose: len(Clusters) is the count, and a stored
// duplicate is a defect waiting to disagree with the array beside it.
//
// Source is not decoration. The two vocabularies are different kinds of fact —
// an issue kind is something wrong inside the cluster, an API resource name is
// something kubeagent was not allowed to look at — and a consumer reading the
// JSON has no other way to tell them apart. The text renderer carries the same
// distinction as two labelled sections.
type Shared struct {
	Signal   string   `json:"signal"`
	Source   string   `json:"source"`
	Clusters []string `json:"clusters"`
}

// The fixed Shared.Source vocabulary.
const (
	SourceIssue     = "issue"
	SourceBlindspot = "blindspot"
)

// clusterEvidence is what one judged cluster contributes to a correlation:
// which signals it showed, not how many times it showed them.
//
// Sets, not counts, and that is load-bearing. A kind hitting four hundred pods
// in one cluster is still one cluster. A count-weighted fold would let a single
// noisy cluster manufacture a fleet-wide signal that does not exist.
type clusterEvidence struct {
	// id is the row identity — the operator's name when the selection source
	// gave one, the kubeconfig context otherwise. The shared-signal section
	// must name what the table names, or an operator cannot cross-reference
	// the two.
	id         string
	issues     map[string]bool
	blindspots map[string]bool
}

// correlate folds per-cluster evidence into the signals that appeared in
// minShared or more clusters, most widespread first.
//
// It is pure, and what it carries is bounded by construction rather than by a
// filter: a Shared holds a row identity, an issue kind or an API resource name,
// and nothing else. In particular nothing on a gate.Blindspot reaches here
// except Resource — never Reason, which is a redacted error string rather than
// a bounded vocabulary, and this document is written to be forwarded.
func correlate(evidence []clusterEvidence) []Shared {
	if len(evidence) < minShared {
		return nil
	}

	issues, blindspots := map[string][]string{}, map[string][]string{}
	for _, e := range evidence {
		for signal := range e.issues {
			issues[signal] = append(issues[signal], e.id)
		}
		for signal := range e.blindspots {
			blindspots[signal] = append(blindspots[signal], e.id)
		}
	}

	shared := append(gather(issues, SourceIssue), gather(blindspots, SourceBlindspot)...)
	if len(shared) == 0 {
		return nil // nil rather than an empty slice, so Report's omitempty drops the key
	}

	sort.Slice(shared, func(i, j int) bool {
		a, b := shared[i], shared[j]
		if ra, rb := sourceRank(a.Source), sourceRank(b.Source); ra != rb {
			return ra < rb
		}
		if len(a.Clusters) != len(b.Clusters) {
			return len(a.Clusters) > len(b.Clusters)
		}
		return a.Signal < b.Signal
	})
	return shared
}

// gather turns one signal-to-clusters map into the entries that clear
// minShared. The identities are sorted here rather than at the call site
// because Go randomizes map iteration: without it the same fleet would render
// two different orders on two runs.
func gather(m map[string][]string, source string) []Shared {
	var out []Shared
	for signal, ids := range m {
		if len(ids) < minShared {
			continue
		}
		sort.Strings(ids)
		out = append(out, Shared{Signal: signal, Source: source, Clusters: ids})
	}
	return out
}

// sourceRank puts every issue before every blind spot. Comparing the strings
// would not: "blindspot" sorts before "issue".
func sourceRank(source string) int {
	if source == SourceIssue {
		return 0
	}
	return 1
}
