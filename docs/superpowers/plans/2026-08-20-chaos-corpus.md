# Chaos Correctness Corpus Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every `./chaos/run.sh` run writes a machine-readable corpus beside its report — one redacted JSON row per scenario naming the injected fault, the assertion outcomes, and whether the scenario was skipped and why — and the nightly chaos matrix credential-scans and uploads it.

**Architecture:** A new `capture()` helper in `chaos/run.sh`, called once per scenario from `run_scenarios()`, slices the scenario's fresh lines out of `$ASSERTLOG`/`$SKIPLOG` by line-count deltas, redacts them through the existing `redact_nodes` seam, and pipes the redacted plaintext into a new `corpus_row()` python3 one-liner that JSON-encodes one row into a mktemp scratch (`$CORPUSTMP`, created in `assert_init` on the same trap line as `$ASSERTLOG`). `main()` promotes the scratch to its final path only after the last scenario returns. **This slice adds NO production Go code** — everything is bash in the chaos harness, one workflow edit, and documentation. The tests are new checks in `chaos/assert-selftest.sh`, which CI already runs on every push (`.github/workflows/ci.yml` line 27) with no cluster and no docker.

**Tech Stack:** bash (`chaos/run.sh`, `chaos/assert.sh`, `chaos/assert-selftest.sh`), python3 (already a harness prerequisite — `preflight` and the CI preflight both require it), GitHub Actions YAML (`.github/workflows/chaos-matrix.yml`), Markdown docs.

## Global Constraints

These bind every task. Copied from the spec (`docs/superpowers/specs/2026-08-18-hypothesis-engine-design.md`, section 4) and the project's standing rules.

- **NEVER execute `./chaos/run.sh` in any form** — no `--only`, no `--context`, nothing. It injects real outages and takes ~40 minutes. **Sourcing** it from `chaos/assert-selftest.sh` probes is safe and is the established pattern: `main()` is guarded by `if [ "${BASH_SOURCE[0]}" = "${0}" ]; then main; fi` (run.sh line 2822), so a source defines functions and parses flags but touches no cluster and no docker. Running `bash chaos/assert-selftest.sh` and `bash -n <file>` is safe and expected.
- **NO production Go code.** No file under `internal/`, no `main.go` change, no `go.mod`/`go.sum` change. `go test` is untouched by this slice.
- **No `schemaVersion` moves.** The corpus is a new artifact, not one of the eight versioned JSON documents. Never run any test with `-update`, for any reason. `internal/report/testdata/golden-scan.txt` stays byte-identical; do NOT regenerate the demo GIF or `website/docs/quickstart.md`.
- **capture() failures must not abort the chaos run** — same contract as `record()`: errors swallowed after a stderr note. The corpus never changes the harness's exit code; the gate stays `assert_summary` alone.
- **Redaction before encoding.** Every row's plaintext passes `redact_nodes` BEFORE `corpus_row` JSON-encodes it — encoding first could split a redaction needle's bytes across escape sequences. `$ASSERTLOG` is NOT pre-redacted (the spec's contrary claim is wrong — see the "spec narrowing" note below), which is exactly why capture must redact.
- **The single-trap rule.** `chaos/assert.sh`'s `assert_init` holds the harness's ONLY `trap ... EXIT` (line 26). `$CORPUSTMP` joins THAT trap line; no task may add a second `trap ... EXIT` anywhere in `run.sh` or `assert.sh` (subshell-local traps inside selftest probes are fine — a subshell's trap dies with the subshell).
- **The `^FAIL`/`^PASS`/`SKIP\t` line-start conventions in `$ASSERTLOG`/`$SKIPLOG` must not be disturbed** — `assert_summary` counts on them (`grep -c '^FAIL'`), and now `capture()` does too. No task edits `_assert_record` or `assert_skip`.
- **Pipeline-subshell hazard:** every `expect_*` call in a scenario sits inside `{ ... } | record ...` — a pipeline runs in a subshell, so accumulating state must live in FILES, never shell variables. `capture()` is called from `run_scenarios()`' loop body, which runs in the MAIN shell — that placement is load-bearing.
- **`set -euo pipefail` is active** (run.sh line 2). Every new statement that can fail must be guarded (`|| true`, `|| { ...; }`, or an `if` condition) unless its failure should genuinely abort the run — for corpus code that is never.
- **Credentials rule:** no secrets, real hostnames, private IPs, real node/context names in any tracked file. Selftest fixtures use synthetic names only (`worker-9.internal.example`, `top-secret-ctx`, `kind-kubeagent-chaos` — established conventions). The one permitted AWS-key literal is `AKIAIOSFODNN7EXAMPLE`.
- **Every commit `git commit -s`** (DCO). No `Co-Authored-By: Claude` trailer, no AI attribution of any kind, anywhere. A commit message must not cite a path under `docs/testing/` or a scenario record ID.
- **TDD:** append the failing selftest checks first, run `bash chaos/assert-selftest.sh`, watch it FAIL (exit 1 — line 494 `[ "$fails" -eq 0 ]` makes the failure count the exit code), then implement, then watch it pass. Report what you actually saw; never claim a red or green run you did not observe.
- **Spec narrowing (record honestly, do not hide):** the spec says the assertion lines are "already scenario-labeled and redaction-safe". Both halves are wrong: `$ASSERTLOG` lines carry no scenario prefix (slicing by line-count delta is the fix) and are unredacted (run.sh lines 2798–2807 prove it — `main()` explicitly redacts the summary built from that log). This plan's design handles both; Task 5's docs state the actual contract.
- **rc semantics (a deliberate spec interpretation):** the spec's example row carries `"rc": 0`. Each scenario's scan exit codes are `local`s inside its function and unrecoverable without touching all 23 bodies, so this plan defines `rc` as the scenario's machine verdict — 0 when no `^FAIL` line exists in its `$ASSERTLOG` slice, 1 otherwise. Task 5 documents this definition in `chaos/README.md`.

## Context every task needs

Verified against the tree at branch point (`main` = v1.21.0, commit 5fde55f):

- `chaos/run.sh` (2822 lines): flag parse at 19–30; portable-mode block 62–82 sets `CTX="$CONTEXT"` and `: "${OUT:=docs/testing/chaos-results-portable.md}"` at line 81; non-portable name derivation 109–124 sets `OUT` at 120 (kind) / 121 (k3s); `. "$ROOT/chaos/assert.sh"` at 129; `record()` at 846–860 (the never-fail report writer: `{ ... } | redact_nodes >> "$OUT"`); `# --- capabilities ---` section starts at 862; `redact_needles` 738–815 (python3 single-pass byte replace of `NODE_NAMES` → `<node-N>` and `CTX` → `<context>`); `redact_nodes` 817–844 (`[ -z "$NODE_NAMES" ]` → `cat` passthrough in kind mode; on failure prints `<redaction failed: section withheld>` and returns 0); `run_scenarios()` 2687–2696; `main()` 2698–2817 with `run_scenarios` called at 2782 and the assert_summary redaction detour at 2811–2816.
- `chaos/assert.sh` (159 lines): `assert_init` 21–27; `$ASSERTLOG` line format `PASS|FAIL<TAB><label><SPACE><detail>` (`printf '%s\t%s %s\n'`, line 32); `$SKIPLOG` line format `SKIP<TAB><title> — <reason>` (em dash, line 54); `scenario_title` 70–77 (`scenario_05_coredns` → `5. coredns`, `scenario_14` → `14.`); `assert_summary` 135–159.
- `chaos/assert-selftest.sh` (494 lines): `check <what> <actual> <want>` helper at 12–19; the sourcing-probe pattern — capture args into locals, `set --`, `. chaos/run.sh`, own mktemps, subshell-local trap — at `requires_probe` (166–185); `distro_probe` (291–297) shows sourcing WITH args for derivation tests; exit code is `[ "$fails" -eq 0 ]` at line 494. CI runs it at `.github/workflows/ci.yml:27`.
- `.github/workflows/chaos-matrix.yml` (234 lines): report-path step (`id: report`) 154–165; credential-grep step (`id: scan`, `if: always()`) 180–215; upload step 228–234 gated `if: ${{ !cancelled() && steps.scan.outcome != 'failure' }}`.
- `run_scenarios()`' scenario list (line 2692, `01_etcd` deliberately last):
  `02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 21_controlplane 22_dnshealth 23_pagerduty 01_etcd`
- The baseline healthy scan (main() lines 2740–2765) is NOT a scenario and writes NO corpus row.
- Scenario 9 makes two `record` calls (9 and 9b) but is ONE scenario function → one row (the ASSERTLOG delta covers both).
- Tasks 1–3 all add code to `chaos/run.sh` in one new `# --- corpus ---` section (inserted between `record()` ending at line 860 and the `# --- capabilities ---` comment at 862) and append checks to `chaos/assert-selftest.sh`. Task 1 creates the section; Tasks 2 and 3 APPEND to it. No later task may rewrite, rename, or reword an earlier task's code or checks.

---

### Task 1: `corpus_row()` — the JSON encoder

**Files:**
- Modify: `chaos/run.sh` (insert the new `# --- corpus ---` section after `record()`, i.e. between lines 860 and 862)
- Test: `chaos/assert-selftest.sh` (append a new probe + checks at the END of the file, before the final summary `printf` at line 492)

**Interfaces:**
- Produces: `corpus_row` — reads a redacted plaintext block on stdin (exactly 7 header lines: scenario title, fault slug, k8s version [may be empty], distro [may be empty], rc [0 or 1], skipped [`true`/`false`], skip reason [may be empty]; then zero or more verbatim assertion lines) and writes ONE compact JSON line to stdout with keys in spec order: `scenario`, `fault`, `k8s`, `distro`, `rc` (int), `assertions` (array of strings), `skipped` (bool), `skip_reason`. Exits non-zero on a malformed block (fewer than 7 lines, or a non-integer rc) — the caller treats that as "row withheld". Task 3's `capture()` consumes this.

- [ ] **Step 1: Append the failing selftest checks**

Insert the following block into `chaos/assert-selftest.sh` immediately BEFORE the final summary lines (the `printf '\n%s\n' "$([ "$fails" -eq 0 ] ...` at line 492):

```bash
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
```

- [ ] **Step 2: Run the selftest and watch it fail**

Run: `bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: the new checks print `NOT OK` (corpus_row is not defined, so the probe's command substitution comes back empty / the rc probes see exit 127 → they may pass by accident — the field-mapping checks are the ones that MUST be `NOT OK`), final line `assert-selftest: N check(s) failed`, `exit=1`. Report the actual failure lines you saw.

- [ ] **Step 3: Implement `corpus_row()` in `chaos/run.sh`**

Insert after `record()` (which ends at line 860 with `}`), before the `# --- capabilities ---` comment block:

```bash
# --- corpus ------------------------------------------------------------------
#
# Beside the human-facing report, every run writes a machine-readable corpus:
# one JSON line per scenario, promoted next to $OUT only when the run
# completes. The corpus is a data contract for training and evaluation OUTSIDE
# this repository; no Go code in kubeagent reads it. chaos/README.md ("Corpus")
# is the written form of the contract.

# corpus_row — one redacted plaintext block on stdin, one JSON line on stdout.
#
# The block's first seven lines are fixed fields (scenario title, fault slug,
# k8s version, distro, rc, skipped, skip reason — the middle two and the last
# may be empty); every remaining line is one verbatim assertion outcome.
# Redaction has ALREADY happened by the time bytes reach this function —
# encoding first could split a redaction needle across JSON escape sequences,
# which is why capture() pipes through redact_nodes before corpus_row, never
# after.
#
# A malformed block (fewer than seven lines, a non-integer rc) exits non-zero
# and the caller withholds the row: a corpus is allowed to lose a row, never
# to carry a guessed one.
corpus_row() {
  python3 -c '
import json, sys

lines = sys.stdin.read().split("\n")
if lines and lines[-1] == "":
    lines.pop()
if len(lines) < 7:
    sys.exit(1)
row = {
    "scenario": lines[0],
    "fault": lines[1],
    "k8s": lines[2],
    "distro": lines[3],
    "rc": int(lines[4]),
    "assertions": lines[7:],
    "skipped": lines[5] == "true",
    "skip_reason": lines[6],
}
row = {k: row[k] for k in ("scenario", "fault", "k8s", "distro", "rc",
                           "assertions", "skipped", "skip_reason")}
sys.stdout.write(json.dumps(row, separators=(",", ":")) + "\n")
'
}
```

(A non-integer `lines[4]` makes `int()` raise, python3 exits non-zero — that is the refusal path; no extra guard needed.)

- [ ] **Step 4: Syntax-check and run the selftest to verify it passes**

Run: `bash -n chaos/run.sh && bash -n chaos/assert-selftest.sh && bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: every check `ok`, final line `assert-selftest: all checks passed`, `exit=0`.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "feat(chaos): add corpus_row, the corpus's JSON encoder"
```

---

### Task 2: `scenario_fault()` — the fault vocabulary

**Files:**
- Modify: `chaos/run.sh` (append to the `# --- corpus ---` section Task 1 created, directly after `corpus_row()`'s closing `}`)
- Test: `chaos/assert-selftest.sh` (append checks after Task 1's block, before the final summary lines)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `scenario_fault <scenario name>` — maps a `run_scenarios` list entry (e.g. `05_coredns`, `14`) to a fixed lowercase-hyphen fault slug on stdout; unknown names print `unknown-scenario` and still return 0 (never-fail — the selftest's completeness check, not a runtime error, is what keeps the vocabulary closed). Task 3's `capture()` consumes this.

The slug names the INJECTED FAULT, not the feature under test. The mapping below was derived by reading each scenario's body (the kubectl/docker commands it actually runs); it is the required table:

| scenario | slug | fault actually injected |
|---|---|---|
| `01_etcd` | `control-plane-docker-stop` | `docker stop` of the control-plane node container |
| `02_certs` | `control-plane-cert-expiry` | none — unconditional skip; the slug names the fault that cannot be injected |
| `03_diskfull` | `node-cordon-diskfull` | cordons a worker (DiskPressure stand-in) + an unschedulable oversized request |
| `04_networkpolicy` | `networkpolicy-deny-all` | Calico-enforced deny-all NetworkPolicy |
| `05_coredns` | `coredns-corefile-broken` | **pinned by the spec** — invalid Corefile patch |
| `06_lb` | `loadbalancer-no-provider` | Service patched to LoadBalancer with no LB controller |
| `07_oom` | `memory-limit-oomkill` | stress workload against a 64Mi limit |
| `08_nsdelete` | `namespace-deletion` | namespace deleted under a running app |
| `09_rollout` | `deployment-bad-image-tag` | maxUnavailable:100% + nonexistent image tag |
| `10_credleak` | `configmap-aws-key-leak` | ConfigMap carrying an AWS-key-shaped literal |
| `11_kubelet` | `worker-containerd-stop` | `systemctl stop containerd` on a worker node |
| `12_watch` | `deployment-bad-image-tag` | same fault as 09, exercised against the watch daemon |
| `13_slo` | `deployment-bad-image-tag` | same fault as 09, exercised against SLO burn rate |
| `14` | `deployment-bad-image-tag` | same fault as 09, exercised against on-incident explanations |
| `15_multicluster` | `deployment-bad-image-tag` | same fault as 09, observed under two context aliases |
| `16_operators` | `certmanager-bad-issuer-ref` | Certificate with a nonexistent issuerRef |
| `17_gitops` | `flux-gitrepo-dns-failure` | GitRepository pointing at an unresolvable host |
| `18_capacity` | `oversized-job-unschedulable` | 40-core Job no node can hold |
| `19_mcp` | `crashloop-pod` | `exit 1` pod → CrashLoopBackOff (MCP is the feature, not the fault) |
| `20_rbac` | `crashloop-pod` | `exit 1` pod → CrashLoopBackOff (RBAC is the feature, not the fault) |
| `21_controlplane` | `no-fault-healthy-readyz` | none injected — scans the live healthy `/readyz` |
| `22_dnshealth` | `coredns-servfail-template` | valid Corefile that answers every query SERVFAIL |
| `23_pagerduty` | `deployment-bad-image-tag` | same fault as 09, exercised against the PagerDuty receiver |

- [ ] **Step 1: Append the failing selftest checks**

Insert after Task 1's corpus_row block in `chaos/assert-selftest.sh` (still before the final summary lines):

```bash
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
```

- [ ] **Step 2: Run the selftest and watch it fail**

Run: `bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: the new checks `NOT OK` (scenario_fault undefined), `exit=1`. Task 1's checks stay `ok`. Report what you saw.

- [ ] **Step 3: Implement `scenario_fault()` in `chaos/run.sh`**

Append inside the `# --- corpus ---` section, directly after `corpus_row()`'s closing `}`:

```bash
# scenario_fault <scenario name> — the fixed slug of the fault the named
# scenario INJECTS, for the corpus row. The slug names the fault, never the
# feature under test: scenarios 9, 12, 13, 14, 15 and 23 inject the literal
# same bad-image fault against six different features and share a slug — the
# row's scenario field is what tells them apart. A scenario that injects
# nothing says so explicitly (21), and scenario 2's slug names the fault that
# cannot be injected quickly or safely.
#
# An unknown name yields "unknown-scenario" and rc 0 — never-fail, because
# capture() must not be able to abort a forty-minute run. What keeps the
# vocabulary closed is chaos/assert-selftest.sh's completeness check, which
# extracts run_scenarios' list and fails CI on any entry without a real slug.
scenario_fault() {
  case "$1" in
    01_etcd)          echo control-plane-docker-stop ;;
    02_certs)         echo control-plane-cert-expiry ;;
    03_diskfull)      echo node-cordon-diskfull ;;
    04_networkpolicy) echo networkpolicy-deny-all ;;
    05_coredns)       echo coredns-corefile-broken ;;
    06_lb)            echo loadbalancer-no-provider ;;
    07_oom)           echo memory-limit-oomkill ;;
    08_nsdelete)      echo namespace-deletion ;;
    09_rollout)       echo deployment-bad-image-tag ;;
    10_credleak)      echo configmap-aws-key-leak ;;
    11_kubelet)       echo worker-containerd-stop ;;
    12_watch)         echo deployment-bad-image-tag ;;
    13_slo)           echo deployment-bad-image-tag ;;
    14)               echo deployment-bad-image-tag ;;
    15_multicluster)  echo deployment-bad-image-tag ;;
    16_operators)     echo certmanager-bad-issuer-ref ;;
    17_gitops)        echo flux-gitrepo-dns-failure ;;
    18_capacity)      echo oversized-job-unschedulable ;;
    19_mcp)           echo crashloop-pod ;;
    20_rbac)          echo crashloop-pod ;;
    21_controlplane)  echo no-fault-healthy-readyz ;;
    22_dnshealth)     echo coredns-servfail-template ;;
    23_pagerduty)     echo deployment-bad-image-tag ;;
    *)                echo unknown-scenario ;;
  esac
}
```

- [ ] **Step 4: Syntax-check and run the selftest to verify it passes**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: all checks `ok`, `exit=0`.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "feat(chaos): add scenario_fault, the corpus's closed fault vocabulary"
```

---

### Task 3: `capture()`, the scratch lifecycle, and the wiring

**Files:**
- Modify: `chaos/assert.sh` (assert_init, lines 21–27: `$CORPUSTMP` joins the mktemp set and the single trap line)
- Modify: `chaos/run.sh` — four places: (a) the corpus path derivation after the name-derivation block (insert after line 124's `fi`, before the `. "$ROOT/chaos/assert.sh"` block at 126–129); (b) `capture()` appended to the `# --- corpus ---` section after `scenario_fault()`; (c) `run_scenarios()` (2687–2696) gains the per-scenario bookkeeping and the `capture` call; (d) `main()` gains the promote step between `run_scenarios` (2782) and `log "done — report: $OUT"` (2784).
- Test: `chaos/assert-selftest.sh` (append after Task 2's block, before the final summary lines)

**Interfaces:**
- Consumes: `corpus_row` (Task 1), `scenario_fault` (Task 2), plus existing `scenario_title`, `redact_nodes`, `$ASSERTLOG`, `$SKIPLOG`.
- Produces: `capture <scenario name> <assertlog-lines-before> <skiplog-lines-before>` — appends one redacted JSON row to `$CORPUSTMP`; always returns 0. Globals `CORPUS_OUT` (final corpus path) and `CORPUS_DISTRO` (the distro to stamp into rows; empty in portable mode). Task 4's workflow edit relies on the exact `CORPUS_OUT` naming.

- [ ] **Step 1: Append the failing selftest checks**

Insert after Task 2's block in `chaos/assert-selftest.sh` (before the final summary lines):

```bash
# --- capture: one scenario, one redacted corpus row ---------------------------
# capture slices the scenario's fresh lines out of the two logs by line-count
# delta, redacts them, and hands them to corpus_row. Same guarded-source
# pattern as requires_probe; each probe owns its scratch files and its
# subshell-local trap.
capture_probe() {   # capture_probe <mode> -> the captured row, or "EMPTY"
  local mode="$1"
  (
    set --
    . chaos/run.sh
    ASSERTLOG="$(mktemp)"; SKIPLOG="$(mktemp)"; CORPUSTMP="$(mktemp)"
    trap 'rm -f "$ASSERTLOG" "$SKIPLOG" "$CORPUSTMP"' EXIT
    : > "$ASSERTLOG"; : > "$SKIPLOG"; : > "$CORPUSTMP"
    K8S_VERSION='v1.34'; CORPUS_DISTRO='kind'
    NODE_NAMES=''; CTX='kind-kubeagent-chaos'
    scenario='05_coredns'; abefore=0; sbefore=0
    case "$mode" in
      pass)
        # One stale line from an earlier scenario proves the delta slicing.
        printf 'PASS\tstale line from an earlier scenario (ok)\n' >> "$ASSERTLOG"
        abefore=1
        printf 'PASS\tCluster: Degraded named (found)\n' >> "$ASSERTLOG"
        printf 'PASS\tscan exit code (2)\n' >> "$ASSERTLOG"
        ;;
      fail)
        printf 'PASS\tscan exit code (2)\n' >> "$ASSERTLOG"
        printf 'FAIL\tCluster: Degraded named (missing)\n' >> "$ASSERTLOG"
        ;;
      skip)
        scenario='02_certs'
        printf 'SKIP\t2. certs — control-plane certificate expiry cannot be forced quickly or safely\n' >> "$SKIPLOG"
        ;;
      redact)
        NODE_NAMES='worker-9.internal.example'
        CTX='top-secret-ctx'
        printf 'PASS\tnode worker-9.internal.example seen from top-secret-ctx (ok)\n' >> "$ASSERTLOG"
        ;;
      rowfail)
        printf 'PASS\tsomething (ok)\n' >> "$ASSERTLOG"
        corpus_row() { return 1; }
        ;;
    esac
    capture "$scenario" "$abefore" "$sbefore" 2>/dev/null
    if [ -s "$CORPUSTMP" ]; then cat "$CORPUSTMP"; else echo EMPTY; fi
  )
}

check 'capture slices only the scenario delta and stamps the axes' \
  "$(capture_probe pass | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print("|".join([r["scenario"], r["fault"], r["k8s"], r["distro"], str(r["rc"]),
                str(len(r["assertions"])), r["assertions"][0],
                str(r["skipped"]).lower(), r["skip_reason"]]))
')" '5. coredns|coredns-corefile-broken|v1.34|kind|0|2|PASS	Cluster: Degraded named (found)|false|'
check 'capture sets rc 1 when the slice carries a FAIL line' \
  "$(capture_probe fail | python3 -c '
import json, sys; print(json.loads(sys.stdin.read())["rc"])')" 1
check 'capture flags a skipped scenario with its reason and no assertions' \
  "$(capture_probe skip | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
print("|".join([r["scenario"], str(r["skipped"]).lower(),
                str(len(r["assertions"])), str(r["rc"]), r["skip_reason"]]))
')" '2. certs|true|0|0|control-plane certificate expiry cannot be forced quickly or safely'
check 'capture redacts node and context names before encoding' \
  "$(capture_probe redact | python3 -c '
import json, sys
r = json.loads(sys.stdin.read())
a = r["assertions"][0]
print("|".join(["node-raw" if "worker-9.internal.example" in a else "node-gone",
                "ctx-raw" if "top-secret-ctx" in a else "ctx-gone",
                "yes" if "<node-1>" in a and "<context>" in a else "no"]))
')" 'node-gone|ctx-gone|yes'
check 'a corpus_row failure costs the row, never the run' \
  "$(capture_probe rowfail)" EMPTY

# --- assert_init owns the corpus scratch on its single trap line --------------
assert_init
check 'assert_init creates the corpus scratch' "$([ -f "$CORPUSTMP" ] && echo yes)" yes
check 'assert.sh still holds exactly one EXIT trap' \
  "$(grep -c "trap 'rm -f" chaos/assert.sh)" 1

# --- the corpus path derives beside the report, per axis ----------------------
corpus_path_probe() {   # corpus_path_probe <run.sh args...> -> "<CORPUS_OUT>|<CORPUS_DISTRO>"
  local args=("$@")
  (
    . chaos/run.sh "${args[@]}"
    printf '%s|%s\n' "$CORPUS_OUT" "$CORPUS_DISTRO"
  ) 2>/dev/null || printf 'rc=%s|\n' "$?"
}
check 'default kind corpus path' \
  "$(corpus_path_probe)" 'docs/testing/chaos-corpus-kind.jsonl|kind'
check 'versioned kind corpus path is minor-then-distro' \
  "$(corpus_path_probe --k8s-version v1.34)" 'docs/testing/chaos-corpus-v1.34-kind.jsonl|kind'
check 'k3s corpus path' \
  "$(corpus_path_probe --distro k3s)" 'docs/testing/chaos-corpus-k3s.jsonl|k3s'
check 'versioned k3s corpus path is minor-then-distro' \
  "$(corpus_path_probe --distro k3s --k8s-version v1.34)" 'docs/testing/chaos-corpus-v1.34-k3s.jsonl|k3s'
check 'portable corpus path claims neither a distro nor a version' \
  "$(corpus_path_probe --context some-ctx)" 'docs/testing/chaos-corpus-portable.jsonl|'
check 'the corpus lands beside a user-chosen --out report' \
  "$(corpus_path_probe --out /tmp/elsewhere/report.md)" '/tmp/elsewhere/chaos-corpus-kind.jsonl|kind'
```

Note the `pass`-mode expected value embeds a REAL TAB between `PASS` and `Cluster:` (inside `'...PASS	Cluster...'`) — type an actual tab character there, not `\t`.

- [ ] **Step 2: Run the selftest and watch it fail**

Run: `bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: the capture and corpus-path checks `NOT OK` (capture and CORPUS_OUT undefined; `[ -f "$CORPUSTMP" ]` fails), `exit=1`. Tasks 1–2 checks stay `ok`. Report what you saw.

- [ ] **Step 3: Implement — four edits in `chaos/run.sh`, one in `chaos/assert.sh`**

**(3a) `chaos/assert.sh` — `assert_init` (replace lines 21–27):**

```bash
assert_init() {
  ASSERTLOG="${ASSERTLOG:-$(mktemp)}"
  SKIPLOG="${SKIPLOG:-$(mktemp)}"
  # The corpus's scratch shares this lifecycle deliberately: same mktemp, same
  # single trap line. A second `trap ... EXIT` anywhere would clobber this one.
  CORPUSTMP="${CORPUSTMP:-$(mktemp)}"
  : > "$ASSERTLOG"
  : > "$SKIPLOG"
  : > "$CORPUSTMP"
  trap 'rm -f "${ASSERTLOG:-}" "${SKIPLOG:-}" "${CORPUSTMP:-}"' EXIT
}
```

**(3b) `chaos/run.sh` — corpus path derivation.** Insert immediately after the name-derivation block's closing `fi` (line 124, after `COREDNS_BACKUP=...`), before the comment block at 126 that precedes `. "$ROOT/chaos/assert.sh"`:

```bash
# The corpus lands beside the report — same directory, fixed name per axis,
# minor-then-distro per the spec (chaos-corpus-v1.34-kind.jsonl), which on the
# k3s path deliberately differs from the report's distro-first name. Portable
# mode gets its own name and claims neither a distro nor a version: the
# harness will not stamp facts it cannot know about a cluster it did not
# create into a corpus row.
if [ "$PORTABLE" = 1 ]; then
  CORPUS_OUT="$(dirname "$OUT")/chaos-corpus-portable.jsonl"
  CORPUS_DISTRO=""
else
  CORPUS_OUT="$(dirname "$OUT")/chaos-corpus${K8S_VERSION:+-$K8S_VERSION}-$DISTRO.jsonl"
  CORPUS_DISTRO="$DISTRO"
fi
```

**(3c) `chaos/run.sh` — `capture()`.** Append inside the `# --- corpus ---` section, after `scenario_fault()`'s closing `}`:

```bash
# capture <scenario name> <assertlog lines before> <skiplog lines before> —
# append one corpus row for the scenario that just returned.
#
# The scenario's slice of $ASSERTLOG is everything after line <before>: the
# log's lines carry no scenario prefix, so the delta IS the attribution. rc is
# the scenario's machine verdict — 0 when no assertion in its slice failed,
# 1 otherwise — NOT a process exit code: each scenario holds its scan exit
# codes in locals and asserts on them there, so the assertion outcomes are the
# only per-scenario verdict that exists outside the scenario's body.
#
# The plaintext block is redacted BEFORE it is JSON-encoded: encoding first
# could split a redaction needle's bytes across escape sequences. $ASSERTLOG
# is NOT pre-redacted (main()'s assert_summary detour redacts it into $OUT for
# the same reason), so this pipeline is where the corpus's redaction promise
# is kept — one seam, shared with the report.
#
# Same never-fail contract as record(): a corpus problem costs the row, with a
# stderr note, never the run. Every step is guarded, the function returns 0,
# and run_scenarios tests the call, which suppresses set -e for the body.
capture() {
  local s="$1" abefore="$2" sbefore="$3"
  local title fault alines rc=0 skipped=false reason="" safter
  title="$(scenario_title "scenario_$s")" || title="$s"
  fault="$(scenario_fault "$s")" || fault="unknown-scenario"
  alines="$(tail -n "+$((abefore + 1))" "$ASSERTLOG" 2>/dev/null || true)"
  if printf '%s\n' "$alines" | grep -q '^FAIL'; then rc=1; fi
  safter="$(wc -l < "$SKIPLOG" | tr -d ' ' || echo 0)"
  if [ "$safter" -gt "$sbefore" ] 2>/dev/null; then
    skipped=true
    # SKIPLOG lines read "SKIP\t<title> — <reason>"; strip through the first
    # " — " (assert_skip's em dash) to keep the reason alone.
    reason="$(tail -n 1 "$SKIPLOG" 2>/dev/null || true)"
    reason="${reason#* — }"
  fi
  {
    printf '%s\n' "$title" "$fault" "$K8S_VERSION" "$CORPUS_DISTRO" "$rc" "$skipped" "$reason"
    if [ -n "$alines" ]; then printf '%s\n' "$alines"; fi
  } | redact_nodes | corpus_row >> "$CORPUSTMP" || {
    printf 'chaos/run.sh: corpus capture failed for scenario %s; row withheld.\n' "$s" >&2
  }
  return 0
}
```

**(3d) `chaos/run.sh` — `run_scenarios()`.** Replace the function body (lines 2687–2696) with (the leading comment about 01_etcd stays verbatim):

```bash
run_scenarios() {
  # 01_etcd runs LAST: stopping the control-plane is the most disruptive fault and
  # etcd/apiserver flap for a while afterwards (and while the API is down even
  # `kubectl wait` can't settle it). Running it last keeps that recovery noise from
  # contaminating the other scenarios' scans.
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 21_controlplane 22_dnshealth 23_pagerduty 01_etcd)
  local abefore sbefore
  for s in "${all[@]}"; do
    if [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ]; then
      # Corpus bookkeeping: where this scenario's slice of the two logs
      # begins. This loop runs in the main shell — a pipeline would lose
      # these variables — and both files exist (assert_init made them).
      abefore="$(wc -l < "$ASSERTLOG" | tr -d ' ')"
      sbefore="$(wc -l < "$SKIPLOG" | tr -d ' ')"
      "scenario_$s"
      # `|| true` makes the never-fail contract structural: a tested call
      # suppresses set -e for capture's whole body, so no corpus problem can
      # abort a forty-minute run.
      capture "$s" "$abefore" "$sbefore" || true
    fi
  done
}
```

**(3e) `chaos/run.sh` — the promote step in `main()`.** Between `run_scenarios` (line 2782) and `log "done — report: $OUT"` (line 2784), insert:

```bash
  # Promote the corpus only now, when every selected scenario has run: an
  # aborted run leaves NO corpus file rather than a truncated one that could
  # be mistaken for complete. Rows were redacted at capture time, so this copy
  # moves no unredacted byte. Never the gate: a failed promote costs the file,
  # with a stderr note, and the exit code still comes from assert_summary.
  if cp "$CORPUSTMP" "$CORPUS_OUT" 2>/dev/null; then
    log "corpus: $CORPUS_OUT ($(wc -l < "$CORPUS_OUT" | tr -d ' ') rows)"
  else
    printf 'chaos/run.sh: corpus promote failed; no corpus written.\n' >&2
  fi
```

- [ ] **Step 4: Syntax-check and run the selftest to verify it passes**

Run: `bash -n chaos/run.sh && bash -n chaos/assert.sh && bash -n chaos/assert-selftest.sh && bash chaos/assert-selftest.sh; echo "exit=$?"`
Expected: all checks `ok` (including every pre-existing one — the assert_init change must not disturb them), `exit=0`.
Also run: `bash chaos/version-selftest.sh` — untouched by this task and must stay green.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh chaos/assert.sh chaos/assert-selftest.sh
git commit -s -m "feat(chaos): capture one redacted corpus row per scenario, promoted on completion"
```

---

### Task 4: the nightly matrix uploads a credential-scanned corpus

**Files:**
- Modify: `.github/workflows/chaos-matrix.yml` (three steps: `Resolve the report path` at 154–165, `Scan the report for credential material` at 180–215, `Upload the report` at 228–234)

**Interfaces:**
- Consumes: Task 3's `CORPUS_OUT` naming — `docs/testing/chaos-corpus-<version>-<distro>.jsonl` (the matrix always passes `--k8s-version`, so the unversioned name never appears in CI).
- Produces: nothing later tasks rely on.

- [ ] **Step 1: Extend the report-path step**

Replace the `run:` block of the `Resolve the report path` step (its comment and `id: report` stay) with:

```yaml
        run: |
          set -euo pipefail
          if [ "$DISTRO" = k3s ]; then
            printf 'path=docs/testing/chaos-results-k3s-%s.md\n' "$VERSION" >> "$GITHUB_OUTPUT"
          else
            printf 'path=docs/testing/chaos-results-%s.md\n' "$VERSION" >> "$GITHUB_OUTPUT"
          fi
          # The corpus is named minor-then-distro on BOTH paths (the spec's
          # order), unlike the k3s report above — chaos/README.md "Corpus"
          # documents the asymmetry.
          printf 'corpus=docs/testing/chaos-corpus-%s-%s.jsonl\n' "$VERSION" "$DISTRO" >> "$GITHUB_OUTPUT"
```

- [ ] **Step 2: Extend the credential grep to cover both files**

Replace the `run:` block of the `Scan the report for credential material` step (`id: scan`, `if: always()`, and the comment above it stay, except: in that comment, change the phrase "because a report is worth publishing only if publishing it is safe" to "because an artifact is worth publishing only if publishing it is safe — and the corpus travels beside the report") with:

```yaml
        run: |
          set -uo pipefail
          rep='${{ steps.report.outputs.path }}'
          corpus='${{ steps.report.outputs.corpus }}'
          files=()
          for f in "$rep" "$corpus"; do
            if [ -f "$f" ]; then files+=("$f"); else echo "no file at $f — skipping it"; fi
          done
          if [ "${#files[@]}" -eq 0 ]; then echo "nothing to scan"; exit 0; fi
          hits=0
          scan() {   # scan <what> <extended-regexp>
            local n
            # `--` stops option parsing: the PEM pattern below begins with a
            # literal `-`, which grep would otherwise read as an option string
            # and refuse to run at all. cat feeds every artifact through one
            # count — JSON escaping never splits these patterns' bytes, and
            # redaction ran before encoding, so the same regexes hold for the
            # corpus.
            n="$(cat "${files[@]}" | grep -acE -- "$2" || true)"
            printf '%-34s %s\n' "$1" "$n"
            [ "$n" = 0 ] || hits=$((hits + 1))
          }
          # A JWT's three dot-separated base64url segments: what a mounted
          # ServiceAccount token looks like on the wire.
          scan 'bearer-token shapes'   'eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.'
          scan 'PEM blocks'            '-----BEGIN [A-Z ]*(PRIVATE KEY|CERTIFICATE)-----'
          scan 'Authorization headers' '[Aa]uthorization: *[Bb]earer'
          scan 'kubeconfig fields'     'client-key-data|client-certificate-data|token: '
          # The documentation value scenario 10 injects on purpose is the only
          # AKIA that may appear; any other one is a real finding. Two steps,
          # not one negative lookahead: POSIX ERE has no lookahead, and
          # `grep -E 'AKIA(?!...)'` does not fail loudly — it warns, matches
          # nothing, and would report a genuinely leaked key as 0.
          n="$(cat "${files[@]}" | grep -aoE 'AKIA[A-Z0-9]{16}' | grep -vxF 'AKIAIOSFODNN7EXAMPLE' | grep -ac . || true)"
          printf '%-34s %s\n' 'unexpected AWS keys' "$n"
          [ "$n" = 0 ] || hits=$((hits + 1))
          if [ "$hits" -ne 0 ]; then
            echo "refusing to upload: the artifacts matched $hits credential pattern(s)" >&2
            exit 1
          fi
          echo "artifacts are clean — safe to upload"
```

- [ ] **Step 3: Extend the upload step**

Replace the `Upload the report` step's name and `with:` block (the long comment above it and the `if:` gate stay exactly as they are) with:

```yaml
      - name: Upload the report and corpus
        if: ${{ !cancelled() && steps.scan.outcome != 'failure' }}
        uses: actions/upload-artifact@v4
        with:
          name: chaos-report-${{ matrix.distro }}-${{ matrix.version }}
          path: |
            ${{ steps.report.outputs.path }}
            ${{ steps.report.outputs.corpus }}
          if-no-files-found: warn
```

(The artifact `name` deliberately does not change — consumers of the nightly artifact keep their download path; the corpus appears inside it.)

- [ ] **Step 4: Verify the YAML parses and the diff is bounded**

Run: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/chaos-matrix.yml")); print("YAML-OK")'`
Expected: `YAML-OK`.
Run: `git diff --stat` — exactly one file, `.github/workflows/chaos-matrix.yml`.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/chaos-matrix.yml
git commit -s -m "ci(chaos): credential-scan and upload the corpus beside the nightly report"
```

---

### Task 5: documentation

**Files:**
- Modify: `chaos/README.md` (new `## Corpus` section inserted immediately BEFORE the `## Scenarios` heading at line 377)
- Modify: `CHANGELOG.md` (under `## [Unreleased]` at line 8, currently empty)
- Modify: `website/docs/roadmap.md` (the post-1.0 row, line 572)
- Modify: `CLAUDE.md` (the hypothesis-engine bullet's closing sentences, lines 579–581)

**Interfaces:**
- Consumes: the semantics Tasks 1–4 implemented (rc definition, redact-before-encode, promote-on-completion, naming, the fault vocabulary, the shared-slug rule, portable-mode empty axes).
- Produces: nothing later tasks rely on.

- [ ] **Step 1: Add the `## Corpus` section to `chaos/README.md`**

Insert immediately before the `## Scenarios` heading (line 377):

````markdown
## Corpus

Every run also writes a machine-readable twin of the report: one JSON line per
scenario, in `chaos-corpus-<minor>-<distro>.jsonl` beside the report —
`chaos-corpus-v1.34-kind.jsonl`; `chaos-corpus-kind.jsonl` when no
`--k8s-version` was given; `chaos-corpus-portable.jsonl` in portable mode.
Note the axis order: the corpus is named minor-then-distro even on the k3s
path, where the report is named `chaos-results-k3s-<minor>.md`.

A row:

```json
{"scenario": "5. coredns", "fault": "coredns-corefile-broken", "k8s": "v1.34", "distro": "kind", "rc": 0, "assertions": ["PASS\tCluster: Degraded named (found '5/5 pods affected')"], "skipped": false, "skip_reason": ""}
```

Field semantics, which are a contract:

- `scenario` — the report heading, from `scenario_title`, so the two artifacts
  can never disagree about a scenario's name.
- `fault` — a fixed slug naming the fault the scenario INJECTS, never the
  feature it tests: scenarios 9, 12, 13, 14, 15 and 23 inject the literal same
  bad-image fault against six different features and share
  `deployment-bad-image-tag` — the `scenario` field is what tells their rows
  apart. A scenario that injects nothing says so (`no-fault-healthy-readyz`),
  and scenario 2's slug names the fault that cannot be injected quickly or
  safely (`control-plane-cert-expiry`). The vocabulary is closed: a selftest
  extracts `run_scenarios`' list and fails CI on any scenario without a slug.
- `k8s`, `distro` — the run's axes. Both empty in portable mode: the harness
  will not stamp facts it cannot know about a cluster it did not create.
- `rc` — the scenario's machine verdict: `0` when no assertion in the
  scenario's slice of the log failed, `1` otherwise. It is NOT a process exit
  code — each scenario holds its scan exit codes in locals and asserts on
  them there, so the assertion outcomes are the only per-scenario verdict
  that exists outside the scenario's body.
- `assertions` — the scenario's `PASS`/`FAIL` lines, verbatim, tab included.
  The baseline healthy scan is not a scenario and writes no row.
- `skipped`, `skip_reason` — a scenario that did not run says so and why, so
  a partial run can never masquerade as a full corpus. A scenario that ran
  some assertions before a mid-body capability guard carries both its
  assertions and `skipped: true`.

Three properties hold by construction:

- **Redacted at capture.** Each row's text passes `redact_nodes` BEFORE it is
  JSON-encoded — encoding first could split a redaction needle's bytes across
  escape sequences. The assertion log itself is not pre-redacted (the
  report's summary takes the same detour), so capture is where the corpus's
  redaction promise is kept: one seam, shared with the report, including the
  withhold-on-failure behavior.
- **Complete or absent.** Rows accumulate in a scratch file and are promoted
  beside the report only after the last scenario returns: an aborted run
  leaves no corpus at all rather than a truncated one that could be mistaken
  for complete. An `--only NN` run writes a one-row corpus, which is
  self-evidently partial.
- **Never the gate.** A corpus problem costs the row (with a stderr note),
  never the run — the same contract as `record()` — and the exit code still
  comes from the assertion summary alone.

The nightly matrix uploads the corpus inside the same artifact as the report,
after the same credential grep clears both files. The corpus is a data
contract for training and evaluation OUTSIDE this repository; no Go code in
kubeagent reads it.
````

- [ ] **Step 2: Add the CHANGELOG entry**

Under `## [Unreleased]` (line 8), add:

```markdown

### Added

- Chaos correctness corpus, the hypothesis engine's final slice: every
  `./chaos/run.sh` run now writes `chaos-corpus-<minor>-<distro>.jsonl` beside
  its report — one JSON row per scenario naming the injected fault (a fixed
  23-entry vocabulary kept closed by a CI selftest), the scenario's verbatim
  assertion lines, and whether it was skipped and why, so a partial run can
  never masquerade as a full corpus. Rows pass the harness's one redaction
  seam before they are JSON-encoded, the file appears only when the run
  completes, and the nightly chaos matrix credential-scans and uploads it
  beside the report. No production Go code: the corpus is a training contract
  for consumers outside this repository, and nothing in kubeagent reads it.
```

- [ ] **Step 3: Close the roadmap item**

In `website/docs/roadmap.md` line 572, make exactly two edits:

(a) Replace `**hypothesis engine slices 1–2 shipped** (`
with `**hypothesis engine complete** (`

(b) Replace the fragment
`and adds a `get_log_causes` tool that classifies a bounded previous-instance log tail into an address-redacted cause — never a raw log line)`
with
`and adds a `get_log_causes` tool that classifies a bounded previous-instance log tail into an address-redacted cause — never a raw log line; slice 3 closes the engine with the chaos correctness corpus: every chaos run writes one redacted JSON row per scenario — injected-fault slug from a closed vocabulary, verbatim assertion lines, skip and reason — beside its report, uploaded nightly after the same credential grep, a training contract no Go code in kubeagent reads)`

(c) Replace the row's tail
`— plus other baseline dimensions, loading a pack into an installed binary without a kubeagent release, and the hypothesis engine's remaining slice, the chaos correctness corpus, still ahead |`
with
`— plus other baseline dimensions and loading a pack into an installed binary without a kubeagent release, still ahead |`

- [ ] **Step 4: Update CLAUDE.md's hypothesis-engine bullet**

Replace (lines 579–581):

```text
  JSON schema moves. Only slice 3, the chaos correctness corpus, remains.
  The remaining post-1.0 work is other baseline dimensions and the
  hypothesis engine's last slice, the chaos correctness corpus.
```

with:

```text
  JSON schema moves.
  Slice 3 has since shipped, and **the hypothesis engine is complete**: every
  chaos run writes a correctness corpus beside its report —
  `chaos-corpus-<minor>-<distro>.jsonl`, one JSON row per scenario carrying
  the injected fault's slug from a closed 23-entry vocabulary (a CI selftest
  extracts `run_scenarios`' list and fails on any scenario without a slug),
  the scenario's verbatim assertion lines, and whether it was skipped and
  why. Rows pass `redact_nodes` BEFORE they are JSON-encoded (the assertion
  log is not pre-redacted), the file is promoted only when the run completes
  so an aborted run leaves nothing rather than a truncated corpus, a corpus
  problem costs the row and never the run (`record()`'s contract), and the
  nightly matrix credential-scans and uploads it beside the report. A row's
  `rc` is the scenario's machine verdict — 0 when no assertion in its slice
  failed — not a process exit code. No production Go code: the corpus is a
  training contract for consumers outside this repository, and nothing in
  kubeagent reads it.
  The remaining post-1.0 work is other baseline dimensions.
```

Do NOT add a `(vX.Y.Z)` parenthetical — the later `release:` commit adds it, never a docs commit.

- [ ] **Step 5: Build the website strictly and verify the diff**

Run:
```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml; cd /home/ubuntu/git/kubeagent
```
Expected: exit 0, no `WARNING` lines about pages. (The red "Material for MkDocs 2.0" banner is cosmetic.)
Run: `git diff --stat` — exactly four files: `chaos/README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md`.
Run: `bash chaos/assert-selftest.sh` — still green (docs only, but cheap and binding).

- [ ] **Step 6: Commit**

```bash
git add chaos/README.md CHANGELOG.md website/docs/roadmap.md CLAUDE.md
git commit -s -m "docs(chaos): write the corpus contract (fields, redaction, promote-on-completion) and close the hypothesis engine"
```
