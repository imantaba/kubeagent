# `kubeagent_inspect` object resolution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `kubeagent_inspect` resolve every object of the seven kinds its
enum advertises — including the controller-owned pod whose identity
`kubeagent_triage` emits for every critical finding, and the healthy workload
`inventory.Prioritize` drops — so `found: false` means the object does not
exist rather than "absent, or healthy, or truncated".

**Architecture:** One pure resolver in `internal/mcp/inspect.go` replaces the
`res.Inventory.Workloads` match loop. Existence is answered against
`scan.Result.Inputs` — the raw unfiltered snapshot whose seven collections are
exactly `inspectKinds` — so the fix adds **no cluster read**. Controller kinds
match `inventory.Assemble(res.Inputs, findings)`, the unfiltered workload set
recomputed in memory; `kind: pod` looks the pod up in `res.Inputs.Pods`
directly, which sidesteps `Assemble`'s `jobPodCap` truncation. A pod answers as
itself and names its controller in a new optional `owner` field. One new export,
`inventory.PodRowFor`, exists so the pod path does not duplicate three
unexported row-building helpers.

**Tech Stack:** Go 1.26, `k8s.io/client-go` (fake clientset for the handler
tests), `github.com/modelcontextprotocol/go-sdk/mcp`. **No new dependency** —
every package this plan touches is already imported somewhere in the module.

## Global Constraints

Copied verbatim from
[the spec](../specs/2026-08-08-mcp-inspect-pod-resolution-design.md)'s
Constraints section. Every task's requirements implicitly include this section.

- `internal/mcp` is read-only toward the cluster (`get`/`list`/`watch` only, no
  writes) and must never import `internal/remediate` or `internal/explain`.
  **Separately**, it makes **no LLM call**. The two are never blurred, and no
  comment, doc line, help string or commit message may suggest an MCP tool is
  related to `--explain`.
- Untrusted API text is sanitized at ingress via `internal/safetext.Line`, never
  at the renderer. Matching decisions run on the raw value. This slice
  introduces no new unvalidated-text ingress point (a pod phase is a
  server-set enum), so it adds no `safetext` call — and must not read any
  message, reason or condition text that is not already surfaced.
- `internal/mcp`'s single carve-out is unchanged: the eager startup connection
  check names the kubeconfig path and context on stderr. The protocol stream and
  every tool result stay path-free. The new code path must introduce no
  kubeconfig path, no full server URL and no node name that was not already
  there.
- **No new dependency:** `go.mod` and `go.sum` must not change. Verify with
  `git diff --stat main -- go.mod go.sum` (must be empty) at the end of every
  task.
- **No schema moves.** `InspectOutput` is not one of the eight versioned JSON
  documents (`report.ScanReport`, `gate.Verdict`, `rbacprofile.RulesDocument`,
  `rbacprofile.CheckDocument`, `watch.IssuesReport`,
  `watch.ExplanationsReport`, `baseline.Document`, `fleet.Report`), so nothing
  under `website/docs/schemas/` moves. **Never run any test with `-update`, for
  any reason.**
- `internal/report/testdata/golden-scan.txt` stays **byte-identical**. The
  README demo GIF and `website/docs/quickstart.md` are **not** regenerated.
- `plugin_manifest_test.go` and `internal/cli/plugin_flags_test.go` must stay
  green.
- No secrets, credentials, private IPs or internal hostnames anywhere — code,
  tests, fixtures, docs or help text. Every example and fixture uses a
  **synthetic** name. Documentation IPs are RFC 5737 (`192.0.2.0/24`,
  `198.51.100.0/24`, `203.0.113.0/24`); example domains are RFC 2606
  (`example.com`, `example.org`, `*.invalid`). Nothing beyond `scheme://host`.
- TDD: write the failing test first, run it, **watch it fail**, then implement.
- `go test` runs with `-p 2`, never `-short`. Go lives at `/usr/local/go/bin` —
  `export PATH=$PATH:/usr/local/go/bin`.
- Every commit is `git commit -s` (DCO is enforced on `main`), authored solely
  by the repository owner, with **no `Co-Authored-By` trailer and no AI
  attribution of any kind**.
- No cluster is needed to implement this slice. **DANGER: never run
  `./chaos/run.sh` in any form** — it takes ~40 minutes and injects real
  outages.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/inventory/inventory.go` | Gains the exported `PodRowFor`; `Assemble` refactored to call it, so there is one pod-row implementation. Later, `workloadStatus` becomes exported `WorkloadStatus` | 1, 4 |
| `internal/inventory/inventory_test.go` | Pins `PodRowFor` field by field; later, the `WorkloadStatus` rename | 1, 4 |
| `internal/mcp/inspect.go` | The resolver (`resolveObject`, `resolvePod`, `resolveReplicaSet`, `findingsOf`, `ownedByReplicaSet`), the `Owner` output field, the widened tool description | 2, 3, 4, 5 |
| `internal/mcp/inspect_test.go` | Resolver table, healthy-Deployment, controller-owned-pod, key-absence, `jobPodCap` and ReplicaSet tests, plus the raw-JSON helper | 2, 3, 4 |
| `skills/triaging-a-cluster/SKILL.md` | Step 3: webhook configurations added; pod inspection and `owner` described | 5 |
| `commands/triage.md` | Step 4's non-inspectable list gains webhook configurations | 5 |
| `skills/reading-kubeagent-findings/SKILL.md` | The skipped-check count is seven, or eight without `--logs` | 5 |
| `website/docs/features/mcp.md` | The `owner` field and what `found: false` now means | 6 |
| `CHANGELOG.md` | `[Unreleased]` → `### Fixed` entry | 6 |

Six tasks. Task 1 is a pure extraction with no behaviour change. Tasks 2, 3 and
4 are the three facets of the one defect, in the order they were found: Task 2
fixes the healthy-workload half and leaves the reported pod half failing; Task 3
fixes the reported defect; Task 4 fixes the ReplicaSet half, discovered during
Task 2. Tasks 5 and 6 are documentation.

**Tasks 2, 3 and 4 all modify `internal/mcp/inspect.go` and
`internal/mcp/inspect_test.go`.** Task 3 **extends** Task 2's `resolveObject` —
it changes its signature to add a `now time.Time` parameter and adds a pod
branch at the top. Task 4 adds a second branch beside it. Neither may rewrite,
rename or reword the other's resolver body or comment, and neither may change an
assertion in a test the other wrote. Task 4 makes exactly one edit inside Task
2's test file, to a fixture comment its own change makes false (Task 4 Step 5a);
that edit touches no assertion.

---

## Task 1: Extract `inventory.PodRowFor`

**Files:**
- Modify: `internal/inventory/inventory.go` (add `PodRowFor` after
  `podImage`, around line 161; change `Assemble`'s pod loop at lines 384-389)
- Test: `internal/inventory/inventory_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks — this is the first.
- Produces: `func PodRowFor(p corev1.Pod, now time.Time) PodRow` in package
  `inventory`. Task 3 calls it.

**Why this task exists:** building a pod's row needs `podReady`,
`podRestarts`, `podImage` and `termTime` — all unexported. Task 3's pod path
needs the same row. Exporting one function beats duplicating four.

- [ ] **Step 1: Cut the branch off `main` at the spec commit**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
git branch --show-current          # must print: main
git status --short                 # must be empty
git log --oneline -1               # must be the spec commit: "docs: design for kubeagent_inspect object resolution"
git checkout -b mcp-inspect-resolution
```

- [ ] **Step 2: Write the failing test**

Append to `internal/inventory/inventory_test.go`. It uses the file's existing
`pod` helper (line 128), which builds a one-container pod that is **not** ready.

```go
// TestPodRowFor_BuildsEveryRowFieldAndAssembleRoutesThroughIt pins PodRowFor's
// output field by field, then checks that Assemble still routes through it. The
// order matters: comparing PodRowFor against Assemble is the ONLY assertion
// that would be worthless, because after Step 5 Assemble delegates to
// PodRowFor — both sides of that comparison go wrong together, so it can never
// catch a field PodRowFor gets wrong. The literal below is the independent
// assertion; the Assemble comparison is a secondary check on the delegation.
func TestPodRowFor_BuildsEveryRowFieldAndAssembleRoutesThroughIt(t *testing.T) {
	// Far from any plausible wall clock on purpose: with `now` near time.Now(),
	// a 72h-old pod lands in HumanAge's "3d" bucket either way, and swapping
	// the injected clock for time.Now() would not fail this test.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := pod("shop", "cart-0", nil, 4, "registry.example.com/cart:1.2.3")
	p.CreationTimestamp = metav1.NewTime(now.Add(-72 * time.Hour))
	p.Spec.NodeName = "node-a"
	p.Status.PodIP = "192.0.2.10"
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			FinishedAt: metav1.NewTime(now.Add(-1 * time.Hour)),
		},
	}

	row := PodRowFor(p, now)

	want := PodRow{
		Name: "cart-0", Phase: "Running", Ready: "0/1", Restarts: 4,
		LastRestart: "2026-01-01T11:00:00Z", Node: "node-a", IP: "192.0.2.10",
		Age: "3d", Image: "registry.example.com/cart:1.2.3",
	}
	if row != want {
		t.Errorf("PodRowFor() = %+v\nwant             = %+v", row, want)
	}

	// Assemble must still route through PodRowFor. Age is the one field that
	// cannot match by construction — Assemble stamps it from the wall clock
	// while PodRowFor was handed a fixed one — so blank it on both sides.
	ws := Assemble(Inputs{Pods: []corev1.Pod{p}}, nil)
	if len(ws) != 1 || len(ws[0].Pods) != 1 {
		t.Fatalf("Assemble() = %+v, want one workload carrying one pod row", ws)
	}
	got := ws[0].Pods[0]
	got.Age, want.Age = "", ""
	if got != want {
		t.Errorf("Assemble's row = %+v\nwant           = %+v", got, want)
	}
}
```

`PodRow` has only string and int fields, so `!=` compares it field-for-field.

Every literal above is derived by reading the helpers, not by running the code:
`Ready` is `"0/1"` because the file's `pod` helper builds a container that is
not ready; `LastRestart` is `termTime`'s RFC 3339 rendering of `now-1h`; `Age`
is `HumanAge`'s bucket for 72h. **Do not** replace the literal with a value
copied out of a test run.

- [ ] **Step 3: Run it and watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory -run TestPodRowFor -count=1
```

Expected: **build failure**, `undefined: PodRowFor`. Report the exact message.

- [ ] **Step 4: Add `PodRowFor`**

Insert in `internal/inventory/inventory.go` immediately after `podImage`
(which ends at line 161):

```go
// PodRowFor projects one pod into the row shape a report renders. Assemble
// calls it for every pod it groups, and internal/mcp's inspect handler calls it
// for a pod it looked up directly — one implementation, so a pod row is the
// same shape whichever surface produced it.
//
// now is a parameter rather than a time.Now() call, so a caller holding an
// injected clock (the MCP server has one) gets a deterministic Age.
func PodRowFor(p corev1.Pod, now time.Time) PodRow {
	restarts, last := podRestarts(p)
	return PodRow{
		Name: p.Name, Phase: string(p.Status.Phase), Ready: podReady(p),
		Restarts: restarts, LastRestart: termTime(last),
		Node: p.Spec.NodeName, IP: p.Status.PodIP,
		Age: HumanAge(p.CreationTimestamp.Time, now), Image: podImage(p),
	}
}
```

- [ ] **Step 5: Refactor `Assemble` to call it**

In `Assemble`'s pod loop, replace the inline row literal at lines 384-389:

```go
		w.Pods = append(w.Pods, PodRow{
			Name: p.Name, Phase: string(p.Status.Phase), Ready: podReady(p),
			Restarts: restarts, LastRestart: termTime(last),
			Node: p.Spec.NodeName, IP: p.Status.PodIP,
			Age: HumanAge(p.CreationTimestamp.Time, time.Now()), Image: podImage(p),
		})
```

with:

```go
		w.Pods = append(w.Pods, PodRowFor(p, time.Now()))
```

`time.Now()` stays inside the loop, called once per pod exactly as today —
hoisting it would be a behaviour change this task is not making. The
`restarts, last := podRestarts(p)` line above it stays: `Assemble` still needs
both for `w.Restarts` and `w.LastRestart`.

- [ ] **Step 6: Run the test and watch it pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory -run TestPodRowFor -count=1 -v
```

Expected: PASS.

- [ ] **Step 7: Falsify the test — break `PodRowFor`, watch it fail**

A test that has never been seen to fail on real code proves nothing. In
`PodRowFor`, temporarily drop the `Node` field:

```go
		Node: "", IP: p.Status.PodIP,
```

```bash
go test ./internal/inventory -run TestPodRowFor -count=1
```

Expected: **FAIL**, with the two `%+v` rows differing on `Node`. Report the
exact output. Then **revert** to `Node: p.Spec.NodeName` and run a third time:

```bash
go test ./internal/inventory -run TestPodRowFor -count=1
```

Expected: PASS.

- [ ] **Step 8: Prove no behaviour changed**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./internal/inventory/... ./internal/report/... ./internal/scan/...
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: `gofmt -l` prints nothing; every package `ok`; the final
`git diff --stat` prints **nothing** — the golden scan output is byte-identical
and no schema moved. **Do not** run any test with `-update`.

- [ ] **Step 9: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go
git commit -s -m "refactor(inventory): extract PodRowFor so one pod row has one implementation

Building a pod's row needs podReady, podRestarts, podImage and termTime,
three of which are unexported. An MCP inspect call that resolves a pod
directly needs the same row, and duplicating four helpers to get it would
let the two shapes drift.

PodRowFor takes now as a parameter rather than calling time.Now(), so a
caller holding an injected clock gets a deterministic Age. Assemble passes
time.Now() per pod, exactly as it did inline, so no rendered byte moves --
pinned by the extraction test and by golden-scan.txt staying identical."
```

---

## Task 2: Resolve controller kinds against the unfiltered snapshot

**Files:**
- Modify: `internal/mcp/inspect.go` (add `resolved`, `findingsOf`,
  `resolveObject`; replace the handler's match loop at lines 121-150; add the
  `internal/diagnose` import)
- Test: `internal/mcp/inspect_test.go` (append)

**Interfaces:**
- Consumes: nothing from Task 1 (Task 3 is the consumer of `PodRowFor`).
- Produces, both used by Task 3:
  - `type resolved struct { Found bool; Kind, Status string; Desired, Ready int; Image string; Pods []inventory.PodRow; Findings []Finding }`
  - `func resolveObject(res scan.Result, kind, namespace, name string) resolved`
  - `func findingsOf(ws []inventory.Workload) []diagnose.Finding`
  - Test helpers `ctrl(kind, name string) []metav1.OwnerReference` and
    `ownedPod(ns, name string, owners []metav1.OwnerReference) *corev1.Pod` in
    `internal/mcp/inspect_test.go`.

**Why `kind: pod` is deliberately still broken after this task:** `Assemble`
seeds a **bare** pod as its own `"Pod"` workload, so the existing
`TestInspect_PodReturnsItsFindingsAndEvents` (a bare pod) stays green through
this task, while a controller-owned pod — which `PodOwners` rolls up to its
Deployment and which is therefore never a `"Pod"` workload — still answers
`found: false`. Task 3 adds the dedicated pod path. Do not attempt it here.

- [ ] **Step 1: Write the two failing tests**

Append to `internal/mcp/inspect_test.go`. `p32` already exists in that file
(line 19); `connect` is in `server_test.go` and `fixedNow` in
`coverage_test.go`.

```go
// ctrl builds the controller owner reference chain fixture the resolver tests
// need. internal/inventory's test package has its own copy; this is a different
// package and cannot reach it.
func ctrl(kind, name string) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
}

// ownedPod is a ready one-container pod owned by the given controller.
func ownedPod(ns, name string, owners []metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app:1.0.0"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			PodIP:             "192.0.2.10",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
		},
	}
}

// TestInspect_HealthyDeploymentIsFound is the second half of the resolution
// defect. inspect answered a lookup against inventory.Prioritize's output — a
// list built for display, which drops healthy-quiet workloads outright — so a
// Deployment that is fully ready with no restarts answered found:false for an
// object the cluster plainly has.
func TestInspect_HealthyDeploymentIsFound(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
	}}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, ownedPod("shop", "web-1a2b-cdef", ctrl("ReplicaSet", "web-1a2b"))))

	out := callInspect(t, cs, map[string]any{"kind": "deployment", "namespace": "shop", "name": "web"})

	if !out.Found {
		t.Fatal("Found = false for a healthy Deployment that exists")
	}
	if out.Kind != "Deployment" || out.Status != "Running" || out.Desired != 1 || out.Ready != 1 {
		t.Errorf("got kind=%q status=%q %d/%d, want Deployment Running 1/1",
			out.Kind, out.Status, out.Ready, out.Desired)
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Errorf("Pods = %+v, want the one pod behind it", out.Pods)
	}
	if len(out.Findings) != 0 {
		t.Errorf("Findings = %+v, want none on a healthy Deployment", out.Findings)
	}
}

// TestResolveObject_FindsEachKindUnderItsOwnKindOnly drives the resolver
// directly — it is a pure function over the snapshot scan.Evaluate returns, so
// it needs no server and no fake clientset. The negative half matters as much
// as the positive: a resolver that ignored the requested kind would answer
// found:true for a Deployment asked about as a StatefulSet.
func TestResolveObject_FindsEachKindUnderItsOwnKindOnly(t *testing.T) {
	const ns = "shop"
	in := inventory.Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"},
			Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
		}},
		ReplicaSets: []appsv1.ReplicaSet{{ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: "orphan-rs",
		}}},
		StatefulSets: []appsv1.StatefulSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db"},
			Spec:       appsv1.StatefulSetSpec{Replicas: p32(1)},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}},
		DaemonSets: []appsv1.DaemonSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "agent"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 1, NumberReady: 1},
		}},
		Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "migrate"}}},
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "nightly"},
			Spec:       batchv1.CronJobSpec{Schedule: "0 3 * * *"},
		}},
		Pods: []corev1.Pod{*ownedPod(ns, "loner", nil)},
	}
	res := scan.Result{Inputs: in}

	cases := []struct {
		kind string
		name string
	}{
		{"pod", "loner"},
		{"deployment", "web"},
		{"replicaset", "orphan-rs"},
		{"statefulset", "db"},
		{"daemonset", "agent"},
		{"job", "migrate"},
		{"cronjob", "nightly"},
	}
	wantKind := map[string]string{
		"pod": "Pod", "deployment": "Deployment", "replicaset": "ReplicaSet",
		"statefulset": "StatefulSet", "daemonset": "DaemonSet", "job": "Job",
		"cronjob": "CronJob",
	}

	for _, tc := range cases {
		got := resolveObject(res, tc.kind, ns, tc.name)
		if !got.Found {
			t.Errorf("resolveObject(%q, %q) Found = false, want true", tc.kind, tc.name)
			continue
		}
		if got.Kind != wantKind[tc.kind] {
			t.Errorf("resolveObject(%q, %q) Kind = %q, want %q",
				tc.kind, tc.name, got.Kind, wantKind[tc.kind])
		}
		// The same name under every other kind must not resolve.
		for _, other := range cases {
			if other.kind == tc.kind {
				continue
			}
			if resolveObject(res, other.kind, ns, tc.name).Found {
				t.Errorf("resolveObject(%q, %q) Found = true; %q is a %s",
					other.kind, tc.name, tc.name, wantKind[tc.kind])
			}
		}
		// A name nothing carries must not resolve under any kind.
		if resolveObject(res, tc.kind, ns, "ghost").Found {
			t.Errorf("resolveObject(%q, %q) Found = true for a name nothing carries", tc.kind, "ghost")
		}
	}
}
```

Add to `internal/mcp/inspect_test.go`'s import block: `batchv1
"k8s.io/api/batch/v1"`, `"github.com/imantaba/kubeagent/internal/inventory"`
and `"github.com/imantaba/kubeagent/internal/scan"`. `appsv1`, `corev1`,
`metav1` and `fake` are already imported there.

- [ ] **Step 2: Run them and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestInspect_HealthyDeploymentIsFound|TestResolveObject_' -count=1
```

Expected: **build failure**, `undefined: resolveObject`. Report the exact
message.

- [ ] **Step 3: Add the resolver**

In `internal/mcp/inspect.go`, add `"github.com/imantaba/kubeagent/internal/diagnose"`
to the import block (it is already imported by `view.go` in the same package;
Go imports are per-file). Then insert the three declarations below **after**
`registerInspect` and before `eventTime`:

```go
// resolved is the answer to an object lookup, separated from InspectOutput so
// the lookup is a pure function a test can call without standing up a server.
type resolved struct {
	Found    bool
	Kind     string
	Status   string
	Desired  int
	Ready    int
	Image    string
	Pods     []inventory.PodRow
	Findings []Finding
}

// findingsOf gathers every diagnose.Finding the scan attached to a workload.
// Re-attaching them to a recomputed workload set is lossless: Workload.Flagged()
// is true whenever len(Findings) > 0 and Prioritize never drops a flagged
// workload, so no finding can have been filtered out of the list this reads.
func findingsOf(ws []inventory.Workload) []diagnose.Finding {
	var out []diagnose.Finding
	for _, w := range ws {
		out = append(out, w.Findings...)
	}
	return out
}

// resolveObject answers "does this object exist, and what is its state" from
// the snapshot scan.Evaluate already returned. It issues no cluster call.
//
// It deliberately does not read res.Inventory.Workloads. That list is
// inventory.Prioritize's output, built for display: it drops healthy-quiet
// workloads outright and Assemble truncates a Job's or CronJob's pod rows at
// jobPodCap. Answering a lookup against it made inspect report found:false for
// objects the cluster plainly had. res.Inputs is the raw snapshot, and its seven
// collections are exactly the seven values in inspectKinds.
//
// The canonical Kind spelling comes from the recomputed workload, never from the
// object: typed client-go objects leave TypeMeta empty. The requested kind is
// matched case-insensitively, as it has always been — the published enum admits
// only the seven lowercase spellings, but this is a pure function a test may
// call with any string, and it must not answer found:false for "Pod" when the
// caller meant "pod".
func resolveObject(res scan.Result, kind, namespace, name string) resolved {
	for _, w := range inventory.Assemble(res.Inputs, findingsOf(res.Inventory.Workloads)) {
		if w.Namespace != namespace || w.Name != name || !strings.EqualFold(w.Kind, kind) {
			continue
		}
		out := resolved{
			Found: true, Kind: w.Kind, Status: w.Status,
			Desired: w.Desired, Ready: w.Ready, Image: w.Image,
		}
		out.Pods = append(out.Pods, w.Pods...)
		// Mirrors findingsFromResult in view.go: a workload can be Flagged()
		// (Ready < Desired, or a bare Failed pod) with no per-pod
		// diagnose.Finding attached — a Deployment stuck at 0/3 ready with no
		// crash-looping pod, for instance. Projecting only w.Findings would
		// report "findings: []" for an object triage already flags; fromWorkload
		// is the same helper triage uses, so the two surfaces agree.
		if len(w.Findings) == 0 {
			if w.Flagged() {
				out.Findings = append(out.Findings, fromWorkload(w))
			}
		} else {
			for _, f := range w.Findings {
				out.Findings = append(out.Findings, fromDiagnose(f))
			}
		}
		return out
	}
	return resolved{}
}
```

- [ ] **Step 4: Call it from the handler**

In `registerInspect`, replace the whole `for _, w := range res.Inventory.Workloads`
loop (lines 121-150, comment included — it moved into `resolveObject`) with:

```go
			if r := resolveObject(res, in.Kind, in.Namespace, in.Name); r.Found {
				out.Found = true
				out.Kind = r.Kind
				out.Status = r.Status
				out.Desired = r.Desired
				out.Ready = r.Ready
				out.Image = r.Image
				out.Pods = append(out.Pods, r.Pods...)
				out.Findings = append(out.Findings, r.Findings...)
			}
```

Nothing else in the handler changes. `out.Kind` keeps its pre-set value —
the caller's own lowercase spelling — on a `found: false` result, exactly as
before.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestInspect_HealthyDeploymentIsFound|TestResolveObject_' -count=1 -v
```

Expected: both PASS.

- [ ] **Step 6: Prove the reported defect is still open**

This task fixes the healthy-workload half only. Write nothing; just confirm
the pod half still fails, so Task 3 has something to fix. Run the whole
package:

```bash
go test -p 2 -count=1 ./internal/mcp/...
```

Expected: `ok` — every existing test still passes, including
`TestInspect_PodReturnsItsFindingsAndEvents` (a **bare** pod, which `Assemble`
seeds as its own `"Pod"` workload). Report that it passed.

- [ ] **Step 7: Falsify the resolver test**

Temporarily drop the kind check in `resolveObject`:

```go
		if w.Namespace != namespace || w.Name != name {
```

```bash
go test ./internal/mcp -run TestResolveObject_ -count=1
```

Expected: **FAIL**, with a `Found = true; ... is a ...` message from the
negative half. Report the exact output. Then **revert** the line to include
`|| !strings.EqualFold(w.Kind, kind)` and run again:

```bash
go test ./internal/mcp -run TestResolveObject_ -count=1
```

Expected: PASS.

- [ ] **Step 8: Full suite and constraint check**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: `gofmt -l` prints nothing; every package `ok`; the final
`git diff --stat` prints **nothing**.

- [ ] **Step 9: Commit**

```bash
git add internal/mcp/inspect.go internal/mcp/inspect_test.go
git commit -s -m "fix(mcp): resolve an inspect lookup against the snapshot, not the display list

kubeagent_inspect matched the requested object against
res.Inventory.Workloads -- inventory.Prioritize's output, a list built for
display. Prioritize drops healthy-quiet workloads outright, so inspecting a
Deployment that was fully ready with no restarts answered found:false for an
object the cluster plainly had.

resolveObject answers existence from res.Inputs instead: the raw snapshot
scan.Evaluate already collected, whose seven collections are exactly the
seven values in the tool's published enum. It recomputes the unfiltered
workload set in memory and re-attaches the scan's findings, which is
lossless -- Flagged() is true whenever a workload has one, and Prioritize
never drops a flagged workload. No new cluster read, and the resolver is a
pure function tested directly.

A controller-owned pod is still not resolvable; that is the next commit."
```

---

## Task 3: Resolve a pod as itself, and name its owner

**Files:**
- Modify: `internal/mcp/inspect.go` (add `Owner` to `InspectOutput` and to
  `resolved`; add `resolvePod`; add the `now time.Time` parameter to
  `resolveObject`; set `out.Owner` in the handler)
- Test: `internal/mcp/inspect_test.go` (append)

**Interfaces:**
- Consumes: `inventory.PodRowFor(p corev1.Pod, now time.Time) PodRow` (Task 1);
  `resolved`, `resolveObject`, `findingsOf`, `ctrl`, `ownedPod` (Task 2).
- Produces: `func resolvePod(res scan.Result, namespace, name string, now time.Time) resolved`,
  and the changed signature
  `func resolveObject(res scan.Result, kind, namespace, name string, now time.Time) resolved`.
  Every Task 2 call site in the tests must be updated to pass `now` — that is
  the one edit Task 3 makes to Task 2's tests, and it must change nothing else
  about them.

**This is the reported defect.** `kubeagent_triage` emits `Kind: "Pod"` with the
pod's own name for every critical finding, and on the tested cluster that was
six of six findings, none of them inspectable.

- [ ] **Step 1: Write the four failing tests**

Append to `internal/mcp/inspect_test.go`:

```go
// callInspectRaw returns the tool's structured result as a decoded JSON object,
// so a test can assert which keys are *present*. InspectOutput cannot answer
// that: after unmarshalling, an omitempty field that was omitted and one that
// encoded its zero value are the same value.
func callInspectRaw(t *testing.T, cs *mcpsdk.ClientSession, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "kubeagent_inspect", Arguments: args,
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool() returned an error result: %+v", res.Content)
	}
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("Marshal(structured) error = %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return out
}

// crashingOwnedPod is crashingPod's controller-owned twin: the shape that
// actually occurs in a cluster. PodOwners rolls it up to its Deployment, so it
// is never a "Pod" workload in its own right.
func crashingOwnedPod(ns, name, rs string) *corev1.Pod {
	p := ownedPod(ns, name, ctrl("ReplicaSet", rs))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "app",
		RestartCount: 7,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CrashLoopBackOff", Message: "back-off restarting failed container",
		}},
	}}
	return p
}

// TestInspect_ControllerOwnedPodIsFoundAndNamesItsOwner is the reported defect.
// kubeagent_triage emits Kind:"Pod" with the pod's own name for every critical
// finding, and the shipped skill tells the model to inspect it with exactly the
// namespace and name the finding supplied. Every controller-owned pod answered
// found:false, because a lookup was being answered against a workload list a
// controller-owned pod is never in.
func TestInspect_ControllerOwnedPodIsFoundAndNamesItsOwner(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
	}}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, crashingOwnedPod("shop", "web-1a2b-cdef", "web-1a2b")))

	out := callInspect(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "web-1a2b-cdef",
	})

	if !out.Found {
		t.Fatal("Found = false for a controller-owned pod that exists; this is the " +
			"identity kubeagent_triage hands out for every critical finding")
	}
	if out.Kind != "Pod" {
		t.Errorf("Kind = %q, want %q — a caller who asked about a pod must not be "+
			"answered about its Deployment", out.Kind, "Pod")
	}
	if out.Status != "Running" {
		t.Errorf("Status = %q, want the pod's own phase %q", out.Status, "Running")
	}
	if out.Owner != "Deployment/web" {
		t.Errorf("Owner = %q, want %q — the escalation pointer, so a caller need not "+
			"guess the owning workload's name", out.Owner, "Deployment/web")
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Fatalf("Pods = %+v, want exactly the one pod asked about", out.Pods)
	}
	if out.Pods[0].Restarts != 7 {
		t.Errorf("Pods[0].Restarts = %d, want 7", out.Pods[0].Restarts)
	}
	if len(out.Findings) != 1 {
		t.Fatalf("Findings = %+v, want the pod's own critical finding", out.Findings)
	}
	f := out.Findings[0]
	if f.Severity != "critical" || f.Kind != "Pod" || f.Name != "web-1a2b-cdef" {
		t.Errorf("Findings[0] = %+v, want a critical finding on the pod itself", f)
	}
}

// TestInspect_PodAnswerHasNoDesiredOrReadyKey guards the rule that absence must
// never read as zero. A pod has no replica count; encoding desired:0 ready:0
// would tell a model the pod is scaled to nothing.
func TestInspect_PodAnswerHasNoDesiredOrReadyKey(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(ownedPod("shop", "loner", nil)))

	raw := callInspectRaw(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "loner",
	})

	if raw["found"] != true {
		t.Fatalf("found = %v, want true (result: %+v)", raw["found"], raw)
	}
	for _, key := range []string{"desired", "ready"} {
		if v, ok := raw[key]; ok {
			t.Errorf("a pod answer carries %q = %v; both are omitempty and a pod has "+
				"no replica count, so absence must never render as zero", key, v)
		}
	}
}

// TestInspect_BarePodHasNoOwnerKey — a pod with no controller must not claim an
// owner it does not have.
func TestInspect_BarePodHasNoOwnerKey(t *testing.T) {
	cs := connect(t, Config{Context: "kind-example"},
		fake.NewSimpleClientset(ownedPod("shop", "loner", nil)))

	raw := callInspectRaw(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "loner",
	})

	if v, ok := raw["owner"]; ok {
		t.Errorf("owner = %v on a bare pod, want the key absent", v)
	}
}

// TestInspect_JobPodPastTheCapIsStillInspectable covers the third way the old
// lookup lost an object that exists: Assemble truncates a Job's or CronJob's pod
// rows at jobPodCap (3), which is right for a report and wrong for a lookup. The
// pod path reads res.Inputs.Pods directly, so the fourth pod is findable.
func TestInspect_JobPodPastTheCapIsStillInspectable(t *testing.T) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "migrate"}}
	objs := []runtime.Object{job}
	for _, n := range []string{"migrate-1", "migrate-2", "migrate-3", "migrate-4"} {
		objs = append(objs, ownedPod("shop", n, ctrl("Job", "migrate")))
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(objs...))

	// The owning Job's own answer is capped at three rows, by design.
	viaJob := callInspect(t, cs, map[string]any{"kind": "job", "namespace": "shop", "name": "migrate"})
	if len(viaJob.Pods) != 3 {
		t.Fatalf("the Job answer carries %d pod rows, want jobPodCap (3) — the fixture "+
			"no longer exercises truncation", len(viaJob.Pods))
	}

	out := callInspect(t, cs, map[string]any{
		"kind": "pod", "namespace": "shop", "name": "migrate-4",
	})
	if !out.Found {
		t.Fatal("Found = false for a Job's fourth pod; a display cap must not hide an " +
			"object from a lookup")
	}
	if out.Owner != "Job/migrate" {
		t.Errorf("Owner = %q, want %q", out.Owner, "Job/migrate")
	}
}
```

`runtime` (`"k8s.io/apimachinery/pkg/runtime"`) is already imported in
`inspect_test.go` (line 14). `batchv1` was added by Task 2.

- [ ] **Step 2: Run them and watch the reported defect fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestInspect_ControllerOwnedPod|TestInspect_PodAnswerHasNo|TestInspect_BarePodHasNoOwner|TestInspect_JobPodPastTheCap' -count=1
```

Expected: **build failure** first — `out.Owner undefined (type InspectOutput
has no field or method Owner)`. Add only the `Owner` field (Step 3's first
edit), re-run, and this time expect a genuine **FAIL**:

- `TestInspect_ControllerOwnedPodIsFoundAndNamesItsOwner`: `Found = false for a
  controller-owned pod that exists` — **this is the reported defect,
  reproduced.**
- `TestInspect_JobPodPastTheCapIsStillInspectable`: `Found = false for a Job's
  fourth pod`.
- `TestInspect_PodAnswerHasNoDesiredOrReadyKey`: `found = false, want true`.
- `TestInspect_BarePodHasNoOwnerKey`: passes already (a bare pod resolves via
  `Assemble`'s `"Pod"` workload and has no owner) — say so; it is a guard
  against a regression Step 4 could introduce, not a gap.

Report what each of the four actually printed. **Never** manufacture a red run
or claim a failure you did not observe.

- [ ] **Step 3: Add the `Owner` field**

In `InspectOutput`, after `Image`:

```go
	// Owner names the controller that owns this pod, as "Deployment/web". It is
	// the escalation pointer: a critical finding names a pod, and this is how a
	// caller learns which workload to inspect next without guessing its name.
	// Absent for a bare pod, which has no controller, and for every kind other
	// than pod.
	Owner string `json:"owner,omitempty"`
```

And the same field on `resolved`, after `Image`:

```go
	Owner    string
```

- [ ] **Step 4: Add the pod path**

Add `now time.Time` as `resolveObject`'s last parameter, and make the pod
branch its first statement:

```go
func resolveObject(res scan.Result, kind, namespace, name string, now time.Time) resolved {
	if strings.EqualFold(kind, "pod") {
		return resolvePod(res, namespace, name, now)
	}
	for _, w := range inventory.Assemble(res.Inputs, findingsOf(res.Inventory.Workloads)) {
```

Then add `resolvePod` immediately after `resolveObject`:

```go
// resolvePod answers a pod lookup from res.Inputs.Pods directly rather than
// through the owning workload's Pods slice. Two reasons, and both are defects
// the old lookup had: a controller-owned pod is never a workload in its own
// right (inventory.PodOwners assigns kind "Pod" only when the pod has no
// controller owner), and Assemble truncates a Job's or CronJob's rows at
// jobPodCap — right for a report, wrong for a lookup.
//
// The answer describes the pod. A caller who asked about a pod and got its
// Deployment's replica counts back would have been answered a different
// question, so Desired and Ready are left unset: both are omitempty, so they
// leave the JSON rather than rendering as 0/0, and absence must never read as
// zero. Image is taken off the row rather than recomputed, so a pod answer and
// a workload answer report the same field the same way.
//
// There is no fromWorkload fallback here. fromWorkload describes a workload's
// ready-versus-desired; synthesising one for a pod would invent a finding no
// detector emitted, and an unready pod is already visible in its row's ready
// field.
func resolvePod(res scan.Result, namespace, name string, now time.Time) resolved {
	key := namespace + "/" + name
	for _, p := range res.Inputs.Pods {
		if p.Namespace != namespace || p.Name != name {
			continue
		}
		row := inventory.PodRowFor(p, now)
		out := resolved{
			Found: true,
			// "Pod" comes from here, never from the object: typed client-go
			// objects leave TypeMeta empty.
			Kind:   "Pod",
			Status: row.Phase,
			Image:  row.Image,
			Pods:   []inventory.PodRow{row},
		}
		if o := inventory.PodOwners(res.Inputs)[key]; o.Kind != "Pod" {
			out.Owner = o.Kind + "/" + o.Name
		}
		for _, f := range findingsOf(res.Inventory.Workloads) {
			if f.Pod == key {
				out.Findings = append(out.Findings, fromDiagnose(f))
			}
		}
		return out
	}
	return resolved{}
}
```

In the handler, pass the clock and carry `Owner` across:

```go
			if r := resolveObject(res, in.Kind, in.Namespace, in.Name, now()); r.Found {
				out.Found = true
				out.Kind = r.Kind
				out.Status = r.Status
				out.Desired = r.Desired
				out.Ready = r.Ready
				out.Image = r.Image
				out.Owner = r.Owner
				out.Pods = append(out.Pods, r.Pods...)
				out.Findings = append(out.Findings, r.Findings...)
			}
```

Finally, update Task 2's `TestResolveObject_FindsEachKindUnderItsOwnKindOnly`
so its four `resolveObject(...)` calls pass a clock. Add
`now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)` at the top of the test
and append `, now` to each call. **Change nothing else about that test** — not
its name, not its table, not its comment.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestInspect_|TestResolveObject_' -count=1 -v
```

Expected: every test PASSES, including the pre-existing
`TestInspect_PodReturnsItsFindingsAndEvents` (the bare pod now resolves through
`resolvePod` instead of through `Assemble`, and must still report its finding
and its events) and
`TestInspect_MissingObjectIsNotFoundNotAnError` (a `found: false` result still
carries empty lists and its events).

- [ ] **Step 6: Falsify the owner guard**

Temporarily drop the bare-pod guard in `resolvePod`:

```go
		o := inventory.PodOwners(res.Inputs)[key]
		out.Owner = o.Kind + "/" + o.Name
```

```bash
go test ./internal/mcp -run TestInspect_BarePodHasNoOwnerKey -count=1
```

Expected: **FAIL** — `owner = Pod/loner on a bare pod, want the key absent`.
Report the exact output. Then **revert** to the `if o := ...; o.Kind != "Pod"`
form and run again:

```bash
go test ./internal/mcp -run TestInspect_BarePodHasNoOwnerKey -count=1
```

Expected: PASS.

- [ ] **Step 7: Full suite and constraint check**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: `gofmt -l` prints nothing; every package `ok` (including the root
package's `plugin_manifest_test.go`); the final `git diff --stat` prints
**nothing**.

- [ ] **Step 8: Commit**

```bash
git add internal/mcp/inspect.go internal/mcp/inspect_test.go
git commit -s -m "fix(mcp): inspect resolves a pod as itself and names its controller

kubeagent_triage emits Kind:\"Pod\" with the pod's own name for every
critical finding, and the shipped skill tells the model to inspect it with
exactly the namespace and name the finding supplied. That call answered
found:false for every controller-owned pod: inventory.PodOwners assigns a
pod kind \"Pod\" only when it has no controller owner, so a ReplicaSet-owned
pod rolls up to its Deployment and is never a workload in its own right.
On a real cluster that was six of six critical findings.

resolvePod looks the pod up in res.Inputs.Pods directly, which also makes a
Job's fourth pod findable -- Assemble truncates a Job's rows at jobPodCap,
right for a report and wrong for a lookup. The answer describes the pod:
kind Pod, the pod's phase, one row, and only the findings whose Pod key
matches. Desired and Ready stay unset so absence cannot read as 0/0, and a
new omitempty owner field names the controller, so escalating to the
workload needs no guess at its name. A bare pod gets no owner key."
```

---

## Task 4: Resolve a ReplicaSet from the snapshot, not from a pod's owner

**Files:**
- Modify: `internal/inventory/inventory.go` (rename `workloadStatus` →
  `WorkloadStatus`, one call site at line 429)
- Modify: `internal/inventory/inventory_test.go` (three call sites at lines 103,
  106, 121, and one comment at line 692 — mechanical rename only)
- Modify: `internal/mcp/inspect.go` (add `resolveReplicaSet` and
  `ownedByReplicaSet`; add the `replicaset` branch to `resolveObject`)
- Test: `internal/mcp/inspect_test.go` (append)

**Interfaces:**
- Consumes: `resolved`, `resolveObject`, `findingsOf`, `ctrl`, `ownedPod`,
  `p32` (Task 2); `crashingOwnedPod` (Task 3); the
  `resolveObject(res, kind, namespace, name string, now time.Time)` signature
  (Task 3).
- Produces: `func inventory.WorkloadStatus(ready, desired int) string`;
  `func resolveReplicaSet(res scan.Result, namespace, name string, now time.Time) resolved`;
  `func ownedByReplicaSet(p corev1.Pod, name string) bool`.

**Why this task exists — the third facet of the same defect.** `inspectKinds`
advertises `replicaset`, and Task 2 resolved it only by accident. `Assemble`
seeds a workload from `Inputs` for Deployment, StatefulSet, DaemonSet, CronJob
and non-CronJob-owned Job — and **not** for ReplicaSet: read
`internal/inventory/inventory.go:335-375` and there is no
`for _, rs := range in.ReplicaSets` loop. A ReplicaSet workload materialises
only as a side effect of a pod whose `PodOwners` result is `ReplicaSet`, and
`PodOwners` returns `ReplicaSet` only when the ReplicaSet has no controller
owner of its own. So two ReplicaSets that plainly exist answered `found: false`:

1. **Every Deployment-owned ReplicaSet** — the common case, since that is what
   a Deployment's rollouts are made of. Its pods roll up to the Deployment, so
   no `ReplicaSet` workload is ever built.
2. **A ReplicaSet with no pods** — an old revision scaled to zero, which is
   exactly the object an operator asks about during a rollout.

Task 2's fixture had to give `orphan-rs` a pod for its case to pass at all.
That was the right call for Task 2 and it is the evidence for this task.

`Assemble` is **not** the place to fix this. It is shared with `scan`'s text
report, and `internal/report/testdata/golden-scan.txt` must stay byte-identical;
seeding ReplicaSets there would add a workload row to every report. The fix
belongs in the resolver, alongside `resolvePod`, for the same reason: these are
the two kinds `Assemble` does not seed.

**Deliberately NOT in this task:** a ReplicaSet answer sets **no** `Owner`.
Task 3 documents `owner` as present for a pod and absent "for every kind other
than `pod`", and that stays true. A caller who asked about a ReplicaSet named it
themselves; the pod path needs `owner` because `kubeagent_triage` hands out a
pod identity the caller did not choose. Do not add it.

- [ ] **Step 1: Export `WorkloadStatus`**

A ReplicaSet answer needs the same `Scaled Down` / `Running` / `Degraded`
vocabulary every other kind reports, and `workloadStatus` is unexported.
Rename it rather than adding a second name for one rule. In
`internal/inventory/inventory.go` at line 180:

```go
// WorkloadStatus renders a ready-versus-desired pair as the one status
// vocabulary every kubeagent surface uses. Assemble sets it for a workload it
// grouped; internal/mcp's inspect handler calls it for a ReplicaSet it looked
// up directly, so a ReplicaSet's status word means the same thing as a
// Deployment's.
func WorkloadStatus(ready, desired int) string {
	if desired == 0 {
		return "Scaled Down"
	}
	if ready >= desired {
		return "Running"
	}
	return "Degraded"
}
```

Then update the one production call site (line 429, inside `Assemble`) and the
three test call sites (`internal/inventory/inventory_test.go` lines 103, 106,
121) plus the comment at line 692. This is a mechanical rename: change nothing
else about those tests, and add nothing to them.

```bash
export PATH=$PATH:/usr/local/go/bin
grep -rn 'workloadStatus' --include='*.go' .    # must print nothing
go build ./... && go test ./internal/inventory -count=1
```

Expected: `grep` prints nothing; `internal/inventory` is `ok`.

- [ ] **Step 2: Write the three failing tests**

Append to `internal/mcp/inspect_test.go`:

There is **no** `scan.Result` fixture helper in this package: Task 2's
`TestResolveObject_FindsEachKindUnderItsOwnKindOnly` builds
`res := scan.Result{Inputs: in}` by hand from an `inventory.Inputs` literal
(`internal/mcp/inspect_test.go:356`), which carries no findings because
`res.Inventory.Workloads` is empty. Do not invent a helper. The first test below
needs real findings, so it goes through the tool with `connect` + `callInspect`
exactly as Task 3's defect test does; the other two need no findings and call
`resolveObject` directly over a hand-built `Inputs`.

```go
// TestInspect_DeploymentOwnedReplicaSetIsFound is the common case and the third
// facet of the reported defect. A Deployment's ReplicaSets are what its
// rollouts are made of, and every one of them answered found:false: Assemble
// never seeds a ReplicaSet from Inputs.ReplicaSets, and PodOwners rolls a
// Deployment-owned ReplicaSet's pods up to the Deployment, so no ReplicaSet
// workload is ever built for it. It runs through the tool rather than calling
// the resolver directly, because it asserts the pod's finding and a finding
// exists only after a real scan.
func TestInspect_DeploymentOwnedReplicaSetIsFound(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: p32(1)},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "shop", Name: "web-1a2b", OwnerReferences: ctrl("Deployment", "web"),
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: p32(1)},
		Status: appsv1.ReplicaSetStatus{ReadyReplicas: 0},
	}
	cs := connect(t, Config{Context: "kind-example"}, fake.NewSimpleClientset(
		dep, rs, crashingOwnedPod("shop", "web-1a2b-cdef", "web-1a2b")))

	out := callInspect(t, cs, map[string]any{
		"kind": "replicaset", "namespace": "shop", "name": "web-1a2b",
	})

	if !out.Found {
		t.Fatal("Found = false for a Deployment-owned ReplicaSet that exists; " +
			"Assemble never seeds one, so the resolver must read Inputs.ReplicaSets")
	}
	if out.Kind != "ReplicaSet" {
		t.Errorf("Kind = %q, want %q", out.Kind, "ReplicaSet")
	}
	if out.Desired != 1 || out.Ready != 0 {
		t.Errorf("Desired/Ready = %d/%d, want 1/0 — taken from the ReplicaSet's own "+
			"spec and status", out.Desired, out.Ready)
	}
	if out.Status != "Degraded" {
		t.Errorf("Status = %q, want %q", out.Status, "Degraded")
	}
	if len(out.Pods) != 1 || out.Pods[0].Name != "web-1a2b-cdef" {
		t.Fatalf("Pods = %+v, want exactly the ReplicaSet's own one pod", out.Pods)
	}
	if len(out.Findings) != 1 || out.Findings[0].Name != "web-1a2b-cdef" {
		t.Errorf("Findings = %+v, want the crash finding on its own pod", out.Findings)
	}
	if out.Owner != "" {
		t.Errorf("Owner = %q, want empty — owner is the pod path's escalation "+
			"pointer and is documented as absent for every other kind", out.Owner)
	}
}

// TestResolveReplicaSet_WithNoPodsIsFound covers the second unresolvable
// ReplicaSet: an old revision scaled to zero. It has no pods, so nothing can
// materialise a workload for it — and it is exactly the object an operator asks
// about mid-rollout.
func TestResolveReplicaSet_WithNoPodsIsFound(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	res := scan.Result{Inputs: inventory.Inputs{
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-0old"},
			Spec:       appsv1.ReplicaSetSpec{Replicas: p32(0)},
		}},
	}}

	got := resolveObject(res, "replicaset", "shop", "web-0old", now)

	if !got.Found {
		t.Fatal("Found = false for a ReplicaSet scaled to zero; a lookup must not " +
			"need the object to have pods")
	}
	if got.Status != "Scaled Down" {
		t.Errorf("Status = %q, want %q", got.Status, "Scaled Down")
	}
	if len(got.Pods) != 0 {
		t.Errorf("Pods = %+v, want none", got.Pods)
	}
}

// TestResolveReplicaSet_CarriesOnlyItsOwnPods pins the owner filter. Two
// revisions of one Deployment run side by side during a rollout; asking about
// one must not return the other's pods.
func TestResolveReplicaSet_CarriesOnlyItsOwnPods(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	res := scan.Result{Inputs: inventory.Inputs{
		Deployments: []appsv1.Deployment{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec:       appsv1.DeploymentSpec{Replicas: p32(2)},
		}},
		ReplicaSets: []appsv1.ReplicaSet{
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-new",
					OwnerReferences: ctrl("Deployment", "web")},
				Spec: appsv1.ReplicaSetSpec{Replicas: p32(1)},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-old",
					OwnerReferences: ctrl("Deployment", "web")},
				Spec: appsv1.ReplicaSetSpec{Replicas: p32(1)},
			},
		},
		Pods: []corev1.Pod{
			*ownedPod("shop", "web-new-aaaa", ctrl("ReplicaSet", "web-new")),
			*ownedPod("shop", "web-old-bbbb", ctrl("ReplicaSet", "web-old")),
		},
	}}

	got := resolveObject(res, "replicaset", "shop", "web-new", now)

	if len(got.Pods) != 1 || got.Pods[0].Name != "web-new-aaaa" {
		t.Fatalf("Pods = %+v, want only web-new's own pod — the other revision's "+
			"pod belongs to web-old", got.Pods)
	}
}
```

`appsv1`, `corev1`, `metav1`, `fake`, `scan`, `inventory` and `time` are all
already imported in `inspect_test.go`.

- [ ] **Step 3: Run them and watch the defect fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run TestResolveReplicaSet -count=1
```

Expected: **FAIL** on the first two —
`Found = false for a Deployment-owned ReplicaSet that exists` and
`Found = false for a ReplicaSet scaled to zero`. The third
(`CarriesOnlyItsOwnPods`) also fails, on `Pods = [], want only web-new's own
pod`, because nothing resolves at all yet. Report the exact output of all
three. **Never** manufacture a red run or claim a failure you did not observe.

- [ ] **Step 4: Add the ReplicaSet path**

Add the branch as `resolveObject`'s second statement, immediately after the pod
branch Task 3 added:

```go
	if strings.EqualFold(kind, "replicaset") {
		return resolveReplicaSet(res, namespace, name, now)
	}
```

Then add both functions immediately after `resolvePod`:

```go
// resolveReplicaSet answers a ReplicaSet lookup from res.Inputs.ReplicaSets
// directly. inventory.Assemble seeds a workload from Inputs for a Deployment,
// StatefulSet, DaemonSet, CronJob and Job, but not for a ReplicaSet: one
// materialises only as a side effect of a pod whose PodOwners result is
// ReplicaSet, and PodOwners returns that only for a ReplicaSet with no
// controller owner of its own. So a Deployment-owned ReplicaSet — what every
// rollout is made of — and a ReplicaSet scaled to zero both had no workload to
// be found in, even though inspectKinds advertises the kind.
//
// Desired and Ready come from the ReplicaSet's own spec and status rather than
// from a pod count, so a scaled-to-zero revision reads as Scaled Down rather
// than as a workload with no pods. Image comes off the first pod row, which is
// how Assemble reports it for every other kind; a ReplicaSet with no pods
// carries no image, and the field is omitempty.
//
// No Owner is set. Owner is the pod path's escalation pointer, for the pod
// identity kubeagent_triage hands a caller who did not choose it; a caller who
// asked about a ReplicaSet named it themselves.
func resolveReplicaSet(res scan.Result, namespace, name string, now time.Time) resolved {
	for _, rs := range res.Inputs.ReplicaSets {
		if rs.Namespace != namespace || rs.Name != name {
			continue
		}
		desired := 0
		if rs.Spec.Replicas != nil {
			desired = int(*rs.Spec.Replicas)
		}
		ready := int(rs.Status.ReadyReplicas)
		out := resolved{
			Found: true,
			// "ReplicaSet" comes from here, never from the object: typed
			// client-go objects leave TypeMeta empty.
			Kind:    "ReplicaSet",
			Status:  inventory.WorkloadStatus(ready, desired),
			Desired: desired,
			Ready:   ready,
		}
		mine := map[string]bool{}
		for _, p := range res.Inputs.Pods {
			if p.Namespace != namespace || !ownedByReplicaSet(p, name) {
				continue
			}
			mine[p.Namespace+"/"+p.Name] = true
			out.Pods = append(out.Pods, inventory.PodRowFor(p, now))
		}
		if len(out.Pods) > 0 {
			out.Image = out.Pods[0].Image
		}
		for _, f := range findingsOf(res.Inventory.Workloads) {
			if mine[f.Pod] {
				out.Findings = append(out.Findings, fromDiagnose(f))
			}
		}
		return out
	}
	return resolved{}
}

// ownedByReplicaSet reports whether p's controller owner is the named
// ReplicaSet. inventory.PodOwners cannot answer this: it deliberately rolls a
// ReplicaSet-owned pod up to the Deployment above it, which is right for a
// report grouped by workload and wrong when the caller named the ReplicaSet.
func ownedByReplicaSet(p corev1.Pod, name string) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" && o.Name == name {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/mcp -run 'TestResolveReplicaSet|TestResolveObject_|TestInspect_' -count=1 -v
```

Expected: every test PASSES, including Task 2's
`TestResolveObject_FindsEachKindUnderItsOwnKindOnly`.

That test needs **no** edit. Its `orphan-rs` case now resolves through
`resolveReplicaSet` rather than through `Assemble`, and the fixture has no
`Spec.Replicas`, so `Desired` becomes 0 and `Status` becomes `Scaled Down`
instead of the `Degraded` a pod-derived workload produced — but the test asserts
only `Found` and `Kind` for each case
(`internal/mcp/inspect_test.go:376-399`), and both still hold. Do not change its
name, its table, its assertions or its fixture.

- [ ] **Step 5a: Narrow Task 2's now-stale fixture comment**

Task 2's fixture carries a comment (`internal/mcp/inspect_test.go:348-353`)
ending:

```go
			// owner resolves to one. A ReplicaSet with no pods, and every
			// Deployment-owned ReplicaSet, are both still unresolvable here.
```

Both of those are resolvable as of this task, so that sentence is now false —
the same class of defect as a doc comment that overclaims, in the opposite
direction. Replace those two lines with:

```go
			// owner resolves to one, so this pod is what puts orphan-rs in
			// Assemble's output at all. resolveReplicaSet no longer depends on
			// that: it reads Inputs.ReplicaSets directly, which is what makes a
			// pod-less or Deployment-owned ReplicaSet resolvable too.
```

Keep the first three lines of that comment ("orphan-rs needs a pod to exist as a
workload at all: Assemble seeds Deployment, StatefulSet, DaemonSet, CronJob and
Job directly from Inputs, but a ReplicaSet workload only materialises from a pod
whose") exactly as they are. This is the one edit to Task 2's test file this task
makes, and it changes no assertion.

- [ ] **Step 6: Falsify the owner filter**

Temporarily drop the name check in `ownedByReplicaSet`:

```go
		if o.Controller != nil && *o.Controller && o.Kind == "ReplicaSet" {
```

```bash
go test ./internal/mcp -run TestResolveReplicaSet_CarriesOnlyItsOwnPods -count=1
```

Expected: **FAIL** — `Pods = [...two rows...], want only web-new's own pod`.
Report the exact output. Then **revert** to the `&& o.Name == name` form and run
again:

```bash
go test ./internal/mcp -run TestResolveReplicaSet -count=1
```

Expected: PASS.

- [ ] **Step 7: Full suite and constraint check**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: `gofmt -l` prints nothing; every package `ok` — in particular
`internal/inventory`, `internal/report` and `internal/scan`, which the
`WorkloadStatus` rename touches; the final `git diff --stat` prints **nothing**,
so the golden scan output is byte-identical and no schema moved. **Do not** run
any test with `-update`.

- [ ] **Step 8: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go \
        internal/mcp/inspect.go internal/mcp/inspect_test.go
git commit -s -m "fix(mcp): inspect resolves a ReplicaSet from the snapshot

inspectKinds advertises replicaset, and two ReplicaSets that plainly exist
answered found:false. inventory.Assemble seeds a workload from Inputs for a
Deployment, StatefulSet, DaemonSet, CronJob and Job, but never for a
ReplicaSet -- one materialises only as a side effect of a pod whose
PodOwners result is ReplicaSet, which happens only when the ReplicaSet has
no controller owner of its own. So every Deployment-owned ReplicaSet, which
is what a rollout is made of, and every ReplicaSet scaled to zero were
unresolvable.

resolveReplicaSet reads res.Inputs.ReplicaSets directly, alongside
resolvePod: these are the two kinds Assemble does not seed. Desired and
Ready come from the ReplicaSet's own spec and status, so an old revision
reads as Scaled Down rather than as a workload with no pods, and its pods
are filtered by controller owner reference rather than through PodOwners,
which rolls them up to the Deployment above. Assemble is untouched -- it is
shared with the text report, and golden-scan.txt is byte-identical.

workloadStatus becomes exported WorkloadStatus so a ReplicaSet's status word
means the same thing as a Deployment's, rather than a second copy of the
rule."
```

---

## Task 5: The tool description and the three shipped plugin documents

**Files:**
- Modify: `internal/mcp/inspect.go:60-61` (the tool `Description`)
- Modify: `skills/triaging-a-cluster/SKILL.md:100-106`
- Modify: `commands/triage.md:15-22`
- Modify: `skills/reading-kubeagent-findings/SKILL.md:27-28`

**Interfaces:**
- Consumes: the `owner` field from Task 3. No Go behaviour changes here beyond
  a string literal.
- Produces: nothing later tasks depend on.

**Why:** the model is told before it calls. `plugin_manifest_test.go`'s
`TestShippedDocsNameOnlyRegisteredTools` parses these files for tool names, so
every edit must keep naming only registered tools —
`kubeagent_triage`, `kubeagent_inspect`, `kubeagent_advisory`,
`list_contexts`.

- [ ] **Step 1: Widen the tool description**

Replace `internal/mcp/inspect.go` lines 60-61:

```go
		Description: "Inspect one workload or pod: its status, its pods, kubeagent's findings for it, and " +
			"its recent Kubernetes events. Read-only: this never changes cluster state.",
```

with:

```go
		Description: "Inspect one workload or pod: its status, its pods, kubeagent's findings for it, and " +
			"its recent Kubernetes events. Takes exactly seven kinds — pod, deployment, statefulset, " +
			"daemonset, replicaset, job and cronjob — and no others. A pod answer describes the pod " +
			"and names the controller that owns it in the owner field. found:false means no such " +
			"object exists in that namespace; its events are still returned. Read-only: this never " +
			"changes cluster state.",
```

- [ ] **Step 2: Correct `skills/triaging-a-cluster/SKILL.md`**

Replace lines 100-106 — the paragraph beginning
"`kubeagent_inspect` takes seven kinds and no others" and ending "dismissed as
noise." — with:

```markdown
`kubeagent_inspect` takes seven kinds and no others: `pod`, `deployment`,
`statefulset`, `daemonset`, `replicaset`, `job`, `cronjob`.

A `critical` finding names a `Pod`, and a pod is directly inspectable: pass
`pod` with the finding's own `namespace` and `name`. The answer describes the
pod — its phase, its single row, its own findings — and names the controller
that owns it in `owner`, as `Deployment/web`. Inspect that workload next when
the question is about the workload rather than the pod; you do not have to
guess its name.

`found: false` means no object of that kind with that name exists in that
namespace. It is not a way of saying "healthy". The result still carries the
object's recent events, which is often the whole story for a pod that has since
been deleted.

Most of kubeagent's Service, Ingress, PVC, PodDisruptionBudget, HPA, webhook
configuration and ResourceQuota findings are `warning`s, and **none of those
seven can be inspected** — the call fails the schema. Report those findings from
the `reason` and `detail` they already carry, and inspect the workload behind
them if the user's question is about one. Do not skip them wholesale; that is
how a real problem gets dismissed as noise.
```

Leave lines 96-98 (the **Lowercase the kind first.** paragraph) exactly as they
are — that instruction is still correct and still necessary.

- [ ] **Step 3: Correct `commands/triage.md`**

In step 4, replace `PodDisruptionBudget, HPA or ResourceQuota finding is not,`
(line 20) with:

```markdown
   PodDisruptionBudget, HPA, webhook configuration or ResourceQuota finding is
   not,
```

so the whole of step 4 reads:

```markdown
4. Call `kubeagent_inspect` on every `critical` finding, and on each `warning`
   finding inside the namespace scope, passing that finding's `namespace` and
   `name` and its `kind` **lowercased** — a finding says `Pod`, the tool takes
   `pod`. Only `pod`, `deployment`, `statefulset`, `daemonset`, `replicaset`,
   `job` and `cronjob` are inspectable; a Service, Ingress, PVC,
   PodDisruptionBudget, HPA, webhook configuration or ResourceQuota finding is
   not, so report it from its own `reason` and `detail`. A pod answer names the
   controller that owns it in `owner` — inspect that workload next if the
   question is about the workload. `critical` and `warning` are the only two
   severities kubeagent emits.
```

- [ ] **Step 4: Correct the skipped-check count**

`skills/reading-kubeagent-findings/SKILL.md` lines 27-28 currently read:

```markdown
Seven checks are skipped on **every** `kubeagent_triage` call, and they fall
into two groups.
```

Replace with:

```markdown
Seven checks are skipped on **every** `kubeagent_triage` call, and they fall
into two groups. A server started **without** `--logs` skips an eighth,
`log-tails`. The shipped Claude Code plugin passes `--logs`, so seven is the
count for a plugin install and eight for a hand-configured server that omits
the flag.
```

The rest of that section — "Five are not reachable…", "The other two…" — still
adds to seven and is unchanged.

- [ ] **Step 5: Verify**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./... && gofmt -l internal/
go test -p 2 -count=1 ./... 
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/
```

Expected: `gofmt -l` prints nothing; every package `ok`, in particular the root
package (`plugin_manifest_test.go`'s `TestShippedDocsNameOnlyRegisteredTools`
and `TestPluginVersionMatchesChart`) and `internal/cli`
(`plugin_flags_test.go`); the final `git diff --stat` prints **nothing**.

Then confirm no document names an unregistered tool, and that no real
infrastructure identifier crept in:

```bash
grep -rn 'kubeagent_[a-z_]*\|list_contexts' skills/ commands/ | grep -v 'kubeagent_triage\|kubeagent_inspect\|kubeagent_advisory\|list_contexts' || echo "OK: only registered tools named"
```

- [ ] **Step 6: Commit**

```bash
git add internal/mcp/inspect.go skills/triaging-a-cluster/SKILL.md \
        skills/reading-kubeagent-findings/SKILL.md commands/triage.md
git commit -s -m "docs(plugin): name the seven inspectable kinds, webhooks among what is not

Three bounded corrections to what the shipped plugin tells a model.

The tool's own description now names the seven kinds it takes, says a pod
answer names its controller in owner, and says found:false means no such
object exists rather than anything about health -- so the model learns the
limit before a schema rejection rather than after one.

The two documents listing what cannot be inspected omitted webhook
configurations, which were three of three warning findings on the cluster
this was tested against.

And \"seven checks are skipped on every triage call\" is eight when the
server runs without --logs. The shipped manifest passes it, so seven is
right for a plugin install and wrong for a hand-configured server."
```

---

## Task 6: The reference page and the changelog

**Files:**
- Modify: `website/docs/features/mcp.md` (line 25's table row; a new section
  between line 31 and the `## The coverage block` heading at line 33)
- Modify: `CHANGELOG.md` (a new bullet at the top of `[Unreleased]`'s
  `### Fixed`, which begins at line 26)

**Interfaces:**
- Consumes: the `owner` field and the `found: false` meaning from Tasks 2-3.
- Produces: nothing.

- [ ] **Step 1: Update the tools table row**

Replace line 25's "What it does" cell — currently "Drills into one workload or
pod: its status, its pods, kubeagent's findings for it, and its recent
Kubernetes events." — so the row reads:

```markdown
| `kubeagent_inspect` | `kind` (required — `pod`, `deployment`, `statefulset`, `daemonset`, `replicaset`, `job`, or `cronjob`), `namespace` (required), `name` (required), `context` (optional) | Drills into one workload or pod: its status, its pods, kubeagent's findings for it, and its recent Kubernetes events. A pod answer names the controller that owns it in `owner`. |
```

- [ ] **Step 2: Add the resolution section**

Insert between the `context`/`--allow-context-switch` paragraph (ending at
line 31) and the `## The coverage block` heading (line 33):

````markdown
## What `kubeagent_inspect` resolves

`found: false` means one thing: no object of that kind, with that name, exists
in that namespace in the snapshot the call collected. It is not a proxy for
"healthy", and it is not a truncation artefact. Existence is answered against
the raw objects the scan collected, not against the workload list the text
report renders — that list is filtered for display, so it drops the healthy
majority and caps a Job's pod rows, both of which are right for a report an
operator reads and wrong for a lookup.

A `found: false` result still carries the object's recent events. That is
deliberate: "the object is gone but its events explain why" is exactly what a
drill-down has to answer, and a deleted pod's events are often the whole story.

A pod answer describes the pod, not its controller. `kind` is `Pod`, `status` is
the pod's own phase, `pods` carries that one row, and `findings` are the pod's
own. `desired` and `ready` are **absent** rather than `0` — a pod has no replica
count, and absence must never read as zero. The controller that owns it is named
in `owner`:

```json
{
  "found": true,
  "kind": "Pod",
  "namespace": "payments",
  "name": "worker-7d9c6f6b8-x2z4q",
  "status": "Running",
  "owner": "Deployment/worker",
  "image": "registry.example.com/worker:1.4.0",
  "pods": [ "…one row for this pod…" ],
  "findings": [ "…this pod's own findings…" ],
  "events": [ "…" ],
  "coverage": { "…" }
}
```

`owner` is the escalation pointer: `kubeagent_triage` reports a critical finding
against a pod, and this is how a caller reaches the workload behind it without
guessing its name. The key is absent for a bare pod — one with no controller,
which must not claim an owner it does not have — and for every kind other than
`pod`.
````

- [ ] **Step 3: Build the docs**

```bash
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f website/mkdocs.yml
cd /home/ubuntu/git/kubeagent
```

Expected: exit 0, no `WARNING` line naming `features/mcp.md`. The red "Material
for MkDocs 2.0" banner is cosmetic. **The bash working directory persists
between calls and has drifted into `website/` before — the `cd` back to the
repo root is not optional.**

- [ ] **Step 4: Add the changelog entry**

Insert as the **first** bullet under `[Unreleased]`'s `### Fixed` heading (line
26), above the existing "`kubeagent mcp` no longer exits…" bullet:

```markdown
- **`kubeagent_inspect` resolves every object of the seven kinds it
  advertises.** It answered a lookup against the workload list the text report
  renders, which `inventory.Prioritize` filters for display — so two classes of
  object that exist answered `found: false`. A controller-owned pod is never a
  workload in its own right, yet `Pod/<name>` is exactly the identity
  `kubeagent_triage` emits for every critical finding, and the shipped skill
  tells a model to inspect it; on the cluster this was found against, that was
  six of six critical findings. A healthy-quiet workload is dropped outright, so
  inspecting a fully-ready Deployment failed too, and a Job's pod rows are
  capped at three. Existence now comes from the raw snapshot the scan already
  collected, whose seven collections are exactly the seven kinds the tool takes,
  so the fix costs **no new cluster read**. A pod answers as itself — `kind`
  `Pod`, its own phase, its one row, its own findings — with `desired` and
  `ready` absent rather than `0`, and a new `owner` field naming the controller
  so a caller can escalate without guessing the workload's name. `found: false`
  now means the object does not exist, and still returns its events. No
  `schemaVersion` moves (`kubeagent_inspect`'s result is not one of the eight
  versioned documents) and no import-graph invariant changes. See
  [MCP server](website/docs/features/mcp.md).
```

- [ ] **Step 5: Verify**

```bash
export PATH=$PATH:/usr/local/go/bin
cd /home/ubuntu/git/kubeagent
go build ./... && go test -p 2 -count=1 ./...
git diff --stat main -- go.mod go.sum internal/report/testdata/golden-scan.txt website/docs/schemas/ website/docs/quickstart.md
```

Expected: every package `ok`; the final `git diff --stat` prints **nothing**.
The repeated Keep-a-Changelog `MD024` lint warnings about duplicated
`### Fixed` headings are expected style — ignore them.

- [ ] **Step 6: Commit**

```bash
git add website/docs/features/mcp.md CHANGELOG.md
git commit -s -m "docs(mcp): document owner and what found:false means

The reference page now says what found:false means -- no object of that kind
with that name exists in that namespace, not a statement about health and not
a truncation artefact -- and that such a result still carries the object's
events, which is deliberate.

It also documents the pod answer's shape: the pod's own phase and row, no
desired or ready key (a pod has no replica count, and absence must never read
as zero), and owner naming the controller so a caller can escalate from a
critical pod finding without guessing the workload's name."
```

---

## Self-Review

**1. Spec coverage.** Every section of
[the spec](../specs/2026-08-08-mcp-inspect-pod-resolution-design.md) maps to a
task:

| Spec requirement | Task |
|---|---|
| Decision 1 — resolve all seven kinds against `res.Inputs` | 2 (controller kinds), 3 (pod), 4 (ReplicaSet) |
| Decision 2 — a pod answer describes the pod and names its owner; `Desired`/`Ready` unset | 3 |
| Decision 3 — fix `inspect`, not `fromDiagnose`; `view.go` untouched | 2, 3, 4 (no task edits `view.go`) |
| Decision 4 — the un-inspectable warning kinds stay so; the model is told | 5 |
| Implementation — the resolver, pure, no new cluster read | 2, 4 |
| Implementation — `inventory.PodRowFor`, `Assemble` refactored to call it | 1 |
| Implementation — findings on the pod path, no `fromWorkload` fallback, `Owner` from `PodOwners` | 3 |
| Documentation table, all five files | 5 (four of them), 6 (`website/docs/features/mcp.md`) |
| Test 1 — `PodRowFor` pinned field by field, then `Assemble` shown to route through it | 1 Step 2 |
| Test 2 — resolver table over all seven kinds, positive and negative | 2 Step 1 |
| Test 3 — controller-owned pod, seen to fail first | 3 Steps 1-2 |
| Test 4 — healthy Deployment | 2 Step 1 |
| Test 5 — absent object still `found: false`, empty lists, events | pre-existing `TestInspect_MissingObjectIsNotFoundNotAnError`, re-run in 3 Step 5 |
| Test 6 — no `desired`/`ready` key on a pod answer | 3 Step 1 |
| Test 7 — no `owner` key on a bare pod | 3 Step 1 |
| Test 8 — a Job's fourth pod is inspectable | 3 Step 1 |
| Test 9 — `golden-scan.txt` byte-identical, never `-update` | every task's verify step |
| Decision 1, ReplicaSet half — found during Task 2: `Assemble` has no `Inputs.ReplicaSets` loop, so a Deployment-owned or pod-less ReplicaSet answered `found: false` | 4 |

The spec's "Recorded, out of scope" item — whether a pod row should carry
`Node` and `IP` at all — is deliberately **not** a task here. It predates this
slice.

**2. Placeholder scan.** No `TBD`, no "add a test for X", no "handle edge
cases". Every code step carries the literal code to write; every doc step
carries the literal replacement prose.

**3. Type consistency.** `PodRowFor(p corev1.Pod, now time.Time) PodRow` is
declared in Task 1 and called in Task 3 with that signature.
`resolveObject` is declared in Task 2 as
`(res scan.Result, kind, namespace, name string) resolved` and Task 3
explicitly changes it to `(res scan.Result, kind, namespace, name string, now time.Time) resolved`,
naming the four test call sites that must be updated. `resolved` gains exactly
one field (`Owner string`) in Task 3, matching `InspectOutput.Owner`.
`findingsOf(ws []inventory.Workload) []diagnose.Finding` is declared in Task 2
and called in Tasks 3 and 4. `ctrl` and `ownedPod` are declared in Task 2's test
step and used in Tasks 3's and 4's; `crashingOwnedPod` and `callInspectRaw` are
Task 3's own and collide with nothing in `internal/mcp`'s test files. Task 4
renames `inventory.workloadStatus(ready, desired int) string` to exported
`WorkloadStatus` with the same signature — a rename, not a second name for one
rule — and calls it from
`resolveReplicaSet(res scan.Result, namespace, name string, now time.Time) resolved`,
which takes the `now` Task 3 added to `resolveObject`'s signature and passes it
to Task 1's `PodRowFor`. `ownedByReplicaSet(p corev1.Pod, name string) bool` is
Task 4's own and collides with nothing.

**Task 4 is an amendment, not a spec section.** It was written during Task 2's
execution, when the resolver table test failed on `replicaset` and the cause
turned out to be wider than the fixture: `inventory.Assemble` has no
`Inputs.ReplicaSets` loop at all, so a ReplicaSet materialises as a workload
only as a side effect of a pod whose `PodOwners` result is `ReplicaSet` — which
excludes every Deployment-owned ReplicaSet and every ReplicaSet with no pods.
The spec's Decision 1 promises all seven advertised kinds resolve. Task 4 keeps
that promise rather than retracting it, and it lands **before** the two
documentation tasks because Tasks 5 and 6 already say seven kinds resolve.

**One refinement of the spec, recorded here rather than smuggled in:** the spec's
Decision 2 lists `Kind`, `Status`, `Pods`, `Findings` and `Owner` for a pod
answer and does not mention `Image`. Task 3 sets `Image` from the pod row it
already built (`row.Image`), so a pod answer and a workload answer report that
field the same way rather than the pod path leaving a top-level `image` absent
while every other kind carries one. It needs no new export and no new read, and
`Image` is `omitempty`, so a pod with no containers still omits the key.

## Execution Handoff

Plan complete and saved to
`docs/superpowers/plans/2026-08-08-mcp-inspect-pod-resolution.md`.

Execution is **subagent-driven**: a fresh implementer subagent per task on a
mid model, an independent task reviewer per task on a mid model, and the
whole-branch review on the most capable model. Branch
`mcp-inspect-resolution`, cut off `main` at the spec commit as Task 1 Step 1.
