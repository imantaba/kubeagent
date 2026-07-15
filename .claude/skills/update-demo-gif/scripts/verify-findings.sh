#!/usr/bin/env bash
# verify-findings.sh — the "test" that runs before recording the demo GIF.
#
# The GIF is only worth recording once the cluster is actually showing the spread
# of failures we injected. Kubernetes takes a little while to reach CrashLoopBackOff
# / ImagePullBackOff / OOMKilled after the workloads are applied, so this polls a
# real `kubeagent scan` until every expected finding is present (or times out).
# Exit 0 = ready to record; exit 1 = something never showed up (don't record a
# half-baked cluster).
#
# Usage: verify-findings.sh [context] [kubeagent-binary]
#   context           kube-context to scan (default: kind-kubeagent-demo)
#   kubeagent-binary  path to the built binary (default: ./kubeagent)
set -uo pipefail

CTX="${1:-kind-kubeagent-demo}"
BIN="${2:-./kubeagent}"

# The finding substrings kubeagent prints for each injected fault.
want=("ImagePullBackOff" "CrashLoopBackOff" "OOMKilled" "no ready endpoints")

for attempt in $(seq 1 20); do   # up to ~100s
  out="$("$BIN" scan --context "$CTX" 2>&1 || true)"
  missing=()
  for w in "${want[@]}"; do
    grep -q "$w" <<<"$out" || missing+=("$w")
  done
  if [ "${#missing[@]}" -eq 0 ]; then
    echo "OK — all expected findings present after ${attempt} check(s)."
    exit 0
  fi
  echo "[$((attempt * 5))s] still waiting on: ${missing[*]}"
  sleep 5
done

echo "TIMED OUT — these findings never appeared: ${missing[*]}" >&2
echo "--- last scan output ---" >&2
echo "$out" >&2
exit 1
