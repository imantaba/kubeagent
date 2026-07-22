# Deterministic next-step suggestions (`--suggest`) — design

**Status:** approved · **Date:** 2026-07-22 · **Type:** new feature (Theme C, slice 1)

## Problem

Today a finding says *what* is wrong (`CrashLoopBackOff`) and *why*
(Reason/Evidence), and for a couple of cases `--fix` can act. But for the many
pod-level failures that aren't auto-fixable, kubeagent offers no "here's what to
do / what to check next." This adds a **deterministic, reviewed** (never
LLM-decided) next-step suggestion per finding — a concise cause-direction plus a
read-only investigation command — behind an opt-in `--suggest` flag. It works
**offline** (no API key) and stays read-only: kubeagent *prints* a command; it
never runs it. This is the deterministic core that a later Theme-C slice will
hand to `--explain` for LLM ranking/phrasing (the LLM ranks; it never invents the
remediation).

## Behavior (approved)

With `--suggest`, each pod finding gains two lines under its existing output:

```text
✗ shop/web  Deployment  0/2 Degraded
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
      ↳ container "web": restartCount=8
      ↳ next step: starts then crashes — inspect the crash output
      ↳ try: kubectl -n shop logs web-abc -c web --previous
```

Without `--suggest` (the default), output is **unchanged** (zero golden churn).

## Design

### 1. `internal/remediation` — the deterministic mapping (pure)

```go
// Suggestion is a deterministic, reviewed next step for a finding. Never LLM-decided.
type Suggestion struct {
	NextStep string // concise cause direction / what to do
	Command  string // a read-only kubectl command to investigate ("" when N/A)
}

// For returns the suggestion for a finding, by its Issue. A finding whose Issue is
// unrecognized gets a safe generic describe suggestion.
func For(f diagnose.Finding) Suggestion
```

- Pure; a `switch` on `f.Issue`. The `Command` is templated from the finding's
  `Pod` (`"ns/name"`, split into `-n <ns> <name>`) and `Container` (`-c <c>`,
  omitted when empty). Commands are **only** `kubectl logs` / `describe pod` /
  `get events` — never a mutation.
- Mapping (the full detector Issue set):

  | Issue | NextStep | Command |
  |---|---|---|
  | `CrashLoopBackOff`, `RestartLoop` | `starts then crashes — inspect the crash output` | `kubectl -n <ns> logs <pod> [-c <c>] --previous` |
  | `ImagePullBackOff`, `ErrImagePull` | `the image can't be pulled — verify the tag exists and the registry credentials` | `kubectl -n <ns> describe pod <pod>` |
  | `OOMKilled` | `the container exceeded its memory limit — raise the limit or fix the leak` | `kubectl -n <ns> describe pod <pod>` |
  | `Unschedulable` | `no node can place the pod — check resource requests, taints, and affinity` | `kubectl -n <ns> describe pod <pod>` |
  | `CreateContainerConfigError` | `a referenced ConfigMap or Secret is missing — create it or fix the reference` | `kubectl -n <ns> describe pod <pod>` |
  | `ProbeFailure` | `the probe keeps failing — check the probe config and the app's health endpoint` | `kubectl -n <ns> describe pod <pod>` |
  | `VolumeAttachError` | `the volume can't attach — check the PVC/PV binding and the CSI driver` | `kubectl -n <ns> describe pod <pod>` |
  | `Init:CrashLoopBackOff`, `Init:ImagePullBackOff`, `Init:OOMKilled` | `an init container is failing — the pod cannot start until it succeeds` | `kubectl -n <ns> logs <pod> [-c <c>] --previous` |
  | `FailedCreate` | `the controller can't create pods — check for quota, LimitRange, or a rejecting admission webhook` | `kubectl -n <ns> get events --field-selector reason=FailedCreate` |
  | `JobFailed` | `the Job exhausted its retries — inspect the failed pod's logs` | `kubectl -n <ns> logs <pod> --previous` |
  | *(default / unrecognized)* | `inspect the object for details` | `kubectl -n <ns> describe pod <pod>` |

- `Init:*` uses `f.Container` (the failing init container) in the `-c`.
- Imports `diagnose` (for `Finding`) + `strings`/`fmt`. No cycle: `diagnose`
  does not import `remediation`.

### 2. `report` — render under each finding when `--suggest`

- Add `Suggest bool` to `report.Input`.
- `printWorkload` gains a `suggest bool` parameter (threaded from the render loop,
  which reads `in.Suggest`). After each finding's existing lines (Reason,
  Evidence, and any `--logs` lines), when `suggest`:
  ```go
  if suggest {
      s := remediation.For(f)
      if s.NextStep != "" {
          fmt.Fprintf(w, "      ↳ next step: %s\n", s.NextStep)
      }
      if s.Command != "" {
          fmt.Fprintf(w, "      ↳ try: %s\n", s.Command)
      }
  }
  ```
- JSON: unchanged — suggestions are a text-rendering concern (the machine-readable
  `Issue` already lets a JSON consumer derive its own next step). Not added to the
  JSON `Finding` in this slice.

### 3. `main.go` — the `--suggest` flag

Add a plain bool flag (consistent with `--explain`/`--certs`/`--security` — no
env, no API key):

```go
	suggest := fs.Bool("suggest", false, "print a deterministic next-step suggestion (and a read-only kubectl command) under each finding")
```

Pass `Suggest: *suggest` into the `report.Input{…}` built by `resultInput`'s
caller (the presentation-extras block in `runScan`, alongside `SecurityVerbose`).

## Global constraints

- **Read-only; opt-in flag; offline.** No new collector, RBAC, watch gauge,
  `Result` field, or API call. Suggestions and commands are text; kubeagent never
  runs them. Touches `internal/remediation` (new) + `internal/report` + `main.go`
  → **LIGHTWEIGHT SMOKE** gate. **Minor** bump v0.41.0 → **v0.42.0**; **patch**
  chart bump (no Helm change).
- **Deterministic & never-LLM** — a fixed, reviewed mapping; no clock, no cluster
  calls, no model. Same input → same output.
- **Default output unchanged** — `--suggest` is off by default, so the baseline
  golden snapshot does not change.
- `inventory`, `clusterhealth`, `rootcause`, `confidence`, `explain.go`, `--fix`,
  the watch daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Suggestions for the Assess-list issues (PDB/HPA/service/PVC/webhook/stuck-terminating
— they already carry prescriptive detail; a later slice); feeding these
deterministic suggestions to `--explain` for LLM ranking (the next Theme-C slice);
a JSON field for the suggestion; running or copy-executing the commands
(read-only — the operator runs them); per-Reason (vs per-Issue) granularity;
mutation commands of any kind.

## Testing

- **`remediation.For` (pure, table-driven):**
  - each Issue in the mapping → its exact NextStep + Command, with `-n <ns>`,
    `<pod>`, and `-c <container>` filled from the finding (e.g. a `CrashLoopBackOff`
    finding `Pod: "shop/web-abc", Container: "web"` → command `kubectl -n shop logs
    web-abc -c web --previous`).
  - `Container == ""` → the command omits `-c` (e.g. `kubectl -n shop logs web-abc
    --previous`).
  - an unrecognized Issue (`"SomethingNew"`) → the generic describe suggestion.
  - a `FailedCreate` finding → the `get events --field-selector reason=FailedCreate`
    command (no `-c`).
  - commands never contain a mutating verb (assert none of `delete`/`apply`/`edit`/
    `patch`/`scale`/`rollout`/`cordon` appears).
- **`report` (Suggest flag):** a workload with a `CrashLoopBackOff` finding
  rendered with `Input{Suggest: true}` includes the `↳ next step:` and `↳ try:`
  lines; with `Suggest: false` (default) neither line appears.
- **`main`:** `--suggest` is a recognized flag (mirrors the existing flag-parse
  tests — e.g. with `--suggest --output bogus` the error is the output-format
  error, proving the flag parsed).
- **Golden:** unchanged (the default path has `Suggest=false`). No golden update.

## Files touched

- **Create:** `internal/remediation/remediation.go` (+ test).
- **Modify:** `internal/report/report.go` (+ test) — `Input.Suggest`, `printWorkload` suggest lines.
- **Modify:** `main.go` (+ `main_test.go`) — the `--suggest` flag + wiring.
- **Docs:** `website/docs/features/diagnostics.md` (a short `--suggest` note), `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
