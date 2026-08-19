package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/redact"
	"github.com/imantaba/kubeagent/internal/safetext"
)

func call(name string, input map[string]string) toolCall {
	b, _ := json.Marshal(input)
	return toolCall{ID: "t1", Name: name, Input: b}
}

func TestReader_DescribePod_StructuredNoSecrets(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{
				Name:    "web",
				Command: []string{"/bin/secret-launcher"},
				Args:    []string{"--token=SECRETARG"},
				Env: []corev1.EnvVar{{
					Name:  "DB_PASSWORD",
					Value: "SECRETENV",
				}},
			}},
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.1.2.3",
			HostIP: "192.168.5.5",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", Ready: false, RestartCount: 5,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting",
				}},
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "CrashLoopBackOff") || !strings.Contains(res.Content, "restarts=5") {
		t.Errorf("missing structured status: %q", res.Content)
	}
	// Egress invariant: none of these forbidden fields must appear in output.
	for _, forbidden := range []string{"10.1.2.3", "192.168.5.5", "SECRETARG", "SECRETENV", "DB_PASSWORD", "secret-launcher"} {
		if strings.Contains(res.Content, forbidden) {
			t.Errorf("forbidden field %q leaked into tool output: %q", forbidden, res.Content)
		}
	}
}

func TestReader_OutOfScope_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "other", "name": "x",
	}), NewScope(nil))
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("out-of-scope call must return an error result, got %+v", res)
	}
}

func TestReader_UnknownKind_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	s := NewScope(nil)
	s.Add("secret", "shop", "creds")
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "secret", "namespace": "shop", "name": "creds",
	}), s)
	if !res.IsError {
		t.Errorf("unknown/unsupported kind must return an error result, got %+v", res)
	}
}

func TestReader_GetEvents_ForInScopeObject(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web-abc.1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Name: "web-abc", Namespace: "shop"},
		Reason:         "BackOff", Message: "Back-off pulling image", Count: 3,
	}
	r := Reader{client: fake.NewSimpleClientset(ev)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_events", map[string]string{
		"namespace": "shop", "name": "web-abc",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "BackOff") {
		t.Errorf("expected events, got %+v", res)
	}
}

func TestReader_DescribeWorkload_StructuredNoSecrets(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Replicas:      3,
		},
	}
	r := Reader{client: fake.NewSimpleClientset(dep)}
	s := NewScope(nil)
	s.Add("deployment", "shop", "web")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "deployment", "namespace": "shop", "name": "web",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "ready=1/3") {
		t.Errorf("missing structured readiness in output: %q", res.Content)
	}
}

func TestReader_GetRelated_OwnerAddsToScope(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5f"}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "owner",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "web-5f") {
		t.Fatalf("expected owner, got %+v", res)
	}
	if !s.Allowed("replicaset", "shop", "web-5f") {
		t.Error("resolved owner must be added to scope")
	}
}

// --- R211/R212: free-text fields (condition/waiting/terminated reasons and
// messages, event reasons and messages) are not validated by the API server,
// so they pass through sanitize (safetext.Line, then redact.Addresses) on
// their way into a tool result.

// TestSanitize_TruncatesToMaxLine pins R212's length bound: sanitize never
// hands the model more than safetext.MaxLine runes of a single free-text
// field, ellipsis included in that budget.
func TestSanitize_TruncatesToMaxLine(t *testing.T) {
	long := strings.Repeat("a", safetext.MaxLine+50)
	got := sanitize(long)
	if r := []rune(got); len(r) != safetext.MaxLine {
		t.Errorf("sanitize(...) length = %d runes, want exactly MaxLine=%d", len(r), safetext.MaxLine)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("sanitize(...) = %q, want a truncated line to end with safetext.Line's ellipsis", got)
	}
}

// TestSanitize_DropsControlAndFormatCharacters pins the other half of R212:
// a Unicode formatting character (category Cf, e.g. U+202E RIGHT-TO-LEFT
// OVERRIDE) is dropped rather than rendered.
func TestSanitize_DropsControlAndFormatCharacters(t *testing.T) {
	got := sanitize("before‮after")
	if strings.ContainsRune(got, '‮') {
		t.Errorf("sanitize(...) = %q, want the Cf override character dropped", got)
	}
	if got != "beforeafter" {
		t.Errorf("sanitize(...) = %q, want %q", got, "beforeafter")
	}
}

// TestSanitize_OrderMatters is the executable form of CORRECTION 8's
// justification for sanitize's internal order (safetext.Line first, then
// redact.Addresses -- never the reverse). A control character can sit
// inside an address and split it, which breaks the address regexp's match.
// Sanitizing first repairs the split before the regexp ever runs, so the
// address is caught; redacting first tests the still-split text, misses it,
// and only afterwards has Line strip the character that was hiding it --
// leaving the address in the clear. This is the opposite of the project's
// usual "match on the raw value" rule, and deliberately so: here the raw
// value is what lets the address evade the match.
func TestSanitize_OrderMatters(t *testing.T) {
	// No port, deliberately: with a port suffix, the fragment left after the
	// override splits the four-part quad can still satisfy the address
	// regexp's separate, more permissive "hostname:port" alternative (e.g.
	// "0.10:53" alone looks like a two-label host with a port), which would
	// redact a piece of the address even in the wrong order and blur the
	// point. A bare IP has no such fallback: split into "10.96." and
	// "0.10", neither the four-group IP alternative nor the ":port"
	// alternative matches either fragment.
	raw := "connecting to 10.96.‮0.10 failed"

	got := sanitize(raw)
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("sanitize(...) = %q, want the control-character-split address redacted", got)
	}
	if strings.Contains(got, "10.96.0.10") {
		t.Errorf("sanitize(...) = %q, the repaired address must not survive in the clear", got)
	}

	// The rejected order: redact first (on text the control character still
	// splits), then sanitize.
	redactedFirst := safetext.Line(redact.Addresses(raw))
	if !strings.Contains(redactedFirst, "10.96.0.10") {
		t.Errorf("redact-then-sanitize unexpectedly caught the split address (%q) -- if the regexp now matches through a Cf character, CORRECTION 8's justification for sanitize's order needs revisiting", redactedFirst)
	}
}

// TestReader_DescribePod_SanitizesFreeTextFields proves R211/R212 are wired
// into all four describePod expressions: the pod condition Reason, the
// waiting container's Reason and Message, and the terminated container's
// Reason. Structured fields (the container name, restart count, exit code)
// must survive untouched.
func TestReader_DescribePod_SanitizesFreeTextFields(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse,
				Reason: "Blocked by 10.96.0.10:53",
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "web", RestartCount: 2,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "CrashLoopBackOff", Message: "back-off talking to 10.96.0.11:53",
					}},
				},
				{
					Name: "sidecar",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "OOMKilled near 10.96.0.12:53", ExitCode: 137,
					}},
				},
			},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, addr := range []string{"10.96.0.10:53", "10.96.0.11:53", "10.96.0.12:53"} {
		if strings.Contains(res.Content, addr) {
			t.Errorf("address %q leaked unredacted into: %q", addr, res.Content)
		}
	}
	if n := strings.Count(res.Content, "<redacted>"); n != 3 {
		t.Errorf("want 3 redactions (condition reason, waiting message, terminated reason), got %d in: %q", n, res.Content)
	}
	if !strings.Contains(res.Content, "CrashLoopBackOff") || !strings.Contains(res.Content, "restarts=2") || !strings.Contains(res.Content, "exit 137") {
		t.Errorf("structured fields must survive sanitizing untouched: %q", res.Content)
	}
}

// TestReader_DescribePod_URLResidualSurvivesRedaction pins the accepted
// residual: redact.Addresses matches bare host:port and IP:port shapes, not
// an arbitrary URL, so a registry address quoted inside an image-pull
// failure keeps its scheme and path intact even after sanitizing. This is
// deliberate, not a bug -- pin it so a later change to the regexp cannot
// silently widen or narrow it unnoticed.
func TestReader_DescribePod_URLResidualSurvivesRedaction(t *testing.T) {
	const wantURL = "https://registry.example.com/v2/library/nginx/manifests/v1"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ErrImagePull",
					Message: `Failed to pull image: Get "` + wantURL + `": unauthorized`,
				}},
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, wantURL) {
		t.Errorf("expected the accepted residual (URL survives sanitizing unredacted), got: %q", res.Content)
	}
}

// TestReader_DescribeWorkload_SanitizesConditionMessage proves R211/R212 are
// wired into describeWorkload's deployment condition Reason and Message.
func TestReader_DescribeWorkload_SanitizesConditionMessage(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
				Reason: "MinimumReplicasUnavailable", Message: "cannot reach 10.96.0.20:53",
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(dep)}
	s := NewScope(nil)
	s.Add("deployment", "shop", "web")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "deployment", "namespace": "shop", "name": "web",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "10.96.0.20:53") {
		t.Errorf("address leaked into deployment condition output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "<redacted>") {
		t.Errorf("want the address redacted: %q", res.Content)
	}
	if !strings.Contains(res.Content, "MinimumReplicasUnavailable") {
		t.Errorf("structured reason must survive: %q", res.Content)
	}
}

// TestReader_DescribeNode_SanitizesConditionMessage proves R211/R212 are
// wired into describeNode's condition Reason and Message -- the site neither
// decision names by line, but CORRECTION 2 requires it wrapped too.
func TestReader_DescribeNode_SanitizesConditionMessage(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: corev1.ConditionFalse,
				Reason: "KubeletNotReady", Message: "container runtime unreachable at 10.96.0.30:6443",
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(node)}
	s := NewScope(nil)
	s.Add("node", "", "node-1")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "node", "namespace": "", "name": "node-1",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, "10.96.0.30:6443") {
		t.Errorf("address leaked into node condition output: %q", res.Content)
	}
	if !strings.Contains(res.Content, "<redacted>") {
		t.Errorf("want the address redacted: %q", res.Content)
	}
	if !strings.Contains(res.Content, "KubeletNotReady") {
		t.Errorf("structured reason must survive: %q", res.Content)
	}
}

// TestReader_GetEvents_SanitizesReasonAndMessage proves R211/R212 are wired
// into getEvents' event Reason and Message.
func TestReader_GetEvents_SanitizesReasonAndMessage(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web-abc.1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Name: "web-abc", Namespace: "shop"},
		Reason:         "FailedMount at 10.96.0.40:2049", Message: "unable to mount volume from 10.96.0.41:2049", Count: 1,
	}
	r := Reader{client: fake.NewSimpleClientset(ev)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("get_events", map[string]string{
		"namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, addr := range []string{"10.96.0.40:2049", "10.96.0.41:2049"} {
		if strings.Contains(res.Content, addr) {
			t.Errorf("address %q leaked into event output: %q", addr, res.Content)
		}
	}
	if n := strings.Count(res.Content, "<redacted>"); n != 2 {
		t.Errorf("want 2 redactions (event reason and message), got %d in: %q", n, res.Content)
	}
}

// TestReader_GetRelated_Owner_SanitizesKindAndName proves the "owner" arm of
// getRelated runs OwnerReference.Kind and .Name through safetext.Line before
// either reaches the rendered tool result or the Scope entry added for the
// resolved owner. Both must agree on the sanitized form: a scope entry built
// from the raw name and a rendered line built from the sanitized one would
// disagree about what is in scope, which is worse than skipping sanitizing
// altogether.
func TestReader_GetRelated_Owner_SanitizesKindAndName(t *testing.T) {
	const bel = "\a" // a control character (BEL); safetext.Line drops it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "Replica" + bel + "Set",
				Name: "web-5f" + bel + "x",
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "owner",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, bel) {
		t.Errorf("control character leaked unsanitized into tool result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "ReplicaSet") || !strings.Contains(res.Content, "web-5fx") {
		t.Errorf("expected the sanitized owner Kind and Name in the tool result, got: %q", res.Content)
	}
	if !s.Allowed("replicaset", "shop", "web-5fx") {
		t.Error("scope entry must use the sanitized name, not the raw one")
	}
	if s.Allowed("replica"+bel+"set", "shop", "web-5f"+bel+"x") {
		t.Error("scope must not contain an entry built from the raw, unsanitized owner reference")
	}
}

// TestReader_GetRelated_Node_SanitizesNodeName proves the "node" arm of
// getRelated runs spec.nodeName through safetext.Line before either the
// rendered tool result or the Scope entry. Same rule as the owner arm above:
// both sinks must agree on the sanitized form.
func TestReader_GetRelated_Node_SanitizesNodeName(t *testing.T) {
	const bel = "\a" // a control character (BEL); safetext.Line drops it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Spec:       corev1.PodSpec{NodeName: "worker" + bel + "-1"},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "node",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, bel) {
		t.Errorf("control character leaked unsanitized into tool result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "worker-1") {
		t.Errorf("expected the sanitized node name in the tool result, got: %q", res.Content)
	}
	if !s.Allowed("node", "", "worker-1") {
		t.Error("scope entry must use the sanitized node name, not the raw one")
	}
	if s.Allowed("node", "", "worker"+bel+"-1") {
		t.Error("scope must not contain an entry built from the raw, unsanitized node name")
	}
}

// TestReader_GetRelated_PVC_SanitizesClaimName proves the "pvc" arm of
// getRelated runs a volume's claimName through safetext.Line before either
// the rendered tool result or the Scope entry. Same rule as the owner and
// node arms above: both sinks must agree on the sanitized form.
func TestReader_GetRelated_PVC_SanitizesClaimName(t *testing.T) {
	const bel = "\a" // a control character (BEL); safetext.Line drops it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "data" + bel + "-0",
			}},
		}}},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "pvc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if strings.Contains(res.Content, bel) {
		t.Errorf("control character leaked unsanitized into tool result: %q", res.Content)
	}
	if !strings.Contains(res.Content, "data-0") {
		t.Errorf("expected the sanitized claim name in the tool result, got: %q", res.Content)
	}
	if !s.Allowed("pvc", "shop", "data-0") {
		t.Error("scope entry must use the sanitized claim name, not the raw one")
	}
	if s.Allowed("pvc", "shop", "data"+bel+"-0") {
		t.Error("scope must not contain an entry built from the raw, unsanitized claim name")
	}
}

// TestReader_FailedReads_ReduceViaRedactError proves the reader.go doc
// comment's closed gap: a failed client-go read reaches the model as
// op + scheme://host + cause — never the request path or query.
func TestReader_FailedReads_ReduceViaRedactError(t *testing.T) {
	failure := &url.Error{
		Op:  "Get",
		URL: "https://10.96.0.1:6443/api/v1/namespaces/shop/pods/web-abc?timeout=30s",
		Err: errors.New("connection refused"),
	}
	tests := []struct {
		name     string
		resource string // the fake clientset resource the reactor intercepts
		verb     string
		call     toolCall
	}{
		{"describe pod", "pods", "get", call("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})},
		{"describe node", "nodes", "get", call("describe", map[string]string{"kind": "node", "namespace": "", "name": "worker-1"})},
		{"describe pvc", "persistentvolumeclaims", "get", call("describe", map[string]string{"kind": "pvc", "namespace": "shop", "name": "data-0"})},
		{"describe deployment", "deployments", "get", call("describe", map[string]string{"kind": "deployment", "namespace": "shop", "name": "web"})},
		{"describe replicaset", "replicasets", "get", call("describe", map[string]string{"kind": "replicaset", "namespace": "shop", "name": "web-rs"})},
		{"describe statefulset", "statefulsets", "get", call("describe", map[string]string{"kind": "statefulset", "namespace": "shop", "name": "db"})},
		{"describe daemonset", "daemonsets", "get", call("describe", map[string]string{"kind": "daemonset", "namespace": "shop", "name": "logger"})},
		{"describe job", "jobs", "get", call("describe", map[string]string{"kind": "job", "namespace": "shop", "name": "migrate"})},
		{"get_events", "events", "list", call("get_events", map[string]string{"namespace": "shop", "name": "web-abc"})},
		{"get_related", "pods", "get", call("get_related", map[string]string{"namespace": "shop", "name": "web-abc", "relation": "owner"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor(tt.verb, tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, failure
			})
			s := NewScope(nil)
			s.Add("pod", "shop", "web-abc")
			s.Add("node", "", "worker-1")
			s.Add("pvc", "shop", "data-0")
			s.Add("deployment", "shop", "web")
			s.Add("replicaset", "shop", "web-rs")
			s.Add("statefulset", "shop", "db")
			s.Add("daemonset", "shop", "logger")
			s.Add("job", "shop", "migrate")
			r := Reader{client: client}
			res := r.execute(context.Background(), tt.call, s)
			if !res.IsError {
				t.Fatalf("expected an error result, got %+v", res)
			}
			if !strings.Contains(res.Content, "https://10.96.0.1:6443") || !strings.Contains(res.Content, "connection refused") {
				t.Errorf("want op + scheme://host + cause to survive, got %q", res.Content)
			}
			for _, leaked := range []string{"/api/v1/namespaces", "timeout=30s"} {
				if strings.Contains(res.Content, leaked) {
					t.Errorf("request path/query leaked into the tool result: %q", res.Content)
				}
			}
		})
	}
}

// logCauseResult is pure so every arm is testable: the fake clientset's
// GetLogs serves a fixed body, so content-dependent arms cannot be driven
// through it.
func TestLogCauseResult_RefusalNamesPodsLogPermission(t *testing.T) {
	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-abc", errors.New("no access")),
		apierrors.NewUnauthorized("no bearer token"),
	} {
		res := logCauseResult("t1", "shop", "web-abc", "app", "", false, err)
		if !res.IsError {
			t.Fatalf("expected an error result for %v", err)
		}
		want := "reading the previous log of shop/web-abc was refused: this identity lacks the pods/log get permission"
		if res.Content != want {
			t.Errorf("content = %q, want %q", res.Content, want)
		}
	}
}

func TestLogCauseResult_OtherErrorReducesViaRedactError(t *testing.T) {
	err := &url.Error{
		Op:  "Get",
		URL: "https://10.96.0.1:6443/api/v1/namespaces/shop/pods/web-abc/log?previous=true",
		Err: errors.New("connection refused"),
	}
	res := logCauseResult("t1", "shop", "web-abc", "app", "", false, err)
	if !res.IsError {
		t.Fatal("expected an error result")
	}
	if !strings.Contains(res.Content, "https://10.96.0.1:6443") || strings.Contains(res.Content, "/api/v1/") {
		t.Errorf("want scheme://host only, got %q", res.Content)
	}
}

func TestLogCauseResult_NoPreviousInstance(t *testing.T) {
	res := logCauseResult("t1", "shop", "web-abc", "app", "", false, nil)
	if res.IsError {
		t.Fatalf("no previous instance is not an error: %q", res.Content)
	}
	want := `no previous-instance log for shop/web-abc container "app" (nothing was refused; the container may not have restarted)`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

// TestLogCauseResult_ExcerptNeverCrosses is the boundary proof: a classified
// log returns ONLY the fixed-vocabulary cause. No line of the log itself —
// addresses, tokens, anything — reaches the result.
func TestLogCauseResult_ExcerptNeverCrosses(t *testing.T) {
	log := "dial tcp 10.96.0.10:6379: connect: connection refused\ntoken=not-a-real-routing-key"
	res := logCauseResult("t1", "shop", "web-abc", "app", log, true, nil)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if !strings.HasPrefix(res.Content, "log cause: ") {
		t.Errorf("want a classified cause, got %q", res.Content)
	}
	for _, leaked := range []string{"10.96.0.10", "dial tcp", "not-a-real-routing-key", "token="} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("raw log content crossed the boundary: %q in %q", leaked, res.Content)
		}
	}
}

func TestLogCauseResult_FallbackCauseOnly(t *testing.T) {
	res := logCauseResult("t1", "shop", "web-abc", "app", "something odd happened", true, nil)
	want := "log cause: last output before exit (no signature in the last 25 lines)"
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
	if strings.Contains(res.Content, "something odd happened") {
		t.Errorf("the last line itself must not cross: %q", res.Content)
	}
}

func TestLogCauseResult_UnclassifiableLog(t *testing.T) {
	// A placeholder-only log classifies to the zero Clue (Cause == "").
	log := "unable to retrieve container logs for containerd://0123456789abcdef"
	res := logCauseResult("t1", "shop", "web-abc", "app", log, true, nil)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	want := `the previous log of shop/web-abc container "app" has no classifiable output`
	if res.Content != want {
		t.Errorf("content = %q, want %q", res.Content, want)
	}
}

func TestReader_GetLogCauses_OutOfScopeIsRefused(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	s := NewScope(nil)
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("out-of-scope pod must be refused, got %+v", res)
	}
}

// TestReader_GetLogCauses_WiredThroughExecute drives the real path: the fake
// clientset serves its fixed "fake logs" body, which classifies to the
// fallback cause — and the body itself must not appear in the result.
func TestReader_GetLogCauses_WiredThroughExecute(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.Content)
	}
	if res.Content != "log cause: last output before exit (no signature in the last 25 lines)" {
		t.Errorf("content = %q", res.Content)
	}
	if strings.Contains(res.Content, "fake logs") {
		t.Errorf("the raw log body crossed the boundary: %q", res.Content)
	}
}

func TestReader_GetLogCauses_ForbiddenViaReactor(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "web-abc", errors.New("no access"))
	})
	r := Reader{client: client}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_log_causes", map[string]string{"namespace": "shop", "pod": "web-abc", "container": "app"}), s)
	if !res.IsError || !strings.Contains(res.Content, "pods/log get permission") {
		t.Errorf("a forbidden read must name the missing permission, got %+v", res)
	}
	if strings.Contains(res.Content, "no access") {
		t.Errorf("the API error's own text must not pass through the refusal arm: %q", res.Content)
	}
}
