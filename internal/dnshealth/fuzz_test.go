package dnshealth

import (
	"math"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

// seedOverflowAcrossKeys builds a /metrics body that pushes Assess's total
// over math.MaxInt64 by summing three different rcode keys, not by doubling
// one. Each key is repeated enough times that its own per-key sum (~3.6e18)
// stays well under math.MaxInt64, so saturatingAdd never caps a single key —
// only the three-key total inside Assess does. Unlike two additions to the
// same key, which can cancel to exactly 0 in two's-complement arithmetic (as
// the seed this replaces did), three distinct positive sums that overflow
// wrap to a large negative number, which int64 arithmetic cannot hide.
func seedOverflowAcrossKeys() string {
	var b strings.Builder
	for _, rcode := range []string{"NOERROR", "SERVFAIL", "REFUSED"} {
		line := `coredns_dns_responses_total{rcode="` + rcode + `"} 9223372036854775807` + "\n"
		b.WriteString(strings.Repeat(line, 400))
	}
	return b.String()
}

// FuzzParseResponses fuzzes both halves of the CoreDNS metrics path: parsing a
// /metrics body, and judging the parsed counts. The second []byte drives the
// Assess parameters through a Cursor, so one target covers the pair without
// needing seven fuzz arguments.
func FuzzParseResponses(f *testing.F) {
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} 42`), []byte("seed"))
	f.Add([]byte(`coredns_dns_responses_total{rcode="SERVFAIL"} NaN`), []byte{})
	f.Add([]byte(`coredns_dns_responses_total{rcode="REFUSED"} +Inf`), []byte{1})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} -Inf`), []byte{2})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} 1e30`), []byte{3})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} -5`), []byte{4})
	f.Add([]byte(seedOverflowAcrossKeys()), []byte{5})
	f.Add([]byte(`coredns_dns_response_rcode_count_total{rcode="SERVFAIL"} 7`), []byte{6})
	f.Add([]byte{}, []byte{})

	f.Fuzz(func(t *testing.T, body, params []byte) {
		agg := ParseResponses(body)
		for rcode, n := range agg {
			if n < 0 {
				t.Errorf("ParseResponses: rcode %q has a negative count %d — a count cannot be negative", rcode, n)
			}
		}

		c := fuzzgen.New(params)
		rep := Assess(agg, c.IntN(8), c.IntN(8), c.IntN(4), c.IntN(4), float64(c.IntN(101))/100, int64(c.IntN(1000)))

		switch rep.Status {
		case "ok", "degraded", "forbidden", "unreachable", "":
		default:
			t.Errorf("Assess: status %q is outside the documented set", rep.Status)
		}
		if math.IsNaN(rep.ServfailRatio) || math.IsInf(rep.ServfailRatio, 0) {
			t.Errorf("Assess: ServfailRatio = %v", rep.ServfailRatio)
		}
		if rep.ServfailRatio < 0 || rep.ServfailRatio > 1 {
			t.Errorf("Assess: ServfailRatio = %v, outside [0,1]", rep.ServfailRatio)
		}
		if rep.ErrorResponses < 0 || rep.TotalResponses < 0 {
			t.Errorf("Assess: negative counts (errors=%d total=%d)", rep.ErrorResponses, rep.TotalResponses)
		}
		if rep.ErrorResponses > rep.TotalResponses {
			t.Errorf("Assess: errors %d exceed total %d", rep.ErrorResponses, rep.TotalResponses)
		}
		fuzzgen.AssertSafe(t, "report.detail", rep.Detail)
	})
}
