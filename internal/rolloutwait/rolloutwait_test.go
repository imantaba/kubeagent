package rolloutwait

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fakeClock advances only when the code under test sleeps, so a 5-minute
// timeout costs no wall-clock time in a test.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time        { return c.now }
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)}
}

func int32p(i int32) *int32 { return &i }

func settledDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: int32p(3)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3,
		},
	}
}

func rollingDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: int32p(3)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 3, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
}

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantKind string
		wantName string
	}{
		{"deployment/api", KindDeployment, "api"},
		{"Deployment/api", KindDeployment, "api"},
		{"statefulset/db", KindStatefulSet, "db"},
		{"daemonset/node-agent", KindDaemonSet, "node-agent"},
	} {
		got, err := ParseTarget(tc.in, "prod")
		if err != nil {
			t.Fatalf("ParseTarget(%q): unexpected error %v", tc.in, err)
		}
		if got.Kind != tc.wantKind || got.Name != tc.wantName || got.Namespace != "prod" {
			t.Errorf("ParseTarget(%q) = %+v, want %s/%s in prod", tc.in, got, tc.wantKind, tc.wantName)
		}
	}
}

func TestParseTargetRejectsBadInput(t *testing.T) {
	for _, in := range []string{"api", "pod/api", "deployment/", "/api", "", "deployment/api/extra"} {
		if _, err := ParseTarget(in, "prod"); err == nil {
			t.Errorf("ParseTarget(%q): want an error, got nil", in)
		}
	}
}

func TestParseTargetRequiresANamespace(t *testing.T) {
	if _, err := ParseTarget("deployment/api", ""); err == nil {
		t.Fatal("ParseTarget with no namespace: want an error, got nil")
	}
}

func TestWaitReturnsImmediatelyWhenAlreadySettled(t *testing.T) {
	client := fake.NewSimpleClientset(settledDeployment())
	clk := newClock()

	got, err := Wait(context.Background(), client,
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Minute, time.Second, clk)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.Settled {
		t.Errorf("Settled = false, want true: %+v", got)
	}
	if got.Detail != "3/3 updated, 3 available" {
		t.Errorf("Detail = %q, want \"3/3 updated, 3 available\"", got.Detail)
	}
	if !clk.now.Equal(newClock().now) {
		t.Error("Wait slept even though the rollout was already settled")
	}
}

func TestWaitPollsUntilSettled(t *testing.T) {
	client := fake.NewSimpleClientset(rollingDeployment())
	calls := 0
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls < 3 {
			return true, rollingDeployment(), nil
		}
		return true, settledDeployment(), nil
	})

	got, err := Wait(context.Background(), client,
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Minute, time.Second, newClock())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.Settled {
		t.Errorf("Settled = false after the rollout completed: %+v", got)
	}
	if calls != 3 {
		t.Errorf("issued %d Gets, want 3", calls)
	}
}

func TestWaitTimesOutWithoutAnError(t *testing.T) {
	client := fake.NewSimpleClientset(rollingDeployment())
	clk := newClock()

	got, err := Wait(context.Background(), client,
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		10*time.Second, time.Second, clk)
	if err != nil {
		t.Fatalf("a timeout is a verdict, not a failure to run; want nil error, got %v", err)
	}
	if got.Settled {
		t.Error("Settled = true, want false")
	}
	if got.Detail != "1/3 updated, 1 available" {
		t.Errorf("Detail = %q, want the last observed state", got.Detail)
	}
}

func TestWaitReturnsTheAPIError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	if _, err := Wait(context.Background(), client,
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Minute, time.Second, newClock()); err == nil {
		t.Fatal("want the API error surfaced, got nil")
	}
}

func TestWaitRejectsANonPositiveInterval(t *testing.T) {
	client := fake.NewSimpleClientset(settledDeployment())

	for _, interval := range []time.Duration{0, -time.Second} {
		_, err := Wait(context.Background(), client,
			Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
			5*time.Minute, interval, newClock())
		if err == nil {
			t.Errorf("interval %s: want an error, got nil", interval)
		}
	}
	// It must refuse before touching the cluster, not after a first read.
	if got := len(client.Actions()); got != 0 {
		t.Errorf("Wait issued %d call(s) on a rejected interval; want 0", got)
	}
}

func TestWaitIssuesOnlyGets(t *testing.T) {
	client := fake.NewSimpleClientset(settledDeployment())

	if _, err := Wait(context.Background(), client,
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Minute, time.Second, newClock()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("Wait issued a %q on %q; rolloutwait is read-only and may only Get",
				a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestWaitStatefulSetSettles(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32p(2)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2,
			CurrentRevision: "db-1", UpdateRevision: "db-1",
		},
	}
	got, err := Wait(context.Background(), fake.NewSimpleClientset(sts),
		Target{Kind: KindStatefulSet, Namespace: "prod", Name: "db"},
		5*time.Minute, time.Second, newClock())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.Settled {
		t.Errorf("Settled = false: %+v", got)
	}
}

func TestWaitStatefulSetMidRevisionIsNotSettled(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod", Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32p(2)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 2,
			CurrentRevision: "db-1", UpdateRevision: "db-2",
		},
	}
	got, err := Wait(context.Background(), fake.NewSimpleClientset(sts),
		Target{Kind: KindStatefulSet, Namespace: "prod", Name: "db"},
		5*time.Second, time.Second, newClock())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.Settled {
		t.Error("a StatefulSet part-way through a revision change is not settled")
	}
}

func TestWaitDaemonSetSettles(t *testing.T) {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "node-agent", Namespace: "prod", Generation: 1},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration: 1, DesiredNumberScheduled: 3,
			UpdatedNumberScheduled: 3, NumberAvailable: 3,
		},
	}
	got, err := Wait(context.Background(), fake.NewSimpleClientset(ds),
		Target{Kind: KindDaemonSet, Namespace: "prod", Name: "node-agent"},
		5*time.Minute, time.Second, newClock())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.Settled {
		t.Errorf("Settled = false: %+v", got)
	}
}

func TestWaitStaleObservedGenerationIsNotSettled(t *testing.T) {
	d := settledDeployment()
	d.Status.ObservedGeneration = 1 // controller has not seen generation 2 yet

	got, err := Wait(context.Background(), fake.NewSimpleClientset(d),
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Second, time.Second, newClock())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got.Settled {
		t.Error("a Deployment whose controller has not observed the new generation is not settled")
	}
}

func TestWaitHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Wait(ctx, fake.NewSimpleClientset(rollingDeployment()),
		Target{Kind: KindDeployment, Namespace: "prod", Name: "api"},
		5*time.Minute, time.Second, newClock()); err == nil {
		t.Fatal("want the context error surfaced, got nil")
	}
}
