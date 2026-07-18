# Log Root-Cause Extraction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Opt-in `scan --logs` fetches a crashing container's previous-instance logs and classifies the failure line into a plain-language cause, deepening kubeagent's "why" from the Kubernetes reason to the application reason.

**Architecture:** A new pure `internal/logscan` classifier (signature library over the log tail) + a thin `collect.PreviousLogs` I/O wrapper. `scan.Evaluate`, when `--logs` is set, enriches the container-crash findings (CrashLoopBackOff / RestartLoop / OOMKilled) with the classified cause, which `report` renders and `--explain` uses (derived cause only).

**Tech Stack:** Go 1.26, standard `regexp`/`strings`, client-go `Pods().GetLogs`, the existing `diagnose.Detector` pipeline.

## Global Constraints

- **No `Co-Authored-By: Claude` trailer** on any commit; author is the human only.
- **Scan-only** — NOT wired into the `watch` daemon (no gauge, no daemon/Helm change).
- **Read-only** (`pods/log` GET), **opt-in** (`--logs`, off by default), **deterministic** (classifier is pure), **offline** (no LLM needed).
- Targets only the container-terminated crash findings — **CrashLoopBackOff, RestartLoop, OOMKilled** (they have `--previous` logs); the detectors set `Finding.Container`, and the enricher runs only on findings where `Container != ""`.
- **`--explain` receives only the derived `LogCause`**, NEVER the raw `LogExcerpt`; the `--explain` privacy note is unchanged.
- Additive JSON: new `Finding` fields are `omitempty`; existing fields/tags unchanged.
- TDD: failing test first, watch it fail, implement, confirm pass, commit.

Run Go with `export PATH=$PATH:/usr/local/go/bin`.

---

### Task 1: `internal/logscan` classifier (pure)

**Files:**
- Create: `internal/logscan/logscan.go`
- Test: `internal/logscan/logscan_test.go`

**Interfaces:**
- Produces: `logscan.Clue{Signature, Excerpt, Cause string}`; `logscan.Classify(log string) Clue`.

- [ ] **Step 1: Write the failing table test**

Create `internal/logscan/logscan_test.go`:

```go
package logscan

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct{ name, log, wantSig, wantCause string }{
		{"panic", "starting up\npanic: runtime error: invalid memory address", "panic", "application panic (code bug)"},
		{"entrypoint", `exec: "server": executable file not found in $PATH`, "entrypoint", "bad command or entrypoint"},
		{"conn-refused", "dial tcp 10.96.0.10:5432: connect: connection refused", "conn-refused", "cannot reach a dependency (10.96.0.10:5432) — connection refused"},
		{"dns", "lookup db on 10.96.0.10:53: no such host", "dns", "DNS resolution failed (name lookup)"},
		{"oom", "fatal error: out of memory", "oom-inproc", "ran out of memory in-process"},
		{"config", "yaml: line 3: mapping values are not allowed", "config", "configuration parse/validation error"},
		{"addr-in-use", "listen tcp :8080: bind: address already in use", "addr-in-use", "port already in use"},
		{"auth", `FATAL: password authentication failed for user "app"`, "auth", "authentication/authorization failure to a dependency"},
		{"perm-denied", "open /data/config: permission denied", "perm-denied", "permission denied — check securityContext / file permissions"},
		{"fallback", "just some log\nexited with code 3", "", "last output before exit (no known signature)"},
	}
	for _, c := range cases {
		got := Classify(c.log)
		if got.Signature != c.wantSig || got.Cause != c.wantCause {
			t.Errorf("%s: Classify()=%+v, want sig=%q cause=%q", c.name, got, c.wantSig, c.wantCause)
		}
	}
	if got := Classify("   \n\n"); got != (Clue{}) {
		t.Errorf("empty log: want zero Clue, got %+v", got)
	}
	if got := Classify("just some log\nexited with code 3"); got.Excerpt != "exited with code 3" {
		t.Errorf("fallback excerpt = %q, want the last non-empty line", got.Excerpt)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/logscan/`
Expected: FAIL to compile — `undefined: Classify` / `undefined: Clue`.

- [ ] **Step 3: Implement**

Create `internal/logscan/logscan.go`:

```go
// Package logscan classifies a crashed container's log tail into a plain-language
// root cause. Pure and read-only: the caller supplies the log text.
package logscan

import (
	"regexp"
	"strings"
)

// Clue is the classified root cause from a container's crash logs.
type Clue struct {
	Signature string `json:"signature"` // matched signature name, "" if fallback
	Excerpt   string `json:"excerpt"`   // the single relevant log line, trimmed/truncated
	Cause     string `json:"cause"`     // plain-language cause
}

type signature struct {
	name  string
	re    *regexp.Regexp
	cause func(m []string) string // builds the cause from the matched line's submatches
}

// signatures are checked in this order; the first signature with any matching line wins.
// The specific, exec:-anchored "entrypoint" is before the generic "perm-denied" so a
// container-start "exec: … permission denied" classifies as a bad entrypoint while a bare
// runtime "permission denied" falls through to perm-denied. "panic" is first so a panic
// body containing "no such file" isn't mis-matched.
var signatures = []signature{
	{"panic", regexp.MustCompile(`(?i)^panic:|goroutine \d+ \[running\]`), func([]string) string { return "application panic (code bug)" }},
	{"entrypoint", regexp.MustCompile(`(?i)exec:.*(?:executable file not found|no such file or directory|permission denied)`), func([]string) string { return "bad command or entrypoint" }},
	{"conn-refused", regexp.MustCompile(`(?i)dial tcp (\S+): connect: connection refused`), func(m []string) string { return "cannot reach a dependency (" + m[1] + ") — connection refused" }},
	{"dns", regexp.MustCompile(`(?i)no such host|server misbehaving`), func([]string) string { return "DNS resolution failed (name lookup)" }},
	{"oom-inproc", regexp.MustCompile(`(?i)out of memory|cannot allocate memory|std::bad_alloc`), func([]string) string { return "ran out of memory in-process" }},
	{"config", regexp.MustCompile(`(?i)^yaml:|invalid character .* looking for|failed to parse|invalid config`), func([]string) string { return "configuration parse/validation error" }},
	{"addr-in-use", regexp.MustCompile(`(?i)bind: address already in use`), func([]string) string { return "port already in use" }},
	{"auth", regexp.MustCompile(`(?i)password authentication failed|access denied|401 unauthorized|403 forbidden`), func([]string) string { return "authentication/authorization failure to a dependency" }},
	{"perm-denied", regexp.MustCompile(`(?i)permission denied|eacces`), func([]string) string { return "permission denied — check securityContext / file permissions" }},
}

const maxExcerpt = 200

// Classify scans the log's non-empty lines against the signature library (in order) and
// returns the first matching line's clue; if none match it falls back to the last
// non-empty line. An empty/whitespace log returns the zero Clue.
func Classify(log string) Clue {
	lines := strings.Split(log, "\n")
	for _, s := range signatures {
		for _, ln := range lines {
			ln = strings.TrimSpace(ln)
			if ln == "" {
				continue
			}
			if m := s.re.FindStringSubmatch(ln); m != nil {
				return Clue{Signature: s.name, Excerpt: truncate(ln), Cause: s.cause(m)}
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			return Clue{Excerpt: truncate(ln), Cause: "last output before exit (no known signature)"}
		}
	}
	return Clue{}
}

func truncate(s string) string {
	if r := []rune(s); len(r) > maxExcerpt {
		return string(r[:maxExcerpt]) + "…"
	}
	return s
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/logscan/`
Expected: PASS. If a regexp fails its case, adjust the regexp (not the test) until green — the table is the behaviour spec.

- [ ] **Step 5: Commit**

```bash
git add internal/logscan/
git commit -m "feat(logscan): classify crash logs into a plain-language cause"
```

---

### Task 2: `Finding` log fields + crash detectors set `Container`

**Files:**
- Modify: `internal/diagnose/diagnose.go` (Finding fields)
- Modify: `internal/diagnose/crashloop.go`, `internal/diagnose/restartloop.go`, `internal/diagnose/oomkilled.go` (set `Container`)
- Test: `internal/diagnose/crashloop_test.go` (assert `Container`)

**Interfaces:**
- Produces: `diagnose.Finding.Container`, `Finding.LogCause`, `Finding.LogExcerpt` (all `string`, `omitempty`). Crash detectors set `Container`.

- [ ] **Step 1: Write the failing test**

Add to `internal/diagnose/crashloop_test.go` (follow the file's existing fake-pod construction; if a helper builds the pod, reuse it — the point is a CrashLoopBackOff pod whose container is named `web`):

```go
func TestCrashLoopDetector_SetsContainer(t *testing.T) {
	f := CrashLoopDetector{}.Detect(crashLoopFacts("web")) // a CrashLoopBackOff pod, container "web"
	if f == nil || f.Container != "web" {
		t.Fatalf("expected Container=\"web\", got %+v", f)
	}
}
```

If `crashloop_test.go` has no `crashLoopFacts` helper, build the `PodFacts` inline the way the existing crashloop tests do (a pod with `Status.ContainerStatuses[0]{Name:"web", State.Waiting.Reason:"CrashLoopBackOff"}`), and assert `f.Container == "web"`.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/diagnose/ -run TestCrashLoopDetector_SetsContainer`
Expected: FAIL — `f.Container` is `""` (field is empty / undefined).

- [ ] **Step 3: Add the fields and set `Container`**

In `internal/diagnose/diagnose.go`, add to the `Finding` struct after `Resources`:

```go
	Container  string `json:"container,omitempty"`  // crashing container, set by crash detectors
	LogCause   string `json:"logCause,omitempty"`   // set by scan --logs enrichment
	LogExcerpt string `json:"logExcerpt,omitempty"` // set by scan --logs enrichment (text output only)
```

In `internal/diagnose/crashloop.go`, in the returned `&Finding{...}`, add `Container: cs.Name,`.
In `internal/diagnose/restartloop.go`, in the returned `&Finding{...}`, add `Container: cs.Name,`.
In `internal/diagnose/oomkilled.go`, in the returned `&Finding{...}`, add `Container: cs.Name,`.

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/diagnose/`
Expected: PASS (new test green; existing detector tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/diagnose/
git commit -m "feat(diagnose): crash detectors record the crashing container"
```

---

### Task 3: `collect.PreviousLogs` + scan wiring + `--logs`

**Files:**
- Modify: `internal/collect/collect.go` (add `PreviousLogs`)
- Modify: `internal/scan/scan.go` (`Options.Logs`; enrich crash findings)
- Modify: `main.go` (`--logs` flag + usage + `scan.Options`)
- Test: `internal/scan/scan_test.go`

**Interfaces:**
- Consumes: `logscan.Classify` (Task 1); `Finding.Container`/`LogCause`/`LogExcerpt` (Task 2).
- Produces: `collect.PreviousLogs(ctx, client, ns, pod, container string) (string, bool)`; `scan.Options.Logs bool`.

- [ ] **Step 1: Write the failing test**

Add to `internal/scan/scan_test.go` (the file already imports `corev1`, `metav1`, `fake`, `context`). The fake clientset's `Pods().GetLogs().DoRaw()` returns `"fake logs"`, so with `--logs` a crashing pod's finding gets a non-empty `LogCause`:

```go
func TestEvaluate_LogsEnrichCrashFindings(t *testing.T) {
	crashPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}}},
	}
	client := fake.NewSimpleClientset(crashPod)
	on, err := Evaluate(context.Background(), client, Options{Logs: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := findLogCause(on, "shop/web-1"); got == "" {
		t.Errorf("with --logs a crash finding should carry a LogCause, got none:\n%+v", on.Inventory.Workloads)
	}
	// Opt-out: no enrichment.
	off, _ := Evaluate(context.Background(), client, Options{})
	if got := findLogCause(off, "shop/web-1"); got != "" {
		t.Errorf("without --logs no LogCause, got %q", got)
	}
}

// findLogCause returns the first finding's LogCause for the given "ns/pod".
func findLogCause(r Result, pod string) string {
	for _, w := range r.Inventory.Workloads {
		for _, f := range w.Findings {
			if f.Pod == pod && f.LogCause != "" {
				return f.LogCause
			}
		}
	}
	return ""
}
```

If `Evaluate` places findings somewhere other than `Result.Inventory.Workloads[].Findings`, adjust `findLogCause` to read them from wherever the scan attaches findings (grep `Findings` in `internal/scan` and `internal/inventory`). The assertion — a crash finding gains a `LogCause` only with `--logs` — is what matters.

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/scan/ -run TestEvaluate_LogsEnrichCrashFindings`
Expected: FAIL — `Options` has no field `Logs` (compile error).

If the fake `GetLogs` **panics** rather than returning bytes, STOP and report NEEDS_CONTEXT (the design assumes the fake returns `"fake logs"`; verified in client-go v0.36.2 `fake_pod_expansion.go`, so this should not happen).

- [ ] **Step 3: Add `collect.PreviousLogs`**

In `internal/collect/collect.go`, add (the file imports `context`, `corev1`, `kubernetes`; add nothing new — `PodLogOptions` is `corev1`):

```go
// PreviousLogs fetches the last-terminated instance's logs for one container, capped at
// 25 lines. Never returns an error (non-fatal, like NodeStats): returns ("", false) on any
// failure (no previous instance, forbidden, transport error, empty).
func PreviousLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string) (string, bool) {
	tail := int64(25)
	raw, err := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, Previous: true, TailLines: &tail,
	}).DoRaw(ctx)
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}
```

- [ ] **Step 4: Wire `scan.go`**

In `internal/scan/scan.go`: add the import `"github.com/imantaba/kubeagent/internal/logscan"`. Add `Logs bool` to `Options` (after `KubeletHealth`).

**Placement is correctness-critical:** insert the enrichment block **between** these two existing lines —

```go
	findings := diagnose.Run(detectors, collect.FactsFrom(inputs.Pods, attachEvents))
	// <-- enrichment goes HERE
	workloads := inventory.Assemble(inputs, findings)
```

`inventory.Assemble` copies each finding **by value** into its workload's `Findings`, so the flat `findings` slice must be enriched *before* that call (enriching after `Assemble` would mutate throwaway copies). Insert:

```go
	if opts.Logs {
		for i := range findings {
			if findings[i].Container == "" {
				continue
			}
			ns, name, ok := splitNamespacedName(findings[i].Pod) // "ns/pod"
			if !ok {
				continue
			}
			if log, ok := collect.PreviousLogs(ctx, client, ns, name, findings[i].Container); ok {
				clue := logscan.Classify(log)
				if clue.Cause != "" {
					findings[i].LogCause = clue.Cause
					findings[i].LogExcerpt = clue.Excerpt
				}
			}
		}
	}
```

`findings[i].Pod` is `"namespace/name"`. If `internal/scan` (or `internal/inventory`) already has a split helper, use it; otherwise add this unexported helper in `scan.go`:

```go
func splitNamespacedName(s string) (ns, name string, ok bool) {
	if i := strings.IndexByte(s, '/'); i > 0 && i < len(s)-1 {
		return s[:i], s[i+1:], true
	}
	return "", "", false
}
```

(add `"strings"` to `scan.go` imports if not already present). Ensure the enrichment runs BEFORE `findings` are attached to the workloads/inventory, so the enriched findings are what the report and `--explain` see — place it immediately after `diagnose.Run` and before whatever consumes `findings`.

- [ ] **Step 5: Wire `main.go`**

Add the flag near the other scan flags:

```go
	logs := fs.Bool("logs", false, "read each crashing container's previous logs and classify the failure (needs the pods/log grant)")
```

Add `[--logs]` to the scan usage string (near `[--kubelet-health]`). Add to the `scan.Options{...}` literal:

```go
		Logs:                   *logs,
```

- [ ] **Step 6: Run the tests**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && gofmt -l internal/collect/collect.go internal/scan/scan.go main.go && go test ./internal/scan/ && go test ./...`
Expected: build OK, gofmt silent, the new scan test passes, full suite green.

- [ ] **Step 7: Commit**

```bash
git add internal/collect/collect.go internal/scan/scan.go main.go
git commit -m "feat(scan): opt-in --logs enriches crash findings with a log root cause"
```

---

### Task 4: Report rendering + golden snapshot

**Files:**
- Modify: `internal/report/report.go` (render the log block in `printWorkload`)
- Modify: `internal/report/report_test.go` (rendering test)
- Modify: `internal/report/golden_test.go` + `internal/report/testdata/golden-scan.txt` (snapshot the new rendering)

**Interfaces:**
- Consumes: `Finding.LogCause`/`LogExcerpt` (Task 2/3).

- [ ] **Step 1: Write the failing test**

Add to `internal/report/report_test.go`:

```go
func TestPrintInventory_ShowsLogRootCause(t *testing.T) {
	var buf bytes.Buffer
	in := Input{
		Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded", NodesReady: 1, NodesTotal: 1},
		Result: inventory.Result{Workloads: []inventory.Workload{{
			Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Findings: []diagnose.Finding{{
				Pod: "shop/web-1", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting",
				Evidence: `container "web", restartCount=8`, Container: "web",
				LogExcerpt: "panic: runtime error: invalid memory address", LogCause: "application panic (code bug)",
			}},
		}}},
	}
	if err := PrintInventory(in, "text", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "logs (previous container):") ||
		!strings.Contains(out, "panic: runtime error: invalid memory address") ||
		!strings.Contains(out, "→ application panic (code bug)") {
		t.Errorf("missing log root-cause block:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintInventory_ShowsLogRootCause`
Expected: FAIL — the block is not rendered.

- [ ] **Step 3: Render the log block**

In `internal/report/report.go`, inside `printWorkload`'s `for _, f := range wl.Findings { ... }` loop, AFTER the `Evidence` render (and after any `f.Resources` line for OOM findings — i.e. as the last per-finding output), add:

```go
		if f.LogExcerpt != "" {
			if _, err := fmt.Fprintf(w, "      logs (previous container):\n        %s\n        → %s\n", f.LogExcerpt, f.LogCause); err != nil {
				return err
			}
		}
```

- [ ] **Step 4: Verify + snapshot the golden**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/report/ -run TestPrintInventory_ShowsLogRootCause` → PASS.

Then add a log clue to one crash finding in the golden fixture so the new format is snapshotted. In `internal/report/golden_test.go`, in `goldenWorkloads()`, on the `web` workload's `diagnose.Finding` (the CrashLoopBackOff one), add fields:

```go
Container: "web", LogExcerpt: "panic: runtime error: invalid memory address", LogCause: "application panic (code bug)",
```

Regenerate and re-run:

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run TestGoldenScanOutput -update
go test ./internal/report/ -run TestGolden -count=1 && go test ./internal/report/ -run TestGolden -count=1   # deterministic, twice
```

Read `internal/report/testdata/golden-scan.txt` and confirm the `web` finding now shows the `logs (previous container):` block. Then:

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./... && gofmt -l internal/report/report.go internal/report/golden_test.go`
Expected: all green, gofmt silent.

- [ ] **Step 5: Commit**

```bash
git add internal/report/report.go internal/report/report_test.go internal/report/golden_test.go internal/report/testdata/golden-scan.txt
git commit -m "feat(report): render the crash log root cause under a finding"
```

---

### Task 5: `--explain` sends the derived cause only

**Files:**
- Modify: `internal/explain/explain.go` (include `LogCause` in the workload prompt)
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `Finding.LogCause`/`LogExcerpt` (Task 2/3).

- [ ] **Step 1: Write the failing test**

Add to `internal/explain/explain_test.go` (call the same unexported `buildInventoryPrompt` the existing tests use; match their construction of `cluster`/`workloads`):

```go
func TestBuildInventoryPrompt_IncludesLogCauseNotExcerpt(t *testing.T) {
	workloads := []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0,
		Findings: []diagnose.Finding{{
			Pod: "shop/web-1", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Evidence: "restartCount=8",
			LogCause: "application panic (code bug)", LogExcerpt: "panic: SECRET_TOKEN=abc123",
		}},
	}}
	prompt := buildInventoryPrompt(clusterhealth.ClusterHealth{Verdict: "Degraded"}, nil, nil, nil, workloads)
	if !strings.Contains(prompt, "application panic (code bug)") {
		t.Errorf("prompt should include the derived LogCause:\n%s", prompt)
	}
	if strings.Contains(prompt, "SECRET_TOKEN") || strings.Contains(prompt, "panic: SECRET_TOKEN") {
		t.Errorf("prompt must NOT include the raw LogExcerpt:\n%s", prompt)
	}
}
```

(Add any imports the test needs — `inventory`, `diagnose`, `clusterhealth`, `strings` — matching the file's existing imports.)

- [ ] **Step 2: Run to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/explain/ -run TestBuildInventoryPrompt_IncludesLogCauseNotExcerpt`
Expected: FAIL — the prompt does not include `LogCause`.

- [ ] **Step 3: Include `LogCause` in the prompt**

In `internal/explain/explain.go`, the workload findings loop (line ~123) currently writes:

```go
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
			}
```

Change the body to append the derived cause when present (never the excerpt):

```go
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
				if f.LogCause != "" {
					fmt.Fprintf(&b, "      log cause: %s\n", f.LogCause)
				}
			}
```

- [ ] **Step 4: Run to verify it passes**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/explain/ && go test ./...`
Expected: PASS; full suite green.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/explain.go internal/explain/explain_test.go
git commit -m "feat(explain): include the derived log cause (never raw log text)"
```

---

### Task 6: RBAC add-on + docs

**Files:**
- Create: `deploy/rbac-logs.yaml`
- Modify: `CHANGELOG.md`, `website/docs/features/diagnostics.md`, `website/docs/quickstart.md`, `README.md`, `deploy/README.md`

- [ ] **Step 1: RBAC add-on**

Create `deploy/rbac-logs.yaml` (mirror `deploy/rbac-diskusage.yaml`'s shape — a ClusterRole + binding to the `kubeagent` ServiceAccount; read it first for the exact metadata/subject):

```yaml
# Opt-in add-on: grants read access to container logs via the pods/log subresource,
# needed only when running `scan --logs`. Apply alongside deploy/ for a restricted
# context; most human kubeconfigs already allow pods/log. Without it, --logs simply
# reports no log cause (non-fatal). kubeagent stays strictly get/list/watch otherwise.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-pods-log
rules:
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-pods-log
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-pods-log
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 2: CHANGELOG**

In `CHANGELOG.md` under `## [Unreleased]` → `### Added`, add:

```markdown
- **Crash log root-cause (opt-in).** `scan --logs` reads each crashing container's
  previous-instance logs (`pods/log`) and classifies the failure into a plain-language
  cause — `application panic (code bug)`, `cannot reach a dependency (…) — connection
  refused`, `bad command or entrypoint`, etc. — shown under the finding as
  `logs (previous container): … → <cause>` and in JSON as `logCause`/`logExcerpt`. Only
  the crash findings (CrashLoopBackOff / RestartLoop / OOMKilled) are probed. Read-only,
  scan-only; needs the `pods/log` grant (`deploy/rbac-logs.yaml`). `--explain` receives
  only the derived cause, never raw log text.
```

- [ ] **Step 3: Website + README**

- `website/docs/features/diagnostics.md`: add a `### Crash log root-cause (opt-in)` subsection (match neighbouring subsections' prose): `scan --logs` fetches the crashed container's `--previous` logs and classifies the failure line; list a few signatures; note read-only/opt-in/scan-only, the `pods/log` add-on, and that `--explain` gets only the derived cause.
- `website/docs/quickstart.md`: add to the flags code block: `# read a crashing container's previous logs and classify the failure` / `./kubeagent scan --logs`.
- `README.md`: add one bullet to the feature list, in the style of the other detectors: `**Crash log root-cause (opt-in)** — scan --logs reads a crashing container's previous logs and names the failure (panic, connection refused, bad entrypoint, …). Needs the pods/log grant.`
- `deploy/README.md`: one line noting `rbac-logs.yaml` as the `--logs` add-on (alongside the `rbac-diskusage.yaml` mention).

- [ ] **Step 4: Verify docs build**

Run: `cd website && <mkvenv>/bin/mkdocs build --strict -f mkdocs.yml` (recreate the venv per prior tasks if missing).
Expected: "Documentation built", no page WARNINGs (ignore the Material team banner).
Also: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...` (should still be green — docs/RBAC only).

- [ ] **Step 5: Commit**

```bash
git add deploy/rbac-logs.yaml CHANGELOG.md website/docs/features/diagnostics.md website/docs/quickstart.md README.md deploy/README.md
git commit -m "docs: document the --logs crash log root-cause + pods/log add-on"
```
