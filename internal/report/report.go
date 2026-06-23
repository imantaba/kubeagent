package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// inventoryReport is the JSON shape for the workload inventory.
type inventoryReport struct {
	Cluster     clusterhealth.ClusterHealth `json:"cluster"`
	Workloads   []inventory.Workload        `json:"workloads"`
	Explanation string                      `json:"explanation,omitempty"`
}

// PrintInventory writes the cluster verdict and grouped workload inventory to w.
func PrintInventory(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: workloads, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, workloads, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printInventoryText(cluster clusterhealth.ClusterHealth, workloads []inventory.Workload, explanation string, w io.Writer) error {
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
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	if len(workloads) == 0 {
		_, err := fmt.Fprintln(w, "No workloads found.")
		return err
	}
	for _, wl := range workloads {
		flag := "  "
		if wl.Flagged() {
			flag = "⚠ "
		}
		header := fmt.Sprintf("%s%s/%s  %s  %d/%d %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Ready, wl.Desired, wl.Status)
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
	}
	if explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ──\n%s\n", explanation); err != nil {
			return err
		}
	}
	return nil
}
