// Package scan runs kubeagent's deterministic evaluation of a cluster — collect,
// diagnose, assemble/prioritize, annotate, assess health and service health —
// and returns the structured result. It is shared by the CLI `scan` command and
// the `watch` daemon. Read-only: only List/Get calls, no writes, no LLM.
package scan

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/batchhealth"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/confidence"
	"github.com/imantaba/kubeagent/internal/createhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/logscan"
	"github.com/imantaba/kubeagent/internal/netpolicy"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/rollout"
	"github.com/imantaba/kubeagent/internal/rootcause"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
)

// Options controls the evaluation scope.
type Options struct {
	Namespace              string
	IncludeCron            bool
	IncludeRestarts        bool
	DiskUsage              bool
	DiskThreshold          float64
	Certs                  bool
	CertWarnDays           int
	Security               bool
	NodeHeartbeatThreshold time.Duration
	ExpectedNodes          []string
	KubeletHealth          bool
	Logs                   bool
}

// Result is the structured health picture. Inputs and Nodes are exposed so the
// CLI can compose its extra views (resource summary, platform facts, credential
// lint, --fix) without re-collecting.
type Result struct {
	Inputs         inventory.Inputs
	Nodes          []corev1.Node
	NodeReserve    nodereserve.Report
	PVCReclaim     pvcreclaim.Report
	DiskUsage      diskusage.Report
	Health         clusterhealth.ClusterHealth
	Inventory      inventory.Result
	ServiceIssues  []svchealth.Issue
	IngressIssues  []ingresshealth.RouteIssue
	PVCIssues      []pvchealth.Issue
	SecurityIssues []secscan.Finding
	KubeletHealth     nodehealth.Report
	Certificates      *certhealth.Report
	StuckTerminating  []termhealth.Issue
	PDBIssues         []pdbhealth.Issue
	HPAIssues         []hpahealth.Issue
}

// systemNamespaces are excluded from the security scan when scanning all
// namespaces: their workloads (CNI, kube-proxy, …) are legitimately privileged.
var systemNamespaces = map[string]bool{"kube-system": true, "kube-node-lease": true, "kube-public": true}

func nonSystemPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if !systemNamespaces[p.Namespace] {
			out = append(out, p)
		}
	}
	return out
}

func splitNamespacedName(s string) (ns, name string, ok bool) {
	if i := strings.IndexByte(s, '/'); i > 0 && i < len(s)-1 {
		return s[:i], s[i+1:], true
	}
	return "", "", false
}

func nonSystemServices(svcs []corev1.Service) []corev1.Service {
	var out []corev1.Service
	for _, s := range svcs {
		if !systemNamespaces[s.Namespace] {
			out = append(out, s)
		}
	}
	return out
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
		diagnose.InitContainerDetector{},
		diagnose.ProbeFailureDetector{},
	}
	attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	unhealthyEvents, _ := collect.UnhealthyEvents(ctx, client, opts.Namespace)
	events := append(attachEvents, unhealthyEvents...)
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, events))
	if opts.Logs {
		enriched := map[string]bool{} // one log fetch + one enriched finding per pod/container
		for i := range findings {
			if findings[i].Container == "" {
				continue
			}
			key := findings[i].Pod + "/" + findings[i].Container
			if enriched[key] {
				continue // a container that trips two detectors (e.g. CrashLoop + OOM) is enriched once
			}
			ns, name, ok := splitNamespacedName(findings[i].Pod) // "ns/pod"
			if !ok {
				continue
			}
			enriched[key] = true
			if log, ok := collect.PreviousLogs(ctx, client, ns, name, findings[i].Container); ok {
				clue := logscan.Classify(log)
				if clue.Cause != "" {
					findings[i].LogCause = clue.Cause
					findings[i].LogExcerpt = clue.Excerpt
				}
			}
		}
	}
	workloads := inventory.Assemble(inputs, findings)
	batchhealth.Annotate(workloads, inputs.Jobs)

	nodes, err := collect.Nodes(ctx, client)
	if err != nil {
		return Result{}, err
	}
	leases, _ := collect.NodeLeases(ctx, client)
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: time.Now(), Threshold: opts.NodeHeartbeatThreshold}, opts.ExpectedNodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	svcs, _ := collect.Services(ctx, client, opts.Namespace)
	slices, _ := collect.EndpointSlices(ctx, client, opts.Namespace)
	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)
	ings, _ := collect.Ingresses(ctx, client, opts.Namespace)
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends)

	var certReport *certhealth.Report
	if opts.Certs {
		warn := opts.CertWarnDays
		if warn <= 0 {
			warn = 30
		}
		tlsSecrets, tlsErr := collect.TLSSecrets(ctx, client, opts.Namespace)
		rep := certhealth.Assess(tlsSecrets, ings, warn, time.Now())
		if apierrors.IsForbidden(tlsErr) {
			rep.Forbidden = true
		}
		certReport = &rep
	}

	var securityIssues []secscan.Finding
	if opts.Security {
		pods, services := inputs.Pods, svcs
		if opts.Namespace == "" {
			pods = nonSystemPods(pods)
			services = nonSystemServices(services)
		}
		securityIssues = secscan.Assess(pods, services, inputs.ReplicaSets)
	}

	pvcs, _ := collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
	namespaces, _ := collect.Namespaces(ctx, client) // forbidden/absent → nil, namespace checks skipped
	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, 2*time.Minute, time.Now())
	pdbs, _ := collect.PodDisruptionBudgets(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	pdbIssues := pdbhealth.Assess(pdbs)
	hpas, _ := collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	hpaIssues := hpahealth.Assess(hpas)
	pvs, _ := collect.PersistentVolumes(ctx, client)
	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)
	pvcEvents, _ := collect.PVCEvents(ctx, client, opts.Namespace)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

	nps, _ := collect.NetworkPolicies(ctx, client, opts.Namespace)
	podLabels := make(map[string]map[string]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		podLabels[p.Namespace+"/"+p.Name] = p.Labels
	}
	podPVCs := make(map[string][]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + p.Name
				podPVCs[key] = append(podPVCs[key], v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	failedCreateEvents, _ := collect.FailedCreateEvents(ctx, client, opts.Namespace)
	createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotatePVC(result.Workloads, podPVCs, pvcIssues)
	rootcause.AnnotateRegistry(result.Workloads)
	confidence.Annotate(result.Workloads)

	var diskReport diskusage.Report
	if opts.DiskUsage {
		var summaries []diskusage.NodeSummary
		for _, n := range nodes {
			if s, ok, _ := collect.NodeStats(ctx, client, n.Name); ok {
				summaries = append(summaries, s)
			}
		}
		diskReport = diskusage.Assess(summaries, opts.DiskThreshold)
	}

	var kubeletHealth nodehealth.Report
	if opts.KubeletHealth {
		var probes []nodehealth.Probe
		for _, n := range nodes {
			probes = append(probes, collect.KubeletHealthz(ctx, client, n.Name))
		}
		kubeletHealth = nodehealth.Assess(probes)
	}

	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, DiskUsage: diskReport, Health: health, Inventory: result, ServiceIssues: serviceIssues, IngressIssues: ingressIssues, PVCIssues: pvcIssues, SecurityIssues: securityIssues, KubeletHealth: kubeletHealth, Certificates: certReport, StuckTerminating: stuckTerminating, PDBIssues: pdbIssues, HPAIssues: hpaIssues}, nil
}
