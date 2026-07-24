# `--fix` RBAC Preflight Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before each guarded `--fix` write, a `SelfSubjectAccessReview` confirms the operator may perform it; a denial refuses cleanly up front (new `preflight` audit disposition) and an SSAR API error fails closed, instead of a mid-apply 403.

**Architecture:** `remediate.Preflight` runs an SSAR for an Action's implied write (`verb=update` on its resource/namespace/name). `Apply` calls it as the final gate before each write; the dry-run path calls the same function to report would-I-be-allowed. `Result.PreflightDenied` drives the new disposition in `runFixes`. `internal/audit` is unchanged (the new disposition is just a string).

**Tech Stack:** Go 1.26, `k8s.io/api/authorization/v1` (SSAR) + client-go (already in the module; fake clientset + reactors in tests). No new dependency.

## Global Constraints

- **Writes only get stricter.** Preflight can only *prevent* a write (deny or SSAR-error → no write); it never enables one. All prior guard rails unchanged (allowlist, protected namespaces, `[y/N]`, `--dry-run`/`--yes`, slice-1 drift bond, slice-2 audit).
- **Fail closed.** An SSAR API error = "could not confirm permission" → no write, surfaced as `error`.
- **No secrets in output/audit.** The preflight reason names only verb/resource/namespace.
- **No new dependency. No RBAC/Helm change** (chart PATCH — SSAR is self-review, granted to all authenticated users via `system:basic-user`). **Golden snapshot unchanged** (no report change).
- **No `Co-Authored-By: Claude` trailer.** **TDD.** gofmt-clean. `go build ./... && go test ./...` before every commit.

## File Structure

- `internal/remediate/remediate.go` — `Preflight`, the attribute mapping, `Result.PreflightDenied`, the gate call in both `apply*` funcs.
- `internal/remediate/remediate_test.go` — `Preflight` unit tests, Apply-integration tests, an `allowFix` reactor helper, allow-reactor added to existing successful-apply tests.
- `main.go` — the `preflight` disposition case + dry-run permission report.
- `main_test.go` — allow-reactor on existing successful-apply tests; new preflight + dry-run-report tests.
- Docs: `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

**Gate ordering (spec-mandated):** in each `apply*`, the preflight runs **after** the state preconditions (drift bond / no-target / uncordon precondition) and **immediately before** the write. So an already-drifted action reports the drift `refused`, not `preflight` — permission is the last gate.

**Fixture consequence (call this out — like slice 1):** once the gate is in `Apply` (Task 2), the fake clientset's default SSAR returns `Allowed:false`, so every existing test that expects a successful write must install an allow-SSAR reactor. The affected tests are enumerated in Tasks 2 and 3. Tests that refuse/error *before* the gate (drift, no-target, protected-ns, uncordon-precondition, unknown-kind) need no reactor.

---

### Task 1: `remediate.Preflight` + `Result.PreflightDenied` (no gate yet)

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Produces (Tasks 2–3 rely on these):
  - `func Preflight(ctx context.Context, client kubernetes.Interface, a Action) (allowed bool, reason string, err error)`
  - `Result.PreflightDenied bool`
  - test helper `allowFix(cli *fake.Clientset)` (installs an allow-SSAR reactor)

This task adds the function and field only — it does NOT wire the gate into `apply*` (that is Task 2), so all existing Apply tests keep passing untouched.

- [ ] **Step 1: Add SSAR reactor imports to the test file**

In `internal/remediate/remediate_test.go`, add to the import block:

```go
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/remediate/remediate_test.go`:

```go
// allowFix makes the fake clientset's SelfSubjectAccessReview return Allowed:true, so
// a write-path test can reach the actual write. Refusal/drift tests short-circuit
// before the preflight and do not need it.
func allowFix(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true},
		}, nil
	})
}

func denyFix(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{
			Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false},
		}, nil
	})
}

func TestPreflight_RolloutUndoBuildsDeploymentAttributes(t *testing.T) {
	cli := fake.NewSimpleClientset()
	var got *authorizationv1.SelfSubjectAccessReview
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		got = a.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	allowed, reason, err := Preflight(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if err != nil || !allowed || reason != "" {
		t.Fatalf("allowed preflight: got (%v,%q,%v)", allowed, reason, err)
	}
	ra := got.Spec.ResourceAttributes
	if ra == nil || ra.Verb != "update" || ra.Group != "apps" || ra.Resource != "deployments" || ra.Namespace != "shop" || ra.Name != "web" {
		t.Errorf("attributes = %+v", ra)
	}
}

func TestPreflight_UncordonBuildsNodeAttributes(t *testing.T) {
	cli := fake.NewSimpleClientset()
	var got *authorizationv1.SelfSubjectAccessReview
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		got = a.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	if _, _, err := Preflight(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"}); err != nil {
		t.Fatal(err)
	}
	ra := got.Spec.ResourceAttributes
	if ra.Verb != "update" || ra.Group != "" || ra.Resource != "nodes" || ra.Namespace != "" || ra.Name != "worker-1" {
		t.Errorf("node attributes = %+v", ra)
	}
}

func TestPreflight_DeniedReturnsReason(t *testing.T) {
	cli := fake.NewSimpleClientset()
	denyFix(cli)
	allowed, reason, err := Preflight(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if allowed || err != nil {
		t.Fatalf("want denied without error, got (%v,%v)", allowed, err)
	}
	if !strings.Contains(reason, "permission to update deployments") || !strings.Contains(reason, "shop") {
		t.Errorf("reason = %q", reason)
	}
}

func TestPreflight_APIErrorSurfaces(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	if _, _, err := Preflight(context.Background(), cli, Action{Kind: "Uncordon", Name: "n1"}); err == nil {
		t.Error("expected the SSAR API error to surface")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -run TestPreflight`
Expected: FAIL — `undefined: Preflight`.

- [ ] **Step 4: Implement `Preflight` + the field**

In `internal/remediate/remediate.go`, add the import `authorizationv1 "k8s.io/api/authorization/v1"`. Add to the `Result` struct after `Refused bool`:

```go
	PreflightDenied bool // the RBAC preflight refused this action; Applied false, Err nil, no write
```

Add the function (place it near `Apply`):

```go
// Preflight asks the API server whether the current credentials may perform the write
// this Action implies (verb=update on its resource/namespace/name) via a
// SelfSubjectAccessReview. Returns (allowed, humanReason, err): err != nil means the
// SSAR call itself failed (callers fail closed and do not write); allowed==false means
// not permitted and reason explains it in plain language.
func Preflight(ctx context.Context, client kubernetes.Interface, a Action) (bool, string, error) {
	var group, resource, ns string
	switch a.Kind {
	case "RolloutUndo":
		group, resource, ns = "apps", "deployments", a.Namespace
	case "Uncordon":
		group, resource, ns = "", "nodes", ""
	default:
		return false, "", fmt.Errorf("unknown action kind %q", a.Kind)
	}
	ssar := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: "update", Group: group, Resource: resource, Namespace: ns, Name: a.Name,
			},
		},
	}
	resp, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	if resp.Status.Allowed {
		return true, "", nil
	}
	if ns == "" {
		return false, fmt.Sprintf("you lack permission to update %s (RBAC)", resource), nil
	}
	return false, fmt.Sprintf("you lack permission to update %s in namespace %q (RBAC)", resource, ns), nil
}
```

- [ ] **Step 5: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS — the four new Preflight tests pass; all existing Apply/Plan tests still pass (the gate is not wired yet).

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): SelfSubjectAccessReview preflight and PreflightDenied result"
```

---

### Task 2: Wire the preflight gate into `Apply`

**Files:**
- Modify: `internal/remediate/remediate.go` (`applyRolloutUndo`, `applyUncordon`)
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: `Preflight`, `Result.PreflightDenied`, `allowFix`/`denyFix` (Task 1).

This task inserts the gate and updates the existing successful-apply tests to grant permission.

- [ ] **Step 1: Write the failing integration tests**

Append to `internal/remediate/remediate_test.go`:

```go
func TestApply_PreflightDeniedMakesNoWrite(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	denyFix(cli)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1,
	})
	if res.Applied || res.Err != nil || !res.PreflightDenied {
		t.Fatalf("denied preflight: got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("preflight denial must make no write")
		}
	}
}

func TestApply_PreflightErrorFailsClosed(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1,
	})
	if res.Applied || res.Err == nil {
		t.Fatalf("SSAR error must fail closed with an error, got %+v", res)
	}
	for _, act := range cli.Actions() {
		if act.GetVerb() == "update" {
			t.Fatal("fail-closed must make no write")
		}
	}
}

func TestApply_DriftShortCircuitsBeforePreflight(t *testing.T) {
	// Drift (current rev 3, previewed 2) must refuse BEFORE preflight — even with a deny
	// reactor installed, the disposition is the drift refusal, not preflight.
	cur := depObj("shop", "web", "nginx:still-broken", "3")
	r1 := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:broken")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:still-broken")
	cli := fake.NewSimpleClientset(cur, &r1, &r2, &r3)
	denyFix(cli)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1,
	})
	if res.PreflightDenied {
		t.Fatal("drift must short-circuit before the preflight gate")
	}
	if !res.Refused {
		t.Fatalf("expected the drift refusal, got %+v", res)
	}
}
```

- [ ] **Step 2: Grant permission in the existing successful-apply tests**

Add `allowFix(cli)` immediately after the `cli := fake.NewSimpleClientset(...)` line in these three tests (they expect `Applied == true`): `TestApply_RollsBackToPreviousTemplate`, `TestApply_MatchingPreviewApplies`, `TestApply_Uncordon`.

- [ ] **Step 3: Run to verify the new + existing tests fail**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -run TestApply`
Expected: FAIL — the new denial/error tests fail (no gate yet, so they'd write/apply), and the three allow-reactored tests still pass (gate absent). This confirms the gate is missing.

- [ ] **Step 4: Insert the gate in both apply funcs**

In `applyRolloutUndo`, after the revision-drift bond block and **before** the `tpl := *target.Spec.Template.DeepCopy()` line:

```go
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
```

In `applyUncordon`, after the precondition block and **before** the `n.Spec.Unschedulable = false` line, insert the identical gate.

- [ ] **Step 5: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS — new integration tests pass; the three allow-reactored tests apply; all refusal/drift/protected-ns tests still pass (they short-circuit before the gate).

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): gate each write behind the RBAC preflight"
```

---

### Task 3: `preflight` disposition + dry-run permission report (`main.go`)

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `remediate.Preflight`, `remediate.Result.PreflightDenied` (Tasks 1–2).

`runFixes` already has `client`; no signature change.

- [ ] **Step 1: Write the failing tests**

Add to `main_test.go` (it already imports `authorizationv1`? No — add `authorizationv1 "k8s.io/api/authorization/v1"` to its imports; `ktesting`, `runtime`, `errors`, `fake` are already imported). Add local reactor helpers and tests:

```go
func allowFixM(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
}
func denyFixM(cli *fake.Clientset) {
	cli.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: false}}, nil
	})
}

func TestRunFixes_AuditRecordsPreflight(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	denyFixM(cli) // permitted to reach the gate, then denied
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "preflight" {
		t.Fatalf("want one preflight record, got %+v", recs)
	}
	if !strings.Contains(out.String(), "no write attempted") {
		t.Errorf("expected the preflight skip line, got: %s", out.String())
	}
}

func TestRunFixes_DryRunReportsPermissionAllowed(t *testing.T) {
	cli := fake.NewSimpleClientset()
	allowFixM(cli)
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	if !strings.Contains(out.String(), "you have permission") {
		t.Errorf("dry-run should report permission, got: %s", out.String())
	}
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("dry-run disposition expected, got %+v", recs)
	}
}

func TestRunFixes_DryRunReportsPermissionDenied(t *testing.T) {
	cli := fake.NewSimpleClientset()
	denyFixM(cli)
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), cli, actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	if !strings.Contains(out.String(), "would be blocked") {
		t.Errorf("dry-run should report the block, got: %s", out.String())
	}
}
```

- [ ] **Step 2: Grant permission in the existing successful-apply main tests**

Add `allowFixM(cli)` immediately after the `cli := fake.NewSimpleClientset(...)` line in the tests that reach a real apply: `TestRunFixes_YesApplies`, `TestRunFixes_UncordonYesApplies`, `TestRunFixes_AuditRecordsApplied`. In `TestRunFixes_AuditRecordsError` add `allowFixM(cli)` too (the preflight must pass so the update-fail reactor is what produces the error) — install `allowFixM(cli)` **before** its update-error reactor (both are PrependReactors on different resources and compose).

- [ ] **Step 3: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test . -run TestRunFixes`
Expected: FAIL — the new preflight/dry-run-report tests fail (no `preflight` case, dry-run doesn't report yet), and (once the gate from Task 2 is present) the successful-apply tests would fail without their allow reactor — confirming the wiring is needed.

- [ ] **Step 4: Add the `preflight` disposition case**

In `runFixes`, extend the apply switch (insert the case before `default`):

```go
		case res.PreflightDenied:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "preflight", res.Detail)
```

- [ ] **Step 5: Make the dry-run branch report permission**

Replace the current dry-run block:

```go
		if dryRun {
			allowed, reason, err := remediate.Preflight(ctx, client, a)
			switch {
			case err != nil:
				fmt.Fprintf(w, "  (dry-run: not applied; permission check errored: %v)\n", err)
				logAudit(a, "dry-run", "permission check errored: "+err.Error())
			case allowed:
				fmt.Fprintln(w, "  (dry-run: not applied; you have permission to apply this)")
				logAudit(a, "dry-run", "permission: allowed")
			default:
				fmt.Fprintf(w, "  (dry-run: not applied; would be blocked — %s)\n", reason)
				logAudit(a, "dry-run", reason)
			}
			continue
		}
```

- [ ] **Step 6: Build, test, smoke**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/...`
Expected: PASS (including golden — no report change).

Binary smoke (no cluster needed for the help line):

```bash
cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build -o kubeagent . && ./kubeagent scan --help 2>&1 | grep -i fix | head -1 && rm -f kubeagent
```

- [ ] **Step 7: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add main.go main_test.go
git commit -m "feat: --fix RBAC preflight disposition and dry-run permission report"
```

---

### Task 4: Docs

**Files:**
- Modify: `website/docs/features/remediation.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: remediation.md** — add an `### RBAC preflight` subsection: before each write, kubeagent runs a `SelfSubjectAccessReview` to confirm the operator may perform it; a denial refuses up front (`skipped: you lack permission to update deployments in namespace "shop" (RBAC); no write attempted`) and is recorded with the new `preflight` disposition; under `--dry-run` the check runs read-only and reports (`you have permission` / `would be blocked — …`); an SSAR API failure fails closed (`error`, no write). Note it needs no extra RBAC (self-review is granted to all authenticated users). Update the disposition list to include `preflight`. Match the page's tone.

- [ ] **Step 2: README.md** — extend the `--fix` mention: "checks with a `SelfSubjectAccessReview` that you're permitted to make each change before attempting it, refusing cleanly if not".

- [ ] **Step 3: CHANGELOG.md** — under `## [Unreleased]` → `### Added`:

```markdown
- **`--fix` RBAC preflight.** Before each guarded write, kubeagent runs a
  `SelfSubjectAccessReview` to confirm the current credentials may perform it
  (`update` on the target deployment/node). A denial refuses up front — `skipped: you
  lack permission to update deployments in namespace "shop" (RBAC); no write attempted`
  — recorded with a new `preflight` audit disposition, instead of a mid-apply 403. An
  SSAR API failure fails closed (no write, `error`). Under `--dry-run` the check runs
  read-only and reports whether each fix would be permitted. Needs no extra RBAC —
  self-review is granted to all authenticated users.
```

- [ ] **Step 4: roadmap.md** — under Theme D's "Shipped" list, add: `--fix` RBAC preflight (`SelfSubjectAccessReview` before each write; clean up-front refusal, `preflight` disposition, dry-run permission report) — the third write-path hardening slice.

- [ ] **Step 5: Verify the site builds**

Run: `cd /home/ubuntu/git/kubeagent/website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -1` (fall back to `mkdocs` on PATH if missing)
Expected: `Documentation built`, no page WARNINGs.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/remediation.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document the --fix RBAC preflight"
```

---

## Release (after all tasks + whole-branch review)

- **Gate: FULL CHAOS GATE** — touches the `--fix` write path. `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate` (backgrounded, ~7 min); every scenario green (the fix scenarios run under a normal kubeconfig, where self-review is allowed, and apply as before).
- **Version:** minor **v0.52.0 → v0.53.0**.
- **Chart: PATCH** — no RBAC/Helm/template change (SSAR is self-review).

## Self-Review notes (author)

- **Spec coverage:** Preflight + attribute mapping + PreflightDenied (Task 1), gate-in-Apply + fail-closed + drift-before-preflight ordering + fixture updates (Task 2), preflight disposition + dry-run report + main fixture updates (Task 3), docs (Task 4), chaos/version/chart (Release). The "no RBAC change" claim is stated in Global Constraints and the Release section.
- **Type consistency:** `Preflight(ctx, client, a Action) (bool, string, error)` used identically in Tasks 2 (Apply gate) and 3 (dry-run); `Result.PreflightDenied` produced in Task 1, consumed in Tasks 2 (set) and 3 (switch case); `allowFix`/`denyFix` in remediate_test, `allowFixM`/`denyFixM` in main_test (separate because they build the same reactor in two packages).
- **Fixture consequence enumerated** in Tasks 2 and 3 (exact test names); refusal/drift tests correctly excluded (they short-circuit before the gate).
- **Ordering invariant tested** explicitly (`TestApply_DriftShortCircuitsBeforePreflight`).
