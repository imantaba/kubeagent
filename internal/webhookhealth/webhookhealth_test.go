package webhookhealth

import (
	"regexp"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func failP() *admissionv1.FailurePolicyType   { f := admissionv1.Fail; return &f }
func ignoreP() *admissionv1.FailurePolicyType { f := admissionv1.Ignore; return &f }
func svcRef(ns, name string) *admissionv1.ServiceReference {
	return &admissionv1.ServiceReference{Namespace: ns, Name: name}
}

func i32(n int32) *int32 { return &n }

// oneRule is a single any-resource rule, used as the default Rules value by the
// vhook/vhookT/mhook/mhookT helpers below. Without it every pre-existing fixture
// — which exists to test backend/timeout logic, not rules-emptiness — would be
// silently caught by the "no Rules" skip Assess gained for a rules-less webhook.
func oneRule() []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Create},
		Rule:       admissionv1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}},
	}}
}

// vhookT / mhookT build a webhook with a timeoutSeconds set.
func vhookT(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig, timeout int32) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, TimeoutSeconds: i32(timeout), Rules: oneRule()}
}
func mhookT(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig, timeout int32) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, TimeoutSeconds: i32(timeout), Rules: oneRule()}
}

func vwc(name string, ws ...admissionv1.ValidatingWebhook) admissionv1.ValidatingWebhookConfiguration {
	return admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func mwc(name string, ws ...admissionv1.MutatingWebhook) admissionv1.MutatingWebhookConfiguration {
	return admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func vhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, Rules: oneRule()}
}
func mhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc, Rules: oneRule()}
}

func svc(ns, name string) corev1.Service {
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}
func svcTyped(ns, name string, t corev1.ServiceType) corev1.Service {
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Type: t}}
}
func sliceFor(ns, svcName string, ready bool) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svcName + "-x", Labels: map[string]string{discoveryv1.LabelServiceName: svcName}},
		Endpoints:  []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
}

func find(issues []Issue, webhook string) (Issue, bool) {
	for _, i := range issues {
		if i.Webhook == webhook {
			return i, true
		}
	}
	return Issue{}, false
}

func TestAssess_NoEndpoints(t *testing.T) {
	v := vwc("policy-webhook", vhook("validate.policy.io", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("kube-system", "policy-svc")}))
	services := []corev1.Service{svc("kube-system", "policy-svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("kube-system", "policy-svc", false)} // 0 ready
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "validate.policy.io")
	if !ok || is.Problem != "NoEndpoints" {
		t.Fatalf("want NoEndpoints, got %+v", is)
	}
	if is.Kind != "ValidatingWebhookConfiguration" || is.Config != "policy-webhook" || is.Service != "kube-system/policy-svc" {
		t.Errorf("wrong identity: %+v", is)
	}
	if is.Reason != "backend Service kube-system/policy-svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_MissingService(t *testing.T) {
	m := mwc("image-signing", mhook("sign.example.com", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("secure", "signer")}))
	is, ok := find(Assess(nil, []admissionv1.MutatingWebhookConfiguration{m}, nil, nil, 15), "sign.example.com")
	if !ok || is.Problem != "MissingService" {
		t.Fatalf("want MissingService, got %+v", is)
	}
	if is.Kind != "MutatingWebhookConfiguration" {
		t.Errorf("kind = %q", is.Kind)
	}
	if is.Reason != "backend Service secure/signer does not exist — failurePolicy Fail rejects every intercepted create/update" {
		t.Errorf("reason = %q", is.Reason)
	}
}

func TestAssess_NilFailurePolicyIsFail(t *testing.T) {
	// nil failurePolicy defaults to Fail in admissionregistration.k8s.io/v1.
	v := vwc("c", vhook("w", nil, admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "w"); !ok {
		t.Fatal("a nil-failurePolicy webhook with a down backend must be flagged")
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	services := []corev1.Service{svc("ns", "up")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "up", true)} // ready
	url := "https://external.example.com/hook"
	cases := []admissionv1.ValidatingWebhookConfiguration{
		vwc("ignore", vhook("ig", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")})), // Ignore → not blocking
		vwc("urlhook", vhook("u", failP(), admissionv1.WebhookClientConfig{URL: &url})),                       // URL, nil timeout → not flagged
		vwc("healthy", vhook("h", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "up")})),     // ready backend
	}
	if got := Assess(cases, nil, services, slices, 15); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_SortedAndPerWebhook(t *testing.T) {
	// two down webhooks in one config → two issues, sorted by webhook name.
	v := vwc("cfg",
		vhook("b-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}),
		vhook("a-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15)
	if len(got) != 2 || got[0].Webhook != "a-hook" || got[1].Webhook != "b-hook" {
		t.Fatalf("want two issues sorted by webhook, got %+v", got)
	}
}

func TestAssess_SortsMutatingBeforeValidating(t *testing.T) {
	// Cross-kind ordering: "MutatingWebhookConfiguration" < "ValidatingWebhookConfiguration".
	v := vwc("vcfg", vhook("vw", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	m := mwc("mcfg", mhook("mw", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, []admissionv1.MutatingWebhookConfiguration{m}, nil, nil, 15)
	if len(got) != 2 || got[0].Kind != "MutatingWebhookConfiguration" || got[1].Kind != "ValidatingWebhookConfiguration" {
		t.Fatalf("want Mutating sorted before Validating, got %+v", got)
	}
}

func TestAssess_HighTimeoutFlagged(t *testing.T) {
	v := vwc("slow-validator", vhookT("policy.example.com", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("kube-system", "policy-svc")}, 30))
	services := []corev1.Service{svc("kube-system", "policy-svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("kube-system", "policy-svc", true)} // healthy backend

	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "policy.example.com")
	if !ok {
		t.Fatal("want a high-timeout issue for a healthy 30s Fail webhook")
	}
	if is.Problem != "HighTimeout" {
		t.Errorf("Problem = %q, want HighTimeout", is.Problem)
	}
	if !strings.Contains(is.Reason, "timeoutSeconds 30") || !strings.Contains(is.Reason, "≥ 15s") {
		t.Errorf("Reason = %q", is.Reason)
	}
}

func TestAssess_TimeoutAtThresholdFlagged(t *testing.T) {
	v := vwc("edge", vhookT("edge.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 15))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "edge.io"); !ok {
		t.Error("timeoutSeconds == threshold (15) should be flagged (inclusive)")
	}
}

func TestAssess_TimeoutBelowThresholdNotFlagged(t *testing.T) {
	v := vwc("ok", vhookT("ok.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 14))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "ok.io"); ok {
		t.Error("timeoutSeconds 14 < 15 should not be flagged")
	}
}

func TestAssess_IgnorePolicyHighTimeoutNotFlagged(t *testing.T) {
	v := vwc("lax", vhookT("lax.io", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "lax.io"); ok {
		t.Error("Ignore-policy webhook must not be latency-flagged")
	}
}

func TestAssess_NilTimeoutNotFlagged(t *testing.T) {
	// vhook (no timeout) → TimeoutSeconds nil
	v := vwc("nilto", vhook("nilto.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "nilto.io"); ok {
		t.Error("nil timeoutSeconds must not be flagged")
	}
}

func TestAssess_BackendDownHighTimeoutNoDoubleReport(t *testing.T) {
	v := vwc("down", vhookT("down.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, 30))
	// no Service "gone" collected → MissingService
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15)
	if len(got) != 1 {
		t.Fatalf("want exactly one issue (backend, not doubled), got %+v", got)
	}
	if got[0].Problem != "MissingService" {
		t.Errorf("Problem = %q, want MissingService (backend wins)", got[0].Problem)
	}
}

func TestAssess_URLWebhookHighTimeoutFlagged(t *testing.T) {
	u := "https://hook.example.com/validate"
	v := vwc("urlhook", vhookT("url.io", failP(), admissionv1.WebhookClientConfig{URL: &u}, 30))
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "url.io")
	if !ok {
		t.Fatal("URL-based Fail webhook with high timeout should be flagged")
	}
	if is.Problem != "HighTimeout" || is.Service != "" {
		t.Errorf("issue = %+v, want HighTimeout with empty Service", is)
	}
}

func TestAssess_MutatingHighTimeoutFlagged(t *testing.T) {
	m := mwc("slow-mutator", mhookT("mut.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess(nil, []admissionv1.MutatingWebhookConfiguration{m}, services, slices, 15), "mut.io"); !ok {
		t.Error("mutating high-timeout webhook should be flagged")
	}
}

func TestAssess_ThresholdRespected(t *testing.T) {
	v := vwc("t", vhookT("t.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 31), "t.io"); ok {
		t.Error("threshold 31 should not flag a 30s webhook")
	}
}

// TestAssess_ProblemValuesAreCamelCase locks the shape of every value Assess
// puts in Problem: CamelCase, not the hyphenated style it used to be. It
// drives all three problem kinds through Assess itself, so a future problem
// value that rejoins the hyphenated style fails this test rather than
// silently reintroducing the vocabulary split this package was renamed to
// close.
func TestAssess_ProblemValuesAreCamelCase(t *testing.T) {
	down := vwc("down-cfg", vhook("missing.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	noEP := vwc("noep-cfg", vhook("noendpoints.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "up")}))
	slow := vwc("slow-cfg", vhookT("slow.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "healthy")}, 30))
	services := []corev1.Service{svc("ns", "up"), svc("ns", "healthy")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "up", false), sliceFor("ns", "healthy", true)}

	got := Assess([]admissionv1.ValidatingWebhookConfiguration{down, noEP, slow}, nil, services, slices, 15)
	if len(got) != 3 {
		t.Fatalf("want 3 issues, got %d: %+v", len(got), got)
	}
	shape := regexp.MustCompile(`^[A-Z][A-Za-z]*$`)
	seen := map[string]bool{}
	for _, is := range got {
		if !shape.MatchString(is.Problem) {
			t.Errorf("Problem %q does not match ^[A-Z][A-Za-z]*$", is.Problem)
		}
		seen[is.Problem] = true
	}
	for _, want := range []string{"MissingService", "NoEndpoints", "HighTimeout"} {
		if !seen[want] {
			t.Errorf("missing Problem value %q among %+v", want, got)
		}
	}
}

// TestAssess_ExternalNameNoEndpointsNotFlagged mirrors svchealth.go:39-41: an
// ExternalName Service is a DNS CNAME, not endpoint-backed, so
// svchealth.ReadyEndpoints reading 0 on it is not evidence of a down backend.
func TestAssess_ExternalNameNoEndpointsNotFlagged(t *testing.T) {
	v := vwc("ext-cfg", vhook("ext.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "ext-svc")}))
	services := []corev1.Service{svcTyped("ns", "ext-svc", corev1.ServiceTypeExternalName)}
	// No EndpointSlice for ext-svc at all — deliberately, since an ExternalName
	// Service is never expected to have one.
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, nil, 15)
	if len(got) != 0 {
		t.Fatalf("an ExternalName backend must not be flagged as endpoint-less, got %+v", got)
	}
}

// TestAssess_ClusterIPNoEndpointsStillFlagged proves the ExternalName guard is
// not over-broad: a same-shaped ClusterIP Service with zero ready endpoints must
// still produce NoEndpoints.
func TestAssess_ClusterIPNoEndpointsStillFlagged(t *testing.T) {
	v := vwc("clusterip-cfg", vhook("cip.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "cip-svc")}))
	services := []corev1.Service{svcTyped("ns", "cip-svc", corev1.ServiceTypeClusterIP)}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "cip-svc", false)}
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "cip.io")
	if !ok || is.Problem != "NoEndpoints" {
		t.Fatalf("a ClusterIP backend with no ready endpoints must still be NoEndpoints, got %+v", is)
	}
}

// TestAssess_ExternalNameMissingServiceStillFlagged proves the guard sits inside
// the found branch only: an ExternalName-intended Service that does not exist at
// all is still MissingService.
func TestAssess_ExternalNameMissingServiceStillFlagged(t *testing.T) {
	v := vwc("ext-missing-cfg", vhook("extmissing.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone-ext")}))
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "extmissing.io")
	if !ok || is.Problem != "MissingService" {
		t.Fatalf("an absent backend must still be MissingService regardless of the intended Service type, got %+v", is)
	}
}

// TestAssess_ExternalNameHighTimeoutStillFlagged proves backendFlagged stays
// false for an ExternalName backend, so the timeout check still runs.
func TestAssess_ExternalNameHighTimeoutStillFlagged(t *testing.T) {
	v := vwc("ext-slow-cfg", vhookT("extslow.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "ext-svc")}, 30))
	services := []corev1.Service{svcTyped("ns", "ext-svc", corev1.ServiceTypeExternalName)}
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, nil, 15), "extslow.io")
	if !ok || is.Problem != "HighTimeout" {
		t.Fatalf("an ExternalName backend with a high timeout must still be flagged as HighTimeout, got %+v", is)
	}
}

// TestAssess_NilRulesNotFlagged: a webhook with Rules: nil intercepts nothing,
// so a down backend on it cannot have rejected anything.
func TestAssess_NilRulesNotFlagged(t *testing.T) {
	w := admissionv1.ValidatingWebhook{Name: "norules.io", FailurePolicy: failP(),
		ClientConfig: admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, Rules: nil}
	v := vwc("norules-cfg", w)
	if got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15); len(got) != 0 {
		t.Fatalf("a webhook with Rules: nil must not be flagged, got %+v", got)
	}
}

// TestAssess_EmptySliceRulesNotFlagged: an explicit empty slice is the same as
// nil for this guard.
func TestAssess_EmptySliceRulesNotFlagged(t *testing.T) {
	w := admissionv1.ValidatingWebhook{Name: "emptyrules.io", FailurePolicy: failP(),
		ClientConfig: admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")},
		Rules:        []admissionv1.RuleWithOperations{}}
	v := vwc("emptyrules-cfg", w)
	if got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15); len(got) != 0 {
		t.Fatalf("a webhook with Rules: []...{} must not be flagged, got %+v", got)
	}
}

// TestAssess_OneRuleStillFlagged proves the skip is on emptiness only: one rule
// is enough for a down backend to be reported.
func TestAssess_OneRuleStillFlagged(t *testing.T) {
	w := admissionv1.ValidatingWebhook{Name: "onerule.io", FailurePolicy: failP(),
		ClientConfig: admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")},
		Rules:        oneRule(),
	}
	v := vwc("onerule-cfg", w)
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "onerule.io")
	if !ok || is.Problem != "MissingService" {
		t.Fatalf("a webhook with one rule must still be flagged, got %+v", is)
	}
}

// TestAssess_EmptyRulesHighTimeoutNotFlagged proves the rules-empty skip
// precedes the timeout check, not just the backend check.
func TestAssess_EmptyRulesHighTimeoutNotFlagged(t *testing.T) {
	w := admissionv1.ValidatingWebhook{Name: "norulesslow.io", FailurePolicy: failP(),
		ClientConfig: admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, TimeoutSeconds: i32(30)}
	v := vwc("norulesslow-cfg", w)
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", true)}
	if got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15); len(got) != 0 {
		t.Fatalf("the rules-empty skip must precede the timeout check, got %+v", got)
	}
}

// TestAssess_MissingServiceHighTimeoutReasonSuffix: a webhook whose backend is
// missing AND whose timeoutSeconds clears the threshold still produces exactly
// one Issue (the backendFlagged guard is correct — a dead backend dominates a
// slow one), but the co-occurring timeout is now named in the reason rather
// than silently dropped.
func TestAssess_MissingServiceHighTimeoutReasonSuffix(t *testing.T) {
	v := vwc("down", vhookT("down.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, 30))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15)
	if len(got) != 1 {
		t.Fatalf("want exactly one issue (backend, not doubled), got %+v", got)
	}
	is := got[0]
	if is.Problem != "MissingService" {
		t.Fatalf("Problem = %q, want MissingService", is.Problem)
	}
	want := "backend Service ns/gone does not exist — failurePolicy Fail rejects every intercepted create/update (and timeoutSeconds 30 ≥ 15s)"
	if is.Reason != want {
		t.Errorf("Reason = %q, want %q", is.Reason, want)
	}
}

// TestAssess_NoEndpointsHighTimeoutReasonSuffix proves the suffix reaches the
// NoEndpoints arm too — both backend arms call the same helper so the two
// sites cannot drift.
func TestAssess_NoEndpointsHighTimeoutReasonSuffix(t *testing.T) {
	v := vwc("noep-slow", vhookT("noepslow.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "svc")}, 30))
	services := []corev1.Service{svc("ns", "svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "svc", false)} // 0 ready
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "noepslow.io")
	if !ok || is.Problem != "NoEndpoints" {
		t.Fatalf("want NoEndpoints, got %+v", is)
	}
	want := "backend Service ns/svc has no ready endpoints — failurePolicy Fail rejects every intercepted create/update (and timeoutSeconds 30 ≥ 15s)"
	if is.Reason != want {
		t.Errorf("Reason = %q, want %q", is.Reason, want)
	}
}

// TestAssess_MissingServiceLowTimeoutNoSuffix proves the suffix only appears
// when the timeout condition is itself true — timeoutSeconds below the
// threshold leaves the backend reason exactly as it was before this decision.
func TestAssess_MissingServiceLowTimeoutNoSuffix(t *testing.T) {
	v := vwc("down5", vhookT("down5.io", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, 5))
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15), "down5.io")
	if !ok || is.Problem != "MissingService" {
		t.Fatalf("want MissingService, got %+v", is)
	}
	want := "backend Service ns/gone does not exist — failurePolicy Fail rejects every intercepted create/update"
	if is.Reason != want {
		t.Errorf("Reason = %q, want %q (no suffix)", is.Reason, want)
	}
}

// TestAssess_HealthyBackendHighTimeoutNoSuffix proves the suffix belongs to a
// backend issue only — a standalone HighTimeout issue (no backend problem on
// the same hook) keeps its own reason unchanged, no double-naming of the same
// timeout.
func TestAssess_HealthyBackendHighTimeoutNoSuffix(t *testing.T) {
	v := vwc("slow-validator", vhookT("policy.example.com", failP(),
		admissionv1.WebhookClientConfig{Service: svcRef("kube-system", "policy-svc")}, 30))
	services := []corev1.Service{svc("kube-system", "policy-svc")}
	slices := []discoveryv1.EndpointSlice{sliceFor("kube-system", "policy-svc", true)} // healthy backend
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices, 15), "policy.example.com")
	if !ok || is.Problem != "HighTimeout" {
		t.Fatalf("want HighTimeout, got %+v", is)
	}
	if strings.Contains(is.Reason, "(and timeoutSeconds") {
		t.Errorf("a standalone HighTimeout reason must not gain the co-occurrence suffix, got %q", is.Reason)
	}
}

// TestAssess_IgnorePolicyMissingServiceHighTimeoutNoIssue proves an Ignore
// policy suppresses both conditions together — not just the timeout check
// alone, which TestAssess_IgnorePolicyHighTimeoutNotFlagged already covers on
// a healthy backend.
func TestAssess_IgnorePolicyMissingServiceHighTimeoutNoIssue(t *testing.T) {
	v := vwc("laxdown", vhookT("laxdown.io", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}, 30))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil, 15)
	if len(got) != 0 {
		t.Fatalf("Ignore policy with missing service and high timeout must produce no issue, got %+v", got)
	}
}
