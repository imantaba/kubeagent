# `--fix` Uncordon Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second `--fix` remediation, `Uncordon`, that makes a mistakenly-cordoned node schedulable again, on the existing guard-railed framework.

**Architecture:** Extend `internal/remediate` — `Plan` gains a `nodes` parameter and emits `Uncordon` actions (node `Unschedulable` with no `NoExecute` taint); `Apply` switches on `Action.Kind` and adds a single `Nodes().Update` clearing `Spec.Unschedulable`. `Action` gains a `Target` display field; `main.go` threads `nodes` into `runFixes`.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1` (already imported), client-go (+ fake clientset for tests).

## Global Constraints

- **READ-ONLY by default:** Uncordon runs only under `--fix`; its only write is one `Nodes().Update`. Without `--fix`, behavior is byte-identical to today. Never LLM-decided.
- **Allowlist grows to exactly `{RolloutUndo, Uncordon}`** — nothing else is ever planned or applied; an unknown `Action.Kind` in `Apply` is a no-op error.
- **Uncordon trigger:** `Node.Spec.Unschedulable == true` AND the node has NO `NoExecute` taint. The auto `node.kubernetes.io/unschedulable:NoSchedule` taint is expected and ignored.
- **Apply-time precondition re-check:** re-verify still-unschedulable + still-no-NoExecute; otherwise NO write (skip Result).
- No new Go module dependency (`corev1 "k8s.io/api/core/v1"` is already imported by `remediate.go`).
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: `Plan` — `Action.Target`, `nodes` param, Uncordon planning

**Files:**
- Modify: `internal/remediate/remediate.go`
- Modify: `main.go` (interim: pass `nil` nodes to keep the build green)
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Produces (changed): `Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node) []Action`; `Action` gains `Target string`; unexported `hasNoExecuteTaint(corev1.Node) bool` reused by Task 2.

- [ ] **Step 1: Update existing `Plan` call-sites + add failing Uncordon tests**

In `internal/remediate/remediate_test.go`: every existing `Plan(...)` call gains a third argument `nil` — the calls at lines ~35, 44, 52, 60, 68, 76 become `Plan(wls, rss, nil)` (and the one that is `Plan([]inventory.Workload{w}, nil)` becomes `Plan([]inventory.Workload{w}, nil, nil)`). Then append (the file already imports `corev1`, `metav1`, `appsv1`, `inventory`, `diagnose`):

```go
func node(name string, unschedulable, noExecute bool) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Spec.Unschedulable = unschedulable
	if unschedulable { // the auto NoSchedule cordon taint is always present; must be ignored
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/unschedulable", Effect: corev1.TaintEffectNoSchedule})
	}
	if noExecute {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute})
	}
	return n
}

func TestPlan_ProposesUncordon(t *testing.T) {
	got := Plan(nil, nil, []corev1.Node{node("worker-1", true, false)})
	if len(got) != 1 || got[0].Kind != "Uncordon" || got[0].Name != "worker-1" {
		t.Fatalf("want one Uncordon for worker-1, got %+v", got)
	}
}

func TestPlan_SkipsSchedulableNode(t *testing.T) {
	if got := Plan(nil, nil, []corev1.Node{node("worker-1", false, false)}); len(got) != 0 {
		t.Fatalf("schedulable node -> no action, got %+v", got)
	}
}

func TestPlan_SkipsNoExecuteTaintedNode(t *testing.T) {
	if got := Plan(nil, nil, []corev1.Node{node("worker-1", true, true)}); len(got) != 0 {
		t.Fatalf("NoExecute-tainted cordoned node -> no action, got %+v", got)
	}
}

func TestPlan_EmitsBothRolloutUndoAndUncordon(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	got := Plan(wls, rss, []corev1.Node{node("worker-1", true, false)})
	kinds := map[string]bool{}
	for _, a := range got {
		kinds[a.Kind] = true
	}
	if len(got) != 2 || !kinds["RolloutUndo"] || !kinds["Uncordon"] {
		t.Fatalf("want both RolloutUndo and Uncordon, got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `Plan` takes 2 args not 3 / `too many arguments` / no `Uncordon` produced.

- [ ] **Step 3: Implement**

In `internal/remediate/remediate.go`: add `Target string` to the `Action` struct (after `Name`):

```go
	Target            string // display target, e.g. "shop/web (Deployment)" or "node/worker-1"
```

Change the `Plan` signature and body to also emit `Uncordon` actions and set `Target` on the `RolloutUndo` action:

```go
func Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node) []Action {
	var actions []Action
	for _, w := range workloads {
		if w.Kind != "Deployment" || protectedNamespaces[w.Namespace] {
			continue
		}
		if !hasImagePullFinding(w) {
			continue
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
		if prev == "" {
			continue
		}
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Target:            w.Namespace + "/" + w.Name + " (Deployment)",
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + prev + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
		})
	}
	for _, n := range nodes {
		if !n.Spec.Unschedulable || hasNoExecuteTaint(n) {
			continue
		}
		actions = append(actions, Action{
			Kind:              "Uncordon",
			Name:              n.Name,
			Target:            "node/" + n.Name,
			Summary:           "uncordon the node (make it schedulable)",
			Reason:            "node is cordoned (SchedulingDisabled)",
			KubectlEquivalent: "kubectl uncordon node/" + n.Name,
		})
	}
	return actions
}

// hasNoExecuteTaint reports whether the node carries any NoExecute taint (an active
// drain / NotReady / pressure) — a signal not to fight by uncordoning.
func hasNoExecuteTaint(n corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}
```

Then keep the build green: `main.go`'s `Plan` call (inside `runFixes`, currently `remediate.Plan(workloads, replicaSets)`) becomes `remediate.Plan(workloads, replicaSets, nil)` (interim — Task 3 threads the real nodes).

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -v && go build ./... && go vet ./internal/remediate/ && gofmt -l internal/remediate/ main.go`
Expected: tests PASS, build succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediate/ main.go
git commit -m "feat(remediate): Plan Uncordon actions + Action.Target (nodes param)"
```

---

### Task 2: `Apply` — Uncordon executor + allowlist growth

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: `Action`, `hasNoExecuteTaint`, `Result` (Task 1 + existing).
- Produces: `Apply` now dispatches on `Kind` to `applyRolloutUndo` / `applyUncordon`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/remediate/remediate_test.go` (imports `context`, `corev1`, `metav1`, `fake` are present):

```go
func TestApply_Uncordon(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if !res.Applied || res.Err != nil {
		t.Fatalf("expected applied, got %+v", res)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if out.Spec.Unschedulable {
		t.Errorf("node should be schedulable after uncordon")
	}
}

func TestApply_UncordonSkipsWhenAlreadySchedulable(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}} // already schedulable
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected no-write skip, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatalf("must not write when already schedulable; saw update")
		}
	}
}

func TestApply_UncordonSkipsWhenNoExecuteTainted(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	n.Spec.Taints = []corev1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoExecute}}
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected no-write skip for NoExecute-tainted node, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatalf("must not write a NoExecute-tainted node; saw update")
		}
	}
}
```

(The existing `TestApply_RejectsUnknownKindAndProtectedNs` still covers the unknown-kind error; `TestApply_RollsBackToPreviousTemplate` must still pass unchanged.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `Apply` returns "unknown action kind \"Uncordon\"" so `res.Err` is non-nil for the uncordon tests.

- [ ] **Step 3: Implement — refactor `Apply` into a dispatcher + add `applyUncordon`**

In `internal/remediate/remediate.go`, replace the current `Apply` function so it dispatches on `Kind`, moving the existing RolloutUndo body verbatim into `applyRolloutUndo`, and adding `applyUncordon`:

```go
// Apply performs an allowlisted remediation's single guarded write via client-go.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result {
	switch a.Kind {
	case "RolloutUndo":
		return applyRolloutUndo(ctx, client, a)
	case "Uncordon":
		return applyUncordon(ctx, client, a)
	default:
		return Result{Action: a, Err: fmt.Errorf("unknown action kind %q", a.Kind)}
	}
}

func applyRolloutUndo(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	if protectedNamespaces[a.Namespace] {
		res.Err = fmt.Errorf("refusing to act in protected namespace %q", a.Namespace)
		return res
	}
	dep, err := client.AppsV1().Deployments(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get deployment: %w", err)
		return res
	}
	rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		res.Err = fmt.Errorf("list replicasets: %w", err)
		return res
	}
	target := pickTarget(dep, rsList.Items)
	if target == nil {
		res.Detail = "no differing prior revision to roll back to (state changed); no write made"
		return res
	}
	tpl := *target.Spec.Template.DeepCopy()
	delete(tpl.Labels, "pod-template-hash")
	dep.Spec.Template = tpl
	if _, err := client.AppsV1().Deployments(a.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update deployment: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = fmt.Sprintf("rolled back %s/%s to revision %d (pod template restored)",
		a.Namespace, a.Name, revFromAnnotations(target.Annotations))
	return res
}

func applyUncordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get node: %w", err)
		return res
	}
	// apply-time precondition: still cordoned and still no NoExecute taint
	if !n.Spec.Unschedulable || hasNoExecuteTaint(*n) {
		res.Detail = "node is no longer a safe uncordon target (already schedulable or NoExecute-tainted); no write made"
		return res
	}
	n.Spec.Unschedulable = false
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = "uncordoned node " + a.Name
	return res
}
```

(Make sure the ORIGINAL `Apply` body — the RolloutUndo logic that started with the protected-namespace check — is fully removed and now lives only in `applyRolloutUndo`.)

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -v && go build ./... && go vet ./internal/remediate/ && gofmt -l internal/remediate/`
Expected: all remediate tests PASS (existing RolloutUndo + unknown-kind + the 3 new Uncordon tests), build succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediate/
git commit -m "feat(remediate): Apply Uncordon via client-go; dispatch Apply on Kind"
```

---

### Task 3: `main.go` — thread `nodes` + kind-aware target display

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `remediate.Plan` (3-arg), `remediate.Apply`, `Action.Target`.
- Produces: `runFixes(ctx, client, workloads, replicaSets, nodes []corev1.Node, dryRun, assumeYes bool, w io.Writer, in io.Reader)`.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go` (add `corev1 "k8s.io/api/core/v1"` to imports if not present — the file already imports `appsv1`, `metav1`, `fake`, `bytes`, `context`, `strings`):

```go
func TestRunFixes_UncordonYesApplies(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}}
	n.Spec.Unschedulable = true
	cli := fake.NewSimpleClientset(n)
	var out bytes.Buffer
	runFixes(context.Background(), cli, nil, nil, []corev1.Node{*n}, false, true, &out, strings.NewReader(""))
	got, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if got.Spec.Unschedulable {
		t.Errorf("expected node uncordoned by --yes")
	}
	if !strings.Contains(out.String(), "node/worker-1") {
		t.Errorf("expected the node target in output, got: %s", out.String())
	}
}
```

Also update the two existing `runFixes(...)` calls (in `TestRunFixes_DryRunWritesNothing` and `TestRunFixes_YesApplies`) to insert `nil` for the new `nodes` argument after `replicaSets` — i.e. `runFixes(context.Background(), cli, fixWorkload(), fixRS(), nil, true, false, &out, ...)` and the `false, true` variant likewise.

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRunFixes'`
Expected: FAIL — `runFixes` takes 8 args not 9 / `undefined: corev1` until wired.

- [ ] **Step 3: Wire `main.go`**

Add `corev1 "k8s.io/api/core/v1"` to `main.go` imports if not already present. Change the `runFixes` signature to insert the `nodes` parameter after `replicaSets`:

```go
func runFixes(ctx context.Context, client kubernetes.Interface, workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, nodes []corev1.Node, dryRun, assumeYes bool, w io.Writer, in io.Reader) {
```

Inside it, change the `Plan` call to pass `nodes` (replacing the Task-1 interim `nil`):

```go
	actions := remediate.Plan(workloads, replicaSets, nodes)
```

Change the proposal line to print the kind-aware `Target` instead of the hardcoded `(Deployment)`:

```go
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n  kubectl equivalent: %s\n",
			a.Target, a.Summary, a.Reason, a.KubectlEquivalent)
```

Update the call in `run` to pass `nodes` (the `nodes` variable from `collect.Nodes(...)` is already in scope earlier in `run`):

```go
		runFixes(context.Background(), client, result.Workloads, inputs.ReplicaSets, nodes, *dryRun, *assumeYes, os.Stdout, os.Stdin)
```

- [ ] **Step 4: Run tests + full build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRunFixes|TestRun_Fix' -v && go build ./... && go vet ./... && go test ./... && gofmt -l main.go main_test.go`
Expected: the fix tests PASS, build succeeds, vet clean, all packages `ok`, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: thread nodes into --fix; print kind-aware target"
```

---

### Task 4: docs — README, CHANGELOG, chaos acceptance

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `chaos/README.md`

- [ ] **Step 1: `README.md` — note the second action**

In the `### Remediation (--fix, opt-in)` section, find the line beginning `**v1 remediation:** \`RolloutUndo\`` and change the label to cover both actions. Replace that sentence with:

```markdown
**Remediations:**

- **`RolloutUndo`** — when a Deployment's newest rollout can't pull its image
  (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it back to
  that revision (a single, reversible `Deployment` update via client-go).
- **`Uncordon`** — when a node is cordoned (`SchedulingDisabled`) and has no
  `NoExecute` taint (i.e. it's accidentally cordoned, not being drained), make it
  schedulable again (a single `Node` update; reversible with `kubectl cordon`).
```

- [ ] **Step 2: `CHANGELOG.md` — add to the `--fix` line under `## [Unreleased]`**

Under `## [Unreleased]` → `### Added`, add:

```markdown
- **`--fix` remediation: `Uncordon`.** A second guard-railed action — an
  accidentally-cordoned node (`SchedulingDisabled`, no `NoExecute` taint) is made
  schedulable again after a per-action confirmation. Same rails as `RolloutUndo`
  (allowlist, apply-time precondition re-check, single write, never LLM-decided).
```

- [ ] **Step 3: `chaos/README.md` — uncordon acceptance note**

In the `### Validating --fix (remediation)` subsection, append:

```markdown
Scenario 3 (node cordon) is the acceptance test for `Uncordon`:

```bash
kubectl --context kind-kubeagent-chaos cordon kubeagent-chaos-worker
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos get node kubeagent-chaos-worker   # SchedulingDisabled should be gone
```
```

- [ ] **Step 4: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed).

```bash
git add README.md CHANGELOG.md chaos/README.md
git commit -m "docs(fix): document the Uncordon remediation + chaos acceptance"
```

---

## Self-Review

**Spec coverage:**
- `Action.Target` + `Plan(+nodes)` + Uncordon planning + `hasNoExecuteTaint` → Task 1. ✓
- Trigger (Unschedulable + no NoExecute; ignore the auto NoSchedule taint) → Task 1 `Plan` loop + `hasNoExecuteTaint`, tested. ✓
- `Apply` Kind-dispatch + `applyUncordon` (single `Nodes().Update`, apply-time precondition, no-write skip) + allowlist growth → Task 2. ✓
- `main.go` threads `nodes`, kind-aware target display → Task 3. ✓
- Guards (allowlist, precondition, NoExecute skip; per-action confirm is existing) → Tasks 1–2, tested (no-write on already-schedulable / NoExecute paths). ✓
- Docs (README, CHANGELOG, chaos) → Task 4; CLAUDE.md intentionally unchanged. ✓
- No new module dep; without `--fix` unchanged → corev1 already imported; `runFixes` only runs under `if *fix`. ✓

**Placeholder scan:** none — complete code in every step.

**Type/name consistency:** `Plan(workloads, replicaSets, nodes)`, `Action{...Target...}`, `Apply` → `applyRolloutUndo`/`applyUncordon`, `hasNoExecuteTaint`, and `runFixes(ctx, client, workloads, replicaSets, nodes, dryRun, assumeYes, w, in)` are used identically across Tasks 1–3. The kind strings `"RolloutUndo"`/`"Uncordon"` and the `NoExecute` taint effect (`corev1.TaintEffectNoExecute`) match across plan/apply/tests.
