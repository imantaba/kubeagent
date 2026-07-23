// Package webhookhealth flags admission webhooks whose failurePolicy is Fail and
// whose backing Service is missing or has no ready endpoints, or has a high
// timeoutSeconds (a latency landmine) — such a webhook rejects every create/update
// it intercepts, cluster-wide. Pure and read-only: the caller supplies the webhook
// configs and the collected Services/EndpointSlices. Advisory (never affects the
// cluster verdict).
package webhookhealth

import (
	"fmt"
	"sort"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"

	"github.com/imantaba/kubeagent/internal/svchealth"
)

// Issue is one Fail-policy admission webhook that is a problem: its backend is
// down (missing-service / no-endpoints) or its timeoutSeconds is a latency risk
// (high-timeout).
type Issue struct {
	Kind    string `json:"kind"`    // "ValidatingWebhookConfiguration" | "MutatingWebhookConfiguration"
	Config  string `json:"config"`  // the configuration object's name
	Webhook string `json:"webhook"` // the individual webhook's .name
	Service string `json:"service"` // "ns/name" of the backend ("" for a URL webhook)
	Problem string `json:"problem"` // "missing-service" | "no-endpoints" | "high-timeout"
	Reason  string `json:"reason"`
}

// hook is a normalized view of a Validating/Mutating webhook entry so one routine
// handles both (they share these fields but are distinct Go types).
type hook struct {
	kind    string
	config  string
	name    string
	fp      *admissionv1.FailurePolicyType
	service *admissionv1.ServiceReference
	timeout *int32
}

// Assess flags Fail-policy webhooks whose backend Service is missing or has no
// ready endpoints, or whose timeoutSeconds is >= timeoutThreshold, sorted by
// (Kind, Config, Webhook).
func Assess(
	validating []admissionv1.ValidatingWebhookConfiguration,
	mutating []admissionv1.MutatingWebhookConfiguration,
	services []corev1.Service,
	slices []discoveryv1.EndpointSlice,
	timeoutThreshold int32,
) []Issue {
	var hooks []hook
	for _, c := range validating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"ValidatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service, w.TimeoutSeconds})
		}
	}
	for _, c := range mutating {
		for _, w := range c.Webhooks {
			hooks = append(hooks, hook{"MutatingWebhookConfiguration", c.Name, w.Name, w.FailurePolicy, w.ClientConfig.Service, w.TimeoutSeconds})
		}
	}

	var out []Issue
	for _, h := range hooks {
		if !failsClosed(h.fp) {
			continue // Ignore policy
		}
		backendFlagged := false
		if h.service != nil {
			id := h.service.Namespace + "/" + h.service.Name
			svc, found := findService(services, h.service.Namespace, h.service.Name)
			switch {
			case !found:
				out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "missing-service",
					Reason: fmt.Sprintf("backend Service %s does not exist — failurePolicy Fail rejects every intercepted create/update", id)})
				backendFlagged = true
			case svchealth.ReadyEndpoints(svc, slices) == 0:
				out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "no-endpoints",
					Reason: fmt.Sprintf("backend Service %s has no ready endpoints — failurePolicy Fail rejects every intercepted create/update", id)})
				backendFlagged = true
			}
		}
		if !backendFlagged && h.timeout != nil && *h.timeout >= timeoutThreshold {
			id := ""
			if h.service != nil {
				id = h.service.Namespace + "/" + h.service.Name
			}
			out = append(out, Issue{Kind: h.kind, Config: h.config, Webhook: h.name, Service: id, Problem: "high-timeout",
				Reason: fmt.Sprintf("timeoutSeconds %d ≥ %ds under failurePolicy Fail — a slow webhook blocks every intercepted create/update for up to %ds, then rejects it", *h.timeout, timeoutThreshold, *h.timeout)})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Config != out[j].Config {
			return out[i].Config < out[j].Config
		}
		return out[i].Webhook < out[j].Webhook
	})
	return out
}

// failsClosed reports whether a webhook blocks on backend failure. A nil
// failurePolicy defaults to Fail in admissionregistration.k8s.io/v1.
func failsClosed(fp *admissionv1.FailurePolicyType) bool {
	return fp == nil || *fp == admissionv1.Fail
}

// findService returns the collected Service matching ns/name, or ok=false.
func findService(services []corev1.Service, ns, name string) (corev1.Service, bool) {
	for _, s := range services {
		if s.Namespace == ns && s.Name == name {
			return s, true
		}
	}
	return corev1.Service{}, false
}
