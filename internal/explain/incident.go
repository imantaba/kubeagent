// Incident explanations for the watch daemon: one object that just broke,
// rendered together with the cluster context the daemon already holds. Only
// structured fields are sent — never pod rows, pod IPs, node names, specs, or
// logs. The daemon performs no additional cluster reads to build this.
package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// IncidentSystemPrompt frames a single-object follow-up. It deliberately differs
// from SystemPrompt: a page that has already fired needs the cause of one
// object's failure, not a ranked remediation plan for the whole cluster.
const IncidentSystemPrompt = `You are a senior Kubernetes SRE. An alert has just fired for ONE object. Explain
why that object broke, using ONLY the facts provided — do not invent causes,
resources, or values that are not given.

Answer in at most 120 words, as three labelled lines:

Cause: the most likely cause of THIS object's failure, in one sentence. If the
facts show the same root cause affecting other objects, say so — a shared cause
is the most useful thing you can tell the person holding the pager.
Check: one or two read-only commands to confirm it (kubectl get/describe/logs).
Fix: use the provided deterministic, pre-reviewed command verbatim. Never
substitute or invent a different command.

No preamble, no restating the input, no generic advice. Prefer "likely" over
false certainty.`

// BuildIncidentPrompt renders the object that just broke plus the surrounding
// context. object is a pre-rendered identity such as "Deployment/shop/web".
//
// Only structured fields are sent. Workload.Pods is deliberately not rendered:
// pod names, node names and pod IPs explain nothing and are exactly the kind of
// detail that should not leave a cluster.
func BuildIncidentPrompt(object string, issues []string, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Alert just fired for %s.\n", object)
	if len(issues) > 0 {
		fmt.Fprintf(&b, "Its issues: %s\n", strings.Join(issues, ", "))
	}
	b.WriteString("\n")

	if cluster.Verdict == "Degraded" {
		fmt.Fprintf(&b, "Cluster health: DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
		for _, iss := range cluster.NodeIssues {
			fmt.Fprintf(&b, "  node %s\n", iss)
		}
		for _, iss := range cluster.SystemIssues {
			fmt.Fprintf(&b, "  system %s\n", iss)
		}
		b.WriteString("\n")
	}

	if len(flagged) > 0 {
		b.WriteString("All workloads currently flagged (this object should be among them):\n")
		for _, w := range flagged {
			fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
				w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
			for _, f := range w.Findings {
				// Evidence is API text and quotes addresses outright — the
				// same rule as findingBlock in explain.go: redact where the
				// prompt is assembled, keep the rendered report raw.
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, redact.Addresses(f.Evidence))
				// LogCause is one of logscan's fixed classifier strings —
				// every signature discards its submatches — so it cannot
				// carry an in-cluster address today. This call is defence in
				// depth at the boundary where a prompt leaves the process: it
				// holds even if a future signature interpolates the line it
				// matched.
				if f.LogCause != "" {
					fmt.Fprintf(&b, "      log cause: %s\n", redact.Addresses(f.LogCause))
				}
				s := suggestionFor(f, w)
				fmt.Fprintf(&b, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
			}
			if w.RootCause != "" {
				fmt.Fprintf(&b, "    root cause: %s\n", w.RootCause)
			}
			if len(w.NetworkPolicies) > 0 {
				fmt.Fprintf(&b, "    network policy: pods selected by %s (possible cause)\n", strings.Join(w.NetworkPolicies, ", "))
			}
			if w.Rollout != nil {
				fmt.Fprintf(&b, "    recent change: rolled out to revision %s %s", w.Rollout.Revision, w.Rollout.Since)
				if w.Rollout.NewImage != "" {
					fmt.Fprintf(&b, ", image %s → %s", w.Rollout.OldImage, w.Rollout.NewImage)
				}
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}

	if len(serviceIssues) > 0 {
		b.WriteString("Service issues:\n")
		for _, is := range serviceIssues {
			// Problem is the failure ("NoEndpoints"); Type is the service kind.
			// BuildInventoryPrompt renders only Type, which loses the failure —
			// an incident explanation needs both.
			fmt.Fprintf(&b, "  - %s/%s (%s, %s): %s\n", is.Namespace, is.Name, is.Type, is.Problem, is.Detail)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Explain why %s broke, in the required three-line form.", object)
	return b.String()
}

// ExplainIncident sends an already-built incident prompt. Building is separate
// from calling so the caller can render on its own goroutine and hand the worker
// a self-contained job.
func (c *Client) ExplainIncident(ctx context.Context, prompt string) (string, error) {
	out, err := c.s.summarize(ctx, IncidentSystemPrompt, prompt)
	if err != nil {
		return "", fmt.Errorf("explaining incident: %w", err)
	}
	// out.Truncated is discarded here, not detected: internal/oncall depends
	// on ExplainIncident's (string, error) signature and WatchVersion stays
	// 1.0, so this path's own truncation goes unrecorded rather than being
	// surfaced. That is an accepted cost of keeping the interface and the
	// schema version fixed, not a claim that the incident narrative cannot
	// be cut.
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", fmt.Errorf("explaining incident: model returned no text")
	}
	return text, nil
}
