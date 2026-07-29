package rbacprofile

import (
	"context"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// allowAllExcept builds a fake clientset whose SelfSubjectAccessReview answers
// yes to everything except the named resources.
func allowAllExcept(denied ...string) *fake.Clientset {
	blocked := map[string]bool{}
	for _, d := range denied {
		blocked[d] = true
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		name := ""
		if ra := review.Spec.ResourceAttributes; ra != nil {
			name = ra.Resource
			if ra.Subresource != "" {
				name += "/" + ra.Subresource
			}
		} else if nra := review.Spec.NonResourceAttributes; nra != nil {
			name = nra.Path
		}
		review.Status = authv1.SubjectAccessReviewStatus{
			Allowed: !blocked[name],
			// A real API server fills Reason with the authorizer's own message,
			// which names the requesting identity. Setting it here proves Check
			// never reads it.
			Reason: "RBAC: user \"<PLACEHOLDER-IDENTITY>\" cannot list secrets",
		}
		return true, review, nil
	})
	return client
}

func TestCheckReportsAllowedFeature(t *testing.T) {
	f, _ := Lookup("certs")
	got, err := Check(context.Background(), allowAllExcept(), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Allowed || len(got[0].Missing) != 0 {
		t.Fatalf("certs reported as %+v, want allowed with nothing missing", got[0])
	}
}

func TestCheckNamesTheMissingActionInKubeagentsWords(t *testing.T) {
	f, _ := Lookup("certs")
	got, err := Check(context.Background(), allowAllExcept("secrets"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Allowed {
		t.Fatal("certs reported as allowed while secrets are denied")
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "list secrets" {
		t.Fatalf("Missing = %v, want [\"list secrets\"]", got[0].Missing)
	}
}

// The API server's reason embeds the requesting identity. It must never reach a
// FeatureStatus, whatever the authorizer chose to say.
func TestCheckNeverQuotesTheAPIServersReason(t *testing.T) {
	f, _ := Lookup("certs")
	got, _ := Check(context.Background(), allowAllExcept("secrets"), []Feature{f})
	for _, m := range got[0].Missing {
		if strings.Contains(m, "PLACEHOLDER-IDENTITY") || strings.Contains(m, "RBAC:") {
			t.Errorf("Missing entry %q carries the API server's own message", m)
		}
	}
}

func TestCheckSplitsSubresources(t *testing.T) {
	f, _ := Lookup("logs")
	got, err := Check(context.Background(), allowAllExcept("pods/log"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "get pods/log" {
		t.Fatalf("Missing = %v, want [\"get pods/log\"]", got[0].Missing)
	}
}

func TestCheckHandlesNonResourceURLs(t *testing.T) {
	f, _ := Lookup("controlplane")
	got, err := Check(context.Background(), allowAllExcept("/readyz"), []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Missing) != 1 || got[0].Missing[0] != "get /readyz" {
		t.Fatalf("Missing = %v, want [\"get /readyz\"]", got[0].Missing)
	}
}

// A feature that costs nothing beyond core must report clean without issuing a
// single access review.
func TestCheckSkipsFeaturesWithNoRules(t *testing.T) {
	f, _ := Lookup("capacity")
	client := allowAllExcept()
	got, err := Check(context.Background(), client, []Feature{f})
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Allowed {
		t.Fatal("capacity needs no grant but reported as blocked")
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "selfsubjectaccessreviews" {
			t.Fatal("Check issued an access review for a feature with no rules")
		}
	}
}

func TestActionStringQualifiesCustomResources(t *testing.T) {
	a := Action{Verb: "list", APIGroup: "cert-manager.io", Resource: "certificates"}
	if got := a.String(); got != "list certificates.cert-manager.io" {
		t.Errorf("Action.String() = %q", got)
	}
}
