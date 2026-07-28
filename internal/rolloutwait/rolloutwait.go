// Package rolloutwait polls one workload until its rollout settles, so
// `kubeagent gate --wait-for` can judge a deploy after it has finished rather
// than mid-flight.
//
// Read-only: it issues Get and nothing else. Sequential: one Get per interval
// in a plain loop with an injected clock, no goroutines — the scan-side CLI
// stays single-threaded in v1. It makes no LLM calls.
//
// The settled criteria mirror `kubectl rollout status`, so an operator's
// intuition about when a deploy is "done" and kubeagent's agree.
package rolloutwait

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The workload kinds with a rollout that can be observed to completion.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// Clock is the time source, injected so the timeout path costs no wall-clock
// time in a test.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// Real is the production Clock.
type Real struct{}

func (Real) Now() time.Time        { return time.Now() }
func (Real) Sleep(d time.Duration) { time.Sleep(d) }

// Target is the workload to watch.
type Target struct {
	Kind      string
	Namespace string
	Name      string
}

// Result reports how the wait ended. Detail is the last observed rollout state,
// which the gate prints whether it settled or not.
type Result struct {
	Settled bool
	Detail  string
}

// ParseTarget turns a --wait-for value ("deployment/api") into a Target.
func ParseTarget(s, namespace string) (Target, error) {
	if namespace == "" {
		return Target{}, fmt.Errorf("--wait-for needs a namespace (pass -n)")
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Target{}, fmt.Errorf("--wait-for %q is not kind/name (e.g. deployment/api)", s)
	}
	var kind string
	switch strings.ToLower(parts[0]) {
	case "deployment", "deployments", "deploy":
		kind = KindDeployment
	case "statefulset", "statefulsets", "sts":
		kind = KindStatefulSet
	case "daemonset", "daemonsets", "ds":
		kind = KindDaemonSet
	default:
		return Target{}, fmt.Errorf("--wait-for kind %q is not supported (want deployment, statefulset or daemonset)", parts[0])
	}
	return Target{Kind: kind, Namespace: namespace, Name: parts[1]}, nil
}

// Wait polls t until its rollout settles or timeout elapses.
//
// A timeout returns Settled=false and a nil error: not settling in time is a
// verdict the gate reports as exit 3, not a failure to run. A non-nil error
// means the API call itself failed, which is a different thing entirely.
func Wait(ctx context.Context, client kubernetes.Interface, t Target, timeout, interval time.Duration, clk Clock) (Result, error) {
	// A non-positive interval is the one input that breaks the loop's shape: it
	// never paces, so against Real it becomes an unpaced Get storm at the API
	// server and against an injected clock it never advances time at all — a
	// hang, not a timeout. Refuse it here rather than trust every caller.
	if interval <= 0 {
		return Result{}, fmt.Errorf("poll interval must be positive, got %s", interval)
	}
	deadline := clk.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		res, err := observe(ctx, client, t)
		if err != nil {
			return Result{}, err
		}
		if res.Settled {
			return res, nil
		}
		if !clk.Now().Before(deadline) {
			return res, nil
		}
		clk.Sleep(interval)
	}
}

// observe reads the workload once and reports whether its rollout has settled.
func observe(ctx context.Context, client kubernetes.Interface, t Target) (Result, error) {
	switch t.Kind {
	case KindDeployment:
		d, err := client.AppsV1().Deployments(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return Result{}, err
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		s := d.Status
		settled := s.ObservedGeneration >= d.Generation &&
			s.UpdatedReplicas == want &&
			s.Replicas == s.UpdatedReplicas &&
			s.AvailableReplicas == s.UpdatedReplicas
		return Result{
			Settled: settled,
			Detail:  fmt.Sprintf("%d/%d updated, %d available", s.UpdatedReplicas, want, s.AvailableReplicas),
		}, nil

	case KindStatefulSet:
		sts, err := client.AppsV1().StatefulSets(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return Result{}, err
		}
		want := int32(1)
		if sts.Spec.Replicas != nil {
			want = *sts.Spec.Replicas
		}
		s := sts.Status
		settled := s.ObservedGeneration >= sts.Generation &&
			s.UpdatedReplicas == want &&
			s.ReadyReplicas == want &&
			s.CurrentRevision == s.UpdateRevision
		return Result{
			Settled: settled,
			Detail:  fmt.Sprintf("%d/%d updated, %d ready", s.UpdatedReplicas, want, s.ReadyReplicas),
		}, nil

	case KindDaemonSet:
		ds, err := client.AppsV1().DaemonSets(t.Namespace).Get(ctx, t.Name, metav1.GetOptions{})
		if err != nil {
			return Result{}, err
		}
		s := ds.Status
		settled := s.ObservedGeneration >= ds.Generation &&
			s.UpdatedNumberScheduled == s.DesiredNumberScheduled &&
			s.NumberAvailable == s.DesiredNumberScheduled
		return Result{
			Settled: settled,
			Detail: fmt.Sprintf("%d/%d updated, %d available",
				s.UpdatedNumberScheduled, s.DesiredNumberScheduled, s.NumberAvailable),
		}, nil
	}
	return Result{}, fmt.Errorf("unsupported kind %q", t.Kind)
}
