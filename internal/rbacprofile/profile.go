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

// RulesDocument is the --output json shape of `rbac print`. The command used to
// emit a bare array of rules; an array root can carry no version field, so the
// rules moved under a key. RoleName is here because it is what the YAML form
// names the ClusterRole, and a consumer generating one needs both halves.
type RulesDocument struct {
	SchemaVersion string `json:"schemaVersion"`
	RoleName      string `json:"roleName"`
	Rules         []Rule `json:"rules"`
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
		Name:      "policy",
		Flag:      "--policy",
		Summary:   "organization-specific checks from a policy file; reads only kinds core already grants",
		CoveredBy: "core",
		ScanOnly:  true,
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
