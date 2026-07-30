package dnshealth

import (
	"math"
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

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
	f.Add([]byte("coredns_dns_responses_total{rcode=\"NOERROR\"} 9223372036854775807\ncoredns_dns_responses_total{rcode=\"NOERROR\"} 9223372036854775807"), []byte{5})
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
		rep := Assess(agg, c.IntN(8), c.IntN(4), c.IntN(4), float64(c.IntN(101))/100, int64(c.IntN(1000)))

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
