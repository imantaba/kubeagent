package policypack

import (
	"bytes"
	"sort"
	"testing"
)

func TestAllIsSortedByName(t *testing.T) {
	packs := All()
	if len(packs) == 0 {
		t.Fatal("no packs — every later assertion would pass vacuously")
	}
	names := make([]string, len(packs))
	for i, p := range packs {
		names[i] = p.Name
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("All() = %v, want sorted by name", names)
	}
}

func TestEveryPackHasASummary(t *testing.T) {
	for _, p := range All() {
		if p.Summary == "" {
			t.Errorf("pack %q has no summary — the listing would print a bare name", p.Name)
		}
	}
}

func TestLookupIsExact(t *testing.T) {
	if _, ok := Lookup("reliability"); !ok {
		t.Fatal(`Lookup("reliability") = false, want the shipped pack`)
	}
	// No case folding and no fuzzy match: the name is the join between the
	// listing and the flag, so a near miss must be refused rather than guessed.
	for _, miss := range []string{"Reliability", "RELIABILITY", "reliabilit", "", "security"} {
		if _, ok := Lookup(miss); ok {
			t.Errorf("Lookup(%q) = true, want false", miss)
		}
	}
}

func TestNamesMatchesAll(t *testing.T) {
	packs := All()
	names := Names()
	if len(names) != len(packs) {
		t.Fatalf("Names() has %d entries, All() has %d", len(names), len(packs))
	}
	for i, p := range packs {
		if names[i] != p.Name {
			t.Errorf("Names()[%d] = %q, want %q", i, names[i], p.Name)
		}
	}
}

func TestBytesReturnsACopy(t *testing.T) {
	first, ok := Bytes("reliability")
	if !ok {
		t.Fatal(`Bytes("reliability") = false, want the shipped pack`)
	}
	if len(first) == 0 {
		t.Fatal("the pack is empty")
	}
	original := append([]byte(nil), first...)
	// A caller that mutates what it was handed must not reach the next caller.
	for i := range first {
		first[i] = 'x'
	}
	second, _ := Bytes("reliability")
	if !bytes.Equal(second, original) {
		t.Error("Bytes returned a view of the embedded pack — mutating it changed what the next caller sees")
	}
}

func TestBytesMissReturnsFalse(t *testing.T) {
	if _, ok := Bytes("no-such-pack"); ok {
		t.Error(`Bytes("no-such-pack") = true, want false`)
	}
}
