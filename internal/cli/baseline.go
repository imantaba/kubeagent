package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/collect"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// baselineOptions is `kubeagent baseline capture`'s parsed command line. One
// field per flag, in declaration order, so flag wiring is testable without a
// cluster: parseBaselineCaptureFlags is pure and runBaselineCapture does the I/O.
type baselineOptions struct {
	kubeconfig  string
	contextName string
	namespace   string
	minPodAge   time.Duration
}

// bindBaselineCaptureFlags declares the flags on cmd, writing into o.
func bindBaselineCaptureFlags(cmd *cobra.Command, o *baselineOptions) {
	f := cmd.Flags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.contextName, "context", "", "kubeconfig context to use (default: current-context)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace to sample (default: all namespaces)")
	f.DurationVar(&o.minPodAge, "min-pod-age", envDur("KUBEAGENT_BASELINE_MIN_POD_AGE", baseline.DefaultMinPodAge),
		"ignore pods younger than this, which would otherwise imply wild rates (KUBEAGENT_BASELINE_MIN_POD_AGE)")
}

// parseBaselineCaptureFlags parses the command line. Pure: it reads the
// environment for the one env-defaulted flag and nothing else, contacts no
// cluster, and writes nothing. It builds a throwaway command so the flag
// declarations have exactly one home, in bindBaselineCaptureFlags.
func parseBaselineCaptureFlags(args []string) (baselineOptions, error) {
	var o baselineOptions
	cmd := &cobra.Command{Use: "capture", SilenceErrors: true, SilenceUsage: true}
	bindBaselineCaptureFlags(cmd, &o)
	if err := cmd.Flags().Parse(Normalize(args, longFlagLookup(cmd))); err != nil {
		return baselineOptions{}, err
	}
	return o, nil
}

// newBaselineCommand builds `kubeagent baseline capture`. Like `policy` and
// `rbac`, the parent keeps its own argument handling rather than a cobra Args
// helper, which would reword the usage error.
func newBaselineCommand() *cobra.Command {
	usage := func() error {
		return fmt.Errorf("usage: %s baseline capture [--kubeconfig path] [--context name] [-n namespace] [--min-pod-age dur]", invokedAs)
	}
	cmd := &cobra.Command{
		Use:           "baseline",
		Short:         "Work with restart-rate baselines",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return usage()
		},
	}
	var o baselineOptions
	capture := &cobra.Command{
		Use:   "capture",
		Short: "Print this cluster's restart-rate baseline as JSON",
		Long: "Print this cluster's restart-rate baseline as JSON, one learned rate per workload.\n\n" +
			"Read-only toward the cluster (list calls only), and it makes no model call — two\n" +
			"separate promises. Nothing is written to disk: redirect the output and review the\n" +
			"file before committing it, because it names your namespaces and workloads.\n\n" +
			"What the rates measure: restarts over the lifetimes of the pods running right now,\n" +
			"not long-term history. A workload whose pods were all recreated an hour ago shows\n" +
			"only what those pods have done since.",
		Example:       "  " + invokedAs + " baseline capture > cluster-baseline.json",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usage()
			}
			return runBaselineCapture(o, os.Stdout)
		},
	}
	bindBaselineCaptureFlags(capture, &o)
	cmd.AddCommand(capture)
	return cmd
}

// runBaselineCapture serves `kubeagent baseline capture`. Read-only toward the
// cluster: one CollectInventory call, which issues List calls only, all of them
// already in the scan RBAC profile's core rules — so this command needs no new
// grant. It makes no model call.
func runBaselineCapture(o baselineOptions, w io.Writer) error {
	if o.minPodAge < 0 {
		return fmt.Errorf("--min-pod-age must not be negative, got %s", o.minPodAge)
	}
	client, err := cluster.NewClient(o.kubeconfig, o.contextName)
	if err != nil {
		return err
	}
	in, err := collect.CollectInventory(context.Background(), client, o.namespace)
	if err != nil {
		return err
	}
	return renderBaselineCapture(in, o.minPodAge, time.Now(), w)
}

// renderBaselineCapture is the pure half: it turns collected inputs into the
// document bytes. Split out so the rendering is testable with no cluster and no
// clock of its own.
func renderBaselineCapture(in inventory.Inputs, minPodAge time.Duration, now time.Time, w io.Writer) error {
	doc := baseline.Capture(podSamples(in, now), minPodAge, now)
	b, err := doc.Marshal()
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// podSamples reduces a scan's raw pod list to the flat samples
// internal/baseline consumes, resolving each pod to its workload through
// inventory.PodOwners — the same rule the report's workload rollup uses.
//
// It reads the pod objects directly rather than inventory.Assemble's output on
// purpose: Prioritize drops healthy-quiet workloads and Assemble truncates a
// Job's pod list, and a baseline needs exactly the workloads and pods those two
// remove.
//
// A pod with no Status.StartTime is skipped. It has never started, so it has
// observed no container runtime, and counting its age would put seconds in the
// denominator during which nothing could have restarted — deflating its
// workload's rate.
func podSamples(in inventory.Inputs, now time.Time) []baseline.PodSample {
	owners := inventory.PodOwners(in)
	out := make([]baseline.PodSample, 0, len(in.Pods))
	for _, p := range in.Pods {
		if p.Status.StartTime == nil {
			continue
		}
		age := now.Sub(p.Status.StartTime.Time).Seconds()
		if age <= 0 {
			continue
		}
		o, ok := owners[p.Namespace+"/"+p.Name]
		if !ok {
			continue
		}
		restarts := 0
		for _, cs := range p.Status.ContainerStatuses {
			restarts += int(cs.RestartCount)
		}
		out = append(out, baseline.PodSample{
			Kind: o.Kind, Namespace: o.Namespace, Name: o.Name,
			Restarts: restarts, AgeSeconds: age,
		})
	}
	return out
}

// loadBaseline reads and parses a baseline document from path. It returns a
// nil document and nil error for an empty path, so `scan --baseline` and
// `gate --baseline` — its two callers, when the flag is left unset — get
// nothing back, rather than a zero-value document that would silently
// compare as "no restarts were ever expected".
//
// Parsing is a fallible step of its own, kept separate from any comparison
// that follows it: an unreadable or wrong-version file is bad input and
// should stop the caller before it does anything else. The path appears in
// the error, which reaches stderr only — the same carve-out --policy already
// has — and it never enters a report.
func loadBaseline(path string) (*baseline.Document, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	doc, err := baseline.Load(b)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// baselineReport compares pod samples against a loaded document. Nil when doc
// is nil, so `scan --baseline` and `gate --baseline` — its two callers —
// render nothing for it when the flag was left unset, rather than a report
// claiming every workload is a fresh deviation. A zero factor or floor takes
// the package default.
func baselineReport(doc *baseline.Document, factor, floor float64, in inventory.Inputs, now time.Time) *baseline.Report {
	if doc == nil {
		return nil
	}
	rep := baseline.Compare(*doc, podSamples(in, now), baseline.CompareOptions{Factor: factor, Floor: floor})
	return &rep
}
