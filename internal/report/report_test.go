package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func sampleFindings() []diagnose.Finding {
	return []diagnose.Finding{
		{Pod: "default/web", Issue: "CrashLoopBackOff", Reason: "crashes", Evidence: "restartCount=14"},
	}
}

func TestPrint_TextIncludesPodAndIssue(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(sampleFindings(), "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "default/web") || !strings.Contains(out, "CrashLoopBackOff") {
		t.Errorf("text output missing pod or issue:\n%s", out)
	}
}

func TestPrint_TextNoFindings(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(nil, "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "No issues found") {
		t.Errorf("expected a clean no-issues message, got %q", buf.String())
	}
}

func TestPrint_JSONIsValidAndRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(sampleFindings(), "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got []diagnose.Finding
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output was not valid JSON: %v", err)
	}
	if len(got) != 1 || got[0].Issue != "CrashLoopBackOff" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestPrint_UnknownFormatErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := Print(nil, "xml", &buf); err == nil {
		t.Error("expected an error for unknown format")
	}
}

func TestPrint_EmptyFormatErrors(t *testing.T) {
	// Print's contract requires an explicit format; main supplies the default.
	var buf bytes.Buffer
	if err := Print(nil, "", &buf); err == nil {
		t.Error("expected an error for an empty format")
	}
}
