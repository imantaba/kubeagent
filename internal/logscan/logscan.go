// Package logscan classifies a crashed container's log tail into a plain-language
// root cause. Pure and read-only: the caller supplies the log text.
package logscan

import (
	"regexp"
	"strings"
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
	{"conn-refused", regexp.MustCompile(`(?i)dial tcp (\S+): connect: connection refused`), func(m []string) string { return "cannot reach a dependency (" + m[1] + ") — connection refused" }},
	{"dns", regexp.MustCompile(`(?i)no such host|server misbehaving`), func([]string) string { return "DNS resolution failed (name lookup)" }},
	{"oom-inproc", regexp.MustCompile(`(?i)out of memory|cannot allocate memory|std::bad_alloc`), func([]string) string { return "ran out of memory in-process" }},
	{"config", regexp.MustCompile(`(?i)^yaml:|invalid character .* looking for|failed to parse|invalid config`), func([]string) string { return "configuration parse/validation error" }},
	{"addr-in-use", regexp.MustCompile(`(?i)bind: address already in use`), func([]string) string { return "port already in use" }},
	{"auth", regexp.MustCompile(`(?i)password authentication failed|access denied|401 unauthorized|403 forbidden`), func([]string) string { return "authentication/authorization failure to a dependency" }},
	{"perm-denied", regexp.MustCompile(`(?i)permission denied|eacces`), func([]string) string { return "permission denied — check securityContext / file permissions" }},
}

const maxExcerpt = 200

// Classify scans the log's non-empty lines against the signature library (in order) and
// returns the first matching line's clue; if none match it falls back to the last
// non-empty line. An empty/whitespace log returns the zero Clue.
func Classify(log string) Clue {
	lines := strings.Split(log, "\n")
	for _, s := range signatures {
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			if m := s.re.FindStringSubmatch(ln); m != nil {
				return Clue{Signature: s.name, Excerpt: truncate(ln), Cause: s.cause(m)}
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			return Clue{Excerpt: truncate(ln), Cause: "last output before exit (no known signature)"}
		}
	}
	return Clue{}
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxExcerpt {
		return string(r[:maxExcerpt]) + "…"
	}
	return s
}
