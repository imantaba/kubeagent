package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/scan"
)

func p32(n int32) *int32 { return &n }

func callInspect(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) InspectOutput {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "kubeagent_inspect", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	var out InspectOutput
	blob, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

func TestInspect_PodReturnsItsFindingsAndEvents(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "payments", Name: "e1"},
		InvolvedObject: corev1.ObjectReference{Namespace: "payments", Name: "api-abc"},
		Reason:         "BackOff", Message: "back-off restarting failed container", Type: "Warning", Count: 5,
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(crashingPod(), event))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "payments", "name": "api-abc"})

	if !out.Found {
		t.Fatal("Found = false, want the pod to be found")
	}
	if len(out.Findings) == 0 {
		t.Error("Findings is empty on a crash-looping pod")
	}
	if len(out.Events) == 0 {
		t.Fatal("Events is empty; inspect exists to surface the events triage summarises away")
	}
	if out.Events[0].Reason != "BackOff" || out.Events[0].Count != 5 {
		t.Errorf("Events[0] = %+v, want the BackOff event with count 5", out.Events[0])
	}
	if out.Events[0].Age == "" {
		t.Error("Events[0].Age is empty; a model cannot judge relevance without it")
	}
}

func TestInspect_MissingObjectIsNotFoundNotAnError(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "payments", "name": "ghost"})

	if out.Found {
		t.Error("Found = true for an object that does not exist")
	}
	if out.Findings == nil || out.Events == nil || out.Pods == nil {
		t.Error("a not-found result must still carry empty lists, so absent and empty stay distinguishable")
	}
	if out.Coverage == nil {
		t.Error("Coverage is nil; every result carries the honesty contract")
	}
}

func TestInspect_UnknownKindIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "kubeagent_inspect",
		Arguments: map[string]any{"kind": "secret", "namespace": "payments", "name": "creds"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded for kind=secret; the published schema's enum must reject it")
	}
}

func TestInspect_MissingRequiredArgumentIsRejected(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset())

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_inspect", Arguments: map[string]any{"kind": "pod"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded without a name; name is required")
	}
	text := firstText(res)
	if !strings.Contains(text, "name") {
		t.Errorf("error text = %q, want it to name the missing property", text)
	}
}

// firstText returns the first text block of a tool result, for asserting on
// SDK-generated validation messages.
func firstText(res *mcpsdk.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// TestInspect_FlaggedWorkloadWithoutPerPodFindingStillReportsAFinding covers a
// Deployment stuck at 0/3 ready with no crash-looping pod behind it (no pods
// at all, in this fixture): the workload has no per-pod diagnose.Finding, but
// it is Flagged() (Ready < Desired). Mirroring view.go's findingsFromResult,
// inspect must still project it through fromWorkload rather than reporting
// "findings: []" for an object triage would flag.
func TestInspect_FlaggedWorkloadWithoutPerPodFindingStillReportsAFinding(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "worker"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(3)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(dep))

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "payments", "name": "worker"})

	if !out.Found {
		t.Fatal("Found = false, want the deployment to be found")
	}
	if out.Ready != 0 || out.Desired != 3 {
		t.Fatalf("Ready/Desired = %d/%d, want 0/3", out.Ready, out.Desired)
	}
	if len(out.Findings) != 1 {
		t.Fatalf("Findings = %+v, want exactly one workload-level finding for a flagged "+
			"deployment with no per-pod finding", out.Findings)
	}
	f := out.Findings[0]
	if f.Severity != "warning" || f.Kind != "Deployment" || f.Namespace != "payments" || f.Name != "worker" {
		t.Errorf("Findings[0] = %+v, want a warning on Deployment payments/worker", f)
	}
	if f.Reason != out.Status {
		t.Errorf("Findings[0].Reason = %q, want the workload's status %q", f.Reason, out.Status)
	}
}

// TestInspect_EventsAreSortedMostRecentFirstWithATotalTiebreak feeds events to
// the fake clientset out of chronological order, including a same-timestamp
// pair, and asserts the returned order is most-recent-first with ties broken
// on Reason (then Message, then Count) rather than left to list order.
func TestInspect_EventsAreSortedMostRecentFirstWithATotalTiebreak(t *testing.T) {
	newest := fixedNow.Add(-1 * time.Minute)
	oldest := fixedNow.Add(-30 * time.Minute)
	tied := fixedNow.Add(-10 * time.Minute)

	mk := func(name, reason, message string, count int32, ts time.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "ops", Name: name},
			InvolvedObject: corev1.ObjectReference{Namespace: "ops", Name: "worker"},
			Reason:         reason, Message: message, Type: "Warning", Count: count,
			LastTimestamp: metav1.NewTime(ts),
		}
	}

	oldE := mk("e-old", "Oldest", "z-message", 1, oldest)
	newE := mk("e-new", "Newest", "a-message", 9, newest)
	tiedB := mk("e-tied-b", "Bravo", "second", 2, tied)
	tiedA := mk("e-tied-a", "Alpha", "first", 5, tied)

	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(oldE, tiedB, newE, tiedA))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "ops", "name": "worker"})

	if len(out.Events) != 4 {
		t.Fatalf("Events = %+v, want 4", out.Events)
	}
	want := []string{"Newest", "Alpha", "Bravo", "Oldest"}
	for i, w := range want {
		if out.Events[i].Reason != w {
			t.Errorf("Events[%d].Reason = %q, want %q (full order: %+v)", i, out.Events[i].Reason, w, out.Events)
		}
	}
}

// TestInspect_EventWithNoTimestampsHasUnknownAge covers an event that carries
// none of LastTimestamp, EventTime or FirstTimestamp. inventory.HumanAge would
// render the zero time.Time as a multi-century age; Age must instead be the
// literal "unknown", never a computed (and nonsensical) duration.
func TestInspect_EventWithNoTimestampsHasUnknownAge(t *testing.T) {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "ops", Name: "e-no-ts"},
		InvolvedObject: corev1.ObjectReference{Namespace: "ops", Name: "worker"},
		Reason:         "Scheduled", Message: "no timestamps at all", Type: "Normal", Count: 1,
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(event))

	out := callInspect(t, cs, map[string]any{"kind": "pod", "namespace": "ops", "name": "worker"})

	if len(out.Events) != 1 {
		t.Fatalf("Events = %+v, want 1", out.Events)
	}
	if out.Events[0].Age != "unknown" {
		t.Errorf("Age = %q, want %q for an event carrying none of the three timestamps",
			out.Events[0].Age, "unknown")
	}
}

// TestInspect_EmptyNamespaceIsRejectedBeforeTheHandlerRuns asserts that
// namespace: "" fails SDK schema validation (a validation failure is a
// CallToolResult{IsError: true}, not a JSON-RPC error — only an unknown tool
// name is that) and, via a reactor on the fake clientset, that the handler
// never runs: an empty namespace can never match a real workload, so letting
// it through would only buy a pointless cluster-wide scan.
func TestInspect_EmptyNamespaceIsRejectedBeforeTheHandlerRuns(t *testing.T) {
	cli := fake.NewSimpleClientset()
	called := false
	cli.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		called = true
		return false, nil, nil
	})
	cs := connect(t, Config{Context: "kind-example"}, cli)

	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "kubeagent_inspect",
		Arguments: map[string]any{"kind": "pod", "namespace": "", "name": "api-abc"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v, want a tool-level error result", err)
	}
	if !res.IsError {
		t.Fatal("CallTool() succeeded with namespace=\"\"; MinLength must reject an empty namespace " +
			"before the handler runs")
	}
	if called {
		t.Error("the fake clientset was called; validation must reject namespace=\"\" before the " +
			"handler ever runs")
	}
}

// ctrl builds the controller owner reference chain fixture the resolver tests
// need. internal/inventory's test package has its own copy; this is a different
// package and cannot reach it.
func ctrl(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

// ownedPod is a ready one-container pod owned by the given controller.
func ownedPod(ns, name string, owners []metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app:1.0.0"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "192.0.2.10",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
}

// TestInspect_HealthyDeploymentIsFound is the second half of the resolution
// defect. inspect answered a lookup against inventory.Prioritize's output — a
// list built for display, which drops healthy-quiet workloads outright — so a
// Deployment that is fully ready with no restarts answered found:false for an
// object the cluster plainly has.
func TestInspect_HealthyDeploymentIsFound(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
	}}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, ownedPod("shop", "web-1a2b-cdef", ctrl("ReplicaSet", "web-1a2b"))))

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "shop", "name": "web"})

	if !out.Found {
		t.Fatal("Found = false for a healthy Deployment that exists")
	}
	if out.Kind != "Deployment" || out.Status != "Running" || out.Desired != 1 || out.Ready != 1 {
		t.Errorf("got kind=%q status=%q %d/%d, want Deployment Running 1/1",
			out.Kind, out.Status, out.Ready, out.Desired)
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Errorf("Pods = %+v, want the one pod behind it", out.Pods)
	}
	if len(out.Findings) != 0 {
		t.Errorf("Findings = %+v, want none on a healthy Deployment", out.Findings)
	}
}

// TestResolveObject_FindsEachKindUnderItsOwnKindOnly drives the resolver
// directly — it is a pure function over the snapshot scan.Evaluate returns, so
// it needs no server and no fake clientset. The negative half matters as much
// as the positive: a resolver that ignored the requested kind would answer
// found:true for a Deployment asked about as a StatefulSet.
func TestResolveObject_FindsEachKindUnderItsOwnKindOnly(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	const ns = "shop"
	in := inventory.Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
			Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		}},
		ReplicaSets: []appsv1.ReplicaSet{{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "orphan-rs",
		}}},
		StatefulSets: []appsv1.StatefulSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
			Spec:       appsv1.StatefulSetSpec{Replicas: p32(1)},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}},
		DaemonSets: []appsv1.DaemonSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "agent"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
		}},
		Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "migrate"}}},
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "nightly"},
			Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
		}},
		Pods: []corev1.Pod{
			*ownedPod(ns, "loner", nil),
			// orphan-rs needs a pod to exist as a workload at all: Assemble seeds
			// Deployment, StatefulSet, DaemonSet, CronJob and Job directly from
			// Inputs, but a ReplicaSet workload only materialises from a pod whose
			// owner resolves to one, so this pod is what puts orphan-rs in
			// Assemble's output at all. resolveReplicaSet no longer depends on
			// that: it reads Inputs.ReplicaSets directly, which is what makes a
			// pod-less or Deployment-owned ReplicaSet resolvable too.
			*ownedPod(ns, "orphan-rs-pod", ctrl("ReplicaSet", "orphan-rs")),
		},
	}
	res := scan.Result{Inputs: in}

	cases := []struct {
		kind string
		name string
	}{
		{"pod", "loner"},
		{"deployment", "web"},
		{"replicaset", "orphan-rs"},
		{"statefulset", "db"},
		{"daemonset", "agent"},
		{"job", "migrate"},
		{"cronjob", "nightly"},
	}
	wantKind := map[string]string{
		"pod": "Pod", "deployment": "Deployment", "replicaset": "ReplicaSet",
		"statefulset": "StatefulSet", "daemonset": "DaemonSet", "job": "Job",
		"cronjob": "CronJob",
	}

	for _, tc := range cases {
		got := resolveObject(res, tc.kind, ns, tc.name, now)
		if !got.Found {
			t.Errorf("resolveObject(%q, %q) Found = false, want true", tc.kind, tc.name)
			continue
		}
		if got.Kind != wantKind[tc.kind] {
			t.Errorf("resolveObject(%q, %q) Kind = %q, want %q",
				tc.kind, tc.name, got.Kind, wantKind[tc.kind])
		}
		// The same name under every other kind must not resolve.
		for _, other := range cases {
			if other.kind == tc.kind {
				continue
			}
			if resolveObject(res, other.kind, ns, tc.name, now).Found {
				t.Errorf("resolveObject(%q, %q) Found = true; %q is a %s",
					other.kind, tc.name, tc.name, wantKind[tc.kind])
			}
		}
		// A name nothing carries must not resolve under any kind.
		if resolveObject(res, tc.kind, ns, "ghost", now).Found {
			t.Errorf("resolveObject(%q, %q) Found = true for a name nothing carries", tc.kind, "ghost")
		}
	}
}

// callInspectRaw returns the tool's structured result as a decoded JSON object,
// so a test can assert which keys are *present*. InspectOutput cannot answer
// that: after unmarshalling, an omitempty field that was omitted and one that
// encoded its zero value are the same value.
func callInspectRaw(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_inspect", Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured) error = %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

// crashingOwnedPod is crashingPod's controller-owned twin: the shape that
// actually occurs in a cluster. PodOwners rolls it up to its Deployment, so it
// is never a "Pod" workload in its own right.
func crashingOwnedPod(ns, name, rs string) *corev1.Pod {
	p := ownedPod(ns, name, ctrl("ReplicaSet", rs))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "app",
		RestartCount: 7,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
		}},
	}}
	return p
}

// TestInspect_ControllerOwnedPodIsFoundAndNamesItsOwner is the reported defect.
// kubeagent_triage emits Kind:"Pod" with the pod's own name for every critical
// finding, and the shipped skill tells the model to inspect it with exactly the
// namespace and name the finding supplied. Every controller-owned pod answered
// found:false, because a lookup was being answered against a workload list a
// controller-owned pod is never in.
func TestInspect_ControllerOwnedPodIsFoundAndNamesItsOwner(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
	}}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, crashingOwnedPod("shop", "web-1a2b-cdef", "web-1a2b")))

	out := callInspect(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "web-1a2b-cdef",
	})

	if !out.Found {
		t.Fatal("Found = false for a controller-owned pod that exists; this is the " +
			"identity kubeagent_triage hands out for every critical finding")
	}
	if out.Kind != "Pod" {
		t.Errorf("Kind = %q, want %q — a caller who asked about a pod must not be "+
			"answered about its Deployment", out.Kind, "Pod")
	}
	if out.Status != "Running" {
		t.Errorf("Status = %q, want the pod's own phase %q", out.Status, "Running")
	}
	if out.Owner != "Deployment/web" {
		t.Errorf("Owner = %q, want %q — the escalation pointer, so a caller need not "+
			"guess the owning workload's name", out.Owner, "Deployment/web")
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Fatalf("Pods = %+v, want exactly the one pod asked about", out.Pods)
	}
	if out.Pods[0].Restarts != 7 {
		t.Errorf("Pods[0].Restarts = %d, want 7", out.Pods[0].Restarts)
	}
	if len(out.Findings) != 1 {
		t.Fatalf("Findings = %+v, want the pod's own critical finding", out.Findings)
	}
	f := out.Findings[0]
	if f.Severity != "critical" || f.Kind != "Pod" || f.Name != "web-1a2b-cdef" {
		t.Errorf("Findings[0] = %+v, want a critical finding on the pod itself", f)
	}
}

// TestResolvePod_StampsTheRowFromTheInjectedClock pins resolvePod's now
// parameter to the pod row it builds. The clock is far from any plausible wall
// clock on purpose: with now near time.Now(), a 72h-old pod lands in HumanAge's
// "3d" bucket either way, and swapping the parameter for time.Now() would not
// fail this test.
func TestResolvePod_StampsTheRowFromTheInjectedClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := ownedPod("shop", "loner", nil)
	p.CreationTimestamp = metav1.NewTime(now.Add(-72 * time.Hour))
	res := scan.Result{Inputs: inventory.Inputs{Pods: []corev1.Pod{*p}}}

	got := resolveObject(res, "pod", "shop", "loner", now)
	if !got.Found {
		t.Fatal("Found = false for a bare pod that exists")
	}
	if len(got.Pods) != 1 {
		t.Fatalf("Pods = %+v, want exactly one row", got.Pods)
	}
	if got.Pods[0].Age != "3d" {
		t.Errorf("Pods[0].Age = %q, want %q — the row must be stamped from the "+
			"injected clock, not from the wall clock", got.Pods[0].Age, "3d")
	}
}

// TestInspect_PodAnswerHasNoDesiredOrReadyKey guards the rule that absence must
// never read as zero. A pod has no replica count; encoding desired:0 ready:0
// would tell a model the pod is scaled to nothing.
func TestInspect_PodAnswerHasNoDesiredOrReadyKey(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(ownedPod("shop", "loner", nil)))

	raw := callInspectRaw(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "loner",
	})

	if raw["found"] != true {
		t.Fatalf("found = %v, want true (result: %+v)", raw["found"], raw)
	}
	for _, key := range []string{"desired", "ready"} {
		if v, ok := raw[key]; ok {
			t.Errorf("a pod answer carries %q = %v; both are omitempty and a pod has "+
				"no replica count, so absence must never render as zero", key, v)
		}
	}
}

// TestInspect_BarePodHasNoOwnerKey — a pod with no controller must not claim an
// owner it does not have.
func TestInspect_BarePodHasNoOwnerKey(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(ownedPod("shop", "loner", nil)))

	raw := callInspectRaw(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "loner",
	})

	if v, ok := raw["owner"]; ok {
		t.Errorf("owner = %v on a bare pod, want the key absent", v)
	}
}

// TestInspect_JobPodPastTheCapIsStillInspectable covers the third way the old
// lookup lost an object that exists: Assemble truncates a Job's or CronJob's pod
// rows at jobPodCap (3), which is right for a report and wrong for a lookup. The
// pod path reads res.Inputs.Pods directly, so the fourth pod is findable.
func TestInspect_JobPodPastTheCapIsStillInspectable(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "migrate"}}
	objs := []runtime.Object{job}
	for _, n := range []string{"migrate-1", "migrate-2", "migrate-3", "migrate-4"} {
		objs = append(objs, ownedPod("shop", n, ctrl("Job", "migrate")))
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(objs...))

	// The owning Job's own answer is capped at three rows, by design.
	viaJob := callInspect(t, cs, map[string]any{"kind": "job", "namespace": "shop", "name": "migrate"})
	if len(viaJob.Pods) != 3 {
		t.Fatalf("the Job answer carries %d pod rows, want jobPodCap (3) — the fixture "+
			"no longer exercises truncation", len(viaJob.Pods))
	}

	out := callInspect(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "migrate-4",
	})
	if !out.Found {
		t.Fatal("Found = false for a Job's fourth pod; a display cap must not hide an " +
			"object from a lookup")
	}
	if out.Owner != "Job/migrate" {
		t.Errorf("Owner = %q, want %q", out.Owner, "Job/migrate")
	}
}

// TestInspect_DeploymentOwnedReplicaSetIsFound is the common case and the third
// facet of the reported defect. A Deployment's ReplicaSets are what its
// rollouts are made of, and every one of them answered found:false: Assemble
// never seeds a ReplicaSet from Inputs.ReplicaSets, and PodOwners rolls a
// Deployment-owned ReplicaSet's pods up to the Deployment, so no ReplicaSet
// workload is ever built for it. It runs through the tool rather than calling
// the resolver directly, because it asserts the pod's finding and a finding
// exists only after a real scan.
func TestInspect_DeploymentOwnedReplicaSetIsFound(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: p32(1)},
		Status: appsv1.ReplicaSetStatus{ReadyReplicas: 0},
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, crashingOwnedPod("shop", "web-1a2b-cdef", "web-1a2b")))

	out := callInspect(t, cs, map[string]any{
		"kind": "replicaset", "namespace": "shop", "name": "web-1a2b",
	})

	if !out.Found {
		t.Fatal("Found = false for a Deployment-owned ReplicaSet that exists; " +
			"Assemble never seeds one, so the resolver must read Inputs.ReplicaSets")
	}
	if out.Kind != "ReplicaSet" {
		t.Errorf("Kind = %q, want %q", out.Kind, "ReplicaSet")
	}
	if out.Desired != 1 || out.Ready != 0 {
		t.Errorf("Desired/Ready = %d/%d, want 1/0 — taken from the ReplicaSet's own "+
			"spec and status", out.Desired, out.Ready)
	}
	if out.Status != "Degraded" {
		t.Errorf("Status = %q, want %q", out.Status, "Degraded")
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Fatalf("Pods = %+v, want exactly the ReplicaSet's own one pod", out.Pods)
	}
	if len(out.Findings) != 1 || out.Findings[0].Name != "web-1a2b-cdef" {
		t.Errorf("Findings = %+v, want the crash finding on its own pod", out.Findings)
	}
	if out.Owner != "" {
		t.Errorf("Owner = %q, want empty — owner is the pod path's escalation "+
			"pointer and is documented as absent for every other kind", out.Owner)
	}
}

// TestResolveReplicaSet_WithNoPodsIsFound covers the second unresolvable
// ReplicaSet: an old revision scaled to zero. It has no pods, so nothing can
// materialise a workload for it — and it is exactly the object an operator asks
// about mid-rollout.
func TestResolveReplicaSet_WithNoPodsIsFound(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	res := scan.Result{Inputs: inventory.Inputs{
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-0old"},
			Spec:       appsv1.ReplicaSetSpec{Replicas: p32(0)},
		}},
	}}

	got := resolveObject(res, "replicaset", "shop", "web-0old", now)

	if !got.Found {
		t.Fatal("Found = false for a ReplicaSet scaled to zero; a lookup must not " +
			"need the object to have pods")
	}
	if got.Status != "Scaled Down" {
		t.Errorf("Status = %q, want %q", got.Status, "Scaled Down")
	}
	if len(got.Pods) != 0 {
		t.Errorf("Pods = %+v, want none", got.Pods)
	}
}

// TestResolveReplicaSet_CarriesOnlyItsOwnPods pins the owner filter. Two
// revisions of one Deployment run side by side during a rollout; asking about
// one must not return the other's pods. It also pins that the resolver's now
// parameter, not the wall clock, drives the pod row's Age: the fixed clock
// here is set far from any plausible real time, so swapping now for
// time.Now() inside the resolver would fail this on Age alone.
func TestResolveReplicaSet_CarriesOnlyItsOwnPods(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newPod := ownedPod("shop", "web-new-aaaa", ctrl("ReplicaSet", "web-new"))
	newPod.CreationTimestamp = metav1.NewTime(now.Add(-72 * time.Hour))
	res := scan.Result{Inputs: inventory.Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec:       appsv1.DeploymentSpec{Replicas: p32(2)},
		}},
		ReplicaSets: []appsv1.ReplicaSet{
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-new",
					OwnerReferences: ctrl("Deployment", "web")},
				Spec: appsv1.ReplicaSetSpec{Replicas: p32(1)},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-old",
					OwnerReferences: ctrl("Deployment", "web")},
				Spec: appsv1.ReplicaSetSpec{Replicas: p32(1)},
			},
		},
		Pods: []corev1.Pod{
			*newPod,
			*ownedPod("shop", "web-old-bbbb", ctrl("ReplicaSet", "web-old")),
		},
	}}

	got := resolveObject(res, "replicaset", "shop", "web-new", now)

	if len(got.Pods) != 1 || got.Pods[0].Name != "web-new-aaaa" {
		t.Fatalf("Pods = %+v, want only web-new's own pod — the other revision's "+
			"pod belongs to web-old", got.Pods)
	}
	if got.Pods[0].Age != "3d" {
		t.Errorf("Pods[0].Age = %q, want %q — the resolver's now parameter must drive "+
			"the row's age, not the wall clock", got.Pods[0].Age, "3d")
	}
}
