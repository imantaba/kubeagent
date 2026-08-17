package investigate

import "testing"

// TestToolSpecs_RequiredMatchesWhatTheExecutorDereferences is a
// hand-maintained table -- not a go/parser walk over the executor bodies,
// which is a different, larger decision the operator did not take. One row
// per tool, each commented with the executor function it mirrors, asserting
// that every input property the executor actually dereferences is declared
// Required in the tool's own JSON schema.
//
// A property a tool does not require may reach the executor as a Go zero
// value ("") when the model omits it from a call. If the executor treats
// that value as meaningful -- a scope lookup key, a client-go call argument
// -- the model must be told it cannot skip supplying it; Required is that
// contract. Keep this table in sync by hand whenever an executor's use of
// its input changes.
func TestToolSpecs_RequiredMatchesWhatTheExecutorDereferences(t *testing.T) {
	tests := []struct {
		tool     string
		requires []string
	}{
		// Reader.describe / Reader.describeWorkload (reader.go) dereference
		// in.Kind (normKind, the describe switch), in.Namespace (nsFor,
		// scope.Allowed, and every kind's Get call), and in.Name
		// (scope.Allowed and every kind's Get call).
		{"describe", []string{"kind", "namespace", "name"}},
		// Reader.getEvents (reader.go) dereferences in.Namespace
		// (scope.HasName, the Events(...).List call) and in.Name
		// (scope.HasName, the involvedObject.name field selector).
		{"get_events", []string{"namespace", "name"}},
		// Reader.getRelated (reader.go) dereferences in.Namespace
		// (scope.Allowed, the pod Get call), in.Name (scope.Allowed, the pod
		// Get call), and in.Relation (the owner/node/pvc switch).
		{"get_related", []string{"namespace", "name", "relation"}},
	}

	byName := make(map[string]toolSpec)
	for _, s := range toolSpecs() {
		byName[s.Name] = s
	}

	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			spec, ok := byName[tt.tool]
			if !ok {
				t.Fatalf("no toolSpec named %q", tt.tool)
			}
			required := make(map[string]bool, len(spec.Required))
			for _, r := range spec.Required {
				required[r] = true
			}
			for _, prop := range tt.requires {
				if !required[prop] {
					t.Errorf("tool %q's executor dereferences %q but Required does not list it: Required=%v", tt.tool, prop, spec.Required)
				}
			}
		})
	}
}
