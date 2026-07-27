# MCP server — design

**Date:** 2026-07-27
**Theme:** G, slice 1 — meet people where they work
**Status:** approved

## Goal

Expose kubeagent's read-only diagnosis to other AI agents as an MCP server, so a
model working on a Kubernetes problem can ask kubeagent what is wrong instead of
inventing `kubectl` invocations.

The server is a new subcommand, `kubeagent mcp`. It speaks JSON-RPC 2.0 over
stdio, serves three task-shaped tools, and reaches the cluster through the same
`internal/scan` pipeline the CLI uses.

## Why this is not just "the CLI with a different output format"

A one-shot CLI is driven by an operator who typed the command, reads the whole
output, and knows which cluster they are pointed at. An MCP server is a
long-lived process driven by a model that did not type anything, reads a JSON
payload it will paraphrase, and knows only what the payload tells it. Three
consequences shape every decision below.

1. **Blast radius is chosen once, by the operator, at startup** — not per call by
   the caller.
2. **Absence must be explicit.** A human reads a stderr warning; a model reads
   JSON and treats a missing field as zero. Every "this did not run" and "this
   was denied" is a structural field, never prose.
3. **Payload size is a first-class constraint.** A model pays for every token of
   a 214-workload inventory it did not need.

## Architecture

```text
MCP client ──spawn──> kubeagent mcp ──> internal/mcp
                                          │  go-sdk stdio server
                                          │  3 tools + 1 conditional
                                          v
                                   cluster.Connect
                                          │
                                   scan.Evaluate ──> advisory.Assess
                                          │
                                   internal/mcp/view.go  (compact projections)
```

`internal/scan.Evaluate(ctx, client, opts) (Result, error)` is already
CLI-agnostic — its own doc comment says `Result` is exposed "so the CLI can
compose its extra views … without re-collecting". The MCP handlers reuse it
in-process. There is no second diagnosis path and no subprocess.

### Approaches considered

- **In-process reuse of `scan.Evaluate`, with the advisory composition extracted
  from `main.go` (chosen).** One diagnosis implementation, one refactor, no
  process spawning. The refactor is required because the advisory sections
  (`--operators`, `--drift`, `--capacity`) are composed inline in `main.go`
  today, and MCP needs the same logic. Duplicating it guarantees drift.
- **In-process reuse, advisory dropped from v1.** Zero refactor, but the third
  tool *is* the advisory sections, so this removes the feature rather than
  simplifying it.
- **Shell out to `kubeagent scan --output json` and reshape.** No refactor, but a
  process per call, our own output re-parsed, and the full-payload size problem
  we deliberately designed away.

### Invariants — stated as absences, not defaults

- **The `--fix` path is unreachable.** Not gated, not confirmable behind a
  parameter, absent. `internal/mcp` does not import `internal/remediate` or
  `internal/remediation`, and no registered tool name carries a write verb. A
  test asserts this.
- **No `--explain` and no `--investigate`.** The caller is already a model. A
  second outbound model call would burn the caller's budget and would put an API
  key inside a process a model is driving. The MCP server makes **zero**
  outbound network calls other than to the Kubernetes API.
- **No goroutines beyond what the SDK's stdio loop requires.** Tool handlers are
  sequential, matching the scan CLI. `internal/watch` remains the project's only
  concurrency exception.
- **stdout carries JSON-RPC and nothing else.** All logging goes to stderr.

Blast radius is therefore exactly: get/list/watch on one pinned context, using
the operator's own kubeconfig credentials — never more than that operator could
already do by hand.

## Transport and scope

**stdio only.** The client spawns `kubeagent mcp` as a child process and speaks
JSON-RPC over its stdin/stdout. No listener, no port, no new authentication
surface, no new attack surface reachable from the network. Credentials are the
operator's existing kubeconfig, read by the child process at startup.

**Cluster scope is pinned at startup.**

```console
kubeagent mcp --context staging
```

serves exactly that context. No tool takes a `context` parameter, and the
context is not discoverable from within the session.

```console
kubeagent mcp --context staging --allow-context-switch
```

additionally registers `list_contexts` and adds an optional `context` parameter
to each tool. The operator picks the blast radius, not the model.

## Protocol library

`github.com/modelcontextprotocol/go-sdk` v1.6.1 (released 2026-05-22), the
official Go SDK.

This takes the project from 4 direct dependencies to 6: the SDK itself, and
`github.com/google/jsonschema-go`, which becomes direct because the tool schemas
are written out explicitly — an inferred schema cannot express an `enum`, and the
`kind` and `sections` arguments need one. Importing only the
`.../go-sdk/mcp` package — the sole package this design uses — pulls six
indirect modules, of which `golang.org/x/oauth2` and `golang.org/x/sys` are
already in kubeagent's graph. **Four modules are genuinely new:**
`github.com/google/jsonschema-go`, `github.com/segmentio/encoding`,
`github.com/segmentio/asm`, and `github.com/yosida95/uritemplate/v3`, plus
version bumps of `golang.org/x/oauth2` (0.34.0 → 0.35.0) and `golang.org/x/sys`
(0.40.0 → 0.41.0). `golang-jwt/jwt/v5` and `golang.org/x/tools` are **not**
pulled in — they belong to the SDK's auth and conformance packages, which this
design does not import. (Measured against v1.6.1 in a scratch module, not
estimated.)

The import is confined to `internal/mcp`; nothing on the scan path imports it,
so the scan binary's dependency surface for diagnosis is unchanged.

Hand-rolling JSON-RPC framing was rejected: the protocol is versioned and
evolving, and a bespoke implementation would be a maintenance liability for no
gain.

## Tool surface

### `kubeagent_triage`

The default entry point. One call answers "what is wrong".

```jsonc
// input
{
  "namespace": "string?",         // omitted = all namespaces
  "includeRestarts": "boolean?",  // default false — noisy signal, opt-in
  "includeCron": "boolean?"       // default false
}
```

```jsonc
// output
{
  "verdict": "healthy" | "degraded",
  "cluster": { "nodesReady": 5, "nodesTotal": 6, "version": "v1.34.0" },
  "workloads": { "total": 214, "healthy": 209, "withFindings": 5 },
  "findings": [
    {
      "severity": "critical",
      "kind": "Pod",
      "namespace": "payments",
      "name": "api-7d4f-x2k9",
      "reason": "CrashLoopBackOff",
      "detail": "back-off restarting failed container",
      "confidence": "high",
      "remediationHint": "check the container's exit code and recent logs"
    }
  ],
  "findingsTotal": 5,
  "findingsOmitted": 0,
  "coverage": { }   // see "The honesty contract"
}
```

Healthy workloads collapse to the `workloads.healthy` count — never 209 rows.

Findings are ranked severity, then namespace, then name. The ordering is total,
so identical cluster state yields a byte-identical payload.

The findings array is capped at **50**. Anything beyond the cap is counted in
`findingsOmitted` and never silently dropped. There is no cursor, no paging
tool, and no server-side session state: a caller that needs the omitted rows
narrows by namespace.

### `kubeagent_inspect`

Drill-down on something triage named.

```jsonc
// input
{
  "kind": "Pod" | "Deployment" | "StatefulSet" | "DaemonSet" | "Job" | "CronJob" | "Node",
  "namespace": "string?",   // required for every kind except Node
  "name": "string"
}
```

```jsonc
// output
{
  "kind": "Pod", "namespace": "payments", "name": "api-7d4f-x2k9",
  "found": true,
  "status": { "phase": "Running", "ready": "1/2", "restarts": 7, "age": "3h12m" },
  "containers": [
    { "name": "api", "image": "example.invalid/api:1.4", "state": "waiting", "lastExitCode": 137 }
  ],
  "findings": [ ],
  "events": [ { "type": "Warning", "reason": "BackOff", "count": 41, "age": "2m" } ],
  "logTail": "string?",
  "coverage": { }   // see "The honesty contract"
}
```

`logTail` is present only when the server was started with `--logs`; otherwise
`coverage.checksSkipped` carries `{"check":"logs","why":"server started without --logs"}`.

A missing object returns `found: false` as a normal result, not an error. A
model asking about a Pod that has already been rescheduled deserves an answer,
not a protocol fault.

### `kubeagent_advisory`

The opt-in sections, requested explicitly.

```jsonc
// input
{
  "sections": ["capacity" | "gitops" | "operators" | "security" | "certificates"],
  "namespace": "string?"
}
```

```jsonc
// output
{
  "sections": { "capacity": { }, "gitops": { } },
  "requested": ["capacity", "gitops"],
  "unavailable": [ { "section": "gitops", "why": "no Argo CD or Flux CRDs present" } ],
  "coverage": { }
}
```

An empty `sections` array is an input error, not "run everything". These
sections cost extra API calls, and a model that omitted the field has not
decided what it wants.

Every section keeps the advisory contract it has in the CLI: it never produces a
`Finding`, never sets `hasAttention`, never changes the Healthy/Degraded
verdict, and never affects `kubeagent_triage`'s output.

The five sections come from two different places, and the handler must not
assume otherwise:

- `capacity`, `gitops`, `operators` are composed by `advisory.Assess` (below).
- `security` and `certificates` are fields of `scan.Result`, produced by
  `scan.Evaluate` under `Options.Security` and `Options.Certs`. Requesting them
  means a second `Evaluate` call with those options set — `kubeagent_triage`
  always runs with both false, which is why triage's output is unaffected.

Section payloads are the existing report structs (`capacity.Report`,
`gitops.Report`, `operators.Report`, `[]secscan.Finding`, `certhealth.Report`).
Those are already summary-shaped and internally capped, so `view.go` builds no
separate projection for them — only for triage findings.

### `list_contexts` — conditional

Registered **only** under `--allow-context-switch`. Returns each context's name
and its server as `scheme://host` — never a full endpoint URL, never a token,
never a client-certificate path. Without the flag the tool is absent from
`tools/list`, so a model cannot discover that other clusters exist.

### Deliberately absent

No `kubeagent_fix`, `kubeagent_apply`, `kubeagent_explain`, or any
arbitrary-`kubectl` escape hatch. The tool list is the entire surface; no verb
hides behind a parameter.

## The honesty contract

Every tool result carries a `coverage` block:

```jsonc
"coverage": {
  "context": "staging",
  "namespaceScope": "payments",          // or "all"
  "collectedAt": "2026-07-27T14:02:11Z",
  "checksRun": ["crashloop", "imagepull", "oom", "pending", "probes", "initcontainers"],
  "checksSkipped": [
    { "check": "logs",     "why": "server started without --logs" },
    { "check": "capacity", "why": "not requested" }
  ],
  "partial": [
    { "resource": "networkpolicies", "why": "forbidden by RBAC" }
  ],
  "metricsServer": "present" | "absent"
}
```

Three rules it enforces:

1. **A denied list is never a healthy zero.** Any `list` that returned a
   permission error appears in `partial`. If the payload cannot be honest, it
   says so in-band.
2. **A skipped check is named with its reason.** "No security findings" and "the
   security scan did not run" are different statements, and the model gets to
   see which one it holds.
3. **`namespaceScope` is echoed back.** A model that asked about one namespace
   and then reports "the cluster is healthy" contradicts a field in its own
   input.

### Freshness: no cache, and the docs say so

Every call re-collects. No TTL, no session, no memoization between calls. During
an incident a stale answer is worse than a slow one, and a cache would need an
invalidation rule that has no correct answer for a cluster changing underneath
it. `collectedAt` is therefore always true. The cost is real — a triage call is
a full inventory collect — and the documentation states it plainly rather than
hiding it.

## Error handling

Two channels, chosen by whether the model can act on the result.

| Situation | Channel |
| --- | --- |
| Unknown tool name | JSON-RPC error — the client asked for something that does not exist |
| Missing required field, wrong type, bad enum value | Tool result, `isError: true`, SDK-generated schema-validation text; the handler never runs |
| Cluster unreachable, auth expired, context gone | Tool result, `isError: true`, redacted message |
| RBAC denied on some resource | Normal result plus `coverage.partial` |
| Object not found (`inspect`) | Normal result, `found: false` |
| metrics-server absent | Normal result, `metricsServer: "absent"` |

The split between the first two rows is the SDK's, not ours, and was measured
rather than assumed: with `mcp.AddTool`, arguments are validated against the
inferred input schema *before* the handler is entered, and a validation failure
is packed into `CallToolResult{IsError: true}` — not raised as a protocol error.
Only an unknown tool name produces a JSON-RPC error. This is the better
behavior for our purposes: a model can read and correct an `isError` result,
whereas a protocol fault is opaque to it.

**Redaction is mandatory on every error path.** client-go errors embed the API
server URL, and a kubeconfig server URL can carry userinfo or an auth-proxy
query string — it is a credential. Every string leaving a handler passes through
`RedactError`, and so does every stderr log line.

**Startup validation is eager.** A missing context, an unreadable kubeconfig, or
a failed connectivity probe fails before the server speaks JSON-RPC at all, with
a plain message on stderr. Better than every subsequent tool call returning the
same error forever.

**Handlers recover from panics.** The SDK does **not** recover: a panicking
handler unwinds through `jsonrpc2`'s per-request goroutine and takes the whole
process down, leaving the client with a dead pipe. (Verified against v1.6.1 —
the process exits with status 2.) Every handler is therefore wrapped so a panic
becomes a redacted `isError: true` result and the session survives.

## File structure

### New package `internal/mcp`

| File | Responsibility |
| --- | --- |
| `server.go` | Build the server, register tools, eager startup validation |
| `triage.go` | `kubeagent_triage` handler |
| `inspect.go` | `kubeagent_inspect` handler |
| `advisory.go` | `kubeagent_advisory` handler |
| `contexts.go` | `list_contexts`, registered only under `--allow-context-switch` |
| `coverage.go` | Builds the `coverage` block every handler returns |
| `view.go` | Compact projections: finding view, deterministic sort, cap and `findingsOmitted` |

Handlers hold no state. `main.go` gains the `mcp` subcommand and its flag set
(`--context`, `--allow-context-switch`, `--logs`, `--kubeconfig`), using the
standard-library `flag` package like every other subcommand.

### Refactor 1 — extract `internal/advisory`

`main.go` composes the operator, GitOps, and capacity reports inline
(`main.go:191-234`), printing failures with
`fmt.Fprintf(os.Stderr, "warning: …")`. MCP needs the same composition but must
turn those failures into `coverage.partial` entries rather than stderr lines.

```go
type Degradation struct {
    Subject string // "operators", "gitops", "pod metrics"
    Reason  string // already redacted
}

type Options struct {
    Operators bool
    Drift     bool
    DriftAge  time.Duration
    Capacity  bool
    Namespace string
}

type Result struct {
    Operators *operators.Report
    GitOps    *gitops.Report
    Capacity  *capacity.Report
    Degradations []Degradation
}

func Assess(ctx context.Context, client kubernetes.Interface,
    dynFactory func() (dynamic.Interface, discovery.DiscoveryInterface, error),
    inputs inventory.Inputs, nodes []corev1.Node, pods []corev1.Pod,
    now time.Time, opts Options) Result
```

Returning degradations instead of printing them lets the CLI print exactly what
it prints today, byte-identical, while MCP renders the same values structurally.

`dynFactory` is lazy so a call with no advisory option still constructs no
dynamic client and issues no discovery request — preserving the existing
guarantee that a plain scan costs no extra API call.

### Refactor 2 — extract `internal/redact`

`RedactURL` and `RedactError` currently live in `internal/alert`, a package
documented as "delivers watch notifications to an operator-configured HTTP
endpoint". Move both to `internal/redact`; update `internal/alert` and
`internal/watch` call sites. No behavior change. This stops `internal/mcp` from
importing the webhook-delivery package in order to scrub a Kubernetes API error.

## Testing

Every test runs against client-go's fake clientset — `scan.Evaluate` takes
`kubernetes.Interface`, so no cluster is required.

- **Golden JSON per tool.** One fixture cluster produces a byte-identical
  payload. This single assertion guards determinism, the sort order, the cap,
  and the coverage shape.
- **The tool list is asserted exactly:** three names without
  `--allow-context-switch`, four with it. Plus a guard that no registered tool
  name matches `fix|apply|delete|patch|create|explain`, so a future write tool
  cannot be added without deliberately deleting a test that explains why it
  exists.
- **Redaction:** an error carrying userinfo and a query token renders as
  `scheme://host` and nothing more.
- **RBAC denial produces `coverage.partial`**, asserted directly — "denied
  renders as zero findings" is the failure mode most likely to mislead a model.
- **`found: false`** on a missing object, not an error.
- **Panic recovery** returns `isError: true` and the process survives.
- **The default scan stays byte-identical** — the golden-scan fixture is
  unchanged, and the advisory extraction adds no API call.

### Chaos scenario 19

Start `kubeagent mcp` against `kind-kubeagent-chaos`, drive `tools/list` and one
`kubeagent_triage` call over stdio, and assert: verdict `degraded`, findings
non-empty, no write-verb tool listed, and `coverage.context` naming the chaos
context.

## Documentation

- `website/docs/features/mcp.md` (new) — what the server is, what it refuses to
  do, the three tools, the coverage contract, the per-call cost.
- README, CHANGELOG, `website/docs/roadmap.md`.

The client-configuration snippet shows the server spawned with `--context` and
carries **no API key**: the server makes no model call, so there is nothing to
configure and nothing to leak.

## Global constraints

- **No `Co-Authored-By: Claude` trailer**, and no Claude / Claude Code /
  Anthropic attribution in any commit message, code comment, or document.
- **Read-only toward the cluster.** Get/list/watch only. No writes, no new RBAC.
- **`--fix` is unreachable from MCP**, structurally, not by configuration.
- **Zero outbound calls** other than to the Kubernetes API.
- **Endpoint URLs and kubeconfigs are credentials.** No log line, error string,
  tool result, or documentation example carries more than `scheme://host`. An
  API key never appears as a flag, container argument, `values.yaml` literal, or
  plain `value:` in a pod environment.
- **Never blur "read-only" into "makes no external calls"** in prose — for the
  MCP server both happen to be true, but they are different claims.
- Standard-library `flag` package only — no Cobra.
- Handlers are sequential; `internal/watch` remains the only concurrency
  exception.
- Determinism: never range a map to produce ordered output without a total sort.
- No secrets, credentials, private IPs, internal hostnames, or real cluster
  endpoints anywhere, including test fixtures.
