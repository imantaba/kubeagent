# Workload Security Posture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in `scan --security` pass that flags PSS-aligned workload security problems — privileged/over-privileged containers, insecure defaults, and exposed Services — in a `SECURITY` text section and JSON `securityIssues`.

**Architecture:** A new pure package `internal/secscan` exposes `Assess(pods, services, replicaSets) []Finding`, mirroring `svchealth`/`ingresshealth`. `scan.Evaluate` gains a `Security` option: it filters out system namespaces (all-namespaces mode only), calls `Assess` over the pods/services/replicaSets it already collects, and stores `Result.SecurityIssues`. `report` renders the advisory section; `main.go` adds the flag. No new collectors, no new RBAC.

**Tech Stack:** Go 1.26, `k8s.io/api/core/v1`, `k8s.io/api/apps/v1`, `k8s.io/apimachinery`. Tests use fake objects + client-go fake clientset.

## Global Constraints

- Read-only; opt-in behind the `--security` flag; **advisory** — never changes the cluster verdict, `kubeagent_cluster_healthy`, or the scan exit code; not wired into `--explain`; no daemon/`watch` gauge in v1.
- **No new RBAC / no new collectors** — pods, services, and replicasets are already collected by `collect.CollectInventory`/`collect.Services`.
- Workload checks are a **curated subset aligned with the Pod Security Standards** (baseline + restricted), documented as *aligned, not conformant*.
- **System namespaces** `kube-system`, `kube-node-lease`, `kube-public` are excluded when scanning **all** namespaces (`opts.Namespace == ""`); an explicit `-n kube-system` includes them.
- Findings group by **top-level workload** (pod → ReplicaSet → Deployment); a multi-replica workload yields one finding-set. Order is most-dangerous first: `baseline` → `restricted` → `kubeagent`.
- Exact names — flag `--security`; JSON field `securityIssues`; profiles `baseline`/`restricted`/`kubeagent`; `Check` values `Privileged`, `HostNamespaces`, `HostPath`, `HostPort`, `AddedCapability`, `RunAsRoot`, `AllowPrivilegeEscalation`, `CapabilitiesNotDropped`, `ExposedService`.
- Commits carry **no `Co-Authored-By: Claude` trailer**.
- TDD: failing test first, watch it fail, implement, pass, commit. Set Go on PATH: `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: `internal/secscan` foundation + Privileged + HostNamespaces

**Files:**
- Create: `internal/secscan/secscan.go`
- Test: `internal/secscan/secscan_test.go`

**Interfaces:**
- Produces:
  - `type Finding struct { Namespace, Workload, Kind, Container, Profile, Check, Detail string }` (JSON tags below).
  - `func Assess(pods []corev1.Pod, services []corev1.Service, replicaSets []appsv1.ReplicaSet) []Finding`.
  - Internal helpers later tasks extend: `podFindings`, `baselinePodChecks`, `containerChecks`, `finding`, `allContainers`.

- [ ] **Step 1: Write the failing test**

Create `internal/secscan/secscan_test.go`:

```go
package secscan

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolp(b bool) *bool    { return &b }
func int64p(i int64) *int64 { return &i }

// rsOwned builds a pod controlled by ReplicaSet rsName, in namespace ns.
func rsOwned(ns, podName, rsName string, ctrs ...corev1.Container) corev1.Pod {
	ctrl := true
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: podName,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rsName, Controller: &ctrl}},
		},
		Spec: corev1.PodSpec{Containers: ctrs},
	}
}

// rsForDeploy builds a ReplicaSet controlled by Deployment depName.
func rsForDeploy(ns, rsName, depName string) appsv1.ReplicaSet {
	ctrl := true
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: rsName,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: depName, Controller: &ctrl}},
	}}
}

// count returns how many findings have the given Check.
func count(fs []Finding, check string) int {
	n := 0
	for _, f := range fs {
		if f.Check == check {
			n++
		}
	}
	return n
}

func TestAssess_PrivilegedFoldsToDeployment(t *testing.T) {
	pod := rsOwned("payments", "api-xyz", "api-rs",
		corev1.Container{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}})
	rs := []appsv1.ReplicaSet{rsForDeploy("payments", "api-rs", "api")}
	got := Assess([]corev1.Pod{pod}, nil, rs)
	if count(got, "Privileged") != 1 {
		t.Fatalf("want one Privileged finding, got %+v", got)
	}
	f := got[0]
	if f.Profile != "baseline" || f.Kind != "Deployment" || f.Workload != "api" ||
		f.Container != "app" || f.Namespace != "payments" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_NotPrivileged(t *testing.T) {
	pod := rsOwned("shop", "web-xyz", "web-rs",
		corev1.Container{Name: "web", SecurityContext: &corev1.SecurityContext{Privileged: boolp(false)}})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "Privileged") != 0 {
		t.Error("a non-privileged container must not be flagged Privileged")
	}
}

func TestAssess_HostNamespaces(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "agent"},
		Spec:       corev1.PodSpec{HostNetwork: true, HostPID: true, Containers: []corev1.Container{{Name: "c"}}},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "HostNamespaces") != 1 {
		t.Fatalf("want one HostNamespaces finding, got %+v", got)
	}
	// bare pod (no controller) -> Kind Pod, its own name; pod-level -> no container.
	f := got[0]
	if f.Kind != "Pod" || f.Workload != "agent" || f.Container != "" {
		t.Errorf("wrong attribution: %+v", f)
	}
}

func TestAssess_DedupsReplicas(t *testing.T) {
	c := corev1.Container{Name: "app", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}
	pods := []corev1.Pod{
		rsOwned("payments", "api-1", "api-rs", c),
		rsOwned("payments", "api-2", "api-rs", c),
	}
	rs := []appsv1.ReplicaSet{rsForDeploy("payments", "api-rs", "api")}
	if n := count(Assess(pods, nil, rs), "Privileged"); n != 1 {
		t.Errorf("two replicas of one Deployment must collapse to one finding, got %d", n)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/secscan/`
Expected: FAIL — `undefined: Assess` / `undefined: Finding`.

- [ ] **Step 3: Write the implementation**

Create `internal/secscan/secscan.go`:

```go
// Package secscan flags high-signal, Pod Security Standards-aligned workload
// security-posture problems — privileged/over-privileged containers, insecure
// container defaults, and exposed Services. It is a curated subset of PSS
// (baseline + restricted), not a conformance implementation. Pure and
// read-only: the caller supplies the pods, services, and replicasets.
package secscan

import (
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	profileBaseline   = "baseline"
	profileRestricted = "restricted"
	profileKubeagent  = "kubeagent"
)

// Finding is one security-posture problem attributed to a workload (or Service).
type Finding struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container,omitempty"`
	Profile   string `json:"profile"`
	Check     string `json:"check"`
	Detail    string `json:"detail"`
}

// Assess flags PSS-aligned security-posture problems in the given pods and
// services. replicaSets is used only to fold a Deployment's pods up to the
// Deployment for display. Pure; the caller supplies already-namespace-filtered
// inputs.
func Assess(pods []corev1.Pod, services []corev1.Service, replicaSets []appsv1.ReplicaSet) []Finding {
	rsByKey := make(map[string]appsv1.ReplicaSet, len(replicaSets))
	for _, rs := range replicaSets {
		rsByKey[rs.Namespace+"/"+rs.Name] = rs
	}
	seen := make(map[string]bool)
	var out []Finding
	add := func(f Finding) {
		key := strings.Join([]string{f.Namespace, f.Kind, f.Workload, f.Container, f.Check}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	}
	for _, pod := range pods {
		wl := resolveWorkload(pod, rsByKey)
		for _, f := range podFindings(pod, wl) {
			add(f)
		}
	}
	sortFindings(out)
	return out
}

// workloadRef is a pod's display owner.
type workloadRef struct{ Kind, Name string }

// resolveWorkload maps a pod to its top-level workload: its controlling owner,
// folded up one level when that owner is a ReplicaSet (→ its Deployment).
func resolveWorkload(pod corev1.Pod, rsByKey map[string]appsv1.ReplicaSet) workloadRef {
	owner := controllerOf(pod.OwnerReferences)
	if owner == nil {
		return workloadRef{Kind: "Pod", Name: pod.Name}
	}
	if owner.Kind == "ReplicaSet" {
		if rs, ok := rsByKey[pod.Namespace+"/"+owner.Name]; ok {
			if d := controllerOf(rs.OwnerReferences); d != nil && d.Kind == "Deployment" {
				return workloadRef{Kind: "Deployment", Name: d.Name}
			}
		}
		return workloadRef{Kind: "ReplicaSet", Name: owner.Name}
	}
	return workloadRef{Kind: owner.Kind, Name: owner.Name}
}

func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// podFindings returns every posture finding for one pod.
func podFindings(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	out = append(out, baselinePodChecks(pod, wl)...)
	out = append(out, containerChecks(pod, wl)...)
	return out
}

// baselinePodChecks covers pod-level baseline controls.
func baselinePodChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	if ns := hostNamespaces(pod); ns != "" {
		out = append(out, finding(pod, wl, profileBaseline, "HostNamespaces", "", "pod shares the host "+ns))
	}
	return out
}

// containerChecks covers per-container baseline controls.
func containerChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	for _, c := range allContainers(pod) {
		if isPrivileged(c) {
			out = append(out, finding(pod, wl, profileBaseline, "Privileged", c.Name,
				fmt.Sprintf("container %q runs privileged (full host access)", c.Name)))
		}
	}
	return out
}

func isPrivileged(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
}

// finding builds a Finding attributed to the pod's workload.
func finding(pod corev1.Pod, wl workloadRef, profile, check, container, detail string) Finding {
	return Finding{
		Namespace: pod.Namespace, Workload: wl.Name, Kind: wl.Kind,
		Container: container, Profile: profile, Check: check, Detail: detail,
	}
}

// allContainers returns the pod's init + regular containers.
func allContainers(pod corev1.Pod) []corev1.Container {
	return append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
}

// hostNamespaces returns a human phrase for the shared host namespaces, or "".
func hostNamespaces(pod corev1.Pod) string {
	var s []string
	if pod.Spec.HostNetwork {
		s = append(s, "network")
	}
	if pod.Spec.HostPID {
		s = append(s, "PID")
	}
	if pod.Spec.HostIPC {
		s = append(s, "IPC")
	}
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, "/") + " namespace"
}

// sortFindings orders most-dangerous first, then namespace/workload/container/check.
func sortFindings(fs []Finding) {
	rank := map[string]int{profileBaseline: 0, profileRestricted: 1, profileKubeagent: 2}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if rank[a.Profile] != rank[b.Profile] {
			return rank[a.Profile] < rank[b.Profile]
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		return a.Check < b.Check
	})
}
```

Note: `int64p` in the test file is unused until Task 3 — Go allows unused functions (only unused imports/locals fail). Keep it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/secscan/`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/secscan/
git commit -m "feat(secscan): security-posture package with Privileged + HostNamespaces checks"
```

---

### Task 2: Baseline checks — HostPath, HostPort, AddedCapability

**Files:**
- Modify: `internal/secscan/secscan.go` (rewrite `baselinePodChecks` and `containerChecks`; add `dangerousAddedCaps`)
- Test: `internal/secscan/secscan_test.go` (append tests)

**Interfaces:**
- Consumes: `Assess`, `finding`, `allContainers` (Task 1).
- Produces: `Check` values `HostPath`, `HostPort`, `AddedCapability`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/secscan/secscan_test.go`:

```go
func TestAssess_HostPath(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "node-agent"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c"}},
			Volumes: []corev1.Volume{{Name: "sock", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run/docker.sock"}}}},
		},
	}
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "HostPath") != 1 {
		t.Fatalf("want one HostPath finding, got %+v", got)
	}
}

func TestAssess_HostPort(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs",
		corev1.Container{Name: "web", Ports: []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 8080}}})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "HostPort") != 1 {
		t.Errorf("want one HostPort finding")
	}
}

func TestAssess_AddedCapability(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE", "SYS_ADMIN"}}},
	})
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "AddedCapability") != 1 {
		t.Fatalf("want one AddedCapability finding, got %+v", got)
	}
	// NET_BIND_SERVICE alone is allowed by baseline.
	ok := rsOwned("shop", "ok-1", "ok-rs", corev1.Container{
		Name: "web",
		SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
			Add: []corev1.Capability{"NET_BIND_SERVICE"}}},
	})
	if count(Assess([]corev1.Pod{ok}, nil, nil), "AddedCapability") != 0 {
		t.Errorf("NET_BIND_SERVICE alone must not be flagged")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/secscan/ -run 'HostPath|HostPort|AddedCapability'`
Expected: FAIL — the checks return no findings yet.

- [ ] **Step 3: Extend the checks**

In `internal/secscan/secscan.go`, replace `baselinePodChecks` with:

```go
// baselinePodChecks covers pod-level baseline controls.
func baselinePodChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	if ns := hostNamespaces(pod); ns != "" {
		out = append(out, finding(pod, wl, profileBaseline, "HostNamespaces", "", "pod shares the host "+ns))
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			out = append(out, finding(pod, wl, profileBaseline, "HostPath", "",
				fmt.Sprintf("mounts hostPath %s (writable host filesystem)", v.HostPath.Path)))
		}
	}
	return out
}
```

Replace `containerChecks` with:

```go
// containerChecks covers per-container baseline controls.
func containerChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	for _, c := range allContainers(pod) {
		if isPrivileged(c) {
			out = append(out, finding(pod, wl, profileBaseline, "Privileged", c.Name,
				fmt.Sprintf("container %q runs privileged (full host access)", c.Name)))
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				out = append(out, finding(pod, wl, profileBaseline, "HostPort", c.Name,
					fmt.Sprintf("container %q binds host port %d", c.Name, p.HostPort)))
				break
			}
		}
		if caps := dangerousAddedCaps(c); len(caps) > 0 {
			out = append(out, finding(pod, wl, profileBaseline, "AddedCapability", c.Name,
				fmt.Sprintf("container %q adds capability %s", c.Name, strings.Join(caps, ", "))))
		}
	}
	return out
}

// baselineAllowedCap is the only capability the baseline profile permits adding.
const baselineAllowedCap = "NET_BIND_SERVICE"

func dangerousAddedCaps(c corev1.Container) []string {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return nil
	}
	var bad []string
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if string(cap) != baselineAllowedCap {
			bad = append(bad, string(cap))
		}
	}
	return bad
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/secscan/`
Expected: PASS (7 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/secscan/
git commit -m "feat(secscan): add baseline HostPath, HostPort, AddedCapability checks"
```

---

### Task 3: Restricted checks + ExposedService

**Files:**
- Modify: `internal/secscan/secscan.go` (add `strconv` import; extend `podFindings`; add `restrictedChecks` and helpers; add the services loop to `Assess`; add `exposedService`)
- Test: `internal/secscan/secscan_test.go` (append tests)

**Interfaces:**
- Consumes: everything from Tasks 1–2.
- Produces: `Check` values `RunAsRoot`, `AllowPrivilegeEscalation`, `CapabilitiesNotDropped`, `ExposedService`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/secscan/secscan_test.go`:

```go
// hardened satisfies every workload check: non-root, no priv-esc, drops ALL.
func hardenedContainer(name string) corev1.Container {
	return corev1.Container{
		Name: name,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolp(true),
			AllowPrivilegeEscalation: boolp(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

func TestAssess_RunAsRoot_DefaultFlagged(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{Name: "web"}) // no securityContext
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 1 {
		t.Error("a container with no runAsNonRoot must be flagged RunAsRoot")
	}
}

func TestAssess_RunAsRoot_PodLevelNonRootSatisfies(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: boolp(true)} // inherited by the container
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 0 {
		t.Error("pod-level runAsNonRoot must satisfy the container")
	}
}

func TestAssess_RunAsUserZeroFlagged(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{
		Name:            "web",
		SecurityContext: &corev1.SecurityContext{RunAsUser: int64p(0), AllowPrivilegeEscalation: boolp(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
	})
	if count(Assess([]corev1.Pod{pod}, nil, nil), "RunAsRoot") != 1 {
		t.Error("runAsUser 0 must be flagged RunAsRoot")
	}
}

func TestAssess_AllowPrivilegeEscalationAndCaps(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", corev1.Container{Name: "web"}) // nothing set
	got := Assess([]corev1.Pod{pod}, nil, nil)
	if count(got, "AllowPrivilegeEscalation") != 1 {
		t.Error("no allowPrivilegeEscalation:false must be flagged")
	}
	if count(got, "CapabilitiesNotDropped") != 1 {
		t.Error("not dropping ALL must be flagged")
	}
}

func TestAssess_HardenedPodClean(t *testing.T) {
	pod := rsOwned("shop", "web-1", "web-rs", hardenedContainer("web"))
	if got := Assess([]corev1.Pod{pod}, nil, nil); len(got) != 0 {
		t.Errorf("a fully hardened pod must yield no findings, got %+v", got)
	}
}

func TestAssess_ExposedService(t *testing.T) {
	svcs := []corev1.Service{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "admin"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 80}}}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "internal"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 80}}}},
	}
	got := Assess(nil, svcs, nil)
	if count(got, "ExposedService") != 1 {
		t.Fatalf("want one ExposedService finding, got %+v", got)
	}
	f := got[0]
	if f.Kind != "Service" || f.Workload != "admin" || f.Profile != "kubeagent" {
		t.Errorf("wrong attribution: %+v", f)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/secscan/ -run 'RunAs|AllowPriv|Hardened|ExposedService'`
Expected: FAIL — restricted checks and the service loop don't exist yet.

- [ ] **Step 3: Implement restricted checks + service check**

In `internal/secscan/secscan.go`, add `"strconv"` to the import block. Replace `podFindings` with:

```go
// podFindings returns every posture finding for one pod.
func podFindings(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	out = append(out, baselinePodChecks(pod, wl)...)
	out = append(out, containerChecks(pod, wl)...)
	out = append(out, restrictedChecks(pod, wl)...)
	return out
}
```

Add the restricted checks and helpers:

```go
// restrictedChecks covers per-container restricted controls, evaluated on
// effective settings (a container's securityContext overrides the pod's).
func restrictedChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	for _, c := range allContainers(pod) {
		if !guaranteedNonRoot(pod, c) {
			out = append(out, finding(pod, wl, profileRestricted, "RunAsRoot", c.Name,
				fmt.Sprintf("container %q is not guaranteed to run as non-root", c.Name)))
		}
		if !escalationDisabled(c) {
			out = append(out, finding(pod, wl, profileRestricted, "AllowPrivilegeEscalation", c.Name,
				fmt.Sprintf("container %q allows privilege escalation (allowPrivilegeEscalation not false)", c.Name)))
		}
		if !dropsAll(c) {
			out = append(out, finding(pod, wl, profileRestricted, "CapabilitiesNotDropped", c.Name,
				fmt.Sprintf("container %q does not drop all capabilities", c.Name)))
		}
	}
	return out
}

func guaranteedNonRoot(pod corev1.Pod, c corev1.Container) bool {
	if nr := effectiveRunAsNonRoot(pod, c); nr != nil && *nr {
		return true
	}
	uid := effectiveRunAsUser(pod, c)
	return uid != nil && *uid > 0
}

func effectiveRunAsNonRoot(pod corev1.Pod, c corev1.Container) *bool {
	if c.SecurityContext != nil && c.SecurityContext.RunAsNonRoot != nil {
		return c.SecurityContext.RunAsNonRoot
	}
	if pod.Spec.SecurityContext != nil {
		return pod.Spec.SecurityContext.RunAsNonRoot
	}
	return nil
}

func effectiveRunAsUser(pod corev1.Pod, c corev1.Container) *int64 {
	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
		return c.SecurityContext.RunAsUser
	}
	if pod.Spec.SecurityContext != nil {
		return pod.Spec.SecurityContext.RunAsUser
	}
	return nil
}

func escalationDisabled(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.AllowPrivilegeEscalation != nil && !*c.SecurityContext.AllowPrivilegeEscalation
}

func dropsAll(c corev1.Container) bool {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return false
	}
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == "ALL" {
			return true
		}
	}
	return false
}

// exposedService flags Services reachable from outside the cluster.
func exposedService(svc corev1.Service) (Finding, bool) {
	var reason string
	switch {
	case svc.Spec.Type == corev1.ServiceTypeLoadBalancer:
		reason = "type LoadBalancer"
	case svc.Spec.Type == corev1.ServiceTypeNodePort:
		reason = "type NodePort"
	case len(svc.Spec.ExternalIPs) > 0:
		reason = "externalIPs set"
	default:
		return Finding{}, false
	}
	return Finding{
		Namespace: svc.Namespace, Workload: svc.Name, Kind: "Service",
		Profile: profileKubeagent, Check: "ExposedService",
		Detail:  fmt.Sprintf("%s exposes %s externally", reason, servicePorts(svc)),
	}, true
}

func servicePorts(svc corev1.Service) string {
	var ps []string
	for _, p := range svc.Spec.Ports {
		ps = append(ps, strconv.Itoa(int(p.Port)))
	}
	if len(ps) == 0 {
		return "no ports"
	}
	return "port(s) " + strings.Join(ps, ",")
}
```

In `Assess`, add the services loop immediately after the pods loop (before `sortFindings(out)`):

```go
	for _, svc := range services {
		if f, ok := exposedService(svc); ok {
			add(f)
		}
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/secscan/`
Expected: PASS (all secscan tests).

- [ ] **Step 5: Commit**

```bash
git add internal/secscan/
git commit -m "feat(secscan): add restricted checks (root, priv-esc, caps) + ExposedService"
```

---

### Task 4: Wire into `scan` (opt-in + system-namespace exclusion) + `--security` flag

**Files:**
- Modify: `internal/scan/scan.go` (import `secscan`; `Options.Security`; `Result.SecurityIssues`; filter helpers; `Evaluate` block)
- Modify: `main.go` (declare `security` flag; pass `Security: *security` into `scan.Options`)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `secscan.Assess`, `secscan.Finding` (Task 3).
- Produces: `scan.Options.Security bool`; `scan.Result.SecurityIssues []secscan.Finding`.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (mirror the file's existing imports; it uses `fake.NewSimpleClientset`. Ensure `corev1`, `metav1`, and `"context"` are imported):

```go
func boolp(b bool) *bool { return &b }

func privPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", SecurityContext: &corev1.SecurityContext{Privileged: boolp(true)}}}},
	}
}

func nsCount(fs []secscan.Finding, ns string) int {
	n := 0
	for _, f := range fs {
		if f.Namespace == ns {
			n++
		}
	}
	return n
}

func TestEvaluate_SecurityOptInAndSystemExclusion(t *testing.T) {
	client := fake.NewSimpleClientset(privPod("default", "app"), privPod("kube-system", "cni"))

	// Flag off: no security findings at all.
	off, err := Evaluate(context.Background(), client, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(off.SecurityIssues) != 0 {
		t.Errorf("without Security, expected no findings, got %+v", off.SecurityIssues)
	}

	// All namespaces: kube-system excluded, default kept.
	all, err := Evaluate(context.Background(), client, Options{Security: true})
	if err != nil {
		t.Fatal(err)
	}
	if nsCount(all.SecurityIssues, "kube-system") != 0 {
		t.Errorf("kube-system must be excluded in all-namespaces mode, got %+v", all.SecurityIssues)
	}
	if nsCount(all.SecurityIssues, "default") == 0 {
		t.Errorf("default namespace privileged pod must be flagged, got %+v", all.SecurityIssues)
	}

	// Explicit -n kube-system: included.
	sys, err := Evaluate(context.Background(), client, Options{Security: true, Namespace: "kube-system"})
	if err != nil {
		t.Fatal(err)
	}
	if nsCount(sys.SecurityIssues, "kube-system") == 0 {
		t.Errorf("explicit -n kube-system must include it, got %+v", sys.SecurityIssues)
	}

	// Advisory: security findings never flip the verdict.
	if all.Health.Verdict != off.Health.Verdict {
		t.Errorf("security must not change the verdict (%q vs %q)", all.Health.Verdict, off.Health.Verdict)
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/secscan"` to the test's imports.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_SecurityOptIn`
Expected: FAIL to compile — `Options` has no `Security`, `Result` has no `SecurityIssues`.

- [ ] **Step 3: Wire `scan.go`**

In `internal/scan/scan.go`, add the import `"github.com/imantaba/kubeagent/internal/secscan"`.

Add to `Options` (after `DiskThreshold`):

```go
	Security bool
```

Add to `Result` (after `IngressIssues`):

```go
	SecurityIssues []secscan.Finding
```

Add the system-namespace set and filter helpers (near the bottom of the file, above `Evaluate` or below it):

```go
// systemNamespaces are excluded from the security scan when scanning all
// namespaces: their workloads (CNI, kube-proxy, …) are legitimately privileged.
var systemNamespaces = map[string]bool{"kube-system": true, "kube-node-lease": true, "kube-public": true}

func nonSystemPods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if !systemNamespaces[p.Namespace] {
			out = append(out, p)
		}
	}
	return out
}

func nonSystemServices(svcs []corev1.Service) []corev1.Service {
	var out []corev1.Service
	for _, s := range svcs {
		if !systemNamespaces[s.Namespace] {
			out = append(out, s)
		}
	}
	return out
}
```

In `Evaluate`, right after the `ingressIssues := ...` line, add:

```go
	var securityIssues []secscan.Finding
	if opts.Security {
		pods, services := inputs.Pods, svcs
		if opts.Namespace == "" {
			pods = nonSystemPods(pods)
			services = nonSystemServices(services)
		}
		securityIssues = secscan.Assess(pods, services, inputs.ReplicaSets)
	}
```

Add `SecurityIssues: securityIssues` to the returned `Result{...}` literal.

- [ ] **Step 4: Wire `main.go`**

In `main.go`, declare the flag next to the other scan `fs.Bool` flags (after the `disk-threshold` flag, ~line 74):

```go
	security := fs.Bool("security", false, "flag insecure workloads and exposed Services (read-only, advisory)")
```

Add `Security: *security,` to the `scan.Options{...}` literal (~line 99).

- [ ] **Step 5: Run tests**

Run: `go build ./... && go test ./internal/scan/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/scan/scan.go internal/scan/scan_test.go main.go
git commit -m "feat(scan): opt-in --security posture pass with system-namespace exclusion"
```

---

### Task 5: Report — `SECURITY` section + JSON `securityIssues`

**Files:**
- Modify: `internal/report/report.go` (import `secscan`; `Input.SecurityIssues`; `inventoryReport.SecurityIssues`; `printSecurityIssues`; call + all-clear guard in `printInventoryText`)
- Modify: `main.go` (pass `SecurityIssues` into `report.Input`)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `secscan.Finding` (Task 3); `scan.Result.SecurityIssues` (Task 4).
- Produces: `report.Input.SecurityIssues []secscan.Finding`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/report/report_test.go` (add the `"github.com/imantaba/kubeagent/internal/secscan"` import):

```go
func TestPrintInventory_TextShowsSecurity(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{{
			Namespace: "payments", Workload: "api", Kind: "Deployment", Container: "app",
			Profile: "baseline", Check: "Privileged", Detail: `container "app" runs privileged (full host access)`,
		}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "SECURITY") {
		t.Errorf("expected a SECURITY section:\n%s", out)
	}
	if !strings.Contains(out, "payments/api  Deployment") || !strings.Contains(out, "[baseline] Privileged") {
		t.Errorf("missing the grouped finding line:\n%s", out)
	}
	if !strings.Contains(out, "Security: 1 finding across 1 workload") {
		t.Errorf("missing the summary line:\n%s", out)
	}
	if strings.Contains(out, "No issues found") {
		t.Errorf("all-clear must be suppressed when security findings exist:\n%s", out)
	}
}

func TestPrintInventory_JSONIncludesSecurity(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster:        clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1},
		SecurityIssues: []secscan.Finding{{Namespace: "shop", Workload: "admin", Kind: "Service", Profile: "kubeagent", Check: "ExposedService", Detail: "type LoadBalancer exposes port(s) 80 externally"}},
	}
	if err := PrintInventory(in, "json", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"securityIssues"`) || !strings.Contains(buf.String(), `"check": "ExposedService"`) {
		t.Errorf("expected securityIssues in JSON:\n%s", buf.String())
	}
}

func TestPrintInventory_NoSecurityWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	in := Input{Cluster: clusterhealth.ClusterHealth{Verdict: "Healthy", NodesReady: 1, NodesTotal: 1}}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "SECURITY") {
		t.Errorf("no SECURITY section expected when there are no findings:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("empty security must not suppress the all-clear:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run 'Security'`
Expected: FAIL to compile — `Input` has no `SecurityIssues`.

- [ ] **Step 3: Add the field, JSON, renderer, and all-clear guard**

In `internal/report/report.go`, add the import `"github.com/imantaba/kubeagent/internal/secscan"`.

Add to `Input` (after `IngressIssues`):

```go
	SecurityIssues     []secscan.Finding
```

Add to `inventoryReport` (after `IngressIssues`):

```go
	SecurityIssues     []secscan.Finding           `json:"securityIssues,omitempty"`
```

Add `SecurityIssues: in.SecurityIssues,` to the `inventoryReport{...}` literal in the json branch of `PrintInventory`.

In `printInventoryText`, after the `if hasAttention { ... }` block closes (before `printNotes`), add:

```go
	hasSecurity := len(in.SecurityIssues) > 0
	if err := printSecurityIssues(in.SecurityIssues, w); err != nil {
		return err
	}
```

Change the all-clear condition from `if !hasAttention && in.Cluster.Verdict == "Healthy" {` to:

```go
	if !hasAttention && !hasSecurity && in.Cluster.Verdict == "Healthy" {
```

Add the renderer (near `printIngressIssues`):

```go
// printSecurityIssues renders the advisory SECURITY section: workloads (and
// Services) with insecure posture, grouped, most-dangerous first.
func printSecurityIssues(issues []secscan.Finding, w io.Writer) error {
	if len(issues) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "SECURITY  (advisory — does not affect the cluster verdict)"); err != nil {
		return err
	}
	type grp struct{ ns, name, kind string }
	var order []grp
	byGrp := map[grp][]secscan.Finding{}
	for _, f := range issues {
		g := grp{f.Namespace, f.Workload, f.Kind}
		if _, ok := byGrp[g]; !ok {
			order = append(order, g)
		}
		byGrp[g] = append(byGrp[g], f)
	}
	for _, g := range order {
		if _, err := fmt.Fprintf(w, "%s/%s  %s\n", g.ns, g.name, g.kind); err != nil {
			return err
		}
		for _, f := range byGrp[g] {
			if _, err := fmt.Fprintf(w, "  [%s] %s — %s\n", f.Profile, f.Check, f.Detail); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w, "\nSecurity: %d %s across %d %s\n\n",
		len(issues), plural(len(issues), "finding", "findings"),
		len(order), plural(len(order), "workload", "workloads")); err != nil {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Update `main.go`**

In `main.go`, add to the `report.Input{...}` literal (after `IngressIssues: res.IngressIssues,`):

```go
		SecurityIssues:     res.SecurityIssues,
```

- [ ] **Step 5: Run the tests**

Run: `go build ./... && go test ./internal/report/`
Expected: PASS (3 new tests + existing unaffected).

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go main.go
git commit -m "feat(report): SECURITY section + securityIssues JSON"
```

---

### Task 6: Docs

**Files:**
- Modify: `CHANGELOG.md` (new `## [Unreleased]` → `### Added`, above `## [0.17.0]`)
- Modify: `website/docs/features/diagnostics.md`
- Modify: `website/docs/roadmap.md`
- Modify: `README.md`

**Interfaces:** none. Use exact names: flag `--security`; JSON `securityIssues`; profiles `baseline`/`restricted`/`kubeagent`.

- [ ] **Step 1: CHANGELOG**

Add a new `## [Unreleased]` section above the current top entry (`## [0.17.0] - 2026-07-11`):

```markdown
## [Unreleased]

### Added

- **Workload security posture.** Opt-in `scan --security` flags PSS-aligned
  hardening problems — privileged/over-privileged containers (privileged, host
  namespaces, hostPath, hostPort, dangerous added capabilities), insecure
  defaults (runs as root, privilege escalation allowed, capabilities not
  dropped), and exposed Services (NodePort/LoadBalancer/externalIPs) — in a
  dedicated `SECURITY` section and JSON `securityIssues`, each labelled
  `baseline`/`restricted`/`kubeagent`. Read-only and advisory (does not change
  the cluster verdict); needs no new RBAC; excludes system namespaces by default.
```

- [ ] **Step 2: diagnostics.md**

Run: `grep -nE '^#|^##|^###' website/docs/features/diagnostics.md | head -40`

Add a subsection after the ingress route health material, matching the heading level/style:

```markdown
### Security posture (opt-in)

`scan --security` walks every workload's pod template and each Service and flags
high-signal, Pod Security Standards-aligned problems: privileged or
over-privileged containers (privileged, host namespaces, `hostPath`, `hostPort`,
dangerous added capabilities), insecure container defaults (runs as root,
`allowPrivilegeEscalation` not disabled, capabilities not dropped), and Services
exposed outside the cluster (`NodePort` / `LoadBalancer` / `externalIPs`). Each
finding is labelled `baseline`, `restricted`, or `kubeagent` and printed in a
dedicated **SECURITY** section (also JSON `securityIssues`). It is a curated
subset aligned with the Pod Security Standards, not a conformance scanner. It is
read-only and **advisory** — it does not change the cluster verdict — needs no
extra RBAC, and skips `kube-system`/`kube-node-lease`/`kube-public` unless you
target one with `-n`.
```

- [ ] **Step 3: roadmap.md**

Run: `grep -nE 'Shipped|Version history' website/docs/roadmap.md | head`

Add a bullet to the "Shipped" list (before the `!!! info "Version history"` block), matching the existing bullet style:

```markdown
- **Workload security posture** — opt-in `scan --security` flags PSS-aligned
  hardening problems (privileged/insecure containers, exposed Services) in a
  `SECURITY` section and JSON `securityIssues`, labelled baseline/restricted/
  kubeagent. Read-only, advisory, no new RBAC. See
  [Failure diagnostics](features/diagnostics.md).
```

- [ ] **Step 4: README**

Run: `grep -nE 'disk-usage|ingress route|detect|lint-secrets' README.md | head`

Add a one-line mention of the security-posture check alongside the existing feature list, matching the surrounding style (mention the `--security` flag and that it is advisory/read-only).

- [ ] **Step 5: Verify the docs build**

Run:

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml
```

Expected: exit 0, "Documentation built", no page `WARNING` lines (the Material for MkDocs team banner is cosmetic baseline noise — ignore it).

- [ ] **Step 6: Commit**

```bash
git add CHANGELOG.md website/docs/features/diagnostics.md website/docs/roadmap.md README.md
git commit -m "docs: document workload security posture (scan --security)"
```

---

## Final verification (after all tasks)

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./...
```

Expected: all packages build; all tests pass. Manual smoke against any cluster with a privileged pod / LoadBalancer Service:

```bash
go build -o kubeagent . && ./kubeagent scan --security --output text | sed -n '/SECURITY/,/^$/p'
./kubeagent scan --security --output json | grep -o '"securityIssues"'
```

Expected: a `SECURITY` section listing `[baseline]`/`[restricted]`/`[kubeagent]` findings; `securityIssues` present in JSON when a workload is flagged.
