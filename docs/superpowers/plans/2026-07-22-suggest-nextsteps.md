# Deterministic --suggest next-steps — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Under an opt-in `--suggest` flag, print a deterministic (never-LLM) next-step suggestion and a read-only `kubectl` investigation command under each pod finding.

**Architecture:** A new pure `internal/remediation` package maps a finding's `Issue` to a fixed, reviewed `Suggestion{NextStep, Command}` (command templated from the finding's pod/container). `report.printWorkload` renders the two lines under each finding when `--suggest` is set; `main.go` adds the flag. Deterministic, offline (no API key), read-only — kubeagent prints the command, never runs it.

**Tech Stack:** Go 1.26, standard-library `flag`. Pure functions; tests use fake findings + `bytes.Buffer` render checks.

## Global Constraints

- **READ-ONLY.** Suggestions and commands are text; kubeagent never runs them. The mapped commands are ONLY `kubectl logs` / `describe pod` / `get events` — never a mutating verb.
- **Deterministic & never-LLM** — a fixed reviewed mapping; no clock, no cluster calls, no model. Same input → same output.
- **Opt-in; offline** — a plain `--suggest` bool flag (no env, no API key), consistent with `--explain`/`--certs`/`--security`. Default output unchanged (off by default).
- **No new collector, RBAC, watch gauge, `Result` field, or JSON field.**
- **v1 uses the standard-library `flag` package only** — no Cobra.
- **No `Co-Authored-By: Claude` trailer** (or any Claude attribution) on any commit. Every commit authored solely by the human.
- **TDD** — write the failing test first, watch it fail, then implement. **gofmt-clean.**
- The suggestion render lines are `      ↳ next step: <NextStep>` and `      ↳ try: <Command>` (6-space indent, matching the finding evidence indent).

---

### Task 1: `internal/remediation` — the deterministic mapping

**Files:**
- Create: `internal/remediation/remediation.go`
- Test: `internal/remediation/remediation_test.go`

**Interfaces:**
- Consumes: `diagnose.Finding` (fields `Issue`, `Pod` = "ns/name", `Container`).
- Produces: `type Suggestion struct { NextStep string; Command string }`; `func For(f diagnose.Finding) Suggestion`.

- [ ] **Step 1: Write the failing test**

Create `internal/remediation/remediation_test.go`:

```go
package remediation

import (
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

func TestFor_TableAndCommands(t *testing.T) {
	cases := []struct {
		issue, container, wantStepSub, wantCmd string
	}{
		{"CrashLoopBackOff", "web", "inspect the crash output", "kubectl -n shop logs web-abc -c web --previous"},
		{"RestartLoop", "web", "inspect the crash output", "kubectl -n shop logs web-abc -c web --previous"},
		{"ImagePullBackOff", "", "the image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"ErrImagePull", "", "the image can't be pulled", "kubectl -n shop describe pod web-abc"},
		{"OOMKilled", "", "exceeded its memory limit", "kubectl -n shop describe pod web-abc"},
		{"Unschedulable", "", "no node can place the pod", "kubectl -n shop describe pod web-abc"},
		{"CreateContainerConfigError", "", "referenced ConfigMap or Secret is missing", "kubectl -n shop describe pod web-abc"},
		{"ProbeFailure", "", "the probe keeps failing", "kubectl -n shop describe pod web-abc"},
		{"VolumeAttachError", "", "the volume can't attach", "kubectl -n shop describe pod web-abc"},
		{"Init:CrashLoopBackOff", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"Init:OOMKilled", "wait-db", "an init container is failing", "kubectl -n shop logs web-abc -c wait-db --previous"},
		{"FailedCreate", "", "the controller can't create pods", "kubectl -n shop get events --field-selector reason=FailedCreate"},
		{"JobFailed", "", "exhausted its retries", "kubectl -n shop logs web-abc --previous"},
		{"SomethingNew", "", "inspect the object for details", "kubectl -n shop describe pod web-abc"},
	}
	for _, tc := range cases {
		f := diagnose.Finding{Issue: tc.issue, Pod: "shop/web-abc", Container: tc.container}
		got := For(f)
		if !strings.Contains(got.NextStep, tc.wantStepSub) {
			t.Errorf("%s: NextStep %q, want it to contain %q", tc.issue, got.NextStep, tc.wantStepSub)
		}
		if got.Command != tc.wantCmd {
			t.Errorf("%s: Command = %q, want %q", tc.issue, got.Command, tc.wantCmd)
		}
	}
}

func TestFor_OmitsContainerWhenEmpty(t *testing.T) {
	f := diagnose.Finding{Issue: "CrashLoopBackOff", Pod: "shop/web-abc"} // no Container
	if got := For(f).Command; got != "kubectl -n shop logs web-abc --previous" {
		t.Fatalf("Command = %q, want no -c flag", got)
	}
}

func TestFor_CommandsAreNeverMutating(t *testing.T) {
	bad := []string{"delete", "apply", "edit", "patch", "scale", "rollout", "cordon", "drain", "create ", "replace"}
	issues := []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "OOMKilled", "Unschedulable",
		"CreateContainerConfigError", "ProbeFailure", "VolumeAttachError", "Init:CrashLoopBackOff", "Init:OOMKilled",
		"FailedCreate", "JobFailed", "RestartLoop", "whatever-default"}
	for _, iss := range issues {
		cmd := For(diagnose.Finding{Issue: iss, Pod: "ns/pod", Container: "c"}).Command
		for _, b := range bad {
			if strings.Contains(cmd, b) {
				t.Errorf("%s: command %q contains a mutating verb %q", iss, cmd, b)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediation/`
Expected: FAIL — `undefined: For`.

- [ ] **Step 3: Write the implementation**

Create `internal/remediation/remediation.go`:

```go
// Package remediation maps a diagnosed finding to a deterministic, reviewed next
// step — a concise cause direction and a read-only kubectl command to investigate.
// Never LLM-decided; the command is printed for the operator, never run.
package remediation

import (
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/diagnose"
)

// Suggestion is a deterministic next step for a finding.
type Suggestion struct {
	NextStep string // concise cause direction / what to do
	Command  string // a read-only kubectl command to investigate ("" when N/A)
}

// For returns the suggestion for a finding, keyed on its Issue. An unrecognized
// Issue gets a safe generic describe suggestion.
func For(f diagnose.Finding) Suggestion {
	ns, pod := splitPod(f.Pod)
	switch f.Issue {
	case "CrashLoopBackOff", "RestartLoop":
		return Suggestion{"starts then crashes — inspect the crash output", logsCmd(ns, pod, f.Container)}
	case "ImagePullBackOff", "ErrImagePull":
		return Suggestion{"the image can't be pulled — verify the tag exists and the registry credentials", describeCmd(ns, pod)}
	case "OOMKilled":
		return Suggestion{"the container exceeded its memory limit — raise the limit or fix the leak", describeCmd(ns, pod)}
	case "Unschedulable":
		return Suggestion{"no node can place the pod — check resource requests, taints, and affinity", describeCmd(ns, pod)}
	case "CreateContainerConfigError":
		return Suggestion{"a referenced ConfigMap or Secret is missing — create it or fix the reference", describeCmd(ns, pod)}
	case "ProbeFailure":
		return Suggestion{"the probe keeps failing — check the probe config and the app's health endpoint", describeCmd(ns, pod)}
	case "VolumeAttachError":
		return Suggestion{"the volume can't attach — check the PVC/PV binding and the CSI driver", describeCmd(ns, pod)}
	case "Init:CrashLoopBackOff", "Init:ImagePullBackOff", "Init:OOMKilled":
		return Suggestion{"an init container is failing — the pod cannot start until it succeeds", logsCmd(ns, pod, f.Container)}
	case "FailedCreate":
		return Suggestion{"the controller can't create pods — check for quota, LimitRange, or a rejecting admission webhook", eventsCmd(ns, "FailedCreate")}
	case "JobFailed":
		return Suggestion{"the Job exhausted its retries — inspect the failed pod's logs", logsCmd(ns, pod, "")}
	default:
		return Suggestion{"inspect the object for details", describeCmd(ns, pod)}
	}
}

func splitPod(p string) (ns, name string) {
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

func logsCmd(ns, pod, container string) string {
	c := ""
	if container != "" {
		c = " -c " + container
	}
	return fmt.Sprintf("kubectl -n %s logs %s%s --previous", ns, pod, c)
}

func describeCmd(ns, pod string) string {
	return fmt.Sprintf("kubectl -n %s describe pod %s", ns, pod)
}

func eventsCmd(ns, reason string) string {
	return fmt.Sprintf("kubectl -n %s get events --field-selector reason=%s", ns, reason)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/remediation/`
Expected: PASS (all tests). Then `gofmt -l internal/remediation/remediation.go internal/remediation/remediation_test.go` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediation/
git commit -m "feat(remediation): deterministic next-step suggestions per finding"
```

---

### Task 2: `report` + `main` — the `--suggest` flag and rendering

**Files:**
- Modify: `internal/report/report.go` (`Input.Suggest`, `printWorkload` param + suggest lines, import)
- Test: `internal/report/report_test.go` (a suggest on/off render test)
- Modify: `main.go` (the `--suggest` flag + wiring)
- Test: `main_test.go` (flag-parse test)

**Interfaces:**
- Consumes: `remediation.For(f diagnose.Finding) Suggestion` (Task 1).
- Produces: `report.Input.Suggest bool`; `printWorkload(wl inventory.Workload, now time.Time, suggest bool, w io.Writer) error`.

- [ ] **Step 1: Write the failing report test**

Add to `internal/report/report_test.go` (mirror how a neighbouring test builds an `Input` with a degraded workload carrying a `CrashLoopBackOff` finding — e.g. the test around line 1349 — and calls `PrintInventory(in, "text", &buf)`):

```go
func TestPrintInventory_SuggestLines(t *testing.T) {
	build := func(suggest bool) string {
		var buf bytes.Buffer
		in := Input{
			Cluster: clusterhealth.ClusterHealth{Verdict: "Degraded"},
			Result: inventory.Prioritized{Workloads: []inventory.Workload{{
				Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
				Findings: []diagnose.Finding{{Pod: "shop/web-abc", Issue: "CrashLoopBackOff", Reason: "keeps crashing", Container: "web"}},
			}}},
			Suggest: suggest,
		}
		if err := PrintInventory(in, "text", &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	on := build(true)
	if !strings.Contains(on, "↳ next step: starts then crashes — inspect the crash output") {
		t.Errorf("missing next-step line:\n%s", on)
	}
	if !strings.Contains(on, "↳ try: kubectl -n shop logs web-abc -c web --previous") {
		t.Errorf("missing try line:\n%s", on)
	}

	off := build(false)
	if strings.Contains(off, "next step:") || strings.Contains(off, "↳ try:") {
		t.Errorf("no suggest lines expected by default:\n%s", off)
	}
}
```

> Match the `Input` / `inventory.Prioritized` / `Result` field construction to the real types used by neighbouring report tests (e.g. the `Result:` field's type — use exactly what the existing tests use). The assertions on the two suggest lines and their absence-by-default are the binding part.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/report/ -run TestPrintInventory_SuggestLines`
Expected: FAIL — `unknown field Suggest in struct literal`.

- [ ] **Step 3: Implement the report change**

In `internal/report/report.go`:

1. Add `"github.com/imantaba/kubeagent/internal/remediation"` to imports.
2. Add to the `Input` struct (next to `SecurityVerbose bool`):

```go
	Suggest bool
```

3. Change `printWorkload`'s signature to accept `suggest`:

```go
func printWorkload(wl inventory.Workload, now time.Time, suggest bool, w io.Writer) error {
```

and update its call site (the workload render loop) to pass `in.Suggest`:

```go
			if err := printWorkload(wl, now, in.Suggest, w); err != nil {
```

4. Inside `printWorkload`'s `for _, f := range wl.Findings` loop, AFTER the
   `f.LogExcerpt` block (the last per-finding block) and before the loop closes,
   add:

```go
		if suggest {
			s := remediation.For(f)
			if s.NextStep != "" {
				if _, err := fmt.Fprintf(w, "      ↳ next step: %s\n", s.NextStep); err != nil {
					return err
				}
			}
			if s.Command != "" {
				if _, err := fmt.Fprintf(w, "      ↳ try: %s\n", s.Command); err != nil {
					return err
				}
			}
		}
```

- [ ] **Step 4: Run the report test to verify it passes**

Run: `go test ./internal/report/`
Expected: PASS (the existing golden test is unaffected — its `Input` has `Suggest` unset/false).

- [ ] **Step 5: Write the main flag test (failing) and add the flag**

Add to `main_test.go` (mirror `TestRun_FixFlagsAccepted`):

```go
func TestRun_SuggestFlagAccepted(t *testing.T) {
	// --suggest must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed.
	err := run([]string{"scan", "--suggest", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}
```

Run: `go test . -run TestRun_SuggestFlagAccepted` → FAIL (`flag provided but not defined: -suggest`).

Then in `main.go`, add the flag next to the other `scan` bool flags (e.g. after `security`):

```go
	suggest := fs.Bool("suggest", false, "print a deterministic next-step suggestion (and a read-only kubectl command) under each finding")
```

and wire it into the `report.Input` in the presentation-extras block (next to `in.SecurityVerbose = *securityVerbose`):

```go
	in.Suggest = *suggest
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/report/ . 2>&1 | tail` and `go build ./...`
Expected: PASS / builds. Then `gofmt -l internal/report/report.go internal/report/report_test.go main.go main_test.go` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/report/ main.go main_test.go
git commit -m "feat(report): render deterministic next-steps under --suggest"
```

---

### Task 3: Docs

**Files:**
- Modify: `website/docs/features/diagnostics.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`

**Interfaces:** none (docs).

- [ ] **Step 1: Update docs**

- `website/docs/features/diagnostics.md`: add a short "`--suggest`" note — an opt-in flag that prints a deterministic next-step and a read-only `kubectl` investigation command under each pod finding; works offline (no API key); kubeagent prints the command, never runs it. Show the example block from the spec.
- `README.md`: add `--suggest` to the flags/usage list.
- `CHANGELOG.md`: under `## [Unreleased]` → `### Added`, add a bullet:
  ```
  - **`--suggest` next steps.** An opt-in flag prints a deterministic, reviewed
    next-step suggestion and a read-only `kubectl` investigation command under each
    pod finding (CrashLoopBackOff → check the previous logs, ImagePullBackOff →
    verify the tag/credentials, …). Offline (no API key), never LLM-decided, and
    read-only — it prints the command, it never runs it.
  ```
- `website/docs/roadmap.md`: add it to the Shipped list, noting it's the first Theme-C (principled intelligence) slice — the deterministic remediation core.

- [ ] **Step 2: Verify docs build (only website/ changed)**

Run: `(cd website && mkdocs build --strict -f mkdocs.yml)` (use the venv mkdocs if not on PATH: `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs`).
Expected: exit 0, "Documentation built", no WARNING about your pages.

- [ ] **Step 3: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 4: Commit**

```bash
git add website/ README.md CHANGELOG.md
git commit -m "docs: document the --suggest deterministic next-steps flag"
```

---

## Release (after all tasks + whole-branch review)

Not a task — the release skill owns this. Touches `internal/remediation` (new) + `internal/report` + `main.go` — no collect/cluster/watch/RBAC/Helm change → **LIGHTWEIGHT SMOKE** gate (a Kind cluster with a crashing pod; run `scan --suggest` and confirm the next-step/try lines render). **Minor** version bump **v0.41.0 → v0.42.0**; **patch** chart bump (no Helm template change — the bump script's default patch is correct; do NOT override to minor). Hold for the user's explicit "run release and push".
