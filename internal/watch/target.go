package watch

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
)

// defaultClusterName labels the target built without an explicit --context: the
// in-cluster ServiceAccount, or the kubeconfig's current-context outside a
// cluster.
const defaultClusterName = "local"

// Target is one cluster the daemon watches. The name is the operator's label for
// it — the --context they typed, or --cluster-name for the default target — and
// it becomes the cluster label on every metric series and the cluster field on
// every issue, explanation and alert.
type Target struct {
	Name   string
	Client kubernetes.Interface
}

// validateTargets rejects a target list that cannot produce a coherent daemon.
// Every failure here is a configuration error, and every one of them is fatal:
// an operator who asked for three clusters and got two silently is worse off
// than one whose daemon refused to start.
func validateTargets(targets []Target) error {
	if len(targets) == 0 {
		return fmt.Errorf("no clusters to watch")
	}
	seen := make(map[string]bool, len(targets))
	for _, t := range targets {
		if t.Name == "" {
			return fmt.Errorf("a cluster name cannot be empty")
		}
		if t.Client == nil {
			return fmt.Errorf("cluster %q has no client", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate cluster name %q: every watched cluster needs a distinct name, because the name is the metric label", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// targetNames returns the target names in input order; newMetrics sorts them.
func targetNames(targets []Target) []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	return names
}
