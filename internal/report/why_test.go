package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func whyWorkload() inventory.Workload {
	return inventory.Workload{
		Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
		RootCause:           "node worker-2 (NotReady)",
		RootCauseConfidence: "high",
		RootCauseTrace: []inventory.Hypothesis{
			{Cause: "node worker-2 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod api-a is scheduled on it"},
			{Cause: "node worker-9 (NotReady)", Kind: "node", Verdict: inventory.VerdictRuledOut, Reason: "no pod of this workload is scheduled on it"},
			{Cause: "PVC data-0 (ProvisioningFailed)", Kind: "pvc", Verdict: inventory.VerdictOutranked, Reason: "node worker-2 (NotReady) is the stronger cause"},
		},
	}
}

func TestPrintWorkloadWhyRendersTrace(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	if err := printWorkload(whyWorkload(), now, false, true, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"      · considered node worker-2 (NotReady): attributed — pod api-a is scheduled on it\n",
		"      · considered node worker-9 (NotReady): ruled out — no pod of this workload is scheduled on it\n",
		"      · considered PVC data-0 (ProvisioningFailed): outranked — node worker-2 (NotReady) is the stronger cause\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPrintWorkloadWhyRendersTraceWithoutAttribution(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	wl := whyWorkload()
	wl.RootCause, wl.RootCauseConfidence = "", ""
	wl.RootCauseTrace = wl.RootCauseTrace[1:2] // only the ruled-out entry
	var b bytes.Buffer
	if err := printWorkload(wl, now, false, true, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "likely caused by") {
		t.Errorf("unattributed workload must not render a cause line:\n%s", out)
	}
	if !strings.Contains(out, "      · considered node worker-9 (NotReady): ruled out — no pod of this workload is scheduled on it\n") {
		t.Errorf("ruled-out trace must render even without an attribution:\n%s", out)
	}
}

func TestPrintWorkloadWithoutWhyOmitsTrace(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	if err := printWorkload(whyWorkload(), now, false, false, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	if strings.Contains(b.String(), "considered") {
		t.Errorf("trace rendered without --why:\n%s", b.String())
	}
}
