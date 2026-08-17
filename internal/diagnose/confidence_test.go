// This file is package diagnose_test, not diagnose: internal/inventory
// imports internal/diagnose, and internal/confidence imports
// internal/inventory, so an internal (package diagnose) test file cannot
// import internal/confidence without an import cycle. The external test
// package sits above both and has no such conflict.
package diagnose_test

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/confidence"
	"github.com/imantaba/kubeagent/internal/knownissues"
)

// confidenceSource names which mechanism supplies a kind's confidence level:
// viaForIssue means confidence.ForIssue(kind) answers directly; viaProducer
// means a detector sets Finding.Confidence itself and confidence.Annotate's
// fill-if-empty guard never consults ForIssue for that kind.
type confidenceSource int

const (
	viaForIssue confidenceSource = iota
	viaProducer
)

// confidenceTable pins the confidence level — or the fact that a producer
// sets it — for every kind internal/knownissues documents: the sixteen
// pod-detector kinds from Kinds() plus the three workload-level kinds from
// WorkloadKinds() (deliberately absent from Kinds(); see that package's own
// comment). TestConfidenceTableCoversEveryKnownIssueKind fails by name on any
// kind this table does not cover, so a seventeenth kind cannot arrive without
// someone writing down which level it is and why.
//
// RolloutStuck has no single level here. internal/rollouthealth's Deployment
// arm sets "high" (a controller-set condition — a direct read) and its
// StatefulSet/DaemonSet arms set "medium" (counters and a grace period — an
// inference); confidence.ForIssue is never consulted for it, because
// confidence.Annotate only fills an empty field. Those two levels are pinned
// in internal/rollouthealth's own table test
// (TestAnnotate_ConfidenceByArm); this table only records that RolloutStuck's
// source is a producer, not ForIssue.
var confidenceTable = map[string]struct {
	source confidenceSource
	level  string // meaningless when source is viaProducer
}{
	"ContainerStartError":             {viaForIssue, "medium"},
	"CrashLoopBackOff":                {viaForIssue, "high"},
	"CreateContainerConfigError":      {viaForIssue, "high"},
	"ErrImagePull":                    {viaForIssue, "high"},
	"ImagePullBackOff":                {viaForIssue, "high"},
	"Init:CrashLoopBackOff":           {viaForIssue, "high"},
	"Init:CreateContainerConfigError": {viaForIssue, "high"},
	"Init:ErrImagePull":               {viaForIssue, "high"},
	"Init:ImagePullBackOff":           {viaForIssue, "high"},
	"Init:OOMKilled":                  {viaForIssue, "high"},
	"OOMKilled":                       {viaForIssue, "high"},
	"ProbeFailure":                    {viaForIssue, "medium"},
	"RestartLoop":                     {viaForIssue, "medium"},
	"Unschedulable":                   {viaForIssue, "high"},
	"VolumeAttachError":               {viaForIssue, "high"},
	"VolumeMountError":                {viaForIssue, "high"},
	"RolloutStuck":                    {viaProducer, ""},
	"FailedCreate":                    {viaForIssue, "high"},
	"JobFailed":                       {viaForIssue, "high"},
}

// TestConfidenceTableCoversEveryKnownIssueKind iterates both halves of
// internal/knownissues' vocabulary — Kinds() (the sixteen pod-detector kinds)
// and WorkloadKinds() (the three workload-level kinds) — nineteen kinds in
// total, and fails by name on any kind confidenceTable does not cover. For
// every kind whose source is viaForIssue, it also asserts
// confidence.ForIssue(kind) equals the table's level.
func TestConfidenceTableCoversEveryKnownIssueKind(t *testing.T) {
	kinds := append(append([]string(nil), knownissues.Kinds()...), knownissues.WorkloadKinds()...)
	if len(kinds) != 19 {
		t.Fatalf("knownissues.Kinds()+WorkloadKinds() = %d kinds, want 19 — update confidenceTable's rows (and this count) if the vocabulary grew", len(kinds))
	}
	for _, kind := range kinds {
		entry, ok := confidenceTable[kind]
		if !ok {
			t.Errorf("kind %q has no entry in confidenceTable — write down its confidence level (or that a producer sets it) and why", kind)
			continue
		}
		if entry.source != viaForIssue {
			continue
		}
		if got := confidence.ForIssue(kind); got != entry.level {
			t.Errorf("confidence.ForIssue(%q) = %q, want %q", kind, got, entry.level)
		}
	}
}

// TestConfidenceTableHasNoStaleEntry is the reverse of
// TestConfidenceTableCoversEveryKnownIssueKind: nothing in confidenceTable
// names a kind internal/knownissues no longer knows. Without this, a
// retired detector's row could sit in confidenceTable forever with no test
// ever flagging it for cleanup. This mirrors the pair
// internal/diagnose/knownissues_test.go already establishes for the kind
// vocabulary itself — TestDetectorsProduceOnlyDocumentedKinds (forward) and
// TestEveryDocumentedKindIsProduced (reverse).
func TestConfidenceTableHasNoStaleEntry(t *testing.T) {
	known := map[string]bool{}
	for _, k := range append(append([]string(nil), knownissues.Kinds()...), knownissues.WorkloadKinds()...) {
		known[k] = true
	}
	for kind := range confidenceTable {
		if !known[kind] {
			t.Errorf("confidenceTable has an entry for %q, which internal/knownissues no longer documents — remove the stale row", kind)
		}
	}
}
