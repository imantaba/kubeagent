package capacity

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// ruleByName finds a rule in the report, or fails the test.
func ruleByName(t *testing.T, rs *RightSizing, name RuleName) Rule {
	t.Helper()
	if rs == nil {
		t.Fatalf("want a right-sizing block, got nil")
	}
	for _, r := range rs.Rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("want rule %q, got %+v", name, rs.Rules)
	return Rule{}
}

func TestRuleNoRequestsFlagsBestEffortPod(t *testing.T) {
	pods := []corev1.Pod{ownedBy(pod("staging", "web-1", "worker1", "", ""), "ReplicaSet", "web-abc")}
	rs := []appsv1.ReplicaSet{replicaSet("staging", "web-abc", "web")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	o := r.Owners[0]
	if o.Kind != "Deployment" || o.Namespace != "staging" || o.Name != "web" {
		t.Errorf("want the ReplicaSet resolved up to Deployment/staging/web, got %+v", o)
	}
	if !o.BestEffort {
		t.Error("want BestEffort set when every container lacks both requests")
	}
}

// A pod where only one of two containers lacks requests is still flagged, but it is
// not BestEffort — the pod-level QoS claim would be false.
func TestRuleNoRequestsMixedPodIsNotBestEffort(t *testing.T) {
	p := pod("staging", "api-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{
		container("app", "100m", "128Mi", "", ""),
		container("sidecar", "", "", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 || r.Owners[0].BestEffort {
		t.Errorf("want flagged but not BestEffort, got %+v", r.Owners)
	}
}

func TestRuleLimitNoRequestNamesTheLimit(t *testing.T) {
	p := pod("prod", "cache-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{container("app", "", "", "", "256Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	if !strings.Contains(r.Owners[0].Detail, "lim 256Mi") {
		t.Errorf("want the limit named, got %q", r.Owners[0].Detail)
	}
}

func TestRuleNeverSchedulableComparesAgainstLargestIncludedNode(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "16", "64Gi"),
		tainted(node("huge-cp", "64", "256Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{ownedBy(pod("batch", "trainer-1", "", "40", "8Gi"), "Job", "trainer")}

	rep := Assess(nodes, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNeverSchedulable)
	if len(r.Owners) != 1 || r.Owners[0].Name != "trainer" {
		t.Fatalf("want the 40-core Job flagged against the 16-core included node, got %+v", r.Owners)
	}
	if !strings.Contains(r.Owners[0].Detail, "40.0 cores") ||
		!strings.Contains(r.Owners[0].Detail, "16.0 cores") {
		t.Errorf("want both quantities in the detail, got %q", r.Owners[0].Detail)
	}
}

// A heterogeneous cluster: one node has lots of CPU and little memory, the other
// has little CPU and lots of memory. A container that requests more CPU than the
// small-CPU node and more memory than the small-memory node fits under BOTH
// cluster-wide maxima individually, yet no single node can ever hold it together
// — a pod lands on exactly one node. The rule must still catch this.
func TestRuleNeverSchedulableHeterogeneousNodes(t *testing.T) {
	nodes := []corev1.Node{
		node("big-cpu", "16", "8Gi"),
		node("big-mem", "4", "64Gi"),
	}
	pods := []corev1.Pod{ownedBy(pod("batch", "odd-shape", "big-cpu", "10", "30Gi"), "Job", "odd-shape")}

	rep := Assess(nodes, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNeverSchedulable)
	if len(r.Owners) != 1 || r.Owners[0].Name != "odd-shape" {
		t.Fatalf("want the mixed-shape container flagged, got %+v", r.Owners)
	}
	if !strings.Contains(r.Owners[0].Detail, "10.0 cores") ||
		!strings.Contains(r.Owners[0].Detail, "30Gi") {
		t.Errorf("want both quantities named in the detail, got %q", r.Owners[0].Detail)
	}
}

// A request that exactly equals allocatable is schedulable, not "never".
func TestRuleNeverSchedulableExcludesExactFit(t *testing.T) {
	pods := []corev1.Pod{pod("batch", "fits", "", "16", "1Gi")}

	rep := Assess([]corev1.Node{node("worker1", "16", "64Gi")}, pods, nil, nil, "")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleNeverSchedulable {
				t.Errorf("want no never-schedulable rule for an exact fit, got %+v", r.Owners)
			}
		}
	}
}

// With no node included there is nothing to compare against, so the rule is silent
// rather than flagging every workload in the cluster.
func TestRuleNeverSchedulableSilentWithNoIncludedNodes(t *testing.T) {
	nodes := []corev1.Node{cordoned(node("parked", "16", "64Gi"))}
	pods := []corev1.Pod{pod("batch", "big", "", "40", "8Gi")}

	rep := Assess(nodes, pods, nil, nil, "")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleNeverSchedulable {
				t.Errorf("want silence with nothing to compare against, got %+v", r.Owners)
			}
		}
	}
}

// Many pods of one Deployment collapse to one row.
func TestRightSizingRollsUpReplicasToOneOwner(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 5; i++ {
		pods = append(pods, ownedBy(pod("staging", fmt.Sprintf("web-%d", i), "worker1", "", ""),
			"ReplicaSet", "web-abc"))
	}
	rs := []appsv1.ReplicaSet{replicaSet("staging", "web-abc", "web")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 {
		t.Errorf("want 5 replicas rolled up to 1 owner, got %+v", r.Owners)
	}
}

// A pod with no controller owner is reported as itself.
func TestRightSizingOwnerlessPod(t *testing.T) {
	pods := []corev1.Pod{pod("default", "loose", "worker1", "", "")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if r.Owners[0].Kind != "Pod" || r.Owners[0].Name != "loose" {
		t.Errorf("want Pod/default/loose, got %+v", r.Owners[0])
	}
}

func TestRightSizingCapsAtTwentyOwners(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 26; i++ {
		pods = append(pods, pod("staging", fmt.Sprintf("p%02d", i), "worker1", "", ""))
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 20 {
		t.Errorf("want 20 owners listed, got %d", len(r.Owners))
	}
	if r.Truncated != 6 {
		t.Errorf("want 6 reported as truncated, got %d", r.Truncated)
	}
}

func TestRightSizingOrdersByNamespaceThenName(t *testing.T) {
	pods := []corev1.Pod{
		pod("zeta", "b", "worker1", "", ""),
		pod("alpha", "z", "worker1", "", ""),
		pod("alpha", "a", "worker1", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	got := []string{}
	for _, o := range r.Owners {
		got = append(got, o.Namespace+"/"+o.Name)
	}
	want := []string{"alpha/a", "alpha/z", "zeta/b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestRightSizingNamespaceScopesEnumerationOnly(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "", ""),
		pod("staging", "b", "worker1", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "prod")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 || r.Owners[0].Namespace != "prod" {
		t.Errorf("want only prod enumerated, got %+v", r.Owners)
	}
	// Headroom still counts both pods: nodes are cluster-scoped.
	if rep.Headroom.IncludedNodes != 1 {
		t.Errorf("want headroom unaffected by -n, got %+v", rep.Headroom)
	}
}

func TestRightSizingNilWhenNothingFlagged(t *testing.T) {
	pods := []corev1.Pod{pod("prod", "good", "worker1", "100m", "128Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	if rep.RightSizing != nil && len(rep.RightSizing.Rules) > 0 {
		t.Errorf("want no rules for a well-formed workload, got %+v", rep.RightSizing.Rules)
	}
}

// Two distinct owners can share a namespace and name but differ in kind — an
// ownerless Pod/prod/worker and a Job/prod/worker, both bare. The comparator must
// treat Kind as a final tiebreaker so the sort is total; otherwise the slice built
// from a randomized map range keeps whatever relative order it arrived in, and
// output would vary between runs of an otherwise-identical scan.
func TestRightSizingOrderIsTotalAcrossKinds(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "worker", "worker1", "", ""),
		ownedBy(pod("prod", "worker-x", "worker1", "", ""), "Job", "worker"),
	}
	nodes := []corev1.Node{node("worker1", "4", "16Gi")}
	want := []string{"Job/prod/worker", "Pod/prod/worker"}

	// Go randomizes map iteration order per range statement, so repeated calls in
	// one process do exercise the bug: build the report several times and require
	// the same order every time, not just once by luck.
	for i := 0; i < 10; i++ {
		rep := Assess(nodes, pods, nil, nil, "")
		r := ruleByName(t, rep.RightSizing, RuleNoRequests)
		if len(r.Owners) != 2 {
			t.Fatalf("run %d: want 2 owners, got %+v", i, r.Owners)
		}
		got := []string{}
		for _, o := range r.Owners {
			got = append(got, o.Kind+"/"+o.Namespace+"/"+o.Name)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("run %d: want deterministic order %v, got %v", i, want, got)
		}
	}
}

// The vocabulary ban is a design constraint, not a style preference: none of these
// words can be justified from a single non-historical sample.
func TestNoForbiddenVocabularyInDetails(t *testing.T) {
	p := pod("prod", "cache-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{container("app", "", "", "", "256Mi")}
	pods := []corev1.Pod{p, pod("staging", "web", "worker1", "", ""),
		pod("batch", "big", "", "40", "8Gi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	for _, r := range rep.RightSizing.Rules {
		for _, o := range r.Owners {
			for _, banned := range []string{"peak", "over-requested", "oversized", "waste"} {
				if strings.Contains(strings.ToLower(o.Detail), banned) {
					t.Errorf("detail %q contains banned word %q", o.Detail, banned)
				}
			}
		}
	}
}
