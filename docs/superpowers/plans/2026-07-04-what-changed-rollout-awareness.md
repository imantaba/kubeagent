# "what changed" rollout awareness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For a flagged Deployment, surface its most recent rollout (revision, age, first-container image delta) when that rollout is recent (7-day window), so a degraded workload reads as a lead ("changed 4d ago · image A → B") rather than a bare symptom.

**Architecture:** A new pure annotator package `internal/rollout` mirrors `internal/netpolicy`: it sets a new optional `Workload.Rollout` hint field from the already-collected `[]appsv1.ReplicaSet`. `main.go` calls it after `netpolicy.Annotate`; `report` renders one line; `explain` adds one prompt fact; JSON gets the field for free.

**Tech Stack:** Go 1.26. `k8s.io/api/apps/v1` and the `deployment.kubernetes.io/revision` annotation parsing are already used by `internal/remediate`. No new module dependency.

## Global Constraints

- **READ-ONLY.** Reuses `inputs.ReplicaSets` (already collected) — no new API calls, no extra egress, no writes, independent of `--fix`.
- **Factual, never causal.** State what changed and when; never assert the rollout caused the symptom.
- **`--explain` stays structured-facts-only** — the rollout fact (revision/age/image) is already in the report; the egress-guard test must still pass.
- Deployments only; first-container image only; hardcoded 7-day window. No new module dependency.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: data model + `internal/rollout` annotator + tests

**Files:**
- Modify: `internal/inventory/inventory.go`
- Create: `internal/rollout/rollout.go`, `internal/rollout/rollout_test.go`

**Interfaces:**
- Produces: `inventory.RolloutChange` struct + `Workload.Rollout *RolloutChange` field; `rollout.Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, now time.Time)`.

- [ ] **Step 1: Add the data model to `internal/inventory/inventory.go`**

Add the `Rollout` field to the `Workload` struct, immediately after the existing `NetworkPolicies` field:

```go
	Rollout         *RolloutChange     `json:"rollout,omitempty"`         // recent-rollout correlation (hint; set by rollout.Annotate)
```

And add the struct definition immediately after the `Workload` struct's closing brace (before the `Flagged` method):

```go
// RolloutChange is a recent-rollout correlation for a flagged Deployment: what
// changed (revision, image) and when. Set by rollout.Annotate; nil when there is
// no recent rollout to report.
type RolloutChange struct {
	Revision string `json:"revision"`
	Since    string `json:"since"`
	OldImage string `json:"oldImage,omitempty"`
	NewImage string `json:"newImage,omitempty"`
}
```

- [ ] **Step 2: Write the failing tests — create `internal/rollout/rollout_test.go`**

```go
package rollout

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

var now = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

// flaggedDep builds a flagged (Ready<Desired) Deployment workload.
func flaggedDep(ns, name string) inventory.Workload {
	return inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}
}

// rs builds a ReplicaSet owned by `owner` at `revision`, created `age` before
// now, whose single container runs `image`.
func rs(ns, name, owner, revision, image string, age time.Duration) appsv1.ReplicaSet {
	r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations:       map[string]string{"deployment.kubernetes.io/revision": revision},
		OwnerReferences:   []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
		CreationTimestamp: metav1.Time{Time: now.Add(-age)},
	}}
	r.Spec.Template.Spec.Containers = []corev1.Container{{Name: "c", Image: image}}
	return r
}

func TestAnnotate_RecentRolloutWithImageChange(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 30*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:bad", 4*24*time.Hour),
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil {
		t.Fatal("expected a Rollout annotation")
	}
	if got.Revision != "2" || got.OldImage != "nginx:1.27" || got.NewImage != "nginx:bad" {
		t.Errorf("unexpected rollout: %+v", got)
	}
	if got.Since == "" {
		t.Errorf("expected a Since age, got empty")
	}
}

func TestAnnotate_OldRolloutSkipped(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 60*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:bad", 30*24*time.Hour), // > 7d old
	}
	Annotate(wls, rss, now)
	if wls[0].Rollout != nil {
		t.Errorf("rollout older than the window should not annotate, got %+v", wls[0].Rollout)
	}
}

func TestAnnotate_ImageUnchanged(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{
		rs("shop", "web-1", "web", "1", "nginx:1.27", 10*24*time.Hour),
		rs("shop", "web-2", "web", "2", "nginx:1.27", 2*24*time.Hour), // same image
	}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil || got.Revision != "2" {
		t.Fatalf("expected rollout revision 2, got %+v", got)
	}
	if got.OldImage != "" || got.NewImage != "" {
		t.Errorf("unchanged image should leave the delta empty, got %+v", got)
	}
}

func TestAnnotate_SingleRevisionNoDelta(t *testing.T) {
	wls := []inventory.Workload{flaggedDep("shop", "web")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1", "nginx:bad", 1*24*time.Hour)}
	Annotate(wls, rss, now)
	got := wls[0].Rollout
	if got == nil || got.Revision != "1" {
		t.Fatalf("expected rollout revision 1, got %+v", got)
	}
	if got.OldImage != "" || got.NewImage != "" {
		t.Errorf("no prior revision -> no delta, got %+v", got)
	}
}

func TestAnnotate_SkipsNonDeploymentAndHealthy(t *testing.T) {
	ss := inventory.Workload{Namespace: "shop", Name: "ss", Kind: "StatefulSet", Desired: 1, Ready: 0}
	healthy := inventory.Workload{Namespace: "shop", Name: "ok", Kind: "Deployment", Desired: 1, Ready: 1}
	wls := []inventory.Workload{ss, healthy}
	rss := []appsv1.ReplicaSet{
		rs("shop", "ss-1", "ss", "1", "img", 1*24*time.Hour),
		rs("shop", "ok-1", "ok", "1", "img", 1*24*time.Hour),
	}
	Annotate(wls, rss, now)
	if wls[0].Rollout != nil || wls[1].Rollout != nil {
		t.Errorf("non-Deployment / healthy should not annotate: %+v %+v", wls[0].Rollout, wls[1].Rollout)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rollout/`
Expected: build failure / FAIL — package `rollout` (and `Annotate`) does not exist yet.

- [ ] **Step 4: Implement — create `internal/rollout/rollout.go`**

```go
// Package rollout annotates flagged Deployments with their most recent rollout —
// what changed (revision, image) and when — so a degraded workload reads as a
// lead ("changed 4d ago") rather than a bare symptom. Pure and read-only; the
// caller supplies workloads, ReplicaSets, and the clock.
package rollout

import (
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const revisionAnno = "deployment.kubernetes.io/revision"

// recencyWindow bounds how old a rollout may be and still be reported as a
// recent change. A flagged Deployment whose current rollout predates this window
// gets no annotation.
const recencyWindow = 7 * 24 * time.Hour

// Annotate sets w.Rollout for each flagged Deployment whose current (highest-
// revision) ReplicaSet was created within recencyWindow of now, recording the
// revision, its age, and the first-container image delta versus the previous
// revision (image left empty when unchanged or when there is no prior revision).
// It mutates the slice elements in place.
func Annotate(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, now time.Time) {
	for i := range workloads {
		w := workloads[i]
		if !w.Flagged() || w.Kind != "Deployment" {
			continue
		}
		cur, prev := currentAndPrevRS(w.Namespace, w.Name, replicaSets)
		if cur == nil {
			continue
		}
		if now.Sub(cur.CreationTimestamp.Time) > recencyWindow {
			continue // rollout too old to be "what changed"
		}
		rc := &inventory.RolloutChange{
			Revision: strconv.Itoa(revOf(*cur)),
			Since:    inventory.HumanSince(cur.CreationTimestamp.Time.UTC().Format(time.RFC3339), now),
		}
		if prev != nil {
			if o, n := firstImage(*prev), firstImage(*cur); o != n && o != "" && n != "" {
				rc.OldImage, rc.NewImage = o, n
			}
		}
		workloads[i].Rollout = rc
	}
}

// currentAndPrevRS returns the ReplicaSets with the highest and second-highest
// revision owned by the named Deployment (prev is nil when only one revision).
func currentAndPrevRS(namespace, deployment string, replicaSets []appsv1.ReplicaSet) (cur, prev *appsv1.ReplicaSet) {
	var owned []appsv1.ReplicaSet
	for _, rs := range replicaSets {
		if rs.Namespace == namespace && ownedBy(rs, deployment) && revOf(rs) > 0 {
			owned = append(owned, rs)
		}
	}
	if len(owned) == 0 {
		return nil, nil
	}
	sort.Slice(owned, func(i, j int) bool { return revOf(owned[i]) > revOf(owned[j]) })
	cur = &owned[0]
	if len(owned) > 1 {
		prev = &owned[1]
	}
	return cur, prev
}

func ownedBy(rs appsv1.ReplicaSet, deployment string) bool {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Name == deployment {
			return true
		}
	}
	return false
}

func revOf(rs appsv1.ReplicaSet) int {
	if v, ok := rs.Annotations[revisionAnno]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

func firstImage(rs appsv1.ReplicaSet) string {
	cs := rs.Spec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rollout/ ./internal/inventory/ -v && go build ./... && go vet ./internal/rollout/ && gofmt -l internal/rollout/ internal/inventory/`
Expected: all rollout tests PASS, inventory still PASS, build succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/inventory/ internal/rollout/
git commit -m "feat(rollout): annotate flagged Deployments with their recent rollout (revision, age, image delta)"
```

---

### Task 2: wiring + report render + `--explain` fact + tests

**Files:**
- Modify: `main.go`, `internal/report/report.go`, `internal/explain/explain.go`
- Test: `internal/report/report_test.go`, `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `inventory.Workload.Rollout` (from Task 1); `rollout.Annotate`.

- [ ] **Step 1: Wire the annotator into `main.go`**

Add the import in the `internal/...` import group (keep alphabetical — after `remediate`, before `report`):

```go
	"github.com/imantaba/kubeagent/internal/rollout"
```

Then, immediately after the existing `netpolicy.Annotate(result.Workloads, podLabels, nps)` line, add:

```go
	rollout.Annotate(result.Workloads, inputs.ReplicaSets, time.Now())
```

(`time` and `inputs.ReplicaSets` are already in scope.)

- [ ] **Step 2: Write the failing report test — add to `internal/report/report_test.go`**

```go
func TestPrintInventory_TextShowsRolloutChange(t *testing.T) {
	wl := inventory.Workload{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "bad image"}},
		Rollout:  &inventory.RolloutChange{Revision: "6", Since: "4d ago", OldImage: "nginx:1.27", NewImage: "nginx:bad"}}
	var buf bytes.Buffer
	result := inventory.Result{Workloads: []inventory.Workload{wl}}
	if err := PrintInventory(clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}, result, nil, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "changed: rollout to revision 6, 4d ago") {
		t.Errorf("missing rollout-change line:\n%s", out)
	}
	if !strings.Contains(out, "image nginx:1.27 → nginx:bad") {
		t.Errorf("missing image delta:\n%s", out)
	}
}
```

(If `bytes`, `clusterhealth`, `diagnose`, `inventory`, `strings` are not already imported in the test file, add them — match the imports the other `TestPrintInventory_*` tests use.)

- [ ] **Step 3: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintInventory_TextShowsRolloutChange`
Expected: FAIL — the line is not rendered yet.

- [ ] **Step 4: Render the line in `internal/report/report.go`**

In `printWorkload`, immediately after the `NetworkPolicies` block (the `if len(wl.NetworkPolicies) > 0 { ... }` block) and before the `for _, p := range wl.Pods {` loop, add:

```go
	if wl.Rollout != nil {
		line := fmt.Sprintf("    ↳ changed: rollout to revision %s, %s", wl.Rollout.Revision, wl.Rollout.Since)
		if wl.Rollout.NewImage != "" {
			line += fmt.Sprintf(" · image %s → %s", wl.Rollout.OldImage, wl.Rollout.NewImage)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
```

- [ ] **Step 5: Add the `--explain` fact in `internal/explain/explain.go`**

In `buildInventoryPrompt`, inside the `for _, w := range workloads` loop, immediately after the `if len(w.NetworkPolicies) > 0 { ... }` block, add:

```go
			if w.Rollout != nil {
				fmt.Fprintf(&b, "    recent change: rolled out to revision %s %s", w.Rollout.Revision, w.Rollout.Since)
				if w.Rollout.NewImage != "" {
					fmt.Fprintf(&b, ", image %s → %s", w.Rollout.OldImage, w.Rollout.NewImage)
				}
				b.WriteString("\n")
			}
```

- [ ] **Step 6: Add the failing→passing explain test — add to `internal/explain/explain_test.go`**

```go
func TestBuildInventoryPrompt_IncludesRolloutChange(t *testing.T) {
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	ws := []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment", Ready: 0, Desired: 1, Status: "Degraded",
		Rollout: &inventory.RolloutChange{Revision: "6", Since: "4d ago", OldImage: "nginx:1.27", NewImage: "nginx:bad"}}}
	got := buildInventoryPrompt(ch, nil, nil, nil, ws)
	if !strings.Contains(got, "recent change: rolled out to revision 6 4d ago") {
		t.Errorf("prompt missing rollout change:\n%s", got)
	}
	if !strings.Contains(got, "image nginx:1.27 → nginx:bad") {
		t.Errorf("prompt missing image delta:\n%s", got)
	}
}
```

- [ ] **Step 7: Run the full report + explain suites**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ ./internal/explain/ -v && go build ./... && go vet ./... && gofmt -l internal/report/ internal/explain/ main.go`
Expected: all report + explain tests PASS (including the existing egress-guard `TestBuildInventoryPrompt_OnlyStructuredFields` and skip-path tests), build succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 8: Commit**

```bash
git add main.go internal/report/ internal/explain/
git commit -m "feat(report,explain): surface the 'what changed' rollout line in scan and --explain"
```

---

### Task 3: docs

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `website/docs/features/diagnostics.md`

- [ ] **Step 1: `CHANGELOG.md` — add to the `[Unreleased] → Added` section**

Under `## [Unreleased]`, add an `### Added` block (before the existing `### Changed` block if present):

```markdown
### Added

- **"What changed" rollout awareness.** A flagged Deployment now shows its most
  recent rollout when it is recent (within 7 days) — the revision, its age, and
  the image delta (`↳ changed: rollout to revision 6, 4d ago · image A → B`) — in
  text, JSON (`rollout`), and `--explain`. Deterministic and read-only (reuses
  the ReplicaSets already collected); factual, with no causal claim.
```

- [ ] **Step 2: `README.md` — note it near the scan/diagnostics description**

Find the paragraph describing the scan's per-workload output (near the failure diagnostics / example output). Add one sentence:

```markdown
For a flagged Deployment, kubeagent also correlates the problem with its most
recent rollout when that rollout is recent — showing the revision, its age, and
the image change (`↳ changed: rollout to revision 6, 4d ago · image A → B`) so
you can see *what changed* at a glance. It is deterministic and never claims the
rollout caused the failure.
```

- [ ] **Step 3: `website/docs/features/diagnostics.md` — a short note**

Read the file and add a short subsection at the end describing the recent-rollout correlation:

```markdown
## What changed

When a Deployment is flagged and its most recent rollout is recent (within 7
days), kubeagent adds a `changed:` line with the revision, its age, and the
first-container image delta:

```text
⚠ shop/web  Deployment  0/1 Degraded
    ⚠ ImagePullBackOff: Bad image reference or registry authentication
    ↳ changed: rollout to revision 6, 4d ago · image nginx:1.27 → nginx:bad
```

It reuses the ReplicaSet history already collected (read-only), states only what
changed and when, and never claims the rollout caused the problem — that
connection is left to you (or `--explain`).
```

- [ ] **Step 4: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed in this task).

```bash
git add README.md CHANGELOG.md website/docs/features/diagnostics.md
git commit -m "docs: document 'what changed' rollout awareness"
```

---

## Self-Review

**Spec coverage:**
- `inventory.RolloutChange` + `Workload.Rollout` field → Task 1. ✓
- `internal/rollout.Annotate` (current rollout, 7-day recency gate, first-container image delta) → Task 1. ✓
- `main.go` wiring after `netpolicy.Annotate` → Task 2. ✓
- Report `changed:` line (+ JSON via field) and `--explain` fact → Task 2. ✓
- Deployments-only / flagged-only / factual (Global Constraints) → the `Annotate` guard `!w.Flagged() || w.Kind != "Deployment"`; no causal wording. ✓
- Docs (README, CHANGELOG Added, website diagnostics) → Task 3. ✓

**Placeholder scan:** none — complete code/text in every step.

**Type/name consistency:** `inventory.RolloutChange{Revision, Since, OldImage, NewImage}`, `Workload.Rollout`, and `rollout.Annotate(workloads, replicaSets, now)` are used identically across tasks and tests. `HumanSince` is fed an RFC3339 string (`cur.CreationTimestamp.Time.UTC().Format(time.RFC3339)`), matching its documented signature.

**Deliberate small duplication:** `ownedBy` / `revOf` in `internal/rollout` parallel `remediate.ownedBy` / `remediate.revFromAnnotations` (each ~4 lines). This is intentional — `rollout` and `remediate` are independent annotator/remediation packages and should not import one another; extracting a shared revision-parsing helper is not worth the coupling for two trivial functions.
