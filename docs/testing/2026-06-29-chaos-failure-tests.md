# kubeagent — Chaos / failure-injection test results

**Date:** 2026-06-29
**kubeagent version under test:** `v0.2.0` (commit `8b8724b`)
**Goal:** Inject realistic cluster failures and record exactly what `kubeagent scan`
reports, to validate detection and surface gaps.

## Safety

All chaos ran on a **disposable local cluster** (`kind`, Docker-in-Docker), on its
own `kind-chaos` kubeconfig context. No production or real cluster was touched:
every chaos command and every `kubeagent` run targeted `--context kind-chaos`
explicitly. The cluster was deleted at the end and the previous kubeconfig
`current-context` restored.

## Environment

- **kind** v0.30.0 → Kubernetes **v1.34.0**, 1 control-plane + 2 workers
- CNI: kindnet · StorageClass: `local-path` · container runtime: containerd
- **No metrics-server** (so the resource summary's usage line is expected to read
  "unavailable")

## Method

For each scenario: inject the fault on `kind-chaos`, wait for the symptom to
appear, run `kubeagent scan` (scoped or full as appropriate), and record the
output verbatim. Between scenarios the injected objects were removed (or the
namespace deleted) so each report is attributable.

## Baseline (healthy cluster)

```text
Cluster: Healthy — 3/3 nodes Ready
Platform: local-path storage · Kubernetes v1.34 · containerd

Resources (cluster):
  CPU     36.0 cores · req 1.1 (3%) · lim 0.3 (0%)
  Memory  117Gi · req 390Mi (0%) · lim 490Mi (0%)
  (usage: metrics-server unavailable)

No issues found. ✅
```

Note: kindnet is not in the CNI heuristic map, so the CNI fact is correctly
omitted; `local-path` storage and the version/runtime are detected. The
`(usage: metrics-server unavailable)` line confirms the metrics graceful-
degradation path.

## Results summary

| # | Scenario | Verdict | What kubeagent reported |
|---|----------|---------|--------------------------|
| 7 | OOMKilled workload | ✅ detected | Degraded workload + `OOMKilled` finding with the container's requests/limits |
| 5 | Broken DNS (CoreDNS crash) | ✅ detected (P1) | `Cluster: Degraded` → P1 `system kube-system/coredns 1/2 Degraded` + CrashLoop |
| 9 | Faulty rolling deployment | ✅ detected | `ImagePullBackOff` finding flags the stuck rollout even while replicas read `3/3` |
| 3 | Disk full on control plane (node condition) | ✅ detected (P1) | P1 `node … SchedulingDisabled` + `Unschedulable` Pending pod (cordon used as a safe stand-in for DiskPressure/NotReady) |
| 4 | NetworkPolicy blocking traffic | ⚠️ symptom only | Shows the Degraded/not-Ready workload, **not** the NetworkPolicy cause |
| 6 | Cloud load balancer failure | ❌ gap | Service stuck `EXTERNAL-IP <pending>` — kubeagent does not scan Services |
| 8 | Accidental namespace deletion | ❌ gap | After deletion the scan reads "No issues found" — no expected-state tracking |
| 10 | Security credential leak | ❌ gap | A Secret with a leaked-looking key is ignored — kubeagent is not a secret scanner |
| 1 | etcd quorum loss | ⚠️ connect-error | Not run: kills the API server → kubeagent emits a connection error, not a diagnosis |
| 2 | Expired certificates | ⚠️ connect-error | Not run: impractical to force; same outcome as #1 |

## Detail (detected scenarios)

### #7 — OOMKilled workload

Injected: a Deployment running `tail /dev/zero` with `memory: 16Mi` limit.

```text
⚠ chaos/memory-hog  Deployment  0/1 Degraded  · 3 restarts, last 36s ago
    ⚠ OOMKilled: Container exceeded its memory limit and was killed
      resources: memory req=16Mi limit=16Mi · cpu req=10m limit=100m
```

The req==limit==16Mi makes the remedy obvious: there is no headroom; raise the
limit. This is the v0.2.0 resource-context enrichment working as designed.

### #5 — Broken DNS (CoreDNS crash)

Injected: an invalid `Corefile` in the `kube-system/coredns` ConfigMap, then a
rollout restart → new CoreDNS pods crash on a syntax error.

```text
Cluster: Degraded — 3/3 nodes Ready
  ⚠ system kube-system/coredns 1/2 Degraded
...
⚠ kube-system/coredns  Deployment  1/2 Degraded  · 10 restarts
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
```

The infra failure is correctly elevated to the **P1 cluster-health verdict
line**, ahead of any workload (P2) issue.

### #9 — Faulty rolling deployment

Injected: a healthy Deployment, then `set image` to a non-existent tag.

```text
⚠ chaos/web  Deployment  3/3 Running
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
    web-…  0/1  Pending   (the new, broken pod)
    web-…  1/1  Running   (old pods still serving)
```

Notable: replica count still reads `3/3` because the rolling update keeps the old
pods until the new ones are Ready. kubeagent still flags the workload via the
`ImagePullBackOff` finding — so a stuck rollout that *looks* healthy by count is
not missed.

### #3 — Node condition (disk-full family)

Injected: `kubectl cordon` of a worker (a safe, reversible stand-in for the
DiskPressure/NotReady conditions a full disk would raise) plus a pod with an
impossible CPU request.

```text
Cluster: Degraded — 3/3 nodes Ready
  ⚠ node chaos-worker SchedulingDisabled
...
⚠ chaos/too-big  Deployment  0/1 Degraded
    ⚠ Unschedulable: No node can schedule this pod (resources, taints, or affinity)
```

The node condition is a **P1** signal; the unschedulable pod is a P2 finding.

### #4 — NetworkPolicy blocking traffic (symptom only)

Injected: a `deny-all` NetworkPolicy plus an app whose readiness probe fails
(standing in for a blocked dependency).

```text
⚠ chaos/api  Deployment  0/2 Degraded
    api-…  0/1  Running   (Running but never Ready)
```

kubeagent reports the **symptom** (a degraded, not-Ready workload) but says
nothing about the NetworkPolicy. It does not read `NetworkPolicy` objects, so the
root cause is invisible — see the roadmap below.

## Gap analysis → roadmap

The chaos run confirmed kubeagent's strength (workload / pod / node / kube-system
failures, with correct P1/P2 prioritization) and four coherent blind spots. Each
is an independent feature with its own spec → plan → build cycle:

1. **Service / LoadBalancer health** — detect Services with no Endpoints and
   `LoadBalancer` Services stuck without an external address (#6).
2. **NetworkPolicy awareness** — when a workload is degraded/not-Ready, note
   whether a restrictive NetworkPolicy selects its pods, turning #4 from a
   symptom into a root-cause hint.
3. **Connectivity / control-plane diagnostics** — turn an API-connection failure
   (#1/#2) into a clear, actionable message instead of a raw transport error.
4. **Secret / credential lint** — optional, opt-in scan for obviously leaked or
   risky credentials in Secrets / env (#10).

Expected-state tracking for #8 (namespace/workload deletion) is intentionally out
of scope for a stateless read-only scanner.
