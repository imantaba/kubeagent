// Package htmlreport renders a scan result as one self-contained HTML document:
// the artifact you attach to an incident ticket, paste into a pull request, or
// mail to a colleague who has no cluster access.
//
// The document is deliberately inert. It carries no <script>, no external
// stylesheet, font, or image, so it opens offline and survives a strict
// Content-Security-Policy — the environments that show these files (artifact
// previews, sandboxed mail clients, corporate proxies) block inline script and
// remote fetches, and none of them block inline CSS.
//
// It also carries no cluster identity: no context name, no API server URL, no
// kubeconfig path. Context names in the wild embed account IDs and internal
// hostnames, and this file is meant to be forwarded. Whoever shares it names the
// cluster in the ticket.
package htmlreport

import (
	_ "embed"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Input is everything the document renders. It is a struct rather than a
// parameter list because the fields arrive from different layers: Report is the
// presentation view main already builds for text and JSON, Findings comes from
// internal/findings, and Blind comes straight off the scan.Result.
type Input struct {
	// Report is the same value --output text and --output json render from.
	Report report.Input
	// Findings is severity-ranked by findings.Flatten, which already ends in
	// findings.Sort. Render preserves that order and never re-sorts: a second
	// definition of the same order would be free to drift from the first.
	Findings []findings.Finding
	// Blind is scan.Result.PartialReads — what kubeagent could not read. It is
	// its own field because report.Input has none, and a shared document that
	// silently omits its blind spots is the same green-when-blind failure the
	// CI gate exists to prevent.
	Blind []scan.ReadFailure
	// Namespace is the -n value; "" means all namespaces. report.Input carries
	// no namespace, and ClusterHealth.ScopeNote is not a substitute: it names no
	// namespace, and it is empty only for a cluster-wide scan — even -n
	// kube-system carries a caveat (the admission-webhook check is skipped
	// under any -n, unconditionally).
	Namespace string
	// Version is the kubeagent version stamped into the header.
	Version string
}

//go:embed report.html.tmpl
var templateSource string

// tmpl is parsed once at package init so a malformed template fails in CI, not
// at an operator's terminal in the middle of an incident.
//
// html/template, never text/template: the contextual auto-escaping is a security
// property here, not formatting. Container termination messages, event reasons,
// and image-pull errors are free-form strings the cluster controls, and they
// land verbatim in a document that gets forwarded.
var tmpl = template.Must(template.New("report").Parse(templateSource))

// view is the flattened shape the template ranges over. Every decision lives in
// newView so the template stays free of logic beyond ranging and conditionals.
type view struct {
	Version     string
	Scope       string
	Generated   string
	Counts      counts
	Findings    []findingRow
	Blind       []blindSpot
	Cluster     clusterhealth.ClusterHealth
	Workloads   []inventory.Workload
	Explanation string
	// SecurityRequested mirrors report.Input.SecurityRequested: true when
	// --security was passed, so the template can say the section was omitted
	// from this document rather than stay silent about it.
	SecurityRequested bool
	// Policy is nil unless --policy was given, so a scan without it renders the
	// same bytes it rendered before the flag existed.
	Policy *policyView
	// Certificates is nil unless there is something to flag — the same
	// suppression rule printCertificates applies in internal/report: Forbidden,
	// or a non-empty Expired/Expiring/Invalid. A --certs run that found nothing
	// renders no section here either; the text renderer's answer for that case
	// is a NOTES bullet, and this document has no NOTES section to duplicate it
	// into. A scan without --certs renders the same bytes it always did.
	Certificates *certificatesView
}

// counts is the header tally, and also labels the severity filter controls.
type counts struct {
	Critical, Warning, Info, Total int
	// AtLeastWarning is Critical+Warning, precomputed because templates have no
	// arithmetic and the "warning and above" filter control needs the number.
	AtLeastWarning int
}

// findingRow is one row of the findings table. Level is the lowercase spelling
// from findings.Level.String(), used as both the visible label and the CSS class
// the severity filter selects on — so the two can never disagree.
type findingRow struct {
	Level     string
	Kind      string
	Namespace string
	Name      string
	Issue     string
	Reason    string
	Owner     string
}

// blindSpot is one unreadable resource as the document shows it: the resource
// named in full, the reason only when safeReason clears it.
type blindSpot struct{ Resource, Reason string }

// policyView is the POLICY section. It is separate from Findings on purpose: a
// violation is a statement about an organization's rules, not about cluster
// health, and the header pills tally cluster health.
type policyView struct {
	Rules        int
	Violations   []policyRow
	NotEvaluated []policyRow
}

// policyRow is one line of the section. Target is the object for a violation
// and empty for an unevaluated rule, which examined no object at all.
type policyRow struct {
	RuleID, Level, Kind, Target, Message, Evidence string
}

// certificatesView is the CERTIFICATES section (opt-in --certs), mirroring
// printCertificates in internal/report: the same three categories, the same
// ingress route lines, the same Forbidden denial line, and the same footer.
type certificatesView struct {
	Forbidden bool
	Expired   []certRow
	Expiring  []certRow
	Invalid   []certInvalidRow
	Checked   int
	WarnDays  int
}

// certRow is one Expired or Expiring row. Days already carries the sign the
// template prints — "N days ago" for Expired, "in N days" for Expiring — so
// the template does no arithmetic. CommonName is sanitized at ingress, in
// certhealth.Assess, and is not re-sanitized here; contextual auto-escaping
// is this package's only defense for cluster-controlled text.
type certRow struct {
	Namespace, Name, CommonName string
	Days                        int
	Ingresses                   []string
}

// certInvalidRow is one Invalid row: a kubernetes.io/tls Secret whose
// certificate could not be parsed.
type certInvalidRow struct {
	Namespace, Name, Detail string
}

// The blind-spots block prints one of these three phrases and never the cluster's own
// words. They are kubeagent's, so nothing a cluster or a policy webhook can put in an
// error message can reach a document that is meant to be forwarded.
const (
	reasonForbidden   = "permission denied — kubeagent's credentials may not list it"
	reasonNotServed   = "the cluster does not serve this resource type"
	reasonUnavailable = "the read failed — see --output text or --output json for the reason"
)

// safeReason classifies a read failure instead of quoting it.
//
// scan.Result.PartialReads carries whatever the API returned. No filter over that text is
// safe: apierrors.NewForbidden interpolates the authorizer's own error, so an
// authorization message embeds the username — an IAM ARN, a node's internal DNS name or
// an OIDC email on a real cluster — and under webhook authorization it carries a
// third-party backend's free text too. Reading the message to choose a phrase can
// misclassify, which costs a reader some precision; quoting it can leak, which breaks the
// only guarantee this document makes. The exact message stays in the text and JSON
// reports, which are not written to be forwarded.
func safeReason(reason string) string {
	switch {
	case strings.Contains(reason, "forbidden"), strings.Contains(reason, "Unauthorized"):
		return reasonForbidden
	// The 404 wording is apimachinery's own literal, which is what a typed List
	// against a resource type the cluster does not serve returns. kubectl's
	// "doesn't have a resource type" wording comes from client-side RESTMapper
	// resolution in k8s.io/kubectl, which kubeagent does not depend on, so
	// matching it here would be matching a string this binary cannot produce.
	case strings.Contains(reason, "could not find the requested resource"):
		return reasonNotServed
	default:
		return reasonUnavailable
	}
}

// pathLike is what this document will not print. A filesystem path identifies
// the machine kubeagent ran on, and this file is written to be forwarded.
const pathLike = "the value was withheld: it looks like a filesystem path"

// noPath drops evidence that looks like a path. Evidence is cluster text, so in
// practice it never is one; the check is here because the cost of being wrong
// is a leak in the one artifact designed to leave the operator's control, and
// the cost of being right is one line of a report a reader can also get from
// --output text.
func noPath(evidence string) string {
	if strings.HasPrefix(evidence, "/") || strings.HasPrefix(evidence, "~") ||
		strings.Contains(evidence, "kubeconfig") {
		return pathLike
	}
	return evidence
}

// Render writes the complete HTML document to w. It performs no cluster calls:
// everything it needs was collected by the scan that produced in.
func Render(w io.Writer, in Input) error {
	return tmpl.Execute(w, newView(in))
}

// newView flattens Input into the template's view model.
func newView(in Input) view {
	now := in.Report.Now
	if now.IsZero() {
		// Same contract as report.Input.Now, which documents zero as wall-clock:
		// a caller that forgets the clock gets today, not year 1.
		now = time.Now()
	}
	scope := "all namespaces"
	if in.Namespace != "" {
		scope = "namespace " + in.Namespace
	}
	blind := make([]blindSpot, 0, len(in.Blind))
	for _, b := range in.Blind {
		blind = append(blind, blindSpot{Resource: b.Resource, Reason: safeReason(b.Reason)})
	}
	v := view{
		Version:           in.Version,
		Scope:             scope,
		Generated:         now.UTC().Format("2006-01-02 15:04:05 UTC"),
		Blind:             blind,
		Cluster:           in.Report.Cluster,
		Workloads:         in.Report.Result.Workloads,
		Explanation:       in.Report.Explanation,
		SecurityRequested: in.Report.SecurityRequested,
	}
	if p := in.Report.Policy; p != nil {
		pv := &policyView{Rules: p.Rules}
		for _, x := range p.Violations {
			target := x.Name
			if x.Namespace != "" {
				target = x.Namespace + "/" + x.Name
			}
			pv.Violations = append(pv.Violations, policyRow{
				RuleID: x.RuleID, Level: string(x.Level), Kind: x.Kind,
				Target: target, Message: x.Message, Evidence: noPath(x.Evidence),
			})
		}
		for _, x := range p.NotEvaluated {
			pv.NotEvaluated = append(pv.NotEvaluated, policyRow{
				RuleID: x.RuleID, Level: string(x.Level), Kind: x.Kind, Message: x.Reason,
			})
		}
		v.Policy = pv
	}
	if rep := in.Report.Certificates; rep != nil && (rep.Forbidden || len(rep.Expired) > 0 || len(rep.Expiring) > 0 || len(rep.Invalid) > 0) {
		cv := &certificatesView{Forbidden: rep.Forbidden, Checked: rep.Checked, WarnDays: rep.WarnDays}
		for _, c := range rep.Expired {
			cv.Expired = append(cv.Expired, certRow{
				Namespace: c.Namespace, Name: c.Name, CommonName: c.CommonName,
				Days: -c.Days, Ingresses: c.Ingresses,
			})
		}
		for _, c := range rep.Expiring {
			cv.Expiring = append(cv.Expiring, certRow{
				Namespace: c.Namespace, Name: c.Name, CommonName: c.CommonName,
				Days: c.Days, Ingresses: c.Ingresses,
			})
		}
		for _, iv := range rep.Invalid {
			cv.Invalid = append(cv.Invalid, certInvalidRow{Namespace: iv.Namespace, Name: iv.Name, Detail: iv.Detail})
		}
		v.Certificates = cv
	}
	for _, f := range in.Findings {
		v.Findings = append(v.Findings, findingRow{
			Level:     f.Level.String(),
			Kind:      f.Kind,
			Namespace: f.Namespace,
			Name:      f.Name,
			Issue:     f.Issue,
			Reason:    f.Reason,
			Owner:     f.Owner,
		})
		switch f.Level {
		case findings.Critical:
			v.Counts.Critical++
		case findings.Warning:
			v.Counts.Warning++
		default:
			v.Counts.Info++
		}
	}
	v.Counts.Total = len(in.Findings)
	v.Counts.AtLeastWarning = v.Counts.Critical + v.Counts.Warning
	return v
}
