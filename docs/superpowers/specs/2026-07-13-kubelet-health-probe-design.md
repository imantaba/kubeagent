# Kubelet Health Probe — Design

**Date:** 2026-07-13
**Status:** Approved

## Goal

Actively probe each node's kubelet `/healthz` and flag a kubelet that is **alive
but reporting unhealthy** (e.g. a failing PLEG/runtime/syncloop subcheck) — a
failure mode the passive checks miss. The lease-heartbeat check catches a kubelet
that stopped renewing (dead/hung); a failing `/healthz` catches one that is
running and responding but self-reports a problem, often *before* the node flips
to `NotReady`.

Opt-in (`--kubelet-health`); **advisory** — it appears in its own `KUBELET
HEALTH` section and JSON but does **not** change the cluster verdict. Reuses the
existing `nodes/proxy` add-on grant (no new RBAC type). Read-only: a `GET
/healthz`.

## Data source

The kubelet's main authenticated server (port 10250) serves `/healthz`, reachable
through the API server via the `nodes/proxy` subresource:
`/api/v1/nodes/<name>/proxy/healthz`. This is the same path/port `--disk-usage`
uses for `/proxy/stats/summary`, so the same `nodes/proxy` grant covers both. The
probe is API-server-mediated, so the scanner's own network to the node does not
matter.

`/healthz` returns HTTP 200 with body `ok` when healthy; a non-200 with a body
listing failed subchecks (e.g. `[-]pleg failed`) when unhealthy.

## Classification

`collect.KubeletHealthz` issues the GET and classifies the result into a
`nodehealth.Probe{Node, Status, Detail}` where `Status` is one of:

- **`ok`** — HTTP 200. No finding.
- **`unhealthy`** — the kubelet responded with a non-200 status. `Detail` = the
  response body's first line, trimmed and truncated (e.g. `[-]pleg failed`).
- **`forbidden`** — HTTP 401/403 (the `nodes/proxy` grant is missing). Skipped,
  non-fatal; used to drive the missing-grant hint.
- **`unreachable`** — a transport error / timeout (node down, kubelet not
  answering). Skipped, non-fatal. A dead node's kubelet is unreachable, so it is
  **not** falsely flagged unhealthy (the node is already `NotReady` via
  clusterhealth).

The classification is a **pure helper** `classify(statusCode int, body []byte,
transportErr error) Probe` so it is unit-testable; the RESTClient call is a thin,
untested wrapper (mirroring `collect.NodeStats`).

All nodes are probed (mirroring `--disk-usage`); unreachable/forbidden ones are
skipped, so in practice only reachable kubelets are evaluated. No explicit Ready
filter is needed — the skip handles dead nodes.

## Components

### `internal/nodehealth` (new, pure)

```go
type Probe struct {
	Node   string `json:"node"`
	Status string `json:"status"`           // "ok" | "unhealthy" | "forbidden" | "unreachable"
	Detail string `json:"detail,omitempty"`
}

type Issue struct {
	Node   string `json:"node"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	Unhealthy []Issue `json:"unhealthy,omitempty"`
	Probed    int     `json:"probed"`
	Forbidden int     `json:"forbidden"`
}

// Assess collapses per-node probes into the advisory report: the unhealthy
// nodes, plus counts used for the daemon gauge and the missing-grant hint.
func Assess(probes []Probe) Report
```

`Assess` copies every `unhealthy` probe into `Unhealthy` (as an `Issue`), sets
`Probed = len(probes)`, and `Forbidden =` the count of `forbidden` probes. Order
follows the input; the caller (scan) probes nodes in node-list order.

### `internal/collect`

```go
func KubeletHealthz(ctx context.Context, client kubernetes.Interface, node string) nodehealth.Probe
```

Does `client.CoreV1().RESTClient().Get().AbsPath("/api/v1/nodes/<node>/proxy/healthz").Do(ctx)`,
reads the status code + raw body, and returns `classify(code, body, err)`. Never
returns an error (non-fatal, like `NodeStats`). `classify` is exported-for-test
within the package (lowercase, same-package test).

### `internal/scan`

`Options` gains `KubeletHealth bool`. `Evaluate`, mirroring the `if opts.DiskUsage`
block:

```go
var kubeletHealth nodehealth.Report
if opts.KubeletHealth {
	var probes []nodehealth.Probe
	for _, n := range nodes {
		probes = append(probes, collect.KubeletHealthz(ctx, client, n.Name))
	}
	kubeletHealth = nodehealth.Assess(probes)
}
```

`Result` gains `KubeletHealth nodehealth.Report`. No `clusterhealth` change (the
verdict is untouched).

### `internal/report`

- `Input.KubeletHealth *nodehealth.Report` (nil when the flag is off).
- A dedicated **`KUBELET HEALTH`** text section (its own block, like `SECURITY`),
  rendered only with the flag on: each unhealthy node as `✗ node <name> kubelet
  /healthz unhealthy: <detail>`. It does **not** contribute to the verdict or the
  "Needs attention" line.
- **Missing-grant hint:** when `Report.Forbidden == Report.Probed && Report.Probed
  > 0` (every probe forbidden — the add-on isn't applied), the section prints a
  one-line hint instead of unhealthy lines: `kubelet-health needs the nodes/proxy
  add-on (deploy/rbac-diskusage.yaml or Helm kubeletHealth.enabled)`.
- **All-clear:** define `hasKubeletHealth` = the section rendered anything (at
  least one unhealthy node **or** the missing-grant hint). When true, the `No
  issues found. ✅` all-clear is suppressed (mirroring `hasSecurity`); the verdict
  and "Needs attention" line are still untouched. When the report is nil, the
  flag is off, or every probe was `ok`/`unreachable` (nothing rendered), the
  all-clear behaves exactly as today.
- JSON: `inventoryReport.KubeletHealth *nodehealth.Report`
  (`json:"kubeletHealth,omitempty"`).

### `main.go` / `internal/watch`

- `main.go` (scan): `--kubelet-health` bool flag ("probe each kubelet's /healthz
  via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)"), into
  `scan.Options`; added to the scan usage string.
- `internal/watch`: `Config.KubeletHealth bool` from `envBool("KUBEAGENT_KUBELET_HEALTH",
  false)`, into the daemon's `scan.Options`. `metrics.go` renders
  `kubeagent_kubelet_unhealthy` from `len(Result.KubeletHealth.Unhealthy)` in the
  success path (last-good on error).

### RBAC (reuse, no new type)

- Raw manifest: `deploy/rbac-diskusage.yaml` already grants `nodes/proxy` `get`.
  It is documented as the shared add-on for both `--disk-usage` and
  `--kubelet-health`; no new manifest is added.
- Helm: broaden the `nodes/proxy` rule's gate in
  `deploy/helm/kubeagent/templates/clusterrole.yaml` from `{{- if
  .Values.diskUsage.enabled }}` to `{{- if or .Values.diskUsage.enabled
  .Values.kubeletHealth.enabled }}`; add `kubeletHealth.enabled` (default `false`)
  to `values.yaml`; and set the `KUBEAGENT_KUBELET_HEALTH` env on the daemon
  Deployment when `kubeletHealth.enabled` is true (mirroring how
  `diskUsage.enabled` drives `KUBEAGENT_DISK_USAGE`).

## Scope boundaries

- Read-only (a `GET /healthz`); opt-in; advisory (no verdict / exit-code /
  `kubeagent_cluster_healthy` impact); not wired into `--explain`.
- Reuses the `nodes/proxy` grant — no new RBAC resource type.
- Complements, does not replace, the lease-heartbeat and NotReady checks
  (different failure mode: alive-but-sick vs dead/hung vs node-condition-flipped).
- v1 treats `/healthz` as a binary ok/not-ok signal with the body as detail; it
  does not parse individual subchecks into structured findings.

## Testing

- `classify` (pure) unit tests: 200 → `ok`; non-200 (e.g. 500 with `[-]pleg
  failed`) → `unhealthy` + trimmed first-line detail; 403 → `forbidden`; a
  transport error → `unreachable`.
- `nodehealth.Assess` table tests: mixed probes → only `unhealthy` collected;
  `Probed`/`Forbidden` counts correct; all-`ok` → empty `Unhealthy`.
- `report`: the `KUBELET HEALTH` section renders unhealthy nodes and is suppressed
  when the flag is off; the all-forbidden hint prints when `Forbidden == Probed`;
  the all-clear is suppressed when there are unhealthy nodes and preserved when
  there are none; JSON contains `kubeletHealth`.
- `internal/watch/metrics` test asserts `kubeagent_kubelet_unhealthy` reflects
  `len(Health-report.Unhealthy)`.

## Docs

- `CHANGELOG.md` (`## [Unreleased]` → `### Added`).
- `website/docs/features/diagnostics.md`: a "Kubelet health probe" subsection.
- `website/docs/features/watch-mode.md`: the `kubeagent_kubelet_unhealthy` gauge
  and the `KUBEAGENT_KUBELET_HEALTH` env; note the shared `nodes/proxy` add-on.
- `website/docs/roadmap.md`: a Shipped bullet.
- `README.md`: one-line mention.

Exact names to use verbatim: flag `--kubelet-health`; env
`KUBEAGENT_KUBELET_HEALTH`; gauge `kubeagent_kubelet_unhealthy`; JSON field
`kubeletHealth`; section header `KUBELET HEALTH`; Helm value
`kubeletHealth.enabled`.
