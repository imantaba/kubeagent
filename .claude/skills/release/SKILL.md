---
name: release
description: Cut a new kubeagent release — run tests + the chaos gate, bump every version reference, commit, push main, and tag so CI publishes the GitHub Release and Docker Hub image. Use when the user asks to "release", "cut a release", "ship vX.Y.Z", "publish a new version", or bump the version after merging a feature.
---

# Releasing kubeagent

kubeagent releases are **tag-driven**: pushing a `v*` tag triggers
`.github/workflows/release.yml`, which tests, builds the ldflags-stamped
`linux/amd64` binary, packages a tarball + `SHA256SUMS`, publishes a GitHub
Release, and (when Docker Hub secrets are set) builds and pushes
`imantaba/kubeagent:<version>` + `:latest`. Your job is everything up to and
including the tag; CI does the rest.

## Invariants (never break)

- **No `Co-Authored-By: Claude` trailer** on any commit (project rule).
- **Never expose API keys to the shell.** The chaos gate runs fine without
  `ANTHROPIC_API_KEY` (the `--explain` scenarios are simply skipped).
- Release only when the user asked. Cut from a **clean tree on `main`** with the
  feature already merged.
- Read-only features should have been **live-validated** before you get here.

## Version choice (SemVer)

- New user-facing feature/detector → **minor** (`0.11.0 → 0.12.0`).
- Bug fix / doc-only / packaging tweak → **patch** (`0.12.0 → 0.12.1`).

Set `VERSION=vX.Y.Z` (with the `v`) and `CHART_VERSION=X.Y` for the steps below.

## Step 1 — Preconditions

```bash
export PATH=$PATH:/usr/local/go/bin
cd <repo>
git branch --show-current      # must be main
git status --short             # must be empty
git log --oneline -3           # feature commits present
```

If not on `main` or the tree is dirty, stop and resolve first.

## Step 2 — Tests

```bash
go build ./... && go test ./...
```

All packages must pass. Do not proceed on any failure.

## Step 3 — Pre-release gate (match the gate to the change)

**Cluster-interaction changes** — anything touching RBAC, `nodes/proxy`, `--fix`,
`internal/collect`, `internal/cluster`, the watch daemon, or Helm templates — get the
**full chaos gate**. It spins up a disposable Kind cluster (Calico CNI), injects the
common outages, and runs `kubeagent scan` against each.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY          # keep keys out of the shell; --explain scenarios skip
./chaos/run.sh --recreate        # long-running; run in background and watch the log
```

Review the results report — every scenario should be green. The harness now **preloads
the Calico images into the Kind nodes** (`preload_calico_images`), which fixes the old
`calico-node exceeded its progress deadline` flake (slow serial in-node pulls). If it
still flakes on a cold cluster, re-run `./chaos/run.sh --recreate`. It tears its cluster
down on exit.

**Detector / report / docs-only changes** fully covered by unit tests + the fake
clientset (a new pure detector, an output-format tweak) don't need the full outage suite —
a **lightweight real-cluster smoke** confirms the change renders on live nodes:

```bash
kind create cluster --name kubeagent-smoke --wait 90s \
  --config <(printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n  - role: worker\n')
go build -o kubeagent . && ./kubeagent scan --context kind-kubeagent-smoke   # eyeball the new output
kind delete cluster --name kubeagent-smoke
```

When in doubt, run the full gate.

## Step 4 — Bump every version reference

Run the bump script — it rewrites the `CHANGELOG` (`[Unreleased]` → the release, a fresh
`[Unreleased]`, and both compare links), the deploy manifest image tag, the chart's
`appVersion` + a patch bump of the chart `version`, and the two docs, then fails if any
stale `v<old>` reference remains:

```bash
scripts/bump-version.sh $VERSION                          # e.g. vX.Y.Z
# RELEASE_DATE=YYYY-MM-DD scripts/bump-version.sh $VERSION  # to override the CHANGELOG date
```

It reads the previous version from the chart's `appVersion` (the single source of truth),
so nothing to pass but the new version.

**Chart version policy:** the script patch-bumps the chart `version` (correct when only
`appVersion` changed). If this release changed the chart's **templates or values**, edit
`deploy/helm/kubeagent/Chart.yaml` `version:` to a **minor** bump instead — the script
prints the exact value.

The script moves whatever sat under `## [Unreleased]` into the new version section, so
make sure your feature's changelog note is there (it should already be, from the feature
branch). Review `git diff`. The repeated `### Added`/`### Changed` MD024 lint warnings are
expected Keep-a-Changelog style — ignore them.

## Step 5 — Verify packaging + docs

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -m1 'image:'   # must show $VERSION
# only if website/ changed:
(cd website && mkdocs build --strict -f mkdocs.yml)         # "Documentation built", no page warnings
```

The red "Material for MkDocs" 2.0 banner is cosmetic — judge by exit 0 and no
`WARNING` lines about your pages.

## Step 6 — Commit + push main

```bash
git add CHANGELOG.md deploy/deployment.yaml deploy/helm/kubeagent/Chart.yaml deploy/README.md website/docs/install.md
git commit -m "release: $VERSION (<one-line summary>)"     # NO Co-Authored-By trailer
git push origin main
```

## Step 7 — Tag → triggers CI

```bash
git tag $VERSION
git push origin $VERSION
```

The `v*` tag starts `release.yml`. It automatically:

1. `go test ./...`
2. builds `kubeagent` with `-ldflags "-X main.version=$VERSION"`
3. packages `kubeagent_${VERSION}_linux_amd64.tar.gz` + `SHA256SUMS`
4. publishes the **GitHub Release** with those assets
5. **only if** the `DOCKERHUB_TOKEN` secret is set: builds and pushes
   `imantaba/kubeagent:$VERSION` and `imantaba/kubeagent:latest`

You do not build the binary or image by hand — the workflow owns that.

## Step 8 — Verify the release

```bash
gh run list --workflow=release.yml --limit 1                 # watch it go green
gh release view $VERSION                                     # assets attached
```

- Confirm the Docker Hub tag exists: `imantaba/kubeagent:$VERSION` (and
  `:latest`) on https://hub.docker.com/r/imantaba/kubeagent. If the image step
  was skipped, the Docker Hub secrets are unset — the GitHub Release still
  succeeds; fix the secrets and re-run only if the image is needed.
- If `website/**` changed, `pages.yml` redeploys https://k8sproject.top on the
  push to `main` — check its run too.

## Step 9 — (Optional) roll the live daemon

If a cluster runs the in-cluster daemon and should track the new image, re-apply
after the image is on Docker Hub (this is a **live-cluster write** — confirm the
target and change explicitly first):

```bash
kubectl --context <ctx> apply -f deploy/
kubectl --context <ctx> -n kubeagent rollout status deploy/kubeagent
```
