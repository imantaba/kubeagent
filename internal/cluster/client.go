package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes clientset from a kubeconfig file.
// If kubeconfigPath is empty, it falls back to $KUBECONFIG, then ~/.kube/config.
// If contextName is empty, the kubeconfig's current-context is used.
func NewClient(kubeconfigPath, contextName string) (*kubernetes.Clientset, error) {
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
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
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
