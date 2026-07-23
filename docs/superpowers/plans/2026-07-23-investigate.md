# `--investigate` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `--investigate` flag that, after a scan, runs a bounded, read-only, model-driven tool-use loop over the findings and emits an evidence-grounded `Investigation` section.

**Architecture:** A new `internal/investigate/` package holds a 3-tool read allowlist, a findings-closure scope guard, a client-go reader that renders structured-only fields, and a bounded tool-use loop behind a small `conversation` interface (Anthropic implements it; tests fake it). `internal/explain` is reused (two identifiers exported) and otherwise untouched. `main.go` wires the flag; `internal/report` renders the section.

**Tech Stack:** Go 1.26, `github.com/anthropics/anthropic-sdk-go` v1.51.0 (tool-use / `Messages.New` with `Tools`), `k8s.io/client-go` (fake clientset in tests), stdlib `flag`.

## Global Constraints

- **READ-ONLY.** get/list only; no writes, ever. Opt-in: without `--investigate` nothing here runs and the deterministic scan is byte-identical.
- **Structured-only egress.** Tool results carry only the structured fields the scan already renders — never raw specs, env, secret data, container args, or logs. No logs at all in v1.
- **Bounded on every axis.** Fixed read allowlist (3 tools), findings-scoped reachable set, `maxToolCalls = 8`, `maxTurns = 6`, plus the caller's context deadline. No user-facing knobs in v1.
- **Anthropic-only in v1.** `--investigate` requires `ANTHROPIC_API_KEY`; error clearly if only `KUBEAGENT_EXPLAIN_ENDPOINT` is set.
- **No new dependency.** Uses the existing `anthropic-sdk-go` tool-use API and client-go.
- **No new RBAC.** All reads are `get`/`list` on resources the base ClusterRole already grants (pods, nodes, events, persistentvolumeclaims, and the workload kinds).
- **`explain` behavior unchanged.** Reuse `explain.BuildInventoryPrompt` and `explain.SystemPrompt`; the `--explain` output stays byte-identical and the golden snapshot is unchanged.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD** — failing test first. **gofmt-clean.** Run `go build ./... && go test ./...` before every commit.

## File Structure

- `internal/explain/explain.go` — **modify:** rename `systemPrompt` → `SystemPrompt`, `buildInventoryPrompt` → `BuildInventoryPrompt` (additive export; behavior identical).
- `internal/investigate/scope.go` — findings-closure guard (`Scope`, grows via `Add`).
- `internal/investigate/reader.go` — executes an allowed tool call via client-go; structured-only rendering.
- `internal/investigate/tools.go` — the 3 tool specs and their Anthropic conversion.
- `internal/investigate/investigate.go` — backend-agnostic loop types, the loop, the Anthropic `conversation` backend, `Client`, and the `Investigate` entrypoint.
- `internal/report/report.go` — **modify:** render the `Investigation` section (text + JSON).
- `main.go` — **modify:** `--investigate` flag, precondition, supersede-`--explain`, call + wire into the report.
- Docs: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: Export the explain reuse points

**Files:**
- Modify: `internal/explain/explain.go`
- Modify: `internal/explain/local.go` (references `systemPrompt`)
- Modify: `internal/explain/local_test.go` (references `systemPrompt`)
- Modify: `internal/explain/explain_test.go` (may reference `buildInventoryPrompt`)

**Interfaces:**
- Produces: `explain.SystemPrompt` (const string) and `explain.BuildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) string` — the investigate package consumes both.

This is a mechanical rename to export two existing identifiers with **no behavior change**. The `--explain` output must stay byte-identical.

- [ ] **Step 1: Find every reference to the two identifiers**

Run: `cd /home/ubuntu/git/kubeagent && grep -rn 'systemPrompt\|buildInventoryPrompt' internal/explain/`
Expected: references in `explain.go` (definition + use in `anthropicSummarizer.summarize`, `ExplainInventory`), `local.go` (uses `systemPrompt`), and the two test files.

- [ ] **Step 2: Rename the const and function to exported names**

In `internal/explain/explain.go`:
- Change `const systemPrompt = ` to `const SystemPrompt = ` (leave the string body exactly as-is).
- Change `func buildInventoryPrompt(` to `func BuildInventoryPrompt(` (leave the signature and body otherwise identical).
- Update the two in-file uses: in `ExplainInventory`, `buildInventoryPrompt(...)` → `BuildInventoryPrompt(...)`; in `anthropicSummarizer.summarize`, `System: []anthropic.TextBlockParam{{Text: systemPrompt}}` → `Text: SystemPrompt`.

In `internal/explain/local.go`: change the `systemPrompt` reference (in the system chat message) to `SystemPrompt`.

- [ ] **Step 3: Update the tests that reference the old names**

In `internal/explain/local_test.go`, the assertion `req.Messages[0].Content != systemPrompt` → `!= SystemPrompt` (and the comment). In `internal/explain/explain_test.go`, rename any `buildInventoryPrompt(` / `systemPrompt` references to the exported names.

- [ ] **Step 4: Build and run the explain + report tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/explain/ ./internal/report/`
Expected: PASS — behavior unchanged, and the golden snapshot in `internal/report` is untouched (proves `--explain` output is byte-identical).

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/explain/
git commit -m "refactor(explain): export SystemPrompt and BuildInventoryPrompt for reuse"
```

---

### Task 2: Findings-closure scope guard

**Files:**
- Create: `internal/investigate/scope.go`
- Test: `internal/investigate/scope_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (fields: `Kind`, `Namespace`, `Name`, `Pods []inventory.PodRow`; `PodRow` fields `Name`, `Node`).
- Produces:
  - `type Scope struct{ ... }`
  - `func NewScope(workloads []inventory.Workload) *Scope`
  - `func (s *Scope) Allowed(kind, namespace, name string) bool`
  - `func (s *Scope) HasName(namespace, name string) bool`
  - `func (s *Scope) Add(kind, namespace, name string)`
  - `func normKind(k string) string`

The scope is the reachable set: each flagged workload, each of its pods, and each pod's node. It **grows** via `Add` when the reader resolves a relation (owner/PVC/node) — modelling one-hop traversal of the finding's resource graph. `normKind` lowercases the Kubernetes kind so `"Deployment"` and `"deployment"` compare equal; node and PVC keys use namespace `""` for nodes.

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestScope_SeedsWorkloadPodsAndNodes(t *testing.T) {
	s := NewScope([]inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods: []inventory.PodRow{{Name: "web-abc", Node: "node-1"}},
	}})
	if !s.Allowed("deployment", "shop", "web") {
		t.Error("workload should be in scope")
	}
	if !s.Allowed("Pod", "shop", "web-abc") {
		t.Error("pod should be in scope (kind case-insensitive)")
	}
	if !s.Allowed("node", "", "node-1") {
		t.Error("pod's node should be in scope")
	}
	if s.Allowed("deployment", "other", "web") {
		t.Error("unrelated namespace must be denied")
	}
	if !s.HasName("shop", "web-abc") {
		t.Error("HasName should match an in-scope object regardless of kind")
	}
	if s.HasName("other", "web-abc") {
		t.Error("HasName must deny an out-of-scope namespace")
	}
}

func TestScope_GrowsViaAdd(t *testing.T) {
	s := NewScope(nil)
	if s.Allowed("pvc", "shop", "data") {
		t.Fatal("pvc not in scope before Add")
	}
	s.Add("PersistentVolumeClaim", "shop", "data")
	if !s.Allowed("pvc", "shop", "data") {
		t.Error("pvc should be reachable after Add (kind normalized)")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: FAIL — `undefined: NewScope`.

- [ ] **Step 3: Implement `scope.go`**

```go
// Package investigate runs a bounded, read-only, model-driven tool-use loop over
// the scan findings to gather evidence before explaining. It is opt-in and never
// writes: get/list only.
package investigate

import (
	"strings"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// key identifies a cluster object. Kind is the lowercased singular ("pod",
// "deployment", "node", "pvc"); cluster-scoped kinds (node) use namespace "".
type key struct {
	kind, namespace, name string
}

// Scope is the findings-closure guard: the set of objects the investigation may
// read. It is seeded from the scan findings and grows one hop at a time as the
// reader resolves relations (owner/PVC/node), modelling traversal of a finding's
// resource graph. Nothing outside this set is ever read.
type Scope struct {
	allowed map[key]bool
}

// NewScope seeds the reachable set from the flagged workloads: each workload, its
// pods, and each pod's node.
func NewScope(workloads []inventory.Workload) *Scope {
	s := &Scope{allowed: map[key]bool{}}
	for _, w := range workloads {
		s.Add(w.Kind, w.Namespace, w.Name)
		for _, p := range w.Pods {
			s.Add("pod", w.Namespace, p.Name)
			if p.Node != "" {
				s.Add("node", "", p.Node)
			}
		}
	}
	return s
}

// Allowed reports whether the given object is in the reachable set.
func (s *Scope) Allowed(kind, namespace, name string) bool {
	return s.allowed[key{normKind(kind), namespace, name}]
}

// HasName reports whether any in-scope object matches namespace+name, ignoring
// kind. Used to authorize events for an in-scope object without knowing its kind.
func (s *Scope) HasName(namespace, name string) bool {
	for k := range s.allowed {
		if k.namespace == namespace && k.name == name {
			return true
		}
	}
	return false
}

// Add extends the reachable set by one object (called when a relation resolves).
func (s *Scope) Add(kind, namespace, name string) {
	s.allowed[key{normKind(kind), namespace, name}] = true
}

// normKind lowercases a Kubernetes kind and maps the long PVC name to "pvc" so
// callers can use either form.
func normKind(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "persistentvolumeclaim" {
		return "pvc"
	}
	return k
}
```

- [ ] **Step 4: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/investigate/scope.go internal/investigate/scope_test.go
git commit -m "feat(investigate): findings-closure scope guard"
```

---

### Task 3: Read-only reader

**Files:**
- Create: `internal/investigate/reader.go`
- Test: `internal/investigate/reader_test.go`

**Interfaces:**
- Consumes: `Scope` (Task 2), `toolCall` (defined here, also used by Task 4).
- Produces:
  - `type toolCall struct { ID, Name string; Input json.RawMessage }`
  - `type toolResult struct { ID, Content string; IsError bool }`
  - `type Reader struct { client kubernetes.Interface }`
  - `func (r Reader) execute(ctx context.Context, call toolCall, scope *Scope) toolResult`

The reader parses a tool call, checks the scope, performs a client-go `Get`/`List`, and renders **structured fields only** — never IPs, env, secret data, args, or logs. Out-of-scope or malformed calls return a `toolResult` with `IsError: true` (the loop feeds this back so the model adapts). `get_related` resolves the target, calls `scope.Add`, and returns the target's key facts.

Tool input shapes (JSON): `describe` `{kind, namespace, name}`; `get_events` `{namespace, name}`; `get_related` `{namespace, name, relation}` where `relation ∈ {owner, pvc, node}` and the source is the named **pod**.

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func call(name string, input map[string]string) toolCall {
	b, _ := json.Marshal(input)
	return toolCall{ID: "t1", Name: name, Input: b}
}

func TestReader_DescribePod_StructuredNoSecrets(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.1.2.3",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "web", Ready: false, RestartCount: 5,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting",
				}},
			}},
		},
	}
	r := Reader{client: fake.NewSimpleClientset(pod)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")

	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "shop", "name": "web-abc",
	}), s)

	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "CrashLoopBackOff") || !strings.Contains(res.Content, "restarts=5") {
		t.Errorf("missing structured status: %q", res.Content)
	}
	if strings.Contains(res.Content, "10.1.2.3") {
		t.Errorf("pod IP must not leak into tool output: %q", res.Content)
	}
}

func TestReader_OutOfScope_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "pod", "namespace": "other", "name": "x",
	}), NewScope(nil))
	if !res.IsError || !strings.Contains(res.Content, "not in scope") {
		t.Errorf("out-of-scope call must return an error result, got %+v", res)
	}
}

func TestReader_UnknownKind_IsError(t *testing.T) {
	r := Reader{client: fake.NewSimpleClientset()}
	s := NewScope(nil)
	s.Add("secret", "shop", "creds")
	res := r.execute(context.Background(), call("describe", map[string]string{
		"kind": "secret", "namespace": "shop", "name": "creds",
	}), s)
	if !res.IsError {
		t.Errorf("unknown/unsupported kind must return an error result, got %+v", res)
	}
}

func TestReader_GetEvents_ForInScopeObject(t *testing.T) {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "web-abc.1", Namespace: "shop"},
		InvolvedObject: corev1.ObjectReference{Name: "web-abc", Namespace: "shop"},
		Reason:         "BackOff", Message: "Back-off pulling image", Count: 3,
	}
	r := Reader{client: fake.NewSimpleClientset(ev)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_events", map[string]string{
		"namespace": "shop", "name": "web-abc",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "BackOff") {
		t.Errorf("expected events, got %+v", res)
	}
}

func TestReader_GetRelated_OwnerAddsToScope(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-abc", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5f"}},
		},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-5f", Namespace: "shop"}}
	r := Reader{client: fake.NewSimpleClientset(pod, rs)}
	s := NewScope(nil)
	s.Add("pod", "shop", "web-abc")
	res := r.execute(context.Background(), call("get_related", map[string]string{
		"namespace": "shop", "name": "web-abc", "relation": "owner",
	}), s)
	if res.IsError || !strings.Contains(res.Content, "web-5f") {
		t.Fatalf("expected owner, got %+v", res)
	}
	if !s.Allowed("replicaset", "shop", "web-5f") {
		t.Error("resolved owner must be added to scope")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: FAIL — `undefined: Reader` / `undefined: toolCall`.

- [ ] **Step 3: Implement `reader.go`**

```go
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// toolCall is one model-requested read (backend-agnostic; the Anthropic backend
// translates tool_use blocks into these).
type toolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// toolResult answers a toolCall. IsError marks a denied or failed read; the loop
// feeds it back so the model can adapt.
type toolResult struct {
	ID      string
	Content string
	IsError bool
}

// Reader executes an allowed tool call via read-only client-go calls, rendering
// only structured fields — never IPs, env, secret data, container args, or logs.
type Reader struct {
	client kubernetes.Interface
}

func (r Reader) execute(ctx context.Context, c toolCall, scope *Scope) toolResult {
	switch c.Name {
	case "describe":
		return r.describe(ctx, c, scope)
	case "get_events":
		return r.getEvents(ctx, c, scope)
	case "get_related":
		return r.getRelated(ctx, c, scope)
	default:
		return errResult(c.ID, fmt.Sprintf("unknown tool %q", c.Name))
	}
}

func errResult(id, msg string) toolResult { return toolResult{ID: id, Content: msg, IsError: true} }

func okResult(id, content string) toolResult { return toolResult{ID: id, Content: content} }

type describeInput struct{ Kind, Namespace, Name string }

func (r Reader) describe(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in describeInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	kind := normKind(in.Kind)
	if !scope.Allowed(kind, nsFor(kind, in.Namespace), in.Name) {
		return errResult(c.ID, fmt.Sprintf("%s %s/%s is not in scope for this investigation", kind, in.Namespace, in.Name))
	}
	switch kind {
	case "pod":
		p, err := r.client.CoreV1().Pods(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describePod(p))
	case "deployment", "replicaset", "statefulset", "daemonset", "job":
		return r.describeWorkload(ctx, c.ID, kind, in.Namespace, in.Name)
	case "node":
		n, err := r.client.CoreV1().Nodes().Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describeNode(n))
	case "pvc":
		pvc, err := r.client.CoreV1().PersistentVolumeClaims(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
		if err != nil {
			return errResult(c.ID, err.Error())
		}
		return okResult(c.ID, describePVC(pvc))
	default:
		return errResult(c.ID, fmt.Sprintf("kind %q is not supported for describe", in.Kind))
	}
}

// nsFor returns "" for cluster-scoped kinds so scope lookups match the seeded keys.
func nsFor(kind, ns string) string {
	if kind == "node" {
		return ""
	}
	return ns
}

func describePod(p *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pod %s/%s: phase=%s node=%s\n", p.Namespace, p.Name, p.Status.Phase, p.Spec.NodeName)
	for _, cond := range p.Status.Conditions {
		fmt.Fprintf(&b, "  condition %s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			fmt.Fprintf(&b, " (%s)", cond.Reason)
		}
		b.WriteString("\n")
	}
	for _, cs := range p.Status.ContainerStatuses {
		fmt.Fprintf(&b, "  container %s: ready=%t restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
		if cs.State.Waiting != nil {
			fmt.Fprintf(&b, " waiting=%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		}
		if cs.State.Terminated != nil {
			fmt.Fprintf(&b, " terminated=%s (exit %d)", cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (r Reader) describeWorkload(ctx context.Context, id, kind, ns, name string) toolResult {
	var b strings.Builder
	switch kind {
	case "deployment":
		d, err := r.client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "deployment %s/%s: ready=%d/%d updated=%d available=%d\n",
			ns, name, d.Status.ReadyReplicas, d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
		for _, cnd := range d.Status.Conditions {
			fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cnd.Type, cnd.Status, cnd.Reason, cnd.Message)
		}
	case "replicaset":
		rs, err := r.client.AppsV1().ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "replicaset %s/%s: ready=%d/%d available=%d\n", ns, name,
			rs.Status.ReadyReplicas, rs.Status.Replicas, rs.Status.AvailableReplicas)
	case "statefulset":
		ss, err := r.client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "statefulset %s/%s: ready=%d/%d\n", ns, name, ss.Status.ReadyReplicas, ss.Status.Replicas)
	case "daemonset":
		ds, err := r.client.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "daemonset %s/%s: ready=%d desired=%d available=%d unavailable=%d\n", ns, name,
			ds.Status.NumberReady, ds.Status.DesiredNumberScheduled, ds.Status.NumberAvailable, ds.Status.NumberUnavailable)
	case "job":
		j, err := r.client.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return errResult(id, err.Error())
		}
		fmt.Fprintf(&b, "job %s/%s: active=%d succeeded=%d failed=%d\n", ns, name, j.Status.Active, j.Status.Succeeded, j.Status.Failed)
	}
	return okResult(id, b.String())
}

func describeNode(n *corev1.Node) string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %s: unschedulable=%t\n", n.Name, n.Spec.Unschedulable)
	for _, cond := range n.Status.Conditions {
		fmt.Fprintf(&b, "  condition %s=%s (%s): %s\n", cond.Type, cond.Status, cond.Reason, cond.Message)
	}
	for _, t := range n.Spec.Taints {
		fmt.Fprintf(&b, "  taint %s=%s:%s\n", t.Key, t.Value, t.Effect)
	}
	return b.String()
}

func describePVC(p *corev1.PersistentVolumeClaim) string {
	sc := ""
	if p.Spec.StorageClassName != nil {
		sc = *p.Spec.StorageClassName
	}
	return fmt.Sprintf("pvc %s/%s: phase=%s storageClass=%s volume=%s\n",
		p.Namespace, p.Name, p.Status.Phase, sc, p.Spec.VolumeName)
}

type eventsInput struct{ Namespace, Name string }

func (r Reader) getEvents(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in eventsInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	if !scope.HasName(in.Namespace, in.Name) {
		return errResult(c.ID, fmt.Sprintf("%s/%s is not in scope for this investigation", in.Namespace, in.Name))
	}
	evs, err := r.client.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + in.Name,
	})
	if err != nil {
		return errResult(c.ID, err.Error())
	}
	if len(evs.Items) == 0 {
		return okResult(c.ID, fmt.Sprintf("no events for %s/%s", in.Namespace, in.Name))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "events for %s/%s:\n", in.Namespace, in.Name)
	for _, e := range evs.Items {
		fmt.Fprintf(&b, "  %s: %s (x%d)\n", e.Reason, e.Message, e.Count)
	}
	return okResult(c.ID, b.String())
}

type relatedInput struct{ Namespace, Name, Relation string }

func (r Reader) getRelated(ctx context.Context, c toolCall, scope *Scope) toolResult {
	var in relatedInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return errResult(c.ID, "invalid input: "+err.Error())
	}
	// The source is always the named pod, which must already be in scope.
	if !scope.Allowed("pod", in.Namespace, in.Name) {
		return errResult(c.ID, fmt.Sprintf("pod %s/%s is not in scope for this investigation", in.Namespace, in.Name))
	}
	p, err := r.client.CoreV1().Pods(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return errResult(c.ID, err.Error())
	}
	switch in.Relation {
	case "owner":
		if len(p.OwnerReferences) == 0 {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s has no owner", in.Namespace, in.Name))
		}
		var b strings.Builder
		for _, o := range p.OwnerReferences {
			scope.Add(o.Kind, in.Namespace, o.Name)
			fmt.Fprintf(&b, "owner of %s: %s %s\n", in.Name, o.Kind, o.Name)
		}
		return okResult(c.ID, b.String())
	case "node":
		if p.Spec.NodeName == "" {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s is not scheduled to a node", in.Namespace, in.Name))
		}
		scope.Add("node", "", p.Spec.NodeName)
		return okResult(c.ID, fmt.Sprintf("node of %s: %s\n", in.Name, p.Spec.NodeName))
	case "pvc":
		var names []string
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				n := v.PersistentVolumeClaim.ClaimName
				scope.Add("pvc", in.Namespace, n)
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			return okResult(c.ID, fmt.Sprintf("pod %s/%s has no PersistentVolumeClaims", in.Namespace, in.Name))
		}
		return okResult(c.ID, fmt.Sprintf("PVCs of %s: %s\n", in.Name, strings.Join(names, ", ")))
	default:
		return errResult(c.ID, fmt.Sprintf("unknown relation %q (want owner|node|pvc)", in.Relation))
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/investigate/reader.go internal/investigate/reader_test.go
git commit -m "feat(investigate): read-only reader (describe/events/related, structured-only)"
```

---

### Task 4: Loop types, tool specs, and the bounded loop

**Files:**
- Create: `internal/investigate/tools.go`
- Modify: `internal/investigate/investigate.go` (create in this task with the loop; the Anthropic backend + entrypoint come in Task 5)
- Test: `internal/investigate/loop_test.go`

**Interfaces:**
- Consumes: `Scope`, `Reader`, `toolCall`, `toolResult`.
- Produces:
  - `type reply struct { Text string; Calls []toolCall; Done bool }`
  - `type conversation interface { start(ctx) (reply, error); next(ctx, results []toolResult) (reply, error); conclude(ctx, results []toolResult) (reply, error) }`
  - `type executor interface { execute(ctx context.Context, c toolCall, scope *Scope) toolResult }` (satisfied by `Reader`)
  - `type toolSpec struct { Name, Description string; Properties any; Required []string }`
  - `func toolSpecs() []toolSpec`
  - `func runLoop(ctx context.Context, conv conversation, exec executor, scope *Scope) (narrative string, trail []string, err error)`
  - `const maxToolCalls = 8`, `const maxTurns = 6`

The loop drives the conversation: on each `tool_use` reply it executes the calls (up to the remaining budget) and feeds results back via `next`; when a cap or turn limit is hit it uses `conclude` (results + a "conclude now" instruction, no tools) and returns that text; when the model finishes (`Done`) it returns the accumulated text. `trail` records each executed call as a human label.

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeConv scripts a fixed sequence of replies; the i-th call to start/next/
// concludes returns replies[i]. It records the tool results it was given.
type fakeConv struct {
	replies   []reply
	i         int
	gotResult int
	concluded bool
	startErr  error
}

func (f *fakeConv) pop() reply {
	r := f.replies[f.i]
	f.i++
	return r
}
func (f *fakeConv) start(ctx context.Context) (reply, error) {
	if f.startErr != nil {
		return reply{}, f.startErr
	}
	return f.pop(), nil
}
func (f *fakeConv) next(ctx context.Context, res []toolResult) (reply, error) {
	f.gotResult += len(res)
	return f.pop(), nil
}
func (f *fakeConv) conclude(ctx context.Context, res []toolResult) (reply, error) {
	f.concluded = true
	f.gotResult += len(res)
	return f.pop(), nil
}

// countingExec records how many calls it executed.
type countingExec struct{ n int }

func (e *countingExec) execute(ctx context.Context, c toolCall, s *Scope) toolResult {
	e.n++
	return okResult(c.ID, "observed")
}

func mkCall(name string, in map[string]string) toolCall {
	b, _ := json.Marshal(in)
	return toolCall{ID: name, Name: name, Input: b}
}

func TestRunLoop_GathersThenConcludes(t *testing.T) {
	conv := &fakeConv{replies: []reply{
		{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})}},
		{Calls: []toolCall{mkCall("get_events", map[string]string{"namespace": "shop", "name": "web-abc"})}},
		{Text: "root cause: bad image", Done: true},
	}}
	exec := &countingExec{}
	narrative, trail, err := runLoop(context.Background(), conv, exec, NewScope(nil))
	if err != nil {
		t.Fatal(err)
	}
	if narrative != "root cause: bad image" {
		t.Errorf("narrative = %q", narrative)
	}
	if exec.n != 2 || len(trail) != 2 {
		t.Errorf("expected 2 executed calls and 2 trail entries, got %d/%d", exec.n, len(trail))
	}
	if !strings.Contains(trail[0], "describe") {
		t.Errorf("trail[0] = %q", trail[0])
	}
}

func TestRunLoop_CapsToolCallsAndConcludes(t *testing.T) {
	// Every reply asks for one more tool call, forever.
	var reps []reply
	for i := 0; i < maxToolCalls+5; i++ {
		reps = append(reps, reply{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "p"})}})
	}
	reps = append(reps, reply{Text: "final under cap", Done: true})
	conv := &fakeConv{replies: reps}
	exec := &countingExec{}
	s := NewScope(nil)
	s.Add("pod", "shop", "p")
	narrative, _, err := runLoop(context.Background(), conv, exec, s)
	if err != nil {
		t.Fatal(err)
	}
	if exec.n > maxToolCalls {
		t.Errorf("executed %d calls, must not exceed maxToolCalls=%d", exec.n, maxToolCalls)
	}
	if !conv.concluded {
		t.Error("loop must call conclude when a cap is hit")
	}
	if narrative != "final under cap" {
		t.Errorf("narrative = %q", narrative)
	}
}

func TestRunLoop_StartErrorPropagates(t *testing.T) {
	conv := &fakeConv{startErr: errors.New("api down")}
	_, _, err := runLoop(context.Background(), conv, &countingExec{}, NewScope(nil))
	if err == nil {
		t.Error("expected the start error to propagate")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: FAIL — `undefined: runLoop` / `undefined: reply`.

- [ ] **Step 3: Implement `tools.go`**

```go
package investigate

// toolSpec is one read-only tool offered to the model (backend-agnostic; the
// Anthropic backend converts these to tool params). Properties is a JSON-schema
// "properties" object.
type toolSpec struct {
	Name        string
	Description string
	Properties  any
	Required    []string
}

// prop is a single JSON-schema string property with a description.
func prop(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// toolSpecs returns the fixed read-only allowlist. Nothing else is offered.
func toolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name:        "describe",
			Description: "Read structured status of one in-scope object (pod, deployment, replicaset, statefulset, daemonset, job, node, or pvc). Returns phase/conditions/container states — never logs, IPs, env, or secrets.",
			Properties: map[string]any{
				"kind":      prop("one of: pod, deployment, replicaset, statefulset, daemonset, job, node, pvc"),
				"namespace": prop("the object's namespace (empty for a node)"),
				"name":      prop("the object's name"),
			},
			Required: []string{"kind", "name"},
		},
		{
			Name:        "get_events",
			Description: "List recent events for one in-scope object by name.",
			Properties: map[string]any{
				"namespace": prop("the object's namespace"),
				"name":      prop("the object's name"),
			},
			Required: []string{"namespace", "name"},
		},
		{
			Name:        "get_related",
			Description: "From an in-scope pod, resolve a related object and bring it into scope: its owner (ReplicaSet/Deployment/Job), its node, or its PersistentVolumeClaims.",
			Properties: map[string]any{
				"namespace": prop("the pod's namespace"),
				"name":      prop("the pod's name"),
				"relation":  prop("one of: owner, node, pvc"),
			},
			Required: []string{"namespace", "name", "relation"},
		},
	}
}
```

- [ ] **Step 4: Implement the loop in `investigate.go`**

```go
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Loop bounds (Global Constraints): no user-facing knobs in v1.
const (
	maxToolCalls = 8
	maxTurns     = 6
)

// reply is one model turn: text and/or tool-use requests. Done is true when the
// model finished its turn without requesting tools.
type reply struct {
	Text  string
	Calls []toolCall
	Done  bool
}

// conversation is a live tool-use session. Only the Anthropic backend implements
// it; tests use a fake. start opens the session; next feeds tool results and
// continues; conclude feeds the final results plus a "stop and answer" instruction
// with no tools offered, forcing a text answer.
type conversation interface {
	start(ctx context.Context) (reply, error)
	next(ctx context.Context, results []toolResult) (reply, error)
	conclude(ctx context.Context, results []toolResult) (reply, error)
}

// executor runs one tool call against the scope (satisfied by Reader).
type executor interface {
	execute(ctx context.Context, c toolCall, scope *Scope) toolResult
}

// runLoop drives the conversation until the model concludes or a bound is hit,
// returning the final narrative and the evidence trail of executed calls.
func runLoop(ctx context.Context, conv conversation, exec executor, scope *Scope) (string, []string, error) {
	rep, err := conv.start(ctx)
	if err != nil {
		return "", nil, err
	}
	var trail []string
	calls := 0
	for turn := 1; ; turn++ {
		if rep.Done || len(rep.Calls) == 0 {
			return strings.TrimSpace(rep.Text), trail, nil
		}
		var results []toolResult
		for _, c := range rep.Calls {
			if calls >= maxToolCalls {
				break
			}
			calls++
			trail = append(trail, label(c))
			results = append(results, exec.execute(ctx, c, scope))
		}
		if calls >= maxToolCalls || turn >= maxTurns {
			rep, err = conv.conclude(ctx, results)
			if err != nil {
				return "", nil, err
			}
			return strings.TrimSpace(rep.Text), trail, nil
		}
		rep, err = conv.next(ctx, results)
		if err != nil {
			return "", nil, err
		}
	}
}

// label renders a tool call for the evidence trail, e.g. "describe pod shop/web-abc".
func label(c toolCall) string {
	var m map[string]string
	_ = json.Unmarshal(c.Input, &m)
	switch c.Name {
	case "describe":
		return fmt.Sprintf("describe %s %s/%s", m["kind"], m["namespace"], m["name"])
	case "get_events":
		return fmt.Sprintf("events %s/%s", m["namespace"], m["name"])
	case "get_related":
		return fmt.Sprintf("related %s/%s→%s", m["namespace"], m["name"], m["relation"])
	default:
		return c.Name
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/investigate/tools.go internal/investigate/investigate.go internal/investigate/loop_test.go
git commit -m "feat(investigate): tool specs and the bounded tool-use loop"
```

---

### Task 5: Anthropic backend + `Investigate` entrypoint

**Files:**
- Modify: `internal/investigate/investigate.go` (append the Anthropic backend, `Client`, `Report`, `Investigate`)
- Test: `internal/investigate/investigate_test.go`

**Interfaces:**
- Consumes: `explain.SystemPrompt`, `explain.BuildInventoryPrompt` (Task 1); `conversation`, `toolSpec`, `runLoop`, `Scope`, `Reader` (Tasks 2–4); `clusterhealth.ClusterHealth`, `resources.Summary`, `platform.Facts`, `svchealth.Issue`, `inventory.Workload`, `kubernetes.Interface`.
- Produces:
  - `type Report struct { Consulted []string; Narrative string }`
  - `type Client struct { newConversation func(system, firstUser string, specs []toolSpec) conversation }`
  - `func New(model string) *Client`
  - `func (c *Client) Investigate(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload, client kubernetes.Interface) (Report, error)`

`Client.newConversation` is a factory field so tests inject a fake conversation without touching the network (mirrors `explain.Client{s summarizer}`). `New` wires the real Anthropic-backed factory. `Investigate` skips (returns a zero `Report`) when the cluster is healthy and there are no workloads or service issues — mirroring `explain.ExplainInventory`.

- [ ] **Step 1: Write the failing test**

```go
package investigate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func TestInvestigate_RunsLoopAndReturnsReport(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: "shop"}}
	client := fake.NewSimpleClientset(pod)

	// Inject a fake conversation: one describe call, then a conclusion.
	c := &Client{newConversation: func(system, firstUser string, specs []toolSpec) conversation {
		if !strings.Contains(system, "read-only tools") {
			t.Error("system prompt should carry the investigation instruction")
		}
		if len(specs) != 3 {
			t.Errorf("expected 3 tool specs, got %d", len(specs))
		}
		return &fakeConv{replies: []reply{
			{Calls: []toolCall{mkCall("describe", map[string]string{"kind": "pod", "namespace": "shop", "name": "web-abc"})}},
			{Text: "root cause: image pull", Done: true},
		}}
	}}

	wl := []inventory.Workload{{
		Kind: "Deployment", Namespace: "shop", Name: "web",
		Pods:     []inventory.PodRow{{Name: "web-abc"}},
		Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "ImagePullBackOff", Reason: "bad tag", Evidence: "ErrImagePull"}},
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, wl, client)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Narrative != "root cause: image pull" {
		t.Errorf("narrative = %q", rep.Narrative)
	}
	if len(rep.Consulted) != 1 || !strings.Contains(rep.Consulted[0], "describe pod shop/web-abc") {
		t.Errorf("consulted = %v", rep.Consulted)
	}
}

func TestInvestigate_SkipsWhenNothingToDo(t *testing.T) {
	called := false
	c := &Client{newConversation: func(string, string, []toolSpec) conversation {
		called = true
		return &fakeConv{}
	}}
	rep, err := c.Investigate(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, nil, nil, fake.NewSimpleClientset())
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("must not open a conversation when there is nothing to investigate")
	}
	if rep.Narrative != "" || len(rep.Consulted) != 0 {
		t.Errorf("expected an empty report, got %+v", rep)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/investigate/`
Expected: FAIL — `undefined: Client` / `undefined: New`.

- [ ] **Step 3: Append the backend + entrypoint to `investigate.go`**

Add these imports to the existing `import` block in `investigate.go`:

```go
	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/platform"
	"github.com/imantaba/kubeagent/internal/resources"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"k8s.io/client-go/kubernetes"
```

Then append:

```go
// investigateSuffix extends the shared explain system prompt with the tool-use
// instruction. The Fix-first structure lives in explain.SystemPrompt (one source).
const investigateSuffix = `

You may call the provided read-only tools to gather more evidence about a finding
before you conclude — describe an object, list its events, or resolve a related
object (owner, node, PVC). Investigate only what the findings point to. Use only
the facts you observe. When you have enough, stop calling tools and give the
explanation in the required structure.`

// Report is the investigation result for the report layer.
type Report struct {
	Consulted []string // evidence trail: the reads that were made
	Narrative string   // the Fix-first explanation, grounded in the evidence
}

// Client runs a bounded, read-only investigation via a tool-use loop.
type Client struct {
	// newConversation builds the model session; a field so tests inject a fake.
	newConversation func(system, firstUser string, specs []toolSpec) conversation
}

// New returns a Client backed by the Anthropic API (the SDK reads ANTHROPIC_API_KEY).
func New(model string) *Client {
	return &Client{
		newConversation: func(system, firstUser string, specs []toolSpec) conversation {
			return &anthropicConversation{
				client: anthropic.NewClient(),
				model:  model,
				system: system,
				tools:  toAnthropicTools(specs),
				msgs:   []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(firstUser))},
			}
		},
	}
}

// Investigate runs the scan-grounded tool-use loop and returns its report. It
// skips (empty report) when the cluster is healthy with no workload or service
// findings — there is nothing to investigate.
func (c *Client) Investigate(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload, client kubernetes.Interface) (Report, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 && len(serviceIssues) == 0 {
		return Report{}, nil
	}
	system := explain.SystemPrompt + investigateSuffix
	firstUser := explain.BuildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads) +
		"\n\nInvestigate the findings with the read-only tools, then explain."
	conv := c.newConversation(system, firstUser, toolSpecs())
	narrative, trail, err := runLoop(ctx, conv, Reader{client: client}, NewScope(workloads))
	if err != nil {
		return Report{}, fmt.Errorf("investigating: %w", err)
	}
	if narrative == "" {
		return Report{}, fmt.Errorf("investigating: model returned no text")
	}
	return Report{Consulted: trail, Narrative: narrative}, nil
}

// anthropicConversation is the real tool-use session, backed by the Anthropic SDK.
type anthropicConversation struct {
	client anthropic.Client
	model  string
	system string
	tools  []anthropic.ToolUnionParam
	msgs   []anthropic.MessageParam
}

func (a *anthropicConversation) start(ctx context.Context) (reply, error) {
	return a.roundtrip(ctx, a.tools)
}

func (a *anthropicConversation) next(ctx context.Context, results []toolResult) (reply, error) {
	a.msgs = append(a.msgs, anthropic.NewUserMessage(toolResultBlocks(results)...))
	return a.roundtrip(ctx, a.tools)
}

func (a *anthropicConversation) conclude(ctx context.Context, results []toolResult) (reply, error) {
	blocks := toolResultBlocks(results)
	blocks = append(blocks, anthropic.NewTextBlock(
		"Stop investigating now and give your final explanation using only what you have observed."))
	a.msgs = append(a.msgs, anthropic.NewUserMessage(blocks...))
	return a.roundtrip(ctx, nil) // no tools offered → the model must answer
}

func (a *anthropicConversation) roundtrip(ctx context.Context, tools []anthropic.ToolUnionParam) (reply, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 2048,
		System:    []anthropic.TextBlockParam{{Text: a.system}},
		Messages:  a.msgs,
		Tools:     tools,
	})
	if err != nil {
		return reply{}, err
	}
	a.msgs = append(a.msgs, resp.ToParam())
	return toReply(resp), nil
}

func toReply(resp *anthropic.Message) reply {
	var r reply
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			r.Text += b.Text
		case anthropic.ToolUseBlock:
			r.Calls = append(r.Calls, toolCall{ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	r.Done = resp.StopReason != anthropic.StopReasonToolUse
	return r
}

func toolResultBlocks(results []toolResult) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, len(results))
	for i, res := range results {
		blocks[i] = anthropic.NewToolResultBlock(res.ID, res.Content, res.IsError)
	}
	return blocks
}

func toAnthropicTools(specs []toolSpec) []anthropic.ToolUnionParam {
	out := make([]anthropic.ToolUnionParam, len(specs))
	for i, s := range specs {
		t := anthropic.ToolUnionParamOfTool(
			anthropic.ToolInputSchemaParam{Properties: s.Properties, Required: s.Required}, s.Name)
		t.OfTool.Description = anthropic.String(s.Description)
		out[i] = t
	}
	return out
}
```

- [ ] **Step 4: Run the tests + full build**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/investigate/ ./internal/explain/`
Expected: PASS. (If `anthropic.String` is not the right optional-string constructor, use the SDK's `param.NewOpt(s)` — check `anthropic.String` exists first with `grep -rn 'func String' $(go env GOMODCACHE)/github.com/anthropics/anthropic-sdk-go@v1.51.0/`.)

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/investigate/investigate.go internal/investigate/investigate_test.go
git commit -m "feat(investigate): Anthropic tool-use backend and Investigate entrypoint"
```

---

### Task 6: Report `Investigation` section

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go` (add a focused test; do NOT touch the golden fixture)

**Interfaces:**
- Consumes: `investigate.Report` is NOT imported; the report takes plain fields (keeps `report` decoupled).
- Produces: two new `Input` fields — `Investigation string` and `InvestigationConsulted []string` — rendered in text and JSON.

The golden coverage guard (`TestGoldenInputCoversAllSections`) does **not** check model-dependent fields (it omits `Explanation`), so the golden fixture and snapshot stay unchanged — do not add investigation data to `goldenInput`.

- [ ] **Step 1: Write the failing test**

Rendering goes through the existing `func PrintInventory(in Input, format string, w io.Writer) error` (format `"text"` or `"json"`) — mirror `TestPrintInventory_JSONIncludesExplanation`.

```go
func TestPrintInventory_TextShowsInvestigation(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:                clusterhealth.ClusterHealth{Verdict: "Healthy"},
		Investigation:          "web — ImagePullBackOff\n- Root cause: bad tag",
		InvestigationConsulted: []string{"describe pod shop/web-abc", "events shop/web-abc"},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "── Investigation ──") ||
		!strings.Contains(out, "consulted: describe pod shop/web-abc · events shop/web-abc") ||
		!strings.Contains(out, "Root cause: bad tag") {
		t.Errorf("investigation section missing/malformed:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesInvestigation(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Investigation:          "narrative",
		InvestigationConsulted: []string{"describe pod shop/web-abc"},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"investigation"`) || !strings.Contains(out, `"narrative"`) || !strings.Contains(out, `"consulted"`) {
		t.Errorf("investigation JSON missing: %s", out)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestReport_Investigation`
Expected: FAIL — `unknown field Investigation`.

- [ ] **Step 3: Add the fields and rendering**

In the `Input` struct (the presentation struct, around line 89) add after `Explanation string`:

```go
	Investigation          string
	InvestigationConsulted []string
```

In the JSON view struct `inventoryReport` (around line 59, after the `Explanation string \`json:"explanation,omitempty"\`` field) add:

```go
	Investigation *investigationView `json:"investigation,omitempty"`
```

Define the view type and a helper near the other view types:

```go
type investigationView struct {
	Consulted []string `json:"consulted,omitempty"`
	Narrative string   `json:"narrative"`
}

// investigationOf builds the JSON view, or nil when no investigation ran.
func investigationOf(in Input) *investigationView {
	if in.Investigation == "" {
		return nil
	}
	return &investigationView{Consulted: in.InvestigationConsulted, Narrative: in.Investigation}
}
```

The JSON is rendered from an inline `inventoryReport{...}` literal inside `PrintInventory`'s `case "json":`. Add one field to that literal, right after `Explanation: in.Explanation,`:

```go
			Investigation:      investigationOf(in),
```

The text output is rendered by the unexported `printInventoryText(in, w)`. In that function, after the `Explanation` block (search for `── Explanation ──`, around line 233):

```go
	if in.Investigation != "" {
		if _, err := fmt.Fprintf(w, "\n── Investigation ──\n"); err != nil {
			return err
		}
		if len(in.InvestigationConsulted) > 0 {
			if _, err := fmt.Fprintf(w, "consulted: %s\n", strings.Join(in.InvestigationConsulted, " · ")); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s\n", in.Investigation); err != nil {
			return err
		}
	}
```

(Ensure `strings` is imported in report.go — it almost certainly already is.)

- [ ] **Step 4: Run the report tests (including the golden guard)**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/`
Expected: PASS — including `TestGoldenScanOutput` (golden unchanged) and `TestGoldenInputCoversAllSections`.

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the Investigation section (text + JSON)"
```

---

### Task 7: Wire `--investigate` into `main.go`

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `investigate.New(...).Investigate(...)` (Task 5), the new report fields (Task 6), `explain.ResolveModel`.
- Produces: the `--investigate` flag and its wiring.

`--investigate` requires `ANTHROPIC_API_KEY` (not the local endpoint — tool-use is Anthropic-only in v1) and **supersedes** `--explain` (if both are set, run the investigation and skip the explain call).

- [ ] **Step 1: Write the failing tests**

The real signature is `func run(args []string) error` (it writes to `os.Stdout` internally). Mirror the existing `TestRun_Explain*` tests.

```go
func TestRun_InvestigateNeedsAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	err := run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("expected an ANTHROPIC_API_KEY error, got %v", err)
	}
}

func TestRun_InvestigateRejectsLocalOnlyEndpoint(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://localhost:11434/v1")
	err := run([]string{"scan", "--investigate"})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("investigate must require an Anthropic key even when a local endpoint is set, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_Investigate`
Expected: FAIL — `flag provided but not defined: -investigate`.

- [ ] **Step 3: Add the flag**

Near the other flags (after `explainFlag`, ~line 70):

```go
	investigateFlag := fs.Bool("investigate", false, "agentic read-only investigation of findings via a bounded tool-use loop (needs ANTHROPIC_API_KEY; supersedes --explain)")
```

Add `--investigate` to the usage string (line 63) after `[--explain]`.

- [ ] **Step 4: Add the precondition**

After the existing `--explain` precondition block (~line 105), add:

```go
	if *investigateFlag && os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("--investigate needs ANTHROPIC_API_KEY (local endpoints do not support the tool-use loop yet)")
	}
```

- [ ] **Step 5: Wire the call (supersede `--explain`)**

Import `"github.com/imantaba/kubeagent/internal/investigate"`. Replace the existing explain block **including its `var explanation string` declaration** (from `var explanation string` at ~line 169 through the closing `}` of `if *explainFlag { ... }`) with the following — do not leave the old `var explanation string` behind or it will be a duplicate declaration:

```go
	var explanation string
	var investigationReport investigate.Report
	switch {
	case *investigateFlag:
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		investigationReport, err = investigate.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).
			Investigate(ctx, health, &summary, &facts, serviceIssues, result.Workloads, client)
		if err != nil {
			return err
		}
	case *explainFlag:
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		explanation, err = explain.NewFromConfig(explainModel, explainEndpoint, os.Getenv("KUBEAGENT_EXPLAIN_API_KEY")).
			ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
		if err != nil {
			return err
		}
	}
```

Where the report `Input` is assembled (after `in := resultInput(res)` and the `in.Explanation = explanation` line — search for where `Explanation` is set on `in`), add:

```go
	in.Investigation = investigationReport.Narrative
	in.InvestigationConsulted = investigationReport.Consulted
```

(If `explanation` is currently assigned directly onto `in.Explanation`, keep that; just add the two investigation lines beside it.)

- [ ] **Step 6: Build and test**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/...`
Expected: PASS.

- [ ] **Step 7: Binary smoke**

```bash
cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build -o kubeagent .
env -u ANTHROPIC_API_KEY ./kubeagent scan --investigate 2>&1 | head -1   # expect the ANTHROPIC_API_KEY error
./kubeagent scan --help 2>&1 | grep -i investigate                        # flag documented
rm -f kubeagent
```

Expected: the precondition error and the flag appearing in help.

- [ ] **Step 8: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add main.go main_test.go
git commit -m "feat: --investigate flag (agentic read-only follow-up reads; supersedes --explain)"
```

---

### Task 8: Docs — diagnostics, README, CHANGELOG, roadmap

**Files:**
- Modify: `website/docs/features/diagnostics.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none (docs only). No code changes; this task ships the user-facing documentation for the feature.

- [ ] **Step 1: Add the diagnostics subsection**

In `website/docs/features/diagnostics.md`, near the `--explain` section, add an `### Agentic investigation (`--investigate`)` subsection covering: it runs the scan then a bounded, read-only tool-use loop that lets the model describe objects, read events, and hop to related objects (owner/node/PVC) to chase a root cause; **needs `ANTHROPIC_API_KEY`** (Anthropic-only — tool-use); **supersedes `--explain`**; findings-scoped, capped (8 reads / 6 turns), **no logs**, structured-only egress, never writes. Show `kubeagent scan --investigate` and a short sample `Investigation` section with a `consulted:` trail.

- [ ] **Step 2: Add a README bullet**

In `README.md`, alongside the `--explain` mention, add a line for `--investigate` (one sentence: agentic read-only follow-up reads, Anthropic-only, supersedes `--explain`).

- [ ] **Step 3: Add the CHANGELOG entry**

Under `## [Unreleased]` → `### Added` in `CHANGELOG.md`:

```markdown
- **Agentic `--investigate`.** After a scan, an opt-in bounded tool-use loop lets the
  model make read-only follow-up reads — describe an object, list its events, hop to a
  related owner/node/PVC — to chase a root cause across the finding's resource graph,
  then emits an `Investigation` section (evidence trail + the grounded fix). Findings-
  scoped, capped (8 reads / 6 turns), no logs, structured-only egress, never writes.
  Anthropic-only (`ANTHROPIC_API_KEY`); supersedes `--explain`.
```

- [ ] **Step 4: Add the roadmap bullet**

In `website/docs/roadmap.md`, under the Theme C "Shipped" list, add a bullet: `--investigate` — agentic read-only follow-up reads (bounded tool-use loop over findings), closing Theme C's principled-intelligence slices.

- [ ] **Step 5: Verify the site builds**

Run: `cd /home/ubuntu/git/kubeagent/website && $HOME/.local/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -1` (or the scratchpad mkdocs path used in this repo)
Expected: `Documentation built` with no page WARNINGs.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document --investigate (agentic read-only follow-up reads)"
```

---

## Release (after all tasks + whole-branch review)

- **Gate:** LIGHTWEIGHT — deterministic unit tests are the gate (fake `conversation` + fake clientset); no cluster change, no new RBAC, no Helm/template change, no writes. **Plus one keyed live smoke:** with a real `ANTHROPIC_API_KEY`, run `--investigate` against a Kind cluster with one injected fault (e.g. a bad-image CrashLoop) and confirm the loop consults the pod's events and concludes with a grounded fix. (The chaos suite can't drive `--investigate` without a key.)
- **Version:** minor **v0.49.0 → v0.50.0**.
- **Chart:** **PATCH** — no new RBAC rule, no template/values change (the base ClusterRole already grants every read).

## Self-Review notes (author)

- **Spec coverage:** mechanism (Tasks 4–5), 3-tool allowlist (Task 4), findings-scoped reach (Task 2 + reader checks in Task 3), fixed caps (Task 4), no-logs / structured-only egress (Task 3 + its egress test), standalone flag + supersede + own section (Tasks 6–7), Anthropic-only precondition (Task 7), reuse explain unchanged (Task 1), no new RBAC / chart patch (Release), docs (Task 8). All covered.
- **Type consistency:** `toolCall`/`toolResult`/`reply`/`Scope`/`conversation`/`executor`/`toolSpec`/`Report`/`Client` names are used identically across Tasks 2–7. `Investigate` signature matches the `explain.ExplainInventory` argument order (cluster, summary, facts, serviceIssues, workloads) plus the k8s `client`.
- **Verification hooks:** each code task confirms an actual current signature (`run(...)`, `Write*` names, `anthropic.String`) before relying on it, so an out-of-order implementer won't hard-code a wrong name.
