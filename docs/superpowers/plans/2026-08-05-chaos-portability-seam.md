# Chaos Harness Portability Seam — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `./chaos/run.sh --context <ctx>` run the namespaced-only subset of the chaos suite against a cluster the harness did not create, refusing every scenario that would write a cluster-scoped object or touch a node, and naming each skip and its reason in the assertion summary so a partial run can never be mistaken for a full green one.

**Architecture:** Three mechanisms, layered. (1) `chaos/assert.sh` gains a skip log — a file, beside `$ASSERTLOG`, for the same subshell reason — plus `assert_skip` and `scenario_title`, all cluster-free and unit-testable by `chaos/assert-selftest.sh`. (2) `chaos/run.sh` gains a closed six-name capability vocabulary, a `requires <capability>` guard clause that composes `assert_skip` with `record`, and the probes that decide which capabilities the target cluster has. (3) `--context <ctx>` selects a portable setup path — its own preflight, its own header, its own baseline, its own sweep — in place of the kind create/Calico path. Twelve scenarios grow a one-line `requires ... || return 0` guard above their existing `log` line; nothing else about any scenario body changes.

**Tech Stack:** Bash 5 (`set -euo pipefail`), `kubectl`, `python3` for JSON parsing, `curl`. **No Go code is touched in this slice at all.**

## Global Constraints

Every task's requirements implicitly include this section. Copied verbatim from the spec and the project's standing rules.

- Every commit needs a `Signed-off-by` trailer matching its author (`git commit -s`) because `main` enforces DCO. Verify with `bash scripts/dco-check.sh main HEAD`. **No `Co-Authored-By` trailer and no AI attribution anywhere** — commits, code, comments, docs, changelog.
- **NO NEW DEPENDENCY and NO NEW BINARY REQUIREMENT.** `go.mod` and `go.sum` must not change (no Go code is touched). Portable mode's binary list **shrinks** — it drops `kind` and `docker` — and must not grow.
- **No chaos helper may ever return non-zero for a recorded outcome.** Under `set -e` a failing assertion must let the remaining scenarios run and surface only at the end in the exit code. `assert_skip` follows the same rule.
- **`assert_summary`'s exit status stays the gate:** non-zero if and only if an assertion failed. A skip is never a failure.
- **The assertion count stays 134** and does not move in `CLAUDE.md:264`, `chaos/README.md:148`, `website/docs/compatibility.md:123` or `website/docs/roadmap.md:533` — those are the only four places the number appears, and it appears in neither `chaos/run.sh` nor `chaos/assert-selftest.sh`. New coverage goes in `chaos/assert-selftest.sh`, whose own count is not published. If a task finds itself changing 134, that is a defect in the task, not a documentation update.
- **`scenario_01_etcd` stays LAST in `run_scenarios()`**, and the `all=(...)` list itself does not change.
- **No secrets, credentials, private IPs or internal hostnames anywhere** — including the results file, CI artifacts, README examples and every doc example. RFC 5737 addresses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); RFC 2606 domains (`example.com`, `example.org`, `example.net`). **Kubeconfig paths and kubeconfig CONTEXT NAMES are credentials:** a context name may appear on **stderr or the console** and must **NEVER** reach `$OUT`. Nothing emitted may carry more than `scheme://host`.
- **Never expose API keys to the shell.** The harness runs with `ANTHROPIC_API_KEY` unset; `explain_flag()` already gates on its presence and **must not change**.
- kubeagent is **read-only toward the cluster**; the chaos **harness** deliberately is not — it injects outages. Never blur the two, and never blur "read-only" into "makes no external calls": read-only describes cluster operations, making no LLM call is a separate, stronger claim.
- **The six versioned JSON documents do not move.** No `schemaVersion` bump, no schema regeneration, no `internal/jsonschema` change.
- `internal/report/testdata/golden-scan.txt` stays byte-identical. Do **NOT** regenerate the demo GIF or `website/docs/quickstart.md`. The Helm chart, `internal/rbacprofile`'s `Feature` table and every generated RBAC manifest are untouched.
- **TDD:** write the failing selftest check first, run it and watch it fail, then implement. Where a change is not selftestable (a `requires` guard on a scenario body, a doc paragraph), the verification step is a real run, named in the task.
- **`shellcheck` is NOT installed on this machine.** Do not instruct anyone to run it. The available syntax gate is `bash -n chaos/run.sh` and `bash -n chaos/assert.sh`, plus `bash chaos/assert-selftest.sh`.
- A live kind cluster named `kubeagent-chaos` (context `kind-kubeagent-chaos`) is up. Per-task verification uses `./chaos/run.sh --only NN --out <scratch path>` against it — seconds, not the 40-minute full gate.
- Work happens on branch `chaos-portability-seam`, already cut off `main` at `21a3dae`. Never commit to `main`.

---

## File Structure

| File | Change |
|---|---|
| `chaos/assert.sh` | `$SKIPLOG` in `assert_init` and its `EXIT` trap; `assert_skip <label> <reason>`; `scenario_title <func-name>`; `assert_summary` gains a third bullet, a fenced skip list and a new console line. Stays cluster-free and report-free. |
| `chaos/assert-selftest.sh` | New comment-delimited blocks covering `assert_skip`, the new summary output, `scenario_title` over all 23 real names, and `requires`. |
| `chaos/run.sh` | `--context` parsing + `PORTABLE` + three refusals; the capability vocabulary, `capability_add`, `requires`, `probe_capabilities`; `portable_preflight`, `portable_header`, `portable_sweep`; a branched `main()`; twelve `requires` guards; scenario 02 rewired through `assert_skip`; the two context-name leaks in scenarios 15 and 19 closed. `main` is invoked only on direct execution so the selftest can source the file. |
| `chaos/README.md` | A `## Portable mode` section; the two now-false "never reads your current kubecontext" claims amended; the Assertions section updated for skip accounting. |
| `website/docs/compatibility.md` | The "not gated in CI" paragraph amended — **without** claiming CI coverage this slice does not add. |
| `website/docs/roadmap.md` | The trailing cross-distro sentence in the Theme H slice 8 paragraph amended. |
| `CHANGELOG.md` | Entries under `## [Unreleased]`. |

**Not touched:** any Go file, `go.mod`, `go.sum`, `CLAUDE.md`, `.github/workflows/chaos-matrix.yml`, `chaos/versions.env`, `chaos/versions.sh`, `chaos/manifests/**`, `deploy/**`, `website/docs/quickstart.md`.

**The nightly matrix workflow parses none of this.** `.github/workflows/chaos-matrix.yml` references `run.sh` and the report path in exactly three places — the invocation, the `rep=` report path, and the artifact upload — and never reads the `assertions: N run, M failed` console line. Changing that line's format (Task 1) breaks no CI parser. Do not "fix" the workflow to match; leaving it alone is correct.

**Two design decisions the spec leaves implicit — settled here, do not reopen:**

1. **`scenario_title` lives in `chaos/assert.sh`, not `chaos/run.sh`.** The spec's file table lists it under `run.sh`, but the spec's testing section requires it be unit-testable, and `chaos/assert-selftest.sh` sources `assert.sh` only. It is a pure string function about a skip's label, so it belongs beside `assert_skip`, and putting it there keeps `assert.sh` cluster-free.
2. **`chaos/run.sh`'s final line becomes `if [ "${BASH_SOURCE[0]}" = "${0}" ]; then main; fi`.** This is what lets `assert-selftest.sh` source `run.sh` to exercise `requires` with no cluster. Direct execution (`./chaos/run.sh`, `bash chaos/run.sh`) is unaffected — both give `$0` equal to `${BASH_SOURCE[0]}`.

---

## The capability vocabulary (referenced by several tasks)

Six names, a closed set. Two are **policy** — a statement about what the harness *may* do, not what it *could* do. Four are **probed** — facts about the target cluster.

| Capability | Kind | Decided by | Guards scenarios |
|---|---|---|---|
| `node_exec` | policy | present only when the harness created the cluster | 01, 11 |
| `cluster_write` | policy | present only when the harness created the cluster | 03, 05, 16, 17, 20, 22 |
| `clean_baseline` | probe | the baseline scan reported `Cluster: Healthy` **and** `No issues found.` | 08 |
| `no_loadbalancer` | probe | a LoadBalancer Service in a temporary `chaos-probe` namespace got no `status.loadBalancer.ingress` within 30s | 06 |
| `no_metrics_server` | probe | the `v1beta1.metrics.k8s.io` APIService is absent | 18 |
| `netpol_enforced` | probe | a DaemonSet named `calico-node`, `cilium`, `weave-net`, `kube-router` or `antrea-agent` exists in `kube-system` | 04 |

**All six are present on kind**, which is why the gate path's assertion count stays 134 and why every probe is exercised on the path that gates releases.

Expected run shapes: kind portable run = **14 run / 9 skipped** (02 + 01,11 + 03,05,16,17,20,22). A managed cluster with a LoadBalancer provider, metrics-server, an enforcing CNI and a dirty baseline = **11 run / 12 skipped** — a prediction, not a measurement.

---

### Task 1: `assert.sh` — the skip log

**Files:**
- Modify: `chaos/assert.sh` (`assert_init` at 21-25, `assert_summary` at 88-104; new `assert_skip`)
- Test: `chaos/assert-selftest.sh` (append new blocks before the final tally)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `SKIPLOG` — a global holding a file path, initialised by `assert_init`, removed by its `EXIT` trap.
  - `assert_skip <label> <reason>` — prints `SKIP: <label> — <reason>` to stdout, appends `SKIP\t<label> — <reason>` to `$SKIPLOG`, always `return 0`. Never touches `$ASSERTLOG`, never touches `$OUT`, never touches the cluster.
  - `assert_summary <report-file>` — unchanged signature; now also writes `- scenarios skipped: <n>` and, when `n > 0`, a fenced block of the skip lines; its console line becomes `assertions: <n> run, <m> failed; <k> scenario[s] skipped`. Exit status is unchanged: non-zero iff an assertion failed.

- [ ] **Step 1: Write the failing selftest checks**

Append to `chaos/assert-selftest.sh`, immediately **before** the final `printf`/`[ "$fails" -eq 0 ]` tally at the end of the file:

```bash
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
```

Note the literal TAB inside the `grep -c '^SKIP	2. certs …'` patterns — it must be a real tab character, matching the `\t` `assert_skip` writes.

- [ ] **Step 2: Run the selftest and watch it fail**

```bash
bash chaos/assert-selftest.sh
```

Expected: FAIL — `assert_skip: command not found` (and, under `set -u`, an unbound `SKIPLOG`). The script's tail must report `assert-selftest: N check(s) failed` and exit non-zero.

- [ ] **Step 3: Extend `assert_init` and its trap**

Replace `assert_init` in `chaos/assert.sh`:

```bash
assert_init() {
  ASSERTLOG="${ASSERTLOG:-$(mktemp)}"
  SKIPLOG="${SKIPLOG:-$(mktemp)}"
  : > "$ASSERTLOG"
  : > "$SKIPLOG"
  trap 'rm -f "${ASSERTLOG:-}" "${SKIPLOG:-}"' EXIT
}
```

- [ ] **Step 4: Add `assert_skip`**

Add immediately after `_assert_record` (so it sits with the recording helpers, above `expect_eq`):

```bash
# assert_skip <label> <reason> — a scenario that did not run, and why.
#
# A skip is not an assertion and not a failure: it never touches $ASSERTLOG and
# it never moves the exit code. It is a STATED GAP, which is the whole point —
# a run that quietly omitted nine scenarios would otherwise look exactly like a
# full green one.
#
# $SKIPLOG is a file for the same reason $ASSERTLOG is: the caller may be inside
# a pipeline, a pipeline runs in a subshell, and a counter incremented there is
# discarded when the block ends.
#
# Like every helper here it returns 0, so `set -e` cannot turn a recorded
# outcome into an aborted run.
assert_skip() {
  printf 'SKIP: %s — %s\n' "$1" "$2"
  printf 'SKIP\t%s — %s\n' "$1" "$2" >> "$SKIPLOG"
  return 0
}
```

- [ ] **Step 5: Extend `assert_summary`**

Replace `assert_summary` in `chaos/assert.sh`:

```bash
# assert_summary <report-file> — append the tally to the report, print it to the
# console, and return non-zero if any assertion failed. That return status is
# what makes ./chaos/run.sh a gate rather than a report.
#
# Skipped scenarios are counted and listed but never change the status: a skip
# is a declared gap, not a failure. It is reported unconditionally, including
# when it is zero, so "0 scenarios skipped" is a claim the run makes out loud
# rather than a silence the reader has to interpret.
assert_summary() {
  local out="$1" total failed skipped
  total="$(wc -l < "$ASSERTLOG" | tr -d ' ')"
  failed="$(grep -c '^FAIL' "$ASSERTLOG" || true)"
  skipped="$(wc -l < "$SKIPLOG" | tr -d ' ')"
  {
    printf '\n## Assertion summary\n\n'
    printf -- '- assertions run: %s\n' "$total"
    printf -- '- failed: %s\n' "$failed"
    printf -- '- scenarios skipped: %s\n' "$skipped"
    if [ "$failed" -gt 0 ]; then
      printf '\n```text\n'
      grep '^FAIL' "$ASSERTLOG"
      printf '```\n'
    fi
    if [ "$skipped" -gt 0 ]; then
      printf '\n```text\n'
      cat "$SKIPLOG"
      printf '```\n'
    fi
  } >> "$out"
  printf '\nassertions: %s run, %s failed; %s scenario%s skipped\n' \
    "$total" "$failed" "$skipped" "$([ "$skipped" -eq 1 ] || echo s)"
  [ "$failed" -eq 0 ]
}
```

- [ ] **Step 6: Run the selftest and watch it pass**

```bash
bash chaos/assert-selftest.sh
```

Expected: `assert-selftest: all checks passed`, exit 0, in well under a second.

- [ ] **Step 7: Confirm the gate path still reports what it always did**

```bash
bash -n chaos/assert.sh && bash -n chaos/run.sh
./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t1.md
```

Expected: exit 0, and the console tail reads `assertions: 8 run, 0 failed; 0 scenarios skipped`.
(`--only 21` is scenario 21, control-plane — it needs no cluster mutation and finishes in seconds. Eight is the three baseline assertions plus scenario 21's five, measured on this cluster before the branch started; what this step checks is the **shape** of the new line and that the run is still green.)

- [ ] **Step 8: Commit**

```bash
git add chaos/assert.sh chaos/assert-selftest.sh
git commit -s -m "chaos: count and name skipped scenarios in the assertion summary"
```

---

### Task 2: `assert.sh` — `scenario_title`

**Files:**
- Modify: `chaos/assert.sh` (add beside `assert_skip`)
- Test: `chaos/assert-selftest.sh`

**Interfaces:**
- Consumes: nothing.
- Produces: `scenario_title <function-name>` — pure, prints one line. `scenario_05_coredns` → `5. coredns`; `scenario_23_pagerduty` → `23. pagerduty`; `scenario_14` (no trailing word) → `14.`. Used by `requires` in Task 3 as the skip label, so a skip's heading is derived from the scenario's own function name and cannot drift from it.

- [ ] **Step 1: Write the failing selftest checks**

Append to `chaos/assert-selftest.sh`, before the final tally:

```bash
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
```

- [ ] **Step 2: Run the selftest and watch it fail**

```bash
bash chaos/assert-selftest.sh
```

Expected: FAIL — `scenario_title: command not found`, 23 failed checks.

- [ ] **Step 3: Implement `scenario_title`**

Add to `chaos/assert.sh`, immediately after `assert_skip`:

```bash
# scenario_title <function name> — the report heading for a skipped scenario,
# derived from the scenario's own function name: scenario_05_coredns -> "5.
# coredns", scenario_14 -> "14.". Deriving it means a skip heading can never
# drift from the scenario it names, which passing the title in as a second
# argument would eventually allow.
#
# 10# forces base 10 on the number: a leading-zero numeral is octal to $(( )),
# so a bare $((08)) is an error rather than 8 — the same trap run.sh's --only
# normalization already documents.
#
# It lives here, not in run.sh, so chaos/assert-selftest.sh can exercise it with
# no cluster; it touches nothing outside its own arguments.
scenario_title() {
  local rest="${1#scenario_}" num word
  case "$rest" in
    *_*) num="${rest%%_*}"; word="${rest#*_}" ;;
    *)   num="$rest";       word="" ;;
  esac
  printf '%s.%s\n' "$((10#$num))" "${word:+ $word}"
}
```

- [ ] **Step 4: Run the selftest and watch it pass**

```bash
bash chaos/assert-selftest.sh
```

Expected: `assert-selftest: all checks passed`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add chaos/assert.sh chaos/assert-selftest.sh
git commit -s -m "chaos: derive a skipped scenario's heading from its function name"
```

---

### Task 3: `run.sh` — the capability vocabulary and `requires`

**Files:**
- Modify: `chaos/run.sh` — add a global `CAPS` beside the other globals at line 13; add the capability block immediately after `record()` (which ends just before `teardown()` at line 288); change the final `main` invocation
- Test: `chaos/assert-selftest.sh`

**Interfaces:**
- Consumes: `assert_skip` and `scenario_title` (Tasks 1-2); `record <title> <verdict>` (existing, reads `$OUT` as a global).
- Produces:
  - `CAPS` — a space-separated string of available capability names. Empty by default.
  - `capability_reason <name>` — prints the one canonical reason string for a known capability and returns 0; prints nothing and returns 1 for an unknown name.
  - `capability_add <name>` — adds a known capability to `CAPS` if absent; exits 2 on an unknown name.
  - `requires <name>` — the guard clause. Returns 0 when available. Otherwise records the skip in `$SKIPLOG`, on the console and in `$OUT`, and returns 1. Exits 2 on an unknown name.
  - `chaos/run.sh` is sourceable: `main` runs only on direct execution.

- [ ] **Step 1: Make `run.sh` sourceable — do this BEFORE anything else in this task**

This is an enabling change, not the behaviour under test, and it comes first for a
concrete reason: **`chaos/run.sh` today ends in a bare `main`, so sourcing it runs
the entire 23-scenario suite against the live cluster.** Writing the selftest
checks first and running them would break CoreDNS, taint a node and leave
namespaces behind before failing. Close that door, then do TDD.

Replace the final line of `chaos/run.sh` (currently a bare `main`):

```bash
# main() runs only on a direct execution. chaos/assert-selftest.sh sources this
# file to exercise the pure helpers above — the capability table, requires — with
# no cluster and no docker, which is the only way those get a test at all.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then main; fi
```

Verify the guard both ways before writing a single check:

```bash
bash -n chaos/run.sh
timeout 10 bash -c '. chaos/run.sh; echo "sourced without running main"'
```

Expected: `sourced without running main`, promptly, with no build and no
`=== baseline healthy scan ===` banner. If the suite starts, the guard is wrong —
stop and fix it before continuing.

- [ ] **Step 2: Write the failing selftest checks**

Append to `chaos/assert-selftest.sh`, before the final tally:

```bash
# --- requires: the capability gate, from the real run.sh ---------------------
# run.sh calls main() only when executed directly, so it can be sourced here to
# exercise its pure helpers with no cluster. Each probe runs in a SUBSHELL:
# sourcing run.sh sets the harness's own globals, and they must not leak into
# the checks around this block.
requires_probe() {   # requires_probe <available caps> <capability> -> "<rc>|<skip lines>|<report sections>"
  (
    . chaos/run.sh
    ASSERTLOG="$(mktemp)"; SKIPLOG="$(mktemp)"; OUT="$(mktemp)"
    : > "$ASSERTLOG"; : > "$SKIPLOG"; : > "$OUT"
    trap 'rm -f "$ASSERTLOG" "$SKIPLOG" "$OUT"' EXIT
    CAPS="$1"
    scenario_99_probe() { requires "$2" || return 1; return 0; }
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
unknown_rc="$(
  (
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
  "$( ( . chaos/run.sh; capability_reason cluster_write ) )" \
  'writes cluster-scoped objects, which the harness will not do on a cluster it does not own'
check 'capability_reason rejects an unknown name' \
  "$( ( . chaos/run.sh; capability_reason nope >/dev/null ) && echo 0 || echo 1 )" 1

# capability_add is idempotent and validates, so a typo cannot silently switch
# a scenario on.
check 'capability_add is idempotent' \
  "$( ( . chaos/run.sh; CAPS=''; capability_add node_exec; capability_add node_exec; printf '%s' "$CAPS" ) )" \
  'node_exec'
check 'capability_add exits 2 on an unknown name' \
  "$( ( . chaos/run.sh; capability_add nope ) >/dev/null 2>&1 && echo 0 || echo $? )" 2
```

- [ ] **Step 3: Run the selftest and watch it fail**

```bash
bash chaos/assert-selftest.sh
```

Expected: FAIL, in under a second — `requires: command not found` and `capability_reason: command not found`, so every check in the new block mismatches. It must not build anything and must not touch the cluster; if it does, Step 1's guard is not in place.

- [ ] **Step 4: Add the `CAPS` global**

Change line 13 of `chaos/run.sh` from:

```bash
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""; K8S_VERSION=""; KIND_IMAGE=""
```

to:

```bash
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""; K8S_VERSION=""; KIND_IMAGE=""
CAPS=""   # the capabilities this run has; see the capability block below
```

- [ ] **Step 5: Add the capability block**

Insert into `chaos/run.sh` immediately after `record()`'s closing brace and immediately before `teardown()`:

```bash
# --- capabilities -----------------------------------------------------------
#
# A scenario declares what it needs; the run decides what it has. The
# vocabulary is a CLOSED SET of six names, each with exactly one reason string,
# so two guards on the same capability can never describe it differently in two
# reports.
#
# Two of them are POLICY, not probe. The question `cluster_write` and
# `node_exec` answer is not whether the harness COULD write a cluster-scoped
# object or shell into a node — with an admin kubeconfig against a managed
# cluster it very often could — but whether it MAY. On a cluster the harness
# created and can delete, it may. On a cluster it merely has credentials for, it
# may not, and the refusal is the safety property this whole seam exists for.
#
# The other four are facts about the target cluster, decided by probe_capabilities
# and by the baseline scan.

# capability_reason <name> — the one canonical reason a scenario is skipped for
# want of this capability. Returns 1 for a name outside the vocabulary.
capability_reason() {
  case "$1" in
    node_exec)         printf 'needs shell access to a node container, which exists only on a cluster the harness created\n' ;;
    cluster_write)     printf 'writes cluster-scoped objects, which the harness will not do on a cluster it does not own\n' ;;
    clean_baseline)    printf 'asserts whole-cluster health, which is only meaningful on a cluster that reported none before the run\n' ;;
    no_loadbalancer)   printf 'asserts a LoadBalancer Service never gets an address, which is false on a cluster with a provider\n' ;;
    no_metrics_server) printf 'asserts the structural-rules-only path, which metrics-server would take instead\n' ;;
    netpol_enforced)   printf 'needs a CNI that enforces NetworkPolicy\n' ;;
    *) return 1 ;;
  esac
}

# capability_add <name> — mark a capability available for this run. Idempotent,
# and it validates: a typo here would silently switch a scenario ON, which is
# the failure mode this seam must never have.
capability_add() {
  capability_reason "$1" >/dev/null || {
    printf 'capability_add: unknown capability %s\n' "$1" >&2
    exit 2
  }
  case " $CAPS " in *" $1 "*) ;; *) CAPS="${CAPS:+$CAPS }$1" ;; esac
}

# requires <capability> — the guard clause at the top of a scenario body:
#
#   scenario_05_coredns() {
#     requires cluster_write || return 0
#     log "scenario 5: ..."
#
# Returns 0 when the capability is available and the scenario proceeds
# unchanged. Otherwise it records the skip three ways — in $SKIPLOG so the
# summary counts it, in $OUT so the report names it, and on the console so a
# watching operator sees it — and returns 1, so `|| return 0` leaves the
# scenario without having touched the cluster.
#
# An unknown capability name EXITS rather than skipping. A silent skip on a
# typo'd guard would look exactly like a passing run, which is precisely the
# defect this seam was built to remove.
requires() {
  local cap="$1" reason title
  reason="$(capability_reason "$cap")" || {
    printf 'requires: unknown capability %s (called from %s)\n' "$cap" "${FUNCNAME[1]}" >&2
    exit 2
  }
  case " $CAPS " in *" $cap "*) return 0 ;; esac
  title="$(scenario_title "${FUNCNAME[1]}")"
  assert_skip "$title" "$reason"
  printf 'Skipped: %s\n' "$reason" | record "$title" "skipped ($reason)"
  return 1
}
```

- [ ] **Step 6: Run the selftest and watch it pass**

```bash
bash -n chaos/run.sh && bash chaos/assert-selftest.sh
```

Expected: `assert-selftest: all checks passed`, exit 0, in under a second — if it takes longer, sourcing is still running `main` and Step 3 is wrong.

- [ ] **Step 7: Confirm direct execution still runs the suite**

```bash
./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t3.md
```

Expected: exit 0, scenario 21 runs, console tail `assertions: 8 run, 0 failed; 0 scenarios skipped`.

- [ ] **Step 8: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "chaos: a capability vocabulary and the requires guard clause"
```

---

### Task 4: `run.sh` — `--context`, portable mode, and the three refusals

**Files:**
- Modify: `chaos/run.sh` (globals line 13-14; flag loop 15-24; a new block between the `--only` normalization at line 29 and the version-axis block at line 48)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `PORTABLE` — `1` when `--context` was given, `0` otherwise. Every later task branches on it.
  - `CONTEXT` — the raw `--context` value; `CTX` is set from it.
  - The portable report path default `docs/testing/chaos-results-portable.md`.

- [ ] **Step 1: Add the globals**

Change lines 13-14 of `chaos/run.sh` to:

```bash
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""; K8S_VERSION=""; KIND_IMAGE=""
CAPS=""   # the capabilities this run has; see the capability block below
PORTABLE=0; CONTEXT=""   # --context selects portable mode; see the block below
```

- [ ] **Step 2: Add the flag**

Add one case to the flag loop, after `--k8s-version`:

```bash
    --context) CONTEXT="$2"; shift ;;
```

- [ ] **Step 3: Add the portable-mode block**

Insert into `chaos/run.sh` between the `--only` zero-pad normalization (line 29) and the `. "$ROOT/chaos/versions.sh"` source line, so the refusals fire before any name is derived — the same "validate before deriving" order the version-axis comment above already establishes:

```bash
# Portable mode. --context runs the namespaced-only subset of the suite against
# a cluster the harness did NOT create: it creates and deletes chaos-* namespaces
# and nothing else, and every scenario that would write a cluster-scoped object
# or shell into a node refuses to run.
#
# Three flags are REFUSED rather than ignored. All three manage a kind cluster's
# lifecycle or its node image, and silently accepting one against someone else's
# production cluster is exactly the trap this mode exists to avoid. They fire
# here, before the version axis derives any name, so an operator who typed a
# contradiction learns it before docker is touched.
if [ -n "$CONTEXT" ]; then
  PORTABLE=1
  if [ "$RECREATE" = 1 ]; then
    echo "--context and --recreate are mutually exclusive: the harness will not delete and rebuild a cluster it does not own" >&2
    exit 2
  fi
  if [ "$TEARDOWN" = 1 ]; then
    echo "--context and --teardown are mutually exclusive: the harness deletes only clusters it created" >&2
    exit 2
  fi
  if [ -n "$K8S_VERSION" ]; then
    echo "--context and --k8s-version are mutually exclusive: the version axis picks a kind node image, which says nothing about a cluster that already exists" >&2
    exit 2
  fi
  CTX="$CONTEXT"
  : "${OUT:=docs/testing/chaos-results-portable.md}"
fi
```

`CLUSTER` and `COREDNS_BACKUP` keep their historical values in portable mode and are simply unused: every function that reads them (`create_cluster`, `teardown`, `preload_*`, `install_calico`, `scenario_05_coredns`) is either off the portable path or refused by `cluster_write`.

**`preflight()` is not modified.** Portable mode does not call it; Task 5 gives portable mode its own, shorter binary list. That is how the requirement "the binary list shrinks and must not grow" is met without touching the kind path.

- [ ] **Step 4: Verify the refusals**

```bash
bash -n chaos/run.sh
for f in --recreate --teardown; do
  ./chaos/run.sh --context kind-kubeagent-chaos $f; echo "rc=$?"
done
./chaos/run.sh --context kind-kubeagent-chaos --k8s-version 1.34; echo "rc=$?"
```

Expected: three refusals, each printing its own named reason on **stderr** and `rc=2`. Nothing is built and no cluster is touched.

- [ ] **Step 5: Verify the kind path is unchanged**

```bash
./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t4.md
```

Expected: exit 0 — `--context` absent, so `PORTABLE=0` and nothing about the run changed.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: --context selects portable mode, and refuses the three kind-lifecycle flags"
```

---

### Task 5: `run.sh` — `portable_preflight` and `portable_sweep`

**Files:**
- Modify: `chaos/run.sh` (add both functions immediately after `preflight()`, which ends at line 116)

**Interfaces:**
- Consumes: `PORTABLE`, `CTX` (Task 4); `log()` (existing).
- Produces:
  - `portable_preflight` — four checks, each exiting 1 with a named reason on stderr. Called by `main()` in Task 6 in place of `preflight`.
  - `portable_sweep` — best-effort deletion of leftover `chaos-*` namespaces. Called by `main()` in Task 6 in place of the teardown branch. Never returns non-zero.

- [ ] **Step 1: Write `portable_preflight`**

Insert into `chaos/run.sh` immediately after `preflight()`:

```bash
# portable_preflight — what the harness must know before it touches a cluster it
# does not own. Every one of these exits rather than degrading: a chaos run that
# started against the wrong cluster cannot be undone by noticing later.
#
# The binary list is SHORTER than preflight()'s, not longer: portable mode
# creates no kind cluster and side-loads no image, so kind and docker are not
# needed at all.
#
# The context name appears on stderr here and nowhere else. It is a credential
# under this project's rules — on a managed cluster it is routinely an ARN or a
# project/region path — but stderr is the operator's own channel, read by the
# person who typed the name, and a preflight failure that will not say which
# cluster it means is not actionable. It never reaches $OUT.
portable_preflight() {
  local b existing probe=chaos-preflight

  for b in kubectl go curl python3; do
    command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
  done

  kubectl config get-contexts "$CTX" >/dev/null 2>&1 || {
    printf 'no such context in the kubeconfig: %s\n' "$CTX" >&2
    printf 'List them with: kubectl config get-contexts\n' >&2
    exit 1
  }

  kubectl --context "$CTX" version -o json >/dev/null 2>&1 || {
    printf 'context %s exists but the cluster did not answer\n' "$CTX" >&2
    printf 'The credentials may be expired, or the API server may be unreachable from here.\n' >&2
    exit 1
  }

  # Debris from an aborted run, or a second run already in progress. Either way
  # the scenarios below would collide with it, and deleting someone else's
  # namespace unasked is not the harness's call to make.
  existing="$(kubectl --context "$CTX" get ns -o name 2>/dev/null \
    | sed 's|^namespace/||' | grep '^chaos-' | tr '\n' ' ' || true)"
  existing="${existing% }"
  if [ -n "$existing" ]; then
    {
      printf 'refusing to start: chaos-* namespaces already exist on the target cluster:\n'
      printf '  %s\n' "$existing"
      printf 'They are debris from an aborted run, or another run is in progress. Delete them first:\n'
      printf '  kubectl --context %s delete ns %s\n' "$CTX" "$existing"
    } >&2
    exit 1
  fi

  # A round trip, not a SelfSubjectAccessReview: what matters is that this
  # identity can actually create AND delete a namespace here, which is the only
  # write the portable subset performs.
  log "portable preflight: namespace create/delete round trip"
  kubectl --context "$CTX" create ns "$probe" >/dev/null 2>&1 || {
    printf 'refusing to start: cannot create a namespace on the target cluster.\n' >&2
    printf 'The portable subset creates and deletes chaos-* namespaces; it needs that permission.\n' >&2
    exit 1
  }
  kubectl --context "$CTX" delete ns "$probe" --wait=true --timeout=120s >/dev/null 2>&1 || {
    printf 'refusing to start: created namespace %s but could not delete it again.\n' "$probe" >&2
    printf 'Delete it by hand before re-running.\n' >&2
    exit 1
  }
}
```

- [ ] **Step 2: Write `portable_sweep`**

Insert immediately after `portable_preflight`:

```bash
# portable_sweep — delete any chaos-* namespace still standing at the end of a
# portable run, so a scenario that failed part way through does not leave a
# broken workload on a cluster the harness does not own.
#
# Best-effort by design, and it must never return non-zero: it runs before
# assert_summary, and a sweep that aborted the script would swallow the very
# exit code the run exists to produce.
portable_sweep() {
  log "sweep leftover chaos-* namespaces"
  local ns
  for ns in $(kubectl --context "$CTX" get ns -o name 2>/dev/null \
                | sed 's|^namespace/||' | grep '^chaos-' || true); do
    kubectl --context "$CTX" delete ns "$ns" --wait=false >/dev/null 2>&1 \
      || log "sweep: could not delete namespace $ns"
  done
  return 0
}
```

- [ ] **Step 3: Verify both by hand against the live kind cluster**

Neither is wired into `main()` yet (that is Task 6), so exercise them by sourcing:

```bash
bash -n chaos/run.sh
bash -c '. chaos/run.sh; CTX=no-such-context-xyz; portable_preflight' ; echo "rc=$?"
bash -c '. chaos/run.sh; CTX=kind-kubeagent-chaos; portable_preflight && echo "preflight ok"'
bash -c '. chaos/run.sh; CTX=kind-kubeagent-chaos; portable_sweep; echo "sweep rc=$?"'
```

Expected: the first prints `no such context in the kubeconfig: no-such-context-xyz` on stderr and `rc=1`; the second prints `preflight ok` after the round-trip; the third prints `sweep rc=0` whether or not any `chaos-*` namespace exists.

Then prove the debris check fires:

```bash
kubectl --context kind-kubeagent-chaos create ns chaos-leftover
bash -c '. chaos/run.sh; CTX=kind-kubeagent-chaos; portable_preflight'; echo "rc=$?"
kubectl --context kind-kubeagent-chaos delete ns chaos-leftover
```

Expected: `refusing to start: chaos-* namespaces already exist`, naming `chaos-leftover`, `rc=1`.

- [ ] **Step 4: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: portable preflight and the end-of-run namespace sweep"
```

---

### Task 6: `run.sh` — the branched `main()`, the portable header, and the baseline

**Files:**
- Modify: `chaos/run.sh` (`main()` at lines 1969-2014; add `portable_header` beside `portable_sweep`)

**Interfaces:**
- Consumes: `PORTABLE`, `CTX` (Task 4); `portable_preflight`, `portable_sweep` (Task 5); `capability_add` (Task 3); `probe_capabilities` (Task 7 — called here, defined there; bash resolves at call time, so this task adds the call and Task 7 adds the function. Until Task 7 lands, run `main()` only via `--only NN` on the kind path, which is what Task 6's verification does — see Step 5.)
- Produces:
  - `portable_header` — writes the portable report header to stdout.
  - `clean_baseline` added to `CAPS` when the baseline scan reported both `Cluster: Healthy` and `No issues found.`, in **either** mode.

- [ ] **Step 1: Write `portable_header`**

Insert into `chaos/run.sh` immediately after `portable_sweep`:

```bash
# portable_header — describe the target cluster in the report WITHOUT naming it.
#
# A kubeconfig context name is a credential under this project's rules, and on a
# managed cluster it is routinely an ARN or a project/region path. Node names are
# no better. What a reader of this report actually needs is the platform, and
# nodeInfo gives that precisely and impersonally: an OS image reading "Amazon
# Linux 2" or "Flatcar Container Linux" identifies the distribution far better
# than a context name would, and identifies no account.
#
# Values are deduplicated: a 300-node cluster running one image should produce
# one line, not 300.
portable_header() {
  local nodes
  nodes="$(kubectl --context "$CTX" get nodes -o json 2>/dev/null | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
items = doc.get("items", [])

def uniq(field):
    seen = []
    for n in items:
        v = n.get("status", {}).get("nodeInfo", {}).get(field, "")
        if v and v not in seen:
            seen.append(v)
    return ", ".join(seen) or "unknown"

print("- Nodes: %d" % len(items))
print("- OS image: %s" % uniq("osImage"))
print("- Container runtime: %s" % uniq("containerRuntimeVersion"))
print("- Kubelet: %s" % uniq("kubeletVersion"))
' 2>/dev/null || true)"
  [ -n "$nodes" ] || nodes='- Nodes: unknown'

  printf '# kubeagent chaos-test results (portable mode)\n\n'
  printf -- '- Mode: portable — an existing cluster the harness did not create and will not delete\n'
  printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
  printf '%s\n' "$nodes"
  printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
}
```

- [ ] **Step 2: Branch `main()`**

Replace `main()` in `chaos/run.sh` in full:

```bash
main() {
  if [ "$PORTABLE" = 1 ]; then
    portable_preflight
    build_kubeagent
  else
    preflight
    build_kubeagent
    create_cluster
    preload_calico_images
    install_calico
  fi

  mkdir -p "$(dirname "$OUT")"
  : > "$OUT"
  assert_init
  if [ "$PORTABLE" = 1 ]; then
    portable_header >> "$OUT"
  else
    {
      printf '# kubeagent chaos-test results\n\n'
      printf -- '- Cluster: Kind %s, Calico CNI, 1 control-plane + 2 workers\n' "$(kind version 2>/dev/null | awk '{print $2}')"
      printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
      printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
    } >> "$OUT"

    # Capture the pristine CoreDNS Corefile TEXT now (cluster is healthy) so scenario 5
    # can restore a known-good config via a clean merge-patch (apply of a get-dump is unreliable).
    kubectl --context "$CTX" -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' > "$COREDNS_BACKUP" 2>/dev/null || true

    wait_system_ready

    # POLICY, not probe: on a cluster the harness created and can delete, it may
    # shell into a node container and write cluster-scoped objects. On a cluster
    # it merely holds credentials for, it may not — however wide those
    # credentials happen to be.
    capability_add node_exec
    capability_add cluster_write
  fi

  log "baseline healthy scan"
  local bout brc bbody btitle bverdict
  bout="$(scan 2>&1)" && brc=0 || brc=$?
  bbody="$(scan_body "$bout")"
  btitle="Baseline (healthy cluster)"; bverdict="baseline"
  if [ "$PORTABLE" = 1 ]; then
    btitle="Baseline"
    bverdict="baseline — on a cluster the harness does not own the verdict is recorded, not asserted"
  fi
  {
    printf '%s\n' "$bout"
    printf '\n--- assertions ---\n'
    if [ "$PORTABLE" = 1 ]; then
      # An operator's cluster is very likely NOT clean, through no fault of
      # kubeagent, so asserting "Cluster: Healthy" here would manufacture a
      # failure out of someone else's backlog. What is true of any conformant
      # cluster is asserted; the verdict itself is recorded in the block above
      # for a human to read.
      expect_eq       "baseline scan exit code"             "$brc" 0
      expect_contains "baseline rendered a cluster verdict"  "$bbody" "Cluster:"
    else
      expect_eq       "baseline scan exit code"          "$brc" 0
      expect_contains "baseline cluster verdict"         "$bbody" "Cluster: Healthy"
      expect_contains "baseline reports nothing to fix"  "$bbody" "No issues found."
    fi
  } | record "$btitle" "$bverdict"

  # clean_baseline is decided by the baseline scan in BOTH modes: scenario 8
  # asserts the WHOLE cluster reads healthy again after a namespace is deleted,
  # which is only checkable on a cluster that read healthy to begin with. On kind
  # it always does — and if it ever does not, the baseline assertions above have
  # already failed the gate.
  if printf '%s\n' "$bbody" | grep -qF "Cluster: Healthy" \
     && printf '%s\n' "$bbody" | grep -qF "No issues found."; then
    capability_add clean_baseline
  fi

  # The probes run AFTER the baseline: the LoadBalancer probe creates and deletes
  # a namespace, and a namespace still terminating during the baseline scan would
  # dirty a verdict the gate depends on.
  probe_capabilities

  run_scenarios

  log "done — report: $OUT"
  if [ "$PORTABLE" = 1 ]; then
    portable_sweep
    echo "portable run finished against an existing cluster; nothing was deleted but the harness's own chaos-* namespaces."
  elif [ "$TEARDOWN" = 1 ]; then
    teardown
  else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    echo "  kind delete cluster --name $CLUSTER"
  fi

  # Non-zero when any assertion failed: this is what makes the harness a gate.
  # A skipped scenario is counted and named in the summary but never changes it.
  assert_summary "$OUT"
}
```

- [ ] **Step 3: Add a temporary no-op `probe_capabilities`**

Task 7 implements it. So this task's `main()` has something to call, add a placeholder immediately above `portable_header`, and **Task 7 replaces its body**:

```bash
# probe_capabilities — decide the capabilities that are facts about the target
# cluster rather than policy. Implemented in the next commit.
probe_capabilities() { :; }
```

- [ ] **Step 4: Verify the kind path is byte-for-byte unchanged**

```bash
bash -n chaos/run.sh
git stash && ./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/before.md; git stash pop
./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/after.md
diff /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/before.md \
     /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/after.md
```

Expected: the only differences are the header's `Kind vX.Y.Z` line (unchanged text) and the new `- scenarios skipped: 0` bullet from Task 1. The `## Baseline (healthy cluster)` heading, its `_Verdict: baseline_` line and all three baseline assertions must be present and identical.

- [ ] **Step 5: Verify the portable path reaches the baseline**

```bash
./chaos/run.sh --context kind-kubeagent-chaos --only 21 \
  --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t6-portable.md
grep -n 'kubeagent-chaos\|kind-' /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t6-portable.md
```

Expected: exit 0. The report opens with `# kubeagent chaos-test results (portable mode)`, carries `- OS image:`, `- Container runtime:` and `- Kubelet:` lines, records `## Baseline` with two assertions, and runs scenario 21. **The `grep` must return nothing** — no context name and no node name anywhere in the report.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: portable setup path, header and baseline"
```

---

### Task 7: `run.sh` — the four capability probes

**Files:**
- Modify: `chaos/run.sh` (replace the placeholder `probe_capabilities` added in Task 6)

**Interfaces:**
- Consumes: `capability_add` (Task 3), `CTX`, `log()`.
- Produces: `probe_capabilities` adds `no_metrics_server`, `netpol_enforced` and `no_loadbalancer` to `CAPS` when the target cluster has them. `clean_baseline` is added by `main()` (Task 6), not here.

- [ ] **Step 1: Implement `probe_capabilities`**

Replace the placeholder in `chaos/run.sh`:

```bash
# probe_capabilities — decide the capabilities that are facts about the TARGET
# CLUSTER rather than policy.
#
# It runs in both modes. On kind every one of these is present, which is what
# keeps the gate path's assertion count unchanged AND exercises the probe code
# on the path that gates releases — a probe only ever run in portable mode would
# be a probe nothing tests.
probe_capabilities() {
  log "probe cluster capabilities"

  # no_metrics_server — scenario 18 asserts the structural-rules-only capacity
  # path. On a cluster with metrics-server, kubeagent takes the metrics path
  # instead and the assertion would fail rather than skip: a false alarm, which
  # is strictly worse than a stated gap.
  if ! kubectl --context "$CTX" get apiservice v1beta1.metrics.k8s.io >/dev/null 2>&1; then
    capability_add no_metrics_server
  fi

  # netpol_enforced — scenario 4 needs a CNI that actually enforces
  # NetworkPolicy. This is a HEURISTIC and its two failure modes are stated in
  # chaos/README.md: an enforcing CNI whose DaemonSet is not on this list gives a
  # false SKIP (safe — the summary names it), and a listed CNI deliberately
  # configured not to enforce gives a false FAILURE in scenario 4. There is no
  # cheap probe that avoids both, and a wrong guess in the safe direction is the
  # one to prefer.
  local ds
  for ds in calico-node cilium weave-net kube-router antrea-agent; do
    if kubectl --context "$CTX" -n kube-system get ds "$ds" >/dev/null 2>&1; then
      capability_add netpol_enforced
      break
    fi
  done

  # no_loadbalancer — scenario 6 asserts a LoadBalancer Service never gets an
  # external address. A cluster with a provider assigns one within seconds and
  # the assertion would fail rather than skip.
  #
  # The Service selects nothing on purpose: a provider assigns an address to a
  # Service with no endpoints just the same, so there is no image to pull and
  # nothing to schedule. The address, if one arrives, is read and discarded —
  # it is never printed, because an external address is exactly the kind of
  # thing this project does not write into a forwarded artifact.
  local pns=chaos-probe addr="" i
  kubectl --context "$CTX" create ns "$pns" --dry-run=client -o yaml \
    | kubectl --context "$CTX" apply -f - >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n "$pns" apply -f - >/dev/null 2>&1 <<'PROBE' || true
apiVersion: v1
kind: Service
metadata:
  name: probe
spec:
  type: LoadBalancer
  selector:
    app: chaos-probe-selects-nothing
  ports:
    - port: 80
      targetPort: 80
PROBE
  for i in $(seq 30); do
    addr="$(kubectl --context "$CTX" -n "$pns" get svc probe \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
    [ -n "$addr" ] && break
    sleep 1
  done
  [ -n "$addr" ] || capability_add no_loadbalancer
  kubectl --context "$CTX" delete ns "$pns" --wait=true --timeout=120s >/dev/null 2>&1 || true

  log "capabilities: ${CAPS:-<none>}"
}
```

The trailing `log` line prints capability names only — six fixed strings from a closed vocabulary, no cluster identity — and goes to the console, not `$OUT`.

- [ ] **Step 2: Verify every probe answers correctly on kind**

```bash
bash -n chaos/run.sh
./chaos/run.sh --only 21 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t7-kind.md 2>&1 \
  | grep -A1 'probe cluster capabilities'
```

Expected: the `capabilities:` line names all six —
`node_exec cluster_write clean_baseline no_metrics_server netpol_enforced no_loadbalancer` (order follows the order they were added, which is what this expects). Anything missing means a probe is wrong and the gate path's counts would move.

- [ ] **Step 3: Verify the portable path finds the four probed ones**

```bash
./chaos/run.sh --context kind-kubeagent-chaos --only 21 \
  --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t7-portable.md 2>&1 \
  | grep -A1 'probe cluster capabilities'
```

Expected: `capabilities: clean_baseline no_metrics_server netpol_enforced no_loadbalancer` — the four probed capabilities present, and neither policy capability, because the harness did not create this cluster.

- [ ] **Step 4: Confirm the probe left nothing behind**

```bash
kubectl --context kind-kubeagent-chaos get ns | grep chaos- || echo "no chaos-* namespaces"
```

Expected: `no chaos-* namespaces`.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: probe LoadBalancer, metrics-server and NetworkPolicy enforcement"
```

---

### Task 8: `run.sh` — the twelve `requires` guards and scenario 02

**Files:**
- Modify: `chaos/run.sh` — one inserted line in each of twelve scenario bodies; `scenario_02_certs` rewired through `assert_skip`

**Interfaces:**
- Consumes: `requires` (Task 3), `assert_skip` and `scenario_title` (Tasks 1-2).
- Produces: nothing new. Every guarded scenario keeps its existing signature, body and assertions.

- [ ] **Step 1: Insert the twelve guards**

In each scenario below, insert `requires <capability> || return 0` as the **first statement of the body**, immediately above the existing `log "scenario N: ..."` line. Every one of the twelve opens with that `log` line today, so the insertion point is uniform and no other line moves.

| Function (definition line) | Guard to insert |
|---|---|
| `scenario_01_etcd` (295) | `requires node_exec \|\| return 0` |
| `scenario_03_diskfull` (318) | `requires cluster_write \|\| return 0` |
| `scenario_04_networkpolicy` (367) | `requires netpol_enforced \|\| return 0` |
| `scenario_05_coredns` (343) | `requires cluster_write \|\| return 0` |
| `scenario_06_lb` (493) | `requires no_loadbalancer \|\| return 0` |
| `scenario_08_nsdelete` (513) | `requires clean_baseline \|\| return 0` |
| `scenario_11_kubelet` (595) | `requires node_exec \|\| return 0` |
| `scenario_16_operators` (1065) | `requires cluster_write \|\| return 0` |
| `scenario_17_gitops` (1242) | `requires cluster_write \|\| return 0` |
| `scenario_18_capacity` (1359) | `requires no_metrics_server \|\| return 0` |
| `scenario_20_rbac` (1594) | `requires cluster_write \|\| return 0` |
| `scenario_22_dnshealth` (1789) | `requires cluster_write \|\| return 0` |

Concretely, `scenario_05_coredns` becomes:

```bash
scenario_05_coredns() {   # break the Corefile -> CoreDNS CrashLoopBackOff
  requires cluster_write || return 0
  log "scenario 5: CoreDNS CrashLoopBackOff (bad Corefile)"
```

Two of these matter especially: `scenario_03_diskfull` and `scenario_11_kubelet` call `worker_node()` on the line right after their `log`, and `worker_node()` exits 1 when no node name matches `worker`. Guarding **above** the `log` line means that call is never reached in portable mode.

Nothing else in any scenario body changes — not an assertion, not a `record` verdict, not a namespace name.

- [ ] **Step 2: Rewire scenario 02 through `assert_skip`**

Replace `scenario_02_certs` in full:

```bash
scenario_02_certs() {   # unconditional documented skip (can't force cert expiry quickly or safely)
  # The one scenario that is skipped on every cluster, kind or not. It is
  # declared through assert_skip rather than left as a bare record() so it is
  # counted in `scenarios skipped` alongside the capability-gated ones: a run
  # that reported "0 skipped" while quietly omitting this scenario is the exact
  # defect the skip accounting was added to remove.
  #
  # Still no expect_* call, and still on purpose: this scenario runs no scan and
  # computes no value, so any assertion could only compare the skip text with
  # itself. The TLS branch is asserted in internal/connectivity's unit tests
  # instead (x509 UnknownAuthority / CertificateInvalid / Hostname errors, plus
  # "x509:" / "certificate" / "tls: " substrings).
  local reason='control-plane certificate expiry cannot be forced quickly or safely'
  log "scenario 2: expired certificates (skipped)"
  assert_skip "$(scenario_title "${FUNCNAME[0]}")" "$reason"
  printf 'Skipped: %s.\nkubeagent TLS / expired-certificate handling is covered by internal/connectivity unit tests.\n' "$reason" \
    | record "2. Expired certificates" "skipped (documented; TLS branch unit-tested)"
}
```

Note `${FUNCNAME[0]}` here (the scenario calls `scenario_title` itself) versus `${FUNCNAME[1]}` inside `requires` (which is called *by* the scenario). The report heading passed to `record` stays the historical `"2. Expired certificates"`; only the **skip-log label** is derived.

- [ ] **Step 3: Verify a guarded scenario still runs on kind**

```bash
bash -n chaos/run.sh
./chaos/run.sh --only 05 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t8-kind-05.md
```

Expected: exit 0, scenario 5 runs in full (`cluster_write` is present on kind), and the console tail reads `... ; 0 scenarios skipped`.

- [ ] **Step 4: Verify the same scenario skips in portable mode**

```bash
./chaos/run.sh --context kind-kubeagent-chaos --only 05 \
  --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t8-portable-05.md
```

Expected: exit 0. The console shows
`SKIP: 5. coredns — writes cluster-scoped objects, which the harness will not do on a cluster it does not own`
and ends `assertions: 2 run, 0 failed; 1 scenario skipped` (the two baseline assertions). The report carries a `## 5. coredns` section with a `_Verdict: skipped (...)_` line, and the summary's fenced skip block lists it. **CoreDNS on the live cluster is untouched** — confirm with `kubectl --context kind-kubeagent-chaos -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' | head -3`.

- [ ] **Step 5: Verify scenario 02 is counted**

```bash
./chaos/run.sh --only 02 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t8-02.md
```

Expected: exit 0, console tail `assertions: 3 run, 0 failed; 1 scenario skipped` (three baseline assertions on the kind path), and the report's skip block lists `SKIP	2. certs — control-plane certificate expiry cannot be forced quickly or safely`.

- [ ] **Step 6: Verify an unguarded scenario is untouched in both modes**

```bash
./chaos/run.sh --context kind-kubeagent-chaos --only 07 \
  --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t8-portable-07.md
```

Expected: exit 0, scenario 7 (OOMKilled) runs in full against the existing cluster, `0 scenarios skipped` beyond the ones already counted, and `kubectl --context kind-kubeagent-chaos get ns | grep chaos-` returns nothing afterwards.

- [ ] **Step 7: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: gate the twelve infrastructure-coupled scenarios on a declared capability"
```

---

### Task 9: the two context-name leaks

**Files:**
- Modify: `chaos/run.sh` — `scenario_15_multicluster` (962-1063) and `scenario_19_mcp` (the block at 1566-1590)

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. **Neither scenario's assertion count changes** — scenario 15 keeps its 10, scenario 19 keeps its 5, and what each proves is unchanged.

Both leaks are pre-existing, and both become load-bearing the moment the harness can be pointed at a real cluster: today `$CTX` is the harmless literal `kind-kubeagent-chaos`, tomorrow it is whatever an operator's kubeconfig calls their production cluster, and the results file is designed to be forwarded.

- [ ] **Step 1: Give scenario 15 a third harness-named alias**

In `scenario_15_multicluster`, after the `alias-b` line, add an `alias-a` for the same cluster:

```bash
  KUBECONFIG="$kc" kubectl config set-context alias-a --cluster="$ccluster" --user="$cuser" >/dev/null
  KUBECONFIG="$kc" kubectl config set-context alias-b --cluster="$ccluster" --user="$cuser" >/dev/null
```

Then change the daemon invocation to name `alias-a` instead of `$CTX`:

```bash
  ./kubeagent watch --kubeconfig "$kc" \
    --context alias-a --context alias-b --context dead \
    -n "$ns" --metrics-addr "127.0.0.1:$port" --heartbeat 10s --debounce 2s >"$wlog" 2>&1 &
```

The daemon labels every metric series and every `/issues` record by **context name**. Naming both aliases means every label the report dumps is a string this harness chose. The `$CTX` context still exists inside `$kc` (`--minify` put it there) and is simply not passed; the three `--context` flags are explicit, so `current-context` is never consulted.

Also update the comment above the kubeconfig construction:

```bash
  # Two harness-chosen names for the SAME cluster prove labelling and the
  # cross-cluster merge without paying for a second Kind cluster; a third
  # context pointing at a closed port proves per-cluster degradation. This does
  # NOT test genuinely divergent cluster state — see the verdict text.
  #
  # Both live names are alias-a and alias-b rather than the real context: the
  # daemon labels every series by context name, this scenario dumps those series
  # straight into the report, and a kubeconfig context name is a credential.
```

- [ ] **Step 2: Rename the two `$CTX`-keyed readings**

Replace the four affected lines in the "Hoist the per-cluster readings" block:

```bash
  local clusters_total up_a up_b up_dead issue_a issue_b issue_dead kubeconfig_material write_verbs
  clusters_total="$(printf '%s\n' "$metrics" | awk '/^kubeagent_clusters_total/{print $2}')"
  up_a="$(printf    '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"alias-a\""){print $2}')"
  up_b="$(printf    '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"alias-b\""){print $2}')"
  up_dead="$(printf '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"dead\""){print $2}')"
  issue_a="$(printf    '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="alias-a"' || true)"
  issue_b="$(printf    '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="alias-b"' || true)"
  issue_dead="$(printf '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="dead"' || true)"
```

(`up_alias` and `issue_alias` become `up_b` and `issue_b`; the old `up_ctx`/`issue_ctx` become `up_a`/`issue_a`. Rename every use — there are no others outside the assertion block below.)

- [ ] **Step 3: Reword the five affected assertion labels**

```bash
    expect_eq "readyz stays 200 with one target dead" "$ready_code" 200
    expect_eq "cluster roster size"                   "$clusters_total" 3
    expect_eq "the first label for the real cluster is up"  "$up_a" 1
    expect_eq "the second label for the real cluster is up" "$up_b" 1
    expect_eq "the unreachable target is down"        "$up_dead"  0
    expect_ge "the broken workload is seen under the first label"  "$issue_a" 1
    expect_ge "and again under the second label"                   "$issue_b" 1
    expect_eq "no issue is attributed to the unreachable target"   "$issue_dead" 0
    expect_eq "no log line carries kubeconfig material" "$kubeconfig_material" 0
    expect_eq "daemon log mentions no write verb"       "$write_verbs" 0
```

Ten assertions, same ten facts.

- [ ] **Step 4: De-interpolate the scenario 15 verdict**

In the `record` verdict string, make exactly two edits — nothing else in that long string moves:

- `kubeagent_cluster_up is 1 for both $CTX and alias-b and 0 for dead`
  → `kubeagent_cluster_up is 1 for both alias-a and alias-b and 0 for dead`
- `each under cluster=\"$CTX\" and again under cluster=\"alias-b\"`
  → `each under cluster=\"alias-a\" and again under cluster=\"alias-b\"`

And change `Scope: alias-b is a second NAME for the same cluster` to `Scope: alias-a and alias-b are two harness-chosen NAMES for the same cluster` so the prose matches.

- [ ] **Step 5: Fix scenario 19**

In `scenario_19_mcp`, after the `IFS='|' read -r got_verdict got_findings got_context <<<"$triage"` line, derive an indicator instead of carrying the value forward:

```bash
  local got_verdict got_findings got_context context_matches
  IFS='|' read -r got_verdict got_findings got_context <<<"$triage"
  # The context name is a credential, and expect_eq echoes the ACTUAL value on
  # its PASS branch — so comparing got_context against $CTX would write the
  # context name into the report on a passing run. Compare a derived indicator
  # instead: the report carries the answer, never the name. `if` rather than a
  # && chain because a false && chain returns 1 and `set -e` would abort here.
  context_matches=no
  if [ -n "${got_context:-}" ] && [ "${got_context}" = "$CTX" ]; then context_matches=yes; fi
```

Change the gate-checks line:

```bash
    printf 'tools/call (id 3) coverage.context matches --context: %s\n' "$context_matches"
```

Change the assertion:

```bash
    expect_eq "the server's context round-trips into the response" "$context_matches" "yes"
```

And end the `record` verdict with:

```
tools/call (id 3) coverage.context matches --context reads yes (the --context the server was started with round-trips into the response; the context name itself is a credential and never reaches this report)
```

replacing `tools/call (id 3) coverage.context reads $CTX (the --context the server was started with round-trips into the response)`.

- [ ] **Step 6: Verify both scenarios still pass and leak nothing**

```bash
bash -n chaos/run.sh
./chaos/run.sh --only 15 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t9-15.md
./chaos/run.sh --only 19 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t9-19.md
grep -c 'kind-kubeagent-chaos' /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t9-15.md \
  /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/t9-19.md
```

Expected: both exit 0; scenario 15 reports 10 assertions passing (13 with the baseline), scenario 19 reports 5 (8 with the baseline); and **both `grep -c` print `0`**. That last number is the point of this task.

- [ ] **Step 7: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: keep the kubeconfig context name out of the results report"
```

---

### Task 10: Documentation

**Files:**
- Modify: `chaos/README.md` (lines 12-14; the Prerequisites section at 16; the Assertions section at 164; the Safety section at 292; a new `## Portable mode` section)
- Modify: `website/docs/compatibility.md:132-136`
- Modify: `website/docs/roadmap.md` (the trailing cross-distro sentence around line 525)
- Modify: `CHANGELOG.md` (under `## [Unreleased]` at line 8)

**Interfaces:**
- Consumes: everything from Tasks 1-9.
- Produces: nothing executable.

**`CLAUDE.md` is deliberately NOT touched.** Its Theme H paragraph and its `134` describe the release gate, which this slice does not change.

- [ ] **Step 1: Amend the two now-false claims in `chaos/README.md`**

The file states twice that the harness "never reads your current kubecontext, so it cannot touch another cluster" — once near the top (lines 12-14) and once in `## Safety`. Both become false under `--context`. Amend each to say what is now true:

> By default the harness targets **only** the Kind context it creates (`kind-kubeagent-chaos`) — it does not read your current kubecontext, so a plain run cannot touch another cluster. `--context <ctx>` deliberately points it at a cluster you already have; that mode is described under [Portable mode](#portable-mode) and carries its own guard rails.

Keep the surrounding wording of each passage; change only the claim.

- [ ] **Step 2: Add the `## Portable mode` section**

Insert into `chaos/README.md` after the `### --explain` subsection and before `## Assertions`:

````markdown
## Portable mode

```bash
./chaos/run.sh --context <ctx>
```

Runs the portable subset against a cluster the harness did **not** create. The
report defaults to `docs/testing/chaos-results-portable.md`.

`kind` and `docker` are not required in this mode — the binary list is
`kubectl`, `go`, `curl` and `python3`.

### What it does to the cluster

Every scenario that runs creates a `chaos-*` namespace, breaks something inside
it, and deletes the namespace afterwards. Whatever a partial run leaves behind
is swept at the end. Two consequences are worth knowing before you point this at
a shared cluster:

- **Scenario 8 deletes a namespace and asserts the cluster reads healthy again.**
  It deletes only the `chaos-nsdelete` namespace it created moments earlier — but
  a cluster where a namespace deletion triggers alerting will alert.
- **Scenario 10 plants a fake AWS access key** (`AKIAIOSFODNN7EXAMPLE`, AWS's own
  published example value) in a ConfigMap so kubeagent's credential-leak detector
  has something to find. A secret scanner or a Falco rule watching the API server
  will fire on it. It is deleted with the namespace.

### What it refuses

Nine of the 23 scenarios are skipped on a cluster the harness does not own:

| Skipped | Why |
|---|---|
| 1, 11 | need shell access to a node container, which exists only on a cluster the harness created |
| 3, 5, 16, 17, 20, 22 | write cluster-scoped objects (node conditions, the CoreDNS ConfigMap, CRDs, ClusterRoles) |
| 2 | control-plane certificate expiry cannot be forced quickly or safely — skipped everywhere, including on Kind |

Four more skip depending on what the cluster turns out to be:

| Skipped when | Scenario | Probe |
|---|---|---|
| a LoadBalancer provider assigns addresses | 6 | a LoadBalancer Service in a temporary `chaos-probe` namespace gets an address within 30s |
| metrics-server is installed | 18 | the `v1beta1.metrics.k8s.io` APIService exists |
| the CNI is not recognised as enforcing | 4 | a `calico-node`, `cilium`, `weave-net`, `kube-router` or `antrea-agent` DaemonSet in `kube-system` |
| the cluster was not already healthy | 8 | the baseline scan reported `Cluster: Healthy` and `No issues found.` |

The CNI probe is a **heuristic** with two named failure modes: an enforcing CNI
whose DaemonSet is not on that list produces a false skip (safe — the summary
names it), and a listed CNI configured not to enforce produces a false failure in
scenario 4. There is no cheap probe that avoids both, and the harness prefers to
be wrong in the direction of skipping.

Three flags are **refused**, not ignored, because all three manage a Kind
cluster's lifecycle: `--recreate`, `--teardown` and `--k8s-version`. Each exits
2 with its reason.

### What it checks before touching anything

1. The named context exists in your kubeconfig.
2. The cluster answers.
3. No `chaos-*` namespace already exists — debris from an aborted run, or a
   second run in progress. The harness names what it found and refuses; it will
   not delete a namespace it did not create.
4. A namespace create/delete round trip actually succeeds with these credentials.

Each failure exits 1 with its reason on **stderr**.

### What is in the report, and what is not

The report names the platform and never the cluster: server version, node count,
and the deduplicated OS image, container runtime and kubelet version from
`nodeInfo`. **No context name, no node name, no address.** A kubeconfig context
name is a credential — on a managed cluster it is routinely an ARN or a
project/region path — and this report is designed to be forwarded.

### The baseline

A cluster you already run is very likely not clean, through no fault of
kubeagent. In portable mode the baseline asserts only that the scan exited 0 and
rendered a verdict; the verdict itself is recorded for a human to read rather
than asserted. A dirty baseline also withdraws `clean_baseline`, which skips
scenario 8.
````

- [ ] **Step 3: Update the Assertions section**

In `chaos/README.md`'s `## Assertions` section, update the console-line description to the new shape and add the skip accounting:

> The run ends with `assertions: 134 run, 0 failed; 1 scenario skipped` on the console and an `## Assertion summary` in the report carrying the same three counts. A failure list is fenced under it when there are failures; a skip list is fenced under it when there are skips.
>
> **A skip is never a failure and never moves the exit code** — it is a declared gap. It is reported unconditionally, including when the count is zero, so a run that skipped nine scenarios can never be read as a full green one. The exit code is non-zero if and only if an assertion failed.

Also amend the existing sentence about scenario 2 carrying no assertion: it still carries none, but it is now **counted** in `scenarios skipped` rather than silently absent.

Leave `134` at line 148 exactly as it is.

- [ ] **Step 4: Amend `website/docs/compatibility.md`**

Replace the paragraph at lines 132-136 with:

```markdown
**What the matrix does not cover:** one distribution (kind), one architecture
(amd64), one CNI (Calico), on `ubuntu-latest`. kubeagent uses only stable
`client-go` APIs and should work on any conformant cluster in the window, but
EKS, GKE, AKS, OpenShift, k3s, and RKE2 are **not gated in CI**, and nothing on
this page claims they are.

The harness itself can now be pointed at a cluster it did not create:
`./chaos/run.sh --context <ctx>` runs the subset of scenarios whose blast radius
is a namespace it creates and deletes, refuses every scenario that would write a
cluster-scoped object or touch a node, and names each skipped scenario and its
reason in the assertion summary — so a partial run can never be mistaken for a
full one. That makes a cross-distribution answer **obtainable by hand**. It does
not make one **gated**, which is still ahead.
```

- [ ] **Step 5: Amend `website/docs/roadmap.md`**

Replace the trailing sentence of the Theme H slice 8 paragraph (around line 525) — currently "Cross-distro coverage (EKS, GKE, AKS, OpenShift, k3s, RKE2) is not part of this slice and remains ahead" — with:

```markdown
Cross-distro coverage (EKS, GKE, AKS, OpenShift, k3s, RKE2) was not part of that
slice. Since then the harness has grown a **portability seam**:
`./chaos/run.sh --context <ctx>` runs the namespaced-only subset against a
cluster the harness did not create, refuses every scenario that would write a
cluster-scoped object or shell into a node, and names each skip and its reason in
the assertion summary. Pointing it at a distribution is now a hand-run away;
**gating one in CI is still ahead** — see
[the chaos harness](https://github.com/imantaba/kubeagent/tree/main/chaos).
```

Leave `134` at line 533 exactly as it is.

- [ ] **Step 6: Add the CHANGELOG entries**

Under `## [Unreleased]` in `CHANGELOG.md`:

```markdown
### Added

- **A portability seam in the chaos harness.** `./chaos/run.sh --context <ctx>`
  runs the suite against a cluster the harness did not create. Each scenario
  declares the infrastructure it needs from a closed six-name vocabulary, and a
  scenario whose need is unmet is skipped with a named reason rather than run or
  silently dropped: six scenarios that write cluster-scoped objects and two that
  need shell access to a node container are refused outright on a cluster the
  harness does not own, and four more are gated on what the cluster turns out to
  have — a LoadBalancer provider, metrics-server, a NetworkPolicy-enforcing CNI,
  and a clean starting verdict. A preflight refuses to start unless the context
  connects, no `chaos-*` namespace already exists, and a namespace create/delete
  round trip succeeds; `--recreate`, `--teardown` and `--k8s-version` are refused
  rather than ignored; and leftover namespaces are swept at the end. The report
  names the platform (server version, node count, deduplicated OS image,
  container runtime and kubelet version) and never the cluster. This makes a
  cross-distribution answer obtainable by hand; gating a distribution in CI is a
  separate piece of work.

### Changed

- **The chaos harness's assertion summary now counts skipped scenarios.** The
  report gains a `- scenarios skipped: N` bullet and, when N is non-zero, a
  fenced list of each skip and its reason; the console line becomes
  `assertions: N run, M failed; K scenarios skipped`. It is reported
  unconditionally, including when it is zero. A skip is never a failure and never
  changes the exit code, which stays non-zero if and only if an assertion failed.

### Fixed

- **The chaos report no longer carries the kubeconfig context name.** The
  multi-cluster and MCP scenarios wrote it into the results file — as a metric
  label, in a `/issues` roster, and as an asserted value that the assertion
  helper echoes on its passing branch. Both now compare harness-chosen names and
  derived indicators instead, proving exactly what they proved before. A context
  name is a credential and the results file is designed to be forwarded.
```

- [ ] **Step 7: Build the site**

```bash
(cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", exit 0, no `WARNING` about `compatibility.md` or `roadmap.md`. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 8: Confirm 134 did not move and nothing leaked**

```bash
grep -rn '134' CLAUDE.md chaos/README.md website/docs/compatibility.md website/docs/roadmap.md
git diff main --stat -- go.mod go.sum internal/ deploy/
```

Expected: exactly four `134` hits, all pre-existing and unchanged; the second command prints nothing.

- [ ] **Step 9: Commit**

```bash
git add chaos/README.md website/docs/compatibility.md website/docs/roadmap.md CHANGELOG.md
git commit -s -m "docs: the chaos harness's portability seam"
```

---

## The slice's gate

Run **once, at the end**, after Task 10 — not per task. Both runs are required and both must be green.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY

# 1. The release gate, unchanged. ~40 minutes.
./chaos/run.sh --recreate
```

Must exit 0 with `assertions: 134 run, 0 failed; 1 scenario skipped` — the count intact, and the one skip being scenario 2, which the harness has always skipped and has never before admitted to.

```bash
# 2. The seam itself, against the cluster run 1 just left up.
./chaos/run.sh --context kind-kubeagent-chaos
```

Must exit 0 with **14 scenarios run and 9 skipped**, all four probes answering correctly (`capabilities: clean_baseline no_metrics_server netpol_enforced no_loadbalancer`), and:

```bash
grep -c 'kind-kubeagent-chaos' docs/testing/chaos-results-portable.md   # must print 0
kubectl --context kind-kubeagent-chaos get ns | grep chaos-             # must print nothing
```

Then verify DCO across the branch:

```bash
bash scripts/dco-check.sh main HEAD
```

### What stays unverified

This machine has kind, helm and docker, and **no k3d, k3s, minikube, crc, `oc`, and no cloud credentials.** Run 2 exercises the portable code path genuinely — a different preflight, a different header, a different baseline, live probes, live skips — but against a cluster that happens to be kind. What it cannot prove is that a *managed* cluster's probes answer correctly: that a real LoadBalancer provider is detected, that metrics-server is detected, that a foreign CNI is recognised, or that a dirty baseline degrades rather than fails. Those are predictions until someone runs this against EKS, GKE, AKS, OpenShift, k3s or RKE2. The docs must not claim otherwise, and Task 10's wording is written to that constraint.

---

## Self-review

**1. Spec coverage.** Every section of the spec maps to a task: the declaration mechanism → Task 3 + Task 8; skip accounting → Task 1; `scenario_title` → Task 2; the entry point and its three refusals → Task 4; preflight against a foreign cluster → Task 5; the portable header, the baseline block and "cluster identity is a credential" → Task 6 + Task 9; the capability probes → Task 7; "what does not move" → the Global Constraints, enforced by the verification steps in Tasks 6, 8 and 10; the files table → the File Structure map; testing → the selftest steps in Tasks 1-3 and the live `--only` runs in Tasks 4-9; the gate and what stays unverified → the closing section.

**2. Placeholder scan.** No TBD, no "handle edge cases", no "similar to Task N". Task 6 introduces a deliberate one-line stub (`probe_capabilities() { :; }`) whose replacement is Task 7 Step 1 — that is a sequencing device with its replacement named, not a placeholder.

**3. Type consistency.** `assert_skip <label> <reason>` (Task 1) is called by `requires` (Task 3) and by `scenario_02_certs` (Task 8) with that signature. `scenario_title <func-name>` (Task 2) is called by `requires` with `${FUNCNAME[1]}` and by scenario 02 with `${FUNCNAME[0]}` — the difference is explained where it appears. `capability_add <name>` (Task 3) is called by `main()` (Task 6) and `probe_capabilities` (Task 7). `SKIPLOG`, `CAPS`, `PORTABLE`, `CONTEXT` and `CTX` are used with the same meaning in every task that touches them. The six capability names are identical in the vocabulary table, `capability_reason`, `probe_capabilities`, the twelve guards and the README table.
