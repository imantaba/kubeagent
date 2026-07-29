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
#
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
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION="${1:-}"
OUTDIR="${2:-}"
[ -n "$VERSION" ] || die "usage: scripts/build-release-archives.sh VERSION OUTDIR"
[ -n "$OUTDIR" ] || die "usage: scripts/build-release-archives.sh VERSION OUTDIR"

# Run from the repo root regardless of where we're invoked.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Glob expansion (the stale-artifact cleanup below, the SHA256SUMS line at
# the bottom) and tar member order must be bytewise everywhere, not sorted by
# whatever locale the builder's shell happens to have — a non-C locale can
# reorder both silently.
export LC_ALL=C

# Defaulted from the commit rather than from the clock: "now" would be
# reproducible only within a single run, which defeats the point. A checkout
# with no commit time is an error, not a reason to substitute one.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --pretty=%ct 2>/dev/null || true)}"
[ -n "$SOURCE_DATE_EPOCH" ] ||
  die "SOURCE_DATE_EPOCH is unset and HEAD has no commit time (not a git checkout?) — set SOURCE_DATE_EPOCH to build reproducibly"

# RELEASE_PLATFORMS exists so the reproducibility test can double-build a
# subset quickly. Every real caller gets the default four.
: "${RELEASE_PLATFORMS:=linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"

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

for platform in $RELEASE_PLATFORMS; do
  os="${platform%/*}"
  arch="${platform#*/}"
  echo "building ${os}/${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o "$stage/kubeagent" .
  # NOTICE travels with LICENSE: Apache-2.0 section 4(d) requires redistributions
  # to carry it.
  cp README.md LICENSE NOTICE "$stage/"
  # tar records whatever mode the staging files happen to carry, and both
  # `go build` and `cp` inherit the caller's umask — a builder running with
  # umask 077 would otherwise produce a different archive from one running
  # with 022, for no reason a verifier could distinguish from tampering.
  chmod 0755 "$stage/kubeagent"
  chmod 0644 "$stage/README.md" "$stage/LICENSE" "$stage/NOTICE"
  # --sort=name sorts bytewise under LC_ALL=C (exported above). Each flag
  # closes one leak: entry order, the building user's uid and name, the
  # staging directory's mtime. gzip is invoked separately because tar -czf
  # gives no way to pass it -n, and the gzip header has its own filename and
  # timestamp fields.
  #
  # --sort=name only reorders names tar discovers itself (directory
  # recursion, --files-from); it does not reorder names given explicitly on
  # the command line, so these four are listed in the bytewise order we want
  # in the archive (LICENSE < NOTICE < README.md < kubeagent under LC_ALL=C,
  # since uppercase sorts before lowercase).
  tar --sort=name --numeric-owner --owner=0 --group=0 \
      --mtime="@${SOURCE_DATE_EPOCH}" \
      -C "$stage" -cf - LICENSE NOTICE README.md kubeagent |
    gzip -n > "${OUTDIR}/kubeagent_${VERSION}_${os}_${arch}.tar.gz"
done

# Unversioned copy so releases/latest/download/kubeagent_linux_amd64.tar.gz
# always resolves to the newest release. That URL is in the wild — the README
# quick-install and people's own notes — and dropping it would break every
# copy of that install line silently.
[ -f "${OUTDIR}/kubeagent_${VERSION}_linux_amd64.tar.gz" ] ||
  die "linux/amd64 was not built (RELEASE_PLATFORMS=${RELEASE_PLATFORMS}) — the unversioned archive that releases/latest/download resolves to cannot be produced"
cp "${OUTDIR}/kubeagent_${VERSION}_linux_amd64.tar.gz" \
   "${OUTDIR}/kubeagent_linux_amd64.tar.gz"

# Bare filenames: the krew manifest renderer looks checksums up by archive
# name, and `sha256sum -c SHA256SUMS` must work from the download directory.
( cd "$OUTDIR" && sha256sum kubeagent_*.tar.gz > SHA256SUMS )
cat "${OUTDIR}/SHA256SUMS"
