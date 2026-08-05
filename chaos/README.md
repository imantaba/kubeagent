# kubeagent chaos-test harness

A repeatable, **pre-release** chaos test. It spins up a disposable **Kind**
cluster, injects the most common production outages, runs `kubeagent scan`
against each, and asserts — with the `expect_eq` / `expect_ge` /
`expect_contains` / `expect_absent` helpers in `chaos/assert.sh` — that
kubeagent's own contract held for each one. It is a **gate**, not just a
report: `./chaos/run.sh` exits non-zero the moment any assertion fails, so a
regression stops a release instead of waiting for someone to notice it while
reading the whole thing.

By default the harness targets **only** the Kind context it creates
(`kind-kubeagent-chaos`, or `kind-kubeagent-chaos-v1-33` and the like when
`--k8s-version` is given) — it does not read your current kubecontext, so a
plain run cannot touch another cluster. `--context <ctx>` deliberately points
it at a cluster you already have; that mode is described under
[Portable mode](#portable-mode) and carries its own guard rails.

## Prerequisites

- **Docker** (the Kind nodes run as containers)
- **kind** ≥ v0.30 — install:
  `curl -sSLo kind https://kind.sigs.k8s.io/dl/v0.30.0/kind-linux-amd64 && chmod +x kind && sudo mv kind /usr/local/bin/`
- **kubectl**, **helm**, **go**, **python3**
- **inotify headroom.** Every kubelet, kube-proxy and controller inside a Kind
  node draws inotify instances from a **host-wide** budget — containers do not
  get their own. Below kind's recommended limits one cluster usually still
  boots, so the machine looks fine, and the *second* one starves: kube-proxy
  dies with `too many open files`, the kubelet never reaches healthy, and
  kubeadm gives up four minutes later with a Go stack trace naming none of it.
  Raise them once:

  ```bash
  sudo sysctl -w fs.inotify.max_user_instances=512
  sudo sysctl -w fs.inotify.max_user_watches=524288
  ```

  `run.sh`'s preflight checks both values and prints exactly those two commands
  when they are low. It only *refuses* to start when they are low **and** another
  Kind cluster is already running, because that pair is what actually breaks —
  running one cluster at a time needs no host change.

## Run

```bash
./chaos/run.sh                 # create cluster, run all scenarios, leave cluster up
./chaos/run.sh --recreate      # delete + recreate the cluster first (clean slate)
./chaos/run.sh --teardown      # delete the cluster when finished
./chaos/run.sh --only 7        # run a single scenario (1..23) for debugging
./chaos/run.sh --out path.md   # write the report somewhere specific
./chaos/run.sh --k8s-version v1.33   # pin the Kubernetes minor (see below)
```

The report is written to `docs/testing/chaos-results.md` by default (the
`docs/testing/` directory is git-ignored, so reports stay local).

### Kubernetes versions

Omitting `--k8s-version` lets kind pick its own node image, which is what the
release gate has always run — the command in the release skill keeps working
byte-for-byte. Passing the flag pins a specific minor instead:

```bash
./chaos/run.sh --k8s-version v1.33 --recreate --teardown
```

The supported minors and the node image for each live in
[`versions.env`](versions.env), read only by `chaos/versions.sh`. Everything that
needs to know what "supported" means resolves it from there rather than keeping
its own copy — the harness today, and CI when the nightly matrix lands. An
unsupported or malformed value is refused before anything touches docker, with
the supported set named on stderr and nothing on stdout — a caller that ignored
the status would otherwise hand `kind create cluster` an empty `--image` and
silently boot whatever kind defaults to.

The images are pinned **by digest**, not by tag. A tag can be retagged upstream,
and a nightly that goes red because someone else moved a tag teaches everyone to
ignore the nightly. With a digest, a red cell is always a kubeagent change.

Everything cluster-shaped is derived from the minor, so two of them coexist on
one machine without colliding — including the CoreDNS scratch file, where a
collision is the nastiest, because it silently restores the wrong Corefile:

| Derived from | default | `--k8s-version v1.33` |
| ------------ | ------- | --------------------- |
| cluster | `kubeagent-chaos` | `kubeagent-chaos-v1-33` |
| context | `kind-kubeagent-chaos` | `kind-kubeagent-chaos-v1-33` |
| report | `docs/testing/chaos-results.md` | `docs/testing/chaos-results-v1.33.md` |
| CoreDNS backup | `/tmp/kubeagent-chaos-coredns.yaml` | `/tmp/kubeagent-chaos-v1-33-coredns.yaml` |
| node image | kind's default | pinned in `versions.env` |

Coexisting is what the inotify prerequisite above pays for; one minor at a time
needs nothing.

To add or move a minor, resolve its digest from the kind release that ships it —
kind's release notes are the authority, not whatever the tag happens to resolve
to today — and edit `versions.env` in one reviewed commit:

```bash
gh release view v0.30.0 --repo kubernetes-sigs/kind |
  grep -oE 'kindest/node:v[0-9.]+@sha256:[0-9a-f]{64}'
```

`bash chaos/version-selftest.sh` then checks the result with no cluster and no
docker, in under a second: every listed minor resolves to a digest-pinned
`kindest/node` reference naming that minor, and an unsupported, malformed, or
merely prefix-matching value (`v1.3` against `v1.33`) is refused with nothing on
stdout.

### What this matrix does and does not cover

The nightly workflow (`.github/workflows/chaos-matrix.yml`) runs the full
suite — the baseline plus all 23 scenarios below — once per supported
Kubernetes minor (currently v1.32, v1.33, v1.34), each on its own disposable
**kind** cluster on a GitHub-hosted `ubuntu-latest` runner, with Calico as the
CNI and kind's own containerd as the node runtime. Three green cells prove
that kubeagent held its contract — the machine-checked assertions described
under Assertions, below — on three kind-hosted minors under twenty-three
specific injected outages. That is **not** a claim that kubeagent is correct in
general, and the axes below are the ones a three-minor matrix could easily be
mistaken for covering:

- **Kubernetes distribution.** Only kind. EKS, GKE, AKS, OpenShift, k3s and
  RKE2 are untested, and so is any managed control plane — a kind
  control-plane container has none of a cloud provider's admission chain,
  IAM-mapped auth, or managed upgrade behaviour.
- **CPU architecture.** Only amd64. The runner is GitHub's `ubuntu-latest`
  and the workflow installs the `kind-linux-amd64` binary; arm64 and every
  other architecture are untested.
- **CNI.** Only Calico (`chaos/kind-config.yaml` disables kind's default CNI
  so Calico can enforce NetworkPolicy). Cilium, kindnet's own default, and
  every other CNI are untested — scenario 4's NetworkPolicy assertion in
  particular exercises Calico's enforcement, not a behaviour every CNI
  guarantees identically.
- **Container runtime.** Only containerd, because that is what a kind node
  ships (scenario 11 stops it by name to test the boundary). CRI-O and other
  runtimes are untested.
- **`--explain` against a real model.** `ANTHROPIC_API_KEY` is never set in
  the nightly workflow, so the harness's normal gate on the flag
  (`explain_flag`, above) means every ordinary scenario's scan runs without
  `--explain` — the nightly exercises kubeagent's deterministic core, not the
  model path. (Read-only cluster access and making no LLM call are two
  separate properties; the nightly holds both, but they are not the same
  claim.) Scenario 14 is the one nuance worth naming precisely: it does pass
  `--explain` to `kubeagent watch`, but against a local Python stub
  (`chaos/explain-stub.py`) that never leaves the runner and needs no key —
  it proves the budget, the throttle, the notification shape, and the egress
  redaction, not the real Anthropic backend, which stays covered by unit
  tests only.

Each cell runs 134 assertions. On a GitHub-hosted runner a cell takes roughly
17 minutes; locally it's 35-40. All three supported
minors have gone green on real runners.

### `--explain`

`run.sh` adds `--explain` to every scan **only when `ANTHROPIC_API_KEY` is set in
your environment** — so the harness is deterministic by default, and includes the
Claude-summarized output when you opt in:

```bash
ANTHROPIC_API_KEY=sk-ant-... ./chaos/run.sh --recreate
```

The key is read from the environment only; it is never written to the report.

## Portable mode

```bash
./chaos/run.sh --context <ctx>
```

Runs the portable subset against a cluster the harness did **not** create. The
report defaults to `docs/testing/chaos-results-portable.md`.

`kind` and `docker` are not required in this mode — the binary list is
`kubectl`, `go`, `curl` and `python3`.

### What it does to the cluster

Every scenario that runs writes to a `chaos-*` namespace it creates, breaks
something inside it, and deletes the namespace afterwards — with one exception:
scenario 21 (control-plane readiness) injects nothing and creates no namespace.
Its entire body is `scan --control-plane-health`, a GET against the live
apiserver's `/readyz`, so it has no blast radius at all. Whatever a partial run
leaves behind is swept at the end. Two consequences are worth knowing before you
point this at a shared cluster:

- **Scenario 8 deletes a namespace and asserts the cluster reads healthy again.**
  It deletes only the `chaos-doomed` namespace it created moments earlier — but
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

Both are enforced the same way, literally: every scenario's write to the
report passes through one filter, and that filter redacts node names and the
context name together, in a single pass, rather than as two independent
steps run one after the other. Two independent steps can each consume their
own needle out of the text before the other's exact match ever sees it —
whichever direction a node name or the context name happens to embed the
other, running the filters in a fixed order gets one of those directions
wrong. A single pass avoids that: every node name and the context name are
matched as literals, never as a regex (a real context name can carry almost
anything a kubeconfig accepts), longest name first, in one left-to-right
scan that never revisits text it has already replaced. A section the filter
cannot redact is withheld — replaced by a marker, never shown unredacted —
and that failure never stops the run.

### The baseline

A cluster you already run is very likely not clean, through no fault of
kubeagent. In portable mode the baseline asserts only that the scan exited 0 and
rendered a verdict; the verdict itself is recorded for a human to read rather
than asserted. A dirty baseline also withdraws `clean_baseline`, which skips
scenario 8.

## Assertions

Every scenario captures a value it already computed from the scan — an exit
code, a substring of the output, a count — and checks it with one of four
helpers from `chaos/assert.sh`: `expect_eq`, `expect_ge`, `expect_contains`,
`expect_absent`. Each one prints a `PASS:` or `FAIL:` line; every scenario
runs its `expect_*` calls inside a `{ ...; } | record ...` block, so that line
goes into the report, not the console — inside every scenario's `## <name>`
section, the raw scan output is followed by a `--- assertions ---` block
naming exactly what was checked and whether it held. A `FAIL` additionally
writes a differently-worded line straight to the console, on stderr, outside
that pipe: `ASSERTION FAILED: <label> <detail>` — a `PASS` writes nothing to
the console. That is what an operator watching a 35–40 minute run actually
sees scroll by.

`main` finishes with `assert_summary`, which appends an `## Assertion summary`
to the end of the report. The run ends with `assertions: N run, M failed; K
scenario(s) skipped` on the console and an `## Assertion summary` in the report
carrying the same three counts. A failure list is fenced under it when there
are failures; a skip list is fenced under it when there are skips.

**A skip is never a failure and never moves the exit code** — it is a declared
gap. It is reported unconditionally, including when the count is zero, so a run
that skipped nine scenarios can never be read as a full green one. The exit
code is non-zero if and only if an assertion failed.

The baseline and all 23 scenarios are asserted except one: scenario 2 (expired
certificates) runs no scan and computes nothing, so it carries no assertion by
design — the TLS branch it would otherwise cover is unit-tested in
`internal/connectivity` instead. It is now counted in `scenarios skipped`
rather than silently absent.

These assertions are written at kubeagent's contract level — a finding
kubeagent reported, a counter kubeagent computed, kubeagent's own exit code —
never at the Kubernetes API server's wording, which can change between minor
versions without kubeagent being wrong. Twenty-three specific injected outages
passing proves kubeagent kept its side of the contract on those twenty-three; it
is not a general correctness proof.

When a `FAIL:` line shows up, the report is what you read to understand it,
not what you read to detect it: each scenario's section opens with a
`_Verdict: ...` line — the rationale for why the scenario exists and what the
checked value is supposed to mean — before the `--- assertions ---` block that
names what actually happened. The cluster is usually still up (`run.sh` leaves
it up unless `--teardown` is passed), so `./chaos/run.sh --only NN --out
/tmp/scratch.md` re-runs just the failing scenario against it.

`bash chaos/assert-selftest.sh` exercises `expect_eq`, `expect_ge`,
`expect_contains`, and `expect_absent` on their own — no Kind cluster, no
scenarios — proving each helper both passes and fails correctly, in under a
second.

## Scenarios

| # | Outage | How it's injected | Expected kubeagent signal |
|---|--------|-------------------|---------------------------|
| 1 | etcd quorum loss | `docker stop` the control-plane node | connectivity diagnosis (connection refused) — a **boundary** |
| 2 | Expired certificates | **skipped** (can't be forced on Kind) | n/a — TLS branch is unit-tested |
| 3 | Disk full (control plane) | `kubectl cordon` + an unschedulable pod | P1 node `SchedulingDisabled` + `Unschedulable` |
| 4 | NetworkPolicy block | an app whose readiness is one in-cluster HTTP call away, then a Calico deny-all — and then the policy is deleted again | degraded workload with a `ProbeFailure` finding while the policy is in force, healthy before it and healthy again after it, so the policy is shown to be the **cause**. No NetworkPolicy hint: `netpolicy.Annotate` suppresses it when a detector already accounts for the workload |
| 5 | Broken DNS | bad Corefile → CoreDNS CrashLoop | P1 cluster `Degraded` + CrashLoopBackOff |
| 6 | Cloud LB failure | `type: LoadBalancer` Service (no provider) | Service issues — no external address |
| 7 | OOMKilled | memory-hog Deployment, 64Mi limit | OOMKilled + container requests/limits |
| 8 | Namespace deletion | `kubectl delete ns` | "No issues found" — a **boundary** (stateless) |
| 9 | Faulty rollout | `kubectl set image` to a bad tag | ImagePullBackOff |
| 10 | Credential leak | ConfigMap with a fake `AKIA…` value | `--lint-secrets` warning (location + pattern only) |
| 11 | Kubelet health probe | `systemctl stop containerd` on a worker (kubelet stays up) | node NotReady (base scan); `--kubelet-health` probes every kubelet via `nodes/proxy` and does **not** false-positive — kubelet `/healthz` stays `ok` (only ping/log/syncloop, not the runtime) — a **boundary** |
| 12 | Stateful watch daemon | run `kubeagent watch` on a loopback metrics address with alerting pointed at a local receiver, then inject and repair the bad-image outage | one `NEW` transition line, one `RESOLVED` line with the firing duration, the incident on `/issues` (active while firing, under `resolved` afterwards), and exactly one resolved alert delivered — the firing alert survives the whole failure-mode walk |
| 13 | SLO burn rate | run `kubeagent watch --slo-target 99.9` on a loopback metrics address, then break the only workload | SLO burn-rate series track real breakage; a cold daemon does not page (coverage gate) |
| 14 | On-incident explanations | run `kubeagent watch --explain --explain-budget 1` against a local stub endpoint, then break two objects at once | exactly one model call and one `reason=explanation` notification — the budget throttles the rest — with the explanation on `/explanations`, the plain firing alerts unaffected, no pod/node detail in any prompt, and no endpoint path in any log line |
| 15 | Multi-cluster hub | one `kubeagent watch` over three targets: this cluster twice under different context names, plus a context pointing at a closed port | `/readyz` still 200 with one target dead, `kubeagent_cluster_up` 1/1/0, `kubeagent_clusters_total 3`, the same broken Deployment listed once per healthy cluster label, and the dead target on the `/issues` roster with an error — a **degradation** test, not a divergent-state test |
| 16 | Operator/CRD adapters | install real cert-manager, create a `Certificate` referencing an `Issuer` that does not exist, and apply an unrelated CRD kubeagent has no adapter for | `--operators` names the cert-manager `Certificate` with `Ready=False`, the unadapted CRD is **absent** from the report (the discovery gate proven in both directions), no CR `spec` content appears in any line, and the cluster verdict stays driven by core workloads |
| 17 | GitOps drift | install real Flux (pinned v2.4.0), create a `GitRepository` pointing at an unresolvable host plus a failing and a suspended `Kustomization` | `--drift` names the failing `Kustomization` as stale (past `--drift-age`) and the other as `suspended`; the `GitRepository`'s repo URL and the fake token embedded in it appear **zero** times anywhere in the report. Flux is removed again afterwards |
| 18 | Capacity hints, no metrics-server | a `Deployment` with no resource requests, one with a memory limit but no request, and a `Job` requesting 40 CPU cores (unschedulable on this cluster) | `--capacity` names all three structural right-sizing rules and reports the metrics-server-unavailable, structural-rules-only path; no cost/peak/waste vocabulary anywhere. CAPACITY is purely advisory and never itself moves the verdict — the cluster still reads **Degraded**, because the 40-core Job's Pod is genuinely unschedulable and the existing Pending/Unschedulable detector reports that independently |
| 19 | MCP server over stdio | drive `kubeagent mcp` as the real binary over real stdio (not the fake clientset): `initialize`, `tools/list`, then `tools/call kubeagent_triage` against a crash-looping pod | `tools/list` names exactly `kubeagent_advisory kubeagent_inspect kubeagent_triage` — no tool name contains a write verb (`fix`/`apply`/`delete`/`patch`/`create`); the triage call returns a `degraded` verdict with at least one finding and `coverage.context` echoing back the `--context` the server was started with |
| 20 | Least-privilege RBAC | generate a `scan`-profile `ClusterRole` with `kubeagent rbac print`, bind it to a fresh ServiceAccount, then run `kubeagent rbac check` and `scan --certs --logs --disk-usage --control-plane-health --dns-health` as that identity alone | `rbac check` reports `core` allowed and exactly `certs diskusage logs` blocked, named from kubeagent's own table, never the API server's; the scan still exits `0`; the report names `secrets`/`pods/log`/`nodes/proxy`/`pods/proxy` as unread rather than showing four empty sections, and does *not* name `/readyz`, which a stock cluster grants to every authenticated identity — a read that succeeded is never reported as a blind spot |
| 21 | Control-plane readiness probe | nothing injected — the probe runs against a healthy apiserver | `--control-plane-health` classifies `/readyz` as `ok` in the JSON report and renders **no** CONTROL PLANE section in the text one; without the flag the JSON carries no `controlPlane` key at all. The unhealthy branch is deliberately not injected: the only apiserver readyz check reachable from outside is etcd, and breaking etcd takes every read the report is built from with it — that is scenario 1 |
| 22 | DNS up but not resolving | a Corefile that keeps CoreDNS Ready and serving `/metrics` while answering every query `SERVFAIL` (the `template` plugin), plus a Job driving enough queries to clear the 100-response floor | `--dns-health` reports `degraded`, names it (`cluster DNS is failing to resolve`) and quantifies the SERVFAIL+REFUSED ratio; exit code stays `0` because the section is advisory; without the flag the JSON carries no `dns` key. Scenario 5 covers the other DNS outage — CoreDNS crash-looping — and cannot reach this one, because a CoreDNS that is down serves no `/metrics` to read |
| 23 | PagerDuty receiver | run `kubeagent watch --alert-format pagerduty` with the routing key in the environment and the endpoint pointed at a local stand-in for `events.pagerduty.com`, then inject and repair the bad-image outage | an Events API v2 `trigger` while the Deployment is broken and a `resolve` after the repair, both on the same identity-derived `dedup_key` — the property that makes a daemon restart re-trigger onto the open incident instead of opening a second one — and the routing key, which travels in the request body only, in **no** line of the daemon's log |

### Validating `--fix` (remediation)

Scenario 9 (faulty rollout) is the acceptance test for `--fix`. After a run leaves
it injected, roll it back and confirm recovery.

The commands below name the default context, `kind-kubeagent-chaos`. After a run
with `--k8s-version`, substitute that run's context throughout — `kubectl config
get-contexts` shows it, and the node in the `Uncordon` check gains the same
suffix (`kubeagent-chaos-v1-33-worker`).

```bash
# Force a degraded rollout: no surge + allow an old pod down, so the failing new
# pod replaces a serving one (Ready < Desired) — which is what --fix now requires
# before proposing a rollback.
kubectl --context kind-kubeagent-chaos -n chaos-rollout patch deploy/web \
  -p '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}'
kubectl --context kind-kubeagent-chaos -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos -n chaos-rollout rollout status deploy/web
```

kubeagent should propose and apply a `RolloutUndo` (the Deployment is degraded —
the new pod can't pull and replaced a serving one), and the Deployment should
return to a healthy image.

Scenario 3 (node cordon) is the acceptance test for `Uncordon`:

```bash
kubectl --context kind-kubeagent-chaos cordon kubeagent-chaos-worker
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos get node kubeagent-chaos-worker   # SchedulingDisabled should be gone
```

Each scenario **injects → scans → reverts** so the next starts clean. Scenario 1
(stopping the control-plane) **runs last** in the suite even though it's listed
first: etcd/apiserver flap for a while after a `docker stop`/`start`, so running
it last keeps that recovery noise out of the other scenarios' scans. After a full
run the cluster's control-plane may still be recovering — use `--recreate` for a
fresh run, or `--teardown` to delete it.

Scenarios 1, 2, and 8 deliberately demonstrate kubeagent's **boundaries** (an unreachable
API returns a connectivity error, not a cluster report; cert expiry is out of
scope on Kind; a stateless scan can't flag a deleted namespace) — the report
labels these as boundaries, not failures.

## A note on LitmusChaos

LitmusChaos was evaluated for the OOMKilled scenario (its canonical
`pod-memory-hog` experiment). The operator installs cleanly, but the chaoshub /
chaos-charts **experiment manifests were empty or 404 at every pinned ref tried**
at build time — too fragile for a test that gates releases. So scenario 7 uses a
**deterministic memory-hog** (`stress`, 64Mi limit) that reliably produces a true
`OOMKilled` (kernel cgroup kill, exit 137). To use real LitmusChaos instead,
vendor a pinned `pod-memory-hog` ChaosExperiment + ChaosEngine and swap it into
`scenario_07_oom`.

## Safety

- By default the harness targets **only** the Kind context it creates
  (`kind-kubeagent-chaos`, or `kind-kubeagent-chaos-v1-33` and the like under
  `--k8s-version`) — it does not read your current kubecontext, so a plain run
  cannot touch another cluster. `--context <ctx>` deliberately points it at a
  cluster you already have; that mode is described under
  [Portable mode](#portable-mode) and carries its own guard rails.
- The credential-leak scenario uses the documentation value
  `AKIAIOSFODNN7EXAMPLE` — never a real secret.
- `ANTHROPIC_API_KEY` is read from the environment and never logged or committed.
