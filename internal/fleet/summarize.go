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

// summarize reduces one cluster's gate verdict to its fleet row. It is pure,
// and it reads only Level and Issue off each finding — which is what keeps a
// namespace, pod, workload or node name out of the report by construction.
func summarize(context string, v gate.Verdict) ClusterSummary {
	s := ClusterSummary{
		Context:    context,
		Verdict:    v.Verdict,
		Blindspots: len(v.Inconclusive),
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
	}
	s.TopIssues = topIssues(counts)
	return s
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
// the context name, which is unique within a kubeconfig — so the order is
// total and two runs over the same fleet render identical bytes.
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
		return a.Context < b.Context
	})
}

// decide derives the fleet verdict and its exit code.
//
// inconclusive outranks fail, mirroring gate.Decide's own switch at
// internal/gate/gate.go:229-240, where the blind case is evaluated before the
// failing case. The reasoning carries over exactly: when kubeagent could not
// see enough, a "fail" may understate what is actually wrong.
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
