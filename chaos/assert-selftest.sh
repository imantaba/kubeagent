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

# --- requires: the capability gate, from the real run.sh ---------------------
# run.sh calls main() only when executed directly, so it can be sourced here to
# exercise its pure helpers with no cluster. Each probe runs in a SUBSHELL:
# sourcing run.sh sets the harness's own globals, and they must not leak into
# the checks around this block.
requires_probe() {   # requires_probe <available caps> <capability> -> "<rc>|<skip lines>|<report sections>"
  (
    # A subshell inherits the enclosing function's positional parameters, so a
    # bare `. chaos/run.sh` here would hand run.sh's own --flag parser "$1"
    # and "$2" as if they were its argv. Capture them first and clear $@
    # before sourcing.
    local caps="$1" cap="$2"
    set --
    . chaos/run.sh
    ASSERTLOG="$(mktemp)"; SKIPLOG="$(mktemp)"; OUT="$(mktemp)"
    : > "$ASSERTLOG"; : > "$SKIPLOG"; : > "$OUT"
    trap 'rm -f "$ASSERTLOG" "$SKIPLOG" "$OUT"' EXIT
    CAPS="$caps"
    scenario_99_probe() { requires "$cap" || return 1; return 0; }
    scenario_99_probe >/dev/null 2>&1 && rc=0 || rc=$?
    printf '%s|%s|%s\n' "$rc" \
      "$(wc -l < "$SKIPLOG" | tr -d ' ')" \
      "$(grep -c '^## ' "$OUT" || true)"
  )
}

check 'requires returns 0 for an available capability' \
  "$(requires_probe 'node_exec cluster_write' cluster_write)" '0|0|0'
check 'requires returns 1 and records one skip for an unavailable capability' \
  "$(requires_probe 'node_exec' cluster_write)" '1|1|1'
check 'requires records a skip for every one of the six names' \
  "$(for c in node_exec cluster_write clean_baseline no_loadbalancer no_metrics_server netpol_enforced; do
       requires_probe '' "$c"
     done | sort -u | tr -d '\n')" '1|1|1'

# An unknown capability name is a harness bug, not a silent skip: a typo in a
# guard would otherwise turn a scenario off and read as a passing run.
#
# These sourcing subshells sit at the script's top level, so $@ here is this
# script's own argv rather than captured positional params — same reason as
# requires_probe above, so they get the same `set --` guard before sourcing.
unknown_rc="$(
  (
    set --
    . chaos/run.sh
    CAPS=''
    scenario_99_probe() { requires no_such_capability; }
    scenario_99_probe
  ) >/dev/null 2>&1 && echo 0 || echo $?
)"
check 'requires exits 2 on an unknown capability name' "$unknown_rc" 2

# capability_reason is the single source of a skip's wording, so two guards on
# the same capability cannot describe it differently.
check 'capability_reason knows cluster_write' \
  "$( ( set --; . chaos/run.sh; capability_reason cluster_write ) )" \
  'writes cluster-scoped objects, which the harness will not do on a cluster it does not own'
check 'capability_reason rejects an unknown name' \
  "$( ( set --; . chaos/run.sh; capability_reason nope >/dev/null ) && echo 0 || echo 1 )" 1

# node_exec is withheld for two different reasons on the two paths where it can
# be withheld, and the reason a reader sees has to be true on both: a foreign
# cluster the harness does not own may still be kubeadm-shaped, and a k3d
# cluster the harness owns outright has no separately stoppable etcd or kubelet.
# Naming both requirements is what keeps the printed reason honest wherever it
# appears — a skip reason that is false in the report is worse than none at all.
check 'capability_reason names both of node_exec requirements' \
  "$( ( set --; . chaos/run.sh; capability_reason node_exec ) )" \
  'needs shell access to a node the harness owns, whose control plane runs etcd and kubelet as separately stoppable units'
check 'the node_exec reason does not rest on ownership alone' \
  "$( ( set --; . chaos/run.sh; capability_reason node_exec ) | grep -ci 'separately stoppable' || true)" 1

# capability_add is idempotent and validates, so a typo cannot silently switch
# a scenario on.
check 'capability_add is idempotent' \
  "$( ( set --; . chaos/run.sh; CAPS=''; capability_add node_exec; capability_add node_exec; printf '%s' "$CAPS" ) )" \
  'node_exec'
check 'capability_add exits 2 on an unknown name' \
  "$( ( set --; . chaos/run.sh; capability_add nope ) >/dev/null 2>&1 && echo 0 || echo $? )" 2

# --- remediation_outcome: which branch a --fix/--rollback run took -----------
# Scenario 9b asserts on two image names and an audit-log count. When the round
# trip fails, those three say THAT it failed and nothing about WHY — applied,
# refused, preflight-denied, an error, and "there was no record to roll back"
# all look identical from the outside. remediation_outcome puts the deciding
# line in the report, and these checks pin what it may and may not carry.
check 'remediation_outcome reports an applied fix' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome '  applied: rolled back chaos-rollout/web to revision 1 (pod template restored)' ) )" \
  'applied: rolled back chaos-rollout/web to revision 1 (pod template restored)'
check 'remediation_outcome reports an applied rollback' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome '  rolled back: rolled chaos-rollout/web forward to revision 2 (pre-fix pod template restored)' ) )" \
  'rolled back: rolled chaos-rollout/web forward to revision 2 (pre-fix pod template restored)'
check 'remediation_outcome reports a refusal with its reason' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome '  skipped: revision 2 no longer exists; no write made' ) )" \
  'skipped: revision 2 no longer exists; no write made'
# The no-record line is the one kubeagent prints with the audit log's filesystem
# path in it. A path is a credential and the report is a forwarded artifact, so
# this case is answered by a sentence of the harness's own — the value is never
# echoed, only the fact.
check 'remediation_outcome names the no-record case' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome 'No applied remediation found in /tmp/tmp.EXAMPLE; nothing to roll back.' ) )" \
  'no applied remediation recorded; nothing to roll back'
check 'remediation_outcome never echoes the audit log path' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome 'No applied remediation found in /tmp/tmp.EXAMPLE; nothing to roll back.' ) \
      | grep -c 'tmp.EXAMPLE' || true )" 0
check 'remediation_outcome reports an error' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome '  ERROR: update deployment: etcdserver: request timed out' ) )" \
  'ERROR: update deployment: etcdserver: request timed out'
# The inverse of a recorded fix can fail to derive at all — an audit record written by
# a version that did not carry revisions, or an action kind with no inverse. kubeagent
# prints that on its own line, without the two-space outcome indent.
check 'remediation_outcome reports an inverse that could not be derived' \
  "$( ( set --; . chaos/run.sh
        remediation_outcome 'Cannot roll back the last applied fix (RolloutUndo chaos-rollout/web (Deployment)): no revision recorded' ) )" \
  'Cannot roll back the last applied fix (RolloutUndo chaos-rollout/web (Deployment)): no revision recorded'
check 'remediation_outcome says so when nothing was printed' \
  "$( ( set --; . chaos/run.sh; remediation_outcome 'Cluster: Healthy' ) )" \
  '(no outcome line printed)'

# --- the distro axis: --distro parses, validates, refuses and derives -------
# run.sh calls main() only on a direct execution, so sourcing it here runs its
# flag parser and its name derivation with no cluster and no docker. Unlike the
# probes above, these WANT positional parameters: `. chaos/run.sh <args>` sets
# the sourced script's argv explicitly, which is exactly what is under test.
distro_probe() {   # distro_probe <run.sh args...> -> "<rc>|<CLUSTER>|<CTX>|<OUT>|<COREDNS_BACKUP>|<K3S_IMAGE>"
  local args=("$@")
  (
    . chaos/run.sh "${args[@]}"
    printf '0|%s|%s|%s|%s|%s\n' "$CLUSTER" "$CTX" "$OUT" "$COREDNS_BACKUP" "${K3S_IMAGE:-}"
  ) 2>/dev/null || printf '%s|||||\n' "$?"
}

# The kind path is the one that gates every release. Every derived name on it
# must be byte-for-byte what it was before --distro existed.
check 'the default path is kind and derives the historical names' \
  "$(distro_probe)" \
  '0|kubeagent-chaos|kind-kubeagent-chaos|docs/testing/chaos-results.md|/tmp/kubeagent-chaos-coredns.yaml|'
check 'kind with a pinned minor derives the historical names' \
  "$(distro_probe --k8s-version v1.34)" \
  '0|kubeagent-chaos-v1-34|kind-kubeagent-chaos-v1-34|docs/testing/chaos-results-v1.34.md|/tmp/kubeagent-chaos-v1-34-coredns.yaml|'
check '--distro kind is the same thing spelled out' \
  "$(distro_probe --distro kind)" "$(distro_probe)"

# k3s derives its own everything. A shared name is a corrupted run: two reports
# overwrite each other, and two runs sharing a CoreDNS scratch file restore the
# wrong Corefile.
check 'k3s derives its own cluster, context, report and CoreDNS scratch names' \
  "$(distro_probe --distro k3s | cut -d'|' -f2-5)" \
  'kubeagent-chaos-k3s|k3d-kubeagent-chaos-k3s|docs/testing/chaos-results-k3s.md|/tmp/kubeagent-chaos-k3s-coredns.yaml'
check 'k3s with a pinned minor derives its own names too' \
  "$(distro_probe --distro k3s --k8s-version v1.33 | cut -d'|' -f2-5)" \
  'kubeagent-chaos-k3s-v1-33|k3d-kubeagent-chaos-k3s-v1-33|docs/testing/chaos-results-k3s-v1.33.md|/tmp/kubeagent-chaos-k3s-v1-33-coredns.yaml'
check 'the two distros collide on nothing for the same minor' \
  "$(comm -12 <(distro_probe --distro kind --k8s-version v1.34 | tr '|' '\n' | tail -n +2 | sort) \
              <(distro_probe --distro k3s  --k8s-version v1.34 | tr '|' '\n' | tail -n +2 | sort) \
     | grep -c . || true)" 0

# k3s always pins an image; kind without --k8s-version still lets kind choose,
# which is what the release gate has always run.
check 'k3s without a minor pins the newest supported image' \
  "$(distro_probe --distro k3s | cut -d'|' -f6)" \
  "$( ( set --; . chaos/versions.sh; chaos_k3s_image "$(chaos_newest)" ) )"
check 'k3s with a minor pins that minor'\''s image' \
  "$(distro_probe --distro k3s --k8s-version v1.33 | cut -d'|' -f6)" \
  "$( ( set --; . chaos/versions.sh; chaos_k3s_image v1.33 ) )"
check 'the kind path resolves no k3s image at all' \
  "$(distro_probe --k8s-version v1.34 | cut -d'|' -f6)" ''

# An unrecognised value is refused before anything is derived from it: it would
# otherwise become a cluster name, a context and a report path unchecked.
check 'an unknown --distro exits 2' "$(distro_probe --distro k3d | cut -d'|' -f1)" 2
check 'the unknown-distro message names what is supported' \
  "$( ( . chaos/run.sh --distro nope ) 2>&1 >/dev/null | grep -c 'supported: kind, k3s' || true)" 1
check 'an unsupported minor is still refused on the k3s path' \
  "$(distro_probe --distro k3s --k8s-version v9.99 | cut -d'|' -f1)" 2

# --context means "a cluster I did not create"; --distro means "create one".
# The fourth refusal joins the three portable mode already has.
check '--distro is refused with --context' \
  "$(distro_probe --context some-ctx --distro k3s | cut -d'|' -f1)" 2
check 'the refusal names both flags' \
  "$( ( . chaos/run.sh --context some-ctx --distro k3s ) 2>&1 >/dev/null \
      | grep -c -- '--context and --distro are mutually exclusive' || true)" 1
check '--context alone is still accepted' \
  "$(distro_probe --context some-ctx | cut -d'|' -f1,3,4)" \
  '0|some-ctx|docs/testing/chaos-results-portable.md'

# --distro manages the lifecycle of a cluster the harness owns, so it composes
# with the three flags that do the same.
check '--distro k3s composes with --recreate, --teardown and --k8s-version' \
  "$(distro_probe --distro k3s --k8s-version v1.33 --recreate --teardown | cut -d'|' -f1,2)" \
  '0|kubeagent-chaos-k3s-v1-33'

# cluster_tool is the single mapping from distro to the binary that creates and
# deletes the cluster: preflight requires it, teardown calls it, and CI installs
# it. Three copies of that answer is two too many.
check 'cluster_tool is kind by default' \
  "$( ( set --; . chaos/run.sh; cluster_tool ) )" kind
check 'cluster_tool is k3d on the k3s path' \
  "$( ( . chaos/run.sh --distro k3s; cluster_tool ) )" k3d

# --- redact_nodes: the portable-mode seam that keeps node AND context names
# out of $OUT ------------------------------------------------------------
# redact_nodes is a pure filter once NODE_NAMES and $CTX are set, so it is
# tested directly: source run.sh in a guarded subshell (same "set --
# before sourcing" pattern as requires_probe above), override the two
# globals, pipe text in, compare what comes out.
redact_probe() {   # redact_probe <node_names> <ctx> <input> -> <output>
  (
    # Capture the args before `set --` for the same reason requires_probe
    # does above: a bare `. chaos/run.sh` hands run.sh's own --flag parser
    # this subshell's positional parameters as if they were its argv.
    local node_names="$1" ctx="$2" input="$3"
    set --
    . chaos/run.sh
    NODE_NAMES="$node_names"
    CTX="$ctx"
    printf '%s' "$input" | redact_nodes
  )
}

check 'kind mode (NODE_NAMES empty) is a byte-identical passthrough, context included' \
  "$(redact_probe '' 'kind-kubeagent-chaos' \
      'log line naming kind-kubeagent-chaos and node worker-1')" \
  'log line naming kind-kubeagent-chaos and node worker-1'

check 'portable mode redacts a plain context name' \
  "$(redact_probe 'no-such-node-zzz' 'kind-kubeagent-chaos' \
      'cluster="kind-kubeagent-chaos"')" \
  'cluster="<context>"'

# A realistic managed-cluster context — the shape AWS's own docs use, with the
# twelve-digit example account 123456789012 — carries `:` and `/`. If it were
# fed to a regex instead of matched literally, `/` and `:` could break the
# pattern (or silently change what it matches).
check 'a context name with regex metacharacters is matched exactly, not as a pattern' \
  "$(redact_probe 'no-such-node-zzz' \
      'arn:aws:eks:us-east-1:123456789012:cluster/prod' \
      '"cluster": "arn:aws:eks:us-east-1:123456789012:cluster/prod"')" \
  '"cluster": "<context>"'

check 'node and context redaction compose' \
  "$(redact_probe 'worker-1' 'kind-kubeagent-chaos' \
      'context kind-kubeagent-chaos saw node worker-1')" \
  'context <context> saw node <node-1>'

# node1/node10: the short name must not eat the prefix of the long one and
# leave a stray "0" behind. This is the same longest-first guarantee
# portable_node_redaction has always relied on, now carried by
# redact_needles's single alternation instead of a hand-built sed script.
check 'a node name that is a prefix of another node name does not fracture it' \
  "$(redact_probe "$(printf 'node1\nnode10')" 'kind-kubeagent-chaos' \
      'saw node1 and node10')" \
  'saw <node-1> and <node-2>'

# The case a two-pass, context-first filter gets right: a node name ("prod")
# that is also a trailing substring of the context string, the shape a node
# pool named after its cluster produces. A single pass must get this right
# too, not just the direction a sequential filter happens to handle.
check 'a node name that is a substring of the context leaves no partial credential' \
  "$(redact_probe 'prod' \
      'arn:aws:eks:us-east-1:123456789012:cluster/prod' \
      'cluster: arn:aws:eks:us-east-1:123456789012:cluster/prod')" \
  'cluster: <context>'

# The mirror case: a bare context name (no `:`, no `/`, so nothing marks it
# as "not a node name") that is itself a substring of a node name — exactly
# GKE's own default node-name shape, gke-<cluster>-<pool>-<hash>. A
# context-first two-pass filter gets this backwards: the context replace
# runs first, consumes "prod" out of the middle of the node name, and the
# node-name pass — built from the original, now-stale name — never matches
# what is left, leaking "-worker-1.example.com" in the clear. This is the
# case redact_needles's single pass exists to fix.
check 'a context name that is a substring of a node name does not fracture the node name' \
  "$(redact_probe 'prod-worker-1.example.com' 'prod' \
      'log line naming node prod-worker-1.example.com in cluster')" \
  'log line naming node <node-1> in cluster'

# Two needles that overlap without either containing the other: context
# "abc-def" and node "def-ghi" share "def" at the boundary in "abc-def-ghi".
# Neither needle's FULL text survives — that is the promise redact_needles
# keeps unconditionally — but the comment above the function is explicit
# that a non-overlapping tail of the needle that loses the race (here,
# node "def-ghi"'s "-ghi") can remain. This locks in that documented,
# narrower residual rather than a stronger claim the design does not make.
check 'overlapping needles: neither survives whole, though a losing tail can remain' \
  "$(redact_probe 'def-ghi' 'abc-def' 'abc-def-ghi')" \
  '<context>-ghi'

check 'text containing neither name is unchanged' \
  "$(redact_probe 'no-such-node-zzz' 'no-such-context-zzz' \
      'nothing sensitive here')" \
  'nothing sensitive here'

# --- redact_nodes: a redaction failure withholds the section, logs to
# stderr, and never aborts the run ----------------------------------------
# Force redact_needles to fail (standing in for the python3 call misbehaving,
# which preflight/portable_preflight's python3 requirement should make
# impossible in practice) and check the three promises in run.sh's comment:
# no fallback to the raw text, a visible marker in its place, a line on
# stderr, and a zero exit so `set -e` never kills the caller's pipeline.
redact_failure_probe() {   # -> "<rc>|<stdout>|<stderr names the failure>"
  local tmpout tmperr rc
  tmpout="$(mktemp)"; tmperr="$(mktemp)"
  (
    set --
    . chaos/run.sh
    NODE_NAMES='no-such-node-zzz'
    CTX='top-secret-context'
    redact_needles() { return 1; }
    printf 'top-secret-context appears here\n' | redact_nodes
  ) >"$tmpout" 2>"$tmperr" && rc=0 || rc=$?
  printf '%s|%s|%s\n' "$rc" "$(cat "$tmpout")" \
    "$(grep -c 'redaction failed' "$tmperr" || true)"
  rm -f "$tmpout" "$tmperr"
}
redact_failure_result="$(redact_failure_probe)"
check 'a redaction failure does not abort the pipeline' \
  "$(printf '%s' "$redact_failure_result" | cut -d'|' -f1)" 0
check 'a redaction failure withholds the section, never the raw credential' \
  "$(printf '%s' "$redact_failure_result" | cut -d'|' -f2)" \
  '<redaction failed: section withheld>'
check 'a redaction failure logs a line on stderr' \
  "$(printf '%s' "$redact_failure_result" | cut -d'|' -f3)" 1

# --- corpus_row: the corpus's JSON encoder, from the real run.sh -------------
# corpus_row is pure: redacted plaintext block in, one JSON line out. Same
# guarded-source pattern as requires_probe above.
corpus_row_probe() {   # corpus_row_probe  (block on stdin) -> JSON line
  (
    set --
    . chaos/run.sh
    corpus_row
  )
}

row="$(
  {
    printf '%s\n' '5. coredns' 'coredns-corefile-broken' 'v1.34' 'kind' '0' 'false' ''
    printf 'PASS\tCluster: Degraded named (found)\n'
    printf 'FAIL\tscan exit code (got 0, want 2)\n'
  } | corpus_row_probe
)"
check 'corpus_row emits exactly one line' \
  "$(printf '%s\n' "$row" | wc -l | tr -d ' ')" 1
check 'corpus_row maps the seven fixed fields and the assertion tail' \
  "$(printf '%s' "$row" | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print("|".join([r["scenario"], r["fault"], r["k8s"], r["distro"], str(r["rc"]),
                str(len(r["assertions"])), str(r["skipped"]).lower(), r["skip_reason"]]))
')" '5. coredns|coredns-corefile-broken|v1.34|kind|0|2|false|'
check 'corpus_row keys follow the spec order' \
  "$(printf '%s' "$row" | python3 -c '
import json, sys
print(",".join(json.loads(sys.stdin.read()).keys()))
')" 'scenario,fault,k8s,distro,rc,assertions,skipped,skip_reason'
check 'corpus_row preserves the tab inside an assertion line' \
  "$(printf '%s' "$row" | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print("yes" if r["assertions"][0] == "PASS\tCluster: Degraded named (found)" else "no")
')" yes

skiprow="$(printf '%s\n' '2. certs' 'control-plane-cert-expiry' '' '' '0' 'true' \
  'control-plane certificate expiry cannot be forced quickly or safely' | corpus_row_probe)"
check 'corpus_row encodes a skipped scenario: empty axes, no assertions, the reason' \
  "$(printf '%s' "$skiprow" | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print("|".join([r["k8s"], r["distro"], str(r["skipped"]).lower(),
                str(len(r["assertions"])), r["skip_reason"]]))
')" '||true|0|control-plane certificate expiry cannot be forced quickly or safely'

# A malformed block (fewer than 7 header lines) is refused, not guessed at:
# the caller withholds the row. Same for a non-integer rc.
short_rc="$( (printf 'only\nthree\nlines\n' | corpus_row_probe) >/dev/null 2>&1 && echo 0 || echo 1 )"
check 'corpus_row refuses a block with fewer than 7 header lines' "$short_rc" 1
badrc_rc="$( (printf '%s\n' 't' 'f' '' '' 'NaN' 'false' '' | corpus_row_probe) >/dev/null 2>&1 && echo 0 || echo 1 )"
check 'corpus_row refuses a non-integer rc' "$badrc_rc" 1

# --- scenario_fault: the corpus's fault vocabulary ----------------------------
# The slug names the INJECTED FAULT, not the feature under test: six scenarios
# inject the literal same bad-image fault against six different features and
# share a slug — the scenario field is what tells their rows apart.
check 'scenario 05 fault slug is pinned by the spec' \
  "$( ( set --; . chaos/run.sh; scenario_fault 05_coredns ) )" coredns-corefile-broken
check 'a no-fault scenario says so instead of inventing a fault' \
  "$( ( set --; . chaos/run.sh; scenario_fault 21_controlplane ) )" no-fault-healthy-readyz
check 'the shared bad-image fault carries one slug across its six scenarios' \
  "$( ( set --; . chaos/run.sh
       for s in 09_rollout 12_watch 13_slo 14 15_multicluster 23_pagerduty; do
         scenario_fault "$s"
       done | sort -u ) )" deployment-bad-image-tag
check 'an unknown scenario name yields the sentinel and does not fail' \
  "$( ( set --; . chaos/run.sh; scenario_fault 99_nope && echo "|rc0" ) )" 'unknown-scenario
|rc0'

# Completeness: every scenario run_scenarios names must map to a real slug.
# The list is extracted from run.sh's own text, so adding a 24th scenario
# without a fault slug fails CI here rather than writing "unknown-scenario"
# into a published corpus.
fault_completeness="$(
  ( set --
    . chaos/run.sh
    names="$(sed -n 's/^  local all=(\(.*\))$/\1/p' chaos/run.sh)"
    [ -n "$names" ] || { echo 'LIST-NOT-FOUND'; exit 0; }
    bad=0
    for s in $names; do
      slug="$(scenario_fault "$s")"
      case "$slug" in
        ''|unknown-scenario) echo "NO-SLUG:$s"; bad=1 ;;
      esac
    done
    [ "$bad" = 0 ] && echo OK
  )
)"
check 'every scenario in run_scenarios has a fault slug' "$fault_completeness" OK
check 'run_scenarios names 23 scenarios' \
  "$(sed -n 's/^  local all=(\(.*\))$/\1/p' chaos/run.sh | wc -w | tr -d ' ')" 23

printf '\n%s\n' "$([ "$fails" -eq 0 ] && echo 'assert-selftest: all checks passed' \
                                     || echo "assert-selftest: $fails check(s) failed")"
[ "$fails" -eq 0 ]
