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
	// 4 per-feature gates plus the outer `{{- if .Values.rbac.create -}}` wrap
	// that has closed the file for every release to date.
	if n := strings.Count(got, "{{- end }}"); n != 5 {
		t.Errorf("chart template has %d conditional ends, want 5", n)
	}
}
