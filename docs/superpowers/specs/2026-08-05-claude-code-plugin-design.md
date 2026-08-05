# kubeagent as a Claude Code plugin — design

**Status:** approved, ready for planning
**Theme:** G · Operator and ecosystem coverage — a third distribution surface
alongside the MCP server and the krew plugin
**Ships as:** a MINOR. The release number is left open: the PagerDuty receiver
slice also claims the next MINOR, and whichever lands first takes it.

## Problem

`kubeagent mcp` already speaks the protocol Claude Code consumes. An operator
can wire it up today:

```bash
claude mcp add kubeagent -- kubeagent mcp --context my-cluster
```

That works, and it is most of the value. What it does not do is teach the model
anything. Claude gets four tools and no instruction on how to use them, so it
improvises: it calls `kubeagent_inspect` before it knows what is broken, it runs
every advisory section because more data feels safer, and — the failure that
actually costs an operator something — it reads an absent key as good news. A
`coverage` block that says `metricsServer: "not-checked"` means *this call never
looked*. A model with no instruction reads it as *no metrics problem was found*
and reports a clean cluster.

kubeagent spent a whole slice building `coverage` so that "nothing is wrong" and
"nothing was checked" could be told apart, and another whole slice (least-privilege
RBAC) making a refused read surface as a named blind spot instead of an empty
section. Handing a model those fields with no instruction throws away both.

So the plugin is not a packaging convenience. The manifest is the cheap part; the
skills are the point.

## Scope

A Claude Code plugin, hosted from this repository, bundling:

- **the existing `kubeagent mcp` server**, wired in the plugin manifest — no new
  Go server code, no new tools;
- **two skills** — one for the workflow (how to chain the tools, when to stop),
  one for the semantics (how to read a finding and, above all, a `coverage`
  block);
- **three slash commands**, each a multi-step workflow rather than a passthrough
  to a single tool;
- **a pin test** that fails the build when the manifests, the skills, or the
  commands drift from what kubeagent actually is;
- **docs** — a feature page, a README section, CHANGELOG, CLAUDE.md, roadmap.

### Explicitly out of scope

Each of these is a decision, not an omission.

- **No bundled subagent.** A `kubeagent-triage` subagent would own tool-chaining
  in its own context window, which is real value, but it duplicates what the
  workflow skill teaches and adds a second surface to keep in sync with the
  detector set. Revisit if the skills prove insufficient in use.
- **No write path.** Nothing in the plugin reaches `--fix` — not a tool, not a
  skill, not a command. This mirrors the `internal/mcp` invariant and makes the
  plugin's promise a single sentence: it diagnoses, it never writes. `--fix`
  stays an action an operator types, with its own confirmation prompts.
- **No hooks.** A `SessionStart` hook checking for the binary on `PATH` would
  catch a missing install earliest, but it fires in every session in every
  project. The workflow skill's preflight step is enough and costs nothing when
  the binary is present.
- **No `watch`, dashboard, or TUI integration.** All three are long-lived
  processes. A stdio MCP server started and stopped by the host is the wrong
  shape for them.
- **No third-party marketplace submission.** Can be layered on later; it depends
  on someone else's review cadence and gives no install path we control.

## Decisions

| Question | Decision | Why |
|---|---|---|
| Bundle contents | MCP + skills + commands | Smallest bundle that beats `claude mcp add`. A manifest alone is a discoverability win and nothing more. |
| Marketplace host | This repo, `.claude-plugin/` at root | One release process. The plugin cannot drift from the binary it wires, because they ship together. |
| Missing binary | Documented, plus a skill preflight step | No hook, no new failure mode. A user with no binary sees the skill explain it rather than a raw MCP connection error. |
| Write path | None | See above. |
| Server flags | `mcp --allow-context-switch --logs` | Context switching is the multi-cluster operator's case, and it cannot reach a cluster the kubeconfig does not already name. `--logs` is what makes CrashLoopBackOff diagnosis useful rather than merely correct. Both cost extra API reads; that is the trade accepted. |
| Skill shape | One workflow skill, one interpretation skill | Two jobs, two files. Per-failure-mode skills would overlap heavily and need updating every time the detector suite grows. |
| Drift control | A pin test, not discipline | The repo's established move: `krew_manifest_test.go`, `release_vars_test.go`, `TestNoProductionImport`, `TestSchemaDrift`. |

## Architecture

The plugin root is the repository root, which is the conventional shape.

```text
.claude-plugin/
  plugin.json                      # plugin manifest, MCP server inline
  marketplace.json                 # marketplace manifest — must sit at repo root
skills/
  triaging-a-cluster/SKILL.md
  reading-kubeagent-findings/SKILL.md
commands/
  triage.md
  why.md
  preflight.md
plugin_manifest_test.go            # repo root, beside krew_manifest_test.go
internal/cli/plugin_flags_test.go
website/docs/features/claude-plugin.md
```

### Two skill directories, two audiences

`.claude/skills/` already exists in this repository and holds `release` and
`update-demo-gif`. Those are **dev-facing**: they are for working *on* kubeagent.
The new root-level `skills/` is **user-facing**: it is shipped to whoever installs
the plugin and is about working *with* kubeagent.

Claude Code auto-discovers only `.claude/skills/`, so there is no functional
collision — a contributor in this checkout will not accidentally load the
user-facing skills. The distinction is nonetheless written down here and in
CLAUDE.md, because the two directory names are one letter and one dot apart.

### `plugin.json`

```json
{
  "name": "kubeagent",
  "description": "Read-only Kubernetes troubleshooting: deterministic diagnosis over MCP, no LLM calls of its own.",
  "version": "1.2.0",
  "author": { "name": "imantaba" },
  "homepage": "https://github.com/imantaba/kubeagent",
  "repository": "https://github.com/imantaba/kubeagent",
  "license": "Apache-2.0",
  "keywords": ["kubernetes", "troubleshooting", "sre", "read-only"],
  "mcpServers": {
    "kubeagent": {
      "command": "kubeagent",
      "args": ["mcp", "--allow-context-switch", "--logs"]
    }
  }
}
```

The server is declared inline rather than in a separate `.mcp.json`. One file
fewer, and the manifest test has one place to look.

`version` is a fourth home for the release version, after the chart, the
CHANGELOG and the deploy manifest. `scripts/bump-version.sh` is extended to cover
it; the manifest test pins it to the chart.

### `marketplace.json`

One marketplace, one plugin, `"source": "./"` — the repository is its own
marketplace. Install becomes:

```text
/plugin marketplace add imantaba/kubeagent
/plugin install kubeagent@kubeagent
```

## The skills

### `triaging-a-cluster`

The workflow. Ordered, with stop conditions, because an unbounded model doing
Kubernetes triage will keep calling tools to look thorough.

1. **Preflight.** Run `command -v kubeagent`. If absent, print the three install
   paths — `go install`, `kubectl krew install --manifest-url=…`, the release
   archive — and stop. This is the whole of the binary-dependency handling: the
   plugin cannot ship a Go binary, so the skill explains the prerequisite at the
   moment it matters.
2. **Always call `kubeagent_triage` first**, namespace-scoped if the user named a
   namespace. Never open with `kubeagent_inspect`: you do not yet know what is
   broken, and guessing an object name wastes a call.
3. **Read `coverage` before `findings`.** Not a suggestion. The interpretation
   skill says what to do with it.
4. **Escalate from the findings, not from intuition.** For each critical or
   high-severity finding, call `kubeagent_inspect` with the kind, namespace and
   name the finding already supplied.
5. **Advisory sections are opt-in and cost extra reads.** Call a section only
   when a finding points at it: certificate expiry → `certificates`, scheduling
   pressure → `capacity`, unhealthy operator custom resources → `operators`. Not
   speculatively.
6. **Stop.** Verdict `healthy` with an empty `partial` list means report healthy
   and stop. Continuing to dig produces noise, not confidence.
7. **Never write.** If the user wants remediation, give them the CLI line to
   type. Do not run it. This is where the read-only promise becomes behaviour
   rather than merely an absent tool.

### `reading-kubeagent-findings`

The semantics. Exists to prevent one specific failure: a model treating an absent
key as good news.

- **`checksSkipped` is not `passed`.** Seven checks are skipped on every
  `kubeagent_triage` call. Three of them — kubelet health, control-plane health,
  DNS health — are not reachable through the MCP server at all; only the CLI's
  `--kubelet-health`, `--control-plane-health` and `--dns-health` flags run them.
  So a triage result is never grounds for saying "DNS is fine".
- **`partial` is a blind spot, not a clean result.** It names a resource kubeagent
  tried to list and could not, typically an RBAC refusal. An empty section under a
  `partial` entry gets reported as unknown, which is exactly what the
  least-privilege RBAC slice built it to express.
- **`metricsServer: "not-checked"` means never looked.** It becomes `available` or
  `absent` only when a call requests capacity data. Reading the literal
  `"not-checked"` as "no metrics problem" silently misses a missing metrics-server.
- **`severity` and `confidence` are independent axes.** High severity with low
  confidence is a lead to verify, not a conclusion to report.
- **Findings are computed, not generated.** Every field comes from a detector, the
  same way `scan` computes it. Quote `reason` and `detail`; do not paraphrase them
  into a stronger claim than kubeagent made. `remediationHint` is kubeagent's
  suggestion. Any other suggestion is the model's, and is labelled as the model's.

## The commands

Three, each a multi-step workflow. A command that wraps one tool call earns
nothing over letting the model call the tool.

| Command | Behaviour |
|---|---|
| `/kubeagent:triage [namespace]` | Sweep, then auto-inspect every critical and high finding, then a ranked report that states its coverage caveats |
| `/kubeagent:why <kind>/<name> [-n ns]` | Root-cause one object: inspect it, read its events, run the one advisory section its finding implies, explain |
| `/kubeagent:preflight [namespace]` | Pre-deploy gate: triage plus the `drift` and `capacity` advisory sections → one go/no-go verdict with blind spots listed |

## Testing

Skills and commands are markdown; there is nothing to unit-test in them. What can
be tested is whether they still describe kubeagent truthfully, and that is what
the pin test does. Each test lives in the package whose symbols it checks.

### `plugin_manifest_test.go` (repo root)

- Both manifests parse as JSON; required fields are non-empty.
- `plugin.json`'s `version` equals `appVersion` in
  `deploy/helm/kubeagent/Chart.yaml`, minus the leading `v`. A release that bumps
  every other version reference and forgets the plugin fails here.
- The marketplace entry's `source` resolves to a directory containing
  `.claude-plugin/plugin.json`.
- **Tool-name drift.** Derive the registered tool names from `internal/mcp/*.go`
  by regexp over the `Name:` fields, then assert that every `kubeagent_*` and
  `list_contexts` mentioned in `skills/**/SKILL.md` and `commands/*.md` is in that
  set. The check fails closed on the documentation side: a skill naming a deleted
  tool breaks the build. A tool registered under a computed name would need this
  test updated, but no tool is registered that way and the failure direction that
  matters — docs promising a tool that no longer exists — is covered.

### `internal/cli/plugin_flags_test.go`

Reads `../../.claude-plugin/plugin.json`, asserts `args[0] == "mcp"`, and feeds
`args[1:]` to the real, unexported `parseMCPFlags`. A renamed or removed flag
fails the test. This uses the actual parser rather than a duplicated list of flag
names, and it adds no exported API that exists only for a test — which is why it
lives here rather than at the repo root.

### Manual acceptance

Automated tests cannot tell whether the skills make Claude behave better. Before
merge, install the plugin locally with `/plugin marketplace add ./`, confirm
`/mcp` reports kubeagent connected, and run all three commands against a kind
cluster from `chaos/` — including at least one scenario with a deliberately
restricted RBAC identity, to confirm a `partial` entry is reported as a blind
spot rather than silently dropped.

## Invariants preserved

- **Read-only.** The plugin adds no write path. `internal/mcp` still cannot reach
  `internal/remediate`, and now neither can any skill or command.
- **No LLM call by kubeagent.** Unchanged, and worth restating because a plugin
  makes it easy to blur: the calling model is Claude; kubeagent's half of the
  conversation is still entirely computed by detectors.
- **The six versioned JSON documents do not move.** The plugin ships no new
  document and changes no field, type or enum value in an existing one. No
  `schemaVersion` bump, no `internal/jsonschema` regeneration.
- **No new Go dependency.** `internal/cli/plugin_flags_test.go` needs only the
  standard library. `plugin_manifest_test.go` additionally reads `Chart.yaml`,
  using the `sigs.k8s.io/yaml` that `krew_manifest_test.go` already imports.

## Compatibility classification

The plugin is a **new surface**, so nothing stable breaks. Per
`website/docs/compatibility.md`, it enters as an addition in a MINOR release.

The manifest's `mcpServers` block is the part that will be tempting to change
later — the flag set in particular. Changing `args` alters the behaviour of an
already-installed plugin on upgrade, so it is treated as a stable surface from
this release onward: adding a flag is MINOR, removing one or changing what an
existing flag means is MAJOR. The compatibility page gains a row saying so.

## Documentation

- **`website/docs/features/claude-plugin.md`** (new) — install, contents, the
  prerequisite binary, the read-only promise stated as two separate claims (no
  writes; no LLM call by kubeagent), why `--allow-context-switch` is on by default
  and what it can and cannot reach, and a pointer to `mcp.md` for the tool
  reference rather than a second copy of it.
- **`website/mkdocs.yml`** — nav entry beside the MCP page.
- **`README.md`** — a section beside "As a `kubectl` plugin (krew)".
- **`CHANGELOG.md`** — under `[Unreleased]`.
- **`CLAUDE.md`** — the plugin as a third distribution surface; the read-only
  promise; the `.claude/skills/` versus `skills/` distinction.
- **`website/docs/roadmap.md`** — Theme G notes the third distribution surface.

## Sequencing

1. Manifests (`plugin.json`, `marketplace.json`) and the two test files. TDD: the
   tests are written first and fail against absent manifests.
2. `scripts/bump-version.sh` extended to cover `plugin.json`, with its existing
   staleness check updated.
3. The two skills.
4. The three commands.
5. Docs, in the order listed above.
6. Manual acceptance against a kind cluster, including the restricted-RBAC
   scenario.
