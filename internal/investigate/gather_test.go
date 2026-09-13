package investigate

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/hypothesis"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// gatherWL builds one flagged workload for gather tests.
func gatherWL(ns, name string, findings ...diagnose.Finding) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Degraded", Findings: findings}
}

func TestFlaggedScopeCapsAtTen(t *testing.T) {
	var ws []inventory.Workload
	healthy := inventory.Workload{Namespace: "shop", Name: "ok", Kind: "Deployment", Ready: 1, Desired: 1, Status: "Running"}
	ws = append(ws, healthy)
	for i := 0; i < 11; i++ {
		ws = append(ws, gatherWL("shop", fmt.Sprintf("web-%02d", i)))
	}
	got := flaggedScope(ws)
	if len(got) != maxGatherWorkloads {
		t.Fatalf("scoped %d workloads, want %d", len(got), maxGatherWorkloads)
	}
	if got[0].Name != "web-00" || got[9].Name != "web-09" {
		t.Errorf("scope must keep report order: first %q last %q", got[0].Name, got[9].Name)
	}
	for _, w := range got {
		if w.Name == "ok" {
			t.Errorf("an unflagged workload entered the scope")
		}
	}
}

func TestGatherEvidenceDeterministicTrailAndSections(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-1", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "web-abc"},
			Reason:         "BackOff", Message: "Back-off restarting failed container", Count: 4},
	)
	w := gatherWL("shop", "web", diagnose.Finding{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: "app"})
	w.RootCauseTrace = []inventory.Hypothesis{
		{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
			Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
		{Cause: "registry ghcr.io", Kind: "registry", Object: "ghcr.io",
			Verdict: inventory.VerdictOutranked, Reason: "node worker-1 (NotReady) is the stronger cause"},
		{Cause: "PVC web-data (ProvisioningFailed)", Kind: "pvc", Object: "web-data",
			Verdict: inventory.VerdictRuledOut, Reason: "not mounted by this workload's pods"},
	}
	scoped := []inventory.Workload{w}
	trail1, bundle1, _ := gatherEvidence(context.Background(), client, scoped)
	trail2, bundle2, _ := gatherEvidence(context.Background(), client, scoped)
	if strings.Join(trail1, "|") != strings.Join(trail2, "|") || bundle1 != bundle2 {
		t.Fatalf("gather must be deterministic")
	}
	// Registry candidates get no read; ruled-out candidates get no read.
	wantTrail := []string{
		"events shop/web-abc",
		"describe node /worker-1",
		"log causes shop/web-abc container app",
	}
	if strings.Join(trail1, "|") != strings.Join(wantTrail, "|") {
		t.Errorf("trail = %v, want %v", trail1, wantTrail)
	}
	for _, label := range wantTrail {
		if !strings.Contains(bundle1, "== "+label+" ==\n") {
			t.Errorf("bundle missing section %q:\n%s", label, bundle1)
		}
	}
	if !strings.Contains(bundle1, "BackOff: Back-off restarting failed container (x4)") {
		t.Errorf("event content missing from bundle:\n%s", bundle1)
	}
}

func TestGatherEvidenceGlobalBudgetIsEight(t *testing.T) {
	client := fake.NewSimpleClientset()
	var scoped []inventory.Workload
	for i := 0; i < 11; i++ {
		scoped = append(scoped, gatherWL("shop", fmt.Sprintf("web-%02d", i)))
	}
	trail, _, _ := gatherEvidence(context.Background(), client, flaggedScope(scoped))
	if len(trail) != maxToolCalls {
		t.Errorf("made %d reads, want the global budget %d", len(trail), maxToolCalls)
	}
}

func TestGatherEvidenceDedupesDescribesAcrossWorkloads(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	shared := inventory.Hypothesis{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
		Verdict: inventory.VerdictAttributed, Reason: "pod a is scheduled on it"}
	w1 := gatherWL("shop", "web")
	w1.RootCauseTrace = []inventory.Hypothesis{shared}
	w2 := gatherWL("shop", "api")
	w2.RootCauseTrace = []inventory.Hypothesis{shared}
	trail, _, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w1, w2})
	describes := 0
	for _, l := range trail {
		if l == "describe node /worker-1" {
			describes++
		}
	}
	if describes != 1 {
		t.Errorf("node described %d times, want 1 (global dedupe); trail: %v", describes, trail)
	}
}

func TestGatherEvidenceFailedReadCountsAndIsReduced(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("boom")
	})
	trail, bundle, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	if len(trail) != 1 {
		t.Fatalf("a refused read must still consume budget; trail: %v", trail)
	}
	if !strings.Contains(bundle, "read failed: ") {
		t.Errorf("failed read must render as a reduced error:\n%s", bundle)
	}
	if strings.Count(bundle, "boom") > 1 {
		t.Errorf("raw error text repeated unexpectedly:\n%s", bundle)
	}
}

func TestGatherEvidenceEventsFallBackToWorkloadName(t *testing.T) {
	client := fake.NewSimpleClientset()
	trail, _, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	if len(trail) != 1 || trail[0] != "events shop/web" {
		t.Errorf("no findings => events for the workload name; trail: %v", trail)
	}
}

func TestGatherEvidenceSkipsLogReadWithoutContainerAndDedupes(t *testing.T) {
	client := fake.NewSimpleClientset()
	w := gatherWL("shop", "web",
		diagnose.Finding{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: ""},
		diagnose.Finding{Pod: "shop/web-abc", Issue: "OOMKilled", Container: "app"},
		diagnose.Finding{Pod: "shop/web-abc", Issue: "ContainerStartError", Container: "app"},
		diagnose.Finding{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Container: "app"},
	)
	trail, _, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w})
	logs := 0
	for _, l := range trail {
		if strings.HasPrefix(l, "log causes ") {
			logs++
		}
	}
	if logs != 1 {
		t.Errorf("want exactly 1 log read (crash family only, empty container skipped, per-container dedupe); trail: %v", trail)
	}
}

func TestCapContentCutsAtLineBoundaryWithMarker(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 99)+"\n", 60) // 6000 bytes of 99-byte lines
	got := capContent(long)
	if len(got) > maxReadBytes+len(truncationMarker)+1 {
		t.Fatalf("capContent returned %d bytes", len(got))
	}
	if !strings.HasSuffix(got, "\n"+truncationMarker) {
		t.Fatalf("cut content must end with the marker, got tail %q", got[len(got)-40:])
	}
	for _, ln := range strings.Split(strings.TrimSuffix(got, "\n"+truncationMarker), "\n") {
		if len(ln) != 99 {
			t.Errorf("a half-written line survived the cut: %q", ln)
		}
	}
	short := "one line\n"
	if capContent(short) != short {
		t.Errorf("content under the cap must pass through unchanged")
	}
}

func TestGatherEvidenceCapsOneReadAtFourKiB(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 200; i++ {
		objs = append(objs, &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("ev-%03d", i), Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "web"},
			Reason:         "BackOff", Message: strings.Repeat("x", 40), Count: 1,
		})
	}
	client := fake.NewSimpleClientset(objs...)
	_, bundle, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	if !strings.Contains(bundle, truncationMarker) {
		t.Errorf("an oversized read must carry the truncation marker")
	}
	if len(bundle) > maxReadBytes+1024 {
		t.Errorf("bundle for one capped read is %d bytes, want ≈%d", len(bundle), maxReadBytes)
	}
}

func TestGatherEvidenceReadsReachTheRules(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-data"},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "ev-1", Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Name: "web-abc"},
			Reason:         "BackOff", Message: "Back-off restarting failed container", Count: 4},
	)
	w := gatherWL("shop", "web", diagnose.Finding{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Container: "app"})
	w.RootCauseTrace = []inventory.Hypothesis{
		{Cause: "node worker-1 (NotReady)", Kind: "node", Object: "worker-1",
			Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"},
		{Cause: "PVC web-data (ProvisioningFailed)", Kind: "pvc", Object: "web-data",
			Verdict: inventory.VerdictOutranked, Reason: "node worker-1 (NotReady) is the stronger cause"},
	}
	_, _, reads := gatherEvidence(context.Background(), client, []inventory.Workload{w})
	if n := reads.Nodes["worker-1"]; n == nil || n.Status.Conditions[0].Status != corev1.ConditionFalse {
		t.Errorf("the node read must reach the rules, got %+v", n)
	}
	if pvc := reads.PVCs["shop/web-data"]; pvc == nil || pvc.Status.Phase != corev1.ClaimPending {
		t.Errorf("the PVC read must reach the rules, got %+v", pvc)
	}
	if evs := reads.Events["shop/web-abc"]; len(evs) != 1 || evs[0].Reason != "BackOff" {
		t.Errorf("the events read must reach the rules, got %+v", evs)
	}
	if len(reads.Failed) != 0 {
		t.Errorf("no read failed, got %v", reads.Failed)
	}
}

func TestGatherEvidenceMissingNodeIsAFailedRead(t *testing.T) {
	client := fake.NewSimpleClientset()
	w := gatherWL("shop", "web")
	w.RootCauseTrace = []inventory.Hypothesis{{Cause: "node worker-1 (NotReady)", Kind: "node",
		Object: "worker-1", Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"}}
	_, bundle, reads := gatherEvidence(context.Background(), client, []inventory.Workload{w})
	if got := reads.Failed["node/worker-1"]; got != "nodes \"worker-1\" not found" {
		t.Errorf("Failed[node/worker-1] = %q", got)
	}
	if _, ok := reads.Nodes["worker-1"]; ok {
		t.Errorf("a failed read must not leave a node behind")
	}
	if !strings.Contains(bundle, "read failed: ") {
		t.Errorf("the bundle still shows the failure:\n%s", bundle)
	}
}

func TestGatherEvidenceFailedReadIsSanitizedForTheRules(t *testing.T) {
	client := fake.NewSimpleClientset()
	boom := fmt.Errorf("boom\x1b[31m\nsecond line")
	client.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	_, bundle, reads := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	got, ok := reads.Failed["events/shop/web"]
	if !ok {
		t.Fatalf("Failed must carry the events read, got %v", reads.Failed)
	}
	if want := safetext.Line(redact.Error(boom)); got != want {
		t.Errorf("Failed[events/shop/web] = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("a stored failure must be one clean line, got %q", got)
	}
	if !strings.Contains(bundle, "read failed: ") {
		t.Errorf("the bundle keeps its reduced-error section:\n%s", bundle)
	}
}

func TestGatherEvidenceBudgetLeavesReadsUnmade(t *testing.T) {
	var objs []runtime.Object
	var ws []inventory.Workload
	for i := 1; i <= 9; i++ {
		name := fmt.Sprintf("worker-%d", i)
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		w := gatherWL("shop", fmt.Sprintf("web-%d", i))
		w.RootCauseTrace = []inventory.Hypothesis{{Cause: "node " + name + " (NotReady)", Kind: "node",
			Object: name, Verdict: inventory.VerdictAttributed, Reason: "pod is scheduled on it"}}
		ws = append(ws, w)
	}
	trail, _, reads := gatherEvidence(context.Background(), fake.NewSimpleClientset(objs...), ws)
	if len(trail) != maxToolCalls {
		t.Fatalf("budget is %d reads, trail has %d", maxToolCalls, len(trail))
	}
	// Each workload costs two reads (events, then its node), so the budget
	// covers four workloads: worker-1..4 are read, worker-5..9 are not.
	for i := 1; i <= 4; i++ {
		if reads.Nodes[fmt.Sprintf("worker-%d", i)] == nil {
			t.Errorf("worker-%d was within budget and must be read", i)
		}
	}
	for i := 5; i <= 9; i++ {
		name := fmt.Sprintf("worker-%d", i)
		if _, ok := reads.Nodes[name]; ok {
			t.Errorf("%s is past the budget and must not be in Nodes", name)
		}
		if _, ok := reads.Failed["node/"+name]; ok {
			t.Errorf("%s was never attempted and must not be in Failed", name)
		}
	}
}

// The shared-cause lines join the model summary under one cap and one
// marker; the two packages must agree on both.
func TestSharedCapsMatchTheSummaryCaps(t *testing.T) {
	if hypothesis.MaxSharedLines != maxSummaryLines {
		t.Errorf("hypothesis.MaxSharedLines = %d, maxSummaryLines = %d", hypothesis.MaxSharedLines, maxSummaryLines)
	}
	if hypothesis.TruncationMarker != truncationMarker {
		t.Errorf("hypothesis.TruncationMarker = %q, truncationMarker = %q", hypothesis.TruncationMarker, truncationMarker)
	}
}
