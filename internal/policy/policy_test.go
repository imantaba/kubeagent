package policy

import (
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/rbacprofile"
)

// TestSecretIsNotSelectable pins the spec's first security exclusion. A
// violation carries evidence, and evidence drawn from a Secret would be secret
// material rendered into a report, a JSON document and a SARIF upload.
func TestSecretIsNotSelectable(t *testing.T) {
	if KindSelectable("Secret") {
		t.Fatal("Secret must never be a selectable kind — a violation would carry secret material as evidence")
	}
	for _, k := range SelectableKinds() {
		if k == "Secret" {
			t.Fatal("SelectableKinds lists Secret")
		}
	}
}

func TestSelectableKindsIsSortedAndHasNoDuplicates(t *testing.T) {
	kinds := SelectableKinds()
	if len(kinds) != 23 {
		t.Fatalf("want 23 selectable kinds, got %d", len(kinds))
	}
	if !sort.StringsAreSorted(kinds) {
		t.Errorf("SelectableKinds must be sorted for deterministic output: %v", kinds)
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("duplicate kind %q", k)
		}
		seen[k] = true
	}
}

// TestSelectableKindsMatchesRBACProfileCore asserts the claim made in
// selectableKinds' doc comment: the set is exactly what internal/rbacprofile's
// "core" feature already grants, minus Event and Lease (deliberately excluded
// as carrying no policy value). It reads rbacprofile.Lookup("core").Rules —
// the real, exported data structure — rather than a hardcoded copy of the
// granted resource list, so it catches drift in either package.
func TestSelectableKindsMatchesRBACProfileCore(t *testing.T) {
	core, ok := rbacprofile.Lookup("core")
	if !ok {
		t.Fatal(`rbacprofile has no "core" feature`)
	}

	// resourceKind maps every resource name a core RBAC rule grants to its
	// Kind. This is the standard Kubernetes resource<->Kind naming
	// convention (a RESTMapper's job in a live cluster), not a copy of
	// policy's own selectableKinds table.
	resourceKind := map[string]string{
		"pods":                            "Pod",
		"nodes":                           "Node",
		"services":                        "Service",
		"configmaps":                      "ConfigMap",
		"events":                          "Event",
		"persistentvolumeclaims":          "PersistentVolumeClaim",
		"persistentvolumes":               "PersistentVolume",
		"namespaces":                      "Namespace",
		"resourcequotas":                  "ResourceQuota",
		"deployments":                     "Deployment",
		"replicasets":                     "ReplicaSet",
		"statefulsets":                    "StatefulSet",
		"daemonsets":                      "DaemonSet",
		"jobs":                            "Job",
		"cronjobs":                        "CronJob",
		"endpointslices":                  "EndpointSlice",
		"networkpolicies":                 "NetworkPolicy",
		"ingressclasses":                  "IngressClass",
		"ingresses":                       "Ingress",
		"storageclasses":                  "StorageClass",
		"leases":                          "Lease",
		"poddisruptionbudgets":            "PodDisruptionBudget",
		"horizontalpodautoscalers":        "HorizontalPodAutoscaler",
		"validatingwebhookconfigurations": "ValidatingWebhookConfiguration",
		"mutatingwebhookconfigurations":   "MutatingWebhookConfiguration",
	}

	// Granted by core but deliberately absent from SelectableKinds: neither
	// carries policy value (see the selectableKinds comment in policy.go).
	excluded := map[string]bool{"Event": true, "Lease": true}

	granted := map[string]bool{}
	for _, r := range core.Rules {
		for _, res := range r.Resources {
			kind, ok := resourceKind[res]
			if !ok {
				t.Fatalf("core RBAC rule grants resource %q with no Kind mapping in this test — add one", res)
			}
			if excluded[kind] {
				continue
			}
			granted[kind] = true
		}
	}

	got := map[string]bool{}
	for _, k := range SelectableKinds() {
		got[k] = true
	}

	for kind := range granted {
		if !got[kind] {
			t.Errorf("rbacprofile core grants %q but it is not a selectable kind", kind)
		}
	}
	for kind := range got {
		if !granted[kind] {
			t.Errorf("%q is selectable but rbacprofile core does not grant it — a policy rule could select a kind kubeagent has no RBAC for", kind)
		}
	}
}

func TestKindNamespacedReportsAPIScope(t *testing.T) {
	cases := []struct {
		kind       string
		namespaced bool
	}{
		{"Pod", true},
		{"Deployment", true},
		{"PodDisruptionBudget", true},
		{"Node", false},
		{"Namespace", false},
		{"PersistentVolume", false},
		{"StorageClass", false},
		{"IngressClass", false},
		{"ValidatingWebhookConfiguration", false},
	}
	for _, c := range cases {
		got, known := KindNamespaced(c.kind)
		if !known {
			t.Errorf("%s: not a known kind", c.kind)
			continue
		}
		if got != c.namespaced {
			t.Errorf("%s: namespaced = %v, want %v", c.kind, got, c.namespaced)
		}
	}
	if _, known := KindNamespaced("Widget"); known {
		t.Error("Widget must not be a known kind")
	}
}

func TestRelationValidForKind(t *testing.T) {
	cases := []struct {
		rel  Relation
		kind string
		want bool
	}{
		{RelationHasPDB, "Deployment", true},
		{RelationHasPDB, "StatefulSet", true},
		{RelationHasPDB, "ReplicaSet", true},
		{RelationHasPDB, "DaemonSet", true},
		{RelationHasPDB, "Pod", false},
		{RelationHasHPA, "Deployment", true},
		{RelationHasHPA, "StatefulSet", true},
		{RelationHasHPA, "ReplicaSet", true},
		{RelationHasHPA, "DaemonSet", false}, // a DaemonSet runs one pod per node; it cannot scale horizontally
		{RelationHasHPA, "Pod", false},
	}
	for _, c := range cases {
		if got := RelationValidForKind(c.rel, c.kind); got != c.want {
			t.Errorf("RelationValidForKind(%q, %q) = %v, want %v", c.rel, c.kind, got, c.want)
		}
	}
}

func TestValidatorsRejectUnknownValues(t *testing.T) {
	if !ValidLevel(LevelCritical) || !ValidLevel(LevelWarning) || !ValidLevel(LevelInfo) {
		t.Error("the three levels must validate")
	}
	if ValidLevel(Level("fatal")) {
		t.Error("fatal is not a level")
	}
	if !ValidOp(OpExists) || !ValidOp(OpNotMatches) || !ValidOp(OpLte) {
		t.Error("declared operators must validate")
	}
	if ValidOp(Op("regex")) {
		t.Error("regex is not an operator")
	}
	if !ValidRelation(RelationHasPDB) || !ValidRelation(RelationHasHPA) {
		t.Error("declared relations must validate")
	}
	if ValidRelation(Relation("hasNetworkPolicy")) {
		t.Error("hasNetworkPolicy is not a relation")
	}
}

func TestReadPlanCoversSelectedAndSupportingKinds(t *testing.T) {
	rules := []Rule{
		{ID: "a", Match: Match{Kind: "Pod"}, Assert: Assert{Path: "metadata.name", Op: OpExists}},
		{ID: "b", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasPDB}},
		{ID: "c", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasHPA}},
		{ID: "d", Match: Match{Kind: "Pod", NamespaceLabels: map[string]string{"tier": "prod"}},
			Assert: Assert{Path: "metadata.name", Op: OpExists}},
	}
	got := ReadPlan(rules)
	want := []string{"Deployment", "HorizontalPodAutoscaler", "Namespace", "Pod", "PodDisruptionBudget"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadPlan = %v, want %v", got, want)
	}
}

// A rule set that needs no supporting list must not make kubeagent ask for
// one. Reading a PDB list nothing looks at is a permission kubeagent did not
// need and an API call it did not have to make.
func TestReadPlanAsksForNothingItDoesNotNeed(t *testing.T) {
	rules := []Rule{{ID: "a", Match: Match{Kind: "Pod"}, Assert: Assert{Path: "metadata.name", Op: OpExists}}}
	got := ReadPlan(rules)
	if strings.Join(got, ",") != "Pod" {
		t.Errorf("ReadPlan = %v, want just Pod", got)
	}
	if len(ReadPlan(nil)) != 0 {
		t.Error("no rules must plan no reads")
	}
}

// A kind that is both selected and supporting is read once.
func TestReadPlanDeduplicatesASelectedSupportingKind(t *testing.T) {
	rules := []Rule{
		{ID: "a", Match: Match{Kind: "PodDisruptionBudget"}, Assert: Assert{Path: "spec.minAvailable", Op: OpExists}},
		{ID: "b", Match: Match{Kind: "Deployment"}, Assert: Assert{Relation: RelationHasPDB}},
	}
	got := ReadPlan(rules)
	want := []string{"Deployment", "PodDisruptionBudget"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ReadPlan = %v, want %v", got, want)
	}
}

func TestInputsFromRoutesTheSupportingLists(t *testing.T) {
	ns := namespaceObj("prod", map[string]string{"tier": "prod"})
	p := pdb("prod", map[string]any{"matchLabels": map[string]any{"app": "web"}})
	h := hpa("prod", "Deployment", "web")
	objects := map[string][]*unstructured.Unstructured{
		"Namespace":               {ns},
		"PodDisruptionBudget":     {p},
		"HorizontalPodAutoscaler": {h},
		"Pod":                     {pod("prod", "a", nil, "docker.example.net/x:1.0")},
	}
	in := InputsFrom(objects, map[string]bool{"Node": true})

	if len(in.Namespaces) != 1 || len(in.PDBs) != 1 || len(in.HPAs) != 1 {
		t.Fatalf("supporting lists not routed: %d namespaces, %d pdbs, %d hpas",
			len(in.Namespaces), len(in.PDBs), len(in.HPAs))
	}
	if len(in.Objects["Pod"]) != 1 {
		t.Error("selected objects did not survive")
	}
	if !in.Unreadable["Node"] {
		t.Error("the unreadable set did not survive — a refused read would render as a pass")
	}
	// The supporting kinds stay in Objects too: a rule may select them.
	if len(in.Objects["PodDisruptionBudget"]) != 1 {
		t.Error("a supporting kind must remain selectable")
	}
}

func TestInputsFromToleratesNilMaps(t *testing.T) {
	in := InputsFrom(nil, nil)
	if len(in.Objects) != 0 || len(in.Unreadable) != 0 {
		t.Error("nil inputs must produce empty ones, not a panic")
	}
}
