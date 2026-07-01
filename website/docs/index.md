---
hide:
  - navigation
  - toc
---

# kubeagent

**Read-only Kubernetes troubleshooting, explained.**

kubeagent scans a cluster, finds unhealthy pods, and explains *why* they're
failing — talking to the cluster through the official Kubernetes Go client
(`client-go`), read-only by default, with opt-in guard-railed remediation.

<div class="hero-cta" markdown>
[Get started](quickstart.md){ .md-button .md-button--primary }
[Install](install.md){ .md-button }
[GitHub](https://github.com/imantaba/kubeagent){ .md-button }
</div>

## What it catches

<div class="grid cards" markdown>

- :material-restart: __CrashLoopBackOff__ — containers stuck restarting
- :material-image-broken-variant: __ImagePullBackOff__ — bad image or registry auth
- :material-memory: __OOMKilled__ — hit the memory limit (shown with the container's requests/limits)
- :material-timer-sand: __Pending / Unschedulable__ — no node can place the pod

</div>

## Beyond pods

<div class="grid cards" markdown>

- :material-server-network: __Service health__ — Services with no ready endpoints and LoadBalancers with no address, backing-aware
- :material-shield-lock-outline: __NetworkPolicy hints__ — which policies select a stuck pod
- :material-lan-disconnect: __Connectivity diagnostics__ — actionable "API server unreachable" messages
- :material-key-alert-outline: __Credential lint__ — opt-in scan for secrets stored in the clear
- :material-chart-box-outline: __Resource context__ — cluster CPU/memory plus per-OOMKill limits
- :material-layers-outline: __Platform facts__ — CNI, ingress, storage, distro, runtime, cloud
- :material-wrench-outline: __Remediation__ — opt-in `--fix` applies safe, reversible fixes after you confirm

</div>

## See it

```text
$ kubeagent scan
Cluster: Healthy — 3/3 nodes Ready
Platform: Cilium CNI · Traefik ingress · Kubernetes v1.30 · containerd

Resources (cluster):
  CPU     24.0 cores · req 6.2 (25%) · lim 18.0 (75%) · used 3.1 (12%)
  Memory  96Gi · req 22Gi (22%) · lim 70Gi (72%) · used 18Gi (18%)

⚠ shop/checkout  Deployment  1/3 Degraded  · 12 restarts, last 2m ago
    ⚠ CrashLoopBackOff: Container repeatedly crashes after starting
Service issues:
  ⚠ shop/checkout  ClusterIP  no ready endpoints
```

Optional `--explain` makes a single Claude API call to summarize findings in
plain English — the deterministic core still works fully offline.

---

Open source on [GitHub](https://github.com/imantaba/kubeagent) ·
[Releases](https://github.com/imantaba/kubeagent/releases)
