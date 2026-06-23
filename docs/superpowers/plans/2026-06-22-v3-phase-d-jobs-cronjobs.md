# kubeagent v3 — Phase D Plan: Jobs & CronJobs

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Include Jobs and CronJobs in the `scan` inventory — Jobs shown with status (Complete/Failed/Running/Pending) and their pods; CronJobs shown with their schedule and active-job count, grouping their Jobs' pods beneath them; completed pods capped so finished Jobs don't flood the output.

**Architecture:** `collect.CollectInventory` also lists `batch/v1` Jobs and CronJobs into `inventory.Inputs`. `inventory.Assemble` resolves Job→CronJob (like RS→Deployment), seeds Job/CronJob workloads with controller-derived status, rolls Job-owned pods up to their CronJob (or standalone Job), and caps Job/CronJob pod rows. `report` renders Job/CronJob workloads without the replica count and shows the schedule + an omitted-pods note. `main` and `explain` need no change (failed Jobs become `Flagged()` → notable automatically).

**Tech Stack:** Go 1.26, `k8s.io/client-go` (BatchV1 + fake clientset), existing `inventory`/`collect`/`report`. No new dependency.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go 1.26.
- **Read-only** (List only); **sequential** (no goroutines); no new dependency.
- **Job status:** `Failed`/`Complete` from Job conditions; else `Running` when `Active > 0`; else `Pending`.
- **CronJob:** no replicas — render with `Spec.Schedule` and an `Active(N)`/`Idle` status; Desired/Ready stay 0.
- **Grouping:** a pod owned by a Job that is owned by a CronJob rolls up to the **CronJob** workload; a pod owned by a standalone Job rolls up to the **Job** workload.
- **Completed-pod cap:** Job/CronJob workloads show at most **3** pod rows, with a `+N more pods` note for the remainder (List order; strict recency ordering is out of scope).
- **Failed Jobs are flagged:** `Workload.Flagged()` also returns true when `Status == "Failed"`, so failed Jobs sort first, get a ⚠, and become notable for `--explain`.
- Each task keeps `go build ./...` and `go test ./...` green.

---

## File Structure

- `internal/inventory/inventory.go` — **modify.** Add `Jobs`/`CronJobs` to `Inputs`; add `PodsOmitted`/`Schedule` to `Workload`; extend `Flagged()`; add `jobStatus`/`cronJobStatus`; extend `Assemble`.
- `internal/inventory/inventory_test.go` — **modify.** Helper + Assemble tests for Jobs/CronJobs.
- `internal/collect/collect.go` — **modify.** List Jobs + CronJobs in `CollectInventory`.
- `internal/collect/collect_test.go` — **modify.** Assert Jobs/CronJobs collected.
- `internal/report/report.go` — **modify.** Render Job/CronJob workloads (no count; schedule; `+N more pods`).
- `internal/report/report_test.go` — **modify.** Job/CronJob rendering tests.
- `README.md` / `docs/design.md` — **modify (Task 5).** Mention Jobs/CronJobs; mark v3 scope complete.

---

## Task 1: `inventory` — Job/CronJob types, helpers, and `Flagged()`

**Files:**
- Modify: `internal/inventory/inventory.go`
- Modify: `internal/inventory/inventory_test.go`

**Interfaces:**
- Produces: `Inputs` gains `Jobs []batchv1.Job` and `CronJobs []batchv1.CronJob`; `Workload` gains `PodsOmitted int` (json `podsOmitted,omitempty`) and `Schedule string` (json `schedule,omitempty`); `Flagged()` also true when `Status=="Failed"`; helpers `jobStatus(batchv1.Job) string`, `cronJobStatus(batchv1.CronJob) string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/inventory/inventory_test.go` (add import `batchv1 "k8s.io/api/batch/v1"`):

```go
func TestJobStatus(t *testing.T) {
	failed := batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}}
	if jobStatus(failed) != "Failed" {
		t.Errorf("failed job: got %q", jobStatus(failed))
	}
	complete := batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	if jobStatus(complete) != "Complete" {
		t.Errorf("complete job: got %q", jobStatus(complete))
	}
	running := batchv1.Job{Status: batchv1.JobStatus{Active: 2}}
	if jobStatus(running) != "Running" {
		t.Errorf("active job: got %q", jobStatus(running))
	}
	pending := batchv1.Job{}
	if jobStatus(pending) != "Pending" {
		t.Errorf("idle job: got %q", jobStatus(pending))
	}
}

func TestCronJobStatus(t *testing.T) {
	active := batchv1.CronJob{Status: batchv1.CronJobStatus{Active: []corev1.ObjectReference{{}, {}}}}
	if cronJobStatus(active) != "Active(2)" {
		t.Errorf("active cronjob: got %q", cronJobStatus(active))
	}
	idle := batchv1.CronJob{}
	if cronJobStatus(idle) != "Idle" {
		t.Errorf("idle cronjob: got %q", cronJobStatus(idle))
	}
}

func TestFlagged_FailedStatus(t *testing.T) {
	w := Workload{Kind: "Job", Ready: 0, Desired: 0, Status: "Failed"}
	if !w.Flagged() {
		t.Error("a Failed job should be flagged")
	}
	ok := Workload{Kind: "Job", Ready: 0, Desired: 0, Status: "Complete"}
	if ok.Flagged() {
		t.Error("a Complete job should not be flagged")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory/ -run 'JobStatus|CronJobStatus|Flagged_FailedStatus' 2>&1 | tail -8
```
Expected: FAIL — undefined `jobStatus`/`cronJobStatus`; `Flagged_FailedStatus` fails (Failed not yet flagged).

- [ ] **Step 3: Update `internal/inventory/inventory.go`**

Add the import `batchv1 "k8s.io/api/batch/v1"`. Add the new fields and helpers, and extend `Flagged`:

```go
// (in Inputs, add:)
	Jobs     []batchv1.Job
	CronJobs []batchv1.CronJob
```

```go
// (in Workload, add after Findings:)
	PodsOmitted int    `json:"podsOmitted,omitempty"`
	Schedule    string `json:"schedule,omitempty"`
```

```go
// Flagged reports whether the workload needs attention.
func (w Workload) Flagged() bool {
	return len(w.Findings) > 0 || w.Ready < w.Desired || w.Status == "Failed"
}
```

```go
// jobStatus maps a Job's conditions/counts to a status string.
func jobStatus(j batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Status == corev1.ConditionTrue {
			switch c.Type {
			case batchv1.JobFailed:
				return "Failed"
			case batchv1.JobComplete:
				return "Complete"
			}
		}
	}
	if j.Status.Active > 0 {
		return "Running"
	}
	return "Pending"
}

// cronJobStatus summarizes a CronJob by its active-job count.
func cronJobStatus(cj batchv1.CronJob) string {
	if n := len(cj.Status.Active); n > 0 {
		return fmt.Sprintf("Active(%d)", n)
	}
	return "Idle"
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/inventory/ -v 2>&1 | tail -30
go vet ./internal/inventory/
go build ./...
```
Expected: PASS — new + existing inventory tests (the existing `Flagged` tests still pass; none use `Status=="Failed"`); vet clean; module builds (Assemble doesn't use the new Inputs fields yet — that's Task 2).

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/
git commit -m "feat(inventory): Job/CronJob status helpers, fields, flag Failed" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: `inventory.Assemble` — group Jobs & CronJobs

**Files:**
- Modify: `internal/inventory/inventory.go`
- Modify: `internal/inventory/inventory_test.go`

**Interfaces:**
- Consumes: `Inputs.Jobs`/`CronJobs` (Task 1), `jobStatus`/`cronJobStatus` (Task 1).
- Behavior: standalone Jobs → `Job` workloads; CronJob-owned Jobs' pods → roll up to the `CronJob` workload; Job/CronJob pod rows capped at `jobPodCap` (3) with `PodsOmitted` set.

- [ ] **Step 1: Write the failing tests**

Add to `internal/inventory/inventory_test.go`:

```go
func TestAssemble_StandaloneJob(t *testing.T) {
	in := Inputs{
		Jobs: []batchv1.Job{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "migrate"},
			Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
		}},
		Pods: []corev1.Pod{pod("batch", "migrate-xyz", ctrlRef("Job", "migrate"), 0, "migrate:1")},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 || ws[0].Kind != "Job" || ws[0].Name != "migrate" {
		t.Fatalf("got %+v", ws)
	}
	if ws[0].Status != "Complete" {
		t.Errorf("status = %q, want Complete", ws[0].Status)
	}
	if len(ws[0].Pods) != 1 {
		t.Errorf("expected 1 pod row, got %d", len(ws[0].Pods))
	}
}

func TestAssemble_CronJobRollsUpItsJobsPods(t *testing.T) {
	in := Inputs{
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "backup"},
			Spec:       batchv1.CronJobSpec{Schedule: "0 2 * * *"},
		}},
		Jobs: []batchv1.Job{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "backup-28000", OwnerReferences: ctrlRef("CronJob", "backup")},
		}},
		Pods: []corev1.Pod{pod("batch", "backup-28000-aaa", ctrlRef("Job", "backup-28000"), 0, "backup:1")},
	}
	ws := Assemble(in, nil)
	// Only the CronJob workload (the Job is not seeded separately; its pod rolls up).
	if len(ws) != 1 || ws[0].Kind != "CronJob" || ws[0].Name != "backup" {
		t.Fatalf("expected one CronJob workload, got %+v", ws)
	}
	if ws[0].Schedule != "0 2 * * *" {
		t.Errorf("schedule = %q", ws[0].Schedule)
	}
	if len(ws[0].Pods) != 1 || ws[0].Pods[0].Name != "backup-28000-aaa" {
		t.Errorf("expected the job's pod under the cronjob, got %+v", ws[0].Pods)
	}
}

func TestAssemble_CapsJobPods(t *testing.T) {
	in := Inputs{
		Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "noisy"}}},
		Pods: []corev1.Pod{
			pod("batch", "noisy-1", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-2", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-3", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-4", ctrlRef("Job", "noisy"), 0, "i"),
			pod("batch", "noisy-5", ctrlRef("Job", "noisy"), 0, "i"),
		},
	}
	ws := Assemble(in, nil)
	if len(ws) != 1 {
		t.Fatalf("got %d workloads", len(ws))
	}
	if len(ws[0].Pods) != 3 {
		t.Errorf("expected pods capped to 3, got %d", len(ws[0].Pods))
	}
	if ws[0].PodsOmitted != 2 {
		t.Errorf("PodsOmitted = %d, want 2", ws[0].PodsOmitted)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/inventory/ -run TestAssemble_Standalone'|'TestAssemble_CronJob'|'TestAssemble_CapsJobPods 2>&1 | tail -10
```
Expected: FAIL — Jobs/CronJobs are not yet seeded/grouped (workloads empty or pods become bare/`Job` pod-derived without status; cap not applied).

- [ ] **Step 3: Extend `Assemble` in `internal/inventory/inventory.go`**

Add a `jobPodCap` const near the top of the file:

```go
const jobPodCap = 3 // max pod rows shown per Job/CronJob workload
```

Then update `Assemble`. Add the Job/CronJob seeding + the Job→CronJob map BEFORE the pod loop, extend the pod-owner switch with a `"Job"` case, and add the cap in the final loop. The full updated function:

```go
func Assemble(in Inputs, findings []diagnose.Finding) []Workload {
	key := func(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

	workloads := map[string]*Workload{}
	controllerKeys := map[string]bool{}
	seed := func(kind, ns, name string, desired, ready int) {
		k := key(kind, ns, name)
		workloads[k] = &Workload{Namespace: ns, Name: name, Kind: kind, Desired: desired, Ready: ready}
		controllerKeys[k] = true
	}
	for _, d := range in.Deployments {
		desired := 1
		if d.Spec.Replicas != nil {
			desired = int(*d.Spec.Replicas)
		}
		seed("Deployment", d.Namespace, d.Name, desired, int(d.Status.ReadyReplicas))
	}
	for _, s := range in.StatefulSets {
		desired := 1
		if s.Spec.Replicas != nil {
			desired = int(*s.Spec.Replicas)
		}
		seed("StatefulSet", s.Namespace, s.Name, desired, int(s.Status.ReadyReplicas))
	}
	for _, ds := range in.DaemonSets {
		seed("DaemonSet", ds.Namespace, ds.Name, int(ds.Status.DesiredNumberScheduled), int(ds.Status.NumberReady))
	}

	// seedJobLike seeds a Job/CronJob workload with a controller-derived status
	// (and schedule), keeping Desired/Ready at 0.
	seedJobLike := func(kind, ns, name, status, schedule string) {
		k := key(kind, ns, name)
		workloads[k] = &Workload{Namespace: ns, Name: name, Kind: kind, Status: status, Schedule: schedule}
		controllerKeys[k] = true
	}
	for _, cj := range in.CronJobs {
		seedJobLike("CronJob", cj.Namespace, cj.Name, cronJobStatus(cj), cj.Spec.Schedule)
	}
	// jobToCronJob resolves a Job to its owning CronJob (namespaced); CronJob-owned
	// Jobs are NOT seeded as their own workloads (their pods roll up to the CronJob).
	jobToCronJob := map[string]string{}
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			jobToCronJob[j.Namespace+"/"+j.Name] = o.Name
			continue
		}
		seedJobLike("Job", j.Namespace, j.Name, jobStatus(j), "")
	}

	// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).
	rsToDeploy := map[string]string{}
	for _, rs := range in.ReplicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}

	podKey := map[string]string{}
	derivedReady := map[string]int{}
	for _, p := range in.Pods {
		kind, name := "Pod", p.Name
		if o := controllerOwner(p.OwnerReferences); o != nil {
			switch o.Kind {
			case "ReplicaSet":
				if dep, ok := rsToDeploy[p.Namespace+"/"+o.Name]; ok {
					kind, name = "Deployment", dep
				} else {
					kind, name = "ReplicaSet", o.Name
				}
			case "Job":
				if cj, ok := jobToCronJob[p.Namespace+"/"+o.Name]; ok {
					kind, name = "CronJob", cj
				} else {
					kind, name = "Job", o.Name
				}
			default:
				kind, name = o.Kind, o.Name
			}
		}
		k := key(kind, p.Namespace, name)
		w, ok := workloads[k]
		if !ok {
			w = &Workload{Namespace: p.Namespace, Name: name, Kind: kind}
			workloads[k] = w
		}
		restarts, last := podRestarts(p)
		w.Restarts += restarts
		if lt := termTime(last); lt != "" && lt > w.LastRestart {
			w.LastRestart = lt
		}
		if w.Image == "" {
			w.Image = podImage(p)
		}
		if podIsReady(p) {
			derivedReady[k]++
		}
		w.Pods = append(w.Pods, PodRow{
			Name: p.Name, Phase: string(p.Status.Phase), Ready: podReady(p),
			Restarts: restarts, LastRestart: termTime(last),
			Node: p.Spec.NodeName, IP: p.Status.PodIP,
			Age: humanAge(p.CreationTimestamp.Time, time.Now()), Image: podImage(p),
		})
		podKey[p.Namespace+"/"+p.Name] = k
	}

	// Pods and findings come from the same scan snapshot, so every finding's
	// pod is present in podKey; an unmatched finding (none today) is dropped.
	for _, f := range findings {
		if k, ok := podKey[f.Pod]; ok {
			workloads[k].Findings = append(workloads[k].Findings, f)
		}
	}

	out := make([]Workload, 0, len(workloads))
	for k, w := range workloads {
		if !controllerKeys[k] {
			w.Desired = len(w.Pods)
			w.Ready = derivedReady[k]
		}
		if (w.Kind == "Job" || w.Kind == "CronJob") && len(w.Pods) > jobPodCap {
			w.PodsOmitted = len(w.Pods) - jobPodCap
			w.Pods = w.Pods[:jobPodCap]
		}
		if w.Status == "" {
			w.Status = workloadStatus(w.Ready, w.Desired)
		}
		out = append(out, *w)
	}
	sortWorkloads(out)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/inventory/ -v 2>&1 | tail -40
go vet ./internal/inventory/
go build ./...
```
Expected: PASS — all inventory tests (Phase B/C + the 3 new Job/CronJob tests); vet clean; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/inventory/
git commit -m "feat(inventory): group Jobs and CronJobs (roll-up + pod cap)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `collect` — list Jobs & CronJobs

**Files:**
- Modify: `internal/collect/collect.go`
- Modify: `internal/collect/collect_test.go`

**Interfaces:**
- Consumes: `inventory.Inputs.Jobs`/`CronJobs` (Task 1).

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (add import `batchv1 "k8s.io/api/batch/v1"`):

```go
func TestCollectInventory_ListsJobsAndCronJobs(t *testing.T) {
	client := fake.NewSimpleClientset(
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "j1"}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "batch", Name: "cj1"}},
	)
	in, err := CollectInventory(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(in.Jobs) != 1 || len(in.CronJobs) != 1 {
		t.Errorf("expected 1 job and 1 cronjob, got %d/%d", len(in.Jobs), len(in.CronJobs))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestCollectInventory_ListsJobsAndCronJobs 2>&1 | tail -8
```
Expected: FAIL — `in.Jobs`/`in.CronJobs` are empty (CollectInventory doesn't list them yet).

- [ ] **Step 3: Add the List calls in `internal/collect/collect.go`**

In `CollectInventory`, after the DaemonSets block and before `return in, nil`, add:

```go
	jobs, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing jobs: %w", err)
	}
	in.Jobs = jobs.Items

	cronjobs, err := client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return in, fmt.Errorf("listing cronjobs: %w", err)
	}
	in.CronJobs = cronjobs.Items
```

(No new import needed in `collect.go` — the items flow into `inventory.Inputs` fields via the `BatchV1()` method chain, same as the apps/v1 controllers.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collect/ -v 2>&1 | tail -15
go vet ./internal/collect/
go build ./...
```
Expected: PASS — new + existing collect tests; vet clean; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list Jobs and CronJobs" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: `report` — render Job/CronJob workloads

**Files:**
- Modify: `internal/report/report.go`
- Modify: `internal/report/report_test.go`

**Interfaces:**
- Behavior: for `Kind` `Job`/`CronJob`, the text header omits the `ready/desired` count (it's always 0/0) and shows the status (and schedule for CronJobs); after the pod rows, a `+N more pods` line appears when `PodsOmitted > 0`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventory_TextJobOmitsCountShowsStatus(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "batch", Name: "migrate", Kind: "Job", Status: "Complete",
		Pods: []inventory.PodRow{{Name: "migrate-x", Phase: "Succeeded", Ready: "0/1"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, ws, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "batch/migrate") || !strings.Contains(out, "Job") || !strings.Contains(out, "Complete") {
		t.Errorf("expected job header with status:\n%s", out)
	}
	if strings.Contains(out, "0/0") {
		t.Errorf("job header should not show a 0/0 replica count:\n%s", out)
	}
}

func TestPrintInventory_TextCronJobShowsScheduleAndOmitted(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "batch", Name: "backup", Kind: "CronJob", Status: "Idle", Schedule: "0 2 * * *",
		Pods:        []inventory.PodRow{{Name: "backup-1"}, {Name: "backup-2"}, {Name: "backup-3"}},
		PodsOmitted: 5,
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, ws, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "0 2 * * *") {
		t.Errorf("expected the cron schedule:\n%s", out)
	}
	if !strings.Contains(out, "+5 more pods") {
		t.Errorf("expected the omitted-pods note:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run 'JobOmits|CronJobShows' 2>&1 | tail -10
```
Expected: FAIL — the header still prints `0/0` for Jobs; no schedule line; no `+N more pods` note.

- [ ] **Step 3: Update `printInventoryText` in `internal/report/report.go`**

Replace the per-workload header construction and add the omitted-pods note. The per-workload loop body becomes:

```go
	for _, wl := range workloads {
		flag := "  "
		if wl.Flagged() {
			flag = "⚠ "
		}
		var header string
		if wl.Kind == "Job" || wl.Kind == "CronJob" {
			header = fmt.Sprintf("%s%s/%s  %s  %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Status)
			if wl.Schedule != "" {
				header += "  (" + wl.Schedule + ")"
			}
		} else {
			header = fmt.Sprintf("%s%s/%s  %s  %d/%d %s", flag, wl.Namespace, wl.Name, wl.Kind, wl.Ready, wl.Desired, wl.Status)
		}
		if wl.Restarts > 0 {
			header += fmt.Sprintf("  · %d restarts", wl.Restarts)
			if wl.LastRestart != "" {
				header += fmt.Sprintf(", last %s", inventory.HumanSince(wl.LastRestart, time.Now()))
			}
		}
		if _, err := fmt.Fprintln(w, header); err != nil {
			return err
		}
		if wl.Image != "" {
			if _, err := fmt.Fprintf(w, "    image %s\n", wl.Image); err != nil {
				return err
			}
		}
		for _, f := range wl.Findings {
			if _, err := fmt.Fprintf(w, "    ⚠ %s: %s\n", f.Issue, f.Reason); err != nil {
				return err
			}
		}
		for _, p := range wl.Pods {
			restarts := fmt.Sprintf("%d", p.Restarts)
			if p.LastRestart != "" {
				restarts += " (" + inventory.HumanSince(p.LastRestart, time.Now()) + ")"
			}
			if _, err := fmt.Fprintf(w, "    %s  %s  %s  restarts=%s  %s  %s  %s\n",
				p.Name, p.Ready, p.Phase, restarts, p.Node, p.IP, p.Age); err != nil {
				return err
			}
		}
		if wl.PodsOmitted > 0 {
			if _, err := fmt.Fprintf(w, "    +%d more pods\n", wl.PodsOmitted); err != nil {
				return err
			}
		}
	}
```

(Only the header `if/else` and the trailing `PodsOmitted` block are new; the image/findings/pod-row rendering is unchanged from Phase B/C.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/report/ -v 2>&1 | tail -25
go vet ./internal/report/
go build ./...
```
Expected: PASS — new Job/CronJob tests + all existing report tests (non-Job workloads still render with the count); vet clean; module builds.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): render Jobs/CronJobs (status, schedule, +N more pods)" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 5: end-to-end verification + docs

**Files:**
- Modify: `README.md`
- Modify: `docs/design.md`

**Note:** `main.go` and `internal/explain` need **no code change** — `CollectInventory` now returns Jobs/CronJobs in `Inputs`, `Assemble` groups them, `report` renders them, and failed Jobs become `Flagged()` → notable for `--explain` automatically. This task verifies the end-to-end wiring and updates docs.

- [ ] **Step 1: Full build + suite + manual smoke**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./...
go test ./... 2>&1
go vet ./...
go build -o kubeagent .
ANTHROPIC_API_KEY= ./kubeagent scan --explain 2>&1 | head -2   # fail-fast unchanged
```
Expected: all packages PASS; vet clean; binary builds; the manual command still prints the `--explain needs the ANTHROPIC_API_KEY environment variable` error.

- [ ] **Step 2: Update `README.md`**

In the Usage section, update the `scan` description comment to include Jobs/CronJobs:

```bash
# scan the whole cluster — leads with a cluster-health verdict (nodes +
# kube-system), then every workload (Deployments, StatefulSets, DaemonSets,
# Jobs, CronJobs, bare pods) with replica/job health, restart history, and
# any problems
./kubeagent scan
```

- [ ] **Step 3: Update `docs/design.md`**

In the Roadmap, change the v3 line from "in progress" to shipped:

```markdown
- **v3 (shipped)** — `scan` is a complete cluster health report: a first-line
  node + kube-system verdict, then every workload (Deployments, StatefulSets,
  DaemonSets, Jobs, CronJobs, bare pods) grouped with health, restart history,
  and integrated detector findings; `--explain` summarizes notable items;
  model selectable via `--model`/`KUBEAGENT_MODEL`.
```

- [ ] **Step 4: Commit**

```bash
git add README.md docs/design.md
git commit -m "docs: v3 scan covers Jobs/CronJobs; mark v3 shipped" -m "Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Self-review

- **Spec coverage (Phase D):** Job collection + status → Task 1/3 ✅; CronJob collection + schedule + active count → Task 1/3 ✅; Job→CronJob roll-up of pods → Task 2 ✅; standalone Jobs as their own workloads → Task 2 ✅; completed-pod cap (3 + `+N more`) → Task 2 (`jobPodCap`, `PodsOmitted`) + Task 4 ✅; failed Jobs flagged/notable → Task 1 `Flagged()` (no explain/main change needed) ✅; report renders Job/CronJob without a replica count, with schedule → Task 4 ✅; docs → Task 5 ✅.
- **Placeholder scan:** none — every step has complete code/commands.
- **Type consistency:** `Inputs.Jobs []batchv1.Job` / `CronJobs []batchv1.CronJob` (Task 1) populated by `CollectInventory` (Task 3) and consumed by `Assemble` (Task 2); `Workload.PodsOmitted`/`Schedule` (Task 1) set in `Assemble` (Task 2) and rendered by `report` (Task 4); `jobStatus`/`cronJobStatus` (Task 1) used in `Assemble` (Task 2); `Flagged()` extension (Task 1) drives sort/⚠/notable with no change to `explain`/`main`.
- **No main change:** confirmed — the pipeline already threads `Inputs` → `Assemble` → `report`/`explain`; Task 5 only verifies + documents.
- **Known simplification:** the pod cap keeps List-order pods (not strictly most-recent) — documented in Global Constraints; acceptable for v3.
