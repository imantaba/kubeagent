package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/confidence"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/gitops"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/jsonschema"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/operators"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/policy"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/webhookhealth"
)

// ScanReport is the JSON document written by `kubeagent scan --output json`. It
// is exported because internal/schemadoc has to name it to generate its
// published schema; nothing outside this package constructs one.
type ScanReport struct {
	SchemaVersion      string                      `json:"schemaVersion"`
	Cluster            clusterhealth.ClusterHealth `json:"cluster"`
	Workloads          []inventory.Workload        `json:"workloads"`
	Resources          *resources.Summary          `json:"resources,omitempty"`
	Platform           *platform.Facts             `json:"platform,omitempty"`
	ServiceIssues      []svchealth.Issue           `json:"serviceIssues,omitempty"`
	CredentialWarnings []credlint.Finding          `json:"credentialWarnings,omitempty"`
	NodeReserve        *nodereserve.Report         `json:"nodeReserve,omitempty"`
	PVCReclaim         *pvcreclaim.Report          `json:"pvcReclaim,omitempty"`
	DiskUsage          *diskusage.Report           `json:"diskUsage,omitempty"`
	IngressIssues      []ingresshealth.RouteIssue  `json:"ingressIssues,omitempty"`
	PVCIssues          []pvchealth.Issue           `json:"pvcIssues,omitempty"`
	SecurityIssues     []secscan.Finding           `json:"securityIssues,omitempty"`
	KubeletHealth      *nodehealth.Report          `json:"kubeletHealth,omitempty"`
	ControlPlane       *controlplane.Probe         `json:"controlPlane,omitempty"`
	DNS                *dnshealth.Report           `json:"dns,omitempty"`
	Certificates       *certhealth.Report          `json:"certificates,omitempty"`
	Operators          *operators.Report           `json:"operators,omitempty"`
	GitOps             *gitops.Report              `json:"gitops,omitempty"`
	Capacity           *capacity.Report            `json:"capacity,omitempty"`
	StuckTerminating   []termhealth.Issue          `json:"stuckTerminating,omitempty"`
	PDBIssues          []pdbhealth.Issue           `json:"pdbIssues,omitempty"`
	HPAIssues          []hpahealth.Issue           `json:"hpaIssues,omitempty"`
	WebhookIssues      []webhookhealth.Issue       `json:"webhookIssues,omitempty"`
	QuotaIssues        []quotahealth.Issue         `json:"quotaIssues,omitempty"`
	Policy             *PolicyView                 `json:"policy,omitempty"`
	Baseline           *baseline.Report            `json:"baseline,omitempty"`
	BlindSpots         []scan.ReadFailure          `json:"blindSpots,omitempty"`
	Explanation        string                      `json:"explanation,omitempty"`
	Investigation      *InvestigationView          `json:"investigation,omitempty"`
	RemediationPlan    []RemediationActionView     `json:"remediationPlan,omitempty"`
}

type InvestigationView struct {
	Consulted []string `json:"consulted,omitempty"`
	Narrative string   `json:"narrative"`
}

// PolicyView is the outcome of a policy run: how many rules ran, what they
// found, and which of them could not run at all.
//
// NotEvaluated is not a footnote. A rule whose kind kubeagent may not read has
// not passed — it has not been checked — and a document that renders the two
// the same way is worse than one that omits the check entirely.
//
// There is deliberately no field for the file a rule came from: a filesystem
// path is a credential, and this document is written to be forwarded.
type PolicyView struct {
	Rules        int                  `json:"rules"`
	Violations   []policy.Violation   `json:"violations,omitempty"`
	NotEvaluated []policy.Unevaluated `json:"notEvaluated,omitempty"`
}

// investigationOf builds the JSON view, or nil when no investigation ran.
func investigationOf(in Input) *InvestigationView {
	if in.Investigation == "" {
		return nil
	}
	return &InvestigationView{Consulted: in.InvestigationConsulted, Narrative: in.Investigation}
}

// RemediationActionView is the JSON shape of one proposed --fix action. Status is
// always "proposed" in this slice; apply outcomes become durable in the audit-log
// slice.
type RemediationActionView struct {
	Kind              string             `json:"kind"`
	Target            string             `json:"target"`
	Summary           string             `json:"summary"`
	Reason            string             `json:"reason"`
	KubectlEquivalent string             `json:"kubectlEquivalent"`
	Changes           []remediate.Change `json:"changes,omitempty"`
	Status            string             `json:"status"`
}

func remediationPlanOf(in Input) []RemediationActionView {
	if len(in.RemediationPlan) == 0 {
		return nil
	}
	out := make([]RemediationActionView, len(in.RemediationPlan))
	for i, a := range in.RemediationPlan {
		out[i] = RemediationActionView{
			Kind: a.Kind, Target: a.Target, Summary: a.Summary, Reason: a.Reason,
			KubectlEquivalent: a.KubectlEquivalent, Changes: a.Changes, Status: "proposed",
		}
	}
	return out
}

// suggestFindings returns a copy of findings with each entry's Suggestion set
// from remediation.For, when that suggestion carries a next step. The input
// slice is never mutated in place: PrintInventory passes in.Result.Workloads
// into the encoded document by reference, and a print function that mutates
// its caller's data is a defect on its own terms, regardless of who else
// might read that data afterward.
func suggestFindings(findings []diagnose.Finding) []diagnose.Finding {
	if len(findings) == 0 {
		return findings
	}
	out := make([]diagnose.Finding, len(findings))
	for i, f := range findings {
		if s := remediation.For(f); s.NextStep != "" {
			f.Suggestion = &diagnose.Suggestion{NextStep: s.NextStep, Command: s.Command}
		}
		out[i] = f
	}
	return out
}

// suggestWorkloads returns a copy of workloads with each workload's findings
// annotated by suggestFindings. Used only for the JSON output path when
// --suggest is set; in.Result.Workloads itself is never touched.
func suggestWorkloads(workloads []inventory.Workload) []inventory.Workload {
	out := make([]inventory.Workload, len(workloads))
	for i, wl := range workloads {
		wl.Findings = suggestFindings(wl.Findings)
		out[i] = wl
	}
	return out
}

// Input carries everything the report renders. Bundled into a struct because the
// positional parameter list had grown unwieldy.
type Input struct {
	Cluster            clusterhealth.ClusterHealth
	Result             inventory.Result
	Resources          *resources.Summary
	Platform           *platform.Facts
	ServiceIssues      []svchealth.Issue
	CredentialWarnings []credlint.Finding
	NodeReserve        *nodereserve.Report
	PVCReclaim         *pvcreclaim.Report
	PVCReclaimFull     bool // --pvc-reclaim: expand the PVC list (text only)
	DiskUsage          *diskusage.Report
	IngressIssues      []ingresshealth.RouteIssue
	PVCIssues          []pvchealth.Issue
	SecurityIssues     []secscan.Finding
	SecurityVerbose    bool
	// SecurityRequested is true when --security was passed. internal/htmlreport
	// is its one reader: that package's doc comment says the document it
	// renders "is meant to be forwarded", so a reader of the page has no other
	// way to tell "no security findings" from "--security was never passed".
	// Rendered there, not exported: it has no json tag by design — SecurityIssues
	// is a bare slice on report.ScanReport, with no wrapper type (unlike
	// Policy *PolicyView, Capacity *capacity.Report, Baseline *baseline.Report)
	// for this bit to ride along on, so tagging it directly would move a
	// schemaVersion, which this decision refuses.
	SecurityRequested bool
	Suggest           bool
	KubeletHealth     *nodehealth.Report
	ControlPlane      *controlplane.Probe
	DNS               *dnshealth.Report
	Certificates      *certhealth.Report
	Operators         *operators.Report
	// GitOps is the advisory GitOps-drift view (opt-in --drift). Nil when the
	// flag is off, so a default scan's JSON is unchanged.
	GitOps *gitops.Report
	// Capacity is the advisory headroom and right-sizing view (opt-in --capacity).
	// Nil when the flag is off, so a default scan's output is unchanged. No json
	// tag: Input is never marshalled — the encoded struct is ScanReport.
	Capacity *capacity.Report
	// Policy is the custom-check view (opt-in --policy). Nil when the flag is
	// absent, so a default scan's text and JSON are unchanged.
	Policy *PolicyView
	// Baseline is the restart-rate comparison (opt-in --baseline). Nil when the
	// flag is absent, so a default scan's text and JSON are unchanged — which is
	// what keeps testdata/golden-scan.txt byte-identical.
	Baseline         *baseline.Report
	StuckTerminating []termhealth.Issue
	PDBIssues        []pdbhealth.Issue
	HPAIssues        []hpahealth.Issue
	WebhookIssues    []webhookhealth.Issue
	// WebhookURLBackends is scan.Result.WebhookURLBackends — the count of
	// in-scope Fail-policy webhooks backed by a clientConfig.url rather than a
	// Service, a backend kubeagent cannot check the reachability of. No json
	// tag: Input is never marshalled — the encoded struct is ScanReport.
	WebhookURLBackends int
	QuotaIssues        []quotahealth.Issue
	// Blind is scan.Result.PartialReads — the collector calls that failed, so a
	// refused read is distinguishable from an empty one. Reasons are rendered
	// verbatim here; see the safeReason comment in internal/htmlreport, which
	// classifies instead because that document is written to be forwarded.
	Blind       []scan.ReadFailure
	Explanation string
	// ExplanationTruncated is true when Explanation was cut short at the
	// model's own output-length ceiling. Rendered, not exported: it has no
	// json tag by design, the same rule InvestigationTruncated follows below
	// — a truncation flag on ScanReport would move a schemaVersion, which
	// this decision refuses.
	ExplanationTruncated   bool
	Investigation          string
	InvestigationConsulted []string
	// InvestigationTruncated is true when Investigation was cut short at the
	// model's own output-length ceiling. Rendered, not exported: it has no
	// json tag by design, the same rule as the two fields above it — a
	// truncation flag on report.InvestigationView would move a
	// schemaVersion, which this decision refuses.
	InvestigationTruncated bool
	// InvestigationSkipped is true when --investigate was passed but found
	// nothing to chase: no workload findings, no service findings, and a
	// cluster verdict that is not Degraded — the same three-way condition
	// investigate.Investigate itself uses to skip. Rendered, not exported:
	// no json tag by design, the same rule as the field above it — a
	// skipped-investigation flag on report.InvestigationView would move a
	// schemaVersion, which this decision refuses.
	InvestigationSkipped bool
	RemediationPlan      []remediate.Action // --fix: the proposed actions (JSON only)
	Now                  time.Time          // clock for relative ages; main sets time.Now(); zero → wall-clock
}

// stuckTerminatingSuppresses reports whether issue already explains why wl is
// failing, so wl's own NEEDS ATTENTION row would just repeat it.
//
// All four conjuncts are required. issue.Kind == "Pod" && wl.Kind == "Pod" is
// not redundant: termhealth.Issue.Kind ranges over Namespace, Pod and
// PersistentVolumeClaim, so a namespace+name match alone would let a
// stuck-terminating PersistentVolumeClaim (or Namespace) suppress a Pod
// workload that merely happens to share its namespace and name.
// wl.Status == "Failed" is deliberately narrower than wl.Flagged() (len(Findings)
// > 0 || Ready < Desired || Status == "Failed"): Flagged() would also suppress a
// terminating pod that is flagged for an unrelated reason — e.g. an
// ImagePullBackOff finding on a pod that also happens to be stuck
// terminating — losing that second finding. That is exactly the trap
// internal/rollouthealth's own comment warns about (rollouthealth.go:146-150):
// a workload failing for two independent reasons must not have one of them
// silently dropped by widening a suppression clause instead of replacing it.
func stuckTerminatingSuppresses(issue termhealth.Issue, wl inventory.Workload) bool {
	return issue.Kind == "Pod" && wl.Kind == "Pod" &&
		issue.Namespace == wl.Namespace && issue.Name == wl.Name &&
		wl.Status == "Failed"
}

// suppressStuckTerminatingRows drops a Failed Pod workload's row when a
// StuckTerminating issue already names the same pod: the terminating row
// explains *why* the pod is Failed (a finalizer, a grace period, or a
// pvc-protection block), so printing both duplicates one fact under two
// headings. The stuck-terminating row is the survivor because it carries the
// reason; the workload row would not.
//
// Allocates a new slice rather than filtering in place: the caller's
// in.Result.Workloads shares its backing array with internal/cli, which
// still holds scan.Result after PrintInventory returns, so reusing that
// array (e.g. workloads[:0]) would corrupt the caller's copy.
func suppressStuckTerminatingRows(workloads []inventory.Workload, issues []termhealth.Issue) []inventory.Workload {
	out := make([]inventory.Workload, 0, len(workloads))
	for _, wl := range workloads {
		suppressed := false
		for _, is := range issues {
			if stuckTerminatingSuppresses(is, wl) {
				suppressed = true
				break
			}
		}
		if !suppressed {
			out = append(out, wl)
		}
	}
	return out
}

// PrintInventory writes the cluster verdict and the prioritized workload set to w.
func PrintInventory(in Input, format string, w io.Writer) error {
	// Suppress before the format switch so the JSON workloads local (below)
	// and printInventoryText's NEEDS ATTENTION loop and summary line all
	// inherit the same filtered set from one change, rather than three.
	in.Result.Workloads = suppressStuckTerminatingRows(in.Result.Workloads, in.StuckTerminating)
	switch format {
	case "json":
		workloads := in.Result.Workloads
		if in.Suggest {
			workloads = suggestWorkloads(workloads)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(ScanReport{
			SchemaVersion:      jsonschema.ScanVersion,
			Cluster:            in.Cluster,
			Workloads:          workloads,
			Resources:          in.Resources,
			Platform:           in.Platform,
			ServiceIssues:      in.ServiceIssues,
			CredentialWarnings: in.CredentialWarnings,
			NodeReserve:        in.NodeReserve,
			PVCReclaim:         in.PVCReclaim,
			DiskUsage:          in.DiskUsage,
			IngressIssues:      in.IngressIssues,
			PVCIssues:          in.PVCIssues,
			SecurityIssues:     in.SecurityIssues,
			KubeletHealth:      in.KubeletHealth,
			ControlPlane:       in.ControlPlane,
			DNS:                in.DNS,
			Certificates:       in.Certificates,
			Operators:          in.Operators,
			GitOps:             in.GitOps,
			Capacity:           in.Capacity,
			StuckTerminating:   in.StuckTerminating,
			PDBIssues:          in.PDBIssues,
			HPAIssues:          in.HPAIssues,
			WebhookIssues:      in.WebhookIssues,
			QuotaIssues:        in.QuotaIssues,
			Policy:             in.Policy,
			Baseline:           in.Baseline,
			BlindSpots:         in.Blind,
			Explanation:        in.Explanation,
			Investigation:      investigationOf(in),
			RemediationPlan:    remediationPlanOf(in),
		})
	case "text":
		return printInventoryText(in, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

// nowOr returns t, or the wall clock when t is the zero value, so callers that
// don't set Input.Now keep rendering ages against time.Now() exactly as before.
func nowOr(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func printInventoryText(in Input, w io.Writer) error {
	now := nowOr(in.Now)
	real, expected := splitServiceIssues(in.ServiceIssues)
	realIng, expectedIng := splitIngressIssues(in.IngressIssues)

	if err := printHeader(in, real, realIng, w); err != nil {
		return err
	}

	hasDisk := in.DiskUsage != nil && len(in.DiskUsage.Over) > 0
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(realIng) > 0 || len(in.PVCIssues) > 0 || len(in.StuckTerminating) > 0 || len(in.PDBIssues) > 0 || len(in.HPAIssues) > 0 || len(in.WebhookIssues) > 0 || len(in.QuotaIssues) > 0
	if hasAttention {
		if _, err := fmt.Fprintln(w, "NEEDS ATTENTION"); err != nil {
			return err
		}
		for _, wl := range in.Result.Workloads {
			if err := printWorkload(wl, now, in.Suggest, w); err != nil {
				return err
			}
		}
		if err := printServiceIssues(real, "  ✗", now, w); err != nil {
			return err
		}
		if err := printCredentialWarnings(in.CredentialWarnings, w); err != nil {
			return err
		}
		if err := printDiskUsage(in.DiskUsage, w); err != nil {
			return err
		}
		if err := printIngressIssues(realIng, "  ✗", w); err != nil {
			return err
		}
		if err := printPVCIssues(in.PVCIssues, w); err != nil {
			return err
		}
		if err := printStuckTerminating(in.StuckTerminating, w); err != nil {
			return err
		}
		if err := printPDBIssues(in.PDBIssues, w); err != nil {
			return err
		}
		if err := printHPAIssues(in.HPAIssues, w); err != nil {
			return err
		}
		if err := printWebhookIssues(in.WebhookIssues, w); err != nil {
			return err
		}
		if err := printQuotaIssues(in.QuotaIssues, w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	hasSecurity := len(in.SecurityIssues) > 0
	if err := printSecurityIssues(in.SecurityIssues, in.SecurityVerbose, w); err != nil {
		return err
	}

	hasKubeletHealth := kubeletHealthRenders(in.KubeletHealth)
	if err := printKubeletHealth(in.KubeletHealth, w); err != nil {
		return err
	}

	hasControlPlane := controlPlaneRenders(in.ControlPlane)
	if err := printControlPlane(in.ControlPlane, w); err != nil {
		return err
	}

	hasDNS := dnsRenders(in.DNS)
	if err := printDNSHealth(in.DNS, w); err != nil {
		return err
	}

	hasCerts := certificatesRender(in.Certificates)
	if err := printCertificates(in.Certificates, w); err != nil {
		return err
	}

	if err := printOperators(in.Operators, w); err != nil {
		return err
	}

	if err := printGitOps(in.GitOps, w); err != nil {
		return err
	}

	if err := printCapacity(in.Capacity, w); err != nil {
		return err
	}

	if err := printPolicy(in.Policy, w); err != nil {
		return err
	}

	if err := printBaseline(in.Baseline, w); err != nil {
		return err
	}

	hasBlind := len(in.Blind) > 0
	if err := printBlindSpots(in.Blind, w); err != nil {
		return err
	}

	if err := printNotes(in, expected, expectedIng, w); err != nil {
		return err
	}

	if err := printContext(in, w); err != nil {
		return err
	}

	if !hasAttention && !hasSecurity && !hasKubeletHealth && !hasControlPlane && !hasDNS && !hasCerts && !hasBlind && in.Cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}

	if in.Explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ── (model-written, not pre-reviewed; verify every command before running)\n%s\n", in.Explanation); err != nil {
			return err
		}
		if in.ExplanationTruncated {
			if _, err := fmt.Fprintln(w, "(narrative truncated at the model's output limit)"); err != nil {
				return err
			}
		}
	}
	if in.Investigation != "" {
		if _, err := fmt.Fprintf(w, "\n── Investigation ──\n"); err != nil {
			return err
		}
		consulted := "(no reads — the model answered from the scan alone)"
		if len(in.InvestigationConsulted) > 0 {
			consulted = strings.Join(in.InvestigationConsulted, " · ")
		}
		if _, err := fmt.Fprintf(w, "consulted: %s\n", consulted); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "(model-generated; verify commands before running)\n"); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "%s\n", in.Investigation); err != nil {
			return err
		}
		if in.InvestigationTruncated {
			if _, err := fmt.Fprintln(w, "(narrative truncated at the model's output limit)"); err != nil {
				return err
			}
		}
	} else if in.InvestigationSkipped {
		// No consulted: line here — there was no investigation, so there is
		// no trail. The heading still renders so this reads as a section
		// like every other, not a bare floating sentence.
		if _, err := fmt.Fprintf(w, "\n── Investigation ──\nInvestigation skipped — no workload findings, no service findings, and the cluster verdict is not Degraded.\n"); err != nil {
			return err
		}
	}
	return nil
}

// splitServiceIssues separates real problems from expected-empty (annotated) ones.
func splitServiceIssues(issues []svchealth.Issue) (real, expected []svchealth.Issue) {
	for _, is := range issues {
		if is.Expected {
			expected = append(expected, is)
		} else {
			real = append(real, is)
		}
	}
	return real, expected
}

// splitIngressIssues separates real broken routes from expected-empty (parked) ones.
func splitIngressIssues(issues []ingresshealth.RouteIssue) (real, expected []ingresshealth.RouteIssue) {
	for _, r := range issues {
		if r.Expected {
			expected = append(expected, r)
		} else {
			real = append(real, r)
		}
	}
	return real, expected
}

// printHeader prints the cluster verdict line and, when anything is flagged, a
// workload-scoped attention line.
func printHeader(in Input, real []svchealth.Issue, realIng []ingresshealth.RouteIssue, w io.Writer) error {
	c := in.Cluster
	if c.Verdict == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Cluster: %s — %d/%d nodes Ready\n",
		c.Verdict, c.NodesReady, c.NodesTotal+c.NodesExpectedAbsent); err != nil {
		return err
	}
	for _, iss := range c.NodeIssues {
		if _, err := fmt.Fprintf(w, "  ✗ node %s\n", iss); err != nil {
			return err
		}
	}
	for _, iss := range c.SystemIssues {
		if _, err := fmt.Fprintf(w, "  ✗ system %s\n", iss); err != nil {
			return err
		}
	}
	if c.ScopeNote != "" {
		// ScopeNote may carry up to two sentences joined by "\n" (R156): the
		// system-rollup caveat and the admission-webhook caveat are about
		// different things and either can appear without the other, so each
		// renders on its own "  · " line rather than one line with an
		// embedded newline. This is kubeagent's own text, not API text —
		// line-wrapping, not parsing.
		for _, sentence := range strings.Split(c.ScopeNote, "\n") {
			if _, err := fmt.Fprintf(w, "  · %s\n", sentence); err != nil {
				return err
			}
		}
	}
	if line := attentionLine(in, real, realIng); line != "" {
		if _, err := fmt.Fprintf(w, "  Needs attention: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

// attentionLine summarizes flagged workloads, services without endpoints,
// volumes over the disk-usage threshold, and broken ingress routes.
func attentionLine(in Input, real []svchealth.Issue, realIng []ingresshealth.RouteIssue) string {
	failing := 0
	attributed := 0
	var causeNodes []string
	seenCause := map[string]bool{}
	for _, wl := range in.Result.Workloads {
		if wl.Flagged() {
			failing++
		}
		if wl.RootCause != "" {
			attributed++
			n := rootCauseNode(wl.RootCause)
			if !seenCause[n] {
				seenCause[n] = true
				causeNodes = append(causeNodes, n)
			}
		}
	}
	var parts []string
	if failing > 0 {
		s := fmt.Sprintf("%d %s failing", failing, plural(failing, "workload", "workloads"))
		if attributed > 0 {
			if len(causeNodes) == 1 {
				s += fmt.Sprintf(" (%d ⇐ %s)", attributed, causeNodes[0])
			} else {
				s += fmt.Sprintf(" (%d ⇐ %d root causes)", attributed, len(causeNodes))
			}
		}
		parts = append(parts, s)
	}
	if len(real) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s without endpoints", len(real), plural(len(real), "service", "services")))
	}
	if in.DiskUsage != nil && len(in.DiskUsage.Over) > 0 {
		n := len(in.DiskUsage.Over)
		parts = append(parts, fmt.Sprintf("%d %s low on disk", n, plural(n, "volume", "volumes")))
	}
	if n := len(realIng); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ingress %s broken", n, plural(n, "route", "routes")))
	}
	if n := len(in.PVCIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing to provision", n, plural(n, "PVC", "PVCs")))
	}
	if n := len(in.StuckTerminating); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s stuck terminating", n, plural(n, "resource", "resources")))
	}
	if n := len(in.PDBIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "PodDisruptionBudget issue", "PodDisruptionBudget issues")))
	}
	if n := len(in.HPAIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s can't scale", n, plural(n, "HPA", "HPAs")))
	}
	webhookFailing, webhookSlow := 0, 0
	for _, i := range in.WebhookIssues {
		if i.Problem == "HighTimeout" {
			webhookSlow++
		} else {
			webhookFailing++
		}
	}
	if webhookFailing > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing", webhookFailing, plural(webhookFailing, "admission webhook", "admission webhooks")))
	}
	if webhookSlow > 0 {
		parts = append(parts, fmt.Sprintf("%d slow %s", webhookSlow, plural(webhookSlow, "admission webhook", "admission webhooks")))
	}
	if n := len(in.QuotaIssues); n > 0 {
		// Entries, not objects: QuotaIssues holds one element per (quota, resource)
		// pair, so one ResourceQuota with three resources over the line counts three
		// — which is both the number of things to fix and the number of rows below.
		parts = append(parts, fmt.Sprintf("%d %s near/over quota", n, plural(n, "ResourceQuota entry", "ResourceQuota entries")))
	}
	return strings.Join(parts, " · ")
}

// rootCauseNode extracts the cause prefix (e.g. "node X" or "registry Y") from a
// RootCause string of the fixed form "<cause> (<detail>)" for the attention-line dedup rollup.
func rootCauseNode(rc string) string {
	return strings.SplitN(rc, " (", 2)[0]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printNotes renders the NOTES section: the advisory bullets that qualify the
// report rather than diagnose a workload — a condition worth knowing that is
// not a finding, a check that ran and flagged nothing, and what the inventory
// left out behind a flag. Each bullet is written by the block that owns its
// input, so the set grows with those blocks and this comment does not
// enumerate them. The header is emitted only if at least one bullet was.
func printNotes(in Input, expected []svchealth.Issue, expectedIng []ingresshealth.RouteIssue, w io.Writer) error {
	now := nowOr(in.Now)
	var b strings.Builder
	if n := in.NodeReserve; n != nil {
		if n.WarnCount > 0 {
			var names []string
			for _, r := range n.Nodes {
				if r.Warning {
					names = append(names, r.Name)
				}
			}
			fmt.Fprintf(&b, "  • %d %s no memory: %s\n", n.WarnCount, plural(n.WarnCount, "node reserves", "nodes reserve"), strings.Join(names, ", "))
			fmt.Fprintln(&b, "      — OS/kubelet memory pressure can destabilize the node")
		}
		if n.EphemeralNone > 0 {
			var names []string
			for _, r := range n.Nodes {
				if r.NoEphemeral {
					names = append(names, r.Name)
				}
			}
			fmt.Fprintf(&b, "  • %d %s no ephemeral-storage: %s\n", n.EphemeralNone, plural(n.EphemeralNone, "node reserves", "nodes reserve"), strings.Join(names, ", "))
			fmt.Fprintln(&b, "      — disk pressure can destabilize the node")
		}
	}
	if err := printPVCReclaim(in.PVCReclaim, in.PVCReclaimFull, &b); err != nil {
		return err
	}
	if err := printServiceIssues(expected, "  •", now, &b); err != nil {
		return err
	}
	if err := printIngressIssues(expectedIng, "  •", &b); err != nil {
		return err
	}
	if in.WebhookURLBackends > 0 {
		fmt.Fprintf(&b, "  • %d Fail-policy admission %s not checked: clientConfig.url backend\n",
			in.WebhookURLBackends, plural(in.WebhookURLBackends, "webhook", "webhooks"))
	}
	if hint := footerHint(in.Result); hint != "" {
		fmt.Fprintf(&b, "  • %s\n", hint)
	}
	// --certs ran (Certificates != nil) but found nothing to flag
	// (certificatesRender is false): confirm the check ran rather than stay
	// silent about it. A run with findings renders the CERTIFICATES section
	// instead, so this bullet and that section never both appear.
	if rep := in.Certificates; rep != nil && !certificatesRender(rep) {
		if rep.Checked == 0 {
			fmt.Fprintf(&b, "  • no TLS certificates found to check\n")
		} else {
			fmt.Fprintf(&b, "  • %d %s checked, none expired or expiring within %dd\n",
				rep.Checked, plural(rep.Checked, "certificate", "certificates"), rep.WarnDays)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "NOTES"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// printContext renders reference material: nodes/reservations, resources, platform.
func printContext(in Input, w io.Writer) error {
	var b strings.Builder
	if n := in.NodeReserve; n != nil && len(n.Nodes) > 0 {
		total := len(n.Nodes)
		fmt.Fprintln(&b, "Kubelet reservations (combined kube+system)")
		fmt.Fprintln(&b, reserveLine("memory", n.WarnCount, total, true))
		fmt.Fprintln(&b, reserveLine("cpu", n.CPUNone, total, false))
		if n.EphemeralReporting == 0 {
			fmt.Fprintf(&b, "  %-17s %s\n", "ephemeral-storage", "not reported")
		} else {
			line := reserveLine("ephemeral-storage", n.EphemeralNone, n.EphemeralReporting, true)
			if missing := total - n.EphemeralReporting; missing > 0 {
				line += fmt.Sprintf("  (%d %s not report it)", missing, plural(missing, "node does", "nodes do"))
			}
			fmt.Fprintln(&b, line)
		}
	}
	if err := printResources(in.Resources, &b); err != nil {
		return err
	}
	if in.Platform != nil {
		if line := in.Platform.Line(); line != "" {
			fmt.Fprintf(&b, "Platform: %s\n", line)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CONTEXT"); err != nil {
		return err
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// reserveLine formats one CONTEXT reservation line, padded so statuses align.
// warn=true appends ⚠ (some node reserves none) or ✓ (all reserve some) — used
// for memory and ephemeral-storage; cpu (warn=false) gets no glyph (informational).
func reserveLine(label string, none, reporting int, warn bool) string {
	var status string
	if none == 0 {
		status = fmt.Sprintf("all %d %s reserve some", reporting, plural(reporting, "node", "nodes"))
		if warn {
			status += "  ✓"
		}
	} else {
		status = fmt.Sprintf("%d of %d nodes reserve none", none, reporting)
		if warn {
			status += "  ⚠"
		}
	}
	return fmt.Sprintf("  %-17s %s", label, status)
}

func printResources(s *resources.Summary, w io.Writer) error {
	if s == nil {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Resources (cluster):"); err != nil {
		return err
	}
	if err := printResLine(w, "CPU   ", s.CPU, "cores", s.MetricsAvailable); err != nil {
		return err
	}
	if err := printResLine(w, "Memory", s.Memory, "", s.MetricsAvailable); err != nil {
		return err
	}
	if !s.MetricsAvailable {
		if _, err := fmt.Fprintln(w, "  (usage: metrics-server unavailable)"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

func printResLine(w io.Writer, label string, l resources.Line, unit string, metrics bool) error {
	alloc := l.Allocatable
	if unit != "" {
		alloc += " " + unit
	}
	line := fmt.Sprintf("  %s  %s · req %s (%d%%) · lim %s (%d%%)",
		label, alloc, l.Requests, l.RequestsPct, l.Limits, l.LimitsPct)
	if metrics {
		line += fmt.Sprintf(" · used %s (%d%%)", l.Usage, l.UsagePct)
	}
	_, err := fmt.Fprintln(w, line)
	return err
}

// printDiskUsage lists node filesystems and PVCs at or over the threshold.
func printDiskUsage(rep *diskusage.Report, w io.Writer) error {
	if rep == nil {
		return nil
	}
	for _, v := range rep.Over {
		// Floored, not rounded, for the same reason printQuotaIssues floors: the
		// word beside this number is "full", so 100% must mean at or over
		// capacity and a volume at 99.9% must not borrow it. The trailing
		// (used/capacity) cannot stand in for the percentage either — fmtBytes
		// rounds to whole Gi, so both figures can round together. The JSON
		// ratio carries the true value for anything that needs it.
		pct := int(v.Ratio * 100)
		var line string
		if v.Kind == "node" {
			line = fmt.Sprintf("  ✗ node %s  disk %d%% full (%s/%s)", v.Name, pct, fmtBytes(v.UsedBytes), fmtBytes(v.CapacityBytes))
		} else {
			line = fmt.Sprintf("  ✗ pvc %s/%s  %d%% full (%s/%s)", v.Namespace, v.Name, pct, fmtBytes(v.UsedBytes), fmtBytes(v.CapacityBytes))
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// fmtBytes renders a byte count in Gi/Mi (or B below 1Mi).
func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// printIngressIssues lists Ingress routes whose backend chain is broken (glyph "  ✗")
// or is intentionally empty (glyph "  •").
func printIngressIssues(issues []ingresshealth.RouteIssue, glyph string, w io.Writer) error {
	for _, r := range issues {
		line := fmt.Sprintf("%s ingress %s/%s", glyph, r.Namespace, r.Ingress)
		if route := r.Host + r.Path; route != "" {
			line += "  " + route
		}
		line += "  " + r.Detail
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// printPVCIssues lists PersistentVolumeClaims stuck Pending because provisioning failed.
func printPVCIssues(issues []pvchealth.Issue, w io.Writer) error {
	for _, iss := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  PersistentVolumeClaim  %s — %s\n", iss.Namespace, iss.Name, iss.Phase, iss.Detail); err != nil {
			return err
		}
	}
	return nil
}

// printStuckTerminating lists resources wedged in Terminating past the threshold.
func printStuckTerminating(issues []termhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		id := is.Name
		if is.Namespace != "" {
			id = is.Namespace + "/" + is.Name
		}
		grace := ""
		if is.PastGrace {
			grace = " (past grace)"
		}
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  Terminating %s%s\n", id, is.Kind, is.Age, grace); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ StuckTerminating: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}

// printPDBIssues lists PodDisruptionBudgets that will block a node drain.
func printPDBIssues(issues []pdbhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  PodDisruptionBudget  %s\n", is.Namespace, is.Name, is.Rule); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ PDBBlocked: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}

// printHPAIssues lists HorizontalPodAutoscalers that cannot scale as intended.
func printHPAIssues(issues []hpahealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  HorizontalPodAutoscaler  targets %s\n", is.Namespace, is.Name, is.Target); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ HPAStuck: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}

// printQuotaIssues lists ResourceQuota entries at or over the usage threshold.
func printQuotaIssues(issues []quotahealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  ResourceQuota  %s\n", is.Namespace, is.Quota, is.Resource); err != nil {
			return err
		}
		label := "QuotaNearLimit"
		if is.Severity == "exhausted" {
			label = "QuotaExhausted"
		}
		// Floored, not rounded: 100% is what this line says about a quota that is
		// at or over its limit, so a quota at 99.9% must not borrow the number.
		// The JSON ratio carries the true value for anything that needs it.
		pct := int(is.Ratio * 100)
		if _, err := fmt.Fprintf(w, "      ⚠ %s: used %s / hard %s (%d%%)\n", label, is.Used, is.Hard, pct); err != nil {
			return err
		}
	}
	return nil
}

// printBlindSpots names what the scan could not read. It prints nothing when the
// scan saw everything, so a clean run's output is unchanged. Reasons are rendered
// verbatim — unlike internal/htmlreport.safeReason, which classifies instead of
// quoting because that document is written to be forwarded; this one is not.
func printBlindSpots(blind []scan.ReadFailure, w io.Writer) error {
	if len(blind) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "BLIND SPOTS"); err != nil {
		return err
	}
	for _, b := range blind {
		if _, err := fmt.Fprintf(w, "  • %s: %s\n", b.Resource, b.Reason); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// printWebhookIssues lists admission webhooks that either already reject every
// intercepted request (missing or endpoint-less backend) or would block one for
// a long time if their backend were slow (high timeoutSeconds).
func printWebhookIssues(issues []webhookhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  webhook %s\n", is.Config, is.Kind, is.Webhook); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ %s: %s\n", is.Problem, is.Reason); err != nil {
			return err
		}
	}
	return nil
}

// printSecurityIssues renders the advisory SECURITY section. By default it is
// signal-first: a one-line tier summary, the baseline/kubeagent ("act-on-these")
// findings in full per workload (worst-first), and the near-universal restricted
// hardening gaps folded into a compact aggregate. verbose lists every finding
// per workload and omits the aggregate.
func printSecurityIssues(issues []secscan.Finding, verbose bool, w io.Writer) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "SECURITY  (advisory — does not affect the cluster verdict)"); err != nil {
		return err
	}

	// Tallies for the summary header and the restricted aggregate.
	var nBaseline, nExposed, nRestricted int
	allWorkloads := map[string]bool{}
	restrictedWorkloads := map[string]bool{}
	restrictedByCheck := map[string]int{}
	for _, f := range issues {
		wl := f.Namespace + "/" + f.Workload
		allWorkloads[wl] = true
		switch f.Profile {
		case "restricted":
			nRestricted++
			restrictedWorkloads[wl] = true
			restrictedByCheck[f.Check]++
		case "kubeagent":
			nExposed++
		default: // baseline
			nBaseline++
		}
	}

	// Summary header: non-zero tiers joined by " · ", then the workload count.
	var parts []string
	if nBaseline > 0 {
		parts = append(parts, fmt.Sprintf("%d baseline", nBaseline))
	}
	if nExposed > 0 {
		parts = append(parts, fmt.Sprintf("%d exposed %s", nExposed, plural(nExposed, "service", "services")))
	}
	if nRestricted > 0 {
		parts = append(parts, fmt.Sprintf("%d restricted hardening %s", nRestricted, plural(nRestricted, "gap", "gaps")))
	}
	parts = append(parts, fmt.Sprintf("%d %s", len(allWorkloads), plural(len(allWorkloads), "workload", "workloads")))
	if _, err := fmt.Fprintf(w, "  %s\n\n", strings.Join(parts, " · ")); err != nil {
		return err
	}

	// Group findings by workload, preserving Assess's per-workload finding order.
	type grp struct{ ns, name, kind string }
	var order []grp
	byGrp := map[grp][]secscan.Finding{}
	for _, f := range issues {
		g := grp{f.Namespace, f.Workload, f.Kind}
		if _, ok := byGrp[g]; !ok {
			order = append(order, g)
		}
		byGrp[g] = append(byGrp[g], f)
	}

	// Detail blocks. Default: only workloads with act-on-these (non-restricted)
	// findings, showing just those. Verbose: every workload, every finding.
	type block struct {
		g     grp
		shown []secscan.Finding
	}
	var blocks []block
	for _, g := range order {
		shown := byGrp[g]
		if !verbose {
			var act []secscan.Finding
			for _, f := range shown {
				if f.Profile != "restricted" {
					act = append(act, f)
				}
			}
			if len(act) == 0 {
				continue // restricted-only workload -> aggregate only
			}
			shown = act
		}
		blocks = append(blocks, block{g, shown})
	}
	// Worst-first: most shown findings, then namespace, then workload.
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if len(a.shown) != len(b.shown) {
			return len(a.shown) > len(b.shown)
		}
		if a.g.ns != b.g.ns {
			return a.g.ns < b.g.ns
		}
		return a.g.name < b.g.name
	})
	for _, b := range blocks {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  %s\n", b.g.ns, b.g.name, b.g.kind); err != nil {
			return err
		}
		for _, f := range b.shown {
			if _, err := fmt.Fprintf(w, "      [%s] %s — %s\n", f.Profile, f.Check, f.Detail); err != nil {
				return err
			}
		}
	}

	// Restricted aggregate (default only, when there are restricted findings).
	// The denominator is every workload in the section, not just the restricted
	// ones: "2 across 1 workload" reads as if restricted coverage were total,
	// when the section may also carry baseline/exposed workloads that are clean
	// on the restricted profile. "of M" names the actual population.
	if !verbose && nRestricted > 0 {
		if _, err := fmt.Fprintf(w, "\n  restricted (hardening gaps, near-universal): %d across %d of %d %s\n",
			nRestricted, len(restrictedWorkloads), len(allWorkloads), plural(len(allWorkloads), "workload", "workloads")); err != nil {
			return err
		}
		var checks []string
		for _, c := range []string{"RunAsRoot", "AllowPrivilegeEscalation", "CapabilitiesNotDropped"} {
			if restrictedByCheck[c] > 0 {
				checks = append(checks, fmt.Sprintf("%s ×%d", c, restrictedByCheck[c]))
			}
		}
		if _, err := fmt.Fprintf(w, "    %s\n", strings.Join(checks, " · ")); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "    → run with --security-verbose to list every finding per workload"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

// printPVCReclaim renders the Delete-reclaim PVCs: a grouped one-line summary by
// default, or the full per-PVC rows when full is true. Nothing prints when empty.
func printPVCReclaim(rep *pvcreclaim.Report, full bool, w io.Writer) error {
	if rep == nil || len(rep.PVCs) == 0 {
		return nil
	}
	if full {
		if _, err := fmt.Fprintf(w, "  • %d %s on Delete reclaim policy — deleting the claim destroys the volume\n",
			len(rep.PVCs), plural(len(rep.PVCs), "PVC", "PVCs")); err != nil {
			return err
		}
		for _, p := range rep.PVCs {
			line := fmt.Sprintf("      %s/%s  pv %s", p.Namespace, p.Name, p.PV)
			if p.StorageClass != "" {
				line += "  class " + p.StorageClass
			}
			if p.Capacity != "" {
				line += "  " + p.Capacity
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintf(w, "  • %d %s on Delete reclaim policy — %s   [--pvc-reclaim]\n",
		len(rep.PVCs), plural(len(rep.PVCs), "PVC", "PVCs"), groupByClass(rep.PVCs))
	return err
}

// groupByClass builds "classA ×N, classB ×M" ordered by count desc, then name.
func groupByClass(pvcs []pvcreclaim.PVCReclaim) string {
	counts := map[string]int{}
	var order []string
	for _, p := range pvcs {
		c := p.StorageClass
		if c == "" {
			c = "(no class)"
		}
		if _, seen := counts[c]; !seen {
			order = append(order, c)
		}
		counts[c]++
	}
	sort.SliceStable(order, func(i, j int) bool {
		if counts[order[i]] != counts[order[j]] {
			return counts[order[i]] > counts[order[j]]
		}
		return order[i] < order[j]
	})
	parts := make([]string, 0, len(order))
	for _, c := range order {
		parts = append(parts, fmt.Sprintf("%s ×%d", c, counts[c]))
	}
	return strings.Join(parts, ", ")
}

// footerHint summarizes hidden categories, naming the flag that reveals each.
func footerHint(result inventory.Result) string {
	var parts []string
	if result.HiddenRestarts > 0 {
		parts = append(parts, fmt.Sprintf("+%d restarted workloads (--include-restarts)", result.HiddenRestarts))
	}
	if result.HiddenCron > 0 {
		parts = append(parts, fmt.Sprintf("+%d CronJobs (--include-cron)", result.HiddenCron))
	}
	return strings.Join(parts, " · ")
}

func printServiceIssues(issues []svchealth.Issue, glyph string, now time.Time, w io.Writer) error {
	for _, is := range issues {
		line := fmt.Sprintf("%s %s/%s  %s  %s", glyph, is.Namespace, is.Name, is.Type, is.Detail)
		if is.Since != "" {
			line += " · " + inventory.HumanSince(is.Since, now)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func printCredentialWarnings(findings []credlint.Finding, w io.Writer) error {
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  %s[%s]  %s\n", f.Namespace, f.Name, f.Kind, f.Location, f.Pattern); err != nil {
			return err
		}
	}
	return nil
}

// findingGroup is one or more findings that render to the same block, kept in
// the order the first of them appeared.
type findingGroup struct {
	head     string   // the "⚠ …" line, without its count suffix
	evidence []string // the distinct "↳ …" lines, first-seen order
	tail     string   // everything below the evidence: resources, logs, suggestions
	count    int      // how many findings this block stands for
}

// groupFindings collapses findings that would render alike into one block
// carrying a count.
//
// A Deployment whose replicas all crash the same way produced one identical
// pair of lines per pod — twenty of them on a twenty-replica Deployment — and
// the text renderer prints no pod name, so there was nothing to tell them
// apart with. Only the text output collapses: the JSON document is the one
// that gets forwarded, and it still carries one finding per pod.
//
// The key is the whole rendered block bar the evidence line. Everything that
// varies per pod and is not evidence keeps the findings apart: a --suggest
// command names the pod, a resources block names the container's limits, a log
// excerpt is that pod's own. Evidence is the one part allowed to vary inside a
// group, and every distinct value is printed — which is what shows the restart
// counts when they differ, since that is where a restart count lives. A group
// prints its head once, one line per distinct evidence value, and its shared
// tail once — the resources block, the log excerpt, the suggestions. Whenever
// it stands for more than one finding, the block is shorter than rendering
// those findings separately, because the head and the tail go out once
// instead of once per finding. Against the finding *count* there is no fixed
// relation: a pair that agrees prints two lines, equal to the count; a pair
// carrying a resources block prints three, more than the count; and six that
// agree with no tail print two lines, fewer than the count. count is what
// says how many findings stand behind the block.
func groupFindings(findings []diagnose.Finding, suggest bool) []findingGroup {
	var groups []findingGroup
	at := map[string]int{} // block key -> index into groups
	for _, f := range findings {
		head, evidence, tail := renderFinding(f, suggest)
		key := head + "\x00" + tail
		i, ok := at[key]
		if !ok {
			at[key] = len(groups)
			groups = append(groups, findingGroup{head: head, tail: tail})
			i = len(groups) - 1
		}
		g := &groups[i]
		g.count++
		if evidence != "" && !hasLine(g.evidence, evidence) {
			g.evidence = append(g.evidence, evidence)
		}
	}
	return groups
}

// hasLine reports whether lines already holds s. A group's evidence list is
// bounded by the number of findings on one workload, so a scan is enough.
func hasLine(lines []string, s string) bool {
	for _, l := range lines {
		if l == s {
			return true
		}
	}
	return false
}

const (
	// maxEvidence is the rune budget for the evidence string on one rendered
	// evidence line. The line itself is eight runes longer — renderFinding
	// prepends the indent and the "↳ " marker — so a cut line measures 508.
	//
	// A container runtime repeats every layer of a failure — the back-off
	// preamble, the rpc error, the unpack failure, the resolve failure, the bare
	// reference — and on a long registry path that composed line runs past the
	// screen. Real ones measure a few hundred characters and are the only place
	// the true cause appears, so the budget is set where it keeps them whole and
	// bites only on the pathological.
	//
	// This is not safetext.MaxLine restated. That budget bounds each hostile
	// value at the moment it enters kubeagent; this one bounds the line a
	// detector composed, which may join several already-sanitized values and
	// several of kubeagent's own words. Different units, so neither implies the
	// other.
	maxEvidence = 500

	// evidenceCut ends a line the budget cut. A silently shortened error reads
	// as the whole error, which is a claim the output would not be keeping.
	evidenceCut = "… (truncated)"
)

// capEvidence fits s inside maxEvidence runes, marking the cut when it makes
// one. Runes, not bytes, so a multi-byte character is never split.
//
// Text only: --output json carries the evidence as stored, without this cap.
// That is not the same as carrying the whole thing — every untrusted API value
// is bounded at safetext.MaxLine runes on the way in, so a longer message is
// already short by the time it reaches here. This cap is a terminal-layout
// decision layered on that bound; the finding's own reason is untouched.
func capEvidence(s string) string {
	if len(s) <= maxEvidence { // bytes >= runes, so most lines end here
		return s
	}
	r := []rune(s)
	if len(r) <= maxEvidence {
		return s
	}
	return string(r[:maxEvidence-utf8.RuneCountInString(evidenceCut)]) + evidenceCut
}

// renderFinding splits one finding's block into the three parts groupFindings
// keys on. Concatenating head+"\n", evidence+"\n" and tail is exactly what the
// renderer emitted before findings were grouped.
func renderFinding(f diagnose.Finding, suggest bool) (head, evidence, tail string) {
	tag := ""
	if f.Confidence != "" && f.Confidence != "high" {
		tag = " [" + f.Confidence + "]"
	}
	head = fmt.Sprintf("    ⚠ %s%s: %s", f.Issue, tag, f.Reason)
	if f.Evidence != "" && f.Evidence != f.Reason {
		evidence = "      ↳ " + capEvidence(f.Evidence)
	}
	var b strings.Builder
	if f.Resources != nil {
		r := f.Resources
		b.WriteString(fmt.Sprintf("      resources: memory req=%s limit=%s · cpu req=%s limit=%s\n",
			r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit))
	}
	if f.LogExcerpt != "" {
		b.WriteString(fmt.Sprintf("      logs (previous container):\n        %s\n        → %s\n", f.LogExcerpt, f.LogCause))
	}
	if suggest {
		s := remediation.For(f)
		if s.NextStep != "" {
			b.WriteString(fmt.Sprintf("      ↳ next step: %s\n", s.NextStep))
		}
		if s.Command != "" {
			b.WriteString(fmt.Sprintf("      ↳ try: %s\n", s.Command))
		}
	}
	return head, evidence, b.String()
}

func printWorkload(wl inventory.Workload, now time.Time, suggest bool, w io.Writer) error {
	flag := "  "
	if wl.Flagged() {
		flag = "✗ "
	}
	var header string
	if wl.Kind == "Job" || wl.Kind == "CronJob" {
		header = fmt.Sprintf("%s%s/%s  %s  %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Status)
		if wl.Schedule != "" {
			header += "  (" + wl.Schedule + ")"
		}
	} else {
		header = fmt.Sprintf("%s%s/%s  %s  %d/%d %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Ready, wl.Desired, wl.Status)
	}
	if wl.Restarts > 0 {
		header += fmt.Sprintf("  · %d restarts", wl.Restarts)
		if wl.LastRestart != "" {
			header += fmt.Sprintf(", last %s", inventory.HumanSince(wl.LastRestart, now))
		}
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if wl.RootCause != "" {
		rcTag := ""
		if c := confidence.ForRootCause(wl.RootCause); c != "" && c != "high" {
			rcTag = " [" + c + "]"
		}
		if _, err := fmt.Fprintf(w, "    ↳ likely caused by %s%s\n", wl.RootCause, rcTag); err != nil {
			return err
		}
	}
	if wl.Image != "" {
		if _, err := fmt.Fprintf(w, "    image %s\n", wl.Image); err != nil {
			return err
		}
	}
	for _, g := range groupFindings(wl.Findings, suggest) {
		head := g.head
		if g.count > 1 {
			head += fmt.Sprintf(" ×%d", g.count)
		}
		if _, err := fmt.Fprintln(w, head); err != nil {
			return err
		}
		for _, e := range g.evidence {
			if _, err := fmt.Fprintln(w, e); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, g.tail); err != nil {
			return err
		}
	}
	if len(wl.NetworkPolicies) > 0 {
		if _, err := fmt.Fprintf(w, "    ⚠ NetworkPolicy: pods selected by %s — may be blocking traffic\n", strings.Join(wl.NetworkPolicies, ", ")); err != nil {
			return err
		}
	}
	if wl.Rollout != nil {
		line := fmt.Sprintf("    ↳ changed: rollout to revision %s, %s", wl.Rollout.Revision, wl.Rollout.Since)
		if wl.Rollout.NewImage != "" {
			line += fmt.Sprintf(" · image %s → %s", wl.Rollout.OldImage, wl.Rollout.NewImage)
			if wl.Rollout.Container != "" {
				line += fmt.Sprintf(" (container %q)", wl.Rollout.Container)
			}
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	for _, p := range wl.Pods {
		restarts := fmt.Sprintf("%d", p.Restarts)
		if p.LastRestart != "" {
			restarts += " (" + inventory.HumanSince(p.LastRestart, now) + ")"
		}
		// State, not Phase: a row printing status.phase reads "Running" for a pod
		// kubectl calls CrashLoopBackOff, and an operator holding both outputs has
		// to work out which tool is wrong. inventory.PodRowFor computes State from
		// the containers and leaves Phase carrying the raw phase for any consumer
		// of the JSON that wants it.
		if _, err := fmt.Fprintf(w, "    %s  %s  %s  restarts=%s  %s  %s  %s\n",
			p.Name, p.Ready, p.State, restarts, orDash(p.Node), orDash(p.IP), p.Age); err != nil {
			return err
		}
	}
	if wl.PodsOmitted > 0 {
		if _, err := fmt.Fprintf(w, "    +%d more pods\n", wl.PodsOmitted); err != nil {
			return err
		}
	}
	return nil
}

// orDash renders an empty pod-row cell as a placeholder. A pod that has not
// been scheduled has no node and no IP; without a placeholder those two cells
// collapse into a run of spaces and the age reads as if it were the node.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// controlPlaneRenders reports whether the CONTROL PLANE section would print.
func controlPlaneRenders(p *controlplane.Probe) bool {
	return p != nil && (p.Status == "unhealthy" || p.Status == "forbidden")
}

// printControlPlane renders the advisory CONTROL PLANE section: the apiserver
// /readyz probe result when it is not ready (or a grant hint when forbidden).
func printControlPlane(p *controlplane.Probe, w io.Writer) error {
	if !controlPlaneRenders(p) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CONTROL PLANE  (opt-in)"); err != nil {
		return err
	}
	switch p.Status {
	case "unhealthy":
		if _, err := fmt.Fprintln(w, "  ✗ control plane not ready"); err != nil {
			return err
		}
		if len(p.Failed) > 0 {
			switch {
			case len(p.Failed) > controlplane.MaxFailedChecks:
				if _, err := fmt.Fprintf(w, "      ⚠ more than %d checks failing: %s, …\n",
					controlplane.MaxFailedChecks, strings.Join(p.Failed[:controlplane.MaxFailedChecks], ", ")); err != nil {
					return err
				}
			default:
				if _, err := fmt.Fprintf(w, "      ⚠ %d %s failing: %s\n",
					len(p.Failed), plural(len(p.Failed), "check", "checks"), strings.Join(p.Failed, ", ")); err != nil {
					return err
				}
			}
		} else {
			if _, err := fmt.Fprintln(w, "      ⚠ apiserver /readyz reported not ready"); err != nil {
				return err
			}
		}
	case "forbidden":
		if _, err := fmt.Fprintln(w, "  ⚠ /readyz forbidden — grant nonResourceURLs /readyz to enable this check"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// dnsRenders reports whether the DNS section would print.
func dnsRenders(p *dnshealth.Report) bool {
	return p != nil && (p.Status == "degraded" || p.Status == "forbidden")
}

// printDNSHealth renders the advisory DNS section: an elevated CoreDNS SERVFAIL+
// REFUSED response ratio (or a grant hint when forbidden).
func printDNSHealth(p *dnshealth.Report, w io.Writer) error {
	if !dnsRenders(p) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "DNS  (opt-in)"); err != nil {
		return err
	}
	switch p.Status {
	case "degraded":
		if _, err := fmt.Fprintln(w, "  ✗ cluster DNS is failing to resolve"); err != nil {
			return err
		}
		pct := float64(int64(p.ServfailRatio*1000+0.5)) / 10
		podsText := fmt.Sprintf("%d pods", p.PodsProbed)
		if p.PodsAnswered != p.PodsProbed {
			podsText = fmt.Sprintf("%d of %d pods", p.PodsAnswered, p.PodsProbed)
		}
		if _, err := fmt.Fprintf(w, "      ⚠ CoreDNS SERVFAIL+REFUSED ratio %.1f%% (%d/%d responses across %s)\n",
			pct, p.ErrorResponses, p.TotalResponses, podsText); err != nil {
			return err
		}
	case "forbidden":
		if _, err := fmt.Fprintln(w, "  ⚠ CoreDNS /metrics forbidden — grant pods/proxy to enable this check"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// certificatesRender reports whether the CERTIFICATES section would print
// anything: expired/expiring/invalid certs, or the missing-grant hint.
func certificatesRender(rep *certhealth.Report) bool {
	if rep == nil {
		return false
	}
	return rep.Forbidden || len(rep.Expired) > 0 || len(rep.Expiring) > 0 || len(rep.Invalid) > 0
}

// printCertificates renders the advisory CERTIFICATES section (opt-in --certs).
func printCertificates(rep *certhealth.Report, w io.Writer) error {
	if !certificatesRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CERTIFICATES  (advisory — public certificate metadata only)"); err != nil {
		return err
	}
	if rep.Forbidden {
		if _, err := fmt.Fprintln(w, "  certificates: secrets access denied — apply deploy/rbac-certs.yaml (or Helm certs.enabled=true)"); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w)
		return err
	}
	for _, c := range rep.Expired {
		if _, err := fmt.Fprintf(w, "  ✗ %s/%s  EXPIRED %dd ago  (CN %s)\n", c.Namespace, c.Name, -c.Days, c.CommonName); err != nil {
			return err
		}
		for _, ing := range c.Ingresses {
			if _, err := fmt.Fprintf(w, "      — fronts ingress %s\n", ing); err != nil {
				return err
			}
		}
	}
	for _, c := range rep.Expiring {
		if _, err := fmt.Fprintf(w, "  ⚠ %s/%s  expires in %dd  (CN %s)\n", c.Namespace, c.Name, c.Days, c.CommonName); err != nil {
			return err
		}
		for _, ing := range c.Ingresses {
			if _, err := fmt.Fprintf(w, "      — fronts ingress %s\n", ing); err != nil {
				return err
			}
		}
	}
	for _, iv := range rep.Invalid {
		if _, err := fmt.Fprintf(w, "  ⚠ %s/%s  %s\n", iv.Namespace, iv.Name, iv.Detail); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "  · %d certificates checked (warn window %dd)\n", rep.Checked, rep.WarnDays); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// operatorsRender reports whether the OPERATORS section would print anything.
func operatorsRender(rep *operators.Report) bool {
	return rep != nil && len(rep.Operators) > 0
}

// printOperators renders the advisory OPERATORS section (opt-in --operators):
// one line per operator, one per resource kind, and the unhealthy resources a
// kind enumerates. Metadata and state only — no CR spec content ever reaches it.
func printOperators(rep *operators.Report, w io.Writer) error {
	if !operatorsRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "OPERATORS  (advisory — operator-reported state; no CR spec content)"); err != nil {
		return err
	}
	for _, op := range rep.Operators {
		line := "  " + op.Operator
		if len(op.APIVersions) > 0 {
			line += " (" + strings.Join(op.APIVersions, ", ") + ")"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		for _, k := range op.Kinds {
			switch {
			case k.Forbidden:
				if _, err := fmt.Fprintf(w, "    %-16slist forbidden — apply deploy/rbac-operators.yaml\n", k.Kind); err != nil {
					return err
				}
				continue
			case k.Error != "":
				if _, err := fmt.Fprintf(w, "    %-16slist failed: %s\n", k.Kind, k.Error); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "    %-16s%s\n", k.Kind, kindSummary(k)); err != nil {
				return err
			}
			for _, r := range k.Unhealthy {
				name := r.Name
				if r.Namespace != "" {
					name = r.Namespace + "/" + r.Name
				}
				line := "      ✗ " + name
				if r.Reason != "" {
					line += "  " + r.Reason
				}
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			if k.Truncated > 0 {
				if _, err := fmt.Fprintf(w, "      … +%d more unhealthy\n", k.Truncated); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// kindSummary renders one kind's counts in a fixed state order, omitting zeros.
// An adapter with no rule counts its resources without judging them, so its
// summary says so rather than implying an assessment happened.
func kindSummary(k operators.KindReport) string {
	if !k.Judged {
		return fmt.Sprintf("%d (not assessed)", k.Total())
	}
	order := []operators.State{
		operators.StateHealthy, operators.StateProgressing,
		operators.StateUnhealthy, operators.StateSuspended, operators.StateUnknown,
	}
	var parts []string
	for _, s := range order {
		if n := k.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}

// gitopsRender reports whether the GITOPS DRIFT section would print anything.
func gitopsRender(rep *gitops.Report) bool {
	return rep != nil && len(rep.Reconcilers) > 0
}

// printGitOps renders the advisory GITOPS DRIFT section (opt-in --drift): one
// line per reconciler, one per kind, and the objects that are not converged.
//
// Advisory, exactly like OPERATORS: it never sets hasAttention, never changes the
// cluster verdict, and takes no part in the all-clear suppression. Metadata and
// state only — no CR spec content, no condition message, and no revision that has
// not been through gitops.ShortRevision.
func printGitOps(rep *gitops.Report, w io.Writer) error {
	if !gitopsRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintf(w,
		"GITOPS DRIFT  (advisory — reconciler-reported; threshold %s; no repo URLs)\n",
		rep.Threshold); err != nil {
		return err
	}
	for _, rc := range rep.Reconcilers {
		line := "  " + rc.Reconciler
		if len(rc.APIVersions) > 0 {
			line += " (" + strings.Join(rc.APIVersions, ", ") + ")"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		for _, k := range rc.Kinds {
			switch {
			case k.Forbidden:
				if _, err := fmt.Fprintf(w, "    %-16slist forbidden — apply deploy/rbac-gitops.yaml\n", k.Kind); err != nil {
					return err
				}
				continue
			case k.Error != "":
				if _, err := fmt.Fprintf(w, "    %-16slist failed: %s\n", k.Kind, k.Error); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "    %-16s%s\n", k.Kind, driftSummary(k)); err != nil {
				return err
			}
			for _, d := range k.Drifted {
				name := d.Name
				if d.Namespace != "" {
					name = d.Namespace + "/" + d.Name
				}
				line := "      " + driftMarker(d.State) + " " + name
				if d.Detail != "" {
					line += "  " + d.Detail
				}
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			if k.Truncated > 0 {
				if _, err := fmt.Fprintf(w, "      … +%d more\n", k.Truncated); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// driftMarker separates "a human must act" from "it is still converging". A
// suspended object is blocked and so carries ✗: the suspension may well be
// deliberate, but reconciliation has stopped either way.
func driftMarker(s gitops.State) string {
	switch s {
	case gitops.StateStale, gitops.StateBlocked:
		return "✗"
	default:
		return "·"
	}
}

// driftSummary renders one kind's counts in a fixed state order, omitting zeros.
// The order is a literal slice, never a range over the counts map, which would
// print differently on every run.
func driftSummary(k gitops.KindReport) string {
	order := []gitops.State{
		gitops.StateSynced, gitops.StatePending,
		gitops.StateStale, gitops.StateBlocked, gitops.StateUnknown,
	}
	var parts []string
	for _, s := range order {
		if n := k.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}

// kubeletHealthRenders reports whether the KUBELET HEALTH section would print
// anything: unhealthy nodes, or the missing-grant hint (every probe forbidden).
func kubeletHealthRenders(rep *nodehealth.Report) bool {
	if rep == nil {
		return false
	}
	return len(rep.Unhealthy) > 0 || (rep.Probed > 0 && rep.Forbidden == rep.Probed)
}

// printKubeletHealth renders the advisory KUBELET HEALTH section: nodes whose
// kubelet /healthz reported unhealthy, or a hint when the nodes/proxy grant is missing.
func printKubeletHealth(rep *nodehealth.Report, w io.Writer) error {
	if !kubeletHealthRenders(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "KUBELET HEALTH  (opt-in)"); err != nil {
		return err
	}
	if rep.Probed > 0 && rep.Forbidden == rep.Probed {
		if _, err := fmt.Fprintln(w, "  kubelet-health needs the nodes/proxy add-on (deploy/rbac-diskusage.yaml or Helm kubeletHealth.enabled=true)"); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w)
		return err
	}
	for _, iss := range rep.Unhealthy {
		line := fmt.Sprintf("  ✗ node %s kubelet /healthz unhealthy", iss.Node)
		if iss.Detail != "" {
			line += ": " + iss.Detail
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// printCapacity renders the advisory CAPACITY section (opt-in --capacity): the
// headroom arithmetic over schedulable nodes, then the structural right-sizing
// rules.
//
// Advisory, exactly like OPERATORS and GITOPS DRIFT: it never sets hasAttention,
// never changes the cluster verdict, and takes no part in the all-clear
// suppression. Two claims it must never make — money, which no cluster publishes,
// and a peak, which a single metrics-server sample cannot establish.
func printCapacity(rep *capacity.Report, w io.Writer) error {
	if rep == nil || (rep.Headroom == nil && rep.RightSizing == nil) {
		return nil
	}
	if _, err := fmt.Fprint(w,
		"CAPACITY  (advisory — resource arithmetic on requests; ignores affinity,\n"+
			"           topology spread, PVC zoning, and PodDisruptionBudgets)\n"); err != nil {
		return err
	}
	if err := printHeadroomBlock(rep.Headroom, w); err != nil {
		return err
	}
	if err := printRightSizingBlock(rep.RightSizing, w); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// printPolicy renders the operator's own checks. Unlike the advisory sections
// it prints even when it found nothing: the operator asked for these rules by
// name, and silence would be indistinguishable from the flag not working.
func printPolicy(v *PolicyView, w io.Writer) error {
	if v == nil {
		return nil
	}

	var critical, warning, info int
	for _, vi := range v.Violations {
		switch vi.Level {
		case policy.LevelCritical:
			critical++
		case policy.LevelWarning:
			warning++
		case policy.LevelInfo:
			info++
		}
	}

	verdict := "no violations"
	if len(v.Violations) > 0 {
		parts := make([]string, 0, 3)
		if critical > 0 {
			parts = append(parts, fmt.Sprintf("%d critical", critical))
		}
		if warning > 0 {
			parts = append(parts, fmt.Sprintf("%d warning", warning))
		}
		if info > 0 {
			parts = append(parts, fmt.Sprintf("%d info", info))
		}
		verdict = strings.Join(parts, ", ")
	}
	if _, err := fmt.Fprintf(w, "POLICY  (%d %s, %s)\n",
		v.Rules, plural(v.Rules, "rule", "rules"), verdict); err != nil {
		return err
	}

	for _, vi := range v.Violations {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  %s\n", vi.Level, vi.RuleID, policyTarget(vi)); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      %s\n", vi.Message); err != nil {
			return err
		}
		if vi.Evidence != "" {
			if _, err := fmt.Fprintf(w, "      value: %s\n", vi.Evidence); err != nil {
				return err
			}
		}
	}
	for _, u := range v.NotEvaluated {
		if _, err := fmt.Fprintf(w, "  ⚠ not evaluated  %s  %s\n", u.RuleID, u.Kind); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      %s\n", u.Reason); err != nil {
			return err
		}
	}

	_, err := fmt.Fprintln(w)
	return err
}

// printBaseline renders the restart-rate comparison. Like printPolicy it prints
// even when it found nothing: the operator passed --baseline by name, and
// silence would be indistinguishable from the flag not working.
//
// The heading states the section's confidence in internal/confidence's
// vocabulary. A learned rate is an inference, not a detector match on a named
// failure mode, and internal/confidence is explicit that such a signal is
// informational only — it never affects priority and it never affects the
// cluster verdict, which this section does not touch.
func printBaseline(r *baseline.Report, w io.Writer) error {
	if r == nil {
		return nil
	}
	if _, err := fmt.Fprint(w, "BASELINE DEVIATIONS  (confidence: medium — a learned rate, not a detector)\n\n"); err != nil {
		return err
	}
	if len(r.Deviations) == 0 {
		if _, err := fmt.Fprintln(w, "  none"); err != nil {
			return err
		}
	}
	for _, d := range r.Deviations {
		target := fmt.Sprintf("%s %s/%s", d.Kind, d.Namespace, d.Name)
		if _, err := fmt.Fprintf(w, "  %-28s %.2f → %.2f restarts/hour   (%s)\n",
			target, d.BaselineRate, d.CurrentRate, deviationDetail(d)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\n  %d %s compared, %d not in the baseline, %d no longer present.\n",
		r.Compared, plural(r.Compared, "workload", "workloads"), r.NotInBaseline, r.GoneFromCluster); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// deviationDetail describes the size of the change. A zero baseline has no
// multiple — "0x baseline" is not a number anyone can act on — so it reports
// only how many pods are behind the current rate.
func deviationDetail(d baseline.Deviation) string {
	pods := fmt.Sprintf("%d %s", d.Pods, plural(d.Pods, "pod", "pods"))
	if d.BaselineRate <= 0 {
		return pods
	}
	return fmt.Sprintf("%.0fx baseline, %s", d.CurrentRate/d.BaselineRate, pods)
}

// policyTarget names the offending object. A cluster-scoped kind has no
// namespace, so it gets no separator rather than an empty one.
func policyTarget(v policy.Violation) string {
	if v.Namespace == "" {
		return v.Kind + " " + v.Name
	}
	return v.Kind + " " + v.Namespace + "/" + v.Name
}

func printHeadroomBlock(h *capacity.Headroom, w io.Writer) error {
	if h == nil {
		return nil
	}
	if _, err := fmt.Fprintln(w, "  Headroom"); err != nil {
		return err
	}
	// Zero included nodes: buildHeadroom leaves every arithmetic field at its nil/zero
	// default in that case, and the spec says print the exclusion list and no rows —
	// "0 free across 0 of N nodes" is an arithmetic statement about an empty set, not
	// a headroom figure.
	if h.IncludedNodes > 0 {
		if err := capacityRow(w, "schedulable", fmt.Sprintf("%s cores, %s free across %d of %d nodes",
			h.FreeCPU, h.FreeMemory, h.IncludedNodes, h.TotalNodes)); err != nil {
			return err
		}
	}
	if h.LargestCPUFit != nil {
		if err := capacityRow(w, "largest pod fit", fmt.Sprintf("%s  %s cores, %s",
			h.LargestCPUFit.Node, h.LargestCPUFit.CPU, h.LargestCPUFit.Memory)); err != nil {
			return err
		}
	}
	if h.LargestMemFit != nil {
		if err := capacityRow(w, "", fmt.Sprintf("%s  %s cores, %s",
			h.LargestMemFit.Node, h.LargestMemFit.CPU, h.LargestMemFit.Memory)); err != nil {
			return err
		}
	}
	if h.TightestNode != nil {
		if err := capacityRow(w, "tightest node", fmt.Sprintf("%s  %d%% of %s requested",
			h.TightestNode.Node, h.TightestNode.Pct, h.TightestNode.Resource)); err != nil {
			return err
		}
	}
	if nl := h.NodeLoss; nl != nil {
		if err := capacityRow(w, "lose "+nl.Node, nodeLossDetail(*nl)); err != nil {
			return err
		}
	}
	for i, e := range h.Excluded {
		label := ""
		if i == 0 {
			label = "excluded"
		}
		if err := capacityRow(w, label, fmt.Sprintf("%s  (%s)", e.Node, e.Reason)); err != nil {
			return err
		}
	}
	return nil
}

// nodeLossDetail respects the one-sided soundness of first-fit-decreasing: a
// successful pass is a constructive placement and so proves the requests fit, while
// a failed pass proves nothing — hence "may not fit", never "does not fit".
func nodeLossDetail(nl capacity.NodeLoss) string {
	switch {
	case nl.SingleNode:
		return "single node — no node-loss arithmetic possible"
	case nl.Fits:
		return fmt.Sprintf("fits — first-fit placed all %d pods", nl.Placed)
	default:
		return fmt.Sprintf("may not fit — first-fit could not place %s (%s cores)",
			nl.Blocker, nl.BlockerCPU)
	}
}

func printRightSizingBlock(rs *capacity.RightSizing, w io.Writer) error {
	if rs == nil || len(rs.Rules) == 0 {
		return nil
	}
	header := "  Right-sizing"
	if rs.MetricsAvailable {
		header += fmt.Sprintf("  (metrics-server: %d of %d pods reporting)",
			rs.PodsReporting, rs.PodsTotal)
	} else {
		header += "  (metrics-server unavailable — structural rules only)"
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	for _, r := range rs.Rules {
		for i, o := range r.Owners {
			label := ""
			if i == 0 {
				label = capacityRuleLabel(r.Name)
			}
			value := fmt.Sprintf("%s/%s/%s", o.Kind, o.Namespace, o.Name)
			if o.Detail != "" {
				value += "  " + o.Detail
			}
			if o.Observed != "" {
				value += "  · " + o.Observed + " observed"
			}
			if err := capacityRow(w, label, value); err != nil {
				return err
			}
			if o.BestEffort {
				if _, err := fmt.Fprintln(w,
					strings.Repeat(" ", capacityValueColumn+2)+"— BestEffort: first evicted under pressure"); err != nil {
					return err
				}
			}
		}
		if r.Truncated > 0 {
			if err := capacityRow(w, "", fmt.Sprintf("… +%d more", r.Truncated)); err != nil {
				return err
			}
		}
	}
	if rs.MetricsAvailable {
		if _, err := fmt.Fprintf(w,
			"\n    one sample per pod, ~30s average — not a peak, not a history\n"); err != nil {
			return err
		}
	}
	return nil
}

// capacityRuleLabel maps a rule constant to its human label. The constants are
// stable JSON keys; these strings are presentation only.
func capacityRuleLabel(n capacity.RuleName) string {
	switch n {
	case capacity.RuleNoRequests:
		return "no requests set"
	case capacity.RuleLimitNoRequest:
		return "limit, no request"
	case capacity.RuleNeverSchedulable:
		return "never schedulable"
	default:
		return string(n)
	}
}

// capacityLabelWidth is the label column's fixed width in capacityRow. The
// longest labels the section emits — "limit, no request" and "never
// schedulable" — are both 17 characters. 19 keeps both clear of the
// two-space floor below, so every row's value lands in the same column;
// pick a narrower width and one of those two labels trips the floor and
// pushes its value one column right of everything else.
const capacityLabelWidth = 19

// capacityIndent is the section's fixed left margin, in front of the label
// column.
const capacityIndent = 4

// capacityValueColumn is the column the value starts in, derived from the
// indent and label width so it never drifts out of sync with capacityRow.
const capacityValueColumn = capacityIndent + capacityLabelWidth

// capacityRow prints one label/value line at the section's fixed indent. An empty
// label produces a continuation line aligned under the previous value. Labels wider
// than the column still get two separating spaces rather than running together.
func capacityRow(w io.Writer, label, value string) error {
	pad := capacityLabelWidth - len(label)
	if pad < 2 {
		pad = 2
	}
	_, err := fmt.Fprintf(w, "    %s%s%s\n", label, strings.Repeat(" ", pad), value)
	return err
}
