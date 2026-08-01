// Package gate turns a scan.Result into a CI verdict: which findings count,
// which blind spots make the run untrustworthy, and what process exit status a
// pipeline should see.
//
// Pure: it performs no cluster calls and no LLM calls. Everything it judges is
// already in the scan.Result it is handed.
package gate

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/policy"
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
	// observed rollout state, which Decide copies into Verdict.Detail so the
	// operator can read it: "the rollout did not settle" is not actionable on
	// its own, but "1/3 replicas updated, 2 unavailable" is.
	TimedOut      bool
	TimeoutDetail string

	// PolicyViolations and PolicyNotEvaluated are the outcome of the --policy
	// run, both empty when no policy file was given. They join the flattened
	// findings, so --fail-on and --wait-for scoping apply to them unchanged.
	PolicyViolations   []policy.Violation
	PolicyNotEvaluated []policy.Unevaluated
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
	SchemaVersion string         `json:"schemaVersion"`
	Verdict       string         `json:"verdict"`
	Code          int            `json:"exitCode"`
	FailOn        findings.Level `json:"failOn"`
	Scope         string         `json:"scope"`

	// Detail is the last rollout state --wait-for observed, present whenever a
	// wait ran and absent otherwise — a pass carries "3/3 replicas ready" as
	// readily as a timeout carries "1/3 replicas updated, 2 unavailable". Only
	// the timeout case renders it as text, because only there does the operator
	// need it to decide between raising --timeout and going to look at the pods.
	Detail string `json:"detail,omitempty"`

	Failing      []findings.Finding `json:"failing"`
	Reported     []findings.Finding `json:"reported"`
	Inconclusive []Blindspot        `json:"inconclusive"`

	// PolicyNotEvaluated lists the rules kubeagent could not run. Violations
	// need no field of their own — they are findings, and they are already in
	// Failing or Reported. A rule that never ran is different in kind: it is the
	// one outcome where the verdict understates what the operator asked for, and
	// a consumer must be able to see it as data rather than by reading English
	// out of a finding's reason. Each also appears in Failing or Reported at its
	// own level; this field says which of them never ran.
	PolicyNotEvaluated []policy.Unevaluated `json:"policyNotEvaluated,omitempty"`
}

// policyIssuePrefix is the prefix findings.FromPolicy puts on every policy
// finding's Issue. isUnevaluatedRule uses it, together with an absent Name, to
// tell a rule that never ran apart from a violation, without adding a field to
// findings.Finding (which would widen the schema drift Task 16 already owns).
const policyIssuePrefix = "policy/"

// isUnevaluatedRule reports whether f is a policy rule that never ran rather
// than an ordinary finding. FromPolicy leaves Namespace, Name and Owner empty
// for an unevaluated rule because no object was examined — a violation's Name
// always comes from a real object's obj.GetName(), which the API server never
// leaves empty — so the prefix plus the absent Name identifies it without a
// dedicated field on findings.Finding.
func isUnevaluatedRule(f findings.Finding) bool {
	return strings.HasPrefix(f.Issue, policyIssuePrefix) && f.Name == ""
}

// inScope reports whether a finding is attributable to the gate's --wait-for
// workload. An unscoped gate judges everything.
func (o Options) inScope(f findings.Finding) bool {
	if o.ScopeKind == "" {
		return true
	}
	// A policy rule that could not be evaluated is a statement about
	// enforcement coverage, not about any object: findings.FromPolicy leaves
	// Namespace, Name and Owner empty because none was examined, so it can
	// satisfy neither branch below and a scoped gate would always drop it —
	// silently turning "this rule never ran" into "this rollout is fine".
	// Detect it with isUnevaluatedRule rather than by "empty Namespace and
	// Name" alone: a policy violation's Name always comes from a real
	// object's obj.GetName(), which the API server never leaves empty, so a
	// violation keeps going through the normal scope check below exactly as
	// before, and no other finding kind uses this signal.
	if isUnevaluatedRule(f) {
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
// Precedence is timeout, then an unevaluated policy rule at or above
// --fail-on, then inconclusive, then fail, then pass. Timeout wins because a
// rollout that never settled makes every other judgement premature.
// Ordinarily inconclusive outranks fail, because a run that could not see the
// cluster has not earned the right to report a confident failure — or a
// confident pass, which is the green-when-blind case this whole command
// exists to prevent. An unevaluated rule is carved out ahead of that: for an
// ordinary finding, "blind" and "fail" describe two different facts (the read
// that failed is not the thing that would have failed), so the blinder fact
// wins. For a rule kubeagent could not run, the read failure and the policy
// failure are the *same* fact — the rule is unevaluated *because* the read
// failed — so downgrading it to merely inconclusive would let waiving the
// read failure with --allow-partial-read make a rule that never ran look less
// bad, which is backwards: the rule still never ran either way. Do not fold
// this case back into the inconclusive branch; that is the bug this carve-out
// fixes. Verdict.Inconclusive still lists the read failure regardless — an
// operator must keep seeing what kubeagent could not read even when the
// top-level verdict is "fail".
func Decide(res scan.Result, opts Options) Verdict {
	v := Verdict{
		SchemaVersion: jsonschema.GateVersion,
		FailOn:        opts.FailOn,
		Scope:         "cluster",
		Detail:        opts.TimeoutDetail,
		Failing:       []findings.Finding{},
		Reported:      []findings.Finding{},
		Inconclusive:  []Blindspot{},
	}
	if opts.ScopeKind != "" {
		v.Scope = fmt.Sprintf("%s/%s in %s", opts.ScopeKind, opts.ScopeName, opts.ScopeNamespace)
	}
	v.PolicyNotEvaluated = opts.PolicyNotEvaluated

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

	all := findings.Flatten(res)
	all = append(all, findings.FromPolicy(opts.PolicyViolations, opts.PolicyNotEvaluated)...)
	findings.Sort(all)

	for _, f := range all {
		if opts.inScope(f) && f.Level >= opts.FailOn {
			v.Failing = append(v.Failing, f)
			continue
		}
		v.Reported = append(v.Reported, f)
	}
	findings.Sort(v.Failing)
	findings.Sort(v.Reported)

	// unevaluatedRuleFailing is true when an in-scope policy rule that never
	// ran is at or above --fail-on. Read off v.Failing, after scoping, rather
	// than opts.PolicyNotEvaluated directly: inScope already keeps these
	// findings in scope under a --wait-for-scoped gate (see the
	// isUnevaluatedRule branch above), and a rule below --fail-on lands in
	// v.Reported, not here, so it must not affect the verdict.
	unevaluatedRuleFailing := false
	for _, f := range v.Failing {
		if isUnevaluatedRule(f) {
			unevaluatedRuleFailing = true
			break
		}
	}

	switch {
	case opts.TimedOut:
		v.Verdict, v.Code = "timeout", CodeTimeout
	case unevaluatedRuleFailing:
		v.Verdict, v.Code = "fail", CodeFail
	case blind:
		v.Verdict, v.Code = "inconclusive", CodeInconclusive
	case len(v.Failing) > 0:
		v.Verdict, v.Code = "fail", CodeFail
	default:
		v.Verdict, v.Code = "pass", CodePass
	}
	return v
}
