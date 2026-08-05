package cli

import (
	"encoding/json"
	"os"
	"testing"
)

// The Claude Code plugin manifest hard-codes the command line that starts the
// MCP server. Renaming or removing an `mcp` flag would leave the manifest
// syntactically valid and behaviourally broken: every install would fail at
// startup, with the error going to a subprocess's stderr where nobody looks.
//
// This runs the manifest's own arguments through the real flag parser, so the
// declarations in bindMCPFlags are the single source of truth. A duplicated
// list of flag names here would rot the same way the manifest does.
func TestPluginManifestMCPArgsParse(t *testing.T) {
	raw, err := os.ReadFile("../../.claude-plugin/plugin.json")
	if err != nil {
		t.Fatalf("reading the plugin manifest: %v", err)
	}
	var manifest struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parsing the plugin manifest: %v", err)
	}

	srv, ok := manifest.MCPServers["kubeagent"]
	if !ok {
		t.Fatal("the plugin manifest declares no kubeagent MCP server")
	}
	if srv.Command != "kubeagent" {
		t.Fatalf("command = %q, want %q", srv.Command, "kubeagent")
	}
	if len(srv.Args) == 0 || srv.Args[0] != "mcp" {
		t.Fatalf("args = %v, want the first element to be %q", srv.Args, "mcp")
	}

	opts, err := parseMCPFlags(srv.Args[1:])
	if err != nil {
		t.Fatalf("the manifest's flags do not parse: %v\n"+
			"args were %v — fix .claude-plugin/plugin.json to match bindMCPFlags",
			err, srv.Args)
	}

	// The two flags the design chose deliberately. If a future change turns
	// either off, that is a behaviour change for every installed plugin and
	// should be a deliberate edit here, not a silent one in the manifest.
	if !opts.allowContextSwitch {
		t.Error("the manifest no longer passes --allow-context-switch")
	}
	if !opts.logs {
		t.Error("the manifest no longer passes --logs")
	}
}
