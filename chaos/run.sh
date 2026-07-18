#!/usr/bin/env bash
set -euo pipefail

# kubeagent chaos-test harness — reproduces common production outages on a
# disposable Kind cluster and runs `kubeagent scan` against each, writing a
# results report for pre-release review. Targets ONLY its own Kind context.

CLUSTER=kubeagent-chaos
CTX=kind-$CLUSTER
COREDNS_BACKUP=/tmp/kubeagent-chaos-coredns.yaml   # pristine Corefile, captured while healthy
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

# Normalize a numeric --only to the zero-padded form used in scenario keys (01..10).
if [ -n "$ONLY" ] && printf '%s' "$ONLY" | grep -qE '^[0-9]+$'; then ONLY=$(printf '%02d' "$ONLY"); fi

: "${OUT:=docs/testing/chaos-results.md}"

log() { printf '\n=== %s ===\n' "$*"; }

preflight() {
  for b in docker kind kubectl helm go; do
    command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
  done
  docker info >/dev/null 2>&1 || { echo "docker daemon not running" >&2; exit 1; }
}

build_kubeagent() { log "build kubeagent"; go build -o ./kubeagent .; ./kubeagent version; }

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    if [ "$RECREATE" = 1 ]; then kind delete cluster --name "$CLUSTER"; else
      echo "cluster $CLUSTER already exists (use --recreate to rebuild)"; return 0; fi
  fi
  log "create kind cluster $CLUSTER"
  kind create cluster --name "$CLUSTER" --config chaos/kind-config.yaml --wait 120s
}

# preload_calico_images side-loads the Calico images into the Kind nodes before we
# apply the CNI. Kind nodes have their own containerd store, so on a cold cluster the
# kubelet pulls calico/cni + calico/node serially from docker.io (~3-4m each) — and the
# calico-node rollout routinely misses its deadline waiting on that, the #1 flake in this
# harness. Pulling to the host once and `kind load`-ing makes the in-node pull instant.
# Best-effort: if a pull/load fails, install_calico's in-node pull + wait still covers it.
preload_calico_images() {
  log "preload Calico images into $CLUSTER nodes"
  local ref
  for ref in $(grep -hoE 'docker\.io/calico/[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+' chaos/manifests/calico.yaml | sort -u); do
    docker image inspect "$ref" >/dev/null 2>&1 || docker pull "$ref" || { echo "preload: pull $ref failed; falling back to in-node pull" >&2; continue; }
    # `docker pull docker.io/calico/x:tag` tags the local image `calico/x:tag`; kind load
    # re-adds the docker.io/ prefix in the node store, matching the manifest's image ref.
    kind load docker-image "${ref#docker.io/}" --name "$CLUSTER" || echo "preload: load $ref failed; falling back to in-node pull" >&2
  done
}

install_calico() {
  log "install Calico CNI"
  kubectl --context "$CTX" apply -f chaos/manifests/calico.yaml
  # Images are preloaded (see preload_calico_images), so the rollout is normally fast;
  # the generous timeout only covers a preload miss falling back to an in-node pull.
  kubectl --context "$CTX" -n kube-system rollout status ds/calico-node --timeout=600s
  kubectl --context "$CTX" wait --for=condition=Ready nodes --all --timeout=600s
}

# wait_system_ready blocks until the core system Deployments are Available, so the
# baseline scan sees a settled cluster. On a freshly-created cluster CoreDNS,
# calico-kube-controllers, and local-path-provisioner can still be Pending for a
# while after the nodes go Ready — scanning too early makes the baseline read
# Degraded (a harness timing artifact, not a real finding).
wait_system_ready() {
  log "wait for system workloads to settle (CoreDNS, Calico controllers, local-path)"
  kubectl --context "$CTX" -n kube-system rollout status deploy/coredns --timeout=300s
  kubectl --context "$CTX" -n kube-system rollout status deploy/calico-kube-controllers --timeout=300s
  kubectl --context "$CTX" -n local-path-storage rollout status deploy/local-path-provisioner --timeout=300s
}

# Append --explain ONLY when a key is present in the environment (never logged).
explain_flag() { [ -n "${ANTHROPIC_API_KEY:-}" ] && echo "--explain" || true; }
# scan [extra args...] — runs kubeagent scan against the chaos context.
scan() { ./kubeagent scan --context "$CTX" "$@" $(explain_flag); }

# record <title> <verdict> ; reads scan (and optional --explain) output from stdin.
# Scan output is wrapped in a code fence; any --explain markdown (after the
# "── Explanation ──" marker kubeagent prints) is emitted raw so its own code
# fences render instead of breaking the outer fence.
record() {
  {
    printf '\n## %s\n\n_Verdict: %s_\n\n' "$1" "$2"
    awk '
      BEGIN { print "```text" }
      /── Explanation ──/ { print "```"; print ""; seen=1; next }
      { print }
      END { if (!seen) print "```" }
    '
  } >> "$OUT"
}

teardown() { log "teardown"; kind delete cluster --name "$CLUSTER"; }

# --- scenarios -------------------------------------------------------------
# Each scenario: inject -> scan (recorded; never aborts the harness) -> revert.

cp_container() { docker ps --filter "name=${CLUSTER}-control-plane" --format '{{.Names}}' | head -1; }

scenario_01_etcd() {   # control-plane / etcd down -> API unreachable
  log "scenario 1: etcd quorum loss (control-plane stopped)"
  local c; c="$(cp_container)"
  docker stop "$c" >/dev/null
  sleep 5
  { scan 2>&1 || true; } | record "1. etcd quorum loss (control-plane stopped)" "boundary: connectivity diagnosis expected"
  docker start "$c" >/dev/null
  kubectl --context "$CTX" wait --for=condition=Ready nodes --all --timeout=180s >/dev/null 2>&1 || true
  # Wait for the abruptly-stopped control-plane static pods (etcd/apiserver/scheduler/
  # controller-manager) to re-stabilize, so this scenario can't bleed crash-loop noise
  # into the next one.
  kubectl --context "$CTX" -n kube-system wait --for=condition=Ready pod -l tier=control-plane --timeout=180s >/dev/null 2>&1 || true
  sleep 10
}

scenario_03_diskfull() {   # cordon stand-in for DiskPressure/SchedulingDisabled
  log "scenario 3: disk full on control plane (node cordon stand-in)"
  local node; node="$(kubectl --context "$CTX" get nodes -o name | grep worker | head -1 | cut -d/ -f2)"
  kubectl --context "$CTX" cordon "$node" >/dev/null
  kubectl --context "$CTX" create ns chaos-diskfull --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-diskfull create deploy toobig --image=registry.k8s.io/pause:3.10 >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n chaos-diskfull patch deploy toobig --type=json \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"1000"}}}]' >/dev/null
  sleep 12
  { scan 2>&1 || true; } | record "3. Disk full on control plane (node cordon + unschedulable pod)" "detected: SchedulingDisabled + Unschedulable"
  kubectl --context "$CTX" uncordon "$node" >/dev/null
  kubectl --context "$CTX" delete ns chaos-diskfull --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_05_coredns() {   # bad Corefile -> CoreDNS CrashLoop
  log "scenario 5: broken DNS (CoreDNS crash)"
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge \
    -p='{"data":{"Corefile":".:53 {\n    this_is_an_invalid_plugin\n}\n"}}' >/dev/null
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  sleep 30
  { scan 2>&1 || true; } | record "5. Broken DNS (CoreDNS crash)" "detected: P1 cluster Degraded + CrashLoopBackOff"
  # Restore the pristine Corefile (captured in main()) via a clean merge-patch.
  local patch; patch=$(python3 -c 'import json,sys; print(json.dumps({"data":{"Corefile":open(sys.argv[1]).read()}}))' "$COREDNS_BACKUP")
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge -p "$patch" >/dev/null
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  kubectl --context "$CTX" -n kube-system rollout status deploy coredns --timeout=120s >/dev/null 2>&1 || true
}

scenario_04_networkpolicy() {   # Calico-enforced deny-all + a degraded (never-Ready) app
  log "scenario 4: NetworkPolicy blocking traffic"
  kubectl --context "$CTX" create ns chaos-np --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-np apply -f - >/dev/null <<'APP'
apiVersion: apps/v1
kind: Deployment
metadata: { name: blocked, labels: { app: blocked } }
spec:
  replicas: 1
  selector: { matchLabels: { app: blocked } }
  template:
    metadata: { labels: { app: blocked } }
    spec:
      containers:
        - name: app
          image: nginx:1.27-alpine
          readinessProbe: { exec: { command: ["false"] }, periodSeconds: 3 }
APP
  kubectl --context "$CTX" -n chaos-np apply -f - >/dev/null <<'NP'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: deny-all }
spec: { podSelector: {}, policyTypes: [Ingress, Egress] }
NP
  sleep 15
  { scan 2>&1 || true; } | record "4. NetworkPolicy blocking traffic (Calico deny-all)" "detected: degraded workload + NetworkPolicy hint"
  kubectl --context "$CTX" delete ns chaos-np --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_06_lb() {   # LoadBalancer Service with no provider -> pending (no external address)
  log "scenario 6: cloud load balancer failure"
  kubectl --context "$CTX" create ns chaos-lb --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-lb apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n chaos-lb rollout status deploy web --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n chaos-lb patch svc web -p '{"spec":{"type":"LoadBalancer"}}' >/dev/null
  sleep 10
  { scan 2>&1 || true; } | record "6. Cloud load balancer failure (LoadBalancer pending)" "detected: Service issues - no external address"
  kubectl --context "$CTX" delete ns chaos-lb --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_08_nsdelete() {   # stateless blind spot
  log "scenario 8: accidental namespace deletion"
  kubectl --context "$CTX" create ns chaos-doomed --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-doomed apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n chaos-doomed rollout status deploy web --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete ns chaos-doomed --wait=true >/dev/null 2>&1 || true
  { scan 2>&1 || true; } | record "8. Accidental namespace deletion" "boundary: stateless scanner reports no issues (no expected-state tracking)"
}

scenario_09_rollout() {   # bad image -> ImagePullBackOff
  log "scenario 9: faulty rolling deployment"
  kubectl --context "$CTX" create ns chaos-rollout --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-rollout apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n chaos-rollout rollout status deploy web --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 18
  { scan 2>&1 || true; } | record "9. Faulty rolling deployment (bad image)" "detected: ImagePullBackOff"
  kubectl --context "$CTX" delete ns chaos-rollout --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_10_credleak() {   # ConfigMap with a fake AWS key -> --lint-secrets
  log "scenario 10: security credential leak"
  kubectl --context "$CTX" create ns chaos-cred --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-cred create cm app-config \
    --from-literal=AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE >/dev/null
  sleep 3
  { scan --lint-secrets 2>&1 || true; } | record "10. Security credential leak (--lint-secrets)" "detected: credential warning (location+pattern only)"
  kubectl --context "$CTX" delete ns chaos-cred --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_11_kubelet() {   # runtime outage: node NotReady, kubelet /healthz still ok -> --kubelet-health abstains
  log "scenario 11: kubelet health probe via nodes/proxy (--kubelet-health)"
  local node; node="$(kubectl --context "$CTX" get nodes -o name | grep -m1 worker | cut -d/ -f2)"
  # Stop the container runtime on a worker (its Kubernetes node name equals its Kind
  # container name, so `docker exec` reaches it). The kubelet marks the node NotReady
  # — the container-runtime health feeds the node's Ready condition, which the base
  # scan flags — but the kubelet HTTP server keeps serving /healthz "ok": the only
  # checks on kubelet /healthz are ping/log/syncloop, and syncloop survives a runtime
  # outage. So --kubelet-health probes every kubelet through nodes/proxy and, correctly,
  # does NOT double-flag this node: it targets a kubelet that *self-reports* unhealthy
  # on /healthz (a failing syncloop), a distinct signal from NotReady. This exercises
  # the probe path end-to-end (RBAC + nodes/proxy + classify) and pins its no-false-
  # positive boundary; the unhealthy-classification path itself is unit-tested in
  # internal/collect (a kubelet /healthz non-200 cannot be forced on Kind).
  docker exec "$node" systemctl stop containerd >/dev/null 2>&1 || true
  kubectl --context "$CTX" wait --for='condition=Ready=false' node/"$node" --timeout=120s >/dev/null 2>&1 || true
  local h; h="$(kubectl --context "$CTX" get --raw "/api/v1/nodes/$node/proxy/healthz" 2>/dev/null || echo '<unreachable>')"
  { scan --kubelet-health 2>&1 || true; } | record "11. Kubelet health probe via nodes/proxy (worker runtime down, --kubelet-health)" "boundary: node NotReady flagged by the base scan; kubelet /healthz reports '$h', so --kubelet-health probes every node and does not double-flag it (no false positive)"
  # Revert: bring the runtime back and let the node settle Ready before the next scenario.
  docker exec "$node" systemctl start containerd >/dev/null 2>&1 || true
  kubectl --context "$CTX" wait --for=condition=Ready node/"$node" --timeout=180s >/dev/null 2>&1 || true
  sleep 10
}

scenario_02_certs() {   # documented skip (can't force cert expiry on Kind)
  log "scenario 2: expired certificates (skipped)"
  printf 'Skipped on Kind: control-plane certificate expiry cannot be forced quickly or safely.\nkubeagent TLS / expired-certificate handling is covered by internal/connectivity unit tests\n(x509 UnknownAuthority / CertificateInvalid / Hostname errors, plus "x509:" / "certificate" / "tls: " substrings).\n' \
    | record "2. Expired certificates" "skipped (documented; TLS branch unit-tested)"
}

scenario_07_oom() {   # deterministic memory-hog -> OOMKilled (see chaos/README.md re: LitmusChaos)
  log "scenario 7: OOMKilled critical workload (memory-hog)"
  kubectl --context "$CTX" create ns chaos-oom --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-oom apply -f - >/dev/null <<'OOM'
apiVersion: apps/v1
kind: Deployment
metadata: { name: oom-target, labels: { app: oom-target } }
spec:
  replicas: 1
  selector: { matchLabels: { app: oom-target } }
  template:
    metadata: { labels: { app: oom-target } }
    spec:
      containers:
        - name: hog
          image: polinux/stress
          resources: { requests: { memory: "32Mi" }, limits: { memory: "64Mi" } }
          command: ["stress"]
          args: ["--vm", "1", "--vm-bytes", "200M", "--vm-hang", "1"]  # touch >limit so the kernel OOM-kills it (reason OOMKilled, not malloc Error)
OOM
  sleep 35
  { scan 2>&1 || true; } | record "7. OOMKilled critical workload (memory-hog, 64Mi limit)" "detected: OOMKilled + container requests/limits"
  kubectl --context "$CTX" delete ns chaos-oom --wait=true --timeout=120s >/dev/null 2>&1 || true
}

run_scenarios() {
  # 01_etcd runs LAST: stopping the control-plane is the most disruptive fault and
  # etcd/apiserver flap for a while afterwards (and while the API is down even
  # `kubectl wait` can't settle it). Running it last keeps that recovery noise from
  # contaminating the other scenarios' scans.
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 01_etcd)
  for s in "${all[@]}"; do
    if [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ]; then "scenario_$s"; fi
  done
}

main() {
  preflight
  build_kubeagent
  create_cluster
  preload_calico_images
  install_calico

  mkdir -p "$(dirname "$OUT")"
  : > "$OUT"
  {
    printf '# kubeagent chaos-test results\n\n'
    printf -- '- Cluster: Kind %s, Calico CNI, 1 control-plane + 2 workers\n' "$(kind version 2>/dev/null | awk '{print $2}')"
    printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
    printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
  } >> "$OUT"

  # Capture the pristine CoreDNS Corefile TEXT now (cluster is healthy) so scenario 5
  # can restore a known-good config via a clean merge-patch (apply of a get-dump is unreliable).
  kubectl --context "$CTX" -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' > "$COREDNS_BACKUP" 2>/dev/null || true

  wait_system_ready

  log "baseline healthy scan"
  { scan 2>&1 || true; } | record "Baseline (healthy cluster)" "baseline"

  run_scenarios

  log "done — report: $OUT"
  if [ "$TEARDOWN" = 1 ]; then teardown; else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    echo "  kind delete cluster --name $CLUSTER"
  fi
}

main
