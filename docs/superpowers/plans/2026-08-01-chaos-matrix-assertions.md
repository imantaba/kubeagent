# Chaos harness: machine-checked assertions (Theme H sub-project 7, slice 7a) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn `./chaos/run.sh` from a report a human reads into a gate that fails on its own, by adding four assertion helpers and wiring every scenario's already-computed evidence through them.

**Architecture:** A new sourced file `chaos/assert.sh` holds four `expect_*` helpers, an outcome log and a summary function. Each helper prints one `PASS`/`FAIL` line on stdout — so a call inside a `{ … } | record …` block lands in the report beside the value it checked — and appends the same outcome to a **file**, `$ASSERTLOG`. The file is load-bearing: `record()` is fed by a pipeline, a pipeline runs in a subshell, and a shell-variable counter incremented there would be discarded the moment the block ended. `main` ends with `assert_summary "$OUT"`, which appends a roll-up to the report and returns non-zero when anything failed.

**Tech Stack:** bash (`set -euo pipefail`), Kind, kubectl, the `kubeagent` binary the harness builds. No Go code. No new dependency.

## Global Constraints

Every task's requirements implicitly include this section.

- Every commit needs a `Signed-off-by` trailer matching its author (`git commit -s`) because `main` enforces DCO; verify with `scripts/dco-check.sh main HEAD`. **No `Co-Authored-By` and no AI attribution anywhere** — commits, code, docs, changelog.
- **NO NEW DEPENDENCY**, and this slice is expected to touch **no Go production code at all**. `go.mod` and `go.sum` must not change. If converting a scenario reveals a kubeagent defect, that is a separate commit with its own unit test, and it must be **reported** rather than folded into an assertion that papers over it.
- **Assertions are written at kubeagent's contract level, never Kubernetes'.** Assert on what kubeagent did with an API message (a finding is present, a counter, an exit code), never on the API server's wording. An assertion that pins upstream text goes red on a change that broke nothing and trains everyone to ignore a red nightly.
- **Every `expect_*` call must be SEEN to fail.** Each task's report must record, per assertion, the perturbation used to demonstrate it failing — the same discipline TDD applies to a unit test. A vacuously-passing assertion is worse than none. The exact procedure is in "Perturbation evidence" below.
- **No per-version expected-value table and no version skips in this slice**; that machinery belongs to slice 7b and must not be anticipated here.
- `ready_replicas()`'s `"?"` is a **FAIL**, never coerced to 0.
- `scenario_01_etcd` stays **LAST** in `run_scenarios()`.
- Omitting any flag keeps today's behaviour: the report path, the cluster name and the context are unchanged in this slice.
- **No secrets / private IPs / internal hostnames anywhere.** RFC 5737 IPs (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), RFC 2606 domains (`example.com`, `example.org`). Scenario 20 already asserts that no credential material reaches the recorded output; that assertion must survive the conversion intact.
- **Never expose API keys to the shell:** the harness runs with `ANTHROPIC_API_KEY` unset and the `--explain` scenarios skip.
- `go test` runs with `-p 2` and never `-short`; CI's `go test -race ./...` must stay green (it should be untouched by this slice).
- A helper must **never abort the run**. The harness is under `set -e`; a FAIL must let the remaining scenarios run and surface at the end, in the exit code.

## File Structure

| File | Responsibility |
| --- | --- |
| `chaos/assert.sh` (**create**) | The four helpers, `assert_init`, `assert_summary`. Sourced by the harness and by the self-test. Knows nothing about scenarios. |
| `chaos/assert-selftest.sh` (**create**) | Scenario-free, cluster-free self-test of the helpers. Runnable in under a second. |
| `chaos/run.sh` (**modify**) | Sources `assert.sh`, calls `assert_init` once, converts each scenario to call `expect_*`, ends `main` with `assert_summary "$OUT"`. |
| `chaos/README.md` (**modify**, Task 10) | Says the harness now exits non-zero. |
| `.claude/skills/release/SKILL.md` (**modify**, Task 10) | Step 3 stops telling the operator to eyeball the report. |
| `CLAUDE.md` (**modify** only if it claims a human reads the report, Task 10) | Same wording fix. |

## Verification reality

A full `./chaos/run.sh --recreate` takes **35–40 minutes** and needs docker, kind, kubectl, helm, go, curl and python3. **No per-task implementer runs the full suite.** Per-task verification is:

1. `bash -n chaos/run.sh` — syntax.
2. `bash chaos/assert-selftest.sh` — the helpers still behave (every task, not just Task 1).
3. `./chaos/run.sh --only NN --out /tmp/chaos-task<N>-<NN>.md` for each scenario the task converted, against the **already-up** cluster.

A cluster named `kubeagent-chaos` (context `kind-kubeagent-chaos`) is up on this machine. `create_cluster` returns early when it exists, so `--only` reuses it. `--only` still runs preflight, the build, the Calico apply (idempotent), the settle wait and the **baseline** before the selected scenario — budget roughly 3–6 minutes per `--only` run. **Always pass `--out`** so a task never clobbers `docs/testing/chaos-results.md`.

The full-suite run is the **slice's gate**, run once at the end by the controller, and it must show the harness exiting 0 on a healthy conversion plus a deliberate perturbation proving it exits non-zero.

## Perturbation evidence

Every assertion must be demonstrated failing. Re-running a scenario once per assertion is unaffordable, so each task uses both of these and records both in its report:

**(a) Value replay — every assertion, no cluster.** After a green `--only NN` run, write the scenario's observed values into a scratch script that sources `chaos/assert.sh` and replays the scenario's assertion block twice: once with the real values (expect all PASS) and once with each value perturbed one at a time (expect exactly that assertion to FAIL). Example shape:

```bash
cat >/tmp/replay.sh <<'SH'
set -uo pipefail
. chaos/assert.sh
assert_init
expect_eq "blocked ready replicas under deny-all" "$1" 0
assert_summary /dev/null
SH
bash /tmp/replay.sh 0   # PASS, summary returns 0
bash /tmp/replay.sh 1   # FAIL, summary returns 1
bash /tmp/replay.sh '?' # FAIL — "couldn't tell" is never a 0
```

This proves the assertion **discriminates**: it is not a comparison that holds for every input.

**(b) In-place end-to-end — at least one assertion per scenario.** Edit the scenario's expected value (or break its injection), re-run `./chaos/run.sh --only NN --out /tmp/…`, and confirm the report carries the `FAIL` line, the console carries `ASSERTION FAILED:`, and the harness **exits non-zero**. Revert before committing; `git diff` must show no leftover perturbation.

The report records, per assertion: the label, the observed value, and which of (a)/(b) demonstrated it failing.

---

### Task 1: Assertion helpers and their self-test

**Files:**
- Create: `chaos/assert.sh`
- Create: `chaos/assert-selftest.sh`
- Modify: `chaos/run.sh` (source the helpers; `assert_init` in `main`; `assert_summary` at the end of `main`)

**Interfaces:**
- Consumes: nothing.
- Produces, for every later task:
  - `assert_init` — creates/truncates `$ASSERTLOG`, registers its cleanup. Called once, by `main`.
  - `expect_eq <label> <actual> <want>` — exact string equality.
  - `expect_ge <label> <actual> <min>` — numeric floor; a non-integer `<actual>` **fails** rather than erroring.
  - `expect_contains <label> <haystack> <needle>` — fixed-string search; the haystack is never echoed.
  - `expect_absent <label> <haystack> <needle>` — the inverse.
  - `assert_summary <report-file>` — appends the roll-up, prints a one-line console total, returns 1 if any assertion failed.
  - Every helper prints its `PASS:`/`FAIL:` line on **stdout** and returns **0**.

- [ ] **Step 1: Write the failing self-test**

Create `chaos/assert-selftest.sh`:

```bash
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
# This is the whole design: all 20 scenarios must still run after one fails.
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
```

Then `chmod +x chaos/assert-selftest.sh`.

- [ ] **Step 2: Run it to watch it fail**

Run: `bash chaos/assert-selftest.sh`
Expected: FAIL — `chaos/assert.sh: No such file or directory`.

- [ ] **Step 3: Write the helpers**

Create `chaos/assert.sh`:

```bash
# Machine-checked assertions for the chaos harness.
#
# Sourced by chaos/run.sh and by chaos/assert-selftest.sh (which exercises these
# helpers with no cluster). Each expect_* helper prints one PASS/FAIL line on
# stdout — so a call inside a `{ ... } | record ...` block lands in the report
# beside the value it checked — and appends the same outcome to $ASSERTLOG.
#
# $ASSERTLOG is a FILE, not a shell variable, and that is load-bearing: record()
# is fed by a pipeline, a pipeline runs in a subshell, and a counter incremented
# there would be discarded the moment the block ended. A file survives.
#
# No helper ever returns non-zero. The harness runs under `set -e`; a failing
# assertion must let the remaining scenarios run and surface at the end, in the
# exit code, rather than aborting the suite half way through.
#
# Assertions are written at kubeagent's contract level, never Kubernetes'. Assert
# on what kubeagent did with an API message — a finding is present, a counter, an
# exit code — never on the API server's wording, which moves between minors and
# would go red on a change that broke nothing.

assert_init() {
  ASSERTLOG="${ASSERTLOG:-$(mktemp)}"
  : > "$ASSERTLOG"
  trap 'rm -f "${ASSERTLOG:-}"' EXIT
}

# _assert_record <PASS|FAIL> <label> <detail>
_assert_record() {
  printf '%s: %s %s\n' "$1" "$2" "$3"
  printf '%s\t%s %s\n' "$1" "$2" "$3" >> "$ASSERTLOG"
  # A failure also goes to the console, so an operator watching a 40-minute run
  # sees it when it happens instead of at the end.
  if [ "$1" = FAIL ]; then printf 'ASSERTION FAILED: %s %s\n' "$2" "$3" >&2; fi
  return 0
}

# expect_eq <label> <actual> <want> — exact string equality.
expect_eq() {
  if [ "$2" = "$3" ]; then
    _assert_record PASS "$1" "($2)"
  else
    _assert_record FAIL "$1" "(got '$2', want '$3')"
  fi
  return 0
}

# expect_ge <label> <actual> <min> — numeric floor. A non-integer actual FAILS
# rather than erroring: ready_replicas() prints "?" when the query itself failed,
# and "couldn't tell" must never be coerced into a confident number.
expect_ge() {
  case "$2" in
    ''|*[!0-9]*)
      _assert_record FAIL "$1" "(got '$2', want an integer >= $3)"
      return 0 ;;
  esac
  if [ "$2" -ge "$3" ]; then
    _assert_record PASS "$1" "($2 >= $3)"
  else
    _assert_record FAIL "$1" "(got $2, want >= $3)"
  fi
  return 0
}

# expect_contains <label> <haystack> <needle> — fixed-string search. The haystack
# is never echoed into the message: it is usually a whole scan.
expect_contains() {
  if printf '%s\n' "$2" | grep -qF -- "$3"; then
    _assert_record PASS "$1" "(found '$3')"
  else
    _assert_record FAIL "$1" "(missing '$3')"
  fi
  return 0
}

# expect_absent <label> <haystack> <needle> — the inverse of expect_contains.
expect_absent() {
  if printf '%s\n' "$2" | grep -qF -- "$3"; then
    _assert_record FAIL "$1" "(found '$3', want it absent)"
  else
    _assert_record PASS "$1" "(absent '$3')"
  fi
  return 0
}

# assert_summary <report-file> — append the roll-up to the report, print the
# totals to the console, and return 1 when anything failed. This return status is
# what makes ./chaos/run.sh a gate rather than a report.
assert_summary() {
  local out="$1" total failed
  total="$(wc -l < "$ASSERTLOG" | tr -d ' ')"
  failed="$(grep -c '^FAIL' "$ASSERTLOG" || true)"
  {
    printf '\n## Assertion summary\n\n'
    printf -- '- assertions run: %s\n' "$total"
    printf -- '- failed: %s\n' "$failed"
    if [ "$failed" -gt 0 ]; then
      printf '\n```text\n'
      grep '^FAIL' "$ASSERTLOG"
      printf '```\n'
    fi
  } >> "$out"
  printf '\nassertions: %s run, %s failed\n' "$total" "$failed"
  [ "$failed" -eq 0 ]
}
```

- [ ] **Step 4: Run the self-test to verify it passes**

Run: `bash chaos/assert-selftest.sh`
Expected: every line `ok`, final line `assert-selftest: all checks passed`, exit 0.

- [ ] **Step 5: Wire the helpers into the harness**

In `chaos/run.sh`, immediately after the `: "${OUT:=docs/testing/chaos-results.md}"` line (line 28), add:

```bash
# Assertion helpers (expect_eq / expect_ge / expect_contains / expect_absent) and
# the summary that turns their outcomes into this script's exit code.
# shellcheck source=chaos/assert.sh
. "$ROOT/chaos/assert.sh"
```

In `main`, immediately after `: > "$OUT"` (line 1212), add:

```bash
  assert_init
```

At the very end of `main`, after the teardown `if`/`else`, add as the **last** statement:

```bash
  # Non-zero when any assertion failed: this is what makes the harness a gate.
  assert_summary "$OUT"
```

`main` is the script's last command, so its return status becomes the script's exit code. Do not add an `exit` — `set -e` already propagates it.

- [ ] **Step 6: Verify the wiring**

Run: `bash -n chaos/run.sh`
Expected: no output, exit 0.

Run: `./chaos/run.sh --only 02 --out /tmp/chaos-task1.md`
Expected: exit 0; the report ends with an `## Assertion summary` section reading `- assertions run: 0` and `- failed: 0` (no scenario has been converted yet — the baseline is Task 2).

Now prove the exit code is real. Temporarily append `expect_eq 'wiring smoke' a b` just before `assert_summary "$OUT"`, re-run the same command, and confirm: the console shows `ASSERTION FAILED: wiring smoke`, the report's summary reads `- failed: 1` and lists the line, and `echo $?` is **1**. Remove the temporary line and confirm `git diff chaos/run.sh` no longer contains it.

- [ ] **Step 7: Commit**

```bash
git add chaos/assert.sh chaos/assert-selftest.sh chaos/run.sh
git commit -s -m "chaos: add machine-checked assertion helpers

expect_eq / expect_ge / expect_contains / expect_absent each print a PASS or
FAIL line beside the value they check and append the outcome to a file. The log
is a file rather than a shell variable because record() is fed by a pipeline,
and a counter incremented in that subshell would be lost. main ends with
assert_summary, so the harness now exits non-zero when an assertion fails
instead of producing a report nobody reads.

A non-integer value fails rather than erroring: ready_replicas() prints \"?\"
when the query itself failed, and \"couldn't tell\" must never be coerced to 0."
```

---

### Task 2: Convert the baseline, scenario 2 and scenario 3

**Files:**
- Modify: `chaos/run.sh` — `main`'s baseline block (line ~1226), `scenario_02_certs` (line 766), `scenario_03_diskfull` (line 151)

**Interfaces:**
- Consumes: `expect_eq`, `expect_contains` from Task 1.
- Produces: the conversion shape every later batch follows — capture `out`/`rc`/`body` first, then print the scan plus an `--- assertions ---` block into `record`.

**Observed values from the last full green run** (use these as the expected values):

| where | value |
| --- | --- |
| baseline | `Cluster: Healthy — 3/3 nodes Ready`, `No issues found.` |
| 3 | `Cluster: Degraded — 3/3 nodes Ready`, `✗ node <worker> SchedulingDisabled`, `⚠ Unschedulable: No node can schedule this pod` |

- [ ] **Step 1: Convert the baseline**

In `main`, replace:

```bash
  log "baseline healthy scan"
  { scan 2>&1 || true; } | record "Baseline (healthy cluster)" "baseline"
```

with:

```bash
  log "baseline healthy scan"
  local bout brc bbody
  bout="$(scan 2>&1)" && brc=0 || brc=$?
  bbody="$(scan_body "$bout")"
  {
    printf '%s\n' "$bout"
    printf '\n--- assertions ---\n'
    expect_eq       "baseline scan exit code"          "$brc" 0
    expect_contains "baseline cluster verdict"         "$bbody" "Cluster: Healthy"
    expect_contains "baseline reports nothing to fix"  "$bbody" "No issues found."
  } | record "Baseline (healthy cluster)" "baseline"
```

`out="$(cmd)" && rc=0 || rc=$?` is the only assignment-plus-status form that is safe under `set -euo pipefail`. `out="$(cmd)"; rc=$?` aborts the run the moment `cmd` exits non-zero.

- [ ] **Step 2: Leave scenario 2 unasserted, and say why**

`scenario_02_certs` is a documented skip: it computes no value and runs no scan. An assertion here could only check that the skip text is the skip text, which is theatre — a vacuously-passing assertion is worse than none. Add the reason as a comment above the `printf` so the next reader does not "fix" the omission:

```bash
scenario_02_certs() {   # documented skip (can't force cert expiry on Kind)
  log "scenario 2: expired certificates (skipped)"
  # No assertion here on purpose: this scenario runs no scan and computes no
  # value, so any expect_* call could only compare the skip text with itself.
  # The TLS branch is asserted in internal/connectivity's unit tests instead.
```

- [ ] **Step 3: Convert scenario 3**

Replace the scan line in `scenario_03_diskfull`:

```bash
  sleep 12
  { scan 2>&1 || true; } | record "3. Disk full on control plane (node cordon + unschedulable pod)" "detected: SchedulingDisabled + Unschedulable"
```

with:

```bash
  sleep 12
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"              "$rc" 0
    expect_contains "cluster verdict"             "$body" "Cluster: Degraded"
    expect_contains "cordoned node named"         "$body" "$node"
    expect_contains "cordon reported"             "$body" "SchedulingDisabled"
    expect_contains "unschedulable pod reported"  "$body" "Unschedulable"
  } | record "3. Disk full on control plane (node cordon + unschedulable pod)" "detected: SchedulingDisabled + Unschedulable"
```

`$node` is already a `local` in this function (line 153) and is still in scope.

- [ ] **Step 4: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 03 --out /tmp/chaos-task2-03.md
```

Expected: exit 0; `/tmp/chaos-task2-03.md` carries three PASS lines under Baseline and five under scenario 3; the summary reads `- assertions run: 8`, `- failed: 0`.

- [ ] **Step 5: Perturbation evidence**

Value replay (a) for all eight assertions, plus in-place (b) for one: change scenario 3's `"Cluster: Degraded"` to `"Cluster: Healthy"`, re-run `--only 03`, confirm the `FAIL` line, the `ASSERTION FAILED:` console line and a non-zero exit, then revert and confirm `git diff` is clean of it. Record both in the task report.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert the baseline and scenario 3

The baseline now fails the run if a settled cluster does not read Healthy with
nothing to fix, and scenario 3 fails if the cordon and the unschedulable pod
stop being reported. Scenario 2 stays unasserted on purpose and says so: it is a
documented skip that computes no value."
```

---

### Task 3: Convert scenarios 4 and 5

**Files:**
- Modify: `chaos/run.sh` — `scenario_04_networkpolicy` (line 180), `scenario_05_coredns` (line 165)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 4 | replicas `1` / `0` / `1`; `chaos-np/blocked` lines under deny-all `1`; in the recovery scan `0` |
| 5 | `Cluster: Degraded — 3/3 nodes Ready`; `✗ kube-system/coredns  Deployment  1/2 Degraded  · 4 restarts, last 28s ago` |

- [ ] **Step 1: Convert scenario 4**

Scenario 4's causal triple is the point of the scenario, and `ready_replicas` returns `"?"` when the query itself failed — `expect_eq` against `1`/`0`/`1` fails on `"?"` with no special casing.

Hoist the two line counts out of the `printf` calls so they can be asserted, then add the assertion block. In `scenario_04_networkpolicy`, extend the `local` on line 182 with `blocked_lines recovery_lines`, and after `recovery_scan="$(scan 2>&1 || true)"` add:

```bash
  blocked_lines="$(scan_body "$blocked_scan"  | grep -c 'chaos-np/blocked' || true)"
  recovery_lines="$(scan_body "$recovery_scan" | grep -c 'chaos-np/blocked' || true)"
```

Replace the two inline `$(scan_body … | grep -c …)` substitutions inside the report block with `"$blocked_lines"` and `"$recovery_lines"` so the report and the assertions read the same number rather than counting twice. Then, immediately before the closing `} | record` of that block, add:

```bash
    echo
    echo '--- assertions ---'
    expect_eq "blocked ready replicas before the policy" "$baseline"  1
    expect_eq "blocked ready replicas under deny-all"    "$broken"    0
    expect_eq "blocked ready replicas after deletion"    "$recovered" 1
    expect_ge "chaos-np/blocked reported under deny-all" "$blocked_lines"  1
    expect_eq "chaos-np/blocked gone from the recovery scan" "$recovery_lines" 0
```

Do **not** assert on `$probe_event`: that string is the kubelet's own probe-failure wording, not kubeagent's. It stays in the report as evidence a human reads when a cell goes red.

- [ ] **Step 2: Convert scenario 5**

Replace the scan line in `scenario_05_coredns`:

```bash
  sleep 30
  { scan 2>&1 || true; } | record "5. Broken DNS (CoreDNS crash)" \
    "expect: …"
```

with (keeping the existing prose verdict argument **byte-for-byte unchanged**):

```bash
  sleep 30
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"            "$rc" 0
    expect_contains "cluster verdict"           "$body" "Cluster: Degraded"
    expect_contains "coredns named as degraded" "$body" "kube-system/coredns"
  } | record "5. Broken DNS (CoreDNS crash)" \
    "expect: …"
```

The scenario's own prose says the named finding is timing-dependent — caught between restarts the pods read `0/1 Running` with no `CrashLoopBackOff` line at all, which is a pass. So there is no assertion on the finding name, and none on the restart count, whose rendered position moves with the format. `Degraded` plus the named workload is the invariant, and that is what is asserted.

- [ ] **Step 3: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 04 --out /tmp/chaos-task3-04.md
./chaos/run.sh --only 05 --out /tmp/chaos-task3-05.md
```

Expected: both exit 0; five PASS lines in scenario 4, three in scenario 5, plus the baseline's three in each report.

- [ ] **Step 4: Perturbation evidence**

Value replay (a) for all eight, **including `"?"` for each of the three replica assertions** — that case is the reason `expect_eq` is used there rather than a numeric compare. In-place (b): change `expect_eq "blocked ready replicas under deny-all" "$broken" 0` to `1`, re-run `--only 04`, confirm FAIL and non-zero exit, revert.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 4 and 5

Scenario 4's causal triple (1 / 0 / 1 ready replicas around the deny-all policy)
and the appearance and disappearance of the degraded workload are now checked,
not merely printed; a \"?\" from a failed query fails the assertion instead of
reading as a confident zero. Scenario 5 asserts the Degraded verdict and the
named workload only — its own prose already records that the specific finding is
timing-dependent."
```

---

### Task 4: Convert scenarios 6, 7 and 8

**Files:**
- Modify: `chaos/run.sh` — `scenario_06_lb` (line 292), `scenario_07_oom` (line 772), `scenario_08_nsdelete` (line 303)

**Interfaces:**
- Consumes: `expect_eq`, `expect_contains`, `expect_absent` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 6 | `✗ chaos-lb/web  LoadBalancer  no external address · 13s ago` |
| 7 | `✗ chaos-oom/oom-target  Deployment  0/1 Degraded`, `⚠ OOMKilled: Container exceeded its memory limit and was killed` |
| 8 | `Cluster: Healthy — 3/3 nodes Ready`, `No issues found. ✅` |

All three follow the Task 2 shape: capture `out`/`rc`/`body`, print, assert.

- [ ] **Step 1: Convert scenario 6**

```bash
  sleep 10
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"            "$rc" 0
    expect_contains "pending Service named"     "$body" "chaos-lb/web"
    expect_contains "no external address flagged" "$body" "no external address"
  } | record "6. Cloud load balancer failure (LoadBalancer pending)" "detected: Service issues - no external address"
```

- [ ] **Step 2: Convert scenario 7**

```bash
  sleep 35
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"           "$rc" 0
    expect_contains "OOM workload named"       "$body" "chaos-oom/oom-target"
    expect_contains "OOMKilled diagnosed"      "$body" "OOMKilled"
  } | record "7. OOMKilled critical workload (memory-hog, 64Mi limit)" "detected: OOMKilled + container requests/limits"
```

- [ ] **Step 3: Convert scenario 8**

This is a boundary scenario: the deleted namespace is a **blind spot**, and the scan is supposed to report nothing. The assertion has to say that positively, otherwise it would pass on a scan that crashed.

```bash
  kubectl --context "$CTX" delete ns chaos-doomed --wait=true >/dev/null 2>&1 || true
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"                    "$rc" 0
    expect_contains "cluster verdict"                   "$body" "Cluster: Healthy"
    expect_contains "stateless scanner reports nothing" "$body" "No issues found."
    expect_absent   "the deleted namespace is not reported" "$body" "chaos-doomed"
  } | record "8. Accidental namespace deletion" "boundary: stateless scanner reports no issues (no expected-state tracking)"
```

- [ ] **Step 4: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 06 --out /tmp/chaos-task4-06.md
./chaos/run.sh --only 07 --out /tmp/chaos-task4-07.md
./chaos/run.sh --only 08 --out /tmp/chaos-task4-08.md
```

Expected: all three exit 0, with 3 / 3 / 4 PASS lines respectively plus the baseline's three.

- [ ] **Step 5: Perturbation evidence**

Value replay (a) for all ten. In-place (b): break scenario 7's injection by raising the memory limit (`64Mi` → `512Mi` in the inline manifest) so nothing is OOM-killed, re-run `--only 07`, confirm `OOMKilled diagnosed` FAILs and the harness exits non-zero, then revert. That perturbation is worth the extra run: it proves the assertion tracks the fault rather than the report's boilerplate.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 6, 7 and 8

Scenario 8's boundary is asserted positively — a healthy verdict, nothing to
fix, and no mention of the deleted namespace — so a scan that crashed can no
longer read the same as a scan that correctly found nothing."
```

---

### Task 5: Convert scenarios 9, 9b, 10 and 11

**Files:**
- Modify: `chaos/run.sh` — `scenario_09_rollout` (line 312), `scenario_10_credleak` (line 340), `scenario_11_kubelet` (line 350)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains`, `expect_absent` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 9 | `⚠ ImagePullBackOff: Bad image reference or registry authentication` |
| 9b | `after --fix: nginx:1.27-alpine`, `after --rollback: nginx:does-not-exist-9999`, `rollback audit records: 1` |
| 10 | `✗ chaos-cred/app-config  ConfigMap[AWS_SECRET_ACCESS_KEY]  AWS access key` |
| 11 | `Cluster: Degraded — 2/3 nodes Ready`, `✗ node <worker> NotReady: KubeletNotReady — container runtime is down`, kubelet `/healthz` body `ok` |

- [ ] **Step 1: Convert scenario 9 (the outage scan)**

```bash
  sleep 18
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"          "$rc" 0
    expect_contains "bad-image workload named" "$body" "chaos-rollout/web"
    expect_contains "image pull failure diagnosed" "$body" "ImagePullBackOff"
  } | record "9. Faulty rolling deployment (bad image)" "detected: ImagePullBackOff"
```

- [ ] **Step 2: Convert scenario 9b (the fix/rollback round trip)**

Hoist the audit count into a variable so it can be both printed and asserted, then assert the round trip:

```bash
  local rollback_records
  rollback_records="$(grep -c '"disposition":"rollback"' "$alog" 2>/dev/null || true)"
  {
    echo "after --fix:      $after_fix"
    echo "after --rollback: $after_rollback"
    printf 'rollback audit records: %s\n' "$rollback_records"
    printf '\n--- assertions ---\n'
    expect_eq "--fix restored a working image"      "$after_fix"      "nginx:1.27-alpine"
    expect_eq "--rollback restored the pre-fix image" "$after_rollback" "nginx:does-not-exist-9999"
    expect_ge "rollback recorded in the audit log"  "$rollback_records" 1
  } | record "9b. Fix then rollback (audit-log round trip)" "rollback restores the pre-fix image"
```

These two image strings are kubeagent's own round trip — `--fix` chose one and `--rollback` restored the other — not Kubernetes wording, so pinning them exactly is correct.

- [ ] **Step 3: Convert scenario 10**

The value assertion here is the one that matters: `--lint-secrets` reports **location and pattern only**, never the credential itself.

```bash
  sleep 3
  local out rc body
  out="$(scan --lint-secrets 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"                "$rc" 0
    expect_contains "leak location named"           "$body" "chaos-cred/app-config"
    expect_contains "credential pattern named"      "$body" "AWS access key"
    expect_absent   "the credential value is never printed" "$body" "AKIAIOSFODNN7EXAMPLE"
  } | record "10. Security credential leak (--lint-secrets)" "detected: credential warning (location+pattern only)"
```

- [ ] **Step 4: Convert scenario 11**

```bash
  local h; h="$(kubectl --context "$CTX" get --raw "/api/v1/nodes/$node/proxy/healthz" 2>/dev/null || echo '<unreachable>')"
  local out rc body
  out="$(scan --kubelet-health 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    # A precondition, not a kubeagent claim: if the kubelet stopped self-reporting
    # "ok" the no-double-flag assertion below would pass for the wrong reason.
    expect_eq       "precondition: kubelet /healthz still reports ok" "$h" "ok"
    expect_eq       "scan exit code"        "$rc" 0
    expect_contains "cluster verdict"       "$body" "Cluster: Degraded"
    expect_contains "runtime-down node flagged NotReady" "$body" "NotReady"
    expect_contains "the affected node is named"         "$body" "$node"
  } | record "11. Kubelet health probe via nodes/proxy (worker runtime down, --kubelet-health)" "boundary: node NotReady flagged by the base scan; kubelet /healthz reports '$h', so --kubelet-health probes every node and does not double-flag it (no false positive)"
```

Then add the no-double-flag assertion, which needs one discovery step because the section heading is not recorded in the last report: run `./chaos/run.sh --only 11 --out /tmp/chaos-task5-11.md`, read the kubelet-health section kubeagent actually renders in that file, and add **one** `expect_absent` asserting the node name does not appear inside it — extracting the section with `awk` between its heading and the next blank-line-separated heading, exactly as `scan_body` extracts the pre-explanation body. If the scan renders no kubelet-health section at all when every kubelet is healthy, assert that instead, with `expect_absent` on the heading. Record which of the two the run showed, and why, in the task report.

- [ ] **Step 5: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 09 --out /tmp/chaos-task5-09.md
./chaos/run.sh --only 10 --out /tmp/chaos-task5-10.md
./chaos/run.sh --only 11 --out /tmp/chaos-task5-11.md
```

Expected: all exit 0. Scenario 11 restores the worker's container runtime on its way out; if the run is interrupted, re-run it before the next task so the cluster is not left with a NotReady node.

- [ ] **Step 6: Perturbation evidence**

Value replay (a) for every assertion. In-place (b): flip scenario 10's `expect_absent` to `expect_contains` on the same needle, re-run `--only 10`, and confirm it FAILs — that demonstrates the value genuinely is absent rather than the needle being unmatchable. Revert.

- [ ] **Step 7: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 9, 9b, 10 and 11

The --fix/--rollback round trip now fails the run if the pre-fix image is not
restored or the rollback goes unaudited, and --lint-secrets is asserted to print
the location and the pattern and never the credential itself. Scenario 11 guards
its boundary with a precondition on the kubelet's own /healthz answer, so the
no-double-flag assertion cannot pass for the wrong reason."
```

---

### Task 6: Convert scenarios 12 and 13

**Files:**
- Modify: `chaos/run.sh` — `scenario_12_watch` (line 374), `scenario_13_slo` (line 459)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 12 | `NEW Deployment/chaos-watch/web:ImagePullBackOff`, `RESOLVED … (fired for 14s)`; firing `5`, resolved `2`, distinct objects `2`; write-verb log lines `0` |
| 13 | `kubeagent_slo_target_ratio{cluster="kind-kubeagent-chaos"} 0.999`; SLO alerts `0`, Deployment alerts `6`, total lines `7` |

- [ ] **Step 1: Convert scenario 12**

Hoist the three counters and the transition log into variables so the report and the assertions read the same values:

```bash
  local transitions firing_n resolved_n distinct_n write_verbs
  transitions="$(grep -E 'kubeagent: (\[[^]]*\] )?(NEW|RESOLVED|FLAPPING) ' "$wlog" || true)"
  firing_n="$(grep -c '"status":"firing"' "$alerts" 2>/dev/null || true)"
  resolved_n="$(grep -c '"status":"resolved"' "$alerts" 2>/dev/null || true)"
  distinct_n="$(grep -o '"kind":"[^"]*","namespace":"[^"]*","name":"[^"]*"' "$alerts" 2>/dev/null | sort -u | wc -l)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"
```

Replace the corresponding inline substitutions in the report block with these variables (the `--- daemon transition log ---` section prints `${transitions:-<no transition lines logged>}`), and add before the closing `} | record`:

```bash
    echo
    echo '--- assertions ---'
    expect_contains "NEW transition for the broken Deployment" "$transitions" "NEW Deployment/$ns/web"
    expect_contains "RESOLVED transition after the repair"     "$transitions" "RESOLVED Deployment/$ns/web"
    expect_ge "firing notifications delivered"      "$firing_n"   1
    expect_eq "distinct objects alerted"            "$distinct_n" 2
    expect_eq "resolved notifications (one per object)" "$resolved_n" 2
    expect_eq "daemon log mentions no write verb"   "$write_verbs" 0
```

`firing_n` is a floor, not an exact value: the Deployment walks `Degraded → ErrImagePull → ImagePullBackOff` and the count moves with timing. `resolved_n` and `distinct_n` are exact — "exactly one resolved alert per broken object" is the per-object rollup this scenario exists to protect.

- [ ] **Step 2: Convert scenario 13**

Hoist the counters and extract the one exact metric:

```bash
  local slo_alerts dep_alerts total_lines target
  slo_alerts="$(grep -c '"kind":"SLO"' "$alerts" 2>/dev/null || true)"
  dep_alerts="$(grep -c '"kind":"Deployment"' "$alerts" 2>/dev/null || true)"
  total_lines="$(wc -l < "$alerts" 2>/dev/null | tr -d ' ')"
  target="$(printf '%s\n' "$broken" | awk '/^kubeagent_slo_target_ratio/{print $2}')"
```

Use those variables in the existing `printf` lines, and add before the closing `} | record`:

```bash
    echo
    echo '--- assertions ---'
    expect_eq "SLO target ratio"                        "$target"      "0.999"
    expect_eq "SLO pages suppressed by the coverage gate" "$slo_alerts"  0
    expect_ge "object alerts still delivered"           "$dep_alerts"  1
    expect_ge "the webhook pipe delivered something"    "$total_lines" 1
```

Nothing asserts the availability ratio or the burn rate: the scenario's own prose records that both are timing-dependent, and pinning them would be asserting a number this harness cannot reproduce. The three assertions that matter are the exact target, the suppressed page, and the two non-zero floors that prove the 0 is a working gate rather than a dead pipe.

- [ ] **Step 3: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 12 --out /tmp/chaos-task6-12.md
./chaos/run.sh --only 13 --out /tmp/chaos-task6-13.md
```

Expected: both exit 0, six PASS lines in 12 and four in 13.

- [ ] **Step 4: Perturbation evidence**

Value replay (a) for all ten, including `total_lines=0` for scenario 13 — the case the scenario's prose calls a scenario FAILURE rather than a suppressed page. In-place (b): change scenario 12's `expect_eq "distinct objects alerted" "$distinct_n" 2` to `3`, re-run `--only 12`, confirm FAIL and non-zero exit, revert.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 12 and 13

The watch daemon's NEW and RESOLVED transitions, the per-object alert rollup and
the read-only guarantee are now checked. Scenario 13 asserts the exact SLO target
ratio, the suppressed page, and the two non-zero floors that distinguish a
working coverage gate from a webhook that never delivered anything."
```

---

### Task 7: Convert scenarios 14 and 15

**Files:**
- Modify: `chaos/run.sh` — `scenario_14` (line 524), `scenario_15_multicluster` (line 601)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains`, `expect_absent` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 14 | calls `1`; explanation notifications `1`; plain firing `2`; prompts leaking `0`; endpoint-path log lines `0`; write verbs `0`; `kubeagent_explain_allowed_total 1`, `kubeagent_explain_throttled_total 4` |
| 15 | `HTTP 200`; `kubeagent_clusters_total 3`; `kubeagent_cluster_up{cluster="alias-b"} 1`, `{cluster="dead"} 0`, `{cluster="kind-kubeagent-chaos"} 1`; kubeconfig material `0`; write verbs `0` |

Scenario 14 uses a **local stub endpoint**, never the Anthropic backend, so it runs with no API key and these assertions hold in the nightly.

- [ ] **Step 1: Convert scenario 14**

Hoist every counter, and pull the two explain metrics out of `$metrics`:

```bash
  local calls_n expl_n firing_n leaks path_lines write_verbs allowed throttled
  calls_n="$(wc -l < "$calls" 2>/dev/null | tr -d ' ')"
  expl_n="$(grep -c '"reason":"explanation"' "$alerts" 2>/dev/null || true)"
  firing_n="$(grep -c '"reason":"new"' "$alerts" 2>/dev/null || true)"
  leaks="$(grep -cE '"prompt":[^\n]*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-z]{6,}|kubeagent-chaos-worker)' "$calls" 2>/dev/null || true)"
  path_lines="$(grep -c "127.0.0.1:$sport/v1" "$wlog" || true)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"
  allowed="$(printf '%s\n' "$metrics"   | awk '/^kubeagent_explain_allowed_total/{print $2}')"
  throttled="$(printf '%s\n' "$metrics" | awk '/^kubeagent_explain_throttled_total/{print $2}')"
```

Use them in the existing `printf`/`sed` lines (replacing the inline pipelines), and add before the closing `} | record`:

```bash
    echo
    echo '--- assertions ---'
    expect_eq "model calls made (budget 1)"          "$calls_n"   1
    expect_eq "explanation notifications delivered"  "$expl_n"    1
    expect_ge "plain firing notifications unaffected" "$firing_n" 1
    expect_eq "explain budget admitted one"          "$allowed"   1
    expect_ge "explain budget throttled the rest"    "$throttled" 1
    expect_eq "no prompt leaks pod or node detail"   "$leaks"     0
    expect_eq "no log line carries the endpoint path" "$path_lines" 0
    expect_eq "daemon log mentions no write verb"    "$write_verbs" 0
    expect_contains "the explanation names the stub model" "$expl" "chaos-stub"
```

- [ ] **Step 2: Convert scenario 15**

Hoist the per-cluster readings out of the grep pipelines:

```bash
  local clusters_total up_ctx up_alias up_dead issue_ctx issue_alias issue_dead kubeconfig_material write_verbs
  clusters_total="$(printf '%s\n' "$metrics" | awk '/^kubeagent_clusters_total/{print $2}')"
  up_ctx="$(printf   '%s\n' "$metrics" | awk -v c="cluster=\"$CTX\""      '$0 ~ /^kubeagent_cluster_up/ && index($0,c){print $2}')"
  up_alias="$(printf '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"alias-b\""){print $2}')"
  up_dead="$(printf  '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"dead\""){print $2}')"
  issue_ctx="$(printf   '%s\n' "$metrics" | grep -c "^kubeagent_issue_active{cluster=\"$CTX\"" || true)"
  issue_alias="$(printf '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="alias-b"' || true)"
  issue_dead="$(printf  '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="dead"' || true)"
  kubeconfig_material="$(grep -cE 'BEGIN CERTIFICATE|client-key-data|client-certificate-data|token:' "$wlog" || true)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"
```

Use them in the existing `printf`/`sed` lines, and add before the closing `} | record`:

```bash
    echo
    echo '--- assertions ---'
    expect_eq "readyz stays 200 with one target dead" "$ready_code" 200
    expect_eq "cluster roster size"                   "$clusters_total" 3
    expect_eq "the real cluster is up"                "$up_ctx"   1
    expect_eq "its second label is up"                "$up_alias" 1
    expect_eq "the unreachable target is down"        "$up_dead"  0
    expect_ge "the broken workload is seen under the real cluster label" "$issue_ctx"   1
    expect_ge "and again under its second label"                         "$issue_alias" 1
    expect_eq "no issue is attributed to the unreachable target"         "$issue_dead"  0
    expect_eq "no log line carries kubeconfig material" "$kubeconfig_material" 0
    expect_eq "daemon log mentions no write verb"       "$write_verbs" 0
```

The issue counts are floors, not exact values: the issue name walks `ErrImagePull → ImagePullBackOff` with timing. The label attribution — seen under both live labels, never under `dead` — is exact, and that is what the multi-cluster merge is for.

- [ ] **Step 3: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 14 --out /tmp/chaos-task7-14.md
./chaos/run.sh --only 15 --out /tmp/chaos-task7-15.md
```

Expected: both exit 0, nine PASS lines in 14 and ten in 15.

- [ ] **Step 4: Perturbation evidence**

Value replay (a) for all nineteen, including an empty `allowed` (the metric missing entirely) — `expect_eq` against `1` must fail, not skip. In-place (b): change scenario 14's `--explain-budget 1` to `5`, re-run `--only 14`, confirm `model calls made (budget 1)` FAILs, revert.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 14 and 15

The explain budget, the throttle, the egress discipline and the endpoint
redaction are now checked rather than printed, and the multi-cluster hub asserts
that the broken workload is attributed to both live cluster labels and never to
the unreachable one."
```

---

### Task 8: Convert scenarios 16, 17 and 18

**Files:**
- Modify: `chaos/run.sh` — `scenario_16_operators` (line 680), `scenario_17_gitops` (line 797), `scenario_18_capacity` (line 876)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains` from Task 1.

These three already end with a `--- gate checks ---` block. Keep the block, hoist each value it prints into a variable, and add an `--- assertions ---` block after it that consumes the same variables — the report keeps showing the raw value beside the verdict.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 16 | Widget `0`; CR spec content `0`; `Certificate     1 unhealthy` |
| 17 | `GITOPS DRIFT  (advisory — …)`; `Kustomization   1 stale, 1 blocked`; doomed `1`; parked suspended `1`; repo URL/token `0` |
| 18 | control-plane excluded `1`; besteffort `1`; limitonly `1`; trainer `1`; metrics-server unavailable `2`; banned vocabulary `0` |

- [ ] **Step 1: Convert scenario 16**

```bash
  local widget_n spec_n cert_line
  widget_n="$(printf '%s\n' "$body" | grep -c 'Widget' || true)"
  spec_n="$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|doomed\.chaos\.invalid' || true)"
  cert_line="$(printf '%s\n' "$body" | grep -m1 'Certificate' || true)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'unadapted Widget kind in report: %s\n' "$widget_n"
    printf 'CR spec content in report:       %s\n' "$spec_n"
    printf 'Certificate line:                %s\n' "$cert_line"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
    printf '\n--- assertions ---\n'
    expect_eq       "unadapted CRD stays out of the report" "$widget_n" 0
    expect_eq       "no custom-resource spec content in the report" "$spec_n" 0
    expect_contains "cert-manager Certificate adapter fired" "$cert_line" "Certificate"
    expect_contains "the failing Certificate is counted unhealthy" "$cert_line" "unhealthy"
  } | record "16. Operator/CRD adapters (--operators)" "detected: cert-manager Certificate Ready=False; unadapted CRD absent (0); no CR spec content (0)"
```

- [ ] **Step 2: Convert scenario 17**

```bash
  local drift_line ks_line doomed_n parked_n leak_n
  drift_line="$(printf '%s\n' "$body" | grep -m1 'GITOPS DRIFT' || true)"
  ks_line="$(printf '%s\n' "$body" | grep -m1 'Kustomization' || true)"
  doomed_n="$(printf '%s\n' "$body" | grep -c "$ns/doomed" || true)"
  parked_n="$(printf '%s\n' "$body" | grep -cE "$ns/parked +suspended" || true)"
  leak_n="$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|git\.chaos\.invalid' || true)"
```

Use those five in the existing `printf` lines, then add:

```bash
    printf '\n--- assertions ---\n'
    expect_contains "GitOps drift section rendered"   "$drift_line" "GITOPS DRIFT"
    expect_contains "the stale Kustomization is counted" "$ks_line" "stale"
    expect_ge "the failing Kustomization is named"    "$doomed_n" 1
    expect_ge "the suspended Kustomization is named suspended" "$parked_n" 1
    expect_eq "no repo URL or token reaches the report" "$leak_n" 0
```

- [ ] **Step 3: Convert scenario 18**

```bash
  local cap_line cp_n besteffort_n limitonly_n trainer_n nometrics_n banned_n
  cap_line="$(printf '%s\n' "$body" | grep -m1 'CAPACITY' || true)"
  cp_n="$(printf '%s\n' "$body" | grep -cE 'control-plane.*NoSchedule taint' || true)"
  besteffort_n="$(printf '%s\n' "$body" | grep -c "Deployment/$ns/besteffort" || true)"
  limitonly_n="$(printf '%s\n' "$body" | grep -c "Deployment/$ns/limitonly" || true)"
  trainer_n="$(printf '%s\n' "$body" | grep -c "Job/$ns/trainer" || true)"
  nometrics_n="$(printf '%s\n' "$body" | grep -c 'metrics-server unavailable' || true)"
  banned_n="$(printf '%s\n' "$body" | grep -ciE 'peak|over-requested|oversized|waste' || true)"
```

Use those in the existing `printf` lines (keeping the `headroom schedulable` and `cluster verdict` lines as the informational reads they already are), then add:

```bash
    printf '\n--- assertions ---\n'
    expect_contains "capacity section rendered"        "$cap_line" "CAPACITY"
    expect_ge "control-plane excluded from headroom"   "$cp_n"          1
    expect_ge "no-requests rule fired (besteffort)"    "$besteffort_n"  1
    expect_ge "limit-without-request rule fired (limitonly)" "$limitonly_n" 1
    expect_ge "never-schedulable rule fired (trainer)" "$trainer_n"     1
    expect_ge "absent metrics-server is stated"        "$nometrics_n"   1
    expect_eq "capacity output stays inside its vocabulary" "$banned_n" 0
```

- [ ] **Step 4: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 16 --out /tmp/chaos-task8-16.md
./chaos/run.sh --only 17 --out /tmp/chaos-task8-17.md
./chaos/run.sh --only 18 --out /tmp/chaos-task8-18.md
```

Expected: all exit 0, with 4 / 5 / 7 PASS lines. Scenarios 16 and 17 install cert-manager and Flux from upstream release URLs, so each takes several minutes and needs network.

- [ ] **Step 5: Perturbation evidence**

Value replay (a) for all sixteen. In-place (b): change scenario 16's `expect_eq "unadapted CRD stays out of the report" "$widget_n" 0` to `1`, re-run `--only 16`, confirm FAIL and non-zero exit, revert.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 16, 17 and 18

The operator, drift and capacity gate checks kept printing numbers nobody
compared. Each is now hoisted into a variable the report prints and an assertion
consumes, so a rule that stops firing — or a repo URL that starts leaking into a
report designed to be forwarded — fails the run."
```

---

### Task 9: Convert scenarios 19, 20 and 1

**Files:**
- Modify: `chaos/run.sh` — `scenario_19_mcp` (line 965), `scenario_20_rbac` (line 1082), `scenario_01_etcd` (line 136)

**Interfaces:**
- Consumes: `expect_eq`, `expect_ge`, `expect_contains`, `expect_absent` from Task 1.

**Observed values from the last full green run:**

| where | value |
| --- | --- |
| 19 | tools `kubeagent_advisory kubeagent_inspect kubeagent_triage`; write verbs `0`; verdict `degraded`; findings `1`; context `kind-kubeagent-chaos` |
| 20 | core allowed `True`; blocked `certs diskusage logs`; scan exit `0`; blind spots `4`; credential material `0` |
| 1 | `kubeagent: The Kubernetes API server … refused the connection.` |

**`scenario_01_etcd` stays LAST in `run_scenarios()`.** Do not touch the `all=(…)` array.

- [ ] **Step 1: Convert scenario 19**

All five values are already in variables. Add after the existing gate-checks block, before the closing `} | record`:

```bash
    printf '\n--- assertions ---\n'
    expect_eq "advertised tools" "$tools" "kubeagent_advisory kubeagent_inspect kubeagent_triage"
    expect_eq "no tool name carries a write verb" "$write_verbs" 0
    expect_eq "triage verdict" "${got_verdict:-}" "degraded"
    expect_ge "triage findings" "${got_findings:-0}" 1
    expect_eq "the server's context round-trips into the response" "${got_context:-}" "$CTX"
```

`write_verbs` is the string `N/A (no tools/list response — nothing was checked)` when no response arrived, and `expect_eq` against `0` fails on it — exactly as the scenario's prose already requires. That is why this is `expect_eq` on a string and not `expect_ge`.

- [ ] **Step 2: Convert scenario 20**

All five values are already in variables. Add after the existing gate-checks block, before the closing `} | record`:

```bash
    printf '\n--- assertions ---\n'
    expect_eq "core profile is allowed"     "${core_ok:-}" "True"
    expect_eq "exactly the ungranted add-ons are blocked" "${blocked:-}" "certs diskusage logs"
    expect_eq "a missing add-on grant degrades the scan, it does not fail it" "$rc" 0
    expect_ge "refused reads are named as blind spots" "$named" 1
    expect_eq "no credential material in the recorded output" "$leaked" 0
```

The `blocked` list is kubeagent's own feature naming from its own table, never the API server's wording, so pinning it exactly is correct. The last two are the assertions this scenario exists for and the reason it must survive slice 7c: that report becomes a CI artifact.

- [ ] **Step 3: Convert scenario 1**

```bash
  docker stop "$c" >/dev/null
  sleep 5
  local out rc
  out="$(scan 2>&1)" && rc=0 || rc=$?
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_ge       "scan exits non-zero when the API is unreachable" "$rc" 1
    expect_contains "connectivity diagnosed in kubeagent's own words" "$out" "refused the connection"
    expect_absent   "no cluster report is rendered"                   "$out" "Cluster: Healthy"
  } | record "1. etcd quorum loss (control-plane stopped)" "boundary: connectivity diagnosis expected"
  docker start "$c" >/dev/null
```

This scenario's `scan` is the one place a non-zero exit is the correct outcome, and the `|| true` that used to swallow it now becomes a recorded assertion.

- [ ] **Step 4: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
./chaos/run.sh --only 19 --out /tmp/chaos-task9-19.md
./chaos/run.sh --only 20 --out /tmp/chaos-task9-20.md
./chaos/run.sh --only 01 --out /tmp/chaos-task9-01.md
```

Expected: all exit 0, with 5 / 5 / 3 PASS lines. Run `--only 01` **last**: it stops the control-plane container and the API server flaps for a minute or two afterwards. Confirm `kubectl --context kind-kubeagent-chaos get nodes` reports all three Ready before handing the task back.

- [ ] **Step 5: Perturbation evidence**

Value replay (a) for all thirteen, including `write_verbs="N/A (no tools/list response — nothing was checked)"` for scenario 19 — the case the prose calls a FAILURE and not a pass. In-place (b): change scenario 20's `expect_ge "refused reads are named as blind spots" "$named" 1` to `99`, re-run `--only 20`, confirm FAIL and non-zero exit, revert.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: assert scenarios 19, 20 and 1

The MCP server's advertised tool set and the absence of any write verb in it are
now checked, as is the least-privilege scan's contract: it degrades rather than
fails, names what it could not read, and carries no credential material into a
report that is about to be forwarded. Scenario 1's non-zero exit stops being
swallowed by || true and becomes a recorded assertion.

scenario_01_etcd stays last in run_scenarios()."
```

---

### Task 10: Document the harness as a gate

**Files:**
- Modify: `chaos/README.md`
- Modify: `.claude/skills/release/SKILL.md` (Step 3)
- Modify: `CLAUDE.md` — **only** if it describes the chaos gate as a report a human reviews
- Modify: `website/docs/roadmap.md` — only if it describes slice 7a as pending

**Interfaces:**
- Consumes: the finished harness from Tasks 1–9.

- [ ] **Step 1: Update `chaos/README.md`**

Read it first, then add a section in its established voice covering: the harness exits **non-zero** when any assertion fails; every scenario's evidence is checked by `expect_eq` / `expect_ge` / `expect_contains` / `expect_absent` from `chaos/assert.sh`; the report ends with an `## Assertion summary` naming every failure; `bash chaos/assert-selftest.sh` exercises the helpers with no cluster in under a second; and the prose `expect:` paragraphs remain, because when a cell goes red the rationale explaining *why the scenario exists* is what a human needs first.

- [ ] **Step 2: Update the release skill's Step 3**

In `.claude/skills/release/SKILL.md`, the "Cluster-interaction changes" block currently says "Review the results report — every scenario should be green." Replace that sentence with wording that says the harness now decides: a zero exit means every assertion passed, a non-zero exit names the failures in the console and in the report's `## Assertion summary`, and the report is what you read to understand a failure — not what you read to detect one. Leave the rest of Step 3, including the `unset ANTHROPIC_API_KEY` line, unchanged.

- [ ] **Step 3: Check `CLAUDE.md` and the roadmap**

```bash
grep -n 'chaos' CLAUDE.md website/docs/roadmap.md
```

Fix only wording that is now false (a claim that a human reviews the report to decide the gate). Do not add a roadmap entry announcing slice 7a as shipped — that belongs to the release that ships it. If neither file makes such a claim, say so in the task report and change nothing.

- [ ] **Step 4: Verify**

```bash
bash -n chaos/run.sh
bash chaos/assert-selftest.sh
git diff --stat
```

Expected: documentation-only diff. `go.mod` and `go.sum` unchanged.

- [ ] **Step 5: Commit**

```bash
git add chaos/README.md .claude/skills/release/SKILL.md
git commit -s -m "docs: the chaos harness is a gate, not a report

It exits non-zero when an assertion fails, so the release step no longer asks an
operator to eyeball twenty scenarios. The prose rationale stays: it is what a
human needs first when a cell goes red."
```

---

## Slice gate (controller, after Task 10)

Not a task — the controller runs this once, before the whole-branch review.

1. `export PATH=$PATH:/usr/local/go/bin && unset ANTHROPIC_API_KEY`
2. `./chaos/run.sh --recreate` (35–40 minutes; run in the background and watch the log). **Expected: exit 0**, with an `## Assertion summary` reporting every assertion run and `- failed: 0`.
3. Then prove the gate can fail: perturb one scenario's expected value, re-run just that scenario with `--only NN --out /tmp/…`, and confirm a non-zero exit and the named `FAIL`. Revert.
4. `go build ./... && go test -p 2 ./...` — untouched by this slice, must stay green.
5. `git diff main --stat` — no Go file, no `go.mod`, no `go.sum`.
6. `scripts/dco-check.sh main HEAD` — every commit signed off.

Record the total assertion count and the wall-clock of the full run in the ledger: **slice 7c needs the wall-clock** to size the nightly's timeout, and the spec's runner-capacity risk is judged against it.

## Self-review

**Spec coverage.** Slice 7a in the spec is "the helpers, plus all 20 scenarios converted, still on today's single version… Ends with a measurement — wall-clock for a full run." Task 1 builds the helpers and the exit-code wiring; Tasks 2–9 convert the baseline and all 20 scenarios (scenario 2 deliberately unasserted, with the reason recorded in code and in this plan); Task 10 documents it; the slice gate measures the wall-clock. The spec's "every `expect_*` call must be seen to fail" is the Perturbation evidence section and a step in every task. The spec's version machinery (`versions.env`, `--k8s-version`, the workflow) is **not** here — it is slices 7b and 7c.

**Placeholder scan.** Every step carries the code to write or the exact command to run. The one discovery step — scenario 11's kubelet-health section heading, Task 5 Step 4 — names the command that produces the value, both possible outcomes, and what to record; it is a measurement, not a TBD.

**Type consistency.** Helper names and argument order are fixed in Task 1 and used identically in Tasks 2–9: `expect_eq <label> <actual> <want>`, `expect_ge <label> <actual> <min>`, `expect_contains <label> <haystack> <needle>`, `expect_absent <label> <haystack> <needle>`. `assert_init` takes no argument; `assert_summary` takes the report path. Every scenario capture uses the same `out="$(cmd)" && rc=0 || rc=$?` form.

**Known risk this plan accepts.** Ten of these assertions pin an exact count (`resolved_n` 2, `calls_n` 1, `clusters_total` 3, the blocked-feature list, the tool list). Each is a value kubeagent computes from its own tables or its own rollup, not a value Kubernetes phrases — that is the line the spec draws. Any of them that proves timing-dependent on the full run must be converted to `expect_ge` **with the reason recorded**, never deleted.
