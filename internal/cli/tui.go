package cli

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/scan"
	"github.com/imantaba/kubeagent/internal/tui"
)

// tuiScanOptions is the default scan the TUI browses: exactly what bare
// `kubeagent scan` runs. The opt-in advisories stay opt-in — a browser that
// silently ran more checks than the command it mirrors would make its coverage
// claim untrue.
// Any warning about the environment goes to w, which the caller must resolve
// before the TUI takes the screen: a line written from inside the refresh
// closure would land underneath the alternate screen and repeat on every
// refresh.
func tuiScanOptions(namespace string, w io.Writer) scan.Options {
	return scan.Options{
		Namespace:               namespace,
		QuotaThreshold:          quotaThresholdFromEnv(w),
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

// bindTUIFlags declares tui's flags on cmd, writing into o. Like gate, tui
// declares its own small flag set rather than inheriting scan's: three flags
// is the whole surface, and there is no --output because a TUI seizes the
// terminal and is not redirectable. --namespace and -n are one flag with a
// shorthand (StringVarP), replacing the two separate StringVar calls the
// standard library needed to accept both spellings.
func bindTUIFlags(cmd *cobra.Command, o *tuiOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace to browse (default: all namespaces)")
}

// parseTUIFlags parses tui's command line without running it. It builds a
// throwaway command so the flag declarations have exactly one home. Pure: it
// contacts no cluster and writes nothing.
func parseTUIFlags(args []string) (tuiOptions, error) {
	var o tuiOptions
	cmd := &cobra.Command{Use: "tui", SilenceErrors: true, SilenceUsage: true}
	bindTUIFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
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

	// Resolved once, here, rather than per refresh: the environment does not
	// change while the TUI runs, and this is the last point at which a warning
	// line is still visible on the terminal.
	scanOpts := tuiScanOptions(o.namespace, os.Stderr)

	return tui.Run(context.Background(), tui.Options{
		Version: version,
		Scope:   scope,
		Scan: func(ctx context.Context) (tui.ScanSnapshot, error) {
			res, err := scan.Evaluate(ctx, client, scanOpts)
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

// newTUICommand builds `kubeagent tui`.
func newTUICommand() *cobra.Command {
	var o tuiOptions
	cmd := &cobra.Command{
		Use:           "tui",
		Short:         "Browse scan results interactively",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUIOpts(o)
		},
	}
	bindTUIFlags(cmd, &o)
	return cmd
}
