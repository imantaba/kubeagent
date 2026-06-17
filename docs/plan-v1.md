# kubeagent v1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a read-only Go CLI that scans a whole Kubernetes cluster and reports pods stuck in CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, or Pending/Unschedulable.

**Architecture:** A one-directional pipeline split into focused packages — `cluster` (connect) → `collect` (list pods) → `diagnose` (a `Detector` interface with one struct per failure mode) → `report` (text/JSON). The diagnosis logic is pure functions over in-memory structs, so it is unit-tested with fake pods and no cluster.

**Tech Stack:** Go 1.26, standard library `flag`, `k8s.io/client-go` (typed clientset), `k8s.io/api` + `k8s.io/apimachinery` (Pod types), Go's built-in `testing`.

## Global Constraints

- Module path: `github.com/imantaba/kubeagent` — every internal import starts with this.
- Go version: `1.26` (toolchain at `/usr/local/go/bin/go`).
- **Read-only:** only `List`/`Get`-style calls. Never create, update, patch, or delete cluster resources.
- CLI framework: **standard-library `flag` only** for v1. No Cobra yet.
- Concurrency: **none** in v1 — sequential for clarity. Goroutines are a documented later extension.
- Exit codes: `0` = ran successfully (issues found or not); `1` = the tool itself failed.
- Learning companion: when a task introduces a new Go concept (JSON, `context.Context`, the fake clientset), append a short entry to `docs/go-concepts.md` in the established "simple example → kubeagent example" style.

---

## File Structure

- `main.go` — package main: arg/flag parsing, validation, pipeline wiring, exit codes.
- `internal/diagnose/diagnose.go` — `PodFacts`, `Finding`, `Detector` interface, `Run`.
- `internal/diagnose/crashloop.go` — `CrashLoopDetector`.
- `internal/diagnose/imagepull.go` — `ImagePullDetector`.
- `internal/diagnose/oomkilled.go` — `OOMKilledDetector`.
- `internal/diagnose/pending.go` — `PendingDetector`.
- `internal/diagnose/helpers_test.go` — shared fake-pod builders for detector tests.
- `internal/cluster/client.go` — `NewClient`, kubeconfig resolution.
- `internal/collect/collect.go` — `Cluster` (list pods → `[]PodFacts`).
- `internal/report/report.go` — `Print` (text + JSON).
- `testdata/broken-pods.yaml` — manifests to reproduce each failure mode for manual verification.

---

## Task 1: Project bootstrap — Go on PATH, module, buildable `main`

**Files:**
- Create: `main.go`
- Create: `go.mod` (via `go mod init`)

**Interfaces:**
- Consumes: nothing.
- Produces: a compiling `main` package and a `go.mod` declaring module `github.com/imantaba/kubeagent`.

- [ ] **Step 1: Put Go on your PATH (persistent)**

```bash
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
export PATH=$PATH:/usr/local/go/bin
go version
```
Expected: `go version go1.26.4 linux/amd64`

- [ ] **Step 2: Initialize the module**

```bash
cd ~/git/kubeagent
go mod init github.com/imantaba/kubeagent
```
Expected: creates `go.mod` containing `module github.com/imantaba/kubeagent` and `go 1.26`.

- [ ] **Step 3: Write a minimal `main.go`**

```go
package main

import "fmt"

func main() {
	fmt.Println("kubeagent: a read-only Kubernetes troubleshooting tool")
}
```

- [ ] **Step 4: Build and run to verify the toolchain works**

```bash
go build ./... && go run .
```
Expected: prints `kubeagent: a read-only Kubernetes troubleshooting tool`, no build errors.

- [ ] **Step 5: Commit**

```bash
git add go.mod main.go
git commit -m "chore: initialize Go module and buildable main"
```

---

## Task 2: Diagnosis core — `PodFacts`, `Finding`, `Detector`, `Run`

**Files:**
- Create: `internal/diagnose/diagnose.go`
- Test: `internal/diagnose/diagnose_test.go`

**Interfaces:**
- Consumes: `k8s.io/api/core/v1` (`corev1.Pod`, `corev1.Event`).
- Produces:
  - `type PodFacts struct { Pod *corev1.Pod; Events []corev1.Event }`
  - `type Finding struct { Pod, Issue, Reason, Evidence string }` (all JSON-tagged)
  - `type Detector interface { Detect(facts PodFacts) *Finding }`
  - `func Run(detectors []Detector, facts []PodFacts) []Finding`

- [ ] **Step 1: Write the failing test**

`internal/diagnose/diagnose_test.go`:
```go
package diagnose

import "testing"

// stubDetector lets us test Run without any real pod logic.
type stubDetector struct{ result *Finding }

func (s stubDetector) Detect(facts PodFacts) *Finding { return s.result }

func TestRunCollectsFindingsFromMatchingDetectors(t *testing.T) {
	hit := stubDetector{result: &Finding{Pod: "ns/p", Issue: "X"}}
	miss := stubDetector{result: nil}

	facts := []PodFacts{{}, {}} // two pods

	got := Run([]Detector{hit, miss}, facts)

	if len(got) != 2 {
		t.Fatalf("expected 2 findings (the hit detector fires once per pod), got %d", len(got))
	}
	if got[0].Issue != "X" {
		t.Errorf("Issue = %q, want X", got[0].Issue)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/diagnose/ -run TestRun -v
```
Expected: FAIL — compile error, `undefined: Finding`, `PodFacts`, `Detector`, `Run`.

- [ ] **Step 3: Write the implementation**

`internal/diagnose/diagnose.go`:
```go
package diagnose

import (
	corev1 "k8s.io/api/core/v1"
)

// PodFacts bundles everything a detector needs about one pod.
// Events is populated for forward-compatibility; v1 detectors read Pod only.
type PodFacts struct {
	Pod    *corev1.Pod
	Events []corev1.Event
}

// Finding is one diagnosis: what's wrong with a pod and why.
type Finding struct {
	Pod      string `json:"pod"`      // "namespace/name"
	Issue    string `json:"issue"`    // "CrashLoopBackOff"
	Reason   string `json:"reason"`   // human-readable root cause
	Evidence string `json:"evidence"` // the exact signal observed
}

// Detector inspects one pod's facts and returns a Finding if it matches,
// or nil when the pod does not exhibit this failure mode.
type Detector interface {
	Detect(facts PodFacts) *Finding
}

// Run applies every detector to every pod and collects all findings.
func Run(detectors []Detector, facts []PodFacts) []Finding {
	var findings []Finding
	for _, f := range facts {
		for _, d := range detectors {
			if finding := d.Detect(f); finding != nil {
				findings = append(findings, *finding)
			}
		}
	}
	return findings
}
```

- [ ] **Step 4: Pull the dependency and run the test**

```bash
go mod tidy
go test ./internal/diagnose/ -run TestRun -v
```
Expected: `go mod tidy` adds `k8s.io/api` (and `k8s.io/apimachinery`) to `go.mod`; test PASSES.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/diagnose/diagnose.go internal/diagnose/diagnose_test.go
git commit -m "feat(diagnose): add Detector interface, Finding, and Run"
```

---

## Task 3: CrashLoopBackOff detector

**Files:**
- Create: `internal/diagnose/crashloop.go`
- Create: `internal/diagnose/helpers_test.go` (shared fake-pod builders)
- Test: `internal/diagnose/crashloop_test.go`

**Interfaces:**
- Consumes: `PodFacts`, `Finding` (Task 2).
- Produces: `type CrashLoopDetector struct{}` with method `Detect(PodFacts) *Finding`.
- Produces (test helper): `func podWaiting(namespace, name, container, reason, message string) *corev1.Pod`.

- [ ] **Step 1: Write the shared fake-pod helper**

`internal/diagnose/helpers_test.go`:
```go
package diagnose

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// podWaiting returns a pod whose single container is Waiting with reason+message.
func podWaiting(namespace, name, container, reason, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: container,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message},
				},
			}},
		},
	}
}
```

- [ ] **Step 2: Write the failing test**

`internal/diagnose/crashloop_test.go`:
```go
package diagnose

import "testing"

func TestCrashLoopDetector_FiresOnCrashLoopBackOff(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "CrashLoopBackOff", "")}

	f := CrashLoopDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "CrashLoopBackOff" {
		t.Errorf("Issue = %q, want CrashLoopBackOff", f.Issue)
	}
	if f.Pod != "default/web" {
		t.Errorf("Pod = %q, want default/web", f.Pod)
	}
}

func TestCrashLoopDetector_IgnoresOtherWaitingReasons(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := CrashLoopDetector{}.Detect(facts); f != nil {
		t.Errorf("expected nil for non-crashloop pod, got %+v", f)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./internal/diagnose/ -run TestCrashLoop -v
```
Expected: FAIL — `undefined: CrashLoopDetector`.

- [ ] **Step 4: Write the implementation**

`internal/diagnose/crashloop.go`:
```go
package diagnose

import "fmt"

// CrashLoopDetector flags containers stuck in CrashLoopBackOff.
type CrashLoopDetector struct{}

func (d CrashLoopDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
			return &Finding{
				Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:    "CrashLoopBackOff",
				Reason:   "Container repeatedly crashes after starting",
				Evidence: fmt.Sprintf("container %q, restartCount=%d", cs.Name, cs.RestartCount),
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/diagnose/ -run TestCrashLoop -v
```
Expected: PASS (both cases).

- [ ] **Step 6: Commit**

```bash
git add internal/diagnose/crashloop.go internal/diagnose/crashloop_test.go internal/diagnose/helpers_test.go
git commit -m "feat(diagnose): add CrashLoopBackOff detector"
```

---

## Task 4: ImagePullBackOff / ErrImagePull detector

**Files:**
- Create: `internal/diagnose/imagepull.go`
- Test: `internal/diagnose/imagepull_test.go`

**Interfaces:**
- Consumes: `PodFacts`, `Finding`, `podWaiting` helper (Task 3).
- Produces: `type ImagePullDetector struct{}` with `Detect(PodFacts) *Finding`.

- [ ] **Step 1: Write the failing test**

`internal/diagnose/imagepull_test.go`:
```go
package diagnose

import (
	"strings"
	"testing"
)

func TestImagePullDetector_FiresOnErrImagePull(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ErrImagePull", `rpc error: pull "x:typo" not found`)}

	f := ImagePullDetector{}.Detect(facts)

	if f == nil {
		t.Fatal("expected a finding, got nil")
	}
	if f.Issue != "ErrImagePull" {
		t.Errorf("Issue = %q, want ErrImagePull", f.Issue)
	}
	if !strings.Contains(f.Evidence, "not found") {
		t.Errorf("Evidence = %q, want it to include the waiting message", f.Evidence)
	}
}

func TestImagePullDetector_FiresOnImagePullBackOff(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ImagePullBackOff", "")}
	if f := ImagePullDetector{}.Detect(facts); f == nil || f.Issue != "ImagePullBackOff" {
		t.Fatalf("expected ImagePullBackOff finding, got %+v", f)
	}
}

func TestImagePullDetector_IgnoresRunningContainers(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := ImagePullDetector{}.Detect(facts); f != nil {
		t.Errorf("expected nil, got %+v", f)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/diagnose/ -run TestImagePull -v
```
Expected: FAIL — `undefined: ImagePullDetector`.

- [ ] **Step 3: Write the implementation**

`internal/diagnose/imagepull.go`:
```go
package diagnose

import "fmt"

// ImagePullDetector flags containers that cannot pull their image.
type ImagePullDetector struct{}

func (d ImagePullDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		w := cs.State.Waiting
		if w != nil && (w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull") {
			return &Finding{
				Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:    w.Reason,
				Reason:   "Bad image reference or registry authentication",
				Evidence: fmt.Sprintf("container %q: %s", cs.Name, w.Message),
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/diagnose/ -run TestImagePull -v
```
Expected: PASS (all three cases).

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/imagepull.go internal/diagnose/imagepull_test.go
git commit -m "feat(diagnose): add ImagePullBackOff/ErrImagePull detector"
```

---

## Task 5: OOMKilled detector

**Files:**
- Create: `internal/diagnose/oomkilled.go`
- Modify: `internal/diagnose/helpers_test.go` (add a terminated-state builder)
- Test: `internal/diagnose/oomkilled_test.go`

**Interfaces:**
- Consumes: `PodFacts`, `Finding`.
- Produces: `type OOMKilledDetector struct{}` with `Detect(PodFacts) *Finding`.
- Produces (test helper): `func podOOMKilled(namespace, name, container string, exitCode int32, viaLastTermination bool) *corev1.Pod`.

- [ ] **Step 1: Add the helper to `helpers_test.go`**

Append to `internal/diagnose/helpers_test.go`:
```go
// podOOMKilled returns a pod with a container terminated by OOMKilled.
// If viaLastTermination is true, the OOM is recorded in LastTerminationState
// (the pod has since restarted); otherwise in the current State.
func podOOMKilled(namespace, name, container string, exitCode int32, viaLastTermination bool) *corev1.Pod {
	term := &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: exitCode}
	cs := corev1.ContainerStatus{Name: container}
	if viaLastTermination {
		cs.LastTerminationState = corev1.ContainerState{Terminated: term}
	} else {
		cs.State = corev1.ContainerState{Terminated: term}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}
```

- [ ] **Step 2: Write the failing test**

`internal/diagnose/oomkilled_test.go`:
```go
package diagnose

import "testing"

func TestOOMKilledDetector_FiresOnCurrentState(t *testing.T) {
	facts := PodFacts{Pod: podOOMKilled("default", "cache", "redis", 137, false)}
	f := OOMKilledDetector{}.Detect(facts)
	if f == nil || f.Issue != "OOMKilled" {
		t.Fatalf("expected OOMKilled finding, got %+v", f)
	}
}

func TestOOMKilledDetector_FiresOnLastTerminationState(t *testing.T) {
	facts := PodFacts{Pod: podOOMKilled("default", "cache", "redis", 137, true)}
	if f := OOMKilledDetector{}.Detect(facts); f == nil {
		t.Fatal("expected OOMKilled finding from LastTerminationState, got nil")
	}
}

func TestOOMKilledDetector_IgnoresCleanExit(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := OOMKilledDetector{}.Detect(facts); f != nil {
		t.Errorf("expected nil, got %+v", f)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./internal/diagnose/ -run TestOOMKilled -v
```
Expected: FAIL — `undefined: OOMKilledDetector`.

- [ ] **Step 4: Write the implementation**

`internal/diagnose/oomkilled.go`:
```go
package diagnose

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// OOMKilledDetector flags containers killed for exceeding their memory limit.
type OOMKilledDetector struct{}

func (d OOMKilledDetector) Detect(facts PodFacts) *Finding {
	for _, cs := range facts.Pod.Status.ContainerStatuses {
		// Check both the current and the previous termination state:
		// a still-dead container reports in State, a restarted one in LastTerminationState.
		for _, term := range []*corev1.ContainerStateTerminated{
			cs.State.Terminated, cs.LastTerminationState.Terminated,
		} {
			if term != nil && term.Reason == "OOMKilled" {
				return &Finding{
					Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
					Issue:    "OOMKilled",
					Reason:   "Container exceeded its memory limit and was killed",
					Evidence: fmt.Sprintf("container %q, exitCode=%d", cs.Name, term.ExitCode),
				}
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

```bash
go test ./internal/diagnose/ -run TestOOMKilled -v
```
Expected: PASS (all three cases).

- [ ] **Step 6: Commit**

```bash
git add internal/diagnose/oomkilled.go internal/diagnose/oomkilled_test.go internal/diagnose/helpers_test.go
git commit -m "feat(diagnose): add OOMKilled detector"
```

---

## Task 6: Pending / Unschedulable detector

**Files:**
- Create: `internal/diagnose/pending.go`
- Modify: `internal/diagnose/helpers_test.go` (add an unschedulable-pod builder)
- Test: `internal/diagnose/pending_test.go`

**Interfaces:**
- Consumes: `PodFacts`, `Finding`.
- Produces: `type PendingDetector struct{}` with `Detect(PodFacts) *Finding`.
- Produces (test helper): `func podUnschedulable(namespace, name, message string) *corev1.Pod`.

- [ ] **Step 1: Add the helper to `helpers_test.go`**

Append to `internal/diagnose/helpers_test.go`:
```go
// podUnschedulable returns a Pending pod with a PodScheduled=False/Unschedulable condition.
func podUnschedulable(namespace, name, message string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: message,
			}},
		},
	}
}
```

- [ ] **Step 2: Write the failing test**

`internal/diagnose/pending_test.go`:
```go
package diagnose

import (
	"strings"
	"testing"
)

func TestPendingDetector_FiresOnUnschedulable(t *testing.T) {
	facts := PodFacts{Pod: podUnschedulable("default", "web", "0/3 nodes are available: insufficient cpu")}

	f := PendingDetector{}.Detect(facts)

	if f == nil || f.Issue != "Unschedulable" {
		t.Fatalf("expected Unschedulable finding, got %+v", f)
	}
	if !strings.Contains(f.Evidence, "insufficient cpu") {
		t.Errorf("Evidence = %q, want the scheduler message", f.Evidence)
	}
}

func TestPendingDetector_IgnoresScheduledPods(t *testing.T) {
	facts := PodFacts{Pod: podWaiting("default", "web", "app", "ContainerCreating", "")}
	if f := PendingDetector{}.Detect(facts); f != nil {
		t.Errorf("expected nil for a non-pending pod, got %+v", f)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

```bash
go test ./internal/diagnose/ -run TestPending -v
```
Expected: FAIL — `undefined: PendingDetector`.

- [ ] **Step 4: Write the implementation**

`internal/diagnose/pending.go`:
```go
package diagnose

import corev1 "k8s.io/api/core/v1"

// PendingDetector flags pods the scheduler cannot place on any node.
type PendingDetector struct{}

func (d PendingDetector) Detect(facts PodFacts) *Finding {
	if facts.Pod.Status.Phase != corev1.PodPending {
		return nil
	}
	for _, c := range facts.Pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == "Unschedulable" {
			return &Finding{
				Pod:      facts.Pod.Namespace + "/" + facts.Pod.Name,
				Issue:    "Unschedulable",
				Reason:   "No node can schedule this pod (resources, taints, or affinity)",
				Evidence: c.Message,
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: Run the full diagnose suite to verify everything passes**

```bash
go test ./internal/diagnose/ -v
```
Expected: PASS — all detector tests plus `TestRun`.

- [ ] **Step 6: Commit**

```bash
git add internal/diagnose/pending.go internal/diagnose/pending_test.go internal/diagnose/helpers_test.go
git commit -m "feat(diagnose): add Pending/Unschedulable detector"
```

---

## Task 7: Report package — text and JSON output

**Files:**
- Create: `internal/report/report.go`
- Test: `internal/report/report_test.go`
- Modify: `docs/go-concepts.md` (add a "JSON encoding" entry)

**Interfaces:**
- Consumes: `diagnose.Finding` (Task 2).
- Produces: `func Print(findings []diagnose.Finding, format string, w io.Writer) error`.

- [ ] **Step 1: Write the failing test**

`internal/report/report_test.go`:
```go
package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func sampleFindings() []diagnose.Finding {
	return []diagnose.Finding{
		{Pod: "default/web", Issue: "CrashLoopBackOff", Reason: "crashes", Evidence: "restartCount=14"},
	}
}

func TestPrint_TextIncludesPodAndIssue(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(sampleFindings(), "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "default/web") || !strings.Contains(out, "CrashLoopBackOff") {
		t.Errorf("text output missing pod or issue:\n%s", out)
	}
}

func TestPrint_TextNoFindings(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(nil, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("expected a clean no-issues message, got %q", buf.String())
	}
}

func TestPrint_JSONIsValidAndRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(sampleFindings(), "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got []diagnose.Finding
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output was not valid JSON: %v", err)
	}
	if len(got) != 1 || got[0].Issue != "CrashLoopBackOff" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestPrint_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(nil, "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/report/ -v
```
Expected: FAIL — `undefined: Print`.

- [ ] **Step 3: Write the implementation**

`internal/report/report.go`:
```go
package report

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Print writes findings to w in the chosen format ("text" or "json").
func Print(findings []diagnose.Finding, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(findings)
	case "text", "":
		return printText(findings, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}

func printText(findings []diagnose.Finding, w io.Writer) error {
	if len(findings) == 0 {
		_, err := fmt.Fprintln(w, "No issues found. ✅")
		return err
	}
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "%s\t%s\n", f.Pod, f.Issue); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "    %s\n", f.Reason); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "    evidence: %s\n\n", f.Evidence); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%d issue(s) found.\n", len(findings))
	return err
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/report/ -v
```
Expected: PASS (all four cases).

- [ ] **Step 5: Add a "JSON encoding" entry to `docs/go-concepts.md`**

Under the "Coming later" section, add a new numbered concept (renumber/move "Coming later" below it):
```markdown
## 10. JSON encoding (`encoding/json`)

Go turns structs into JSON (and back) with the `encoding/json` package. Struct
field tags like `` `json:"pod"` `` control the JSON key names.

**Simple example:**

```go
type User struct {
    Name string `json:"name"`
    Age  int    `json:"age"`
}

b, _ := json.Marshal(User{Name: "ann", Age: 30}) // {"name":"ann","age":30}
```

**kubeagent example:** `Finding` carries JSON tags, so `--output json` emits a
clean array:

```go
enc := json.NewEncoder(w)
enc.SetIndent("", "  ")
enc.Encode(findings)
```
```

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go docs/go-concepts.md
git commit -m "feat(report): text and JSON output; document JSON in go-concepts"
```

---

## Task 8: Cluster package — build a clientset from kubeconfig

**Files:**
- Create: `internal/cluster/client.go`
- Test: `internal/cluster/client_test.go`

**Interfaces:**
- Consumes: `k8s.io/client-go/kubernetes`, `k8s.io/client-go/tools/clientcmd`.
- Produces:
  - `func NewClient(kubeconfigPath string) (*kubernetes.Clientset, error)`
  - `func resolveKubeconfig(explicit string) string` (unexported, tested directly)

- [ ] **Step 1: Write the failing test**

`internal/cluster/client_test.go`:
```go
package cluster

import "testing"

func TestResolveKubeconfig_PrefersExplicitPath(t *testing.T) {
	if got := resolveKubeconfig("/tmp/my.kubeconfig"); got != "/tmp/my.kubeconfig" {
		t.Errorf("got %q, want the explicit path", got)
	}
}

func TestResolveKubeconfig_FallsBackToEnv(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/env.kubeconfig")
	if got := resolveKubeconfig(""); got != "/tmp/env.kubeconfig" {
		t.Errorf("got %q, want the KUBECONFIG value", got)
	}
}

func TestNewClient_BadPathReturnsError(t *testing.T) {
	if _, err := NewClient("/nonexistent/kubeconfig"); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/cluster/ -v
```
Expected: FAIL — `undefined: resolveKubeconfig`, `NewClient`.

- [ ] **Step 3: Write the implementation**

`internal/cluster/client.go`:
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
func NewClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	path := resolveKubeconfig(kubeconfigPath)

	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", path, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
}

func resolveKubeconfig(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".kube", "config")
}
```

- [ ] **Step 4: Pull the dependency and run the test**

```bash
go mod tidy
go test ./internal/cluster/ -v
```
Expected: `go mod tidy` adds `k8s.io/client-go`; all three tests PASS.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/cluster/client.go internal/cluster/client_test.go
git commit -m "feat(cluster): build clientset from kubeconfig with sensible fallbacks"
```

---

## Task 9: Collect package — list pods into `[]PodFacts`

**Files:**
- Create: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`
- Modify: `docs/go-concepts.md` (add a "context.Context and the fake clientset" entry)

**Interfaces:**
- Consumes: `kubernetes.Interface`, `diagnose.PodFacts`.
- Produces: `func Cluster(ctx context.Context, client kubernetes.Interface) ([]diagnose.PodFacts, error)`.
- Note: the parameter type is the **interface** `kubernetes.Interface` (not `*kubernetes.Clientset`) so tests can pass a fake. The real `*Clientset` from Task 8 satisfies it.

- [ ] **Step 1: Write the failing test (uses the fake clientset)**

`internal/collect/collect_test.go`:
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

	facts, err := Cluster(context.Background(), client)
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
	facts, err := Cluster(context.Background(), fake.NewSimpleClientset())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("expected 0 facts, got %d", len(facts))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./internal/collect/ -v
```
Expected: FAIL — `undefined: Cluster`.

- [ ] **Step 3: Write the implementation**

`internal/collect/collect.go`:
```go
package collect

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Cluster lists every pod in every namespace and wraps each in PodFacts.
// It is read-only: it performs a single List call and never mutates anything.
func Cluster(ctx context.Context, client kubernetes.Interface) ([]diagnose.PodFacts, error) {
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
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

- [ ] **Step 4: Run the test to verify it passes**

```bash
go mod tidy
go test ./internal/collect/ -v
```
Expected: PASS (both cases).

- [ ] **Step 5: Add a concept entry to `docs/go-concepts.md`**

Add before "Coming later":
```markdown
## 11. context.Context and the fake clientset

`context.Context` is Go's standard way to carry cancellation/timeout through a
call chain. Most `client-go` calls take a `ctx` as their first argument.

Depending on the **interface** `kubernetes.Interface` (instead of the concrete
`*kubernetes.Clientset`) lets tests substitute a fake implementation — no real
cluster needed.

**kubeagent example:**

```go
func Cluster(ctx context.Context, client kubernetes.Interface) ([]diagnose.PodFacts, error) {
    pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
    ...
}

// test:
client := fake.NewSimpleClientset(&corev1.Pod{ /* ... */ })
facts, _ := Cluster(context.Background(), client)
```
```

- [ ] **Step 6: Commit**

```bash
git add go.sum internal/collect/collect.go internal/collect/collect_test.go docs/go-concepts.md
git commit -m "feat(collect): list pods into PodFacts; document context and fake client"
```

---

## Task 10: Wire `main` and verify against a real cluster

**Files:**
- Modify: `main.go`
- Test: `main_test.go`
- Create: `testdata/broken-pods.yaml`

**Interfaces:**
- Consumes: `cluster.NewClient`, `collect.Cluster`, `diagnose.{Detector,Run,*Detector}`, `report.Print`.
- Produces: `func run(args []string) error` (testable entrypoint) and a `main` that maps its error to exit code 1.

- [ ] **Step 1: Write the failing test for arg handling**

`main_test.go`:
```go
package main

import "testing"

func TestRun_NoArgsReturnsUsage(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("expected a usage error with no args")
	}
}

func TestRun_RejectsUnknownSubcommand(t *testing.T) {
	if err := run([]string{"explode"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

func TestRun_RejectsBadOutputFormat(t *testing.T) {
	// This must fail on validation BEFORE any cluster connection is attempted.
	if err := run([]string{"scan", "--output", "bogus"}); err == nil {
		t.Fatal("expected an error for a bad --output value")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test . -run TestRun -v
```
Expected: FAIL — current `main.go` has no `run` function.

- [ ] **Step 3: Replace `main.go` with the wired pipeline**

`main.go`:
```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/report"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kubeagent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("usage: kubeagent scan [--kubeconfig path] [--output text|json]")
	}

	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	output := fs.String("output", "text", "output format: text | json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	// Validate format up front so we fail fast, before touching the network.
	if *output != "text" && *output != "json" {
		return fmt.Errorf("unknown output format %q (want text or json)", *output)
	}

	client, err := cluster.NewClient(*kubeconfig)
	if err != nil {
		return err
	}
	facts, err := collect.Cluster(context.Background(), client)
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

- [ ] **Step 4: Run the test to verify it passes, then build**

```bash
go test . -run TestRun -v
go build -o kubeagent .
```
Expected: tests PASS; `./kubeagent` binary builds.

- [ ] **Step 5: Run the whole suite**

```bash
go test ./...
```
Expected: every package PASSES.

- [ ] **Step 6: Create reproduction manifests**

`testdata/broken-pods.yaml`:
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: bad-image
  namespace: kubeagent-test
spec:
  containers:
    - name: app
      image: does-not-exist.example.com/nope:latest
---
apiVersion: v1
kind: Pod
metadata:
  name: crash-loop
  namespace: kubeagent-test
spec:
  containers:
    - name: app
      image: busybox
      command: ["sh", "-c", "echo starting; exit 1"]
---
apiVersion: v1
kind: Pod
metadata:
  name: unschedulable
  namespace: kubeagent-test
spec:
  nodeSelector:
    kubeagent.example/nonexistent: "true"
  containers:
    - name: app
      image: busybox
      command: ["sleep", "3600"]
```

- [ ] **Step 7: Manual verification against a cluster**

```bash
kubectl create namespace kubeagent-test
kubectl apply -f testdata/broken-pods.yaml
# give the kubelet ~30s to reach CrashLoopBackOff / ImagePullBackOff
kubectl get pods -n kubeagent-test
./kubeagent scan
./kubeagent scan --output json
```
Expected: the text run lists `kubeagent-test/bad-image` (ImagePullBackOff or ErrImagePull), `kubeagent-test/crash-loop` (CrashLoopBackOff), and `kubeagent-test/unschedulable` (Unschedulable); the JSON run prints the same as a JSON array. (OOMKilled is the hardest to reproduce reliably and is covered by unit tests; optionally add a low-memory-limit pod to see it live.)

- [ ] **Step 8: Clean up the test namespace (read-only tool; we only clean what we created)**

```bash
kubectl delete namespace kubeagent-test
```

- [ ] **Step 9: Commit**

```bash
git add main.go main_test.go testdata/broken-pods.yaml
git commit -m "feat: wire scan pipeline in main and add reproduction manifests"
```

---

## Task 11: Update README status and push

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the Status section**

In `README.md`, replace the Status section body with:
```markdown
## Status

✅ **v1 shipped** — `kubeagent scan` performs a read-only, whole-cluster scan and
reports CrashLoopBackOff, ImagePullBackOff/ErrImagePull, OOMKilled, and
Pending/Unschedulable pods, in text or JSON.

🔜 **v2** — an optional `--explain` flag that makes a single Claude API call to
summarize findings in plain English.
```

- [ ] **Step 2: Final full test + build**

```bash
go test ./... && go build -o kubeagent .
```
Expected: all tests pass, binary builds.

- [ ] **Step 3: Commit and push the whole v1**

```bash
git add README.md
git commit -m "docs: mark v1 shipped"
git push origin main
```
Expected: `main` updated on GitHub with the full working v1.

---

## Notes / deliberate scope decisions

- **Events not collected in v1.** The spec lists per-pod events in `PodFacts`, but all four v1 detectors derive their evidence from pod **status/conditions** directly. To avoid collecting data nothing reads (YAGNI), `collect.Cluster` lists pods only; the `Events` field stays present (forward-compatible) but empty. Event collection + matching becomes a v-next item, useful for richer Pending evidence and for the v2 `--explain` call.
- **`scan` subcommand via stdlib `flag`.** With one command we check `os.Args[1] == "scan"` by hand. Cobra is the documented growth path when `diagnose <pod>` is added.
- **OOMKilled live-repro is flaky** by nature; it is fully covered by unit tests, so the manual step treats it as optional.

---

## Self-review

- **Spec coverage:** whole-cluster scan (Task 9 + 10) ✅; CrashLoopBackOff (3) ✅; ImagePullBackOff/ErrImagePull (4) ✅; OOMKilled (5) ✅; Pending/Unschedulable (6) ✅; text + JSON output (7) ✅; read-only (List-only in 9, asserted in Global Constraints) ✅; kubeconfig resolution (8) ✅; stdlib `flag` + `scan` (10) ✅; exit codes (10) ✅; `go-concepts.md` growth (7, 9) ✅. The only intentional deviation — events not collected — is documented in Notes above.
- **Placeholder scan:** no TBD/TODO; every code step contains complete, runnable code.
- **Type consistency:** `Detector.Detect(PodFacts) *Finding` is used identically across Tasks 2–6; `Cluster(ctx, kubernetes.Interface) ([]diagnose.PodFacts, error)` in Task 9 matches `main`'s call in Task 10; `report.Print([]diagnose.Finding, string, io.Writer) error` in Task 7 matches Task 10's call; detector type names (`CrashLoopDetector`, `ImagePullDetector`, `OOMKilledDetector`, `PendingDetector`) match between their tasks and the `detectors` slice in Task 10.
