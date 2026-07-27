package mcp

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/advisory"
	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// advisorySections is both the published enum and the accepted set.
var advisorySections = []any{"operators", "drift", "capacity", "security", "certificates"}

// AdvisoryInput is the kubeagent_advisory argument object.
type AdvisoryInput struct {
	Sections  []string `json:"sections"`
	Namespace string   `json:"namespace,omitempty"`
	Context   string   `json:"context,omitempty"`
}

// AdvisoryOutput carries one entry per requested section. Requested echoes what
// was asked for, so a caller comparing it with the keys of Sections can see at
// a glance that nothing was dropped on the floor.
type AdvisoryOutput struct {
	Requested []string       `json:"requested"`
	Sections  map[string]any `json:"sections"`
	Coverage  *Coverage      `json:"coverage"`
}

// normalizeSections dedupes and sorts, so the same request always produces the
// same payload regardless of the order the caller listed them in.
func normalizeSections(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func registerAdvisory(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_advisory",
		Description: "Run kubeagent's opt-in advisory sections: operator health, GitOps drift, scheduling " +
			"capacity, security posture, and certificate expiry. Each section costs extra API reads, so " +
			"request only what you need. Read-only: this never changes cluster state.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"sections": {
					Type:        "array",
					Description: "which advisory sections to run",
					Items:       &jsonschema.Schema{Type: "string", Enum: advisorySections},
				},
				"namespace": {
					Type:        "string",
					Description: "limit to one namespace; omit for the whole cluster",
				},
				"context": {
					Type: "string",
					Description: "kubeconfig context to use; only accepted when the server was started " +
						"with --allow-context-switch",
				},
			},
			Required: []string{"sections"},
		},
	}

	mcpsdk.AddTool(s, tool, guard("kubeagent_advisory",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in AdvisoryInput) (*mcpsdk.CallToolResult, AdvisoryOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, AdvisoryOutput{}, errContextSwitchDisabled
			}

			want := map[string]bool{}
			requested := normalizeSections(in.Sections)
			for _, s := range requested {
				want[s] = true
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			out := AdvisoryOutput{Requested: requested, Sections: map[string]any{}, Coverage: cov}

			// One scan feeds every section: the advisory assessors need the
			// workload objects it already listed. security and certificates
			// are computed inside Evaluate and only when asked for.
			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Security:        want["security"],
				Certs:           want["certificates"],
				CertWarnDays:    30,
			})
			if err != nil {
				return nil, AdvisoryOutput{}, errors.New("scanning the cluster: " + redact.Error(err))
			}
			cov.markPartial(res.PartialReads)

			if want["security"] {
				cov.markRun("security")
				out.Sections["security"] = res.SecurityIssues
			}
			if want["certificates"] {
				cov.markRun("certificates")
				if res.Certificates == nil {
					cov.markSkipped("certificates", "no certificate data was returned for this scope")
				} else {
					out.Sections["certificates"] = res.Certificates
				}
			}

			if want["operators"] || want["drift"] || want["capacity"] {
				pods, podsErr := advisory.ClusterPods(ctx, client, in.Namespace, res.Inputs.Pods)
				if podsErr != nil {
					cov.Partial = append(cov.Partial, PartialRead{
						Resource: "pods (cluster-wide)",
						Why: redact.Error(podsErr) +
							"; headroom is computed from namespace " + in.Namespace +
							" only and overstates free capacity",
					})
				}

				adv := advisory.Assess(ctx, client,
					func() (dynamic.Interface, discovery.DiscoveryInterface, error) {
						return cluster.NewDynamicClients(cfg.Kubeconfig, cfg.Context)
					},
					advisory.Inputs{
						Deployments:  res.Inputs.Deployments,
						StatefulSets: res.Inputs.StatefulSets,
						DaemonSets:   res.Inputs.DaemonSets,
						Jobs:         res.Inputs.Jobs,
						CronJobs:     res.Inputs.CronJobs,
						ReplicaSets:  res.Inputs.ReplicaSets,
						Nodes:        res.Nodes,
						Pods:         pods,
					},
					advisory.Options{
						Operators: want["operators"],
						Drift:     want["drift"],
						DriftAge:  24 * time.Hour,
						Capacity:  want["capacity"],
						Namespace: in.Namespace,
					}, now())

				// A degradation whose section still produced a report is a
				// partial read; one whose report is nil is a skipped check.
				reports := map[string]any{}
				if adv.Operators != nil {
					reports["operators"] = adv.Operators
				}
				if adv.GitOps != nil {
					reports["drift"] = adv.GitOps
				}
				if adv.Capacity != nil {
					reports["capacity"] = adv.Capacity
				}
				for _, d := range adv.Degradations {
					for _, section := range d.Sections {
						if _, produced := reports[section]; produced {
							cov.Partial = append(cov.Partial, PartialRead{Resource: section, Why: d.Reason})
						} else {
							cov.markSkipped(section, d.Reason)
						}
					}
				}
				for _, section := range []string{"operators", "drift", "capacity"} {
					if !want[section] {
						continue
					}
					if rep, ok := reports[section]; ok {
						cov.markRun(section)
						out.Sections[section] = rep
					}
				}

				if want["capacity"] {
					cov.MetricsServer = "absent"
					if adv.MetricsAvailable {
						cov.MetricsServer = "available"
					}
				}
			}

			return nil, out, nil
		}))
}
