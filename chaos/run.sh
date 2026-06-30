#!/usr/bin/env bash
set -euo pipefail

# kubeagent chaos-test harness — reproduces common production outages on a
# disposable Kind cluster and runs `kubeagent scan` against each, writing a
# results report for pre-release review. Targets ONLY its own Kind context.

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

install_calico() {
  log "install Calico CNI"
  kubectl --context "$CTX" apply -f chaos/manifests/calico.yaml
  # First run pulls large Calico images (cni/node) serially — allow generous time.
  kubectl --context "$CTX" -n kube-system rollout status ds/calico-node --timeout=600s
  kubectl --context "$CTX" wait --for=condition=Ready nodes --all --timeout=600s
}

# Append --explain ONLY when a key is present in the environment (never logged).
explain_flag() { [ -n "${ANTHROPIC_API_KEY:-}" ] && echo "--explain" || true; }
# scan [extra args...] — runs kubeagent scan against the chaos context.
scan() { ./kubeagent scan --context "$CTX" "$@" $(explain_flag); }

# record <title> <verdict> ; reads scan output from stdin.
record() {
  { printf '\n## %s\n\n_Verdict: %s_\n\n```text\n' "$1" "$2"; cat; printf '```\n'; } >> "$OUT"
}

teardown() { log "teardown"; kind delete cluster --name "$CLUSTER"; }

# Scenarios are registered here; later tasks add scenario_NN_* functions.
run_scenarios() { :; }

main() {
  preflight
  build_kubeagent
  create_cluster
  install_calico

  mkdir -p "$(dirname "$OUT")"
  : > "$OUT"
  {
    printf '# kubeagent chaos-test results\n\n'
    printf -- '- Cluster: Kind %s, Calico CNI, 1 control-plane + 2 workers\n' "$(kind version 2>/dev/null | awk '{print $2}')"
    printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
    printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
  } >> "$OUT"

  log "baseline healthy scan"
  scan 2>&1 | record "Baseline (healthy cluster)" "baseline"

  run_scenarios

  log "done — report: $OUT"
  if [ "$TEARDOWN" = 1 ]; then teardown; else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    echo "  kind delete cluster --name $CLUSTER"
  fi
}

main
