package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/credlint"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// inventoryReport is the JSON shape for the workload inventory.
type inventoryReport struct {
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
	SecurityIssues     []secscan.Finding           `json:"securityIssues,omitempty"`
	Explanation        string                      `json:"explanation,omitempty"`
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
	SecurityIssues     []secscan.Finding
	SecurityVerbose    bool
	Explanation        string
}

// PrintInventory writes the cluster verdict and the prioritized workload set to w.
func PrintInventory(in Input, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{
			Cluster:            in.Cluster,
			Workloads:          in.Result.Workloads,
			Resources:          in.Resources,
			Platform:           in.Platform,
			ServiceIssues:      in.ServiceIssues,
			CredentialWarnings: in.CredentialWarnings,
			NodeReserve:        in.NodeReserve,
			PVCReclaim:         in.PVCReclaim,
			DiskUsage:          in.DiskUsage,
			IngressIssues:      in.IngressIssues,
			SecurityIssues:     in.SecurityIssues,
			Explanation:        in.Explanation,
		})
	case "text":
		return printInventoryText(in, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printInventoryText(in Input, w io.Writer) error {
	real, expected := splitServiceIssues(in.ServiceIssues)

	if err := printHeader(in, real, w); err != nil {
		return err
	}

	hasDisk := in.DiskUsage != nil && len(in.DiskUsage.Over) > 0
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(in.IngressIssues) > 0
	if hasAttention {
		if _, err := fmt.Fprintln(w, "NEEDS ATTENTION"); err != nil {
			return err
		}
		for _, wl := range in.Result.Workloads {
			if err := printWorkload(wl, w); err != nil {
				return err
			}
		}
		if err := printServiceIssues(real, "  ✗", w); err != nil {
			return err
		}
		if err := printCredentialWarnings(in.CredentialWarnings, w); err != nil {
			return err
		}
		if err := printDiskUsage(in.DiskUsage, w); err != nil {
			return err
		}
		if err := printIngressIssues(in.IngressIssues, w); err != nil {
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

	if err := printNotes(in, expected, w); err != nil {
		return err
	}

	if err := printContext(in, w); err != nil {
		return err
	}

	if !hasAttention && !hasSecurity && in.Cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}

	if in.Explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ──\n%s\n", in.Explanation); err != nil {
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

// printHeader prints the cluster verdict line and, when anything is flagged, a
// workload-scoped attention line.
func printHeader(in Input, real []svchealth.Issue, w io.Writer) error {
	c := in.Cluster
	if c.Verdict == "" {
		return nil
	}
	if _, err := fmt.Fprintf(w, "Cluster: %s — %d/%d nodes Ready\n", c.Verdict, c.NodesReady, c.NodesTotal); err != nil {
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
		if _, err := fmt.Fprintf(w, "  · %s\n", c.ScopeNote); err != nil {
			return err
		}
	}
	if line := attentionLine(in, real); line != "" {
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
func attentionLine(in Input, real []svchealth.Issue) string {
	failing := 0
	for _, wl := range in.Result.Workloads {
		if wl.Flagged() {
			failing++
		}
	}
	var parts []string
	if failing > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing", failing, plural(failing, "workload", "workloads")))
	}
	if len(real) > 0 {
		parts = append(parts, fmt.Sprintf("%d %s without endpoints", len(real), plural(len(real), "service", "services")))
	}
	if in.DiskUsage != nil && len(in.DiskUsage.Over) > 0 {
		n := len(in.DiskUsage.Over)
		parts = append(parts, fmt.Sprintf("%d %s low on disk", n, plural(n, "volume", "volumes")))
	}
	if n := len(in.IngressIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ingress %s broken", n, plural(n, "route", "routes")))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printNotes renders advisory content: expected-empty services, PVC reclaim, and
// the hidden-counts footer.
func printNotes(in Input, expected []svchealth.Issue, w io.Writer) error {
	var b strings.Builder
	if n := in.NodeReserve; n != nil && n.WarnCount > 0 {
		var names []string
		for _, r := range n.Nodes {
			if r.Warning {
				names = append(names, r.Name)
			}
		}
		fmt.Fprintf(&b, "  • %d %s reserve no memory: %s\n", n.WarnCount, plural(n.WarnCount, "node", "nodes"), strings.Join(names, ", "))
	}
	if err := printPVCReclaim(in.PVCReclaim, in.PVCReclaimFull, &b); err != nil {
		return err
	}
	if err := printServiceIssues(expected, "  •", &b); err != nil {
		return err
	}
	if hint := footerHint(in.Result); hint != "" {
		fmt.Fprintf(&b, "  • %s\n", hint)
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
		ok := total - n.WarnCount
		line := fmt.Sprintf("Nodes  %d/%d reserve memory OK", ok, total)
		if n.WarnCount == 0 {
			line = fmt.Sprintf("Nodes  %d nodes · kubelet reservations OK", total)
		}
		fmt.Fprintln(&b, line)
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
		pct := int(v.Ratio*100 + 0.5)
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

// printIngressIssues lists Ingress routes whose backend chain is broken.
func printIngressIssues(issues []ingresshealth.RouteIssue, w io.Writer) error {
	for _, r := range issues {
		line := fmt.Sprintf("  ✗ ingress %s/%s", r.Namespace, r.Ingress)
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
	if !verbose && nRestricted > 0 {
		if _, err := fmt.Fprintf(w, "\n  restricted (hardening gaps, near-universal): %d across %d %s\n",
			nRestricted, len(restrictedWorkloads), plural(len(restrictedWorkloads), "workload", "workloads")); err != nil {
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
		for _, p := range rep.PVCs {
			line := fmt.Sprintf("  • %s/%s  pv %s", p.Namespace, p.Name, p.PV)
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

func printServiceIssues(issues []svchealth.Issue, glyph string, w io.Writer) error {
	for _, is := range issues {
		line := fmt.Sprintf("%s %s/%s  %s  %s", glyph, is.Namespace, is.Name, is.Type, is.Detail)
		if is.Since != "" {
			line += " · " + inventory.HumanSince(is.Since, time.Now())
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

func printWorkload(wl inventory.Workload, w io.Writer) error {
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
			header += fmt.Sprintf(", last %s", inventory.HumanSince(wl.LastRestart, time.Now()))
		}
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	if wl.Image != "" {
		if _, err := fmt.Fprintf(w, "    image %s\n", wl.Image); err != nil {
			return err
		}
	}
	for _, f := range wl.Findings {
		if _, err := fmt.Fprintf(w, "    ⚠ %s: %s\n", f.Issue, f.Reason); err != nil {
			return err
		}
		if f.Evidence != "" && f.Evidence != f.Reason {
			if _, err := fmt.Fprintf(w, "      ↳ %s\n", f.Evidence); err != nil {
				return err
			}
		}
		if f.Resources != nil {
			r := f.Resources
			if _, err := fmt.Fprintf(w, "      resources: memory req=%s limit=%s · cpu req=%s limit=%s\n",
				r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit); err != nil {
				return err
			}
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
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	for _, p := range wl.Pods {
		restarts := fmt.Sprintf("%d", p.Restarts)
		if p.LastRestart != "" {
			restarts += " (" + inventory.HumanSince(p.LastRestart, time.Now()) + ")"
		}
		if _, err := fmt.Fprintf(w, "    %s  %s  %s  restarts=%s  %s  %s  %s\n",
			p.Name, p.Ready, p.Phase, restarts, p.Node, p.IP, p.Age); err != nil {
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
