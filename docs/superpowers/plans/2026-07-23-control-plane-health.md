# Control-plane / etcd health (`--control-plane-health`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `--control-plane-health` check that probes the apiserver `/readyz?verbose` endpoint and flags an unhealthy control plane / etcd, mirroring the existing `--kubelet-health` probe end-to-end.

**Architecture:** A new pure package `internal/controlplane` (`ParseReadyz` classifies an HTTP code + verbose body into a `Probe`), a non-fatal `collect.ControlPlaneReadyz` probe, gated into `scan.Evaluate` as `Result.ControlPlane`, rendered as an opt-in `CONTROL PLANE` report section (pointer in `Input`, mapped via the runScan extras block exactly like `--kubelet-health`), a `watch` gauge, and a conditional `/readyz` RBAC grant.

**Tech Stack:** Go 1.26, `k8s.io/client-go` REST `AbsPath`, standard library (`strings`), fake clientset for the off-by-default scan test.

## Global Constraints

- **Read-only; opt-in; offline.** No writes, no LLM. Off by default → default output and the golden snapshot are unchanged.
- **Endpoint:** apiserver `/readyz?verbose` via `RESTClient().Get().AbsPath("/readyz").Param("verbose","true")`.
- **Statuses:** `ok` (200), `unhealthy` (other 2xx/5xx with body — list failing `[-]` checks), `forbidden` (401/403), `unreachable` (code 0). Failing-check names are the first token after each `[-]` prefix.
- **Advisory** — the `CONTROL PLANE` section renders but does NOT change the cluster verdict.
- **Pure & deterministic** — `ParseReadyz` reads only its arguments. The collector is the only I/O and never returns an error.
- **Mirror `--kubelet-health` exactly** for the wiring: `Input.ControlPlane` is a `*controlplane.Probe` (nil when off, so JSON omits it), mapped in the runScan extras block (`if *controlPlaneHealth { cpRep = &res.ControlPlane }; in.ControlPlane = cpRep`), NOT via `resultInput`. The section header carries `(opt-in)`.
- **Daemon** plumbs `KUBEAGENT_CONTROL_PLANE_HEALTH` → `watch.Config.ControlPlaneHealth` → `scan.Options.ControlPlaneHealth` (mirroring `KubeletHealth`).
- **RBAC:** the `/readyz` grant is a conditional add-on (Helm value `controlPlaneHealth.enabled` + a raw `deploy/rbac-controlplane.yaml`), mirroring `nodes/proxy` for disk-usage/kubelet-health.
- Gate: **FULL CHAOS GATE**. **Minor** bump v0.44.0 → **v0.45.0**; **chart MINOR** bump.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: `internal/controlplane` package (pure classification)

**Files:**
- Create: `internal/controlplane/controlplane.go`
- Test: `internal/controlplane/controlplane_test.go`

**Interfaces:**
- Produces: `type Probe struct { Status string; Failed []string }` and `func ParseReadyz(code int, body []byte) Probe`. Used by Tasks 2 (collector/scan), 3 (report), 4 (main), 5 (watch).

- [ ] **Step 1: Write the failing test**

Create `internal/controlplane/controlplane_test.go`:

```go
package controlplane

import (
	"reflect"
	"testing"
)

func TestParseReadyz_OK(t *testing.T) {
	body := []byte("[+]ping ok\n[+]etcd ok\nreadyz check passed\n")
	got := ParseReadyz(200, body)
	if got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %v, want none", got.Failed)
	}
}

func TestParseReadyz_UnhealthyListsFailedChecks(t *testing.T) {
	body := []byte("[+]ping ok\n[+]etcd ok\n[-]poststarthook/x failed: reason\n[-]informer-sync failed\nreadyz check failed\n")
	got := ParseReadyz(500, body)
	if got.Status != "unhealthy" {
		t.Fatalf("Status = %q, want unhealthy", got.Status)
	}
	want := []string{"poststarthook/x", "informer-sync"}
	if !reflect.DeepEqual(got.Failed, want) {
		t.Errorf("Failed = %v, want %v", got.Failed, want)
	}
}

func TestParseReadyz_EtcdFailure(t *testing.T) {
	body := []byte("[+]ping ok\n[-]etcd failed: reason withheld\nreadyz check failed\n")
	got := ParseReadyz(500, body)
	if got.Status != "unhealthy" {
		t.Fatalf("Status = %q, want unhealthy", got.Status)
	}
	found := false
	for _, f := range got.Failed {
		if f == "etcd" {
			found = true
		}
	}
	if !found {
		t.Errorf("Failed = %v, want it to contain etcd", got.Failed)
	}
}

func TestParseReadyz_Forbidden(t *testing.T) {
	for _, code := range []int{401, 403} {
		if got := ParseReadyz(code, nil); got.Status != "forbidden" {
			t.Errorf("code %d: Status = %q, want forbidden", code, got.Status)
		}
	}
}

func TestParseReadyz_Unreachable(t *testing.T) {
	if got := ParseReadyz(0, nil); got.Status != "unreachable" {
		t.Errorf("Status = %q, want unreachable", got.Status)
	}
}

func TestParseReadyz_EmptyBodyUnhealthy(t *testing.T) {
	got := ParseReadyz(503, nil)
	if got.Status != "unhealthy" {
		t.Errorf("Status = %q, want unhealthy", got.Status)
	}
	if len(got.Failed) != 0 {
		t.Errorf("Failed = %v, want none (generic not-ready)", got.Failed)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/controlplane/ 2>&1 | head`
Expected: compile failure — `undefined: ParseReadyz`.

- [ ] **Step 3: Write the implementation**

Create `internal/controlplane/controlplane.go`:

```go
// Package controlplane classifies the apiserver /readyz?verbose response into an
// advisory control-plane / etcd health probe. Pure and read-only: the caller
// (collect) does the HTTP GET and passes the status code and body here. Mirrors
// the nodehealth classify helper for kubelet /healthz.
package controlplane

import "strings"

// Probe is the apiserver /readyz classification.
type Probe struct {
	Status string   `json:"status"`           // "ok" | "unhealthy" | "forbidden" | "unreachable"
	Failed []string `json:"failed,omitempty"` // failing check names when unhealthy
}

// ParseReadyz classifies an HTTP status code and /readyz?verbose body into a Probe.
// 200 is ok; 401/403 is forbidden (grant missing); code 0 (no HTTP status) is
// unreachable; any other code means not-ready — the failing checks are the names
// immediately after each "[-]" line of the verbose body.
func ParseReadyz(code int, body []byte) Probe {
	switch {
	case code == 200:
		return Probe{Status: "ok"}
	case code == 401 || code == 403:
		return Probe{Status: "forbidden"}
	case code == 0:
		return Probe{Status: "unreachable"}
	default:
		return Probe{Status: "unhealthy", Failed: failedChecks(body)}
	}
}

// failedChecks extracts the check name from each "[-]<name> …" line of a verbose
// /readyz body, in order. Returns nil when there are none (a generic not-ready).
func failedChecks(body []byte) []string {
	var failed []string
	for _, ln := range strings.Split(string(body), "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "[-]") {
			if fields := strings.Fields(ln[3:]); len(fields) > 0 {
				failed = append(failed, fields[0])
			}
		}
	}
	return failed
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/controlplane/ -v 2>&1 | tail -20`
Expected: all tests PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/controlplane/
git add internal/controlplane/
git commit -m "feat(controlplane): classify apiserver /readyz into a control-plane health probe"
```

---

### Task 2: `collect.ControlPlaneReadyz` + scan wiring

**Files:**
- Modify: `internal/collect/collect.go` (add `ControlPlaneReadyz` next to `KubeletHealthz`, ≈ line 424)
- Modify: `internal/scan/scan.go` (`Options.ControlPlaneHealth`, `Result.ControlPlane`, the gated probe in `Evaluate`)
- Test: `internal/scan/scan_test.go` (`TestEvaluate_ControlPlaneHealthOffByDefault`)

**Interfaces:**
- Consumes: `controlplane.Probe` + `controlplane.ParseReadyz` (Task 1).
- Produces: `collect.ControlPlaneReadyz(ctx, client) controlplane.Probe`; `scan.Options.ControlPlaneHealth bool`; `scan.Result.ControlPlane controlplane.Probe`. Used by Tasks 3, 4, 5.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (mirrors `TestEvaluate_DiskUsageOffByDefault` / `TestEvaluate_KubeletHealthOffByDefault` — the off-by-default pattern that avoids the fake RESTClient; the ON parse-path is covered by Task 1's `ParseReadyz` table and the release smoke):

```go
func TestEvaluate_ControlPlaneHealthOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ControlPlane.Status != "" {
		t.Errorf("control plane must not be probed when disabled, got %+v", res.ControlPlane)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_ControlPlaneHealthOffByDefault 2>&1 | head`
Expected: compile failure — `res.ControlPlane` undefined.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, add next to `KubeletHealthz` (and add the `controlplane` import if not present):

```go
// ControlPlaneReadyz probes the apiserver /readyz?verbose endpoint and classifies
// the result. Never returns an error (non-fatal, like KubeletHealthz). Needs the
// nonResourceURLs /readyz get grant; a 401/403 yields Status "forbidden".
func ControlPlaneReadyz(ctx context.Context, client kubernetes.Interface) controlplane.Probe {
	var code int
	body, _ := client.CoreV1().RESTClient().Get().
		AbsPath("/readyz").Param("verbose", "true").
		Do(ctx).StatusCode(&code).Raw()
	return controlplane.ParseReadyz(code, body)
}
```

Add the import to `internal/collect/collect.go`:

```go
	"github.com/imantaba/kubeagent/internal/controlplane"
```

- [ ] **Step 4: Wire into scan**

In `internal/scan/scan.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/controlplane"` (alphabetical — between `confidence`/`connectivity` and `createhealth`; place it so `gofmt -l` is clean).
2. Add to the `Options` struct next to `KubeletHealth`:

```go
	ControlPlaneHealth bool
```

3. Add to the `Result` struct next to `KubeletHealth`:

```go
	ControlPlane controlplane.Probe
```

4. In `Evaluate`, near the existing kubelet-health block, add:

```go
	var controlPlane controlplane.Probe
	if opts.ControlPlaneHealth {
		controlPlane = collect.ControlPlaneReadyz(ctx, client)
	}
```

5. Add `ControlPlane: controlPlane` to the returned `Result{…}` literal (next to `KubeletHealth: kubeletHealth`).

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ ./internal/scan/ 2>&1 | tail -5`
Expected: PASS (the new off-by-default test and all existing collect/scan tests).

- [ ] **Step 6: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/collect/ internal/scan/
git add internal/collect/collect.go internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): probe apiserver /readyz when --control-plane-health is set"
```

---

### Task 3: Report — the `CONTROL PLANE` section

**Files:**
- Modify: `internal/report/report.go` (`Input.ControlPlane`, JSON struct + build literal, `controlPlaneRenders`, `printControlPlane`, call site, empty-cluster gate)
- Test: `internal/report/report_test.go` (`TestPrintControlPlane`)

**Interfaces:**
- Consumes: `controlplane.Probe` (Task 1).
- Produces: `report.Input.ControlPlane *controlplane.Probe` (set by Task 4's extras block); the rendered `CONTROL PLANE` section.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (import `"github.com/imantaba/kubeagent/internal/controlplane"`):

```go
func TestPrintControlPlane(t *testing.T) {
	// unhealthy → section with the failing checks
	unhealthy := &controlplane.Probe{Status: "unhealthy", Failed: []string{"etcd", "poststarthook/x"}}
	var b bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: unhealthy}, "text", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "CONTROL PLANE") || !strings.Contains(out, "control plane not ready") {
		t.Errorf("missing CONTROL PLANE section:\n%s", out)
	}
	if !strings.Contains(out, "2 checks failing: etcd, poststarthook/x") {
		t.Errorf("missing failing-checks line:\n%s", out)
	}

	// forbidden → grant hint
	var bf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: &controlplane.Probe{Status: "forbidden"}}, "text", &bf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bf.String(), "/readyz") {
		t.Errorf("forbidden should print a /readyz grant hint:\n%s", bf.String())
	}

	// ok / nil → nothing
	for _, p := range []*controlplane.Probe{{Status: "ok"}, nil} {
		var bo bytes.Buffer
		if err := PrintInventory(Input{Result: inventory.Result{}, ControlPlane: p}, "text", &bo); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bo.String(), "CONTROL PLANE") {
			t.Errorf("probe %+v should render no CONTROL PLANE section:\n%s", p, bo.String())
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintControlPlane 2>&1 | head`
Expected: FAIL — `Input` has no `ControlPlane` field.

- [ ] **Step 3: Add the field, renderer, and wiring**

In `internal/report/report.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/controlplane"` (alphabetical, before `credlint`).
2. Add to the JSON `inventoryReport` struct next to `KubeletHealth` (≈ line 48):

```go
	ControlPlane       *controlplane.Probe         `json:"controlPlane,omitempty"`
```

3. Add to the `Input` struct next to `KubeletHealth` (≈ line 76):

```go
	ControlPlane       *controlplane.Probe
```

4. Add to the Input→inventoryReport JSON-build literal next to `KubeletHealth: in.KubeletHealth` (≈ line 106):

```go
			ControlPlane:       in.ControlPlane,
```

5. Add the renderer + the "renders?" helper next to `printKubeletHealth` (≈ line 1010):

```go
// controlPlaneRenders reports whether the CONTROL PLANE section would print.
func controlPlaneRenders(p *controlplane.Probe) bool {
	return p != nil && (p.Status == "unhealthy" || p.Status == "forbidden")
}

// printControlPlane renders the advisory CONTROL PLANE section: the apiserver
// /readyz probe result when it is not ready (or a grant hint when forbidden).
func printControlPlane(p *controlplane.Probe, w io.Writer) error {
	if !controlPlaneRenders(p) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "CONTROL PLANE  (opt-in)"); err != nil {
		return err
	}
	switch p.Status {
	case "unhealthy":
		if _, err := fmt.Fprintln(w, "  ✗ control plane not ready"); err != nil {
			return err
		}
		if len(p.Failed) > 0 {
			if _, err := fmt.Fprintf(w, "      ⚠ %d checks failing: %s\n", len(p.Failed), strings.Join(p.Failed, ", ")); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(w, "      ⚠ apiserver /readyz reported not ready"); err != nil {
				return err
			}
		}
	case "forbidden":
		if _, err := fmt.Fprintln(w, "  ⚠ /readyz forbidden — grant nonResourceURLs /readyz to enable this check"); err != nil {
			return err
		}
	}
	return nil
}
```

6. Add the call site + gate next to the kubelet-health block (≈ line 191):

```go
	hasControlPlane := controlPlaneRenders(in.ControlPlane)
	if err := printControlPlane(in.ControlPlane, w); err != nil {
		return err
	}
```

7. Add `!hasControlPlane` to the "No issues found" empty-cluster guard (≈ line 209):

```go
	if !hasAttention && !hasSecurity && !hasKubeletHealth && !hasControlPlane && !hasCerts && in.Cluster.Verdict == "Healthy" {
```

- [ ] **Step 4: Run the report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -5`
Expected: PASS, including the unchanged `TestGoldenScanOutput` (the golden fixture sets no `ControlPlane`, so the snapshot is unaffected).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/report/
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the opt-in CONTROL PLANE section"
```

---

### Task 4: `main.go` — flag, env, and the extras-block mapping

**Files:**
- Modify: `main.go` (usage string, `--control-plane-health` flag, CLI `Options`, extras-block pointer mapping, env for the daemon-config path)
- Test: `main_test.go` (`TestRun_ControlPlaneHealthFlagAccepted`)

**Interfaces:**
- Consumes: `scan.Options.ControlPlaneHealth` (Task 2), `scan.Result.ControlPlane` (Task 2), `report.Input.ControlPlane` (Task 3).
- Produces: end-to-end CLI rendering when `--control-plane-health` is set.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (mirrors the existing flag-parse tests):

```go
func TestRun_ControlPlaneHealthFlagAccepted(t *testing.T) {
	err := run([]string{"scan", "--control-plane-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_ControlPlaneHealthFlagAccepted 2>&1 | head`
Expected: FAIL — `flag provided but not defined: -control-plane-health`.

- [ ] **Step 3: Add the flag, mapping, and env**

In `main.go`:

1. Add `[--control-plane-health]` to the usage string (line ≈ 61, next to `[--kubelet-health]`).
2. Add the flag near `kubeletHealth` (≈ line 76):

```go
	controlPlaneHealth := fs.Bool("control-plane-health", false, "probe the apiserver /readyz endpoint and flag an unhealthy control plane / etcd (needs the /readyz grant)")
```

3. In the CLI `scan.Options{…}` literal, next to `KubeletHealth: *kubeletHealth` (≈ line 118):

```go
		ControlPlaneHealth:     *controlPlaneHealth,
```

4. In the runScan extras block, mirror the `kubeletRep` pointer pattern (≈ line 174 and ≈ line 187):

```go
	var cpRep *controlplane.Probe
	if *controlPlaneHealth {
		cpRep = &res.ControlPlane
	}
```

and, next to `in.KubeletHealth = kubeletRep`:

```go
	in.ControlPlane = cpRep
```

5. In the `watch.Config{…}` literal (the daemon path, ≈ line 260 next to `KubeletHealth: envBool("KUBEAGENT_KUBELET_HEALTH", false)`):

```go
		ControlPlaneHealth:     envBool("KUBEAGENT_CONTROL_PLANE_HEALTH", false),
```

6. Add the import `"github.com/imantaba/kubeagent/internal/controlplane"` to `main.go` (alphabetical).

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . 2>&1 | tail -5`
Expected: build succeeds and the `main` tests PASS. (This depends on Task 5 having added `watch.Config.ControlPlaneHealth`; if Task 5 is not yet done, temporarily the `watch.Config` field is missing — do Task 5 before this step, OR add the `watch.Config` field as part of this task. See note below.)

> **Ordering note:** `main.go`'s `watch.Config{…}` literal in step 5 references `watch.Config.ControlPlaneHealth`, which Task 5 adds. To keep this task independently compilable, add the `ControlPlaneHealth bool` field to `internal/watch/watch.go`'s `Config` struct as part of THIS task's step 3 (it is a one-line struct field; Task 5 then consumes it in the `scan.Options` literal and the gauge). Commit both together here.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w main.go internal/watch/watch.go
git add main.go main_test.go internal/watch/watch.go
git commit -m "feat(main): add --control-plane-health flag, mapping, and env"
```

---

### Task 5: `watch` daemon — config plumbing + gauge

**Files:**
- Modify: `internal/watch/watch.go` (pass `ControlPlaneHealth` into the daemon's `scan.Options`)
- Modify: `internal/watch/metrics.go` (a `controlPlaneUnhealthy` field, its snapshot assignment, the gauge line)
- Test: `internal/watch/metrics_test.go` (extend the render assertion)

**Interfaces:**
- Consumes: `scan.Result.ControlPlane` (Task 2), `watch.Config.ControlPlaneHealth` (field added in Task 4).
- Produces: the `kubeagent_control_plane_unhealthy` gauge.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, add to the test's `scan.Result` fixture:

```go
		ControlPlane: controlplane.Probe{Status: "unhealthy", Failed: []string{"etcd"}},
```

and add `"kubeagent_control_plane_unhealthy 1"` to the asserted-substrings slice. Import `"github.com/imantaba/kubeagent/internal/controlplane"` in the test file.

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult 2>&1 | tail`
Expected: FAIL — the rendered metrics lack `kubeagent_control_plane_unhealthy 1`.

- [ ] **Step 3: Add the daemon plumbing + gauge**

1. In `internal/watch/watch.go`, add `ControlPlaneHealth: cfg.ControlPlaneHealth` to the `scan.Options{…}` literal (≈ line 95, next to `KubeletHealth: cfg.KubeletHealth`). (The `Config.ControlPlaneHealth` field was added in Task 4.)

2. In `internal/watch/metrics.go`:
   - Add a struct field next to `kubeletUnhealthy`:

```go
	controlPlaneUnhealthy int
```

   - In the snapshot/update function, next to `m.kubeletUnhealthy = len(res.KubeletHealth.Unhealthy)`:

```go
	m.controlPlaneUnhealthy = 0
	if res.ControlPlane.Status == "unhealthy" {
		m.controlPlaneUnhealthy = 1
	}
```

   - In the render function, next to the `kubeagent_kubelet_unhealthy` gauge line:

```go
	gauge("kubeagent_control_plane_unhealthy", "Apiserver /readyz reported the control plane not ready", float64(m.controlPlaneUnhealthy))
```

- [ ] **Step 4: Run the watch suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ . 2>&1 | tail -3`
Expected: PASS (watch + main).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/watch/
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_control_plane_unhealthy gauge"
```

---

### Task 6: RBAC + Helm add-on

**Files:**
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (conditional `/readyz` grant)
- Modify: `deploy/helm/kubeagent/values.yaml` (`controlPlaneHealth.enabled`)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (env when enabled)
- Create: `deploy/rbac-controlplane.yaml` (raw-manifest add-on)

**Interfaces:** none (deploy manifests). Enables the `--control-plane-health` probe against a real cluster.

- [ ] **Step 1: Add the conditional grant to the Helm ClusterRole**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, add a conditional block after the existing `nodes/proxy` block (before the `certs` block):

```yaml
  {{- if .Values.controlPlaneHealth.enabled }}
  - nonResourceURLs: ["/readyz"]
    verbs: [get]
  {{- end }}
```

- [ ] **Step 2: Add the values toggle**

In `deploy/helm/kubeagent/values.yaml`, next to `kubeletHealth:`:

```yaml
controlPlaneHealth:
  enabled: false
```

- [ ] **Step 3: Add the deployment env**

In `deploy/helm/kubeagent/templates/deployment.yaml`:

- Add `.Values.controlPlaneHealth.enabled` to the `{{- if or … }}` guard that wraps the env block (≈ line 37):

```yaml
          {{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled .Values.certs.enabled .Values.controlPlaneHealth.enabled }}
```

- Add the env entry next to the `KUBEAGENT_KUBELET_HEALTH` block:

```yaml
            {{- if .Values.controlPlaneHealth.enabled }}
            - name: KUBEAGENT_CONTROL_PLANE_HEALTH
              value: "true"
            {{- end }}
```

- [ ] **Step 4: Create the raw-manifest add-on**

Create `deploy/rbac-controlplane.yaml` (mirroring `deploy/rbac-diskusage.yaml`'s style):

```yaml
# Optional add-on grant for `kubeagent scan --control-plane-health` (and the daemon
# with KUBEAGENT_CONTROL_PLANE_HEALTH=true): read the apiserver /readyz endpoint to
# flag an unhealthy control plane / etcd. Strictly read-only (a single nonResourceURL
# GET). Apply alongside deploy/rbac.yaml when the control-plane-health check is used.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-controlplane
rules:
  - nonResourceURLs: ["/readyz"]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-controlplane
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-controlplane
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

(Match the ServiceAccount name/namespace used by `deploy/rbac-diskusage.yaml` — read that file first and mirror its `subjects` block exactly.)

- [ ] **Step 5: Verify the chart templates + lint**

```bash
export PATH=$PATH:$HOME/.local/bin
# default values: the /readyz grant must be ABSENT
helm template x deploy/helm/kubeagent | grep -c '/readyz' | grep -qx 0 && echo "absent by default ✓"
# enabled: the grant and env must be PRESENT
helm template x deploy/helm/kubeagent --set controlPlaneHealth.enabled=true | grep -q '/readyz' && echo "grant present when enabled ✓"
helm template x deploy/helm/kubeagent --set controlPlaneHealth.enabled=true | grep -q 'KUBEAGENT_CONTROL_PLANE_HEALTH' && echo "env present when enabled ✓"
helm lint deploy/helm/kubeagent 2>&1 | tail -2
```
Expected: absent-by-default, present-when-enabled (grant + env), lint 0 failures.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/kubeagent/templates/clusterrole.yaml deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/rbac-controlplane.yaml
git commit -m "feat(deploy): conditional /readyz grant for --control-plane-health"
```

---

### Task 7: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- `website/docs/features/diagnostics.md`: add a section near the other opt-in probes (`--kubelet-health`, `--disk-usage`) describing `--control-plane-health`: probes the apiserver `/readyz?verbose` and flags an unhealthy control plane / etcd, naming the failing checks; opt-in, read-only; needs the `/readyz` grant (Helm `controlPlaneHealth.enabled=true` or `deploy/rbac-controlplane.yaml`); the daemon gauge `kubeagent_control_plane_unhealthy`; advisory. Show the example:

  ```text
  CONTROL PLANE  (opt-in)
    ✗ control plane not ready
        ⚠ 2 checks failing: etcd, poststarthook/start-kube-apiserver-admission-initializer
  ```

- `README.md`: add `--control-plane-health` to the opt-in flag/usage list, noting the `/readyz` add-on grant.

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`:

  ```
  - **Control-plane / etcd health (`--control-plane-health`).** An opt-in probe of
    the apiserver `/readyz?verbose` endpoint flags an unhealthy control plane —
    naming the failing checks (etcd, admission/controller poststarthooks,
    informer-sync). Read-only; needs the `/readyz` add-on grant; the daemon exposes
    `kubeagent_control_plane_unhealthy`. First of the Theme-B control-plane closers.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the ResourceQuota entry, tagged **Theme-B**, noting it closes the control-plane/etcd-health gap (apiserver + etcd; scheduler/controller-manager a follow-on) and links to `features/diagnostics.md`.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the --control-plane-health check"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches `internal/collect`, the **RBAC** manifests + Helm **templates** (clusterrole/values/deployment), and `internal/watch` → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`), plus a live smoke of `--control-plane-health` (a healthy Kind cluster should print nothing; to see the section, the release smoke can eyeball the `forbidden` path by running without the grant, or note the parse-path is unit-covered). **Minor** bump **v0.44.0 → v0.45.0**; **chart MINOR** bump — clusterrole/values/deployment templates changed, so override the bump script's default patch to the next minor (0.15.0 → 0.16.0). Hold for the user's explicit "run release and push".
```
