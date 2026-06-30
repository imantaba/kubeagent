# Chaos-Test Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: this plan is validated against a **live Kind cluster** (Docker + network), which sandboxed subagents cannot drive — use superpowers:executing-plans (inline, controller runs each task and validates against the cluster). Steps use checkbox (`- [ ]`) syntax.

**Goal:** A committed `chaos/` harness that, on a disposable Kind cluster, injects the 10 production-outage scenarios, runs `kubeagent scan` against each, and writes a results report — run manually before each release.

**Architecture:** One bash orchestrator (`chaos/run.sh`) drives cluster lifecycle (Kind + Calico), a hybrid fault set (LitmusChaos for OOMKilled, `kubectl`/`docker` for the rest), and report assembly. Each scenario is **inject → scan → revert**. The script targets a fixed `kind-kubeagent-chaos` context and refuses to act on any other cluster.

**Tech Stack:** Kind v0.30 (k8s v1.34), Calico, LitmusChaos operator, kubectl, helm, docker, the `kubeagent` Go binary (built from source).

## Global Constraints

- **No real cluster touched.** The harness creates and targets only `kind-kubeagent-chaos`; it never reads the operator's current kubecontext. Every `kubeagent`/`kubectl`/`docker` action names that context/cluster explicitly.
- **No secrets committed.** The credential-leak scenario uses an obviously-fake documentation value (`AKIAIOSFODNN7EXAMPLE`). `ANTHROPIC_API_KEY` is read from the environment only — never echoed, logged, or written to the report.
- **`--explain` is conditional:** scans append `--explain` **iff `$ANTHROPIC_API_KEY` is non-empty**, so the harness is deterministic without a key and includes `--explain` when the operator runs it.
- **Committed:** everything under `chaos/` + the README release-checklist edit. **Git-ignored / local:** the report under `docs/testing/`, the cluster, and the built `./kubeagent`.
- **No Go code change.** Do not modify the Go module, `ci.yml`, or `release.yml`.
- Cluster name `kubeagent-chaos`; context `kind-kubeagent-chaos`. Calico podSubnet `192.168.0.0/16`.
- Each scenario must **revert** so the next scenario starts clean. Scenarios 1/2/8 are recorded as kubeagent *boundaries*, not failures.
- Lint every shell change with `bash -n chaos/run.sh` (and `shellcheck` if installed).

---

### Task 1: Harness skeleton — cluster lifecycle, helpers, report scaffold

**Files:**
- Create: `chaos/kind-config.yaml`, `chaos/run.sh`, `chaos/manifests/calico.yaml` (downloaded, pinned)
- Modify: `.gitignore` (ignore the built binary path is already covered by `/kubeagent`; nothing new needed — verify)

**Interfaces:**
- Produces: `chaos/run.sh` with flag parsing (`--teardown`, `--recreate`, `--only <n>`, `--out <path>`), `preflight()`, `build_kubeagent()`, `create_cluster()`, `install_calico()`, helpers `scan()` / `explain_flag()` / `record()`, `main()` loop, and `teardown()`. Scenarios are added in later tasks as `scenario_NN_*` functions registered in an ordered list.

- [ ] **Step 1: `chaos/kind-config.yaml`**

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true      # Calico provides the CNI (kindnet doesn't enforce NetworkPolicy)
  podSubnet: "192.168.0.0/16"  # Calico default
nodes:
  - role: control-plane
  - role: worker
  - role: worker
```

- [ ] **Step 2: Download + commit the pinned Calico manifest**

```bash
mkdir -p chaos/manifests
curl -sSLo chaos/manifests/calico.yaml https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml
grep -c 'kind: ' chaos/manifests/calico.yaml   # sanity: non-empty manifest
```
(Committing the manifest pins the Calico version and avoids a run-time fetch.)

- [ ] **Step 3: `chaos/run.sh` skeleton**

```bash
#!/usr/bin/env bash
set -euo pipefail

CLUSTER=kubeagent-chaos
CTX=kind-$CLUSTER
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""

while [ $# -gt 0 ]; do
  case "$1" in
    --teardown) TEARDOWN=1 ;;
    --recreate) RECREATE=1 ;;
    --only) ONLY="$2"; shift ;;
    --out) OUT="$2"; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac; shift
done

: "${OUT:=docs/testing/chaos-results.md}"   # date is prefixed by the operator/run; see Task 4

log() { printf '\n=== %s ===\n' "$*"; }

preflight() {
  for b in docker kind kubectl helm go; do command -v "$b" >/dev/null || { echo "missing: $b" >&2; exit 1; }; done
  docker info >/dev/null 2>&1 || { echo "docker daemon not running" >&2; exit 1; }
}

build_kubeagent() { log "build kubeagent"; go build -o ./kubeagent .; ./kubeagent version; }

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    if [ "$RECREATE" = 1 ]; then kind delete cluster --name "$CLUSTER"; else
      echo "cluster $CLUSTER exists (use --recreate to rebuild)"; return; fi
  fi
  log "create kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --config chaos/kind-config.yaml --wait 120s
}

install_calico() {
  log "install Calico"
  kubectl --context "$CTX" apply -f chaos/manifests/calico.yaml
  kubectl --context "$CTX" -n kube-system rollout status ds/calico-node --timeout=180s
  kubectl --context "$CTX" wait --for=condition=Ready nodes --all --timeout=180s
}

explain_flag() { [ -n "${ANTHROPIC_API_KEY:-}" ] && echo "--explain" || true; }
scan() { ./kubeagent scan --context "$CTX" "$@" $(explain_flag); }   # extra args (e.g. --lint-secrets) before --explain

record() {  # record <title> <verdict> <<<"output"
  { printf '\n## %s\n\n_Verdict: %s_\n\n```text\n' "$1" "$2"; cat; printf '```\n'; } >> "$OUT"
}

teardown() { log "teardown"; kind delete cluster --name "$CLUSTER"; }

main() {
  preflight
  build_kubeagent
  create_cluster
  install_calico
  mkdir -p "$(dirname "$OUT")"
  : > "$OUT"
  { printf '# kubeagent chaos-test results\n\n'
    printf -- '- Cluster: Kind %s / k8s %s, Calico CNI\n' "$(kind version | awk '{print $2}')" "$(kubectl --context "$CTX" version -o json 2>/dev/null | grep -o '"gitVersion":"[^"]*"' | head -1)"
    printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
  } >> "$OUT"

  # baseline
  log "baseline healthy scan"
  scan 2>&1 | record "Baseline (healthy cluster)" "baseline"

  # scenarios registered here in later tasks:
  run_scenarios

  log "done — report: $OUT"
  [ "$TEARDOWN" = 1 ] && teardown || echo "cluster left up ($CTX). Re-run with --teardown to delete."
}

run_scenarios() { :; }   # replaced as scenarios are added (Tasks 2-4)

main
```

- [ ] **Step 4: Lint + live-validate the skeleton**

```bash
chmod +x chaos/run.sh
bash -n chaos/run.sh
command -v shellcheck >/dev/null && shellcheck chaos/run.sh || echo "(shellcheck not installed — skipped)"
# live: cluster comes up with Calico, baseline recorded, teardown works
./chaos/run.sh --recreate
kubectl --context kind-kubeagent-chaos get nodes
grep -q "Baseline (healthy cluster)" docs/testing/chaos-results.md && echo "baseline recorded OK"
```
Expected: 3 nodes Ready (Calico), baseline section in the report, "No issues found" in the baseline scan.

- [ ] **Step 5: Commit**

```bash
git add chaos/ && git commit -m "chaos: harness skeleton (Kind+Calico lifecycle, scan/record helpers, report)"
```

---

### Task 2: Control-plane scenarios — etcd-down (1), disk-full/cordon (3), CoreDNS crash (5)

**Files:**
- Modify: `chaos/run.sh` (add `scenario_01_etcd`, `scenario_03_diskfull`, `scenario_05_coredns`; register them in `run_scenarios`)

**Interfaces:**
- Consumes: `scan`, `record`, `CTX`, `CLUSTER` from Task 1.

- [ ] **Step 1: Add the three scenario functions**

```bash
cp_container() { docker ps --filter "name=${CLUSTER}-control-plane" --format '{{.Names}}' | head -1; }

scenario_01_etcd() {   # control-plane / etcd down -> API unreachable
  log "scenario 1: etcd/control-plane down"
  local c; c="$(cp_container)"
  docker stop "$c" >/dev/null
  sleep 5
  scan 2>&1 | record "1. etcd quorum loss (control-plane stopped)" "boundary: connectivity diagnosis expected"
  docker start "$c" >/dev/null
  kubectl --context "$CTX" wait --for=condition=Ready nodes --all --timeout=180s
}

scenario_03_diskfull() {   # cordon stand-in for DiskPressure/SchedulingDisabled
  log "scenario 3: disk full (cordon stand-in)"
  local node; node="$(kubectl --context "$CTX" get nodes -o name | grep worker | head -1 | cut -d/ -f2)"
  kubectl --context "$CTX" cordon "$node"
  kubectl --context "$CTX" create ns chaos-diskfull --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-diskfull create deploy toobig --image=registry.k8s.io/pause:3.10 || true
  kubectl --context "$CTX" -n chaos-diskfull patch deploy toobig --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"1000"}}}]'
  sleep 10
  scan 2>&1 | record "3. Disk full on control plane (node cordon + unschedulable pod)" "detected: SchedulingDisabled + Unschedulable"
  kubectl --context "$CTX" uncordon "$node"
  kubectl --context "$CTX" delete ns chaos-diskfull --wait=false
}

scenario_05_coredns() {   # bad Corefile -> CoreDNS CrashLoop
  log "scenario 5: CoreDNS crash"
  kubectl --context "$CTX" -n kube-system get cm coredns -o yaml > /tmp/coredns-backup.yaml
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge \
    -p='{"data":{"Corefile":".:53 {\n    this_is_an_invalid_plugin\n}\n"}}'
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns
  sleep 25
  scan 2>&1 | record "5. Broken DNS (CoreDNS crash)" "detected: P1 cluster Degraded + CrashLoopBackOff"
  kubectl --context "$CTX" -n kube-system apply -f /tmp/coredns-backup.yaml
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns
  kubectl --context "$CTX" -n kube-system rollout status deploy coredns --timeout=120s
}
```

- [ ] **Step 2: Register in `run_scenarios`** (replace the Task-1 stub)

```bash
run_scenarios() {
  local all=(01_etcd 03_diskfull 05_coredns)
  for s in "${all[@]}"; do [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ] && "scenario_$s"; done
}
```

- [ ] **Step 3: Lint + live-validate each**

```bash
bash -n chaos/run.sh
./chaos/run.sh --only 1   # expect connectivity diagnosis in report, then nodes Ready again
./chaos/run.sh --only 3   # expect SchedulingDisabled + Unschedulable, then uncordoned
./chaos/run.sh --only 5   # expect P1 coredns CrashLoop, then coredns healthy again
```
Expected: each scenario's `record` section shows the signal in the verdict column; after each, `kubectl get nodes`/`get pods -n kube-system` confirm the revert.

- [ ] **Step 4: Commit**

```bash
git add chaos/run.sh && git commit -m "chaos: control-plane scenarios (etcd-down, disk-full cordon, CoreDNS crash)"
```

---

### Task 3: Workload scenarios — NetworkPolicy (4), LB-pending (6), namespace deletion (8), faulty rollout (9), credential leak (10)

**Files:**
- Modify: `chaos/run.sh` (add five scenario functions; extend `run_scenarios`)
- Create: `chaos/manifests/app.yaml` (a simple healthy Deployment+Service used by several scenarios)

**Interfaces:**
- Consumes: `scan`, `record`, `CTX` from Task 1.

- [ ] **Step 1: `chaos/manifests/app.yaml`** (reusable sample app)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: web, labels: { app: web } }
spec:
  replicas: 2
  selector: { matchLabels: { app: web } }
  template:
    metadata: { labels: { app: web } }
    spec:
      containers:
        - name: web
          image: registry.k8s.io/echoserver:1.10
          ports: [{ containerPort: 8080 }]
          readinessProbe: { httpGet: { path: /, port: 8080 }, initialDelaySeconds: 3 }
---
apiVersion: v1
kind: Service
metadata: { name: web }
spec:
  selector: { app: web }
  ports: [{ port: 80, targetPort: 8080 }]
```

- [ ] **Step 2: Add the five scenario functions**

```bash
scenario_04_networkpolicy() {   # Calico-enforced deny-all -> readiness fails
  log "scenario 4: NetworkPolicy block"
  kubectl --context "$CTX" create ns chaos-np --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-np apply -f chaos/manifests/app.yaml
  kubectl --context "$CTX" -n chaos-np apply -f - <<'NP'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: deny-all }
spec: { podSelector: {}, policyTypes: [Ingress, Egress] }
NP
  kubectl --context "$CTX" -n chaos-np rollout status deploy web --timeout=60s || true
  sleep 10
  scan 2>&1 | record "4. NetworkPolicy blocking traffic (Calico deny-all)" "detected: degraded workload + NetworkPolicy hint"
  kubectl --context "$CTX" delete ns chaos-np --wait=false
}

scenario_06_lb() {   # LoadBalancer Service stays pending (no LB provider on Kind)
  log "scenario 6: cloud LB failure"
  kubectl --context "$CTX" create ns chaos-lb --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-lb apply -f chaos/manifests/app.yaml
  kubectl --context "$CTX" -n chaos-lb patch svc web -p '{"spec":{"type":"LoadBalancer"}}'
  sleep 8
  scan 2>&1 | record "6. Cloud load balancer failure (LoadBalancer pending)" "detected: Service issues - no external address"
  kubectl --context "$CTX" delete ns chaos-lb --wait=false
}

scenario_08_nsdelete() {   # stateless blind spot
  log "scenario 8: namespace deletion"
  kubectl --context "$CTX" create ns chaos-doomed --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-doomed apply -f chaos/manifests/app.yaml
  kubectl --context "$CTX" -n chaos-doomed rollout status deploy web --timeout=60s || true
  kubectl --context "$CTX" delete ns chaos-doomed --wait=true
  scan 2>&1 | record "8. Accidental namespace deletion" "boundary: stateless scanner reports no issues (no expected-state tracking)"
}

scenario_09_rollout() {   # bad image -> ImagePullBackOff
  log "scenario 9: faulty rolling deployment"
  kubectl --context "$CTX" create ns chaos-rollout --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-rollout apply -f chaos/manifests/app.yaml
  kubectl --context "$CTX" -n chaos-rollout rollout status deploy web --timeout=60s || true
  kubectl --context "$CTX" -n chaos-rollout set image deploy/web web=registry.k8s.io/echoserver:does-not-exist
  sleep 15
  scan 2>&1 | record "9. Faulty rolling deployment (bad image)" "detected: ImagePullBackOff"
  kubectl --context "$CTX" delete ns chaos-rollout --wait=false
}

scenario_10_credleak() {   # ConfigMap with a fake AWS key -> --lint-secrets
  log "scenario 10: credential leak"
  kubectl --context "$CTX" create ns chaos-cred --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-cred create cm app-config \
    --from-literal=AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE
  sleep 3
  scan --lint-secrets 2>&1 | record "10. Security credential leak (--lint-secrets)" "detected: credential warning (location+pattern only)"
  kubectl --context "$CTX" delete ns chaos-cred --wait=false
}
```

- [ ] **Step 3: Extend `run_scenarios`**

```bash
run_scenarios() {
  local all=(01_etcd 03_diskfull 04_networkpolicy 05_coredns 06_lb 08_nsdelete 09_rollout 10_credleak)
  for s in "${all[@]}"; do [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ] && "scenario_$s"; done
}
```

- [ ] **Step 4: Lint + live-validate**

```bash
bash -n chaos/run.sh
for n in 4 6 8 9 10; do ./chaos/run.sh --only $n; done
```
Expected: scenario 4 → degraded web + NetworkPolicy hint; 6 → "no external address"; 8 → "No issues found" (boundary); 9 → ImagePullBackOff; 10 → a credential warning with no value shown. Each namespace is cleaned up.

- [ ] **Step 5: Commit**

```bash
git add chaos/ && git commit -m "chaos: workload scenarios (NetworkPolicy, LB-pending, ns-delete, faulty rollout, cred leak)"
```

---

### Task 4: LitmusChaos OOMKilled (7) + expired-certs skip (2)

**Files:**
- Create: `chaos/manifests/litmus/` (operator install reference, `pod-memory-hog` experiment, target app, ChaosEngine + RBAC)
- Modify: `chaos/run.sh` (add `scenario_02_certs`, `scenario_07_oom`; register them)

**Interfaces:**
- Consumes: `scan`, `record`, `CTX`.

- [ ] **Step 1: Litmus manifests**

Download the pinned Litmus operator and the `pod-memory-hog` experiment into `chaos/manifests/litmus/`:

```bash
mkdir -p chaos/manifests/litmus
curl -sSLo chaos/manifests/litmus/operator.yaml https://litmuschaos.github.io/litmus/litmus-operator-v3.9.0.yaml
curl -sSLo chaos/manifests/litmus/pod-memory-hog.yaml https://hub.litmuschaos.io/api/chaos/3.9.0?file=charts/generic/pod-memory-hog/experiment.yaml
```

Create `chaos/manifests/litmus/target.yaml` — a Deployment with a memory limit, annotated for chaos, plus the ServiceAccount/Role/RoleBinding the experiment needs and the ChaosEngine:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: { name: oom-target, labels: { app: oom-target } }
spec:
  replicas: 1
  selector: { matchLabels: { app: oom-target } }
  template:
    metadata: { labels: { app: oom-target }, annotations: { litmuschaos.io/chaos: "true" } }
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
          resources: { limits: { memory: "64Mi" }, requests: { memory: "64Mi" } }
---
# ServiceAccount + Role + RoleBinding (litmus-admin scoped to the chaos ns) + ChaosEngine
# (full RBAC + ChaosEngine targeting app=oom-target with EXPERIMENT pod-memory-hog,
#  MEMORY_CONSUMPTION beyond the 64Mi limit, TOTAL_CHAOS_DURATION 60)
```

(The exact RBAC + ChaosEngine YAML is finalized during live validation against the installed operator version; the ChaosEngine sets `EXPERIMENT=pod-memory-hog`, `appns=chaos-oom`, `applabel=app=oom-target`, `MEMORY_CONSUMPTION=256`.)

- [ ] **Step 2: Add the two scenario functions**

```bash
scenario_02_certs() {   # documented skip
  log "scenario 2: expired certificates (skipped)"
  printf 'Skipped on Kind: control-plane cert expiry cannot be forced quickly/safely.\nkubeagent TLS/expired-cert handling is covered by internal/connectivity unit tests\n(x509 UnknownAuthority/CertificateInvalid/Hostname + "x509:"/"certificate" + "tls: ").\n' \
    | record "2. Expired certificates" "skipped (documented; TLS branch unit-tested)"
}

scenario_07_oom() {   # LitmusChaos pod-memory-hog
  log "scenario 7: OOMKilled (LitmusChaos pod-memory-hog)"
  kubectl --context "$CTX" apply -f chaos/manifests/litmus/operator.yaml
  kubectl --context "$CTX" -n litmus rollout status deploy chaos-operator-ce --timeout=180s || true
  kubectl --context "$CTX" create ns chaos-oom --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -
  kubectl --context "$CTX" -n chaos-oom apply -f chaos/manifests/litmus/pod-memory-hog.yaml
  kubectl --context "$CTX" -n chaos-oom apply -f chaos/manifests/litmus/target.yaml
  kubectl --context "$CTX" -n chaos-oom rollout status deploy oom-target --timeout=120s || true
  sleep 45   # let the memory-hog drive the container OOM
  scan 2>&1 | record "7. OOMKilled critical workload (LitmusChaos pod-memory-hog)" "detected: OOMKilled + container requests/limits"
  kubectl --context "$CTX" delete ns chaos-oom --wait=false
  kubectl --context "$CTX" delete -f chaos/manifests/litmus/operator.yaml --wait=false || true
}
```

**Fallback (record if Litmus proves unreliable during validation):** replace the target with a deterministic memory-hog Deployment (`image: polinux/stress`, `args: ["stress","--vm","1","--vm-bytes","256M"]`, `resources.limits.memory: 64Mi`) → the kubelet OOMKills it directly. If used, note it in the scenario's verdict line and `chaos/README.md`.

- [ ] **Step 3: Register all 10 in `run_scenarios`**

```bash
run_scenarios() {
  local all=(01_etcd 02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak)
  for s in "${all[@]}"; do [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ] && "scenario_$s"; done
}
```

- [ ] **Step 4: Lint + live-validate**

```bash
bash -n chaos/run.sh
./chaos/run.sh --only 7    # expect an OOMKilled finding with the container's 64Mi limit
./chaos/run.sh --only 2    # expect the skip section
```
Expected: scenario 7 records an OOMKilled finding (Litmus or fallback); scenario 2 records the documented skip.

- [ ] **Step 5: Commit**

```bash
git add chaos/ && git commit -m "chaos: OOMKilled via LitmusChaos pod-memory-hog + documented expired-certs skip"
```

---

### Task 5: Docs — `chaos/README.md` + release-gating checklist; full end-to-end run

**Files:**
- Create: `chaos/README.md`
- Modify: `README.md` (the "Cutting a release" section)

- [ ] **Step 1: `chaos/README.md`**

Write it covering: purpose (pre-release validation), prerequisites (docker, kind v0.30, kubectl, helm, go), how to run (`./chaos/run.sh`, flags `--recreate`/`--teardown`/`--only N`/`--out`), the `--explain` note (set `ANTHROPIC_API_KEY` to include it; otherwise deterministic), the 10-scenario table (method + expected signal, copied from the spec), the boundary scenarios (1/2/8), and where the report lands (`docs/testing/`, git-ignored).

- [ ] **Step 2: Release-checklist edit in `README.md`**

In the "Cutting a release" section, add a first step before tagging:

```markdown
**Pre-release chaos test.** On a machine with Docker, run the chaos suite on a
disposable Kind cluster and review the report for detection regressions:

```bash
./chaos/run.sh --recreate --teardown        # deterministic
# or, to also exercise --explain:
ANTHROPIC_API_KEY=sk-ant-... ./chaos/run.sh --recreate --teardown
```

Review `docs/testing/*-chaos-results.md` (git-ignored), confirm each scenario's
expected signal still appears, then proceed to tag.
```
```

- [ ] **Step 3: Full end-to-end run (the release's results report)**

```bash
ts=$(date +%F)   # operator supplies the date
./chaos/run.sh --recreate --out "docs/testing/${ts}-chaos-results.md"
```
Expected: all 10 scenarios recorded, cluster left up for optional `--explain` re-run. Skim the report; confirm the boundary scenarios read as boundaries, not surprises.

- [ ] **Step 4: Commit**

```bash
git add chaos/README.md README.md
git commit -m "docs(chaos): README + pre-release chaos-test checklist"
```

---

## Self-Review

**Spec coverage:**
- Kind+Calico lifecycle, helpers, report scaffold, `--explain`-if-key, teardown → Task 1. ✓
- 9 reproduced + boundary scenarios (1,3,5 / 4,6,8,9,10 / 7,2) → Tasks 2–4 (all 10). ✓
- LitmusChaos for OOMKilled (+ fallback), expired-certs skip → Task 4. ✓
- Safety: fixed `kind-kubeagent-chaos` context, fake credential value, key never logged → Task 1 helpers + scenario 10 (`AKIAIOSFODNN7EXAMPLE`) + `explain_flag`. ✓
- Report to git-ignored `docs/testing/`, release checklist → Tasks 1, 4-report, 5. ✓
- No Go/ci/release changes → only `chaos/` + README. ✓

**Placeholder scan:** the only deferred detail is the Litmus RBAC/ChaosEngine YAML finalized during live validation (Task 4 Step 1), with a concrete deterministic fallback specified — not a vague "handle it later." Everything else is concrete commands.

**Type/name consistency:** `CLUSTER=kubeagent-chaos` / `CTX=kind-kubeagent-chaos`, the `scenario_NN_*` names, the `run_scenarios` list, `scan`/`record`/`explain_flag` helpers, and the `--only`/`--recreate`/`--teardown`/`--out` flags are used identically across Tasks 1–5. The report path default (`docs/testing/chaos-results.md`) and the dated override (Task 5) are consistent with the git-ignored `docs/testing/` policy.
