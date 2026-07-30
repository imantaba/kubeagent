# Bounded scan concurrency — design

Theme H sub-project 4 of 7 on the road to the v1.0 production contract.
Retires the deliberate v1 simplification recorded in `CLAUDE.md`:

> v1 CLI (`scan`) is **sequential** — no goroutines.

## The problem, measured

The sub-project was scoped as "the scan is slow because it is sequential."
It is not. It is slow because kubeagent never configures the client-side
rate limiter, so client-go applies its legacy default.

`internal/cluster/client.go` builds every clientset kubeagent uses and never
sets `QPS` or `Burst`. With both zero, `rest.RESTClientFor` falls back to
`DefaultQPS = 5.0` and `DefaultBurst = 10` and installs a token bucket **per
API-group client**. CoreV1 carries nearly every read a scan makes — pods, four
event lists, services, PVCs, PVs, namespaces, resourcequotas, secrets, nodes,
and every `nodes/proxy`, `pods/proxy` and `pods/log` round trip — so CoreV1
alone blows past a burst of 10 on any cluster.

Measured against the three-node chaos Kind cluster, the same binary twice, the
second built with `config.QPS = -1` and nothing else changed:

| scan | with the default limiter | limiter disabled |
|---|---|---|
| `scan` | 0.43 s | — |
| `scan --kubelet-health --disk-usage --dns-health --control-plane-health --certs --security` | **2.42 s** | **0.15 s** |

Rendered output was byte-identical between the two binaries. The speed-up is
**16×, with no goroutines at all.**

The arithmetic confirms the mechanism rather than merely correlating with it:
the 2.27 s delta divided by 5 QPS is ~11 requests past the burst, so ~21
CoreV1 requests — which is exactly what that flag combination issues on a
three-node cluster.

The consequence for the design is the important part. **Bounded concurrency
layered on top of a 5 QPS bucket buys almost nothing** — eight workers still
drain one token bucket at five per second. Concurrency only pays once the
throttle is gone, and then it pays on the axis the limiter never touched: the
per-node fan-out (`NodeStats` + `KubeletHealthz`, two round trips per node,
sequential today) and the ~27 independent list calls, one round trip each.
On a 200-node cluster at 30 ms round trip that fan-out is 12 s of wall clock
that eight workers cut to about 1.5 s.

Both changes ship together. Either alone would be half a fix.

## Goals

- Remove the accidental rate ceiling and replace it with a deliberate
  concurrency ceiling.
- Run the scan's independent reads under a fixed worker cap.
- Change **no rendered byte** for a given cluster state.
- Prove the output does not depend on completion order, by test.

## Non-goals

- Retries, backoff, or any transfer-level byte cap. Out of scope.
- `main.go`'s post-`Evaluate` reads (`NodeMetrics`, `ClusterPods`,
  `StorageClasses`, `IngressClasses`, `SystemDaemonSets`) and
  `advisory.Assess`. Sub-project 5 rewrites that CLI onto Cobra; parallelizing
  it now would be churn against churn. Five round trips left on the table,
  named here so the omission is a decision and not an oversight.
- Concurrency anywhere in the `watch` daemon's own structure. It already runs
  a goroutine per cluster; this work changes what happens *inside* one of
  those goroutines.
- Any change to the six versioned JSON documents.

## Architecture

### `internal/parallel` — the whole concurrency primitive

One new package, one exported function, stdlib only:

```go
// Do runs fn(ctx, i) for every i in [0,n) with at most workers running at
// once, and returns the results in INDEX order — never completion order.
func Do[T any](ctx context.Context, workers, n int, fn func(context.Context, int) T) []T
```

Workers write `out[i]` at distinct indices, so nothing is shared and no mutex
is involved. `workers` is clamped to at least 1 and at most `n`.

**No early exit on cancellation.** `Do` dispatches every index even when `ctx`
is already done, and each `fn` observes `ctx` itself and returns its own error.
The alternative — stop dispatching and leave the tail of `out` zero-valued —
produces a `(result, nil-error)` pair that is indistinguishable from a read
that succeeded and found nothing. In a tool whose whole contract is
"a blind spot is named, never silently empty," that is the one failure mode
worth designing against. This is documented on the function.

The package imports nothing outside the standard library, so it is importable
by every surface package including `internal/mcp`, `internal/gate`,
`internal/tui` and `internal/rbacprofile`, which may not reach
`internal/remediate` or `internal/explain`.

### The worker cap

`defaultScanWorkers = 8`, overridable by `KUBEAGENT_SCAN_WORKERS`. A value that
does not parse as an integer falls back to 8; a value that parses but lies
outside `1..64` is clamped to the nearer bound. Neither case errors — a scan
should not fail because of a tuning knob.

The cap is **per `Evaluate` call, not process-global.** `internal/watch`
already runs one goroutine per watched cluster, each calling `scan.Evaluate`,
so a five-cluster watch reaches 40 in-flight reads — eight against each of
five different API servers. Per-call is the correct unit because the resource
being protected is one API server, and the docs state the multiplication
explicitly.

Eight is a modest number of simultaneous reads for any API server, and it is
the escape hatch as well: an operator scanning a struggling control plane can
set `KUBEAGENT_SCAN_WORKERS=1` and get exactly today's behaviour, minus the
rate limiter.

### The rate limiter

`internal/cluster.restConfig` sets `config.QPS = -1` — client-go's documented
idiom for "install no client-side limiter" — unless `KUBEAGENT_QPS` and
`KUBEAGENT_BURST` are set, in which case those are honoured. Setting
`KUBEAGENT_QPS` without `KUBEAGENT_BURST` uses client-go's `DefaultBurst`.

This follows current upstream practice. `sigs.k8s.io/controller-runtime`
v0.24.1 defaults the same way, with the comment:

> Disable client-side ratelimiter by default, we can rely on API priority and
> fairness

API Priority and Fairness is GA (`flowcontrol.apiserver.k8s.io/v1`) and does
the shedding server-side, where the server knows its own load. A client-side
token bucket does not: it holds the same rate whether the API server is idle
or dying. A worker cap is self-limiting in a way a token bucket is not — a
slow server slows the workers, which lowers the request rate automatically.

The ceiling therefore moves from *rate* to *concurrency*, and a scan's total
request count is bounded anyway (~27 lists plus two per node plus one per
crash-looping container), so this is a bounded burst, not sustained load.

**Blast radius:** `restConfig` is the single place kubeconfig resolution
lives, so this affects `scan`, `watch`, `mcp`, `gate`, `tui`, `rbac` and
`--fix` alike. That, plus the changes in `internal/collect`, is why this
sub-project takes the **full chaos gate**.

### Two phases inside `scan.Evaluate`

**Phase 1 — every read that depends on nothing.** 27 with all add-ons on:

- the seven inventory lists (pods, deployments, replicasets, statefulsets,
  daemonsets, jobs, cronjobs)
- nodes, leases
- four event lists (volume-attach, unhealthy, PVC, FailedCreate)
- services, endpointslices, ingresses
- PVCs, PVs, storageclasses, namespaces
- PDBs, HPAs, resourcequotas, networkpolicies
- validating and mutating webhook configurations (only when the scan is
  cluster-wide, as today)
- TLS secrets (only under `--certs`, as today)

`collect.CollectInventory` splits into seven small exported functions in the
exact style of the existing `Services`, `Ingresses` and `NetworkPolicies`
helpers, each keeping its current `fmt.Errorf("listing pods: %w", err)`
wrapping so `connectivity.Diagnose` still recognizes the fatal cases.
`CollectInventory` stays as a thin sequential composition of the seven; only
`scan.Evaluate` and `internal/collect`'s own tests call it today, and
`Evaluate` switches to the seven so its lists join one flat pool instead of
nesting a pool inside a pool.

Between the phases, `diagnose.Run` — pure, in-memory, untouched.

**Phase 2 — the fan-outs, one flat pool.** `NodeStats` per node under
`--disk-usage`, `KubeletHealthz` per node under `--kubelet-health`,
`CoreDNSMetrics` per CoreDNS pod under `--dns-health`, `PreviousLogs` per
finding-bearing container under `--logs`, and the single `ControlPlaneReadyz`
under `--control-plane-health`. Every one depends only on phase 1's results,
and none on another.

The `--logs` fan-out keeps today's one-fetch-per-`pod/container` dedupe (a
container tripping both CrashLoop and OOM is enriched once). That dedupe now
runs **before** dispatch, walking `findings` in order to build the index list,
so which containers are fetched — and which finding each result is written
back to — is decided sequentially and cannot depend on the schedule.

Every `Assess` and `Annotate` call stays exactly where it is, sequential and
pure. Detectors remain pure functions; this work does not touch them.

### One `now`

`Evaluate` calls `time.Now()` five separate times today (scan.go lines 195,
239, 260, 284, 345). Under parallel execution the sections those instants feed
can disagree about what time it is. One `now := time.Now()` at the top,
threaded to all five, makes the whole evaluation coherent for one instant.
It changes no output shape and no test.

## Determinism

This is the whole risk of the sub-project, so the design makes ordering a
property of the source text rather than of the scheduler.

### Where order is observable

`scan.Result.PartialReads` is rendered **verbatim, unsorted**, in five places:

- text — `report.printBlindSpots` iterates the slice and never sorts
- `scan --output json` — the `blindSpots` array
- `gate --output json` and SARIF — the `inconclusive` array
- `scan --output html`
- the TUI

Today that order is the order `Evaluate` happens to call `note()` and
`blind()`. Under a naive port to goroutines it would become completion order,
and the bytes would change from run to run.

### The mechanism

**`note()` and `blind()` leave the concurrent region entirely.**

Each read is a closure that writes only its own typed destination variable and
returns only its own error. Nothing concurrent touches `partialReads` or
`blindSeen`. After a phase completes, a single sequential loop walks a
declared `reportOrder` list and calls `note`/`blind` in that order.

Three things follow. The rendered order is a list in the source, not a race.
`blindSeen`'s first-wins dedupe needs no lock, because it only ever runs on
one goroutine. And the concurrent region has no shared mutable state at all,
which is a stronger claim than "the shared state is locked."

### The order itself

`reportOrder` reproduces today's call order exactly:

```text
events (volume-attach), events (unhealthy), pods/log, leases, services,
endpointslices, ingresses, secrets, persistentvolumeclaims, namespaces,
poddisruptionbudgets, horizontalpodautoscalers,
validatingwebhookconfigurations, mutatingwebhookconfigurations,
persistentvolumes, events (PVC), storageclasses, resourcequotas,
networkpolicies, events (FailedCreate), nodes/proxy (disk usage),
nodes/proxy (kubelet health), /readyz, pods/proxy
```

`pods/log` sits third because the `--logs` enrichment loop sits early in
today's `Evaluate`, not because anyone chose that position. Sorting the list
alphabetically was the alternative and is the better contract in the
abstract; it is rejected because it changes rendered bytes on any cluster with
more than one blind spot, and byte-identity is the constraint this
sub-project accepted.

One existing behaviour is preserved deliberately rather than fixed: the four
event lists all report under the resource name `events`, so a transport error
affecting all four appends four identical lines today (only *forbidden* reads
dedupe, via `blind`). Changing that would change bytes. It is noted here so a
reviewer reads it as observed and kept, not as missed.

`golden-scan.txt` cannot catch a regression here — it has no BLIND SPOTS
section — so the order is pinned by a unit test and checked live by chaos
scenario 20, which produces three blind spots.

## Error handling

Unchanged in behaviour, only in where it happens.

- The fatal reads stay fatal: a failed pods, deployments, replicasets,
  statefulsets, daemonsets, jobs, cronjobs or nodes list still aborts
  `Evaluate` with the same wrapped error. Because phase 1 runs them all, the
  sequential consumption loop checks the fatal errors first, **in declaration
  order**, so the error a caller sees for a cluster that fails several lists
  is the same one it sees today.
- One behaviour genuinely does change, and it is worth stating plainly: today
  a failed pods list returns from `Evaluate` immediately and the remaining
  reads never run, whereas under phase 1 **all 27 reads are issued before the
  fatal error is discovered**. Nothing about that is unsafe — every one of
  them is a `get`/`list`, the scan is read-only, and a cluster whose pods list
  fails is usually one where the other lists fail too — but a caller against
  a wholly unreachable API server now pays one pool's worth of failing
  requests instead of one. The alternative, cancelling the pool on the first
  fatal error, would make *which* reads completed depend on the schedule, and
  therefore make `partialReads` non-deterministic. Determinism wins; the cost
  is a handful of failing requests on a cluster that is already broken.
- Every other read still degrades: `note()` records a `ReadFailure`, and a
  forbidden or unauthorized one becomes a blind spot phrased in kubeagent's
  own words via `blindReason`, never the API server's message.
- The non-erroring probes (`KubeletHealthz`, `CoreDNSMetrics`,
  `ControlPlaneReadyz`) keep their current contract of returning a classified
  status rather than an error.
- A panic in one read would take down the process, as it does today. The fuzz
  suite from sub-project 3 already asserts that no cluster object can panic a
  detector; nothing here widens that surface.

## Testing

| Test | What it proves |
|---|---|
| Reversal reactor | A fake-clientset reactor whose delay is inversely proportional to dispatch index, so completion order is exactly **reversed**. The result must deep-equal a sequential run. Deterministic, fast, and maximally adversarial — the worst ordering becomes the default ordering instead of something a random schedule might reach. |
| Randomized-delay property test | `Evaluate` run N times against randomized per-read delays; every result deep-equal. Catches orderings the reversal case does not reach. |
| `workers=1` vs `workers=8` | Identical results from the two extremes of the knob. This is what the env override buys as a test lever. |
| Blind-spot order test | A fake client refusing several reads, asserting the exact rendered order against `reportOrder`. |
| `internal/parallel` unit tests | Index-order preservation, `n=0`, `workers > n`, `workers < 1`, and that every index is answered even when `ctx` is already cancelled. |
| `-race` added to CI's `go test` step | A data race fails the build rather than flaking one run in fifty. |
| `golden-scan.txt` unchanged | Rendered bytes did not move. |
| Full chaos gate | 20 scenarios against a live Kind cluster; scenario 20's three blind spots check ordering live, and the `--kubelet-health`, `--disk-usage` and `--dns-health` scenarios exercise both fan-outs. |

TDD throughout: each property is watched failing against the pre-change code
before its implementation lands.

`go test` runs with `-p 2` as always, never `-short`. `-race` composes with
both.

## Documentation

- **`CLAUDE.md`** — the "v1 CLI (`scan`) is **sequential** — no goroutines"
  invariant is now false and is rewritten: the scan collects under a bounded
  worker pool, detectors stay pure, results are index-ordered, and rendered
  order is declared rather than observed.
- **`docs/go-concepts.md`** — two entries, because they are two concepts and
  §19 covers only mutexes: a bounded worker pool that keeps its order
  (goroutines, channels, `sync.WaitGroup`, and why writing distinct slice
  indices needs no mutex), and generics. Plain everyday example first, then
  the kubeagent example, no Python comparisons.
- **`CHANGELOG.md`** — the limiter change is user-visible and belongs under
  Changed with its measured numbers; the two new env knobs under Added.
- **`website/docs/roadmap.md`** — Theme H bullet.
- Tuning docs for `KUBEAGENT_SCAN_WORKERS`, `KUBEAGENT_QPS` and
  `KUBEAGENT_BURST`, including the per-cluster multiplication under `watch`.

## What does not change

- No JSON shape moves, so **no `schemaVersion` bump** in `internal/jsonschema`
  and no regeneration under `website/docs/schemas/`.
- No new verb or resource, so `internal/rbacprofile`'s `Feature` table, the
  generated RBAC manifests and the chart ClusterRole are untouched.
- No new third-party dependency; `go.mod` and `go.sum` are not edited.
  `internal/parallel` is stdlib only, which is why `golang.org/x/sync/errgroup`
  is not used.
- Standard-library `flag` only — the two knobs are environment variables in
  the established `KUBEAGENT_*` style, not new flags. Cobra is sub-project 5.
- Read-only toward the cluster: `get`/`list` only, no writes, no LLM call.

## Global constraints

Carried verbatim into the implementation plan:

- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`); `main` enforces DCO.
- No `Co-Authored-By` trailer and no AI attribution anywhere — commits, PR
  bodies, code, comments, docs, changelog.
- Detectors stay pure functions.
- Standard-library `flag` only; no Cobra.
- No new third-party dependency.
- No secrets, credentials, private IPs or internal hostnames anywhere,
  including tests and fixtures. Documentation IPs are RFC 5737; example
  domains are RFC 2606.
- URLs are credentials: no log line, error, metric label or rendered document
  carries more than `scheme://host`.
- Kubeconfig paths are credentials.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- `go test` runs with `-p 2`, never `-short`.
- `internal/parallel` must never import `internal/remediate` or
  `internal/explain`.
- Never implement on `main`; this work lands on the
  `bounded-scan-concurrency` branch.

## Risks

**A shared variable slips into a read closure.** The reversal reactor and
`-race` are the net; the design's answer is that closures write only their own
destination, which a reviewer can check by reading one list.

**The limiter change surprises a large cluster.** A scan of a 500-node cluster
with `--kubelet-health --disk-usage` issues ~1000 proxied reads that were
previously spread over minutes by the token bucket and will now arrive eight
at a time. APF is the server-side answer and `KUBEAGENT_SCAN_WORKERS=1` the
client-side one; both are documented.

**Blind-spot ordering regresses silently.** The golden file cannot see it.
Mitigated by the dedicated order test and by chaos scenario 20.
