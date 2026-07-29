# Supply-Chain Integrity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every kubeagent release verifiable by someone who trusts nothing on the release page — signed, described by an SBOM, carrying build provenance, and rebuildable to the same bytes.

**Architecture:** Four self-contained changes to the release pipeline, no Go code under `internal/` touched. `scripts/build-release-archives.sh` becomes deterministic; a new `scripts/release-vars.sh` classifies the tag so a pre-release cannot move `:latest`; `.github/workflows/release.yml` gains keyless cosign signing, syft SBOMs and GitHub build-provenance attestations; a new documentation page tells a user how to check all of it.

**Tech Stack:** Bash (GNU tar ≥1.28, gzip), Go 1.26 (tests only, root `package main`), GitHub Actions, cosign (keyless / Fulcio / Rekor), syft (SPDX JSON), `actions/attest-build-provenance` + `actions/attest-sbom`.

**Spec:** [docs/superpowers/specs/2026-07-29-supply-chain-design.md](../specs/2026-07-29-supply-chain-design.md) (commit `8835cb4`).

**Branch:** `supply-chain`, cut off `main` at `ffd4a09`. Work happens in the worktree at `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/wt-supply`.

## Global Constraints

- **Every commit carries a `Signed-off-by` trailer matching its author.** Use `git commit -s`. `main` enforces DCO on pull requests (`scripts/dco-check.sh`); an unsigned commit cannot be merged. Verify with `scripts/dco-check.sh main` before reporting DONE.
- **No `Co-Authored-By: Claude` trailer, and no AI attribution of any kind** in any commit message, document, comment, changelog entry or code.
- **No secrets, credentials, private IPs or internal hostnames anywhere**, including test fixtures. Documentation IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains are RFC 2606 (`.example`). The only hosts this slice may name are `github.com`, `docker.io`, `token.actions.githubusercontent.com` and `k8sproject.top`.
- **No Go code under `internal/` changes.** All new Go is test-only, in the root `package main`.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.**
- **Go lives at `/usr/local/go/bin`** — `export PATH=$PATH:/usr/local/go/bin`.
- **Run the suite as `go test -p 2 ./...`.** Full parallelism can trip a known Go linker panic on `internal/advisory`; `-p 2` passes clean. Do not run with `-short`: the reproducibility test is not skipped, but `-short` is how a future reader might try to dodge it.
- **Nothing in this slice reads a kubeconfig, contacts a cluster, or makes a model call.**

---

### Task 1: Reproducible archives

Make the same tag rebuild to the same bytes.

`NOTICE` already ships in the archive and already appears in all four `files:` blocks of `krew/kubeagent.yaml.tmpl` — the Apache-2.0 relicense landed that. Do not re-add it; the archive test asserts it as a regression guard.

**Files:**
- Modify: `scripts/build-release-archives.sh`
- Test: `release_archives_test.go` (create, root `package main`)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: two environment knobs on `scripts/build-release-archives.sh` that later tasks and the docs rely on —
  - `SOURCE_DATE_EPOCH` (integer seconds; defaults to the HEAD commit time) fixes every archive mtime;
  - `RELEASE_PLATFORMS` (space-separated `os/arch` list; defaults to `linux/amd64 linux/arm64 darwin/amd64 darwin/arm64`) selects what gets built. It must always include `linux/amd64`.

- [ ] **Step 1: Write the failing test**

Create `release_archives_test.go`:

```go
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// These tests execute scripts/build-release-archives.sh itself. The script is
// what the release workflow runs, so the script is what must be tested: a Go
// reimplementation of its tar invocation would keep passing while the real
// script rotted.

const archiveTestVersion = "v9.9.9"

// A fixed epoch, so the test asserts an exact mtime rather than "whatever the
// build happened to stamp". 2023-11-14T22:13:20Z.
const archiveTestEpoch = 1700000000

// buildArchives runs the real script into outdir for two platforms. Two is
// enough to exercise the cross-platform loop while keeping a double build
// fast; linux/amd64 must be present because the script also produces the
// unversioned copy from it.
func buildArchives(t *testing.T, outdir string) {
	t.Helper()
	cmd := exec.Command("scripts/build-release-archives.sh", archiveTestVersion, outdir)
	cmd.Env = append(os.Environ(),
		"SOURCE_DATE_EPOCH="+strconv.Itoa(archiveTestEpoch),
		"RELEASE_PLATFORMS=linux/amd64 linux/arm64",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-release-archives.sh: %v\n%s", err, out)
	}
}

func TestReleaseArchives(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	buildArchives(t, dirA)
	buildArchives(t, dirB)

	// Same tag, same bytes. Without this a verifier who rebuilds the release
	// gets a mismatch and cannot tell tampering from timestamps.
	t.Run("checksums are identical across builds", func(t *testing.T) {
		sumsA := readArchiveFile(t, filepath.Join(dirA, "SHA256SUMS"))
		sumsB := readArchiveFile(t, filepath.Join(dirB, "SHA256SUMS"))
		if string(sumsA) != string(sumsB) {
			t.Errorf("SHA256SUMS differs between two builds of the same tree:\nfirst:\n%s\nsecond:\n%s", sumsA, sumsB)
		}
	})

	archive := filepath.Join(dirA, "kubeagent_"+archiveTestVersion+"_linux_amd64.tar.gz")

	// Two builds on one machine agree even when every entry carries
	// uid=1000 uname=ubuntu — the archive would still be unreproducible for
	// everyone else. These assertions are what actually pin that down.
	t.Run("tar entries carry no builder identity or clock", func(t *testing.T) {
		want := time.Unix(archiveTestEpoch, 0)
		for _, h := range tarHeaders(t, archive) {
			if h.Uid != 0 || h.Gid != 0 {
				t.Errorf("%s: uid/gid = %d/%d, want 0/0", h.Name, h.Uid, h.Gid)
			}
			if h.Uname != "" || h.Gname != "" {
				t.Errorf("%s: uname/gname = %q/%q, want empty", h.Name, h.Uname, h.Gname)
			}
			if !h.ModTime.Equal(want) {
				t.Errorf("%s: mtime = %s, want %s (SOURCE_DATE_EPOCH)", h.Name, h.ModTime.UTC(), want.UTC())
			}
		}
	})

	t.Run("entries are sorted and complete", func(t *testing.T) {
		var names []string
		for _, h := range tarHeaders(t, archive) {
			names = append(names, h.Name)
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("entries are not in sorted order: %v", names)
		}
		// Apache-2.0 section 4(d): NOTICE travels with the redistribution.
		for _, want := range []string{"kubeagent", "README.md", "LICENSE", "NOTICE"} {
			if !containsString(names, want) {
				t.Errorf("archive is missing %s; entries: %v", want, names)
			}
		}
	})

	// tar -czf lets gzip stamp its own header. gzip -n does not.
	t.Run("gzip header carries no timestamp", func(t *testing.T) {
		head := make([]byte, 10)
		f, err := os.Open(archive)
		if err != nil {
			t.Fatalf("open archive: %v", err)
		}
		defer f.Close()
		if _, err := io.ReadFull(f, head); err != nil {
			t.Fatalf("read gzip header: %v", err)
		}
		if mtime := binary.LittleEndian.Uint32(head[4:8]); mtime != 0 {
			t.Errorf("gzip header MTIME = %d, want 0 (gzip -n)", mtime)
		}
		const flagFNAME = 0x08
		if head[3]&flagFNAME != 0 {
			t.Errorf("gzip header FLG = %#x, want the FNAME bit clear (gzip -n)", head[3])
		}
	})
}

func readArchiveFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func tarHeaders(t *testing.T, archive string) []*tar.Header {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatalf("open %s: %v", archive, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()

	var headers []*tar.Header
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		headers = append(headers, h)
	}
	return headers
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -run TestReleaseArchives -v .
```

Expected: FAIL. `RELEASE_PLATFORMS` is not honoured yet, so the script builds all four platforms and then the mtime, uid/uname and gzip-header assertions fail on the archives it produced.

- [ ] **Step 3: Make the script deterministic**

In `scripts/build-release-archives.sh`, extend the header comment block — after the paragraph ending "…is a claim the project cannot back." — with:

```bash
# Archives are byte-reproducible: the same tag rebuilt on another machine
# produces the same SHA256SUMS. tar is told not to record the staging
# directory's mtime or the building user's uid and name, gzip is told not to
# stamp its own header, and the Go build is trimmed of absolute paths. Without
# this a verifier who rebuilds a release gets a mismatch and cannot tell
# tampering from timestamps.
#
# Requires GNU tar 1.28 or newer (--sort=name). bsdtar, the macOS default,
# does not accept these flags; the script already targets Linux and
# cross-compiles the darwin binaries.
#
# Environment:
#   SOURCE_DATE_EPOCH   archive mtime, seconds. Defaults to the HEAD commit time.
#   RELEASE_PLATFORMS   space-separated os/arch list. Defaults to all four.
#                       Must include linux/amd64.
```

After the `cd "$ROOT"` line, add:

```bash
# Defaulted from the commit rather than from the clock: "now" would be
# reproducible only within a single run, which defeats the point. A checkout
# with no commit time is an error, not a reason to substitute one.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --pretty=%ct 2>/dev/null || true)}"
[ -n "$SOURCE_DATE_EPOCH" ] ||
  die "SOURCE_DATE_EPOCH is unset and HEAD has no commit time (not a git checkout?) — set SOURCE_DATE_EPOCH to build reproducibly"

# RELEASE_PLATFORMS exists so the reproducibility test can double-build a
# subset quickly. Every real caller gets the default four.
: "${RELEASE_PLATFORMS:=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
```

Replace the build loop:

```bash
for platform in $RELEASE_PLATFORMS; do
  os="${platform%/*}"
  arch="${platform#*/}"
  echo "building ${os}/${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o "$stage/kubeagent" .
  # NOTICE travels with LICENSE: Apache-2.0 section 4(d) requires redistributions
  # to carry it.
  cp README.md LICENSE NOTICE "$stage/"
  # LC_ALL=C so --sort=name sorts bytewise everywhere, not by the builder's
  # locale collation. Each flag closes one leak: entry order, the building
  # user's uid and name, the staging directory's mtime. gzip is invoked
  # separately because tar -czf gives no way to pass it -n, and the gzip
  # header has its own filename and timestamp fields.
  LC_ALL=C tar --sort=name --numeric-owner --owner=0 --group=0 \
      --mtime="@${SOURCE_DATE_EPOCH}" \
      -C "$stage" -cf - kubeagent README.md LICENSE NOTICE |
    gzip -n > "${OUTDIR}/kubeagent_${VERSION}_${os}_${arch}.tar.gz"
done
```

Guard the unversioned copy — a `RELEASE_PLATFORMS` without `linux/amd64` must fail loudly, not silently drop the URL that is in the wild:

```bash
[ -f "${OUTDIR}/kubeagent_${VERSION}_linux_amd64.tar.gz" ] ||
  die "linux/amd64 was not built (RELEASE_PLATFORMS=${RELEASE_PLATFORMS}) — the unversioned archive that releases/latest/download resolves to cannot be produced"
cp "${OUTDIR}/kubeagent_${VERSION}_linux_amd64.tar.gz" \
   "${OUTDIR}/kubeagent_linux_amd64.tar.gz"
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test -run TestReleaseArchives -v .
```

Expected: PASS, all four subtests.

- [ ] **Step 5: Run the whole suite**

```bash
go test -p 2 ./...
```

Expected: all packages PASS. `krew_manifest_test.go` exercises the same archives indirectly and must stay green.

- [ ] **Step 6: Commit**

```bash
git add scripts/build-release-archives.sh release_archives_test.go
git commit -s -m "build: make release archives byte-reproducible

tar recorded the staging directory's mtime and the building user's uid and
name, and tar -czf let gzip stamp its own header, so the same tag rebuilt
elsewhere produced a different SHA256SUMS. A verifier who rebuilt a release
got a mismatch and could not tell tampering from timestamps.

The Go build is trimmed of absolute paths, tar takes a fixed mtime from
SOURCE_DATE_EPOCH with numeric zero ownership and bytewise entry order, and
gzip -n drops the header timestamp and filename."
```

---

### Task 2: Release classification and the `:latest` guard

Stop a pre-release tag from repointing `imantaba/kubeagent:latest`. This is a prerequisite for the gate, which pushes `v0.68.0-rc.1` through the real workflow.

**Files:**
- Create: `scripts/release-vars.sh`
- Modify: `.github/workflows/release.yml`
- Test: `release_vars_test.go` (create, root `package main`)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `scripts/release-vars.sh VERSION`, which prints exactly two lines of GitHub-Actions output syntax to stdout — `prerelease=<true|false>` then `push_latest=<true|false>` — and exits non-zero on a version that is not a SemVer release tag. The workflow reads them as `steps.rel.outputs.prerelease` and `steps.rel.outputs.push_latest`; Task 3 uses the same step id `rel`.

- [ ] **Step 1: Write the failing test**

Create `release_vars_test.go`:

```go
package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Executes the real scripts/release-vars.sh, for the same reason
// krew_manifest_test.go executes the real renderer: the script is what the
// release workflow runs.

// releaseVars runs the script and parses its key=value lines.
func releaseVars(t *testing.T, version string) map[string]string {
	t.Helper()
	out, err := exec.Command("scripts/release-vars.sh", version).Output()
	if err != nil {
		t.Fatalf("release-vars.sh %q: %v", version, err)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("release-vars.sh %q: line %q is not key=value", version, line)
		}
		vars[k] = v
	}
	return vars
}

func TestReleaseVars_Classification(t *testing.T) {
	cases := []struct {
		version    string
		prerelease string
		pushLatest string
	}{
		{"v1.2.3", "false", "true"},
		{"v0.68.0", "false", "true"},
		{"v1.2.3-rc.1", "true", "false"},
		{"v0.68.0-alpha.2", "true", "false"},
		{"v1.0.0-0.beta", "true", "false"},
		// Build metadata is not a pre-release: +build.5 says how it was
		// built, not that it is provisional.
		{"v1.2.3+build.5", "false", "true"},
		{"v1.2.3-rc.1+build.5", "true", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			vars := releaseVars(t, tc.version)
			if got := vars["prerelease"]; got != tc.prerelease {
				t.Errorf("prerelease = %q, want %q", got, tc.prerelease)
			}
			if got := vars["push_latest"]; got != tc.pushLatest {
				t.Errorf("push_latest = %q, want %q", got, tc.pushLatest)
			}
		})
	}
}

// A malformed tag must stop the release. Exiting 0 with a best guess would
// publish a release under a name nobody chose.
func TestReleaseVars_RejectsMalformedVersions(t *testing.T) {
	for _, version := range []string{"", "1.2.3", "v1.2", "vX.Y.Z", "latest", "v1.2.3.4", "v1.2.3 -rc.1"} {
		t.Run("reject "+version, func(t *testing.T) {
			out, err := exec.Command("scripts/release-vars.sh", version).CombinedOutput()
			if err == nil {
				t.Fatalf("release-vars.sh %q exited 0, want non-zero; output:\n%s", version, out)
			}
			if !strings.Contains(string(out), "error:") {
				t.Errorf("release-vars.sh %q: stderr does not explain the failure:\n%s", version, out)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test -run TestReleaseVars -v .
```

Expected: FAIL — `fork/exec scripts/release-vars.sh: no such file or directory`.

- [ ] **Step 3: Write the script**

Create `scripts/release-vars.sh` (and `chmod +x` it):

```bash
#!/usr/bin/env bash
# release-vars.sh — classify a release tag for the release workflow.
#
# Usage:  scripts/release-vars.sh VERSION
#
# Prints GitHub-Actions output syntax on stdout, for redirection into
# "$GITHUB_OUTPUT":
#
#   prerelease=false
#   push_latest=true
#
# A SemVer pre-release (v1.2.3-rc.1) must not move the :latest image tag.
# Every unpinned `docker pull imantaba/kubeagent` resolves through it, and a
# release candidate is by definition not what an unpinned pull should get. It
# is also published as a GitHub pre-release rather than as the newest release.
# Without this rule the pre-release tag that exercises the release pipeline
# would ship a candidate to everyone.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION="${1:-}"
[ -n "$VERSION" ] || die "usage: scripts/release-vars.sh VERSION"

# SemVer with a mandatory leading v. Anything else stops the release: a
# malformed tag should not produce a release with a malformed name.
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
  die "not a SemVer release tag: $VERSION (want vMAJOR.MINOR.PATCH[-prerelease][+build])"
fi

# Strip build metadata before looking for the pre-release hyphen: the hyphen
# in v1.2.3+build-5 belongs to the metadata, not to a pre-release.
core="${VERSION%%+*}"
if [[ "$core" == *-* ]]; then
  prerelease=true
  push_latest=false
else
  prerelease=false
  push_latest=true
fi

printf 'prerelease=%s\n' "$prerelease"
printf 'push_latest=%s\n' "$push_latest"
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
chmod +x scripts/release-vars.sh
go test -run TestReleaseVars -v .
```

Expected: PASS, every subtest.

- [ ] **Step 5: Wire the guard into the release workflow**

In `.github/workflows/release.yml`, add a classification step immediately after the existing `Resolve version` step:

```yaml
      - name: Classify release
        id: rel
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: scripts/release-vars.sh "$VERSION" >> "$GITHUB_OUTPUT"
```

In the `Publish GitHub Release` step, add one line under `with:` (beside `tag_name`):

```yaml
          prerelease: ${{ steps.rel.outputs.prerelease }}
```

Replace the `Build + push image` step's `run:` body so `:latest` moves only for a real release:

```yaml
      - name: Build + push image
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        env:
          VERSION: ${{ steps.ver.outputs.version }}
          PUSH_LATEST: ${{ steps.rel.outputs.push_latest }}
        run: |
          docker build -t "imantaba/kubeagent:${VERSION}" --build-arg VERSION="${VERSION}" .
          docker push "imantaba/kubeagent:${VERSION}"
          if [ "$PUSH_LATEST" = "true" ]; then
            docker tag "imantaba/kubeagent:${VERSION}" imantaba/kubeagent:latest
            docker push imantaba/kubeagent:latest
          else
            echo "pre-release ${VERSION}: leaving :latest pointing where it is"
          fi
```

- [ ] **Step 6: Check the workflow still parses**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('release.yml parses')"
```

Expected: `release.yml parses`.

There is no automated test of the workflow YAML itself. A test asserting on step text would pin the file's prose rather than its behaviour, and the behaviour is proven by the pre-release dry run in the gate.

- [ ] **Step 7: Run the whole suite**

```bash
go test -p 2 ./...
```

Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add scripts/release-vars.sh release_vars_test.go .github/workflows/release.yml
git commit -s -m "ci: keep pre-release tags off the :latest image tag

Any v* tag repointed imantaba/kubeagent:latest, so a release candidate tag
would have handed every unpinned docker pull an untested build. release-vars.sh
classifies the tag: a SemVer pre-release publishes as a GitHub pre-release and
pushes only its own image tag, and a malformed tag stops the release rather
than producing one under a name nobody chose."
```

---

### Task 3: Signing, SBOM and provenance

**Files:**
- Modify: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `steps.rel.outputs.prerelease` and `steps.rel.outputs.push_latest` from Task 2's `Classify release` step; `SOURCE_DATE_EPOCH` behaviour from Task 1 (no change needed — CI's checkout provides the commit time).
- Produces: two new release assets whose names the documentation task cites verbatim — `SHA256SUMS.cosign.bundle` and `kubeagent_<version>_sbom.spdx.json` — plus attestations keyed by artifact digest in the GitHub attestation store.

- [ ] **Step 1: Grant the workflow the permissions signing needs**

Replace the top-level `permissions:` block:

```yaml
permissions:
  contents: write      # publish the GitHub Release
  id-token: write      # OIDC token -> short-lived Fulcio certificate (keyless cosign)
  attestations: write  # actions/attest-* write to the GitHub attestation store
```

- [ ] **Step 2: Install cosign and syft**

After the `actions/setup-go@v5` step:

```yaml
      - uses: sigstore/cosign-installer@v3
      - uses: anchore/sbom-action/download-syft@v0
```

- [ ] **Step 3: Sign the checksums and generate the SBOM**

Insert these two steps after `Render krew manifest` and before `Publish GitHub Release`:

```yaml
      # SHA256SUMS already binds every archive by hash, so one signature over
      # it covers the whole release. Keyless: the signing certificate is
      # issued to this workflow's OIDC identity and recorded in Rekor, so
      # there is no private key to guard, rotate, or steal.
      - name: Sign checksums
        run: cosign sign-blob --yes dist/SHA256SUMS --bundle dist/SHA256SUMS.cosign.bundle

      # syft scans the built binary rather than go.sum: the SBOM should list
      # what was linked in, not everything the module graph could have
      # provided.
      - name: Generate SBOM
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: |
          mkdir -p sbom
          tar xzf "dist/kubeagent_${VERSION}_linux_amd64.tar.gz" -C sbom kubeagent
          syft scan "file:sbom/kubeagent" -o "spdx-json=dist/kubeagent_${VERSION}_sbom.spdx.json"
```

- [ ] **Step 4: Attest the archives**

Immediately after the two steps above:

```yaml
      - name: Attest archive provenance
        uses: actions/attest-build-provenance@v2
        with:
          subject-path: dist/kubeagent_*.tar.gz

      # Bound to the linux/amd64 archive specifically, because that is the
      # binary syft scanned. Claiming this SBOM describes the darwin archives
      # would be a statement nobody verified.
      - name: Attest archive SBOM
        uses: actions/attest-sbom@v2
        with:
          subject-path: dist/kubeagent_${{ steps.ver.outputs.version }}_linux_amd64.tar.gz
          sbom-path: dist/kubeagent_${{ steps.ver.outputs.version }}_sbom.spdx.json
```

- [ ] **Step 5: Attach the new assets to the Release**

In the `Publish GitHub Release` step's `files:` list, add two lines after `dist/SHA256SUMS`:

```yaml
            dist/SHA256SUMS.cosign.bundle
            dist/kubeagent_${{ steps.ver.outputs.version }}_sbom.spdx.json
```

- [ ] **Step 6: Capture the image digest**

In the `Build + push image` step from Task 2, add `id: image` and capture the digest right after pushing the version tag — before the `:latest` tag exists, so `RepoDigests` holds exactly one entry:

```yaml
      - name: Build + push image
        id: image
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        env:
          VERSION: ${{ steps.ver.outputs.version }}
          PUSH_LATEST: ${{ steps.rel.outputs.push_latest }}
        run: |
          docker build -t "imantaba/kubeagent:${VERSION}" --build-arg VERSION="${VERSION}" .
          docker push "imantaba/kubeagent:${VERSION}"
          # Sign and attest by digest, never by tag: a tag is a moving
          # pointer, and a signature over a moving pointer says nothing about
          # what a later pull receives.
          digest="$(docker image inspect --format '{{ index .RepoDigests 0 }}' "imantaba/kubeagent:${VERSION}" | cut -d@ -f2)"
          echo "digest=${digest}" >> "$GITHUB_OUTPUT"
          if [ "$PUSH_LATEST" = "true" ]; then
            docker tag "imantaba/kubeagent:${VERSION}" imantaba/kubeagent:latest
            docker push imantaba/kubeagent:latest
          else
            echo "pre-release ${VERSION}: leaving :latest pointing where it is"
          fi
```

- [ ] **Step 7: Sign and attest the image**

Append these four steps at the end of the job. Each keeps the existing
`DOCKERHUB_TOKEN` guard, so a fork without secrets still produces a complete,
signed GitHub Release:

```yaml
      - name: Sign image
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        run: cosign sign --yes "imantaba/kubeagent@${{ steps.image.outputs.digest }}"

      # push-to-registry: false — attestations live in GitHub's attestation
      # store, keyed by digest, and `gh attestation verify oci://…` resolves
      # them from there. Pushing them to the registry instead would depend on
      # Docker Hub's OCI referrers support, which is not something to stake a
      # release on. The image signature is the one registry-side artifact:
      # cosign's tag-based signature layout has worked on Docker Hub for years.
      - name: Attest image provenance
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        uses: actions/attest-build-provenance@v2
        with:
          subject-name: docker.io/imantaba/kubeagent
          subject-digest: ${{ steps.image.outputs.digest }}
          push-to-registry: false

      # The image SBOM is generated separately from the binary's: it also
      # covers the distroless base layer.
      - name: Generate image SBOM
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        env:
          VERSION: ${{ steps.ver.outputs.version }}
        run: syft scan "docker:imantaba/kubeagent:${VERSION}" -o spdx-json=image-sbom.spdx.json

      - name: Attest image SBOM
        if: ${{ env.DOCKERHUB_TOKEN != '' }}
        uses: actions/attest-sbom@v2
        with:
          subject-name: docker.io/imantaba/kubeagent
          subject-digest: ${{ steps.image.outputs.digest }}
          sbom-path: image-sbom.spdx.json
          push-to-registry: false
```

- [ ] **Step 8: Check the workflow parses and the step graph is sane**

```bash
python3 -c "import yaml; d=yaml.safe_load(open('.github/workflows/release.yml')); print('\n'.join(s.get('name', s.get('uses','')) for s in d['jobs']['release']['steps']))"
```

Expected: the step list prints in this order — checkout, setup-go, cosign-installer, download-syft, Resolve version, Classify release, Test, Build release archives, Render krew manifest, Sign checksums, Generate SBOM, Attest archive provenance, Attest archive SBOM, Publish GitHub Release, Log in to Docker Hub, Build + push image, Sign image, Attest image provenance, Generate image SBOM, Attest image SBOM.

- [ ] **Step 9: Run the whole suite**

```bash
go test -p 2 ./...
```

Expected: all packages PASS (this task changes no Go code; the run confirms nothing regressed).

- [ ] **Step 10: Commit**

```bash
git add .github/workflows/release.yml
git commit -s -m "ci: sign releases and publish an SBOM and build provenance

Checksums and a container image published by the same job that computed them
are self-asserted: a compromised release run rewrites both and nothing detects
it. Releases now carry a keyless cosign signature over SHA256SUMS, an SPDX
SBOM of the linux/amd64 binary, and SLSA build provenance binding the archives
and the image to this repository, commit and workflow run.

Signing is keyless, so no private key exists to guard or rotate: verification
pins the workflow identity through Fulcio and Rekor. The image is signed and
attested by digest rather than by tag, because a signature over a moving
pointer says nothing about what a later pull receives."
```

---

### Task 4: Documentation

**Files:**
- Create: `website/docs/verify.md`
- Modify: `website/mkdocs.yml` (nav)
- Modify: `website/docs/install.md`
- Modify: `SECURITY.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:**
- Consumes: the asset names Task 3 produces — `SHA256SUMS.cosign.bundle`, `kubeagent_<version>_sbom.spdx.json` — and the knobs Task 1 produces (`SOURCE_DATE_EPOCH`, `RELEASE_PLATFORMS`).
- Produces: nothing later tasks consume.

- [ ] **Step 1: Write the verification page**

Create `website/docs/verify.md`:

````markdown
# Verifying a release

Every kubeagent release is signed, ships a bill of materials, and can be
rebuilt from source to the same bytes. Nothing below needs an account, and
there is no public key to fetch: verification pins the **identity of the
workflow** that published the release, and that identity is public.

Set the release you downloaded:

```bash
VERSION=v1.2.3
base="https://github.com/imantaba/kubeagent/releases/download/${VERSION}"
```

## 1. Checksums

`SHA256SUMS` lists every archive in the release by hash.

```bash
curl -sSLO "${base}/kubeagent_${VERSION}_linux_amd64.tar.gz"
curl -sSLO "${base}/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
```

On its own this proves only that the archive matches the release page. The
next step is what makes the release page itself checkable.

## 2. Signature

kubeagent signs `SHA256SUMS`. Because that file already binds every archive by
hash, one signature covers the whole release. You need
[cosign](https://docs.sigstore.dev/cosign/installation/).

```bash
curl -sSLO "${base}/SHA256SUMS.cosign.bundle"

cosign verify-blob SHA256SUMS \
  --bundle SHA256SUMS.cosign.bundle \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/imantaba/kubeagent/.github/workflows/release.yml@refs/tags/${VERSION}"
```

Expected output: `Verified OK`.

The container image is signed the same way, by digest:

```bash
cosign verify "imantaba/kubeagent:${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "https://github.com/imantaba/kubeagent/.github/workflows/release.yml@refs/tags/${VERSION}"
```

!!! info "Why there is no key to download"
    Signing is keyless. The release workflow exchanges its OIDC token for a
    short-lived certificate and the signature is recorded in Rekor, a public
    transparency log. What you pin is the repository, the workflow file and
    the tag — a stolen key can sign anything from anywhere, while an identity
    that exists only inside a run of this repository's release workflow cannot
    be carried away.

## 3. Provenance and SBOM

Build provenance states which source, commit and workflow run produced the
bytes. It is verified with the [GitHub CLI](https://cli.github.com/):

```bash
gh attestation verify "kubeagent_${VERSION}_linux_amd64.tar.gz" --repo imantaba/kubeagent
gh attestation verify "oci://docker.io/imantaba/kubeagent:${VERSION}" --repo imantaba/kubeagent
```

The release also carries `kubeagent_${VERSION}_sbom.spdx.json`, an SPDX JSON
bill of materials for the linux/amd64 binary: every Go module linked into it,
at the version it was built from. It answers "is this release affected by a
vulnerability in dependency X" without reading the source tree. The container
image carries its own SBOM as an attestation, because it additionally contains
the distroless base layer.

```bash
curl -sSLO "${base}/kubeagent_${VERSION}_sbom.spdx.json"
```

## 4. Reproducing the build

Release archives are byte-reproducible: build the tag yourself and you get the
same checksums as the release page.

```bash
git clone https://github.com/imantaba/kubeagent
cd kubeagent
git checkout "${VERSION}"
scripts/build-release-archives.sh "${VERSION}" dist
diff <(sort dist/SHA256SUMS) <(curl -sSL "${base}/SHA256SUMS" | sort)
```

No output means the bytes match.

Two preconditions must hold, or the checksums differ for reasons that are not
tampering:

- **The same Go toolchain.** Patch releases of Go change code generation. The
  downloaded binary records the version it was built with — `go version
  ./kubeagent` — and building with a different one produces different bytes.
- **A clean checkout of the tag.** Go stamps the VCS revision and a
  dirty-tree flag into every binary, so uncommitted edits change the output.

The archive timestamp comes from the tagged commit, not from your clock;
`SOURCE_DATE_EPOCH` overrides it if you need to.
````

- [ ] **Step 2: Add the page to the nav**

In `website/mkdocs.yml`, in the `nav:` block, add one line directly after `- Install: install.md`:

```yaml
  - Verifying a release: verify.md
```

- [ ] **Step 3: Link it from the install page**

In `website/docs/install.md`, after the `!!! tip "Latest release"` block that follows the prebuilt-binary snippet, add:

```markdown
!!! tip "Verify more than the checksum"
    Releases are signed, carry an SBOM and build provenance, and are
    byte-reproducible — see [Verifying a release](verify.md).
```

- [ ] **Step 4: Link it from the security policy**

In `SECURITY.md`, insert a new section between `## Supported versions` and
`## What counts as a vulnerability here`:

```markdown
## Verifying a release

Release archives are signed with [cosign](https://docs.sigstore.dev/) keyless
signing, ship an SPDX SBOM, carry SLSA build provenance, and are
byte-reproducible from the tagged source. There is no key to distribute:
verification pins the release workflow's identity. The commands are in
[Verifying a release](https://k8sproject.top/verify/).
```

- [ ] **Step 5: Update the README**

In `README.md`, in the `### Prebuilt binary` section, after the code block and before the "Windows is not published" paragraph, add:

```markdown
Releases are signed, ship an SPDX SBOM and SLSA build provenance, and are
byte-reproducible — see [Verifying a release](https://k8sproject.top/verify/).
```

In the `### Cutting a release` section, replace the paragraph beginning "The release workflow runs the tests, builds the four platform archives" with:

```markdown
The release workflow runs the tests, builds the four platform archives
(`kubeagent_<version>_{linux,darwin}_{amd64,arm64}.tar.gz`) plus `SHA256SUMS`,
signs the checksums with keyless cosign, generates an SPDX SBOM, attests build
provenance for the archives and the image, renders the krew plugin manifest
from `krew/kubeagent.yaml.tmpl` with those checksums, and attaches everything
to the GitHub Release. A SemVer pre-release tag (`v1.2.3-rc.1`) publishes as a
GitHub pre-release and does not move the `:latest` image tag. Every push and PR
is checked by the CI workflow (vet + test + build).
```

- [ ] **Step 6: Add the changelog entry**

In `CHANGELOG.md`, under `## [Unreleased]` in the existing `### Added` section, add at the top of that section's list:

```markdown
- **Signed releases, SBOM and build provenance.** Every release now carries a
  keyless [cosign](https://docs.sigstore.dev/) signature over `SHA256SUMS` —
  which binds every archive by hash — plus a signed container image, an SPDX
  SBOM of the linux/amd64 binary, and SLSA build provenance for the archives
  and the image. Verification pins the release workflow's identity through
  Fulcio and Rekor, so there is no key to distribute or rotate. New page:
  [Verifying a release](https://k8sproject.top/verify/).
- **Byte-reproducible release archives.** The same tag rebuilt on another
  machine now produces the same `SHA256SUMS`: the Go build is trimmed of
  absolute paths, tar records a fixed mtime with numeric zero ownership and
  bytewise entry order, and gzip no longer stamps its header.
```

Then, in the `### Changed` section of `[Unreleased]`, add:

```markdown
- **Pre-release tags no longer move `imantaba/kubeagent:latest`.** A SemVer
  pre-release (`v1.2.3-rc.1`) publishes as a GitHub pre-release and pushes only
  its own image tag, so an unpinned `docker pull` keeps resolving to the newest
  stable release. A tag that is not a SemVer release version now stops the
  release workflow instead of producing a release under a malformed name.
```

- [ ] **Step 7: Update the roadmap**

In `website/docs/roadmap.md`, replace the Theme H bullet:

```markdown
- **H · Supply-chain & trust** — signed releases, SBOM and build provenance
  (shipped: keyless cosign signatures, an SPDX SBOM, SLSA build provenance and
  byte-reproducible archives — see [Verifying a release](verify.md));
  least-privilege RBAC profiles per feature, and fuzzed detectors remain ahead.
```

In the `## Shipped` list, append a bullet at the end:

```markdown
- **Verifiable releases** — keyless cosign signatures over `SHA256SUMS` and the
  container image, an SPDX SBOM, SLSA build provenance, and byte-reproducible
  archives, all checkable without a key — see [Verifying a release](verify.md)
```

- [ ] **Step 8: Build the docs strictly**

```bash
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: `Documentation built in …`, exit 0, and no `WARNING` line naming
`verify.md` or a broken link. The red "Material for MkDocs 2.0" banner is
cosmetic.

- [ ] **Step 9: Run the whole suite**

```bash
go test -p 2 ./...
```

Expected: all packages PASS. In particular `internal/report`'s golden test must
still pass — this task changes no report output, so a failure there means
something unrelated was touched.

- [ ] **Step 10: Commit**

```bash
git add website/docs/verify.md website/mkdocs.yml website/docs/install.md website/docs/roadmap.md SECURITY.md README.md CHANGELOG.md
git commit -s -m "docs: document how to verify a release

A signature nobody knows how to check is decoration. The new page walks the
four checks in the order a verifier performs them — checksums, signature,
provenance and SBOM, then reproducing the build from source — with the exact
certificate identity to pin and the two preconditions (same Go toolchain,
clean checkout of the tag) that make a mismatch mean tampering rather than
environment drift."
```

---

## Verification gate (controller, after Task 4)

Not a chaos gate: this slice touches no `internal/collect`, no `internal/cluster`,
no RBAC, no `nodes/proxy`, no `--fix`, no watch daemon and no Helm template.

1. `go build ./... && go test -p 2 ./...`, then `scripts/build-release-archives.sh v0.0.0-local dist-check` and inspect the archives by hand (`tar tvf` shows `0/0` ownership and the fixed timestamp).
2. `scripts/dco-check.sh main` — every commit on the branch signed off.
3. Whole-branch review on opus; fix Critical/Important.
4. Merge, then push `v0.68.0-rc.1`. The full workflow runs.
5. Verify against the published pre-release: `cosign verify-blob` on `SHA256SUMS`, `gh attestation verify` on an archive, `cosign verify` on the image, `gh attestation verify oci://…`, and confirm the SBOM lists the expected modules.
6. Confirm `imantaba/kubeagent:latest` still resolves to v0.67.0 — the guard is why the dry run is safe.
7. Delete the tag and the pre-release, then cut the real v0.68.0.
