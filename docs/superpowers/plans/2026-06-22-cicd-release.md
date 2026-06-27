# kubeagent CI/CD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a CI workflow (vet/test/build on push & PR) and a release workflow (tag `v*` or manual dispatch → linux/amd64 binary published as GitHub Release assets), plus a `kubeagent version` subcommand whose value is stamped at release build time.

**Architecture:** Hand-rolled GitHub Actions — `.github/workflows/ci.yml` and `.github/workflows/release.yml`. The release build injects the version via `-ldflags "-X main.version=<ver>"`; `main.go` gains `var version` + a `versionLine()` helper exposed as the `version` subcommand.

**Tech Stack:** Go 1.26 (stdlib), GitHub Actions (`actions/checkout@v4`, `actions/setup-go@v5`, `softprops/action-gh-release@v2`). No new Go module dependency.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go from `go.mod` (1.26.4).
- CLI keeps the standard-library `flag` style and subcommand shape (`scan`, now also `version`).
- **No new Go module dependency** (only GitHub Actions, which run in CI, not in the binary).
- **Triggers:** release on pushed tag `v*` AND `workflow_dispatch` (required `version` input). CI on push to `main` and on PRs.
- **Architecture target:** linux/amd64 only.
- **Release assets:** `kubeagent_<version>_linux_amd64.tar.gz` (binary + `README.md`) and `SHA256SUMS`.
- Actions pinned at major versions: `checkout@v4`, `setup-go@v5`, `action-gh-release@v2`.
- Workflow YAML validated locally with `python3 -c "import yaml; yaml.safe_load(open(PATH))"` (actionlint not available here); they truly run on GitHub.
- Each task keeps `go build ./...` and `go test ./...` green.

---

## File Structure

- `main.go` — **modify.** Add `var version`, `versionLine()`, and the `version` subcommand in `run`.
- `main_test.go` — **modify.** Tests for `versionLine()` and `run(["version"])`.
- `.github/workflows/ci.yml` — **new.** CI (vet/test/build on push & PR).
- `.github/workflows/release.yml` — **new.** Release (build + publish linux/amd64 assets).
- `README.md` — **modify.** Install / Releases section.

---

## Task 1: `version` subcommand

**Files:**
- Modify: `main.go`
- Modify: `main_test.go`

**Interfaces:**
- Produces: `var version = "dev"` (overridden by `-ldflags "-X main.version=<ver>"`); `func versionLine() string` → `"kubeagent " + version`; `run` handles the `version` subcommand (prints `versionLine()` to stdout, returns nil).

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go`:

```go
func TestVersionLine(t *testing.T) {
	// In tests the binary isn't ldflags-stamped, so version is the "dev" default.
	if got := versionLine(); got != "kubeagent dev" {
		t.Errorf("versionLine() = %q, want %q", got, "kubeagent dev")
	}
}

func TestRun_Version(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Errorf("run([version]) returned error: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestVersionLine|TestRun_Version' 2>&1 | tail -8
```
Expected: FAIL — compile error: undefined `versionLine` (and `run` doesn't handle `version` yet).

- [ ] **Step 3: Implement in `main.go`**

Add the version var + helper near the top of the file (after the imports, before `main`):

```go
// version is the build version, overridden at release time via
// -ldflags "-X main.version=<tag>". Local/dev builds report "dev".
var version = "dev"

// versionLine is the one-line string printed by `kubeagent version`.
func versionLine() string {
	return "kubeagent " + version
}
```

In `run`, handle the `version` subcommand before the `scan` usage check, and add `version` to the usage string. The top of `run` becomes:

```go
func run(args []string) error {
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(os.Stdout, versionLine())
		return nil
	}
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json] [--explain] [--model name] [--include-cron] [--include-restarts] | kubeagent version")
	}
	// ... rest of run unchanged ...
```

(`fmt` and `os` are already imported.)

- [ ] **Step 4: Run the tests + build + verify ldflags stamping**

```bash
go test ./... 2>&1 | tail -5
go vet ./...
go build -ldflags "-X main.version=v9.9.9-test" -o /tmp/ka-version-test .
/tmp/ka-version-test version
```
Expected: all packages PASS; vet clean; the last command prints `kubeagent v9.9.9-test` (proves the ldflags stamping the release workflow relies on).

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add 'version' subcommand (stamped via -ldflags)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Create `.github/workflows/ci.yml`**

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test ./...
      - name: Build
        run: go build ./...
```

- [ ] **Step 2: Validate the YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml')); print('ci.yml: valid YAML')"
```
Expected: `ci.yml: valid YAML` (no exception).

- [ ] **Step 3: Sanity-check the steps locally**

```bash
export PATH=$PATH:/usr/local/go/bin
go vet ./... && go test ./... >/dev/null 2>&1 && go build ./... && echo "CI steps pass locally"
```
Expected: `CI steps pass locally` (the same commands the workflow runs).

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -m "ci: vet/test/build on push and PR" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: Release workflow

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: the `-X main.version` stamping from Task 1 (the build step injects the resolved version).

- [ ] **Step 1: Create `.github/workflows/release.yml`**

```yaml
name: Release

on:
  push:
    tags: ['v*']
  workflow_dispatch:
    inputs:
      version:
        description: "Release version tag, e.g. v1.2.3"
        required: true

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Resolve version
        id: ver
        run: echo "version=${{ github.event.inputs.version || github.ref_name }}" >> "$GITHUB_OUTPUT"

      - name: Test
        run: go test ./...

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
          sha256sum "kubeagent_${VERSION}_linux_amd64.tar.gz" > SHA256SUMS
          cat SHA256SUMS

      - name: Publish GitHub Release
        uses: softprops/action-gh-release@v2
        with:
          tag_name: ${{ steps.ver.outputs.version }}
          files: |
            kubeagent_${{ steps.ver.outputs.version }}_linux_amd64.tar.gz
            SHA256SUMS
```

- [ ] **Step 2: Validate the YAML**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('release.yml: valid YAML')"
```
Expected: `release.yml: valid YAML`.

- [ ] **Step 3: Dry-run the build + package + checksum steps locally**

Run the exact build/package/checksum commands the workflow runs, with a fake version, to prove they work and produce the named assets:

```bash
export PATH=$PATH:/usr/local/go/bin
VERSION=v0.0.0-dryrun
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=${VERSION}" -o kubeagent .
tar -czf "kubeagent_${VERSION}_linux_amd64.tar.gz" kubeagent README.md
sha256sum "kubeagent_${VERSION}_linux_amd64.tar.gz" > /tmp/SHA256SUMS-dryrun
cat /tmp/SHA256SUMS-dryrun
./kubeagent version
# cleanup the local build artifacts (they are gitignored, but don't leave them around)
rm -f kubeagent "kubeagent_${VERSION}_linux_amd64.tar.gz"
```
Expected: a `kubeagent_v0.0.0-dryrun_linux_amd64.tar.gz` is produced, the checksum line prints, and `./kubeagent version` prints `kubeagent v0.0.0-dryrun`. (`kubeagent` and `*.tar.gz` are gitignored, so they won't be committed; the `rm` keeps the tree clean.)

- [ ] **Step 4: Commit**

```bash
git status --short   # confirm no stray build artifacts staged
git add .github/workflows/release.yml
git commit -m "ci: release workflow builds + publishes linux/amd64 assets" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Docs — Install / Releases

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Add an Install section**

In `README.md`, insert the following **immediately before the `## Roadmap` heading**:

```markdown
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

### Cutting a release

Push a version tag — or run the **Release** workflow manually from the Actions
tab with a `version` input:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The release workflow runs the tests, builds
`kubeagent_<version>_linux_amd64.tar.gz` + `SHA256SUMS`, and attaches them to the
GitHub Release. Every push and PR is checked by the CI workflow (vet + test +
build).
```

- [ ] **Step 2: Verify nothing broke**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | tail -4
```
Expected: build + tests still green (docs-only change).

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: install + release instructions" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-review

- **Spec coverage:** `version` subcommand stamped via ldflags → Task 1 ✅; `ci.yml` (push/PR vet+test+build, read perms, go.mod toolchain) → Task 2 ✅; `release.yml` (tag `v*` + dispatch, version resolution, test gate, linux/amd64 ldflags build, tar.gz + SHA256SUMS, `action-gh-release@v2`, `contents: write`) → Task 3 ✅; assets `kubeagent_<version>_linux_amd64.tar.gz` + `SHA256SUMS` → Task 3 ✅; README Install/Releases → Task 4 ✅; actions pinned at major versions ✅; no new module dependency ✅; YAML validated via pyyaml ✅.
- **Out-of-scope honored:** no other OS/arch, no GoReleaser/deb/Homebrew/containers, no signing/SBOM, no LICENSE file, no changelog generation.
- **Placeholder scan:** none — every step has complete code/YAML/commands.
- **Type/consistency:** `version`/`versionLine()` defined in Task 1 are what the release build stamps in Task 3 (`-X main.version`); the asset name `kubeagent_${VERSION}_linux_amd64.tar.gz` is identical in Task 3 (build/package + `files:`) and Task 4 (download docs); the `version` resolution (`inputs.version || ref_name`) feeds the build, package, and `tag_name` consistently.
- **Green-per-task:** Task 1 is TDD (build/tests green); Tasks 2–4 add workflow/doc files that don't affect `go build`/`go test` (they stay green); workflows validated by YAML parse + local dry-run of the exact commands.
