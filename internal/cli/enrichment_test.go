package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/investigate"
)

// TestEnrichmentFailure pins the three error shapes the model-enrichment path
// (--explain or --investigate) can fail with, and what enrichmentFailure
// reduces each to for stderr. See R227: an *anthropic.Error's own Error()
// method embeds the full request URL (path and query) plus the upstream
// response body; a *url.Error a failed call to a local
// KUBEAGENT_EXPLAIN_ENDPOINT wraps embeds the full URL the same way; anything
// else is text kubeagent itself authored and is left alone.
func TestEnrichmentFailure(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    []string // substrings that must appear
		mustNot []string // substrings that must NOT appear
	}{
		{
			name: "anthropic API error reduces to status and scheme://host",
			err: &anthropic.Error{
				StatusCode: 500,
				Request: &http.Request{
					Method: "POST",
					URL:    &url.URL{Scheme: "https", Host: "api.anthropic.com", Path: "/v1/messages", RawQuery: "key=secret-token"},
				},
				Response: &http.Response{StatusCode: 500},
			},
			want:    []string{"500", "https://api.anthropic.com"},
			mustNot: []string{"/v1/messages", "key=secret-token"},
		},
		{
			// apierror.Error.Error() dereferences Request and Request.URL
			// unguarded and panics on nil; enrichmentFailure must not.
			name: "anthropic API error with a nil Request does not panic and still reduces",
			err:  &anthropic.Error{StatusCode: 503},
			want: []string{"503"},
		},
		{
			name: "url.Error (the local-endpoint wrap shape) reduces via redact.Error: operation and scheme://host survive, path does not",
			err: fmt.Errorf("calling local explain endpoint: %w", &url.Error{
				Op:  "Post",
				URL: "http://127.0.0.1:1/v1/chat/completions",
				Err: errors.New("connection refused"),
			}),
			want:    []string{"Post", "http://127.0.0.1:1", "connection refused"},
			mustNot: []string{"/v1/chat/completions"},
		},
		{
			name: "a kubeagent-authored error is left unchanged",
			err:  errors.New("investigating: model returned no text"),
			want: []string{"investigating: model returned no text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := enrichmentFailure(tt.err)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("enrichmentFailure(...) = %q, want it to contain %q", got, w)
				}
			}
			for _, m := range tt.mustNot {
				if strings.Contains(got, m) {
					t.Errorf("enrichmentFailure(...) = %q, want it NOT to contain %q", got, m)
				}
			}
		})
	}
}

func TestEnrichmentFailureNilError(t *testing.T) {
	if got := enrichmentFailure(nil); got != "" {
		t.Errorf("enrichmentFailure(nil) = %q, want empty", got)
	}
}

// TestRunModelPath drives the extracted model-enrichment arm directly, with
// injected fakes, so R223(A)'s two promises are provable without a network:
// a failure never surfaces as an error (the deterministic report must still
// render), and --investigate supersedes --explain (only one arm ever runs).
func TestRunModelPath(t *testing.T) {
	t.Run("neither flag set: no call is made", func(t *testing.T) {
		called := false
		res := runModelPath(scanOptions{},
			func() (investigate.Report, error) { called = true; return investigate.Report{}, nil },
			func() (string, error) { called = true; return "", nil },
		)
		if called {
			t.Error("neither --explain nor --investigate set: no call should be made")
		}
		if res.notice != "" {
			t.Errorf("notice = %q, want empty", res.notice)
		}
	})

	t.Run("investigate supersedes explain: only investigateFn runs", func(t *testing.T) {
		investigateCalled, explainCalled := false, false
		want := investigate.Report{Narrative: "the pod is crashlooping", Consulted: []string{"describe pod shop/web"}}
		res := runModelPath(scanOptions{investigate: true, explain: true},
			func() (investigate.Report, error) { investigateCalled = true; return want, nil },
			func() (string, error) { explainCalled = true; return "should not run", nil },
		)
		if !investigateCalled || explainCalled {
			t.Errorf("investigateCalled=%v explainCalled=%v, want investigate only", investigateCalled, explainCalled)
		}
		if res.investigation.Narrative != want.Narrative {
			t.Errorf("investigation.Narrative = %q, want %q", res.investigation.Narrative, want.Narrative)
		}
		if res.notice != "" {
			t.Errorf("notice = %q, want empty on success", res.notice)
		}
	})

	t.Run("investigate failure produces a notice naming --investigate instead of an error", func(t *testing.T) {
		res := runModelPath(scanOptions{investigate: true},
			func() (investigate.Report, error) {
				return investigate.Report{}, errors.New("investigating: model returned no text")
			},
			func() (string, error) { return "", nil },
		)
		if !strings.Contains(res.notice, "--investigate") {
			t.Errorf("notice = %q, want it to name --investigate", res.notice)
		}
		if !strings.Contains(res.notice, "model returned no text") {
			t.Errorf("notice = %q, want the underlying reason", res.notice)
		}
		if res.investigation.Narrative != "" {
			t.Errorf("investigation.Narrative = %q, want empty on failure", res.investigation.Narrative)
		}
	})

	t.Run("explain failure produces a notice naming --explain instead of an error", func(t *testing.T) {
		res := runModelPath(scanOptions{explain: true},
			func() (investigate.Report, error) { return investigate.Report{}, nil },
			func() (string, error) { return "", errors.New("boom") },
		)
		if !strings.Contains(res.notice, "--explain") {
			t.Errorf("notice = %q, want it to name --explain", res.notice)
		}
		if !strings.Contains(res.notice, "boom") {
			t.Errorf("notice = %q, want the underlying reason", res.notice)
		}
		if res.explanation != "" {
			t.Errorf("explanation = %q, want empty on failure", res.explanation)
		}
	})

	t.Run("explain success populates explanation with no notice", func(t *testing.T) {
		res := runModelPath(scanOptions{explain: true},
			func() (investigate.Report, error) { return investigate.Report{}, nil },
			func() (string, error) { return "summary text", nil },
		)
		if res.explanation != "summary text" {
			t.Errorf("explanation = %q, want %q", res.explanation, "summary text")
		}
		if res.notice != "" {
			t.Errorf("notice = %q, want empty", res.notice)
		}
	})
}
