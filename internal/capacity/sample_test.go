package capacity

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func usageOf(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func TestSampleAttachesToFlaggedOwner(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}
	usage := map[string]corev1.ResourceList{"staging/web-1": usageOf("30m", "64Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.0") {
		t.Errorf("want an observed CPU reading, got %q", o.Observed)
	}
	if !rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable true")
	}
	if rep.RightSizing.PodsReporting != 1 || rep.RightSizing.PodsTotal != 1 {
		t.Errorf("want 1 of 1 reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}

// Replicas of one owner are summed, because the row names the workload.
func TestSampleSumsAcrossOwnerReplicas(t *testing.T) {
	pods := []corev1.Pod{
		pod("staging", "web-1", "worker1", "", ""),
		pod("staging", "web-2", "worker1", "", ""),
	}
	pods[0] = ownedBy(pods[0], "StatefulSet", "web")
	pods[1] = ownedBy(pods[1], "StatefulSet", "web")
	usage := map[string]corev1.ResourceList{
		"staging/web-1": usageOf("100m", "64Mi"),
		"staging/web-2": usageOf("400m", "64Mi"),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.5") {
		t.Errorf("want the two replicas summed to 0.5 cores, got %q", o.Observed)
	}
}

// THE core constraint: a healthy workload with a tiny sample must never appear.
func TestSampleNeverSelectsAWorkload(t *testing.T) {
	pods := []corev1.Pod{pod("prod", "thrifty", "worker1", "4", "8Gi")}
	usage := map[string]corev1.ResourceList{"prod/thrifty": usageOf("1m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "8", "32Gi")}, pods, nil, nil, usage, "")

	if rep.RightSizing != nil {
		t.Errorf("want no right-sizing block: a sample must never create a row, got %+v",
			rep.RightSizing)
	}
}

// limitWithoutRequest fires on either resource: a CPU limit with no CPU request, or
// a memory limit with no memory request. observedFor must pair the row with the
// SAME resource the rule flagged, not hard-code memory regardless of which one
// fired — a CPU-flagged row annotated with an unrelated memory figure invites the
// exact wrong conclusion.
func TestObservedForLimitNoRequestPairsWithFlaggedResourceCPU(t *testing.T) {
	deployments := []appsv1.Deployment{deployment("prod", "cpu-only", container("app", "", "", "2", ""))}
	templates := Templates(deployments, nil, nil, nil, nil)
	rs := []appsv1.ReplicaSet{replicaSet("prod", "cpu-only-abc", "cpu-only")}
	pods := []corev1.Pod{ownedBy(pod("prod", "cpu-only-1", "worker1", "", ""), "ReplicaSet", "cpu-only-abc")}
	usage := map[string]corev1.ResourceList{"prod/cpu-only-1": usageOf("1500m", "999Gi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, templates, usage, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	o := r.Owners[0]
	if !strings.Contains(o.Detail, "lim 2.0 cores") {
		t.Fatalf("want the CPU limit named in Detail, got %q", o.Detail)
	}
	if !strings.Contains(o.Observed, "1.5") || !strings.Contains(o.Observed, "cores") {
		t.Errorf("want a CPU observed reading pairing the CPU-flagged row, got %q", o.Observed)
	}
	if strings.Contains(o.Observed, "Gi") {
		t.Errorf("want no memory figure attached to a CPU-flagged row, got %q", o.Observed)
	}
}

// The memory branch, preserved: a memory-limit-without-request row still pairs with
// a memory reading.
func TestObservedForLimitNoRequestPairsWithFlaggedResourceMemory(t *testing.T) {
	deployments := []appsv1.Deployment{deployment("prod", "cache", container("app", "", "", "", "256Mi"))}
	templates := Templates(deployments, nil, nil, nil, nil)
	rs := []appsv1.ReplicaSet{replicaSet("prod", "cache-abc", "cache")}
	pods := []corev1.Pod{ownedBy(pod("prod", "cache-1", "worker1", "", ""), "ReplicaSet", "cache-abc")}
	usage := map[string]corev1.ResourceList{"prod/cache-1": usageOf("50m", "240Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, templates, usage, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	o := r.Owners[0]
	if !strings.Contains(o.Detail, "lim 256Mi") {
		t.Fatalf("want the memory limit named in Detail, got %q", o.Detail)
	}
	if o.Observed != "240Mi" {
		t.Errorf("want the memory observed reading, got %q", o.Observed)
	}
}

func TestSampleAbsentMetricsStillRendersRules(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, nil, "")

	if rep.RightSizing == nil || len(rep.RightSizing.Rules) != 1 {
		t.Fatalf("want the structural rule with no metrics, got %+v", rep.RightSizing)
	}
	if rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable false")
	}
	if rep.RightSizing.Rules[0].Owners[0].Observed != "" {
		t.Error("want no observed value with no metrics")
	}
}

// THE core constraint, stronger form: a usage sample never puts a workload on the
// list even when a DIFFERENT owner in the same scan is flagged. TestSampleNeverSelectsAWorkload
// above only covers the case where nothing is flagged at all (RightSizing is nil,
// the sample has nowhere to go); this proves the invariant holds once RightSizing is
// non-nil and attachSamples is actually walking populated rules.
func TestSampleNeverAddsARowForACleanOwner(t *testing.T) {
	flagged := pod("prod", "flagged-1", "worker1", "", "")      // no requests -> RuleNoRequests
	clean := pod("prod", "clean-1", "worker1", "200m", "256Mi") // structurally clean
	pods := []corev1.Pod{flagged, clean}
	usage := map[string]corev1.ResourceList{
		"prod/flagged-1": usageOf("10m", "8Mi"),
		"prod/clean-1":   usageOf("50m", "64Mi"), // a sample exists for the clean owner too
	}

	rep := Assess([]corev1.Node{node("worker1", "8", "32Gi")}, pods, nil, nil, usage, "")

	if rep.RightSizing == nil {
		t.Fatal("want a right-sizing block: the flagged owner must still produce a rule")
	}
	for _, r := range rep.RightSizing.Rules {
		for _, o := range r.Owners {
			if o.Name == "clean-1" {
				t.Errorf("want the clean owner never added as a row despite having a usage "+
					"sample, got %+v in rule %s", o, r.Name)
			}
		}
	}
	// Coverage counts pods the section considered, not just pods that ended up on a
	// row: both pods (flagged and clean) reported to metrics-server.
	if rep.RightSizing.PodsTotal != 2 || rep.RightSizing.PodsReporting != 2 {
		t.Errorf("want both pods counted for coverage, got %d of %d reporting",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}

// Coverage counts the pods the section considered, not every pod in the cluster.
func TestSampleCoverageCountsScopedPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "", ""),
		pod("prod", "b", "worker1", "", ""),
		pod("staging", "c", "worker1", "", ""),
	}
	usage := map[string]corev1.ResourceList{"prod/a": usageOf("10m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, usage, "prod")

	if rep.RightSizing.PodsTotal != 2 || rep.RightSizing.PodsReporting != 1 {
		t.Errorf("want 1 of 2 in-scope pods reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}
