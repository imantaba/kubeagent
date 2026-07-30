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
	"github.com/imantaba/kubeagent/internal/certhealth"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/confidence"
	"github.com/imantaba/kubeagent/internal/controlplane"
	"github.com/imantaba/kubeagent/internal/createhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/diskusage"
	"github.com/imantaba/kubeagent/internal/dnshealth"
	"github.com/imantaba/kubeagent/internal/hpahealth"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/logscan"
	"github.com/imantaba/kubeagent/internal/netpolicy"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pdbhealth"
	"github.com/imantaba/kubeagent/internal/pvchealth"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/quotahealth"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/rollout"
	"github.com/imantaba/kubeagent/internal/rollouthealth"
	"github.com/imantaba/kubeagent/internal/rootcause"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/termhealth"
	"github.com/imantaba/kubeagent/internal/webhookhealth"
)

// Options controls the evaluation scope.
type Options struct {
	Namespace               string
	IncludeCron             bool
	IncludeRestarts         bool
	DiskUsage               bool
	DiskThreshold           float64
	QuotaThreshold          float64
	Certs                   bool
	CertWarnDays            int
	Security                bool
	NodeHeartbeatThreshold  time.Duration
	ExpectedNodes           []string
	KubeletHealth           bool
	ControlPlaneHealth      bool
	DNSHealth               bool
	DNSServfailRatio        float64
	Logs                    bool
	WebhookTimeoutThreshold int32
}

// ReadFailure records a collector call that failed. A scan degrades rather
// than aborting when an optional list is denied, so without this record an
// RBAC-denied list and a genuinely empty one produce the same output.
type ReadFailure struct {
	Resource string
	Reason   string
}

// Result is the structured health picture. Inputs and Nodes are exposed so the
// CLI can compose its extra views (resource summary, platform facts, credential
// lint, --fix) without re-collecting.
type Result struct {
	Inputs           inventory.Inputs
	Nodes            []corev1.Node
	NodeReserve      nodereserve.Report
	PVCReclaim       pvcreclaim.Report
	DiskUsage        diskusage.Report
	Health           clusterhealth.ClusterHealth
	Inventory        inventory.Result
	ServiceIssues    []svchealth.Issue
	IngressIssues    []ingresshealth.RouteIssue
	PVCIssues        []pvchealth.Issue
	SecurityIssues   []secscan.Finding
	KubeletHealth    nodehealth.Report
	ControlPlane     controlplane.Probe
	DNS              dnshealth.Report
	Certificates     *certhealth.Report
	StuckTerminating []termhealth.Issue
	PDBIssues        []pdbhealth.Issue
	HPAIssues        []hpahealth.Issue
	WebhookIssues    []webhookhealth.Issue
	QuotaIssues      []quotahealth.Issue

	// PartialReads names the collector calls that failed. Empty means every
	// list this scan attempted answered successfully.
	PartialReads []ReadFailure
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

// coreDNSPods returns the Running CoreDNS pods (kube-system, k8s-app=kube-dns).
func coreDNSPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Namespace == "kube-system" && p.Labels["k8s-app"] == "kube-dns" && p.Status.Phase == corev1.PodRunning {
			out = append(out, p)
		}
	}
	return out
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

// blindReason phrases a refused read. The leading "forbidden" is load-bearing:
// internal/htmlreport.safeReason classifies by substring, and a reason without
// it is rendered as the generic "the read failed" line.
func blindReason(action string) string {
	return "forbidden: kubeagent's credentials may not " + action
}

// Evaluate performs the read-only evaluation. The returned error is the raw
// collection error (callers may wrap it with connectivity.Diagnose).
func Evaluate(ctx context.Context, client kubernetes.Interface, opts Options) (Result, error) {
	var partialReads []ReadFailure

	// blind records a blind spot in kubeagent's own words. The reason always
	// starts with "forbidden" so internal/htmlreport.safeReason classifies it as
	// a permission problem rather than degrading it to a generic phrase — and so
	// it never carries the API server's message, which names the requesting
	// identity.
	blindSeen := map[string]bool{}
	blind := func(resource, action string) {
		if blindSeen[resource] {
			return // one line per feature, not one per node
		}
		blindSeen[resource] = true
		partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: blindReason(action)})
	}

	// A refusal is reported in kubeagent's own words. The API server's message
	// interpolates the authorizer's error, which names the requesting identity — a
	// ServiceAccount, an IAM ARN, an OIDC email — and under webhook authorization
	// carries arbitrary third-party text. Everything else keeps the redacted error,
	// which is what makes an unreachable API server distinguishable from a refused one.
	note := func(resource string, err error) {
		switch {
		case err == nil:
			return
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			blind(resource, "read "+resource)
		default:
			partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: redact.Error(err)})
		}
	}

	inputs, err := collect.CollectInventory(ctx, client, opts.Namespace)
	if err != nil {
		return Result{}, err
	}

	detectors := diagnose.DefaultDetectors(time.Now())
	attachEvents, attachErr := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
	note("events", attachErr)
	unhealthyEvents, unhealthyErr := collect.UnhealthyEvents(ctx, client, opts.Namespace)
	note("events", unhealthyErr)
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
			log, ok, logErr := collect.PreviousLogs(ctx, client, ns, name, findings[i].Container)
			if apierrors.IsForbidden(logErr) || apierrors.IsUnauthorized(logErr) {
				blind("pods/log", "get pods/log")
			}
			if ok {
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
	leases, leasesErr := collect.NodeLeases(ctx, client)
	note("leases", leasesErr)
	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: time.Now(), Threshold: opts.NodeHeartbeatThreshold}, opts.ExpectedNodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	svcs, svcsErr := collect.Services(ctx, client, opts.Namespace)
	note("services", svcsErr)
	slices, slicesErr := collect.EndpointSlices(ctx, client, opts.Namespace)
	note("endpointslices", slicesErr)
	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)
	svchealth.AnnotateEndpointCause(serviceIssues, svcs, inputs.Pods, health.DownNodes)
	ings, ingsErr := collect.Ingresses(ctx, client, opts.Namespace)
	note("ingresses", ingsErr)
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends, inputs.Pods, health.DownNodes)

	var certReport *certhealth.Report
	if opts.Certs {
		warn := opts.CertWarnDays
		if warn <= 0 {
			warn = 30
		}
		tlsSecrets, tlsErr := collect.TLSSecrets(ctx, client, opts.Namespace)
		rep := certhealth.Assess(tlsSecrets, ings, warn, time.Now())
		if apierrors.IsForbidden(tlsErr) || apierrors.IsUnauthorized(tlsErr) {
			rep.Forbidden = true
			blind("secrets", "list secrets")
		} else {
			note("secrets", tlsErr)
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

	pvcs, pvcsErr := collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
	note("persistentvolumeclaims", pvcsErr)
	namespaces, namespacesErr := collect.Namespaces(ctx, client) // forbidden/absent → nil, namespace checks skipped
	note("namespaces", namespacesErr)
	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, 2*time.Minute, time.Now())
	pdbs, pdbsErr := collect.PodDisruptionBudgets(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	note("poddisruptionbudgets", pdbsErr)
	pdbIssues := pdbhealth.Assess(pdbs)
	hpas, hpasErr := collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace) // forbidden/absent → nil, check skipped
	note("horizontalpodautoscalers", hpasErr)
	hpaIssues := hpahealth.Assess(hpas)
	var webhookIssues []webhookhealth.Issue
	if opts.Namespace == "" { // webhook backends can live in any namespace; only sound cluster-wide
		vwc, vwcErr := collect.ValidatingWebhookConfigurations(ctx, client)
		note("validatingwebhookconfigurations", vwcErr)
		mwc, mwcErr := collect.MutatingWebhookConfigurations(ctx, client)
		note("mutatingwebhookconfigurations", mwcErr)
		webhookThreshold := opts.WebhookTimeoutThreshold
		if webhookThreshold <= 0 {
			webhookThreshold = 15
		}
		webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices, webhookThreshold)
	}
	pvs, pvsErr := collect.PersistentVolumes(ctx, client)
	note("persistentvolumes", pvsErr)
	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)
	pvcEvents, pvcEventsErr := collect.PVCEvents(ctx, client, opts.Namespace)
	note("events", pvcEventsErr)
	storageClasses, storageClassesErr := collect.StorageClasses(ctx, client)
	note("storageclasses", storageClassesErr)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents, storageClasses, pvs)

	quotaThreshold := opts.QuotaThreshold
	if quotaThreshold <= 0 || quotaThreshold > 1 {
		quotaThreshold = 0.90
	}
	quotas, quotasErr := collect.ResourceQuotas(ctx, client, opts.Namespace)
	note("resourcequotas", quotasErr)
	quotaIssues := quotahealth.Assess(quotas, quotaThreshold)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

	nps, npsErr := collect.NetworkPolicies(ctx, client, opts.Namespace)
	note("networkpolicies", npsErr)
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
	failedCreateEvents, failedCreateErr := collect.FailedCreateEvents(ctx, client, opts.Namespace)
	note("events", failedCreateErr)
	createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
	rollouthealth.Annotate(result.Workloads, inputs.Deployments)
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
			s, ok, err := collect.NodeStats(ctx, client, n.Name)
			if err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					blind("nodes/proxy", "get nodes/proxy")
				}
				continue // an unreachable kubelet is a node problem, not a grant problem
			}
			if ok {
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
		if kubeletHealth.Forbidden > 0 {
			blind("nodes/proxy", "get nodes/proxy")
		}
	}

	var controlPlane controlplane.Probe
	if opts.ControlPlaneHealth {
		controlPlane = collect.ControlPlaneReadyz(ctx, client)
		if controlPlane.Status == "forbidden" {
			blind("/readyz", "get /readyz")
		}
	}

	var dnsReport dnshealth.Report
	if opts.DNSHealth {
		ratio := opts.DNSServfailRatio
		if ratio <= 0 || ratio > 1 {
			ratio = 0.05
		}
		cdns := coreDNSPods(inputs.Pods)
		agg := map[string]int64{}
		forbidden, unreachable := 0, 0
		for _, p := range cdns {
			body, code := collect.CoreDNSMetrics(ctx, client, p.Namespace, p.Name)
			switch {
			case code == 401 || code == 403:
				forbidden++
			case code == 200:
				for rc, n := range dnshealth.ParseResponses(body) {
					agg[rc] += n
				}
			default:
				unreachable++
			}
		}
		if forbidden > 0 {
			blind("pods/proxy", "get pods/proxy")
		}
		dnsReport = dnshealth.Assess(agg, len(cdns), forbidden, unreachable, ratio, 100)
	}

	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, DiskUsage: diskReport, Health: health, Inventory: result, ServiceIssues: serviceIssues, IngressIssues: ingressIssues, PVCIssues: pvcIssues, SecurityIssues: securityIssues, KubeletHealth: kubeletHealth, ControlPlane: controlPlane, DNS: dnsReport, Certificates: certReport, StuckTerminating: stuckTerminating, PDBIssues: pdbIssues, HPAIssues: hpaIssues, WebhookIssues: webhookIssues, QuotaIssues: quotaIssues, PartialReads: partialReads}, nil
}
