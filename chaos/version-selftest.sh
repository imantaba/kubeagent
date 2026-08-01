#!/usr/bin/env bash
# Cluster-free self-test for chaos/versions.sh. Proves each function answers
# correctly for a supported minor AND rejects an unsupported one, because a
# resolver that silently returns empty would hand `kind create cluster` an empty
# --image and boot the wrong Kubernetes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=chaos/versions.sh
. "$ROOT/chaos/versions.sh"

fails=0
check() {   # check <label> <actual> <want>
  if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s (got %s, want %s)\n' "$1" "$2" "$3"; fails=$((fails + 1)); fi
}

# --- chaos_versions -------------------------------------------------------
v="$(chaos_versions)"
check "chaos_versions lists v1.32" "$(printf '%s' "$v" | grep -c 'v1\.32' || true)" 1
check "chaos_versions lists v1.33" "$(printf '%s' "$v" | grep -c 'v1\.33' || true)" 1
check "chaos_versions lists v1.34" "$(printf '%s' "$v" | grep -c 'v1\.34' || true)" 1

# --- chaos_image ----------------------------------------------------------
# Every supported minor must resolve to a digest-pinned reference. A bare tag
# would let a silently retagged upstream image turn a green nightly red with no
# kubeagent change, which is the whole reason this file exists.
for m in $(chaos_versions); do
  img="$(chaos_image "$m")" && rc=0 || rc=$?
  check "chaos_image $m exits 0"          "$rc" 0
  check "chaos_image $m names kindest"    "$(printf '%s' "$img" | grep -c '^kindest/node:' || true)" 1
  check "chaos_image $m is digest-pinned" "$(printf '%s' "$img" | grep -cE '@sha256:[0-9a-f]{64}$' || true)" 1
  check "chaos_image $m names the minor"  "$(printf '%s' "$img" | grep -cF "kindest/node:${m}." || true)" 1
done

# A shape-valid minor that is merely a PREFIX of a supported one must still be
# rejected. Without this, relaxing the membership test to a prefix match leaves
# every other check in this file passing: chaos_image v1.3 still fails, but only
# incidentally, because no KUBEAGENT_CHAOS_IMAGE_v1_3 exists — while chaos_suffix
# v1.3 happily returns "-v1-3" and names a cluster that boots the wrong minor.
for near in v1.3 v1.320 v01.32; do
  out="$(chaos_image "$near" 2>/dev/null)" && rc=0 || rc=$?
  check "chaos_image rejects the near-miss '$near'"  "$rc"  1
  check "chaos_image prints nothing for '$near'"     "$out" ""
  out="$(chaos_suffix "$near" 2>/dev/null)" && rc=0 || rc=$?
  check "chaos_suffix rejects the near-miss '$near'" "$rc"  1
  check "chaos_suffix prints nothing for '$near'"    "$out" ""
done

# --- chaos_image rejects ---------------------------------------------------
out="$(chaos_image v9.99 2>/dev/null)" && rc=0 || rc=$?
check "chaos_image rejects an unsupported minor"     "$rc"  1
check "chaos_image prints nothing on stdout when it rejects" "$out" ""
err="$(chaos_image v9.99 2>&1 >/dev/null)" || true
check "the rejection names the supported set" "$(printf '%s' "$err" | grep -c 'v1\.34' || true)" 1

# A malformed value must be refused BEFORE it is used to build a variable name.
for bad in '' 'v1' '1.33' 'v1.33; echo pwned' '../etc'; do
  out="$(chaos_image "$bad" 2>/dev/null)" && rc=0 || rc=$?
  check "chaos_image rejects '$bad'"                "$rc"  1
  check "chaos_image prints nothing for '$bad'"     "$out" ""
done

# --- chaos_suffix ---------------------------------------------------------
check "chaos_suffix v1.33" "$(chaos_suffix v1.33)" "-v1-33"
check "chaos_suffix v1.34" "$(chaos_suffix v1.34)" "-v1-34"
out="$(chaos_suffix 'v1.33; echo pwned' 2>/dev/null)" && rc=0 || rc=$?
check "chaos_suffix rejects a malformed minor" "$rc" 1

# --- set -e safety --------------------------------------------------------
# A rejection must not abort a caller that is checking the status itself.
survived=no
chaos_image v9.99 >/dev/null 2>&1 || survived=yes
check "a rejection does not abort the caller under set -e" "$survived" yes

printf '\n%s checks failed\n' "$fails"
[ "$fails" -eq 0 ]
