package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/scan"
)

// inspectKinds is both the published enum and the accepted set.
var inspectKinds = []any{"pod", "deployment", "statefulset", "daemonset", "replicaset", "job", "cronjob"}

// Event is one Kubernetes event, flattened.
type Event struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int32  `json:"count"`
	Age     string `json:"age"`
}

// InspectInput is the kubeagent_inspect argument object.
type InspectInput struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Context   string `json:"context,omitempty"`
}

// InspectOutput is the kubeagent_inspect result.
type InspectOutput struct {
	Found     bool               `json:"found"`
	Kind      string             `json:"kind"`
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Status    string             `json:"status,omitempty"`
	Desired   int                `json:"desired,omitempty"`
	Ready     int                `json:"ready,omitempty"`
	Image     string             `json:"image,omitempty"`
	Pods      []inventory.PodRow `json:"pods"`
	Findings  []Finding          `json:"findings"`
	Events    []Event            `json:"events"`
	Coverage  *Coverage          `json:"coverage"`
}

func registerInspect(s *mcpsdk.Server, cfg Config, client kubernetes.Interface, now func() time.Time) {
	tool := &mcpsdk.Tool{
		Name: "kubeagent_inspect",
		Description: "Inspect one workload or pod: its status, its pods, kubeagent's findings for it, and " +
			"its recent Kubernetes events. Read-only: this never changes cluster state.",
		InputSchema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"kind": {
					Type:        "string",
					Enum:        inspectKinds,
					Description: "the kind of object to inspect",
				},
				"namespace": {
					Type:        "string",
					Description: "the object's namespace",
				},
				"name": {
					Type:        "string",
					Description: "the object's name",
				},
				"context": {
					Type: "string",
					Description: "kubeconfig context to use; only accepted when the server was started " +
						"with --allow-context-switch",
				},
			},
			Required: []string{"kind", "namespace", "name"},
		},
	}

	mcpsdk.AddTool(s, tool, guard("kubeagent_inspect",
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, in InspectInput) (*mcpsdk.CallToolResult, InspectOutput, error) {
			if in.Context != "" && !cfg.AllowContextSwitch {
				return nil, InspectOutput{}, errContextSwitchDisabled
			}

			res, err := scan.Evaluate(ctx, client, scan.Options{
				Namespace:       in.Namespace,
				IncludeCron:     true,
				IncludeRestarts: true,
				Logs:            cfg.Logs,
			})
			if err != nil {
				return nil, InspectOutput{}, errors.New("scanning the namespace: " + redact.Error(err))
			}

			cov := newCoverage(contextLabel(cfg.Context), in.Namespace, now())
			cov.markRun("workloads", "pod-diagnosis", "events")
			cov.markPartial(res.PartialReads)

			out := InspectOutput{
				Kind:      in.Kind,
				Namespace: in.Namespace,
				Name:      in.Name,
				Pods:      []inventory.PodRow{},
				Findings:  []Finding{},
				Events:    []Event{},
				Coverage:  cov,
			}

			for _, w := range res.Inventory.Workloads {
				if w.Namespace != in.Namespace || w.Name != in.Name ||
					!strings.EqualFold(w.Kind, in.Kind) {
					continue
				}
				out.Found = true
				out.Kind = w.Kind
				out.Status = w.Status
				out.Desired = w.Desired
				out.Ready = w.Ready
				out.Image = w.Image
				out.Pods = append(out.Pods, w.Pods...)
				for _, f := range w.Findings {
					out.Findings = append(out.Findings, fromDiagnose(f))
				}
				break
			}

			// Events are read for the named object whether or not the scan
			// found a workload for it: "the object is gone but its events
			// explain why" is exactly the case a drill-down must answer.
			events, evErr := collect.ObjectEvents(ctx, client, in.Namespace, in.Name)
			if evErr != nil {
				cov.Partial = append(cov.Partial, PartialRead{Resource: "events", Why: redact.Error(evErr)})
			}
			for _, e := range events {
				out.Events = append(out.Events, Event{
					Type:    e.Type,
					Reason:  e.Reason,
					Message: e.Message,
					Count:   e.Count,
					Age:     inventory.HumanAge(eventTime(e), now()),
				})
			}

			sortFindings(out.Findings)
			return nil, out, nil
		}))
}

// eventTime picks the most recent timestamp an event carries. Series events
// set EventTime, repeated events set LastTimestamp, and one-shot events set
// only FirstTimestamp.
func eventTime(e corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}
