# Roadmap

## Shipped

- **v1** — `kubeagent scan`: deterministic whole-cluster scan and diagnosis of
  [CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, and
  Pending/Unschedulable pods](features/diagnostics.md)
- **v2** — optional `--explain` flag: one Claude API call summarizes findings in
  plain English; the deterministic core still works offline with no API key
- **Resource context** — compact CPU/memory summary (allocatable, reserved,
  limits, live usage); OOMKilled findings annotated with the container's
  requests/limits; sent to `--explain` — see [Resource context](features/resource-context.md)
- **Platform facts** — CNI, ingress, storage provisioner, Kubernetes version +
  distribution, container runtime, and cloud detected read-only and shown under
  the cluster verdict; sent to `--explain` — see [Platform facts](features/platform-facts.md)
- **Service health** — flags selector-based Services with zero ready endpoints
  and LoadBalancer Services with no external address; backing-workload
  annotations distinguish expected-empty from broken — see [Service health](features/service-health.md)
- **NetworkPolicy hints** — when a workload is degraded with no detector finding,
  names the NetworkPolicies whose podSelector matches its pods — see
  [NetworkPolicy hints](features/networkpolicy.md)
- **Connectivity diagnostics** — when the API server is unreachable, prints an
  actionable diagnosis (down control plane, timeout, TLS/cert error, 401/403,
  DNS) instead of a raw transport error — see [Connectivity diagnostics](features/connectivity.md)
- **Credential lint** — `scan --lint-secrets` flags credentials stored in the
  clear in ConfigMaps and pod env literals; reports location and pattern only,
  never the value, and never sends findings to `--explain` — see
  [Credential lint](features/credential-lint.md)

!!! info "Version history"
    [GitHub Releases](https://github.com/imantaba/kubeagent/releases) and the
    [CHANGELOG](https://github.com/imantaba/kubeagent/blob/main/CHANGELOG.md)
    are the source of truth for what shipped in each version.
