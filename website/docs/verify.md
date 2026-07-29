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

!!! info "If the release was cut by manual dispatch"
    The identity above pins `@refs/tags/${VERSION}`, which is what a tag push
    produces: the certificate is issued for the workflow run, and by the time
    it runs the pushed tag already exists. A release cut by manually
    dispatching the workflow from the Actions tab is signed under the branch
    it was dispatched from instead — `@refs/heads/<branch>` — because that tag
    doesn't exist yet at the moment the certificate is issued; the release
    step creates it afterwards. Pinning the exact tag identity against such a
    release fails with a certificate-identity mismatch even though the
    artifact is genuine. If that's the release you're checking, verify with a
    regexp identity that accepts either form:

    ```bash
    cosign verify-blob SHA256SUMS \
      --bundle SHA256SUMS.cosign.bundle \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      --certificate-identity-regexp "^https://github\.com/imantaba/kubeagent/\.github/workflows/release\.yml@refs/(tags|heads)/"
    ```

    Prefer the exact `--certificate-identity` form above when you know the
    release came from a pushed tag: it's the stronger claim, since it pins the
    one ref the release is supposed to be.

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

Those two commands check the build provenance only: `gh attestation verify`
enforces the SLSA provenance predicate unless you name another one.

The release also carries `kubeagent_${VERSION}_sbom.spdx.json`, an SPDX JSON
bill of materials for the linux/amd64 binary: every Go module linked into it,
at the version it was built from. It answers "is this release affected by a
vulnerability in dependency X" without reading the source tree. The container
image carries its own SBOM as an attestation, because it additionally contains
the distroless base layer.

The SBOM is a separate attestation with its own predicate type, so it needs
its own command:

```bash
gh attestation verify "kubeagent_${VERSION}_linux_amd64.tar.gz" \
  --repo imantaba/kubeagent \
  --predicate-type https://spdx.dev/Document
gh attestation verify "oci://docker.io/imantaba/kubeagent:${VERSION}" \
  --repo imantaba/kubeagent \
  --predicate-type https://spdx.dev/Document
```

Adding `--format json` prints the statement, whose `predicate` field is the
attested SBOM itself — that copy is the signed one.

The full predicate type carries the SPDX version — `https://spdx.dev/Document/v2.3`
at the time of writing. `gh` matches `--predicate-type` as a prefix, so the
shorter form above keeps working when the SPDX version moves.

```bash
curl -sSLO "${base}/kubeagent_${VERSION}_sbom.spdx.json"
```

The downloadable `.spdx.json` asset is a convenience copy for tools that want
a plain file. `SHA256SUMS` covers the archives, not the SBOM, so an asset you
fetched that way carries no signature of its own — diff it against the
attested predicate if the file itself has to be trusted.

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

Three preconditions must hold, or the checksums differ for reasons that are
not tampering:

- **The same Go toolchain.** Patch releases of Go change code generation. The
  downloaded binary records the version it was built with — `go version
  ./kubeagent` — and building with a different one produces different bytes.
- **A clean checkout of the tag.** Go stamps the VCS revision and a
  dirty-tree flag into every binary, so uncommitted edits change the output.
- **GNU tar 1.28 or newer.** The build script's `--sort=name`,
  `--numeric-owner` and `--mtime` flags are GNU tar options; bsdtar, the
  default on macOS, does not accept them.

The archive timestamp comes from the tagged commit, not from your clock;
`SOURCE_DATE_EPOCH` overrides it if you need to.
