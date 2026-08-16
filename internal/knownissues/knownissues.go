// Package knownissues is kubeagent's offline reference for the failure kinds
// its pod and workload detectors report: what each one means, what usually
// causes it, and what to look at next. It does not cover kubeagent's other
// cluster-level checks: HorizontalPodAutoscaler, PodDisruptionBudget,
// Ingress, Service, PersistentVolumeClaim, ResourceQuota, admission webhooks
// and the rollout checks each report their own findings, outside this
// reference's closure.
//
// The content is curated prose compiled into the binary, so `kubeagent
// known-issues` answers with no cluster, no kubeconfig and no network. It holds
// no client and no context, issues no cluster call and makes no model call —
// two separate promises, and neither implies the other. In particular this is
// not a smaller --explain: nothing here is generated, and nothing here is sent
// anywhere.
//
// The package imports nothing from kubeagent and nothing outside the standard
// library, which puts it in the same class as internal/jsonschema,
// internal/dashboard, internal/baseline and internal/glob and makes reaching
// internal/remediate or internal/explain impossible by construction rather than
// by rule. internal/knownissues/imports_test.go enforces both halves. The
// consequence is that the registry cannot check itself against the detector
// set; that check lives in internal/diagnose/knownissues_test.go, where both
// sides are in scope.
package knownissues

// Entry is what kubeagent knows about one failure mode, offline.
type Entry struct {
	// Kind is the exact Finding.Issue value a detector emits. It is the join
	// key between a scan's output and this reference, which is why it is a
	// verbatim copy rather than a prettier restatement.
	Kind string

	// Summary is one line, lowercase, no trailing period: it is rendered
	// inline in the list view beside the kind.
	Summary string

	// Detail is the sentence or two printed above the causes when one entry is
	// printed in full. Capitalised, punctuated.
	Detail string

	// Causes are what actually produces this, most common first.
	Causes []string

	// Checks are read-only next steps. Any object name is a placeholder.
	Checks []string

	// Docs is the anchor on the project's own documentation site, or empty.
	Docs string
}

// All returns every entry, sorted by Kind.
//
// The result is independent of the registry, all the way down: the slice, each
// Entry, and each Entry's Causes and Checks are fresh. A caller may sort,
// filter, truncate or rewrite what it is handed without any of it reaching the
// next caller.
func All() []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = clone(e)
	}
	return out
}

// clone returns an Entry that shares nothing with the registry: the Causes and
// Checks slices are copied too, not just the struct header. Without this, a
// caller that reordered or rewrote a returned Entry's Causes would silently
// rewrite the registry for every later caller.
func clone(e Entry) Entry {
	out := e
	out.Causes = append([]string(nil), e.Causes...)
	out.Checks = append([]string(nil), e.Checks...)
	return out
}

// Kinds returns every Kind, sorted — the same order as All.
func Kinds() []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

// workloadKinds are the Finding.Issue values kubeagent emits from scan's
// workload passes rather than from the pod detector set this reference
// documents. They are deliberately absent from entries: known-issues covers the
// deterministic detectors, and a workload-level finding is explained on the
// diagnostics page instead.
//
// The list exists so the CLI can tell two different situations apart. An
// operator who reads RolloutStuck off a scan and asks about it must not be told
// the kind is unknown — kubeagent emitted it a second earlier. A typo must
// still be called unknown, because that is the correct word for it.
//
// It is closed the same way entries is, and by the same kind of test — but that
// test cannot live here. This package imports nothing, so it cannot see scan's
// workload passes; internal/scan/workloadkinds_test.go holds the check, where
// the passes are in scope.
var workloadKinds = []string{"RolloutStuck", "FailedCreate", "JobFailed"}

// WorkloadKinds returns the workload-level kinds, in registry order.
//
// The result is a copy: a caller may sort, filter or rewrite what it is handed
// without any of it reaching the next caller, the same promise All and Lookup
// make.
func WorkloadKinds() []string {
	return append([]string(nil), workloadKinds...)
}

// IsWorkloadKind reports whether kind is one of the workload-level kinds.
//
// Matched byte for byte, for the reason Lookup is: the caller passes a
// Finding.Issue value verbatim, so a near miss is a typo and belongs in the
// unknown-kind message rather than in this one.
func IsWorkloadKind(kind string) bool {
	for _, k := range workloadKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Lookup returns the entry for a kind, matched byte for byte.
//
// Deliberately no normalisation: no case folding, no Init: stripping, no fuzzy
// match. Falling back from "Init:OOMKilled" to "OOMKilled" would be the
// tempting convenience and it would be wrong — an init container killed for
// memory blocks the pod from ever starting, which is a different failure with
// different causes and different next steps. The caller passes a Finding.Issue
// value verbatim, and an exact match is the only honest answer.
func Lookup(kind string) (Entry, bool) {
	for _, e := range entries {
		if e.Kind == kind {
			return clone(e), true
		}
	}
	return Entry{}, false
}
