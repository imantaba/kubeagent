package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_NoArgsReturnsUsage(t *testing.T) {
	if err := run(nil); err == nil {
		t.Fatal("expected a usage error with no args")
	}
}

func TestRun_RejectsUnknownSubcommand(t *testing.T) {
	if err := run([]string{"explode"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}

func TestRun_RejectsBadOutputFormat(t *testing.T) {
	// This must fail on validation BEFORE any cluster connection is attempted.
	if err := run([]string{"scan", "--output", "bogus"}); err == nil {
		t.Fatal("expected an error for a bad --output value")
	}
}

func TestRun_ExplainRequiresAPIKey(t *testing.T) {
	// --explain without a key must fail fast, before any cluster connection.
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := run([]string{"scan", "--explain"})
	if err == nil {
		t.Fatal("expected an error when --explain is set without ANTHROPIC_API_KEY")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected error to mention ANTHROPIC_API_KEY, got: %v", err)
	}
}

func TestRun_ModelFlagIsRecognized(t *testing.T) {
	// --model must be a known flag: with it set and no API key, the error is
	// the fail-fast key error, NOT "flag provided but not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := run([]string{"scan", "--explain", "--model", "claude-sonnet-4-6"})
	if err == nil {
		t.Fatal("expected the fail-fast API-key error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error (proves --model parsed), got: %v", err)
	}
}

func TestRun_IncludeFlagsAreRecognized(t *testing.T) {
	// --include-cron / --include-restarts must be known flags: with --explain and
	// no key, the error is the fail-fast key error, not "flag not defined".
	t.Setenv("ANTHROPIC_API_KEY", "")
	err := run([]string{"scan", "--explain", "--include-cron", "--include-restarts"})
	if err == nil {
		t.Fatal("expected the fail-fast API-key error")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("expected ANTHROPIC_API_KEY error (proves the flags parsed), got: %v", err)
	}
}

func TestVersionLine(t *testing.T) {
	// In tests the binary isn't ldflags-stamped, so version is the "dev" default.
	if got := versionLine(); got != "kubeagent dev" {
		t.Errorf("versionLine() = %q, want %q", got, "kubeagent dev")
	}
}

func TestRun_Version(t *testing.T) {
	if err := run([]string{"version"}); err != nil {
		t.Errorf("run([version]) returned error: %v", err)
	}
}

func TestRun_DiagnosesUnreachableAPI(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	// A kubeconfig pointing at a port nothing listens on → loopback connection
	// refused (no external network). Exercises the connectivity diagnosis path.
	cfg := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:1
  name: dead
contexts:
- context:
    cluster: dead
    user: dead
  name: dead
current-context: dead
users:
- name: dead
  user: {}
`
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"scan", "--kubeconfig", kc})
	if err == nil {
		t.Fatal("expected an error for an unreachable API server")
	}
	out := err.Error()
	if !strings.Contains(out, "refused") || !strings.Contains(out, "details:") {
		t.Errorf("expected a connection-refused diagnosis with a details line, got: %v", out)
	}
}
