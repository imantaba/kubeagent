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
	// namespace and is empty for -n kube-system.
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
	Blind       []scan.ReadFailure
	Cluster     clusterhealth.ClusterHealth
	Workloads   []inventory.Workload
	Explanation string
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
	v := view{
		Version:     in.Version,
		Scope:       scope,
		Generated:   now.UTC().Format("2006-01-02 15:04:05 UTC"),
		Blind:       in.Blind,
		Cluster:     in.Report.Cluster,
		Workloads:   in.Report.Result.Workloads,
		Explanation: in.Report.Explanation,
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
