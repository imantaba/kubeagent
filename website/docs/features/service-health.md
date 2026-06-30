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
| CronJob or Job | `(backs CronJob — expected between runs)` |
| DaemonSet / Deployment / StatefulSet scaled to 0 | `(backs Deployment — scaled to 0)` |

A Deployment or StatefulSet with a non-zero replica count and no ready
endpoints **stays primary** — that is a real problem.

## Example output

```text
Service issues

  NAMESPACE   SERVICE         PROBLEM
  staging     api-gateway     no ready endpoints
  staging     cron-svc        no ready endpoints (backs CronJob — expected between runs)
  production  public-lb       no external address (waiting 18m)
```

## Interaction with `--explain`

Service issues are sent to `--explain` along with pod findings. The model
receives the namespace, Service name, and problem description — not raw
endpoint objects or IPs.

## Scope

Checks are read-only and honor the scan's `-n` (namespace) scope.
