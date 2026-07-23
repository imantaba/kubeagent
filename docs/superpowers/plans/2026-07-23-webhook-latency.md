# Admission-webhook latency risk Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag a Fail-policy admission webhook whose `timeoutSeconds` is ≥ 15 (a latency landmine) by extending the existing `internal/webhookhealth` check, with a `WebhookSlow` render label and a partitioned daemon gauge.

**Architecture:** Extend `webhookhealth.Assess` (add a `timeoutThreshold` param + a latency branch on the normalized `hook`), thread an env-tunable threshold through `scan.Options`/`main`/`watch.Config`, add a `WebhookSlow` label branch in the report, partition the `kubeagent_admission_webhooks_failing` gauge and add `kubeagent_admission_webhook_latency_risks`, and expose the threshold via a Helm value.

**Tech Stack:** Go 1.26, `k8s.io/api/admissionregistration/v1` (`TimeoutSeconds *int32`), pure fake-object tests + fake clientset.

## Global Constraints

- **Read-only; always-on; no new flag/collector/RBAC/package.** Webhook configs + Services/EndpointSlices are already collected. Env-only tuning knob.
- **Threshold:** default **15**, env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` (value ≤ 0 → 15). Comparison is `timeoutSeconds >= threshold` (inclusive).
- **Scope:** only `failurePolicy: Fail` (or nil → Fail). Ignore-policy webhooks are NOT flagged. Nil `timeoutSeconds` is NOT flagged.
- **No double-report:** a webhook already flagged for its backend (missing-service / no-endpoints) is NOT also flagged for latency. URL-based Fail webhooks (no Service) ARE latency-checked.
- **Advisory** — no cluster-verdict change.
- **Issue Problem string** is exactly `high-timeout`. **Render label** is `WebhookSlow`.
- The existing `kubeagent_admission_webhooks_failing` gauge keeps its backend-only meaning (partition excludes `high-timeout`); new gauge `kubeagent_admission_webhook_latency_risks`.
- Gate: touches `internal/watch` + a Helm template → **FULL CHAOS GATE**. **Minor** bump v0.46.0 → **v0.47.0**; **chart MINOR** bump.
- **No `Co-Authored-By: Claude` trailer** on any commit. **TDD.** gofmt-clean.

---

### Task 1: `webhookhealth` latency branch + scan wiring

**Files:**
- Modify: `internal/webhookhealth/webhookhealth.go` (`hook.timeout`, `Assess` param + latency branch, doc comment)
- Modify: `internal/webhookhealth/webhookhealth_test.go` (update existing `Assess` calls to the new 5-arg signature; add latency tests + helpers)
- Modify: `internal/scan/scan.go` (`Options.WebhookTimeoutThreshold`, clamped `Assess` call)
- Test: `internal/scan/scan_test.go` (`TestEvaluate_FlagsSlowWebhook`)

**Interfaces:**
- Consumes: `admissionv1.ValidatingWebhook`/`MutatingWebhook` (`.TimeoutSeconds *int32`, `.FailurePolicy`, `.ClientConfig.Service`); the existing `webhookhealth.Issue` (fields `Kind`, `Config`, `Webhook`, `Service`, `Problem`, `Reason`).
- Produces: `webhookhealth.Assess(validating, mutating, services, slices, timeoutThreshold int32) []Issue` (new 5th param); `scan.Options.WebhookTimeoutThreshold int32`; a `high-timeout` `Issue.Problem`. Used by Tasks 2 (report), 4 (watch), 6 (golden).

- [ ] **Step 1: Write the failing tests**

In `internal/webhookhealth/webhookhealth_test.go`, add helpers near the top (after the existing `svcRef` helper):

```go
func i32(n int32) *int32 { return &n }

// vhookT / mhookT build a webhook with a timeoutSeconds set.
func vhookT(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig, timeout int32) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, TimeoutSeconds: i32(timeout)}
}
func mhookT(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig, timeout int32) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, TimeoutSeconds: i32(timeout)}
}
```

Add the latency tests:

```go
func TestAssess_HighTimeoutFlagged(t *testing.T) {
	v := vwc("slow-validator", vhookT("policy.example.com", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("kube-system", "policy-svc")}, 30))
	services := []corev1.Service{svc("kube-system", "policy-svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("kube-system", "policy-svc", true)} // healthy backend

	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "policy.example.com")
	if !ok {
		t.Fatal("want a high-timeout issue for a healthy 30s Fail webhook")
	}
	if is.Problem != "high-timeout" {
		t.Errorf("Problem = %q, want high-timeout", is.Problem)
	}
	if !strings.Contains(is.Reason, "timeoutSeconds 30") || !strings.Contains(is.Reason, "≥ 15s") {
		t.Errorf("Reason = %q", is.Reason)
	}
}

func TestAssess_TimeoutAtThresholdFlagged(t *testing.T) {
	v := vwc("edge", vhookT("edge.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 15))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "edge.io"); !ok {
		t.Error("timeoutSeconds == threshold (15) should be flagged (inclusive)")
	}
}

func TestAssess_TimeoutBelowThresholdNotFlagged(t *testing.T) {
	v := vwc("ok", vhookT("ok.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 14))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "ok.io"); ok {
		t.Error("timeoutSeconds 14 < 15 should not be flagged")
	}
}

func TestAssess_IgnorePolicyHighTimeoutNotFlagged(t *testing.T) {
	v := vwc("lax", vhookT("lax.io", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "lax.io"); ok {
		t.Error("Ignore-policy webhook must not be latency-flagged")
	}
}

func TestAssess_NilTimeoutNotFlagged(t *testing.T) {
	// vhook (no timeout) → TimeoutSeconds nil
	v := vwc("nilto", vhook("nilto.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "nilto.io"); ok {
		t.Error("nil timeoutSeconds must not be flagged")
	}
}

func TestAssess_BackendDownHighTimeoutNoDoubleReport(t *testing.T) {
	v := vwc("down", vhookT("down.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, 30))
	// no Service "gone" collected → missing-service
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15)
	if len(got) != 1 {
		t.Fatalf("want exactly one issue (backend, not doubled), got %+v", got)
	}
	if got[0].Problem != "missing-service" {
		t.Errorf("Problem = %q, want missing-service (backend wins)", got[0].Problem)
	}
}

func TestAssess_URLWebhookHighTimeoutFlagged(t *testing.T) {
	u := "https://hook.example.com/validate"
	v := vwc("urlhook", vhookT("url.io", failP(), admissionv1.WebhookClientConfig{URL: &u}, 30))
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "url.io")
	if !ok {
		t.Fatal("URL-based Fail webhook with high timeout should be flagged")
	}
	if is.Problem != "high-timeout" || is.Service != "" {
		t.Errorf("issue = %+v, want high-timeout with empty Service", is)
	}
}

func TestAssess_MutatingHighTimeoutFlagged(t *testing.T) {
	m := mwc("slow-mutator", mhookT("mut.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess(nil, []admissionv1.MutatingWebhookConfiguration{m}, services, slices, 15), "mut.io"); !ok {
		t.Error("mutating high-timeout webhook should be flagged")
	}
}

func TestAssess_ThresholdRespected(t *testing.T) {
	v := vwc("t", vhookT("t.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 31), "t.io"); ok {
		t.Error("threshold 31 should not flag a 30s webhook")
	}
}
```

Add `"strings"` to the test file's imports (for the Reason substring checks) if not present.

**Also update every EXISTING `Assess(...)` call in this test file** to add the 5th arg `, 15` (the existing tests pass 4 args; find each `Assess(` call and append `, 15` before the closing paren).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/webhookhealth/ 2>&1 | head`
Expected: compile failure — `Assess` still takes 4 params / `too many arguments`.

- [ ] **Step 3: Extend `webhookhealth.go`**

In `internal/webhookhealth/webhookhealth.go`:

1. Add `timeout *int32` to the `hook` struct (last field).
2. Update both hook-building literals to pass the timeout:

```go
	for _, c := range validating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"ValidatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service, w.TimeoutSeconds})
		}
	}
	for _, c := range mutating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"MutatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service, w.TimeoutSeconds})
		}
	}
```

3. Change the `Assess` signature to add `timeoutThreshold int32` as the last parameter.

4. Replace the issue-building loop body with the restructured version (URL webhooks reach the latency check; no double-report):

```go
	var out []Issue
	for _, h := range hooks {
		if !failsClosed(h.fp) {
			continue // Ignore policy
		}
		backendFlagged := false
		if h.service != nil {
			id := h.service.Namespace + "/" + h.service.Name
			svc, found := findService(services, h.service.Namespace, h.service.Name)
			switch {
			case !found:
				out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "missing-service",
					Reason: fmt.Sprintf("backend Service %s does not exist — failurePolicy Fail rejects every intercepted create/update", id)})
				backendFlagged = true
			case svchealth.ReadyEndpoints(svc, slices) == 0:
				out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "no-endpoints",
					Reason: fmt.Sprintf("backend Service %s has no ready endpoints — failurePolicy Fail rejects every intercepted create/update", id)})
				backendFlagged = true
			}
		}
		if !backendFlagged && h.timeout != nil && *h.timeout >= timeoutThreshold {
			id := ""
			if h.service != nil {
				id = h.service.Namespace + "/" + h.service.Name
			}
			out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "high-timeout",
				Reason: fmt.Sprintf("timeoutSeconds %d ≥ %ds under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to %ds, then rejects it", *h.timeout, timeoutThreshold, *h.timeout)})
		}
	}
```

(The `sort.Slice` by (Kind, Config, Webhook) after the loop is unchanged. Update the package doc comment's first sentence to mention "or has a high timeoutSeconds (a latency landmine)".)

- [ ] **Step 4: Update the scan call site**

In `internal/scan/scan.go`:

1. Add `WebhookTimeoutThreshold int32` to the `Options` struct (near the other webhook-adjacent options; anywhere in the struct is fine).
2. Replace the `webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices)` line (≈ line 235) with:

```go
		webhookThreshold := opts.WebhookTimeoutThreshold
		if webhookThreshold <= 0 {
			webhookThreshold = 15
		}
		webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices, webhookThreshold)
```

- [ ] **Step 5: Write the scan integration test**

Add to `internal/scan/scan_test.go` (add imports `admissionv1 "k8s.io/api/admissionregistration/v1"` and ensure `p32` — the existing `*int32` helper — is available; if a Fail-policy pointer helper is needed, inline it):

```go
func TestEvaluate_FlagsSlowWebhook(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	url := "https://hook.example.com/validate"
	fail := admissionv1.Fail
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "slow-validator"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:          "policy.example.com",
			FailurePolicy: &fail,
			ClientConfig:  admissionv1.WebhookClientConfig{URL: &url},
			TimeoutSeconds: p32(20),
		}},
	}
	cli := fake.NewSimpleClientset(node, vwc)
	res, err := Evaluate(context.Background(), cli, Options{}) // Namespace "" → webhook check runs; threshold 0 → 15
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range res.WebhookIssues {
		if is.Problem == "high-timeout" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a high-timeout webhook issue, got %+v", res.WebhookIssues)
	}
}
```

(`p32(20)` returns `*int32`; if the helper is named differently in scan_test, use that. If `admissionv1` is already imported, don't duplicate.)

- [ ] **Step 6: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/webhookhealth/ ./internal/scan/ 2>&1 | tail -5`
Expected: PASS (the new latency tests, the updated existing tests, the scan integration test).

- [ ] **Step 7: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/webhookhealth/ internal/scan/
git add internal/webhookhealth/ internal/scan/scan.go internal/scan/scan_test.go
git commit -m "feat(webhookhealth): flag Fail-policy webhooks with a high timeoutSeconds"
```

---

### Task 2: Report — the `WebhookSlow` label

**Files:**
- Modify: `internal/report/report.go` (`printWebhookIssues` label branch)
- Test: `internal/report/report_test.go` (`TestPrintWebhookIssues_LatencyLabel`)

**Interfaces:**
- Consumes: `webhookhealth.Issue` with `Problem == "high-timeout"` (Task 1).
- Produces: the `WebhookSlow` render label.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestPrintWebhookIssues_LatencyLabel(t *testing.T) {
	in := Input{Result: inventory.Result{}, WebhookIssues: []webhookhealth.Issue{
		{Kind: "ValidatingWebhookConfiguration", Config: "slow-validator", Webhook: "policy.example.com", Problem: "high-timeout", Reason: "timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it"},
		{Kind: "ValidatingWebhookConfiguration", Config: "down-validator", Webhook: "down.io", Service: "ns/svc", Problem: "no-endpoints", Reason: "backend Service ns/svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update"},
	}}
	var b bytes.Buffer
	if err := PrintInventory(in, "text", &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "⚠ WebhookSlow: timeoutSeconds 30 ≥ 15s") {
		t.Errorf("missing WebhookSlow line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ WebhookDown: backend Service ns/svc has no ready endpoints") {
		t.Errorf("backend issue should still render WebhookDown:\n%s", out)
	}
}
```

(Ensure `webhookhealth` is imported in `report_test.go` — it likely already is from the existing webhook tests.)

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintWebhookIssues_LatencyLabel 2>&1 | head`
Expected: FAIL — the high-timeout issue currently renders `WebhookDown:` (the assertion for `WebhookSlow` fails).

- [ ] **Step 3: Add the label branch**

In `internal/report/report.go`, replace the body of `printWebhookIssues`'s inner Reason line with the branched label:

```go
func printWebhookIssues(issues []webhookhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  webhook %s\n", is.Config, is.Kind, is.Webhook); err != nil {
			return err
		}
		label := "WebhookDown"
		if is.Problem == "high-timeout" {
			label = "WebhookSlow"
		}
		if _, err := fmt.Fprintf(w, "      ⚠ %s: %s\n", label, is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -5`
Expected: PASS, including `TestGoldenScanOutput` (the existing golden webhook fixture is a backend issue → still `WebhookDown` → snapshot unchanged).

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/report/
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): label a high-timeout webhook issue WebhookSlow"
```

---

### Task 3: `main.go` — the threshold env

**Files:**
- Modify: `main.go` (CLI `scan.Options` + `watch.Config` env)
- Modify: `internal/watch/watch.go` (add `Config.WebhookTimeoutThreshold int32`)
- Test: `main_test.go` (`TestResultInput_or_env` style — see below)

**Interfaces:**
- Consumes: `scan.Options.WebhookTimeoutThreshold` (Task 1).
- Produces: the env-driven threshold for the CLI and daemon.

- [ ] **Step 1: Add the env wiring + the watch.Config field**

In `internal/watch/watch.go`, add to the `Config` struct (next to the other threshold fields):

```go
	WebhookTimeoutThreshold int32
```

In `main.go`:

1. In the CLI `scan.Options{…}` literal, add:

```go
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
```

2. In the `watch.Config{…}` literal, add:

```go
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
```

(`envInt(name string, def int) int` already exists — used for `KUBEAGENT_CERT_WARN_DAYS`. Cast its result to `int32`.)

- [ ] **Step 2: Write the failing test**

Add to `main_test.go`:

```go
func TestEnvInt_WebhookTimeoutDefault(t *testing.T) {
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "")
	if got := envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15); got != 15 {
		t.Errorf("unset env should default to 15, got %d", got)
	}
	t.Setenv("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", "25")
	if got := envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15); got != 25 {
		t.Errorf("env override should be 25, got %d", got)
	}
}
```

- [ ] **Step 3: Run it to verify it passes (after wiring compiles)**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . -run TestEnvInt_WebhookTimeoutDefault 2>&1 | tail`
Expected: build succeeds (the `watch.Config` field makes both `main.go` literals compile) and the test PASSES. (This test exercises the `envInt` helper directly — the wiring's correctness is that `go build` compiles and the scan integration test in Task 1 already proves the default-15 behavior end-to-end.)

- [ ] **Step 4: Run the main + watch build**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w main.go internal/watch/watch.go
git add main.go main_test.go internal/watch/watch.go
git commit -m "feat(main): read KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS for the webhook latency threshold"
```

---

### Task 4: `watch` — partition the gauge

**Files:**
- Modify: `internal/watch/watch.go` (pass `WebhookTimeoutThreshold` into the daemon's `scan.Options`)
- Modify: `internal/watch/metrics.go` (partition `webhooksFailing`, add `webhookLatencyRisks` + gauge)
- Test: `internal/watch/metrics_test.go` (extend the render assertion)

**Interfaces:**
- Consumes: `scan.Result.WebhookIssues` (with `Problem`), `watch.Config.WebhookTimeoutThreshold` (Task 3).
- Produces: the `kubeagent_admission_webhook_latency_risks` gauge; `kubeagent_admission_webhooks_failing` now counts backend issues only.

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, ensure the `scan.Result` fixture's `WebhookIssues` contains ONE backend issue (Problem `no-endpoints` — likely already present) AND add one high-timeout issue:

```go
		WebhookIssues: []webhookhealth.Issue{
			{Kind: "ValidatingWebhookConfiguration", Config: "down", Webhook: "d.io", Problem: "no-endpoints", Reason: "..."},
			{Kind: "ValidatingWebhookConfiguration", Config: "slow", Webhook: "s.io", Problem: "high-timeout", Reason: "..."},
		},
```

(Match the fixture's existing `WebhookIssues` shape; if it already has exactly one `no-endpoints` issue, just add the `high-timeout` one.) Keep the existing assertion `"kubeagent_admission_webhooks_failing 1"` (still 1 backend issue) and add `"kubeagent_admission_webhook_latency_risks 1"`. Ensure `webhookhealth` is imported in the test file.

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult 2>&1 | tail`
Expected: FAIL — `kubeagent_admission_webhook_latency_risks 1` absent, and (if the fixture now has 2 webhook issues) `kubeagent_admission_webhooks_failing` would be 2 without the partition.

- [ ] **Step 3: Add the daemon threshold link + partition the gauge**

1. In `internal/watch/watch.go`, add `WebhookTimeoutThreshold: cfg.WebhookTimeoutThreshold` to the `scan.Options{…}` literal (next to the other threshold fields).

2. In `internal/watch/metrics.go`:
   - Add a struct field next to `webhooksFailing`:

```go
	webhookLatencyRisks int
```

   - Replace `m.webhooksFailing = len(res.WebhookIssues)` in the snapshot/update function with the partition:

```go
	m.webhooksFailing = 0
	m.webhookLatencyRisks = 0
	for _, i := range res.WebhookIssues {
		if i.Problem == "high-timeout" {
			m.webhookLatencyRisks++
		} else {
			m.webhooksFailing++
		}
	}
```

   - In the render function, next to the `kubeagent_admission_webhooks_failing` gauge line:

```go
	gauge("kubeagent_admission_webhook_latency_risks", "Fail-policy admission webhooks with a high timeoutSeconds (a latency landmine)", float64(m.webhookLatencyRisks))
```

- [ ] **Step 4: Run the watch suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ . 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: gofmt + commit**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -w internal/watch/
git add internal/watch/watch.go internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): partition the webhook gauge; add kubeagent_admission_webhook_latency_risks"
```

---

### Task 5: Helm — the threshold value

**Files:**
- Modify: `deploy/helm/kubeagent/values.yaml` (`webhookLatency.timeoutThreshold`)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (unconditional threshold env)

**Interfaces:** none. Lets Helm daemon users tune the threshold.

- [ ] **Step 1: Add the values key**

In `deploy/helm/kubeagent/values.yaml`, add (near the other feature blocks):

```yaml
webhookLatency:
  timeoutThreshold: 15
```

- [ ] **Step 2: Make the env unconditional and add the threshold entry**

In `deploy/helm/kubeagent/templates/deployment.yaml`, the container `env:` is currently gated by `{{- if or .Values.diskUsage.enabled … .Values.dnsHealth.enabled }}`. The webhook-latency check is always-on, so `env:` must always render. Change the guarded `env:` to an unconditional `env:` with the threshold as its first entry:

Replace:

```yaml
          {{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled .Values.certs.enabled .Values.controlPlaneHealth.enabled .Values.dnsHealth.enabled }}
          env:
```

with:

```yaml
          env:
            - name: KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS
              value: {{ .Values.webhookLatency.timeoutThreshold | quote }}
```

and remove the now-orphaned `{{- end }}` that previously closed the `{{- if or … }}` guard (the one after the `certs` env block — the LAST `{{- end }}` of the env section, immediately before the `volumeMounts:`/`resources:`/next container key). The per-flag `{{- if .Values.X.enabled }}` blocks inside stay exactly as they are. Read the file carefully to remove the correct single `{{- end }}` so the YAML nesting stays valid.

- [ ] **Step 3: Verify the chart templates + lint**

```bash
export PATH=$PATH:$HOME/.local/bin
# env + threshold always render (even with NO opt-in features enabled):
helm template x deploy/helm/kubeagent | grep -q 'KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS' && echo "threshold env always present OK"
helm template x deploy/helm/kubeagent | grep -A1 'KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS' | grep -q '"15"' && echo "default 15 OK"
# an opt-in feature still renders too (nesting intact):
helm template x deploy/helm/kubeagent --set dnsHealth.enabled=true | grep -q 'KUBEAGENT_DNS_HEALTH' && echo "opt-in env still renders OK"
helm lint deploy/helm/kubeagent 2>&1 | tail -2
```
Expected: threshold env always present (value "15"), opt-in envs still render when enabled, lint 0 failures.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml
git commit -m "feat(deploy): expose the webhook latency threshold via Helm"
```

---

### Task 6: Golden snapshot — a `WebhookSlow` fixture

**Files:**
- Modify: `internal/report/golden_test.go` (add a high-timeout webhook Issue to the fixture)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerated via `-update`)

**Interfaces:**
- Consumes: the golden fixture builder's `WebhookIssues` list and `webhookhealth.Issue` (Task 1); the `WebhookSlow` label (Task 2).

- [ ] **Step 1: Add the fixture Issue**

In `internal/report/golden_test.go`, find where the fixture `Input`/`Result` sets `WebhookIssues` (search for `WebhookIssues`). Append a high-timeout Issue to that slice:

```go
		{Kind: "ValidatingWebhookConfiguration", Config: "slow-validator", Webhook: "policy.example.com", Problem: "high-timeout",
			Reason: "timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it"},
```

(If the golden fixture does not yet set `WebhookIssues`, add a `WebhookIssues: []webhookhealth.Issue{ … }` field to the fixture `Input` with this one entry, and import `webhookhealth` in `golden_test.go`.)

- [ ] **Step 2: Run the golden test to verify it fails (snapshot mismatch)**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestGoldenScanOutput 2>&1 | tail`
Expected: FAIL — the rendered output now includes the `✗ slow-validator … / ⚠ WebhookSlow: …` block absent from the committed snapshot.

- [ ] **Step 3: Regenerate the snapshot**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report -run TestGoldenScanOutput -update`
Expected: exit 0; `testdata/golden-scan.txt` now shows the `slow-validator` block with `⚠ WebhookSlow: timeoutSeconds 30 ≥ 15s …`.

- [ ] **Step 4: Run the full report suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ 2>&1 | tail -3`
Expected: PASS (`TestGoldenScanOutput` + `TestGoldenInputCoversAllSections`).

- [ ] **Step 5: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "test(report): cover a WebhookSlow issue in the golden scan snapshot"
```

---

### Task 7: Documentation

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update the docs**

- `website/docs/features/diagnostics.md`: in the existing admission-webhook section, add that kubeagent also flags a **Fail-policy webhook with a high `timeoutSeconds`** (≥ 15 by default; env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`, Helm `webhookLatency.timeoutThreshold`) as a latency landmine, rendered `WebhookSlow`; the daemon gauge `kubeagent_admission_webhook_latency_risks`; advisory; always-on. Show the example:

  ```text
  WEBHOOK
    ✗ slow-validator  ValidatingWebhookConfiguration  webhook policy.example.com
        ⚠ WebhookSlow: timeoutSeconds 30 ≥ 15s under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to 30s, then rejects it
  ```

- `README.md`: add the webhook latency-risk check to the always-on detector list (mention the `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` env / Helm `webhookLatency.timeoutThreshold`, the `WebhookSlow` label, and the gauge).

- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`:

  ```
  - **Admission-webhook latency risk.** `scan` flags a Fail-policy admission webhook
    whose `timeoutSeconds` is at or above 15 (env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`,
    Helm `webhookLatency.timeoutThreshold`) — a latency landmine that blocks every
    intercepted create/update for up to that long, then rejects it. Rendered
    `WebhookSlow`; the daemon exposes `kubeagent_admission_webhook_latency_risks`.
    Read-only, always-on, advisory. Closes the Theme-B admission-webhook line.
  ```

- `website/docs/roadmap.md`: add a Shipped bullet after the DNS-health entry, tagged **Theme-B**, noting it closes the admission-webhook line (failure + latency) and links to `features/diagnostics.md`.

- [ ] **Step 2: Verify the docs build**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (venv fallback: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, no page WARNINGs.

- [ ] **Step 3: Run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the admission-webhook latency risk check"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the `release` skill owns this. Touches `internal/watch` (gauge) + a Helm **template** (deployment/values) → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`), plus a live smoke: a Kind cluster with a Fail-policy ValidatingWebhookConfiguration whose `timeoutSeconds: 30`, then `scan` and confirm the `WebhookSlow` line (its backend Service should exist/be-healthy or use a URL config so the latency issue isn't pre-empted by a backend issue). **Minor** bump **v0.46.0 → v0.47.0**; **chart MINOR** bump — deployment/values templates changed, so override the bump script's default patch to the next minor (0.17.0 → 0.18.0). Hold for the user's explicit "run release and push".
```
