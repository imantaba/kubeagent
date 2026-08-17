package mcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/scan"
)

// MaxFindings caps a tool result. A model pays for every token it reads, and a
// thousand-finding payload from a broken cluster is worse than fifty findings
// plus an honest count of what was dropped.
const MaxFindings = 50

// Finding is one problem, flattened for a model to read. It is deliberately a
// separate type from diagnose.Finding and from every *health.Issue: those are
// internal shapes that change with the detectors, while this one is the
// published contract.
type Finding struct {
	// Severity is "critical" (a detector matched a concrete failure mode) or
	// "warning" (a health check flagged something that needs a human).
	Severity        string `json:"severity"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
	Confidence      string `json:"confidence,omitempty"`
	RemediationHint string `json:"remediationHint,omitempty"`
}

func splitNamespacedName(s string) (namespace, name string) {
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func fromDiagnose(f diagnose.Finding) Finding {
	ns, name := splitNamespacedName(f.Pod)
	detail := f.Reason
	if f.Evidence != "" {
		detail = strings.TrimSpace(detail + " (" + f.Evidence + ")")
	}
	return Finding{
		Severity:        "critical",
		Kind:            "Pod",
		Namespace:       ns,
		Name:            name,
		Reason:          f.Issue,
		Detail:          detail,
		Confidence:      f.Confidence,
		RemediationHint: remediation.For(f).NextStep,
	}
}

// pdbCategoryToIssue maps a pdbhealth.Issue's lowercase Category onto the
// finding vocabulary's CamelCase spelling shared by gate JSON, the watch
// daemon's /issues and this tool result. A category this map does not
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
	"unable":    "HPAUnableToScale",
	"metrics":   "HPAMetricsFailed",
	"disabled":  "HPAScalingDisabled",
	"ambiguous": "HPAAmbiguousSelector",
	"capped":    "HPACapped",
}

// hpaIssue applies hpaCategoryToIssue, falling back to the raw category on a
// miss.
func hpaIssue(category string) string {
	if v, ok := hpaCategoryToIssue[category]; ok {
		return v
	}
	return category
}

func fromWorkload(w inventory.Workload) Finding {
	return Finding{
		Severity:  "warning",
		Kind:      w.Kind,
		Namespace: w.Namespace,
		Name:      w.Name,
		Reason:    w.Status,
		Detail:    fmt.Sprintf("%d/%d ready", w.Ready, w.Desired),
	}
}

// findingsFromResult projects every attention-worthy class scan.Result carries
// into one flat list. The classes mirror the text report's hasAttention
// expression: leaving one out would make a triage result say "healthy" about a
// cluster the CLI calls degraded. Two of hasAttention's inputs — credential
// lint and disk usage — are computed by the CLI outside scan.Evaluate; the
// triage handler declares those as skipped checks rather than pretending they
// were clean.
func findingsFromResult(res scan.Result) []Finding {
	out := []Finding{}

	for _, w := range res.Inventory.Workloads {
		if len(w.Findings) == 0 {
			// A workload can reach here unflagged: Prioritize includes
			// restart-only and idle-cron workloads whenever the triage handler
			// sets IncludeRestarts/IncludeCron, and those are healthy right now
			// — reporting them as warnings would be a false positive.
			if w.Flagged() {
				out = append(out, fromWorkload(w))
			}
			continue
		}
		for _, f := range w.Findings {
			out = append(out, fromDiagnose(f))
		}
	}

	for _, i := range res.ServiceIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Severity: "warning", Kind: "Service", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Problem, Detail: i.Detail})
	}
	for _, i := range res.IngressIssues {
		if i.Expected {
			continue
		}
		out = append(out, Finding{Severity: "warning", Kind: "Ingress", Namespace: i.Namespace,
			Name: i.Ingress, Reason: i.Problem, Detail: i.Detail})
	}
	for _, i := range res.PVCIssues {
		out = append(out, Finding{Severity: "warning", Kind: "PersistentVolumeClaim", Namespace: i.Namespace,
			Name: i.Name, Reason: i.Reason, Detail: i.Detail})
	}
	for _, i := range res.StuckTerminating {
		out = append(out, Finding{Severity: "warning", Kind: i.Kind, Namespace: i.Namespace,
			Name: i.Name, Reason: "StuckTerminating", Detail: i.Reason})
	}
	for _, i := range res.PDBIssues {
		out = append(out, Finding{Severity: "warning", Kind: "PodDisruptionBudget", Namespace: i.Namespace,
			Name: i.Name, Reason: pdbIssue(i.Category), Detail: i.Reason})
	}
	for _, i := range res.HPAIssues {
		out = append(out, Finding{Severity: "warning", Kind: "HorizontalPodAutoscaler", Namespace: i.Namespace,
			Name: i.Name, Reason: hpaIssue(i.Category), Detail: i.Reason})
	}
	for _, i := range res.WebhookIssues {
		out = append(out, Finding{Severity: "warning", Kind: i.Kind, Namespace: "",
			Name: i.Config, Reason: i.Problem, Detail: i.Reason})
	}
	for _, i := range res.QuotaIssues {
		out = append(out, Finding{Severity: "warning", Kind: "ResourceQuota", Namespace: i.Namespace,
			Name: i.Quota, Reason: i.Severity, Detail: fmt.Sprintf("%s %s/%s used", i.Resource, i.Used, i.Hard)})
	}
	sortFindings(out)
	return out
}

// severityRank orders severities without depending on their spelling sorting
// the right way (alphabetically "critical" < "warning" is a coincidence).
func severityRank(s string) int {
	if s == "critical" {
		return 0
	}
	return 1
}

// sortFindings imposes a total order, so two scans of an unchanged cluster
// produce byte-identical payloads and a caller can diff them.
func sortFindings(f []Finding) {
	sort.SliceStable(f, func(a, b int) bool {
		x, y := f[a], f[b]
		if ra, rb := severityRank(x.Severity), severityRank(y.Severity); ra != rb {
			return ra < rb
		}
		if x.Namespace != y.Namespace {
			return x.Namespace < y.Namespace
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Reason < y.Reason
	})
}

// capFindings truncates to MaxFindings and reports how many were dropped. It
// truncates in the order given — it does not sort — so a critical finding
// survives ahead of a warning only if the caller already ran sortFindings.
func capFindings(f []Finding) ([]Finding, int) {
	if len(f) <= MaxFindings {
		return f, 0
	}
	return f[:MaxFindings], len(f) - MaxFindings
}
