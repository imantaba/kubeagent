package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// These tests execute scripts/render-krew-manifest.sh itself. The script is
// what the release workflow runs, so the script is what must be tested: a Go
// reimplementation of its substitutions would keep passing while the real
// script rotted.

const krewTestVersion = "v9.9.9"

// Four DISTINCT fixture checksums. A renderer that pasted one checksum into
// all four platform slots would satisfy "every sha256 is 64 hex characters"
// and still ship a manifest that fails for three platforms out of four.
var krewTestSums = map[string]string{
	"linux_amd64":  strings.Repeat("a1", 32),
	"linux_arm64":  strings.Repeat("b2", 32),
	"darwin_amd64": strings.Repeat("c3", 32),
	"darwin_arm64": strings.Repeat("d4", 32),
}

var krewPlatformOrder = []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

type krewManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version          string `json:"version"`
		Homepage         string `json:"homepage"`
		ShortDescription string `json:"shortDescription"`
		Description      string `json:"description"`
		Caveats          string `json:"caveats"`
		Platforms        []struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
			URI    string `json:"uri"`
			Sha256 string `json:"sha256"`
			Bin    string `json:"bin"`
			Files  []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"files"`
		} `json:"platforms"`
	} `json:"spec"`
}

// krewFixtureSums renders a SHA256SUMS body in `sha256sum` format for the
// given platforms, in the given order.
func krewFixtureSums(platforms []string) string {
	var b strings.Builder
	for _, p := range platforms {
		fmt.Fprintf(&b, "%s  kubeagent_%s_%s.tar.gz\n", krewTestSums[p], krewTestVersion, p)
	}
	return b.String()
}

// renderKrewManifest runs the real script with a fixture checksum file and
// returns its stdout. It fails the test if the script exits nonzero.
func renderKrewManifest(t *testing.T, sums string) string {
	t.Helper()
	out, stderr, err := runKrewRenderer(t, sums)
	if err != nil {
		t.Fatalf("render-krew-manifest.sh: %v\nstderr: %s", err, stderr)
	}
	return out
}

func runKrewRenderer(t *testing.T, sums string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	sumsFile := filepath.Join(dir, "SHA256SUMS")
	if writeErr := os.WriteFile(sumsFile, []byte(sums), 0o644); writeErr != nil {
		t.Fatalf("write fixture SHA256SUMS: %v", writeErr)
	}
	cmd := exec.Command("scripts/render-krew-manifest.sh", krewTestVersion, sumsFile)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func parseKrewManifest(t *testing.T, rendered string) krewManifest {
	t.Helper()
	var m krewManifest
	if err := yaml.Unmarshal([]byte(rendered), &m); err != nil {
		t.Fatalf("rendered manifest does not parse as YAML: %v\n%s", err, rendered)
	}
	return m
}

func TestRenderKrewManifest_Identity(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	if want := "krew.googlecontainertools.github.com/v1alpha2"; m.APIVersion != want {
		t.Errorf("apiVersion = %q, want %q", m.APIVersion, want)
	}
	if m.Kind != "Plugin" {
		t.Errorf("kind = %q, want %q", m.Kind, "Plugin")
	}
	// krew's pluginNameToBin prefixes the plugin name with "kubectl-" when it
	// creates the symlink, so a kubectl-prefixed name here would install as
	// kubectl-kubectl-kubeagent. krew's validator accepts that name, so
	// nothing upstream catches the mistake.
	if m.Metadata.Name != "kubeagent" {
		t.Errorf("metadata.name = %q, want %q", m.Metadata.Name, "kubeagent")
	}
	if m.Spec.Version != krewTestVersion {
		t.Errorf("spec.version = %q, want %q", m.Spec.Version, krewTestVersion)
	}
	// krew rejects a short description containing a line break.
	if strings.ContainsAny(m.Spec.ShortDescription, "\n\r") {
		t.Errorf("shortDescription = %q, want no line breaks", m.Spec.ShortDescription)
	}
	if m.Spec.ShortDescription == "" {
		t.Error("shortDescription is empty; krew requires one")
	}
}

func TestRenderKrewManifest_EveryPlatformGetsItsOwnURIAndChecksum(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	if len(m.Spec.Platforms) != 4 {
		t.Fatalf("len(spec.platforms) = %d, want 4", len(m.Spec.Platforms))
	}

	seen := map[string]bool{}
	for i, p := range m.Spec.Platforms {
		key := p.Selector.MatchLabels["os"] + "_" + p.Selector.MatchLabels["arch"]
		if _, ok := krewTestSums[key]; !ok {
			t.Errorf("platform %d: unexpected selector %v", i, p.Selector.MatchLabels)
			continue
		}
		if seen[key] {
			t.Errorf("platform %s appears more than once", key)
		}
		seen[key] = true

		wantURI := "https://github.com/imantaba/kubeagent/releases/download/" +
			krewTestVersion + "/kubeagent_" + krewTestVersion + "_" + key + ".tar.gz"
		if p.URI != wantURI {
			t.Errorf("platform %s: uri = %q, want %q", key, p.URI, wantURI)
		}
		if p.Sha256 != krewTestSums[key] {
			t.Errorf("platform %s: sha256 = %q, want %q", key, p.Sha256, krewTestSums[key])
		}
		if p.Bin != "kubeagent" {
			t.Errorf("platform %s: bin = %q, want %q", key, p.Bin, "kubeagent")
		}
		if len(p.Files) == 0 {
			t.Errorf("platform %s: files is empty; krew requires it unspecified or non-empty", key)
		}
	}
	if len(seen) != 4 {
		t.Errorf("covered platforms = %v, want all four of %v", seen, krewPlatformOrder)
	}
}

// The renderer must look each checksum up by archive filename. Rendering the
// same four checksums listed in a different order must produce byte-identical
// output; a renderer that read SHA256SUMS positionally would swap them.
func TestRenderKrewManifest_LooksChecksumsUpByNameNotLineOrder(t *testing.T) {
	ordered := renderKrewManifest(t, krewFixtureSums(krewPlatformOrder))
	shuffled := renderKrewManifest(t, krewFixtureSums(
		[]string{"darwin_arm64", "linux_amd64", "darwin_amd64", "linux_arm64"}))

	if ordered != shuffled {
		t.Errorf("reordering SHA256SUMS changed the manifest; checksums are being read by line position, not by filename\n--- ordered ---\n%s\n--- shuffled ---\n%s", ordered, shuffled)
	}
}

func TestRenderKrewManifest_LeavesNoPlaceholder(t *testing.T) {
	rendered := renderKrewManifest(t, krewFixtureSums(krewPlatformOrder))
	if strings.Contains(rendered, "{{") {
		t.Errorf("rendered manifest still contains an unsubstituted placeholder:\n%s", rendered)
	}
}

// A missing checksum must fail loudly. A manifest rendered with an empty
// sha256 fails at install time with an opaque verification error, which is
// exactly what krew's checksums exist to catch.
func TestRenderKrewManifest_FailsWhenAChecksumIsMissing(t *testing.T) {
	stdout, stderr, err := runKrewRenderer(t, krewFixtureSums(
		[]string{"linux_amd64", "linux_arm64", "darwin_amd64"})) // darwin_arm64 absent

	if err == nil {
		t.Fatalf("renderer succeeded with a missing checksum; stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "kubeagent_"+krewTestVersion+"_darwin_arm64.tar.gz") {
		t.Errorf("stderr = %q, want it to name the archive whose checksum is missing", stderr)
	}
}

func TestRenderKrewManifest_CaveatsStateFlagOrderingAndReadOnly(t *testing.T) {
	m := parseKrewManifest(t, renderKrewManifest(t, krewFixtureSums(krewPlatformOrder)))

	// krew prints caveats right after a successful install — the moment both
	// of these are relevant.
	for _, want := range []string{
		"kubectl kubeagent scan --context",
		"read-only",
		"--fix",
	} {
		if !strings.Contains(m.Spec.Caveats, want) {
			t.Errorf("spec.caveats does not mention %q:\n%s", want, m.Spec.Caveats)
		}
	}
}
