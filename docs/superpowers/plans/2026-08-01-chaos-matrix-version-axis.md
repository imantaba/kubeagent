# Chaos matrix — the version axis (slice 7b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `chaos/run.sh` a `--k8s-version` flag that runs the whole 20-scenario suite against any supported Kubernetes minor, on a cluster, context, report and scratch path derived from that minor so two versions can never collide — and prove the suite green on two minors that are not the default.

**Architecture:** A digest-pinned data file (`chaos/versions.env`) names the supported minors and their `kindest/node` images. A small sourceable library (`chaos/versions.sh`) is the only code that reads it, exposing three functions the harness and a cluster-free self-test both call. `chaos/run.sh` gains one flag; everything cluster-shaped is then derived from the selected minor. Omitting the flag changes nothing at all.

**Tech Stack:** bash (`set -euo pipefail`), kind v0.30.0, kubectl, docker. No Go code. No new dependency.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every commit needs a `Signed-off-by` trailer matching its author** (`git commit -s`) — `main` enforces DCO. Verify with `scripts/dco-check.sh main HEAD`. Do not add a second `Signed-off-by` by hand. A `git merge -m` commit does not get one automatically.
- **No `Co-Authored-By` trailer and no AI / Claude / Claude Code / Anthropic attribution anywhere** — commit messages, code, comments, documentation prose, changelog.
- **NO NEW DEPENDENCY, and NO Go production code.** `go.mod`, `go.sum` and everything under `internal/` and `*.go` must be byte-identical to `main` at the end of this slice. Verify with `git diff --stat main..HEAD -- go.mod go.sum internal/ '*.go'` — it must print nothing.
- **If a version run reveals a kubeagent defect, that is a separate commit with its own unit test, and it must be reported rather than folded into a harness change that papers over it.** A version-specific expected-value table is explicitly forbidden (see below) — so is quietly loosening an assertion until it passes.
- **No per-version expected-value table.** An assertion that cannot be phrased to hold on all three minors is asserting the wrong thing, and the fix is the assertion, not a lookup table. A genuine absent-API skip is a rare escape hatch and each use must carry a comment naming the API and the minor that lacks it.
- **Assertions stay at kubeagent's contract level, never Kubernetes'.** Assert what kubeagent did with an API message — a finding is present, a counter, an exit code — never the API server's or the kubelet's wording. An assertion that pins upstream text goes red on a change that broke nothing and trains everyone to ignore a red nightly.
- **Omitting `--k8s-version` keeps today's behaviour exactly:** cluster `kubeagent-chaos`, context `kind-kubeagent-chaos`, report `docs/testing/chaos-results.md`, and **no `--image` flag passed to `kind create cluster`** so kind's own embedded default image is used. An operator's muscle memory and the release skill's documented command must keep working unchanged.
- **`scenario_01_etcd` stays LAST** in `run_scenarios()`'s hardcoded array — it stops the control plane and the API server flaps afterwards.
- **`ready_replicas()`'s `"?"` is a FAIL, never coerced to 0.**
- **No secrets, credentials, private IPs or internal hostnames anywhere** — including the version file, error text, report content and every doc example. Documentation IPs must be RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`, `example.org`, `example.net`). A fixture *named* like a real credential is a defect even when its value is fake.
- **Never expose API keys to the shell.** Every harness run in this slice is preceded by `unset ANTHROPIC_API_KEY`; the `--explain` scenarios then skip by design. Never instruct anyone to export a key.
- **Kubeconfig and filesystem paths are credentials** in forwarded artifacts. The report is a forwarded artifact. A repo-relative path like `chaos/versions.env` in an error message is fine; an absolute path from the runner's filesystem is not.
- **`go test` runs with `-p 2`, never `-short`.** CI's `go test -race ./...` must stay green — it should be untouched by this slice.
- **`docs/testing/` is gitignored**, so every generated report (versioned or not) is untracked. **`docs/superpowers/` is NOT gitignored** — this plan is committed.
- **Never `git add -A` or `git add .`** — stage by name.
- Branch is `chaos-matrix`, already checked out, already carrying slice 7a. Never implement on `main`.

---

## Prior state — what slice 7a left behind

Read this before Task 1; it is the ground you are building on.

`chaos/run.sh` (~1560 lines, `set -euo pipefail`, run from the repo root) creates a disposable Kind cluster, applies Calico as the CNI, waits for the system workloads to settle, records a baseline scan, then runs 20 outage scenarios and writes a Markdown section per scenario into `$OUT`.

Slice 7a made it a **gate**. `chaos/assert.sh` provides `expect_eq`, `expect_ge`, `expect_contains`, `expect_absent`, `assert_init` and `assert_summary`. Each scenario is written as:

```sh
{
  ...evidence printed here...
  echo '--- assertions ---'
  expect_eq "some label" "$actual" 0
} | record "<title>" "<prose verdict>"
```

Because `record` is fed by a **pipeline**, and a pipeline runs in a **subshell**, the failure counter is a **file** (`$ASSERTLOG`), not a shell variable. `assert_summary "$OUT"` is `main`'s last statement and `main` is the script's last line, so the script's exit status is the gate's. A full run currently reports `assertions: 103 run, 0 failed`.

Facts you will need:

| | |
| --- | --- |
| cluster name | `CLUSTER=kubeagent-chaos` at `chaos/run.sh:8` |
| context | `CTX=kind-$CLUSTER` at `chaos/run.sh:9` |
| CoreDNS scratch file | `COREDNS_BACKUP=/tmp/kubeagent-chaos-coredns.yaml` at `chaos/run.sh:10` |
| flag parsing | the `while [ $# -gt 0 ]` loop at `chaos/run.sh:15-23` |
| report default | `: "${OUT:=docs/testing/chaos-results.md}"` at `chaos/run.sh:30` |
| assert library sourced | `. "$ROOT/chaos/assert.sh"` at `chaos/run.sh:35` |
| cluster creation | `create_cluster()` at `chaos/run.sh:48-55`, runs `kind create cluster --name "$CLUSTER" --config chaos/kind-config.yaml --wait 120s` |
| node topology | `chaos/kind-config.yaml`: 1 control-plane + 2 workers, `disableDefaultCNI: true`, `podSubnet: 192.168.0.0/16` |
| installed kind | v0.30.0, whose embedded default node image is `kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a` |

A full `./chaos/run.sh --recreate` takes **35-40 minutes**. A targeted `./chaos/run.sh --only NN` against an already-up cluster takes **3-6 minutes** (it still runs preflight, the build, the idempotent Calico apply, the settle wait and the baseline before the selected scenario).

---

## File structure

| File | Status | Responsibility |
| --- | --- | --- |
| `chaos/versions.env` | create | Data only. The supported minors and their digest-pinned node images. No logic. |
| `chaos/versions.sh` | create | The only code that reads `versions.env`. Three functions: `chaos_versions`, `chaos_image`, `chaos_suffix`. Sourceable, side-effect-free beyond loading the data file. |
| `chaos/version-selftest.sh` | create | Cluster-free, sub-second proof that each function answers correctly and rejects correctly. Mirrors `chaos/assert-selftest.sh`. |
| `chaos/run.sh` | modify | One new flag; derive cluster, context, report path and scratch path from it; pass `--image` when set. |
| `chaos/README.md` | modify | Document the axis and how to add a minor. |
| `.claude/skills/release/SKILL.md` | modify | Step 3 mentions the axis exists and that the release gate is still the default single-version run. |

---

### Task 1: The version data file and its resolver

**Files:**
- Create: `chaos/versions.env`
- Create: `chaos/versions.sh`
- Create: `chaos/version-selftest.sh`
- Test: `chaos/version-selftest.sh` is itself the test

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, all defined in `chaos/versions.sh` and used by Task 2:
  - `chaos_versions` — prints the supported minors, space-separated, on one line. No arguments. Always exits 0.
  - `chaos_image <minor>` — prints the digest-pinned image reference for `<minor>` on stdout and exits 0. On an unknown or malformed minor, prints nothing to stdout, prints a message naming the supported set to **stderr**, and exits 1.
  - `chaos_suffix <minor>` — prints the cluster-name suffix for `<minor>`: `v1.33` becomes `-v1-33`. Exits 1 the same way on a malformed minor.

**Why a separate file rather than more lines in `run.sh`:** `run.sh` does work at load time (it parses flags, sources `assert.sh`, `cd`s to the repo root). Nothing in it can be exercised without a cluster. Putting the resolution in a sourceable library is what makes a cluster-free self-test possible at all, and it mirrors the `assert.sh` / `assert-selftest.sh` pair slice 7a already established.

- [ ] **Step 1: Write the failing test**

Create `chaos/version-selftest.sh`. It must be executable, scenario-free, cluster-free and sub-second. Model its shape on `chaos/assert-selftest.sh` — read that file first and match its style.

```sh
#!/usr/bin/env bash
# Cluster-free self-test for chaos/versions.sh. Proves each function answers
# correctly for a supported minor AND rejects an unsupported one, because a
# resolver that silently returns empty would hand `kind create cluster` an empty
# --image and boot the wrong Kubernetes.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=chaos/versions.sh
. "$ROOT/chaos/versions.sh"

fails=0
check() {   # check <label> <actual> <want>
  if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"
  else printf 'FAIL %s (got %s, want %s)\n' "$1" "$2" "$3"; fails=$((fails + 1)); fi
}

# --- chaos_versions -------------------------------------------------------
v="$(chaos_versions)"
check "chaos_versions lists v1.32" "$(printf '%s' "$v" | grep -c 'v1\.32' || true)" 1
check "chaos_versions lists v1.33" "$(printf '%s' "$v" | grep -c 'v1\.33' || true)" 1
check "chaos_versions lists v1.34" "$(printf '%s' "$v" | grep -c 'v1\.34' || true)" 1

# --- chaos_image ----------------------------------------------------------
# Every supported minor must resolve to a digest-pinned reference. A bare tag
# would let a silently retagged upstream image turn a green nightly red with no
# kubeagent change, which is the whole reason this file exists.
for m in $(chaos_versions); do
  img="$(chaos_image "$m")" && rc=0 || rc=$?
  check "chaos_image $m exits 0"          "$rc" 0
  check "chaos_image $m names kindest"    "$(printf '%s' "$img" | grep -c '^kindest/node:' || true)" 1
  check "chaos_image $m is digest-pinned" "$(printf '%s' "$img" | grep -cE '@sha256:[0-9a-f]{64}$' || true)" 1
  check "chaos_image $m names the minor"  "$(printf '%s' "$img" | grep -c "kindest/node:${m}\." || true)" 1
done

# --- chaos_image rejects ---------------------------------------------------
out="$(chaos_image v9.99 2>/dev/null)" && rc=0 || rc=$?
check "chaos_image rejects an unsupported minor"     "$rc"  1
check "chaos_image prints nothing on stdout when it rejects" "$out" ""
err="$(chaos_image v9.99 2>&1 >/dev/null)" || true
check "the rejection names the supported set" "$(printf '%s' "$err" | grep -c 'v1\.34' || true)" 1

# A malformed value must be refused BEFORE it is used to build a variable name.
for bad in '' 'v1' '1.33' 'v1.33; echo pwned' '../etc'; do
  out="$(chaos_image "$bad" 2>/dev/null)" && rc=0 || rc=$?
  check "chaos_image rejects '$bad'"                "$rc"  1
  check "chaos_image prints nothing for '$bad'"     "$out" ""
done

# --- chaos_suffix ---------------------------------------------------------
check "chaos_suffix v1.33" "$(chaos_suffix v1.33)" "-v1-33"
check "chaos_suffix v1.34" "$(chaos_suffix v1.34)" "-v1-34"
out="$(chaos_suffix 'v1.33; echo pwned' 2>/dev/null)" && rc=0 || rc=$?
check "chaos_suffix rejects a malformed minor" "$rc" 1

# --- set -e safety --------------------------------------------------------
# A rejection must not abort a caller that is checking the status itself.
survived=no
chaos_image v9.99 >/dev/null 2>&1 || survived=yes
check "a rejection does not abort the caller under set -e" "$survived" yes

printf '\n%s checks failed\n' "$fails"
[ "$fails" -eq 0 ]
```

- [ ] **Step 2: Run it and watch it fail**

```bash
chmod +x chaos/version-selftest.sh
bash chaos/version-selftest.sh
```

Expected: fails immediately — `chaos/versions.sh: No such file or directory`.

- [ ] **Step 3: Write the data file**

Create `chaos/versions.env`. Data only — no logic, no command substitution, nothing that executes. It is sourced, so it must be safe to source.

```sh
# The Kubernetes minors kubeagent supports, and the kind node image for each.
#
# Digest-pinned on purpose: a tag can be retagged upstream, and a nightly that
# fails because someone else moved a tag teaches everyone to ignore the nightly.
# With a digest, a red cell is always a kubeagent change.
#
# These images ship with kind v0.30.0. Adding or moving a minor is a deliberate,
# reviewed one-line commit — never a silent drift. To add one, resolve its digest
# the same way its neighbours were resolved (chaos/README.md documents the
# command) and add both the minor and its image below.
KUBEAGENT_CHAOS_VERSIONS="v1.32 v1.33 v1.34"
KUBEAGENT_CHAOS_IMAGE_v1_32="kindest/node:v1.32.8@sha256:abd489f042d2b644e2d033f5c2d900bc707798d075e8186cb65e3f1367a9d5a1"
KUBEAGENT_CHAOS_IMAGE_v1_33="kindest/node:v1.33.4@sha256:25a6018e48dfcaee478f4a59af81157a437f15e6e140bf103f85a2e7cd0cbbf2"
KUBEAGENT_CHAOS_IMAGE_v1_34="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
```

Those three digests were resolved from the registry against `kindest/node` and the `v1.34.0` one matches byte-for-byte the image `kind` v0.30.0 embeds as its default — confirmed with `strings "$(command -v kind)" | grep -oE 'kindest/node:v[0-9.]+@sha256:[a-f0-9]+'`. Use them verbatim. Tasks 3 and 4 boot the other two and are where a wrong digest would surface.

- [ ] **Step 4: Write the resolver**

Create `chaos/versions.sh`:

```sh
#!/usr/bin/env bash
# Resolves a Kubernetes minor to the kind node image the chaos harness should
# boot, and to the suffix that keeps two versions' clusters from colliding.
# Sourced by chaos/run.sh and by chaos/version-selftest.sh; the data itself
# lives in chaos/versions.env so the harness and the nightly workflow can never
# disagree about what "supported" means.

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
```

Two bash details that matter here and are easy to get wrong:

- `${!var:-}` is indirect expansion with a default. Plain `${!var}` aborts under `set -u` when the variable is unset, which would kill the caller instead of returning the error this function is written to return.
- `printf -- '-%s\n'` needs the `--`: without it `printf` reads the leading `-` of the format string as a flag.

- [ ] **Step 5: Run the self-test and watch it pass**

```bash
bash chaos/version-selftest.sh
```

Expected: every line `ok`, `0 checks failed`, exit 0.

Then prove the self-test can actually fail — a self-test that cannot fail proves nothing. Temporarily change `KUBEAGENT_CHAOS_IMAGE_v1_33` in `chaos/versions.env` to drop its `@sha256:` digest, re-run, confirm the `chaos_image v1.33 is digest-pinned` check FAILS and the script exits 1, then revert. Record the exact perturbation and the exact failing line in your report.

- [ ] **Step 6: Check it under shellcheck if available**

```bash
command -v shellcheck >/dev/null && shellcheck chaos/versions.sh chaos/version-selftest.sh || echo "shellcheck not installed, skipped"
```

Fix anything it reports. If it is not installed, say so in the report and move on — it is not a blocker.

- [ ] **Step 7: Commit**

```bash
git add chaos/versions.env chaos/versions.sh chaos/version-selftest.sh
git commit -s -m "chaos: pin the supported Kubernetes minors and resolve them"
```

Write a body explaining why the images are digest-pinned. No AI attribution. Then verify: `scripts/dco-check.sh main HEAD`.

---

### Task 2: Wire the axis into the harness

**Files:**
- Modify: `chaos/run.sh` — the header block (lines 8-10), the flag loop (15-23), the report default (30), `create_cluster()` (48-55), and one hardcoded node name in scenario 14 (line 731)
- Test: `bash -n chaos/run.sh`, `bash chaos/version-selftest.sh`, and a targeted `--only` run with no flag

**Interfaces:**
- Consumes: `chaos_image`, `chaos_suffix`, `chaos_versions` from Task 1.
- Produces: the `--k8s-version <minor>` flag and the derived names Tasks 3 and 4 run against.

**The derivation, exactly:**

| | no flag (unchanged) | `--k8s-version v1.33` |
| --- | --- | --- |
| `CLUSTER` | `kubeagent-chaos` | `kubeagent-chaos-v1-33` |
| `CTX` | `kind-kubeagent-chaos` | `kind-kubeagent-chaos-v1-33` |
| `OUT` default | `docs/testing/chaos-results.md` | `docs/testing/chaos-results-v1.33.md` |
| `COREDNS_BACKUP` | `/tmp/kubeagent-chaos-coredns.yaml` | `/tmp/kubeagent-chaos-v1-33-coredns.yaml` |
| `kind create cluster` | no `--image` flag at all | `--image "$(chaos_image v1.33)"` |

Note the report path uses the **dotted** minor (`v1.33`) while the cluster name uses the **dashed** suffix (`-v1-33`) — dots are not legal in a kind cluster name but are perfectly fine in a filename, and the dotted form is what a human reads. This asymmetry is deliberate; do not "fix" it.

`COREDNS_BACKUP` is in the table for a reason that is easy to miss: it is a fixed `/tmp` path, and two versions running on one machine would overwrite each other's pristine Corefile. It must be derived like everything else.

An explicit `--out` still wins over the derived default — the derivation only supplies the default.

- [ ] **Step 1: Write the failing check**

There is no unit-test harness for `run.sh` itself, so the check is a direct observation of the derived values. Add this scratch script at `/tmp/derive-check.sh` (do **not** commit it):

```sh
#!/usr/bin/env bash
# Prints the values chaos/run.sh derives, without creating any cluster.
set -euo pipefail
for args in "" "--k8s-version v1.33"; do
  printf '=== args: %s ===\n' "${args:-<none>}"
  # shellcheck disable=SC2086
  bash -c '
    set -euo pipefail
    # Stop run.sh right after its header, before it does any work.
    sed "/^log() {/,\$d" chaos/run.sh > /tmp/run-header.sh
    . /tmp/run-header.sh "$@"
    printf "CLUSTER=%s\nCTX=%s\nOUT=%s\nCOREDNS_BACKUP=%s\nIMAGE=%s\n" \
      "$CLUSTER" "$CTX" "$OUT" "$COREDNS_BACKUP" "${KIND_IMAGE:-<none>}"
  ' bash $args
done
```

Run it: `bash /tmp/derive-check.sh`

Expected before the change: both blocks print the same unversioned values, and `IMAGE=<none>` in both — that is the failure.

If `sed`-ing the header proves fragile against the real file, do not fight it: instead verify by adding a temporary `printf` of the five values at the top of `main`, running `./chaos/run.sh --k8s-version v1.33 --only 99` (a scenario key that matches nothing, so nothing runs), and removing the `printf` afterwards. Say in your report which method you used.

- [ ] **Step 2: Source the resolver and add the flag**

In `chaos/run.sh`, source `versions.sh` next to where `assert.sh` is already sourced (line 33-35), keeping the same comment style:

```sh
# Kubernetes version axis: chaos_versions / chaos_image / chaos_suffix, backed by
# the digest-pinned set in chaos/versions.env.
# shellcheck source=chaos/versions.sh
. "$ROOT/chaos/versions.sh"
```

Add `K8S_VERSION=""` and `KIND_IMAGE=""` beside the other flag defaults on line 13, then a case arm in the flag loop:

```sh
    --k8s-version) K8S_VERSION="$2"; shift ;;
```

Keep the existing `*) echo "unknown flag: $1" >&2; exit 2 ;;` arm exactly as it is.

- [ ] **Step 3: Derive everything from the flag**

After the flag loop and **before** the `: "${OUT:=...}"` line, insert:

```sh
# The version axis. Omitting --k8s-version keeps the historical names and lets
# kind pick its own default image, so the release skill's documented command and
# an operator's muscle memory keep working byte-for-byte.
#
# Everything cluster-shaped is derived from one place: two minors run on one
# machine otherwise collide on the cluster, the context, the report and the
# CoreDNS scratch file — and a collision on the last one is the nastiest,
# because it silently restores the wrong Corefile.
if [ -n "$K8S_VERSION" ]; then
  KIND_IMAGE="$(chaos_image "$K8S_VERSION")" || exit 2
  suffix="$(chaos_suffix "$K8S_VERSION")"
  CLUSTER="$CLUSTER$suffix"
  CTX="kind-$CLUSTER"
  COREDNS_BACKUP="/tmp/$CLUSTER-coredns.yaml"
  : "${OUT:=docs/testing/chaos-results-$K8S_VERSION.md}"
fi
```

`chaos_image` is called before `chaos_suffix` on purpose: it is the call that validates, and an unsupported minor must exit 2 having created nothing. `|| exit 2` matches the exit code the unknown-flag arm already uses for a bad command line, and `chaos_image` has already written the reason to stderr.

Leave the existing `: "${OUT:=docs/testing/chaos-results.md}"` line where it is — a `:` assignment is a no-op once `OUT` is set, so the versioned default set above wins and the unversioned one still applies when no flag was passed.

- [ ] **Step 4: Pass the image to kind**

In `create_cluster()`, the `kind create cluster` line becomes:

```sh
  log "create kind cluster $CLUSTER${KIND_IMAGE:+ (image $KIND_IMAGE)}"
  kind create cluster --name "$CLUSTER" --config chaos/kind-config.yaml --wait 120s \
    ${KIND_IMAGE:+--image "$KIND_IMAGE"}
```

`${KIND_IMAGE:+--image "$KIND_IMAGE"}` expands to nothing at all when `KIND_IMAGE` is empty, so the no-flag path runs the exact command it runs today. Note this is one of the few places an unquoted expansion is correct — it must split into two words. Add a short comment saying so, or shellcheck (and the next reader) will flag it as a bug.

`kind`'s `--image` overrides the node image for every node in the `--config` file, so the 1 control-plane + 2 worker topology is unaffected.

- [ ] **Step 5: Fix the hardcoded node name in scenario 14**

`chaos/run.sh:731` reads:

```sh
  leaks="$(grep -cE '"prompt":[^\n]*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-z]{6,}|kubeagent-chaos-worker)' "$calls" 2>/dev/null || true)"
```

That literal `kubeagent-chaos-worker` is a node name. On a versioned cluster the node is named `kubeagent-chaos-v1-33-worker`, so the alternation stops matching and **the leak check goes vacuous** — it would report 0 leaks on a run where the daemon really did forward a node name into a model prompt. `expect_eq "... leaks" "$leaks" 0` would pass, which is exactly the "passes on a broken build" failure slice 7a exists to prevent.

Derive it from `$CLUSTER`:

```sh
  # The node name is derived, not hardcoded: on a versioned cluster the nodes are
  # named after that cluster, and a stale literal here would quietly match nothing
  # and report every run leak-free.
  leaks="$(grep -cE '"prompt":[^\n]*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-z]{6,}|'"$CLUSTER"'-worker)' "$calls" 2>/dev/null || true)"
```

Then **sweep for siblings**. Run:

```bash
grep -n 'kubeagent-chaos' chaos/run.sh
```

Every hit other than the `CLUSTER=` assignment on line 8 is a candidate for the same bug. Fix each one that is a cluster-derived name; leave anything that is genuinely a fixed string. List every hit and your ruling on it in your report.

- [ ] **Step 6: Verify the derivation both ways**

```bash
bash -n chaos/run.sh
bash chaos/version-selftest.sh
bash /tmp/derive-check.sh
```

Expected from the derive check: with no flag, the four historical values and `IMAGE=<none>`; with `--k8s-version v1.33`, `CLUSTER=kubeagent-chaos-v1-33`, `CTX=kind-kubeagent-chaos-v1-33`, `OUT=docs/testing/chaos-results-v1.33.md`, `COREDNS_BACKUP=/tmp/kubeagent-chaos-v1-33-coredns.yaml`, and a digest-pinned `IMAGE`.

Then check the three refusal paths, none of which create anything:

```bash
./chaos/run.sh --k8s-version v9.99 ; echo "exit=$?"   # expect exit=2, stderr names the supported set
./chaos/run.sh --k8s-version       ; echo "exit=$?"   # expect a non-zero exit, no cluster created
kind get clusters                                      # expect NO kubeagent-chaos-v9-99
```

And confirm `--out` still overrides:

```bash
bash /tmp/derive-check.sh 2>/dev/null | head -1   # sanity
./chaos/run.sh --k8s-version v1.33 --out /tmp/explicit.md --only 99 ; echo "exit=$?"
```

`--only 99` matches no scenario, so this exercises the flag plumbing without running an outage. Confirm the run reports `/tmp/explicit.md` as its report path, not the derived one. Note this DOES create a v1.33 cluster — that is fine and Task 3 will reuse it; it takes a few minutes.

- [ ] **Step 7: Prove the default run is unchanged**

This is the constraint that matters most in this task. The `kubeagent-chaos` cluster from slice 7a is up; reuse it.

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --only 04 --out /tmp/s04-after-axis.md ; echo "exit=$?"
```

Expected: `exit=0`, `assertions: 10 run, 0 failed`. Scenario 4 is a good probe because it exercises the CNI, two scans and the report path. Also run `--only 14` (`./chaos/run.sh --only 14 --out /tmp/s14-after-axis.md`) — that is the scenario whose node-name regex you just changed, and it must still report `leaks: 0` with 9 assertions passing.

- [ ] **Step 8: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: run the suite against any supported Kubernetes minor"
```

The body should say what is derived and why omitting the flag changes nothing. Mention the scenario 14 node-name fix explicitly — it is a latent vacuous assertion, not a cosmetic rename. No AI attribution.

---

### Task 3: First green full run on v1.33

**Files:**
- Modify: whatever the run reveals — most likely `chaos/run.sh`, possibly nothing
- Test: the full suite itself

**Interfaces:**
- Consumes: `--k8s-version` from Task 2.
- Produces: a green v1.33 report, and a list of any real defects found.

This is the slice's actual deliverable. The spec says to expect this task to find real defects — in the harness, and possibly in kubeagent. Budget 40-60 minutes including triage.

- [ ] **Step 1: Run the full suite against v1.33**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --k8s-version v1.33 --recreate \
  --out /tmp/chaos-v1.33.md > /tmp/chaos-v1.33.log 2>&1 ; echo "exit=$?"
```

Run it in the background and watch the log. When it finishes:

```bash
tail -3 /tmp/chaos-v1.33.log
grep -c '^PASS:' /tmp/chaos-v1.33.md
grep    '^FAIL:' /tmp/chaos-v1.33.md
grep 'ASSERTION FAILED' /tmp/chaos-v1.33.log
```

The v1.34 baseline for comparison is **103 assertions, 0 failed**. The v1.33 run must run the same 103 — a *lower* count means a scenario aborted early and is as much a defect as a FAIL.

- [ ] **Step 2: Triage every failure, and rule on each before fixing anything**

For each FAIL, decide which of these it is, and say so explicitly in your report:

1. **A harness assumption that is version-specific** — a name, a path, a field that moved. Fix the harness.
2. **An assertion pinned to Kubernetes' wording rather than kubeagent's contract** — the Global Constraints forbid these. Rewrite the assertion to assert what kubeagent *did*, not what the API server said. This is the case the spec predicts most.
3. **A real kubeagent defect on v1.33.** **Stop. Do not touch the assertion.** Report it, with the evidence, as its own finding. It gets its own commit with its own Go unit test — which is out of this task's scope, so hand it back rather than absorbing it.
4. **A flake** — a timing artifact, an image pull, a rollout deadline. Re-run that scenario alone (`--k8s-version v1.33 --only NN`) to confirm, and if it is genuinely a flake, fix the wait rather than widening the assertion.

**Forbidden fixes**, all of which would make the gate worthless and all of which are Global Constraints:

- a per-version expected-value table
- loosening `expect_eq` to `expect_ge`, or lowering a floor, to make a number fit
- skipping a scenario on a minor without a comment naming the exact API that is absent
- deleting an assertion

- [ ] **Step 3: Fix, and re-run only what you changed**

After each harness fix, re-run just the affected scenario:

```bash
./chaos/run.sh --k8s-version v1.33 --only NN --out /tmp/v133-NN.md ; echo "exit=$?"
```

Then confirm the fix did not break the default version:

```bash
./chaos/run.sh --only NN --out /tmp/v134-NN.md ; echo "exit=$?"
```

Both must pass. A fix that makes v1.33 pass and v1.34 fail has just moved the problem.

- [ ] **Step 4: Full green re-run**

Once every scenario passes individually, run the whole suite again end to end:

```bash
./chaos/run.sh --k8s-version v1.33 --recreate \
  --out /tmp/chaos-v1.33-final.md > /tmp/chaos-v1.33-final.log 2>&1 ; echo "exit=$?"
```

Expected: `exit=0` and `assertions: 103 run, 0 failed`. If the count differs from 103, explain exactly why in your report — a legitimate reason exists only if you added or removed an assertion, and you must say which.

- [ ] **Step 5: Re-run the default version end to end**

```bash
./chaos/run.sh --recreate --out /tmp/chaos-v1.34-recheck.md > /tmp/chaos-v1.34-recheck.log 2>&1 ; echo "exit=$?"
```

Expected: `exit=0`, `assertions: 103 run, 0 failed`. Skip this only if Steps 2-3 changed no shared code at all, and say so.

- [ ] **Step 6: Commit**

Commit only if you changed something. One commit per distinct fix, each with a body naming the version-specific behaviour it accounts for.

```bash
git add chaos/run.sh
git commit -s -m "chaos: <what was version-specific and how it is now derived>"
```

Report the wall-clock time of the full v1.33 run — slice 7c needs it to set the workflow's job timeout.

---

### Task 4: First green full run on v1.32

**Files:**
- Modify: whatever the run reveals
- Test: the full suite itself

**Interfaces:**
- Consumes: everything Tasks 2 and 3 produced.
- Produces: a green v1.32 report — the oldest supported minor, and the one most likely to lack an API the harness assumes.

v1.32 is two minors behind the default and is where a genuinely absent API would show up. This is the one place the Global Constraints allow a skip — and only with a comment naming the API and the minor that lacks it.

- [ ] **Step 1: Run the full suite against v1.32**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --k8s-version v1.32 --recreate \
  --out /tmp/chaos-v1.32.md > /tmp/chaos-v1.32.log 2>&1 ; echo "exit=$?"
```

- [ ] **Step 2: Triage with the same four-way ruling as Task 3**

Same rules, same forbidden fixes. One addition specific to this minor: if a scenario genuinely cannot run because v1.32 lacks an API that v1.33 and v1.34 have, a skip is permitted, and it must look like this — the comment naming the API and the minor is mandatory:

```sh
  # <API group/version/kind> is not served by Kubernetes v1.32; this scenario
  # cannot be injected there. Skipped rather than asserted-around, so the report
  # says "not run" instead of quietly passing.
  if [ "$K8S_VERSION" = "v1.32" ]; then
    printf 'skipped: <API> is not served by Kubernetes v1.32\n' \
      | record "<N>. <title>" "skipped on this minor"
    return
  fi
```

A skipped scenario runs **no** assertions — it must not record a PASS. Before writing a skip, satisfy yourself the API is genuinely absent (`kubectl --context "$CTX" api-resources | grep -i <kind>`) rather than merely renamed or moved to a different group; a rename is case 1, a harness fix.

- [ ] **Step 3: Fix and re-run per scenario, then all three versions**

Per-scenario as in Task 3, then the full suite on each of the three minors in turn, each exiting 0. Report the assertion count for each; if v1.32's is lower because of a documented skip, say which scenario and which API.

- [ ] **Step 4: Commit**

One commit per distinct fix, each with a body naming the version-specific behaviour. Report every defect you ruled as case 3 (a real kubeagent defect) — do not fix those here.

---

### Task 5: Document the axis

**Files:**
- Modify: `chaos/README.md`
- Modify: `.claude/skills/release/SKILL.md` (Step 3)
- Test: read the files back and check every command in them against the code

**Interfaces:**
- Consumes: the flag and the derived names from Task 2, and the measured run times from Tasks 3 and 4.

Slice 7a's own review caught a documentation claim that was false (a helper described as printing to the console when it only ever printed to the report). Documentation that describes the gate inaccurately is a real defect. Check every claim you write against the code.

- [ ] **Step 1: Read what is there now**

Read `chaos/README.md` end to end, and Step 3 of `.claude/skills/release/SKILL.md`. Note that `chaos/README.md` has copy-paste examples pinned to `kind-kubeagent-chaos` — those stay correct for the default run and must not be rewritten to be versioned. The point is to document that the axis exists, not to version every example.

- [ ] **Step 2: Add a version-axis section to `chaos/README.md`**

It must cover, accurately:

- `--k8s-version <minor>`, and the fact that omitting it changes nothing.
- The derivation table from Task 2 (cluster, context, report, CoreDNS scratch file, image), with the real values.
- Which minors are supported, and that `chaos/versions.env` is the single source both this harness and the nightly workflow read.
- Why the images are digest-pinned, in one sentence: a retagged upstream image would otherwise turn a nightly red with no kubeagent change.
- How to add a minor, with the actual command that resolves a digest:

  ````
  ```bash
  TOKEN=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:kindest/node:pull" \
    | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
  curl -s -o /dev/null -D - -H "Authorization: Bearer $TOKEN" \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://registry-1.docker.io/v2/kindest/node/manifests/v1.35.0" \
    | grep -i '^docker-content-digest:'
  ```
  ````

- The measured wall-clock for a full run on each minor, from Tasks 3 and 4.
- That two minors can be run on one machine without colliding — that is the whole reason everything is derived.

- [ ] **Step 3: Update the release skill's Step 3**

One or two sentences: the release gate is still the default single-version `./chaos/run.sh --recreate`, and `--k8s-version` exists for cross-version checking. Do not turn the release gate into a three-version run — that is a 2-hour gate and nobody asked for it. Check the surrounding text is still accurate while you are in there.

- [ ] **Step 4: Verify every claim**

Re-read what you wrote against `chaos/run.sh`, `chaos/versions.sh` and `chaos/versions.env`. For each factual claim — a flag name, a derived value, an exit code, a file path — point at the line that makes it true. List those file:line pairs in your report. A claim you cannot point at is a claim to delete.

- [ ] **Step 5: Commit**

```bash
git add chaos/README.md .claude/skills/release/SKILL.md
git commit -s -m "docs: the chaos harness runs on any supported Kubernetes minor"
```

`.claude/skills/release/SKILL.md` is tracked but sits under a gitignored path, so it needs `git add -f` if a plain `git add` refuses it.

---

## Slice gate

After Task 5, before the whole-branch review:

- [ ] The full suite exits 0 on all three minors: `--k8s-version v1.32`, `--k8s-version v1.33`, and no flag. Record each one's assertion count and wall-clock.
- [ ] No two of them collide: after all three have run, `kind get clusters` shows three distinct clusters and `ls docs/testing/chaos-results*` shows three distinct reports.
- [ ] A deliberate perturbation on a non-default minor still fails the gate: break one injection or one expected value under `--k8s-version v1.33`, confirm exit 1 and an `ASSERTION FAILED:` line on the console, then revert. The gate must be a gate on every cell, not only the default one.
- [ ] `git diff --stat main..HEAD -- go.mod go.sum internal/ '*.go'` prints nothing.
- [ ] `scripts/dco-check.sh main HEAD` reports every commit signed off.
- [ ] `go build ./... && go test ./... -p 2` is green.

## Self-review notes

**Spec coverage.** The spec's slice 7b is "`versions.env`, `--k8s-version`, per-version cluster/context/report naming, and the first green run against a minor that is not v1.34." Task 1 covers `versions.env`; Task 2 covers the flag and the naming (plus `COREDNS_BACKUP`, which the spec's table omits but which is a real collision the spec's own "must never collide on a laptop" requirement demands); Tasks 3 and 4 cover the green runs, on both non-default minors rather than one, because slice 7c matrixes all three and a minor first exercised in CI is a minor that fails in CI. Task 5 is documentation, which the spec assigns to 7c — pulled forward here because the axis is unusable undocumented and 7c's docs are about the *nightly*, not the flag.

**Deliberately out of scope.** The nightly workflow, artifacts, and the "which axes are untested" documentation are slice 7c. No GitHub Actions file is touched here.

**Known coupling this plan does not solve.** Several scenarios bind fixed localhost ports (18082, 18083 and others). Two *concurrent* runs on one machine would collide on those regardless of the version suffix. The spec's requirement is that two cells never collide, and in the nightly each version gets its own runner; locally, runs are sequential. If a task finds a reason to make the ports derived too, that is a legitimate finding to report — not a requirement of this slice.
