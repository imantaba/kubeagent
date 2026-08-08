package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/diagnose"
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

func registerInspect(s *mcpsdk.Server, cfg Config, base kubernetes.Interface, switchTo clientFactory, now func() time.Time) {
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
					MinLength:   jsonschema.Ptr(1),
					Description: "the object's namespace",
				},
				"name": {
					Type:        "string",
					MinLength:   jsonschema.Ptr(1),
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
			client, contextName, err := clientFor(cfg, base, switchTo, in.Context)
			if err != nil {
				return nil, InspectOutput{}, err
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

			cov := newCoverage(contextName, in.Namespace, now())
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

			if r := resolveObject(res, in.Kind, in.Namespace, in.Name); r.Found {
				out.Found = true
				out.Kind = r.Kind
				out.Status = r.Status
				out.Desired = r.Desired
				out.Ready = r.Ready
				out.Image = r.Image
				out.Pods = append(out.Pods, r.Pods...)
				out.Findings = append(out.Findings, r.Findings...)
			}

			// Events are read for the named object whether or not the scan
			// found a workload for it: "the object is gone but its events
			// explain why" is exactly the case a drill-down must answer.
			events, evErr := collect.ObjectEvents(ctx, client, in.Namespace, in.Name)
			if evErr != nil {
				cov.Partial = append(cov.Partial, PartialRead{Resource: "events", Why: redact.Error(evErr)})
			}
			sortEvents(events)
			for _, e := range events {
				out.Events = append(out.Events, Event{
					Type:    e.Type,
					Reason:  e.Reason,
					Message: e.Message,
					Count:   e.Count,
					Age:     eventAge(e, now()),
				})
			}

			sortFindings(out.Findings)
			return nil, out, nil
		}))
}

// resolved is the answer to an object lookup, separated from InspectOutput so
// the lookup is a pure function a test can call without standing up a server.
type resolved struct {
	Found    bool
	Kind     string
	Status   string
	Desired  int
	Ready    int
	Image    string
	Pods     []inventory.PodRow
	Findings []Finding
}

// findingsOf gathers every diagnose.Finding the scan attached to a workload.
// Re-attaching them to a recomputed workload set is lossless: Workload.Flagged()
// is true whenever len(Findings) > 0 and Prioritize never drops a flagged
// workload, so no finding can have been filtered out of the list this reads.
func findingsOf(ws []inventory.Workload) []diagnose.Finding {
	var out []diagnose.Finding
	for _, w := range ws {
		out = append(out, w.Findings...)
	}
	return out
}

// resolveObject answers "does this object exist, and what is its state" from
// the snapshot scan.Evaluate already returned. It issues no cluster call.
//
// It deliberately does not *match* against res.Inventory.Workloads. That list is
// inventory.Prioritize's output, built for display: it drops healthy-quiet
// workloads outright and Assemble truncates a Job's or CronJob's pod rows at
// jobPodCap. Answering a lookup against it made inspect report found:false for
// objects the cluster plainly had. Existence comes from res.Inputs, the raw
// snapshot the scan already collected, so this costs no cluster call.
// res.Inventory.Workloads is still read, through findingsOf, for one narrow
// purpose: to recover the findings the scan already attached.
//
// The canonical Kind spelling comes from the recomputed workload, never from the
// object: typed client-go objects leave TypeMeta empty. The requested kind is
// matched case-insensitively, as it has always been — the published enum admits
// only the seven lowercase spellings, but this is a pure function a test may
// call with any string, and it must not answer found:false for "Pod" when the
// caller meant "pod".
func resolveObject(res scan.Result, kind, namespace, name string) resolved {
	for _, w := range inventory.Assemble(res.Inputs, findingsOf(res.Inventory.Workloads)) {
		if w.Namespace != namespace || w.Name != name || !strings.EqualFold(w.Kind, kind) {
			continue
		}
		out := resolved{
			Found: true, Kind: w.Kind, Status: w.Status,
			Desired: w.Desired, Ready: w.Ready, Image: w.Image,
		}
		out.Pods = append(out.Pods, w.Pods...)
		// Mirrors findingsFromResult in view.go: a workload can be
		// Flagged() (Ready < Desired, or a bare Failed pod) with no per-pod
		// diagnose.Finding attached — a Deployment stuck at 0/3 ready with no
		// crash-looping pod, for instance. Projecting only w.Findings would
		// report "findings: []" for an object triage already flags; fromWorkload
		// is the same helper triage uses, so the two surfaces agree.
		if len(w.Findings) == 0 {
			if w.Flagged() {
				out.Findings = append(out.Findings, fromWorkload(w))
			}
		} else {
			for _, f := range w.Findings {
				out.Findings = append(out.Findings, fromDiagnose(f))
			}
		}
		return out
	}
	return resolved{}
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

// sortEvents orders events most-recent-first, so a drill-down leads with what
// just happened rather than whatever order the clientset happened to list
// them in. Ties on timestamp — including two events that share none, which
// eventTime both resolve to the zero time.Time — are broken on Reason, then
// Message, then Count, so the order is total and two scans of an unchanged
// cluster produce byte-identical payloads.
func sortEvents(events []corev1.Event) {
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		ta, tb := eventTime(a), eventTime(b)
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return a.Count < b.Count
	})
}

// eventAge renders an event's age, or the literal "unknown" when the event
// carries none of the three timestamps eventTime looks at. Real events
// reliably set at least one, so this is defensive: without it, the zero
// time.Time would render through inventory.HumanAge as a multi-century age,
// and a nonsense age is worse than an honest "we do not know" — this
// project's rule is that absence must never read as zero.
func eventAge(e corev1.Event, now time.Time) string {
	if e.LastTimestamp.IsZero() && e.EventTime.IsZero() && e.FirstTimestamp.IsZero() {
		return "unknown"
	}
	return inventory.HumanAge(eventTime(e), now)
}
