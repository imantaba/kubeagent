// Package scan runs kubeagent's deterministic evaluation of a cluster — collect,
// diagnose, assemble/prioritize, annotate, assess health and service health —
// and returns the structured result. It is shared by the CLI `scan` command and
// the `watch` daemon. Read-only: only List/Get calls, no writes, no LLM.
package scan

import (
	"context"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
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

// DefaultQuotaThreshold is the fraction of a ResourceQuota's hard limit at
// which the used share is reported as near its limit. It is the default the
// CLI applies to KUBEAGENT_QUOTA_THRESHOLD and the value Evaluate falls back
// to when it is handed a threshold outside (0, 1] — one number, so the two
// can never drift apart.
const DefaultQuotaThreshold = 0.90

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
	// WebhookURLBackends counts the in-scope Fail-policy webhooks backed by a
	// clientConfig.url rather than a Service — a backend this scan cannot check
	// the reachability of, disclosed as a count rather than guessed at as an
	// Issue. Only ever non-zero cluster-wide: it is computed inside the same
	// opts.Namespace == "" guard as WebhookIssues.
	WebhookURLBackends int
	QuotaIssues        []quotahealth.Issue

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
	// One clock for the whole evaluation. Five separate time.Now() calls made
	// "how old is this?" depend on where in the scan the question was asked;
	// with the reads overlapping, it would depend on the schedule too.
	now := time.Now()

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

	// blindWith records a blind spot whose cause is not a permission refusal —
	// an endpoint that did not answer. It shares blindSeen with blind so a
	// resource is still named once, and it deliberately does not go through
	// blindReason, whose "forbidden" prefix would be a false claim here.
	blindWith := func(resource, reason string) {
		if blindSeen[resource] {
			return
		}
		blindSeen[resource] = true
		partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: reason})
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

	// ------------------------------------------------------------------ phase 1
	//
	// Every read that depends on nothing but opts. The closures share no mutable
	// state: each writes only its own destination variable and returns only its
	// own error, so no entry in this list can observe anything another entry
	// does. Nothing here appends to partialReads — blind spots are recorded
	// afterwards, in report order, which is what keeps the rendered output
	// independent of which read answered first.
	var reads []func(context.Context) error
	add := func(f func(context.Context) error) int {
		reads = append(reads, f)
		return len(reads) - 1
	}

	var (
		pods     []corev1.Pod
		deploys  []appsv1.Deployment
		rsets    []appsv1.ReplicaSet
		stses    []appsv1.StatefulSet
		dsets    []appsv1.DaemonSet
		jobs     []batchv1.Job
		cronJobs []batchv1.CronJob
		nodes    []corev1.Node

		attachEvents       []corev1.Event
		mountEvents        []corev1.Event
		unhealthyEvents    []corev1.Event
		pvcEvents          []corev1.Event
		failedCreateEvents []corev1.Event
		leases             []coordinationv1.Lease
		svcs               []corev1.Service
		slices             []discoveryv1.EndpointSlice
		ings               []networkingv1.Ingress
		tlsSecrets         []corev1.Secret
		pvcs               []corev1.PersistentVolumeClaim
		namespaces         []corev1.Namespace
		pdbs               []policyv1.PodDisruptionBudget
		hpas               []autoscalingv2.HorizontalPodAutoscaler
		vwc                []admissionv1.ValidatingWebhookConfiguration
		mwc                []admissionv1.MutatingWebhookConfiguration
		pvs                []corev1.PersistentVolume
		storageClasses     []storagev1.StorageClass
		quotas             []corev1.ResourceQuota
		nps                []networkingv1.NetworkPolicy
	)

	iPods := add(func(ctx context.Context) error {
		var err error
		pods, err = collect.Pods(ctx, client, opts.Namespace)
		return err
	})
	iDeploys := add(func(ctx context.Context) error {
		var err error
		deploys, err = collect.Deployments(ctx, client, opts.Namespace)
		return err
	})
	iRSets := add(func(ctx context.Context) error {
		var err error
		rsets, err = collect.ReplicaSets(ctx, client, opts.Namespace)
		return err
	})
	iSTSes := add(func(ctx context.Context) error {
		var err error
		stses, err = collect.StatefulSets(ctx, client, opts.Namespace)
		return err
	})
	iDSets := add(func(ctx context.Context) error {
		var err error
		dsets, err = collect.DaemonSets(ctx, client, opts.Namespace)
		return err
	})
	iJobs := add(func(ctx context.Context) error {
		var err error
		jobs, err = collect.Jobs(ctx, client, opts.Namespace)
		return err
	})
	iCronJobs := add(func(ctx context.Context) error {
		var err error
		cronJobs, err = collect.CronJobs(ctx, client, opts.Namespace)
		return err
	})
	iNodes := add(func(ctx context.Context) error {
		var err error
		nodes, err = collect.Nodes(ctx, client)
		return err
	})
	iAttach := add(func(ctx context.Context) error {
		var err error
		attachEvents, err = collect.VolumeAttachEvents(ctx, client, opts.Namespace)
		return err
	})
	iMount := add(func(ctx context.Context) error {
		var err error
		mountEvents, err = collect.FailedMountEvents(ctx, client, opts.Namespace)
		return err
	})
	iUnhealthy := add(func(ctx context.Context) error {
		var err error
		unhealthyEvents, err = collect.UnhealthyEvents(ctx, client, opts.Namespace)
		return err
	})
	iLeases := add(func(ctx context.Context) error {
		var err error
		leases, err = collect.NodeLeases(ctx, client)
		return err
	})
	iSvcs := add(func(ctx context.Context) error {
		var err error
		svcs, err = collect.Services(ctx, client, opts.Namespace)
		return err
	})
	iSlices := add(func(ctx context.Context) error {
		var err error
		slices, err = collect.EndpointSlices(ctx, client, opts.Namespace)
		return err
	})
	iIngs := add(func(ctx context.Context) error {
		var err error
		ings, err = collect.Ingresses(ctx, client, opts.Namespace)
		return err
	})
	var iSecrets int
	if opts.Certs {
		iSecrets = add(func(ctx context.Context) error {
			var err error
			tlsSecrets, err = collect.TLSSecrets(ctx, client, opts.Namespace)
			return err
		})
	}
	iPVCs := add(func(ctx context.Context) error {
		var err error
		pvcs, err = collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
		return err
	})
	// forbidden/absent → nil, namespace checks skipped
	iNamespaces := add(func(ctx context.Context) error {
		var err error
		namespaces, err = collect.Namespaces(ctx, client)
		return err
	})
	// forbidden/absent → nil, check skipped
	iPDBs := add(func(ctx context.Context) error {
		var err error
		pdbs, err = collect.PodDisruptionBudgets(ctx, client, opts.Namespace)
		return err
	})
	// forbidden/absent → nil, check skipped
	iHPAs := add(func(ctx context.Context) error {
		var err error
		hpas, err = collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace)
		return err
	})
	var iVWC, iMWC int
	if opts.Namespace == "" { // webhook backends can live in any namespace; only sound cluster-wide
		iVWC = add(func(ctx context.Context) error {
			var err error
			vwc, err = collect.ValidatingWebhookConfigurations(ctx, client)
			return err
		})
		iMWC = add(func(ctx context.Context) error {
			var err error
			mwc, err = collect.MutatingWebhookConfigurations(ctx, client)
			return err
		})
	}
	iPVs := add(func(ctx context.Context) error {
		var err error
		pvs, err = collect.PersistentVolumes(ctx, client)
		return err
	})
	iPVCEvents := add(func(ctx context.Context) error {
		var err error
		pvcEvents, err = collect.PVCEvents(ctx, client, opts.Namespace)
		return err
	})
	iStorageClasses := add(func(ctx context.Context) error {
		var err error
		storageClasses, err = collect.StorageClasses(ctx, client)
		return err
	})
	iQuotas := add(func(ctx context.Context) error {
		var err error
		quotas, err = collect.ResourceQuotas(ctx, client, opts.Namespace)
		return err
	})
	iNPs := add(func(ctx context.Context) error {
		var err error
		nps, err = collect.NetworkPolicies(ctx, client, opts.Namespace)
		return err
	})
	iFailedCreate := add(func(ctx context.Context) error {
		var err error
		failedCreateEvents, err = collect.FailedCreateEvents(ctx, client, opts.Namespace)
		return err
	})

	errs := runReads(ctx, reads)

	// The fatal reads, checked in the order CollectInventory checked them so an
	// unreachable cluster still reports the error it always reported. Every read
	// above has already been issued by the time we get here: stopping the pool at
	// the first fatal error would make which reads completed depend on the
	// schedule, and with it PartialReads.
	for _, i := range []int{iPods, iDeploys, iRSets, iSTSes, iDSets, iJobs, iCronJobs, iNodes} {
		if errs[i] != nil {
			return Result{}, errs[i]
		}
	}
	inputs := inventory.Inputs{
		Pods: pods, Deployments: deploys, ReplicaSets: rsets, StatefulSets: stses,
		DaemonSets: dsets, Jobs: jobs, CronJobs: cronJobs,
	}

	// Pure work that decides what phase 2 reads. Deciding it here, sequentially,
	// is what makes the phase-2 work list independent of the schedule.
	events := make([]corev1.Event, 0, len(attachEvents)+len(mountEvents)+len(unhealthyEvents))
	events = append(events, attachEvents...)
	events = append(events, mountEvents...)
	events = append(events, unhealthyEvents...)
	findings := diagnose.Run(diagnose.DefaultDetectors(now), collect.FactsFrom(inputs.Pods, events))

	type logTarget struct {
		finding   int
		namespace string
		pod       string
		container string
	}
	var logTargets []logTarget
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
			logTargets = append(logTargets, logTarget{finding: i, namespace: ns, pod: name, container: findings[i].Container})
		}
	}

	var cdns []corev1.Pod
	if opts.DNSHealth {
		cdns = coreDNSPods(inputs.Pods)
	}

	// ------------------------------------------------------------------ phase 2
	//
	// The fan-outs, one flat pool: a node's kubelet is no slower to answer than a
	// CoreDNS pod's metrics, so splitting them into separate pools would only add
	// barriers. Same rule as phase 1 — every closure owns its own slot.
	var reads2 []func(context.Context) error
	add2 := func(f func(context.Context) error) int {
		reads2 = append(reads2, f)
		return len(reads2) - 1
	}

	logText := make([]string, len(logTargets))
	logOK := make([]bool, len(logTargets))
	logIdx := make([]int, len(logTargets))
	for k := range logTargets {
		logIdx[k] = add2(func(ctx context.Context) error {
			var err error
			logText[k], logOK[k], err = collect.PreviousLogs(ctx, client, logTargets[k].namespace, logTargets[k].pod, logTargets[k].container)
			return err
		})
	}

	var (
		summaries []diskusage.NodeSummary
		statsOK   []bool
		statsIdx  []int
	)
	if opts.DiskUsage {
		summaries = make([]diskusage.NodeSummary, len(nodes))
		statsOK = make([]bool, len(nodes))
		statsIdx = make([]int, len(nodes))
		for k := range nodes {
			statsIdx[k] = add2(func(ctx context.Context) error {
				var err error
				summaries[k], statsOK[k], err = collect.NodeStats(ctx, client, nodes[k].Name)
				return err
			})
		}
	}

	var probes []nodehealth.Probe
	if opts.KubeletHealth {
		probes = make([]nodehealth.Probe, len(nodes))
		for k := range nodes {
			add2(func(ctx context.Context) error {
				probes[k] = collect.KubeletHealthz(ctx, client, nodes[k].Name)
				return nil
			})
		}
	}

	var readyz controlplane.Probe
	if opts.ControlPlaneHealth {
		add2(func(ctx context.Context) error {
			readyz = collect.ControlPlaneReadyz(ctx, client)
			return nil
		})
	}

	dnsBody := make([][]byte, len(cdns))
	dnsCode := make([]int, len(cdns))
	for k := range cdns {
		add2(func(ctx context.Context) error {
			dnsBody[k], dnsCode[k] = collect.CoreDNSMetrics(ctx, client, cdns[k].Namespace, cdns[k].Name)
			return nil
		})
	}

	errs2 := runReads(ctx, reads2)

	// ------------------------------------------------------- the report order
	//
	// Every blind spot and every read failure is recorded here, in this fixed
	// order, after both pools have finished. Nothing above appends to
	// partialReads, so the rendered order is a property of this block and not of
	// which read answered first. The numbers match the report-order table in
	// docs/superpowers/specs/2026-07-30-bounded-scan-concurrency-design.md.
	note("events", errs[iAttach])    // 1
	note("events", errs[iMount])     // 1, same resource
	note("events", errs[iUnhealthy]) // 2
	for k := range logTargets {      // 3
		if err := errs2[logIdx[k]]; apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			blind("pods/log", "get pods/log")
		}
		if logOK[k] {
			if clue := logscan.Classify(logText[k]); clue.Cause != "" {
				findings[logTargets[k].finding].LogCause = clue.Cause
				findings[logTargets[k].finding].LogExcerpt = clue.Excerpt
			}
		}
	}
	note("leases", errs[iLeases])         // 4
	note("services", errs[iSvcs])         // 5
	note("endpointslices", errs[iSlices]) // 6
	note("ingresses", errs[iIngs])        // 7

	var certReport *certhealth.Report // 8
	if opts.Certs {
		warn := opts.CertWarnDays
		if warn < 0 {
			// Defence-in-depth only: internal/cli/scan.go refuses a negative
			// --cert-warn-days before Evaluate is ever called, so the CLI can
			// no longer reach this branch. 0 is a real window meaning
			// "expired only" and is passed through unchanged, not clamped to
			// the 30-day default.
			warn = 30
		}
		rep := certhealth.Assess(tlsSecrets, ings, warn, now)
		if err := errs[iSecrets]; apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			rep.Forbidden = true
			blind("secrets", "list secrets")
		} else {
			note("secrets", err)
		}
		certReport = &rep
	}

	note("persistentvolumeclaims", errs[iPVCs])   // 9
	note("namespaces", errs[iNamespaces])         // 10
	note("poddisruptionbudgets", errs[iPDBs])     // 11
	note("horizontalpodautoscalers", errs[iHPAs]) // 12
	if opts.Namespace == "" {
		note("validatingwebhookconfigurations", errs[iVWC]) // 13
		note("mutatingwebhookconfigurations", errs[iMWC])   // 14
	}
	note("persistentvolumes", errs[iPVs])         // 15
	note("events", errs[iPVCEvents])              // 16
	note("storageclasses", errs[iStorageClasses]) // 17
	note("resourcequotas", errs[iQuotas])         // 18
	note("networkpolicies", errs[iNPs])           // 19
	note("events", errs[iFailedCreate])           // 20

	var diskReport diskusage.Report // 21
	if opts.DiskUsage {
		var kept []diskusage.NodeSummary
		for k := range nodes {
			if err := errs2[statsIdx[k]]; err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					blind("nodes/proxy", "get nodes/proxy")
				}
				continue // an unreachable kubelet is a node problem, not a grant problem
			}
			if statsOK[k] {
				kept = append(kept, summaries[k])
			}
		}
		diskReport = diskusage.Assess(kept, opts.DiskThreshold)
	}

	var kubeletHealth nodehealth.Report // 22
	if opts.KubeletHealth {
		kubeletHealth = nodehealth.Assess(probes)
		// NOTE: the blind spot fires on any refusal while report.printKubeletHealth
		// prints its grant hint only when every probe was refused. nodes/proxy is
		// cluster-scoped, so a partial refusal has not been observed; if one ever is,
		// gate both on Forbidden > 0 and say how many nodes were refused.
		if kubeletHealth.Forbidden > 0 {
			blind("nodes/proxy", "get nodes/proxy")
		}
	}

	if opts.ControlPlaneHealth && readyz.Status == "forbidden" { // 23
		blind("/readyz", "get /readyz")
	}
	if opts.ControlPlaneHealth && readyz.Status == "unreachable" {
		blindWith("/readyz", "kubeagent could not reach the apiserver /readyz endpoint")
	}

	var dnsReport dnshealth.Report // 24
	if opts.DNSHealth {
		ratio := opts.DNSServfailRatio
		if ratio <= 0 || ratio > 1 {
			ratio = 0.05
		}
		agg := map[string]int64{}
		answered, forbidden, unreachable := 0, 0, 0
		for k := range cdns {
			switch {
			case dnsCode[k] == 401 || dnsCode[k] == 403:
				forbidden++
			case dnsCode[k] == 200:
				answered++
				for rc, n := range dnshealth.ParseResponses(dnsBody[k]) {
					agg[rc] += n
				}
			default:
				unreachable++
			}
		}
		if forbidden > 0 {
			blind("pods/proxy", "get pods/proxy")
		}
		dnsReport = dnshealth.Assess(agg, len(cdns), answered, forbidden, unreachable, ratio, 100)
		if dnsReport.Status == "unreachable" {
			blindWith("pods/proxy", "kubeagent could not reach the CoreDNS :9153/metrics endpoint")
		}
	}

	// ------------------------------------------------ pure: no reads past here
	workloads := inventory.Assemble(inputs, findings)
	batchhealth.Annotate(workloads, inputs.Jobs)

	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: now, Threshold: opts.NodeHeartbeatThreshold, Unavailable: errs[iLeases] != nil}, opts.ExpectedNodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)
	svchealth.AnnotateEndpointCause(serviceIssues, svcs, inputs.Pods, health.DownNodes)
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends, inputs.Pods, health.DownNodes)

	var securityIssues []secscan.Finding
	if opts.Security {
		p, s := inputs.Pods, svcs
		if opts.Namespace == "" {
			p = nonSystemPods(p)
			s = nonSystemServices(s)
		}
		securityIssues = secscan.Assess(p, s, inputs.ReplicaSets)
	}

	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, nodes, terminatingThreshold(), now)
	pdbIssues := pdbhealth.Assess(pdbs)
	hpaIssues := hpahealth.Assess(hpas)

	var webhookIssues []webhookhealth.Issue
	var webhookURLBackends int
	if opts.Namespace == "" {
		webhookThreshold := opts.WebhookTimeoutThreshold
		if webhookThreshold <= 0 {
			webhookThreshold = 15
		}
		webhookIssues, webhookURLBackends = webhookhealth.Assess(vwc, mwc, svcs, slices, webhookThreshold)
	}

	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents, storageClasses, pvs, 10*time.Minute, now)

	// The CLI already refuses an out-of-range KUBEAGENT_QUOTA_THRESHOLD and
	// says so. This clamp is for every other caller: Evaluate must stay safe
	// on a zero-value Options, which no flag parser ever touched.
	quotaThreshold := opts.QuotaThreshold
	if quotaThreshold <= 0 || quotaThreshold > 1 {
		quotaThreshold = DefaultQuotaThreshold
	}
	quotaIssues := quotahealth.Assess(quotas, quotaThreshold)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

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
	createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
	rollouthealth.Annotate(result.Workloads, inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Pods, now)
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, now)
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotatePVC(result.Workloads, podPVCs, pvcIssues)
	rootcause.AnnotateRegistry(result.Workloads)
	confidence.Annotate(result.Workloads)

	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, DiskUsage: diskReport, Health: health, Inventory: result, ServiceIssues: serviceIssues, IngressIssues: ingressIssues, PVCIssues: pvcIssues, SecurityIssues: securityIssues, KubeletHealth: kubeletHealth, ControlPlane: readyz, DNS: dnsReport, Certificates: certReport, StuckTerminating: stuckTerminating, PDBIssues: pdbIssues, HPAIssues: hpaIssues, WebhookIssues: webhookIssues, WebhookURLBackends: webhookURLBackends, QuotaIssues: quotaIssues, PartialReads: partialReads}, nil
}
