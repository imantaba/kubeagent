# Cross-version chaos matrix — design

**Theme H, sub-project 7.** The last piece before the v1.0 production contract.

## The problem

`chaos/run.sh` is the project's only real-cluster gate, and it tests exactly one
shape of cluster: Kind `v1.34.0`, Calico CNI, one control-plane and two workers.
Every claim kubeagent makes about any other Kubernetes version is untested.

Two distinct gaps, and they compound:

1. **No version coverage.** kubeagent reads API fields, condition reasons,
   `/readyz` check names and certificate paths whose shape moves between minors.
   Nothing proves a scan against v1.32 produces the same findings it produces
   against v1.34 — or produces anything at all.
2. **No machine-checked pass/fail.** Each scenario records its output plus a
   prose `expect:` paragraph and a human reads the report before a release. That
   works for a gate someone runs deliberately; it cannot work nightly across
   three versions. A scenario whose detector silently stopped firing still exits
   0 today, and a report nobody reads is not a gate.

The second gap is the harder one, and it is why this sub-project is more than a
matrix strategy in a workflow file.

## What this builds

A nightly GitHub Actions matrix that runs the full chaos suite against the three
supported Kubernetes minors and **fails on its own**, naming the scenario and the
assertion that broke.

### Scope

**In:** the Kubernetes-version axis, driven by pinned `kindest/node` images.

**Out, deliberately:** CNI (Cilium, kindnet), distro (k3s, minikube) and
cloud-managed (EKS/GKE/AKS) axes. Each is a real gap; each is also a different
kind of work — a CNI axis changes what scenario 4 can prove, and a managed axis
needs credentials and money. They stay **documented as untested** rather than
half-tested. Honest coverage beats broad coverage.

**Out:** `--explain`. The nightly has no `ANTHROPIC_API_KEY`, so those scenarios
skip exactly as they already do locally. That is a property of the design, not a
limitation to fix: the deterministic core is what a nightly gate is for.

## Architecture

Four pieces, built in three slices.

### 1. `chaos/versions.env` — the version set

Three `kindest/node` images, one per supported minor, each **pinned by
`sha256` digest**:

```sh
# The Kubernetes minors kubeagent supports. Digest-pinned: a nightly failure is
# then always a kubeagent change, never a silently retagged upstream image.
KUBEAGENT_CHAOS_VERSIONS="v1.32 v1.33 v1.34"
KUBEAGENT_CHAOS_IMAGE_v1_32="kindest/node:v1.32.x@sha256:<digest>"
KUBEAGENT_CHAOS_IMAGE_v1_33="kindest/node:v1.33.x@sha256:<digest>"
KUBEAGENT_CHAOS_IMAGE_v1_34="kindest/node:v1.34.0@sha256:<digest>"
```

One file, read by both the local harness and the workflow, so the two can never
disagree about what "supported" means. Adding a minor is a deliberate one-line
commit, reviewed like any other change — not a silent drift when upstream moves
a tag.

### 2. `chaos/run.sh --k8s-version <minor>` — the axis

Selects the image, and derives everything cluster-shaped from it:

| | today | with the flag |
| --- | --- | --- |
| cluster | `kubeagent-chaos` | `kubeagent-chaos-v1-33` |
| context | `kind-kubeagent-chaos` | `kind-kubeagent-chaos-v1-33` |
| report | `docs/testing/chaos-results.md` | `docs/testing/chaos-results-v1.33.md` |

Two cells must never collide, on a runner or on a laptop. Omitting the flag
keeps today's names and today's default image, so an operator's muscle memory
and the release skill's documented command both keep working unchanged.

### 3. Assertion helpers — what makes a nightly meaningful

Today a scenario computes its evidence, prints it into the report, and describes
in prose what the numbers should say. The numbers are already there; nothing
checks them.

Add four helpers that each scenario calls with values it **already computes**:

```sh
expect_eq       <label> <actual> <want>
expect_ge       <label> <actual> <min>
expect_contains <label> <haystack> <needle>
expect_absent   <label> <haystack> <needle>
```

Each writes a `PASS`/`FAIL` line into the report beside the value, and bumps a
global failure counter. `main` exits non-zero when the counter is non-zero, so
`./chaos/run.sh` becomes a gate that can fail rather than a report that can be
ignored. The existing prose stays exactly where it is — when a cell goes red, the
rationale explaining *why the scenario exists* is what a human needs first.

**Assertions are written at kubeagent's contract level, never Kubernetes'.**

- Good: "the Pending finding names `chaos-diskfull/stuck`", "scan exit code is
  0", "blind spots naming a missing grant is at least 1", "`chaos-np/blocked`
  appears 0 times in the recovery scan".
- Bad: "the message reads `0/3 nodes are available: 1 Insufficient cpu…`".

The second form tests Kubernetes, not kubeagent. It goes red on an upstream
wording change that broke nothing, and it trains everyone to ignore a red
nightly — the failure mode that kills gates. Where a scenario today records a
raw API message, the assertion must be on what kubeagent *did with it*, not on
its text.

**No per-version expected-value table, and no per-scenario version skips as a
primary mechanism.** An assertion that cannot be phrased to hold on all three
minors is a signal that it is asserting the wrong thing, and the fix is the
assertion. A genuine absent-API skip stays available as a rare escape hatch, and
each use must carry a comment naming the API and the minor that lacks it.

### 4. `.github/workflows/chaos-matrix.yml` — the nightly

`schedule` (cron) plus `workflow_dispatch`, modelled on the existing
`fuzz.yml`. `strategy.matrix.version` over the three minors, `fail-fast: false`
so one bad minor does not hide the others, each job uploading its report as an
artifact. No secrets are needed and none are granted.

## Slices

Ordered so each is independently valuable and independently reviewable.

**Slice 7a — machine-checked assertions.** The helpers, plus all 20 scenarios
converted, still on today's single version. Ships the biggest gap by itself: a
detector that stops firing starts failing the gate. Ends with a measurement —
wall-clock for a full run — that the workflow slice needs.

**Slice 7b — the version axis.** `versions.env`, `--k8s-version`, per-version
cluster/context/report naming, and the first green run against a minor that is
not v1.34. This is where version-specific breakage actually surfaces; expect the
slice to find real defects in kubeagent, not only in the harness.

**Slice 7c — the nightly workflow.** The matrix, artifacts, and the
documentation naming which versions are tested and which axes are not.

## Risks

**Runner capacity.** A 2-core GitHub runner running Kind with three nodes,
Calico, and 20 scenarios is the main threat to this design. The harness already
preloads Calico images (the historical flake), but nothing here is proven on a
runner. Slice 7a therefore ends by measuring a full local run, and slice 7c
starts by measuring one cell on a real runner via `workflow_dispatch` before the
cron is enabled. **If a cell cannot finish reliably, the honest response is fewer
scenarios per cell — recorded as reduced coverage — not a longer timeout and a
flaky nightly.**

**Assertion quality.** Twenty scenarios converted quickly is twenty chances to
write an assertion that passes vacuously. Every `expect_*` call must be seen to
fail: the slice's review requires evidence that each new assertion was
demonstrated failing at least once (by perturbing the value it reads), the same
discipline TDD applies to a unit test.

**Scenario 1 runs last for a reason.** It stops the control plane, and
etcd/apiserver flap afterwards. That ordering is load-bearing and must survive
both the assertion conversion and the version parameterization.

## Constraints inherited

- Every commit signed off (`git commit -s`); `main` enforces DCO. No AI
  attribution anywhere.
- **No new dependency.** `go.mod` and `go.sum` must not change. This sub-project
  is expected to touch no Go production code at all — a change to `internal/`
  means a defect was found, and it gets its own commit with its own unit test.
- Read-only toward the cluster except the harness's own `--fix` scenario, which
  already exists and stays as it is.
- No secrets, private IPs or internal hostnames anywhere, including the recorded
  reports and the workflow file. RFC 5737 IPs, RFC 2606 domains. The reports
  under `docs/testing/` are gitignored, and a CI artifact is not: slice 7c must
  confirm an uploaded report carries no bearer token or certificate material —
  scenario 20 already asserts this locally and that assertion becomes
  load-bearing once the report leaves the machine.
- `go test` runs with `-p 2`, never `-short`; CI's `go test -race ./...` stays
  green.
