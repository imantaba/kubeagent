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
