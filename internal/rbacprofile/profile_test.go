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

// pvcreclaim's summary once described a released-PV audit that does not
// exist; diskusage's once claimed to read inode pressure it never decodes.
// Both summaries must name the check that actually runs.
func TestPvcReclaimAndDiskUsageSummariesAreAccurate(t *testing.T) {
	pvcreclaim, ok := Lookup("pvcreclaim")
	if !ok {
		t.Fatal("no pvcreclaim feature in the table")
	}
	if !strings.Contains(pvcreclaim.Summary, "PersistentVolumeClaim") {
		t.Errorf("pvcreclaim Summary %q does not name PersistentVolumeClaim", pvcreclaim.Summary)
	}
	if strings.Contains(pvcreclaim.Summary, "released") {
		t.Errorf("pvcreclaim Summary %q still describes the released-PV audit that does not exist", pvcreclaim.Summary)
	}

	diskusage, ok := Lookup("diskusage")
	if !ok {
		t.Fatal("no diskusage feature in the table")
	}
	if !strings.Contains(diskusage.Summary, "PersistentVolumeClaim") {
		t.Errorf("diskusage Summary %q does not name PersistentVolumeClaim", diskusage.Summary)
	}
	if strings.Contains(diskusage.Summary, "inode") {
		t.Errorf("diskusage Summary %q still claims to read inode pressure, which parseNodeSummary never decodes", diskusage.Summary)
	}
}

// security's summary once listed individual checks (privileged, hostPath,
// hostNetwork), so a tenth check made it stale again. It now names
// categories, which a new check within an existing category does not.
func TestSecuritySummaryNamesCategories(t *testing.T) {
	f, ok := Lookup("security")
	if !ok {
		t.Fatal("no security feature in the table")
	}
	want := "workload and Service security posture (privileged and host access, restricted-profile gaps, externally exposed Services) — no grant beyond core"
	if f.Summary != want {
		t.Errorf("security Summary = %q, want %q", f.Summary, want)
	}
}

// --investigate makes a model call, but every read the tool loop can issue
// stays within core's grants: EndpointSlices for a Service's ready-endpoint
// count, never the legacy Endpoints resource — pinned from the other side by
// TestReaderReadsStayWithinGrantedRBAC in internal/investigate, which drives
// one call of every tool/arm and checks each recorded action's resource
// against what core and logs actually ship. The one extra-grant dependency,
// get_log_causes reading pods/log, has its home in the logs feature: used
// when present, refused cleanly when not — which is why Rules stays empty
// here rather than duplicating logs' grant.
func TestInvestigateFeatureNeedsNoGrant(t *testing.T) {
	f, ok := Lookup("investigate")
	if !ok {
		t.Fatal("no investigate feature in the table")
	}
	if f.Flag != "--investigate" {
		t.Errorf("investigate Flag = %q, want --investigate", f.Flag)
	}
	if len(f.Rules) != 0 {
		t.Errorf("investigate declares %d rules; it needs no grant beyond core", len(f.Rules))
	}
	want := "read-only agentic investigation of findings via a model tool-use loop — no grant beyond core; its log-cause tool uses the logs feature's pods/log grant when present and refuses cleanly without it"
	if f.Summary != want {
		t.Errorf("investigate Summary = %q, want %q", f.Summary, want)
	}
}

// Where a feature's grant lives is not a comment, it is data: either the feature
// ships its own manifest, or another feature's manifest already covers it.
func TestEveryGrantHasExactlyOneHome(t *testing.T) {
	for _, f := range Features() {
		if len(f.Rules) == 0 {
			// A feature can declare zero rules of its own and still name who
			// covers it (policy: every kind it may select is one core already
			// grants), but it may never claim a manifest it does nothing to
			// justify.
			if f.Manifest != "" {
				t.Errorf("feature %q needs no grant but claims a manifest", f.Name)
			}
			if f.CoveredBy != "" {
				if _, ok := Lookup(f.CoveredBy); !ok {
					t.Errorf("feature %q is CoveredBy unknown feature %q", f.Name, f.CoveredBy)
				}
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

// The policy feature must cost nothing: the kinds a rule may select are the
// kinds core already grants, so a shipped ClusterRole that grew a rule because
// of this feature means the selectable-kind table drifted from coreRules.
func TestPolicyFeatureAddsNoRules(t *testing.T) {
	var f *Feature
	for i := range features {
		if features[i].Name == "policy" {
			f = &features[i]
		}
	}
	if f == nil {
		t.Fatal("no policy feature in the table")
	}
	if len(f.Rules) != 0 {
		t.Errorf("the policy feature declares %d rules; it must be covered by core", len(f.Rules))
	}
	if f.CoveredBy != "core" {
		t.Errorf("CoveredBy = %q, want core", f.CoveredBy)
	}
	if f.Manifest != "" || f.RoleName != "" || f.HelmCondition != "" {
		t.Errorf("the policy feature must ship no manifest and gate nothing in the chart: %#v", f)
	}
	if !f.ScanOnly {
		t.Error("ScanOnly = false; the watch daemon has no --policy")
	}
}
