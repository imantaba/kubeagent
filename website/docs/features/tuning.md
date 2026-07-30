# Performance tuning

`kubeagent scan` reads a lot of a cluster in one go — pods and every controller
kind, events, services, endpoint slices, ingresses, PVCs, namespaces, PDBs,
HPAs, webhook configurations, PVs, storage classes, quotas and network
policies, plus a per-node or per-pod read for each add-on you enable. Those
reads are independent of one another, so kubeagent issues them together.

Everything on this page is read-only. None of it changes what a scan reports —
only how quickly it gets there.

## `KUBEAGENT_SCAN_WORKERS`

How many of the scan's independent reads may be in flight at once.

| | |
|---|---|
| Default | `8` |
| Range | `1`–`64`, clamped to the nearer bound |
| Bad value | Ignored — the default is used, and the scan still runs |

```bash
KUBEAGENT_SCAN_WORKERS=16 kubeagent scan --disk-usage --kubelet-health
```

Raise it on a large cluster where the scan is dominated by per-node reads; lower
it to `1` to reproduce the old strictly-sequential behaviour, which is
occasionally useful when you are trying to attribute API-server load.

A worker cap is self-limiting in a way a request rate is not. When the API
server slows down, every worker is blocked on its own in-flight request, so
kubeagent slows down with it. A fixed request rate holds the same number whether
the server is idle or dying.

**Under `kubeagent watch`, this multiplies.** The daemon runs one goroutine per
watched cluster, so a daemon watching four clusters at the default cap may have
up to 32 reads in flight — eight against each of four different API servers.
Nothing is shared between them, so per-server load is still eight; it is the
daemon's own file-descriptor and memory use that scales.

## `KUBEAGENT_QPS` and `KUBEAGENT_BURST`

A client-side request rate limit. **Unset, kubeagent applies none**, which is
the default and the recommended setting.

| | |
|---|---|
| `KUBEAGENT_QPS` | Requests per second. Must be a positive, finite number; anything else — including `Inf` — is ignored |
| `KUBEAGENT_BURST` | Bucket size. Must be a positive integer; only takes effect alongside `KUBEAGENT_QPS` |

```bash
KUBEAGENT_QPS=20 KUBEAGENT_BURST=40 kubeagent scan
```

### Why the default is "no client-side limit"

client-go installs a 5 requests-per-second, burst-10 token bucket on **each
per-API-group client** when a program leaves `QPS` unset. Nearly every read a
scan makes goes through the core API group, so that one bucket metered the whole
scan.

Measured on a three-node cluster, `scan` with every add-on enabled:

| | Wall clock |
|---|---|
| With client-go's default limiter | 2.42 s |
| With no client-side limiter | 0.15 s |

Byte-identical output, and no concurrency involved in either run — that is
purely the limiter.

Load shedding belongs on the server, where the information is. Kubernetes
API Priority and Fairness (`flowcontrol.apiserver.k8s.io/v1`, GA) queues and
sheds by request class based on what the API server can actually take;
a client-side rate cannot know that. kubeagent's reads are all `get` and `list`,
so APF classifies them accordingly.

### When to set one anyway

- A shared cluster whose administrators have asked every client for a request
  budget.
- An API server without APF configured, where a client-side limit is the only
  brake available.
- Reproducing a support case that was captured under a specific rate.

Set `KUBEAGENT_QPS` alone and client-go applies its own default burst; set both
to control the bucket precisely.
