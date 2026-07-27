package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
	return config, nil
}

// NewInClusterOrKubeconfig builds a clientset from the in-cluster service-account
// when running inside a pod; otherwise it falls back to NewClient(kubeconfig,
// context) for local development.
func NewInClusterOrKubeconfig(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return kubernetes.NewForConfig(cfg)
	} else if err != rest.ErrNotInCluster {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}
	return NewClient(kubeconfigPath, contextName)
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
