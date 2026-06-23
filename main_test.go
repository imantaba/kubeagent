package main

import (
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
