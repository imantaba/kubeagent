# Operator health (`--operators`)

`kubeagent scan --operators` reports what the operators you actually run say
about themselves — cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the
Prometheus operator — without kubeagent being compiled against any of their Go
APIs, and without a rebuild when you install one.

```bash
kubeagent scan --operators
```

```text
OPERATORS  (advisory — operator-reported state; no CR spec content)
  cert-manager (cert-manager.io/v1)
    Certificate     12 healthy, 1 unhealthy
      ✗ shop/web-tls  Ready=False: IssuerNotFound
    Issuer          3 healthy
    ClusterIssuer   1 healthy
  Argo CD (argoproj.io/v1alpha1)
    Application     48 healthy, 2 progressing
  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)
    Kustomization   9 healthy, 1 suspended
    HelmRelease     4 healthy
  Prometheus operator (monitoring.coreos.com/v1)
    Prometheus      1 healthy
    ServiceMonitor  214 (not assessed)
```

!!! note
    The example above is synthetic. Your cluster's output will reflect only
    the operators it actually serves.

The flag is off by default; set `KUBEAGENT_OPERATORS=true` to enable it
without passing `--operators` on every invocation.

## Discovery is the installation signal

An operator counts as installed when the API server *serves its API group* — not
because a Deployment happens to be named after it. If the group is absent,
kubeagent skips that adapter entirely: zero API calls, no error, and no line in
the report. A cluster running none of the six costs one discovery round trip.

That is also why nothing needs configuring. Install cert-manager tomorrow and
the next `--operators` scan picks it up.

## What is covered

| Operator | API group | Kinds | Signal |
| --- | --- | --- | --- |
| cert-manager | `cert-manager.io/v1` | `Certificate`, `Issuer`, `ClusterIssuer` | `Ready` condition |
| CloudNativePG | `postgresql.cnpg.io/v1` | `Cluster` | `Ready` condition |
| Longhorn | `longhorn.io/v1beta2` | `Volume` | `status.robustness` |
| Argo CD | `argoproj.io/v1alpha1` | `Application` | `status.health.status` |
| Flux | `kustomize.toolkit.fluxcd.io/v1`, `helm.toolkit.fluxcd.io/v2` | `Kustomization`, `HelmRelease` | `Ready` condition, `spec.suspend` |
| Prometheus operator | `monitoring.coreos.com/v1` | `Prometheus`, `ServiceMonitor` | `Available` condition; `ServiceMonitor` is counted, not judged |

A `ServiceMonitor` has no `.status` at all. It is counted so the report can say
the operator is installed and how much it is scraping — judging it would mean
inventing a health signal that does not exist, so its line says
`(not assessed)`.

Counts are per resource **kind**, not per operator: Flux's `Kustomization` and
`HelmRelease` roll up separately, and the 214 unjudged `ServiceMonitor`s above
are never averaged into the one `Prometheus` CR's health.

## What it deliberately does not do

- **It never drives the cluster verdict.** The section is advisory, like
  `--certs` and `--disk-usage`. A third-party CRD's opinion of itself, read
  through field paths kubeagent infers, must not decide whether your cluster is
  Healthy or Degraded — it never changes the exit code and never produces a
  Finding.
- **It does not compute GitOps drift.** Argo CD's `OutOfSync` and Flux's
  equivalent are deliberately not read: drift is a separate concern, and
  flagging it here would make every pending deploy look like a failure.
- **It never writes.** No CRD is in the `--fix` allowlist, and none will be.
- **A suspended reconciler is not an incident.** A Flux `Kustomization` with
  `spec.suspend: true` reports `suspended`, not `unhealthy` — its `Ready`
  condition went stale the moment somebody parked it on purpose.

## When kubeagent cannot tell

Every rule degrades to `unknown` rather than to `unhealthy`. If an operator
renames a field, or serves a CRD version kubeagent has not seen, the resource is
counted as `unknown` and never flagged. A drifted heuristic should report a
missing signal, not a false outage. A detached Longhorn volume is the everyday
case: it reports `robustness: unknown`, and an idle PVC is not an incident.

## What the report can contain

Metadata and state only: namespace, name, kind, state, and the operator's own
condition **reason**. Never a CR's `spec`, never arbitrary `status` content, and
never a condition's free-text `message`.

That boundary is deliberate. An Argo CD `Application` carries a Git repository
URL that can embed a token; a cert-manager `Issuer` references ACME account
keys; a CNPG `Cluster` names backup credentials; and cert-manager writes ACME
order URLs into condition messages. Only the CamelCase `reason` — a token by API
convention — is read, and it is trimmed to one line.

Large estates are bounded too: counts are always exact, but at most 20 unhealthy
resources are listed per kind, with the remainder reported as `… +N more
unhealthy` rather than silently dropped.

## RBAC

Most human kubeconfigs already allow these `list` calls. On a restricted context
or for the in-cluster ServiceAccount, apply the scan-only add-on:

```bash
kubectl apply -f deploy/rbac-operators.yaml
```

Without it, `--operators` still runs: API discovery is available to every
authenticated user, so the report names which operators are installed and marks
each kind as forbidden — a genuinely useful answer rather than an error.
