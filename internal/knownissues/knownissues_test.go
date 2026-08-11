package knownissues

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// The fourteen kinds DefaultDetectors can emit. This list is duplicated from
// internal/diagnose deliberately: this package imports nothing from kubeagent,
// so the join is proved from the other side, in
// internal/diagnose/knownissues_test.go, where both sets are in scope.
var wantKinds = []string{
	"CrashLoopBackOff",
	"CreateContainerConfigError",
	"ErrImagePull",
	"ImagePullBackOff",
	"Init:CrashLoopBackOff",
	"Init:CreateContainerConfigError",
	"Init:ErrImagePull",
	"Init:ImagePullBackOff",
	"Init:OOMKilled",
	"OOMKilled",
	"ProbeFailure",
	"RestartLoop",
	"Unschedulable",
	"VolumeAttachError",
}

func TestKindsAreTheFourteen(t *testing.T) {
	got := Kinds()
	if len(got) != len(wantKinds) {
		t.Fatalf("Kinds() has %d entries, want %d: %v", len(got), len(wantKinds), got)
	}
	for i, k := range wantKinds {
		if got[i] != k {
			t.Errorf("Kinds()[%d] = %q, want %q", i, got[i], k)
		}
	}
}

// Two entries for one kind would make Lookup's answer depend on slice order,
// and the second would be unreachable.
func TestNoDuplicateKinds(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range All() {
		if seen[e.Kind] {
			t.Errorf("duplicate entry for kind %q", e.Kind)
		}
		seen[e.Kind] = true
	}
}

// All() is sorted by Kind, and Kinds() is All()'s kinds in the same order. A
// caller that ranges over one and indexes the other must not be surprised.
func TestAllIsSortedAndAgreesWithKinds(t *testing.T) {
	all := All()
	if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].Kind < all[j].Kind }) {
		t.Error("All() is not sorted by Kind")
	}
	kinds := Kinds()
	if len(kinds) != len(all) {
		t.Fatalf("Kinds() has %d, All() has %d", len(kinds), len(all))
	}
	for i := range all {
		if kinds[i] != all[i].Kind {
			t.Errorf("index %d: Kinds() = %q, All() = %q", i, kinds[i], all[i].Kind)
		}
	}
}

// All() must not hand a caller the package's own backing array: a consumer that
// sorts or truncates the result must not corrupt the registry for the next one.
func TestAllReturnsACopy(t *testing.T) {
	first := All()
	if len(first) == 0 {
		t.Fatal("All() is empty")
	}
	first[0].Kind = "mutated"
	if All()[0].Kind == "mutated" {
		t.Error("All() shares its backing array with the registry")
	}
}

// The copy goes all the way down. A caller that rewrites a returned entry's
// Causes must not rewrite the registry — the shallow copy of the []Entry header
// alone does not prevent that, because the Causes slice inside each Entry would
// still point at the registry's own backing array.
func TestAllCopiesTheNestedSlices(t *testing.T) {
	first := All()
	if len(first) == 0 || len(first[0].Causes) == 0 || len(first[0].Checks) == 0 {
		t.Fatal("All() is empty, or its first entry has no causes or checks")
	}
	first[0].Causes[0] = "mutated"
	first[0].Checks[0] = "mutated"
	again := All()
	if again[0].Causes[0] == "mutated" {
		t.Error("All() shares its Causes slice with the registry")
	}
	if again[0].Checks[0] == "mutated" {
		t.Error("All() shares its Checks slice with the registry")
	}
}

// Lookup hands out an Entry by value and has the same nested aliasing.
func TestLookupCopiesTheNestedSlices(t *testing.T) {
	e, ok := Lookup("OOMKilled")
	if !ok {
		t.Fatal("OOMKilled missing")
	}
	e.Causes[0] = "mutated"
	again, _ := Lookup("OOMKilled")
	if again.Causes[0] == "mutated" {
		t.Error("Lookup shares its Causes slice with the registry")
	}
}

// Lookup is an exact match. No case folding, no Init: stripping, no fuzz.
func TestLookupIsExact(t *testing.T) {
	for _, k := range wantKinds {
		e, ok := Lookup(k)
		if !ok {
			t.Errorf("Lookup(%q) not found", k)
			continue
		}
		if e.Kind != k {
			t.Errorf("Lookup(%q).Kind = %q", k, e.Kind)
		}
	}
	for _, miss := range []string{"oomkilled", "OOMKILLED", " OOMKilled", "OOMKilled ", "Init:Nope", "", "Pending"} {
		if _, ok := Lookup(miss); ok {
			t.Errorf("Lookup(%q) matched, want no match", miss)
		}
	}
}

// An Init: kind is its own failure mode, not an alias for the base kind. If
// Lookup ever fell back by stripping the prefix, these two would return the
// same entry.
func TestInitKindsAreDistinctEntries(t *testing.T) {
	base, ok := Lookup("OOMKilled")
	if !ok {
		t.Fatal("OOMKilled missing")
	}
	init, ok := Lookup("Init:OOMKilled")
	if !ok {
		t.Fatal("Init:OOMKilled missing")
	}
	if base.Detail == init.Detail {
		t.Error("Init:OOMKilled reuses OOMKilled's Detail; it is a different failure mode")
	}
}

func TestEveryEntryIsPopulated(t *testing.T) {
	for _, e := range All() {
		if e.Kind == "" || e.Summary == "" || e.Detail == "" {
			t.Errorf("%q: an empty Kind, Summary or Detail", e.Kind)
		}
		if len(e.Causes) < 2 {
			t.Errorf("%q: %d causes, want at least 2", e.Kind, len(e.Causes))
		}
		if len(e.Checks) < 2 {
			t.Errorf("%q: %d checks, want at least 2", e.Kind, len(e.Checks))
		}
		if e.Docs == "" {
			t.Errorf("%q: no Docs anchor", e.Kind)
		}
	}
}

// The struct's doc comments promise a Summary is lowercase with no trailing
// period and a Detail is capitalised and punctuated. A comment that promises
// what the code does not keep is a defect, so the promise is a test.
func TestProseStyle(t *testing.T) {
	for _, e := range All() {
		if r := []rune(e.Summary)[0]; unicode.IsUpper(r) {
			t.Errorf("%q: Summary starts uppercase: %q", e.Kind, e.Summary)
		}
		if strings.HasSuffix(e.Summary, ".") {
			t.Errorf("%q: Summary ends with a period: %q", e.Kind, e.Summary)
		}
		if r := []rune(e.Detail)[0]; !unicode.IsUpper(r) {
			t.Errorf("%q: Detail starts lowercase: %q", e.Kind, e.Detail)
		}
		if !strings.HasSuffix(e.Detail, ".") {
			t.Errorf("%q: Detail is not punctuated: %q", e.Kind, e.Detail)
		}
		for _, c := range e.Causes {
			if !strings.HasSuffix(c, ".") {
				t.Errorf("%q: cause is not punctuated: %q", e.Kind, c)
			}
		}
	}
}

// hostMarkers are the substrings that would mean a host, an address or a URL
// had reached the prose. URLs are credentials in this repository; the one
// permitted host is the project's own and it belongs in Docs, which this test
// deliberately does not scan.
var hostMarkers = []string{"://", "http", "www.", ".com", ".net", ".org", ".io", "k8sproject"}

var dottedQuad = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func TestNoHostReachesTheProse(t *testing.T) {
	for _, e := range All() {
		fields := map[string][]string{
			"Summary": {e.Summary},
			"Detail":  {e.Detail},
			"Causes":  e.Causes,
			"Checks":  e.Checks,
		}
		for name, texts := range fields {
			for _, text := range texts {
				lower := strings.ToLower(text)
				for _, m := range hostMarkers {
					if strings.Contains(lower, m) {
						t.Errorf("%q %s contains %q: %q", e.Kind, name, m, text)
					}
				}
				if dottedQuad.MatchString(text) {
					t.Errorf("%q %s contains an address: %q", e.Kind, name, text)
				}
			}
		}
	}
}

// Docs is the one field allowed to carry a host, and only the project's own.
func TestDocsPointAtTheProjectSite(t *testing.T) {
	const prefix = "https://k8sproject.top/"
	for _, e := range All() {
		if !strings.HasPrefix(e.Docs, prefix) {
			t.Errorf("%q: Docs = %q, want a %s anchor", e.Kind, e.Docs, prefix)
		}
	}
}

// allowedPlaceholders is the closed set a Checks line may substitute. A real
// namespace, pod, container, node or object name in shipped help text would be
// someone's cluster leaking into the binary.
var allowedPlaceholders = map[string]bool{
	"<namespace>": true, "<pod>": true, "<container>": true, "<node>": true, "<name>": true,
}

var placeholder = regexp.MustCompile(`<[^>]*>`)

func TestChecksUseOnlyAllowedPlaceholders(t *testing.T) {
	for _, e := range All() {
		for _, c := range e.Checks {
			for _, p := range placeholder.FindAllString(c, -1) {
				if !allowedPlaceholders[p] {
					t.Errorf("%q: check uses %q, which is not an allowed placeholder: %q", e.Kind, p, c)
				}
			}
		}
	}
}
