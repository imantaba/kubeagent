# kubeagent chaos-test harness

A repeatable, **pre-release** chaos test. It spins up a disposable **Kind**
cluster, injects the most common production outages, runs `kubeagent scan`
against each, and writes a results report you review before tagging a release.

It is read-only with respect to any real cluster: it creates and targets only
its own `kind-kubeagent-chaos` context and never reads your current kubecontext.

## Prerequisites

- **Docker** (the Kind nodes run as containers)
- **kind** ≥ v0.30 — install:
  `curl -sSLo kind https://kind.sigs.k8s.io/dl/v0.30.0/kind-linux-amd64 && chmod +x kind && sudo mv kind /usr/local/bin/`
- **kubectl**, **helm**, **go**, **python3**

## Run

```bash
./chaos/run.sh                 # create cluster, run all scenarios, leave cluster up
./chaos/run.sh --recreate      # delete + recreate the cluster first (clean slate)
./chaos/run.sh --teardown      # delete the cluster when finished
./chaos/run.sh --only 7        # run a single scenario (1..10) for debugging
./chaos/run.sh --out path.md   # write the report somewhere specific
```

The report is written to `docs/testing/chaos-results.md` by default (the
`docs/testing/` directory is git-ignored, so reports stay local).

### `--explain`

`run.sh` adds `--explain` to every scan **only when `ANTHROPIC_API_KEY` is set in
your environment** — so the harness is deterministic by default, and includes the
Claude-summarized output when you opt in:

```bash
ANTHROPIC_API_KEY=sk-ant-... ./chaos/run.sh --recreate
```

The key is read from the environment only; it is never written to the report.

## Scenarios

| # | Outage | How it's injected | Expected kubeagent signal |
|---|--------|-------------------|---------------------------|
| 1 | etcd quorum loss | `docker stop` the control-plane node | connectivity diagnosis (connection refused) — a **boundary** |
| 2 | Expired certificates | **skipped** (can't be forced on Kind) | n/a — TLS branch is unit-tested |
| 3 | Disk full (control plane) | `kubectl cordon` + an unschedulable pod | P1 node `SchedulingDisabled` + `Unschedulable` |
| 4 | NetworkPolicy block | Calico deny-all + a never-Ready app | degraded workload + NetworkPolicy hint |
| 5 | Broken DNS | bad Corefile → CoreDNS CrashLoop | P1 cluster `Degraded` + CrashLoopBackOff |
| 6 | Cloud LB failure | `type: LoadBalancer` Service (no provider) | Service issues — no external address |
| 7 | OOMKilled | memory-hog Deployment, 64Mi limit | OOMKilled + container requests/limits |
| 8 | Namespace deletion | `kubectl delete ns` | "No issues found" — a **boundary** (stateless) |
| 9 | Faulty rollout | `kubectl set image` to a bad tag | ImagePullBackOff |
| 10 | Credential leak | ConfigMap with a fake `AKIA…` value | `--lint-secrets` warning (location + pattern only) |

### Validating `--fix` (remediation)

Scenario 9 (faulty rollout) is the acceptance test for `--fix`. After a run leaves
it injected, roll it back and confirm recovery:

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

- Targets only `kind-kubeagent-chaos`; refuses to touch any other cluster.
- The credential-leak scenario uses the documentation value
  `AKIAIOSFODNN7EXAMPLE` — never a real secret.
- `ANTHROPIC_API_KEY` is read from the environment and never logged or committed.
