# `--fix` Audit Log Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `--fix --audit-log <path>`, kubeagent appends one JSON-Lines record per remediation action — across every disposition (dry-run, declined, applied, refused, error) — to a durable, append-only, secret-free audit file.

**Architecture:** A new `internal/audit` package owns the durable `Record` and an append-only JSON-Lines `Writer`. `remediate.Result` gains a `Refused` flag on its guarded no-write paths so `runFixes` has a clean disposition signal. `main.go` adds the `--audit-log` flag (fail-fast file open, 0o600, O_APPEND) and threads an `*audit.Writer` into `runFixes`, which logs one record per action after determining its disposition.

**Tech Stack:** Go 1.26, stdlib only (`encoding/json`, `os`, `time`), client-go fake clientset in tests. No new dependency.

## Global Constraints

- **Writes stay guard-railed and opt-in.** The only remediation-behavior change is the additive `Result.Refused` flag (no new writes, no changed writes). The audit file is written after each action's outcome is known; logging never changes what is applied.
- **No secrets in the audit record.** Only `remediate.Change` values (revisions/images/counts), booleans, and our own `Target`/`Detail`/`Kind`/`Namespace`/`Name` strings — never env, specs, or secrets.
- **Append-only, `0o600`, JSON Lines** — one standalone JSON object per line; file opened `O_CREATE|O_APPEND|O_WRONLY`.
- **Fail fast on an unwritable audit path** — open the file (returning an error from `run`) BEFORE any mutation; a mid-run `Log` error warns to stderr but does not abort.
- **No new dependency. No RBAC/Helm change** (chart PATCH). **Golden snapshot unchanged.**
- **No `Co-Authored-By: Claude` trailer.** **TDD** — failing test first. **gofmt-clean.** `go build ./... && go test ./...` before every commit.

## File Structure

- `internal/audit/audit.go` — `Record`, `RecordFor`, `Writer`, `NewWriter`, `Log`. One responsibility: the durable audit record and its append-only writer.
- `internal/remediate/remediate.go` — `Result.Refused` flag on the three nil-Err refusal returns.
- `main.go` — `--audit-log` flag, fail-fast open, `runFixes` audit param + per-disposition logging.
- Docs: `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`, `website/docs/roadmap.md`.

**Disposition reconciliation (important):** `remediate` has these non-applied return paths in `applyRolloutUndo`/`applyUncordon`:
- protected-namespace guard, `get`/`list`/`update` failures → set `res.Err` (disposition **error**)
- rollout `target == nil` ("no differing prior revision"), rollout revision-drift bond, uncordon precondition → `Applied` false, `Err` nil, "no write made" (disposition **refused**)

Only the three nil-Err "no write made" paths get `Refused = true`. The protected-namespace guard keeps its `Err` (it is correctly an error, not a silent refusal). `runFixes` maps disposition by checking **`Err` first**, then `Applied`, then `Refused`, so an errored path is never mislabeled.

---

### Task 1: `internal/audit` package

**Files:**
- Create: `internal/audit/audit.go`
- Test: `internal/audit/audit_test.go`

**Interfaces:**
- Consumes: `remediate.Action` (fields `Kind, Namespace, Name, Target string`, `Changes []remediate.Change`), `remediate.Change` (`Field, From, To string` with json tags).
- Produces (Task 3 relies on these exact names):
  - `type Record struct { Time, Kind, Namespace, Name, Target string; Changes []remediate.Change; Disposition, Detail string }` (json tags below)
  - `func RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record`
  - `type Writer struct { ... }`
  - `func NewWriter(w io.Writer) *Writer`
  - `func (a *Writer) Log(r Record) error`

- [ ] **Step 1: Write the failing test**

Create `internal/audit/audit_test.go`:

```go
package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/remediate"
)

var fixedNow = time.Date(2026, 7, 24, 6, 30, 0, 0, time.UTC)

func TestRecordFor_MapsActionAndDisposition(t *testing.T) {
	a := remediate.Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web",
		Target:  "shop/web (Deployment)",
		Changes: []remediate.Change{{Field: "revision", From: "5", To: "4"}},
	}
	r := RecordFor(a, "applied", "rolled back shop/web to revision 4", fixedNow)
	if r.Time != "2026-07-24T06:30:00Z" {
		t.Errorf("time = %q, want RFC3339 UTC", r.Time)
	}
	if r.Kind != "RolloutUndo" || r.Namespace != "shop" || r.Name != "web" || r.Target != "shop/web (Deployment)" {
		t.Errorf("action fields not mapped: %+v", r)
	}
	if r.Disposition != "applied" || r.Detail != "rolled back shop/web to revision 4" {
		t.Errorf("disposition/detail wrong: %+v", r)
	}
	if len(r.Changes) != 1 || r.Changes[0] != (remediate.Change{Field: "revision", From: "5", To: "4"}) {
		t.Errorf("changes not passed through: %+v", r.Changes)
	}
}

func TestRecordFor_NodeActionEmptyNamespace(t *testing.T) {
	a := remediate.Action{Kind: "Uncordon", Name: "worker-1", Target: "node/worker-1"}
	r := RecordFor(a, "dry-run", "", fixedNow)
	if r.Namespace != "" {
		t.Errorf("node action namespace = %q, want empty", r.Namespace)
	}
	if r.Disposition != "dry-run" {
		t.Errorf("disposition = %q", r.Disposition)
	}
}

func TestWriter_LogWritesOneJSONLinePerCall(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Log(RecordFor(remediate.Action{Kind: "Uncordon", Name: "n1", Target: "node/n1"}, "applied", "uncordoned node n1", fixedNow)); err != nil {
		t.Fatal(err)
	}
	if err := w.Log(RecordFor(remediate.Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web", Target: "shop/web (Deployment)"}, "refused", "state changed since preview; no write made", fixedNow)); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Errorf("line %d is not standalone JSON: %v (%q)", i, err, line)
		}
	}
	// spot-check the second record's disposition round-trips
	var second Record
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second.Disposition != "refused" {
		t.Errorf("second disposition = %q, want refused", second.Disposition)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriter_LogSurfacesWriteError(t *testing.T) {
	w := NewWriter(failWriter{})
	if err := w.Log(RecordFor(remediate.Action{Kind: "Uncordon", Name: "n1"}, "applied", "", fixedNow)); err == nil {
		t.Error("expected a write error to surface")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/audit/`
Expected: FAIL — the package/`RecordFor`/`NewWriter` don't exist yet.

- [ ] **Step 3: Implement `audit.go`**

```go
// Package audit writes a durable, append-only JSON-Lines record of every --fix
// remediation outcome. It records only safe display values (the same revision /
// image / count fields the preview shows, plus our own detail strings) — never
// pod specs, env, or secrets.
package audit

import (
	"encoding/json"
	"io"
	"time"

	"github.com/imantaba/kubeagent/internal/remediate"
)

// Record is one durable audit entry: what was proposed and what became of it.
type Record struct {
	Time        string             `json:"time"`
	Kind        string             `json:"kind"`
	Namespace   string             `json:"namespace,omitempty"`
	Name        string             `json:"name"`
	Target      string             `json:"target"`
	Changes     []remediate.Change `json:"changes,omitempty"`
	Disposition string             `json:"disposition"`
	Detail      string             `json:"detail,omitempty"`
}

// RecordFor builds a Record from an action, its disposition, a detail string, and a
// clock. Pure — no I/O. now is formatted as RFC3339 in UTC.
func RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record {
	return Record{
		Time:        now.UTC().Format(time.RFC3339),
		Kind:        a.Kind,
		Namespace:   a.Namespace,
		Name:        a.Name,
		Target:      a.Target,
		Changes:     a.Changes,
		Disposition: disposition,
		Detail:      detail,
	}
}

// Writer appends JSON-Lines records to an underlying writer (the open audit file,
// or any io.Writer in tests). One JSON object per line.
type Writer struct {
	w io.Writer
}

// NewWriter wraps w as an audit Writer.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Log marshals r to a single JSON line (terminated by "\n") and writes it. It
// returns any marshal or write error.
func (a *Writer) Log(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = a.w.Write(b)
	return err
}
```

- [ ] **Step 4: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/audit/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/audit/
git commit -m "feat(audit): JSON-Lines remediation audit record and writer"
```

---

### Task 2: `remediate.Result.Refused` flag

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Produces: `Result.Refused bool` — true on the three nil-Err "no write made" paths (rollout no-target, rollout drift bond, uncordon precondition); false on a clean apply and on error paths.

The three paths to flag are (current line references):
- `applyRolloutUndo`: the `target == nil` return with Detail `"no differing prior revision to roll back to (state changed); no write made"`.
- `applyRolloutUndo`: the revision-drift bond return with the `"state changed since preview ..."` Detail.
- `applyUncordon`: the `"node is no longer a safe uncordon target ...; no write made"` return.

Do NOT flag the protected-namespace guard or the get/list/update error returns (they set `Err`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/remediate/remediate_test.go`:

```go
func TestApply_RefusedFlagOnDrift(t *testing.T) {
	cur := depObj("shop", "web", "nginx:still-broken", "3")
	r1 := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	r2 := rsWithImage("shop", "web-2", "web", "2", "nginx:broken")
	r3 := rsWithImage("shop", "web-3", "web", "3", "nginx:still-broken")
	cli := fake.NewSimpleClientset(cur, &r1, &r2, &r3)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1,
	})
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("drift must set Refused (not Applied, no Err), got %+v", res)
	}
}

func TestApply_RefusedFlagOnNoTarget(t *testing.T) {
	cur := depObj("shop", "web", "nginx:x", "2")
	only := rsWithImage("shop", "web-2", "web", "2", "nginx:x") // only the current revision
	cli := fake.NewSimpleClientset(cur, &only)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("no-target must set Refused, got %+v", res)
	}
}

func TestApply_RefusedFlagOnUncordonPrecondition(t *testing.T) {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}} // already schedulable
	cli := fake.NewSimpleClientset(n)
	res := Apply(context.Background(), cli, Action{Kind: "Uncordon", Name: "worker-1"})
	if res.Applied || res.Err != nil || !res.Refused {
		t.Fatalf("uncordon precondition must set Refused, got %+v", res)
	}
}

func TestApply_CleanApplyNotRefused(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	res := Apply(context.Background(), cli, Action{
		Kind: "RolloutUndo", Namespace: "shop", Name: "web", CurrentRevision: 2, TargetRevision: 1,
	})
	if !res.Applied || res.Refused {
		t.Fatalf("clean apply must not be Refused, got %+v", res)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -run TestApply_Refused`
Expected: FAIL — `res.Refused` unknown field.

- [ ] **Step 3: Add the field and set it on the three paths**

In the `Result` struct, add after `Applied bool`:

```go
	Refused bool // a guarded no-write refusal (drift, no target, unsafe precondition); Applied false, Err nil
```

In `applyRolloutUndo`, the `target == nil` return — set the flag before returning:

```go
	if target == nil {
		res.Detail = "no differing prior revision to roll back to (state changed); no write made"
		res.Refused = true
		return res
	}
```

The revision-drift bond return:

```go
	if curRev != a.CurrentRevision || targetRev != a.TargetRevision {
		res.Detail = fmt.Sprintf(
			"state changed since preview (revision %d is now current and the rollback would land on %d; previewed %d → %d) — re-run kubeagent scan --fix; no write made",
			curRev, targetRev, a.CurrentRevision, a.TargetRevision)
		res.Refused = true
		return res
	}
```

In `applyUncordon`, the precondition return:

```go
	if !n.Spec.Unschedulable || hasNoExecuteTaint(*n) {
		res.Detail = "node is no longer a safe uncordon target (already schedulable or NoExecute-tainted); no write made"
		res.Refused = true
		return res
	}
```

Leave the protected-namespace guard and all `res.Err = ...` returns unchanged.

- [ ] **Step 4: Run the tests**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./internal/remediate/`
Expected: PASS (new Refused tests + all existing Apply/Plan tests unchanged).

- [ ] **Step 5: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add internal/remediate/
git commit -m "feat(remediate): Result.Refused flag on guarded no-write paths"
```

---

### Task 3: `--audit-log` flag + `runFixes` logging (`main.go`)

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `audit.NewWriter`, `audit.RecordFor`, `audit.Writer` (Task 1); `remediate.Result.Refused` (Task 2).
- Produces: `runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer)` — new trailing `*audit.Writer` param (nil ⇒ no logging).

- [ ] **Step 1: Write the failing test**

Add to `main_test.go` (import `"github.com/imantaba/kubeagent/internal/audit"`, `"encoding/json"`, `"strings"` if not present):

```go
func auditLines(t *testing.T, s string) []audit.Record {
	t.Helper()
	var recs []audit.Record
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		var r audit.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line not JSON: %v (%q)", err, line)
		}
		recs = append(recs, r)
	}
	return recs
}

func TestRunFixes_AuditRecordsDryRun(t *testing.T) {
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true /*dryRun*/, false, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "dry-run" {
		t.Fatalf("want one dry-run record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsDeclined(t *testing.T) {
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, false, false, &out, strings.NewReader("n\n"), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "declined" {
		t.Fatalf("want one declined record, got %+v", recs)
	}
}

func TestRunFixes_AuditRecordsApplied(t *testing.T) {
	// Mirror TestRunFixes_YesApplies' live-appliable fixtures exactly.
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	var out, auditBuf bytes.Buffer
	actions := remediate.Plan(fixWorkload(), rss, nil)
	runFixes(context.Background(), cli, actions, false, true /*yes*/, &out, strings.NewReader(""), audit.NewWriter(&auditBuf))
	recs := auditLines(t, auditBuf.String())
	if len(recs) != 1 || recs[0].Disposition != "applied" {
		t.Fatalf("want one applied record, got %+v", recs)
	}
}

func TestRunFixes_NilAuditWriterLogsNothing(t *testing.T) {
	var out bytes.Buffer
	actions := remediate.Plan(fixWorkload(), fixRS(), nil)
	runFixes(context.Background(), fake.NewSimpleClientset(), actions, true, false, &out, strings.NewReader(""), nil)
	// no panic, no audit output; the human output still rendered
	if !strings.Contains(out.String(), "Proposed fix") {
		t.Error("human output should still render with a nil audit writer")
	}
}
```

Note on fixtures: reuse the existing `fixWorkload()` / `fixRS()` helpers. For the applied case, use the same fixtures the existing `TestRunFixes_YesApplies` uses to build a live-appliable fake clientset — if that test constructs its clientset inline, factor a tiny helper `fixApplyFixtures() ([]inventory.Workload, []appsv1.ReplicaSet, *fake.Clientset)` returning the workloads, replicasets, and a clientset seeded with the matching Deployment+ReplicaSets, OR inline the same construction the existing test uses. Match whatever `TestRunFixes_YesApplies` already does.

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go test . -run TestRunFixes_Audit`
Expected: FAIL — compile error (runFixes takes 7 args, tests pass 8).

- [ ] **Step 3: Add the audit param + per-disposition logging to `runFixes`**

Change the signature and add logging at each disposition point:

```go
func runFixes(ctx context.Context, client kubernetes.Interface, actions []remediate.Action, dryRun, assumeYes bool, w io.Writer, in io.Reader, auditw *audit.Writer) {
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	logAudit := func(a remediate.Action, disposition, detail string) {
		if auditw == nil {
			return
		}
		if err := auditw.Log(audit.RecordFor(a, disposition, detail, time.Now())); err != nil {
			fmt.Fprintf(w, "  (audit log write failed: %v)\n", err)
		}
	}
	reader := bufio.NewReader(in)
	for _, a := range actions {
		fmt.Fprintf(w, "\nProposed fix: %s — %s\n  reason: %s\n", a.Target, a.Summary, a.Reason)
		if len(a.Changes) > 0 {
			fmt.Fprintln(w, "  will change:")
			for _, c := range a.Changes {
				if c.From == "" && c.To == "" {
					fmt.Fprintf(w, "    %s\n", c.Field)
				} else {
					fmt.Fprintf(w, "    %s: %s → %s\n", c.Field, c.From, c.To)
				}
			}
		}
		fmt.Fprintf(w, "  kubectl equivalent: %s\n", a.KubectlEquivalent)
		if dryRun {
			fmt.Fprintln(w, "  (dry-run: not applied)")
			logAudit(a, "dry-run", "")
			continue
		}
		if !assumeYes {
			fmt.Fprint(w, "  Apply? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Fprintln(w, "  skipped.")
				logAudit(a, "declined", "")
				continue
			}
		}
		res := remediate.Apply(ctx, client, a)
		switch {
		case res.Err != nil:
			fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
			logAudit(a, "error", res.Err.Error())
		case res.Applied:
			fmt.Fprintf(w, "  applied: %s\n", res.Detail)
			logAudit(a, "applied", res.Detail)
		default:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
			logAudit(a, "refused", res.Detail)
		}
	}
}
```

(The `default` case covers `res.Refused` and any other non-applied, non-error outcome — both map to `refused`, consistent with the Err-first ordering.)

- [ ] **Step 4: Add the `--audit-log` flag and fail-fast wiring in `runScan`**

Near the other fix flags (after `assumeYes`):

```go
	auditLog := fs.String("audit-log", "", "with --fix: append a JSON-lines audit record per action to this file")
```

Add `[--audit-log path]` inside the `--fix` group of the usage string (line ~61): change `[--fix [--dry-run|--yes]]` to `[--fix [--dry-run|--yes] [--audit-log path]]`.

Replace the fix callsite block:

```go
	if *fix {
		var auditw *audit.Writer
		if *auditLog != "" {
			f, err := os.OpenFile(*auditLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("opening audit log %q: %w", *auditLog, err)
			}
			defer f.Close()
			auditw = audit.NewWriter(f)
		}
		runFixes(context.Background(), client, fixPlan, *dryRun, *assumeYes, os.Stdout, os.Stdin, auditw)
	}
```

Add the import `"github.com/imantaba/kubeagent/internal/audit"`. `os` and `time` are already imported.

- [ ] **Step 5: Update the existing `TestRunFixes_*` calls to the new signature**

The four existing `TestRunFixes_*` tests call `runFixes(..., &out, strings.NewReader(...))` — add a trailing `, nil` (no audit writer) to each.

- [ ] **Step 6: Build, test, and smoke**

Run: `cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build ./... && go test . ./internal/...`
Expected: PASS (including golden — no report change).

Binary smoke:

```bash
cd /home/ubuntu/git/kubeagent && export PATH=$PATH:/usr/local/go/bin && go build -o kubeagent .
./kubeagent scan --help 2>&1 | grep -i audit-log      # flag documented
./kubeagent scan --fix --audit-log /root/no/such/dir/x 2>&1 | head -1   # unwritable path → fail-fast error (if a cluster is reachable; otherwise a cluster error is fine)
rm -f kubeagent
```

Expected: the `--audit-log` flag appears in help.

- [ ] **Step 7: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add main.go main_test.go
git commit -m "feat: --audit-log flag records every --fix disposition (JSON lines)"
```

---

### Task 4: Docs

**Files:**
- Modify: `website/docs/features/remediation.md`
- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `website/docs/roadmap.md`

**Interfaces:** none (docs only).

- [ ] **Step 1: remediation.md** — add an `### Audit log (`--audit-log`)` subsection: what it does (append-only JSON-Lines record per action), the invocation `kubeagent scan --fix --yes --audit-log /var/log/kubeagent-fix.log`, a sample two-line jsonl excerpt (one `applied`, one `refused`) using placeholder registries/names, and the disposition vocabulary (`dry-run|declined|applied|refused|error`). Note it is `0o600`, append-only, and records every disposition including dry-run. Match the page's tone.

- [ ] **Step 2: README.md** — extend the `--fix` mention: "with `--audit-log <path>`, appends a JSON-lines record of every remediation outcome (applied / refused / declined / dry-run / error)".

- [ ] **Step 3: CHANGELOG.md** — under `## [Unreleased]` → `### Added`:

```markdown
- **`--fix` audit log.** A new `--audit-log <path>` flag (with `--fix`) appends a
  durable, append-only JSON-Lines record of every remediation outcome — one line per
  action with its timestamp, target, previewed changes, and disposition
  (`dry-run` / `declined` / `applied` / `refused` / `error`). Secret-free by
  construction (only the previewed diff values and result detail are recorded); the
  file is opened `0o600` and append-only, and an unwritable path fails before any
  write. The accountability half of the remediation contract.
```

- [ ] **Step 4: roadmap.md** — under Theme D's "Shipped" list, add: `--fix` audit log (`--audit-log`, append-only JSON-Lines record of every remediation disposition) — the accountability half of the remediation contract.

- [ ] **Step 5: Verify the site builds**

Run: `cd /home/ubuntu/git/kubeagent/website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkvenv/bin/mkdocs build --strict -f mkdocs.yml 2>&1 | tail -1` (fall back to `mkdocs` on PATH if missing)
Expected: `Documentation built`, no page WARNINGs.

- [ ] **Step 6: Commit**

```bash
cd /home/ubuntu/git/kubeagent
git add website/docs/features/remediation.md README.md CHANGELOG.md website/docs/roadmap.md
git commit -m "docs: document the --fix audit log"
```

---

## Release (after all tasks + whole-branch review)

- **Gate: FULL CHAOS GATE** — touches the `--fix` write path (`runFixes` signature, `Result.Refused`). `unset ANTHROPIC_API_KEY && ./chaos/run.sh --recreate` (backgrounded, ~7 min); every scenario green.
- **Version:** minor **v0.51.0 → v0.52.0**.
- **Chart: PATCH** — no RBAC/Helm/template change.

## Self-Review notes (author)

- **Spec coverage:** audit package + Record/RecordFor/Writer/Log (Task 1), Result.Refused on the nil-Err refusal paths (Task 2), --audit-log flag + fail-fast open + per-disposition logging + nil-writer no-op (Task 3), docs (Task 4), chaos/version/chart (Release). The spec's "record every disposition" is covered by Task 3's five logging points; "no secrets" holds because Record only carries Change/Detail/Target/Kind/Namespace/Name.
- **Type consistency:** `audit.RecordFor(a remediate.Action, disposition, detail string, now time.Time) Record` and `(*audit.Writer).Log(Record) error` are used identically in Task 3; `Result.Refused` produced in Task 2 and consumed only implicitly (the runFixes `default` branch covers it) — the disposition mapping checks Err→Applied→default, so Refused never needs a direct read, which is intentional and noted.
- **Reconciliation flagged:** the protected-namespace guard stays an `error` disposition (it sets Err); only the three nil-Err "no write made" paths get `Refused`. Documented in the File Structure section and Task 2.
- **No import cycle:** `audit` imports `remediate`; `remediate` does not import `audit`.
