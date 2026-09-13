package hypothesis

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// emptyReads is a Reads with every map allocated and nothing in it.
func emptyReads() Reads {
	return Reads{
		Nodes:  map[string]*corev1.Node{},
		PVCs:   map[string]*corev1.PersistentVolumeClaim{},
		Events: map[string][]corev1.Event{},
		Failed: map[string]string{},
	}
}

// nodeWithReady builds a node whose Ready condition carries status.
func nodeWithReady(name string, status corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}}}
}

// nodeCandidateOn is an attributed node candidate naming node with reason.
func nodeCandidateOn(node, reason string) inventory.Hypothesis {
	return inventory.Hypothesis{Cause: "node " + node + " (" + reason + ")", Kind: "node", Object: node,
		Verdict: inventory.VerdictAttributed, Reason: "pod web-abc is scheduled on it"}
}

// workloadWith is shop/web carrying the given trace.
func workloadWith(trace ...inventory.Hypothesis) inventory.Workload {
	return inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Degraded", RootCauseTrace: trace}
}

func TestNodeRuleNotReady(t *testing.T) {
	cases := []struct {
		name     string
		node     *corev1.Node
		outcome  Outcome
		evidence string
	}{
		{"true", nodeWithReady("worker-1", corev1.ConditionTrue), Refuted, "Ready condition is True now"},
		{"false", nodeWithReady("worker-1", corev1.ConditionFalse), Confirmed, "Ready condition is False now"},
		{"unknown", nodeWithReady("worker-1", corev1.ConditionUnknown), Confirmed, "Ready condition is Unknown now"},
		{"no ready condition", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}, Confirmed, "the node has no Ready condition"},
		{"odd status", nodeWithReady("worker-1", corev1.ConditionStatus("Maybe")), Unverified, "Ready condition is not one kubeagent expects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := emptyReads()
			rd.Nodes["worker-1"] = tc.node
			r := Decide(workloadWith(nodeCandidateOn("worker-1", "NotReady")), rd)
			if len(r.Decisions) != 1 {
				t.Fatalf("want 1 decision, got %+v", r.Decisions)
			}
			if d := r.Decisions[0]; d.Outcome != tc.outcome || d.Evidence != tc.evidence {
				t.Errorf("got %q %q, want %q %q", d.Outcome, d.Evidence, tc.outcome, tc.evidence)
			}
		})
	}
}

func TestNodeRuleHeartbeat(t *testing.T) {
	for _, reason := range []string{"kubelet not heartbeating", "no kubelet lease"} {
		cases := []struct {
			name     string
			node     *corev1.Node
			outcome  Outcome
			evidence string
		}{
			{"true", nodeWithReady("worker-1", corev1.ConditionTrue), Unverified, "Ready condition is True, but the kubelet lease was not re-read"},
			{"false", nodeWithReady("worker-1", corev1.ConditionFalse), Confirmed, "Ready condition is False now"},
			{"unknown", nodeWithReady("worker-1", corev1.ConditionUnknown), Confirmed, "Ready condition is Unknown now"},
			{"missing", &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}, Confirmed, "the node has no Ready condition"},
		}
		for _, tc := range cases {
			t.Run(reason+"/"+tc.name, func(t *testing.T) {
				rd := emptyReads()
				rd.Nodes["worker-1"] = tc.node
				d := Decide(workloadWith(nodeCandidateOn("worker-1", reason)), rd).Decisions[0]
				if d.Outcome != tc.outcome || d.Evidence != tc.evidence {
					t.Errorf("got %q %q, want %q %q", d.Outcome, d.Evidence, tc.outcome, tc.evidence)
				}
			})
		}
	}
}

func TestNodeRuleUnknownReasonReadsLikeNotReady(t *testing.T) {
	rd := emptyReads()
	rd.Nodes["worker-1"] = nodeWithReady("worker-1", corev1.ConditionTrue)
	d := Decide(workloadWith(nodeCandidateOn("worker-1", "SomethingElse")), rd).Decisions[0]
	if d.Outcome != Refuted || d.Evidence != "Ready condition is True now" {
		t.Errorf("a reason outside the three known ones must follow the NotReady rule, got %q %q", d.Outcome, d.Evidence)
	}
}

func TestNodeRuleFailedRead(t *testing.T) {
	rd := emptyReads()
	rd.Nodes["worker-1"] = nodeWithReady("worker-1", corev1.ConditionFalse)
	rd.Failed["node/worker-1"] = "nodes \"worker-1\" is forbidden"
	d := Decide(workloadWith(nodeCandidateOn("worker-1", "NotReady")), rd).Decisions[0]
	if d.Outcome != Unverified || d.Evidence != "fresh read failed: nodes \"worker-1\" is forbidden" {
		t.Errorf("Failed must win over a stale object, got %q %q", d.Outcome, d.Evidence)
	}
}

func TestNodeRuleNeverRead(t *testing.T) {
	d := Decide(workloadWith(nodeCandidateOn("worker-1", "NotReady")), emptyReads()).Decisions[0]
	if d.Outcome != Unverified || d.Evidence != "not re-read: the read budget was spent first" {
		t.Errorf("got %q %q", d.Outcome, d.Evidence)
	}
}

func TestNodeRuleEmptyObjectIsNeverRead(t *testing.T) {
	h := nodeCandidateOn("worker-1", "NotReady")
	h.Object = ""
	rd := emptyReads()
	rd.Nodes["worker-1"] = nodeWithReady("worker-1", corev1.ConditionFalse)
	d := Decide(workloadWith(h), rd).Decisions[0]
	if d.Outcome != Unverified || d.Evidence != "not re-read: the read budget was spent first" {
		t.Errorf("an empty Object must never look anything up, got %q %q", d.Outcome, d.Evidence)
	}
}

func TestDecideSkipsRuledOut(t *testing.T) {
	h := nodeCandidateOn("worker-1", "NotReady")
	h.Verdict = inventory.VerdictRuledOut
	rd := emptyReads()
	rd.Nodes["worker-1"] = nodeWithReady("worker-1", corev1.ConditionFalse)
	r := Decide(workloadWith(h), rd)
	if len(r.Decisions) != 0 || r.Decided {
		t.Errorf("a ruled-out candidate is never re-checked, got %+v", r)
	}
}

func TestDecideNoCandidates(t *testing.T) {
	r := Decide(workloadWith(), emptyReads())
	if r.Workload != "shop/web" || r.Decided || len(r.Decisions) != 0 {
		t.Errorf("got %+v", r)
	}
}

func TestDecideRowFirstConfirmedWins(t *testing.T) {
	rd := emptyReads()
	rd.Nodes["w1"] = nodeWithReady("w1", corev1.ConditionTrue)  // refuted
	rd.Nodes["w2"] = nodeWithReady("w2", corev1.ConditionTrue)  // unverified (heartbeat)
	rd.Nodes["w3"] = nodeWithReady("w3", corev1.ConditionFalse) // confirmed
	rd.Nodes["w4"] = nodeWithReady("w4", corev1.ConditionFalse) // confirmed
	refuted := nodeCandidateOn("w1", "NotReady")
	unverified := nodeCandidateOn("w2", "no kubelet lease")
	confirmed := nodeCandidateOn("w3", "NotReady")
	confirmed2 := nodeCandidateOn("w4", "NotReady")
	cases := []struct {
		name    string
		trace   []inventory.Hypothesis
		decided bool
		cause   string
		outcome Outcome
	}{
		{"confirmed last still wins", []inventory.Hypothesis{refuted, unverified, confirmed}, true, "node w3 (NotReady)", Confirmed},
		{"confirmed beats unverified", []inventory.Hypothesis{unverified, confirmed}, true, "node w3 (NotReady)", Confirmed},
		{"unverified beats refuted", []inventory.Hypothesis{refuted, unverified}, true, "node w2 (no kubelet lease)", Unverified},
		{"all refuted is undecided", []inventory.Hypothesis{refuted}, false, "", ""},
		{"first confirmed wins", []inventory.Hypothesis{confirmed, confirmed2}, true, "node w3 (NotReady)", Confirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Decide(workloadWith(tc.trace...), rd)
			if r.Decided != tc.decided || r.Cause != tc.cause || r.Outcome != tc.outcome {
				t.Errorf("got decided=%v cause=%q outcome=%q, want %v %q %q", r.Decided, r.Cause, r.Outcome, tc.decided, tc.cause, tc.outcome)
			}
			if len(r.Decisions) != len(tc.trace) {
				t.Errorf("every non-ruled-out candidate gets a decision: got %d, want %d", len(r.Decisions), len(tc.trace))
			}
		})
	}
}

func TestDecideUnknownKindIsUnverified(t *testing.T) {
	h := inventory.Hypothesis{Cause: "dns cluster (SERVFAIL)", Kind: "dns", Object: "cluster",
		Verdict: inventory.VerdictAttributed, Reason: "lookups fail"}
	r := Decide(workloadWith(h), emptyReads())
	if d := r.Decisions[0]; d.Outcome != Unverified || d.Evidence != "no rule re-checks this candidate kind" {
		t.Errorf("got %q %q", d.Outcome, d.Evidence)
	}
	if !r.Decided || r.Outcome != Unverified {
		t.Errorf("an unverified-only row is decided as unverified, got %+v", r)
	}
}
