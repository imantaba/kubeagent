package quotahealth

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// quota builds a ResourceQuota with the given hard/used maps (quantity strings).
func quota(ns, name string, hard, used map[corev1.ResourceName]string) corev1.ResourceQuota {
	h := corev1.ResourceList{}
	for k, v := range hard {
		h[k] = resource.MustParse(v)
	}
	u := corev1.ResourceList{}
	for k, v := range used {
		u[k] = resource.MustParse(v)
	}
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.ResourceQuotaStatus{Hard: h, Used: u},
	}
}

func approxEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// findIssue returns the first Issue for a resource, or a zero Issue and false.
func findIssue(issues []Issue, res string) (Issue, bool) {
	for _, is := range issues {
		if is.Resource == res {
			return is, true
		}
	}
	return Issue{}, false
}

func TestAssess_Near(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"pods": "50"},
		map[corev1.ResourceName]string{"pods": "47"})}

	got := Assess(qs, 0.90)

	if len(got) != 1 {
		t.Fatalf("want 1 issue, got %+v", got)
	}
	is := got[0]
	if is.Severity != "near" {
		t.Errorf("Severity = %q, want near", is.Severity)
	}
	if is.Used != "47" || is.Hard != "50" {
		t.Errorf("Used/Hard = %q/%q, want 47/50", is.Used, is.Hard)
	}
	if !approxEq(is.Ratio, 0.94) {
		t.Errorf("Ratio = %v, want ~0.94", is.Ratio)
	}
	if is.Namespace != "shop" || is.Quota != "compute" || is.Resource != "pods" {
		t.Errorf("identity = %s/%s %s, want shop/compute pods", is.Namespace, is.Quota, is.Resource)
	}
}

func TestAssess_Exhausted(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"requests.cpu": "4"},
		map[corev1.ResourceName]string{"requests.cpu": "4"})}

	got := Assess(qs, 0.90)

	if len(got) != 1 || got[0].Severity != "exhausted" {
		t.Fatalf("want one exhausted issue, got %+v", got)
	}
	if !approxEq(got[0].Ratio, 1.0) {
		t.Errorf("Ratio = %v, want 1.0", got[0].Ratio)
	}
}

func TestAssess_SubThresholdNotFlagged(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"pods": "50"},
		map[corev1.ResourceName]string{"pods": "40"})} // 0.80

	if got := Assess(qs, 0.90); len(got) != 0 {
		t.Errorf("want no issue at 0.80 < 0.90, got %+v", got)
	}
}

func TestAssess_ZeroHardSkipped(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "no-pods",
		map[corev1.ResourceName]string{"pods": "0"},
		map[corev1.ResourceName]string{"pods": "0"})}

	if got := Assess(qs, 0.90); len(got) != 0 {
		t.Errorf("want no issue for hard=0 (no div-by-zero), got %+v", got)
	}
}

func TestAssess_GenericResources(t *testing.T) {
	qs := []corev1.ResourceQuota{quota("shop", "compute",
		map[corev1.ResourceName]string{"requests.cpu": "4", "count/configmaps": "10"},
		map[corev1.ResourceName]string{"requests.cpu": "3800m", "count/configmaps": "9"})}

	got := Assess(qs, 0.90)

	if len(got) != 2 {
		t.Fatalf("want 2 issues (cpu + configmaps), got %+v", got)
	}
	cpu, ok := findIssue(got, "requests.cpu")
	if !ok || cpu.Used != "3800m" || cpu.Hard != "4" || !approxEq(cpu.Ratio, 0.95) {
		t.Errorf("requests.cpu issue = %+v, want used 3800m/hard 4 ~0.95", cpu)
	}
	cm, ok := findIssue(got, "count/configmaps")
	if !ok || cm.Used != "9" || cm.Hard != "10" || !approxEq(cm.Ratio, 0.90) {
		t.Errorf("count/configmaps issue = %+v, want used 9/hard 10 ~0.90", cm)
	}
}

func TestAssess_SortPrecedence(t *testing.T) {
	qs := []corev1.ResourceQuota{
		quota("b-ns", "q", map[corev1.ResourceName]string{"pods": "100"}, map[corev1.ResourceName]string{"pods": "95"}), // near 0.95
		quota("a-ns", "q", map[corev1.ResourceName]string{"pods": "100"}, map[corev1.ResourceName]string{"pods": "92"}), // near 0.92
		quota("z-ns", "q", map[corev1.ResourceName]string{"pods": "10"}, map[corev1.ResourceName]string{"pods": "10"}),  // exhausted 1.0
	}

	got := Assess(qs, 0.90)

	if len(got) != 3 {
		t.Fatalf("want 3 issues, got %+v", got)
	}
	if got[0].Severity != "exhausted" || got[0].Namespace != "z-ns" {
		t.Errorf("issue[0] = %+v, want exhausted z-ns first", got[0])
	}
	if got[1].Namespace != "b-ns" || !approxEq(got[1].Ratio, 0.95) {
		t.Errorf("issue[1] = %+v, want near 0.95 (b-ns) before 0.92", got[1])
	}
	if got[2].Namespace != "a-ns" || !approxEq(got[2].Ratio, 0.92) {
		t.Errorf("issue[2] = %+v, want near 0.92 (a-ns) last", got[2])
	}
}

func TestAssess_EmptyReturnsNonNil(t *testing.T) {
	got := Assess(nil, 0.90)
	if got == nil {
		t.Error("want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %+v", got)
	}
}
