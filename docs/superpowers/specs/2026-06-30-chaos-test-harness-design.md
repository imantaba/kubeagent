# kubeagent — Design: chaos-test harness (pre-release)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-30

## Goal

A committed, repeatable chaos-test harness that, on a disposable **Kind** cluster,
reproduces the 10 most common production-outage scenarios, runs `kubeagent scan`
against each, and writes a results report — so a maintainer validates kubeagent's
detection (and surfaces regressions/gaps) **before every release**. Run manually;
results reviewed by a human.

## Decisions (from brainstorming)

- **Disposable Kind cluster** (Docker-in-Docker), never a real/production cluster.
- **Gating:** a committed `chaos/` harness run **manually before a release**, plus
  a checklist item in the release docs. Not a per-push CI job (chaos is heavy and
  best run deliberately).
- **Realism — pragmatic:** automate 9/10 reliably; document substitutions for the
  rest (see scenario table). `expired certs` is skipped (can't force on Kind).
- **CNI:** Calico (Kind's default kindnet does not enforce NetworkPolicy).
- **Chaos tooling — hybrid:** LitmusChaos for OOMKilled (`pod-memory-hog`, its
  canonical experiment); deterministic `kubectl`/`docker` injection for the rest.
- **`--explain`:** the harness adds `--explain` to scans **only when
  `ANTHROPIC_API_KEY` is present in the operator's environment** — so it is
  deterministic-only when the key is absent, and includes `--explain` when the
  operator (who holds the key) runs it. The key never needs to enter an agent's
  shell.
- **Teardown:** cluster left **up** by default (for `--explain`/inspection); a
  `--teardown` flag deletes it.

## Invariants / constraints

- **READ-ONLY tool under test:** `kubeagent` only reads. The *harness* injects
  faults, but exclusively on its own disposable `kind-kubeagent-chaos` context —
  every `kubeagent`/`kubectl`/`docker` call targets that cluster explicitly.
- **No real cluster touched.** The harness refuses to run against any non-Kind
  context (it creates and targets its own cluster by name).
- **No secrets committed.** The credential-leak scenario uses an obviously-fake
  test value (a documentation `AKIA…` placeholder), never a real secret. The
  `ANTHROPIC_API_KEY` is read from the environment only, never logged or written.
- **Committed:** `chaos/` (script, kind config, manifests, README) + the
  release-docs checklist edit. **Git-ignored / local:** the results report under
  `docs/testing/`, the Kind cluster, and the built `kubeagent` binary.
- No change to kubeagent's Go code or release workflow behavior (only docs gain a
  checklist item).

## Architecture

```text
chaos/run.sh
  ├─ preflight: require docker, kind, helm, kubectl, go; refuse if a kind cluster
  │             of the same name exists unless --recreate
  ├─ build: go build -o ./kubeagent .
  ├─ cluster: kind create --config chaos/kind-config.yaml  (name kubeagent-chaos)
  ├─ CNI: install Calico (manifests/calico.yaml) → wait Ready
  ├─ Litmus: install operator + pod-memory-hog experiment (manifests/litmus/*)
  ├─ for each scenario S in 1..10:
  │     inject(S) → ./kubeagent scan --context kind-kubeagent-chaos [--explain?] → capture → revert(S)
  ├─ write report → docs/testing/<date>-chaos-results.md
  └─ teardown if --teardown, else leave cluster up
```

## Component 1 — orchestrator (`chaos/run.sh`)

A bash script with: a preflight check; cluster lifecycle; helpers `scan()` (runs
`./kubeagent scan --context kind-kubeagent-chaos`, appending `--explain` iff
`$ANTHROPIC_API_KEY` is non-empty) and `record()` (writes a section to the report);
one function per scenario (`scenario_01_etcd` … `scenario_10_credleak`), each
**inject → scan → revert**; and report assembly. Flags: `--teardown` (delete
cluster at end), `--recreate` (delete+recreate if the cluster exists),
`--only <n>` (run a single scenario for debugging), `--out <path>` (override the
report path).

The context name is fixed (`kind-kubeagent-chaos`); the script never reads the
operator's current kubecontext, so it cannot act on an unintended cluster.

## Component 2 — cluster config (`chaos/kind-config.yaml`)

1 control-plane + 2 workers; `networking.disableDefaultCNI: true` and a
`podSubnet` compatible with Calico. Three nodes so workload scheduling, cordon,
and DaemonSet scenarios are meaningful.

## Component 3 — manifests (`chaos/manifests/`)

- `calico.yaml` (pinned Calico version)
- `litmus/` — operator install + `pod-memory-hog` ChaosExperiment, a target
  Deployment (with a memory limit) annotated for chaos, the ChaosEngine, and the
  RBAC/ServiceAccount the experiment needs
- supporting YAML reused by scenarios: deny-all NetworkPolicy, a LoadBalancer
  Service, the faulty/healthy sample Deployment, the credential-leak ConfigMap/pod

## Scenario implementations (the 10)

| # | Scenario | Inject | Expected kubeagent signal | Revert |
|---|----------|--------|---------------------------|--------|
| 1 | etcd quorum loss | `docker stop` the control-plane node container | connectivity diagnosis (connection refused/reset) + `details:` | `docker start` |
| 2 | Expired certificates | **SKIPPED** — recorded with rationale (TLS-cert branch unit-tested) | n/a | n/a |
| 3 | Disk full (control plane) | `kubectl cordon` a node + a pod with an impossible CPU request | P1 `node … SchedulingDisabled` + `Unschedulable` pending pod | `kubectl uncordon`, delete pod |
| 4 | NetworkPolicy block | deny-all NetworkPolicy in an app namespace (Calico enforces) | degraded workload + `NetworkPolicy` hint | delete NP / namespace |
| 5 | CoreDNS crash | bad Corefile in `kube-system/coredns` + rollout restart | `Cluster: Degraded` → P1 `kube-system/coredns` + CrashLoopBackOff | restore Corefile + restart |
| 6 | Cloud LB failure | create a `type: LoadBalancer` Service (no LB provider on Kind) | "Service issues … no external address" | delete service |
| 7 | OOMKilled | LitmusChaos `pod-memory-hog` on a limited target Deployment | OOMKilled finding + the container's requests/limits | delete ChaosEngine + app |
| 8 | Namespace deletion | create ns + workload, then `kubectl delete ns` | validates the known stateless limitation (no expected-state tracking) | n/a |
| 9 | Faulty rolling deployment | `kubectl set image` to a non-existent tag | ImagePullBackOff flagged though replicas read healthy | set good image / delete |
| 10 | Security credential leak | ConfigMap + pod env with a fake `AKIA…` value | `scan --lint-secrets` flags location+pattern (never value) | delete objects |

Scenarios 1, 2, 8 are expected to show kubeagent's *boundaries* (1 → connectivity
error not a cluster report; 8 → stateless blind spot); the report records that
explicitly so a reviewer isn't surprised. Scenario 10 runs `kubeagent scan
--lint-secrets`; `--explain` is never given credential findings.

## Component 4 — results report (`docs/testing/<date>-chaos-results.md`)

The harness writes a timestamped markdown: environment (Kind/k8s versions, CNI),
then per scenario — what was injected, the verbatim `kubeagent scan` output
(deterministic, plus `--explain` when a key was present), and a verdict
(detected / boundary / gap). A final "Gaps / future fixes" section the maintainer
reviews. This file is git-ignored (kept local), matching the existing
`docs/testing/` policy.

## Component 5 — release gating (docs)

Add a **Pre-release chaos test** step to the release docs (the README "Cutting a
release" section): before tagging, run `./chaos/run.sh`, review the generated
report, confirm no detection regressions vs the prior report, then tag. The
deterministic run needs no API key; `--explain` validation is the maintainer's
option.

## Testing the harness itself

The harness is shell + manifests, validated by **running it against a live Kind
cluster** (the canonical test): each scenario must inject cleanly, produce the
expected kubeagent signal, and revert without leaking state into the next. A
`--only <n>` flag supports validating one scenario at a time during development.
`bash -n` / shellcheck (if available) lint the script. There are no Go unit tests
(no Go code changes).

## Out of scope (explicit non-goals)

- A per-push CI job (the harness is a deliberate pre-release manual run).
- Faithful reproduction of expired certs, multi-node etcd quorum, or real disk
  exhaustion (pragmatic stand-ins are used and documented).
- LitmusChaos ChaosCenter/portal (only the lightweight operator + one experiment).
- Asserting/failing automatically on kubeagent output (a human reviews the report);
  the harness reports, it does not gate by exit code on detection quality.
- Any change to kubeagent's Go code or the release/CI workflows' behavior.
