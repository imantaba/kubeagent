# `--fix` / `--rollback` conflict retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the four `--fix`/`--rollback` write paths retry a save that hits a 409 Conflict, re-running every guard and the permission preflight on a fresh read each try.

**Architecture:** Each of `applyRolloutUndo`, `applyRolloutForward`, `applyUncordon` and `applyCordon` in `internal/remediate/remediate.go` keeps its protected-namespace check outside, and moves its Get → guards → `Preflight` → mutate → `Update` body into a closure handed to `retry.RetryOnConflict(retry.DefaultRetry, …)`. Inside the closure, every outcome except a failed `Update` is recorded on `res` and ends the loop with `nil`; a failed `Update` returns its raw error, so `RetryOnConflict` retries only a conflict, and the `update deployment:` / `update node:` wrap happens once after the loop.

**Tech Stack:** Go 1.26, `k8s.io/client-go` v0.36.2 (`util/retry`, `kubernetes/fake`, `testing`), `k8s.io/apimachinery` v0.36.2 (`api/errors`).

**Spec:** `docs/superpowers/specs/2026-10-07-remediate-conflict-retry-design.md`

## Global Constraints

- Retry only a conflict, bounded by `retry.DefaultRetry` (5 tries, ~10 ms apart). No new flag, no new env var, no configurable backoff.
- Keep `Update`. Never switch to a patch or server-side apply.
- Every try re-runs every guard and `Preflight` on a fresh read. The protected-namespace check stays outside the loop.
- Every `Detail` string and every error text an operator sees stays byte-identical to today's.
- One audit record per action; `internal/cli` is not touched.
- No JSON shape change; no `schemaVersion` moves; `go.mod`/`go.sum` do not change.
- TDD: write the failing test, run it, see it fail, then implement.
- Never run any test with `-update`.
- Commit with `git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s`. No `Co-Authored-By` trailer and no AI attribution of any kind. Never `git add -A` or `git add .`; never stage `.gitignore`.
- Go is at `/usr/local/go/bin`: `export PATH=$PATH:/usr/local/go/bin`.

## Review Focus

1. Permission revoked between tries — the retry's `Preflight` says no: expect `PreflightDenied`, no write. Pinned by `TestApply_UncordonPreflightDeniedOnRetryMakesNoWrite` (Task 2).
2. Object deleted between tries — the retry's `Get` fails: expect `get node: …`, never `update node: get node: …`. Pinned by `TestApply_UncordonObjectGoneOnRetry` (Task 2).
3. A third party changes the image between tries on a rollback: expect the drift refusal, not a clobber. Pinned by `TestApply_RolloutForwardRefusesWhenConflictRevealsImageChange` (Task 1).
4. Leftover state from a failed try leaking into the result (e.g. `Applied` set from an earlier attempt): the closure resets `res` at the start of every try. Pinned by the refusal-after-conflict tests asserting `!res.Applied`.
5. A cancelled context during the retry: the next `Get` returns the context error and the loop ends at once (a non-conflict error is never retried). The fake clientset ignores contexts, so this is covered by construction, not a test; a reviewer should confirm no code path retries a non-`Update` error.

---

### Task 1: Retry the two Deployment write paths

**Files:**
- Modify: `internal/remediate/remediate.go` (imports; `applyRolloutUndo`; `applyRolloutForward`)
- Test: `internal/remediate/remediate_test.go` (imports; one helper; six tests appended at the end of the file)

**Interfaces:**
- Consumes: existing test helpers `depObj(ns, name, image, curRev string) *appsv1.Deployment`, `rsWithImage(ns, name, owner, rev, image string) appsv1.ReplicaSet`, `allowFix(cli *fake.Clientset)`, `Inverse(kind, ns, name string, fromRevision, toRevision int) (Action, error)`, `Change{Field, From, To string}`.
- Produces: test helper `conflictUpdates(cli *fake.Clientset, resource string, n int, mutate func()) *int` — Task 2 uses it with `resource` `"nodes"`.

- [ ] **Step 1: Add the test imports and the helper**

In `internal/remediate/remediate_test.go`, the import block becomes:

```go
import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)
```

Append to the end of the file:

```go
// conflictUpdates makes the first n updates of resource fail with a 409
// Conflict (n < 0: every one), running mutate (if non-nil) just before each
// conflict — as a controller writing the object in between would. It returns
// a pointer to the number of update calls seen.
//
// mutate must change state through cli.Tracker(), never through the clientset:
// the fake clientset holds its lock while a reactor runs.
func conflictUpdates(cli *fake.Clientset, resource string, n int, mutate func()) *int {
	calls := 0
	cli.PrependReactor("update", resource, func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if n >= 0 && calls > n {
			return false, nil, nil
		}
		if mutate != nil {
			mutate()
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: resource}, "x",
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	})
	return &calls
}

var deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
```

- [ ] **Step 2: Write the failing Deployment tests**

Append to `internal/remediate/remediate_test.go`:

```go
func TestApply_RolloutUndoRetriesAfterConflict(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	allowFix(cli)
	calls := conflictUpdates(cli, "deployments", 1, nil)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1})
	if !res.Applied || res.Err != nil || res.Refused {
		t.Fatalf("one conflict must be retried and applied, got %+v", res)
	}
	if *calls != 2 {
		t.Errorf("update calls = %d, want 2 (one conflict, one success)", *calls)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Errorf("image = %q, want nginx:1.27", got)
	}
}

func TestApply_RolloutForwardRetriesAfterConflict(t *testing.T) {
	cur := depObj("shop", "web", "nginx:1.27", "4")
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsWithImage("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(cur, &r4, &r5)
	allowFix(cli)
	calls := conflictUpdates(cli, "deployments", 1, nil)
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	res := Apply(context.Background(), cli, a)
	if !res.Applied || res.Err != nil || res.Refused {
		t.Fatalf("one conflict must be retried and applied, got %+v", res)
	}
	if *calls != 2 {
		t.Errorf("update calls = %d, want 2", *calls)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:2.0" {
		t.Errorf("image = %q, want the pre-fix nginx:2.0", got)
	}
}

func TestApply_RolloutUndoRefusesWhenConflictRevealsNewRollout(t *testing.T) {
	// Between kubeagent's read and its save, a new rollout lands (revision 3).
	// The retry must re-read, see the revision bond broken, and refuse.
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	allowFix(cli)
	calls := conflictUpdates(cli, "deployments", 1, func() {
		moved := cur.DeepCopy()
		moved.Annotations["deployment.kubernetes.io/revision"] = "3"
		if err := cli.Tracker().Update(deploymentsGVR, moved, "shop"); err != nil {
			t.Errorf("tracker update: %v", err)
		}
	})
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1})
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("a fresh copy that breaks the bond must be refused without error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "state changed since preview") {
		t.Errorf("detail = %q, want the drift refusal", res.Detail)
	}
	if *calls != 1 {
		t.Errorf("update calls = %d, want 1 (the refused retry makes no write)", *calls)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:does-not-exist" {
		t.Errorf("image = %q, want it untouched", got)
	}
}

func TestApply_RolloutForwardRefusesWhenConflictRevealsImageChange(t *testing.T) {
	// Between kubeagent's read and its save, someone deploys nginx:other.
	// The retry must re-read, see the drift, and refuse rather than clobber.
	cur := depObj("shop", "web", "nginx:1.27", "4")
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsWithImage("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(cur, &r4, &r5)
	allowFix(cli)
	calls := conflictUpdates(cli, "deployments", 1, func() {
		moved := cur.DeepCopy()
		moved.Spec.Template.Spec.Containers[0].Image = "nginx:other"
		if err := cli.Tracker().Update(deploymentsGVR, moved, "shop"); err != nil {
			t.Errorf("tracker update: %v", err)
		}
	})
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	a.Changes = append(a.Changes, Change{Field: "image (c)", From: "nginx:2.0", To: "nginx:1.27"})
	res := Apply(context.Background(), cli, a)
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("image drift seen on retry must refuse, got %+v", res)
	}
	if !strings.Contains(res.Detail, "state changed since the fix") {
		t.Errorf("detail = %q, want the drift refusal", res.Detail)
	}
	if *calls != 1 {
		t.Errorf("update calls = %d, want 1", *calls)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:other" {
		t.Errorf("image = %q, want the third party's nginx:other left alone", got)
	}
}

func TestApply_GivesUpAfterRepeatedConflicts(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	allowFix(cli)
	calls := conflictUpdates(cli, "deployments", -1, nil)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1})
	if res.Applied || res.Err == nil {
		t.Fatalf("endless conflicts must end in an error, got %+v", res)
	}
	if !apierrors.IsConflict(res.Err) {
		t.Errorf("err = %v, want a conflict", res.Err)
	}
	if !strings.HasPrefix(res.Err.Error(), "update deployment: ") || strings.Count(res.Err.Error(), "update deployment: ") != 1 {
		t.Errorf("err = %q, want exactly one %q prefix", res.Err, "update deployment: ")
	}
	if *calls != 5 {
		t.Errorf("update calls = %d, want 5 (retry.DefaultRetry)", *calls)
	}
}

func TestApply_DoesNotRetryNonConflictError(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	allowFix(cli)
	calls := 0
	cli.PrependReactor("update", "deployments", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, nil, apierrors.NewTimeoutError("request timed out", 1)
	})
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1})
	if res.Applied || res.Err == nil {
		t.Fatalf("a timeout must end in an error, got %+v", res)
	}
	if !strings.HasPrefix(res.Err.Error(), "update deployment: ") {
		t.Errorf("err = %q, want the %q prefix", res.Err, "update deployment: ")
	}
	if calls != 1 {
		t.Errorf("update calls = %d, want 1 (only a conflict is retried)", calls)
	}
}
```

- [ ] **Step 3: Run the tests and watch them fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate -run 'RetriesAfterConflict|WhenConflictReveals|GivesUpAfterRepeatedConflicts|DoesNotRetryNonConflictError' -v`

Expected: `TestApply_RolloutUndoRetriesAfterConflict`, `TestApply_RolloutForwardRetriesAfterConflict`, `TestApply_RolloutUndoRefusesWhenConflictRevealsNewRollout`, `TestApply_RolloutForwardRefusesWhenConflictRevealsImageChange` and `TestApply_GivesUpAfterRepeatedConflicts` FAIL (no retry: `Err` is the conflict, update calls = 1). `TestApply_DoesNotRetryNonConflictError` PASSES — it pins today's behaviour so the change cannot widen what is retried.

- [ ] **Step 4: Add the retry import**

In `internal/remediate/remediate.go`, add `"k8s.io/client-go/util/retry"` to the import block, directly after `"k8s.io/client-go/kubernetes"`.

- [ ] **Step 5: Rewrite `applyRolloutUndo`**

Replace the whole function with:

```go
func applyRolloutUndo(ctx context.Context, client kubernetes.Interface, a Action) Result {
	if protectedNamespaces[a.Namespace] {
		return Result{Action: a, Err: fmt.Errorf("refusing to act in protected namespace %q", a.Namespace)}
	}
	// A 409 Conflict means something — usually the Deployment controller —
	// wrote the object between the read and the save. Each try re-reads it and
	// re-runs every guard and the preflight, so a save only ever lands on a copy
	// the guards approved. Only the Update's own error leaves the closure, so
	// only a conflict is retried.
	var res Result
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		res = Result{Action: a}
		dep, err := client.AppsV1().Deployments(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			res.Err = fmt.Errorf("get deployment: %w", err)
			return nil
		}
		rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			res.Err = fmt.Errorf("list replicasets: %w", err)
			return nil
		}
		target := pickTarget(dep, rsList.Items)
		if target == nil {
			res.Detail = "no differing prior revision to roll back to (state changed); no write made"
			res.Refused = true
			return nil
		}
		curRev, targetRev := revFromAnnotations(dep.Annotations), revFromAnnotations(target.Annotations)
		if curRev != a.CurrentRevision || targetRev != a.TargetRevision {
			res.Detail = fmt.Sprintf(
				"state changed since preview (revision %d is now current and the rollback would land on %d; previewed %d → %d) — re-run kubeagent scan --fix; no write made",
				curRev, targetRev, a.CurrentRevision, a.TargetRevision)
			res.Refused = true
			return nil
		}
		allowed, reason, err := Preflight(ctx, client, a)
		if err != nil {
			res.Err = fmt.Errorf("permission preflight failed: %w", err)
			return nil
		}
		if !allowed {
			res.PreflightDenied = true
			res.Detail = reason + "; no write attempted"
			return nil
		}
		tpl := *target.Spec.Template.DeepCopy()
		delete(tpl.Labels, "pod-template-hash")
		dep.Spec.Template = tpl
		if _, err := client.AppsV1().Deployments(a.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return err
		}
		res.Applied = true
		res.Detail = fmt.Sprintf("rolled back %s/%s to revision %d (pod template restored)",
			a.Namespace, a.Name, revFromAnnotations(target.Annotations))
		return nil
	})
	if err != nil {
		res.Err = fmt.Errorf("update deployment: %w", err)
	}
	return res
}
```

- [ ] **Step 6: Rewrite `applyRolloutForward`**

Replace the whole function, keeping its doc comment above it unchanged, with:

```go
func applyRolloutForward(ctx context.Context, client kubernetes.Interface, a Action) Result {
	if protectedNamespaces[a.Namespace] {
		return Result{Action: a, Err: fmt.Errorf("refusing to act in protected namespace %q", a.Namespace)}
	}
	// Retried on a 409 Conflict exactly as applyRolloutUndo is: every try
	// re-reads and re-runs steps 1-3 and the preflight.
	var res Result
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		res = Result{Action: a}
		dep, err := client.AppsV1().Deployments(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			res.Err = fmt.Errorf("get deployment: %w", err)
			return nil
		}
		rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			res.Err = fmt.Errorf("list replicasets: %w", err)
			return nil
		}

		// 1. Find the restore target by TargetRevision (= fromRevision; survives the fix).
		var target *appsv1.ReplicaSet
		for i := range rsList.Items {
			rs := &rsList.Items[i]
			if ownedBy(*rs, a.Name) && revFromAnnotations(rs.Annotations) == a.TargetRevision {
				target = rs
				break
			}
		}
		if target == nil {
			res.Detail = fmt.Sprintf("revision %d no longer exists; no write made", a.TargetRevision)
			res.Refused = true
			return nil
		}

		// 2. Nothing to undo: Deployment template is already the target template.
		if templatesEqual(dep.Spec.Template, target.Spec.Template) {
			res.Detail = "already at the pre-fix revision; no write made"
			res.Refused = true
			return nil
		}

		// 3. Drift: the Deployment's current per-container images must still match the
		// post-fix images recorded in the Action's Changes.  Any mismatch means a third
		// party has changed something since the fix — refuse rather than clobber.
		const imagePrefix = "image ("
		for _, c := range a.Changes {
			if !strings.HasPrefix(c.Field, imagePrefix) {
				continue
			}
			// Parse "image (<containerName>)" → containerName
			name := strings.TrimSuffix(strings.TrimPrefix(c.Field, imagePrefix), ")")
			want := c.To // what the fix left as the post-fix image
			// Find the container by name in the current Deployment template.
			var actual string
			for _, container := range dep.Spec.Template.Spec.Containers {
				if container.Name == name {
					actual = container.Image
					break
				}
			}
			if actual != want {
				res.Detail = fmt.Sprintf(
					"state changed since the fix (container %q is now %s; the fix left it at %s) — no write made",
					name, actual, want)
				res.Refused = true
				return nil
			}
		}

		allowed, reason, err := Preflight(ctx, client, a)
		if err != nil {
			res.Err = fmt.Errorf("permission preflight failed: %w", err)
			return nil
		}
		if !allowed {
			res.PreflightDenied = true
			res.Detail = reason + "; no write attempted"
			return nil
		}
		tpl := *target.Spec.Template.DeepCopy()
		delete(tpl.Labels, "pod-template-hash")
		dep.Spec.Template = tpl
		if _, err := client.AppsV1().Deployments(a.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return err
		}
		res.Applied = true
		res.Detail = fmt.Sprintf("rolled %s/%s forward to revision %d (pre-fix pod template restored)",
			a.Namespace, a.Name, a.TargetRevision)
		return nil
	})
	if err != nil {
		res.Err = fmt.Errorf("update deployment: %w", err)
	}
	return res
}
```

Also append one sentence to the doc comment's first paragraph so it still describes the function: change `then the RBAC preflight, then the single write.` to `then the RBAC preflight, then the single write — the whole sequence retried on a write conflict.`

- [ ] **Step 7: Run the new tests and the whole package**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate -run 'RetriesAfterConflict|WhenConflictReveals|GivesUpAfterRepeatedConflicts|DoesNotRetryNonConflictError' -v`
Expected: all six PASS.

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate ./internal/cli && go vet ./internal/remediate`
Expected: `ok` for both packages; vet prints nothing.

- [ ] **Step 8: Commit**

```bash
git add internal/remediate/remediate.go internal/remediate/remediate_test.go
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "fix(remediate): retry a Deployment write that hits a conflict

--fix and --rollback read a Deployment, check it, and save it. When the
Deployment controller wrote the object in between, the save failed with
409 Conflict and the action ended as an error (chaos matrix k3s v1.34,
scenario 9b). Each try now re-reads the Deployment and re-runs every
guard and the permission preflight before saving again, up to
retry.DefaultRetry's five tries. Only a conflict is retried."
```

---

### Task 2: Retry the two Node write paths, and document the retry

**Files:**
- Modify: `internal/remediate/remediate.go` (`applyUncordon`; `applyCordon`)
- Test: `internal/remediate/remediate_test.go` (five tests appended at the end of the file)
- Modify: `website/docs/features/remediation.md` (section "The preview is a contract")
- Modify: `CHANGELOG.md` (`[Unreleased]`)

**Interfaces:**
- Consumes: from Task 1, `conflictUpdates(cli *fake.Clientset, resource string, n int, mutate func()) *int`, the `"k8s.io/client-go/util/retry"` import in `remediate.go`, and the test imports `errors`, `apierrors`, `schema`. Existing: `allowFix(cli *fake.Clientset)`, `Inverse(...)`.
- Produces: nothing later tasks use.

- [ ] **Step 1: Write the failing Node tests**

Append to `internal/remediate/remediate_test.go`:

```go
var nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

func cordonedNode(name string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	n.Spec.Unschedulable = true
	return n
}

func TestApply_UncordonRetriesAfterConflict(t *testing.T) {
	cli := fake.NewSimpleClientset(cordonedNode("worker-1"))
	allowFix(cli)
	calls := conflictUpdates(cli, "nodes", 1, nil)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if !res.Applied || res.Err != nil || res.Refused {
		t.Fatalf("one conflict must be retried and applied, got %+v", res)
	}
	if *calls != 2 {
		t.Errorf("update calls = %d, want 2", *calls)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if out.Spec.Unschedulable {
		t.Error("node should be schedulable after uncordon")
	}
}

func TestApply_CordonRetriesAfterConflict(t *testing.T) {
	cli := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})
	allowFix(cli)
	calls := conflictUpdates(cli, "nodes", 1, nil)
	a, _ := Inverse("Uncordon", "", "worker-1", 0, 0)
	res := Apply(context.Background(), cli, a)
	if !res.Applied || res.Err != nil || res.Refused {
		t.Fatalf("one conflict must be retried and applied, got %+v", res)
	}
	if *calls != 2 {
		t.Errorf("update calls = %d, want 2", *calls)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if !out.Spec.Unschedulable {
		t.Error("node should be cordoned")
	}
}

func TestApply_UncordonRefusesWhenConflictRevealsSchedulableNode(t *testing.T) {
	// Between kubeagent's read and its save, someone else uncordons the node.
	n := cordonedNode("worker-1")
	cli := fake.NewSimpleClientset(n)
	allowFix(cli)
	calls := conflictUpdates(cli, "nodes", 1, func() {
		moved := n.DeepCopy()
		moved.Spec.Unschedulable = false
		if err := cli.Tracker().Update(nodesGVR, moved, ""); err != nil {
			t.Errorf("tracker update: %v", err)
		}
	})
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("a fresh copy that fails the guard must be refused without error, got %+v", res)
	}
	if !strings.Contains(res.Detail, "no longer a safe uncordon target") {
		t.Errorf("detail = %q, want the uncordon refusal", res.Detail)
	}
	if *calls != 1 {
		t.Errorf("update calls = %d, want 1", *calls)
	}
}

func TestApply_UncordonPreflightDeniedOnRetryMakesNoWrite(t *testing.T) {
	// Permission is revoked between the first try and the retry.
	cli := fake.NewSimpleClientset(cordonedNode("worker-1"))
	reviews := 0
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		reviews++
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: reviews == 1},
		}, nil
	})
	calls := conflictUpdates(cli, "nodes", 1, nil)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil || !res.PreflightDenied {
		t.Fatalf("a denied preflight on the retry must skip the write, got %+v", res)
	}
	if reviews != 2 {
		t.Errorf("preflight reviews = %d, want 2 (one per try)", reviews)
	}
	if *calls != 1 {
		t.Errorf("update calls = %d, want 1", *calls)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if !out.Spec.Unschedulable {
		t.Error("node must stay cordoned")
	}
}

func TestApply_UncordonObjectGoneOnRetry(t *testing.T) {
	// The node is deleted between the first try and the retry: the error is the
	// retry's failed read, wrapped once, never "update node: get node: …".
	cli := fake.NewSimpleClientset(cordonedNode("worker-1"))
	allowFix(cli)
	conflictUpdates(cli, "nodes", 1, func() {
		if err := cli.Tracker().Delete(nodesGVR, "", "worker-1"); err != nil {
			t.Errorf("tracker delete: %v", err)
		}
	})
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err == nil {
		t.Fatalf("a vanished node must end in an error, got %+v", res)
	}
	if !apierrors.IsNotFound(res.Err) {
		t.Errorf("err = %v, want not-found", res.Err)
	}
	if !strings.HasPrefix(res.Err.Error(), "get node: ") || strings.Contains(res.Err.Error(), "update node") {
		t.Errorf("err = %q, want only the %q prefix", res.Err, "get node: ")
	}
}

func TestApply_UncordonGivesUpAfterRepeatedConflicts(t *testing.T) {
	cli := fake.NewSimpleClientset(cordonedNode("worker-1"))
	allowFix(cli)
	calls := conflictUpdates(cli, "nodes", -1, nil)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err == nil || !apierrors.IsConflict(res.Err) {
		t.Fatalf("endless conflicts must end in a conflict error, got %+v", res)
	}
	if !strings.HasPrefix(res.Err.Error(), "update node: ") || strings.Count(res.Err.Error(), "update node: ") != 1 {
		t.Errorf("err = %q, want exactly one %q prefix", res.Err, "update node: ")
	}
	if *calls != 5 {
		t.Errorf("update calls = %d, want 5", *calls)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate -run 'Uncordon.*Conflict|CordonRetriesAfterConflict|OnRetry' -v`

Expected: `TestApply_UncordonRetriesAfterConflict`, `TestApply_CordonRetriesAfterConflict`, `TestApply_UncordonRefusesWhenConflictRevealsSchedulableNode`, `TestApply_UncordonPreflightDeniedOnRetryMakesNoWrite`, `TestApply_UncordonObjectGoneOnRetry` and `TestApply_UncordonGivesUpAfterRepeatedConflicts` FAIL (no retry: `Err` is the conflict, one update, one review).

- [ ] **Step 3: Rewrite `applyUncordon`**

Replace the whole function with:

```go
func applyUncordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	// Retried on a 409 Conflict as applyRolloutUndo is: every try re-reads the
	// node and re-runs the precondition and the preflight.
	var res Result
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		res = Result{Action: a}
		n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			res.Err = fmt.Errorf("get node: %w", err)
			return nil
		}
		// apply-time precondition: still cordoned and still no NoExecute taint
		if !n.Spec.Unschedulable || hasNoExecuteTaint(*n) {
			res.Detail = "node is no longer a safe uncordon target (already schedulable or NoExecute-tainted); no write made"
			res.Refused = true
			return nil
		}
		allowed, reason, err := Preflight(ctx, client, a)
		if err != nil {
			res.Err = fmt.Errorf("permission preflight failed: %w", err)
			return nil
		}
		if !allowed {
			res.PreflightDenied = true
			res.Detail = reason + "; no write attempted"
			return nil
		}
		n.Spec.Unschedulable = false
		if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			return err
		}
		res.Applied = true
		res.Detail = "uncordoned node " + a.Name
		return nil
	})
	if err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
	}
	return res
}
```

- [ ] **Step 4: Rewrite `applyCordon`**

Replace the whole function, keeping its doc comment, with:

```go
func applyCordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	// Retried on a 409 Conflict as applyRolloutUndo is.
	var res Result
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		res = Result{Action: a}
		n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			res.Err = fmt.Errorf("get node: %w", err)
			return nil
		}
		if n.Spec.Unschedulable {
			res.Detail = "node is already cordoned; no write made"
			res.Refused = true
			return nil
		}
		allowed, reason, err := Preflight(ctx, client, a)
		if err != nil {
			res.Err = fmt.Errorf("permission preflight failed: %w", err)
			return nil
		}
		if !allowed {
			res.PreflightDenied = true
			res.Detail = reason + "; no write attempted"
			return nil
		}
		n.Spec.Unschedulable = true
		if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
			return err
		}
		res.Applied = true
		res.Detail = "re-cordoned node " + a.Name
		return nil
	})
	if err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
	}
	return res
}
```

- [ ] **Step 5: Run the new tests, the package, and the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate -run 'Uncordon.*Conflict|CordonRetriesAfterConflict|OnRetry' -v`
Expected: all six PASS.

Run: `export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./...`
Expected: vet prints nothing; every package `ok`.

- [ ] **Step 6: Document the retry**

In `website/docs/features/remediation.md`, section `## The preview is a contract`, directly after the paragraph that ends `To
retry, re-run the scan so a fresh preview is computed against current state.`, insert this paragraph (blank line before and after):

```markdown
A save can also be refused because something else — usually the Deployment
controller — wrote the object a moment before kubeagent did (an HTTP 409
Conflict). That is not drift, so kubeagent tries again, up to five times:
each try reads the object afresh and runs the same checks and the same
permission preflight before saving. If the fresh copy fails a check, the
action is refused with no write, exactly as above. Only a conflict is
retried; any other error ends the action at once.
```

In `CHANGELOG.md`, directly under `## [Unreleased]`, insert (blank line before and after):

```markdown
### Fixed

- `--fix` and `--rollback` no longer fail when the object changes between
  kubeagent's read and its save. A 409 Conflict — typically the Deployment
  controller updating a Deployment it was just handed — is now retried up to
  five times, and every try re-reads the object and re-runs every safety
  check and the permission preflight. A fresh copy that fails a check is
  refused with no write, as before. Only a conflict is retried.
```

- [ ] **Step 7: Commit**

```bash
git add internal/remediate/remediate.go internal/remediate/remediate_test.go website/docs/features/remediation.md CHANGELOG.md
git -c user.name=imantaba -c user.email=itn.taba@gmail.com -c core.fileMode=false commit -s -m "fix(remediate): retry a Node write that hits a conflict

Uncordon and Cordon now retry a 409 Conflict the way the Deployment
paths do: re-read the node, re-run the precondition and the permission
preflight, then save again, up to five tries. Document the retry in the
remediation guide and the changelog."
```
