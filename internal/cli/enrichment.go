package cli

import (
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/imantaba/kubeagent/internal/investigate"
	"github.com/imantaba/kubeagent/internal/redact"
)

// enrichmentFailure reduces an error from the model-enrichment path (--explain
// or --investigate) to one line safe for stderr.
//
// An *anthropic.Error's own Error() method (apierror.go) formats
// `%s %q: %d %s` on the request's method and full URL — path and query
// included — followed by the upstream response body, which is exactly the
// leak this guards against, so that shape is reduced here rather than by
// calling Error(). Every other shape, including the *url.Error a failed call
// to a local KUBEAGENT_EXPLAIN_ENDPOINT wraps, is redact.Error's job already:
// internal/redact's package doc says nothing outside it should format a URL
// or a URL-carrying error, and redact.Error already keeps the operation and
// recurses into the wrapped cause rather than discarding it. Request and
// Request.URL are dereferenced unguarded inside apierror.Error.Error(), so
// both are nil-checked here too rather than assumed set.
func enrichmentFailure(err error) string {
	if err == nil {
		return ""
	}
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		if ae.Request != nil && ae.Request.URL != nil {
			return fmt.Sprintf("api error %d from %s", ae.StatusCode, redact.URL(ae.Request.URL.String()))
		}
		return fmt.Sprintf("api error %d", ae.StatusCode)
	}
	return redact.Error(err)
}

// modelPathResult is what running the model-enrichment path produces: either
// the state a successful arm feeds into the report (explanation,
// investigation), or, on failure, a notice for stderr. An enrichment failure
// is never fatal to the scan (R223), so this carries no error.
type modelPathResult struct {
	explanation   string
	investigation investigate.Report
	notice        string
}

// runModelPath runs whichever model-enrichment arm is selected —
// --investigate supersedes --explain, matching the flag's own description —
// and reduces a failure to one notice line instead of aborting the run.
// investigateFn and explainFn are the actual calls, injected so this is
// testable with no network and no cluster.
func runModelPath(o scanOptions, investigateFn func() (investigate.Report, error), explainFn func() (string, error)) modelPathResult {
	switch {
	case o.investigate:
		rep, err := investigateFn()
		if err != nil {
			return modelPathResult{notice: fmt.Sprintf("--investigate: %s", enrichmentFailure(err))}
		}
		return modelPathResult{investigation: rep}
	case o.explain:
		text, err := explainFn()
		if err != nil {
			return modelPathResult{notice: fmt.Sprintf("--explain: %s", enrichmentFailure(err))}
		}
		return modelPathResult{explanation: text}
	}
	return modelPathResult{}
}
