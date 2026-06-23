# kubeagent v3 — Phase B Plan: core workload inventory

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `kubeagent scan` print a grouped workload inventory (Deployments, StatefulSets, DaemonSets, and bare pods) — each workload's ready/desired replicas, aggregated restarts + last-restart time, per-pod rows, and any integrated detector findings — in text and JSON; rework `--explain` to summarize only the notable workloads.

**Architecture:** A new `internal/inventory` package groups pods into `Workload`s via `ownerReferences` (resolving ReplicaSet→Deployment), reads controller `.status` for ready/desired, aggregates restarts, and attaches detector findings. `collect` lists the controllers; `report` renders the grouped view; `explain` filters to notable workloads. Detectors are unchanged (still per-pod). The final task rewires `main` to the new pipeline and deletes the old findings-only path.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (typed AppsV1/CoreV1 + fake clientset), stdlib `flag`, existing `anthropic-sdk-go`. No new dependency.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go 1.26.
- **Read-only:** only `List`/`Get`; never mutate cluster resources.
- **Sequential:** no goroutines; the extra `List` calls run one after another.
- CLI: standard-library `flag` only — no Cobra.
- **Phase B kinds only:** Deployments, StatefulSets, DaemonSets, and bare pods. Nodes are Phase C; Jobs/CronJobs are Phase D — do not add them here.
- **Ready/Desired from controller `.status`:** Deployment/StatefulSet → `.status.Replicas` / `.status.ReadyReplicas`; DaemonSet → `.status.DesiredNumberScheduled` / `.status.NumberReady`; pod-derived workloads (bare pods / unlisted owners) → counts from their pods.
- **Restart aggregation:** `Restarts` = Σ container `restartCount`; `LastRestart` = max `LastTerminationState.Terminated.FinishedAt` (RFC3339 UTC, `""` when none).
- **Egress (for `--explain`):** only the notable workloads' structured fields are sent — never raw pod specs, env vars, or secrets.
- **Flagged-first ordering:** workloads with findings or `Ready<Desired` sort before healthy ones.
- Each task keeps `go build ./...` and `go test ./...` green.

---

## File Structure

- `internal/inventory/inventory.go` — **new.** `Workload`, `PodRow`, `Inputs` types; pure helpers; `Assemble`.
- `internal/inventory/inventory_test.go` — **new.** Helper + `Assemble` unit tests (fake pods/controllers).
- `internal/collect/collect.go` — **modify.** Add `CollectInventory` (lists controllers+pods) and `FactsFrom` (pods→`[]diagnose.PodFacts`).
- `internal/collect/collect_test.go` — **modify.** Fake-clientset tests for the new functions.
- `internal/report/report.go` — **modify.** Add `PrintInventory` (grouped text + JSON object). The old `Print` is removed in Task 6.
- `internal/report/report_test.go` — **modify.** Tests for `PrintInventory`.
- `internal/explain/explain.go` — **modify.** Add `Notable`, `(*Client).ExplainInventory`, `buildInventoryPrompt`. Old `Explain`/`buildPrompt` removed in Task 6.
- `internal/explain/explain_test.go` — **modify.** Tests for `Notable` + `ExplainInventory`.
- `main.go` / `main_test.go` — **modify (Task 6).** Rewire pipeline; delete dead code.
- `README.md` / `docs/design.md` — **modify (Task 6).** Document the new scan output.

---

## Task 1: `inventory` — types + pure helpers

**Files:**
- Create: `internal/inventory/inventory.go`
- Test: `internal/inventory/inventory_test.go`

**Interfaces:**
- Produces: types `Workload`, `PodRow`, `Inputs`; `(Workload).Flagged() bool`; unexported helpers `termTime`, `humanAge`, `controllerOwner`, `podRestarts`, `podReady`, `podIsReady`, `podImage`, `workloadStatus`.
- Consumes: `diagnose.Finding` (fields `Pod`, `Issue`, `Reason`, `Evidence`).

- [ ] **Step 1: Write the failing tests**

Create `internal/inventory/inventory_test.go`:

```go
package inventory

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTermTime(t *testing.T) {
	if got := termTime(metav1.Time{}); got != "" {
		t.Errorf("zero time: got %q, want empty", got)
	}
	ts := metav1.Date(2026, 6, 22, 8, 14, 3, 0, time.UTC)
	if got := termTime(ts); got != "2026-06-22T08:14:03Z" {
		t.Errorf("got %q, want RFC3339 UTC", got)
	}
}

func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"days", now.Add(-36 * 24 * time.Hour), "36d"},
		{"hours", now.Add(-5 * time.Hour), "5h"},
		{"minutes", now.Add(-3 * time.Minute), "3m"},
		{"seconds", now.Add(-10 * time.Second), "10s"},
		{"future clamps to 0s", now.Add(time.Hour), "0s"},
	}
	for _, c := range cases {
		if got := humanAge(c.t, now); got != c.want {
			t.Errorf("%s: humanAge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestControllerOwner(t *testing.T) {
	yes := true
	no := false
	refs := []metav1.OwnerReference{
		{Kind: "Node", Name: "n1", Controller: &no},
		{Kind: "ReplicaSet", Name: "rs1", Controller: &yes},
	}
	if o := controllerOwner(refs); o == nil || o.Kind != "ReplicaSet" {
		t.Errorf("expected the controller ref (ReplicaSet), got %+v", o)
	}
	if o := controllerOwner(nil); o != nil {
		t.Errorf("expected nil for no refs, got %+v", o)
	}
}

func TestPodRestarts(t *testing.T) {
	t1 := metav1.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := metav1.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) // later
	p := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{RestartCount: 31, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t1}}},
		{RestartCount: 1, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: t2}}},
	}}}
	n, last := podRestarts(p)
	if n != 32 {
		t.Errorf("total restarts = %d, want 32", n)
	}
	if termTime(last) != "2026-06-10T00:00:00Z" {
		t.Errorf("last restart = %q, want the later time", termTime(last))
	}
}

func TestPodReadyAndIsReady(t *testing.T) {
	p := corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Ready: true}, {Ready: false},
		}},
	}
	if got := podReady(p); got != "1/2" {
		t.Errorf("podReady = %q, want 1/2", got)
	}
	if podIsReady(p) {
		t.Error("podIsReady should be false when a container is not ready")
	}
	p.Status.ContainerStatuses[1].Ready = true
	if !podIsReady(p) {
		t.Error("podIsReady should be true when all containers are ready")
	}
}

func TestWorkloadStatusAndFlagged(t *testing.T) {
	if workloadStatus(3, 3) != "Running" {
		t.Error("3/3 should be Running")
	}
	if workloadStatus(1, 2) != "Degraded" {
		t.Error("1/2 should be Degraded")
	}
	healthy := Workload{Ready: 3, Desired: 3}
	if healthy.Flagged() {
		t.Error("healthy workload should not be flagged")
	}
	degraded := Workload{Ready: 1, Desired: 2}
	if !degraded.Flagged() {
		t.Error("degraded workload should be flagged")
	}
	withFinding := Workload{Ready: 1, Desired: 1, Findings: []struct{}{}}
	_ = withFinding // placeholder replaced below
}
```

Note: replace the final `withFinding` block — `Findings` is `[]diagnose.Finding`. Use:

```go
	withFinding := Workload{Ready: 1, Desired: 1, Findings: []diagnose.Finding{{Pod: "ns/p", Issue: "X"}}}
	if !withFinding.Flagged() {
		t.Error("a workload with a finding should be flagged even when ready==desired")
	}
```

and add the import `"github.com/imantaba/kubeagent/internal/diagnose"`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory/ 2>&1 | tail -8
```
Expected: FAIL — compile error: undefined types/helpers (`Workload`, `termTime`, etc.).

- [ ] **Step 3: Write the implementation**

Create `internal/inventory/inventory.go`:

```go
// Package inventory groups a cluster's pods into workloads (Deployments,
// StatefulSets, DaemonSets, and bare pods), computing replica health and
// restart history, and attaches detector findings to the owning workload.
package inventory

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// PodRow is one pod under a workload.
type PodRow struct {
	Name        string
	Phase       string
	Ready       string // "1/1"
	Restarts    int
	LastRestart string // RFC3339 UTC, "" if none
	Node        string
	IP          string
	Age         string
	Image       string
}

// Workload is one controller (or bare pod) and its aggregated health.
type Workload struct {
	Namespace   string
	Name        string
	Kind        string // Deployment | StatefulSet | DaemonSet | ReplicaSet | Pod
	Desired     int
	Ready       int
	Status      string // Running | Degraded
	Restarts    int
	LastRestart string
	Image       string
	Pods        []PodRow
	Findings    []diagnose.Finding
}

// Flagged reports whether the workload needs attention: it has a detector
// finding or is not fully ready.
func (w Workload) Flagged() bool {
	return len(w.Findings) > 0 || w.Ready < w.Desired
}

func termTime(t metav1.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

func humanAge(t, now time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

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

func podRestarts(p corev1.Pod) (int, metav1.Time) {
	total := 0
	var last metav1.Time
	for _, cs := range p.Status.ContainerStatuses {
		total += int(cs.RestartCount)
		if term := cs.LastTerminationState.Terminated; term != nil {
			if last.IsZero() || term.FinishedAt.After(last.Time) {
				last = term.FinishedAt
			}
		}
	}
	return total, last
}

func podReady(p corev1.Pod) string {
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
}

func podIsReady(p corev1.Pod) bool {
	if len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func podImage(p corev1.Pod) string {
	if len(p.Spec.Containers) > 0 {
		return p.Spec.Containers[0].Image
	}
	return ""
}

func workloadStatus(ready, desired int) string {
	if desired > 0 && ready >= desired {
		return "Running"
	}
	return "Degraded"
}
```

(The `Inputs` type and `Assemble` are added in Task 2.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/inventory/ -v 2>&1 | tail -25
go vet ./internal/inventory/
```
Expected: PASS — all helper tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/
git commit -m "feat(inventory): workload/pod types and pure helpers" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `inventory.Assemble` — grouping & aggregation

**Files:**
- Modify: `internal/inventory/inventory.go`
- Modify: `internal/inventory/inventory_test.go`

**Interfaces:**
- Produces: `type Inputs struct { Pods []corev1.Pod; Deployments []appsv1.Deployment; ReplicaSets []appsv1.ReplicaSet; StatefulSets []appsv1.StatefulSet; DaemonSets []appsv1.DaemonSet }`; `func Assemble(in Inputs, findings []diagnose.Finding) []Workload`.
- Consumes: the helpers from Task 1.

- [ ] **Step 1: Write the failing test**

Add to `internal/inventory/inventory_test.go` (add imports `appsv1 "k8s.io/api/apps/v1"`):

```go
func ctrlRef(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

func TestAssemble_DeploymentGroupsPodsAndAggregates(t *testing.T) {
	in := Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cattle-system", Name: "rancher"},
			Status:     appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 3},
		}},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "cattle-system", Name: "rancher-f7fb", OwnerReferences: ctrlRef("Deployment", "rancher")},
		}},
		Pods: []corev1.Pod{
			pod("cattle-system", "rancher-f7fb-64smq", ctrlRef("ReplicaSet", "rancher-f7fb"), 31, "rancher/rancher:v2.14.1"),
			pod("cattle-system", "rancher-f7fb-d2th5", ctrlRef("ReplicaSet", "rancher-f7fb"), 32, "rancher/rancher:v2.14.1"),
		},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 {
		t.Fatalf("expected 1 workload, got %d: %+v", len(ws), ws)
	}
	w := ws[0]
	if w.Kind != "Deployment" || w.Name != "rancher" {
		t.Errorf("kind/name = %s/%s, want Deployment/rancher", w.Kind, w.Name)
	}
	if w.Desired != 3 || w.Ready != 3 || w.Status != "Running" {
		t.Errorf("got %d/%d %s, want 3/3 Running", w.Ready, w.Desired, w.Status)
	}
	if w.Restarts != 63 {
		t.Errorf("restarts = %d, want 63", w.Restarts)
	}
	if len(w.Pods) != 2 {
		t.Errorf("expected 2 pod rows, got %d", len(w.Pods))
	}
	if w.Image != "rancher/rancher:v2.14.1" {
		t.Errorf("image = %q", w.Image)
	}
}

func TestAssemble_AttachesFindingsAndSortsFlaggedFirst(t *testing.T) {
	in := Inputs{
		Deployments: []appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "healthy"}, Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "broken"}, Status: appsv1.DeploymentStatus{Replicas: 2, ReadyReplicas: 2}},
		},
		ReplicaSets: []appsv1.ReplicaSet{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "healthy-rs", OwnerReferences: ctrlRef("Deployment", "healthy")}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "broken-rs", OwnerReferences: ctrlRef("Deployment", "broken")}},
		},
		Pods: []corev1.Pod{
			pod("a", "healthy-rs-1", ctrlRef("ReplicaSet", "healthy-rs"), 0, "img"),
			pod("a", "broken-rs-1", ctrlRef("ReplicaSet", "broken-rs"), 5, "img"),
		},
	}
	findings := []diagnose.Finding{{Pod: "a/broken-rs-1", Issue: "CrashLoopBackOff", Reason: "boom", Evidence: "x"}}
	ws := Assemble(in, findings)
	if len(ws) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(ws))
	}
	if ws[0].Name != "broken" || !ws[0].Flagged() {
		t.Errorf("flagged workload should sort first; got %+v", ws[0])
	}
	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "CrashLoopBackOff" {
		t.Errorf("finding not attached to broken: %+v", ws[0].Findings)
	}
	if ws[1].Name != "healthy" || ws[1].Flagged() {
		t.Errorf("healthy workload should sort last and be unflagged; got %+v", ws[1])
	}
}

func TestAssemble_BarePodBecomesItsOwnWorkload(t *testing.T) {
	in := Inputs{Pods: []corev1.Pod{
		readyPod("default", "lonely", nil, "img"), // no owner refs → bare pod
	}}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Pod" || ws[0].Name != "lonely" {
		t.Fatalf("expected a bare-pod workload, got %+v", ws)
	}
	if ws[0].Desired != 1 || ws[0].Ready != 1 || ws[0].Status != "Running" {
		t.Errorf("bare pod health = %d/%d %s, want 1/1 Running", ws[0].Ready, ws[0].Desired, ws[0].Status)
	}
}
```

Add these test helpers to the test file:

```go
// pod builds a one-container pod with the given restart count (recorded in the
// current container status) and image. It is NOT ready by default.
func pod(ns, name string, owners []metav1.OwnerReference, restarts int32, image string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		Status: corev1.PodStatus{
			Phase:            corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: restarts, Ready: false}},
		},
	}
}

// readyPod is like pod but its single container is Ready.
func readyPod(ns, name string, owners []metav1.OwnerReference, image string) corev1.Pod {
	p := pod(ns, name, owners, 0, image)
	p.Status.ContainerStatuses[0].Ready = true
	return p
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/inventory/ -run TestAssemble 2>&1 | tail -8
```
Expected: FAIL — undefined `Inputs` / `Assemble`.

- [ ] **Step 3: Add `Inputs` and `Assemble` to `internal/inventory/inventory.go`**

Add the `appsv1 "k8s.io/api/apps/v1"` import, then:

```go
// Inputs are the raw lists Assemble consumes (Phase B kinds only).
type Inputs struct {
	Pods         []corev1.Pod
	Deployments  []appsv1.Deployment
	ReplicaSets  []appsv1.ReplicaSet
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
}

// Assemble groups pods into workloads, reads controller status for ready/desired,
// aggregates restarts, attaches findings, and returns workloads sorted
// flagged-first then by namespace/name.
func Assemble(in Inputs, findings []diagnose.Finding) []Workload {
	key := func(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

	workloads := map[string]*Workload{}
	controllerKeys := map[string]bool{}
	seed := func(kind, ns, name string, desired, ready int) {
		k := key(kind, ns, name)
		workloads[k] = &Workload{Namespace: ns, Name: name, Kind: kind, Desired: desired, Ready: ready}
		controllerKeys[k] = true
	}
	for _, d := range in.Deployments {
		seed("Deployment", d.Namespace, d.Name, int(d.Status.Replicas), int(d.Status.ReadyReplicas))
	}
	for _, s := range in.StatefulSets {
		seed("StatefulSet", s.Namespace, s.Name, int(s.Status.Replicas), int(s.Status.ReadyReplicas))
	}
	for _, ds := range in.DaemonSets {
		seed("DaemonSet", ds.Namespace, ds.Name, int(ds.Status.DesiredNumberScheduled), int(ds.Status.NumberReady))
	}

	// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).
	rsToDeploy := map[string]string{}
	for _, rs := range in.ReplicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}

	podKey := map[string]string{}   // "ns/name" -> workload key
	derivedReady := map[string]int{} // ready-pod count for pod-derived workloads
	for _, p := range in.Pods {
		kind, name := "Pod", p.Name
		if o := controllerOwner(p.OwnerReferences); o != nil {
			if o.Kind == "ReplicaSet" {
				if dep, ok := rsToDeploy[p.Namespace+"/"+o.Name]; ok {
					kind, name = "Deployment", dep
				} else {
					kind, name = "ReplicaSet", o.Name
				}
			} else {
				kind, name = o.Kind, o.Name
			}
		}
		k := key(kind, p.Namespace, name)
		w, ok := workloads[k]
		if !ok {
			w = &Workload{Namespace: p.Namespace, Name: name, Kind: kind}
			workloads[k] = w
		}
		restarts, last := podRestarts(p)
		w.Restarts += restarts
		if lt := termTime(last); lt != "" && lt > w.LastRestart {
			w.LastRestart = lt
		}
		if w.Image == "" {
			w.Image = podImage(p)
		}
		if podIsReady(p) {
			derivedReady[k]++
		}
		w.Pods = append(w.Pods, PodRow{
			Name: p.Name, Phase: string(p.Status.Phase), Ready: podReady(p),
			Restarts: restarts, LastRestart: termTime(last),
			Node: p.Spec.NodeName, IP: p.Status.PodIP,
			Age: humanAge(p.CreationTimestamp.Time, time.Now()), Image: podImage(p),
		})
		podKey[p.Namespace+"/"+p.Name] = k
	}

	for _, f := range findings {
		if k, ok := podKey[f.Pod]; ok {
			workloads[k].Findings = append(workloads[k].Findings, f)
		}
	}

	out := make([]Workload, 0, len(workloads))
	for k, w := range workloads {
		if !controllerKeys[k] {
			w.Desired = len(w.Pods)
			w.Ready = derivedReady[k]
		}
		w.Status = workloadStatus(w.Ready, w.Desired)
		out = append(out, *w)
	}
	sortWorkloads(out)
	return out
}
```

Add the sort helper (and import `"sort"`):

```go
func sortWorkloads(ws []Workload) {
	sort.Slice(ws, func(i, j int) bool {
		if ws[i].Flagged() != ws[j].Flagged() {
			return ws[i].Flagged() // flagged first
		}
		if ws[i].Namespace != ws[j].Namespace {
			return ws[i].Namespace < ws[j].Namespace
		}
		return ws[i].Name < ws[j].Name
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/inventory/ -v 2>&1 | tail -30
go vet ./internal/inventory/
```
Expected: PASS — all helper + Assemble tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/
git commit -m "feat(inventory): Assemble groups pods into workloads with health" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `collect` — list controllers + facts helper

**Files:**
- Modify: `internal/collect/collect.go`
- Modify: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func CollectInventory(ctx context.Context, client kubernetes.Interface, namespace string) (inventory.Inputs, error)`; `func FactsFrom(pods []corev1.Pod) []diagnose.PodFacts`.
- Consumes: `inventory.Inputs` (Task 2), `diagnose.PodFacts`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/collect/collect_test.go` (add imports `appsv1 "k8s.io/api/apps/v1"`):

```go
func TestCollectInventory_ListsControllersAndPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "rs1"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ds1"}},
	)
	in, err := CollectInventory(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Pods) != 1 || len(in.Deployments) != 1 || len(in.ReplicaSets) != 1 ||
		len(in.StatefulSets) != 1 || len(in.DaemonSets) != 1 {
		t.Errorf("expected one of each kind, got %+v", in)
	}
}

func TestCollectInventory_ScopesToNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "d2"}},
	)
	in, err := CollectInventory(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Deployments) != 1 || in.Deployments[0].Namespace != "a" {
		t.Errorf("expected only namespace a, got %+v", in.Deployments)
	}
}

func TestFactsFrom_WrapsEachPod(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p2"}},
	}
	facts := FactsFrom(pods)
	if len(facts) != 2 || facts[0].Pod == nil || facts[0].Pod.Name != "p1" {
		t.Fatalf("expected 2 facts wrapping each pod, got %+v", facts)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run 'CollectInventory|FactsFrom' 2>&1 | tail -8
```
Expected: FAIL — undefined `CollectInventory` / `FactsFrom`.

- [ ] **Step 3: Implement in `internal/collect/collect.go`**

Add imports `appsv1 "k8s.io/api/apps/v1"` and `"github.com/imantaba/kubeagent/internal/inventory"`, then add:

```go
// CollectInventory lists pods and the Phase-B controller kinds (Deployments,
// ReplicaSets, StatefulSets, DaemonSets) in the given namespace (or all
// namespaces when empty). Read-only: List calls only.
func CollectInventory(ctx context.Context, client kubernetes.Interface, namespace string) (inventory.Inputs, error) {
	var in inventory.Inputs

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing pods: %w", err)
	}
	in.Pods = pods.Items

	deploys, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing deployments: %w", err)
	}
	in.Deployments = deploys.Items

	rs, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing replicasets: %w", err)
	}
	in.ReplicaSets = rs.Items

	sts, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing statefulsets: %w", err)
	}
	in.StatefulSets = sts.Items

	ds, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing daemonsets: %w", err)
	}
	in.DaemonSets = ds.Items

	return in, nil
}

// FactsFrom wraps each pod in a diagnose.PodFacts for the detectors.
func FactsFrom(pods []corev1.Pod) []diagnose.PodFacts {
	facts := make([]diagnose.PodFacts, 0, len(pods))
	for i := range pods {
		pod := pods[i] // copy so &pod is stable per iteration
		facts = append(facts, diagnose.PodFacts{Pod: &pod})
	}
	return facts
}
```

(`collect.go` already imports `context`, `fmt`, `metav1`, `corev1`, `kubernetes`, and `diagnose` for the existing `Cluster`. If any are missing, add them.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collect/ -v 2>&1 | tail -20
go vet ./internal/collect/
go build ./...
```
Expected: PASS — new + existing collect tests; vet clean; module builds (the new code is not yet wired into main — that's Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list controllers (CollectInventory) + FactsFrom" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: `report` — grouped workload output

**Files:**
- Modify: `internal/report/report.go`
- Modify: `internal/report/report_test.go`

**Interfaces:**
- Produces: `func PrintInventory(workloads []inventory.Workload, explanation, format string, w io.Writer) error`.
- text: cluster verdict is NOT here yet (Phase C) — print the grouped inventory. json: `{"workloads": [...]}` (+ `"explanation"` when non-empty).

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add import `"github.com/imantaba/kubeagent/internal/inventory"`):

```go
func sampleWorkloads() []inventory.Workload {
	return []inventory.Workload{{
		Namespace: "cattle-system", Name: "rancher", Kind: "Deployment",
		Desired: 3, Ready: 3, Status: "Running", Restarts: 64, LastRestart: "2026-06-02T08:14:03Z",
		Image: "rancher/rancher:v2.14.1",
		Pods: []inventory.PodRow{
			{Name: "rancher-64smq", Phase: "Running", Ready: "1/1", Restarts: 31, LastRestart: "2026-06-02T08:14:03Z", Node: "nova-worker-3", IP: "10.42.4.41", Age: "36d", Image: "rancher/rancher:v2.14.1"},
		},
	}}
}

func TestPrintInventory_TextShowsWorkloadAndPods(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleWorkloads(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"cattle-system/rancher", "Deployment", "3/3", "Running", "64", "rancher-64smq", "nova-worker-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextFlagsWorkloadWithFinding(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Desired: 2, Ready: 1, Status: "Degraded",
		Findings: []diagnose.Finding{{Pod: "kube-system/coredns-x", Issue: "CrashLoopBackOff", Reason: "boom", Evidence: "e"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(ws, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CrashLoopBackOff") || !strings.Contains(out, "Degraded") {
		t.Errorf("expected the finding + Degraded to show:\n%s", out)
	}
}

func TestPrintInventory_JSONObjectWithWorkloads(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleWorkloads(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Workloads   []inventory.Workload `json:"workloads"`
		Explanation string               `json:"explanation"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not the workloads object: %v", err)
	}
	if len(got.Workloads) != 1 || got.Workloads[0].Name != "rancher" || got.Explanation != "" {
		t.Errorf("workloads object mismatch: %+v", got)
	}
}

func TestPrintInventory_JSONIncludesExplanation(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(sampleWorkloads(), "rancher is fine", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"explanation": "rancher is fine"`) {
		t.Errorf("expected explanation field:\n%s", buf.String())
	}
}

func TestPrintInventory_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(nil, "", "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}
```

For the `inventory.Workload` JSON to round-trip, add JSON tags in Task 4 Step 3 (below).

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run PrintInventory 2>&1 | tail -8
```
Expected: FAIL — undefined `PrintInventory`.

- [ ] **Step 3: Add JSON tags to the inventory types, then implement `PrintInventory`**

First, in `internal/inventory/inventory.go`, add JSON tags so the JSON output is stable (modify the `Workload` and `PodRow` field tags):

```go
type PodRow struct {
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	Ready       string `json:"ready"`
	Restarts    int    `json:"restarts"`
	LastRestart string `json:"lastRestart,omitempty"`
	Node        string `json:"node"`
	IP          string `json:"ip"`
	Age         string `json:"age"`
	Image       string `json:"image"`
}

type Workload struct {
	Namespace   string             `json:"namespace"`
	Name        string             `json:"name"`
	Kind        string             `json:"kind"`
	Desired     int                `json:"desired"`
	Ready       int                `json:"ready"`
	Status      string             `json:"status"`
	Restarts    int                `json:"restarts"`
	LastRestart string             `json:"lastRestart,omitempty"`
	Image       string             `json:"image"`
	Pods        []PodRow           `json:"pods"`
	Findings    []diagnose.Finding `json:"findings,omitempty"`
}
```

Then add to `internal/report/report.go` (add import `"github.com/imantaba/kubeagent/internal/inventory"`):

```go
// inventoryReport is the JSON shape for the workload inventory.
type inventoryReport struct {
	Workloads   []inventory.Workload `json:"workloads"`
	Explanation string               `json:"explanation,omitempty"`
}

// PrintInventory writes the grouped workload inventory to w in the chosen
// format. explanation, when non-empty, is appended (text) or added (json).
func PrintInventory(workloads []inventory.Workload, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Workloads: workloads, Explanation: explanation})
	case "text":
		return printInventoryText(workloads, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printInventoryText(workloads []inventory.Workload, explanation string, w io.Writer) error {
	if len(workloads) == 0 {
		if _, err := fmt.Fprintln(w, "No workloads found."); err != nil {
			return err
		}
	}
	for _, wl := range workloads {
		flag := "  "
		if wl.Flagged() {
			flag = "⚠ "
		}
		header := fmt.Sprintf("%s%s/%s  %s  %d/%d %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Ready, wl.Desired, wl.Status)
		if wl.Restarts > 0 {
			header += fmt.Sprintf("  · %d restarts", wl.Restarts)
			if wl.LastRestart != "" {
				header += fmt.Sprintf(", last %s", wl.LastRestart)
			}
		}
		if _, err := fmt.Fprintln(w, header); err != nil {
			return err
		}
		if wl.Image != "" {
			if _, err := fmt.Fprintf(w, "    image %s\n", wl.Image); err != nil {
				return err
			}
		}
		for _, f := range wl.Findings {
			if _, err := fmt.Fprintf(w, "    ⚠ %s: %s\n", f.Issue, f.Reason); err != nil {
				return err
			}
		}
		for _, p := range wl.Pods {
			restarts := fmt.Sprintf("%d", p.Restarts)
			if p.LastRestart != "" {
				restarts += " (" + p.LastRestart + ")"
			}
			if _, err := fmt.Fprintf(w, "    %s  %s  %s  restarts=%s  %s  %s  %s\n",
				p.Name, p.Ready, p.Phase, restarts, p.Node, p.IP, p.Age); err != nil {
				return err
			}
		}
	}
	if explanation != "" {
		if _, err := fmt.Fprintf(w, "\n── Explanation ──\n%s\n", explanation); err != nil {
			return err
		}
	}
	return nil
}
```

(The old `Print` and `printText` remain for now; Task 6 deletes them.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/report/ -v 2>&1 | tail -25
go vet ./internal/report/
go build ./...
```
Expected: PASS — new `PrintInventory` tests + the existing `Print` tests; vet clean; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/report/ internal/inventory/
git commit -m "feat(report): grouped workload output (PrintInventory) + JSON tags" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: `explain` — notable filter + workload prompt

**Files:**
- Modify: `internal/explain/explain.go`
- Modify: `internal/explain/explain_test.go`

**Interfaces:**
- Produces: `func Notable(workloads []inventory.Workload) []inventory.Workload`; `func (c *Client) ExplainInventory(ctx context.Context, workloads []inventory.Workload) (string, error)`; unexported `buildInventoryPrompt`.
- Notable = workloads that are `Flagged()` (have a finding or `Ready<Desired`) **or** have `Restarts >= notableRestartThreshold` (5).

- [ ] **Step 1: Write the failing tests**

Add to `internal/explain/explain_test.go` (add import `"github.com/imantaba/kubeagent/internal/inventory"`):

```go
func TestNotable_SelectsFlaggedAndHighRestarts(t *testing.T) {
	ws := []inventory.Workload{
		{Name: "healthy", Ready: 3, Desired: 3, Restarts: 0},
		{Name: "degraded", Ready: 1, Desired: 2},
		{Name: "restarted", Ready: 3, Desired: 3, Restarts: 64},
		{Name: "withfinding", Ready: 1, Desired: 1, Findings: []diagnose.Finding{{Pod: "a/b", Issue: "OOMKilled"}}},
		{Name: "quiet", Ready: 1, Desired: 1, Restarts: 2},
	}
	got := Notable(ws)
	names := map[string]bool{}
	for _, w := range got {
		names[w.Name] = true
	}
	if names["healthy"] || names["quiet"] {
		t.Errorf("healthy/quiet should not be notable: %v", names)
	}
	if !names["degraded"] || !names["restarted"] || !names["withfinding"] {
		t.Errorf("expected degraded, restarted, withfinding; got %v", names)
	}
}

func TestExplainInventory_SkipsWhenNothingNotable(t *testing.T) {
	f := &fakeSummarizer{reply: "should not be used"}
	c := &Client{s: f}
	got, err := c.ExplainInventory(context.Background(), []inventory.Workload{{Name: "ok", Ready: 1, Desired: 1}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || f.called {
		t.Errorf("expected no call and empty result; got %q called=%v", got, f.called)
	}
}

func TestExplainInventory_SummarizesNotable(t *testing.T) {
	f := &fakeSummarizer{reply: "  coredns is degraded.  "}
	c := &Client{s: f}
	ws := []inventory.Workload{{Namespace: "kube-system", Name: "coredns", Kind: "Deployment", Ready: 1, Desired: 2}}
	got, err := c.ExplainInventory(context.Background(), ws)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "coredns is degraded." || !f.called {
		t.Errorf("got %q called=%v", got, f.called)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/explain/ -run 'Notable|ExplainInventory' 2>&1 | tail -8
```
Expected: FAIL — undefined `Notable` / `ExplainInventory`.

- [ ] **Step 3: Implement in `internal/explain/explain.go`**

Add import `"github.com/imantaba/kubeagent/internal/inventory"`, then:

```go
// notableRestartThreshold: a healthy workload with at least this many total
// restarts is still worth explaining.
const notableRestartThreshold = 5

// Notable selects the workloads worth sending to the model: those flagged
// (finding or not fully ready) or with a high restart count.
func Notable(workloads []inventory.Workload) []inventory.Workload {
	var out []inventory.Workload
	for _, w := range workloads {
		if w.Flagged() || w.Restarts >= notableRestartThreshold {
			out = append(out, w)
		}
	}
	return out
}

// ExplainInventory summarizes the notable workloads in plain English. With
// nothing notable it returns "" and makes no API call.
func (c *Client) ExplainInventory(ctx context.Context, workloads []inventory.Workload) (string, error) {
	notable := Notable(workloads)
	if len(notable) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(notable))
	if err != nil {
		return "", fmt.Errorf("explaining workloads: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining workloads: model returned no text")
	}
	return out, nil
}

// buildInventoryPrompt renders the notable workloads into a compact prompt.
// Only structured fields are sent — never raw pod specs or secrets.
func buildInventoryPrompt(workloads []inventory.Workload) string {
	var b strings.Builder
	b.WriteString("A read-only scan summarized these Kubernetes workloads needing attention:\n\n")
	for _, w := range workloads {
		fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
			w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
		for _, f := range w.Findings {
			fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
		}
	}
	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/explain/ -v 2>&1 | tail -25
go vet ./internal/explain/
go build ./...
```
Expected: PASS — new + existing explain tests; vet clean; module builds (old `Explain` still present, removed in Task 6).

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): notable-workload filter + ExplainInventory" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 6: `main` — rewire the pipeline + remove the old path

**Files:**
- Modify: `main.go`
- Modify: `main_test.go`
- Modify: `internal/report/report.go` (delete old `Print`/`printText`)
- Modify: `internal/report/report_test.go` (delete old `Print` tests)
- Modify: `internal/explain/explain.go` (delete old `Explain`/`buildPrompt`)
- Modify: `internal/explain/explain_test.go` (delete old `Explain` tests)
- Modify: `internal/collect/collect.go` (delete old `Cluster`)
- Modify: `internal/collect/collect_test.go` (delete old `Cluster` tests)
- Modify: `README.md`, `docs/design.md`

**Interfaces:**
- Consumes: `collect.CollectInventory`, `collect.FactsFrom`, `inventory.Assemble`, `report.PrintInventory`, `(*explain.Client).ExplainInventory`.

- [ ] **Step 1: Rewire `run` in `main.go`**

Add the `inventory` import (`"github.com/imantaba/kubeagent/internal/inventory"`). Replace the body from the `client, err := cluster.NewClient(...)` line through the final `return` with:

```go
	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	inputs, err := collect.CollectInventory(context.Background(), client, namespace)
	if err != nil {
		return err
	}

	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
	}
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods))
	workloads := inventory.Assemble(inputs, findings)

	var explanation string
	if *explainFlag {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, workloads)
		if err != nil {
			return err
		}
	}

	return report.PrintInventory(workloads, explanation, *output, os.Stdout)
```

- [ ] **Step 2: Build to find the now-dead code**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... 2>&1 | tail -5
```
Expected: builds (old funcs are now unused but still compile). Proceed to delete them.

- [ ] **Step 3: Delete the old findings-only path**

- In `internal/report/report.go`: delete `Print`, `printText`, and the `explainedReport` struct (keep `inventoryReport`, `PrintInventory`, `printInventoryText`).
- In `internal/report/report_test.go`: delete `sampleFindings`, `TestPrint_TextIncludesPodAndIssue`, `TestPrint_TextNoFindings`, `TestPrint_TextAppendsExplanation`, `TestPrint_TextNoExplanationBlockWhenEmpty`, `TestPrint_JSONBareArrayWhenNoExplanation`, `TestPrint_JSONWrapsWhenExplanation`, `TestPrint_UnknownFormatErrors` (the old one — keep `TestPrintInventory_UnknownFormatErrors`), `TestPrint_EmptyFormatErrors`. Keep all `TestPrintInventory_*`.
- In `internal/explain/explain.go`: delete `Explain` and `buildPrompt` (keep `ExplainInventory`, `Notable`, `buildInventoryPrompt`, `systemPrompt`, the `summarizer` seam, `New`, `ResolveModel`, `DefaultModel`, `anthropicSummarizer`).
- In `internal/explain/explain_test.go`: delete `TestBuildPrompt_IncludesEveryFindingField`, `TestExplain_SkipsCallWhenNoFindings`, `TestExplain_ReturnsTrimmedSummary`, `TestExplain_WrapsSummarizerError`, `TestExplain_ErrorsOnEmptySummary`. Keep `TestResolveModel`, `TestNotable_*`, `TestExplainInventory_*`, and the `fakeSummarizer` helper.
- In `internal/collect/collect.go`: delete `Cluster` (keep `CollectInventory`, `FactsFrom`).
- In `internal/collect/collect_test.go`: delete `TestCluster_ReturnsFactsForAllPods`, `TestCluster_EmptyClusterReturnsNoFacts`, `TestCluster_ScopesToNamespace`. Keep the `CollectInventory`/`FactsFrom` tests.

- [ ] **Step 4: Update `main_test.go`**

The existing `TestRun_*` tests still pass (they exercise arg validation before any cluster call). No change is required, but verify they still compile and pass in Step 6. (Do not add cluster-dependent tests — the pipeline is covered by the package unit tests.)

- [ ] **Step 5: Update docs**

- `README.md`: under Usage, after the `./kubeagent scan` line, note that `scan` now prints a grouped workload inventory (healthy and unhealthy), e.g. replace the first comment with:

```bash
# scan the whole cluster — prints every workload (Deployments, StatefulSets,
# DaemonSets, bare pods) with replica health, restart history, and any problems
./kubeagent scan
```

- `docs/design.md`: in the Roadmap, add a line under v2:

```markdown
- **v3 (in progress)** — `scan` becomes a complete workload health report
  (grouped inventory + integrated detectors); `--explain` summarizes notable
  workloads; model selectable via `--model`/`KUBEAGENT_MODEL`.
```

- [ ] **Step 6: Run the full suite + build + manual smoke**

```bash
go test ./... 2>&1
go vet ./... 2>&1
go build -o kubeagent .
ANTHROPIC_API_KEY= ./kubeagent scan --explain 2>&1 | head -2   # fail-fast still works
```
Expected: all packages PASS; vet clean; binary builds; the manual command prints the `--explain needs the ANTHROPIC_API_KEY environment variable` error.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "feat: scan prints the grouped workload inventory (v3 Phase B)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-review

- **Spec coverage (Phase B):** workload kinds Deployments/StatefulSets/DaemonSets/bare pods → Task 2/3 ✅; ready/desired from controller `.status` → Task 2 `seed` ✅; restart Σ + max `LastRestart` → Task 1 `podRestarts` + Task 2 aggregation ✅; per-pod rows (ready/restarts/last/node/ip/age/image) → Task 1/2 `PodRow` ✅; owner resolution incl. RS→Deployment → Task 2 ✅; integrated detector findings → Task 2 attach ✅; flagged-first sort → Task 2 `sortWorkloads` ✅; grouped text + JSON `{workloads}` → Task 4 ✅; notable-only `--explain` (flagged or ≥5 restarts) → Task 5 ✅; main rewire + old path removed → Task 6 ✅; per-pod timestamps (the folded-in v2.1 goal) → `PodRow.LastRestart` ✅.
- **Out of Phase B scope (correctly absent):** Nodes / cluster-health verdict (Phase C); Jobs/CronJobs (Phase D); the cluster JSON key (Phase C wraps `{cluster, workloads}`).
- **Placeholder scan:** none — every step has complete code/commands. (Task 1 Step 1 explicitly flags and replaces the `Findings: []struct{}{}` placeholder line with the real `[]diagnose.Finding{...}` version.)
- **Type consistency:** `inventory.Inputs` / `Workload` / `PodRow` used identically across Tasks 2–6; `Assemble(in, findings)`, `CollectInventory(ctx, client, ns) (inventory.Inputs, error)`, `FactsFrom([]corev1.Pod) []diagnose.PodFacts`, `PrintInventory(workloads, explanation, format, w)`, `Notable([]inventory.Workload) []inventory.Workload`, `ExplainInventory(ctx, workloads)` match their definitions and call sites.
- **Green-per-task:** Tasks 1–5 add code without breaking existing callers (old `Print`/`Explain`/`Cluster` stay until Task 6); Task 6 cuts over and deletes them in one step, ending fully green.
