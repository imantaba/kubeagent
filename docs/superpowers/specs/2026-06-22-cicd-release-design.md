# kubeagent — Design: CI/CD (CI + Linux release)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-22

## Goal

Add GitHub Actions CI/CD: a **CI** workflow that vets/tests/builds on every push
and PR, and a **release** workflow that builds a Linux/amd64 binary and publishes
it as GitHub Release assets (tarball + checksums). The released binary reports
its own version.

## Decisions (from brainstorming)

- **Hand-rolled GitHub Actions** (no GoReleaser) — transparent, zero extra
  tooling, fits the project's lean ethos.
- **Triggers:** release runs on a pushed version tag `v*` **and** on manual
  `workflow_dispatch` (with a `version` input).
- **Architecture:** **linux/amd64** only.
- **Version stamping:** embed the version into the binary via
  `-ldflags "-X main.version=<ver>"`, exposed as a `kubeagent version`
  subcommand; dev builds report `dev`.
- **Scope:** both a CI workflow (push/PR) and the release workflow.

## Invariants preserved

- **Read-only** tool behavior is unchanged; this is build/release tooling only.
- CLI keeps the standard-library `flag` style and the subcommand shape (`scan`,
  now also `version`).
- No new Go module dependency (the only additions are GitHub Actions, which run
  in CI, not in the binary).

## Component 1 — `version` subcommand (`main.go`)

- Add a package-level `var version = "dev"`.
- `versionLine() string` — a pure helper returning `"kubeagent " + version` (so
  it's unit-testable without capturing stdout).
- In `run`, handle the `version` subcommand before the `scan` dispatch:
  `if args[0] == "version" { fmt.Fprintln(os.Stdout, versionLine()); return nil }`.
- Add `version` to the usage string.
- Release builds pass `-ldflags "-X main.version=$VERSION"`; local `go build`
  leaves it as `dev`.

## Component 2 — `.github/workflows/ci.yml`

- **Triggers:** `push` to `main`, and `pull_request`.
- **Job (ubuntu-latest):** `actions/checkout@v4` → `actions/setup-go@v5` with
  `go-version-file: go.mod` (tracks the project's Go version; module cache on) →
  `go vet ./...` → `go test ./...` → `go build ./...`.
- **Permissions:** default read-only (no write needed).

## Component 3 — `.github/workflows/release.yml`

- **Triggers:** `push` with `tags: ['v*']`, and `workflow_dispatch` with a
  required `version` input (e.g. `v1.2.3`).
- **Permissions:** `contents: write` (to create the Release and upload assets).
- **Job (ubuntu-latest):**
  1. `actions/checkout@v4` → `actions/setup-go@v5` (`go-version-file: go.mod`).
  2. Resolve `VERSION` — on tag push it's the tag (`github.ref_name`); on
     dispatch it's the `version` input. (`VERSION="${{ github.event.inputs.version || github.ref_name }}"`.)
  3. `go test ./...` (gate the release on passing tests).
  4. Build: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$VERSION" -o kubeagent .`
  5. Package: `tar -czf kubeagent_${VERSION}_linux_amd64.tar.gz kubeagent README.md`.
  6. Checksums: `sha256sum kubeagent_${VERSION}_linux_amd64.tar.gz > SHA256SUMS`.
  7. Publish: `softprops/action-gh-release@v2` with `tag_name: ${VERSION}` and
     `files:` the tarball + `SHA256SUMS`. On a tag push it attaches to that tag's
     release; on dispatch it creates the tag/release at the current commit.

**Release assets:** `kubeagent_<version>_linux_amd64.tar.gz` and `SHA256SUMS`.

## Component 4 — Docs (`README.md`)

An **Install / Releases** section:
- Download the Linux asset from the Releases page (or `gh release download`),
  verify with `sha256sum -c SHA256SUMS`, `tar xzf …`, then run `./kubeagent scan`.
- `kubeagent version` prints the build's version.
- How to cut a release: `git tag v1.2.3 && git push origin v1.2.3`, or run the
  release workflow manually from the Actions tab with a `version`.

## Testing / validation

- **`version` subcommand — TDD:** unit-test `versionLine()` (pure) and that
  `run(["version"])` returns nil; existing `TestRun_*` stay green.
- **Workflows:** validated with `actionlint` if available, otherwise a YAML
  parse (`python3 -c 'import yaml,sys; yaml.safe_load(open(f))'`) plus careful
  review. (They only truly execute on GitHub when a tag is pushed / the workflow
  is dispatched.)
- Actions are pinned at major versions: `actions/checkout@v4`,
  `actions/setup-go@v5`, `softprops/action-gh-release@v2`.

## Out of scope (explicit non-goals)

- Other OSes/arches (Windows, macOS, linux/arm64) — amd64 Linux only.
- GoReleaser / Homebrew tap / apt or deb packaging / container images.
- Signing (cosign/GPG) and SBOMs.
- A `LICENSE` file — none exists today; licensing is left out of this change
  (the tarball ships the binary + `README.md`). Can be added later.
- Auto-generated changelogs / release notes (the Release uses the default body).
