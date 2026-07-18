#!/usr/bin/env bash
# bump-version.sh — bump every version reference for a kubeagent release.
#
# The version is scattered across the CHANGELOG, the raw deploy manifest, the Helm
# chart (two fields), and two docs. Bumping by hand is error-prone (a missed compare
# link or a stale image tag ships a wrong release), so this does all of it in one go
# and verifies nothing stale remains.
#
# It does NOT commit, tag, or push — that stays with the operator / the `release`
# skill, which owns the tests + gate that must pass first.
#
# Usage:  scripts/bump-version.sh vX.Y.Z
#   RELEASE_DATE=YYYY-MM-DD  override the CHANGELOG date (default: today)
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

NEW_V="${1:-}"
[ -n "$NEW_V" ] || die "usage: scripts/bump-version.sh vX.Y.Z"
[[ "$NEW_V" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must look like v1.2.3 (got '$NEW_V')"
NEW="${NEW_V#v}"                                   # 1.2.3
DATE="${RELEASE_DATE:-$(date +%Y-%m-%d)}"

# Run from the repo root regardless of where we're invoked.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CHART=deploy/helm/kubeagent/Chart.yaml
CHANGELOG=CHANGELOG.md
REPO_URL=https://github.com/imantaba/kubeagent

# Current (previous) app version comes from the chart — the single source of truth.
OLD="$(grep -oP '^appVersion: "v\K[0-9]+\.[0-9]+\.[0-9]+' "$CHART")" \
  || die "could not read current appVersion from $CHART"
[ "$OLD" != "$NEW" ] || die "current version is already v$NEW"
echo "bumping v$OLD → v$NEW  (date $DATE)"

# --- CHANGELOG ------------------------------------------------------------------
grep -q '^## \[Unreleased\]$' "$CHANGELOG" || die "no '## [Unreleased]' section in $CHANGELOG"
# Turn the current [Unreleased] section into the release, and open a fresh empty one.
sed -i "0,/^## \[Unreleased\]$/s//## [Unreleased]\n\n## [$NEW] - $DATE/" "$CHANGELOG"
# Repoint the Unreleased compare link at the new tag, and add the new release's link.
sed -i "s#^\[Unreleased\]: .*#[Unreleased]: $REPO_URL/compare/v$NEW...HEAD#" "$CHANGELOG"
sed -i "/^\[$OLD\]: /i [$NEW]: $REPO_URL/compare/v$OLD...v$NEW" "$CHANGELOG"

# --- deploy + docs (raw string swaps, targeted so we don't touch history) --------
sed -i "s#imantaba/kubeagent:v$OLD#imantaba/kubeagent:v$NEW#g" deploy/deployment.yaml
sed -i "s#^appVersion: \"v$OLD\"#appVersion: \"v$NEW\"#" "$CHART"
sed -i "s#--set image.tag=v$OLD#--set image.tag=v$NEW#g" deploy/README.md
sed -i "s#imantaba/kubeagent:v$OLD#imantaba/kubeagent:v$NEW#g; s#--set image.tag=v$OLD#--set image.tag=v$NEW#g" website/docs/install.md

# --- Helm chart's own version: patch-bump by default -----------------------------
# Convention: patch when only appVersion changed; MINOR when templates/values changed.
# The script can't tell, so it patch-bumps and tells you to override if templates moved.
CHART_VER="$(grep -oP '^version: \K[0-9]+\.[0-9]+\.[0-9]+' "$CHART")"
CHART_NEW="$(awk -F. '{print $1"."$2"."$3+1}' <<<"$CHART_VER")"
sed -i "s#^version: $CHART_VER\$#version: $CHART_NEW#" "$CHART"

# --- verify nothing stale remains ------------------------------------------------
STALE="$(grep -rn "v$OLD" --include=*.yaml --include=*.md . \
  | grep -v "$CHANGELOG" | grep -v docs/superpowers | grep -v '.superpowers/' || true)"
[ -z "$STALE" ] || { echo "STALE references to v$OLD remain:" >&2; echo "$STALE" >&2; exit 1; }

cat <<EOF

bumped to v$NEW:
  CHANGELOG.md            [Unreleased] → [$NEW] - $DATE (+ compare links; fresh [Unreleased])
  deploy/deployment.yaml  image tag → v$NEW
  $CHART                  appVersion → v$NEW · chart version $CHART_VER → $CHART_NEW (patch)
  deploy/README.md        --set image.tag → v$NEW
  website/docs/install.md image pin + --set image.tag → v$NEW

no stale v$OLD references outside CHANGELOG/superpowers ✅

next:
  • if the chart's templates/values changed this release, edit $CHART
    'version:' to a MINOR bump ($CHART_VER → $(awk -F. '{print $1"."$2+1".0"}' <<<"$CHART_VER")) instead of the patch above.
  • review 'git diff', then commit + push main and tag v$NEW (the release skill does the rest).
EOF
