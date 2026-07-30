# Fuzzed Detectors Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make it impossible for a malformed, empty, or hostile Kubernetes API object to panic a kubeagent scan or push raw attacker-controlled bytes onto an operator's terminal, and prove it with Go native fuzzing.

**Architecture:** Three new pieces plus fixes at the ingress points they expose. `internal/safetext` is a pure, three-rule sanitizer (`Line`) applied where untrusted text first enters a kubeagent value. `internal/fuzzgen` is a test-only deterministic bytes-to-object builder: a `Cursor` draws a Pod, its Events, and a TLS Secret from a fuzzer's `[]byte`, drawing DNS-1123-safe alphabets for the fields the API server validates and hostile bytes for the fields it does not. Seven fuzz targets across six packages assert four properties — no panic, purity, determinism, output safety — and their seed corpora replay on every plain `go test`. A nightly `workflow_dispatch`-able GitHub Actions matrix runs one `(package, target)` pair per job, because `go test -fuzz` accepts exactly one target and one package per invocation.

**Tech Stack:** Go 1.26 standard library only (`testing` native fuzzing, `strings`, `unicode`, `unicode/utf8`, `go/parser`, `math`, `strconv`), `k8s.io/api` types already in `go.mod`, GitHub Actions.

## Global Constraints

Every task's requirements implicitly include this section. Copied verbatim from the spec and the project's standing rules.

- **Every commit needs a `Signed-off-by` trailer matching its author** — use `git commit -s`. `main` enforces DCO. Verify with `scripts/dco-check.sh`.
- **No `Co-Authored-By: Claude` trailer and no AI attribution of any kind** — not in commit messages, code, comments, docs, changelogs, or PR text. Every commit is authored solely by the human.
- **Detectors stay pure functions.** No I/O, no clock reads, no mutation of the `PodFacts` they are handed.
- **Standard-library `flag` package only** — no Cobra, no new CLI framework.
- **No new third-party dependency.** Nothing may be added to `go.mod`.
- **No secrets, credentials, private IPs, or internal hostnames anywhere** — including test fixtures and fuzz seed corpora. Documentation and test IPs are RFC 5737 (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`); example domains are RFC 2606 (`.example`).
- **URLs are credentials.** No log line, error, metric label, results file, rendered manifest, doc example, or seed corpus entry may carry more than `scheme://host`.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** No task in this plan intends to change the report format. If the golden test fails, the change is wrong — do not regenerate the golden file.
- **`go test` runs with `-p 2`** (full parallelism trips a known Go linker panic on `internal/advisory`). Never `-short`.
- **`internal/safetext` and `internal/fuzzgen` must never import `internal/remediate` or `internal/explain`.** There must be no code path from either into a write or into a model call.
- **Each fuzz property must be watched failing on current code before its fix lands**, and each fix mutation-checked afterwards (revert the fix, confirm the property fails again, restore).
- Usage and error text uses `invokedAs` (from `argv[0]`), never a hardcoded `"kubeagent"`. No task here writes user-facing CLI text, but the rule stands.
- Go lives at `/usr/local/go/bin`: `export PATH=$PATH:/usr/local/go/bin`.
- Branch is `fuzzed-detectors`, cut from `main` at `c07bd7f`. Never commit to `main`.

## File Structure

**New:**

| File | Responsibility |
|------|----------------|
| `internal/safetext/safetext.go` | `MaxLine` + `Line` — the one sanitizer, three rules, no state |
| `internal/safetext/safetext_test.go` | Table test per rule plus the no-op-on-clean-text case |
| `internal/diagnose/defaults.go` | `DefaultDetectors(now)` — the detector set, one definition |
| `internal/diagnose/defaults_test.go` | Order, count, and injected-clock assertions |
| `internal/fuzzgen/cursor.go` | `Cursor` — total, wrapping, never-panicking primitive draws |
| `internal/fuzzgen/build.go` | `Pod`, `Events`, `TLSSecret` builders; `Base` |
| `internal/fuzzgen/assert.go` | `AssertSafe`, `AssertBounded` |
| `internal/fuzzgen/cursor_test.go` | Cursor totality, bounds, determinism |
| `internal/fuzzgen/build_test.go` | Builder determinism and DNS-1123 shape |
| `internal/fuzzgen/imports_test.go` | `TestNoProductionImport` — keeps `testing` out of the shipped binary |
| `internal/diagnose/fuzz_test.go` | `FuzzDetectors` |
| `internal/logscan/fuzz_test.go` | `FuzzClassify` |
| `internal/redact/fuzz_test.go` | `FuzzRedactURL`, `FuzzRedactError` |
| `internal/dnshealth/fuzz_test.go` | `FuzzParseResponses` |
| `internal/controlplane/fuzz_test.go` | `FuzzParseReadyz` |
| `internal/certhealth/fuzz_test.go` | `FuzzCertAssess` + the seed PEM literal |
| `.github/workflows/fuzz.yml` | Nightly + `workflow_dispatch` matrix, one job per `(package, target)` |

**Modified:**

| File | Change |
|------|--------|
| `internal/scan/scan.go:195-205` | Detector list becomes `diagnose.DefaultDetectors(time.Now())` |
| `internal/diagnose/imagepull.go:16` | Sanitize `w.Message` |
| `internal/diagnose/configerror.go:28` | Sanitize `w.Message` |
| `internal/diagnose/initcontainer.go:41` | Sanitize `w.Message` |
| `internal/diagnose/pending.go:18` | Sanitize `c.Message` |
| `internal/diagnose/volumeattach.go:31` | Sanitize `ev.Message` |
| `internal/diagnose/restartloop.go:41` | Sanitize `term.Reason` |
| `internal/diagnose/probefailure.go:47` | Sanitize `Container` (the eighth site — see Task 4) |
| `internal/logscan/logscan.go:55-71` | `truncate` becomes `sanitize`; `Line` before the 200-rune bound; sanitize the conn-refused capture |
| `internal/dnshealth/dnshealth.go:25-58,76-110` | Reject NaN/Inf/negative; clamp; saturating adds |
| `internal/controlplane/controlplane.go:34-46` | Sanitize check names; cap the list |
| `internal/certhealth/certhealth.go:73-76` | Sanitize the certificate's CommonName / SAN fallback |
| `internal/collect/collect.go:470-497` | 1 MiB cap on the three proxied reads |
| `CONTRIBUTING.md` | Fuzzing bullet in Testing |
| `docs/go-concepts.md` | New concept entry: native fuzzing |
| `CHANGELOG.md` | `[Unreleased]` — Added + Fixed |
| `website/docs/roadmap.md` | Theme H slice 3 shipped |
| `CLAUDE.md` | Two new invariants |

---

### Task 1: `internal/safetext` — the sanitizer

**Files:**
- Create: `internal/safetext/safetext.go`
- Test: `internal/safetext/safetext_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `const safetext.MaxLine = 512`; `func safetext.Line(s string) string`. Guarantee later tasks rely on: `Line`'s result is valid UTF-8, contains no `unicode.IsControl` rune, no `unicode.Is(unicode.Cf, …)` rune, no U+2028/U+2029, and is **at most `MaxLine` runes** (the ellipsis is inside the budget, not on top of it).

- [ ] **Step 1: Write the failing test**

Create `internal/safetext/safetext_test.go`:

```go
package safetext

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean text is unchanged", `Error: ImagePullBackOff pulling "registry.example/app:1.2"`, `Error: ImagePullBackOff pulling "registry.example/app:1.2"`},
		{"empty stays empty", "", ""},
		{"ansi escape loses its ESC", "\x1b[2J\x1b[Hgotcha", "[2J[Hgotcha"},
		{"osc title escape loses ESC and BEL", "\x1b]0;pwned\x07rest", "]0;pwnedrest"},
		{"nul is dropped", "a\x00b", "ab"},
		{"carriage return cannot overwrite the line", "real\rfake", "real fake"},
		{"newline folds to a space", "line1\nline2", "line1 line2"},
		{"tab folds to a space", "a\tb", "a b"},
		{"rtl override is dropped", "before\u202eafter", "beforeafter"},
		{"zero-width joiner is dropped", "a\u200db", "ab"},
		{"unicode line separator folds to a space", "a\u2028b\u2029c", "a b c"},
		{"invalid utf-8 becomes the replacement rune", "bad\xffbyte", "bad�byte"},
		{"surrounding whitespace is trimmed", "  padded\n", "padded"},
		{"non-ascii text survives", "café — naïve ✓", "café — naïve ✓"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Line(tc.in); got != tc.want {
				t.Errorf("Line(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLineTruncatesToMaxLineRunes(t *testing.T) {
	got := Line(strings.Repeat("x", MaxLine+200))
	if n := utf8.RuneCountInString(got); n != MaxLine {
		t.Errorf("rune count = %d, want exactly %d", n, MaxLine)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated result %q does not end in an ellipsis", got[len(got)-8:])
	}
}

func TestLineTruncatesOnRuneBoundaries(t *testing.T) {
	// A multi-byte rune must never be cut in half: a byte-indexed truncation
	// would produce invalid UTF-8, which is exactly what Line exists to remove.
	got := Line(strings.Repeat("é", MaxLine+200))
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != MaxLine {
		t.Errorf("rune count = %d, want exactly %d", n, MaxLine)
	}
}

func TestLineIsIdempotent(t *testing.T) {
	// Sanitizing twice must not change the result: detectors compose fields, and
	// a value may pass through Line at more than one layer.
	for _, in := range []string{"clean", "a\x1b[1mb", strings.Repeat("x", MaxLine+10), "bad\xffbyte"} {
		once := Line(in)
		if twice := Line(once); twice != once {
			t.Errorf("Line(Line(%q)) = %q, want %q", in, twice, once)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/safetext -p 2
```

Expected: FAIL to build — `internal/safetext/safetext.go` does not exist, so `Line` and `MaxLine` are undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/safetext/safetext.go`:

```go
// Package safetext bounds and sanitizes text that reaches kubeagent from fields
// the Kubernetes API server does not validate — container logs, event and
// condition messages, container-status reasons, certificate subjects — before it
// is put in front of an operator.
//
// Object names, namespaces and container names are NOT this package's problem:
// the API server validates them as DNS-1123 labels, so a real cluster cannot
// present anything hostile there. The unvalidated fields are the ones that
// matter, and the tail of a crashed container's log is the one an unprivileged
// attacker controls outright.
//
// Pure: no I/O, no state, no clock. Safe to call from a detector.
package safetext

import (
	"strings"
	"unicode"
)

// MaxLine is the rune budget for one sanitized line: long enough for any real
// kubelet or scheduler message, short enough that a hostile multi-megabyte one
// cannot own the terminal. The ellipsis a truncated line ends with is inside
// this budget, so Line's result is never longer than MaxLine runes.
const MaxLine = 512

// Line returns s fit to print: valid UTF-8, on one line, with no control or
// Unicode formatting characters, at most MaxLine runes.
//
// Three rules, in this order:
//
//  1. Invalid UTF-8 bytes become U+FFFD. A terminal's handling of a stray
//     continuation byte is its own business, not kubeagent's.
//  2. Whitespace controls (tab, newline, carriage return, vertical tab, form
//     feed) and the Unicode line separators U+2028/U+2029 fold to a space, so a
//     multi-line message reads as words rather than running together. Every
//     other control character is dropped — that covers ESC, which is what makes
//     an ANSI escape sequence an escape sequence, and NUL and BEL. So are the
//     Unicode formatting characters (category Cf): U+202E RIGHT-TO-LEFT OVERRIDE
//     reorders everything after it, and unicode.IsControl does not catch it
//     because it is Cf, not Cc.
//  3. The result is trimmed and truncated to MaxLine runes — runes, not bytes,
//     so a multi-byte character is never cut in half.
//
// Idempotent: Line(Line(s)) == Line(s).
func Line(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', '\u2028', '\u2029':
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > MaxLine {
		return string(r[:MaxLine-1]) + "…"
	}
	return s
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/safetext -p 2 -v
```

Expected: PASS, all subtests.

- [ ] **Step 5: Confirm the package's whole import list is two standard-library packages**

```bash
go list -deps ./internal/safetext | grep kubeagent
```

Expected: only `github.com/imantaba/kubeagent/internal/safetext` itself. Nothing from `internal/remediate` or `internal/explain`, and no third-party package.

- [ ] **Step 6: Commit**

```bash
git add internal/safetext
git commit -s -m "feat(safetext): sanitize untrusted API text before it reaches a terminal

The fields kubeagent's detectors read most are the fields the API server does
not validate: waiting.Message, terminated.Reason, condition and event messages,
container log text. safetext.Line bounds them to 512 runes and strips control
characters, Unicode formatting characters and invalid UTF-8, folding whitespace
controls to spaces so a multi-line message still reads."
```

---

### Task 2: `diagnose.DefaultDetectors` — one detector set

**Files:**
- Create: `internal/diagnose/defaults.go`
- Create: `internal/diagnose/defaults_test.go`
- Modify: `internal/scan/scan.go:195-205`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `func diagnose.DefaultDetectors(now time.Time) []Detector` — the nine production detectors, in report order, with `RestartLoopDetector.Now` set to `now`. Task 4's fuzz target calls it so the fuzzed run exercises exactly what a real scan runs.

**Why:** `RestartLoopDetector.Now` is the only non-determinism in the production detector set, and it is already injected. Extracting the list means the fuzz target cannot drift out of sync with what `scan` actually runs, and passing a fixed instant is what makes a determinism property meaningful.

- [ ] **Step 1: Write the failing test**

Create `internal/diagnose/defaults_test.go`:

```go
package diagnose

import (
	"fmt"
	"testing"
	"time"
)

func TestDefaultDetectorsOrder(t *testing.T) {
	// The order is the order findings are reported in. A reordering is a
	// user-visible output change, so it is pinned here as well as in the report
	// package's golden test.
	want := []string{
		"diagnose.CrashLoopDetector",
		"diagnose.ImagePullDetector",
		"diagnose.OOMKilledDetector",
		"diagnose.PendingDetector",
		"diagnose.VolumeAttachDetector",
		"diagnose.RestartLoopDetector",
		"diagnose.InitContainerDetector",
		"diagnose.ProbeFailureDetector",
		"diagnose.ConfigErrorDetector",
	}
	got := DefaultDetectors(time.Now())
	if len(got) != len(want) {
		t.Fatalf("got %d detectors, want %d", len(got), len(want))
	}
	for i, d := range got {
		if name := fmt.Sprintf("%T", d); name != want[i] {
			t.Errorf("detector %d = %s, want %s", i, name, want[i])
		}
	}
}

func TestDefaultDetectorsInjectsTheClock(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	for _, d := range DefaultDetectors(now) {
		if rl, ok := d.(RestartLoopDetector); ok {
			if !rl.Now.Equal(now) {
				t.Errorf("RestartLoopDetector.Now = %v, want %v", rl.Now, now)
			}
			return
		}
	}
	t.Fatal("no RestartLoopDetector in the default set — the injected clock is unreachable")
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/diagnose -run 'TestDefaultDetectors' -p 2
```

Expected: FAIL to build — `undefined: DefaultDetectors`.

- [ ] **Step 3: Write the implementation**

Create `internal/diagnose/defaults.go`:

```go
package diagnose

import "time"

// DefaultDetectors returns the detector set every kubeagent command runs, in the
// order findings are reported.
//
// now is a parameter rather than a time.Now() call because RestartLoopDetector
// measures how long ago a container last exited, and it is the only detector in
// this set that reads a clock. Injecting the instant keeps the whole set a pure
// function of its inputs, which is what the determinism property in
// FuzzDetectors and the report package's golden test both depend on.
func DefaultDetectors(now time.Time) []Detector {
	return []Detector{
		CrashLoopDetector{},
		ImagePullDetector{},
		OOMKilledDetector{},
		PendingDetector{},
		VolumeAttachDetector{},
		RestartLoopDetector{Now: now},
		InitContainerDetector{},
		ProbeFailureDetector{},
		ConfigErrorDetector{},
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./internal/diagnose -run 'TestDefaultDetectors' -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Rewire `scan` to the shared set**

In `internal/scan/scan.go`, replace this block (currently at lines 195-205):

```go
	detectors := []diagnose.Detector{
		diagnose.CrashLoopDetector{},
		diagnose.ImagePullDetector{},
		diagnose.OOMKilledDetector{},
		diagnose.PendingDetector{},
		diagnose.VolumeAttachDetector{},
		diagnose.RestartLoopDetector{Now: time.Now()},
		diagnose.InitContainerDetector{},
		diagnose.ProbeFailureDetector{},
		diagnose.ConfigErrorDetector{},
	}
```

with:

```go
	detectors := diagnose.DefaultDetectors(time.Now())
```

- [ ] **Step 6: Confirm the golden output is byte-identical and the build is clean**

```bash
go build ./... && go vet ./... && go test ./... -p 2
```

Expected: PASS everywhere, in particular `internal/report`'s `TestGoldenScanOutput`. If `time` becomes an unused import in `internal/scan/scan.go`, `go build` says so — remove it only if the file has no other use of it (check with `grep -n 'time\.' internal/scan/scan.go` before removing).

```bash
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: empty output — the golden file is untouched. Do not regenerate it.

- [ ] **Step 7: Commit**

```bash
git add internal/diagnose/defaults.go internal/diagnose/defaults_test.go internal/scan/scan.go
git commit -s -m "refactor(diagnose): name the production detector set once

DefaultDetectors(now) replaces the literal slice scan built inline. The fuzz
target added next runs the same set a real scan runs, and cannot drift out of
sync with it. now stays a parameter because RestartLoopDetector is the only
detector that reads a clock."
```

---

### Task 3: `internal/fuzzgen` — the deterministic object builder

**Files:**
- Create: `internal/fuzzgen/cursor.go`
- Create: `internal/fuzzgen/build.go`
- Create: `internal/fuzzgen/assert.go`
- Create: `internal/fuzzgen/cursor_test.go`
- Create: `internal/fuzzgen/build_test.go`
- Create: `internal/fuzzgen/imports_test.go`

**Interfaces:**
- Consumes: `safetext.MaxLine` (Task 1) is *not* imported here — `fuzzgen` deliberately knows nothing about the sanitizer, so a property written with it cannot be circular. `AssertSafe`'s rejection set is stated independently.
- Produces, all used by Tasks 4-6:
  ```go
  var  fuzzgen.Base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
  func fuzzgen.New(b []byte) *Cursor
  func (c *Cursor) Bool() bool
  func (c *Cursor) IntN(n int) int                  // 0 <= result < n; n <= 0 yields 0
  func (c *Cursor) Int32() int32                    // full range, negatives included
  func (c *Cursor) Pick(opts []string) string       // "" for an empty slice
  func (c *Cursor) Hostile(maxLen int) string       // 0..maxLen arbitrary bytes; may be invalid UTF-8
  func (c *Cursor) Name(maxLen int) string          // DNS-1123 label, 1..min(maxLen,63) chars
  func (c *Cursor) Time(base time.Time) metav1.Time // within +/-30 days of base, second resolution
  func (c *Cursor) Pod() *corev1.Pod
  func (c *Cursor) Events(pod *corev1.Pod, max int) []corev1.Event
  func (c *Cursor) TLSSecret(crt []byte) corev1.Secret
  func fuzzgen.AssertSafe(t *testing.T, where, s string)
  func fuzzgen.AssertBounded(t *testing.T, where, s string, max int)
  ```

**Two design rules a reviewer should hold this task to:**

1. **Every draw is total.** No input — empty, one byte, all `0xff` — may make any `Cursor` method panic or return out of range. The byte stream wraps, so it is effectively infinite; every caller must therefore bound its own loops with `IntN`, never `for c.Bool()`.
2. **Hostile bytes go only where the API server allows them.** Names, namespaces and container names are DNS-1123-validated, so `Name` draws from `[a-z0-9-]`. `Hostile` is for `waiting.Message`, `terminated.Reason`, condition and event `Message`, `Event.InvolvedObject.FieldPath`, log text and `tls.crt` — the fields the API server does not validate. Drawing a pod name from arbitrary bytes would make the output-safety property assert something about names no cluster can produce: coverage that looks real and is noise.

- [ ] **Step 1: Write the failing Cursor test**

Create `internal/fuzzgen/cursor_test.go`:

```go
package fuzzgen

import (
	"bytes"
	"regexp"
	"testing"
	"unicode/utf8"
)

// inputs covers the shapes a fuzzer actually hands a target: nothing, one byte,
// all zeroes, all ones, and something arbitrary.
var inputs = [][]byte{
	nil,
	{},
	{0},
	{0xff},
	bytes.Repeat([]byte{0}, 64),
	bytes.Repeat([]byte{0xff}, 64),
	[]byte("kubeagent"),
	[]byte{0x1b, 0x5b, 0x32, 0x4a, 0x00, 0xff, 0xfe},
}

func TestCursorIsTotal(t *testing.T) {
	// No input may make any draw panic. A fuzz target that panicked inside its
	// own generator would report a defect in the generator, not in kubeagent.
	for _, in := range inputs {
		c := New(in)
		_ = c.Bool()
		_ = c.IntN(0)
		_ = c.IntN(-1)
		_ = c.IntN(7)
		_ = c.Int32()
		_ = c.Pick(nil)
		_ = c.Pick([]string{"a", "b"})
		_ = c.Hostile(0)
		_ = c.Hostile(-1)
		_ = c.Hostile(32)
		_ = c.Name(0)
		_ = c.Name(-1)
		_ = c.Name(30)
		_ = c.Time(Base)
	}
}

func TestIntNStaysInRange(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 500; i++ {
			for _, n := range []int{1, 2, 3, 7, 256, 1000} {
				if got := c.IntN(n); got < 0 || got >= n {
					t.Fatalf("IntN(%d) = %d, out of range", n, got)
				}
			}
		}
		if got := c.IntN(0); got != 0 {
			t.Errorf("IntN(0) = %d, want 0", got)
		}
		if got := c.IntN(-5); got != 0 {
			t.Errorf("IntN(-5) = %d, want 0", got)
		}
	}
}

func TestHostileRespectsItsLengthCap(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 200; i++ {
			if got := c.Hostile(16); len(got) > 16 {
				t.Fatalf("Hostile(16) returned %d bytes", len(got))
			}
		}
		if got := c.Hostile(0); got != "" {
			t.Errorf("Hostile(0) = %q, want empty", got)
		}
		if got := c.Hostile(-1); got != "" {
			t.Errorf("Hostile(-1) = %q, want empty", got)
		}
	}
}

var dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestNameIsAlwaysADNS1123Label(t *testing.T) {
	// The API server validates these, so a real cluster cannot present anything
	// else. If Name could emit a hostile byte, every output-safety property
	// downstream would be testing an object no cluster can produce.
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 500; i++ {
			got := c.Name(30)
			if got == "" {
				t.Fatal("Name returned an empty string")
			}
			if len(got) > 30 {
				t.Fatalf("Name(30) = %q, %d chars", got, len(got))
			}
			if !dns1123.MatchString(got) {
				t.Fatalf("Name = %q, not a DNS-1123 label", got)
			}
		}
		if got := c.Name(0); !dns1123.MatchString(got) {
			t.Errorf("Name(0) = %q, not a DNS-1123 label", got)
		}
		if got := c.Name(500); len(got) > 63 {
			t.Errorf("Name(500) = %d chars, want at most 63 (a DNS-1123 label's limit)", len(got))
		}
	}
}

func TestHostileCanProduceInvalidUTF8(t *testing.T) {
	// The point of Hostile is that it is hostile. If 1000 draws over a byte range
	// that includes 0xff never yield invalid UTF-8, the generator is sanitizing
	// its own output and the properties downstream are asserting nothing.
	c := New(bytes.Repeat([]byte{0xff, 0xfe, 0x1b, 0x00}, 64))
	var sawInvalid, sawControl bool
	for i := 0; i < 1000; i++ {
		s := c.Hostile(8)
		if !utf8.ValidString(s) {
			sawInvalid = true
		}
		for _, b := range []byte(s) {
			if b < 0x20 {
				sawControl = true
			}
		}
	}
	if !sawInvalid {
		t.Error("Hostile never produced invalid UTF-8")
	}
	if !sawControl {
		t.Error("Hostile never produced a control byte")
	}
}

func TestCursorIsDeterministic(t *testing.T) {
	// Native fuzzing replays a crasher from its bytes. If the same bytes built a
	// different object each time, a reported crash would not reproduce.
	for _, in := range inputs {
		a, b := New(in), New(in)
		for i := 0; i < 200; i++ {
			if x, y := a.IntN(97), b.IntN(97); x != y {
				t.Fatalf("IntN diverged at draw %d: %d vs %d", i, x, y)
			}
			if x, y := a.Hostile(12), b.Hostile(12); x != y {
				t.Fatalf("Hostile diverged at draw %d: %q vs %q", i, x, y)
			}
		}
	}
}

func TestCursorWrapsShortInput(t *testing.T) {
	// One byte must still fund an arbitrarily long draw sequence: the fuzzer
	// starts small, and a generator that ran dry would silently stop varying.
	c := New([]byte{0x5a})
	for i := 0; i < 10_000; i++ {
		_ = c.IntN(31)
	}
	if got := c.Name(20); !dns1123.MatchString(got) {
		t.Errorf("after 10k draws from one byte, Name = %q", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/fuzzgen -p 2
```

Expected: FAIL to build — the package does not exist.

- [ ] **Step 3: Write the Cursor**

Create `internal/fuzzgen/cursor.go`:

```go
// Package fuzzgen builds Kubernetes API objects deterministically from a
// fuzzer's []byte, and asserts the two properties every kubeagent output field
// must hold.
//
// TEST-ONLY. This package imports "testing"; nothing outside a _test.go file
// may import it, or every kubeagent binary would carry the testing package's
// flag registrations. TestNoProductionImport in this package enforces that.
//
// Go's native fuzzing feeds []byte, string and primitives — never a struct — so
// a detector fuzz target needs a deterministic bytes-to-object builder. That is
// what Cursor is: a cursor over the fuzzer's bytes with one method per field
// shape, wrapping when it runs out so no input is too short to fund a draw.
package fuzzgen

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Base is the fixed instant every generated timestamp is drawn around. Fuzz
// targets pass it to diagnose.DefaultDetectors so a whole fuzzed run is a pure
// function of the fuzzer's bytes — no wall clock anywhere.
var Base = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// Cursor draws values from a fuzzer's input bytes. Every method is total: no
// input, however short or hostile, can make one panic or return out of range.
//
// The byte stream wraps, so it is effectively infinite. Callers must bound their
// own loops with IntN — `for c.Bool() { … }` would never terminate on an input
// of all-odd bytes.
type Cursor struct {
	b []byte
	i int
}

// New returns a Cursor over b. A nil or empty b yields a Cursor whose every draw
// reads zero, which is a legitimate object, not an error.
func New(b []byte) *Cursor { return &Cursor{b: b} }

// next returns the next input byte, wrapping to the start when exhausted and
// yielding 0 when there is no input at all.
func (c *Cursor) next() byte {
	if len(c.b) == 0 {
		return 0
	}
	v := c.b[c.i%len(c.b)]
	c.i++
	return v
}

// Bool draws a boolean from the low bit of the next byte.
func (c *Cursor) Bool() bool { return c.next()&1 == 1 }

// IntN draws an int in [0, n). n <= 0 yields 0. Two bytes are drawn, so the
// modulo bias is negligible for the small n this generator uses; Cursor is a
// generator, not a PRNG.
func (c *Cursor) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	v := int(c.next())<<8 | int(c.next())
	return v % n
}

// Int32 draws a full-range int32, negatives included.
func (c *Cursor) Int32() int32 {
	var v uint32
	for i := 0; i < 4; i++ {
		v = v<<8 | uint32(c.next())
	}
	return int32(v)
}

// Pick draws one of opts, or "" when opts is empty.
func (c *Cursor) Pick(opts []string) string {
	if len(opts) == 0 {
		return ""
	}
	return opts[c.IntN(len(opts))]
}

// Hostile draws 0..maxLen arbitrary bytes as a string. The result may carry
// control characters, ANSI escapes, or invalid UTF-8 — it stands in for the API
// fields the API server does not validate: event and condition messages,
// waiting and terminated reasons, involvedObject field paths, log text.
func (c *Cursor) Hostile(maxLen int) string {
	n := c.IntN(maxLen + 1)
	out := make([]byte, n)
	for i := range out {
		out[i] = c.next()
	}
	return string(out)
}

const nameAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789-"

// Name draws a DNS-1123 label of 1..min(maxLen, 63) characters: lowercase
// alphanumerics and dashes, never starting or ending with a dash.
//
// Object names, namespaces and container names are validated by the API server,
// so a real cluster can never present anything else here. Drawing them from
// hostile bytes would make an output-safety property assert something about
// names that cannot exist.
func (c *Cursor) Name(maxLen int) string {
	if maxLen < 1 {
		maxLen = 1
	}
	if maxLen > 63 {
		maxLen = 63
	}
	out := make([]byte, 1+c.IntN(maxLen))
	for i := range out {
		out[i] = nameAlphabet[c.IntN(len(nameAlphabet))]
	}
	if out[0] == '-' {
		out[0] = 'a'
	}
	if out[len(out)-1] == '-' {
		out[len(out)-1] = 'z'
	}
	return string(out)
}

// Time draws an instant within +/-30 days of base, at second resolution —
// metav1.Time serializes seconds, so anything finer would be noise the fuzzer
// spends its budget on.
func (c *Cursor) Time(base time.Time) metav1.Time {
	off := time.Duration(int64(c.Int32())%(30*24*3600)) * time.Second
	return metav1.NewTime(base.Add(off).Truncate(time.Second))
}
```

- [ ] **Step 4: Run the Cursor test to verify it passes**

```bash
go test ./internal/fuzzgen -run 'TestCursor|TestIntN|TestHostile|TestName' -p 2 -v
```

Expected: PASS.

- [ ] **Step 5: Write the failing builder test**

Create `internal/fuzzgen/build_test.go`:

```go
package fuzzgen

import (
	"bytes"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodIsDeterministic(t *testing.T) {
	for _, in := range inputs {
		a, b := New(in).Pod(), New(in).Pod()
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("Pod() from the same %d bytes differed", len(in))
		}
	}
}

func TestPodNamesAreValid(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 200; i++ {
			pod := c.Pod()
			for _, n := range []string{pod.Namespace, pod.Name} {
				if !dns1123.MatchString(n) {
					t.Fatalf("pod identity %q is not a DNS-1123 label", n)
				}
			}
			for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
				if !dns1123.MatchString(cs.Name) {
					t.Fatalf("container name %q is not a DNS-1123 label", cs.Name)
				}
			}
		}
	}
}

func TestPodSpecAndStatusAgreeOnContainerNames(t *testing.T) {
	// OOMKilledDetector looks a status container up in the spec to report its
	// limits. If the generator never made the two agree, that lookup would
	// always miss and the Resources path would never be fuzzed.
	c := New(bytes.Repeat([]byte("kubeagent"), 32))
	for i := 0; i < 200; i++ {
		pod := c.Pod()
		spec := map[string]bool{}
		for _, ctr := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
			spec[ctr.Name] = true
		}
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if !spec[cs.Name] {
				t.Fatalf("status container %q has no matching spec container", cs.Name)
			}
		}
	}
}

func TestPodReachesEveryDetectableState(t *testing.T) {
	// A generator that never emits CrashLoopBackOff would leave CrashLoopDetector
	// unfuzzed while the run still looked healthy. Assert the states the nine
	// detectors key on are all reachable.
	c := New(bytes.Repeat([]byte{0x00, 0x3f, 0x7f, 0xa5, 0xff, 0x11}, 64))
	seenWaiting := map[string]bool{}
	seenTerminated := map[string]bool{}
	var seenRunning, seenUnschedulable bool
	for i := 0; i < 20_000; i++ {
		pod := c.Pod()
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if w := cs.State.Waiting; w != nil {
				seenWaiting[w.Reason] = true
			}
			if cs.State.Running != nil {
				seenRunning = true
			}
			for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if term != nil {
					seenTerminated[term.Reason] = true
				}
			}
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Reason == "Unschedulable" {
				seenUnschedulable = true
			}
		}
	}
	for _, r := range []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "ContainerCreating"} {
		if !seenWaiting[r] {
			t.Errorf("no pod ever waited with reason %q", r)
		}
	}
	if !seenTerminated["OOMKilled"] {
		t.Error("no container was ever OOMKilled")
	}
	if !seenRunning {
		t.Error("no container was ever Running — RestartLoopDetector is unreachable")
	}
	if !seenUnschedulable {
		t.Error("no pod was ever Unschedulable — PendingDetector is unreachable")
	}
}

func TestEventsReachTheProbeAndAttachPaths(t *testing.T) {
	c := New(bytes.Repeat([]byte{0x00, 0x3f, 0x7f, 0xa5, 0xff, 0x11}, 64))
	seenReason := map[string]bool{}
	var seenRealFieldPath, seenProbeMessage bool
	for i := 0; i < 20_000; i++ {
		pod := c.Pod()
		for _, e := range c.Events(pod, 4) {
			seenReason[e.Reason] = true
			for _, ctr := range pod.Spec.Containers {
				if e.InvolvedObject.FieldPath == "spec.containers{"+ctr.Name+"}" {
					seenRealFieldPath = true
				}
			}
			if len(e.Message) > 8 && e.Message[:8] == "Readines" {
				seenProbeMessage = true
			}
		}
	}
	for _, r := range []string{"Unhealthy", "FailedAttachVolume"} {
		if !seenReason[r] {
			t.Errorf("no event ever had reason %q", r)
		}
	}
	if !seenRealFieldPath {
		t.Error("no event field path ever named a container the pod actually has")
	}
	if !seenProbeMessage {
		t.Error("no event ever carried a recognizable probe-failure message")
	}
}

func TestTLSSecretNeverCarriesAPrivateKey(t *testing.T) {
	// certhealth.Assess must never depend on tls.key, and this generator must
	// not tempt it to.
	c := New([]byte("seed"))
	for i := 0; i < 100; i++ {
		s := c.TLSSecret([]byte("not-a-cert"))
		if s.Type != corev1.SecretTypeTLS {
			t.Fatalf("secret type = %q, want %q", s.Type, corev1.SecretTypeTLS)
		}
		if _, ok := s.Data["tls.key"]; ok {
			t.Fatal("generated TLS secret carries a tls.key entry")
		}
		if got := string(s.Data["tls.crt"]); got != "not-a-cert" {
			t.Errorf("tls.crt = %q, want the crt passed in", got)
		}
	}
	if _, ok := c.TLSSecret(nil).Data["tls.crt"]; ok {
		t.Error("a nil crt should leave tls.crt absent, so the missing-tls.crt path is reachable")
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

```bash
go test ./internal/fuzzgen -run 'TestPod|TestEvents|TestTLSSecret' -p 2
```

Expected: FAIL to build — `Pod`, `Events`, `TLSSecret` undefined.

- [ ] **Step 7: Write the builders**

Create `internal/fuzzgen/build.go`:

```go
package fuzzgen

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The reasons real clusters emit, plus "" — a kubelet may leave a reason empty,
// and a detector keyed on a string comparison must survive that.
var (
	waitingReasons = []string{
		"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull",
		"CreateContainerConfigError", "ContainerCreating", "PodInitializing", "",
	}
	terminatedReasons = []string{"OOMKilled", "Error", "Completed", "ContainerStatusUnknown", ""}
	conditionReasons  = []string{"Unschedulable", "ContainersNotReady", "PodCompleted", ""}
	eventReasons      = []string{"Unhealthy", "FailedAttachVolume", "FailedMount", "BackOff", "FailedScheduling", "Killing", ""}
	podPhases         = []corev1.PodPhase{
		corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed, corev1.PodUnknown, "",
	}
	probeMessagePrefixes = []string{
		"Readiness probe failed: ",
		"Liveness probe failed: ",
		"Startup probe failed: ",
		"Readiness probe failed: HTTP probe failed with statuscode: 503 ",
		"Liveness probe failed: dial tcp 192.0.2.10:8080: connect: connection refused ",
	}
)

// Pod builds a pod whose identity is DNS-1123-valid and whose unvalidated fields
// are hostile: 1-3 container statuses with matching spec containers, 0-2 init
// container statuses, 0-3 conditions, and a phase.
func (c *Cursor) Pod() *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: c.Name(20),
			Name:      c.Name(30),
		},
		Status: corev1.PodStatus{Phase: podPhases[c.IntN(len(podPhases))]},
	}
	pod.Status.ContainerStatuses = c.containerStatuses(1 + c.IntN(3))
	pod.Status.InitContainerStatuses = c.containerStatuses(c.IntN(3))
	for _, cs := range pod.Status.ContainerStatuses {
		pod.Spec.Containers = append(pod.Spec.Containers, c.container(cs.Name))
	}
	for _, cs := range pod.Status.InitContainerStatuses {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, c.container(cs.Name))
	}
	for n := c.IntN(4); n > 0; n-- {
		pod.Status.Conditions = append(pod.Status.Conditions, c.condition())
	}
	if c.Bool() {
		pod.Spec.NodeName = c.Name(20)
	}
	if c.Bool() {
		t := c.Time(Base)
		pod.DeletionTimestamp = &t
	}
	return pod
}

// containerStatuses builds n statuses. Reason strings are drawn from the real
// vocabulary so the detectors' string comparisons match; Message is hostile,
// because the API server does not validate it.
func (c *Cursor) containerStatuses(n int) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for i := 0; i < n; i++ {
		cs := corev1.ContainerStatus{
			Name:         c.Name(20),
			RestartCount: c.Int32(),
		}
		switch c.IntN(3) {
		case 0:
			cs.State.Waiting = &corev1.ContainerStateWaiting{
				Reason:  c.Pick(waitingReasons),
				Message: c.Hostile(96),
			}
		case 1:
			cs.State.Running = &corev1.ContainerStateRunning{StartedAt: c.Time(Base)}
		case 2:
			cs.State.Terminated = c.terminated()
		}
		if c.Bool() {
			cs.LastTerminationState.Terminated = c.terminated()
		}
		out = append(out, cs)
	}
	return out
}

func (c *Cursor) terminated() *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		Reason:     c.Pick(terminatedReasons),
		Message:    c.Hostile(96),
		ExitCode:   c.Int32(),
		StartedAt:  c.Time(Base),
		FinishedAt: c.Time(Base),
	}
}

func (c *Cursor) condition() corev1.PodCondition {
	types := []corev1.PodConditionType{
		corev1.PodScheduled, corev1.PodReady, corev1.PodInitialized, corev1.ContainersReady,
	}
	statuses := []corev1.ConditionStatus{corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionUnknown, ""}
	return corev1.PodCondition{
		Type:    types[c.IntN(len(types))],
		Status:  statuses[c.IntN(len(statuses))],
		Reason:  c.Pick(conditionReasons),
		Message: c.Hostile(96),
	}
}

// container builds the spec entry for a status container, sometimes with a
// memory limit so the OOMKilled finding's Resources path is reachable.
func (c *Cursor) container(name string) corev1.Container {
	ctr := corev1.Container{Name: name, Image: c.Name(20) + ":" + c.Name(8)}
	if c.Bool() {
		ctr.LivenessProbe = &corev1.Probe{}
	}
	if c.Bool() {
		ctr.ReadinessProbe = &corev1.Probe{}
	}
	if c.Bool() {
		ctr.Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(int64(c.IntN(4096)+1)*1024*1024, resource.BinarySI),
		}
	}
	return ctr
}

// Events builds 0..max events for pod. Reason is drawn from the real vocabulary
// so the detectors' filters match; Message and FieldPath are hostile, because
// the API server validates neither and ProbeFailureDetector parses both.
func (c *Cursor) Events(pod *corev1.Pod, max int) []corev1.Event {
	var out []corev1.Event
	for n := c.IntN(max + 1); n > 0; n-- {
		e := corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: pod.Namespace, Name: c.Name(20)},
			Reason:        c.Pick(eventReasons),
			LastTimestamp: c.Time(Base),
			InvolvedObject: corev1.ObjectReference{
				Kind:      "Pod",
				Namespace: pod.Namespace,
				Name:      pod.Name,
			},
		}
		switch c.IntN(3) {
		case 0:
			e.Message = c.Hostile(96)
		case 1:
			e.Message = c.Pick(probeMessagePrefixes) + c.Hostile(48)
		case 2:
			e.Message = "Multi-Attach error for volume " + c.Name(20) + " " + c.Hostile(48)
		}
		switch {
		case c.Bool() && len(pod.Spec.Containers) > 0:
			e.InvolvedObject.FieldPath = "spec.containers{" + pod.Spec.Containers[c.IntN(len(pod.Spec.Containers))].Name + "}"
		case c.Bool():
			e.InvolvedObject.FieldPath = "spec.containers{" + c.Hostile(24) + "}"
		default:
			e.InvolvedObject.FieldPath = c.Hostile(24)
		}
		out = append(out, e)
	}
	return out
}

// TLSSecret wraps crt in a kubernetes.io/tls Secret. Deliberately NO tls.key
// entry: certhealth.Assess parses only the public certificate and must never
// depend on the private key. A nil or empty crt leaves tls.crt absent, which is
// the shape that reaches the "missing tls.crt" branch.
func (c *Cursor) TLSSecret(crt []byte) corev1.Secret {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: c.Name(20), Name: c.Name(30)},
		Type:       corev1.SecretTypeTLS,
	}
	if len(crt) > 0 {
		s.Data = map[string][]byte{"tls.crt": crt}
	}
	return s
}
```

- [ ] **Step 8: Run the builder test to verify it passes**

```bash
go test ./internal/fuzzgen -p 2 -v
```

Expected: PASS. If `TestPodReachesEveryDetectableState` or `TestEventsReachTheProbeAndAttachPaths` reports an unreachable state, the generator is wrong — fix the generator, do not relax the assertion. These tests are the only thing standing between a real fuzz campaign and one that explores nothing.

- [ ] **Step 9: Write the assertions**

Create `internal/fuzzgen/assert.go`:

```go
package fuzzgen

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

// AssertSafe fails the test when s carries anything that must never reach an
// operator's terminal: invalid UTF-8, a control character, a Unicode formatting
// character, or a Unicode line separator.
//
// The rejection set is stated here independently of internal/safetext, on
// purpose: a property written in terms of the sanitizer's own definition of
// "safe" would be circular and would pass no matter what the sanitizer did.
//
// Length is deliberately NOT checked here. A detector composes
// fmt.Sprintf("container %q: %s", name, safetext.Line(msg)), so the composed
// field legitimately exceeds one line's budget while every untrusted part is
// bounded. Folding length into this assertion would fail on every composed
// field, and the natural response — raising the limit until nothing fails —
// tests nothing. Use AssertBounded for the parts.
func AssertSafe(t *testing.T, where, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s: invalid UTF-8 in %q", where, s)
		return
	}
	for i, r := range s {
		switch {
		case r == '\u2028' || r == '\u2029':
			t.Errorf("%s: Unicode line separator %U at byte %d in %q", where, r, i, s)
		case unicode.IsControl(r):
			t.Errorf("%s: control character %U at byte %d in %q", where, r, i, s)
		case unicode.Is(unicode.Cf, r):
			// U+202E RIGHT-TO-LEFT OVERRIDE and friends are category Cf, not Cc,
			// so unicode.IsControl does not catch them. They reorder everything
			// printed after them.
			t.Errorf("%s: Unicode formatting character %U at byte %d in %q", where, r, i, s)
		}
	}
}

// AssertBounded fails the test when s is longer than max runes. Runes, not
// bytes: a 512-rune line of CJK is over 1500 bytes and perfectly reasonable.
func AssertBounded(t *testing.T, where, s string, max int) {
	t.Helper()
	if n := utf8.RuneCountInString(s); n > max {
		t.Errorf("%s: %d runes exceeds the %d-rune budget", where, n, max)
	}
}
```

- [ ] **Step 10: Write the test-only import guard**

Create `internal/fuzzgen/imports_test.go`:

```go
package fuzzgen

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoProductionImport keeps fuzzgen out of the shipped binary. It generates
// hostile Kubernetes objects and imports "testing"; if any non-test file
// imported it, every kubeagent binary would carry the testing package's flag
// registrations and its init-time cost.
//
// go/parser with ImportsOnly is the whole implementation — no new dependency,
// and it reads import lists without type-checking the tree.
func TestNoProductionImport(t *testing.T) {
	const self = "github.com/imantaba/kubeagent/internal/fuzzgen"
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".github", "website", "chaos", "docs", "deploy":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if p == self {
				t.Errorf("%s imports %s from a non-test file — fuzzgen is test-only", path, self)
			}
			if strings.HasSuffix(p, "/internal/remediate") || strings.HasSuffix(p, "/internal/explain") {
				if strings.Contains(filepath.ToSlash(path), "/internal/fuzzgen/") {
					t.Errorf("%s imports %s — fuzzgen must never reach a write or a model call", path, p)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}
```

- [ ] **Step 11: Run the whole package and the whole suite**

```bash
go test ./internal/fuzzgen -p 2 -v
go build ./... && go vet ./... && go test ./... -p 2
```

Expected: PASS everywhere.

- [ ] **Step 12: Commit**

```bash
git add internal/fuzzgen
git commit -s -m "test(fuzzgen): deterministic hostile-object builder for fuzz targets

Go's native fuzzing feeds []byte and primitives, never a struct, so a detector
fuzz target needs a bytes-to-object builder. Cursor is total and wrapping: no
input can panic a draw, and one byte funds an arbitrarily long sequence.

Names, namespaces and container names draw from a DNS-1123 alphabet because the
API server validates them; only genuinely unvalidated fields — messages,
reasons, field paths, tls.crt — get hostile bytes. Fuzzing a pod name no cluster
can produce would be coverage that looks real and is noise.

AssertSafe states its rejection set independently of internal/safetext so the
property cannot be circular, and leaves length to AssertBounded so a composed
field is not judged by one untrusted part's budget."
```

---

### Task 4: `FuzzDetectors` and the detector ingress fixes

**Files:**
- Create: `internal/diagnose/fuzz_test.go`
- Modify: `internal/diagnose/imagepull.go:16`, `internal/diagnose/configerror.go:28`, `internal/diagnose/initcontainer.go:41`, `internal/diagnose/pending.go:18`, `internal/diagnose/volumeattach.go:31`, `internal/diagnose/restartloop.go:41`, `internal/diagnose/probefailure.go:47`

**Interfaces:**
- Consumes: `safetext.MaxLine`, `safetext.Line` (Task 1); `diagnose.DefaultDetectors` (Task 2); `fuzzgen.New`, `fuzzgen.Base`, `Cursor.Pod`, `Cursor.Events`, `fuzzgen.AssertSafe`, `fuzzgen.AssertBounded` (Task 3).
- Produces: nothing new for later tasks.

**The eighth site.** The spec's defect-2 table lists seven ingress points. Writing this plan turned up an eighth: `probefailure.go` sets `Container: container`, where `container` comes from `containerFromFieldPath(ev.InvolvedObject.FieldPath)` — the substring between the first `{` and `}` of a field path the API server does not validate. `probeEvidence` interpolates it with `%q`, which escapes it, so `Evidence` was already safe; the `Container` field itself is raw. `AssertSafe` on `Finding.Container` catches it. Sanitizing it is correct and harmless: the lookup in `containerRunning` runs on the raw value before the assignment, and a real cluster's field path already carries a DNS-1123 name, so `Line` is a no-op on every real input.

**What is deliberately not changed.** `Finding.Issue` and `Finding.Reason` hold fixed literals at every site — `imagepull.go` and `initcontainer.go` are both guarded by `w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull"`, so `Issue` can only ever be one of those two strings or `"Init:"` prefixed to one. `Finding.Pod` is `namespace + "/" + name`, both DNS-1123-validated. Container names interpolated with `%q` are already escaped by `fmt`. The properties assert all of these anyway — the point is that the assertions hold today for those fields, and it is worth knowing if that ever changes.

- [ ] **Step 1: Write the fuzz target**

Create `internal/diagnose/fuzz_test.go`:

```go
package diagnose

import (
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// evidenceBudget is the ceiling a composed Evidence field must stay under. The
// untrusted part is bounded by safetext.MaxLine; the extra 256 runes are the
// budget for kubeagent's own fixed prefix and a DNS-1123 container name (63
// characters, plus the quotes fmt %q adds). This catches unbounded growth — a
// field that carried a whole log or a whole event message — not an off-by-a-few.
const evidenceBudget = safetext.MaxLine + 256

// FuzzDetectors asserts four properties of the production detector set over
// arbitrary pods and events:
//
//	no panic     — Run returns for every input
//	purity       — the facts handed in are not mutated
//	determinism  — the same input yields the same findings
//	output safe  — every string field is printable and bounded
//
// The pod's identity is DNS-1123-valid because the API server validates it; the
// fields it does not validate (messages, reasons, field paths) are hostile. See
// internal/fuzzgen.
func FuzzDetectors(f *testing.F) {
	f.Add([]byte("crashloop"))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("Readiness probe failed: HTTP probe failed with statuscode: 503"))
	f.Add([]byte("Multi-Attach error for volume pvc-0"))
	f.Add([]byte("\u202egnp.txt.exe"))

	f.Fuzz(func(t *testing.T, in []byte) {
		c := fuzzgen.New(in)
		pod := c.Pod()
		events := c.Events(pod, 4)
		facts := PodFacts{Pod: pod, Events: events}

		podBefore := pod.DeepCopy()
		eventsBefore := make([]corev1.Event, len(events))
		for i := range events {
			eventsBefore[i] = *events[i].DeepCopy()
		}

		findings := Run(DefaultDetectors(fuzzgen.Base), []PodFacts{facts})

		if !reflect.DeepEqual(pod, podBefore) {
			t.Errorf("a detector mutated the pod it was handed; detectors must be pure")
		}
		if !reflect.DeepEqual(events, eventsBefore) {
			t.Errorf("a detector mutated the events it was handed; detectors must be pure")
		}

		again := Run(DefaultDetectors(fuzzgen.Base), []PodFacts{facts})
		if !reflect.DeepEqual(findings, again) {
			t.Errorf("the detector set is not deterministic:\nfirst:  %+v\nsecond: %+v", findings, again)
		}

		for i, fd := range findings {
			where := fmt.Sprintf("finding[%d]", i)
			fuzzgen.AssertSafe(t, where+".pod", fd.Pod)
			fuzzgen.AssertSafe(t, where+".issue", fd.Issue)
			fuzzgen.AssertSafe(t, where+".reason", fd.Reason)
			fuzzgen.AssertSafe(t, where+".evidence", fd.Evidence)
			fuzzgen.AssertSafe(t, where+".container", fd.Container)
			fuzzgen.AssertBounded(t, where+".evidence", fd.Evidence, evidenceBudget)
			fuzzgen.AssertBounded(t, where+".container", fd.Container, safetext.MaxLine)
			if fd.Resources != nil {
				fuzzgen.AssertSafe(t, where+".resources.container", fd.Resources.Container)
				fuzzgen.AssertSafe(t, where+".resources.memLimit", fd.Resources.MemLimit)
				fuzzgen.AssertSafe(t, where+".resources.cpuLimit", fd.Resources.CPULimit)
			}
		}
	})
}
```

- [ ] **Step 2: Watch the property fail on the seed corpus alone**

The seeds replay on a plain `go test` with no fuzzing budget. That must already be enough to fail.

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/diagnose -run 'FuzzDetectors' -p 2 -v
```

Expected: FAIL, with `AssertSafe` errors naming control characters or invalid UTF-8 in `evidence` (and possibly `container`). Record the exact failure lines in the task report — this is the "watched failing" evidence the plan requires.

If it passes, the generator is not reaching the ingress sites. Do not proceed. Diagnose by fuzzing briefly:

```bash
go test ./internal/diagnose -run '^$' -fuzz '^FuzzDetectors$' -fuzztime 30s
```

- [ ] **Step 3: Sanitize `imagepull.go`**

In `internal/diagnose/imagepull.go`, add the import `"github.com/imantaba/kubeagent/internal/safetext"` and change:

```go
				Evidence: fmt.Sprintf("container %q: %s", cs.Name, w.Message),
```

to:

```go
				Evidence: fmt.Sprintf("container %q: %s", cs.Name, safetext.Line(w.Message)),
```

- [ ] **Step 4: Sanitize `configerror.go`**

In `internal/diagnose/configerror.go`, add the `safetext` import and change:

```go
				Evidence:  fmt.Sprintf("%s %q: %s", kind, cs.Name, w.Message),
```

to:

```go
				Evidence:  fmt.Sprintf("%s %q: %s", kind, cs.Name, safetext.Line(w.Message)),
```

- [ ] **Step 5: Sanitize `initcontainer.go`**

In `internal/diagnose/initcontainer.go`, add the `safetext` import and change:

```go
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, w.Message),
```

to:

```go
			Evidence:  fmt.Sprintf("init container %q %s: %s", cs.Name, pos, safetext.Line(w.Message)),
```

- [ ] **Step 6: Sanitize `pending.go`**

In `internal/diagnose/pending.go`, add the `safetext` import and change:

```go
				Evidence: c.Message,
```

to:

```go
				Evidence: safetext.Line(c.Message),
```

- [ ] **Step 7: Sanitize `volumeattach.go`**

In `internal/diagnose/volumeattach.go`, add the `safetext` import and change:

```go
		Evidence: ev.Message,
```

to:

```go
		Evidence: safetext.Line(ev.Message),
```

Leave the `strings.Contains(ev.Message, "Multi-Attach")` test above it on the **raw** message: that is a matching decision, not output, and sanitizing before matching would let a hostile message evade the Multi-Attach branch by inserting a control character mid-word.

- [ ] **Step 8: Sanitize `restartloop.go`**

In `internal/diagnose/restartloop.go`, add the `safetext` import and change:

```go
			Evidence:  fmt.Sprintf("container %q, %d restarts, last exit %d (%s), %s ago", cs.Name, cs.RestartCount, term.ExitCode, term.Reason, age),
```

to:

```go
			Evidence:  fmt.Sprintf("container %q, %d restarts, last exit %d (%s), %s ago", cs.Name, cs.RestartCount, term.ExitCode, safetext.Line(term.Reason), age),
```

- [ ] **Step 9: Sanitize `probefailure.go`'s `Container` field**

In `internal/diagnose/probefailure.go`, add the `safetext` import and change:

```go
		Container: container,
```

to:

```go
		// containerFromFieldPath returns a substring of an unvalidated field
		// path. probeEvidence escapes it with %q, but this field is raw, and it
		// reaches JSON, SARIF and the TUI. A real cluster's field path already
		// carries a DNS-1123 name, so this is a no-op on every real input.
		Container: safetext.Line(container),
```

- [ ] **Step 10: Run the property again — it must now pass**

```bash
go test ./internal/diagnose -run 'FuzzDetectors' -p 2 -v
go test ./internal/diagnose -p 2
```

Expected: PASS.

- [ ] **Step 11: Fuzz for real, briefly**

```bash
go test ./internal/diagnose -run '^$' -fuzz '^FuzzDetectors$' -fuzztime 90s
```

Expected: no failures, and no new files under `internal/diagnose/testdata/fuzz/`. If Go writes a crasher there, it is a real finding: read it, fix the cause, and **commit the crasher file** — it becomes a permanent regression seed.

- [ ] **Step 12: Mutation-check the fix**

Revert one sanitization (for example `pending.go` back to `Evidence: c.Message`), confirm the property fails again, then restore it. This proves the property is load-bearing and not passing for an unrelated reason.

```bash
# after reverting pending.go by hand
go test ./internal/diagnose -run 'FuzzDetectors' -p 2   # expected: FAIL
# restore the fix
go test ./internal/diagnose -run 'FuzzDetectors' -p 2   # expected: PASS
```

Record both outcomes in the task report.

- [ ] **Step 13: Confirm the golden output is untouched**

```bash
go build ./... && go vet ./... && go test ./... -p 2
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: all tests PASS and the golden diff is empty. Every message in the golden fixture is clean text, so `Line` is a no-op on it.

- [ ] **Step 14: Commit**

```bash
git add internal/diagnose
git commit -s -m "fix(diagnose): sanitize unvalidated API text at every ingress point

waiting.Message, terminated.Reason, condition and event messages, and the
container name parsed out of an event field path all reached Finding fields raw.
The API server validates none of them, so a pod spec an unprivileged user can
create could put ANSI escapes, a right-to-left override, or invalid UTF-8 into
an operator's terminal — using the same escapes kubeagent's own TUI uses to
switch screens.

Eight sites now pass through safetext.Line. FuzzDetectors asserts no panic,
purity, determinism and output safety over arbitrary pods and events, and fails
on the seed corpus alone without these fixes.

The Multi-Attach match in volumeattach stays on the raw message: sanitizing
before matching would let a control character mid-word evade the branch."
```

---

### Task 5: `FuzzClassify`, `FuzzRedactURL`, `FuzzRedactError`

**Files:**
- Create: `internal/logscan/fuzz_test.go`
- Create: `internal/redact/fuzz_test.go`
- Modify: `internal/logscan/logscan.go:31` (the conn-refused cause), `internal/logscan/logscan.go:55,60,66-71` (`truncate` becomes `sanitize`)

**Interfaces:**
- Consumes: `safetext.Line`, `safetext.MaxLine` (Task 1); `fuzzgen.AssertSafe`, `fuzzgen.AssertBounded` (Task 3).
- Produces: nothing new for later tasks.

**Why logscan is the most important target in this plan.** The tail of a crashed container's log is the one input an unprivileged attacker controls outright — no RBAC needed, just a container that prints bytes and exits. It reaches an operator's terminal through `--logs`. Two fields carry it: `Clue.Excerpt` (the matched line, rune-truncated but never filtered) and `Clue.Cause`, whose conn-refused branch splices in `m[1]`, a `\S+` capture from the log. `\S` excludes only `[\t\n\f\r ]`, so that capture can carry ESC, NUL, and invalid UTF-8.

**Sanitization order.** `Line` runs **before** the 200-rune bound, not after. Sanitizing after truncation would spend the excerpt budget on characters about to be dropped, and would leave a control character inside the kept prefix untouched.

- [ ] **Step 1: Write the logscan fuzz target**

Create `internal/logscan/fuzz_test.go`:

```go
package logscan

import (
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

// FuzzClassify fuzzes the one input an unprivileged attacker controls outright:
// the tail of a crashed container's own log. Both Clue fields that carry log
// text must come back printable and bounded, and classification must be
// deterministic.
func FuzzClassify(f *testing.F) {
	f.Add("")
	f.Add("panic: runtime error: invalid memory address or nil pointer dereference")
	f.Add("dial tcp 192.0.2.10:5432: connect: connection refused")
	f.Add("exec: \"/app/server\": permission denied")
	f.Add("\x1b[2J\x1b[H")
	f.Add("dial tcp \x1b]0;pwned\x07: connect: connection refused")
	f.Add("dial tcp \xff\xfe: connect: connection refused")
	f.Add("yaml: line 3: found character that cannot start any token\n\u202e")
	f.Add("\n\n   \n")

	f.Fuzz(func(t *testing.T, log string) {
		clue := Classify(log)

		fuzzgen.AssertSafe(t, "clue.signature", clue.Signature)
		fuzzgen.AssertSafe(t, "clue.excerpt", clue.Excerpt)
		fuzzgen.AssertSafe(t, "clue.cause", clue.Cause)

		// maxExcerpt + 1: the ellipsis truncate appends when it cuts.
		fuzzgen.AssertBounded(t, "clue.excerpt", clue.Excerpt, maxExcerpt+1)
		// The cause is a fixed sentence plus, in the conn-refused case, one
		// sanitized capture from the log.
		fuzzgen.AssertBounded(t, "clue.cause", clue.Cause, 1024)

		if again := Classify(log); again != clue {
			t.Errorf("Classify is not deterministic:\nfirst:  %+v\nsecond: %+v", clue, again)
		}
	})
}
```

- [ ] **Step 2: Watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/logscan -run 'FuzzClassify' -p 2 -v
```

Expected: FAIL, with `AssertSafe` errors on `clue.excerpt` (the ANSI and invalid-UTF-8 seeds) and on `clue.cause` (the `dial tcp \x1b]0;pwned\x07` seed). Record both.

- [ ] **Step 3: Sanitize logscan**

In `internal/logscan/logscan.go`:

Add the import:

```go
import (
	"regexp"
	"strings"

	"github.com/imantaba/kubeagent/internal/safetext"
)
```

Change the conn-refused signature's cause builder from:

```go
	{"conn-refused", regexp.MustCompile(`(?i)dial tcp (\S+): connect: connection refused`), func(m []string) string { return "cannot reach a dependency (" + m[1] + ") — connection refused" }},
```

to:

```go
	// m[1] is a \S+ capture from the container's own log. \S excludes only
	// whitespace, so it can carry ESC, NUL, or invalid UTF-8 — sanitize it.
	{"conn-refused", regexp.MustCompile(`(?i)dial tcp (\S+): connect: connection refused`), func(m []string) string {
		return "cannot reach a dependency (" + safetext.Line(m[1]) + ") — connection refused"
	}},
```

Rename `truncate` to `sanitize` and make it sanitize before it truncates:

```go
// sanitize makes one log line fit to print and bounds it to maxExcerpt runes.
// safetext.Line runs FIRST: sanitizing after truncation would spend the excerpt
// budget on characters about to be dropped, and would leave a control character
// inside the kept prefix untouched.
func sanitize(s string) string {
	s = safetext.Line(s)
	if r := []rune(s); len(r) > maxExcerpt {
		return string(r[:maxExcerpt]) + "…"
	}
	return s
}
```

Update both call sites in `Classify`:

```go
			if m := s.re.FindStringSubmatch(ln); m != nil {
				return Clue{Signature: s.name, Excerpt: sanitize(ln), Cause: s.cause(m)}
			}
```

```go
		if ln := strings.TrimSpace(lines[i]); ln != "" {
			return Clue{Excerpt: sanitize(ln), Cause: "last output before exit (no known signature)"}
		}
```

Regex matching still runs on the **raw** line, for the same reason as `volumeattach`: a control character spliced mid-word must not let a log evade a signature.

- [ ] **Step 4: Run it — must pass**

```bash
go test ./internal/logscan -p 2 -v
go test ./internal/logscan -run '^$' -fuzz '^FuzzClassify$' -fuzztime 90s
```

Expected: PASS, and no new `internal/logscan/testdata/fuzz/` files.

- [ ] **Step 5: Mutation-check**

Revert `sanitize` to the old `truncate` body (no `safetext.Line`), confirm `go test ./internal/logscan -run 'FuzzClassify' -p 2` FAILS, then restore.

- [ ] **Step 6: Commit logscan**

```bash
git add internal/logscan
git commit -s -m "fix(logscan): sanitize the crash-log excerpt and the dependency capture

A crashed container's log tail is the one input an unprivileged attacker
controls outright, and --logs puts it on an operator's terminal. Clue.Excerpt
was rune-truncated but never filtered, and the conn-refused cause spliced in a
\\S+ capture from the log — \\S excludes only whitespace, so ESC, NUL and invalid
UTF-8 all passed through.

safetext.Line now runs before the 200-rune bound, so the budget is not spent on
characters about to be dropped. Signature matching still runs on the raw line: a
control character spliced mid-word must not let a log evade a signature."
```

- [ ] **Step 7: Write the redact fuzz targets**

Create `internal/redact/fuzz_test.go`:

```go
package redact

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// FuzzRedactURL asserts the whole contract by exact equality: whatever URL
// returns for a parseable input is scheme://host and nothing else — no path, no
// query, no fragment, no userinfo.
//
// Exact equality, not containment. A containment check ("the output must not
// contain the path") fails constantly on fuzzed input, because a one-character
// path is a substring of almost any output, and a check that only rejects long
// paths silently stops testing short ones.
func FuzzRedactURL(f *testing.F) {
	f.Add("https://api.example/v1/messages?key=REDACTED-LOOKING-TOKEN")
	f.Add("https://hooks.example/services/T000/B000/xxxxxxxx")
	f.Add("https://user:pw@host.example/path#frag")
	f.Add("")
	f.Add("not a url")
	f.Add("://")
	f.Add("http://[::1]:8080/x")
	f.Add("\x1b[2Jhttps://host.example/")

	f.Fuzz(func(t *testing.T, raw string) {
		got := URL(raw)
		if got == "(redacted)" {
			return // the safe fallback is always acceptable
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("URL(%q) = %q, but the input does not parse: %v", raw, got, err)
		}
		if want := u.Scheme + "://" + u.Host; got != want {
			t.Errorf("URL(%q) = %q, want %q", raw, got, want)
		}
		if strings.Contains(got, "@") {
			t.Errorf("URL(%q) = %q — userinfo survived redaction", raw, got)
		}
	})
}

// FuzzRedactError asserts that walking a *url.Error chain redacts the URL at
// every level while keeping the operation and the cause, again by exact
// equality against what URL itself returns.
func FuzzRedactError(f *testing.F) {
	f.Add("https://api.example/v1/messages?key=REDACTED-LOOKING-TOKEN")
	f.Add("")
	f.Add("not a url")
	f.Add("https://user:pw@host.example/path")

	f.Fuzz(func(t *testing.T, raw string) {
		err := &url.Error{Op: "Post", URL: raw, Err: errors.New("boom")}
		want := "Post " + URL(raw) + ": boom"
		if got := Error(err); got != want {
			t.Errorf("Error(%q) = %q, want %q", raw, got, want)
		}

		// A nested *url.Error must be redacted at both levels.
		nested := &url.Error{Op: "Get", URL: raw, Err: err}
		wantNested := "Get " + URL(raw) + ": " + want
		if got := Error(nested); got != wantNested {
			t.Errorf("Error(nested %q) = %q, want %q", raw, got, wantNested)
		}
	})
}
```

- [ ] **Step 8: Run the redact targets**

```bash
go test ./internal/redact -p 2 -v
go test ./internal/redact -run '^$' -fuzz '^FuzzRedactURL$' -fuzztime 60s
go test ./internal/redact -run '^$' -fuzz '^FuzzRedactError$' -fuzztime 60s
```

Expected: PASS. These two are the one place in this plan where the property may pass on first write — `redact` is already correct, and the target's job is to keep it that way. If either finds a crasher, commit the crasher file and fix `redact`.

- [ ] **Step 9: Commit redact**

```bash
git add internal/redact
git commit -s -m "test(redact): fuzz URL and Error against exact expected output

URLs are credentials: a webhook URL is a bearer token in path form. Both targets
assert by exact equality rather than containment — a containment check on a
fuzzed URL fails constantly, since a one-character path is a substring of almost
any output, and a check that only rejects long paths silently stops testing
short ones."
```

---

### Task 6: `FuzzParseResponses`, `FuzzParseReadyz`, `FuzzCertAssess` and their fixes

**Files:**
- Create: `internal/dnshealth/fuzz_test.go`
- Create: `internal/controlplane/fuzz_test.go`
- Create: `internal/certhealth/fuzz_test.go`
- Modify: `internal/dnshealth/dnshealth.go` (`ParseResponses` and `Assess`)
- Modify: `internal/controlplane/controlplane.go` (`failedChecks`)
- Modify: `internal/certhealth/certhealth.go:73-76` (the CommonName / SAN fallback)

**Interfaces:**
- Consumes: `safetext.Line`, `safetext.MaxLine` (Task 1); `fuzzgen.New`, `Cursor.IntN`, `Cursor.TLSSecret`, `fuzzgen.AssertSafe`, `fuzzgen.AssertBounded` (Task 3).
- Produces: nothing new for later tasks.

**Defect 1, restated.** `strconv.ParseFloat` accepts `"NaN"`, `"+Inf"` and `"-Inf"` without error, and converting a non-finite or out-of-range float to `int64` is implementation-defined in Go — empirically `-9223372036854775808` on this platform for all of `NaN`, `+Inf`, `-Inf` and `1e30`. A CoreDNS exporter (or anything answering on that port) that reports `NaN` therefore turns a DNS response count negative, and `Assess`'s ratio goes with it.

**The certhealth site.** `cert.Subject.CommonName` and `cert.DNSNames[0]` come out of a parsed X.509 certificate. Any user who can create a `kubernetes.io/tls` Secret controls those bytes, and X.509 string types do not exclude control characters. `Cert.CommonName` is rendered in the text report, the JSON, the HTML report and the TUI.

- [ ] **Step 1: Write the dnshealth fuzz target**

Create `internal/dnshealth/fuzz_test.go`:

```go
package dnshealth

import (
	"math"
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
)

// FuzzParseResponses fuzzes both halves of the CoreDNS metrics path: parsing a
// /metrics body, and judging the parsed counts. The second []byte drives the
// Assess parameters through a Cursor, so one target covers the pair without
// needing seven fuzz arguments.
func FuzzParseResponses(f *testing.F) {
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} 42`), []byte("seed"))
	f.Add([]byte(`coredns_dns_responses_total{rcode="SERVFAIL"} NaN`), []byte{})
	f.Add([]byte(`coredns_dns_responses_total{rcode="REFUSED"} +Inf`), []byte{1})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} -Inf`), []byte{2})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} 1e30`), []byte{3})
	f.Add([]byte(`coredns_dns_responses_total{rcode="NOERROR"} -5`), []byte{4})
	f.Add([]byte("coredns_dns_responses_total{rcode=\"NOERROR\"} 9223372036854775807\ncoredns_dns_responses_total{rcode=\"NOERROR\"} 9223372036854775807"), []byte{5})
	f.Add([]byte(`coredns_dns_response_rcode_count_total{rcode="SERVFAIL"} 7`), []byte{6})
	f.Add([]byte{}, []byte{})

	f.Fuzz(func(t *testing.T, body, params []byte) {
		agg := ParseResponses(body)
		for rcode, n := range agg {
			if n < 0 {
				t.Errorf("ParseResponses: rcode %q has a negative count %d — a count cannot be negative", rcode, n)
			}
		}

		c := fuzzgen.New(params)
		rep := Assess(agg, c.IntN(8), c.IntN(4), c.IntN(4), float64(c.IntN(101))/100, int64(c.IntN(1000)))

		switch rep.Status {
		case "ok", "degraded", "forbidden", "unreachable", "":
		default:
			t.Errorf("Assess: status %q is outside the documented set", rep.Status)
		}
		if math.IsNaN(rep.ServfailRatio) || math.IsInf(rep.ServfailRatio, 0) {
			t.Errorf("Assess: ServfailRatio = %v", rep.ServfailRatio)
		}
		if rep.ServfailRatio < 0 || rep.ServfailRatio > 1 {
			t.Errorf("Assess: ServfailRatio = %v, outside [0,1]", rep.ServfailRatio)
		}
		if rep.ErrorResponses < 0 || rep.TotalResponses < 0 {
			t.Errorf("Assess: negative counts (errors=%d total=%d)", rep.ErrorResponses, rep.TotalResponses)
		}
		if rep.ErrorResponses > rep.TotalResponses {
			t.Errorf("Assess: errors %d exceed total %d", rep.ErrorResponses, rep.TotalResponses)
		}
		fuzzgen.AssertSafe(t, "report.detail", rep.Detail)
	})
}
```

- [ ] **Step 2: Watch it fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/dnshealth -run 'FuzzParseResponses' -p 2 -v
```

Expected: FAIL — the `NaN`, `+Inf`, `-Inf` and `1e30` seeds each yield a negative count, and the saturating-sum seed overflows `total`. Record the failures.

- [ ] **Step 3: Harden `ParseResponses`**

In `internal/dnshealth/dnshealth.go`, add `"math"` to the imports and, above `ParseResponses`, add:

```go
// maxCount bounds one parsed sample. Prometheus counters are float64; a value
// beyond 2^53 is either a broken exporter or a hostile one, and clamping keeps
// the int64 conversion defined — converting a float outside int64's range is
// implementation-defined in Go and yields math.MinInt64 on amd64, turning a huge
// count into a negative one.
const maxCount = int64(1) << 53

// saturatingAdd returns a+b clamped to math.MaxInt64 rather than wrapping
// negative. Both arguments are non-negative at every call site.
func saturatingAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
```

Then replace the parse-and-accumulate block:

```go
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[rcode] += int64(v)
```

with:

```go
		v, err := strconv.ParseFloat(fields[0], 64)
		// ParseFloat accepts "NaN", "+Inf" and "-Inf" without error, and a
		// negative counter is not a counter. Converting any of them to int64 is
		// implementation-defined and yields math.MinInt64 here, which would turn
		// a DNS response count negative and drag the error ratio with it.
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			continue
		}
		n := int64(v)
		if v > float64(maxCount) {
			n = maxCount
		}
		out[rcode] = saturatingAdd(out[rcode], n)
```

- [ ] **Step 4: Harden `Assess`**

In `Assess`, replace:

```go
	var total int64
	for _, v := range agg {
		total += v
	}
```

with:

```go
	// Assess is exported and pure: it must hold for any map a caller hands it,
	// not only for one ParseResponses produced. Negative counts are dropped and
	// the sum saturates rather than wrapping.
	var total int64
	for _, v := range agg {
		if v < 0 {
			continue
		}
		total = saturatingAdd(total, v)
	}
```

and replace:

```go
	errors := agg["SERVFAIL"] + agg["REFUSED"]
	ratio := float64(errors) / float64(total)
```

with:

```go
	errors := saturatingAdd(max(agg["SERVFAIL"], 0), max(agg["REFUSED"], 0))
	if errors > total {
		errors = total
	}
	ratio := float64(errors) / float64(total)
```

`max` is a Go 1.21+ builtin — no import and no new dependency.

- [ ] **Step 5: Run it — must pass**

```bash
go test ./internal/dnshealth -p 2 -v
go test ./internal/dnshealth -run '^$' -fuzz '^FuzzParseResponses$' -fuzztime 90s
```

Expected: PASS, no new `testdata/fuzz` files.

- [ ] **Step 6: Mutation-check**

Revert the `math.IsNaN(v) || math.IsInf(v, 0) || v < 0` guard, confirm `go test ./internal/dnshealth -run 'FuzzParseResponses' -p 2` FAILS, restore.

- [ ] **Step 7: Write the controlplane fuzz target**

Create `internal/controlplane/fuzz_test.go`:

```go
package controlplane

import (
	"fmt"
	"testing"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// FuzzParseReadyz fuzzes the apiserver /readyz classifier. The failing-check
// names are tokens lifted straight out of the response body, which no schema
// constrains, and the list had no count bound at all.
func FuzzParseReadyz(f *testing.F) {
	f.Add(200, []byte("[+]etcd ok\nreadyz check passed"))
	f.Add(500, []byte("[-]etcd failed: reason withheld\n[-]poststarthook/start-apiserver failed"))
	f.Add(403, []byte{})
	f.Add(0, []byte{})
	f.Add(503, []byte("[-]\x1b]0;pwned\x07 failed"))
	f.Add(503, []byte("[-]\xff\xfe failed"))
	f.Add(503, []byte("[-]\u202eetcd failed"))
	f.Add(503, []byte("[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x\n[-]x"))

	f.Fuzz(func(t *testing.T, code int, body []byte) {
		p := ParseReadyz(code, body)

		switch p.Status {
		case "ok", "unhealthy", "forbidden", "unreachable":
		default:
			t.Errorf("ParseReadyz(%d): status %q is outside the documented set", code, p.Status)
		}
		if len(p.Failed) > maxFailedChecks {
			t.Errorf("ParseReadyz(%d): %d failing checks exceeds the %d-entry cap", code, len(p.Failed), maxFailedChecks)
		}
		for i, name := range p.Failed {
			where := fmt.Sprintf("failed[%d]", i)
			fuzzgen.AssertSafe(t, where, name)
			fuzzgen.AssertBounded(t, where, name, safetext.MaxLine)
		}

		if again := ParseReadyz(code, body); again.Status != p.Status || len(again.Failed) != len(p.Failed) {
			t.Errorf("ParseReadyz is not deterministic")
		}
	})
}
```

- [ ] **Step 8: Watch it fail**

```bash
go test ./internal/controlplane -run 'FuzzParseReadyz' -p 2 -v
```

Expected: FAIL to build first — `maxFailedChecks` is undefined. That is the honest starting point: the cap does not exist. Add the constant in Step 9 and re-run to see the `AssertSafe` failures on the escape/invalid-UTF-8/RTL seeds and the cap failure on the 25-check seed. Record them.

- [ ] **Step 9: Cap and sanitize `failedChecks`**

In `internal/controlplane/controlplane.go`, replace the import line and `failedChecks`:

```go
import (
	"strings"

	"github.com/imantaba/kubeagent/internal/safetext"
)

// maxFailedChecks bounds the reported failing-check list. A real apiserver has
// on the order of a dozen readyz checks; a body claiming hundreds is not one,
// and a report is for reading, not for archiving whatever answered the port.
const maxFailedChecks = 20

// failedChecks extracts the check name from each "[-]<name> …" line of a verbose
// /readyz body, in order, sanitized and capped at maxFailedChecks. Returns nil
// when there are none (a generic not-ready).
//
// These names are tokens from an HTTP response body that no schema constrains,
// and they are printed. Sanitizing them here rather than at each renderer keeps
// the guarantee with the parser that produced them.
func failedChecks(body []byte) []string {
	var failed []string
	for _, ln := range strings.Split(string(body), "\n") {
		if len(failed) == maxFailedChecks {
			break
		}
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "[-]") {
			if fields := strings.Fields(ln[3:]); len(fields) > 0 {
				failed = append(failed, safetext.Line(fields[0]))
			}
		}
	}
	return failed
}
```

- [ ] **Step 10: Run it — must pass**

```bash
go test ./internal/controlplane -p 2 -v
go test ./internal/controlplane -run '^$' -fuzz '^FuzzParseReadyz$' -fuzztime 60s
```

Expected: PASS. If an existing `controlplane` unit test asserted an exact `Failed` slice, confirm its fixture is clean text — `Line` is a no-op on clean names, so no existing expectation should change. If one does change, stop and report it rather than editing the expectation.

- [ ] **Step 11: Mutation-check**

Remove the `safetext.Line` call, confirm FAIL; restore. Then raise `maxFailedChecks` to 1000, confirm the cap assertion FAILs on the 25-check seed; restore to 20.

- [ ] **Step 12: Write the certhealth fuzz target**

Create `internal/certhealth/fuzz_test.go`:

```go
package certhealth

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// seedCertPEM is a throwaway self-signed certificate for the RFC 2606 example
// domain fuzz.example, valid for a century, generated once for this seed. Its
// private key was deleted immediately and never existed in the repository — a
// certificate is public by definition, and this one fronts nothing.
const seedCertPEM = `-----BEGIN CERTIFICATE-----
MIIDKjCCAhKgAwIBAgIUXLenN2guMRspaOeIZ6HsoZrO3MIwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMZnV6ei5leGFtcGxlMCAXDTI2MDcyOTIxMDczMFoYDzIx
MjYwNzA1MjEwNzMwWjAXMRUwEwYDVQQDDAxmdXp6LmV4YW1wbGUwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQDL85gB5AIwi7n6PXnqy3i9A4FlGxxmcuMY
x126LLyM0KD9uMMIadykNJtnaUP4cVhtALlGPyQnMm7s1eHaim8rQKES1sXLuBJ2
WFnod5PQ8CVeDId/1xYtnZUENVa1jTwuAHqXusvXFKfa81snHJrLDUAhQSbzeBZi
fFCjNk6I/SLmhFQWK8k9Ir8CaxD24WojR7pAHFZiKyke/h3JVWvbROwGEw8gXwCJ
hzwnRgx1qwH5vdQ0DoJyiT653oKoF5Ea8Ns94gBBMLSgjH3QKsaxiKcFfQwKhFEV
ib9f8iMH0v2P8lZ+zjGE/7ivFLSLmSfe9Tz/tIldaxMuz0IOEsUzAgMBAAGjbDBq
MB0GA1UdDgQWBBSBMmkpEszQNAWTK2Va7IOsJsOd6TAfBgNVHSMEGDAWgBSBMmkp
EszQNAWTK2Va7IOsJsOd6TAPBgNVHRMBAf8EBTADAQH/MBcGA1UdEQQQMA6CDGZ1
enouZXhhbXBsZTANBgkqhkiG9w0BAQsFAAOCAQEAGZMUlmYr4vCHqmgFBrd/Th1C
5emfJXgdKBDlbaVXx0Kqx+j/dt6scmfeEBbqCn8xYwNXoJcKYtqyq+Y3f5Oc8P9n
8hhQp2YPQATOtWG0t4T3R3QzfCLJh86FxeQALxIcDThM1r793oh8c5WtGr6QM8iW
NDwenlFTkIgv2aGgjeYij5rwfIolmh7eGED5muMKSGECkeYYTVzdYLdhMEtsiPxD
YCG0bsXna/zjXI6zGgEjMs8RNIgxNR7Z3nRj7zAlx3pxEBCK9zkXACYDLzXIAT8D
iuYGaGgubV018cZJPVVBH6/MX+q5TMQ9L9PCGHRF8fTtR65PQhFMDIPMFad1BA==
-----END CERTIFICATE-----
`

var fuzzNow = time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

// FuzzCertAssess feeds arbitrary bytes to the X.509 path. The bytes stand in for
// tls.crt, which any identity that can create a kubernetes.io/tls Secret
// controls, and the certificate subject it yields is printed in every report.
func FuzzCertAssess(f *testing.F) {
	f.Add([]byte(seedCertPEM), []byte("seed"))
	f.Add([]byte{}, []byte{})
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nnot base64 at all\n-----END CERTIFICATE-----\n"), []byte{1})
	f.Add([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), []byte{2})
	f.Add([]byte("\x1b[2J\x1b[H"), []byte{3})
	f.Add([]byte("\xff\xfe\xfd"), []byte{4})

	f.Fuzz(func(t *testing.T, crt, params []byte) {
		c := fuzzgen.New(params)
		secret := c.TLSSecret(crt)
		warnDays := c.IntN(400)

		rep := Assess([]corev1.Secret{secret}, nil, warnDays, fuzzNow)

		if rep.Checked != 1 {
			t.Errorf("Checked = %d, want 1 — one kubernetes.io/tls secret went in", rep.Checked)
		}
		for _, inv := range rep.Invalid {
			switch inv.Detail {
			case "missing tls.crt", "invalid certificate data":
			default:
				t.Errorf("Invalid.Detail = %q, outside the documented set", inv.Detail)
			}
			fuzzgen.AssertSafe(t, "invalid.namespace", inv.Namespace)
			fuzzgen.AssertSafe(t, "invalid.name", inv.Name)
		}
		for _, cert := range append(append([]Cert{}, rep.Expired...), rep.Expiring...) {
			fuzzgen.AssertSafe(t, "cert.commonName", cert.CommonName)
			fuzzgen.AssertBounded(t, "cert.commonName", cert.CommonName, safetext.MaxLine)
			fuzzgen.AssertSafe(t, "cert.notAfter", cert.NotAfter)
			if _, err := time.Parse(time.RFC3339, cert.NotAfter); err != nil {
				t.Errorf("NotAfter = %q is not RFC3339: %v", cert.NotAfter, err)
			}
			for i, ing := range cert.Ingresses {
				fuzzgen.AssertSafe(t, "cert.ingresses", ing)
				_ = i
			}
		}

		again := Assess([]corev1.Secret{secret}, nil, warnDays, fuzzNow)
		if !reflect.DeepEqual(rep, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}
```

- [ ] **Step 13: Watch it fail**

```bash
go test ./internal/certhealth -run 'FuzzCertAssess' -p 2 -v
```

Expected: PASS on the seeds alone — none of them yields a certificate with a hostile subject, because forging one takes more than a few bytes. Then fuzz to find it:

```bash
go test ./internal/certhealth -run '^$' -fuzz '^FuzzCertAssess$' -fuzztime 180s
```

Expected: either a crasher under `internal/certhealth/testdata/fuzz/FuzzCertAssess/` (commit it as a regression seed) or a clean run. If the fuzzer does not reach a hostile CommonName in three minutes, the fix in Step 14 still lands — the reachability argument is in the code, not in whether a random search found it — but say so plainly in the task report rather than claiming the property was watched failing. To make the gap concrete, add this deterministic unit test alongside the fuzz target and watch **it** fail:

```go
func TestAssessSanitizesTheCertificateSubject(t *testing.T) {
	// A cert whose CN carries an ANSI escape. Reproduced from the leaf of a
	// self-signed certificate generated with CN "a\x1b[2Jb" — any identity that
	// can create a kubernetes.io/tls Secret can supply one.
	crt := certPEM(t, "a\x1b[2Jb", nil, fuzzNow.Add(24*time.Hour))
	rep := Assess([]corev1.Secret{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "tls"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": crt},
	}}, nil, 30, fuzzNow)
	if len(rep.Expiring) != 1 {
		t.Fatalf("Expiring = %d certs, want 1", len(rep.Expiring))
	}
	if got := rep.Expiring[0].CommonName; strings.ContainsRune(got, 0x1b) {
		t.Errorf("CommonName = %q still carries an ESC byte", got)
	}
}
```

This needs `certPEM` from `certhealth_test.go` — change its first parameter from `*testing.T` to `testing.TB` so both a test and a fuzz target can call it, and add `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` and `"strings"` to the fuzz file's imports.

- [ ] **Step 14: Sanitize the certificate subject**

In `internal/certhealth/certhealth.go`, add the import `"github.com/imantaba/kubeagent/internal/safetext"` and change:

```go
		name := cert.Subject.CommonName
		if name == "" && len(cert.DNSNames) > 0 {
			name = cert.DNSNames[0]
		}
```

to:

```go
		// The subject and SANs are attacker-controlled: any identity that can
		// create a kubernetes.io/tls Secret chooses them, X.509 string types do
		// not exclude control characters, and CommonName is printed in the text
		// report, the JSON, the HTML report and the TUI.
		name := safetext.Line(cert.Subject.CommonName)
		if name == "" && len(cert.DNSNames) > 0 {
			name = safetext.Line(cert.DNSNames[0])
		}
```

- [ ] **Step 15: Run it — must pass**

```bash
go test ./internal/certhealth -p 2 -v
go test ./internal/certhealth -run '^$' -fuzz '^FuzzCertAssess$' -fuzztime 120s
```

Expected: PASS. Mutation-check by reverting the two `safetext.Line` calls and confirming `TestAssessSanitizesTheCertificateSubject` FAILs, then restore.

- [ ] **Step 16: Full suite and golden check**

```bash
go build ./... && go vet ./... && go test ./... -p 2
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: PASS, golden diff empty.

- [ ] **Step 17: Commit**

```bash
git add internal/dnshealth internal/controlplane internal/certhealth
git commit -s -m "fix: harden the three advisory parsers against hostile input

dnshealth: strconv.ParseFloat accepts \"NaN\", \"+Inf\" and \"-Inf\" without error,
and converting a non-finite or out-of-range float to int64 is
implementation-defined — it yields math.MinInt64 here. A CoreDNS exporter
reporting NaN therefore turned a response count negative and dragged the error
ratio with it. Non-finite and negative samples are now dropped, large ones
clamped, and every accumulation saturates instead of wrapping.

controlplane: the failing-check names are tokens lifted from an HTTP body no
schema constrains, printed unfiltered, with no count bound at all. They are now
sanitized and capped at 20.

certhealth: a certificate's CommonName and DNS SANs are chosen by whoever
creates the Secret, and X.509 string types do not exclude control characters.
Both are sanitized before they reach a report.

Each fix has a fuzz target that fails without it."
```

---

### Task 7: The read cap, the CI matrix, and the docs

**Files:**
- Modify: `internal/collect/collect.go:470-497`
- Modify: `internal/collect/collect_test.go` (append two tests)
- Create: `.github/workflows/fuzz.yml`
- Modify: `CONTRIBUTING.md`, `docs/go-concepts.md`, `CHANGELOG.md`, `website/docs/roadmap.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: nothing from Tasks 1-6 in the code change; the workflow references the targets those tasks created.
- Produces: nothing.

**Defect 3, stated precisely — and its honest limit.** All three proxied reads end in `Do(ctx).StatusCode(&code).Raw()`. client-go's `Result.Raw()` returns a body it has **already read in full**, with no cap, and gives no access to the underlying reader. A cap applied to the returned slice therefore bounds what the parsers handle and what any later copy costs; it does **not** bound the transfer or the peak allocation inside client-go. Bounding the transfer needs a custom `http.RoundTripper` on the rest config, which is a separate change with its own blast radius. The comment in the code must say this, and so must the CHANGELOG entry — a cap described as bounding the transfer would be a false claim.

**Why the gate for this branch is the full chaos gate.** This task touches `internal/collect`. Per the project's gate rule, that means `./chaos/run.sh --recreate`, not a smoke test — see the wrap-up below.

- [ ] **Step 1: Write the failing cap tests**

Append to `internal/collect/collect_test.go`:

```go
func TestCapBody(t *testing.T) {
	small := []byte("ok")
	if got := capBody(small); string(got) != "ok" {
		t.Errorf("capBody shortened a small body to %q", got)
	}
	big := bytes.Repeat([]byte("a"), maxProxyBody+4096)
	if got := capBody(big); len(got) != maxProxyBody {
		t.Errorf("capBody returned %d bytes, want %d", len(got), maxProxyBody)
	}
	if got := capBody(nil); got != nil {
		t.Errorf("capBody(nil) = %v, want nil", got)
	}
}

// A proxied endpoint answering with far more than kubeagent will ever parse must
// not hand the whole body to a parser. client-go's Raw() has already read it
// all, so this bounds what the parsers see and what a later copy costs — not the
// transfer itself.
func TestCoreDNSMetricsCapsTheBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxProxyBody+64*1024))
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("building a client for the oversized-body server: %v", err)
	}
	body, code := CoreDNSMetrics(context.Background(), client, "kube-system", "coredns-1")
	if code != 200 {
		t.Errorf("code = %d, want 200", code)
	}
	if len(body) != maxProxyBody {
		t.Errorf("body = %d bytes, want it capped at %d", len(body), maxProxyBody)
	}
}
```

`bytes` may already be imported in this file; check before adding it.

- [ ] **Step 2: Run them to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/collect -run 'TestCapBody|TestCoreDNSMetricsCapsTheBody' -p 2
```

Expected: FAIL to build — `capBody` and `maxProxyBody` are undefined.

- [ ] **Step 3: Add the cap and apply it at all three sites**

In `internal/collect/collect.go`, above `KubeletHealthz`, add:

```go
// maxProxyBody bounds what kubeagent will parse from a proxied endpoint — a
// kubelet /healthz, a CoreDNS /metrics, an apiserver /readyz. 1 MiB is well past
// any real response and well short of a body worth parsing by mistake.
//
// This bounds the parsers and any later copy, NOT the transfer: client-go's
// Result.Raw() returns a body it has already read in full, with no cap, and
// gives no access to the underlying reader. Bounding the transfer would need a
// custom http.RoundTripper on the rest config — a separate change.
const maxProxyBody = 1 << 20

// capBody returns at most maxProxyBody bytes of b.
func capBody(b []byte) []byte {
	if len(b) > maxProxyBody {
		return b[:maxProxyBody]
	}
	return b
}
```

Then apply it at the three read sites:

```go
	return classify(node, code, capBody(body))
```

```go
	return capBody(body), code
```

```go
	return controlplane.ParseReadyz(code, capBody(body))
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/collect -p 2 -v
```

Expected: PASS, including the existing `TestNodeStatsReturnsForbidden` and `TestClassifyKubeletHealthz`.

- [ ] **Step 5: Commit the cap**

```bash
git add internal/collect
git commit -s -m "fix(collect): cap the three proxied reads at 1 MiB

The kubelet /healthz, CoreDNS /metrics and apiserver /readyz reads all end in
Raw(), which returns a body already read in full with no cap, and handed it
whole to a parser. Fuzzing those parsers while their input was unbounded would
have been incoherent.

The cap bounds what the parsers handle and what a later copy costs. It does NOT
bound the transfer: Raw() gives no access to the underlying reader, so that
needs a custom http.RoundTripper on the rest config — a separate change."
```

- [ ] **Step 6: Write the fuzz workflow**

Create `.github/workflows/fuzz.yml`:

```yaml
name: fuzz

# Fuzzing is a search, not a check: it belongs on a schedule, not on every push.
# The seed corpus of every target replays on the normal `go test ./...` in CI, so
# a regression a past campaign found still fails a pull request immediately.
#
# `go test -fuzz` accepts exactly one target and one package per invocation,
# which is why this is a matrix of (package, target) pairs rather than one job.
on:
  schedule:
    - cron: '17 3 * * *'
  workflow_dispatch:
    inputs:
      fuzztime:
        description: 'Budget per target, as a Go duration (e.g. 120s, 10m)'
        required: false
        default: '120s'

permissions:
  contents: read

jobs:
  fuzz:
    runs-on: ubuntu-latest
    strategy:
      fail-fast: false
      matrix:
        include:
          - package: ./internal/diagnose
            target: FuzzDetectors
          - package: ./internal/logscan
            target: FuzzClassify
          - package: ./internal/redact
            target: FuzzRedactURL
          - package: ./internal/redact
            target: FuzzRedactError
          - package: ./internal/dnshealth
            target: FuzzParseResponses
          - package: ./internal/controlplane
            target: FuzzParseReadyz
          - package: ./internal/certhealth
            target: FuzzCertAssess
    name: ${{ matrix.target }}
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Fuzz ${{ matrix.target }}
        run: |
          go test ${{ matrix.package }} \
            -run '^$' \
            -fuzz '^${{ matrix.target }}$' \
            -fuzztime '${{ inputs.fuzztime || '120s' }}'

      - name: Upload any crasher
        if: failure()
        uses: actions/upload-artifact@v4
        with:
          name: crashers-${{ matrix.target }}
          path: internal/**/testdata/fuzz/**
          if-no-files-found: ignore
```

- [ ] **Step 7: Validate the workflow's shape locally, then run it once for real**

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/fuzz.yml')); print('yaml ok')"
```

Then, after the branch is pushed (this is the one step that needs the remote):

```bash
git push -u origin fuzzed-detectors
gh workflow run fuzz.yml --ref fuzzed-detectors -f fuzztime=20s
gh run list --workflow=fuzz.yml --limit 1
```

Watch all seven jobs go green. A workflow that has never run is a workflow that is assumed to work, not known to.

- [ ] **Step 8: Add the CONTRIBUTING bullet**

In `CONTRIBUTING.md`, in the Testing section, after the golden-output paragraph, add:

```markdown
- **Fuzzing.** Seven native fuzz targets cover the parsers and the detector set
  (`FuzzDetectors`, `FuzzClassify`, `FuzzRedactURL`, `FuzzRedactError`,
  `FuzzParseResponses`, `FuzzParseReadyz`, `FuzzCertAssess`). Their seed corpora
  replay on a plain `go test ./...`, so a regression a past campaign found fails
  your pull request immediately — no fuzzing budget needed. A real campaign runs
  nightly in `.github/workflows/fuzz.yml`, one job per target, because
  `go test -fuzz` takes exactly one target and one package per invocation. To
  run one yourself:

  ```bash
  go test ./internal/diagnose -run '^$' -fuzz '^FuzzDetectors$' -fuzztime 60s
  ```

  If Go writes a file under `testdata/fuzz/<Target>/`, that is a real finding:
  fix the cause and **commit the file** — it becomes a permanent regression seed.
  Objects come from `internal/fuzzgen`, which is test-only: it draws DNS-1123
  alphabets for the fields the API server validates and hostile bytes for the
  fields it does not. Text that crosses into a kubeagent value passes through
  `internal/safetext.Line` at its ingress point, not at each renderer.
```

- [ ] **Step 9: Add the go-concepts entry**

In `docs/go-concepts.md`, insert a new numbered section before the `## Coming later` heading, following the established style — plain everyday example first, then the kubeagent example, no Python comparisons:

```markdown
## 21. Fuzzing: let the test invent the input

A normal test picks the inputs. You think of the cases, you write them down, and
the test checks those. That works right up to the case you did not think of.

Fuzzing turns it around. You write down a *property* — something that must be
true for every input — and the toolchain invents inputs trying to break it. Go
has this built in: a function named `FuzzXxx` taking `*testing.F`.

Say you have a function that shortens a string to ten characters:

```go
func Short(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}
```

The property is easy to say: the result is never longer than ten. Write it:

```go
func FuzzShort(f *testing.F) {
	f.Add("hello")             // a seed: a starting point for the search
	f.Fuzz(func(t *testing.T, s string) {
		if len(Short(s)) > 10 {
			t.Errorf("Short(%q) is too long", s)
		}
	})
}
```

Two things to know before running it. First, `f.Add` seeds replay on a plain
`go test` with no fuzzing at all — so seeds are permanent regression tests, free.
Second, `go test -fuzz` searches only when you ask, one target at a time:

```bash
go test -run '^$' -fuzz '^FuzzShort$' -fuzztime 30s
```

That property passes. Now try a version with a rune-aware cut, and the fuzzer
finds `"héllo wörld!"` — slicing bytes cut a two-byte rune in half and produced
invalid UTF-8. That is the shape of almost every fuzzing find: not a case you
were careless about, a case you had no reason to picture.

The catch is that `f.Fuzz` only accepts `[]byte`, `string`, and the numeric and
`bool` primitives. It will never hand you a struct.

### In kubeagent

kubeagent's detectors take a `*corev1.Pod` — exactly the struct the fuzzer will
not build. So `internal/fuzzgen` builds one *from* bytes: a `Cursor` walks the
fuzzer's input and draws a field at a time, wrapping when it runs out so even a
one-byte input funds a whole pod.

```go
c := fuzzgen.New(in)          // in is the fuzzer's []byte
pod := c.Pod()
facts := diagnose.PodFacts{Pod: pod, Events: c.Events(pod, 4)}
findings := diagnose.Run(diagnose.DefaultDetectors(fuzzgen.Base), []diagnose.PodFacts{facts})
```

Same bytes, same pod, every time — which is what makes a reported crash
reproducible. And the properties are the four things a detector must always do:
not panic, not modify the pod it was handed, return the same findings twice, and
never put a raw byte from the cluster onto a terminal.

One design decision is worth more than the mechanics. The generator does *not*
draw pod names from hostile bytes. The API server validates names, namespaces
and container names as DNS-1123 labels, so no real cluster can present anything
else there — a property that failed on a pod named `"\x1b[2J"` would be
reporting a bug in the generator. Hostile bytes go only where the API server
validates nothing: `waiting.Message`, `terminated.Reason`, event and condition
messages, and a container's own log. That last one is the important one. It is
the only input an attacker needs no permissions at all to control.
```

- [ ] **Step 10: Add the CHANGELOG entry**

In `CHANGELOG.md`, under `## [Unreleased]`, add:

```markdown
### Added

- Native fuzzing across the detectors and the advisory parsers: seven `go test
  -fuzz` targets assert that no Kubernetes object or endpoint response can panic
  a scan, that the detector set stays pure and deterministic, and that no raw
  byte from the cluster reaches a terminal. Seed corpora replay on a plain `go
  test`, so a regression fails a pull request without any fuzzing budget; a real
  campaign runs nightly in `.github/workflows/fuzz.yml`. Objects come from the
  test-only `internal/fuzzgen`, which draws DNS-1123 alphabets for the fields the
  API server validates and hostile bytes for the fields it does not.
- `internal/safetext`: one sanitizer (`Line`) for text arriving from unvalidated
  API fields — bounds it to 512 runes and removes control characters, Unicode
  formatting characters (U+202E and friends, which `unicode.IsControl` does not
  catch) and invalid UTF-8.

### Fixed

- Text from fields the Kubernetes API server does not validate reached an
  operator's terminal unfiltered at eight ingress points: `waiting.Message`,
  `terminated.Reason`, `PodScheduled` and event messages, the container name
  parsed out of an event field path, a crashed container's log excerpt, and the
  dependency address spliced into a `connection refused` cause. The log tail is
  the one an unprivileged attacker controls outright, and it carried the same
  ANSI escapes kubeagent's own TUI uses to switch screens. All eight now pass
  through `safetext.Line`.
- `dnshealth`: `strconv.ParseFloat` accepts `"NaN"`, `"+Inf"` and `"-Inf"`
  without error, and converting a non-finite or out-of-range float to `int64` is
  implementation-defined — it yields `math.MinInt64` on amd64. A CoreDNS
  exporter reporting any of them turned a DNS response count negative and
  dragged the error ratio with it. Non-finite and negative samples are dropped,
  large ones clamped, and every accumulation saturates instead of wrapping.
- `controlplane`: the failing `/readyz` check names were tokens lifted from an
  HTTP body no schema constrains, printed unfiltered, with no count bound. They
  are now sanitized and capped at 20.
- `certhealth`: a certificate's `CommonName` and DNS SANs are chosen by whoever
  creates the `kubernetes.io/tls` Secret, and X.509 string types do not exclude
  control characters. Both are sanitized before they reach a report.
- `collect`: the kubelet `/healthz`, CoreDNS `/metrics` and apiserver `/readyz`
  reads handed an unbounded body straight to a parser. Parsed input is now capped
  at 1 MiB. This bounds the parsers, not the transfer: client-go's `Raw()`
  returns a body it has already read in full and gives no access to the
  underlying reader.
```

- [ ] **Step 11: Update the roadmap**

In `website/docs/roadmap.md`, find the Theme H section and mark the fuzzed-detectors slice shipped, in the same voice as the slices above it — one sentence on what landed and one on what it found. Keep the remaining Theme H item (the v1.0 production contract) as ahead.

- [ ] **Step 12: Add the two CLAUDE.md invariants**

In `CLAUDE.md`, in the **Invariants (do not break)** section, add:

```markdown
- **Untrusted API text is sanitized at ingress, not at each renderer.** Every
  value read from a field the API server does not validate — `waiting.Message`,
  `terminated.Reason`, condition and event messages, `involvedObject.fieldPath`,
  container log text, an X.509 subject or SAN, a `/readyz` check name — passes
  through `internal/safetext.Line` at the point it first enters a kubeagent
  value. Renderers then print what they are given. Adding a new renderer must
  never mean adding a new sanitizer, and a new detector that reads an
  unvalidated field must sanitize it there. Matching decisions
  (`strings.Contains`, a regexp) run on the **raw** value: sanitizing before
  matching would let a control character spliced mid-word evade a signature.
- **`internal/fuzzgen` is test-only.** It imports `testing`, so no non-test file
  may import it — every kubeagent binary would otherwise carry the testing
  package's flag registrations. `TestNoProductionImport` enforces this by walking
  the repository with `go/parser`. Like `internal/safetext`, it must never import
  `internal/remediate` or `internal/explain`.
```

- [ ] **Step 13: Verify the docs build and the whole suite**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin:/usr/local/go/bin
go build ./... && go vet ./... && go test ./... -p 2
(cd website && mkdocs build --strict -f mkdocs.yml)
scripts/dco-check.sh
git diff --stat internal/report/testdata/golden-scan.txt
```

Expected: tests PASS; `mkdocs build --strict` exits 0 with no `WARNING` lines about changed pages; the DCO check passes for every commit on the branch; the golden diff is empty. No `mkdocs.yml` nav change is needed — fuzzing is contributor-facing and lives in `CONTRIBUTING.md` and `docs/go-concepts.md`, neither of which is in the site nav.

- [ ] **Step 14: Commit**

```bash
git add .github/workflows/fuzz.yml CONTRIBUTING.md docs/go-concepts.md CHANGELOG.md website/docs/roadmap.md CLAUDE.md
git commit -s -m "ci+docs: nightly fuzz matrix, and write down what fuzzing found

fuzz.yml runs one job per (package, target) pair because go test -fuzz takes
exactly one of each per invocation, nightly and on demand with a fuzztime input,
uploading any crasher as an artifact. Seed corpora already replay on the normal
go test, so a pull request fails on a past finding without a fuzzing budget.

CONTRIBUTING gains a Fuzzing bullet, go-concepts a native-fuzzing entry, and
CLAUDE.md two invariants: sanitize at ingress rather than per renderer, and
fuzzgen stays out of the shipped binary."
```

---

## Wrap-up (controller, not a task)

- **Gate: the full chaos gate.** This branch touches `internal/collect`, so the project's gate rule applies:

  ```bash
  export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
  unset ANTHROPIC_API_KEY
  ./chaos/run.sh --recreate
  ```

  Every scenario must come back green. Run it in the background and watch the log.

- **Whole-branch review** on the most capable model, with the branch package from `scripts/review-package $(git merge-base main HEAD) HEAD` and the Minor findings collected in the ledger.

- **Then** `superpowers:finishing-a-development-branch`, and the release checkpoint.

## Self-Review

**Spec coverage.** Every component in the spec's nine-row table maps to a task: `safetext` (1), `DefaultDetectors` (2), `fuzzgen` (3), the seven fuzz targets (4, 5, 6), the three defect fixes (4 — ingress; 6 — dnshealth; 7 — collect cap), CI wiring (7), documentation (7). The spec's non-goals are respected: no oracle or differential implementation, no fuzzing of the I/O layer or the renderers.

**Two deliberate divergences from the spec, both recorded in the task text where they land:**

1. The spec's rule list drops every control character. Task 1 folds the *whitespace* controls (`\t`, `\n`, `\v`, `\f`, `\r`) and U+2028/U+2029 to a space instead, and drops the rest. Dropping `\n` outright would join words across lines (`"line1\nline2"` → `"line1line2"`), which is worse output for the same safety. Truncation still runs afterwards, so the budget is unchanged. `Line` also truncates to `MaxLine-1` runes plus the ellipsis, so its result is never longer than `MaxLine` — the spec left the ellipsis outside the budget, which would have forced every downstream `AssertBounded` call to pass `MaxLine+1`.
2. The spec's defect-2 table lists seven ingress sites. Task 4 fixes **eight**: `probefailure.go`'s `Container` field assigns the raw substring of an unvalidated `involvedObject.fieldPath`. The spec missed it because `probeEvidence` escapes the same value with `%q`, which made `Evidence` safe and hid that the sibling field was not.

Task 6 also adds a sanitization site the spec did not name — `certhealth`'s `CommonName`/SAN — for the same reason: writing the property surfaced it. The spec's own framing anticipated this ("add fuzzing and fix what it finds"), so it is in scope rather than scope creep.

**Placeholder scan.** No step says "add appropriate error handling", "write tests for the above", or "similar to Task N". Every code step carries the literal code. The two steps that legitimately cannot carry literal text are Task 7's roadmap edit (Step 11), which asks for one sentence in the voice of the surrounding slices because the exact surrounding text is not reproduced here, and Task 6's Step 13, which describes what to do in each of two outcomes rather than guessing which a random search will produce.

**Type consistency.** `safetext.Line`/`MaxLine` are used with the same signature in Tasks 4, 5, 6 and 7 as Task 1 defines. `fuzzgen.New`, `Base`, `Cursor.Pod`, `Cursor.Events(pod, max)`, `Cursor.TLSSecret(crt)`, `Cursor.IntN`, `AssertSafe(t, where, s)` and `AssertBounded(t, where, s, max)` match Task 3's definitions at every call site. `DefaultDetectors(now time.Time) []Detector` matches between Task 2 and Task 4. `maxFailedChecks` is referenced in Task 6's fuzz target (Step 7) and defined in the same task (Step 9) — the build failure in between is deliberate and called out. `capBody`/`maxProxyBody` are referenced in Task 7's test (Step 1) and defined in Step 3, likewise. Field names were read from the source rather than assumed: `dnshealth.Report.ServfailRatio` (not `ErrorRatio`), `controlplane.Probe.Failed`, `certhealth.Cert.CommonName`/`NotAfter`/`Ingresses`, `certhealth.Invalid.Detail`, `diagnose.Finding.{Pod,Issue,Reason,Evidence,Container,Resources}`.
