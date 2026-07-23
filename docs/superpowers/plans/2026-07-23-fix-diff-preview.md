# `--fix` Diff Preview + Preview→Apply Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every proposed `--fix` action shows a curated field-level `will change:` diff before any write, `Apply` refuses to write if the cluster drifted from what was previewed, and the JSON report carries the plan as `remediationPlan`.

**Architecture:** All contract logic lives in `internal/remediate`: a `Change` type, plan-time diff computed purely from the ReplicaSet list `Plan` already receives, and an apply-time revision bond in `applyRolloutUndo`. `main.go` computes the plan once, renders the diff in `runFixes`, and passes the plan to the report; `internal/report` adds a JSON view. Text report unchanged → golden untouched.

**Tech Stack:** Go 1.26, client-go (fake clientset + `cli.Actions()` recorder in tests), stdlib only. No new dependency.

## Global Constraints

- **Writes stay guard-railed and opt-in.** No new write paths; the bond only makes `Apply` stricter (refusals, never new writes). Protected namespaces, per-action confirmation, `--dry-run`/`--yes` unchanged.
- **No secrets in output.** Diff renders only revisions, image refs, booleans, and counts — never env values, args, or raw template content.
- **`Plan` stays pure** — no I/O; the diff comes from already-collected data.
- **No new dependency. No RBAC/Helm change** (chart PATCH).
- **Golden snapshot unchanged** — the text report does not render the plan; `TestGoldenScanOutput` must keep passing without regeneration.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD** — failing test first. **gofmt-clean.** `go build ./... && go test ./...` before every commit.

## File Structure

- `internal/remediate/remediate.go` — `Change`, enriched `Action`, `planTarget` (replaces `previousRevision`), `templateChanges`/`otherChangeCount`, the apply-time bond.
- `main.go` — plan-once wiring; `runFixes` takes `[]remediate.Action`; `will change:` block.
- `internal/report/report.go` — `Input.RemediationPlan`, `remediationActionView`, JSON field.
- Docs: `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: Plan-time diff + aligned target selection (`internal/remediate`)

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: existing `Action`, `Plan`, fixtures `dep(...)`, `rs(...)`, `rsWithImage(...)` in the test file.
- Produces (later tasks rely on these exact names):
  - `type Change struct { Field string `json:"field"`; From string `json:"from,omitempty"`; To string `json:"to,omitempty"` }`
  - `Action` fields `Changes []Change`, `CurrentRevision int`, `TargetRevision int`
  - `planTarget(namespace, deployment string, replicaSets []appsv1.ReplicaSet) (cur, target *appsv1.ReplicaSet)`

**BEHAVIORAL CHANGE (intended, spec-mandated):** `Plan`'s target selection is aligned with `pickTarget`: the target is the highest revision strictly below current **whose pod template differs** (pod-template-hash stripped). The old `previousRevision` (bare second-highest revision) is removed. Consequence: the existing Plan tests that use the template-less `rs()` fixture for both revisions (their templates are equal, both empty) will propose NO action after this change — those tests must be updated to `rsWithImage` fixtures with differing images. That is not test-weakening; it is the alignment the spec requires (fixes a latent plan/apply mismatch).

- [ ] **Step 1: Write the failing tests**

Append to `internal/remediate/remediate_test.go`:

```go
func TestPlan_RolloutUndoCarriesDiffAndRevisions(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:broken"),
	}
	got := Plan(wls, rss, nil)
	if len(got) != 1 {
		t.Fatalf("want one action, got %+v", got)
	}
	a := got[0]
	if a.CurrentRevision != 2 || a.TargetRevision != 1 {
		t.Errorf("revisions: got cur=%d target=%d, want 2/1", a.CurrentRevision, a.TargetRevision)
	}
	want := []Change{
		{Field: "revision", From: "2", To: "1"},
		{Field: "image (c)", From: "nginx:broken", To: "nginx:1.27"},
	}
	if len(a.Changes) != 2 || a.Changes[0] != want[0] || a.Changes[1] != want[1] {
		t.Errorf("changes = %+v, want %+v", a.Changes, want)
	}
}

func TestPlan_SkipsSameTemplatePriorRevision(t *testing.T) {
	// rev 1 and rev 2 have IDENTICAL templates (same image): nothing to roll back to.
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:same"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:same"),
	}
	if got := Plan(wls, rss, nil); len(got) != 0 {
		t.Fatalf("same-template prior revision -> no action (plan/apply alignment), got %+v", got)
	}
}

func TestPlan_TargetSkipsSameTemplateToDeeperRevision(t *testing.T) {
	// rev 3 (current, broken), rev 2 same template as 3, rev 1 differs -> target must be 1.
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{
		rsWithImage("shop", "web-1", "web", "1", "nginx:1.27"),
		rsWithImage("shop", "web-2", "web", "2", "nginx:broken"),
		rsWithImage("shop", "web-3", "web", "3", "nginx:broken"),
	}
	got := Plan(wls, rss, nil)
	if len(got) != 1 || got[0].TargetRevision != 1 {
		t.Fatalf("want target revision 1 (rev 2 template equals current), got %+v", got)
	}
}

func TestPlan_ReportsOtherTemplateFieldChanges(t *testing.T) {
	// Same image, but the target adds a command -> no image line; one "other fields" line.
	cur := rsWithImage("shop", "web-2", "web", "2", "nginx:1.27")
	prior := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	prior.Spec.Template.Spec.Containers[0].Command = []string{"/bin/serve"}
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	got := Plan(wls, []appsv1.ReplicaSet{prior, cur}, nil)
	if len(got) != 1 {
		t.Fatalf("want one action, got %+v", got)
	}
	a := got[0]
	if len(a.Changes) != 2 || a.Changes[0].Field != "revision" {
		t.Fatalf("changes = %+v, want revision line + other-fields line", a.Changes)
	}
	other := a.Changes[1]
	if other.Field != "1 other template field changed" || other.From != "" || other.To != "" {
		t.Errorf("other-fields line = %+v; must carry a count only, never contents", other)
	}
	for _, c := range a.Changes {
		if strings.Contains(c.Field+c.From+c.To, "/bin/serve") {
			t.Errorf("template content leaked into the diff: %+v", c)
		}
	}
}

func TestPlan_UncordonCarriesStaticChange(t *testing.T) {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: corev1.NodeSpec{Unschedulable: true}}
	got := Plan(nil, nil, []corev1.Node{n})
	if len(got) != 1 {
		t.Fatalf("want one uncordon, got %+v", got)
	}
	want := Change{Field: "spec.unschedulable", From: "true", To: "false"}
	if len(got[0].Changes) != 1 || got[0].Changes[0] != want {
		t.Errorf("changes = %+v, want [%+v]", got[0].Changes, want)
	}
}
```

Add `"strings"` to the test file imports.

- [ ] **Step 2: Update the existing Plan tests to differing-image fixtures**

The aligned selection requires a differing template, so update these tests in place (same assertions, richer fixtures):
- `TestPlan_ProposesRolloutUndo`, `TestPlan_ErrImagePullAlsoTriggers`, `TestPlan_SkipsAvailableDeployment`, `TestPlan_SkipsProtectedNamespace`, `TestPlan_EmitsBothRolloutUndoAndUncordon`: replace each pair `rs(ns, "web-1", "web", "1"), rs(ns, "web-2", "web", "2")` with `rsWithImage(ns, "web-1", "web", "1", "nginx:1.27"), rsWithImage(ns, "web-2", "web", "2", "nginx:broken")`.
- `TestPlan_SkipsWithoutImagePullFinding` and `TestPlan_SkipsWithoutPriorRevision` can stay as-is (they assert no action, which still holds).

- [ ] **Step 3: Run to verify the new tests fail**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `undefined: Change` (and `a.CurrentRevision` unknown field).

- [ ] **Step 4: Implement in `remediate.go`**

Add the type and fields:

```go
// Change is one previewed field change, e.g. {"image (web)", "web:v2", "web:v1"}.
// From/To are always safe display values (revisions, image refs, booleans, counts) —
// never env values or raw template content. A count-only line (e.g. "2 other
// template fields changed") leaves From/To empty.
type Change struct {
	Field string `json:"field"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}
```

`Action` gains (after `KubectlEquivalent`):

```go
	Changes         []Change // the previewed field-level diff (rendered + JSON)
	CurrentRevision int      // RolloutUndo: revision current at preview time (0 for Uncordon)
	TargetRevision  int      // RolloutUndo: revision the rollback lands on (0 for Uncordon)
```

Replace `previousRevision` with `planTarget` (delete `previousRevision`):

```go
// planTarget returns the deployment's current (highest-revision) owned ReplicaSet
// and the rollback target — the highest revision strictly below current whose pod
// template differs — or nils if there is no current or no differing prior revision.
// This is the same selection rule Apply's pickTarget uses, so what Plan previews is
// what Apply lands on.
func planTarget(namespace, deployment string, replicaSets []appsv1.ReplicaSet) (cur, target *appsv1.ReplicaSet) {
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != namespace || !ownedBy(*rs, deployment) || revFromAnnotations(rs.Annotations) == 0 {
			continue
		}
		if cur == nil || revFromAnnotations(rs.Annotations) > revFromAnnotations(cur.Annotations) {
			cur = rs
		}
	}
	if cur == nil {
		return nil, nil
	}
	curRev := revFromAnnotations(cur.Annotations)
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != namespace || !ownedBy(*rs, deployment) {
			continue
		}
		r := revFromAnnotations(rs.Annotations)
		if r == 0 || r >= curRev {
			continue
		}
		if templatesEqual(rs.Spec.Template, cur.Spec.Template) {
			continue
		}
		if target == nil || r > revFromAnnotations(target.Annotations) {
			target = rs
		}
	}
	return cur, target
}
```

The diff builders:

```go
// templateChanges renders the curated preview diff between the current and target
// templates: the revision line, per-container image changes, and a count-only line
// for any other differences. Never prints template contents.
func templateChanges(cur, target appsv1.ReplicaSet) []Change {
	curRev, targetRev := revFromAnnotations(cur.Annotations), revFromAnnotations(target.Annotations)
	changes := []Change{{Field: "revision", From: strconv.Itoa(curRev), To: strconv.Itoa(targetRev)}}
	targetImages := map[string]string{}
	for _, c := range target.Spec.Template.Spec.Containers {
		targetImages[c.Name] = c.Image
	}
	for _, c := range cur.Spec.Template.Spec.Containers {
		if to, ok := targetImages[c.Name]; ok && to != c.Image {
			changes = append(changes, Change{Field: "image (" + c.Name + ")", From: c.Image, To: to})
		}
	}
	if n := otherChangeCount(cur.Spec.Template, target.Spec.Template); n > 0 {
		field := strconv.Itoa(n) + " other template field"
		if n > 1 {
			field += "s"
		}
		changes = append(changes, Change{Field: field + " changed"})
	}
	return changes
}

// otherChangeCount counts template differences beyond container images, comparing
// with pod-template-hash stripped and images neutralized (they are reported
// separately). Each differing aspect counts once; contents are never exposed.
func otherChangeCount(a, b corev1.PodTemplateSpec) int {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	delete(ac.Labels, "pod-template-hash")
	delete(bc.Labels, "pod-template-hash")
	for i := range ac.Spec.Containers {
		ac.Spec.Containers[i].Image = ""
	}
	for i := range bc.Spec.Containers {
		bc.Spec.Containers[i].Image = ""
	}
	n := 0
	if !apiequality.Semantic.DeepEqual(ac.Labels, bc.Labels) {
		n++
	}
	if !apiequality.Semantic.DeepEqual(ac.Annotations, bc.Annotations) {
		n++
	}
	if len(ac.Spec.Containers) != len(bc.Spec.Containers) || len(ac.Spec.InitContainers) != len(bc.Spec.InitContainers) {
		n++
	} else {
		for i := range ac.Spec.Containers {
			if !apiequality.Semantic.DeepEqual(ac.Spec.Containers[i], bc.Spec.Containers[i]) {
				n++
			}
		}
		for i := range ac.Spec.InitContainers {
			if !apiequality.Semantic.DeepEqual(ac.Spec.InitContainers[i], bc.Spec.InitContainers[i]) {
				n++
			}
		}
	}
	podA, podB := ac.Spec.DeepCopy(), bc.Spec.DeepCopy()
	podA.Containers, podB.Containers = nil, nil
	podA.InitContainers, podB.InitContainers = nil, nil
	if !apiequality.Semantic.DeepEqual(podA, podB) {
		n++
	}
	return n
}
```

Rewire `Plan`'s RolloutUndo branch (replacing the `previousRevision` call):

```go
		cur, target := planTarget(w.Namespace, w.Name, replicaSets)
		if target == nil {
			continue
		}
		targetRev := revFromAnnotations(target.Annotations)
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Target:            w.Namespace + "/" + w.Name + " (Deployment)",
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + strconv.Itoa(targetRev) + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
			Changes:           templateChanges(*cur, *target),
			CurrentRevision:   revFromAnnotations(cur.Annotations),
			TargetRevision:    targetRev,
		})
```

Uncordon branch gains:

```go
			Changes: []Change{{Field: "spec.unschedulable", From: "true", To: "false"}},
```

Remove the now-unused `sort` import if `previousRevision` was its only user.

- [ ] **Step 5: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS (new + updated tests; Apply tests still pass — the bond comes in Task 2).

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): plan-time field-level diff and aligned target selection"
```

---

### Task 2: The preview→apply bond (`applyRolloutUndo`)

**Files:**
- Modify: `internal/remediate/remediate.go` (applyRolloutUndo only)
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: `Action.CurrentRevision`/`TargetRevision` (Task 1), existing `pickTarget`, `revFromAnnotations`, fixtures `depObj(...)`, `rsWithImage(...)`.
- Produces: the refusal contract — on revision drift, `Result{Applied:false, Detail:"state changed since preview ..."}` and **zero writes**.

- [ ] **Step 1: Write the failing tests**

```go
func TestApply_RefusesOnCurrentRevisionDrift(t *testing.T) {
	// Previewed cur=2 target=1, but a new rollout happened: deployment is now rev 3.
	cur := depObj("shop", "web", "nginx:still-broken", "3")
	r1 := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:broken")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:still-broken")
	cli := fake.NewSimpleClientset(cur, &r1, &r2, &r3)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 2, TargetRevision: 1,
	})
	if res.Applied || res.Err != nil {
		t.Fatalf("drift must refuse without error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "state changed since preview") {
		t.Errorf("detail = %q, want the drift refusal", res.Detail)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" || act.GetVerb() == "patch" || act.GetVerb() == "delete" {
			t.Fatalf("drift refusal must make no write, saw %s", act.GetVerb())
		}
	}
}

func TestApply_RefusesOnTargetRevisionDrift(t *testing.T) {
	// Previewed target=1, but rev 1's RS is gone; pickTarget would land on rev 2.
	cur := depObj("shop", "web", "nginx:broken3", "3")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:1.27")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:broken3")
	cli := fake.NewSimpleClientset(cur, &r2, &r3)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 3, TargetRevision: 1,
	})
	if res.Applied || !strings.Contains(res.Detail, "state changed since preview") {
		t.Fatalf("target drift must refuse, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("target drift refusal must make no write")
		}
	}
}

func TestApply_MatchingPreviewApplies(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		CurrentRevision: 2, TargetRevision: 1,
	})
	if !res.Applied || res.Err != nil {
		t.Fatalf("matching preview must apply, got %+v", res)
	}
}
```

- [ ] **Step 2: Update the existing Apply tests to carry the promised revisions**

The bond is strict: `applyRolloutUndo` always enforces the revisions. Update:
- `TestApply_RollsBackToPreviousTemplate`: the `Action{...}` literal gains `CurrentRevision: 2, TargetRevision: 1`.
- `TestApply_NoTargetWhenOnlyCurrentRevision`: pickTarget returns nil before the bond check, so its Action needs no revisions — leave as-is (verify it still passes).

- [ ] **Step 3: Run to verify the new tests fail**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -run TestApply`
Expected: FAIL — the drift tests apply (no bond yet), `Applied == true` where refusal expected.

- [ ] **Step 4: Implement the bond**

In `applyRolloutUndo`, after the `target == nil` early return and before the template copy:

```go
	curRev, targetRev := revFromAnnotations(dep.Annotations), revFromAnnotations(target.Annotations)
	if curRev != a.CurrentRevision || targetRev != a.TargetRevision {
		res.Detail = fmt.Sprintf(
			"state changed since preview (revision %d is now current and the rollback would land on %d; previewed %d → %d) — re-run kubeagent scan --fix; no write made",
			curRev, targetRev, a.CurrentRevision, a.TargetRevision)
		return res
	}
```

- [ ] **Step 5: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS — drift refuses with zero writes; matching preview applies exactly as before.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): bind Apply to the previewed revisions (refuse on drift)"
```

---

### Task 3: Plan-once wiring + `will change:` rendering (`main.go`)

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `remediate.Plan`, enriched `remediate.Action` (Tasks 1–2).
- Produces: `runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader)` — new signature (drops workloads/replicaSets/nodes); `fixPlan` variable in `runScan` that Task 4 passes to the report.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (near the other `TestRunFixes_*`):

```go
func TestRunFixes_PrintsWillChangeBlock(t *testing.T) {
	actions := []remediate.Action{{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target: "shop/web (Deployment)", Summary: "roll back to the previous revision",
		Reason:            "newest rollout cannot pull its image; a prior revision (1) exists",
		KubectlEquivalent: "kubectl -n shop rollout undo deployment/web",
		Changes: []remediate.Change{
			{Field: "revision", From: "2", To: "1"},
			{Field: "image (c)", From: "nginx:broken", To: "nginx:1.27"},
			{Field: "1 other template field changed"},
		},
		CurrentRevision: 2, TargetRevision: 1,
	}}
	var out bytes.Buffer
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true /*dryRun*/, false, &out, strings.NewReader(""))
	s := out.String()
	for _, want := range []string{
		"will change:",
		"revision: 2 → 1",
		"image (c): nginx:broken → nginx:1.27",
		"1 other template field changed",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test . -run TestRunFixes_PrintsWillChangeBlock`
Expected: FAIL — compile error (runFixes still takes workloads/replicaSets/nodes; `remediate.Change` used).

- [ ] **Step 3: Change `runFixes` to take the planned actions and render the diff**

New signature and body top (replacing the `remediate.Plan` call inside):

```go
// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). The actions were
// planned once in runScan; Apply is bound to what each preview promised. Writes
// are guarded inside remediate.Apply.
func runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
```

In the proposal print, insert the `will change:` block between the reason line and the kubectl line — replace the existing single Fprintf with:

```go
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n", a.Target, a.Summary, a.Reason)
		if len(a.Changes) > 0 {
			fmt.Fprintln(w, "  will change:")
			for _, c := range a.Changes {
				if c.From == "" && c.To == "" {
					fmt.Fprintf(w, "    %s\n", c.Field)
				} else {
					fmt.Fprintf(w, "    %s: %s → %s\n", c.Field, c.From, c.To)
				}
			}
		}
		fmt.Fprintf(w, "  kubectl equivalent: %s\n", a.KubectlEquivalent)
```

- [ ] **Step 4: Plan once in `runScan`**

Before the report input is assembled (near the other flag-gated sections, before `in := resultInput(res)`), add:

```go
	var fixPlan []remediate.Action
	if *fix {
		fixPlan = remediate.Plan(result.Workloads, res.Inputs.ReplicaSets, nodes)
	}
```

Change the callsite after `report.PrintInventory`:

```go
	if *fix {
		runFixes(context.Background(), client, fixPlan, *dryRun, *assumeYes, os.Stdout, os.Stdin)
	}
```

(Task 4 will add `in.RemediationPlan = fixPlan` beside the other `in.` assignments; do not add it in this task.)

- [ ] **Step 5: Update the four existing `TestRunFixes_*` tests to the new signature**

Each currently passes workloads/replicaSets/nodes; convert to planning first, e.g.:

```go
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""))
```

and for the uncordon pair: `actions := remediate.Plan(nil, nil, []corev1.Node{*n})`. **Check `fixRS()`:** with the Task-1 alignment, its two revisions must have differing templates for Plan to propose — if `fixRS()` builds template-less ReplicaSets, update it to differing images (mirror `rsWithImage`). `TestRunFixes_YesApplies` must keep passing: the planned action now carries the revisions and Apply's bond must match the fake cluster's state (it will, since plan and cluster share the same fixtures).

- [ ] **Step 6: Build and test everything**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/remediate/`
Expected: PASS — including the golden test (`go test ./internal/report/` also passes untouched).

- [ ] **Step 7: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add main.go main_test.go
git commit -m "feat: render the will-change diff in --fix proposals (plan once)"
```

---

### Task 4: JSON `remediationPlan` (`internal/report`)

**Files:**
- Modify: `internal/report/report.go`
- Modify: `main.go` (one line: `in.RemediationPlan = fixPlan`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `remediate.Action` / `remediate.Change` (Task 1), `fixPlan` in `runScan` (Task 3).
- Produces: `Input.RemediationPlan []remediate.Action`; JSON key `remediationPlan` (absent when nil).

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (import `"github.com/imantaba/kubeagent/internal/remediate"`):

```go
func TestPrintInventory_JSONIncludesRemediationPlan(t *testing.T) {
	var buf bytes.Buffer
	in := Input{RemediationPlan: []remediate.Action{{
		Kind: "RolloutUndo", Target: "shop/web (Deployment)",
		Summary: "roll back to the previous revision", Reason: "r",
		KubectlEquivalent: "kubectl -n shop rollout undo deployment/web",
		Changes:           []remediate.Change{{Field: "revision", From: "2", To: "1"}},
	}}}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	for _, want := range []string{`"remediationPlan"`, `"kind": "RolloutUndo"`, `"status": "proposed"`, `"field": "revision"`} {
		if !strings.Contains(s, want) {
			t.Errorf("JSON missing %s:\n%s", want, s)
		}
	}
}

func TestPrintInventory_JSONOmitsRemediationPlanWhenNil(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(Input{}, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "remediationPlan") {
		t.Error("remediationPlan must be absent when no plan was computed")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run RemediationPlan`
Expected: FAIL — `unknown field RemediationPlan`.

- [ ] **Step 3: Implement the view**

`Input` gains (after `InvestigationConsulted`):

```go
	RemediationPlan        []remediate.Action // --fix: the proposed actions (JSON only)
```

Near the other view types:

```go
// remediationActionView is the JSON shape of one proposed --fix action. Status is
// always "proposed" in this slice; apply outcomes become durable in the audit-log
// slice.
type remediationActionView struct {
	Kind              string             `json:"kind"`
	Target            string             `json:"target"`
	Summary           string             `json:"summary"`
	Reason            string             `json:"reason"`
	KubectlEquivalent string             `json:"kubectlEquivalent"`
	Changes           []remediate.Change `json:"changes,omitempty"`
	Status            string             `json:"status"`
}

func remediationPlanOf(in Input) []remediationActionView {
	if len(in.RemediationPlan) == 0 {
		return nil
	}
	out := make([]remediationActionView, len(in.RemediationPlan))
	for i, a := range in.RemediationPlan {
		out[i] = remediationActionView{
			Kind: a.Kind, Target: a.Target, Summary: a.Summary, Reason: a.Reason,
			KubectlEquivalent: a.KubectlEquivalent, Changes: a.Changes, Status: "proposed",
		}
	}
	return out
}
```

`inventoryReport` gains (after `Investigation`):

```go
	RemediationPlan []remediationActionView `json:"remediationPlan,omitempty"`
```

The JSON literal in `PrintInventory` gains, after `Investigation: investigationOf(in),`:

```go
			RemediationPlan:    remediationPlanOf(in),
```

Import `"github.com/imantaba/kubeagent/internal/remediate"` in report.go. In `main.go`, beside `in.Investigation = ...`, add:

```go
	in.RemediationPlan = fixPlan
```

- [ ] **Step 4: Run all report tests + golden**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/report/ .`
Expected: PASS — including `TestGoldenScanOutput` (text path renders nothing new; golden untouched).

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/report/ main.go
git commit -m "feat(report): remediationPlan in JSON output"
```

---

### Task 5: Docs

**Files:**
- Modify: `website/docs/features/remediation.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: remediation.md** — in the `--fix` doc: add the `will change:` block to the sample proposal output (revision + image lines, matching the Task-3 format); a short **"The preview is a contract"** paragraph (Apply refuses with `state changed since preview … no write made` if the cluster moved — re-run the scan); and a `remediationPlan` JSON example (kind/target/summary/reason/kubectlEquivalent/changes/status:"proposed"). Match the page's existing tone and heading style.

- [ ] **Step 2: README.md** — extend the existing `--fix` line/bullet with "shows a field-level diff of exactly what will change, and refuses to apply if the cluster drifted since the preview".

- [ ] **Step 3: CHANGELOG.md** — under `## [Unreleased]` → `### Added`:

```markdown
- **`--fix` diff preview + preview→apply contract.** Every proposed fix now shows a
  curated `will change:` diff (revision, per-container images, a safe count of other
  template changes — never env values or template contents) computed at plan time,
  and `Apply` is bound to the preview: if the cluster drifted since (a new rollout,
  the target revision gone), it refuses with `state changed since preview` and makes
  no write. With `--output json`, the plan is included as `remediationPlan`
  (status `proposed`) — the foundation for the coming audit log. Plan and apply now
  share one target-selection rule (highest prior revision with a differing template).
```

- [ ] **Step 4: roadmap.md** — under Theme D, start its "Shipped" list with: `--fix` diff preview + preview→apply contract (plan-time `will change:` diff, drift refusal, `remediationPlan` JSON) — the foundation for the audit-log and RBAC-preflight slices.

- [ ] **Step 5: Verify the site builds**

Run: `cd /home/ubuntu/git/kubeagent/website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -1` (fall back to `mkdocs` on PATH if that venv path is missing)
Expected: `Documentation built`, no page WARNINGs.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/remediation.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document the --fix diff preview and preview→apply contract"
```

---

## Release (after all tasks + whole-branch review)

- **Gate: FULL CHAOS GATE** — touches the `--fix` write path. `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate` (backgrounded, ~7 min); every scenario green, with the fix scenarios exercising the preview + bond live.
- **Version:** minor **v0.50.0 → v0.51.0**.
- **Chart: PATCH** — no RBAC/Helm/template change.

## Self-Review notes (author)

- **Spec coverage:** Change type + curated diff (Task 1), pickTarget alignment (Task 1, with the fixture-update consequence called out), bond/refusal + zero-write drift tests (Task 2), `will change:` render + plan-once (Task 3), `remediationPlan` JSON + golden untouched (Task 4), docs (Task 5), chaos gate/version/chart (Release). All spec sections covered.
- **Type consistency:** `Change{Field,From,To}` with the count-only line's empty From/To used identically in Tasks 1/3/4; `planTarget` produced in Task 1, consumed nowhere else (Apply keeps `pickTarget` — intentionally, the bond reconciles them); `runFixes` new signature consistent between Tasks 3's test and implementation.
- **Known consequence made explicit:** existing Plan tests and `fixRS()` fixtures need differing-image templates after the alignment; the affected tests are enumerated in Tasks 1 and 3.
