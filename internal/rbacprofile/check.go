package rbacprofile

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/imantaba/kubeagent/internal/redact"
)

// Action is one thing kubeagent needs to be allowed to do: a single verb on a
// single resource, subresource or non-resource URL. A Rule expands into one
// Action per resource per verb, because that is the granularity the API server
// answers at.
type Action struct {
	Verb           string
	APIGroup       string
	Resource       string // "pods"
	Subresource    string // "log"; empty for a whole resource
	NonResourceURL string // "/readyz"; set instead of the three fields above
}

// String is how kubeagent names a missing permission: built from the table, in
// kubeagent's own words. The API server's explanation is never used, because it
// embeds the requesting identity.
func (a Action) String() string {
	if a.NonResourceURL != "" {
		return a.Verb + " " + a.NonResourceURL
	}
	name := a.Resource
	if a.Subresource != "" {
		name += "/" + a.Subresource
	}
	if a.APIGroup != "" {
		name += "." + a.APIGroup
	}
	return a.Verb + " " + name
}

// Actions expands a rule into the individual access reviews it implies.
func (r Rule) Actions() []Action {
	var out []Action
	for _, verb := range r.Verbs {
		for _, url := range r.NonResourceURLs {
			out = append(out, Action{Verb: verb, NonResourceURL: url})
		}
		for _, res := range r.Resources {
			resource, subresource, _ := strings.Cut(res, "/")
			out = append(out, Action{Verb: verb, APIGroup: r.APIGroup, Resource: resource, Subresource: subresource})
		}
	}
	return out
}

// FeatureStatus is the result of checking one feature against a live identity.
type FeatureStatus struct {
	Name    string `json:"name"`
	Flag    string `json:"flag,omitempty"`
	Summary string `json:"summary"`
	Allowed bool   `json:"allowed"`
	// Missing lists the actions the identity may not perform, phrased by
	// kubeagent from its own table. Never the API server's words.
	Missing []string `json:"missing,omitempty"`
}

// CheckDocument is the --output json shape of `rbac check`, wrapping what was a
// bare array so the output can declare its version.
type CheckDocument struct {
	SchemaVersion string          `json:"schemaVersion"`
	Features      []FeatureStatus `json:"features"`
}

// Check asks the API server whether the current identity may perform every
// action the named features need.
//
// It creates SelfSubjectAccessReview objects. That is a POST, but a virtual
// one: the API server evaluates the request and persists nothing, which is the
// same API `kubectl auth can-i` uses. Nothing in the cluster changes, and no
// extra grant is needed to ask — system:basic-user, bound to
// system:authenticated, already allows it.
func Check(ctx context.Context, client kubernetes.Interface, features []Feature) ([]FeatureStatus, error) {
	out := make([]FeatureStatus, 0, len(features))
	for _, f := range features {
		status := FeatureStatus{Name: f.Name, Flag: f.Flag, Summary: f.Summary, Allowed: true}
		for _, r := range MergeRules(f.Rules) {
			for _, a := range r.Actions() {
				ok, err := allowed(ctx, client, a)
				if err != nil {
					// redact.Error only special-cases *url.Error (see
					// internal/redact); a cluster that denies the access-review
					// request itself typically returns a *apierrors.StatusError,
					// whose message can embed the requesting identity, and that
					// passes through unredacted here. Accepted on this one path
					// only, because of where the returned error can go: Check's
					// caller (kubeagent rbac check) surfaces a plain error
					// straight to the operator's own stderr and nowhere else.
					// It must never cross into a FeatureStatus, the --output
					// json results, or any other value this package hands back
					// — those are built solely from Action.String, never from a
					// server-supplied message (see the comment on
					// res.Status.Reason below).
					return nil, fmt.Errorf("could not check %q: %s", a, redact.Error(err))
				}
				if !ok {
					status.Allowed = false
					status.Missing = append(status.Missing, a.String())
				}
			}
		}
		out = append(out, status)
	}
	return out, nil
}

func allowed(ctx context.Context, client kubernetes.Interface, a Action) (bool, error) {
	review := &authv1.SelfSubjectAccessReview{}
	if a.NonResourceURL != "" {
		review.Spec.NonResourceAttributes = &authv1.NonResourceAttributes{Path: a.NonResourceURL, Verb: a.Verb}
	} else {
		review.Spec.ResourceAttributes = &authv1.ResourceAttributes{
			Group:       a.APIGroup,
			Resource:    a.Resource,
			Subresource: a.Subresource,
			Verb:        a.Verb,
		}
	}
	res, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	// res.Status.Reason is deliberately not read. It carries the authorizer's
	// own message, which names the requesting identity — an IAM ARN, an OIDC
	// email, an internal DNS name — and under webhook authorization carries
	// third-party free text. kubeagent says why in its own words instead.
	return res.Status.Allowed, nil
}
