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
