// Package parallel provides one primitive: a bounded worker pool whose results
// come back in index order. It imports nothing from kubeagent and holds no
// client, no context of its own and no state between calls.
package parallel

import (
	"context"
	"sync"
)

// Do runs fn(ctx, i) once for every i in [0, n) with at most workers calls in
// flight at a time, and returns the results in INDEX order: out[i] is always
// fn's return for index i, never the order the calls finished in.
//
// That ordering is the reason the package exists. kubeagent renders its scan
// output in a fixed order, and a pool that returned completion order would make
// the rendered bytes depend on how fast each API call happened to answer.
//
// Do returns only after every call has finished. It never cancels ctx and never
// stops early — not on an error, not on cancellation. A caller that received a
// zero value for index i could not tell "this call was skipped" from "this call
// succeeded and found nothing", so skipping would turn a cancelled run into a
// run that silently reports an empty result. ctx is passed through to fn, which
// decides what a cancelled context means for its own work.
//
// workers below 1 is treated as 1. n at or below 0 returns nil without calling
// fn at all.
//
// The result type is the caller's: Do carries whatever fn returns, and nothing
// about the pool is specific to any one type. kubeagent's scan instantiates it
// with error, but the index-ordered result slice — not the error — is the
// contract callers depend on.
func Do[T any](ctx context.Context, workers, n int, fn func(context.Context, int) T) []T {
	if n <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}

	// Each worker writes only out[i] for the index it drew, so the workers share
	// no memory and the pool needs no lock.
	out := make([]T, n)
	next := make(chan int)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := range next {
				out[i] = fn(ctx, i)
			}
		}()
	}

	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()

	return out
}
