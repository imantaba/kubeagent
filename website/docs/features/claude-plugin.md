# Claude Code plugin

kubeagent installs into [Claude Code](https://claude.com/claude-code) as a
plugin: the `kubeagent mcp` server, plus two skills and three commands that
teach the model how to use it.

!!! note
    Two separate claims, stated precisely rather than blurred together. Nothing
    in the plugin **writes to the cluster** — no tool, no skill, no command
    reaches the guard-railed `--fix` path, which stays something an operator
    types. Separately, and more strongly, **kubeagent makes no LLM call of its
    own**. Claude is the calling model; every field in every result it reads is
    computed by kubeagent's detectors, exactly as `scan` computes them.

## Install

kubeagent's binary is a prerequisite — a plugin cannot ship one. Install it
first by any of the routes on the [install page](../install.md), then:

```text
/plugin marketplace add imantaba/kubeagent
/plugin install kubeagent@kubeagent
```

Confirm the server connected with `/mcp`. If it shows as failed, the usual
cause is `kubeagent` not being on the `PATH` Claude Code sees.

## What you get

**The MCP server**, started as `kubeagent mcp --allow-context-switch --logs`.
Context switching is on so the model can name any context in your kubeconfig —
it cannot reach a cluster your kubeconfig does not already name — and `--logs`
adds the log-tail enrichment that makes a CrashLoopBackOff finding actionable
rather than merely correct. Both cost extra API reads per call. The four tools
themselves are documented under [MCP server](mcp.md).

**Two skills.** `triaging-a-cluster` is the workflow: call `kubeagent_triage`
first, escalate to `kubeagent_inspect` using names the findings supplied,
request an advisory section only when a finding points at one, and stop when the
verdict is healthy. `reading-kubeagent-findings` is the semantics, and it exists
to prevent one specific failure — a model reading an absent key as good news. A
skipped check is not a passing one, a `partial` entry is a blind spot rather
than a clean result, and `metricsServer: "not-checked"` means the call never
looked.

**Three commands.**

| Command | What it does |
|---|---|
| `/kubeagent:triage [namespace]` | Sweeps, auto-inspects every critical and warning finding, reports with its coverage caveats stated |
| `/kubeagent:why <kind>/<name> [-n ns]` | Root-causes one object: its findings, its events, and the one advisory section its failure implies |
| `/kubeagent:preflight [namespace]` | A pre-deploy gate: triage plus `drift` and `capacity` → one GO/NO-GO with blind spots listed |

## Two skills directories

If you are working *on* kubeagent rather than *with* it: `.claude/skills/` in
this repository is dev-facing — it holds `release` and `update-demo-gif`, which
maintain the project itself — and root-level `skills/` is what the plugin ships
to users. Claude Code auto-discovers only the former, so a contributor never
loads the user-facing skills by accident.

## Stability

The manifest's `mcpServers` block is a stable surface from the release that
introduced it. Adding a flag is a MINOR change; removing one, or changing what
an existing flag means, is MAJOR — because either alters the behaviour of an
already-installed plugin on upgrade. See
[compatibility](../compatibility.md).
