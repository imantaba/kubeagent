package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/scan"
)

var fixedNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

func TestNewCoverage_EmptySlicesMarshalAsArraysNotNull(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)

	blob, err := json.Marshal(cov)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	for _, key := range []string{"checksRun", "checksSkipped", "partial"} {
		v, ok := got[key]
		if !ok {
			t.Errorf("coverage is missing %q; a caller cannot tell an empty list from an absent one", key)
			continue
		}
		if _, isSlice := v.([]any); !isSlice {
			t.Errorf("coverage.%s = %v (%T), want a JSON array", key, v, v)
		}
	}
	if got["namespaceScope"] != "all namespaces" {
		t.Errorf("namespaceScope = %v, want %q", got["namespaceScope"], "all namespaces")
	}
	if got["collectedAt"] != "2026-07-27T12:00:00Z" {
		t.Errorf("collectedAt = %v, want RFC3339 UTC", got["collectedAt"])
	}
	if got["metricsServer"] != "not-checked" {
		t.Errorf("metricsServer = %v, want %q — a check that never ran must not claim absence",
			got["metricsServer"], "not-checked")
	}
}

func TestCoverage_NamespaceScopeNamesTheNamespace(t *testing.T) {
	cov := newCoverage("kind-example", "payments", fixedNow)
	if cov.NamespaceScope != "payments" {
		t.Errorf("NamespaceScope = %q, want %q", cov.NamespaceScope, "payments")
	}
}

func TestCoverage_MarkPartialCarriesResourceAndReason(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)
	cov.markPartial([]scan.ReadFailure{{Resource: "networkpolicies", Reason: "forbidden"}})

	if len(cov.Partial) != 1 {
		t.Fatalf("Partial = %v, want one entry", cov.Partial)
	}
	if cov.Partial[0].Resource != "networkpolicies" || cov.Partial[0].Why != "forbidden" {
		t.Errorf("Partial[0] = %+v, want {networkpolicies forbidden}", cov.Partial[0])
	}
}

func TestCoverage_MarkRunAndMarkSkippedAccumulate(t *testing.T) {
	cov := newCoverage("kind-example", "", fixedNow)
	cov.markRun("workloads", "services")
	cov.markSkipped("logs", "not requested")

	if len(cov.ChecksRun) != 2 || cov.ChecksRun[0] != "workloads" {
		t.Errorf("ChecksRun = %v, want [workloads services]", cov.ChecksRun)
	}
	if len(cov.ChecksSkipped) != 1 || cov.ChecksSkipped[0].Check != "logs" || cov.ChecksSkipped[0].Why != "not requested" {
		t.Errorf("ChecksSkipped = %v, want one {logs, not requested}", cov.ChecksSkipped)
	}
}
