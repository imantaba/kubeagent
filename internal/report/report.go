package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// inventoryReport is the JSON shape for the workload inventory.
type inventoryReport struct {
	Cluster       clusterhealth.ClusterHealth `json:"cluster"`
	Workloads     []inventory.Workload        `json:"workloads"`
	Resources     *resources.Summary          `json:"resources,omitempty"`
	Platform      *platform.Facts             `json:"platform,omitempty"`
	ServiceIssues []svchealth.Issue           `json:"serviceIssues,omitempty"`
	Explanation   string                      `json:"explanation,omitempty"`
}

// PrintInventory writes the cluster verdict and the prioritized workload set to w.
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, ServiceIssues: serviceIssues, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, serviceIssues, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, explanation string, w io.Writer) error {
	if cluster.Verdict != "" {
		if _, err := fmt.Fprintf(w, "Cluster: %s — %d/%d nodes Ready\n", cluster.Verdict, cluster.NodesReady, cluster.NodesTotal); err != nil {
			return err
		}
		for _, iss := range cluster.NodeIssues {
			if _, err := fmt.Fprintf(w, "  ⚠ node %s\n", iss); err != nil {
				return err
			}
		}
		for _, iss := range cluster.SystemIssues {
			if _, err := fmt.Fprintf(w, "  ⚠ system %s\n", iss); err != nil {
				return err
			}
		}
		if cluster.ScopeNote != "" {
			if _, err := fmt.Fprintf(w, "  · %s\n", cluster.ScopeNote); err != nil {
				return err
			}
		}
		if facts != nil {
			if line := facts.Line(); line != "" {
				if _, err := fmt.Fprintf(w, "Platform: %s\n", line); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if err := printResources(summary, w); err != nil {
		return err
	}

	for _, wl := range result.Workloads {
		if err := printWorkload(wl, w); err != nil {
			return err
		}
	}

	if err := printServiceIssues(serviceIssues, w); err != nil {
		return err
	}

	if len(result.Workloads) == 0 && len(serviceIssues) == 0 && cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}

	if hint := footerHint(result); hint != "" {
		if _, err := fmt.Fprintln(w, hint); err != nil {
			return err
		}
	}

	if explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ──\n%s\n", explanation); err != nil {
			return err
		}
	}
	return nil
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

func printServiceIssues(issues []svchealth.Issue, w io.Writer) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Service issues:"); err != nil {
		return err
	}
	for _, is := range issues {
		line := fmt.Sprintf("  ⚠ %s/%s  %s  %s", is.Namespace, is.Name, is.Type, is.Detail)
		if is.Since != "" {
			line += " · " + inventory.HumanSince(is.Since, time.Now())
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func printWorkload(wl inventory.Workload, w io.Writer) error {
	flag := "  "
	if wl.Flagged() {
		flag = "⚠ "
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
		if f.Resources != nil {
			r := f.Resources
			if _, err := fmt.Fprintf(w, "      resources: memory req=%s limit=%s · cpu req=%s limit=%s\n",
				r.MemRequest, r.MemLimit, r.CPURequest, r.CPULimit); err != nil {
				return err
			}
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
