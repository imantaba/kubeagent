# Hypothesis Engine — Slice 1 (trace + `scan --why`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The root-cause attribution pass keeps every candidate it evaluates — node, PVC, registry — each with a closed-set verdict (`attributed` / `ruled_out` / `outranked`) and one evidence sentence; `scan --output json` always carries the trace on a workload row and `scan --why` renders it in the text report, with scan's schema moving 1.7 → 1.8 (additive).

**Architecture:** Two new types in `internal/inventory` (`Hypothesis`, the named string type `Verdict`) plus two `omitempty` fields on `Workload`; the three annotators in `internal/rootcause` gain a `record` helper and record every candidate instead of discarding losers; `internal/confidence.Annotate` stores the attribution confidence beside the `RootCause` it already grades; `internal/report.printWorkload` gains a `why` parameter; `internal/cli` gains the `--why` flag. **`internal/scan/scan.go` needs no change** — the annotation chain (`scan.go:811-818`) already runs in the right order and the trace rides on values it already passes. Attribution behavior is untouched: same rules, same precedence, byte-identical `RootCause` strings, byte-identical default text output.

**Spec:** `docs/superpowers/specs/2026-08-18-hypothesis-engine-design.md` (approach A, slice 1). **Two spec claims are stale and corrected here:** the spec says scan's schema moves "1.3 → 1.4" — reality on main is **1.7 → 1.8** (`internal/jsonschema/jsonschema.go:54`); and the branch has since been rebased onto main (v1.19.0), so every line number in this plan is against current main.

**Tech Stack:** Go 1.26 stdlib only. No new dependency.

## Global Constraints

Every task's requirements implicitly include all of these:

- **READ-ONLY toward the cluster, and SEPARATELY: no LLM call.** Two promises, never blurred. Nothing in this slice touches a client, a context, or a model. `internal/rootcause`, `internal/confidence` and `internal/inventory` stay pure (no client, no context, no I/O).
- **Attribution behavior is byte-identical.** `RootCause` strings, rule precedence (node → PVC → registry), thresholds, and the default (`--why`-less) text output do not change. All 28 existing tests in `internal/rootcause/rootcause_test.go` must pass **unmodified**. `internal/report/testdata/golden-scan.txt` stays byte-identical; `go test ./internal/report -run TestGolden` must pass **unmodified — never run it with `-update`**.
- **The ONE permitted `-update`** in this build is `go test ./internal/schemadoc -run TestSchemaDrift -update` in Task 1, the CLAUDE.md-mandated regeneration for the scan 1.7→1.8 move. After running it, `git status --short` must show `website/docs/schemas/scan.schema.json` as the only regenerated file, and the drift test's classification must be additive (MINOR). No other test is ever run with `-update`, for any reason.
- **No trace in `findings.Finding`.** `internal/findings`, `internal/gate`, `internal/fleet`, `internal/policy` are untouched. `gate` stays schema 1.1; the other six documents do not move.
- **NO NEW DEPENDENCY:** `git diff --stat main -- go.mod go.sum` must be empty at the end of every task.
- **Untrusted API text is sanitized at ingress** (`internal/safetext.Line`), never at renderers, and **matching runs on the RAW value**. The registry host stays wrapped in `safetext.Line` exactly where the existing code wraps it (`rootcause.go:89`); node names, PVC names and pod names are API-server-validated (DNS-1123) and are embedded unwrapped, matching the existing `RootCause` strings.
- **Do NOT regenerate the demo GIF or `website/docs/quickstart.md`** — the default output is byte-identical, so neither moves.
- **Synthetic names only** in every test and doc example: `shop/api`, `worker-2`, `ghcr.io`, `data-0`. No real infra identifier of any kind.
- **TDD:** write the failing test first, watch it fail, then implement. Run tests with `go test -p 2`, never `-short`.
- **Every commit `git commit -s`** (DCO, identity `imantaba <itn.taba@gmail.com>`), authored solely by the human — NO `Co-Authored-By`, no AI attribution anywhere. Commit messages never cite a `docs/testing/` path or a scenario record ID.
- Work on branch `hypothesis-engine`, **never on `main`**. Never run `./chaos/run.sh` in any form. No cluster is needed for any task.
- `internal/report` may import `internal/confidence` and `internal/inventory` (it already does). No package wall changes: nothing here imports `internal/remediate` or `internal/explain`.
- MkDocs check (Task 8 only): the ONLY working mkdocs is `/tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs`; run it from `website/` and `cd` back to the repo root afterwards.

## Fixed vocabulary (used verbatim across tasks)

- **Kinds** (plain string, exactly three values): `"node"`, `"pvc"`, `"registry"`.
- **Verdicts** (named type `inventory.Verdict`, closed set): `attributed`, `ruled_out`, `outranked`.
- **Cause strings:** an `attributed` entry's `Cause` equals the workload's `RootCause` exactly. A node candidate is `"node NAME (REASON)"`; a PVC candidate is `"PVC NAME (REASON)"`; a registry candidate that is *not* attributed is the bare `"registry HOST"` (no count — the count exists only when a group clears the threshold); an undeterminable-image candidate is `"registry unknown"`.
- **Reasons:**
  - node attributed: `"pod <first pod of the workload on that node> is scheduled on it"`
  - node ruled_out: `"no pod of this workload is scheduled on it"`
  - PVC attributed: `"pod <first pod mounting it> mounts it"`
  - PVC ruled_out: `"not mounted by this workload's pods"`
  - registry attributed: `"<N> workloads failing to pull from this host clear the threshold of 2"`
  - registry ruled_out (threshold): `"only workload failing to pull from this host; threshold is 2"`
  - registry ruled_out (no image): `"image reference undeterminable"`
  - **every** outranked entry, all kinds: `"<the workload's RootCause> is the stronger cause"`
- **Noise bound (spec):** only flagged workloads get trace entries, and only when candidates exist (a healthy cluster records nothing; `Annotate` with no down nodes, `AnnotatePVC` with no issues, `AnnotateRegistry` on workloads without pull findings all record nothing).
- **Trace order:** append order = evaluation order — `Annotate` (nodes, sorted), then `AnnotatePVC` (issue keys, sorted, own-namespace only), then `AnnotateRegistry` (hosts, sorted; per-workload entries in the workload loop) — matching the chain in `internal/scan/scan.go:811-818`.

---

### Task 1: Trace types + scan schema 1.7 → 1.8 (one atomic commit)

This task is deliberately schema-complete in ONE commit: adding JSON-tagged fields to `inventory.Workload`, or the named string type `inventory.Verdict`, individually leaves the suite red — `TestSchemaDrift` fails on a shape change without a version bump, and `TestEveryNamedStringTypeIsEnumeratedOrFreeForm` (`internal/schemadoc/schemadoc_test.go`) fails on an unregistered named string type. Types, version bump, enum registration, schema regeneration and the doc version lines land together.

**Files:**
- Modify: `internal/inventory/inventory.go` (Workload struct ends at line 56; `RolloutChange` follows at 61)
- Modify: `internal/jsonschema/jsonschema.go:28-54` (ScanVersion comment + value)
- Modify: `internal/schemadoc/schemadoc.go:97-118` (enums map) + its import block
- Regenerate: `website/docs/schemas/scan.schema.json` (via the one permitted `-update`)
- Modify: `website/docs/features/json-schema.md` (two version fragments)
- Test: `internal/inventory/inventory_test.go` (append; file exists, `package inventory`, already imports `strings` — add `encoding/json`)

**Interfaces:**
- Produces (used verbatim by Tasks 2–7):
  - `type Verdict string` with constants `VerdictAttributed Verdict = "attributed"`, `VerdictRuledOut Verdict = "ruled_out"`, `VerdictOutranked Verdict = "outranked"`
  - `type Hypothesis struct { Cause string; Kind string; Verdict Verdict; Reason string }` (json tags `cause`, `kind`, `verdict`, `reason`)
  - `Workload.RootCauseTrace []Hypothesis` (json `rootCauseTrace,omitempty`) and `Workload.RootCauseConfidence string` (json `rootCauseConfidence,omitempty`)

- [ ] **Step 1: Write the failing test** — append to `internal/inventory/inventory_test.go` (add `"encoding/json"` to its import block):

```go
func TestWorkloadTraceFieldsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Workload{Namespace: "shop", Name: "api", Kind: "Deployment"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"rootCauseTrace", "rootCauseConfidence"} {
		if strings.Contains(string(b), key) {
			t.Errorf("workload with no trace encodes %q: %s", key, b)
		}
	}
}

func TestWorkloadTraceRoundTrips(t *testing.T) {
	want := Workload{
		Namespace: "shop", Name: "api", Kind: "Deployment",
		RootCauseTrace: []Hypothesis{{
			Cause:   "node worker-2 (NotReady)",
			Kind:    "node",
			Verdict: VerdictAttributed,
			Reason:  "pod api-a is scheduled on it",
		}},
		RootCauseConfidence: "high",
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Workload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.RootCauseTrace) != 1 || got.RootCauseTrace[0] != want.RootCauseTrace[0] {
		t.Errorf("trace = %+v, want %+v", got.RootCauseTrace, want.RootCauseTrace)
	}
	if got.RootCauseConfidence != "high" {
		t.Errorf("rootCauseConfidence = %q, want high", got.RootCauseConfidence)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test -p 2 ./internal/inventory -run 'TestWorkloadTrace'`
Expected: FAIL — compile error, `undefined: Hypothesis` / `undefined: VerdictAttributed`.

- [ ] **Step 3: Add the types and fields.** In `internal/inventory/inventory.go`, extend `Workload` — the struct currently ends (line 55):

```go
	RootCause       string             `json:"rootCause,omitempty"`       // "node X (reason)", "registry Y (N workloads failing to pull)", or "PVC Z (reason)" — root-cause attribution (hint; set by rootcause.Annotate/AnnotatePVC/AnnotateRegistry)
```

Add directly after that line, inside the struct:

```go
	RootCauseTrace      []Hypothesis `json:"rootCauseTrace,omitempty"`      // every root-cause candidate the attribution pass evaluated, whatever the verdict (set by the same three annotators; empty when no candidate existed)
	RootCauseConfidence string       `json:"rootCauseConfidence,omitempty"` // confidence of the RootCause attribution ("high" | "medium"; set by confidence.Annotate, empty when RootCause is)
```

Then add the two types after the `RolloutChange` type (after line 67):

```go
// A Verdict says what the attribution pass concluded about one candidate
// cause. The vocabulary is closed: attributed (this candidate is the
// workload's RootCause), ruled_out (its evidence did not match this
// workload), outranked (its evidence matched, but precedence chose a
// stronger cause).
type Verdict string

const (
	VerdictAttributed Verdict = "attributed"
	VerdictRuledOut   Verdict = "ruled_out"
	VerdictOutranked  Verdict = "outranked"
)

// A Hypothesis is one candidate root cause the attribution pass evaluated for
// a workload, kept whatever the verdict was. The trace exists so an operator
// can see what was considered and rejected, not only what won: "ruled out"
// and "outranked" are answers, not omissions. Kind is one of "node", "pvc",
// "registry".
type Hypothesis struct {
	Cause   string  `json:"cause"`
	Kind    string  `json:"kind"`
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
}
```

- [ ] **Step 4: Run the inventory tests**

Run: `go test -p 2 ./internal/inventory`
Expected: PASS.

- [ ] **Step 5: Watch the schema guards go red** (this is the TDD red for the schema half)

Run: `go test -p 2 ./internal/schemadoc`
Expected: FAIL twice — `TestSchemaDrift` (scan's shape changed with no version bump) and `TestEveryNamedStringTypeIsEnumeratedOrFreeForm` (`inventory.Verdict` is a named string type registered nowhere). Report both failures verbatim; do not proceed if the failures are different ones.

- [ ] **Step 6: Bump the version and register the enum.** In `internal/jsonschema/jsonschema.go`, extend the ScanVersion comment and value. Replace (lines 40-54):

```go
	// deterministic next step `scan --suggest` already prints in the text
	// report, populated only when the flag is set; 1.7 added `kind` on a
	// finding, the kind of the object the finding's `pod` names when it is
	// not a pod ("Job" or "CronJob"), set only by the JobFailed producer.
	// All seven are additive: every
```

with:

```go
	// deterministic next step `scan --suggest` already prints in the text
	// report, populated only when the flag is set; 1.7 added `kind` on a
	// finding, the kind of the object the finding's `pod` names when it is
	// not a pod ("Job" or "CronJob"), set only by the JobFailed producer;
	// 1.8 added `rootCauseTrace` and `rootCauseConfidence` on a workload row
	// — every root-cause candidate the attribution pass evaluated, each with
	// a closed-set verdict, plus the stored confidence of the winning
	// attribution. All nine are additive: every
```

and replace (lines 50-53):

```go
	// key, a scan without `--suggest` legitimately encodes no `suggestion`
	// key, a pod-level finding legitimately encodes no `kind` key, but a
	// property in `required` is a MAJOR change however new or however often
	// it is set.
	ScanVersion     = "1.7"
```

with:

```go
	// key, a scan without `--suggest` legitimately encodes no `suggestion`
	// key, a pod-level finding legitimately encodes no `kind` key, a workload
	// with no evaluated candidate legitimately encodes no `rootCauseTrace`
	// key and an unattributed one no `rootCauseConfidence` key, but a
	// property in `required` is a MAJOR change however new or however often
	// it is set.
	ScanVersion     = "1.8"
```

In `internal/schemadoc/schemadoc.go`, add to the `enums` map (after the `"policy.Level"` entry at line 115-117):

```go
	"inventory.Verdict": {
		string(inventory.VerdictAttributed), string(inventory.VerdictRuledOut),
		string(inventory.VerdictOutranked),
	},
```

and add `"github.com/imantaba/kubeagent/internal/inventory"` to schemadoc.go's import block (schemadoc deliberately imports the surface packages — this is allowed by CLAUDE.md's invariants).

- [ ] **Step 7: Regenerate the published scan schema — the ONE permitted `-update`**

Run: `go test ./internal/schemadoc -run TestSchemaDrift -update`
Expected: PASS, output naming the scan change **additive (MINOR)**. If it says BREAKING, STOP and report — something is wrong with the field tags.

- [ ] **Step 8: Verify exactly what moved**

Run: `git status --short` and `git diff --stat`
Expected: modified `internal/inventory/inventory.go`, `internal/inventory/inventory_test.go`, `internal/jsonschema/jsonschema.go`, `internal/schemadoc/schemadoc.go`, `website/docs/schemas/scan.schema.json` — and NOTHING else. No other file under `website/docs/schemas/` moved.
Then run: `grep -c '"ruled_out"' website/docs/schemas/scan.schema.json`
Expected: at least 1 (the verdict enum landed in the published schema).

- [ ] **Step 9: Update the two version fragments in `website/docs/features/json-schema.md`.** Find them with `grep -n '1\.7' website/docs/features/json-schema.md`. Change the fragment `` `1.7`, having gained seven `` to `` `1.8`, having gained nine `` (line ~52) and the fragment `` still works against `1.7` `` to `` still works against `1.8` `` (line ~59). Touch nothing else in the file.

- [ ] **Step 10: Full verification**

Run: `go build ./... && go test -p 2 ./internal/inventory ./internal/schemadoc ./internal/jsonschema ./internal/report`
Expected: all PASS. The report run proves the golden files were untouched by the type addition (goldenInput sets `RootCause` directly and never runs annotators, so nothing renders differently).
Run: `git diff --stat main -- go.mod go.sum`
Expected: empty.

- [ ] **Step 11: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go internal/jsonschema/jsonschema.go internal/schemadoc/schemadoc.go website/docs/schemas/scan.schema.json website/docs/features/json-schema.md
git commit -s -m "feat(inventory): root-cause hypothesis trace types; scan schema 1.8

Workload gains rootCauseTrace (every candidate the attribution pass
evaluated, with a closed-set verdict) and rootCauseConfidence, both
omitempty and additive. inventory.Verdict registers in schemadoc's enum
map, and the published scan schema regenerates via the drift test's
-update, classified additive (MINOR)."
```

---

### Task 2: Node rule records its candidates

**Files:**
- Modify: `internal/rootcause/rootcause.go:19-59` (`Annotate` + new helpers)
- Test: `internal/rootcause/rootcause_test.go` (append; `package rootcause`, helpers `wl(ns, name, ready, desired, nodes...)` — pods are named `name-a`, `name-b`, … in node order)

**Interfaces:**
- Consumes: `inventory.Hypothesis`, `inventory.Verdict*` constants (Task 1).
- Produces (used by Tasks 3–4): unexported helpers in package rootcause:
  - `func record(w *inventory.Workload, cause, kind string, verdict inventory.Verdict, reason string)` — appends one Hypothesis to `w.RootCauseTrace`
  - `func podOn(w inventory.Workload, node string) string` — first pod of w placed on node, `""` when none

**Behavior contract:** `RootCause` outcomes are byte-identical to today for every input — all existing `TestAnnotate_*` tests pass unmodified. New behavior: for each **flagged** workload and each down node (sorted order), exactly one trace entry — `ruled_out` when no pod of the workload is on that node; `attributed` when it is the winning (first sorted, hosting) node; `outranked` when a pod is on it but an earlier node already won. Not-flagged workloads and an empty down list record nothing.

- [ ] **Step 1: Write the failing tests** — append to `internal/rootcause/rootcause_test.go`:

```go
func TestAnnotate_TraceAttributed(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	want := inventory.Hypothesis{
		Cause:   "node worker-2 (NotReady)",
		Kind:    "node",
		Verdict: inventory.VerdictAttributed,
		Reason:  "pod api-a is scheduled on it",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotate_TraceRuledOutNodeNotHosting(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-9")} // healthy node
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if ws[0].RootCause != "" {
		t.Fatalf("RootCause = %q, want empty", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "node worker-2 (NotReady)",
		Kind:    "node",
		Verdict: inventory.VerdictRuledOut,
		Reason:  "no pod of this workload is scheduled on it",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotate_TraceOutrankedSameKindTie(t *testing.T) {
	// Pods on two down nodes: sorted-first worker-2 wins (existing pinned
	// behavior), worker-5 matched evidence but lost — outranked, not dropped.
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-5", "worker-2")}
	down := []clusterhealth.DownNode{{Name: "worker-5", Reason: "NotReady"}, {Name: "worker-2", Reason: "NotReady"}}
	Annotate(ws, down)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Fatalf("RootCause = %q", ws[0].RootCause)
	}
	if len(ws[0].RootCauseTrace) != 2 {
		t.Fatalf("trace has %d entries, want 2: %+v", len(ws[0].RootCauseTrace), ws[0].RootCauseTrace)
	}
	wantSecond := inventory.Hypothesis{
		Cause:   "node worker-5 (NotReady)",
		Kind:    "node",
		Verdict: inventory.VerdictOutranked,
		Reason:  "node worker-2 (NotReady) is the stronger cause",
	}
	if ws[0].RootCauseTrace[1] != wantSecond {
		t.Errorf("trace[1] = %+v, want %+v", ws[0].RootCauseTrace[1], wantSecond)
	}
	if ws[0].RootCauseTrace[0].Verdict != inventory.VerdictAttributed {
		t.Errorf("trace[0].Verdict = %q, want attributed", ws[0].RootCauseTrace[0].Verdict)
	}
}

func TestAnnotate_TraceNamesPodOnTheNode(t *testing.T) {
	// api-a is on the healthy worker-9; api-b is the pod on the down node —
	// the attributed reason must name api-b, not the first pod overall.
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-9", "worker-2")}
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if got := ws[0].RootCauseTrace[0].Reason; got != "pod api-b is scheduled on it" {
		t.Errorf("attributed reason = %q, want pod api-b is scheduled on it", got)
	}
}

func TestAnnotate_TraceEmptyForUnflaggedAndEmptyDown(t *testing.T) {
	healthy := wl("shop", "api", 2, 2, "worker-2")
	healthy.Status = "Running"
	ws := []inventory.Workload{healthy}
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	if len(ws[0].RootCauseTrace) != 0 {
		t.Errorf("unflagged workload got a trace: %+v", ws[0].RootCauseTrace)
	}
	flagged := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")}
	Annotate(flagged, nil)
	if len(flagged[0].RootCauseTrace) != 0 {
		t.Errorf("empty down list produced a trace: %+v", flagged[0].RootCauseTrace)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test -p 2 ./internal/rootcause -run 'TestAnnotate_Trace'`
Expected: FAIL — the trace assertions find empty `RootCauseTrace` (the types exist since Task 1, so these fail on assertions, not compilation).

- [ ] **Step 3: Implement.** In `internal/rootcause/rootcause.go`, add the two helpers (place after `podNodes`, line 59):

```go
// record appends one evaluated candidate to the workload's trace. The trace
// keeps everything the pass considered, whatever the verdict — "ruled out"
// and "outranked" are answers, not omissions.
func record(w *inventory.Workload, cause, kind string, verdict inventory.Verdict, reason string) {
	w.RootCauseTrace = append(w.RootCauseTrace, inventory.Hypothesis{
		Cause: cause, Kind: kind, Verdict: verdict, Reason: reason,
	})
}

// podOn names the first pod of w placed on node, in w.Pods order. Empty when
// none is — callers use it only after establishing placement.
func podOn(w inventory.Workload, node string) string {
	for _, p := range w.Pods {
		if p.Node == node {
			return p.Name
		}
	}
	return ""
}
```

Replace `Annotate`'s workload loop (lines 35-47) — the surrounding setup (reasonByNode, names, sort) is unchanged:

```go
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() {
			continue
		}
		on := podNodes(*w)
		for _, name := range names {
			cause := "node " + name + " (" + reasonByNode[name] + ")"
			switch {
			case !on[name]:
				record(w, cause, "node", inventory.VerdictRuledOut, "no pod of this workload is scheduled on it")
			case w.RootCause == "":
				w.RootCause = cause
				record(w, cause, "node", inventory.VerdictAttributed, "pod "+podOn(*w, name)+" is scheduled on it")
			default:
				record(w, cause, "node", inventory.VerdictOutranked, w.RootCause+" is the stronger cause")
			}
		}
	}
```

(The old code `break`s after the first attribution; the new loop continues so later candidates are recorded. The attributed node is still the first sorted hosting node, so `RootCause` is unchanged.) Update `Annotate`'s doc comment: append the sentence "Every down node evaluated for a flagged workload is recorded in w.RootCauseTrace with a verdict, whatever the outcome."

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/rootcause`
Expected: PASS — all 28 pre-existing tests (unmodified) AND the five new ones.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./internal/rootcause ./internal/report ./internal/scan`
Expected: PASS (report proves goldens unmoved; scan proves the chain composes).
Run: `git diff --stat main -- go.mod go.sum` → empty. `git status --short` → only `internal/rootcause/rootcause.go` and `internal/rootcause/rootcause_test.go` modified.

- [ ] **Step 6: Commit**

```bash
git add internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -s -m "feat(rootcause): record node candidates in the hypothesis trace

Annotate now records every down node it evaluates for a flagged
workload: attributed, ruled_out (no pod on it), or outranked (a
sorted-earlier node won). RootCause outcomes are byte-identical; the
existing tests pass unmodified."
```

---

### Task 3: PVC rule records its candidates

**Files:**
- Modify: `internal/rootcause/rootcause.go` (`AnnotatePVC`, currently lines 120-161)
- Test: `internal/rootcause/rootcause_test.go` (append; helper `pvcWL(ns, name, podName)` builds a flagged 0/1 Pending Deployment with one named pod)

**Interfaces:**
- Consumes: `record` from Task 2; `inventory.Verdict*` constants from Task 1.

**Behavior contract:** `RootCause` outcomes byte-identical (all existing `TestAnnotatePVC_*` pass unmodified, including `TestAnnotatePVC_NamespaceIsolation` and `TestAnnotatePVC_ExistingRootCausePreserved`). New behavior: for each **flagged** workload, each broken-PVC issue key **in the workload's own namespace** (sorted order) gets one trace entry — `ruled_out` when no pod of the workload mounts it, `attributed` when it wins, `outranked` when mounted but the workload already carries a RootCause (from the node pass, or a sorted-earlier PVC). Foreign-namespace issues are not candidates and record nothing (they could never have matched — `mounted` keys are always own-namespace — so this filter cannot change `RootCause`). The change from today: already-attributed workloads are no longer skipped before evaluation — they are evaluated for `outranked`/`ruled_out` entries without ever writing `RootCause`.

- [ ] **Step 1: Write the failing tests** — append to `internal/rootcause/rootcause_test.go`:

```go
func TestAnnotatePVC_TraceAttributedNamesMountingPod(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	want := inventory.Hypothesis{
		Cause:   "PVC reports-data (ProvisioningFailed)",
		Kind:    "pvc",
		Verdict: inventory.VerdictAttributed,
		Reason:  "pod reports-1 mounts it",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotatePVC_TraceRuledOutNotMounted(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"other-healthy-pvc"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "" {
		t.Fatalf("RootCause = %q, want empty", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "PVC reports-data (ProvisioningFailed)",
		Kind:    "pvc",
		Verdict: inventory.VerdictRuledOut,
		Reason:  "not mounted by this workload's pods",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotatePVC_TraceOutrankedByNodeAttribution(t *testing.T) {
	w := pvcWL("shop", "reports", "reports-1")
	w.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{w}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "shop", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Fatalf("node attribution must survive, got %q", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "PVC reports-data (ProvisioningFailed)",
		Kind:    "pvc",
		Verdict: inventory.VerdictOutranked,
		Reason:  "node worker-2 (NotReady) is the stronger cause",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotatePVC_TraceOutrankedSameKindTie(t *testing.T) {
	// Pod mounts two broken PVCs: alpha-data wins (sorted first, existing
	// pinned behavior), zeta-data matched evidence but lost.
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"zeta-data", "alpha-data"}}
	issues := []pvchealth.Issue{
		{Namespace: "shop", Name: "zeta-data", Reason: "ProvisioningFailed"},
		{Namespace: "shop", Name: "alpha-data", Reason: "FailedBinding"},
	}
	AnnotatePVC(ws, podPVCs, issues)
	if ws[0].RootCause != "PVC alpha-data (FailedBinding)" {
		t.Fatalf("RootCause = %q", ws[0].RootCause)
	}
	if len(ws[0].RootCauseTrace) != 2 {
		t.Fatalf("trace has %d entries, want 2: %+v", len(ws[0].RootCauseTrace), ws[0].RootCauseTrace)
	}
	wantSecond := inventory.Hypothesis{
		Cause:   "PVC zeta-data (ProvisioningFailed)",
		Kind:    "pvc",
		Verdict: inventory.VerdictOutranked,
		Reason:  "PVC alpha-data (FailedBinding) is the stronger cause",
	}
	if ws[0].RootCauseTrace[1] != wantSecond {
		t.Errorf("trace[1] = %+v, want %+v", ws[0].RootCauseTrace[1], wantSecond)
	}
}

func TestAnnotatePVC_TraceSkipsForeignNamespaceCandidates(t *testing.T) {
	ws := []inventory.Workload{pvcWL("shop", "reports", "reports-1")}
	podPVCs := map[string][]string{"shop/reports-1": {"reports-data"}}
	issues := []pvchealth.Issue{{Namespace: "other", Name: "reports-data", Reason: "ProvisioningFailed"}}
	AnnotatePVC(ws, podPVCs, issues)
	if len(ws[0].RootCauseTrace) != 0 {
		t.Errorf("a foreign-namespace PVC is not a candidate, got trace %+v", ws[0].RootCauseTrace)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test -p 2 ./internal/rootcause -run 'TestAnnotatePVC_Trace'`
Expected: FAIL on the trace assertions (empty traces; the outranked-by-node test also fails because today's code skips attributed workloads).

- [ ] **Step 3: Implement.** Replace `AnnotatePVC`'s workload loop (lines 142-160) — setup (reasonByKey, keys, sort) unchanged:

```go
	for i := range workloads {
		w := &workloads[i]
		if !w.Flagged() {
			continue
		}
		mounted := map[string]string{} // "ns/claim" → first pod mounting it
		for _, p := range w.Pods {
			for _, claim := range podPVCs[w.Namespace+"/"+p.Name] {
				key := w.Namespace + "/" + claim
				if _, seen := mounted[key]; !seen {
					mounted[key] = p.Name
				}
			}
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, w.Namespace+"/") {
				continue // a PVC in another namespace is not a candidate for this workload
			}
			name := key[strings.IndexByte(key, '/')+1:]
			cause := "PVC " + name + " (" + reasonByKey[key] + ")"
			pod, isMounted := mounted[key]
			switch {
			case !isMounted:
				record(w, cause, "pvc", inventory.VerdictRuledOut, "not mounted by this workload's pods")
			case w.RootCause == "":
				w.RootCause = cause
				record(w, cause, "pvc", inventory.VerdictAttributed, "pod "+pod+" mounts it")
			default:
				record(w, cause, "pvc", inventory.VerdictOutranked, w.RootCause+" is the stronger cause")
			}
		}
	}
```

Two deliberate changes from today, neither able to move `RootCause`: the `w.RootCause != ""` skip is gone (attributed workloads now collect `outranked`/`ruled_out` entries; the `w.RootCause == ""` case guard is what protects the attribution), and `mounted` is now `map[string]string` so the attributed reason can name the mounting pod. Update `AnnotatePVC`'s doc comment: append "Every own-namespace broken PVC evaluated for a flagged workload is recorded in w.RootCauseTrace with a verdict, whatever the outcome; a PVC in another namespace is not a candidate."

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/rootcause`
Expected: PASS — every pre-existing test unmodified, plus the five new ones.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./internal/rootcause ./internal/report ./internal/scan` → PASS.
Run: `git diff --stat main -- go.mod go.sum` → empty.

- [ ] **Step 6: Commit**

```bash
git add internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -s -m "feat(rootcause): record PVC candidates in the hypothesis trace

AnnotatePVC records every own-namespace broken PVC it evaluates for a
flagged workload — attributed (naming the mounting pod), ruled_out, or
outranked — and now evaluates already-attributed workloads for the
trace without touching their RootCause. Outcomes are byte-identical;
the existing tests pass unmodified."
```

---

### Task 4: Registry rule records its candidates + chain-order integration test

**Files:**
- Modify: `internal/rootcause/rootcause.go` (`AnnotateRegistry` lines 61-94, `pullImage` lines 96-107)
- Test: `internal/rootcause/rootcause_test.go` (append; helpers `pullWL(name, image, issue)` / `pullWLWithImage(name, displayImage, findingImage, issue)`)

**Interfaces:**
- Consumes: `record` (Task 2), `inventory.Verdict*` (Task 1).
- Changes an internal signature: `pullImage(w inventory.Workload) (image string, hasPull bool)` — `hasPull` is now "a pull finding exists" (today's `ok` conflates "no pull finding" with "pull finding whose image is empty"; the trace needs the distinction: no candidate vs `ruled_out` undeterminable). Determinable = `image != ""`. **The grouping set is unchanged** — a workload enters `groups` only when flagged, unattributed, and determinable — so every `RootCause` outcome is byte-identical.

**Behavior contract:** all existing `TestAnnotateRegistry_*` pass unmodified. New entries: a flagged workload with a pull finding gets exactly one registry entry — `ruled_out` `"registry unknown"` / `"image reference undeterminable"` when the finding carries no image; `outranked` bare `"registry HOST"` when already attributed; `attributed` with the full cause string when its group clears the threshold; `ruled_out` bare `"registry HOST"` / threshold reason when it does not. A flagged workload with no pull finding records nothing here.

- [ ] **Step 1: Write the failing tests** — append to `internal/rootcause/rootcause_test.go`:

```go
func TestAnnotateRegistry_TraceAttributedGroup(t *testing.T) {
	ws := []inventory.Workload{
		pullWL("frontend", "ghcr.io/shop/frontend:2.4", "ImagePullBackOff"),
		pullWL("search", "ghcr.io/shop/search:1.9", "ErrImagePull"),
	}
	AnnotateRegistry(ws)
	want := inventory.Hypothesis{
		Cause:   "registry ghcr.io (2 workloads failing to pull)",
		Kind:    "registry",
		Verdict: inventory.VerdictAttributed,
		Reason:  "2 workloads failing to pull from this host clear the threshold of 2",
	}
	for i := range ws {
		if ws[i].RootCause != "registry ghcr.io (2 workloads failing to pull)" {
			t.Fatalf("ws[%d].RootCause = %q", i, ws[i].RootCause)
		}
		if len(ws[i].RootCauseTrace) != 1 || ws[i].RootCauseTrace[0] != want {
			t.Errorf("ws[%d] trace = %+v, want [%+v]", i, ws[i].RootCauseTrace, want)
		}
	}
}

func TestAnnotateRegistry_TraceRuledOutBelowThreshold(t *testing.T) {
	ws := []inventory.Workload{pullWL("frontend", "ghcr.io/shop/frontend:2.4", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" {
		t.Fatalf("RootCause = %q, want empty", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "registry ghcr.io",
		Kind:    "registry",
		Verdict: inventory.VerdictRuledOut,
		Reason:  "only workload failing to pull from this host; threshold is 2",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotateRegistry_TraceOutrankedByEarlierAttribution(t *testing.T) {
	attributed := pullWL("frontend", "ghcr.io/shop/frontend:2.4", "ImagePullBackOff")
	attributed.RootCause = "node worker-2 (NotReady)"
	ws := []inventory.Workload{attributed}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Fatalf("earlier attribution must survive, got %q", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "registry ghcr.io",
		Kind:    "registry",
		Verdict: inventory.VerdictOutranked,
		Reason:  "node worker-2 (NotReady) is the stronger cause",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotateRegistry_TraceRuledOutUndeterminableImage(t *testing.T) {
	ws := []inventory.Workload{pullWLWithImage("frontend", "ghcr.io/shop/frontend:2.4", "", "ImagePullBackOff")}
	AnnotateRegistry(ws)
	if ws[0].RootCause != "" {
		t.Fatalf("RootCause = %q, want empty", ws[0].RootCause)
	}
	want := inventory.Hypothesis{
		Cause:   "registry unknown",
		Kind:    "registry",
		Verdict: inventory.VerdictRuledOut,
		Reason:  "image reference undeterminable",
	}
	if len(ws[0].RootCauseTrace) != 1 || ws[0].RootCauseTrace[0] != want {
		t.Errorf("trace = %+v, want [%+v]", ws[0].RootCauseTrace, want)
	}
}

func TestAnnotateRegistry_TraceEmptyWithoutPullFinding(t *testing.T) {
	ws := []inventory.Workload{wl("shop", "api", 0, 2, "worker-2")} // flagged, no findings
	AnnotateRegistry(ws)
	if len(ws[0].RootCauseTrace) != 0 {
		t.Errorf("no pull finding must record nothing, got %+v", ws[0].RootCauseTrace)
	}
}

func TestTraceAcrossAnnotatorsKeepsScanOrder(t *testing.T) {
	// One workload on a down node, mounting a broken PVC, with a pull finding.
	// internal/scan/scan.go's chain order (Annotate → AnnotatePVC →
	// AnnotateRegistry) must yield: node attributed, then pvc outranked, then
	// registry outranked — append order is evaluation order.
	w := wl("shop", "api", 0, 2, "worker-2")
	w.Findings = append(w.Findings, diagnose.Finding{Pod: "shop/api", Issue: "ImagePullBackOff",
		Reason: "Bad image reference or registry authentication", Image: "ghcr.io/shop/api:2.4"})
	ws := []inventory.Workload{w}
	Annotate(ws, []clusterhealth.DownNode{{Name: "worker-2", Reason: "NotReady"}})
	AnnotatePVC(ws, map[string][]string{"shop/api-a": {"api-data"}},
		[]pvchealth.Issue{{Namespace: "shop", Name: "api-data", Reason: "ProvisioningFailed"}})
	AnnotateRegistry(ws)
	if ws[0].RootCause != "node worker-2 (NotReady)" {
		t.Fatalf("RootCause = %q", ws[0].RootCause)
	}
	got := ws[0].RootCauseTrace
	if len(got) != 3 {
		t.Fatalf("trace has %d entries, want 3: %+v", len(got), got)
	}
	wantKinds := []string{"node", "pvc", "registry"}
	wantVerdicts := []inventory.Verdict{inventory.VerdictAttributed, inventory.VerdictOutranked, inventory.VerdictOutranked}
	for i := range got {
		if got[i].Kind != wantKinds[i] || got[i].Verdict != wantVerdicts[i] {
			t.Errorf("trace[%d] = kind %q verdict %q, want %q %q", i, got[i].Kind, got[i].Verdict, wantKinds[i], wantVerdicts[i])
		}
	}
	for i, h := range got[1:] {
		if h.Reason != "node worker-2 (NotReady) is the stronger cause" {
			t.Errorf("trace[%d] outranked reason = %q", i+1, h.Reason)
		}
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test -p 2 ./internal/rootcause -run 'TestAnnotateRegistry_Trace|TestTraceAcrossAnnotators'`
Expected: FAIL on the trace assertions.

- [ ] **Step 3: Implement.** Replace `AnnotateRegistry` (lines 68-94) and `pullImage` (lines 96-107) with:

```go
func AnnotateRegistry(workloads []inventory.Workload) {
	groups := map[string][]int{}
	for i := range workloads {
		w := &workloads[i]
		img, hasPull := pullImage(*w)
		if !w.Flagged() || !hasPull {
			continue
		}
		if img == "" {
			record(w, "registry unknown", "registry", inventory.VerdictRuledOut, "image reference undeterminable")
			continue
		}
		if w.RootCause != "" {
			record(w, "registry "+safetext.Line(registryHost(img)), "registry", inventory.VerdictOutranked, w.RootCause+" is the stronger cause")
			continue
		}
		host := registryHost(img)
		groups[host] = append(groups[host], i)
	}
	hosts := make([]string, 0, len(groups))
	for h := range groups {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		members := groups[host]
		if len(members) < 2 {
			for _, i := range members {
				record(&workloads[i], "registry "+safetext.Line(host), "registry", inventory.VerdictRuledOut, "only workload failing to pull from this host; threshold is 2")
			}
			continue
		}
		cause := fmt.Sprintf("registry %s (%d workloads failing to pull)", safetext.Line(host), len(members))
		reason := fmt.Sprintf("%d workloads failing to pull from this host clear the threshold of 2", len(members))
		for _, i := range members {
			workloads[i].RootCause = cause
			record(&workloads[i], cause, "registry", inventory.VerdictAttributed, reason)
		}
	}
}

// pullImage returns the first pull finding's Image in w.Findings order, and
// whether the workload has a pull finding at all. An empty image with
// hasPull=true means the finding carries no image reference — the caller
// records that as ruled out rather than grouping under the wrong host,
// arm (B): grouping under the wrong host is worse than not grouping.
func pullImage(w inventory.Workload) (image string, hasPull bool) {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return f.Image, true
		}
	}
	return "", false
}
```

Keep AnnotateRegistry's existing doc comment and append: "Every flagged workload with a pull finding is recorded in w.RootCauseTrace with a verdict — attributed, ruled_out (below threshold, or image undeterminable), or outranked when an earlier rule already attributed it." The grouping condition is byte-equivalent to today's (`!Flagged || RootCause != "" || !determinable` never enters `groups`), so `RootCause` cannot move.

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/rootcause`
Expected: PASS — every pre-existing test unmodified (in particular `TestAnnotateRegistry_NodeAttributionWinsAndShrinksGroup`: the node-attributed workload now also carries an outranked entry, but that test only asserts `RootCause`), plus the six new ones.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./internal/rootcause ./internal/report ./internal/scan` → PASS.
Run: `git diff --stat main -- go.mod go.sum` → empty.

- [ ] **Step 6: Commit**

```bash
git add internal/rootcause/rootcause.go internal/rootcause/rootcause_test.go
git commit -s -m "feat(rootcause): record registry candidates in the hypothesis trace

AnnotateRegistry records every flagged workload with a pull finding —
attributed when its host group clears the threshold of 2, ruled_out
below it or when the image reference is undeterminable, outranked when
node or PVC evidence already won. pullImage now reports whether a pull
finding exists at all, separately from whether its image is
determinable; the grouping set is unchanged and every RootCause
outcome is byte-identical."
```

---

### Task 5: Store the attribution confidence on the workload row

**Files:**
- Modify: `internal/confidence/confidence.go:64-72` (`Annotate`)
- Test: `internal/confidence/confidence_test.go` (append; `package confidence`, imports `diagnose` and `inventory`)

**Interfaces:**
- Consumes: `Workload.RootCauseConfidence` (Task 1). `ForRootCause` is unchanged.
- Position in the chain is already correct: `confidence.Annotate` is the last call in `internal/scan/scan.go`'s annotation chain (line 818), after all three rootcause annotators — no scan.go change.

- [ ] **Step 1: Write the failing test** — append to `internal/confidence/confidence_test.go`:

```go
func TestAnnotate_StoresRootCauseConfidence(t *testing.T) {
	ws := []inventory.Workload{
		{Namespace: "shop", Name: "api", RootCause: "node worker-2 (NotReady)"},
		{Namespace: "shop", Name: "web", RootCause: "registry ghcr.io (2 workloads failing to pull)"},
		{Namespace: "shop", Name: "db", RootCause: "PVC data-0 (ProvisioningFailed)"},
		{Namespace: "shop", Name: "cache"}, // no attribution
	}
	Annotate(ws)
	for i, want := range []string{"high", "medium", "high", ""} {
		if got := ws[i].RootCauseConfidence; got != want {
			t.Errorf("ws[%d].RootCauseConfidence = %q, want %q", i, got, want)
		}
	}
	// Idempotent: a second call writes the same values.
	Annotate(ws)
	if ws[0].RootCauseConfidence != "high" || ws[3].RootCauseConfidence != "" {
		t.Error("Annotate must be idempotent for RootCauseConfidence")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test -p 2 ./internal/confidence -run TestAnnotate_StoresRootCauseConfidence`
Expected: FAIL — `RootCauseConfidence` stays `""` on the attributed rows.

- [ ] **Step 3: Implement.** In `Annotate` (confidence.go:64-72), add the store at the top of the workload loop:

```go
func Annotate(workloads []inventory.Workload) {
	for i := range workloads {
		if c := ForRootCause(workloads[i].RootCause); c != "" {
			workloads[i].RootCauseConfidence = c
		}
		for j := range workloads[i].Findings {
			if workloads[i].Findings[j].Confidence == "" {
				workloads[i].Findings[j].Confidence = ForIssue(workloads[i].Findings[j].Issue)
			}
		}
	}
}
```

Append to Annotate's doc comment: "Annotate also stores the attribution confidence on the workload row (RootCauseConfidence), the same word internal/report renders as a tag, so the JSON document carries it too; an empty or unrecognized RootCause stores nothing."

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/confidence`
Expected: PASS, all tests including the four pre-existing ones unmodified.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./internal/confidence ./internal/scan ./internal/report` → PASS.
Run: `git diff --stat main -- go.mod go.sum` → empty.

- [ ] **Step 6: Commit**

```bash
git add internal/confidence/confidence.go internal/confidence/confidence_test.go
git commit -s -m "feat(confidence): store attribution confidence on the workload row

Annotate writes ForRootCause's grade into RootCauseConfidence beside
the RootCause it grades — the same word the text report renders as a
tag, now carried by the JSON document too. Empty when RootCause is."
```

---

### Task 6: Render the trace in the text report (`Input.Why` + `printWorkload`)

**Files:**
- Modify: `internal/report/report.go` — `Input` struct (add `Why` after `Suggest`, line 197), `printWorkload` signature (line 1403) and body (after the RootCause block ending line 1434), the single caller (line 394)
- Test: Create `internal/report/why_test.go` (`package report`)

**Interfaces:**
- Consumes: `inventory.Hypothesis`, `inventory.Verdict*` (Task 1). `report.go` already imports `strings`, `bytes` is needed only in the test.
- Produces (used by Task 7): `report.Input.Why bool` — presentation-only, **no json tag** (`Input` is never marshalled; the trace itself rides on `inventory.Workload`, which is already in `ScanReport`). New signature: `printWorkload(wl inventory.Workload, now time.Time, suggest, why bool, w io.Writer) error`. `printWorkload` has exactly ONE production caller (report.go:394) and no test callers today — verify with `grep -rn "printWorkload(" internal/`.
- Render format, exactly: `      · considered %s: %s — %s\n` (6-space indent, two under the 4-space `↳ likely caused by`) with `(h.Cause, verdict, h.Reason)` where `verdict = strings.ReplaceAll(string(h.Verdict), "_", " ")` — so `ruled_out` renders as `ruled out`. The trace renders whenever `why` is true and the workload has entries, **including when nothing was attributed** (a ruled-out-everything trace is exactly what `--why` is for).

- [ ] **Step 1: Write the failing tests** — create `internal/report/why_test.go`:

```go
package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/inventory"
)

func whyWorkload() inventory.Workload {
	return inventory.Workload{
		Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
		RootCause:           "node worker-2 (NotReady)",
		RootCauseConfidence: "high",
		RootCauseTrace: []inventory.Hypothesis{
			{Cause: "node worker-2 (NotReady)", Kind: "node", Verdict: inventory.VerdictAttributed, Reason: "pod api-a is scheduled on it"},
			{Cause: "node worker-9 (NotReady)", Kind: "node", Verdict: inventory.VerdictRuledOut, Reason: "no pod of this workload is scheduled on it"},
			{Cause: "PVC data-0 (ProvisioningFailed)", Kind: "pvc", Verdict: inventory.VerdictOutranked, Reason: "node worker-2 (NotReady) is the stronger cause"},
		},
	}
}

func TestPrintWorkloadWhyRendersTrace(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	if err := printWorkload(whyWorkload(), now, false, true, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		"      · considered node worker-2 (NotReady): attributed — pod api-a is scheduled on it\n",
		"      · considered node worker-9 (NotReady): ruled out — no pod of this workload is scheduled on it\n",
		"      · considered PVC data-0 (ProvisioningFailed): outranked — node worker-2 (NotReady) is the stronger cause\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPrintWorkloadWhyRendersTraceWithoutAttribution(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	wl := whyWorkload()
	wl.RootCause, wl.RootCauseConfidence = "", ""
	wl.RootCauseTrace = wl.RootCauseTrace[1:2] // only the ruled-out entry
	var b bytes.Buffer
	if err := printWorkload(wl, now, false, true, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "likely caused by") {
		t.Errorf("unattributed workload must not render a cause line:\n%s", out)
	}
	if !strings.Contains(out, "      · considered node worker-9 (NotReady): ruled out — no pod of this workload is scheduled on it\n") {
		t.Errorf("ruled-out trace must render even without an attribution:\n%s", out)
	}
}

func TestPrintWorkloadWithoutWhyOmitsTrace(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	if err := printWorkload(whyWorkload(), now, false, false, &b); err != nil {
		t.Fatalf("printWorkload: %v", err)
	}
	if strings.Contains(b.String(), "considered") {
		t.Errorf("trace rendered without --why:\n%s", b.String())
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test -p 2 ./internal/report -run 'TestPrintWorkload'`
Expected: FAIL — compile error (`printWorkload` takes 4 args, tests pass 5).

- [ ] **Step 3: Implement.** In `internal/report/report.go`:

(a) Add to `Input` directly after `Suggest bool` (line 197):

```go
	// Why is true when --why was passed: printWorkload then renders the
	// root-cause hypothesis trace under each workload. Presentation-only, no
	// json tag — Input is never marshalled; the trace itself rides on
	// inventory.Workload inside ScanReport, so JSON always carries it.
	Why bool
```

(b) Change the signature (line 1403):

```go
func printWorkload(wl inventory.Workload, now time.Time, suggest, why bool, w io.Writer) error {
```

(c) After the `if wl.RootCause != "" { ... }` block (ends line 1434), before the `if wl.Image != ""` block, insert:

```go
	if why {
		for _, h := range wl.RootCauseTrace {
			verdict := strings.ReplaceAll(string(h.Verdict), "_", " ")
			if _, err := fmt.Fprintf(w, "      · considered %s: %s — %s\n", h.Cause, verdict, h.Reason); err != nil {
				return err
			}
		}
	}
```

(d) Update the one caller (line 394):

```go
			if err := printWorkload(wl, now, in.Suggest, in.Why, w); err != nil {
```

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/report`
Expected: PASS — including `TestGoldenScanOutput` and `TestGoldenPolicyScanOutput` **unmodified** (goldenInput never sets `Why`, and its workloads carry no trace, so the golden bytes are untouched; if a golden test fails, the change is wrong — NEVER run it with `-update`).

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./internal/report ./internal/cli ./internal/htmlreport` → PASS (cli compiles against the unchanged `Input` field addition; htmlreport reuses `report.Input` and must not need any change — it never reads `Why` or the trace).
Run: `git diff --stat main -- go.mod go.sum internal/report/testdata/` → empty for all three.

- [ ] **Step 6: Commit**

```bash
git add internal/report/report.go internal/report/why_test.go
git commit -s -m "feat(report): render the hypothesis trace under --why

printWorkload gains a why parameter and renders each recorded
candidate as '· considered <cause>: <verdict> — <reason>' under the
likely-caused-by line, including when nothing was attributed. Off by
default: without Why the output is byte-identical, and the golden
tests pass unmodified."
```

---

### Task 7: The `scan --why` flag

**Files:**
- Modify: `internal/cli/scan.go` — `scanOptions` (add `why bool` after `suggest bool`, line 68), `bindScanFlags` (add the BoolVar beside `--suggest`, line 121), the presentation fill block (add `in.Why = o.why` beside `in.Suggest = o.suggest`, line 404)
- Test: `internal/cli/cli_test.go` (append to the existing flag tests; extend `TestParseScanFlagsCarriesEveryValue` at line 2785 and `TestParseScanFlagsDefaults` at line 2832)

**Interfaces:**
- Consumes: `report.Input.Why` (Task 6), `scanOptions`/`bindScanFlags`/`parseScanFlags` (existing).
- `in.Suggest = o.suggest` at `internal/cli/scan.go:404` is the ONLY fill site for presentation extras (verified: `grep -rn "\.Suggest = " internal/cli/` has one hit) — mirror it exactly once. `internal/cli/fix.go`'s `resultInput` maps `scan.Result`-derived fields only and is NOT touched.

- [ ] **Step 1: Write the failing tests.** In `internal/cli/cli_test.go`: add `"--why",` to the argument list of `TestParseScanFlagsCarriesEveryValue` (after the `"--expected-nodes", "node-a,node-b",` line) and this assertion in its body:

```go
	if !opts.why {
		t.Error("why = false, want true")
	}
```

Add to `TestParseScanFlagsDefaults`'s body:

```go
	if opts.why {
		t.Error("why = true by default, want false")
	}
```

And append a flag-accepted test, on the model of `TestRun_SuggestFlagAccepted` (cli_test.go:529):

```go
func TestRun_WhyFlagAccepted(t *testing.T) {
	// --why must be a defined flag: this fails on output-format validation
	// (before any cluster call), proving the flag parsed.
	err := Run([]string{"scan", "--why", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected the output-format error (flag accepted), got: %v", err)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test -p 2 ./internal/cli -run 'TestParseScanFlags|TestRun_WhyFlagAccepted'`
Expected: FAIL — compile error (`opts.why` undefined), then after adding only the struct field it would fail with `unknown flag: --why`.

- [ ] **Step 3: Implement.** In `internal/cli/scan.go`:

(a) `scanOptions`: after `suggest bool` (line 68) add:

```go
	why                    bool
```

(b) `bindScanFlags`: after the `--suggest` declaration (line 121) add:

```go
	f.BoolVar(&o.why, "why", false, "print the root-cause hypothesis trace under each workload: every candidate cause considered, with its verdict and evidence")
```

(c) The fill block: after `in.Suggest = o.suggest` (line 404) add:

```go
	in.Why = o.why
```

- [ ] **Step 4: Run the tests**

Run: `go test -p 2 ./internal/cli`
Expected: PASS — the whole package, including `plugin_flags_test.go` (it pins the flags the shipped plugin skill text names; a NEW flag does not fail it — verify, and if it does fail, read its message and report rather than editing shipped skill text in this task).

- [ ] **Step 5: Full verification**

Run: `go build ./... && go test -p 2 ./...`
Expected: every package PASS (first full-suite run since Task 1; catches any cross-package surprise).
Run: `git diff --stat main -- go.mod go.sum` → empty.
Then a no-cluster smoke of the rendered help: `go build -o kubeagent . && ./kubeagent scan --help 2>&1 | grep -A1 -- '--why'` → shows the new flag and usage line; `rm kubeagent` afterwards (the binary is untracked — do not commit it).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/scan.go internal/cli/cli_test.go
git commit -s -m "feat(cli): scan --why flag

Declares --why on scan and wires it to report.Input.Why beside
--suggest's fill. Text output without the flag is byte-identical."
```

---

### Task 8: Documentation (diagnostics, CHANGELOG, CLAUDE.md, roadmap) + mkdocs gate

**Files:**
- Modify: `website/docs/features/diagnostics.md` (the `### Root-cause attribution` section, line 391 area)
- Modify: `CHANGELOG.md` (`## [Unreleased]` → `### Added`)
- Modify: `CLAUDE.md` (the schema paragraph at line 253; a new Post-1.0 bullet; the closing remaining-work sentence)
- Modify: `website/docs/roadmap.md` (the `post-1.0` table row, line 572)

**Interfaces:** consumes the exact flag name, JSON keys, verdict vocabulary and render format from Tasks 1–7. No Go code in this task.

- [ ] **Step 1: `website/docs/features/diagnostics.md`.** In the `### Root-cause attribution` section (line 391), after the existing prose describing the three rules and their precedence, add a subsection:

````markdown
#### The hypothesis trace and `scan --why`

The attribution pass keeps every candidate it evaluates, not only the
winner. Each flagged workload with at least one candidate carries a
`rootCauseTrace` in the JSON document (scan schema 1.8, additive): one entry
per candidate with a `cause`, a `kind` (`node`, `pvc` or `registry`), a
closed-set `verdict` and one evidence sentence. The verdicts:

- `attributed` — this candidate is the workload's `rootCause`.
- `ruled_out` — its evidence did not match this workload (no pod on that
  node, PVC not mounted, only one workload failing against that registry).
- `outranked` — its evidence matched, but precedence chose a stronger
  cause. This is the verdict that tells you the pass *saw* the candidate
  and rejected it deliberately.

The stored `rootCauseConfidence` sits beside the trace — the same word the
text report renders as a tag.

In the text report the trace is opt-in: `scan --why` prints it under each
workload. Without the flag the output is unchanged.

```text
✗ shop/api  Deployment  0/2 Degraded
    ↳ likely caused by node worker-2 (NotReady)
      · considered node worker-2 (NotReady): attributed — pod api-a is scheduled on it
      · considered node worker-9 (NotReady): ruled out — no pod of this workload is scheduled on it
      · considered PVC data-0 (ProvisioningFailed): outranked — node worker-2 (NotReady) is the stronger cause
```

A healthy cluster records nothing: only flagged workloads are evaluated,
and only when candidates exist. Attribution itself is unchanged — same
rules, same precedence, same `rootCause` strings.
````

(Match the section's existing heading levels — if the surrounding subsections use `####`, keep `####`; adjust only if the file's structure differs.)

- [ ] **Step 2: `CHANGELOG.md`.** Under `## [Unreleased]`, add (create the `### Added` heading if absent):

```markdown
### Added

- **`scan --why` and the root-cause hypothesis trace.** The attribution pass
  now keeps every candidate cause it evaluates — node, PVC and registry —
  with a closed-set verdict (`attributed`, `ruled_out`, `outranked`) and one
  evidence sentence per candidate, instead of discarding everything it
  rejected. `scan --output json` always carries the trace on a workload row
  (`rootCauseTrace`, with the stored `rootCauseConfidence` beside it; scan
  schema 1.7 → 1.8, additive), and `scan --why` prints it in the text report
  under each `↳ likely caused by` line. Without `--why`, text output is
  byte-identical to before, and attribution itself is unchanged: same rules,
  same precedence, same `rootCause` strings.
```

- [ ] **Step 3: `CLAUDE.md`.** Three edits:

(a) The schema paragraph (line 253): change `` at schema version **1.7** (added `policy`, then `baseline`, then a pod row's `state`, then `unreachable` on `nodehealth.Report`, then `podsAnswered` on `dnshealth.Report`, then `suggestion` on a finding, then `kind` on a finding, all seven `omitempty`), `` to `` at schema version **1.8** (added `policy`, then `baseline`, then a pod row's `state`, then `unreachable` on `nodehealth.Report`, then `podsAnswered` on `dnshealth.Report`, then `suggestion` on a finding, then `kind` on a finding, then a workload row's `rootCauseTrace` and `rootCauseConfidence`, all nine `omitempty`), ``.

(b) Add a Post-1.0 bullet in the Roadmap section, after the known-issues/policy-packs/fleet bullets and before the closing remaining-work sentence — **with NO `(vX.Y.Z)` parenthetical; that is added exclusively by the later `release:` commit**:

```markdown
- **Post-1.0 — the hypothesis engine, slice 1 has shipped:** the root-cause
  attribution pass keeps every candidate it evaluates instead of discarding
  the losers. `inventory.Workload` gains `rootCauseTrace` — one entry per
  candidate with a closed-set verdict (`attributed`, `ruled_out`,
  `outranked`) and one evidence sentence — and `rootCauseConfidence`, the
  stored grade of the winning attribution (scan schema 1.7 → 1.8, additive,
  both `omitempty`). `scan --why` renders the trace in the text report;
  JSON always carries it. Attribution behavior is byte-identical: same
  rules, same precedence, same `RootCause` strings, and the trace never
  reaches `findings.Finding`, so `gate` and `fleet` are untouched. Slices 2
  (trace-primed `--investigate`) and 3 (the chaos correctness corpus)
  remain.
```

(c) The closing sentence "The remaining post-1.0 work is other baseline dimensions." becomes: "The remaining post-1.0 work is other baseline dimensions and the hypothesis engine's remaining slices."

- [ ] **Step 4: `website/docs/roadmap.md`.** In the `post-1.0` table row (line 572), after the curated-policy-packs clause (ends "…though no outside pack has yet come through it), the second half of this item's first form"), insert this new clause, verbatim:

```text
; **hypothesis engine slice 1 shipped** (`scan --why` — the attribution pass records every candidate root cause with a closed-set verdict and one evidence sentence, JSON always carries the trace on a workload row, scan schema 1.8, attribution behavior unchanged)
```

And extend the row's closing "— plus other baseline dimensions, and loading a pack into an installed binary without a kubeagent release, still ahead" to "— plus other baseline dimensions, loading a pack into an installed binary without a kubeagent release, and the hypothesis engine's remaining slices (a trace-primed `--investigate`, the chaos correctness corpus), still ahead".

- [ ] **Step 5: Verify the docs build**

Run:
```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv2/bin/mkdocs build --strict -f mkdocs.yml; cd /home/ubuntu/git/kubeagent
```
Expected: exit 0, "Documentation built", no WARNING lines about the edited pages. (The red "Material for MkDocs 2.0" banner is cosmetic.) **`cd` back to the repo root — the working directory persists.**

- [ ] **Step 6: Full verification**

Run: `go build ./... && go test -p 2 ./...` → every package PASS (docs edits cannot move code, but `plugin_manifest_test.go` and `internal/cli/plugin_flags_test.go` pin doc/manifest text against registered flags — this run proves the docs edits kept them green).
Run: `git diff --stat main -- go.mod go.sum website/docs/quickstart.md internal/report/testdata/` → empty for all.

- [ ] **Step 7: Commit**

```bash
git add website/docs/features/diagnostics.md CHANGELOG.md CLAUDE.md website/docs/roadmap.md
git commit -s -m "docs: document scan --why and the root-cause hypothesis trace

diagnostics.md gains the trace subsection with the verdict vocabulary
and a sample --why rendering; CHANGELOG records the feature under
Unreleased; CLAUDE.md moves the scan schema paragraph to 1.8 and adds
the post-1.0 hypothesis-engine bullet; roadmap.md records slice 1 and
names the remaining slices."
```

---

## Self-review notes (already applied)

- **Spec coverage:** trace fields + omitempty (Task 1), every-candidate recording for all three rules with the outranked trust-property (Tasks 2–4), confidence storage (Task 5), `--why` rendering including the unattributed case (Task 6), the flag (Task 7), docs + schema-version documentation (Tasks 1, 8). Spec non-goals honored: no `findings.Finding` change, no attribution behavior change, no scan.go change, no MCP/htmlreport change (`internal/mcp/view.go` copies explicit fields, so the trace never crosses the MCP boundary — deliberate, matching the spec's boundary note).
- **Stale-spec corrections carried:** schema 1.7→1.8 (not 1.3→1.4); rebased line numbers.
- **Type consistency:** `inventory.Verdict` named type everywhere; kinds `"node"`/`"pvc"`/`"registry"` lowercase everywhere; `record`/`podOn` signatures identical across Tasks 2–4; `printWorkload(wl, now, suggest, why, w)` in Task 6 matches Task 7's caller expectations (Task 7 touches only the Input fill, not the call at report.go:394, which Task 6 already updated).
- **Placeholder scan:** every test and edit is written out in full; no "similar to Task N", no TBDs.
