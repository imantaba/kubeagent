# Chaos k3s Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Gate a second Kubernetes distribution (k3s, created by k3d) in the nightly chaos matrix, so the cross-distribution promise at `website/docs/compatibility.md:132-136` is closed by evidence rather than by a hand-run.

**Architecture:** `chaos/run.sh` gains a `--distro kind|k3s` axis beside the existing `--k8s-version` axis. `kind` stays the default and every derived name on that path is byte-for-byte what it is today. `k3s` is a first-class creation mode: the harness creates the cluster with k3d, owns it, and deletes it — so `cluster_write` is granted honestly. `node_exec` is withheld on k3s because k3s has no separately stoppable etcd or kubelet, and its canonical reason string stops being ownership-shaped. The four discovered capabilities are **not** special-cased: the probes reach their k3s answers on their own.

**Tech Stack:** bash (`chaos/run.sh`, `chaos/versions.sh`, `chaos/versions.env`, `chaos/assert-selftest.sh`, `chaos/version-selftest.sh`), GitHub Actions YAML, Markdown. **No Go code changes.**

**Spec:** [docs/superpowers/specs/2026-08-05-chaos-k3s-gate-design.md](../specs/2026-08-05-chaos-k3s-gate-design.md) (commits `7a00500`, `31bc369`). The spec is the requirements; it records settled decisions with the alternatives each closes off, and reopening one is a defect.

## Global Constraints

Every task's requirements implicitly include this section.

- **DANGER — never run `./chaos/run.sh` in any form.** Not bare, not `--recreate`, not `--only`, not `--context`, not `--distro`. A run takes ~40 minutes and injects real outages (stops etcd, fills disks, deletes namespaces). Every end-to-end run in this plan is the controller's, not an implementer's. `bash -n chaos/run.sh`, `bash chaos/assert-selftest.sh` and `bash chaos/version-selftest.sh` are the only harness commands a task may run.
- Every commit needs a `Signed-off-by` trailer matching its author (`git commit -s`) — `main` enforces DCO. Verify with `bash scripts/dco-check.sh main HEAD`. Do **not** add a second `Signed-off-by` by hand.
- **No `Co-Authored-By: Claude` trailer and no AI attribution anywhere** — commits, code, comments, docs, changelog, workflow files.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change; no Go file is touched. **Portable mode's binary list is `kubectl go curl python3` and must not grow.** k3d is required on exactly one path (`--distro k3s`) and nowhere else.
- **No chaos helper may ever return non-zero for a recorded outcome.** Under `set -euo pipefail` a failing assertion must let the remaining scenarios run and surface only at the end in the exit code.
- **`assert_summary`'s exit status stays the gate**: non-zero if and only if an assertion failed. A skip is never a failure.
- **The published count 134 does not move.** It lives in exactly four files — `CLAUDE.md`, `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md` — and it is the **kind** cell's count. It appears in neither `chaos/run.sh` nor `chaos/assert-selftest.sh`, whose own counts are not published. If a task finds itself editing 134, that is a defect in the task.
- **`scenario_01_etcd` stays LAST in `run_scenarios()`, and the `all=(...)` list itself does not change.**
- **The vocabulary stays a closed set of six capability names.** No name is added or removed. `capability_add` still exits 2 on an unknown name; `requires` still exits 2 on an unknown name rather than skipping silently.
- No secrets, credentials, private IPs or internal hostnames anywhere — the results file, CI artifacts, workflow files, README and every doc example. RFC 5737 addresses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), RFC 2606 domains (`example.com`, `example.org`, `example.net`). Kubeconfig paths and kubeconfig **context names** are credentials, as are Kubernetes **node names** and **external load-balancer addresses**. A context name may appear on stderr or the console and must never reach `$OUT`. Nothing emitted may carry more than `scheme://host`.
- **Never expose API keys to the shell.** The harness runs with `ANTHROPIC_API_KEY` unset; `explain_flag()` already gates on its presence and must not change. Reading the variable to report `enabled`/`disabled` is fine; emitting its value is not.
- kubeagent is **read-only toward the cluster**; the chaos **harness** deliberately is not — it injects outages. Never merge those into one sentence, and never blur "read-only" into "makes no external calls": read-only describes cluster operations, making no LLM call is a separate, stronger claim.
- The six versioned JSON documents do not move. No `schemaVersion` bump, no schema regeneration. `internal/report/testdata/golden-scan.txt` stays byte-identical. Do **not** regenerate the demo GIF or `website/docs/quickstart.md`.
- **The kind path must not change behaviour.** Every derived name (cluster, context, report path, CoreDNS scratch file), the kind header line, the kind `wait_system_ready` waits and the kind image resolution stay exactly as they are. The full kind run must still measure 134 assertions / 0 failed / 1 scenario skipped.
- **`shellcheck` is not installed on this machine.** `bash -n <file>` is the syntax gate, and every task runs it on every file it edits.
- TDD: write the failing selftest check first, watch it fail, then implement. New cluster-free coverage goes in `chaos/assert-selftest.sh` or `chaos/version-selftest.sh`, both of which must stay sub-second.

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `chaos/versions.env` | the data: supported minors, and one digest-pinned image per minor **per distribution** | 1 |
| `chaos/versions.sh` | resolvers: `chaos_versions`, `chaos_image`, `chaos_suffix`, **`chaos_k3s_image`**, **`chaos_newest`** | 1 |
| `chaos/version-selftest.sh` | cluster-free proof that the resolvers answer and reject correctly | 1 |
| `chaos/run.sh` | the harness: flag axes, preflight, cluster lifecycle, capabilities, scenarios, report | 2–8 |
| `chaos/assert-selftest.sh` | cluster-free proof of the harness's pure helpers | 2, 3, 7 |
| `.github/workflows/chaos-matrix.yml` | the nightly gate: a distribution axis beside the minor axis | 9 |
| `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md`, `CLAUDE.md`, `CHANGELOG.md` | what the gate now covers, in the operator's and the consumer's words | 10 |

---

### Task 1: The k3s image table and its resolvers

**Files:**
- Modify: `chaos/versions.env`
- Modify: `chaos/versions.sh`
- Test: `chaos/version-selftest.sh`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `chaos_k3s_image <minor>` → prints a digest-pinned `rancher/k3s` reference on stdout, exit 0; on an unsupported or malformed minor prints **nothing** on stdout, a message on stderr, exit 1. `chaos_newest` → prints the newest supported minor (`v1.34` today), exit 0. Task 2 calls both.

**Context:** `chaos/versions.sh` already holds `chaos_versions`, `_chaos_known`, `chaos_image` and `chaos_suffix`, and `chaos/versions.env` holds the data. `_chaos_known` does the shape check *and* the membership test, and both existing resolvers call it before deriving a variable name. The new resolver follows that discipline exactly — the image reference becomes `k3d cluster create --image`, and a caller that ignored a non-zero status would otherwise boot whatever k3d defaults to.

The three image references below were resolved against `registry-1.docker.io` with a bearer token from `auth.docker.io` (never `hub.docker.com`) and are the newest patch of each supported minor. Use them **verbatim**.

- [ ] **Step 1: Write the failing checks**

Append to `chaos/version-selftest.sh`, immediately before the `# --- set -e safety` block:

```bash
# --- chaos_newest ---------------------------------------------------------
# The newest supported minor, resolved rather than typed. Two callers mean
# "the newest one" — the k3s path's default image and the CI matrix's single
# k3s cell — and a second copy of that answer is a copy that goes stale.
check "chaos_newest is the last entry in the supported list" \
  "$(chaos_newest)" "$(chaos_versions | awk '{print $NF}')"
check "chaos_newest names a minor both resolvers accept" \
  "$(chaos_image "$(chaos_newest)" >/dev/null && chaos_k3s_image "$(chaos_newest)" >/dev/null && echo ok)" ok

# --- chaos_k3s_image ------------------------------------------------------
# Same contract as chaos_image, for the same reason: a bare tag would let a
# silently retagged upstream image turn a green nightly red with no kubeagent
# change, and an empty answer would hand `k3d cluster create` an empty --image.
for m in $(chaos_versions); do
  img="$(chaos_k3s_image "$m")" && rc=0 || rc=$?
  check "chaos_k3s_image $m exits 0"           "$rc" 0
  check "chaos_k3s_image $m names rancher/k3s" "$(printf '%s' "$img" | grep -c '^rancher/k3s:' || true)" 1
  check "chaos_k3s_image $m is digest-pinned"  "$(printf '%s' "$img" | grep -cE '@sha256:[0-9a-f]{64}$' || true)" 1
  check "chaos_k3s_image $m names the minor"   "$(printf '%s' "$img" | grep -cF "rancher/k3s:${m}." || true)" 1
done

# A rejection must print nothing on stdout, whatever shape the bad value has:
# an unsupported minor, a near-miss prefix, a malformed string, or an injection
# attempt that must never reach the variable-name derivation.
for bad in v9.99 v1.3 v1.320 v01.32 '' 'v1' '1.33' 'v1.33; echo pwned' '../etc'; do
  out="$(chaos_k3s_image "$bad" 2>/dev/null)" && rc=0 || rc=$?
  check "chaos_k3s_image rejects '$bad'"            "$rc"  1
  check "chaos_k3s_image prints nothing for '$bad'" "$out" ""
done
err="$(chaos_k3s_image v9.99 2>&1 >/dev/null)" || true
check "the k3s rejection names the supported set" \
  "$(printf '%s' "$err" | grep -c 'v1\.34' || true)" 1
```

- [ ] **Step 2: Run the selftest to verify it fails**

Run: `bash chaos/version-selftest.sh`
Expected: FAIL — `chaos_newest: command not found` / `chaos_k3s_image: command not found`, and a non-zero exit.

- [ ] **Step 3: Add the images**

In `chaos/versions.env`, extend the header comment and append the three k3s references. The file's existing comment explains why the kind images are digest-pinned; the addition explains why there are now two families:

```bash
# Two image families, one minor list. The kind images boot a kubeadm-shaped
# control plane; the rancher/k3s images boot k3s under k3d. A minor is only
# "supported" if BOTH resolve, because the nightly matrix runs kind on every
# minor and k3s on the newest one, and a minor that resolves for one and not
# the other is a half-supported minor nobody can act on.
KUBEAGENT_CHAOS_K3S_IMAGE_v1_32="rancher/k3s:v1.32.13-k3s1@sha256:7534b63e02277917f77c584ed5532b31562c760d6bb8fe88059002e9bdeee033"
KUBEAGENT_CHAOS_K3S_IMAGE_v1_33="rancher/k3s:v1.33.13-k3s2@sha256:ada5ff2e138120efe877f76d514dedda65b304122112b982eab532732c028c89"
KUBEAGENT_CHAOS_K3S_IMAGE_v1_34="rancher/k3s:v1.34.10-k3s1@sha256:e27c6ae5717752d4460efbb06595966a06d044301af5c8cf6c0bbf6b9bf53e3b"
```

- [ ] **Step 4: Add the resolvers**

In `chaos/versions.sh`, append after `chaos_suffix`:

```bash
# chaos_newest — the newest supported minor (the last entry in the list).
#
# Two places mean "the newest one": the k3s path's default image, and the CI
# matrix's single k3s cell. Both resolve it from here rather than naming a
# minor, so adding or dropping a minor stays the one-line commit it is today.
chaos_newest() { printf '%s\n' "$KUBEAGENT_CHAOS_VERSIONS" | awk '{print $NF}'; }

# chaos_k3s_image <minor> — the digest-pinned rancher/k3s image for <minor>:
# chaos_image's counterpart on the k3s path, with the same contract for the
# same reason. It validates before it derives a variable name, and it never
# prints a partial answer, because a caller that ignored the status would hand
# `k3d cluster create` an empty --image and boot whatever k3d defaults to —
# which, unlike kind's default, moves with the k3d release.
chaos_k3s_image() {
  if ! _chaos_known "${1:-}"; then
    printf 'unsupported --k8s-version: %s (supported: %s)\n' \
      "${1:-<empty>}" "$KUBEAGENT_CHAOS_VERSIONS" >&2
    return 1
  fi
  local var="KUBEAGENT_CHAOS_K3S_IMAGE_${1//./_}"
  if [ -z "${!var:-}" ]; then
    printf 'chaos/versions.env lists %s but defines no k3s image for it\n' "$1" >&2
    return 1
  fi
  printf '%s\n' "${!var}"
}
```

Update `chaos/versions.sh`'s file header comment: it says the file "Resolves a Kubernetes minor to the kind node image"; it now resolves a minor to **the node image for either distribution**.

- [ ] **Step 5: Run the selftests to verify they pass**

Run: `bash -n chaos/versions.sh && bash chaos/version-selftest.sh && bash chaos/assert-selftest.sh`
Expected: both PASS (`0 checks failed`, `assert-selftest: all checks passed`), each in well under a second.

- [ ] **Step 6: Commit**

```bash
git add chaos/versions.env chaos/versions.sh chaos/version-selftest.sh
git commit -s -m "chaos: resolve a digest-pinned k3s image for every supported minor"
```

---

### Task 2: The `--distro` flag and the derived-name axis

**Files:**
- Modify: `chaos/run.sh` (globals at line 13, the flag loop at 18-28, the `--context` block at 45-61, the version-axis block at 80-87)
- Test: `chaos/assert-selftest.sh`

**Interfaces:**
- Consumes: `chaos_k3s_image <minor>`, `chaos_newest` (Task 1).
- Produces: globals `DISTRO` (`kind`|`k3s`), `DISTRO_SET` (`0`|`1`), `K3S_IMAGE` (the digest-pinned reference, empty on the kind path). Tasks 3–9 read `DISTRO`; Task 4 reads `K3S_IMAGE`.

**Context:** `run.sh`'s flag loop rejects an unknown flag with `exit 2`. The `--context` block (portable mode) fires three refusals *before* the version axis derives any name — the same discipline `chaos_image` follows, and the fourth refusal joins it. The version axis today derives `CLUSTER`, `CTX`, `COREDNS_BACKUP` and `OUT` only when `--k8s-version` is given; it now derives them for both axes. **A collision on `COREDNS_BACKUP` is the nastiest of the four, because it silently restores the wrong Corefile** — so the k3s path must not inherit the default kind run's `/tmp/kubeagent-chaos-coredns.yaml`.

- [ ] **Step 1: Write the failing checks**

Append to `chaos/assert-selftest.sh`, immediately before the `# --- redact_nodes:` block:

```bash
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
```

- [ ] **Step 2: Run the selftest to verify it fails**

Run: `bash chaos/assert-selftest.sh`
Expected: FAIL — every new check misses, starting with `unknown flag: --distro` giving rc 2 where 0 is wanted.

- [ ] **Step 3: Declare the globals and parse the flag**

In `chaos/run.sh`, extend line 13's globals block:

```bash
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""; K8S_VERSION=""; KIND_IMAGE=""
DISTRO=kind; DISTRO_SET=0; K3S_IMAGE=""   # the distribution axis; see the block below
```

Add to the flag loop, after the `--k8s-version` arm:

```bash
    --distro) DISTRO="$2"; DISTRO_SET=1; shift ;;
```

- [ ] **Step 4: Validate the value before anything is derived from it**

Insert immediately after the `--only` zero-pad normalization (line 33) and **before** the `--context` block:

```bash
# The distribution axis. --distro picks which Kubernetes distribution the
# harness CREATES: kind (the default, and what every command line written before
# this flag existed means) or k3s, run in containers by k3d.
#
# The value is validated HERE, before the --context refusal below and before any
# name is derived from it, for the same reason chaos_image validates before
# chaos_suffix derives a name: a value that becomes a cluster name, a context and
# a report path has no business being unchecked.
case "$DISTRO" in
  kind|k3s) ;;
  *) echo "unknown --distro: $DISTRO (supported: kind, k3s)" >&2; exit 2 ;;
esac
```

- [ ] **Step 5: Add the fourth refusal**

Inside the `if [ -n "$CONTEXT" ]; then` block, after the `--k8s-version` refusal:

```bash
  if [ "$DISTRO_SET" = 1 ]; then
    echo "--context and --distro are mutually exclusive: --context runs against a cluster that already exists, and --distro says which distribution to create" >&2
    exit 2
  fi
```

Extend that block's existing comment — it says "Three flags are REFUSED rather than ignored"; it is now four, and the fourth is refused because the two flags answer contradictory questions rather than because it manages a lifecycle.

- [ ] **Step 6: Derive every cluster-shaped name from one place**

Replace the version-axis block (the `if [ -n "$K8S_VERSION" ]; then … fi` at 80-87 and the `: "${OUT:=docs/testing/chaos-results.md}"` line that follows it) with:

```bash
# Two axes, one derivation. Everything cluster-shaped — the cluster name, the
# context, the report path and the CoreDNS scratch file — is derived here,
# because two runs that collide on any one of them corrupt each other, and a
# collision on the last one is the nastiest: it silently restores the wrong
# Corefile.
#
# The kind path is byte-for-byte what it was before --distro existed. Omitting
# --k8s-version still lets kind pick its own node image, so the release skill's
# documented command and an operator's muscle memory keep working.
#
# The k3s path ALWAYS pins an image, even without --k8s-version: k3d bundles a
# default k3s version that moves with the k3d release, and an unpinned image
# makes a red cell ambiguous — the whole reason versions.env exists.
#
# chaos_k3s_image / chaos_image run FIRST because they are the calls that
# validate. chaos_suffix therefore cannot fail below.
#
# Portable mode is skipped entirely: it has already set CTX from --context and
# OUT to the portable report, and all three of the flags this block reads are
# refused above.
if [ "$PORTABLE" = 0 ]; then
  if [ "$DISTRO" = k3s ]; then
    CLUSTER="$CLUSTER-k3s"
    K3S_IMAGE="$(chaos_k3s_image "${K8S_VERSION:-$(chaos_newest)}")" || exit 2
  fi
  if [ -n "$K8S_VERSION" ]; then
    if [ "$DISTRO" = kind ]; then KIND_IMAGE="$(chaos_image "$K8S_VERSION")" || exit 2; fi
    suffix="$(chaos_suffix "$K8S_VERSION")"
    CLUSTER="$CLUSTER$suffix"
  fi
  case "$DISTRO" in
    kind) CTX="kind-$CLUSTER"; : "${OUT:=docs/testing/chaos-results${K8S_VERSION:+-$K8S_VERSION}.md}" ;;
    k3s)  CTX="k3d-$CLUSTER";  : "${OUT:=docs/testing/chaos-results-k3s${K8S_VERSION:+-$K8S_VERSION}.md}" ;;
  esac
  COREDNS_BACKUP="/tmp/$CLUSTER-coredns.yaml"
fi
```

Note the `. "$ROOT/chaos/versions.sh"` source line must stay **above** this block — it already is (line 66).

- [ ] **Step 7: Run the selftests to verify they pass**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh && bash chaos/version-selftest.sh`
Expected: both PASS. Every pre-existing check must still pass — in particular the `requires`, `capability_*` and `redact_*` blocks, which source `run.sh` the same way.

- [ ] **Step 8: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "chaos: add the --distro axis and derive every cluster-shaped name from it"
```

---

### Task 3: Preflight and the inotify check become distro-aware

**Files:**
- Modify: `chaos/run.sh` (`check_inotify_limits` at 113-140, `preflight` at 142-148)
- Test: `chaos/assert-selftest.sh`

**Interfaces:**
- Consumes: `DISTRO` (Task 2).
- Produces: `cluster_tool` → prints `kind` or `k3d`. Tasks 4 and 9 use the same mapping.

**Context:** `preflight()` requires `docker kind kubectl helm go curl python3`. On the k3s path `kind` is not used at all and `k3d` is. `check_inotify_limits` warns whenever the host budget is low and **refuses to start** only when it is low *and* another cluster is already up — that pair is what actually breaks. Its "other clusters" query shells `kind get clusters`, which answers nothing about k3d clusters. The failure mode is identical on both (every kubelet and kube-proxy inside a node container draws from the same host-wide budget), so the warning text becomes distro-neutral and only the query branches.

`portable_preflight`'s binary list — `kubectl go curl python3` — **must not change**. Portable mode creates no cluster and side-loads no image.

- [ ] **Step 1: Write the failing checks**

Append to `chaos/assert-selftest.sh`, after the distro-axis block from Task 2:

```bash
# cluster_tool is the single mapping from distro to the binary that creates and
# deletes the cluster: preflight requires it, teardown calls it, and CI installs
# it. Three copies of that answer is two too many.
check 'cluster_tool is kind by default' \
  "$( ( set --; . chaos/run.sh; cluster_tool ) )" kind
check 'cluster_tool is k3d on the k3s path' \
  "$( ( . chaos/run.sh --distro k3s; cluster_tool ) )" k3d
```

- [ ] **Step 2: Run the selftest to verify it fails**

Run: `bash chaos/assert-selftest.sh`
Expected: FAIL — `cluster_tool: command not found`, so both checks get an empty value.

- [ ] **Step 3: Add `cluster_tool` and branch the preflight**

In `chaos/run.sh`, add immediately above `check_inotify_limits`:

```bash
# cluster_tool — the binary that creates and deletes this run's cluster. One
# mapping, read by preflight, by teardown and by the inotify check, so a fourth
# caller cannot invent a fifth answer.
cluster_tool() { case "$DISTRO" in k3s) printf 'k3d\n' ;; *) printf 'kind\n' ;; esac; }
```

Replace `preflight`'s binary loop:

```bash
preflight() {
  for b in docker "$(cluster_tool)" kubectl helm go curl python3; do
    command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
  done
  docker info >/dev/null 2>&1 || { echo "docker daemon not running" >&2; exit 1; }
  check_inotify_limits
}
```

- [ ] **Step 4: Make the "other clusters" query distro-aware**

In `check_inotify_limits`, replace the single `others=` assignment with:

```bash
  case "$DISTRO" in
    k3s) others="$(k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | grep -vx "$CLUSTER" | tr '\n' ' ' || true)" ;;
    *)   others="$(kind get clusters 2>/dev/null | grep -vx "$CLUSTER" | tr '\n' ' ' || true)" ;;
  esac
```

Reword the two message blocks so they are true of both distributions — the numbers are kind's published recommendation and stay, but the prose stops saying kind is the only thing that needs them:

- `'inotify limits are below what kind needs:\n'` → `'inotify limits are below what a containerized Kubernetes node needs:\n'`
- `'  fs.inotify.max_user_instances = %s (kind recommends %s)\n'` → `'  fs.inotify.max_user_instances = %s (recommended: %s)\n'` (and the same for `max_user_watches`)
- `'Refusing to start: these kind clusters are already running and will\n'` → `'Refusing to start: these clusters are already running and will\n'`

Extend the function's header comment: it explains the budget in terms of "a kind node"; add one sentence saying a k3d node is the same shape and draws from the same budget, which is why only the query branches.

- [ ] **Step 5: Run the selftests to verify they pass**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh`
Expected: PASS, including both new checks.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "chaos: preflight and the inotify check learn which distro is being created"
```

---

### Task 4: Create, load into, and delete a k3d cluster

**Files:**
- Modify: `chaos/run.sh` (`preload_flux_images` at 445-455, `teardown` at 746-749, and a new `create_cluster_k3s` beside `create_cluster` at 400-418)

**Interfaces:**
- Consumes: `DISTRO`, `K3S_IMAGE`, `CLUSTER`, `RECREATE` (Task 2); `cluster_tool` (Task 3).
- Produces: `create_cluster_k3s`, `node_image_load <ref>`. Task 8 calls `create_cluster_k3s` from `main`.

**Context:** k3d v5.9.0 is installed at `$HOME/.local/bin/k3d` on this machine (SHA256 `06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0`); Task 9 pins the same version and digest in CI. Measured: `k3d cluster create <name> --servers 1 --agents 2 --image <digest> --wait --timeout 300s` takes about 71 seconds and leaves the kubeconfig context `k3d-<name>` selected. `k3d cluster list --no-headers` prints one line per cluster with the name first. `k3d image import <ref> --cluster <name>` is the counterpart of `kind load docker-image <ref> --name <name>`. `k3d cluster delete <name>` takes the name positionally.

**`preload_calico_images` stays kind-only and unchanged** — it is called from the kind branch alone, and its `docker.io/` prefix handling is specific to how `kind load` tags an image.

- [ ] **Step 1: Add `create_cluster_k3s`**

In `chaos/run.sh`, immediately after `create_cluster`:

```bash
# create_cluster_k3s — create_cluster's counterpart on the k3s path.
#
# k3d runs k3s in containers, so the harness owns this cluster exactly as fully
# as it owns a kind one: it created it, it can delete it, and nobody else's
# workload is on it. That ownership is what lets main() grant cluster_write here
# as honestly as it does on kind.
#
# STOCK DEFAULTS ARE LOAD-BEARING, not an aesthetic preference. k3s ships Traefik
# as a LoadBalancer holding ports 80 and 443 on every node its affinity allows,
# and that is exactly why ServiceLB never publishes an address for a second
# port-80 Service — which is what makes probe_capabilities grant no_loadbalancer
# and scenario 6 RUN rather than skip. Disabling Traefik (`--k3s-arg
# '--disable=traefik@server:*'`) would free port 80, ServiceLB would assign an
# address, and scenario 6 would fail. Do not add it.
create_cluster_k3s() {
  local existing
  existing="$(k3d cluster list --no-headers 2>/dev/null | awk '{print $1}')" || existing=""
  if printf '%s\n' "$existing" | grep -qx "$CLUSTER"; then
    if [ "$RECREATE" = 1 ]; then k3d cluster delete "$CLUSTER"; else
      echo "cluster $CLUSTER already exists (use --recreate to rebuild)"; return 0; fi
  fi
  log "create k3d cluster $CLUSTER (image $K3S_IMAGE)"
  k3d cluster create "$CLUSTER" --servers 1 --agents 2 --image "$K3S_IMAGE" \
    --wait --timeout 300s
}
```

The cluster list is captured on its own line rather than piped straight into `grep -qx`: under `pipefail`, `grep -q` exits the moment it matches and can leave the producer with a broken pipe, so a match would be indistinguishable from a failure.

- [ ] **Step 2: Add `node_image_load` and route the Flux preload through it**

Immediately above `preload_flux_images`:

```bash
# node_image_load <ref> — side-load an image into this distro's node store.
#
# Both node kinds keep their own containerd store, which is the entire reason
# the preloads exist; only the command differs.
node_image_load() {
  case "$DISTRO" in
    k3s) k3d image import "$1" --cluster "$CLUSTER" ;;
    *)   kind load docker-image "$1" --name "$CLUSTER" ;;
  esac
}
```

In `preload_flux_images`, replace the `kind load docker-image "$ref" --name "$CLUSTER" || echo …` line with:

```bash
    node_image_load "$ref" || echo "preload: load $ref failed; falling back to in-node pull" >&2
```

and extend the comment above it: the reason it needs no prefix stripping is unchanged, and the reason the preload matters on k3d is the same one it matters on kind — a node's own containerd store, not anything kind-specific.

- [ ] **Step 3: Make teardown delete the cluster it created**

```bash
# A failed teardown must not abort main before assert_summary runs: the exit
# code callers read is the assertion gate's, and losing it to the cluster tool's
# would report a delete failure as a scenario failure and drop the report's
# summary.
teardown() {
  log "teardown"
  case "$DISTRO" in
    k3s) k3d cluster delete "$CLUSTER" || log "teardown: k3d cluster delete failed (cluster may still exist)" ;;
    *)   kind delete cluster --name "$CLUSTER" || log "teardown: kind delete cluster failed (cluster may still exist)" ;;
  esac
}
```

- [ ] **Step 4: Verify syntax and the selftests**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh && bash chaos/version-selftest.sh`
Expected: PASS. (Neither new function is cluster-free testable — both shell out to a cluster tool. Their correctness is established by the controller's end-to-end runs.)

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: create, side-load into and delete a k3d cluster"
```

---

### Task 5: `wait_system_ready` waits for the workloads each distro actually has

**Files:**
- Modify: `chaos/run.sh` (`wait_system_ready` at 471-476)

**Interfaces:**
- Consumes: `DISTRO`, `CTX` (Task 2).
- Produces: `wait_for_deploy <namespace> <name> [seconds]`.

**Context:** Today `wait_system_ready` waits on `kube-system deploy/coredns`, `kube-system deploy/calico-kube-controllers` and `local-path-storage deploy/local-path-provisioner`. Measured on k3s: there is **no `local-path-storage` namespace** and **no Calico controller**; `kube-system` holds coredns, local-path-provisioner, metrics-server and traefik. Traefik arrives later than the others because k3s installs it through a HelmChart job, and `kubectl rollout status` fails outright on a Deployment that does not exist yet.

**Waiting for Traefik is load-bearing, not tidiness.** Until Traefik's LoadBalancer has claimed port 80, `probe_capabilities`' LoadBalancer probe can get an address from ServiceLB — `no_loadbalancer` would be withheld and scenario 6 would skip instead of running, intermittently.

- [ ] **Step 1: Add `wait_for_deploy`**

Immediately above `wait_system_ready`:

```bash
# wait_for_deploy <namespace> <name> [seconds] — wait for a Deployment to EXIST,
# then for it to become Available.
#
# `kubectl rollout status` fails outright on a Deployment that is not there yet,
# and on k3s several of these are created by a HelmChart job some time after the
# nodes go Ready. Poll for existence first, then hand over to rollout status,
# which is what actually waits for Available. A Deployment that never appears
# leaves rollout status to fail loudly, which is the same outcome the kind path
# has always had.
wait_for_deploy() {
  local ns="$1" name="$2" secs="${3:-300}" i
  for i in $(seq "$secs"); do
    kubectl --context "$CTX" -n "$ns" get deploy "$name" >/dev/null 2>&1 && break
    sleep 1
  done
  kubectl --context "$CTX" -n "$ns" rollout status "deploy/$name" --timeout="${secs}s"
}
```

- [ ] **Step 2: Branch `wait_system_ready`**

```bash
# wait_system_ready blocks until the core system Deployments are Available, so the
# baseline scan sees a settled cluster. On a freshly-created cluster these can
# still be Pending for a while after the nodes go Ready — scanning too early makes
# the baseline read Degraded (a harness timing artifact, not a real finding).
#
# The two distributions genuinely ship different workloads: k3s has no Calico
# controller and no local-path-storage namespace, and runs its own
# local-path-provisioner, metrics-server and Traefik in kube-system. Waiting for
# a Deployment the cluster does not have would fail every run.
#
# Traefik is on the k3s list for a reason that is easy to mistake for
# thoroughness: until its LoadBalancer has claimed port 80, ServiceLB can still
# hand probe_capabilities' probe Service an address, no_loadbalancer would be
# withheld, and scenario 6 would skip instead of running. Waiting is what makes
# that answer stable rather than a race.
wait_system_ready() {
  case "$DISTRO" in
    k3s)
      log "wait for system workloads to settle (CoreDNS, local-path, metrics-server, Traefik)"
      wait_for_deploy kube-system coredns
      wait_for_deploy kube-system local-path-provisioner
      wait_for_deploy kube-system metrics-server
      wait_for_deploy kube-system traefik
      ;;
    *)
      log "wait for system workloads to settle (CoreDNS, Calico controllers, local-path)"
      kubectl --context "$CTX" -n kube-system rollout status deploy/coredns --timeout=300s
      kubectl --context "$CTX" -n kube-system rollout status deploy/calico-kube-controllers --timeout=300s
      kubectl --context "$CTX" -n local-path-storage rollout status deploy/local-path-provisioner --timeout=300s
      ;;
  esac
}
```

The kind arm is copied verbatim from today's body — same three commands, same order, same timeouts.

- [ ] **Step 3: Verify syntax and the selftests**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: wait for the system workloads each distribution actually ships"
```

---

### Task 6: The report header names the distribution

**Files:**
- Modify: `chaos/run.sh` (extract the inline header block in `main` at 2505-2511 into a function beside `portable_header` at 327)

**Interfaces:**
- Consumes: `DISTRO`, `CTX` (Task 2).
- Produces: `created_header` → writes the report header to stdout. Task 8 calls it from `main` in place of the inline block.

**Context:** `main` writes the header inline for the harness-created path and calls `portable_header` for the portable one. Extracting the created-cluster form gives the two paths the same shape and gives the k3s form somewhere to live. The kind form's text must not change — it is `- Cluster: Kind %s, Calico CNI, 1 control-plane + 2 workers` with `kind version | awk '{print $2}'`.

Measured: `k3d version` prints `k3d version v5.9.0` on its first line, so `awk 'NR==1 {print $3}'` yields `v5.9.0`.

- [ ] **Step 1: Add `created_header`**

Immediately above `portable_header`:

```bash
# created_header — the report header for a cluster the harness created.
#
# Distro-specific because the two clusters differ in exactly the ways a reader of
# this report needs to know: which CNI enforces (or does not enforce)
# NetworkPolicy, whether metrics-server is present, and whether a LoadBalancer
# Service can ever get an address. Those three are what decide which scenarios
# run at all, so naming the add-ons is naming the shape of the run.
#
# Neither form names a node. The harness owns these node names, so they are not
# the credential a foreign cluster's are — but a header that carries one is one
# copy-paste away from a portable report, and there is no reason for it to.
created_header() {
  printf '# kubeagent chaos-test results\n\n'
  case "$DISTRO" in
    k3s)
      printf -- '- Cluster: k3d %s, k3s stock defaults (Flannel CNI, Traefik + ServiceLB, metrics-server, local-path), 1 server + 2 agents\n' \
        "$(k3d version 2>/dev/null | awk 'NR==1 {print $3}')"
      ;;
    *)
      printf -- '- Cluster: Kind %s, Calico CNI, 1 control-plane + 2 workers\n' \
        "$(kind version 2>/dev/null | awk '{print $2}')"
      ;;
  esac
  printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
  printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
}
```

The `explain` line reads the variable to report `enabled`/`disabled` and never emits its value — unchanged from the block it replaces.

- [ ] **Step 2: Verify syntax and the selftests**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh`
Expected: PASS. `main` still writes the old inline block at this point — Task 8 swaps it. Adding the function now keeps that swap a two-line change.

- [ ] **Step 3: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: give the created-cluster report header a distro-aware form"
```

---

### Task 7: Worker selection by role label, and `node_exec`'s honest reason

**Files:**
- Modify: `chaos/run.sh` (`worker_node` at 535-547, `capability_reason` at 692-702)
- Test: `chaos/assert-selftest.sh`

**Interfaces:**
- Consumes: `DISTRO`, `CTX` (Task 2).
- Produces: nothing new; both are existing functions with changed behaviour.

**Context:** `worker_node` greps node names for the literal `worker`. kind names its workers `<cluster>-worker` and `<cluster>-worker2`; k3d names its agents `k3d-<cluster>-agent-0` and `-agent-1`. Measured on k3d: the server node carries `node-role.kubernetes.io/control-plane=true` and the agents carry no role label at all, so selecting a node **without** that label is correct on both. Scenario 03 (cordon) is `cluster_write`-guarded and therefore **runs** on k3s, so this is load-bearing rather than cosmetic.

`capability_reason node_exec` currently reads *"needs shell access to a node container, which exists only on a cluster the harness created"*. That is false on a k3s run — the harness **did** create it, and `docker exec` into a k3d node would work. The refusal is about the control plane's **shape**: k3s defaults to an embedded sqlite datastore (no etcd to stop) and its kubelet is part of the single k3s process. The reason string becomes a statement about shape, true on both a foreign cluster and a harness-created k3s one.

`worker_node` cannot be selftested cluster-free — it shells `kubectl` against a live cluster. Its correctness on both distributions comes from the controller's two end-to-end runs, where scenario 03 cordons a node on each.

- [ ] **Step 1: Write the failing checks**

The selftest already pins `capability_reason cluster_write`'s wording. Append beside it:

```bash
# node_exec's reason is about the control plane's SHAPE, not about ownership.
# The harness owns a k3d cluster as fully as it owns a kind one, so an
# ownership-shaped reason would be simply false on the k3s path — and a skip
# reason that is false in the report is worse than no reason at all.
check 'capability_reason grounds node_exec in shape, not ownership' \
  "$( ( set --; . chaos/run.sh; capability_reason node_exec ) )" \
  'needs shell access to a node running a kubeadm-shaped control plane, where etcd and kubelet are separately stoppable units'
check 'the node_exec reason makes no ownership claim' \
  "$( ( set --; . chaos/run.sh; capability_reason node_exec ) | grep -ci 'harness created\|does not own' || true)" 0
```

- [ ] **Step 2: Run the selftest to verify it fails**

Run: `bash chaos/assert-selftest.sh`
Expected: FAIL on both — the current string is returned instead.

- [ ] **Step 3: Reword the reason**

In `capability_reason`, replace the `node_exec)` arm:

```bash
    node_exec)         printf 'needs shell access to a node running a kubeadm-shaped control plane, where etcd and kubelet are separately stoppable units\n' ;;
```

Extend the capability block's header comment, which today says the two policy capabilities turn on whether the harness owns the cluster. That is still exactly true of `cluster_write`. `node_exec` is narrower and now says so: the harness owns a k3d cluster too, but k3s has no separately stoppable etcd or kubelet, so granting it there would make scenarios 1 and 11 **run and fail** — strictly worse than a named skip.

- [ ] **Step 4: Select the worker by role label**

Replace `worker_node`'s body and update its comment:

```bash
# worker_node — the name of the first node that is not the control plane.
#
# Two scenarios cordon or docker-exec into a worker by name. Selecting on the
# ABSENCE of the control-plane role label is what makes this work on both
# distributions: kind names its workers <cluster>-worker and <cluster>-worker2,
# k3d names its agents k3d-<cluster>-agent-0, and only the label is common to
# both. (Matching the literal string "worker" silently found nothing on k3d.)
#
# Reading it with a bare pipeline fails badly when nothing matches: under
# `set -o pipefail` an empty result aborts the run at the assignment, with no
# message and no assertion summary, so a single-node or renamed cluster looks
# like a harness crash. Diagnose it instead — this is an environment problem, in
# the same class as a missing binary, not a kubeagent finding.
worker_node() {
  local n
  n="$(kubectl --context "$CTX" get nodes \
        -l '!node-role.kubernetes.io/control-plane' -o name 2>/dev/null \
        | sed -n '1s|^node/||p')" || true
  if [ -z "$n" ]; then
    {
      printf 'no worker node found in context %s.\n' "$CTX"
      printf 'The harness creates one control-plane node and two workers (k3d: one server, two agents); scenarios 3 and 11 need one.\n'
      printf 'Re-create the cluster with: %s%s --recreate\n' "$0" \
        "$([ "$DISTRO" = kind ] || printf ' --distro %s' "$DISTRO")"
    } >&2
    exit 1
  fi
  printf '%s\n' "$n"
}
```

`sed -n '1s|…|…|p'` rather than `head -1`: `sed` reads its whole input, so there is no broken pipe for `pipefail` to catch.

The context name appears here on **stderr** only, which is the accepted carve-out this function already relies on — it never reaches `$OUT`.

- [ ] **Step 5: Run the selftests to verify they pass**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh`
Expected: PASS, including both new checks and every pre-existing one.

- [ ] **Step 6: Commit**

```bash
git add chaos/run.sh chaos/assert-selftest.sh
git commit -s -m "chaos: select a worker by role label, and ground node_exec's refusal in shape"
```

---

### Task 8: Wire the k3s path through `main`

**Files:**
- Modify: `chaos/run.sh` (`main` at 2482-2596)

**Interfaces:**
- Consumes: everything from Tasks 2–7.
- Produces: nothing new.

**Context:** `main` branches twice on `PORTABLE`: once to set the cluster up, once to write the header and grant the two policy capabilities. Both branches gain a k3s arm. The four discovered capabilities are **not** touched — `probe_capabilities` and the baseline block reach their k3s answers on their own, which is the whole point of the seam.

`cluster_write` is granted on **both** created paths: the harness created the cluster and can delete it, on kind and on k3d alike. `node_exec` is granted on the **kind path only**.

- [ ] **Step 1: Branch the setup**

Replace the setup block at the top of `main`:

```bash
  if [ "$PORTABLE" = 1 ]; then
    portable_preflight
    portable_node_redaction
    build_kubeagent
  else
    preflight
    build_kubeagent
    case "$DISTRO" in
      k3s) create_cluster_k3s ;;
      *)   create_cluster; preload_calico_images; install_calico ;;
    esac
  fi
```

Calico is kind-only: k3s ships Flannel and the harness leaves it alone, which is exactly why `netpol_enforced` is not granted there.

- [ ] **Step 2: Branch the header and the capability grants**

Replace the header/backup/capability block:

```bash
  if [ "$PORTABLE" = 1 ]; then
    portable_header >> "$OUT"
  else
    created_header >> "$OUT"

    # Capture the pristine CoreDNS Corefile TEXT now (cluster is healthy) so scenario 5
    # can restore a known-good config via a clean merge-patch (apply of a get-dump is unreliable).
    kubectl --context "$CTX" -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' > "$COREDNS_BACKUP" 2>/dev/null || true

    wait_system_ready

    # POLICY, not probe: on a cluster the harness created and can delete, it may
    # write cluster-scoped objects. That is true of a k3d cluster exactly as it
    # is of a kind one — the harness created both.
    capability_add cluster_write

    # node_exec is narrower, and granted on the kind path only. `docker exec`
    # into a k3d node would work, so this is not about access: k3s defaults to
    # an embedded sqlite datastore, so there is no etcd to stop, and its kubelet
    # is part of the single k3s process rather than a separate unit. Granting it
    # here would make scenarios 1 and 11 RUN AND FAIL, which is strictly worse
    # than a named skip — see capability_reason's wording.
    if [ "$DISTRO" = kind ]; then capability_add node_exec; fi
  fi
```

- [ ] **Step 3: Name the right delete command on exit**

In the tail block, replace the "cluster left up" hint:

```bash
  else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    case "$DISTRO" in
      k3s) echo "  k3d cluster delete $CLUSTER" ;;
      *)   echo "  kind delete cluster --name $CLUSTER" ;;
    esac
  fi
```

The context name on the console is the accepted carve-out — the operator's own channel, and it never reaches `$OUT`.

- [ ] **Step 4: Verify the whole file**

Run: `bash -n chaos/run.sh && bash chaos/assert-selftest.sh && bash chaos/version-selftest.sh`
Expected: PASS.

Also confirm by inspection, and report each in the task report:
- `run_scenarios`' `all=(...)` list is unchanged and `01_etcd` is still last.
- No occurrence of `134` was added anywhere in `chaos/run.sh`.
- The six capability names are unchanged — `grep -c 'printf' ` inside `capability_reason` still finds six arms.
- `portable_preflight`'s binary list is still exactly `kubectl go curl python3`.
- `git diff main -- go.mod go.sum` is empty.

- [ ] **Step 5: Commit**

```bash
git add chaos/run.sh
git commit -s -m "chaos: wire the k3s path through main, granting cluster_write but not node_exec"
```

---

### Task 9: The nightly matrix gains a distribution axis

**Files:**
- Modify: `.github/workflows/chaos-matrix.yml`

**Interfaces:**
- Consumes: `chaos_versions`, `chaos_newest`, `chaos_image`, `chaos_k3s_image` (Task 1); `./chaos/run.sh --distro` (Task 2).
- Produces: nothing consumed by later tasks.

**Context:** Today the `versions` job emits a JSON array of minors and the `chaos` job's matrix is `version: ${{ fromJson(...) }}`. Adding `distro` through `include:` would **merge** into the existing cell whose `version` matches rather than adding a cell, so the `versions` job now emits the whole matrix object — `{"include":[…]}` — and the job consumes it as `matrix: ${{ fromJson(…) }}`.

Four cells: `kind` × three supported minors, plus one `k3s` cell at the newest supported minor, resolved from `chaos/versions.env` rather than typed. All four run in parallel, so wall-clock does not move.

k3d **v5.9.0**, asset `k3d-linux-amd64`, SHA256 `06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0`. There is no published checksums asset for k3d, so the value is recorded here the way `KIND_SHA256` already is — bumping the k3d version means re-fetching the asset and updating this value alongside it, never dropping the check. Fetching a public release asset in CI is not a URL leak: the rule governs what kubeagent **emits**.

`ANTHROPIC_API_KEY` stays unset for the whole workflow. The credential scan runs over the k3s report exactly as it does over the kind ones.

- [ ] **Step 1: Emit the whole matrix from the `versions` job**

Rename the output to `matrix` (unchanged) but build an include list. Replace the `resolve` step's script:

```bash
          set -euo pipefail
          . chaos/versions.sh
          picked="${INPUT_VERSIONS:-}"
          [ -n "$picked" ] || picked="$(chaos_versions)"
          # Validate every requested minor through the same resolvers the
          # harness uses, so a typo in a dispatch input fails here — in
          # seconds — instead of after a cluster has been built.
          for v in $picked; do chaos_image "$v" >/dev/null; done
          # The distribution axis is deliberately NOT a cross product: the kind
          # cells cover the minor dimension and one k3s cell covers the
          # distribution dimension, so a full product would double runner cost
          # for mostly redundant signal. The k3s cell runs the newest supported
          # minor, resolved from chaos/versions.env so it cannot go stale.
          newest="$(chaos_newest)"
          chaos_k3s_image "$newest" >/dev/null
          json="$(printf '%s\n' $picked \
            | jq -R '{version: ., distro: "kind"}' \
            | jq -sc --arg newest "$newest" '{include: (. + [{version: $newest, distro: "k3s"}])}')"
          printf 'matrix=%s\n' "$json" >> "$GITHUB_OUTPUT"
          printf 'running: %s (kind) + %s (k3s)\n' "$picked" "$newest"
```

- [ ] **Step 2: Consume it as the whole matrix**

```yaml
    strategy:
      fail-fast: false
      matrix: ${{ fromJson(needs.versions.outputs.matrix) }}
    name: chaos (${{ matrix.distro }} ${{ matrix.version }})
```

Extend the workflow's header comment: it says "One job per supported Kubernetes minor"; it is now one job per supported minor on kind, plus one k3s cell at the newest minor, and it says why the axis is not a cross product.

- [ ] **Step 3: Install the cluster tool the cell needs**

Gate the existing kind step and add its k3d sibling:

```yaml
      - name: Install kind
        if: matrix.distro == 'kind'
        env:
          # (comment unchanged)
          KIND_SHA256: 517ab7fc89ddeed5fa65abf71530d90648d9638ef0c4cde22c2c11f8097b8889
        run: |
          # (script unchanged)

      - name: Install k3d
        if: matrix.distro == 'k3s'
        env:
          # k3d-linux-amd64, the k3d-io/k3d v5.9.0 release asset. k3d publishes
          # no checksums asset, so this value was recorded from the downloaded
          # binary. Bumping the k3d version means re-fetching the asset and
          # updating this value alongside it — never dropping the check.
          K3D_SHA256: 06d8f25bc3a971c4eb29e0ff08429b180402db0f4dec838c9eac427e296800a0
        run: |
          set -euo pipefail
          curl -sSLo /tmp/k3d https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-linux-amd64
          printf '%s  /tmp/k3d\n' "$K3D_SHA256" | sha256sum -c -
          chmod +x /tmp/k3d
          sudo mv /tmp/k3d /usr/local/bin/k3d
          k3d version
```

The inotify step stays unconditional — a k3d node draws from the same host-wide budget — and its comment gains one clause saying so.

- [ ] **Step 4: Make the preflight, the run, the scan and the upload distro-aware**

```yaml
      - name: Preflight tools
        env:
          DISTRO: ${{ matrix.distro }}
        run: |
          set -euo pipefail
          tool=kind; [ "$DISTRO" = k3s ] && tool=k3d
          for b in docker "$tool" kubectl helm go curl python3; do
            command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
          done
          docker info >/dev/null

      # The report path is derived exactly the way run.sh derives it, so the
      # scan and the upload below can never read a path the harness did not
      # write.
      - name: Resolve the report path
        id: report
        env:
          DISTRO: ${{ matrix.distro }}
          VERSION: ${{ matrix.version }}
        run: |
          set -euo pipefail
          if [ "$DISTRO" = k3s ]; then
            printf 'path=docs/testing/chaos-results-k3s-%s.md\n' "$VERSION" >> "$GITHUB_OUTPUT"
          else
            printf 'path=docs/testing/chaos-results-%s.md\n' "$VERSION" >> "$GITHUB_OUTPUT"
          fi

      - name: Run the chaos suite on ${{ matrix.distro }} ${{ matrix.version }}
        run: ./chaos/run.sh --distro '${{ matrix.distro }}' --k8s-version '${{ matrix.version }}' --recreate --teardown
```

In the scan step, replace the hardcoded `rep=` line with `rep='${{ steps.report.outputs.path }}'`, leaving every pattern and the two-step AWS check exactly as they are.

In the upload step, use the same output for `path:` and name the artifact `chaos-report-${{ matrix.distro }}-${{ matrix.version }}`.

- [ ] **Step 5: Verify the workflow parses**

Run:

```bash
python3 -c 'import sys,yaml; yaml.safe_load(open(".github/workflows/chaos-matrix.yml"))' && echo "yaml ok"
```

Then check the matrix the `versions` job would emit, without GitHub:

```bash
bash -c '. chaos/versions.sh
picked="$(chaos_versions)"; newest="$(chaos_newest)"
printf "%s\n" $picked | jq -R "{version: ., distro: \"kind\"}" \
  | jq -sc --arg newest "$newest" "{include: (. + [{version: \$newest, distro: \"k3s\"}])}"'
```

Expected: `{"include":[{"version":"v1.32","distro":"kind"},{"version":"v1.33","distro":"kind"},{"version":"v1.34","distro":"kind"},{"version":"v1.34","distro":"k3s"}]}` — four cells.

If `yaml` or `jq` is unavailable, say so in the task report rather than skipping the check silently.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/chaos-matrix.yml
git commit -s -m "ci: run the chaos matrix on k3s as well as kind"
```

---

### Task 10: Say what the gate now covers

**Files:**
- Modify: `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md`, `CLAUDE.md`, `CHANGELOG.md`

**Interfaces:** consumes the k3s run's **measured** assertion count, failure count and skip list, which the controller supplies in the dispatch. **Do not run `./chaos/run.sh`** — every number in this task is handed to you.

**Context:** Five documents describe what the gate covers, and each has a different reader.

The kind cell's **134 does not move** in any of them. What changes is that `website/docs/compatibility.md`'s "134 machine-checked assertions per cell" becomes explicitly a statement about the kind axis. The k3s cell's measured count is published in **two** places only — `website/docs/compatibility.md` and `chaos/README.md`, the two documents that describe what CI covers — named alongside the five scenarios it skips and why. It goes in neither `CLAUDE.md` nor the roadmap: a number in four places is a number that goes stale in two of them.

Expected k3s skips, all five naming their reason in the assertion summary: **01 etcd** and **11 kubelet** (`node_exec`), **04 networkpolicy** (`netpol_enforced`), **18 capacity** (`no_metrics_server`), **02 certs** (the unconditional documented skip, as on kind). Eighteen scenarios run, including all six `cluster_write` ones and scenario 06.

- [ ] **Step 1: `chaos/README.md`**

- **Prerequisites**: add k3d beside kind, marked as needed **only** for `--distro k3s` — `curl -sSLo k3d https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-linux-amd64 && chmod +x k3d && sudo mv k3d /usr/local/bin/`. Note the inotify paragraph applies to both, because a k3d node is a container too.
- **Run**: add `./chaos/run.sh --distro k3s   # create a k3s cluster (k3d) instead of kind`.
- A new **`### Distributions`** subsection after `### Kubernetes versions`: what `--distro` does, that `kind` is the default and every older command line is unchanged, that the k3s path always pins a `rancher/k3s` digest while the kind path without `--k8s-version` still lets kind choose, the derived report paths, and that `--distro` is refused with `--context` because the two flags answer contradictory questions. State plainly that **the harness must not disable Traefik on the k3s path**, and why: Traefik holds port 80, which is what keeps ServiceLB from publishing an address, which is what makes scenario 6 run rather than skip.
- **What this matrix does and does not cover**: the "Kubernetes distribution" bullet no longer says "Only kind" and no longer lists k3s among the untested. It now says the matrix covers kind on every supported minor and k3s on the newest, names the five scenarios the k3s cell skips and why, and keeps EKS, GKE, AKS, OpenShift and RKE2 as untested — with the managed-control-plane caveat intact, because neither kind nor k3d has a cloud provider's admission chain, IAM-mapped auth, or managed upgrade behaviour.
- Line 151's "Each cell runs 134 assertions" becomes explicit about which cell: each **kind** cell runs 134; the k3s cell runs `<measured>`, fewer because five scenarios skip rather than because anything is weaker.

- [ ] **Step 2: `website/docs/compatibility.md`**

Rewrite the two paragraphs at lines 120-136 and the portable-mode paragraph at 138-145:

- The nightly matrix now runs **two distributions**: kind on each supported minor and k3s on the newest, each on its own disposable cluster. Say the kind cells run 134 machine-checked assertions each and the k3s cell runs `<measured>`, with five scenarios skipped and named.
- The "What the matrix does not cover" paragraph drops k3s from the not-gated list and keeps EKS, GKE, AKS, OpenShift and RKE2 — architecture (amd64) and runner (`ubuntu-latest`) unchanged. CNI is no longer "one": Calico on the kind cells, Flannel on the k3s cell, and NetworkPolicy enforcement is asserted only where a CNI enforces it.
- The portable-mode paragraph's closing sentence — "That makes a cross-distribution answer **obtainable by hand**. It does not make one **gated**, which is still ahead." — is now false in its second half. Replace it with what is true: a second distribution **is** gated, and portable mode remains the way to point the harness at a distribution the matrix does not gate.
- Do **not** claim coverage this slice does not add. One distribution beyond kind is gated; five are not.

- [ ] **Step 3: `website/docs/roadmap.md`**

The line reading "Pointing it at a distribution is now a hand-run away; **gating one in CI is still ahead**" is the promise this slice closes. Rewrite it to say a second distribution is gated nightly, name k3s, and keep the honest remainder — the managed distributions are still ungated and still reachable only by hand. Do not put an assertion count here.

- [ ] **Step 4: `CLAUDE.md`**

The Theme H paragraph says the chaos matrix "runs the full suite nightly once per supported minor". Add that it now also runs one k3s cell at the newest supported minor, and that `--distro kind|k3s` selects which distribution the harness creates. **No number.**

- [ ] **Step 5: `CHANGELOG.md`**

Under `## [Unreleased]`, an **Added** entry: `chaos/run.sh --distro kind|k3s`, the digest-pinned `rancher/k3s` image per supported minor, and the nightly matrix's k3s cell. A **Changed** entry: `node_exec`'s skip reason now names the control plane's shape rather than cluster ownership, and `worker_node` selects by role label so it works on both distributions. No AI attribution; no `Co-Authored-By`.

- [ ] **Step 6: Verify the docs build**

Run:

```bash
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f website/mkdocs.yml
```

Expected: exit 0 with no `WARNING` naming a changed page. The red "Material for MkDocs 2.0" banner is cosmetic.

Then confirm 134 still appears in exactly the four published places and nowhere new:

```bash
grep -rn '134' CLAUDE.md chaos/README.md website/docs/compatibility.md website/docs/roadmap.md chaos/run.sh chaos/assert-selftest.sh
```

Expected: one hit in each of the four documents, none in either script.

- [ ] **Step 7: Commit**

```bash
git add chaos/README.md website/docs/compatibility.md website/docs/roadmap.md CLAUDE.md CHANGELOG.md
git commit -s -m "docs: the nightly chaos matrix gates a second distribution"
```

---

## After the tasks: the gate

Not a task — the controller's work, because a chaos run injects real outages and takes about forty minutes.

1. `./chaos/run.sh --recreate` on the kind path must still measure **134 assertions, 0 failed, 1 scenario skipped**. Any other number is a regression in this branch, not a documentation update.
2. `./chaos/run.sh --distro k3s --recreate --teardown` must complete, and its assertion count, failure count and skip list are **measured** — the five expected skips and nothing else. Anything outside that list skipping, or any scenario in it running, is a finding to investigate rather than a number to write down.
3. The k3s report must carry no node name, no context name and no external address.
4. Task 10's numbers come from step 2 and are written only after it.

## Self-review

- **Spec coverage.** The flag and its four refusals (Task 2); the distro-dependent binary list (Task 3); the capability table, with `node_exec` withheld and reworded and the four probes untouched (Tasks 7, 8); `worker_node`, Calico, the Flux preload, `wait_system_ready`, `check_inotify_limits`, the report header, the derived names and `versions.env`/`versions.sh` (Tasks 1, 3–8); CI (Task 9); numbers and where they live, plus every document (Task 10); the end-to-end runs (the gate section). The spec's "what this slice does not do" list is honoured: no RKE2, no ownership-affirmation flag, no k3s-shaped variants of scenarios 01 and 11, no mirror-image assertions for 06 and 18, no Go change.
- **Placeholders.** `<measured>` in Task 10 is the one deliberate blank, and the task says who fills it and where it comes from. Every other value is literal.
- **Type consistency.** `DISTRO`, `DISTRO_SET`, `K3S_IMAGE`, `cluster_tool`, `node_image_load`, `create_cluster_k3s`, `wait_for_deploy`, `created_header`, `chaos_k3s_image`, `chaos_newest` are spelled identically in every task that produces or consumes them.
