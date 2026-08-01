#!/usr/bin/env bash
# Resolves a Kubernetes minor to the kind node image the chaos harness should
# boot, and to the suffix that keeps two versions' clusters from colliding.
# Sourced by chaos/run.sh and by chaos/version-selftest.sh; the data itself
# lives in chaos/versions.env, so everything that needs to know what "supported"
# means resolves it from one place rather than keeping its own copy — the
# harness and the release skill's per-minor loop today, and CI when the nightly
# matrix lands.

CHAOS_VERSIONS_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=chaos/versions.env
. "$CHAOS_VERSIONS_ROOT/chaos/versions.env"

# chaos_versions — the supported minors, space-separated on one line.
chaos_versions() { printf '%s\n' "$KUBEAGENT_CHAOS_VERSIONS"; }

# _chaos_known <minor> — 0 when <minor> is well-formed AND listed as supported.
# The shape check runs first and is not decoration: the minor is turned into a
# variable name below, and a value that is not vN.N has no business getting that
# far. Membership is tested word-by-word so that "v1.3" cannot match "v1.33".
_chaos_known() {
  case "$1" in v[0-9]*.[0-9]*) ;; *) return 1 ;; esac
  printf '%s' "$1" | grep -qE '^v[0-9]+\.[0-9]+$' || return 1
  local v
  for v in $KUBEAGENT_CHAOS_VERSIONS; do [ "$v" = "$1" ] && return 0; done
  return 1
}

# chaos_image <minor> — the digest-pinned node image, or exit 1 with a message
# on stderr naming what IS supported. Never prints a partial answer: a caller
# that ignored the status would otherwise hand `kind create cluster` an empty
# --image and silently boot whatever kind defaults to.
chaos_image() {
  if ! _chaos_known "${1:-}"; then
    printf 'unsupported --k8s-version: %s (supported: %s)\n' \
      "${1:-<empty>}" "$KUBEAGENT_CHAOS_VERSIONS" >&2
    return 1
  fi
  local var="KUBEAGENT_CHAOS_IMAGE_${1//./_}"
  if [ -z "${!var:-}" ]; then
    printf 'chaos/versions.env lists %s but defines no image for it\n' "$1" >&2
    return 1
  fi
  printf '%s\n' "${!var}"
}

# chaos_suffix <minor> — the name suffix for <minor>: v1.33 -> -v1-33. Dots are
# not legal in a kind cluster name, and the suffix is what keeps two minors'
# clusters, contexts and scratch files from colliding on one machine.
chaos_suffix() {
  _chaos_known "${1:-}" || {
    printf 'unsupported --k8s-version: %s (supported: %s)\n' \
      "${1:-<empty>}" "$KUBEAGENT_CHAOS_VERSIONS" >&2
    return 1
  }
  printf -- '-%s\n' "${1//./-}"
}
