package cluster

import "testing"

func TestResolveKubeconfig_PrefersExplicitPath(t *testing.T) {
	if got := resolveKubeconfig("/tmp/my.kubeconfig"); got != "/tmp/my.kubeconfig" {
		t.Errorf("got %q, want the explicit path", got)
	}
}

func TestResolveKubeconfig_FallsBackToEnv(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/env.kubeconfig")
	if got := resolveKubeconfig(""); got != "/tmp/env.kubeconfig" {
		t.Errorf("got %q, want the KUBECONFIG value", got)
	}
}

func TestNewClient_BadPathReturnsError(t *testing.T) {
	if _, err := NewClient("/nonexistent/kubeconfig"); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}
