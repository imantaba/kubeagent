package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/diagnose"
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
