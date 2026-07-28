package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const kubeconfigFixture = `apiVersion: v1
kind: Config
current-context: staging
clusters:
  - name: staging-cluster
    cluster:
      server: https://staging.example.com:6443/some/path?token=<PLACEHOLDER>
  - name: prod-cluster
    cluster:
      server: https://prod.example.com:6443
contexts:
  - name: staging
    context:
      cluster: staging-cluster
      user: staging-user
  - name: prod
    context:
      cluster: prod-cluster
      user: prod-user
users:
  - name: staging-user
    user: {}
  - name: prod-user
    user: {}
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(kubeconfigFixture), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func TestContexts_ListsEveryContextSortedWithTheCurrentOneMarked(t *testing.T) {
	got, err := Contexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("Contexts() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Contexts() returned %d contexts, want 2", len(got))
	}
	if got[0].Name != "prod" || got[1].Name != "staging" {
		t.Errorf("names = %q, %q; want them sorted", got[0].Name, got[1].Name)
	}
	if got[0].Current {
		t.Error("prod is marked current; staging is current-context")
	}
	if !got[1].Current {
		t.Error("staging is not marked current")
	}
	if got[1].Cluster != "staging-cluster" {
		t.Errorf("Cluster = %q, want %q", got[1].Cluster, "staging-cluster")
	}
}

func TestContexts_ServerIsReducedToSchemeAndHost(t *testing.T) {
	got, err := Contexts(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("Contexts() error = %v", err)
	}
	for _, c := range got {
		if c.Server != "https://staging.example.com:6443" && c.Server != "https://prod.example.com:6443" {
			t.Errorf("Server = %q; an API server URL may carry no more than scheme://host", c.Server)
		}
	}
}

func TestContexts_MissingFileErrorDoesNotEchoThePath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "customer-acme-prod-kubeconfig")

	_, err := Contexts(secret)
	if err == nil {
		t.Fatal("Contexts() error = nil, want a load failure")
	}
	if filepath.Base(secret) != "" && strings.Contains(err.Error(), filepath.Base(secret)) {
		t.Errorf("error = %q; a kubeconfig path names a customer and an environment and must not be echoed", err)
	}
}
