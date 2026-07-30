// Package dnshealth turns CoreDNS /metrics response counters into an advisory
// resolution-health report: it flags an elevated SERVFAIL+REFUSED response ratio
// (DNS up but failing to resolve). Pure and read-only: the caller (scan) probes
// the CoreDNS pods and passes the parsed counts here.
package dnshealth

import (
	"math"
	"strconv"
	"strings"
)

// Report is the advisory CoreDNS resolution-health result.
type Report struct {
	Status         string  `json:"status"`         // "ok" | "degraded" | "forbidden" | "unreachable" | ""
	ServfailRatio  float64 `json:"servfailRatio"`  // (SERVFAIL+REFUSED)/total
	ErrorResponses int64   `json:"errorResponses"` // SERVFAIL + REFUSED
	TotalResponses int64   `json:"totalResponses"`
	PodsProbed     int     `json:"podsProbed"`
	Detail         string  `json:"detail,omitempty"`
}

// maxCount bounds one parsed sample. Prometheus counters are float64; a value
// beyond 2^53 is either a broken exporter or a hostile one, and clamping keeps
// the int64 conversion defined — converting a float outside int64's range is
// implementation-defined in Go and yields math.MinInt64 on amd64, turning a huge
// count into a negative one.
const maxCount = int64(1) << 53

// saturatingAdd returns a+b clamped to math.MaxInt64 rather than wrapping
// negative. Both arguments are non-negative at every call site.
func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// ParseResponses sums CoreDNS DNS response counts by rcode from one pod's /metrics
// body. It reads both the current metric name (coredns_dns_responses_total) and the
// pre-1.7 name (coredns_dns_response_rcode_count_total). Returns rcode → count.
func ParseResponses(body []byte) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "coredns_dns_responses_total{") &&
			!strings.HasPrefix(line, "coredns_dns_response_rcode_count_total{") {
			continue
		}
		rcode := labelValue(line, "rcode")
		if rcode == "" {
			continue
		}
		brace := strings.LastIndexByte(line, '}')
		if brace < 0 {
			continue
		}
		fields := strings.Fields(line[brace+1:])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		// ParseFloat accepts "NaN", "+Inf" and "-Inf" without error, and a
		// negative counter is not a counter. Converting any of them to int64 is
		// implementation-defined and yields math.MinInt64 here, which would turn
		// a DNS response count negative and drag the error ratio with it.
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		n := int64(v)
		if v > float64(maxCount) {
			n = maxCount
		}
		out[rcode] = saturatingAdd(out[rcode], n)
	}
	return out
}

// labelValue returns the value of the `key="..."` label in a Prometheus sample
// line, or "" when absent.
func labelValue(line, key string) string {
	needle := key + `="`
	i := strings.Index(line, needle)
	if i < 0 {
		return ""
	}
	rest := line[i+len(needle):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// Assess collapses the aggregated rcode counts (summed across all probed pods) and
// the per-pod probe outcomes into a Report. threshold is the error ratio that trips
// "degraded"; floor is the minimum total responses required to judge.
func Assess(agg map[string]int64, podsProbed, forbidden, unreachable int, threshold float64, floor int64) Report {
	if podsProbed == 0 {
		return Report{Status: ""}
	}
	if forbidden > 0 && forbidden == podsProbed {
		return Report{Status: "forbidden"}
	}
	if unreachable == podsProbed {
		return Report{Status: "unreachable"}
	}
	// Assess is exported and pure: it must hold for any map a caller hands it,
	// not only for one ParseResponses produced. Negative counts are dropped and
	// the sum saturates rather than wrapping.
	// Assess is exported and pure: it must hold for any map a caller hands it,
	// not only for one ParseResponses produced. Negative counts are dropped and
	// the sum saturates rather than wrapping.
	var total int64
	for _, v := range agg {
		if v < 0 {
			continue
		}
		total = saturatingAdd(total, v)
	}
	if total == 0 {
		// No pod returned usable metrics; prefer the concrete failure reason.
		switch {
		case forbidden > 0:
			return Report{Status: "forbidden"}
		case unreachable > 0:
			return Report{Status: "unreachable"}
		default:
			return Report{Status: "", PodsProbed: podsProbed}
		}
	}
	errors := saturatingAdd(max(agg["SERVFAIL"], 0), max(agg["REFUSED"], 0))
	if errors > total {
		errors = total
	}
	ratio := float64(errors) / float64(total)
	if total < floor {
		return Report{Status: "ok", TotalResponses: total, PodsProbed: podsProbed}
	}
	if ratio >= threshold {
		return Report{Status: "degraded", ServfailRatio: ratio, ErrorResponses: errors, TotalResponses: total, PodsProbed: podsProbed}
	}
	return Report{Status: "ok", ServfailRatio: ratio, ErrorResponses: errors, TotalResponses: total, PodsProbed: podsProbed}
}
