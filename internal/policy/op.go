package policy

import (
	"math"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"
)

// checkOp applies one operator to one slot.
//
// ok reports whether the slot satisfies the assertion. skip reports that the
// slot had nothing to say — an absent value under an operator other than
// exists/notExists, a non-scalar value, or a comparison neither side of which
// parses as a number or a Kubernetes quantity. A skipped slot is not a
// violation: a policy must never turn a field it cannot read into an
// accusation.
// maxMatchLen bounds what the glob operators will look at. Every value a
// policy realistically matches on — an image reference, a label value, a
// storage class name — is far below this; the cap exists for the annotation
// nobody expected. Do not remove it on the belief that globMatch is linear:
// it is not, and glob.go says so.
const maxMatchLen = 4096

func checkOp(op Op, s Slot, values []string) (ok, skip bool) {
	// exists and notExists are the only operators that have an opinion about
	// absence itself.
	switch op {
	case OpExists:
		return s.Present, false
	case OpNotExists:
		return !s.Present, false
	}
	if !s.Present {
		return false, true
	}
	// Defence in depth: the loader enforces arity, so production never gets
	// here with no values. A fuzz target calling checkOp directly can.
	if len(values) == 0 {
		return false, true
	}
	got, isScalar := stringOf(s.Value)
	if !isScalar {
		return false, true
	}
	switch op {
	case OpIn:
		for _, v := range values {
			if got == v {
				return true, false
			}
		}
		return false, false
	case OpNotIn:
		for _, v := range values {
			if got == v {
				return false, false
			}
		}
		return true, false
	case OpMatches, OpNotMatches:
		// globMatch is O(len(pattern) * len(got)) in the worst case — a single
		// star followed by a long partly-matching literal run. `got` comes from
		// the cluster and an annotation value can reach hundreds of kilobytes,
		// so an unbounded call is a workload author's denial of service against
		// a scan. Over the cap the slot is skipped, the same answer a non-scalar
		// gets: kubeagent declines to judge rather than guessing.
		if len(got) > maxMatchLen {
			return false, true
		}
		for _, v := range values {
			if globMatch(v, got) {
				return op == OpMatches, false
			}
		}
		return op == OpNotMatches, false
	case OpGt, OpGte, OpLt, OpLte:
		cmp, cmpOK := compareNumeric(got, values[0])
		if !cmpOK {
			return false, true
		}
		switch op {
		case OpGt:
			return cmp > 0, false
		case OpGte:
			return cmp >= 0, false
		case OpLt:
			return cmp < 0, false
		default:
			return cmp <= 0, false
		}
	}
	// An operator the loader should have rejected. Say nothing rather than
	// accuse.
	return false, true
}

// stringOf renders a scalar unstructured value as text, reporting false for
// anything that is not a scalar.
//
// runtime.DefaultUnstructuredConverter produces exactly five leaf types —
// string, bool, int64, float64 and nil — plus map[string]any and []any for
// the interior. Non-finite floats are refused: formatting a NaN and then
// comparing it produces nonsense, and an integer conversion of one overflows,
// which is the bug class the fuzz campaign already found in the DNS health
// parser.
func stringOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", false
		}
		return strconv.FormatFloat(t, 'g', -1, 64), true
	default:
		return "", false
	}
}

// compareNumeric compares two textual values, returning -1, 0 or 1, and
// whether the comparison was possible at all.
//
// Plain numbers first, so 3 vs 2 does not need a quantity round-trip; then
// Kubernetes quantities, so 500m vs 1 and 8Gi vs 4Gi compare correctly. If
// either side fails both, the caller skips.
func compareNumeric(a, b string) (int, bool) {
	af, aErr := strconv.ParseFloat(a, 64)
	bf, bErr := strconv.ParseFloat(b, 64)
	if aErr == nil && bErr == nil {
		if math.IsNaN(af) || math.IsNaN(bf) || math.IsInf(af, 0) || math.IsInf(bf, 0) {
			return 0, false
		}
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	aq, aErr := resource.ParseQuantity(a)
	if aErr != nil {
		return 0, false
	}
	bq, bErr := resource.ParseQuantity(b)
	if bErr != nil {
		return 0, false
	}
	return aq.Cmp(bq), true
}
