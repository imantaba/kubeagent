package gitops

import "testing"

func TestShortRevision(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain sha", "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "a1b2c3d"},
		{"short sha", "a1b2c3d", "a1b2c3d"},
		{"flux branch qualified", "main@sha1:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "a1b2c3d"},
		{"flux tag qualified", "v1.2.3@sha1:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c", "9f8e7d6"},
		{"branch name alone", "main", "(revision withheld)"},
		{"tag alone", "v1.2.3", "(revision withheld)"},
		{"chart version", "1.14.5", "(revision withheld)"},
		{"repo url with token", "https://tok3n@git.example/org/repo.git", "(revision withheld)"},
		{"branch that looks hex until the ref split", "deadbeef@sha1:notahexrevision", "(revision withheld)"},
		{"too short", "a1b2c3", "(revision withheld)"},
		{"uppercase is not a git sha", "A1B2C3D4E5F6A7B8", "(revision withheld)"},
		{"empty", "", "(revision withheld)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortRevision(tt.raw); got != tt.want {
				t.Errorf("ShortRevision(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
