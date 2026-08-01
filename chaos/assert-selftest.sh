#!/usr/bin/env bash
# Scenario-free self-test for the chaos assertion helpers: no cluster, no docker,
# runs in under a second. Proves each helper both PASSES and FAILS, that a FAIL
# never aborts the caller under `set -e`, that an outcome recorded inside a
# subshell still counts (record() feeds every scenario through a pipeline), and
# that assert_summary's exit status follows the failure count.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
. chaos/assert.sh

fails=0
check() {   # check <what> <actual> <want>
  if [ "$2" = "$3" ]; then
    printf 'ok     %s\n' "$1"
  else
    printf 'NOT OK %s: got %s, want %s\n' "$1" "$2" "$3"
    fails=$((fails + 1))
  fi
}

# --- each helper passes on the value it is meant to accept --------------------
assert_init
line="$(expect_eq       'eq'       abc abc)"
check 'expect_eq prints PASS'        "${line%%:*}" PASS
line="$(expect_ge       'ge'       3 3)"
check 'expect_ge prints PASS at the floor' "${line%%:*}" PASS
line="$(expect_contains 'contains' 'alpha beta' beta)"
check 'expect_contains prints PASS'  "${line%%:*}" PASS
line="$(expect_absent   'absent'   'alpha beta' gamma)"
check 'expect_absent prints PASS'    "${line%%:*}" PASS
assert_summary /dev/null >/dev/null && rc=0 || rc=$?
check 'summary returns 0 with no failures' "$rc" 0

# --- each helper fails on the value it is meant to reject ---------------------
assert_init
line="$(expect_eq       'eq'       abc xyz)"
check 'expect_eq prints FAIL'        "${line%%:*}" FAIL
line="$(expect_ge       'ge'       2 3)"
check 'expect_ge prints FAIL below the floor' "${line%%:*}" FAIL
line="$(expect_contains 'contains' 'alpha beta' gamma)"
check 'expect_contains prints FAIL'  "${line%%:*}" FAIL
line="$(expect_absent   'absent'   'alpha beta' beta)"
check 'expect_absent prints FAIL'    "${line%%:*}" FAIL
assert_summary /dev/null >/dev/null && rc=0 || rc=$?
check 'summary returns 1 after failures' "$rc" 1

# --- "couldn't tell" is a failure, never a zero ------------------------------
# ready_replicas() prints "?" when the query itself failed. Coercing that to 0
# would turn a harness fault into a confident reading.
assert_init
line="$(expect_ge 'ready replicas' '?' 1)"
check 'expect_ge fails on "?"'       "${line%%:*}" FAIL
line="$(expect_ge 'ready replicas' '' 1)"
check 'expect_ge fails on empty'     "${line%%:*}" FAIL

# --- a FAIL does not abort the caller under set -e ---------------------------
# This is the whole design: all 22 scenarios must still run after one fails.
assert_init
reached=no
expect_eq 'deliberate failure' a b >/dev/null
reached=yes
check 'execution continues past a FAIL' "$reached" yes

# --- an outcome recorded inside a subshell still counts ----------------------
# Every scenario calls these helpers inside `{ ... } | record ...`, and a
# pipeline runs in a subshell: a shell-variable counter would be lost here.
assert_init
{ expect_eq 'inside a pipeline' a b; } | cat >/dev/null
assert_summary /dev/null >/dev/null && rc=0 || rc=$?
check 'subshell failure survives into the summary' "$rc" 1

# --- the summary lands in the report file ------------------------------------
assert_init
expect_eq 'reported failure' a b >/dev/null
report="$(mktemp)"
assert_summary "$report" >/dev/null || true
check 'summary names the failure in the report' \
  "$(grep -c 'reported failure' "$report")" 1
check 'summary reports the failure count' \
  "$(grep -c '^- failed: 1$' "$report")" 1
rm -f "$report"

printf '\n%s\n' "$([ "$fails" -eq 0 ] && echo 'assert-selftest: all checks passed' \
                                     || echo "assert-selftest: $fails check(s) failed")"
[ "$fails" -eq 0 ]
