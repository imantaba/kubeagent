# Cross-version chaos matrix — slice 7c (the nightly workflow) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the chaos suite nightly on GitHub Actions across all three supported Kubernetes minors, upload each cell's report as an artifact that is provably free of credential material, and document exactly which axes are tested and which are not.

**Architecture:** One new workflow file, `.github/workflows/chaos-matrix.yml`, modelled on the existing `.github/workflows/fuzz.yml`: `schedule` + `workflow_dispatch`, `strategy.matrix.version` over the minors in `chaos/versions.env`, `fail-fast: false` so one bad minor cannot hide the others, one artifact per cell. Two cluster-free self-tests (`chaos/assert-selftest.sh`, `chaos/version-selftest.sh`) move into `ci.yml` so the harness's own logic is checked on every pull request rather than only on the nightly. The cron is enabled **last**, after a real runner has been measured — per the spec, a cell that cannot finish reliably gets fewer scenarios and a recorded coverage reduction, never a longer timeout.

**Tech Stack:** GitHub Actions (`ubuntu-latest`), bash, kind, kubectl, helm, Go. No new Go code, no new dependency.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every commit needs a `Signed-off-by` trailer matching its author** (`git commit -s`) — `main` enforces DCO. Verify with `scripts/dco-check.sh main HEAD`. A `git merge -m` commit does not get one automatically; amend with `git commit --amend -s --no-edit`. Never add a second `Signed-off-by` by hand.
- **No AI attribution anywhere** — no `Co-Authored-By: Claude` trailer, no "Generated with Claude Code" footer, no mention of an AI assistant in a commit message, a workflow file, a doc, a comment, or a changelog entry.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change. This slice touches **no Go production code at all**.
- **No secrets are needed by this workflow and none are granted.** `permissions: contents: read`. No `secrets.*` reference of any kind. `ANTHROPIC_API_KEY` is never set, so the `--explain` scenarios skip — that is the design, not a gap.
- **A CI artifact is not gitignored.** `docs/testing/` reports stay local today; uploaded, they leave the machine. Scenario 20's existing assertion — `credential material in recorded output: 0` — becomes load-bearing. The workflow must independently confirm an uploaded report carries no bearer token or certificate material, and fail the job if it does.
- **No secrets, credentials, private IPs, or internal hostnames anywhere**, including the workflow file, its logs, and every uploaded artifact. Documentation IPs must be RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`example.com`, `example.org`, `example.net`).
- **URLs are credentials.** No log line, workflow step, doc example, or artifact may carry more than `scheme://host`. The project's own `https://k8sproject.top/...` links are fine.
- **`chaos/versions.env` is the single source of truth for what "supported" means.** The workflow must not hardcode a second copy of the minor list; it reads the same file the harness reads, so the two can never disagree.
- **Never expose API keys to the shell.** No step may echo, export, or print one.
- **A cell that cannot finish reliably gets fewer scenarios, recorded as reduced coverage — never a longer timeout and a flaky nightly.** (Spec, Risks.)
- `go test` runs with `-p 2` locally and never `-short`; CI's `go test -race ./...` must stay green and is untouched by this slice.
- Never implement on `main` directly. Work continues on branch `chaos-matrix`.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin`.
- Never `git add -A` or `git add .` — stage by name.

---

## File Structure

| File | Status | Responsibility |
| ---- | ------ | -------------- |
| `.github/workflows/chaos-matrix.yml` | create | The nightly matrix: one job per supported minor, each running the full suite and uploading its report. |
| `.github/workflows/ci.yml` | modify | Add the two cluster-free harness self-tests so they run on every pull request. |
| `chaos/README.md` | modify | A "What is and is not tested" section naming the covered axes and, explicitly, the uncovered ones. |
| `website/docs/roadmap.md` | modify | Record slice 7c shipping. |
| `CHANGELOG.md` | modify | One `[Unreleased]` entry for the nightly matrix. |

The `chaos/` scripts themselves are **not** modified by this slice. If a task finds it needs to change `chaos/run.sh`, that is a signal the plan is wrong — stop and report rather than editing the harness the nightly is meant to run unchanged.

---

### Task 1: The cluster-free self-tests run on every pull request

The two self-tests take under a second each and need no cluster, yet today nothing runs them automatically. Wire them into `ci.yml` before building the nightly, so the harness's own logic is defended by the fast job rather than only by the slow one.

**Files:**
- Modify: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `chaos/assert-selftest.sh` and `chaos/version-selftest.sh`, both already present, both exiting non-zero on failure.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Confirm both self-tests pass locally and exit non-zero when broken**

```bash
export PATH=$PATH:/usr/local/go/bin
bash chaos/assert-selftest.sh   ; echo "assert-selftest exit=$?"
bash chaos/version-selftest.sh  ; echo "version-selftest exit=$?"
```

Expected: both print their check lines and `exit=0`.

Now prove each one can fail, so the CI step is not decorative. Do this with a
throwaway copy — **never** edit the real file:

```bash
sed 's/^KUBEAGENT_CHAOS_VERSIONS=.*/KUBEAGENT_CHAOS_VERSIONS="v1.32 v1.33 v1.34 v9.99"/' \
  chaos/versions.env > /tmp/versions-broken.env
cp chaos/versions.env /tmp/versions-real.env
cp /tmp/versions-broken.env chaos/versions.env
bash chaos/version-selftest.sh; echo "broken exit=$?"     # expect non-zero
cp /tmp/versions-real.env chaos/versions.env              # restore immediately
git diff --exit-code chaos/versions.env && echo "restored clean"
```

Record both exit codes in your report.

- [ ] **Step 2: Add the step to `ci.yml`**

Add this as a new step in the existing `build-test` job, after `Build`:

```yaml
      - name: Chaos harness self-tests
        run: |
          bash chaos/assert-selftest.sh
          bash chaos/version-selftest.sh
```

These need no cluster, no docker and no Go — they are pure bash over
`chaos/assert.sh` and `chaos/versions.sh`, which is exactly why they belong in
the fast job.

- [ ] **Step 3: Verify the workflow file still parses**

```bash
python3 -c 'import yaml,sys; yaml.safe_load(open(".github/workflows/ci.yml")); print("ci.yml parses")'
```

Expected: `ci.yml parses`.

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/ci.yml
git commit -s -m "ci: run the chaos harness self-tests on every pull request

chaos/assert-selftest.sh and chaos/version-selftest.sh need no cluster and
finish in under a second each, but nothing ran them automatically: a change
that broke an assertion helper or the version resolver would have been caught
only by the 35-minute gate, or not at all.

They belong in the fast job precisely because they are cluster-free. The
nightly matrix that follows runs the harness; this runs the harness's own
logic."
```

---

### Task 2: The nightly workflow, dispatch-only, measured on a real runner

Write the matrix workflow with **no `schedule` trigger yet**. A brand-new workflow file cannot be `workflow_dispatch`ed until it exists on the default branch, so this task also adds a temporary `push` trigger scoped to this branch — Task 4 removes it and adds the cron once a real runner has been measured.

**Files:**
- Create: `.github/workflows/chaos-matrix.yml`

**Interfaces:**
- Consumes: `chaos/versions.env` (the minor list), `chaos/run.sh --k8s-version <minor> --recreate --teardown`, and the report it writes to `docs/testing/chaos-results-<minor>.md`.
- Produces: the workflow file Tasks 3 and 4 modify; the job name `chaos (<minor>)` that Task 5's documentation refers to.

- [ ] **Step 1: Write the workflow**

```yaml
name: chaos-matrix

# The chaos suite is a 35-40 minute gate that needs a real Kubernetes cluster,
# so it belongs on a schedule and on demand — never on a pull request. What
# defends a pull request is CI's fast job, which runs the harness's own
# cluster-free self-tests.
#
# One job per supported Kubernetes minor. fail-fast is off on purpose: the
# whole point of a matrix is to learn that v1.32 broke while v1.34 is fine, and
# a cancelled sibling job teaches nothing.
#
# No secrets are needed and none are granted. ANTHROPIC_API_KEY is never set,
# so the --explain scenarios skip by design — the deterministic core is what a
# nightly gate is for.
on:
  # Temporary during slice 7c: a workflow file that has never been on the
  # default branch cannot be dispatched, and this cell has to be measured on a
  # real runner before the cron is enabled. Removed in the same commit that
  # adds `schedule`.
  push:
    branches: [chaos-matrix]
  workflow_dispatch:
    inputs:
      versions:
        description: 'Space-separated minors to run, or empty for every supported minor'
        required: false
        default: ''

permissions:
  contents: read

jobs:
  # The minor list lives in exactly one place, chaos/versions.env, so the
  # harness and this workflow can never disagree about what "supported" means.
  # A hardcoded second copy here is the bug this job exists to prevent.
  versions:
    runs-on: ubuntu-latest
    outputs:
      matrix: ${{ steps.resolve.outputs.matrix }}
    steps:
      - uses: actions/checkout@v4
      - id: resolve
        run: |
          set -euo pipefail
          . chaos/versions.sh
          picked="${{ inputs.versions }}"
          [ -n "$picked" ] || picked="$(chaos_versions)"
          # Validate every requested minor through the same resolver the
          # harness uses, so a typo in a dispatch input fails here — in
          # seconds — instead of after a cluster has been built.
          for v in $picked; do chaos_image "$v" >/dev/null; done
          json="$(printf '%s\n' $picked | jq -R . | jq -sc .)"
          printf 'matrix=%s\n' "$json" >> "$GITHUB_OUTPUT"
          printf 'running: %s\n' "$picked"

  chaos:
    needs: versions
    runs-on: ubuntu-latest
    timeout-minutes: 120
    strategy:
      fail-fast: false
      matrix:
        version: ${{ fromJson(needs.versions.outputs.matrix) }}
    name: chaos (${{ matrix.version }})
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Install kind
        run: |
          set -euo pipefail
          curl -sSLo /tmp/kind https://kind.sigs.k8s.io/dl/v0.30.0/kind-linux-amd64
          chmod +x /tmp/kind
          sudo mv /tmp/kind /usr/local/bin/kind
          kind version

      # A kind node's kubelet, kube-proxy and controllers all draw inotify
      # instances from a host-wide budget, and a runner ships with kind's
      # pre-recommendation defaults. One cluster usually still boots at those
      # values, which is exactly what makes the failure intermittent rather
      # than obvious. Raise them before anything is created.
      - name: Raise inotify limits for kind
        run: |
          sudo sysctl -w fs.inotify.max_user_instances=512
          sudo sysctl -w fs.inotify.max_user_watches=524288

      - name: Preflight tools
        run: |
          for b in docker kind kubectl helm go curl python3; do
            command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
          done
          docker info >/dev/null

      - name: Run the chaos suite on ${{ matrix.version }}
        run: ./chaos/run.sh --k8s-version '${{ matrix.version }}' --recreate --teardown
```

The `--teardown` matters on a runner: the job's disk is finite and the cluster
has no reason to outlive it.

- [ ] **Step 2: Verify the file parses and the resolver logic is right**

```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/chaos-matrix.yml")); print("parses")'
```

Then prove the matrix-resolution shell does what the YAML claims, outside Actions:

```bash
cd /home/ubuntu/git/kubeagent
bash -c 'set -euo pipefail; . chaos/versions.sh; picked="$(chaos_versions)";
  for v in $picked; do chaos_image "$v" >/dev/null; done;
  printf "%s\n" $picked | jq -R . | jq -sc .'
```

Expected: `["v1.32","v1.33","v1.34"]`.

And prove a bad dispatch input fails fast rather than building a cluster:

```bash
bash -c 'set -euo pipefail; . chaos/versions.sh; for v in v1.33 v9.99; do chaos_image "$v" >/dev/null; done' \
  ; echo "exit=$?"
```

Expected: non-zero, with the supported set named on stderr.

- [ ] **Step 3: Commit and push, which triggers the first real-runner measurement**

```bash
git add .github/workflows/chaos-matrix.yml
git commit -s -m "chaos: add the nightly cross-version matrix (dispatch only)

One job per supported Kubernetes minor, fail-fast off so one bad minor cannot
hide the others. The minor list is resolved from chaos/versions.env through
the same chaos_image resolver the harness uses, so a typo in a dispatch input
fails in seconds instead of after a cluster has been built, and the workflow
can never hold a second, stale copy of what 'supported' means.

No schedule yet, and a temporary push trigger on this branch: a workflow file
that has never reached the default branch cannot be dispatched, and the spec
requires one cell measured on a real runner before the cron is enabled. If a
cell cannot finish reliably the answer is fewer scenarios, recorded as reduced
coverage — not a longer timeout and a flaky nightly.

No secrets are needed and none are granted. ANTHROPIC_API_KEY is never set, so
the --explain scenarios skip by design."
git push origin chaos-matrix
```

- [ ] **Step 4: Measure the run and record the numbers**

```bash
gh run list --workflow=chaos-matrix.yml --limit 1
gh run watch "$(gh run list --workflow=chaos-matrix.yml --limit 1 --json databaseId --jq '.[0].databaseId')"
```

Record in your report, per cell: total wall-clock, the exit status, and — from
the log — the `assertions: N run, M failed` line. **Do not** change the timeout
if a cell is slow; report the number and stop. The decision about reduced
coverage is the controller's, and it is a coverage decision, not a timeout one.

---

### Task 3: Prove the uploaded artifact carries no credential material

An artifact leaves the machine. Scenario 20 already asserts locally that no credential material reaches the recorded output; this task makes the workflow re-check it independently, immediately before upload, and fail the job rather than publish.

**Files:**
- Modify: `.github/workflows/chaos-matrix.yml`

**Interfaces:**
- Consumes: the report the previous step wrote to `docs/testing/chaos-results-<minor>.md`.
- Produces: the artifact named `chaos-report-<minor>`.

- [ ] **Step 1: Write the scan step and the upload, appended to the `chaos` job**

```yaml
      # Locally these reports are gitignored; uploaded, they leave the machine.
      # Scenario 20 already asserts that a refused read is reported in
      # kubeagent's own words and never carries the ServiceAccount's bearer
      # token or certificate material the way the raw API server message would.
      # This is the independent second check on the one artifact that actually
      # travels — belt and braces, because a report is worth publishing only if
      # publishing it is safe.
      #
      # `if: always()` because a FAILED run's report is the one worth reading,
      # and a failed run is exactly when nobody is watching the scan.
      - name: Scan the report for credential material
        if: always()
        run: |
          set -uo pipefail
          rep="docs/testing/chaos-results-${{ matrix.version }}.md"
          [ -f "$rep" ] || { echo "no report at $rep — nothing to scan"; exit 0; }
          hits=0
          scan() {   # scan <what> <extended-regexp>
            local n
            n="$(grep -acE "$2" "$rep" || true)"
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
          n="$(grep -aoE 'AKIA[A-Z0-9]{16}' "$rep" | grep -vxF 'AKIAIOSFODNN7EXAMPLE' | grep -ac . || true)"
          printf '%-34s %s\n' 'unexpected AWS keys' "$n"
          [ "$n" = 0 ] || hits=$((hits + 1))
          if [ "$hits" -ne 0 ]; then
            echo "refusing to upload: the report matched $hits credential pattern(s)" >&2
            exit 1
          fi
          echo "report is clean — safe to upload"

      - name: Upload the report
        if: always()
        uses: actions/upload-artifact@v4
        with:
          name: chaos-report-${{ matrix.version }}
          path: docs/testing/chaos-results-${{ matrix.version }}.md
          if-no-files-found: warn
```

- [ ] **Step 2: The `AKIA` pattern uses a negative lookahead — prove `grep -E` supports it, or replace it**

POSIX ERE has **no** lookahead. `grep -E` will not accept `(?!...)`. Verify:

```bash
printf 'AKIAIOSFODNN7EXAMPLE\nAKIAAAAAAAAAAAAAAAAA\n' | grep -acE 'AKIA(?!IOSFODNN7EXAMPLE)[A-Z0-9]{16}'; echo "exit=$?"
```

If that does not do what the step intends, replace the line with the two-step
form, which uses only ERE:

```bash
          scan_akia() {
            local n
            n="$(grep -aoE 'AKIA[A-Z0-9]{16}' "$rep" | grep -vxF 'AKIAIOSFODNN7EXAMPLE' | grep -ac . || true)"
            printf '%-34s %s\n' 'unexpected AWS keys' "$n"
            [ "$n" = 0 ] || hits=$((hits + 1))
          }
          scan_akia
```

Whichever form survives, it must be the one in the committed file. Record which
you used and the evidence.

- [ ] **Step 3: Prove the scan can FAIL, against a real report**

A scan that has never been seen to fail is a scan nobody should trust. Extract
the step's script into a standalone file, run it against a genuine report from
the local matrix, then against a copy with one planted token:

```bash
cp docs/testing/chaos-results-v1.33.md /tmp/report-clean.md
# A fake JWT — three base64url segments, no real signature, not a credential.
sed '5a Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJmYWtlIn0.ZmFrZQ' \
  /tmp/report-clean.md > /tmp/report-dirty.md
# then run the scan body against each, and record both exit codes
```

Expected: clean → exit 0 and `report is clean`; dirty → exit 1 naming the
pattern count. Record both in your report. Do **not** commit either scratch
file.

- [ ] **Step 4: Verify, commit, push, and confirm the artifact appears**

```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/chaos-matrix.yml")); print("parses")'
git add .github/workflows/chaos-matrix.yml
git commit -s -m "chaos: refuse to upload a report carrying credential material

Locally these reports are gitignored. Uploaded as an artifact they leave the
machine, which turns scenario 20's existing 'credential material in recorded
output: 0' assertion into a load-bearing one. This is the independent second
check on the one file that actually travels: bearer-token shapes, PEM blocks,
Authorization headers, kubeconfig credential fields, and any AWS key that is
not the documentation value scenario 10 injects on purpose.

It runs with if: always(), because a failed run's report is the one worth
reading and a failed run is exactly when nobody is watching."
git push origin chaos-matrix
```

Then confirm on the run that the artifact exists and the scan step printed
`report is clean`.

---

### Task 4: Enable the cron, drop the temporary trigger

Only after Task 2's measurement is in hand. This task is where a coverage reduction, if the measurement demanded one, gets written down.

**Files:**
- Modify: `.github/workflows/chaos-matrix.yml`

- [ ] **Step 1: Replace the trigger block**

```yaml
on:
  schedule:
    # Offset from fuzz.yml's 03:17 so the two nightlies do not contend for
    # runners.
    - cron: '41 2 * * *'
  workflow_dispatch:
    inputs:
      versions:
        description: 'Space-separated minors to run, or empty for every supported minor'
        required: false
        default: ''
```

The `push:` block and its comment go away entirely.

- [ ] **Step 2: If — and only if — Task 2's measurement showed a cell cannot finish reliably**

Do not raise `timeout-minutes`. Reduce the scenario set for the nightly and
record the reduction where a reader will find it, in the workflow itself and in
`chaos/README.md`. The harness already supports `--only NN`; a reduced cell runs
a named subset and the comment says which scenarios the nightly does **not**
cover and why. If the measurement was comfortable, skip this step and say so in
your report.

- [ ] **Step 3: Verify and commit**

```bash
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/chaos-matrix.yml")); print("parses")'
git add .github/workflows/chaos-matrix.yml
git commit -s -m "chaos: enable the nightly matrix cron

The temporary push trigger goes away with it: it existed only because a
workflow file that has never reached the default branch cannot be dispatched,
and the spec required one cell measured on a real runner before the schedule
was enabled. That measurement is recorded in the slice's report.

Offset from fuzz.yml's 03:17 so the two nightlies do not contend for runners."
```

---

### Task 5: Document what is tested and, explicitly, what is not

A matrix that names three minors invites the reader to assume everything else is covered too. Say what is not.

**Files:**
- Modify: `chaos/README.md`
- Modify: `website/docs/roadmap.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Add a "What this matrix does and does not cover" section to `chaos/README.md`**

Place it after the existing "Kubernetes versions" section. It must name, as
uncovered axes, at least: Kubernetes distributions other than kind (EKS, GKE,
AKS, OpenShift, k3s, RKE2); CPU architectures other than the runner's amd64;
CNIs other than Calico; container runtimes other than the one kind ships;
managed-control-plane behaviour; and the `--explain` scenarios, which need an
API key the nightly deliberately does not have. State plainly that three green
cells prove kubeagent held its contract on three kind-hosted minors under twenty
specific injected outages — not that kubeagent is correct in general.

- [ ] **Step 2: Record the slice in `website/docs/roadmap.md`**

Follow the wording pattern the earlier Theme H slices already use in that file.

- [ ] **Step 3: Add the `CHANGELOG.md` entry under `## [Unreleased]`**

- [ ] **Step 4: Build the site if `website/` changed**

```bash
(cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: exit 0, no `WARNING` lines naming your pages. The red "Material for
MkDocs 2.0" banner is cosmetic.

- [ ] **Step 5: Commit**

```bash
git add chaos/README.md website/docs/roadmap.md CHANGELOG.md
git commit -s -m "docs: name what the chaos matrix does not cover

Three green cells prove kubeagent held its contract on three kind-hosted
Kubernetes minors under twenty specific injected outages. They do not prove
anything about EKS, GKE, AKS, OpenShift, k3s or RKE2; about any architecture
other than amd64; about a CNI other than Calico; or about the --explain path,
which needs an API key the nightly deliberately does not have.

A matrix that names three minors invites the reader to assume the rest is
covered too, so the uncovered axes are written down beside the covered ones."
```

---

## Slice gate

Before the whole-branch review:

1. `scripts/dco-check.sh main HEAD` reports every commit signed off.
2. `git diff main...HEAD -- go.mod go.sum` is empty, and `git diff --stat main...HEAD -- '*.go'` is empty.
3. Both workflow files parse under `yaml.safe_load`.
4. A real `chaos-matrix.yml` run has gone green on all three cells, with each cell's `assertions: N run, 0 failed` line and its wall-clock recorded.
5. Each cell uploaded a `chaos-report-<minor>` artifact, and the credential scan printed `report is clean` for each.
6. The credential scan has been **seen to fail** on a planted token.
7. `chaos/README.md` names the uncovered axes.

Then the whole-branch review, on the most capable model, over the full
`main..HEAD` range for the sub-project — slices 7a, 7b and 7c together.
