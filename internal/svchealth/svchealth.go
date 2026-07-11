// Package svchealth flags Service-level problems a pod/workload scan misses: a
// selector-based Service with no ready backend endpoints, and a LoadBalancer
// Service with no external address. It is pure — the caller supplies the
// Services and EndpointSlices.
package svchealth

import (
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
)

// Issue is one Service-level problem.
type Issue struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Type      string `json:"type"`               // ClusterIP | NodePort | LoadBalancer
	Problem   string `json:"problem"`            // "NoEndpoints" | "NoExternalAddress"
	Detail    string `json:"detail"`             // human one-liner
	Since     string `json:"since,omitempty"`    // RFC3339 service creationTimestamp (LB age)
	Expected  bool   `json:"expected,omitempty"` // true for an expected (annotated) NoEndpoints issue
	Backing   string `json:"backing,omitempty"`  // representative backing kind, when classified
}

// Assess flags Service problems. One Issue per (service, problem); a LoadBalancer
// with no address AND no endpoints yields two. Result is sorted by
// (Namespace, Name, Problem). ExternalName and selectorless Services are skipped.
func Assess(services []corev1.Service, slices []discoveryv1.EndpointSlice, backends []Backend) []Issue {
	var out []Issue
	for _, s := range services {
		if s.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
			lb := Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoExternalAddress", Detail: "no external address",
			}
			if !s.CreationTimestamp.IsZero() {
				lb.Since = s.CreationTimestamp.Time.UTC().Format(time.RFC3339)
			}
			out = append(out, lb)
		}
		if len(s.Spec.Selector) == 0 {
			continue
		}
		if ReadyEndpoints(s, slices) == 0 {
			is := Issue{
				Namespace: s.Namespace, Name: s.Name, Type: string(s.Spec.Type),
				Problem: "NoEndpoints", Detail: "no ready endpoints",
			}
			if backing, detail, ok := classifyBacking(s, backends); ok {
				is.Expected = true
				is.Backing = backing
				is.Detail = detail
			}
			out = append(out, is)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Problem < out[j].Problem
	})
	return out
}

// ReadyEndpoints counts ready backend addresses for a Service across its
// EndpointSlices (matched by namespace + the kubernetes.io/service-name label).
func ReadyEndpoints(svc corev1.Service, slices []discoveryv1.EndpointSlice) int {
	total := 0
	for _, sl := range slices {
		if sl.Namespace != svc.Namespace || sl.Labels[discoveryv1.LabelServiceName] != svc.Name {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				total += len(ep.Addresses)
			}
		}
	}
	return total
}

// Backend describes a workload that may back a Service: its pod-template labels
// and whether it currently wants any pods.
type Backend struct {
	Kind           string // Deployment | StatefulSet | DaemonSet | Job | CronJob
	Namespace      string
	TemplateLabels map[string]string // the Service selector must be a subset of these
	Desired        int               // replicas / DesiredNumberScheduled (ignored when Ephemeral)
	Ephemeral      bool              // true for Job and CronJob
}

// BackendsFrom adapts the already-collected controller slices into Backends.
func BackendsFrom(deploys []appsv1.Deployment, statefulsets []appsv1.StatefulSet, daemonsets []appsv1.DaemonSet, jobs []batchv1.Job, cronjobs []batchv1.CronJob) []Backend {
	var out []Backend
	for _, d := range deploys {
		desired := 1
		if d.Spec.Replicas != nil {
			desired = int(*d.Spec.Replicas)
		}
		out = append(out, Backend{Kind: "Deployment", Namespace: d.Namespace, TemplateLabels: d.Spec.Template.Labels, Desired: desired})
	}
	for _, s := range statefulsets {
		desired := 1
		if s.Spec.Replicas != nil {
			desired = int(*s.Spec.Replicas)
		}
		out = append(out, Backend{Kind: "StatefulSet", Namespace: s.Namespace, TemplateLabels: s.Spec.Template.Labels, Desired: desired})
	}
	for _, ds := range daemonsets {
		out = append(out, Backend{Kind: "DaemonSet", Namespace: ds.Namespace, TemplateLabels: ds.Spec.Template.Labels, Desired: int(ds.Status.DesiredNumberScheduled)})
	}
	for _, j := range jobs {
		out = append(out, Backend{Kind: "Job", Namespace: j.Namespace, TemplateLabels: j.Spec.Template.Labels, Ephemeral: true})
	}
	for _, cj := range cronjobs {
		out = append(out, Backend{Kind: "CronJob", Namespace: cj.Namespace, TemplateLabels: cj.Spec.JobTemplate.Spec.Template.Labels, Ephemeral: true})
	}
	return out
}

// selectorMatches reports whether every key/value in selector is present in
// labels — i.e. the Service would select pods carrying these template labels.
// An empty selector never matches (selectorless Services are skipped upstream).
func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// classifyBacking decides whether a Service's lack of endpoints is expected
// because of the workload backing it. It returns ok=false (a primary issue)
// when a live non-ephemeral controller (desired>0) backs the Service, or when
// nothing matches. Otherwise it returns the representative backing kind and the
// explanatory detail line.
func classifyBacking(svc corev1.Service, backends []Backend) (backing, detail string, ok bool) {
	var matches []Backend
	for _, b := range backends {
		if b.Namespace == svc.Namespace && selectorMatches(svc.Spec.Selector, b.TemplateLabels) {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return "", "", false
	}
	for _, b := range matches {
		if !b.Ephemeral && b.Desired > 0 {
			return "", "", false // a live controller should have endpoints — real issue
		}
	}
	b := pickBacking(matches)
	return b.Kind, backingDetail(b), true
}

var backingOrder = map[string]int{"CronJob": 0, "Job": 1, "DaemonSet": 2, "Deployment": 3, "StatefulSet": 4}

// pickBacking chooses a representative backend in precedence order
// CronJob, Job, DaemonSet, Deployment, StatefulSet.
func pickBacking(matches []Backend) Backend {
	best := matches[0]
	for _, b := range matches[1:] {
		if backingOrder[b.Kind] < backingOrder[best.Kind] {
			best = b
		}
	}
	return best
}

// backingDetail is the human one-liner for an expected NoEndpoints issue.
func backingDetail(b Backend) string {
	switch b.Kind {
	case "CronJob":
		return "no ready endpoints (backs CronJob — expected between runs)"
	case "Job":
		return "no ready endpoints (backs Job — expected between runs)"
	case "DaemonSet":
		return "no ready endpoints (backs DaemonSet — 0 desired)"
	case "Deployment":
		return "no ready endpoints (backs Deployment — scaled to 0)"
	case "StatefulSet":
		return "no ready endpoints (backs StatefulSet — scaled to 0)"
	}
	return "no ready endpoints"
}
