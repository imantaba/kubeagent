package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bump-version.sh derives its repo root from BASH_SOURCE and cd's there, so it
// always edits the real tree. To test it we copy the script into a fixture
// tree and run it there. The copy is read from disk at test time, so it cannot
// drift from the script the release actually runs.
func TestBumpVersionMovesPluginManifest(t *testing.T) {
	script, err := os.ReadFile("scripts/bump-version.sh")
	if err != nil {
		t.Fatalf("reading the bump script: %v", err)
	}

	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("scripts/bump-version.sh", string(script))
	if err := os.Chmod(filepath.Join(root, "scripts/bump-version.sh"), 0o755); err != nil {
		t.Fatal(err)
	}

	write("CHANGELOG.md", strings.Join([]string{
		"# Changelog", "",
		"## [Unreleased]", "",
		"## [1.2.0] - 2026-08-05", "",
		"[Unreleased]: https://github.com/imantaba/kubeagent/compare/v1.2.0...HEAD",
		"[1.2.0]: https://github.com/imantaba/kubeagent/compare/v1.1.0...v1.2.0", "",
	}, "\n"))
	write("deploy/helm/kubeagent/Chart.yaml", "version: 0.4.0\nappVersion: \"v1.2.0\"\n")
	write("deploy/deployment.yaml", "image: imantaba/kubeagent:v1.2.0\n")
	write("deploy/README.md", "--set image.tag=v1.2.0\n")
	write("website/docs/install.md", "imantaba/kubeagent:v1.2.0 and --set image.tag=v1.2.0\n")
	write(".claude-plugin/plugin.json", "{\n  \"name\": \"kubeagent\",\n  \"version\": \"1.2.0\"\n}\n")

	cmd := exec.Command("bash", filepath.Join(root, "scripts/bump-version.sh"), "v1.3.0")
	cmd.Env = append(os.Environ(), "RELEASE_DATE=2026-08-06")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bump-version.sh failed: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatalf("reading the bumped manifest: %v", err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the bumped manifest is no longer valid JSON: %v\n%s", err, raw)
	}
	if manifest.Version != "1.3.0" {
		t.Errorf("plugin.json version = %q after bumping to v1.3.0, want %q\n"+
			"script output:\n%s", manifest.Version, "1.3.0", out)
	}
}
