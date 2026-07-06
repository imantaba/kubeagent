# kubeagent — Design: volume-attach (Multi-Attach) detector

**Status:** approved design (pre-implementation)
**Date:** 2026-07-06

## Goal

Detect pods that are stuck because a volume cannot be attached — most commonly a
**Multi-Attach error** (a ReadWriteOnce volume still attached to another node),
but also CSI attach timeouts, volume-not-found, and similar. Today none of the
four detectors catch this: the pod is scheduled (so `Unschedulable` never fires),
its container sits in `ContainerCreating` (not a crash/imagepull reason), and the
signal lives in a `FailedAttachVolume` **Event** — which the detectors never read.
Result: the pod shows as a degraded workload with **no explanatory finding**.

## Motivation (live test)

On the `hetzner-nova` cluster, a Multi-Attach pod would raise
`kubeagent_workloads_flagged` but produce no finding naming the cause. This
detector closes that gap.

## Decision (from brainstorming)

- **Match any `FailedAttachVolume` event** on a scheduled-but-not-ready pod — the
  whole "volume can't attach" family. Issue = `VolumeAttachError`; the finding
  names Multi-Attach specifically when the event message says so.
- **Read events via the existing `PodFacts.Events` field** (present for exactly
  this, currently unpopulated) and a new detector — not an informer, not an
  annotator.
- **The daemon does not watch events** (they are high-churn); events are List-ed
  cheaply per `scan.Evaluate` reconcile, which the pod-informer + heartbeat
  already trigger.

## Invariants / constraints (unchanged)

- **READ-ONLY.** One extra `List` of events (field-selected). No writes, no LLM.
- Detectors stay pure `Detect(PodFacts) *Finding`.
- No new Go module dependency (`corev1.Event`, field selectors already available).
- The daemon's trigger model is unchanged (no events informer).

## Component 1 — collect the events (`internal/collect`)

```go
// VolumeAttachEvents lists FailedAttachVolume Warning events in the namespace
// (empty = all), read-only. Attach failures are rare, so the field-selected List
// is cheap.
func VolumeAttachEvents(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Event, error)
```

Implemented with `client.CoreV1().Events(namespace).List(ctx,
metav1.ListOptions{FieldSelector: "reason=FailedAttachVolume"})`.

## Component 2 — correlate (`FactsFrom`)

Change the signature so facts carry the pod's relevant events:

```go
func FactsFrom(pods []corev1.Pod, events []corev1.Event) []diagnose.PodFacts
```

Build a `map["namespace/name"][]corev1.Event` from each event's
`InvolvedObject` (Kind `Pod`, matching namespace + name) and set
`PodFacts.Events` for the matching pod. Pods with no matching events get an empty
slice (unchanged behavior for every existing detector, which reads `Pod` only).

The single production caller is `scan.Evaluate`; tests are updated to pass the
new argument (`nil` where events are irrelevant).

## Component 3 — the detector (`internal/diagnose/volumeattach.go`)

```go
type VolumeAttachDetector struct{}
func (d VolumeAttachDetector) Detect(facts PodFacts) *Finding
```

Fires when **all**:
1. the pod is **not Ready** (no `PodReady` condition with status `True`); and
2. the pod is **still stuck at container creation** — a container is `Waiting`
   with reason `ContainerCreating`, or the pod has no container statuses yet. This
   excludes a pod whose volume eventually attached and is now `Running`,
   `CrashLoopBackOff`, or `ImagePullBackOff` (those have different waiting reasons
   / have progressed past volume setup), so a *stale* attach event within its 1h
   TTL cannot cause a false positive; and
3. `facts.Events` contains a `FailedAttachVolume` event for this pod.

Emits:
- `Issue: "VolumeAttachError"`
- `Reason:` when the newest matching event's message contains `"Multi-Attach"`,
  `"the volume is attached to another node (Multi-Attach) — the pod cannot mount
  it"`; otherwise `"a volume cannot be attached to the pod's node"`.
- `Evidence:` the newest matching event's `Message` (e.g. `Multi-Attach error for
  volume "pvc-…" Volume is already exclusively attached to one node…`).

Pure: it reads only `facts.Pod` status and `facts.Events`.

## Component 4 — wire into `scan.Evaluate` (`internal/scan`)

- Collect events best-effort (like the other enrichments):
  `events, _ := collect.VolumeAttachEvents(ctx, client, opts.Namespace)`.
- Pass them to `FactsFrom(inputs.Pods, events)`.
- Add `diagnose.VolumeAttachDetector{}` to the detector slice.

Both the CLI `scan` and the `watch` daemon inherit it. The finding then flows —
with no further changes — to the text report, JSON, `--explain`, and the daemon's
`kubeagent_findings{issue="VolumeAttachError"}` metric (all of which iterate
findings by issue).

## Component 5 — daemon RBAC (`deploy/rbac.yaml`)

Add `events` (core group) with `get`/`list`/`watch` to the read-only ClusterRole.
Without it the daemon's events List is forbidden and the detector silently
no-ops. The RBAC remains strictly read-only (no write verbs).

## Testing (TDD)

- **Detector** (`volumeattach_test.go`, fake pods + events):
  - not-Ready pod + a `FailedAttachVolume` event with a Multi-Attach message →
    finding with the Multi-Attach reason and the message as evidence.
  - not-Ready pod + a generic `FailedAttachVolume` (non-Multi-Attach) message →
    finding with the generic reason.
  - Ready/Running pod (even with a stale attach event) → nil.
  - not-Ready pod with an event for a *different* pod → nil (no false correlation).
  - no events → nil.
- **`FactsFrom` correlation** (`collect`): an event whose `InvolvedObject` names
  pod A attaches to A's facts, not B's; a non-Pod event is ignored.
- **`scan.Evaluate` integration** (fake clientset): a stuck pod + a
  `FailedAttachVolume` event → the result's workload carries a `VolumeAttachError`
  finding. Existing behavior for healthy clusters is unchanged.
- **Live validation:** the real Multi-Attach repro on `hetzner-nova` (a small RWO
  PVC forced onto two nodes) → the deployed daemon emits
  `kubeagent_findings{issue="VolumeAttachError"}` and the scan shows the finding.

## Out of scope (explicit non-goals)

- `FailedMount` / post-attach mount failures (noisier; ConfigMap/Secret mount
  races self-heal) — the scope is attach failures only.
- Inspecting `VolumeAttachment` objects or per-pod event lookups beyond the one
  field-selected List.
- Any remediation (`--fix`) for volume issues.
