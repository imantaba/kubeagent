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
// The report names kubeconfig context names and issue kinds. It never names a
// node, namespace, pod or workload, and that is structural rather than
// filtered: a summary is counts plus issue kinds, a shape an object name cannot
// fit into. Nor does it ever carry a kubeconfig path — the one accepted place a
// path may appear is stderr, from internal/cli, and this package writes no
// errors of its own.
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
	Name   string
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
}

// ClusterSummary is one cluster's outcome: counts and issue kinds, never object
// names.
type ClusterSummary struct {
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
	Context string `json:"context"`

	// Reason is drawn from the fixed vocabulary below and is never an
	// err.Error(), which can carry a server URL or a filesystem path. The
	// underlying error still reaches the operator on stderr, from internal/cli.
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
		if err == nil {
			// scan.Evaluate reads through whatever client the caller handed in.
			// A real clientset's HTTP round trip already fails once ctx is done,
			// but nothing here guarantees the client underneath ever looks at
			// ctx — so the budget is enforced here too, rather than only trusted
			// to the client. Checked after the call, not before: a read that
			// finished within budget must not be discarded because the budget
			// has since ticked over.
			err = ctx.Err()
		}
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
	for i, r := range results {
		if r.err != nil {
			rep.Unreachable = append(rep.Unreachable, Unreachable{
				Context: targets[i].Name,
				Reason:  reasonFor(r.err),
			})
			continue
		}
		rep.Clusters = append(rep.Clusters, summarize(targets[i].Name, r.verdict))
	}

	sortSummaries(rep.Clusters)
	sort.Slice(rep.Unreachable, func(i, j int) bool {
		return rep.Unreachable[i].Context < rep.Unreachable[j].Context
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
// credential into a document written to be forwarded. The operator still gets
// the underlying error, on stderr, from internal/cli.
func reasonFor(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ReasonTimedOut
	}
	return ReasonUnreachable
}
