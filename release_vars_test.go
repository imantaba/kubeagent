package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Executes the real scripts/release-vars.sh, for the same reason
// krew_manifest_test.go executes the real renderer: the script is what the
// release workflow runs.

// releaseVars runs the script and parses its key=value lines.
func releaseVars(t *testing.T, version string) map[string]string {
	t.Helper()
	out, err := exec.Command("scripts/release-vars.sh", version).Output()
	if err != nil {
		t.Fatalf("release-vars.sh %q: %v", version, err)
	}
	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("release-vars.sh %q: line %q is not key=value", version, line)
		}
		vars[k] = v
	}
	return vars
}

func TestReleaseVars_Classification(t *testing.T) {
	cases := []struct {
		version    string
		prerelease string
		pushLatest string
	}{
		{"v1.2.3", "false", "true"},
		{"v0.68.0", "false", "true"},
		{"v1.2.3-rc.1", "true", "false"},
		{"v0.68.0-alpha.2", "true", "false"},
		{"v1.0.0-0.beta", "true", "false"},
		// Build metadata is not a pre-release: +build.5 says how it was
		// built, not that it is provisional.
		{"v1.2.3+build.5", "false", "true"},
		{"v1.2.3-rc.1+build.5", "true", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			vars := releaseVars(t, tc.version)
			if got := vars["prerelease"]; got != tc.prerelease {
				t.Errorf("prerelease = %q, want %q", got, tc.prerelease)
			}
			if got := vars["push_latest"]; got != tc.pushLatest {
				t.Errorf("push_latest = %q, want %q", got, tc.pushLatest)
			}
		})
	}
}

// A malformed tag must stop the release. Exiting 0 with a best guess would
// publish a release under a name nobody chose.
func TestReleaseVars_RejectsMalformedVersions(t *testing.T) {
	for _, version := range []string{"", "1.2.3", "v1.2", "vX.Y.Z", "latest", "v1.2.3.4", "v1.2.3 -rc.1"} {
		t.Run("reject "+version, func(t *testing.T) {
			out, err := exec.Command("scripts/release-vars.sh", version).CombinedOutput()
			if err == nil {
				t.Fatalf("release-vars.sh %q exited 0, want non-zero; output:\n%s", version, out)
			}
			if !strings.Contains(string(out), "error:") {
				t.Errorf("release-vars.sh %q: stderr does not explain the failure:\n%s", version, out)
			}
		})
	}
}
