# The chaos harness's portability seam — design

**Status:** approved, ready to plan
**Date:** 2026-08-05
**Slice:** post-1.0 contract work, first of two closing the cross-distribution
promise in [website/docs/compatibility.md](../../../website/docs/compatibility.md).

## The promise this closes

`website/docs/compatibility.md` says, of the nightly chaos matrix:

> **What the matrix does not cover:** one distribution (kind), one architecture
> (amd64), one CNI (Calico), on `ubuntu-latest`. kubeagent uses only stable
> `client-go` APIs and should work on any conformant cluster in the window, but
> EKS, GKE, AKS, OpenShift, k3s, and RKE2 are not gated in CI. Cross-distribution
> coverage is on the roadmap.

Today the harness cannot even be *pointed* at one of those clusters: `main()`
creates a kind cluster, installs Calico into it, and every scenario runs against
the context it just made. This slice builds the seam — a way to run the portable
subset of the suite against a cluster the operator already has. **Gating k3s or
RKE2 in CI is a deliberate follow-up slice built on top of this seam and is not
part of this one.**

That distinction matters for the docs: after this slice, EKS/GKE/AKS/OpenShift
are still **not gated in CI**. What changes is that a human can point the harness
at one and get a machine-checked answer. The compatibility page must say exactly
that and not a word more.

## The shape, settled

Three decisions were settled before this document and are not reopened:

1. **Each scenario declares what infrastructure it needs.** A scenario that
   cannot run says so; it never silently passes.
2. **The portable subset is namespaced-only.** A scenario whose whole blast
   radius is one `chaos-*` namespace it creates and deletes may run against a
   cluster the harness does not own. Anything that edits CoreDNS, installs an
   operator, creates cluster-scoped RBAC, cordons a node or stops a container
   declares itself out.
3. **A capability the harness needs *absent* is probed, not assumed.** Two
   scenarios assert the absence of a cluster capability. On a managed cluster
   that has it, they would fail rather than skip — a false alarm, which is worse
   than a skip. The harness probes and skips instead.

## The classification

Every one of the 23 scenarios, read and assigned:

| Class | Scenarios | Count |
|---|---|---|
| Portable, unconditional | 07 oom, 09 rollout, 10 credleak, 12 watch, 13 slo, 14 explain, 15 multicluster, 19 mcp, 21 controlplane, 23 pagerduty | 10 |
| Portable, capability-gated | 04 networkpolicy, 06 lb, 08 nsdelete, 18 capacity | 4 |
| Cluster-scoped writes — refused | 03 diskfull, 05 coredns, 16 operators, 17 gitops, 20 rbac, 22 dnshealth | 6 |
| Host-coupled — impossible | 01 etcd, 11 kubelet | 2 |
| Already an unconditional skip | 02 certs | 1 |

**Cluster-scoped writes**, verified against `chaos/run.sh`: 03 cordons and
uncordons a worker Node; 05 and 22 patch the `kube-system` `coredns` ConfigMap's
Corefile and restart the Deployment; 16 applies cert-manager from a URL, deletes
a `kube-system` Lease and creates and deletes the CRD
`widgets.chaos.example.com`; 17 installs Flux cluster-wide; 20 creates a
`ClusterRole` and `ClusterRoleBinding` named `chaos-rbac-scan`. Rewriting a
production cluster's Corefile is a real outage; this class is the safety heart of
the slice and is refused outright, not made conditional on a probe.

**Host-coupled**: 01 runs `docker stop` / `docker start` on the control-plane
node container; 11 runs `docker exec "$node" systemctl stop containerd`. Neither
is expressible where the nodes are not local containers.

**Capability-gated** is the interesting class. Each of the four asserts something
about the cluster's own configuration, not about the fault it injects:

- **06 lb** creates a `LoadBalancer` Service and asserts
  `no external address flagged`. A cluster with a LoadBalancer provider assigns
  an address and the assertion fails.
- **18 capacity** is documented in its own comment as *"structural rules on a
  cluster with no metrics-server"* and creates a 40-core Job that must never
  schedule. Both premises break on a real cluster.
- **04 networkpolicy** needs a CNI that actually enforces NetworkPolicy. On kind
  the harness installs Calico itself; on a foreign cluster it cannot.
- **08 nsdelete** asserts that after deleting a namespace the whole cluster still
  reads `Cluster: Healthy` and `No issues found.` — the point being that a
  stateless scanner leaves no trace of what is gone. That claim is only checkable
  on a cluster that was clean to begin with.

## The capability vocabulary

Six capabilities, a closed set. Each has one canonical reason string defined
once, so a skip's wording cannot drift between call sites.

| Capability | How it is decided | Scenarios |
|---|---|---|
| `node_exec` | **Policy.** Present only when the harness created the cluster. | 01, 11 |
| `cluster_write` | **Policy.** Present only when the harness created the cluster. | 03, 05, 16, 17, 20, 22 |
| `clean_baseline` | **Probed** by the baseline scan: present when it reports `Cluster: Healthy` and `No issues found.` | 08 |
| `no_loadbalancer` | **Probed**: a `LoadBalancer` Service in a temporary `chaos-probe` namespace gets no `status.loadBalancer.ingress` entry within 30s. | 06 |
| `no_metrics_server` | **Probed**: the `v1beta1.metrics.k8s.io` APIService is absent. | 18 |
| `netpol_enforced` | **Probed**: a DaemonSet named `calico-node`, `cilium`, `weave-net`, `kube-router` or `antrea-agent` is present in `kube-system`. | 04 |

`node_exec` and `cluster_write` are policy rather than probe by the settled
namespaced-only ruling: the question is not whether the harness *could* write
cluster-scoped objects on someone's cluster — with an admin kubeconfig it
usually could — but whether it *may*. It may not.

`netpol_enforced`'s probe is a heuristic and the design states its two failure
modes rather than hiding them: an enforcing CNI whose DaemonSet name is not on
the list produces a false **skip** (safe, and the report names it); a listed CNI
deliberately configured not to enforce produces a false **failure** in scenario
4. The first is the common case and is acceptable; the second is the probe's
named limit.

## The declaration mechanism

A guard clause as the first statement of the scenario body:

```bash
scenario_05_coredns() {
  requires cluster_write || return 0
  log "scenario 5: CoreDNS CrashLoop (bad Corefile)"
  ...
```

`requires` returns 0 when the capability is available and the scenario proceeds
unchanged. When it is not, it records the skip — in the report, in the skip log,
and on the console — and returns 1, so `|| return 0` leaves the scenario without
touching the cluster.

The declaration lives **adjacent to the code it describes** so it cannot drift
from the scenario body. Alternatives closed off: a capability table beside
`run_scenarios()` (a second place to edit, and a scenario's requirement is a fact
about its body, not about the dispatch list); a naming convention encoded in the
function name (unreadable, and unversionable); a separate manifest file (two
sources of truth for one fact).

`requires` takes **one argument**, the capability. The skip's report heading is
derived from the calling function's name by a pure function
`scenario_title scenario_05_coredns` → `5. coredns`, which is unit-testable with
no cluster and cannot drift. Passing the scenario's full report title as a second
argument was considered and rejected: it duplicates a string that already appears
in the scenario's `record` call at the bottom of the body, and nothing could
catch the two diverging.

Scenario 02, the pre-existing unconditional skip, is rewired through the same
mechanism (`assert_skip` directly — it has no capability, its reason is that
certificate expiry cannot be forced quickly or safely). It keeps its documented
comment explaining why it asserts nothing.

## Skip accounting — the property that protects the gate

Today a skipped scenario contributes zero assertions and appears nowhere in
`## Assertion summary`. The kind gate has always silently skipped scenario 02 and
reported `assertions: 134 run, 0 failed`. A portable run against EKS that skipped
twelve scenarios would print the same *shape* of line and be indistinguishable
from a full green run to anyone not counting. **That is the defect this slice
must not ship.**

`chaos/assert.sh` gains a `$SKIPLOG` companion to `$ASSERTLOG`. It is a file for
exactly the reason `$ASSERTLOG` is one, recorded in that file's own header
comment: `record()` is fed by a pipeline, a pipeline runs in a subshell, and a
counter incremented there is discarded when the block ends.

```text
assert_skip <label> <reason>
```

appends `SKIP\t<label> — <reason>` to `$SKIPLOG` and prints one line to stdout.
It never returns non-zero, like every other helper in that file.

`assert_summary` grows a third bullet and, when anything was skipped, a fenced
list naming each one:

````markdown
## Assertion summary

- assertions run: 134
- failed: 0
- scenarios skipped: 1

```text
SKIP	2. certs — control-plane certificate expiry cannot be forced quickly or safely
```
````

and the console line becomes:

```text
assertions: 134 run, 0 failed; 1 scenario skipped
```

The count is printed unconditionally, including when it is zero — a line that
appears only sometimes is a line nobody learns to read.

**The exit code is unchanged.** `assert_summary` still returns non-zero only when
an assertion failed. A skip is not a failure; it is a stated gap, and the summary
is where it is stated.

`assert.sh` stays free of any knowledge of the report file and of the cluster:
`assert_skip` writes only to `$SKIPLOG` and stdout, and `run.sh`'s `requires`
composes it with `record`. That layering is what keeps `chaos/assert-selftest.sh`
runnable with no cluster.

## The entry point

A `--context <ctx>` flag on `chaos/run.sh`. Passing it selects portable mode.

**Three flags are refused, not ignored** — each exits 2 with a named reason,
because all three are meaningless against a cluster the harness does not own and
silently ignoring them would be a trap:

| Combination | Message |
|---|---|
| `--context` + `--recreate` | refuses: kubeagent's chaos harness will not delete and rebuild a cluster it does not own |
| `--context` + `--teardown` | refuses: the harness deletes only clusters it created |
| `--context` + `--k8s-version` | refuses: the version axis selects a kind node image, which does not apply to an existing cluster |

`--only` and `--out` keep working. The report path defaults to
`docs/testing/chaos-results-portable.md`, so a partial run can never overwrite
the kind gate's `docs/testing/chaos-results.md`.

`main()` branches once, at setup: portable mode runs `portable_preflight` in
place of `preflight`, `create_cluster`, `preload_calico_images`, `install_calico`
and `wait_system_ready` — all five of which exist to build and settle a cluster
that, in this mode, already exists. `build_kubeagent` runs in both. Everything
after the setup block is shared code.

## Preflight against a cluster the harness does not own

`portable_preflight` refuses the run unless all of the following hold. Each
failure names what is wrong and exits non-zero **before anything is created**:

1. The required binaries are present. The list **shrinks**: `kind` and `docker`
   are needed only to build and manage a kind cluster. `kubectl`, `go`, `curl`
   and `python3` remain. **No new binary requirement is introduced by this
   slice.**
2. The named context exists in the kubeconfig and connects.
3. **No `chaos-*` namespace already exists.** Debris from an aborted run, or a
   concurrent run against the same cluster; either way, proceeding would produce
   an unreadable result. The message names the namespaces found and says to
   delete them.
4. A namespace create/delete round-trip succeeds, so a run does not discover
   halfway through that it lacks the one write it needs.

At the end of a portable run, `portable_sweep` deletes any `chaos-*` namespace
still standing, so a scenario that failed part way does not leave a workload on
someone's cluster. Check 3 is the backstop for a sweep that itself could not
complete.

**Two behaviours are documented rather than fixed, because they are inherent to
what the harness does:** scenario 08 deletes a namespace (its own), and scenario
10 plants a *fake* AWS access key in a ConfigMap, which will trip an operator's
secret scanner or Falco rule. Both are named in `chaos/README.md` under portable
mode. The chaos harness is deliberately **not** read-only — that is precisely why
pointing it at a real cluster needs these guard rails. (kubeagent itself remains
read-only toward the cluster; that is a separate promise about the tool, not
about its test harness.)

## The baseline block

`main()`'s baseline scan asserts `Cluster: Healthy` and `No issues found.`. On an
operator's real cluster that is very likely false through no fault of kubeagent,
and would be the first thing a portable run reported — a false alarm before a
single scenario had run.

In portable mode the baseline block asserts what is actually true of any cluster:
the scan exits 0, and it rendered a report (a `Cluster:` verdict line is
present). The verdict itself is **recorded** in the report either way. The kind
path is unchanged byte-for-byte.

That block is also where `clean_baseline` is decided: when the baseline does read
`Cluster: Healthy` and `No issues found.`, the capability is present and scenario
08 runs. On kind it always does.

## Cluster identity is a credential

Kubeconfig context names are credentials under this project's rules, and on a
real cluster the context name is routinely an EKS ARN or a GKE
project/region/name path. The report today is safe only because
`kind-kubeagent-chaos` happens not to be sensitive.

Three consequences:

1. **The harness never writes the context name to `$OUT`.** It may appear on
   stderr — the operator's own channel — and nowhere else.

2. **The portable report's header describes the target without naming it.** In
   place of the kind header's
   `- Cluster: Kind v0.30.0, Calico CNI, 1 control-plane + 2 workers`, portable
   mode prints the server version and the deduplicated `nodeInfo` fields —
   OS image, container runtime, kubelet version — which identify the platform
   precisely (`Amazon Linux 2`, `containerd://1.7.x`) and are not names. Node
   names are not printed.

3. **Two scenarios write the context name into the report today, and both are
   fixed as part of the slice.** Each is harmless only because
   `kind-kubeagent-chaos` happens not to be sensitive; in portable mode each
   would be the operator's real context name. Both are genuine defects
   independent of portable mode, and both fixes hold the assertion count and the
   assertion meanings unchanged.

   **Scenario 19 (three places).** It prints
   `tools/call (id 3) coverage.context: <name>` into the gate-checks block;
   asserts
   `expect_eq "the server's context round-trips into the response" "$got_context" "$CTX"`,
   and `expect_eq` echoes its actual value on **PASS**; and interpolates `$CTX`
   into the `record` verdict string. Fix: compare a derived match indicator
   (`yes`/`no`) rather than the two names, print the indicator rather than the
   name, and reword the verdict to say *the context the server was started with*
   without naming it.

   **Scenario 15 (structural).** The multi-cluster daemon labels each cluster's
   metric series by its context name, and the scenario dumps
   `kubeagent_cluster_up{cluster="…"}` lines, the `/issues` cluster roster and a
   per-cluster issue count straight into the report — so the context name reaches
   `$OUT` as data, not just as prose. It also interpolates `$CTX` into its
   `record` verdict twice. Fix at the source: the scenario already writes a
   temporary kubeconfig and adds an `alias-b` and a `dead` context to it, so it
   adds a third harness-named alias for the real cluster and passes
   `--context alias-a --context alias-b --context dead` — never handing the
   daemon the operator's context name at all. Every label in the metrics, in
   `/issues` and in the report is then harness-chosen. The scenario still targets
   the same cluster through the same credentials; only the label changes.

`.github/workflows/chaos-matrix.yml` already scans the report for credential
material before uploading it and refuses the upload on a hit. That gate is
unchanged and is not this slice's only defence — the harness must not write the
material in the first place.

## What does not move

- **The assertion count stays 134.** Every capability is present on kind, so no
  scenario's assertions change on the gate path. `134` in `CLAUDE.md`,
  `chaos/README.md`, `website/docs/compatibility.md`, `website/docs/roadmap.md`
  and `chaos/run.sh` therefore does not move. New coverage lands in
  `chaos/assert-selftest.sh`, whose own count is not a published number.
- `scenario_01_etcd` stays **last** in `run_scenarios()`. It skips in portable
  mode; the ordering invariant is unchanged.
- No helper returns non-zero. `assert_skip` follows the rule the rest of
  `assert.sh` follows.
- `go.mod` and `go.sum` do not change — no Go code is touched at all.
- The six versioned JSON documents do not move. No `schemaVersion` bump, no
  schema regeneration.
- `internal/report/testdata/golden-scan.txt` stays byte-identical. The demo GIF
  and `website/docs/quickstart.md` are not regenerated.
- The Helm chart, `internal/rbacprofile`'s Feature table and every generated RBAC
  manifest are untouched.

## Files

| File | Change |
|---|---|
| `chaos/assert.sh` | `$SKIPLOG` in `assert_init` and its trap; `assert_skip`; `assert_summary` reports and lists skips |
| `chaos/assert-selftest.sh` | coverage for `assert_skip`, the new summary output, and `scenario_title` |
| `chaos/run.sh` | `--context` and its three refusals; `PORTABLE`; the capability table, `requires`, `scenario_title`; `portable_preflight`, `portable_sweep`; the `main()` setup branch, header and baseline branch; `requires` guards in nine scenarios; scenario 02 rewired; the context-name fixes in scenarios 15 and 19 |
| `chaos/README.md` | portable mode: what it runs, what it refuses, the preflight, the two documented behaviours |
| `website/docs/compatibility.md` | amend the "not gated in CI" paragraph — accurately, without claiming CI coverage this slice does not add |
| `website/docs/roadmap.md` | the cross-distribution item: seam built, CI gating still ahead |
| `CHANGELOG.md` | under `[Unreleased]` |

## Testing

`chaos/assert-selftest.sh` is the established precedent for testing harness logic
with no cluster, and every new pure shell function gets the same treatment, TDD —
failing test first:

- `scenario_title` over each of the 23 real function names, including
  `scenario_14` which has no trailing word.
- `assert_skip` appends exactly one `$SKIPLOG` line and returns 0.
- `assert_summary` with 0 skips, with 1, and with several: the bullet, the fenced
  list, the console line, and — the property that matters — that a run with
  failures **and** skips still exits non-zero, and a run with skips and no
  failures exits zero.
- `requires` returns 0 for an available capability and 1 for an unavailable one,
  and an unknown capability name is an error rather than a silent skip.

Plus `bash -n chaos/run.sh` and `shellcheck` where available.

## The gate, and what stays unverified

The gate is both of:

1. **`./chaos/run.sh --recreate`** — the full kind suite, ~40 minutes, must exit
   0 with `assertions: 134 run, 0 failed` and now `1 scenario skipped`.
2. **`./chaos/run.sh --context kind-kubeagent-chaos`** — the portable path
   against the same cluster.

Run 2 is a genuine test of the seam rather than a tautology: it takes the
non-kind code path end to end, `portable_preflight` runs for real, and all four
capability probes execute against a live cluster and must answer correctly —
`clean_baseline` present, `no_loadbalancer` present, `no_metrics_server` present,
`netpol_enforced` present. Expected shape: **14 scenarios run, 9 skipped** (02,
plus 01 and 11 for `node_exec`, plus the six `cluster_write` scenarios). The
skipped set is exactly the set whose reasons are policy rather than probe, which
is the correct answer for a cluster the harness is pretending not to own.

**What that does not verify, stated plainly.** This machine has kind, helm and
docker and no k3d, k3s, minikube, crc, `oc`, or cloud credentials. Until someone
runs the portable path on real infrastructure, these remain unverified:

- behaviour with a **LoadBalancer provider** present — the `no_loadbalancer`
  probe's negative branch has no live test here;
- behaviour with **metrics-server** present — likewise for `no_metrics_server`;
- behaviour on a **non-Calico CNI**, including whether `netpol_enforced`'s
  DaemonSet list covers what people actually run;
- behaviour on a cluster whose **baseline is not clean** — the `clean_baseline`
  negative branch;
- **OpenShift**, where SCCs may reject the chaos pods outright and the failure
  would be a scheduling refusal rather than any of the classes above.

On a managed cluster with a LoadBalancer provider, metrics-server, an enforcing
CNI and a non-clean baseline, the expected shape is **11 scenarios run, 12
skipped**. That number is a prediction from the classification, not a measured
result, and the docs must present it as such.

The honest summary for `chaos/README.md`: this slice makes the harness *runnable*
against a foreign cluster and makes every gap it leaves *countable*. It does not
claim any distribution is tested until someone has run it against one and the
result is recorded.
