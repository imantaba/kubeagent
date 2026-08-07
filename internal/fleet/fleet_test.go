package fleet

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("MARKERVALUEA"))},
		{Name: "example-b", Client: fake.NewSimpleClientset(crashingPod("MARKERVALUEB"))},
	}, Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second})

	if len(rep.Shared) == 0 {
		t.Fatalf("rep.Shared is empty — the walk over it below checks nothing unless the fixture actually correlates two clusters")
	}

	var sb strings.Builder
	sb.WriteString(rep.SchemaVersion + " " + rep.Verdict)
	for _, c := range rep.Clusters {
		sb.WriteString(" " + c.Context + " " + c.Verdict + " " + strings.Join(c.TopIssues, " "))
	}
	for _, u := range rep.Unreachable {
		sb.WriteString(" " + u.Context + " " + u.Reason)
	}
	for _, s := range rep.Shared {
		sb.WriteString(" " + s.Signal + " " + s.Source + " " + strings.Join(s.Clusters, " "))
	}
	if strings.Contains(sb.String(), "MARKERVALUEA") {
		t.Errorf("report carries an object name: %q", sb.String())
	}
	if strings.Contains(sb.String(), "MARKERVALUEB") {
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

// A per-cluster timeout is the shape a wedged control plane takes: the read
// that was in flight comes back with an error wrapping context.DeadlineExceeded.
// The fake clientset ignores ctx entirely — its List methods never inspect it —
// so a cancelled or expired context alone cannot produce that error here; a
// real API server produces it by failing the in-flight request instead. The
// reactor stands in for that failure at the point collect.Pods makes its List
// call, which collect.Pods then wraps exactly as it would a real one, so the
// error reaches reasonFor by the same route a real timeout would: the fixed
// vocabulary must come from unwrapping that error, never from err.Error(),
// which can carry a server URL or a filesystem path.
func TestSweepReportsATimeoutFromTheFixedVocabulary(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	rep := Sweep(context.Background(), []Target{{Name: "example-a", Client: client}},
		Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: 30 * time.Second})

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

// A correlation adds no severity. Every finding it counts was already counted
// in the cluster that produced it, and that cluster already got its verdict
// from gate.Decide — so folding the same evidence again would double-count it,
// and would let a sweep disagree with a single-cluster `kubeagent gate` about
// the same cluster, which this package's doc comment says can never happen.
func TestSweepCorrelationChangesNoVerdict(t *testing.T) {
	targets := []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
		{Name: "example-b", Client: fake.NewSimpleClientset(crashingPod("beta"))},
	}
	opts := Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second}

	rep := Sweep(context.Background(), targets, opts)

	if len(rep.Shared) == 0 {
		t.Fatal("no correlation; the fixture must share a signal or this test proves nothing")
	}
	if rep.Verdict != "fail" || rep.Code != 1 {
		t.Errorf("verdict = %q/%d, want fail/1 — the same answer slice 1 gave", rep.Verdict, rep.Code)
	}
	for _, c := range rep.Clusters {
		if c.Verdict != "fail" {
			t.Errorf("cluster %s = %q, want fail — a correlation changes no cluster verdict",
				c.Context, c.Verdict)
		}
	}
}

// One cluster cannot correlate with itself, however much it reports.
func TestSweepOfOneClusterCarriesNoCorrelation(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "example-a", Client: fake.NewSimpleClientset(crashingPod("alpha"))},
	}, Options{FailOn: findings.Critical, Workers: 1, ClusterTimeout: 30 * time.Second})

	if rep.Shared != nil {
		t.Errorf("Shared = %+v, want nil so omitempty drops the key", rep.Shared)
	}
}

// Target must never gain a kubeconfig path field. The caller builds the client
// precisely so that a credential never enters this package, and a field set is
// the only thing a test can pin that a comment cannot.
func TestTargetCarriesNoKubeconfigPath(t *testing.T) {
	var got []string
	typ := reflect.TypeOf(Target{})
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"Name", "Context", "Client"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Target fields = %v, want exactly %v — a kubeconfig path is a credential this package must never hold", got, want)
	}
}

// A name is written only when it says something the context does not. A
// kubeconfig sweep hands in Name with Context unset, and its rows must carry no
// name at all so the JSON stays byte-identical to v1.10.0's.
func TestSweepNamesAClusterOnlyWhenItDiffersFromItsContext(t *testing.T) {
	rep := Sweep(context.Background(), []Target{
		{Name: "edge-a", Context: "default", Client: healthyClient()},
		{Name: "prod-eu", Client: healthyClient()},
		{Name: "prod-us", Context: "prod-us", Client: healthyClient()},
	}, Options{FailOn: findings.Critical, Workers: 3, ClusterTimeout: 30 * time.Second})

	want := map[string]string{ // context -> expected Name
		"default": "edge-a",
		"prod-eu": "",
		"prod-us": "",
	}
	if len(rep.Clusters) != 3 {
		t.Fatalf("Clusters = %d, want 3", len(rep.Clusters))
	}
	for _, c := range rep.Clusters {
		expected, known := want[c.Context]
		if !known {
			t.Fatalf("unexpected context %q", c.Context)
		}
		if c.Name != expected {
			t.Errorf("cluster %q Name = %q, want %q", c.Context, c.Name, expected)
		}
	}
}

// The Unreachable sort had the same non-total comparator the summary sort had,
// and the same fix: order on the row identity, not the context.
func TestSweepSortsUnreachableByIdentityNotContext(t *testing.T) {
	targets := []Target{
		{Name: "edge-d", Context: "default"},
		{Name: "edge-b", Context: "default"},
		{Name: "edge-c", Context: "default"},
		{Name: "edge-a", Context: "default"},
	}
	rep := Sweep(context.Background(), targets,
		Options{FailOn: findings.Critical, Workers: 4, ClusterTimeout: 30 * time.Second})

	var got []string
	for _, u := range rep.Unreachable {
		got = append(got, u.Name)
	}
	want := []string{"edge-a", "edge-b", "edge-c", "edge-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unreachable order = %v, want %v", got, want)
	}
	for _, u := range rep.Unreachable {
		if u.Context != "default" || u.Reason != ReasonUnreachable {
			t.Errorf("unreachable row = %+v, want context default and the fixed reason", u)
		}
	}
}

// What a cluster is CALLED must not change what it is JUDGED to be. The same
// fake clients wrapped the way a kubeconfig sweep wraps them and the way a
// fleet file wraps them must produce the same verdict, the same exit code and
// the same per-row counts. This is the test that pins "a sweep and a
// single-cluster gate can never disagree about the same cluster" across a new
// selection source.
func TestSelectionSourceChangesNoVerdict(t *testing.T) {
	opts := Options{FailOn: findings.Critical, Workers: 2, ClusterTimeout: 30 * time.Second}

	crashing := fake.NewSimpleClientset(crashingPod("alpha"))
	healthy := healthyClient()

	fromKubeconfig := Sweep(context.Background(), []Target{
		{Name: "edge-a", Client: crashing},
		{Name: "edge-b", Client: healthy},
	}, opts)

	fromFile := Sweep(context.Background(), []Target{
		{Name: "edge-a", Context: "default", Client: crashing},
		{Name: "edge-b", Context: "default", Client: healthy},
	}, opts)

	if fromKubeconfig.Verdict != fromFile.Verdict || fromKubeconfig.Code != fromFile.Code {
		t.Fatalf("verdict = %q/%d from a kubeconfig and %q/%d from a file, want identical",
			fromKubeconfig.Verdict, fromKubeconfig.Code, fromFile.Verdict, fromFile.Code)
	}
	if len(fromKubeconfig.Clusters) != len(fromFile.Clusters) {
		t.Fatalf("row counts differ: %d and %d", len(fromKubeconfig.Clusters), len(fromFile.Clusters))
	}
	for i := range fromKubeconfig.Clusters {
		a, b := fromKubeconfig.Clusters[i], fromFile.Clusters[i]
		if a.Verdict != b.Verdict || a.Critical != b.Critical || a.Warning != b.Warning ||
			a.Info != b.Info || a.Blindspots != b.Blindspots {
			t.Errorf("row %d differs beyond its name:\n %+v\n %+v", i, a, b)
		}
	}
}
