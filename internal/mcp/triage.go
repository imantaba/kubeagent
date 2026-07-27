package mcp

import (
	"context"
	"errors"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// Cluster identifies what was looked at.
type Cluster struct {
	Context string `json:"context"`
	Version string `json:"version"`
	Nodes   int    `json:"nodes"`
}

// TriageInput is the kubeagent_triage argument object.
type TriageInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"limit the scan to one namespace; omit to scan the whole cluster"`
	Context   string `json:"context,omitempty" jsonschema:"kubeconfig context to use; only accepted when the server was started with --allow-context-switch"`
}

// TriageOutput is the kubeagent_triage result.
type TriageOutput struct {
	// Verdict is "healthy" or "degraded".
	Verdict         string    `json:"verdict"`
	Cluster         Cluster   `json:"cluster"`
	Findings        []Finding `json:"findings"`
	FindingsOmitted int       `json:"findingsOmitted,omitempty"`
	Coverage        *Coverage `json:"coverage"`
}

// errContextSwitchDisabled is returned verbatim to the caller, so it explains
// the fix without naming a kubeconfig path or a server address.
var errContextSwitchDisabled = errors.New(
	"this server was started without --allow-context-switch, so it only answers for the cluster it was started against")

func registerTriage(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_triage",
		Description: "Run kubeagent's deterministic read-only diagnosis over a cluster or one namespace and " +
			"return a verdict, the findings that support it, and a coverage block stating what was and was " +
			"not examined. Read-only: this never changes cluster state.",
	}
	mcpsdk.AddTool(s, tool, guard("kubeagent_triage",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in TriageInput) (*mcpsdk.CallToolResult, TriageOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, TriageOutput{}, errContextSwitchDisabled
			}

			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Logs:            cfg.Logs,
			})
			if err != nil {
				return nil, TriageOutput{}, errors.New("scanning the cluster: " + redact.Error(err))
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			cov.markRun("workloads", "pod-diagnosis", "services", "ingresses", "persistentvolumeclaims",
				"terminating", "poddisruptionbudgets", "horizontalpodautoscalers", "webhooks", "resourcequotas")
			cov.markSkipped("credential-lint", "not run by triage; use the kubeagent CLI")
			cov.markSkipped("disk-usage", "not run by triage; it needs node stats the server does not request")
			cov.markSkipped("security", "not run by triage; call kubeagent_advisory with section \"security\"")
			cov.markSkipped("certificates", "not run by triage; call kubeagent_advisory with section \"certificates\"")
			cov.markSkipped("kubelet-health", "not run by triage; it is opt-in and not reachable through "+
				"kubeagent_advisory either — use the kubeagent CLI's --kubelet-health flag")
			cov.markSkipped("control-plane-health", "not run by triage; it is opt-in and not reachable through "+
				"kubeagent_advisory either — use the kubeagent CLI's --control-plane-health flag")
			cov.markSkipped("dns-health", "not run by triage; it is opt-in and not reachable through "+
				"kubeagent_advisory either — use the kubeagent CLI's --dns-health flag")
			cov.markPartial(res.PartialReads)
			if cfg.Logs {
				cov.markRun("log-tails")
			} else {
				cov.markSkipped("log-tails", "the server was started without --logs")
			}

			findings, omitted := capFindings(findingsFromResult(res))
			verdict := "healthy"
			if len(findings) > 0 {
				verdict = "degraded"
			}

			return nil, TriageOutput{
				Verdict: verdict,
				Cluster: Cluster{
					Context: contextLabel(cfg.Context),
					Version: platform.Detect(res.Nodes, nil, nil, nil).KubeVersion,
					Nodes:   len(res.Nodes),
				},
				Findings:        findings,
				FindingsOmitted: omitted,
				Coverage:        cov,
			}, nil
		}))
}
