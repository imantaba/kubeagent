package dnshealth

import (
	"math"
	"reflect"
	"testing"
)

func TestParseResponses_CurrentMetric(t *testing.T) {
	body := []byte(`# HELP coredns_dns_responses_total Counter of responses.
# TYPE coredns_dns_responses_total counter
coredns_dns_responses_total{server="dns://:53",zone=".",view="",rcode="NOERROR"} 900
coredns_dns_responses_total{server="dns://:53",zone=".",view="",rcode="SERVFAIL"} 100
`)
	got := ParseResponses(body)
	want := map[string]int64{"NOERROR": 900, "SERVFAIL": 100}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseResponses_LegacyMetric(t *testing.T) {
	body := []byte(`coredns_dns_response_rcode_count_total{server="dns://:53",zone=".",rcode="REFUSED"} 5
`)
	got := ParseResponses(body)
	if got["REFUSED"] != 5 {
		t.Errorf("legacy metric: REFUSED = %d, want 5", got["REFUSED"])
	}
}

func TestParseResponses_SumsSeriesAndIgnoresJunk(t *testing.T) {
	body := []byte(`coredns_dns_responses_total{server="a",rcode="NOERROR"} 10
coredns_dns_responses_total{server="b",rcode="NOERROR"} 15

# a comment
coredns_build_info{version="1.11"} 1
coredns_dns_responses_total{server="a"} 99
not a metric line
`)
	got := ParseResponses(body)
	if got["NOERROR"] != 25 {
		t.Errorf("NOERROR = %d, want 25 (summed across series)", got["NOERROR"])
	}
	if len(got) != 1 {
		t.Errorf("want only NOERROR (no-rcode and unrelated lines ignored), got %v", got)
	}
}

func TestAssess_Degraded(t *testing.T) {
	agg := map[string]int64{"NOERROR": 9000, "SERVFAIL": 800, "REFUSED": 200}
	got := Assess(agg, 2, 0, 0, 0.05, 100)
	if got.Status != "degraded" {
		t.Fatalf("Status = %q, want degraded", got.Status)
	}
	if got.ErrorResponses != 1000 || got.TotalResponses != 10000 {
		t.Errorf("errors/total = %d/%d, want 1000/10000", got.ErrorResponses, got.TotalResponses)
	}
	if got.ServfailRatio < 0.099 || got.ServfailRatio > 0.101 {
		t.Errorf("ratio = %v, want ~0.10", got.ServfailRatio)
	}
	if got.PodsProbed != 2 {
		t.Errorf("PodsProbed = %d, want 2", got.PodsProbed)
	}
}

func TestAssess_UnderThreshold(t *testing.T) {
	agg := map[string]int64{"NOERROR": 9800, "SERVFAIL": 200} // 2%
	if got := Assess(agg, 1, 0, 0, 0.05, 100); got.Status != "ok" {
		t.Errorf("Status = %q, want ok", got.Status)
	}
}

func TestAssess_BelowFloorNotFlagged(t *testing.T) {
	agg := map[string]int64{"NOERROR": 30, "SERVFAIL": 20} // 40% but only 50 total
	if got := Assess(agg, 1, 0, 0, 0.05, 100); got.Status != "ok" {
		t.Errorf("Status = %q, want ok (below floor)", got.Status)
	}
}

func TestAssess_NoPods(t *testing.T) {
	if got := Assess(nil, 0, 0, 0, 0.05, 100); got.Status != "" {
		t.Errorf("Status = %q, want empty (no CoreDNS pods)", got.Status)
	}
}

func TestAssess_AllForbidden(t *testing.T) {
	if got := Assess(nil, 2, 2, 0, 0.05, 100); got.Status != "forbidden" {
		t.Errorf("Status = %q, want forbidden", got.Status)
	}
}

func TestAssess_AllUnreachable(t *testing.T) {
	if got := Assess(nil, 2, 0, 2, 0.05, 100); got.Status != "unreachable" {
		t.Errorf("Status = %q, want unreachable", got.Status)
	}
}

// Mixed total failure (one 403, one transport error, no metrics returned): the
// concrete "forbidden" reason takes priority over "unreachable" so the operator
// sees the actionable grant hint rather than a misleading "ok".
func TestAssess_MixedForbiddenAndUnreachable(t *testing.T) {
	if got := Assess(nil, 2, 1, 1, 0.05, 100); got.Status != "forbidden" {
		t.Errorf("Status = %q, want forbidden (mixed failure, forbidden priority)", got.Status)
	}
}

// TestAssess_HostileMaps calls Assess directly with maps no ParseResponses
// output could ever contain. Assess is exported and pure, so it must hold for
// any map a caller hands it — but FuzzParseResponses can never reach that
// guard, because there Assess only ever receives a map ParseResponses already
// sanitized. These cases exercise the guard the way a hostile direct caller
// would.
func TestAssess_HostileMaps(t *testing.T) {
	cases := []struct {
		name string
		agg  map[string]int64
	}{
		{
			name: "negative counts",
			agg:  map[string]int64{"NOERROR": -10, "SERVFAIL": -5, "REFUSED": -3},
		},
		{
			name: "math.MinInt64",
			agg:  map[string]int64{"NOERROR": math.MinInt64, "SERVFAIL": 100},
		},
		{
			name: "math.MaxInt64 in more than one key saturates rather than wraps",
			agg:  map[string]int64{"NOERROR": math.MaxInt64, "SERVFAIL": math.MaxInt64, "REFUSED": math.MaxInt64},
		},
		{
			// Moderate magnitudes, not math.MinInt64: a large-enough negative
			// term here would underflow past int64's minimum and wrap around to
			// a large *positive* total, which would coincidentally satisfy every
			// invariant below despite the guard being gone — the same trap
			// finding 5 flagged for a fuzz seed applies just as much here.
			name: "mix of negative and positive",
			agg:  map[string]int64{"NOERROR": 500, "SERVFAIL": -2000, "REFUSED": 50, "NXDOMAIN": -100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Assess(tc.agg, 1, 0, 0, 0.05, 0)

			switch rep.Status {
			case "ok", "degraded", "forbidden", "unreachable", "":
			default:
				t.Errorf("Status = %q, outside the documented set", rep.Status)
			}
			if rep.ErrorResponses < 0 || rep.TotalResponses < 0 {
				t.Errorf("negative counts: errors=%d total=%d", rep.ErrorResponses, rep.TotalResponses)
			}
			if rep.ErrorResponses > rep.TotalResponses {
				t.Errorf("errors %d exceed total %d", rep.ErrorResponses, rep.TotalResponses)
			}
			if math.IsNaN(rep.ServfailRatio) || math.IsInf(rep.ServfailRatio, 0) {
				t.Errorf("ServfailRatio = %v", rep.ServfailRatio)
			}
			if rep.ServfailRatio < 0 || rep.ServfailRatio > 1 {
				t.Errorf("ServfailRatio = %v, outside [0,1]", rep.ServfailRatio)
			}
		})
	}
}
