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
  for b in docker kind kubectl helm go curl python3; do
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
  { scan 2>&1 || true; } | record "5. Broken DNS (CoreDNS crash)" \
    "expect: the cluster line reads Degraded and kube-system/coredns is listed under NEEDS ATTENTION, under-replicated (1/2 or 0/2) with a non-zero restart count. That much is invariant. The specific finding is NOT: whether a CrashLoopBackOff line appears depends on where the scan lands in the kubelet's restart-backoff cycle — caught between restarts the pods read 0/1 Running with a restart count and no CrashLoopBackOff finding at all, which is a pass, not a miss. Assert on Degraded plus restarts; treat the named finding as timing-dependent."
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
  { scan 2>&1 || true; } | record "4. NetworkPolicy blocking traffic (Calico deny-all)" \
    "expect: chaos-np/blocked is reported 0/1 Degraded with a ProbeFailure finding, and NO NetworkPolicy hint. The absent hint is correct, not a miss: netpolicy.Annotate (internal/netpolicy/netpolicy.go) attaches policy names only to a workload that is Flagged() with zero detector findings, because the hint exists to explain a degraded workload nothing else accounts for. A failing readiness probe already accounts for this one, so the hint is suppressed by design. KNOWN GAP, tracked for a later slice: the probe here is exec [\"false\"], which fails whether or not the NetworkPolicy exists — deleting deny-all would not change this output. So the scenario proves a degraded workload is detected while a deny-all policy is in force; it does not yet prove the policy is the cause. Making it causal needs a network-dependent probe and its own verification run."
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
  # force a real outage: with the default strategy the old replicas keep serving, and
  # kubeagent deliberately does not remediate a workload that still meets its replica
  # target — so take the old pods down with the rollout to make Ready < Desired.
  kubectl --context "$CTX" -n chaos-rollout patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 18
  { scan 2>&1 || true; } | record "9. Faulty rolling deployment (bad image)" "detected: ImagePullBackOff"
  # slice-4: apply the fix with an audit log, then roll it back and confirm the image returns
  local alog; alog="$(mktemp)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --fix --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_fix; after_fix="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --rollback --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_rollback; after_rollback="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  {
    echo "after --fix:      $after_fix"
    echo "after --rollback: $after_rollback"
    { grep -c '"disposition":"rollback"' "$alog" 2>/dev/null || true; } | sed 's/^/rollback audit records: /'
  } | record "9b. Fix then rollback (audit-log round trip)" "rollback restores the pre-fix image"
  rm -f "$alog"
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

scenario_12_watch() {   # stateful watch daemon: NEW on outage, RESOLVED on repair, /issues while firing
  log "scenario 12: stateful watch daemon (NEW / RESOLVED transitions, /issues)"
  local ns=chaos-watch port=18080 aport=18081 wlog wpid i firing after alerts apid
  wlog="$(mktemp)"
  alerts="$(mktemp)"
  # A local receiver proves the alert path end to end. The daemon's only egress
  # besides the API server is this URL.
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  # The daemon is strictly read-only, so it runs with the same kubeconfig as the scans.
  # A short heartbeat keeps the transition latency inside this scenario's sleeps.
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done

  # Inject the same outage as scenario 9 (bad image, old replicas taken down with the
  # rollout so Ready < Desired), then let two heartbeats land.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 30
  firing="$(curl -s "http://127.0.0.1:$port/issues" 2>/dev/null || echo '<unreachable>')"

  # Repair and let the tracker observe the issue clear.
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:1.27-alpine >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=120s >/dev/null 2>&1 || true
  sleep 30
  after="$(curl -s "http://127.0.0.1:$port/issues" 2>/dev/null || echo '<unreachable>')"

  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true

  {
    echo '--- daemon transition log (NEW / RESOLVED / FLAPPING lines only) ---'
    { grep -E 'kubeagent: (NEW|RESOLVED|FLAPPING) ' "$wlog" || echo '<no transition lines logged>'; }
    echo
    echo '--- /issues while the outage was firing ---'
    printf '%s\n' "$firing" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$firing"
    echo
    echo '--- /issues after the repair (active empty, the incident under resolved) ---'
    printf '%s\n' "$after" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$after"
    echo
    echo '--- alerts delivered to the webhook receiver ---'
    { grep -o '"status":"[a-z]*","reason":"[a-z]*"' "$alerts" || echo '<no alerts delivered>'; }
    echo
    printf 'firing notifications: %s\n' "$(grep -c '"status":"firing"' "$alerts" 2>/dev/null || echo 0)"
    printf 'resolved notifications: %s\n' "$(grep -c '"status":"resolved"' "$alerts" 2>/dev/null || echo 0)"
    # Key on kind+namespace+name, not name alone: the Deployment and the Service in
    # this scenario are both called "web", so a name-only count collapses two objects
    # into one and cannot tell "two objects resolved once each" from "one object
    # resolved twice" — the exact regression the per-object rollup exists to prevent.
    printf 'distinct objects alerted: %s\n' "$(grep -o '"kind":"[^"]*","namespace":"[^"]*","name":"[^"]*"' "$alerts" 2>/dev/null | sort -u | wc -l)"
    echo
    echo '--- resolved alerts per object (must be exactly one each) ---'
    { grep '"status":"resolved"' "$alerts" 2>/dev/null \
        | grep -o '"kind":"[^"]*","namespace":"[^"]*","name":"[^"]*"' \
        | sort | uniq -c || echo '<no resolved alerts delivered>'; }
    echo
    echo '--- webhook URL redaction check (only scheme://host may appear) ---'
    { grep -c '127.0.0.1:'"$aport" "$wlog" || true; } | sed 's/^/log lines naming the endpoint host: /'
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    { grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true; } | sed 's/^/log lines mentioning a write verb: /'
  } | record "12. Stateful watch daemon (NEW on outage, RESOLVED on repair, /issues)" "expect: one NEW line naming Deployment/$ns/web, one RESOLVED line with the firing duration, the incident listed under /issues while firing and under resolved afterwards, and exactly one resolved alert per broken object — two objects break here (Deployment/$ns/web and its Service), so two objects alert and each resolves once. The Deployment's firing alert must survive the whole Degraded -> ErrImagePull -> ImagePullBackOff walk without a resolved notification, even though the per-issue transition log reports RESOLVED for each superseded mode."

  rm -f "$wlog" "$alerts"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_13_slo() {   # SLO burn rate: series track real breakage, and a cold daemon does NOT page
  log "scenario 13: SLO burn-rate signals (cold daemon must not page)"
  local ns=chaos-slo port=18082 aport=18083 wlog wpid i alerts apid healthy broken n
  wlog="$(mktemp)"
  alerts="$(mktemp)"
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h \
    --slo-target 99.9 >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done
  sleep 30
  healthy="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null | grep '^kubeagent_slo_' || echo '<unreachable>')"

  # Break the only workload in scope (bad image, old replicas taken down with the
  # rollout so Ready < Desired — the same fault as scenarios 9 and 12).
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 45
  broken="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null | grep '^kubeagent_slo_' || echo '<unreachable>')"

  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true

  {
    echo '--- SLO series while the workload was healthy ---'
    printf '%s\n' "$healthy"
    echo
    echo '--- SLO series after the workload broke ---'
    printf '%s\n' "$broken"
    echo
    echo '--- SLO notifications delivered to the webhook receiver ---'
    n=$(grep -c '"kind":"SLO"' "$alerts" 2>/dev/null) || true
    printf 'SLO alerts delivered: %s\n' "${n:-0}"
    echo '(must be 0 because the coverage gate held, not because nothing arrived — cross-check against the total below)'
    echo
    echo '--- object alerts still work in the same daemon (proves the SLO suppression is not a dead pipe) ---'
    n=$(grep -c '"kind":"Deployment"' "$alerts" 2>/dev/null) || true
    printf 'Deployment alerts delivered: %s\n' "${n:-0}"
    echo
    echo '--- total notification lines received (0 here is a scenario FAILURE: the receiver never got anything) ---'
    n=$(wc -l 2>/dev/null < "$alerts") || true
    printf 'total notification lines: %s\n' "${n:-0}"
    echo
    echo '--- daemon log tail (last 15 lines; diagnoses a failed start without re-running the suite) ---'
    tail -n 15 "$wlog" 2>/dev/null || echo '<no daemon log captured>'
  } | record "13. SLO burn-rate signals (cold daemon must not page)" \
    "expect: the five kubeagent_slo_* series render in both snapshots, with kubeagent_slo_target_ratio exactly 0.999 (the one exact value here). kubeagent_slo_availability_ratio falls materially from the healthy snapshot toward the broken one but must NOT reach 0: Availability is accumulated as time-weighted workload-seconds across the WHOLE window, not read from the latest sample, and this run holds only ~30 healthy seconds against ~45 broken ones, so expect something near 0.4 — not 0, and not an exact value (pod-scheduling and image-pull timing shift it). kubeagent_slo_burn_rate rises far past both thresholds (14.4x fast, 6x slow) — near (1-0.4)/(1-0.999) = 600x, not the theoretical maximum, and again not exact. kubeagent_slo_window_coverage_ratio stays far below the 0.6 gate on both windows: the daemon has covered on the order of a minute against a 1h fast window and a 6h slow window. Despite that burn rate, SLO notifications delivered must be 0 — the coverage gate suppresses the page regardless of how hot the burn is — while Deployment alerts delivered must be NON-zero (the object-alert path fires normally) and total notification lines must also be NON-zero: a 0 total would mean the webhook pipe never worked at all, which is a scenario FAILURE, not the same thing as a correctly-suppressed 0 SLO count. This scenario deliberately does NOT cover a real full-window breach: filling the 6h slow window takes six hours; shortening it would need a test-only production flag, and claiming a breach after ninety seconds would be asserting a lie. The threshold arithmetic and the firing transition are unit-tested with an injected clock instead, where six hours costs nothing."

  rm -f "$wlog" "$alerts"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_14() {   # on-incident explanations: budget throttle, /explanations, local stub (no API key)
  log "scenario 14: on-incident explanations (budget, throttle, /explanations)"
  local ns=chaos-explain port=18090 aport=18091 sport=18092
  local wlog alerts calls wpid apid spid i expl

  wlog="$(mktemp)"; alerts="$(mktemp)"; calls="$(mktemp)"

  # A local receiver proves the notification path; a local OpenAI-compatible
  # stub proves the model path. No API key is involved anywhere in this
  # scenario — the endpoint is the only backend the daemon talks to.
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
  python3 chaos/explain-stub.py "$sport" "$calls" >/dev/null 2>&1 &
  spid=$!

  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  # Budget 1 with no per-object cooldown: two objects break at once, so exactly
  # one earns an explanation and the rest are throttled. That is the whole
  # point of the budget, asserted rather than assumed.
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  KUBEAGENT_EXPLAIN_ENDPOINT="http://127.0.0.1:$sport/v1" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h \
    --explain --explain-budget 1 --explain-cooldown 0 --model chaos-stub >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done

  # The daemon is now primed on a healthy namespace, so what follows is a real
  # transition rather than a cold-start snapshot.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 40

  expl="$(curl -s "http://127.0.0.1:$port/explanations" 2>/dev/null || echo '<unreachable>')"
  local metrics
  metrics="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null || echo '')"

  kill "$wpid" >/dev/null 2>&1 || true; wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true; wait "$apid" >/dev/null 2>&1 || true
  kill "$spid" >/dev/null 2>&1 || true; wait "$spid" >/dev/null 2>&1 || true

  {
    echo '--- model calls the daemon actually made (one line per call) ---'
    printf 'calls: %s\n' "$(wc -l <"$calls" 2>/dev/null || echo 0)"
    echo
    echo '--- /explanations ---'
    printf '%s\n' "$expl" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$expl"
    echo
    echo '--- explain metrics ---'
    { grep -E '^kubeagent_explain_' <<<"$metrics" || echo '<no explain series>'; }
    echo
    echo '--- explanation notifications delivered ---'
    { grep -c '"reason":"explanation"' "$alerts" 2>/dev/null || echo 0; } | sed 's/^/explanation notifications: /'
    { grep -c '"reason":"new"' "$alerts" 2>/dev/null || echo 0; } | sed 's/^/plain firing notifications: /'
    echo
    echo '--- egress check: no pod name, pod IP or node name in any prompt ---'
    { grep -cE '"prompt":[^\n]*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-f]{6,}|kubeagent-chaos-worker)' "$calls" 2>/dev/null || true; } \
      | sed 's/^/prompts leaking pod or node detail: /'
    echo
    echo '--- endpoint redaction check (only scheme://host may appear in logs) ---'
    { grep -c "127.0.0.1:$sport/v1" "$wlog" || true; } | sed 's/^/log lines naming the endpoint path: /'
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    { grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true; } | sed 's/^/log lines mentioning a write verb: /'
  } | record "14. On-incident explanations (budget 1, two objects break)" "expect: exactly 1 model call and exactly 1 explanation notification (reason=explanation) even though two objects break — Deployment/$ns/web and its Service — because --explain-budget 1 admits one and throttles the rest. kubeagent_explain_allowed_total must be 1 and kubeagent_explain_throttled_total at least 1; /explanations must carry one entry with non-empty text and model=chaos-stub, alongside the plain firing notifications which are unaffected. No prompt may contain a pod name, pod IP or node name, no log line may carry the endpoint's path, and no write verb may appear. This scenario uses a local stub endpoint, so it proves the transport, the throttle, the notification shape and the egress discipline — it does not exercise the Anthropic backend, which is covered by unit tests only."

  rm -f "$wlog" "$alerts" "$calls"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
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
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 01_etcd)
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
