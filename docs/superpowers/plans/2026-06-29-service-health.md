# Service / LoadBalancer Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flag Services routing to zero ready endpoints and LoadBalancer Services with no external address, in a new "Service issues" section (text + JSON) and in `--explain`.

**Architecture:** A new pure package `internal/svchealth` assesses `[]Issue` from Services + EndpointSlices. `internal/collect` gains two read-only List helpers (namespace-scoped). `report` and `explain` render the issues; `main.go` wires them in as a new `serviceIssues` parameter alongside the existing summary/facts.

**Tech Stack:** Go 1.26, client-go, `k8s.io/api/{core,discovery}/v1` (subpackages of the already-required `k8s.io/api`), stdlib `flag`/`sort`/`time`.

## Global Constraints

- **READ-ONLY:** only new List calls (Services, EndpointSlices). No create/update/patch/delete.
- **No new Go module dependency.** `k8s.io/api/discovery/v1` is a subpackage of `k8s.io/api`, already in go.mod.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Namespace scope:** Service checks honor the scan's `-n` (namespace passed through; empty = all).
- **`--explain` egress:** only Service namespace/name/type/problem strings — never pod/endpoint IPs, specs, or secrets.
- **Best-effort:** List failures in `main` are non-fatal (empty input → no issues).
- **Endpoint readiness:** an endpoint is ready when `Conditions.Ready == nil || *Conditions.Ready` (nil means ready, per kube-proxy).
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` before any `go` command. Run `gofmt -l` on touched files; fix with `gofmt -w`.
- **Scope (YAGNI):** no P1 elevation, no port/targetPort checks, no Ingress, no `report.Input` struct refactor (separate later step).

---

### Task 1: `internal/svchealth` — Service issue assessment (pure)

**Files:**
- Create: `internal/svchealth/svchealth.go`
- Test: `internal/svchealth/svchealth_test.go`

**Interfaces:**
- Produces:
  - `type Issue struct { Namespace, Name, Type, Problem, Detail, Since string }`
  - `func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice) []Issue`

- [ ] **Step 1: Write the failing test**

Create `internal/svchealth/svchealth_test.go`:

```go
package svchealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svc(ns, name string, t corev1.ServiceType, selector map[string]string, lbIngress int) corev1.Service {
	s := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.ServiceSpec{Type: t, Selector: selector},
	}
	for i := 0; i < lbIngress; i++ {
		s.Status.LoadBalancer.Ingress = append(s.Status.LoadBalancer.Ingress, corev1.LoadBalancerIngress{IP: "1.2.3.4"})
	}
	return s
}

func slice(ns, svcName string, readyStates ...*bool) discoveryv1.EndpointSlice {
	es := discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-abc", Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
	}
	for _, r := range readyStates {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: r}})
	}
	return es
}

func boolp(b bool) *bool { return &b }

func TestAssess_NoEndpoints(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	// a slice exists but all endpoints not-ready
	slices := []discoveryv1.EndpointSlice{slice("default", "web", boolp(false))}
	got := Assess(svcs, slices)
	if len(got) != 1 || got[0].Problem != "NoEndpoints" || got[0].Type != "ClusterIP" || got[0].Detail != "no ready endpoints" {
		t.Fatalf("want one NoEndpoints issue, got %+v", got)
	}
}

func TestAssess_HasReadyEndpoints_NoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", boolp(true), nil)} // one ready, one nil(=ready)
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("ready endpoints should yield no issue, got %+v", got)
	}
}

func TestAssess_NilReadyCountsAsReady(t *testing.T) {
	svcs := []corev1.Service{svc("default", "web", corev1.ServiceTypeClusterIP, map[string]string{"app": "web"}, 0)}
	slices := []discoveryv1.EndpointSlice{slice("default", "web", nil)} // nil Ready => ready
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("nil Ready should count as ready, got %+v", got)
	}
}

func TestAssess_ExternalNameAndSelectorlessSkipped(t *testing.T) {
	svcs := []corev1.Service{
		svc("default", "ext", corev1.ServiceTypeExternalName, nil, 0),
		svc("default", "manual", corev1.ServiceTypeClusterIP, nil, 0), // no selector
	}
	if got := Assess(svcs, nil); len(got) != 0 {
		t.Fatalf("ExternalName and selectorless services must be skipped, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddress(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)}
	// has a ready endpoint, so the ONLY issue should be the missing LB address
	slices := []discoveryv1.EndpointSlice{slice("prod", "api-lb", boolp(true))}
	got := Assess(svcs, slices)
	if len(got) != 1 || got[0].Problem != "NoExternalAddress" || got[0].Detail != "no external address" {
		t.Fatalf("want one NoExternalAddress issue, got %+v", got)
	}
	if got[0].Since == "" {
		t.Errorf("NoExternalAddress issue should carry Since (creationTimestamp)")
	}
}

func TestAssess_LoadBalancerWithAddress_NoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 1)}
	slices := []discoveryv1.EndpointSlice{slice("prod", "api-lb", boolp(true))}
	if got := Assess(svcs, slices); len(got) != 0 {
		t.Fatalf("LB with an address and endpoints should have no issue, got %+v", got)
	}
}

func TestAssess_LoadBalancerNoAddressAndNoEndpoints_TwoIssues(t *testing.T) {
	svcs := []corev1.Service{svc("prod", "api-lb", corev1.ServiceTypeLoadBalancer, map[string]string{"app": "api"}, 0)}
	got := Assess(svcs, nil) // no slices => no endpoints
	if len(got) != 2 {
		t.Fatalf("want two issues (no address + no endpoints), got %+v", got)
	}
	// sorted by Problem: "NoEndpoints" < "NoExternalAddress"
	if got[0].Problem != "NoEndpoints" || got[1].Problem != "NoExternalAddress" {
		t.Errorf("issues should be sorted by problem, got %s then %s", got[0].Problem, got[1].Problem)
	}
}

func TestAssess_SortedByNamespaceName(t *testing.T) {
	svcs := []corev1.Service{
		svc("b", "z", corev1.ServiceTypeClusterIP, map[string]string{"a": "b"}, 0),
		svc("a", "y", corev1.ServiceTypeClusterIP, map[string]string{"a": "b"}, 0),
	}
	got := Assess(svcs, nil)
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("want sorted by namespace, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/`
Expected: FAIL — package has no non-test files / `undefined: Assess`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/svchealth/svchealth.go`:

```go
// Package svchealth flags Service-level problems a pod/workload scan misses: a
// selector-based Service with no ready backend endpoints, and a LoadBalancer
// Service with no external address. It is pure — the caller supplies the
// Services and EndpointSlices.
package svchealth

import (
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

// Issue is one Service-level problem.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Type      string `json:"type"`            // ClusterIP | NodePort | LoadBalancer
	Problem   string `json:"problem"`         // "NoEndpoints" | "NoExternalAddress"
	Detail    string `json:"detail"`          // human one-liner
	Since     string `json:"since,omitempty"` // RFC3339 service creationTimestamp (LB age)
}

// Assess flags Service problems. One Issue per (service, problem); a LoadBalancer
// with no address AND no endpoints yields two. Result is sorted by
// (Namespace, Name, Problem). ExternalName and selectorless Services are skipped.
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice) []Issue {
	var out []Issue
	for _, s := range services {
		if s.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
			out = append(out, Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoExternalAddress", Detail: "no external address",
				Since: s.CreationTimestamp.Time.UTC().Format(time.RFC3339),
			})
		}
		if len(s.Spec.Selector) == 0 {
			continue
		}
		if readyEndpoints(s, slices) == 0 {
			out = append(out, Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Problem < out[j].Problem
	})
	return out
}

// readyEndpoints counts ready backend addresses for a Service across its
// EndpointSlices (matched by namespace + the kubernetes.io/service-name label).
func readyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int {
	total := 0
	for _, sl := range slices {
		if sl.Namespace != svc.Namespace || sl.Labels[discoveryv1.LabelServiceName] != svc.Name {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				total += len(ep.Addresses)
			}
		}
	}
	return total
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/svchealth/ -v && go vet ./internal/svchealth/ && gofmt -l internal/svchealth/`
Expected: all tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/svchealth/
git commit -m "feat(svchealth): assess Services with no endpoints / no LB address"
```

---

### Task 2: `internal/collect` — list Services + EndpointSlices

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces:
  - `func Services(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Service, error)`
  - `func EndpointSlices(ctx context.Context, client kubernetes.Interface, namespace string) ([]discoveryv1.EndpointSlice, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/collect/collect_test.go` (add `discoveryv1 "k8s.io/api/discovery/v1"` to imports; `context`, `testing`, `corev1`, `metav1`, and `k8s.io/client-go/kubernetes/fake` are already imported):

```go
func TestServices_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "s2"}},
	)
	svcs, err := Services(context.Background(), client, "")
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(svcs) != 2 {
		t.Errorf("want 2 services, got %d", len(svcs))
	}
}

func TestEndpointSlices_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "s1-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "s1"}}},
	)
	slices, err := EndpointSlices(context.Background(), client, "")
	if err != nil {
		t.Fatalf("EndpointSlices: %v", err)
	}
	if len(slices) != 1 || slices[0].Labels[discoveryv1.LabelServiceName] != "s1" {
		t.Errorf("unexpected slices: %+v", slices)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/`
Expected: FAIL — `undefined: Services` / `undefined: EndpointSlices`.

- [ ] **Step 3: Write minimal implementation**

In `internal/collect/collect.go` add `discoveryv1 "k8s.io/api/discovery/v1"` to imports, then append:

```go
// Services lists Services in the namespace (empty = all), read-only.
func Services(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.Service, error) {
	svcs, err := client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w", err)
	}
	return svcs.Items, nil
}

// EndpointSlices lists EndpointSlices in the namespace (empty = all), read-only.
func EndpointSlices(ctx context.Context, client kubernetes.Interface, namespace string) ([]discoveryv1.EndpointSlice, error) {
	slices, err := client.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing endpointslices: %w", err)
	}
	return slices.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -v && go vet ./internal/collect/ && gofmt -l internal/collect/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list Services and EndpointSlices (namespace-scoped)"
```

---

### Task 3: `internal/report` — Service issues section + JSON + signature

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `svchealth.Issue`.
- Produces (changed signature):
  `func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, explanation, format string, w io.Writer) error`

- [ ] **Step 1: Write the failing test**

In `internal/report/report_test.go` add `"github.com/imantaba/kubeagent/internal/svchealth"` to imports, add the tests below, and insert `nil` as the new **fifth** argument (after the `facts` argument, before `explanation`) in **every existing** `PrintInventory(...)` call in the file:

```go
func sampleServiceIssues() []svchealth.Issue {
	return []svchealth.Issue{
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"},
		{Namespace: "default", Name: "api-lb", Type: "LoadBalancer", Problem: "NoExternalAddress", Detail: "no external address", Since: "2026-06-29T00:00:00Z"},
	}
}

func TestPrintInventory_TextShowsServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Service issues:", "default/web", "ClusterIP", "no ready endpoints", "default/api-lb", "LoadBalancer", "no external address"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextNoServiceSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Service issues:") {
		t.Errorf("no Service issues section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_ServiceIssuesSuppressAllClear(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("all-clear must not print when there are service issues:\n%s", buf.String())
	}
}

func TestPrintInventory_JSONIncludesServiceIssues(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, sampleServiceIssues(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		ServiceIssues []svchealth.Issue `json:"serviceIssues"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.ServiceIssues) != 2 || got.ServiceIssues[0].Name != "web" {
		t.Errorf("serviceIssues missing/wrong in JSON: %+v", got.ServiceIssues)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/`
Expected: FAIL — too many arguments / `undefined: svchealth`.

- [ ] **Step 3: Write minimal implementation**

In `internal/report/report.go`:

Add `"github.com/imantaba/kubeagent/internal/svchealth"` to imports.

Add to `inventoryReport` (after `Platform`):

```go
	ServiceIssues []svchealth.Issue `json:"serviceIssues,omitempty"`
```

Change `PrintInventory` signature and the JSON/text dispatch:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, ServiceIssues: serviceIssues, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, serviceIssues, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

Change `printInventoryText`'s signature to add `serviceIssues []svchealth.Issue` (after `facts`), and replace the workloads/all-clear block (currently the `if len(result.Workloads) == 0 { … } else { … }`) with this — render workloads, then the Service issues section, then a widened all-clear:

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, explanation string, w io.Writer) error {
	// ... existing verdict block + printResources(summary, w) unchanged ...

	for _, wl := range result.Workloads {
		if err := printWorkload(wl, w); err != nil {
			return err
		}
	}

	if err := printServiceIssues(serviceIssues, w); err != nil {
		return err
	}

	if len(result.Workloads) == 0 && len(serviceIssues) == 0 && cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}

	// ... existing footerHint + explanation blocks unchanged ...
}
```

Add the section helper:

```go
func printServiceIssues(issues []svchealth.Issue, w io.Writer) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Service issues:"); err != nil {
		return err
	}
	for _, is := range issues {
		line := fmt.Sprintf("  ⚠ %s/%s  %s  %s", is.Namespace, is.Name, is.Type, is.Detail)
		if is.Since != "" {
			line += " · " + inventory.HumanSince(is.Since, time.Now())
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
```

(`report.go` already imports `time` and `internal/inventory`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -v && go vet ./internal/report/ && gofmt -l internal/report/`
Expected: PASS (new + all existing), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): Service issues section + JSON field"
```

---

### Task 4: `internal/explain` — Service issues in the prompt

**Files:**
- Modify: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `svchealth.Issue`.
- Produces (changed signatures):
  - `func (c *Client) ExplainInventory(ctx, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) (string, error)`
  - `func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) string`

- [ ] **Step 1: Write the failing test**

In `internal/explain/explain_test.go` add `"github.com/imantaba/kubeagent/internal/svchealth"` to imports, insert `nil` as the new **fifth** argument (after `facts`) in every existing `ExplainInventory(...)` call and as the new **fourth** argument (after `facts`) in every existing `buildInventoryPrompt(...)` call, then add:

```go
func TestBuildInventoryPrompt_IncludesServiceIssues(t *testing.T) {
	issues := []svchealth.Issue{
		{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"},
	}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, issues, nil)
	if !strings.Contains(got, "Service issues:") || !strings.Contains(got, "default/web (ClusterIP): no ready endpoints") {
		t.Errorf("prompt missing service issues:\n%s", got)
	}
}

func TestExplainInventory_ExplainsWhenOnlyServiceIssues(t *testing.T) {
	f := &fakeSummarizer{reply: "web has no endpoints"}
	c := &Client{s: f}
	issues := []svchealth.Issue{{Namespace: "default", Name: "web", Type: "ClusterIP", Problem: "NoEndpoints", Detail: "no ready endpoints"}}
	got, err := c.ExplainInventory(context.Background(), clusterhealth.ClusterHealth{Verdict: "Healthy"}, nil, nil, issues, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "web has no endpoints" || !f.called {
		t.Errorf("expected service-only issues to be explained; got %q called=%v", got, f.called)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/`
Expected: FAIL — wrong arg count / `undefined: svchealth`.

- [ ] **Step 3: Write minimal implementation**

In `internal/explain/explain.go`: add `"github.com/imantaba/kubeagent/internal/svchealth"` to imports; change both signatures to add `serviceIssues []svchealth.Issue` after `facts`; widen the skip; render the block.

```go
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) (string, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 && len(serviceIssues) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads))
	// ... rest unchanged ...
}
```

In `buildInventoryPrompt`, change the signature and add the block after the workloads block (before the final `b.WriteString("\nExplain …")`):

```go
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, workloads []inventory.Workload) string {
	var b strings.Builder
	// ... existing degraded-cluster, platform, resources, workloads blocks unchanged ...

	if len(serviceIssues) > 0 {
		b.WriteString("Service issues:\n")
		for _, is := range serviceIssues {
			fmt.Fprintf(&b, "  - %s/%s (%s): %s\n", is.Namespace, is.Name, is.Type, is.Detail)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nExplain what is going wrong and suggest concrete next steps.")
	return b.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -v && go vet ./internal/explain/ && gofmt -l internal/explain/`
Expected: PASS (new + all existing, incl. the egress-guard test), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): include Service issues in the prompt"
```

---

### Task 5: wire `main.go`, document, CHANGELOG

**Files:**
- Modify: `main.go`
- Modify: `README.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: `collect.Services`/`EndpointSlices`, `svchealth.Assess`, the new `report.PrintInventory` / `explain.ExplainInventory` signatures.

- [ ] **Step 1: Wire main.go**

Add `"github.com/imantaba/kubeagent/internal/svchealth"` to imports. After the existing `facts := platform.Detect(...)` block (and before `inventory.Prioritize` / the explain call), insert:

```go
	svcs, _ := collect.Services(context.Background(), client, namespace)
	slices, _ := collect.EndpointSlices(context.Background(), client, namespace)
	serviceIssues := svchealth.Assess(svcs, slices)
```

Update the two consumers to pass `serviceIssues` (after `&facts`):

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, &summary, &facts, serviceIssues, result.Workloads)
```

```go
	return report.PrintInventory(health, result, &summary, &facts, serviceIssues, explanation, *output, os.Stdout)
```

- [ ] **Step 2: Build, vet, gofmt, full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && gofmt -l main.go && go build -o /tmp/kubeagent .`
Expected: all packages `ok`, `gofmt -l main.go` prints nothing, build succeeds.

- [ ] **Step 3: Document in README.md**

Add a `### Service health` subsection in the scan/usage area (before `## Install`):

```markdown
### Service health

`scan` flags Service-level problems a pod scan misses: a selector-based Service
routing to **zero ready endpoints** (selector typo, all backends down) and a
**LoadBalancer Service with no external address** (showing its age so you can
tell provisioning from stuck). These appear in a "Service issues" section (text
and JSON) and are sent to `--explain`. ExternalName and selectorless Services are
skipped. Checks are read-only and honor the scan's `-n` scope.
```

- [ ] **Step 4: Update CHANGELOG.md**

Under `## [Unreleased]`, replace the `### Planned` list's Service/LoadBalancer bullet by adding an `### Added` subsection above `### Planned` (and removing that one bullet from Planned):

```markdown
## [Unreleased]

### Added

- **Service / LoadBalancer health.** `scan` flags selector-based Services with no
  ready endpoints and LoadBalancer Services with no external address, in a new
  "Service issues" section (text + JSON) and in `--explain`.

### Planned

- **NetworkPolicy awareness** — …
- **Connectivity / control-plane diagnostics** — …
- **Secret / credential lint** — …
```

(Keep the remaining three Planned bullets intact; only remove the Service/LoadBalancer one.)

- [ ] **Step 5: Commit**

```bash
git add main.go README.md CHANGELOG.md
git commit -m "feat: wire Service health into scan + explain; document + changelog"
```

---

## Self-Review

**Spec coverage:**
- `svchealth.Assess` (no-endpoints + LB-no-address, skip ExternalName/selectorless, nil-Ready=ready, two-issues, sort) → Task 1. ✓
- collect Services + EndpointSlices (namespace-scoped) → Task 2. ✓
- report Service-issues section + JSON + all-clear suppression → Task 3. ✓
- explain Service-issues block + widened skip + egress-safe → Task 4. ✓
- main wiring (best-effort, namespace-scoped) + README + CHANGELOG → Task 5. ✓

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `svchealth.Issue`/`Assess`, the `[]svchealth.Issue` parameter, and the `PrintInventory`/`ExplainInventory`/`buildInventoryPrompt` signatures (with `serviceIssues` placed after `facts`) are used identically across Tasks 1–5. `Since` is RFC3339 in both Task 1 (produced) and Task 3 (consumed via `inventory.HumanSince`).
