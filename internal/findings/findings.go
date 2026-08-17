// Package findings owns kubeagent's severity model for the CI gate: an ordered
// Level, the finding-kind -> level table, and the projection of a scan.Result
// into one flat, deterministically ordered list.
//
// Severity is assigned ad hoc elsewhere in the tree (internal/mcp/view.go,
// internal/watch/issues.go, internal/report, internal/gitops,
// internal/quotahealth). This package does not replace those: internal/gate is
// its only consumer for now, because migrating the others would change the MCP
// tool payloads shipped in v0.63.0 and regenerate the golden report fixture.
// The table below deliberately mirrors internal/mcp/view.go so the two agree.
//
// Pure: no cluster calls, no LLM calls.
package findings

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Level is a finding's severity, ordered so a threshold comparison is a plain
// >= and does not depend on how the names happen to sort.
type Level int

const (
	// Info is reserved: no detector emits it yet, but --fail-on info must have
	// a meaning, and adding an informational class later must not renumber the
	// levels above it.
	Info Level = iota
	Warning
	Critical
)

func (l Level) String() string {
	switch l {
	case Critical:
		return "critical"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// MarshalJSON emits the spelling, not the ordinal: the JSON is a published
// contract and must not change if a level is inserted between two others.
func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

// Parse turns a --fail-on value into a Level.
func Parse(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return Info, nil
	case "warning":
		return Warning, nil
	case "critical":
		return Critical, nil
	}
	return Info, fmt.Errorf("unknown level %q (want critical, warning or info)", s)
}

// Finding is one problem with a severity attached. Owner names the workload the
// finding hangs off ("Deployment/api"), which is what lets the gate scope a
// post-deploy verify to one rollout: a diagnose.Finding carries only a pod name
// and no controller reference.
type Finding struct {
	Level     Level  `json:"level"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Issue     string `json:"issue"`
	Reason    string `json:"reason"`
	Owner     string `json:"owner,omitempty"`
}

// splitNamespacedName splits diagnose.Finding.Pod ("namespace/name"). A value
// with no slash is treated as a bare name in no namespace rather than dropped.
func splitNamespacedName(s string) (namespace, name string) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// fromDiagnose maps a detector match. Every diagnose.Finding is Critical: a
// detector fires only on a concrete, named failure mode, never on a heuristic.
func fromDiagnose(f diagnose.Finding, owner string) Finding {
	ns, name := splitNamespacedName(f.Pod)
	reason := f.Reason
	if f.Evidence != "" {
		reason = strings.TrimSpace(reason + " (" + f.Evidence + ")")
	}
	return Finding{
		Level: Critical, Kind: "Pod", Namespace: ns, Name: name,
		Issue: f.Issue, Reason: reason, Owner: owner,
	}
}

// fromWorkload maps a workload that Flagged() but produced no detector match:
// something is wrong, but no detector named it, so it is a Warning.
func fromWorkload(w inventory.Workload) Finding {
	return Finding{
		Level: Warning, Kind: w.Kind, Namespace: w.Namespace, Name: w.Name,
		Issue:  w.Status,
		Reason: fmt.Sprintf("%d/%d ready", w.Ready, w.Desired),
		Owner:  w.Kind + "/" + w.Name,
	}
}

// pdbCategoryToIssue maps a pdbhealth.Issue's lowercase Category onto the
// finding vocabulary's CamelCase spelling shared by gate JSON, the watch
// daemon's /issues and the MCP tool result. A category this map does not
// recognise falls back to the raw value instead of vanishing, so a category
// landing out of order (WP14 teaches pdbhealth.Assess to emit "singleton"
// after this map already carries it) still renders rather than disappearing
// silently.
var pdbCategoryToIssue = map[string]string{
	"unsatisfiable": "PDBUnsatisfiable",
	"stale":         "PDBStale",
	"blocking":      "PDBBlocked",
	"singleton":     "PDBSingleton",
}

// pdbIssue applies pdbCategoryToIssue, falling back to the raw category on a
// miss.
func pdbIssue(category string) string {
	if v, ok := pdbCategoryToIssue[category]; ok {
		return v
	}
	return category
}

// hpaCategoryToIssue is pdbCategoryToIssue's HPA counterpart, same
// fallback-on-miss rule.
var hpaCategoryToIssue = map[string]string{
	"unable":  "HPAUnableToScale",
	"metrics": "HPAMetricsFailed",
	"capped":  "HPACapped",
}

// hpaIssue applies hpaCategoryToIssue, falling back to the raw category on a
// miss.
func hpaIssue(category string) string {
	if v, ok := hpaCategoryToIssue[category]; ok {
		return v
	}
	return category
}

// Flatten projects every attention-worthy class scan.Result carries into one
// ordered list. The classes mirror internal/mcp/view.go's findingsFromResult:
// leaving one out would make the gate pass a cluster the CLI calls degraded.
func Flatten(res scan.Result) []Finding {
	out := []Finding{}

	for _, w := range res.Inventory.Workloads {
		owner := w.Kind + "/" + w.Name
		if len(w.Findings) == 0 {
			// Prioritize includes restart-only and idle-cron workloads that are
			// healthy right now; reporting those would be a false positive.
			if w.Flagged() {
				out = append(out, fromWorkload(w))
			}
			continue
		}
		for _, f := range w.Findings {
			out = append(out, fromDiagnose(f, owner))
		}
	}

	for _, i := range res.ServiceIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Level: Warning, Kind: "Service", Namespace: i.Namespace,
			Name: i.Name, Issue: i.Problem, Reason: i.Detail})
	}
	for _, i := range res.IngressIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Level: Warning, Kind: "Ingress", Namespace: i.Namespace,
			Name: i.Ingress, Issue: i.Problem, Reason: i.Detail})
	}
	for _, i := range res.PVCIssues {
		out = append(out, Finding{Level: Warning, Kind: "PersistentVolumeClaim", Namespace: i.Namespace,
			Name: i.Name, Issue: i.Reason, Reason: i.Detail})
	}
	for _, i := range res.StuckTerminating {
		out = append(out, Finding{Level: Warning, Kind: i.Kind, Namespace: i.Namespace,
			Name: i.Name, Issue: "StuckTerminating", Reason: i.Reason})
	}
	for _, i := range res.PDBIssues {
		out = append(out, Finding{Level: Warning, Kind: "PodDisruptionBudget", Namespace: i.Namespace,
			Name: i.Name, Issue: pdbIssue(i.Category), Reason: i.Reason})
	}
	for _, i := range res.HPAIssues {
		out = append(out, Finding{Level: Warning, Kind: "HorizontalPodAutoscaler", Namespace: i.Namespace,
			Name: i.Name, Issue: hpaIssue(i.Category), Reason: i.Reason})
	}
	for _, i := range res.WebhookIssues {
		out = append(out, Finding{Level: Warning, Kind: i.Kind, Namespace: "",
			Name: i.Config, Issue: i.Problem, Reason: i.Reason})
	}
	for _, i := range res.QuotaIssues {
		out = append(out, Finding{Level: Warning, Kind: "ResourceQuota", Namespace: i.Namespace,
			Name: i.Quota, Issue: i.Severity,
			Reason: fmt.Sprintf("%s %s/%s used", i.Resource, i.Used, i.Hard)})
	}

	Sort(out)
	return out
}

// policyLevel maps a policy level onto the gate's severity ordering. The two
// types stay separate on purpose: internal/policy may not import this package
// (findings imports scan, scan imports policy), and a policy file's vocabulary
// should not be a Go ordinal.
func policyLevel(l policy.Level) Level {
	switch l {
	case policy.LevelCritical:
		return Critical
	case policy.LevelWarning:
		return Warning
	default:
		return Info
	}
}

// FromPolicy projects a policy run into findings.
//
// Issue is "policy/<ruleID>": internal/sarif uses Issue as the SARIF rule id
// verbatim, and without the prefix an operator's rule named "OOMKilled" would
// merge with the detector of that name in a code-scanning dashboard.
//
// A rule that could not be evaluated becomes a finding at its own level rather
// than a Blindspot. --allow-partial-read exists to waive a blind spot the
// operator accepted; an operator who wrote a rule did not accept not running
// it. It carries no Name, because no object was examined.
func FromPolicy(violations []policy.Violation, notEvaluated []policy.Unevaluated) []Finding {
	out := make([]Finding, 0, len(violations)+len(notEvaluated))

	for _, v := range violations {
		reason := v.Message
		if v.Evidence != "" {
			reason = strings.TrimSpace(reason + " (" + v.Evidence + ")")
		}
		out = append(out, Finding{
			Level: policyLevel(v.Level), Kind: v.Kind,
			Namespace: v.Namespace, Name: v.Name,
			Issue: "policy/" + v.RuleID, Reason: reason,
		})
	}
	for _, u := range notEvaluated {
		out = append(out, Finding{
			Level: policyLevel(u.Level), Kind: u.Kind,
			Issue: "policy/" + u.RuleID, Reason: u.Reason,
		})
	}
	return out
}

// FromBaseline maps restart-rate deviations to findings at Info.
//
// Info is where the level's own comment reserved it: "no detector emits it yet,
// but --fail-on info must have a meaning". This is that meaning. A deviation is
// an inference from a learned rate, not a detector match on a concrete named
// failure mode, so it is reported at every --fail-on setting and fails a gate
// only at --fail-on info — which is the operator opting in. Because
// --fail-on defaults to critical, no existing pipeline changes behavior.
//
// It carries no Owner: the deviation already names the workload itself in
// Kind/Namespace/Name, which is the first branch a --wait-for-scoped gate
// matches on.
func FromBaseline(r *baseline.Report) []Finding {
	if r == nil {
		return nil
	}
	out := make([]Finding, 0, len(r.Deviations))
	for _, d := range r.Deviations {
		out = append(out, Finding{
			Level: Info, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name,
			Issue:  "RestartRateDeviation",
			Reason: baselineReason(d),
		})
	}
	return out
}

// baselineReason renders the two rates and the size of the change. A zero
// baseline has no multiple, so it reports only how many pods are behind the
// current rate.
func baselineReason(d baseline.Deviation) string {
	if d.BaselineRate <= 0 {
		return fmt.Sprintf("%.2f -> %.2f restarts/hour (%d pods)", d.BaselineRate, d.CurrentRate, d.Pods)
	}
	return fmt.Sprintf("%.2f -> %.2f restarts/hour (%.0fx baseline, %d pods)",
		d.BaselineRate, d.CurrentRate, d.CurrentRate/d.BaselineRate, d.Pods)
}

// Sort imposes a total order so an unchanged cluster renders byte-identical
// output and two runs diff cleanly. Highest severity first, then by object.
func Sort(f []Finding) {
	sort.SliceStable(f, func(a, b int) bool {
		x, y := f[a], f[b]
		if x.Level != y.Level {
			return x.Level > y.Level
		}
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		return x.Issue < y.Issue
	})
}
