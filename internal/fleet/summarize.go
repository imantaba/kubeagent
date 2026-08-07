package fleet

import (
	"sort"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
)

// maxTopIssues caps the issue kinds a summary carries. Three, because the
// column has to stay readable at three hundred rows and because the
// fourth-most-common kind has never been what makes an operator open a cluster.
const maxTopIssues = 3

// identity is what the report calls a cluster: the operator's name when the
// selection source gave one, the kubeconfig context otherwise. It is the single
// definition — the sorts, the renderer and the evidence all go through it.
func identity(name, context string) string {
	if name != "" {
		return name
	}
	return context
}

// summarize reduces one cluster's gate verdict to its fleet row, and to the
// evidence that row cannot carry.
//
// It is pure, and it reads only Level and Issue off each finding and only
// Resource off each blind spot — which is what keeps a namespace, pod, workload
// or node name out of the report by construction, and keeps a blind spot's
// redacted Reason out of it too.
//
// The row's Blindspots count and the evidence's blindspots set answer different
// questions and are allowed to disagree: the count is how many reads failed,
// the set is which resources they were, and two failed reads of one resource
// are two of the former and one of the latter.
//
// It copies the name and context it is handed rather than resolving them. Sweep
// resolves the pair once, before the branch that chooses between a summary and
// an unreachable row, so the rule lives in one place instead of two.
func summarize(name, context string, v gate.Verdict) (ClusterSummary, clusterEvidence) {
	s := ClusterSummary{
		Name:       name,
		Context:    context,
		Verdict:    v.Verdict,
		Blindspots: len(v.Inconclusive),
	}
	ev := clusterEvidence{
		id:         identity(name, context),
		issues:     map[string]bool{},
		blindspots: map[string]bool{},
	}

	counts := map[string]int{}
	for _, f := range append(append([]findings.Finding{}, v.Failing...), v.Reported...) {
		switch f.Level {
		case findings.Critical:
			s.Critical++
		case findings.Warning:
			s.Warning++
		default:
			s.Info++
		}
		counts[f.Issue]++
		ev.issues[f.Issue] = true
	}
	for _, b := range v.Inconclusive {
		ev.blindspots[b.Resource] = true
	}

	s.TopIssues = topIssues(counts)
	return s, ev
}

// topIssues returns at most maxTopIssues kinds, most frequent first, ties by
// name ascending. Go randomizes map iteration order per run, so the tiebreak is
// not a nicety: without it the same cluster would render differently twice.
func topIssues(counts map[string]int) []string {
	if len(counts) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})
	if len(kinds) > maxTopIssues {
		kinds = kinds[:maxTopIssues]
	}
	return kinds
}

// rank orders verdicts worst first. Unreachable is rank 0 and is not a
// ClusterSummary verdict — the text renderer synthesises a row at that rank for
// each unreachable cluster, which is why the constant lives here rather than in
// the renderer.
func rank(verdict string) int {
	switch verdict {
	case "unreachable":
		return 0
	case "inconclusive":
		return 1
	case "fail":
		return 2
	default: // pass
		return 3
	}
}

// sortSummaries puts the worst cluster first, in place. The last tiebreak is
// the row identity, unique for a different reason on each of the two paths
// that reach here: on a kubeconfig sweep the identity is a context name,
// unique within the one kubeconfig it came from; on a fleet-file sweep it is
// the entry's own resolved name, which fleetfile.Load refuses to let
// collide. Either way the order is total, so two runs over the same fleet
// render identical bytes.
//
// It was the context name until the fleet file arrived, justified by the
// context being unique within a kubeconfig. That premise dies the moment a
// sweep spans several kubeconfigs: four per-cluster k3s kubeconfigs are four
// clusters whose context is "default", which makes the comparator non-total,
// and sort.Slice is not stable.
func sortSummaries(s []ClusterSummary) {
	sort.Slice(s, func(i, j int) bool {
		a, b := s[i], s[j]
		if ra, rb := rank(a.Verdict), rank(b.Verdict); ra != rb {
			return ra < rb
		}
		if a.Critical != b.Critical {
			return a.Critical > b.Critical
		}
		if a.Warning != b.Warning {
			return a.Warning > b.Warning
		}
		if a.Info != b.Info {
			return a.Info > b.Info
		}
		return identity(a.Name, a.Context) < identity(b.Name, b.Context)
	})
}

// decide derives the fleet verdict and its exit code.
//
// inconclusive outranks fail, carrying over the reasoning behind gate.Decide's
// own switch: when kubeagent could not see enough, a "fail" may understate what
// is actually wrong. Only the ordering of those two outcomes carries over, not
// gate.Decide's case list — it reaches "fail" by two routes, one of them (an
// in-scope policy rule that never ran) evaluated ahead of the blind case. Both
// routes have already collapsed to the string "fail" by the time a per-cluster
// verdict arrives here, so this function sees one fail outcome, not two.
func decide(clusters []ClusterSummary, unreachable []Unreachable) (string, int) {
	failing := false
	for _, c := range clusters {
		switch c.Verdict {
		case "inconclusive":
			return "inconclusive", gate.CodeInconclusive
		case "fail":
			failing = true
		}
	}
	if len(unreachable) > 0 {
		return "inconclusive", gate.CodeInconclusive
	}
	if failing {
		return "fail", gate.CodeFail
	}
	return "pass", gate.CodePass
}
