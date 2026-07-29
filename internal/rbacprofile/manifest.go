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
