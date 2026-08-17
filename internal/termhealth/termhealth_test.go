package termhealth

import (
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var now = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

func delTime(ago time.Duration) *metav1.Time { t := metav1.NewTime(now.Add(-ago)); return &t }

func TestAssess_StuckNamespaceNamesCondition(t *testing.T) {
	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns", DeletionTimestamp: delTime(3 * time.Hour)},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: "Some content in the namespace has finalizers remaining: kubernetes."}}},
	}
	got := Assess([]corev1.Namespace{ns}, nil, nil, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "Namespace" || got[0].Namespace != "" || got[0].Name != "legacy-ns" {
		t.Fatalf("want one Namespace issue, got %+v", got)
	}
	if got[0].Age != "3h" {
		t.Errorf("Age = %q, want 3h", got[0].Age)
	}
	if !contains(got[0].Reason, "NamespaceFinalizersRemaining") || !contains(got[0].Reason, "finalizers remaining") {
		t.Errorf("Reason = %q, want it to name the condition", got[0].Reason)
	}
}

func TestAssess_PodPastGraceWithFinalizer(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-7c9d5",
		DeletionTimestamp: delTime(8 * time.Minute), Finalizers: []string{"example.com/cleanup-hook"}}}
	got := Assess(nil, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "Pod" || !got[0].PastGrace {
		t.Fatalf("want one PastGrace Pod issue, got %+v", got)
	}
	if got[0].Namespace != "shop" || got[0].Age != "8m" || !contains(got[0].Reason, "example.com/cleanup-hook") {
		t.Errorf("issue = %+v", got[0])
	}
}

func TestAssess_PodPastGraceNoFinalizer(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "orphan", DeletionTimestamp: delTime(10 * time.Minute)}}
	got := Assess(nil, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 1 || !contains(got[0].Reason, "deletion not confirmed") {
		t.Fatalf("want the node/kubelet reason, got %+v", got)
	}
}

func TestAssess_PVCProtectionNamesMountingPod(t *testing.T) {
	pvc := corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "data",
		DeletionTimestamp: delTime(20 * time.Minute), Finalizers: []string{"kubernetes.io/pvc-protection"}}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "db-0"},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}}}
	got := Assess(nil, []corev1.Pod{pod}, []corev1.PersistentVolumeClaim{pvc}, 2*time.Minute, now)
	if len(got) != 1 || got[0].Kind != "PersistentVolumeClaim" || !contains(got[0].Reason, "still mounted by pod shop/db-0") {
		t.Fatalf("want the mounting-pod reason, got %+v", got)
	}
}

func TestAssess_BelowThresholdAndNoDeletionSkipped(t *testing.T) {
	recent := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "recent", DeletionTimestamp: delTime(30 * time.Second)}}
	alive := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "alive"}}
	if got := Assess(nil, []corev1.Pod{recent, alive}, nil, 2*time.Minute, now); len(got) != 0 {
		t.Errorf("a <threshold deletion and a non-deleting pod must not be flagged, got %+v", got)
	}
}

func TestAssess_SortedByKindNamespaceName(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "z-ns", DeletionTimestamp: delTime(1 * time.Hour)}}
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p", DeletionTimestamp: delTime(1 * time.Hour)}}
	got := Assess([]corev1.Namespace{ns}, []corev1.Pod{pod}, nil, 2*time.Minute, now)
	if len(got) != 2 || got[0].Kind != "Namespace" || got[1].Kind != "Pod" {
		t.Errorf("want Namespace before Pod (sorted by Kind), got %+v", got)
	}
}

func TestAssess_NamespaceIgnoresResolvedCondition(t *testing.T) {
	// A ConditionFalse (resolved) condition must NOT be reported; fall back to spec.finalizers.
	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns", DeletionTimestamp: delTime(1 * time.Hour)},
		Spec:       corev1.NamespaceSpec{Finalizers: []corev1.FinalizerName{"kubernetes"}},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionFalse, Message: "resolved"}}},
	}
	got := Assess([]corev1.Namespace{ns}, nil, nil, 2*time.Minute, now)
	if len(got) != 1 {
		t.Fatalf("want one issue, got %+v", got)
	}
	if contains(got[0].Reason, "NamespaceFinalizersRemaining") {
		t.Errorf("a ConditionFalse condition must not be reported as the blocker, got %q", got[0].Reason)
	}
	if !contains(got[0].Reason, "finalizers kubernetes") {
		t.Errorf("want the spec.finalizers fallback, got %q", got[0].Reason)
	}
}

func TestAssess_ExactlyAtThresholdNotFlagged(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "edge", DeletionTimestamp: delTime(2 * time.Minute)}}
	if got := Assess(nil, []corev1.Pod{pod}, nil, 2*time.Minute, now); len(got) != 0 {
		t.Errorf("a deletion exactly at the threshold must not be flagged, got %+v", got)
	}
}

func TestCompactDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{31 * time.Second, "<1m"},
		{59 * time.Second, "<1m"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m"},
		{61 * time.Minute, "1h"},
		{25 * time.Hour, "1d"},
	}
	for _, c := range cases {
		if got := compactDur(c.d); got != c.want {
			t.Errorf("compactDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// hostileMsg carries what a condition message may contain but a terminal must
// never receive: an ANSI escape that clears the screen, a right-to-left override
// that reorders everything after it, a NUL, and an invalid UTF-8 byte.
const hostileMsg = "content has finalizers\x1b[2J‮gnp\x00 remaining\xff"

// A namespace condition's message is free text the API server does not validate,
// and it lands in the Issue's Reason — which travels into findings.Finding.Reason,
// gate's verdict JSON and the SARIF a pipeline uploads.
func TestAssess_SanitizesANamespaceConditionMessage(t *testing.T) {
	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-ns", DeletionTimestamp: delTime(3 * time.Hour)},
		Status: corev1.NamespaceStatus{Conditions: []corev1.NamespaceCondition{
			{Type: "NamespaceFinalizersRemaining", Status: corev1.ConditionTrue, Message: hostileMsg}}},
	}
	got := Assess([]corev1.Namespace{ns}, nil, nil, 2*time.Minute, now)
	if len(got) != 1 {
		t.Fatalf("want one issue, got %d", len(got))
	}
	assertSanitized(t, got[0].Reason)
	// The condition is selected by type, so sanitizing the message changes no
	// matching decision — the type must still name itself in the reason.
	if !contains(got[0].Reason, "NamespaceFinalizersRemaining") {
		t.Errorf("Reason = %q, want it to still name the condition type", got[0].Reason)
	}
}

// assertSanitized fails when s carries anything safetext.Line removes. It is
// deliberately not "s == safetext.Line(raw)": the point is what reaches the
// terminal, not which helper produced it.
func assertSanitized(t *testing.T, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("invalid UTF-8 reached the reason: %q", s)
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("control or formatting character %U reached the reason: %q", r, s)
		}
	}
}
