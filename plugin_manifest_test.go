package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The Claude Code plugin manifests are hand-written JSON that makes promises
// about kubeagent: which binary to run, which flags it accepts, which version
// it is. Nothing in the Go build would otherwise notice when one of those
// promises stops being true, so these tests are the only thing standing
// between a renamed flag and a plugin that fails on every call.

const (
	pluginManifestPath      = ".claude-plugin/plugin.json"
	marketplaceManifestPath = ".claude-plugin/marketplace.json"
	chartPath               = "deploy/helm/kubeagent/Chart.yaml"
)

type pluginManifest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	MCPServers  map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"mcpServers"`
}

type marketplaceManifest struct {
	Name    string `json:"name"`
	Plugins []struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		Description string `json:"description"`
	} `json:"plugins"`
}

// readPluginManifest parses .claude-plugin/plugin.json or fails the test.
// Tasks 3-5 reuse it.
func readPluginManifest(t *testing.T) pluginManifest {
	t.Helper()
	raw, err := os.ReadFile(pluginManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", pluginManifestPath, err)
	}
	var m pluginManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parsing %s: %v", pluginManifestPath, err)
	}
	return m
}

func TestPluginManifestShape(t *testing.T) {
	m := readPluginManifest(t)

	if m.Name != "kubeagent" {
		t.Errorf("name = %q, want %q", m.Name, "kubeagent")
	}
	if m.Description == "" {
		t.Error("description is empty; it is what the marketplace listing shows")
	}
	if m.Version == "" {
		t.Error("version is empty")
	}

	srv, ok := m.MCPServers["kubeagent"]
	if !ok {
		t.Fatalf("mcpServers has no %q entry; got keys %v", "kubeagent", mapKeys(m.MCPServers))
	}
	if srv.Command != "kubeagent" {
		t.Errorf("mcpServers.kubeagent.command = %q, want %q", srv.Command, "kubeagent")
	}
	if len(srv.Args) == 0 || srv.Args[0] != "mcp" {
		t.Errorf("mcpServers.kubeagent.args = %v, want it to start with %q", srv.Args, "mcp")
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPluginVersionMatchesChart pins the manifest to the chart's appVersion,
// which scripts/bump-version.sh treats as the single source of truth. A
// release that bumps every other version reference and forgets the plugin
// fails here rather than shipping a manifest claiming the previous release.
func TestPluginVersionMatchesChart(t *testing.T) {
	raw, err := os.ReadFile(chartPath)
	if err != nil {
		t.Fatalf("reading %s: %v", chartPath, err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatalf("parsing %s: %v", chartPath, err)
	}
	want := chart.AppVersion
	if want == "" {
		t.Fatalf("%s has no appVersion", chartPath)
	}
	// The chart spells it "v1.2.0"; the plugin manifest spells it "1.2.0".
	want = want[1:]

	if got := readPluginManifest(t).Version; got != want {
		t.Errorf("plugin.json version = %q, chart appVersion implies %q\n"+
			"run scripts/bump-version.sh, or fix the manifest by hand", got, want)
	}
}

// TestMarketplaceEntryResolves checks that the marketplace points at a
// directory that actually holds a plugin manifest. A typo'd source path
// produces a marketplace that installs nothing, with no other signal.
func TestMarketplaceEntryResolves(t *testing.T) {
	raw, err := os.ReadFile(marketplaceManifestPath)
	if err != nil {
		t.Fatalf("reading %s: %v", marketplaceManifestPath, err)
	}
	var mp marketplaceManifest
	if err := json.Unmarshal(raw, &mp); err != nil {
		t.Fatalf("parsing %s: %v", marketplaceManifestPath, err)
	}
	if mp.Name == "" {
		t.Error("marketplace name is empty")
	}
	if len(mp.Plugins) != 1 {
		t.Fatalf("want exactly 1 plugin entry, got %d", len(mp.Plugins))
	}

	entry := mp.Plugins[0]
	if entry.Name != "kubeagent" {
		t.Errorf("plugin entry name = %q, want %q", entry.Name, "kubeagent")
	}
	if entry.Description == "" {
		t.Error("plugin entry description is empty")
	}

	manifest := filepath.Join(entry.Source, ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Errorf("marketplace source %q does not resolve to a plugin: %v", entry.Source, err)
	}
}

// pluginDocFiles is the explicit list of shipped skill and command files.
// It is a required-paths list, not a directory walk: a walk would pass
// vacuously if the directory went missing, which is the failure most worth
// catching. Tasks that add a skill or command add its path here.
var pluginDocFiles = []string{
	"skills/triaging-a-cluster/SKILL.md",
	"skills/reading-kubeagent-findings/SKILL.md",
	"commands/triage.md",
	"commands/why.md",
	"commands/preflight.md",
}

// toolNameRE matches an MCP tool name wherever it appears — in a Go
// registration or in prose.
var toolNameRE = regexp.MustCompile(`\b(kubeagent_[a-z_]+|list_contexts)\b`)

// registrationRE matches only the Name field of a tool registration, so
// prose inside internal/mcp (a coverage "why" string naming another tool,
// for instance) is not mistaken for a registration.
var registrationRE = regexp.MustCompile(`\bName:\s*"(kubeagent_[a-z_]+|list_contexts)"`)

// registeredMCPTools derives the set of tool names internal/mcp actually
// registers, by reading the source. Deriving it beats hard-coding it: a tool
// renamed in Go is then a test failure in the docs, not a silent lie.
func registeredMCPTools(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir("internal/mcp")
	if err != nil {
		t.Fatalf("reading internal/mcp: %v", err)
	}
	tools := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("internal/mcp", name))
		if err != nil {
			t.Fatalf("reading internal/mcp/%s: %v", name, err)
		}
		for _, m := range registrationRE.FindAllStringSubmatch(string(raw), -1) {
			tools[m[1]] = true
		}
	}
	if len(tools) == 0 {
		t.Fatal("found no registered MCP tools in internal/mcp; the registration " +
			"pattern changed and this test no longer checks anything")
	}
	return tools
}

// TestShippedDocsNameOnlyRegisteredTools fails closed on the documentation
// side: a skill or command that tells the model to call a tool kubeagent no
// longer registers breaks the build. That is the drift that costs a user
// something — the model follows the instruction and the call errors.
func TestShippedDocsNameOnlyRegisteredTools(t *testing.T) {
	registered := registeredMCPTools(t)

	for _, path := range pluginDocFiles {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading %s: %v", path, err)
			continue
		}
		mentioned := toolNameRE.FindAllString(string(raw), -1)
		if len(mentioned) == 0 {
			t.Errorf("%s names no MCP tool at all; it is supposed to teach the "+
				"model how to call them", path)
			continue
		}
		for _, name := range mentioned {
			if !registered[name] {
				t.Errorf("%s names tool %q, which internal/mcp does not register "+
					"(registered: %v)", path, name, sortedKeys(registered))
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
