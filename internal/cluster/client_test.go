package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveKubeconfig_PrefersExplicitPath(t *testing.T) {
	got, err := resolveKubeconfig("/tmp/my.kubeconfig")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tmp/my.kubeconfig" {
		t.Errorf("got %q, want the explicit path", got)
	}
}

func TestResolveKubeconfig_FallsBackToEnv(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/env.kubeconfig")
	got, err := resolveKubeconfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tmp/env.kubeconfig" {
		t.Errorf("got %q, want the KUBECONFIG value", got)
	}
}

func TestNewClient_BadPathReturnsError(t *testing.T) {
	if _, err := NewClient("/nonexistent/kubeconfig", ""); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}

// twoContextKubeconfig writes a minimal kubeconfig with contexts "alpha" and
// "beta" (current-context: alpha) and returns its path.
func twoContextKubeconfig(t *testing.T) string {
	t.Helper()
	const cfg = `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: c-alpha
  cluster:
    server: https://alpha.example:6443
    insecure-skip-tls-verify: true
- name: c-beta
  cluster:
    server: https://beta.example:6443
    insecure-skip-tls-verify: true
contexts:
- name: alpha
  context: {cluster: c-alpha, user: u-alpha}
- name: beta
  context: {cluster: c-beta, user: u-beta}
users:
- name: u-alpha
  user: {token: fake-alpha}
- name: u-beta
  user: {token: fake-beta}
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestNewClient_SelectsNamedContext(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, "beta"); err != nil {
		t.Errorf("expected success selecting context %q, got %v", "beta", err)
	}
}

func TestNewClient_UnknownContextErrors(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, "ghost"); err == nil {
		t.Error("expected an error for a non-existent context, got nil")
	}
}

func TestNewClient_EmptyContextUsesCurrent(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, err := NewClient(path, ""); err != nil {
		t.Errorf("expected success using current-context, got %v", err)
	}
}
