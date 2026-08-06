package fleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/findings"
	"github.com/imantaba/kubeagent/internal/jsonschema"
)

// crashingPod is a pod in CrashLoopBackOff — the signal every detector suite
// agrees on, so it makes a cluster fail without depending on any one detector's
// thresholds. Names are markers, so TestSweepCarriesNoObjectName can look for
// them in the rendered report.
func crashingPod(marker string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: marker + "-pod", Namespace: marker + "-ns"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         marker + "-container",
				RestartCount: 9,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: "back-off restarting failed container",
				}},
			}},
		},
	}
}

func healthyClient() kubernetes.Interface { return fake.NewSimpleClientset() }

func TestSweepSummarizesEveryTarget(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: healthyClient()},
	}, Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second})

	if rep.SchemaVersion != jsonschema.FleetVersion {
		t.Errorf("SchemaVersion = %q, want %q", rep.SchemaVersion, jsonschema.FleetVersion)
	}
	if len(rep.Clusters) != 2 {
		t.Fatalf("Clusters = %d, want 2", len(rep.Clusters))
	}
	if len(rep.Unreachable) != 0 {
		t.Errorf("Unreachable = %v, want none — both fake clients answer", rep.Unreachable)
	}
	if rep.Clusters[0].Context != "example-a" || rep.Clusters[0].Verdict != "fail" {
		t.Errorf("worst cluster = %+v, want example-a failing first", rep.Clusters[0])
	}
	if rep.Verdict != "fail" || rep.Code != 1 {
		t.Errorf("verdict = %q/%d, want fail/1", rep.Verdict, rep.Code)
	}
	if rep.FailOn != findings.Critical {
		t.Errorf("FailOn = %v, want critical echoed back", rep.FailOn)
	}
}

// The pool must not make the report a function of which cluster answered first.
// parallel.Do is index-ordered and sortSummaries is total, so the same input
// must produce byte-identical output every time.
func TestSweepIsDeterministic(t *testing.T) {
	targets := []Target{
		{Name: "example-c", Client: fake.NewSimpleClientset(crashingPod("gamma"))},
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: healthyClient()},
		{Name: "example-d", Client: healthyClient()},
	}
	opts := Options{FailOn: findings.Critical, Workers: 4, ClusterTimeout: 30 * time.Second}

	first := Sweep(context.Background(), targets, opts)
	for i := 0; i < 20; i++ {
		got := Sweep(context.Background(), targets, opts)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs from run 0:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// The whole report — every string it carries — must be free of node, namespace,
// pod, workload and container names.
func TestSweepCarriesNoObjectName(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("MARKERVALUE"))},
	}, Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: 30 * time.Second})

	var sb strings.Builder
	sb.WriteString(rep.SchemaVersion + " " + rep.Verdict)
	for _, c := range rep.Clusters {
		sb.WriteString(" " + c.Context + " " + c.Verdict + " " + strings.Join(c.TopIssues, " "))
	}
	for _, u := range rep.Unreachable {
		sb.WriteString(" " + u.Context + " " + u.Reason)
	}
	if strings.Contains(sb.String(), "MARKERVALUE") {
		t.Errorf("report carries an object name: %q", sb.String())
	}
}

// A nil client is the shape internal/cli hands in when it could not build one.
// The cluster must be named as a blind spot, never silently dropped — one
// missing row out of three hundred is invisible.
func TestSweepNamesAClusterItCouldNotRead(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: healthyClient()},
		{Name: "example-unreachable", Client: nil},
	}, Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second})

	if len(rep.Clusters) != 1 {
		t.Errorf("Clusters = %d, want only the reachable one", len(rep.Clusters))
	}
	want := []Unreachable{{Context: "example-unreachable", Reason: ReasonUnreachable}}
	if !reflect.DeepEqual(rep.Unreachable, want) {
		t.Errorf("Unreachable = %+v, want %+v", rep.Unreachable, want)
	}
	if rep.Verdict != "inconclusive" || rep.Code != 2 {
		t.Errorf("verdict = %q/%d, want inconclusive/2 — an unread cluster is not a pass",
			rep.Verdict, rep.Code)
	}
}

// A cancelled context is the shape a per-cluster timeout takes. The reason must
// come from the fixed vocabulary, not from err.Error(), which can carry a
// server URL or a filesystem path.
func TestSweepReportsATimeoutFromTheFixedVocabulary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := Sweep(ctx, []Target{{Name: "example-a", Client: healthyClient()}},
		Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: time.Nanosecond})

	if len(rep.Unreachable) != 1 {
		t.Fatalf("Unreachable = %+v, want one entry", rep.Unreachable)
	}
	if rep.Unreachable[0].Reason != ReasonTimedOut {
		t.Errorf("Reason = %q, want %q", rep.Unreachable[0].Reason, ReasonTimedOut)
	}
}

// Zero targets is a legal sweep — internal/cli refuses an empty selection
// before it gets here, so this only pins that Sweep does not panic and does not
// invent a verdict it did not measure.
func TestSweepOfNothingIsAPassWithEmptySlices(t *testing.T) {
	rep := Sweep(context.Background(), nil, Options{FailOn: findings.Critical})

	if rep.Verdict != "pass" || rep.Code != 0 {
		t.Errorf("verdict = %q/%d, want pass/0", rep.Verdict, rep.Code)
	}
	if rep.Clusters == nil || rep.Unreachable == nil {
		t.Errorf("Clusters = %v, Unreachable = %v — both must be empty slices so the "+
			"JSON document has [] rather than null", rep.Clusters, rep.Unreachable)
	}
}
