# Workload Security Posture — Design

**Date:** 2026-07-11
**Status:** Approved

## Goal

Answer "which of my workloads are dangerously configured?" deterministically and
offline. A new opt-in `scan --security` pass reads the pods and services the scan
already collects and flags the high-signal hardening problems:

- **privileged / over-privileged containers** (privileged, host namespaces,
  hostPath, hostPort, dangerous added capabilities),
- **insecure container defaults** (runs as root, privilege escalation allowed,
  capabilities not dropped),
- **exposed services** (NodePort / LoadBalancer / externalIPs).

The workload checks are **aligned with the Kubernetes Pod Security Standards
(PSS)** baseline and restricted profiles — a curated, high-signal subset, *not* a
full PSS conformance implementation. Findings are labelled by origin
(`baseline` / `restricted` / `kubeagent`).

Read-only; **advisory** — never changes the cluster verdict,
`kubeagent_cluster_healthy`, or the exit code. Opt-in behind `--security`. **No
new RBAC**: pods and services are already in the read set.

## Non-goals (v1)

- **OPA Gatekeeper / Kyverno awareness** — surfacing installed policies and their
  violations is a separate mechanism (policy CRDs + per-engine violation
  reports); deferred to a v2.
- **Full PSS conformance** — the exotic controls (seccomp/SELinux/AppArmor
  profiles, sysctls, `/proc` mount type, allowed volume-type enumeration) are
  intentionally omitted as noise-not-signal for a troubleshooting CLI. We
  document the check set as "PSS-aligned," not conformant.
- Image CVE scanning, runtime/behavioral analysis, RBAC-permission auditing.
- Any `--fix` remediation — this pass is strictly read-only advisory.

## Data source

Pods (`corev1.Pod`) and Services (`corev1.Service`) are already collected by the
scan (pods for the workload diagnosis, services for `svchealth`); ReplicaSets
(`appsv1.ReplicaSet`) are already collected by `collect.CollectInventory` (used
to resolve Deployment ownership). The check is a pure function of those specs —
no new collector, no new API group, no new RBAC.

## Components

### `internal/secscan` (new, pure)

```go
// Assess flags high-signal, PSS-aligned security-posture problems in the given
// pods and services. replicaSets is used only to fold a Deployment's pods up to
// the Deployment for display. Pure: the caller supplies already-namespace-
// filtered inputs. Read-only.
func Assess(pods []corev1.Pod, services []corev1.Service, replicaSets []appsv1.ReplicaSet) []Finding

type Finding struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`            // controller owner name, else pod/service name
	Kind      string `json:"kind"`                // "Deployment" | "DaemonSet" | "Pod" | "Service" | ...
	Container string `json:"container,omitempty"` // container name; "" for pod-level or Service findings
	Profile   string `json:"profile"`             // "baseline" | "restricted" | "kubeagent"
	Check     string `json:"check"`               // machine-stable, e.g. "Privileged"
	Detail    string `json:"detail"`              // human-readable
}
```

**Check set (v1), by profile.** `Check` values are machine-stable:

`[baseline]` — genuinely dangerous:

| Check | Level | Fires when |
|-------|-------|-----------|
| `Privileged` | container | `securityContext.privileged == true` |
| `HostNamespaces` | pod | any of `hostNetwork` / `hostPID` / `hostIPC` is true (detail names which) |
| `HostPath` | pod | a `hostPath` volume is mounted (one finding per hostPath volume; detail names the path) |
| `HostPort` | container | a container port sets `hostPort` |
| `AddedCapability` | container | `capabilities.add` contains anything other than `NET_BIND_SERVICE` (detail lists them) |

`[restricted]` — hardening gaps (evaluated on **effective** settings; a
container's `securityContext` overrides the pod's):

| Check | Level | Fires when |
|-------|-------|-----------|
| `RunAsRoot` | container | neither container nor pod guarantees non-root: no `runAsNonRoot: true` in effect, or `runAsUser == 0` |
| `AllowPrivilegeEscalation` | container | `allowPrivilegeEscalation` is not explicitly `false` |
| `CapabilitiesNotDropped` | container | `capabilities.drop` does not include `ALL` |

`[kubeagent]` — beyond PSS:

| Check | Level | Fires when |
|-------|-------|-----------|
| `ExposedService` | service | `spec.type` is `NodePort` or `LoadBalancer`, or `spec.externalIPs` is non-empty (detail names type + port(s)) |

Effective-settings note: `Privileged`, `AllowPrivilegeEscalation`, and
capabilities are container-level only. `runAsNonRoot` / `runAsUser` fall back
from container `securityContext` to pod `securityContext`. Host namespaces and
`hostPath` are pod-level. Absent `securityContext` counts as the insecure default
for the restricted checks (that is the point of the restricted profile).

**Grouping / dedup.** A 10-replica Deployment must not emit ten identical
findings. `Assess` dedupes by `(namespace, workload, container, check)`, where
`workload` is the pod's **top-level** controller: the controlling
`ownerReference`, folded up one level when that owner is a `ReplicaSet` (resolved
via the `replicaSets` argument to the owning `Deployment`), so the labels read
`Deployment "api"` — consistent with how the rest of the scan names workloads.
DaemonSet/StatefulSet/Job pods already own their controller directly (no
intermediate ReplicaSet). Standalone pods key on their own name with `Kind`
`Pod`. `Workload`/`Kind` in each emitted Finding carry that resolved top-level
identity.

**Ordering.** Findings sort most-dangerous first: `baseline` → `restricted` →
`kubeagent`, then by namespace/workload/container, for stable output and tests.

### `internal/scan`

`Evaluate` gains a `security bool` option (set from the `--security` flag). When
true, it filters the collected pods/services to the in-scope namespaces, calls
`secscan.Assess`, and stores the result on `Result.SecurityIssues
[]secscan.Finding`. When false, the field stays nil and nothing is computed.

**System-namespace exclusion** lives here (not in `Assess`, which stays a pure
function of its inputs): when scanning **all** namespaces (`opts.Namespace ==
""`), pods/services in `kube-system`, `kube-node-lease`, and `kube-public` are
dropped before `Assess` — CNI, kube-proxy, and similar are legitimately
privileged, and flagging them is noise. An explicit `-n kube-system` overrides
the exclusion (the user asked for that namespace). The exclusion set is a small
documented constant.

Security findings are **advisory**: `SecurityIssues` is never fed into
`clusterhealth`/the verdict and never affects the exit code.

### `internal/report` (text + JSON)

- **Text:** a dedicated **`SECURITY`** section, rendered only when `--security`
  is set, printed *separately* from `NEEDS ATTENTION` (hardening posture is not
  an active outage). Grouped by namespace/workload, most-dangerous first, each
  line `[profile] <check> — <detail>`, followed by a one-line summary
  (`Security: N finding(s) across M workload(s)`). This summary does **not**
  contribute to the "Needs attention" verdict line.
- **JSON:** a `securityIssues` array of `Finding` objects
  (`json:"securityIssues,omitempty"` — absent when the flag is off or there are
  no findings).

Example text:

```text
SECURITY  (advisory — does not affect the cluster verdict)
payments/api  Deployment
  [baseline]   Privileged — container "app" runs privileged (full host access)
  [baseline]   AddedCapability — "app" adds SYS_ADMIN
  [restricted] RunAsRoot — "app" is not guaranteed to run as non-root
shop/admin  Service
  [kubeagent]  ExposedService — type LoadBalancer exposes port 80 externally

Security: 4 findings across 2 workloads
```

### `main.go` / CLI

Add a `-security` bool flag ("flag insecure workloads and exposed services
(read-only, advisory)"), pass it into the scan options and into `report.Input`.
No change to defaults — without the flag the scan behaves exactly as today.

## Scope boundaries

- Read-only; opt-in; advisory (no verdict / exit-code / `kubeagent_cluster_healthy`
  impact). Not wired into `--explain`. No daemon/`watch` gauge in v1 (possible
  v2; `watch` would otherwise compute security unconditionally).
- Curated PSS-aligned subset — documented as aligned, not conformant.
- No new RBAC, no new API groups, no new collectors.

## Testing

- `secscan.Assess` is a pure function — TDD, table-driven over fake pods/services
  built by test helpers. Every check gets a positive **and** a negative case.
- Effective-settings cases: container `securityContext` overriding pod
  `securityContext` for `RunAsRoot` (pod sets `runAsNonRoot`, container does not,
  and vice versa).
- Dedup: multiple pods of one controller owner collapse to one finding-set.
- `ExposedService`: NodePort / LoadBalancer / externalIPs positive, ClusterIP
  negative.
- The system-namespace exclusion is tested at the `scan` layer (all-namespaces
  drops `kube-system`; explicit `-n kube-system` keeps it).
- Report: text `SECURITY` section renders and is suppressed without the flag;
  JSON contains `securityIssues`; the all-clear and cluster verdict are
  unaffected by security findings.

## Docs

- `CHANGELOG.md` (`### Added`), `website/docs/features/diagnostics.md` (a
  "Security posture" subsection), `website/docs/roadmap.md` (Shipped bullet),
  `README.md` (one-line mention). Exact names to use verbatim: flag `--security`;
  JSON field `securityIssues`; profiles `baseline` / `restricted` / `kubeagent`;
  check values as tabled above.
