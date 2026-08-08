# Learned Restart-Rate Baseline — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship post-1.0 roadmap item 1, slice 1 — an operator-captured restart-rate baseline (`kubeagent baseline capture`) that `scan --baseline` and `gate --baseline` compare a live cluster against, reporting only workloads that restart much more than is normal *for this cluster*.

**Architecture:** A new stdlib-only pure package `internal/baseline` holds the maths and the document format; it imports nothing from kubeagent, so reaching `internal/remediate` or `internal/explain` is impossible by construction. `kubeagent baseline capture` reads the cluster once, reduces pods to flat samples, and prints a versioned JSON document to stdout — it writes no file. `scan --baseline` and `gate --baseline` load that document, compare, and render: scan as a text/JSON section marked `confidence: medium`, gate as `findings.Info` findings that never fail a gate unless the operator passes `--fail-on info`.

**Tech Stack:** Go 1.26, standard library only for `internal/baseline`; Cobra for the CLI; client-go's existing typed collectors for the reads; `internal/jsonschema` + `internal/schemadoc` for the published schema.

**Source spec:** [docs/superpowers/specs/2026-08-06-restart-baseline-design.md](../specs/2026-08-06-restart-baseline-design.md) (commit `0d4628d`). Branch `restart-baseline`, cut off `main` at `70fcd8c`.

---

## One correction to the spec, already decided — do not reopen it

Spec §8 says sample construction should "walk `inventory.Result.Workloads` … and the pod names under each". **That is wrong and must not be implemented.** Two things in `internal/inventory` remove exactly what a baseline needs:

- `inventory.Prioritize` **drops healthy-quiet workloads entirely** ([internal/inventory/inventory.go:436-470](../../../internal/inventory/inventory.go)). A baseline built on its output would contain only workloads that are already unhealthy — the opposite of "what is normal here".
- `inventory.Assemble` **truncates a Job's or CronJob's pod list at `jobPodCap = 3`**, recording the remainder in `PodsOmitted`. Those pods carry restarts that belong in the denominator.

**The resolution this plan implements:** `baseline capture` calls `collect.CollectInventory(ctx, client, namespace)` (one read, all seven lists, `List` calls only, already covered by the `scan` RBAC profile's core rules) and resolves each pod to its workload through a **new exported per-pod resolver** in `internal/inventory` — `Owner` + `PodOwners(in Inputs) map[string]Owner` — extracted from `Assemble`'s own pod loop, with `Assemble` refactored to call it so there is exactly one implementation of the rule. `scan` and `gate` reuse the same path via `scan.Result.Inputs`, which is the untruncated, unfiltered `inventory.Inputs`.

This adds a function and a type. It does **not** change `inventory.PodRow` or `inventory.Workload`, so the spec's stated constraint holds.

---

## Global Constraints

Copied verbatim from spec §13, plus the two rules the tasks below depend on.

- `go.mod` and `go.sum` **must not change**. `internal/baseline` is stdlib-only.
- `internal/report/testdata/golden-scan.txt` stays **byte-identical**. Do **not** regenerate the demo GIF or `website/docs/quickstart.md`.
- The Helm chart, `deploy/`, `internal/rbacprofile`'s `Feature` table and every generated RBAC manifest are untouched.
- Detectors stay pure functions; `internal/diagnose` is not modified.
- `internal/watch`, `internal/watchstate`, `internal/slo`, `internal/tui`, `internal/mcp`, `internal/dashboard` and `internal/htmlreport` are not modified.
- `internal/baseline` imports nothing from kubeagent. `internal/findings` and `internal/report` may import it; it may import neither of them, nor `internal/scan`, `internal/remediate`, `internal/explain` or `internal/investigate`.
- Every commit needs a `Signed-off-by` trailer matching its author (`git commit -s`) — `main` enforces DCO. Verify with `bash scripts/dco-check.sh main HEAD`. **No `Co-Authored-By` trailer and no AI attribution anywhere** — commits, code, comments, docs, changelog.
- No secrets, credentials, private IPs or internal hostnames anywhere — code, tests, fixtures, help text, schemas, docs. RFC 5737 addresses (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`), RFC 2606 domains (`example.com`, `example.org`, `example.net`). Node names, kubeconfig context names and kubeconfig paths are credentials.
- Never expose API keys to the shell.
- `go test` runs locally with `-p 2`, never `-short`; CI runs `go test -race ./...`.
- Work never lands on `main` directly. This slice is on branch `restart-baseline`.
- **TDD throughout:** write the failing test first, run it, watch it fail, then implement.
- **Go lives at `/usr/local/go/bin`** — `export PATH=$PATH:/usr/local/go/bin` before any `go` command.
- A comment, doc line or help string that promises something the code does not keep is a **defect**, not a style nit.

---

## File Structure

**Created:**

| File | Responsibility |
|---|---|
| `internal/baseline/baseline.go` | Types, `Capture`, `Compare`, `Load`, `Marshal`. Pure, stdlib-only. |
| `internal/baseline/baseline_test.go` | Unit tests: the maths, the filter, the two-threshold rule, ordering, round-trip. |
| `internal/baseline/imports_test.go` | `go/parser` guards: no kubeagent import, no non-stdlib import. |
| `internal/baseline/fuzz_test.go` | `FuzzBaselineLoad` — no byte sequence may panic `Load`, and what it accepts must be arithmetic-safe. |
| `internal/cli/baseline.go` | `newBaselineCommand()` / `baseline capture`; `podSamples`, `loadBaseline`, `baselineReport` — shared by scan and gate. |
| `internal/cli/baseline_test.go` | `podSamples` and `runBaselineCapture` tests against a fake clientset. |
| `website/docs/features/baseline.md` | Feature documentation. |
| `website/docs/schemas/baseline-v1.json` | Generated — never hand-edited. |

**Modified:**

| File | Change |
|---|---|
| `internal/inventory/inventory.go` | Add `Owner` + `PodOwners`; refactor `Assemble` to call it. |
| `internal/inventory/inventory_test.go` | Direct tests for `PodOwners`. |
| `internal/jsonschema/jsonschema.go:27-32` | Add `BaselineVersion = "1.0"`; later `ScanVersion` → `"1.2"`. |
| `internal/schemadoc/schemadoc.go:41-78` | Seventh `Documents` entry; the "six graphs" comment becomes seven. |
| `internal/schemadoc/schemadoc_test.go:44-46` | `!= 6` → `!= 7`; add the version-pin test. |
| `internal/cli/root.go:91,118-119` | Register `newBaselineCommand()`; name it in `usageError()`. |
| `internal/cli/scan.go` | Three flags, load + compare, `in.Baseline`. |
| `internal/cli/gate.go` | Three flags, load + compare, `opts.Baseline`. |
| `internal/cli/surface_test.go` | Scan table 34 → 37, gate table 10 → 13, new baseline table. |
| `internal/report/report.go` | `Input.Baseline`, `ScanReport.Baseline`, `printBaseline`. |
| `internal/findings/findings.go` | `FromBaseline`. |
| `internal/gate/gate.go` | `Options.Baseline`; `Decide` appends its findings. |
| `README.md`, `website/docs/**`, `CLAUDE.md`, `CHANGELOG.md` | Docs (Task 7). |

---

### Task 1: `internal/baseline` — the pure package

**Files:**
- Create: `internal/baseline/baseline.go`
- Create: `internal/baseline/baseline_test.go`
- Create: `internal/baseline/imports_test.go`
- Create: `internal/baseline/fuzz_test.go`

**Interfaces:**
- Consumes: nothing. This package is a leaf — it imports only `bytes`, `encoding/json`, `errors`, `fmt`, `math`, `sort`, `strconv`, `strings`, `time`.
- Produces, for every later task:
  - `type PodSample struct { Kind, Namespace, Name string; Restarts int; AgeSeconds float64 }`
  - `type Entry struct { Kind, Namespace, Name string; RestartsPerHour float64; Pods int; ObservedSeconds float64 }`
  - `type Document struct { SchemaVersion, CapturedAt string; MinPodAgeSeconds float64; Workloads []Entry }`
  - `type Deviation struct { Kind, Namespace, Name string; BaselineRate, CurrentRate float64; Pods int }`
  - `type Report struct { Deviations []Deviation; Compared, NotInBaseline, GoneFromCluster int }`
  - `type CompareOptions struct { Factor, Floor float64 }`
  - `const SchemaVersion = "1.0"`, `const DefaultFactor = 3.0`, `const DefaultFloor = 0.5`, `const DefaultMinPodAge = time.Hour`
  - `func Capture(pods []PodSample, minPodAge time.Duration, now time.Time) Document`
  - `func Compare(doc Document, pods []PodSample, opts CompareOptions) Report`
  - `func Load(b []byte) (Document, error)`
  - `func (d Document) Marshal() ([]byte, error)`

- [ ] **Step 1: Write the failing unit tests**

Create `internal/baseline/baseline_test.go`:

```go
package baseline

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestCaptureComputesPodHourNormalisedRate(t *testing.T) {
	// Two pods, 7200s each = 4 pod-hours; 6 restarts total = 1.5/hour.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 4, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
	}, time.Hour, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", doc.SchemaVersion, SchemaVersion)
	}
	if doc.CapturedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("CapturedAt = %q, want an RFC3339 UTC instant", doc.CapturedAt)
	}
	if doc.MinPodAgeSeconds != 3600 {
		t.Errorf("MinPodAgeSeconds = %v, want 3600", doc.MinPodAgeSeconds)
	}
	if len(doc.Workloads) != 1 {
		t.Fatalf("Workloads = %d entries, want 1", len(doc.Workloads))
	}
	e := doc.Workloads[0]
	if e.RestartsPerHour != 1.5 {
		t.Errorf("RestartsPerHour = %v, want 1.5", e.RestartsPerHour)
	}
	if e.Pods != 2 || e.ObservedSeconds != 14400 {
		t.Errorf("Pods/ObservedSeconds = %d/%v, want 2/14400", e.Pods, e.ObservedSeconds)
	}
}

func TestCaptureExcludesAYoungPodFromBothSides(t *testing.T) {
	// The young pod's 5 restarts and its 60 seconds must BOTH vanish. If only
	// the numerator were filtered the rate would drop; if only the denominator
	// were, it would spike. Either bug leaves 1.0 here.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 5, AgeSeconds: 60},
	}, time.Hour, time.Time{})

	if len(doc.Workloads) != 1 {
		t.Fatalf("Workloads = %d entries, want 1", len(doc.Workloads))
	}
	e := doc.Workloads[0]
	if e.RestartsPerHour != 1 || e.Pods != 1 || e.ObservedSeconds != 7200 {
		t.Errorf("got %.4f/hour over %d pods and %vs, want 1/hour over 1 pod and 7200s",
			e.RestartsPerHour, e.Pods, e.ObservedSeconds)
	}
}

func TestCaptureOmitsAWorkloadWithNoCountedPods(t *testing.T) {
	// Unknown is not zero: an entry at 0/hour would later read as "this
	// workload never restarts", which the sample cannot support.
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 60},
	}, time.Hour, time.Time{})
	if len(doc.Workloads) != 0 {
		t.Errorf("Workloads = %+v, want no entry for a workload with no counted pods", doc.Workloads)
	}
}

func TestCaptureSortsByKindNamespaceName(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "StatefulSet", Namespace: "prod", Name: "cache", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "web", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "kube-system", Name: "api", AgeSeconds: 7200},
	}, time.Hour, time.Time{})

	var got []string
	for _, e := range doc.Workloads {
		got = append(got, e.Kind+"/"+e.Namespace+"/"+e.Name)
	}
	want := []string{
		"Deployment/kube-system/api",
		"Deployment/prod/api",
		"Deployment/prod/web",
		"StatefulSet/prod/cache",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestCaptureRefusesAnUnusableAge(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: math.NaN()},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: math.Inf(1)},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 1, AgeSeconds: -5},
	}, 0, time.Time{})
	if len(doc.Workloads) != 0 {
		t.Errorf("Workloads = %+v, want no entry — every sample had an unusable age", doc.Workloads)
	}
}

// baseDoc is a one-workload document at the given rate, captured with a
// one-hour floor.
func baseDoc(rate float64) Document {
	return Document{
		SchemaVersion: SchemaVersion, MinPodAgeSeconds: 3600,
		Workloads: []Entry{{Kind: "Deployment", Namespace: "prod", Name: "api",
			RestartsPerHour: rate, Pods: 1, ObservedSeconds: 7200}},
	}
}

// atRate is one 7200-second pod carrying whatever restart count produces rate.
func atRate(rate float64) []PodSample {
	return []PodSample{{Kind: "Deployment", Namespace: "prod", Name: "api",
		Restarts: int(rate * 2), AgeSeconds: 7200}}
}

func TestCompareNeedsBothThresholds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		base, cur  float64
		wantFlag   bool
	}{
		// 4x and +3.0/hour: both hold.
		{"clearly worse", 1, 4, true},
		// 20x but only +0.19/hour: the floor suppresses it.
		{"big multiple, tiny absolute change", 0.01, 0.2, false},
		// +2.0/hour but only 2x: the factor suppresses it.
		{"big absolute change, small multiple", 2, 4, false},
		// Baseline zero: the multiplicative test is trivially true, so the
		// floor is the only thing deciding — and 1.0 clears it.
		{"zero baseline above the floor", 0, 1, true},
		// Baseline zero and current below the floor: not reported.
		{"zero baseline below the floor", 0, 0.4, false},
		// Nobody is paged for a thing improving.
		{"improvement", 4, 1, false},
		{"unchanged", 2, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := Compare(baseDoc(tc.base), atRate(tc.cur), CompareOptions{})
			if got := len(rep.Deviations) == 1; got != tc.wantFlag {
				t.Errorf("%.3f -> %.3f flagged = %v, want %v (deviations: %+v)",
					tc.base, tc.cur, got, tc.wantFlag, rep.Deviations)
			}
			if rep.Compared != 1 {
				t.Errorf("Compared = %d, want 1", rep.Compared)
			}
		})
	}
}

func TestCompareHonoursExplicitOptions(t *testing.T) {
	// 2x with a +1.0/hour rise: default Factor 3.0 refuses it, Factor 2.0 takes it.
	if rep := Compare(baseDoc(1), atRate(2), CompareOptions{}); len(rep.Deviations) != 0 {
		t.Errorf("default options flagged 1 -> 2, want no deviation")
	}
	rep := Compare(baseDoc(1), atRate(2), CompareOptions{Factor: 2, Floor: 0.5})
	if len(rep.Deviations) != 1 {
		t.Fatalf("Factor 2 did not flag 1 -> 2: %+v", rep.Deviations)
	}
	d := rep.Deviations[0]
	if d.BaselineRate != 1 || d.CurrentRate != 2 || d.Pods != 1 {
		t.Errorf("deviation = %+v, want baseline 1, current 2, 1 pod", d)
	}
}

func TestCompareCountsNewAndGoneWorkloadsWithoutFlaggingThem(t *testing.T) {
	doc := baseDoc(1)
	doc.Workloads = append(doc.Workloads, Entry{
		Kind: "Deployment", Namespace: "prod", Name: "gone", RestartsPerHour: 1, Pods: 1, ObservedSeconds: 7200})

	rep := Compare(doc, []PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "brand-new", Restarts: 40, AgeSeconds: 7200},
	}, CompareOptions{})

	if rep.Compared != 1 || rep.NotInBaseline != 1 || rep.GoneFromCluster != 1 {
		t.Errorf("compared/new/gone = %d/%d/%d, want 1/1/1", rep.Compared, rep.NotInBaseline, rep.GoneFromCluster)
	}
	if len(rep.Deviations) != 0 {
		t.Errorf("deviations = %+v, want none — a workload absent from the baseline is never flagged", rep.Deviations)
	}
}

func TestCompareAppliesTheCapturedFloorNotTheCallers(t *testing.T) {
	// The document says one hour. A 60-second pod carrying 40 restarts must be
	// excluded on the compare side too, or the asymmetry alone produces an alarm.
	rep := Compare(baseDoc(1), []PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 2, AgeSeconds: 7200},
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 40, AgeSeconds: 60},
	}, CompareOptions{})
	if len(rep.Deviations) != 0 {
		t.Errorf("deviations = %+v, want none — the young pod must not count", rep.Deviations)
	}
}

func TestCompareAlwaysReturnsANonNilDeviationSlice(t *testing.T) {
	rep := Compare(Document{SchemaVersion: SchemaVersion}, nil, CompareOptions{})
	if rep.Deviations == nil {
		t.Error("Deviations is nil; a run that found nothing must encode \"deviations\": []")
	}
}

func TestMarshalLoadRoundTrip(t *testing.T) {
	doc := Capture([]PodSample{
		{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 7200},
	}, time.Hour, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if b[len(b)-1] != '\n' {
		t.Error("Marshal output does not end in a newline")
	}
	got, err := Load(b)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CapturedAt != doc.CapturedAt || got.MinPodAgeSeconds != doc.MinPodAgeSeconds {
		t.Errorf("round trip changed the header: %+v vs %+v", got, doc)
	}
	if len(got.Workloads) != 1 || got.Workloads[0] != doc.Workloads[0] {
		t.Errorf("round trip changed the workloads: %+v vs %+v", got.Workloads, doc.Workloads)
	}
}

func TestLoadRejectsADifferentMajor(t *testing.T) {
	_, err := Load([]byte(`{"schemaVersion":"2.0","workloads":[]}`))
	if err == nil {
		t.Fatal("Load accepted a different MAJOR version")
	}
	if !strings.Contains(err.Error(), "2.0") {
		t.Errorf("error %q does not name the version it refused", err)
	}
}

func TestLoadAcceptsAHigherMinor(t *testing.T) {
	// additionalProperties is unset on purpose: a document written by a later
	// MINOR must still load here, unknown keys and all.
	doc, err := Load([]byte(`{"schemaVersion":"1.9","workloads":[],"somethingNew":true}`))
	if err != nil {
		t.Fatalf("Load rejected a higher MINOR: %v", err)
	}
	if doc.Workloads == nil {
		t.Error("Load left Workloads nil")
	}
}

func TestLoadRejectsAMissingOrMalformedVersion(t *testing.T) {
	for _, src := range []string{
		`{"workloads":[]}`,
		`{"schemaVersion":"","workloads":[]}`,
		`{"schemaVersion":"1","workloads":[]}`,
		`{"schemaVersion":"x.y","workloads":[]}`,
		`{"schemaVersion":"1.","workloads":[]}`,
		`not json`,
	} {
		if _, err := Load([]byte(src)); err == nil {
			t.Errorf("Load accepted %q", src)
		}
	}
}

func TestLoadRejectsAnUnusableEntry(t *testing.T) {
	for _, src := range []string{
		`{"schemaVersion":"1.0","workloads":[{"namespace":"prod","name":"api"}]}`,
		`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","namespace":"prod"}]}`,
		`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","name":"api","restartsPerHour":-1}]}`,
		`{"schemaVersion":"1.0","minPodAgeSeconds":-1,"workloads":[]}`,
	} {
		if _, err := Load([]byte(src)); err == nil {
			t.Errorf("Load accepted %q", src)
		}
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/baseline/
```

Expected: the package does not build — `undefined: Capture`, `undefined: Document`, and so on.

- [ ] **Step 3: Write the implementation**

Create `internal/baseline/baseline.go`:

```go
// Package baseline learns what a workload's restart rate normally is on one
// cluster, and compares a later observation against it.
//
// The package is pure. It imports nothing from kubeagent and nothing outside
// the standard library, holds no client and no context, issues no cluster call
// and makes no model call — the last two are separate promises. The caller
// reduces pods to PodSample values, so no Kubernetes type crosses the
// boundary, which is what makes reaching internal/remediate or
// internal/explain impossible by construction rather than by rule.
// internal/baseline/imports_test.go enforces both halves of that.
//
// What the rate honestly measures: restarts over the lifetimes of the pods
// present when the sample was taken. It is not long-term history. A workload
// whose pods were all recreated an hour before capture shows only what those
// pods have done since. internal/capacity states the equivalent limit for
// metrics-server samples; this one is stated here, in the feature docs, and in
// the `baseline capture` help text.
package baseline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is the version Capture writes into every Document. It is
// spelled here rather than imported from internal/jsonschema because this
// package imports nothing from kubeagent;
// internal/schemadoc's TestBaselineSchemaVersionMatches pins the two together,
// so the duplication cannot drift silently.
const SchemaVersion = "1.0"

// The defaults. Exported because the CLI declares them as flag defaults and
// there must be exactly one spelling of each number.
const (
	// DefaultFactor is the multiplicative threshold: a workload must reach this
	// multiple of its baseline rate before it is a deviation.
	DefaultFactor = 3.0
	// DefaultFloor is the absolute threshold in restarts per hour, which is what
	// stops 0.001 -> 0.01 reading as a 10x alarm.
	DefaultFloor = 0.5
	// DefaultMinPodAge is how old a pod must be to count toward a rate.
	DefaultMinPodAge = time.Hour
)

// maxVersionLen bounds the schemaVersion string an operator-supplied document
// may carry, so a malformed file cannot put an arbitrary blob into an error
// message.
const maxVersionLen = 32

// PodSample is one pod's contribution, already resolved to its workload.
type PodSample struct {
	Kind       string  // "Deployment" | "StatefulSet" | "DaemonSet" | "Job" | "CronJob" | "Pod"
	Namespace  string
	Name       string  // the WORKLOAD's name, not the pod's
	Restarts   int     // sum of ContainerStatus.RestartCount across the pod's containers
	AgeSeconds float64 // now - pod start
}

// Entry is one workload's learned normal.
type Entry struct {
	Kind            string  `json:"kind"`
	Namespace       string  `json:"namespace"`
	Name            string  `json:"name"`
	RestartsPerHour float64 `json:"restartsPerHour"`
	Pods            int     `json:"pods"`            // pods that counted
	ObservedSeconds float64 `json:"observedSeconds"` // total pod-seconds behind the rate
}

// Document is the artifact `kubeagent baseline capture` prints and
// `--baseline` reads. It is a published, versioned JSON document.
type Document struct {
	SchemaVersion    string  `json:"schemaVersion"`
	CapturedAt       string  `json:"capturedAt"` // RFC3339 UTC
	MinPodAgeSeconds float64 `json:"minPodAgeSeconds"`
	Workloads        []Entry `json:"workloads"`
}

// Deviation is one workload whose current rate is abnormal for this cluster.
// Tagged because Report is embedded in report.ScanReport and therefore lands in
// `scan --output json` and in the published scan schema.
type Deviation struct {
	Kind         string  `json:"kind"`
	Namespace    string  `json:"namespace"`
	Name         string  `json:"name"`
	BaselineRate float64 `json:"baselineRestartsPerHour"`
	CurrentRate  float64 `json:"currentRestartsPerHour"`
	Pods         int     `json:"pods"` // pods behind CurrentRate
}

// Report is what Compare returns.
type Report struct {
	// Deviations is always non-nil: a run that found nothing encodes
	// "deviations": [], which says the comparison happened, where an absent key
	// would not.
	Deviations      []Deviation `json:"deviations"`
	Compared        int         `json:"compared"`        // present in both the document and the cluster
	NotInBaseline   int         `json:"notInBaseline"`   // in the cluster, absent from the document
	GoneFromCluster int         `json:"goneFromCluster"` // in the document, absent from the cluster
}

// CompareOptions tunes the deviation rule. A zero field takes its default, the
// same convention watchstate.Options uses.
type CompareOptions struct {
	Factor float64 // default DefaultFactor
	Floor  float64 // default DefaultFloor
}

// Capture reduces a cluster's pods to one entry per workload. now is passed in
// rather than read, matching RestartLoopDetector's injected instant and
// watchstate's "the caller passes now".
func Capture(pods []PodSample, minPodAge time.Duration, now time.Time) Document {
	minSeconds := minPodAge.Seconds()
	if minSeconds < 0 || math.IsNaN(minSeconds) {
		minSeconds = 0
	}
	return Document{
		SchemaVersion:    SchemaVersion,
		CapturedAt:       now.UTC().Format(time.RFC3339),
		MinPodAgeSeconds: minSeconds,
		Workloads:        rates(pods, minSeconds),
	}
}

// Compare judges pods against doc.
//
// The minimum pod age comes from doc.MinPodAgeSeconds, never from the caller.
// A capture and a compare run with different floors would silently produce
// garbage, so the symmetry is read out of the document rather than left to a
// caller's discipline.
func Compare(doc Document, pods []PodSample, opts CompareOptions) Report {
	if opts.Factor <= 0 || math.IsNaN(opts.Factor) {
		opts.Factor = DefaultFactor
	}
	if opts.Floor <= 0 || math.IsNaN(opts.Floor) {
		opts.Floor = DefaultFloor
	}

	base := make(map[string]Entry, len(doc.Workloads))
	for _, e := range doc.Workloads {
		base[key(e.Kind, e.Namespace, e.Name)] = e
	}

	rep := Report{Deviations: []Deviation{}}
	seen := make(map[string]bool, len(doc.Workloads))
	for _, e := range rates(pods, doc.MinPodAgeSeconds) {
		k := key(e.Kind, e.Namespace, e.Name)
		b, ok := base[k]
		if !ok {
			rep.NotInBaseline++
			continue
		}
		seen[k] = true
		rep.Compared++
		if !deviates(b.RestartsPerHour, e.RestartsPerHour, opts) {
			continue
		}
		rep.Deviations = append(rep.Deviations, Deviation{
			Kind: e.Kind, Namespace: e.Namespace, Name: e.Name,
			BaselineRate: b.RestartsPerHour, CurrentRate: e.RestartsPerHour,
			Pods: e.Pods,
		})
	}
	for _, e := range doc.Workloads {
		if !seen[key(e.Kind, e.Namespace, e.Name)] {
			rep.GoneFromCluster++
		}
	}
	sortDeviations(rep.Deviations)
	return rep
}

// deviates applies the two-threshold rule. BOTH must hold: the multiplicative
// test catches "this got much worse relative to itself", and the absolute floor
// is what stops 0.001 -> 0.01 reading as a 10x alarm. The floor also carries
// the baseline-is-zero case, where the multiplicative test is trivially true.
// Only increases deviate — a workload restarting less than its baseline is not
// reported, because nobody is paged for a thing improving.
func deviates(baseRate, currentRate float64, opts CompareOptions) bool {
	return currentRate >= baseRate*opts.Factor && currentRate-baseRate >= opts.Floor
}

// rates is the shared maths: one entry per workload that has at least one
// counted pod, sorted. Capture and Compare both go through it, so the two sides
// can never compute a rate two different ways.
func rates(pods []PodSample, minSeconds float64) []Entry {
	type acc struct {
		id       PodSample
		restarts int
		seconds  float64
		pods     int
	}
	totals := map[string]*acc{}
	for _, p := range pods {
		if !counts(p, minSeconds) {
			continue
		}
		k := key(p.Kind, p.Namespace, p.Name)
		a, ok := totals[k]
		if !ok {
			a = &acc{id: p}
			totals[k] = a
		}
		a.restarts += p.Restarts
		a.seconds += p.AgeSeconds
		a.pods++
	}

	out := make([]Entry, 0, len(totals))
	for _, a := range totals {
		out = append(out, Entry{
			Kind: a.id.Kind, Namespace: a.id.Namespace, Name: a.id.Name,
			RestartsPerHour: ratePerHour(a.restarts, a.seconds),
			Pods:            a.pods,
			ObservedSeconds: a.seconds,
		})
	}
	sortEntries(out)
	return out
}

// counts reports whether a sample contributes to a rate. A pod younger than the
// floor is excluded from BOTH the numerator and the denominator: a
// 30-second-old pod with 2 restarts implies 240 restarts/hour and would swamp
// every older pod in its workload. A non-finite or non-positive age is refused
// for the same reason — it cannot contribute meaningful pod-seconds, and a NaN
// would poison the whole workload's sum.
func counts(p PodSample, minSeconds float64) bool {
	if math.IsNaN(p.AgeSeconds) || math.IsInf(p.AgeSeconds, 0) || p.AgeSeconds <= 0 {
		return false
	}
	if p.Restarts < 0 {
		return false
	}
	return p.AgeSeconds >= minSeconds
}

// ratePerHour normalises a workload's restarts across its pods' observed
// seconds. counts already refuses a non-positive age, so zero seconds cannot
// reach here today; the guard stays because a division that could produce +Inf
// must not depend on a caller's invariant holding.
func ratePerHour(restarts int, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(restarts) / (seconds / 3600)
}

func key(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

func sortEntries(e []Entry) {
	sort.Slice(e, func(i, j int) bool {
		if e[i].Kind != e[j].Kind {
			return e[i].Kind < e[j].Kind
		}
		if e[i].Namespace != e[j].Namespace {
			return e[i].Namespace < e[j].Namespace
		}
		return e[i].Name < e[j].Name
	})
}

func sortDeviations(d []Deviation) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Kind != d[j].Kind {
			return d[i].Kind < d[j].Kind
		}
		if d[i].Namespace != d[j].Namespace {
			return d[i].Namespace < d[j].Namespace
		}
		return d[i].Name < d[j].Name
	})
}

// Marshal renders the document the way `kubeagent baseline capture` prints it:
// two-space indented with a trailing newline, matching every other JSON
// document kubeagent writes.
func (d Document) Marshal() ([]byte, error) {
	if d.Workloads == nil {
		d.Workloads = []Entry{}
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Load parses a captured document.
//
// It accepts any document whose schemaVersion has the same MAJOR as this build
// writes and rejects a different MAJOR by name. That matches the published
// schemas' own contract: additionalProperties is unset on purpose, so a
// document from a later MINOR must still load here, unknown keys and all.
//
// What Load returns is arithmetic-safe: no NaN, no Inf, no negative rate. JSON
// has no NaN or Inf literal and encoding/json refuses an out-of-range
// magnitude, so those cannot arrive through Decode today — the checks stay
// because that guarantee is Load's contract and must not depend on a property
// of the decoder.
func Load(b []byte) (Document, error) {
	var d Document
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&d); err != nil {
		return Document{}, fmt.Errorf("parsing baseline: %w", err)
	}
	if d.SchemaVersion == "" {
		return Document{}, errors.New("baseline has no schemaVersion")
	}
	got, err := majorOf(d.SchemaVersion)
	if err != nil {
		return Document{}, err
	}
	want, err := majorOf(SchemaVersion)
	if err != nil {
		return Document{}, err
	}
	if got != want {
		return Document{}, fmt.Errorf(
			"baseline schemaVersion %s is major version %s; this build reads major version %s — recapture it with `kubeagent baseline capture`",
			d.SchemaVersion, got, want)
	}
	if !usableNumber(d.MinPodAgeSeconds) || d.MinPodAgeSeconds < 0 {
		return Document{}, errors.New("baseline minPodAgeSeconds is not a usable number")
	}
	for i, e := range d.Workloads {
		// The index, not the name: an error message is not the place to reprint
		// cluster-shaped data from an operator-supplied file.
		if e.Kind == "" || e.Name == "" {
			return Document{}, fmt.Errorf("baseline workload %d has no kind or no name", i)
		}
		if !usableNumber(e.RestartsPerHour) || e.RestartsPerHour < 0 {
			return Document{}, fmt.Errorf("baseline workload %d has an unusable restartsPerHour", i)
		}
		if !usableNumber(e.ObservedSeconds) || e.ObservedSeconds < 0 {
			return Document{}, fmt.Errorf("baseline workload %d has an unusable observedSeconds", i)
		}
	}
	if d.Workloads == nil {
		d.Workloads = []Entry{}
	}
	return d, nil
}

func usableNumber(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// majorOf returns a version string's MAJOR component. Every kubeagent
// schemaVersion is MAJOR.MINOR, both decimal.
//
// The version is quoted with %q in the error: it comes from an
// operator-supplied file, and %q Go-escapes every control byte, so a hostile
// value cannot reach a terminal raw.
func majorOf(v string) (string, error) {
	if len(v) > maxVersionLen {
		return "", fmt.Errorf("baseline schemaVersion is %d bytes, over the %d-byte cap", len(v), maxVersionLen)
	}
	i := strings.Index(v, ".")
	if i <= 0 || i == len(v)-1 {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	if _, err := strconv.Atoi(v[:i]); err != nil {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	if _, err := strconv.Atoi(v[i+1:]); err != nil {
		return "", fmt.Errorf("baseline schemaVersion %q is not MAJOR.MINOR", v)
	}
	return v[:i], nil
}
```

- [ ] **Step 4: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/baseline/ -v
```

Expected: PASS, every test.

- [ ] **Step 5: Write the import guards**

Create `internal/baseline/imports_test.go`:

```go
package baseline

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// modulePath is kubeagent's module path. Any import beginning with it is an
// import of kubeagent.
const modulePath = "github.com/imantaba/kubeagent"

// TestNoKubeagentImport is the structural half of this package's contract.
// internal/baseline holds the maths behind a signal that reaches a report and a
// gate; the design makes reaching internal/remediate or internal/explain
// impossible by construction rather than by a rule someone has to remember, by
// forbidding every kubeagent import. That is strictly stronger than the
// two-entry rule internal/fuzzgen's `constrained` map applies to the other
// surface packages, which is why this package is absent from that map — the
// weaker rule there would add nothing to the stronger one here. It is the same
// class as internal/dashboard and internal/jsonschema.
//
// Only non-test files are walked: a test may import a kubeagent package to
// build a fixture without weakening what the shipped package can reach.
func TestNoKubeagentImport(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			if strings.HasPrefix(p, modulePath) {
				t.Errorf("%s imports %s — internal/baseline must import nothing from kubeagent", path, p)
			}
		}
	}
}

// TestStdlibOnly is the second half: internal/baseline may import nothing
// outside the standard library either, so its behavior is a function of the Go
// release alone and go.mod can never move because of this package. The
// convention Go itself uses is that a module path's first segment contains a
// dot; a standard-library import path never does.
func TestStdlibOnly(t *testing.T) {
	for _, path := range packageFiles(t) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for _, p := range importsOf(t, path) {
			first, _, _ := strings.Cut(p, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %s — internal/baseline must import only the standard library", path, p)
			}
		}
	}
}

func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, imp := range f.Imports {
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// packageFiles lists this package's Go files. The test binary runs with the
// package directory as its working directory, so a glob is enough — no walk,
// and no dependency on where the repository is checked out.
func packageFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("globbing package files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found — the guard tests would pass vacuously")
	}
	return files
}
```

- [ ] **Step 6: Write the fuzz target**

Create `internal/baseline/fuzz_test.go`:

```go
package baseline

import "testing"

// FuzzBaselineLoad asserts that no byte sequence in an operator-supplied
// baseline file can panic the loader, and that anything Load accepts is safe to
// use: every number is finite, every entry is identifiable, and Compare can run
// against it without producing a non-finite rate. It joins the fuzz targets
// Theme H slice 3 added — the document is semi-trusted input in exactly the
// class already covered there.
func FuzzBaselineLoad(f *testing.F) {
	f.Add(`{"schemaVersion":"1.0","capturedAt":"2026-01-01T00:00:00Z","minPodAgeSeconds":3600,` +
		`"workloads":[{"kind":"Deployment","namespace":"prod","name":"api",` +
		`"restartsPerHour":0.5,"pods":3,"observedSeconds":10800}]}`)
	f.Add(`{"schemaVersion":"1.0","capturedAt":"2026-01-01T00:00:00Z","minPodAgeSeconds":3600,"workloads":[`)
	f.Add(`{"schemaVersion":"2.0","workloads":[]}`)
	f.Add(`{"schemaVersion":"1.0","minPodAgeSeconds":1e400,"workloads":[]}`)
	f.Add(`{"schemaVersion":"1.0","workloads":[{"kind":"Deployment","name":"api","restartsPerHour":1e400}]}`)
	f.Add(`{"schemaVersion":"1.0","workloads":null}`)
	f.Add(`{"schemaVersion":"\x00.0","workloads":[]}`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, src string) {
		doc, err := Load([]byte(src))
		if err != nil {
			return
		}
		if doc.Workloads == nil {
			t.Fatal("Load returned a nil Workloads slice")
		}
		for i, e := range doc.Workloads {
			if e.Kind == "" || e.Name == "" {
				t.Errorf("accepted entry %d has no kind or no name", i)
			}
			if !usableNumber(e.RestartsPerHour) || e.RestartsPerHour < 0 {
				t.Errorf("accepted entry %d has restartsPerHour %v", i, e.RestartsPerHour)
			}
		}

		// A document Load accepted must be comparable without panicking and
		// without producing a rate no renderer can print.
		rep := Compare(doc, []PodSample{
			{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 9, AgeSeconds: 7200},
		}, CompareOptions{})
		if rep.Deviations == nil {
			t.Error("Compare returned a nil Deviations slice")
		}
		for _, d := range rep.Deviations {
			if !usableNumber(d.BaselineRate) || !usableNumber(d.CurrentRate) {
				t.Errorf("deviation carries a non-finite rate: %+v", d)
			}
		}

		// Round trip: what Load accepts, Marshal must re-emit and Load must
		// accept again. A document that survives one hop but not two would make
		// re-capturing a file lossy.
		b, merr := doc.Marshal()
		if merr != nil {
			t.Fatalf("Marshal rejected a document Load accepted: %v", merr)
		}
		again, aerr := Load(b)
		if aerr != nil {
			t.Fatalf("a document Load accepted did not survive Marshal+Load: %v", aerr)
		}
		if len(again.Workloads) != len(doc.Workloads) {
			t.Errorf("round trip changed the workload count: %d then %d", len(doc.Workloads), len(again.Workloads))
		}
	})
}
```

The file's only import is `testing`: `usableNumber` lives in `baseline.go` and this file is in the same package.

- [ ] **Step 7: Run everything**

```bash
export PATH=$PATH:/usr/local/go/bin
go vet ./internal/baseline/
go test ./internal/baseline/ -v
go test ./internal/baseline/ -run FuzzBaselineLoad -fuzz FuzzBaselineLoad -fuzztime 30s
```

Expected: vet clean, all tests PASS, the fuzz run completes with no new crashers. If the fuzzer writes anything under `internal/baseline/testdata/fuzz/`, that is a real finding — fix the code and commit the crasher as a regression seed.

- [ ] **Step 8: Commit**

```bash
git add internal/baseline/
git commit -s -m "feat(baseline): add the pure restart-rate baseline package

internal/baseline holds the maths and the document format for a learned
restart-rate baseline: Capture reduces pod samples to one pod-hour-normalised
rate per workload, Compare judges a later observation under a two-threshold
rule, and Load/Marshal read and write the versioned JSON document.

The package imports nothing from kubeagent and nothing outside the standard
library, which makes reaching internal/remediate or internal/explain impossible
by construction rather than by rule. imports_test.go enforces both halves, and
FuzzBaselineLoad asserts that no byte sequence in an operator-supplied file can
panic the loader or produce a non-finite rate."
```

---

### Task 2: `inventory.PodOwners` — one implementation of the pod-to-workload rule

**Files:**
- Modify: `internal/inventory/inventory.go:240-380` (add `Owner`/`PodOwners`; refactor `Assemble`)
- Test: `internal/inventory/inventory_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces, for Task 4:
  - `type Owner struct { Kind, Namespace, Name string }`
  - `func PodOwners(in Inputs) map[string]Owner` — keyed by the pod's `"namespace/name"`.

**Why:** see "One correction to the spec" above. `baseline capture` needs every workload and every pod; `Assemble`'s output has neither. Extracting the rule rather than copying it is what keeps the two callers from drifting.

- [ ] **Step 1: Write the failing tests**

Append to `internal/inventory/inventory_test.go`. The file already has `pod(ns, name string, owners []metav1.OwnerReference, restarts int32, image string) corev1.Pod` and `ctrlRef(kind, name string) []metav1.OwnerReference` — use them.

```go
func TestPodOwnersResolvesEveryOwnershipShape(t *testing.T) {
	in := Inputs{
		Pods: []corev1.Pod{
			pod("prod", "api-abc-1", ctrlRef("ReplicaSet", "api-abc"), 0, "app:1.0"),
			pod("prod", "orphan-rs-1", ctrlRef("ReplicaSet", "unknown-rs"), 0, "app:1.0"),
			pod("prod", "cache-0", ctrlRef("StatefulSet", "cache"), 0, "app:1.0"),
			pod("prod", "nightly-1-x", ctrlRef("Job", "nightly-1"), 0, "app:1.0"),
			pod("prod", "oneoff-x", ctrlRef("Job", "oneoff"), 0, "app:1.0"),
			pod("prod", "detached-x", ctrlRef("Job", "detached"), 0, "app:1.0"),
			pod("prod", "bare", nil, 0, "app:1.0"),
		},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-abc",
				OwnerReferences: ctrlRef("Deployment", "api")},
		}},
		Jobs: []batchv1.Job{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "nightly-1",
				OwnerReferences: ctrlRef("CronJob", "nightly")}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "oneoff"}},
			// detached names a CronJob that is not in in.CronJobs, so its pod
			// must roll up to the Job, not to a CronJob nothing lists.
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "detached",
				OwnerReferences: ctrlRef("CronJob", "vanished")}},
		},
		CronJobs: []batchv1.CronJob{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "nightly"},
		}},
	}

	got := PodOwners(in)
	want := map[string]Owner{
		"prod/api-abc-1":   {Kind: "Deployment", Namespace: "prod", Name: "api"},
		"prod/orphan-rs-1": {Kind: "ReplicaSet", Namespace: "prod", Name: "unknown-rs"},
		"prod/cache-0":     {Kind: "StatefulSet", Namespace: "prod", Name: "cache"},
		"prod/nightly-1-x": {Kind: "CronJob", Namespace: "prod", Name: "nightly"},
		"prod/oneoff-x":    {Kind: "Job", Namespace: "prod", Name: "oneoff"},
		"prod/detached-x":  {Kind: "Job", Namespace: "prod", Name: "detached"},
		"prod/bare":        {Kind: "Pod", Namespace: "prod", Name: "bare"},
	}
	if len(got) != len(want) {
		t.Fatalf("PodOwners returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("PodOwners[%q] = %+v, want %+v", k, got[k], w)
		}
	}
}

func TestPodOwnersKeepsEveryPodOfAJob(t *testing.T) {
	// Assemble truncates a Job's pod list at jobPodCap so a report stays
	// readable. PodOwners must not: a baseline needs every pod's restarts.
	in := Inputs{Jobs: []batchv1.Job{{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "batch"}}}}
	for i := 0; i < jobPodCap+4; i++ {
		in.Pods = append(in.Pods, pod("prod", "batch-"+strconv.Itoa(i), ctrlRef("Job", "batch"), 0, "app:1.0"))
	}
	if got := len(PodOwners(in)); got != jobPodCap+4 {
		t.Errorf("PodOwners returned %d entries, want %d — every pod must be resolved", got, jobPodCap+4)
	}
}
```

Add `strconv` to the test file's imports if it is not already there, and `appsv1`/`batchv1`/`metav1`/`corev1` as needed (the file already builds `Inputs` in the `Assemble` tests — reuse whatever aliases it uses).

- [ ] **Step 2: Run the tests and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory/ -run TestPodOwners
```

Expected: build failure — `undefined: PodOwners`, `undefined: Owner`.

- [ ] **Step 3: Add `Owner` and `PodOwners`**

In `internal/inventory/inventory.go`, immediately before `func Assemble`:

```go
// Owner is the workload a pod rolls up to.
type Owner struct {
	Kind      string
	Namespace string
	Name      string
}

// PodOwners resolves every pod in in.Pods to the workload it belongs to, keyed
// by the pod's "namespace/name". It is kubeagent's single implementation of the
// pod-to-workload rule: Assemble calls it, and so does `kubeagent baseline
// capture`.
//
// baseline capture cannot read that rule off Assemble's output. Prioritize
// drops healthy-quiet workloads from the report and Assemble truncates a Job's
// or CronJob's pod list at jobPodCap — both right for a report an operator
// reads, both wrong for a baseline, which needs precisely the healthy majority
// and every pod behind it.
//
// The rule: a ReplicaSet-owned pod rolls up to that ReplicaSet's Deployment
// when the ReplicaSet is in in.ReplicaSets and names one, and to the ReplicaSet
// otherwise; a Job-owned pod rolls up to that Job's CronJob when the Job names
// one AND that CronJob is in in.CronJobs, and to the Job otherwise; any other
// controller owner is taken at face value; a pod with no owner is its own
// "Pod" workload.
func PodOwners(in Inputs) map[string]Owner {
	// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).
	rsToDeploy := map[string]string{}
	for _, rs := range in.ReplicaSets {
		if o := controllerOwner(rs.OwnerReferences); o != nil && o.Kind == "Deployment" {
			rsToDeploy[rs.Namespace+"/"+rs.Name] = o.Name
		}
	}
	// jobToCronJob resolves Job -> owning CronJob name (namespaced).
	jobToCronJob := map[string]string{}
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			jobToCronJob[j.Namespace+"/"+j.Name] = o.Name
		}
	}
	// cronJobs is the set Assemble seeds as CronJob workloads. Gating the
	// Job -> CronJob promotion on it is exactly Assemble's old
	// controllerKeys[key("CronJob", ns, cj)] check: the only CronJob entries
	// controllerKeys ever held came from in.CronJobs.
	cronJobs := map[string]bool{}
	for _, cj := range in.CronJobs {
		cronJobs[cj.Namespace+"/"+cj.Name] = true
	}

	out := make(map[string]Owner, len(in.Pods))
	for _, p := range in.Pods {
		kind, name := "Pod", p.Name
		if o := controllerOwner(p.OwnerReferences); o != nil {
			switch o.Kind {
			case "ReplicaSet":
				if dep, ok := rsToDeploy[p.Namespace+"/"+o.Name]; ok {
					kind, name = "Deployment", dep
				} else {
					kind, name = "ReplicaSet", o.Name
				}
			case "Job":
				if cj, ok := jobToCronJob[p.Namespace+"/"+o.Name]; ok && cronJobs[p.Namespace+"/"+cj] {
					kind, name = "CronJob", cj
				} else {
					kind, name = "Job", o.Name
				}
			default:
				kind, name = o.Kind, o.Name
			}
		}
		out[p.Namespace+"/"+p.Name] = Owner{Kind: kind, Namespace: p.Namespace, Name: name}
	}
	return out
}
```

- [ ] **Step 4: Run the new tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory/ -run TestPodOwners -v
```

Expected: PASS.

- [ ] **Step 5: Refactor `Assemble` to call it**

Three edits inside `Assemble`, so the rule has exactly one implementation.

**(a)** In the `for _, j := range in.Jobs` loop, drop the map write but keep the skip and the seeding:

```go
	// CronJob-owned Jobs are NOT seeded as their own workloads (their pods roll
	// up to the CronJob, per PodOwners).
	for _, j := range in.Jobs {
		if o := controllerOwner(j.OwnerReferences); o != nil && o.Kind == "CronJob" {
			continue
		}
		seedJobLike("Job", j.Namespace, j.Name, jobStatus(j), "")
	}
```

**(b)** Delete the `rsToDeploy` block entirely (the `// rsToDeploy resolves ReplicaSet -> Deployment name (namespaced).` comment and its loop) — it now lives in `PodOwners`.

**(c)** Replace the owner-resolution switch at the top of the pod loop with a lookup:

```go
	owners := PodOwners(in)
	podKey := map[string]string{}    // "ns/name" -> workload key
	derivedReady := map[string]int{} // ready-pod count for pod-derived workloads
	for _, p := range in.Pods {
		o := owners[p.Namespace+"/"+p.Name]
		k := key(o.Kind, p.Namespace, o.Name)
		w, ok := workloads[k]
		if !ok {
			w = &Workload{Namespace: p.Namespace, Name: o.Name, Kind: o.Kind}
			workloads[k] = w
		}
		// …the rest of the loop body is unchanged…
```

Everything from `restarts, last := podRestarts(p)` onward stays exactly as it is.

- [ ] **Step 6: Run the whole inventory suite — the existing tests are the no-behavior-change proof**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/inventory/ -v
```

Expected: PASS, every pre-existing `TestAssemble_*` included. Those tests were written against the inline switch; passing them unchanged is what proves the extraction preserved the rule.

- [ ] **Step 7: Run the packages that depend on the rollup**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./internal/inventory/ ./internal/report/ ./internal/scan/ ./internal/findings/
```

Expected: PASS, including `TestGoldenScanOutput` — `golden-scan.txt` must stay byte-identical.

- [ ] **Step 8: Commit**

```bash
git add internal/inventory/inventory.go internal/inventory/inventory_test.go
git commit -s -m "refactor(inventory): extract PodOwners from Assemble

Assemble resolved each pod to its workload with an inline switch. A baseline
capture needs the same rule but cannot read it off Assemble's output: Prioritize
drops healthy-quiet workloads and Assemble truncates a Job's pod list at
jobPodCap, and a baseline needs exactly the workloads and pods those two remove.

Extracting the rule into an exported PodOwners, with Assemble refactored to call
it, keeps one implementation instead of two that can drift. The pre-existing
Assemble tests pass unchanged, which is the no-behavior-change proof."
```

---

### Task 3: The seventh versioned JSON document

**Files:**
- Modify: `internal/jsonschema/jsonschema.go:27-32`
- Modify: `internal/schemadoc/schemadoc.go:41-78` (the `Documents` slice and the `enums` comment above it)
- Modify: `internal/schemadoc/schemadoc_test.go:44-46`
- Create (generated): `website/docs/schemas/baseline-v1.json`

**Interfaces:**
- Consumes: `baseline.Document` from Task 1.
- Produces: `jsonschema.BaselineVersion` (`"1.0"`), and `kubeagent schema baseline` as a working command (it reads `schemadoc.Documents`, so no CLI change is needed).

- [ ] **Step 1: Write the failing tests**

In `internal/schemadoc/schemadoc_test.go`, change the count assertion:

```go
	if len(Documents) != 7 {
		t.Errorf("Documents has %d entries, want the seven documented surfaces", len(Documents))
	}
```

And add a new test to the same file:

```go
// TestBaselineSchemaVersionMatches pins internal/baseline's own SchemaVersion
// constant to the one internal/jsonschema publishes. internal/baseline imports
// nothing from kubeagent, so it cannot reference jsonschema.BaselineVersion
// directly; this is where the two spellings are held together. Every other
// surface sets its schemaVersion from jsonschema and needs no such test.
func TestBaselineSchemaVersionMatches(t *testing.T) {
	if baseline.SchemaVersion != jsonschema.BaselineVersion {
		t.Errorf("baseline.SchemaVersion = %q, jsonschema.BaselineVersion = %q — they must agree",
			baseline.SchemaVersion, jsonschema.BaselineVersion)
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/baseline"` to the test file's imports.

- [ ] **Step 2: Run and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc/
```

Expected: `undefined: jsonschema.BaselineVersion`, and `Documents has 6 entries, want the seven documented surfaces`.

- [ ] **Step 3: Add the version constant**

In `internal/jsonschema/jsonschema.go`, inside the existing const block:

```go
const (
	ScanVersion     = "1.1"
	GateVersion     = "1.1"
	RBACVersion     = "1.0"
	WatchVersion    = "1.0"
	BaselineVersion = "1.0"
)
```

(Keep whatever doc comment the block already carries; only the new line is added. `ScanVersion` moves to `"1.2"` in Task 5, not here.)

- [ ] **Step 4: Add the seventh document**

In `internal/schemadoc/schemadoc.go`, append to `Documents` (after the `watch-explanations` entry) and import `internal/baseline`:

```go
	{
		Name: "baseline", Surface: "baseline", Version: jsonschema.BaselineVersion,
		Root:        reflect.TypeOf(baseline.Document{}),
		Title:       "kubeagent restart-rate baseline",
		Description: "The document written by `kubeagent baseline capture`: one learned restart rate per workload, the minimum pod age behind it, and when it was captured. `scan --baseline` and `gate --baseline` read it back.",
	},
```

Update the comment above `enums`, which currently reads `// enums is every named type in the six graphs whose values are a closed set.`:

```go
// enums is every named type in the seven graphs whose values are a closed set.
```

`baseline.Document` contributes no named type with a closed value set, so `enums`, `overrides` and `freeFormStrings` gain no entries.

- [ ] **Step 5: Generate the schema**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc -run TestSchemaDrift -update
go test ./internal/schemadoc/ -v
```

Expected: `-update` logs `wrote website/docs/schemas/baseline-v1.json` and writes nothing else (no other document's shape moved). The second run is clean.

- [ ] **Step 6: Confirm the command works with no cluster**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o /tmp/kubeagent-baseline-check . && KUBECONFIG=/nonexistent /tmp/kubeagent-baseline-check schema baseline | head -20
rm -f /tmp/kubeagent-baseline-check
```

Expected: the schema prints. `kubeagent schema` reads `schemadoc.Documents` and contacts nothing.

- [ ] **Step 7: Full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/jsonschema/jsonschema.go internal/schemadoc/schemadoc.go internal/schemadoc/schemadoc_test.go website/docs/schemas/baseline-v1.json
git commit -s -m "feat(schema): publish the baseline document at version 1.0

A file an operator captures today, commits, and feeds to a different kubeagent
build in six months is exactly what the other six schema versions exist to
protect, so the baseline document joins them as the seventh.

TestBaselineSchemaVersionMatches pins internal/baseline's own SchemaVersion
constant to jsonschema.BaselineVersion: the package imports nothing from
kubeagent and so cannot reference the constant directly, and this is where the
two spellings are held together."
```

---

### Task 4: `kubeagent baseline capture`

**Files:**
- Create: `internal/cli/baseline.go`
- Create: `internal/cli/baseline_test.go`
- Modify: `internal/cli/root.go:91` (`usageError`) and `:118-119` (`AddCommand`)
- Modify: `internal/cli/surface_test.go`

**Interfaces:**
- Consumes: `baseline.{Capture, PodSample, Document, DefaultMinPodAge, Marshal}` (Task 1); `inventory.{Inputs, Owner, PodOwners}` (Task 2).
- Produces, for Tasks 5 and 6:
  - `func podSamples(in inventory.Inputs, now time.Time) []baseline.PodSample`
  - `func loadBaseline(path string) (*baseline.Document, error)`
  - `func baselineReport(doc *baseline.Document, factor, floor float64, in inventory.Inputs, now time.Time) *baseline.Report`

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/baseline_test.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// startedPod is a pod that started `age` before `now` with `restarts` restarts
// across one container.
func startedPod(ns, name string, owners []metav1.OwnerReference, restarts int32, started metav1.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Status: corev1.PodStatus{
			StartTime:         &started,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: restarts}},
		},
	}
}

func TestPodSamplesResolvesWorkloadAndAge(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	twoHoursAgo := metav1.NewTime(now.Add(-2 * time.Hour))

	in := inventory.Inputs{
		Pods: []corev1.Pod{
			startedPod("prod", "api-abc-1", ctrlRefCLI("ReplicaSet", "api-abc"), 3, twoHoursAgo),
			// No StartTime: it has never run, so it has observed no container
			// runtime and must not contribute pod-seconds.
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "pending"}},
		},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-abc",
				OwnerReferences: ctrlRefCLI("Deployment", "api")},
		}},
	}

	got := podSamples(in, now)
	if len(got) != 1 {
		t.Fatalf("podSamples returned %d samples, want 1: %+v", len(got), got)
	}
	want := baseline.PodSample{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 7200}
	if got[0] != want {
		t.Errorf("sample = %+v, want %+v", got[0], want)
	}
}

func TestPodSamplesSumsRestartsAcrossContainers(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	p := startedPod("prod", "multi", nil, 0, metav1.NewTime(now.Add(-time.Hour)))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", RestartCount: 2},
		{Name: "sidecar", RestartCount: 5},
	}
	got := podSamples(inventory.Inputs{Pods: []corev1.Pod{p}}, now)
	if len(got) != 1 || got[0].Restarts != 7 {
		t.Errorf("samples = %+v, want one sample with 7 restarts", got)
	}
}

func TestRunBaselineCaptureRendersADocument(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	in := inventory.Inputs{Pods: []corev1.Pod{
		startedPod("prod", "cache-0", ctrlRefCLI("StatefulSet", "cache"), 4, metav1.NewTime(now.Add(-2*time.Hour))),
	}}

	var buf bytes.Buffer
	if err := renderBaselineCapture(in, time.Hour, now, &buf); err != nil {
		t.Fatalf("renderBaselineCapture: %v", err)
	}
	var doc baseline.Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not the baseline document: %v\n%s", err, buf.String())
	}
	if doc.SchemaVersion != baseline.SchemaVersion || doc.CapturedAt != "2026-01-02T12:00:00Z" {
		t.Errorf("header = %+v, want schemaVersion %q at 2026-01-02T12:00:00Z", doc, baseline.SchemaVersion)
	}
	if len(doc.Workloads) != 1 || doc.Workloads[0].Kind != "StatefulSet" || doc.Workloads[0].RestartsPerHour != 2 {
		t.Errorf("workloads = %+v, want one StatefulSet at 2 restarts/hour", doc.Workloads)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("output does not end in a newline")
	}
}

func TestBaselineReportRoundTripsThroughLoad(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	in := inventory.Inputs{Pods: []corev1.Pod{
		startedPod("prod", "api-0", ctrlRefCLI("StatefulSet", "api"), 2, metav1.NewTime(now.Add(-2*time.Hour))),
	}}
	var buf bytes.Buffer
	if err := renderBaselineCapture(in, time.Hour, now, &buf); err != nil {
		t.Fatalf("renderBaselineCapture: %v", err)
	}
	doc, err := baseline.Load(buf.Bytes())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The same cluster right after capture must show no deviation at all.
	rep := baselineReport(&doc, 0, 0, in, now)
	if rep == nil {
		t.Fatal("baselineReport returned nil for a loaded document")
	}
	if len(rep.Deviations) != 0 || rep.Compared != 1 {
		t.Errorf("report = %+v, want 1 compared and no deviations", rep)
	}
}

func TestLoadBaselineIsNilWithoutTheFlag(t *testing.T) {
	doc, err := loadBaseline("")
	if doc != nil || err != nil {
		t.Errorf("loadBaseline(\"\") = %v, %v; want nil, nil", doc, err)
	}
}

func TestLoadBaselineReportsAnUnreadableFile(t *testing.T) {
	if _, err := loadBaseline("/nonexistent/baseline.json"); err == nil {
		t.Error("loadBaseline accepted a path that does not exist")
	}
}

// ctrlRefCLI mirrors internal/inventory's test helper; internal/cli cannot
// reach that package's test scope.
func ctrlRefCLI(kind, name string) []metav1.OwnerReference {
	ctrl := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &ctrl}}
}
```

Add to `internal/cli/surface_test.go`:

```go
// TestCommandSurfaceBaselineCapture asserts every flag on `baseline capture`
// reaches the field it configures, the same guard the scan and gate tables give
// those commands.
func TestCommandSurfaceBaselineCapture(t *testing.T) {
	cases := []struct {
		flag  string
		args  []string
		check func(baselineOptions) bool
	}{
		{"kubeconfig", []string{"--kubeconfig", "/nonexistent/kubeconfig"}, func(o baselineOptions) bool { return o.kubeconfig == "/nonexistent/kubeconfig" }},
		{"context", []string{"--context", "example-context"}, func(o baselineOptions) bool { return o.contextName == "example-context" }},
		{"namespace", []string{"--namespace", "example-ns"}, func(o baselineOptions) bool { return o.namespace == "example-ns" }},
		{"min-pod-age", []string{"--min-pod-age", "15m"}, func(o baselineOptions) bool { return o.minPodAge == 15*time.Minute }},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			o, err := parseBaselineCaptureFlags(tc.args)
			if err != nil {
				t.Fatalf("parseBaselineCaptureFlags(%v): %v", tc.args, err)
			}
			if !tc.check(o) {
				t.Errorf("--%s did not reach its field; got %+v", tc.flag, o)
			}
		})
	}
	if len(cases) != 4 {
		t.Errorf("baseline capture surface table has %d cases, want 4 — one per declared flag", len(cases))
	}
}

// TestCommandSurfaceBaselineCaptureDefaults asserts the one non-zero default.
func TestCommandSurfaceBaselineCaptureDefaults(t *testing.T) {
	o, err := parseBaselineCaptureFlags(nil)
	if err != nil {
		t.Fatalf("parseBaselineCaptureFlags(nil): %v", err)
	}
	if o.minPodAge != baseline.DefaultMinPodAge {
		t.Errorf("--min-pod-age default = %s, want %s", o.minPodAge, baseline.DefaultMinPodAge)
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/baseline"` to `surface_test.go`'s imports.

- [ ] **Step 2: Run and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -run 'TestPodSamples|TestRunBaselineCapture|TestBaselineReport|TestLoadBaseline|TestCommandSurfaceBaseline'
```

Expected: build failure — `undefined: podSamples`, `undefined: baselineOptions`, and so on.

- [ ] **Step 3: Write `internal/cli/baseline.go`**

```go
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

// loadBaseline reads and parses --baseline. Nil document and nil error when the
// flag was absent.
//
// Kept separate from the comparison so a bad file is refused before any cluster
// read happens: an unreadable or wrong-version baseline is bad input, in the
// same class as a bad flag, and nothing about the cluster should have been
// attempted. The path appears in the error, which reaches stderr only — the
// same carve-out --policy already has, and it never enters a report.
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

// baselineReport compares a scan's pods against a loaded document. Nil when no
// document was loaded, so a run without --baseline renders and encodes exactly
// as it did before the flag existed. A zero factor or floor takes the package
// default.
func baselineReport(doc *baseline.Document, factor, floor float64, in inventory.Inputs, now time.Time) *baseline.Report {
	if doc == nil {
		return nil
	}
	rep := baseline.Compare(*doc, podSamples(in, now), baseline.CompareOptions{Factor: factor, Floor: floor})
	return &rep
}
```

- [ ] **Step 4: Register the command**

In `internal/cli/root.go`, add `newBaselineCommand()` to the `AddCommand` call:

```go
	root.AddCommand(newVersionCommand(), newSchemaCommand(), newMCPCommand(), newTUICommand(), newScanCommand(),
		newWatchCommand(), newGateCommand(), newRBACCommand(), newPolicyCommand(), newBaselineCommand(),
		newCompletionCommand())
```

And in `usageError()`, insert this fragment between the `policy validate` clause and the `schema` clause (the string is one long `fmt.Errorf` argument — add the text inline, keeping `%[1]s`):

```text
 | %[1]s baseline capture [--kubeconfig path] [--context name] [-n namespace] [--min-pod-age dur]
```

so the sequence reads `… | %[1]s policy validate <file>… | %[1]s baseline capture [--kubeconfig path] [--context name] [-n namespace] [--min-pod-age dur] | %[1]s schema [name] | …`.

- [ ] **Step 5: Run the tests and watch them pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -v
```

Expected: PASS, including `TestUsageErrorNamesEveryCommand` (which fails until the usage string names `baseline`) and `TestNormalizeNeverSwallowsAFlagAfterABooleanFlag` (which walks the whole tree, so it now walks `baseline capture` too).

- [ ] **Step 6: Try it against a real disposable cluster**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
kind create cluster --name kubeagent-baseline --wait 90s
go build -o kubeagent .
./kubeagent baseline capture --context kind-kubeagent-baseline --min-pod-age 0 | head -30
```

Expected: a JSON document with `schemaVersion: "1.0"` and entries for the kube-system workloads. `--min-pod-age 0` is what makes a freshly created cluster produce entries at all.

Leave the cluster up — Tasks 5 and 6 use it. Do **not** run `./chaos/run.sh`.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/baseline.go internal/cli/baseline_test.go internal/cli/root.go internal/cli/surface_test.go kubeagent
git reset kubeagent   # the built binary is not committed
git commit -s -m "feat(cli): add kubeagent baseline capture

One read (collect.CollectInventory, List calls only), each pod resolved to its
workload through inventory.PodOwners, and the resulting document printed to
stdout. Nothing is written to disk, which keeps --audit-log the only file write
in the repository and matches rbac print.

Read-only toward the cluster and makes no model call: two separate promises,
stated separately in the help text. The samples come from the raw pod objects
rather than Assemble's output, because Prioritize drops the healthy-quiet
workloads a baseline is mostly made of."
```

---

### Task 5: `scan --baseline`

**Files:**
- Modify: `internal/cli/scan.go:35-70` (`scanOptions`), `:81-117` (`bindScanFlags`), and the body of `runScan`
- Modify: `internal/report/report.go:46-77` (`ScanReport`), `:136-182` (`Input`), `:326-332` (section order), and a new `printBaseline` beside `printPolicy` at `:1514-1577`
- Modify: `internal/jsonschema/jsonschema.go` (`ScanVersion` → `"1.2"`)
- Modify: `internal/cli/surface_test.go` (scan table 34 → 37, defaults)
- Modify: `internal/cli/normalize_test.go` (a `-baseline` row)
- Regenerate: `website/docs/schemas/scan-v1.json`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Consumes: `loadBaseline`, `baselineReport` (Task 4); `baseline.{Report, Deviation, DefaultFactor, DefaultFloor}` (Task 1).
- Produces, for Task 6: nothing new; Task 6 reuses `loadBaseline`/`baselineReport`.

- [ ] **Step 1: Write the failing report test**

Append to `internal/report/report_test.go`:

```go
func TestPrintBaselineRendersDeviationsAndTotals(t *testing.T) {
	var buf bytes.Buffer
	err := printBaseline(&baseline.Report{
		Deviations: []baseline.Deviation{
			{Kind: "Deployment", Namespace: "prod", Name: "api", BaselineRate: 0.12, CurrentRate: 2.40, Pods: 3},
			{Kind: "StatefulSet", Namespace: "prod", Name: "cache", BaselineRate: 0, CurrentRate: 0.80, Pods: 2},
		},
		Compared: 42, NotInBaseline: 3, GoneFromCluster: 1,
	}, &buf)
	if err != nil {
		t.Fatalf("printBaseline: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Baseline deviations (confidence: medium — a learned rate, not a detector)",
		"Deployment prod/api",
		"0.12 → 2.40 restarts/hour",
		"(20x baseline, 3 pods)",
		"StatefulSet prod/cache",
		"0.00 → 0.80 restarts/hour",
		"(2 pods)",
		"42 workloads compared, 3 not in the baseline, 1 no longer present.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// A zero baseline has no multiple: "0x baseline" is not a number anyone can act on.
	if strings.Contains(out, "x baseline, 2 pods") {
		t.Errorf("a zero baseline was rendered with a multiple:\n%s", out)
	}
}

func TestPrintBaselineSaysSoWhenItFoundNothing(t *testing.T) {
	var buf bytes.Buffer
	if err := printBaseline(&baseline.Report{Deviations: []baseline.Deviation{}, Compared: 7}, &buf); err != nil {
		t.Fatalf("printBaseline: %v", err)
	}
	if !strings.Contains(buf.String(), "none") {
		t.Errorf("an empty comparison rendered nothing an operator can read:\n%s", buf.String())
	}
}

func TestPrintBaselineIsSilentWithoutTheFlag(t *testing.T) {
	var buf bytes.Buffer
	if err := printBaseline(nil, &buf); err != nil {
		t.Fatalf("printBaseline(nil): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printBaseline(nil) wrote %q; the section must be entirely conditional", buf.String())
	}
}
```

Add `"github.com/imantaba/kubeagent/internal/baseline"` to the test file's imports.

Update the scan surface table in `internal/cli/surface_test.go` — three new rows and the count:

```go
		{"baseline", []string{"--baseline", "/nonexistent/baseline.json"}, func(o scanOptions) bool { return o.baselinePath == "/nonexistent/baseline.json" }},
		{"baseline-factor", []string{"--baseline-factor", "5"}, func(o scanOptions) bool { return o.baselineFactor == 5 }},
		{"baseline-floor", []string{"--baseline-floor", "0.25"}, func(o scanOptions) bool { return o.baselineFloor == 0.25 }},
```

```go
	if len(cases) != 37 {
		t.Errorf("scan surface table has %d cases, want 37 — one per declared flag", len(cases))
	}
```

Add the two new defaults to `TestCommandSurfaceScanDefaults` in the style that test already uses:

```go
	if o.baselineFactor != baseline.DefaultFactor {
		t.Errorf("--baseline-factor default = %v, want %v", o.baselineFactor, baseline.DefaultFactor)
	}
	if o.baselineFloor != baseline.DefaultFloor {
		t.Errorf("--baseline-floor default = %v, want %v", o.baselineFloor, baseline.DefaultFloor)
	}
```

And a row in `internal/cli/normalize_test.go`'s `TestNormalize` table, proving the single-dash shim covers the new flag:

```go
		{"new baseline flag in single-dash form", []string{"-baseline", "cluster-baseline.json"}, []string{"--baseline", "cluster-baseline.json"}},
```

- [ ] **Step 2: Run and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ ./internal/cli/
```

Expected: `undefined: printBaseline`, `o.baselinePath undefined`, and the two count assertions failing.

- [ ] **Step 3: Add the report plumbing**

In `internal/report/report.go`, import `internal/baseline`, then:

**(a)** Add to `ScanReport`, immediately after the `Policy` line:

```go
	Baseline           *baseline.Report            `json:"baseline,omitempty"`
```

**(b)** Add to `Input`, immediately after the `Policy` field:

```go
	// Baseline is the restart-rate comparison (opt-in --baseline). Nil when the
	// flag is absent, so a default scan's text and JSON are unchanged — which is
	// what keeps testdata/golden-scan.txt byte-identical.
	Baseline *baseline.Report
```

**(c)** In `PrintInventory`'s `ScanReport{…}` literal, add `Baseline: in.Baseline,` beside `Policy:`.

**(d)** Add the renderer beside `printPolicy`:

```go
// printBaseline renders the restart-rate comparison. Like printPolicy it prints
// even when it found nothing: the operator passed --baseline by name, and
// silence would be indistinguishable from the flag not working.
//
// The heading states the section's confidence in internal/confidence's
// vocabulary. A learned rate is an inference, not a detector match on a named
// failure mode, and internal/confidence is explicit that such a signal is
// informational only — it never affects priority and it never affects the
// cluster verdict, which this section does not touch.
func printBaseline(r *baseline.Report, w io.Writer) error {
	if r == nil {
		return nil
	}
	if _, err := fmt.Fprint(w, "Baseline deviations (confidence: medium — a learned rate, not a detector)\n\n"); err != nil {
		return err
	}
	if len(r.Deviations) == 0 {
		if _, err := fmt.Fprintln(w, "  none"); err != nil {
			return err
		}
	}
	for _, d := range r.Deviations {
		target := fmt.Sprintf("%s %s/%s", d.Kind, d.Namespace, d.Name)
		if _, err := fmt.Fprintf(w, "  %-28s %.2f → %.2f restarts/hour   (%s)\n",
			target, d.BaselineRate, d.CurrentRate, deviationDetail(d)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\n  %s compared, %d not in the baseline, %d no longer present.\n",
		plural(r.Compared, "workload", "workloads"), r.NotInBaseline, r.GoneFromCluster); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w)
	return err
}

// deviationDetail describes the size of the change. A zero baseline has no
// multiple — "0x baseline" is not a number anyone can act on — so it reports
// only how many pods are behind the current rate.
func deviationDetail(d baseline.Deviation) string {
	pods := plural(d.Pods, "pod", "pods")
	if d.BaselineRate <= 0 {
		return pods
	}
	return fmt.Sprintf("%.0fx baseline, %s", d.CurrentRate/d.BaselineRate, pods)
}
```

**(e)** Call it, immediately after the `printPolicy` block:

```go
	if err := printBaseline(in.Baseline, w); err != nil {
		return err
	}
```

- [ ] **Step 4: Add the three flags and the wiring**

In `internal/cli/scan.go`, add to `scanOptions` (after the policy field):

```go
	baselinePath   string
	baselineFactor float64
	baselineFloor  float64
```

Add to `bindScanFlags`:

```go
	f.StringVar(&o.baselinePath, "baseline", "", "compare restart rates against this captured baseline (see `"+invokedAs+" baseline capture`)")
	f.Float64Var(&o.baselineFactor, "baseline-factor", envFloat("KUBEAGENT_BASELINE_FACTOR", baseline.DefaultFactor),
		"with --baseline: flag a workload at this multiple of its baseline rate (KUBEAGENT_BASELINE_FACTOR)")
	f.Float64Var(&o.baselineFloor, "baseline-floor", envFloat("KUBEAGENT_BASELINE_FLOOR", baseline.DefaultFloor),
		"with --baseline: also require this absolute rise in restarts/hour (KUBEAGENT_BASELINE_FLOOR)")
```

In `runScan`, load the document **before any cluster call** — insert immediately before `client, err := cluster.NewClient(o.kubeconfig, o.contextName)`:

```go
	// Load before connecting: an unreadable or wrong-version baseline is bad
	// input, and nothing about the cluster should have been attempted when the
	// run fails on it.
	baselineDoc, err := loadBaseline(o.baselinePath)
	if err != nil {
		return err
	}
```

The existing `client, err := cluster.NewClient(…)` line stays exactly as it is: `client` is a new variable on the left, so `:=` still compiles even though `err` is now already declared above it.

And immediately before `in := resultInput(res)`:

```go
	baselineRep := baselineReport(baselineDoc, o.baselineFactor, o.baselineFloor, res.Inputs, time.Now())
```

Then beside the other `in.` assignments:

```go
	in.Baseline = baselineRep
```

- [ ] **Step 5: Bump the scan schema and regenerate**

In `internal/jsonschema/jsonschema.go`, `ScanVersion` becomes `"1.2"`.

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/schemadoc -run TestSchemaDrift -update
go test ./internal/schemadoc/ -v
git diff --stat website/docs/schemas/
```

Expected: only `scan-v1.json` changes, and the second run is clean. If `-update` **refuses** with a `BREAKING` classification, stop — an added `omitempty` property must classify as additive, and a refusal means the field was declared required or a type moved.

- [ ] **Step 6: Run everything, including the golden test**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: PASS everywhere, and `golden-scan.txt` shows **no** diff. If it moved, the section is not conditional — fix that, do not regenerate the golden file.

- [ ] **Step 7: Try it end to end on the disposable cluster**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent .
./kubeagent baseline capture --context kind-kubeagent-baseline --min-pod-age 0 > /tmp/baseline.json
./kubeagent scan --context kind-kubeagent-baseline --baseline /tmp/baseline.json | tail -20
./kubeagent scan --context kind-kubeagent-baseline --baseline /tmp/baseline.json --output json | \
  python3 -c 'import json,sys; print(json.load(sys.stdin)["baseline"])'
```

Expected: the text section renders with zero deviations (nothing has changed since capture) and the JSON carries a `baseline` key. Then confirm a deviation is reachable:

```bash
kubectl --context kind-kubeagent-baseline create deployment flapper --image=busybox:1.36 -- /bin/sh -c 'sleep 5; exit 1'
sleep 180
./kubeagent scan --context kind-kubeagent-baseline --baseline /tmp/baseline.json | tail -20
```

Expected: `flapper` appears under `not in the baseline` (it did not exist at capture time) — which is the correct behavior, and confirms new workloads are counted, never flagged. To see a real deviation, re-capture with the flapper present, wait, and compare again.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/scan.go internal/cli/surface_test.go internal/cli/normalize_test.go \
        internal/report/report.go internal/report/report_test.go \
        internal/jsonschema/jsonschema.go website/docs/schemas/scan-v1.json
git commit -s -m "feat(scan): compare restart rates against a captured baseline

scan --baseline loads a document captured earlier and reports workloads whose
current restart rate is both a multiple of and an absolute rise above their
learned normal. --baseline-factor and --baseline-floor tune the two thresholds.

The section states its own confidence in internal/confidence's vocabulary: a
learned rate is an inference, not a detector match, so it is informational and
does not touch the cluster verdict. It renders only when --baseline was passed,
which is what keeps testdata/golden-scan.txt byte-identical.

scan's schema moves 1.1 -> 1.2: one added optional property, classified
additive by TestSchemaDrift."
```

---

### Task 6: `gate --baseline`

**Files:**
- Modify: `internal/findings/findings.go` (add `FromBaseline` beside `FromPolicy` at `:206-227`)
- Modify: `internal/gate/gate.go:30-55` (`Options`) and `:164-233` (`Decide`)
- Modify: `internal/cli/gate.go:50-83` and the body of `runGateOpts`
- Modify: `internal/cli/surface_test.go` (gate table 10 → 13, defaults)
- Test: `internal/findings/findings_test.go`, `internal/gate/gate_test.go`

**Interfaces:**
- Consumes: `baseline.Report` (Task 1); `loadBaseline`, `baselineReport` (Task 4).
- Produces: `func FromBaseline(r *baseline.Report) []Finding`; `gate.Options.Baseline *baseline.Report`.

**Gate's schema does NOT move.** Deviations become ordinary `findings.Finding` values in the existing `Failing`/`Reported` arrays, so `gate.Verdict` gains no key and `GateVersion` stays `"1.1"`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/findings/findings_test.go`:

```go
func TestFromBaselineMapsDeviationsToInfo(t *testing.T) {
	got := FromBaseline(&baseline.Report{Deviations: []baseline.Deviation{
		{Kind: "Deployment", Namespace: "prod", Name: "api", BaselineRate: 0.12, CurrentRate: 2.40, Pods: 3},
		{Kind: "StatefulSet", Namespace: "prod", Name: "cache", BaselineRate: 0, CurrentRate: 0.80, Pods: 2},
	}})
	if len(got) != 2 {
		t.Fatalf("FromBaseline returned %d findings, want 2", len(got))
	}
	for _, f := range got {
		if f.Level != Info {
			t.Errorf("%s is at %v, want Info — a learned rate is an inference, not a detector match", f.Name, f.Level)
		}
		if f.Issue != "RestartRateDeviation" {
			t.Errorf("Issue = %q, want RestartRateDeviation", f.Issue)
		}
	}
	if got[0].Reason != "0.12 -> 2.40 restarts/hour (20x baseline, 3 pods)" {
		t.Errorf("Reason = %q", got[0].Reason)
	}
	if got[1].Reason != "0.00 -> 0.80 restarts/hour (2 pods)" {
		t.Errorf("zero-baseline Reason = %q, want no multiple", got[1].Reason)
	}
}

func TestFromBaselineIsEmptyWithoutAReport(t *testing.T) {
	if got := FromBaseline(nil); len(got) != 0 {
		t.Errorf("FromBaseline(nil) = %+v, want nothing", got)
	}
}
```

Append to `internal/gate/gate_test.go`:

```go
func TestDecideReportsADeviationButDoesNotFailByDefault(t *testing.T) {
	rep := &baseline.Report{Deviations: []baseline.Deviation{
		{Kind: "Deployment", Namespace: "prod", Name: "api", BaselineRate: 0.12, CurrentRate: 2.40, Pods: 3},
	}}

	v := Decide(scan.Result{}, Options{FailOn: findings.Critical, Baseline: rep})
	if v.Verdict != "pass" || v.Code != CodePass {
		t.Errorf("verdict = %s (%d), want pass — a deviation must never fail a gate at the default --fail-on", v.Verdict, v.Code)
	}
	if len(v.Reported) != 1 || v.Reported[0].Issue != "RestartRateDeviation" {
		t.Errorf("Reported = %+v, want the deviation reported", v.Reported)
	}

	v = Decide(scan.Result{}, Options{FailOn: findings.Info, Baseline: rep})
	if v.Verdict != "fail" || v.Code != CodeFail {
		t.Errorf("verdict = %s (%d), want fail at --fail-on info", v.Verdict, v.Code)
	}
	if len(v.Failing) != 1 {
		t.Errorf("Failing = %+v, want the deviation failing", v.Failing)
	}
}
```

Add the three gate surface rows and update the count in `internal/cli/surface_test.go`:

```go
		{"baseline", []string{"--baseline", "/nonexistent/baseline.json"}, func(o gateOptions) bool { return o.baselinePath == "/nonexistent/baseline.json" }},
		{"baseline-factor", []string{"--baseline-factor", "5"}, func(o gateOptions) bool { return o.baselineFactor == 5 }},
		{"baseline-floor", []string{"--baseline-floor", "0.25"}, func(o gateOptions) bool { return o.baselineFloor == 0.25 }},
```

```go
	if len(cases) != 13 {
		t.Errorf("gate surface table has %d cases, want 13 — one per declared flag", len(cases))
	}
```

And the two defaults in `TestCommandSurfaceGateDefaults`, matching the style that test already uses.

- [ ] **Step 2: Run and watch them fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/findings/ ./internal/gate/ ./internal/cli/
```

Expected: `undefined: FromBaseline`, `unknown field Baseline in gate.Options`, and the gate count assertion failing.

- [ ] **Step 3: Add `FromBaseline`**

In `internal/findings/findings.go`, import `internal/baseline` and add beside `FromPolicy`:

```go
// FromBaseline maps restart-rate deviations to findings at Info.
//
// Info is where the level's own comment reserved it: "no detector emits it yet,
// but --fail-on info must have a meaning". This is that meaning. A deviation is
// an inference from a learned rate, not a detector match on a concrete named
// failure mode, so it is reported at every --fail-on setting and fails a gate
// only at --fail-on info — which is the operator opting in. Because
// --fail-on defaults to critical, no existing pipeline changes behavior.
//
// It carries no Owner: the deviation already names the workload itself in
// Kind/Namespace/Name, which is the first branch a --wait-for-scoped gate
// matches on.
func FromBaseline(r *baseline.Report) []Finding {
	if r == nil {
		return nil
	}
	out := make([]Finding, 0, len(r.Deviations))
	for _, d := range r.Deviations {
		out = append(out, Finding{
			Level: Info, Kind: d.Kind, Namespace: d.Namespace, Name: d.Name,
			Issue:  "RestartRateDeviation",
			Reason: baselineReason(d),
		})
	}
	return out
}

// baselineReason renders the two rates and the size of the change. A zero
// baseline has no multiple, so it reports only how many pods are behind the
// current rate.
func baselineReason(d baseline.Deviation) string {
	if d.BaselineRate <= 0 {
		return fmt.Sprintf("%.2f -> %.2f restarts/hour (%d pods)", d.BaselineRate, d.CurrentRate, d.Pods)
	}
	return fmt.Sprintf("%.2f -> %.2f restarts/hour (%.0fx baseline, %d pods)",
		d.BaselineRate, d.CurrentRate, d.CurrentRate/d.BaselineRate, d.Pods)
}
```

- [ ] **Step 4: Add the gate option and the hook**

In `internal/gate/gate.go`, import `internal/baseline` and add to `Options` after `PolicyNotEvaluated`:

```go
	// Baseline is the restart-rate comparison (--baseline), nil when the flag is
	// absent. Its deviations join the flattened findings at findings.Info, so
	// --fail-on and --wait-for scoping apply to them unchanged. Because
	// --fail-on defaults to critical, a deviation never fails a gate unless the
	// operator asks for it with --fail-on info.
	Baseline *baseline.Report
```

In `Decide`, beside the existing `FromPolicy` append:

```go
	all := findings.Flatten(res)
	all = append(all, findings.FromPolicy(opts.PolicyViolations, opts.PolicyNotEvaluated)...)
	all = append(all, findings.FromBaseline(opts.Baseline)...)
	findings.Sort(all)
```

- [ ] **Step 5: Add the three flags and the wiring**

In `internal/cli/gate.go`, add to `gateOptions`:

```go
	baselinePath   string
	baselineFactor float64
	baselineFloor  float64
```

Add to `bindGateFlags`, using the **same** usage strings as scan's so `--help` reads identically on both commands:

```go
	f.StringVar(&o.baselinePath, "baseline", "", "compare restart rates against this captured baseline (see `"+invokedAs+" baseline capture`)")
	f.Float64Var(&o.baselineFactor, "baseline-factor", envFloat("KUBEAGENT_BASELINE_FACTOR", baseline.DefaultFactor),
		"with --baseline: flag a workload at this multiple of its baseline rate (KUBEAGENT_BASELINE_FACTOR)")
	f.Float64Var(&o.baselineFloor, "baseline-floor", envFloat("KUBEAGENT_BASELINE_FLOOR", baseline.DefaultFloor),
		"with --baseline: also require this absolute rise in restarts/hour (KUBEAGENT_BASELINE_FLOOR)")
```

In `runGateOpts`, load the document with the other flag validation — immediately after the `--poll-interval` check, before `cluster.NewClient`:

```go
	// Exit 4 for a bad baseline file, for the same reason a bad policy file
	// takes it: bad input, in the same class as a bad flag, and nothing was
	// attempted against the cluster. Exit 1 would claim kubeagent looked and
	// found problems.
	baselineDoc, err := loadBaseline(o.baselinePath)
	if err != nil {
		return &exitError{code: gate.CodeUsage, msg: err.Error()}
	}
```

And after the `scanRes, err := scan.Evaluate(…)` block succeeds, before `verdict := gate.Decide(scanRes, opts)`:

```go
	opts.Baseline = baselineReport(baselineDoc, o.baselineFactor, o.baselineFloor, scanRes.Inputs, time.Now())
```

Add `"time"` to the file's imports if it is not already there.

Also add the usage-string fragment for gate in `internal/cli/root.go`'s `usageError()`, inside the existing gate clause, after `[--policy path (repeatable)]`:

```text
 [--baseline path] [--baseline-factor f] [--baseline-floor f]
```

- [ ] **Step 6: Run everything**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
go test ./internal/schemadoc -run TestSchemaDrift
```

Expected: PASS everywhere, and TestSchemaDrift clean **without** `-update` — gate's shape did not move.

- [ ] **Step 7: Try it on the disposable cluster**

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent .
./kubeagent gate --context kind-kubeagent-baseline --baseline /tmp/baseline.json; echo "exit: $?"
./kubeagent gate --context kind-kubeagent-baseline --baseline /tmp/baseline.json --fail-on info; echo "exit: $?"
./kubeagent gate --context kind-kubeagent-baseline --baseline /nonexistent/x.json; echo "exit: $? (want 4)"
```

Expected: the default run passes (a deviation is reported, never failing); `--fail-on info` fails only if a deviation is present; a missing file exits 4 with the message on stderr.

- [ ] **Step 8: Commit**

```bash
git add internal/findings/findings.go internal/findings/findings_test.go \
        internal/gate/gate.go internal/gate/gate_test.go \
        internal/cli/gate.go internal/cli/root.go internal/cli/surface_test.go
git commit -s -m "feat(gate): report restart-rate deviations at findings.Info

gate --baseline maps each deviation to a Finding at Info, the level whose own
comment reserved it for exactly this: reported at every --fail-on setting, and
failing a gate only at --fail-on info. Because --fail-on defaults to critical,
no existing pipeline changes behavior and no new gate flag is needed for the
opt-in.

gate.Verdict gains no key, so the gate schema stays at 1.1: deviations land in
the Failing and Reported arrays that already exist."
```

---

### Task 7: Documentation

**Files:**
- Create: `website/docs/features/baseline.md`
- Modify: `website/mkdocs.yml:57-78` (features nav)
- Modify: `website/docs/features/json-schema.md:102-107` (six schema URLs → seven)
- Modify: `website/docs/features/ci-gate.md:341`
- Modify: `README.md:141`, `website/docs/features/diagnostics.md:316-320`, `website/docs/roadmap.md:74`
- Modify: `website/docs/roadmap.md` (the post-1.0 milestone row)
- Modify: `CLAUDE.md` (six → seven documents; `internal/baseline` layering)
- Modify: `CHANGELOG.md` (under `## [Unreleased]`)

**Interfaces:** none — this task ships no code.

- [ ] **Step 1: Write the feature page**

Create `website/docs/features/baseline.md`. It must cover, in this order:

1. **What it is** — an operator-captured file recording what each workload's restart rate normally is on *this* cluster, and a comparison that reports workloads much worse than their own normal.
2. **The honesty clause, prominently and early** — verbatim in substance from spec §5: "This measures restarts over the lifetimes of the pods present when the sample was taken. It is not long-term history. A workload whose pods were all recreated an hour before capture shows only what those pods have done since." Reference `internal/capacity`'s equivalent statement about metrics-server as the precedent.
3. **The workflow** — `kubeagent baseline capture > cluster-baseline.json`, review it, commit it, then `kubeagent scan --baseline cluster-baseline.json` and `kubeagent gate --baseline cluster-baseline.json`.
4. **The maths** — `RestartsPerHour = sum(restarts of counted pods) / (sum(age of counted pods) / 3600)`; a pod counts only at or above `--min-pod-age` (default `1h`) and an excluded pod leaves *both* sides; a workload with no counted pods gets no entry, because unknown is not zero.
5. **The two thresholds** — `current >= baseline × factor` (default 3.0) **and** `current − baseline >= floor` (default 0.5/hour); why both; only increases; new and gone workloads counted, never flagged.
6. **The flags and env vars** — the table of `--baseline`, `--baseline-factor` / `KUBEAGENT_BASELINE_FACTOR`, `--baseline-floor` / `KUBEAGENT_BASELINE_FLOOR`, `--min-pod-age` / `KUBEAGENT_BASELINE_MIN_POD_AGE`.
7. **Gate behavior** — `findings.Info`; never fails a gate at the default `--fail-on critical`; `--fail-on info` is the explicit opt-in.
8. **Guarantees** — `baseline capture` is read-only toward the cluster (List calls only) and makes no model call; state those as **two separate promises**. It needs no RBAC grant beyond what `scan` already has. It writes no file: the document goes to stdout so the operator sees it first.
9. **What it is not** — the whole of spec §10: no HTML/TUI/MCP/dashboard surface, no watch-daemon integration, no inventory drift, no second dimension, no automatic capture, no multi-baseline merge.
10. **The schema** — link `../schemas/baseline-v1.json` and note `kubeagent schema baseline` prints it with no cluster.

Every example uses generic names (`prod`, `api`, `cache`). No node names, no context names, no real paths, no URLs beyond the project's own `https://k8sproject.top/…`.

- [ ] **Step 2: Add it to the nav**

In `website/mkdocs.yml`, in the features list, add the entry after the in-cluster dashboard line, matching the surrounding indentation exactly:

```yaml
      - Restart-rate baseline: features/baseline.md
```

- [ ] **Step 3: Update the schema index**

In `website/docs/features/json-schema.md:102-107`, add the seventh URL to the list, in the same style as the six already there, pointing at `baseline-v1.json` and naming the surface as `kubeagent baseline capture`.

- [ ] **Step 4: Correct the "no baseline/diff mode" line**

`website/docs/features/ci-gate.md:341` currently reads:

```markdown
- No baseline/diff mode (comparing this run against a previous one).
```

Replace it with a line that is true of what now ships — restart rates are covered, everything else is not:

```markdown
- Diff mode covers restart rates only: `--baseline` compares this run's restart
  rates against a [captured baseline](baseline.md). Nothing else is compared
  against a previous run — findings, inventory and resource usage are judged
  fresh each time.
```

- [ ] **Step 5: Free the word "baseline" for the learned meaning**

Three places call the static `--expected-nodes` list a "baseline". Reword each to "expected-node list", leaving the surrounding text otherwise untouched:

- `README.md:141` — `**Expected-node baseline (opt-in)**` → `**Expected-node list (opt-in)**`
- `website/docs/features/diagnostics.md:316` — `### Expected-node baseline` → `### Expected-node list`
- `website/docs/roadmap.md:74` — `- **Expected-node baseline** —` → `- **Expected-node list** —`

**In the same edit**, fix a real leak in the section at `website/docs/features/diagnostics.md:318-320`: the published example names `nova-worker-1` and `nova-worker-2`, which are internal hostnames from a real estate. Replace them with generic names throughout that paragraph:

```markdown
`scan --expected-nodes node-a,node-b,…` declares the node names you
expect. kubeagent flags each declared node that has **no `Node` object** in the
cluster — `✗ node node-b expected but absent from the cluster` — which
```

Verify with `grep -rn "nova-" README.md website/`, which must return nothing.

- [ ] **Step 6: Update the roadmap's post-1.0 row**

In `website/docs/roadmap.md`'s milestone table, the **post-1.0** row currently reads:

```markdown
| **post-1.0** | The best, sustained | Anomaly/baseline learning ("what's normal for *this* cluster"); fleet-scale (hundreds of clusters); a curated community detector library and known-issues knowledge base |
```

Replace with a row that records exactly what shipped and no more:

```markdown
| **post-1.0** | The best, sustained | Anomaly/baseline learning ("what's normal for *this* cluster") — **restart rates shipped** (`kubeagent baseline capture`, `scan --baseline`, `gate --baseline`); other dimensions, fleet-scale (hundreds of clusters), and a curated community detector library and known-issues knowledge base still ahead |
```

- [ ] **Step 7: Update `CLAUDE.md`**

Two edits.

**(a)** The versioned-contract invariant currently opens `**The six JSON documents are a versioned contract.**` and lists six roots. Make it seven, adding `baseline.Document`, and update the version note: scan is now at **1.2** (added `baseline`, `omitempty`), gate stays at **1.1**, and baseline enters at **1.0**.

**(b)** Add `internal/baseline` to the layering invariants, in the paragraph that names `internal/dashboard` and `internal/jsonschema` as the imports-nothing class:

```markdown
  `internal/baseline` (the learned restart-rate package) is an eighth case and
  joins the strictest class: it **imports nothing from kubeagent at all** and
  nothing outside the standard library, which puts it alongside
  `internal/jsonschema` and `internal/dashboard` and makes reaching
  `internal/remediate` or `internal/explain` impossible by construction rather
  than by rule. `internal/baseline/imports_test.go` enforces both halves. It
  holds no client and no context, issues no cluster call and makes no model
  call — two separate promises. `internal/findings` and `internal/report` import
  it; it imports neither of them
  (see [website/docs/features/baseline.md](website/docs/features/baseline.md)).
```

- [ ] **Step 8: Write the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`:

```markdown
### Added

- **A learned restart-rate baseline — "what's normal for *this* cluster".**
  `kubeagent baseline capture` prints a JSON document recording each workload's
  restart rate, normalised across its pods' observed lifetimes; `scan --baseline
  <file>` and `gate --baseline <file>` compare a later run against it and report
  workloads that restart much more than their own normal. A workload deviates
  only when both thresholds hold — at least `--baseline-factor` times its
  baseline rate (default 3.0) **and** at least `--baseline-floor` restarts/hour
  above it (default 0.5) — so a rise from 0.001 to 0.01 is not a 10× alarm.
  Pods younger than `--min-pod-age` (default 1h) are excluded from both sides of
  the rate.
- **The baseline document is the seventh versioned JSON document**, published at
  version 1.0 as `website/docs/schemas/baseline-v1.json` and printable with
  `kubeagent schema baseline`.
- `kubeagent baseline capture` is read-only toward the cluster (List calls only,
  all of them already in the `scan` RBAC profile) and makes no model call. It
  writes no file: the document goes to stdout, so an operator reviews it before
  deciding where it goes.

### Changed

- **`scan`'s JSON schema moves 1.1 → 1.2**, additively: `ScanReport` gains an
  optional `baseline` object, present only when `--baseline` was passed. Every
  existing consumer is unaffected. `gate`'s schema does not move — deviations
  are ordinary findings at the `info` level, so they land in the `failing` and
  `reported` arrays that already exist, and because `--fail-on` defaults to
  `critical` a deviation never fails a gate unless an operator asks for it with
  `--fail-on info`.
- The static `--expected-nodes` list is now called the **expected-node list**
  rather than the "expected-node baseline", so "baseline" names one thing.
```

- [ ] **Step 9: Build the site and run the suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
(cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: tests PASS; mkdocs exits 0 with no `WARNING` lines naming your pages. The red "Material for MkDocs 2.0" banner is cosmetic.

- [ ] **Step 10: Secret-leak sweep**

```bash
grep -rn "nova-\|ekb-" README.md website/ CLAUDE.md CHANGELOG.md
grep -rnE "https?://" website/docs/features/baseline.md
```

Expected: the first returns nothing. The second returns only `https://k8sproject.top/…` links, if any.

- [ ] **Step 11: Commit**

```bash
git add README.md CHANGELOG.md CLAUDE.md website/docs/features/baseline.md \
        website/docs/features/ci-gate.md website/docs/features/diagnostics.md \
        website/docs/features/json-schema.md website/docs/roadmap.md website/mkdocs.yml
git commit -s -m "docs: the learned restart-rate baseline

A new feature page carrying the honesty clause prominently — the rate measures
restarts over the lifetimes of the pods present at capture, not long-term
history — plus the workflow, the two thresholds, and everything this slice
deliberately does not do.

'Baseline' now names one thing: the static --expected-nodes list is the
expected-node list. The ci-gate page's 'no baseline/diff mode' line is corrected
to say what is and is not compared against a previous run, and the diagnostics
example's node names are made generic."
```

---

## Acceptance

Run on the branch when every task is complete (spec §15):

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test -p 2 ./...
git diff main -- internal/report/testdata/golden-scan.txt   # must be empty
git diff main -- go.mod go.sum                              # must be empty
go test ./internal/schemadoc -run TestSchemaDrift            # clean, no -update
bash scripts/dco-check.sh main HEAD
(cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Plus the live checks in Task 4 step 6, Task 5 step 7 and Task 6 step 7, and:

```bash
KUBECONFIG=/nonexistent ./kubeagent schema baseline | head -5   # prints with no cluster
```

**The chaos harness is not extended in this slice** and **must not be run**: nothing here touches cluster writes, `nodes/proxy`, RBAC or the daemon, so the pre-release gate is the lightweight disposable-cluster smoke above, not the full outage suite. A chaos run injects real outages and takes about forty minutes.

Tear the disposable cluster down when the acceptance checks pass:

```bash
kind delete cluster --name kubeagent-baseline
```

## Self-review notes

- **Spec coverage:** §4 → Task 1; §5, §6 → Task 1; §7 → Tasks 3 and 5; §8 → Tasks 4, 5, 6; §9 → Tasks 5 and 6 (the confidence heading and the Info level); §10 → Task 7 step 1 item 9; §11 → the test steps of every task; §12 → Task 4's help text and Task 7 step 10; §13 → Global Constraints; §14 → Task 7; §15 → Acceptance.
- **The one spec deviation** is §8's capture source, corrected at the top of this plan and implemented in Task 2. It is stated there so an implementer does not reopen it.
- `docs/go-concepts.md` gains no entry: this slice introduces no Go concept the cheat-sheet lacks. Add one only if the implementation reaches for something new.
