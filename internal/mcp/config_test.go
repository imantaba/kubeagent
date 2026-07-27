package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfig_MarshalNeverEchoesKubeconfigOrContext asserts on the marshalled
// JSON bytes, not on Go field values: a kubeconfig path names a customer, a
// cluster and an environment, and the doc comment's promise is only real if
// encoding/json is structurally prevented from emitting it.
func TestConfig_MarshalNeverEchoesKubeconfigOrContext(t *testing.T) {
	cfg := Config{
		Kubeconfig:         "/home/example/.kube/config",
		Context:            "example-context",
		AllowContextSwitch: true,
		Logs:               true,
	}

	blob, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if strings.Contains(string(blob), cfg.Kubeconfig) {
		t.Errorf("Marshal() = %s, contains the kubeconfig path", blob)
	}
	if strings.Contains(string(blob), cfg.Context) {
		t.Errorf("Marshal() = %s, contains the context name", blob)
	}
	if strings.Contains(string(blob), `"Kubeconfig"`) || strings.Contains(string(blob), `"Context"`) {
		t.Errorf("Marshal() = %s, contains a Kubeconfig/Context key; those fields must never be marshalled", blob)
	}
}
