#!/usr/bin/env bash
# dco-check.sh — verify every commit in a range carries a DCO sign-off.
#
# Usage:  scripts/dco-check.sh BASE_SHA HEAD_SHA
#         scripts/dco-check.sh main            # BASE..HEAD, HEAD implied
#
# A commit passes when it carries a `Signed-off-by: Name <email>` trailer whose
# email matches the commit author's, compared case-insensitively — the same
# rule the DCO app enforces on GitHub. Merge commits are skipped: their content
# is signed off on the commits they merge.
#
# Exits 0 when every commit is signed off, 1 with a per-commit report otherwise.
set -euo pipefail

die() { echo "error: $*" >&2; exit 1; }

BASE="${1:-}"
HEAD_REF="${2:-HEAD}"
[ -n "$BASE" ] || die "usage: scripts/dco-check.sh BASE_SHA [HEAD_SHA]"

git rev-parse --verify --quiet "$BASE^{commit}" >/dev/null ||
	die "base commit not found: $BASE (fetch the full history — actions/checkout needs fetch-depth: 0)"
git rev-parse --verify --quiet "$HEAD_REF^{commit}" >/dev/null ||
	die "head commit not found: $HEAD_REF"

# --no-merges: a merge commit introduces no authored change of its own.
mapfile -t commits < <(git rev-list --no-merges "$BASE..$HEAD_REF")

if [ "${#commits[@]}" -eq 0 ]; then
	echo "DCO: no commits to check in $BASE..$HEAD_REF"
	exit 0
fi

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

failed=0
for sha in "${commits[@]}"; do
	subject=$(git show -s --format=%s "$sha")
	author_email=$(lower "$(git show -s --format=%ae "$sha")")

	signed=0
	while IFS= read -r trailer_email; do
		[ -n "$trailer_email" ] || continue
		if [ "$(lower "$trailer_email")" = "$author_email" ]; then
			signed=1
			break
		fi
	done < <(git show -s --format=%B "$sha" |
		grep -iE '^[[:space:]]*Signed-off-by:' |
		sed -nE 's/.*<([^>]+)>.*/\1/p')

	if [ "$signed" -eq 1 ]; then
		printf 'DCO: ok       %s %s\n' "${sha:0:8}" "$subject"
	else
		printf 'DCO: MISSING  %s %s\n' "${sha:0:8}" "$subject"
		printf '              author: %s\n' "$(git show -s --format='%an <%ae>' "$sha")"
		failed=$((failed + 1))
	fi
done

if [ "$failed" -gt 0 ]; then
	cat >&2 <<-EOF

		$failed of ${#commits[@]} commit(s) are missing a matching
		"Signed-off-by: Name <email>" trailer.

		kubeagent uses the Developer Certificate of Origin (https://developercertificate.org/).
		Sign off future commits with:

		    git commit -s

		and sign off the ones already on this branch with:

		    git rebase --signoff $BASE
		    git push --force-with-lease

		The sign-off email must match the commit author's email.
		See CONTRIBUTING.md.
	EOF
	exit 1
fi

echo "DCO: all ${#commits[@]} commit(s) signed off."
