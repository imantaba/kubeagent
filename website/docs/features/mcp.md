# MCP server

`kubeagent mcp` serves the exact same deterministic diagnosis `kubeagent scan`
runs, over the [Model Context Protocol](https://modelcontextprotocol.io) on
stdio, so another AI agent can call kubeagent as a tool instead of shelling
out to the CLI and parsing text.

!!! note
    Two separate claims, stated precisely rather than blurred together. The
    server is **read-only toward the cluster**: every tool issues only
    `get`/`list`/`watch` calls — the same calls `scan` makes. No tool can
    reach the guard-railed `--fix` writer, and no tool name contains a write
    verb; there is no code path from this package into `internal/remediate`.
    Separately, and more strongly, the server **makes no LLM call of its
    own** — nothing it returns is generated text. Every field in every
    result is computed by kubeagent's detectors, exactly as `scan` computes
    them; the calling agent may itself be a model, but kubeagent's half of
    the conversation never is.

## The four tools

| Tool | Arguments | What it does |
|------|-----------|---------------|
| `kubeagent_triage` | `namespace` (optional), `context` (optional) | Runs the same scan `kubeagent scan` runs and returns a `healthy`/`degraded` verdict, the findings that support it, and a coverage block. |
| `kubeagent_inspect` | `kind` (required — `pod`, `deployment`, `statefulset`, `daemonset`, `replicaset`, `job`, or `cronjob`), `namespace` (required), `name` (required), `context` (optional) | Drills into one workload or pod: its status, its pods, kubeagent's findings for it, and its recent Kubernetes events. |
| `kubeagent_advisory` | `sections` (required — any of `operators`, `drift`, `capacity`, `security`, `certificates`), `namespace` (optional), `context` (optional) | Runs kubeagent's opt-in advisory sections. Each section costs extra API reads, so it only runs what's requested. |
| `list_contexts` | none | Lists the kubeconfig contexts the server may switch between. **Only registered when the server was started with `--allow-context-switch`.** |

`context` on `kubeagent_triage`, `kubeagent_inspect`, and `kubeagent_advisory`
is accepted only when the server was started with `--allow-context-switch`;
otherwise a call naming one is rejected without ever reaching the cluster.

## The coverage block

Every result from the three diagnosis tools — `kubeagent_triage`,
`kubeagent_inspect` and `kubeagent_advisory` — carries a `coverage` object, so
a model can tell "nothing is wrong" from "nothing was checked": the same
failure mode a JSON reader hits when it treats an absent key as zero.
(`list_contexts` has no coverage block; it reads the kubeconfig, not the
cluster, so there is nothing for it to have looked at or missed.) A
`kubeagent_triage` call against a cluster with one crash-looping pod, started
**without** `--logs`, returns:

```json
{
  "verdict": "degraded",
  "cluster": {
    "context": "my-cluster",
    "version": "v1.31.2",
    "nodes": 3
  },
  "findings": [
    {
      "severity": "critical",
      "kind": "Pod",
      "namespace": "payments",
      "name": "worker-7d9c6f6b8-x2z4q",
      "reason": "Container repeatedly crashes after starting",
      "detail": "Container repeatedly crashes after starting (container \"worker\", restartCount=5)",
      "confidence": "high",
      "remediationHint": "starts then crashes — inspect the crash output"
    }
  ],
  "coverage": {
    "context": "my-cluster",
    "namespaceScope": "all namespaces",
    "collectedAt": "2026-07-28T09:14:02Z",
    "checksRun": [
      "workloads", "pod-diagnosis", "services", "ingresses",
      "persistentvolumeclaims", "terminating", "poddisruptionbudgets",
      "horizontalpodautoscalers", "webhooks", "resourcequotas"
    ],
    "checksSkipped": [
      { "check": "credential-lint", "why": "not run by triage; use the kubeagent CLI" },
      { "check": "disk-usage", "why": "not run by triage; it needs node stats the server does not request" },
      { "check": "security", "why": "not run by triage; call kubeagent_advisory with section \"security\"" },
      { "check": "certificates", "why": "not run by triage; call kubeagent_advisory with section \"certificates\"" },
      { "check": "kubelet-health", "why": "not run by triage; it is opt-in and not reachable through kubeagent_advisory either — use the kubeagent CLI's --kubelet-health flag" },
      { "check": "control-plane-health", "why": "not run by triage; it is opt-in and not reachable through kubeagent_advisory either — use the kubeagent CLI's --control-plane-health flag" },
      { "check": "dns-health", "why": "not run by triage; it is opt-in and not reachable through kubeagent_advisory either — use the kubeagent CLI's --dns-health flag" },
      { "check": "log-tails", "why": "the server was started without --logs" }
    ],
    "partial": [],
    "metricsServer": "not-checked"
  }
}
```

`checksRun` names every check that actually executed; `checksSkipped` names
every check that did not, each with a reason — `kubeagent_triage`
deliberately does not run credential linting, disk-usage, the security and
certificate sections (call `kubeagent_advisory` for those), or the three
opt-in health probes (kubelet, control-plane, DNS), which are not reachable
through `kubeagent_advisory` either — the CLI's `--kubelet-health`,
`--control-plane-health`, and `--dns-health` flags are the only way to run
them. Those seven are skipped on every `kubeagent_triage` call. The eighth
entry above, `log-tails`, is the one that varies: it is skipped only because
this server was started without `--logs`, and it moves to `checksRun` on a
server started with it. `partial` names a resource kubeagent tried to list and
couldn't, so an empty result is distinguishable from a denied one.

`metricsServer` is the literal string `"not-checked"` until a call actually
requests capacity data (`kubeagent_advisory` with section `"capacity"`); only
then does it become `"available"` or `"absent"`. Read `"not-checked"` as "this
call never looked," not as "no metrics problem was found" — a model that
reads it as the latter will silently miss a missing metrics-server.

## Configuration

Point an MCP host at the binary:

```json
{
  "mcpServers": {
    "kubeagent": {
      "command": "kubeagent",
      "args": ["mcp", "--context", "my-cluster"]
    }
  }
}
```

Flags: `--kubeconfig` (default: the usual `$KUBECONFIG`/`~/.kube/config`
resolution), `--context` (default: the kubeconfig's current context),
`--allow-context-switch` (off by default), `--logs` (off by default — enables
the log-tail enrichment `scan --logs` performs).

## Context switching

Off by default. A server started against one cluster only ever answers for
that cluster — a call naming a different `context` is rejected before it
reaches the cluster. Starting with `--allow-context-switch` also registers
`list_contexts`, and lets `kubeagent_triage`, `kubeagent_inspect`, and
`kubeagent_advisory` accept a `context` argument naming any context in the
same kubeconfig.

`--allow-context-switch` also changes what happens at startup when your
kubeconfig marks **no** context as current — the usual posture when you hold
several production kubeconfigs and do not want a stray `kubectl` to reach one.
Without the flag, the server exits: there is no default cluster and no way to
name one. With it, the server starts anyway, with no default cluster. All four
tools stay registered, `list_contexts` answers as usual with an empty
`current`, and a call naming a `context` works normally. A call naming none is
refused with a message telling the caller to list the contexts and pick one.

Nothing else degrades. A kubeconfig that cannot be read, one naming no
contexts at all, an API server that cannot be reached, and a `--context` that
does not resolve all still exit at startup with the error below — a server
that starts and then fails every call is worse than one that refuses to start.

## Freshness

There is no cache. Every call runs a fresh scan against the live cluster, so
an agent making several calls in a session never reasons about a stale
snapshot — the tradeoff is that each call costs the same API reads `scan`
would.

## What it does not do

- **No remediation.** No tool can reach `--fix`; the server has no write
  path into the cluster at all.
- **No `--explain`.** Nothing here calls an LLM — see the note at the top of
  this page. A calling agent is expected to reason over the structured
  result itself.
- **No `watch`-style streaming.** Each tool call is one point-in-time scan;
  there is no informer, no push notification, and no persistent session
  state between calls.

!!! note "The startup error on stderr is not redacted"
    Every result on the protocol stream is free of kubeconfig paths, and any
    API server URL in it is reduced to `scheme://host` — that redaction is
    what keeps `list_contexts` and every tool's error path safe to hand to a
    remote model. Startup is different. `kubeagent mcp` validates the
    cluster connection *before* it starts serving — a server that starts
    happily and then fails every call teaches the calling agent that
    kubeagent is unreliable — and if that validation fails, the process
    exits with an error naming the kubeconfig file and context it tried,
    printed to **stderr**, because that is what an operator needs to fix it.
    MCP hosts commonly capture a server subprocess's stderr into their own
    logs. If your host treats its logs as shareable, know that this one
    startup error is the exception to "no kubeconfig paths cross the MCP
    boundary" — it is deliberate, not a defect, but it is not redacted.
