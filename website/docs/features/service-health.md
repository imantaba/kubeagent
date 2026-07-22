# Service health

`scan` flags Service-level problems that a pod scan alone misses.

## What is checked

### Selector-based Services with zero ready endpoints

A Service whose pod selector matches no ready pods — caused by a selector typo,
all backends being down, or a scaled-to-zero deployment. `kubeagent` reports
these under a **"Service issues"** section in both text and JSON output.

ExternalName and selectorless Services are skipped.

### LoadBalancer Services with no external address

A `LoadBalancer` Service that has been pending an external IP longer than
expected. `kubeagent` shows how long the Service has been waiting so you can
distinguish a normal provisioning delay from a stuck controller.

## Backing-awareness

A "no ready endpoints" finding whose backing workload expects no pods is
annotated so it does not read as a primary problem:

| Backing workload | Annotation |
|------------------|-----------|
| CronJob | `(backs CronJob — expected between runs)` |
| Job | `(backs Job — expected between runs)` |
| DaemonSet (desired 0) | `(backs DaemonSet — 0 desired)` |
| Deployment (replicas 0) | `(backs Deployment — scaled to 0)` |
| StatefulSet (replicas 0) | `(backs StatefulSet — scaled to 0)` |

A Deployment or StatefulSet with a non-zero replica count and no ready
endpoints **stays primary** — that is a real problem.

## Why a Service has no endpoints

For a broken (not expected-empty) Service, `scan` names the cause in the
finding's Detail line, using a read-only correlation over the collected pods
and node health:

- **Selector matches no pods** — the Service's `spec.selector` does not match
  any pod in the same namespace (likely a selector typo or a missing deployment).
- **Matching pods on a down node** — the selector matches pods, but they all sit
  on a node that is `NotReady` or whose kubelet has stopped heartbeating.
- **N matching pods, 0 ready** — the selector matches pods, but none of them are
  Ready (e.g., failing probes, CrashLoopBackOff, or Pending pods waiting to
  schedule).

Example Detail values:

```text
no ready endpoints — the selector matches no pods
no ready endpoints — matching pods on down node worker-2 (NotReady)
no ready endpoints — matching pods on 2 down nodes
no ready endpoints — 3 matching pods, 0 ready
```

This is a read-only enrichment; `scan` never writes to the cluster.

## Example output

```text
Service issues

  NAMESPACE   SERVICE         PROBLEM
  staging     api-gateway     no ready endpoints — the selector matches no pods
  staging     cron-svc        no ready endpoints (backs CronJob — expected between runs)
  production  public-lb       no external address (waiting 18m)
```

> Services where the cause cannot be determined (e.g. a matching pod is Ready but
> the endpoint object hasn't caught up yet) still show the bare
> `no ready endpoints` form.

## Interaction with `--explain`

Service issues are sent to `--explain` along with pod findings. The model
receives the namespace, Service name, and problem description — not raw
endpoint objects or IPs.

## Scope

Checks are read-only and honor the scan's `-n` (namespace) scope.
