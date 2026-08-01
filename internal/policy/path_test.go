package policy

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// podWithContainers builds the unstructured shape
// runtime.DefaultUnstructuredConverter produces for a Pod: every container is
// a map, and a container that sets no CPU limit simply has no
// resources.limits.cpu key.
func podWithContainers(cpuLimits ...string) map[string]any {
	containers := make([]any, 0, len(cpuLimits))
	for i, lim := range cpuLimits {
		c := map[string]any{"name": string(rune('a' + i)), "image": "app:1.0"}
		if lim != "" {
			c["resources"] = map[string]any{"limits": map[string]any{"cpu": lim}}
		}
		containers = append(containers, c)
	}
	return map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": "web", "namespace": "prod"},
		"spec":     map[string]any{"containers": containers},
	}
}

// resolve collects every slot walk yields. The tests below read as claims
// about the full ordered slot list a path names — arity, order, which
// positions are absent — so they collect first and assert on the list. Nothing
// in production wants every slot: check stops at the first that violates.
func resolve(obj map[string]any, segs []segment) []Slot {
	var out []Slot
	walk(obj, segs, func(s Slot) bool {
		out = append(out, s)
		return true
	})
	return out
}

func mustParse(t *testing.T, path string) []segment {
	t.Helper()
	segs, err := parsePath(path)
	if err != nil {
		t.Fatalf("parsePath(%q): %v", path, err)
	}
	return segs
}

// TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes is the load-bearing
// test of this package. A Pod with three containers where only the middle one
// sets a CPU limit must resolve to THREE slots — absent, present, absent — in
// container order. Anything that returns a single present slot makes
// `exists` on spec.containers[*].resources.limits.cpu silently pass a Pod
// where two of three containers are unlimited, which is the exact bug this
// rule exists to catch.
func TestWildcardYieldsOneSlotPerElementIncludingAbsentOnes(t *testing.T) {
	obj := podWithContainers("", "500m", "")
	slots := resolve(obj, mustParse(t, "spec.containers[*].resources.limits.cpu"))

	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3 (one per container, present or not): %#v", len(slots), slots)
	}
	if slots[0].Present {
		t.Errorf("container 0 sets no CPU limit; slot must be absent, got %#v", slots[0].Value)
	}
	if !slots[1].Present || slots[1].Value != "500m" {
		t.Errorf("container 1 sets 500m; got present=%v value=%#v", slots[1].Present, slots[1].Value)
	}
	if slots[2].Present {
		t.Errorf("container 2 sets no CPU limit; slot must be absent, got %#v", slots[2].Value)
	}
}

func TestWildcardOnEveryContainerPresent(t *testing.T) {
	obj := podWithContainers("100m", "200m", "300m")
	slots := resolve(obj, mustParse(t, "spec.containers[*].resources.limits.cpu"))
	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(slots))
	}
	for i, want := range []string{"100m", "200m", "300m"} {
		if !slots[i].Present || slots[i].Value != want {
			t.Errorf("slot %d = (%v, %#v), want (true, %q)", i, slots[i].Present, slots[i].Value, want)
		}
	}
}

func TestWildcardOnEmptyListYieldsZeroSlots(t *testing.T) {
	obj := podWithContainers()
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 0 {
		t.Fatalf("an empty list must yield zero slots, got %d", len(slots))
	}
}

func TestWildcardOnMissingListYieldsOneAbsentSlot(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("want one absent slot, got %#v", slots)
	}
}

func TestWildcardOnNonListYieldsOneAbsentSlot(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"containers": "not-a-list"}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].image"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("want one absent slot, got %#v", slots)
	}
}

func TestPlainPathResolution(t *testing.T) {
	obj := podWithContainers("100m")
	cases := []struct {
		path    string
		present bool
		value   any
	}{
		{"metadata.name", true, "web"},
		{"metadata.namespace", true, "prod"},
		{"metadata.uid", false, nil},
		{"metadata.name.deeper", false, nil}, // cursor is a string, not a map
		{"spec", true, nil},                  // present, value is the map itself
	}
	for _, c := range cases {
		slots := resolve(obj, mustParse(t, c.path))
		if len(slots) != 1 {
			t.Errorf("%s: got %d slots, want 1", c.path, len(slots))
			continue
		}
		if slots[0].Present != c.present {
			t.Errorf("%s: present = %v, want %v", c.path, slots[0].Present, c.present)
			continue
		}
		if c.value != nil && slots[0].Value != c.value {
			t.Errorf("%s: value = %#v, want %#v", c.path, slots[0].Value, c.value)
		}
	}
}

// TestExplicitNullIsAbsent: YAML `key: null` decodes to a nil interface value.
// Treating it as present would make `exists` pass on a field that holds
// nothing.
func TestExplicitNullIsAbsent(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"nodeName": nil}}
	slots := resolve(obj, mustParse(t, "spec.nodeName"))
	if len(slots) != 1 || slots[0].Present {
		t.Fatalf("an explicit null must be absent, got %#v", slots)
	}
}

// TestNestedWildcardsMultiply: two wildcards on the same path produce the
// cross-product, still one slot per leaf position.
func TestNestedWildcardsMultiply(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{"containers": []any{
		map[string]any{"ports": []any{
			map[string]any{"containerPort": int64(80)},
			map[string]any{"containerPort": int64(443)},
		}},
		map[string]any{"ports": []any{
			map[string]any{"containerPort": int64(8080)},
		}},
		map[string]any{}, // no ports at all
	}}}
	slots := resolve(obj, mustParse(t, "spec.containers[*].ports[*].containerPort"))
	if len(slots) != 4 {
		t.Fatalf("got %d slots, want 4 (2 + 1 + 1 absent): %#v", len(slots), slots)
	}
	if !slots[0].Present || slots[0].Value != int64(80) {
		t.Errorf("slot 0 = %#v, want 80", slots[0])
	}
	if !slots[2].Present || slots[2].Value != int64(8080) {
		t.Errorf("slot 2 = %#v, want 8080", slots[2])
	}
	if slots[3].Present {
		t.Errorf("the container with no ports must contribute one absent slot, got %#v", slots[3])
	}
}

func TestParsePathAcceptsValidPaths(t *testing.T) {
	for _, p := range []string{
		"metadata.name",
		"spec.containers[*].image",
		"spec.containers[*].ports[*].containerPort",
		"spec",
		`metadata.labels["app.kubernetes.io/name"]`,
		`metadata.annotations["example.com/owner"]`,
		`metadata.labels["tier"].nested`,
	} {
		if _, err := parsePath(p); err != nil {
			t.Errorf("parsePath(%q) = %v, want no error", p, err)
		}
	}
}

// A Kubernetes label key contains dots and a slash, so splitting on dots
// cannot reach it at all. This is the case the quoted form exists for, and a
// grammar that accepts the spelling but resolves it as three nested lookups
// would find nothing and report every object as compliant.
func TestQuotedKeyIsOneLookupNotThree(t *testing.T) {
	segs, err := parsePath(`metadata.labels["app.kubernetes.io/name"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3 (metadata, labels, the whole key)", len(segs))
	}
	if segs[2].key != "app.kubernetes.io/name" {
		t.Errorf("key = %q, want the literal label key", segs[2].key)
	}
	if segs[2].wildcard {
		t.Error(`["key"] is a lookup, never a wildcard`)
	}

	obj := map[string]any{"metadata": map[string]any{
		"labels": map[string]any{"app.kubernetes.io/name": "checkout"},
	}}
	slots := resolve(obj, segs)
	if len(slots) != 1 || !slots[0].Present || slots[0].Value != "checkout" {
		t.Fatalf("resolve = %#v, want one present slot holding the label value", slots)
	}
}

func TestParsePathRejectsMalformedPaths(t *testing.T) {
	for _, p := range []string{
		"",                         // empty
		".",                        // empty segment
		"metadata..name",           // empty segment
		".metadata",                // leading dot
		"metadata.",                // trailing dot
		"spec.containers[0].image", // an index is not supported; only [*]
		"spec.containers[].image",  // empty bracket
		"spec.containers[*",        // unterminated
		"spec.containers*].image",  // stray bracket
		"spec.[*].image",           // wildcard with no key
		"[*].image",                // wildcard with no key, at the start
		`metadata.labels["unterminated`,
		`metadata.labels[""]`,    // empty key
		`metadata.labels["a"b"]`, // a quote inside the key
		`metadata.labels[app]`,   // unquoted bracket key
	} {
		if _, err := parsePath(p); err == nil {
			t.Errorf("parsePath(%q) = nil error, want a rejection", p)
		}
	}
}

// TestWalkStopsAtTheFirstSlotTheVisitorRejects is why walk exists. check only
// ever needs the first slot that violates; materializing the rest is work the
// answer does not depend on. A visitor returning false must end the traversal
// there — not merely have its later calls ignored.
func TestWalkStopsAtTheFirstSlotTheVisitorRejects(t *testing.T) {
	obj := podWithContainers("100m", "200m", "300m")
	segs := mustParse(t, "spec.containers[*].resources.limits.cpu")

	var seen []any
	walk(obj, segs, func(s Slot) bool {
		seen = append(seen, s.Value)
		return s.Value != "200m" // stop on the second container
	})

	want := []any{"100m", "200m"}
	if len(seen) != len(want) {
		t.Fatalf("visitor saw %d slots (%#v), want %d: traversal did not stop", len(seen), seen, len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("slot %d = %#v, want %#v", i, seen[i], want[i])
		}
	}
}

// TestCheckDoesNotMaterializeEverySlot is the regression guard for the cost
// that motivated walk: check must not build a slot list proportional to the
// object. Evaluating one rule against a 40 000-element object used to allocate
// 40 224 times and 17 MB, and a cluster's worth of objects times a policy's
// worth of rules multiplies both. The bound is generous — the point is the
// difference between "a few allocations" and "one per element", not a precise
// count that a Go release could move. Early exit is pinned separately, by
// TestWalkStopsAtTheFirstSlotTheVisitorRejects.
func TestCheckDoesNotMaterializeEverySlot(t *testing.T) {
	containers := make([]any, 0, 200)
	for i := 0; i < 200; i++ {
		ports := make([]any, 0, 200)
		for j := 0; j < 200; j++ {
			ports = append(ports, map[string]any{"containerPort": int64(8000 + j)})
		}
		containers = append(containers, map[string]any{"name": "c", "ports": ports})
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Pod",
		"metadata": map[string]any{"name": "web", "namespace": "prod"},
		"spec":     map[string]any{"containers": containers},
	}}
	segs := mustParse(t, "spec.containers[*].ports[*].containerPort")
	r := Rule{ID: "ports-exist", Assert: Assert{Op: OpExists}}

	allocs := testing.AllocsPerRun(3, func() {
		if _, violated := check(r, segs, obj, Inputs{}); violated {
			t.Fatal("exists must hold: every port sets containerPort")
		}
	})
	if allocs > 1000 {
		t.Errorf("check allocated %.0f times on a 40000-slot object; want < 1000 "+
			"(one allocation per element means the traversal is materializing the whole slot list again)", allocs)
	}
}
