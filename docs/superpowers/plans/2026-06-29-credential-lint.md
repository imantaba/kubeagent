# Credential Lint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An opt-in `--lint-secrets` scan that flags credentials stored in the clear (ConfigMap values, pod env `value:` literals), reporting location + pattern type only — **never the value**, and **never** to `--explain`.

**Architecture:** A new pure package `internal/credlint` classifies (name, value) pairs into value-free `Finding`s. `internal/collect` lists ConfigMaps (namespace-scoped). `report` renders a "Credential warnings" section (text + JSON); `main.go` runs the scan only when `--lint-secrets` is set and passes the findings to report (not explain).

**Tech Stack:** Go 1.26, client-go, stdlib `regexp`/`strings`/`sort`. No new module dependency.

## Global Constraints

- **SECURITY (cardinal rule):** `credlint.Finding` has NO value field. `Scan`/`classify` never return or store a matched value. `report` renders only namespace/name/kind/location/pattern. Credential findings are NEVER passed to `ExplainInventory`/`buildInventoryPrompt`.
- **READ-ONLY:** one new List (ConfigMaps), only when `--lint-secrets` is set. No mutation.
- **No new Go module dependency.**
- **Sequential**, stdlib `flag`. Exit codes unchanged. Default-off → byte-identical to today.
- **Namespace scope:** ConfigMaps honor the scan's `-n` (pass `namespace`).
- **Skips (false-positive control):** `valueFrom`/`secretKeyRef` env (references, safe); empty / purely-numeric / `true`/`false` / `$`-prefixed (`$(…)`,`${…}`,`$VAR`) values for the name-heuristic. Value-pattern matches (AWS/private-key/GitHub/JWT) fire regardless of name.
- **All-clear:** the existing "No issues found. ✅" condition gains `&& len(credentialWarnings) == 0`.
- **TDD:** failing test first, watch it fail, implement, watch it pass, commit. `export PATH=$PATH:/usr/local/go/bin` first. Run `gofmt -l` on touched files; fix with `gofmt -w`.
- **Scope (YAGNI):** ConfigMaps + pod env literals only (no Secrets, no BinaryData, no entropy detection); no `report.Input` refactor.

---

### Task 1: `internal/credlint` — classify credentials in the clear (pure)

**Files:**
- Create: `internal/credlint/credlint.go`
- Test: `internal/credlint/credlint_test.go`

**Interfaces:**
- Produces:
  - `type Finding struct { Namespace, Name, Kind, Location, Pattern string }`
  - `func Scan(configMaps []corev1.ConfigMap, pods []corev1.Pod) []Finding`

- [ ] **Step 1: Write the failing test**

Create `internal/credlint/credlint_test.go`:

```go
package credlint

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func cm(ns, name string, data map[string]string) corev1.ConfigMap {
	return corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Data: data}
}

func podEnv(ns, name, container string, env []corev1.EnvVar) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: container, Env: env}}},
	}
}

func TestScan_ConfigMapValuePatterns(t *testing.T) {
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{
		"aws":  "AKIAIOSFODNN7EXAMPLE",
		"pem":  "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----",
		"gh":   "ghp_1234567890abcdefghijklmnopqrstuvwx",
		"jwt":  "eyJhbGciOi.eyJzdWIiOi.sIgnaTURE",
		"note": "just a normal config value",
	})}
	got := Scan(cms, nil)
	want := map[string]string{"aws": "AWS access key", "pem": "private key", "gh": "GitHub token", "jwt": "JWT"}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for _, f := range got {
		if f.Kind != "ConfigMap" || f.Name != "cfg" {
			t.Errorf("unexpected finding object: %+v", f)
		}
		if want[f.Location] != f.Pattern {
			t.Errorf("location %q: pattern = %q, want %q", f.Location, f.Pattern, want[f.Location])
		}
	}
}

func TestScan_CredentialLikeNameWithLiteral(t *testing.T) {
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{
		"DB_PASSWORD": "hunter2pass",
		"TOKEN_TTL":   "3600",          // numeric → skipped
		"DEBUG":       "true",          // boolean → skipped
		"API_KEY":     "${API_KEY}",    // reference → skipped
		"MAX_CONNS":   "50",            // numeric → skipped
	})}
	got := Scan(cms, nil)
	if len(got) != 1 || got[0].Location != "DB_PASSWORD" || got[0].Pattern != "credential-like name with a literal value" {
		t.Fatalf("want one DB_PASSWORD finding, got %+v", got)
	}
}

func TestScan_PodEnvLiteralAndValueFromSkipped(t *testing.T) {
	pods := []corev1.Pod{podEnv("default", "web", "app", []corev1.EnvVar{
		{Name: "AWS_SECRET", Value: "AKIAIOSFODNN7EXAMPLE"}, // literal → flagged
		{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}, // ref → skipped
		{Name: "PORT", Value: "8080"}, // numeric → skipped
	})}
	got := Scan(nil, pods)
	if len(got) != 1 || got[0].Kind != "Pod" || got[0].Location != "app/AWS_SECRET" || got[0].Pattern != "AWS access key" {
		t.Fatalf("want one pod-env finding at app/AWS_SECRET, got %+v", got)
	}
}

func TestScan_NeverRecordsTheValue(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	cms := []corev1.ConfigMap{cm("default", "cfg", map[string]string{"aws": secret, "DB_PASSWORD": "hunter2pass"})}
	got := Scan(cms, nil)
	if len(got) == 0 {
		t.Fatal("expected findings")
	}
	for _, f := range got {
		for _, field := range []string{f.Namespace, f.Name, f.Kind, f.Location, f.Pattern} {
			if strings.Contains(field, secret) || strings.Contains(field, "hunter2pass") {
				t.Errorf("a finding field leaked a secret value: %+v", f)
			}
		}
	}
}

func TestScan_SortedAndStable(t *testing.T) {
	cms := []corev1.ConfigMap{
		cm("b", "y", map[string]string{"aws": "AKIAIOSFODNN7EXAMPLE"}),
		cm("a", "z", map[string]string{"aws": "AKIAIOSFODNN7EXAMPLE"}),
	}
	got := Scan(cms, nil)
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("want sorted by namespace, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/credlint/`
Expected: FAIL — package has no non-test files / `undefined: Scan`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/credlint/credlint.go`:

```go
// Package credlint flags credentials stored in the clear — in ConfigMap values
// or pod env `value:` literals. It is pure and security-conscious: a Finding
// records only WHERE a credential lives and WHAT pattern matched, never the
// value itself.
package credlint

import (
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Finding is one credential-in-the-clear warning. It deliberately has no value.
type Finding struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`     // "ConfigMap" | "Pod"
	Location  string `json:"location"` // ConfigMap data key, or "container/ENV_NAME"
	Pattern   string `json:"pattern"`  // what matched (never the value)
}

var (
	awsKeyRe  = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	ghTokenRe = regexp.MustCompile(`ghp_[0-9A-Za-z]{36}|github_pat_[0-9A-Za-z_]{22,}`)
	jwtRe     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	credName  = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`)
	numericRe = regexp.MustCompile(`^[0-9]+$`)
)

// classify returns a pattern label and true when (name, value) looks like a
// credential in the clear. It never returns the value.
func classify(name, value string) (string, bool) {
	switch {
	case awsKeyRe.MatchString(value):
		return "AWS access key", true
	case strings.Contains(value, "-----BEGIN ") && strings.Contains(value, "PRIVATE KEY-----"):
		return "private key", true
	case ghTokenRe.MatchString(value):
		return "GitHub token", true
	case jwtRe.MatchString(value):
		return "JWT", true
	case credName.MatchString(name) && looksLikeLiteralSecret(value):
		return "credential-like name with a literal value", true
	}
	return "", false
}

// looksLikeLiteralSecret excludes empties, numbers, booleans, and shell/template
// references so the name heuristic doesn't fire on benign config.
func looksLikeLiteralSecret(value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || numericRe.MatchString(v) || strings.HasPrefix(v, "$") {
		return false
	}
	switch strings.ToLower(v) {
	case "true", "false":
		return false
	}
	return true
}

// Scan flags credential-shaped values in ConfigMap data and pod (init + regular)
// container env literals. Result is sorted by (Namespace, Name, Location).
func Scan(configMaps []corev1.ConfigMap, pods []corev1.Pod) []Finding {
	var out []Finding
	for _, c := range configMaps {
		for key, value := range c.Data {
			if pat, ok := classify(key, value); ok {
				out = append(out, Finding{Namespace: c.Namespace, Name: c.Name, Kind: "ConfigMap", Location: key, Pattern: pat})
			}
		}
	}
	for _, p := range pods {
		containers := append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...)
		for _, ctr := range containers {
			for _, e := range ctr.Env {
				if e.ValueFrom != nil || e.Value == "" {
					continue // references are the safe pattern; nothing to lint
				}
				if pat, ok := classify(e.Name, e.Value); ok {
					out = append(out, Finding{Namespace: p.Namespace, Name: p.Name, Kind: "Pod", Location: ctr.Name + "/" + e.Name, Pattern: pat})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Location < out[j].Location
	})
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/credlint/ -v && go vet ./internal/credlint/ && gofmt -l internal/credlint/`
Expected: all tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/credlint/
git commit -m "feat(credlint): flag credentials in the clear (ConfigMap/env), value-free findings"
```

---

### Task 2: `internal/collect` — list ConfigMaps

**Files:**
- Modify: `internal/collect/collect.go`
- Test: `internal/collect/collect_test.go`

**Interfaces:**
- Produces: `func ConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ConfigMap, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/collect/collect_test.go` (it already imports `context`, `testing`, `corev1`, `metav1`, and `k8s.io/client-go/kubernetes/fake`):

```go
func TestConfigMaps_Lists(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c1"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "c2"}},
	)
	cms, err := ConfigMaps(context.Background(), client, "")
	if err != nil {
		t.Fatalf("ConfigMaps: %v", err)
	}
	if len(cms) != 2 {
		t.Errorf("want 2 configmaps, got %d", len(cms))
	}
}

func TestConfigMaps_NamespaceScoped(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "c1"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "c2"}},
	)
	cms, err := ConfigMaps(context.Background(), client, "a")
	if err != nil {
		t.Fatalf("ConfigMaps: %v", err)
	}
	if len(cms) != 1 || cms[0].Namespace != "a" {
		t.Errorf("want only namespace a, got %+v", cms)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/`
Expected: FAIL — `undefined: ConfigMaps`.

- [ ] **Step 3: Write minimal implementation**

In `internal/collect/collect.go` append:

```go
// ConfigMaps lists ConfigMaps in the namespace (empty = all), read-only.
func ConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ConfigMap, error) {
	cms, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing configmaps: %w", err)
	}
	return cms.Items, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/collect/ -v && go vet ./internal/collect/ && gofmt -l internal/collect/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/collect/
git commit -m "feat(collect): list ConfigMaps (namespace-scoped)"
```

---

### Task 3: `internal/report` — Credential warnings section + JSON

**Files:**
- Modify: `internal/report/report.go`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `credlint.Finding`.
- Produces (changed signature):
  `func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, explanation, format string, w io.Writer) error`

- [ ] **Step 1: Write the failing test**

In `internal/report/report_test.go` add `"github.com/imantaba/kubeagent/internal/credlint"` to imports, add the tests below, and insert `nil` as the new **sixth** argument (after the `serviceIssues` argument, before `explanation`) in **every existing** `PrintInventory(...)` call in the file:

```go
func sampleCredWarnings() []credlint.Finding {
	return []credlint.Finding{
		{Namespace: "default", Name: "app-config", Kind: "ConfigMap", Location: "DB_PASSWORD", Pattern: "credential-like name with a literal value"},
		{Namespace: "default", Name: "web", Kind: "Pod", Location: "app/AWS_SECRET", Pattern: "AWS access key"},
	}
}

func TestPrintInventory_TextShowsCredentialWarnings(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, sampleCredWarnings(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Credential warnings (--lint-secrets):", "default/app-config", "ConfigMap[DB_PASSWORD]", "default/web", "Pod[app/AWS_SECRET]", "AWS access key"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestPrintInventory_TextNoCredentialSectionWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, nil, "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "Credential warnings") {
		t.Errorf("no credential section expected when empty:\n%s", buf.String())
	}
}

func TestPrintInventory_CredentialWarningsSuppressAllClear(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, sampleCredWarnings(), "", "text", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(buf.String(), "No issues found") {
		t.Errorf("all-clear must not print when there are credential warnings:\n%s", buf.String())
	}
}

func TestPrintInventory_JSONIncludesCredentialWarnings(t *testing.T) {
	var buf bytes.Buffer
	ch := clusterhealth.ClusterHealth{Verdict: "Healthy", NodesTotal: 1, NodesReady: 1}
	if err := PrintInventory(ch, inventory.Result{}, nil, nil, nil, sampleCredWarnings(), "", "json", &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got struct {
		CredentialWarnings []credlint.Finding `json:"credentialWarnings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.CredentialWarnings) != 2 || got.CredentialWarnings[0].Location != "DB_PASSWORD" {
		t.Errorf("credentialWarnings missing/wrong in JSON: %+v", got.CredentialWarnings)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/`
Expected: FAIL — too many arguments / `undefined: credlint`.

- [ ] **Step 3: Write minimal implementation**

In `internal/report/report.go`:

Add `"github.com/imantaba/kubeagent/internal/credlint"` to imports.

Add to `inventoryReport` (after `ServiceIssues`):

```go
	CredentialWarnings []credlint.Finding `json:"credentialWarnings,omitempty"`
```

Change `PrintInventory` signature (add `credentialWarnings []credlint.Finding` after `serviceIssues`) and the JSON encode + text dispatch:

```go
func PrintInventory(cluster clusterhealth.ClusterHealth, result inventory.Result, summary *resources.Summary, facts *platform.Facts, serviceIssues []svchealth.Issue, credentialWarnings []credlint.Finding, explanation, format string, w io.Writer) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(inventoryReport{Cluster: cluster, Workloads: result.Workloads, Resources: summary, Platform: facts, ServiceIssues: serviceIssues, CredentialWarnings: credentialWarnings, Explanation: explanation})
	case "text":
		return printInventoryText(cluster, result, summary, facts, serviceIssues, credentialWarnings, explanation, w)
	default:
		return fmt.Errorf("unknown output format %q (want text or json)", format)
	}
}
```

Change `printInventoryText`'s signature to add `credentialWarnings []credlint.Finding` (after `serviceIssues`). After the `printServiceIssues(serviceIssues, w)` call, add the credential section, and extend the all-clear condition:

```go
	if err := printServiceIssues(serviceIssues, w); err != nil {
		return err
	}

	if err := printCredentialWarnings(credentialWarnings, w); err != nil {
		return err
	}

	if len(result.Workloads) == 0 && len(serviceIssues) == 0 && len(credentialWarnings) == 0 && cluster.Verdict == "Healthy" {
		if _, err := fmt.Fprintln(w, "No issues found. ✅"); err != nil {
			return err
		}
	}
```

Add the helper:

```go
func printCredentialWarnings(findings []credlint.Finding, w io.Writer) error {
	if len(findings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Credential warnings (--lint-secrets):"); err != nil {
		return err
	}
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "  ⚠ %s/%s  %s[%s]  %s\n", f.Namespace, f.Name, f.Kind, f.Location, f.Pattern); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -v && go vet ./internal/report/ && gofmt -l internal/report/`
Expected: PASS (new + all existing), vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): credential warnings section + JSON; suppress all-clear"
```

---

### Task 4: wire `main.go`, document, CHANGELOG

**Files:**
- Modify: `main.go`
- Test: `main_test.go`
- Modify: `README.md`
- Modify: `CHANGELOG.md`

**Interfaces:**
- Consumes: `collect.ConfigMaps`, `credlint.Scan`, the new `report.PrintInventory` signature.

- [ ] **Step 1: Write the failing flag-parse test**

Append to `main_test.go`:

```go
func TestRun_LintSecretsFlagAccepted(t *testing.T) {
	// --lint-secrets must be a defined flag: this fails on output-format
	// validation (which happens before any cluster connection), proving the flag
	// parsed rather than erroring with "flag provided but not defined".
	err := run([]string{"scan", "--lint-secrets", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_LintSecretsFlagAccepted`
Expected: FAIL — `flag provided but not defined: -lint-secrets` (the flag doesn't exist yet).

- [ ] **Step 3: Wire main.go**

Add `"github.com/imantaba/kubeagent/internal/credlint"` to imports. Register the flag alongside the others (near `includeRestarts`):

```go
	lintSecrets := fs.Bool("lint-secrets", false, "scan ConfigMaps and pod env for credentials stored in the clear (never prints values)")
```

Add `--lint-secrets` to the usage string in `run` (the `fmt.Errorf("usage: …")` line) so it's listed.

Before the `return report.PrintInventory(...)` call, insert:

```go
	var credWarnings []credlint.Finding
	if *lintSecrets {
		cms, _ := collect.ConfigMaps(context.Background(), client, namespace)
		credWarnings = credlint.Scan(cms, inputs.Pods)
	}
```

And update the report call to pass `credWarnings` after `serviceIssues`:

```go
	return report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, explanation, *output, os.Stdout)
```

(The `ExplainInventory` call is unchanged — credential findings are never sent to the model.)

- [ ] **Step 4: Run the test + full suite**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run TestRun_LintSecretsFlagAccepted -v && go vet ./... && go test ./... && gofmt -l main.go main_test.go && go build -o /tmp/kubeagent .`
Expected: the new test PASSES, all packages `ok`, gofmt prints nothing, build succeeds.

- [ ] **Step 5: Document in README.md**

Add a `### Credential lint (opt-in)` subsection in the scan/usage area (before `## Install`):

```markdown
### Credential lint (opt-in)

`scan --lint-secrets` flags credentials stored in the clear — values in ConfigMaps
and pod env `value:` literals (where a `secretKeyRef` should have been used) that
match a known pattern (AWS key, private key, GitHub token, JWT) or a
credential-like key name with a literal value. It reports only the location and
pattern — **never the value** — and these findings are **never sent to
`--explain`**. Off by default (no ConfigMaps are read without the flag).
Read-only and namespace-scoped.
```

- [ ] **Step 6: Update CHANGELOG.md**

Under `## [Unreleased]` → `### Added`, add a bullet, and REMOVE the "Secret / credential lint" bullet from `### Planned` (the `### Planned` subsection is now empty — remove the now-empty `### Planned` heading too):

```markdown
- **Credential lint (opt-in).** `scan --lint-secrets` flags credentials stored in
  the clear (ConfigMap values, pod env literals) by location and pattern — never
  the value, and never sent to `--explain`.
```

- [ ] **Step 7: Commit**

```bash
git add main.go main_test.go README.md CHANGELOG.md
git commit -m "feat: --lint-secrets credential lint in scan; document + changelog"
```

---

## Self-Review

**Spec coverage:**
- `credlint.Scan` (value patterns, name+literal heuristic, skips, sort, **no-value-leak test**) → Task 1. ✓
- collect ConfigMaps (namespace-scoped) → Task 2. ✓
- report section + JSON + all-clear suppression → Task 3. ✓
- main `--lint-secrets` (gated; off = no ConfigMap read) + usage + README + CHANGELOG → Task 4. ✓
- explain never receives credential findings (no signature change) → by construction (Tasks 3–4). ✓

**Placeholder scan:** none.

**Type consistency:** `credlint.Finding`/`Scan`, the `credentialWarnings []credlint.Finding` parameter (placed after `serviceIssues`), and the `PrintInventory` signature are used identically across Tasks 1–4. The text format `Kind[Location]` in Task 3's helper matches the substrings asserted in Task 3's tests.
