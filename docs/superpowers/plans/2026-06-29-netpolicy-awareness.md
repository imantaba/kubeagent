# NetworkPolicy Awareness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a workload is flagged with no detector finding, name the NetworkPolicies whose podSelector matches its pods — a root-cause hint for the chaos #4 "mysterious degraded" case — rendered on the workload in text/JSON/`--explain`.

**Architecture:** A new pure package `internal/netpolicy` annotates assembled workloads with the names of selecting NetworkPolicies (stored in a new `Workload.NetworkPolicies` field). `internal/collect` lists NetworkPolicies (namespace-scoped). The hint rides on the `Workload`, so report/explain render it with no signature changes; `main.go` wires the annotation step after prioritization.

**Tech Stack:** Go 1.26, client-go, `k8s.io/api/networking/v1` (already used), `k8s.io/apimachinery/pkg/apis/meta/v1` + `.../labels` (already present), stdlib `sort`/`strings`.

## Global Constraints

- **READ-ONLY:** one new List call (NetworkPolicies). No create/update/patch/delete.
- **No new Go module dependency.** `networking/v1` is already imported (IngressClass); selector matching uses apimachinery's `LabelSelectorAsSelector` + `labels.Set`.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Namespace scope:** NetworkPolicies are namespaced; the check honors the scan's `-n` (pass `namespace` through).
- **`--explain` egress:** only NetworkPolicy **names** — never pod/endpoint IPs, selector values, raw specs, or secrets.
- **Best-effort:** a List failure in `main` is non-fatal (nil policies → no hints).
- **Trigger:** annotate only workloads where `w.Flagged() && len(w.Findings) == 0` and a same-namespace NetworkPolicy's podSelector matches a pod.
- **Matching:** `metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)`; an empty podSelector matches all pods (deny-all). A malformed selector is skipped. A pod missing from `podLabels` is treated as having empty labels.
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` before any `go` command. Run `gofmt -l` on touched files; fix with `gofmt -w`.
- **Scope (YAGNI):** list selecting NP names only — no rule/traffic analysis, no default-deny grading, no `namespaceSelector`/CNI-enforcement checks.

---

### Task 1: `internal/netpolicy` — annotate workloads with selecting NPs (pure)

**Files:**
- Modify: `internal/inventory/inventory.go` (add one `Workload` field)
- Create: `internal/netpolicy/netpolicy.go`
- Test: `internal/netpolicy/netpolicy_test.go`

**Interfaces:**
- Produces:
  - `Workload.NetworkPolicies []string` (`json:"networkPolicies,omitempty"`)
  - `func Annotate(workloads []inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy)`

- [ ] **Step 1: Add the `Workload` field**

In `internal/inventory/inventory.go`, add to the `Workload` struct (after the `Priority` field, before the closing `}`):

```go
	NetworkPolicies []string `json:"networkPolicies,omitempty"` // names of NPs selecting this workload's pods (hint; set by netpolicy.Annotate)
```

- [ ] **Step 2: Write the failing test**

Create `internal/netpolicy/netpolicy_test.go`:

```go
package netpolicy

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func np(ns, name string, sel map[string]string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: sel}},
	}
}

// degraded builds a flagged (ready<desired), finding-less Deployment with one pod.
func degraded(ns, name, podName string) inventory.Workload {
	return inventory.Workload{
		Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
		Pods: []inventory.PodRow{{Name: podName, Phase: "Running", Ready: "0/1"}},
	}
}

func TestAnnotate_FlaggedNoFindingsSelected(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{
		np("default", "deny-all", nil),                            // empty selector → matches all
		np("default", "allow-api", map[string]string{"app": "api"}), // matches app=api
	}
	Annotate(ws, podLabels, pols)
	got := ws[0].NetworkPolicies
	if len(got) != 2 || got[0] != "allow-api" || got[1] != "deny-all" {
		t.Fatalf("got %+v, want [allow-api deny-all] (sorted)", got)
	}
}

func TestAnnotate_SkipsWhenHasFinding(t *testing.T) {
	w := degraded("default", "api", "api-1")
	w.Findings = []diagnose.Finding{{Pod: "default/api-1", Issue: "OOMKilled"}}
	ws := []inventory.Workload{w}
	Annotate(ws, map[string]map[string]string{"default/api-1": {"app": "api"}}, []networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a workload with a detector finding must not get an NP hint, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_SkipsHealthy(t *testing.T) {
	healthy := inventory.Workload{
		Namespace: "default", Name: "web", Kind: "Deployment", Desired: 1, Ready: 1, Status: "Running",
		Pods: []inventory.PodRow{{Name: "web-1"}},
	}
	ws := []inventory.Workload{healthy}
	Annotate(ws, map[string]map[string]string{"default/web-1": {"app": "web"}}, []networkingv1.NetworkPolicy{np("default", "deny-all", nil)})
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a healthy workload must not get an NP hint, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_LabelMismatchNoHint(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{np("default", "for-web", map[string]string{"app": "web"})}
	Annotate(ws, podLabels, pols)
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a non-matching policy must not be listed, got %+v", ws[0].NetworkPolicies)
	}
}

func TestAnnotate_CrossNamespaceNoHint(t *testing.T) {
	ws := []inventory.Workload{degraded("default", "api", "api-1")}
	podLabels := map[string]map[string]string{"default/api-1": {"app": "api"}}
	pols := []networkingv1.NetworkPolicy{np("other", "deny-all", nil)} // different namespace
	Annotate(ws, podLabels, pols)
	if ws[0].NetworkPolicies != nil {
		t.Errorf("a policy in another namespace must not match, got %+v", ws[0].NetworkPolicies)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/netpolicy/`
Expected: FAIL — package has no non-test files / `undefined: Annotate`.

- [ ] **Step 4: Write minimal implementation**

Create `internal/netpolicy/netpolicy.go`:

```go
// Package netpolicy annotates workloads with the names of NetworkPolicies that
// select their pods — a root-cause hint for a degraded workload with no known
// detector cause. It is pure; the caller supplies workloads, pod labels, and
// policies.
package netpolicy

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/imantaba/kubeagent/internal/inventory"
)

// Annotate sets w.NetworkPolicies for each workload that is flagged with no
// detector finding and whose pods are selected by one or more NetworkPolicies in
// the same namespace. podLabels maps "namespace/podName" to that pod's labels.
// It mutates the slice elements in place.
func Annotate(workloads []inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) {
	for i := range workloads {
		w := workloads[i]
		if !w.Flagged() || len(w.Findings) > 0 {
			continue
		}
		if names := selectingPolicies(w, podLabels, policies); len(names) > 0 {
			workloads[i].NetworkPolicies = names
		}
	}
}

// selectingPolicies returns the sorted, de-duplicated names of NetworkPolicies in
// the workload's namespace whose podSelector matches any of its pods.
func selectingPolicies(w inventory.Workload, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy) []string {
	set := map[string]bool{}
	for _, p := range policies {
		if p.Namespace != w.Namespace {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(&p.Spec.PodSelector)
		if err != nil {
			continue // malformed selector — skip defensively
		}
		for _, pr := range w.Pods {
			if sel.Matches(labels.Set(podLabels[w.Namespace+"/"+pr.Name])) {
				set[p.Name] = true
				break
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/netpolicy/ -v && go vet ./internal/netpolicy/ ./internal/inventory/ && gofmt -l internal/netpolicy/ internal/inventory/`
Expected: all tests PASS, vet clean, gofmt prints nothing. (Existing inventory tests still pass — the new field is additive.)

- [ ] **Step 6: Commit**

```bash
git add internal/netpolicy/ internal/inventory/inventory.go
git commit -m "feat(netpolicy): annotate finding-less degraded workloads with selecting NetworkPolicies"
```

---

### Task 2: `internal/collect` — list NetworkPolicies

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func NetworkPolicies(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.NetworkPolicy, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/collect/collect_test.go` (it already imports `networkingv1 "k8s.io/api/networking/v1"`, `context`, `testing`, `metav1`, and `k8s.io/client-go/kubernetes/fake`):

```go
func TestNetworkPolicies_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny-all"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "allow-web"}},
	)
	nps, err := NetworkPolicies(context.Background(), client, "")
	if err != nil {
		t.Fatalf("NetworkPolicies: %v", err)
	}
	if len(nps) != 2 {
		t.Errorf("want 2 network policies, got %d", len(nps))
	}
}

func TestNetworkPolicies_NamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "deny-all"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "allow-web"}},
	)
	nps, err := NetworkPolicies(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("NetworkPolicies: %v", err)
	}
	if len(nps) != 1 || nps[0].Namespace != "a" {
		t.Errorf("want only namespace a, got %+v", nps)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/`
Expected: FAIL — `undefined: NetworkPolicies`.

- [ ] **Step 3: Write minimal implementation**

In `internal/collect/collect.go` append (the `networkingv1` import already exists):

```go
// NetworkPolicies lists NetworkPolicies in the namespace (empty = all), read-only.
func NetworkPolicies(ctx context.Context, client kubernetes.Interface, namespace string) ([]networkingv1.NetworkPolicy, error) {
	nps, err := client.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing networkpolicies: %w", err)
	}
	return nps.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -v && go vet ./internal/collect/ && gofmt -l internal/collect/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list NetworkPolicies (namespace-scoped)"
```

---

### Task 3: `internal/report` — render the NetworkPolicy hint

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `inventory.Workload.NetworkPolicies`. No signature change.

- [ ] **Step 1: Write the failing test**

Append to `internal/report/report_test.go`:

```go
func TestPrintInventory_TextShowsNetworkPolicyHint(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "default", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
		NetworkPolicies: []string{"deny-all", "web-allow"},
		Pods:            []inventory.PodRow{{Name: "api-1", Phase: "Running", Ready: "0/1"}},
	}}
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: ws}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "NetworkPolicy: pods selected by deny-all, web-allow") {
		t.Errorf("expected NP hint line:\n%s", buf.String())
	}
}

func TestPrintInventory_TextNoNetworkPolicyHintWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(clusterhealth.ClusterHealth{}, inventory.Result{Workloads: sampleWorkloads()}, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "NetworkPolicy:") {
		t.Errorf("no NP hint expected when the workload has none:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run NetworkPolicy`
Expected: FAIL — the hint line is not yet rendered.

- [ ] **Step 3: Write minimal implementation**

In `internal/report/report.go`, in `printWorkload`, immediately AFTER the `for _, f := range wl.Findings { … }` loop and BEFORE the `for _, p := range wl.Pods { … }` loop, insert:

```go
	if len(wl.NetworkPolicies) > 0 {
		if _, err := fmt.Fprintf(w, "    ⚠ NetworkPolicy: pods selected by %s — may be blocking traffic\n", strings.Join(wl.NetworkPolicies, ", ")); err != nil {
			return err
		}
	}
```

(`report.go` already imports `strings`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -v && go vet ./internal/report/ && gofmt -l internal/report/`
Expected: PASS (new + all existing), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): render the NetworkPolicy hint under a workload"
```

---

### Task 4: `internal/explain` — NetworkPolicy hint in the prompt

**Files:**
- Modify: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `inventory.Workload.NetworkPolicies`. No signature change.

- [ ] **Step 1: Write the failing test**

Append to `internal/explain/explain_test.go`:

```go
func TestBuildInventoryPrompt_IncludesNetworkPolicyHint(t *testing.T) {
	ws := []inventory.Workload{{
		Namespace: "default", Name: "api", Kind: "Deployment", Ready: 0, Desired: 2, Status: "Degraded",
		NetworkPolicies: []string{"deny-all"},
	}}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil, ws)
	if !strings.Contains(got, "network policy: pods selected by deny-all (possible cause)") {
		t.Errorf("prompt missing NP hint:\n%s", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -run NetworkPolicy`
Expected: FAIL — the hint is not yet in the prompt.

- [ ] **Step 3: Write minimal implementation**

In `internal/explain/explain.go`, in `buildInventoryPrompt`, inside the `for _, w := range workloads` loop, immediately AFTER the `for _, f := range w.Findings { … }` loop and before that workload iteration's closing `}`, insert:

```go
			if len(w.NetworkPolicies) > 0 {
				fmt.Fprintf(&b, "    network policy: pods selected by %s (possible cause)\n", strings.Join(w.NetworkPolicies, ", "))
			}
```

(`explain.go` already imports `strings`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -v && go vet ./internal/explain/ && gofmt -l internal/explain/`
Expected: PASS (new + all existing incl. the egress-guard test), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): include the NetworkPolicy hint in the prompt"
```

---

### Task 5: wire `main.go`, document, CHANGELOG

**Files:**
- Modify: `main.go`
- Modify: `README.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: `collect.NetworkPolicies`, `netpolicy.Annotate`.

- [ ] **Step 1: Wire main.go**

Add `"github.com/imantaba/kubeagent/internal/netpolicy"` to imports. After the `result := inventory.Prioritize(...)` block and BEFORE the `--explain` call, insert:

```go
	nps, _ := collect.NetworkPolicies(context.Background(), client, namespace)
	podLabels := make(map[string]map[string]string, len(inputs.Pods))
	for _, p := range inputs.Pods {
		podLabels[p.Namespace+"/"+p.Name] = p.Labels
	}
	netpolicy.Annotate(result.Workloads, podLabels, nps)
```

(No call-site signature changes — the hint rides on `result.Workloads`, which both `ExplainInventory` and `report.PrintInventory` already receive.)

- [ ] **Step 2: Build, vet, gofmt, full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && gofmt -l main.go && go build -o /tmp/kubeagent .`
Expected: all packages `ok`, `gofmt -l main.go` prints nothing, build succeeds.

- [ ] **Step 3: Document in README.md**

Add a `### NetworkPolicy hints` subsection in the scan/usage area (before `## Install`):

```markdown
### NetworkPolicy hints

When a workload is degraded with no detector finding (e.g. pods Running but never
Ready), `scan` names the NetworkPolicies whose podSelector matches its pods —
`⚠ NetworkPolicy: pods selected by deny-all — may be blocking traffic` — and sends
the names to `--explain`. It is a hint, not a verdict: kubeagent does not analyze
the policy rules or know what traffic the pod needs, so it points you at the
policies to check. Read-only, namespace-scoped; only policy names are sent to the
model. (Note: some CNIs, e.g. kindnet, do not enforce NetworkPolicies at all.)
```

- [ ] **Step 4: Update CHANGELOG.md**

Under `## [Unreleased]` → `### Added`, add a bullet, and REMOVE the NetworkPolicy bullet from `### Planned` (leave the other two Planned bullets intact):

```markdown
- **NetworkPolicy awareness.** A degraded workload with no detector finding is
  annotated with the NetworkPolicies selecting its pods (a root-cause hint), in
  text, JSON, and `--explain`.
```

- [ ] **Step 5: Commit**

```bash
git add main.go README.md CHANGELOG.md
git commit -m "feat: wire NetworkPolicy hints into scan; document + changelog"
```

---

## Self-Review

**Spec coverage:**
- `Workload.NetworkPolicies` field + `netpolicy.Annotate` (trigger: flagged + no findings; empty-selector matches all; namespace match; sorted/deduped; malformed-selector skip) → Task 1. ✓
- collect NetworkPolicies (namespace-scoped) → Task 2. ✓
- report hint sub-line → Task 3. ✓
- explain hint line (names only, egress-safe) → Task 4. ✓
- main wiring (best-effort, podLabels from inputs.Pods, after Prioritize) + README + CHANGELOG → Task 5. ✓

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `Workload.NetworkPolicies []string`, `Annotate(workloads, podLabels map[string]map[string]string, policies []networkingv1.NetworkPolicy)`, and the `strings.Join(..., ", ")` rendering are used identically across Tasks 1–5. report/explain read the field with no signature change; main builds `podLabels` keyed by `namespace/podName` exactly as `selectingPolicies` looks it up.
