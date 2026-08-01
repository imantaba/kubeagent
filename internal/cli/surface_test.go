package cli

import (
	"strings"
	"testing"
	"time"
)

// TestCommandSurfaceScan asserts that every flag on every command reaches the
// field it configures. It is written against the standard-library flag
// implementation and must pass unchanged after the Cobra migration: a CLI
// test that only passes once Cobra lands proves nothing about compatibility.
//
// The 79 flag declarations across seven commands are the migration's largest
// failure mode — a mistyped default or a field bound to the wrong flag fails
// silently, producing a scan that is subtly wrong rather than one that errors.
// This table is the mitigation.
func TestCommandSurfaceScan(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(scanOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o scanOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o scanOptions) bool { return o.contextName == "example-context" }},
		{"output", []string{"--output", "json"}, func(o scanOptions) bool { return o.output == "json" }},
		{"explain", []string{"--explain"}, func(o scanOptions) bool { return o.explain }},
		{"investigate", []string{"--investigate"}, func(o scanOptions) bool { return o.investigate }},
		{"model", []string{"--model", "example-model"}, func(o scanOptions) bool { return o.model == "example-model" }},
		{"include-cron", []string{"--include-cron"}, func(o scanOptions) bool { return o.includeCron }},
		{"include-restarts", []string{"--include-restarts"}, func(o scanOptions) bool { return o.includeRestarts }},
		{"lint-secrets", []string{"--lint-secrets"}, func(o scanOptions) bool { return o.lintSecrets }},
		{"pvc-reclaim", []string{"--pvc-reclaim"}, func(o scanOptions) bool { return o.pvcReclaimFull }},
		{"disk-usage", []string{"--disk-usage"}, func(o scanOptions) bool { return o.diskUsage }},
		{"disk-threshold", []string{"--disk-threshold", "0.42"}, func(o scanOptions) bool { return o.diskThreshold == 0.42 }},
		{"kubelet-health", []string{"--kubelet-health"}, func(o scanOptions) bool { return o.kubeletHealth }},
		{"control-plane-health", []string{"--control-plane-health"}, func(o scanOptions) bool { return o.controlPlaneHealth }},
		{"dns-health", []string{"--dns-health"}, func(o scanOptions) bool { return o.dnsHealth }},
		{"certs", []string{"--certs"}, func(o scanOptions) bool { return o.certs }},
		{"cert-warn-days", []string{"--cert-warn-days", "7"}, func(o scanOptions) bool { return o.certWarnDays == 7 }},
		{"operators", []string{"--operators"}, func(o scanOptions) bool { return o.operators }},
		{"drift", []string{"--drift"}, func(o scanOptions) bool { return o.drift }},
		{"drift-age", []string{"--drift-age", "30m"}, func(o scanOptions) bool { return o.driftAge == 30*time.Minute }},
		{"capacity", []string{"--capacity"}, func(o scanOptions) bool { return o.capacity }},
		{"logs", []string{"--logs"}, func(o scanOptions) bool { return o.logs }},
		{"node-heartbeat-threshold", []string{"--node-heartbeat-threshold", "90s"}, func(o scanOptions) bool { return o.nodeHeartbeatThreshold == 90*time.Second }},
		{"expected-nodes", []string{"--expected-nodes", "node-a,node-b"}, func(o scanOptions) bool { return o.expectedNodes == "node-a,node-b" }},
		{"security", []string{"--security"}, func(o scanOptions) bool { return o.security }},
		{"security-verbose", []string{"--security-verbose"}, func(o scanOptions) bool { return o.securityVerbose }},
		{"suggest", []string{"--suggest"}, func(o scanOptions) bool { return o.suggest }},
		{"fix", []string{"--fix"}, func(o scanOptions) bool { return o.fix }},
		{"dry-run", []string{"--dry-run"}, func(o scanOptions) bool { return o.dryRun }},
		{"yes", []string{"--yes"}, func(o scanOptions) bool { return o.assumeYes }},
		{"audit-log", []string{"--audit-log", "/nonexistent/audit.jsonl"}, func(o scanOptions) bool { return o.auditLog == "/nonexistent/audit.jsonl" }},
		{"rollback", []string{"--rollback"}, func(o scanOptions) bool { return o.rollback }},
		{"namespace", []string{"--namespace", "example-ns"}, func(o scanOptions) bool { return o.namespace == "example-ns" }},
		{"n", []string{"-n", "example-ns"}, func(o scanOptions) bool { return o.namespace == "example-ns" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseScanFlags(tc.args)
			if err != nil {
				t.Fatalf("parseScanFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 34 {
		t.Errorf("scan surface table has %d cases, want 34 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceScanDefaults asserts scan's non-zero flag defaults. A
// default that silently becomes the zero value is the other half of the
// failure mode TestCommandSurfaceScan guards: a flag can be wired to the
// right field and still ship with the wrong out-of-the-box behavior.
func TestCommandSurfaceScanDefaults(t *testing.T) {
	// driftAge is the only one of these five defaults that reads the
	// environment (KUBEAGENT_DRIFT_AGE); clear it so a developer's shell
	// cannot change what "default" means here.
	t.Setenv("KUBEAGENT_DRIFT_AGE", "")
	o, err := parseScanFlags(nil)
	if err != nil {
		t.Fatalf("parseScanFlags(nil): %v", err)
	}
	if o.output != "text" {
		t.Errorf("output default = %q, want text", o.output)
	}
	if o.diskThreshold != 0.80 {
		t.Errorf("diskThreshold default = %v, want 0.80", o.diskThreshold)
	}
	if o.certWarnDays != 30 {
		t.Errorf("certWarnDays default = %d, want 30", o.certWarnDays)
	}
	if o.driftAge != time.Hour {
		t.Errorf("driftAge default = %v, want 1h", o.driftAge)
	}
	if o.nodeHeartbeatThreshold != 40*time.Second {
		t.Errorf("nodeHeartbeatThreshold default = %v, want 40s", o.nodeHeartbeatThreshold)
	}
}

// TestCommandSurfaceWatch is TestCommandSurfaceScan's counterpart for
// `kubeagent watch`.
func TestCommandSurfaceWatch(t *testing.T) {
	// Thirteen of watch's flags default from the environment, reading the
	// twelve keys below — --namespace and -n share KUBEAGENT_NAMESPACE. This
	// is the same set TestParseWatchFlagsCarriesEveryValue clears: a
	// developer's shell must not decide whether an explicit flag value lands.
	for _, k := range []string{
		"KUBEAGENT_CLUSTER_NAME", "KUBEAGENT_INCLUDE_LOCAL", "KUBEAGENT_METRICS_ADDR",
		"KUBEAGENT_HEARTBEAT", "KUBEAGENT_DEBOUNCE", "KUBEAGENT_ALERT_FORMAT",
		"KUBEAGENT_ALERT_REPEAT", "KUBEAGENT_SLO_TARGET", "KUBEAGENT_NAMESPACE",
		"KUBEAGENT_EXPLAIN", "KUBEAGENT_EXPLAIN_COOLDOWN", "KUBEAGENT_EXPLAIN_BUDGET",
	} {
		t.Setenv(k, "")
	}
	cases := []struct {
		flag  string
		args  []string
		check func(watchOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o watchOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o watchOptions) bool { return len(o.contexts) == 1 && o.contexts[0] == "example-context" }},
		{"cluster-name", []string{"--cluster-name", "example-cluster"}, func(o watchOptions) bool { return o.clusterName == "example-cluster" }},
		{"include-local", []string{"--include-local"}, func(o watchOptions) bool { return o.includeLocal }},
		{"metrics-addr", []string{"--metrics-addr", "192.0.2.10:9090"}, func(o watchOptions) bool { return o.metricsAddr == "192.0.2.10:9090" }},
		{"heartbeat", []string{"--heartbeat", "30s"}, func(o watchOptions) bool { return o.heartbeat == 30*time.Second }},
		{"debounce", []string{"--debounce", "5s"}, func(o watchOptions) bool { return o.debounce == 5*time.Second }},
		{"include-cron", []string{"--include-cron"}, func(o watchOptions) bool { return o.includeCron }},
		{"include-restarts", []string{"--include-restarts"}, func(o watchOptions) bool { return o.includeRestarts }},
		{"alert-format", []string{"--alert-format", "slack"}, func(o watchOptions) bool { return o.alertFormat == "slack" }},
		{"alert-repeat", []string{"--alert-repeat", "2h"}, func(o watchOptions) bool { return o.alertRepeat == 2*time.Hour }},
		{"slo-target", []string{"--slo-target", "99.9"}, func(o watchOptions) bool { return o.sloTarget == 99.9 }},
		{"explain", []string{"--explain"}, func(o watchOptions) bool { return o.explain }},
		{"explain-cooldown", []string{"--explain-cooldown", "15m"}, func(o watchOptions) bool { return o.explainCooldown == 15*time.Minute }},
		{"explain-budget", []string{"--explain-budget", "7"}, func(o watchOptions) bool { return o.explainBudget == 7 }},
		{"model", []string{"--model", "example-model"}, func(o watchOptions) bool { return o.model == "example-model" }},
		{"namespace", []string{"--namespace", "example-ns"}, func(o watchOptions) bool { return o.namespace == "example-ns" }},
		{"n", []string{"-n", "example-ns"}, func(o watchOptions) bool { return o.namespace == "example-ns" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseWatchFlags(tc.args)
			if err != nil {
				t.Fatalf("parseWatchFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 18 {
		t.Errorf("watch surface table has %d cases, want 18 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceGate is TestCommandSurfaceScan's counterpart for
// `kubeagent gate`.
func TestCommandSurfaceGate(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(gateOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o gateOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o gateOptions) bool { return o.contextName == "example-context" }},
		{"output", []string{"--output", "sarif"}, func(o gateOptions) bool { return o.output == "sarif" }},
		{"fail-on", []string{"--fail-on", "warning"}, func(o gateOptions) bool { return o.failOn == "warning" }},
		{"wait-for", []string{"--wait-for", "deployment/example-api"}, func(o gateOptions) bool { return o.waitFor == "deployment/example-api" }},
		{"timeout", []string{"--timeout", "90s"}, func(o gateOptions) bool { return o.timeout == 90*time.Second }},
		{"poll-interval", []string{"--poll-interval", "3s"}, func(o gateOptions) bool { return o.pollInterval == 3*time.Second }},
		{"allow-partial-read", []string{"--allow-partial-read", "leases"}, func(o gateOptions) bool {
			return len(o.allowPartialRead) == 1 && o.allowPartialRead[0] == "leases"
		}},
		{"namespace", []string{"--namespace", "example-ns"}, func(o gateOptions) bool { return o.namespace == "example-ns" }},
		{"n", []string{"-n", "example-ns"}, func(o gateOptions) bool { return o.namespace == "example-ns" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseGateFlags(tc.args)
			if err != nil {
				t.Fatalf("parseGateFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 10 {
		t.Errorf("gate surface table has %d cases, want 10 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceGateDefaults asserts gate's non-zero flag defaults.
func TestCommandSurfaceGateDefaults(t *testing.T) {
	o, err := parseGateFlags(nil)
	if err != nil {
		t.Fatalf("parseGateFlags(nil): %v", err)
	}
	if o.output != "text" {
		t.Errorf("output default = %q, want text", o.output)
	}
	if o.failOn != "critical" {
		t.Errorf("failOn default = %q, want critical", o.failOn)
	}
	if o.timeout != 5*time.Minute {
		t.Errorf("timeout default = %v, want 5m", o.timeout)
	}
	if o.pollInterval != 2*time.Second {
		t.Errorf("pollInterval default = %v, want 2s", o.pollInterval)
	}
}

// TestCommandSurfaceMCP is TestCommandSurfaceScan's counterpart for
// `kubeagent mcp`.
func TestCommandSurfaceMCP(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(mcpOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o mcpOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o mcpOptions) bool { return o.contextName == "example-context" }},
		{"allow-context-switch", []string{"--allow-context-switch"}, func(o mcpOptions) bool { return o.allowContextSwitch }},
		{"logs", []string{"--logs"}, func(o mcpOptions) bool { return o.logs }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseMCPFlags(tc.args)
			if err != nil {
				t.Fatalf("parseMCPFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 4 {
		t.Errorf("mcp surface table has %d cases, want 4 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceTUI is TestCommandSurfaceScan's counterpart for
// `kubeagent tui`.
func TestCommandSurfaceTUI(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(tuiOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o tuiOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o tuiOptions) bool { return o.contextName == "example-context" }},
		{"namespace", []string{"--namespace", "example-ns"}, func(o tuiOptions) bool { return o.namespace == "example-ns" }},
		{"n", []string{"-n", "example-ns"}, func(o tuiOptions) bool { return o.namespace == "example-ns" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseTUIFlags(tc.args)
			if err != nil {
				t.Fatalf("parseTUIFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 4 {
		t.Errorf("tui surface table has %d cases, want 4 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceRBACPrint is TestCommandSurfaceScan's counterpart for
// `kubeagent rbac print`.
func TestCommandSurfaceRBACPrint(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(rbacPrintOptions) bool
	}{
		{"profile", []string{"--profile", "watch"}, func(o rbacPrintOptions) bool { return o.profile == "watch" }},
		{"features", []string{"--features", "core"}, func(o rbacPrintOptions) bool { return o.features == "core" }},
		{"role-name", []string{"--role-name", "example-role"}, func(o rbacPrintOptions) bool { return o.roleName == "example-role" }},
		{"output", []string{"--output", "json"}, func(o rbacPrintOptions) bool { return o.output == "json" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseRBACPrintFlags(tc.args)
			if err != nil {
				t.Fatalf("parseRBACPrintFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 4 {
		t.Errorf("rbac print surface table has %d cases, want 4 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceRBACPrintDefaults asserts rbac print's non-zero flag
// defaults.
func TestCommandSurfaceRBACPrintDefaults(t *testing.T) {
	o, err := parseRBACPrintFlags(nil)
	if err != nil {
		t.Fatalf("parseRBACPrintFlags(nil): %v", err)
	}
	if o.profile != "scan" {
		t.Errorf("profile default = %q, want scan", o.profile)
	}
	if o.roleName != "kubeagent" {
		t.Errorf("roleName default = %q, want kubeagent", o.roleName)
	}
	if o.output != "yaml" {
		t.Errorf("output default = %q, want yaml", o.output)
	}
}

// TestCommandSurfaceRBACCheck is TestCommandSurfaceScan's counterpart for
// `kubeagent rbac check`.
func TestCommandSurfaceRBACCheck(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(rbacCheckOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o rbacCheckOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o rbacCheckOptions) bool { return o.contextName == "example-context" }},
		{"profile", []string{"--profile", "scan"}, func(o rbacCheckOptions) bool { return o.profile == "scan" }},
		{"features", []string{"--features", "core"}, func(o rbacCheckOptions) bool { return o.features == "core" }},
		{"output", []string{"--output", "json"}, func(o rbacCheckOptions) bool { return o.output == "json" }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseRBACCheckFlags(tc.args)
			if err != nil {
				t.Fatalf("parseRBACCheckFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 5 {
		t.Errorf("rbac check surface table has %d cases, want 5 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceRBACCheckDefaults asserts rbac check's non-zero flag
// defaults.
func TestCommandSurfaceRBACCheckDefaults(t *testing.T) {
	o, err := parseRBACCheckFlags(nil)
	if err != nil {
		t.Fatalf("parseRBACCheckFlags(nil): %v", err)
	}
	if o.profile != "full" {
		t.Errorf("profile default = %q, want full", o.profile)
	}
	if o.output != "text" {
		t.Errorf("output default = %q, want text", o.output)
	}
}

// TestErrorStrings pins every validation message and the exit code it
// produces. These are kubeagent's public contract at v1.0; the Cobra
// migration must not reword one of them.
func TestErrorStrings(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	cases := []struct {
		name     string
		args     []string
		wantErr  string
		wantCode int
	}{
		{
			name:     "scan unknown output",
			args:     []string{"scan", "--output", "xml"},
			wantErr:  `unknown output format "xml" (want text, json or html)`,
			wantCode: 1,
		},
		{
			name:     "scan explain without a key",
			args:     []string{"scan", "--explain"},
			wantErr:  "--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model",
			wantCode: 1,
		},
		{
			name:     "scan investigate without a key",
			args:     []string{"scan", "--investigate"},
			wantErr:  "--investigate needs ANTHROPIC_API_KEY (local endpoints do not support the tool-use loop yet)",
			wantCode: 1,
		},
		{
			name:     "scan rollback with fix",
			args:     []string{"scan", "--rollback", "--fix", "--audit-log", "/nonexistent/audit.jsonl"},
			wantErr:  "--rollback and --fix are mutually exclusive",
			wantCode: 1,
		},
		{
			name:     "scan rollback without an audit log",
			args:     []string{"scan", "--rollback"},
			wantErr:  "--rollback requires --audit-log (the file to read the last applied fix from)",
			wantCode: 1,
		},
		{
			name:     "gate unknown output",
			args:     []string{"gate", "--output", "xml"},
			wantErr:  `unknown output format "xml" (want text, json or sarif)`,
			wantCode: 4,
		},
		{
			name:     "gate non-positive poll interval",
			args:     []string{"gate", "--poll-interval", "0s"},
			wantErr:  "--poll-interval must be positive, got 0s",
			wantCode: 4,
		},
		{
			name:     "rbac print unknown output",
			args:     []string{"rbac", "print", "--output", "xml"},
			wantErr:  `unknown --output "xml": want yaml or json`,
			wantCode: 1,
		},
		{
			name:     "rbac print unknown profile",
			args:     []string{"rbac", "print", "--profile", "nonesuch"},
			wantErr:  `unknown --profile "nonesuch": want scan, watch or full`,
			wantCode: 1,
		},
		{
			name:     "rbac print unknown feature",
			args:     []string{"rbac", "print", "--features", "nonesuch"},
			wantErr:  `unknown feature "nonesuch"`,
			wantCode: 1,
		},
		{
			// --output is the flag the user can see is wrong from the message
			// alone; --profile's set is longer and its error is the one to keep
			// for when --output is fine. Both commands validate --output first,
			// so the two verbs cannot disagree about which error comes out.
			name:     "rbac print reports a bad output before a bad profile",
			args:     []string{"rbac", "print", "--output", "xml", "--profile", "nonesuch"},
			wantErr:  `unknown --output "xml": want yaml or json`,
			wantCode: 1,
		},
		{
			name:     "rbac check reports a bad output before a bad profile",
			args:     []string{"rbac", "check", "--output", "xml", "--profile", "nonesuch"},
			wantErr:  `unknown --output "xml": want text or json`,
			wantCode: 1,
		},
		{
			name:     "rbac check unknown output",
			args:     []string{"rbac", "check", "--output", "xml"},
			wantErr:  `unknown --output "xml": want text or json`,
			wantCode: 1,
		},
		{
			name:     "watch empty context",
			args:     []string{"watch", "--context", ""},
			wantErr:  "--context cannot be empty",
			wantCode: 1,
		},
		{
			name:     "schema too many arguments",
			args:     []string{"schema", "scan", "gate"},
			wantErr:  "usage: kubeagent schema [name]",
			wantCode: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(tc.args)
			if err == nil {
				t.Fatalf("Run(%v) = nil, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Run(%v) error = %q, want it to contain %q", tc.args, err, tc.wantErr)
			}
			if got := exitCodeFor(err); got != tc.wantCode {
				t.Errorf("Run(%v) exit code = %d, want %d", tc.args, got, tc.wantCode)
			}
		})
	}
}

// TestInvokedAsSpelling asserts that both spellings of the command name reach
// the text a user sees. krew installs the binary as kubectl-kubeagent and
// kubectl execs it under that name, so a plugin user must never be told about
// a "kubeagent" binary that is not on their PATH.
func TestInvokedAsSpelling(t *testing.T) {
	for _, tc := range []struct{ argv0, want string }{
		{"/usr/local/bin/kubeagent", "kubeagent"},
		{"./kubeagent", "kubeagent"},
		{"/home/example/.krew/bin/kubectl-kubeagent", "kubectl kubeagent"},
		{"/usr/local/bin/kubectl-kubeagent-extra", "kubeagent"},
	} {
		if got := invocationName(tc.argv0); got != tc.want {
			t.Errorf("invocationName(%q) = %q, want %q", tc.argv0, got, tc.want)
		}
	}
}

func TestInvokedAsReachesUsageAndWarnings(t *testing.T) {
	for _, spelling := range []string{"kubeagent", "kubectl kubeagent"} {
		t.Run(spelling, func(t *testing.T) {
			old := invokedAs
			invokedAs = spelling
			defer func() { invokedAs = old }()

			err := Run(nil)
			if err == nil {
				t.Fatal("Run(nil) = nil, want the usage error")
			}
			if !strings.Contains(err.Error(), "usage: "+spelling+" scan") {
				t.Errorf("usage error = %q, want it to name %q", err, spelling)
			}

			var buf strings.Builder
			warnf(&buf, "example warning")
			if !strings.HasPrefix(buf.String(), spelling+": warning: ") {
				t.Errorf("warnf output = %q, want it to start with %q", buf.String(), spelling+": warning: ")
			}
		})
	}
}
