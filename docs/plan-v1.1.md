# kubeagent v1.1 — Implementation Plan: `--context` and `-n/--namespace`

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--context` flag (select kubeconfig context) and a `-n/--namespace` flag (scope the scan to one namespace) to `kubeagent scan`, both backward compatible.

**Architecture:** Thread a context name through `cluster.NewClient` (via client-go's `ConfigOverrides`) and a namespace through `collect.Cluster` (via `Pods(namespace)`), then wire both as flags in `main.go`. Empty values preserve v1 behavior exactly.

**Tech Stack:** Go 1.26, stdlib `flag`, `k8s.io/client-go` (`clientcmd`), existing test patterns (fake clientset, temp-file fixtures).

## Global Constraints

- Module path: `github.com/imantaba/kubeagent`; Go 1.26.
- **Read-only**: only `List`/`Get`-style calls; never mutate cluster resources.
- CLI: standard-library `flag` only — no Cobra.
- Sequential — no goroutines.
- Exit codes: `0` ran successfully; `1` tool failed.
- **Backward compatible**: empty `--context` = kubeconfig current-context; empty `-n` = all namespaces (both = v1 behavior).
- No new module dependencies.

---

## File Structure

- `internal/cluster/client.go` — `NewClient` gains a `contextName` parameter; switch from `BuildConfigFromFlags` to deferred-loading + overrides.
- `internal/cluster/client_test.go` — update existing call; add a temp-kubeconfig fixture test for context selection.
- `internal/collect/collect.go` — `Cluster` gains a `namespace` parameter.
- `internal/collect/collect_test.go` — update existing call; add a namespace-scoped test.
- `main.go` — add `--context`, `--namespace`/`-n`; wire them; update usage string.
- `README.md` — usage block.
- `docs/go-concepts.md` — one entry on the `-n` shorthand idiom.

---

## Task 1: `cluster.NewClient` — context override

**Files:**
- Modify: `internal/cluster/client.go`
- Test: `internal/cluster/client_test.go`

**Interfaces:**
- Produces: `func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error)`.
- `resolveKubeconfig(string) (string, error)` is unchanged.

- [ ] **Step 1: Update the failing test (and add the context fixture)**

Replace the body of `internal/cluster/client_test.go` with:

```go
package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveKubeconfig_PrefersExplicitPath(t *testing.T) {
	got, err := resolveKubeconfig("/tmp/my.kubeconfig")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tmp/my.kubeconfig" {
		t.Errorf("got %q, want the explicit path", got)
	}
}

func TestResolveKubeconfig_FallsBackToEnv(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/env.kubeconfig")
	got, err := resolveKubeconfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tmp/env.kubeconfig" {
		t.Errorf("got %q, want the KUBECONFIG value", got)
	}
}

func TestNewClient_BadPathReturnsError(t *testing.T) {
	if _, err := NewClient("/nonexistent/kubeconfig", ""); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}

// twoContextKubeconfig writes a minimal kubeconfig with contexts "alpha" and
// "beta" (current-context: alpha) and returns its path.
func twoContextKubeconfig(t *testing.T) string {
	t.Helper()
	const cfg = `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: c-alpha
  cluster:
    server: https://alpha.example:6443
    insecure-skip-tls-verify: true
- name: c-beta
  cluster:
    server: https://beta.example:6443
    insecure-skip-tls-verify: true
contexts:
- name: alpha
  context: {cluster: c-alpha, user: u-alpha}
- name: beta
  context: {cluster: c-beta, user: u-beta}
users:
- name: u-alpha
  user: {token: fake-alpha}
- name: u-beta
  user: {token: fake-beta}
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestNewClient_SelectsNamedContext(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, "beta"); err != nil {
		t.Errorf("expected success selecting context %q, got %v", "beta", err)
	}
}

func TestNewClient_UnknownContextErrors(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, "ghost"); err == nil {
		t.Error("expected an error for a non-existent context, got nil")
	}
}

func TestNewClient_EmptyContextUsesCurrent(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, ""); err != nil {
		t.Errorf("expected success using current-context, got %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cluster/ -run 'TestNewClient_SelectsNamedContext|TestNewClient_UnknownContextErrors|TestNewClient_EmptyContextUsesCurrent' 2>&1 | tail -8
```
Expected: FAIL — compile error: `NewClient` called with 2 args but defined with 1 (plus the new tests fail).

- [ ] **Step 3: Update the implementation**

Replace `internal/cluster/client.go` with:

```go
package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes clientset from a kubeconfig file.
// If kubeconfigPath is empty, it falls back to $KUBECONFIG, then ~/.kube/config.
// If contextName is empty, the kubeconfig's current-context is used.
func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = path
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q (context %q): %w", path, contextName, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
}

func resolveKubeconfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory for default kubeconfig: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cluster/ -v 2>&1 | tail -16
go vet ./internal/cluster/
```
Expected: PASS — all six tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/cluster/client.go internal/cluster/client_test.go
git commit -m "feat(cluster): support selecting a kubeconfig context"
```

---

## Task 2: `collect.Cluster` — namespace scoping

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func Cluster(ctx context.Context, client kubernetes.Interface, namespace string) ([]diagnose.PodFacts, error)`.
- Empty namespace = all namespaces.

- [ ] **Step 1: Update the failing test (add namespace cases)**

Replace `internal/collect/collect_test.go` with:

```go
package collect

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCluster_ReturnsFactsForAllPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "p2"}},
	)

	facts, err := Cluster(context.Background(), client, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 pod facts, got %d", len(facts))
	}
	if facts[0].Pod == nil || facts[0].Pod.Name == "" {
		t.Error("expected each fact to carry a non-empty Pod")
	}
}

func TestCluster_EmptyClusterReturnsNoFacts(t *testing.T) {
	facts, err := Cluster(context.Background(), fake.NewSimpleClientset(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
}

func TestCluster_ScopesToNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p1"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "p2"}},
	)

	facts, err := Cluster(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 pod fact in namespace a, got %d", len(facts))
	}
	if facts[0].Pod.Namespace != "a" {
		t.Errorf("got pod in namespace %q, want a", facts[0].Pod.Namespace)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect/ -run TestCluster_ScopesToNamespace 2>&1 | tail -8
```
Expected: FAIL — compile error: `Cluster` called with 3 args but defined with 2.

- [ ] **Step 3: Update the implementation**

In `internal/collect/collect.go`, change the signature and the List call:

```go
// Cluster lists pods (in the given namespace, or all namespaces when namespace
// is empty) and wraps each in PodFacts. It is read-only: a single List call,
// never mutating anything.
func Cluster(ctx context.Context, client kubernetes.Interface, namespace string) ([]diagnose.PodFacts, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	facts := make([]diagnose.PodFacts, 0, len(pods.Items))
	for i := range pods.Items {
		pod := pods.Items[i] // copy so &pod is stable per iteration
		facts = append(facts, diagnose.PodFacts{Pod: &pod})
	}
	return facts, nil
}
```

(Only the doc comment, the signature's new `namespace string` parameter, and `Pods(namespace)` change; the rest is unchanged.)

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collect/ -v 2>&1 | tail -10
go vet ./internal/collect/
```
Expected: PASS — all three tests; vet clean.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/collect.go internal/collect/collect_test.go
git commit -m "feat(collect): scope pod listing to a namespace"
```

---

## Task 3: `main.go` wiring + docs

**Files:**
- Modify: `main.go`
- Modify: `README.md`
- Modify: `docs/go-concepts.md`

**Interfaces:**
- Consumes: `cluster.NewClient(kubeconfig, contextName)` (Task 1), `collect.Cluster(ctx, client, namespace)` (Task 2).

- [ ] **Step 1: Update the implementation**

Replace the `run` function in `main.go` with:

```go
func run(args []string) error {
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--context name] [-n namespace] [--output text|json]")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	output := fs.String("output", "text", "output format: text | json")
	var namespace string
	fs.StringVar(&namespace, "namespace", "", "namespace to scan (default: all namespaces)")
	fs.StringVar(&namespace, "n", "", "namespace to scan (shorthand)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// Validate format up front so we fail fast, before touching the network.
	if *output != "text" && *output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", *output)
	}

	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}
	facts, err := collect.Cluster(context.Background(), client, namespace)
	if err != nil {
		return err
	}

	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
	}
	findings := diagnose.Run(detectors, facts)

	return report.Print(findings, *output, os.Stdout)
}
```

(The `main` function and imports are unchanged.)

- [ ] **Step 2: Run the existing arg tests + build**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestRun -v 2>&1 | tail -8
go build -o kubeagent .
./kubeagent scan --help 2>&1 | head -8 || true
```
Expected: the three `TestRun_*` tests PASS (validation order unchanged); binary builds; `--help` lists `--context`, `--namespace`, `-n`, `--kubeconfig`, `--output`.

- [ ] **Step 3: Run the full suite**

```bash
go test ./... 2>&1
go vet ./... 2>&1
```
Expected: all packages PASS; vet clean.

- [ ] **Step 4: Update `README.md` usage block**

Replace the Usage code block in `README.md` with:

```bash
go build -o kubeagent .

# scan the whole cluster (uses $KUBECONFIG or ~/.kube/config, current-context)
./kubeagent scan

# pick a context and scope to one namespace, emit JSON
./kubeagent scan --context my-cluster -n my-namespace --output json
```

- [ ] **Step 5: Add a `go-concepts.md` entry**

Add before "## Coming later" in `docs/go-concepts.md`:

```markdown
## 12. A short flag alias with the stdlib `flag` package

Go's standard `flag` package has no built-in "long and short" option. The idiom
is to bind **two flag names to the same variable** with `StringVar` — whichever
the user passes writes to that one variable.

**Simple example:**

```go
var name string
fs.StringVar(&name, "name", "", "your name")
fs.StringVar(&name, "n", "", "your name (shorthand)")
// `--name ann` and `-n ann` both set name = "ann"
```

**kubeagent example:** `--namespace` and `-n` both set the scan namespace:

```go
var namespace string
fs.StringVar(&namespace, "namespace", "", "namespace to scan (default: all namespaces)")
fs.StringVar(&namespace, "n", "", "namespace to scan (shorthand)")
```
```

- [ ] **Step 6: Commit**

```bash
git add main.go README.md docs/go-concepts.md
git commit -m "feat: add --context and -n/--namespace flags to scan; docs"
```

---

## Self-review

- **Spec coverage:** `--context` via override (Task 1) ✅; `-n/--namespace` scoping (Task 2 + wiring Task 3) ✅; backward compatibility — empty context uses current (Task 1 `TestNewClient_EmptyContextUsesCurrent`), empty namespace lists all (Task 2 `TestCluster_ReturnsFactsForAllPods`) ✅; usage string + README + go-concepts (Task 3) ✅; no new deps (uses existing `clientcmd`) ✅; read-only (still a single `List`) ✅.
- **Placeholder scan:** none — all steps carry complete code.
- **Type consistency:** `NewClient(kubeconfigPath, contextName string)` defined in Task 1 matches its call in Task 3; `Cluster(ctx, client, namespace string)` defined in Task 2 matches its call in Task 3; `namespace` is a `string` var passed by value throughout.
