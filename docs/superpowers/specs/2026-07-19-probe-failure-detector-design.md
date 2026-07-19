# ProbeFailure detector — design

**Status:** approved · **Date:** 2026-07-19 · **Type:** new detector (v1 core)

## Goal

Diagnose the most common state kubeagent cannot currently explain: a pod that is
**Running but not Ready with no crash** — a container whose **readiness**,
**liveness**, or **startup** probe keeps failing. Today such a pod shows as a
`Degraded` workload with no named cause. A new `ProbeFailureDetector` names the
probe, the container, and a plain-language failure reason.

## Motivation

kubeagent's identity is "tells you *why*, not just that." The current detectors
(CrashLoopBackOff, ImagePullBackOff, OOMKilled, Pending, VolumeAttachError,
RestartLoop) all key off container *state* or scheduler messages. None of them
explain "`0/1 Ready`, `Running`, no restarts" — the classic failing-readiness-probe
case — nor do they name a probe as the cause of a liveness restart loop.

## Scope

**In:** readiness, liveness, and startup probe failures, read from the kubelet's
`Unhealthy` Warning events (read-only `List`, same pattern as `VolumeAttachDetector`).

**Complementary to the restart detectors** (the approved choice): a liveness/startup
probe that keeps failing *restarts* the container, so `RestartLoop`/`CrashLoopBackOff`
may already flag the pod (the *pattern*). `ProbeFailure` fires independently and names
the *cause* (the probe) — exactly like `CrashLoopBackOff` and `OOMKilled` both fire for
one container today. Double-flagging of a crash/pull pod is prevented structurally by
the guard (below), not by cross-detector coupling.

**Out of scope (YAGNI):**
- `ProbeError` events (the probe could not be executed) — rarer; only `Unhealthy`.
- Init-container probe failures — the guard naturally excludes them (see below); rare.
- Probe *configuration* critique (e.g. "initialDelaySeconds too low") — we report
  observed failures, not config advice.
- Any new CLI flag — the detector is **always-on** (no extra RBAC or cost, so nothing
  to gate behind an opt-in).
- Surfacing the raw probe message / pod IP / exec output anywhere — deliberately
  excluded for privacy (see §3).

## Global constraints

- **Read-only.** Lists `Unhealthy` events only. **No new RBAC** — events are already
  listed by `scan` (for `FailedAttachVolume`).
- **Core, always-on** detector — runs in both the CLI `scan` and the `watch` daemon
  (both call `scan.Evaluate`). It is read-only, so it needs no scan-only gating.
- **Privacy by construction:** no pod IP and no exec-probe command output ever enters
  a `Finding` field. `report.go` and `explain.go` are **unchanged**; the `--explain`
  privacy promise ("never sends pod IPs") holds without any redaction step.
- Detector is a **pure function** of `PodFacts` (`Pod` + `Events`); classifier is pure
  and deterministic (newest event by `LastTimestamp`; ordered substring reasons).
- **TDD**; detectors unit-tested with fake pods + events; `collect` via fake clientset.
- **No `Co-Authored-By: Claude` trailer** on any commit.

## Design

### 1. The detector — guard and flow

`internal/diagnose/probefailure.go`:

```go
type ProbeFailureDetector struct{}

func (d ProbeFailureDetector) Detect(facts PodFacts) *Finding {
    if podReady(facts.Pod) {          // reuse the existing helper in volumeattach.go
        return nil                    // a recovered/Ready pod is never flagged
    }
    ev := newestUnhealthyEvent(facts.Events)   // newest Reason=="Unhealthy", or nil
    if ev == nil {
        return nil
    }
    container := containerFromFieldPath(ev.InvolvedObject.FieldPath) // "spec.containers{web}" -> "web"
    // Overlap guard: the probe's container must be currently Running (not Waiting).
    // A CrashLoopBackOff / ImagePullBackOff / ContainerCreating container is Waiting,
    // so it is left to its own detector and never double-flagged here.
    if container != "" {
        if !containerRunning(facts.Pod, container) {
            return nil
        }
    } else if facts.Pod.Status.Phase != corev1.PodRunning {
        return nil                    // unparseable FieldPath -> pod-level fallback guard
    }
    probeType, reason := classifyProbe(ev.Message) // ("readiness","HTTP 503"); reason "" if unknown
    if probeType == "" {
        return nil                    // not a probe Unhealthy message; be conservative
    }
    return &Finding{
        Pod:       facts.Pod.Namespace + "/" + facts.Pod.Name,
        Issue:     "ProbeFailure",
        Reason:    probeReason(probeType),           // static, clean, per-probe (see §2)
        Evidence:  probeEvidence(container, probeType, reason), // clean, IP-free (see §3)
        Container: container,                          // structured; enables --logs bonus
    }
}
```

Helpers new to this file: `newestUnhealthyEvent`, `containerFromFieldPath`,
`containerRunning`, `classifyProbe`, `probeReason`, `probeEvidence`. `podReady` is the
existing helper in `volumeattach.go` (same package).

- `newestUnhealthyEvent(events)` — filter `e.Reason == "Unhealthy"`, return the newest
  by `LastTimestamp` (mirrors `newestAttachEvent`), or nil.
- `containerFromFieldPath(fp)` — extract the name inside braces:
  `spec.containers{web}` → `web`; `spec.initContainers{init}` → `init`; `""`/no braces → `""`.
- `containerRunning(pod, name)` — find the container in `pod.Status.ContainerStatuses`;
  return `cs.State.Running != nil`. (Init containers are in `InitContainerStatuses`, so
  an init-container probe's parsed name is not found here → `false` → no finding. That is
  the intended, safe exclusion of init-container probes.)

Why the guard works: readiness failure → container Running, pod not Ready → fires.
Liveness/startup restart → container Running again after the kill, pod not Ready → fires
(alongside `RestartLoop`). Crash/pull → container `Waiting` → skipped. Recovered pod →
`podReady` true → skipped.

### 2. Probe type → static Reason

`classifyProbe(message)` reads the probe type from the message prefix
(`"Readiness probe failed"` → `readiness`, `"Liveness probe failed"` → `liveness`,
`"Startup probe failed"` → `startup`; anything else → `""`).

`probeReason(probeType)` returns a fixed, clean, IP-free sentence per type — safe to
send to `--explain`:

| probe | Reason |
|---|---|
| readiness | `the readiness probe keeps failing — the pod is kept out of Service endpoints` |
| liveness | `the liveness probe keeps failing — the kubelet restarts the container` |
| startup | `the startup probe keeps failing — the container never finishes starting` |

### 3. Failure reason → clean, IP-free Evidence (the privacy core)

Probe event messages can carry a **pod IP**
(`Get "http://10.244.1.5:8080/healthz"…`) or, for `exec` probes, **arbitrary command
output** (possibly secrets). The detector therefore **never stores the raw message**.
`classifyProbe` derives a coarse `reason` by ordered, case-insensitive substring match
on the message tail — **first match wins**:

| order | substring in message | reason |
|---|---|---|
| 1 | `connection refused` | `connection refused` |
| 2 | `connection reset` | `connection reset` |
| 3 | `no route to host` / `network is unreachable` | `unreachable` |
| 4 | `no such host` / `server misbehaving` | `DNS lookup failed` |
| 5 | `context deadline exceeded` / `timeout` / `i/o timeout` / `Client.Timeout` | `timed out` |
| 6 | `statuscode:` | `HTTP <code>` (integer after `statuscode: `) |
| 7 | `NOT_SERVING` | `gRPC NOT_SERVING` |
| — | none of the above (incl. exec-probe output) | `""` (tail dropped) |

`probeEvidence(container, probeType, reason)` builds:
`container "<name>": <probeType> probe failed` and appends ` — <reason>` only when
`reason != ""`. Examples:

- `container "web": readiness probe failed — HTTP 503`
- `container "api": liveness probe failed — connection refused`
- `container "db": startup probe failed — timed out`
- `container "worker": liveness probe failed`  ← exec / unrecognized tail: dropped

**Privacy proof (by construction):** the container name comes from `FieldPath` (never
the message); `reason` is drawn only from the fixed table above; the raw message tail is
never copied when unrecognized. Therefore neither `Reason` nor `Evidence` can contain a
pod IP or exec output. `explain.go` (which sends `Issue`/`Reason`/`Evidence`) and
`report.go` need **no change**, and the `--explain` privacy note stays accurate.

### 4. Wiring — `internal/collect` and `scan.Evaluate`

New collector, mirroring `VolumeAttachEvents`:

```go
func UnhealthyEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error) {
    events, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=Unhealthy"})
    if err != nil {
        return nil, fmt.Errorf("listing probe (Unhealthy) events: %w", err)
    }
    return events.Items, nil
}
```

In `scan.Evaluate`, fetch it (non-fatal, like the attach events) and merge before
`FactsFrom`, then register the detector:

```go
attachEvents, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)
unhealthyEvents, _ := collect.UnhealthyEvents(ctx, client, opts.Namespace)
events := append(attachEvents, unhealthyEvents...)
findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, events))
```

```go
detectors := []diagnose.Detector{
    diagnose.CrashLoopDetector{}, diagnose.ImagePullDetector{}, diagnose.OOMKilledDetector{},
    diagnose.PendingDetector{}, diagnose.VolumeAttachDetector{},
    diagnose.RestartLoopDetector{Now: time.Now()},
    diagnose.ProbeFailureDetector{},   // after RestartLoop: pattern then cause in output
}
```

`FactsFrom` is unchanged (it already buckets any `Pod`-involved events by pod).

### 5. What does *not* change

- **`report.go`** — the finding renders through the existing generic block
  (`⚠ <Issue>: <Reason>` then `↳ <Evidence>`). No new field, no format change.
- **`explain.go`** — unchanged; `Reason`/`Evidence` are clean by construction.
- **`watch` daemon / `watch.Config`** — unchanged; the daemon inherits the detector via
  the shared `scan.Evaluate` and stays read-only.
- **RBAC / deploy manifests / Helm** — unchanged (no new permission).
- **JSON schema** — unchanged; `"issue": "ProbeFailure"` is a new value of an existing
  field; `container` is an existing field.

### 6. `--logs` interaction (no work required)

`ProbeFailure` sets `Container`, so `scan --logs` will opportunistically enrich it. For a
liveness/startup restart, `collect.PreviousLogs` returns the killed instance's logs (a
useful bonus root cause); for a readiness failure with no prior instance it returns
`("", false)` (non-fatal, no block). The M4 per-container de-dup already prevents a double
log block when `RestartLoop` and `ProbeFailure` both fire for one container. No new code.

### 7. Output example

```text
✗ shop/web  Deployment  0/1 Degraded
    ⚠ ProbeFailure: the readiness probe keeps failing — the pod is kept out of Service endpoints
      ↳ container "web": readiness probe failed — HTTP 503
    web-5b8-8fskg  0/1  Running  restarts=0  worker-1  10.244.2.2  2m
```

Complementary (liveness restart loop — both findings, pattern then cause):

```text
✗ shop/api  Deployment  0/1 Degraded  · 5 restarts, last 20s ago
    ⚠ RestartLoop: container keeps exiting and restarting
      ↳ container "api", restartCount=5
    ⚠ ProbeFailure: the liveness probe keeps failing — the kubelet restarts the container
      ↳ container "api": liveness probe failed — connection refused
```

## Error handling

- `UnhealthyEvents` List error → ignored in `Evaluate` (non-fatal, mirrors
  `VolumeAttachEvents`); the detector simply sees no events and returns nil.
- No `Unhealthy` event for a pod → no finding.
- Unparseable / empty `FieldPath` → pod-level fallback guard (`Phase == Running`).
- Message without a known probe prefix → no finding (conservative).

## Testing

TDD, detector-level and integration:

- **`probefailure_test.go`** (fake pods + events, no cluster):
  - readiness HTTP 503 on a Running-but-not-Ready pod → `ProbeFailure`, Evidence
    `container "web": readiness probe failed — HTTP 503`, Container `web`.
  - liveness `connection refused` → reason `connection refused`, Reason mentions restart.
  - startup `context deadline exceeded` → reason `timed out`.
  - exec probe (unrecognized tail) → Evidence has **no** ` — ` suffix (tail dropped);
    assert the raw tail text is absent (privacy).
  - **overlap guard:** a `CrashLoopBackOff` container (Waiting) with a stale `Unhealthy`
    event → **no** `ProbeFailure` finding.
  - a `Ready` pod with an old `Unhealthy` event → no finding.
  - `containerFromFieldPath` unit cases: `spec.containers{web}`→`web`, init/empty/no-brace.
  - a message containing a pod IP → assert the IP substring is **absent** from both
    `Reason` and `Evidence` (privacy regression guard).
- **`collect` test** — `UnhealthyEvents` via fake clientset returns the seeded event;
  field-selector path exercised.
- **`scan` integration test** — `Evaluate` on a fake clientset with a readiness-failing
  pod + its `Unhealthy` event yields a `ProbeFailure` finding on the workload; a healthy
  pod yields none.
- **Golden** — add a readiness `ProbeFailure` finding to the golden fixture and
  regenerate `testdata/golden-scan.txt` with `-update`. This adds sample lines but does
  **not** change the report *format*, so it does not itself require a demo-GIF rebuild;
  the README GIF / quickstart example output are refreshed per the standard
  golden-change protocol (CLAUDE.md) at release time, as with prior detectors.

## Files touched

- **Create:** `internal/diagnose/probefailure.go`, `internal/diagnose/probefailure_test.go`
- **Modify:** `internal/collect/collect.go` (+ `collect_test.go`) — `UnhealthyEvents`
- **Modify:** `internal/scan/scan.go` (+ `scan_test.go`) — fetch/merge events, register detector
- **Modify:** `internal/report/golden_test.go` + `testdata/golden-scan.txt` — fixture + snapshot
- **Docs:** `website/docs/features/diagnostics.md` (new subsection), `CHANGELOG.md`
  (`### Added`), `website/docs/quickstart.md` (mention in the failure-mode list), README
  bullet.

## Non-goals recap

`ProbeError` events; init-container probes; probe-config critique; a CLI flag; and any
surfacing of the raw probe message, pod IP, or exec output.
