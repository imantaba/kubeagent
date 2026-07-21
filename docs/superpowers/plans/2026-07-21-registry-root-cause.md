# Shared-Registry Root Cause Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When two or more workloads fail image pulls from the same registry host, name that registry as the shared root cause on each — the "registry outage / expired pull creds / rate limit" incident — instead of N disconnected `ImagePullBackOff` findings.

**Architecture:** A second annotator in the existing `internal/rootcause` package, `AnnotateRegistry(workloads)`, groups flagged not-yet-attributed workloads with pull-failure findings by `registryHost(Workload.Image)` and attributes groups of ≥2. It runs right after the node annotator in `scan.Evaluate` (node wins). The report needs only one wording generalization: the multi-cause attention rollup becomes `(M ⇐ K root causes)`.

**Tech Stack:** Go 1.26, standard library only for the new code (`fmt`, `sort`, `strings`). No new dependencies, no new API calls.

## Global Constraints

- **READ-ONLY; NO new RBAC / no new collector / no flag** — everything derives from `Workload.Image` + `Findings`, already assembled.
- **Pure & deterministic** — sorted host processing; fixed strings. **Node attribution wins**: `AnnotateRegistry` skips (and excludes from N) any workload whose `RootCause` is already set.
- **Always-on** — runs in `scan` and the `watch` daemon via `scan.Evaluate`.
- `inventory`, `clusterhealth`, `printWorkload`, `internal/collect`, `internal/watch`, `explain.go`, RBAC, and Helm stay **unchanged**. JSON shape unchanged (the existing `rootCause` field just gains a new value).
- **No `Co-Authored-By: Claude` trailer** (or any AI attribution) on any commit. **TDD.**
- Glyphs: `↳` U+21B3, `⇐` U+21D0 — copy exactly, never substitute ASCII.
- Set PATH first every task: `export PATH=$PATH:/usr/local/go/bin`.
- Spec: [docs/superpowers/specs/2026-07-21-registry-root-cause-design.md](../specs/2026-07-21-registry-root-cause-design.md).

---

## File Structure

- **Modify** `internal/rootcause/rootcause.go` (+ test) — `AnnotateRegistry`, `registryHost`.
- **Modify** `internal/scan/scan.go` (+ test) — one wiring line after the node annotator.
- **Modify** `internal/report/report.go` (+ test) — multi-cause wording `(M ⇐ K root causes)`; update the existing multi-node test.
- **Modify** `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` — two ghcr.io pull-failing fixture workloads + regenerate.
- **Docs:** `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

---

### Task 1: `rootcause.AnnotateRegistry` + `registryHost`

**Files:**
- Modify: `internal/rootcause/rootcause.go`
- Test: `internal/rootcause/rootcause_test.go`

**Interfaces:**
- Consumes: `inventory.Workload` (`Flagged()`, `RootCause`, `Image`, `Findings[].Issue`).
- Produces: `func AnnotateRegistry(workloads []inventory.Workload)`; unexported `registryHost(image string) string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/rootcause/rootcause_test.go` (the file already imports `testing`, `clusterhealth`, `inventory`; add `"github.com/imantaba/kubeagent/internal/diagnose"` to its imports):

```go
// pullWL builds a flagged Deployment with an image-pull finding on the given image.
func pullWL(name, image, issue string) inventory.Workload {
	return inventory.Workload{Namespace: "shop", Name: name, Kind: "Deployment",
		Ready: 0, Desired: 1, Status: "Degraded", Image: image,
		Findings: []diagnose.Finding{{Pod: "shop/" + name, Issue: issue,
			Reason: "Bad image reference or registry authentication"}}}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/org/app:v1":       "ghcr.io",
		"nginx:1.27":               "docker.io",
		"library/nginx":            "docker.io",
		"registry.local:5000/app":  "registry.local:5000",
		"localhost/app":            "localhost",
		"nginx":                    "docker.io",
	}
	for image, want := range cases {
		if got := registryHost(image); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestAnnotateRegistry_GroupOfTwoAttributed(t *testing.T) {
	ws := []inventory.Workload{
		pullWL("frontend", "ghcr.io/shop/frontend:2.4", "ImagePullBackOff"),
		pullWL("search", "ghcr.io/shop/search:1.9", "ErrImagePull"),
	}
	AnnotateRegistry(ws)
	want := "registry ghcr.io (2 workloads failing to pull)"
	if ws[0].RootCause != want || ws[1].RootCause != want {
		t.Errorf("both should be attributed %q, got %q / %q", want, ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_SingleFailerNotAttributed(t *testing.T) {
	ws := []inventory.Workload{pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" {
		t.Errorf("a lone pull-failer must not be blamed on the registry, got %q", ws[0].RootCause)
	}
}

func TestAnnotateRegistry_NodeAttributionWinsAndShrinksGroup(t *testing.T) {
	nodeOwned := pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")
	nodeOwned.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{nodeOwned, pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Errorf("node attribution must be preserved, got %q", ws[0].RootCause)
	}
	if ws[1].RootCause != "" {
		t.Errorf("with the node-attributed workload excluded, the group is 1 -> no attribution, got %q", ws[1].RootCause)
	}
}

func TestAnnotateRegistry_NonPullFindingNotGrouped(t *testing.T) {
	crash := pullWL("worker", "ghcr.io/shop/worker:5", "CrashLoopBackOff")
	ws := []inventory.Workload{crash, pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" || ws[1].RootCause != "" {
		t.Errorf("a crash finding is not a pull failure; group is 1 -> none attributed, got %q / %q", ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_NotFlaggedSkipped(t *testing.T) {
	healthy := pullWL("web", "ghcr.io/shop/web:3.1", "ImagePullBackOff")
	healthy.Ready, healthy.Desired, healthy.Status = 1, 1, "Running"
	healthy.Findings = nil // healthy: no findings, not flagged
	ws := []inventory.Workload{healthy, pullWL("api", "ghcr.io/shop/api:1.0", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" || ws[1].RootCause != "" {
		t.Errorf("unflagged workload must not count toward the group, got %q / %q", ws[0].RootCause, ws[1].RootCause)
	}
}

func TestAnnotateRegistry_TwoGroupsIndependent(t *testing.T) {
	ws := []inventory.Workload{
		pullWL("a", "ghcr.io/x/a:1", "ImagePullBackOff"),
		pullWL("b", "ghcr.io/x/b:1", "ImagePullBackOff"),
		pullWL("c", "quay.io/y/c:1", "ErrImagePull"),
		pullWL("d", "quay.io/y/d:1", "ImagePullBackOff"),
	}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "registry ghcr.io (2 workloads failing to pull)" ||
		ws[2].RootCause != "registry quay.io (2 workloads failing to pull)" {
		t.Errorf("each group gets its own host, got %q / %q", ws[0].RootCause, ws[2].RootCause)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause`
Expected: FAIL — build error (`registryHost`, `AnnotateRegistry` undefined).

- [ ] **Step 3: Implement**

In `internal/rootcause/rootcause.go`: add `"fmt"` and `"strings"` to the import block (alongside the existing `"sort"`), extend the package comment's first sentence context is fine as-is, and append:

```go
// AnnotateRegistry sets w.RootCause = "registry <host> (<N> workloads failing to
// pull)" on each flagged, not-yet-attributed workload whose image-pull failure
// shares a registry host with at least one other such workload — the shared
// signature of a registry outage, expired pull credentials, or rate limiting.
// Pure and deterministic (hosts processed in sorted order). Call after Annotate:
// node attribution wins, and a node-attributed workload is excluded from the
// group count too.
func AnnotateRegistry(workloads []inventory.Workload) {
	groups := map[string][]int{}
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() || w.RootCause != "" || w.Image == "" || !hasPullFailure(*w) {
			continue
		}
		host := registryHost(w.Image)
		groups[host] = append(groups[host], i)
	}
	hosts := make([]string, 0, len(groups))
	for h := range groups {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		members := groups[host]
		if len(members) < 2 {
			continue
		}
		cause := fmt.Sprintf("registry %s (%d workloads failing to pull)", host, len(members))
		for _, i := range members {
			workloads[i].RootCause = cause
		}
	}
}

// hasPullFailure reports whether the workload carries an image-pull finding.
func hasPullFailure(w inventory.Workload) bool {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return true
		}
	}
	return false
}

// registryHost extracts the registry host from a container image reference using
// the standard rules: the first path segment is a registry iff it contains "." or
// ":" or is "localhost"; otherwise the image lives on Docker Hub ("docker.io").
func registryHost(image string) string {
	seg, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(seg, ".:") && seg != "localhost") {
		return "docker.io"
	}
	return seg
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/rootcause -v && go vet ./internal/rootcause`
Expected: PASS — all new `TestRegistryHost`/`TestAnnotateRegistry_*` plus the existing `TestAnnotate_*` node tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -m "feat(rootcause): attribute shared image-pull failures to their registry"
```

---

### Task 2: Wire `AnnotateRegistry` into `scan.Evaluate`

**Files:**
- Modify: `internal/scan/scan.go`
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `rootcause.AnnotateRegistry` (Task 1); the existing node-annotator line at scan.go:192.
- Produces: no signature change; registry `RootCause` values now appear on `result.Inventory.Workloads`.

- [ ] **Step 1: Write the failing integration test**

Add to `internal/scan/scan_test.go` (reuses the existing `p32` helper; mirrors the crash-loop test's fake-object shape — `Workload.Image` is derived from the pod's first container image):

```go
func TestEvaluate_AttributesSharedRegistryFailure(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	depA := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "frontend"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	depB := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "search"},
		Spec: appsv1.DeploymentSpec{Replicas: p32(1)}}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "frontend-1",
		Labels: map[string]string{"app": "frontend"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "frontend", Image: "ghcr.io/shop/frontend:2.4"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "frontend", Ready: false, Image: "ghcr.io/shop/frontend:2.4",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "search-1",
		Labels: map[string]string{"app": "search"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "search", Image: "ghcr.io/shop/search:1.9"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "search", Ready: false, Image: "ghcr.io/shop/search:1.9",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	cli := fake.NewSimpleClientset(node, depA, depB, podA, podB)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for _, w := range res.Inventory.Workloads {
		if w.RootCause == "registry ghcr.io (2 workloads failing to pull)" {
			attributed++
		}
	}
	if attributed != 2 {
		t.Errorf("want both workloads attributed to registry ghcr.io, got %d: %+v", attributed, res.Inventory.Workloads)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan -run TestEvaluate_AttributesSharedRegistry`
Expected: FAIL — `RootCause` empty (wiring absent).

- [ ] **Step 3: Add the wiring**

In `internal/scan/scan.go`, immediately AFTER the existing line `rootcause.Annotate(result.Workloads, health.DownNodes)` (line 192), add:

```go
	rootcause.AnnotateRegistry(result.Workloads)
```

(No import change — `rootcause` is already imported. The order is load-bearing: node attribution first.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/scan`
Expected: PASS (new test + all existing `Evaluate` tests).

- [ ] **Step 5: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): run registry root-cause attribution after node attribution"
```

---

### Task 3: `report` — generalize the multi-cause wording

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:** none new — a string change plus comment updates.

- [ ] **Step 1: Update the existing test + add the mixed-cause test (failing first)**

In `internal/report/report_test.go`:

(a) In `TestPrintInventory_RootCauseMultiNodeRollup` (around line 1443), change the assertion from `"(2 ⇐ 2 unhealthy nodes)"` to `"(2 ⇐ 2 root causes)"` (and its error message from "2 unhealthy nodes" to "2 root causes").

(b) Add a new test:

```go
func TestPrintInventory_RootCauseMixedNodeAndRegistry(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "node worker-2 (NotReady)"},
		{Namespace: "shop", Name: "frontend", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "search", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 2, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "(3 ⇐ 2 root causes)") {
		t.Errorf("mixed node+registry causes should roll up as 2 root causes:\n%s", out)
	}
	if !strings.Contains(out, "↳ likely caused by registry ghcr.io (2 workloads failing to pull)") {
		t.Errorf("registry cause line should render via the generic path:\n%s", out)
	}
}

func TestPrintInventory_SingleRegistryCauseNamed(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "frontend", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "search", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			RootCause: "registry ghcr.io (2 workloads failing to pull)"},
	}
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 3, NodesTotal: 3},
		Result: inventory.Result{Workloads: ws}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(2 ⇐ registry ghcr.io)") {
		t.Errorf("single distinct cause should be named:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run tests to verify the state**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory_RootCause`
Expected: FAIL — the updated multi-node test and the new mixed test both want "root causes" but the code still prints "unhealthy nodes". (`TestPrintInventory_SingleRegistryCauseNamed` may already pass — the single-cause path is generic.)

- [ ] **Step 3: Implement the wording change**

In `internal/report/report.go`, `attentionLine` (line ~263), change:

```go
				s += fmt.Sprintf(" (%d ⇐ %d unhealthy nodes)", attributed, len(causeNodes))
```

to:

```go
				s += fmt.Sprintf(" (%d ⇐ %d root causes)", attributed, len(causeNodes))
```

Update the comments to be cause-generic: on `rootCauseNode` (line ~284), change the comment to say it extracts the cause prefix (`node X` / `registry Y`) from the fixed `"<cause> (<detail>)"` format; on `attentionLine`'s doc comment mention root causes rather than nodes if it names them. Do not rename identifiers (keep `rootCauseNode`, `causeNodes`) — a rename would churn Task 4's anchors for no behavior gain.

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestPrintInventory`
Expected: PASS. `TestGoldenScanOutput` will now FAIL (snapshot still says "unhealthy nodes") — that is EXPECTED and is fixed in Task 4; do NOT regenerate here.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): generalize the attention rollup to mixed root causes"
```

---

### Task 4: Golden fixture + snapshot

**Files:**
- Modify: `internal/report/golden_test.go`
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated)

**Interfaces:** none — exercises Tasks 1–3 output through the real renderer. The golden renders a pre-built `Input` (it does not run the annotators), so `RootCause` is set directly on the fixture entries.

- [ ] **Step 1: Add two ghcr.io pull-failing workloads to the fixture**

In `internal/report/golden_test.go`, `goldenWorkloads()`, insert these two entries immediately AFTER the existing `api` entry (grouping the pull failures together; style mirrors `api`):

```go
		{Namespace: "shop", Name: "frontend", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Image: "ghcr.io/shop/frontend:2.4", RootCause: "registry ghcr.io (2 workloads failing to pull)",
			Pods:     []inventory.PodRow{{Name: "frontend-58d-x2vqp", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-3", IP: "10.244.3.14", Age: "3h", Image: "ghcr.io/shop/frontend:2.4"}},
			Findings: []diagnose.Finding{{Pod: "shop/frontend", Issue: "ImagePullBackOff", Reason: "Bad image reference or registry authentication", Evidence: `container "frontend": Back-off pulling image "ghcr.io/shop/frontend:2.4": 403 Forbidden`}}},
		{Namespace: "shop", Name: "search", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Image: "ghcr.io/shop/search:1.9", RootCause: "registry ghcr.io (2 workloads failing to pull)",
			Pods:     []inventory.PodRow{{Name: "search-7b4-mm1zq", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-3", IP: "10.244.3.15", Age: "3h", Image: "ghcr.io/shop/search:1.9"}},
			Findings: []diagnose.Finding{{Pod: "shop/search", Issue: "ErrImagePull", Reason: "Bad image reference or registry authentication", Evidence: `container "search": pulling image "ghcr.io/shop/search:1.9": 403 Forbidden`}}},
```

- [ ] **Step 2: Run the golden test to see it fail (snapshot stale)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput`
Expected: FAIL — the attention line now reads `13 workloads failing (6 ⇐ 3 root causes)` and two new workload blocks exist.

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: PASS.

- [ ] **Step 4: Inspect the regenerated snapshot**

Run: `grep -n "workloads failing\|likely caused by\|frontend\|search" internal/report/testdata/golden-scan.txt | head -20`
Expected: (a) the attention clause reads `13 workloads failing (6 ⇐ 3 root causes)` (13 = 11 + 2 new; 6 = 4 node-attributed + 2 registry-attributed; 3 = worker-1, worker-2, ghcr.io); (b) two new `✗ shop/frontend` / `✗ shop/search` blocks each carrying `    ↳ likely caused by registry ghcr.io (2 workloads failing to pull)` before their finding lines; (c) the four existing node-attribution lines are unchanged. If any number differs, STOP — recheck the fixture.

- [ ] **Step 5: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections` — modes now also include `ErrImagePull`).

- [ ] **Step 6: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover registry root-cause attribution in the golden snapshot"
```

---

### Task 5: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs only). `go build`/`go test` stay green.

- [ ] **Step 1: Extend the diagnostics subsection**

In `website/docs/features/diagnostics.md`: retitle the heading `### Root-cause attribution (node)` to `### Root-cause attribution`, and append this paragraph at the end of that subsection (after the existing node paragraph):

```markdown
The same mechanism names a **shared registry** as the root cause: when two or
more workloads are failing image pulls (`ImagePullBackOff` / `ErrImagePull`)
whose images resolve to the same registry host, each is attributed
`↳ likely caused by registry <host> (<N> workloads failing to pull)` — the
signature of a registry outage, expired pull credentials, or rate limiting. A
single workload failing a pull is never blamed on the registry (that is usually a
typo'd image), and a workload already attributed to a hard-down node keeps the
node attribution. Docker Hub images (`nginx:...`) group under `docker.io`.
```

- [ ] **Step 2: Extend the README bullet**

In `README.md`, extend the existing root-cause bullet — change:

```markdown
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X") instead of N disconnected findings.
```

to:

```markdown
- **Root-cause attribution** — when a node is NotReady or its kubelet stops
  heartbeating, workloads with pods on it are attributed to that node ("↳ likely
  caused by node X"); when several workloads fail image pulls from the same
  registry, they are attributed to that registry — one shared cause instead of N
  disconnected findings.
```

- [ ] **Step 3: CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]` → `### Added` (create the headers if the last release consumed them):

```markdown
- **Shared-registry root-cause attribution.** When two or more workloads fail
  image pulls from the same registry host, `scan` names that registry as the
  shared root cause on each ("↳ likely caused by registry ghcr.io (2 workloads
  failing to pull)") — the registry-outage / expired-credentials / rate-limit
  incident. A lone pull failure is never blamed on the registry, and node
  attribution takes precedence. The attention-line rollup now reads
  "(M ⇐ K root causes)" when causes mix. Read-only, always-on, no new RBAC.
```

- [ ] **Step 4: Extend the roadmap Shipped bullet**

In `website/docs/roadmap.md`, extend the node-attribution Shipped bullet — change its text to:

```markdown
- **Root-cause attribution (nodes & registries)** — a hard-down node (NotReady or
  kubelet-not-heartbeating) becomes the named root cause of the workloads with pods
  on it, and a registry shared by two-plus failing image pulls becomes the named
  root cause of those workloads; the first slices of the root-cause correlation
  theme. See [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: Verify docs build (if mkdocs available) + full suite**

Run: `cd website && mkdocs build --strict -f mkdocs.yml 2>&1 | tail -3; cd ..` (skip with a note if mkdocs is not installed — convenience check, not a gate).
Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/diagnostics.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document shared-registry root-cause attribution"
```

---

## Notes for the executor

- **Release gate (post-merge):** touches `rootcause`/`scan`/`report` only — **not** `internal/collect`/`cluster`/`watch`/RBAC/Helm — so a **lightweight real-cluster smoke** (live negative check + a Kind cluster with two deployments pulling from an unreachable registry) suffices; no full chaos gate. Version bump: **minor**, v0.29.0 → **v0.30.0**.
- **Precedence is load-bearing:** `AnnotateRegistry` runs after the node annotator and must skip (and not count) workloads with an existing `RootCause`.
- **Honesty:** threshold 2 and the hedged "likely" wording are deliberate — do not strengthen either.
- The retitled diagnostics heading changes its anchor (`#root-cause-attribution-node` → `#root-cause-attribution`); no in-repo links point at the old anchor (verify with a grep before committing docs).
