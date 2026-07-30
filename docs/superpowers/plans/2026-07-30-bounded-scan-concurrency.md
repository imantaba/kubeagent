# Bounded Scan Concurrency Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Retire the "v1 `scan` is sequential" simplification — run the scan's independent cluster reads through a bounded worker pool and stop client-go from rate-limiting kubeagent against itself, without changing a single rendered byte.

**Architecture:** A new `internal/parallel` package provides one primitive, `Do`, which runs N indexed calls with at most `workers` in flight and returns the results in **index order, never completion order**. `internal/scan.Evaluate` is restructured into two read phases — phase 1 is every read that depends on nothing but `Options`, phase 2 is the fan-outs whose work list phase 1 determines — with all blind-spot bookkeeping moved out of the concurrent region into a single sequential block that walks a declared report order. Separately, `internal/cluster` stops accepting client-go's default 5 QPS / burst 10 token bucket, which otherwise throttles the very reads the pool is trying to overlap.

**Tech Stack:** Go 1.26 (generics, `context`, `sync.WaitGroup`, channels), `k8s.io/client-go` (`rest.Config`, fake clientset, `httptest`), standard-library `flag` only.

## Global Constraints

Every task's requirements implicitly include this section.

- **Signed-off commits.** `main` enforces DCO. Every commit needs a `Signed-off-by` trailer matching its author — use `git commit -s`. Author identity: `imantaba <itn.taba@gmail.com>`. Verify with `scripts/dco-check.sh origin/main HEAD`.
- **No AI attribution anywhere.** No `Co-Authored-By: Claude` trailer, no "Generated with Claude Code" line, no mention of Claude/Claude Code/Anthropic in any commit message, code comment, doc, changelog entry, or PR body. Every commit is authored solely by the human.
- **Read-only toward the cluster.** `get`/`list` only. No writes outside the opt-in `--fix` path, which this sub-project does not touch.
- **No LLM calls.** Nothing in this sub-project calls a model. `internal/parallel` must never import `internal/remediate` or `internal/explain`.
- **Detectors stay pure functions.** No detector gains I/O, a clock, or a goroutine.
- **Standard-library `flag` only.** No Cobra — that is sub-project 5.
- **No new third-party dependency.** `go.mod` and `go.sum` are untouched by every task in this plan. If a task seems to need a dependency, stop and escalate.
- **No secrets, credentials, private IPs, or internal hostnames** anywhere — source, tests, fixtures, docs, comments. Documentation IP addresses use RFC 5737 ranges; example domains use RFC 2606 (`example.com`, `.example`, `.invalid`). Use `<PLACEHOLDER>` where a real value would otherwise go.
- **URLs are credentials.** No log line, error, metric label, results file, rendered manifest, doc example, or test fixture may carry more than `scheme://host`. (`httptest` server URLs stay inside the test process and are never logged.)
- **Kubeconfig paths are credentials.** Do not add a kubeconfig path to any new error, log line, or artifact.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** No task in this plan regenerates it. If `go test ./internal/report` reports a golden diff, that is a defect in the task, not a golden to update.
- **`go test` runs with `-p 2`** — full parallelism trips a known Go linker panic on `internal/advisory`. Never pass `-short`. Full suite command: `go test -p 2 ./...`.
- **No `schemaVersion` bump.** This sub-project changes no JSON document shape, so `internal/jsonschema` and `website/docs/schemas/` are untouched. `go test -p 2 ./internal/schemadoc` must stay green without `-update`.
- **Blocked reasons stay kubeagent's own words** — never the API server's message and never `Status.Reason`. The existing `blindReason` helper is the only phrasing; do not add a second one.
- **`docs/go-concepts.md` is gitignored.** Edit it on disk, never `git add` it.
- **Never `git add -A` or `git add .`** — stage files by name.
- **Branch:** all work lands on `bounded-scan-concurrency` (cut from `main` at `be11967`). Never commit to `main`.
- **Go on PATH:** `export PATH=$PATH:/usr/local/go/bin`.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/parallel/parallel.go` (create) | 1 | The one primitive: `Do` — bounded fan-out, index-ordered results. Imports only `context` and `sync`. |
| `internal/parallel/parallel_test.go` (create) | 1 | Unit tests for `Do`, including a controlled schedule inversion. |
| `internal/cluster/client.go` (modify) | 2 | Add `applyRateLimits`; call it from both `restConfig` and `NewInClusterOrKubeconfig`. |
| `internal/cluster/client_test.go` (modify) | 2 | Assert the limiter default and both env overrides. |
| `internal/collect/collect.go` (modify) | 3 | Split `CollectInventory` into seven exported single-list functions. |
| `internal/collect/collect_test.go` (modify) | 3 | Cover the seven functions' error wrapping. |
| `internal/scan/workers.go` (create) | 4, 5, 6 | `scanWorkers()` (task 4) and `runReads` (task 5, rewritten task 6) — how the scan's reads are executed. |
| `internal/scan/workers_test.go` (create) | 4 | `scanWorkers` env parsing and clamping. |
| `internal/scan/scan.go` (modify) | 5 | Two-phase restructure of `Evaluate`; one clock; the `reportOrder` consumption block. |
| `internal/scan/scan_test.go` (modify) | 6 | Determinism tests: worker-count invariance, repeat invariance, a real schedule inversion, blind-spot order. |
| `.github/workflows/ci.yml` (modify) | 6 | Add `-race` to the test step. |
| `CLAUDE.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `website/docs/features/tuning.md`, `website/mkdocs.yml`, `docs/go-concepts.md` | 7 | Documentation. |

---

### Task 1: `internal/parallel` — the bounded, index-ordered pool

**Files:**
- Create: `internal/parallel/parallel.go`
- Test: `internal/parallel/parallel_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func parallel.Do[T any](ctx context.Context, workers, n int, fn func(context.Context, int) T) []T`. Task 6 is the only consumer.

**A note on the generic parameter, so the reviewer is told rather than discovering it:** the plan's only call site instantiates `T = error`, so `Do[T any]` looks speculative under YAGNI. It is generic deliberately, and the approved spec specifies it that way: the index-ordered result slice is what makes the primitive safe by construction, and the result type belongs to the caller, not to the pool. State this in the doc comment (the code below does). Do not narrow it to `[]error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/parallel/parallel_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/parallel/
```

Expected: FAIL — the package does not exist (`no required module provides package .../internal/parallel`).

- [ ] **Step 3: Write the implementation**

Create `internal/parallel/parallel.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test -race -p 2 ./internal/parallel/
```

Expected: PASS, with no race reported. `-race` matters here more than anywhere else in the plan.

- [ ] **Step 5: Verify the package imports nothing from kubeagent**

```bash
go list -deps ./internal/parallel | grep kubeagent
```

Expected: only `github.com/imantaba/kubeagent/internal/parallel` itself. In particular, no `internal/remediate` and no `internal/explain`.

- [ ] **Step 6: Commit**

```bash
git add internal/parallel/parallel.go internal/parallel/parallel_test.go
git commit -s -m "feat(parallel): add a bounded worker pool with index-ordered results

Do runs n indexed calls with at most workers in flight and returns the
results in index order, never completion order. Ordering is the contract:
the scan renders in a fixed order, so a pool that returned completion
order would make the rendered bytes depend on API-server latency.

Do never stops early on error or cancellation. A zero value for a skipped
index is indistinguishable from a successful read that found nothing, so
skipping would turn a cancelled scan into one that reports an empty
cluster."
```

---

### Task 2: Stop client-go rate-limiting kubeagent against itself

**Files:**
- Modify: `internal/cluster/client.go` (`restConfig` at :56-73, `NewInClusterOrKubeconfig` at :78-85)
- Test: `internal/cluster/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func applyRateLimits(config *rest.Config)` — unexported, package `cluster`. No exported surface changes. Two environment variables become live: `KUBEAGENT_QPS` (float, >0) and `KUBEAGENT_BURST` (int, >0).

**Why this task exists.** kubeagent never sets `QPS`/`Burst` on its `rest.Config`, so `rest.RESTClientFor` installs client-go's default `flowcontrol.NewTokenBucketRateLimiter(5.0, 10)` on **each per-API-group client**. CoreV1 carries nearly every read a scan makes, so CoreV1 alone blows past burst 10 and the rest of the scan is metered at 5 requests per second. Measured on a three-node cluster: `scan --kubelet-health --disk-usage --dns-health --control-plane-health --certs --security` took **2.42 s** with the limiter and **0.15 s** without — a 16× difference for byte-identical output, with zero goroutines involved. Bounded concurrency on top of a 5 QPS bucket buys almost nothing, which is why both changes ship together. The API server's own Priority and Fairness (`flowcontrol.apiserver.k8s.io/v1`, GA) sheds load based on what the server can actually take; a fixed client-side rate cannot.

**`NewInClusterOrKubeconfig` bypasses `restConfig` entirely** — it builds its config from `rest.InClusterConfig()` and never touches the kubeconfig path. That is why the fix is a shared helper called from both places, not an edit inside `restConfig`. Missing the in-cluster branch would leave the daemon running under the old limiter.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cluster/client_test.go`. The tests are in package `cluster` (same package as the code), so `restConfig` is directly callable. `twoContextKubeconfig(t)` already exists in this file and writes an RFC 2606-compliant fixture.

```go
// client-go defaults every per-API-group client to a 5 QPS / burst 10 token
// bucket. CoreV1 carries nearly every read a scan makes, so that default meters
// the scan at 5 requests per second — measured at 2.42s versus 0.15s for the
// same, byte-identical output. QPS -1 disables the limiter and leaves shedding
// to the API server's Priority and Fairness.
func TestRestConfigDisablesTheClientSideRateLimiter(t *testing.T) {
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QPS != -1 {
		t.Errorf("QPS = %v, want -1 (limiter disabled)", cfg.QPS)
	}
}

func TestRestConfigHonoursKubeagentQPS(t *testing.T) {
	t.Setenv("KUBEAGENT_QPS", "25")
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QPS != 25 {
		t.Errorf("QPS = %v, want 25", cfg.QPS)
	}
}

func TestRestConfigHonoursKubeagentBurst(t *testing.T) {
	t.Setenv("KUBEAGENT_QPS", "25")
	t.Setenv("KUBEAGENT_BURST", "50")
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Burst != 50 {
		t.Errorf("Burst = %v, want 50", cfg.Burst)
	}
}

// A bad knob must degrade to a working scan, not to an error and not to the
// throttled default.
func TestRestConfigIgnoresAnUnusableQPSValue(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-5", ""} {
		t.Setenv("KUBEAGENT_QPS", v)
		path := twoContextKubeconfig(t)
		cfg, err := restConfig(path, "alpha")
		if err != nil {
			t.Fatalf("KUBEAGENT_QPS=%q: %v", v, err)
		}
		if cfg.QPS != -1 {
			t.Errorf("KUBEAGENT_QPS=%q gave QPS = %v, want -1", v, cfg.QPS)
		}
	}
}

func TestRestConfigIgnoresAnUnusableBurstValue(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-5"} {
		t.Setenv("KUBEAGENT_QPS", "25")
		t.Setenv("KUBEAGENT_BURST", v)
		path := twoContextKubeconfig(t)
		cfg, err := restConfig(path, "alpha")
		if err != nil {
			t.Fatalf("KUBEAGENT_BURST=%q: %v", v, err)
		}
		if cfg.Burst != 0 {
			t.Errorf("KUBEAGENT_BURST=%q gave Burst = %v, want 0 (client-go's own default)", v, cfg.Burst)
		}
	}
}

// The in-cluster branch of NewInClusterOrKubeconfig builds its config from
// rest.InClusterConfig() and never reaches restConfig, so the limiter fix has to
// live in a helper both call. This test guards the helper directly: if someone
// inlines it back into restConfig, the daemon silently keeps the old limiter.
func TestApplyRateLimitsDisablesTheLimiterOnAnyConfig(t *testing.T) {
	cfg := &rest.Config{Host: "https://apiserver.example:6443"}
	applyRateLimits(cfg)
	if cfg.QPS != -1 {
		t.Errorf("QPS = %v, want -1", cfg.QPS)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/cluster/
```

Expected: FAIL — `undefined: applyRateLimits`, and once that compiles, `QPS = 0, want -1`.

- [ ] **Step 3: Write the implementation**

Add `strconv` to the imports in `internal/cluster/client.go` (`os` is already imported), then add the helper:

```go
// applyRateLimits sets the client-side request rate for every client kubeagent
// builds. The default is no client-side limit at all.
//
// client-go installs a 5 QPS / burst 10 token bucket on each per-API-group
// client when QPS is left at zero. CoreV1 carries nearly every read a scan
// makes, so that default meters the whole scan: measured on a three-node
// cluster, a scan with every add-on enabled took 2.42s with the limiter and
// 0.15s without, for byte-identical output. A client-side rate also holds the
// same number whether the API server is idle or dying, while the server's own
// Priority and Fairness (flowcontrol.apiserver.k8s.io/v1, GA) sheds load based
// on what it can actually take. QPS -1 disables the limiter entirely.
//
// KUBEAGENT_QPS restores a client-side limit for anyone who needs one — a
// shared cluster with a strict admission budget, a debugging session. A value
// that does not parse, or is not positive, is ignored: a bad knob degrades to a
// working scan, never to an error. KUBEAGENT_BURST only takes effect alongside
// KUBEAGENT_QPS, because with the limiter disabled there is no bucket to size;
// left unset, client-go applies its own default burst.
func applyRateLimits(config *rest.Config) {
	config.QPS = -1
	if s := os.Getenv("KUBEAGENT_QPS"); s != "" {
		if v, err := strconv.ParseFloat(s, 32); err == nil && v > 0 {
			config.QPS = float32(v)
		}
	}
	if s := os.Getenv("KUBEAGENT_BURST"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			config.Burst = v
		}
	}
}
```

In `restConfig`, replace the final `return config, nil` (client.go:72) with:

```go
	applyRateLimits(config)
	return config, nil
```

In `NewInClusterOrKubeconfig`, replace the in-cluster branch (client.go:79-80) with:

```go
	if cfg, err := rest.InClusterConfig(); err == nil {
		applyRateLimits(cfg) // this branch never reaches restConfig
		return kubernetes.NewForConfig(cfg)
	} else if err != rest.ErrNotInCluster {
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/cluster/
```

Expected: PASS.

- [ ] **Step 5: Confirm the four config sites are covered**

```bash
grep -n "rest.Config\|NewForConfig\|InClusterConfig" internal/cluster/client.go
```

Every path that builds a `*rest.Config` must reach `applyRateLimits`: `restConfig` (used by `NewClient` and `NewDynamicClients`) and the in-cluster branch of `NewInClusterOrKubeconfig`. There are no others in non-test code.

- [ ] **Step 6: Run the full suite**

```bash
go test -p 2 ./...
```

Expected: PASS, including `./internal/report` (golden file unchanged).

- [ ] **Step 7: Commit**

```bash
git add internal/cluster/client.go internal/cluster/client_test.go
git commit -s -m "perf(cluster): disable client-go's client-side rate limiter

client-go installs a 5 QPS / burst 10 token bucket on each per-API-group
client when QPS is left unset. CoreV1 carries nearly every read a scan
makes, so that default metered the whole scan: measured on a three-node
cluster, a scan with every add-on enabled took 2.42s with the limiter and
0.15s without, for byte-identical output.

The API server's Priority and Fairness sheds load based on what it can
actually take; a fixed client-side rate holds the same number whether the
server is idle or dying. KUBEAGENT_QPS restores a client-side limit for
anyone who needs one.

applyRateLimits is a shared helper because NewInClusterOrKubeconfig builds
its config from rest.InClusterConfig() and never reaches restConfig — the
in-cluster daemon would otherwise keep the old limiter."
```

---

### Task 3: Split `CollectInventory` into seven single-list functions

**Files:**
- Modify: `internal/collect/collect.go:29-78`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all in package `collect`, all read-only `List` calls:
  - `func Pods(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Pod, error)`
  - `func Deployments(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.Deployment, error)`
  - `func ReplicaSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.ReplicaSet, error)`
  - `func StatefulSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.StatefulSet, error)`
  - `func DaemonSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.DaemonSet, error)`
  - `func Jobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.Job, error)`
  - `func CronJobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.CronJob, error)`
  - `CollectInventory` keeps its exact signature and becomes a sequential composition of the seven.

**Name check:** none of these seven names is taken. `AllPods` and `SystemDaemonSets` exist and are different functions with different semantics (cluster-scoped, no namespace filter); leave them alone.

**Error wrapping is load-bearing.** Each function must produce byte-identical error text to today: `fmt.Errorf("listing pods: %w", err)`, `"listing deployments: %w"`, `"listing replicasets: %w"`, `"listing statefulsets: %w"`, `"listing daemonsets: %w"`, `"listing jobs: %w"`, `"listing cronjobs: %w"`. A caller that matched on this text still matches after the split.

- [ ] **Step 1: Write the failing tests**

Append to `internal/collect/collect_test.go`:

```go
// The seven list functions the scan's phase-1 pool calls. Each wraps its error
// with the same text CollectInventory used, so an operator reading a failure
// sees the same sentence they always did.
func TestSingleListFunctionsWrapTheirErrors(t *testing.T) {
	cases := []struct {
		name     string
		resource string
		want     string
		call     func(context.Context, kubernetes.Interface) error
	}{
		{"Pods", "pods", "listing pods: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.Pods(ctx, c, "")
			return err
		}},
		{"Deployments", "deployments", "listing deployments: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.Deployments(ctx, c, "")
			return err
		}},
		{"ReplicaSets", "replicasets", "listing replicasets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.ReplicaSets(ctx, c, "")
			return err
		}},
		{"StatefulSets", "statefulsets", "listing statefulsets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.StatefulSets(ctx, c, "")
			return err
		}},
		{"DaemonSets", "daemonsets", "listing daemonsets: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.DaemonSets(ctx, c, "")
			return err
		}},
		{"Jobs", "jobs", "listing jobs: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.Jobs(ctx, c, "")
			return err
		}},
		{"CronJobs", "cronjobs", "listing cronjobs: ", func(ctx context.Context, c kubernetes.Interface) error {
			_, err := collect.CronJobs(ctx, c, "")
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("list", tc.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			err := tc.call(context.Background(), client)
			if err == nil {
				t.Fatalf("%s returned no error, want one", tc.name)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Errorf("%s error = %q, want prefix %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

// The namespace argument still scopes the list.
func TestSingleListFunctionsHonourTheNamespaceFilter(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "a"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "b"}},
	)
	pods, err := collect.Pods(context.Background(), client, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "a" {
		t.Errorf("Pods(ns=shop) = %d pods %v, want just shop/a", len(pods), pods)
	}
}
```

If `collect_test.go` does not already import `errors`, `strings`, `k8stesting`, `runtime`, or `kubernetes`, add them.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/collect/
```

Expected: FAIL — `undefined: collect.Pods`, `undefined: collect.Deployments`, and so on.

- [ ] **Step 3: Write the implementation**

Replace `internal/collect/collect.go:29-78` with:

```go
// Pods lists pods in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func Pods(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	return list.Items, nil
}

// Deployments lists Deployments in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func Deployments(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.Deployment, error) {
	list, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	return list.Items, nil
}

// ReplicaSets lists ReplicaSets in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func ReplicaSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.ReplicaSet, error) {
	list, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	return list.Items, nil
}

// StatefulSets lists StatefulSets in the given namespace (or all namespaces when
// empty). Read-only: a List call.
func StatefulSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.StatefulSet, error) {
	list, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	return list.Items, nil
}

// DaemonSets lists DaemonSets in the given namespace (or all namespaces when
// empty). Read-only: a List call. SystemDaemonSets is a different thing: it
// lists kube-system only, regardless of the scan's namespace filter.
func DaemonSets(ctx context.Context, client kubernetes.Interface, namespace string) ([]appsv1.DaemonSet, error) {
	list, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	return list.Items, nil
}

// Jobs lists Jobs in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func Jobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.Job, error) {
	list, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	return list.Items, nil
}

// CronJobs lists CronJobs in the given namespace (or all namespaces when empty).
// Read-only: a List call.
func CronJobs(ctx context.Context, client kubernetes.Interface, namespace string) ([]batchv1.CronJob, error) {
	list, err := client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing cronjobs: %w", err)
	}
	return list.Items, nil
}

// CollectInventory lists pods and the controller kinds (Deployments, ReplicaSets,
// StatefulSets, DaemonSets, Jobs, CronJobs) in the given namespace (or all
// namespaces when empty). Read-only: List calls only. It stops at the first
// failure and returns what it had, the same as it always did; the scan calls the
// seven functions directly so it can issue them together.
func CollectInventory(ctx context.Context, client kubernetes.Interface, namespace string) (inventory.Inputs, error) {
	var in inventory.Inputs
	var err error

	if in.Pods, err = Pods(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.Deployments, err = Deployments(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.ReplicaSets, err = ReplicaSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.StatefulSets, err = StatefulSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.DaemonSets, err = DaemonSets(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.Jobs, err = Jobs(ctx, client, namespace); err != nil {
		return in, err
	}
	if in.CronJobs, err = CronJobs(ctx, client, namespace); err != nil {
		return in, err
	}
	return in, nil
}
```

One behavioural detail to preserve deliberately: on failure, `CollectInventory` returns the partially filled `inventory.Inputs` alongside the error, exactly as before. Its only production caller discards it.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/collect/
```

Expected: PASS, including the three pre-existing `CollectInventory` tests.

- [ ] **Step 5: Run the full suite**

```bash
go build ./... && go test -p 2 ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -s -m "refactor(collect): split CollectInventory into seven list functions

Pods, Deployments, ReplicaSets, StatefulSets, DaemonSets, Jobs and CronJobs
each list one kind and keep CollectInventory's exact error wrapping, so a
failure reads the same sentence it always did. CollectInventory becomes a
sequential composition of the seven and keeps its signature.

The scan needs the seven separately to issue them together."
```

---

### Task 4: `scanWorkers` — the worker cap and its knob

**Files:**
- Create: `internal/scan/workers.go`
- Test: `internal/scan/workers_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func scanWorkers() int` — unexported, package `scan`. Returns 8 by default; `KUBEAGENT_SCAN_WORKERS` overrides; clamped to `1..64`; never errors.

**Why 8, and why a cap at all.** Eight is enough to overlap the scan's 27 phase-1 reads into a handful of round trips while staying well inside what a single API server answers comfortably, and it is small enough that a hundred kubeagent installs do not add up to a thundering herd. A worker cap is self-limiting in a way a rate limit is not: when the API server slows down, the workers slow down with it, because each one is blocked on its own in-flight request. Under `kubeagent watch` the daemon runs one goroutine per cluster, so the effective cap is `workers × clusters` — task 7 documents that.

- [ ] **Step 1: Write the failing test**

Create `internal/scan/workers_test.go`:

```go
package scan

import "testing"

func TestScanWorkersDefault(t *testing.T) {
	t.Setenv("KUBEAGENT_SCAN_WORKERS", "")
	if got := scanWorkers(); got != 8 {
		t.Errorf("scanWorkers() = %d, want 8", got)
	}
}

func TestScanWorkersHonoursTheEnvironment(t *testing.T) {
	t.Setenv("KUBEAGENT_SCAN_WORKERS", "3")
	if got := scanWorkers(); got != 3 {
		t.Errorf("scanWorkers() = %d, want 3", got)
	}
}

// A bad knob must degrade to a working scan, never to an error.
func TestScanWorkersFallsBackOnAnUnparseableValue(t *testing.T) {
	for _, v := range []string{"eight", "3.5", "1_000", " 4"} {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", v)
		if got := scanWorkers(); got != 8 {
			t.Errorf("KUBEAGENT_SCAN_WORKERS=%q gave %d, want the default 8", v, got)
		}
	}
}

func TestScanWorkersClampsToTheNearerBound(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"0", 1},
		{"-1", 1},
		{"-1000", 1},
		{"1", 1},
		{"64", 64},
		{"65", 64},
		{"100000", 64},
	}
	for _, tc := range cases {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", tc.env)
		if got := scanWorkers(); got != tc.want {
			t.Errorf("KUBEAGENT_SCAN_WORKERS=%q gave %d, want %d", tc.env, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/scan/ -run TestScanWorkers
```

Expected: FAIL — `undefined: scanWorkers`.

- [ ] **Step 3: Write the implementation**

Create `internal/scan/workers.go`:

```go
package scan

import (
	"os"
	"strconv"
)

const (
	// defaultScanWorkers is how many of the scan's independent reads may be in
	// flight at once. Eight overlaps the scan's two dozen phase-1 reads into a
	// handful of round trips while staying well inside what one API server
	// answers comfortably. A worker cap is self-limiting in a way a request rate
	// is not: when the API server slows down, every worker is blocked on its own
	// in-flight request, so kubeagent slows down with it.
	defaultScanWorkers = 8

	minScanWorkers = 1
	maxScanWorkers = 64
)

// scanWorkers returns the scan's worker cap. KUBEAGENT_SCAN_WORKERS overrides
// the default; a value that does not parse is ignored, and a value outside
// 1..64 is clamped to the nearer bound. It never fails — a bad knob degrades to
// a working scan, not to an error.
//
// Under `kubeagent watch` the daemon runs one goroutine per cluster, so the
// effective cap across the process is this number times the number of clusters
// being watched.
func scanWorkers() int {
	s := os.Getenv("KUBEAGENT_SCAN_WORKERS")
	if s == "" {
		return defaultScanWorkers
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultScanWorkers
	}
	if n < minScanWorkers {
		return minScanWorkers
	}
	if n > maxScanWorkers {
		return maxScanWorkers
	}
	return n
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test -p 2 ./internal/scan/ -run TestScanWorkers
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/workers.go internal/scan/workers_test.go
git commit -s -m "feat(scan): add the scan worker cap and its KUBEAGENT_SCAN_WORKERS knob

Eight by default, clamped to 1..64, never an error: a bad knob degrades to
a working scan. A worker cap is self-limiting where a request rate is not —
when the API server slows down, every worker blocks on its own in-flight
request, so kubeagent slows down with it."
```

---

### Task 5: Restructure `Evaluate` into two read phases — still executing sequentially

**Files:**
- Modify: `internal/scan/scan.go:157-418` (the whole `Evaluate` function) and its import block
- Modify: `internal/scan/workers.go` (add `runReads`)

**Interfaces:**
- Consumes: `collect.Pods`, `collect.Deployments`, `collect.ReplicaSets`, `collect.StatefulSets`, `collect.DaemonSets`, `collect.Jobs`, `collect.CronJobs` (task 3).
- Produces: `func runReads(ctx context.Context, reads []func(context.Context) error) []error` — unexported, package `scan`. Returns one error per closure, **in index order**. Task 6 replaces only this function's body.
- `Evaluate`'s signature, `Options`, `Result`, `ReadFailure` and every rendered byte are unchanged.

**This task adds no concurrency.** It lands the shape — two phases, one clock, a single ordered consumption block — while `runReads` still runs the closures one after another in a `for` loop. Every existing test must stay green and the golden file must stay byte-identical, which is exactly the evidence that the restructure preserved behaviour. Task 6 then swaps four lines.

**The one behaviour that genuinely changes, stated plainly:** today a failed pods list returns from `Evaluate` immediately and the remaining reads never run. After this task **all 27 phase-1 reads are issued before the fatal error is discovered**. Nothing about that is unsafe — every one of them is a `get`/`list`, the scan is read-only, and a cluster whose pods list fails is usually one where the other lists fail too — but a caller pointed at a wholly unreachable API server now pays one pool's worth of failing requests instead of one. The alternative, stopping at the first fatal error, would make *which* reads completed depend on the schedule and therefore make `PartialReads` non-deterministic. Determinism wins.

**The determinism argument, which is what a reviewer should check this task against:** the concurrent region has **no shared mutable state at all** — that is stronger than "the shared state is locked". `note()` and `blind()` are never called from a read closure; each closure writes only its own destination variable and returns only its own error; and a sequential block afterwards walks a fixed order. So the rendered order is a property of that block, not of which read answered first.

**The report order.** These 24 positions reproduce today's `PartialReads` order exactly. Verify against the current `internal/scan/scan.go` before changing anything — the order is the contract.

```text
 1  events            (volume-attach)      12  horizontalpodautoscalers
 2  events            (unhealthy)          13  validatingwebhookconfigurations
 3  pods/log          (--logs)             14  mutatingwebhookconfigurations
 4  leases                                 15  persistentvolumes
 5  services                               16  events            (PVC)
 6  endpointslices                         17  storageclasses
 7  ingresses                              18  resourcequotas
 8  secrets           (--certs)            19  networkpolicies
 9  persistentvolumeclaims                 20  events            (FailedCreate)
10  namespaces                             21  nodes/proxy       (--disk-usage)
11  poddisruptionbudgets                   22  nodes/proxy       (--kubelet-health)
                                           23  /readyz           (--control-plane-health)
                                           24  pods/proxy        (--dns-health)
```

Two consequences of the existing helpers that this order must keep working:

- `blind` deduplicates by resource, so positions 21 and 22 both name `nodes/proxy` and only the first one to fire produces a line. Disk usage before kubelet health, as today.
- `note` deduplicates **only** through `blind`, i.e. only for forbidden/unauthorized errors. Four event lists refused by RBAC yield one `events` line; four event lists failing with a transport error yield four `events` lines, at positions 1, 2, 16 and 20. That is today's behaviour and is deliberately kept.

- [ ] **Step 1: Add `runReads` to `internal/scan/workers.go`**

```go
// runReads executes every read closure and returns their errors in INDEX order:
// errs[i] is always the error from reads[i]. The index-ordered contract is what
// lets the body become a bounded worker pool without any caller changing.
//
// Every closure owns its own destination variables and touches nothing another
// closure touches, so this needs no lock and no shared state. Blind spots are
// recorded by the caller afterwards, in a fixed report order.
func runReads(ctx context.Context, reads []func(context.Context) error) []error {
	errs := make([]error, len(reads))
	for i, f := range reads {
		errs[i] = f(ctx)
	}
	return errs
}
```

Add `"context"` to `workers.go`'s imports.

- [ ] **Step 2: Add the nine client-go type imports to `internal/scan/scan.go`**

Phase 1's destination variables are named, so their types must be too. Add to the second import group (alongside the existing `corev1`, `apierrors` and `kubernetes`):

```go
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
```

- [ ] **Step 3: Replace the body of `Evaluate` (scan.go:157-418)**

The comment on `blindReason`, and the `blindSeen`/`blind`/`note` block (scan.go:160-188), carry unchanged from today — copy them verbatim rather than rewriting them.

```go
func Evaluate(ctx context.Context, client kubernetes.Interface, opts Options) (Result, error) {
	// One clock for the whole evaluation. Five separate time.Now() calls made
	// "how old is this?" depend on where in the scan the question was asked;
	// with the reads overlapping, it would depend on the schedule too.
	now := time.Now()

	var partialReads []ReadFailure

	// blind records a blind spot in kubeagent's own words. The reason always
	// starts with "forbidden" so internal/htmlreport.safeReason classifies it as
	// a permission problem rather than degrading it to a generic phrase — and so
	// it never carries the API server's message, which names the requesting
	// identity.
	blindSeen := map[string]bool{}
	blind := func(resource, action string) {
		if blindSeen[resource] {
			return // one line per feature, not one per node
		}
		blindSeen[resource] = true
		partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: blindReason(action)})
	}

	// A refusal is reported in kubeagent's own words. The API server's message
	// interpolates the authorizer's error, which names the requesting identity — a
	// ServiceAccount, an IAM ARN, an OIDC email — and under webhook authorization
	// carries arbitrary third-party text. Everything else keeps the redacted error,
	// which is what makes an unreachable API server distinguishable from a refused one.
	note := func(resource string, err error) {
		switch {
		case err == nil:
			return
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
			blind(resource, "read "+resource)
		default:
			partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: redact.Error(err)})
		}
	}

	// ------------------------------------------------------------------ phase 1
	//
	// Every read that depends on nothing but opts. The closures share no mutable
	// state: each writes only its own destination variable and returns only its
	// own error, so no entry in this list can observe anything another entry
	// does. Nothing here appends to partialReads — blind spots are recorded
	// afterwards, in report order, which is what keeps the rendered output
	// independent of which read answered first.
	var reads []func(context.Context) error
	add := func(f func(context.Context) error) int {
		reads = append(reads, f)
		return len(reads) - 1
	}

	var (
		pods     []corev1.Pod
		deploys  []appsv1.Deployment
		rsets    []appsv1.ReplicaSet
		stses    []appsv1.StatefulSet
		dsets    []appsv1.DaemonSet
		jobs     []batchv1.Job
		cronJobs []batchv1.CronJob
		nodes    []corev1.Node

		attachEvents       []corev1.Event
		unhealthyEvents    []corev1.Event
		pvcEvents          []corev1.Event
		failedCreateEvents []corev1.Event
		leases             []coordinationv1.Lease
		svcs               []corev1.Service
		slices             []discoveryv1.EndpointSlice
		ings               []networkingv1.Ingress
		tlsSecrets         []corev1.Secret
		pvcs               []corev1.PersistentVolumeClaim
		namespaces         []corev1.Namespace
		pdbs               []policyv1.PodDisruptionBudget
		hpas               []autoscalingv2.HorizontalPodAutoscaler
		vwc                []admissionv1.ValidatingWebhookConfiguration
		mwc                []admissionv1.MutatingWebhookConfiguration
		pvs                []corev1.PersistentVolume
		storageClasses     []storagev1.StorageClass
		quotas             []corev1.ResourceQuota
		nps                []networkingv1.NetworkPolicy
	)

	iPods := add(func(ctx context.Context) error {
		var err error
		pods, err = collect.Pods(ctx, client, opts.Namespace)
		return err
	})
	iDeploys := add(func(ctx context.Context) error {
		var err error
		deploys, err = collect.Deployments(ctx, client, opts.Namespace)
		return err
	})
	iRSets := add(func(ctx context.Context) error {
		var err error
		rsets, err = collect.ReplicaSets(ctx, client, opts.Namespace)
		return err
	})
	iSTSes := add(func(ctx context.Context) error {
		var err error
		stses, err = collect.StatefulSets(ctx, client, opts.Namespace)
		return err
	})
	iDSets := add(func(ctx context.Context) error {
		var err error
		dsets, err = collect.DaemonSets(ctx, client, opts.Namespace)
		return err
	})
	iJobs := add(func(ctx context.Context) error {
		var err error
		jobs, err = collect.Jobs(ctx, client, opts.Namespace)
		return err
	})
	iCronJobs := add(func(ctx context.Context) error {
		var err error
		cronJobs, err = collect.CronJobs(ctx, client, opts.Namespace)
		return err
	})
	iNodes := add(func(ctx context.Context) error {
		var err error
		nodes, err = collect.Nodes(ctx, client)
		return err
	})
	iAttach := add(func(ctx context.Context) error {
		var err error
		attachEvents, err = collect.VolumeAttachEvents(ctx, client, opts.Namespace)
		return err
	})
	iUnhealthy := add(func(ctx context.Context) error {
		var err error
		unhealthyEvents, err = collect.UnhealthyEvents(ctx, client, opts.Namespace)
		return err
	})
	iLeases := add(func(ctx context.Context) error {
		var err error
		leases, err = collect.NodeLeases(ctx, client)
		return err
	})
	iSvcs := add(func(ctx context.Context) error {
		var err error
		svcs, err = collect.Services(ctx, client, opts.Namespace)
		return err
	})
	iSlices := add(func(ctx context.Context) error {
		var err error
		slices, err = collect.EndpointSlices(ctx, client, opts.Namespace)
		return err
	})
	iIngs := add(func(ctx context.Context) error {
		var err error
		ings, err = collect.Ingresses(ctx, client, opts.Namespace)
		return err
	})
	var iSecrets int
	if opts.Certs {
		iSecrets = add(func(ctx context.Context) error {
			var err error
			tlsSecrets, err = collect.TLSSecrets(ctx, client, opts.Namespace)
			return err
		})
	}
	iPVCs := add(func(ctx context.Context) error {
		var err error
		pvcs, err = collect.PersistentVolumeClaims(ctx, client, opts.Namespace)
		return err
	})
	// forbidden/absent → nil, namespace checks skipped
	iNamespaces := add(func(ctx context.Context) error {
		var err error
		namespaces, err = collect.Namespaces(ctx, client)
		return err
	})
	// forbidden/absent → nil, check skipped
	iPDBs := add(func(ctx context.Context) error {
		var err error
		pdbs, err = collect.PodDisruptionBudgets(ctx, client, opts.Namespace)
		return err
	})
	// forbidden/absent → nil, check skipped
	iHPAs := add(func(ctx context.Context) error {
		var err error
		hpas, err = collect.HorizontalPodAutoscalers(ctx, client, opts.Namespace)
		return err
	})
	var iVWC, iMWC int
	if opts.Namespace == "" { // webhook backends can live in any namespace; only sound cluster-wide
		iVWC = add(func(ctx context.Context) error {
			var err error
			vwc, err = collect.ValidatingWebhookConfigurations(ctx, client)
			return err
		})
		iMWC = add(func(ctx context.Context) error {
			var err error
			mwc, err = collect.MutatingWebhookConfigurations(ctx, client)
			return err
		})
	}
	iPVs := add(func(ctx context.Context) error {
		var err error
		pvs, err = collect.PersistentVolumes(ctx, client)
		return err
	})
	iPVCEvents := add(func(ctx context.Context) error {
		var err error
		pvcEvents, err = collect.PVCEvents(ctx, client, opts.Namespace)
		return err
	})
	iStorageClasses := add(func(ctx context.Context) error {
		var err error
		storageClasses, err = collect.StorageClasses(ctx, client)
		return err
	})
	iQuotas := add(func(ctx context.Context) error {
		var err error
		quotas, err = collect.ResourceQuotas(ctx, client, opts.Namespace)
		return err
	})
	iNPs := add(func(ctx context.Context) error {
		var err error
		nps, err = collect.NetworkPolicies(ctx, client, opts.Namespace)
		return err
	})
	iFailedCreate := add(func(ctx context.Context) error {
		var err error
		failedCreateEvents, err = collect.FailedCreateEvents(ctx, client, opts.Namespace)
		return err
	})

	errs := runReads(ctx, reads)

	// The fatal reads, checked in the order CollectInventory checked them so an
	// unreachable cluster still reports the error it always reported. Every read
	// above has already been issued by the time we get here: stopping the pool at
	// the first fatal error would make which reads completed depend on the
	// schedule, and with it PartialReads.
	for _, i := range []int{iPods, iDeploys, iRSets, iSTSes, iDSets, iJobs, iCronJobs, iNodes} {
		if errs[i] != nil {
			return Result{}, errs[i]
		}
	}
	inputs := inventory.Inputs{
		Pods: pods, Deployments: deploys, ReplicaSets: rsets, StatefulSets: stses,
		DaemonSets: dsets, Jobs: jobs, CronJobs: cronJobs,
	}

	// Pure work that decides what phase 2 reads. Deciding it here, sequentially,
	// is what makes the phase-2 work list independent of the schedule.
	events := append(attachEvents, unhealthyEvents...)
	findings := diagnose.Run(diagnose.DefaultDetectors(now), collect.FactsFrom(inputs.Pods, events))

	type logTarget struct {
		finding   int
		namespace string
		pod       string
		container string
	}
	var logTargets []logTarget
	if opts.Logs {
		enriched := map[string]bool{} // one log fetch + one enriched finding per pod/container
		for i := range findings {
			if findings[i].Container == "" {
				continue
			}
			key := findings[i].Pod + "/" + findings[i].Container
			if enriched[key] {
				continue // a container that trips two detectors (e.g. CrashLoop + OOM) is enriched once
			}
			ns, name, ok := splitNamespacedName(findings[i].Pod) // "ns/pod"
			if !ok {
				continue
			}
			enriched[key] = true
			logTargets = append(logTargets, logTarget{finding: i, namespace: ns, pod: name, container: findings[i].Container})
		}
	}

	var cdns []corev1.Pod
	if opts.DNSHealth {
		cdns = coreDNSPods(inputs.Pods)
	}

	// ------------------------------------------------------------------ phase 2
	//
	// The fan-outs, one flat pool: a node's kubelet is no slower to answer than a
	// CoreDNS pod's metrics, so splitting them into separate pools would only add
	// barriers. Same rule as phase 1 — every closure owns its own slot.
	var reads2 []func(context.Context) error
	add2 := func(f func(context.Context) error) int {
		reads2 = append(reads2, f)
		return len(reads2) - 1
	}

	logText := make([]string, len(logTargets))
	logOK := make([]bool, len(logTargets))
	logIdx := make([]int, len(logTargets))
	for k := range logTargets {
		logIdx[k] = add2(func(ctx context.Context) error {
			var err error
			logText[k], logOK[k], err = collect.PreviousLogs(ctx, client, logTargets[k].namespace, logTargets[k].pod, logTargets[k].container)
			return err
		})
	}

	var (
		summaries []diskusage.NodeSummary
		statsOK   []bool
		statsIdx  []int
	)
	if opts.DiskUsage {
		summaries = make([]diskusage.NodeSummary, len(nodes))
		statsOK = make([]bool, len(nodes))
		statsIdx = make([]int, len(nodes))
		for k := range nodes {
			statsIdx[k] = add2(func(ctx context.Context) error {
				var err error
				summaries[k], statsOK[k], err = collect.NodeStats(ctx, client, nodes[k].Name)
				return err
			})
		}
	}

	var probes []nodehealth.Probe
	if opts.KubeletHealth {
		probes = make([]nodehealth.Probe, len(nodes))
		for k := range nodes {
			add2(func(ctx context.Context) error {
				probes[k] = collect.KubeletHealthz(ctx, client, nodes[k].Name)
				return nil
			})
		}
	}

	var readyz controlplane.Probe
	if opts.ControlPlaneHealth {
		add2(func(ctx context.Context) error {
			readyz = collect.ControlPlaneReadyz(ctx, client)
			return nil
		})
	}

	dnsBody := make([][]byte, len(cdns))
	dnsCode := make([]int, len(cdns))
	for k := range cdns {
		add2(func(ctx context.Context) error {
			dnsBody[k], dnsCode[k] = collect.CoreDNSMetrics(ctx, client, cdns[k].Namespace, cdns[k].Name)
			return nil
		})
	}

	errs2 := runReads(ctx, reads2)

	// ------------------------------------------------------- the report order
	//
	// Every blind spot and every read failure is recorded here, in this fixed
	// order, after both pools have finished. Nothing above appends to
	// partialReads, so the rendered order is a property of this block and not of
	// which read answered first. The numbers match the report-order table in
	// docs/superpowers/specs/2026-07-30-bounded-scan-concurrency-design.md.
	note("events", errs[iAttach])     // 1
	note("events", errs[iUnhealthy])  // 2
	for k := range logTargets {       // 3
		if err := errs2[logIdx[k]]; apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			blind("pods/log", "get pods/log")
		}
		if logOK[k] {
			if clue := logscan.Classify(logText[k]); clue.Cause != "" {
				findings[logTargets[k].finding].LogCause = clue.Cause
				findings[logTargets[k].finding].LogExcerpt = clue.Excerpt
			}
		}
	}
	note("leases", errs[iLeases])                 // 4
	note("services", errs[iSvcs])                 // 5
	note("endpointslices", errs[iSlices])         // 6
	note("ingresses", errs[iIngs])                // 7

	var certReport *certhealth.Report // 8
	if opts.Certs {
		warn := opts.CertWarnDays
		if warn <= 0 {
			warn = 30
		}
		rep := certhealth.Assess(tlsSecrets, ings, warn, now)
		if err := errs[iSecrets]; apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			rep.Forbidden = true
			blind("secrets", "list secrets")
		} else {
			note("secrets", err)
		}
		certReport = &rep
	}

	note("persistentvolumeclaims", errs[iPVCs])        // 9
	note("namespaces", errs[iNamespaces])              // 10
	note("poddisruptionbudgets", errs[iPDBs])          // 11
	note("horizontalpodautoscalers", errs[iHPAs])      // 12
	if opts.Namespace == "" {
		note("validatingwebhookconfigurations", errs[iVWC]) // 13
		note("mutatingwebhookconfigurations", errs[iMWC])   // 14
	}
	note("persistentvolumes", errs[iPVs])              // 15
	note("events", errs[iPVCEvents])                   // 16
	note("storageclasses", errs[iStorageClasses])      // 17
	note("resourcequotas", errs[iQuotas])              // 18
	note("networkpolicies", errs[iNPs])                // 19
	note("events", errs[iFailedCreate])                // 20

	var diskReport diskusage.Report // 21
	if opts.DiskUsage {
		var kept []diskusage.NodeSummary
		for k := range nodes {
			if err := errs2[statsIdx[k]]; err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					blind("nodes/proxy", "get nodes/proxy")
				}
				continue // an unreachable kubelet is a node problem, not a grant problem
			}
			if statsOK[k] {
				kept = append(kept, summaries[k])
			}
		}
		diskReport = diskusage.Assess(kept, opts.DiskThreshold)
	}

	var kubeletHealth nodehealth.Report // 22
	if opts.KubeletHealth {
		kubeletHealth = nodehealth.Assess(probes)
		if kubeletHealth.Forbidden > 0 {
			blind("nodes/proxy", "get nodes/proxy")
		}
	}

	if opts.ControlPlaneHealth && readyz.Status == "forbidden" { // 23
		blind("/readyz", "get /readyz")
	}

	var dnsReport dnshealth.Report // 24
	if opts.DNSHealth {
		ratio := opts.DNSServfailRatio
		if ratio <= 0 || ratio > 1 {
			ratio = 0.05
		}
		agg := map[string]int64{}
		forbidden, unreachable := 0, 0
		for k := range cdns {
			switch {
			case dnsCode[k] == 401 || dnsCode[k] == 403:
				forbidden++
			case dnsCode[k] == 200:
				for rc, n := range dnshealth.ParseResponses(dnsBody[k]) {
					agg[rc] += n
				}
			default:
				unreachable++
			}
		}
		if forbidden > 0 {
			blind("pods/proxy", "get pods/proxy")
		}
		dnsReport = dnshealth.Assess(agg, len(cdns), forbidden, unreachable, ratio, 100)
	}

	// ------------------------------------------------ pure: no reads past here
	workloads := inventory.Assemble(inputs, findings)
	batchhealth.Annotate(workloads, inputs.Jobs)

	health := clusterhealth.Assess(nodes, clusterhealth.Heartbeat{Leases: leases, Now: now, Threshold: opts.NodeHeartbeatThreshold}, opts.ExpectedNodes, workloads)
	health.ScopeNote = clusterhealth.NamespaceScopeNote(opts.Namespace)

	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)
	svchealth.AnnotateEndpointCause(serviceIssues, svcs, inputs.Pods, health.DownNodes)
	ingressIssues := ingresshealth.Assess(ings, svcs, slices, backends, inputs.Pods, health.DownNodes)

	var securityIssues []secscan.Finding
	if opts.Security {
		p, s := inputs.Pods, svcs
		if opts.Namespace == "" {
			p = nonSystemPods(p)
			s = nonSystemServices(s)
		}
		securityIssues = secscan.Assess(p, s, inputs.ReplicaSets)
	}

	stuckTerminating := termhealth.Assess(namespaces, inputs.Pods, pvcs, 2*time.Minute, now)
	pdbIssues := pdbhealth.Assess(pdbs)
	hpaIssues := hpahealth.Assess(hpas)

	var webhookIssues []webhookhealth.Issue
	if opts.Namespace == "" {
		webhookThreshold := opts.WebhookTimeoutThreshold
		if webhookThreshold <= 0 {
			webhookThreshold = 15
		}
		webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices, webhookThreshold)
	}

	pvcReclaim := pvcreclaim.Assess(pvcs, pvs)
	pvcIssues := pvchealth.Assess(pvcs, pvcEvents, storageClasses, pvs)

	quotaThreshold := opts.QuotaThreshold
	if quotaThreshold <= 0 || quotaThreshold > 1 {
		quotaThreshold = 0.90
	}
	quotaIssues := quotahealth.Assess(quotas, quotaThreshold)

	result := inventory.Prioritize(workloads, inventory.Opts{
		IncludeRestarts: opts.IncludeRestarts,
		IncludeCron:     opts.IncludeCron,
	})

	podLabels := make(map[string]map[string]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		podLabels[p.Namespace+"/"+p.Name] = p.Labels
	}
	podPVCs := make(map[string][]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				key := p.Namespace + "/" + p.Name
				podPVCs[key] = append(podPVCs[key], v.PersistentVolumeClaim.ClaimName)
			}
		}
	}
	createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)
	rollouthealth.Annotate(result.Workloads, inputs.Deployments)
	netpolicy.Annotate(result.Workloads, podLabels, nps)
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, now)
	rootcause.Annotate(result.Workloads, health.DownNodes)
	rootcause.AnnotatePVC(result.Workloads, podPVCs, pvcIssues)
	rootcause.AnnotateRegistry(result.Workloads)
	confidence.Annotate(result.Workloads)

	return Result{Inputs: inputs, Nodes: nodes, NodeReserve: nodereserve.Assess(nodes), PVCReclaim: pvcReclaim, DiskUsage: diskReport, Health: health, Inventory: result, ServiceIssues: serviceIssues, IngressIssues: ingressIssues, PVCIssues: pvcIssues, SecurityIssues: securityIssues, KubeletHealth: kubeletHealth, ControlPlane: readyz, DNS: dnsReport, Certificates: certReport, StuckTerminating: stuckTerminating, PDBIssues: pdbIssues, HPAIssues: hpaIssues, WebhookIssues: webhookIssues, QuotaIssues: quotaIssues, PartialReads: partialReads}, nil
}
```

- [ ] **Step 4: Build and run the scan package's existing tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./internal/scan/
```

Expected: PASS, with every pre-existing test in `scan_test.go` green and **unmodified**. If a pre-existing test needs editing to pass, that is a behaviour change — stop and report it rather than editing the test.

- [ ] **Step 5: Confirm the golden output is byte-identical**

```bash
go test -p 2 ./internal/report/
git status --short internal/report/testdata/golden-scan.txt
```

Expected: PASS and no modification to the golden file.

- [ ] **Step 6: Run the full suite**

```bash
go vet ./... && go test -p 2 ./...
```

Expected: PASS, every package.

- [ ] **Step 7: Commit**

```bash
git add internal/scan/scan.go internal/scan/workers.go
git commit -s -m "refactor(scan): split Evaluate into two read phases

Phase 1 is every read that depends on nothing but Options; phase 2 is the
fan-outs whose work list phase 1 determines. Both go through runReads,
which still executes them one at a time — this commit lands the shape, not
the concurrency.

The shape is what makes the concurrency safe. No read closure calls note()
or blind(); each writes only its own destination and returns only its own
error; a single sequential block afterwards walks a fixed report order. The
concurrent region will have no shared mutable state at all, which is a
stronger property than locking shared state.

Evaluate also takes one clock instead of five, so everything ages against
the same instant regardless of where in the scan it is asked about.

One behaviour changes: all 27 phase-1 reads are now issued before a fatal
error is discovered, where the pods list used to return immediately. Every
one is a get/list on a read-only scan, and stopping early would make which
reads completed depend on the schedule, and with it PartialReads."
```

---

### Task 6: Run the two phases concurrently, and prove the output does not move

**Files:**
- Modify: `internal/scan/workers.go` (the body of `runReads` only)
- Test: `internal/scan/scan_test.go` (append)
- Modify: `.github/workflows/ci.yml:20-24`

**Interfaces:**
- Consumes: `parallel.Do` (task 1), `scanWorkers` (task 4), `runReads` (task 5).
- Produces: no new interface. `runReads` keeps its signature.

**Write the tests first and watch them pass against the sequential `runReads`.** That is not a wasted step: a determinism test that only passes once concurrency is enabled is a test that was written to the implementation. These four tests must hold both before and after step 3 — the difference is that after step 3 they are actually load-bearing.

Test C is the important one. It cannot use a fake-clientset reactor: `k8stesting.Fake.Invokes` holds a write lock across the whole reactor body (`k8s.io/client-go/testing/fake.go:133-153`), so reactor-based delays serialize instead of overlapping. Measured: eight concurrent reads through a 50 ms sleeping reactor took 403.9 ms, i.e. fully serial, with an arbitrary completion order. The test therefore drives the phase-2 node fan-out over **real HTTP** through the existing `nodeStatsFailingClient` wrapper, which swaps `CoreV1().RESTClient()` for a real REST client while every typed client still delegates to the fake.

- [ ] **Step 1: Write the determinism tests**

Append to `internal/scan/scan_test.go`. Add `reflect` and `sync` to its imports; `errors`, `strings`, `httptest`, `schema`, `k8stesting`, `rest`, `runtime` and `kubernetes` are already there.

```go
// slowKubeletHealthzServer answers /api/v1/nodes/<name>/proxy/healthz with a 500
// after a delay that is LONGEST for the first node and shortest for the last, so
// the probes finish in exactly the reverse of the order they were dispatched.
//
// It goes over real HTTP through the swapped RESTClient because that is the only
// way to get genuine overlap: k8stesting.Fake.Invokes holds a write lock across
// the whole reactor body, so a reactor that sleeps serialises the calls instead
// of overlapping them.
func slowKubeletHealthzServer(t *testing.T, nodes []string) (rest.Interface, func() []string) {
	t.Helper()

	index := map[string]int{}
	for i, n := range nodes {
		index[n] = i
	}

	var mu sync.Mutex
	var completed []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := nodeNameFromHealthzPath(r.URL.Path)
		i, ok := index[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(time.Duration(len(nodes)-i) * 25 * time.Millisecond)

		mu.Lock()
		completed = append(completed, name)
		mu.Unlock()

		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("[-]check-" + name + " failed\n"))
	}))
	t.Cleanup(server.Close)

	// QPS -1 for the same reason production uses it: client-go's default token
	// bucket would meter these probes and defeat the inversion under test.
	real, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, QPS: -1})
	if err != nil {
		t.Fatalf("building a client for the slow kubelet server: %v", err)
	}
	return real.CoreV1().RESTClient(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), completed...)
	}
}

// nodeNameFromHealthzPath pulls <name> out of /api/v1/nodes/<name>/proxy/healthz.
func nodeNameFromHealthzPath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) < 4 || parts[2] != "nodes" {
		return ""
	}
	return parts[3]
}

// The phase-2 node fan-out reports in node order even when the probes finish in
// exactly the opposite order. nodehealth.Assess preserves probe order, so
// Unhealthy is a direct read-out of the order the pool wrote its slots in.
func TestKubeletHealthFanOutReportsInNodeOrderNotCompletionOrder(t *testing.T) {
	names := []string{"node-0", "node-1", "node-2", "node-3", "node-4"}
	objs := make([]runtime.Object, 0, len(names))
	for _, n := range names {
		objs = append(objs, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
	}

	restClient, completedFn := slowKubeletHealthzServer(t, names)
	client := nodeStatsFailingClient{Interface: fake.NewSimpleClientset(objs...), rest: restClient}

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8") // all five probes in flight at once
	res, err := Evaluate(context.Background(), client, Options{KubeletHealth: true})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, iss := range res.KubeletHealth.Unhealthy {
		got = append(got, iss.Node)
	}
	if !reflect.DeepEqual(got, names) {
		t.Errorf("KubeletHealth.Unhealthy = %v, want %v — the report must follow node order, not completion order", got, names)
	}
}

// PartialReads follows the fixed report order, not the order the refusals
// arrived. The reactors below are registered in the reverse of the expected
// order, so a report that followed registration or completion order comes out
// backwards and this test fails.
func TestPartialReadsFollowReportOrderNotCompletionOrder(t *testing.T) {
	client := fake.NewSimpleClientset()
	forbid := func(resource string) {
		client.Fake.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
		})
	}
	forbid("networkpolicies")
	forbid("persistentvolumes")
	forbid("namespaces")
	forbid("services")

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, p := range res.PartialReads {
		got = append(got, p.Resource)
	}
	want := []string{"services", "namespaces", "persistentvolumes", "networkpolicies"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PartialReads = %v, want %v", got, want)
	}
}

// Four event lists fail with a transport error, so four entries are recorded —
// note() deduplicates only through blind(), i.e. only for refusals. This pins the
// "four identical lines" behaviour that the report order deliberately keeps.
func TestFourFailingEventListsRecordFourEntries(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.Fake.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.PartialReads) != 4 {
		t.Fatalf("PartialReads = %+v, want 4 entries (volume-attach, unhealthy, PVC, FailedCreate)", res.PartialReads)
	}
	for i, p := range res.PartialReads {
		if p.Resource != "events" {
			t.Errorf("PartialReads[%d].Resource = %q, want %q", i, p.Resource, "events")
		}
	}
}

// determinismFixture builds a cluster with a crash-looping pod, a Deployment, a
// Service with no endpoints, a PVC and two nodes. Every timestamp is 48 hours in
// the past so inventory.HumanAge renders whole days ("2d"): an age rendered in
// seconds would differ between two runs milliseconds apart and the comparison
// below would be testing the clock rather than the pool.
func determinismFixture() []runtime.Object {
	old := metav1.NewTime(time.Now().Add(-48 * time.Hour))
	return []runtime.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", CreationTimestamp: old},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b", CreationTimestamp: old},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", CreationTimestamp: old, Labels: map[string]string{"app": "api"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
				Name: "api", RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"}},
			}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "worker-1", CreationTimestamp: old, Labels: map[string]string{"app": "worker"}},
			Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/2 nodes are available",
			}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api", CreationTimestamp: old},
			Spec:   appsv1.DeploymentSpec{Replicas: ptrInt32(3)},
			Status: appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 1}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api", CreationTimestamp: old},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "api"}, Ports: []corev1.ServicePort{{Port: 80}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data", CreationTimestamp: old},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
	}
}

// Two runs at different worker counts must produce an identical Result. The
// third run repeats the second so that a fixture whose output drifts with the
// clock fails as a fixture bug rather than as a concurrency bug.
func TestEvaluateIsIndependentOfTheWorkerCount(t *testing.T) {
	run := func(workers string) Result {
		t.Setenv("KUBEAGENT_SCAN_WORKERS", workers)
		res, err := Evaluate(context.Background(), fake.NewSimpleClientset(determinismFixture()...), Options{Security: true, IncludeRestarts: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	eight, eightAgain := run("8"), run("8")
	if !reflect.DeepEqual(eight, eightAgain) {
		t.Fatalf("two runs at the same worker count differed — the fixture is not stable, so the comparison below would be meaningless")
	}

	one := run("1")
	if !reflect.DeepEqual(one, eight) {
		t.Errorf("Evaluate at 1 worker differs from Evaluate at 8 workers:\n one   = %+v\n eight = %+v", one, eight)
	}
}

// Repeating the same scan many times must produce the same Result every time.
// The reactor delays each list by an amount derived from the resource name — a
// stable delay, no randomness — which is enough to vary which goroutine reaches
// the fake's lock first, and so to sample real orderings.
func TestEvaluateIsStableAcrossRepeatedRuns(t *testing.T) {
	newClient := func() *fake.Clientset {
		c := fake.NewSimpleClientset(determinismFixture()...)
		c.Fake.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			time.Sleep(time.Duration(len(action.GetResource().Resource)%5) * time.Millisecond)
			return false, nil, nil // fall through to the tracker
		})
		return c
	}

	t.Setenv("KUBEAGENT_SCAN_WORKERS", "8")
	first, err := Evaluate(context.Background(), newClient(), Options{Security: true, IncludeRestarts: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 20; i++ {
		got, err := Evaluate(context.Background(), newClient(), Options{Security: true, IncludeRestarts: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differed from run 0 — the output depends on the schedule", i)
		}
	}
}
```

If `ptrInt32` does not already exist in the `scan` package's test files, add it next to `determinismFixture`:

```go
func ptrInt32(n int32) *int32 { return &n }
```

- [ ] **Step 2: Run the new tests against the still-sequential `runReads`**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/scan/ -run 'TestKubeletHealthFanOut|TestPartialReadsFollowReportOrder|TestFourFailingEventLists|TestEvaluateIsIndependent|TestEvaluateIsStable' -v
```

Expected: PASS. These assert properties that must hold in both worlds; they are the safety net for step 3, not a demonstration of it.

- [ ] **Step 3: Replace the body of `runReads` with the pool**

In `internal/scan/workers.go`, add `"github.com/imantaba/kubeagent/internal/parallel"` to the imports and replace the loop:

```go
func runReads(ctx context.Context, reads []func(context.Context) error) []error {
	return parallel.Do(ctx, scanWorkers(), len(reads), func(ctx context.Context, i int) error {
		return reads[i](ctx)
	})
}
```

- [ ] **Step 4: Run the scan package under the race detector**

```bash
go test -race -p 2 ./internal/scan/
```

Expected: PASS with no race reported. A race here means a read closure is touching something another closure touches — find it, do not paper over it with a mutex.

- [ ] **Step 5: Confirm the reversal test is actually inverting the schedule**

The concurrency is now real, so `TestKubeletHealthFanOutReportsInNodeOrderNotCompletionOrder` should complete in roughly 125 ms (the longest single probe) rather than roughly 375 ms (the sum):

```bash
go test -p 2 ./internal/scan/ -run TestKubeletHealthFanOut -v
```

Expected: PASS, with the reported time closer to 0.1s than to 0.4s. If it is still ~0.4s the probes are not overlapping — investigate before continuing.

- [ ] **Step 6: Run the full suite, with and without the race detector**

```bash
go vet ./... && go test -p 2 ./... && go test -race -p 2 ./...
```

Expected: PASS both times. The `-race` run takes about two minutes. The golden file must be unchanged:

```bash
git status --short internal/report/testdata/golden-scan.txt
```

- [ ] **Step 7: Add `-race` to CI**

In `.github/workflows/ci.yml`, change the test step's command from `go test ./...` to:

```yaml
      - name: Test
        run: go test -race ./...
```

Keep the surrounding `go vet ./...` and `go build ./...` steps as they are, and keep whatever step names the file already uses.

- [ ] **Step 8: Commit**

```bash
git add internal/scan/workers.go internal/scan/scan_test.go .github/workflows/ci.yml
git commit -s -m "perf(scan): run the scan's independent reads through a bounded pool

runReads now dispatches through parallel.Do with the scanWorkers cap. That
is the whole product change — the two-phase shape and the fixed report
order landed in the previous commit, which is what makes this diff small.

The determinism tests hold in both worlds by design: a test that only
passes once concurrency is on is a test written to the implementation.
They cover worker-count invariance, invariance across twenty repeats, the
fixed blind-spot order, and a real schedule inversion — five kubelet probes
answered over HTTP in exactly reverse order, still reported in node order.
The inversion goes over real HTTP because k8stesting.Fake.Invokes holds a
write lock across the reactor body, so a reactor-based delay serialises.

CI now runs the suite under -race."
```

---

### Task 7: Documentation

**Files:**
- Modify: `CLAUDE.md` (the sequential-scan invariant)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: `website/docs/roadmap.md` (the Theme H bullet)
- Create: `website/docs/features/tuning.md`
- Modify: `website/mkdocs.yml` (nav)
- Modify (on disk only, never staged): `docs/go-concepts.md`

**Interfaces:** none — documentation only. No `main.go` change: the two knobs are environment variables, deliberately not flags, because they tune how kubeagent talks to an API server rather than what the scan reports.

- [ ] **Step 1: Rewrite the sequential-scan invariant in `CLAUDE.md`**

Find the bullet beginning `- v1 CLI (`scan`) is **sequential** — no goroutines.` and replace that opening sentence — keeping the rest of the bullet (the `internal/watch`, `internal/mcp`, `internal/gate`, `internal/tui`, `internal/rbacprofile` and `internal/htmlreport` clauses) exactly as it is — with:

```markdown
- **`scan` runs its independent reads through a bounded worker pool**
  (`internal/parallel`, capped by `KUBEAGENT_SCAN_WORKERS`, 8 by default). The
  v1 "sequential, no goroutines" simplification is retired. Determinism is
  preserved by construction, not by discipline: no read closure touches shared
  state, each writes only its own destination, and a sequential block afterwards
  walks a fixed report order — so the rendered bytes are never a function of
  which read answered first. `internal/parallel` must never import
  `internal/remediate` or `internal/explain`. `internal/watch` is no longer the
  only documented long-lived-process exception: …
```

(The `…` continues into the existing text, unchanged.)

- [ ] **Step 2: Write the `CHANGELOG.md` entry**

`## [Unreleased]` is currently empty. Fill it:

```markdown
## [Unreleased]

### Added

- `KUBEAGENT_SCAN_WORKERS` — how many of `scan`'s independent cluster reads may
  be in flight at once. 8 by default, clamped to 1..64. A value that does not
  parse is ignored rather than raised as an error. Under `kubeagent watch` the
  daemon runs one goroutine per cluster, so the effective cap across the process
  is this number times the number of clusters watched.
- `KUBEAGENT_QPS` and `KUBEAGENT_BURST` — restore a client-side request rate
  limit for anyone who needs one. Unset, kubeagent applies none.

### Changed

- `scan` now issues its independent cluster reads through a bounded worker pool
  instead of one at a time. Output is unchanged, byte for byte: blind spots and
  read failures are recorded by a sequential block that walks a fixed report
  order, so the rendered order can never depend on which read answered first.
- kubeagent no longer accepts client-go's default client-side rate limiter
  (5 QPS, burst 10 per API-group client). That default metered the scan against
  itself: measured on a three-node cluster, a scan with every add-on enabled
  took 2.42s with the limiter and 0.15s without, for byte-identical output. Load
  shedding is left to the API server's own Priority and Fairness, which knows
  what the server can take; `KUBEAGENT_QPS` restores a client-side limit.
```

- [ ] **Step 3: Update the Theme H bullet in `website/docs/roadmap.md`**

The bullet currently ends `The rest of Theme H — the v1.0 production contract — remains ahead.` Insert before that sentence:

```markdown
Slice 5 — bounded scan concurrency — has shipped: `scan`'s independent reads run
through a bounded worker pool (`internal/parallel`, `KUBEAGENT_SCAN_WORKERS`,
8 by default), and kubeagent no longer accepts client-go's default 5 QPS
client-side rate limiter, which had been metering the scan against itself —
2.42s versus 0.15s for byte-identical output on a three-node cluster. Ordering
is preserved by construction: no read closure touches shared state, and a
sequential block afterwards walks a fixed report order
([tuning](features/tuning.md)).
```

Adjust the slice number if the surrounding text has moved on; match the numbering the bullet already uses.

- [ ] **Step 4: Write `website/docs/features/tuning.md`**

```markdown
# Performance tuning

`kubeagent scan` reads a lot of a cluster in one go — pods and every controller
kind, events, services, endpoint slices, ingresses, PVCs, namespaces, PDBs,
HPAs, webhook configurations, PVs, storage classes, quotas and network
policies, plus a per-node or per-pod read for each add-on you enable. Those
reads are independent of one another, so kubeagent issues them together.

Everything on this page is read-only. None of it changes what a scan reports —
only how quickly it gets there.

## `KUBEAGENT_SCAN_WORKERS`

How many of the scan's independent reads may be in flight at once.

| | |
|---|---|
| Default | `8` |
| Range | `1`–`64`, clamped to the nearer bound |
| Bad value | Ignored — the default is used, and the scan still runs |

```bash
KUBEAGENT_SCAN_WORKERS=16 kubeagent scan --disk-usage --kubelet-health
```

Raise it on a large cluster where the scan is dominated by per-node reads; lower
it to `1` to reproduce the old strictly-sequential behaviour, which is
occasionally useful when you are trying to attribute API-server load.

A worker cap is self-limiting in a way a request rate is not. When the API
server slows down, every worker is blocked on its own in-flight request, so
kubeagent slows down with it. A fixed request rate holds the same number whether
the server is idle or dying.

**Under `kubeagent watch`, this multiplies.** The daemon runs one goroutine per
watched cluster, so a daemon watching four clusters at the default cap may have
up to 32 reads in flight — eight against each of four different API servers.
Nothing is shared between them, so per-server load is still eight; it is the
daemon's own file-descriptor and memory use that scales.

## `KUBEAGENT_QPS` and `KUBEAGENT_BURST`

A client-side request rate limit. **Unset, kubeagent applies none**, which is
the default and the recommended setting.

| | |
|---|---|
| `KUBEAGENT_QPS` | Requests per second. Must be a positive number; anything else is ignored |
| `KUBEAGENT_BURST` | Bucket size. Must be a positive integer; only takes effect alongside `KUBEAGENT_QPS` |

```bash
KUBEAGENT_QPS=20 KUBEAGENT_BURST=40 kubeagent scan
```

### Why the default is "no client-side limit"

client-go installs a 5 requests-per-second, burst-10 token bucket on **each
per-API-group client** when a program leaves `QPS` unset. Nearly every read a
scan makes goes through the core API group, so that one bucket metered the whole
scan.

Measured on a three-node cluster, `scan` with every add-on enabled:

| | Wall clock |
|---|---|
| With client-go's default limiter | 2.42 s |
| With no client-side limiter | 0.15 s |

Byte-identical output, and no concurrency involved in either run — that is
purely the limiter.

Load shedding belongs on the server, where the information is. Kubernetes
API Priority and Fairness (`flowcontrol.apiserver.k8s.io/v1`, GA) queues and
sheds by request class based on what the API server can actually take;
a client-side rate cannot know that. kubeagent's reads are all `get` and `list`,
so APF classifies them accordingly.

### When to set one anyway

- A shared cluster whose administrators have asked every client for a request
  budget.
- An API server without APF configured, where a client-side limit is the only
  brake available.
- Reproducing a support case that was captured under a specific rate.

Set `KUBEAGENT_QPS` alone and client-go applies its own default burst; set both
to control the bucket precisely.
```

- [ ] **Step 5: Add the nav entry to `website/mkdocs.yml`**

Under the `Features:` list, add an entry keeping the file's existing indentation and ordering convention:

```yaml
      - Performance tuning: features/tuning.md
```

- [ ] **Step 6: Build the site**

```bash
export PATH=$PATH:$HOME/.local/bin
cd website && mkdocs build --strict -f mkdocs.yml && cd ..
```

Expected: "Documentation built", exit 0, and no `WARNING` naming `features/tuning.md`. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 7: Add two entries to `docs/go-concepts.md`**

This file is **gitignored** — edit it on disk and never `git add` it. Append in the established style: a plain everyday example first, then the kubeagent example, and no Python comparisons. Match the heading style of §§1-20 and continue the numbering (the next free numbers after §22 "Reflection"), replacing the "## Coming later … nothing yet" placeholder.

The first entry, on the bounded worker pool that keeps its order:

```markdown
## 23. A worker pool that keeps its order

Say you send five letters and want five replies back. You could post one, wait
for the reply, post the next — five round trips, one after another. Or you post
all five at once and wait. The replies come back in whatever order the post
office manages, which is *not* the order you sent them.

If you then read the replies aloud in arrival order, you get a different story
every time you do it. If you put each reply into a numbered pigeonhole as it
arrives and read the pigeonholes in order afterwards, you get the same story
every time — even though the letters still arrived in a jumble.

That is the whole idea. In Go:

```go
out := make([]string, 5)          // five numbered pigeonholes

var wg sync.WaitGroup
for i := 0; i < 5; i++ {
    wg.Add(1)
    go func() {                   // Go 1.22 and later give each iteration its
        defer wg.Done()           // own i, so this closure captures the right one
        out[i] = send(i)          // each goroutine writes ONLY its own slot
    }()
}
wg.Wait()                         // wait for all five

for _, reply := range out {       // read in slot order, not arrival order
    fmt.Println(reply)
}
```

`sync.WaitGroup` is the waiting: `Add` says how many you are waiting for, `Done`
says one is finished, `Wait` blocks until the count reaches zero.

Note what is *not* here: no mutex. Each goroutine writes `out[i]` for its own
`i`, and no two goroutines share an `i`. Different elements of a slice are
different memory, so there is nothing to protect. A mutex would only be needed
if they all wrote to the same variable.

To run at most three at a time rather than all five, add a channel of indices
and start three goroutines that pull from it:

```go
next := make(chan int)
for w := 0; w < 3; w++ {
    go func() {
        for i := range next {     // ranges until next is closed
            out[i] = send(i)
        }
    }()
}
for i := 0; i < 5; i++ {
    next <- i
}
close(next)                       // tells the workers there is no more
```

**In kubeagent:** `internal/parallel.Do` is exactly this, with the pigeonholes
typed by the caller. `internal/scan.Evaluate` builds a list of read closures —
list the pods, list the services, probe this node's kubelet — hands it to `Do`,
and gets back one result per closure in list order. A slow API server changes
how long the scan takes and nothing else: the report comes out in the same order
every time, because the order is decided by the list, not by the network.

There is a second habit here worth copying. None of those closures records a
"blind spot" line itself; each just returns its own error. A single loop
afterwards walks a fixed order and records the lines. That is stronger than
locking a shared list — there is no shared list to lock.
```

The second entry, on generics:

```markdown
## 24. Generics — one function, many types

A vending machine that only sells crisps needs a whole second machine for
chocolate. A machine with a "whatever is in slot 4" mechanism sells both, and it
does not need to know what is in slot 4 to hand it over.

Before generics, a Go function that returned "the first item of a list" needed
one copy per type:

```go
func firstInt(xs []int) int          { return xs[0] }
func firstString(xs []string) string { return xs[0] }
```

With a type parameter, one function covers both:

```go
func first[T any](xs []T) T {
    return xs[0]
}

n := first([]int{4, 5, 6})          // T is int; n is 4
s := first([]string{"a", "b"})      // T is string; s is "a"
```

`[T any]` declares the type parameter. `any` is the constraint — "any type at
all". You rarely write `first[int](...)`: Go infers `T` from the argument.

The rule of thumb is that a type parameter is worth it when the function's logic
genuinely does not care about the type. `first` does not: it returns element
zero whatever that is. A function that added the items up *would* care, and
would need a constraint that admits only numbers.

**In kubeagent:** `internal/parallel.Do[T any]` runs a set of calls and returns
their results in index order. Whether those results are errors, structs or byte
slices is the caller's business — the pool's job is only to run them and keep
them in order. The scan happens to use `error`, but the ordering is what callers
depend on, and ordering has nothing to do with the type.
```

- [ ] **Step 8: Verify the docs and the working tree**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
git status --short
```

`git status --short` must NOT list `docs/go-concepts.md` (it is gitignored). It
must not list `internal/report/testdata/golden-scan.txt` either.

- [ ] **Step 9: Commit**

```bash
git add CLAUDE.md CHANGELOG.md website/docs/roadmap.md website/docs/features/tuning.md website/mkdocs.yml
git commit -s -m "docs: retire the sequential-scan invariant, document the tuning knobs

CLAUDE.md's 'v1 scan is sequential, no goroutines' invariant is replaced by
the property that actually holds now: determinism by construction, not by
avoiding concurrency. A new features/tuning.md documents
KUBEAGENT_SCAN_WORKERS, KUBEAGENT_QPS and KUBEAGENT_BURST, including the
2.42s-versus-0.15s measurement behind the limiter default and how the
worker cap multiplies under watch."
```

---

## Gate and merge

After task 7's review comes back clean, before the branch is finished:

- [ ] **Full chaos gate.** This branch touches `internal/cluster` and
  `internal/collect`, so it gets the full suite, not a smoke test.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY          # keep keys out of the shell
./chaos/run.sh --recreate        # long-running; run in the background and watch the log
```

Every scenario must be green with no `PARSE-FAILED`. Scenario 4 (NetworkPolicy
causality) is a known cold-cluster flake — re-run `--recreate` if it trips.

- [ ] **Measure the change on the chaos cluster**, so the numbers in the
  changelog and `tuning.md` are the numbers this branch actually produces:

```bash
go build -o kubeagent .
time ./kubeagent scan --context kind-kubeagent-chaos --kubelet-health --disk-usage \
  --dns-health --control-plane-health --certs --security > /dev/null
time KUBEAGENT_SCAN_WORKERS=1 KUBEAGENT_QPS=5 KUBEAGENT_BURST=10 ./kubeagent scan \
  --context kind-kubeagent-chaos --kubelet-health --disk-usage \
  --dns-health --control-plane-health --certs --security > /dev/null
```

Then confirm the two runs render identically:

```bash
./kubeagent scan --context kind-kubeagent-chaos --kubelet-health --disk-usage \
  --dns-health --control-plane-health --certs --security > /tmp/pool.txt
KUBEAGENT_SCAN_WORKERS=1 ./kubeagent scan --context kind-kubeagent-chaos \
  --kubelet-health --disk-usage --dns-health --control-plane-health --certs \
  --security > /tmp/serial.txt
diff /tmp/pool.txt /tmp/serial.txt
```

`diff` must be silent. If the measured numbers differ materially from 2.42s /
0.15s, update `CHANGELOG.md`, `website/docs/features/tuning.md` and
`website/docs/roadmap.md` to the measured values before merging — a documented
measurement that nobody re-measured is a claim, not a fact.

- [ ] **Whole-branch review on the most capable model**, then
  `superpowers:finishing-a-development-branch`.

---

## Self-review

**Spec coverage.** Walking the spec's sections against the tasks:

| Spec section | Task |
|---|---|
| `internal/parallel` (spec :72) | 1 |
| The worker cap (:98) | 4 |
| The rate limiter (:117) | 2 |
| Two phases inside `scan.Evaluate` (:145) | 5, with `CollectInventory` split out to task 3 |
| One `now` (:187) | 5 |
| Determinism / where order is observable / the mechanism (:195-227) | 5 (the structure), 6 (the proof) |
| The order itself (:228) | 5 — the 24-position table is reproduced in the task text |
| Error handling (:259), including the "all 27 reads issue before the fatal error" change | 5 |
| Testing (:290) | 1 and 6. **Deviation, deliberate:** the spec's reversal reactor is unimplementable — `k8stesting.Fake.Invokes` holds a write lock across the reactor body, measured at 403.9 ms for eight 50 ms concurrent reads, i.e. fully serial. The controlled inversion moved into `internal/parallel`'s own tests where the function under test is fully controlled, and a *real* inversion runs over HTTP against the phase-2 node fan-out. Task 6 states this in its preamble. |
| Documentation (:309) | 7 |
| What does not change (:326) | Enforced by the golden-file check in tasks 5 and 6 and the `diff` in the gate |
| Global constraints (:339) | The Global Constraints section |
| Risks (:363) | Task 6's `-race` runs, the CI change, and the gate's `diff` |

No spec requirement is unassigned.

**Placeholder scan.** Every code step carries the actual code. The two places
that say "match what the file already does" — the `mkdocs.yml` nav indentation
and the CI step name — are formatting conventions the implementer reads off the
file in front of them, not decisions deferred.

**Type consistency.** `runReads(ctx context.Context, reads []func(context.Context) error) []error` is
introduced in task 5 and keeps that exact signature in task 6, which is why task
6's product diff is four lines. `parallel.Do`'s signature in task 1 matches its
one call site in task 6. `scanWorkers() int` from task 4 is called from
`runReads`, not from `Evaluate`, so the two call sites in `Evaluate` never
change. The seven `collect` function names from task 3 are the names used in
task 5's phase-1 closures. `determinismFixture` and `slowKubeletHealthzServer`
are defined in task 6 and used only there.
