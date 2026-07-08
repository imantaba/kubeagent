// Package scan runs kubeagent's deterministic evaluation of a cluster — collect,
// diagnose, assemble/prioritize, annotate, assess health and service health —
// and returns the structured result. It is shared by the CLI `scan` command and
// the `watch` daemon. Read-only: only List/Get calls, no writes, no LLM.
package scan

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/netpolicy"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/rollout"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// Options controls the evaluation scope.
type Options struct {
	Namespace       string
	IncludeCron     bool
	IncludeRestarts bool
}

// Result is the structured health picture. Inputs and Nodes are exposed so the
// CLI can compose its extra views (resource summary, platform facts, credential
// lint, --fix) without re-collecting.
type Result struct {
	Inputs        inventory.Inputs
	Nodes         []corev1.Node
	NodeReserve   nodereserve.Report
	PVCReclaim    pvcreclaim.Report
	Health        clusterhealth.ClusterHealth
	Inventory     inventory.Result
	ServiceIssues []svchealth.Issue
}

// Evaluate performs the read-only evaluation. The returned error is the raw
// collection error (callers may wrap it with connectivity.Diagnose).
func Evaluate(ctx context.Context, client kubernetes.Interface, opts Options) (Result, error) {
	inputs, err := collect.CollectInventory(ctx, client, opts.Namespace)
	if err != nil {
		return Result{}, err
	}

	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
		diagnose.VolumeAttachDetector{},
		diagnose.RestartLoopDetector{Now: time.Now()},
	}
	attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, attachEvents))
	workloads := inventory.Assemble(inputs, findings)

	nodes, err := collect.Nodes(ctx, client)
	if err != nil {
		return Result{}, err
	}
	health := clusterhealth.Assess(nodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	svcs, _ := collect.Services(ctx, client, opts.Namespace)
	slices, _ := collect.EndpointSlices(ctx, client, opts.Namespace)
	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)

	pvcs, _ := collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
	pvs, _ := collect.PersistentVolumes(ctx, client)
	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

	nps, _ := collect.NetworkPolicies(ctx, client, opts.Namespace)
	podLabels := make(map[string]map[string]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		podLabels[p.Namespace+"/"+p.Name] = p.Labels
	}
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())

	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, Health: health, Inventory: result, ServiceIssues: serviceIssues}, nil
}
