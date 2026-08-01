package policy

import (
	"fmt"
	"strings"
)

// Slot is one position a path resolved to. A path resolves to an ordered list
// of slots, not to a list of values: a wildcard produces one slot per list
// element EVEN WHEN that element lacks the rest of the path, and that slot is
// absent.
//
// This is the whole reason the type exists. On a Pod with three containers
// where only one sets a CPU limit, spec.containers[*].resources.limits.cpu
// resolves to three slots — one present, two absent — so `exists` violates.
// Collapsing to "the values that were found" would silently pass that Pod.
type Slot struct {
	Present bool
	Value   any
}

// segment is one dotted component of a path, optionally with a [*] suffix.
type segment struct {
	key      string
	wildcard bool
}

// parsePath splits a path into segments. Three forms:
//
//	name          a field
//	name[*]       every element of a list field
//	["literal"]   a map key written verbatim
//
// The bracket-quoted form is not a convenience. Kubernetes label and
// annotation keys contain dots and slashes, so app.kubernetes.io/name cannot
// be reached by splitting on dots at all — metadata.labels["app.kubernetes.io/name"]
// is the only way to write it, and label rules are the most common thing an
// operator wants a policy for.
//
// An index like [0] is rejected rather than silently ignored: a policy that
// reads like it pins the first container but does not would be worse than one
// that refuses to load.
func parsePath(path string) ([]segment, error) {
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}
	var segs []segment
	for i := 0; i < len(path); {
		switch {
		case path[i] == '.':
			return nil, fmt.Errorf("path %q has an empty segment", path)
		case strings.HasPrefix(path[i:], "[*]"):
			return nil, fmt.Errorf("path %q: [*] must follow a field name", path)
		case path[i] == '[':
			key, next, err := parseQuotedKey(path, i)
			if err != nil {
				return nil, err
			}
			segs = append(segs, segment{key: key})
			i = next
		default:
			j := i
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			seg := segment{key: path[i:j]}
			if strings.ContainsRune(seg.key, ']') {
				return nil, fmt.Errorf("path %q: stray bracket in %q", path, seg.key)
			}
			i = j
			// [*] binds to the field name it follows. ["key"] does not — it is
			// its own segment, handled on the next pass.
			if strings.HasPrefix(path[i:], "[*]") {
				seg.wildcard = true
				i += 3
			}
			segs = append(segs, seg)
		}
		// Between segments: a dot separator, or a bracket that opens the next
		// segment. Anything else is a path the operator did not mean to write.
		if i < len(path) && path[i] == '.' {
			i++
			if i == len(path) {
				return nil, fmt.Errorf("path %q has an empty segment", path)
			}
		}
	}
	return segs, nil
}

// parseQuotedKey reads a ["literal"] segment starting at i and returns the key
// and the index just past the closing bracket. The key is taken verbatim:
// there are no escapes, because a Kubernetes map key cannot contain a double
// quote and inventing an escape syntax for a character that cannot occur would
// be a rule to get wrong for nothing.
func parseQuotedKey(path string, i int) (string, int, error) {
	rest := path[i:]
	if !strings.HasPrefix(rest, `["`) {
		return "", 0, fmt.Errorf(`path %q: expected ["key"] or [*], got %q`, path, rest)
	}
	end := strings.Index(rest[2:], `"]`)
	if end < 0 {
		return "", 0, fmt.Errorf(`path %q: unterminated ["key"]`, path)
	}
	key := rest[2 : 2+end]
	if key == "" {
		return "", 0, fmt.Errorf(`path %q: ["..."] key is empty`, path)
	}
	if strings.ContainsRune(key, '"') {
		return "", 0, fmt.Errorf(`path %q: a double quote inside ["..."] is not supported`, path)
	}
	return key, i + 2 + end + 2, nil
}

// absent is the zero slot. Named so the propagation rules below read as rules.
var absent = Slot{}

// walk yields, in order, the slots segs names in obj, and stops as soon as
// visit returns false.
//
// Arity is the invariant: an absent cursor propagates as exactly one absent
// successor, a plain segment maps one cursor to one successor, and a wildcard
// maps one cursor to one successor per list element (zero for an empty list).
//
// Why a visitor rather than a returned slice: a caller almost never needs
// every slot. check returns on the first one that violates, and a wildcard
// over a large object names one slot per element — tens of thousands on an
// object near the API server's size limit — so building the whole list is
// work the answer does not depend on. resolve is the wrapper for the callers
// that do want it all.
func walk(obj map[string]any, segs []segment, visit func(Slot) bool) {
	walkFrom(Slot{Present: true, Value: any(obj)}, segs, visit)
}

// walkFrom yields the slots reachable from s through segs. It reports whether
// the traversal should continue: once the visitor answers false, every frame
// above unwinds without visiting a sibling.
func walkFrom(s Slot, segs []segment, visit func(Slot) bool) bool {
	if len(segs) == 0 {
		return visit(s)
	}
	seg, rest := segs[0], segs[1:]

	if !s.Present {
		// An absent slot stays exactly one absent slot, wildcard or not.
		// Collapsing it here would lose the arity the caller depends on.
		return walkFrom(absent, rest, visit)
	}
	m, ok := s.Value.(map[string]any)
	if !ok {
		return walkFrom(absent, rest, visit)
	}
	v, ok := m[seg.key]
	if !ok || v == nil {
		// A key present with an explicit null holds nothing; `exists` must
		// not pass on it.
		return walkFrom(absent, rest, visit)
	}
	if !seg.wildcard {
		return walkFrom(Slot{Present: true, Value: v}, rest, visit)
	}
	list, ok := v.([]any)
	if !ok {
		return walkFrom(absent, rest, visit)
	}
	// An empty list yields zero slots: there is nothing to assert about.
	for _, e := range list {
		if !walkFrom(Slot{Present: true, Value: e}, rest, visit) {
			return false
		}
	}
	return true
}
