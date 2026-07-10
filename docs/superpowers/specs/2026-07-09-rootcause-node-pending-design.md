# Root-Cause for Pending & Node NotReady — Design

**Date:** 2026-07-09
**Status:** Approved

## Goal

Turn two "symptom only" scan outputs into root-cause answers:

- **Node NotReady** currently prints only `NotReady`; surface *why* (the
  `NodeReady` condition's Reason + Message, e.g. `KubeletNotReady — container
  runtime network not ready: cni config uninitialized`).
- **Pending / other findings** already capture the exact signal in
  `Finding.Evidence` (for a pending pod, the scheduler's verbatim
  `0/5 nodes are available: 3 Insufficient memory, …`), but the **text** scan
  prints only `Issue: Reason` — the Evidence reaches JSON and `--explain` but not
  the plain text output. Show it.

Both are read-only, need **no new API call and no new RBAC** (nodes are already
collected; `Finding.Evidence` already exists). Advisory — the cluster verdict
logic is unchanged; only the messages get richer.

## A. Node NotReady root cause

`internal/clusterhealth/clusterhealth.go`'s `nodeHealth` reads the `NodeReady`
condition's `Status` only. Change it to also read that condition's `Reason` and
`Message` and, when the node is NotReady, include them in the issue string.

- New issue string for the NotReady case: `NotReady: <Reason> — <Message>`, where
  `<Message>` is trimmed to its first line and truncated to 120 characters
  (append `…` when truncated) so the header stays one line. When Reason/Message
  are empty, fall back to plain `NotReady`.
- The `ClusterHealth.NodeIssues []string` shape is unchanged — the enriched
  string flows through the text header (`✗ node <iss>`) and JSON `nodeIssues`
  automatically.
- Pressure conditions (`MemoryPressure`/`DiskPressure`/`PIDPressure`) and
  `SchedulingDisabled` keep their current short labels — self-describing, no
  change.
- Verdict logic unchanged: a NotReady node still makes the cluster `Degraded`
  exactly as before.

Helper: a small `trimLine(s string, max int) string` (first line, rune-safe
truncation with an ellipsis) in the clusterhealth package.

## B. Show Evidence in the text report

`internal/report/report.go`'s `printWorkload` renders each finding as
`    ⚠ <Issue>: <Reason>`. Add an indented Evidence line beneath it when the
finding's `Evidence` is non-empty and not equal to `Reason`:

```
    ⚠ Unschedulable: No node can schedule this pod (resources, taints, or affinity)
      ↳ 0/5 nodes are available: 3 Insufficient memory, 2 node(s) had untolerated taint
```

- Guard: `if f.Evidence != "" && f.Evidence != f.Reason`.
- Applies to every detector's finding (CrashLoopBackOff, OOMKilled,
  VolumeAttachError, RestartLoop, Unschedulable…), so the concrete signal shows
  in text, matching what JSON and `--explain` already receive.
- JSON output is unchanged (Evidence was already serialized); `--explain` is
  unchanged.

## Data flow

No collection changes. `clusterhealth.Assess` produces richer `NodeIssues`
strings; `report.printWorkload` renders the already-present `Finding.Evidence`.
Both flow through existing text/JSON paths.

## Testing

- **clusterhealth** (`clusterhealth_test.go`): a NotReady node whose `NodeReady`
  condition has `Reason=KubeletNotReady`, `Message=…` → the issue string contains
  `NotReady: KubeletNotReady — …`; a long message is trimmed to one line ≤120
  chars with `…`; a Ready node produces no NotReady issue; empty Reason/Message
  falls back to plain `NotReady`. The existing fake-node test helper takes
  conditions, so extend it (or add a variant) to set Reason/Message on the
  `NodeReady` condition.
- **report** (`report_test.go`): a workload finding with `Evidence` different from
  `Reason` renders the `↳ <evidence>` line; a finding with empty Evidence, or
  Evidence equal to Reason, renders no extra line.

## Scope boundaries

- **No node Warning events** (`Rebooted`, `ContainerGCFailed`, …) — the
  `NodeReady` condition message is the direct cause and needs no extra API call.
  Deferred as a possible follow-up.
- Verdict, exit code, JSON schema, and `--explain` behavior unchanged.
- No new detector, no `--fix`, no new dependency, no RBAC change.

## Docs

- `CHANGELOG.md` `[Unreleased]`.
- `website/docs/features/diagnostics.md`: node NotReady now names the cause; text
  findings now show the underlying signal.
