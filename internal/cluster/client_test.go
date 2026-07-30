package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
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

// Outside a pod (no service-account env), NewInClusterOrKubeconfig must fall back
// to the kubeconfig path — here, a bogus path yields a load error, proving it took
// the kubeconfig branch rather than the in-cluster one.
func TestNewInClusterOrKubeconfig_FallsBackToKubeconfig(t *testing.T) {
	os.Unsetenv("KUBERNETES_SERVICE_HOST")
	if _, err := NewInClusterOrKubeconfig("/no/such/kubeconfig", ""); err == nil {
		t.Fatal("expected a kubeconfig load error in the fallback path, got nil")
	}
}

func TestNewDynamicClients_BuildsBothFromAKubeconfig(t *testing.T) {
	// Client construction contacts no API server: this passes with nothing running.
	path := twoContextKubeconfig(t)
	dyn, disco, err := NewDynamicClients(path, "beta")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dyn == nil {
		t.Error("dynamic client is nil")
	}
	if disco == nil {
		t.Error("discovery client is nil")
	}
}

func TestNewDynamicClients_UnknownContextIsAnError(t *testing.T) {
	path := twoContextKubeconfig(t)
	if _, _, err := NewDynamicClients(path, "does-not-exist"); err == nil {
		t.Fatal("expected an error for an unknown context, got nil")
	}
}

func TestNewDynamicClients_BadPathReturnsError(t *testing.T) {
	if _, _, err := NewDynamicClients("/nonexistent/kubeconfig", ""); err == nil {
		t.Fatal("expected an error for a missing kubeconfig, got nil")
	}
}

// client-go defaults every per-API-group client to a 5 QPS / burst 10 token
// bucket. CoreV1 carries nearly every read a scan makes, so that default meters
// the scan at 5 requests per second — measured at 2.42s versus 0.15s for the
// same, byte-identical output. QPS -1 disables the limiter and leaves shedding
// to the API server's Priority and Fairness.
func TestRestConfigDisablesTheClientSideRateLimiter(t *testing.T) {
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QPS != -1 {
		t.Errorf("QPS = %v, want -1 (limiter disabled)", cfg.QPS)
	}
}

func TestRestConfigHonoursKubeagentQPS(t *testing.T) {
	t.Setenv("KUBEAGENT_QPS", "25")
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QPS != 25 {
		t.Errorf("QPS = %v, want 25", cfg.QPS)
	}
}

func TestRestConfigHonoursKubeagentBurst(t *testing.T) {
	t.Setenv("KUBEAGENT_QPS", "25")
	t.Setenv("KUBEAGENT_BURST", "50")
	path := twoContextKubeconfig(t)
	cfg, err := restConfig(path, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Burst != 50 {
		t.Errorf("Burst = %v, want 50", cfg.Burst)
	}
}

// A bad knob must degrade to a working scan, not to an error and not to the
// throttled default.
func TestRestConfigIgnoresAnUnusableQPSValue(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-5", "", "Inf", "+Inf", "Infinity"} {
		t.Setenv("KUBEAGENT_QPS", v)
		path := twoContextKubeconfig(t)
		cfg, err := restConfig(path, "alpha")
		if err != nil {
			t.Fatalf("KUBEAGENT_QPS=%q: %v", v, err)
		}
		if cfg.QPS != -1 {
			t.Errorf("KUBEAGENT_QPS=%q gave QPS = %v, want -1", v, cfg.QPS)
		}
	}
}

func TestRestConfigIgnoresAnUnusableBurstValue(t *testing.T) {
	for _, v := range []string{"not-a-number", "0", "-5"} {
		t.Setenv("KUBEAGENT_QPS", "25")
		t.Setenv("KUBEAGENT_BURST", v)
		path := twoContextKubeconfig(t)
		cfg, err := restConfig(path, "alpha")
		if err != nil {
			t.Fatalf("KUBEAGENT_BURST=%q: %v", v, err)
		}
		if cfg.Burst != 0 {
			t.Errorf("KUBEAGENT_BURST=%q gave Burst = %v, want 0 (client-go's own default)", v, cfg.Burst)
		}
	}
}

// The in-cluster branch of NewInClusterOrKubeconfig builds its config from
// rest.InClusterConfig() and never reaches restConfig, so the limiter fix has to
// live in a helper both call. This test guards the helper directly: if someone
// inlines it back into restConfig, the daemon silently keeps the old limiter.
func TestApplyRateLimitsDisablesTheLimiterOnAnyConfig(t *testing.T) {
	cfg := &rest.Config{Host: "https://apiserver.example:6443"}
	applyRateLimits(cfg)
	if cfg.QPS != -1 {
		t.Errorf("QPS = %v, want -1", cfg.QPS)
	}
}
