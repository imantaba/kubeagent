package logscan

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

// FuzzClassify fuzzes the one input an unprivileged attacker controls outright:
// the tail of a crashed container's own log. Both Clue fields that carry log
// text must come back printable and bounded, and classification must be
// deterministic.
func FuzzClassify(f *testing.F) {
	f.Add("")
	f.Add("panic: runtime error: invalid memory address or nil pointer dereference")
	f.Add("dial tcp 192.0.2.10:5432: connect: connection refused")
	f.Add("exec: \"/app/server\": permission denied")
	f.Add("\x1b[2J\x1b[H")
	f.Add("dial tcp \x1b]0;pwned\x07: connect: connection refused")
	f.Add("dial tcp \xff\xfe: connect: connection refused")
	f.Add("yaml: line 3: found character that cannot start any token\n\u202e")
	f.Add("\n\n   \n")

	f.Fuzz(func(t *testing.T, log string) {
		clue := Classify(log)

		fuzzgen.AssertSafe(t, "clue.signature", clue.Signature)
		fuzzgen.AssertSafe(t, "clue.excerpt", clue.Excerpt)
		fuzzgen.AssertSafe(t, "clue.cause", clue.Cause)

		// maxExcerpt + 1: the ellipsis truncate appends when it cuts.
		fuzzgen.AssertBounded(t, "clue.excerpt", clue.Excerpt, maxExcerpt+1)
		// The cause is a fixed sentence plus, in the conn-refused case, one
		// sanitized capture from the log.
		fuzzgen.AssertBounded(t, "clue.cause", clue.Cause, 1024)

		if again := Classify(log); again != clue {
			t.Errorf("Classify is not deterministic:\nfirst:  %+v\nsecond: %+v", clue, again)
		}
	})
}
