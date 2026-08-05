# kubeagent as a Claude Code plugin — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship kubeagent as an installable Claude Code plugin, hosted from this repository, that wires the existing `kubeagent mcp` server and teaches the calling model how to use it correctly.

**Architecture:** No new Go production code. Two JSON manifests at `.claude-plugin/`, two user-facing skills under `skills/`, three slash commands under `commands/`, and two Go test files that fail the build when any of that drifts from what kubeagent actually registers. `scripts/bump-version.sh` gains the manifest as a fifth version home.

**Tech Stack:** Go 1.26 (tests only — standard library plus the already-imported `sigs.k8s.io/yaml`), JSON manifests, Markdown skills and commands, Bash.

## Global Constraints

- Branch `claude-code-plugin` is already cut off `main`. Never commit to `main`.
- **No `Co-Authored-By: Claude` trailer** on any commit, and no Claude/Anthropic attribution anywhere in commit messages, code, or docs.
- **Read-only.** No file created by this plan may invoke `kubeagent … --fix` or instruct a model to. No new Go production code at all, so no import-graph invariant can move.
- **No new Go dependency.** `internal/cli/plugin_flags_test.go` uses only the standard library. `plugin_manifest_test.go` additionally uses `sigs.k8s.io/yaml` (already a direct dependency at `v1.6.0`, already imported by `krew_manifest_test.go`).
- **No `schemaVersion` moves.** This plan ships no new JSON document and changes no field, type, or enum value in the six versioned ones. Do not run `go test ./internal/schemadoc -run TestSchemaDrift -update`.
- The current release version is **`1.2.0`** (source of truth: `appVersion: "v1.2.0"` in `deploy/helm/kubeagent/Chart.yaml`). `plugin.json`'s `version` carries **no** `v` prefix.
- The four MCP tool names, verbatim: `kubeagent_triage`, `kubeagent_inspect`, `kubeagent_advisory`, `list_contexts`.
- The four `kubeagent mcp` flags, verbatim: `--kubeconfig`, `--context`, `--allow-context-switch`, `--logs`.
- PATH setup for every Go command: `export PATH=$PATH:/usr/local/go/bin`.
- Repo Markdown convention: table separator rows are spaceless (`|---|---|`), and every fenced code block declares a language. The Markdown linter warns about the former; match the repo, not the linter.
- Do not stage `docs/superpowers/specs/2026-08-05-pagerduty-receiver-design.md`. It is an unrelated untracked file in the working tree. Always `git add` by explicit path.

---

## File Structure

| File | Responsibility |
|---|---|
| `.claude-plugin/plugin.json` | Plugin manifest. Declares the MCP server inline. |
| `.claude-plugin/marketplace.json` | Marketplace manifest. One entry, `source: "./"`. |
| `skills/triaging-a-cluster/SKILL.md` | The workflow: tool order, escalation rule, stop conditions, no-write rule. |
| `skills/reading-kubeagent-findings/SKILL.md` | The semantics: how to read `coverage`, `severity`, `confidence`. |
| `commands/triage.md` | `/kubeagent:triage` — sweep, auto-inspect, ranked report. |
| `commands/why.md` | `/kubeagent:why` — root-cause one object. |
| `commands/preflight.md` | `/kubeagent:preflight` — pre-deploy go/no-go. |
| `plugin_manifest_test.go` | Manifest shape, version pin, marketplace source, tool-name drift. |
| `internal/cli/plugin_flags_test.go` | The manifest's `args` parse through the real `mcp` flag parser. |
| `scripts/bump-version.sh` | Modified: `plugin.json` becomes a fifth version home. |
| `bump_version_plugin_test.go` | Executes a copy of the bump script against a fixture tree. |
| `website/docs/features/claude-plugin.md` | The feature page. |

---

### Task 1: Manifests and the manifest shape test

**Files:**
- Create: `.claude-plugin/plugin.json`
- Create: `.claude-plugin/marketplace.json`
- Test: `plugin_manifest_test.go` (repo root, package `main`)

**Interfaces:**
- Consumes: nothing.
- Produces: `.claude-plugin/plugin.json` with `mcpServers.kubeagent.args == ["mcp", "--allow-context-switch", "--logs"]` and `version == "1.2.0"`. Task 2 reads that `args` array; Task 6 rewrites that `version` string. `plugin_manifest_test.go` declares `func readPluginManifest(t *testing.T) pluginManifest`, reused by Tasks 3–5.

- [ ] **Step 1: Write the failing test**

Create `plugin_manifest_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Claude Code plugin manifests are hand-written JSON that makes promises
// about kubeagent: which binary to run, which flags it accepts, which version
// it is. Nothing in the Go build would otherwise notice when one of those
// promises stops being true, so these tests are the only thing standing
// between a renamed flag and a plugin that fails on every call.

const (
	pluginManifestPath      = ".claude-plugin/plugin.json"
	marketplaceManifestPath = ".claude-plugin/marketplace.json"
	chartPath               = "deploy/helm/kubeagent/Chart.yaml"
)

type pluginManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	MCPServers  map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcpServers"`
}

type marketplaceManifest struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		Description string `json:"description"`
	} `json:"plugins"`
}

// readPluginManifest parses .claude-plugin/plugin.json or fails the test.
// Tasks 3-5 reuse it.
func readPluginManifest(t *testing.T) pluginManifest {
	t.Helper()
	raw, err := os.ReadFile(pluginManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", pluginManifestPath, err)
	}
	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing %s: %v", pluginManifestPath, err)
	}
	return m
}

func TestPluginManifestShape(t *testing.T) {
	m := readPluginManifest(t)

	if m.Name != "kubeagent" {
		t.Errorf("name = %q, want %q", m.Name, "kubeagent")
	}
	if m.Description == "" {
		t.Error("description is empty; it is what the marketplace listing shows")
	}
	if m.Version == "" {
		t.Error("version is empty")
	}

	srv, ok := m.MCPServers["kubeagent"]
	if !ok {
		t.Fatalf("mcpServers has no %q entry; got keys %v", "kubeagent", mapKeys(m.MCPServers))
	}
	if srv.Command != "kubeagent" {
		t.Errorf("mcpServers.kubeagent.command = %q, want %q", srv.Command, "kubeagent")
	}
	if len(srv.Args) == 0 || srv.Args[0] != "mcp" {
		t.Errorf("mcpServers.kubeagent.args = %v, want it to start with %q", srv.Args, "mcp")
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPluginVersionMatchesChart pins the manifest to the chart's appVersion,
// which scripts/bump-version.sh treats as the single source of truth. A
// release that bumps every other version reference and forgets the plugin
// fails here rather than shipping a manifest claiming the previous release.
func TestPluginVersionMatchesChart(t *testing.T) {
	raw, err := os.ReadFile(chartPath)
	if err != nil {
		t.Fatalf("reading %s: %v", chartPath, err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatalf("parsing %s: %v", chartPath, err)
	}
	want := chart.AppVersion
	if want == "" {
		t.Fatalf("%s has no appVersion", chartPath)
	}
	// The chart spells it "v1.2.0"; the plugin manifest spells it "1.2.0".
	want = want[1:]

	if got := readPluginManifest(t).Version; got != want {
		t.Errorf("plugin.json version = %q, chart appVersion implies %q\n"+
			"run scripts/bump-version.sh, or fix the manifest by hand", got, want)
	}
}

// TestMarketplaceEntryResolves checks that the marketplace points at a
// directory that actually holds a plugin manifest. A typo'd source path
// produces a marketplace that installs nothing, with no other signal.
func TestMarketplaceEntryResolves(t *testing.T) {
	raw, err := os.ReadFile(marketplaceManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", marketplaceManifestPath, err)
	}
	var mp marketplaceManifest
	if err := json.Unmarshal(raw, &mp); err != nil {
		t.Fatalf("parsing %s: %v", marketplaceManifestPath, err)
	}
	if mp.Name == "" {
		t.Error("marketplace name is empty")
	}
	if len(mp.Plugins) != 1 {
		t.Fatalf("want exactly 1 plugin entry, got %d", len(mp.Plugins))
	}

	entry := mp.Plugins[0]
	if entry.Name != "kubeagent" {
		t.Errorf("plugin entry name = %q, want %q", entry.Name, "kubeagent")
	}
	if entry.Description == "" {
		t.Error("plugin entry description is empty")
	}

	manifest := filepath.Join(entry.Source, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("marketplace source %q does not resolve to a plugin: %v", entry.Source, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestPluginManifestShape|TestPluginVersionMatchesChart|TestMarketplaceEntryResolves' -v
```

Expected: FAIL. Every test fails at `reading .claude-plugin/plugin.json: ... no such file or directory` (and the marketplace test on its own missing file).

- [ ] **Step 3: Create the plugin manifest**

Create `.claude-plugin/plugin.json`:

```json
{
  "name": "kubeagent",
  "description": "Read-only Kubernetes troubleshooting: deterministic diagnosis over MCP, no LLM calls of its own.",
  "version": "1.2.0",
  "author": { "name": "imantaba" },
  "homepage": "https://github.com/imantaba/kubeagent",
  "repository": "https://github.com/imantaba/kubeagent",
  "license": "Apache-2.0",
  "keywords": ["kubernetes", "troubleshooting", "sre", "read-only", "mcp"],
  "mcpServers": {
    "kubeagent": {
      "command": "kubeagent",
      "args": ["mcp", "--allow-context-switch", "--logs"]
    }
  }
}
```

- [ ] **Step 4: Create the marketplace manifest**

Create `.claude-plugin/marketplace.json`:

```json
{
  "name": "kubeagent",
  "owner": { "name": "imantaba", "url": "https://github.com/imantaba" },
  "plugins": [
    {
      "name": "kubeagent",
      "source": "./",
      "description": "Read-only Kubernetes troubleshooting over MCP, with skills that teach a model how to read a coverage block."
    }
  ]
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestPluginManifestShape|TestPluginVersionMatchesChart|TestMarketplaceEntryResolves' -v
```

Expected: PASS, three tests.

- [ ] **Step 6: Commit**

```bash
git add .claude-plugin/plugin.json .claude-plugin/marketplace.json plugin_manifest_test.go
git commit -m "plugin: add the Claude Code plugin and marketplace manifests

The manifests declare the existing kubeagent mcp server inline, with
--allow-context-switch and --logs. Three tests pin their shape, pin the
version to the chart's appVersion so a release cannot ship a stale manifest,
and check that the marketplace source resolves to a real plugin directory."
```

---

### Task 2: Pin the manifest's flags to the real flag parser

**Files:**
- Test: `internal/cli/plugin_flags_test.go` (create, package `cli`)

**Interfaces:**
- Consumes: `.claude-plugin/plugin.json` from Task 1; the unexported `parseMCPFlags(args []string) (mcpOptions, error)` already in `internal/cli/mcp.go`.
- Produces: nothing other tasks consume.

**Why this test lives here and not at the repo root:** `parseMCPFlags` is unexported. Testing the manifest's flags against the *real* parser — rather than against a hand-copied list of flag names that would rot in exactly the same way — means the test must live in package `cli`. That is worth more than keeping every manifest test in one file, and it avoids exporting a symbol that exists only for a test.

- [ ] **Step 1: Write the test**

Create `internal/cli/plugin_flags_test.go`:

```go
package cli

import (
	"encoding/json"
	"os"
	"testing"
)

// The Claude Code plugin manifest hard-codes the command line that starts the
// MCP server. Renaming or removing an `mcp` flag would leave the manifest
// syntactically valid and behaviourally broken: every install would fail at
// startup, with the error going to a subprocess's stderr where nobody looks.
//
// This runs the manifest's own arguments through the real flag parser, so the
// declarations in bindMCPFlags are the single source of truth. A duplicated
// list of flag names here would rot the same way the manifest does.
func TestPluginManifestMCPArgsParse(t *testing.T) {
	raw, err := os.ReadFile("../../.claude-plugin/plugin.json")
	if err != nil {
		t.Fatalf("reading the plugin manifest: %v", err)
	}
	var manifest struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parsing the plugin manifest: %v", err)
	}

	srv, ok := manifest.MCPServers["kubeagent"]
	if !ok {
		t.Fatal("the plugin manifest declares no kubeagent MCP server")
	}
	if srv.Command != "kubeagent" {
		t.Fatalf("command = %q, want %q", srv.Command, "kubeagent")
	}
	if len(srv.Args) == 0 || srv.Args[0] != "mcp" {
		t.Fatalf("args = %v, want the first element to be %q", srv.Args, "mcp")
	}

	opts, err := parseMCPFlags(srv.Args[1:])
	if err != nil {
		t.Fatalf("the manifest's flags do not parse: %v\n"+
			"args were %v — fix .claude-plugin/plugin.json to match bindMCPFlags",
			err, srv.Args)
	}

	// The two flags the design chose deliberately. If a future change turns
	// either off, that is a behaviour change for every installed plugin and
	// should be a deliberate edit here, not a silent one in the manifest.
	if !opts.allowContextSwitch {
		t.Error("the manifest no longer passes --allow-context-switch")
	}
	if !opts.logs {
		t.Error("the manifest no longer passes --logs")
	}
}
```

- [ ] **Step 2: Run the test — it passes, which is not yet proof**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli -run TestPluginManifestMCPArgsParse -v
```

Expected: PASS. A guard test that passes on first write has not been shown to have teeth. Step 3 shows it.

- [ ] **Step 3: Mutate the manifest and watch the test fail**

Temporarily change the `args` line in `.claude-plugin/plugin.json` to:

```json
      "args": ["mcp", "--allow-context-swtich", "--logs"]
```

(note the transposed letters — this is exactly the class of typo the test exists to catch), then run:

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli -run TestPluginManifestMCPArgsParse -v
```

Expected: FAIL with `the manifest's flags do not parse: unknown flag: --allow-context-swtich`.

- [ ] **Step 4: Revert the mutation and confirm green**

```bash
git checkout .claude-plugin/plugin.json
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli -run TestPluginManifestMCPArgsParse -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/plugin_flags_test.go
git commit -m "test: parse the plugin manifest's mcp flags with the real parser

bindMCPFlags stays the single source of truth for which flags exist. A
duplicated list of flag names in the test would rot exactly the way the
manifest does, so the manifest's own args go through parseMCPFlags instead."
```

---

### Task 3: The workflow skill, guarded by a tool-name drift test

**Files:**
- Create: `skills/triaging-a-cluster/SKILL.md`
- Modify: `plugin_manifest_test.go` (append)

**Interfaces:**
- Consumes: `readPluginManifest` is not needed here; the new test reads `internal/mcp/*.go` and `skills/**/SKILL.md` directly.
- Produces: `plugin_manifest_test.go` gains `func registeredMCPTools(t *testing.T) map[string]bool` and `var pluginDocFiles []string`. Tasks 4 and 5 append paths to `pluginDocFiles`.

**Why the test needs an explicit file list:** a drift test that merely walks `skills/` and checks whatever it finds passes vacuously when the directory is missing or a file is deleted. `pluginDocFiles` is an explicit required-paths list, so deleting a skill fails the build.

- [ ] **Step 1: Write the failing test**

Append to `plugin_manifest_test.go`:

```go
// pluginDocFiles is the explicit list of shipped skill and command files.
// It is a required-paths list, not a directory walk: a walk would pass
// vacuously if the directory went missing, which is the failure most worth
// catching. Tasks that add a skill or command add its path here.
var pluginDocFiles = []string{
	"skills/triaging-a-cluster/SKILL.md",
}

// toolNameRE matches an MCP tool name wherever it appears — in a Go
// registration or in prose.
var toolNameRE = regexp.MustCompile(`\b(kubeagent_[a-z_]+|list_contexts)\b`)

// registrationRE matches only the Name field of a tool registration, so
// prose inside internal/mcp (a coverage "why" string naming another tool,
// for instance) is not mistaken for a registration.
var registrationRE = regexp.MustCompile(`\bName:\s*"(kubeagent_[a-z_]+|list_contexts)"`)

// registeredMCPTools derives the set of tool names internal/mcp actually
// registers, by reading the source. Deriving it beats hard-coding it: a tool
// renamed in Go is then a test failure in the docs, not a silent lie.
func registeredMCPTools(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir("internal/mcp")
	if err != nil {
		t.Fatalf("reading internal/mcp: %v", err)
	}
	tools := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("internal/mcp", name))
		if err != nil {
			t.Fatalf("reading internal/mcp/%s: %v", name, err)
		}
		for _, m := range registrationRE.FindAllStringSubmatch(string(raw), -1) {
			tools[m[1]] = true
		}
	}
	if len(tools) == 0 {
		t.Fatal("found no registered MCP tools in internal/mcp; the registration " +
			"pattern changed and this test no longer checks anything")
	}
	return tools
}

// TestShippedDocsNameOnlyRegisteredTools fails closed on the documentation
// side: a skill or command that tells the model to call a tool kubeagent no
// longer registers breaks the build. That is the drift that costs a user
// something — the model follows the instruction and the call errors.
func TestShippedDocsNameOnlyRegisteredTools(t *testing.T) {
	registered := registeredMCPTools(t)

	for _, path := range pluginDocFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		mentioned := toolNameRE.FindAllString(string(raw), -1)
		if len(mentioned) == 0 {
			t.Errorf("%s names no MCP tool at all; it is supposed to teach the "+
				"model how to call them", path)
			continue
		}
		for _, name := range mentioned {
			if !registered[name] {
				t.Errorf("%s names tool %q, which internal/mcp does not register "+
					"(registered: %v)", path, name, sortedKeys(registered))
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

Extend the import block at the top of `plugin_manifest_test.go` to:

```go
import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: FAIL with `reading skills/triaging-a-cluster/SKILL.md: ... no such file or directory`.

- [ ] **Step 3: Write the skill**

Create `skills/triaging-a-cluster/SKILL.md`:

````markdown
---
name: triaging-a-cluster
description: Use when asked to diagnose a Kubernetes cluster, namespace, or workload with kubeagent - establishes the tool order, when to escalate, when to stop, and the rule that nothing here ever writes to the cluster.
---

# Triaging a cluster with kubeagent

kubeagent's MCP tools return findings computed by detectors, not generated text.
Your job is to call them in the right order, read what comes back honestly, and
stop when there is nothing left to learn.

## Step 0: preflight

The plugin cannot ship kubeagent's binary. Before the first call, check it is
installed:

```bash
command -v kubeagent
```

If that produces nothing, stop and tell the user to install it, offering all
three paths:

```bash
go install github.com/imantaba/kubeagent@latest
```

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
```

Or download a prebuilt binary from
<https://github.com/imantaba/kubeagent/releases>.

Do not attempt the diagnosis without it. The MCP tools will simply fail.

## Step 1: always start with kubeagent_triage

Call `kubeagent_triage` first, every time. Pass `namespace` if the user named
one; omit it for a whole-cluster sweep.

Never open with `kubeagent_inspect`. You do not yet know what is broken, so you
would be guessing at an object name, and a guess costs a call and returns
nothing.

If the user named a cluster rather than a namespace, call `list_contexts` first
to find its exact context name, then pass that as `context`.

## Step 2: read coverage before findings

Read the `coverage` block before you read a single finding. An empty `findings`
list means "no finding was produced", which is not the same as "the cluster is
healthy" — the checks that would have produced one may never have run.

The `reading-kubeagent-findings` skill covers how. Follow it. This is the single
most common way to report a cluster healthy when it is not.

## Step 3: escalate from the findings, not from intuition

Every finding carries a `severity` of `critical` (a detector matched a concrete
failure mode) or `warning` (a health check flagged something that needs a
human). Those are the only two values.

Call `kubeagent_inspect` on every `critical` finding, using the `kind`,
`namespace`, and `name` the finding already gave you. It returns that object's
status, its pods, kubeagent's findings for it, and its recent Kubernetes events.
Inspect a `warning` when it sits inside the scope the user asked about — most of
kubeagent's Service, Ingress, PVC, PodDisruptionBudget, HPA and quota findings
are warnings, so skipping them wholesale is how a real problem gets dismissed as
noise.

Do not inspect objects no finding pointed at. Do not invent names.

## Step 4: call advisory sections only when a finding points at one

`kubeagent_advisory` sections each cost extra API reads, so they are opt-in.
Call one when a finding implies it, not speculatively:

| Finding is about | Section |
|---|---|
| Certificate expiry or TLS | `certificates` |
| Pending pods, scheduling pressure, resource limits | `capacity` |
| Unhealthy operator custom resources | `operators` |
| Live state diverging from Git | `drift` |
| Privileged pods, host mounts, weak security context | `security` |

Requesting all five on a healthy cluster wastes reads and produces noise you
will then have to explain away.

## Step 5: stop

Stop when either is true:

- The verdict is `healthy` and `coverage.partial` is empty. Say so and stop.
  Continuing to dig produces noise, not confidence.
- You have inspected every `critical` finding, plus the `warning` findings in
  the user's scope, and reported them.

Then write the report: the verdict, the findings ranked by severity, and — always
— what was not checked, in the user's words rather than as a raw JSON dump.

## Never write to the cluster

kubeagent has a guard-railed `--fix` mode. It is not reachable from here, and
that is deliberate. No MCP tool can write, and this skill does not shell out to
the CLI to work around that.

If the user wants remediation, give them the command to run themselves:

```bash
kubeagent scan --namespace <ns> --fix
```

Tell them it will ask for confirmation per action and re-verify afterwards. Do
not run it for them. Do not run `kubectl delete`, `kubectl patch`, `kubectl
rollout restart`, or any other mutation as a substitute.
````

- [ ] **Step 4: Run the test to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: PASS.

- [ ] **Step 5: Mutate the skill and watch the test fail**

Temporarily change the line `Call \`kubeagent_triage\` first, every time.` in the skill to name `kubeagent_triage_all`, then:

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: FAIL with `names tool "kubeagent_triage_all", which internal/mcp does not register`.

Revert with `git checkout skills/triaging-a-cluster/SKILL.md` — or, since the file is not yet tracked, re-edit the line back by hand — and re-run to confirm PASS.

- [ ] **Step 6: Commit**

```bash
git add skills/triaging-a-cluster/SKILL.md plugin_manifest_test.go
git commit -m "plugin: add the triaging-a-cluster workflow skill

Tool order, an escalation rule that reads object names off the findings rather
than guessing them, explicit stop conditions, and the no-write rule stated as
behaviour rather than left implicit in the absence of a write tool.

A drift test derives the registered tool names from internal/mcp and fails the
build when a shipped skill names one that no longer exists."
```

---

### Task 4: The interpretation skill

**Files:**
- Create: `skills/reading-kubeagent-findings/SKILL.md`
- Modify: `plugin_manifest_test.go` (the `pluginDocFiles` list)

**Interfaces:**
- Consumes: `pluginDocFiles` from Task 3.
- Produces: one more entry in `pluginDocFiles`.

- [ ] **Step 1: Extend the required-paths list so the test fails**

In `plugin_manifest_test.go`, change `pluginDocFiles` to:

```go
var pluginDocFiles = []string{
	"skills/triaging-a-cluster/SKILL.md",
	"skills/reading-kubeagent-findings/SKILL.md",
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: FAIL with `reading skills/reading-kubeagent-findings/SKILL.md: ... no such file or directory`.

- [ ] **Step 3: Write the skill**

Create `skills/reading-kubeagent-findings/SKILL.md`:

````markdown
---
name: reading-kubeagent-findings
description: Use when reading a result from any kubeagent MCP tool - explains the coverage block, the difference between a skipped check and a passing one, and why severity and confidence are independent axes.
---

# Reading a kubeagent result

Every field in a kubeagent result is computed by a detector. None of it is
generated prose. Read it precisely and it will not mislead you; read it loosely
and it will.

This skill exists to prevent one specific mistake: **reading an absent key as
good news.**

## Read coverage first

`kubeagent_triage`, `kubeagent_inspect`, and `kubeagent_advisory` each return a
`coverage` object. It exists so you can tell "nothing is wrong" from "nothing
was checked". Read it before the findings.

### checksSkipped is not "passed"

`checksRun` names the checks that executed. `checksSkipped` names the ones that
did not, each with a reason. A skipped check produced no finding because it never
ran.

Seven checks are skipped on **every** `kubeagent_triage` call. Three of them —
kubelet health, control-plane health, DNS health — are not reachable through the
MCP server at all. Only the CLI's `--kubelet-health`, `--control-plane-health`,
and `--dns-health` flags run them.

So a triage result is never grounds for saying "DNS is fine". If the user asks
about DNS, tell them the tool did not check and give them the CLI command:

```bash
kubeagent scan --dns-health
```

Two more — the security and certificate sections — are skipped by triage but
*are* reachable: call `kubeagent_advisory` with the section you need.

### partial is a blind spot

`partial` names a resource kubeagent tried to list and could not — most often an
RBAC refusal. An empty section under a `partial` entry means **unknown**, not
**clean**.

Report it as a blind spot, and name the resource. "No NetworkPolicy problems
found" is wrong when NetworkPolicies are in `partial`. "kubeagent could not read
NetworkPolicies, so that is unchecked" is right.

### metricsServer: "not-checked" means never looked

`coverage.metricsServer` is the literal string `"not-checked"` until a call
requests capacity data — that is, until `kubeagent_advisory` runs with section
`capacity`. Only then does it become `"available"` or `"absent"`.

Reading `"not-checked"` as "no metrics problem" silently misses a cluster with no
metrics-server installed at all.

## severity and confidence are independent

`severity` is how bad it would be: `critical` when a detector matched a concrete
failure mode, `warning` when a health check flagged something that needs a
human. Those are the only two values. `confidence` is how sure kubeagent is:
`high` when the state is one Kubernetes itself asserts, `medium` when it is a
kubeagent heuristic. The two vocabularies do not overlap — there is no `high`
severity and no `critical` confidence.

A `critical` finding carrying `medium` confidence is a **lead to verify**, not a
conclusion to report. Escalate it with `kubeagent_inspect` and read the object's
events before you tell the user their production database is failing.

## verdict is derived, not separate

`verdict` is `healthy` or `degraded`, computed from the findings. It is a summary
of them, not an additional independent judgement. Do not report a verdict that
contradicts the findings list you are also showing.

## Quote findings; do not strengthen them

`reason` and `detail` are what the detector concluded. Quote them or paraphrase
them faithfully. Do not upgrade "container repeatedly crashes after starting"
into "the application is broken" — kubeagent observed the former and did not
claim the latter.

`remediationHint` is kubeagent's suggestion. Anything else you suggest is yours,
and say so plainly: "kubeagent suggests X; separately, I would check Y."
````

- [ ] **Step 4: Run the test to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add skills/reading-kubeagent-findings/SKILL.md plugin_manifest_test.go
git commit -m "plugin: add the reading-kubeagent-findings skill

The coverage block, the least-privilege RBAC blind spots and the
'not-checked' metrics sentinel only pay off if the reader knows what they
mean. This is that instruction: a skipped check is not a passing one, a
partial entry is unknown rather than clean, and severity and confidence are
independent axes."
```

---

### Task 5: The three slash commands

**Files:**
- Create: `commands/triage.md`
- Create: `commands/why.md`
- Create: `commands/preflight.md`
- Modify: `plugin_manifest_test.go` (the `pluginDocFiles` list)

**Interfaces:**
- Consumes: `pluginDocFiles` from Tasks 3 and 4; both skills by name.
- Produces: three more entries in `pluginDocFiles`.

Each command is a multi-step workflow. A command that wraps a single tool call earns nothing over letting the model call the tool, so none of these do that.

- [ ] **Step 1: Extend the required-paths list so the test fails**

In `plugin_manifest_test.go`, change `pluginDocFiles` to:

```go
var pluginDocFiles = []string{
	"skills/triaging-a-cluster/SKILL.md",
	"skills/reading-kubeagent-findings/SKILL.md",
	"commands/triage.md",
	"commands/why.md",
	"commands/preflight.md",
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: FAIL, three times, one `no such file or directory` per missing command.

- [ ] **Step 3: Write `commands/triage.md`**

```markdown
---
description: Sweep a cluster or namespace with kubeagent, inspect every serious finding, and report what was and was not checked.
argument-hint: "[namespace]"
---

Triage the cluster with kubeagent. Namespace scope: $1 (empty means all namespaces).

Follow the `triaging-a-cluster` skill for the workflow and the
`reading-kubeagent-findings` skill for how to read what comes back.

1. Preflight: confirm `kubeagent` is on PATH. If not, stop and give the install
   commands.
2. Call `kubeagent_triage`, passing `namespace` only if $1 is non-empty.
3. Read the `coverage` block before the findings.
4. Call `kubeagent_inspect` on every `critical` finding, and on each `warning`
   finding inside the namespace scope, passing that finding's `kind`,
   `namespace`, and `name`. `critical` and `warning` are the only two
   severities kubeagent emits.
5. Do not call `kubeagent_advisory` unless a finding points at a specific
   section.

Report, in this order:

- The verdict, in one sentence.
- Findings ranked by severity, each with what `kubeagent_inspect` added — the
  pod state and the events that explain it.
- **What was not checked.** Name the skipped checks that matter for the user's
  question, and every entry in `coverage.partial` as a blind spot rather than a
  clean result.

Do not remediate. If a fix is obvious, give the user the `kubeagent scan --fix`
command to run themselves and say that it confirms each action.
```

- [ ] **Step 4: Write `commands/why.md`**

```markdown
---
description: Root-cause one Kubernetes object with kubeagent - its findings, its events, and the one advisory section its failure implies.
argument-hint: "<kind>/<name> [-n namespace]"
---

Explain why this object is unhealthy: $ARGUMENTS

Follow the `triaging-a-cluster` skill and the `reading-kubeagent-findings`
skill.

1. Preflight: confirm `kubeagent` is on PATH.
2. Parse the argument into a kind and a name. Valid kinds are `pod`,
   `deployment`, `statefulset`, `daemonset`, `replicaset`, `job`, and
   `cronjob`. If the namespace was not given with `-n`, ask for it rather than
   guessing — `kubeagent_inspect` requires it.
3. Call `kubeagent_inspect` with that kind, namespace, and name.
4. Read its findings and its recent Kubernetes events together. The events
   usually carry the sentence the finding summarises.
5. Call `kubeagent_advisory` for **at most one** section, and only if a finding
   points at it: `certificates` for TLS expiry, `capacity` for scheduling or
   resource pressure, `operators` for an unhealthy custom resource, `drift` for
   divergence from Git, `security` for a privileged or host-mounted pod.

Report:

- What is wrong, quoting the finding's `reason` and `detail` rather than
  strengthening them.
- The evidence: the events and container state that support it.
- kubeagent's `remediationHint` if there is one, labelled as kubeagent's. Any
  further suggestion of your own, labelled as yours.
- Anything `coverage` says was not checked that bears on this object.

Do not remediate.
```

- [ ] **Step 5: Write `commands/preflight.md`**

```markdown
---
description: Pre-deploy go/no-go check - kubeagent triage plus the drift and capacity advisory sections, with blind spots listed.
argument-hint: "[namespace]"
---

Run a pre-deploy readiness check. Namespace scope: $1 (empty means all namespaces).

Follow the `triaging-a-cluster` skill and the `reading-kubeagent-findings`
skill.

1. Preflight: confirm `kubeagent` is on PATH.
2. Call `kubeagent_triage`, passing `namespace` only if $1 is non-empty.
3. Call `kubeagent_advisory` once, with sections `drift` and `capacity`. These
   two are the pre-deploy questions: is live state already diverging from Git,
   and is there room to schedule what is about to land. Both are requested
   unconditionally here — unlike ordinary triage — because that is what makes
   this a gate.
4. Inspect any `critical` finding with `kubeagent_inspect`. Leave the `warning`
   findings uninspected unless a `critical` one points at them; this is a gate,
   not a full audit.

Report a single **GO** or **NO-GO**, then the reasoning:

- NO-GO if there is any `critical` finding, or if `drift` shows the target
  namespace already diverging from Git.
- GO with caveats if the only findings are `warning`.
- **Always** list the blind spots: every entry in `coverage.partial`, and
  `metricsServer` if it is not `available` — a capacity verdict without
  metrics-server is a guess, and say so.

Never treat a skipped check as a passing one. A gate that says GO because it did
not look is worse than no gate.

Do not remediate, and do not run the deployment.
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestShippedDocsNameOnlyRegisteredTools -v
```

Expected: PASS.

- [ ] **Step 7: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: PASS throughout. No production code changed, so nothing else should move.

- [ ] **Step 8: Commit**

```bash
git add commands/triage.md commands/why.md commands/preflight.md plugin_manifest_test.go
git commit -m "plugin: add the triage, why and preflight slash commands

Three multi-step workflows rather than three passthroughs — a command wrapping
a single tool call would earn nothing over letting the model call the tool.
preflight is the one that requests advisory sections unconditionally, because
a gate that stays silent about what it did not check is worse than no gate."
```

---

### Task 6: Teach `scripts/bump-version.sh` about the manifest

**Files:**
- Modify: `scripts/bump-version.sh`
- Test: `bump_version_plugin_test.go` (create, repo root, package `main`)

**Interfaces:**
- Consumes: `.claude-plugin/plugin.json` from Task 1.
- Produces: nothing other tasks consume.

**Two things to get right.** First, the manifest's version has **no** `v` prefix, while every other version reference in the script does — so it needs its own substitution rather than joining an existing one. Second, the script's "nothing stale remains" grep covers only `--include=*.yaml --include=*.md` and searches for `v$OLD`. Widening it to `*.json` would not catch the manifest anyway (no `v` prefix) and would sweep in generated schema documents under `website/docs/schemas/`. So the manifest gets its own explicit assertion instead.

The test executes a **copy** of the real script against a fixture tree, because the script derives its root from `BASH_SOURCE` and `cd`s there — it cannot be pointed at another directory. The copy is read from disk at test time, so it cannot rot relative to the original.

- [ ] **Step 1: Write the failing test**

Create `bump_version_plugin_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bump-version.sh derives its repo root from BASH_SOURCE and cd's there, so it
// always edits the real tree. To test it we copy the script into a fixture
// tree and run it there. The copy is read from disk at test time, so it cannot
// drift from the script the release actually runs.
func TestBumpVersionMovesPluginManifest(t *testing.T) {
	script, err := os.ReadFile("scripts/bump-version.sh")
	if err != nil {
		t.Fatalf("reading the bump script: %v", err)
	}

	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("scripts/bump-version.sh", string(script))
	if err := os.Chmod(filepath.Join(root, "scripts/bump-version.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	write("CHANGELOG.md", strings.Join([]string{
		"# Changelog", "",
		"## [Unreleased]", "",
		"## [1.2.0] - 2026-08-05", "",
		"[Unreleased]: https://github.com/imantaba/kubeagent/compare/v1.2.0...HEAD",
		"[1.2.0]: https://github.com/imantaba/kubeagent/compare/v1.1.0...v1.2.0", "",
	}, "\n"))
	write("deploy/helm/kubeagent/Chart.yaml", "version: 0.4.0\nappVersion: \"v1.2.0\"\n")
	write("deploy/deployment.yaml", "image: imantaba/kubeagent:v1.2.0\n")
	write("deploy/README.md", "--set image.tag=v1.2.0\n")
	write("website/docs/install.md", "imantaba/kubeagent:v1.2.0 and --set image.tag=v1.2.0\n")
	write(".claude-plugin/plugin.json", "{\n  \"name\": \"kubeagent\",\n  \"version\": \"1.2.0\"\n}\n")

	cmd := exec.Command("bash", filepath.Join(root, "scripts/bump-version.sh"), "v1.3.0")
	cmd.Env = append(os.Environ(), "RELEASE_DATE=2026-08-06")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bump-version.sh failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatalf("reading the bumped manifest: %v", err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the bumped manifest is no longer valid JSON: %v\n%s", err, raw)
	}
	if manifest.Version != "1.3.0" {
		t.Errorf("plugin.json version = %q after bumping to v1.3.0, want %q\n"+
			"script output:\n%s", manifest.Version, "1.3.0", out)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestBumpVersionMovesPluginManifest -v
```

Expected: FAIL with `plugin.json version = "1.2.0" after bumping to v1.3.0, want "1.3.0"`.

- [ ] **Step 3: Add the substitution to the script**

In `scripts/bump-version.sh`, in the `--- deploy + docs` block, after the `website/docs/install.md` line, add:

```bash
# The Claude Code plugin manifest spells the version WITHOUT a leading v, so it
# needs its own substitution rather than joining the v-prefixed swaps above.
sed -i "s#^\(\s*\"version\":\s*\)\"$OLD\"#\1\"$NEW\"#" .claude-plugin/plugin.json
```

- [ ] **Step 4: Add the explicit assertion**

In `scripts/bump-version.sh`, immediately after the existing `STALE` check block (the one ending `exit 1; }`), add:

```bash
# The staleness grep above searches for "v$OLD" in *.yaml and *.md, so it can
# never see the plugin manifest: that file is JSON and its version has no v.
# Assert it directly instead of widening the grep, which would sweep in the
# generated schema documents under website/docs/schemas/.
grep -q "\"version\": \"$NEW\"" .claude-plugin/plugin.json \
  || die ".claude-plugin/plugin.json still does not declare version $NEW"
```

- [ ] **Step 5: Add the manifest to the summary output**

In the `cat <<EOF` summary block, after the `website/docs/install.md` line, add:

```text
  .claude-plugin/plugin.json  version → $NEW (Claude Code plugin manifest)
```

- [ ] **Step 6: Run the test to verify it passes**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestBumpVersionMovesPluginManifest -v
```

Expected: PASS.

- [ ] **Step 7: Confirm the real repo is untouched**

```bash
git diff --stat .claude-plugin/plugin.json
```

Expected: no output. The test runs against a temp fixture; if this shows a diff, the script was run against the real tree by mistake — revert it with `git checkout .claude-plugin/plugin.json`.

- [ ] **Step 8: Commit**

```bash
git add scripts/bump-version.sh bump_version_plugin_test.go
git commit -m "release: bump the plugin manifest's version with everything else

The manifest is a fifth home for the release version and the only one that
spells it without a leading v, so it gets its own substitution and its own
assertion — the script's staleness grep looks for v\$OLD in yaml and md and
could never have seen it.

Tested by running a copy of the script against a fixture tree, since it
derives its root from BASH_SOURCE and always edits the real one."
```

---

### Task 7: Documentation

**Files:**
- Create: `website/docs/features/claude-plugin.md`
- Modify: `website/mkdocs.yml` (nav, after line 78)
- Modify: `README.md` (the `## Install` section, before `### As a \`kubectl\` plugin (krew)`)
- Modify: `website/docs/compatibility.md` (a new stable-surface subsection)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: `CLAUDE.md` (roadmap section)
- Modify: `website/docs/roadmap.md` (Theme G)

**Interfaces:**
- Consumes: everything from Tasks 1–6.
- Produces: nothing.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/claude-plugin.md`:

````markdown
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
| `/kubeagent:triage [namespace]` | Sweeps, auto-inspects every critical and high finding, reports with its coverage caveats stated |
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
````

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, after the `- In-cluster dashboard: features/dashboard.md` line (line 78), add:

```yaml
      - Claude Code plugin: features/claude-plugin.md
```

- [ ] **Step 3: Add the README section**

In `README.md`, immediately after the `## Install` heading and before `### As a \`kubectl\` plugin (krew)`, insert:

````markdown
### As a Claude Code plugin

Install the binary first by any route below, then, in Claude Code:

```text
/plugin marketplace add imantaba/kubeagent
/plugin install kubeagent@kubeagent
```

You get the `kubeagent mcp` server wired up, two skills that teach the model how
to read a coverage block, and `/kubeagent:triage`, `/kubeagent:why` and
`/kubeagent:preflight`. It is read-only: nothing in the plugin reaches `--fix`.
See [Claude Code plugin](website/docs/features/claude-plugin.md).
````

- [ ] **Step 4: Add the compatibility subsection**

In `website/docs/compatibility.md`, after the `### The watch daemon's \`/dashboard\` endpoint` subsection and before `## Unstable surfaces — do not build on these`, add:

```markdown
### The Claude Code plugin manifest

`.claude-plugin/plugin.json` declares the command line that starts the MCP
server. Because that command line is baked into every installation and re-read
on upgrade, changing it changes the behaviour of plugins already installed.
Within 1.x, adding a flag to `mcpServers.kubeagent.args` is MINOR; removing one,
or changing what an existing flag means, is MAJOR. The plugin's `name` and the
marketplace entry's `name` are stable — they are what `/plugin install
kubeagent@kubeagent` names.

**The skill and command text is not stable.** It is instruction for a model and
will be reworded as the detectors change. Nothing should parse it.
```

- [ ] **Step 5: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]`, add:

```markdown
### Added

- **Claude Code plugin.** kubeagent installs into Claude Code with
  `/plugin marketplace add imantaba/kubeagent`, wiring the existing
  `kubeagent mcp` server together with two skills and three commands
  (`/kubeagent:triage`, `/kubeagent:why`, `/kubeagent:preflight`). The skills
  are the point rather than the manifest: a model handed the four MCP tools
  with no instruction reads an absent `coverage` key as good news, which throws
  away both the coverage block and the least-privilege RBAC blind-spot
  reporting. Read-only throughout — no tool, skill, or command reaches `--fix`
  — and no new Go production code, so no import-graph invariant moves and none
  of the six versioned JSON documents change. Two tests pin the manifests
  against the chart's `appVersion`, against the real `mcp` flag parser, and
  against the tool names `internal/mcp` actually registers. See
  [Claude Code plugin](website/docs/features/claude-plugin.md).
```

- [ ] **Step 6: Update CLAUDE.md**

In `CLAUDE.md`, in the bullet beginning `**Theme G slices 1, 2, 3, 4a and 4b have shipped:**`, append to the end of that bullet:

```markdown
  A third distribution surface now sits alongside the MCP server and the krew
  plugin: kubeagent installs into Claude Code as a plugin
  (`.claude-plugin/plugin.json` + `marketplace.json`, with user-facing skills
  under `skills/` and commands under `commands/`), documented in
  [website/docs/features/claude-plugin.md](website/docs/features/claude-plugin.md).
  It ships no Go production code and is **read-only**: no tool, skill, or
  command reaches `--fix`. Note the two skills directories — `.claude/skills/`
  is dev-facing (it holds `release` and `update-demo-gif`); root-level
  `skills/` is what the plugin ships to users. Claude Code
  auto-discovers only the former. `plugin_manifest_test.go` and
  `internal/cli/plugin_flags_test.go` fail the build when the manifests or the
  shipped skill text drift from the flags and tool names kubeagent registers.
```

- [ ] **Step 7: Update the roadmap**

In `website/docs/roadmap.md`, find the Theme G section and note the third distribution surface alongside the MCP server and krew plugin, matching the surrounding prose style. Read the section first; do not paste the CLAUDE.md wording verbatim, as the roadmap is written for users rather than contributors.

- [ ] **Step 8: Verify the docs build and the suite is green**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: PASS.

If a local MkDocs environment exists (`website/.venv/`), also run:

```bash
cd website && .venv/bin/mkdocs build --strict
```

Expected: builds with no warnings. A missing nav target or a broken relative link fails `--strict`. If no venv exists, skip this and note it in the task report.

- [ ] **Step 9: Commit**

```bash
git add website/docs/features/claude-plugin.md website/mkdocs.yml README.md \
        website/docs/compatibility.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -m "docs: document the Claude Code plugin

A feature page, a README install section beside the krew one, a nav entry, a
CHANGELOG entry, and the roadmap's third distribution surface.

Compatibility gains a row the design argued for: the manifest's args are baked
into every installation and re-read on upgrade, so removing a flag or changing
its meaning is a MAJOR change. The skill text is explicitly not stable — it is
instruction for a model, not a format to parse."
```

---

### Task 8: Manual acceptance

**Files:** none created or modified.

**Interfaces:**
- Consumes: everything from Tasks 1–7.
- Produces: a written result recorded in the task ledger.

Automated tests can prove the manifests are honest. They cannot prove the skills make Claude behave better, and they cannot prove the manifest schemas are the ones Claude Code actually expects — that is only knowable by installing it.

- [ ] **Step 1: Install the plugin from the local checkout**

In Claude Code, from this repository:

```text
/plugin marketplace add ./
/plugin install kubeagent@kubeagent
```

Expected: both succeed.

**If either is rejected for a schema reason** — an unrecognised or missing field in `plugin.json` or `marketplace.json` — fix the manifest to match the error, update the assertions in `plugin_manifest_test.go` to match the corrected shape, re-run `go test .`, and commit that fix before continuing. This step is the only place the real schema gets validated, so a correction here is an expected outcome, not a failure of the plan.

- [ ] **Step 2: Confirm the server connected**

```text
/mcp
```

Expected: `kubeagent` listed as connected, exposing `kubeagent_triage`, `kubeagent_inspect`, `kubeagent_advisory`, and `list_contexts` — the fourth appears only because `--allow-context-switch` is passed, so its presence confirms the manifest's args reached the process.

- [ ] **Step 3: Exercise all three commands against a kind cluster**

Bring up a cluster from `chaos/` with at least one broken workload, then run each of:

```text
/kubeagent:triage
/kubeagent:why pod/<a-failing-pod> -n <ns>
/kubeagent:preflight
```

Expected: each completes, names its coverage caveats, and — critically — never runs a mutating command.

- [ ] **Step 4: The restricted-RBAC case**

Re-run `/kubeagent:triage` using a kubeconfig context bound to an identity that cannot list one of the resource kinds kubeagent reads (`kubeagent rbac print` names the grants; drop one).

Expected: the refused kind appears in `coverage.partial`, and the report **names it as a blind spot** rather than reporting that section clean. This is the specific behaviour the `reading-kubeagent-findings` skill exists to produce, and the one worth verifying by hand.

- [ ] **Step 5: Record the result**

Write the outcome of each step into the task ledger, including any manifest corrections made in Step 1. If Step 4 does not produce a blind-spot report, that is a skill-text defect: sharpen `reading-kubeagent-findings`, commit, and re-run.

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: manifests and layout → Task 1; the flag pin → Task 2; the two skills → Tasks 3 and 4; the three commands → Task 5; the tool-name drift test → Task 3 (extended in 4 and 5); release wiring → Task 6; all seven documentation targets → Task 7; manual acceptance including the restricted-RBAC scenario → Task 8. The spec's "Invariants preserved" section needs no task — it is a set of things this plan must not do, and it is restated in Global Constraints.

**Placeholder scan.** One step is deliberately not literal: Task 7 Step 7 (the roadmap) says to read the section and match its prose style rather than pasting fixed text, because the roadmap is user-facing narrative and a verbatim paste of the CLAUDE.md wording would read wrong there. Every other step contains the exact content to write.

**Type consistency.** `readPluginManifest` (Task 1) is defined once and reused. `pluginDocFiles` (Task 3) is extended by name in Tasks 4 and 5. `registeredMCPTools`, `toolNameRE`, `registrationRE`, and `sortedKeys` are defined once in Task 3. `mapKeys` is defined in Task 1 and not redefined. The `pluginManifest` struct in `plugin_manifest_test.go` and the anonymous struct in `internal/cli/plugin_flags_test.go` are separate by necessity — different packages — and both are shown in full.

**One known unknown, handled rather than hidden.** The exact required fields of `marketplace.json` are not verifiable from this repository. Task 1 writes the conventional shape; Task 8 Step 1 is where it meets the real loader, and that step explicitly instructs the implementer to correct both manifest and test if the loader disagrees.
