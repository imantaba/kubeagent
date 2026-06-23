package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// explainedReport is the JSON shape when an explanation is present.
type explainedReport struct {
	Findings    []diagnose.Finding `json:"findings"`
	Explanation string             `json:"explanation"`
}

// Print writes findings to w in the chosen format ("text" or "json"). When
// explanation is non-empty it is appended (text) or wrapped in (json) the
// output; when empty, the output is identical to v1.1.
func Print(findings []diagnose.Finding, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if explanation == "" {
			return enc.Encode(findings)
		}
		return enc.Encode(explainedReport{Findings: findings, Explanation: explanation})
	case "text":
		return printText(findings, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printText(findings []diagnose.Finding, explanation string, w io.Writer) error {
	if len(findings) == 0 {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	} else {
		for _, f := range findings {
			if _, err := fmt.Fprintf(w, "%s\t%s\n", f.Pod, f.Issue); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "    %s\n", f.Reason); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "    evidence: %s\n\n", f.Evidence); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%d issue(s) found.\n", len(findings)); err != nil {
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

// inventoryReport is the JSON shape for the workload inventory.
type inventoryReport struct {
	Workloads   []inventory.Workload `json:"workloads"`
	Explanation string               `json:"explanation,omitempty"`
}

// PrintInventory writes the grouped workload inventory to w in the chosen
// format. explanation, when non-empty, is appended (text) or added (json).
func PrintInventory(workloads []inventory.Workload, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Workloads: workloads, Explanation: explanation})
	case "text":
		return printInventoryText(workloads, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printInventoryText(workloads []inventory.Workload, explanation string, w io.Writer) error {
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
				header += fmt.Sprintf(", last %s", wl.LastRestart)
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
				restarts += " (" + p.LastRestart + ")"
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
