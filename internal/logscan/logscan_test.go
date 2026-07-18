package logscan

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct{ name, log, wantSig, wantCause string }{
		{"panic", "starting up\npanic: runtime error: invalid memory address", "panic", "application panic (code bug)"},
		{"entrypoint", `exec: "server": executable file not found in $PATH`, "entrypoint", "bad command or entrypoint"},
		{"conn-refused", "dial tcp 10.96.0.10:5432: connect: connection refused", "conn-refused", "cannot reach a dependency (10.96.0.10:5432) — connection refused"},
		{"dns", "lookup db on 10.96.0.10:53: no such host", "dns", "DNS resolution failed (name lookup)"},
		{"oom", "fatal error: out of memory", "oom-inproc", "ran out of memory in-process"},
		{"config", "yaml: line 3: mapping values are not allowed", "config", "configuration parse/validation error"},
		{"addr-in-use", "listen tcp :8080: bind: address already in use", "addr-in-use", "port already in use"},
		{"auth", `FATAL: password authentication failed for user "app"`, "auth", "authentication/authorization failure to a dependency"},
		{"perm-denied", "open /data/config: permission denied", "perm-denied", "permission denied — check securityContext / file permissions"},
		{"fallback", "just some log\nexited with code 3", "", "last output before exit (no known signature)"},
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
