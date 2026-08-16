package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/imantaba/kubeagent/internal/safetext"
	"github.com/imantaba/kubeagent/internal/scan"
)

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// splitCSV splits a comma-separated list into a slice, returning nil for empty.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// envOr returns the env var value if set, else def.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDur parses a duration env var, falling back to def on empty/invalid.
func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// envBool parses a boolean env var, falling back to def on empty/invalid.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// envFloat parses a float env var, falling back to def on empty/invalid.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// quotaThresholdFromEnv reads KUBEAGENT_QUOTA_THRESHOLD as a fraction in
// (0, 1], falling back to scan.DefaultQuotaThreshold.
//
// Unlike envFloat, it does not fall back silently. "80" is a plausible thing
// to write for "warn me at 80%", parses as a float, and is not a fraction —
// so the scan ran at the default while the operator believed they had changed
// the threshold. One line on w says what was ignored and what is being used
// instead. A set-but-empty value is left alone: os.Getenv cannot tell it from
// unset, and warning on it would fire for an ordinary trailing
// "KUBEAGENT_QUOTA_THRESHOLD=" in a shell profile.
func quotaThresholdFromEnv(w io.Writer) float64 {
	v := os.Getenv("KUBEAGENT_QUOTA_THRESHOLD")
	if v == "" {
		return scan.DefaultQuotaThreshold
	}
	f, err := strconv.ParseFloat(v, 64)
	if err == nil && f > 0 && f <= 1 {
		return f
	}
	warnf(w, "KUBEAGENT_QUOTA_THRESHOLD=%q is not a fraction in (0, 1]; using %.2f", v, scan.DefaultQuotaThreshold)
	return scan.DefaultQuotaThreshold
}

// envInt returns the env var parsed as an int, else def.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envDuration returns the env var parsed as a Go duration ("30m", "2h"), else def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// validateExpectedNodes rejects any declared expected-node name that is not
// a valid DNS-1123 subdomain (lowercase letters, digits, '-' and '.') — the
// only shape a real Kubernetes node name can have. src names where the value
// came from (a flag or an env var) so the error tells the operator which one
// to fix. The offending value is sanitized through safetext.Line before it
// is quoted into the message: an untrusted declared name must never carry a
// control character into the operator's terminal.
func validateExpectedNodes(names []string, src string) error {
	for _, n := range names {
		if len(validation.IsDNS1123Subdomain(n)) > 0 {
			return fmt.Errorf("%s: %q is not a valid node name (a node name is a DNS-1123 subdomain: lowercase letters, digits, '-' and '.')", src, safetext.Line(n))
		}
	}
	return nil
}
