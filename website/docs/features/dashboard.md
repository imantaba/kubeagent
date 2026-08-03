# In-cluster dashboard

`kubeagent watch --dashboard` serves a read-only HTML page at `/dashboard` on
the daemon's metrics port. It is the URL you hand someone who asks what is
broken right now.

```bash
kubeagent watch --dashboard
# then, from inside the cluster or through a port-forward:
#   http://localhost:8080/dashboard
```

## What it shows

Everything on the page comes from state the daemon already tracks for
`/metrics` and `/issues`. The dashboard performs **no cluster read of its own**,
which is why enabling it changes no RBAC.

| Section | What it answers |
|---|---|
| Clusters | Is each watched cluster reachable, and when did it last evaluate? |
| Active incidents | What is broken, for how long, and is it flapping? |
| Resolved incidents | What recovered, when, and how long did it take? |
| Totals | New, resolved, flapping and dropped counts, and mean time to resolution. |
| SLO | Availability, burn rate, error budget left and coverage per window — only with `--slo-target`. |
| Explanations | The latest explanation per object — only with `--explain`. |

The cluster strip is always present, including when both incident lists are
empty. An empty list from an unreachable cluster and an empty list from a
healthy one are not the same thing, and this band is what tells them apart. A
cluster that has not completed its first evaluation says so rather than reading
as down.

The page reloads itself every 30 seconds through a `<meta http-equiv="refresh">`.
The interval is fixed and has no flag: informers detect in roughly two seconds
and the heartbeat is sixty, so thirty already sits between them.

## What it does not do

- **It does not browse the cluster.** No namespace list, no workload
  drill-down, no pod detail, no events. That is [`kubeagent tui`](tui.md), which
  runs on a laptop against a kubeconfig and performs its own reads.
- **It changes nothing.** No buttons, no forms, no actions. `/dashboard` is a
  `GET` that renders a snapshot.
- **It never triggers an explanation.** The page renders explanations the
  incident pipeline already computed. A dashboard request makes no model call.
- **It shows no blind spots.** What kubeagent could not read is a `scan`
  concept the daemon does not carry; use `kubeagent scan` or
  [`kubeagent gate`](ci-gate.md) for that.

## Exposure and authentication

**kubeagent implements no authentication for the dashboard.** This is a
decision, not an omission.

The posture is identical to what `/metrics` and `/issues` already have on the
same port: unauthenticated, and `ClusterIP` by default. **kubeagent terminates
no TLS.**

The rationale: the daemon's entire security story is that it holds no credential
beyond its own read-only ServiceAccount. A password or token store inside
kubeagent would contradict that, in exchange for guarding a page whose data the
same port already serves unauthenticated.

Put authentication where it belongs — in front:

- an Ingress with basic-auth, or an `oauth2-proxy` sidecar, terminating TLS and
  authenticating before the request reaches the daemon;
- a `NetworkPolicy` keeping the metrics port reachable only from your monitoring
  namespace;
- or nothing at all, if the Service stays `ClusterIP` and you reach it with
  `kubectl port-forward`.

The page itself carries no cluster identity beyond the operator-chosen cluster
**names** that `/issues` and every metric series already carry. No API server
URL, no kubeconfig path, no kubeconfig context name, and no URL of any kind —
the meta refresh carries an interval, not a target.

## Enabling it

=== "CLI"

    ```bash
    kubeagent watch --dashboard
    ```

=== "Environment"

    ```bash
    KUBEAGENT_DASHBOARD=true kubeagent watch
    ```

=== "Helm"

    ```bash
    helm upgrade --install kubeagent deploy/helm/kubeagent \
      --namespace kubeagent --create-namespace \
      --set dashboard.enabled=true
    ```

It shares the existing metrics port, so there is no new Service port and no new
RBAC rule to grant.

## Stability

`--dashboard`, `KUBEAGENT_DASHBOARD`, `dashboard.enabled`, and the existence of
`/dashboard` returning HTML are **stable within 1.x**.

**The page's markup and layout are not.** It is an artifact for a human to look
at, and its structure will change. Anyone who wants a contract parses
[`/issues`](watch-mode.md#issues), which is versioned. See
[Compatibility](../compatibility.md).
