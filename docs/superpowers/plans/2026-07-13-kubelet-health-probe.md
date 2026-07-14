# Kubelet Health Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in `scan --kubelet-health` probes each node's kubelet `/healthz` via `nodes/proxy` and flags a kubelet that is reachable but reporting unhealthy — in an advisory `KUBELET HEALTH` section + JSON `kubeletHealth` + daemon gauge `kubeagent_kubelet_unhealthy`.

**Architecture:** Mirrors `--disk-usage`. A new pure `internal/nodehealth` (`Probe`/`Report`/`Assess`); `collect.KubeletHealthz` does the proxy GET and a pure `classify` maps status→`ok`/`unhealthy`/`forbidden`/`unreachable`; `scan.Evaluate` loops nodes under an opt-in flag; `report` renders an advisory section; watch adds a gauge; Helm reuses the `nodes/proxy` add-on. Advisory — the cluster verdict is untouched.

**Tech Stack:** Go 1.26, `k8s.io/client-go` REST client (`nodes/proxy` subresource). Tests use fake objects + fake clientset.

## Global Constraints

- Read-only (a `GET /healthz`); **opt-in** (`--kubelet-health`, off by default); **advisory** — never changes the cluster verdict / `kubeagent_cluster_healthy` / exit code; not in `--explain`.
- Classification: HTTP 200 → `ok`; a non-200 the kubelet returned → `unhealthy` (detail = first `[-]` failed-check line, else first line, truncated); 401/403 → `forbidden`; transport error / no status → `unreachable`. Forbidden/unreachable are skipped (non-fatal, like `NodeStats`) — never flagged unhealthy.
- Reuses the existing `nodes/proxy` grant (`deploy/rbac-diskusage.yaml`, shared) — **no new RBAC type**. Helm broadens the `nodes/proxy` gate to `or diskUsage.enabled kubeletHealth.enabled` and adds a `kubeletHealth.enabled` value.
- Exact names: flag `--kubelet-health`; env `KUBEAGENT_KUBELET_HEALTH`; gauge `kubeagent_kubelet_unhealthy`; JSON field `kubeletHealth`; section header `KUBELET HEALTH`; Helm value `kubeletHealth.enabled`.
- The `KUBELET HEALTH` section is its own advisory block (like `SECURITY`): it does not change the verdict/attention line, but suppresses the `No issues found. ✅` all-clear when it renders anything.
- Commits carry no `Co-Authored-By: Claude` trailer. TDD. `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: `internal/nodehealth` package

**Files:**
- Create: `internal/nodehealth/nodehealth.go`
- Test: `internal/nodehealth/nodehealth_test.go`

**Interfaces:**
- Produces: `type Probe struct { Node, Status, Detail string }`; `type Issue struct { Node, Detail string }`; `type Report struct { Unhealthy []Issue; Probed, Forbidden int }`; `func Assess(probes []Probe) Report`.

- [ ] **Step 1: Write the failing test**

Create `internal/nodehealth/nodehealth_test.go`:

```go
package nodehealth

import "testing"

func TestAssess_CollectsUnhealthyAndCounts(t *testing.T) {
	probes := []Probe{
		{Node: "a", Status: "ok"},
		{Node: "b", Status: "unhealthy", Detail: "[-]pleg failed"},
		{Node: "c", Status: "forbidden"},
		{Node: "d", Status: "unreachable"},
	}
	rep := Assess(probes)
	if rep.Probed != 4 || rep.Forbidden != 1 {
		t.Fatalf("counts wrong: %+v", rep)
	}
	if len(rep.Unhealthy) != 1 || rep.Unhealthy[0].Node != "b" || rep.Unhealthy[0].Detail != "[-]pleg failed" {
		t.Errorf("want one unhealthy b, got %+v", rep.Unhealthy)
	}
}

func TestAssess_AllOKEmpty(t *testing.T) {
	rep := Assess([]Probe{{Node: "a", Status: "ok"}, {Node: "b", Status: "ok"}})
	if len(rep.Unhealthy) != 0 || rep.Forbidden != 0 || rep.Probed != 2 {
		t.Errorf("all ok -> no unhealthy: %+v", rep)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/nodehealth/`
Expected: FAIL to compile — `undefined: Probe` / `Assess`.

- [ ] **Step 3: Write the implementation**

Create `internal/nodehealth/nodehealth.go`:

```go
// Package nodehealth turns per-node kubelet /healthz probe results into an
// advisory report of nodes whose kubelet is reachable but reporting unhealthy.
// Pure and read-only: the caller (scan) does the probing.
package nodehealth

// Probe is one node's kubelet /healthz classification.
type Probe struct {
	Node   string `json:"node"`
	Status string `json:"status"` // "ok" | "unhealthy" | "forbidden" | "unreachable"
	Detail string `json:"detail,omitempty"`
}

// Issue is one node flagged unhealthy.
type Issue struct {
	Node   string `json:"node"`
	Detail string `json:"detail,omitempty"`
}

// Report is the advisory kubelet-health result.
type Report struct {
	Unhealthy []Issue `json:"unhealthy,omitempty"`
	Probed    int     `json:"probed"`
	Forbidden int     `json:"forbidden"`
}

// Assess collapses per-node probes into the report: the unhealthy nodes plus the
// probed/forbidden counts (used for the daemon gauge and the missing-grant hint).
func Assess(probes []Probe) Report {
	rep := Report{Probed: len(probes)}
	for _, p := range probes {
		switch p.Status {
		case "unhealthy":
			rep.Unhealthy = append(rep.Unhealthy, Issue{Node: p.Node, Detail: p.Detail})
		case "forbidden":
			rep.Forbidden++
		}
	}
	return rep
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/nodehealth/`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/nodehealth/
git commit -m "feat(nodehealth): advisory report from kubelet /healthz probes"
```

---

### Task 2: `collect.KubeletHealthz` + `classify`

**Files:**
- Modify: `internal/collect/collect.go` (add `strings` + `nodehealth` imports; `KubeletHealthz`, `classify`, `healthzDetail`)
- Test: `internal/collect/collect_test.go` (add `classify` unit tests)

**Interfaces:**
- Consumes: `nodehealth.Probe` (Task 1).
- Produces: `func KubeletHealthz(ctx context.Context, client kubernetes.Interface, node string) nodehealth.Probe`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go`:

```go
func TestClassifyKubeletHealthz(t *testing.T) {
	cases := []struct {
		code                   int
		body                   string
		wantStatus, wantDetail string
	}{
		{200, "ok", "ok", ""},
		{500, "[+]ping ok\n[-]pleg failed\nhealthz check failed", "unhealthy", "[-]pleg failed"},
		{500, "healthz check failed", "unhealthy", "healthz check failed"},
		{403, "forbidden", "forbidden", ""},
		{0, "", "unreachable", ""},
	}
	for _, c := range cases {
		p := classify("n", c.code, []byte(c.body))
		if p.Node != "n" || p.Status != c.wantStatus || p.Detail != c.wantDetail {
			t.Errorf("classify(%d, %q) = {%s, %q}, want {%s, %q}", c.code, c.body, p.Status, p.Detail, c.wantStatus, c.wantDetail)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestClassifyKubeletHealthz`
Expected: FAIL to compile — `undefined: classify`.

- [ ] **Step 3: Implement**

In `internal/collect/collect.go`, add `"strings"` and `"github.com/imantaba/kubeagent/internal/nodehealth"` to the imports (the block already imports `diskusage`; add `nodehealth` beside it). Add:

```go
// KubeletHealthz probes a node's kubelet /healthz via the nodes/proxy subresource
// and classifies the result. Never returns an error (non-fatal, like NodeStats).
func KubeletHealthz(ctx context.Context, client kubernetes.Interface, node string) nodehealth.Probe {
	var code int
	body, _ := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/healthz", node)).
		Do(ctx).StatusCode(&code).Raw()
	return classify(node, code, body)
}

// classify maps a /healthz probe result to a Probe. 200 is ok; 401/403 is
// forbidden (grant missing); code 0 (no HTTP status — transport error) is
// unreachable; any other status the kubelet returned is unhealthy.
func classify(node string, code int, body []byte) nodehealth.Probe {
	switch {
	case code == 200:
		return nodehealth.Probe{Node: node, Status: "ok"}
	case code == 401 || code == 403:
		return nodehealth.Probe{Node: node, Status: "forbidden"}
	case code == 0:
		return nodehealth.Probe{Node: node, Status: "unreachable"}
	default:
		return nodehealth.Probe{Node: node, Status: "unhealthy", Detail: healthzDetail(body, 120)}
	}
}

// healthzDetail returns the first failed-check line ("[-]…") from a kubelet
// /healthz body, else the first non-empty line, trimmed and truncated to max runes.
func healthzDetail(body []byte, max int) string {
	var first string
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if strings.HasPrefix(ln, "[-]") {
			return truncateRunes(ln, max)
		}
	}
	return truncateRunes(first, max)
}

func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./... && go test ./internal/collect/ -run TestClassifyKubeletHealthz`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): probe + classify kubelet /healthz via nodes/proxy"
```

---

### Task 3: Wire into `scan` + `--kubelet-health`

**Files:**
- Modify: `internal/scan/scan.go` (import `nodehealth`; `Options.KubeletHealth`; `Result.KubeletHealth`; `Evaluate` probe loop; `Result` literal)
- Modify: `main.go` (flag + usage string + `scan.Options` literal)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `collect.KubeletHealthz`, `nodehealth.Report`/`Assess` (Tasks 1–2).
- Produces: `scan.Options.KubeletHealth bool`; `scan.Result.KubeletHealth nodehealth.Report`.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (imports `corev1`, `metav1`, `fake`, `context` already present):

```go
func TestEvaluate_KubeletHealthOptIn(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	)
	// Opt-in: the node is probed (the fake clientset can't serve the kubelet
	// proxy, so the probe classifies as unreachable — but Probed proves the loop ran).
	on, err := Evaluate(context.Background(), client, Options{KubeletHealth: true})
	if err != nil {
		t.Fatal(err)
	}
	if on.KubeletHealth.Probed != 1 {
		t.Errorf("with --kubelet-health the node must be probed: %+v", on.KubeletHealth)
	}
	// Opt-out: no probing.
	off, _ := Evaluate(context.Background(), client, Options{})
	if off.KubeletHealth.Probed != 0 {
		t.Errorf("without --kubelet-health no probing: %+v", off.KubeletHealth)
	}
}
```

If the fake clientset's proxy call panics (rather than returning an error), STOP and report NEEDS_CONTEXT — the design assumes it returns an error → `code 0` → `unreachable`.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_KubeletHealth`
Expected: FAIL — `Options`/`Result` have no `KubeletHealth`.

- [ ] **Step 3: Wire `scan.go`**

In `internal/scan/scan.go`, add the import `"github.com/imantaba/kubeagent/internal/nodehealth"`. Add to `Options` (after `ExpectedNodes`):

```go
	KubeletHealth bool
```

Add to `Result` (after `DiskUsage`):

```go
	KubeletHealth nodehealth.Report
```

After the `if opts.DiskUsage { ... }` block (right before the `return Result{...}`), add:

```go
	var kubeletHealth nodehealth.Report
	if opts.KubeletHealth {
		var probes []nodehealth.Probe
		for _, n := range nodes {
			probes = append(probes, collect.KubeletHealthz(ctx, client, n.Name))
		}
		kubeletHealth = nodehealth.Assess(probes)
	}
```

Add `KubeletHealth: kubeletHealth` to the returned `Result{...}` literal.

- [ ] **Step 4: Wire `main.go`**

Add the flag next to the disk flags (after `disk-threshold`, ~line 74):

```go
	kubeletHealth := fs.Bool("kubelet-health", false, "probe each kubelet's /healthz via nodes/proxy and flag unhealthy nodes (needs the nodes/proxy add-on)")
```

Add `[--kubelet-health]` to the scan usage string (~line 60), after `[--disk-usage [--disk-threshold r]]`.

Add to the `scan.Options{...}` literal (~line 99):

```go
		KubeletHealth:          *kubeletHealth,
```

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/scan/scan.go main.go
git commit -m "feat(scan): opt-in --kubelet-health probe"
```

---

### Task 4: Report — `KUBELET HEALTH` section + JSON

**Files:**
- Modify: `internal/report/report.go` (import `nodehealth`; `Input.KubeletHealth`; `inventoryReport.KubeletHealth`; `printKubeletHealth` + `kubeletHealthRenders`; call + all-clear guard)
- Modify: `main.go` (pass `KubeletHealth` into `report.Input`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `nodehealth.Report` (Task 1); `scan.Result.KubeletHealth` (Task 3).
- Produces: `report.Input.KubeletHealth *nodehealth.Report`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add the `"github.com/imantaba/kubeagent/internal/nodehealth"` import):

```go
func TestPrintInventory_KubeletHealthUnhealthy(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 2, Unhealthy: []nodehealth.Issue{{Node: "worker-2", Detail: "[-]pleg failed"}}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "KUBELET HEALTH") || !strings.Contains(out, "✗ node worker-2 kubelet /healthz unhealthy: [-]pleg failed") {
		t.Errorf("missing kubelet-health section:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when unhealthy:\n%s", out)
	}
}

func TestPrintInventory_KubeletHealthForbiddenHint(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 3, Forbidden: 3},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "needs the nodes/proxy add-on") {
		t.Errorf("missing missing-grant hint:\n%s", buf.String())
	}
}

func TestPrintInventory_KubeletHealthCleanNoSection(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 3}, // all ok
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "KUBELET HEALTH") {
		t.Errorf("no section expected when all healthy:\n%s", out)
	}
	if !strings.Contains(out, "No issues found") {
		t.Errorf("all-clear preserved when clean:\n%s", out)
	}
}

func TestPrintInventory_KubeletHealthJSON(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:       clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		KubeletHealth: &nodehealth.Report{Probed: 1, Unhealthy: []nodehealth.Issue{{Node: "w", Detail: "d"}}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"kubeletHealth"`) || !strings.Contains(buf.String(), `"node": "w"`) {
		t.Errorf("expected kubeletHealth in JSON:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run KubeletHealth`
Expected: FAIL to compile — `Input` has no `KubeletHealth`.

- [ ] **Step 3: Add the field, JSON, renderer, and all-clear guard**

In `internal/report/report.go`, add the import `"github.com/imantaba/kubeagent/internal/nodehealth"`. Add to `Input` (after `SecurityVerbose`):

```go
	KubeletHealth      *nodehealth.Report
```

Add to `inventoryReport` (after `SecurityIssues`):

```go
	KubeletHealth      *nodehealth.Report          `json:"kubeletHealth,omitempty"`
```

Add `KubeletHealth: in.KubeletHealth,` to the `inventoryReport{...}` literal in the json branch.

In `printInventoryText`, after the security section block (the `printSecurityIssues` call), add:

```go
	hasKubeletHealth := kubeletHealthRenders(in.KubeletHealth)
	if err := printKubeletHealth(in.KubeletHealth, w); err != nil {
		return err
	}
```

Change the all-clear condition from `if !hasAttention && !hasSecurity && in.Cluster.Verdict == "Healthy" {` to:

```go
	if !hasAttention && !hasSecurity && !hasKubeletHealth && in.Cluster.Verdict == "Healthy" {
```

Add the renderer + helper (near `printDiskUsage`):

```go
// kubeletHealthRenders reports whether the KUBELET HEALTH section would print
// anything: unhealthy nodes, or the missing-grant hint (every probe forbidden).
func kubeletHealthRenders(rep *nodehealth.Report) bool {
	if rep == nil {
		return false
	}
	return len(rep.Unhealthy) > 0 || (rep.Probed > 0 && rep.Forbidden == rep.Probed)
}

// printKubeletHealth renders the advisory KUBELET HEALTH section: nodes whose
// kubelet /healthz reported unhealthy, or a hint when the nodes/proxy grant is missing.
func printKubeletHealth(rep *nodehealth.Report, w io.Writer) error {
	if !kubeletHealthRenders(rep) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "KUBELET HEALTH  (opt-in)"); err != nil {
		return err
	}
	if rep.Probed > 0 && rep.Forbidden == rep.Probed {
		if _, err := fmt.Fprintln(w, "  kubelet-health needs the nodes/proxy add-on (deploy/rbac-diskusage.yaml or Helm kubeletHealth.enabled)"); err != nil {
			return err
		}
		return nil
	}
	for _, iss := range rep.Unhealthy {
		line := fmt.Sprintf("  ✗ node %s kubelet /healthz unhealthy", iss.Node)
		if iss.Detail != "" {
			line += ": " + iss.Detail
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Update `main.go`**

In `main.go`, mirror the disk-usage pointer pattern. After the `var diskRep *diskusage.Report; if *diskUsage { diskRep = &res.DiskUsage }` block, add:

```go
	var kubeletRep *nodehealth.Report
	if *kubeletHealth {
		kubeletRep = &res.KubeletHealth
	}
```

Add the import `"github.com/imantaba/kubeagent/internal/nodehealth"` to `main.go`, and add to the `report.Input{...}` literal (after `DiskUsage: diskRep,`):

```go
		KubeletHealth:      kubeletRep,
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (4 new tests + existing unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): advisory KUBELET HEALTH section + kubeletHealth JSON"
```

---

### Task 5: Daemon gauge + Helm enablement

**Files:**
- Modify: `internal/watch/watch.go` (`Config.KubeletHealth`; pass into `scan.Options`)
- Modify: `internal/watch/metrics.go` (`metrics` field; `update`; `render`)
- Modify: `main.go` (`watch.Config` literal — env)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (broaden nodes/proxy gate)
- Modify: `deploy/helm/kubeagent/values.yaml` (add `kubeletHealth.enabled`)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (env block)
- Test: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `scan.Options.KubeletHealth` (Task 3); `Result.KubeletHealth.Unhealthy` (Tasks 1/3).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, set a kubelet-health report on the `sampleResult()` `Result` (add `KubeletHealth: nodehealth.Report{Probed: 2, Unhealthy: []nodehealth.Issue{{Node: "w"}}}` — add the `"github.com/imantaba/kubeagent/internal/nodehealth"` import), and add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_kubelet_unhealthy 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauge missing (and compile error until the field exists).

- [ ] **Step 3: Implement the daemon side**

In `internal/watch/watch.go`, add to `Config` (after `ExpectedNodes`):

```go
	KubeletHealth bool
```

and add it to the `scan.Options{...}` construction (line ~92):

```go
		KubeletHealth: cfg.KubeletHealth,
```

In `internal/watch/metrics.go`, add to the `metrics` struct (after `volumesOverDisk int`):

```go
	kubeletUnhealthy int
```

In `update`, in the success path alongside the other node gauges (e.g. after `m.nodesStaleHeartbeat = res.Health.NodesStaleHeartbeat` or `m.nodesExpectedAbsent = ...`), add:

```go
	m.kubeletUnhealthy = len(res.KubeletHealth.Unhealthy)
```

In `render`, after the `kubeagent_nodes_expected_absent` gauge line, add:

```go
	gauge("kubeagent_kubelet_unhealthy", "Nodes whose kubelet /healthz reported unhealthy", float64(m.kubeletUnhealthy))
```

In `main.go`, in the `watch.Config{...}` literal (after `ExpectedNodes: splitCSV(...)`, ~line 219), add:

```go
		KubeletHealth:          envBool("KUBEAGENT_KUBELET_HEALTH", false),
```

- [ ] **Step 4: Implement the Helm enablement**

In `deploy/helm/kubeagent/values.yaml`, after the `diskUsage:` block, add:

```yaml

kubeletHealth:
  enabled: false
```

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, broaden the `nodes/proxy` gate — change `{{- if .Values.diskUsage.enabled }}` (the line above the `nodes/proxy` rule) to:

```yaml
  {{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled }}
```

In `deploy/helm/kubeagent/templates/deployment.yaml`, replace the disk-usage env block (currently `{{- if .Values.diskUsage.enabled }} env: … {{- end }}`, ~lines 37–43) with one that renders when either feature is enabled and includes each feature's env conditionally:

```yaml
          {{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled }}
          env:
            {{- if .Values.diskUsage.enabled }}
            - name: KUBEAGENT_DISK_USAGE
              value: "true"
            - name: KUBEAGENT_DISK_THRESHOLD
              value: {{ .Values.diskUsage.threshold | quote }}
            {{- end }}
            {{- if .Values.kubeletHealth.enabled }}
            - name: KUBEAGENT_KUBELET_HEALTH
              value: "true"
            {{- end }}
          {{- end }}
```

- [ ] **Step 5: Run tests + verify the chart**

Run:

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin:/usr/local/bin
go build ./... && go test ./internal/watch/
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent --set kubeletHealth.enabled=true | grep -A2 'nodes/proxy'
helm template x deploy/helm/kubeagent --set kubeletHealth.enabled=true | grep KUBEAGENT_KUBELET_HEALTH
helm template x deploy/helm/kubeagent | grep -iE 'create|update|patch|delete' | grep -i verb && echo BAD || echo "read-only OK"
```

Expected: watch tests PASS; lint clean; with `kubeletHealth.enabled=true` the rendered ClusterRole includes `nodes/proxy [get]` and the Deployment has `KUBEAGENT_KUBELET_HEALTH`; the write-verb check prints `read-only OK`.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go main.go deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/clusterrole.yaml deploy/helm/kubeagent/templates/deployment.yaml
git commit -m "feat(watch): kubeagent_kubelet_unhealthy gauge + Helm kubeletHealth.enabled"
```

---

### Task 6: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `website/docs/roadmap.md`
- Modify: `README.md`

**Interfaces:** none. Exact names: flag `--kubelet-health`; env `KUBEAGENT_KUBELET_HEALTH`; gauge `kubeagent_kubelet_unhealthy`; JSON `kubeletHealth`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry with:

```markdown
## [Unreleased]

### Added

- **Kubelet health probe.** Opt-in `scan --kubelet-health` probes each node's
  kubelet `/healthz` through the `nodes/proxy` subresource and flags a kubelet
  that is reachable but reporting unhealthy — `✗ node worker-2 kubelet /healthz
  unhealthy: [-]pleg failed` — the "alive but sick" case the lease-heartbeat and
  NotReady checks miss. Shown in a `KUBELET HEALTH` section and JSON
  `kubeletHealth`, with the watch gauge `kubeagent_kubelet_unhealthy` (set
  `KUBEAGENT_KUBELET_HEALTH`). Read-only and **advisory** (does not change the
  cluster verdict); reuses the same `nodes/proxy` add-on as `--disk-usage` (no
  new RBAC).
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^### ' website/docs/features/diagnostics.md | head`

Add a subsection (matching the heading level/style) after the node-heartbeat / expected-node material:

```markdown
### Kubelet health probe (opt-in)

`scan --kubelet-health` actively probes each node's kubelet `/healthz` through
the `nodes/proxy` subresource (the same add-on `--disk-usage` uses) and flags a
kubelet that is **reachable but reporting unhealthy** — `✗ node worker-2 kubelet
/healthz unhealthy: [-]pleg failed`. This is the "alive but sick" failure mode
(a failing PLEG/runtime/syncloop subcheck) that the passive lease-heartbeat and
`NotReady` checks miss, and it often shows *before* the node flips to `NotReady`.
A dead/unreachable kubelet is skipped (already flagged by the node checks), and a
missing `nodes/proxy` grant prints a one-line hint. It is read-only (a `GET`),
opt-in, and **advisory** — it appears in the `KUBELET HEALTH` section and JSON
`kubeletHealth` but does not change the cluster verdict. Enable it in the daemon
with `KUBEAGENT_KUBELET_HEALTH=true` and the `nodes/proxy` add-on
(`deploy/rbac-diskusage.yaml` or Helm `kubeletHealth.enabled=true`).
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|KUBEAGENT_' website/docs/features/watch-mode.md | head -30`

Add `kubeagent_kubelet_unhealthy` to the documented metrics (neighbouring format), described as "Number of nodes whose kubelet /healthz reported unhealthy", and document the `KUBEAGENT_KUBELET_HEALTH` env (needs the shared `nodes/proxy` add-on) alongside the other `KUBEAGENT_*` env vars.

- [ ] **Step 4: roadmap.md**

Run: `grep -nE 'Shipped|Version history' website/docs/roadmap.md | head`

Add a bullet to the "Shipped" list (before the `!!! info "Version history"` block), matching the existing style:

```markdown
- **Kubelet health probe** — opt-in `scan --kubelet-health` probes each kubelet's
  `/healthz` via `nodes/proxy` and flags an alive-but-unhealthy kubelet in a
  `KUBELET HEALTH` section, with the daemon gauge `kubeagent_kubelet_unhealthy`.
  See [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: README**

Run: `grep -nE 'disk-usage|kubelet|node-heartbeat|expected|detect' README.md | head`

Add a one-line mention of the kubelet-health probe alongside the existing feature list, matching the surrounding style.

- [ ] **Step 6: Verify the docs build**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, "Documentation built", no page `WARNING` lines (the Material for MkDocs team banner is cosmetic — ignore it). If the venv is missing, recreate: `python3 -m venv <path> && <path>/bin/pip install -r requirements.txt`.

- [ ] **Step 7: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md website/docs/roadmap.md README.md
git commit -m "docs: document the kubelet health probe + its daemon gauge"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke against a cluster with the `nodes/proxy` grant:

```bash
go build -o kubeagent . && ./kubeagent scan --kubelet-health | sed -n '/KUBELET HEALTH/,/^$/p'
./kubeagent scan --kubelet-health --output json | grep -o '"kubeletHealth"'
```

Expected: a `KUBELET HEALTH` section (or the missing-grant hint without the add-on); `kubeletHealth` present in JSON. On a healthy cluster with the grant, no section prints.
