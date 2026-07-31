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

// tuiOptions is `kubeagent tui`'s parsed command line. One field per flag, in
// declaration order. It exists so flag wiring is testable without a cluster:
// parseTUIFlags is pure, and runTUIOpts does the I/O.
type tuiOptions struct {
	kubeconfig  string
	contextName string
	namespace   string
}

// parseTUIFlags parses `kubeagent tui`'s command line. Pure: it contacts no
// cluster and writes nothing. Like gate, it declares its own small flag set
// rather than inheriting scan's: three flags is the whole surface, and there
// is no --output because a TUI seizes the terminal and is not redirectable.
func parseTUIFlags(args []string) (tuiOptions, error) {
	var o tuiOptions
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	fs.StringVar(&o.namespace, "namespace", "", "namespace to browse (default: all namespaces)")
	fs.StringVar(&o.namespace, "n", "", "namespace to browse (shorthand)")
	if err := fs.Parse(args); err != nil {
		return tuiOptions{}, err
	}
	return o, nil
}

// runTUIOpts serves `kubeagent tui`. o is the already-parsed command line, as
// produced by parseTUIFlags.
func runTUIOpts(o tuiOptions) error {
	client, err := cluster.NewClient(o.kubeconfig, o.contextName)
	if err != nil {
		return err
	}

	scope := "all namespaces"
	if o.namespace != "" {
		scope = "namespace " + o.namespace
	}

	return tui.Run(context.Background(), tui.Options{
		Version: version,
		Scope:   scope,
		Scan: func(ctx context.Context) (tui.ScanSnapshot, error) {
			res, err := scan.Evaluate(ctx, client, tuiScanOptions(o.namespace))
			if err != nil {
				return tui.ScanSnapshot{}, err
			}
			return tui.Snapshot(res, time.Now()), nil
		},
	})
}

func runTUI(args []string) error {
	o, err := parseTUIFlags(args)
	if err != nil {
		return err
	}
	return runTUIOpts(o)
}
