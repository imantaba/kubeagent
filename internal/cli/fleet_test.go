package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/cluster"
)

func contexts() []cluster.ContextInfo {
	return []cluster.ContextInfo{
		{Name: "example-eu-1"},
		{Name: "example-eu-2", Current: true},
		{Name: "example-us-3"},
		{Name: "staging-1"},
	}
}

func TestSelectContexts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wanted      []string
		allContexts bool
		match       string
		want        []string
		wantErr     string
	}{
		{name: "no selection is the current context", want: []string{"example-eu-2"}},
		{name: "explicit contexts, in the order given",
			wanted: []string{"example-us-3", "example-eu-1"},
			want:   []string{"example-us-3", "example-eu-1"}},
		{name: "all contexts, sorted", allContexts: true,
			want: []string{"example-eu-1", "example-eu-2", "example-us-3", "staging-1"}},
		{name: "a glob filter", allContexts: true, match: "example-eu-*",
			want: []string{"example-eu-1", "example-eu-2"}},
		{name: "a glob crossing a slash is still one pattern", allContexts: true, match: "*-us-*",
			want: []string{"example-us-3"}},
		{name: "match without all-contexts", match: "example-*",
			wantErr: "--match needs --all-contexts"},
		{name: "context with all-contexts", wanted: []string{"example-eu-1"}, allContexts: true,
			wantErr: "--context and --all-contexts cannot be combined"},
		{name: "an unknown context", wanted: []string{"nowhere"},
			wantErr: `unknown context "nowhere"`},
		{name: "a glob matching nothing", allContexts: true, match: "nowhere-*",
			wantErr: "no context matches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectContexts(contexts(), tc.wanted, tc.allContexts, tc.match)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("selectContexts() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectContexts() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("selectContexts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A kubeconfig with no current context and no explicit selection has nothing to
// sweep — that is bad input, not an empty pass.
func TestSelectContextsWithNoCurrentContext(t *testing.T) {
	_, err := selectContexts([]cluster.ContextInfo{{Name: "example-a"}}, nil, false, "")
	if err == nil || !strings.Contains(err.Error(), "no context selected") {
		t.Errorf("error = %v, want it to say no context is selected", err)
	}
}

// Every selection error is exit 4 — bad input, before any cluster was touched.
func TestSelectContextsErrorsAreUsageErrors(t *testing.T) {
	_, err := selectContexts(contexts(), nil, false, "example-*")
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
}

func TestParseFleetFlags(t *testing.T) {
	o, err := parseFleetFlags([]string{
		"--all-contexts", "--match", "example-*", "--fail-on", "warning",
		"--workers", "3", "--cluster-timeout", "90s", "--output", "json", "-n", "example-ns",
	})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if !o.allContexts || o.match != "example-*" || o.failOn != "warning" ||
		o.workers != 3 || o.clusterTimeout.String() != "1m30s" ||
		o.output != "json" || o.namespace != "example-ns" {
		t.Errorf("parsed = %+v, want every flag to reach its field", o)
	}
}

// The single-dash long-flag spelling the standard library accepted still works,
// because Normalize rewrites it before pflag sees it.
func TestParseFleetFlagsAcceptsSingleDashLongNames(t *testing.T) {
	o, err := parseFleetFlags([]string{"-output", "json", "-fail-on", "info"})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if o.output != "json" || o.failOn != "info" {
		t.Errorf("parsed = %+v, want the single-dash spelling to reach the fields", o)
	}
}

func TestParseFleetFlagsRejectsAnUnknownOutput(t *testing.T) {
	o, err := parseFleetFlags([]string{"--output", "yaml"})
	if err != nil {
		t.Fatalf("parseFleetFlags() error = %v", err)
	}
	if err := validateFleetOptions(o); err == nil || !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error = %v, want it to name the unsupported format", err)
	} else if exitCodeFor(err) != 4 {
		t.Errorf("exit code = %d, want 4", exitCodeFor(err))
	}
}

// A non-positive --cluster-timeout is refused rather than accepted as "no
// deadline". fleet.Sweep attaches a deadline only when the budget is positive,
// and internal/parallel's pool returns only after every worker returns — so a
// single cluster whose API server accepts the connection and then never answers
// would block the whole sweep forever, rendering nothing at all for any cluster.
// A hang with no output is a worse answer than an error, so the CLI refuses the
// value that makes it possible. `gate` already applies the same guard to
// --poll-interval.
func TestValidateFleetOptionsRejectsANonPositiveClusterTimeout(t *testing.T) {
	for _, spelling := range []string{"0", "-1s"} {
		o, err := parseFleetFlags([]string{"--cluster-timeout", spelling})
		if err != nil {
			t.Fatalf("parseFleetFlags(--cluster-timeout %s) error = %v", spelling, err)
		}
		err = validateFleetOptions(o)
		if err == nil || !strings.Contains(err.Error(), "--cluster-timeout") {
			t.Errorf("--cluster-timeout %s: error = %v, want it to name the flag", spelling, err)
			continue
		}
		if exitCodeFor(err) != 4 {
			t.Errorf("--cluster-timeout %s: exit code = %d, want 4", spelling, exitCodeFor(err))
		}
	}
}

// The default is positive, so an operator who passes no budget at all is never
// refused. Without this the guard above could be satisfied by rejecting every
// value.
func TestValidateFleetOptionsAcceptsTheDefaultClusterTimeout(t *testing.T) {
	o, err := parseFleetFlags(nil)
	if err != nil {
		t.Fatalf("parseFleetFlags(nil) error = %v", err)
	}
	if o.clusterTimeout <= 0 {
		t.Fatalf("default --cluster-timeout = %v, want a positive budget", o.clusterTimeout)
	}
	if err := validateFleetOptions(o); err != nil {
		t.Errorf("validateFleetOptions(defaults) = %v, want nil", err)
	}
}

// TestBuildFleetTargetsNaming pins the one naming rule buildFleetTargets has:
// one client per selected context, in the order given. It reuses the same
// hermetic fixture watch's TestBuildTargetsNaming uses — every server in it is
// an unreachable loopback address, so building a clientset (which dials
// nothing) stays instant and touches no network.
func TestBuildFleetTargetsNaming(t *testing.T) {
	kc := multiContextKubeconfigPath(t)

	targets, err := buildFleetTargets(kc, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("buildFleetTargets: %v", err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.Name)
		if tg.Client == nil {
			t.Errorf("target %q has a nil client", tg.Name)
		}
	}
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("target names = %v, want %v", got, want)
	}
}

// TestBuildFleetTargetsRejectsAnUnknownContext pins the last selection rule in
// the brief's table: a cluster.NewClient failure is a usage error (exit 4)
// naming the context. selectContexts already keeps an unknown name from
// reaching here in the ordinary run, so this drives buildFleetTargets directly
// — the same way watch's TestBuildTargetsRejectsAnUnknownContext drives
// buildTargets directly.
func TestBuildFleetTargetsRejectsAnUnknownContext(t *testing.T) {
	kc := multiContextKubeconfigPath(t)

	_, err := buildFleetTargets(kc, []string{"nope"})
	if err == nil {
		t.Fatal("buildFleetTargets accepted a context that is not in the kubeconfig")
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error = %v, want it to name the context", err)
	}
	if got := exitCodeFor(err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
}
