package webhookhealth

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func failP() *admissionv1.FailurePolicyType  { f := admissionv1.Fail; return &f }
func ignoreP() *admissionv1.FailurePolicyType { f := admissionv1.Ignore; return &f }
func svcRef(ns, name string) *admissionv1.ServiceReference {
	return &admissionv1.ServiceReference{Namespace: ns, Name: name}
}

func vwc(name string, ws ...admissionv1.ValidatingWebhook) admissionv1.ValidatingWebhookConfiguration {
	return admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func mwc(name string, ws ...admissionv1.MutatingWebhook) admissionv1.MutatingWebhookConfiguration {
	return admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: name}, Webhooks: ws}
}
func vhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.ValidatingWebhook {
	return admissionv1.ValidatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc}
}
func mhook(name string, fp *admissionv1.FailurePolicyType, cc admissionv1.WebhookClientConfig) admissionv1.MutatingWebhook {
	return admissionv1.MutatingWebhook{Name: name, FailurePolicy: fp, ClientConfig: cc}
}

func svc(ns, name string) corev1.Service {
	return corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
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
	is, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, services, slices), "validate.policy.io")
	if !ok || is.Problem != "no-endpoints" {
		t.Fatalf("want no-endpoints, got %+v", is)
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
	is, ok := find(Assess(nil, []admissionv1.MutatingWebhookConfiguration{m}, nil, nil), "sign.example.com")
	if !ok || is.Problem != "missing-service" {
		t.Fatalf("want missing-service, got %+v", is)
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
	if _, ok := find(Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil), "w"); !ok {
		t.Fatal("a nil-failurePolicy webhook with a down backend must be flagged")
	}
}

func TestAssess_NotFlagged(t *testing.T) {
	services := []corev1.Service{svc("ns", "up")}
	slices := []discoveryv1.EndpointSlice{sliceFor("ns", "up", true)} // ready
	url := "https://external.example.com/hook"
	cases := []admissionv1.ValidatingWebhookConfiguration{
		vwc("ignore", vhook("ig", ignoreP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")})), // Ignore → not blocking
		vwc("urlhook", vhook("u", failP(), admissionv1.WebhookClientConfig{URL: &url})),                        // URL → can't check
		vwc("healthy", vhook("h", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "up")})),      // ready backend
	}
	if got := Assess(cases, nil, services, slices); len(got) != 0 {
		t.Fatalf("expected nothing flagged, got %+v", got)
	}
}

func TestAssess_SortedAndPerWebhook(t *testing.T) {
	// two down webhooks in one config → two issues, sorted by webhook name.
	v := vwc("cfg",
		vhook("b-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}),
		vhook("a-hook", failP(), admissionv1.WebhookClientConfig{Service: svcRef("ns", "gone")}))
	got := Assess([]admissionv1.ValidatingWebhookConfiguration{v}, nil, nil, nil)
	if len(got) != 2 || got[0].Webhook != "a-hook" || got[1].Webhook != "b-hook" {
		t.Fatalf("want two issues sorted by webhook, got %+v", got)
	}
}
