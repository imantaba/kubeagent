# Capacity hints (`--capacity`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship one opt-in, advisory `CAPACITY` report section with two sub-blocks — scheduling **Headroom** (per-node arithmetic, node-loss by first-fit) and **Right-sizing** (three structural workload rules, with a metrics-server sample attached as context only).

**Architecture:** A new pure package `internal/capacity` takes nodes, pods, ReplicaSets, and an optional per-pod usage map and returns a `Report`. It issues no API calls of its own; nodes, pods, and ReplicaSets are already collected on every scan, so the headroom half is free. The one new fetch is `collect.PodMetrics`, a raw GET on `/apis/metrics.k8s.io/v1beta1/pods` mirroring the existing `NodeMetrics`. Wiring follows the CLI-composed-view pattern already used by `--operators` and `--drift`: `main.go` calls `capacity.Assess` and sets `in.Capacity`; the report renders it. `internal/scan` and `internal/watch` are untouched and the daemon gains no RBAC.

**Tech Stack:** Go 1.26, standard-library `flag` only, `k8s.io/api/core/v1`, `k8s.io/api/apps/v1`, `k8s.io/apimachinery/pkg/api/resource`, client-go fake clientset for the collect test.

## Global Constraints

Copied from `docs/superpowers/specs/2026-07-27-capacity-hints-design.md`. Every task's requirements implicitly include this section.

- **No money.** No price table, no currency symbol, no `cost`/`spend` wording in code, output, or docs. Every figure is cores and GiB.
- **No peak.** metrics-server returns one sample (~30s average) and retains no history. The words **`peak`**, **`over-requested`**, **`oversized`**, and **`waste`** must be structurally absent from `internal/capacity`, from the rendered report, and from the docs — the same discipline that keeps `drifted for` out of `internal/gitops`. Task 4 ships a test that greps rendered output for them.
- **The sample never selects.** A workload appears only because a structural rule flagged it. The usage sample is attached to an existing row as context and never creates a row.
- **Advisory only.** The section never produces a `Finding`, never sets `hasAttention`, never changes the Healthy/Degraded verdict, never changes the exit code, and takes no part in the all-clear suppression.
- **Read-only.** List/get only. No new RBAC manifest: nodes and pods are already granted, and the `metrics.k8s.io` read needs nothing beyond the existing node-metrics path.
- **Metadata and resource quantities only.** Nothing else from a pod `spec` reaches the output — no image reference, no command, no env var, no annotation.
- **Fixed footer wording**, verbatim: `one sample per pod, ~30s average — not a peak, not a history`.
- **Determinism.** Every list is explicitly sorted; never range over a map to produce output. Ties broken by name ascending.
- **Cap at 20** owners per rule with `… +N more` — never a silent drop. Matches the `--drift` cap.
- **Flag:** `--capacity`, env `KUBEAGENT_CAPACITY`. No threshold flag — nothing here has a tunable boundary.
- **stdlib `flag` only**; no new module dependencies.
- **No `Co-Authored-By: Claude` trailer** (or any Claude / Claude Code / Anthropic attribution) on any commit, in any code comment, or in any doc. Every commit is authored solely by the human.
- No secrets, credentials, private IPs, or internal hostnames anywhere — use `<PLACEHOLDER>`.

### One deliberate correction to the spec

Spec §1 gives the signature as `Assess(nodes, pods, usage, namespace)`, but spec §3 requires resolving a ReplicaSet-owned pod up to its **Deployment**, which is impossible from pods alone — the pod's controller owner is the ReplicaSet, and only the ReplicaSet object names the Deployment. This plan therefore uses:

```go
func Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
    usage map[string]corev1.ResourceList, namespace string) Report
```

`res.Inputs.ReplicaSets` is already collected on every scan (`internal/inventory/inventory.go:200`), so this adds no API call. This is the only intentional divergence from the spec text.

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/capacity/capacity.go` | Exported types, rule-name constants, `Assess`, node classification (`classifyNodes`), shared quantity helpers. |
| `internal/capacity/headroom.go` | The `schedulable` / `largest pod fit` / `tightest node` rows. |
| `internal/capacity/nodeloss.go` | First-fit-decreasing node-loss arithmetic. |
| `internal/capacity/rightsize.go` | The three structural rules, owner roll-up, ordering, 20-cap. |
| `internal/capacity/sample.go` | Attaches observed samples to already-flagged owners; coverage counts. |
| `internal/capacity/helpers_test.go` | Fake node/pod/ReplicaSet builders shared by the package's tests. |
| `internal/collect/collect.go` | `PodMetrics` + `parsePodMetrics` beside the existing `NodeMetrics`. |
| `internal/resources/resources.go` | `formatCPU`/`formatMem` exported as `FormatCPU`/`FormatMem` so `capacity` reuses them instead of duplicating. |
| `internal/report/report.go` | `printCapacity`, the `Input.Capacity` field, and the `capacity` key on **`inventoryReport`**. |
| `main.go` | `--capacity` flag, env fallback, usage string, `capacity.Assess` call. |
| `website/docs/features/capacity.md` | Feature page. |
| `chaos/run.sh` | Scenario 18. |

---

### Task 1: Package skeleton — types, node classification, shared formatters

**Files:**

- Create: `internal/capacity/capacity.go`
- Create: `internal/capacity/capacity_test.go`
- Create: `internal/capacity/helpers_test.go`
- Modify: `internal/resources/resources.go` (export two formatters)

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: the exported types below; `resources.FormatCPU(resource.Quantity) string` and `resources.FormatMem(resource.Quantity) string`; and the unexported `nodeCapacity` struct, `classifyNodes`, `podRequests`, and `terminal` used by Tasks 2–5.

- [ ] **Step 1: Export the two formatters in `internal/resources/resources.go`**

Rename `formatCPU` → `FormatCPU` and `formatMem` → `FormatMem`, update the four call sites in each, and update their doc comments. They are currently referenced only inside `resources.go` (no test refers to them), so this is a pure rename.

```go
// FormatCPU renders a quantity as cores with one decimal, e.g. "8.0". Exported so
// internal/capacity renders identical numbers rather than duplicating the rule.
func FormatCPU(q resource.Quantity) string {
	return fmt.Sprintf("%.1f", float64(q.MilliValue())/1000)
}

// FormatMem renders a quantity in Gi (or Mi below 1Gi), rounded, e.g. "16Gi".
// Exported so internal/capacity renders identical numbers rather than duplicating
// the rule.
func FormatMem(q resource.Quantity) string {
	b := q.Value()
	if b >= 1<<30 {
		return fmt.Sprintf("%.0fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
}
```

Then in `cpuLine` and `memLine`, replace every `formatCPU(` with `FormatCPU(` and every `formatMem(` with `FormatMem(`.

- [ ] **Step 2: Run the resources tests to prove the rename broke nothing**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/resources/
```

Expected: `ok  	github.com/imantaba/kubeagent/internal/resources`

- [ ] **Step 3: Write the fake builders**

Create `internal/capacity/helpers_test.go`:

```go
package capacity

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a Ready, schedulable, untainted node with the given allocatable.
// cpu is a quantity string like "4" or "500m"; mem like "16Gi".
func node(name, cpu, mem string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// notReady flips a node's Ready condition to False.
func notReady(n corev1.Node) corev1.Node {
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	return n
}

// cordoned marks a node unschedulable.
func cordoned(n corev1.Node) corev1.Node {
	n.Spec.Unschedulable = true
	return n
}

// tainted adds a taint with the given effect.
func tainted(n corev1.Node, effect corev1.TaintEffect) corev1.Node {
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key: "node-role.kubernetes.io/control-plane", Effect: effect,
	})
	return n
}

// pod builds a Running pod on nodeName with one container requesting cpu/mem.
// An empty quantity string means that request is absent.
func pod(namespace, name, nodeName, cpu, mem string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{container("app", cpu, mem, "", "")},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// container builds one container. Empty strings omit that request or limit.
func container(name, cpuReq, memReq, cpuLim, memLim string) corev1.Container {
	c := corev1.Container{Name: name}
	if cpuReq != "" || memReq != "" {
		c.Resources.Requests = corev1.ResourceList{}
		if cpuReq != "" {
			c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
		}
		if memReq != "" {
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memReq)
		}
	}
	if cpuLim != "" || memLim != "" {
		c.Resources.Limits = corev1.ResourceList{}
		if cpuLim != "" {
			c.Resources.Limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
		}
		if memLim != "" {
			c.Resources.Limits[corev1.ResourceMemory] = resource.MustParse(memLim)
		}
	}
	return c
}

// ownedBy sets a controller ownerReference of the given kind and name.
func ownedBy(p corev1.Pod, kind, name string) corev1.Pod {
	yes := true
	p.OwnerReferences = []metav1.OwnerReference{
		{Kind: kind, Name: name, Controller: &yes},
	}
	return p
}

// replicaSet builds a ReplicaSet owned by a Deployment of the given name.
func replicaSet(namespace, name, deployment string) appsv1.ReplicaSet {
	yes := true
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: name,
		OwnerReferences: []metav1.OwnerReference{
			{Kind: "Deployment", Name: deployment, Controller: &yes},
		},
	}}
}
```

- [ ] **Step 4: Write the failing classification tests**

Create `internal/capacity/capacity_test.go`:

```go
package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestClassifyNodesIncludesHealthyNodes(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "4", "16Gi")}
	pods := []corev1.Pod{pod("prod", "a", "worker1", "1", "2Gi")}

	included, excluded := classifyNodes(nodes, pods)

	if len(included) != 2 {
		t.Fatalf("want 2 included nodes, got %d", len(included))
	}
	if len(excluded) != 0 {
		t.Fatalf("want no exclusions, got %+v", excluded)
	}
	if included[0].name != "worker1" || included[1].name != "worker2" {
		t.Fatalf("want included sorted by name, got %s, %s", included[0].name, included[1].name)
	}
	if included[0].cpuReq != 1000 {
		t.Errorf("want worker1 cpuReq 1000m, got %d", included[0].cpuReq)
	}
	if included[1].cpuReq != 0 {
		t.Errorf("want worker2 cpuReq 0, got %d", included[1].cpuReq)
	}
}

func TestClassifyNodesExclusionReasons(t *testing.T) {
	nodes := []corev1.Node{
		node("ok", "4", "16Gi"),
		notReady(node("down", "4", "16Gi")),
		cordoned(node("parked", "4", "16Gi")),
		tainted(node("cp", "4", "16Gi"), corev1.TaintEffectNoSchedule),
		tainted(node("drained", "4", "16Gi"), corev1.TaintEffectNoExecute),
	}

	included, excluded := classifyNodes(nodes, nil)

	if len(included) != 1 || included[0].name != "ok" {
		t.Fatalf("want only ok included, got %+v", included)
	}
	want := map[string]string{
		"down":    "NotReady",
		"parked":  "cordoned",
		"cp":      "NoSchedule taint",
		"drained": "NoExecute taint",
	}
	if len(excluded) != len(want) {
		t.Fatalf("want %d exclusions, got %+v", len(want), excluded)
	}
	for _, e := range excluded {
		if want[e.Node] != e.Reason {
			t.Errorf("node %s: want reason %q, got %q", e.Node, want[e.Node], e.Reason)
		}
	}
}

// A node that is simultaneously NotReady, cordoned and tainted reports exactly one
// reason, the first in the documented order, so the output cannot vary by map order.
func TestClassifyNodesReportsFirstReasonOnly(t *testing.T) {
	n := tainted(cordoned(notReady(node("triple", "4", "16Gi"))), corev1.TaintEffectNoSchedule)

	_, excluded := classifyNodes([]corev1.Node{n}, nil)

	if len(excluded) != 1 {
		t.Fatalf("want exactly 1 exclusion, got %+v", excluded)
	}
	if excluded[0].Reason != "NotReady" {
		t.Errorf("want first reason NotReady, got %q", excluded[0].Reason)
	}
}

// Terminal pods reserve nothing — the same rule internal/resources already applies.
func TestClassifyNodesSkipsTerminalPods(t *testing.T) {
	done := pod("prod", "done", "worker1", "2", "4Gi")
	done.Status.Phase = corev1.PodSucceeded
	failed := pod("prod", "failed", "worker1", "2", "4Gi")
	failed.Status.Phase = corev1.PodFailed

	included, _ := classifyNodes([]corev1.Node{node("worker1", "4", "16Gi")},
		[]corev1.Pod{done, failed})

	if included[0].cpuReq != 0 {
		t.Errorf("want terminal pods to reserve nothing, got cpuReq %d", included[0].cpuReq)
	}
}

// A pod scheduled onto an excluded node must not be counted against any included
// node — its requests belong to capacity the section has already disclaimed.
func TestClassifyNodesIgnoresPodsOnExcludedNodes(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "4", "16Gi"),
		tainted(node("cp", "4", "16Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{pod("kube-system", "apiserver", "cp", "2", "4Gi")}

	included, _ := classifyNodes(nodes, pods)

	if len(included) != 1 || included[0].cpuReq != 0 {
		t.Fatalf("want the cp pod ignored, got %+v", included)
	}
}
```

- [ ] **Step 5: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/
```

Expected: FAIL — `undefined: classifyNodes` (and the helpers file will not compile against a package with no non-test source, which is the same signal).

- [ ] **Step 6: Write `internal/capacity/capacity.go`**

```go
// Package capacity reports scheduling headroom and structurally wrong workload
// shapes. It is pure: the caller supplies nodes, pods, ReplicaSets, and an optional
// per-pod usage sample. It issues no API calls and is advisory only — nothing here
// produces a Finding or changes the cluster verdict.
//
// Two numbers this package deliberately never produces. It never renders money:
// no cluster publishes prices, so every figure is cores and GiB. And it never
// claims a peak: metrics-server returns a single sample of roughly a 30-second
// average and retains no history, so a usage reading is attached as context to a
// workload some structural rule already flagged, and never selects one by itself.
package capacity

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RuleName identifies a structural right-sizing rule. The constant is the JSON and
// lookup key; the report owns the human label.
type RuleName string

const (
	// RuleNoRequests: a container declares neither a CPU nor a memory request.
	RuleNoRequests RuleName = "noRequests"
	// RuleLimitNoRequest: a container sets a limit for a resource but no request
	// for it, so Kubernetes defaults the request to the limit.
	RuleLimitNoRequest RuleName = "limitNoRequest"
	// RuleNeverSchedulable: a container request exceeds the largest included node's
	// allocatable, so the pod can never be placed.
	RuleNeverSchedulable RuleName = "neverSchedulable"
)

// maxOwnersPerRule caps enumeration per rule; the remainder is reported as a count,
// never silently dropped. Matches the internal/gitops cap.
const maxOwnersPerRule = 20

// Report is the advisory capacity view. Either half may be nil.
type Report struct {
	Headroom    *Headroom    `json:"headroom,omitempty"`
	RightSizing *RightSizing `json:"rightSizing,omitempty"`
}

// Headroom is the scheduling picture over included nodes only.
type Headroom struct {
	IncludedNodes int             `json:"includedNodes"`
	TotalNodes    int             `json:"totalNodes"`
	FreeCPU       string          `json:"freeCPU"`
	FreeMemory    string          `json:"freeMemory"`
	LargestCPUFit *NodeFit        `json:"largestCPUFit,omitempty"`
	LargestMemFit *NodeFit        `json:"largestMemFit,omitempty"`
	TightestNode  *TightNode      `json:"tightestNode,omitempty"`
	NodeLoss      *NodeLoss       `json:"nodeLoss,omitempty"`
	Excluded      []NodeExclusion `json:"excluded,omitempty"`
}

// NodeFit is one node's free capacity. CPU and Memory always come from the SAME
// node: a pod lands on one node, so a cross-node maximum would describe a shape
// nothing can schedule.
type NodeFit struct {
	Node   string `json:"node"`
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

// TightNode is the node closest to full, by whichever of its two ratios is higher.
type TightNode struct {
	Node     string `json:"node"`
	Resource string `json:"resource"` // "CPU" or "memory"
	Pct      int    `json:"pct"`
}

// NodeLoss is the first-fit-decreasing result of removing the largest included
// node. Fits=true is a constructive proof the requests fit; Fits=false is not a
// proof they do not, which is why the report says "may not fit".
type NodeLoss struct {
	Node       string `json:"node"`
	SingleNode bool   `json:"singleNode"`
	Fits       bool   `json:"fits"`
	Placed     int    `json:"placed"`
	Blocker    string `json:"blocker,omitempty"`
	BlockerCPU string `json:"blockerCPU,omitempty"`
}

// NodeExclusion names a node whose capacity was not counted, and why.
type NodeExclusion struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// RightSizing carries the structural rules that matched, plus sample coverage.
type RightSizing struct {
	Rules            []Rule `json:"rules,omitempty"`
	MetricsAvailable bool   `json:"metricsAvailable"`
	PodsReporting    int    `json:"podsReporting"`
	PodsTotal        int    `json:"podsTotal"`
}

// Rule is one structural rule and the owners that matched it.
type Rule struct {
	Name      RuleName `json:"name"`
	Owners    []Owner  `json:"owners"`
	Truncated int      `json:"truncated,omitempty"`
}

// Owner is one flagged workload. Detail is rule-specific; Observed is the attached
// usage sample and is empty when metrics-server did not answer for its pods.
type Owner struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Detail     string `json:"detail,omitempty"`
	Observed   string `json:"observed,omitempty"`
	BestEffort bool   `json:"bestEffort,omitempty"`
}

// nodeCapacity is one included node's arithmetic, in milli-cores and bytes.
type nodeCapacity struct {
	name     string
	cpuAlloc int64
	memAlloc int64
	cpuReq   int64
	memReq   int64
}

func (n nodeCapacity) freeCPU() int64 { return max64(0, n.cpuAlloc-n.cpuReq) }
func (n nodeCapacity) freeMem() int64 { return max64(0, n.memAlloc-n.memReq) }

// classifyNodes splits nodes into the included set (Ready, schedulable, untainted)
// and the excluded set with a reason, and accumulates each included node's pod
// requests. Pods on excluded nodes are ignored: their requests sit on capacity the
// section has already disclaimed. Both slices are sorted by node name.
//
// Exactly one reason is reported per excluded node, in the order checked here, so
// output never varies between runs.
func classifyNodes(nodes []corev1.Node, pods []corev1.Pod) ([]nodeCapacity, []NodeExclusion) {
	var included []nodeCapacity
	var excluded []NodeExclusion
	byName := map[string]int{}
	for _, n := range nodes {
		if reason := excludeReason(n); reason != "" {
			excluded = append(excluded, NodeExclusion{Node: n.Name, Reason: reason})
			continue
		}
		byName[n.Name] = len(included)
		included = append(included, nodeCapacity{
			name:     n.Name,
			cpuAlloc: n.Status.Allocatable.Cpu().MilliValue(),
			memAlloc: n.Status.Allocatable.Memory().Value(),
		})
	}
	for _, p := range pods {
		i, ok := byName[p.Spec.NodeName]
		if !ok || terminal(p) {
			continue
		}
		cpu, mem := podRequests(p)
		included[i].cpuReq += cpu
		included[i].memReq += mem
	}
	sort.Slice(included, func(a, b int) bool { return included[a].name < included[b].name })
	sort.Slice(excluded, func(a, b int) bool { return excluded[a].Node < excluded[b].Node })
	return included, excluded
}

// excludeReason returns "" when the node is usable for ordinary scheduling.
func excludeReason(n corev1.Node) string {
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
			break
		}
	}
	if !ready {
		return "NotReady"
	}
	if n.Spec.Unschedulable {
		return "cordoned"
	}
	for _, t := range n.Spec.Taints {
		switch t.Effect {
		case corev1.TaintEffectNoSchedule:
			return "NoSchedule taint"
		case corev1.TaintEffectNoExecute:
			return "NoExecute taint"
		}
	}
	return ""
}

// podRequests sums a pod's container requests as milli-cores and bytes.
func podRequests(p corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	return cpu, mem
}

// terminal reports whether a pod has stopped and so reserves nothing.
func terminal(p corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ownedByDaemonSet reports whether the pod's controller owner is a DaemonSet.
// DaemonSet pods do not rehome when their node is removed, so node-loss arithmetic
// excludes them.
func ownedByDaemonSet(p corev1.Pod) bool {
	o := controllerOwner(p.OwnerReferences)
	return o != nil && o.Kind == "DaemonSet"
}

// controllerOwner returns the controller ownerReference, or the first reference
// when none is marked controller, or nil when there are none.
func controllerOwner(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}
```

`appsv1` is deliberately **not** imported here — no non-test file in this task needs
it. Task 2 adds it with the `Assess` signature. (`helpers_test.go` imports it for the
`replicaSet` builder, which Task 4's tests use; an unused test helper function is legal
Go and does not need a placeholder.)

- [ ] **Step 7: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ ./internal/resources/ -v 2>&1 | tail -25
```

Expected: every `TestClassifyNodes*` PASS, resources PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/capacity/ internal/resources/resources.go
git commit -m "feat(capacity): package skeleton, node classification, shared formatters"
```

---

### Task 2: Headroom rows and `Assess`

**Files:**

- Create: `internal/capacity/headroom.go`
- Create: `internal/capacity/headroom_test.go`
- Modify: `internal/capacity/capacity.go` (add `Assess`; drop the `var _ = appsv1.ReplicaSet{}` placeholder)

**Interfaces:**

- Consumes: `nodeCapacity`, `classifyNodes`, `NodeFit`, `TightNode`, `Headroom`, `Report` from Task 1; `resources.FormatCPU`/`FormatMem`.
- Produces: `Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet, usage map[string]corev1.ResourceList, namespace string) Report` — the final signature, used unchanged by Tasks 3–7. `buildHeadroom(included []nodeCapacity, excluded []NodeExclusion, total int, pods []corev1.Pod) *Headroom`.

The `replicaSets` and `usage` parameters are accepted now and gain meaning in Tasks 4 and 5; declaring the final signature once avoids a churn commit later.

- [ ] **Step 1: Write the failing tests**

Create `internal/capacity/headroom_test.go`:

```go
package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestHeadroomSchedulableSumsIncludedNodesOnly(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "4", "16Gi"),
		node("worker2", "4", "16Gi"),
		tainted(node("cp", "8", "32Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{pod("prod", "a", "worker1", "1", "4Gi")}

	rep := Assess(nodes, pods, nil, nil, "")

	h := rep.Headroom
	if h == nil {
		t.Fatal("want a headroom block")
	}
	if h.IncludedNodes != 2 || h.TotalNodes != 3 {
		t.Errorf("want 2 of 3 nodes, got %d of %d", h.IncludedNodes, h.TotalNodes)
	}
	// 8 allocatable cores across the two workers, 1 requested.
	if h.FreeCPU != "7.0" {
		t.Errorf("want FreeCPU 7.0, got %q", h.FreeCPU)
	}
	if h.FreeMemory != "28Gi" {
		t.Errorf("want FreeMemory 28Gi, got %q", h.FreeMemory)
	}
	if len(h.Excluded) != 1 || h.Excluded[0].Node != "cp" {
		t.Errorf("want cp excluded, got %+v", h.Excluded)
	}
}

// largest pod fit must never mix nodes: the memory reported beside the winning CPU
// node is that same node's memory.
func TestHeadroomLargestFitNeverMixesNodes(t *testing.T) {
	nodes := []corev1.Node{node("bigcpu", "16", "8Gi"), node("bigmem", "2", "64Gi")}

	rep := Assess(nodes, nil, nil, nil, "")

	h := rep.Headroom
	if h.LargestCPUFit == nil || h.LargestCPUFit.Node != "bigcpu" {
		t.Fatalf("want bigcpu as the CPU fit, got %+v", h.LargestCPUFit)
	}
	if h.LargestCPUFit.Memory != "8Gi" {
		t.Errorf("want bigcpu's own memory 8Gi beside it, got %q", h.LargestCPUFit.Memory)
	}
	if h.LargestMemFit == nil || h.LargestMemFit.Node != "bigmem" {
		t.Fatalf("want a separate bigmem line, got %+v", h.LargestMemFit)
	}
	if h.LargestMemFit.CPU != "2.0" {
		t.Errorf("want bigmem's own CPU 2.0 beside it, got %q", h.LargestMemFit.CPU)
	}
}

// When one node wins both, there is no second line to print.
func TestHeadroomLargestFitSingleLineWhenSameNode(t *testing.T) {
	nodes := []corev1.Node{node("big", "16", "64Gi"), node("small", "2", "8Gi")}

	rep := Assess(nodes, nil, nil, nil, "")

	if rep.Headroom.LargestMemFit != nil {
		t.Errorf("want no separate memory line when one node wins both, got %+v",
			rep.Headroom.LargestMemFit)
	}
}

func TestHeadroomTightestNodePicksHigherRatio(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "4", "16Gi")}
	pods := []corev1.Pod{
		pod("prod", "cpuhog", "worker1", "3", "1Gi"),   // 75% CPU, 6% memory
		pod("prod", "memhog", "worker2", "1", "14Gi"),  // 25% CPU, 87% memory
	}

	rep := Assess(nodes, pods, nil, nil, "")

	tn := rep.Headroom.TightestNode
	if tn == nil || tn.Node != "worker2" {
		t.Fatalf("want worker2 tightest, got %+v", tn)
	}
	if tn.Resource != "memory" || tn.Pct != 87 {
		t.Errorf("want memory 87%%, got %s %d%%", tn.Resource, tn.Pct)
	}
}

func TestHeadroomNilWhenNoNodesIncluded(t *testing.T) {
	nodes := []corev1.Node{cordoned(node("parked", "4", "16Gi"))}

	rep := Assess(nodes, nil, nil, nil, "")

	h := rep.Headroom
	if h == nil {
		t.Fatal("want a headroom block carrying the exclusion list")
	}
	if h.IncludedNodes != 0 {
		t.Errorf("want 0 included, got %d", h.IncludedNodes)
	}
	if h.LargestCPUFit != nil || h.TightestNode != nil {
		t.Errorf("want no rows with nothing included, got %+v / %+v",
			h.LargestCPUFit, h.TightestNode)
	}
	if len(h.Excluded) != 1 {
		t.Errorf("want the exclusion still reported, got %+v", h.Excluded)
	}
}

// A node reporting zero allocatable must not divide by zero.
func TestHeadroomZeroAllocatableNode(t *testing.T) {
	nodes := []corev1.Node{node("empty", "0", "0")}

	rep := Assess(nodes, nil, nil, nil, "")

	if rep.Headroom.TightestNode == nil || rep.Headroom.TightestNode.Pct != 0 {
		t.Errorf("want 0%% for a zero-allocatable node, got %+v", rep.Headroom.TightestNode)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/
```

Expected: FAIL — `undefined: Assess`.

- [ ] **Step 3: Write `internal/capacity/headroom.go`**

```go
package capacity

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/imantaba/kubeagent/internal/resources"
)

// buildHeadroom reduces the included nodes to the four headroom rows. It returns a
// block even when nothing is included, because the exclusion list is itself the
// answer in that case.
func buildHeadroom(included []nodeCapacity, excluded []NodeExclusion, total int, pods []corev1.Pod) *Headroom {
	h := &Headroom{
		IncludedNodes: len(included),
		TotalNodes:    total,
		Excluded:      excluded,
		FreeCPU:       formatMilliCPU(0),
		FreeMemory:    formatBytes(0),
	}
	if len(included) == 0 {
		return h
	}

	var freeCPU, freeMem int64
	cpuWinner, memWinner := 0, 0
	tightest, tightestPct, tightestRes := 0, -1, "CPU"
	for i, n := range included {
		freeCPU += n.freeCPU()
		freeMem += n.freeMem()
		if n.freeCPU() > included[cpuWinner].freeCPU() {
			cpuWinner = i
		}
		if n.freeMem() > included[memWinner].freeMem() {
			memWinner = i
		}
		cpuPct, memPct := ratio(n.cpuReq, n.cpuAlloc), ratio(n.memReq, n.memAlloc)
		pct, res := cpuPct, "CPU"
		if memPct > cpuPct {
			pct, res = memPct, "memory"
		}
		if pct > tightestPct {
			tightest, tightestPct, tightestRes = i, pct, res
		}
	}

	h.FreeCPU = formatMilliCPU(freeCPU)
	h.FreeMemory = formatBytes(freeMem)
	h.LargestCPUFit = fitOf(included[cpuWinner])
	if memWinner != cpuWinner {
		h.LargestMemFit = fitOf(included[memWinner])
	}
	h.TightestNode = &TightNode{
		Node: included[tightest].name, Resource: tightestRes, Pct: tightestPct,
	}
	// h.NodeLoss is filled by Task 3, which adds nodeloss.go and the one call line
	// here. The pods parameter exists for it and is unused in this task — an unused
	// function parameter is legal Go and no placeholder is needed.
	return h
}

func fitOf(n nodeCapacity) *NodeFit {
	return &NodeFit{
		Node:   n.name,
		CPU:    formatMilliCPU(n.freeCPU()),
		Memory: formatBytes(n.freeMem()),
	}
}

// ratio is percent of whole, 0 when whole is not positive — the same guard
// internal/resources applies.
func ratio(part, whole int64) int {
	if whole <= 0 {
		return 0
	}
	return int(part * 100 / whole)
}

// formatMilliCPU renders milli-cores through the shared formatter so the CAPACITY
// section and the Resources block never disagree about the same number.
func formatMilliCPU(milli int64) string {
	return resources.FormatCPU(*resource.NewMilliQuantity(milli, resource.DecimalSI))
}

func formatBytes(b int64) string {
	return resources.FormatMem(*resource.NewQuantity(b, resource.BinarySI))
}
```

No stub for node-loss: `buildHeadroom` simply leaves `h.NodeLoss` nil in this task, and Task 3 adds both `nodeloss.go` and the single call line. Nothing temporary is introduced.

- [ ] **Step 4: Add `Assess` to `internal/capacity/capacity.go`**

Add `appsv1 "k8s.io/api/apps/v1"` to the import block, then:

```go
// Assess builds the advisory capacity view. nodes and pods are cluster-wide in
// every case — headroom arithmetic is meaningless scoped to one namespace — while
// namespace, when non-empty, scopes the right-sizing enumeration only.
//
// usage is the per-pod sample keyed "namespace/name"; a nil or empty map means
// metrics-server did not answer, and the right-sizing rules still apply.
func Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	usage map[string]corev1.ResourceList, namespace string) Report {
	included, excluded := classifyNodes(nodes, pods)
	return Report{
		Headroom: buildHeadroom(included, excluded, len(nodes), pods),
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -v 2>&1 | tail -30
```

Expected: every `TestHeadroom*` and `TestClassifyNodes*` PASS. `go vet ./internal/capacity/` may report the unused `replicaSets`/`usage` parameters as fine — unused function parameters are legal Go and intentional here.

- [ ] **Step 6: Commit**

```bash
git add internal/capacity/
git commit -m "feat(capacity): headroom rows over included nodes only"
```

---

### Task 3: Node-loss by first-fit-decreasing

**Files:**

- Create: `internal/capacity/nodeloss.go`
- Create: `internal/capacity/nodeloss_test.go`
- Modify: `internal/capacity/headroom.go` (one call line in `buildHeadroom`)

**Interfaces:**

- Consumes: `nodeCapacity`, `NodeLoss`, `podRequests`, `terminal`, `ownedByDaemonSet`, `controllerOwner` from Task 1; `formatMilliCPU` from Task 2.
- Produces: `nodeLoss(included []nodeCapacity, pods []corev1.Pod) *NodeLoss`, called by `buildHeadroom`.

**The asymmetry that governs the wording.** First-fit-decreasing is a heuristic. When it places every pod, that placement is a *constructive proof* the requests fit — so `Fits: true` is a true claim. When it fails, some other packing might still succeed, so `Fits: false` licenses only "may not fit", never "does not fit". Task 6 renders exactly those two strings.

- [ ] **Step 1: Write the failing tests**

Create `internal/capacity/nodeloss_test.go`:

```go
package capacity

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNodeLossFitsWhenRemainingNodesHaveRoom(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "8", "32Gi"), node("worker2", "8", "32Gi")}
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "1", "2Gi"),
		pod("prod", "b", "worker1", "1", "2Gi"),
	}

	rep := Assess(nodes, pods, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl == nil {
		t.Fatal("want a node-loss result")
	}
	if !nl.Fits {
		t.Errorf("want fits, got %+v", nl)
	}
	if nl.Placed != 2 {
		t.Errorf("want 2 pods placed, got %d", nl.Placed)
	}
}

func TestNodeLossReportsBlockerWhenFirstFitCannotPlace(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "8", "32Gi"), node("worker2", "2", "8Gi")}
	pods := []corev1.Pod{ownedBy(pod("prod", "db-0", "worker1", "6", "2Gi"), "StatefulSet", "db")}

	rep := Assess(nodes, pods, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl.Fits {
		t.Fatalf("want a placement failure, got %+v", nl)
	}
	if nl.Node != "worker1" {
		t.Errorf("want the largest node as the victim, got %q", nl.Node)
	}
	if nl.Blocker != "StatefulSet/prod/db" {
		t.Errorf("want the owner named as blocker, got %q", nl.Blocker)
	}
	if nl.BlockerCPU != "6.0" {
		t.Errorf("want the blocker's CPU request, got %q", nl.BlockerCPU)
	}
}

// DaemonSet pods do not rehome when their node goes away, so they must not count
// against the remaining nodes' room.
func TestNodeLossExcludesDaemonSetPods(t *testing.T) {
	nodes := []corev1.Node{node("worker1", "4", "16Gi"), node("worker2", "1", "2Gi")}
	pods := []corev1.Pod{ownedBy(pod("kube-system", "cni-abc", "worker1", "3", "8Gi"), "DaemonSet", "cni")}

	rep := Assess(nodes, pods, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if !nl.Fits {
		t.Errorf("want fits — the only pod is a DaemonSet pod, got %+v", nl)
	}
	if nl.Placed != 0 {
		t.Errorf("want 0 pods needing placement, got %d", nl.Placed)
	}
}

func TestNodeLossSingleNode(t *testing.T) {
	rep := Assess([]corev1.Node{node("only", "4", "16Gi")}, nil, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if nl == nil || !nl.SingleNode {
		t.Fatalf("want the single-node case flagged, got %+v", nl)
	}
	if nl.Fits {
		t.Errorf("want Fits false when no arithmetic is possible, got %+v", nl)
	}
}

// Equal-sized nodes must not make the victim depend on slice order.
func TestNodeLossVictimTieBreaksByName(t *testing.T) {
	nodes := []corev1.Node{node("zeta", "4", "16Gi"), node("alpha", "4", "16Gi")}

	rep := Assess(nodes, nil, nil, nil, "")

	if rep.Headroom.NodeLoss.Node != "alpha" {
		t.Errorf("want the alphabetically first node on a tie, got %q", rep.Headroom.NodeLoss.Node)
	}
}

// Decreasing order matters: placing the big pod first is what makes first-fit
// succeed here. A naive in-order pass would fill worker2 with the small pod and
// then fail the big one.
func TestNodeLossPlacesLargestFirst(t *testing.T) {
	nodes := []corev1.Node{
		node("victim", "8", "32Gi"),
		node("roomy", "4", "16Gi"),
		node("tiny", "1", "4Gi"),
	}
	pods := []corev1.Pod{
		pod("prod", "small", "victim", "500m", "1Gi"),
		pod("prod", "big", "victim", "4", "8Gi"),
	}

	rep := Assess(nodes, pods, nil, nil, "")

	nl := rep.Headroom.NodeLoss
	if !nl.Fits || nl.Placed != 2 {
		t.Errorf("want both placed by first-fit-decreasing, got %+v", nl)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -run TestNodeLoss
```

Expected: FAIL — `buildHeadroom` never sets `NodeLoss`, so `TestNodeLossFitsWhenRemainingNodesHaveRoom` fails at `want a node-loss result`.

- [ ] **Step 3: Write `internal/capacity/nodeloss.go`**

```go
package capacity

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// nodeLoss removes the largest included node and tries to place its non-DaemonSet
// pods on the remaining included nodes by first-fit-decreasing.
//
// The heuristic is one-sided sound. A successful pass is a constructive placement
// and therefore proves the requests fit; a failed pass proves nothing, because a
// different packing may still succeed. Callers must render the failure as "may not
// fit", never as "does not fit".
//
// This is resource arithmetic only: it ignores affinity and anti-affinity, topology
// spread constraints, PVC zoning, and PodDisruptionBudgets.
func nodeLoss(included []nodeCapacity, pods []corev1.Pod) *NodeLoss {
	if len(included) == 0 {
		return nil
	}
	victim := largestNode(included)
	if len(included) == 1 {
		return &NodeLoss{Node: victim.name, SingleNode: true}
	}

	type slot struct {
		cpu, mem int64
	}
	remaining := make([]*slot, 0, len(included)-1)
	for _, n := range included {
		if n.name == victim.name {
			continue
		}
		remaining = append(remaining, &slot{cpu: n.freeCPU(), mem: n.freeMem()})
	}

	evictees := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.Spec.NodeName != victim.name || terminal(p) || ownedByDaemonSet(p) {
			continue
		}
		evictees = append(evictees, p)
	}
	// Decreasing by CPU, then memory, then namespace/name so equal pods keep a
	// stable order and the reported blocker never varies between runs.
	sort.Slice(evictees, func(a, b int) bool {
		ca, ma := podRequests(evictees[a])
		cb, mb := podRequests(evictees[b])
		switch {
		case ca != cb:
			return ca > cb
		case ma != mb:
			return ma > mb
		case evictees[a].Namespace != evictees[b].Namespace:
			return evictees[a].Namespace < evictees[b].Namespace
		default:
			return evictees[a].Name < evictees[b].Name
		}
	})

	placed := 0
	for _, p := range evictees {
		cpu, mem := podRequests(p)
		fitted := false
		for _, s := range remaining {
			if s.cpu >= cpu && s.mem >= mem {
				s.cpu -= cpu
				s.mem -= mem
				fitted = true
				break
			}
		}
		if !fitted {
			return &NodeLoss{
				Node:       victim.name,
				Placed:     placed,
				Blocker:    ownerLabel(p),
				BlockerCPU: formatMilliCPU(cpu),
			}
		}
		placed++
	}
	return &NodeLoss{Node: victim.name, Fits: true, Placed: placed}
}

// largestNode picks the included node with the most allocatable CPU, breaking ties
// by name ascending so the row is deterministic on a uniform cluster.
func largestNode(included []nodeCapacity) nodeCapacity {
	best := included[0]
	for _, n := range included[1:] {
		if n.cpuAlloc > best.cpuAlloc || (n.cpuAlloc == best.cpuAlloc && n.name < best.name) {
			best = n
		}
	}
	return best
}

// ownerLabel names a pod by its controller owner, e.g. "StatefulSet/prod/db", or
// by the pod itself when it has no owner. It does not resolve ReplicaSet up to
// Deployment — the blocker line names the object first-fit could not place, and
// Task 4's roll-up is a separate concern with different inputs.
func ownerLabel(p corev1.Pod) string {
	if o := controllerOwner(p.OwnerReferences); o != nil {
		return fmt.Sprintf("%s/%s/%s", o.Kind, p.Namespace, o.Name)
	}
	return fmt.Sprintf("Pod/%s/%s", p.Namespace, p.Name)
}
```

- [ ] **Step 4: Call it from `buildHeadroom` in `internal/capacity/headroom.go`**

Replace the three-line "filled by Task 3" comment left at the end of `buildHeadroom` with the call:

```go
	h.NodeLoss = nodeLoss(included, pods)
	return h
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -v 2>&1 | tail -30
```

Expected: every `TestNodeLoss*` PASS, all earlier tests still PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/capacity/
git commit -m "feat(capacity): node-loss arithmetic by first-fit-decreasing"
```

---

### Task 4: The three structural rules, owner roll-up, ordering, and the cap

**Files:**

- Create: `internal/capacity/rightsize.go`
- Create: `internal/capacity/rightsize_test.go`
- Modify: `internal/capacity/capacity.go` (`Assess` populates `RightSizing`)

**Interfaces:**

- Consumes: `Rule`, `Owner`, `RightSizing`, `RuleName` constants, `maxOwnersPerRule`, `nodeCapacity`, `terminal`, `controllerOwner` from Task 1; `formatMilliCPU`/`formatBytes` from Task 2.
- Produces: `buildRightSizing(pods []corev1.Pod, replicaSets []appsv1.ReplicaSet, included []nodeCapacity, namespace string) *RightSizing`; `ownerOf(p corev1.Pod, rsIndex map[string]string) Owner`; `deploymentIndex(replicaSets []appsv1.ReplicaSet) map[string]string`. Task 5 calls the last two.

**The three rules, exactly:**

| Rule | Trigger (per container) |
| --- | --- |
| `RuleNoRequests` | The container declares neither a CPU nor a memory request. `Owner.BestEffort` is set only when **every** container in the pod is like this. |
| `RuleLimitNoRequest` | The container sets a limit for a resource but no request for that same resource. `Detail` names the limit, e.g. `lim 256Mi`. |
| `RuleNeverSchedulable` | A container's CPU or memory request exceeds the largest **included** node's allocatable for that resource. `Detail` reads e.g. `req 40 cores > largest node (16 cores)`. Skipped entirely when no node is included — with nothing to compare against, the rule cannot fire. |

Not rules, deliberately: `request == limit` (usually an intentional Guaranteed-QoS choice) and `no memory limit` (a defensible cluster-wide policy, not a defect). Do not add them.

- [ ] **Step 1: Write the failing tests**

Create `internal/capacity/rightsize_test.go`:

```go
package capacity

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// ruleByName finds a rule in the report, or fails the test.
func ruleByName(t *testing.T, rs *RightSizing, name RuleName) Rule {
	t.Helper()
	if rs == nil {
		t.Fatalf("want a right-sizing block, got nil")
	}
	for _, r := range rs.Rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("want rule %q, got %+v", name, rs.Rules)
	return Rule{}
}

func TestRuleNoRequestsFlagsBestEffortPod(t *testing.T) {
	pods := []corev1.Pod{ownedBy(pod("staging", "web-1", "worker1", "", ""), "ReplicaSet", "web-abc")}
	rs := []appsv1.ReplicaSet{replicaSet("staging", "web-abc", "web")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	o := r.Owners[0]
	if o.Kind != "Deployment" || o.Namespace != "staging" || o.Name != "web" {
		t.Errorf("want the ReplicaSet resolved up to Deployment/staging/web, got %+v", o)
	}
	if !o.BestEffort {
		t.Error("want BestEffort set when every container lacks both requests")
	}
}

// A pod where only one of two containers lacks requests is still flagged, but it is
// not BestEffort — the pod-level QoS claim would be false.
func TestRuleNoRequestsMixedPodIsNotBestEffort(t *testing.T) {
	p := pod("staging", "api-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{
		container("app", "100m", "128Mi", "", ""),
		container("sidecar", "", "", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 || r.Owners[0].BestEffort {
		t.Errorf("want flagged but not BestEffort, got %+v", r.Owners)
	}
}

func TestRuleLimitNoRequestNamesTheLimit(t *testing.T) {
	p := pod("prod", "cache-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{container("app", "", "", "", "256Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	if !strings.Contains(r.Owners[0].Detail, "lim 256Mi") {
		t.Errorf("want the limit named, got %q", r.Owners[0].Detail)
	}
}

func TestRuleNeverSchedulableComparesAgainstLargestIncludedNode(t *testing.T) {
	nodes := []corev1.Node{
		node("worker1", "16", "64Gi"),
		tainted(node("huge-cp", "64", "256Gi"), corev1.TaintEffectNoSchedule),
	}
	pods := []corev1.Pod{ownedBy(pod("batch", "trainer-1", "", "40", "8Gi"), "Job", "trainer")}

	rep := Assess(nodes, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNeverSchedulable)
	if len(r.Owners) != 1 || r.Owners[0].Name != "trainer" {
		t.Fatalf("want the 40-core Job flagged against the 16-core included node, got %+v", r.Owners)
	}
	if !strings.Contains(r.Owners[0].Detail, "40.0 cores") ||
		!strings.Contains(r.Owners[0].Detail, "16.0 cores") {
		t.Errorf("want both quantities in the detail, got %q", r.Owners[0].Detail)
	}
}

// A request that exactly equals allocatable is schedulable, not "never".
func TestRuleNeverSchedulableExcludesExactFit(t *testing.T) {
	pods := []corev1.Pod{pod("batch", "fits", "", "16", "1Gi")}

	rep := Assess([]corev1.Node{node("worker1", "16", "64Gi")}, pods, nil, nil, "")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleNeverSchedulable {
				t.Errorf("want no never-schedulable rule for an exact fit, got %+v", r.Owners)
			}
		}
	}
}

// With no node included there is nothing to compare against, so the rule is silent
// rather than flagging every workload in the cluster.
func TestRuleNeverSchedulableSilentWithNoIncludedNodes(t *testing.T) {
	nodes := []corev1.Node{cordoned(node("parked", "16", "64Gi"))}
	pods := []corev1.Pod{pod("batch", "big", "", "40", "8Gi")}

	rep := Assess(nodes, pods, nil, nil, "")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleNeverSchedulable {
				t.Errorf("want silence with nothing to compare against, got %+v", r.Owners)
			}
		}
	}
}

// Many pods of one Deployment collapse to one row.
func TestRightSizingRollsUpReplicasToOneOwner(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 5; i++ {
		pods = append(pods, ownedBy(pod("staging", fmt.Sprintf("web-%d", i), "worker1", "", ""),
			"ReplicaSet", "web-abc"))
	}
	rs := []appsv1.ReplicaSet{replicaSet("staging", "web-abc", "web")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 {
		t.Errorf("want 5 replicas rolled up to 1 owner, got %+v", r.Owners)
	}
}

// A pod with no controller owner is reported as itself.
func TestRightSizingOwnerlessPod(t *testing.T) {
	pods := []corev1.Pod{pod("default", "loose", "worker1", "", "")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if r.Owners[0].Kind != "Pod" || r.Owners[0].Name != "loose" {
		t.Errorf("want Pod/default/loose, got %+v", r.Owners[0])
	}
}

func TestRightSizingCapsAtTwentyOwners(t *testing.T) {
	var pods []corev1.Pod
	for i := 0; i < 26; i++ {
		pods = append(pods, pod("staging", fmt.Sprintf("p%02d", i), "worker1", "", ""))
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 20 {
		t.Errorf("want 20 owners listed, got %d", len(r.Owners))
	}
	if r.Truncated != 6 {
		t.Errorf("want 6 reported as truncated, got %d", r.Truncated)
	}
}

func TestRightSizingOrdersByNamespaceThenName(t *testing.T) {
	pods := []corev1.Pod{
		pod("zeta", "b", "worker1", "", ""),
		pod("alpha", "z", "worker1", "", ""),
		pod("alpha", "a", "worker1", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	got := []string{}
	for _, o := range r.Owners {
		got = append(got, o.Namespace+"/"+o.Name)
	}
	want := []string{"alpha/a", "alpha/z", "zeta/b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestRightSizingNamespaceScopesEnumerationOnly(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "", ""),
		pod("staging", "b", "worker1", "", ""),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "prod")

	r := ruleByName(t, rep.RightSizing, RuleNoRequests)
	if len(r.Owners) != 1 || r.Owners[0].Namespace != "prod" {
		t.Errorf("want only prod enumerated, got %+v", r.Owners)
	}
	// Headroom still counts both pods: nodes are cluster-scoped.
	if rep.Headroom.IncludedNodes != 1 {
		t.Errorf("want headroom unaffected by -n, got %+v", rep.Headroom)
	}
}

func TestRightSizingNilWhenNothingFlagged(t *testing.T) {
	pods := []corev1.Pod{pod("prod", "good", "worker1", "100m", "128Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	if rep.RightSizing != nil && len(rep.RightSizing.Rules) > 0 {
		t.Errorf("want no rules for a well-formed workload, got %+v", rep.RightSizing.Rules)
	}
}

// The vocabulary ban is a design constraint, not a style preference: none of these
// words can be justified from a single non-historical sample.
func TestNoForbiddenVocabularyInDetails(t *testing.T) {
	p := pod("prod", "cache-1", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{container("app", "", "", "", "256Mi")}
	pods := []corev1.Pod{p, pod("staging", "web", "worker1", "", ""),
		pod("batch", "big", "", "40", "8Gi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	for _, r := range rep.RightSizing.Rules {
		for _, o := range r.Owners {
			for _, banned := range []string{"peak", "over-requested", "oversized", "waste"} {
				if strings.Contains(strings.ToLower(o.Detail), banned) {
					t.Errorf("detail %q contains banned word %q", o.Detail, banned)
				}
			}
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -run 'TestRule|TestRightSizing|TestNoForbidden'
```

Expected: FAIL — `want a right-sizing block, got nil`.

- [ ] **Step 3: Write `internal/capacity/rightsize.go`**

```go
package capacity

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// buildRightSizing applies the three structural rules and rolls the matches up by
// owner. Every rule is provable from the pod spec alone; no usage data is read
// here, and none of these rules can be satisfied or suppressed by a usage sample.
//
// namespace, when non-empty, scopes the enumeration only — the caller still passes
// the cluster-wide pod list, because included carries cluster-wide arithmetic.
func buildRightSizing(pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	included []nodeCapacity, namespace string) *RightSizing {

	rsIndex := deploymentIndex(replicaSets)
	// key -> owner, so replicas of one workload collapse to a single row.
	matches := map[RuleName]map[string]Owner{
		RuleNoRequests:       {},
		RuleLimitNoRequest:   {},
		RuleNeverSchedulable: {},
	}

	var biggest nodeCapacity
	for _, n := range included {
		if n.cpuAlloc > biggest.cpuAlloc {
			biggest.cpuAlloc = n.cpuAlloc
		}
		if n.memAlloc > biggest.memAlloc {
			biggest.memAlloc = n.memAlloc
		}
	}

	for _, p := range pods {
		if terminal(p) || (namespace != "" && p.Namespace != namespace) {
			continue
		}
		o := ownerOf(p, rsIndex)
		key := o.Kind + "/" + o.Namespace + "/" + o.Name

		if noRequests, allBare := noRequestContainers(p); noRequests {
			e := o
			e.BestEffort = allBare
			// A pod already recorded without BestEffort must not be upgraded by a
			// sibling replica that happens to be bare; take the weaker claim.
			if prev, ok := matches[RuleNoRequests][key]; ok && !prev.BestEffort {
				e.BestEffort = false
			}
			matches[RuleNoRequests][key] = e
		}
		if detail := limitWithoutRequest(p); detail != "" {
			e := o
			e.Detail = detail
			if _, ok := matches[RuleLimitNoRequest][key]; !ok {
				matches[RuleLimitNoRequest][key] = e
			}
		}
		if len(included) > 0 {
			if detail := exceedsLargestNode(p, biggest); detail != "" {
				e := o
				e.Detail = detail
				if _, ok := matches[RuleNeverSchedulable][key]; !ok {
					matches[RuleNeverSchedulable][key] = e
				}
			}
		}
	}

	// Fixed rule order — a literal slice, never a range over the map.
	order := []RuleName{RuleNoRequests, RuleLimitNoRequest, RuleNeverSchedulable}
	var rules []Rule
	for _, name := range order {
		owners := make([]Owner, 0, len(matches[name]))
		for _, o := range matches[name] {
			owners = append(owners, o)
		}
		if len(owners) == 0 {
			continue
		}
		sort.Slice(owners, func(a, b int) bool {
			if owners[a].Namespace != owners[b].Namespace {
				return owners[a].Namespace < owners[b].Namespace
			}
			return owners[a].Name < owners[b].Name
		})
		r := Rule{Name: name}
		if len(owners) > maxOwnersPerRule {
			r.Truncated = len(owners) - maxOwnersPerRule
			owners = owners[:maxOwnersPerRule]
		}
		r.Owners = owners
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return nil
	}
	return &RightSizing{Rules: rules}
}

// noRequestContainers reports whether any container declares neither a CPU nor a
// memory request, and whether every container is like that — the second is what
// makes the pod BestEffort.
func noRequestContainers(p corev1.Pod) (any_, all bool) {
	all = len(p.Spec.Containers) > 0
	for _, c := range p.Spec.Containers {
		bare := c.Resources.Requests.Cpu().IsZero() && c.Resources.Requests.Memory().IsZero()
		if bare {
			any_ = true
		} else {
			all = false
		}
	}
	return any_, all && any_
}

// limitWithoutRequest returns a detail string when a container sets a limit for a
// resource but no request for it. Kubernetes then defaults the request to the
// limit, so the workload reserves the full limit cluster-wide.
func limitWithoutRequest(p corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if !c.Resources.Limits.Cpu().IsZero() && c.Resources.Requests.Cpu().IsZero() {
			return "lim " + formatMilliCPU(c.Resources.Limits.Cpu().MilliValue()) + " cores"
		}
		if !c.Resources.Limits.Memory().IsZero() && c.Resources.Requests.Memory().IsZero() {
			return "lim " + formatBytes(c.Resources.Limits.Memory().Value())
		}
	}
	return ""
}

// exceedsLargestNode returns a detail string when a container requests more of a
// resource than the largest included node can ever offer.
func exceedsLargestNode(p corev1.Pod, biggest nodeCapacity) string {
	for _, c := range p.Spec.Containers {
		if cpu := c.Resources.Requests.Cpu().MilliValue(); cpu > biggest.cpuAlloc {
			return fmt.Sprintf("req %s cores > largest node (%s cores)",
				formatMilliCPU(cpu), formatMilliCPU(biggest.cpuAlloc))
		}
		if mem := c.Resources.Requests.Memory().Value(); mem > biggest.memAlloc {
			return fmt.Sprintf("req %s > largest node (%s)",
				formatBytes(mem), formatBytes(biggest.memAlloc))
		}
	}
	return ""
}

// deploymentIndex maps "namespace/replicaset" to its owning Deployment's name, so a
// pod can be rolled up two levels. ReplicaSets with no Deployment owner are absent
// from the map and the pod rolls up to the ReplicaSet itself.
func deploymentIndex(replicaSets []appsv1.ReplicaSet) map[string]string {
	idx := make(map[string]string, len(replicaSets))
	for _, rs := range replicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			idx[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}
	return idx
}

// ownerOf resolves a pod to the workload a human would name: through a ReplicaSet
// to its Deployment when possible, else the direct controller, else the pod.
func ownerOf(p corev1.Pod, rsIndex map[string]string) Owner {
	o := controllerOwner(p.OwnerReferences)
	if o == nil {
		return Owner{Kind: "Pod", Namespace: p.Namespace, Name: p.Name}
	}
	if o.Kind == "ReplicaSet" {
		if dep, ok := rsIndex[p.Namespace+"/"+o.Name]; ok {
			return Owner{Kind: "Deployment", Namespace: p.Namespace, Name: dep}
		}
	}
	return Owner{Kind: o.Kind, Namespace: p.Namespace, Name: o.Name}
}
```

- [ ] **Step 4: Wire it into `Assess` in `internal/capacity/capacity.go`**

Replace the `Assess` body written in Task 2 with:

```go
func Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	usage map[string]corev1.ResourceList, namespace string) Report {
	included, excluded := classifyNodes(nodes, pods)
	return Report{
		Headroom:    buildHeadroom(included, excluded, len(nodes), pods),
		RightSizing: buildRightSizing(pods, replicaSets, included, namespace),
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -v 2>&1 | tail -40
```

Expected: every test PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/capacity/
git commit -m "feat(capacity): three structural right-sizing rules with owner roll-up"
```

---

### Task 5: `collect.PodMetrics` and sample attachment

**Files:**

- Modify: `internal/collect/collect.go` (add `PodMetrics` after `NodeMetrics` at line 187; add `parsePodMetrics` after `parseNodeMetrics`)
- Modify: `internal/collect/collect_test.go` (add the parser test)
- Create: `internal/capacity/sample.go`
- Create: `internal/capacity/sample_test.go`
- Modify: `internal/capacity/capacity.go` (`Assess` passes `usage` into the sample step)

**Interfaces:**

- Consumes: `RightSizing`, `Owner`, `Rule` from Task 1; `buildRightSizing` from Task 4; `ownerOf`/`deploymentIndex` from Task 4.
- Produces: `collect.PodMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error)` keyed `"namespace/name"`; `attachSamples(rs *RightSizing, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet, usage map[string]corev1.ResourceList, namespace string)`.

**The shape difference from `NodeMetrics`.** A `PodMetricsList` item has no top-level `usage`; usage lives per container under `containers[].usage`, so the parser sums across containers. Getting this wrong yields an empty map that silently reads as "metrics-server absent".

- [ ] **Step 1: Write the failing parser test**

Append to `internal/collect/collect_test.go`:

```go
func TestParsePodMetricsSumsContainers(t *testing.T) {
	body := []byte(`{"items":[
	  {"metadata":{"namespace":"prod","name":"web-1"},
	   "containers":[{"name":"app","usage":{"cpu":"120m","memory":"200Mi"}},
	                 {"name":"sidecar","usage":{"cpu":"30m","memory":"56Mi"}}]},
	  {"metadata":{"namespace":"staging","name":"api-1"},
	   "containers":[{"name":"app","usage":{"cpu":"5m","memory":"32Mi"}}]}
	]}`)

	got, err := parsePodMetrics(body)
	if err != nil {
		t.Fatalf("parsePodMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 pods, got %d", len(got))
	}
	cpu := got["prod/web-1"][corev1.ResourceCPU]
	if cpu.MilliValue() != 150 {
		t.Errorf("want the two containers summed to 150m, got %s", cpu.String())
	}
	mem := got["prod/web-1"][corev1.ResourceMemory]
	if mem.Value() != 256*1024*1024 {
		t.Errorf("want 256Mi summed, got %s", mem.String())
	}
	if _, ok := got["staging/api-1"]; !ok {
		t.Error("want the second pod keyed namespace/name")
	}
}

func TestParsePodMetricsRejectsBadQuantity(t *testing.T) {
	body := []byte(`{"items":[{"metadata":{"namespace":"prod","name":"x"},
	  "containers":[{"name":"app","usage":{"cpu":"not-a-quantity"}}]}]}`)

	if _, err := parsePodMetrics(body); err == nil {
		t.Error("want an error for an unparseable quantity")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestParsePodMetrics
```

Expected: FAIL — `undefined: parsePodMetrics`.

- [ ] **Step 3: Add `PodMetrics` and `parsePodMetrics` to `internal/collect/collect.go`**

Immediately after `NodeMetrics` (which ends at line 187):

```go
// PodMetrics reads live per-pod usage from metrics-server via a raw GET on the
// metrics API, keyed "namespace/name". available is false (and err nil) when
// metrics-server is absent or forbidden, so a scan still succeeds without it —
// the same contract as NodeMetrics.
func PodMetrics(ctx context.Context, client kubernetes.Interface) (map[string]corev1.ResourceList, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/pods").DoRaw(ctx)
	if err != nil {
		return nil, false, nil // metrics-server absent/forbidden — non-fatal
	}
	usage, err := parsePodMetrics(data)
	if err != nil {
		return nil, false, err
	}
	return usage, len(usage) > 0, nil
}
```

And after `parseNodeMetrics`:

```go
// parsePodMetrics decodes a metrics.k8s.io PodMetricsList body into per-pod
// resource quantities keyed "namespace/name". Unlike NodeMetricsList, usage is
// reported per container, so each pod's containers are summed.
func parsePodMetrics(data []byte) (map[string]corev1.ResourceList, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parsing pod metrics: %w", err)
	}
	out := make(map[string]corev1.ResourceList, len(list.Items))
	for _, it := range list.Items {
		rl := corev1.ResourceList{}
		for _, c := range it.Containers {
			for k, v := range c.Usage {
				q, err := resource.ParseQuantity(v)
				if err != nil {
					return nil, fmt.Errorf("parsing usage %q for pod %s/%s: %w",
						v, it.Metadata.Namespace, it.Metadata.Name, err)
				}
				cur := rl[corev1.ResourceName(k)]
				cur.Add(q)
				rl[corev1.ResourceName(k)] = cur
			}
		}
		out[it.Metadata.Namespace+"/"+it.Metadata.Name] = rl
	}
	return out, nil
}
```

- [ ] **Step 4: Write the failing sample-attachment tests**

Create `internal/capacity/sample_test.go`:

```go
package capacity

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func usageOf(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func TestSampleAttachesToFlaggedOwner(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}
	usage := map[string]corev1.ResourceList{"staging/web-1": usageOf("30m", "64Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.0") {
		t.Errorf("want an observed CPU reading, got %q", o.Observed)
	}
	if !rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable true")
	}
	if rep.RightSizing.PodsReporting != 1 || rep.RightSizing.PodsTotal != 1 {
		t.Errorf("want 1 of 1 reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}

// Replicas of one owner are summed, because the row names the workload.
func TestSampleSumsAcrossOwnerReplicas(t *testing.T) {
	pods := []corev1.Pod{
		pod("staging", "web-1", "worker1", "", ""),
		pod("staging", "web-2", "worker1", "", ""),
	}
	pods[0] = ownedBy(pods[0], "StatefulSet", "web")
	pods[1] = ownedBy(pods[1], "StatefulSet", "web")
	usage := map[string]corev1.ResourceList{
		"staging/web-1": usageOf("100m", "64Mi"),
		"staging/web-2": usageOf("400m", "64Mi"),
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "")

	o := rep.RightSizing.Rules[0].Owners[0]
	if !strings.Contains(o.Observed, "0.5") {
		t.Errorf("want the two replicas summed to 0.5 cores, got %q", o.Observed)
	}
}

// THE core constraint: a healthy workload with a tiny sample must never appear.
func TestSampleNeverSelectsAWorkload(t *testing.T) {
	pods := []corev1.Pod{pod("prod", "thrifty", "worker1", "4", "8Gi")}
	usage := map[string]corev1.ResourceList{"prod/thrifty": usageOf("1m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "8", "32Gi")}, pods, nil, usage, "")

	if rep.RightSizing != nil {
		t.Errorf("want no right-sizing block: a sample must never create a row, got %+v",
			rep.RightSizing)
	}
}

func TestSampleAbsentMetricsStillRendersRules(t *testing.T) {
	pods := []corev1.Pod{pod("staging", "web-1", "worker1", "", "")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, nil, "")

	if rep.RightSizing == nil || len(rep.RightSizing.Rules) != 1 {
		t.Fatalf("want the structural rule with no metrics, got %+v", rep.RightSizing)
	}
	if rep.RightSizing.MetricsAvailable {
		t.Error("want MetricsAvailable false")
	}
	if rep.RightSizing.Rules[0].Owners[0].Observed != "" {
		t.Error("want no observed value with no metrics")
	}
}

// Coverage counts the pods the section considered, not every pod in the cluster.
func TestSampleCoverageCountsScopedPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("prod", "a", "worker1", "", ""),
		pod("prod", "b", "worker1", "", ""),
		pod("staging", "c", "worker1", "", ""),
	}
	usage := map[string]corev1.ResourceList{"prod/a": usageOf("10m", "8Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, usage, "prod")

	if rep.RightSizing.PodsTotal != 2 || rep.RightSizing.PodsReporting != 1 {
		t.Errorf("want 1 of 2 in-scope pods reporting, got %d of %d",
			rep.RightSizing.PodsReporting, rep.RightSizing.PodsTotal)
	}
}
```

- [ ] **Step 5: Run them to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ -run TestSample
```

Expected: FAIL — `want an observed CPU reading, got ""`.

- [ ] **Step 6: Write `internal/capacity/sample.go`**

```go
package capacity

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// attachSamples records coverage and writes an observed reading onto owners that a
// structural rule ALREADY flagged. It never adds, removes, or reorders an owner.
//
// This is the whole discipline of the feature: metrics-server returns one sample of
// roughly a 30-second average and keeps no history, so a reading cannot justify
// putting a workload on the list — only annotating one that a provable rule put
// there.
func attachSamples(rs *RightSizing, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	usage map[string]corev1.ResourceList, namespace string) {
	if rs == nil {
		return
	}
	rsIndex := deploymentIndex(replicaSets)

	type total struct{ cpu, mem int64 }
	byOwner := map[string]*total{}
	for _, p := range pods {
		if terminal(p) || (namespace != "" && p.Namespace != namespace) {
			continue
		}
		rs.PodsTotal++
		u, ok := usage[p.Namespace+"/"+p.Name]
		if !ok {
			continue
		}
		rs.PodsReporting++
		o := ownerOf(p, rsIndex)
		key := o.Kind + "/" + o.Namespace + "/" + o.Name
		if byOwner[key] == nil {
			byOwner[key] = &total{}
		}
		cpu := u[corev1.ResourceCPU]
		mem := u[corev1.ResourceMemory]
		byOwner[key].cpu += cpu.MilliValue()
		byOwner[key].mem += mem.Value()
	}
	rs.MetricsAvailable = rs.PodsReporting > 0
	if !rs.MetricsAvailable {
		return
	}
	for ri := range rs.Rules {
		for oi := range rs.Rules[ri].Owners {
			o := &rs.Rules[ri].Owners[oi]
			t, ok := byOwner[o.Kind+"/"+o.Namespace+"/"+o.Name]
			if !ok {
				continue
			}
			o.Observed = observedFor(rs.Rules[ri].Name, t.cpu, t.mem)
		}
	}
}

// observedFor picks the reading that speaks to the rule: the memory rule shows
// memory, everything else shows CPU.
func observedFor(rule RuleName, cpuMilli, memBytes int64) string {
	if rule == RuleLimitNoRequest {
		return formatBytes(memBytes)
	}
	return formatMilliCPU(cpuMilli) + " cores"
}
```

- [ ] **Step 7: Call it from `Assess` in `internal/capacity/capacity.go`**

```go
func Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet,
	usage map[string]corev1.ResourceList, namespace string) Report {
	included, excluded := classifyNodes(nodes, pods)
	rightSizing := buildRightSizing(pods, replicaSets, included, namespace)
	attachSamples(rightSizing, pods, replicaSets, usage, namespace)
	return Report{
		Headroom:    buildHeadroom(included, excluded, len(nodes), pods),
		RightSizing: rightSizing,
	}
}
```

- [ ] **Step 8: Run everything**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/capacity/ ./internal/collect/ 2>&1 | tail -10
```

Expected: both `ok`.

- [ ] **Step 9: Commit**

```bash
git add internal/capacity/ internal/collect/
git commit -m "feat(capacity): per-pod metrics fetch and observed-sample attachment"
```

---

### Task 6: Report section and JSON

**Files:**

- Modify: `internal/report/report.go` — add `Capacity` to **`inventoryReport`** (line ~59, after `GitOps`), add `Capacity` to `Input` (after the `GitOps` field, ~line 134), add `Capacity: in.Capacity` to the `enc.Encode(inventoryReport{...})` literal (~line 172), call `printCapacity` after `printGitOps` (~line 283), and add `printCapacity` + helpers at the end of the file
- Create: `internal/report/capacity_test.go`

**Interfaces:**

- Consumes: everything exported from `internal/capacity`.
- Produces: `Input.Capacity *capacity.Report`; the `capacity` JSON key on `inventoryReport`.

> **Read this before writing the JSON field. This project has shipped the same bug twice.**
> `report.Input` is **never marshalled** — it is the render call's parameter bag, and none of its other fields carry json tags. The struct `--output json` actually encodes is `inventoryReport` (`internal/report/report.go:40`), populated field-by-field in the `enc.Encode(inventoryReport{...})` literal. Putting `json:"capacity,omitempty"` on `Input.Capacity` produces a tag that is never read and a `--output json` that never emits the key. **Both** edits are required: the tagged field on `inventoryReport` *and* the line in the `Encode` literal. Step 1's tests fail if either is missing.

- [ ] **Step 1: Write the failing tests**

Create `internal/report/capacity_test.go`:

```go
package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/capacity"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
)

func sampleCapacity() *capacity.Report {
	return &capacity.Report{
		Headroom: &capacity.Headroom{
			IncludedNodes: 3, TotalNodes: 5,
			FreeCPU: "5.9", FreeMemory: "108Gi",
			LargestCPUFit: &capacity.NodeFit{Node: "worker1", CPU: "2.4", Memory: "42Gi"},
			TightestNode:  &capacity.TightNode{Node: "worker2", Resource: "CPU", Pct: 92},
			NodeLoss: &capacity.NodeLoss{
				Node: "worker1", Fits: false, Placed: 4,
				Blocker: "StatefulSet/prod/db", BlockerCPU: "2.1",
			},
			Excluded: []capacity.NodeExclusion{
				{Node: "control-plane-1", Reason: "NoSchedule taint"},
				{Node: "worker3", Reason: "cordoned"},
			},
		},
		RightSizing: &capacity.RightSizing{
			MetricsAvailable: true, PodsReporting: 14, PodsTotal: 16,
			Rules: []capacity.Rule{
				{Name: capacity.RuleNoRequests, Owners: []capacity.Owner{
					{Kind: "Deployment", Namespace: "staging", Name: "web",
						Observed: "0.0 cores", BestEffort: true},
				}},
				{Name: capacity.RuleNeverSchedulable, Owners: []capacity.Owner{
					{Kind: "Job", Namespace: "batch", Name: "trainer",
						Detail: "req 40.0 cores > largest node (16.0 cores)"},
				}, Truncated: 3},
			},
		},
	}
}

func TestPrintCapacity(t *testing.T) {
	var buf bytes.Buffer
	if err := printCapacity(sampleCapacity(), &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"CAPACITY  (advisory",
		"  Headroom",
		"5.9 cores, 108Gi free across 3 of 5 nodes",
		"worker1  2.4 cores, 42Gi",
		"worker2  92% of CPU requested",
		"may not fit — first-fit could not place StatefulSet/prod/db (2.1 cores)",
		"control-plane-1  (NoSchedule taint)",
		"worker3  (cordoned)",
		"Right-sizing  (metrics-server: 14 of 16 pods reporting)",
		"no requests set",
		"Deployment/staging/web",
		"BestEffort: first evicted under pressure",
		"never schedulable",
		"… +3 more",
		"one sample per pod, ~30s average — not a peak, not a history",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

// FFD failure licenses "may not fit" only. A flat "does not fit" would claim the
// heuristic proved something it cannot.
func TestPrintCapacityNeverSaysDoesNotFit(t *testing.T) {
	var buf bytes.Buffer
	if err := printCapacity(sampleCapacity(), &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if strings.Contains(buf.String(), "does not fit") {
		t.Error(`rendered "does not fit"; first-fit failure only licenses "may not fit"`)
	}
}

func TestPrintCapacityFitsAndSingleNode(t *testing.T) {
	rep := &capacity.Report{Headroom: &capacity.Headroom{
		IncludedNodes: 2, TotalNodes: 2, FreeCPU: "4.0", FreeMemory: "16Gi",
		NodeLoss: &capacity.NodeLoss{Node: "worker1", Fits: true, Placed: 7},
	}}
	var buf bytes.Buffer
	if err := printCapacity(rep, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if !strings.Contains(buf.String(), "fits — first-fit placed all 7 pods") {
		t.Errorf("want the fits wording, got:\n%s", buf.String())
	}

	single := &capacity.Report{Headroom: &capacity.Headroom{
		IncludedNodes: 1, TotalNodes: 1, FreeCPU: "4.0", FreeMemory: "16Gi",
		NodeLoss: &capacity.NodeLoss{Node: "only", SingleNode: true},
	}}
	buf.Reset()
	if err := printCapacity(single, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	if !strings.Contains(buf.String(), "single node — no node-loss arithmetic possible") {
		t.Errorf("want the single-node wording, got:\n%s", buf.String())
	}
}

func TestPrintCapacityAbsentMetrics(t *testing.T) {
	rep := &capacity.Report{RightSizing: &capacity.RightSizing{
		MetricsAvailable: false, PodsTotal: 9,
		Rules: []capacity.Rule{{Name: capacity.RuleNoRequests,
			Owners: []capacity.Owner{{Kind: "Pod", Namespace: "default", Name: "loose"}}}},
	}}
	var buf bytes.Buffer
	if err := printCapacity(rep, &buf); err != nil {
		t.Fatalf("printCapacity: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "metrics-server unavailable — structural rules only") {
		t.Errorf("want the unavailable line, got:\n%s", out)
	}
	if !strings.Contains(out, "Pod/default/loose") {
		t.Error("want the structural row to render anyway")
	}
	if strings.Contains(out, "not a peak") {
		t.Error("want no sample footer when there is no sample")
	}
}

func TestPrintCapacitySkipsEmpty(t *testing.T) {
	for name, rep := range map[string]*capacity.Report{
		"nil":   nil,
		"empty": {},
	} {
		var buf bytes.Buffer
		if err := printCapacity(rep, &buf); err != nil {
			t.Fatalf("%s: printCapacity: %v", name, err)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: want no output, got %q", name, buf.String())
		}
	}
}

func TestPrintInventoryJSONIncludesCapacity(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:  clusterhealth.ClusterHealth{Verdict: "Healthy"},
		Capacity: sampleCapacity(),
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	raw, ok := got["capacity"]
	if !ok {
		t.Fatalf("want a capacity key in --output json, got keys %v", keysOf(got))
	}
	if !strings.Contains(string(raw), `"headroom"`) {
		t.Errorf("want the headroom block encoded, got %s", raw)
	}
}

func TestPrintInventoryJSONOmitsCapacityWhenNil(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatalf("PrintInventory: %v", err)
	}
	if strings.Contains(buf.String(), `"capacity"`) {
		t.Error("a default scan's JSON must be unchanged")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run TestPrintCapacity
```

Expected: FAIL — `undefined: printCapacity`.

- [ ] **Step 3: Add the two struct fields and the two wiring lines**

In `inventoryReport`, immediately after the `GitOps` line:

```go
	Capacity           *capacity.Report            `json:"capacity,omitempty"`
```

In `Input`, immediately after the `GitOps` field:

```go
	// Capacity is the advisory headroom and right-sizing view (opt-in --capacity).
	// Nil when the flag is off, so a default scan's output is unchanged. No json
	// tag: Input is never marshalled — the encoded struct is inventoryReport.
	Capacity *capacity.Report
```

In the `enc.Encode(inventoryReport{...})` literal, immediately after `GitOps: in.GitOps,`:

```go
			Capacity:           in.Capacity,
```

In `PrintInventory`'s text path, immediately after the `printGitOps` block:

```go
	if err := printCapacity(in.Capacity, w); err != nil {
		return err
	}
```

Add `"github.com/imantaba/kubeagent/internal/capacity"` to the import block.

- [ ] **Step 4: Write `printCapacity` and its helpers at the end of `internal/report/report.go`**

```go
// printCapacity renders the advisory CAPACITY section (opt-in --capacity): the
// headroom arithmetic over schedulable nodes, then the structural right-sizing
// rules.
//
// Advisory, exactly like OPERATORS and GITOPS DRIFT: it never sets hasAttention,
// never changes the cluster verdict, and takes no part in the all-clear
// suppression. Two claims it must never make — money, which no cluster publishes,
// and a peak, which a single metrics-server sample cannot establish.
func printCapacity(rep *capacity.Report, w io.Writer) error {
	if rep == nil || (rep.Headroom == nil && rep.RightSizing == nil) {
		return nil
	}
	if _, err := fmt.Fprint(w,
		"CAPACITY  (advisory — resource arithmetic on requests; ignores affinity,\n"+
			"           topology spread, PVC zoning, and PodDisruptionBudgets)\n"); err != nil {
		return err
	}
	if err := printHeadroomBlock(rep.Headroom, w); err != nil {
		return err
	}
	if err := printRightSizingBlock(rep.RightSizing, w); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

func printHeadroomBlock(h *capacity.Headroom, w io.Writer) error {
	if h == nil {
		return nil
	}
	if _, err := fmt.Fprintln(w, "  Headroom"); err != nil {
		return err
	}
	if err := capacityRow(w, "schedulable", fmt.Sprintf("%s cores, %s free across %d of %d nodes",
		h.FreeCPU, h.FreeMemory, h.IncludedNodes, h.TotalNodes)); err != nil {
		return err
	}
	if h.LargestCPUFit != nil {
		if err := capacityRow(w, "largest pod fit", fmt.Sprintf("%s  %s cores, %s",
			h.LargestCPUFit.Node, h.LargestCPUFit.CPU, h.LargestCPUFit.Memory)); err != nil {
			return err
		}
	}
	if h.LargestMemFit != nil {
		if err := capacityRow(w, "", fmt.Sprintf("%s  %s cores, %s",
			h.LargestMemFit.Node, h.LargestMemFit.CPU, h.LargestMemFit.Memory)); err != nil {
			return err
		}
	}
	if h.TightestNode != nil {
		if err := capacityRow(w, "tightest node", fmt.Sprintf("%s  %d%% of %s requested",
			h.TightestNode.Node, h.TightestNode.Pct, h.TightestNode.Resource)); err != nil {
			return err
		}
	}
	if nl := h.NodeLoss; nl != nil {
		if err := capacityRow(w, "lose "+nl.Node, nodeLossDetail(*nl)); err != nil {
			return err
		}
	}
	for i, e := range h.Excluded {
		label := ""
		if i == 0 {
			label = "excluded"
		}
		if err := capacityRow(w, label, fmt.Sprintf("%s  (%s)", e.Node, e.Reason)); err != nil {
			return err
		}
	}
	return nil
}

// nodeLossDetail respects the one-sided soundness of first-fit-decreasing: a
// successful pass is a constructive placement and so proves the requests fit, while
// a failed pass proves nothing — hence "may not fit", never "does not fit".
func nodeLossDetail(nl capacity.NodeLoss) string {
	switch {
	case nl.SingleNode:
		return "single node — no node-loss arithmetic possible"
	case nl.Fits:
		return fmt.Sprintf("fits — first-fit placed all %d pods", nl.Placed)
	default:
		return fmt.Sprintf("may not fit — first-fit could not place %s (%s cores)",
			nl.Blocker, nl.BlockerCPU)
	}
}

func printRightSizingBlock(rs *capacity.RightSizing, w io.Writer) error {
	if rs == nil || len(rs.Rules) == 0 {
		return nil
	}
	header := "  Right-sizing"
	if rs.MetricsAvailable {
		header += fmt.Sprintf("  (metrics-server: %d of %d pods reporting)",
			rs.PodsReporting, rs.PodsTotal)
	} else {
		header += "  (metrics-server unavailable — structural rules only)"
	}
	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}
	for _, r := range rs.Rules {
		for i, o := range r.Owners {
			label := ""
			if i == 0 {
				label = capacityRuleLabel(r.Name)
			}
			value := fmt.Sprintf("%s/%s/%s", o.Kind, o.Namespace, o.Name)
			if o.Detail != "" {
				value += "  " + o.Detail
			}
			if o.Observed != "" {
				value += "  · " + o.Observed + " observed"
			}
			if err := capacityRow(w, label, value); err != nil {
				return err
			}
			if o.BestEffort {
				if _, err := fmt.Fprintln(w,
					strings.Repeat(" ", 24)+"— BestEffort: first evicted under pressure"); err != nil {
					return err
				}
			}
		}
		if r.Truncated > 0 {
			if err := capacityRow(w, "", fmt.Sprintf("… +%d more", r.Truncated)); err != nil {
				return err
			}
		}
	}
	if rs.MetricsAvailable {
		if _, err := fmt.Fprintf(w,
			"\n    one sample per pod, ~30s average — not a peak, not a history\n"); err != nil {
			return err
		}
	}
	return nil
}

// capacityRuleLabel maps a rule constant to its human label. The constants are
// stable JSON keys; these strings are presentation only.
func capacityRuleLabel(n capacity.RuleName) string {
	switch n {
	case capacity.RuleNoRequests:
		return "no requests set"
	case capacity.RuleLimitNoRequest:
		return "limit, no request"
	case capacity.RuleNeverSchedulable:
		return "never schedulable"
	default:
		return string(n)
	}
}

// capacityRow prints one label/value line at the section's fixed indent. An empty
// label produces a continuation line aligned under the previous value. Labels wider
// than the column still get two separating spaces rather than running together.
func capacityRow(w io.Writer, label, value string) error {
	const width = 18
	pad := width - len(label)
	if pad < 2 {
		pad = 2
	}
	_, err := fmt.Fprintf(w, "    %s%s%s\n", label, strings.Repeat(" ", pad), value)
	return err
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run 'TestPrintCapacity|TestPrintInventoryJSON' -v 2>&1 | tail -20
```

Expected: all PASS.

- [ ] **Step 6: Prove the section stays out of the verdict**

```bash
grep -n 'Capacity' internal/report/report.go | grep -i 'hasAttention\|Verdict'
```

Expected: no output. The section must appear in neither.

- [ ] **Step 7: Commit**

```bash
git add internal/report/
git commit -m "feat(report): render the advisory CAPACITY section and its JSON"
```

---

### Task 7: CLI wiring — `--capacity`

**Files:**

- Modify: `main.go` — flag registration (after `driftAge`, ~line 89), the usage string (line 66), and the `capacity.Assess` call (after the operators/GitOps block)
- Modify: `main_test.go` — flag-registration and usage tests

**Interfaces:**

- Consumes: `capacity.Assess(nodes, pods, replicaSets, usage, namespace)` from Task 5; `collect.PodMetrics` from Task 5; `Input.Capacity` from Task 6.
- Produces: nothing later tasks consume.

> **The usage string is a real shipped surface. The previous slice omitted its two flags from `main.go:66` and the whole-branch review caught it as an Important finding.** Do not skip Step 3. `main_test.go`'s existing `TestRun_UsageMentionsDriftFlag` asserts on the exact substring `[--operators] [--drift] [--drift-age dur] [--logs]`; inserting `--capacity` between them breaks that assertion, and updating it is part of this task.

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go`, beside the existing `--drift` pair:

```go
func TestRun_CapacityFlagAccepted(t *testing.T) {
	// --capacity must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed rather than erroring with
	// "flag provided but not defined".
	err := run([]string{"scan", "--capacity", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}

func TestRun_UsageMentionsCapacityFlag(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("expected a usage error with no args")
	}
	if !strings.Contains(err.Error(), "[--drift-age dur] [--capacity] [--logs]") {
		t.Fatalf("expected the usage string to mention --capacity between --drift-age and --logs, got: %v", err)
	}
}
```

Update the existing `TestRun_UsageMentionsDriftFlag` assertion from

```go
	if !strings.Contains(err.Error(), "[--operators] [--drift] [--drift-age dur] [--logs]") {
```

to

```go
	if !strings.Contains(err.Error(), "[--operators] [--drift] [--drift-age dur] [--capacity]") {
```

- [ ] **Step 2: Run them to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'TestRun_Capacity|TestRun_UsageMentions'
```

Expected: FAIL — `flag provided but not defined: -capacity`, and the usage assertions fail.

- [ ] **Step 3: Register the flag and fix the usage string in `main.go`**

After the `driftAge` line (~line 89):

```go
	capacityFlag := fs.Bool("capacity", envBool("KUBEAGENT_CAPACITY", false), "report scheduling headroom and structurally wrong workload shapes (advisory; uses metrics-server for context when present)")
```

In the usage string at line 66, insert `[--capacity]` immediately after `[--drift-age dur]`, so that fragment reads:

```
[--operators] [--drift] [--drift-age dur] [--capacity] [--logs]
```

- [ ] **Step 4: Call `capacity.Assess` in `main.go`**

Immediately after the operators/GitOps block closes:

```go
	// Capacity hints: opt-in and advisory. Nodes, pods and ReplicaSets are already
	// collected for this scan, so headroom costs no extra API call; only the
	// per-pod metrics read is new, and its absence is not an error.
	var capacityRep *capacity.Report
	if *capacityFlag {
		podUsage, _, perr := collect.PodMetrics(context.Background(), client)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: warning: pod metrics unavailable: %v\n", perr)
		}
		rep := capacity.Assess(nodes, resourcePods, res.Inputs.ReplicaSets, podUsage, namespace)
		capacityRep = &rep
	}
```

`resourcePods` is the cluster-wide pod slice `main.go` already builds for the resources summary (it refetches all pods when `-n` is set), which is exactly what `Assess` needs: cluster-wide arithmetic, namespace-scoped enumeration.

Add `"github.com/imantaba/kubeagent/internal/capacity"` to the imports, and set the field where the other advisory views are set:

```go
	in.Capacity = capacityRep
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test . -run 'TestRun_' -v 2>&1 | tail -20
```

Expected: all `TestRun_*` PASS.

- [ ] **Step 6: Smoke the flag against no cluster**

```bash
go build -o kubeagent . && ./kubeagent scan --capacity --kubeconfig /nonexistent 2>&1 | head -3
```

Expected: a connect error, not a panic and not `flag provided but not defined`.

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): --capacity flag wiring the advisory capacity view"
```

---

### Task 8: Docs, golden fixture, changelog, roadmap

**Files:**

- Create: `website/docs/features/capacity.md`
- Modify: `website/mkdocs.yml` (nav entry beside `features/gitops-drift.md`)
- Modify: `README.md` (flag row beside `--drift`)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/roadmap.md` (Theme F now complete)
- Modify: `internal/report/golden_test.go` (add a `Capacity` value to the fixture Input)
- Regenerate: `internal/report/testdata/golden-scan.txt`

> **On sibling doc surfaces.** The previous slice's doc task omitted `deploy/README.md`, which carries one section per scan-only RBAC add-on, and the gap reached review. Check it here too — but for **this** feature the correct answer is deliberately "no change": `--capacity` ships **no** `deploy/rbac-*.yaml`, because nodes and pods are already granted and the `metrics.k8s.io` read needs nothing beyond the existing node-metrics path. Do not invent an RBAC file or a `deploy/README.md` section. Confirm the decision by running `grep -n 'rbac-' deploy/README.md` and satisfying yourself every listed file still exists.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/capacity.md`, following the structure of `website/docs/features/gitops-drift.md`: an opening statement of the question it answers, a rendered example, the two premises it refuses (no money, no peak) stated early, the Headroom row table with the exclusion rule, the first-fit asymmetry, the three right-sizing rules with their rationale and the two rules deliberately excluded, the sample-never-selects discipline, and a closing "what it deliberately does not do" list (never a Finding, never remediates, not wired into `watch`, no new RBAC).

The example block must match what `printCapacity` actually renders — copy it from the golden fixture after Step 4 rather than writing it by hand.

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, add the page immediately after the GitOps drift entry:

```yaml
      - GitOps drift: features/gitops-drift.md
      - Capacity hints: features/capacity.md
```

- [ ] **Step 3: Add the golden fixture data**

In `internal/report/golden_test.go`, add a `Capacity` field to the fixture `Input`, immediately after the existing `GitOps:` block:

```go
		Capacity: &capacity.Report{
			Headroom: &capacity.Headroom{
				IncludedNodes: 2, TotalNodes: 3,
				FreeCPU: "5.9", FreeMemory: "8Gi",
				LargestCPUFit: &capacity.NodeFit{Node: "worker1", CPU: "3.5", Memory: "6Gi"},
				TightestNode:  &capacity.TightNode{Node: "worker2", Resource: "memory", Pct: 78},
				NodeLoss: &capacity.NodeLoss{
					Node: "worker1", Fits: false, Placed: 3,
					Blocker: "StatefulSet/prod/db", BlockerCPU: "2.1",
				},
				Excluded: []capacity.NodeExclusion{
					{Node: "control-plane-1", Reason: "NoSchedule taint"},
				},
			},
			RightSizing: &capacity.RightSizing{
				MetricsAvailable: true, PodsReporting: 11, PodsTotal: 12,
				Rules: []capacity.Rule{
					{Name: capacity.RuleNoRequests, Owners: []capacity.Owner{
						{Kind: "Deployment", Namespace: "staging", Name: "web",
							Observed: "0.0 cores", BestEffort: true},
					}},
					{Name: capacity.RuleLimitNoRequest, Owners: []capacity.Owner{
						{Kind: "Deployment", Namespace: "prod", Name: "cache",
							Detail: "lim 256Mi", Observed: "240Mi"},
					}},
				},
			},
		},
```

Add the `capacity` import to that file.

- [ ] **Step 4: Regenerate the golden file**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report -run TestGoldenScanOutput -update
git diff internal/report/testdata/golden-scan.txt
```

Expected: a pure insertion of the `CAPACITY` block after the `GITOPS DRIFT` block, with no other line changed. If any pre-existing line moves or changes, stop — something other than an added section changed, and that needs explaining before it is committed.

- [ ] **Step 5: Verify the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./... 2>&1 | grep -v '^ok' | head
```

Expected: no output.

- [ ] **Step 6: Add the README row**

In the flag table in `README.md`, add a row immediately after `--drift`:

```markdown
| `--capacity` | Scheduling headroom and structurally wrong workload shapes (advisory) |
```

Match the exact column layout of the surrounding rows.

- [ ] **Step 7: Add the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`, add an `### Added` bullet in the style of the `--drift` entry: what the section reports, the two sub-blocks, the no-money and no-peak premises, that the sample never selects a workload, that it is advisory and changes no verdict or exit code, and that it needs no new RBAC.

- [ ] **Step 8: Mark Theme F complete in the roadmap**

In `website/docs/roadmap.md`, update the Theme F text at line ~421 — `cost/right-sizing, and scheduling-headroom hints still to come` is no longer true once this ships. Rewrite that sentence to record Theme F as complete and name the three slices (operator/CRD adapters, GitOps drift, capacity hints).

- [ ] **Step 9: Build the docs site**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | grep -iE 'warning|error|built')
```

Expected: `Documentation built in …`, and no `WARNING` naming `capacity.md`. The red "Material for MkDocs" banner is cosmetic.

- [ ] **Step 10: Commit**

```bash
git add website/ README.md CHANGELOG.md internal/report/
git commit -m "docs(capacity): feature page, golden fixture, changelog, roadmap"
```

---

### Task 9: Chaos scenario 18

**Files:**

- Modify: `chaos/run.sh` (add `scenario_18_capacity`, register it in `run_scenarios`, widen the `--only` comment to `(01..18)`)
- Modify: `chaos/README.md` (scenario count `(1..17)` → `(1..18)`, new table row)

**Interfaces:**

- Consumes: the shipped `--capacity` flag from Task 7.
- Produces: `scenario_18_capacity`, registered in `run_scenarios`.

Read `scenario_17_gitops` in `chaos/run.sh` first and mirror its idiom exactly: every `kubectl` pinned with `--context "$CTX"`, heredocs on every `apply -f -`, and a `record` call naming the gate checks.

**Why this scenario matters.** The Kind cluster runs **no metrics-server**, so this exercises the absent-metrics path — the one a bare Kind or air-gapped user actually hits — rather than the enriched path unit tests already cover.

- [ ] **Step 1: Write the scenario**

Add before `run_scenarios`:

```bash
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

  local out body
  out="$(scan --capacity 2>&1 || true)"
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'CAPACITY section:              %s\n' "$(printf '%s\n' "$body" | grep -m1 'CAPACITY' || true)"
    printf 'headroom schedulable:          %s\n' "$(printf '%s\n' "$body" | grep -m1 'schedulable' || true)"
    printf 'control-plane excluded:        %s\n' "$(printf '%s\n' "$body" | grep -cE 'control-plane.*NoSchedule taint' || true)"
    printf 'no requests set (besteffort):  %s\n' "$(printf '%s\n' "$body" | grep -c "Deployment/$ns/besteffort" || true)"
    printf 'limit, no request (limitonly): %s\n' "$(printf '%s\n' "$body" | grep -c "Deployment/$ns/limitonly" || true)"
    printf 'never schedulable (trainer):   %s\n' "$(printf '%s\n' "$body" | grep -c "Job/$ns/trainer" || true)"
    printf 'metrics-server unavailable:    %s\n' "$(printf '%s\n' "$body" | grep -c 'metrics-server unavailable' || true)"
    printf 'no banned vocabulary:          %s\n' "$(printf '%s\n' "$body" | grep -ciE 'peak|over-requested|oversized|waste' || true)"
    printf 'cluster verdict:               %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
  } | record "18. Capacity hints (--capacity)" "detected: all three structural rules; metrics-server absent path; banned vocabulary absent (0)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}
```

- [ ] **Step 2: Register it**

In `run_scenarios`, add `18_capacity` after `17_gitops` and before `01_etcd`:

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 01_etcd)
```

Widen the `--only` comment from `(01..17)` to `(01..18)`.

- [ ] **Step 3: Verify the harness parses**

```bash
bash -n chaos/run.sh
```

Expected: no output.

- [ ] **Step 4: Run it against a live Kind cluster**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate --only 18
```

Expected in `docs/testing/chaos-results.md`: `1` for each of the three structural rows and for the control-plane exclusion and the unavailable line, `0` for the banned-vocabulary check, and an unchanged cluster verdict.

If `Job/chaos-capacity/trainer` does not appear, the cluster's largest node has more than 40 allocatable cores — raise the Job's request above the real node size rather than lowering the bar for the rule.

- [ ] **Step 5: Document the scenario**

Add a row to the scenario table in `chaos/README.md` matching the existing rows' wording, and update the count `(1..17)` → `(1..18)`.

- [ ] **Step 6: Commit**

```bash
git add chaos/
git commit -m "test(chaos): scenario 18 — capacity hints with no metrics-server"
```

---

## Self-Review

**Spec coverage.**

| Spec section | Task |
| --- | --- |
| No money / no peak premise | Global Constraints; enforced by tests in 4 and rendered wording in 6; documented in 8 |
| `internal/capacity` package, `Assess` signature | 1, 2 (signature), 4, 5 |
| `collect.PodMetrics` mirroring `NodeMetrics` | 5 |
| CLI-composed-view wiring, no watch/RBAC change | 7 (wiring), 8 (the deliberate no-RBAC decision) |
| Advisory only, no Finding, no verdict change | 6 (Step 6 grep), 9 (verdict gate check) |
| Node inclusion rules and exclusion reasons | 1 |
| Headroom rows: schedulable, largest fit, tightest | 2 |
| `largest pod fit` never mixes nodes | 2 (`TestHeadroomLargestFitNeverMixesNodes`) |
| Node-loss by FFD, DaemonSet exclusion, one-sided soundness | 3, wording in 6 |
| Single-node and zero-node edge cases | 2, 3, 6 |
| Three structural rules, two deliberate exclusions | 4 |
| Owner roll-up through ReplicaSet to Deployment | 4 |
| Ordering, 20-cap, `… +N more` | 4 |
| `-n` scopes enumeration only | 4, 5 |
| Sample attached, never selecting | 5 (`TestSampleNeverSelectsAWorkload`) |
| Coverage line and absent-metrics path | 5, 6, 9 |
| Fixed footer wording | 6 |
| Text section placement and format | 6 |
| JSON on `inventoryReport`, not `Input` | 6 (callout + two tests) |
| Golden fixture | 8 |
| Docs, mkdocs nav, README, CHANGELOG, roadmap | 8 |
| Chaos scenario | 9 |

**Placeholders.** None: every step carries the code or the exact command. Task 8 Step 1 describes a prose document by its required structure rather than pasting 170 lines of Markdown — the structure list is exhaustive and the example block is specified as "copy from the golden fixture", which is a concrete instruction, not a TBD.

**Type consistency.** `Assess(nodes []corev1.Node, pods []corev1.Pod, replicaSets []appsv1.ReplicaSet, usage map[string]corev1.ResourceList, namespace string) Report` is identical in Tasks 2, 4, 5, and 7. `nodeCapacity` fields (`name`, `cpuAlloc`, `memAlloc`, `cpuReq`, `memReq`) are used consistently in Tasks 1–4. `RuleName` constants are used, never their display strings, everywhere except `capacityRuleLabel` in Task 6. `formatMilliCPU`/`formatBytes` (Task 2) are used in Tasks 3, 4, and 5. `NodeLoss.Fits`/`SingleNode`/`Blocker`/`BlockerCPU` match between Tasks 3 and 6.

**Known ordering dependency.** Task 2 leaves `Headroom.NodeLoss` nil and `buildHeadroom`'s `pods` parameter unused; Task 3 adds `nodeloss.go` and the one call line that fills it. No stub, no placeholder, and nothing temporary to delete — a reviewer of Task 2 should expect the unused parameter and the nil field, both explained by a comment in the code.

**One deliberate divergence from the spec**, already stated in Global Constraints: `Assess` takes `replicaSets` because spec §3's Deployment roll-up cannot be computed from pods alone.
