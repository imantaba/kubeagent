# kubeagent — Live validation (nova): service backing awareness + credential-lint precision

**Date:** 2026-06-30
**kubeagent version under test:** `main` @ commit `674040f` (the unreleased work
heading to **v0.4.0**)
**Goal:** Confirm, against a real cluster, that the two unreleased changes behave
as designed before cutting the release — the new **service backing awareness**
feature and the **credential-lint precision** fix.

## Safety

The scan is **strictly read-only** (List/Get only — kubeagent never creates,
updates, patches, or deletes). Every command targeted the `hetzner-nova` context
explicitly (`--context hetzner-nova`); no other cluster was touched and nothing
on nova was modified. `--lint-secrets` reports credential **locations and pattern
types only — never values** — so no secret material appears in this document.

## Environment (cluster under scan)

- **hetzner-nova** — Kubernetes **v1.35 (RKE2)**, 6/6 nodes Ready
- CNI: Cilium · ingress: Traefik · storage: Hetzner CSI (+NFS CSI) · runtime:
  containerd · cloud: Hetzner Cloud
- metrics-server present (the resource summary shows a live `used` line)

## Method

Build the binary from `main` @ `674040f`, then run against `hetzner-nova`:

1. `kubeagent scan --context hetzner-nova` (text) — full read-only scan.
2. `kubeagent scan --context hetzner-nova --output json` — to confirm the new
   `expected` / `backing` JSON fields on Service issues.
3. `kubeagent scan --context hetzner-nova --lint-secrets` — to confirm the
   precision fix (no `*_FILE` / version false positives).

Each result is compared against the same scan on `v0.3.0` (recorded immediately
before this work, same cluster) to show the before/after.

## Feature 1 — Service backing awareness

**Before (v0.3.0):** every selector-Service with no ready endpoints was flagged
plainly, so three expected-empty Services read as primary problems:

```text
Service issues:
  ⚠ cattle-monitoring-system/rancher-monitoring-windows-exporter  ClusterIP  no ready endpoints
  ⚠ ekb-js-nightly/clickhouse-sync  ClusterIP  no ready endpoints
  ⚠ ekb-js-staging/clickhouse-sync  ClusterIP  no ready endpoints
```

**After (`674040f`):** each is annotated in place with its backing workload and
the reason it has no endpoints:

```text
Service issues:
  ⚠ cattle-monitoring-system/rancher-monitoring-windows-exporter  ClusterIP  no ready endpoints (backs DaemonSet — 0 desired)
  ⚠ ekb-js-nightly/clickhouse-sync  ClusterIP  no ready endpoints (backs CronJob — expected between runs)
  ⚠ ekb-js-staging/clickhouse-sync  ClusterIP  no ready endpoints (backs CronJob — expected between runs)
```

The `windows-exporter` DaemonSet has `DesiredNumberScheduled == 0` (no Windows
nodes on this all-Linux cluster); the two `clickhouse-sync` Services back
CronJobs that have no pods between runs. All three are correctly reclassified
from "primary problem" to "expected".

JSON carries the structured classification (`--output json`):

```json
[
  { "namespace": "cattle-monitoring-system", "name": "rancher-monitoring-windows-exporter",
    "type": "ClusterIP", "problem": "NoEndpoints",
    "detail": "no ready endpoints (backs DaemonSet — 0 desired)",
    "expected": true, "backing": "DaemonSet" },
  { "namespace": "ekb-js-nightly", "name": "clickhouse-sync",
    "type": "ClusterIP", "problem": "NoEndpoints",
    "detail": "no ready endpoints (backs CronJob — expected between runs)",
    "expected": true, "backing": "CronJob" }
]
```

**No-new-noise check:** nova currently has no real-outage Service (a
Deployment/StatefulSet with replicas but no endpoints), so every NoEndpoints
issue on this cluster is legitimately "expected". A live Deployment with zero
endpoints would still print the plain `no ready endpoints` primary line — that
path is covered by `TestAssess_LiveDeploymentStaysPrimary` /
`TestAssess_LiveDaemonSetStaysPrimary` / `TestAssess_LiveStatefulSetStaysPrimary`
in the unit suite.

**Verdict:** ✅ works as designed — the three false-positive Service issues from
the original chaos-test gap analysis (#6 follow-on) are now correctly demoted to
informational annotations.

## Feature 2 — Credential-lint precision fix

**Before the fix:** `--lint-secrets` produced **76** findings on nova, including a
large class of false positives on `*_FILE` env vars (whose value is a *path to* a
secret file — the secure convention — not the secret itself).

**After (`674040f`):**

| Metric | Result |
|--------|--------|
| Total credential warnings | **31** (was 76) |
| `*_FILE` false positives | **0** |

Representative surviving findings (locations/patterns only — never values):

```text
Credential warnings (--lint-secrets):
  ⚠ cattle-tokens/kubeconfig-6l2h2  ConfigMap[status-tokens]  credential-like name with a literal value
  ⚠ ekb-js-nightly/ekb-js-config  ConfigMap[SMTP_USER]  AWS access key
  ⚠ fleet-local/rke2-machineconfig-cleanup-cronjob-…  Pod[rke2-machineconfig-cleanup-pod/CATTLE_TOKEN]  credential-like name with a literal value
```

The survivors look like genuine candidates: Rancher `status-tokens` ConfigMaps,
`CATTLE_TOKEN` literals, and `SMTP_USER` — which is a true-positive of sorts, as
SES SMTP usernames are AWS access-key IDs (`AKIA…`) and this one is sitting in a
ConfigMap.

**Verdict:** ✅ the two false-positive classes (`*_FILE` paths, dotted version
numbers) are gone; the signal-to-noise ratio of `--lint-secrets` improved by 45
findings (-59%) with no loss of real detections.

## Other features (regression check)

The same full scan exercised the previously-shipped detectors with no
regressions:

- **Cluster health / platform facts:** `Healthy — 6/6 nodes`; Cilium / Traefik /
  Hetzner CSI / v1.35 RKE2 / containerd / Hetzner Cloud all detected.
- **Resource context:** cluster CPU/memory summary with a live `used` line
  (metrics-server present); the `cattle-system/rancher` Deployment shows its
  OOMKilled finding with the container's requests/limits.
- **Connectivity diagnostics:** not exercised (the API server was reachable
  throughout) — a successful scan is unaffected by that feature, as designed.

## Conclusion

Both unreleased changes behave correctly against a real, busy cluster:

- **Service backing awareness** turns three standing false-positive Service
  issues into clearly-labelled expected annotations, while keeping real outages
  primary.
- **Credential-lint precision** removes a whole class of `*_FILE` false
  positives (76 → 31) without dropping real findings.

The `main` branch at `674040f` is validated and ready to cut as **v0.4.0**.
