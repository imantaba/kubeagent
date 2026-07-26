# Multi-cluster hub Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kubeagent watch --context prod-eu --context prod-us` watches N clusters from one process behind one HTTP endpoint, with every metric series, issue record, explanation and alert naming its cluster.

**Architecture:** One informer set, `watchstate.Tracker`, `alertstate.Roller` and SLO tracker per cluster, each in its own goroutine; one shared HTTP server, metrics snapshot, alert sink and explanation budget. Cluster identity is stamped at the boundary — `alertstate.Object` gains a `Cluster` field and each cluster's Roller stamps its own name — so `watchstate` needs no change at all. A configuration error is fatal at startup; a cluster failing at runtime degrades that cluster only.

**Tech Stack:** Go 1.26, standard-library `flag`, client-go informers and fake clientset, Helm, bash (chaos harness).

Spec: [docs/superpowers/specs/2026-07-26-multi-cluster-hub-design.md](../specs/2026-07-26-multi-cluster-hub-design.md), committed at `148b775`.

## Global Constraints

- `export PATH=$PATH:/usr/local/go/bin` before any `go` command. Build with `go build ./...`, test with `go test ./...`.
- **Read-only invariant:** every cluster is touched with get/list/watch only. No writes, no new verbs, no new local RBAC. This holds for every target.
- **No `Co-Authored-By: Claude` trailer** and no Claude/Claude Code/Anthropic attribution in any commit message, code comment, doc or changelog entry.
- **Credentials never reach a process argument, a log line beyond `scheme://host`, or a `values.yaml` literal.** The alert webhook URL, the model endpoint URL, the model API key, and now the multi-cluster kubeconfig are all credentials. They come from the environment or a mounted Secret.
- **No secrets, real credentials, private IPs or internal hostnames in any file.** Use `<PLACEHOLDER>`.
- **TDD:** write the failing test first, run it, watch it fail, then implement.
- **v1 CLI uses the standard-library `flag` package only** — no Cobra.
- The default cluster name is the exact string `local`. The metrics label key is exactly `cluster`.
- The `cluster` label is emitted on **every** per-cluster series, including single-cluster operation. The alert and explain series (`kubeagent_alerts_*`, `kubeagent_alert_last_success_timestamp_seconds`, `kubeagent_explain_*`) and `kubeagent_clusters_total` stay **unlabelled** — one sink, one budget, one process.
- Prometheus exposition rule: `# HELP` and `# TYPE` appear **once** per metric family, before its sample lines. With N clusters the header cannot move inside the per-cluster loop.
- Every daemon log line written from a per-cluster code path is prefixed `kubeagent: [<cluster>] `.
- `docs/go-concepts.md` entries use a plain everyday example first, then the kubeagent example. **No Python comparisons.**

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/alertstate/alertstate.go` | `Object.Cluster`, `Options.Cluster`, Roller stamps its cluster | 1 |
| `internal/alert/encode.go` | `cluster` in the JSON payload and the Alertmanager labels | 1 |
| `internal/oncall/oncall.go` | cluster in the throttle key, the store key and `Explanation` | 2 |
| `internal/watch/metrics.go` | per-cluster snapshots, `cluster` label on every per-cluster series, `/issues` and `/explanations` views | 3, 5 |
| `internal/watch/target.go` (new) | `Target` type and `validateTargets` | 4 |
| `internal/watch/cluster.go` (new) | `clusterWorker`: one cluster's informers, trackers and reconcile loop | 4 |
| `internal/watch/watch.go` | `Run` builds workers, owns the shared server/sink/explainer, waits | 4 |
| `internal/watch/slo.go` | `logSLO` gains a cluster prefix | 4 |
| `main.go` | repeatable `--context`, `--cluster-name`, `--include-local`, target construction | 6 |
| `deploy/helm/kubeagent/values.yaml`, `templates/deployment.yaml` | `multicluster.*` values, Secret-mounted kubeconfig, guard-rails | 7 |
| `chaos/run.sh`, `chaos/README.md` | scenario 15 | 8 |
| `website/docs/`, `deploy/README.md`, `CHANGELOG.md`, `docs/go-concepts.md`, `website/docs/roadmap.md` | documentation | 9 |

---

### Task 1: Cluster identity on alerts

**Files:**
- Modify: `internal/alertstate/alertstate.go`
- Modify: `internal/alert/encode.go`
- Test: `internal/alertstate/alertstate_test.go`, `internal/alert/encode_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `alertstate.Object{Cluster, Kind, Namespace, Name}` with `String()` rendering `prod-eu/Deployment/shop/web`; `alertstate.Options{Repeat time.Duration; Cluster string}`; `alertstate.New(Options)` unchanged in signature.

- [ ] **Step 1: Write the failing tests**

Append to `internal/alertstate/alertstate_test.go`:

```go
func TestObjectStringNamesTheCluster(t *testing.T) {
	namespaced := alertstate.Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"}
	if got, want := namespaced.String(), "prod-eu/Deployment/shop/web"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	clusterScoped := alertstate.Object{Cluster: "prod-eu", Kind: "Node", Name: "worker-2"}
	if got, want := clusterScoped.String(), "prod-eu/Node/worker-2"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestRollerStampsItsCluster pins the boundary rule: watchstate.Key carries no
// cluster, because each cluster gets its own tracker and roller. The roller is
// what turns a cluster-free key into a cluster-qualified alert, so if it stops
// stamping, two clusters' alerts for the same object name collapse into one.
func TestRollerStampsItsCluster(t *testing.T) {
	r := alertstate.New(alertstate.Options{Cluster: "prod-us"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	ns := r.Roll([]watchstate.Record{{
		Key:         watchstate.Key{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "CrashLoopBackOff"},
		FiringSince: at,
		LastSeen:    at,
	}}, at)
	if len(ns) != 1 {
		t.Fatalf("Roll returned %d notifications, want 1", len(ns))
	}
	if got := ns[0].Object.Cluster; got != "prod-us" {
		t.Errorf("Object.Cluster = %q, want %q", got, "prod-us")
	}
}
```

Append to `internal/alert/encode_test.go`:

```go
func TestEncodeJSONCarriesTheCluster(t *testing.T) {
	body, err := alert.Encode(alert.FormatJSON, alertstate.Notification{
		Object:      alertstate.Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"CrashLoopBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["cluster"] != "prod-eu" {
		t.Errorf("cluster = %v, want prod-eu", got["cluster"])
	}
}

func TestEncodeAlertmanagerLabelsTheCluster(t *testing.T) {
	body, err := alert.Encode(alert.FormatAlertmanager, alertstate.Notification{
		Object:      alertstate.Object{Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"CrashLoopBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got []struct {
		Labels map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	if got[0].Labels["cluster"] != "prod-eu" {
		t.Errorf("labels[cluster] = %q, want prod-eu", got[0].Labels["cluster"])
	}
}
```

**Note on the encode tests:** `encode` is unexported. Check how the existing tests in `internal/alert/encode_test.go` reach it — if they are in package `alert` (internal tests) call `encode(...)` directly and drop the `alert.` prefix and the exported `Encode` name from the two tests above; if they are in `alert_test` and go through an exported helper, use whatever that file already uses. Match the existing file, do not add a new exported wrapper.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alertstate/ ./internal/alert/ 2>&1 | head -30
```

Expected: FAIL — `unknown field Cluster in struct literal of type alertstate.Object` and `unknown field Cluster in struct literal of type alertstate.Options`.

- [ ] **Step 3: Add the Cluster field to Object and Options**

In `internal/alertstate/alertstate.go`, replace the `Object` type and its `String` method:

```go
// Object identifies the thing an alert is about. Namespace is empty for
// cluster-scoped objects. Cluster names the cluster the object lives in: the
// daemon can watch several, and two clusters routinely run a Deployment with
// the same namespace and name.
type Object struct {
	Cluster   string
	Kind      string
	Namespace string
	Name      string
}

// String renders "prod-eu/Deployment/shop/web", or "prod-eu/Node/worker-2" when
// cluster-scoped.
func (o Object) String() string {
	if o.Namespace == "" {
		return o.Cluster + "/" + o.Kind + "/" + o.Name
	}
	return o.Cluster + "/" + o.Kind + "/" + o.Namespace + "/" + o.Name
}
```

Replace the `Options` type:

```go
// Options tunes the re-send cadence and names the cluster this roller serves.
// A zero Repeat takes the default, following the same convention as
// watchstate.Options.
type Options struct {
	Repeat  time.Duration
	Cluster string
}
```

In `Roller.Roll`, find every place that builds an `Object` from a `watchstate.Record` key and add `Cluster: r.opts.Cluster` as the first field. There is one such construction site inside `Roll`'s grouping loop; if `firing` or any helper builds one too, stamp it there as well. Grep to be sure:

```bash
grep -n "Object{" internal/alertstate/alertstate.go
```

Every literal in that file must carry `Cluster: r.opts.Cluster` (or, for a method without a receiver, take the cluster as a parameter — prefer stamping at the single grouping site).

- [ ] **Step 4: Add cluster to both encoders**

In `internal/alert/encode.go`, add the field to `jsonPayload` immediately before `Kind`, so the wire order reads cluster-then-object:

```go
type jsonPayload struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	Cluster     string   `json:"cluster"`
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	Text        string   `json:"text,omitempty"`
	FiringSince string   `json:"firingSince"`
	ResolvedAt  string   `json:"resolvedAt,omitempty"`
	Flapping    bool     `json:"flapping"`
}
```

and set it in `encodeJSON`:

```go
		Cluster:     n.Object.Cluster,
```

In `encodeAlertmanager`, add the label:

```go
	labels := map[string]string{
		"alertname": "KubeagentIssue",
		"cluster":   n.Object.Cluster,
		"kind":      n.Object.Kind,
		"name":      n.Object.Name,
	}
```

`encodeSlack` needs no change — it renders through `Object.String()`, which now names the cluster.

- [ ] **Step 5: Fix every other construction site in the repo**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go vet ./...
grep -rn "alertstate.Object{" --include=*.go .
```

Existing `Object` literals compile unchanged (an absent `Cluster` is the empty string), so the build will pass — but tests that assert on `Object.String()` output will now fail because the string gained a leading `/`. Run the suite and update every such assertion to include a cluster name:

```bash
go test ./... 2>&1 | grep -E "^(ok|FAIL|---)" | grep -v "^ok"
```

Update the failing assertions to use a non-empty cluster (`"local"` unless the test is already about a named cluster). Do not "fix" them by dropping the cluster from `String()`.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./... 2>&1 | tail -30
```

Expected: every package `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/alertstate internal/alert
git add -u
git commit -m "feat(alertstate): name the cluster on every alert object"
```

---

### Task 2: Cluster identity in the explainer

**Files:**
- Modify: `internal/oncall/oncall.go`
- Test: `internal/oncall/oncall_test.go`

**Interfaces:**
- Consumes: `alertstate.Object.Cluster` (Task 1).
- Produces:
  - `func (e *Explainer) Consider(clusterName string, d watchstate.Delta, health clusterhealth.ClusterHealth, flagged []inventory.Workload, serviceIssues []svchealth.Issue, now time.Time)` — `clusterName` is the new **first** parameter, and the old `cluster clusterhealth.ClusterHealth` parameter is renamed `health`.
  - `oncall.Explanation` gains a leading `Cluster string` field.

- [ ] **Step 1: Write the failing test**

Append to `internal/oncall/oncall_test.go`:

```go
// TestConsiderKeysThrottleAndStorePerCluster pins the one place cluster
// identity cannot live at the boundary. The Explainer is shared across every
// cluster because the hourly budget is a cost control, and cost is a property
// of the process. Its cooldown map and its served store are keyed by object, so
// without the cluster in that key, shop/web in prod-eu and shop/web in prod-us
// share one cooldown slot and overwrite each other on /explanations.
func TestConsiderKeysThrottleAndStorePerCluster(t *testing.T) {
	var calls int64
	ex := oncall.New(oncall.Config{
		Client: explainerFunc(func(context.Context, string) (string, error) {
			atomic.AddInt64(&calls, 1)
			return "because the image tag does not exist", nil
		}),
		Model:    "test-model",
		Cooldown: time.Hour, // long enough that a second call for the SAME key is refused
		Budget:   10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ex.Start(ctx)
	defer ex.Close()

	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	rec := watchstate.Record{
		Key:         watchstate.Key{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "ImagePullBackOff"},
		FiringSince: at,
		LastSeen:    at,
	}
	delta := watchstate.Delta{New: []watchstate.Record{rec}}

	// The first Consider per cluster is the cold-start prime and explains nothing.
	ex.Consider("prod-eu", watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, at)
	ex.Consider("prod-eu", delta, clusterhealth.ClusterHealth{}, nil, nil, at)
	ex.Consider("prod-us", delta, clusterhealth.ClusterHealth{}, nil, nil, at)

	waitFor(t, func() bool { return atomic.LoadInt64(&calls) == 2 })

	latest := ex.Latest()
	if len(latest) != 2 {
		t.Fatalf("Latest() has %d entries, want 2 (one per cluster)", len(latest))
	}
	seen := map[string]bool{}
	for _, x := range latest {
		seen[x.Cluster] = true
	}
	if !seen["prod-eu"] || !seen["prod-us"] {
		t.Errorf("Latest() clusters = %v, want both prod-eu and prod-us", seen)
	}
}
```

**Note on the helpers:** `explainerFunc` and `waitFor` may or may not already exist in `internal/oncall/oncall_test.go`. Read the file first and reuse whatever fake-client type and polling helper it already defines, renaming the calls above to match. Only if neither exists, add:

```go
type explainerFunc func(context.Context, string) (string, error)

func (f explainerFunc) ExplainIncident(ctx context.Context, p string) (string, error) { return f(ctx, p) }

// waitFor polls cond for up to two seconds. The explainer runs its model call on
// a worker goroutine, so the assertion cannot read the result synchronously.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
```

The cold-start prime is per-Explainer, not per-cluster: the existing `primed` flag is a single bool. With one shared Explainer, the **first** `Consider` call from **any** cluster consumes the prime. The test above accounts for this by priming with an empty delta before the two real ones. Do **not** change `primed` to be per-cluster — a restart should still not spend the budget on pre-existing problems, and the first real transition from any cluster arrives after every worker has reconciled at least once.

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/oncall/ 2>&1 | head -20
```

Expected: FAIL — `too many arguments in call to ex.Consider` and `x.Cluster undefined`.

- [ ] **Step 3: Thread the cluster through oncall**

In `internal/oncall/oncall.go`:

Add the field to `Explanation`:

```go
// Explanation is one delivered explanation, as served by /explanations.
type Explanation struct {
	Cluster     string
	Kind        string
	Namespace   string
	Name        string
	Issues      []string
	ExplainedAt time.Time
	Model       string
	Text        string
}
```

Change `Consider`'s signature and its call into `objectsFrom`. The existing parameter named `cluster` holds cluster **health**, not a name, so it is renamed `health` to free the name:

```go
func (e *Explainer) Consider(clusterName string, d watchstate.Delta, health clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue, now time.Time) {
```

Inside, the only other change is the loop head and the prompt call:

```go
	for _, obj := range objectsFrom(clusterName, d.New) {
```

```go
			prompt: explain.BuildIncidentPrompt(obj.obj.String(), obj.issues, health, flagged, serviceIssues),
```

`obj.obj.String()` now begins with the cluster name, which is exactly what the prompt should say — it is the object's identity, and it names no pod, IP or node.

Change `objectsFrom` to stamp the cluster into both the throttle key and the Object:

```go
// objectsFrom folds per-issue records into one entry per object, in a stable
// order so a storm produces a deterministic admission sequence rather than one
// that depends on map iteration. The cluster is part of the key: one Explainer
// serves every cluster, so two clusters running the same namespace and name
// would otherwise share a cooldown slot.
func objectsFrom(cluster string, records []watchstate.Record) []objectRef {
	index := map[string]*objectRef{}
	var order []string
	for _, r := range records {
		key := cluster + "/" + r.Key.Kind + "/" + r.Key.Namespace + "/" + r.Key.Name
		ref, ok := index[key]
		if !ok {
			ref = &objectRef{
				key: key,
				obj: alertstate.Object{Cluster: cluster, Kind: r.Key.Kind, Namespace: r.Key.Namespace, Name: r.Key.Name},
			}
			index[key] = ref
			order = append(order, key)
```

(the rest of `objectsFrom` is unchanged).

In `run`, carry the cluster into the stored explanation:

```go
	e.store(Explanation{
		Cluster: j.obj.Cluster,
		Kind:    j.obj.Kind, Namespace: j.obj.Namespace, Name: j.obj.Name,
		Issues: j.issues, ExplainedAt: now, Model: e.model, Text: text,
	})
```

In `store`, put the cluster in the store key for the same reason:

```go
	key := x.Cluster + "/" + x.Kind + "/" + x.Namespace + "/" + x.Name
```

- [ ] **Step 4: Fix the one caller and run the suite**

`internal/watch/watch.go` calls `ex.Consider(d, res.Health, ...)`. Update it to pass a cluster name; at this point in the plan `Run` still serves a single cluster, so use the package constant that Task 3 introduces. If Task 3 has not landed yet in your branch, add it now in `internal/watch/watch.go`:

```go
// defaultClusterName labels the target built without an explicit --context.
const defaultClusterName = "local"
```

and call:

```go
	ex.Consider(defaultClusterName, d, res.Health, flaggedWorkloads(res), res.ServiceIssues, now)
```

Then:

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head -30
```

Expected: every package passes. Any `oncall_test.go` or `watch_test.go` call to `Consider` with the old argument list must gain the cluster name as its first argument.

- [ ] **Step 5: Commit**

```bash
git add internal/oncall internal/watch
git add -u
git commit -m "feat(oncall): key the explanation throttle and store per cluster"
```

---

### Task 3: Metrics keyed by cluster

**Files:**
- Modify: `internal/watch/metrics.go`
- Modify: `internal/watch/watch.go` (call sites only)
- Test: `internal/watch/metrics_test.go`, `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: `defaultClusterName` (Task 2 step 4).
- Produces:
  - `func newMetrics(names []string) *metrics` — names are the configured target names; they are sorted and fixed for the process lifetime.
  - `func (m *metrics) update(cluster string, res *scan.Result, dur time.Duration, now time.Time, err error)`
  - `func (m *metrics) updateIssues(cluster string, tr *watchstate.Tracker, now time.Time)`
  - `func (m *metrics) updateSLO(cluster string, enabled bool, target float64, fast, slow slo.Report)`
  - `func (m *metrics) markReady(cluster string)` and `func (m *metrics) isReady() bool` (ready once every configured cluster has called `markReady`)
  - `func (m *metrics) updateAlerts(alert.Stats)` and `func (m *metrics) updateExplain(bool, oncall.Stats, []oncall.Explanation)` — unchanged, process-wide.
  - `type clusterSnapshot struct` holding every per-cluster field.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/metrics_test.go`:

```go
// TestRenderLabelsEveryPerClusterSeries pins the label contract. It is emitted
// even with one cluster: a label that only appears once a second cluster is
// added would break every dashboard on the day an operator adds their second
// cluster, which is the worst possible moment.
func TestRenderLabelsEveryPerClusterSeries(t *testing.T) {
	m := newMetrics([]string{"prod-us", "prod-eu"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	m.update("prod-eu", sampleResult(), time.Millisecond, at, nil)
	m.update("prod-us", sampleResult(), time.Millisecond, at, nil)

	out := m.render()
	for _, want := range []string{
		`kubeagent_cluster_healthy{cluster="prod-eu"}`,
		`kubeagent_cluster_healthy{cluster="prod-us"}`,
		`kubeagent_nodes_ready{cluster="prod-eu"}`,
		`kubeagent_workloads_flagged{cluster="prod-us"}`,
		`kubeagent_scans_total{cluster="prod-eu"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render() missing %s\n%s", want, out)
		}
	}

	// Clusters render in sorted order, so the output is stable across restarts.
	if strings.Index(out, `kubeagent_cluster_healthy{cluster="prod-eu"}`) >
		strings.Index(out, `kubeagent_cluster_healthy{cluster="prod-us"}`) {
		t.Error("clusters must render in sorted order")
	}
}

// TestRenderEmitsOneHelpPerFamily pins the exposition format. Prometheus rejects
// a scrape that repeats HELP for a family, so the header cannot move inside the
// per-cluster loop.
func TestRenderEmitsOneHelpPerFamily(t *testing.T) {
	m := newMetrics([]string{"a", "b", "c"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	for _, c := range []string{"a", "b", "c"} {
		m.update(c, sampleResult(), time.Millisecond, at, nil)
	}
	if got := strings.Count(m.render(), "# HELP kubeagent_nodes_ready "); got != 1 {
		t.Errorf("HELP for kubeagent_nodes_ready appears %d times, want 1", got)
	}
}

// TestRenderLeavesProcessWideSeriesUnlabelled pins the other half of the
// contract: there is one alert sink and one explanation budget, so labelling
// their counters per cluster would attribute a process-wide number to a cluster
// that did not produce it.
func TestRenderLeavesProcessWideSeriesUnlabelled(t *testing.T) {
	m := newMetrics([]string{"prod-eu", "prod-us"})
	m.updateAlerts(alert.Stats{FiringOK: 3})
	out := m.render()
	if !strings.Contains(out, `kubeagent_alerts_sent_total{status="firing",outcome="ok"} 3`) {
		t.Errorf("alert series must keep exactly its existing labels\n%s", out)
	}
	if strings.Contains(out, `kubeagent_alerts_sent_total{cluster=`) {
		t.Error("alert series must not carry a cluster label")
	}
	if !strings.Contains(out, "kubeagent_clusters_total 2\n") {
		t.Errorf("kubeagent_clusters_total must be unlabelled and equal the target count\n%s", out)
	}
}

// TestClusterUpReportsPerClusterEvaluationOutcome pins the degradation signal:
// one cluster erroring must not disturb the others' readings.
func TestClusterUpReportsPerClusterEvaluationOutcome(t *testing.T) {
	m := newMetrics([]string{"good", "bad"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)
	m.update("good", sampleResult(), time.Millisecond, at, nil)
	m.update("bad", &scan.Result{}, time.Millisecond, at, errors.New("connection refused"))

	out := m.render()
	if !strings.Contains(out, `kubeagent_cluster_up{cluster="good"} 1`) {
		t.Errorf("healthy cluster must report up=1\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_cluster_up{cluster="bad"} 0`) {
		t.Errorf("erroring cluster must report up=0\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_scan_errors_total{cluster="bad"} 1`) {
		t.Errorf("the error must be counted against its own cluster\n%s", out)
	}
	if !strings.Contains(out, `kubeagent_scan_errors_total{cluster="good"} 0`) {
		t.Errorf("the healthy cluster must not inherit the error\n%s", out)
	}
}

// TestIsReadyWaitsForEveryCluster pins the readiness rule: ready means "every
// target has finished its first reconcile attempt", never "everything is fine".
func TestIsReadyWaitsForEveryCluster(t *testing.T) {
	m := newMetrics([]string{"a", "b"})
	if m.isReady() {
		t.Fatal("must not be ready before any cluster reports")
	}
	m.markReady("a")
	if m.isReady() {
		t.Error("must not be ready with one cluster outstanding")
	}
	m.markReady("b")
	if !m.isReady() {
		t.Error("must be ready once every cluster has reported")
	}
}
```

Add `"errors"` and `"strings"` to the test file's imports if absent, plus `"github.com/imantaba/kubeagent/internal/alert"` and `"github.com/imantaba/kubeagent/internal/scan"` if absent.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ 2>&1 | head -20
```

Expected: FAIL — `too many arguments in call to newMetrics` / `too many arguments in call to m.update`.

- [ ] **Step 3: Restructure the metrics snapshot**

In `internal/watch/metrics.go`, replace the `metrics` struct, `newMetrics`, and every `update*`/`markReady`/`isReady` method with:

```go
// clusterSnapshot is one cluster's evaluation state as of its last reconcile.
// Every field here was a field on metrics before the daemon learned to watch
// more than one cluster; the split is what keeps two workers from clobbering
// each other's readings.
type clusterSnapshot struct {
	up                    bool
	lastError             string
	healthy               float64
	nodesReady            int
	nodesTotal            int
	nodesNoReserve        int
	nodesStaleHeartbeat   int
	nodesExpectedAbsent   int
	kubeletUnhealthy      int
	controlPlaneUnhealthy int
	dnsServfailRatio      float64
	pvcsReclaimDelete     int
	flagged               int
	serviceIssues         int
	ingressIssues         int
	pvcPendingIssues      int
	stuckTerminating      int
	pdbBlockingIssues     int
	hpaScalingIssues      int
	webhooksFailing       int
	webhookLatencyRisks   int
	quotaIssues           int
	findings              map[string]int
	lastScanUnix          int64
	scanSeconds           float64
	scansTotal            int64
	scanErrors            int64
	nodeFSRatio           map[string]float64
	volumesOverDisk       int
	certsRan              bool
	certsExpired          int
	certsExpiring         int
	issues                issueSnapshot
	slo                   sloSnapshot
}

// metrics holds one snapshot per watched cluster plus the process-wide alert and
// explanation state, and renders the lot as Prometheus text. All access is
// mutex-guarded; each cluster's worker updates its own snapshot and the HTTP
// handler reads them all.
type metrics struct {
	mu       sync.RWMutex
	names    []string // configured target names, sorted; fixed for the process lifetime
	clusters map[string]*clusterSnapshot
	pending  map[string]bool // clusters yet to finish a first reconcile attempt
	alerts   alert.Stats     // process-wide: one sink
	explain  explainSnapshot // process-wide: one budget
}

// newMetrics pre-creates a snapshot per cluster so kubeagent_cluster_up renders
// 0 for a cluster that has not reported yet, rather than the series being absent
// — an absent series and a down cluster look identical on a dashboard, and they
// are not the same thing.
func newMetrics(names []string) *metrics {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	m := &metrics{
		names:    sorted,
		clusters: make(map[string]*clusterSnapshot, len(sorted)),
		pending:  make(map[string]bool, len(sorted)),
	}
	for _, n := range sorted {
		m.clusters[n] = &clusterSnapshot{findings: map[string]int{}}
		m.pending[n] = true
	}
	return m
}

// snapshot returns the named cluster's snapshot, creating one if the caller
// names a cluster newMetrics did not know about. That cannot happen with a
// validated target list, but a nil map entry would panic under the write lock
// and take the whole daemon down, which is a worse failure than an extra series.
func (m *metrics) snapshot(cluster string) *clusterSnapshot {
	c, ok := m.clusters[cluster]
	if !ok {
		c = &clusterSnapshot{findings: map[string]int{}}
		m.clusters[cluster] = c
		m.names = append(m.names, cluster)
		sort.Strings(m.names)
	}
	return c
}

// update records one reconcile for one cluster. On err != nil only the
// attempt/error counters, the timing and the up flag move; the last good
// snapshot of that cluster's gauges is preserved, and no other cluster is
// touched.
func (m *metrics) update(cluster string, res *scan.Result, dur time.Duration, now time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.snapshot(cluster)
	c.scansTotal++
	c.scanSeconds = dur.Seconds()
	c.lastScanUnix = now.Unix()
	if err != nil {
		c.scanErrors++
		c.up = false
		c.lastError = err.Error()
		return
	}
	c.up = true
	c.lastError = ""
	if res.Health.Verdict == "Healthy" {
		c.healthy = 1
	} else {
		c.healthy = 0
	}
	c.nodesReady = res.Health.NodesReady
	c.nodesTotal = res.Health.NodesTotal
	c.nodesNoReserve = res.NodeReserve.WarnCount
	c.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat
	c.nodesExpectedAbsent = res.Health.NodesExpectedAbsent
	c.kubeletUnhealthy = len(res.KubeletHealth.Unhealthy)
	c.controlPlaneUnhealthy = 0
	if res.ControlPlane.Status == "unhealthy" {
		c.controlPlaneUnhealthy = 1
	}
	c.dnsServfailRatio = res.DNS.ServfailRatio
	c.pvcsReclaimDelete = res.PVCReclaim.Count
	c.serviceIssues = realServiceIssues(res.ServiceIssues)
	c.ingressIssues = realIngressIssues(res.IngressIssues)
	c.pvcPendingIssues = len(res.PVCIssues)
	c.stuckTerminating = len(res.StuckTerminating)
	c.pdbBlockingIssues = len(res.PDBIssues)
	c.hpaScalingIssues = len(res.HPAIssues)
	c.webhooksFailing = 0
	c.webhookLatencyRisks = 0
	for _, i := range res.WebhookIssues {
		if i.Problem == "high-timeout" {
			c.webhookLatencyRisks++
		} else {
			c.webhooksFailing++
		}
	}
	c.quotaIssues = len(res.QuotaIssues)
	flagged := 0
	findings := map[string]int{}
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			flagged++
		}
		for _, f := range w.Findings {
			findings[f.Issue]++
		}
	}
	c.flagged = flagged
	c.findings = findings
	if len(res.DiskUsage.Nodes) > 0 {
		ratios := make(map[string]float64, len(res.DiskUsage.Nodes))
		for _, n := range res.DiskUsage.Nodes {
			ratios[n.Node] = n.Ratio
		}
		c.nodeFSRatio = ratios
		c.volumesOverDisk = len(res.DiskUsage.Over)
	}
	if res.Certificates != nil {
		c.certsRan = true
		c.certsExpired = len(res.Certificates.Expired)
		c.certsExpiring = len(res.Certificates.Expiring)
	}
}

// markReady records that this cluster has finished its first reconcile attempt.
func (m *metrics) markReady(cluster string) {
	m.mu.Lock()
	delete(m.pending, cluster)
	m.mu.Unlock()
}

// isReady reports whether every configured cluster has finished a first
// reconcile attempt — success or failure. Readiness answers "can this process
// serve?", not "is everything fine": tying it to cluster health would let one
// unreachable remote cluster pull the pod out of its Service endpoints, stopping
// Prometheus from scraping it, and so blind the operator to the clusters that
// are working.
func (m *metrics) isReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.pending) == 0
}

// updateIssues records one cluster's tracker state for rendering. now becomes
// that snapshot's reference time for every age it reports.
func (m *metrics) updateIssues(cluster string, tr *watchstate.Tracker, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot(cluster).issues = issueSnapshot{At: now, Active: tr.Active(), Resolved: tr.RecentlyResolved(), Stats: tr.Stats()}
}

// updateSLO records one cluster's latest SLO report. Each cluster burns its own
// error budget: an availability SLO computed across clusters would be meaningless.
func (m *metrics) updateSLO(cluster string, enabled bool, target float64, fast, slow slo.Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot(cluster).slo = sloSnapshot{Enabled: enabled, Target: target, Fast: fast, Slow: slow}
}
```

Leave `updateAlerts` and `updateExplain` exactly as they are — they write `m.alerts` and `m.explain`, which stay process-wide.

- [ ] **Step 4: Rewrite render()**

Replace the whole `render()` method in `internal/watch/metrics.go` with:

```go
func (m *metrics) render() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var b strings.Builder

	// A per-cluster family renders HELP and TYPE once, then one sample line per
	// cluster. Prometheus rejects a repeated HELP for the same family, so the
	// header cannot move inside the loop.
	gauge := func(name, help string, get func(*clusterSnapshot) float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, n := range m.names {
			fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(m.clusters[n]))
		}
	}
	counter := func(name, help string, get func(*clusterSnapshot) int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, n := range m.names {
			fmt.Fprintf(&b, "%s{cluster=%q} %d\n", name, n, get(m.clusters[n]))
		}
	}
	// A process-wide family carries no cluster label: there is one alert sink
	// and one explanation budget, so attributing their counters to a cluster
	// would be false.
	plainGauge := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
	}
	plainCounter := func(name, help string, v int64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
	}
	plainCounterF := func(name, help string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", name, help, name, name, v)
	}
	bit := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}

	plainGauge("kubeagent_clusters_total", "Clusters this daemon is configured to watch", float64(len(m.names)))
	gauge("kubeagent_cluster_up", "1 if the last evaluation of this cluster succeeded, else 0", func(c *clusterSnapshot) float64 { return bit(c.up) })
	gauge("kubeagent_cluster_healthy", "1 if the cluster verdict is Healthy, else 0", func(c *clusterSnapshot) float64 { return c.healthy })
	gauge("kubeagent_nodes_ready", "Number of Ready nodes", func(c *clusterSnapshot) float64 { return float64(c.nodesReady) })
	gauge("kubeagent_nodes_total", "Total number of nodes", func(c *clusterSnapshot) float64 { return float64(c.nodesTotal) })
	gauge("kubeagent_nodes_without_reservations", "Nodes whose kubelet reserves no memory (allocatable == capacity)", func(c *clusterSnapshot) float64 { return float64(c.nodesNoReserve) })
	gauge("kubeagent_nodes_stale_heartbeat", "Ready nodes whose kubelet lease is stale (kubelet not heartbeating)", func(c *clusterSnapshot) float64 { return float64(c.nodesStaleHeartbeat) })
	gauge("kubeagent_nodes_expected_absent", "Declared expected nodes that are absent from the cluster", func(c *clusterSnapshot) float64 { return float64(c.nodesExpectedAbsent) })
	gauge("kubeagent_kubelet_unhealthy", "Nodes whose kubelet /healthz reported unhealthy", func(c *clusterSnapshot) float64 { return float64(c.kubeletUnhealthy) })
	gauge("kubeagent_control_plane_unhealthy", "Apiserver /readyz reported the control plane not ready", func(c *clusterSnapshot) float64 { return float64(c.controlPlaneUnhealthy) })
	gauge("kubeagent_dns_servfail_ratio", "CoreDNS SERVFAIL+REFUSED response ratio (0 when healthy or not probed)", func(c *clusterSnapshot) float64 { return c.dnsServfailRatio })
	gauge("kubeagent_pvcs_reclaim_delete", "PVCs whose bound PV has reclaimPolicy Delete", func(c *clusterSnapshot) float64 { return float64(c.pvcsReclaimDelete) })
	gauge("kubeagent_workloads_flagged", "Number of workloads currently flagged", func(c *clusterSnapshot) float64 { return float64(c.flagged) })
	gauge("kubeagent_service_issues", "Number of Service issues", func(c *clusterSnapshot) float64 { return float64(c.serviceIssues) })
	gauge("kubeagent_ingress_route_issues", "Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port", func(c *clusterSnapshot) float64 { return float64(c.ingressIssues) })
	gauge("kubeagent_pvc_pending_issues", "PVCs stuck Pending because provisioning or binding failed", func(c *clusterSnapshot) float64 { return float64(c.pvcPendingIssues) })
	gauge("kubeagent_resources_stuck_terminating", "Resources (namespaces, pods, PVCs) wedged in Terminating past the threshold", func(c *clusterSnapshot) float64 { return float64(c.stuckTerminating) })
	gauge("kubeagent_pdb_blocking_issues", "PodDisruptionBudgets that will block a node drain", func(c *clusterSnapshot) float64 { return float64(c.pdbBlockingIssues) })
	gauge("kubeagent_hpa_scaling_issues", "HorizontalPodAutoscalers that cannot scale as intended", func(c *clusterSnapshot) float64 { return float64(c.hpaScalingIssues) })
	gauge("kubeagent_admission_webhooks_failing", "Fail-policy admission webhooks whose backend is missing or has no ready endpoints", func(c *clusterSnapshot) float64 { return float64(c.webhooksFailing) })
	gauge("kubeagent_admission_webhook_latency_risks", "Fail-policy admission webhooks with a high timeoutSeconds (a latency landmine)", func(c *clusterSnapshot) float64 { return float64(c.webhookLatencyRisks) })
	gauge("kubeagent_resourcequota_issues", "ResourceQuota entries at or over the usage threshold", func(c *clusterSnapshot) float64 { return float64(c.quotaIssues) })

	fmt.Fprintf(&b, "# HELP kubeagent_findings Current findings by issue type\n# TYPE kubeagent_findings gauge\n")
	for _, n := range m.names {
		c := m.clusters[n]
		issues := make([]string, 0, len(c.findings))
		for k := range c.findings {
			issues = append(issues, k)
		}
		sort.Strings(issues)
		for _, k := range issues {
			fmt.Fprintf(&b, "kubeagent_findings{cluster=%q,issue=%q} %d\n", n, k, c.findings[k])
		}
	}

	gauge("kubeagent_issues_active", "Issues currently firing, tracked across reconciles", func(c *clusterSnapshot) float64 { return float64(len(c.issues.Active)) })
	gauge("kubeagent_issues_flapping", "Active issues that have crossed the flap threshold", func(c *clusterSnapshot) float64 {
		n := 0
		for _, r := range c.issues.Active {
			if r.Flapping {
				n++
			}
		}
		return float64(n)
	})
	counter("kubeagent_issues_new_total", "Issue firings observed since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.NewTotal })
	counter("kubeagent_issues_resolved_total", "Issue firings that resolved since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.ResolvedTotal })
	counter("kubeagent_issues_flapping_total", "Times an issue crossed the flap threshold since start", func(c *clusterSnapshot) int64 { return c.issues.Stats.FlapTotal })
	counter("kubeagent_issues_dropped_total", "New issues left untracked because the tracker is at capacity", func(c *clusterSnapshot) int64 { return c.issues.Stats.DroppedTotal })
	gauge("kubeagent_issue_resolution_seconds_sum", "Seconds issues spent firing before resolving (MTTR numerator)", func(c *clusterSnapshot) float64 { return c.issues.Stats.ResolutionSecondsSum })
	counter("kubeagent_issue_resolution_seconds_count", "Issue firings that resolved (MTTR denominator)", func(c *clusterSnapshot) int64 { return c.issues.Stats.ResolutionSecondsCount })

	anyActive := false
	for _, n := range m.names {
		if len(m.clusters[n].issues.Active) > 0 {
			anyActive = true
			break
		}
	}
	if anyActive {
		fmt.Fprintf(&b, "# HELP kubeagent_issue_active 1 while this issue instance is firing\n# TYPE kubeagent_issue_active gauge\n")
		for _, n := range m.names {
			for _, r := range m.clusters[n].issues.Active {
				fmt.Fprintf(&b, "kubeagent_issue_active{%s} 1\n", issueLabels(n, r.Key))
			}
		}
		fmt.Fprintf(&b, "# HELP kubeagent_issue_age_seconds Seconds since this issue instance started firing\n# TYPE kubeagent_issue_age_seconds gauge\n")
		for _, n := range m.names {
			c := m.clusters[n]
			for _, r := range c.issues.Active {
				fmt.Fprintf(&b, "kubeagent_issue_age_seconds{%s} %d\n", issueLabels(n, r.Key), ageSeconds(r.FiringSince, c.issues.At))
			}
		}
	}

	anyFS := false
	for _, n := range m.names {
		if len(m.clusters[n].nodeFSRatio) > 0 {
			anyFS = true
			break
		}
	}
	if anyFS {
		fmt.Fprintf(&b, "# HELP kubeagent_node_fs_usage_ratio Node root filesystem used/capacity (0-1)\n# TYPE kubeagent_node_fs_usage_ratio gauge\n")
		for _, n := range m.names {
			c := m.clusters[n]
			nodes := make([]string, 0, len(c.nodeFSRatio))
			for k := range c.nodeFSRatio {
				nodes = append(nodes, k)
			}
			sort.Strings(nodes)
			for _, node := range nodes {
				fmt.Fprintf(&b, "kubeagent_node_fs_usage_ratio{cluster=%q,node=%q} %g\n", n, node, c.nodeFSRatio[node])
			}
		}
		gauge("kubeagent_volumes_over_disk_threshold", "Node+PVC volumes at or over the disk-usage threshold", func(c *clusterSnapshot) float64 { return float64(c.volumesOverDisk) })
	}

	anyCerts := false
	for _, n := range m.names {
		if m.clusters[n].certsRan {
			anyCerts = true
			break
		}
	}
	if anyCerts {
		certGauge := func(name, help string, get func(*clusterSnapshot) float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.certsRan {
					continue
				}
				fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(c))
			}
		}
		certGauge("kubeagent_certificates_expired", "TLS certificates already expired (opt-in --certs)", func(c *clusterSnapshot) float64 { return float64(c.certsExpired) })
		certGauge("kubeagent_certificates_expiring", "TLS certificates expiring within the warn window (opt-in --certs)", func(c *clusterSnapshot) float64 { return float64(c.certsExpiring) })
	}

	fmt.Fprintf(&b, "# HELP kubeagent_alerts_sent_total Alert notifications delivered since start\n# TYPE kubeagent_alerts_sent_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "ok", m.alerts.FiringOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "firing", "failed", m.alerts.FiringFailed)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "ok", m.alerts.ResolvedOK)
	fmt.Fprintf(&b, "kubeagent_alerts_sent_total{status=%q,outcome=%q} %d\n", "resolved", "failed", m.alerts.ResolvedFailed)
	fmt.Fprintf(&b, "# HELP kubeagent_alerts_dropped_total Alert notifications dropped without delivery\n# TYPE kubeagent_alerts_dropped_total counter\n")
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "queue_full", m.alerts.DroppedQueueFull)
	fmt.Fprintf(&b, "kubeagent_alerts_dropped_total{reason=%q} %d\n", "retries_exhausted", m.alerts.DroppedRetriesExhausted)
	plainGauge("kubeagent_alert_last_success_timestamp_seconds", "Unix time of the last successful alert delivery (0 if none)", float64(m.alerts.LastSuccessUnix))

	anySLO := false
	for _, n := range m.names {
		if m.clusters[n].slo.Enabled {
			anySLO = true
			break
		}
	}
	if anySLO {
		sloGauge := func(name, help string, get func(*clusterSnapshot) float64) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.slo.Enabled {
					continue
				}
				fmt.Fprintf(&b, "%s{cluster=%q} %g\n", name, n, get(c))
			}
		}
		sloWindowed := func(name, help string, get func(*clusterSnapshot) (float64, float64)) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
			for _, n := range m.names {
				c := m.clusters[n]
				if !c.slo.Enabled {
					continue
				}
				fast, slow := get(c)
				fmt.Fprintf(&b, "%s{cluster=%q,window=\"fast\"} %g\n", name, n, fast)
				fmt.Fprintf(&b, "%s{cluster=%q,window=\"slow\"} %g\n", name, n, slow)
			}
		}
		sloGauge("kubeagent_slo_target_ratio", "Configured availability SLO as a ratio", func(c *clusterSnapshot) float64 { return c.slo.Target })
		sloWindowed("kubeagent_slo_availability_ratio",
			"Time-weighted fraction of workload-seconds that are not flagged, over the window",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.Availability, c.slo.Slow.Availability })
		sloWindowed("kubeagent_slo_burn_rate",
			"Error-budget consumption multiple over the window (1 = spending exactly at budget)",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.BurnRate, c.slo.Slow.BurnRate })
		sloWindowed("kubeagent_slo_window_coverage_ratio",
			"Fraction of the window carrying samples; below 0.6 the burn alert is suppressed",
			func(c *clusterSnapshot) (float64, float64) { return c.slo.Fast.Coverage, c.slo.Slow.Coverage })
		// Clamped at zero: a burn above 1x means the window's budget is already
		// spent, and a negative "remaining" is nonsense on a dashboard.
		sloGauge("kubeagent_slo_error_budget_remaining_ratio",
			"Fraction of the error budget left over the slow window, clamped to [0,1]",
			func(c *clusterSnapshot) float64 {
				remaining := 1 - c.slo.Slow.BurnRate
				if remaining < 0 {
					remaining = 0
				}
				return remaining
			})
	}

	if m.explain.Enabled {
		plainCounter("kubeagent_explain_allowed_total", "Incident explanations the throttle admitted since start", m.explain.Stats.Allowed)
		plainCounter("kubeagent_explain_throttled_total", "Incident explanations refused by the cooldown or the hourly budget", m.explain.Stats.Throttled)
		plainCounter("kubeagent_explain_failed_total", "Incident explanations whose model call errored or returned no text", m.explain.Stats.Failed)
		plainCounter("kubeagent_explain_dropped_total", "Incident explanations admitted but dropped because the worker queue was full", m.explain.Stats.Dropped)
		plainGauge("kubeagent_explain_budget_remaining", "Model calls left in the hourly budget", m.explain.Stats.BudgetRemaining)
	}

	gauge("kubeagent_last_scan_timestamp_seconds", "Unix time of the last evaluation", func(c *clusterSnapshot) float64 { return float64(c.lastScanUnix) })
	gauge("kubeagent_scan_duration_seconds", "Duration of the last evaluation in seconds", func(c *clusterSnapshot) float64 { return c.scanSeconds })
	counter("kubeagent_scans_total", "Total evaluations run", func(c *clusterSnapshot) int64 { return c.scansTotal })
	counter("kubeagent_scan_errors_total", "Total evaluations that errored", func(c *clusterSnapshot) int64 { return c.scanErrors })
	_ = plainCounterF
	return b.String()
}
```

Then delete the `_ = plainCounterF` line and the `plainCounterF` helper if nothing uses it — `kubeagent_issue_resolution_seconds_sum` is now rendered by the per-cluster `gauge` helper above (it was a float counter before; keep the `# TYPE ... counter` semantics by writing it with a dedicated helper instead if `go vet` or the reviewer objects to the type change). To avoid the type change entirely, replace that one call with:

```go
	fmt.Fprintf(&b, "# HELP kubeagent_issue_resolution_seconds_sum Seconds issues spent firing before resolving (MTTR numerator)\n# TYPE kubeagent_issue_resolution_seconds_sum counter\n")
	for _, n := range m.names {
		fmt.Fprintf(&b, "kubeagent_issue_resolution_seconds_sum{cluster=%q} %g\n", n, m.clusters[n].issues.Stats.ResolutionSecondsSum)
	}
```

and drop both `plainCounterF` and the `_ = plainCounterF` line. Use this form — the metric must stay a counter.

Update `issueLabels` to lead with the cluster:

```go
func issueLabels(cluster string, k watchstate.Key) string {
	return fmt.Sprintf("cluster=%q,kind=%q,namespace=%q,name=%q,issue=%q", cluster, k.Kind, k.Namespace, k.Name, k.Issue)
}
```

- [ ] **Step 5: Update the call sites in watch.go**

In `internal/watch/watch.go`, `Run` currently calls `newMetrics()`, `m.markReady()`, and `applyResult` calls `m.update(...)`, `m.updateIssues(...)`, `m.updateSLO(...)`. Pass `defaultClusterName` to each:

```go
	m := newMetrics([]string{defaultClusterName})
```

```go
	m.markReady(defaultClusterName)
```

and in `applyResult`:

```go
	m.update(defaultClusterName, res, dur, now, err)
```
```go
	m.updateSLO(defaultClusterName, true, sloTr.Target(), v.Fast, v.Slow)
```
```go
	m.updateIssues(defaultClusterName, tr, now)
```

Task 4 replaces these constants with the worker's own name.

- [ ] **Step 6: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/watch/ 2>&1 | tail -40
```

Expected: the five new tests pass. Existing `metrics_test.go` assertions that expect an unlabelled series (`kubeagent_nodes_ready 3`) now fail — update each to the labelled form (`kubeagent_nodes_ready{cluster="local"} 3`) and update `newMetrics()` calls to `newMetrics([]string{"local"})`. Do not weaken an assertion to a substring that would pass without the label.

- [ ] **Step 7: Run the whole suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./... 2>&1 | grep -v "^ok" | head -20
```

Expected: no output beyond the trailing blank — every package passes.

- [ ] **Step 8: Commit**

```bash
git add internal/watch
git commit -m "feat(watch): key every metric snapshot and series by cluster"
```

---

### Task 4: Per-cluster workers

**Files:**
- Create: `internal/watch/target.go`
- Create: `internal/watch/cluster.go`
- Modify: `internal/watch/watch.go`
- Modify: `internal/watch/slo.go` (`logSLO` signature)
- Test: `internal/watch/target_test.go` (new), `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: `newMetrics([]string)`, `m.update(cluster, …)`, `m.markReady(cluster)`, `m.updateIssues(cluster, …)`, `m.updateSLO(cluster, …)` (Task 3); `alertstate.Options{Cluster}` (Task 1); `ex.Consider(clusterName, …)` (Task 2).
- Produces:
  - `type Target struct { Name string; Client kubernetes.Interface }`
  - `func Run(ctx context.Context, targets []Target, cfg Config) error`
  - `type clusterWorker struct` with fields `name string`, `m *metrics`, `tr *watchstate.Tracker`, `al *alerter`, `ex *oncall.Explainer`, `sloTr *slo.Tracker`, `sloN *sloNotifier`, `factory informers.SharedInformerFactory`, `client kubernetes.Interface`, `opts scan.Options`, `cfg Config`
  - `func (w *clusterWorker) applyResult(res *scan.Result, dur time.Duration, now time.Time, err error)` — replaces the free function `applyResult`
  - `func clusterLogf(cluster, format string, args ...any)`

- [ ] **Step 1: Write the failing tests**

Create `internal/watch/target_test.go`:

```go
package watch

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestValidateTargets pins the startup contract. Client construction contacts no
// API server, so anything wrong at this point is a configuration error — a
// misspelled context — and must be fatal. Degrading into silently watching two
// of the three clusters an operator asked for is the failure mode this prevents.
func TestValidateTargets(t *testing.T) {
	ok := fake.NewSimpleClientset()
	tests := []struct {
		name    string
		targets []Target
		wantErr string
	}{
		{"empty list", nil, "no clusters"},
		{"empty name", []Target{{Name: "", Client: ok}}, "empty"},
		{"nil client", []Target{{Name: "a"}}, "no client"},
		{"duplicate names", []Target{{Name: "a", Client: ok}, {Name: "a", Client: ok}}, "duplicate"},
		{"valid", []Target{{Name: "a", Client: ok}, {Name: "b", Client: ok}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTargets(tc.targets)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTargets = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTargets = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
```

Append to `internal/watch/watch_test.go`:

```go
// TestRun_OneBrokenClusterDoesNotStopTheOthers is the isolation guarantee. A
// remote cluster going away must degrade to a per-cluster reading, not take the
// daemon with it — and /readyz must still report ready, because a NotReady pod
// leaves its Service endpoints and Prometheus then stops scraping the clusters
// that ARE working.
func TestRun_OneBrokenClusterDoesNotStopTheOthers(t *testing.T) {
	good := fake.NewSimpleClientset()
	bad := fake.NewSimpleClientset()
	bad.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, []Target{{Name: "good", Client: good}, {Name: "bad", Client: bad}}, Config{
			MetricsAddr: addr,
			Heartbeat:   time.Hour,
			Debounce:    10 * time.Millisecond,
		})
	}()

	// Ready means "every cluster finished a first attempt", so this returning 200
	// is itself the assertion that the broken cluster did not wedge readiness.
	waitForReady(t, "http://"+addr+"/readyz")

	body := httpGetBody(t, "http://"+addr+"/metrics")
	if !strings.Contains(body, `kubeagent_cluster_up{cluster="good"} 1`) {
		t.Errorf("working cluster must report up=1\n%s", body)
	}
	if !strings.Contains(body, `kubeagent_cluster_up{cluster="bad"} 0`) {
		t.Errorf("broken cluster must report up=0\n%s", body)
	}
	if !strings.Contains(body, "kubeagent_clusters_total 2") {
		t.Errorf("both clusters must be counted\n%s", body)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of cancellation")
	}
}

// TestRun_RejectsADuplicateClusterNameBeforeStartingAnything pins that the
// target check runs with the other config validation, before the metrics server
// listens: once WaitForCacheSync is underway a reachable-but-unresponsive API
// server can hide a config error behind what looks like a cluster hang.
func TestRun_RejectsADuplicateClusterNameBeforeStartingAnything(t *testing.T) {
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := fake.NewSimpleClientset()

	err := Run(ctx, []Target{{Name: "dup", Client: c}, {Name: "dup", Client: c}}, Config{
		MetricsAddr: addr,
		Heartbeat:   time.Hour,
		Debounce:    time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Run = %v, want a duplicate-name error", err)
	}
	if _, dialErr := net.DialTimeout("tcp", addr, 200*time.Millisecond); dialErr == nil {
		t.Error("the metrics server must not be listening after a rejected config")
	}
}
```

**Note on the helpers:** `internal/watch/watch_test.go` already has tests that pick a free loopback address, poll `/readyz`, and read a body (see `TestRun_GracefulShutdown` and `TestRun_RejectsBadSLOTargetBeforeCacheSync`). Read them and reuse the existing helpers, renaming the calls above to match. Only if none exist, add:

```go
// freeLoopbackAddr reserves a loopback port and releases it, so the daemon can
// bind it. Racy in principle, fine in a test binary.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitForReady(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reported ready within 10s", url)
}

func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return string(body)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ 2>&1 | head -20
```

Expected: FAIL — `undefined: Target`, `undefined: validateTargets`.

- [ ] **Step 3: Create the Target type**

Create `internal/watch/target.go`:

```go
package watch

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
)

// defaultClusterName labels the target built without an explicit --context: the
// in-cluster ServiceAccount, or the kubeconfig's current-context outside a
// cluster.
const defaultClusterName = "local"

// Target is one cluster the daemon watches. The name is the operator's label for
// it — the --context they typed, or --cluster-name for the default target — and
// it becomes the cluster label on every metric series and the cluster field on
// every issue, explanation and alert.
type Target struct {
	Name   string
	Client kubernetes.Interface
}

// validateTargets rejects a target list that cannot produce a coherent daemon.
// Every failure here is a configuration error, and every one of them is fatal:
// an operator who asked for three clusters and got two silently is worse off
// than one whose daemon refused to start.
func validateTargets(targets []Target) error {
	if len(targets) == 0 {
		return fmt.Errorf("no clusters to watch")
	}
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.Name == "" {
			return fmt.Errorf("a cluster name cannot be empty")
		}
		if t.Client == nil {
			return fmt.Errorf("cluster %q has no client", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate cluster name %q: every watched cluster needs a distinct name, because the name is the metric label", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// targetNames returns the target names in input order; newMetrics sorts them.
func targetNames(targets []Target) []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	return names
}
```

Remove the `defaultClusterName` constant from `internal/watch/watch.go` if Task 2 added it there — it lives in `target.go` now.

- [ ] **Step 4: Create the per-cluster worker**

Create `internal/watch/cluster.go`:

```go
package watch

import (
	"context"
	"fmt"
	"log"
	"time"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/oncall"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/slo"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

// cacheSyncTimeout bounds how long one cluster's informers may take to fill
// before the worker proceeds anyway. An unreachable cluster would otherwise
// block in WaitForCacheSync forever, and with the readiness rule being "every
// cluster has finished a first attempt", that one cluster would hold the whole
// daemon out of its Service endpoints. The informers keep retrying in the
// background on the daemon's own context, so a cluster that comes back later
// simply starts producing successful evaluations.
const cacheSyncTimeout = 30 * time.Second

// clusterLogf prefixes a daemon log line with the cluster it concerns. Without
// it, N interleaved reconcile loops produce an unreadable log.
func clusterLogf(cluster, format string, args ...any) {
	log.Printf("kubeagent: ["+cluster+"] "+format, args...)
}

// clusterWorker owns everything that is per cluster: the informers, the issue
// tracker, the alert roller and the SLO tracker. The alert sink, the explainer
// and the metrics snapshot are shared and are handed in.
type clusterWorker struct {
	name    string
	client  kubernetes.Interface
	cfg     Config
	opts    scan.Options
	factory informers.SharedInformerFactory
	trigger chan struct{}

	m     *metrics
	al    *alerter
	ex    *oncall.Explainer
	tr    *watchstate.Tracker
	sloTr *slo.Tracker
	sloN  *sloNotifier
}

// newClusterWorker builds one cluster's worker and registers its informer
// handlers. It is deliberately fallible and deliberately called synchronously
// from Run, before any goroutine starts: a handler that cannot be registered is
// a startup failure, not something to discover in a background goroutine that
// has no way to report it.
func newClusterWorker(t Target, cfg Config, m *metrics, al *alerter, ex *oncall.Explainer) (*clusterWorker, error) {
	var factory informers.SharedInformerFactory
	if cfg.Namespace != "" {
		factory = informers.NewSharedInformerFactoryWithOptions(t.Client, 0, informers.WithNamespace(cfg.Namespace))
	} else {
		factory = informers.NewSharedInformerFactory(t.Client, 0)
	}
	w := &clusterWorker{
		name:    t.Name,
		client:  t.Client,
		cfg:     cfg,
		factory: factory,
		trigger: make(chan struct{}, 1),
		m:       m,
		al:      al,
		ex:      ex,
		tr:      watchstate.New(watchstate.Options{}),
		opts: scan.Options{
			Namespace: cfg.Namespace, IncludeCron: cfg.IncludeCron, IncludeRestarts: cfg.IncludeRestarts,
			DiskUsage: cfg.DiskUsage, DiskThreshold: cfg.DiskThreshold, QuotaThreshold: cfg.QuotaThreshold,
			NodeHeartbeatThreshold: cfg.NodeHeartbeatThreshold, ExpectedNodes: cfg.ExpectedNodes,
			KubeletHealth: cfg.KubeletHealth, ControlPlaneHealth: cfg.ControlPlaneHealth,
			DNSHealth: cfg.DNSHealth, DNSServfailRatio: cfg.DNSServfailRatio,
			Certs: cfg.Certs, CertWarnDays: cfg.CertWarnDays, WebhookTimeoutThreshold: cfg.WebhookTimeoutThreshold,
		},
	}
	w.sloTr, w.sloN = newSLOTracker(cfg)

	enqueue := func() {
		select {
		case w.trigger <- struct{}{}:
		default: // already pending
		}
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { enqueue() },
		UpdateFunc: func(interface{}, interface{}) { enqueue() },
		DeleteFunc: func(interface{}) { enqueue() },
	}
	for _, inf := range []cache.SharedIndexInformer{
		factory.Core().V1().Pods().Informer(),
		factory.Apps().V1().Deployments().Informer(),
		factory.Apps().V1().ReplicaSets().Informer(),
		factory.Core().V1().Nodes().Informer(),
		factory.Core().V1().Services().Informer(),
		factory.Discovery().V1().EndpointSlices().Informer(),
	} {
		if _, err := inf.AddEventHandler(h); err != nil {
			return nil, fmt.Errorf("cluster %q: adding informer handler: %w", t.Name, err)
		}
	}
	return w, nil
}

// run drives one cluster until ctx is cancelled.
func (w *clusterWorker) run(ctx context.Context) {
	w.factory.Start(ctx.Done())

	// The sync wait is bounded but the informers are not: factory.Start above
	// runs them on ctx, so they keep retrying after this returns.
	syncCtx, cancelSync := context.WithTimeout(ctx, cacheSyncTimeout)
	synced := w.factory.WaitForCacheSync(syncCtx.Done())
	cancelSync()
	for _, ok := range synced {
		if !ok {
			clusterLogf(w.name, "warning: informer caches did not fully sync within %s; evaluating anyway (the informers keep retrying)", cacheSyncTimeout)
			break
		}
	}
	clusterLogf(w.name, "watching cluster (namespace=%q, heartbeat=%s)", scopeLabel(w.cfg.Namespace), w.cfg.Heartbeat)
	if w.sloTr != nil {
		clusterLogf(w.name, "SLO burn-rate tracking enabled (target=%g%%, windows 1h/6h, alert suppressed below %d%% window coverage)",
			w.cfg.SLOTarget*100, 60)
	}

	w.reconcile(ctx)
	// Ready as soon as the FIRST ATTEMPT is done, success or failure. Readiness
	// answers "can this process serve?", not "is this cluster fine".
	w.m.markReady(w.name)

	heartbeat := time.NewTicker(w.cfg.Heartbeat)
	defer heartbeat.Stop()
	debounce := time.NewTimer(w.cfg.Debounce)
	debounce.Stop()
	defer debounce.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
			if !pending {
				pending = true
				debounce.Reset(w.cfg.Debounce)
			}
		case <-debounce.C:
			pending = false
			w.reconcile(ctx)
		case <-heartbeat.C:
			w.reconcile(ctx)
		}
	}
}

func (w *clusterWorker) reconcile(ctx context.Context) {
	start := time.Now()
	res, err := scan.Evaluate(ctx, w.client, w.opts)
	w.applyResult(&res, time.Since(start), time.Now(), err)
}

// applyResult folds one evaluation into the metrics, the issue tracker, the SLO
// tracker, and the shared alerter. A failed evaluation never reaches any
// tracker: an evaluation error is not "all clear", and treating it as one would
// resolve every tracked issue, then re-fire them all on the next success — and
// page the on-call for an API blip. The SLO tracker sits on the same side of
// that return for the same reason: an API error is neither "all healthy" nor
// "all broken", so it must not become a sample. The gap shows up as reduced
// window coverage, which is the honest representation.
//
// sloTr and sloN are always produced together by newSLOTracker (both nil, or
// both set), but the struct does not enforce that, so both are checked here
// rather than trusting the pairing: sloN.step on a nil sloN would panic.
func (w *clusterWorker) applyResult(res *scan.Result, dur time.Duration, now time.Time, err error) {
	w.m.update(w.name, res, dur, now, err)
	if err != nil {
		clusterLogf(w.name, "evaluation error: %v", err)
		return
	}
	d := w.tr.Observe(issueKeys(res), now)
	w.al.notify(w.tr, now)
	// The object alert has already been enqueued above, LLM-free. Only now is
	// the model considered, and only for objects the throttle admits.
	w.ex.Consider(w.name, d, res.Health, flaggedWorkloads(res), res.ServiceIssues, now)
	if w.sloTr != nil && w.sloN != nil {
		c := res.Inventory.Census
		w.sloTr.Observe(c.Good, c.Total, now)
		v := w.sloTr.Verdict(now)
		w.m.updateSLO(w.name, true, w.sloTr.Target(), v.Fast, v.Slow)
		if n, ok := w.sloN.step(v, now); ok {
			logSLO(w.name, n, v)
			w.al.enqueue(n)
		}
	}
	w.m.updateIssues(w.name, w.tr, now)
	w.m.updateAlerts(w.al.stats())
	w.m.updateExplain(w.ex != nil, w.ex.Stats(now), w.ex.Latest())
	logDelta(w.name, res, d, len(w.tr.Active()), w.tr.FlapWindow())
}
```

`w.al.notify` must stamp the cluster, so the roller has to be per-cluster. `alerter` currently holds one `roller`. Change `alerter` in `watch.go` to hold the sink only, and give the worker its own roller:

In `internal/watch/watch.go`, change the `alerter` type and its `notify` method:

```go
// alerter routes tracker state to the outbound sink. A nil *alerter means no
// webhook is configured, which is the default: every method is a no-op, so the
// reconcile loop needs no conditional. There is one alerter per process — one
// webhook, one bounded queue — while the per-object rollup state lives in each
// cluster's own roller, because two clusters routinely run an object with the
// same namespace and name.
type alerter struct {
	sink *alert.Sink
}

// notify hands one cluster's rolled-up notifications to the sink. Enqueue never
// blocks.
func (a *alerter) notify(roller *alertstate.Roller, tr *watchstate.Tracker, now time.Time) {
	if a == nil {
		return
	}
	for _, n := range roller.Roll(tr.Active(), now) {
		a.sink.Enqueue(n)
	}
}
```

and drop the `roller` field from `newAlerter`'s return:

```go
	return &alerter{sink: sink}, nil
```

Give `clusterWorker` a `roller *alertstate.Roller` field, build it in `newClusterWorker`:

```go
		roller:  alertstate.New(alertstate.Options{Repeat: cfg.AlertRepeat, Cluster: t.Name}),
```

and call it in `applyResult`:

```go
	w.al.notify(w.roller, w.tr, now)
```

- [ ] **Step 5: Rewrite Run**

Replace `Run` in `internal/watch/watch.go` with:

```go
// Run starts the metrics server and one informer-driven control loop per target,
// blocking until ctx is cancelled.
func Run(ctx context.Context, targets []Target, cfg Config) error {
	// Validate every piece of configuration before anything else starts. A bad
	// --slo-target, --alert-format, --alert-repeat or target list must fail
	// fast: once the metrics server is listening and WaitForCacheSync is
	// underway, a reachable-but-unresponsive API server can block that sync
	// forever, hiding the config error behind what looks like a cluster hang.
	if err := validateTargets(targets); err != nil {
		return err
	}
	if err := validateSLOTarget(cfg.SLOTarget); err != nil {
		return err
	}
	if err := validateExplain(cfg); err != nil {
		return err
	}

	// [the alertCtx / newAlerter / defer block and the explainCtx / newExplainer
	//  / defer block are unchanged — keep them verbatim, including their
	//  comments about defer ordering]

	m := newMetrics(targetNames(targets))

	// Every worker is constructed before any of them runs, so a handler that
	// cannot be registered still fails the whole daemon at startup.
	workers := make([]*clusterWorker, 0, len(targets))
	for _, t := range targets {
		w, err := newClusterWorker(t, cfg, m, al, ex)
		if err != nil {
			return err
		}
		workers = append(workers, w)
	}

	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: m.handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("kubeagent: metrics server error: %v", err)
		}
	}()
	log.Printf("kubeagent: watching %d cluster(s); metrics on %s", len(workers), cfg.MetricsAddr)

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w *clusterWorker) {
			defer wg.Done()
			w.run(ctx)
		}(w)
	}

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutCtx)
	cancel()
	wg.Wait()
	log.Printf("kubeagent: shutting down")
	return nil
}
```

Add `"sync"` to the imports and remove the now-unused `informers`, `cache`, `scan` and `watchstate` imports from `watch.go` if nothing else in that file uses them (`go build` will tell you). Delete the old free `applyResult` function and the old inline informer/loop code from `watch.go` — they live in `cluster.go` now. Keep `issueKeys`, `flaggedWorkloads`, `scopeLabel`, `validateExplain`, `newExplainer`, `noteTeardown`, `newAlerter`, `alerter.stats`, `alerter.enqueue`, and `logDelta` where they are.

Change `logDelta` and `logSLO` to name their cluster:

```go
// logDelta prints one line per transition plus a summary. A reconcile that
// changed nothing prints nothing, so steady state stays quiet.
func logDelta(cluster string, res *scan.Result, d watchstate.Delta, active int, flapWindow time.Duration) {
	if len(d.New) == 0 && len(d.Resolved) == 0 && len(d.NewlyFlapping) == 0 {
		return
	}
	for _, r := range d.New {
		clusterLogf(cluster, "NEW %s", r.Key)
	}
	for _, r := range d.Resolved {
		clusterLogf(cluster, "RESOLVED %s (fired for %s)", r.Key, r.ResolvedAt.Sub(r.FiringSince).Round(time.Second))
	}
	for _, r := range d.NewlyFlapping {
		clusterLogf(cluster, "FLAPPING %s (%d firings in %s)", r.Key, r.RecentFirings, flapWindow)
	}
	clusterLogf(cluster, "cluster %s (%d/%d nodes ready) — %d issue(s) active, %d new, %d resolved",
		res.Health.Verdict, res.Health.NodesReady, res.Health.NodesTotal, active, len(d.New), len(d.Resolved))
}
```

In `internal/watch/slo.go`:

```go
func logSLO(cluster string, n alertstate.Notification, v slo.Verdict) {
	if n.Status == alertstate.StatusResolved {
		clusterLogf(cluster, "RESOLVED SLO/error-budget (burn back under threshold; fast=%.1fx slow=%.1fx)",
			v.Fast.BurnRate, v.Slow.BurnRate)
		return
	}
	clusterLogf(cluster, "%s SLO/error-budget:ErrorBudgetBurn (fast=%.1fx slow=%.1fx, coverage fast=%.0f%% slow=%.0f%%)",
		map[alertstate.Reason]string{alertstate.ReasonNew: "NEW", alertstate.ReasonRepeat: "REPEAT"}[n.Reason],
		v.Fast.BurnRate, v.Slow.BurnRate, v.Fast.Coverage*100, v.Slow.Coverage*100)
}
```

`sloNotifier.notification` must also stamp the cluster on its Object, so the SLO alert names which cluster's budget burned. Give `sloNotifier` a `cluster` field, set by `newSLOTracker`:

```go
func newSLONotifier(cluster string, repeat time.Duration) *sloNotifier {
	if repeat <= 0 {
		repeat = defaultSLORepeat
	}
	return &sloNotifier{cluster: cluster, repeat: repeat}
}
```

```go
func newSLOTracker(cluster string, cfg Config) (*slo.Tracker, *sloNotifier) {
	if cfg.SLOTarget == 0 {
		return nil, nil
	}
	gap := 2 * cfg.Heartbeat
	tr := slo.New(slo.Options{Target: cfg.SLOTarget, MaxSampleGap: gap})
	return tr, newSLONotifier(cluster, cfg.AlertRepeat)
}
```

```go
		Object:      alertstate.Object{Cluster: n.cluster, Kind: sloAlertKind, Name: sloAlertName},
```

and in `newClusterWorker`, call `w.sloTr, w.sloN = newSLOTracker(t.Name, cfg)`.

- [ ] **Step 6: Update the existing watch tests**

Every existing test that calls `Run(ctx, client, cfg)` becomes `Run(ctx, []Target{{Name: "local", Client: client}}, cfg)`. Every test that calls the free `applyResult(m, tr, al, ex, sloTr, sloN, res, dur, at, err)` becomes a method call on a worker. Add this helper to `internal/watch/watch_test.go`:

```go
// testWorker builds a worker with no informers and no client — enough for the
// applyResult tests, which drive the fold directly rather than through a
// reconcile.
func testWorker(m *metrics, tr *watchstate.Tracker) *clusterWorker {
	return &clusterWorker{
		name:   defaultClusterName,
		m:      m,
		tr:     tr,
		roller: alertstate.New(alertstate.Options{Cluster: defaultClusterName}),
	}
}
```

So `applyResult(m, tr, nil, nil, nil, nil, sampleResult(), time.Millisecond, at, nil)` becomes:

```go
	testWorker(m, tr).applyResult(sampleResult(), time.Millisecond, at, nil)
```

and a test that supplies an alerter or an SLO tracker sets those fields on the returned worker before calling:

```go
	w := testWorker(m, tr)
	w.al = al
	w.sloTr, w.sloN = sloTr, sloN
	w.applyResult(sampleResult(), time.Millisecond, at, nil)
```

Do not change what any existing test asserts — only how it reaches the code. Log-content assertions will need the `[local] ` prefix added.

- [ ] **Step 7: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/watch/ 2>&1 | tail -40
```

Expected: PASS, including the two new `Run` tests and `TestValidateTargets`.

- [ ] **Step 8: Race check**

N goroutines writing one mutex-guarded snapshot is exactly the kind of change the race detector exists for.

```bash
export PATH=$PATH:/usr/local/go/bin
go test -race ./internal/watch/ 2>&1 | tail -20
```

Expected: PASS with no `WARNING: DATA RACE`.

- [ ] **Step 9: Build main.go**

`main.go` still calls `watchRun(ctx, client, cfg)`, which no longer compiles. Make it build with a one-line change; Task 6 does the real flag work:

```go
	return watchRun(ctx, []watch.Target{{Name: "local", Client: client}}, watch.Config{
```

and in `main_test.go`, update `captureWatchConfig`'s stub signature to `func(_ context.Context, _ []watch.Target, cfg watch.Config) error`.

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head -20
```

Expected: no failures.

- [ ] **Step 10: Commit**

```bash
git add internal/watch main.go main_test.go
git commit -m "feat(watch): run one informer set per cluster in a single daemon"
```

---

### Task 5: Cluster fields on /issues and /explanations

**Files:**
- Modify: `internal/watch/metrics.go`
- Test: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `clusterSnapshot` and `m.names` (Task 3); `oncall.Explanation.Cluster` (Task 2).
- Produces: `/issues` with a `clusters` array and a `cluster` field per record; `/explanations` with a `cluster` field per entry.

- [ ] **Step 1: Write the failing test**

Append to `internal/watch/metrics_test.go`:

```go
// TestIssuesJSONMergesClustersAndNamesEachRecord pins the /issues shape: the
// arrays merge across clusters with each record naming its own, the stats sum,
// and a clusters array reports per-target status so an operator can tell "no
// issues" apart from "that cluster is unreachable".
func TestIssuesJSONMergesClustersAndNamesEachRecord(t *testing.T) {
	m := newMetrics([]string{"prod-eu", "prod-us"})
	at := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

	eu := watchstate.New(watchstate.Options{})
	eu.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "CrashLoopBackOff"}}, at)
	m.update("prod-eu", sampleResult(), time.Millisecond, at, nil)
	m.updateIssues("prod-eu", eu, at)

	us := watchstate.New(watchstate.Options{})
	us.Observe([]watchstate.Key{{Kind: "Deployment", Namespace: "shop", Name: "web", Issue: "ImagePullBackOff"}}, at)
	m.update("prod-us", &scan.Result{}, time.Millisecond, at, errors.New("connection refused"))
	m.updateIssues("prod-us", us, at)

	body, err := m.issuesJSON()
	if err != nil {
		t.Fatalf("issuesJSON: %v", err)
	}
	var got struct {
		Clusters []struct {
			Name     string `json:"name"`
			Up       bool   `json:"up"`
			LastScan string `json:"lastScan"`
			Error    string `json:"error"`
		} `json:"clusters"`
		Active []struct {
			Cluster string `json:"cluster"`
			Name    string `json:"name"`
			Issue   string `json:"issue"`
		} `json:"active"`
		Stats struct {
			NewTotal int64 `json:"newTotal"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(got.Clusters) != 2 {
		t.Fatalf("clusters has %d entries, want 2", len(got.Clusters))
	}
	if got.Clusters[0].Name != "prod-eu" || !got.Clusters[0].Up {
		t.Errorf("clusters[0] = %+v, want prod-eu up", got.Clusters[0])
	}
	if got.Clusters[1].Up {
		t.Errorf("prod-us must report up=false")
	}
	if got.Clusters[1].Error == "" {
		t.Error("an unreachable cluster must report why")
	}

	seen := map[string]string{}
	for _, r := range got.Active {
		seen[r.Cluster] = r.Issue
	}
	if seen["prod-eu"] != "CrashLoopBackOff" || seen["prod-us"] != "ImagePullBackOff" {
		t.Errorf("active records = %v, want one per cluster with its own issue", seen)
	}
	if got.Stats.NewTotal != 2 {
		t.Errorf("stats.newTotal = %d, want 2 (summed across clusters)", got.Stats.NewTotal)
	}
}

func TestExplanationsJSONNamesTheCluster(t *testing.T) {
	m := newMetrics([]string{"prod-eu"})
	m.updateExplain(true, oncall.Stats{Allowed: 1}, []oncall.Explanation{{
		Cluster: "prod-eu", Kind: "Deployment", Namespace: "shop", Name: "web",
		Issues: []string{"ImagePullBackOff"}, ExplainedAt: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Model: "test-model", Text: "the image tag does not exist",
	}})
	body, err := m.explanationsJSON()
	if err != nil {
		t.Fatalf("explanationsJSON: %v", err)
	}
	if !strings.Contains(string(body), `"cluster":"prod-eu"`) {
		t.Errorf("explanations must name the cluster\n%s", body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run "IssuesJSON|ExplanationsJSON" 2>&1 | head -20
```

Expected: FAIL — `clusters has 0 entries, want 2` and the missing `"cluster"` key.

- [ ] **Step 3: Add the cluster to both views**

In `internal/watch/metrics.go`, add `Cluster` to `issueView` and `explanationView`, add the `clusterView` type, and rewrite the two JSON methods:

```go
// issueView is one record as served by /issues. The pointer fields distinguish
// "not applicable" from a legitimate zero: active records carry ageSeconds and
// omit resolution data, resolved records the reverse.
type issueView struct {
	Cluster           string `json:"cluster"`
	Kind              string `json:"kind"`
	Namespace         string `json:"namespace,omitempty"`
	Name              string `json:"name"`
	Issue             string `json:"issue"`
	FirstSeen         string `json:"firstSeen"`
	FiringSince       string `json:"firingSince"`
	LastSeen          string `json:"lastSeen"`
	Firings           int    `json:"firings"`
	Flapping          bool   `json:"flapping"`
	AgeSeconds        *int64 `json:"ageSeconds,omitempty"`
	ResolvedAt        string `json:"resolvedAt,omitempty"`
	ResolutionSeconds *int64 `json:"resolutionSeconds,omitempty"`
}

// clusterView is one watched cluster's status. It exists so an operator can tell
// "this cluster reported no issues" apart from "this cluster could not be
// reached" — an empty active list looks identical either way.
type clusterView struct {
	Name     string `json:"name"`
	Up       bool   `json:"up"`
	LastScan string `json:"lastScan"`
	Error    string `json:"error,omitempty"`
}

type issuesView struct {
	Clusters []clusterView `json:"clusters"`
	Active   []issueView   `json:"active"`
	Resolved []issueView   `json:"resolved"`
	Stats    statsView     `json:"stats"`
}
```

Change `issueViews` to stamp the cluster:

```go
func issueViews(cluster string, rs []watchstate.Record, at time.Time, resolved bool) []issueView {
	out := make([]issueView, 0, len(rs))
	for _, r := range rs {
		v := issueView{
			Cluster:     cluster,
			Kind:        r.Key.Kind,
			Namespace:   r.Key.Namespace,
			Name:        r.Key.Name,
			Issue:       r.Key.Issue,
			FirstSeen:   rfc3339(r.FirstSeen),
			FiringSince: rfc3339(r.FiringSince),
			LastSeen:    rfc3339(r.LastSeen),
			Firings:     r.Firings,
			Flapping:    r.Flapping,
		}
		if resolved {
			v.ResolvedAt = rfc3339(r.ResolvedAt)
			secs := ageSeconds(r.FiringSince, r.ResolvedAt)
			v.ResolutionSeconds = &secs
		} else {
			secs := ageSeconds(r.FiringSince, at)
			v.AgeSeconds = &secs
		}
		out = append(out, v)
	}
	return out
}
```

Rewrite `issuesJSON`:

```go
// issuesJSON renders the tracked-issue snapshot across every watched cluster.
// Held under the read lock so no worker can swap a snapshot mid-encode.
func (m *metrics) issuesJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := issuesView{
		Clusters: make([]clusterView, 0, len(m.names)),
		Active:   []issueView{},
		Resolved: []issueView{},
	}
	for _, n := range m.names {
		c := m.clusters[n]
		cv := clusterView{Name: n, Up: c.up, Error: c.lastError}
		if c.lastScanUnix != 0 {
			cv.LastScan = rfc3339(time.Unix(c.lastScanUnix, 0))
		}
		out.Clusters = append(out.Clusters, cv)
		out.Active = append(out.Active, issueViews(n, c.issues.Active, c.issues.At, false)...)
		out.Resolved = append(out.Resolved, issueViews(n, c.issues.Resolved, c.issues.At, true)...)
		out.Stats.NewTotal += c.issues.Stats.NewTotal
		out.Stats.ResolvedTotal += c.issues.Stats.ResolvedTotal
		out.Stats.FlapTotal += c.issues.Stats.FlapTotal
		out.Stats.DroppedTotal += c.issues.Stats.DroppedTotal
		out.Stats.ResolutionSecondsSum += c.issues.Stats.ResolutionSecondsSum
		out.Stats.ResolutionSecondsCount += c.issues.Stats.ResolutionSecondsCount
	}
	return json.Marshal(out)
}
```

Add `Cluster` to `explanationView` as its first field with tag `json:"cluster"`, and set it in `explanationsJSON`:

```go
		out.Explanations = append(out.Explanations, explanationView{
			Cluster:     x.Cluster,
			Kind:        x.Kind,
```

- [ ] **Step 4: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ 2>&1 | tail -20
```

Expected: PASS. Update any existing `/issues` assertion that indexed the old shape.

- [ ] **Step 5: Commit**

```bash
git add internal/watch
git commit -m "feat(watch): name the cluster on /issues and /explanations"
```

---

### Task 6: CLI flags

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `watch.Target`, `watch.Run(ctx, []watch.Target, cfg)` (Task 4).
- Produces: `--context` repeatable, `--cluster-name` (default `local`, env `KUBEAGENT_CLUSTER_NAME`), `--include-local` (default false, env `KUBEAGENT_INCLUDE_LOCAL`), and `func buildTargets(kubeconfig, clusterName string, contexts []string, includeLocal bool) ([]watch.Target, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
// TestContextListCollectsRepeats pins the flag type: --context is repeatable,
// and each occurrence names one cluster to watch.
func TestContextListCollectsRepeats(t *testing.T) {
	var got contextList
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Var(&got, "context", "")
	if err := fs.Parse([]string{"--context", "a", "--context", "b"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("contextList = %v, want [a b]", got)
	}
	if err := fs.Parse([]string{"--context", ""}); err == nil {
		t.Error("an empty --context must be rejected")
	}
}

// TestBuildTargetsNaming pins the three naming rules: no --context means one
// default target named by --cluster-name; each --context names its own target;
// and --include-local adds the default target alongside them.
func TestBuildTargetsNaming(t *testing.T) {
	kc := multiContextKubeconfigPath(t)
	t.Setenv("KUBECONFIG", kc)

	tests := []struct {
		name         string
		contexts     []string
		includeLocal bool
		want         []string
	}{
		{"no contexts", nil, false, []string{"local"}},
		{"one context", []string{"alpha"}, false, []string{"alpha"}},
		{"two contexts", []string{"alpha", "beta"}, false, []string{"alpha", "beta"}},
		{"include local", []string{"alpha"}, true, []string{"local", "alpha"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := buildTargets(kc, "local", tc.contexts, tc.includeLocal)
			if err != nil {
				t.Fatalf("buildTargets: %v", err)
			}
			var got []string
			for _, tg := range targets {
				got = append(got, tg.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("target names = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildTargetsRejectsAnUnknownContext pins the fail-fast rule. Building a
// client contacts no API server, so a failure here is a misspelled context, and
// silently watching fewer clusters than the operator asked for is the outcome
// this prevents.
func TestBuildTargetsRejectsAnUnknownContext(t *testing.T) {
	kc := multiContextKubeconfigPath(t)
	t.Setenv("KUBECONFIG", kc)
	if _, err := buildTargets(kc, "local", []string{"nope"}, false); err == nil {
		t.Fatal("buildTargets accepted a context that is not in the kubeconfig")
	}
}

// multiContextKubeconfigPath writes a kubeconfig with two contexts pointing at a
// closed loopback port. Every server here is unreachable on purpose: building a
// clientset performs no network I/O, so these tests stay hermetic.
func multiContextKubeconfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
current-context: alpha
clusters:
  - name: alpha
    cluster:
      server: https://127.0.0.1:1
  - name: beta
    cluster:
      server: https://127.0.0.1:2
contexts:
  - name: alpha
    context: {cluster: alpha, user: alpha}
  - name: beta
    context: {cluster: beta, user: beta}
users:
  - name: alpha
    user: {token: <PLACEHOLDER>}
  - name: beta
    user: {token: <PLACEHOLDER>}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}
```

Add `"slices"`, `"path/filepath"`, `"io"` and `"flag"` to `main_test.go`'s imports if absent.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . 2>&1 | head -20
```

Expected: FAIL — `undefined: contextList`, `undefined: buildTargets`.

- [ ] **Step 3: Add the flag type and the target builder**

In `main.go`, above `runWatch`:

```go
// contextList collects a repeatable --context flag: one occurrence per cluster
// the daemon should watch.
type contextList []string

func (c *contextList) String() string { return strings.Join(*c, ",") }

func (c *contextList) Set(v string) error {
	if v == "" {
		return fmt.Errorf("--context cannot be empty")
	}
	*c = append(*c, v)
	return nil
}

// buildTargets resolves the flags to the clusters the daemon will watch.
// Building a client contacts no API server, so a failure here is a
// configuration error — a misspelled context — and it is fatal: an operator who
// asked for three clusters and silently got two is worse off than one whose
// daemon refused to start.
func buildTargets(kubeconfig, clusterName string, contexts []string, includeLocal bool) ([]watch.Target, error) {
	var targets []watch.Target
	if len(contexts) == 0 || includeLocal {
		client, err := cluster.NewInClusterOrKubeconfig(kubeconfig, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, watch.Target{Name: clusterName, Client: client})
	}
	for _, name := range contexts {
		client, err := cluster.NewClient(kubeconfig, name)
		if err != nil {
			return nil, err
		}
		targets = append(targets, watch.Target{Name: name, Client: client})
	}
	return targets, nil
}
```

- [ ] **Step 4: Wire the flags**

In `runWatch`, replace the `contextName` flag with the three new ones:

```go
	var contexts contextList
	fs.Var(&contexts, "context", "kubeconfig context to watch; repeat the flag to watch several clusters from one daemon")
	clusterName := fs.String("cluster-name", envOr("KUBEAGENT_CLUSTER_NAME", "local"), "name for the default cluster — the one watched when no --context is given; becomes its `cluster` metric label")
	includeLocal := fs.Bool("include-local", envBool("KUBEAGENT_INCLUDE_LOCAL", false), "also watch the default cluster alongside every --context (no-op when no --context is given)")
```

and replace the client construction with:

```go
	targets, err := buildTargets(*kubeconfig, *clusterName, contexts, *includeLocal)
	if err != nil {
		return err
	}
```

and the call:

```go
	return watchRun(ctx, targets, watch.Config{
```

Update the usage string at `main.go:64` — replace the watch clause's `[--context name]` with:

```
[--context name (repeatable)] [--cluster-name name] [--include-local]
```

Leave the `scan` clause's `[--context name]` alone: `scan` stays single-cluster.

- [ ] **Step 5: Run the tests**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head -20
```

Expected: no failures. `main_test.go`'s `captureWatchConfig` stub already takes `[]watch.Target` from Task 4 step 9.

- [ ] **Step 6: Hermetic check**

The watch wiring tests must not read the machine's kubeconfig — that is exactly what broke the 0.58.0 release build.

```bash
export PATH=$PATH:/usr/local/go/bin
HOME="$(mktemp -d)" KUBECONFIG= go test . 2>&1 | tail -10
```

Expected: PASS. If a new test fails here, point it at `multiContextKubeconfigPath(t)` or `deadKubeconfigPath(t)` and set `KUBECONFIG` with `t.Setenv`.

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): make --context repeatable and name the default cluster"
```

---

### Task 7: Helm multi-cluster support

**Files:**
- Modify: `deploy/helm/kubeagent/values.yaml`
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml`
- Test: manual `helm template` assertions (this chart has no unit-test harness)

**Interfaces:**
- Consumes: `--kubeconfig`, `--context` (repeatable), `--cluster-name`, `--include-local` (Task 6).
- Produces: the `multicluster.*` values block.

- [ ] **Step 1: Write the failing check**

Create the assertion script at `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/helm-multicluster-check.sh`:

```bash
#!/usr/bin/env bash
# Asserts the chart's multicluster guard-rails and rendering. Run from the repo root.
set -uo pipefail
export PATH=$PATH:/usr/local/bin
chart=deploy/helm/kubeagent
fails=0

check_fails() {   # check_fails <description> <expected message fragment> <helm set args...>
  local desc="$1" want="$2"; shift 2
  local out
  out="$(helm template x "$chart" "$@" 2>&1)"
  if [ $? -eq 0 ]; then
    echo "FAIL: $desc — render succeeded but must fail"; fails=$((fails+1)); return
  fi
  if ! grep -qF "$want" <<<"$out"; then
    echo "FAIL: $desc — message did not contain: $want"; echo "$out" | tail -3; fails=$((fails+1)); return
  fi
  echo "ok: $desc"
}

check_contains() {   # check_contains <description> <expected line fragment> <helm set args...>
  local desc="$1" want="$2"; shift 2
  local out
  if ! out="$(helm template x "$chart" "$@" 2>&1)"; then
    echo "FAIL: $desc — render failed"; echo "$out" | tail -3; fails=$((fails+1)); return
  fi
  if ! grep -qF "$want" <<<"$out"; then
    echo "FAIL: $desc — output did not contain: $want"; fails=$((fails+1)); return
  fi
  echo "ok: $desc"
}

check_absent() {   # check_absent <description> <fragment that must NOT appear> <helm set args...>
  local desc="$1" bad="$2"; shift 2
  local out
  if ! out="$(helm template x "$chart" "$@" 2>&1)"; then
    echo "FAIL: $desc — render failed"; echo "$out" | tail -3; fails=$((fails+1)); return
  fi
  if grep -qF "$bad" <<<"$out"; then
    echo "FAIL: $desc — output contained: $bad"; fails=$((fails+1)); return
  fi
  echo "ok: $desc"
}

check_fails "enabled without a Secret is refused" \
  "multicluster.existingSecret is required" \
  --set multicluster.enabled=true --set 'multicluster.contexts={prod-eu}'

check_fails "enabled with no contexts is refused" \
  "multicluster.contexts must list at least one context" \
  --set multicluster.enabled=true --set multicluster.existingSecret=kubeagent-clusters

check_contains "the kubeconfig comes from the mounted Secret" \
  '--kubeconfig=/etc/kubeagent/kubeconfig/kubeconfig' \
  --set multicluster.enabled=true --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu,prod-us}'

check_contains "each context becomes a --context argument" \
  '- "--context=prod-us"' \
  --set multicluster.enabled=true --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu,prod-us}'

check_contains "the local cluster is watched by default" \
  '- "--include-local"' \
  --set multicluster.enabled=true --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu}'

check_contains "the Secret is mounted read-only" \
  'readOnly: true' \
  --set multicluster.enabled=true --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu}'

check_absent "single-cluster rendering is unchanged" \
  '--kubeconfig' 

echo
if [ "$fails" -ne 0 ]; then echo "$fails check(s) failed"; exit 1; fi
echo "all checks passed"
```

- [ ] **Step 2: Run it to verify it fails**

```bash
chmod +x /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/helm-multicluster-check.sh
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/helm-multicluster-check.sh
```

Expected: several `FAIL:` lines — `multicluster` is not a known value yet, so the guard-rails never fire and the arguments never render.

- [ ] **Step 3: Add the values**

Append to `deploy/helm/kubeagent/values.yaml`:

```yaml
# Watch several clusters from this one daemon. Each context in the mounted
# kubeconfig becomes its own informer set, tracker and set of metric series,
# labelled cluster="<context>", behind this single /metrics endpoint.
#
# The kubeconfig is NEVER a value here. A values file lands in Git, in
# `helm get values`, and in CI logs, and a kubeconfig carries the credentials for
# every cluster it names. Create a Secret and name it in existingSecret:
#
#   kubectl -n kubeagent create secret generic kubeagent-clusters \
#     --from-file=kubeconfig=<PLACEHOLDER>
#
# Each credential inside that kubeconfig MUST be read-only (get/list/watch).
# kubeagent issues no writes, but it cannot enforce that from inside the pod:
# a kubeconfig holding a cluster-admin token would give this daemon
# write-capable credentials it never uses but nonetheless holds.
multicluster:
  enabled: false
  # Name of an existing Secret holding the kubeconfig. Required when enabled.
  existingSecret: ""
  # Key within existingSecret holding the kubeconfig document.
  secretKey: kubeconfig
  # Contexts within that kubeconfig to watch. At least one is required when
  # enabled; each becomes a cluster label of the same name.
  contexts: []
  # Also watch the cluster this daemon runs in, through its ServiceAccount and
  # the ClusterRole below.
  includeLocal: true
  # The cluster label for that local cluster.
  localName: local
```

- [ ] **Step 4: Add the template**

In `deploy/helm/kubeagent/templates/deployment.yaml`, insert this block inside `args:`, immediately after the `{{- if .Values.watch.namespace }}` block and before the `{{- if .Values.alerts.enabled }}` block:

```yaml
            {{- if .Values.multicluster.enabled }}
            {{- if not .Values.multicluster.existingSecret }}
            {{- fail "multicluster.existingSecret is required when multicluster.enabled is true (a kubeconfig carries the credentials for every cluster it names, so it must come from a Secret, never from values.yaml)" }}
            {{- end }}
            {{- if not .Values.multicluster.contexts }}
            {{- fail "multicluster.contexts must list at least one context when multicluster.enabled is true" }}
            {{- end }}
            - "--kubeconfig=/etc/kubeagent/kubeconfig/{{ .Values.multicluster.secretKey }}"
            {{- range .Values.multicluster.contexts }}
            - "--context={{ . }}"
            {{- end }}
            - "--cluster-name={{ .Values.multicluster.localName }}"
            {{- if .Values.multicluster.includeLocal }}
            - "--include-local"
            {{- end }}
            {{- end }}
```

Add the volume mount, immediately before the `ports:` key:

```yaml
          {{- if .Values.multicluster.enabled }}
          volumeMounts:
            - name: cluster-kubeconfig
              mountPath: /etc/kubeagent/kubeconfig
              readOnly: true
          {{- end }}
```

Add the volume, immediately before the `{{- with .Values.nodeSelector }}` block (at `spec.template.spec` level, same indentation as `containers:`):

```yaml
      {{- if .Values.multicluster.enabled }}
      volumes:
        - name: cluster-kubeconfig
          secret:
            secretName: {{ .Values.multicluster.existingSecret | quote }}
            defaultMode: 0400
      {{- end }}
```

- [ ] **Step 5: Run the checks and lint**

```bash
export PATH=$PATH:/usr/local/bin
/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/helm-multicluster-check.sh
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent --set multicluster.enabled=true \
  --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu,prod-us}' | grep -A4 'volumes:'
```

Expected: `all checks passed`, `1 chart(s) linted, 0 chart(s) failed`, and a `volumes:` block naming the Secret with `defaultMode: 0400`.

- [ ] **Step 6: Confirm no credential reaches an argument**

```bash
export PATH=$PATH:/usr/local/bin
helm template x deploy/helm/kubeagent --set multicluster.enabled=true \
  --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu}' \
  | grep -E '^\s+- "--' | grep -viE 'kubeconfig=/etc/kubeagent' | grep -iE 'token|key|secret|password'
```

Expected: no output. A process argument is world-readable through `/proc`, so the only credential-adjacent argument permitted is the **path** to the mounted kubeconfig.

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/kubeagent
git commit -m "feat(helm): mount a kubeconfig Secret and watch several clusters"
```

**Chart version note for the release:** this task changes the chart's templates and values, so at release time `deploy/helm/kubeagent/Chart.yaml` needs a **minor** chart-version bump (0.21.x → 0.22.0), not the patch bump `scripts/bump-version.sh` applies by default. Record this in the progress ledger.

---

### Task 8: Chaos scenario 15

**Files:**
- Modify: `chaos/run.sh`
- Modify: `chaos/README.md`

**Interfaces:**
- Consumes: the full daemon from Tasks 1-6.
- Produces: `scenario_15_multicluster`, reachable via `./chaos/run.sh --only 15`.

- [ ] **Step 1: Read the existing pattern**

Read `scenario_14` in `chaos/run.sh` (around line 519) and the `--only` dispatcher and run order near the bottom of the file. Scenario 15 follows the same shape: temp files, a background daemon on a loopback metrics address, a `/readyz` poll, an injected fault, captured output, `kill`+`wait`, a `record` block, cleanup. Reuse whatever helpers that scenario uses; do not invent new ones.

- [ ] **Step 2: Add the scenario**

Insert after `scenario_14`'s closing brace in `chaos/run.sh`:

```bash
scenario_15_multicluster() {   # one daemon, three targets: two names for this cluster and one dead endpoint
  log "scenario 15: multi-cluster hub (labelling, merge, and per-cluster degradation)"
  local ns=chaos-multi port=18094
  local wlog kc wpid i ccluster cuser metrics issues

  wlog="$(mktemp)"; kc="$(mktemp)"

  # A second context pointing at the SAME cluster proves labelling and the
  # cross-cluster merge without paying for a second Kind cluster; a third
  # context pointing at a closed port proves per-cluster degradation. This does
  # NOT test genuinely divergent cluster state — see the verdict text.
  kubectl --context "$CTX" config view --raw --minify --flatten >"$kc"
  ccluster="$(KUBECONFIG="$kc" kubectl config view -o jsonpath='{.contexts[0].context.cluster}')"
  cuser="$(KUBECONFIG="$kc" kubectl config view -o jsonpath='{.contexts[0].context.user}')"
  KUBECONFIG="$kc" kubectl config set-context alias-b --cluster="$ccluster" --user="$cuser" >/dev/null
  KUBECONFIG="$kc" kubectl config set-cluster dead-cluster --server=https://127.0.0.1:1 >/dev/null
  KUBECONFIG="$kc" kubectl config set-context dead --cluster=dead-cluster --user="$cuser" >/dev/null

  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  ./kubeagent watch --kubeconfig "$kc" \
    --context "$CTX" --context alias-b --context dead \
    -n "$ns" --metrics-addr "127.0.0.1:$port" --heartbeat 10s --debounce 2s >"$wlog" 2>&1 &
  wpid=$!
  # Readiness must arrive despite the dead target: ready means every cluster
  # finished a FIRST ATTEMPT, not that every cluster is healthy.
  for i in $(seq 60); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done
  local ready_code
  ready_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz" 2>/dev/null || echo 000)"

  # Break the workload so there is a real issue to see twice, once per label.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 40

  metrics="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null || echo '')"
  issues="$(curl -s "http://127.0.0.1:$port/issues" 2>/dev/null || echo '<unreachable>')"

  kill "$wpid" >/dev/null 2>&1 || true; wait "$wpid" >/dev/null 2>&1 || true

  {
    echo "--- /readyz status code with one target permanently dead ---"
    printf 'HTTP %s\n' "$ready_code"
    echo
    echo '--- per-cluster up/down ---'
    { grep -E '^kubeagent_(cluster_up|clusters_total)' <<<"$metrics" || echo '<no cluster series>'; }
    echo
    echo '--- the same broken Deployment, once per healthy cluster label ---'
    { grep -E '^kubeagent_issue_active' <<<"$metrics" | grep 'web' || echo '<no active issue series>'; }
    echo
    echo '--- /issues cluster roster ---'
    printf '%s\n' "$issues" | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin).get("clusters"), indent=2))' 2>/dev/null \
      || echo '<could not parse /issues>'
    echo
    echo '--- active issues by cluster ---'
    printf '%s\n' "$issues" | python3 -c 'import json,sys,collections; print(dict(collections.Counter(r["cluster"] for r in json.load(sys.stdin).get("active",[]))))' 2>/dev/null \
      || echo '<could not parse /issues>'
    echo
    echo '--- credential check: no kubeconfig material in any log line ---'
    { grep -cE 'BEGIN CERTIFICATE|client-key-data|client-certificate-data|token:' "$wlog" || true; } \
      | sed 's/^/log lines carrying kubeconfig material: /'
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    { grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true; } | sed 's/^/log lines mentioning a write verb: /'
    echo
    echo '--- daemon log tail (last 15 lines) ---'
    tail -n 15 "$wlog" 2>/dev/null || echo '<no daemon log captured>'
  } | record "15. Multi-cluster hub (three targets, one dead)" "expect: /readyz returns HTTP 200 even though the 'dead' target never reaches its API server — readiness means every cluster finished a first attempt, because a NotReady pod leaves its Service endpoints and Prometheus would then stop scraping the clusters that ARE working. kubeagent_clusters_total is 3; kubeagent_cluster_up is 1 for both $CTX and alias-b and 0 for dead. The broken Deployment appears in kubeagent_issue_active exactly twice — once with cluster=\"$CTX\" and once with cluster=\"alias-b\" — and the /issues cluster roster lists all three with dead carrying a non-empty error. No log line may carry kubeconfig material, and no write verb may appear. Scope: alias-b is a second NAME for the same cluster, so this proves labelling, the cross-cluster merge and the degradation path — the parts most likely to regress — but it does not exercise genuinely divergent cluster state, which would need a second Kind cluster and is covered by unit tests with independent fake clientsets instead. Every daemon log line must also carry a [<cluster>] prefix; with three interleaved reconcile loops an unprefixed line is a bug."

  rm -f "$wlog" "$kc"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}
```

- [ ] **Step 3: Register it in the dispatcher and the run order**

Find the `--only` case statement and the sequential run list near the bottom of `chaos/run.sh` and add `15` / `scenario_15_multicluster` alongside `14` / `scenario_14`, in the same style. Scenario 1 must stay last in the run order. Also update the `--only` help text in the usage block from its current range to `(1..15)`.

```bash
grep -n "scenario_14\|--only\|1\.\.1[0-9]" chaos/run.sh
```

- [ ] **Step 4: Verify the shell is sound**

`chaos/run.sh` runs under `set -euo pipefail`, where `grep -c` exits 1 on zero matches and `pipefail` propagates that out of a command substitution. Every such pipeline above already carries `|| true` inside the substitution — check that nothing you added lacks it.

```bash
bash -n chaos/run.sh && echo "syntax ok"
```

Expected: `syntax ok`.

- [ ] **Step 5: Run the scenario**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
go build -o kubeagent .
./chaos/run.sh --only 15
```

Expected: the scenario completes and `docs/testing/chaos-results.md` contains section 15 with `HTTP 200`, `kubeagent_clusters_total 3`, `cluster_up` 1/1/0, two `kubeagent_issue_active` lines for `web`, and zero on both the credential and write-verb counters. If the Kind cluster is not up, run `./chaos/run.sh --recreate --only 15`.

- [ ] **Step 6: Update the scenario table**

In `chaos/README.md`, add a row 15 after row 14:

```markdown
| 15 | Multi-cluster hub | one `kubeagent watch` over three targets: this cluster twice under different context names, plus a context pointing at a closed port | `/readyz` still 200 with one target dead, `kubeagent_cluster_up` 1/1/0, `kubeagent_clusters_total 3`, the same broken Deployment listed once per healthy cluster label, and the dead target on the `/issues` roster with an error — a **degradation** test, not a divergent-state test |
```

and update the `--only` line in the Run section from `--only 7        # run a single scenario (1..13) for debugging` to `(1..15)`.

- [ ] **Step 7: Commit**

```bash
git add chaos/run.sh chaos/README.md
git commit -m "test(chaos): prove multi-cluster labelling, merge and degradation"
```

---

### Task 9: Documentation

**Files:**
- Modify: `website/docs/` (the watch reference page), `deploy/README.md`, `CHANGELOG.md`, `docs/go-concepts.md`, `website/docs/roadmap.md`

**Interfaces:**
- Consumes: everything from Tasks 1-8.
- Produces: no code.

- [ ] **Step 1: Find the pages that document the watch flags and the chart**

```bash
grep -rln "slo-target\|explain-budget" website/docs/ deploy/README.md README.md
grep -rn "explain:" deploy/README.md | head
```

Update every page that lists the `watch` flags or the chart values. Follow each page's existing structure; do not restructure them.

- [ ] **Step 2: Document the flags and the label**

On the watch reference page, in the flag table, add:

| Flag | Default | Meaning |
|---|---|---|
| `--context <name>` | current-context | Cluster to watch. **Repeat the flag** to watch several clusters from one daemon. |
| `--cluster-name <name>` | `local` | Name for the default cluster — the one watched when no `--context` is given. Becomes its `cluster` metric label. |
| `--include-local` | off | Also watch the default cluster alongside every `--context`. A no-op when no `--context` is given. |

and add a section:

```markdown
## Watching several clusters

    kubeagent watch --context prod-eu --context prod-us --context staging

One informer set per cluster runs inside a single process behind one HTTP
endpoint. Every metric series carries a `cluster` label, `/issues` and
`/explanations` carry a `cluster` field, and every alert names its cluster.

The `cluster` label is present even with one cluster, where it defaults to
`local`. PromQL selectors match regardless of extra labels, so a query written
against a single-cluster daemon keeps working; a recording rule that groups
`by (...)` should add `cluster` to the grouping.

**A configuration error is fatal; a cluster failure is not.** A context that is
not in the kubeconfig stops the daemon at startup — building a client contacts no
API server, so a failure there is a typo, and silently watching two of the three
clusters you asked for is worse than not starting. A cluster that becomes
unreachable at runtime reports `kubeagent_cluster_up 0` and an error on the
`/issues` roster; its tracked issues stay firing, and every other cluster keeps
reconciling.

`/readyz` reports ready once every cluster has finished its **first reconcile
attempt** — success or failure — and never flips afterward on cluster health.
Readiness answers "can this process serve?", not "is everything fine": tying it
to cluster health would let one unreachable remote cluster pull the pod out of
its Service endpoints, stopping Prometheus from scraping it, and so blind you to
the clusters that are working.

**Credentials.** One process holds read-only credentials for every cluster it
watches, so the daemon and its kubeconfig Secret are as sensitive as the union of
those clusters. **Each credential in that kubeconfig must be read-only
(get/list/watch).** kubeagent issues no writes, but it cannot enforce that from
inside the pod: a kubeconfig holding a cluster-admin token would give this daemon
write-capable credentials it never uses but nonetheless holds.

Cross-cutting settings stay global: one webhook, one explanation budget, one
`--slo-target`. If you need them split per cluster, run one daemon per cluster —
that still works, and each one labels its series with its own `--cluster-name`.
```

- [ ] **Step 3: Document the chart values**

In `deploy/README.md`, in the values table (or the section documenting `explain.*`), add the `multicluster.*` keys with the same wording as the `values.yaml` comments from Task 7, including the Secret requirement and the read-only-credential warning. Add the example:

```bash
kubectl -n kubeagent create secret generic kubeagent-clusters \
  --from-file=kubeconfig=<PLACEHOLDER>

helm upgrade --install kubeagent deploy/helm/kubeagent -n kubeagent \
  --set multicluster.enabled=true \
  --set multicluster.existingSecret=kubeagent-clusters \
  --set 'multicluster.contexts={prod-eu,prod-us}'
```

Note that the chart's ClusterRole still covers the **local** cluster only; permissions in remote clusters ride entirely on the credentials inside the mounted kubeconfig.

- [ ] **Step 4: Add the Go concept**

Append to `docs/go-concepts.md`, in the file's established style — a plain everyday example first, then the kubeagent example, no Python comparisons:

```markdown
## Many workers, one shared snapshot

A kitchen with three cooks and one order board. Each cook works their own
station and writes finished dishes onto the shared board. Two cooks writing at
the same instant would smudge the board, so there is one rule: take the pen,
write, put the pen back. Anyone reading the board takes the pen too — otherwise
they might read a half-written line.

In Go the pen is a `sync.Mutex`, and "take the pen" is `Lock()`:

    type board struct {
        mu     sync.Mutex
        dishes map[string]int
    }

    func (b *board) done(station string) {
        b.mu.Lock()
        defer b.mu.Unlock()
        b.dishes[station]++
    }

`defer b.mu.Unlock()` runs when the function returns, however it returns — so
the pen goes back even if the code panics partway.

A `sync.RWMutex` splits the pen in two: any number of readers may look at the
board at once, but a writer waits for them all to finish and then works alone.
That fits a board written rarely and read often.

kubeagent's watch daemon is this kitchen. Each watched cluster gets a goroutine
that runs its own informers and its own evaluation loop, and they all write into
one `metrics` struct that the HTTP handler reads on every scrape:

    type metrics struct {
        mu       sync.RWMutex
        clusters map[string]*clusterSnapshot
    }

    func (m *metrics) update(cluster string, ...) {
        m.mu.Lock()
        defer m.mu.Unlock()
        c := m.snapshot(cluster)
        c.nodesReady = res.Health.NodesReady
        // ...
    }

Each worker only ever touches its own `clusterSnapshot`, so the clusters cannot
corrupt each other's readings — but they share the map and the render pass, and
that is what the mutex protects.

`go test -race ./internal/watch/` runs the suite with Go's race detector, which
fails the test if two goroutines touch the same memory without a lock between
them. Run it whenever goroutines share anything.
```

- [ ] **Step 5: Update the changelog and the roadmap**

Add to `CHANGELOG.md` under `## [Unreleased]`:

```markdown
### Added

- **Multi-cluster hub.** `kubeagent watch --context prod-eu --context prod-us`
  runs one informer set per cluster inside a single process, behind one HTTP
  endpoint. `--context` is now repeatable, `--cluster-name` names the default
  cluster (the one watched with no `--context`), and `--include-local` adds it
  alongside the listed contexts. Every metric series carries a `cluster` label,
  `/issues` and `/explanations` carry a `cluster` field, `/issues` gains a
  `clusters` roster with each target's up/down state, and every alert names its
  cluster. A context missing from the kubeconfig is fatal at startup; a cluster
  that fails at runtime reports `kubeagent_cluster_up 0` and degrades on its own
  while the others keep reconciling. `/readyz` reports ready once every cluster
  has finished a first reconcile attempt and never flips on cluster health. The
  daemon remains strictly read-only toward every cluster: get/list/watch only.
- The Helm chart gained `multicluster.*`: a kubeconfig mounted read-only from a
  Secret (never a `values.yaml` literal), one `--context` per listed entry, and
  the local cluster watched alongside them through its existing ServiceAccount.
  The chart's ClusterRole is unchanged and still covers the local cluster only —
  each remote credential must be read-only, which kubeagent cannot enforce from
  inside the pod.
- Chaos scenario 15 covers multi-cluster labelling, the cross-cluster merge, and
  per-cluster degradation with a deliberately dead third target.

### Changed

- Every per-cluster metric series now carries a `cluster` label, including in
  single-cluster operation, where it defaults to `local`. PromQL selectors match
  regardless of extra labels, so existing queries keep working; a recording rule
  that groups `by (...)` should add `cluster`. The alert and explanation series
  stay unlabelled — one sink and one budget per process, not per cluster.
- Alert payloads gained a cluster: a `cluster` field in the JSON format, a
  `cluster` label in the Alertmanager format, and a `prod-eu/Deployment/shop/web`
  object path in the Slack text.
```

In `website/docs/roadmap.md`, mark Theme E's multi-cluster hub slice shipped and the theme complete, following the file's existing convention for a shipped slice.

- [ ] **Step 6: Build the site**

```bash
export PATH=$PATH:/usr/local/go/bin
cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml; cd ..
```

Expected: exit 0 and no `WARNING` lines naming your pages. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 7: Secret check**

```bash
grep -rnE '(AKIA|BEGIN [A-Z ]*PRIVATE KEY|sk-ant-)' website/docs deploy/README.md CHANGELOG.md docs/go-concepts.md
grep -rnE '\b(10|192\.168)\.[0-9]+\.[0-9]+\.[0-9]+' website/docs deploy/README.md | grep -v '127\.0\.0\.1'
```

Expected: no output. Every credential in an example must be `<PLACEHOLDER>`.

- [ ] **Step 8: Full suite and commit**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... 2>&1 | grep -v "^ok" | head
git add website deploy/README.md CHANGELOG.md docs/go-concepts.md
git commit -m "docs: document the multi-cluster hub"
```

---

## Self-Review

**Spec coverage.** Targets and naming → Tasks 4, 6. Process shape (per-cluster workers, shared server/sink/explainer) → Task 4. Cluster identity at the boundary → Tasks 1, 2, 4. Metrics label policy, `kubeagent_cluster_up`, `kubeagent_clusters_total`, unlabelled alert/explain series, sorted rendering → Task 3. Failure isolation and `/readyz` → Tasks 3 (readiness state), 4 (bounded cache sync, degradation), 5 (`/issues` roster). HTTP surface → Task 5. Alerts (all three formats) → Task 1. Helm → Task 7. Invariants → asserted in Tasks 7 and 8. Testing → unit tests in Tasks 1-6, chaos in Task 8. Documentation → Task 9. Out-of-scope items appear in no task, as intended.

**One gap found and closed while reviewing:** the spec says `/readyz` goes ready after every cluster's first reconcile *attempt*, but the existing code waits on `WaitForCacheSync` before its first reconcile, and that call blocks forever against an unreachable cluster — so a dead target would have held the whole daemon out of readiness. Task 4 bounds the wait with `cacheSyncTimeout` while leaving the informers themselves running on the daemon's context.

**Two further deviations from a naive reading of the spec, both deliberate:**
- The spec puts the roller in the shared alerter; the plan moves it into each worker, because roller state is per-object and two clusters routinely run the same namespace/name. The sink — the part the spec calls shared — stays shared.
- `sloNotifier` gains a cluster so the error-budget alert names whose budget burned. The spec's "each cluster burns its own error budget" implies it; the plan makes it explicit.

**Type consistency.** `Target{Name, Client}` (Task 4) is used identically in Task 6. `newMetrics([]string)`, `update(cluster, …)`, `updateIssues(cluster, …)`, `updateSLO(cluster, …)`, `markReady(cluster)` are defined in Task 3 and consumed in Task 4. `Consider(clusterName, d, health, …)` is defined in Task 2 and called in Task 4. `alertstate.Options{Repeat, Cluster}` is defined in Task 1 and used in Task 4. `issueLabels(cluster, key)` is defined and used in Task 3. `alerter.notify(roller, tr, now)` is defined and called in Task 4.
