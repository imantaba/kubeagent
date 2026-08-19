// Package logscan classifies a crashed container's log tail into a plain-language
// root cause. Pure and read-only: the caller supplies the log text.
package logscan

import (
	"regexp"
	"strings"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// Clue is the classified root cause from a container's crash logs.
type Clue struct {
	Signature string `json:"signature"` // matched signature name, "" if fallback
	Excerpt   string `json:"excerpt"`   // the single relevant log line, trimmed/truncated
	Cause     string `json:"cause"`     // plain-language cause
}

type signature struct {
	name  string
	re    *regexp.Regexp
	cause func(m []string) string // builds the cause from the matched line's submatches
}

// signatures are checked in this order; the first signature with any matching line wins.
// The specific, exec:-anchored "entrypoint" is before the generic "perm-denied" so a
// container-start "exec: … permission denied" classifies as a bad entrypoint while a bare
// runtime "permission denied" falls through to perm-denied. "panic" is first so a panic
// body containing "no such file" isn't mis-matched.
var signatures = []signature{
	{"panic", regexp.MustCompile(`(?i)^panic:|goroutine \d+ \[running\]:`), func([]string) string { return "application panic (code bug)" }},
	{"entrypoint", regexp.MustCompile(`(?i)exec:.*(?:executable file not found|no such file or directory|permission denied)`), func([]string) string { return "bad command or entrypoint" }},
	// No submatch reaches the returned cause. report.go renders LogCause
	// unredacted, so interpolating the dialed address here would forward
	// whatever the container printed — including a credential embedded in a
	// URL — off the machine. The fixed string returned below is the only
	// guarantee this file makes. The raw line still reaches Clue.Excerpt,
	// sanitized and truncated, and internal/scan runs redact.Addresses over
	// that — but this is a partial defence, not a second guarantee.
	// redact.Addresses catches an IPv4 address, a bracketed IPv6 address
	// with a port, and a dotted hostname with a port; its hostname
	// alternative requires a dot, so a single-label service host — "dial tcp
	// redis:6379", the ordinary same-namespace shape — survives into
	// Finding.LogExcerpt and ships in scan --output json.
	{"conn-refused", regexp.MustCompile(`(?i)dial tcp \S+: connect: connection refused`), func([]string) string {
		return "cannot reach a dependency — connection refused"
	}},
	{"dns", regexp.MustCompile(`(?i)no such host|server misbehaving`), func([]string) string { return "DNS resolution failed (name lookup)" }},
	{"oom-inproc", regexp.MustCompile(`(?i)out of memory|cannot allocate memory|std::bad_alloc`), func([]string) string { return "ran out of memory in-process" }},
	{"config", regexp.MustCompile(`(?i)^yaml:|invalid character .* looking for|failed to parse|invalid config`), func([]string) string { return "configuration parse/validation error" }},
	{"addr-in-use", regexp.MustCompile(`(?i)bind: address already in use`), func([]string) string { return "port already in use" }},
	{"auth", regexp.MustCompile(`(?i)password authentication failed|access denied|401 unauthorized|403 forbidden`), func([]string) string { return "authentication/authorization failure to a dependency" }},
	{"perm-denied", regexp.MustCompile(`(?i)permission denied|eacces`), func([]string) string { return "permission denied — check securityContext / file permissions" }},
}

const maxExcerpt = 200

// containerRuntimePlaceholder matches the kubelet's own stand-in for "there is
// no log to show" (e.g. "unable to retrieve container logs for
// containerd://<id>: rpc error: ..."). It is control-plane text, never
// something the crashed container itself wrote to stdout/stderr, so it must
// not be classified as if it were.
var containerRuntimePlaceholder = regexp.MustCompile(`(?i)^unable to retrieve container logs for `)

// isOnlyPlaceholder reports whether every non-empty line is the kubelet's
// placeholder. A body that mixes the placeholder with genuine output (a real
// panic line, say) still classifies normally — see Classify.
func isOnlyPlaceholder(lines []string) bool {
	found := false
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if !containerRuntimePlaceholder.MatchString(ln) {
			return false
		}
		found = true
	}
	return found
}

// Classify scans the log's non-empty lines against the signature library (in order) and
// returns the first matching line's clue; if none match it falls back to the last
// non-empty line. An empty/whitespace log returns the zero Clue. A log whose only
// non-empty content is the container-runtime placeholder also returns the zero
// Clue: there is nothing here to classify.
func Classify(log string) Clue {
	lines := strings.Split(log, "\n")
	if isOnlyPlaceholder(lines) {
		return Clue{}
	}
	for _, s := range signatures {
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			if m := s.re.FindStringSubmatch(ln); m != nil {
				return Clue{Signature: s.name, Excerpt: sanitize(ln), Cause: s.cause(m)}
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			// "25" names internal/collect.PreviousLogs's TailLines: that is the
			// only window this function ever sees, so a signature earlier in the
			// container's own output is invisible here, not absent. Keep the two
			// in sync if either changes.
			return Clue{Excerpt: sanitize(ln), Cause: "last output before exit (no signature in the last 25 lines)"}
		}
	}
	return Clue{}
}

// sanitize makes one log line fit to print and bounds it to maxExcerpt runes.
// safetext.Line runs FIRST: sanitizing after truncation would spend the excerpt
// budget on characters about to be dropped, and would leave a control character
// inside the kept prefix untouched.
func sanitize(s string) string {
	s = safetext.Line(s)
	if r := []rune(s); len(r) > maxExcerpt {
		return string(r[:maxExcerpt]) + "…"
	}
	return s
}
