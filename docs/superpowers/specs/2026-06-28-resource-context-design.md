# kubeagent — Design: resource context for findings & `--explain`

**Status:** approved design (pre-implementation)
**Date:** 2026-06-28

## Goal

Give the scan report and the AI (`--explain`) the numbers they need to reason
about OOMKills and cluster capacity:

1. On an **OOMKilled** finding, show the killed container's CPU and memory
   **requests + limits**.
2. Add a **cluster-wide resource summary** — allocatable, reserved (requests),
   limits, and live usage — for CPU and memory.

The AI can then judge whether to raise a limit or scale out (is there node
headroom?), instead of guessing.

## Decisions (from brainstorming)

- **Cluster summary content:** reserved (requests) **and** live usage. Usage is
  read from metrics-server.
- **OOM finding detail:** CPU **and** memory, **requests + limits** (requests vs
  limits tells the overcommit story).
- **Surfacing:** always on — fed to the `--explain` prompt and shown compactly in
  text and JSON. Per-OOM container resources render inline with that finding.
- **Scope (YAGNI):** cluster **totals only** (no per-node breakdown); resource
  context attached to **OOMKilled only** (not every detector); **CPU + memory**
  only. All are easy follow-ups later.

## Invariants preserved

- **Read-only:** only List/Get plus one raw `GET` on the metrics API. Never
  mutate cluster resources.
- **No new Go module dependency.** Live usage is read via the existing client's
  raw REST (`AbsPath("/apis/metrics.k8s.io/v1beta1/nodes")`); quantities use
  `k8s.io/apimachinery/pkg/api/resource`, already in the dependency tree.
- **Sequential**, stdlib `flag`, exit codes `0`/`1` unchanged.
- **`--explain` egress unchanged in spirit:** only structured summary fields are
  sent. Cluster resource **totals** and container resource **quantities** go to
  the model — never pod IPs, per-node names, raw specs, env, or secrets.
- **Offline-capable:** metrics-server is optional. When it is absent the scan
  still works; usage is reported as unavailable.

## Architecture

The pipeline gains one pure aggregation step and one optional I/O fetch:

```text
collect (pods, controllers, nodes, +node metrics)
      → diagnose (OOMKilled now attaches container resources)
      → inventory.Assemble → inventory.Prioritize
      → resources.Summarize(nodes, pods, usage)   ← new pure step
      → report / explain
```

## Component 1 — OOMKilled finding carries container resources

`internal/diagnose/diagnose.go` — `Finding` gains an optional nested struct:

```go
// ContainerResources is the requests/limits of one container, as human-readable
// quantity strings ("500m", "4Gi"). An unset request or limit is "unset".
type ContainerResources struct {
    Container  string `json:"container"`
    CPURequest string `json:"cpuRequest"`
    CPULimit   string `json:"cpuLimit"`
    MemRequest string `json:"memRequest"`
    MemLimit   string `json:"memLimit"`
}

type Finding struct {
    Pod       string              `json:"pod"`
    Issue     string              `json:"issue"`
    Reason    string              `json:"reason"`
    Evidence  string              `json:"evidence"`
    Resources *ContainerResources `json:"resources,omitempty"` // set by OOMKilled
}
```

`internal/diagnose/oomkilled.go` — when a container is OOMKilled, look it up in
`facts.Pod.Spec.Containers` by name and fill `Resources` from
`container.Resources.Requests`/`.Limits` (`corev1.ResourceCPU`,
`corev1.ResourceMemory`). A missing quantity renders as `"unset"`. A small helper
`quantityOrUnset(rl corev1.ResourceList, name corev1.ResourceName) string`
formats one entry. Other detectors leave `Resources` nil.

## Component 2 — `internal/resources` (pure aggregation)

```go
// Line is one resource's cluster accounting. Quantities are human-readable
// strings; percentages are integers (of allocatable), 0 when allocatable is 0.
type Line struct {
    Allocatable string `json:"allocatable"`
    Requests    string `json:"requests"`
    Limits      string `json:"limits"`
    Usage       string `json:"usage,omitempty"` // "" when metrics unavailable
    RequestsPct int    `json:"requestsPct"`
    LimitsPct   int    `json:"limitsPct"`
    UsagePct    int    `json:"usagePct,omitempty"`
}

// Summary is the cluster-wide CPU and memory picture.
type Summary struct {
    CPU              Line `json:"cpu"`
    Memory           Line `json:"memory"`
    MetricsAvailable bool `json:"metricsAvailable"`
}

// Summarize aggregates node allocatable, pod requests/limits, and (optional)
// node usage into a cluster Summary. usage is keyed by node name; nil/empty
// usage yields MetricsAvailable=false and empty Usage fields.
func Summarize(nodes []corev1.Node, pods []corev1.Pod, usage map[string]corev1.ResourceList) Summary
```

Rules:
- **Allocatable** = Σ over nodes of `node.Status.Allocatable[cpu|memory]`.
- **Requests / Limits** = Σ over **non-terminal** pods (phase not Succeeded/Failed)
  of each container's `Resources.Requests`/`.Limits` for cpu/memory. Terminal
  pods reserve nothing — matches `kubectl describe node`'s "Allocated resources".
- **Usage** = Σ over nodes of `usage[nodeName][cpu|memory]` when present.
- **Percent** = value / allocatable × 100 using `MilliValue()` for CPU and
  `Value()` for memory; allocatable 0 → 0.
- Formatting helpers: CPU as cores `"%.1f"` (e.g. `4.0`, `19.0`); memory rounded
  to `Gi` (e.g. `28Gi`, `115Gi`), falling back to `Mi` under 1Gi.

## Component 3 — metrics + cluster pods (`internal/collect`)

```go
// NodeMetrics reads live per-node usage from metrics-server via a raw GET on the
// metrics API. available is false (no error surfaced) when metrics-server is
// absent or forbidden, so a scan still succeeds without it.
func NodeMetrics(ctx context.Context, client kubernetes.Interface) (usage map[string]corev1.ResourceList, available bool, err error)

// AllPods lists pods across all namespaces (read-only). Used for the cluster
// resource summary when the scan itself is namespace-scoped.
func AllPods(ctx context.Context, client kubernetes.Interface) ([]corev1.Pod, error)
```

- `NodeMetrics` calls
  `client.CoreV1().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").DoRaw(ctx)`
  and delegates parsing to a pure, unit-tested
  `parseNodeMetrics(data []byte) (map[string]corev1.ResourceList, error)`.
  The parser unmarshals the metrics list shape:
  `{ "items": [ { "metadata": {"name": ...}, "usage": {"cpu": "...", "memory": "..."} } ] }`,
  parsing each value with `resource.ParseQuantity`.
- A `DoRaw` error (e.g. 404/Forbidden — no metrics-server) maps to
  `available=false, err=nil`.

## Component 4 — surfacing

### text (`internal/report`)

A compact block prints directly under the cluster verdict block:

```text
Resources (cluster):
  CPU     19 cores · req 4.0 (21%) · lim 12.0 (63%) · used 3.3 (17%)
  Memory  115Gi · req 28Gi (24%) · lim 40Gi (35%) · used 95Gi (83%)
```

- When `MetricsAvailable` is false: omit the `· used …` segment and append a
  single note line `  (usage: metrics-server unavailable)`.
- Per-OOM finding renders an extra sub-line under the existing `⚠ Issue: Reason`:
  `      resources: memory req=1Gi limit=4Gi · cpu req=500m limit=3`.

### json (`internal/report`)

`inventoryReport` gains `Resources *resources.Summary` (`omitempty`). Each OOM
finding already carries its `resources` via the `Finding` struct.

### explain (`internal/explain`)

`buildInventoryPrompt` gains a section (cluster totals only):

```text
Cluster resources:
  CPU: allocatable 19 cores, requests 4.0 (21%), limits 12.0 (63%), usage 3.3 (17%)
  Memory: allocatable 115Gi, requests 28Gi (24%), limits 40Gi (35%), usage 95Gi (83%)
```

and, under an OOM finding line, appends
`      container resources: memory req=1Gi limit=4Gi, cpu req=500m limit=3`.
When metrics are unavailable the usage clause is dropped. `ExplainInventory`
takes the `Summary` as a new parameter; the healthy-and-empty skip is unchanged.

## Component 5 — wiring (`main.go`)

After `collect.Nodes(...)`:

```go
usage, _, _ := collect.NodeMetrics(ctx, client) // graceful: usage nil if absent
pods := inputs.Pods
if namespace != "" {
    if all, err := collect.AllPods(ctx, client); err == nil {
        pods = all
    }
}
summary := resources.Summarize(nodes, pods, usage)
```

`summary` is passed to `report.PrintInventory` and `explain.ExplainInventory`.
Everything else in `run` is unchanged.

## Testing (TDD)

- `resources.Summarize` — table tests: allocatable/requests/limits sums;
  terminal pods excluded from reserved; percentages (incl. allocatable 0 → 0%);
  with and without `usage`; CPU cores and memory Gi/Mi formatting.
- `collect.parseNodeMetrics` — parses a sample metrics JSON into per-node
  quantities; malformed input errors.
- `diagnose` (OOMKilled) — finding carries container `Resources` with req+limit
  for cpu+mem; a container with no limit renders `unset`; non-OOM findings keep
  `Resources` nil.
- `report` — text resource block with metrics, and without (note line); JSON
  includes `resources`; per-OOM sub-line renders.
- `explain` — prompt includes the `Cluster resources:` section and the per-OOM
  container line; egress guard holds (no node names/IPs/specs leak).
- `main` — wiring stays green; metrics absence is non-fatal.

## Out of scope (explicit non-goals)

- Per-node resource breakdown (cluster totals only).
- Resource context on non-OOM findings (CrashLoop, ImagePull, Pending).
- Resources other than CPU and memory (no ephemeral-storage, GPUs, hugepages).
- A flag to toggle the summary (it is always on per the decision above).
- Historical/trend usage or metrics beyond the current snapshot.
