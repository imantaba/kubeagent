// Package gitops answers one question about a cluster reconciled by Argo CD or
// Flux: is it still converging on Git, and if not, for how long? Pure: no
// Kubernetes client, no I/O, unit-tested with fixture objects.
//
// This is never a comparison against Git. kubeagent clones no repository, talks
// to no Git host, and renders no manifest. Every signal is read from the
// reconciler's own status, so "drift" here means the reconciler itself says it
// has not converged.
//
// Advisory only, like internal/operators: a reconciler's opinion of itself, read
// through field paths kubeagent infers, must not drive kubeagent's headline
// verdict.
//
// This package is also a boundary the raw objects do not cross. No spec string,
// no condition message, and no unredacted revision reaches a Workload: an Argo CD
// Application's spec.source.repoURL can carry a token, a condition message
// routinely embeds URLs, and Flux publishes revisions as "<ref>@sha1:<hash>"
// where <ref> is arbitrary user text.
package gitops

import (
	"sort"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/operators"
)

// State is one reconciled object's convergence, as its own reconciler reports it.
type State string

const (
	StateSynced  State = "synced"  // the reconciler reports it has converged
	StatePending State = "pending" // differs, younger than the threshold, can self-heal
	StateStale   State = "stale"   // has differed for longer than the threshold
	StateBlocked State = "blocked" // cannot self-heal at any age
	StateUnknown State = "unknown" // no usable signal
)

// severity orders enumeration worst-first so the per-kind cap drops the least
// interesting rows rather than an arbitrary alphabetical tail.
func (s State) severity() int {
	switch s {
	case StateBlocked:
		return 0
	case StateStale:
		return 1
	case StatePending:
		return 2
	default:
		return 3
	}
}

// Workload is one reconciled object, reduced to what the report may show.
type Workload struct {
	Reconciler string `json:"reconciler"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	State      State  `json:"state"`
	Detail     string `json:"detail,omitempty"` // short, single line, already redacted
}

// KindReport is one reconciled kind's roll-up.
type KindReport struct {
	Kind       string        `json:"kind"`
	APIVersion string        `json:"apiVersion"`
	Counts     map[State]int `json:"counts"`            // exact, never truncated
	Drifted    []Workload    `json:"drifted,omitempty"` // every non-synced object, capped at MaxPerKind
	Truncated  int           `json:"truncated,omitempty"`
	Forbidden  bool          `json:"forbidden,omitempty"`
	Error      string        `json:"error,omitempty"` // any other list failure, redacted
}

// Total is the number of objects counted for this kind.
func (k KindReport) Total() int {
	n := 0
	for _, c := range k.Counts {
		n += c
	}
	return n
}

// ReconcilerReport is one reconciler's roll-up across its kinds.
type ReconcilerReport struct {
	Reconciler  string       `json:"reconciler"`
	APIVersions []string     `json:"apiVersions,omitempty"`
	Kinds       []KindReport `json:"kinds,omitempty"`
}

// Report is the whole advisory view. Empty when neither reconciler is installed.
type Report struct {
	Threshold   string             `json:"threshold"` // human form of --drift-age, e.g. "1h"
	Reconcilers []ReconcilerReport `json:"reconcilers,omitempty"`
}

// MaxPerKind bounds how many non-synced objects one kind enumerates. An Argo CD
// estate can hold thousands of Applications: counts stay exact, the printed list
// does not, and the remainder is reported rather than dropped.
const MaxPerKind = 20

// assessment is one object's verdict before it becomes a Workload.
type assessment struct {
	State  State
	Detail string
}

// assessor evaluates one object of one kind.
type assessor func(obj map[string]any, now time.Time, threshold time.Duration) assessment

// Assess reduces each fetched adapter result to drift states and counts.
//
// Non-GitOps kinds are ignored rather than rejected: when a scan runs both
// --operators and --drift, main.go lists the cluster once with the operator
// adapter superset and hands the same results to both assessors.
//
// Deterministic: reconcilers and kinds keep the order collect handed them, and
// each kind's enumeration is sorted worst-first, then by namespace and name,
// before it is capped.
func Assess(fetched []operators.Fetched, now time.Time, threshold time.Duration) Report {
	if threshold < 0 {
		threshold = 0
	}
	rep := Report{Threshold: durText(threshold)}
	index := map[string]int{} // reconciler name → position in rep.Reconcilers
	for _, f := range fetched {
		assess, ok := assessorFor(f.Adapter)
		if !ok {
			continue
		}
		i, seen := index[f.Adapter.Operator]
		if !seen {
			rep.Reconcilers = append(rep.Reconcilers, ReconcilerReport{Reconciler: f.Adapter.Operator})
			i = len(rep.Reconcilers) - 1
			index[f.Adapter.Operator] = i
		}
		rc := &rep.Reconcilers[i]
		if f.APIVersion != "" && !contains(rc.APIVersions, f.APIVersion) {
			rc.APIVersions = append(rc.APIVersions, f.APIVersion)
		}
		if k, keep := kindReport(f, assess, now, threshold); keep {
			rc.Kinds = append(rc.Kinds, k)
		}
	}
	return rep
}

// assessorFor matches a fetched adapter to its evaluator by group and resource.
func assessorFor(a operators.Adapter) (assessor, bool) {
	switch {
	case a.Group == "argoproj.io" && a.Resource == "applications":
		return assessArgo, true
	case a.Group == "kustomize.toolkit.fluxcd.io" && a.Resource == "kustomizations":
		return assessKustomization, true
	case a.Group == "helm.toolkit.fluxcd.io" && a.Resource == "helmreleases":
		return assessHelmRelease, true
	}
	return nil, false
}

// kindReport builds one kind's roll-up and reports whether it has anything to
// say. A kind with no objects, no denial, and no error is omitted: "installed and
// idle" is carried by the reconciler's own entry.
func kindReport(f operators.Fetched, assess assessor, now time.Time, threshold time.Duration) (KindReport, bool) {
	k := KindReport{
		Kind:       f.Adapter.Kind,
		APIVersion: f.APIVersion,
		Counts:     map[State]int{},
		Forbidden:  f.Forbidden,
	}
	if f.Err != nil {
		// A cluster's API URL can carry userinfo or an auth-proxy token, and
		// client-go wraps it in a *url.Error. Reduce it to scheme://host.
		k.Error = alert.RedactError(f.Err)
	}
	if k.Forbidden || k.Error != "" {
		return k, true
	}
	if len(f.Items) == 0 {
		return k, false
	}
	var drifted []Workload
	for _, item := range f.Items {
		a := assess(item.Object, now, threshold)
		k.Counts[a.State]++
		if a.State == StateSynced {
			continue
		}
		drifted = append(drifted, Workload{
			Reconciler: f.Adapter.Operator,
			Kind:       f.Adapter.Kind,
			Namespace:  item.GetNamespace(),
			Name:       item.GetName(),
			State:      a.State,
			Detail:     a.Detail,
		})
	}
	sort.SliceStable(drifted, func(i, j int) bool {
		if si, sj := drifted[i].State.severity(), drifted[j].State.severity(); si != sj {
			return si < sj
		}
		if drifted[i].Namespace != drifted[j].Namespace {
			return drifted[i].Namespace < drifted[j].Namespace
		}
		return drifted[i].Name < drifted[j].Name
	})
	if len(drifted) > MaxPerKind {
		k.Truncated = len(drifted) - MaxPerKind
		drifted = drifted[:MaxPerKind]
	}
	k.Drifted = drifted
	return k, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
