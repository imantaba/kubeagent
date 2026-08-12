package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/connectivity"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/gate"
	"github.com/imantaba/kubeagent/internal/rolloutwait"
	"github.com/imantaba/kubeagent/internal/sarif"
	"github.com/imantaba/kubeagent/internal/scan"
)

// scopeTo narrows opts to the workload --wait-for named.
//
// It is a function rather than three inline assignments so a test can drive the
// exact line runGate uses: swapping name and namespace would scope the gate to
// a workload that does not exist, which reads as a clean pass — the failure
// this whole command exists to prevent.
func scopeTo(opts gate.Options, t rolloutwait.Target) gate.Options {
	opts.ScopeKind, opts.ScopeName, opts.ScopeNamespace = t.Kind, t.Name, t.Namespace
	return opts
}

// gateScanOptions builds the scan.Options runGate hands to scan.Evaluate. It
// is the only place runGate builds them, and it exists as its own function —
// rather than a literal inline at the call site — for the same reason
// scopeTo does: so a test can drive the exact values runGate uses without a
// live cluster. That matters here because scan.Evaluate clamps an
// out-of-range or zero threshold back to its own default, which would
// silently mask a bug where an env var never reached the struct at all.
//
// Any warning about the environment goes to w, so a caller that must not
// interleave with its own output can redirect it. A fleet sweep calls this
// once for the whole run, not once per cluster, so an unusable
// KUBEAGENT_QUOTA_THRESHOLD is reported once however many clusters follow.
func gateScanOptions(namespace string, w io.Writer) scan.Options {
	return scan.Options{
		Namespace:               namespace,
		QuotaThreshold:          quotaThresholdFromEnv(w),
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
	}
}

// gateOptions is `kubeagent gate`'s parsed command line. One field per flag,
// in declaration order. It exists so flag wiring is testable without a
// cluster: parseGateFlags is pure, and runGateOpts does the I/O.
type gateOptions struct {
	kubeconfig       string
	contextName      string
	output           string
	failOn           string
	waitFor          string
	timeout          time.Duration
	pollInterval     time.Duration
	allowPartialRead []string
	policyPaths      []string
	policyPackNames  []string
	baselinePath     string
	baselineFactor   float64
	baselineFloor    float64
	namespace        string
}

// bindGateFlags declares gate's flags on cmd, writing into o. Flag names,
// defaults and usage strings are unchanged from the standard-library FlagSet
// this replaces, except --namespace/-n: the standard library needed two
// separate declarations to accept both spellings, pflag expresses that as one
// flag with a shorthand, and only the long form's usage text survives.
// --allow-partial-read uses StringArrayVar, not StringSliceVar: the slice
// form splits its input on commas, which would silently turn one
// comma-containing resource name into two.
func bindGateFlags(cmd *cobra.Command, o *gateOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	f.StringVar(&o.output, "output", "text", "output format: text | json | sarif")
	f.StringVar(&o.failOn, "fail-on", "critical", "fail the gate at this severity or above: critical | warning | info")
	f.StringVar(&o.waitFor, "wait-for", "", "post-deploy verify: wait for this workload's rollout to settle, then judge only it (kind/name, e.g. deployment/api)")
	f.DurationVar(&o.timeout, "timeout", 5*time.Minute, "with --wait-for: give up waiting after this long (exit 3)")
	f.DurationVar(&o.pollInterval, "poll-interval", 2*time.Second, "with --wait-for: how often to re-read the workload")
	f.StringArrayVar(&o.allowPartialRead, "allow-partial-read", nil, "accept that this resource cannot be read, instead of exiting 2 (repeatable, e.g. leases)")
	f.StringArrayVar(&o.policyPaths, "policy", nil, "evaluate organization-specific checks from this policy file or directory (repeatable)")
	f.StringArrayVar(&o.policyPackNames, "policy-pack", nil, "evaluate a curated rule pack compiled into kubeagent (repeatable; see `kubeagent policy packs`)")
	f.StringVar(&o.baselinePath, "baseline", "", "compare restart rates against this captured baseline (see "+invokedAs+" baseline capture)")
	f.Float64Var(&o.baselineFactor, "baseline-factor", envFloat("KUBEAGENT_BASELINE_FACTOR", baseline.DefaultFactor),
		"with --baseline: flag a workload at this multiple of its baseline rate (KUBEAGENT_BASELINE_FACTOR)")
	f.Float64Var(&o.baselineFloor, "baseline-floor", envFloat("KUBEAGENT_BASELINE_FLOOR", baseline.DefaultFloor),
		"with --baseline: also require this absolute rise in restarts/hour (KUBEAGENT_BASELINE_FLOOR)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace to judge (default: all namespaces)")
}

// parseGateFlags parses `kubeagent gate`'s command line. Pure: it contacts no
// cluster and writes nothing. It returns a plain error rather than an
// *exitError — the exit-4-on-parse-failure contract belongs to runGate, not
// here, so this stays usable from a test that just wants the parsed values.
// It builds a throwaway command so the flag declarations have exactly one
// home, in bindGateFlags.
func parseGateFlags(args []string) (gateOptions, error) {
	var o gateOptions
	cmd := &cobra.Command{Use: "gate", SilenceErrors: true, SilenceUsage: true}
	bindGateFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
		return gateOptions{}, err
	}
	return o, nil
}

// runGateOpts serves `kubeagent gate`. o is the already-parsed command line,
// as produced by parseGateFlags.
//
// Every flag error returns exit 4 rather than writing an empty SARIF
// document: a valid, empty SARIF would upload as a clean scan, so a typo in a
// flag name must never read as "no problems found".
func runGateOpts(o gateOptions) error {
	if o.output != "text" && o.output != "json" && o.output != "sarif" {
		return &exitError{code: gate.CodeUsage,
			msg: fmt.Sprintf("unknown output format %q (want text, json or sarif)", o.output)}
	}
	level, err := findings.Parse(o.failOn)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	if o.pollInterval <= 0 {
		return &exitError{code: gate.CodeUsage,
			msg: fmt.Sprintf("--poll-interval must be positive, got %s", o.pollInterval)}
	}
	var target rolloutwait.Target
	if o.waitFor != "" {
		target, err = rolloutwait.ParseTarget(o.waitFor, o.namespace)
		if err != nil {
			return &exitError{code: gate.CodeUsage, msg: err.Error()}
		}
	}

	// Exit 4 for a bad baseline file, for the same reason a bad policy file
	// takes it: bad input, in the same class as a bad flag, and nothing was
	// attempted against the cluster. Exit 1 would claim kubeagent looked and
	// found problems.
	baselineDoc, err := loadBaseline(o.baselinePath)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}

	// Exit 4, not 1: cluster.NewClient builds a rest.Config and a clientset
	// without touching the network, so its failures are an unusable kubeconfig
	// or context — bad input, in the same class as a bad flag. Nothing was
	// attempted against any cluster, and exit 1 would claim kubeagent looked
	// and found problems.
	client, err := cluster.NewClient(o.kubeconfig, o.contextName)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	ctx := context.Background()

	opts := gate.Options{FailOn: level, AllowPartialRead: o.allowPartialRead}

	// Exit 4 for a bad policy file: it is bad input, in the same class as a bad
	// flag, and nothing was attempted against the cluster. Exit 1 would claim
	// kubeagent looked and found problems.
	pv, err := evaluatePolicy(ctx, o.policyPaths, o.policyPackNames, o.kubeconfig, o.contextName, o.namespace)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	if pv != nil {
		opts.PolicyViolations, opts.PolicyNotEvaluated = pv.Violations, pv.NotEvaluated
	}

	if o.waitFor != "" {
		opts = scopeTo(opts, target)
		res, err := rolloutwait.Wait(ctx, client, target, o.timeout, o.pollInterval, rolloutwait.Real{})
		if err != nil {
			// Exit 2: the poll reached the cluster and could not read the
			// workload — an RBAC denial or an unreachable API. That is the same
			// claim a partial read makes, so it gets the same code.
			if diag, ok := connectivity.Diagnose(err); ok {
				return &exitError{code: gate.CodeInconclusive,
					msg: fmt.Sprintf("%s\ndetails: %v", diag, err)}
			}
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
		opts.TimedOut, opts.TimeoutDetail = !res.Settled, res.Detail
		if o.output == "text" {
			fmt.Fprintf(os.Stdout, "%s/%s in %s: %s\n\n", target.Kind, target.Name, target.Namespace, res.Detail)
		}
	}

	// A bare scan, configured the same way scan and watch configure it —
	// including its env-tunable thresholds (KUBEAGENT_QUOTA_THRESHOLD,
	// KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS) — so gate judges the same default
	// check set a bare kubeagent scan would flag. The opt-in advisory
	// sections are deliberately not exposed on gate in this slice: each one
	// is extra API reads and its own gate tests, and adding them later is
	// additive and breaks no contract.
	scanRes, err := scan.Evaluate(ctx, client, gateScanOptions(o.namespace, os.Stderr))
	if err != nil {
		// Exit 2 for the same reason the wait uses it: the scan failed outright,
		// so there is no verdict, and a gate that saw nothing must never report
		// the confident failure exit 1 stands for.
		if diag, ok := connectivity.Diagnose(err); ok {
			return &exitError{code: gate.CodeInconclusive,
				msg: fmt.Sprintf("%s\ndetails: %v", diag, err)}
		}
		return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
	}

	opts.Baseline = baselineReport(baselineDoc, o.baselineFactor, o.baselineFloor, scanRes.Inputs, time.Now())

	verdict := gate.Decide(scanRes, opts)

	// Rendering failures take exit 2 for the same reason the scan's do: the
	// verdict exists but never reached the pipeline, so there is nothing for it
	// to read. A half-written SARIF document on a closed pipe must not exit 1
	// and claim kubeagent found problems.
	switch o.output {
	case "json":
		b, err := json.MarshalIndent(verdict, "", "  ")
		if err != nil {
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
		if _, err := fmt.Fprintf(os.Stdout, "%s\n", b); err != nil {
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
	case "sarif":
		b, err := sarif.Render(verdict, version)
		if err != nil {
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
		if _, err := os.Stdout.Write(b); err != nil {
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
	default:
		if err := gate.RenderText(os.Stdout, verdict); err != nil {
			return &exitError{code: gate.CodeInconclusive, msg: err.Error()}
		}
	}

	if verdict.Code != gate.CodePass {
		// The verdict is already on stdout; an empty msg keeps main() from
		// printing a second, redundant error line.
		return &exitError{code: verdict.Code}
	}
	return nil
}

// runGate is the CI/CD gate: scan once, judge, and exit with a code a
// pipeline can branch on. Read-only, and it makes no LLM call — the whole
// point is a deterministic verdict a build can depend on.
//
// A flag-parse failure still exits 4, exactly as before the parser and the
// runner split apart: parseGateFlags returns a plain error, and this wrapper
// gives it the same gate.CodeUsage wrapping every other usage error gets.
func runGate(args []string) error {
	o, err := parseGateFlags(args)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
	return runGateOpts(o)
}

// newGateCommand builds `kubeagent gate`.
func newGateCommand() *cobra.Command {
	var o gateOptions
	cmd := &cobra.Command{
		Use:           "gate",
		Short:         "Judge a scan and exit with a code CI can branch on",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGateOpts(o)
		},
	}
	bindGateFlags(cmd, &o)
	// Without this, a bad flag reaches Main as a plain error and exits 1 —
	// reading to a pipeline as "kubeagent found problems" rather than "the
	// flags were wrong", the exact confusion the five-code contract exists to
	// prevent.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	})
	return cmd
}
