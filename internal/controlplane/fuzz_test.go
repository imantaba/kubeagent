package controlplane

import (
	"fmt"
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// FuzzParseReadyz fuzzes the apiserver /readyz classifier. The failing-check
// names are tokens lifted straight out of the response body, which no schema
// constrains, and the list had no count bound at all.
func FuzzParseReadyz(f *testing.F) {
	f.Add(200, []byte("[+]etcd ok\nreadyz check passed"))
	f.Add(500, []byte("[-]etcd failed: reason withheld\n[-]poststarthook/start-apiserver failed"))
	f.Add(403, []byte{})
	f.Add(0, []byte{})
	f.Add(503, []byte("[-]\x1b]0;pwned\x07 failed"))
	f.Add(503, []byte("[-]\xff\xfe failed"))
	f.Add(503, append([]byte("[-]"), append([]byte{0xe2, 0x80, 0xae}, []byte("etcd failed")...)...)) // "[-]" + U+202E RTL override + "etcd failed"
	f.Add(503, []byte("[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x"))

	f.Fuzz(func(t *testing.T, code int, body []byte) {
		p := ParseReadyz(code, body)

		switch p.Status {
		case "ok", "unhealthy", "forbidden", "unreachable":
		default:
			t.Errorf("ParseReadyz(%d): status %q is outside the documented set", code, p.Status)
		}
		// Deliberately a literal, not maxFailedChecks: a test that reads the
		// constant under test only proves the code agrees with itself, and the
		// point of this bound is that a /readyz body cannot make kubeagent print
		// an unbounded list, however large maxFailedChecks itself is set.
		if len(p.Failed) > 20 {
			t.Errorf("ParseReadyz(%d): %d failing checks exceeds the 20-entry cap", code, len(p.Failed))
		}
		for i, name := range p.Failed {
			where := fmt.Sprintf("failed[%d]", i)
			fuzzgen.AssertSafe(t, where, name)
			fuzzgen.AssertBounded(t, where, name, safetext.MaxLine)
		}

		if again := ParseReadyz(code, body); again.Status != p.Status || len(again.Failed) != len(p.Failed) {
			t.Errorf("ParseReadyz is not deterministic")
		}
	})
}
