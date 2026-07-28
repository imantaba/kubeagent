# `kubectl` krew plugin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Package the existing kubeagent binary for [krew](https://krew.sigs.k8s.io) so `kubectl kubeagent scan` works anywhere `kubectl` does, without changing what the binary does.

**Architecture:** Four independent pieces. (1) A pure `invocationName(argv0)` in `main.go` so usage and error text name the command the user actually typed. (2) `scripts/build-release-archives.sh` builds four platform archives + `SHA256SUMS`. (3) `scripts/render-krew-manifest.sh` renders `krew/kubeagent.yaml.tmpl` into the krew plugin manifest using those checksums, looked up by archive filename. (4) `release.yml` calls both scripts and attaches `kubeagent.yaml` to the GitHub Release. The manifest is generated at release time and never committed.

**Tech Stack:** Go 1.26 (stdlib `flag`, no Cobra), bash + coreutils (`sed`, `awk`, `sha256sum`, `tar`), GitHub Actions, `sigs.k8s.io/yaml` (test-only YAML parsing).

Spec: [docs/superpowers/specs/2026-07-28-krew-plugin-design.md](../specs/2026-07-28-krew-plugin-design.md) (commit `6946e25`).
Branch: `krew-plugin`, cut off `main` at `16e0eef` (v0.63.0).

## Global Constraints

- **Read-only invariant holds.** This slice changes no cluster-interaction code. Nothing in it may add a `Create`/`Update`/`Patch`/`Delete` call.
- **Standard-library `flag` only — no Cobra.** krew requires nothing from the CLI framework.
- **No `Co-Authored-By: Claude` trailer** (or any Claude / Claude Code / Anthropic attribution) on any commit, in any file, comment, doc, or changelog entry.
- **URLs and kubeconfig paths are credentials.** No example, comment, or manifest field may carry more than `scheme://host` for a private endpoint, and no example may name a real cluster, host, or path. Public GitHub / Docker Hub URLs for this project's own releases are fine.
- **RFC 5737 addresses only** in any documentation or fixture that needs an IP: `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** No report output changes in this slice.
- **`kubeagent version` output is unchanged** — still `kubeagent vX.Y.Z`. Version names the software; usage names the command you type.
- Plugin name is exactly `kubeagent`, **never** `kubectl-kubeagent`: krew's `pluginNameToBin` prefixes `kubectl-` itself, so the prefixed name would install as `kubectl-kubectl-kubeagent`. (krew's validator would *accept* the prefixed name — `safePluginRegexp` is `^[\w-]+$` — so nothing upstream catches this.)
- Manifest `apiVersion` is exactly `krew.googlecontainertools.github.com/v1alpha2`; `kind` is exactly `Plugin`.
- Four platforms, no more: `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`. **Windows is deliberately excluded** — no test or smoke run in this project has ever executed on it, and listing it in the manifest would be a claim the project cannot back.
- Every archive is `CGO_ENABLED=0` and stamped with `-ldflags "-X main.version=${VERSION}"`.
- The unversioned `kubeagent_linux_amd64.tar.gz` asset **stays** — `releases/latest/download/kubeagent_linux_amd64.tar.gz` is in the wild in the README quick-install and in people's notes.
- **Tests execute the real shell scripts** (`os/exec`). A Go reimplementation of a script's substitutions is a plan failure: it would keep passing while the script rotted.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin` before any `go` command.

## File Structure

| File | Responsibility |
|------|----------------|
| `main.go` | Gains `invocationName(argv0 string) string` and the package-level `invokedAs`; usage error and `main`'s stderr prefix read `invokedAs`. |
| `main_test.go` | Table test for `invocationName`; wiring tests for the usage string under both spellings. |
| `scripts/build-release-archives.sh` | **New.** `VERSION OUTDIR` → four archives + unversioned copy + `SHA256SUMS`. |
| `krew/kubeagent.yaml.tmpl` | **New.** The krew manifest with `{{VERSION}}` / `{{SHA256_<PLATFORM>}}` placeholders. |
| `scripts/render-krew-manifest.sh` | **New.** `VERSION SHA256SUMS_FILE` → rendered manifest on stdout. |
| `krew_manifest_test.go` | **New.** Drives `render-krew-manifest.sh` via `os/exec` and asserts on parsed YAML. Package `main`, repo root, so the script's relative path resolves and `go test ./...` picks it up. |
| `.github/workflows/release.yml` | Calls both scripts; attaches all six artifacts plus `kubeagent.yaml`. |
| `.gitignore` | Ignores `/dist/` so a local build or smoke run does not dirty the tree (the release skill requires a clean tree). |
| `README.md`, `website/docs/install.md`, `website/docs/quickstart.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md` | Doc surfaces. |

`deploy/README.md` is deliberately untouched: it covers the in-cluster watch daemon, which krew has nothing to do with.

---

### Task 1: `invocationName` — telling the user the truth about their own command

krew installs the binary as `~/.krew/bin/kubectl-kubeagent` and `kubectl` execs it under that name, so `os.Args[0]`'s basename is `kubectl-kubeagent` (Go does not resolve the symlink). Today the usage error tells that user to run `kubeagent`, a command that is not on their `PATH`.

**Files:**
- Modify: `main.go` (imports; new function + var after `versionLine`; `main` at :53-58; the usage error at :72)
- Test: `main_test.go` (append at end of file)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `func invocationName(argv0 string) string` and `var invokedAs string` (package `main`). No later task depends on them.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
// invocationName is a pure function of argv[0] so the kubectl-plugin spelling
// can be tested without launching a process under that name.
func TestInvocationName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{"/home/u/.krew/bin/kubectl-kubeagent", "kubectl kubeagent"},
		{"kubectl-kubeagent", "kubectl kubeagent"},
		{"./kubeagent", "kubeagent"},
		{"/usr/local/bin/kubeagent", "kubeagent"},
		// kubectl-kubeagent as a DIRECTORY component must not match. A naive
		// strings.Contains passes every other row and fails this one.
		{"/opt/kubectl-kubeagent/kubeagent", "kubeagent"},
		{"", "kubeagent"},
		{"kubectl-kubeagent-extra", "kubeagent"},
	}
	for _, tt := range tests {
		if got := invocationName(tt.argv0); got != tt.want {
			t.Errorf("invocationName(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

func TestRun_UsageNamesThePlainBinaryByDefault(t *testing.T) {
	// The test binary's argv[0] basename is never "kubectl-kubeagent", so the
	// default spelling under `go test` is the plain one.
	if invokedAs != "kubeagent" {
		t.Fatalf("invokedAs = %q under go test, want %q", invokedAs, "kubeagent")
	}
	err := run(nil)
	if err == nil {
		t.Fatal("run(nil) = nil, want the usage error")
	}
	if !strings.Contains(err.Error(), "usage: kubeagent scan") {
		t.Errorf("usage = %q, want it to start by naming `kubeagent scan`", err)
	}
}

func TestRun_UsageNamesTheKubectlPluginInvocation(t *testing.T) {
	saved := invokedAs
	invokedAs = "kubectl kubeagent"
	defer func() { invokedAs = saved }()

	err := run(nil)
	if err == nil {
		t.Fatal("run(nil) = nil, want the usage error")
	}
	got := err.Error()
	// Every subcommand the usage lists must be named the way the user would
	// type it, not just the first one.
	for _, want := range []string{
		"usage: kubectl kubeagent scan",
		"| kubectl kubeagent watch",
		"| kubectl kubeagent mcp",
		"| kubectl kubeagent version",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("usage = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "usage: kubeagent scan") {
		t.Errorf("usage = %q, still tells a kubectl-plugin user to run the bare binary", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestInvocationName|TestRun_UsageNames' 2>&1 | head -20
```

Expected: FAIL — `undefined: invocationName` and `undefined: invokedAs` (a compile error, so the whole package test fails).

- [ ] **Step 3: Add `invocationName` and `invokedAs`**

In `main.go`, add `"path/filepath"` to the import block (alphabetically, after `"os/signal"`):

```go
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
```

Then insert after `versionLine()` (currently ends at `main.go:51`):

```go
// invocationName returns how the user invoked this process, for use in usage
// and error text. krew installs the binary as ~/.krew/bin/kubectl-kubeagent
// and kubectl execs it under that name, so argv[0]'s basename tells us which
// command the user actually typed. Anything else — a plain ./kubeagent, a
// kubectl-kubeagent directory in the path, a kubectl-kubeagent-extra sibling
// plugin — is the ordinary binary.
func invocationName(argv0 string) string {
	if filepath.Base(argv0) == "kubectl-kubeagent" {
		return "kubectl kubeagent"
	}
	return "kubeagent"
}

// invokedAs is the command name used in usage and error text, resolved once at
// startup. Tests override it to exercise the kubectl-plugin spelling.
var invokedAs = invocationName(os.Args[0])
```

- [ ] **Step 4: Wire it into the two call sites**

Replace `main()` (`main.go:53-58`):

```go
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", invokedAs, err)
		os.Exit(1)
	}
}
```

Replace the usage error (`main.go:72`) — the `%[1]s` indexed verb repeats the single argument, so the four command positions all get the same spelling. Every other character of the string is unchanged:

```go
		return fmt.Errorf("usage: %[1]s scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--investigate] [--model name] [--include-cron] [--include-restarts] [--pvc-reclaim] [--lint-secrets] [--security] [--security-verbose] [--disk-usage [--disk-threshold r]] [--kubelet-health] [--control-plane-health] [--dns-health] [--certs [--cert-warn-days n]] [--operators] [--drift] [--drift-age dur] [--capacity] [--logs] [--node-heartbeat-threshold dur] [--expected-nodes a,b,…] [--fix [--dry-run|--yes] [--audit-log path]] [--rollback --audit-log path] | %[1]s watch [--kubeconfig path] [--context name (repeatable)] [--cluster-name name] [--include-local] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]] | %[1]s mcp [--kubeconfig path] [--context name] [--allow-context-switch] [--logs] | %[1]s version", invokedAs)
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestInvocationName|TestRun_UsageNames' -v 2>&1 | tail -20
```

Expected: PASS for all three.

- [ ] **Step 6: Verify nothing else regressed**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head
go test ./internal/report -run TestGoldenScanOutput -v 2>&1 | tail -5
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: no failures; the golden test passes; `git diff --stat` on the golden fixture prints **nothing** (byte-identical). The pre-existing usage tests (`TestUsage_MentionsTheMCPSubcommand`, `TestRun_UsageMentionsCapacityFlag`, `TestRun_UsageMentionsDriftFlag`, `TestUsageMentionsTheExplainFlags`, …) assert on substrings like `"kubeagent mcp"`, which the default spelling still produces — they must all still pass unchanged.

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): name the command the user actually typed in usage and errors

krew installs the binary as ~/.krew/bin/kubectl-kubeagent and kubectl execs
it under that name, so a plugin user reading the usage error was told to run
kubeagent — a command not on their PATH. invocationName reads argv[0]'s
basename and the usage string and the stderr prefix follow it.

kubeagent version is unchanged: version names the software, usage names the
command you type."
```

---

### Task 2: `scripts/build-release-archives.sh` — one platform becomes four

The release workflow builds `linux/amd64` inline in YAML today, which can only be exercised by pushing a tag — the one place where being wrong is most expensive. Moving it into a script lets the smoke gate run the same code before the tag exists.

**Files:**
- Create: `scripts/build-release-archives.sh`
- Modify: `.gitignore`

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `scripts/build-release-archives.sh VERSION OUTDIR`. In `OUTDIR` it writes `kubeagent_${VERSION}_{linux,darwin}_{amd64,arm64}.tar.gz` (4), `kubeagent_linux_amd64.tar.gz` (unversioned copy), and `SHA256SUMS` (5 lines, **bare filenames**, `sha256sum` format). Task 3's renderer looks checksums up by those exact filenames; Task 4's workflow uploads those exact paths.

**No unit test, deliberately.** The script is four cross-compiles and a `tar`; a test asserting on its output would be asserting that `go build` and `tar` work. Its gate is the real krew smoke install (Task 6 verification below, run by the controller). Step 3 below verifies it by executing it and inspecting the artifacts — that is the test cycle for this task.

- [ ] **Step 1: Write the script**

Create `scripts/build-release-archives.sh`:

```bash
#!/usr/bin/env bash
# build-release-archives.sh — build the release archive for every published
# platform, plus SHA256SUMS.
#
# Usage:  scripts/build-release-archives.sh VERSION OUTDIR
#
# Produces, in OUTDIR:
#   kubeagent_${VERSION}_linux_amd64.tar.gz
#   kubeagent_${VERSION}_linux_arm64.tar.gz
#   kubeagent_${VERSION}_darwin_amd64.tar.gz
#   kubeagent_${VERSION}_darwin_arm64.tar.gz
#   kubeagent_linux_amd64.tar.gz   unversioned copy, so
#                                  releases/latest/download/... keeps resolving
#   SHA256SUMS                     bare filenames, one line per archive
#
# The release workflow calls this, and so does the local krew smoke gate, so
# both exercise the same build. Windows is deliberately not built: no test or
# smoke run in this project has ever executed on it, and shipping a binary for
# a platform nobody has run is a claim the project cannot back.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION="${1:-}"
OUTDIR="${2:-}"
[ -n "$VERSION" ] || die "usage: scripts/build-release-archives.sh VERSION OUTDIR"
[ -n "$OUTDIR" ] || die "usage: scripts/build-release-archives.sh VERSION OUTDIR"

# Run from the repo root regardless of where we're invoked.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

mkdir -p "$OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd)"   # absolute: `tar -C` and the subshell below must agree

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  echo "building ${os}/${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -ldflags "-X main.version=${VERSION}" -o "$stage/kubeagent" .
  cp README.md LICENSE "$stage/"
  tar -czf "${OUTDIR}/kubeagent_${VERSION}_${os}_${arch}.tar.gz" \
    -C "$stage" kubeagent README.md LICENSE
done

# Unversioned copy so releases/latest/download/kubeagent_linux_amd64.tar.gz
# always resolves to the newest release. That URL is in the wild — the README
# quick-install and people's own notes — and dropping it would break every
# copy of that install line silently.
cp "${OUTDIR}/kubeagent_${VERSION}_linux_amd64.tar.gz" \
   "${OUTDIR}/kubeagent_linux_amd64.tar.gz"

# Bare filenames: the krew manifest renderer looks checksums up by archive
# name, and `sha256sum -c SHA256SUMS` must work from the download directory.
( cd "$OUTDIR" && sha256sum kubeagent_*.tar.gz > SHA256SUMS )
cat "${OUTDIR}/SHA256SUMS"
```

Then make it executable (git tracks the bit; the workflow depends on it):

```bash
chmod +x scripts/build-release-archives.sh
```

- [ ] **Step 2: Ignore the build output directory**

In `.gitignore`, under the existing `# Release artifacts (produced by the release workflow / local dry-runs)` block, add `/dist/` so the entry reads:

```gitignore
# Release artifacts (produced by the release workflow / local dry-runs)
*.tar.gz
SHA256SUMS
/dist/
```

- [ ] **Step 3: Run the script and verify every artifact**

```bash
export PATH=$PATH:/usr/local/go/bin
rm -rf /tmp/krew-build-check && mkdir -p /tmp/krew-build-check
scripts/build-release-archives.sh v0.0.0-buildcheck /tmp/krew-build-check/dist
ls -1 /tmp/krew-build-check/dist
```

Expected: exactly six entries —

```
SHA256SUMS
kubeagent_linux_amd64.tar.gz
kubeagent_v0.0.0-buildcheck_darwin_amd64.tar.gz
kubeagent_v0.0.0-buildcheck_darwin_arm64.tar.gz
kubeagent_v0.0.0-buildcheck_linux_amd64.tar.gz
kubeagent_v0.0.0-buildcheck_linux_arm64.tar.gz
```

Then check the contents, the checksum file, and that the cross-compiles really produced foreign binaries:

```bash
cd /tmp/krew-build-check
tar tzf dist/kubeagent_v0.0.0-buildcheck_darwin_arm64.tar.gz
wc -l < dist/SHA256SUMS
mkdir -p x-linux x-darwin
tar xzf dist/kubeagent_v0.0.0-buildcheck_linux_amd64.tar.gz  -C x-linux  kubeagent
tar xzf dist/kubeagent_v0.0.0-buildcheck_darwin_arm64.tar.gz -C x-darwin kubeagent
od -An -tx1 -N4 x-linux/kubeagent    # ELF magic
od -An -tx1 -N4 x-darwin/kubeagent   # Mach-O 64-bit magic
./x-linux/kubeagent version
( cd dist && sha256sum -c SHA256SUMS )
```

Expected:
- `tar tzf` lists exactly `kubeagent`, `README.md`, `LICENSE`.
- `wc -l` prints `5`.
- linux magic: ` 7f 45 4c 46` (ELF). darwin magic: ` cf fa ed fe` (Mach-O 64-bit LE) — a *different* value, proving `GOOS=darwin` was honoured rather than four copies of the host build.
- `kubeagent version` prints `kubeagent v0.0.0-buildcheck` — the ldflags stamp landed.
- `sha256sum -c` prints `: OK` for all five archives.

Clean up: `rm -rf /tmp/krew-build-check`.

- [ ] **Step 4: Confirm the tree is still clean**

```bash
cd "$(git rev-parse --show-toplevel)" && git status --short
```

Expected: only the two intended files (`?? scripts/build-release-archives.sh`, ` M .gitignore`). No stray `dist/`, `*.tar.gz`, or `SHA256SUMS` in the repo root.

- [ ] **Step 5: Commit**

```bash
git add scripts/build-release-archives.sh .gitignore
git commit -m "build: script the release archives for four platforms

linux and darwin x amd64 and arm64, each with the binary, README and LICENSE,
plus the unversioned linux/amd64 copy and SHA256SUMS. Windows is deliberately
excluded: nothing in this project has ever been run on it.

Lives in a script rather than inline in the workflow YAML so the krew smoke
gate can build with exactly the code CI runs, before a tag exists."
```

---

### Task 3: the krew manifest — template, renderer, and a test that runs the real script

**Files:**
- Create: `krew/kubeagent.yaml.tmpl`
- Create: `scripts/render-krew-manifest.sh`
- Create: `krew_manifest_test.go` (package `main`, repo root — `go test` runs with the package directory as its working directory, so `scripts/render-krew-manifest.sh` resolves)
- Modify: `go.mod`, `go.sum` (via `go mod tidy` — `sigs.k8s.io/yaml v1.6.0` moves from indirect to direct)

**Interfaces:**
- Consumes: the `SHA256SUMS` format produced by Task 2 — `sha256sum` output with **bare** archive filenames.
- Produces: `scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE` → the rendered manifest on **stdout**, nonzero exit + a message on stderr if any checksum is missing or any placeholder survives. Task 4 redirects its stdout to `dist/kubeagent.yaml`.

- [ ] **Step 1: Write the template**

Create `krew/kubeagent.yaml.tmpl`:

```yaml
# krew plugin manifest template. Rendered at release time by
# scripts/render-krew-manifest.sh; the rendered manifest is attached to the
# GitHub Release and is never committed — a checksum written before the tag is
# a guess about bytes that do not exist yet.
apiVersion: krew.googlecontainertools.github.com/v1alpha2
kind: Plugin
metadata:
  # No kubectl- prefix: krew's own pluginNameToBin adds it when it creates the
  # ~/.krew/bin symlink. A plugin named kubectl-kubeagent would install as
  # kubectl-kubectl-kubeagent.
  name: kubeagent
spec:
  version: {{VERSION}}
  homepage: https://github.com/imantaba/kubeagent
  shortDescription: Diagnose why Kubernetes workloads are broken, read-only.
  description: |
    kubeagent scans a cluster and names the underlying cause of each failing
    workload — the container exit reason, the scheduler's message, the failed
    image pull — rather than only reporting that something is unhealthy. The
    report is prioritized: cluster health first, then workload failures.

    A scan is read-only: get and list calls, nothing else. It works offline.
    Opt-in flags add a plain-English --explain summary, --output json, and
    guard-railed --fix remediation with per-action confirmation.

      kubectl kubeagent scan
      kubectl kubeagent scan -n payments --output json
  caveats: |
    Flags belong AFTER the plugin name. kubectl does not forward its own
    global flags to plugins:

      kubectl kubeagent scan --context prod-eu     # works
      kubectl --context prod-eu kubeagent scan     # does not

    kubeagent's --context and --kubeconfig are spelled exactly like kubectl's,
    and KUBECONFIG is read from the environment the same way, so the habit
    transfers intact.

    `kubectl kubeagent scan` is read-only: it issues get and list calls only.
    Writes happen solely under the opt-in --fix flag, which is limited to a
    fixed allowlist of reversible actions, refuses protected namespaces, and
    confirms every action before applying it.
  platforms:
    - selector:
        matchLabels:
          os: linux
          arch: amd64
      uri: https://github.com/imantaba/kubeagent/releases/download/{{VERSION}}/kubeagent_{{VERSION}}_linux_amd64.tar.gz
      sha256: {{SHA256_LINUX_AMD64}}
      bin: kubeagent
      files:
        - from: kubeagent
          to: .
        - from: LICENSE
          to: .
    - selector:
        matchLabels:
          os: linux
          arch: arm64
      uri: https://github.com/imantaba/kubeagent/releases/download/{{VERSION}}/kubeagent_{{VERSION}}_linux_arm64.tar.gz
      sha256: {{SHA256_LINUX_ARM64}}
      bin: kubeagent
      files:
        - from: kubeagent
          to: .
        - from: LICENSE
          to: .
    - selector:
        matchLabels:
          os: darwin
          arch: amd64
      uri: https://github.com/imantaba/kubeagent/releases/download/{{VERSION}}/kubeagent_{{VERSION}}_darwin_amd64.tar.gz
      sha256: {{SHA256_DARWIN_AMD64}}
      bin: kubeagent
      files:
        - from: kubeagent
          to: .
        - from: LICENSE
          to: .
    - selector:
        matchLabels:
          os: darwin
          arch: arm64
      uri: https://github.com/imantaba/kubeagent/releases/download/{{VERSION}}/kubeagent_{{VERSION}}_darwin_arm64.tar.gz
      sha256: {{SHA256_DARWIN_ARM64}}
      bin: kubeagent
      files:
        - from: kubeagent
          to: .
        - from: LICENSE
          to: .
```

- [ ] **Step 2: Write the failing test**

Create `krew_manifest_test.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// These tests execute scripts/render-krew-manifest.sh itself. The script is
// what the release workflow runs, so the script is what must be tested: a Go
// reimplementation of its substitutions would keep passing while the real
// script rotted.

const krewTestVersion = "v9.9.9"

// Four DISTINCT fixture checksums. A renderer that pasted one checksum into
// all four platform slots would satisfy "every sha256 is 64 hex characters"
// and still ship a manifest that fails for three platforms out of four.
var krewTestSums = map[string]string{
	"linux_amd64":  strings.Repeat("a1", 32),
	"linux_arm64":  strings.Repeat("b2", 32),
	"darwin_amd64": strings.Repeat("c3", 32),
	"darwin_arm64": strings.Repeat("d4", 32),
}

var krewPlatformOrder = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

type krewManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version          string `json:"version"`
		Homepage         string `json:"homepage"`
		ShortDescription string `json:"shortDescription"`
		Description      string `json:"description"`
		Caveats          string `json:"caveats"`
		Platforms        []struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			URI    string `json:"uri"`
			Sha256 string `json:"sha256"`
			Bin    string `json:"bin"`
			Files  []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"files"`
		} `json:"platforms"`
	} `json:"spec"`
}

// krewFixtureSums renders a SHA256SUMS body in `sha256sum` format for the
// given platforms, in the given order.
func krewFixtureSums(platforms []string) string {
	var b strings.Builder
	for _, p := range platforms {
		fmt.Fprintf(&b, "%s  kubeagent_%s_%s.tar.gz\n", krewTestSums[p], krewTestVersion, p)
	}
	return b.String()
}

// renderKrewManifest runs the real script with a fixture checksum file and
// returns its stdout. It fails the test if the script exits nonzero.
func renderKrewManifest(t *testing.T, sums string) string {
	t.Helper()
	out, stderr, err := runKrewRenderer(t, sums)
	if err != nil {
		t.Fatalf("render-krew-manifest.sh: %v\nstderr: %s", err, stderr)
	}
	return out
}

func runKrewRenderer(t *testing.T, sums string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	sumsFile := filepath.Join(dir, "SHA256SUMS")
	if writeErr := os.WriteFile(sumsFile, []byte(sums), 0o644); writeErr != nil {
		t.Fatalf("write fixture SHA256SUMS: %v", writeErr)
	}
	cmd := exec.Command("scripts/render-krew-manifest.sh", krewTestVersion, sumsFile)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func parseKrewManifest(t *testing.T, rendered string) krewManifest {
	t.Helper()
	var m krewManifest
	if err := yaml.Unmarshal([]byte(rendered), &m); err != nil {
		t.Fatalf("rendered manifest does not parse as YAML: %v\n%s", err, rendered)
	}
	return m
}

func TestRenderKrewManifest_Identity(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	if want := "krew.googlecontainertools.github.com/v1alpha2"; m.APIVersion != want {
		t.Errorf("apiVersion = %q, want %q", m.APIVersion, want)
	}
	if m.Kind != "Plugin" {
		t.Errorf("kind = %q, want %q", m.Kind, "Plugin")
	}
	// krew's pluginNameToBin prefixes the plugin name with "kubectl-" when it
	// creates the symlink, so a kubectl-prefixed name here would install as
	// kubectl-kubectl-kubeagent. krew's validator accepts that name, so
	// nothing upstream catches the mistake.
	if m.Metadata.Name != "kubeagent" {
		t.Errorf("metadata.name = %q, want %q", m.Metadata.Name, "kubeagent")
	}
	if m.Spec.Version != krewTestVersion {
		t.Errorf("spec.version = %q, want %q", m.Spec.Version, krewTestVersion)
	}
	// krew rejects a short description containing a line break.
	if strings.ContainsAny(m.Spec.ShortDescription, "\n\r") {
		t.Errorf("shortDescription = %q, want no line breaks", m.Spec.ShortDescription)
	}
	if m.Spec.ShortDescription == "" {
		t.Error("shortDescription is empty; krew requires one")
	}
}

func TestRenderKrewManifest_EveryPlatformGetsItsOwnURIAndChecksum(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	if len(m.Spec.Platforms) != 4 {
		t.Fatalf("len(spec.platforms) = %d, want 4", len(m.Spec.Platforms))
	}

	seen := map[string]bool{}
	for i, p := range m.Spec.Platforms {
		key := p.Selector.MatchLabels["os"] + "_" + p.Selector.MatchLabels["arch"]
		if _, ok := krewTestSums[key]; !ok {
			t.Errorf("platform %d: unexpected selector %v", i, p.Selector.MatchLabels)
			continue
		}
		if seen[key] {
			t.Errorf("platform %s appears more than once", key)
		}
		seen[key] = true

		wantURI := "https://github.com/imantaba/kubeagent/releases/download/" +
			krewTestVersion + "/kubeagent_" + krewTestVersion + "_" + key + ".tar.gz"
		if p.URI != wantURI {
			t.Errorf("platform %s: uri = %q, want %q", key, p.URI, wantURI)
		}
		if p.Sha256 != krewTestSums[key] {
			t.Errorf("platform %s: sha256 = %q, want %q", key, p.Sha256, krewTestSums[key])
		}
		if p.Bin != "kubeagent" {
			t.Errorf("platform %s: bin = %q, want %q", key, p.Bin, "kubeagent")
		}
		if len(p.Files) == 0 {
			t.Errorf("platform %s: files is empty; krew requires it unspecified or non-empty", key)
		}
	}
	if len(seen) != 4 {
		t.Errorf("covered platforms = %v, want all four of %v", seen, krewPlatformOrder)
	}
}

// The renderer must look each checksum up by archive filename. Rendering the
// same four checksums listed in a different order must produce byte-identical
// output; a renderer that read SHA256SUMS positionally would swap them.
func TestRenderKrewManifest_LooksChecksumsUpByNameNotLineOrder(t *testing.T) {
	ordered := renderKrewManifest(t, krewFixtureSums(krewPlatformOrder))
	shuffled := renderKrewManifest(t, krewFixtureSums(
		[]string{"darwin_arm64", "linux_amd64", "darwin_amd64", "linux_arm64"}))

	if ordered != shuffled {
		t.Errorf("reordering SHA256SUMS changed the manifest; checksums are being read by line position, not by filename\n--- ordered ---\n%s\n--- shuffled ---\n%s", ordered, shuffled)
	}
}

func TestRenderKrewManifest_LeavesNoPlaceholder(t *testing.T) {
	rendered := renderKrewManifest(t, krewFixtureSums(krewPlatformOrder))
	if strings.Contains(rendered, "{{") {
		t.Errorf("rendered manifest still contains an unsubstituted placeholder:\n%s", rendered)
	}
}

// A missing checksum must fail loudly. A manifest rendered with an empty
// sha256 fails at install time with an opaque verification error, which is
// exactly what krew's checksums exist to catch.
func TestRenderKrewManifest_FailsWhenAChecksumIsMissing(t *testing.T) {
	stdout, stderr, err := runKrewRenderer(t, krewFixtureSums(
		[]string{"linux_amd64", "linux_arm64", "darwin_amd64"})) // darwin_arm64 absent

	if err == nil {
		t.Fatalf("renderer succeeded with a missing checksum; stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "kubeagent_"+krewTestVersion+"_darwin_arm64.tar.gz") {
		t.Errorf("stderr = %q, want it to name the archive whose checksum is missing", stderr)
	}
}

func TestRenderKrewManifest_CaveatsStateFlagOrderingAndReadOnly(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	// krew prints caveats right after a successful install — the moment both
	// of these are relevant.
	for _, want := range []string{
		"kubectl kubeagent scan --context",
		"read-only",
		"--fix",
	} {
		if !strings.Contains(m.Spec.Caveats, want) {
			t.Errorf("spec.caveats does not mention %q:\n%s", want, m.Spec.Caveats)
		}
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go mod tidy
go test . -run TestRenderKrewManifest 2>&1 | head -20
```

Expected: FAIL — every subtest fails with `fork/exec scripts/render-krew-manifest.sh: no such file or directory`. (`go mod tidy` first, so the failure is the missing script and not a missing `sigs.k8s.io/yaml` import.)

- [ ] **Step 4: Write the renderer**

Create `scripts/render-krew-manifest.sh`:

```bash
#!/usr/bin/env bash
# render-krew-manifest.sh — render the krew plugin manifest for a release.
#
# Usage:  scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE > kubeagent.yaml
#
# Writes the rendered manifest to stdout. Checksums are looked up BY ARCHIVE
# FILENAME in SHA256SUMS_FILE, never by line order: this script and
# build-release-archives.sh must agree on names, not on positions.
#
# The manifest is generated at release time and never committed. A checksum
# written before the tag is a guess about bytes that do not exist yet;
# generating it in the same job that computed the checksums makes a stale
# checksum structurally impossible.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION="${1:-}"
SUMS="${2:-}"
[ -n "$VERSION" ] || die "usage: scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE"
[ -n "$SUMS" ] || die "usage: scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE"
[ -f "$SUMS" ] || die "no such checksum file: $SUMS"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMPL="$ROOT/krew/kubeagent.yaml.tmpl"
[ -f "$TMPL" ] || die "no such template: $TMPL"

# sum_for ARCHIVE — the sha256 recorded for exactly that filename.
# sha256sum writes "<hash>  <name>", or "<hash> *<name>" in binary mode.
sum_for() {
  local want="$1"
  local hash
  hash="$(awk -v f="$want" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$SUMS")"
  [ -n "$hash" ] || die "no checksum for $want in $SUMS"
  printf '%s' "$hash"
}

# Assign to variables first: a failing command substitution inside a `sed -e`
# argument would NOT trip `set -e`, and the manifest would silently render
# with an empty sha256.
SUM_LINUX_AMD64="$(sum_for "kubeagent_${VERSION}_linux_amd64.tar.gz")"
SUM_LINUX_ARM64="$(sum_for "kubeagent_${VERSION}_linux_arm64.tar.gz")"
SUM_DARWIN_AMD64="$(sum_for "kubeagent_${VERSION}_darwin_amd64.tar.gz")"
SUM_DARWIN_ARM64="$(sum_for "kubeagent_${VERSION}_darwin_arm64.tar.gz")"

rendered="$(sed \
  -e "s|{{VERSION}}|${VERSION}|g" \
  -e "s|{{SHA256_LINUX_AMD64}}|${SUM_LINUX_AMD64}|g" \
  -e "s|{{SHA256_LINUX_ARM64}}|${SUM_LINUX_ARM64}|g" \
  -e "s|{{SHA256_DARWIN_AMD64}}|${SUM_DARWIN_AMD64}|g" \
  -e "s|{{SHA256_DARWIN_ARM64}}|${SUM_DARWIN_ARM64}|g" \
  "$TMPL")"

# A surviving placeholder means the template grew a field this script does not
# know about. Fail here rather than shipping a manifest that fails for a user.
if printf '%s\n' "$rendered" | grep -q '{{'; then
  die "unsubstituted placeholder(s) in the rendered manifest: $(printf '%s\n' "$rendered" | grep -o '{{[A-Z0-9_]*}}' | sort -u | tr '\n' ' ')"
fi

printf '%s\n' "$rendered"
```

Then:

```bash
chmod +x scripts/render-krew-manifest.sh
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestRenderKrewManifest -v 2>&1 | tail -20
```

Expected: PASS for all six tests.

- [ ] **Step 6: Prove the by-name lookup test is not vacuous**

Temporarily break the renderer so it reads checksums positionally, and confirm the test catches it:

```bash
export PATH=$PATH:/usr/local/go/bin
backup="$(mktemp)"
cp scripts/render-krew-manifest.sh "$backup"

# break it: read the linux/amd64 sum from the FIRST LINE instead of by filename
sed -i 's|^SUM_LINUX_AMD64=.*|SUM_LINUX_AMD64="$(head -1 "$SUMS" \| cut -d" " -f1)"|' \
  scripts/render-krew-manifest.sh
grep '^SUM_LINUX_AMD64=' scripts/render-krew-manifest.sh          # confirm the mutation landed
go test . -run TestRenderKrewManifest_LooksChecksumsUpByNameNotLineOrder 2>&1 | tail -5

# restore
cp "$backup" scripts/render-krew-manifest.sh && rm "$backup"
git diff --stat scripts/render-krew-manifest.sh   # must print nothing if already committed, else re-check by eye
go test . -run TestRenderKrewManifest 2>&1 | tail -3
```

(The `\|` in the `sed` replacement escapes the pipe so `sed` does not read it as the expression delimiter; the line written into the script is a plain `head -1 … | cut …`.)

Expected: the broken version **FAILS** with "checksums are being read by line position, not by filename"; after restoring, all six PASS again. Record both outputs in the task report.

- [ ] **Step 7: Verify the whole suite and the dependency move**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head
git diff go.mod
```

Expected: no failures; `go.mod` shows `sigs.k8s.io/yaml v1.6.0` moved into the direct `require` block (no version change, no new module downloaded — it was already in the graph as an indirect dependency of client-go).

- [ ] **Step 8: Commit**

```bash
git add krew/kubeagent.yaml.tmpl scripts/render-krew-manifest.sh krew_manifest_test.go go.mod go.sum
git commit -m "feat(krew): render the plugin manifest from a template at release time

krew/kubeagent.yaml.tmpl carries the plugin metadata and four platform
entries; scripts/render-krew-manifest.sh substitutes the version and the four
checksums, looked up by archive filename rather than by line position, and
refuses to emit a manifest with a surviving placeholder or a missing checksum.

The Go test executes the script itself rather than reimplementing its
substitutions, and uses four distinct fixture checksums so a renderer that
pasted one into all four slots fails."
```

---

### Task 4: wire the release workflow to both scripts

**Files:**
- Modify: `.github/workflows/release.yml:35-60` (the Build, Package, and Publish steps)

**Interfaces:**
- Consumes: `scripts/build-release-archives.sh VERSION OUTDIR` (Task 2) and `scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE` (Task 3).
- Produces: a GitHub Release carrying seven assets — four versioned archives, the unversioned `kubeagent_linux_amd64.tar.gz`, `SHA256SUMS`, and `kubeagent.yaml`.

The Docker steps that follow are untouched: `Dockerfile` is a multi-stage build that compiles inside the image (`COPY . .` then `go build`), so it never depended on the repo-root `kubeagent` binary the old Build step produced.

- [ ] **Step 1: Replace the Build and Package steps**

In `.github/workflows/release.yml`, replace the `Build linux/amd64` step (lines 35-41) and the `Package + checksums` step (lines 43-51) — both of them, whose current text is exactly:

```yaml
      - name: Build linux/amd64
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: |
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
            go build -ldflags "-X main.version=${VERSION}" -o kubeagent .

      - name: Package + checksums
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: |
          tar -czf "kubeagent_${VERSION}_linux_amd64.tar.gz" kubeagent README.md
          # Unversioned copy so releases/latest/download/kubeagent_linux_amd64.tar.gz
          # always resolves to the newest release (used by the README quick-install).
          cp "kubeagent_${VERSION}_linux_amd64.tar.gz" kubeagent_linux_amd64.tar.gz
          sha256sum "kubeagent_${VERSION}_linux_amd64.tar.gz" > SHA256SUMS
          cat SHA256SUMS
```

with:

```yaml
      - name: Build release archives
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: scripts/build-release-archives.sh "${VERSION}" dist

      - name: Render krew manifest
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: |
          scripts/render-krew-manifest.sh "${VERSION}" dist/SHA256SUMS > dist/kubeagent.yaml
          cat dist/kubeagent.yaml
```

- [ ] **Step 2: Update the release assets**

Replace the whole `Publish GitHub Release` step, whose current text is exactly:

```yaml
      - name: Publish GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          tag_name: ${{ steps.ver.outputs.version }}
          files: |
            kubeagent_${{ steps.ver.outputs.version }}_linux_amd64.tar.gz
            kubeagent_linux_amd64.tar.gz
            SHA256SUMS
```

with:

```yaml
      - name: Publish GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          tag_name: ${{ steps.ver.outputs.version }}
          files: |
            dist/kubeagent_${{ steps.ver.outputs.version }}_linux_amd64.tar.gz
            dist/kubeagent_${{ steps.ver.outputs.version }}_linux_arm64.tar.gz
            dist/kubeagent_${{ steps.ver.outputs.version }}_darwin_amd64.tar.gz
            dist/kubeagent_${{ steps.ver.outputs.version }}_darwin_arm64.tar.gz
            dist/kubeagent_linux_amd64.tar.gz
            dist/SHA256SUMS
            dist/kubeagent.yaml
```

Assets upload under their basename, so `dist/kubeagent.yaml` becomes `kubeagent.yaml` — which is what `releases/latest/download/kubeagent.yaml` resolves to, and what the documented `kubectl krew install --manifest-url` line points at.

- [ ] **Step 3: Verify the workflow parses and the executable bits are set**

```bash
python3 -c "import yaml,sys; d=yaml.safe_load(open('.github/workflows/release.yml')); print([s.get('name') for s in d['jobs']['release']['steps']])"
git ls-files -s scripts/build-release-archives.sh scripts/render-krew-manifest.sh
```

Expected: the step list prints in order — `None` (checkout), `None` (setup-go), `Resolve version`, `Test`, `Build release archives`, `Render krew manifest`, `Publish GitHub Release`, `Log in to Docker Hub`, `Build + push image`. Both scripts show mode `100755`; if either shows `100644`, run `git update-index --chmod=+x <path>` and re-check.

- [ ] **Step 4: Dry-run the two steps exactly as the workflow does**

```bash
export PATH=$PATH:/usr/local/go/bin
rm -rf dist
scripts/build-release-archives.sh v0.0.0-wfcheck dist
scripts/render-krew-manifest.sh v0.0.0-wfcheck dist/SHA256SUMS > dist/kubeagent.yaml
grep -c 'sha256:' dist/kubeagent.yaml
grep 'sha256:' dist/kubeagent.yaml | sort -u | wc -l
grep -c '{{' dist/kubeagent.yaml || echo "0 placeholders"
ls dist
rm -rf dist
```

Expected: `4` sha256 lines; `4` **distinct** sha256 lines (four different archives, four different hashes — a renderer or build that produced identical bytes for all four platforms would show fewer); `0 placeholders`; `ls` shows the six artifacts plus `kubeagent.yaml`. `rm -rf dist` afterwards, and `git status --short` must show only ` M .github/workflows/release.yml`.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: build four platforms and attach the krew manifest to the release

The release job now calls scripts/build-release-archives.sh and
scripts/render-krew-manifest.sh instead of building one platform inline, and
uploads kubeagent.yaml alongside the archives so
releases/latest/download/kubeagent.yaml always names the current version.

The Docker steps are untouched: the Dockerfile compiles inside the image and
never used the repo-root binary the old Build step produced."
```

---

### Task 5: documentation

Every surface that tells someone how to install kubeagent must learn about the plugin, or the docs will disagree with each other.

**Files:**
- Modify: `README.md` (Install section at :408-423; the release-workflow paragraph at :447-450)
- Modify: `website/docs/install.md` (the `## Prebuilt binary (linux/amd64)` section at :1-23)
- Modify: `website/docs/quickstart.md` (the `## Build and run` section)
- Modify: `CHANGELOG.md` (`## [Unreleased]`)
- Modify: `website/docs/roadmap.md` (the Theme G bullet at :424-428; the `**v0.5x**` milestone row)
- Modify: `CLAUDE.md` (the `## Build, test, run` section; the Theme G note in `## Roadmap`)
- Deliberately untouched: `deploy/README.md` (in-cluster watch daemon; krew has nothing to do with it)

**Interfaces:**
- Consumes: the install command and asset names produced by Tasks 2-4.
- Produces: nothing other tasks depend on.

The install command documented everywhere is exactly:

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
```

Not `kubectl krew install kubeagent` — kubeagent is not in the upstream krew-index yet, and the docs must say so plainly rather than implying membership that does not exist.

- [ ] **Step 1: README — add krew to the Install section**

Replace `README.md` lines 408-423 in full. The existing text is exactly:

````markdown
## Install

Prebuilt **linux/amd64** binaries are attached to each
[GitHub Release](https://github.com/imantaba/kubeagent/releases). Download, verify
the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_linux_amd64.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_linux_amd64.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```
````

Replace it with:

````markdown
## Install

### As a `kubectl` plugin (krew)

With [krew](https://krew.sigs.k8s.io) installed, install kubeagent from the
manifest attached to the latest release:

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
kubectl kubeagent scan
```

kubeagent is not in the upstream krew-index yet, so `--manifest-url` is
required — plain `kubectl krew install kubeagent` will not find it.

Flags go **after** the plugin name; `kubectl` does not forward its own global
flags to plugins:

```bash
kubectl kubeagent scan --context prod-eu     # works
kubectl --context prod-eu kubeagent scan     # does not
```

### Prebuilt binary

Binaries for **linux/amd64**, **linux/arm64**, **darwin/amd64** and
**darwin/arm64** are attached to each
[GitHub Release](https://github.com/imantaba/kubeagent/releases). Download, verify
the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
OS=linux; ARCH=amd64
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```

Windows is not published: nothing in this project's test or chaos suite has
ever run on it.
````

`--ignore-missing` is **required**, not cosmetic: `SHA256SUMS` now lists five archives and the reader downloaded one, so the plain `sha256sum -c SHA256SUMS` this recipe used would fail on the four absent files and exit nonzero. A copy-pasteable recipe that errors is worse than no recipe.

- [ ] **Step 2: README — correct the release-workflow description**

Replace the sentence at `README.md:447-450`:

```markdown
The release workflow runs the tests, builds
`kubeagent_<version>_linux_amd64.tar.gz` + `SHA256SUMS`, and attaches them to the
GitHub Release. Every push and PR is checked by the CI workflow (vet + test +
build).
```

with:

```markdown
The release workflow runs the tests, builds the four platform archives
(`kubeagent_<version>_{linux,darwin}_{amd64,arm64}.tar.gz`) plus `SHA256SUMS`,
renders the krew plugin manifest from `krew/kubeagent.yaml.tmpl` with those
checksums, and attaches everything to the GitHub Release. Every push and PR is
checked by the CI workflow (vet + test + build).
```

- [ ] **Step 3: website/docs/install.md — krew section and the platform table**

Replace the whole `## Prebuilt binary (linux/amd64)` section (lines 1-24, from the `# Install` heading down to and including the "Latest release" tip) with:

```markdown
# Install

## As a `kubectl` plugin (krew)

With [krew](https://krew.sigs.k8s.io) installed, install kubeagent from the
manifest attached to the latest release:

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
kubectl kubeagent scan
```

kubeagent is not in the upstream krew-index yet, so `--manifest-url` is
required — plain `kubectl krew install kubeagent` will not find it.

!!! warning "Flags go after the plugin name"
    `kubectl` does not forward its own global flags to plugins:

    ```bash
    kubectl kubeagent scan --context prod-eu     # works
    kubectl --context prod-eu kubeagent scan     # does not
    ```

    kubeagent's `--context` and `--kubeconfig` are spelled exactly like
    kubectl's, and `KUBECONFIG` is read from the environment the same way, so
    the habit transfers intact.

## Prebuilt binary

Binaries are attached to each GitHub Release for:

| OS | Arch | Archive |
|----|------|---------|
| linux | amd64 | `kubeagent_<version>_linux_amd64.tar.gz` |
| linux | arm64 | `kubeagent_<version>_linux_arm64.tar.gz` |
| macOS | amd64 | `kubeagent_<version>_darwin_amd64.tar.gz` |
| macOS | arm64 | `kubeagent_<version>_darwin_arm64.tar.gz` |

Windows is not published: nothing in this project's test or chaos suite has
ever run on it.

Download, verify the checksum, and run:

```bash
VERSION=v1.2.3   # the release you want
OS=linux; ARCH=amd64
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
curl -sSLO "${base}/kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
tar xzf "kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz"
./kubeagent version   # prints the build's version
./kubeagent scan
```

!!! tip "Latest release"
    Find all releases — including the latest version number to substitute for
    `VERSION` above — on the
    [Releases page](https://github.com/imantaba/kubeagent/releases).
```

`--ignore-missing` matters now that `SHA256SUMS` lists five archives and the reader downloaded one.

- [ ] **Step 4: website/docs/quickstart.md — the first command a new user runs**

In `website/docs/quickstart.md`, replace the `## Build and run` section:

```markdown
## Build and run

```bash
go build -o kubeagent .

# scan: prioritized problem report — cluster health (P1) then workload failures (P2)
./kubeagent scan
```
```

with:

```markdown
## Install and run

As a `kubectl` plugin, via [krew](https://krew.sigs.k8s.io):

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml

# scan: prioritized problem report — cluster health (P1) then workload failures (P2)
kubectl kubeagent scan
```

Or build from source and run the binary directly — every command below works
either way, as `kubectl kubeagent …` or as `./kubeagent …`:

```bash
go build -o kubeagent .
./kubeagent scan
```

See [Install](install.md) for prebuilt binaries and the in-cluster daemon.
```

- [ ] **Step 5: CHANGELOG — the `[Unreleased]` entry**

Under `## [Unreleased]` in `CHANGELOG.md` (line 8), insert:

```markdown
## [Unreleased]

### Added

- **`kubectl` plugin via krew** — kubeagent installs as a `kubectl` plugin with
  `kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml`,
  after which `kubectl kubeagent scan` works anywhere `kubectl` does. The
  binary is unchanged: same detectors, same output, same read-only default.
  Usage and error text now name the command you actually typed, so a plugin
  user is no longer told to run `kubeagent`, which is not on their `PATH`.
  Not yet in the upstream krew-index, so `--manifest-url` is required.

### Changed

- **Releases now ship four platforms** — `linux/amd64`, `linux/arm64`,
  `darwin/amd64` and `darwin/arm64`, each a tarball with the binary, `README.md`
  and `LICENSE`, all listed in `SHA256SUMS`. The unversioned
  `kubeagent_linux_amd64.tar.gz` asset is unchanged, so the existing
  `releases/latest/download/…` quick-install keeps working. The krew manifest
  is rendered at release time from `krew/kubeagent.yaml.tmpl` with those
  checksums and attached as `kubeagent.yaml`. Windows is deliberately not
  published.
```

- [ ] **Step 6: roadmap — mark the krew plugin shipped**

Three edits in `website/docs/roadmap.md`.

**(a)** Replace the Theme G bullet (lines 424-428), whose current text is exactly:

```markdown
- **G · Meet people where they work** — an **MCP server** so other AI agents can
  call kubeagent's read-only diagnosis as a trusted tool (shipped, `kubeagent
  mcp`); a `kubectl` krew plugin; a CI/CD gate mode (pre-deploy sanity,
  post-deploy verify, SARIF, exit codes); an interactive TUI and a shareable
  HTML report.
```

with:

```markdown
- **G · Meet people where they work** — an **MCP server** so other AI agents can
  call kubeagent's read-only diagnosis as a trusted tool (shipped, `kubeagent
  mcp`); a **`kubectl` krew plugin** (shipped, `kubectl kubeagent`); a CI/CD
  gate mode (pre-deploy sanity, post-deploy verify, SARIF, exit codes); an
  interactive TUI and a shareable HTML report.
```

**(b)** Replace the `**v0.5x**` milestone row (line 444) with:

```markdown
| **v0.5x** | Interfaces & adoption (G) | **MCP server** (shipped, `kubeagent mcp`); **`kubectl` krew plugin** (shipped); CI/CD gate mode + SARIF; interactive TUI + HTML report; optional in-cluster dashboard |
```

**(c)** Append **two** bullets to the end of the `## Shipped` list — that list is oldest-first, so they go after the final bullet (the multicluster-watch entry ending `[Watch mode](features/watch-mode.md#watching-several-clusters).` at line 358) and **before** the blank line and the `!!! info "Version history"` admonition at line 360. Do not put them near `### Themes` or `### Principles that don't change`; those are different lists.

Two bullets, not one: the MCP server shipped in v0.63.0 and was never added to this list — only to the Theme G bullet and the milestone row. Adding krew alone would leave the list claiming a Theme G with no MCP server in it.

```markdown
- **MCP server (`kubeagent mcp`)** — serves kubeagent's deterministic, read-only
  diagnosis to other AI agents over MCP on stdio: `kubeagent_triage`,
  `kubeagent_inspect`, `kubeagent_advisory`, and (only with
  `--allow-context-switch`) `list_contexts`. There is no write path and no model
  call anywhere in the server, and kubeconfig paths never reach a caller.
  **Theme G — slice 1.** See [MCP server](features/mcp.md).
- **`kubectl` plugin (krew)** — kubeagent installs as a `kubectl` plugin through
  [krew](https://krew.sigs.k8s.io), so `kubectl kubeagent scan` works anywhere
  `kubectl` does. Releases now carry four platform archives (linux and macOS ×
  amd64 and arm64) and a krew manifest rendered from those archives' checksums;
  the binary is unchanged apart from usage text that names whichever command you
  typed. Not in the upstream krew-index yet, so install is by `--manifest-url`.
  **Theme G — slice 2.** See [Install](install.md).
```

- [ ] **Step 7: CLAUDE.md — build/run and the Theme G note**

In `CLAUDE.md`, replace the `Run:` line in `## Build, test, run`:

```markdown
- Run:   `./kubeagent scan [--kubeconfig path] [--output text|json]`
- Or as a `kubectl` plugin (krew): `kubectl kubeagent scan …` — same binary,
  same flags. `invocationName` in `main.go` reads `argv[0]` so usage and error
  text name whichever spelling the user typed.
```

And in `## Roadmap`, replace the "Theme G slice 1 has shipped" bullet:

```markdown
- **Theme G slices 1 and 2 have shipped:** the MCP server (`kubeagent mcp`),
  documented in [website/docs/features/mcp.md](website/docs/features/mcp.md),
  and the `kubectl` krew plugin (`krew/kubeagent.yaml.tmpl` +
  `scripts/render-krew-manifest.sh`, rendered at release time and never
  committed). The rest of Theme G — a CI/CD gate mode, an interactive TUI,
  and a shareable HTML report — remains ahead.
```

- [ ] **Step 8: Verify the docs build and nothing contradicts**

```bash
export PATH=$PATH:$HOME/.local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml) 2>&1 | tail -5
grep -rn "linux/amd64 binaries\|linux/amd64) binaries\|only linux/amd64" README.md website/docs/ || echo "no stale single-platform claim"
grep -rn "krew install kubeagent$" README.md website/docs/ || echo "no index-membership claim"
```

Expected: `Documentation built`, exit 0, no `WARNING` lines naming the edited pages (the red "Material for MkDocs 2.0" banner is cosmetic); both greps report their "no …" fallback, meaning no surface still claims a single platform or implies krew-index membership.

- [ ] **Step 9: Commit**

```bash
git add README.md website/docs/install.md website/docs/quickstart.md CHANGELOG.md website/docs/roadmap.md CLAUDE.md
git commit -m "docs: install kubeagent as a kubectl plugin

README, install, quickstart, changelog, roadmap and CLAUDE.md all learn the
krew install line and the four published platforms. The docs say plainly that
--manifest-url is required because kubeagent is not in the upstream
krew-index, rather than implying a membership that does not exist.

deploy/README.md is deliberately untouched: it covers the in-cluster watch
daemon, which krew has nothing to do with."
```

---

## Gate (run by the controller after Task 5, before the whole-branch review)

Not the chaos suite: this slice changes no `internal/collect`, `internal/cluster`, RBAC, `--fix`, watch-daemon or Helm-template code, and the chaos suite does not exercise packaging at all. The gate is a real krew install of a really-built archive.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
OUT=$(mktemp -d)

kind create cluster --name kubeagent-smoke --wait 90s

scripts/build-release-archives.sh v0.0.0-smoke "$OUT"
scripts/render-krew-manifest.sh v0.0.0-smoke "$OUT/SHA256SUMS" > "$OUT/kubeagent.yaml"

kubectl krew install --manifest="$OUT/kubeagent.yaml" \
  --archive="$OUT/kubeagent_v0.0.0-smoke_linux_amd64.tar.gz"

kubectl kubeagent version                              # want: kubeagent v0.0.0-smoke
kubectl kubeagent scan --context kind-kubeagent-smoke  # want: a real report
kubectl kubeagent                                      # want: "usage: kubectl kubeagent scan …"

kubectl krew uninstall kubeagent
kind delete cluster --name kubeagent-smoke
rm -rf "$OUT"
```

Three things this proves that no unit test can:

1. `--archive` skips the download but **not** the checksum — krew still verifies the local file against the manifest's `sha256`. Because the manifest was rendered from the `SHA256SUMS` the build script wrote, a successful install proves the two scripts agree on the same bytes.
2. `kubectl kubeagent scan` runs the real binary through krew's symlink against a live cluster.
3. The bare `kubectl kubeagent` line is the entire point of `invocationName`: it requires the process to have been launched under the name `kubectl-kubeagent` by kubectl itself.

If `kubectl krew` is not installed on the gate machine, install it per <https://krew.sigs.k8s.io/docs/user-guide/setup/install/> first; the gate cannot be substituted with a unit test.

## Whole-branch review

Dispatch on the most capable model (opus) with `scripts/review-package $(git merge-base main HEAD) HEAD`. Point it at the Global Constraints above, at the spec, and at any Minor findings recorded in `.superpowers/sdd/progress.md`.
