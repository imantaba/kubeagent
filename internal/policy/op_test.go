package policy

import (
	"math"
	"strings"
	"testing"
)

func present(v any) Slot { return Slot{Present: true, Value: v} }

func TestCheckOpAbsenceTable(t *testing.T) {
	cases := []struct {
		op       Op
		wantOK   bool
		wantSkip bool
	}{
		{OpExists, false, false},   // absent fails exists — a violation
		{OpNotExists, true, false}, // absent satisfies notExists
		{OpIn, false, true},
		{OpNotIn, false, true},
		{OpMatches, false, true},
		{OpNotMatches, false, true},
		{OpGt, false, true},
		{OpGte, false, true},
		{OpLt, false, true},
		{OpLte, false, true},
	}
	for _, c := range cases {
		ok, skip := checkOp(c.op, absent, []string{"1"})
		if ok != c.wantOK || skip != c.wantSkip {
			t.Errorf("checkOp(%s, absent) = (%v, %v), want (%v, %v)", c.op, ok, skip, c.wantOK, c.wantSkip)
		}
	}
}

func TestCheckOpPresence(t *testing.T) {
	if ok, skip := checkOp(OpExists, present("x"), nil); !ok || skip {
		t.Errorf("exists on a present slot = (%v, %v), want (true, false)", ok, skip)
	}
	if ok, skip := checkOp(OpNotExists, present("x"), nil); ok || skip {
		t.Errorf("notExists on a present slot = (%v, %v), want (false, false)", ok, skip)
	}
}

func TestCheckOpSetMembership(t *testing.T) {
	vals := []string{"Always", "IfNotPresent"}
	if ok, _ := checkOp(OpIn, present("Always"), vals); !ok {
		t.Error("Always is in the set")
	}
	if ok, _ := checkOp(OpIn, present("Never"), vals); ok {
		t.Error("Never is not in the set")
	}
	if ok, _ := checkOp(OpNotIn, present("Never"), vals); !ok {
		t.Error("Never satisfies notIn")
	}
	if ok, _ := checkOp(OpNotIn, present("Always"), vals); ok {
		t.Error("Always fails notIn")
	}
}

func TestCheckOpGlobMatching(t *testing.T) {
	vals := []string{"registry.example.com/*", "quay.example.org/*"}
	if ok, _ := checkOp(OpMatches, present("registry.example.com/team/app:1.0"), vals); !ok {
		t.Error("an allowlisted registry must match")
	}
	if ok, _ := checkOp(OpMatches, present("docker.example.net/app:1.0"), vals); ok {
		t.Error("an unlisted registry must not match")
	}
	if ok, _ := checkOp(OpNotMatches, present("docker.example.net/app:1.0"), vals); !ok {
		t.Error("an unlisted registry satisfies notMatches")
	}
}

// An annotation value can be hundreds of kilobytes and comes from whoever
// wrote the workload. globMatch is quadratic in the worst case, so an
// unbounded call would let that author stall a scan. Over the cap the slot is
// skipped, never silently reported as matching or as not matching — both would
// be a judgement kubeagent did not actually make.
func TestCheckOpSkipsAValueTooLongToMatchSafely(t *testing.T) {
	long := strings.Repeat("a", maxMatchLen+1)
	atCap := strings.Repeat("a", maxMatchLen)

	for _, op := range []Op{OpMatches, OpNotMatches} {
		ok, skip := checkOp(op, present(long), []string{"a*"})
		if !skip {
			t.Errorf("%s on a %d-byte value: skip=false, want true", op, len(long))
		}
		if ok {
			t.Errorf("%s on an over-cap value returned ok=true; a skipped slot decides nothing", op)
		}
		// One byte under the cap still evaluates: the cap must not quietly
		// disable matching for ordinary values.
		if _, skip := checkOp(op, present(atCap), []string{"a*"}); skip {
			t.Errorf("%s on a %d-byte value was skipped; the cap is inclusive", op, len(atCap))
		}
	}

	// Only the glob operators are capped. Equality is a byte compare and costs
	// nothing, so capping it would drop a comparison that was safe to make.
	if _, skip := checkOp(OpIn, present(long), []string{long}); skip {
		t.Error("in was skipped on a long value; only the glob operators are capped")
	}
}

func TestCheckOpNumericAndQuantityComparison(t *testing.T) {
	cases := []struct {
		op       Op
		got      any
		want     string
		wantOK   bool
		wantSkip bool
		why      string
	}{
		{OpLte, "4Gi", "4Gi", true, false, "equal quantities"},
		{OpLte, "8Gi", "4Gi", false, false, "8Gi exceeds 4Gi"},
		{OpLt, "500m", "1", true, false, "millicores against a whole core"},
		{OpGte, int64(3), "2", true, false, "plain integers"},
		{OpGt, float64(1.5), "2", false, false, "plain floats"},
		{OpGte, "3", "2", true, false, "numeric strings"},
		{OpLte, "not-a-number", "4Gi", false, true, "an unparseable field must not become a false accusation"},
		{OpLte, "4Gi", "not-a-number", false, true, "an unparseable threshold skips too"},
		{OpLte, math.NaN(), "4", false, true, "NaN is not comparable"},
		{OpLte, math.Inf(1), "4", false, true, "Inf is not comparable"},
	}
	for _, c := range cases {
		ok, skip := checkOp(c.op, present(c.got), []string{c.want})
		if ok != c.wantOK || skip != c.wantSkip {
			t.Errorf("checkOp(%s, %#v, %q) = (%v, %v), want (%v, %v) — %s",
				c.op, c.got, c.want, ok, skip, c.wantOK, c.wantSkip, c.why)
		}
	}
}

func TestCheckOpSkipsNonScalarValues(t *testing.T) {
	for _, v := range []any{
		map[string]any{"a": "b"},
		[]any{"a"},
	} {
		for _, op := range []Op{OpIn, OpMatches, OpGt} {
			if ok, skip := checkOp(op, present(v), []string{"a"}); ok || !skip {
				t.Errorf("checkOp(%s, %#v) = (%v, %v), want (false, true)", op, v, ok, skip)
			}
		}
	}
}

// TestCheckOpSkipsWhenValuesAreMissing is defence in depth: the loader makes
// this unreachable in production, but a fuzz target calls checkOp directly.
func TestCheckOpSkipsWhenValuesAreMissing(t *testing.T) {
	for _, op := range []Op{OpIn, OpNotIn, OpMatches, OpNotMatches, OpGt, OpGte, OpLt, OpLte} {
		if ok, skip := checkOp(op, present("x"), nil); ok || !skip {
			t.Errorf("checkOp(%s, present, nil values) = (%v, %v), want (false, true)", op, ok, skip)
		}
	}
}

func TestStringOf(t *testing.T) {
	cases := []struct {
		in     any
		want   string
		wantOK bool
	}{
		{"x", "x", true},
		{true, "true", true},
		{false, "false", true},
		{int64(7), "7", true},
		{float64(1.5), "1.5", true},
		{float64(2), "2", true},
		{math.NaN(), "", false},
		{math.Inf(-1), "", false},
		{map[string]any{}, "", false},
		{[]any{}, "", false},
		{nil, "", false},
	}
	for _, c := range cases {
		got, ok := stringOf(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("stringOf(%#v) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
