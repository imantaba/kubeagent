package gate

import (
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/findings"
)

// RenderText writes the human-readable verdict a CI log shows. It is the
// default --output, so it leads with the one line an operator scanning a wall
// of build output needs, and only then explains it.
func RenderText(w io.Writer, v Verdict) error {
	var headline string
	switch v.Verdict {
	case "pass":
		headline = fmt.Sprintf("GATE: pass — nothing at or above %s (scope: %s)", v.FailOn, v.Scope)
	case "fail":
		headline = fmt.Sprintf("GATE: fail — %s at or above %s (scope: %s)",
			plural(len(v.Failing), "finding", "findings"), v.FailOn, v.Scope)
	case "inconclusive":
		headline = fmt.Sprintf("GATE: inconclusive — kubeagent could not see enough to judge (scope: %s)", v.Scope)
	case "timeout":
		headline = fmt.Sprintf("GATE: timeout — the rollout did not settle (scope: %s)", v.Scope)
	default:
		headline = fmt.Sprintf("GATE: %s (scope: %s)", v.Verdict, v.Scope)
	}
	if _, err := fmt.Fprintln(w, headline); err != nil {
		return err
	}
	if v.Verdict == "timeout" && v.Detail != "" {
		if _, err := fmt.Fprintf(w, "  last observed: %s\n", v.Detail); err != nil {
			return err
		}
	}

	for _, f := range v.Failing {
		if _, err := fmt.Fprintf(w, "\n  %s  %s %s  %s\n", f.Level, f.Kind, findingIdentity(f), f.Issue); err != nil {
			return err
		}
		if f.Reason != "" {
			if _, err := fmt.Fprintf(w, "            %s\n", f.Reason); err != nil {
				return err
			}
		}
	}

	for _, b := range v.Inconclusive {
		suffix := ""
		if b.Waived {
			suffix = " (waived)"
		}
		if _, err := fmt.Fprintf(w, "\n  could not read %s: %s%s\n", b.Resource, b.Reason, suffix); err != nil {
			return err
		}
	}

	if len(v.Reported) > 0 {
		if _, err := fmt.Fprintf(w, "\nnot counted (below --fail-on): %s\n",
			plural(len(v.Reported), "finding", "findings")); err != nil {
			return err
		}
	}
	return nil
}

// findingIdentity renders a finding's Namespace/Name pair as the identity
// column: "namespace/name" for a namespaced finding, "name" alone for a
// cluster-scoped one (empty Namespace). A cluster-scoped finding — a
// ValidatingWebhookConfiguration, for example — genuinely has no namespace;
// printing an empty Namespace with the unconditional "%s/%s" form left a
// stray leading slash on that identity. One helper so any future text
// renderer makes the same decision in one place.
func findingIdentity(f findings.Finding) string {
	if f.Namespace == "" {
		return f.Name
	}
	return f.Namespace + "/" + f.Name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
