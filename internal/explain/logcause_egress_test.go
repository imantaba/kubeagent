package explain

import (
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// LogCause is one of logscan's fixed classifier strings — every signature
// discards its submatches — so it cannot carry an in-cluster address today.
// These two tests hold the boundary rather than the classifier: they assert
// that the two places a prompt is assembled redact what they are handed,
// whatever built it. leakedAddr is therefore a shape production no longer
// emits, which is the point — the tests fail if either boundary stops
// redacting, before a future signature makes that shape reachable again.
//
// Found by the cross-version chaos matrix: scenario 14's egress assertion went
// red on a real runner when the broken workload logged a connection refused to
// a Service ClusterIP before the daemon explained it.
const leakedAddr = "10.96.14.203:80"

func workloadWithLogCause() inventory.Workload {
	return inventory.Workload{
		Namespace: "chaos-explain", Name: "web", Kind: "Deployment",
		Ready: 0, Desired: 2, Status: "CrashLoopBackOff", Restarts: 5,
		Findings: []diagnose.Finding{{
			Issue: "CrashLoopBackOff", Reason: "Error", Evidence: "restarted 5 times",
			LogCause: "cannot reach a dependency (" + leakedAddr + ") — connection refused",
		}},
	}
}

func TestIncidentPromptDoesNotCarryTheLoggedAddress(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/chaos-explain/web", []string{"CrashLoopBackOff"},
		clusterhealth.ClusterHealth{}, []inventory.Workload{workloadWithLogCause()}, nil)
	if strings.Contains(p, leakedAddr) {
		t.Errorf("incident prompt carries the address read from a container log: %q", leakedAddr)
	}
	// The diagnostic value must survive the redaction — a prompt that lost the
	// cause entirely would pass this test while making the feature worse.
	if !strings.Contains(p, "cannot reach a dependency") {
		t.Error("incident prompt dropped the log cause instead of redacting the address")
	}
}

func TestInventoryPromptDoesNotCarryTheLoggedAddress(t *testing.T) {
	p := BuildInventoryPrompt(clusterhealth.ClusterHealth{}, nil, nil, nil,
		[]inventory.Workload{workloadWithLogCause()})
	if strings.Contains(p, leakedAddr) {
		t.Errorf("scan --explain prompt carries the address read from a container log: %q", leakedAddr)
	}
	if !strings.Contains(p, "cannot reach a dependency") {
		t.Error("scan --explain prompt dropped the log cause instead of redacting the address")
	}
}
