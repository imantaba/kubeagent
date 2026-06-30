# Resource context

`scan` prints a compact cluster resource summary alongside the diagnostic
output, giving you the context needed to interpret findings without switching to
a separate tool.

## What is included

- **Allocatable** — total CPU and memory the cluster can schedule onto.
- **Reserved / Requests** — the sum of all pod resource requests.
- **Limits** — the sum of all pod resource limits.
- **Live usage** — actual node-level consumption, when `metrics-server` is
  installed.

Live usage is best-effort: without `metrics-server` the summary still shows
allocatable, reserved, and limits, and notes usage as unavailable.

## OOMKill annotations

Each OOMKilled finding is annotated with the killed container's requests and
limits. This gives you the data you need to judge whether to raise the limit or
scale out, right in the scan output, without a separate `kubectl describe`
command.

## Interaction with `--explain`

The resource summary is included in what is sent to `--explain` so the model
can give memory- and CPU-aware advice — for example, suggesting a limit
increase versus a horizontal scale-out based on cluster headroom.

## Example output

```text
Cluster resources
  CPU    allocatable: 24 cores   requests: 14.2 cores   limits: 28.0 cores
  Memory allocatable: 96 Gi      requests: 38.4 Gi      limits: 64.0 Gi      usage: 41.2 Gi
```

!!! note
    Reading live usage requires a single GET on the metrics API and is always
    read-only. Without `metrics-server` the usage column shows `(unavailable)`.
