#!/usr/bin/env bash
# Resolves a Kubernetes minor to the node image for either distribution the
# chaos harness should boot, and to the suffix that keeps two versions'
# clusters from colliding.
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

# chaos_newest — the newest supported minor (the last entry in the list).
#
# Two places mean "the newest one": the k3s path's default image, and the CI
# matrix's single k3s cell. Both resolve it from here rather than naming a
# minor, so adding or dropping a minor stays the one-line commit it is today.
chaos_newest() { printf '%s\n' "$KUBEAGENT_CHAOS_VERSIONS" | awk '{print $NF}'; }

# chaos_k3s_image <minor> — the digest-pinned rancher/k3s image for <minor>:
# chaos_image's counterpart on the k3s path, with the same contract for the
# same reason. It validates before it derives a variable name, and it never
# prints a partial answer, because a caller that ignored the status would hand
# `k3d cluster create` an empty --image and boot whatever k3d defaults to —
# which, unlike kind's default, moves with the k3d release.
chaos_k3s_image() {
  if ! _chaos_known "${1:-}"; then
    printf 'unsupported --k8s-version: %s (supported: %s)\n' \
      "${1:-<empty>}" "$KUBEAGENT_CHAOS_VERSIONS" >&2
    return 1
  fi
  local var="KUBEAGENT_CHAOS_K3S_IMAGE_${1//./_}"
  if [ -z "${!var:-}" ]; then
    printf 'chaos/versions.env lists %s but defines no k3s image for it\n' "$1" >&2
    return 1
  fi
  printf '%s\n' "${!var}"
}
