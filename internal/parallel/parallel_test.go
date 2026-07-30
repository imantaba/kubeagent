package parallel_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/parallel"
)

func TestDoReturnsResultsInIndexOrder(t *testing.T) {
	got := parallel.Do(context.Background(), 4, 6, func(_ context.Context, i int) int {
		return i * i
	})
	want := []int{0, 1, 4, 9, 16, 25}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Do = %v, want %v", got, want)
	}
}

// The whole point of the package: a result slice ordered by index even when the
// calls finish in exactly the opposite order.
func TestDoReturnsIndexOrderWhenCompletionOrderIsReversed(t *testing.T) {
	const n = 8

	var mu sync.Mutex
	var completed []int

	got := parallel.Do(context.Background(), n, n, func(_ context.Context, i int) int {
		// Index 0 sleeps longest, index n-1 finishes first.
		time.Sleep(time.Duration(n-i) * 20 * time.Millisecond)
		mu.Lock()
		completed = append(completed, i)
		mu.Unlock()
		return i * 10
	})

	wantCompleted := []int{7, 6, 5, 4, 3, 2, 1, 0}
	if !reflect.DeepEqual(completed, wantCompleted) {
		t.Fatalf("calls completed in %v, want %v — the schedule was not inverted, so this test proves nothing", completed, wantCompleted)
	}

	want := []int{0, 10, 20, 30, 40, 50, 60, 70}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Do = %v, want %v — results must follow index order, not completion order", got, want)
	}
}

func TestDoRunsAtMostWorkersAtOnce(t *testing.T) {
	const n, workers = 20, 3

	var mu sync.Mutex
	inFlight, peak := 0, 0

	parallel.Do(context.Background(), workers, n, func(_ context.Context, i int) struct{} {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		inFlight--
		mu.Unlock()
		return struct{}{}
	})

	if peak > workers {
		t.Errorf("peak concurrency was %d, want at most %d", peak, workers)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d — the calls never overlapped, so the cap was not exercised", peak)
	}
}

func TestDoWithNoWorkReturnsNothingAndNeverCallsFn(t *testing.T) {
	for _, n := range []int{0, -1} {
		called := false
		got := parallel.Do(context.Background(), 4, n, func(_ context.Context, i int) int {
			called = true
			return i
		})
		if len(got) != 0 {
			t.Errorf("Do(n=%d) returned %v, want an empty slice", n, got)
		}
		if called {
			t.Errorf("Do(n=%d) called fn, want no calls", n)
		}
	}
}

func TestDoTreatsWorkersBelowOneAsOne(t *testing.T) {
	got := parallel.Do(context.Background(), 0, 3, func(_ context.Context, i int) int { return i })
	if want := []int{0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("Do(workers=0) = %v, want %v", got, want)
	}
	got = parallel.Do(context.Background(), -5, 3, func(_ context.Context, i int) int { return i })
	if want := []int{0, 1, 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("Do(workers=-5) = %v, want %v", got, want)
	}
}

func TestDoWithMoreWorkersThanWorkAnswersEveryIndex(t *testing.T) {
	got := parallel.Do(context.Background(), 64, 3, func(_ context.Context, i int) int { return i + 1 })
	if want := []int{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("Do(workers=64, n=3) = %v, want %v", got, want)
	}
}

// A cancelled ctx must not make Do skip indices. A skipped index leaves a zero
// value in the result, and a zero value is indistinguishable from a successful
// read that found nothing — which would turn a cancelled scan into a scan that
// silently reports an empty cluster. Do hands ctx to fn and lets fn decide.
func TestDoAnswersEveryIndexEvenWhenContextIsAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := parallel.Do(ctx, 4, 5, func(ctx context.Context, i int) error {
		return ctx.Err()
	})

	if len(got) != 5 {
		t.Fatalf("Do returned %d results, want 5", len(got))
	}
	for i, err := range got {
		if err != context.Canceled {
			t.Errorf("result[%d] = %v, want context.Canceled — fn must be called for every index", i, err)
		}
	}
}
