# Stuck-rollout detector (`RolloutStuck`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a Deployment whose rollout has wedged — a `ReplicaFailure` condition or a `Progressing=False/ProgressDeadlineExceeded` condition — naming it as a `RolloutStuck` finding when no pod-level detector already explains the failure.

**Architecture:** A new pure, read-only workload-level annotator (`internal/rollouthealth`) mirroring `internal/createhealth`: it reads the assembled+prioritized workloads and the collected Deployments, and appends one `RolloutStuck` finding to each flagged Deployment that has no existing finding and whose `status.conditions` show a stuck rollout. Wired into `scan.Evaluate` immediately after `createhealth.Annotate`. The new Issue is integrated into `remediation` (a `describe deployment` next step) and pinned `high` in `confidence`.

**Tech Stack:** Go 1.26, standard library + `k8s.io/api/apps/v1` and `k8s.io/api/core/v1`; tests are pure fake-object unit tests plus one fake-clientset integration test.

## Global Constraints

- **Read-only; always-on; no flag.** No new collector, RBAC, watch gauge, `Result` field, or report change. Deployments are already collected (`inputs.Deployments`).
- **Pure & deterministic** — the detector reads only the passed workloads and Deployments; no clock, no cluster calls, no model.
- **Confidence:** `RolloutStuck` is a direct Kubernetes-asserted state → `high` automatically (not in the `confidence` `medium` set); no change to the `confidence` package itself, only a test pin.
- **Zero false positives** — gated on `Flagged()` + `Kind == "Deployment"` + `len(Findings) == 0` + a specific failure condition. Paused (`reason=DeploymentPaused`), progressing-within-deadline (`status=True`), and healthy Deployments are never flagged.
- **Precedence:** when both conditions are present, `ReplicaFailure` (status True) wins over `Progressing`/`ProgressDeadlineExceeded`. One finding per workload.
- **Issue string** is exactly `RolloutStuck`. **Reason** is exactly `the Deployment's rollout cannot complete — the new pods are not becoming available` (the dash is an em dash, U+2014). **Evidence** is `Progressing (ProgressDeadlineExceeded): <message>` or `ReplicaFailure: <message>`.
- `confidence`, `inventory`, `clusterhealth`, `rootcause`, the `rollout` annotator, `explain.go`, `--fix`, the watch daemon stay **unchanged**.
- **No `Co-Authored-By: Claude` trailer** on any commit — every commit authored solely by the human. **TDD.** gofmt-clean.
- Gate: **LIGHTWEIGHT SMOKE**. **Minor** bump v0.42.0 → **v0.43.0**; **patch** chart bump.

---

### Task 1: `rollouthealth` package (the detector)

**Files:**
- Create: `internal/rollouthealth/rollouthealth.go`
- Test: `internal/rollouthealth/rollouthealth_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (fields `Namespace`, `Name`, `Kind`, `Desired`, `Ready`, `Status`, `Findings`; method `Flagged() bool`), `diagnose.Finding` (fields `Pod`, `Issue`, `Reason`, `Evidence`), `appsv1.Deployment` (`.Status.Conditions []appsv1.DeploymentCondition`).
- Produces: `func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment)` — mutates `workloads` in place, appending a `RolloutStuck` finding. Used by Task 2 (`scan.Evaluate`).

- [ ] **Step 1: Write the failing test**

Create `internal/rollouthealth/rollouthealth_test.go`:

```go
package rollouthealth

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// cond builds a Deployment status condition.
func cond(t appsv1.DeploymentConditionType, s corev1.ConditionStatus, reason, msg string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{Type: t, Status: s, Reason: reason, Message: msg}
}

// deploy builds a Deployment with the given status conditions.
func deploy(ns, name string, conds ...appsv1.DeploymentCondition) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     appsv1.DeploymentStatus{Conditions: conds},
	}
}

// degraded builds a flagged (Ready < Desired) Deployment workload with no findings.
func degraded(ns, name string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 3, Ready: 2, Status: "Degraded"}
}

const deadlineMsg = `ReplicaSet "api-7f9c" has timed out progressing.`
const replicaFailMsg = `pods "api-7f9c-" is forbidden: exceeded quota: compute`

func TestAnnotate_ProgressDeadlineExceeded(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	f := ws[0].Findings[0]
	if f.Issue != "RolloutStuck" {
		t.Errorf("Issue = %q, want RolloutStuck", f.Issue)
	}
	if f.Reason != "the Deployment's rollout cannot complete — the new pods are not becoming available" {
		t.Errorf("Reason = %q", f.Reason)
	}
	if !strings.HasPrefix(f.Evidence, "Progressing (ProgressDeadlineExceeded): ") || !strings.Contains(f.Evidence, deadlineMsg) {
		t.Errorf("Evidence = %q, want the Progressing-prefixed message", f.Evidence)
	}
	if f.Pod != "shop/api" {
		t.Errorf("Pod = %q, want shop/api", f.Pod)
	}
}

func TestAnnotate_ReplicaFailure(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", replicaFailMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "RolloutStuck" {
		t.Fatalf("want one RolloutStuck finding, got %+v", ws[0].Findings)
	}
	if ev := ws[0].Findings[0].Evidence; !strings.HasPrefix(ev, "ReplicaFailure: ") || !strings.Contains(ev, replicaFailMsg) {
		t.Errorf("Evidence = %q, want the ReplicaFailure-prefixed message", ev)
	}
}

func TestAnnotate_ReplicaFailureWinsOverProgressing(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg),
		cond(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", replicaFailMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 {
		t.Fatalf("want one finding, got %+v", ws[0].Findings)
	}
	if ev := ws[0].Findings[0].Evidence; !strings.HasPrefix(ev, "ReplicaFailure: ") {
		t.Errorf("Evidence = %q, want ReplicaFailure to win precedence", ev)
	}
}

func TestAnnotate_NotFlaggedCases(t *testing.T) {
	cases := []struct {
		name string
		w    inventory.Workload
		d    appsv1.Deployment
	}{
		{"paused", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentProgressing, corev1.ConditionUnknown, "DeploymentPaused", "Deployment is paused"))},
		{"progressing within deadline", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", "ReplicaSet is progressing"))},
		{"healthy available", degraded("shop", "api"),
			deploy("shop", "api", cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, "MinimumReplicasAvailable", "ok"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := []inventory.Workload{tc.w}
			Annotate(ws, []appsv1.Deployment{tc.d})
			if len(ws[0].Findings) != 0 {
				t.Errorf("%s: want no finding, got %+v", tc.name, ws[0].Findings)
			}
		})
	}
}

func TestAnnotate_SkipsWorkloadWithExistingFinding(t *testing.T) {
	w := degraded("shop", "api")
	w.Findings = []diagnose.Finding{{Pod: "shop/api", Issue: "ImagePullBackOff"}}
	ws := []inventory.Workload{w}
	ds := []appsv1.Deployment{deploy("shop", "api",
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg))}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 1 || ws[0].Findings[0].Issue != "ImagePullBackOff" {
		t.Errorf("want the existing finding untouched, got %+v", ws[0].Findings)
	}
}

func TestAnnotate_SkipsNonDeploymentAndUnflagged(t *testing.T) {
	// A flagged StatefulSet named like a stuck Deployment: the Kind gate must skip it
	// even though a same-named Deployment with the stuck condition is present.
	sts := inventory.Workload{Namespace: "db", Name: "pg", Kind: "StatefulSet", Desired: 3, Ready: 0, Status: "Degraded"}
	// An unflagged Deployment (Ready == Desired) must be skipped.
	healthy := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 3, Ready: 3, Status: "Running"}
	ws := []inventory.Workload{sts, healthy}
	ds := []appsv1.Deployment{
		deploy("db", "pg", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
		deploy("shop", "web", cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", deadlineMsg)),
	}

	Annotate(ws, ds)

	if len(ws[0].Findings) != 0 {
		t.Errorf("StatefulSet: want no finding, got %+v", ws[0].Findings)
	}
	if len(ws[1].Findings) != 0 {
		t.Errorf("unflagged Deployment: want no finding, got %+v", ws[1].Findings)
	}
}

func TestAnnotate_NoMatchingDeployment(t *testing.T) {
	ws := []inventory.Workload{degraded("shop", "api")}
	Annotate(ws, nil) // no Deployments at all — must not panic, no finding
	if len(ws[0].Findings) != 0 {
		t.Errorf("want no finding when the Deployment is absent, got %+v", ws[0].Findings)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rollouthealth/ 2>&1 | head`
Expected: build/compile failure — `undefined: Annotate` (the package file does not exist yet).

- [ ] **Step 3: Write the implementation**

Create `internal/rollouthealth/rollouthealth.go`:

```go
// Package rollouthealth attaches a "RolloutStuck" finding to a flagged Deployment
// whose rollout has wedged — the new ReplicaSet's pods are not becoming available,
// so the Deployment's status carries a ReplicaFailure condition or a
// Progressing=False/ProgressDeadlineExceeded condition. Pure and read-only: the
// caller supplies the assembled+prioritized workloads and the Deployments (for
// their status conditions). Mirrors createhealth.Annotate.
package rollouthealth

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate appends a "RolloutStuck" finding to each flagged Deployment workload
// that has no existing finding and whose Deployment status shows a stuck rollout.
// It mutates the slice elements in place. Runs after createhealth.Annotate so a
// lingering FailedCreate event wins the "no existing finding" gate.
func Annotate(workloads []inventory.Workload, deployments []appsv1.Deployment) {
	byName := make(map[string]*appsv1.Deployment, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		byName[d.Namespace+"/"+d.Name] = d
	}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.Kind != "Deployment" || len(w.Findings) > 0 {
			continue
		}
		dep, ok := byName[w.Namespace+"/"+w.Name]
		if !ok {
			continue
		}
		if ev, stuck := stuckCondition(dep); stuck {
			w.Findings = append(w.Findings, diagnose.Finding{
				Pod:      w.Namespace + "/" + w.Name,
				Issue:    "RolloutStuck",
				Reason:   "the Deployment's rollout cannot complete — the new pods are not becoming available",
				Evidence: ev,
			})
		}
	}
}

// stuckCondition returns the evidence string and true when the Deployment's
// status shows a wedged rollout. ReplicaFailure (the concrete pod-creation
// blocker) takes precedence over Progressing/ProgressDeadlineExceeded.
func stuckCondition(dep *appsv1.Deployment) (string, bool) {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return fmt.Sprintf("ReplicaFailure: %s", c.Message), true
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
			return fmt.Sprintf("Progressing (ProgressDeadlineExceeded): %s", c.Message), true
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rollouthealth/ -v 2>&1 | tail -20`
Expected: all tests PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/rollouthealth/
git add internal/rollouthealth/
git commit -m "feat(rollouthealth): flag a wedged Deployment rollout as RolloutStuck"
```

---

### Task 2: Wire the annotator into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go` (import + one call after `createhealth.Annotate`, near line 241–243)
- Test: `internal/scan/scan_test.go` (add one integration test)

**Interfaces:**
- Consumes: `rollouthealth.Annotate(result.Workloads, inputs.Deployments)` from Task 1; the existing `scan.Evaluate(ctx, client, Options)` returning a `Result` whose `Inventory.Workloads []inventory.Workload` carry findings. `inputs.Deployments` (`[]appsv1.Deployment`) is already collected.
- Produces: end-to-end `RolloutStuck` detection in `scan` output.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (the helpers `p32`, `boolp`, and imports `appsv1`, `corev1`, `metav1`, `fake` already exist in that file):

```go
func TestEvaluate_FlagsStuckRollout(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(3)},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
				Message: `ReplicaSet "api-7f9c" has timed out progressing.`},
		}},
	}
	cli := fake.NewSimpleClientset(node, dep)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Issue == "RolloutStuck" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a RolloutStuck finding, got %+v", res.Inventory.Workloads)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_FlagsStuckRollout -v 2>&1 | tail`
Expected: FAIL — `expected a RolloutStuck finding` (the annotator is not wired yet).

- [ ] **Step 3: Wire the annotator**

In `internal/scan/scan.go`, add the import in the existing import block (alphabetical, next to `createhealth`):

```go
	"github.com/imantaba/kubeagent/internal/rollouthealth"
```

Immediately after the existing `createhealth.Annotate(result.Workloads, inputs.ReplicaSets, failedCreateEvents)` line (≈ line 241), add:

```go
	rollouthealth.Annotate(result.Workloads, inputs.Deployments)
```

(It must come **after** `createhealth.Annotate` so a lingering `FailedCreate` event wins the "no existing finding" gate.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ 2>&1 | tail -5`
Expected: PASS (the new test and all existing `scan` tests).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run the RolloutStuck annotator after createhealth"
```

---

### Task 3: Integrate the new Issue into `remediation` and `confidence`

**Files:**
- Modify: `internal/remediation/remediation.go` (a `RolloutStuck` case + a `describeDeployCmd` helper)
- Modify: `internal/remediation/remediation_test.go` (table case + never-mutating guard entry)
- Modify: `internal/confidence/confidence_test.go` (the `high` classification pin)

**Interfaces:**
- Consumes: `diagnose.Finding` with `Issue == "RolloutStuck"` and `Pod == "ns/deployment-name"`; the existing `remediation.For(f diagnose.Finding) Suggestion` and its `splitPod`/`describeCmd` helpers; `confidence.ForIssue(issue string) string`.
- Produces: `remediation.For` returns a `describe deployment` command for `RolloutStuck`; `confidence.ForIssue("RolloutStuck") == "high"` is pinned.

- [ ] **Step 1: Write the failing tests**

In `internal/remediation/remediation_test.go`, add a case to the `cases` table in `TestFor_TableAndCommands` (after the `JobFailed` row):

```go
		{"RolloutStuck", "", "the rollout is wedged", "kubectl -n shop describe deployment web-abc"},
```

and add `"RolloutStuck"` to the `issues` slice in `TestFor_CommandsAreNeverMutating`:

```go
		"Init:ImagePullBackOff", "FailedCreate", "JobFailed", "RestartLoop", "RolloutStuck", "whatever-default"}
```

In `internal/confidence/confidence_test.go`, add `"RolloutStuck"` to the `high` slice in `TestForIssue`:

```go
		"Init:OOMKilled", "FailedCreate", "JobFailed", "CreateContainerConfigError", "RolloutStuck", "SomeFutureDirectDetector"}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediation/ ./internal/confidence/ 2>&1 | tail`
Expected: `remediation` FAILS (`RolloutStuck` falls to the default → `kubectl -n shop describe pod web-abc`, not `describe deployment`); `confidence` PASSES already (the `SomeFutureDirectDetector` catch-all makes any unknown issue `high`) — the pin just documents/guards it.

- [ ] **Step 3: Add the `RolloutStuck` case + helper**

In `internal/remediation/remediation.go`, add a case immediately before `default:` in the `switch f.Issue`:

```go
	case "RolloutStuck":
		return Suggestion{"the rollout is wedged — inspect the new ReplicaSet's pods and events", describeDeployCmd(ns, pod)}
```

and add the helper next to `describeCmd`:

```go
func describeDeployCmd(ns, deploy string) string {
	return fmt.Sprintf("kubectl -n %s describe deployment %s", ns, deploy)
}
```

(The dash in the next-step string is an em dash, U+2014, matching the other suggestions.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediation/ ./internal/confidence/ 2>&1 | tail`
Expected: both packages PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/remediation/
git add internal/remediation/ internal/confidence/confidence_test.go
git commit -m "feat(remediation): RolloutStuck suggests describe deployment; pin high confidence"
```

---

### Task 4: Golden snapshot — add a `RolloutStuck` fixture workload

**Files:**
- Modify: `internal/report/golden_test.go` (add one workload to the pre-built fixture)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated via `-update`)

**Interfaces:**
- Consumes: the existing `reportsData()` fixture builder returning `[]inventory.Workload` (see the workloads around lines 115–182, ending with the `worker`/`CreateContainerConfigError` workload).
- Produces: a golden snapshot that renders and guards the `RolloutStuck` finding.

- [ ] **Step 1: Add the fixture workload**

In `internal/report/golden_test.go`, in the slice returned by the fixture builder, add this workload immediately after the `worker`/`CreateContainerConfigError` workload (just before the closing `}` of the slice literal, near line 182):

```go
		{Namespace: "shop", Name: "payments", Kind: "Deployment", Desired: 3, Ready: 2, Status: "Degraded",
			Findings: []diagnose.Finding{{Pod: "shop/payments", Issue: "RolloutStuck",
				Reason:   "the Deployment's rollout cannot complete — the new pods are not becoming available",
				Evidence: `Progressing (ProgressDeadlineExceeded): ReplicaSet "payments-7f9c" has timed out progressing.`}}},
```

- [ ] **Step 2: Run the golden test to verify it fails (snapshot mismatch)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput 2>&1 | tail`
Expected: FAIL — the rendered output now contains the `payments` / `RolloutStuck` workload and the `N workloads failing` count differs from the committed snapshot.

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: exit 0; `internal/report/testdata/golden-scan.txt` now shows a `✗ shop/payments  Deployment  2/3 Degraded` block with the `⚠ RolloutStuck:` line and its `↳ Progressing (ProgressDeadlineExceeded): …` evidence, and the failing-workload count incremented by one.

- [ ] **Step 4: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -3`
Expected: PASS (including `TestGoldenScanOutput` and `TestGoldenInputCoversAllSections`).

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a RolloutStuck workload in the golden scan snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- `website/docs/features/diagnostics.md`: add a short section (near the other always-on workload-level checks such as `FailedCreate` / `CreateContainerConfigError`) describing `RolloutStuck` — a **Deployment** whose rollout has wedged (`Progressing=False/ProgressDeadlineExceeded` or a `ReplicaFailure` condition); surfaced only when no pod-level finding already explains the failure; read-only, always-on, no flag. Show an example block:

  ```text
  ✗ shop/api  Deployment  2/3 Degraded
      ⚠ RolloutStuck: the Deployment's rollout cannot complete — the new pods are not becoming available
        ↳ Progressing (ProgressDeadlineExceeded): ReplicaSet "api-7f9c" has timed out progressing.
  ```

  (The `↳` is U+21B3; the dash in "cannot complete —" is an em dash U+2014.) Follow the style of the neighboring always-on detector notes; do not restructure the page.

- `README.md`: add `RolloutStuck` to the diagnostics/detector list (it is **always-on, no flag** — put it with the other detectors, not the flags list). Match the phrasing of the neighboring detector entries.

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add:

  ```
  - **Stuck-rollout detection (`RolloutStuck`).** `scan` flags a Deployment whose
    rollout has wedged — its `Progressing` condition is
    `ProgressDeadlineExceeded`, or it carries a `ReplicaFailure` condition — so
    the new pods are not becoming available. Surfaced only when no pod-level
    finding already explains the failure (zero redundancy). Read-only, always-on;
    no new flag, metric, or RBAC.
  ```

- `website/docs/roadmap.md`: add a bullet to the Shipped list (after the `CreateContainerConfigError` entry), tagged as a **Theme-B** (deeper diagnosis) slice, noting it names a wedged Deployment rollout distinctly from the underlying pod crash and links to `features/diagnostics.md`.

- [ ] **Step 2: Verify the docs build (only `website/` changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (if `mkdocs` is not on `PATH`, use the venv: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no `WARNING` about your pages.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the RolloutStuck stuck-rollout detector"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches `internal/rollouthealth` (new) + one line of `internal/scan` + `internal/remediation` + test/doc changes — no collect/cluster/watch/RBAC/Helm change → **LIGHTWEIGHT SMOKE** gate (a Kind cluster with a Deployment whose rollout times out — e.g. a new revision with a bad image and a short `progressDeadlineSeconds` — then `scan` and confirm the `RolloutStuck` line renders; note the pod-level ImagePullBackOff may claim it first, so use a case with no pod finding, e.g. a `ReplicaFailure`/quota block, to see `RolloutStuck` directly). **Minor** bump **v0.42.0 → v0.43.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
```
