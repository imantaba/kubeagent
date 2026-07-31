package cli

import (
	"context"
	"flag"
	"time"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/tui"
)

// tuiScanOptions is the default scan the TUI browses: exactly what bare
// `kubeagent scan` runs. The opt-in advisories stay opt-in — a browser that
// silently ran more checks than the command it mirrors would make its coverage
// claim untrue.
func tuiScanOptions(namespace string) scan.Options {
	return scan.Options{
		Namespace:               namespace,
		QuotaThreshold:          envFloat("KUBEAGENT_QUOTA_THRESHOLD", 0.90),
		WebhookTimeoutThreshold: int32(envInt("KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS", 15)),
	}
}

// runTUI serves `kubeagent tui`. Like gate, it declares its own small flag set
// rather than inheriting scan's: three flags is the whole surface, and there is
// no --output because a TUI seizes the terminal and is not redirectable.
func runTUI(args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	contextName := fs.String("context", "", "kubeconfig context to use (default: current-context)")
	var namespace string
	fs.StringVar(&namespace, "namespace", "", "namespace to browse (default: all namespaces)")
	fs.StringVar(&namespace, "n", "", "namespace to browse (shorthand)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := cluster.NewClient(*kubeconfig, *contextName)
	if err != nil {
		return err
	}

	scope := "all namespaces"
	if namespace != "" {
		scope = "namespace " + namespace
	}

	return tui.Run(context.Background(), tui.Options{
		Version: version,
		Scope:   scope,
		Scan: func(ctx context.Context) (tui.ScanSnapshot, error) {
			res, err := scan.Evaluate(ctx, client, tuiScanOptions(namespace))
			if err != nil {
				return tui.ScanSnapshot{}, err
			}
			return tui.Snapshot(res, time.Now()), nil
		},
	})
}
