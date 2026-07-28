package findings

import (
	"encoding/json"
	"testing"
)

func TestLevelOrdering(t *testing.T) {
	if !(Info < Warning && Warning < Critical) {
		t.Fatalf("want Info < Warning < Critical, got %d %d %d", Info, Warning, Critical)
	}
}

func TestLevelString(t *testing.T) {
	for _, tc := range []struct {
		level Level
		want  string
	}{
		{Info, "info"},
		{Warning, "warning"},
		{Critical, "critical"},
	} {
		if got := tc.level.String(); got != tc.want {
			t.Errorf("Level(%d).String() = %q, want %q", tc.level, got, tc.want)
		}
	}
}

func TestLevelMarshalJSON(t *testing.T) {
	b, err := json.Marshal(Critical)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `"critical"` {
		t.Errorf("Marshal(Critical) = %s, want \"critical\"", b)
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
	}{
		{"info", Info},
		{"warning", Warning},
		{"critical", Critical},
	} {
		got, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): unexpected error %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseRejectsUnknown(t *testing.T) {
	if _, err := Parse("fatal"); err == nil {
		t.Fatal("Parse(\"fatal\"): want an error, got nil")
	}
}
