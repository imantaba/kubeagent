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
	"github.com/imantaba/kubeagent/internal/inventory"
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
	trail1, bundle1 := gatherEvidence(context.Background(), client, scoped)
	trail2, bundle2 := gatherEvidence(context.Background(), client, scoped)
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
	trail, _ := gatherEvidence(context.Background(), client, flaggedScope(scoped))
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
	trail, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w1, w2})
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
	trail, bundle := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
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
	trail, _ := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
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
	trail, _ := gatherEvidence(context.Background(), client, []inventory.Workload{w})
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
	_, bundle := gatherEvidence(context.Background(), client, []inventory.Workload{gatherWL("shop", "web")})
	if !strings.Contains(bundle, truncationMarker) {
		t.Errorf("an oversized read must carry the truncation marker")
	}
	if len(bundle) > maxReadBytes+1024 {
		t.Errorf("bundle for one capped read is %d bytes, want ≈%d", len(bundle), maxReadBytes)
	}
}
