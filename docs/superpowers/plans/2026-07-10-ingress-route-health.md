# Ingress Route Health Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For each Ingress routing rule, resolve its backend Service and flag broken routes (`NoService` / `NoEndpoints` / `PortNotExposed`) — the usual causes of a 502/503 — in NEEDS ATTENTION, JSON, and a daemon gauge.

**Architecture:** A new pure package `internal/ingresshealth` correlates Ingress rules to Services, reusing `svchealth.ReadyEndpoints` (newly exported) for endpoint readiness. `collect.Ingresses` lists Ingresses; `scan.Evaluate` runs the assessment over the Services/EndpointSlices it already collects; report + daemon expose the issues; RBAC gains read-only `ingresses`.

**Tech Stack:** Go 1.26, `k8s.io/api/networking/v1`, `k8s.io/api/core/v1`, `k8s.io/api/discovery/v1`. Tests use fake objects + client-go fake clientset.

## Global Constraints

- Read-only; no `--fix`; not wired into `--explain`. Advisory — does NOT change the cluster verdict, `kubeagent_cluster_healthy`, or the scan exit code.
- New collectors are best-effort (a List error yields no issues, never fails the scan).
- RBAC adds `ingresses` (`get`/`list`/`watch`) to the `networking.k8s.io` rule in BOTH `deploy/rbac.yaml` and the Helm ClusterRole — on by default (plain read-only).
- Route checks (Service backends only; Resource backends skipped): Service missing → `NoService`; `ReadyEndpoints == 0` → `NoEndpoints`; a named/numbered port absent from `svc.Spec.Ports` → `PortNotExposed`. A backend that specifies **no** port is never a `PortNotExposed` candidate.
- Backends resolve within the Ingress's **own namespace** (k8s forbids cross-namespace ingress backends).
- Exact names: JSON field `ingressIssues`; Problem values `NoService`/`NoEndpoints`/`PortNotExposed`; daemon gauge `kubeagent_ingress_route_issues`.
- Commits carry **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit.

---

### Task 1: Export `svchealth.ReadyEndpoints`

**Files:**
- Modify: `internal/svchealth/svchealth.go` (rename `readyEndpoints` → `ReadyEndpoints`; update its one caller at line ~51)

**Interfaces:**
- Produces: `func ReadyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int` (exported; identical body/behavior).

- [ ] **Step 1: Rename and update the caller**

In `internal/svchealth/svchealth.go`, rename the function `readyEndpoints` to `ReadyEndpoints` (exported), keep the signature and body exactly, update its doc comment's leading word, and change the sole call site (in `Assess`, `if readyEndpoints(s, slices) == 0 {`) to `if ReadyEndpoints(s, slices) == 0 {`.

- [ ] **Step 2: Verify build + existing tests unaffected**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/svchealth/`
Expected: PASS — pure rename, no behavior change.

- [ ] **Step 3: Commit**

```bash
git add internal/svchealth/svchealth.go
git commit -m "refactor(svchealth): export ReadyEndpoints for reuse"
```

---

### Task 2: `internal/ingresshealth` package

**Files:**
- Create: `internal/ingresshealth/ingresshealth.go`
- Test: `internal/ingresshealth/ingresshealth_test.go`

**Interfaces:**
- Consumes: `svchealth.ReadyEndpoints` (Task 1).
- Produces:
  - `type RouteIssue struct { Namespace, Ingress, Host, Path, Service, Port, Problem, Detail string }` (JSON tags below).
  - `func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice) []RouteIssue`.

- [ ] **Step 1: Write the failing test**

Create `internal/ingresshealth/ingresshealth_test.go`:

```go
package ingresshealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svc(ns, name string, ports ...int32) corev1.Service {
	var sp []corev1.ServicePort
	for _, p := range ports {
		sp = append(sp, corev1.ServicePort{Port: p})
	}
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Ports: sp}}
}

// sliceFor builds an EndpointSlice with `ready` ready addresses for a service.
func sliceFor(ns, svcName string, ready int) discoveryv1.EndpointSlice {
	t := true
	var eps []discoveryv1.Endpoint
	for i := 0; i < ready; i++ {
		eps = append(eps, discoveryv1.Endpoint{Conditions: discoveryv1.EndpointConditions{Ready: &t}})
	}
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-abc", Labels: map[string]string{"kubernetes.io/service-name": svcName}},
		Endpoints:  eps,
	}
}

// ing builds an Ingress with a single host/path rule to a service:port(number).
func ing(ns, name, host, path, svcName string, portNum int32) networkingv1.Ingress {
	return networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{
					Path: path,
					Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
						Name: svcName,
						Port: networkingv1.ServiceBackendPort{Number: portNum},
					}},
				}},
			}},
		}}},
	}
}

func TestAssess_NoService(t *testing.T) {
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "missing", 80)}, nil, nil)
	if len(got) != 1 || got[0].Problem != "NoService" {
		t.Fatalf("want one NoService, got %+v", got)
	}
	if got[0].Namespace != "shop" || got[0].Ingress != "web" || got[0].Service != "missing" || got[0].Host != "x.io" || got[0].Path != "/api" {
		t.Errorf("wrong row: %+v", got[0])
	}
}

func TestAssess_NoEndpoints(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, nil) // no slices -> 0 ready
	if len(got) != 1 || got[0].Problem != "NoEndpoints" {
		t.Fatalf("want one NoEndpoints, got %+v", got)
	}
	if got[0].Port != "80" {
		t.Errorf("want port 80, got %q", got[0].Port)
	}
}

func TestAssess_PortNotExposed(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 1)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 8080)}, svcs, slices) // ready, but 8080 not exposed
	if len(got) != 1 || got[0].Problem != "PortNotExposed" {
		t.Fatalf("want one PortNotExposed, got %+v", got)
	}
}

func TestAssess_HealthyRouteNoIssue(t *testing.T) {
	svcs := []corev1.Service{svc("shop", "api", 80)}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 2)}
	got := Assess([]networkingv1.Ingress{ing("shop", "web", "x.io", "/api", "api", 80)}, svcs, slices)
	if len(got) != 0 {
		t.Fatalf("healthy route should yield no issue, got %+v", got)
	}
}

func TestAssess_NamedPortMatch(t *testing.T) {
	s := corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}}}}
	slices := []discoveryv1.EndpointSlice{sliceFor("shop", "api", 1)}
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "x.io",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "api", Port: networkingv1.ServiceBackendPort{Name: "http"}}}}},
			}},
		}}},
	}
	if got := Assess([]networkingv1.Ingress{in}, []corev1.Service{s}, slices); len(got) != 0 {
		t.Errorf("named port 'http' should match, got %+v", got)
	}
}

func TestAssess_DefaultBackendChecked(t *testing.T) {
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{Name: "fallback", Port: networkingv1.ServiceBackendPort{Number: 80}},
		}},
	}
	got := Assess([]networkingv1.Ingress{in}, nil, nil)
	if len(got) != 1 || got[0].Problem != "NoService" || got[0].Service != "fallback" || got[0].Host != "" || got[0].Path != "" {
		t.Fatalf("default backend should be checked, got %+v", got)
	}
}

func TestAssess_ResourceBackendSkipped(t *testing.T) {
	in := networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{{Path: "/", Backend: networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "assets"}}}},
			}},
		}}},
	}
	if got := Assess([]networkingv1.Ingress{in}, nil, nil); len(got) != 0 {
		t.Errorf("resource backend must be skipped, got %+v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/ingresshealth/`
Expected: FAIL — `undefined: Assess` / `undefined: RouteIssue`.

- [ ] **Step 3: Write the implementation**

Create `internal/ingresshealth/ingresshealth.go`:

```go
// Package ingresshealth flags Ingress routes whose backend Service is missing,
// has no ready endpoints, or does not expose the referenced port — the usual
// causes of a 502/503 from an ingress controller. Pure: the caller supplies the
// Ingresses, Services, and EndpointSlices. Read-only.
package ingresshealth

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imantaba/kubeagent/internal/svchealth"
)

// RouteIssue is one broken Ingress route: a rule (or the default backend) whose
// backend Service chain is broken.
type RouteIssue struct {
	Namespace string `json:"namespace"`
	Ingress   string `json:"ingress"`
	Host      string `json:"host,omitempty"`
	Path      string `json:"path,omitempty"`
	Service   string `json:"service"`
	Port      string `json:"port,omitempty"`
	Problem   string `json:"problem"` // "NoService" | "NoEndpoints" | "PortNotExposed"
	Detail    string `json:"detail"`
}

// Assess resolves each Ingress rule's backend Service (in the Ingress's own
// namespace) and flags broken routes. Only Service backends are checked;
// Resource backends are skipped.
func Assess(ingresses []networkingv1.Ingress, services []corev1.Service, slices []discoveryv1.EndpointSlice) []RouteIssue {
	byKey := make(map[string]corev1.Service, len(services))
	for _, s := range services {
		byKey[s.Namespace+"/"+s.Name] = s
	}
	var issues []RouteIssue
	for _, ing := range ingresses {
		if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
			if iss, ok := check(ing.Namespace, ing.Name, "", "", *b.Service, byKey, slices); ok {
				issues = append(issues, iss)
			}
		}
		for _, rule := range ing.Spec.Rules {
			if rule.HTTP == nil {
				continue
			}
			for _, p := range rule.HTTP.Paths {
				if p.Backend.Service == nil {
					continue // Resource backend — skip
				}
				if iss, ok := check(ing.Namespace, ing.Name, rule.Host, p.Path, *p.Backend.Service, byKey, slices); ok {
					issues = append(issues, iss)
				}
			}
		}
	}
	return issues
}

func check(ns, ingName, host, path string, be networkingv1.IngressServiceBackend, byKey map[string]corev1.Service, slices []discoveryv1.EndpointSlice) (RouteIssue, bool) {
	port := portString(be.Port)
	r := RouteIssue{Namespace: ns, Ingress: ingName, Host: host, Path: path, Service: be.Name, Port: port}
	svc, ok := byKey[ns+"/"+be.Name]
	if !ok {
		r.Problem = "NoService"
		r.Detail = fmt.Sprintf("backend Service %s not found", be.Name)
		return r, true
	}
	if svchealth.ReadyEndpoints(svc, slices) == 0 {
		r.Problem = "NoEndpoints"
		if port != "" {
			r.Detail = fmt.Sprintf("backend Service %s:%s has no ready endpoints (likely 502/503)", be.Name, port)
		} else {
			r.Detail = fmt.Sprintf("backend Service %s has no ready endpoints (likely 502/503)", be.Name)
		}
		return r, true
	}
	if !portMatches(be.Port, svc) {
		r.Problem = "PortNotExposed"
		r.Detail = fmt.Sprintf("backend Service %s does not expose port %s", be.Name, port)
		return r, true
	}
	return RouteIssue{}, false
}

// portString renders a backend port as its name, else its number, else "".
func portString(p networkingv1.ServiceBackendPort) string {
	if p.Name != "" {
		return p.Name
	}
	if p.Number != 0 {
		return strconv.Itoa(int(p.Number))
	}
	return ""
}

// portMatches reports whether the backend's named/numbered port exists on the
// Service. A backend that specifies no port is never a mismatch.
func portMatches(p networkingv1.ServiceBackendPort, svc corev1.Service) bool {
	if p.Name == "" && p.Number == 0 {
		return true
	}
	for _, sp := range svc.Spec.Ports {
		if p.Name != "" && sp.Name == p.Name {
			return true
		}
		if p.Number != 0 && sp.Port == p.Number {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/ingresshealth/`
Expected: PASS (all 7 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/ingresshealth/
git commit -m "feat(ingresshealth): flag broken Ingress routes (missing service, no endpoints, port)"
```

---

### Task 3: Collect Ingresses + wire into `scan`

**Files:**
- Modify: `internal/collect/collect.go` (add `Ingresses` after `IngressClasses`, ~168)
- Test: `internal/collect/collect_test.go`
- Modify: `internal/scan/scan.go` (import, `Result`, `Evaluate`)

**Interfaces:**
- Consumes: `ingresshealth.Assess` (Task 2).
- Produces: `func Ingresses(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.Ingress, error)`; `scan.Result.IngressIssues []ingresshealth.RouteIssue`.

- [ ] **Step 1: Write the failing collect test**

Add to `internal/collect/collect_test.go` (mirror the existing imports; it uses the fake clientset):

```go
func TestIngresses_List(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}},
	)
	ings, err := Ingresses(context.Background(), client, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ings) != 1 || ings[0].Name != "web" {
		t.Errorf("want 1 ingress web, got %+v", ings)
	}
}
```

If `collect_test.go` does not already import `networkingv1 "k8s.io/api/networking/v1"`, add it.

- [ ] **Step 2: Run it to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -run TestIngresses_List`
Expected: FAIL to compile — `undefined: Ingresses`.

- [ ] **Step 3: Add the collect helper**

In `internal/collect/collect.go`, after `IngressClasses`, add:

```go
// Ingresses lists Ingresses in the namespace (empty = all), read-only.
func Ingresses(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.Ingress, error) {
	ings, err := client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingresses: %w", err)
	}
	return ings.Items, nil
}
```

- [ ] **Step 4: Wire into `scan`**

In `internal/scan/scan.go`, add the import `"github.com/imantaba/kubeagent/internal/ingresshealth"`. Add a field to `Result` (after `ServiceIssues`):

```go
	IngressIssues []ingresshealth.RouteIssue
```

In `Evaluate`, right after the existing `serviceIssues := svchealth.Assess(svcs, slices, backends)` line, add:

```go
	ings, _ := collect.Ingresses(ctx, client, opts.Namespace)
	ingressIssues := ingresshealth.Assess(ings, svcs, slices)
```

Add `IngressIssues: ingressIssues` to the returned `Result` literal.

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/collect/ ./internal/scan/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go internal/scan/scan.go
git commit -m "feat(scan): collect Ingresses and assess route health in Result"
```

---

### Task 4: Report — NEEDS ATTENTION ingress lines + JSON + attention count

**Files:**
- Modify: `internal/report/report.go` (`Input`, `inventoryReport`, `printInventoryText`, `attentionLine`, add `printIngressIssues`, import `ingresshealth`)
- Modify: `main.go` (pass `IngressIssues` into `report.Input`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `ingresshealth.RouteIssue` (Task 2); `scan.Result.IngressIssues` (Task 3).
- Produces: `report.Input.IngressIssues []ingresshealth.RouteIssue`.

`report.Input` is a struct, so adding a field needs no callsite changes.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add the `"github.com/imantaba/kubeagent/internal/ingresshealth"` import):

```go
func TestPrintInventory_TextShowsIngressIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{{
			Namespace: "shop", Ingress: "web", Host: "example.com", Path: "/api",
			Service: "api-svc", Port: "8080", Problem: "NoEndpoints",
			Detail: "backend Service api-svc:8080 has no ready endpoints (likely 502/503)",
		}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "NEEDS ATTENTION") {
		t.Errorf("a broken ingress route should trip NEEDS ATTENTION:\n%s", out)
	}
	if !strings.Contains(out, "✗ ingress shop/web") || !strings.Contains(out, "example.com/api") || !strings.Contains(out, "likely 502/503") {
		t.Errorf("missing the ingress route line:\n%s", out)
	}
	if !strings.Contains(out, "Needs attention: 1 ingress route broken") {
		t.Errorf("attention line should count the broken route:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesIngressIssues(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		IngressIssues: []ingresshealth.RouteIssue{{Namespace: "shop", Ingress: "web", Service: "api-svc", Problem: "NoService", Detail: "backend Service api-svc not found"}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"ingressIssues"`) || !strings.Contains(buf.String(), `"problem": "NoService"`) {
		t.Errorf("expected ingressIssues in JSON:\n%s", buf.String())
	}
}

func TestPrintInventory_NoIngressLinesWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ingress") {
		t.Errorf("no ingress lines expected when there are no issues:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty ingress issues must not suppress all-clear:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/report/ -run 'IngressIssues|NoIngressLines'`
Expected: FAIL to compile — `Input` has no `IngressIssues` field.

- [ ] **Step 3: Add the field, JSON, render, and attention count**

In `internal/report/report.go`: add the import `"github.com/imantaba/kubeagent/internal/ingresshealth"`. Add to `Input` (after `DiskUsage`):

```go
	IngressIssues      []ingresshealth.RouteIssue
```

Add to `inventoryReport` (after `DiskUsage`):

```go
	IngressIssues      []ingresshealth.RouteIssue  `json:"ingressIssues,omitempty"`
```

Add `IngressIssues: in.IngressIssues,` to the `inventoryReport{...}` literal in the json branch.

In `printInventoryText`, extend `hasAttention` and render the ingress lines inside the NEEDS ATTENTION block, after `printDiskUsage`:

```go
	hasDisk := in.DiskUsage != nil && len(in.DiskUsage.Over) > 0
	hasAttention := len(in.Result.Workloads) > 0 || len(real) > 0 || len(in.CredentialWarnings) > 0 || hasDisk || len(in.IngressIssues) > 0
	if hasAttention {
		if _, err := fmt.Fprintln(w, "NEEDS ATTENTION"); err != nil {
			return err
		}
		for _, wl := range in.Result.Workloads {
			if err := printWorkload(wl, w); err != nil {
				return err
			}
		}
		if err := printServiceIssues(real, "  ✗", w); err != nil {
			return err
		}
		if err := printCredentialWarnings(in.CredentialWarnings, w); err != nil {
			return err
		}
		if err := printDiskUsage(in.DiskUsage, w); err != nil {
			return err
		}
		if err := printIngressIssues(in.IngressIssues, w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
```

Add the renderer (near `printDiskUsage`):

```go
// printIngressIssues lists Ingress routes whose backend chain is broken.
func printIngressIssues(issues []ingresshealth.RouteIssue, w io.Writer) error {
	for _, r := range issues {
		line := fmt.Sprintf("  ✗ ingress %s/%s", r.Namespace, r.Ingress)
		if route := r.Host + r.Path; route != "" {
			line += "  " + route
		}
		line += "  " + r.Detail
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
```

In `attentionLine`, after the disk term, add the ingress term:

```go
	if n := len(in.IngressIssues); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ingress %s broken", n, plural(n, "route", "routes")))
	}
```

- [ ] **Step 4: Update the `main.go` caller**

In `main.go`, in the `report.Input{...}` literal (near `DiskUsage: diskRep,`), add:

```go
		IngressIssues:      res.IngressIssues,
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (3 new tests + existing unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): show broken ingress routes in NEEDS ATTENTION + ingressIssues JSON"
```

---

### Task 5: Daemon gauge `kubeagent_ingress_route_issues`

**Files:**
- Modify: `internal/watch/metrics.go` (`metrics` struct, `update`, `render`)
- Modify: `internal/watch/metrics_test.go`

**Interfaces:**
- Consumes: `scan.Result.IngressIssues` (Task 3).

- [ ] **Step 1: Write the failing test**

In `internal/watch/metrics_test.go`, extend `sampleResult()` with an ingress issue and assert the gauge. Add to the `sampleResult` `scan.Result{...}` literal (it already imports the needed packages; add `"github.com/imantaba/kubeagent/internal/ingresshealth"`):

```go
		IngressIssues: []ingresshealth.RouteIssue{{Namespace: "shop", Ingress: "web", Service: "api-svc", Problem: "NoEndpoints"}},
```

Add to the `want` list in `TestMetrics_RenderReflectsResult`:

```go
		"kubeagent_ingress_route_issues 1",
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/watch/ -run TestMetrics_RenderReflectsResult`
Expected: FAIL — gauge missing (and compile error until the field exists).

- [ ] **Step 3: Add the field, update, and gauge**

In `internal/watch/metrics.go`, add to the `metrics` struct (after `serviceIssues int`):

```go
	ingressIssues int
```

In `update`, after `m.serviceIssues = len(res.ServiceIssues)`, add:

```go
	m.ingressIssues = len(res.IngressIssues)
```

In `render`, after the `kubeagent_service_issues` gauge line, add:

```go
	gauge("kubeagent_ingress_route_issues", "Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port", float64(m.ingressIssues))
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/watch/`
Expected: PASS (the error-path last-good test still holds; the new field rides the success-path snapshot).

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): expose kubeagent_ingress_route_issues gauge"
```

---

### Task 6: RBAC — grant `ingresses` read

**Files:**
- Modify: `deploy/rbac.yaml` (networking.k8s.io rule, ~25)
- Modify: `deploy/helm/kubeagent/templates/clusterrole.yaml` (networking.k8s.io rule, ~23)

**Interfaces:** none (manifests).

- [ ] **Step 1: Update `deploy/rbac.yaml`**

Change the `networking.k8s.io` rule's resources to add `ingresses`:

```yaml
  - apiGroups: ["networking.k8s.io"]
    resources: [networkpolicies, ingressclasses, ingresses]
    verbs: [get, list, watch]
```

- [ ] **Step 2: Update the Helm ClusterRole**

In `deploy/helm/kubeagent/templates/clusterrole.yaml`, make the same change to the `networking.k8s.io` rule:

```yaml
  - apiGroups: ["networking.k8s.io"]
    resources: [networkpolicies, ingressclasses, ingresses]
    verbs: [get, list, watch]
```

- [ ] **Step 3: Verify the chart renders read-only**

Run:

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -A2 'networking.k8s.io'
helm template x deploy/helm/kubeagent | grep -iE 'create|update|patch|delete' | grep -i verb && echo BAD || echo "read-only OK"
```

Expected: lint clean; the rendered networking rule lists `ingresses` with verbs `[get, list, watch]`; the write-verb check prints `read-only OK`.

- [ ] **Step 4: Commit**

```bash
git add deploy/rbac.yaml deploy/helm/kubeagent/templates/clusterrole.yaml
git commit -m "feat(rbac): grant read-only ingresses for route health"
```

---

### Task 7: Docs

**Files:**
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/features/watch-mode.md`
- Modify: `website/docs/roadmap.md`
- Modify: `README.md`

**Interfaces:** none. Use exact names: JSON `ingressIssues`; Problem values `NoService`/`NoEndpoints`/`PortNotExposed`; gauge `kubeagent_ingress_route_issues`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry with:

```markdown
## [Unreleased]

### Added

- **Ingress route health.** `scan` now resolves each Ingress rule's backend
  Service and flags broken routes — the backend Service is missing (`NoService`),
  has no ready endpoints (`NoEndpoints`, the classic 502/503), or does not expose
  the referenced port (`PortNotExposed`) — in the NEEDS ATTENTION section and JSON
  `ingressIssues`, with the watch-daemon gauge `kubeagent_ingress_route_issues`.
  This turns "why is my ingress returning 502?" into a concrete cause. Reads
  Ingresses (a new read-only RBAC grant); advisory (does not change the cluster
  verdict).
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add a subsection after the existing service/disk material, matching the heading level/style:

```markdown
### Ingress route health

`scan` walks every Ingress rule (and default backend) and follows the route to
its backend Service. It flags a route when the Service is missing, has no ready
endpoints (the usual cause of a 502/503), or does not expose the referenced
port — so a broken public route reads as, e.g., `ingress shop/web
example.com/api backend Service api-svc:8080 has no ready endpoints (likely
502/503)`. Only Service backends are checked (Resource backends are skipped), and
routes resolve within the Ingress's own namespace. It is read-only and advisory:
it appears in **NEEDS ATTENTION** and JSON `ingressIssues` but does not change
the cluster verdict.
```

- [ ] **Step 3: watch-mode.md**

Run: `grep -nE 'kubeagent_|metric' website/docs/features/watch-mode.md | head -30`

Add `kubeagent_ingress_route_issues` to the documented metrics in the neighbouring format, described as "Number of Ingress routes whose backend Service is missing, has no ready endpoints, or does not expose the referenced port."

- [ ] **Step 4: roadmap.md**

Run: `grep -nE 'Shipped|Version history' website/docs/roadmap.md | head`

Add a bullet to the "Shipped" list (before the `!!! info "Version history"` block), matching the existing bullet style:

```markdown
- **Ingress route health** — `scan` follows each Ingress rule to its backend
  Service and flags routes whose Service is missing, has no ready endpoints, or
  does not expose the referenced port — the usual causes of a 502/503 — in
  NEEDS ATTENTION, JSON `ingressIssues`, and the daemon gauge
  `kubeagent_ingress_route_issues`. See [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 5: README**

Run: `grep -nE 'disk-usage|node reservation|PVC reclaim|detect' README.md | head`

Add a one-line mention of the ingress route-health check alongside the existing feature list, matching the surrounding style.

- [ ] **Step 6: Verify the website builds**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/2cf9d068-6b9a-43b4-a854-5fe7b5f3453e/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: "Documentation built" with no strict WARNING lines about the edited pages.

- [ ] **Step 7: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/features/watch-mode.md website/docs/roadmap.md README.md
git commit -m "docs: document ingress route health and its daemon gauge"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke against any cluster with a broken ingress (a route whose backend has no ready pods):

```bash
go build -o kubeagent . && ./kubeagent scan --output text | sed -n '/NEEDS ATTENTION/,/^$/p'
./kubeagent scan --output json | grep -o '"ingressIssues"'
```

Expected: a `✗ ingress <ns>/<name> <host><path> …no ready endpoints (likely 502/503)` line in NEEDS ATTENTION; `ingressIssues` present in JSON when a route is broken.
