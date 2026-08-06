// Package knownissues is kubeagent's offline reference for the failure kinds
// its deterministic detectors report: what each one means, what usually causes
// it, and what to look at next.
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
// The result is a fresh slice each call: a caller that sorts, filters or
// truncates it must not be able to corrupt the registry for the next one. The
// Causes and Checks slices inside each Entry are still shared, which is why
// this package hands out no way to mutate them and every consumer only ranges.
func All() []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
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
			return e, true
		}
	}
	return Entry{}, false
}
