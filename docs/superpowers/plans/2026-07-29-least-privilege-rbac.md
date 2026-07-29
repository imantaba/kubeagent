# Least-privilege RBAC profiles — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the mapping from kubeagent feature to Kubernetes permission a single
machine-readable table, generate every shipped RBAC manifest from it, let an operator
ask what a feature costs and whether their identity has it, and make a missing grant
visible in scan output instead of silent.

**Architecture:** A new pure package `internal/rbacprofile` holds one `Feature` table.
`manifest.go` renders `deploy/rbac.yaml`, the seven `deploy/rbac-*.yaml` add-ons and the
Helm ClusterRole from that table; a golden test regenerates them with `-update` and fails
on drift, so generation and assertion are the same code path. `check.go` turns the same
table into `SelfSubjectAccessReview` probes behind `kubeagent rbac check`. Finally
`internal/scan` records a blind spot whenever a feature-flagged read is refused, so a
missing grant reads as a named gap rather than an empty section.

**Tech Stack:** Go 1.26, standard-library `flag`, client-go (`kubernetes.Interface`,
`k8s.io/api/authorization/v1`, the fake clientset for tests), bash for the chaos harness.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every commit needs a `Signed-off-by` trailer matching its author** — use `git commit -s`.
  `main` enforces DCO. Identity: `imantaba <itn.taba@gmail.com>`. Verify with `scripts/dco-check.sh main`.
- **No `Co-Authored-By: Claude` trailer and no AI attribution of any kind** anywhere: commit
  messages, code comments, docs, CHANGELOG entries. Every commit is authored solely by the human.
- **v1 uses the standard-library `flag` package only — no Cobra.** New subcommands use
  `flag.NewFlagSet(name, flag.ContinueOnError)`, matching `runGate` in `main.go`.
- **`internal/rbacprofile` must never import `internal/remediate` or `internal/explain`.**
  There is no code path from it into a write or into a model call.
- **Blocked reasons are kubeagent's own words, never the API server's.** Never read
  `SelfSubjectAccessReview.Status.Reason` and never interpolate an `apierrors` message into
  user-visible text: both carry the authorizer's own string, which embeds the requesting
  identity (IAM ARN, OIDC email, internal DNS name) and, under webhook authorization,
  third-party free text. Follow the precedent in
  [internal/htmlreport/htmlreport.go:102-139](internal/htmlreport/htmlreport.go#L102-L139) —
  classify, never quote.
- **`internal/htmlreport.safeReason` classifies by substring.** Any new blind-spot reason
  string that means "permission denied" MUST contain the lowercase substring `forbidden`, or
  the HTML report silently degrades it to the generic "the read failed" phrase.
- **No secrets, credentials, private IPs or internal hostnames** in code, tests, docs or
  fixtures — use `<PLACEHOLDER>`. Documentation and test IPs must be RFC 5737
  (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains RFC 2606 (`.example`).
- **URLs and kubeconfig paths are credentials.** No log line, error, results file or doc
  example may carry more than `scheme://host`, and no forwarded artifact may carry a
  kubeconfig path or a ServiceAccount token.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** If a change to
  `internal/scan` would alter it, that is a defect in this plan — stop and escalate.
- **`go test` runs with `-p 2`**: `go test -p 2 ./...`. Full parallelism trips a known Go
  linker panic on `internal/advisory`. Never use `-short`.
- **`deploy/rbac.yaml` keeps the `kubeagent-readonly` ClusterRole name and its
  `get, list, watch` verbs**, and the seven add-on manifests keep their existing role names
  (`kubeagent-certs`, `kubeagent-pods-log`, `kubeagent-nodes-proxy`, `kubeagent-dnshealth`,
  `kubeagent-controlplane`, `kubeagent-operators`, `kubeagent-gitops`), so existing installs
  do not break.
- **Go is at `/usr/local/go/bin`** — `export PATH=$PATH:/usr/local/go/bin` before building.
- kubeagent is **read-only toward the cluster** by default. `--fix` is the sole write path
  and this plan does not touch it. `kubeagent rbac check` issues `SelfSubjectAccessReview`
  POSTs, which persist nothing; that carve-out is documented in Task 6.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/rbacprofile/profile.go` | The `Rule`/`Feature` types, the feature table, profiles, rule merging and watch-verb elevation. Pure: no client, no context, no I/O. |
| `internal/rbacprofile/profile_test.go` | Table invariants (no write verbs, every feature well-formed) and merge/elevation behaviour. |
| `internal/rbacprofile/manifest.go` | YAML rendering: a standalone ClusterRole, the base manifest, each add-on manifest, the Helm ClusterRole template. Pure string building. |
| `internal/rbacprofile/manifest_test.go` | Unit tests for the rendering helpers. |
| `internal/rbacprofile/golden_test.go` | Writes the nine generated files under `-update`; otherwise asserts them byte-for-byte. |
| `internal/rbacprofile/check.go` | `Action`, `FeatureStatus`, `Check` — the only file that talks to a cluster. |
| `internal/rbacprofile/check_test.go` | `Check` against the fake clientset with a reactor that allows/denies specific attributes. |
| `main.go` | `rbac` dispatch, `runRBACPrint`, `runRBACCheck`, the `selectedRules`/`selectedFeatures` helpers, and `advisoryBlindSpots`. |
| `main_test.go` | Tests for the pure helpers above. |
| `internal/collect/collect.go` | `NodeStats` and `PreviousLogs` widened so "forbidden" is distinguishable from "absent". |
| `internal/scan/scan.go` | The `blind()` helper and the feature-collector wiring. |
| `deploy/*.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml` | Generated output — never hand-edited after Task 2. |
| `chaos/run.sh` | `scenario_20_rbac`, a real least-privilege identity against a real cluster. |
| `website/docs/features/rbac.md`, `website/mkdocs.yml`, `deploy/README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md` | Documentation. |

**Key ordering decision:** the feature table is ordered so that rendering the Helm
ClusterRole in table order reproduces the file that ships today. Chart-gated features come
first in the chart's existing order (`core`, `diskusage`, `kubelethealth`, `dnshealth`,
`controlplane`, `certs`), then the scan-only ones (`logs`, `operators`, `gitops`), then the
features that need no grant at all.

**Not in scope, deliberately:** `logs`, `operators` and `gitops` are scan-only. The watch
daemon reads no container logs and no operator/GitOps custom resources, which is why the
chart has no toggle for them — see the headers of `deploy/rbac-logs.yaml`,
`deploy/rbac-operators.yaml` and `deploy/rbac-gitops.yaml`, which say so explicitly. That is
intent, not drift. Task 1 encodes it as a `ScanOnly` field with a test, rather than adding
chart values for grants the daemon would never use.

---

### Task 1: The feature table

**Files:**

- Create: `internal/rbacprofile/profile.go`
- Test: `internal/rbacprofile/profile_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces: `Rule`, `Feature`, `Profile`, `Features() []Feature`, `Lookup(name string) (Feature, bool)`,
  `Profiles() []Profile`, `ProfileByName(name string) (Profile, bool)`,
  `Resolve(p Profile) ([]Rule, error)`, `MergeRules(rules []Rule) []Rule`,
  `WithWatch(rules []Rule) []Rule`, and the exported verb slices `ReadVerbs`, `ListOnly`, `GetOnly`.

- [ ] **Step 1: Write the failing test**

Create `internal/rbacprofile/profile_test.go`:

```go
package rbacprofile

import (
	"strings"
	"testing"
)

// The whole point of this package is that kubeagent asks for nothing it cannot
// justify. A write verb reaching the table would be shipped to every operator
// as a manifest, so it is the one thing tested first.
func TestTableGrantsOnlyReadVerbs(t *testing.T) {
	allowed := map[string]bool{"get": true, "list": true, "watch": true}
	for _, f := range Features() {
		for _, r := range f.Rules {
			for _, v := range r.Verbs {
				if !allowed[v] {
					t.Errorf("feature %q asks for verb %q; only get/list/watch may appear", f.Name, v)
				}
			}
		}
	}
}

func TestEveryFeatureIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range Features() {
		if f.Name == "" {
			t.Fatal("a feature has an empty Name")
		}
		if seen[f.Name] {
			t.Errorf("duplicate feature name %q", f.Name)
		}
		seen[f.Name] = true
		if f.Summary == "" {
			t.Errorf("feature %q has no Summary; `kubeagent rbac` would print a blank line", f.Name)
		}
		if f.Name != "core" && f.Flag == "" {
			t.Errorf("feature %q has no Flag; only core is unflagged", f.Name)
		}
		for _, r := range f.Rules {
			if len(r.Resources) == 0 && len(r.NonResourceURLs) == 0 {
				t.Errorf("feature %q has a rule naming neither resources nor URLs", f.Name)
			}
			if len(r.Resources) > 0 && len(r.NonResourceURLs) > 0 {
				t.Errorf("feature %q mixes resources and nonResourceURLs in one rule", f.Name)
			}
			if len(r.Verbs) == 0 {
				t.Errorf("feature %q has a rule with no verbs", f.Name)
			}
		}
	}
}

// Where a feature's grant lives is not a comment, it is data: either the feature
// ships its own manifest, or another feature's manifest already covers it.
func TestEveryGrantHasExactlyOneHome(t *testing.T) {
	for _, f := range Features() {
		if len(f.Rules) == 0 {
			if f.Manifest != "" || f.CoveredBy != "" {
				t.Errorf("feature %q needs no grant but claims a manifest", f.Name)
			}
			continue
		}
		if (f.Manifest == "") == (f.CoveredBy == "") {
			t.Errorf("feature %q must set exactly one of Manifest and CoveredBy", f.Name)
		}
		if f.CoveredBy != "" {
			if _, ok := Lookup(f.CoveredBy); !ok {
				t.Errorf("feature %q is CoveredBy unknown feature %q", f.Name, f.CoveredBy)
			}
			continue
		}
		if f.RoleName == "" {
			t.Errorf("feature %q ships manifest %q with no RoleName", f.Name, f.Manifest)
		}
		if f.Doc == "" {
			t.Errorf("feature %q ships manifest %q with no Doc header", f.Name, f.Manifest)
		}
	}
}

// The daemon reads no container logs and no custom resources, so those grants are
// deliberately absent from the chart. Encoding that as data keeps a later reader
// from "fixing" the gap.
func TestScanOnlyFeaturesAreNotChartGated(t *testing.T) {
	for _, f := range Features() {
		// core is always present in the chart (it is the base manifest, not a
		// gated add-on), so — like ScanOnly features — it carries no
		// HelmCondition. See the HelmCondition field doc.
		if f.Name == "core" || len(f.Rules) == 0 || f.CoveredBy != "" {
			continue
		}
		if f.ScanOnly && f.HelmCondition != "" {
			t.Errorf("feature %q is scan-only but carries a Helm condition", f.Name)
		}
		if !f.ScanOnly && f.HelmCondition == "" {
			t.Errorf("feature %q is used by the daemon but has no Helm condition", f.Name)
		}
	}
}

func TestScanProfileIsCoreAtGetList(t *testing.T) {
	p, ok := ProfileByName("scan")
	if !ok {
		t.Fatal("no scan profile")
	}
	rules, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 10 {
		t.Fatalf("scan profile has %d rules, want the 10 core API groups", len(rules))
	}
	for _, r := range rules {
		if strings.Join(r.Verbs, ",") != "get,list" {
			t.Errorf("group %q has verbs %v, want [get list]", r.APIGroup, r.Verbs)
		}
	}
}

func TestWatchProfileElevatesCoreOnly(t *testing.T) {
	p, _ := ProfileByName("watch")
	rules, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if strings.Join(r.Verbs, ",") != "get,list,watch" {
			t.Errorf("group %q has verbs %v, want [get list watch]", r.APIGroup, r.Verbs)
		}
	}
}

// gitops's three rules are a subset of operators'. Asking for both must not emit
// the same grant twice, or `kubeagent rbac print --profile full` prints a role no
// reviewer would sign off.
func TestResolveMergesOverlappingFeatures(t *testing.T) {
	rules, err := Resolve(Profile{Name: "custom", Features: []string{"operators", "gitops"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if seen[r.APIGroup] {
			t.Errorf("API group %q appears in two rules with the same verbs", r.APIGroup)
		}
		seen[r.APIGroup] = true
	}
	if len(rules) != 7 {
		t.Errorf("operators+gitops resolved to %d rules, want operators' 7", len(rules))
	}
}

func TestFullProfileCoversEveryFeature(t *testing.T) {
	p, _ := ProfileByName("full")
	for _, f := range Features() {
		found := false
		for _, name := range p.Features {
			if name == f.Name {
				found = true
			}
		}
		if !found {
			t.Errorf("feature %q is missing from the full profile", f.Name)
		}
	}
}

func TestResolveRejectsUnknownFeature(t *testing.T) {
	if _, err := Resolve(Profile{Name: "custom", Features: []string{"nope"}}); err == nil {
		t.Fatal("Resolve accepted an unknown feature name")
	}
}

// Callers must not be able to edit the shipped table through the slice they get back.
func TestFeaturesReturnsACopy(t *testing.T) {
	first := Features()
	first[0].Name = "clobbered"
	if Features()[0].Name == "clobbered" {
		t.Fatal("Features() handed out the package's own slice")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test -p 2 ./internal/rbacprofile/
```

Expected: FAIL — the package does not build (`undefined: Features`).

- [ ] **Step 3: Write the implementation**

Create `internal/rbacprofile/profile.go`. The table below is the whole point of the
package; copy the `Doc` strings **verbatim** from the header comments of the existing
`deploy/rbac-*.yaml` files (they are shown in full here — do not paraphrase them, they
are shipped documentation).

```go
// Package rbacprofile is the single source of truth for which Kubernetes
// permissions each kubeagent feature needs.
//
// The manifests under deploy/ and the Helm ClusterRole are generated from the
// table here (see manifest.go and golden_test.go), and `kubeagent rbac` reads
// the same table both to print a minimal role and to check a live identity
// against it. One table, so a grant cannot drift from the code that uses it.
//
// This file and manifest.go are pure: no client, no context, no I/O. check.go
// is the one file that talks to a cluster.
package rbacprofile

import (
	"fmt"
	"sort"
	"strings"
)

// The verb sets the table uses. Every one is read-only. kubeagent issues no
// writes outside --fix, which this table deliberately does not describe: a
// remediation runs against the operator's own credentials, not a shipped role.
var (
	// ReadVerbs is what a one-shot command needs.
	ReadVerbs = []string{"get", "list"}
	// ListOnly is for collections kubeagent only ever enumerates.
	ListOnly = []string{"list"}
	// GetOnly is for subresources and non-resource URLs.
	GetOnly = []string{"get"}
)

// Rule is one RBAC rule: either resources in an API group, or non-resource
// URLs, plus the verbs needed. Exactly one of Resources and NonResourceURLs is
// set.
type Rule struct {
	APIGroup        string   `json:"apiGroup"`
	Resources       []string `json:"resources,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
	Verbs           []string `json:"verbs"`
}

// Feature is one thing an operator can turn on, and what it costs in grants.
type Feature struct {
	// Name identifies the feature to `kubeagent rbac --features`.
	Name string
	// Flag is the scan flag that enables it; empty for core.
	Flag string
	// Summary is the one-line description `kubeagent rbac` prints.
	Summary string
	// Doc is the header comment the generated manifest carries. Empty when the
	// feature ships no manifest of its own.
	Doc string
	// Manifest is the file under deploy/ generated for this feature; empty when
	// another feature's manifest covers it or it needs no grant.
	Manifest string
	// RoleName is the ClusterRole name inside that manifest.
	RoleName string
	// CoveredBy names the feature whose manifest already grants these rules.
	// Set for features that share a grant: kubelet-health and disk-usage both
	// read nodes/proxy, so one manifest serves both.
	CoveredBy string
	// ScanOnly marks a feature the watch daemon never uses, so the Helm chart
	// deliberately gates no grant for it.
	ScanOnly bool
	// HelmCondition is the raw template condition gating this feature's rules in
	// the chart. Empty for core (always present) and for ScanOnly features.
	HelmCondition string
	// Rules is what the feature needs beyond core. Empty means core suffices.
	Rules []Rule
}

// coreRules is what every kubeagent command reads, in the order the shipped
// manifests have carried for releases. Changing the order rewrites every
// generated file, so leave it alone unless a rule is genuinely added.
var coreRules = []Rule{
	{APIGroup: "", Resources: []string{"pods", "nodes", "services", "configmaps", "events", "persistentvolumeclaims", "persistentvolumes", "namespaces", "resourcequotas"}, Verbs: ReadVerbs},
	{APIGroup: "apps", Resources: []string{"deployments", "replicasets", "statefulsets", "daemonsets"}, Verbs: ReadVerbs},
	{APIGroup: "batch", Resources: []string{"jobs", "cronjobs"}, Verbs: ReadVerbs},
	{APIGroup: "discovery.k8s.io", Resources: []string{"endpointslices"}, Verbs: ReadVerbs},
	{APIGroup: "networking.k8s.io", Resources: []string{"networkpolicies", "ingressclasses", "ingresses"}, Verbs: ReadVerbs},
	{APIGroup: "storage.k8s.io", Resources: []string{"storageclasses"}, Verbs: ReadVerbs},
	{APIGroup: "coordination.k8s.io", Resources: []string{"leases"}, Verbs: ReadVerbs},
	{APIGroup: "policy", Resources: []string{"poddisruptionbudgets"}, Verbs: ReadVerbs},
	{APIGroup: "autoscaling", Resources: []string{"horizontalpodautoscalers"}, Verbs: ReadVerbs},
	{APIGroup: "admissionregistration.k8s.io", Resources: []string{"validatingwebhookconfigurations", "mutatingwebhookconfigurations"}, Verbs: ReadVerbs},
}

// features is ordered so that rendering the Helm ClusterRole in table order
// reproduces the file that ships today: core, then the chart-gated add-ons in
// the chart's existing order, then the scan-only ones, then the features that
// cost no extra grant at all.
var features = []Feature{
	{
		Name:    "core",
		Summary: "the inventory every command reads: pods, nodes, workloads, events, services, PVCs and the rest",
		Doc:     "Strictly read-only: get/list/watch only. No create/update/patch/delete anywhere.",
		// The base manifest is rendered by RenderBaseManifest, not RenderAddonManifest,
		// because it also carries the ServiceAccount. Manifest names it so the golden
		// test can find it; RoleName is the name existing installs already bind.
		Manifest: "rbac.yaml",
		RoleName: "kubeagent-readonly",
		Rules:    coreRules,
	},
	{
		Name:          "diskusage",
		Flag:          "--disk-usage",
		Summary:       "node filesystem and inode pressure, read from the kubelet summary API",
		Manifest:      "rbac-diskusage.yaml",
		RoleName:      "kubeagent-nodes-proxy",
		HelmCondition: "if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled",
		Doc: `Opt-in add-on: grants the kubeagent ServiceAccount read access to the kubelet
via the nodes/proxy subresource — both the summary API (disk-usage) and
/healthz (kubelet-health). Needed when the daemon runs with
KUBEAGENT_DISK_USAGE=true or KUBEAGENT_KUBELET_HEALTH=true (or scan is run with
--disk-usage or --kubelet-health). Apply alongside deploy/ to enable those
checks. Without it, kubeagent stays strictly get/list/watch.`,
		Rules: []Rule{{APIGroup: "", Resources: []string{"nodes/proxy"}, Verbs: GetOnly}},
	},
	{
		Name:      "kubelethealth",
		Flag:      "--kubelet-health",
		Summary:   "each kubelet's /healthz, to tell a sick kubelet from a sick node",
		CoveredBy: "diskusage",
		Rules:     []Rule{{APIGroup: "", Resources: []string{"nodes/proxy"}, Verbs: GetOnly}},
	},
	{
		Name:          "dnshealth",
		Flag:          "--dns-health",
		Summary:       "CoreDNS SERVFAIL/REFUSED ratio, read from each CoreDNS pod's metrics endpoint",
		Manifest:      "rbac-dnshealth.yaml",
		RoleName:      "kubeagent-dnshealth",
		HelmCondition: "if .Values.dnsHealth.enabled",
		Doc: `Optional add-on grant for ` + "`kubeagent scan --dns-health`" + ` (and the daemon with
KUBEAGENT_DNS_HEALTH=true): read each CoreDNS pod's :9153/metrics via the
pods/proxy subresource to flag an elevated SERVFAIL+REFUSED response ratio.
Strictly read-only. Apply alongside deploy/rbac.yaml when the dns-health check is used.`,
		Rules: []Rule{{APIGroup: "", Resources: []string{"pods/proxy"}, Verbs: GetOnly}},
	},
	{
		Name:          "controlplane",
		Flag:          "--control-plane-health",
		Summary:       "the apiserver /readyz endpoint, to flag an unhealthy control plane or etcd",
		Manifest:      "rbac-controlplane.yaml",
		RoleName:      "kubeagent-controlplane",
		HelmCondition: "if .Values.controlPlaneHealth.enabled",
		Doc: `Optional add-on grant for ` + "`kubeagent scan --control-plane-health`" + ` (and the daemon
with KUBEAGENT_CONTROL_PLANE_HEALTH=true): read the apiserver /readyz endpoint to
flag an unhealthy control plane / etcd. Strictly read-only (a single nonResourceURL
GET). Apply alongside deploy/rbac.yaml when the control-plane-health check is used.`,
		Rules: []Rule{{NonResourceURLs: []string{"/readyz"}, Verbs: GetOnly}},
	},
	{
		Name:          "certs",
		Flag:          "--certs",
		Summary:       "TLS certificate expiry, read from the public tls.crt of kubernetes.io/tls Secrets",
		Manifest:      "rbac-certs.yaml",
		RoleName:      "kubeagent-certs",
		HelmCondition: "if .Values.certs.enabled",
		Doc: `Opt-in add-on: grants the kubeagent ServiceAccount LIST access to Secrets so
the --certs / KUBEAGENT_CERTS certificate-expiry check can read the PUBLIC
certificate (tls.crt) of kubernetes.io/tls Secrets. kubeagent never reads
tls.key and never prints secret values. Apply alongside deploy/ to enable the
check. Without it, kubeagent makes no Secrets API calls at all.
Note: Kubernetes RBAC cannot restrict Secret access per-field; the application code is what never accesses tls.key.`,
		Rules: []Rule{{APIGroup: "", Resources: []string{"secrets"}, Verbs: ListOnly}},
	},
	{
		Name:     "logs",
		Flag:     "--logs",
		Summary:  "the last lines of a crashed container's previous log, to name the cause",
		Manifest: "rbac-logs.yaml",
		RoleName: "kubeagent-pods-log",
		ScanOnly: true,
		Doc: `Opt-in add-on: grants read access to container logs via the pods/log subresource,
needed only when running ` + "`scan --logs`" + `. Apply alongside deploy/ for a restricted
context; most human kubeconfigs already allow pods/log. Without it, --logs simply
reports no log cause (non-fatal). kubeagent stays strictly get/list/watch otherwise.`,
		Rules: []Rule{{APIGroup: "", Resources: []string{"pods/log"}, Verbs: GetOnly}},
	},
	{
		Name:     "operators",
		Flag:     "--operators",
		Summary:  "health of installed operators: cert-manager, CNPG, Longhorn, Argo CD, Flux, Prometheus",
		Manifest: "rbac-operators.yaml",
		RoleName: "kubeagent-operators",
		ScanOnly: true,
		Doc: `Opt-in add-on: grants list access to the operator custom resources ` + "`scan\n--operators`" + ` reads. Apply alongside deploy/ for a restricted context; most
human kubeconfigs already allow these. Without it, --operators still names
which operators are installed (API discovery is open to every authenticated
user) and marks each kind as forbidden — a useful answer, not an error.

Scan-only: the watch daemon does not read operator CRDs, so this is not wired
into the Helm chart. list only — kubeagent never writes to a CRD.`,
		Rules: []Rule{
			{APIGroup: "cert-manager.io", Resources: []string{"certificates", "issuers", "clusterissuers"}, Verbs: ListOnly},
			{APIGroup: "postgresql.cnpg.io", Resources: []string{"clusters"}, Verbs: ListOnly},
			{APIGroup: "longhorn.io", Resources: []string{"volumes"}, Verbs: ListOnly},
			{APIGroup: "argoproj.io", Resources: []string{"applications"}, Verbs: ListOnly},
			{APIGroup: "kustomize.toolkit.fluxcd.io", Resources: []string{"kustomizations"}, Verbs: ListOnly},
			{APIGroup: "helm.toolkit.fluxcd.io", Resources: []string{"helmreleases"}, Verbs: ListOnly},
			{APIGroup: "monitoring.coreos.com", Resources: []string{"prometheuses", "servicemonitors"}, Verbs: ListOnly},
		},
	},
	{
		Name:     "gitops",
		Flag:     "--drift",
		Summary:  "GitOps reconciler drift: Argo CD Applications, Flux Kustomizations and HelmReleases",
		Manifest: "rbac-gitops.yaml",
		RoleName: "kubeagent-gitops",
		ScanOnly: true,
		Doc: `Opt-in add-on: grants list access to the three GitOps custom resources ` + "`scan\n--drift`" + ` reads. Apply alongside deploy/ for a restricted context; most human
kubeconfigs already allow these. Without it, --drift still names which
reconciler is installed (API discovery is open to every authenticated user)
and marks each kind as forbidden — a useful answer, not an error.

Its three rules are a subset of deploy/rbac-operators.yaml, so applying that
file alone is enough to run both flags; this one exists so a drift-only user
needs no grant on Longhorn volumes or CNPG clusters.

Scan-only: the watch daemon does not read GitOps CRDs, so this is not wired
into the Helm chart. list only — kubeagent never writes to a CRD.`,
		Rules: []Rule{
			{APIGroup: "argoproj.io", Resources: []string{"applications"}, Verbs: ListOnly},
			{APIGroup: "kustomize.toolkit.fluxcd.io", Resources: []string{"kustomizations"}, Verbs: ListOnly},
			{APIGroup: "helm.toolkit.fluxcd.io", Resources: []string{"helmreleases"}, Verbs: ListOnly},
		},
	},
	// Everything below costs nothing beyond core. Listing them is the point:
	// `kubeagent rbac` should answer "what does --capacity cost me?" with
	// "nothing", not with silence.
	{Name: "capacity", Flag: "--capacity", Summary: "node headroom and scheduling capacity — no grant beyond core"},
	{Name: "security", Flag: "--security", Summary: "workload security posture (privileged, hostPath, hostNetwork) — no grant beyond core"},
	{Name: "pvcreclaim", Flag: "--pvc-reclaim", Summary: "released PersistentVolumes left behind by deleted claims — no grant beyond core"},
	{Name: "credlint", Flag: "--lint-secrets", Summary: "credentials visible in workload env vars — no grant beyond core"},
	{Name: "cronjobs", Flag: "--include-cron", Summary: "CronJob and Job history in the inventory — no grant beyond core"},
	{Name: "restarts", Flag: "--include-restarts", Summary: "containers restarting without crash-looping — no grant beyond core"},
}

// Features returns the table. The slice and its rules are copies: a caller that
// edits what it gets back cannot change what kubeagent asks the API server for.
func Features() []Feature {
	out := make([]Feature, len(features))
	for i, f := range features {
		f.Rules = copyRules(f.Rules)
		out[i] = f
	}
	return out
}

// Lookup finds a feature by name.
func Lookup(name string) (Feature, bool) {
	for _, f := range Features() {
		if f.Name == name {
			return f, true
		}
	}
	return Feature{}, false
}

// Profile is a named feature set: what one way of running kubeagent needs.
type Profile struct {
	// Name is what --profile accepts.
	Name string
	// Features lists feature names, resolved in order.
	Features []string
	// Watch elevates core's read verbs to also allow watch. Only the daemon
	// opens watches; a one-shot scan never does.
	Watch bool
}

// Profiles returns the built-in profiles.
func Profiles() []Profile {
	return []Profile{
		{Name: "scan", Features: []string{"core"}},
		{Name: "watch", Features: []string{"core"}, Watch: true},
		{Name: "full", Features: allFeatureNames()},
	}
}

// ProfileByName finds a built-in profile.
func ProfileByName(name string) (Profile, bool) {
	for _, p := range Profiles() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

func allFeatureNames() []string {
	names := make([]string, 0, len(features))
	for _, f := range features {
		names = append(names, f.Name)
	}
	return names
}

// Resolve turns a profile into the rules a ClusterRole should carry.
func Resolve(p Profile) ([]Rule, error) {
	var rules []Rule
	for _, name := range p.Features {
		f, ok := Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown feature %q", name)
		}
		rules = append(rules, f.Rules...)
	}
	merged := MergeRules(rules)
	if p.Watch {
		merged = WithWatch(merged)
	}
	return merged, nil
}

// MergeRules folds rules naming the same API group with the same verbs into
// one, unioning their resources. Order is first-appearance, so a generated
// manifest keeps the table's declared order and a diff stays readable.
func MergeRules(rules []Rule) []Rule {
	byKey := map[string]*Rule{}
	var order []string
	for _, r := range rules {
		key := ruleKey(r)
		existing, ok := byKey[key]
		if !ok {
			cp := r
			cp.Resources = append([]string(nil), r.Resources...)
			cp.NonResourceURLs = append([]string(nil), r.NonResourceURLs...)
			cp.Verbs = append([]string(nil), r.Verbs...)
			byKey[key] = &cp
			order = append(order, key)
			continue
		}
		existing.Resources = union(existing.Resources, r.Resources)
		existing.NonResourceURLs = union(existing.NonResourceURLs, r.NonResourceURLs)
	}
	out := make([]Rule, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	return out
}

// ruleKey is what makes two rules mergeable: the same API group (or both
// non-resource) asked for with the same verbs.
func ruleKey(r Rule) string {
	verbs := append([]string(nil), r.Verbs...)
	sort.Strings(verbs)
	if len(r.NonResourceURLs) > 0 {
		return "url\x00" + strings.Join(verbs, ",")
	}
	return "res\x00" + r.APIGroup + "\x00" + strings.Join(verbs, ",")
}

// union appends the members of b that a lacks, preserving a's order. Sorting
// would reorder resource lists the manifests have shipped for releases.
func union(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			seen[v] = true
			a = append(a, v)
		}
	}
	return a
}

// WithWatch elevates rules whose verbs are exactly ReadVerbs to also allow
// watch. Rules with other verbs — a list-only add-on, a subresource get — are
// returned unchanged: kubeagent never opens a watch on them.
func WithWatch(rules []Rule) []Rule {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if strings.Join(r.Verbs, ",") == strings.Join(ReadVerbs, ",") {
			r.Verbs = []string{"get", "list", "watch"}
		}
		out = append(out, r)
	}
	return out
}

func copyRules(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, r := range rules {
		r.Resources = append([]string(nil), r.Resources...)
		r.NonResourceURLs = append([]string(nil), r.NonResourceURLs...)
		r.Verbs = append([]string(nil), r.Verbs...)
		out[i] = r
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/rbacprofile/ -v
```

Expected: PASS, all eleven tests.

- [ ] **Step 5: Add a Go-concepts entry**

This task introduces **struct tags on a table used for two different outputs**
(`json:"apiGroup"` on `Rule`, read only by `--output json`). Append one entry to
`docs/go-concepts.md` in the established style — a plain everyday example first, then the
kubeagent one. No Python comparisons. One example is enough.

- [ ] **Step 6: Commit**

```bash
git add internal/rbacprofile/profile.go internal/rbacprofile/profile_test.go docs/go-concepts.md
git commit -s -m "feat(rbac): add the rbacprofile feature table

One Go table maps each kubeagent feature to the exact API groups, resources and
verbs it needs. The manifests, the Helm chart and the new rbac command all read
it, so a grant can no longer drift from the code that uses it."
```

---

### Task 2: Generate the manifests from the table

**Files:**

- Create: `internal/rbacprofile/manifest.go`
- Create: `internal/rbacprofile/manifest_test.go`
- Create: `internal/rbacprofile/golden_test.go`
- Modify (regenerated, never hand-edited afterwards): `deploy/rbac.yaml`,
  `deploy/rbac-certs.yaml`, `deploy/rbac-controlplane.yaml`, `deploy/rbac-diskusage.yaml`,
  `deploy/rbac-dnshealth.yaml`, `deploy/rbac-gitops.yaml`, `deploy/rbac-logs.yaml`,
  `deploy/rbac-operators.yaml`, `deploy/helm/kubeagent/templates/clusterrole.yaml`

**Interfaces:**

- Consumes: `Feature`, `Rule`, `Profile`, `Features`, `ProfileByName`, `Resolve` from Task 1.
- Produces: `RenderRules(rules []Rule, indent int) string`,
  `RenderClusterRole(name string, rules []Rule) string`, `RenderBaseManifest() string`,
  `RenderAddonManifest(f Feature) string`, `RenderHelmClusterRole() string`.
  Task 3 calls `RenderClusterRole`.

**Note on YAML:** hand-roll the emission. `sigs.k8s.io/yaml` would rewrite every rule into
block style and rewrite all nine shipped files for no reason. The target is the compact flow
style already in the tree: `resources: [pods, nodes]`, `verbs: [get, list, watch]`,
`apiGroups: [""]` — and API group names quoted (`apiGroups: ["apps"]`). Today
`deploy/rbac-operators.yaml` and `deploy/rbac-gitops.yaml` leave CRD group names unquoted;
generation normalises them to quoted, which is the one intended content change in this task.

- [ ] **Step 1: Write the failing tests**

Create `internal/rbacprofile/manifest_test.go`:

```go
package rbacprofile

import (
	"strings"
	"testing"
)

func TestRenderRulesUsesFlowStyle(t *testing.T) {
	got := RenderRules([]Rule{
		{APIGroup: "", Resources: []string{"pods", "nodes"}, Verbs: []string{"get", "list"}},
		{APIGroup: "apps", Resources: []string{"deployments"}, Verbs: []string{"get", "list", "watch"}},
		{NonResourceURLs: []string{"/readyz"}, Verbs: []string{"get"}},
	}, 2)
	want := `  - apiGroups: [""]
    resources: [pods, nodes]
    verbs: [get, list]
  - apiGroups: ["apps"]
    resources: [deployments]
    verbs: [get, list, watch]
  - nonResourceURLs: ["/readyz"]
    verbs: [get]
`
	if got != want {
		t.Errorf("RenderRules produced:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderRulesHonoursIndent(t *testing.T) {
	got := RenderRules([]Rule{{APIGroup: "", Resources: []string{"pods"}, Verbs: []string{"get"}}}, 4)
	if !strings.HasPrefix(got, "    - apiGroups:") {
		t.Errorf("indent 4 produced %q", got)
	}
}

func TestRenderClusterRoleNamesTheRole(t *testing.T) {
	got := RenderClusterRole("kubeagent-scan", []Rule{{APIGroup: "", Resources: []string{"pods"}, Verbs: []string{"get"}}})
	want := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-scan
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get]
`
	if got != want {
		t.Errorf("RenderClusterRole produced:\n%s\nwant:\n%s", got, want)
	}
}

// Existing installs bind kubeagent-readonly with get/list/watch. Regenerating
// the base manifest must not quietly rename or downgrade it.
func TestBaseManifestKeepsItsContract(t *testing.T) {
	got := RenderBaseManifest()
	for _, want := range []string{
		"name: kubeagent-readonly",
		"kind: ServiceAccount",
		"kind: ClusterRoleBinding",
		"verbs: [get, list, watch]",
		"do not edit by hand",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("base manifest is missing %q", want)
		}
	}
	if strings.Contains(got, "verbs: [get, list]\n") {
		t.Error("base manifest downgraded a core rule off watch")
	}
}

func TestAddonManifestCarriesItsDocAndBinding(t *testing.T) {
	f, ok := Lookup("certs")
	if !ok {
		t.Fatal("no certs feature")
	}
	got := RenderAddonManifest(f)
	if !strings.Contains(got, "# Opt-in add-on: grants the kubeagent ServiceAccount LIST access to Secrets so") {
		t.Error("addon manifest dropped its Doc header")
	}
	if !strings.Contains(got, "name: kubeagent-certs") {
		t.Error("addon manifest lost its role name")
	}
	if strings.Count(got, "kind: ClusterRoleBinding") != 1 {
		t.Error("addon manifest should carry exactly one ClusterRoleBinding")
	}
}

// The chart gates four add-ons and deliberately gates none of the scan-only ones.
func TestHelmClusterRoleGatesExactlyTheChartFeatures(t *testing.T) {
	got := RenderHelmClusterRole()
	for _, want := range []string{
		"{{- if .Values.rbac.create -}}",
		"{{- if or .Values.diskUsage.enabled .Values.kubeletHealth.enabled }}",
		"{{- if .Values.dnsHealth.enabled }}",
		"{{- if .Values.controlPlaneHealth.enabled }}",
		"{{- if .Values.certs.enabled }}",
		`{{ include "kubeagent.fullname" . }}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("chart template is missing %q", want)
		}
	}
	for _, unwanted := range []string{"pods/log", "cert-manager.io", "argoproj.io"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("chart template grants %q, but the daemon never reads it", unwanted)
		}
	}
	// 5, not 4: the four per-feature gates plus the outer
	// {{- if .Values.rbac.create -}} wrap the chart has always carried.
	if n := strings.Count(got, "{{- end }}"); n != 5 {
		t.Errorf("chart template has %d conditional ends, want 5", n)
	}
}
```

Create `internal/rbacprofile/golden_test.go`:

```go
package rbacprofile

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the generated RBAC manifests")

// generated maps each committed file to the function that produces it. This is
// the whole drift guard: the same map both writes the files under -update and
// asserts them without it, so the generator and the assertion can never disagree.
func generated() map[string]string {
	out := map[string]string{
		filepath.Join("..", "..", "deploy", "rbac.yaml"):                                                  RenderBaseManifest(),
		filepath.Join("..", "..", "deploy", "helm", "kubeagent", "templates", "clusterrole.yaml"):         RenderHelmClusterRole(),
	}
	for _, f := range Features() {
		if f.Manifest == "" || f.Name == "core" {
			continue
		}
		out[filepath.Join("..", "..", "deploy", f.Manifest)] = RenderAddonManifest(f)
	}
	return out
}

func TestGeneratedManifests(t *testing.T) {
	for path, want := range generated() {
		if *update {
			if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
				t.Fatalf("writing %s: %v", path, err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("%s is out of date with internal/rbacprofile.\n%s\n\nRegenerate: go test ./internal/rbacprofile -run TestGeneratedManifests -update",
				path, firstDiff(string(got), string(want)))
		}
	}
}

// Deleting a feature row must not leave an orphaned manifest applying grants
// nothing in the table justifies.
func TestNoOrphanedManifests(t *testing.T) {
	onDisk, err := filepath.Glob(filepath.Join("..", "..", "deploy", "rbac-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	claimed := generated()
	for _, path := range onDisk {
		if _, ok := claimed[path]; !ok {
			t.Errorf("%s is not generated by any feature in the table", path)
		}
	}
}

// firstDiff reports the first differing line, so a failure names the drift
// instead of dumping two whole manifests.
func firstDiff(got, want string) string {
	g, w := splitLines(got), splitLines(want)
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return "first difference at line " + itoa(i+1) + ":\n  on disk:   " + gl + "\n  generated: " + wl
		}
	}
	return "files differ only in trailing bytes"
}
```

`splitLines` and `itoa` are trivial helpers — use `strings.Split(s, "\n")` and
`strconv.Itoa` directly rather than defining them, adjusting the imports accordingly.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test -p 2 ./internal/rbacprofile/
```

Expected: FAIL — `undefined: RenderRules`.

- [ ] **Step 3: Write the implementation**

Create `internal/rbacprofile/manifest.go`:

```go
package rbacprofile

import (
	"strings"
)

// generatedHeader is the first thing every generated file says, because the
// first instinct on seeing a wrong grant is to edit the YAML.
const generatedHeader = `# Generated from internal/rbacprofile — do not edit by hand.
# Regenerate: go test ./internal/rbacprofile -run TestGeneratedManifests -update
`

// RenderRules writes rules as the YAML sequence that goes under `rules:`,
// indented by indent spaces, in the compact flow style the shipped manifests
// use. Emitting it by hand rather than through a YAML library is deliberate:
// a marshaller would rewrite every rule into block style and churn nine files.
func RenderRules(rules []Rule, indent int) string {
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for _, r := range rules {
		if len(r.NonResourceURLs) > 0 {
			b.WriteString(pad + "- nonResourceURLs: [" + quotedList(r.NonResourceURLs) + "]\n")
		} else {
			b.WriteString(pad + `- apiGroups: ["` + r.APIGroup + "\"]\n")
			b.WriteString(pad + "  resources: [" + strings.Join(r.Resources, ", ") + "]\n")
		}
		b.WriteString(pad + "  verbs: [" + strings.Join(r.Verbs, ", ") + "]\n")
	}
	return b.String()
}

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = `"` + s + `"`
	}
	return strings.Join(quoted, ", ")
}

// RenderClusterRole renders a standalone ClusterRole document. `kubeagent rbac
// print` uses this directly.
func RenderClusterRole(name string, rules []Rule) string {
	return "apiVersion: rbac.authorization.k8s.io/v1\n" +
		"kind: ClusterRole\n" +
		"metadata:\n" +
		"  name: " + name + "\n" +
		"rules:\n" +
		RenderRules(rules, 2)
}

// renderBinding renders the ClusterRoleBinding that ties a role to the
// kubeagent ServiceAccount.
func renderBinding(name string) string {
	return "apiVersion: rbac.authorization.k8s.io/v1\n" +
		"kind: ClusterRoleBinding\n" +
		"metadata:\n" +
		"  name: " + name + "\n" +
		"roleRef:\n" +
		"  apiGroup: rbac.authorization.k8s.io\n" +
		"  kind: ClusterRole\n" +
		"  name: " + name + "\n" +
		"subjects:\n" +
		"  - kind: ServiceAccount\n" +
		"    name: kubeagent\n" +
		"    namespace: kubeagent\n"
}

// comment turns a Doc string into a YAML comment block.
func comment(doc string) string {
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if line == "" {
			b.WriteString("#\n")
			continue
		}
		b.WriteString("# " + line + "\n")
	}
	return b.String()
}

// RenderBaseManifest renders deploy/rbac.yaml: the ServiceAccount every install
// binds, plus the core ClusterRole at the daemon's watch verbs.
func RenderBaseManifest() string {
	core, _ := Lookup("core")
	p, _ := ProfileByName("watch")
	rules, err := Resolve(p)
	if err != nil {
		panic("rbacprofile: the watch profile does not resolve: " + err.Error())
	}
	return generatedHeader +
		comment(core.Doc) +
		"apiVersion: v1\n" +
		"kind: ServiceAccount\n" +
		"metadata:\n" +
		"  name: kubeagent\n" +
		"  namespace: kubeagent\n" +
		"---\n" +
		RenderClusterRole(core.RoleName, rules) +
		"---\n" +
		renderBinding(core.RoleName)
}

// RenderAddonManifest renders one deploy/rbac-<feature>.yaml: the opt-in
// ClusterRole for a single feature, plus its binding.
func RenderAddonManifest(f Feature) string {
	return generatedHeader +
		comment(f.Doc) +
		RenderClusterRole(f.RoleName, MergeRules(f.Rules)) +
		"---\n" +
		renderBinding(f.RoleName)
}

// RenderHelmClusterRole renders the chart's ClusterRole template: core's rules
// unconditionally, then one gated block per chart-gated feature. Scan-only
// features are absent by design — the daemon never reads what they grant.
func RenderHelmClusterRole() string {
	core, _ := Lookup("core")
	p, _ := ProfileByName("watch")
	rules, err := Resolve(p)
	if err != nil {
		panic("rbacprofile: the watch profile does not resolve: " + err.Error())
	}

	var b strings.Builder
	b.WriteString("{{- if .Values.rbac.create -}}\n")
	b.WriteString(generatedHeader)
	b.WriteString(comment(core.Doc))
	b.WriteString("apiVersion: rbac.authorization.k8s.io/v1\n")
	b.WriteString("kind: ClusterRole\n")
	b.WriteString("metadata:\n")
	b.WriteString(`  name: {{ include "kubeagent.fullname" . }}` + "\n")
	b.WriteString("  labels:\n")
	b.WriteString(`    {{- include "kubeagent.labels" . | nindent 4 }}` + "\n")
	b.WriteString("rules:\n")
	b.WriteString(RenderRules(rules, 2))
	for _, f := range Features() {
		if f.HelmCondition == "" {
			continue
		}
		b.WriteString("  {{- " + f.HelmCondition + " }}\n")
		b.WriteString(RenderRules(MergeRules(f.Rules), 2))
		b.WriteString("  {{- end }}\n")
	}
	b.WriteString("---\n")
	b.WriteString("apiVersion: rbac.authorization.k8s.io/v1\n")
	b.WriteString("kind: ClusterRoleBinding\n")
	b.WriteString("metadata:\n")
	b.WriteString(`  name: {{ include "kubeagent.fullname" . }}` + "\n")
	b.WriteString("  labels:\n")
	b.WriteString(`    {{- include "kubeagent.labels" . | nindent 4 }}` + "\n")
	b.WriteString("roleRef:\n")
	b.WriteString("  apiGroup: rbac.authorization.k8s.io\n")
	b.WriteString("  kind: ClusterRole\n")
	b.WriteString(`  name: {{ include "kubeagent.fullname" . }}` + "\n")
	b.WriteString("subjects:\n")
	b.WriteString("  - kind: ServiceAccount\n")
	b.WriteString(`    name: {{ include "kubeagent.serviceAccountName" . }}` + "\n")
	b.WriteString("    namespace: {{ .Release.Namespace }}\n")
	b.WriteString("{{- end }}\n")
	return b.String()
}
```

- [ ] **Step 4: Regenerate and inspect the diff**

```bash
go test -p 2 ./internal/rbacprofile/ -run TestGeneratedManifests -update
git diff --stat deploy/
git diff deploy/
```

Read the whole diff. The **only** acceptable changes are:

1. the two-line generated header added to each file;
2. `deploy/rbac.yaml` gaining the `# Strictly read-only: get/list/watch only…` comment it
   does not carry today (it comes from `core.Doc`, which the chart template already prints);
3. CRD API group names in `rbac-operators.yaml` and `rbac-gitops.yaml` gaining quotes;
4. whitespace normalisation inside the Doc comment blocks.

`kubeagent-readonly`, the seven add-on role names, every resource, every verb and the four
Helm conditions must be unchanged. If anything else moved, the table is wrong — fix the
table, not the YAML.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go test -p 2 ./internal/rbacprofile/ -v
go test -p 2 ./...
```

Expected: PASS everywhere. `internal/report/testdata/golden-scan.txt` must be untouched —
confirm with `git status --short internal/report/`.

- [ ] **Step 6: Verify the chart still renders**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm lint deploy/helm/kubeagent
helm template x deploy/helm/kubeagent | grep -c 'kind: ClusterRole'
helm template x deploy/helm/kubeagent --set certs.enabled=true --set diskUsage.enabled=true | grep -E 'secrets|nodes/proxy'
```

Expected: lint passes; the default render has 3 — the `ClusterRole`, the
`ClusterRoleBinding`, and the `kind: ClusterRole` line inside that binding's `roleRef`, which
the substring grep also matches. It is 3 before this task too, so the number to check is that
it did not change. The second render shows both `resources: [secrets]` and
`resources: [nodes/proxy]`.

- [ ] **Step 7: Commit**

```bash
git add internal/rbacprofile/ deploy/
git commit -s -m "feat(rbac): generate every RBAC manifest from the table

deploy/rbac.yaml, the seven add-on manifests and the chart ClusterRole are now
rendered from internal/rbacprofile and asserted by a golden test, so a grant and
the code that needs it cannot drift apart. Role names and verbs are unchanged;
regenerate with go test ./internal/rbacprofile -run TestGeneratedManifests -update."
```

---

### Task 3: `kubeagent rbac print` and `kubeagent rbac check`

**Files:**

- Create: `internal/rbacprofile/check.go`
- Create: `internal/rbacprofile/check_test.go`
- Modify: `main.go` (dispatch chain around line 126-145, and the usage string at line 144)
- Modify: `main_test.go`

**Interfaces:**

- Consumes: `Feature`, `Rule`, `Profile`, `Features`, `Lookup`, `ProfileByName`, `Resolve`
  from Task 1; `RenderClusterRole` from Task 2; `cluster.NewClient(kubeconfig, contextName)`
  and `redact.Error(err) string` from the existing tree.
- Produces: `Action`, `Action.String() string`, `Rule.Actions() []Action`, `FeatureStatus`,
  `Check(ctx, client, features) ([]FeatureStatus, error)`; and in `main.go`,
  `selectedFeatures(profile, features string) ([]rbacprofile.Feature, error)` and
  `selectedRules(profile, features string) ([]rbacprofile.Rule, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/rbacprofile/check_test.go`:

```go
package rbacprofile

import (
	"context"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// allowAllExcept builds a fake clientset whose SelfSubjectAccessReview answers
// yes to everything except the named resources.
func allowAllExcept(denied ...string) *fake.Clientset {
	blocked := map[string]bool{}
	for _, d := range denied {
		blocked[d] = true
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		name := ""
		if ra := review.Spec.ResourceAttributes; ra != nil {
			name = ra.Resource
			if ra.Subresource != "" {
				name += "/" + ra.Subresource
			}
		} else if nra := review.Spec.NonResourceAttributes; nra != nil {
			name = nra.Path
		}
		review.Status = authv1.SubjectAccessReviewStatus{
			Allowed: !blocked[name],
			// A real API server fills Reason with the authorizer's own message,
			// which names the requesting identity. Setting it here proves Check
			// never reads it.
			Reason: "RBAC: user \"<PLACEHOLDER-IDENTITY>\" cannot list secrets",
		}
		return true, review, nil
	})
	return client
}

func TestCheckReportsAllowedFeature(t *testing.T) {
	f, _ := Lookup("certs")
	got, err := Check(context.Background(), allowAllExcept(), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Allowed || len(got[0].Missing) != 0 {
		t.Fatalf("certs reported as %+v, want allowed with nothing missing", got[0])
	}
}

func TestCheckNamesTheMissingActionInKubeagentsWords(t *testing.T) {
	f, _ := Lookup("certs")
	got, err := Check(context.Background(), allowAllExcept("secrets"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Allowed {
		t.Fatal("certs reported as allowed while secrets are denied")
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "list secrets" {
		t.Fatalf("Missing = %v, want [\"list secrets\"]", got[0].Missing)
	}
}

// The API server's reason embeds the requesting identity. It must never reach a
// FeatureStatus, whatever the authorizer chose to say.
func TestCheckNeverQuotesTheAPIServersReason(t *testing.T) {
	f, _ := Lookup("certs")
	got, _ := Check(context.Background(), allowAllExcept("secrets"), []Feature{f})
	for _, m := range got[0].Missing {
		if strings.Contains(m, "PLACEHOLDER-IDENTITY") || strings.Contains(m, "RBAC:") {
			t.Errorf("Missing entry %q carries the API server's own message", m)
		}
	}
}

func TestCheckSplitsSubresources(t *testing.T) {
	f, _ := Lookup("logs")
	got, err := Check(context.Background(), allowAllExcept("pods/log"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "get pods/log" {
		t.Fatalf("Missing = %v, want [\"get pods/log\"]", got[0].Missing)
	}
}

func TestCheckHandlesNonResourceURLs(t *testing.T) {
	f, _ := Lookup("controlplane")
	got, err := Check(context.Background(), allowAllExcept("/readyz"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "get /readyz" {
		t.Fatalf("Missing = %v, want [\"get /readyz\"]", got[0].Missing)
	}
}

// A feature that costs nothing beyond core must report clean without issuing a
// single access review.
func TestCheckSkipsFeaturesWithNoRules(t *testing.T) {
	f, _ := Lookup("capacity")
	client := allowAllExcept()
	got, err := Check(context.Background(), client, []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Allowed {
		t.Fatal("capacity needs no grant but reported as blocked")
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "selfsubjectaccessreviews" {
			t.Fatal("Check issued an access review for a feature with no rules")
		}
	}
}

func TestActionStringQualifiesCustomResources(t *testing.T) {
	a := Action{Verb: "list", APIGroup: "cert-manager.io", Resource: "certificates"}
	if got := a.String(); got != "list certificates.cert-manager.io" {
		t.Errorf("Action.String() = %q", got)
	}
}
```

Append to `main_test.go`:

```go
func TestSelectedRulesResolvesAProfile(t *testing.T) {
	rules, err := selectedRules("scan", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 10 {
		t.Fatalf("scan profile resolved to %d rules, want 10", len(rules))
	}
}

func TestSelectedRulesPrefersExplicitFeatures(t *testing.T) {
	rules, err := selectedRules("scan", "core, certs")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rules {
		for _, res := range r.Resources {
			if res == "secrets" {
				found = true
			}
		}
	}
	if !found {
		t.Error("--features core,certs did not include the secrets grant")
	}
}

func TestSelectedRulesRejectsAnUnknownProfile(t *testing.T) {
	if _, err := selectedRules("everything", ""); err == nil {
		t.Fatal("selectedRules accepted an unknown profile")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test -p 2 ./internal/rbacprofile/ . 2>&1 | head -20
```

Expected: FAIL — `undefined: Check`, `undefined: selectedRules`.

- [ ] **Step 3: Write `internal/rbacprofile/check.go`**

```go
package rbacprofile

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/redact"
)

// Action is one thing kubeagent needs to be allowed to do: a single verb on a
// single resource, subresource or non-resource URL. A Rule expands into one
// Action per resource per verb, because that is the granularity the API server
// answers at.
type Action struct {
	Verb           string
	APIGroup       string
	Resource       string // "pods"
	Subresource    string // "log"; empty for a whole resource
	NonResourceURL string // "/readyz"; set instead of the three fields above
}

// String is how kubeagent names a missing permission: built from the table, in
// kubeagent's own words. The API server's explanation is never used, because it
// embeds the requesting identity.
func (a Action) String() string {
	if a.NonResourceURL != "" {
		return a.Verb + " " + a.NonResourceURL
	}
	name := a.Resource
	if a.Subresource != "" {
		name += "/" + a.Subresource
	}
	if a.APIGroup != "" {
		name += "." + a.APIGroup
	}
	return a.Verb + " " + name
}

// Actions expands a rule into the individual access reviews it implies.
func (r Rule) Actions() []Action {
	var out []Action
	for _, verb := range r.Verbs {
		for _, url := range r.NonResourceURLs {
			out = append(out, Action{Verb: verb, NonResourceURL: url})
		}
		for _, res := range r.Resources {
			resource, subresource, _ := strings.Cut(res, "/")
			out = append(out, Action{Verb: verb, APIGroup: r.APIGroup, Resource: resource, Subresource: subresource})
		}
	}
	return out
}

// FeatureStatus is the result of checking one feature against a live identity.
type FeatureStatus struct {
	Name    string `json:"name"`
	Flag    string `json:"flag,omitempty"`
	Summary string `json:"summary"`
	Allowed bool   `json:"allowed"`
	// Missing lists the actions the identity may not perform, phrased by
	// kubeagent from its own table. Never the API server's words.
	Missing []string `json:"missing,omitempty"`
}

// Check asks the API server whether the current identity may perform every
// action the named features need.
//
// It creates SelfSubjectAccessReview objects. That is a POST, but a virtual
// one: the API server evaluates the request and persists nothing, which is the
// same API `kubectl auth can-i` uses. Nothing in the cluster changes, and no
// extra grant is needed to ask — system:basic-user, bound to
// system:authenticated, already allows it.
func Check(ctx context.Context, client kubernetes.Interface, features []Feature) ([]FeatureStatus, error) {
	out := make([]FeatureStatus, 0, len(features))
	for _, f := range features {
		status := FeatureStatus{Name: f.Name, Flag: f.Flag, Summary: f.Summary, Allowed: true}
		for _, r := range MergeRules(f.Rules) {
			for _, a := range r.Actions() {
				ok, err := allowed(ctx, client, a)
				if err != nil {
					return nil, fmt.Errorf("could not check %q: %s", a, redact.Error(err))
				}
				if !ok {
					status.Allowed = false
					status.Missing = append(status.Missing, a.String())
				}
			}
		}
		out = append(out, status)
	}
	return out, nil
}

func allowed(ctx context.Context, client kubernetes.Interface, a Action) (bool, error) {
	review := &authv1.SelfSubjectAccessReview{}
	if a.NonResourceURL != "" {
		review.Spec.NonResourceAttributes = &authv1.NonResourceAttributes{Path: a.NonResourceURL, Verb: a.Verb}
	} else {
		review.Spec.ResourceAttributes = &authv1.ResourceAttributes{
			Group:       a.APIGroup,
			Resource:    a.Resource,
			Subresource: a.Subresource,
			Verb:        a.Verb,
		}
	}
	res, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	// res.Status.Reason is deliberately not read. It carries the authorizer's
	// own message, which names the requesting identity — an IAM ARN, an OIDC
	// email, an internal DNS name — and under webhook authorization carries
	// third-party free text. kubeagent says why in its own words instead.
	return res.Status.Allowed, nil
}
```

- [ ] **Step 4: Wire the command into `main.go`**

Add to the dispatch chain in `run()`, immediately after the `tui` clause:

```go
	if len(args) > 0 && args[0] == "rbac" {
		return runRBAC(args[1:])
	}
```

Append `| %[1]s rbac print [--profile scan|watch|full] [--features a,b,…] [--role-name name] [--output yaml|json] | %[1]s rbac check [--kubeconfig path] [--context name] [--profile scan|watch|full] [--features a,b,…] [--output text|json]` to the big usage `fmt.Errorf` string, immediately before the trailing `| %[1]s version`.

Then add these functions near `runGate`:

```go
// runRBAC dispatches the two rbac verbs. Standard-library flag only — v1 has no
// Cobra, so each verb owns its own FlagSet, the same shape runGate uses.
func runRBAC(args []string) error {
	if len(args) > 0 && args[0] == "print" {
		return runRBACPrint(args[1:])
	}
	if len(args) > 0 && args[0] == "check" {
		return runRBACCheck(args[1:])
	}
	return fmt.Errorf("usage: %[1]s rbac print [--profile scan|watch|full] [--features a,b,…] [--role-name name] [--output yaml|json] | %[1]s rbac check [--kubeconfig path] [--context name] [--profile scan|watch|full] [--features a,b,…] [--output text|json]", invokedAs)
}

// splitFeatureList turns "core, certs" into ["core", "certs"], tolerating the
// spaces a human types.
func splitFeatureList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// selectedFeatures resolves the --profile / --features pair to feature rows.
// --features wins when both are given: naming features is the more specific
// request.
func selectedFeatures(profile, features string) ([]rbacprofile.Feature, error) {
	var names []string
	if strings.TrimSpace(features) != "" {
		names = splitFeatureList(features)
	} else {
		p, ok := rbacprofile.ProfileByName(profile)
		if !ok {
			return nil, fmt.Errorf("unknown --profile %q: want scan, watch or full", profile)
		}
		names = p.Features
	}
	out := make([]rbacprofile.Feature, 0, len(names))
	for _, name := range names {
		f, ok := rbacprofile.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown feature %q", name)
		}
		out = append(out, f)
	}
	return out, nil
}

// selectedRules resolves the same pair to the rules a ClusterRole should carry.
func selectedRules(profile, features string) ([]rbacprofile.Rule, error) {
	if strings.TrimSpace(features) != "" {
		return rbacprofile.Resolve(rbacprofile.Profile{Name: "custom", Features: splitFeatureList(features)})
	}
	p, ok := rbacprofile.ProfileByName(profile)
	if !ok {
		return nil, fmt.Errorf("unknown --profile %q: want scan, watch or full", profile)
	}
	return rbacprofile.Resolve(p)
}

func runRBACPrint(args []string) error {
	fs := flag.NewFlagSet("rbac print", flag.ContinueOnError)
	profile := fs.String("profile", "scan", "permission profile: scan | watch | full")
	features := fs.String("features", "", "comma-separated feature names, instead of a profile")
	roleName := fs.String("role-name", "kubeagent", "metadata.name of the printed ClusterRole")
	output := fs.String("output", "yaml", "output format: yaml | json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rules, err := selectedRules(*profile, *features)
	if err != nil {
		return err
	}
	switch *output {
	case "yaml":
		fmt.Fprint(os.Stdout, rbacprofile.RenderClusterRole(*roleName, rules))
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rules)
	default:
		return fmt.Errorf("unknown --output %q: want yaml or json", *output)
	}
	return nil
}

func runRBACCheck(args []string) error {
	fs := flag.NewFlagSet("rbac check", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	profile := fs.String("profile", "full", "permission profile: scan | watch | full")
	features := fs.String("features", "", "comma-separated feature names, instead of a profile")
	output := fs.String("output", "text", "output format: text | json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	selected, err := selectedFeatures(*profile, *features)
	if err != nil {
		return err
	}
	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}
	statuses, err := rbacprofile.Check(context.Background(), client, selected)
	if err != nil {
		return err
	}
	blocked := 0
	for _, s := range statuses {
		if !s.Allowed {
			blocked++
		}
	}
	if *output == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(statuses); err != nil {
			return err
		}
	} else {
		for _, s := range statuses {
			label := s.Name
			if s.Flag != "" {
				label += " (" + s.Flag + ")"
			}
			if s.Allowed {
				fmt.Fprintf(os.Stdout, "  ok       %s\n", label)
				continue
			}
			fmt.Fprintf(os.Stdout, "  blocked  %s — needs %s\n", label, strings.Join(s.Missing, ", "))
		}
		if blocked == 0 {
			fmt.Fprintf(os.Stdout, "\nAll %d checked features are permitted.\n", len(statuses))
		} else {
			fmt.Fprintf(os.Stdout, "\n%d of %d features are blocked. Print the role they need:\n  %s rbac print --profile %s\n", blocked, len(statuses), invokedAs, *profile)
		}
	}
	if blocked > 0 {
		// Exit 1 so a CI step can gate on it, the same contract `kubeagent gate`
		// offers. Nothing failed to run — the answer is simply "no".
		return &exitError{code: 1, msg: fmt.Sprintf("%d of %d features are blocked", blocked, len(statuses))}
	}
	return nil
}
```

Add `"github.com/imantaba/kubeagent/internal/rbacprofile"` to `main.go`'s imports.
`context`, `encoding/json`, `flag`, `fmt`, `os` and `strings` are already imported — confirm
before adding.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
go build ./... && go test -p 2 ./internal/rbacprofile/ . -v
```

Expected: PASS.

- [ ] **Step 6: Exercise it by hand**

```bash
go build -o kubeagent .
./kubeagent rbac print --profile scan
./kubeagent rbac print --features core,certs,logs --role-name kubeagent-scan
./kubeagent rbac print --profile full --output json | head -20
./kubeagent rbac                       # usage, exit 1
./kubeagent rbac print --profile nope  # error naming the three valid profiles
```

Sanity-check that `rbac print --profile watch` output is byte-identical to the ClusterRole
section of `deploy/rbac.yaml` apart from `metadata.name`.

- [ ] **Step 7: Commit**

```bash
git add internal/rbacprofile/check.go internal/rbacprofile/check_test.go main.go main_test.go
git commit -s -m "feat(rbac): add kubeagent rbac print and kubeagent rbac check

print emits the minimal ClusterRole for a profile or an explicit feature list.
check asks the API server, through SelfSubjectAccessReview, whether the current
identity may do what each feature needs, and names any gap in kubeagent's own
words — the authorizer's message embeds the requesting identity and is never
read. It exits 1 when a feature is blocked, so CI can gate on it."
```

---

### Task 4: Uniform blind spots

**Files:**

- Modify: `internal/collect/collect.go` (`NodeStats` at line 400, `PreviousLogs` at line 451)
- Modify: `internal/scan/scan.go` (`Evaluate`, lines 150-372)
- Modify: `internal/collect/collect_test.go`, `internal/scan/scan_test.go`
- Modify: `main.go` (`advisoryBlindSpots`), `main_test.go`

**Interfaces:**

- Consumes: `scan.ReadFailure{Resource, Reason string}` (`internal/scan/scan.go:71`),
  `advisory.Degradation{Sections []string; Subject, Reason string}`,
  `certhealth.Report.Forbidden bool`, `nodehealth.Report.Forbidden int`,
  `controlplane.Probe.Status string` (`"ok" | "unhealthy" | "forbidden" | "unreachable"`).
- Produces: `collect.NodeStats(...) (diskusage.NodeSummary, bool, error)` — signature
  unchanged, but the error is now real instead of always nil;
  `collect.PreviousLogs(...) (string, bool, error)`;
  `advisoryBlindSpots([]advisory.Degradation) []scan.ReadFailure` in `main.go`.

**The problem this closes:** eighteen core list calls already record a `ReadFailure` when
they are refused, and `scan --output text/json/html` renders those as named blind spots. The
feature-flagged collectors do not: `--disk-usage` and `--logs` discard the error entirely,
so a missing `nodes/proxy` or `pods/log` grant is not merely silent — it is unrepresentable.
`--certs`, `--kubelet-health`, `--dns-health` and `--control-plane-health` do detect
forbidden, but only inside their own report, so the top-level blind-spot list stays empty. A
scan that could not see is presented exactly like a scan that saw nothing wrong. That is the
green-when-blind defect `kubeagent gate` exists to prevent.

**The `forbidden` substring rule:** `internal/htmlreport.safeReason` classifies a reason by
substring — `"forbidden"` or `"Unauthorized"` means permission denied, anything else falls
through to a generic phrase. Every reason string added below therefore **starts with
`forbidden:`**. This is load-bearing, not stylistic.

- [ ] **Step 1: Write the failing tests**

Add to `internal/collect/collect_test.go`:

```go
// A forbidden nodes/proxy read must be distinguishable from a node that simply
// has no stats. Before this, both came back as (zero, false, nil).
func TestNodeStatsReturnsForbidden(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node-1", errors.New("no access"))
	})
	_, ok, err := NodeStats(context.Background(), client, "node-1")
	if ok {
		t.Fatal("NodeStats reported success on a forbidden read")
	}
	if err == nil {
		t.Fatal("NodeStats swallowed a forbidden read; the caller cannot report a blind spot")
	}
}

func TestPreviousLogsReturnsForbidden(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "pod-1", errors.New("no access"))
	})
	_, ok, err := PreviousLogs(context.Background(), client, "ns", "pod-1", "app")
	if ok {
		t.Fatal("PreviousLogs reported success on a forbidden read")
	}
	if err == nil {
		t.Fatal("PreviousLogs swallowed a forbidden read")
	}
}
```

The fake clientset's reactor for the log subresource may not intercept `DoRaw` on the REST
client; if a reactor cannot be made to fire for either call, assert instead that the
signature returns a non-nil error when the underlying request fails, using
`fake.NewSimpleClientset()` with the REST client configured to fail — and if that is not
reachable either, drop the collect-level test for that one function and cover the behaviour
through the `scan` tests below, which is where it is observable. Do not delete a test to
make it pass; state which route you took in the report.

Add to `internal/scan/scan_test.go`:

```go
// Reason strings must contain "forbidden" or internal/htmlreport.safeReason
// degrades them to a generic phrase.
func TestBlindSpotReasonsAreClassifiable(t *testing.T) {
	for _, r := range []string{
		blindReason("get nodes/proxy"),
		blindReason("get pods/log"),
		blindReason("list secrets"),
		blindReason("get pods/proxy"),
		blindReason("get /readyz"),
	} {
		if !strings.Contains(r, "forbidden") {
			t.Errorf("reason %q lacks the substring \"forbidden\"; the HTML report will not classify it", r)
		}
	}
}

// A forbidden --certs read must surface as a named blind spot, not only as a
// flag inside the certificate report.
func TestForbiddenCertsRecordsABlindSpot(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("no access"))
	})
	res, err := Evaluate(context.Background(), client, Options{Certs: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range res.PartialReads {
		if p.Resource == "secrets" && strings.Contains(p.Reason, "forbidden") {
			found = true
		}
	}
	if !found {
		t.Errorf("PartialReads = %+v, want a forbidden entry for secrets", res.PartialReads)
	}
}

// One blind spot per feature, not one per node: a 200-node cluster must not
// print 200 identical lines.
func TestForbiddenDiskUsageRecordsOneBlindSpot(t *testing.T) {
	client := fake.NewSimpleClientset(fakeNode("node-1"), fakeNode("node-2"), fakeNode("node-3"))
	res, err := Evaluate(context.Background(), client, Options{DiskUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range res.PartialReads {
		if p.Resource == "nodes/proxy" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("recorded %d nodes/proxy blind spots for 3 nodes, want at most 1", n)
	}
}
```

`fakeNode` is an existing helper in the `scan`/`diagnose` test helpers — reuse it; if the
package's helper has a different name, use whatever `scan_test.go` already uses to build a
node.

Add to `main_test.go`:

```go
func TestAdvisoryBlindSpotsNamesEachDegradedSubject(t *testing.T) {
	got := advisoryBlindSpots([]advisory.Degradation{
		{Sections: []string{"drift"}, Subject: "argoproj.io/applications", Reason: "forbidden"},
		{Sections: []string{"operators"}, Subject: "longhorn.io/volumes", Reason: "the server could not find the requested resource"},
	})
	if len(got) != 1 {
		t.Fatalf("got %d blind spots, want only the forbidden one: %+v", len(got), got)
	}
	if got[0].Resource != "argoproj.io/applications" {
		t.Errorf("Resource = %q", got[0].Resource)
	}
	if !strings.Contains(got[0].Reason, "forbidden") {
		t.Errorf("Reason = %q, want it to contain \"forbidden\"", got[0].Reason)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test -p 2 ./internal/collect/ ./internal/scan/ . 2>&1 | head -30
```

Expected: FAIL — `undefined: blindReason`, `undefined: advisoryBlindSpots`, and assignment
mismatches on `PreviousLogs`.

- [ ] **Step 3: Widen the two collectors**

In `internal/collect/collect.go`, replace `NodeStats`'s error-swallowing return:

```go
// NodeStats reads one node's kubelet summary API through the nodes/proxy
// subresource. It returns (zero, false, err) when the read is refused or the
// node is unreachable, so a scan can still succeed without it while naming what
// it could not see — a discarded error here would make a missing nodes/proxy
// grant not merely silent but unrepresentable.
func NodeStats(ctx context.Context, client kubernetes.Interface, node string) (diskusage.NodeSummary, bool, error) {
	data, err := client.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", node)).DoRaw(ctx)
	if err != nil {
		return diskusage.NodeSummary{}, false, err
	}
	return parseNodeSummary(node, data)
}
```

and widen `PreviousLogs`:

```go
// PreviousLogs reads the tail of a container's previous run. It returns
// (\"\", false, err) when the read is refused, so --logs without a pods/log
// grant reports a blind spot instead of quietly finding no log cause. An empty
// log is (\"\", false, nil): nothing was refused, there was simply nothing there.
func PreviousLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string) (string, bool, error) {
	tail := int64(25)
	raw, err := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: true, TailLines: &tail,
	}).DoRaw(ctx)
	if err != nil {
		return "", false, err
	}
	if len(raw) == 0 {
		return "", false, nil
	}
	return string(raw), true, nil
}
```

- [ ] **Step 4: Record blind spots in `internal/scan/scan.go`**

Add next to the existing `note` closure at line 152:

```go
	// blind records a blind spot in kubeagent's own words. The reason always
	// starts with "forbidden" so internal/htmlreport.safeReason classifies it as
	// a permission problem rather than degrading it to a generic phrase — and so
	// it never carries the API server's message, which names the requesting
	// identity.
	blindSeen := map[string]bool{}
	blind := func(resource, action string) {
		if blindSeen[resource] {
			return // one line per feature, not one per node
		}
		blindSeen[resource] = true
		partialReads = append(partialReads, ReadFailure{Resource: resource, Reason: blindReason(action)})
	}
```

and, at package scope:

```go
// blindReason phrases a refused read. The leading "forbidden" is load-bearing:
// internal/htmlreport.safeReason classifies by substring, and a reason without
// it is rendered as the generic "the read failed" line.
func blindReason(action string) string {
	return "forbidden: kubeagent's credentials may not " + action
}
```

Then wire each feature collector:

**`--certs`** (line 233-238) — after `rep.Forbidden = true`:

```go
		if apierrors.IsForbidden(tlsErr) {
			rep.Forbidden = true
			blind("secrets", "list secrets")
		} else {
			note("secrets", tlsErr)
		}
```

**`--logs`** (line 195) — take the third return value:

```go
			log, ok, logErr := collect.PreviousLogs(ctx, client, ns, name, findings[i].Container)
			if logErr != nil {
				blind("pods/log", "get pods/log")
			}
			if ok {
```

**`--disk-usage`** (line 326):

```go
		for _, n := range nodes {
			s, ok, err := collect.NodeStats(ctx, client, n.Name)
			if err != nil {
				if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
					blind("nodes/proxy", "get nodes/proxy")
				}
				continue // an unreachable kubelet is a node problem, not a grant problem
			}
			if ok {
				summaries = append(summaries, s)
			}
		}
```

**`--kubelet-health`** (after `kubeletHealth = nodehealth.Assess(probes)`):

```go
		if kubeletHealth.Forbidden > 0 {
			blind("nodes/proxy", "get nodes/proxy")
		}
```

**`--control-plane-health`** (after `controlPlane = collect.ControlPlaneReadyz(ctx, client)`):

```go
		if controlPlane.Status == "forbidden" {
			blind("/readyz", "get /readyz")
		}
```

**`--dns-health`** (after the CoreDNS loop, before `dnsReport = ...`):

```go
		if forbidden > 0 {
			blind("pods/proxy", "get pods/proxy")
		}
```

Confirm `apierrors` is already imported in `scan.go` (it is, for `apierrors.IsForbidden` at
line 235).

- [ ] **Step 5: Add `advisoryBlindSpots` to `main.go`**

`advisory.Assess` returns degradations for `--operators` and `--drift` (custom resources the
identity may not list). They are printed to stderr today and vanish from the report. Add,
near the existing `advisory.Assess` call around line 271:

```go
// advisoryBlindSpots turns the advisory degradations that are permission
// problems into blind spots the report will name. Reasons are already redacted
// by internal/advisory; the "forbidden:" prefix is what makes the HTML report
// classify them, and it is kubeagent's word, not the API server's. A degradation
// with any other cause — a CRD that is simply not installed — is not a blind
// spot and is left to the existing stderr warning.
func advisoryBlindSpots(degradations []advisory.Degradation) []scan.ReadFailure {
	var out []scan.ReadFailure
	for _, d := range degradations {
		if !strings.Contains(strings.ToLower(d.Reason), "forbidden") {
			continue
		}
		out = append(out, scan.ReadFailure{
			Resource: d.Subject,
			Reason:   "forbidden: kubeagent's credentials may not list " + d.Subject,
		})
	}
	return out
}
```

and append its result to the scan result's `PartialReads` before the report is rendered,
alongside the existing `warnf` loop.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
go build ./... && go test -p 2 ./...
```

Expected: PASS everywhere.

**Then confirm the report golden file is untouched:**

```bash
git status --short internal/report/
```

Expected: no output. If `testdata/golden-scan.txt` changed, a blind spot leaked into the
default scan path — that is a defect in this task, not a golden file to regenerate.

- [ ] **Step 7: Commit**

```bash
git add internal/collect/ internal/scan/ main.go main_test.go
git commit -s -m "fix(scan): name a blind spot whenever a feature read is refused

--disk-usage and --logs discarded the read error entirely, so a missing
nodes/proxy or pods/log grant was not merely silent but unrepresentable; --certs,
--kubelet-health, --dns-health, --control-plane-health and the advisory
degradations recorded forbidden only inside their own section. All of them now
record a top-level blind spot, once per feature rather than once per node, phrased
in kubeagent's own words so the HTML report classifies it and no API server
message — which names the requesting identity — reaches the output."
```

---

### Task 5: Chaos scenario — a real least-privilege identity

**Files:**

- Modify: `chaos/run.sh` (add `scenario_20_rbac`, register it in `run_scenarios()`)

**Interfaces:**

- Consumes: `./kubeagent rbac print` and `./kubeagent rbac check` from Task 3; the blind-spot
  output from Task 4; the harness's `log`, `record` and `$CTX` conventions.
- Produces: nothing other tasks read.

**What it proves:** every other test in this branch runs against a fake clientset or a
kubeconfig with full access. This is the only test where the API server actually refuses
kubeagent, which is the entire point of the feature. It asserts the scan **succeeds** under
a scan-profile-only identity and **names** the three grants it lacks, rather than printing
three empty sections.

- [ ] **Step 1: Write the scenario**

Add to `chaos/run.sh`, after `scenario_19_mcp`:

```bash
scenario_20_rbac() {   # a real least-privilege identity: the API server actually says no
  log "scenario 20: least-privilege RBAC (kubeagent rbac + a scan-profile-only identity)"
  local ns=chaos-rbac
  kubectl --context "$CTX" create namespace "$ns" --dry-run=client -o yaml |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" create serviceaccount scanner \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # The role under test is generated by the binary itself, so this scenario also
  # proves `rbac print` emits something the API server accepts.
  ./kubeagent rbac print --profile scan --role-name chaos-rbac-scan |
    kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" create clusterrolebinding chaos-rbac-scan \
    --clusterrole=chaos-rbac-scan --serviceaccount="$ns:scanner" \
    --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # Build a kubeconfig for that ServiceAccount. It holds a bearer token, so it is
  # a credential: it lives in a temp file, is never printed, and is removed below.
  local kc ca token server
  kc="$(mktemp)"
  ca="$(mktemp)"
  token="$(kubectl --context "$CTX" -n "$ns" create token scanner --duration=1h)"
  server="$(kubectl --context "$CTX" config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  kubectl --context "$CTX" config view --minify --raw \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$ca"
  KUBECONFIG="$kc" kubectl config set-cluster chaos \
    --server="$server" --certificate-authority="$ca" --embed-certs=true >/dev/null
  KUBECONFIG="$kc" kubectl config set-credentials chaos --token="$token" >/dev/null
  KUBECONFIG="$kc" kubectl config set-context chaos --cluster=chaos --user=chaos >/dev/null
  KUBECONFIG="$kc" kubectl config use-context chaos >/dev/null

  # rbac check under that identity: core allowed, the three add-ons blocked.
  # It exits 1 by design when anything is blocked, so guard it.
  local check
  check="$(./kubeagent rbac check --kubeconfig "$kc" --features core,certs,logs,diskusage \
    --output json 2>/dev/null || true)"
  local core_ok blocked
  core_ok="$(printf '%s' "$check" | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
for r in rows:
    if r["name"] == "core":
        print(r["allowed"])
' 2>/dev/null || true)"
  blocked="$(printf '%s' "$check" | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except ValueError:
    sys.exit(0)
print(" ".join(sorted(r["name"] for r in rows if not r["allowed"])))
' 2>/dev/null || true)"

  # The scan itself: three add-on flags the identity cannot serve. It must still
  # succeed and must name what it could not read.
  local out rc
  out="$(mktemp)"
  ./kubeagent scan --kubeconfig "$kc" --certs --logs --disk-usage >"$out" 2>&1 && rc=0 || rc=$?

  local named
  named="$(grep -ciE 'secrets|pods/log|nodes/proxy' "$out" || true)"

  {
    echo '--- rbac check --output json (feature -> allowed) ---'
    printf '%s\n' "$check"
    printf '\n--- scan under the scan-profile-only identity (exit %s) ---\n' "$rc"
    cat "$out"
    printf '\n--- gate checks ---\n'
    printf 'rbac check: core allowed:            %s\n' "${core_ok:-<no response>}"
    printf 'rbac check: blocked features:        %s\n' "${blocked:-<none>}"
    printf 'scan exit code:                      %s\n' "$rc"
    printf 'blind spots naming a missing grant:  %s\n' "$named"
  } | record "20. Least-privilege RBAC (scan-profile-only identity)" \
    "expect: rbac check core allowed reads True — the generated scan-profile role really does cover core; rbac check blocked features reads exactly 'certs diskusage logs' — the three add-ons the identity was never granted, named by kubeagent from its own table and never quoting the API server; scan exit code is 0 — a missing add-on grant degrades the scan, it does not fail it; blind spots naming a missing grant is at least 1 — the report NAMES secrets / pods/log / nodes/proxy as unread rather than printing three empty sections. That last line reading 0 is the failure this scenario exists to catch: a scan that could not see must never look like a scan that saw nothing wrong"

  rm -f "$kc" "$ca" "$out"
  unset token
  kubectl --context "$CTX" delete clusterrolebinding chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete clusterrole chaos-rbac-scan >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete namespace "$ns" --wait=false >/dev/null 2>&1 || true
}
```

- [ ] **Step 2: Register it**

In `run_scenarios()`, add `20_rbac` to the array immediately before `01_etcd` (which must
stay last):

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 01_etcd)
```

- [ ] **Step 3: Shell-check the script**

```bash
bash -n chaos/run.sh
```

Expected: no output. The full harness runs in the gate after Task 6, not here — it needs a
Kind cluster and takes far longer than a task cycle.

- [ ] **Step 4: Commit**

```bash
git add chaos/run.sh
git commit -s -m "test(chaos): add scenario 20, a real least-privilege identity

Binds a ServiceAccount to the role \`kubeagent rbac print --profile scan\`
generates, then runs a scan with three add-on flags that identity cannot serve.
Asserts the scan still succeeds and names the three grants it lacks, rather than
printing three empty sections. The bearer token lives in a temp kubeconfig that
is never printed and is removed on the way out."
```

---

### Task 6: Documentation

**Files:**

- Create: `website/docs/features/rbac.md`
- Modify: `website/mkdocs.yml` (features nav, lines 57-72)
- Modify: `deploy/README.md`
- Modify: `website/docs/features/ci-gate.md` (a preflight note)
- Modify: `CHANGELOG.md` (`[Unreleased]`)
- Modify: `website/docs/roadmap.md` (Theme H bullet)
- Modify: `CLAUDE.md` (the `SelfSubjectAccessReview` carve-out)

**Interfaces:**

- Consumes: everything from Tasks 1-5.
- Produces: nothing other tasks read.

- [ ] **Step 1: Write `website/docs/features/rbac.md`**

Cover, in this order:

1. **What it answers** — "what does `--certs` cost me?" and "may this identity run it?".
2. **`kubeagent rbac print`** — the three profiles (`scan` = core at `get,list`; `watch` =
   core at `get,list,watch`, what the daemon needs; `full` = core plus every add-on), the
   `--features` escape hatch, `--role-name`, and `--output json`. Show
   `kubeagent rbac print --profile scan | kubectl apply -f -`.
3. **`kubeagent rbac check`** — what it does, that it exits 1 when a feature is blocked, and
   an example of both output formats.
4. **The feature table** — a markdown table of every feature: name, flag, what it grants
   (`apiGroup/resource: verbs`, or "nothing beyond core"). Generate the rows from
   `kubeagent rbac print --profile full --output json` so they cannot be wrong.
5. **Blind spots** — that a missing grant is named in the report, with an example line, and
   that the scan still exits 0. Cross-link `ci-gate.md`.
6. **A note on `SelfSubjectAccessReview`**: it is a POST that persists nothing, the same API
   `kubectl auth can-i` uses; it is the only non-`--fix` POST kubeagent makes; it needs no
   grant of its own because `system:basic-user` is bound to `system:authenticated`; and
   kubeagent never reads `Status.Reason`, because the authorizer's message names the
   requesting identity.
7. **A note that the manifests are generated** — `deploy/*.yaml` and the chart ClusterRole
   come from `internal/rbacprofile`; edit the table, run
   `go test ./internal/rbacprofile -run TestGeneratedManifests -update`.

No real cluster names, no real identities, no private IPs. Use RFC 2606 `.example` domains
and RFC 5737 IPs if an example needs either.

- [ ] **Step 2: Add the nav entry**

In `website/mkdocs.yml`, in the features list (lines 57-72), add:

```yaml
      - Least-privilege RBAC: features/rbac.md
```

- [ ] **Step 3: Update `deploy/README.md`**

Add a short section near the top of the RBAC discussion (around line 38) saying the
manifests in this directory are generated from `internal/rbacprofile` and must not be
hand-edited, with the regenerate command, and pointing at `kubeagent rbac print` for a role
narrower than the shipped ones. Then re-read the existing RBAC references (lines 4, 63, 85,
96, 105, 115, 122, 135, 142, 175, 194, 236) and fix anything the generated output made
inaccurate.

- [ ] **Step 4: Add the preflight note to `website/docs/features/ci-gate.md`**

Two or three sentences: `kubeagent gate` fails closed on an unreadable resource, so running
`kubeagent rbac check --profile scan` as an earlier CI step turns "the gate is red because a
grant is missing" into a message that names the grant. Note the exit-1-when-blocked contract.

- [ ] **Step 5: Update `CHANGELOG.md`**

Under `## [Unreleased]`, in `### Added`:

```markdown
- `kubeagent rbac print` and `kubeagent rbac check`: print the minimal ClusterRole a
  profile or feature list needs, and ask the API server whether the current identity may
  run it. `check` exits 1 when a feature is blocked, so CI can gate on it.
- Every RBAC manifest under `deploy/` and the chart's ClusterRole are now generated from a
  single feature table in `internal/rbacprofile`, with a golden test that fails on drift.
  Role names, resources and verbs are unchanged.
```

and in `### Fixed`:

```markdown
- A refused read behind a feature flag is now named as a blind spot in the scan report.
  `--disk-usage` and `--logs` discarded the error entirely, so a missing `nodes/proxy` or
  `pods/log` grant was invisible; `--certs`, `--kubelet-health`, `--dns-health`,
  `--control-plane-health` and the `--operators`/`--drift` advisories recorded it only
  inside their own section. A scan that could not see no longer looks like a scan that saw
  nothing wrong.
```

- [ ] **Step 6: Update `website/docs/roadmap.md`**

Mark the Theme H "per-feature least-privilege RBAC profiles" item as shipped, in the style
the file already uses for shipped items, linking `features/rbac.md`.

- [ ] **Step 7: Add the carve-out to `CLAUDE.md`**

In the **Invariants** section, after the `kubeagent tui` sentence, add verbatim:

> `kubeagent rbac` (`internal/rbacprofile`) is a fifth case: a one-shot, read-only command
> that makes **no LLM calls** and must never import `internal/remediate` or
> `internal/explain`. Its `check` verb creates `SelfSubjectAccessReview` objects — a virtual
> resource the API server evaluates and never persists, the same API `kubectl auth can-i`
> uses. It is the sole non-`--fix` path in kubeagent that issues a POST, and it changes no
> cluster state.

- [ ] **Step 8: Build the docs**

```bash
export PATH=$PATH:$HOME/.local/bin
cd website && mkdocs build --strict -f mkdocs.yml && cd ..
```

Expected: "Documentation built", exit 0, no `WARNING` lines naming the new page. The red
"Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 9: Full test run and secret sweep**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
git diff main --stat
scripts/dco-check.sh main
```

Expected: all tests pass, every commit on the branch carries a matching `Signed-off-by`.

- [ ] **Step 10: Commit**

```bash
git add website/ deploy/README.md CHANGELOG.md CLAUDE.md
git commit -s -m "docs: document least-privilege RBAC profiles

Adds website/docs/features/rbac.md covering rbac print, rbac check, the feature
table and how a missing grant now reads in the report; notes in deploy/README.md
that the manifests are generated; a preflight note in the CI gate page; and the
SelfSubjectAccessReview carve-out in the read-only invariant."
```

---

## Self-Review

**Spec coverage.** Every section of
[docs/superpowers/specs/2026-07-29-least-privilege-rbac-design.md](docs/superpowers/specs/2026-07-29-least-privilege-rbac-design.md)
maps to a task: the table → Task 1; profiles → Task 1 (`Profiles`, `Resolve`, `WithWatch`);
generation → Task 2; the two `kubeagent rbac` verbs → Task 3; "reasons are kubeagent's
words" → Task 3 (`Action.String`, never `Status.Reason`) and Task 4 (`blindReason`); uniform
blind spots → Task 4; the read-only invariant carve-out → Task 6; testing → the unit tests in
Tasks 1-4 plus the chaos scenario in Task 5; the gate → run after Task 6; documentation →
Task 6.

**One deliberate departure from the spec.** The spec's plan sketch called for adding
`logs.enabled`, `operators.enabled` and `gitops.enabled` toggles to the chart, on the reading
that their absence was drift. It is not: `deploy/rbac-logs.yaml`, `deploy/rbac-operators.yaml`
and `deploy/rbac-gitops.yaml` each state in their header that the feature is scan-only and
therefore deliberately not wired into the chart. Adding chart toggles would grant the daemon
permissions it never uses — the opposite of least privilege. Task 1 encodes the intent as a
`ScanOnly` field with `TestScanOnlyFeaturesAreNotChartGated` instead.

**Type consistency.** `Rule`, `Feature` and `Profile` are defined once in Task 1 and used
unchanged in Tasks 2 and 3. `RenderClusterRole(name, rules)` is defined in Task 2 and called
in Task 3. `FeatureStatus` is defined in Task 3 and consumed by the chaos scenario's JSON
parsing in Task 5 (`name`, `allowed`). `scan.ReadFailure{Resource, Reason}` is the existing
type, used unchanged by Task 4's `blind` and `advisoryBlindSpots`.

**Known soft spot, flagged rather than hidden.** Task 4's `collect` tests depend on the fake
clientset firing a reactor for `DoRaw` on the REST client and for the `pods/log`
subresource. If it does not, the step says what to do instead and requires the implementer to
report which route they took — it does not permit deleting the assertion.
