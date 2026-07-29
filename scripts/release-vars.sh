#!/usr/bin/env bash
# release-vars.sh — classify a release tag for the release workflow.
#
# Usage:  scripts/release-vars.sh VERSION
#
# Prints GitHub-Actions output syntax on stdout, for redirection into
# "$GITHUB_OUTPUT":
#
#   prerelease=false
#   push_latest=true
#
# A SemVer pre-release (v1.2.3-rc.1) must not move the :latest image tag.
# Every unpinned `docker pull imantaba/kubeagent` resolves through it, and a
# release candidate is by definition not what an unpinned pull should get. It
# is also published as a GitHub pre-release rather than as the newest release.
# Without this rule the pre-release tag that exercises the release pipeline
# would ship a candidate to everyone.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

VERSION="${1:-}"
[ -n "$VERSION" ] || die "usage: scripts/release-vars.sh VERSION"

# SemVer with a mandatory leading v. Anything else stops the release: a
# malformed tag should not produce a release with a malformed name.
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$ ]]; then
  die "not a SemVer release tag: $VERSION (want vMAJOR.MINOR.PATCH[-prerelease][+build])"
fi

# Strip build metadata before looking for the pre-release hyphen: the hyphen
# in v1.2.3+build-5 belongs to the metadata, not to a pre-release.
core="${VERSION%%+*}"
if [[ "$core" == *-* ]]; then
  prerelease=true
  push_latest=false
else
  prerelease=false
  push_latest=true
fi

printf 'prerelease=%s\n' "$prerelease"
printf 'push_latest=%s\n' "$push_latest"
