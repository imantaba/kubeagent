# `kubeagent mcp` without a current context

**Status:** design, approved 2026-08-08
**Branch:** `mcp-no-current-context`, cut off `main` at `b7ea9cb`

## What broke

The Claude Code plugin shipped in the previous branch was loaded into a live
client for the first time. The skills and the three commands registered
correctly. The MCP server did not:

```text
MCP error -32000: Connection closed
```

Two independent causes, found by running the manifest's own command line by
hand.

**The binary on `PATH` predated the MCP server.** `/home/ubuntu/.local/bin/kubeagent`
was a `dev` build whose usage line named only `scan`, `watch` and `version`.
`kubeagent mcp` was not a subcommand, so the process printed usage, exited 1,
and the client reported a closed connection. Upgrading fixed it.

**The kubeconfig had no current context.** Four contexts, `CURRENT` empty —
a reasonable posture for an operator with several production clusters who does
not want a stray `kubectl` to hit one by accident. `Serve` validates its
connection eagerly and exits:

```text
kubeagent: connecting to the cluster: loading kubeconfig "…": invalid
configuration: no configuration has been provided
```

The second one is the interesting failure, because the server was started with
`--allow-context-switch` — the flag whose entire purpose is to let a caller name
a cluster per call, backed by a `list_contexts` tool that exists to discover the
names. That machinery was unreachable: the server died before serving, so the
tool that would have resolved the problem could never be called. A capability
kubeagent already ships was gated behind the very condition it was designed to
handle.

## Slice 1 — start without a default cluster

### The trigger, narrowly

`Serve` keeps its eager `cluster.NewClient` call. When `NewClient` fails, it
asks one further question, and degrades only when both halves are true:

1. `cfg.AllowContextSwitch` is set. Without it no tool call may name a context,
   so a server with no default cluster could answer nothing — exiting is the
   honest outcome.
2. The kubeconfig parses into at least one context and none of them is current.

Everything else exits exactly as today, with the same message on stderr: a
missing or unreadable kubeconfig, a kubeconfig naming zero contexts, an
unreachable API server, a `--context` that does not resolve. Those are not the
case that broke, and failing loudly on them stays correct — a typo'd
`--kubeconfig` or a VPN that is down should not degrade into a server that
looks healthy and refuses every call.

### How the second half is answered

`cluster.Contexts(cfg.Kubeconfig)` already exists, is already used by
`list_contexts`, is contractually path-free (it discards the underlying error
and returns a fixed message), and already computes `Current: name ==
raw.CurrentContext` per entry. A kubeconfig with no `current-context` therefore
yields a non-empty slice with no entry marked current — exactly the predicate,
with no new parsing and no matching against clientcmd's error strings, which
are not part of any contract kubeagent can rely on.

### What "degraded" means

One thing: the `base` clientset is nil.

`newServer` is unchanged and registers the same four tools. `list_contexts`
never consulted `base` — it reads the kubeconfig directly — so it works
untouched, and its `current` field is `""`, which is already the zero value of
its type and already the accurate answer.

`clientFor` grows one branch. Today `requested == ""` returns `base`
unconditionally:

```go
func clientFor(cfg Config, base kubernetes.Interface, switchTo clientFactory, requested string) (kubernetes.Interface, string, error) {
	if requested == "" {
		if base == nil {
			return nil, "", errNoDefaultContext
		}
		return base, contextLabel(cfg.Context), nil
	}
	…
}
```

`errNoDefaultContext` mirrors the existing `errContextSwitchDisabled`: a
sentinel defined beside it, returned verbatim across the MCP boundary, naming
the fix without naming a kubeconfig path or a server address.

> this kubeconfig has no current context, so there is no default cluster: call
> `list_contexts` and pass one of its names as `context`

Calls that name a context are untouched on every path — `switchTo(requested)`
never consulted `base`, so `kubeagent_triage`, `kubeagent_inspect` and
`kubeagent_advisory` all work normally against any named context while `base`
is nil.

### What does not move

- **No `schemaVersion`.** No field, type or enum value changes on any of the
  eight versioned JSON documents. `Coverage.context` still reports whichever
  context answered the call.
- **Read-only.** No new cluster call of any kind; the degraded path makes
  strictly fewer.
- **No LLM call**, and `internal/mcp` still imports neither `internal/remediate`
  nor `internal/explain`.
- **The startup carve-out.** The exit path still prints the unredacted
  kubeconfig path to stderr, the deliberate exception documented in
  `website/docs/features/mcp.md`. The degraded path prints nothing, exits
  nothing, and reaches the protocol stream only through `errNoDefaultContext`,
  which carries no path.

### Compatibility

A server that previously exited now starts. Nothing that previously worked
changes, and no flag, output shape or exit code moves — additive, MINOR under
the 1.x rules in `website/docs/compatibility.md`.

### Testing slice 1

- `clientFor` is pure: a table test over the four combinations of
  `requested`/`base` asserts the new sentinel is returned for exactly one, and
  that a named context still resolves when `base` is nil.
- The trigger predicate is exercised against real kubeconfig files written to
  `t.TempDir()` — no cluster needed. Cases: contexts with one current
  (no degrade), contexts with none current (degrade), zero contexts (no
  degrade), unreadable file (no degrade), and none-current with
  `AllowContextSwitch` off (no degrade).
- End to end, `newServer(cfg, version, nil, switchTo, now)` is driven over the
  real protocol against a fake clientset: a `kubeagent_triage` call with no
  `context` returns the sentinel text, the same call naming a context returns a
  normal result, and `list_contexts` answers with `current` empty.

## Slice 2 — preflight capability, not presence

`command -v kubeagent` passed on the stale binary. Presence is not capability,
and the gap is not cosmetic: the plugin's skills and commands load even when its
MCP server has died, so a model runs `/kubeagent:triage`, follows the skill,
records the preflight as passed, and then calls `kubeagent_triage` — a tool that
is not in its list.

Step 0 of `skills/triaging-a-cluster/SKILL.md` is reordered to check the thing
that actually matters first, and it costs no subprocess: **are the `kubeagent_*`
tools in the model's own tool list?** If they are, the server is up and
connected and nothing else needs checking. Only when they are absent does the
skill shell out, to distinguish three causes that need three different answers:

| Probe | Meaning | What the skill says |
|---|---|---|
| `command -v kubeagent` finds nothing | not installed | install it — the three existing paths |
| found, but `kubeagent mcp --help` fails | binary predates the MCP server | **upgrade** it; installing again over the top is the same command but the diagnosis differs |
| both succeed | binary is fine, the server could not connect | after slice 1 this narrows to an unreadable kubeconfig or an unreachable API server; report that, and do not claim the cluster is healthy |

The middle row is the case that broke, and the current text cannot produce it —
it can only say "install kubeagent", which the operator had already done.

The skill names no version number. A version floor would rot; `kubeagent mcp
--help` asks the binary itself and stays true.

`commands/triage.md`, `commands/why.md` and `commands/preflight.md` each open
with "confirm `kubeagent` is on PATH". They defer to the skill for the workflow,
so they change to point at the skill's preflight rather than restating a check
that is now three-way.

### Testing slice 2

`TestShippedDocsNameOnlyRegisteredTools` already fails the build when shipped
text names a tool `internal/mcp` does not register, and it requires each shipped
doc to name at least one. Both still hold after the rewrite; the new text names
more tools, not fewer.

## Deliberately out of scope

**A test pinning every `kubeagent <verb>` command line in the shipped skills
against the real Cobra tree.** It fits the established pattern — the manifest's
flags already run through `parseMCPFlags`, and shipped tool names already run
through the registry — and it would have caught nothing here, because the
command lines in the skill were correct. The binary was old. Deferred until a
wrong flag actually ships.

**Degrading on an unreachable API server.** Considered and rejected: with
`--allow-context-switch` another context might be healthy, but a current context
whose cluster is down is the ordinary single-cluster failure, and turning it into
a per-call surprise costs more than it buys.

**Any change to the plugin manifest.** `--allow-context-switch --logs` is
already correct; slice 1 is what makes the first of those two flags reachable in
the case that broke.
