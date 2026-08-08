package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/cluster"
	"github.com/imantaba/kubeagent/internal/fleet"
	"github.com/imantaba/kubeagent/internal/fleetfile"
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

// selectEntries preserves FILE ORDER — unlike selectContexts, which sorts,
// because a kubeconfig's context list has no order an operator chose. A fleet
// file does: the order the operator wrote. Sweep sorts the report anyway, so
// this only decides which clusters go to which worker.
func TestSelectEntries(t *testing.T) {
	all := []fleetfile.Entry{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Context: "default"},
		{Name: "prod-us", Context: "prod-us"},
		{Name: "edge-b", Context: "default"},
	}

	tests := []struct {
		name    string
		match   string
		want    []string // resolved names, in the order selectEntries returns them
		wantErr string
	}{
		{
			name: "no match takes every entry in file order",
			want: []string{"prod-eu", "edge-a", "prod-us", "edge-b"},
		},
		{
			name:  "a match takes the subset in file order",
			match: "prod-*",
			want:  []string{"prod-eu", "prod-us"},
		},
		{
			name:  "the match runs against the row identity, not the context",
			match: "edge-*",
			want:  []string{"edge-a", "edge-b"},
		},
		{
			name:    "a match selecting nothing is refused",
			match:   "staging-*",
			wantErr: "no cluster matches --match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectEntries(all, tt.match)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("selectEntries() error = nil, want one containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("selectEntries() error = %q, want it to contain %q", err, tt.wantErr)
				}
				if code := exitCodeFor(err); code != 4 {
					t.Errorf("exit code = %d, want 4 — bad input, found before any cluster was touched", code)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectEntries() error = %v, want none", err)
			}
			var names []string
			for _, e := range got {
				names = append(names, e.Name)
			}
			if !reflect.DeepEqual(names, tt.want) {
				t.Errorf("selectEntries() = %v, want %v", names, tt.want)
			}
		})
	}
}

// The whole flag-conflict matrix from the spec. --fleet-file names the
// clusters, so a flag that also names them is refused; a flag that says how to
// reach them or which subset to take is not.
func TestValidateFleetOptionsFleetFileConflicts(t *testing.T) {
	base := func() fleetOptions {
		return fleetOptions{
			fleetFile:      "/fleet/clusters.yaml",
			failOn:         "critical",
			output:         "text",
			clusterTimeout: 60 * time.Second,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*fleetOptions)
		wantErr string
	}{
		{
			name:    "--fleet-file and --context are refused",
			mutate:  func(o *fleetOptions) { o.contexts = []string{"prod-eu"} },
			wantErr: "--fleet-file and --context cannot be combined",
		},
		{
			name:    "--fleet-file and --all-contexts are refused",
			mutate:  func(o *fleetOptions) { o.allContexts = true },
			wantErr: "--fleet-file and --all-contexts cannot be combined",
		},
		{
			name:   "--fleet-file and --kubeconfig are allowed",
			mutate: func(o *fleetOptions) { o.kubeconfig = "/fleet/fallback.kubeconfig" },
		},
		{
			name:   "--fleet-file and --match are allowed",
			mutate: func(o *fleetOptions) { o.match = "prod-*" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := base()
			tt.mutate(&o)
			err := validateFleetOptions(o)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateFleetOptions() error = %v, want none", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateFleetOptions() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateFleetOptions() error = %q, want it to contain %q", err, tt.wantErr)
			}
			if code := exitCodeFor(err); code != 4 {
				t.Errorf("exit code = %d, want 4", code)
			}
		})
	}
}

// --match filters something a sweep would otherwise take all of. There are now
// two such sources, and the refusal has to name both or an operator reading it
// learns only half the answer.
func TestSelectContextsMatchNeedsAllContextsOrAFleetFile(t *testing.T) {
	_, err := selectContexts([]cluster.ContextInfo{{Name: "prod-eu"}}, nil, false, "prod-*")
	if err == nil {
		t.Fatal("selectContexts() error = nil, want one")
	}
	for _, want := range []string{"--all-contexts", "--fleet-file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
}

// --fleet-file accepts the single-dash long-flag spelling, like every other
// flag: Normalize is what keeps command lines written against v0.72 working.
func TestParseFleetFlagsAcceptsTheFleetFileFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--fleet-file", "/fleet/clusters.yaml"},
		{"-fleet-file", "/fleet/clusters.yaml"},
	} {
		o, err := parseFleetFlags(args)
		if err != nil {
			t.Fatalf("parseFleetFlags(%v) error = %v", args, err)
		}
		if o.fleetFile != "/fleet/clusters.yaml" {
			t.Errorf("parseFleetFlags(%v) fleetFile = %q, want /fleet/clusters.yaml", args, o.fleetFile)
		}
	}
}

// An unreadable file is bad input, found before any cluster was touched, and
// the path may be named because this reaches stderr and nowhere else.
func TestReadFleetFileNamesTheFlagAndThePathOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	_, err := readFleetFile(path)
	if err == nil {
		t.Fatal("readFleetFile() error = nil, want one")
	}
	if !strings.Contains(err.Error(), "--fleet-file") || !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name both --fleet-file and the path", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// A file that reads but does not load is the same class of failure.
func TestReadFleetFileReportsALoadFailureAtExitFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	if err := os.WriteFile(path, []byte("- name: edge-a\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	_, err := readFleetFile(path)
	if err == nil {
		t.Fatal("readFleetFile() error = nil, want one")
	}
	if !strings.Contains(err.Error(), "entry 1 has no context") {
		t.Errorf("error = %q, want the load failure", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// buildFleetFileTargets uses the entry's own kubeconfig when it names one and
// the fallback otherwise, and it carries the identity pair through to the
// target. cluster.NewClient does no network I/O, so this needs no cluster.
func TestBuildFleetFileTargets(t *testing.T) {
	fallback := fleetFileKubeconfigPath(t, "prod-eu")
	perCluster := fleetFileKubeconfigPath(t, "default")

	targets, err := buildFleetFileTargets(fallback, []fleetfile.Entry{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Kubeconfig: perCluster, Context: "default"},
	})
	if err != nil {
		t.Fatalf("buildFleetFileTargets() error = %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	for i, want := range []fleet.Target{
		{Name: "prod-eu", Context: "prod-eu"},
		{Name: "edge-a", Context: "default"},
	} {
		if targets[i].Name != want.Name || targets[i].Context != want.Context {
			t.Errorf("target %d = {Name:%q Context:%q}, want {%q %q}",
				i, targets[i].Name, targets[i].Context, want.Name, want.Context)
		}
		if targets[i].Client == nil {
			t.Errorf("target %d has no client", i)
		}
	}
}

// A context the kubeconfig does not define is a configuration defect, not a
// reachability event: cluster.NewClient does no network I/O. Fatal at exit 4,
// the same ruling buildFleetTargets makes, and the same standard
// TestBuildFleetTargetsRejectsAnUnknownContext holds it to: the error must
// name what failed, not just that something did.
func TestBuildFleetFileTargetsRejectsAnUnknownContext(t *testing.T) {
	path := fleetFileKubeconfigPath(t, "prod-eu")
	_, err := buildFleetFileTargets(path, []fleetfile.Entry{
		{Name: "edge-a", Context: "nonexistent"},
	})
	if err == nil {
		t.Fatal("buildFleetFileTargets() error = nil, want one")
	}
	if !strings.Contains(err.Error(), `"edge-a"`) {
		t.Errorf("error = %v, want it to name the entry", err)
	}
	if code := exitCodeFor(err); code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
}

// fleetFileKubeconfigPath writes a one-context kubeconfig into a temp dir. The
// server is a loopback address on a port nothing listens on: cluster.NewClient
// does no network I/O, so nothing ever connects to it.
func fleetFileKubeconfigPath(t *testing.T, contextName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := "apiVersion: v1\n" +
		"kind: Config\n" +
		"current-context: " + contextName + "\n" +
		"clusters:\n" +
		"  - name: " + contextName + "\n" +
		"    cluster:\n" +
		"      server: https://127.0.0.1:1\n" +
		"contexts:\n" +
		"  - name: " + contextName + "\n" +
		"    context: {cluster: " + contextName + ", user: " + contextName + "}\n" +
		"users:\n" +
		"  - name: " + contextName + "\n" +
		"    user: {token: <PLACEHOLDER>}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}
