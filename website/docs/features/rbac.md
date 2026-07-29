# Least-privilege RBAC

Two questions come up before every kubeagent install: "what does `--certs`
cost me?" and "may this identity actually run it?" `kubeagent rbac` answers
both without a trip to the cluster's audit log — `print` shows the minimal
role a profile or feature list needs, and `check` asks the API server
directly whether the identity in front of it may do each thing.

Both verbs make **no LLM call** and are read-only toward the cluster in the
sense that matters everywhere else in these docs: no create, update, patch,
or delete. `rbac check` is the one documented exception to "kubeagent issues
only GET/LIST calls" — see [SelfSubjectAccessReview](#a-note-on-selfsubjectaccessreview)
below for exactly what that exception is and is not.

## `kubeagent rbac print`

`print` renders the `ClusterRole` a profile or feature list needs, so it can
be applied — or reviewed — before kubeagent ever runs.

| Profile | Verbs on core | Who needs it |
|---------|---------------|--------------|
| `scan` (default) | `get`, `list` | a one-shot `kubeagent scan` |
| `watch` | `get`, `list`, `watch` | the `kubeagent watch` daemon, which opens informers |
| `full` | core plus every add-on's rules, at `get`/`list` | reviewing the maximum this binary ever asks for |

```bash
kubeagent rbac print --profile scan | kubectl apply -f -
```

Flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--profile` | `scan` | `scan` \| `watch` \| `full` |
| `--features` | (empty) | comma-separated feature names, instead of a profile — the escape hatch for a role narrower than any built-in profile, e.g. `--features core,certs` |
| `--role-name` | `kubeagent` | `metadata.name` of the printed `ClusterRole` |
| `--output` | `yaml` | `yaml` \| `json` — `json` prints the resolved `Rule` list, not a `ClusterRole` document, which is what a script diffing grants against a live role wants |

`--features` wins when both are given: naming features is the more specific
request. A feature list is resolved to rules the same way a profile is, so
`--features core,certs,logs` prints exactly the role a scan that only ever
runs with `--certs --logs` needs — nothing `--operators` or `--drift` would
add.

## `kubeagent rbac check`

`check` takes the same `--profile`/`--features` selection, but instead of
printing a role, it creates a
[`SelfSubjectAccessReview`](#a-note-on-selfsubjectaccessreview) for every
action each selected feature needs, against the identity the kubeconfig
already carries. Nothing is printed and nothing is applied — it is a
question, not a change.

| Flag | Default | Meaning |
|------|---------|---------|
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | path to kubeconfig |
| `--context` | current-context | kubeconfig context to use |
| `--profile` | `full` | `scan` \| `watch` \| `full` — note the default differs from `print`: checking defaults to asking about everything |
| `--features` | (empty) | comma-separated feature names, instead of a profile |
| `--output` | `text` | `text` \| `json` |

It exits `1` when any checked feature is blocked, and `0` when every checked
feature is permitted — the same shape `kubeagent gate` uses, so a CI step can
branch on it directly without parsing output.

### `--output text`

Run against a real identity holding only the `scan` profile's role — core is
granted, three add-ons are not:

```bash
kubeagent rbac check --features core,certs,logs,diskusage
```

```text
  ok       core
  blocked  certs (--certs) — needs list secrets
  blocked  logs (--logs) — needs get pods/log
  blocked  diskusage (--disk-usage) — needs get nodes/proxy

3 of 4 features are blocked. Print the role they need:
  kubeagent rbac print --profile full
```

Exit code: `1`.

### `--output json`

The same check, as JSON:

```bash
kubeagent rbac check --features core,certs,logs,diskusage --output json
```

```json
[
  { "name": "core", "summary": "the inventory every command reads: pods, nodes, workloads, events, services, PVCs and the rest", "allowed": true },
  { "name": "certs", "flag": "--certs", "summary": "TLS certificate expiry, read from the public tls.crt of kubernetes.io/tls Secrets", "allowed": false, "missing": ["list secrets"] },
  { "name": "logs", "flag": "--logs", "summary": "the last lines of a crashed container's previous log, to name the cause", "allowed": false, "missing": ["get pods/log"] },
  { "name": "diskusage", "flag": "--disk-usage", "summary": "node filesystem and inode pressure, read from the kubelet summary API", "allowed": false, "missing": ["get nodes/proxy"] }
]
```

Every `missing` entry is phrased by kubeagent from its own table — `Action.String()`
in `internal/rbacprofile` — never from the API server's own denial message,
which can embed the requesting identity (see the note below).

## The feature table

Every feature kubeagent ships, what flag turns it on, and exactly what it
grants beyond core. Generated from `internal/rbacprofile.Features()` — the
same table `print` and `check` both read — so this table cannot say
something the code doesn't do. (`kubeagent rbac print --profile full --output
json` was tried first for this, but it returns the profile's *merged* rule
list — several features share a merge key, e.g. `diskusage`, `kubelethealth`
and `dnshealth` all collapse toward the same `get`-only rule — which loses
the per-feature attribution this table needs. The rows below come from
walking the table directly instead.)

| Feature | Flag | Grants |
|---------|------|--------|
| `core` | (always on) | `get`/`list` on pods, nodes, services, configmaps, events, PVCs, PVs, namespaces, resourcequotas, Deployments, ReplicaSets, StatefulSets, DaemonSets, Jobs, CronJobs, EndpointSlices, NetworkPolicies, IngressClasses, Ingresses, StorageClasses, Leases, PodDisruptionBudgets, HorizontalPodAutoscalers, and both webhook-configuration kinds |
| `diskusage` | `--disk-usage` | `nodes/proxy: get` |
| `kubelethealth` | `--kubelet-health` | `nodes/proxy: get` (shares `diskusage`'s manifest — one grant covers both) |
| `dnshealth` | `--dns-health` | `pods/proxy: get` |
| `controlplane` | `--control-plane-health` | `/readyz: get` |
| `certs` | `--certs` | `secrets: list` |
| `logs` | `--logs` | `pods/log: get` |
| `operators` | `--operators` | `list` on `cert-manager.io/{certificates,issuers,clusterissuers}`, `postgresql.cnpg.io/clusters`, `longhorn.io/volumes`, `argoproj.io/applications`, `kustomize.toolkit.fluxcd.io/kustomizations`, `helm.toolkit.fluxcd.io/helmreleases`, `monitoring.coreos.com/{prometheuses,servicemonitors}` |
| `gitops` | `--drift` | `list` on `argoproj.io/applications`, `kustomize.toolkit.fluxcd.io/kustomizations`, `helm.toolkit.fluxcd.io/helmreleases` (a subset of `operators`'s rules) |
| `capacity` | `--capacity` | nothing beyond core |
| `security` | `--security` | nothing beyond core |
| `pvcreclaim` | `--pvc-reclaim` | nothing beyond core |
| `credlint` | `--lint-secrets` | nothing beyond core |
| `cronjobs` | `--include-cron` | nothing beyond core |
| `restarts` | `--include-restarts` | nothing beyond core |

`operators` and `gitops` are `list`-only and scan-only: the watch daemon
never reads these custom resources, so neither is wired into the Helm chart —
adding a chart toggle for a grant the daemon never uses would be the opposite
of least privilege.

## Blind spots

A missing grant never fails a scan. It degrades it, and it says so. Five
reads are checked directly in `internal/scan` — `secrets` (`certs`),
`pods/log` (`logs`), `nodes/proxy` (`diskusage`/`kubelethealth`), `pods/proxy`
(`dnshealth`), and `/readyz` (`controlplane`) — and a refusal on any of them
is named the same way everywhere kubeagent reports:

- `scan`'s default text output prints a `BLIND SPOTS` section:
  ```text
  BLIND SPOTS
    • secrets: forbidden: kubeagent's credentials may not list secrets
  ```
- `scan --output json` carries the same list under `"blindSpots"`, omitted
  entirely when nothing was refused.
- `scan --output html`, `kubeagent gate` (as `inconclusive`), `kubeagent tui`,
  and the MCP tools each surface the same underlying record in their own
  shape.

In every case the scan still exits `0`: a partial read degrades what
kubeagent could see, it is not a failure of the tool itself. If a red
`kubeagent gate` needs to fail the build on a blind spot rather than pass
through it, that is `gate`'s own contract — see
[CI/CD gate](ci-gate.md#why-exit-2-exists-and-is-not-opt-in) for exit `2` and
`--allow-partial-read`.

`--operators` and `--drift` are a narrower case: a `list` refusal on one
custom-resource kind stays inside that flag's own section — reported as
`forbidden` next to the operator or reconciler it names — rather than joining
the unified blind-spot list above, because kubeagent can still answer *which*
operator or reconciler is installed (API discovery is open to every
authenticated user) even without permission to list its resources. See
[Operator health](operators.md) and [GitOps drift](gitops-drift.md) for how
each renders that case.

## A note on `SelfSubjectAccessReview`

`rbac check` creates
[`SelfSubjectAccessReview`](https://kubernetes.io/docs/reference/kubernetes-api/authorization-resources/self-subject-access-review-v1/)
objects. That is technically a POST — the one place in kubeagent, outside
`--fix`, that issues one — but it is a virtual resource: the API server
evaluates the request and persists nothing, which is the same API `kubectl
auth can-i` uses. It needs no grant of its own to run, because the built-in
`system:basic-user` `ClusterRole` is bound to the `system:authenticated`
group, and every identity that can reach the API server at all is
authenticated. Nothing in the cluster changes as a result of asking.

kubeagent never reads the response's `Status.Reason`. A real API server fills
it with the authorizer's own message, which names the requesting identity —
an IAM ARN, an OIDC email, an internal DNS name — and under webhook
authorization can carry arbitrary third-party text. Every `missing` entry
`rbac check` prints instead comes from `Action.String()`, phrased from
kubeagent's own table, never from anything the server said.

## A note on generated manifests

Every manifest under `deploy/*.yaml` and the Helm chart's `ClusterRole`
(`deploy/helm/kubeagent/templates/clusterrole.yaml`) are rendered from this
same table in `internal/rbacprofile` — they are not maintained by hand. To
change a grant, edit the table in `internal/rbacprofile/profile.go` and
regenerate:

```bash
go test ./internal/rbacprofile -run TestGeneratedManifests -update
```

A golden test fails the build if a manifest and the table it should come from
ever drift apart. See [`deploy/README.md`](https://github.com/imantaba/kubeagent/blob/main/deploy/README.md)
for what each generated file grants, and use `kubeagent rbac print` above for
a role narrower than any of the shipped manifests.
