# `--fix` Rollback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent scan --rollback --audit-log <path>` reads the most recent applied fix from the audit log, derives its deterministic inverse, and applies it through every existing guard rail — recording a new `rollback` disposition.

**Architecture:** `internal/audit` gains structured `fromRevision`/`toRevision` fields and a `ReadLast` reader. `internal/remediate` gains a pure `Inverse` mapper (plain args — `audit` imports `remediate`, never the reverse) plus two allowlisted inverse kinds, `RolloutForward` and `Cordon`, each with the same drift bond + RBAC preflight + protected-namespace guards. `main.go` adds `--rollback` and a `runRollback` sibling of `runFixes`.

**Tech Stack:** Go 1.26, stdlib (`encoding/json`, `bufio`, `os`), client-go (fake clientset + reactors in tests). No new dependency.

## Global Constraints

- **Never LLM-decided.** `Inverse` is a pure deterministic mapping over an allowlist.
- **Same guard rails.** Rollback writes go through preview → confirm → drift bond → RBAC preflight → audit, identical to fixes. No new bypass.
- **Fail closed / refuse on drift.** Cluster moved since the fix (revision changed, node already cordoned, target revision gone) → refuse, no write.
- **No secrets** in records or output (revisions, names, counts only).
- **No new dependency. No RBAC/Helm change** — the inverses use the same `update deployments` / `update nodes` verbs (chart **PATCH**).
- **Golden snapshot unchanged.** **No `Co-Authored-By: Claude` trailer.** **TDD.** gofmt-clean. `go build ./... && go test ./...` before every commit.

## File Structure

- `internal/audit/audit.go` — `Record.FromRevision`/`ToRevision`, `RecordFor` population, `ReadLast`.
- `internal/remediate/remediate.go` — `Inverse`, `applyRolloutForward`, `applyCordon`, `Apply` dispatch, **`Preflight` switch extension**.
- `main.go` — `--rollback` flag, preconditions, `runRollback`, the `rollback` disposition.
- `chaos/run.sh` — scenario 9 gains an apply-then-rollback check.
- Docs: `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

**CRITICAL — `Preflight` must learn the new kinds.** `remediate.Preflight` switches on `a.Kind` and **errors on an unknown kind** (`default: return false, "", fmt.Errorf("unknown action kind %q", a.Kind)`). If `RolloutForward`/`Cordon` are not added to that switch, every rollback fails its preflight with "unknown action kind" and never writes. Task 2 covers this.

---

### Task 1: `internal/audit` — structured revisions + `ReadLast`

**Files:**
- Modify: `internal/audit/audit.go`
- Test: `internal/audit/audit_test.go`

**Interfaces:**
- Consumes: `remediate.Action` (has `CurrentRevision int`, `TargetRevision int`).
- Produces (Tasks 3–4 rely on these):
  - `Record.FromRevision int` (json `fromRevision,omitempty`), `Record.ToRevision int` (json `toRevision,omitempty`)
  - `func ReadLast(path string, want func(Record) bool) (Record, bool, error)`

- [ ] **Step 1: Write the failing tests**

Append to `internal/audit/audit_test.go` (add `"os"`, `"path/filepath"` to imports if absent):

```go
func TestRecordFor_CarriesRevisions(t *testing.T) {
	a := remediate.Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target: "shop/web (Deployment)", CurrentRevision: 5, TargetRevision: 4}
	r := RecordFor(a, "applied", "rolled back", fixedNow)
	if r.FromRevision != 5 || r.ToRevision != 4 {
		t.Errorf("revisions = %d/%d, want 5/4", r.FromRevision, r.ToRevision)
	}
}

func TestRecordFor_UncordonHasNoRevisions(t *testing.T) {
	a := remediate.Action{Kind: "Uncordon", Name: "worker-1", Target: "node/worker-1"}
	r := RecordFor(a, "applied", "uncordoned", fixedNow)
	if r.FromRevision != 0 || r.ToRevision != 0 {
		t.Errorf("node action must have zero revisions, got %d/%d", r.FromRevision, r.ToRevision)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "fromRevision") {
		t.Errorf("zero revisions must be omitted from JSON: %s", b)
	}
}

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func applied(r Record) bool { return r.Disposition == "applied" }

func TestReadLast_ReturnsNewestMatch(t *testing.T) {
	p := writeLines(t,
		`{"time":"2026-07-24T06:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":3,"toRevision":2}`,
		`{"time":"2026-07-24T07:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"refused"}`,
		`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`,
	)
	rec, found, err := ReadLast(p, applied)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if rec.FromRevision != 5 || rec.ToRevision != 4 {
		t.Errorf("want the newest applied record (5→4), got %d→%d", rec.FromRevision, rec.ToRevision)
	}
}

func TestReadLast_SkipsMalformedLines(t *testing.T) {
	p := writeLines(t,
		`{"time":"2026-07-24T06:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"applied"}`,
		`this is not json`,
		`{"partial":`,
	)
	rec, found, err := ReadLast(p, applied)
	if err != nil || !found || rec.Name != "w1" {
		t.Fatalf("malformed lines must be skipped; got rec=%+v found=%v err=%v", rec, found, err)
	}
}

func TestReadLast_NoMatch(t *testing.T) {
	p := writeLines(t, `{"time":"2026-07-24T06:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"declined"}`)
	if _, found, err := ReadLast(p, applied); err != nil || found {
		t.Fatalf("want found=false without error, got found=%v err=%v", found, err)
	}
}

func TestReadLast_MissingFileErrors(t *testing.T) {
	if _, _, err := ReadLast(filepath.Join(t.TempDir(), "nope.log"), applied); err == nil {
		t.Error("expected an error for a missing audit file")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/audit/`
Expected: FAIL — `r.FromRevision` unknown field, `undefined: ReadLast`.

- [ ] **Step 3: Implement**

In `internal/audit/audit.go` add `"bufio"` and `"os"` to the imports. Add to `Record` after `Changes`:

```go
	FromRevision int `json:"fromRevision,omitempty"` // RolloutUndo: revision before the fix (enables rollback)
	ToRevision   int `json:"toRevision,omitempty"`   // RolloutUndo: revision the fix landed on
```

In `RecordFor`, add to the returned literal:

```go
		FromRevision: a.CurrentRevision,
		ToRevision:   a.TargetRevision,
```

Add the reader:

```go
// ReadLast scans the JSON-Lines audit file and returns the most recent record
// satisfying want. Malformed lines are skipped so a truncated tail cannot break
// rollback; found is false when no record matches.
func ReadLast(path string, want func(Record) bool) (Record, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Record{}, false, err
	}
	defer f.Close()
	var last Record
	var found bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue // skip malformed/truncated lines
		}
		if want(r) {
			last, found = r, true
		}
	}
	if err := sc.Err(); err != nil {
		return Record{}, false, err
	}
	return last, found, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/audit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/audit/
git commit -m "feat(audit): structured revisions and ReadLast for rollback"
```

---

### Task 2: `remediate.Inverse` + the two inverse action kinds

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: existing `Action`, `Result`, `Preflight`, `pickTarget`, `ownedBy`, `revFromAnnotations`, `protectedNamespaces`, test helpers `allowFix`/`denyFix`, `depObj`, `rsWithImage`.
- Produces (Task 3 relies on):
  - `func Inverse(kind, namespace, name string, fromRevision, toRevision int) (Action, error)`
  - action kinds `"RolloutForward"` and `"Cordon"` handled by `Apply` **and** `Preflight`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/remediate/remediate_test.go`:

```go
func TestInverse_RolloutUndoBecomesRolloutForward(t *testing.T) {
	a, err := Inverse("RolloutUndo", "shop", "web", 5, 4)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "RolloutForward" || a.Namespace != "shop" || a.Name != "web" {
		t.Fatalf("action = %+v", a)
	}
	if a.CurrentRevision != 4 || a.TargetRevision != 5 {
		t.Errorf("revisions = %d→%d, want 4→5 (undo the fix)", a.CurrentRevision, a.TargetRevision)
	}
	want := Change{Field: "revision", From: "4", To: "5"}
	if len(a.Changes) != 1 || a.Changes[0] != want {
		t.Errorf("changes = %+v, want [%+v]", a.Changes, want)
	}
}

func TestInverse_UncordonBecomesCordon(t *testing.T) {
	a, err := Inverse("Uncordon", "", "worker-1", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "Cordon" || a.Name != "worker-1" {
		t.Fatalf("action = %+v", a)
	}
	want := Change{Field: "spec.unschedulable", From: "false", To: "true"}
	if len(a.Changes) != 1 || a.Changes[0] != want {
		t.Errorf("changes = %+v, want [%+v]", a.Changes, want)
	}
}

func TestInverse_RolloutUndoWithoutRevisionsErrors(t *testing.T) {
	_, err := Inverse("RolloutUndo", "shop", "web", 0, 0)
	if err == nil || !strings.Contains(err.Error(), "v0.54") {
		t.Errorf("pre-v0.54 record must error mentioning the version, got %v", err)
	}
}

func TestInverse_UnknownKindErrors(t *testing.T) {
	if _, err := Inverse("Nope", "", "x", 0, 0); err == nil {
		t.Error("unknown kind must error")
	}
}

func TestApply_RolloutForwardRestoresRevision(t *testing.T) {
	// current rev 4 (the fix landed here); rev 5 is the pre-fix revision to restore.
	cur := depObj("shop", "web", "nginx:1.27", "4")
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsWithImage("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(cur, &r4, &r5)
	allowFix(cli)
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	res := Apply(context.Background(), cli, a)
	if !res.Applied || res.Err != nil {
		t.Fatalf("expected the rollback to apply, got %+v", res)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:2.0" {
		t.Errorf("image = %q, want the pre-fix nginx:2.0", got)
	}
}

func TestApply_RolloutForwardRefusesOnDrift(t *testing.T) {
	cur := depObj("shop", "web", "nginx:other", "6") // moved on since the fix
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsWithImage("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(cur, &r4, &r5)
	allowFix(cli)
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	res := Apply(context.Background(), cli, a)
	if res.Applied || !res.Refused {
		t.Fatalf("drift must refuse, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("drift refusal must make no write")
		}
	}
}

func TestApply_RolloutForwardRefusesWhenTargetRevisionGone(t *testing.T) {
	cur := depObj("shop", "web", "nginx:1.27", "4")
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27") // rev 5 deleted
	cli := fake.NewSimpleClientset(cur, &r4)
	allowFix(cli)
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	res := Apply(context.Background(), cli, a)
	if res.Applied || !res.Refused {
		t.Fatalf("missing target revision must refuse, got %+v", res)
	}
}

func TestApply_RolloutForwardPreflightDenied(t *testing.T) {
	cur := depObj("shop", "web", "nginx:1.27", "4")
	r4 := rsWithImage("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsWithImage("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(cur, &r4, &r5)
	denyFix(cli)
	a, _ := Inverse("RolloutUndo", "shop", "web", 5, 4)
	res := Apply(context.Background(), cli, a)
	if !res.PreflightDenied || res.Applied {
		t.Fatalf("denied preflight expected, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("preflight denial must make no write")
		}
	}
}

func TestApply_CordonSetsUnschedulable(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}} // schedulable
	cli := fake.NewSimpleClientset(n)
	allowFix(cli)
	a, _ := Inverse("Uncordon", "", "worker-1", 0, 0)
	res := Apply(context.Background(), cli, a)
	if !res.Applied || res.Err != nil {
		t.Fatalf("cordon should apply, got %+v", res)
	}
	out, _ := cli.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if !out.Spec.Unschedulable {
		t.Error("node should be cordoned")
	}
}

func TestApply_CordonRefusesWhenAlreadyCordoned(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
		Spec: corev1.NodeSpec{Unschedulable: true}}
	cli := fake.NewSimpleClientset(n)
	allowFix(cli)
	a, _ := Inverse("Uncordon", "", "worker-1", 0, 0)
	res := Apply(context.Background(), cli, a)
	if res.Applied || !res.Refused {
		t.Fatalf("already-cordoned must refuse, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("refusal must make no write")
		}
	}
}

func TestPreflight_KnowsInverseKinds(t *testing.T) {
	cli := fake.NewSimpleClientset()
	var got *authorizationv1.SelfSubjectAccessReview
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		got = a.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	if _, _, err := Preflight(context.Background(), cli, Action{Kind: "RolloutForward", Namespace: "shop", Name: "web"}); err != nil {
		t.Fatalf("RolloutForward preflight errored: %v", err)
	}
	if ra := got.Spec.ResourceAttributes; ra.Resource != "deployments" || ra.Group != "apps" || ra.Namespace != "shop" {
		t.Errorf("RolloutForward attributes = %+v", got.Spec.ResourceAttributes)
	}
	if _, _, err := Preflight(context.Background(), cli, Action{Kind: "Cordon", Name: "worker-1"}); err != nil {
		t.Fatalf("Cordon preflight errored: %v", err)
	}
	if ra := got.Spec.ResourceAttributes; ra.Resource != "nodes" || ra.Group != "" || ra.Namespace != "" {
		t.Errorf("Cordon attributes = %+v", got.Spec.ResourceAttributes)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `undefined: Inverse`.

- [ ] **Step 3: Implement `Inverse`**

Add to `internal/remediate/remediate.go`:

```go
// Inverse returns the deterministic undo of a previously applied remediation, from the
// plain values an audit record carries (this package must not import internal/audit —
// audit imports remediate). Pure: no I/O, never LLM-decided. The returned Action flows
// through the same guard rails as any planned action.
func Inverse(kind, namespace, name string, fromRevision, toRevision int) (Action, error) {
	switch kind {
	case "RolloutUndo":
		if fromRevision == 0 || toRevision == 0 {
			return Action{}, fmt.Errorf("this audit record predates structured rollback data (kubeagent < v0.54); cannot derive a safe rollback")
		}
		return Action{
			Kind:      "RolloutForward",
			Namespace: namespace,
			Name:      name,
			Target:    namespace + "/" + name + " (Deployment)",
			Summary:   "roll forward to the pre-fix revision",
			Reason:    fmt.Sprintf("undo the fix that rolled %s/%s back from revision %d to %d", namespace, name, fromRevision, toRevision),
			KubectlEquivalent: fmt.Sprintf("kubectl -n %s rollout undo deployment/%s --to-revision=%d", namespace, name, fromRevision),
			Changes: []Change{{
				Field: "revision",
				From:  strconv.Itoa(toRevision),
				To:    strconv.Itoa(fromRevision),
			}},
			CurrentRevision: toRevision,   // where the fix left it
			TargetRevision:  fromRevision, // where we are restoring to
		}, nil
	case "Uncordon":
		return Action{
			Kind:              "Cordon",
			Name:              name,
			Target:            "node/" + name,
			Summary:           "re-cordon the node (make it unschedulable)",
			Reason:            "undo the fix that uncordoned node " + name,
			KubectlEquivalent: "kubectl cordon " + name,
			Changes:           []Change{{Field: "spec.unschedulable", From: "false", To: "true"}},
		}, nil
	default:
		return Action{}, fmt.Errorf("no inverse defined for action kind %q", kind)
	}
}
```

- [ ] **Step 4: Extend `Preflight` and `Apply` for the new kinds**

In `Preflight`'s `switch a.Kind` block, extend the existing cases so the inverse kinds map to the same attributes as their forward counterparts:

```go
	case "RolloutUndo", "RolloutForward":
		group, resource, ns = "apps", "deployments", a.Namespace
	case "Uncordon", "Cordon":
		group, resource, ns = "", "nodes", ""
```

In `Apply`'s switch, add:

```go
	case "RolloutForward":
		return applyRolloutForward(ctx, client, a)
	case "Cordon":
		return applyCordon(ctx, client, a)
```

- [ ] **Step 5: Implement the two apply functions**

```go
// applyRolloutForward restores a Deployment to the revision it had before a fix, using
// the same guarded sequence as applyRolloutUndo: state precondition (the deployment is
// still where the fix left it and the target revision still exists), then the RBAC
// preflight, then the single write.
func applyRolloutForward(ctx context.Context, client kubernetes.Interface, a Action) Result {
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
	if curRev := revFromAnnotations(dep.Annotations); curRev != a.CurrentRevision {
		res.Detail = fmt.Sprintf(
			"state changed since the fix (revision %d is now current; the fix left it at %d) — no write made",
			curRev, a.CurrentRevision)
		res.Refused = true
		return res
	}
	rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		res.Err = fmt.Errorf("list replicasets: %w", err)
		return res
	}
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
		return res
	}
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
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
	res.Detail = fmt.Sprintf("rolled %s/%s forward to revision %d (pre-fix pod template restored)",
		a.Namespace, a.Name, a.TargetRevision)
	return res
}

// applyCordon re-cordons a node that a previous fix uncordoned.
func applyCordon(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	n, err := client.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get node: %w", err)
		return res
	}
	if n.Spec.Unschedulable {
		res.Detail = "node is already cordoned; no write made"
		res.Refused = true
		return res
	}
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err)
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
		return res
	}
	n.Spec.Unschedulable = true
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update node: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = "re-cordoned node " + a.Name
	return res
}
```

- [ ] **Step 6: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS (all new tests plus every existing remediate test).

- [ ] **Step 7: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): Inverse mapping with guarded RolloutForward and Cordon"
```

---

### Task 3: `--rollback` flag and `runRollback` (`main.go`)

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `audit.ReadLast`, `audit.Record`, `audit.NewWriter`, `audit.RecordFor` (Task 1); `remediate.Inverse`, `remediate.Apply` (Task 2).
- Produces: `func runRollback(ctx context.Context, client kubernetes.Interface, auditPath string, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) error`

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go`:

```go
func TestRunRollback_UndoesLastAppliedFix(t *testing.T) {
	// The cluster is where the fix left it: rev 4 (nginx:1.27); rev 5 is pre-fix.
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	r4 := rsFor("shop", "web-4", "web", "4", "nginx:1.27")
	r5 := rsFor("shop", "web-5", "web", "5", "nginx:2.0")
	cli := fake.NewSimpleClientset(d, r4, r5)
	allowFix(cli)

	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, auditBuf bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf)); err != nil {
		t.Fatal(err)
	}
	got, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "nginx:2.0" {
		t.Errorf("image = %q, want the pre-fix nginx:2.0", img)
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "rollback" {
		t.Fatalf("want one rollback record, got %+v", recs)
	}
}

func TestRunRollback_NothingToRollBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"Uncordon","name":"w1","target":"node/w1","disposition":"declined"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := fake.NewSimpleClientset()
	var out bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true, &out, strings.NewReader(""), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to roll back") {
		t.Errorf("expected the nothing-to-roll-back message, got: %s", out.String())
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("no write may happen when there is nothing to roll back")
		}
	}
}

func TestRunRollback_PreV054RecordRefuses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := fake.NewSimpleClientset()
	var out bytes.Buffer
	if err := runRollback(context.Background(), cli, p, false, true, &out, strings.NewReader(""), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "v0.54") {
		t.Errorf("expected the version refusal, got: %s", out.String())
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("a pre-v0.54 record must not produce a write")
		}
	}
}

func TestRunRollback_DryRunWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "4"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}}}
	cli := fake.NewSimpleClientset(d)
	allowFix(cli)
	p := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(p, []byte(`{"time":"2026-07-24T08:00:00Z","kind":"RolloutUndo","namespace":"shop","name":"web","target":"shop/web (Deployment)","disposition":"applied","fromRevision":5,"toRevision":4}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, auditBuf bytes.Buffer
	if err := runRollback(context.Background(), cli, p, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf)); err != nil {
		t.Fatal(err)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("dry-run must not write")
		}
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("want one dry-run record, got %+v", recs)
	}
}

func TestRun_RollbackNeedsAuditLog(t *testing.T) {
	err := run([]string{"scan", "--rollback"})
	if err == nil || !strings.Contains(err.Error(), "--audit-log") {
		t.Errorf("expected an --audit-log requirement error, got %v", err)
	}
}

func TestRun_RollbackAndFixAreExclusive(t *testing.T) {
	err := run([]string{"scan", "--rollback", "--fix", "--audit-log", "/tmp/x.log"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected a mutual-exclusion error, got %v", err)
	}
}
```

Add a ReplicaSet fixture helper next to the existing `fixRS` helper (it returns pointers for `NewSimpleClientset`):

```go
func rsFor(ns, name, owner, rev, image string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": owner, "pod-template-hash": "h" + rev}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		}},
	}
}
```

Add `"os"` and `"path/filepath"` to `main_test.go`'s imports if absent.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRunRollback|TestRun_Rollback'`
Expected: FAIL — `undefined: runRollback`, `flag provided but not defined: -rollback`.

- [ ] **Step 3: Add the flag and preconditions**

Near the other fix flags in `run(...)`:

```go
	rollback := fs.Bool("rollback", false, "undo the most recent applied fix recorded in --audit-log (requires --audit-log)")
```

Add `[--rollback --audit-log path]` to the usage string after the `--fix` group.

After the existing `--fix`-related preconditions (before any cluster work):

```go
	if *rollback && *fix {
		return fmt.Errorf("--rollback and --fix are mutually exclusive")
	}
	if *rollback && *auditLog == "" {
		return fmt.Errorf("--rollback requires --audit-log (the file to read the last applied fix from)")
	}
```

- [ ] **Step 4: Implement `runRollback` and wire the callsite**

Add `runRollback` next to `runFixes`:

```go
// runRollback undoes the most recent applied remediation recorded in the audit log. The
// inverse action is derived deterministically (never LLM-decided) and applied through
// the same guard rails as any fix: preview, confirmation, drift bond, RBAC preflight.
func runRollback(ctx context.Context, client kubernetes.Interface, auditPath string, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) error {
	rec, found, err := audit.ReadLast(auditPath, func(r audit.Record) bool { return r.Disposition == "applied" })
	if err != nil {
		return fmt.Errorf("reading audit log %q: %w", auditPath, err)
	}
	if !found {
		fmt.Fprintf(w, "\nNo applied remediation found in %s; nothing to roll back.\n", auditPath)
		return nil
	}
	a, err := remediate.Inverse(rec.Kind, rec.Namespace, rec.Name, rec.FromRevision, rec.ToRevision)
	if err != nil {
		fmt.Fprintf(w, "\nCannot roll back the last applied fix (%s %s): %v\n", rec.Kind, rec.Target, err)
		return nil
	}
	logAudit := func(disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: audit log write failed: %v\n", err)
		}
	}
	fmt.Fprintf(w, "\nRolling back the fix applied at %s\nProposed rollback: %s — %s\n  reason: %s\n",
		rec.Time, a.Target, a.Summary, a.Reason)
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
	if dryRun {
		fmt.Fprintln(w, "  (dry-run: not applied)")
		logAudit("dry-run", "")
		return nil
	}
	if !assumeYes {
		fmt.Fprint(w, "  Roll back? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.ToLower(strings.TrimSpace(line)) != "y" {
			fmt.Fprintln(w, "  skipped.")
			logAudit("declined", "")
			return nil
		}
	}
	res := remediate.Apply(ctx, client, a)
	switch {
	case res.Err != nil:
		fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		logAudit("error", res.Err.Error())
	case res.Applied:
		fmt.Fprintf(w, "  rolled back: %s\n", res.Detail)
		logAudit("rollback", res.Detail)
	case res.PreflightDenied:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("preflight", res.Detail)
	default:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit("refused", res.Detail)
	}
	return nil
}
```

At the fix callsite in `runScan`, extend the existing `if *fix { … }` block so rollback shares the same audit-writer opening. Replace it with:

```go
	if *fix || *rollback {
		var auditw *audit.Writer
		if *auditLog != "" {
			f, err := os.OpenFile(*auditLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("opening audit log %q: %w", *auditLog, err)
			}
			defer f.Close()
			auditw = audit.NewWriter(f)
		}
		if *rollback {
			if err := runRollback(context.Background(), client, *auditLog, *dryRun, *assumeYes, os.Stdout, os.Stdin, auditw); err != nil {
				return err
			}
		} else {
			runFixes(context.Background(), client, fixPlan, *dryRun, *assumeYes, os.Stdout, os.Stdin, auditw)
		}
	}
```

(The plan computation `fixPlan` stays gated on `*fix` as it is today — rollback does not plan.)

- [ ] **Step 5: Build, test, smoke**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/...`
Expected: PASS (including golden).

```bash
cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build -o kubeagent .
./kubeagent scan --rollback 2>&1 | head -1          # expect the --audit-log requirement error
./kubeagent scan --help 2>&1 | grep -i rollback     # flag documented
rm -f kubeagent
```

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add main.go main_test.go
git commit -m "feat: --rollback undoes the last applied fix through the same guard rails"
```

---

### Task 4: Chaos scenario — apply then roll back

**Files:**
- Modify: `chaos/run.sh`

**Interfaces:** none (shell harness).

Scenario 9 (`scenario_9_rollout`) creates namespace `chaos-rollout`, applies `chaos/manifests/app.yaml`, sets a bad image, sleeps, then records the scan verdict. This task extends it with a live apply-then-rollback check using an audit log in a temp file.

- [ ] **Step 1: Extend the scenario**

In `chaos/run.sh`, inside `scenario_9_rollout`, after the existing `record` line and **before** the namespace deletion, add:

```bash
  # slice-4: apply the fix with an audit log, then roll it back and confirm the image returns
  local alog; alog="$(mktemp)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --fix --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_fix; after_fix="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  ./kubeagent scan --context "$CTX" -n chaos-rollout --rollback --yes --audit-log "$alog" >/dev/null 2>&1 || true
  local after_rollback; after_rollback="$(kubectl --context "$CTX" -n chaos-rollout get deploy web -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  {
    echo "after --fix:      $after_fix"
    echo "after --rollback: $after_rollback"
    grep -c '"disposition":"rollback"' "$alog" 2>/dev/null | sed 's/^/rollback audit records: /'
  } | record "9b. Fix then rollback (audit-log round trip)" "rollback restores the pre-fix image"
  rm -f "$alog"
```

- [ ] **Step 2: Verify the script parses**

Run: `cd /home/ubuntu/git/kubeagent && bash -n chaos/run.sh`
Expected: no output (syntax OK).

- [ ] **Step 3: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add chaos/run.sh
git commit -m "test(chaos): exercise fix-then-rollback in the faulty-rollout scenario"
```

---

### Task 5: Docs

**Files:**
- Modify: `website/docs/features/remediation.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: remediation.md** — add a `### Rollback (`--rollback`)` section: `kubeagent scan --rollback --audit-log /var/log/kubeagent-fix.log` reads the most recent `applied` record and proposes its deterministic inverse (`RolloutUndo` → roll forward to the pre-fix revision; `Uncordon` → re-cordon), which runs through the same preview diff, `[y/N]`, drift bond and RBAC preflight as any fix, and is recorded with a new `rollback` disposition. Show a sample proposal block with a `will change: revision: 4 → 5` line. Note: one action per invocation (re-run to walk further back); it refuses if the cluster moved since the fix; audit records written before v0.54 lack the structured revisions and are refused with a clear message; `--rollback` requires `--audit-log` and is mutually exclusive with `--fix`. Add `rollback` to the disposition table.

- [ ] **Step 2: README.md** — extend the `--fix` paragraph: "and `--rollback` undoes the most recent applied fix (read from the audit log) through the same guard rails".

- [ ] **Step 3: CHANGELOG.md** — under `## [Unreleased]` → `### Added`:

```markdown
- **`--fix` rollback.** `kubeagent scan --rollback --audit-log <path>` reads the most
  recent applied remediation from the audit log and undoes it — rolling a Deployment
  forward to its pre-fix revision, or re-cordoning a node — through the same guard rails
  as any fix: curated preview diff, `[y/N]` confirmation, drift bond (refuses if the
  cluster moved since), RBAC preflight, and an audit record with the new `rollback`
  disposition. The inverse is derived deterministically from structured
  `fromRevision`/`toRevision` fields now written into every audit record; records
  written before v0.54 are refused with a clear message rather than guessed at. One
  action per invocation; `--rollback` requires `--audit-log` and cannot be combined
  with `--fix`.
```

- [ ] **Step 4: roadmap.md** — under Theme D's "Shipped" list add: `--fix` rollback (`--rollback`, undo the last applied fix from the audit log through every guard rail) — the fourth and final write-path hardening slice, completing Theme D. If the roadmap marks themes as in-progress/complete, mark Theme D complete.

- [ ] **Step 5: Verify the site builds**

Run: `cd /home/ubuntu/git/kubeagent/website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -1` (fall back to `mkdocs` on PATH)
Expected: `Documentation built`, no page WARNINGs.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/remediation.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document --fix rollback"
```

---

## Release (after all tasks + whole-branch review)

- **Gate: FULL CHAOS GATE** — new write paths. `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate` (backgrounded, ~10 min); every scenario green including the new 9b fix-then-rollback check.
- **Version:** minor **v0.53.0 → v0.54.0**.
- **Chart: PATCH** — no RBAC/Helm/template change (same `update` verbs).

## Self-Review notes (author)

- **Spec coverage:** structured revisions + `ReadLast` (Task 1); `Inverse`, both inverse kinds with drift bond + preflight + protected-ns, and the **`Preflight` switch extension** (Task 2); `--rollback` flag, preconditions, `runRollback`, `rollback` disposition, shared audit writer (Task 3); chaos round-trip (Task 4); docs (Task 5); gate/version/chart (Release).
- **Import cycle avoided:** `Inverse` takes plain values; only `main.go` bridges `audit.Record` → `remediate.Inverse`.
- **Type consistency:** `Inverse(kind, namespace, name string, fromRevision, toRevision int) (Action, error)` used identically in Tasks 2 and 3; `Record.FromRevision`/`ToRevision` produced in Task 1, consumed in Task 3; `runRollback`'s signature matches its test calls.
- **Known trap flagged:** `Preflight`'s unknown-kind default would break every rollback — Task 2 Step 4 extends it, and `TestPreflight_KnowsInverseKinds` guards it.
