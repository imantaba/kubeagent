#!/usr/bin/env bash
# build-release-archives.sh — build the release archive for every published
# platform, plus SHA256SUMS.
#
# Usage:  scripts/build-release-archives.sh VERSION OUTDIR
#
# A relative OUTDIR is resolved against the repo root (the script cd's there
# before creating it), not the caller's current directory.
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

# Remove this script's own previous outputs so a reused, dirty OUTDIR starts
# from a known state. Without this, running twice into the same OUTDIR with
# different VERSIONs (and no cleanup in between) leaves the earlier run's
# archives sitting next to the new ones, and the `kubeagent_*.tar.gz` glob
# below picks them up too — stale checksums land in SHA256SUMS silently, exit
# code 0. Only the patterns this script creates are removed, and only in
# OUTDIR itself (no recursion).
removed=0
for f in "$OUTDIR"/kubeagent_*.tar.gz "$OUTDIR/SHA256SUMS"; do
  if [ -e "$f" ]; then
    rm -f -- "$f"
    removed=1
  fi
done
[ "$removed" -eq 1 ] && echo "removed stale artifacts from a previous run in $OUTDIR"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${platform%/*}"
  arch="${platform#*/}"
  echo "building ${os}/${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -ldflags "-X main.version=${VERSION}" -o "$stage/kubeagent" .
  # NOTICE travels with LICENSE: Apache-2.0 section 4(d) requires redistributions
  # to carry it.
  cp README.md LICENSE NOTICE "$stage/"
  tar -czf "${OUTDIR}/kubeagent_${VERSION}_${os}_${arch}.tar.gz" \
    -C "$stage" kubeagent README.md LICENSE NOTICE
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
