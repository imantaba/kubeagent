package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/advisory"
	"github.com/imantaba/kubeagent/internal/audit"
	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/htmlreport"
	"github.com/imantaba/kubeagent/internal/remediate"
	"github.com/imantaba/kubeagent/internal/report"
	"github.com/imantaba/kubeagent/internal/scan"
)

// advisoryBlindSpots turns the advisory degradations that are permission
// problems into blind spots the report will name. Reasons are already redacted
// by internal/advisory; the "forbidden:" prefix is what makes the HTML report
// classify them, and it is kubeagent's word, not the API server's. A degradation
// with any other cause — a CRD that is simply not installed — is not a blind
// spot and is left to the existing stderr warning.
func advisoryBlindSpots(degradations []advisory.Degradation) []scan.ReadFailure {
	var out []scan.ReadFailure
	for _, d := range degradations {
		if !strings.Contains(strings.ToLower(d.Reason), "forbidden") {
			continue
		}
		out = append(out, scan.ReadFailure{
			Resource: d.Subject,
			Reason:   "forbidden: kubeagent's credentials may not list " + d.Subject,
		})
	}
	return out
}

// renderScan writes the scan output in the requested format. It is its own
// function — rather than an inline branch at the call site — for the same reason
// gateScanOptions is: so a test can drive the exact values runScan uses without a
// live cluster. Inline, the only way to reach the HTML path would be to connect
// to a cluster, and a field that silently never reached htmlreport.Input would
// ship unnoticed.
func renderScan(w io.Writer, format string, in report.Input, res scan.Result, namespace string) error {
	if format == "html" {
		return htmlreport.Render(w, htmlreport.Input{
			Report:    in,
			Findings:  findings.Flatten(res),
			Blind:     res.PartialReads,
			Namespace: namespace,
			Version:   version,
		})
	}
	return report.PrintInventory(in, format, w)
}

// resultInput maps every scan.Result-derived field onto a report.Input. Keeping
// this mapping in one testable place guards against a Result field silently never
// reaching the report — as StuckTerminating once did when only the inline literal
// carried the wiring. The presentation-layer extras (clock, resource summary,
// platform facts, flag-gated reports, credential/explain output) are filled in by
// the caller after this returns.
func resultInput(res scan.Result) report.Input {
	return report.Input{
		Cluster:            res.Health,
		Result:             res.Inventory,
		ServiceIssues:      res.ServiceIssues,
		NodeReserve:        &res.NodeReserve,
		PVCReclaim:         &res.PVCReclaim,
		IngressIssues:      res.IngressIssues,
		PVCIssues:          res.PVCIssues,
		SecurityIssues:     res.SecurityIssues,
		Certificates:       res.Certificates,
		StuckTerminating:   res.StuckTerminating,
		PDBIssues:          res.PDBIssues,
		HPAIssues:          res.HPAIssues,
		WebhookIssues:      res.WebhookIssues,
		WebhookURLBackends: res.WebhookURLBackends,
		QuotaIssues:        res.QuotaIssues,
		Blind:              res.PartialReads,
	}
}

// runRollback undoes the most recent applied remediation recorded in the audit log. The
// inverse action is derived deterministically (never LLM-decided) and applied through
// the same guard rails as any fix: preview, confirmation, drift bond, RBAC preflight.
func runRollback(ctx context.Context, client kubernetes.Interface, auditPath string, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) error {
	rec, found, err := audit.ReadLast(auditPath, func(r audit.Record) bool { return r.Disposition == "applied" })
	if err != nil {
		return fmt.Errorf("reading audit log %q: %w", auditPath, err)
	}
	if !found {
		fmt.Fprintf(w, "\nNo applied remediation found in %s; nothing to roll back.\n", auditPath)
		return nil
	}
	a, err := remediate.Inverse(rec.Kind, rec.Namespace, rec.Name, rec.FromRevision, rec.ToRevision)
	if err != nil {
		fmt.Fprintf(w, "\nCannot roll back the last applied fix (%s %s): %v\n", rec.Kind, rec.Target, err)
		return nil
	}
	logAudit := func(disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: audit log write failed: %v\n", err)
		}
	}
	fmt.Fprintf(w, "\nRolling back the fix applied at %s\nProposed rollback: %s — %s\n  reason: %s\n",
		rec.Time, a.Target, a.Summary, a.Reason)
	if len(a.Changes) > 0 {
		fmt.Fprintln(w, "  will change:")
		for _, c := range a.Changes {
			if c.From == "" && c.To == "" {
				fmt.Fprintf(w, "    %s\n", c.Field)
			} else {
				fmt.Fprintf(w, "    %s: %s → %s\n", c.Field, c.From, c.To)
			}
		}
	}
	fmt.Fprintf(w, "  kubectl equivalent: %s\n", a.KubectlEquivalent)
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: not applied)")
		logAudit("dry-run", "")
		return nil
	}
	if !assumeYes {
		fmt.Fprint(w, "  Roll back? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(w, "  skipped.")
			logAudit("declined", "")
			return nil
		}
	}
	res := remediate.Apply(ctx, client, a)
	switch {
	case res.Err != nil:
		fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		logAudit("error", res.Err.Error())
	case res.Applied:
		fmt.Fprintf(w, "  rolled back: %s\n", res.Detail)
		logAudit("rollback", res.Detail)
	case res.PreflightDenied:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("preflight", res.Detail)
	default:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("refused", res.Detail)
	}
	return nil
}

// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). The actions were
// planned once in runScan; Apply is bound to what each preview promised. Writes
// are guarded inside remediate.Apply. auditw may be nil (no audit logging).
func runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	logAudit := func(a remediate.Action, disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: audit log write failed: %v\n", err)
		}
	}
	reader := bufio.NewReader(in)
	for _, a := range actions {
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n", a.Target, a.Summary, a.Reason)
		if len(a.Changes) > 0 {
			fmt.Fprintln(w, "  will change:")
			for _, c := range a.Changes {
				if c.From == "" && c.To == "" {
					fmt.Fprintf(w, "    %s\n", c.Field)
				} else {
					fmt.Fprintf(w, "    %s: %s → %s\n", c.Field, c.From, c.To)
				}
			}
		}
		fmt.Fprintf(w, "  kubectl equivalent: %s\n", a.KubectlEquivalent)
		if dryRun {
			allowed, reason, err := remediate.Preflight(ctx, client, a)
			switch {
			case err != nil:
				fmt.Fprintf(w, "  (dry-run: not applied; permission check errored: %v)\n", err)
				logAudit(a, "dry-run", "permission check errored: "+err.Error())
			case allowed:
				fmt.Fprintln(w, "  (dry-run: not applied; you have permission to apply this)")
				logAudit(a, "dry-run", "permission: allowed")
			default:
				fmt.Fprintf(w, "  (dry-run: not applied; would be blocked — %s)\n", reason)
				logAudit(a, "dry-run", reason)
			}
			continue
		}
		if !assumeYes {
			fmt.Fprint(w, "  Apply? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Fprintln(w, "  skipped.")
				logAudit(a, "declined", "")
				continue
			}
		}
		res := remediate.Apply(ctx, client, a)
		switch {
		case res.Err != nil:
			fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
			logAudit(a, "error", res.Err.Error())
		case res.Applied:
			fmt.Fprintf(w, "  applied: %s\n", res.Detail)
			logAudit(a, "applied", res.Detail)
		case res.PreflightDenied:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "preflight", res.Detail)
		default:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "refused", res.Detail)
		}
	}
}
