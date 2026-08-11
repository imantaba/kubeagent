package diagnose

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pfPod builds a Running-but-not-Ready pod with one Running container.
func pfPod(ns, name, container string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  container,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

// pfEvent builds an Unhealthy probe event targeting a pod's container.
func pfEvent(ns, pod, container, message string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: ns, Name: pod + ".ev"},
		Reason:         "Unhealthy",
		Type:           "Warning",
		Message:        message,
		LastTimestamp:  metav1.Now(),
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: ns, Name: pod, FieldPath: "spec.containers{" + container + "}"},
	}
}

// pfNow is the instant the probe-ranking fixtures below treat as T0. The
// detector reads no clock — the window is anchored on the newest event — so
// this is only a fixed origin for the fixtures, not an injected now.
var pfNow = time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC)

// pfEventAt is pfEvent with an explicit LastTimestamp, relative to pfNow.
func pfEventAt(ns, pod, container, message string, ago time.Duration) corev1.Event {
	ev := pfEvent(ns, pod, container, message)
	ev.Name = pod + "." + container + "." + message
	ev.LastTimestamp = metav1.NewTime(pfNow.Add(-ago))
	return ev
}

// pfMultiPod is pfPod with several Running containers.
func pfMultiPod(ns, name string, containers ...string) *corev1.Pod {
	pod := pfPod(ns, name, containers[0])
	for _, c := range containers[1:] {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:  c,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		})
	}
	return pod
}

const (
	pfReadinessHTTP = "Readiness probe failed: HTTP probe failed with statuscode: 503"
	pfLivenessRefus = "Liveness probe failed: dial tcp 10.0.0.1:8080: connect: connection refused"
	pfStartupTimout = `Startup probe failed: Get "http://10.0.0.1/": context deadline exceeded`
)

func TestProbeFailureDetector_ReadinessHTTP(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a ProbeFailure finding, got nil")
	}
	if f.Issue != "ProbeFailure" || f.Container != "web" {
		t.Errorf("Issue/Container = %q/%q, want ProbeFailure/web", f.Issue, f.Container)
	}
	if want := `container "web": readiness probe failed — HTTP 503`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
	if !strings.Contains(f.Reason, "readiness probe keeps failing") {
		t.Errorf("Reason = %q, want it to name the readiness probe", f.Reason)
	}
}

func TestProbeFailureDetector_NoPodIPLeak(t *testing.T) {
	msg := `Liveness probe failed: Get "http://10.244.1.5:8080/healthz": dial tcp 10.244.1.5:8080: connect: connection refused`
	facts := PodFacts{Pod: pfPod("shop", "api-1", "api"), Events: []corev1.Event{pfEvent("shop", "api-1", "api", msg)}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding")
	}
	if strings.Contains(f.Evidence, "10.244.1.5") || strings.Contains(f.Reason, "10.244.1.5") {
		t.Errorf("pod IP leaked: Evidence=%q Reason=%q", f.Evidence, f.Reason)
	}
	if want := `container "api": liveness probe failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestProbeFailureDetector_SkipsWaitingContainer(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := (ProbeFailureDetector{}).Detect(facts); f != nil {
		t.Errorf("a Waiting (CrashLoopBackOff) container must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_SkipsReadyPod(t *testing.T) {
	pod := pfPod("shop", "web-1", "web")
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	facts := PodFacts{Pod: pod, Events: []corev1.Event{
		pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503"),
	}}
	if f := (ProbeFailureDetector{}).Detect(facts); f != nil {
		t.Errorf("a Ready pod must not be flagged, got %+v", f)
	}
}

func TestProbeFailureDetector_FallbackNoFieldPath(t *testing.T) {
	ev := pfEvent("shop", "web-1", "web", "Readiness probe failed: HTTP probe failed with statuscode: 503")
	ev.InvolvedObject.FieldPath = ""
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{ev}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("with empty FieldPath but pod Running+notReady, expected a finding")
	}
	if f.Container != "" {
		t.Errorf("Container = %q, want empty", f.Container)
	}
	if want := "readiness probe failed — HTTP 503"; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q (no container prefix)", f.Evidence, want)
	}
}

func TestContainerFromFieldPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"spec.containers{web}", "web"},
		{"spec.initContainers{init}", "init"},
		{"spec.containers{}", ""},
		{"", ""},
		{"spec.containers", ""},
	}
	for _, c := range cases {
		if got := containerFromFieldPath(c.in); got != c.want {
			t.Errorf("containerFromFieldPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClassifyProbe(t *testing.T) {
	cases := []struct{ msg, wantType, wantReason string }{
		{"Readiness probe failed: HTTP probe failed with statuscode: 503", "readiness", "HTTP 503"},
		{"Liveness probe failed: dial tcp 10.0.0.1:8080: connect: connection refused", "liveness", "connection refused"},
		{"Liveness probe failed: read tcp 10.0.0.1:8080: connection reset by peer", "liveness", "connection reset"},
		{"Readiness probe failed: dial tcp 10.0.0.1:8080: connect: no route to host", "readiness", "unreachable"},
		{`Startup probe failed: Get "http://10.0.0.1/": context deadline exceeded`, "startup", "timed out"},
		{"Readiness probe failed: dial tcp: lookup db on 10.96.0.10:53: no such host", "readiness", "DNS lookup failed"},
		{`Liveness probe failed: service unhealthy (responded with "NOT_SERVING")`, "liveness", "gRPC NOT_SERVING"},
		{"Liveness probe failed: cat: /tmp/healthy: No such file or directory", "liveness", ""},
		{"BackOff restarting failed container", "", ""},
	}
	for _, c := range cases {
		gotType, gotReason := classifyProbe(c.msg)
		if gotType != c.wantType || gotReason != c.wantReason {
			t.Errorf("classifyProbe(%q) = (%q,%q), want (%q,%q)", c.msg, gotType, gotReason, c.wantType, c.wantReason)
		}
	}
}

// R10 (B): the newest event won outright, so a readiness failure a second
// newer than a liveness failure decided the whole finding. Rank by consequence
// instead — liveness > startup > readiness — inside a window anchored on the
// newest event.
func TestProbeFailure_LivenessOutranksANewerReadiness(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
		pfEventAt("shop", "web-1", "web", pfLivenessRefus, 30*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if !strings.Contains(f.Reason, "liveness probe keeps failing") {
		t.Errorf("Reason = %q, want the liveness consequence", f.Reason)
	}
	// R10 (C): every probe type in the window is named, not only the selected
	// one — the reason sentence is singular, the evidence line is not.
	want := `container "web": liveness and readiness probes failed — connection refused`
	if f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// The window is what keeps the ranking honest: events live about an hour, so an
// unbounded ranking would let a long-resolved liveness failure beat a live
// readiness one.
func TestProbeFailure_StaleLivenessOutsideTheWindowLoses(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
		pfEventAt("shop", "web-1", "web", pfLivenessRefus, 5*time.Minute),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if !strings.Contains(f.Reason, "readiness probe keeps failing") {
		t.Errorf("Reason = %q, want the readiness consequence", f.Reason)
	}
	if want := `container "web": readiness probe failed — HTTP 503`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q — a stale type must not be listed either", f.Evidence, want)
	}
}

// The window is anchored on the newest event rather than on now, which is why
// this detector still reads no clock. A whole pod's events shifted an hour into
// the past rank exactly as they do fresh.
func TestProbeFailure_WindowIsAnchoredOnTheNewestEventNotOnNow(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, time.Hour),
		pfEventAt("shop", "web-1", "web", pfLivenessRefus, time.Hour+30*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if want := `container "web": liveness and readiness probes failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// An event exactly on the boundary is inside it: the window is T0 - 2m or
// later.
func TestProbeFailure_WindowBoundaryIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"on the boundary", probeRankWindow, `container "web": liveness and readiness probes failed — connection refused`},
		{"one second past it", probeRankWindow + time.Second, `container "web": readiness probe failed — HTTP 503`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
				pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
				pfEventAt("shop", "web-1", "web", pfLivenessRefus, tc.ago),
			}}
			f := ProbeFailureDetector{}.Detect(facts)
			if f == nil {
				t.Fatal("expected a finding, got nil")
			}
			if f.Evidence != tc.want {
				t.Errorf("Evidence = %q, want %q", f.Evidence, tc.want)
			}
		})
	}
}

// The full ordering, and the three-type evidence line.
func TestProbeFailure_RanksLivenessOverStartupOverReadiness(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
		pfEventAt("shop", "web-1", "web", pfStartupTimout, 20*time.Second),
		pfEventAt("shop", "web-1", "web", pfLivenessRefus, 40*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	want := `container "web": liveness, startup and readiness probes failed — connection refused`
	if f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

func TestProbeFailure_StartupOutranksReadiness(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
		pfEventAt("shop", "web-1", "web", pfStartupTimout, 20*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if !strings.Contains(f.Reason, "startup probe keeps failing") {
		t.Errorf("Reason = %q, want the startup consequence", f.Reason)
	}
	if want := `container "web": startup and readiness probes failed — timed out`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// Within one probe type the newest event still wins, so the reason suffix comes
// from the most recent failure of the type that was selected.
func TestProbeFailure_NewestWinsWithinTheSelectedType(t *testing.T) {
	facts := PodFacts{Pod: pfPod("shop", "web-1", "web"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfLivenessRefus, 0),
		pfEventAt("shop", "web-1", "web", "Liveness probe failed: dial tcp 10.0.0.1:8080: connect: no route to host", 30*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if want := `container "web": liveness probe failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// The types listed belong to the container the finding names. A liveness
// failure on a sidecar outranks a readiness failure on the app container and
// takes the finding with it — but it must not collect the app container's
// probe type into its own evidence line.
func TestProbeFailure_ListsOnlyTheSelectedContainersTypes(t *testing.T) {
	facts := PodFacts{Pod: pfMultiPod("shop", "web-1", "web", "sidecar"), Events: []corev1.Event{
		pfEventAt("shop", "web-1", "web", pfReadinessHTTP, 0),
		pfEventAt("shop", "web-1", "sidecar", pfLivenessRefus, 30*time.Second),
	}}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Container != "sidecar" {
		t.Errorf("Container = %q, want sidecar", f.Container)
	}
	if want := `container "sidecar": liveness probe failed — connection refused`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// pfPodWithProbe is a Running-but-not-Ready pod whose container declares one
// probe of the given type with the given handler.
func pfPodWithProbe(container, probeType string, h corev1.ProbeHandler) *corev1.Pod {
	pod := pfPod("shop", "web-1", container)
	c := corev1.Container{Name: container}
	p := &corev1.Probe{ProbeHandler: h}
	switch probeType {
	case "readiness":
		c.ReadinessProbe = p
	case "liveness":
		c.LivenessProbe = p
	case "startup":
		c.StartupProbe = p
	}
	pod.Spec.Containers = []corev1.Container{c}
	return pod
}

// The exec case: the reason vocabulary is HTTP/TCP shaped and an exec probe's
// output is exactly the text that may not be forwarded, so the evidence line
// used to end after "probe failed" with no explanation of the silence. The
// handler is a typed field on an object the API server validates, so naming it
// adds no leak surface — and the probe's output is still never read.
func TestProbeFailure_NamesTheHandlerWhenTheReasonIsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler corev1.ProbeHandler
		want    string
	}{
		{"exec", corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"cat", "/tmp/healthy"}}},
			`container "web": readiness probe failed — exec probe, output withheld`},
		{"httpGet", corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz"}},
			`container "web": readiness probe failed — httpGet probe, output withheld`},
		{"tcpSocket", corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}},
			`container "web": readiness probe failed — tcpSocket probe, output withheld`},
		{"gRPC", corev1.ProbeHandler{GRPC: &corev1.GRPCAction{Port: 9000}},
			`container "web": readiness probe failed — gRPC probe, output withheld`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := PodFacts{
				Pod:    pfPodWithProbe("web", "readiness", tc.handler),
				Events: []corev1.Event{pfEventAt("shop", "web-1", "web", "Readiness probe failed: cat: /tmp/healthy: No such file or directory", 0)},
			}
			f := ProbeFailureDetector{}.Detect(facts)
			if f == nil {
				t.Fatal("expected a finding, got nil")
			}
			if f.Evidence != tc.want {
				t.Errorf("Evidence = %q, want %q", f.Evidence, tc.want)
			}
		})
	}
}

// The exec probe's own output is what stays withheld. A message the reason
// vocabulary does recognise is still reported as that reason.
func TestProbeFailure_HandlerNotNamedWhenTheReasonIsKnown(t *testing.T) {
	facts := PodFacts{
		Pod:    pfPodWithProbe("web", "readiness", corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/health"}}}),
		Events: []corev1.Event{pfEventAt("shop", "web-1", "web", "Readiness probe failed: command timeout", 0)},
	}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if want := `container "web": readiness probe failed — timed out`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// No probe of that type in the spec — nothing to name, and the line ends where
// it always did rather than guessing.
func TestProbeFailure_NoHandlerLeavesTheLineBare(t *testing.T) {
	facts := PodFacts{
		Pod:    pfPodWithProbe("web", "liveness", corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/health"}}}),
		Events: []corev1.Event{pfEventAt("shop", "web-1", "web", "Readiness probe failed: cat: /tmp/healthy: No such file or directory", 0)},
	}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if want := `container "web": readiness probe failed`; f.Evidence != want {
		t.Errorf("Evidence = %q, want %q", f.Evidence, want)
	}
}

// The exec command itself is never read, whatever it contains.
func TestProbeFailure_HandlerNamingLeaksNoCommand(t *testing.T) {
	facts := PodFacts{
		Pod:    pfPodWithProbe("web", "readiness", corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/bin/check", "--token", "not-a-real-routing-key"}}}),
		Events: []corev1.Event{pfEventAt("shop", "web-1", "web", "Readiness probe failed: cat: /tmp/healthy: No such file or directory", 0)},
	}
	f := ProbeFailureDetector{}.Detect(facts)
	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if strings.Contains(f.Evidence, "not-a-real-routing-key") || strings.Contains(f.Reason, "not-a-real-routing-key") {
		t.Errorf("the exec command reached the finding: Evidence=%q Reason=%q", f.Evidence, f.Reason)
	}
}
