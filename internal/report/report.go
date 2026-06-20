package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Print writes findings to w in the chosen format ("text" or "json").
func Print(findings []diagnose.Finding, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	case "text", "":
		return printText(findings, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printText(findings []diagnose.Finding, w io.Writer) error {
	if len(findings) == 0 {
		_, err := fmt.Fprintln(w, "No issues found. ✅")
		return err
	}
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
	_, err := fmt.Fprintf(w, "%d issue(s) found.\n", len(findings))
	return err
}
