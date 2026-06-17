# kubeagent — Design (v1)

A Kubernetes troubleshooting tool written in Go. v1 is a **deterministic
diagnostic CLI** that scans a whole cluster, finds unhealthy pods, and explains
why they're failing. A later version adds a single Claude API call to summarize
findings in plain English.

This project has two equal goals:

1. **Build a genuinely useful tool** — something you'd actually run against a
   cluster.
2. **Learn Go** — every design choice favors learning idiomatic Go. See
   [go-concepts.md](go-concepts.md) for the running cheat-sheet.

---

## Scope

### v1 (this design)

- **Invocation:** `kubeagent scan` — scans the **whole cluster** (all namespaces).
- **Failure modes detected:**
  - **CrashLoopBackOff** — container repeatedly crashes after starting.
  - **ImagePullBackOff / ErrImagePull** — bad image reference or registry auth.
  - **OOMKilled** — container hit its memory limit and was killed.
  - **Pending / Unschedulable** — no node can place the pod (resources, taints,
    affinity).
- **Output:** human-readable text by default; `--output json` for machines.
- **Read-only.** The tool only *reads* cluster state (list pods, read status,
  list events). It never creates, edits, or deletes anything.

### Out of scope for v1 (YAGNI)

- LLM integration (deferred to v2 — see Roadmap).
- Per-pod targeting (`diagnose <pod>`) — v1 scans everything.
- Concurrency (goroutines) — v1 is sequential for clarity.
- Probe-failure and config-error detectors — easy to add later via the same
  `Detector` interface, but not in the v1 set.

---

## Architecture

A simple, one-directional pipeline. Each stage is its own Go package with one
job and a small public surface.

```
flags → cluster.NewClient() → collect.Cluster() → diagnose.Run() → report.Print()
        (connection)           ([]PodFacts)        ([]Finding)      (text/JSON)
```

### Package layout

```
kubeagent/
├── go.mod                       # module github.com/imantaba/kubeagent
├── main.go                      # package main — entrypoint: flags + wiring
├── internal/
│   ├── cluster/client.go        # kubeconfig → *kubernetes.Clientset
│   ├── collect/collect.go       # list pods + their events → []PodFacts
│   ├── diagnose/
│   │   ├── diagnose.go          # Detector interface + Finding + PodFacts + Run()
│   │   ├── crashloop.go         # one detector per failure mode
│   │   ├── imagepull.go
│   │   ├── oomkilled.go
│   │   └── pending.go
│   └── report/report.go         # []Finding → terminal / JSON output
```

`internal/` is a Go convention: packages under it can only be imported by code
in this same module — good encapsulation for a tool that has no public library
API.

---

## Core types — the `Detector` interface

The heart of the tool. Three small types:

```go
// PodFacts bundles everything a detector needs about one pod.
type PodFacts struct {
    Pod    *corev1.Pod      // the live pod object from client-go
    Events []corev1.Event   // recent events for that pod
}

// Finding is one diagnosis: what's wrong and why.
type Finding struct {
    Pod      string   // "namespace/name"
    Issue    string   // "CrashLoopBackOff"
    Reason   string   // human-readable root cause
    Evidence string   // the exact signal: exit code, event message, ...
}

// Detector inspects one pod and returns a Finding if it matches,
// or nil if this pod doesn't have this problem.
type Detector interface {
    Detect(facts PodFacts) *Finding
}
```

Each failure mode is a small struct implementing `Detect`. Example:

```go
type CrashLoopDetector struct{}

func (d CrashLoopDetector) Detect(facts PodFacts) *Finding {
    for _, cs := range facts.Pod.Status.ContainerStatuses {
        if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
            return &Finding{
                Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
                Issue:    "CrashLoopBackOff",
                Reason:   "Container repeatedly crashes after starting",
                Evidence: fmt.Sprintf("container %q, restartCount=%d", cs.Name, cs.RestartCount),
            }
        }
    }
    return nil // nil = "not my problem"
}
```

`Run` loops every detector over every pod. Adding a new failure mode later is
**one new file** and one line in the detector list — the loop never changes:

```go
func Run(detectors []Detector, facts []PodFacts) []Finding {
    var findings []Finding
    for _, f := range facts {
        for _, d := range detectors {
            if finding := d.Detect(f); finding != nil {
                findings = append(findings, *finding)
            }
        }
    }
    return findings
}
```

### How each failure mode is recognized

- **CrashLoopBackOff** — a container status with `State.Waiting.Reason ==
  "CrashLoopBackOff"`. Evidence: container name + restart count.
- **ImagePullBackOff / ErrImagePull** — `State.Waiting.Reason` is
  `"ImagePullBackOff"` or `"ErrImagePull"`. Evidence: the waiting message
  (usually the image and the registry error).
- **OOMKilled** — a container's last terminated state has `Reason == "OOMKilled"`
  (check `LastTerminationState.Terminated` and/or `State.Terminated`). Evidence:
  exit code 137 + container name.
- **Pending / Unschedulable** — pod `Status.Phase == "Pending"` with a
  `PodScheduled` condition of `status=False, reason=Unschedulable`. Evidence: the
  scheduler message from the condition or the related `FailedScheduling` event.

---

## Connecting to the cluster (`cluster` package)

`cluster.NewClient(kubeconfigPath string) (*kubernetes.Clientset, error)`:

1. Resolve kubeconfig path: `--kubeconfig` flag → `$KUBECONFIG` → `~/.kube/config`.
2. Build a `*rest.Config` from it (`clientcmd.BuildConfigFromFlags`).
3. Return `kubernetes.NewForConfig(config)` — the typed clientset.

This is the same configuration path `kubectl` uses, so it respects the user's
current context.

---

## Collecting facts (`collect` package)

`collect.Cluster(clientset) ([]PodFacts, error)`:

1. List all pods across all namespaces: `clientset.CoreV1().Pods("").List(...)`.
2. For each pod, gather recent events (used by detectors for evidence — e.g. the
   scheduler's `FailedScheduling` message). Events are fetched per namespace and
   matched to pods by `involvedObject`.
3. Return a `[]PodFacts`.

Only this package touches the network. Everything downstream operates on plain
in-memory structs.

---

## Reporting (`report` package)

`report.Print(findings []Finding, format string, w io.Writer)`:

- **text** (default): grouped, readable output. Example:

  ```
  kube-system/coredns-abc        CrashLoopBackOff
      Container repeatedly crashes after starting
      evidence: container "coredns", restartCount=14

  default/web-xyz                ImagePullBackOff
      Bad image reference or registry auth
      evidence: Failed to pull image "myrepo/web:typo": not found

  2 issue(s) found across 37 pods.
  ```

- **json**: a JSON array of `Finding` objects, for piping into other tools.

If no findings: print a clean "no issues found" summary and exit 0.

---

## Error handling

Go style: functions that can fail return `(result, error)`, and callers check
`if err != nil` immediately. Errors are wrapped with context using
`fmt.Errorf("...: %w", err)` so the failure chain is visible. `main` prints the
top-level error to stderr and exits non-zero.

---

## Testing

- **`diagnose` package (the brains):** pure unit tests. Construct fake `PodFacts`
  (a `*corev1.Pod` with the relevant status) and assert each detector returns the
  right `Finding` or `nil`. **No cluster, no network.** Table-driven tests cover
  each failure mode plus the "healthy pod → nil" case.
- **`collect` / `cluster` (the I/O):** tested against a real cluster during
  development, and can later use client-go's `fake.Clientset` for hermetic tests.
- **`report`:** assert formatted output by writing to a `bytes.Buffer`.

Run all tests with `go test ./...`.

---

## CLI surface (v1)

```
kubeagent scan [flags]

Flags:
  --kubeconfig string   path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)
  --output string       output format: text | json   (default "text")
```

Implemented with the standard-library `flag` package (one command, two flags —
no need for a CLI framework yet). Cobra is the documented growth path when a
second command (`diagnose <pod>`) is added.

Exit codes: `0` = ran successfully (whether or not issues were found),
`1` = the tool itself failed (e.g. couldn't reach the cluster).

---

## Roadmap

- **v1** (this design) — deterministic whole-cluster scan + diagnosis.
- **v2 — `--explain`** — an optional flag that takes the collected findings (and
  a little raw context) and makes **one** Claude API call to produce a plain-
  English summary and suggested next steps. Kept as a single, well-bounded call
  so the deterministic core stays usable offline and without an API key.
- **Later (learning extensions):** concurrent fact-collection with goroutines;
  more detectors (probe failures, `CreateContainerConfigError`); a `diagnose
  <pod>` command via Cobra.

---

## Learning objectives mapped to this design

| Go concept                         | Where you meet it in kubeagent           |
| ---------------------------------- | ----------------------------------------- |
| Packages, `internal/`, exports     | the package layout                        |
| Modules / `go.mod`                 | project setup                             |
| Structs & methods                  | `Finding`, `PodFacts`, each detector      |
| Interfaces (implicit satisfaction) | `Detector`                                |
| Pointers & `nil` as "optional"     | `Detect(...) *Finding` returning `nil`    |
| Slices, `append`, `range`          | `Run`, the detector list                  |
| Multiple returns & error handling  | `cluster`, `collect`, wrapping with `%w`  |
| The standard `testing` package     | the `diagnose` unit tests                 |
| Goroutines (future)                | concurrent fact-collection (v-later)      |

See [go-concepts.md](go-concepts.md) for each concept explained with a simple
example followed by its kubeagent example.
