# Operator/CRD adapters — design

**Date:** 2026-07-26
**Theme:** F · Ecosystem & operators — slice 1 (the adapter foundation)
**Status:** approved for planning

## Goal

Teach `kubeagent scan` to report the health of the operators people actually
run — cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the Prometheus
operator — without compiling against any of their Go APIs and without giving a
third-party CRD authority over kubeagent's own verdict.

## Non-goals (later F slices, or deliberately never)

- **GitOps drift** — comparing desired Git state against live state. Slice 1
  reports what Argo CD and Flux already say about themselves; it does not
  compute drift.
- **Cost / right-sizing and scheduling-headroom hints** — a separate F slice.
- **Deep per-operator diagnosis** — cert-manager's
  `Certificate → CertificateRequest → Order → Challenge` failure chain, CNPG
  switchover state, Longhorn replica placement. Slice 1 establishes the adapter
  seam; depth lands per operator afterwards.
- **The `watch` daemon.** Scan only. The daemon gets operator awareness once the
  adapter shape has proven itself on real clusters; wiring `dynamicinformer` for
  every discovered GVR across every watched cluster is a larger surface than this
  slice should carry, and the multi-cluster hub only just landed.
- **Writing to any CRD, ever.** Not in `--fix`'s allowlist, not in a later
  slice, not behind a flag. kubeagent does not operate other people's operators.
- **Reporting the operator's own version.** Discovery yields the served API
  version, which is what the adapter needs. Inferring the operator's release
  from a Deployment image tag is guesswork; skipped under YAGNI.

## The problem this slice settles

kubeagent is entirely typed client-go today: zero uses of the dynamic client,
zero uses of discovery, zero `unstructured` anywhere. Every collector calls a
generated typed method (`client.AppsV1().Deployments(ns).List`). That works
because every kind it reads ships with Kubernetes.

Operator CRDs do not ship with Kubernetes. A cluster may serve
`cert-manager.io/v1` or may never have heard of it, and kubeagent must behave
correctly either way, from a single binary, without a rebuild.

### Why dynamic client + discovery gate

Three options were weighed:

1. **Typed imports of each operator's API module.** Compile-time-checked field
   access, at the cost of six third-party modules in `go.mod`, each pinning its
   own `k8s.io/api` version, and a build that a new operator release can break.
   kubeagent's release cadence would become coupled to six other projects'.
2. **A generic `.status.conditions` reader with no per-operator knowledge.**
   Smallest possible code, and it covers operators nobody anticipated — but the
   output is uniformly generic, and it is silently wrong for exactly the
   operators that matter most: Argo CD puts health in `.status.health.status`
   and Longhorn in `.status.robustness`, neither of which is a condition.
3. **Dynamic client, gated on discovery, with a declarative per-operator
   adapter table.** Chosen.

Option 3 adds no module dependency — `k8s.io/client-go` already ships
`dynamic`, `dynamic/fake`, `discovery`, and (for a later slice)
`dynamicinformer`. It reads any CRD version the cluster serves. It costs
compile-time type safety: field paths become `[]string{"status","robustness"}`
rather than struct selectors. That cost is mitigated structurally, not by
discipline — see [Testing](#testing).

## Architecture

Three new units plus two small integrations, following the shape every existing
opt-in feature already uses (`certhealth`, `diskusage`, `credlint`): I/O in
`collect`, a pure assessment package, and the CLI composing the view.

```text
cluster.NewDynamicClients          collect.OperatorResources        operators.Assess
  (dynamic.Interface +      →        (discovery gate, then     →     (pure, table-driven,
   discovery.DiscoveryInterface)      dynamic List per GVR)           no client)
                                                                            ↓
                                                              report.Input.Operators
```

`internal/scan` and `internal/watch` are **untouched**. `scan.Result` already
documents that the CLI composes extra views from `Inputs`/`Nodes` without
re-collecting; `credlint` and `platform` both do exactly this. Operator
assessment is one more such view, which is why it needs no change to
`scan.Options`, `scan.Result`, the daemon, or any RBAC the daemon uses.

### `internal/operators` — pure assessment

No Kubernetes client, no I/O, unit-tested with fixture objects.

```go
// State is one resource's health as its own operator reports it.
type State string

const (
    StateHealthy     State = "healthy"
    StateProgressing State = "progressing"
    StateUnhealthy   State = "unhealthy"
    StateSuspended   State = "suspended"
    StateUnknown     State = "unknown"
)

// Rule decides one resource's State from its unstructured content.
type Rule interface {
    Evaluate(obj map[string]any) (State, string) // State + short reason
}

// Adapter describes one operator resource kubeagent knows how to read.
type Adapter struct {
    Operator    string   // "cert-manager"
    Group       string   // "cert-manager.io"
    Version     string   // preferred version to try first: "v1"
    Resource    string   // plural, as discovery reports it: "certificates"
    Kind        string   // "Certificate"
    SuspendPath []string // optional; truthy value ⇒ StateSuspended
    Rule        Rule     // nil ⇒ counted, never judged
}

// Resource is one fetched CR, reduced to what the report may show.
type Resource struct {
    Operator  string `json:"operator"`
    Kind      string `json:"kind"`
    Namespace string `json:"namespace,omitempty"`
    Name      string `json:"name"`
    State     State  `json:"state"`
    Reason    string `json:"reason,omitempty"`
}

// KindReport is one resource kind's roll-up. Counts are per kind, not per
// operator: 214 unjudged ServiceMonitors must not be averaged into the one
// Prometheus CR's health.
type KindReport struct {
    Kind       string        `json:"kind"`
    APIVersion string        `json:"apiVersion"`          // as served, e.g. "cert-manager.io/v1"
    Judged     bool          `json:"judged"`              // false for adapters with no Rule
    Counts     map[State]int `json:"counts"`              // exact, never truncated
    Unhealthy  []Resource    `json:"unhealthy,omitempty"` // capped; see Bounding
    Truncated  int           `json:"truncated,omitempty"` // how many unhealthy were omitted
    Forbidden  bool          `json:"forbidden,omitempty"` // RBAC denied this kind
    Error      string        `json:"error,omitempty"`     // any other list failure, redacted
}

// OperatorReport is one operator's roll-up across its kinds.
type OperatorReport struct {
    Operator string       `json:"operator"`
    Kinds    []KindReport `json:"kinds"`
}

// Report is the whole advisory view. Empty when no known operator is installed.
type Report struct {
    Operators []OperatorReport `json:"operators,omitempty"`
}

// Fetched is one adapter's raw result, as collect hands it over.
type Fetched struct {
    Adapter    Adapter
    APIVersion string                       // the version actually served
    Items      []unstructured.Unstructured  // nil when Err is set
    Forbidden  bool
    Err        error
}

func Assess(fetched []Fetched) Report
```

Keeping `Assess` pure over `[]Fetched` — with the adapter carried inside each
element rather than passed alongside — is what makes every adapter testable
without a cluster, and removes any chance of `Assess` pairing a result with the
wrong adapter.

### The two rule implementations

```go
// ConditionRule reads .status.conditions[type=Type].status, the Kubernetes API
// convention. "True" ⇒ healthy, "False" ⇒ unhealthy, "Unknown" ⇒ progressing,
// condition absent ⇒ unknown. The condition's Reason is the reported reason.
type ConditionRule struct{ Type string }

// FieldRule reads a string at Path and maps it through explicit value sets. A
// value in no set ⇒ unknown: an unrecognized value from a CRD version we have
// not seen must not be reported as an outage.
type FieldRule struct {
    Path        []string
    Healthy     []string
    Progressing []string
    Unhealthy   []string
}
```

`StateUnknown` never counts as a problem. A heuristic field path that misses —
because the operator renamed a field, or serves a version we have not seen —
must degrade to "I cannot tell", never to "your database is down".

### The adapter table

Six operators, ten resources. Field paths and values below are the ones each
project documents; every entry is pinned by a fixture test.

| Operator | Group / version | Resource | Kind | Rule |
| --- | --- | --- | --- | --- |
| cert-manager | `cert-manager.io/v1` | `certificates` | `Certificate` | `ConditionRule{Ready}` |
| cert-manager | `cert-manager.io/v1` | `issuers` | `Issuer` | `ConditionRule{Ready}` |
| cert-manager | `cert-manager.io/v1` | `clusterissuers` | `ClusterIssuer` | `ConditionRule{Ready}` |
| CloudNativePG | `postgresql.cnpg.io/v1` | `clusters` | `Cluster` | `ConditionRule{Ready}` |
| Longhorn | `longhorn.io/v1beta2` | `volumes` | `Volume` | `FieldRule{status.robustness}` |
| Argo CD | `argoproj.io/v1alpha1` | `applications` | `Application` | `FieldRule{status.health.status}` |
| Flux | `kustomize.toolkit.fluxcd.io/v1` | `kustomizations` | `Kustomization` | `ConditionRule{Ready}` + `SuspendPath` |
| Flux | `helm.toolkit.fluxcd.io/v2` | `helmreleases` | `HelmRelease` | `ConditionRule{Ready}` + `SuspendPath` |
| Prometheus operator | `monitoring.coreos.com/v1` | `prometheuses` | `Prometheus` | `ConditionRule{Available}` |
| Prometheus operator | `monitoring.coreos.com/v1` | `servicemonitors` | `ServiceMonitor` | none — counted only |

Rule details that are not obvious:

- **Longhorn `status.robustness`**: `healthy` → healthy; `degraded` → unhealthy;
  `faulted` → unhealthy; `unknown` → unknown. A detached volume reports
  robustness `unknown`, which is why `unknown` must be a non-problem state — an
  idle PVC is not an incident.
- **Argo CD `status.health.status`**: `Healthy` → healthy; `Progressing` →
  progressing; `Degraded`/`Missing` → unhealthy; `Suspended` → suspended;
  `Unknown` → unknown. Sync status is deliberately **not** read: `OutOfSync` is
  drift, which is the next F slice, and flagging it here would make every
  pending deploy look like a failure.
- **Flux `spec.suspend: true`** → suspended regardless of its stale `Ready`
  condition. A suspended reconciler is a deliberate operator choice, and the
  roadmap's principles require parked states be understood rather than paged on.
- **A namespaced `Issuer` is included, not just `ClusterIssuer`.** A broken
  namespaced `Issuer` breaks every `Certificate` in its namespace, and it is the
  more common shape in application namespaces. It is also the one adapter row
  that exercises namespaced-vs-cluster-scoped handling within a single operator.
- **`ServiceMonitor` has no `.status` at all.** It is counted so the report can
  say the Prometheus operator is installed and how much it is scraping; judging
  it would mean inventing a health signal that does not exist. Its `KindReport`
  carries `Judged: false` so the report never implies an assessment happened.
- **Prometheus `Available` condition** exists in prometheus-operator ≥ 0.68. On
  older versions the condition is absent, so the rule yields `unknown` and the
  resource is counted, not flagged — the correct degradation.

Adding an operator later is one table row plus its fixture test. That is the
whole point of the slice.

### `internal/collect` — the I/O

```go
// OperatorResources gates each adapter on discovery, then lists the ones the
// cluster actually serves. Read-only: List calls only.
func OperatorResources(ctx context.Context, disco discovery.DiscoveryInterface,
    dyn dynamic.Interface, adapters []operators.Adapter, namespace string) []operators.Fetched
```

Behaviour, in order:

1. **Discovery gate.** One `ServerGroups`/`ServerResourcesForGroupVersion` pass
   builds the set of served group/version/resources. An adapter whose group is
   not served is skipped entirely — **zero API calls, no error, no report
   entry**. A cluster running none of the six produces an empty report and one
   discovery round trip.
2. **Version tolerance.** The adapter names a preferred version; if the cluster
   serves the group at a different version (Longhorn `v1beta1` vs `v1beta2`),
   the served one is used and recorded in `APIVersions`. A group served at no
   version kubeagent recognizes is reported installed with zero resources
   assessed, rather than silently dropped.
3. **Namespace scoping.** Discovery reports `Namespaced` per resource.
   Namespaced resources honour scan's `-n`; cluster-scoped ones
   (`ClusterIssuer`) are always listed cluster-wide, matching how `Nodes` already
   ignores the namespace filter.
4. **Failure isolation.** A `Forbidden` error on one resource sets that
   operator's `Forbidden` flag and continues to the next; every other error is
   recorded against that one adapter. One broken CRD never fails the scan, and
   never fails another operator.

Discovery needs no RBAC grant: the default `system:discovery` ClusterRole is
bound to `system:authenticated` on every conformant cluster.

### `internal/cluster` — client construction

`NewClient` returns `*kubernetes.Clientset` and its kubeconfig resolution is
inlined. This slice extracts that resolution into an unexported
`restConfig(kubeconfigPath, contextName) (*rest.Config, error)` and adds:

```go
// NewDynamicClients builds the dynamic and discovery clients for the same
// kubeconfig/context NewClient would use. Contacts no API server.
func NewDynamicClients(kubeconfigPath, contextName string) (dynamic.Interface, discovery.DiscoveryInterface, error)
```

`NewClient` and `NewInClusterOrKubeconfig` keep their exact signatures — every
existing caller is untouched, and the config-resolution logic exists once.

### CLI and report

- New flag `--operators` on `scan`, off by default, matching `--certs` /
  `--dns-health` / `--control-plane-health`. Env override
  `KUBEAGENT_OPERATORS` for parity with the other opt-ins.
- The dynamic and discovery clients are built **only when the flag is set** — a
  default scan constructs nothing new and issues no discovery call.
- `report.Input` gains `Operators *operators.Report`; JSON gains a top-level
  `operators` object; text output gains one advisory section after the existing
  advisory sections.

Text shape, deliberately compact — one line per kind, since counts are per kind:

```text
Operators (advisory):
  cert-manager (cert-manager.io/v1)
    Certificate      12 healthy, 1 unhealthy
      ✗ shop/web-tls        Ready=False: order is pending
    Issuer            3 healthy
    ClusterIssuer     1 healthy
  Argo CD (argoproj.io/v1alpha1)
    Application      48 healthy, 2 progressing
  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)
    Kustomization     9 healthy, 1 suspended
    HelmRelease       4 healthy
  Prometheus operator (monitoring.coreos.com/v1)
    Prometheus        1 healthy
    ServiceMonitor  214 (not assessed)
```

A kind with zero resources is omitted. An operator whose every kind is empty is
still listed with its API version, because "installed and idle" is a different
answer from "not installed" — the latter omits the operator entirely.

### Bounding

An Argo CD estate can hold thousands of `Application`s, and `--operators` must
not turn a scan into a wall of text or an unbounded allocation.

- Counts in `Counts` are always exact — they come from the listed length, not
  the printed list.
- `Unhealthy` is capped at **20 per operator**, ordered by namespace then name
  for determinism, with the remainder reported as `Truncated`. Text output
  renders `… +N more unhealthy` so truncation is never silent.
- Only `unhealthy` resources are listed. Progressing, suspended, and unknown
  resources are counted, never enumerated: they are states, not incidents.

### Verdict and security boundaries

- **Advisory only.** The operator report never affects `Healthy`/`Degraded`,
  exactly like `--certs` and `--disk-usage`. A third-party CRD's opinion of
  itself — read through field paths kubeagent infers — must not drive
  kubeagent's headline verdict.
- **Read-only.** `list` on the operator groups. No `get` on individual objects,
  no watch, no writes, no new verb anywhere.
- **The report shows metadata and state only.** Namespace, name, kind, state,
  and the operator's own condition reason. It must **never** print a CR's `spec`
  or arbitrary `status` content: an Argo CD `Application` carries a Git repo URL
  that can embed a token, a cert-manager `Issuer` references ACME account keys,
  and a CNPG `Cluster` names backup credentials. The reduction to the
  `Resource` struct in `collect` is the enforcement point — the raw
  `unstructured` object never reaches `report`.
- **Condition reasons are truncated** to a single line and length-capped before
  they enter a `Resource`, so an operator that stuffs a multi-kilobyte message
  into a condition cannot flood the report.
- **Not sent anywhere new.** The operator report is part of the scan result, so
  `--explain` sees it like any other finding. `--fix` never sees it: no CRD is
  in the allowlist, and none will be added by this slice.

### New RBAC (opt-in, scan-only)

`deploy/rbac-operators.yaml`: a separate ClusterRole plus ClusterRoleBinding
granting `list` on the six groups' resources, following `rbac-logs.yaml`'s shape
— a standalone add-on bound to the same ServiceAccount, **not** an aggregation
into the base role, so applying `deploy/` without it changes nothing.

This is a **scan-only add-on and gets no Helm values**, exactly as
`rbac-logs.yaml` does: the chart deploys the `watch` daemon, and the daemon does
not read operator CRDs in this slice. Adding chart values for a flag the chart
cannot set would be dead configuration. It also means **the chart needs no
version bump for this slice**.

Most human kubeconfigs already carry these permissions, so a `kubectl`-style
user needs no add-on at all. Absent the grant, `--operators` still runs:
discovery works for every authenticated user, so the report names which
operators are installed and marks each kind `Forbidden` — a genuinely useful
answer rather than an error.

## Error handling summary

| Situation | Behaviour |
| --- | --- |
| Operator group not served | Adapter skipped. No API call, no error, no report entry. |
| Group served, version unrecognized | Operator reported installed, resources not assessed. |
| `list` forbidden | That operator marked `Forbidden`, scan continues. |
| Other list error on one resource | Recorded against that adapter only. |
| CR missing the rule's field/condition | `StateUnknown` — counted, never flagged. |
| Field present with an unrecognized value | `StateUnknown`. Never `unhealthy`. |
| No known operator installed | Empty report; the section is omitted entirely. |

## Testing

The dynamic approach trades compile-time field checking for runtime string
paths. The mitigation is structural: **an adapter table row without a fixture
test is incomplete work**, not a style preference.

- **Per-adapter fixture tests** in `internal/operators`. Each of the nine table
  rows gets healthy, unhealthy, and missing-field fixtures built from CR shapes
  as each project documents them. A shared table-driven test over all adapters
  is *additional*, not a substitute — it cannot catch a wrong field path,
  because a wrong path and an absent field are indistinguishable to it.
- **`internal/collect`** against `dynamic/fake` plus a fake discovery: the
  absent-group skip (asserting **zero** dynamic calls), version fallback,
  namespace scoping for namespaced vs cluster-scoped resources, `Forbidden`
  isolation, and the 20-item cap with correct `Truncated`.
- **`internal/report`**: the new section's text rendering, its omission when the
  report is empty, and a golden-output regeneration.
- **Chaos scenario 16**, on the existing Kind cluster: install real cert-manager
  (one manifest), create a `Certificate` against an `Issuer` that cannot
  possibly succeed, and assert `--operators` names it with `Ready=False` while
  the cluster verdict stays driven by core workloads. Then apply a CRD kubeagent
  has no adapter for and assert it is absent from the report — proving the gate
  in both directions. Finally assert no report line carries CR spec content.

## Deliverables

- `internal/operators/` — `operators.go` (types, `Assess`), `rules.go`,
  `adapters.go` (the table), and per-adapter fixture tests.
- `internal/collect` — `OperatorResources`.
- `internal/cluster` — `restConfig` extraction plus `NewDynamicClients`.
- `main.go` — `--operators` flag, env override, usage line, wiring.
- `internal/report` — `Operators` field, text section, JSON, golden update.
- `deploy/rbac-operators.yaml` — scan-only add-on, no Helm values, no chart bump.
- Docs: a `website/docs/features/operators.md` page, `deploy/README.md`
  section, CHANGELOG entry, and the roadmap's Theme F line updated to record the
  slice.
- `chaos/run.sh` scenario 16 and its `chaos/README.md` row.

## Open risks, stated rather than hidden

- **`--operators` is the 15th `scan` flag.** The usage string already exceeds
  700 characters on one line. This slice adds to a known problem it does not
  fix; Theme H's Cobra migration is where that gets resolved.
- **Field paths drift with operator releases.** The `unknown`-not-`unhealthy`
  default bounds the damage to a missing signal rather than a false alarm, and
  the fixture tests make a drift visible the moment someone updates a fixture,
  but kubeagent cannot detect drift on a cluster it has never seen.
- **cert-manager in the chaos gate adds install time** to an already long run.
  It is worth it: a synthetic CRD would exercise the dynamic path without
  proving a single real adapter is correct.
