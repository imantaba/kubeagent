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
CAPS=""   # the capabilities this run has; see the capability block below
PORTABLE=0; CONTEXT=""   # --context selects portable mode; see the block below
NODE_SED=""   # sed script redacting node names from portable-mode report text

while [ $# -gt 0 ]; do
  case "$1" in
    --teardown) TEARDOWN=1 ;;
    --recreate) RECREATE=1 ;;
    --only) ONLY="$2"; shift ;;
    --out) OUT="$2"; shift ;;
    --k8s-version) K8S_VERSION="$2"; shift ;;
    --context) CONTEXT="$2"; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac; shift
done

# Normalize a numeric --only to the zero-padded form used in scenario keys (01..23).
# 10# forces base 10: printf reads a leading-zero numeral as octal, so a plain
# --only 08 or --only 09 errored and normalized to 00, silently matching nothing.
if [ -n "$ONLY" ] && printf '%s' "$ONLY" | grep -qE '^[0-9]+$'; then ONLY=$(printf '%02d' "$((10#$ONLY))"); fi

# Portable mode. --context runs the namespaced-only subset of the suite against
# a cluster the harness did NOT create: it creates and deletes chaos-* namespaces
# and nothing else, and every scenario that would write a cluster-scoped object
# or shell into a node refuses to run.
#
# Three flags are REFUSED rather than ignored. All three manage a kind cluster's
# lifecycle or its node image, and silently accepting one against someone else's
# production cluster is exactly the trap this mode exists to avoid. They fire
# here, before the version axis derives any name, so an operator who typed a
# contradiction learns it before docker is touched.
if [ -n "$CONTEXT" ]; then
  PORTABLE=1
  if [ "$RECREATE" = 1 ]; then
    echo "--context and --recreate are mutually exclusive: the harness will not delete and rebuild a cluster it does not own" >&2
    exit 2
  fi
  if [ "$TEARDOWN" = 1 ]; then
    echo "--context and --teardown are mutually exclusive: the harness deletes only clusters it created" >&2
    exit 2
  fi
  if [ -n "$K8S_VERSION" ]; then
    echo "--context and --k8s-version are mutually exclusive: the version axis picks a kind node image, which says nothing about a cluster that already exists" >&2
    exit 2
  fi
  CTX="$CONTEXT"
  : "${OUT:=docs/testing/chaos-results-portable.md}"
fi

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

# portable_preflight — what the harness must know before it touches a cluster it
# does not own. Every one of these exits rather than degrading: a chaos run that
# started against the wrong cluster cannot be undone by noticing later.
#
# The binary list is SHORTER than preflight()'s, not longer: portable mode
# creates no kind cluster and side-loads no image, so kind and docker are not
# needed at all.
#
# The context name appears on stderr here and nowhere else. It is a credential
# under this project's rules — on a managed cluster it is routinely an ARN or a
# project/region path — but stderr is the operator's own channel, read by the
# person who typed the name, and a preflight failure that will not say which
# cluster it means is not actionable. It never reaches $OUT.
portable_preflight() {
  local b existing nslist probe=chaos-preflight list_rc=0

  for b in kubectl go curl python3; do
    command -v "$b" >/dev/null || { echo "missing required tool: $b" >&2; exit 1; }
  done

  kubectl config get-contexts "$CTX" >/dev/null 2>&1 || {
    printf 'no such context in the kubeconfig: %s\n' "$CTX" >&2
    printf 'List them with: kubectl config get-contexts\n' >&2
    exit 1
  }

  kubectl --context "$CTX" version -o json >/dev/null 2>&1 || {
    printf 'context %s exists but the cluster did not answer\n' "$CTX" >&2
    printf 'The credentials may be expired, or the API server may be unreachable from here.\n' >&2
    exit 1
  }

  # Debris from an aborted run, or a second run already in progress. Either way
  # the scenarios below would collide with it, and deleting someone else's
  # namespace unasked is not the harness's call to make.
  #
  # The list call is captured on its own line, separately from the grep that
  # follows: under pipefail, a `kubectl get ns` that FAILS and a `kubectl get
  # ns` that legitimately finds no chaos-* namespace both end the pipeline on
  # grep's no-match exit status, indistinguishable without the `|| true`
  # already needed for the second, legitimate case. Judging the list call on
  # its own status, before grep ever runs, is what tells them apart — an
  # identity that can create and delete a namespace by name but cannot list
  # them cluster-wide must not be waved through as "no debris found".
  nslist="$(kubectl --context "$CTX" get ns -o name 2>/dev/null)" || list_rc=$?
  if [ "$list_rc" -ne 0 ]; then
    printf 'refusing to start: could not list namespaces on the target cluster.\n' >&2
    printf 'The portable subset must know whether chaos-* debris is already present, and it will not proceed blind.\n' >&2
    exit 1
  fi
  existing="$(printf '%s\n' "$nslist" | sed 's|^namespace/||' | grep '^chaos-' | tr '\n' ' ' || true)"
  existing="${existing% }"
  if [ -n "$existing" ]; then
    {
      printf 'refusing to start: chaos-* namespaces already exist on the target cluster:\n'
      printf '  %s\n' "$existing"
      printf 'They are debris from an aborted run, or another run is in progress. Delete them first:\n'
      printf '  kubectl --context %s delete ns %s\n' "$CTX" "$existing"
    } >&2
    exit 1
  fi

  # A round trip, not a SelfSubjectAccessReview: what matters is that this
  # identity can actually create AND delete a namespace here, which is the only
  # write the portable subset performs.
  log "portable preflight: namespace create/delete round trip"
  kubectl --context "$CTX" create ns "$probe" >/dev/null 2>&1 || {
    printf 'refusing to start: cannot create a namespace on the target cluster.\n' >&2
    printf 'The portable subset creates and deletes chaos-* namespaces; it needs that permission.\n' >&2
    exit 1
  }
  kubectl --context "$CTX" delete ns "$probe" --wait=true --timeout=120s >/dev/null 2>&1 || {
    printf 'refusing to start: created namespace %s but could not delete it again.\n' "$probe" >&2
    printf 'Delete it by hand before re-running.\n' >&2
    exit 1
  }
}

# portable_sweep — delete any chaos-* namespace still standing at the end of a
# portable run, so a scenario that failed part way through does not leave a
# broken workload on a cluster the harness does not own.
#
# Best-effort by design, and it must never return non-zero: it runs before
# assert_summary, and a sweep that aborted the script would swallow the very
# exit code the run exists to produce.
portable_sweep() {
  log "sweep leftover chaos-* namespaces"
  local ns
  for ns in $(kubectl --context "$CTX" get ns -o name 2>/dev/null \
                | sed 's|^namespace/||' | grep '^chaos-' || true); do
    kubectl --context "$CTX" delete ns "$ns" --wait=false >/dev/null 2>&1 \
      || log "sweep: could not delete namespace $ns"
  done
  return 0
}

# probe_capabilities — decide the capabilities that are facts about the TARGET
# CLUSTER rather than policy.
#
# It runs in both modes. On kind every one of these is present, which is what
# keeps the gate path's assertion count unchanged AND exercises the probe code
# on the path that gates releases — a probe only ever run in portable mode would
# be a probe nothing tests.
probe_capabilities() {
  log "probe cluster capabilities"

  # no_metrics_server — scenario 18 asserts the structural-rules-only capacity
  # path. On a cluster with metrics-server, kubeagent takes the metrics path
  # instead and the assertion would fail rather than skip: a false alarm, which
  # is strictly worse than a stated gap.
  if ! kubectl --context "$CTX" get apiservice v1beta1.metrics.k8s.io >/dev/null 2>&1; then
    capability_add no_metrics_server
  fi

  # netpol_enforced — scenario 4 needs a CNI that actually enforces
  # NetworkPolicy. This is a HEURISTIC with exactly two failure modes: an
  # enforcing CNI whose DaemonSet is not on this list gives a false SKIP (safe
  # — the summary names it), and a listed CNI deliberately configured not to
  # enforce gives a false FAILURE in scenario 4. There is no cheap probe that
  # avoids both, and a wrong guess in the safe direction is the one to prefer.
  local ds
  for ds in calico-node cilium weave-net kube-router antrea-agent; do
    if kubectl --context "$CTX" -n kube-system get ds "$ds" >/dev/null 2>&1; then
      capability_add netpol_enforced
      break
    fi
  done

  # no_loadbalancer — scenario 6 asserts a LoadBalancer Service never gets an
  # external address. A cluster with a provider assigns one within seconds and
  # the assertion would fail rather than skip.
  #
  # The Service selects nothing on purpose: a provider assigns an address to a
  # Service with no endpoints just the same, so there is no image to pull and
  # nothing to schedule. The address, if one arrives, is read and discarded —
  # it is never printed, because an external address is exactly the kind of
  # thing this project does not write into a forwarded artifact.
  local pns=chaos-probe addr="" i
  kubectl --context "$CTX" create ns "$pns" --dry-run=client -o yaml \
    | kubectl --context "$CTX" apply -f - >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n "$pns" apply -f - >/dev/null 2>&1 <<'PROBE' || true
apiVersion: v1
kind: Service
metadata:
  name: probe
spec:
  type: LoadBalancer
  selector:
    app: chaos-probe-selects-nothing
  ports:
    - port: 80
      targetPort: 80
PROBE
  for i in $(seq 30); do
    addr="$(kubectl --context "$CTX" -n "$pns" get svc probe \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
    [ -n "$addr" ] && break
    sleep 1
  done
  [ -n "$addr" ] || capability_add no_loadbalancer
  kubectl --context "$CTX" delete ns "$pns" --wait=true --timeout=120s >/dev/null 2>&1 || true

  log "capabilities: ${CAPS:-<none>}"
}

# portable_header — describe the target cluster in the report WITHOUT naming it.
#
# A kubeconfig context name is a credential under this project's rules, and on a
# managed cluster it is routinely an ARN or a project/region path. Node names are
# no better. What a reader of this report actually needs is the platform, and
# nodeInfo gives that precisely and impersonally: an OS image reading "Amazon
# Linux 2" or "Flatcar Container Linux" identifies the distribution far better
# than a context name would, and identifies no account.
#
# Values are deduplicated: a 300-node cluster running one image should produce
# one line, not 300.
portable_header() {
  local nodes
  nodes="$(kubectl --context "$CTX" get nodes -o json 2>/dev/null | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
items = doc.get("items", [])

def uniq(field):
    seen = []
    for n in items:
        v = n.get("status", {}).get("nodeInfo", {}).get(field, "")
        if v and v not in seen:
            seen.append(v)
    return ", ".join(seen) or "unknown"

print("- Nodes: %d" % len(items))
print("- OS image: %s" % uniq("osImage"))
print("- Container runtime: %s" % uniq("containerRuntimeVersion"))
print("- Kubelet: %s" % uniq("kubeletVersion"))
' 2>/dev/null || true)"
  [ -n "$nodes" ] || nodes='- Nodes: unknown'

  printf '# kubeagent chaos-test results (portable mode)\n\n'
  printf -- '- Mode: portable — an existing cluster the harness did not create and will not delete\n'
  printf -- '- Kubernetes: %s\n' "$(kubectl --context "$CTX" version -o json 2>/dev/null \
    | python3 -c 'import sys,json; print(json.load(sys.stdin).get("serverVersion",{}).get("gitVersion",""))' 2>/dev/null)"
  printf '%s\n' "$nodes"
  printf -- '- explain: %s\n' "$([ -n "${ANTHROPIC_API_KEY:-}" ] && echo enabled || echo 'disabled (no ANTHROPIC_API_KEY)')"
}

# portable_node_redaction — build the sed script that keeps node names out of $OUT.
#
# A node name is a credential under this project's rules: on EKS it is routinely
# an ip-10-x-x-x.ec2.internal, a private address and an internal hostname in one
# string. kubeagent's own scan output names nodes on purpose — an operator
# reading their terminal needs to know which node to fix — but this harness
# copies that text into a file meant to be read elsewhere, so it redacts on the
# way in.
#
# Names are DNS-1123 subdomains, so `.` is the only metacharacter to escape.
# Substitutions are applied longest-first: with both `node1` and `node10`
# present, the short name would otherwise eat the prefix of the long one and
# leave a stray `0` behind. The number in the placeholder comes from the sorted
# order instead, so `<node-1>` names the same node on every run.
portable_node_redaction() {
  local names
  names="$(kubectl --context "$CTX" get nodes -o name 2>/dev/null)" || names=""
  NODE_SED="$(printf '%s\n' "$names" | sed 's|^node/||' | LC_ALL=C sort | python3 -c '
import sys
names = [l.strip() for l in sys.stdin if l.strip()]
place = {n: i + 1 for i, n in enumerate(names)}
parts = []
for n in sorted(names, key=len, reverse=True):
    parts.append("s/%s/<node-%d>/g" % (n.replace(".", "\\."), place[n]))
print(";".join(parts))
' 2>/dev/null || true)"
  if [ -z "$NODE_SED" ]; then
    printf 'refusing to start: could not read the node list on the target cluster.\n' >&2
    printf 'Portable mode redacts node names from the report, and it will not write one it cannot redact.\n' >&2
    exit 1
  fi
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

# preload_flux_images <manifest> — side-loads Flux's controller images into the
# Kind nodes, for exactly the reason preload_calico_images exists: a Kind node has
# its own containerd store and the kubelet serializes image pulls, so on a cold
# cluster the six ghcr.io controllers pull one after another. Measured on this
# harness: helm-controller alone took 4m01s, and source-controller and
# kustomize-controller — the only two scenario 17 needs — were still pulling after
# their rollout waits had already timed out. The scenario then scanned a Flux that
# had never reconciled anything, so its Kustomization read "Ready not reported"
# and the `stale` assertion failed on a missing precondition rather than on
# anything kubeagent did.
#
# Best-effort, like the Calico preload: a failed pull or load falls back to the
# in-node pull, and the precondition assertions in the scenario catch what that
# fallback does not manage in time.
preload_flux_images() {
  log "preload Flux images into $CLUSTER nodes"
  local ref
  for ref in $(grep -hoE 'ghcr\.io/fluxcd/[A-Za-z0-9._/-]+:[A-Za-z0-9._-]+' "$1" | sort -u); do
    docker image inspect "$ref" >/dev/null 2>&1 || docker pull "$ref" || { echo "preload: pull $ref failed; falling back to in-node pull" >&2; continue; }
    # Unlike the Calico refs there is no prefix to strip: docker only normalizes
    # away docker.io, so a ghcr.io ref is tagged locally under its full name and
    # `kind load` puts it in the node store under the name the manifest asks for.
    kind load docker-image "$ref" --name "$CLUSTER" || echo "preload: load $ref failed; falling back to in-node pull" >&2
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

# json_get <document> <dotted path> — the value at a dotted path in a JSON scan
# report, or a marker in place of one: "<absent>" when the path is not in the
# document, "<unparseable>" when the document is not JSON at all.
#
# It never fails, so a caller under `set -euo pipefail` does not lose the run at
# the assignment. It never prints an empty string either: an omitempty section
# that is simply not there and a report that failed to render both read as ""
# otherwise, and an assertion cannot tell "the flag correctly emitted nothing"
# from "the scan produced nothing". The markers say which.
json_get() {
  printf '%s' "$1" | python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except ValueError:
    print("<unparseable>"); sys.exit(0)
for key in sys.argv[1].split("."):
    if not isinstance(doc, dict) or key not in doc:
        print("<absent>"); sys.exit(0)
    doc = doc[key]
print(doc)
' "$2" 2>/dev/null || printf '<unparseable>\n'
}

# worker_node — the name of the first worker node.
#
# Two scenarios cordon or docker-exec into a worker by name. Reading it with a
# bare `kubectl get nodes | grep worker` fails badly when nothing matches: under
# `set -o pipefail` the unmatched grep aborts the run at the assignment, with no
# message and no assertion summary, so a single-node or renamed cluster looks
# like a harness crash. Diagnose it instead — this is an environment problem, in
# the same class as a missing binary, not a kubeagent finding.
worker_node() {
  local n
  n="$(kubectl --context "$CTX" get nodes -o name | grep -m1 worker | cut -d/ -f2)" || true
  if [ -z "$n" ]; then
    {
      printf 'no worker node found in context %s.\n' "$CTX"
      printf 'The harness creates 1 control-plane + 2 workers; scenarios 3 and 11 need one.\n'
      printf 'Re-create the cluster with: %s --recreate\n' "$0"
    } >&2
    exit 1
  fi
  printf '%s\n' "$n"
}

# redact_nodes — filter report text through the portable-mode node redaction.
#
# In kind mode there is nothing to redact: the harness created those nodes and
# named them itself, and NODE_SED is empty, so this is a passthrough and the
# kind path's bytes do not move. In portable mode NODE_SED is never empty —
# portable_node_redaction exits rather than let it be.
redact_nodes() {
  if [ -z "$NODE_SED" ]; then cat; else sed -E "$NODE_SED"; fi
}

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
  } | redact_nodes >> "$OUT"
}

# --- capabilities -----------------------------------------------------------
#
# A scenario declares what it needs; the run decides what it has. The
# vocabulary is a CLOSED SET of six names, each with exactly one reason string,
# so two guards on the same capability can never describe it differently in two
# reports.
#
# Two of them are POLICY, not probe. The question `cluster_write` and
# `node_exec` answer is not whether the harness COULD write a cluster-scoped
# object or shell into a node — with an admin kubeconfig against a managed
# cluster it very often could — but whether it MAY. On a cluster the harness
# created and can delete, it may. On a cluster it merely has credentials for, it
# may not, and the refusal is the safety property this whole seam exists for.
#
# The other four are facts about the target cluster, decided by probe_capabilities
# and by the baseline scan.

# capability_reason <name> — the one canonical reason a scenario is skipped for
# want of this capability. Returns 1 for a name outside the vocabulary.
capability_reason() {
  case "$1" in
    node_exec)         printf 'needs shell access to a node container, which exists only on a cluster the harness created\n' ;;
    cluster_write)     printf 'writes cluster-scoped objects, which the harness will not do on a cluster it does not own\n' ;;
    clean_baseline)    printf 'asserts whole-cluster health, which is only meaningful on a cluster that reported none before the run\n' ;;
    no_loadbalancer)   printf 'asserts a LoadBalancer Service never gets an address, which is false on a cluster with a provider\n' ;;
    no_metrics_server) printf 'asserts the structural-rules-only path, which metrics-server would take instead\n' ;;
    netpol_enforced)   printf 'needs a CNI that enforces NetworkPolicy\n' ;;
    *) return 1 ;;
  esac
}

# capability_add <name> — mark a capability available for this run. Idempotent,
# and it validates: a typo here would silently switch a scenario ON, which is
# the failure mode this seam must never have.
capability_add() {
  capability_reason "$1" >/dev/null || {
    printf 'capability_add: unknown capability %s\n' "$1" >&2
    exit 2
  }
  case " $CAPS " in *" $1 "*) ;; *) CAPS="${CAPS:+$CAPS }$1" ;; esac
}

# requires <capability> — the guard clause at the top of a scenario body:
#
#   scenario_05_coredns() {
#     requires cluster_write || return 0
#     log "scenario 5: ..."
#
# Returns 0 when the capability is available and the scenario proceeds
# unchanged. Otherwise it records the skip three ways — in $SKIPLOG so the
# summary counts it, in $OUT so the report names it, and on the console so a
# watching operator sees it — and returns 1, so `|| return 0` leaves the
# scenario without having touched the cluster.
#
# An unknown capability name EXITS rather than skipping. A silent skip on a
# typo'd guard would look exactly like a passing run, which is precisely the
# defect this seam was built to remove.
requires() {
  local cap="$1" reason title
  reason="$(capability_reason "$cap")" || {
    printf 'requires: unknown capability %s (called from %s)\n' "$cap" "${FUNCNAME[1]}" >&2
    exit 2
  }
  case " $CAPS " in *" $cap "*) return 0 ;; esac
  title="$(scenario_title "${FUNCNAME[1]}")"
  assert_skip "$title" "$reason"
  printf 'Skipped: %s\n' "$reason" | record "$title" "skipped ($reason)"
  return 1
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
  local node; node="$(worker_node)"
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
  local node; node="$(worker_node)"
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
  local dash dash_code dash_type dash_webhook
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
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h --dashboard >"$wlog" 2>&1 &
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

  # The dashboard is a face on the same state /issues just served, captured at
  # the same moment so the two cannot disagree about what was firing.
  dash_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/dashboard" 2>/dev/null || echo 000)"
  dash_type="$(curl -s -o /dev/null -w '%{content_type}' "http://127.0.0.1:$port/dashboard" 2>/dev/null || true)"
  dash="$(curl -s "http://127.0.0.1:$port/dashboard" 2>/dev/null || echo '<unreachable>')"
  # Count, never quote: assert.sh embeds a needle in its own PASS/FAIL line, so
  # an expect_absent here would write the endpoint into the report on every
  # passing run — the leak the assertion exists to rule out. The webhook
  # redaction check twenty lines above and scenario 20 both count for the same
  # reason.
  dash_webhook="$(printf '%s\n' "$dash" | grep -cF -- "127.0.0.1:$aport" || true)"

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
    echo '--- dashboard served while the outage was firing ---'
    printf 'status: %s\ncontent type: %s\npage bytes: %s\n' \
      "$dash_code" "$dash_type" "$(printf '%s' "$dash" | wc -c | tr -d ' ')"
    printf 'page lines naming the webhook endpoint host: %s\n' "$dash_webhook"
    echo
    echo '--- assertions ---'
    expect_contains "NEW transition for the broken Deployment" "$transitions" "NEW Deployment/$ns/web"
    expect_contains "RESOLVED transition after the repair"     "$transitions" "RESOLVED Deployment/$ns/web"
    expect_ge "firing notifications delivered"      "$firing_n"   1
    expect_eq "distinct objects alerted"            "$distinct_n" 2
    expect_eq "resolved notifications (one per object)" "$resolved_n" 2
    expect_eq "daemon log mentions no write verb"   "$write_verbs" 0
    expect_eq       "dashboard returns 200 while an incident is firing" "$dash_code" 200
    expect_contains "dashboard content type is HTML"                    "$dash_type" "text/html"
    expect_contains "dashboard names the broken workload"               "$dash"      "$ns/web"
    # A webhook URL is a credential, and this scenario is the only place in the
    # suite where the daemon actually holds one. Asserting it never reaches the
    # page is a stronger test of the "URLs are credentials" rule than a generic
    # path grep would be, because the value is really there to leak. The
    # assertion carries a count rather than the endpoint itself, so a passing
    # run does not print what it just proved absent.
    expect_eq       "dashboard body carries no alert webhook URL"       "$dash_webhook" 0
  } | record "12. Stateful watch daemon (NEW on outage, RESOLVED on repair, /issues)" "expect: one NEW line naming Deployment/$ns/web, one RESOLVED line with the firing duration, the incident listed under /issues while firing and under resolved afterwards, and exactly one resolved alert per broken object — two objects break here (Deployment/$ns/web and its Service), so two objects alert and each resolves once. The Deployment's firing alert must survive the whole Degraded -> ErrImagePull -> ImagePullBackOff walk without a resolved notification, even though the per-issue transition log reports RESOLVED for each superseded mode. The dashboard is served from the same snapshot at the same moment: 200, HTML, naming the broken workload, and carrying none of the webhook URL the daemon holds in memory."

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
  # Fetch once to a per-cluster scratch file: the preload has to read the image
  # refs out of the same manifest that gets applied, and two minors running on one
  # machine must not share it (the same reason COREDNS_BACKUP is per-cluster).
  local fluxyaml="/tmp/$CLUSTER-flux-install.yaml"
  if curl -fsSL "$fluxurl" -o "$fluxyaml" && [ -s "$fluxyaml" ]; then
    preload_flux_images "$fluxyaml"
    kubectl --context "$CTX" apply -f "$fluxyaml" >/dev/null 2>&1 || true
  else
    echo "flux manifest fetch failed; applying from the release URL without a preload" >&2
    kubectl --context "$CTX" apply -f "$fluxurl" >/dev/null 2>&1 || true
  fi
  # Generous now only to cover a preload miss falling back to an in-node pull —
  # with the images side-loaded these return in seconds.
  kubectl --context "$CTX" -n flux-system rollout status deploy/source-controller --timeout=300s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n flux-system rollout status deploy/kustomize-controller --timeout=300s >/dev/null 2>&1 || true

  # Everything below claims something about what a reconciler DID. Capture whether
  # the two controllers that would do it are actually up, and assert it beside the
  # rest — so a Flux that never started names itself instead of surfacing as a
  # confusing "the Kustomization is not counted stale".
  local sc_ready kc_ready
  sc_ready="$(ready_replicas flux-system source-controller)"
  kc_ready="$(ready_replicas flux-system kustomize-controller)"

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
    printf 'source-controller ready:         %s\n' "$sc_ready"
    printf 'kustomize-controller ready:      %s\n' "$kc_ready"
    printf 'GITOPS DRIFT section:            %s\n' "$drift_line"
    printf 'Kustomization line:              %s\n' "$ks_line"
    printf 'doomed enumerated:               %s\n' "$doomed_n"
    printf 'parked enumerated as suspended:  %s\n' "$parked_n"
    printf 'repo URL or token in report:     %s\n' "$leak_n"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
    printf '\n--- assertions ---\n'
    expect_ge "precondition: source-controller is reconciling"    "$sc_ready" 1
    expect_ge "precondition: kustomize-controller is reconciling" "$kc_ready" 1
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

  # A crash-looping pod, started now so it is in CrashLoopBackOff by the time the
  # scan runs. Without one, --logs proves nothing: kubeagent reads logs only for a
  # pod it already has a finding about, so on a healthy cluster it never attempts
  # the read, never gets refused, and never names pods/log. The scenario would
  # then pass while covering three of the four refusals it exists to check.
  kubectl --context "$CTX" -n "$ns" run rbac-crasher \
    --image=busybox --restart=Always -- /bin/sh -c 'exit 1' >/dev/null 2>&1 || true

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

  # The scan itself: five add-on flags, four of which the identity cannot serve
  # (--control-plane-health is the exception; see the assertions below). It must
  # still succeed and must name what it could not read. Wait for the crasher
  # first, so the pods/log refusal is actually reached rather than skipped for
  # want of a finding to investigate.
  local waited=0
  while [ "$waited" -lt 90 ]; do
    if kubectl --context "$CTX" -n "$ns" get pod rbac-crasher \
      -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null |
      grep -q CrashLoopBackOff; then
      break
    fi
    sleep 5
    waited=$((waited + 5))
  done

  local out rc
  out="$(mktemp)"
  ./kubeagent scan --kubeconfig "$kc" --certs --logs --disk-usage \
    --control-plane-health --dns-health >"$out" 2>&1 && rc=0 || rc=$?

  local named body
  # Count BLIND SPOTS lines, not every mention. A bare word count also matches the
  # CERTIFICATES and DNS section lines, so it stayed at its floor even when a
  # refusal went unnamed — an assertion that cannot go red for the reason its
  # label gives. This pattern matches only the rendered blind-spot line.
  named="$(grep -cE '^ *• (secrets|pods/log|nodes/proxy|pods/proxy): forbidden' "$out" || true)"
  body="$(cat "$out")"

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
    expect_ge "refused reads are named as blind spots" "$named" 4
    expect_contains "the --certs refusal is named"      "$body" "• secrets: forbidden"
    expect_contains "the --logs refusal is named"       "$body" "• pods/log: forbidden"
    expect_contains "the --disk-usage refusal is named" "$body" "• nodes/proxy: forbidden"
    expect_contains "the --dns-health refusal is named" "$body" "• pods/proxy: forbidden"
    # --control-plane-health is passed to the scan above but is NOT a refused read
    # here: the stock system:public-info-viewer ClusterRoleBinding grants get on
    # /readyz to system:authenticated, so even this identity reads it and the probe
    # reports a healthy control plane. Asserting a blind spot would be asserting a
    # bug. The other half of the same contract is what must hold — a read that
    # succeeded is never reported as one kubeagent could not make.
    expect_absent "a read that succeeded is not named as a blind spot" "$body" "/readyz: forbidden"
    expect_eq "no credential material in the recorded output" "$leaked" 0
  } | record "20. Least-privilege RBAC (scan-profile-only identity)" \
    "expect: rbac check core allowed reads True — the generated scan-profile role really does cover core; rbac check blocked features reads exactly 'certs diskusage logs' — the three add-ons the identity was never granted, named by kubeagent from its own table and never quoting the API server; scan exit code is 0 — a missing add-on grant degrades the scan, it does not fail it; all four refusals are named, one blind-spot line each for secrets (--certs), pods/log (--logs), nodes/proxy (--disk-usage) and pods/proxy (--dns-health), rather than four empty sections. A scan that could not see must never look like a scan that saw nothing wrong. The pods/log line is why this scenario runs a crash-looping pod: kubeagent reads logs only for a pod it has a finding about, so on a healthy namespace that refusal is never even attempted and the assertion would pass without ever testing it. --control-plane-health is passed too and is deliberately the counter-example: a stock cluster grants get on /readyz to system:authenticated through the system:public-info-viewer binding, so that read is not refused and must not be named as a blind spot — the same contract read the other way round. credential material in recorded output must read 0 — this scan output is about to be committed into docs/testing/chaos-results.md, and a refused read reported in kubeagent's own words never carries the scanner ServiceAccount's bearer token or certificate material the way the raw API server message would"

  rm -f "$kc" "$ca" "$out"
  unset token
  kubectl --context "$CTX" delete clusterrolebinding chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete clusterrole chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
}

scenario_21_controlplane() {   # --control-plane-health: the apiserver /readyz probe, live
  log "scenario 21: control-plane readiness probe (--control-plane-health)"
  # No fault to inject. The failing branch of this probe is not reachable from
  # outside the control plane: the only apiserver /readyz check an operator can
  # break is etcd, and breaking etcd also takes every read the scan depends on
  # with it — that is scenario 1, where the answer is a connectivity diagnosis
  # and no report at all. What is worth proving live, and what no unit test can
  # prove, is the other half: that the nonResourceURL GET this flag rests on
  # really is issued against a real apiserver, is really granted, and really
  # comes back ready — and that a ready control plane is reported by saying
  # nothing rather than by inventing a section. The unhealthy and forbidden
  # classifications stay with internal/controlplane's unit tests: the refusal is
  # not reachable live either, because the stock system:public-info-viewer
  # ClusterRoleBinding grants get on /readyz to system:authenticated, so no
  # identity a chaos scenario can build is refused it. Scenario 20 asserts the
  # consequence of that instead — no blind-spot line for a read that succeeded.
  local out rc body json plain status ungated
  out="$(scan --control-plane-health 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  json="$(scan --control-plane-health --output json 2>/dev/null || true)"
  plain="$(scan --output json 2>/dev/null || true)"
  status="$(json_get "$json" controlPlane.status)"
  ungated="$(json_get "$plain" controlPlane.status)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'scan exit code:                       %s\n' "$rc"
    printf 'controlPlane.status with the flag:    %s\n' "$status"
    printf 'controlPlane.status without the flag: %s\n' "$ungated"
    printf '\n--- assertions ---\n'
    expect_eq     "scan exit code"                                 "$rc" 0
    expect_eq     "the /readyz probe ran and read ready"           "$status" "ok"
    expect_absent "a ready control plane renders no section"       "$body" "CONTROL PLANE  (opt-in)"
    expect_absent "a ready control plane makes no not-ready claim" "$body" "control plane not ready"
    expect_eq     "the probe is opt-in: no controlPlane key without the flag" "$ungated" "<absent>"
  } | record "21. Control-plane readiness probe (--control-plane-health)" \
    "expect: controlPlane.status reads ok — the flag issued a real GET against the live apiserver's /readyz, the grant the scan profile hands out covered it, and the response classified as ready. That is the half of this probe a unit test cannot reach: a wrong path, a client that never sent the request, or a nonResourceURL missing from the generated role all look identical to a passing unit test and all show up here as a status that is not ok. The text report is asserted to stay silent — a healthy control plane earns no CONTROL PLANE section and no 'control plane not ready' line — and without the flag the JSON carries no controlPlane key at all, which is what makes the ok above evidence that the probe ran rather than evidence that the key is always there. NOT covered here, or anywhere live: the unhealthy and forbidden classifications. The only apiserver readyz check reachable from outside is etcd, and stopping etcd takes the reads the report is built from with it, so that path is scenario 1's connectivity diagnosis; and the refusal cannot be staged at all, because the stock system:public-info-viewer binding grants get on /readyz to system:authenticated. Both branches rest on internal/controlplane's unit tests"
}

scenario_22_dnshealth() {   # --dns-health: CoreDNS up and Ready, answering SERVFAIL
  log "scenario 22: DNS resolving to SERVFAIL (--dns-health)"
  local ns=chaos-dns
  # A Corefile that keeps CoreDNS healthy and answers every query SERVFAIL. That
  # is the outage --dns-health exists for and the one scenario 5 cannot reach:
  # scenario 5 crash-loops CoreDNS, and a CoreDNS that is down serves no /metrics
  # to read, so the response-ratio check never runs at all. Here the pods stay
  # Ready, /metrics stays up, and resolution is broken anyway — DNS up but not
  # resolving, which is exactly the case a liveness check misses.
  local corefile patch
  corefile='.:53 {
    errors
    health {
       lameduck 5s
    }
    ready
    template ANY ANY {
      rcode SERVFAIL
    }
    prometheus :9153
}
'
  patch="$(printf '%s' "$corefile" | python3 -c 'import json,sys; print(json.dumps({"data":{"Corefile":sys.stdin.read()}}))')"
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge -p "$patch" >/dev/null
  # Restart rather than wait for CoreDNS's own reload: it applies the new Corefile
  # at a known moment, and it resets coredns_dns_responses_total, so the ratio the
  # scan reads is this scenario's traffic instead of the whole run's history.
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  kubectl --context "$CTX" -n kube-system rollout status deploy coredns --timeout=180s >/dev/null

  # Drive enough queries to clear dnshealth's 100-response floor. Below it the
  # verdict is "ok" whatever the ratio, deliberately: a handful of answers is not
  # evidence of anything. Each lookup expands through the pod's search domains and
  # asks for two record types, so 200 iterations is several hundred responses.
  kubectl --context "$CTX" create namespace "$ns" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" create job dns-probe --image=busybox -- \
    /bin/sh -c 'i=0; while [ $i -lt 200 ]; do nslookup probe.example.com >/dev/null 2>&1; i=$((i+1)); done' >/dev/null
  kubectl --context "$CTX" -n "$ns" wait --for=condition=complete job/dns-probe --timeout=180s >/dev/null 2>&1 || true

  local out rc body json plain status total ungated
  out="$(scan --dns-health 2>&1)" && rc=0 || rc=$?
  body="$(scan_body "$out")"
  json="$(scan --dns-health --output json 2>/dev/null || true)"
  plain="$(scan --output json 2>/dev/null || true)"
  status="$(json_get "$json" dns.status)"
  total="$(json_get "$json" dns.totalResponses)"
  ungated="$(json_get "$plain" dns.status)"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'scan exit code:               %s\n' "$rc"
    printf 'dns.status with the flag:     %s\n' "$status"
    printf 'dns.totalResponses:           %s\n' "$total"
    printf 'dns.status without the flag:  %s\n' "$ungated"
    printf '\n--- assertions ---\n'
    expect_eq       "scan exit code"                            "$rc" 0
    expect_ge       "enough responses to judge"                 "$total" 100
    expect_eq       "the probe classifies DNS as degraded"      "$status" "degraded"
    expect_contains "the DNS section is rendered"               "$body" "DNS  (opt-in)"
    expect_contains "the failure is named, not just counted"    "$body" "cluster DNS is failing to resolve"
    expect_contains "the ratio is quantified"                   "$body" "SERVFAIL+REFUSED ratio"
    expect_eq       "the check is opt-in: no dns key without the flag" "$ungated" "<absent>"
  } | record "22. DNS resolving to SERVFAIL (--dns-health)" \
    "expect: dns.status reads degraded off a CoreDNS that is Ready, passing its own health and ready probes, and serving /metrics — the outage a pod-liveness view of DNS cannot see and the reason this check reads response counters rather than pod status. dns.totalResponses must clear 100 first: dnshealth refuses to judge a ratio below that floor, so an assertion on the verdict without one would pass on a cluster where nothing had asked a question yet. The rendered report must name the failure in kubeagent's own words ('cluster DNS is failing to resolve') and quantify it (the SERVFAIL+REFUSED ratio), not merely print a number; exit code stays 0 because the DNS section is advisory, the same contract every opt-in section keeps. Without the flag the JSON carries no dns key at all, which is what makes the degraded above evidence that the probe ran. Scenario 5 covers the other DNS outage — CoreDNS itself crash-looping — and cannot reach this one: a CoreDNS that is down has no /metrics endpoint to read"

  # Restore the pristine Corefile (captured in main()) via a clean merge-patch.
  local restore; restore=$(python3 -c 'import json,sys; print(json.dumps({"data":{"Corefile":open(sys.argv[1]).read()}}))' "$COREDNS_BACKUP")
  kubectl --context "$CTX" -n kube-system patch cm coredns --type=merge -p "$restore" >/dev/null
  kubectl --context "$CTX" -n kube-system rollout restart deploy coredns >/dev/null
  kubectl --context "$CTX" -n kube-system rollout status deploy coredns --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
}

scenario_23_pagerduty() {   # PagerDuty receiver: trigger on outage, resolve on repair, one dedup_key, no key in the log
  log "scenario 23: PagerDuty alert format (trigger/resolve on one dedup_key)"
  local ns=chaos-pagerduty port=18096 aport=18097 wlog wpid i events apid
  wlog="$(mktemp)"
  events="$(mktemp)"
  # The receiver stands in for events.pagerduty.com. KUBEAGENT_ALERT_WEBHOOK
  # overrides the default endpoint, which is what makes this format testable at
  # all without reaching PagerDuty.
  python3 chaos/alert-receiver.py "$aport" "$events" >/dev/null 2>&1 &
  apid=$!
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  # The routing key is a credential and has no flag: it comes from the
  # environment, exactly like the webhook URL beside it. This value is a fixture,
  # not a key — no PagerDuty account is contacted by this scenario.
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  KUBEAGENT_ALERT_ROUTING_KEY="not-a-real-routing-key" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format pagerduty --alert-repeat 1h >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done

  # Same outage as scenario 9: a bad image, with the old replicas taken down by
  # the rollout so Ready < Desired.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 30

  # Repair and let the tracker observe the issue clear.
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:1.27-alpine >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=120s >/dev/null 2>&1 || true
  sleep 30

  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true

  # Every capture is `|| true` guarded: grep exits 1 on zero matches, and under
  # `set -euo pipefail` a bare assignment that fails would abort the scenario
  # before the namespace cleanup at the bottom ever ran.
  local trigger_n resolve_n event_n keyed_n web_dedup_n resolve_dedup key_in_log
  trigger_n="$(grep -c '"event_action":"trigger"' "$events" 2>/dev/null || true)"
  resolve_n="$(grep -c '"event_action":"resolve"' "$events" 2>/dev/null || true)"
  event_n="$(grep -c '"event_action"' "$events" 2>/dev/null || true)"
  # The routing key must be in every body. Counting both sides and comparing two
  # numbers keeps the key itself out of the assertion label.
  keyed_n="$(grep -cF -- '"routing_key":"not-a-real-routing-key"' "$events" 2>/dev/null || true)"
  # The property the whole format rests on: one object, one incident key, across
  # its trigger and its resolve. Not a count of all keys — how many other objects
  # break alongside the Deployment depends on timing, and asserting that number
  # would be asserting the scheduler rather than the encoder. Scoped to
  # Deployment/$ns/web specifically: the Service of the same name genuinely
  # fires its own dedup_key too (svchealth correctly flags it as a real,
  # non-expected NoEndpoints issue while the Deployment has zero ready pods),
  # and an unscoped suffix match would count that second, distinct object's
  # key alongside the Deployment's.
  web_dedup_n="$(grep -o "\"dedup_key\":\"[^/]*/Deployment/$ns/web\"" "$events" 2>/dev/null | sort -u | wc -l || true)"
  resolve_dedup="$(grep '"event_action":"resolve"' "$events" 2>/dev/null | grep -o '"dedup_key":"[^"]*"' | sort -u || true)"
  # Count, never quote: assert.sh embeds its needle in the PASS/FAIL line, so an
  # expect_absent here would write the routing key into the report on every
  # passing run — the leak this assertion exists to rule out.
  key_in_log="$(grep -cF -- "not-a-real-routing-key" "$wlog" || true)"

  {
    echo '--- PagerDuty events delivered to the receiver ---'
    { grep -o '"event_action":"[a-z]*","dedup_key":"[^"]*"' "$events" || echo '<no events delivered>'; }
    echo
    printf 'trigger events: %s\n' "$trigger_n"
    printf 'resolve events: %s\n' "$resolve_n"
    printf 'events carrying a routing key: %s of %s\n' "$keyed_n" "$event_n"
    printf 'distinct dedup keys for %s/web: %s\n' "$ns" "$web_dedup_n"
    echo
    echo '--- routing-key redaction check (the daemon log must never carry it) ---'
    printf 'daemon log lines carrying the routing key: %s\n' "$key_in_log"
    echo
    echo '--- assertions ---'
    expect_ge       "trigger events delivered"                        "$trigger_n"   1
    expect_ge       "resolve events delivered after the repair"       "$resolve_n"   1
    expect_eq       "every delivered event carries the routing key"   "$keyed_n"     "$event_n"
    expect_eq       "the Deployment fires on exactly one dedup key"   "$web_dedup_n" 1
    expect_contains "the resolve carries the Deployment's dedup key"  "$resolve_dedup" "Deployment/$ns/web"
    expect_eq       "daemon log carries no routing key"               "$key_in_log"  0
  } | record "23. PagerDuty receiver (trigger on outage, resolve on repair, one dedup_key per object)" "expect: the daemon posts Events API v2 bodies to a local stand-in for events.pagerduty.com — a trigger while the Deployment is broken, a resolve after the repair, and both on the same identity-derived dedup_key, which is what makes a daemon restart re-trigger onto the open incident instead of opening a second one. The routing key travels in the request body only: it is in every delivered event and in no line of the daemon's log."

  rm -f "$wlog" "$events"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}

run_scenarios() {
  # 01_etcd runs LAST: stopping the control-plane is the most disruptive fault and
  # etcd/apiserver flap for a while afterwards (and while the API is down even
  # `kubectl wait` can't settle it). Running it last keeps that recovery noise from
  # contaminating the other scenarios' scans.
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 21_controlplane 22_dnshealth 23_pagerduty 01_etcd)
  for s in "${all[@]}"; do
    if [ -z "$ONLY" ] || [ "$ONLY" = "${s%%_*}" ]; then "scenario_$s"; fi
  done
}

main() {
  if [ "$PORTABLE" = 1 ]; then
    portable_preflight
    portable_node_redaction
    build_kubeagent
  else
    preflight
    build_kubeagent
    create_cluster
    preload_calico_images
    install_calico
  fi

  mkdir -p "$(dirname "$OUT")"
  : > "$OUT"
  assert_init
  if [ "$PORTABLE" = 1 ]; then
    portable_header >> "$OUT"
  else
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

    # POLICY, not probe: on a cluster the harness created and can delete, it may
    # shell into a node container and write cluster-scoped objects. On a cluster
    # it merely holds credentials for, it may not — however wide those
    # credentials happen to be.
    capability_add node_exec
    capability_add cluster_write
  fi

  log "baseline healthy scan"
  local bout brc bbody btitle bverdict
  bout="$(scan 2>&1)" && brc=0 || brc=$?
  bbody="$(scan_body "$bout")"
  btitle="Baseline (healthy cluster)"; bverdict="baseline"
  if [ "$PORTABLE" = 1 ]; then
    btitle="Baseline"
    bverdict="baseline — on a cluster the harness does not own the verdict is recorded, not asserted"
  fi
  {
    printf '%s\n' "$bout"
    printf '\n--- assertions ---\n'
    if [ "$PORTABLE" = 1 ]; then
      # An operator's cluster is very likely NOT clean, through no fault of
      # kubeagent, so asserting "Cluster: Healthy" here would manufacture a
      # failure out of someone else's backlog. What is true of any conformant
      # cluster is asserted; the verdict itself is recorded in the block above
      # for a human to read.
      expect_eq       "baseline scan exit code"             "$brc" 0
      expect_contains "baseline rendered a cluster verdict"  "$bbody" "Cluster:"
    else
      expect_eq       "baseline scan exit code"          "$brc" 0
      expect_contains "baseline cluster verdict"         "$bbody" "Cluster: Healthy"
      expect_contains "baseline reports nothing to fix"  "$bbody" "No issues found."
    fi
  } | record "$btitle" "$bverdict"

  # clean_baseline is decided by the baseline scan in BOTH modes: scenario 8
  # asserts the WHOLE cluster reads healthy again after a namespace is deleted,
  # which is only checkable on a cluster that read healthy to begin with. On kind
  # it always does — and if it ever does not, the baseline assertions above have
  # already failed the gate.
  if printf '%s\n' "$bbody" | grep -qF "Cluster: Healthy" \
     && printf '%s\n' "$bbody" | grep -qF "No issues found."; then
    capability_add clean_baseline
  fi

  # probe_capabilities runs AFTER the baseline: its LoadBalancer probe creates
  # and deletes a namespace, and a namespace still terminating during the
  # baseline scan would dirty a verdict the gate depends on.
  probe_capabilities

  run_scenarios

  log "done — report: $OUT"
  if [ "$PORTABLE" = 1 ]; then
    portable_sweep
    echo "portable run finished against an existing cluster; nothing was deleted but the harness's own chaos-* namespaces."
  elif [ "$TEARDOWN" = 1 ]; then
    teardown
  else
    echo "cluster left up ($CTX). Re-run with --teardown to delete, or:"
    echo "  kind delete cluster --name $CLUSTER"
  fi

  # assert_summary is a second writer to $OUT, alongside record(): its FAIL
  # block quotes each failed assertion's detail text verbatim, and a scenario
  # that ever hands a node name to expect_contains as its needle would carry
  # that name into the report by a route redact_nodes never sees if this
  # called the append directly. chaos/assert.sh cannot call redact_nodes
  # itself — it knows nothing of the cluster or the report by design, which is
  # what lets chaos/assert-selftest.sh source it alone and stay cluster-free —
  # so the filtering happens here instead: write the summary to a scratch
  # file nobody else reads, then redact THAT into $OUT.
  #
  # Non-zero when any assertion failed: this is what makes the harness a gate,
  # and that status must survive the detour. A skipped scenario is counted and
  # named in the summary but never changes it.
  local sumfile rc=0
  sumfile="$(mktemp)"
  assert_summary "$sumfile" || rc=$?
  redact_nodes < "$sumfile" >> "$OUT"
  rm -f "$sumfile"
  return "$rc"
}

# main() runs only on a direct execution. chaos/assert-selftest.sh sources this
# file to exercise the pure helpers above — the capability table, requires — with
# no cluster and no docker, which is the only way those get a test at all.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then main; fi
