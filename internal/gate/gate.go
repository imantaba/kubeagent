// Package gate turns a scan.Result into a CI verdict: which findings count,
// which blind spots make the run untrustworthy, and what process exit status a
// pipeline should see.
//
// Pure: it performs no cluster calls and no LLM calls. Everything it judges is
// already in the scan.Result it is handed.
package gate

import (
	"fmt"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Exit codes. These are a published contract: a pipeline branches on them, so
// their meanings are fixed and may never be reassigned.
const (
	CodePass         = 0 // nothing at or above --fail-on
	CodeFail         = 1 // findings at or above --fail-on
	CodeInconclusive = 2 // kubeagent could not see enough to judge
	CodeTimeout      = 3 // --wait-for did not settle within --timeout
	CodeUsage        = 4 // bad flags or arguments
)

// Options is what the CLI decided before the scan ran.
type Options struct {
	FailOn findings.Level

	// ScopeKind/ScopeName/ScopeNamespace narrow which findings count, set from
	// --wait-for. All empty means the whole cluster counts.
	ScopeKind      string
	ScopeName      string
	ScopeNamespace string

	// AllowPartialRead names resources whose read failure the operator has
	// explicitly accepted (--allow-partial-read), by scan.ReadFailure.Resource.
	AllowPartialRead []string

	// TimedOut reports that --wait-for never settled. TimeoutDetail is the last
	// observed rollout state, for the operator to read.
	TimedOut      bool
	TimeoutDetail string
}

// Blindspot is one failed collector call plus whether --allow-partial-read
// waived it. Waived entries stay in the verdict: an operator should still see
// what they chose not to be told about.
type Blindspot struct {
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
	Waived   bool   `json:"waived"`
}

// Verdict is the gate's decision, and is the --output json document verbatim.
// Verdict and Code are derived together and can never disagree: a shell reads
// the code, a jq filter reads the string, and neither should have to derive the
// other.
type Verdict struct {
	Verdict      string             `json:"verdict"`
	Code         int                `json:"exitCode"`
	FailOn       findings.Level     `json:"failOn"`
	Scope        string             `json:"scope"`
	Failing      []findings.Finding `json:"failing"`
	Reported     []findings.Finding `json:"reported"`
	Inconclusive []Blindspot        `json:"inconclusive"`
}

// inScope reports whether a finding is attributable to the gate's --wait-for
// workload. An unscoped gate judges everything.
func (o Options) inScope(f findings.Finding) bool {
	if o.ScopeKind == "" {
		return true
	}
	if f.Namespace != o.ScopeNamespace {
		return false
	}
	// The finding is the workload itself (a flagged Deployment), or hangs off it
	// (a pod it owns, whose Owner findings.Flatten recorded).
	if f.Kind == o.ScopeKind && f.Name == o.ScopeName {
		return true
	}
	return f.Owner == o.ScopeKind+"/"+o.ScopeName
}

// Decide judges res under opts.
//
// Precedence is timeout, then inconclusive, then fail, then pass. Timeout wins
// because a rollout that never settled makes every other judgement premature,
// and inconclusive beats fail because a run that could not see the cluster has
// not earned the right to report a confident failure — or a confident pass,
// which is the green-when-blind case this whole command exists to prevent.
func Decide(res scan.Result, opts Options) Verdict {
	v := Verdict{
		FailOn:       opts.FailOn,
		Scope:        "cluster",
		Failing:      []findings.Finding{},
		Reported:     []findings.Finding{},
		Inconclusive: []Blindspot{},
	}
	if opts.ScopeKind != "" {
		v.Scope = fmt.Sprintf("%s/%s in %s", opts.ScopeKind, opts.ScopeName, opts.ScopeNamespace)
	}

	waived := make(map[string]bool, len(opts.AllowPartialRead))
	for _, r := range opts.AllowPartialRead {
		waived[r] = true
	}
	blind := false
	for _, pr := range res.PartialReads {
		b := Blindspot{Resource: pr.Resource, Reason: pr.Reason, Waived: waived[pr.Resource]}
		if !b.Waived {
			blind = true
		}
		v.Inconclusive = append(v.Inconclusive, b)
	}

	for _, f := range findings.Flatten(res) {
		if opts.inScope(f) && f.Level >= opts.FailOn {
			v.Failing = append(v.Failing, f)
			continue
		}
		v.Reported = append(v.Reported, f)
	}
	findings.Sort(v.Failing)
	findings.Sort(v.Reported)

	switch {
	case opts.TimedOut:
		v.Verdict, v.Code = "timeout", CodeTimeout
	case blind:
		v.Verdict, v.Code = "inconclusive", CodeInconclusive
	case len(v.Failing) > 0:
		v.Verdict, v.Code = "fail", CodeFail
	default:
		v.Verdict, v.Code = "pass", CodePass
	}
	return v
}
