package logscan

import (
	"fmt"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct{ name, log, wantSig, wantCause string }{
		{"panic", "starting up\npanic: runtime error: invalid memory address", "panic", "application panic (code bug)"},
		{"entrypoint", `exec: "server": executable file not found in $PATH`, "entrypoint", "bad command or entrypoint"},
		{"conn-refused", "dial tcp 10.96.0.10:5432: connect: connection refused", "conn-refused", "cannot reach a dependency — connection refused"},
		{"dns", "lookup db on 10.96.0.10:53: no such host", "dns", "DNS resolution failed (name lookup)"},
		{"oom", "fatal error: out of memory", "oom-inproc", "ran out of memory in-process"},
		{"config", "yaml: line 3: mapping values are not allowed", "config", "configuration parse/validation error"},
		{"addr-in-use", "listen tcp :8080: bind: address already in use", "addr-in-use", "port already in use"},
		{"auth", `FATAL: password authentication failed for user "app"`, "auth", "authentication/authorization failure to a dependency"},
		{"perm-denied", "open /data/config: permission denied", "perm-denied", "permission denied — check securityContext / file permissions"},
		{"fallback", "just some log\nexited with code 3", "", "last output before exit (no signature in the last 25 lines)"},
	}
	for _, c := range cases {
		got := Classify(c.log)
		if got.Signature != c.wantSig || got.Cause != c.wantCause {
			t.Errorf("%s: Classify()=%+v, want sig=%q cause=%q", c.name, got, c.wantSig, c.wantCause)
		}
	}
	if got := Classify("   \n\n"); got != (Clue{}) {
		t.Errorf("empty log: want zero Clue, got %+v", got)
	}
	if got := Classify("just some log\nexited with code 3"); got.Excerpt != "exited with code 3" {
		t.Errorf("fallback excerpt = %q, want the last non-empty line", got.Excerpt)
	}
}

// TestClassify_ConnRefusedNeverEmitsContainerText pins R218: the conn-refused
// cause is the same fixed constant no matter what the container's own output
// looked like — no submatch reaches Cause. The raw line, including whatever
// it contains, still reaches Excerpt (sanitized, capped at 200 runes), which
// is where the operator sees it — report.go renders it directly above the
// cause, and it does not disappear, it just leaves the one field that is
// forwarded off the machine unredacted.
func TestClassify_ConnRefusedNeverEmitsContainerText(t *testing.T) {
	const want = "cannot reach a dependency — connection refused"
	cases := []struct{ name, log string }{
		{"credentialed URL", "dial tcp postgres://appuser:hunter2@db.example.com:5432/orders: connect: connection refused"},
		{"arbitrary text", "dial tcp NOT-AN-ADDRESS: connect: connection refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.log)
			if got.Cause != want {
				t.Errorf("Cause = %q, want %q", got.Cause, want)
			}
			for _, secret := range []string{"appuser", "hunter2", "db.example.com"} {
				if strings.Contains(got.Cause, secret) {
					t.Errorf("Cause = %q, must not contain %q", got.Cause, secret)
				}
			}
		})
	}
	if got := Classify(cases[0].log); got.Excerpt != cases[0].log {
		t.Errorf("Excerpt = %q, want the whole raw line %q", got.Excerpt, cases[0].log)
	}
}

// TestClassify_RefusesContainerRuntimePlaceholder pins R187: a body whose
// only non-empty content is the kubelet's own log-unavailable placeholder is
// refused (zero Clue) rather than classified — matching a signature against
// it would report the container runtime's own message as if it were the
// crashed container's behaviour.
func TestClassify_RefusesContainerRuntimePlaceholder(t *testing.T) {
	const placeholder = "unable to retrieve container logs for containerd://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	if got := Classify(placeholder); got != (Clue{}) {
		t.Errorf("placeholder alone: want zero Clue, got %+v", got)
	}

	mixed := "panic: runtime error: invalid memory address\n" + placeholder
	if got := Classify(mixed); got.Signature != "panic" || got.Cause != "application panic (code bug)" {
		t.Errorf("placeholder beside real output: want the panic cause, got %+v — the refusal must not swallow a real answer", got)
	}

	midline := "container runtime says: unable to retrieve container logs for containerd://xyz, retrying"
	if got := Classify(midline); got.Signature != "" || got.Cause != "last output before exit (no signature in the last 25 lines)" {
		t.Errorf("mid-line mention (unanchored): want normal fallback classification, got %+v", got)
	}
}

// TestClassify_FallbackMissesASignatureOutsideTheTailWindow pins R186: kubeagent
// asks the API server for only the last 25 lines (internal/collect.PreviousLogs's
// TailLines), so a signature earlier than that window is never in the text
// Classify sees. This fixture simulates that boundary: of 30 original lines,
// only line 1 carries panic:, and the text Classify receives is just the last
// 25 (lines 6-30) — the same window PreviousLogs requests — so the panic line
// is already gone by the time Classify runs, and the fallback names the window
// it actually searched instead of claiming no signature exists anywhere.
func TestClassify_FallbackMissesASignatureOutsideTheTailWindow(t *testing.T) {
	all := make([]string, 0, 30)
	all = append(all, "panic: runtime error: invalid memory address")
	for i := 2; i <= 30; i++ {
		all = append(all, fmt.Sprintf("line %d: ordinary output", i))
	}
	tail := all[len(all)-25:] // the last 25 lines, matching PreviousLogs's TailLines
	log := strings.Join(tail, "\n")

	got := Classify(log)
	if got.Signature != "" || got.Cause != "last output before exit (no signature in the last 25 lines)" {
		t.Errorf("Classify() = %+v, want the fallback cause naming the 25-line window", got)
	}
	if want := "line 30: ordinary output"; got.Excerpt != want {
		t.Errorf("Excerpt = %q, want %q", got.Excerpt, want)
	}
}
