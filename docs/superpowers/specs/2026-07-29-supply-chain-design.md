# Supply-chain integrity for kubeagent releases — design

**Theme H, slice 1.** Signed releases, SBOM, build provenance, and byte-reproducible
archives. Branch `supply-chain`, cut off `main` at `ffd4a09`.

## The problem

A kubeagent release today is a set of tarballs, a `SHA256SUMS` file, a rendered krew
manifest, and a Docker Hub image. Everything in that list is *self-asserted*. The
checksums live next to the artifacts they describe, on the same page, published by the
same token — a compromised release job rewrites both and nothing detects it. There is
no statement of what source produced the bytes, no list of what is inside them, and no
way for anyone outside the release job to check either.

Three gaps, in the order a reader hits them:

1. **Nothing is signed.** `SHA256SUMS` proves the archive you downloaded is the archive
   the release page describes. It proves nothing about who produced either.
2. **Nothing declares its contents.** No SBOM, so "is kubeagent affected by CVE-X in
   dependency Y" has no answer short of reading `go.sum` for the right tag.
3. **Nothing is reproducible.** `tar -czf` records the mtime, uid and username of
   whoever ran the build, so rebuilding the same tag produces a different `SHA256SUMS`.
   A verifier who tries to confirm the bytes independently gets a mismatch and cannot
   tell tampering from timestamps. This was raised as a Minor during the krew-plugin
   review and deferred to this theme.

## Goals

- Every release artifact carries a signature that verifies against the workflow
  identity that produced it — no long-lived key anywhere.
- Every release publishes an SBOM.
- Every release carries SLSA build provenance binding the artifacts to this repository,
  this commit and this workflow run.
- The same tag rebuilt on a different machine produces byte-identical archives.
- One documented page tells a user how to check all of the above.

## Non-goals

- SLSA build L3. Reaching it means handing the build to
  `slsa-framework/slsa-github-generator`'s reusable workflow, which would displace
  `scripts/build-release-archives.sh` as the thing CI actually runs. L2 via GitHub's
  native attestation keeps one build path, tested locally and in CI.
- Signing anything a user does not download: intermediate CI artifacts, the rendered
  krew manifest (whose checksums are already covered by the signed `SHA256SUMS`).
- The rest of Theme H — per-feature least-privilege RBAC, fuzzed detectors — which
  touch `internal/collect` and the Helm templates and belong in their own slices with
  their own gates.
- Any change to what kubeagent *does*. No Go code under `internal/` is touched. The
  read-only invariant, the `--fix` guard rails and `internal/report/testdata/golden-scan.txt`
  are untouched by construction.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Key material | **Keyless** (GitHub OIDC → Fulcio → Rekor) | No private key exists to leak, guard or rotate. Verifiers pin the workflow identity, which is stronger than pinning a key: a stolen key signs anything, a stolen identity does not exist. |
| Signing surface | `SHA256SUMS` + the container image | `SHA256SUMS` already binds all five archives by hash, so one blob signature covers them all. Signing each archive separately doubles the asset list to say the same thing. |
| SBOM | **syft → SPDX JSON**, for the linux/amd64 binary and for the image | SPDX is the format named first by CNCF and NTIA guidance. Scanning the *binary* reports the modules actually linked, not everything in `go.sum`. The image adds the distroless base layer, which a Go-native tool cannot see. |
| Provenance | **`actions/attest-build-provenance`** (SLSA v1, build L2) | Two steps in the existing job. Same Fulcio/Rekor keyless path as the signatures, verified with `gh attestation verify` or `cosign verify-attestation`. |

## Component 1 — reproducible archives

`scripts/build-release-archives.sh` is the single build path: the release workflow runs
it and so does the local krew smoke gate. It gains determinism at three layers.

**Go build.** Add `-trimpath`, which drops the builder's absolute paths out of the
binary. The `-ldflags "-X main.version=…"` stamp stays as-is.

**tar.** Replace `tar -czf` with an explicit, deterministic invocation:

```bash
tar --sort=name --numeric-owner --owner=0 --group=0 \
    --mtime="@${SOURCE_DATE_EPOCH}" \
    -C "$stage" -cf - kubeagent README.md LICENSE NOTICE | gzip -n > "$out"
```

Each flag closes one leak: `--sort=name` fixes entry order, `--owner=0 --group=0
--numeric-owner` removes the building user's uid and name, `--mtime` removes the
staging directory's creation time. `gzip -n` is separate on purpose — the gzip header
carries its own filename and timestamp fields, which `tar -czf` does not let you
control.

`--sort=name` requires GNU tar 1.28 or newer. That is what `ubuntu-latest` and the
project's own Linux development box run; bsdtar (a macOS default) does not accept these
flags. The script already targets Linux — it cross-compiles the darwin binaries rather
than building on a Mac — so this narrows nothing that was previously supported, and the
script fails loudly rather than producing a non-reproducible archive.

**`SOURCE_DATE_EPOCH`.** Honoured from the environment if set; otherwise the HEAD
commit time (`git log -1 --pretty=%ct`). If neither is available the script dies with a
usable message rather than silently substituting "now" — a fallback that quietly
defeats the whole component is worse than a failure.

**`NOTICE` joins the tarball.** The project relicensed to Apache-2.0 with a `NOTICE`
file (`dfbf10d`), and §4(d) of that license requires NOTICE contents to travel with
redistributions of the work. The tarball layout is being rewritten anyway; shipping the
binary without its NOTICE is a compliance gap a CNCF review would find. `krew/kubeagent.yaml.tmpl`
gains a matching `files:` entry for all four platforms, so a krew install lands it too.

**`RELEASE_PLATFORMS`.** A new environment override, defaulting to the current four
(`linux/amd64 linux/arm64 darwin/amd64 darwin/arm64`). The reproducibility test sets it
to two platforms so a double build stays fast; the default is what every real caller
gets, so the knob cannot silently shrink a release.

## Component 2 — release classification and the `:latest` guard

Today any tag matching `v*` repoints `imantaba/kubeagent:latest`. That makes a
pre-release tag actively dangerous: pushing `v0.68.0-rc.1` to exercise the release
pipeline would hand every `docker pull imantaba/kubeagent` an untested release
candidate. Since the gate for this slice *is* a pre-release tag (see Verification), the
guard is a prerequisite, not a nicety.

New `scripts/release-vars.sh VERSION` writes GitHub-Actions output syntax to stdout:

```text
prerelease=false
push_latest=true
```

Rules:

- The version must match `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`.
  Anything else is a hard error — a malformed tag should stop the release, not produce
  a release with a malformed name.
- `prerelease=true` exactly when a SemVer pre-release part is present (a `-` after the
  numeric core). Build metadata (`+…`) alone is not a pre-release.
- `push_latest` is the negation of `prerelease`.

The workflow consumes it in one step (`scripts/release-vars.sh "$VERSION" >> "$GITHUB_OUTPUT"`),
passes `prerelease:` to `softprops/action-gh-release`, and guards the `:latest` tag and
push on `push_latest`. The versioned image tag is pushed either way — a pre-release
image is still worth having, it just must not be what `latest` resolves to.

## Component 3 — signing, SBOM and provenance in the release workflow

Job permissions grow from `contents: write` to also carry `id-token: write` (the OIDC
token Fulcio exchanges for a signing certificate) and `attestations: write` (the GitHub
attestation store).

Tooling is installed by pinned actions: `sigstore/cosign-installer` and
`anchore/sbom-action/download-syft`.

### Archives

```bash
cosign sign-blob --yes dist/SHA256SUMS --bundle dist/SHA256SUMS.cosign.bundle

tar xzf "dist/kubeagent_${VERSION}_linux_amd64.tar.gz" -C sbom kubeagent
syft scan "file:sbom/kubeagent" -o "spdx-json=dist/kubeagent_${VERSION}_sbom.spdx.json"
```

Then `actions/attest-build-provenance` over `dist/kubeagent_*.tar.gz`, and
`actions/attest-sbom` binding the SBOM to `kubeagent_${VERSION}_linux_amd64.tar.gz` —
that archive specifically, because that is the binary syft scanned. Claiming the SBOM
describes the darwin/arm64 archive would be a statement nobody verified.

Both new files are added to the Release asset list.

### Image

The image steps stay behind the existing `DOCKERHUB_TOKEN != ''` guard, so a fork
without secrets still produces a complete, signed GitHub Release. The push step
captures the digest into a step output; everything downstream signs and attests **by
digest**, never by tag, because a tag is a moving pointer and a signature over a moving
pointer is worthless.

```bash
cosign sign --yes "imantaba/kubeagent@${DIGEST}"
syft scan "docker:imantaba/kubeagent:${VERSION}" -o spdx-json=image-sbom.spdx.json
```

followed by `actions/attest-build-provenance` and `actions/attest-sbom` with
`subject-name: docker.io/imantaba/kubeagent` and `subject-digest: ${DIGEST}`.

**`push-to-registry: false`.** Attestations land in GitHub's attestation store, keyed by
digest, and `gh attestation verify oci://…` resolves them from there. Pushing them to
the registry instead would depend on Docker Hub's OCI referrers support, which is not
something this project should stake a release on. The image *signature* is the one
registry-side artifact, and cosign's tag-based signature layout has worked on Docker
Hub for years.

### What a release publishes, after this slice

| Artifact | Produced by | Verified with |
|---|---|---|
| 5 tarballs, `SHA256SUMS`, `kubeagent.yaml` | unchanged | `sha256sum -c` |
| `SHA256SUMS.cosign.bundle` | `cosign sign-blob` | `cosign verify-blob --bundle` |
| `kubeagent_<version>_sbom.spdx.json` | syft, on the linux/amd64 binary | any SPDX consumer |
| provenance for the 5 tarballs | `actions/attest-build-provenance` | `gh attestation verify` |
| SBOM attestation for the linux/amd64 tarball | `actions/attest-sbom` | `gh attestation verify` |
| image signature | `cosign sign` (by digest) | `cosign verify` |
| image provenance + SBOM attestation | `attest-*` (by digest) | `gh attestation verify oci://…` |

Verification pins issuer `https://token.actions.githubusercontent.com` and identity
`https://github.com/imantaba/kubeagent/.github/workflows/release.yml@refs/tags/<version>`.

**What goes public.** Keyless signing records the artifact digests and that workflow
identity in Rekor, a public transparency log. Both are already public facts about a
public release. No token, no path, no host beyond `github.com` enters the log — the
project's rule that URLs and kubeconfig paths are credentials is unaffected, because
nothing in this pipeline touches a cluster or a kubeconfig at all.

## Component 4 — documentation

New page `website/docs/verify.md` ("Verifying a release"), in the nav directly after
Install, with four sections in the order a verifier works:

1. **Checksums** — the existing `sha256sum --ignore-missing -c SHA256SUMS` flow.
2. **Signature** — `cosign verify-blob` with the exact `--certificate-identity` and
   `--certificate-oidc-issuer` values, and one sentence on why identity beats a key.
3. **Provenance and SBOM** — `gh attestation verify` for the archive and for
   `oci://docker.io/imantaba/kubeagent:<version>`; where the SBOM asset is and what it
   covers.
4. **Reproducing the build** — clone, check out the tag, run
   `scripts/build-release-archives.sh`, compare against the published `SHA256SUMS`.

Section 4 states its two honest preconditions: **the same Go toolchain** (patch releases
change code generation; `go version ./kubeagent` on the downloaded binary reports the
exact version the release used) and **a clean checkout of the tag** (Go stamps the VCS
revision and a dirty-tree flag into every binary). A verifier who hits a mismatch must
be able to tell which of these they tripped, rather than concluding tampering.

`install.md` and `SECURITY.md` link to the new page. `README.md` gains one line.
`CHANGELOG.md` gets an `[Unreleased]` entry. The roadmap's Theme H bullet records the
slice as shipped.

## Testing

Two test targets, both executing the real scripts — a Go reimplementation of a shell
script keeps passing while the script rots, which is exactly the failure mode
`krew_manifest_test.go` was written to avoid. Both live in the root `main` package
beside it.

**Reproducibility (`release_archives_test.go`).** Runs `scripts/build-release-archives.sh`
twice into two directories with a fixed `SOURCE_DATE_EPOCH` and
`RELEASE_PLATFORMS="linux/amd64 linux/arm64"`, then asserts:

- the two `SHA256SUMS` files are identical;
- opening a tarball with `archive/tar`, every entry has `Uid == 0`, `Gid == 0`, empty
  `Uname`/`Gname`, `ModTime` equal to `SOURCE_DATE_EPOCH`, and names in sorted order;
- the gzip header's MTIME field (bytes 4–7) is zero.

The metadata assertions carry the weight. Two builds on one machine agree even when the
tarball embeds `uid=1000 uname=ubuntu` — a same-machine double build alone would pass
while the archive stayed unreproducible for everyone else. The entry list also pins that
`NOTICE` ships.

**Classification (`release_vars_test.go`).** A table over version strings executing
`scripts/release-vars.sh`: `v1.2.3` → not a pre-release, pushes latest; `v1.2.3-rc.1`,
`v0.68.0-alpha.2`, `v1.0.0-0.beta` → pre-release, does not push latest; `v1.2.3+build.5`
→ build metadata is not a pre-release; `1.2.3` (no `v`), `v1.2`, `vX.Y.Z`, empty → hard
error, non-zero exit.

## Verification gate

Not a chaos gate. This slice touches no `internal/collect`, no `internal/cluster`, no
RBAC, no `nodes/proxy`, no `--fix`, no watch daemon and no Helm template; it does not
touch cluster-facing Go code at all. The gate matches the change, which here means
exercising the release pipeline itself:

1. `go build ./... && go test ./...`, plus `scripts/build-release-archives.sh` run
   locally and the archives inspected by hand.
2. Push `v0.68.0-rc.1`. The full workflow runs.
3. Verify against the published pre-release: `cosign verify-blob` on `SHA256SUMS`,
   `gh attestation verify` on an archive, `cosign verify` on the image digest,
   `gh attestation verify oci://…`, and confirm the SBOM lists the expected modules.
4. Confirm `imantaba/kubeagent:latest` still points at v0.67.0 — the guard is the whole
   reason the dry run is safe.
5. Delete the tag and the pre-release.

Only after that does the real version bump and release happen.

## Global constraints

- Every commit carries a `Signed-off-by` trailer matching its author (`git commit -s`).
  `main` enforces DCO on pull requests as of `e1e9659`; a branch that ignores it cannot
  be merged through the project's own process.
- No `Co-Authored-By: Claude` trailer, and no AI attribution in any commit message,
  document, comment or changelog entry.
- No secrets, credentials, private IPs or internal hostnames anywhere, including test
  fixtures. Documentation IPs are RFC 5737; example domains are RFC 2606. No internal,
  private or operator-specific host may appear; links to public project and tool
  documentation (`github.com`, `docker.io`, `token.actions.githubusercontent.com`, the
  project's own site, `docs.sigstore.dev`, `cli.github.com`, and the like) are fine.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- No Go code under `internal/` changes. Nothing in this slice reads a kubeconfig,
  contacts a cluster, or makes a model call.
