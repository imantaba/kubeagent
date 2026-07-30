package cluster

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/imantaba/kubeagent/internal/redact"
)

// NewClient builds a Kubernetes clientset from a kubeconfig file.
// If kubeconfigPath is empty, it falls back to $KUBECONFIG, then ~/.kube/config.
// If contextName is empty, the kubeconfig's current-context is used.
func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	config, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
}

// NewDynamicClients builds the dynamic and discovery clients for the same
// kubeconfig/context NewClient would use — the pair `scan --operators` needs to
// read custom resources it was not compiled against. Contacts no API server.
func NewDynamicClients(kubeconfigPath, contextName string) (dynamic.Interface, discovery.DiscoveryInterface, error) {
	config, err := restConfig(kubeconfigPath, contextName)
	if err != nil {
		return nil, nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("creating discovery client: %w", err)
	}
	return dyn, disco, nil
}

// restConfig resolves the kubeconfig path and context into a REST config. It is
// the single place that resolution lives, so every client kubeagent builds
// honours --kubeconfig and --context identically.
func restConfig(kubeconfigPath, contextName string) (*rest.Config, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = path
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		if contextName == "" {
			return nil, fmt.Errorf("loading kubeconfig %q: %w", path, err)
		}
		return nil, fmt.Errorf("loading kubeconfig %q (context %q): %w", path, contextName, err)
	}
	applyRateLimits(config)
	return config, nil
}

// applyRateLimits sets the client-side request rate for every client kubeagent
// builds. The default is no client-side limit at all.
//
// client-go installs a 5 QPS / burst 10 token bucket on each per-API-group
// client when QPS is left at zero. CoreV1 carries nearly every read a scan
// makes, so that default meters the whole scan: measured on a three-node
// cluster, a scan with every add-on enabled took 6.01s with the limiter and
// 0.12s without, one read at a time in both, for byte-identical output. Under
// the limiter the worker pool buys nothing — eight workers finish in the same
// 6.01s as one — so the two changes ship together. A client-side rate holds the
// same number whether the API server is idle or dying, while the server's own
// Priority and Fairness (flowcontrol.apiserver.k8s.io/v1, GA) sheds load based
// on what it can actually take. QPS -1 disables the limiter entirely.
//
// KUBEAGENT_QPS restores a client-side limit for anyone who needs one — a
// shared cluster with a strict admission budget, a debugging session. A value
// that does not parse, is not positive, or is not finite, is ignored: a bad
// knob degrades to a working scan, never to an error. "Inf"/"+Inf"/"Infinity"
// parse successfully and are > 0, so the finiteness check is not redundant
// with the positivity check — without it, client-go would build a token-bucket
// limiter with an IEEE infinite rate rather than leaving the limiter disabled.
// KUBEAGENT_BURST only takes effect alongside KUBEAGENT_QPS, because with the
// limiter disabled there is no bucket to size; left unset, client-go applies
// its own default burst.
func applyRateLimits(config *rest.Config) {
	config.QPS = -1
	if s := os.Getenv("KUBEAGENT_QPS"); s != "" {
		if v, err := strconv.ParseFloat(s, 32); err == nil && v > 0 && !math.IsInf(v, 0) {
			config.QPS = float32(v)
		}
	}
	if s := os.Getenv("KUBEAGENT_BURST"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			config.Burst = v
		}
	}
}

// NewInClusterOrKubeconfig builds a clientset from the in-cluster service-account
// when running inside a pod; otherwise it falls back to NewClient(kubeconfig,
// context) for local development.
func NewInClusterOrKubeconfig(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		applyRateLimits(cfg) // this branch never reaches restConfig
		return kubernetes.NewForConfig(cfg)
	} else if err != rest.ErrNotInCluster {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	return NewClient(kubeconfigPath, contextName)
}

// ContextInfo describes one kubeconfig context, with the API server URL
// already reduced to scheme://host.
type ContextInfo struct {
	Name    string
	Cluster string
	Server  string
	Current bool
}

// Contexts lists the contexts a kubeconfig defines. It deliberately never
// includes the kubeconfig path, not in the result and not in its errors: a
// path like ~/.kube/customer-acme-prod names a customer, a cluster and an
// environment, and this list is served to a remote caller.
func Contexts(kubeconfigPath string) ([]ContextInfo, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, errors.New("locating the kubeconfig")
	}
	raw, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, errors.New("loading the kubeconfig")
	}

	out := make([]ContextInfo, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		info := ContextInfo{Name: name, Cluster: c.Cluster, Current: name == raw.CurrentContext}
		if cl, ok := raw.Clusters[c.Cluster]; ok {
			info.Server = redact.URL(cl.Server)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func resolveKubeconfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory for default kubeconfig: %w", err)
	}
	return filepath.Join(home, ".kube", "config"), nil
}
