// Package advisory runs kubeagent's optional advisory sections — operator
// health, GitOps drift and capacity — and reports what it could not run.
//
// The sections are opt-in and each depends on API access the core scan does
// not need: CRDs for operators and drift, metrics-server for capacity. When
// that access is missing the section is skipped, and a skipped section that
// simply vanishes from the output is indistinguishable from a section that
// found nothing. Assess therefore returns a Degradation for every section it
// could not fully run, so the CLI can print a warning and the MCP server can
// put the same fact in its coverage block.
package advisory

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/gitops"
	"github.com/imantaba/kubeagent/internal/operators"
	"github.com/imantaba/kubeagent/internal/redact"
)

// Degradation records an advisory section that could not run, or could only
// run partially. Sections holds machine-readable section names; Subject is the
// phrase a human-facing warning uses; Reason is already redacted.
type Degradation struct {
	Sections []string
	Subject  string
	Reason   string
}

// Options selects which advisory sections to run.
type Options struct {
	Operators bool
	Drift     bool
	DriftAge  time.Duration
	Capacity  bool
	Namespace string
}

// Inputs carries the workload objects the scan already listed, so the advisory
// sections re-use them instead of listing again.
type Inputs struct {
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
	Jobs         []batchv1.Job
	CronJobs     []batchv1.CronJob
	ReplicaSets  []appsv1.ReplicaSet
	Nodes        []corev1.Node
	Pods         []corev1.Pod
}

// Result carries one pointer per section — nil means the section did not run —
// plus the reasons for anything missing.
type Result struct {
	Operators *operators.Report
	GitOps    *gitops.Report
	Capacity  *capacity.Report
	// MetricsAvailable reports whether metrics-server answered. The capacity
	// report is still produced without it, from requests and limits alone, so
	// this flag is the only way a consumer can tell a headroom figure backed
	// by real usage from one backed by declared requests.
	MetricsAvailable bool
	Degradations     []Degradation
}

// DynFactory builds the dynamic and discovery clients the CRD-reading sections
// need. It is a function rather than a pair of clients so that a scan with no
// advisory section enabled never builds them, and so tests can fail them.
type DynFactory func() (dynamic.Interface, discovery.DiscoveryInterface, error)

// FlagNames renders the enabled CRD-reading flags for a human-facing warning.
func FlagNames(operators, drift bool) string {
	switch {
	case operators && drift:
		return "--operators/--drift"
	case operators:
		return "--operators"
	default:
		return "--drift"
	}
}

// Assess runs the enabled advisory sections.
func Assess(ctx context.Context, client kubernetes.Interface, dyn DynFactory, in Inputs, opts Options, now time.Time) Result {
	var res Result

	if opts.Operators || opts.Drift {
		sections := []string{}
		if opts.Operators {
			sections = append(sections, "operators")
		}
		if opts.Drift {
			sections = append(sections, "drift")
		}
		dynClient, disco, err := dyn()
		if err != nil {
			res.Degradations = append(res.Degradations, Degradation{
				Sections: sections,
				Subject:  FlagNames(opts.Operators, opts.Drift),
				Reason:   redact.Error(err),
			})
		} else {
			adapters := gitops.Adapters()
			if opts.Operators {
				adapters = operators.Adapters()
			}
			fetched := collect.OperatorResources(ctx, disco, dynClient, adapters, opts.Namespace)
			if opts.Operators {
				rep := operators.Assess(fetched)
				res.Operators = &rep
			}
			if opts.Drift {
				rep := gitops.Assess(fetched, now, opts.DriftAge)
				res.GitOps = &rep
			}
		}
	}

	if opts.Capacity {
		// collect.PodMetrics reports an absent or forbidden metrics-server
		// through its second return value, not an error: that case is normal
		// and non-fatal. A non-nil error here means the response was
		// unparseable, which is worth warning about.
		podUsage, available, err := collect.PodMetrics(ctx, client)
		res.MetricsAvailable = available
		if err != nil {
			res.Degradations = append(res.Degradations, Degradation{
				Sections: []string{"capacity"},
				Subject:  "pod metrics",
				Reason:   redact.Error(err),
			})
		}
		templates := capacity.Templates(in.Deployments, in.StatefulSets, in.DaemonSets, in.Jobs, in.CronJobs)
		rep := capacity.Assess(in.Nodes, in.Pods, in.ReplicaSets, templates, podUsage, opts.Namespace)
		res.Capacity = &rep
	}

	return res
}

// ClusterPods returns the pods capacity headroom should be computed from. A
// namespaced scan still needs every pod in the cluster, because headroom is a
// node-level fact: computing it from one namespace's pods would report the
// other namespaces' consumption as free. When the cluster-wide list fails it
// falls back to the scoped pods and returns the error so the caller can say so.
func ClusterPods(ctx context.Context, client kubernetes.Interface, namespace string, scoped []corev1.Pod) ([]corev1.Pod, error) {
	if namespace == "" {
		return scoped, nil
	}
	all, err := collect.AllPods(ctx, client)
	if err != nil {
		return scoped, err
	}
	return all, nil
}
