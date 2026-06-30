# Service Backing Awareness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Annotate a "no ready endpoints" Service issue with its backing workload when that workload expects no pods (CronJob/Job, or a DaemonSet/Deployment/StatefulSet with desired 0), so these stop reading as primary problems.

**Architecture:** Add a pure `Backend` descriptor + `BackendsFrom` adapter to `internal/svchealth`, give `Assess` a `backends` parameter and classification logic (real-outage controllers with desired>0 stay primary; everything else with a match is annotated as expected), and wire `main.go` to build backends from the already-collected controller slices. Report text already prints `Detail`; JSON serializes the new `Issue` fields automatically — no `report.go` change.

**Tech Stack:** Go 1.26, stdlib `flag`, `k8s.io/api` (`apps/v1`, `batch/v1`, `core/v1`, `discovery/v1`), client-go fake clientset for tests.

## Global Constraints

- **READ-ONLY.** No new API calls — Deployments, StatefulSets, DaemonSets, Jobs, CronJobs are already collected by `collect.CollectInventory`. Never create/update/patch/delete.
- **No new Go module dependency.** Only already-present `k8s.io/api` subpackages.
- **Sequential**, stdlib `flag`. Exit codes unchanged.
- `svchealth` stays **pure**: the caller supplies Services, EndpointSlices, and backends.
- **No new noise on real issues.** A primary (real-outage) `NoEndpoints` issue and the LoadBalancer `NoExternalAddress` issue render exactly as today.
- **Exact `Detail` strings** (use verbatim, including the em-dash `—`):
  - CronJob → `no ready endpoints (backs CronJob — expected between runs)`
  - Job → `no ready endpoints (backs Job — expected between runs)`
  - DaemonSet (desired 0) → `no ready endpoints (backs DaemonSet — 0 desired)`
  - Deployment (desired 0) → `no ready endpoints (backs Deployment — scaled to 0)`
  - StatefulSet (desired 0) → `no ready endpoints (backs StatefulSet — scaled to 0)`
- Go binary is at `/usr/local/go/bin`: run `export PATH=$PATH:/usr/local/go/bin` before any go command.

---

### Task 1: `Backend` type, `selectorMatches`, `BackendsFrom`

**Files:**
- Modify: `internal/svchealth/svchealth.go`
- Test: `internal/svchealth/svchealth_test.go`

**Interfaces:**
- Produces:
  - `type Backend struct { Kind, Namespace string; TemplateLabels map[string]string; Desired int; Ephemeral bool }`
  - `func BackendsFrom(deploys []appsv1.Deployment, statefulsets []appsv1.StatefulSet, daemonsets []appsv1.DaemonSet, jobs []batchv1.Job, cronjobs []batchv1.CronJob) []Backend`
  - `func selectorMatches(selector, labels map[string]string) bool` (unexported)

- [ ] **Step 1: Write the failing tests**

Append to `internal/svchealth/svchealth_test.go`. Add `"k8s.io/api/apps/v1"` as `appsv1` and `"k8s.io/api/batch/v1"` as `batchv1` to the imports (the file already imports `corev1`, `discoveryv1`, `metav1`, `testing`).

```go
func int32p(i int32) *int32 { return &i }

func TestSelectorMatches(t *testing.T) {
	cases := []struct {
		name           string
		sel, labels    map[string]string
		want           bool
	}{
		{"subset", map[string]string{"app": "web"}, map[string]string{"app": "web", "tier": "fe"}, true},
		{"missing key", map[string]string{"app": "web"}, map[string]string{"tier": "fe"}, false},
		{"value mismatch", map[string]string{"app": "web"}, map[string]string{"app": "api"}, false},
		{"empty selector", map[string]string{}, map[string]string{"app": "web"}, false},
		{"nil labels", map[string]string{"app": "web"}, nil, false},
	}
	for _, c := range cases {
		if got := selectorMatches(c.sel, c.labels); got != c.want {
			t.Errorf("%s: selectorMatches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBackendsFrom(t *testing.T) {
	deploys := []appsv1.Deployment{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d1"},
			Spec: appsv1.DeploymentSpec{Replicas: int32p(3),
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "d"}}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "d2"}, // nil replicas → 1
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "d2"}}}}},
	}
	sts := []appsv1.StatefulSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"},
			Spec: appsv1.StatefulSetSpec{Replicas: int32p(0),
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "s"}}}}},
	}
	ds := []appsv1.DaemonSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "ds1"},
			Spec:   appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ds"}}}},
			Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 0}},
	}
	jobs := []batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "j1"},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "j"}}}}},
	}
	cronjobs := []batchv1.CronJob{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "cj1"},
			Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "cj"}}}}}}},
	}

	got := BackendsFrom(deploys, sts, ds, jobs, cronjobs)
	if len(got) != 6 {
		t.Fatalf("want 6 backends, got %d: %+v", len(got), got)
	}
	by := map[string]Backend{}
	for _, b := range got {
		by[b.Kind+"/"+b.Namespace+"/"+labelVal(b.TemplateLabels)] = b
	}
	if b := by["Deployment/a/d"]; b.Desired != 3 || b.Ephemeral {
		t.Errorf("deploy d1: want Desired 3, not ephemeral, got %+v", b)
	}
	if b := by["Deployment/a/d2"]; b.Desired != 1 {
		t.Errorf("deploy d2 nil replicas: want Desired 1, got %+v", b)
	}
	if b := by["StatefulSet/a/s"]; b.Desired != 0 || b.Ephemeral {
		t.Errorf("sts: want Desired 0, not ephemeral, got %+v", b)
	}
	if b := by["DaemonSet/a/ds"]; b.Desired != 0 || b.Ephemeral {
		t.Errorf("ds: want Desired 0 from status, not ephemeral, got %+v", b)
	}
	if b := by["Job/a/j"]; !b.Ephemeral {
		t.Errorf("job: want Ephemeral true, got %+v", b)
	}
	if b := by["CronJob/a/cj"]; !b.Ephemeral {
		t.Errorf("cronjob: want Ephemeral true, got %+v", b)
	}
}

// labelVal returns the single label value (test helper for stable map keys).
func labelVal(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/`
Expected: FAIL — `undefined: selectorMatches`, `undefined: BackendsFrom`, `undefined: Backend`.

- [ ] **Step 3: Write the implementation**

In `internal/svchealth/svchealth.go`, add `appsv1 "k8s.io/api/apps/v1"` and `batchv1 "k8s.io/api/batch/v1"` to the imports, and append:

```go
// Backend describes a workload that may back a Service: its pod-template labels
// and whether it currently wants any pods.
type Backend struct {
	Kind           string            // Deployment | StatefulSet | DaemonSet | Job | CronJob
	Namespace      string
	TemplateLabels map[string]string // the Service selector must be a subset of these
	Desired        int               // replicas / DesiredNumberScheduled (ignored when Ephemeral)
	Ephemeral      bool              // true for Job and CronJob
}

// BackendsFrom adapts the already-collected controller slices into Backends.
func BackendsFrom(deploys []appsv1.Deployment, statefulsets []appsv1.StatefulSet, daemonsets []appsv1.DaemonSet, jobs []batchv1.Job, cronjobs []batchv1.CronJob) []Backend {
	var out []Backend
	for _, d := range deploys {
		desired := 1
		if d.Spec.Replicas != nil {
			desired = int(*d.Spec.Replicas)
		}
		out = append(out, Backend{Kind: "Deployment", Namespace: d.Namespace, TemplateLabels: d.Spec.Template.Labels, Desired: desired})
	}
	for _, s := range statefulsets {
		desired := 1
		if s.Spec.Replicas != nil {
			desired = int(*s.Spec.Replicas)
		}
		out = append(out, Backend{Kind: "StatefulSet", Namespace: s.Namespace, TemplateLabels: s.Spec.Template.Labels, Desired: desired})
	}
	for _, ds := range daemonsets {
		out = append(out, Backend{Kind: "DaemonSet", Namespace: ds.Namespace, TemplateLabels: ds.Spec.Template.Labels, Desired: int(ds.Status.DesiredNumberScheduled)})
	}
	for _, j := range jobs {
		out = append(out, Backend{Kind: "Job", Namespace: j.Namespace, TemplateLabels: j.Spec.Template.Labels, Ephemeral: true})
	}
	for _, cj := range cronjobs {
		out = append(out, Backend{Kind: "CronJob", Namespace: cj.Namespace, TemplateLabels: cj.Spec.JobTemplate.Spec.Template.Labels, Ephemeral: true})
	}
	return out
}

// selectorMatches reports whether every key/value in selector is present in
// labels — i.e. the Service would select pods carrying these template labels.
// An empty selector never matches (selectorless Services are skipped upstream).
func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/ -v && go vet ./internal/svchealth/ && gofmt -l internal/svchealth/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/svchealth/
git commit -m "feat(svchealth): Backend descriptor + BackendsFrom adapter + selectorMatches"
```

---

### Task 2: `Assess` classification + `Issue` fields

**Files:**
- Modify: `internal/svchealth/svchealth.go`
- Modify: `main.go:123` (interim `nil` to keep the build green; Task 3 wires the real value)
- Test: `internal/svchealth/svchealth_test.go`

**Interfaces:**
- Consumes: `Backend`, `selectorMatches` (Task 1).
- Produces (changed signature):
  `func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []Backend) []Issue`
  and `Issue` gains `Expected bool json:"expected,omitempty"` and `Backing string json:"backing,omitempty"`.

- [ ] **Step 1: Update existing `Assess` call-sites in the test file**

In `internal/svchealth/svchealth_test.go`, every existing `Assess(...)` call takes a new third argument `nil`. There are 8 calls (lines ~39, 48, 56, 66, 76, 88, 95, 110): change `Assess(svcs, slices)` → `Assess(svcs, slices, nil)` and `Assess(svcs, nil)` → `Assess(svcs, nil, nil)`. With `nil` backends, classification finds no match, so every `NoEndpoints` issue stays primary plain (`Detail == "no ready endpoints"`, `Expected == false`) — these tests keep their current expectations.

- [ ] **Step 2: Write the failing classification tests**

Append to `internal/svchealth/svchealth_test.go` (add `"encoding/json"` and `"strings"` to imports):

```go
// backend is a terse Backend literal for classification tests.
func backend(kind, ns string, desired int, ephemeral bool, labels map[string]string) Backend {
	return Backend{Kind: kind, Namespace: ns, TemplateLabels: labels, Desired: desired, Ephemeral: ephemeral}
}

func TestAssess_ExpectedBackings(t *testing.T) {
	sel := map[string]string{"app": "x"}
	cases := []struct {
		name        string
		be          Backend
		wantBacking string
		wantDetail  string
	}{
		{"cronjob", backend("CronJob", "default", 0, true, map[string]string{"app": "x"}), "CronJob", "no ready endpoints (backs CronJob — expected between runs)"},
		{"job", backend("Job", "default", 0, true, map[string]string{"app": "x"}), "Job", "no ready endpoints (backs Job — expected between runs)"},
		{"daemonset 0", backend("DaemonSet", "default", 0, false, map[string]string{"app": "x"}), "DaemonSet", "no ready endpoints (backs DaemonSet — 0 desired)"},
		{"deployment 0", backend("Deployment", "default", 0, false, map[string]string{"app": "x"}), "Deployment", "no ready endpoints (backs Deployment — scaled to 0)"},
		{"statefulset 0", backend("StatefulSet", "default", 0, false, map[string]string{"app": "x"}), "StatefulSet", "no ready endpoints (backs StatefulSet — scaled to 0)"},
	}
	for _, c := range cases {
		svcs := []corev1.Service{svc("default", "x", corev1.ServiceTypeClusterIP, sel, 0)}
		got := Assess(svcs, nil, []Backend{c.be})
		if len(got) != 1 {
			t.Fatalf("%s: want 1 issue, got %+v", c.name, got)
		}
		if !got[0].Expected || got[0].Backing != c.wantBacking || got[0].Detail != c.wantDetail {
			t.Errorf("%s: got Expected=%v Backing=%q Detail=%q", c.name, got[0].Expected, got[0].Backing, got[0].Detail)
		}
	}
}

func TestAssess_LiveDeploymentStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("Deployment", "default", 3, false, map[string]string{"app": "web"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" || got[0].Backing != "" {
		t.Fatalf("a live (desired>0) Deployment with no endpoints must stay primary, got %+v", got)
	}
}

func TestAssess_RealOutageWinsOverCoincidentalJob(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{
		backend("Deployment", "default", 3, false, map[string]string{"app": "web"}),
		backend("CronJob", "default", 0, true, map[string]string{"app": "web"}),
	}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected {
		t.Fatalf("a live Deployment match must keep the issue primary even if a job also matches, got %+v", got)
	}
}

func TestAssess_NoMatchingBackendStaysPrimary(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	backends := []Backend{backend("CronJob", "default", 0, true, map[string]string{"app": "other"})}
	got := Assess(svcs, nil, backends)
	if len(got) != 1 || got[0].Expected || got[0].Detail != "no ready endpoints" {
		t.Fatalf("a non-matching backend must leave the issue primary, got %+v", got)
	}
}

func TestIssue_JSONOmitsEmptyExpectedAndBacking(t *testing.T) {
	primary, _ := json.Marshal(Issue{Namespace: "n", Name: "x", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"})
	if strings.Contains(string(primary), "expected") || strings.Contains(string(primary), "backing") {
		t.Errorf("primary issue JSON must omit expected/backing: %s", primary)
	}
	expected, _ := json.Marshal(Issue{Namespace: "n", Name: "x", Problem: "NoEndpoints", Expected: true, Backing: "CronJob"})
	if !strings.Contains(string(expected), `"expected":true`) || !strings.Contains(string(expected), `"backing":"CronJob"`) {
		t.Errorf("expected issue JSON must carry expected/backing: %s", expected)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/`
Expected: FAIL — `Assess` takes 2 args not 3 / `Issue` has no field `Expected`.

- [ ] **Step 4: Write the implementation**

In `internal/svchealth/svchealth.go`:

Add the two fields to `Issue` (after `Since`):

```go
	Expected bool   `json:"expected,omitempty"` // true for an expected (annotated) NoEndpoints issue
	Backing  string `json:"backing,omitempty"`  // representative backing kind, when classified
```

Change the `Assess` signature to add `backends []Backend` and classify the NoEndpoints issue. Replace the existing NoEndpoints block:

```go
		if readyEndpoints(s, slices) == 0 {
			out = append(out, Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			})
		}
```

with:

```go
		if readyEndpoints(s, slices) == 0 {
			is := Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			}
			if backing, detail, ok := classifyBacking(s, backends); ok {
				is.Expected = true
				is.Backing = backing
				is.Detail = detail
			}
			out = append(out, is)
		}
```

And change the function signature line to:

```go
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []Backend) []Issue {
```

Add the classification helpers (anywhere below `Assess`):

```go
// classifyBacking decides whether a Service's lack of endpoints is expected
// because of the workload backing it. It returns ok=false (a primary issue)
// when a live non-ephemeral controller (desired>0) backs the Service, or when
// nothing matches. Otherwise it returns the representative backing kind and the
// explanatory detail line.
func classifyBacking(svc corev1.Service, backends []Backend) (backing, detail string, ok bool) {
	var matches []Backend
	for _, b := range backends {
		if b.Namespace == svc.Namespace && selectorMatches(svc.Spec.Selector, b.TemplateLabels) {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return "", "", false
	}
	for _, b := range matches {
		if !b.Ephemeral && b.Desired > 0 {
			return "", "", false // a live controller should have endpoints — real issue
		}
	}
	b := pickBacking(matches)
	return b.Kind, backingDetail(b), true
}

// pickBacking chooses a representative backend in precedence order
// CronJob, Job, DaemonSet, Deployment, StatefulSet.
func pickBacking(matches []Backend) Backend {
	order := map[string]int{"CronJob": 0, "Job": 1, "DaemonSet": 2, "Deployment": 3, "StatefulSet": 4}
	best := matches[0]
	for _, b := range matches[1:] {
		if order[b.Kind] < order[best.Kind] {
			best = b
		}
	}
	return best
}

// backingDetail is the human one-liner for an expected NoEndpoints issue.
func backingDetail(b Backend) string {
	switch b.Kind {
	case "CronJob":
		return "no ready endpoints (backs CronJob — expected between runs)"
	case "Job":
		return "no ready endpoints (backs Job — expected between runs)"
	case "DaemonSet":
		return "no ready endpoints (backs DaemonSet — 0 desired)"
	case "Deployment":
		return "no ready endpoints (backs Deployment — scaled to 0)"
	case "StatefulSet":
		return "no ready endpoints (backs StatefulSet — scaled to 0)"
	}
	return "no ready endpoints"
}
```

- [ ] **Step 5: Keep the build green — update `main.go`**

`main.go:123` currently calls `svchealth.Assess(svcs, slices)`, which no longer compiles. Change it to pass `nil` for now (Task 3 replaces `nil` with the real backends):

```go
	serviceIssues := svchealth.Assess(svcs, slices, nil)
```

- [ ] **Step 6: Run tests + build to verify everything passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/ -v && go build ./... && go vet ./... && gofmt -l internal/svchealth/ main.go`
Expected: all svchealth tests PASS (new + existing), `go build ./...` succeeds, vet clean, gofmt prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/svchealth/ main.go
git commit -m "feat(svchealth): classify NoEndpoints by backing workload; annotate expected"
```

---

### Task 3: wire `main.go`, document, CHANGELOG

**Files:**
- Modify: `main.go:121-123`
- Modify: `README.md` (the `### Service health` section, lines ~93-100)
- Modify: `CHANGELOG.md` (the `## [Unreleased]` section)
- Test: `main_test.go` (no new test needed — covered below)

**Interfaces:**
- Consumes: `svchealth.BackendsFrom`, the new `svchealth.Assess` signature.

- [ ] **Step 1: Wire `main.go`**

Replace the interim `nil` call. The block at `main.go:121-123` currently reads:

```go
	svcs, _ := collect.Services(context.Background(), client, namespace)
	slices, _ := collect.EndpointSlices(context.Background(), client, namespace)
	serviceIssues := svchealth.Assess(svcs, slices, nil)
```

Change it to:

```go
	svcs, _ := collect.Services(context.Background(), client, namespace)
	slices, _ := collect.EndpointSlices(context.Background(), client, namespace)
	backends := svchealth.BackendsFrom(inputs.Deployments, inputs.StatefulSets, inputs.DaemonSets, inputs.Jobs, inputs.CronJobs)
	serviceIssues := svchealth.Assess(svcs, slices, backends)
```

(`inputs` is the `collect.CollectInventory` result already in scope earlier in `run`.)

- [ ] **Step 2: Build + run the full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./... && go vet ./... && gofmt -l main.go`
Expected: build succeeds, all packages `ok`, vet clean, gofmt prints nothing.

- [ ] **Step 3: Document in `README.md`**

In the `### Service health` section, replace the sentence:

```markdown
skipped. Checks are read-only and honor the scan's `-n` scope.
```

with:

```markdown
skipped. A "no ready endpoints" issue whose backing workload expects no pods — a
CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — is annotated
with that backing (e.g. `backs CronJob — expected between runs`) so it does not
read as a primary problem; a Deployment/StatefulSet with replicas and no
endpoints stays primary. Checks are read-only and honor the scan's `-n` scope.
```

- [ ] **Step 4: Update `CHANGELOG.md`**

Under `## [Unreleased]`, add an `### Added` subsection **above** the existing `### Fixed` subsection (Keep-a-Changelog orders Added before Fixed):

```markdown
### Added

- **Service backing awareness.** A "no ready endpoints" Service issue is now
  annotated with its backing workload when that workload expects no pods — a
  CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — so these stop
  reading as primary problems (text + JSON `expected`/`backing`). A
  Deployment/StatefulSet with replicas and no endpoints stays a primary issue.
```

- [ ] **Step 5: Commit**

```bash
git add main.go README.md CHANGELOG.md
git commit -m "feat: wire service backing awareness into scan; document + changelog"
```

---

## Self-Review

**Spec coverage:**
- `Backend` + `BackendsFrom` adapter (Component 1) → Task 1. ✓
- `selectorMatches` → Task 1. ✓
- `Assess` classification + `Issue.Expected/Backing` (Component 2) → Task 2. ✓
- Detail strings table (verbatim) → Task 2 `backingDetail`. ✓
- Real-outage-wins precedence → Task 2 `classifyBacking` (desired>0 short-circuit) + test. ✓
- text unchanged (Detail carries it) / JSON auto-serializes (Component 3) → no report change; JSON field test in Task 2. ✓
- wiring (Component 4) → Task 3. ✓
- READ-ONLY / no new dep / no new noise (Global Constraints) → no new API calls; only existing k8s.io/api subpackages; primary issues unchanged. ✓

**Placeholder scan:** none — every code/step is complete.

**Type consistency:** `Backend{Kind, Namespace, TemplateLabels, Desired, Ephemeral}`, `BackendsFrom(deploys, statefulsets, daemonsets, jobs, cronjobs)`, `Assess(services, slices, backends)`, and `classifyBacking` returning `(backing, detail string, ok bool)` are used identically across Tasks 1–3. The `backingDetail` strings match the `TestAssess_ExpectedBackings` expectations exactly.
