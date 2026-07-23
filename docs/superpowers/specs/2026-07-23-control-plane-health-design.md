# Control-plane / etcd health (`--control-plane-health`) — design

**Status:** approved · **Date:** 2026-07-23 · **Type:** new opt-in check (Theme B, deeper diagnosis — closer #1 of 3)

## Problem

`clusterhealth` checks node health and a **kube-system workload rollup**
(Deployments/DaemonSets like CoreDNS), but the **control-plane subsystem** —
kube-apiserver readiness and **etcd** — is never checked as such. Nothing reads
`/readyz`, `/livez`, `/healthz`, or `componentstatuses` today. When etcd is
degraded or the apiserver is up-but-not-ready (admission/controller poststarthook
failing, informer-sync stuck), the cluster is serving but impaired, and kubeagent
says nothing. This adds an opt-in, read-only probe of the apiserver's own
`/readyz?verbose` endpoint — which names etcd and each readiness check directly —
mirroring the existing `--kubelet-health` probe of kubelet `/healthz`.

Scheduler and controller-manager health (separate health ports; partly covered by
the existing kube-system rollup when they run as static pods) are **deferred** to a
follow-on slice. This slice covers **apiserver readiness + etcd**, the highest-value
uncovered gap.

## Behavior (approved)

With `--control-plane-health`, kubeagent probes the apiserver `/readyz?verbose`
once and, when it reports not-ready, prints a `CONTROL PLANE` section:

```text
CONTROL PLANE
  ✗ control plane not ready
      ⚠ 2 checks failing: etcd, poststarthook/start-kube-apiserver-admission-initializer
```

- **Probed only when `--control-plane-health` is set** (opt-in, like
  `--kubelet-health`/`--disk-usage`). Off by default → default output unchanged.
- **Statuses:** `ok` (HTTP 200 — silent), `unhealthy` (non-200 with a body — lists
  the failing `[-]` checks), `forbidden` (401/403 — the `/readyz` grant is missing;
  prints a one-line hint), `unreachable` (transport error — code 0; silent beyond a
  hint, since a fully-down apiserver is already handled by connectivity diagnosis).
- **Advisory** — the `CONTROL PLANE` section appears but does **not** change the
  cluster Healthy/Unhealthy verdict in this slice (consistent with the other opt-in
  probes; verdict integration is a possible follow-on).
- **Failing-check names** come from the `[-]<name> …` lines of the verbose body
  (e.g. `etcd`, `etcd-readiness`, `informer-sync`, `poststarthook/*`), each named so
  the operator knows which subsystem is down.

## Design

### 1. `internal/controlplane` — pure classification (new package)

```go
// Probe is the apiserver /readyz classification.
type Probe struct {
    Status string   `json:"status"`           // "ok" | "unhealthy" | "forbidden" | "unreachable"
    Failed []string `json:"failed,omitempty"` // failing check names when unhealthy
}

// ParseReadyz classifies an HTTP status code + /readyz?verbose body into a Probe.
func ParseReadyz(code int, body []byte) Probe
```

- `code == 200` → `Probe{Status: "ok"}`.
- `code == 401 || code == 403` → `Probe{Status: "forbidden"}`.
- `code == 0` → `Probe{Status: "unreachable"}`.
- else → `Probe{Status: "unhealthy", Failed: <names>}`, where `<names>` is every
  token immediately after a `[-]` prefix (`strings.Fields(line[3:])[0]`), across the
  trimmed lines of the body. An empty/omitted body yields `unhealthy` with `Failed`
  nil (a generic "not ready"). Imports `strings` only. Pure — no I/O.

### 2. `collect.ControlPlaneReadyz` — the probe

Mirrors `collect.KubeletHealthz` (never returns an error):

```go
// ControlPlaneReadyz probes the apiserver /readyz?verbose endpoint and classifies
// the result. Never returns an error (non-fatal, like KubeletHealthz). Needs the
// nonResourceURLs /readyz get grant; a 401/403 yields Status "forbidden".
func ControlPlaneReadyz(ctx context.Context, client kubernetes.Interface) controlplane.Probe {
    var code int
    body, _ := client.CoreV1().RESTClient().Get().
        AbsPath("/readyz").Param("verbose", "true").
        Do(ctx).StatusCode(&code).Raw()
    return controlplane.ParseReadyz(code, body)
}
```

### 3. `scan.Evaluate` — gated probe

- Add `ControlPlaneHealth bool` to `scan.Options` (next to `KubeletHealth`).
- Add `ControlPlane controlplane.Probe` to `Result` (next to `KubeletHealth`).
- In `Evaluate`, mirroring the `KubeletHealth` block: when
  `opts.ControlPlaneHealth`, set `result.ControlPlane = collect.ControlPlaneReadyz(ctx, client)`.
  When off, leave the zero value (`Status == ""` → not probed).

### 4. `report` — the `CONTROL PLANE` section

- Add `ControlPlane controlplane.Probe` to `report.Input` and to the JSON
  `inventoryReport` struct (`json:"controlPlane,omitempty"` — the zero value with an
  empty Status omits cleanly since Status is `""`); copy it in the Input→report
  JSON-build literal.
- A `printControlPlane(p controlplane.Probe, w io.Writer)` helper:
  - `Status == "unhealthy"`: print the `CONTROL PLANE` header, `  ✗ control plane not
    ready`, and `      ⚠ <n> checks failing: <comma-joined Failed>` (or
    `      ⚠ apiserver /readyz reported not ready` when `Failed` is empty).
  - `Status == "forbidden"`: print a one-line hint (grant `/readyz` to enable) —
    rendered in the same advisory style as kubelet-health's forbidden hint.
  - `Status == "ok"`, `"unreachable"`, or `""`: print nothing.
- Call `printControlPlane(in.ControlPlane, w)` after the `KUBELET HEALTH` section.
- **`main.go` `resultInput`** maps `ControlPlane: res.ControlPlane` (the seam).
- JSON: `controlPlane` serializes for free.

### 5. `main.go` — flag + env + daemon plumbing

- `controlPlaneHealth := fs.Bool("control-plane-health", false, "probe the apiserver /readyz endpoint and flag an unhealthy control plane / etcd (needs the /readyz grant)")`.
- Set `ControlPlaneHealth: *controlPlaneHealth` in the CLI `scan.Options`.
- Daemon: read `KUBEAGENT_CONTROL_PLANE_HEALTH` (envBool, default false) into
  `watch.Config`; `watch.Config` gains `ControlPlaneHealth bool`; `internal/watch`'s
  `scan.Options` literal sets `ControlPlaneHealth: cfg.ControlPlaneHealth` (mirroring
  `KubeletHealth`).

### 6. `watch` — the daemon gauge

Add `controlPlaneUnhealthy int` to the metrics struct; set it to `1` when
`res.ControlPlane.Status == "unhealthy"` else `0`; register
`gauge("kubeagent_control_plane_unhealthy", "Apiserver /readyz reported the control plane not ready", float64(m.controlPlaneUnhealthy))`.

### 7. RBAC — a conditional `/readyz` grant

- `deploy/helm/kubeagent/templates/clusterrole.yaml`: add a conditional rule block
  gated by a new value, next to the `nodes/proxy` block:
  ```yaml
  {{- if .Values.controlPlaneHealth.enabled }}
  - nonResourceURLs: ["/readyz"]
    verbs: [get]
  {{- end }}
  ```
  (nonResourceURLs cannot be combined with `resources`/`apiGroups` in one rule, so
  this is its own block.)
- `deploy/helm/kubeagent/values.yaml`: add `controlPlaneHealth: { enabled: false }`.
- `deploy/helm/kubeagent/templates/deployment.yaml`: set env
  `KUBEAGENT_CONTROL_PLANE_HEALTH: "true"` when `.Values.controlPlaneHealth.enabled`
  (mirror how `KUBEAGENT_DISK_USAGE`/`KUBEAGENT_KUBELET_HEALTH` are set from values).
- `deploy/rbac-controlplane.yaml`: a raw-manifest add-on (mirroring
  `deploy/rbac-diskusage.yaml`) that grants `nonResourceURLs: ["/readyz"] get` for
  the non-Helm path, with a short header comment.

## Global constraints

- **Read-only; opt-in; offline.** No writes, no LLM. Touches `internal/controlplane`
  (new) + `internal/collect` + `internal/scan` + `internal/report` + `main.go` +
  `internal/watch` + the RBAC manifests, values, and deployment template.
- **Gate:** collect/RBAC/watch/Helm-template changes → **FULL CHAOS GATE**. **Minor**
  bump v0.44.0 → **v0.45.0**; **chart MINOR** bump (clusterrole/values/deployment
  templates changed).
- **Pure & deterministic** — `ParseReadyz` reads only its arguments; no clock, no
  cluster calls. The collector is the only I/O and is non-fatal.
- **Default output unchanged** — opt-in; off by default, so the golden snapshot does
  not change.
- **Advisory** — the cluster verdict logic is untouched.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, `explain.go`, `--fix`, and
  the existing detectors/Assess checks stay **unchanged**.
- **Privacy** — `/readyz?verbose` returns check names and pass/fail state, no secrets
  or cluster data; safe to display.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

## Out of scope (YAGNI)

Scheduler / controller-manager health (separate health ports / static-pod checks —
a follow-on; partly covered by the kube-system rollup today); `componentstatuses`
(deprecated); deep etcd metrics (member list, DB size, raft — needs etcd access, not
available via the apiserver); wiring control-plane-not-ready into the cluster
verdict (advisory for now); probing `/livez` in addition to `/readyz` (readyz is the
richer superset for this purpose); a CLI threshold or per-check severity (a failing
readyz check is binary).

## Testing

- **`controlplane.ParseReadyz` (pure, table-driven):**
  - `code 200`, healthy body → `Status "ok"`, no Failed.
  - `code 500` with a body containing `[+]ping ok`, `[+]etcd ok`,
    `[-]poststarthook/x failed: reason`, `[-]informer-sync failed`,
    `readyz check failed` → `Status "unhealthy"`, `Failed == ["poststarthook/x",
    "informer-sync"]` (order preserved; only `[-]` lines).
  - a body with `[-]etcd failed: reason withheld` → `Failed` contains `"etcd"`.
  - `code 403` → `"forbidden"`; `code 401` → `"forbidden"`.
  - `code 0` → `"unreachable"`.
  - `code 503` with an empty body → `"unhealthy"`, `Failed` nil.
- **`collect.ControlPlaneReadyz` (fake clientset):** a fake REST response is hard to
  stub with the standard fake clientset; test at minimum that the function returns a
  `Probe` without panicking against `fake.NewSimpleClientset()` (which yields a code
  and empty body → a deterministic classification) — the classification logic itself
  is covered by the `ParseReadyz` table. (Mirror whatever depth the `KubeletHealthz`
  test uses.)
- **`scan` integration:** `Evaluate` with `Options{ControlPlaneHealth: true}` against
  a fake clientset sets `Result.ControlPlane` to a non-empty Status; with the flag
  off, `Result.ControlPlane.Status == ""` (not probed).
- **`report` render:** an `Input{ControlPlane: {Status:"unhealthy", Failed:["etcd",
  "poststarthook/x"]}}` renders the `CONTROL PLANE` section with `✗ control plane not
  ready` and `2 checks failing: etcd, poststarthook/x`; a `forbidden` Probe renders
  the grant hint; an `ok`/empty Probe renders nothing.
- **`main` seam:** `resultInput` copies `ControlPlane` from `Result` to `Input`.
- **`main` flag:** `--control-plane-health --output bogus` errors with the
  output-format error (proves the flag parsed).
- **`watch` gauge:** a `Result` with `ControlPlane.Status == "unhealthy"` renders
  `kubeagent_control_plane_unhealthy 1`; healthy/absent → `0`.
- **Golden:** unchanged (opt-in; the default fixture does not set ControlPlane).

## Files touched

- **Create:** `internal/controlplane/controlplane.go` (+ test).
- **Modify:** `internal/collect/collect.go` (+ test) — `ControlPlaneReadyz`.
- **Modify:** `internal/scan/scan.go` (+ test) — `Options.ControlPlaneHealth`, `Result.ControlPlane`.
- **Modify:** `internal/report/report.go` (+ test) — `Input.ControlPlane`, `printControlPlane`.
- **Modify:** `main.go` (+ `main_test.go`) — flag, env, `resultInput` seam.
- **Modify:** `internal/watch/watch.go`, `internal/watch/metrics.go` (+ test) — config plumbing + gauge.
- **Modify:** `deploy/helm/kubeagent/templates/clusterrole.yaml`, `values.yaml`, `templates/deployment.yaml`; **create** `deploy/rbac-controlplane.yaml`.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.
