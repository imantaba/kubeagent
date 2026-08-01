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
TEARDOWN=0; RECREATE=0; ONLY=""; OUT=""; K8S_VERSION=""; KIND_IMAGE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --teardown) TEARDOWN=1 ;;
    --recreate) RECREATE=1 ;;
    --only) ONLY="$2"; shift ;;
    --out) OUT="$2"; shift ;;
    --k8s-version) K8S_VERSION="$2"; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac; shift
done

# Normalize a numeric --only to the zero-padded form used in scenario keys (01..20).
# 10# forces base 10: printf reads a leading-zero numeral as octal, so a plain
# --only 08 or --only 09 errored and normalized to 00, silently matching nothing.
if [ -n "$ONLY" ] && printf '%s' "$ONLY" | grep -qE '^[0-9]+$'; then ONLY=$(printf '%02d' "$((10#$ONLY))"); fi

# Kubernetes version axis: chaos_versions / chaos_image / chaos_suffix, backed by
# the digest-pinned set in chaos/versions.env.
# shellcheck source=chaos/versions.sh
. "$ROOT/chaos/versions.sh"

# The version axis. Omitting --k8s-version keeps the historical names and lets
# kind pick its own default image, so the release skill's documented command and
# an operator's muscle memory keep working byte-for-byte.
#
# Everything cluster-shaped is derived from one place: two minors run on one
# machine otherwise collide on the cluster, the context, the report and the
# CoreDNS scratch file — and a collision on the last one is the nastiest,
# because it silently restores the wrong Corefile.
#
# chaos_image runs FIRST because it is the call that validates: an unsupported or
# malformed minor is refused here, before any name is derived from it and before
# preflight has touched docker. chaos_suffix therefore cannot fail below.
if [ -n "$K8S_VERSION" ]; then
  KIND_IMAGE="$(chaos_image "$K8S_VERSION")" || exit 2
  suffix="$(chaos_suffix "$K8S_VERSION")"
  CLUSTER="$CLUSTER$suffix"
  CTX="kind-$CLUSTER"
  COREDNS_BACKUP="/tmp/$CLUSTER-coredns.yaml"
  : "${OUT:=docs/testing/chaos-results-$K8S_VERSION.md}"
fi

: "${OUT:=docs/testing/chaos-results.md}"

# Assertion helpers (expect_eq / expect_ge / expect_contains / expect_absent) and
# the summary that turns their outcomes into this script's exit code.
# shellcheck source=chaos/assert.sh
. "$ROOT/chaos/assert.sh"

log() { printf '\n=== %s ===\n' "$*"; }

# check_inotify_limits — the harness's own diagnosis of a failure mode that
# otherwise costs four minutes and explains nothing.
#
# Every kubelet, kube-proxy and controller in a kind node takes inotify
# instances from a HOST-WIDE budget; containers do not get their own. Below
# kind's recommended values a single cluster usually still boots, so the
# machine looks fine — until a second cluster starts and the new node's
# kube-proxy dies with "too many open files". kubeadm then waits four minutes
# for a kubelet that will never be healthy and exits with a Go stack trace
# naming none of this. The version axis makes that likely rather than rare:
# per-minor cluster names are exactly what lets two clusters coexist.
#
# Warn whenever the limits are low; fail only when they are low AND another
# kind cluster is already up, because that pair is what actually breaks. A
# re-run against this run's own cluster is not another cluster.
check_inotify_limits() {
  local want_instances=512 want_watches=524288 have_instances have_watches others
  have_instances="$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || true)"
  have_watches="$(sysctl -n fs.inotify.max_user_watches 2>/dev/null || true)"
  # Not Linux, or the keys are unreadable: nothing to say, and nothing to block.
  case "$have_instances$have_watches" in *[!0-9]*|'') return 0 ;; esac
  [ "$have_instances" -ge "$want_instances" ] && [ "$have_watches" -ge "$want_watches" ] && return 0

  others="$(kind get clusters 2>/dev/null | grep -vx "$CLUSTER" | tr '\n' ' ' || true)"
  others="${others% }"
  {
    printf 'inotify limits are below what kind needs:\n'
    printf '  fs.inotify.max_user_instances = %s (kind recommends %s)\n' "$have_instances" "$want_instances"
    printf '  fs.inotify.max_user_watches   = %s (kind recommends %s)\n' "$have_watches"   "$want_watches"
    printf 'Raise them with:\n'
    printf '  sudo sysctl -w fs.inotify.max_user_instances=%s\n' "$want_instances"
    printf '  sudo sysctl -w fs.inotify.max_user_watches=%s\n'   "$want_watches"
  } >&2
  if [ -n "${others// /}" ]; then
    {
      printf 'Refusing to start: these kind clusters are already running and will\n'
      printf 'exhaust the budget, so this cluster'"'"'s kubelet would never come up:\n'
      printf '  %s\n' "$others"
      printf 'Delete them, or raise the limits above.\n'
    } >&2
    exit 1
  fi
}

preflight() {
  for b in docker kind kubectl helm go curl python3; do
    command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
  done
  docker info >/dev/null 2>&1 || { echo "docker daemon not running" >&2; exit 1; }
  check_inotify_limits
}

build_kubeagent() { log "build kubeagent"; go build -o ./kubeagent .; ./kubeagent version; }

create_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    if [ "$RECREATE" = 1 ]; then kind delete cluster --name "$CLUSTER"; else
      echo "cluster $CLUSTER already exists (use --recreate to rebuild)"; return 0; fi
  fi
  log "create kind cluster $CLUSTER${KIND_IMAGE:+ (image $KIND_IMAGE)}"
  # ${KIND_IMAGE:+--image "$KIND_IMAGE"} is deliberately unquoted: it must expand
  # to either two words (--image and the value) or nothing at all, so the no-flag
  # path runs the exact `kind create cluster` command it always has.
  # shellcheck disable=SC2086
  kind create cluster --name "$CLUSTER" --config chaos/kind-config.yaml --wait 120s \
    ${KIND_IMAGE:+--image "$KIND_IMAGE"}
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

# ready_replicas <namespace> <deployment> — the Deployment's ready replica count,
# or 0 when the field is absent (a Deployment with no ready pod omits it entirely).
# Prints "?" when the query itself failed: "no ready pods" and "couldn't tell" are
# different answers, and a scenario that reads a causal claim off these numbers
# must not silently turn an API blip into a confident zero.
ready_replicas() {
  local n
  if ! n="$(kubectl --context "$CTX" -n "$1" get deploy "$2" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"; then
    echo "?"
    return
  fi
  echo "${n:-0}"
}

# scan_body <scan output> — the structured part of a scan, with any --explain
# markdown (everything after the "── Explanation ──" marker) stripped. Counting
# object names over the raw output would also count model prose, which names
# objects freely and would break a "must be 0" check whenever a key is set.
scan_body() { printf '%s\n' "$1" | awk '/── Explanation ──/ { exit } { print }'; }

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

# A failed teardown must not abort main before assert_summary runs: the exit
# code callers read is the assertion gate's, and losing it to kind's would
# report a delete failure as a scenario failure and drop the report's summary.
teardown() { log "teardown"; kind delete cluster --name "$CLUSTER" || log "teardown: kind delete cluster failed (cluster may still exist)"; }

# --- scenarios -------------------------------------------------------------
# Each scenario: inject -> scan (recorded; never aborts the harness) -> revert.

cp_container() { docker ps --filter "name=${CLUSTER}-control-plane" --format '{{.Names}}' | head -1; }

scenario_01_etcd() {   # control-plane / etcd down -> API unreachable
  log "scenario 1: etcd quorum loss (control-plane stopped)"
  local c; c="$(cp_container)"
  docker stop "$c" >/dev/null
  sleep 5
  local out rc
  out="$(scan 2>&1)" && rc=0 || rc=$?
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_ge       "scan exits non-zero when the API is unreachable" "$rc" 1
    expect_contains "connectivity diagnosed in kubeagent's own words" "$out" "refused the connection"
    expect_absent   "no cluster report is rendered"                   "$out" "Cluster: Healthy"
  } | record "1. etcd quorum loss (control-plane stopped)" "boundary: connectivity diagnosis expected"
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
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"              "$rc" 0
    expect_contains "cluster verdict"             "$body" "Cluster: Degraded"
    expect_contains "cordoned node named"         "$body" "$node"
    expect_contains "cordon reported"             "$body" "SchedulingDisabled"
    expect_contains "unschedulable pod reported"  "$body" "Unschedulable"
  } | record "3. Disk full on control plane (node cordon + unschedulable pod)" "detected: SchedulingDisabled + Unschedulable"
  kubectl --context "$CTX" uncordon "$node" >/dev/null
  kubectl --context "$CTX" delete ns chaos-diskfull --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_05_coredns() {   # bad Corefile -> CoreDNS CrashLoop
  log "scenario 5: broken DNS (CoreDNS crash)"
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge \
    -p='{"data":{"Corefile":".:53 {\n    this_is_an_invalid_plugin\n}\n"}}' >/dev/null
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  sleep 30
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"            "$rc" 0
    expect_contains "cluster verdict"           "$body" "Cluster: Degraded"
    expect_contains "coredns named as degraded" "$body" "kube-system/coredns"
  } | record "5. Broken DNS (CoreDNS crash)" \
    "expect: the cluster line reads Degraded and kube-system/coredns is listed under NEEDS ATTENTION, under-replicated (1/2 or 0/2) with a non-zero restart count. That much is invariant. The specific finding is NOT: whether a CrashLoopBackOff line appears depends on where the scan lands in the kubelet's restart-backoff cycle — caught between restarts the pods read 0/1 Running with a restart count and no CrashLoopBackOff finding at all, which is a pass, not a miss. Assert on Degraded plus restarts; treat the named finding as timing-dependent."
  # Restore the pristine Corefile (captured in main()) via a clean merge-patch.
  local patch; patch=$(python3 -c 'import json,sys; print(json.dumps({"data":{"Corefile":open(sys.argv[1]).read()}}))' "$COREDNS_BACKUP")
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge -p "$patch" >/dev/null
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  kubectl --context "$CTX" -n kube-system rollout status deploy coredns --timeout=120s >/dev/null 2>&1 || true
}

scenario_04_networkpolicy() {   # Calico-enforced deny-all as the *cause* of a degraded app
  log "scenario 4: NetworkPolicy blocking traffic"
  local ns=chaos-np i baseline broken recovered blocked_scan recovery_scan probe_event blocked_lines recovery_lines blocked_rc recovery_rc
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  # The probe must be *pod-sourced* for the policy to matter. Calico permits the
  # kubelet's own probe traffic to a local pod even under a deny-all Ingress
  # policy, so an httpGet/tcpSocket probe stays green while the policy is in
  # force (verified on this harness's cluster) — only an exec probe that makes
  # the pod itself talk to the network is actually blocked, by the Egress half.
  # Hence: a plain backend to talk to, and a client whose readiness is one
  # in-cluster HTTP call away.
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null <<'APP'
apiVersion: v1
kind: Service
metadata: { name: backend }
spec:
  selector: { app: backend }
  ports: [{ port: 80, targetPort: 80 }]
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: backend, labels: { app: backend } }
spec:
  replicas: 1
  selector: { matchLabels: { app: backend } }
  template:
    metadata: { labels: { app: backend } }
    spec:
      containers:
        - name: app
          image: nginx:1.27-alpine
---
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
          readinessProbe:
            exec: { command: ["wget", "-q", "-T", "2", "-O", "/dev/null", "http://backend"] }
            periodSeconds: 3
            timeoutSeconds: 3
APP
  kubectl --context "$CTX" -n "$ns" rollout status deploy backend --timeout=120s >/dev/null 2>&1 || true
  # Baseline: the client must reach Ready with NO policy in force. If it never
  # does, the rest of the scenario proves nothing — the recorded output says so
  # rather than quietly reading like a pass.
  kubectl --context "$CTX" -n "$ns" rollout status deploy blocked --timeout=120s >/dev/null 2>&1 || true
  baseline="$(ready_replicas "$ns" blocked)"

  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null <<'NP'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: deny-all }
spec: { podSelector: {}, policyTypes: [Ingress, Egress] }
NP
  # Wait for the policy to actually bite (probe period 3s x default threshold 3)
  # instead of sleeping blind.
  for i in $(seq 30); do
    [ "$(ready_replicas "$ns" blocked)" = "0" ] && break
    sleep 2
  done
  broken="$(ready_replicas "$ns" blocked)"
  blocked_scan="$(scan 2>&1)" && blocked_rc=0 || blocked_rc=$?
  probe_event="$(kubectl --context "$CTX" -n "$ns" get events --field-selector reason=Unhealthy \
    --sort-by=.lastTimestamp -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | tail -1 || true)"

  # Change exactly one thing — remove the policy — and rescan. Nothing else about
  # the workload is touched, so a recovery here can only be the policy's doing.
  kubectl --context "$CTX" -n "$ns" delete netpol deny-all >/dev/null 2>&1 || true
  for i in $(seq 30); do
    [ "$(ready_replicas "$ns" blocked)" = "1" ] && break
    sleep 2
  done
  recovered="$(ready_replicas "$ns" blocked)"
  recovery_scan="$(scan 2>&1)" && recovery_rc=0 || recovery_rc=$?
  blocked_lines="$(scan_body "$blocked_scan"  | grep -c 'chaos-np/blocked' || true)"
  recovery_lines="$(scan_body "$recovery_scan" | grep -c 'chaos-np/blocked' || true)"

  {
    printf 'blocked ready replicas before the policy: %s (must be 1)\n' "$baseline"
    printf 'blocked ready replicas under deny-all:    %s (must be 0)\n' "$broken"
    printf 'blocked ready replicas after deletion:    %s (must be 1)\n' "$recovered"
    echo
    echo '--- newest readiness-probe failure event (the blocked call, not a rigged "false") ---'
    # Sorted by time, not lexically: a benign startup-race Unhealthy event can
    # sort ahead of the policy-caused one and would then be shown under a label
    # claiming it is the blocked call. An empty result is a normal list response,
    # not an error, so the "none" case is a value test rather than a || branch.
    printf '%s\n' "${probe_event:-<no Unhealthy event recorded>}"
    echo
    echo '--- scan WITH deny-all in force ---'
    printf '%s\n' "$blocked_scan"
    echo
    printf 'scan exit code under deny-all: %s\n' "$blocked_rc"
    printf 'chaos-np/blocked lines in that scan: %s\n' "$blocked_lines"
    echo
    echo '--- scan after deleting ONLY the NetworkPolicy ---'
    printf '%s\n' "$recovery_scan"
    echo
    printf 'scan exit code after deleting the policy: %s\n' "$recovery_rc"
    printf 'chaos-np/blocked lines in the recovery scan: %s\n' "$recovery_lines"
    echo
    echo '--- assertions ---'
    expect_eq "blocked ready replicas before the policy" "$baseline"  1
    expect_eq "blocked ready replicas under deny-all"    "$broken"    0
    expect_eq "blocked ready replicas after deletion"    "$recovered" 1
    expect_eq "scan exit code under deny-all"          "$blocked_rc"  0
    expect_ge "chaos-np/blocked reported under deny-all" "$blocked_lines"  1
    # The exit code is asserted before the line count on purpose: a scan that
    # crashed prints no object names either, so "0 lines" alone would read as a
    # clean recovery. Only a scan that succeeded makes the count meaningful.
    expect_eq "scan exit code after deleting the policy" "$recovery_rc" 0
    expect_eq "chaos-np/blocked gone from the recovery scan" "$recovery_lines" 0
  } | record "4. NetworkPolicy blocking traffic (Calico deny-all, causal)" \
    "expect: the three replica counts read 1 / 0 / 1 (a \"?\" means the query itself failed — a harness fault, not a reading). That triple is the whole point of this scenario — the workload is healthy before the policy, degraded while it is in force, and healthy again once it is deleted with nothing else changed, so the policy is demonstrably the cause rather than merely present. The Unhealthy event must show the wget call timing out; the old version of this scenario used an exec probe of \"false\", which failed identically with or without the policy and therefore proved nothing. In the scan taken under deny-all, chaos-np/blocked is reported 0/1 Degraded with a ProbeFailure finding and NO NetworkPolicy hint. The absent hint is correct, not a miss: netpolicy.Annotate (internal/netpolicy/netpolicy.go) attaches policy names only to a workload that is Flagged() with zero detector findings, because the hint exists to explain a degraded workload nothing else accounts for. A failing readiness probe already accounts for this one, so the hint is suppressed by design. In the recovery scan, chaos-np/blocked must not appear at all: its line count must be 0 while the count in the first scan is non-zero. Note the CNI subtlety this scenario encodes: Calico still lets the kubelet probe a local pod under a deny-all Ingress policy, so a network-dependent probe here has to be pod-sourced (exec) and blocked by the Egress half."
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_06_lb() {   # LoadBalancer Service with no provider -> pending (no external address)
  log "scenario 6: cloud load balancer failure"
  kubectl --context "$CTX" create ns chaos-lb --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-lb apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n chaos-lb rollout status deploy web --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n chaos-lb patch svc web -p '{"spec":{"type":"LoadBalancer"}}' >/dev/null
  sleep 10
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"            "$rc" 0
    expect_contains "pending Service named"     "$body" "chaos-lb/web"
    expect_contains "no external address flagged" "$body" "no external address"
  } | record "6. Cloud load balancer failure (LoadBalancer pending)" "detected: Service issues - no external address"
  kubectl --context "$CTX" delete ns chaos-lb --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_08_nsdelete() {   # stateless blind spot
  log "scenario 8: accidental namespace deletion"
  kubectl --context "$CTX" create ns chaos-doomed --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n chaos-doomed apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n chaos-doomed rollout status deploy web --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete ns chaos-doomed --wait=true >/dev/null 2>&1 || true
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"                    "$rc" 0
    expect_contains "cluster verdict"                   "$body" "Cluster: Healthy"
    expect_contains "stateless scanner reports nothing" "$body" "No issues found."
    expect_absent   "the deleted namespace is not reported" "$body" "chaos-doomed"
  } | record "8. Accidental namespace deletion" "boundary: stateless scanner reports no issues (no expected-state tracking)"
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
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"          "$rc" 0
    expect_contains "bad-image workload named" "$body" "chaos-rollout/web"
    expect_contains "image pull failure diagnosed" "$body" "ImagePullBackOff"
  } | record "9. Faulty rolling deployment (bad image)" "detected: ImagePullBackOff"
  # slice-4: apply the fix with an audit log, then roll it back and confirm the image returns
  local alog; alog="$(mktemp)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --fix --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_fix; after_fix="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --rollback --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_rollback; after_rollback="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  local rollback_records
  rollback_records="$(grep -c '"disposition":"rollback"' "$alog" 2>/dev/null || true)"
  {
    echo "after --fix:      $after_fix"
    echo "after --rollback: $after_rollback"
    printf 'rollback audit records: %s\n' "$rollback_records"
    printf '\n--- assertions ---\n'
    expect_eq "--fix restored a working image"      "$after_fix"      "nginx:1.27-alpine"
    expect_eq "--rollback restored the pre-fix image" "$after_rollback" "nginx:does-not-exist-9999"
    expect_ge "rollback recorded in the audit log"  "$rollback_records" 1
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
  local out rc body
  out="$(scan --lint-secrets 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"                "$rc" 0
    expect_contains "leak location named"           "$body" "chaos-cred/app-config"
    expect_contains "credential pattern named"      "$body" "AWS access key"
    expect_absent   "the credential value is never printed" "$body" "AKIAIOSFODNN7EXAMPLE"
  } | record "10. Security credential leak (--lint-secrets)" "detected: credential warning (location+pattern only)"
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
  local out rc body
  out="$(scan --kubelet-health 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    # A precondition, not a kubeagent claim: if the kubelet stopped self-reporting
    # "ok" the no-double-flag assertion below would pass for the wrong reason.
    expect_eq       "precondition: kubelet /healthz still reports ok" "$h" "ok"
    expect_eq       "scan exit code"        "$rc" 0
    expect_contains "cluster verdict"       "$body" "Cluster: Degraded"
    expect_contains "runtime-down node flagged NotReady" "$body" "NotReady"
    expect_contains "the affected node is named"         "$body" "$node"
    # No-double-flag boundary: every kubelet self-reports healthy on /healthz (the
    # precondition above), so kubeletHealthRenders (internal/report/report.go) has
    # nothing to print and the whole KUBELET HEALTH section is omitted — there is no
    # section to extract the node name out of. If a future kubelet regressed
    # unhealthy, the heading itself would appear and this assertion would catch it.
    expect_absent  "no kubelet-health section renders (every kubelet self-reports healthy)" "$body" "KUBELET HEALTH"
  } | record "11. Kubelet health probe via nodes/proxy (worker runtime down, --kubelet-health)" "boundary: node NotReady flagged by the base scan; kubelet /healthz reports '$h', so --kubelet-health probes every node and does not double-flag it (no false positive)"
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

  # Hoist the transition log and the counters into variables so the report and
  # the assertions below read the same values. The `|| true` guards are load-
  # bearing under `set -euo pipefail`: grep (and, via pipefail, a pipe ending
  # in one) exits 1 on zero matches, and a bare `var=$(...)` assignment that
  # fails would abort the whole scenario mid-function under `set -e` — before
  # the namespace cleanup at the bottom ever ran. A `|| true` here does not
  # coerce the value itself: a failed extraction still yields an empty (or, for
  # the wc-l-terminated pipe, zero) capture, so a broken pipeline is still
  # caught by the expect_* calls below rather than being silently swallowed.
  local transitions firing_n resolved_n distinct_n write_verbs
  transitions="$(grep -E 'kubeagent: (\[[^]]*\] )?(NEW|RESOLVED|FLAPPING) ' "$wlog" || true)"
  firing_n="$(grep -c '"status":"firing"' "$alerts" 2>/dev/null || true)"
  resolved_n="$(grep -c '"status":"resolved"' "$alerts" 2>/dev/null || true)"
  distinct_n="$(grep -o '"kind":"[^"]*","namespace":"[^"]*","name":"[^"]*"' "$alerts" 2>/dev/null | sort -u | wc -l || true)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"

  {
    echo '--- daemon transition log (NEW / RESOLVED / FLAPPING lines only) ---'
    # The cluster name is bracketed into every daemon line by clusterLogf
    # (internal/watch/cluster.go), so this pattern has to allow it. Without the
    # optional bracket this grep silently matched nothing from e5ef861 onward and
    # the section read '<no transition lines logged>' on every run — a missing
    # transition and a stale pattern looked identical.
    printf '%s\n' "${transitions:-<no transition lines logged>}"
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
    printf 'firing notifications: %s\n' "$firing_n"
    printf 'resolved notifications: %s\n' "$resolved_n"
    # Key on kind+namespace+name, not name alone: the Deployment and the Service in
    # this scenario are both called "web", so a name-only count collapses two objects
    # into one and cannot tell "two objects resolved once each" from "one object
    # resolved twice" — the exact regression the per-object rollup exists to prevent.
    printf 'distinct objects alerted: %s\n' "$distinct_n"
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
    printf 'log lines mentioning a write verb: %s\n' "$write_verbs"
    echo
    echo '--- assertions ---'
    expect_contains "NEW transition for the broken Deployment" "$transitions" "NEW Deployment/$ns/web"
    expect_contains "RESOLVED transition after the repair"     "$transitions" "RESOLVED Deployment/$ns/web"
    expect_ge "firing notifications delivered"      "$firing_n"   1
    expect_eq "distinct objects alerted"            "$distinct_n" 2
    expect_eq "resolved notifications (one per object)" "$resolved_n" 2
    expect_eq "daemon log mentions no write verb"   "$write_verbs" 0
  } | record "12. Stateful watch daemon (NEW on outage, RESOLVED on repair, /issues)" "expect: one NEW line naming Deployment/$ns/web, one RESOLVED line with the firing duration, the incident listed under /issues while firing and under resolved afterwards, and exactly one resolved alert per broken object — two objects break here (Deployment/$ns/web and its Service), so two objects alert and each resolves once. The Deployment's firing alert must survive the whole Degraded -> ErrImagePull -> ImagePullBackOff walk without a resolved notification, even though the per-issue transition log reports RESOLVED for each superseded mode."

  rm -f "$wlog" "$alerts"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_13_slo() {   # SLO burn rate: series track real breakage, and a cold daemon does NOT page
  log "scenario 13: SLO burn-rate signals (cold daemon must not page)"
  local ns=chaos-slo port=18082 aport=18083 wlog wpid i alerts apid healthy broken
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

  # Hoist the counters and the one exact metric into variables so the report
  # and the assertions below read the same values. No `|| echo 0` fallback: a
  # scrape or extraction that failed must surface as an empty (or, for the
  # wc-l-terminated pipe, a genuinely zero) value that expect_eq/expect_ge can
  # catch, not a coerced pass.
  local slo_alerts dep_alerts total_lines target
  slo_alerts="$(grep -c '"kind":"SLO"' "$alerts" 2>/dev/null || true)"
  dep_alerts="$(grep -c '"kind":"Deployment"' "$alerts" 2>/dev/null || true)"
  total_lines="$(wc -l < "$alerts" 2>/dev/null | tr -d ' ')"
  target="$(printf '%s\n' "$broken" | awk '/^kubeagent_slo_target_ratio/{print $2}')"

  {
    echo '--- SLO series while the workload was healthy ---'
    printf '%s\n' "$healthy"
    echo
    echo '--- SLO series after the workload broke ---'
    printf '%s\n' "$broken"
    echo
    echo '--- SLO notifications delivered to the webhook receiver ---'
    printf 'SLO alerts delivered: %s\n' "$slo_alerts"
    echo '(must be 0 because the coverage gate held, not because nothing arrived — cross-check against the total below)'
    echo
    echo '--- object alerts still work in the same daemon (proves the SLO suppression is not a dead pipe) ---'
    printf 'Deployment alerts delivered: %s\n' "$dep_alerts"
    echo
    echo '--- total notification lines received (0 here is a scenario FAILURE: the receiver never got anything) ---'
    printf 'total notification lines: %s\n' "$total_lines"
    echo
    echo '--- daemon log tail (last 15 lines; diagnoses a failed start without re-running the suite) ---'
    tail -n 15 "$wlog" 2>/dev/null || echo '<no daemon log captured>'
    echo
    echo '--- assertions ---'
    expect_eq "SLO target ratio"                        "$target"      "0.999"
    expect_eq "SLO pages suppressed by the coverage gate" "$slo_alerts"  0
    expect_ge "object alerts still delivered"           "$dep_alerts"  1
    expect_ge "the webhook pipe delivered something"    "$total_lines" 1
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

  # Hoist every counter, and pull the two explain metrics out of $metrics, so
  # the report and the assertions below read the same values.
  local calls_n expl_n firing_n leaks path_lines write_verbs allowed throttled
  calls_n="$(wc -l < "$calls" 2>/dev/null | tr -d ' ')"
  expl_n="$(grep -c '"reason":"explanation"' "$alerts" 2>/dev/null || true)"
  firing_n="$(grep -c '"reason":"new"' "$alerts" 2>/dev/null || true)"
  # The node name is derived, not hardcoded: on a versioned cluster the nodes are
  # named after that cluster, and a stale literal here would quietly match nothing
  # and report every run leak-free.
  #
  # `.*`, not `[^\n]*`. In a POSIX ERE bracket expression a backslash is literal,
  # so `[^\n]` means "any character except a backslash and except the letter n" —
  # and a prompt reading "pod on node …" carries an n long before the leaked
  # token, so the match failed and the count came back 0 no matter what leaked.
  # grep is line-oriented anyway, which is what `[^\n]*` was reaching for.
  leaks="$(grep -cE '"prompt":.*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-z]{6,}|'"$CLUSTER"'-worker)' "$calls" 2>/dev/null || true)"
  path_lines="$(grep -c "127.0.0.1:$sport/v1" "$wlog" || true)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"
  allowed="$(printf '%s\n' "$metrics"   | awk '/^kubeagent_explain_allowed_total/{print $2}')"
  throttled="$(printf '%s\n' "$metrics" | awk '/^kubeagent_explain_throttled_total/{print $2}')"

  {
    echo '--- model calls the daemon actually made (one line per call) ---'
    printf 'calls: %s\n' "$calls_n"
    echo
    echo '--- /explanations ---'
    printf '%s\n' "$expl" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$expl"
    echo
    echo '--- explain metrics ---'
    { grep -E '^kubeagent_explain_' <<<"$metrics" || echo '<no explain series>'; }
    echo
    echo '--- explanation notifications delivered ---'
    printf 'explanation notifications: %s\n' "$expl_n"
    printf 'plain firing notifications: %s\n' "$firing_n"
    echo
    echo '--- egress check: no pod name, pod IP or node name in any prompt ---'
    printf 'prompts leaking pod or node detail: %s\n' "$leaks"
    echo
    echo '--- endpoint redaction check (only scheme://host may appear in logs) ---'
    printf 'log lines naming the endpoint path: %s\n' "$path_lines"
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    printf 'log lines mentioning a write verb: %s\n' "$write_verbs"
    echo
    echo '--- assertions ---'
    expect_eq "model calls made (budget 1)"          "$calls_n"   1
    expect_eq "explanation notifications delivered"  "$expl_n"    1
    expect_ge "plain firing notifications unaffected" "$firing_n" 1
    expect_eq "explain budget admitted one"          "$allowed"   1
    expect_ge "explain budget throttled the rest"    "$throttled" 1
    expect_eq "no prompt leaks pod or node detail"   "$leaks"     0
    expect_eq "no log line carries the endpoint path" "$path_lines" 0
    expect_eq "daemon log mentions no write verb"    "$write_verbs" 0
    expect_contains "the explanation names the stub model" "$expl" "chaos-stub"
  } | record "14. On-incident explanations (budget 1, two objects break)" "expect: exactly 1 model call and exactly 1 explanation notification (reason=explanation) even though two objects break — Deployment/$ns/web and its Service — because --explain-budget 1 admits one and throttles the rest. kubeagent_explain_allowed_total must be 1 and kubeagent_explain_throttled_total at least 1; /explanations must carry one entry with non-empty text and model=chaos-stub, alongside the plain firing notifications which are unaffected. No prompt may contain a pod name, pod IP or node name, no log line may carry the endpoint's path, and no write verb may appear. This scenario uses a local stub endpoint, so it proves the transport, the throttle, the notification shape and the egress discipline — it does not exercise the Anthropic backend, which is covered by unit tests only."

  rm -f "$wlog" "$alerts" "$calls"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_15_multicluster() {   # one daemon, three targets: two names for this cluster and one dead endpoint
  log "scenario 15: multi-cluster hub (labelling, merge, and per-cluster degradation)"
  local ns=chaos-multi port=18094
  local wlog kc wpid i ccluster cuser metrics issues

  wlog="$(mktemp)"; kc="$(mktemp)"

  # A second context pointing at the SAME cluster proves labelling and the
  # cross-cluster merge without paying for a second Kind cluster; a third
  # context pointing at a closed port proves per-cluster degradation. This does
  # NOT test genuinely divergent cluster state — see the verdict text.
  kubectl --context "$CTX" config view --raw --minify --flatten >"$kc"
  ccluster="$(KUBECONFIG="$kc" kubectl config view -o jsonpath='{.contexts[0].context.cluster}')"
  cuser="$(KUBECONFIG="$kc" kubectl config view -o jsonpath='{.contexts[0].context.user}')"
  KUBECONFIG="$kc" kubectl config set-context alias-b --cluster="$ccluster" --user="$cuser" >/dev/null
  KUBECONFIG="$kc" kubectl config set-cluster dead-cluster --server=https://127.0.0.1:1 >/dev/null
  KUBECONFIG="$kc" kubectl config set-context dead --cluster=dead-cluster --user="$cuser" >/dev/null

  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  ./kubeagent watch --kubeconfig "$kc" \
    --context "$CTX" --context alias-b --context dead \
    -n "$ns" --metrics-addr "127.0.0.1:$port" --heartbeat 10s --debounce 2s >"$wlog" 2>&1 &
  wpid=$!
  # Readiness must arrive despite the dead target: ready means every cluster
  # finished a FIRST ATTEMPT, not that every cluster is healthy.
  for i in $(seq 60); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done
  local ready_code
  ready_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz" 2>/dev/null || echo 000)"

  # Break the workload so there is a real issue to see twice, once per label.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 40

  metrics="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null || echo '')"
  issues="$(curl -s "http://127.0.0.1:$port/issues" 2>/dev/null || echo '<unreachable>')"

  kill "$wpid" >/dev/null 2>&1 || true; wait "$wpid" >/dev/null 2>&1 || true

  # Hoist the per-cluster readings out of the grep pipelines, so the report
  # and the assertions below read the same values.
  local clusters_total up_ctx up_alias up_dead issue_ctx issue_alias issue_dead kubeconfig_material write_verbs
  clusters_total="$(printf '%s\n' "$metrics" | awk '/^kubeagent_clusters_total/{print $2}')"
  up_ctx="$(printf   '%s\n' "$metrics" | awk -v c="cluster=\"$CTX\""      '$0 ~ /^kubeagent_cluster_up/ && index($0,c){print $2}')"
  up_alias="$(printf '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"alias-b\""){print $2}')"
  up_dead="$(printf  '%s\n' "$metrics" | awk '$0 ~ /^kubeagent_cluster_up/ && index($0,"cluster=\"dead\""){print $2}')"
  issue_ctx="$(printf   '%s\n' "$metrics" | grep -c "^kubeagent_issue_active{cluster=\"$CTX\"" || true)"
  issue_alias="$(printf '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="alias-b"' || true)"
  issue_dead="$(printf  '%s\n' "$metrics" | grep -c '^kubeagent_issue_active{cluster="dead"' || true)"
  kubeconfig_material="$(grep -cE 'BEGIN CERTIFICATE|client-key-data|client-certificate-data|token:' "$wlog" || true)"
  write_verbs="$(grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true)"

  {
    echo "--- /readyz status code with one target permanently dead ---"
    printf 'HTTP %s\n' "$ready_code"
    echo
    echo '--- per-cluster up/down ---'
    { grep -E '^kubeagent_(cluster_up|clusters_total)' <<<"$metrics" || echo '<no cluster series>'; }
    echo
    echo '--- the same broken workload, once per healthy cluster label ---'
    { grep -E '^kubeagent_issue_active' <<<"$metrics" | grep 'web' || echo '<no active issue series>'; }
    echo
    echo '--- /issues cluster roster ---'
    printf '%s\n' "$issues" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin).get("clusters"), indent=2))' 2>/dev/null \
      || echo '<could not parse /issues>'
    echo
    echo '--- active issues by cluster ---'
    printf '%s\n' "$issues" | python3 -c 'import json,sys,collections; print(dict(collections.Counter(r["cluster"] for r in json.load(sys.stdin).get("active",[]))))' 2>/dev/null \
      || echo '<could not parse /issues>'
    echo
    echo '--- credential check: no kubeconfig material in any log line ---'
    printf 'log lines carrying kubeconfig material: %s\n' "$kubeconfig_material"
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    printf 'log lines mentioning a write verb: %s\n' "$write_verbs"
    echo
    echo '--- daemon log tail (last 15 lines) ---'
    tail -n 15 "$wlog" 2>/dev/null || echo '<no daemon log captured>'
    echo
    echo '--- assertions ---'
    expect_eq "readyz stays 200 with one target dead" "$ready_code" 200
    expect_eq "cluster roster size"                   "$clusters_total" 3
    expect_eq "the real cluster is up"                "$up_ctx"   1
    expect_eq "its second label is up"                "$up_alias" 1
    expect_eq "the unreachable target is down"        "$up_dead"  0
    expect_ge "the broken workload is seen under the real cluster label" "$issue_ctx"   1
    expect_ge "and again under its second label"                         "$issue_alias" 1
    expect_eq "no issue is attributed to the unreachable target"         "$issue_dead"  0
    expect_eq "no log line carries kubeconfig material" "$kubeconfig_material" 0
    expect_eq "daemon log mentions no write verb"       "$write_verbs" 0
  } | record "15. Multi-cluster hub (three targets, one dead)" "expect: /readyz returns HTTP 200 even though the 'dead' target never reaches its API server — readiness means every cluster finished a first attempt, because a NotReady pod leaves its Service endpoints and Prometheus would then stop scraping the clusters that ARE working. kubeagent_clusters_total is 3; kubeagent_cluster_up is 1 for both $CTX and alias-b and 0 for dead. The broken workload appears in kubeagent_issue_active once per healthy cluster label — four lines in all: the Deployment's ErrImagePull and its same-named Service's NoEndpoints (the container-name coupling scenario 12 documents), each under cluster=\"$CTX\" and again under cluster=\"alias-b\" — and the /issues cluster roster lists all three with dead carrying a non-empty error. No log line may carry kubeconfig material, and no write verb may appear. Scope: alias-b is a second NAME for the same cluster, so this proves labelling, the cross-cluster merge and the degradation path — the parts most likely to regress — but it does not exercise genuinely divergent cluster state, which would need a second Kind cluster and is covered by unit tests with independent fake clientsets instead. Every daemon log line must also carry a [<cluster>] prefix; with three interleaved reconcile loops an unprefixed line is a bug."

  rm -f "$wlog" "$kc"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_16_operators() {   # real cert-manager CRDs -> --operators; an unadapted CRD stays absent
  log "scenario 16: operator/CRD adapters (--operators)"
  local ns=chaos-operators
  local cmurl="https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml"
  # cert-manager's three leader-election Leases (cert-manager-controller,
  # cert-manager-cainjector-leader-election, and its sibling -core, one each
  # for the main controller and the two injector loops cainjector runs) all
  # live in kube-system — nothing the cert-manager release manifest declares,
  # so `kubectl delete -f "$cmurl"` at the end of a previous run (below) never
  # removes them, and they survive to the next run. client-go's
  # leader-election library waits a full LeaseDuration (60s, cert-manager's
  # default) from the moment a FRESH process first observes ANY existing lease
  # record before it will steal leadership — it distrusts the record's own
  # renewTime and times the wait from its own first observation instead, no
  # matter how stale that renewTime already is. Until cainjector wins its
  # leases it injects no caBundle into the webhook configuration, so on a warm
  # cluster the ValidatingWebhookConfiguration's caBundle stays empty for
  # about a minute after every reapply and any Certificate admitted in that
  # window is rejected with "x509: certificate signed by unknown authority"
  # (the API server has no CA to validate the webhook's serving certificate
  # against); separately, until the controller wins its lease it processes no
  # Certificate at all, so Ready stays Unknown instead of settling to False.
  # Confirmed live: caBundle took ~88s to populate and Ready took over 90s to
  # settle with the stale leases left in place; both were near-immediate with
  # them deleted first. Deleting all three here lets the incoming pods win
  # leadership immediately, every run.
  kubectl --context "$CTX" -n kube-system delete lease \
    cert-manager-controller \
    cert-manager-cainjector-leader-election \
    cert-manager-cainjector-leader-election-core \
    --ignore-not-found >/dev/null 2>&1 || true
  kubectl --context "$CTX" apply -f "$cmurl" >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n cert-manager rollout status deploy/cert-manager --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n cert-manager rollout status deploy/cert-manager-webhook --timeout=180s >/dev/null 2>&1 || true
  # The webhook Deployment reports Available before its Service always has a
  # ready endpoint — a Certificate applied in that window is rejected with
  # "connect: connection refused" by the API server's admission call. Wait for
  # the endpoint itself, not just the Deployment condition.
  local i
  for i in $(seq 24); do
    kubectl --context "$CTX" -n cert-manager get endpoints cert-manager-webhook \
      -o jsonpath='{.subsets[0].addresses[0].ip}' 2>/dev/null | grep -q . && break
    sleep 5
  done
  # A live endpoint is necessary but not sufficient: the API server also
  # refuses the webhook's serving certificate with "x509: certificate signed
  # by unknown authority" until cainjector has patched its CA into the
  # ValidatingWebhookConfiguration's caBundle (see the leader-election comment
  # above). Wait for that directly — it is the precondition the API server
  # actually enforces — instead of retrying the Certificate apply blind.
  for i in $(seq 24); do
    kubectl --context "$CTX" get validatingwebhookconfiguration cert-manager-webhook \
      -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null | grep -q . && break
    sleep 5
  done
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # A Certificate pointing at an Issuer that does not exist: cert-manager sets
  # Ready=False (observed reason: DoesNotExist, cert-manager v1.16.2) within
  # seconds, with no ACME round trip and no outbound network. The distinctive
  # secretName and commonName are the spec-leak probe below — neither may
  # appear anywhere in the report. Retried as a safety margin: even once the
  # webhook is reachable and its caBundle is populated, the admission server
  # can take another moment to settle.
  for i in $(seq 6); do
    kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'CERT' && break
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: doomed
spec:
  secretName: doomed-tls-chaosonlytoken
  commonName: doomed.chaos.invalid
  dnsNames: [doomed.chaos.invalid]
  issuerRef:
    name: no-such-issuer
    kind: Issuer
CERT
    sleep 5
  done

  # A CRD kubeagent has no adapter for: the discovery gate must leave it out.
  kubectl --context "$CTX" apply -f - >/dev/null <<'CRD'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.chaos.example.com
spec:
  group: chaos.example.com
  scope: Namespaced
  names: { plural: widgets, singular: widget, kind: Widget }
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true }
CRD
  sleep 5
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'WIDGET' || true
apiVersion: chaos.example.com/v1
kind: Widget
metadata: { name: unadapted }
WIDGET

  sleep 20
  local out rc body
  out="$(scan --operators 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  local widget_n spec_n cert_line
  widget_n="$(printf '%s\n' "$body" | grep -c 'Widget' || true)"
  spec_n="$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|doomed\.chaos\.invalid' || true)"
  cert_line="$(printf '%s\n' "$body" | grep -m1 'Certificate' || true)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'unadapted Widget kind in report: %s\n' "$widget_n"
    printf 'CR spec content in report:       %s\n' "$spec_n"
    printf 'Certificate line:                %s\n' "$cert_line"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code" "$rc" 0
    expect_eq       "unadapted CRD stays out of the report" "$widget_n" 0
    expect_eq       "no custom-resource spec content in the report" "$spec_n" 0
    expect_contains "cert-manager Certificate adapter fired" "$cert_line" "Certificate"
    expect_contains "the failing Certificate is counted unhealthy" "$cert_line" "unhealthy"
  } | record "16. Operator/CRD adapters (--operators)" "detected: cert-manager Certificate Ready=False; unadapted CRD absent (0); no CR spec content (0)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete crd widgets.chaos.example.com --wait=false >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete -f "$cmurl" --wait=false >/dev/null 2>&1 || true
}

scenario_02_certs() {   # documented skip (can't force cert expiry on Kind)
  log "scenario 2: expired certificates (skipped)"
  # No assertion here on purpose: this scenario runs no scan and computes no
  # value, so any expect_* call could only compare the skip text with itself.
  # The TLS branch is asserted in internal/connectivity's unit tests instead.
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
  local out rc body
  out="$(scan 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"           "$rc" 0
    expect_contains "OOM workload named"       "$body" "chaos-oom/oom-target"
    expect_contains "OOMKilled diagnosed"      "$body" "OOMKilled"
  } | record "7. OOMKilled critical workload (memory-hog, 64Mi limit)" "detected: OOMKilled + container requests/limits"
  kubectl --context "$CTX" delete ns chaos-oom --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_17_gitops() {   # real Flux -> --drift; a failing and a suspended Kustomization
  log "scenario 17: GitOps drift (--drift)"
  local ns=chaos-gitops
  local fluxurl="https://github.com/fluxcd/flux2/releases/download/v2.4.0/install.yaml"
  kubectl --context "$CTX" apply -f "$fluxurl" >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n flux-system rollout status deploy/source-controller --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n flux-system rollout status deploy/kustomize-controller --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # A GitRepository pointing at a host that cannot resolve: source-controller
  # fails fast with no outbound network and no dependency on a real repo, so the
  # Kustomization below settles on Ready=False within seconds. The token in the
  # URL and the distinctive path are the leak probe — neither may appear anywhere
  # in the report.
  local i
  for i in $(seq 6); do
    kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'GITREPO' && break
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: doomed
spec:
  interval: 30s
  url: https://chaosonlytoken@git.chaos.invalid/org/repo.git
  ref:
    branch: main
GITREPO
    sleep 5
  done

  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'KS' || true
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: doomed
spec:
  interval: 30s
  path: ./overlays/chaosonlytoken
  prune: false
  sourceRef:
    kind: GitRepository
    name: doomed
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: parked
spec:
  suspend: true
  interval: 30s
  path: ./overlays/chaosonlytoken
  prune: false
  sourceRef:
    kind: GitRepository
    name: doomed
KS

  sleep 45
  local out rc body
  # --drift-age 10s so the 45s-old failure classifies as stale rather than as a
  # deploy that is still converging: this exercises the threshold, not just the
  # parser.
  out="$(scan --drift --drift-age 10s 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  local drift_line ks_line doomed_n parked_n leak_n
  drift_line="$(printf '%s\n' "$body" | grep -m1 'GITOPS DRIFT' || true)"
  ks_line="$(printf '%s\n' "$body" | grep -m1 'Kustomization' || true)"
  doomed_n="$(printf '%s\n' "$body" | grep -c "$ns/doomed" || true)"
  parked_n="$(printf '%s\n' "$body" | grep -cE "$ns/parked +suspended" || true)"
  leak_n="$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|git\.chaos\.invalid' || true)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'GITOPS DRIFT section:            %s\n' "$drift_line"
    printf 'Kustomization line:              %s\n' "$ks_line"
    printf 'doomed enumerated:               %s\n' "$doomed_n"
    printf 'parked enumerated as suspended:  %s\n' "$parked_n"
    printf 'repo URL or token in report:     %s\n' "$leak_n"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code" "$rc" 0
    expect_contains "GitOps drift section rendered"   "$drift_line" "GITOPS DRIFT"
    expect_contains "the stale Kustomization is counted" "$ks_line" "stale"
    expect_ge "the failing Kustomization is named"    "$doomed_n" 1
    expect_ge "the suspended Kustomization is named suspended" "$parked_n" 1
    expect_eq "no repo URL or token reaches the report" "$leak_n" 0
  } | record "17. GitOps drift (--drift)" "detected: Flux Kustomization not ready + one suspended; no repo URL or token (0)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete -f "$fluxurl" --wait=false >/dev/null 2>&1 || true
}

scenario_18_capacity() {   # --capacity: structural rules on a cluster with no metrics-server
  log "scenario 18: capacity hints (--capacity)"
  local ns=chaos-capacity
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # Three deliberately wrong shapes, one per structural rule. The 40-core Job can
  # never be scheduled on this cluster, which is the point: the rule proves it from
  # the spec without waiting for a Pending pod.
  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null <<'SHAPES'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: besteffort
spec:
  replicas: 2
  selector: {matchLabels: {app: besteffort}}
  template:
    metadata: {labels: {app: besteffort}}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: limitonly
spec:
  replicas: 1
  selector: {matchLabels: {app: limitonly}}
  template:
    metadata: {labels: {app: limitonly}}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9
          resources:
            limits:
              memory: 256Mi
---
apiVersion: batch/v1
kind: Job
metadata:
  name: trainer
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9
          resources:
            requests:
              cpu: "40"
SHAPES

  kubectl --context "$CTX" -n "$ns" rollout status deploy/besteffort --timeout=90s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n "$ns" rollout status deploy/limitonly --timeout=90s >/dev/null 2>&1 || true

  local out rc body
  out="$(scan --capacity 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  local cap_line cp_n besteffort_n limitonly_n trainer_n nometrics_n banned_n
  cap_line="$(printf '%s\n' "$body" | grep -m1 'CAPACITY' || true)"
  cp_n="$(printf '%s\n' "$body" | grep -cE 'control-plane.*NoSchedule taint' || true)"
  besteffort_n="$(printf '%s\n' "$body" | grep -c "Deployment/$ns/besteffort" || true)"
  limitonly_n="$(printf '%s\n' "$body" | grep -c "Deployment/$ns/limitonly" || true)"
  trainer_n="$(printf '%s\n' "$body" | grep -c "Job/$ns/trainer" || true)"
  nometrics_n="$(printf '%s\n' "$body" | grep -c 'metrics-server unavailable' || true)"
  banned_n="$(printf '%s\n' "$body" | grep -ciE 'peak|over-requested|oversized|waste' || true)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'CAPACITY section:              %s\n' "$cap_line"
    # The old "headroom schedulable" read here used a bare `grep -m1
    # 'schedulable'`, which is a substring of "Unschedulable" and so matched
    # whichever line came first in the report — usually the unrelated
    # Pending/Unschedulable finding for the 40-core Job, not the CAPACITY
    # section's own headroom summary. "control-plane excluded" below, asserted
    # against the precise 'control-plane.*NoSchedule taint' pattern, is the
    # accurate evidence for the same Headroom subsection and replaces it.
    printf 'control-plane excluded:        %s\n' "$cp_n"
    printf 'no requests set (besteffort):  %s\n' "$besteffort_n"
    printf 'limit, no request (limitonly): %s\n' "$limitonly_n"
    printf 'never schedulable (trainer):   %s\n' "$trainer_n"
    printf 'metrics-server unavailable:    %s\n' "$nometrics_n"
    # The "not a peak, not a history" footer only renders when MetricsAvailable is
    # true; with no metrics-server on this cluster that footer never prints, so the
    # only remaining route for any of these words is the structural-rules output
    # itself. 0 here means the rules stayed within the section's own vocabulary
    # rules, not that a peak-shaped phrase happened to be filtered out.
    printf 'no banned vocabulary:          %s\n' "$banned_n"
    # Informational only: the Job's request is genuinely unschedulable on this
    # cluster, so the existing Pending/Unschedulable detector fires a real Finding
    # and the verdict below is expected to read Degraded. CAPACITY is advisory and
    # never itself contributes to this line — that is what this gate is checking.
    printf 'cluster verdict:               %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code" "$rc" 0
    expect_contains "capacity section rendered"        "$cap_line" "CAPACITY"
    expect_ge "control-plane excluded from headroom"   "$cp_n"          1
    expect_ge "no-requests rule fired (besteffort)"    "$besteffort_n"  1
    expect_ge "limit-without-request rule fired (limitonly)" "$limitonly_n" 1
    expect_ge "never-schedulable rule fired (trainer)" "$trainer_n"     1
    expect_ge "absent metrics-server is stated"        "$nometrics_n"   1
    expect_eq "capacity output stays inside its vocabulary" "$banned_n" 0
  } | record "18. Capacity hints (--capacity)" "detected: all three structural rules; metrics-server absent path; banned vocabulary absent (0); CAPACITY itself never drives the verdict (the Pending/Unschedulable finding from the 40-core Job does)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

scenario_19_mcp() {   # kubeagent mcp: the real binary over real stdio, not the fake clientset
  log "scenario 19: MCP server over stdio (kubeagent mcp)"
  local ns=chaos-mcp
  kubectl --context "$CTX" create namespace "$ns" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" run mcp-crasher \
    --image=busybox --restart=Always -- /bin/sh -c 'exit 1' >/dev/null 2>&1 || true

  # Give it long enough to reach CrashLoopBackOff, so the triage call below has a
  # real finding to report instead of an empty, unfalsifiable "healthy".
  local waited=0
  while [ "$waited" -lt 90 ]; do
    if kubectl --context "$CTX" -n "$ns" get pod mcp-crasher \
      -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null |
      grep -q CrashLoopBackOff; then
      break
    fi
    sleep 5
    waited=$((waited + 5))
  done

  local out err
  out="$(mktemp)"
  err="$(mktemp)"
  {
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"chaos","version":"0"}}}'
    printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
    printf '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"kubeagent_triage","arguments":{"namespace":"%s"}}}\n' "$ns"
    # Hold stdin open: closing it while a request is still in flight makes the
    # server exit with "server is closing: EOF" before it answers (measured
    # against this harness — the naive `kubectl ... | ./kubeagent mcp` pipeline
    # without this tail sleep never got a reply).
    sleep 10
  } | ./kubeagent mcp --context "$CTX" >"$out" 2>"$err" || true

  # The SDK serves requests concurrently and a probe run here observed the id-3
  # response land before id-2, so every field below is picked out by response id,
  # never by line position. set -euo pipefail is in effect for the whole script,
  # so both python3 extractions and the greps under them are guarded with
  # `|| true`: a missing or malformed response must record as an empty/0
  # gate-check line for a human to see, not abort the harness before scenario 1
  # (the etcd scenario, which must run last) ever gets a chance to run.
  local tools
  tools="$(python3 -c '
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    if msg.get("id") == 2:
        try:
            print(" ".join(sorted(t["name"] for t in msg["result"]["tools"])))
        except (KeyError, TypeError):
            pass
        break
' "$out" 2>/dev/null || true)"

  # A bare "0" here would be ambiguous: with no id-2 response at all, $tools is
  # empty and a count over it is also 0 — the line would read exactly like a
  # verified pass while nothing was actually checked. The read-only guarantee is
  # the one claim in this scenario that must never be green by accident, so the
  # no-response case records as N/A instead of a number.
  local write_verbs
  if [ -z "$tools" ]; then
    write_verbs="N/A (no tools/list response — nothing was checked)"
  else
    # One name per line, so this counts matching tool NAMES rather than matching
    # lines — over the single space-joined line it could only ever read 0 or 1.
    write_verbs="$(printf '%s\n' "$tools" | tr ' ' '\n' | grep -ciE 'fix|apply|delete|patch|create' || true)"
  fi

  local triage
  triage="$(python3 -c '
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except ValueError:
        continue
    if msg.get("id") == 3:
        try:
            r = msg["result"]["structuredContent"]
            print(r["verdict"], len(r["findings"]), r["coverage"]["context"], sep="|")
        except (KeyError, TypeError):
            pass
        break
' "$out" 2>/dev/null || true)"
  local got_verdict got_findings got_context
  IFS='|' read -r got_verdict got_findings got_context <<<"$triage"

  {
    echo '--- raw stdout (one JSON-RPC response per line) ---'
    cat "$out"
    echo
    echo '--- stderr ---'
    cat "$err"
    printf '\n--- gate checks ---\n'
    printf 'tools/list (id 2) tool names:       %s\n' "$tools"
    printf 'tool names containing a write verb:  %s\n' "$write_verbs"
    printf 'tools/call (id 3) verdict:          %s\n' "${got_verdict:-}"
    printf 'tools/call (id 3) findings count:   %s\n' "${got_findings:-0}"
    printf 'tools/call (id 3) coverage.context: %s\n' "${got_context:-}"
    printf '\n--- assertions ---\n'
    expect_eq "advertised tools" "$tools" "kubeagent_advisory kubeagent_inspect kubeagent_triage"
    expect_eq "no tool name carries a write verb" "$write_verbs" 0
    expect_eq "triage verdict" "${got_verdict:-}" "degraded"
    expect_ge "triage findings" "${got_findings:-0}" 1
    expect_eq "the server's context round-trips into the response" "${got_context:-}" "$CTX"
  } | record "19. MCP server over stdio (kubeagent mcp)" \
    "expect: tools/list (id 2) tool names reads exactly 'kubeagent_advisory kubeagent_inspect kubeagent_triage'; tool names containing a write verb reads 0 — no fix/apply/delete/patch/create verb in any tool name, so the server advertises no path to a cluster write. That line reading N/A is a FAILURE, not a pass: it means no tools/list response arrived and the read-only claim went unchecked; tools/call (id 3) verdict reads degraded (the crash-looping pod is a real finding); tools/call (id 3) findings count is at least 1; tools/call (id 3) coverage.context reads $CTX (the --context the server was started with round-trips into the response)"

  rm -f "$out" "$err"
  kubectl --context "$CTX" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
}

scenario_20_rbac() {   # a real least-privilege identity: the API server actually says no
  log "scenario 20: least-privilege RBAC (kubeagent rbac + a scan-profile-only identity)"
  local ns=chaos-rbac
  kubectl --context "$CTX" create namespace "$ns" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" create serviceaccount scanner \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # The role under test is generated by the binary itself, so this scenario also
  # proves `rbac print` emits something the API server accepts.
  ./kubeagent rbac print --profile scan --role-name chaos-rbac-scan |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" create clusterrolebinding chaos-rbac-scan \
    --clusterrole=chaos-rbac-scan --serviceaccount="$ns:scanner" \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # Build a kubeconfig for that ServiceAccount. It holds a bearer token, so it is
  # a credential: it lives in a temp file, is never printed, and is removed below.
  local kc ca token server
  kc="$(mktemp)"
  ca="$(mktemp)"
  token="$(kubectl --context "$CTX" -n "$ns" create token scanner --duration=1h)"
  server="$(kubectl --context "$CTX" config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  kubectl --context "$CTX" config view --minify --raw \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$ca"
  # A kubeconfig that pins the CA out-of-band carries no certificate-authority-data,
  # and base64 -d on empty input succeeds — so without this branch the scenario would
  # build a kubeconfig pointing at an empty CA file and fail much later, looking like
  # a kubeagent bug rather than a harness one.
  if [ -s "$ca" ]; then
    KUBECONFIG="$kc" kubectl config set-cluster chaos \
      --server="$server" --certificate-authority="$ca" --embed-certs=true >/dev/null
  else
    KUBECONFIG="$kc" kubectl config set-cluster chaos \
      --server="$server" --insecure-skip-tls-verify=true >/dev/null
  fi
  KUBECONFIG="$kc" kubectl config set-credentials chaos --token="$token" >/dev/null
  KUBECONFIG="$kc" kubectl config set-context chaos --cluster=chaos --user=chaos >/dev/null
  KUBECONFIG="$kc" kubectl config use-context chaos >/dev/null

  # rbac check under that identity: core allowed, the three add-ons blocked.
  # It exits 1 by design when anything is blocked, so guard it.
  local check
  check="$(./kubeagent rbac check --kubeconfig "$kc" --features core,certs,logs,diskusage \
    --output json 2>/dev/null || true)"
  # rbac check --output json emits a document object ({"schemaVersion", "features": [...]}),
  # not a bare array — an array root can't carry a version field. A shape that isn't that
  # object prints PARSE-FAILED rather than going quiet, so a future format change shows up
  # as a broken parse in the recorded report instead of silently emptying core_ok/blocked
  # the way a bare `except ValueError: sys.exit(0)` would.
  local core_ok blocked
  core_ok="$(printf '%s' "$check" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    rows = doc["features"]
    for r in rows:
        if r["name"] == "core":
            print(r["allowed"])
            break
except (ValueError, TypeError, KeyError):
    print("PARSE-FAILED")
' 2>/dev/null || true)"
  blocked="$(printf '%s' "$check" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    rows = doc["features"]
    print(" ".join(sorted(r["name"] for r in rows if not r["allowed"])))
except (ValueError, TypeError, KeyError):
    print("PARSE-FAILED")
' 2>/dev/null || true)"

  # The scan itself: three add-on flags the identity cannot serve. It must still
  # succeed and must name what it could not read.
  local out rc
  out="$(mktemp)"
  ./kubeagent scan --kubeconfig "$kc" --certs --logs --disk-usage >"$out" 2>&1 && rc=0 || rc=$?

  local named
  named="$(grep -ciE 'secrets|pods/log|nodes/proxy' "$out" || true)"

  # Same trip-wire scenario 15 runs over its daemon log, here over the scan
  # output this scenario is about to commit into docs/testing/chaos-results.md:
  # a refused read must be named in kubeagent's own words, never in the API
  # server's, which under the built-in RBAC authorizer embeds the requesting
  # identity's bearer token material.
  local leaked
  leaked="$(grep -cE 'BEGIN CERTIFICATE|client-key-data|client-certificate-data|token:' "$out" || true)"

  {
    echo '--- rbac check --output json (feature -> allowed) ---'
    printf '%s\n' "$check"
    printf '\n--- scan under the scan-profile-only identity (exit %s) ---\n' "$rc"
    cat "$out"
    printf '\n--- gate checks ---\n'
    printf 'rbac check: core allowed:            %s\n' "${core_ok:-<no response>}"
    printf 'rbac check: blocked features:        %s\n' "${blocked:-<none>}"
    printf 'scan exit code:                      %s\n' "$rc"
    printf 'blind spots naming a missing grant:  %s\n' "$named"
    printf 'credential material in recorded output: %s (want 0)\n' "$leaked"
    printf '\n--- assertions ---\n'
    expect_eq "core profile is allowed"     "${core_ok:-}" "True"
    expect_eq "exactly the ungranted add-ons are blocked" "${blocked:-}" "certs diskusage logs"
    expect_eq "a missing add-on grant degrades the scan, it does not fail it" "$rc" 0
    expect_ge "refused reads are named as blind spots" "$named" 1
    expect_eq "no credential material in the recorded output" "$leaked" 0
  } | record "20. Least-privilege RBAC (scan-profile-only identity)" \
    "expect: rbac check core allowed reads True — the generated scan-profile role really does cover core; rbac check blocked features reads exactly 'certs diskusage logs' — the three add-ons the identity was never granted, named by kubeagent from its own table and never quoting the API server; scan exit code is 0 — a missing add-on grant degrades the scan, it does not fail it; blind spots naming a missing grant is at least 1 — the report NAMES secrets / pods/log / nodes/proxy as unread rather than printing three empty sections. That last line reading 0 is the failure this scenario exists to catch: a scan that could not see must never look like a scan that saw nothing wrong. credential material in recorded output must read 0 — this scan output is about to be committed into docs/testing/chaos-results.md, and a refused read reported in kubeagent's own words never carries the scanner ServiceAccount's bearer token or certificate material the way the raw API server message would"

  rm -f "$kc" "$ca" "$out"
  unset token
  kubectl --context "$CTX" delete clusterrolebinding chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete clusterrole chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
}

run_scenarios() {
  # 01_etcd runs LAST: stopping the control-plane is the most disruptive fault and
  # etcd/apiserver flap for a while afterwards (and while the API is down even
  # `kubectl wait` can't settle it). Running it last keeps that recovery noise from
  # contaminating the other scenarios' scans.
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 01_etcd)
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
  assert_init
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
  local bout brc bbody
  bout="$(scan 2>&1)" && brc=0 || brc=$?
  bbody="$(scan_body "$bout")"
  {
    printf '%s\n' "$bout"
    printf '\n--- assertions ---\n'
    expect_eq       "baseline scan exit code"          "$brc" 0
    expect_contains "baseline cluster verdict"         "$bbody" "Cluster: Healthy"
    expect_contains "baseline reports nothing to fix"  "$bbody" "No issues found."
  } | record "Baseline (healthy cluster)" "baseline"

  run_scenarios

  log "done — report: $OUT"
  if [ "$TEARDOWN" = 1 ]; then teardown; else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    echo "  kind delete cluster --name $CLUSTER"
  fi

  # Non-zero when any assertion failed: this is what makes the harness a gate.
  assert_summary "$OUT"
}

main
