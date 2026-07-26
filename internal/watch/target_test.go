package watch

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestValidateTargets pins the startup contract. Client construction contacts no
// API server, so anything wrong at this point is a configuration error — a
// misspelled context — and must be fatal. Degrading into silently watching two
// of the three clusters an operator asked for is the failure mode this prevents.
func TestValidateTargets(t *testing.T) {
	ok := fake.NewSimpleClientset()
	tests := []struct {
		name    string
		targets []Target
		wantErr string
	}{
		{"empty list", nil, "no clusters"},
		{"empty name", []Target{{Name: "", Client: ok}}, "empty"},
		{"nil client", []Target{{Name: "a"}}, "no client"},
		{"duplicate names", []Target{{Name: "a", Client: ok}, {Name: "a", Client: ok}}, "duplicate"},
		{"valid", []Target{{Name: "a", Client: ok}, {Name: "b", Client: ok}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTargets(tc.targets)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTargets = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTargets = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}
