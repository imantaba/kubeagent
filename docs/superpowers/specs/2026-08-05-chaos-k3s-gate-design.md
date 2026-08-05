# Gating a second Kubernetes distribution in CI — design

**Status:** approved, ready to plan
**Date:** 2026-08-05
**Follows:** `2026-08-05-chaos-portability-seam-design.md` (shipped as v1.4.0)

## The promise this closes

Two pages say the same thing today. `website/docs/roadmap.md` records that
pointing the chaos harness at a distribution is "a hand-run away; **gating one
in CI is still ahead**". `website/docs/compatibility.md` is blunter: "one
distribution (kind), one architecture (amd64), one CNI (Calico), on
`ubuntu-latest` … EKS, GKE, AKS, OpenShift, k3s, and RKE2 are **not gated in
CI**, and nothing on this page claims they are."

This slice gates k3s. After it, the compatibility page names two distributions
in CI and says exactly what the second one covers and what it does not.

## Why k3s, and why the harness creates it

k3s and RKE2 are the only distributions on that list installable on a
GitHub-hosted `ubuntu-latest` runner with no cloud account. EKS, GKE and AKS
need credentials this project does not have and will not get; OpenShift needs a
subscription. Between the two, k3s differs from kind along axes the harness
already reasons about — it ships Traefik and ServiceLB, it ships metrics-server,
and its CNI is Flannel with no NetworkPolicy enforcement — so it exercises three
of the portability seam's four discovered capability probes from the opposite
side. That is the test of whether the seam is real or accidental.

The harness **creates** the k3s cluster rather than being pointed at one. This
is the decision the whole design turns on, so it is worth writing down why the
two cheaper alternatives were rejected.

*Reusing portable mode as-is* would gate k3s at the portable subset — and on k3s
that subset is thinner than the 86 assertions measured against kind, because the
three probes above all go the other way and take scenarios 04, 06 and 18 with
them. It would also mean the six `cluster_write` scenarios are never exercised
on anything but kind, permanently.

*Adding an "I own this cluster" affirmation flag* would re-open `cluster_write`
on a foreign context cheaply. It was rejected because the flag's only purpose is
to disable the safety property the portability seam exists for, and once it
exists an operator can point it at production. The seam's rule — the harness
writes cluster-scoped objects only on a cluster it created — stays absolute,
with no escape hatch.

So the harness gains a second **creation** mode. Ownership keeps meaning exactly
one thing: the harness made this cluster and can delete it.

## The flag

```text
./chaos/run.sh --distro kind|k3s [--k8s-version <minor>] [--recreate] [--teardown]
```

`--distro` defaults to `kind`; every command line written before this slice
keeps its exact meaning. On `--distro k3s` the harness creates a k3d cluster —
k3s running in containers — which gives it the same create/delete lifecycle it
has with kind, a digest-pinnable image, and identical behaviour on a laptop and
on a runner.

`--distro` composes with `--k8s-version`, `--recreate` and `--teardown`: all
three manage the lifecycle of a cluster the harness owns, which it now does.

`--distro` is **refused with `--context`**, exiting 2 with a named reason on
stderr, alongside the three refusals portable mode already has. `--context`
means "a cluster I did not create"; `--distro` means "create one". The refusal
fires in the same place as the existing three — before the version axis derives
any name, so a contradiction is reported before docker is touched.

An unrecognised distro name exits 2 with a message naming what is supported,
before any name is derived from it. This mirrors `chaos_image`'s validate-first
discipline: a value that becomes a cluster name, a context and a report path has
no business being unchecked.

### k3d is a new binary, and only in one place

`preflight()`'s binary list becomes distro-dependent:

| path | binaries |
|------|----------|
| `--distro kind` (default) | `docker kind kubectl helm go curl python3` — unchanged |
| `--distro k3s` | `docker k3d kubectl helm go curl python3` |
| `--context <ctx>` (portable) | `kubectl go curl python3` — unchanged |

Portable mode's list does not grow. That constraint was about portable mode
specifically — the mode that runs on someone else's cluster from someone else's
laptop — and it still holds.

## Capabilities on k3s

The vocabulary stays a closed set of six names. What changes is which of them a
k3s run is granted, and one reason string.

| capability | k3s | why |
|---|---|---|
| `cluster_write` | **granted** | the harness created this cluster and can delete it |
| `node_exec` | **withheld** | k3s has no kubeadm-shaped control plane to exec into |
| `clean_baseline` | decided by the baseline scan | unchanged in both modes |
| `no_metrics_server` | not granted | k3s ships metrics-server |
| `no_loadbalancer` | not granted | k3s ships ServiceLB |
| `netpol_enforced` | not granted | Flannel, and no CNI DaemonSet the probe recognises |

The four discovered capabilities are **not special-cased for k3s**. The probes
run exactly as they do today and reach those answers on their own. That is
deliberate: a probe that had to be told about k3s would prove nothing, and a
probe that is wrong makes its scenario **fail**, not skip — so three skips on
k3s are evidence the probes work.

### Why `node_exec` is withheld, and why its reason changes

Scenario 01 stops etcd; scenario 11 stops kubelet. Both are kubeadm-shaped. k3s
defaults to an embedded sqlite datastore — there is no etcd to stop — and its
kubelet is part of the single k3s process rather than a separate unit. k3d nodes
*are* containers, so `docker exec` would work; granting `node_exec` on that basis
would make both scenarios **run and fail**, which is strictly worse than a named
skip.

So `node_exec` is granted on the kind path only, and its canonical reason string
stops being ownership-shaped and becomes shape-shaped. Today it reads:

> needs shell access to a node container, which exists only on a cluster the
> harness created

which would be false on a k3s run — the harness *did* create it. It becomes a
statement about the control plane's shape instead, true on both a foreign
cluster and a harness-created k3s one:

> needs shell access to a node running a kubeadm-shaped control plane, where
> etcd and kubelet are separately stoppable units

That grounds the refusal in something checkable and stays accurate on a cluster
the harness owns.

Teaching scenarios 01 and 11 k3s-shaped variants (embedded etcd via
`--cluster-init`, stopping the k3s agent process) was considered and rejected
for this slice: it is a much larger change, and on a small k3s cluster stopping
embedded etcd takes the API server with it, which is a different test than the
one scenario 01 writes today.

### Expected shape of a k3s run

Six scenarios skip, each naming its reason in the assertion summary:

| scenario | skips because |
|---|---|
| 01 etcd | `node_exec` |
| 11 kubelet | `node_exec` |
| 04 networkpolicy | `netpol_enforced` |
| 06 loadbalancer | `no_loadbalancer` |
| 18 capacity | `no_metrics_server` |
| 02 certs | the unconditional documented skip, as on kind |

Seventeen scenarios run, including all six `cluster_write` ones. The assertion
count that run produces is **measured during the build**, not predicted here.

## Harness changes beyond the flag

Each of these is a real difference between the two distributions, not
scaffolding.

**`worker_node()`** greps node names for the literal `worker`. kind names its
workers `<cluster>-worker`, `<cluster>-worker2`; k3d names its agents
`k3d-<cluster>-agent-0`. The function switches to selecting a node that does not
carry the control-plane role label, which is correct on both. Scenario 03
(cordon) is `cluster_write`-guarded and therefore runs on k3s, so this is
load-bearing, not cosmetic.

**Calico** is kind-only. `preload_calico_images` and `install_calico` do not run
on the k3s path — the cluster ships Flannel and the harness leaves it alone.

**`preload_flux_images`** side-loads with `kind load docker-image`. On the k3s
path it uses `k3d image import` instead. The preload exists because Flux's six
controllers otherwise pull serially and scenario 17's Kustomization has not
reconciled by the time its rollout wait expires — that failure mode is a
property of a node's containerd store, not of kind, so it applies to k3d too.
The helper stays best-effort in both cases.

**`wait_system_ready`** waits on `deploy/coredns` in both. Its other two waits —
`deploy/calico-kube-controllers` and `local-path-storage/local-path-provisioner`
— are kind-only shapes; k3s runs its own local-path-provisioner in `kube-system`
and has no Calico controller. The wait becomes distro-aware so that neither
distribution waits for a Deployment the other one has.

**`check_inotify_limits`** warns on host-wide inotify budget and refuses to start
when the budget is low *and* another cluster is already up. The warning is
distro-neutral; the "other clusters" query becomes distro-aware (`kind get
clusters` on one path, `k3d cluster list` on the other), since it is exactly the
same failure with the same fix.

**The report header** is distro-specific. The kind form hardcodes "Kind %s,
Calico CNI, 1 control-plane + 2 workers"; the k3s form names k3d's version, the
k3s server version, and the stock add-ons that make three probes answer the way
they do. Neither form carries a node name.

**Derived names** follow the version axis's existing pattern: a distro suffix
joins the minor suffix, so `--distro k3s --k8s-version v1.34` and
`--distro kind --k8s-version v1.34` can coexist on one machine without
colliding on the cluster name, the context, the report path or the CoreDNS
scratch file. The report defaults to `docs/testing/chaos-results-k3s*.md`.

**`chaos/versions.env`** gains a digest-pinned `rancher/k3s` image for each
supported minor, resolved the same way the kind images were and documented in
`chaos/README.md` the same way. `chaos/versions.sh` gains a `chaos_k3s_image
<minor>` beside `chaos_image`, refusing an unsupported minor identically. Images
are defined for all three supported minors even though CI runs one, so an
operator can run any of them by hand.

## Two risks the local run must settle

Named here rather than discovered during the build:

1. **k3s re-applies its add-on manifests.** k3s deploys CoreDNS from an
   auto-deploying manifest directory. Scenarios 05 (bad Corefile) and 22 (DNS
   health) both modify the CoreDNS ConfigMap. If k3s reverts those edits, the
   scenarios fail rather than skip. The end-to-end run on this machine is what
   answers it. If it happens, the accommodation is scenario-level and is one of
   exactly two: re-assert the edit after k3s reverts it, or drive the change
   through k3s's own supported CoreDNS customization path. Withholding a
   capability to mute the two scenarios is **not** an option — `cluster_write`
   is granted honestly on a cluster the harness owns, and a capability must
   never be repurposed as a per-scenario mute switch.
2. **ServiceLB assigns node IPs as external addresses.** Scenario 06 is skipped,
   so nothing asserts on the address. Nothing else in the harness reads a
   LoadBalancer address except the `no_loadbalancer` probe, which already reads
   it and discards it without printing — an external address is exactly the kind
   of value this project does not write into a forwarded artifact. This must
   stay true on k3s.

## CI

`chaos-matrix.yml` gains a distribution dimension. The existing per-minor cells
become `distro: kind`; one cell is added via `include:` — the newest supported
minor, `distro: k3s`. That minor is resolved from `chaos/versions.env` rather
than typed into the workflow, so it cannot go stale when the supported set
moves; today it is `v1.34`. Four cells, all parallel, so wall-clock does not
move. One workflow, one credential-scan step, one artifact upload, one place
to read a failure.

A full cross product (k3s × every minor) was rejected: the kind axis already
covers the minor dimension and the k3s axis covers the distribution one, so the
extra cells would double runner cost for mostly redundant signal. A sibling
workflow was rejected because it duplicates the runner setup, the credential
scan and the upload, and those copies drift.

The tool-preflight step installs k3d pinned by SHA256, the way kind already is,
and only on the k3s cell. `ANTHROPIC_API_KEY` remains unset for the whole
workflow. The credential scan runs over the k3s report exactly as it does over
the kind ones — the harness owns the k3s cluster, so no redaction applies, but
the scan is what proves that rather than an assumption.

## Numbers, and where they live

**134 does not move.** It is the kind cell's assertion count and it stays as
written in `CLAUDE.md`, `chaos/README.md`, `website/docs/compatibility.md` and
`website/docs/roadmap.md`. What changes is that compatibility.md's "134
machine-checked assertions per cell" sentence becomes explicitly a statement
about the kind axis rather than about every cell.

The k3s cell's measured count is published in **two** places only —
`website/docs/compatibility.md` and `chaos/README.md`, the two documents that
describe what CI covers — named alongside the six scenarios it skips and why.
It does not go into `CLAUDE.md` or the roadmap: a number in four places is a
number that goes stale in two of them.

If any task finds itself editing 134, that is a defect in the task.

## Testing

**Pure shell, no cluster** (`chaos/assert-selftest.sh`, the established
precedent, still sub-second — its own check count is not published):

- an unrecognised `--distro` value exits 2 and names what is supported
- `--distro` together with `--context` is refused, exit 2, named reason
- `--distro k3s` with `--k8s-version`, `--recreate`, `--teardown` is accepted
- derived names differ between distros for the same minor — cluster, context,
  report path and CoreDNS scratch file
- `chaos_k3s_image` returns a digest-pinned image for every supported minor and
  refuses an unsupported one, with no partial answer on the failure path
- the reworded `node_exec` reason is present and does not claim ownership

`worker_node` gets no selftest check: it calls `kubectl` against a live cluster,
so there is nothing in it a cluster-free selftest can exercise. Its correctness
on both distributions is established by the two end-to-end runs below, where
scenario 03 cordons a node on each.

**End-to-end on this machine, before the CI workflow is trusted:**

- the full kind run stays green at 134 assertions, 0 failed, 1 scenario skipped
- a full k3s run completes and its assertion count, failures and skip list are
  measured — the six expected skips and nothing else
- the k3s report carries no node name, no context name and no external address

k3d is a single downloadable binary, so both runs are reproducible here. Nothing
in this design is provable only by a green CI badge. The CI-only unknowns are
the runner's inotify budget and its image-pull rate — properties of the runner,
not of the design.

## What this slice does not do

- It does not gate RKE2, EKS, GKE, AKS or OpenShift, and no document may imply
  otherwise.
- It does not add an ownership-affirmation flag, now or as a follow-up.
- It does not teach scenarios 01 and 11 k3s-shaped variants.
- It does not write the mirror-image assertions for scenarios 06 and 18 — the
  other side of ServiceLB and metrics-server. That is a named follow-up, and it
  would move the published assertion count.
- It changes no Go code. `go.mod` and `go.sum` do not move, the six versioned
  JSON documents do not move, and `internal/report/testdata/golden-scan.txt`
  stays byte-identical.

## Constraints carried from the project

- kubeagent is read-only toward the cluster. The chaos **harness** deliberately
  is not — it injects outages. These are never merged into one sentence, and
  "read-only" is never blurred into "makes no external calls": read-only
  describes cluster operations, making no LLM call is a separate, stronger
  claim.
- No chaos helper returns non-zero for a recorded outcome. A failing assertion
  lets the remaining scenarios run and surfaces only at the end, in the exit
  code.
- `assert_summary`'s exit status stays the gate: non-zero if and only if an
  assertion failed. A skip is never a failure.
- `scenario_01_etcd` stays last in `run_scenarios()`, and the `all=(...)` list
  does not change.
- No secrets, credentials, private IPs or internal hostnames anywhere —
  results file, CI artifacts, workflow files, README and every doc example.
  RFC 5737 addresses, RFC 2606 domains. Kubeconfig paths, kubeconfig context
  names, Kubernetes node names and external load-balancer addresses are
  credentials. Nothing emitted carries more than `scheme://host`.
- `ANTHROPIC_API_KEY` is never set, referenced or exported by the harness or the
  workflow. `explain_flag()` gates on its presence and does not change.
- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`); `main` enforces DCO. No AI attribution anywhere.
