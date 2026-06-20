package main

import "testing"

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
