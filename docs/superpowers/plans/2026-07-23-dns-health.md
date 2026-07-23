# DNS / CoreDNS resolution health (`--dns-health`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `--dns-health` check that reads CoreDNS `:9153/metrics` and flags an elevated SERVFAIL+REFUSED response ratio (DNS up but failing to resolve), mirroring the `--control-plane-health` opt-in-probe pattern.

**Architecture:** A new pure package `internal/dnshealth` (`ParseResponses` sums CoreDNS response counts by rcode; `Assess` turns the aggregate + probe outcomes into a `Report`), a non-fatal `collect.CoreDNSMetrics` pods/proxy probe, gated into `scan.Evaluate` (find CoreDNS pods → probe each → aggregate → assess) as `Result.DNS`, an opt-in `DNS` report section (pointer in `Input`, extras-block mapping), a float `watch` gauge, and a conditional `pods/proxy` RBAC grant.

**Tech Stack:** Go 1.26, `k8s.io/client-go` REST `AbsPath` pods/proxy, standard library (`strings`, `strconv`), fake clientset for the off-by-default scan test.

## Global Constraints

- **Read-only; opt-in; offline.** No writes, no LLM. Off by default → default output and the golden snapshot unchanged.
- **Error set:** SERVFAIL + REFUSED (NXDOMAIN/NOERROR are legitimate). Ratio = errors/total.
- **Threshold:** default **0.05**, env `KUBEAGENT_DNS_SERVFAIL_RATIO` (clamped to `(0,1]`); volume **floor fixed at 100** total responses.
- **Metric names:** parse BOTH `coredns_dns_responses_total` and the pre-1.7 `coredns_dns_response_rcode_count_total`.
- **CoreDNS pods:** namespace `kube-system`, pod label `k8s-app == "kube-dns"`, phase Running.
- **Advisory** — the `DNS` section renders but does NOT change the cluster verdict.
- **Pure & deterministic** — `ParseResponses`/`Assess` read only their args; the collector is the only I/O and never returns an error (probe failure → forbidden/unreachable, never a scan failure).
- **Mirror `--control-plane-health`** for the wiring: `Input.DNS` is a `*dnshealth.Report` (nil when off → JSON omits it), mapped in the runScan extras block (`if *dnsHealth { dnsRep = &res.DNS }; in.DNS = dnsRep`), NOT via `resultInput`. Section header carries `(opt-in)`.
- **Daemon** plumbs `KUBEAGENT_DNS_HEALTH` + `KUBEAGENT_DNS_SERVFAIL_RATIO` → `watch.Config` → `scan.Options`.
- **RBAC:** the `pods/proxy` grant is a conditional add-on (Helm `dnsHealth.enabled` + raw `deploy/rbac-dnshealth.yaml`).
- Gate: **FULL CHAOS GATE**. **Minor** bump v0.45.0 → **v0.46.0**; **chart MINOR** bump.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: `internal/dnshealth` package (pure parse + assess)

**Files:**
- Create: `internal/dnshealth/dnshealth.go`
- Test: `internal/dnshealth/dnshealth_test.go`

**Interfaces:**
- Produces: `type Report struct { Status string; ServfailRatio float64; ErrorResponses, TotalResponses int64; PodsProbed int; Detail string }`, `func ParseResponses(body []byte) map[string]int64`, `func Assess(agg map[string]int64, podsProbed, forbidden, unreachable int, threshold float64, floor int64) Report`. Used by Tasks 2 (scan), 3 (report), 4 (main), 5 (watch).

- [ ] **Step 1: Write the failing test**

Create `internal/dnshealth/dnshealth_test.go`:

```go
package dnshealth

import (
	"reflect"
	"testing"
)

func TestParseResponses_CurrentMetric(t *testing.T) {
	body := []byte(`# HELP coredns_dns_responses_total Counter of responses.
# TYPE coredns_dns_responses_total counter
coredns_dns_responses_total{server="dns://:53",zone=".",view="",rcode="NOERROR"} 900
coredns_dns_responses_total{server="dns://:53",zone=".",view="",rcode="SERVFAIL"} 100
`)
	got := ParseResponses(body)
	want := map[string]int64{"NOERROR": 900, "SERVFAIL": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResponses_LegacyMetric(t *testing.T) {
	body := []byte(`coredns_dns_response_rcode_count_total{server="dns://:53",zone=".",rcode="REFUSED"} 5
`)
	got := ParseResponses(body)
	if got["REFUSED"] != 5 {
		t.Errorf("legacy metric: REFUSED = %d, want 5", got["REFUSED"])
	}
}

func TestParseResponses_SumsSeriesAndIgnoresJunk(t *testing.T) {
	body := []byte(`coredns_dns_responses_total{server="a",rcode="NOERROR"} 10
coredns_dns_responses_total{server="b",rcode="NOERROR"} 15

# a comment
coredns_build_info{version="1.11"} 1
coredns_dns_responses_total{server="a"} 99
not a metric line
`)
	got := ParseResponses(body)
	if got["NOERROR"] != 25 {
		t.Errorf("NOERROR = %d, want 25 (summed across series)", got["NOERROR"])
	}
	if len(got) != 1 {
		t.Errorf("want only NOERROR (no-rcode and unrelated lines ignored), got %v", got)
	}
}

func TestAssess_Degraded(t *testing.T) {
	agg := map[string]int64{"NOERROR": 9000, "SERVFAIL": 800, "REFUSED": 200}
	got := Assess(agg, 2, 0, 0, 0.05, 100)
	if got.Status != "degraded" {
		t.Fatalf("Status = %q, want degraded", got.Status)
	}
	if got.ErrorResponses != 1000 || got.TotalResponses != 10000 {
		t.Errorf("errors/total = %d/%d, want 1000/10000", got.ErrorResponses, got.TotalResponses)
	}
	if got.ServfailRatio < 0.099 || got.ServfailRatio > 0.101 {
		t.Errorf("ratio = %v, want ~0.10", got.ServfailRatio)
	}
	if got.PodsProbed != 2 {
		t.Errorf("PodsProbed = %d, want 2", got.PodsProbed)
	}
}

func TestAssess_UnderThreshold(t *testing.T) {
	agg := map[string]int64{"NOERROR": 9800, "SERVFAIL": 200} // 2%
	if got := Assess(agg, 1, 0, 0, 0.05, 100); got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
}

func TestAssess_BelowFloorNotFlagged(t *testing.T) {
	agg := map[string]int64{"NOERROR": 30, "SERVFAIL": 20} // 40% but only 50 total
	if got := Assess(agg, 1, 0, 0, 0.05, 100); got.Status != "ok" {
		t.Errorf("Status = %q, want ok (below floor)", got.Status)
	}
}

func TestAssess_NoPods(t *testing.T) {
	if got := Assess(nil, 0, 0, 0, 0.05, 100); got.Status != "" {
		t.Errorf("Status = %q, want empty (no CoreDNS pods)", got.Status)
	}
}

func TestAssess_AllForbidden(t *testing.T) {
	if got := Assess(nil, 2, 2, 0, 0.05, 100); got.Status != "forbidden" {
		t.Errorf("Status = %q, want forbidden", got.Status)
	}
}

func TestAssess_AllUnreachable(t *testing.T) {
	if got := Assess(nil, 2, 0, 2, 0.05, 100); got.Status != "unreachable" {
		t.Errorf("Status = %q, want unreachable", got.Status)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/dnshealth/ 2>&1 | head`
Expected: compile failure — `undefined: ParseResponses` / `Assess`.

- [ ] **Step 3: Write the implementation**

Create `internal/dnshealth/dnshealth.go`:

```go
// Package dnshealth turns CoreDNS /metrics response counters into an advisory
// resolution-health report: it flags an elevated SERVFAIL+REFUSED response ratio
// (DNS up but failing to resolve). Pure and read-only: the caller (scan) probes
// the CoreDNS pods and passes the parsed counts here.
package dnshealth

import (
	"strconv"
	"strings"
)

// Report is the advisory CoreDNS resolution-health result.
type Report struct {
	Status         string  `json:"status"`         // "ok" | "degraded" | "forbidden" | "unreachable" | ""
	ServfailRatio  float64 `json:"servfailRatio"`  // (SERVFAIL+REFUSED)/total
	ErrorResponses int64   `json:"errorResponses"` // SERVFAIL + REFUSED
	TotalResponses int64   `json:"totalResponses"`
	PodsProbed     int     `json:"podsProbed"`
	Detail         string  `json:"detail,omitempty"`
}

// ParseResponses sums CoreDNS DNS response counts by rcode from one pod's /metrics
// body. It reads both the current metric name (coredns_dns_responses_total) and the
// pre-1.7 name (coredns_dns_response_rcode_count_total). Returns rcode → count.
func ParseResponses(body []byte) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "coredns_dns_responses_total{") &&
			!strings.HasPrefix(line, "coredns_dns_response_rcode_count_total{") {
			continue
		}
		rcode := labelValue(line, "rcode")
		if rcode == "" {
			continue
		}
		brace := strings.LastIndexByte(line, '}')
		if brace < 0 {
			continue
		}
		fields := strings.Fields(line[brace+1:])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[rcode] += int64(v)
	}
	return out
}

// labelValue returns the value of the `key="..."` label in a Prometheus sample
// line, or "" when absent.
func labelValue(line, key string) string {
	needle := key + `="`
	i := strings.Index(line, needle)
	if i < 0 {
		return ""
	}
	rest := line[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// Assess collapses the aggregated rcode counts (summed across all probed pods) and
// the per-pod probe outcomes into a Report. threshold is the error ratio that trips
// "degraded"; floor is the minimum total responses required to judge.
func Assess(agg map[string]int64, podsProbed, forbidden, unreachable int, threshold float64, floor int64) Report {
	if podsProbed == 0 {
		return Report{Status: ""}
	}
	if forbidden > 0 && forbidden == podsProbed {
		return Report{Status: "forbidden"}
	}
	if unreachable == podsProbed {
		return Report{Status: "unreachable"}
	}
	var total int64
	for _, v := range agg {
		total += v
	}
	if total == 0 {
		// No pod returned usable metrics; prefer the concrete failure reason.
		switch {
		case forbidden > 0:
			return Report{Status: "forbidden"}
		case unreachable > 0:
			return Report{Status: "unreachable"}
		default:
			return Report{Status: "", PodsProbed: podsProbed}
		}
	}
	errors := agg["SERVFAIL"] + agg["REFUSED"]
	ratio := float64(errors) / float64(total)
	if total < floor {
		return Report{Status: "ok", TotalResponses: total, PodsProbed: podsProbed}
	}
	if ratio >= threshold {
		return Report{Status: "degraded", ServfailRatio: ratio, ErrorResponses: errors, TotalResponses: total, PodsProbed: podsProbed}
	}
	return Report{Status: "ok", ServfailRatio: ratio, ErrorResponses: errors, TotalResponses: total, PodsProbed: podsProbed}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/dnshealth/ -v 2>&1 | tail -20`
Expected: all tests PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/dnshealth/
git add internal/dnshealth/
git commit -m "feat(dnshealth): flag an elevated CoreDNS SERVFAIL+REFUSED ratio"
```

---

### Task 2: `collect.CoreDNSMetrics` + scan wiring

**Files:**
- Modify: `internal/collect/collect.go` (add `CoreDNSMetrics` next to `KubeletHealthz`/`ControlPlaneReadyz`)
- Modify: `internal/scan/scan.go` (`Options.DNSHealth`+`DNSServfailRatio`, `Result.DNS`, the `coreDNSPods` helper + gated probe/aggregate/assess block)
- Test: `internal/scan/scan_test.go` (`TestEvaluate_DNSHealthOffByDefault`)

**Interfaces:**
- Consumes: `dnshealth.ParseResponses`, `dnshealth.Assess`, `dnshealth.Report` (Task 1).
- Produces: `collect.CoreDNSMetrics(ctx, client, ns, pod) ([]byte, int)`; `scan.Options.DNSHealth bool` + `DNSServfailRatio float64`; `scan.Result.DNS dnshealth.Report`. Used by Tasks 3, 4, 5.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (mirrors `TestEvaluate_ControlPlaneHealthOffByDefault`):

```go
func TestEvaluate_DNSHealthOffByDefault(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}},
	)
	res, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.DNS.Status != "" {
		t.Errorf("DNS must not be probed when disabled, got %+v", res.DNS)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_DNSHealthOffByDefault 2>&1 | head`
Expected: compile failure — `res.DNS` undefined.

- [ ] **Step 3: Add the collector**

In `internal/collect/collect.go`, add next to `ControlPlaneReadyz` (imports `fmt`, `context`, `kubernetes` already present; add `dnshealth` is NOT needed — the collector returns raw bytes+code):

```go
// CoreDNSMetrics fetches a CoreDNS pod's :9153/metrics via the pods/proxy
// subresource, returning the raw body and HTTP status code. Never returns an error
// (non-fatal, like KubeletHealthz). Needs the pods/proxy get grant; a 401/403 is
// surfaced to the caller via the code.
func CoreDNSMetrics(ctx context.Context, client kubernetes.Interface, namespace, pod string) ([]byte, int) {
	var code int
	body, _ := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:9153/proxy/metrics", namespace, pod)).
		Do(ctx).StatusCode(&code).Raw()
	return body, code
}
```

- [ ] **Step 4: Wire into scan**

In `internal/scan/scan.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/dnshealth"` (alphabetical — after `diskusage`, before `hpahealth`; verify `gofmt -l` clean).
2. Add to `Options` (next to `ControlPlaneHealth`):

```go
	DNSHealth        bool
	DNSServfailRatio float64
```

3. Add to `Result` (next to `ControlPlane`):

```go
	DNS dnshealth.Report
```

4. Add the CoreDNS-pod helper (near the bottom of the file, next to other unexported helpers):

```go
// coreDNSPods returns the Running CoreDNS pods (kube-system, k8s-app=kube-dns).
func coreDNSPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Namespace == "kube-system" && p.Labels["k8s-app"] == "kube-dns" && p.Status.Phase == corev1.PodRunning {
			out = append(out, p)
		}
	}
	return out
}
```

5. In `Evaluate`, near the control-plane block, add the gated probe:

```go
	var dnsReport dnshealth.Report
	if opts.DNSHealth {
		ratio := opts.DNSServfailRatio
		if ratio <= 0 || ratio > 1 {
			ratio = 0.05
		}
		cdns := coreDNSPods(inputs.Pods)
		agg := map[string]int64{}
		forbidden, unreachable := 0, 0
		for _, p := range cdns {
			body, code := collect.CoreDNSMetrics(ctx, client, p.Namespace, p.Name)
			switch {
			case code == 401 || code == 403:
				forbidden++
			case code == 200:
				for rc, n := range dnshealth.ParseResponses(body) {
					agg[rc] += n
				}
			default:
				unreachable++
			}
		}
		dnsReport = dnshealth.Assess(agg, len(cdns), forbidden, unreachable, ratio, 100)
	}
```

6. Add `DNS: dnsReport` to the returned `Result{…}` literal (next to `ControlPlane: controlPlane`).

- [ ] **Step 5: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ ./internal/scan/ 2>&1 | tail -5`
Expected: PASS (the new off-by-default test and all existing collect/scan tests).

- [ ] **Step 6: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/collect/ internal/scan/
git add internal/collect/collect.go internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(scan): probe CoreDNS /metrics when --dns-health is set"
```

---

### Task 3: Report — the `DNS` section

**Files:**
- Modify: `internal/report/report.go` (`Input.DNS`, JSON struct + build literal, `dnsRenders`, `printDNSHealth`, call site, empty-cluster guard)
- Test: `internal/report/report_test.go` (`TestPrintDNSHealth`)

**Interfaces:**
- Consumes: `dnshealth.Report` (Task 1).
- Produces: `report.Input.DNS *dnshealth.Report` (set by Task 4's extras block); the rendered `DNS` section.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (import `"github.com/imantaba/kubeagent/internal/dnshealth"`):

```go
func TestPrintDNSHealth(t *testing.T) {
	degraded := &dnshealth.Report{Status: "degraded", ServfailRatio: 0.123, ErrorResponses: 1234, TotalResponses: 10000, PodsProbed: 2}
	var b bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, DNS: degraded}, "text", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "DNS") || !strings.Contains(out, "cluster DNS is failing to resolve") {
		t.Errorf("missing DNS section:\n%s", out)
	}
	if !strings.Contains(out, "12.3%") || !strings.Contains(out, "1234/10000 responses across 2 pods") {
		t.Errorf("missing ratio detail:\n%s", out)
	}

	// forbidden → grant hint
	var bf bytes.Buffer
	if err := PrintInventory(Input{Result: inventory.Result{}, DNS: &dnshealth.Report{Status: "forbidden"}}, "text", &bf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bf.String(), "pods/proxy") {
		t.Errorf("forbidden should print a pods/proxy grant hint:\n%s", bf.String())
	}

	// ok / unreachable / nil → nothing
	for _, p := range []*dnshealth.Report{{Status: "ok"}, {Status: "unreachable"}, nil} {
		var bo bytes.Buffer
		if err := PrintInventory(Input{Result: inventory.Result{}, DNS: p}, "text", &bo); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bo.String(), "cluster DNS") {
			t.Errorf("probe %+v should render no DNS finding:\n%s", p, bo.String())
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintDNSHealth 2>&1 | head`
Expected: FAIL — `Input` has no `DNS` field.

- [ ] **Step 3: Add the field, renderer, and wiring**

In `internal/report/report.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/dnshealth"` (alphabetical, after `diskusage`).
2. Add to the JSON `inventoryReport` struct next to `ControlPlane` (≈ line 50):

```go
	DNS                *dnshealth.Report           `json:"dns,omitempty"`
```

3. Add to the `Input` struct next to `ControlPlane` (≈ line 77):

```go
	DNS                *dnshealth.Report
```

4. Add to the Input→inventoryReport JSON-build literal next to `ControlPlane: in.ControlPlane` (≈ line 107):

```go
			DNS:                in.DNS,
```

5. Add the renderer + helper next to `printControlPlane`:

```go
// dnsRenders reports whether the DNS section would print.
func dnsRenders(p *dnshealth.Report) bool {
	return p != nil && (p.Status == "degraded" || p.Status == "forbidden")
}

// printDNSHealth renders the advisory DNS section: an elevated CoreDNS SERVFAIL+
// REFUSED response ratio (or a grant hint when forbidden).
func printDNSHealth(p *dnshealth.Report, w io.Writer) error {
	if !dnsRenders(p) {
		return nil
	}
	if _, err := fmt.Fprintln(w, "DNS  (opt-in)"); err != nil {
		return err
	}
	switch p.Status {
	case "degraded":
		if _, err := fmt.Fprintln(w, "  ✗ cluster DNS is failing to resolve"); err != nil {
			return err
		}
		pct := float64(int64(p.ServfailRatio*1000+0.5)) / 10
		if _, err := fmt.Fprintf(w, "      ⚠ CoreDNS SERVFAIL+REFUSED ratio %.1f%% (%d/%d responses across %d pods)\n",
			pct, p.ErrorResponses, p.TotalResponses, p.PodsProbed); err != nil {
			return err
		}
	case "forbidden":
		if _, err := fmt.Fprintln(w, "  ⚠ CoreDNS /metrics forbidden — grant pods/proxy to enable this check"); err != nil {
			return err
		}
	}
	return nil
}
```

6. Add the call site + gate next to the control-plane block (≈ line 193, after `printControlPlane`):

```go
	hasDNS := dnsRenders(in.DNS)
	if err := printDNSHealth(in.DNS, w); err != nil {
		return err
	}
```

7. Add `!hasDNS` to the "No issues found" empty-cluster guard (the `if !hasAttention && … && !hasControlPlane && !hasCerts && …` line):

```go
	if !hasAttention && !hasSecurity && !hasKubeletHealth && !hasControlPlane && !hasDNS && !hasCerts && in.Cluster.Verdict == "Healthy" {
```

- [ ] **Step 4: Run the report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -5`
Expected: PASS, including the unchanged `TestGoldenScanOutput` (the golden fixture sets no `DNS`).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/report/
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render the opt-in DNS section"
```

---

### Task 4: `main.go` — flag, env, extras-block mapping

**Files:**
- Modify: `main.go` (usage string, `--dns-health` flag, CLI `Options`, extras-block pointer mapping, `watch.Config` env; and one `watch.Config` struct field addition per the ordering note)
- Modify: `internal/watch/watch.go` (add the two `Config` fields so main.go compiles — consumed in Task 5)
- Test: `main_test.go` (`TestRun_DNSHealthFlagAccepted`)

**Interfaces:**
- Consumes: `scan.Options.DNSHealth`+`DNSServfailRatio` (Task 2), `scan.Result.DNS` (Task 2), `report.Input.DNS` (Task 3).
- Produces: end-to-end CLI rendering when `--dns-health` is set.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestRun_DNSHealthFlagAccepted(t *testing.T) {
	err := run([]string{"scan", "--dns-health", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("want unknown-output-format error (proving the flag parsed), got %v", err)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_DNSHealthFlagAccepted 2>&1 | head`
Expected: FAIL — `flag provided but not defined: -dns-health`.

- [ ] **Step 3: Add the flag, mapping, env, and the watch.Config fields**

In `internal/watch/watch.go`, add to the `Config` struct (next to `ControlPlaneHealth`):

```go
	DNSHealth        bool
	DNSServfailRatio float64
```

In `main.go`:

1. Add `[--dns-health]` to the usage string (next to `[--control-plane-health]`).
2. Add the flag near `controlPlaneHealth`:

```go
	dnsHealth := fs.Bool("dns-health", false, "probe CoreDNS /metrics and flag an elevated SERVFAIL+REFUSED response ratio (needs the pods/proxy grant)")
```

3. In the CLI `scan.Options{…}` literal, next to `ControlPlaneHealth: *controlPlaneHealth`:

```go
		DNSHealth:              *dnsHealth,
		DNSServfailRatio:       envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
```

4. In the runScan extras block, mirror the `cpRep` pointer pattern:

```go
	var dnsRep *dnshealth.Report
	if *dnsHealth {
		dnsRep = &res.DNS
	}
```

and, next to `in.ControlPlane = cpRep`:

```go
	in.DNS = dnsRep
```

5. In the `watch.Config{…}` literal (daemon path), next to `ControlPlaneHealth: envBool("KUBEAGENT_CONTROL_PLANE_HEALTH", false)`:

```go
		DNSHealth:              envBool("KUBEAGENT_DNS_HEALTH", false),
		DNSServfailRatio:       envFloat("KUBEAGENT_DNS_SERVFAIL_RATIO", 0.05),
```

6. Add the import `"github.com/imantaba/kubeagent/internal/dnshealth"` to `main.go` (alphabetical).

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . 2>&1 | tail -5`
Expected: build succeeds and the `main` tests PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w main.go internal/watch/watch.go
git add main.go main_test.go internal/watch/watch.go
git commit -m "feat(main): add --dns-health flag, mapping, and env"
```

---

### Task 5: `watch` daemon — config plumbing + gauge

**Files:**
- Modify: `internal/watch/watch.go` (pass `DNSHealth`+`DNSServfailRatio` into the daemon's `scan.Options`)
- Modify: `internal/watch/metrics.go` (a `dnsServfailRatio float64` field, its snapshot assignment, the gauge line)
- Test: `internal/watch/metrics_test.go` (extend the render assertion)

**Interfaces:**
- Consumes: `scan.Result.DNS` (Task 2), `watch.Config.DNSHealth`+`DNSServfailRatio` (Task 4).
- Produces: the `kubeagent_dns_servfail_ratio` gauge.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, add to the test's `scan.Result` fixture:

```go
		DNS: dnshealth.Report{Status: "degraded", ServfailRatio: 0.12},
```

and add `"kubeagent_dns_servfail_ratio 0.12"` to the asserted-substrings slice. Import `"github.com/imantaba/kubeagent/internal/dnshealth"`.

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult 2>&1 | tail`
Expected: FAIL — the rendered metrics lack `kubeagent_dns_servfail_ratio 0.12`.

- [ ] **Step 3: Add the daemon plumbing + gauge**

1. In `internal/watch/watch.go`, add `DNSHealth: cfg.DNSHealth, DNSServfailRatio: cfg.DNSServfailRatio` to the `scan.Options{…}` literal (next to `ControlPlaneHealth: cfg.ControlPlaneHealth`).

2. In `internal/watch/metrics.go`:
   - Add a struct field next to `controlPlaneUnhealthy`:

```go
	dnsServfailRatio float64
```

   - In the snapshot/update function, next to `m.controlPlaneUnhealthy = …`:

```go
	m.dnsServfailRatio = res.DNS.ServfailRatio
```

   - In the render function, next to the `kubeagent_control_plane_unhealthy` gauge line:

```go
	gauge("kubeagent_dns_servfail_ratio", "CoreDNS SERVFAIL+REFUSED response ratio (0 when healthy or not probed)", m.dnsServfailRatio)
```

- [ ] **Step 4: Run the watch suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ . 2>&1 | tail -3`
Expected: PASS (watch + main).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/watch/
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_dns_servfail_ratio gauge"
```

---

### Task 6: RBAC + Helm add-on

**Files:**
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (conditional `pods/proxy` grant)
- Modify: `deploy/helm/kubeagent/values.yaml` (`dnsHealth.enabled`)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (env when enabled)
- Create: `deploy/rbac-dnshealth.yaml` (raw add-on)

**Interfaces:** none. Enables the `--dns-health` probe against a real cluster.

- [ ] **Step 1: Add the conditional grant to the Helm ClusterRole**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, add a conditional block after the existing `nodes/proxy` block (which is gated by `diskUsage`/`kubeletHealth`), before the `certs` block:

```yaml
  {{- if .Values.dnsHealth.enabled }}
  - apiGroups: [""]
    resources: [pods/proxy]
    verbs: [get]
  {{- end }}
```

- [ ] **Step 2: Add the values toggle**

In `deploy/helm/kubeagent/values.yaml`, next to `controlPlaneHealth:`:

```yaml
dnsHealth:
  enabled: false
```

- [ ] **Step 3: Add the deployment env**

In `deploy/helm/kubeagent/templates/deployment.yaml`:

- Add `.Values.dnsHealth.enabled` to the `{{- if or … }}` env-block guard:

```yaml
          {{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled .Values.certs.enabled .Values.controlPlaneHealth.enabled .Values.dnsHealth.enabled }}
```

- Add the env entry next to the `KUBEAGENT_CONTROL_PLANE_HEALTH` block:

```yaml
            {{- if .Values.dnsHealth.enabled }}
            - name: KUBEAGENT_DNS_HEALTH
              value: "true"
            {{- end }}
```

- [ ] **Step 4: Create the raw-manifest add-on**

Create `deploy/rbac-dnshealth.yaml` (mirror `deploy/rbac-controlplane.yaml`'s structure; use the SAME ServiceAccount name+namespace as its subjects block — read that file first):

```yaml
# Optional add-on grant for `kubeagent scan --dns-health` (and the daemon with
# KUBEAGENT_DNS_HEALTH=true): read each CoreDNS pod's :9153/metrics via the
# pods/proxy subresource to flag an elevated SERVFAIL+REFUSED response ratio.
# Strictly read-only. Apply alongside deploy/rbac.yaml when the dns-health check is used.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-dnshealth
rules:
  - apiGroups: [""]
    resources: [pods/proxy]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-dnshealth
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-dnshealth
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 5: Verify the chart templates + lint**

```bash
export PATH=$PATH:$HOME/.local/bin
# default: pods/proxy grant ABSENT
helm template x deploy/helm/kubeagent | grep -c 'pods/proxy' | grep -qx 0 && echo "absent by default OK"
# enabled: grant + env PRESENT
helm template x deploy/helm/kubeagent --set dnsHealth.enabled=true | grep -q 'pods/proxy' && echo "grant present OK"
helm template x deploy/helm/kubeagent --set dnsHealth.enabled=true | grep -q 'KUBEAGENT_DNS_HEALTH' && echo "env present OK"
helm lint deploy/helm/kubeagent 2>&1 | tail -2
```
Expected: absent-by-default; grant+env present when enabled; lint 0 failures. (`pods/proxy` does not appear anywhere else in the chart, so the default `grep -c` is genuinely 0.)

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/kubeagent/templates/clusterrole.yaml deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/rbac-dnshealth.yaml
git commit -m "feat(deploy): conditional pods/proxy grant for --dns-health"
```

---

### Task 7: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- `website/docs/features/diagnostics.md`: add a section near the other opt-in probes (`--control-plane-health`, `--kubelet-health`) describing `--dns-health`: probes each CoreDNS pod's `:9153/metrics` and flags a SERVFAIL+REFUSED response ratio ≥ 5% (env `KUBEAGENT_DNS_SERVFAIL_RATIO`) over a 100-response floor; catches "DNS up but failing to resolve"; opt-in, read-only; needs the `pods/proxy` grant (Helm `dnsHealth.enabled=true` or `deploy/rbac-dnshealth.yaml`); the daemon gauge `kubeagent_dns_servfail_ratio`; advisory. Show the example:

  ```text
  DNS  (opt-in)
    ✗ cluster DNS is failing to resolve
        ⚠ CoreDNS SERVFAIL+REFUSED ratio 12.3% (1234/10000 responses across 2 pods)
  ```

- `README.md`: add `--dns-health` to the opt-in flags list, noting the `pods/proxy` grant and the gauge.

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`:

  ```
  - **DNS / CoreDNS resolution health (`--dns-health`).** An opt-in probe of each
    CoreDNS pod's `:9153/metrics` flags an elevated SERVFAIL+REFUSED response ratio
    (default ≥ 5% over a 100-response floor; env `KUBEAGENT_DNS_SERVFAIL_RATIO`) —
    catching DNS that is up but failing to resolve, which the CoreDNS-pod health
    check misses. Read-only; needs the `pods/proxy` add-on grant; the daemon exposes
    `kubeagent_dns_servfail_ratio`. Second of the Theme-B control-plane closers.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the control-plane-health entry, tagged **Theme-B**, noting it closes the DNS-resolution-health gap (up-but-failing) and links to `features/diagnostics.md`.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the --dns-health check"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches `internal/collect`, the **RBAC** manifests + Helm **templates**, and `internal/watch` → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`), plus a live smoke: on a healthy Kind cluster `scan --dns-health` prints nothing (low SERVFAIL); the parse/assess path is unit-covered. **Minor** bump **v0.45.0 → v0.46.0**; **chart MINOR** bump — clusterrole/values/deployment templates changed, so override the bump script's default patch to the next minor (0.16.0 → 0.17.0). Hold for the user's explicit "run release and push".
```
