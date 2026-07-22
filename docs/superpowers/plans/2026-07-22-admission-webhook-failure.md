# Admission-webhook-failure check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag an admission webhook (`ValidatingWebhookConfiguration` / `MutatingWebhookConfiguration`) whose `failurePolicy` is `Fail` and whose backing Service is missing or has zero ready endpoints — it will reject every create/update it intercepts — in `scan`'s NEEDS ATTENTION output.

**Architecture:** A new pure leaf package `internal/webhookhealth` classifies each webhook entry from the collected webhook configs plus the already-collected Services/EndpointSlices, reusing `svchealth.ReadyEndpoints` for backend readiness. Two new cluster-scoped collectors feed it. `scan.Evaluate` runs it (cluster-wide scans only) into `Result.WebhookIssues`; `report` renders a two-line NEEDS ATTENTION block and an attention-line fragment; the `watch` daemon exposes a gauge. A new base `admissionregistration.k8s.io` RBAC grant is added to the manifest and Helm chart.

**Tech Stack:** Go 1.26, standard-library `flag`, client-go (`AdmissionregistrationV1`), `k8s.io/api/admissionregistration/v1`, `k8s.io/api/core/v1`, `k8s.io/api/discovery/v1`. Tests use client-go's fake clientset and fake objects.

## Global Constraints

- **READ-ONLY.** `List` only; never create/update/patch/delete. No LLM.
- **Always-on; no flag.** One new base RBAC grant (`admissionregistration.k8s.io`).
- **Advisory to the verdict** — `WebhookIssues` never flips Healthy/Degraded.
- **Cluster-wide only** — the check is skipped under `--namespace` (a namespace-scoped scan collects only that namespace's Services, so a backend elsewhere would look "missing"). Gate on `opts.Namespace == ""`.
- **Pure & deterministic** — `webhookhealth.Assess` reads only the passed objects; output sorted by (Kind, Config, Webhook); no clock, no cluster calls.
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement.
- Constants used in output: `✗` U+2717, `⚠` U+26A0, `—` em dash U+2014.
- The `scan.Result` → `report.Input` mapping MUST go through `resultInput()` in `main.go` (Task 5), not only the `scan.Result` literal — this is the wiring gap that hid the stuck-terminating feature from the CLI in v0.34.0.

---

### Task 1: `webhookhealth.Assess` — the pure classifier

**Files:**
- Create: `internal/webhookhealth/webhookhealth.go`
- Test: `internal/webhookhealth/webhookhealth_test.go`

**Interfaces:**
- Consumes: `svchealth.ReadyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int` (existing).
- Produces: `type Issue struct { Kind, Config, Webhook, Service, Problem, Reason string }` (JSON tags `kind`, `config`, `webhook`, `service`, `problem`, `reason`); `func Assess(validating []admissionv1.ValidatingWebhookConfiguration, mutating []admissionv1.MutatingWebhookConfiguration, services []corev1.Service, slices []discoveryv1.EndpointSlice) []Issue`.

- [ ] **Step 1: Write the failing test**

Create `internal/webhookhealth/webhookhealth_test.go`:

```go
package webhookhealth

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func failP() *admissionv1.FailurePolicyType  { f := admissionv1.Fail; return &f }
func ignoreP() *admissionv1.FailurePolicyType { f := admissionv1.Ignore; return &f }
func svcRef(ns, name string) *admissionv1.ServiceReference {
	return &admissionv1.ServiceReference{Namespace: ns, Name: name}
}

func vwc(name string, ws ...admissionv1.ValidatingWebhook) admissionv1.ValidatingWebhookConfiguration {
	return admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func mwc(name string, ws ...admissionv1.MutatingWebhook) admissionv1.MutatingWebhookConfiguration {
	return admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func vhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc}
}
func mhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc}
}

func svc(ns, name string) corev1.Service {
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}
func sliceFor(ns, svcName string, ready bool) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
}

func find(issues []Issue, webhook string) (Issue, bool) {
	for _, i := range issues {
		if i.Webhook == webhook {
			return i, true
		}
	}
	return Issue{}, false
}

func TestAssess_NoEndpoints(t *testing.T) {
	v := vwc("policy-webhook", vhook("validate.policy.io", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("kube-system", "policy-svc")}))
	services := []corev1.Service{svc("kube-system", "policy-svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("kube-system", "policy-svc", false)} // 0 ready
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices), "validate.policy.io")
	if !ok || is.Problem != "no-endpoints" {
		t.Fatalf("want no-endpoints, got %+v", is)
	}
	if is.Kind != "ValidatingWebhookConfiguration" || is.Config != "policy-webhook" || is.Service != "kube-system/policy-svc" {
		t.Errorf("wrong identity: %+v", is)
	}
	if is.Reason != "backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_MissingService(t *testing.T) {
	m := mwc("image-signing", mhook("sign.example.com", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("secure", "signer")}))
	is, ok := find(Assess(nil, []admissionv1.MutatingWebhookConfiguration{m}, nil, nil), "sign.example.com")
	if !ok || is.Problem != "missing-service" {
		t.Fatalf("want missing-service, got %+v", is)
	}
	if is.Kind != "MutatingWebhookConfiguration" {
		t.Errorf("kind = %q", is.Kind)
	}
	if is.Reason != "backend Service secure/signer does not exist — failurePolicy Fail rejects every intercepted create/update" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_NilFailurePolicyIsFail(t *testing.T) {
	// nil failurePolicy defaults to Fail in admissionregistration.k8s.io/v1.
	v := vwc("c", vhook("w", nil, admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil), "w"); !ok {
		t.Fatal("a nil-failurePolicy webhook with a down backend must be flagged")
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	services := []corev1.Service{svc("ns", "up")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "up", true)} // ready
	url := "https://external.example.com/hook"
	cases := []admissionv1.ValidatingWebhookConfiguration{
		vwc("ignore", vhook("ig", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")})), // Ignore → not blocking
		vwc("urlhook", vhook("u", failP(), admissionv1.WebhookClientConfig{URL: &url})),                        // URL → can't check
		vwc("healthy", vhook("h", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "up")})),      // ready backend
	}
	if got := Assess(cases, nil, services, slices); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_SortedAndPerWebhook(t *testing.T) {
	// two down webhooks in one config → two issues, sorted by webhook name.
	v := vwc("cfg",
		vhook("b-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}),
		vhook("a-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil)
	if len(got) != 2 || got[0].Webhook != "a-hook" || got[1].Webhook != "b-hook" {
		t.Fatalf("want two issues sorted by webhook, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/webhookhealth/`
Expected: FAIL — `undefined: Assess`.

- [ ] **Step 3: Write the implementation**

Create `internal/webhookhealth/webhookhealth.go`:

```go
// Package webhookhealth flags admission webhooks whose failurePolicy is Fail and
// whose backing Service is missing or has no ready endpoints — such a webhook
// rejects every create/update it intercepts, cluster-wide. Pure and read-only: the
// caller supplies the webhook configs and the collected Services/EndpointSlices.
// Advisory (never affects the cluster verdict).
package webhookhealth

import (
	"fmt"
	"sort"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/imantaba/kubeagent/internal/svchealth"
)

// Issue is one admission webhook whose backend is down under a Fail policy.
type Issue struct {
	Kind    string `json:"kind"`    // "ValidatingWebhookConfiguration" | "MutatingWebhookConfiguration"
	Config  string `json:"config"`  // the configuration object's name
	Webhook string `json:"webhook"` // the individual webhook's .name
	Service string `json:"service"` // "ns/name" of the backend
	Problem string `json:"problem"` // "missing-service" | "no-endpoints"
	Reason  string `json:"reason"`
}

// hook is a normalized view of a Validating/Mutating webhook entry so one routine
// handles both (they share these fields but are distinct Go types).
type hook struct {
	kind    string
	config  string
	name    string
	fp      *admissionv1.FailurePolicyType
	service *admissionv1.ServiceReference
}

// Assess flags Fail-policy webhooks whose backend Service is missing or has no
// ready endpoints, sorted by (Kind, Config, Webhook).
func Assess(
	validating []admissionv1.ValidatingWebhookConfiguration,
	mutating []admissionv1.MutatingWebhookConfiguration,
	services []corev1.Service,
	slices []discoveryv1.EndpointSlice,
) []Issue {
	var hooks []hook
	for _, c := range validating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"ValidatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service})
		}
	}
	for _, c := range mutating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"MutatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service})
		}
	}

	var out []Issue
	for _, h := range hooks {
		if !failsClosed(h.fp) || h.service == nil {
			continue // Ignore policy, or a URL-based webhook we can't check
		}
		id := h.service.Namespace + "/" + h.service.Name
		svc, found := findService(services, h.service.Namespace, h.service.Name)
		switch {
		case !found:
			out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "missing-service",
				Reason: fmt.Sprintf("backend Service %s does not exist — failurePolicy Fail rejects every intercepted create/update", id)})
		case svchealth.ReadyEndpoints(svc, slices) == 0:
			out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "no-endpoints",
				Reason: fmt.Sprintf("backend Service %s has no ready endpoints — failurePolicy Fail rejects every intercepted create/update", id)})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Config != out[j].Config {
			return out[i].Config < out[j].Config
		}
		return out[i].Webhook < out[j].Webhook
	})
	return out
}

// failsClosed reports whether a webhook blocks on backend failure. A nil
// failurePolicy defaults to Fail in admissionregistration.k8s.io/v1.
func failsClosed(fp *admissionv1.FailurePolicyType) bool {
	return fp == nil || *fp == admissionv1.Fail
}

// findService returns the collected Service matching ns/name, or ok=false.
func findService(services []corev1.Service, ns, name string) (corev1.Service, bool) {
	for _, s := range services {
		if s.Namespace == ns && s.Name == name {
			return s, true
		}
	}
	return corev1.Service{}, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/webhookhealth/`
Expected: PASS (all tests).

- [ ] **Step 5: Commit**

```bash
git add internal/webhookhealth/
git commit -m "feat(webhookhealth): classify admission webhooks with a down backend"
```

---

### Task 2: collectors — `ValidatingWebhookConfigurations` + `MutatingWebhookConfigurations`

**Files:**
- Modify: `internal/collect/collect.go` (add two functions + `admissionv1` import)
- Test: `internal/collect/collect_test.go` (add one test)

**Interfaces:**
- Produces: `func ValidatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.ValidatingWebhookConfiguration, error)`; `func MutatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.MutatingWebhookConfiguration, error)`.

- [ ] **Step 1: Write the failing test**

Add to `internal/collect/collect_test.go` (add `admissionv1 "k8s.io/api/admissionregistration/v1"` to its imports):

```go
func TestWebhookConfigurations(t *testing.T) {
	vwc := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "vw"}}
	mwc := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "mw"}}
	client := fake.NewSimpleClientset(vwc, mwc)
	v, err := ValidatingWebhookConfigurations(context.Background(), client)
	if err != nil || len(v) != 1 || v[0].Name != "vw" {
		t.Fatalf("validating: got %+v err %v", v, err)
	}
	m, err := MutatingWebhookConfigurations(context.Background(), client)
	if err != nil || len(m) != 1 || m[0].Name != "mw" {
		t.Fatalf("mutating: got %+v err %v", m, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/collect/ -run TestWebhookConfigurations`
Expected: FAIL — `undefined: ValidatingWebhookConfigurations`.

- [ ] **Step 3: Write the implementation**

Add `admissionv1 "k8s.io/api/admissionregistration/v1"` to the imports of `internal/collect/collect.go`, then add these two functions next to `HorizontalPodAutoscalers`:

```go
// ValidatingWebhookConfigurations lists all validating admission webhook configs
// (cluster-scoped; read-only). Used by the admission-webhook-failure check. Needs
// the base admissionregistration.k8s.io grant; a forbidden/absent result omits it.
func ValidatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.ValidatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing validatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
}

// MutatingWebhookConfigurations lists all mutating admission webhook configs
// (cluster-scoped; read-only).
func MutatingWebhookConfigurations(ctx context.Context, client kubernetes.Interface) ([]admissionv1.MutatingWebhookConfiguration, error) {
	list, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing mutatingwebhookconfigurations: %w", err)
	}
	return list.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/collect/ -run TestWebhookConfigurations`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list admission webhook configurations (read-only)"
```

---

### Task 3: `scan.Evaluate` — wiring + `Result.WebhookIssues` + scope guard

**Files:**
- Modify: `internal/scan/scan.go` (import, `Result.WebhookIssues` field, scope-guarded collect+assess, return literal)
- Test: `internal/scan/scan_test.go` (add three tests)

**Interfaces:**
- Consumes: `webhookhealth.Assess`, `collect.ValidatingWebhookConfigurations`, `collect.MutatingWebhookConfigurations` (Tasks 1–2); the existing locals `svcs` (`collect.Services`) and `slices` (`collect.EndpointSlices`).
- Produces: `Result.WebhookIssues []webhookhealth.Issue`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/scan/scan_test.go` (add `admissionv1 "k8s.io/api/admissionregistration/v1"` and `discoveryv1 "k8s.io/api/discovery/v1"` to imports if absent):

```go
// downWebhook builds a Fail validating webhook whose backend Service exists but
// has no ready endpoints, plus that Service and a not-ready EndpointSlice.
func downWebhookObjects() []runtime.Object {
	fail := admissionv1.Fail
	notReady := false
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-webhook"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:          "validate.policy.io",
			FailurePolicy: &fail,
			ClientConfig:  admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: "kube-system", Name: "policy-svc"}},
		}},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "policy-svc"}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "policy-svc-x", Labels: map[string]string{discoveryv1.LabelServiceName: "policy-svc"}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &notReady}}},
	}
	return []runtime.Object{vwc, svc, slice}
}

func TestEvaluate_FlagsDownWebhook(t *testing.T) {
	cli := fake.NewSimpleClientset(downWebhookObjects()...)
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.WebhookIssues) != 1 || res.WebhookIssues[0].Problem != "no-endpoints" {
		t.Fatalf("expected one no-endpoints webhook issue, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_WebhookCheckSkippedWhenNamespaceScoped(t *testing.T) {
	cli := fake.NewSimpleClientset(downWebhookObjects()...)
	res, err := Evaluate(context.Background(), cli, Options{Namespace: "kube-system"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.WebhookIssues) != 0 {
		t.Fatalf("the webhook check must be skipped under --namespace, got %+v", res.WebhookIssues)
	}
}

func TestEvaluate_ForbiddenWebhooksStillScans(t *testing.T) {
	cli := fake.NewSimpleClientset()
	cli.Fake.PrependReactor("list", "validatingwebhookconfigurations", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "admissionregistration.k8s.io", Resource: "validatingwebhookconfigurations"}, "", nil)
	})
	res, err := Evaluate(context.Background(), cli, Options{})
	if err != nil {
		t.Fatalf("a forbidden webhook list must not error, got %v", err)
	}
	if len(res.WebhookIssues) != 0 {
		t.Fatalf("forbidden webhook list must yield no issues, got %+v", res.WebhookIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scan/ -run 'TestEvaluate_(FlagsDownWebhook|WebhookCheckSkippedWhenNamespaceScoped|ForbiddenWebhooksStillScans)'`
Expected: FAIL — `res.WebhookIssues undefined`.

- [ ] **Step 3: Write the implementation**

In `internal/scan/scan.go`:

1. Add the import `"github.com/imantaba/kubeagent/internal/webhookhealth"`.
2. Add a field to the `Result` struct, next to `HPAIssues []hpahealth.Issue`:

```go
	WebhookIssues     []webhookhealth.Issue
```

3. After the HPA wiring (`hpaIssues := hpahealth.Assess(hpas)`), add the scope-guarded block (the `svcs` and `slices` locals from `collect.Services`/`collect.EndpointSlices` are already in scope higher up):

```go
	var webhookIssues []webhookhealth.Issue
	if opts.Namespace == "" { // webhook backends can live in any namespace; only sound cluster-wide
		vwc, _ := collect.ValidatingWebhookConfigurations(ctx, client)
		mwc, _ := collect.MutatingWebhookConfigurations(ctx, client)
		webhookIssues = webhookhealth.Assess(vwc, mwc, svcs, slices)
	}
```

4. Add `WebhookIssues: webhookIssues,` to the `return Result{…}` literal (next to `HPAIssues: hpaIssues`).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/scan/
git commit -m "feat(scan): assess admission webhooks into Result.WebhookIssues (cluster-wide only)"
```

---

### Task 4: `report` — NEEDS ATTENTION section + attention line + JSON

**Files:**
- Modify: `internal/report/report.go` (Input field, JSON field, `hasAttention`, `printWebhookIssues`, render call site, `attentionLine`)
- Test: `internal/report/report_test.go` (add tests)

**Interfaces:**
- Consumes: `webhookhealth.Issue` (Task 1), `Result.WebhookIssues` (Task 3).
- Produces: `report.Input.WebhookIssues []webhookhealth.Issue`.

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go` (add `"github.com/imantaba/kubeagent/internal/webhookhealth"` to imports; the cluster field type is `clusterhealth.ClusterHealth` — mirror `TestPrintInventory_HPAIssues`):

```go
func TestPrintInventory_WebhookIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"},
		WebhookIssues: []webhookhealth.Issue{
			{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "validate.policy.io",
				Service: "kube-system/policy-svc", Problem: "no-endpoints",
				Reason: "backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update"},
		},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "✗ policy-webhook  ValidatingWebhookConfiguration  webhook validate.policy.io") {
		t.Errorf("missing webhook header line:\n%s", out)
	}
	if !strings.Contains(out, "⚠ WebhookDown: backend Service kube-system/policy-svc has no ready endpoints") {
		t.Errorf("missing WebhookDown reason line:\n%s", out)
	}
	if !strings.Contains(out, "1 admission webhook failing") {
		t.Errorf("missing attention-line fragment:\n%s", out)
	}
}

func TestPrintInventory_NoWebhookSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy"}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "WebhookDown") {
		t.Errorf("no webhook section expected when empty:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run TestPrintInventory_WebhookIssues`
Expected: FAIL — `unknown field WebhookIssues in struct literal`.

- [ ] **Step 3: Write the implementation**

In `internal/report/report.go` (mirror the existing `HPAIssues` handling — place each webhook piece right after its HPA counterpart):

1. Add `"github.com/imantaba/kubeagent/internal/webhookhealth"` to imports.
2. In the JSON `inventoryReport` struct (where `HPAIssues` has its json tag), add:

```go
	WebhookIssues      []webhookhealth.Issue       `json:"webhookIssues,omitempty"`
```

and set it in the struct literal that copies from `in` (next to `HPAIssues: in.HPAIssues,`):

```go
		WebhookIssues:      in.WebhookIssues,
```

3. In the `Input` struct, add next to `HPAIssues []hpahealth.Issue`:

```go
	WebhookIssues      []webhookhealth.Issue
```

4. Extend `hasAttention` (currently ends `… || len(in.HPAIssues) > 0`):

```go
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(realIng) > 0 || len(in.PVCIssues) > 0 || len(in.StuckTerminating) > 0 || len(in.PDBIssues) > 0 || len(in.HPAIssues) > 0 || len(in.WebhookIssues) > 0
```

5. Add the render call right after `printHPAIssues`:

```go
		if err := printWebhookIssues(in.WebhookIssues, w); err != nil {
			return err
		}
```

6. Add the printer next to `printHPAIssues`:

```go
// printWebhookIssues lists admission webhooks that will reject every intercepted request.
func printWebhookIssues(issues []webhookhealth.Issue, w io.Writer) error {
	for _, is := range issues {
		if _, err := fmt.Fprintf(w, "  ✗ %s  %s  webhook %s\n", is.Config, is.Kind, is.Webhook); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "      ⚠ WebhookDown: %s\n", is.Reason); err != nil {
			return err
		}
	}
	return nil
}
```

7. In `attentionLine`, after the `HPAIssues` fragment block, add:

```go
	if n := len(in.WebhookIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s failing", n, plural(n, "admission webhook", "admission webhooks")))
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/report/ -run TestPrintInventory`
Expected: PASS. (The golden test `TestGoldenScanOutput` still passes — the golden fixture has no webhook issues yet; the fixture update is Task 8. Do NOT modify the golden fixture or testdata here.)

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go
git commit -m "feat(report): render admission-webhook-failure section and attention line"
```

---

### Task 5: `main.go` — carry `WebhookIssues` through the `resultInput` seam

**Files:**
- Modify: `main.go` (add `WebhookIssues: res.WebhookIssues` to `resultInput`)
- Test: `main_test.go` (add one seam test)

**Interfaces:**
- Consumes: `scan.Result.WebhookIssues` (Task 3), `report.Input.WebhookIssues` (Task 4).

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (next to `TestResultInput_CarriesHPAIssues`; add `"github.com/imantaba/kubeagent/internal/webhookhealth"` to imports):

```go
func TestResultInput_CarriesWebhookIssues(t *testing.T) {
	// Regression: the scan.Result → report.Input mapping must carry WebhookIssues,
	// or the section never renders in the CLI (the stuck-terminating v0.34.0 bug).
	res := scan.Result{WebhookIssues: []webhookhealth.Issue{
		{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "validate.policy.io", Problem: "no-endpoints", Reason: "…"},
	}}
	in := resultInput(res)
	if len(in.WebhookIssues) != 1 || in.WebhookIssues[0].Config != "policy-webhook" {
		t.Fatalf("resultInput must carry WebhookIssues into report.Input, got %+v", in.WebhookIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestResultInput_CarriesWebhookIssues`
Expected: FAIL — `in.WebhookIssues` is empty (field not mapped).

- [ ] **Step 3: Write the implementation**

In `main.go`, add to the `resultInput` return literal (next to `HPAIssues: res.HPAIssues,`):

```go
		WebhookIssues:    res.WebhookIssues,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestResultInput`
Expected: PASS (StuckTerminating, PDBIssues, HPAIssues, and WebhookIssues seam tests).

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "fix(report): wire WebhookIssues through the resultInput seam"
```

---

### Task 6: `watch` — the `kubeagent_admission_webhooks_failing` gauge

**Files:**
- Modify: `internal/watch/metrics.go` (field, update, gauge)
- Test: `internal/watch/metrics_test.go` (add assertion)

**Interfaces:**
- Consumes: `scan.Result.WebhookIssues` (Task 3).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, in `TestMetrics_RenderReflectsResult`, add `WebhookIssues: []webhookhealth.Issue{{Config: "policy-webhook", Webhook: "w"}}` to the sample `scan.Result` (add the import `"github.com/imantaba/kubeagent/internal/webhookhealth"`) and add to the asserted substrings list:

```go
		"kubeagent_admission_webhooks_failing 1",
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/watch/ -run Metrics`
Expected: FAIL — gauge absent / count 0.

- [ ] **Step 3: Write the implementation**

In `internal/watch/metrics.go`:

1. Add a field next to `hpaScalingIssues int`:

```go
	webhooksFailing     int
```

2. In the update path (next to `m.hpaScalingIssues = len(res.HPAIssues)`):

```go
	m.webhooksFailing = len(res.WebhookIssues)
```

3. Add the gauge render (next to the `kubeagent_hpa_scaling_issues` gauge):

```go
	gauge("kubeagent_admission_webhooks_failing", "Fail-policy admission webhooks whose backend is missing or has no ready endpoints", float64(m.webhooksFailing))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/watch/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/
git commit -m "feat(watch): expose kubeagent_admission_webhooks_failing gauge"
```

---

### Task 7: RBAC + Helm — the `admissionregistration.k8s.io` grant

**Files:**
- Modify: `deploy/rbac.yaml` (add rule)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (add rule)

**Interfaces:** none (manifests).

- [ ] **Step 1: Add the rule to both files**

In `deploy/rbac.yaml`, after the `autoscaling` rule block (the one granting `horizontalpodautoscalers`, at lines 36-38), add:

```yaml
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: [validatingwebhookconfigurations, mutatingwebhookconfigurations]
    verbs: [get, list, watch]
```

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, after the same `autoscaling` rule block (lines 34-36) and BEFORE the `{{- if or .Values.diskUsage.enabled … }}` conditional block, add the identical three lines.

- [ ] **Step 2: Verify Helm renders the rule and lints clean**

Run:
```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A2 'apiGroups: \["admissionregistration.k8s.io"\]'
```
Expected: `1 chart(s) linted, 0 chart(s) failed`; the grep shows the webhook-configurations rule.

- [ ] **Step 3: Verify the raw manifest has the rule**

Run: `grep -A2 'admissionregistration' deploy/rbac.yaml`
Expected: shows the rule with `validatingwebhookconfigurations, mutatingwebhookconfigurations` under `apiGroups: ["admissionregistration.k8s.io"]`.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only admissionregistration.k8s.io webhook configs"
```

---

### Task 8: Golden snapshot + coverage guard + docs

**Files:**
- Modify: `internal/report/golden_test.go` (add a webhook to `goldenInput`; extend `TestGoldenInputCoversAllSections`)
- Modify: `internal/report/testdata/golden-scan.txt` (regenerate)
- Modify: `website/docs/features/diagnostics.md`, `website/docs/features/watch-mode.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** Consumes everything above.

- [ ] **Step 1: Add a webhook to the golden fixture and extend the coverage guard**

In `internal/report/golden_test.go`, in the `goldenInput` builder, add (next to the `HPAIssues:` field), importing `webhookhealth`:

```go
		WebhookIssues: []webhookhealth.Issue{
			{Kind: "ValidatingWebhookConfiguration", Config: "policy-webhook", Webhook: "validate.policy.io",
				Service: "kube-system/policy-svc", Problem: "no-endpoints",
				Reason: "backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update"},
		},
```

(the `—` is em dash U+2014). Extend the guard in `TestGoldenInputCoversAllSections` — change the trailing condition `|| len(in.HPAIssues) == 0` to also include `|| len(in.WebhookIssues) == 0`.

- [ ] **Step 2: Run the golden test to confirm it fails (snapshot drift)**

Run: `go test ./internal/report/ -run TestGoldenScanOutput`
Expected: FAIL — the rendered output now contains the webhook block, which the snapshot lacks.

- [ ] **Step 3: Regenerate the golden snapshot**

Run: `go test ./internal/report -run TestGoldenScanOutput -update`
Then inspect: `git diff internal/report/testdata/golden-scan.txt` — it must add the two webhook lines (`✗ policy-webhook  ValidatingWebhookConfiguration  webhook validate.policy.io` and `⚠ WebhookDown: backend Service …`) and update the `Needs attention:` line with `· 1 admission webhook failing`.

- [ ] **Step 4: Run the full report suite**

Run: `go test ./internal/report/`
Expected: PASS.

- [ ] **Step 5: Update docs**

- `website/docs/features/diagnostics.md`: add an "Admission-webhook failure" entry describing the two problems (missing-service / no-endpoints), the Fail-policy scope, cluster-wide-only, and that it's advisory/read-only.
- `website/docs/features/watch-mode.md`: add the `kubeagent_admission_webhooks_failing` gauge to the metrics list.
- `README.md`: add admission-webhook failure to the detector list.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **Admission-webhook-failure detection.** `scan` flags a Validating/Mutating
    webhook whose `failurePolicy` is `Fail` and whose backing Service is missing
    or has no ready endpoints — it would reject every create/update it intercepts.
    Read-only, advisory, and cluster-wide only (skipped under `--namespace`); the
    daemon exposes `kubeagent_admission_webhooks_failing`. Adds a base
    `admissionregistration.k8s.io` read grant.
  ```
- `website/docs/roadmap.md`: move admission-webhook failure from the Theme-B "headed" list into the Shipped list.

- [ ] **Step 6: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 7: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/report/golden_test.go internal/report/testdata/golden-scan.txt website/ README.md CHANGELOG.md
git commit -m "test+docs: golden coverage and documentation for admission-webhook failure"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/collect` + RBAC + Helm → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`) plus a targeted live smoke (a Fail-policy `ValidatingWebhookConfiguration` pointing at a Service with no endpoints, and one pointing at a missing Service). **Minor** version bump **v0.36.0 → v0.37.0**; **minor** chart bump (Helm template changed). Hold for the user's explicit "run release and push".
