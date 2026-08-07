// Package fleet sweeps many clusters in one read-only pass and reduces each to
// a one-line verdict.
//
// The per-cluster pipeline is exactly the one `kubeagent gate` runs —
// scan.Evaluate then the pure gate.Decide — so a fleet sweep and a
// single-cluster gate can never disagree about the same cluster. fleet adds no
// diagnosis of its own.
//
// Two separate promises, and they are not restatements of each other. First,
// fleet is read-only toward every cluster it touches: get and list only, no
// write of any kind, and no --fix path. Second and additionally, fleet makes no
// LLM call. The package accordingly imports neither internal/remediate nor
// internal/explain, which internal/fleet/imports_test.go enforces.
//
// The report names kubeconfig context names, issue kinds, and the API resource
// names of refused reads. It never names a node, namespace, pod or workload,
// and that is structural rather than filtered: a summary is counts plus issue
// kinds, and a correlation is context names plus a signal drawn from one of two
// closed vocabularies — shapes an object name cannot fit into. In particular a
// correlation reads gate.Blindspot.Resource and never gate.Blindspot.Reason,
// which is a redacted error string rather than a bounded vocabulary. Nor does
// the report ever carry a kubeconfig path — the one accepted place a path may
// appear is stderr, from internal/cli, and this package writes no errors of its
// own.
package fleet

import (
	"context"
	"errors"
	"sort"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/parallel"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Target is one cluster to sweep. The caller builds the client, because
// building it needs a kubeconfig path and a kubeconfig path is a credential
// this package must never hold.
type Target struct {
	// Name is the row identity: what the report calls this cluster. For a
	// kubeconfig sweep it is the context name.
	Name string

	// Context is the kubeconfig context this cluster was reached through.
	// Empty means it is the same as Name — a caller that did not distinguish
	// the two has only one identity to report.
	Context string

	Client kubernetes.Interface
}

// Options configures a sweep.
type Options struct {
	// FailOn is the level at or above which a finding fails a cluster.
	FailOn findings.Level

	// Scan is the per-cluster read, handed in whole rather than rebuilt here so
	// that fleet judges exactly the check set `kubeagent gate` judges. Sharing
	// the constructor makes that a structural fact rather than a copied list
	// two commands must be kept in step by hand.
	Scan scan.Options

	// Workers bounds how many clusters are read at once. Sweep clamps it to
	// 1..64, so a zero value is 1 rather than an error.
	Workers int

	// ClusterTimeout is the per-cluster budget. A cluster that overruns it is
	// unreachable with ReasonTimedOut, and the other clusters keep going.
	//
	// A non-positive value attaches no deadline at all, and Sweep does not
	// second-guess that: imposing a budget the caller did not ask for would be
	// the worse surprise. It does mean one API server that accepts a connection
	// and then never answers blocks the whole sweep, because the worker pool
	// returns only once every worker has — so internal/cli refuses a
	// non-positive --cluster-timeout rather than passing it through.
	ClusterTimeout time.Duration
}

// Report is `kubeagent fleet --output json` verbatim.
type Report struct {
	SchemaVersion string           `json:"schemaVersion"`
	Verdict       string           `json:"verdict"`
	Code          int              `json:"exitCode"`
	FailOn        findings.Level   `json:"failOn"`
	Clusters      []ClusterSummary `json:"clusters"`
	Unreachable   []Unreachable    `json:"unreachable"`

	// Shared is the cross-cluster correlation: which signals appeared in two or
	// more judged clusters. Absent from the document when there is none, so a
	// consumer written against the version before it is unaffected.
	Shared []Shared `json:"shared,omitempty"`
}

// ClusterSummary is one cluster's outcome: counts and issue kinds, never object
// names.
type ClusterSummary struct {
	// Name is the row identity when the selection source gave one that differs
	// from the context. Absent otherwise. Context always holds a real
	// kubeconfig context name, as it has since v1.7.0, so a consumer piping it
	// into `kubectl --context` keeps working.
	Name string `json:"name,omitempty"`

	Context    string `json:"context"`
	Verdict    string `json:"verdict"`
	Critical   int    `json:"critical"`
	Warning    int    `json:"warning"`
	Info       int    `json:"info"`
	Blindspots int    `json:"blindspots"`

	// TopIssues is at most three issue kinds, most frequent first, ties broken
	// by kind name ascending so the slice is deterministic. It is a signpost,
	// not an inventory: the operator runs `scan` against that one context for
	// the full list.
	TopIssues []string `json:"topIssues,omitempty"`
}

// Unreachable is a cluster that was selected and could not be judged.
//
// Unreachable is not the same as refused. A cluster kubeagent reached but was
// not allowed to read fully gets an ordinary ClusterSummary with a non-zero
// Blindspots count and an inconclusive verdict, because scan.Evaluate returned
// and gate.Decide recorded the refusal. Unreachable is only for a cluster that
// produced no scan.Result at all.
type Unreachable struct {
	// Name is the row identity when the selection source gave one that differs
	// from the context. Absent otherwise, so a sweep selected from a kubeconfig
	// encodes no name key and its document stays byte-identical to v1.10.0's.
	//
	// An unreachable per-cluster-kubeconfig entry would otherwise render as
	// "default", which names nothing.
	Name string `json:"name,omitempty"`

	Context string `json:"context"`

	// Reason is drawn from the fixed vocabulary below and is never an
	// err.Error(), which can carry a server URL or a filesystem path. The
	// underlying error is dropped rather than routed somewhere safer: this
	// document is written to be forwarded, and there is no stream on which
	// fleet could publish a per-cluster error without also publishing it to
	// whoever receives the report. An operator who needs the detail runs
	// `kubeagent gate --context <name>` against the one cluster.
	Reason string `json:"reason"`
}

// The fixed Unreachable.Reason vocabulary.
const (
	ReasonUnreachable = "connecting to the cluster"
	ReasonTimedOut    = "timed out"
)

// maxWorkers bounds the pool. Three hundred clusters at once would be three
// hundred TLS handshakes and three hundred concurrent API server conversations
// from one process — the cap is what makes "fleet-scale" a bounded read rather
// than a thundering herd the operator finds out about from their control plane.
const maxWorkers = 64

// Sweep reads every target and returns the fleet report.
//
// Determinism is preserved by construction, not by discipline: parallel.Do
// returns results in index order rather than completion order, each closure
// writes only its own result, and the sequential pass afterwards sorts by a
// total order. The rendered bytes are never a function of which cluster
// answered first.
//
// A target whose read fails is named in Unreachable rather than dropped. At
// fleet size that matters more than at one: one missing row out of three
// hundred is invisible, so the count reaches the header line and the verdict
// too.
func Sweep(ctx context.Context, targets []Target, opts Options) Report {
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > maxWorkers {
		workers = maxWorkers
	}

	type outcome struct {
		verdict gate.Verdict
		err     error
	}

	results := parallel.Do(ctx, workers, len(targets), func(ctx context.Context, i int) outcome {
		t := targets[i]
		if t.Client == nil {
			return outcome{err: errClientUnavailable}
		}
		if opts.ClusterTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, opts.ClusterTimeout)
			defer cancel()
		}
		res, err := scan.Evaluate(ctx, t.Client, opts.Scan)
		if err != nil {
			return outcome{err: err}
		}
		return outcome{verdict: gate.Decide(res, gate.Options{FailOn: opts.FailOn})}
	})

	rep := Report{
		SchemaVersion: jsonschema.FleetVersion,
		FailOn:        opts.FailOn,
		Clusters:      []ClusterSummary{},
		Unreachable:   []Unreachable{},
	}
	evidence := make([]clusterEvidence, 0, len(targets))
	for i, r := range results {
		// Resolve the pair once, here, rather than in each branch below: a
		// caller that gave only a Name has one identity to report, and a name
		// equal to its context says nothing the context does not — so it is
		// blanked and omitempty drops the key.
		name, ctx := targets[i].Name, targets[i].Context
		if ctx == "" {
			ctx = name
		}
		if name == ctx {
			name = ""
		}

		if r.err != nil {
			rep.Unreachable = append(rep.Unreachable, Unreachable{
				Name:    name,
				Context: ctx,
				Reason:  reasonFor(r.err),
			})
			continue
		}
		summary, ev := summarize(name, ctx, r.verdict)
		rep.Clusters = append(rep.Clusters, summary)
		evidence = append(evidence, ev)
	}

	// Only judged clusters contribute. An unreachable cluster produced no
	// verdict and so no evidence — it is absent from this slice rather than
	// present and empty, which is also what makes the rendered denominator the
	// count of clusters kubeagent actually judged.
	rep.Shared = correlate(evidence)

	sortSummaries(rep.Clusters)
	sort.Slice(rep.Unreachable, func(i, j int) bool {
		a, b := rep.Unreachable[i], rep.Unreachable[j]
		return identity(a.Name, a.Context) < identity(b.Name, b.Context)
	})
	rep.Verdict, rep.Code = decide(rep.Clusters, rep.Unreachable)
	return rep
}

// errClientUnavailable stands in for a target internal/cli could not build a
// client for. It never reaches the report — reasonFor maps it to the fixed
// vocabulary — and it carries no path of its own.
var errClientUnavailable = errors.New("no client")

// reasonFor maps a read failure to the fixed Unreachable.Reason vocabulary.
// Deliberately not err.Error(): a client-go error routinely carries the API
// server URL, and a wrapped one can carry a kubeconfig path. Either would put a
// credential into a document written to be forwarded. The error is dropped
// here, not logged somewhere safer — this package writes to no stream at all,
// and any stream it did write to would reach the report's readers too.
func reasonFor(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ReasonTimedOut
	}
	return ReasonUnreachable
}
