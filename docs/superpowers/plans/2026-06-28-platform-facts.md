# Platform Facts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a second line under the cluster verdict reporting the detected platform stack — CNI, ingress, storage, Kubernetes version+distro, container runtime, cloud — also in JSON and fed to `--explain`.

**Architecture:** A new pure package `internal/platform` detects `Facts` from cluster-wide inputs (nodes, kube-system DaemonSets, StorageClasses, IngressClasses) and renders a one-line summary via `Facts.Line()`. `internal/collect` gains three read-only List helpers. `report` and `explain` render the line (both via `Facts.Line()`); `main.go` wires it through as a `*platform.Facts` alongside the existing `*resources.Summary`.

**Tech Stack:** Go 1.26, client-go, `k8s.io/api/{core,apps,storage,networking}/v1` (all subpackages of the already-required `k8s.io/api`), stdlib `flag`/`strings`/`sort`.

## Global Constraints

- **READ-ONLY:** only new List calls (StorageClasses, IngressClasses, kube-system DaemonSets). Never create/update/patch/delete.
- **No new Go module dependency.** `k8s.io/api/storage/v1` and `k8s.io/api/networking/v1` are subpackages of `k8s.io/api`, already in go.mod.
- **Sequential**, stdlib `flag`, exit codes unchanged.
- **Cluster-wide regardless of `-n`:** facts come from cluster-scoped resources + an explicit kube-system DaemonSet list.
- **`--explain` egress:** only infrastructure **type names** — never pod IPs, per-node names, raw specs, secrets, or the raw `providerID` (only the derived cloud name).
- **Best-effort:** every fact degrades to omitted when undetected; List failures in `main` are non-fatal.
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` before any `go` command. Run `gofmt -l` on files you touch; fix with `gofmt -w`.
- **Scope (YAGNI):** the six facts only; no add-on detection; CNI/ingress/cloud via name-heuristics with documented "unknown" fallback.

---

### Task 1: `internal/platform` — detection + one-line summary (pure)

**Files:**
- Create: `internal/platform/platform.go`
- Test: `internal/platform/platform_test.go`

**Interfaces:**
- Produces:
  - `type Storage struct { Name string; Default bool }`
  - `type Facts struct { CNI, Ingress string; Storage []Storage; KubeVersion, Distro, Runtime, Cloud string }`
  - `func Detect(nodes []corev1.Node, systemDaemonSets []appsv1.DaemonSet, scs []storagev1.StorageClass, ics []networkingv1.IngressClass) Facts`
  - `func (f Facts) Line() string`

- [ ] **Step 1: Write the failing test**

Create `internal/platform/platform_test.go`:

```go
package platform

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ds(name string) appsv1.DaemonSet {
	return appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: name}}
}

func sc(name, provisioner string, isDefault bool) storagev1.StorageClass {
	s := storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Provisioner: provisioner}
	if isDefault {
		s.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return s
}

func ic(name, controller string) networkingv1.IngressClass {
	return networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: networkingv1.IngressClassSpec{Controller: controller}}
}

func node(kubelet, runtime, providerID string) corev1.Node {
	return corev1.Node{
		Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion:          kubelet,
			ContainerRuntimeVersion: runtime,
		}},
	}
}

func TestDetect_HetznerNovaCombo(t *testing.T) {
	f := Detect(
		[]corev1.Node{node("v1.35.4+rke2r1", "containerd://2.2.3-k3s1", "hcloud://131304002")},
		[]appsv1.DaemonSet{ds("cilium"), ds("hcloud-csi-node"), ds("rke2-traefik")},
		[]storagev1.StorageClass{sc("hcloud-volumes", "csi.hetzner.cloud", true), sc("hcloud-volumes-retain", "csi.hetzner.cloud", false), sc("nfs", "nfs.csi.k8s.io", false)},
		[]networkingv1.IngressClass{ic("traefik", "traefik.io/ingress-controller")},
	)
	if f.CNI != "Cilium" {
		t.Errorf("CNI = %q, want Cilium", f.CNI)
	}
	if f.Ingress != "Traefik" {
		t.Errorf("Ingress = %q, want Traefik", f.Ingress)
	}
	if len(f.Storage) != 2 || f.Storage[0].Name != "Hetzner CSI" || !f.Storage[0].Default || f.Storage[1].Name != "NFS CSI" {
		t.Errorf("Storage = %+v, want [Hetzner CSI(default), NFS CSI]", f.Storage)
	}
	if f.KubeVersion != "v1.35" || f.Distro != "RKE2" {
		t.Errorf("version/distro = %q/%q, want v1.35/RKE2", f.KubeVersion, f.Distro)
	}
	if f.Runtime != "containerd" || f.Cloud != "Hetzner Cloud" {
		t.Errorf("runtime/cloud = %q/%q, want containerd/Hetzner Cloud", f.Runtime, f.Cloud)
	}
	want := "Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud"
	if got := f.Line(); got != want {
		t.Errorf("Line()\n got %q\nwant %q", got, want)
	}
}

func TestDetect_FallbacksAndUnknowns(t *testing.T) {
	// Unknown CNI daemonset, raw ingress controller, raw provisioner, EKS distro, no providerID.
	f := Detect(
		[]corev1.Node{node("v1.29.4-eks-036c24b", "containerd://1.7.0", "")},
		[]appsv1.DaemonSet{ds("some-random-agent")},
		[]storagev1.StorageClass{sc("custom", "example.com/custom", false)},
		[]networkingv1.IngressClass{ic("x", "example.com/my-ingress")},
	)
	if f.CNI != "" {
		t.Errorf("CNI = %q, want empty (unknown)", f.CNI)
	}
	if f.Ingress != "example.com/my-ingress" {
		t.Errorf("Ingress = %q, want raw controller fallback", f.Ingress)
	}
	if len(f.Storage) != 1 || f.Storage[0].Name != "example.com/custom" {
		t.Errorf("Storage = %+v, want raw provisioner fallback", f.Storage)
	}
	if f.KubeVersion != "v1.29" || f.Distro != "EKS" {
		t.Errorf("version/distro = %q/%q, want v1.29/EKS", f.KubeVersion, f.Distro)
	}
	if f.Cloud != "" {
		t.Errorf("Cloud = %q, want empty (no providerID)", f.Cloud)
	}
}

func TestDetect_CNIVariants(t *testing.T) {
	cases := []struct{ dsName, want string }{
		{"calico-node", "Calico"},
		{"canal", "Canal"},
		{"kube-flannel-ds", "Flannel"}, // substring match
		{"weave-net", "Weave Net"},
		{"aws-node", "AWS VPC CNI"},
	}
	for _, c := range cases {
		f := Detect([]corev1.Node{}, []appsv1.DaemonSet{ds(c.dsName)}, nil, nil)
		if f.CNI != c.want {
			t.Errorf("ds %q: CNI = %q, want %q", c.dsName, f.CNI, c.want)
		}
	}
}

func TestLine_OmitsEmptyAndIsEmptyForZero(t *testing.T) {
	if got := (Facts{}).Line(); got != "" {
		t.Errorf("empty Facts Line() = %q, want empty", got)
	}
	f := Facts{CNI: "Cilium", Cloud: "AWS"} // only two facts
	if got := f.Line(); got != "Cilium CNI · AWS" {
		t.Errorf("Line() = %q, want \"Cilium CNI · AWS\"", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/platform/`
Expected: FAIL — package has no non-test files / `undefined: Detect`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/platform/platform.go`:

```go
// Package platform detects a cluster's platform stack — CNI, ingress, storage,
// Kubernetes version/distro, container runtime, and cloud — from cluster-wide
// resources. Detection is best-effort: an unrecognized fact is left empty.
package platform

import (
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
)

// Storage is one detected storage provisioner (friendly name) and whether it is
// the cluster default StorageClass.
type Storage struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// Facts is the detected platform stack. Every field is best-effort; an
// undetected fact is the zero value.
type Facts struct {
	CNI         string    `json:"cni,omitempty"`
	Ingress     string    `json:"ingress,omitempty"`
	Storage     []Storage `json:"storage,omitempty"`
	KubeVersion string    `json:"kubeVersion,omitempty"`
	Distro      string    `json:"distro,omitempty"`
	Runtime     string    `json:"runtime,omitempty"`
	Cloud       string    `json:"cloud,omitempty"`
}

// cniByDaemonSet maps a known CNI DaemonSet name fragment to its product name,
// in priority order (first match wins).
var cniByDaemonSet = []struct{ fragment, product string }{
	{"cilium", "Cilium"},
	{"calico-node", "Calico"},
	{"canal", "Canal"},
	{"kube-flannel", "Flannel"},
	{"weave-net", "Weave Net"},
	{"antrea-agent", "Antrea"},
	{"kube-ovn", "Kube-OVN"},
	{"aws-node", "AWS VPC CNI"},
}

var ingressByController = map[string]string{
	"traefik.io/ingress-controller":  "Traefik",
	"k8s.io/ingress-nginx":           "ingress-nginx",
	"haproxy.org/ingress-controller": "HAProxy",
	"projectcontour.io/contour":      "Contour",
	"ingress.k8s.aws/alb":            "AWS ALB",
}

var storageByProvisioner = map[string]string{
	"csi.hetzner.cloud":            "Hetzner CSI",
	"nfs.csi.k8s.io":               "NFS CSI",
	"ebs.csi.aws.com":              "AWS EBS",
	"pd.csi.storage.gke.io":        "GCE PD",
	"disk.csi.azure.com":           "Azure Disk",
	"driver.longhorn.io":           "Longhorn",
	"rancher.io/local-path":        "local-path",
	"kubernetes.io/no-provisioner": "static",
}

var cloudByScheme = map[string]string{
	"hcloud":       "Hetzner Cloud",
	"aws":          "AWS",
	"gce":          "GCP",
	"azure":        "Azure",
	"digitalocean": "DigitalOcean",
	"vsphere":      "vSphere",
}

// Detect derives platform Facts from cluster-wide inputs.
func Detect(nodes []corev1.Node, systemDaemonSets []appsv1.DaemonSet, scs []storagev1.StorageClass, ics []networkingv1.IngressClass) Facts {
	var f Facts
	f.CNI = detectCNI(systemDaemonSets)
	f.Ingress = detectIngress(ics)
	f.Storage = detectStorage(scs)
	if len(nodes) > 0 {
		n := nodes[0]
		f.KubeVersion, f.Distro = parseKubeVersion(n.Status.NodeInfo.KubeletVersion)
		f.Runtime = before(n.Status.NodeInfo.ContainerRuntimeVersion, "://")
		f.Cloud = cloudByScheme[before(n.Spec.ProviderID, "://")]
	}
	return f
}

func detectCNI(dss []appsv1.DaemonSet) string {
	for _, want := range cniByDaemonSet {
		for _, d := range dss {
			if strings.Contains(d.Name, want.fragment) {
				return want.product
			}
		}
	}
	return ""
}

func detectIngress(ics []networkingv1.IngressClass) string {
	var firstRaw string
	for _, c := range ics {
		ctrl := c.Spec.Controller
		if name, ok := ingressByController[ctrl]; ok {
			return name
		}
		if firstRaw == "" && ctrl != "" {
			firstRaw = ctrl
		}
	}
	return firstRaw
}

func detectStorage(scs []storagev1.StorageClass) []Storage {
	seen := map[string]int{} // name -> index in out
	var out []Storage
	for _, s := range scs {
		name := storageByProvisioner[s.Provisioner]
		if name == "" {
			name = s.Provisioner
		}
		if name == "" {
			continue
		}
		isDefault := s.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
		if i, ok := seen[name]; ok {
			if isDefault {
				out[i].Default = true
			}
			continue
		}
		seen[name] = len(out)
		out = append(out, Storage{Name: name, Default: isDefault})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// parseKubeVersion returns "vMAJOR.MINOR" and the distro inferred from the build
// metadata suffix (rke2/k3s/eks/gke), each empty when undetected.
func parseKubeVersion(v string) (version, distro string) {
	if v == "" {
		return "", ""
	}
	low := strings.ToLower(v)
	switch {
	case strings.Contains(low, "rke2"):
		distro = "RKE2"
	case strings.Contains(low, "k3s"):
		distro = "k3s"
	case strings.Contains(low, "eks"):
		distro = "EKS"
	case strings.Contains(low, "gke"):
		distro = "GKE"
	}
	base := v
	if i := strings.IndexByte(base, '+'); i >= 0 {
		base = base[:i]
	}
	parts := strings.Split(strings.TrimPrefix(base, "v"), ".")
	if len(parts) >= 2 {
		version = "v" + parts[0] + "." + parts[1]
	} else {
		version = base
	}
	return version, distro
}

// before returns s up to the first occurrence of sep, or "" when sep is absent.
func before(s, sep string) string {
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return ""
}

// Line renders the facts as a single human-readable summary, e.g.
// "Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud".
// It returns "" when no fact is set.
func (f Facts) Line() string {
	var parts []string
	if f.CNI != "" {
		parts = append(parts, f.CNI+" CNI")
	}
	if f.Ingress != "" {
		parts = append(parts, f.Ingress+" ingress")
	}
	if s := f.storageSummary(); s != "" {
		parts = append(parts, s+" storage")
	}
	if f.KubeVersion != "" {
		v := "Kubernetes " + f.KubeVersion
		if f.Distro != "" {
			v += " (" + f.Distro + ")"
		}
		parts = append(parts, v)
	}
	if f.Runtime != "" {
		parts = append(parts, f.Runtime)
	}
	if f.Cloud != "" {
		parts = append(parts, f.Cloud)
	}
	return strings.Join(parts, " · ")
}

func (f Facts) storageSummary() string {
	if len(f.Storage) == 0 {
		return ""
	}
	primary := f.Storage[0].Name
	var others []string
	for _, s := range f.Storage[1:] {
		others = append(others, s.Name)
	}
	if len(others) > 0 {
		return primary + " (+" + strings.Join(others, ", ") + ")"
	}
	return primary
}
```

Note: `before` returns `""` when the separator is absent — for `Runtime` this means a runtime string without `://` yields `""`; real nodes always include `://` (e.g. `containerd://…`). The `Line()` rendering of the storage segment is `<primary> (+<others>) storage`, matching the test's expected string.

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/platform/ -v`
Expected: PASS (all four tests). Then `go vet ./internal/platform/` and `gofmt -l internal/platform/` (no output).

- [ ] **Step 5: Commit**

```bash
git add internal/platform/
git commit -m "feat(platform): detect CNI/ingress/storage/version/runtime/cloud + one-line summary"
```

---

### Task 2: `internal/collect` — StorageClasses, IngressClasses, SystemDaemonSets

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces:
  - `func StorageClasses(ctx context.Context, client kubernetes.Interface) ([]storagev1.StorageClass, error)`
  - `func IngressClasses(ctx context.Context, client kubernetes.Interface) ([]networkingv1.IngressClass, error)`
  - `func SystemDaemonSets(ctx context.Context, client kubernetes.Interface) ([]appsv1.DaemonSet, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/collect/collect_test.go` (add imports `appsv1 "k8s.io/api/apps/v1"`, `networkingv1 "k8s.io/api/networking/v1"`, `storagev1 "k8s.io/api/storage/v1"`; it already imports `context`, `testing`, `corev1`, `metav1`, and `k8s.io/client-go/kubernetes/fake`):

```go
func TestStorageClasses_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Provisioner: "p1"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Provisioner: "p2"},
	)
	scs, err := StorageClasses(context.Background(), client)
	if err != nil {
		t.Fatalf("StorageClasses: %v", err)
	}
	if len(scs) != 2 {
		t.Errorf("want 2 storageclasses, got %d", len(scs))
	}
}

func TestIngressClasses_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "traefik"}, Spec: networkingv1.IngressClassSpec{Controller: "traefik.io/ingress-controller"}},
	)
	ics, err := IngressClasses(context.Background(), client)
	if err != nil {
		t.Fatalf("IngressClasses: %v", err)
	}
	if len(ics) != 1 || ics[0].Spec.Controller != "traefik.io/ingress-controller" {
		t.Errorf("unexpected ingressclasses: %+v", ics)
	}
}

func TestSystemDaemonSets_OnlyKubeSystem(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "fluentd"}},
	)
	dss, err := SystemDaemonSets(context.Background(), client)
	if err != nil {
		t.Fatalf("SystemDaemonSets: %v", err)
	}
	if len(dss) != 1 || dss[0].Name != "cilium" {
		t.Errorf("want only kube-system/cilium, got %+v", dss)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/`
Expected: FAIL — `undefined: StorageClasses` / `IngressClasses` / `SystemDaemonSets`.

- [ ] **Step 3: Write minimal implementation**

In `internal/collect/collect.go` add to imports `appsv1 "k8s.io/api/apps/v1"`, `networkingv1 "k8s.io/api/networking/v1"`, `storagev1 "k8s.io/api/storage/v1"`, then append:

```go
// StorageClasses lists all StorageClasses (cluster-scoped, read-only).
func StorageClasses(ctx context.Context, client kubernetes.Interface) ([]storagev1.StorageClass, error) {
	scs, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing storageclasses: %w", err)
	}
	return scs.Items, nil
}

// IngressClasses lists all IngressClasses (cluster-scoped, read-only).
func IngressClasses(ctx context.Context, client kubernetes.Interface) ([]networkingv1.IngressClass, error) {
	ics, err := client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ingressclasses: %w", err)
	}
	return ics.Items, nil
}

// SystemDaemonSets lists DaemonSets in kube-system (read-only) — used to detect
// the CNI regardless of the scan's namespace scope.
func SystemDaemonSets(ctx context.Context, client kubernetes.Interface) ([]appsv1.DaemonSet, error) {
	dss, err := client.AppsV1().DaemonSets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing kube-system daemonsets: %w", err)
	}
	return dss.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -v && go vet ./internal/collect/ && gofmt -l internal/collect/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list StorageClasses, IngressClasses, kube-system DaemonSets"
```

---

### Task 3: `internal/report` — Platform line + JSON field

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `platform.Facts`, `platform.Facts.Line()`.
- Produces (changed signature):
  `func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, explanation, format string, w io.Writer) error`

- [ ] **Step 1: Write the failing test**

In `internal/report/report_test.go` add `"github.com/imantaba/kubeagent/internal/platform"` to imports, add the tests below, and insert `nil` as the new **fourth** argument (after the summary argument) in **every existing** `PrintInventory(...)` call in the file (each currently passes the summary as the 3rd arg — `nil` or `sampleSummary()`; add `, nil` right after it):

```go
func sampleFacts() *platform.Facts {
	return &platform.Facts{
		CNI: "Cilium", Ingress: "Traefik",
		Storage:     []platform.Storage{{Name: "Hetzner CSI", Default: true}, {Name: "NFS CSI"}},
		KubeVersion: "v1.35", Distro: "RKE2", Runtime: "containerd", Cloud: "Hetzner Cloud",
	}
}

func TestPrintInventory_TextShowsPlatformLine(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, sampleFacts(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	want := "Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud"
	if !strings.Contains(out, want) {
		t.Errorf("missing platform line %q:\n%s", want, out)
	}
	// Platform must appear under the verdict (before any workloads / resources).
	if strings.Index(out, "Platform:") < strings.Index(out, "Cluster: Healthy") {
		t.Errorf("platform line should follow the cluster verdict:\n%s", out)
	}
}

func TestPrintInventory_TextOmitsPlatformWhenNilOrEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Platform:") {
		t.Errorf("no platform line expected for nil facts:\n%s", buf.String())
	}
	var buf2 bytes.Buffer
	if err := PrintInventory(ch, inventory.Result{}, nil, &platform.Facts{}, "", "text", &buf2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf2.String(), "Platform:") {
		t.Errorf("no platform line expected for empty facts:\n%s", buf2.String())
	}
}

func TestPrintInventory_JSONIncludesPlatform(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, sampleFacts(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		Platform *platform.Facts `json:"platform"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Platform == nil || got.Platform.CNI != "Cilium" || len(got.Platform.Storage) != 2 {
		t.Errorf("platform missing/wrong in JSON: %+v", got.Platform)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/`
Expected: FAIL — too many arguments / `undefined: platform`.

- [ ] **Step 3: Write minimal implementation**

In `internal/report/report.go`:

Add `"github.com/imantaba/kubeagent/internal/platform"` to imports.

Add to `inventoryReport` (after `Workloads`):

```go
	Platform    *platform.Facts             `json:"platform,omitempty"`
```

Change `PrintInventory` signature and the JSON/text dispatch:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

Change `printInventoryText` to accept `facts *platform.Facts` and render the Platform line inside the verdict block, immediately before the block's trailing `fmt.Fprintln(w)`:

```go
func printInventoryText(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, explanation string, w io.Writer) error {
	if cluster.Verdict != "" {
		// ... existing verdict line, node/system issues, scope note (unchanged) ...
		if facts != nil {
			if line := facts.Line(); line != "" {
				if _, err := fmt.Fprintf(w, "Platform: %s\n", line); err != nil {
					return err
				}
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}

	if err := printResources(summary, w); err != nil {
		return err
	}
	// ... rest unchanged ...
}
```

(The `facts` block goes after the `ScopeNote` `if` and before the existing trailing `fmt.Fprintln(w)` at the end of the `if cluster.Verdict != ""` block.)

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -v && go vet ./internal/report/ && gofmt -l internal/report/`
Expected: PASS (new + all existing), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): platform line under the verdict + JSON platform field"
```

---

### Task 4: `internal/explain` — Platform line in the prompt

**Files:**
- Modify: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `platform.Facts.Line()`.
- Produces (changed signatures):
  - `func (c *Client) ExplainInventory(ctx, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, workloads []inventory.Workload) (string, error)`
  - `func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, workloads []inventory.Workload) string`

- [ ] **Step 1: Write the failing test**

In `internal/explain/explain_test.go` add `"github.com/imantaba/kubeagent/internal/platform"` to imports, insert `nil` as the new **fourth** argument (after summary) in **every existing** `ExplainInventory(...)` call and as the new **third** argument (after summary) in every existing `buildInventoryPrompt(...)` call, then add:

```go
func TestBuildInventoryPrompt_IncludesPlatform(t *testing.T) {
	f := &platform.Facts{CNI: "Cilium", Ingress: "Traefik", KubeVersion: "v1.35", Distro: "RKE2", Runtime: "containerd", Cloud: "Hetzner Cloud"}
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, f, nil)
	if !strings.Contains(got, "Platform: Cilium CNI · Traefik ingress · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud") {
		t.Errorf("prompt missing platform line:\n%s", got)
	}
}

func TestBuildInventoryPrompt_OmitsPlatformWhenNil(t *testing.T) {
	got := buildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded", NodesTotal: 1, NodesReady: 0, NodeIssues: []string{"n1 NotReady"}}, nil, nil, nil)
	if strings.Contains(got, "Platform:") {
		t.Errorf("no platform line expected when facts nil:\n%s", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/`
Expected: FAIL — wrong arg count / `undefined: platform`.

- [ ] **Step 3: Write minimal implementation**

In `internal/explain/explain.go`: add `"github.com/imantaba/kubeagent/internal/platform"` to imports; change `ExplainInventory` to take `facts *platform.Facts` and forward it; change `buildInventoryPrompt` to take `facts` and render a Platform line after the degraded-cluster block and before the `summary` block:

```go
func (c *Client) ExplainInventory(ctx context.Context, cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, workloads []inventory.Workload) (string, error) {
	if cluster.Verdict != "Degraded" && len(workloads) == 0 {
		return "", nil
	}
	out, err := c.s.summarize(ctx, buildInventoryPrompt(cluster, summary, facts, workloads))
	// ... rest unchanged ...
}
```

```go
func buildInventoryPrompt(cluster clusterhealth.ClusterHealth, summary *resources.Summary, facts *platform.Facts, workloads []inventory.Workload) string {
	var b strings.Builder
	// ... existing degraded-cluster block unchanged ...

	if facts != nil {
		if line := facts.Line(); line != "" {
			fmt.Fprintf(&b, "Platform: %s\n\n", line)
		}
	}

	if summary != nil {
		// ... existing Cluster resources block unchanged ...
	}
	// ... rest unchanged ...
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -v && go vet ./internal/explain/ && gofmt -l internal/explain/`
Expected: PASS (new + all existing, including the retained egress-guard test), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): include the platform line in the prompt"
```

---

### Task 5: wire it in `main.go`, document, verify live

**Files:**
- Modify: `main.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: `collect.StorageClasses`/`IngressClasses`/`SystemDaemonSets`, `platform.Detect`, the new `report.PrintInventory` / `explain.ExplainInventory` signatures.

- [ ] **Step 1: Wire the pipeline in `main.go`**

Add `"github.com/imantaba/kubeagent/internal/platform"` to imports. After the line `summary := resources.Summarize(nodes, resourcePods, usage)`, insert:

```go
	scs, _ := collect.StorageClasses(context.Background(), client)
	ics, _ := collect.IngressClasses(context.Background(), client)
	sysDS, _ := collect.SystemDaemonSets(context.Background(), client)
	facts := platform.Detect(nodes, sysDS, scs, ics)
```

Update the two consumers to pass `&facts` right after `&summary`:

```go
		explanation, err = explain.New(explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))).ExplainInventory(ctx, health, &summary, &facts, result.Workloads)
```

```go
	return report.PrintInventory(health, result, &summary, &facts, explanation, *output, os.Stdout)
```

- [ ] **Step 2: Build, vet, gofmt, and run the whole suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go vet ./... && go test ./... && gofmt -l main.go && go build -o /tmp/kubeagent .`
Expected: all packages `ok`, `gofmt -l main.go` prints nothing, build succeeds.

- [ ] **Step 3: Verify live against the cluster**

Run: `/tmp/kubeagent scan -n cattle-system`
Expected: a `Platform:` line appears directly under the `Cluster:` verdict, e.g.
`Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud`

Run: `/tmp/kubeagent scan --output json | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["platform"], indent=2))'`
Expected: a `platform` object with `cni`, `ingress`, `storage`, `kubeVersion`, `distro`, `runtime`, `cloud`.

Paste both outputs into the report.

- [ ] **Step 4: Document in `README.md`**

In the scan/usage area (before `## Install`), add:

```markdown
### Platform facts

`scan` prints a second line under the cluster verdict naming the detected stack —
CNI, ingress, storage provisioner(s), Kubernetes version + distribution, container
runtime, and cloud — for example:

`Platform: Cilium CNI · Traefik ingress · Hetzner CSI (+NFS CSI) storage · Kubernetes v1.35 (RKE2) · containerd · Hetzner Cloud`

Detection is best-effort and read-only (it lists StorageClasses, IngressClasses,
and kube-system DaemonSets, and reads node info); an unrecognized fact is omitted.
The same summary is included in the JSON output (`platform`) and sent to
`--explain` so the model can give stack-aware advice. No instance identifiers
(e.g. the raw `providerID`) are emitted — only the derived cloud name.
```

- [ ] **Step 5: Commit**

```bash
git add main.go README.md
git commit -m "feat: wire platform facts into scan + explain; document"
```

---

## Self-Review

**Spec coverage:**
- `internal/platform` Facts + Detect (CNI/ingress/storage/version/distro/runtime/cloud, heuristics + fallbacks) → Task 1. ✓
- `Facts.Line()` shared one-line summary → Task 1 (used by report + explain). ✓
- collect StorageClasses/IngressClasses/SystemDaemonSets → Task 2. ✓
- text Platform line under the verdict; omitted when nil/empty → Task 3. ✓
- JSON `platform` field → Task 3. ✓
- explain Platform line; egress safe (type names only) → Task 4. ✓
- wiring cluster-wide (independent of `-n`); best-effort List failures → Task 5. ✓
- docs → Task 5. ✓

**Placeholder scan:** none — every step has concrete code/commands.

**Type consistency:** `platform.Facts`/`Storage`, `Detect`, `Facts.Line()`, the `*platform.Facts` parameter, and the `PrintInventory`/`ExplainInventory`/`buildInventoryPrompt` signatures are used identically across Tasks 1–5. The Line() storage format `"<primary> (+<others>) storage"` is asserted identically in Task 1 and Task 3.
