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
# This is the whole design: all 23 scenarios must still run after one fails.
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

# --- assert_skip: a skip is recorded, printed, and is never a failure --------
assert_init
skip_line="$(assert_skip '5. coredns' 'writes cluster-scoped objects, which the harness will not do on a cluster it does not own')"
check 'assert_skip prints one console line' \
  "$skip_line" \
  'SKIP: 5. coredns — writes cluster-scoped objects, which the harness will not do on a cluster it does not own'
check 'assert_skip leaves the assertion log untouched' "$(wc -l < "$ASSERTLOG" | tr -d ' ')" 0
check 'assert_skip appends one skip-log line'         "$(wc -l < "$SKIPLOG"   | tr -d ' ')" 1
assert_skip '2. certs' 'documented' >/dev/null && rc=0 || rc=$?
check 'assert_skip returns 0'                         "$rc" 0
check 'assert_skip appends, it does not overwrite'    "$(wc -l < "$SKIPLOG" | tr -d ' ')" 2

# --- assert_summary: skips are counted, in the report and on the console -----
assert_init
expect_eq 'a passing check' 1 1 >/dev/null
rep="$(mktemp)"
console="$(assert_summary "$rep")" && rc=0 || rc=$?
check 'a run with no skips still exits 0' "$rc" 0
check 'the console line names zero skips' \
  "$(printf '%s' "$console" | tail -n1)" 'assertions: 1 run, 0 failed; 0 scenarios skipped'
check 'the report carries the third bullet' "$(grep -c '^- scenarios skipped: 0$' "$rep")" 1
check 'no skip block is written when there are none' "$(grep -c '^SKIP' "$rep")" 0
rm -f "$rep"

assert_init
expect_eq 'a passing check' 1 1 >/dev/null
assert_skip '2. certs' 'control-plane certificate expiry cannot be forced quickly or safely' >/dev/null
rep="$(mktemp)"
console="$(assert_summary "$rep")" && rc=0 || rc=$?
check 'one skip does not change the exit code' "$rc" 0
check 'the console line is singular for one skip' \
  "$(printf '%s' "$console" | tail -n1)" 'assertions: 1 run, 0 failed; 1 scenario skipped'
check 'the report counts the skip'   "$(grep -c '^- scenarios skipped: 1$' "$rep")" 1
check 'the report lists the skip'    "$(grep -c '^SKIP	2. certs — control-plane certificate expiry cannot be forced quickly or safely$' "$rep")" 1
rm -f "$rep"

assert_init
expect_eq 'a failing check' 1 2 >/dev/null 2>&1
assert_skip '5. coredns' 'a reason' >/dev/null
assert_skip '3. diskfull' 'another reason' >/dev/null
assert_skip '1. etcd' 'a third reason' >/dev/null
rep="$(mktemp)"
console="$(assert_summary "$rep")" && rc=0 || rc=$?
check 'a failure still fails the gate, skips or not' "$rc" 1
check 'the console line is plural for several skips' \
  "$(printf '%s' "$console" | tail -n1)" 'assertions: 1 run, 1 failed; 3 scenarios skipped'
check 'the report counts every skip' "$(grep -c '^- scenarios skipped: 3$' "$rep")" 1
check 'the report lists every skip'  "$(grep -c '^SKIP	' "$rep")" 3
check 'the report still lists the failure' "$(grep -c '^FAIL	' "$rep")" 1
rm -f "$rep"

# --- scenario_title: the skip heading, derived from the function name --------
# Every real scenario name in run.sh, so a rename that breaks the derivation is
# caught here rather than in a report heading nobody diffs.
check 'scenario_title 01' "$(scenario_title scenario_01_etcd)"          '1. etcd'
check 'scenario_title 02' "$(scenario_title scenario_02_certs)"         '2. certs'
check 'scenario_title 03' "$(scenario_title scenario_03_diskfull)"      '3. diskfull'
check 'scenario_title 04' "$(scenario_title scenario_04_networkpolicy)" '4. networkpolicy'
check 'scenario_title 05' "$(scenario_title scenario_05_coredns)"       '5. coredns'
check 'scenario_title 06' "$(scenario_title scenario_06_lb)"            '6. lb'
check 'scenario_title 07' "$(scenario_title scenario_07_oom)"           '7. oom'
check 'scenario_title 08' "$(scenario_title scenario_08_nsdelete)"      '8. nsdelete'
check 'scenario_title 09' "$(scenario_title scenario_09_rollout)"       '9. rollout'
check 'scenario_title 10' "$(scenario_title scenario_10_credleak)"      '10. credleak'
check 'scenario_title 11' "$(scenario_title scenario_11_kubelet)"       '11. kubelet'
check 'scenario_title 12' "$(scenario_title scenario_12_watch)"         '12. watch'
check 'scenario_title 13' "$(scenario_title scenario_13_slo)"           '13. slo'
check 'scenario_title 14 (no trailing word)' "$(scenario_title scenario_14)" '14.'
check 'scenario_title 15' "$(scenario_title scenario_15_multicluster)"  '15. multicluster'
check 'scenario_title 16' "$(scenario_title scenario_16_operators)"     '16. operators'
check 'scenario_title 17' "$(scenario_title scenario_17_gitops)"        '17. gitops'
check 'scenario_title 18' "$(scenario_title scenario_18_capacity)"      '18. capacity'
check 'scenario_title 19' "$(scenario_title scenario_19_mcp)"           '19. mcp'
check 'scenario_title 20' "$(scenario_title scenario_20_rbac)"          '20. rbac'
check 'scenario_title 21' "$(scenario_title scenario_21_controlplane)"  '21. controlplane'
check 'scenario_title 22' "$(scenario_title scenario_22_dnshealth)"     '22. dnshealth'
check 'scenario_title 23' "$(scenario_title scenario_23_pagerduty)"     '23. pagerduty'

printf '\n%s\n' "$([ "$fails" -eq 0 ] && echo 'assert-selftest: all checks passed' \
                                     || echo "assert-selftest: $fails check(s) failed")"
[ "$fails" -eq 0 ]
