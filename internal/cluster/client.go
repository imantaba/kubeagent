package cluster

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a Kubernetes clientset from a kubeconfig file.
// If kubeconfigPath is empty, it falls back to $KUBECONFIG, then ~/.kube/config.
func NewClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	path, err := resolveKubeconfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %q: %w", path, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}
	return clientset, nil
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
