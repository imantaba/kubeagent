# `kubectl` krew plugin — design

**Theme G slice 2.** Ships kubeagent as a `kubectl` plugin installable through
[krew](https://krew.sigs.k8s.io), so `kubectl kubeagent scan` works anywhere
`kubectl` does.

Slice 1 (the MCP server, `kubeagent mcp`, shipped in v0.63.0) put kubeagent
where *agents* work. This slice puts it where *people* work: the shell prompt
they already have open, under the command they already type.

## Goal

One sentence: package the existing binary for krew, without changing what the
binary does.

Nothing about the diagnosis, the detectors, the output, or the read-only
guarantee changes. The whole slice is distribution plus one cosmetic honesty
fix — a CLI invoked as `kubectl kubeagent` must not print usage text telling
the user to run `kubeagent`, a command that is not on their `PATH`.

## Architecture

Four moving parts, each independently testable:

```
release.yml ──▶ scripts/build-release-archives.sh
                          │
                          ├──build 4 platforms──▶  kubeagent_${VER}_${OS}_${ARCH}.tar.gz  ×4
                          │                                   │
                          │                              sha256 ×4
                          ▼                                   ▼
scripts/render-krew-manifest.sh  ◀── krew/kubeagent.yaml.tmpl
     │
     ▼
kubeagent.yaml  ──attached to the GitHub Release──▶  kubectl krew install --manifest-url …
                                                              │
                                                              ▼
                                              ~/.krew/bin/kubectl-kubeagent  (symlink)
                                                              │
                                                              ▼
                                              main.go: invocationName(os.Args[0])
```

The binary is unchanged except for `invocationName`. The manifest is generated,
never committed. The renderer is a shell script that CI and the Go test both
call — one implementation, no reimplementation to drift against.

---

## 1 · Distribution: in-repo manifest first

`kubectl krew install --manifest-url=<url>` installs a plugin from a manifest
served anywhere, with no index involved. The manifest is attached to each
GitHub Release, so `releases/latest/download/kubeagent.yaml` always names the
current version.

The upstream `kubernetes-sigs/krew-index` PR is **deliberately out of scope**.
It is a public pull request to a third-party repository, reviewed by people
outside this project on their own schedule, and it requires a published release
whose manifest is already known-good. Proving the manifest against a real
release is exactly what this slice does; the index submission is a separate,
explicitly authorized step afterwards.

Consequence for users during this slice: the install line is

```bash
kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml
```

not `kubectl krew install kubeagent`. The docs say so plainly rather than
implying index membership that does not exist.

## 2 · Release pipeline: one platform becomes four

`.github/workflows/release.yml` builds `linux/amd64` today. It grows to a
four-way matrix:

| OS | Arch |
|----|------|
| linux | amd64 |
| linux | arm64 |
| darwin | amd64 |
| darwin | arm64 |

All four are `CGO_ENABLED=0` with the same `-ldflags "-X main.version=$VERSION"`
stamp. kubeagent is pure Go with no cgo and no OS-specific syscalls, so
cross-compilation is a build-tag-free `GOOS`/`GOARCH` change.

**Windows is excluded on purpose.** krew supports it, but no test, chaos
scenario, or smoke run in this project has ever executed on Windows. Shipping a
binary for a platform nobody has run is a claim the project cannot back, and it
would appear in the manifest as a supported platform — a false positive of
exactly the kind the coverage work in slice 1 exists to prevent.

Archive naming: `kubeagent_${VERSION}_${OS}_${ARCH}.tar.gz`, each containing
the binary, `README.md`, and `LICENSE`. `SHA256SUMS` grows from one line to
four (five including the unversioned copy).

**The build lives in `scripts/build-release-archives.sh`, not inline in the
workflow YAML.** It takes a version and an output directory, produces the four
archives plus the unversioned copy and `SHA256SUMS`, and prints nothing the
caller has to parse. `release.yml` calls it; so does the smoke gate in §7. A
build step written inline in the workflow can only be exercised by pushing a
tag — the one place where being wrong is most expensive. A script can be run
locally before the tag exists, which is exactly what the gate does.

**The unversioned `kubeagent_linux_amd64.tar.gz` asset stays.** The README
quick-install resolves
`releases/latest/download/kubeagent_linux_amd64.tar.gz`; that URL is in the
wild, in people's notes and scripts. Removing it to tidy the asset list would
break every copy of that install line silently.

## 3 · The manifest is generated, never committed

`krew/kubeagent.yaml.tmpl` is a plain-text template with named placeholders.
`scripts/render-krew-manifest.sh VERSION SHA256SUMS_FILE` writes the rendered
manifest to **stdout** and substitutes:

- the version, once, into `spec.version`;
- the four archive URIs;
- the four sha256 checksums, read by archive filename from `SHA256SUMS_FILE`
  (never by line order — the two scripts must agree on names, not positions).

Its checksum inputs come from the `SHA256SUMS` that
`scripts/build-release-archives.sh` just wrote, so the bytes hashed are the
bytes uploaded. CI runs the renderer after packaging and attaches the rendered
`kubeagent.yaml` to the Release.

The alternative — a committed manifest with checksums written by
`scripts/bump-version.sh` — cannot work: the bump runs *before* the tag, and the
tag is what triggers the build that produces the archives. Any checksum written
at bump time is a guess about bytes that do not exist yet. Generating the
manifest in the same job that computed the checksums makes a stale checksum
structurally impossible.

**Testing the renderer, not a copy of it.** The Go test executes
`scripts/render-krew-manifest.sh` itself via `os/exec`, in a temp directory,
with fixture version and checksum values, then asserts on the output. A test
that reimplemented the substitutions in Go would keep passing while the real
script rotted — the script is what CI runs, so the script is what the test must
run.

Assertions on the rendered YAML:

- it parses as YAML;
- `apiVersion` is `krew.googlecontainertools.github.com/v1alpha2` and `kind` is
  `Plugin`;
- `metadata.name` is `kubeagent`, **without** a `kubectl-` prefix. krew's
  validator would accept `kubectl-kubeagent` — `safePluginRegexp` is
  `^[\w-]+$`, which that name matches — so nothing stops the mistake at
  install time. What stops it is `pluginNameToBin`, which prefixes the plugin
  name with `kubectl-` when creating the symlink: a plugin named
  `kubectl-kubeagent` installs as `kubectl-kubectl-kubeagent` and is invoked
  as `kubectl kubectl-kubeagent`. The test asserts the name because the
  failure is silent and downstream, not because krew rejects it;
- exactly four `spec.platforms` entries, with `matchLabels` covering each
  `{os, arch}` pair once;
- every `uri` carries the fixture version and matches its platform's archive
  name;
- every `sha256` is the fixture checksum for that platform — the test uses four
  *distinct* fixture checksums, so a renderer that pasted one checksum into all
  four slots fails;
- `bin` is `kubeagent` for all four;
- no placeholder token survives anywhere in the output.

That last two matter most. A manifest with a wrong checksum fails at install
time with an opaque verification error, which is precisely the failure krew's
checksums exist to catch — the test must not be able to pass on a manifest that
would fail for a user.

## 4 · `invocationName`: telling the user the truth about their own command

krew symlinks the binary as `~/.krew/bin/kubectl-kubeagent` (confirmed against
krew's `pluginNameToBin`, which prefixes the plugin name with `kubectl-`).
`kubectl` finds it on `PATH` and executes it, so `os.Args[0]`'s basename is
`kubectl-kubeagent`. Go does not resolve the symlink, so the name the user
typed survives into the process.

```go
// invocationName returns how the user invoked this process, for use in usage
// and error text: "kubectl kubeagent" when running as a kubectl plugin,
// "kubeagent" otherwise.
func invocationName(argv0 string) string
```

A pure function of one string. It reads `filepath.Base(argv0)` and returns
`"kubectl kubeagent"` for exactly `kubectl-kubeagent`, `"kubeagent"` for
everything else.

Table-driven test cases:

| `argv0` | expected |
|---------|----------|
| `/home/u/.krew/bin/kubectl-kubeagent` | `kubectl kubeagent` |
| `kubectl-kubeagent` | `kubectl kubeagent` |
| `./kubeagent` | `kubeagent` |
| `/usr/local/bin/kubeagent` | `kubeagent` |
| `/opt/kubectl-kubeagent/kubeagent` | `kubeagent` |
| `` (empty) | `kubeagent` |
| `kubectl-kubeagent-extra` | `kubeagent` |

The fifth case is the one worth writing down: `kubectl-kubeagent` appearing as a
*directory* component must not match. A naive `strings.Contains` passes every
other row and fails this one.

Call sites: the `usage:` error in `run`, and the `kubeagent:` prefix in `main`'s
`Fprintln(os.Stderr, …)`. The usage string is one long literal today; it gains
the invocation name by substitution rather than being rewritten.

`kubeagent version` keeps printing `kubeagent vX.Y.Z` unchanged. Version names
the software; usage names the command you type. They are different questions
and only one of them depends on how the process was launched.

## 5 · kubectl flag ordering

`kubectl` does not forward its own global flags to plugins. Flags belong after
the plugin name:

```bash
kubectl kubeagent scan --context prod-eu       # works
kubectl --context prod-eu kubeagent scan       # does not
```

This costs nothing in practice: kubeagent's `--context` and `--kubeconfig` are
spelled exactly like kubectl's, and `KUBECONFIG` is inherited from the
environment the same way it always was, so the habit transfers intact.

Stated in the manifest's `caveats:` field — which krew prints after a
successful install, the moment it is relevant — and in the docs.

The manifest `caveats:` also restates the read-only default: `kubectl kubeagent
scan` performs only `get`/`list` calls, and `--fix` remains the single opt-in
write path with its own confirmation. Someone installing a plugin into their
`kubectl` deserves to read that at install time, not to go looking for it.

## 6 · Error handling

Nothing new can fail at runtime — the binary is unchanged. The failure modes
this slice introduces all live in the release pipeline:

- **A build fails for one platform.** The job fails as a whole; no partial
  release is published, because the manifest and the archives are attached in
  the same step. A manifest referencing an archive that does not exist would be
  worse than no manifest.
- **A checksum is wrong.** Structurally excluded — computed and consumed inside
  one job — and the renderer test's four distinct fixture checksums catch a
  renderer that mixes them up.
- **The rendered manifest is malformed.** Caught by the Go test before it can
  reach a release, and again by the smoke gate, which installs from it for real.

## 7 · Testing and the release gate

**Unit:** `invocationName` (table above) and the renderer script driven through
`os/exec` (assertions above). Both run in `go test ./...`.

`scripts/build-release-archives.sh` gets **no** unit test: it is four
cross-compiles and a `tar`, and a test that asserted on its output would be
asserting that `go build` and `tar` work. Its gate is the smoke run below,
which builds with it for real and then installs what it produced. Saying that
here rather than leaving the omission unexplained — an untested script in a
release pipeline is a choice, not an oversight.

**Gate:** this slice changes no cluster-interaction code — no `internal/collect`,
no `internal/cluster`, no RBAC, no `--fix`, no watch daemon, no Helm templates —
so the full chaos suite is not the right gate. It changes packaging, which the
chaos suite does not exercise at all.

The gate is a real end-to-end krew install:

```bash
kind create cluster --name kubeagent-smoke --wait 90s

# the same script CI runs, into a scratch dir — four archives + SHA256SUMS
scripts/build-release-archives.sh v0.0.0-smoke "$OUT"
scripts/render-krew-manifest.sh v0.0.0-smoke "$OUT/SHA256SUMS" > "$OUT/kubeagent.yaml"

kubectl krew install --manifest="$OUT/kubeagent.yaml" \
  --archive="$OUT/kubeagent_v0.0.0-smoke_linux_amd64.tar.gz"
kubectl kubeagent version                              # "kubeagent v0.0.0-smoke"
kubectl kubeagent scan --context kind-kubeagent-smoke  # renders a real report
kubectl kubeagent                                      # usage says "kubectl kubeagent scan …"
kind delete cluster --name kubeagent-smoke
```

`--archive` skips the download but **not** the checksum: krew still verifies the
local file against the manifest's `sha256`. Because the manifest was rendered
from the `SHA256SUMS` the build script wrote, the smoke proves the two scripts
agree on the same bytes — the failure mode §6 calls "structurally excluded" is
here checked rather than asserted.

The `kubectl kubeagent` line is the point of the whole `invocationName` change
and is the one assertion that cannot be made from a unit test: it needs the
binary to have been launched through krew's symlink by kubectl itself.

## 8 · Documentation surfaces

Every surface that tells someone how to install kubeagent must learn about the
plugin, or the docs will disagree with each other:

- `README.md` — install section gains the krew line beside the tarball line.
- `website/docs/install.md` — same, with the platform table.
- `website/docs/quickstart.md` — the first command a new user runs.
- `CHANGELOG.md` — under `[Unreleased]`.
- `website/docs/roadmap.md` — Theme G: mark the krew plugin shipped, leaving the
  CI/CD gate mode, the TUI and the HTML report ahead.
- `CLAUDE.md` — the build/run section, which currently documents only
  `./kubeagent scan`.

`deploy/README.md` is deliberately untouched: it covers the in-cluster watch
daemon, which krew has nothing to do with.

## Out of scope

- The upstream `krew-index` PR (see §1).
- Windows binaries (see §2).
- Any change to detectors, output formats, or the JSON schema.
- Cobra. krew requires nothing from the CLI framework, and the v1 stdlib-`flag`
  constraint holds.

## Constraints (unchanged, binding)

- Read-only toward the cluster by default; `--fix` is the only write path and is
  never LLM-decided.
- Standard-library `flag` only — no Cobra.
- The scan CLI stays sequential; `internal/watch` and `internal/mcp` remain the
  documented long-lived exceptions.
- Endpoint URLs and kubeconfig paths are credentials: no log line, error string,
  manifest field, or documentation example carries more than `scheme://host`,
  and no example names a real cluster, host, or path.
- `internal/report/testdata/golden-scan.txt` stays byte-identical — this slice
  changes no report output.
- No `Co-Authored-By` trailer on any commit.
