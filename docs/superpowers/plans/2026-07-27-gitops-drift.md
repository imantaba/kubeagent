# GitOps Drift Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in, advisory `GITOPS DRIFT` section to `kubeagent scan` that
answers "is this cluster still converging on Git, and if not, for how long?" for Argo
CD and Flux.

**Architecture:** A new pure package `internal/gitops` (no Kubernetes client) reduces
unstructured Argo CD `Application` and Flux `Kustomization`/`HelmRelease` objects to
five drift states. It reuses the shipped `collect.OperatorResources` fetch path, which
is already generic over `[]operators.Adapter`, so `--drift` adds no collection code and
`--operators --drift` together list the cluster once. `main.go` sets a `report.Input`
field and the renderer prints it — the CLI-composed-view pattern, so `internal/scan`
and `internal/watch` are untouched.

**Tech Stack:** Go 1.26, `k8s.io/apimachinery/.../unstructured`, standard-library
`flag`, `regexp`, `time`.

**Spec:** [docs/superpowers/specs/2026-07-27-gitops-drift-design.md](../specs/2026-07-27-gitops-drift-design.md)

## Global Constraints

- **Read-only.** `list` verbs only. No writes, no new verbs, no `--fix` extension.
- **No CR `spec` content and no condition `message`** may reach a report field, a log
  line, or an error string. Booleans read out of `spec` (`spec.suspend`,
  `spec.syncPolicy.automated`) decide state; they are never rendered. A `reason` is a
  CamelCase token and may be rendered; a `message` is arbitrary prose that routinely
  embeds URLs, and a URL can carry a token.
- **No revision-derived text escapes `ShortRevision`.** Never a branch name, never a
  repo URL, never a chart version.
- **Advisory always.** No `Finding`, no effect on the cluster verdict or the exit code.
  The `GITOPS DRIFT` section does not participate in the all-clear suppression
  condition and never sets `hasAttention`.
- **A missing timestamp is never "older than the threshold."** It degrades to `pending`
  (can self-heal) or `blocked` (cannot) — never `stale`.
- **`internal/scan` and `internal/watch` are unchanged.** The daemon gains no RBAC.
- **A scan without `--drift` is byte-identical to v0.60.0** in both text and JSON.
- v1 CLI uses the **standard-library `flag`** package only — no Cobra.
- **TDD**: write the failing test, run it, watch it fail, then implement.
- **No `Co-Authored-By: Claude` trailer** and no Claude/Claude Code/Anthropic
  attribution in any commit message, code comment, or doc. Every commit is authored
  solely by the human.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/gitops/gitops.go` (new) | package doc, `State`, `Workload`, `KindReport`, `ReconcilerReport`, `Report`, `MaxPerKind`, `Assess`, `kindReport` |
| `internal/gitops/adapters.go` (new) | `Adapters()` — the three GitOps rows |
| `internal/gitops/revision.go` (new) | `ShortRevision` — the only path a revision reaches output |
| `internal/gitops/argo.go` (new) | `assessArgo` |
| `internal/gitops/flux.go` (new) | `assessKustomization`, `assessHelmRelease`, `assessFlux` |
| `internal/gitops/helpers.go` (new) | `condition`, `ageOf`, `byAge`, `shortAge`, `durText`, `nestedString`, `parseTime` |
| `internal/report/report.go` | `Input.GitOps`, `gitopsRender`, `printGitOps`, `driftSummary` |
| `main.go` | `--drift`, `--drift-age`, `envDuration`, shared-fetch wiring |
| `deploy/rbac-gitops.yaml` (new) | scan-only ClusterRole + binding, `list` on three apiGroups |
| `chaos/run.sh` | scenario 17 |

---

### Task 1: `internal/gitops` skeleton — types, adapters, helpers, revision redaction

**Files:**

- Create: `internal/gitops/gitops.go`, `internal/gitops/adapters.go`,
  `internal/gitops/revision.go`, `internal/gitops/helpers.go`
- Test: `internal/gitops/revision_test.go`, `internal/gitops/helpers_test.go`,
  `internal/gitops/adapters_test.go`

**Interfaces:**

- Consumes: `operators.Adapter` from `github.com/imantaba/kubeagent/internal/operators`.
- Produces: `gitops.State` + its five constants, `gitops.Workload`,
  `gitops.KindReport`, `gitops.ReconcilerReport`, `gitops.Report`,
  `gitops.MaxPerKind`, `gitops.Adapters() []operators.Adapter`,
  `gitops.ShortRevision(string) string`, and the unexported `assessment`,
  `condition`, `ageOf`, `byAge`, `shortAge`, `durText`, `nestedString`, `parseTime`
  used by Tasks 2–4.

- [ ] **Step 1: Write the failing tests**

`internal/gitops/revision_test.go`:

```go
package gitops

import "testing"

func TestShortRevision(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain sha", "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "a1b2c3d"},
		{"short sha", "a1b2c3d", "a1b2c3d"},
		{"flux branch qualified", "main@sha1:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", "a1b2c3d"},
		{"flux tag qualified", "v1.2.3@sha1:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c", "9f8e7d6"},
		{"branch name alone", "main", "(revision withheld)"},
		{"tag alone", "v1.2.3", "(revision withheld)"},
		{"chart version", "1.14.5", "(revision withheld)"},
		{"repo url with token", "https://tok3n@git.example/org/repo.git", "(revision withheld)"},
		{"branch that looks hex until the ref split", "deadbeef@sha1:notahexrevision", "(revision withheld)"},
		{"too short", "a1b2c3", "(revision withheld)"},
		{"uppercase is not a git sha", "A1B2C3D4E5F6A7B8", "(revision withheld)"},
		{"empty", "", "(revision withheld)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShortRevision(tt.raw); got != tt.want {
				t.Errorf("ShortRevision(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
```

`internal/gitops/helpers_test.go`:

```go
package gitops

import (
	"testing"
	"time"
)

func TestShortAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{12 * time.Minute, "12m"},
		{5 * time.Hour, "5h"},
		{72 * time.Hour, "3d"},
	}
	for _, tt := range tests {
		if got := shortAge(tt.d); got != tt.want {
			t.Errorf("shortAge(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestDurText(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{time.Hour, "1h"},
		{90 * time.Minute, "90m"},
		{30 * time.Second, "30s"},
		{0, "0s"},
		{-time.Hour, "0s"},
	}
	for _, tt := range tests {
		if got := durText(tt.d); got != tt.want {
			t.Errorf("durText(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestAgeOf(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

	if _, known := ageOf(time.Time{}, now); known {
		t.Error("a zero timestamp must not be a known age")
	}
	d, known := ageOf(now.Add(-3*time.Hour), now)
	if !known || d != 3*time.Hour {
		t.Errorf("ageOf(3h ago) = %v, %v; want 3h, true", d, known)
	}
	// Clock skew: a stamp in the future is not staleness.
	d, known = ageOf(now.Add(time.Hour), now)
	if !known || d != 0 {
		t.Errorf("ageOf(future) = %v, %v; want 0, true", d, known)
	}
}

func TestByAge(t *testing.T) {
	const thr = time.Hour
	if got := byAge(0, false, thr); got != StatePending {
		t.Errorf("unknown age = %q, want pending — a missing timestamp is never stale", got)
	}
	if got := byAge(thr, true, thr); got != StatePending {
		t.Errorf("exactly at the threshold = %q, want pending", got)
	}
	if got := byAge(thr+time.Nanosecond, true, thr); got != StateStale {
		t.Errorf("one nanosecond past the threshold = %q, want stale", got)
	}
}

func TestConditionReadsNoMessage(t *testing.T) {
	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{
					"type":               "Ready",
					"status":             "False",
					"reason":             "BuildFailed",
					"message":            "failed to clone https://tok3n@git.example/org/repo",
					"lastTransitionTime": "2026-07-27T09:00:00Z",
				},
			},
		},
	}
	status, reason, changed, found := condition(obj, "Ready")
	if !found || status != "False" || reason != "BuildFailed" {
		t.Fatalf("condition() = %q, %q, %v, %v", status, reason, changed, found)
	}
	if want := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC); !changed.Equal(want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	if _, _, _, found := condition(obj, "Stalled"); found {
		t.Error("absent condition reported as found")
	}
}
```

`internal/gitops/adapters_test.go`:

```go
package gitops

import "testing"

func TestAdaptersAreGitOpsKindsOnly(t *testing.T) {
	want := map[string]string{
		"argoproj.io/applications":                       "Application",
		"kustomize.toolkit.fluxcd.io/kustomizations":     "Kustomization",
		"helm.toolkit.fluxcd.io/helmreleases":            "HelmRelease",
	}
	got := Adapters()
	if len(got) != len(want) {
		t.Fatalf("Adapters() has %d rows, want %d", len(got), len(want))
	}
	for _, a := range got {
		key := a.Group + "/" + a.Resource
		kind, ok := want[key]
		if !ok {
			t.Errorf("unexpected adapter %s", key)
			continue
		}
		if a.Kind != kind {
			t.Errorf("%s: Kind = %q, want %q", key, a.Kind, kind)
		}
		if a.Rule != nil {
			t.Errorf("%s: carries a health Rule; health belongs to internal/operators", key)
		}
		if len(a.SuspendPath) != 0 {
			t.Errorf("%s: carries a SuspendPath; this package reads suspend itself", key)
		}
		if a.Version == "" || a.Operator == "" {
			t.Errorf("%s: Operator/Version must be set", key)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/
```

Expected: FAIL — the package does not exist (`no Go files`/undefined identifiers).

- [ ] **Step 3: Implement**

`internal/gitops/gitops.go`:

```go
// Package gitops answers one question about a cluster reconciled by Argo CD or
// Flux: is it still converging on Git, and if not, for how long? Pure: no
// Kubernetes client, no I/O, unit-tested with fixture objects.
//
// This is never a comparison against Git. kubeagent clones no repository, talks
// to no Git host, and renders no manifest. Every signal is read from the
// reconciler's own status, so "drift" here means the reconciler itself says it
// has not converged.
//
// Advisory only, like internal/operators: a reconciler's opinion of itself, read
// through field paths kubeagent infers, must not drive kubeagent's headline
// verdict.
//
// This package is also a boundary the raw objects do not cross. No spec string,
// no condition message, and no unredacted revision reaches a Workload: an Argo CD
// Application's spec.source.repoURL can carry a token, a condition message
// routinely embeds URLs, and Flux publishes revisions as "<ref>@sha1:<hash>"
// where <ref> is arbitrary user text.
package gitops

import (
	"sort"
	"time"

	"github.com/imantaba/kubeagent/internal/alert"
	"github.com/imantaba/kubeagent/internal/operators"
)

// State is one reconciled object's convergence, as its own reconciler reports it.
type State string

const (
	StateSynced  State = "synced"  // the reconciler reports it has converged
	StatePending State = "pending" // differs, younger than the threshold, can self-heal
	StateStale   State = "stale"   // has differed for longer than the threshold
	StateBlocked State = "blocked" // cannot self-heal at any age
	StateUnknown State = "unknown" // no usable signal
)

// severity orders enumeration worst-first so the per-kind cap drops the least
// interesting rows rather than an arbitrary alphabetical tail.
func (s State) severity() int {
	switch s {
	case StateBlocked:
		return 0
	case StateStale:
		return 1
	case StatePending:
		return 2
	default:
		return 3
	}
}

// Workload is one reconciled object, reduced to what the report may show.
type Workload struct {
	Reconciler string `json:"reconciler"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	State      State  `json:"state"`
	Detail     string `json:"detail,omitempty"` // short, single line, already redacted
}

// KindReport is one reconciled kind's roll-up.
type KindReport struct {
	Kind       string        `json:"kind"`
	APIVersion string        `json:"apiVersion"`
	Counts     map[State]int `json:"counts"`            // exact, never truncated
	Drifted    []Workload    `json:"drifted,omitempty"` // every non-synced object, capped at MaxPerKind
	Truncated  int           `json:"truncated,omitempty"`
	Forbidden  bool          `json:"forbidden,omitempty"`
	Error      string        `json:"error,omitempty"` // any other list failure, redacted
}

// Total is the number of objects counted for this kind.
func (k KindReport) Total() int {
	n := 0
	for _, c := range k.Counts {
		n += c
	}
	return n
}

// ReconcilerReport is one reconciler's roll-up across its kinds.
type ReconcilerReport struct {
	Reconciler  string       `json:"reconciler"`
	APIVersions []string     `json:"apiVersions,omitempty"`
	Kinds       []KindReport `json:"kinds,omitempty"`
}

// Report is the whole advisory view. Empty when neither reconciler is installed.
type Report struct {
	Threshold   string             `json:"threshold"` // human form of --drift-age, e.g. "1h"
	Reconcilers []ReconcilerReport `json:"reconcilers,omitempty"`
}

// MaxPerKind bounds how many non-synced objects one kind enumerates. An Argo CD
// estate can hold thousands of Applications: counts stay exact, the printed list
// does not, and the remainder is reported rather than dropped.
const MaxPerKind = 20

// assessment is one object's verdict before it becomes a Workload.
type assessment struct {
	State  State
	Detail string
}

// assessor evaluates one object of one kind.
type assessor func(obj map[string]any, now time.Time, threshold time.Duration) assessment

// Assess reduces each fetched adapter result to drift states and counts.
//
// Non-GitOps kinds are ignored rather than rejected: when a scan runs both
// --operators and --drift, main.go lists the cluster once with the operator
// adapter superset and hands the same results to both assessors.
//
// Deterministic: reconcilers and kinds keep the order collect handed them, and
// each kind's enumeration is sorted worst-first, then by namespace and name,
// before it is capped.
func Assess(fetched []operators.Fetched, now time.Time, threshold time.Duration) Report {
	if threshold < 0 {
		threshold = 0
	}
	rep := Report{Threshold: durText(threshold)}
	index := map[string]int{} // reconciler name → position in rep.Reconcilers
	for _, f := range fetched {
		assess, ok := assessorFor(f.Adapter)
		if !ok {
			continue
		}
		i, seen := index[f.Adapter.Operator]
		if !seen {
			rep.Reconcilers = append(rep.Reconcilers, ReconcilerReport{Reconciler: f.Adapter.Operator})
			i = len(rep.Reconcilers) - 1
			index[f.Adapter.Operator] = i
		}
		rc := &rep.Reconcilers[i]
		if f.APIVersion != "" && !contains(rc.APIVersions, f.APIVersion) {
			rc.APIVersions = append(rc.APIVersions, f.APIVersion)
		}
		if k, keep := kindReport(f, assess, now, threshold); keep {
			rc.Kinds = append(rc.Kinds, k)
		}
	}
	return rep
}

// assessorFor matches a fetched adapter to its evaluator by group and resource.
func assessorFor(a operators.Adapter) (assessor, bool) {
	switch {
	case a.Group == "argoproj.io" && a.Resource == "applications":
		return assessArgo, true
	case a.Group == "kustomize.toolkit.fluxcd.io" && a.Resource == "kustomizations":
		return assessKustomization, true
	case a.Group == "helm.toolkit.fluxcd.io" && a.Resource == "helmreleases":
		return assessHelmRelease, true
	}
	return nil, false
}

// kindReport builds one kind's roll-up and reports whether it has anything to
// say. A kind with no objects, no denial, and no error is omitted: "installed and
// idle" is carried by the reconciler's own entry.
func kindReport(f operators.Fetched, assess assessor, now time.Time, threshold time.Duration) (KindReport, bool) {
	k := KindReport{
		Kind:       f.Adapter.Kind,
		APIVersion: f.APIVersion,
		Counts:     map[State]int{},
		Forbidden:  f.Forbidden,
	}
	if f.Err != nil {
		// A cluster's API URL can carry userinfo or an auth-proxy token, and
		// client-go wraps it in a *url.Error. Reduce it to scheme://host.
		k.Error = alert.RedactError(f.Err)
	}
	if k.Forbidden || k.Error != "" {
		return k, true
	}
	if len(f.Items) == 0 {
		return k, false
	}
	var drifted []Workload
	for _, item := range f.Items {
		a := assess(item.Object, now, threshold)
		k.Counts[a.State]++
		if a.State == StateSynced {
			continue
		}
		drifted = append(drifted, Workload{
			Reconciler: f.Adapter.Operator,
			Kind:       f.Adapter.Kind,
			Namespace:  item.GetNamespace(),
			Name:       item.GetName(),
			State:      a.State,
			Detail:     a.Detail,
		})
	}
	sort.SliceStable(drifted, func(i, j int) bool {
		if si, sj := drifted[i].State.severity(), drifted[j].State.severity(); si != sj {
			return si < sj
		}
		if drifted[i].Namespace != drifted[j].Namespace {
			return drifted[i].Namespace < drifted[j].Namespace
		}
		return drifted[i].Name < drifted[j].Name
	})
	if len(drifted) > MaxPerKind {
		k.Truncated = len(drifted) - MaxPerKind
		drifted = drifted[:MaxPerKind]
	}
	k.Drifted = drifted
	return k, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
```

`internal/gitops/adapters.go`:

```go
package gitops

import "github.com/imantaba/kubeagent/internal/operators"

// Adapters lists the three kinds a GitOps drift scan reads. They carry no Rule
// and no SuspendPath: health is internal/operators' question, and this package
// reads suspend through its own field paths.
//
// The rows deliberately duplicate three entries of operators.Adapters(). That
// table is the operator census; this one is the smallest set --drift can fetch on
// its own, so a drift-only user needs no grant on Longhorn volumes or CNPG
// clusters. Assess matches on group and resource, so it is equally happy being
// handed either table's results.
func Adapters() []operators.Adapter {
	return []operators.Adapter{
		{
			Operator: "Argo CD", Group: "argoproj.io", Version: "v1alpha1",
			Resource: "applications", Kind: "Application",
		},
		{
			Operator: "Flux", Group: "kustomize.toolkit.fluxcd.io", Version: "v1",
			Resource: "kustomizations", Kind: "Kustomization",
		},
		{
			Operator: "Flux", Group: "helm.toolkit.fluxcd.io", Version: "v2",
			Resource: "helmreleases", Kind: "HelmRelease",
		},
	}
}
```

`internal/gitops/revision.go`:

```go
package gitops

import (
	"regexp"
	"strings"
)

// revisionWithheld is what a revision renders as when it is not a bare commit SHA.
const revisionWithheld = "(revision withheld)"

// hexRevision matches a git object name and nothing else. Uppercase is excluded:
// git writes lowercase, and accepting anything wider widens the leak surface.
var hexRevision = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// ShortRevision reduces a reconciler-reported revision to a short commit SHA, or
// withholds it entirely. It is the only path by which revision-derived text
// reaches the report.
//
// Flux publishes revisions as "<ref>@sha1:<hash>", where <ref> is arbitrary user
// text — a branch name, a tag, sometimes a path. Argo CD reports a bare SHA, but
// the same field has held tags and chart versions. Anything that is not a plain
// lowercase hex SHA of 7-40 characters is withheld rather than guessed at.
func ShortRevision(raw string) string {
	s := raw
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if !hexRevision.MatchString(s) {
		return revisionWithheld
	}
	return s[:7]
}
```

`internal/gitops/helpers.go`:

```go
package gitops

import (
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// condition finds one status condition and returns its status, its reason, and
// the time it last changed.
//
// The message is deliberately not returned. A reason is a CamelCase token by API
// convention; a message is arbitrary operator prose that routinely embeds URLs
// (a Flux clone failure quotes the repository URL verbatim), and a URL can carry
// a token.
func condition(obj map[string]any, condType string) (status, reason string, changed time.Time, found bool) {
	conds, ok, err := unstructured.NestedSlice(obj, "status", "conditions")
	if err != nil || !ok {
		return "", "", time.Time{}, false
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _, _ := unstructured.NestedString(m, "type"); t != condType {
			continue
		}
		status, _, _ = unstructured.NestedString(m, "status")
		reason, _, _ = unstructured.NestedString(m, "reason")
		changed = parseTime(nestedString(m, "lastTransitionTime"))
		return status, reason, changed, true
	}
	return "", "", time.Time{}, false
}

// ageOf returns how long ago t was and whether the timestamp was usable at all.
// A stamp in the future is clock skew, not negative staleness.
func ageOf(t, now time.Time) (time.Duration, bool) {
	if t.IsZero() {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d, true
}

// byAge classifies a difference by how long it has persisted. The threshold is
// exclusive — exactly at it is still converging — and an unknown age is never
// stale: a heuristic that cannot measure must not accuse.
func byAge(age time.Duration, known bool, threshold time.Duration) State {
	if !known {
		return StatePending
	}
	if age > threshold {
		return StateStale
	}
	return StatePending
}

// shortAge renders an elapsed duration the way the rest of the report does.
func shortAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// durText renders a configured threshold the way an operator would write it:
// 1h, 90m, 30s. Unlike shortAge it never rounds a value away.
func durText(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int(d/time.Second))
	default:
		return d.String()
	}
}

// nestedString reads a string field, yielding "" for absent, wrong-typed, or
// malformed paths — all of which mean the same thing here: no signal.
func nestedString(obj map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(obj, path...)
	return s
}

// parseTime reads an RFC3339 API timestamp, yielding the zero time on anything
// unparseable so ageOf reports the age as unknown.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
```

Note: `gitops.go` references `assessArgo`, `assessKustomization`, and
`assessHelmRelease`, which Tasks 2 and 3 write. To keep this task compiling and
its tests runnable, add a temporary `internal/gitops/assess_stubs.go` containing:

```go
package gitops

import "time"

// Temporary stubs; Task 2 replaces assessArgo and Task 3 replaces the Flux pair.
func assessArgo(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}

func assessKustomization(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}

func assessHelmRelease(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessment{State: StateUnknown}
}
```

Task 2 deletes the `assessArgo` stub, Task 3 deletes the file.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/ -v
go build ./...
```

Expected: PASS for `TestShortRevision`, `TestShortAge`, `TestDurText`, `TestAgeOf`,
`TestByAge`, `TestConditionReadsNoMessage`, `TestAdaptersAreGitOpsKindsOnly`.

- [ ] **Step 5: Commit**

```bash
git add internal/gitops/
git commit -m "feat(gitops): drift types, adapters, and revision redaction"
```

---

### Task 2: Argo CD `Application` assessment

**Files:**

- Create: `internal/gitops/argo.go`
- Modify: `internal/gitops/assess_stubs.go` (delete the `assessArgo` stub)
- Test: `internal/gitops/argo_test.go`

**Interfaces:**

- Consumes: `assessment`, `State` constants, `ShortRevision`, `ageOf`, `byAge`,
  `shortAge`, `nestedString`, `parseTime` from Task 1.
- Produces: `assessArgo(obj map[string]any, now time.Time, threshold time.Duration) assessment`,
  already referenced by `assessorFor`.

**Rules (evaluated in this order):**

1. `status.sync.status == "Synced"` → synced.
2. `status.sync.status == "OutOfSync"`:
   1. `status.operationState.phase` is `Failed` or `Error` → blocked, `last sync failed`.
   2. `spec.syncPolicy.automated` absent → blocked, `(auto-sync off)`.
   3. otherwise classified by the age of `status.operationState.finishedAt`.
3. anything else → unknown.

- [ ] **Step 1: Write the failing test**

`internal/gitops/argo_test.go`:

```go
package gitops

import (
	"strings"
	"testing"
	"time"
)

var argoNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// argoApp builds an Application fixture. Every field a real Application carries
// that must never be rendered is present, so any test asserting on Detail also
// proves the leak boundary holds.
func argoApp(sync, phase, finishedAt string, automated bool) map[string]any {
	spec := map[string]any{
		"source": map[string]any{
			"repoURL":        "https://tok3n@git.example/org/repo.git",
			"targetRevision": "main",
			"path":           "overlays/prod",
		},
	}
	if automated {
		spec["syncPolicy"] = map[string]any{"automated": map[string]any{"prune": true}}
	}
	status := map[string]any{
		"sync": map[string]any{
			"status":   sync,
			"revision": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
		},
	}
	if phase != "" || finishedAt != "" {
		op := map[string]any{"message": "one or more objects failed to apply: https://tok3n@git.example"}
		if phase != "" {
			op["phase"] = phase
		}
		if finishedAt != "" {
			op["finishedAt"] = finishedAt
		}
		status["operationState"] = op
	}
	return map[string]any{"spec": spec, "status": status}
}

func TestAssessArgo(t *testing.T) {
	const thr = time.Hour
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
		wantIn    []string // substrings the detail must contain
	}{
		{
			name:      "synced",
			obj:       argoApp("Synced", "Succeeded", "2026-07-27T11:00:00Z", true),
			wantState: StateSynced,
		},
		{
			name:      "out of sync with auto-sync, young",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-27T11:56:00Z", true),
			wantState: StatePending,
			wantIn:    []string{"OutOfSync a1b2c3d", "last synced 4m ago"},
		},
		{
			name:      "out of sync with auto-sync, past the threshold",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", true),
			wantState: StateStale,
			wantIn:    []string{"OutOfSync a1b2c3d", "last synced 6d ago"},
		},
		{
			name:      "out of sync without auto-sync will not self-heal",
			obj:       argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false),
			wantState: StateBlocked,
			wantIn:    []string{"auto-sync off", "last synced 6d ago"},
		},
		{
			name:      "last sync operation failed",
			obj:       argoApp("OutOfSync", "Failed", "2026-07-27T11:59:00Z", true),
			wantState: StateBlocked,
			wantIn:    []string{"last sync failed"},
		},
		{
			name:      "operation error is also blocked",
			obj:       argoApp("OutOfSync", "Error", "2026-07-27T11:59:00Z", true),
			wantState: StateBlocked,
			wantIn:    []string{"last sync failed"},
		},
		{
			name:      "no operationState at all: differs but the age is unknowable",
			obj:       argoApp("OutOfSync", "", "", true),
			wantState: StatePending,
			wantIn:    []string{"age unknown"},
		},
		{
			name:      "a failed operation in the past does not taint a synced app",
			obj:       argoApp("Synced", "Failed", "2026-07-21T12:00:00Z", true),
			wantState: StateSynced,
		},
		{
			name:      "unreported sync status",
			obj:       argoApp("Unknown", "", "", true),
			wantState: StateUnknown,
		},
		{
			name:      "empty object",
			obj:       map[string]any{},
			wantState: StateUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessArgo(tt.obj, argoNow, thr)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			for _, leak := range []string{"tok3n", "git.example", "overlays/prod", "failed to apply"} {
				if strings.Contains(got.Detail, leak) {
					t.Errorf("Detail = %q leaks %q", got.Detail, leak)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/ -run TestAssessArgo -v
```

Expected: FAIL — the stub returns `unknown` for every case.

- [ ] **Step 3: Implement**

Delete the `assessArgo` stub from `internal/gitops/assess_stubs.go`, then create
`internal/gitops/argo.go`:

```go
package gitops

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assessArgo reads an Argo CD Application's convergence.
//
// Argo publishes drift directly — status.sync.status is OutOfSync or it is not —
// but it does NOT publish how long an Application has been out of sync. No such
// timestamp exists. The only honest anchor is status.operationState.finishedAt,
// when the last sync operation finished, so the detail reads "last synced 6d ago"
// and never "drifted for 6d".
func assessArgo(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	switch nestedString(obj, "status", "sync", "status") {
	case "Synced":
		return assessment{State: StateSynced}
	case "OutOfSync":
		// fall through
	default:
		return assessment{State: StateUnknown, Detail: "sync state unreported"}
	}

	head := "OutOfSync " + ShortRevision(nestedString(obj, "status", "sync", "revision"))

	// Checked only under OutOfSync: an Application that recovered from a failed
	// operation and is now Synced must not be flagged for its history.
	if phase := nestedString(obj, "status", "operationState", "phase"); phase == "Failed" || phase == "Error" {
		return assessment{State: StateBlocked, Detail: head + ", last sync failed"}
	}

	age, known := ageOf(parseTime(nestedString(obj, "status", "operationState", "finishedAt")), now)
	when := "age unknown"
	if known {
		when = "last synced " + shortAge(age) + " ago"
	}
	if !hasAutoSync(obj) {
		return assessment{State: StateBlocked, Detail: head + ", " + when + " (auto-sync off)"}
	}
	return assessment{State: byAge(age, known, threshold), Detail: head + ", " + when}
}

// hasAutoSync reports whether spec.syncPolicy.automated is present, which decides
// whether the Application can converge without a human.
//
// Reading a bool-shaped decision out of spec is established precedent —
// internal/operators reads spec.suspend the same way. Rendering a spec string is
// not, and this package renders none.
func hasAutoSync(obj map[string]any) bool {
	_, found, err := unstructured.NestedMap(obj, "spec", "syncPolicy", "automated")
	return err == nil && found
}
```

Note the import block: this file needs only `time` and the `unstructured` package.
It must compile `gofmt`-clean with no unused imports.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/gitops/
go test ./internal/gitops/ -v
```

Expected: PASS, `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/gitops/
git commit -m "feat(gitops): assess Argo CD Application convergence"
```

---

### Task 3: Flux `Kustomization` and `HelmRelease` assessment

**Files:**

- Create: `internal/gitops/flux.go`
- Delete: `internal/gitops/assess_stubs.go`
- Test: `internal/gitops/flux_test.go`

**Interfaces:**

- Consumes: `assessment`, `State` constants, `ShortRevision`, `condition`, `ageOf`,
  `byAge`, `shortAge`, `nestedString` from Task 1.
- Produces: `assessKustomization` and `assessHelmRelease`, both matching the
  `assessor` signature and already referenced by `assessorFor`.

**Rules (evaluated in this order, both kinds):**

1. `spec.suspend == true` → blocked, `suspended`.
2. `Stalled` condition `True` → blocked, `stalled: <Reason>`.
3. `Ready` condition `False` → classified by the age of that condition's
   `lastTransitionTime`; detail `not ready 3d: <Reason>`, prefixed with the revision
   pair when it differs.
4. **Kustomization only** — `status.lastAttemptedRevision` differs from
   `status.lastAppliedRevision` (both present and unequal, or attempted present with
   applied absent) → classified by the same age; detail
   `attempted a1b2c3d, applied 9f8e7d6, unchanged 5m`.
5. `Ready` condition `True` → synced.
6. no `Ready` condition → unknown.

HelmRelease skips step 4: `helm.toolkit.fluxcd.io/v2` has no
`status.lastAppliedRevision` (it existed in `v2beta1` and was removed), so there is
no honest attempted-vs-applied signal to read.

- [ ] **Step 1: Write the failing test**

`internal/gitops/flux_test.go`:

```go
package gitops

import (
	"strings"
	"testing"
	"time"
)

var fluxNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// fluxObj builds a Kustomization/HelmRelease fixture. As with the Argo fixture,
// every must-never-render field is present so the detail assertions double as a
// leak test.
func fluxObj(suspend bool, conds []map[string]any, attempted, applied string) map[string]any {
	spec := map[string]any{
		"path":      "./overlays/prod",
		"sourceRef": map[string]any{"kind": "GitRepository", "name": "tok3n-repo"},
	}
	if suspend {
		spec["suspend"] = true
	}
	status := map[string]any{}
	if len(conds) > 0 {
		items := make([]any, 0, len(conds))
		for _, c := range conds {
			items = append(items, c)
		}
		status["conditions"] = items
	}
	if attempted != "" {
		status["lastAttemptedRevision"] = attempted
	}
	if applied != "" {
		status["lastAppliedRevision"] = applied
	}
	return map[string]any{"spec": spec, "status": status}
}

func cond(condType, status, reason, at string) map[string]any {
	return map[string]any{
		"type":               condType,
		"status":             status,
		"reason":             reason,
		"message":            "failed to clone https://tok3n@git.example/org/repo",
		"lastTransitionTime": at,
	}
}

const (
	revA = "main@sha1:a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"
	revB = "main@sha1:9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
)

func TestAssessKustomization(t *testing.T) {
	const thr = time.Hour
	ready := func(status, reason, at string) []map[string]any {
		return []map[string]any{cond("Ready", status, reason, at)}
	}
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
		wantIn    []string
	}{
		{
			name:      "synced",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-27T11:00:00Z"), revA, revA),
			wantState: StateSynced,
		},
		{
			name:      "suspended will not self-heal",
			obj:       fluxObj(true, ready("True", "ReconciliationSucceeded", "2026-07-15T12:00:00Z"), revA, revA),
			wantState: StateBlocked,
			wantIn:    []string{"suspended"},
		},
		{
			name: "stalled will not self-heal",
			obj: fluxObj(false, []map[string]any{
				cond("Ready", "False", "BuildFailed", "2026-07-27T11:59:00Z"),
				cond("Stalled", "True", "InvalidPath", "2026-07-27T11:59:00Z"),
			}, revA, revB),
			wantState: StateBlocked,
			wantIn:    []string{"stalled", "InvalidPath"},
		},
		{
			name:      "not ready, younger than the threshold",
			obj:       fluxObj(false, ready("False", "BuildFailed", "2026-07-27T11:58:00Z"), revA, revB),
			wantState: StatePending,
			wantIn:    []string{"attempted a1b2c3d", "applied 9f8e7d6", "not ready 2m", "BuildFailed"},
		},
		{
			name:      "not ready for days",
			obj:       fluxObj(false, ready("False", "BuildFailed", "2026-07-24T12:00:00Z"), revA, revB),
			wantState: StateStale,
			wantIn:    []string{"not ready 3d", "BuildFailed"},
		},
		{
			name:      "not ready with no usable timestamp is never stale",
			obj:       fluxObj(false, ready("False", "BuildFailed", ""), revA, revB),
			wantState: StatePending,
			wantIn:    []string{"age unknown"},
		},
		{
			name:      "ready but the newest revision has not landed",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-24T12:00:00Z"), revA, revB),
			wantState: StateStale,
			wantIn:    []string{"attempted a1b2c3d", "applied 9f8e7d6", "unchanged 3d"},
		},
		{
			name:      "nothing has ever landed",
			obj:       fluxObj(false, ready("True", "ReconciliationSucceeded", "2026-07-27T11:55:00Z"), revA, ""),
			wantState: StatePending,
			wantIn:    []string{"attempted a1b2c3d", "applied none"},
		},
		{
			name:      "no conditions at all",
			obj:       fluxObj(false, nil, "", ""),
			wantState: StateUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessKustomization(tt.obj, fluxNow, thr)
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, want)
				}
			}
			for _, leak := range []string{"tok3n", "git.example", "overlays/prod", "failed to clone", "sha1"} {
				if strings.Contains(got.Detail, leak) {
					t.Errorf("Detail = %q leaks %q", got.Detail, leak)
				}
			}
		})
	}
}

func TestAssessHelmReleaseSkipsRevisionComparison(t *testing.T) {
	const thr = time.Hour
	// HelmRelease v2 has no status.lastAppliedRevision. An object carrying only
	// lastAttemptedRevision must still read as synced when Ready is True —
	// inventing a mismatch signal would flag every healthy release.
	obj := fluxObj(false, []map[string]any{
		cond("Ready", "True", "InstallSucceeded", "2026-07-24T12:00:00Z"),
	}, revA, "")
	if got := assessHelmRelease(obj, fluxNow, thr); got.State != StateSynced {
		t.Errorf("State = %q, want synced (detail %q)", got.State, got.Detail)
	}
	// The same object read as a Kustomization does report the mismatch.
	if got := assessKustomization(obj, fluxNow, thr); got.State != StateStale {
		t.Errorf("Kustomization State = %q, want stale", got.State)
	}
}

func TestAssessHelmReleaseConditions(t *testing.T) {
	const thr = time.Hour
	tests := []struct {
		name      string
		obj       map[string]any
		wantState State
	}{
		{"suspended", fluxObj(true, []map[string]any{cond("Ready", "True", "InstallSucceeded", "2026-07-27T11:00:00Z")}, "", ""), StateBlocked},
		{"stalled", fluxObj(false, []map[string]any{cond("Stalled", "True", "RetriesExceeded", "2026-07-27T11:00:00Z")}, "", ""), StateBlocked},
		{"not ready for days", fluxObj(false, []map[string]any{cond("Ready", "False", "UpgradeFailed", "2026-07-24T12:00:00Z")}, "", ""), StateStale},
		{"not ready, minutes old", fluxObj(false, []map[string]any{cond("Ready", "False", "UpgradeFailed", "2026-07-27T11:58:00Z")}, "", ""), StatePending},
		{"ready", fluxObj(false, []map[string]any{cond("Ready", "True", "UpgradeSucceeded", "2026-07-27T11:00:00Z")}, "", ""), StateSynced},
		{"no conditions", fluxObj(false, nil, "", ""), StateUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := assessHelmRelease(tt.obj, fluxNow, thr); got.State != tt.wantState {
				t.Errorf("State = %q, want %q (detail %q)", got.State, tt.wantState, got.Detail)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/ -run 'TestAssess(Kustomization|HelmRelease)' -v
```

Expected: FAIL — the stubs return `unknown` for every case.

- [ ] **Step 3: Implement**

Delete `internal/gitops/assess_stubs.go`, then create `internal/gitops/flux.go`:

```go
package gitops

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assessKustomization reads a Flux Kustomization's convergence.
func assessKustomization(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessFlux(obj, now, threshold, true)
}

// assessHelmRelease reads a Flux HelmRelease's convergence, minus the revision
// comparison: helm.toolkit.fluxcd.io/v2 has no status.lastAppliedRevision (it
// existed in v2beta1 and was removed), so there is no honest attempted-vs-applied
// signal to read. Inventing one would flag every healthy release.
func assessHelmRelease(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessFlux(obj, now, threshold, false)
}

// assessFlux reads the convergence both Flux kinds share.
//
// Flux never says "OutOfSync" — it reapplies continuously — so its drift signal
// is indirect: a suspended object has stopped reconciling, a stalled one has
// given up, and a Ready=False one is failing to land what Git says. The age
// anchor for all three is the Ready condition's lastTransitionTime, which is how
// long the object has held its current reconciliation state.
func assessFlux(obj map[string]any, now time.Time, threshold time.Duration, checkRevisions bool) assessment {
	if v, found, err := unstructured.NestedBool(obj, "spec", "suspend"); err == nil && found && v {
		return assessment{State: StateBlocked, Detail: "suspended"}
	}
	if status, reason, _, found := condition(obj, "Stalled"); found && status == "True" {
		return assessment{State: StateBlocked, Detail: withReason("stalled", reason)}
	}

	readyStatus, readyReason, changed, readyFound := condition(obj, "Ready")
	age, known := ageOf(changed, now)
	revisions := ""
	if checkRevisions {
		revisions = revisionPair(obj)
	}

	if readyFound && readyStatus == "False" {
		detail := withReason("not ready"+forAge(age, known), readyReason)
		if revisions != "" {
			detail = revisions + ", " + detail
		}
		return assessment{State: byAge(age, known, threshold), Detail: detail}
	}
	if revisions != "" {
		return assessment{State: byAge(age, known, threshold), Detail: revisions + ", unchanged" + forAge(age, known)}
	}
	if readyFound && readyStatus == "True" {
		return assessment{State: StateSynced}
	}
	return assessment{State: StateUnknown, Detail: "Ready not reported"}
}

// revisionPair renders the attempted/applied pair when the newest revision has
// not landed, and "" when there is nothing to say. Both values pass through
// ShortRevision: Flux writes them as "<ref>@sha1:<hash>", and <ref> is arbitrary
// user text.
func revisionPair(obj map[string]any) string {
	attempted := nestedString(obj, "status", "lastAttemptedRevision")
	applied := nestedString(obj, "status", "lastAppliedRevision")
	if attempted == "" || attempted == applied {
		return ""
	}
	shown := "none"
	if applied != "" {
		shown = ShortRevision(applied)
	}
	return "attempted " + ShortRevision(attempted) + ", applied " + shown
}

// forAge appends the age anchor, or says plainly that there is none. An unknown
// age reads as unknown; it is never silently treated as zero or as stale.
func forAge(age time.Duration, known bool) string {
	if !known {
		return ", age unknown"
	}
	return " " + shortAge(age)
}

// withReason appends the reconciler's own condition reason — a CamelCase token by
// API convention. The condition message is never appended: it is arbitrary prose
// that routinely quotes the repository URL.
func withReason(head, reason string) string {
	if reason == "" {
		return head
	}
	return head + ": " + reason
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/gitops/
go test ./internal/gitops/ -v
go build ./...
```

Expected: PASS for every test in the package; `gofmt -l` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/gitops/
git commit -m "feat(gitops): assess Flux Kustomization and HelmRelease convergence"
```

---

### Task 4: `Assess` aggregation — grouping, ordering, truncation, and the leak boundary

**Files:**

- Test: `internal/gitops/gitops_test.go` (create)
- Modify: `internal/gitops/gitops.go` only if a test exposes a defect

**Interfaces:**

- Consumes: everything from Tasks 1–3, plus `operators.Fetched` and
  `unstructured.Unstructured`.
- Produces: no new exported API. This task proves `Assess` behaves.

`Assess` was written in Task 1; this task is its test suite. If a test fails because
`Assess` is wrong, fix `Assess` — do not weaken the test.

- [ ] **Step 1: Write the failing tests**

`internal/gitops/gitops_test.go`:

```go
package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/operators"
)

var assessNow = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// adapterFor returns the gitops adapter row for a plural resource name.
func adapterFor(t *testing.T, resource string) operators.Adapter {
	t.Helper()
	for _, a := range Adapters() {
		if a.Resource == resource {
			return a
		}
	}
	t.Fatalf("no adapter for %q", resource)
	return operators.Adapter{}
}

// item wraps a fixture object with a namespace and name.
func item(ns, name string, obj map[string]any) unstructured.Unstructured {
	obj["metadata"] = map[string]any{"namespace": ns, "name": name}
	return unstructured.Unstructured{Object: obj}
}

func TestAssessGroupsByReconciler(t *testing.T) {
	fetched := []operators.Fetched{
		{
			Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
			Items: []unstructured.Unstructured{
				item("prod", "payments", argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false)),
				item("prod", "web", argoApp("Synced", "Succeeded", "2026-07-27T11:00:00Z", true)),
			},
		},
		{
			Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1",
			Items: []unstructured.Unstructured{
				item("flux-system", "infra", fluxObj(false, []map[string]any{
					cond("Ready", "False", "BuildFailed", "2026-07-24T12:00:00Z"),
				}, revA, revB)),
			},
		},
		{
			Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Items: []unstructured.Unstructured{
				item("apps", "redis", fluxObj(false, []map[string]any{
					cond("Ready", "True", "InstallSucceeded", "2026-07-27T11:00:00Z"),
				}, "", "")),
			},
		},
	}
	rep := Assess(fetched, assessNow, time.Hour)

	if rep.Threshold != "1h" {
		t.Errorf("Threshold = %q, want %q", rep.Threshold, "1h")
	}
	if len(rep.Reconcilers) != 2 {
		t.Fatalf("got %d reconcilers, want 2 (Argo CD, Flux)", len(rep.Reconcilers))
	}
	if rep.Reconcilers[0].Reconciler != "Argo CD" || rep.Reconcilers[1].Reconciler != "Flux" {
		t.Errorf("reconcilers = %q, %q; want Argo CD, Flux in adapter-table order",
			rep.Reconcilers[0].Reconciler, rep.Reconcilers[1].Reconciler)
	}
	flux := rep.Reconcilers[1]
	if len(flux.Kinds) != 2 {
		t.Errorf("Flux has %d kinds, want 2", len(flux.Kinds))
	}
	if len(flux.APIVersions) != 2 {
		t.Errorf("Flux APIVersions = %v, want both served group/versions", flux.APIVersions)
	}
	argoKind := rep.Reconcilers[0].Kinds[0]
	if argoKind.Counts[StateSynced] != 1 || argoKind.Counts[StateBlocked] != 1 {
		t.Errorf("Application counts = %v, want 1 synced + 1 blocked", argoKind.Counts)
	}
	if len(argoKind.Drifted) != 1 || argoKind.Drifted[0].Name != "payments" {
		t.Errorf("Drifted = %+v, want only the blocked Application", argoKind.Drifted)
	}
}

func TestAssessIgnoresNonGitOpsKinds(t *testing.T) {
	// main.go hands Assess the operator adapter superset when both flags are set.
	var certManager operators.Adapter
	for _, a := range operators.Adapters() {
		if a.Resource == "certificates" {
			certManager = a
		}
	}
	if certManager.Resource == "" {
		t.Fatal("operators.Adapters() no longer has a certificates row")
	}
	rep := Assess([]operators.Fetched{{
		Adapter: certManager, APIVersion: "cert-manager.io/v1",
		Items: []unstructured.Unstructured{item("shop", "web-tls", map[string]any{})},
	}}, assessNow, time.Hour)
	if len(rep.Reconcilers) != 0 {
		t.Errorf("got %+v, want no reconcilers — cert-manager is not a GitOps kind", rep.Reconcilers)
	}
}

func TestAssessOmitsEmptyKindsButKeepsDenialsAndErrors(t *testing.T) {
	rep := Assess([]operators.Fetched{
		{Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1"},
		{Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1", Forbidden: true},
		{Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Err: errors.New("Get \"https://tok3n@10.0.0.1:6443/apis\": connection refused")},
	}, assessNow, time.Hour)

	if len(rep.Reconcilers) != 2 {
		t.Fatalf("got %d reconcilers, want 2", len(rep.Reconcilers))
	}
	if len(rep.Reconcilers[0].Kinds) != 0 {
		t.Errorf("an installed but empty kind must be omitted, got %+v", rep.Reconcilers[0].Kinds)
	}
	flux := rep.Reconcilers[1]
	if len(flux.Kinds) != 2 {
		t.Fatalf("Flux has %d kinds, want the forbidden one and the failed one", len(flux.Kinds))
	}
	if !flux.Kinds[0].Forbidden {
		t.Error("forbidden kind must be kept")
	}
	if flux.Kinds[1].Error == "" {
		t.Error("failed list must be kept")
	}
	if strings.Contains(flux.Kinds[1].Error, "tok3n") {
		t.Errorf("Error = %q leaks credentials; it must go through alert.RedactError", flux.Kinds[1].Error)
	}
}

func TestAssessOrdersWorstFirstAndTruncates(t *testing.T) {
	var items []unstructured.Unstructured
	// 15 pending, then 6 blocked and 4 stale, deliberately appended after them so
	// only an ordering by severity can rescue the interesting rows from the cap.
	for i := 0; i < 15; i++ {
		items = append(items, item("aaa", fmt.Sprintf("pending-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-27T11:59:00Z", true)))
	}
	for i := 0; i < 6; i++ {
		items = append(items, item("zzz", fmt.Sprintf("blocked-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", false)))
	}
	for i := 0; i < 4; i++ {
		items = append(items, item("zzz", fmt.Sprintf("stale-%02d", i),
			argoApp("OutOfSync", "Succeeded", "2026-07-21T12:00:00Z", true)))
	}
	rep := Assess([]operators.Fetched{{
		Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1", Items: items,
	}}, assessNow, time.Hour)

	k := rep.Reconcilers[0].Kinds[0]
	if k.Total() != 25 {
		t.Errorf("Total() = %d, want 25 — counts are exact even when the list is capped", k.Total())
	}
	if len(k.Drifted) != MaxPerKind {
		t.Fatalf("enumerated %d, want %d", len(k.Drifted), MaxPerKind)
	}
	if k.Truncated != 5 {
		t.Errorf("Truncated = %d, want 5", k.Truncated)
	}
	for i := 0; i < 6; i++ {
		if k.Drifted[i].State != StateBlocked {
			t.Fatalf("Drifted[%d].State = %q, want blocked first", i, k.Drifted[i].State)
		}
	}
	for i := 6; i < 10; i++ {
		if k.Drifted[i].State != StateStale {
			t.Fatalf("Drifted[%d].State = %q, want stale after blocked", i, k.Drifted[i].State)
		}
	}
	if k.Drifted[10].State != StatePending || k.Drifted[10].Name != "pending-00" {
		t.Errorf("Drifted[10] = %+v, want pending sorted by name", k.Drifted[10])
	}
}

func TestAssessClampsNegativeThreshold(t *testing.T) {
	rep := Assess([]operators.Fetched{{
		Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
		Items: []unstructured.Unstructured{
			item("prod", "web", argoApp("OutOfSync", "Succeeded", "2026-07-27T11:59:00Z", true)),
		},
	}}, assessNow, -time.Hour)
	if rep.Threshold != "0s" {
		t.Errorf("Threshold = %q, want %q", rep.Threshold, "0s")
	}
	if got := rep.Reconcilers[0].Kinds[0].Drifted[0].State; got != StateStale {
		t.Errorf("State = %q, want stale — a zero threshold flags anything that differs", got)
	}
}

// TestAssessLeaksNothing is the boundary test: every fixture carries a repo URL
// with a token, a spec path, a branch-qualified revision, and a prose condition
// message. None may survive into the assessed report, in Go or in JSON.
func TestAssessLeaksNothing(t *testing.T) {
	fetched := []operators.Fetched{
		{Adapter: adapterFor(t, "applications"), APIVersion: "argoproj.io/v1alpha1",
			Items: []unstructured.Unstructured{
				item("prod", "payments", argoApp("OutOfSync", "Failed", "2026-07-21T12:00:00Z", false)),
			}},
		{Adapter: adapterFor(t, "kustomizations"), APIVersion: "kustomize.toolkit.fluxcd.io/v1",
			Items: []unstructured.Unstructured{
				item("flux-system", "infra", fluxObj(false, []map[string]any{
					cond("Ready", "False", "BuildFailed", "2026-07-24T12:00:00Z"),
				}, revA, revB)),
			}},
		{Adapter: adapterFor(t, "helmreleases"), APIVersion: "helm.toolkit.fluxcd.io/v2",
			Items: []unstructured.Unstructured{
				item("apps", "redis", fluxObj(true, []map[string]any{
					cond("Ready", "False", "UpgradeFailed", "2026-07-24T12:00:00Z"),
				}, revA, "")),
			}},
	}
	rep := Assess(fetched, assessNow, time.Hour)
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	haystack := fmt.Sprintf("%+v %s", rep, blob)
	for _, leak := range []string{
		"tok3n", "git.example", "overlays/prod", "./overlays/prod",
		"failed to clone", "failed to apply", "sha1:", "tok3n-repo",
		"a1b2c3d4e5f6", // the full SHA — only the 7-character prefix may appear
	} {
		if strings.Contains(haystack, leak) {
			t.Errorf("assessed report leaks %q", leak)
		}
	}
}

// TestEveryAdapterHasAFixture stops a new adapter row shipping untested: adding a
// row without teaching assessorFor about it would silently produce an empty
// section instead of a wrong one.
func TestEveryAdapterHasAFixture(t *testing.T) {
	for _, a := range Adapters() {
		if _, ok := assessorFor(a); !ok {
			t.Errorf("adapter %s/%s has no assessor — add one and a fixture test", a.Group, a.Resource)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/ -v
```

Expected: at minimum the new tests compile and run. Any failure is a real defect in
`Assess` — fix `Assess`, not the test.

- [ ] **Step 3: Fix any defect the tests expose**

No new production code is planned for this task. If `TestAssessOrdersWorstFirstAndTruncates`
or `TestAssessLeaksNothing` fails, correct `internal/gitops/gitops.go` (or the
assessor the failure points at) until it passes.

- [ ] **Step 4: Run the full package suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/gitops/ -v
go vet ./internal/gitops/
```

Expected: PASS, no vet findings.

- [ ] **Step 5: Commit**

```bash
git add internal/gitops/
git commit -m "test(gitops): aggregation, ordering, truncation, and the leak boundary"
```

---

### Task 5: Render the `GITOPS DRIFT` section

**Files:**

- Modify: `internal/report/report.go`
- Test: `internal/report/gitops_test.go` (create)

**Interfaces:**

- Consumes: `gitops.Report` and friends from Tasks 1–4.
- Produces: `report.Input.GitOps *gitops.Report` (json tag `gitops,omitempty`),
  and the unexported `gitopsRender`, `printGitOps`, `driftSummary`.

The section is modelled directly on `printOperators` (`internal/report/report.go:1188`)
— same indentation, same `%-16s` kind column, same forbidden/error handling, same
trailing blank line.

- [ ] **Step 1: Write the failing test**

`internal/report/gitops_test.go`:

```go
package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/gitops"
)

func driftFixture() *gitops.Report {
	return &gitops.Report{
		Threshold: "1h",
		Reconcilers: []gitops.ReconcilerReport{
			{
				Reconciler:  "Argo CD",
				APIVersions: []string{"argoproj.io/v1alpha1"},
				Kinds: []gitops.KindReport{{
					Kind:       "Application",
					APIVersion: "argoproj.io/v1alpha1",
					Counts: map[gitops.State]int{
						gitops.StateSynced: 14, gitops.StatePending: 1, gitops.StateBlocked: 1,
					},
					Drifted: []gitops.Workload{
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "prod", Name: "payments",
							State: gitops.StateBlocked, Detail: "OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)"},
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "staging", Name: "web",
							State: gitops.StatePending, Detail: "OutOfSync 9f8e7d6, last synced 4m ago"},
					},
				}},
			},
			{
				Reconciler:  "Flux",
				APIVersions: []string{"kustomize.toolkit.fluxcd.io/v1", "helm.toolkit.fluxcd.io/v2"},
				Kinds: []gitops.KindReport{
					{
						Kind:       "Kustomization",
						APIVersion: "kustomize.toolkit.fluxcd.io/v1",
						Counts:     map[gitops.State]int{gitops.StateSynced: 9, gitops.StateStale: 1},
						Drifted: []gitops.Workload{
							{Reconciler: "Flux", Kind: "Kustomization", Namespace: "flux-system", Name: "infra",
								State: gitops.StateStale, Detail: "attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed"},
						},
						Truncated: 2,
					},
					{Kind: "HelmRelease", APIVersion: "helm.toolkit.fluxcd.io/v2",
						Counts: map[gitops.State]int{gitops.StateSynced: 4}},
				},
			},
		},
	}
}

func TestPrintGitOps(t *testing.T) {
	var buf bytes.Buffer
	if err := printGitOps(driftFixture(), &buf); err != nil {
		t.Fatalf("printGitOps: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"GITOPS DRIFT  (advisory — reconciler-reported; threshold 1h; no repo URLs)",
		"  Argo CD (argoproj.io/v1alpha1)",
		"    Application     14 synced, 1 pending, 1 blocked",
		"      ✗ prod/payments  OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)",
		"      · staging/web  OutOfSync 9f8e7d6, last synced 4m ago",
		"  Flux (kustomize.toolkit.fluxcd.io/v1, helm.toolkit.fluxcd.io/v2)",
		"    Kustomization   9 synced, 1 stale",
		"      ✗ flux-system/infra  attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed",
		"      … +2 more",
		"    HelmRelease     4 synced",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestPrintGitOpsSkipsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := printGitOps(nil, &buf); err != nil {
		t.Fatalf("printGitOps(nil): %v", err)
	}
	if err := printGitOps(&gitops.Report{Threshold: "1h"}, &buf); err != nil {
		t.Fatalf("printGitOps(empty): %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %q, want nothing when there is no reconciler installed", buf.String())
	}
}

func TestPrintGitOpsForbiddenAndError(t *testing.T) {
	var buf bytes.Buffer
	rep := &gitops.Report{
		Threshold: "1h",
		Reconcilers: []gitops.ReconcilerReport{{
			Reconciler: "Flux",
			Kinds: []gitops.KindReport{
				{Kind: "Kustomization", Forbidden: true},
				{Kind: "HelmRelease", Error: "Get \"https://api.example\": connection refused"},
			},
		}},
	}
	if err := printGitOps(rep, &buf); err != nil {
		t.Fatalf("printGitOps: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "list forbidden — apply deploy/rbac-gitops.yaml") {
		t.Errorf("missing the RBAC hint\n%s", out)
	}
	if !strings.Contains(out, "list failed: Get \"https://api.example\": connection refused") {
		t.Errorf("missing the list failure\n%s", out)
	}
}

func TestDriftSummaryOrderIsFixed(t *testing.T) {
	k := gitops.KindReport{Counts: map[gitops.State]int{
		gitops.StateUnknown: 1, gitops.StateBlocked: 2, gitops.StateStale: 3,
		gitops.StatePending: 4, gitops.StateSynced: 5,
	}}
	const want = "5 synced, 4 pending, 3 stale, 2 blocked, 1 unknown"
	// Repeated because ranging a map would pass once and fail later.
	for i := 0; i < 20; i++ {
		if got := driftSummary(k); got != want {
			t.Fatalf("driftSummary() = %q, want %q", got, want)
		}
	}
	if got := driftSummary(gitops.KindReport{Counts: map[gitops.State]int{}}); got != "0" {
		t.Errorf("empty counts = %q, want %q", got, "0")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report/ -run 'TestPrintGitOps|TestDriftSummary' -v
```

Expected: FAIL — `printGitOps`, `driftSummary`, and the import are undefined.

- [ ] **Step 3: Implement**

In `internal/report/report.go`, add the import
`"github.com/imantaba/kubeagent/internal/gitops"` alongside the existing
`internal/operators` import.

Add the field to `Input`, immediately after the existing `Operators` field:

```go
	// GitOps is the advisory GitOps-drift view (opt-in --drift). Nil when the
	// flag is off, so a default scan's JSON is unchanged.
	GitOps *gitops.Report `json:"gitops,omitempty"`
```

Add the call immediately after the `printOperators` call (currently
`internal/report/report.go:271`):

```go
	if err := printGitOps(in.GitOps, w); err != nil {
		return err
	}
```

Add the renderer beside `printOperators`:

```go
// gitopsRender reports whether the GITOPS DRIFT section would print anything.
func gitopsRender(rep *gitops.Report) bool {
	return rep != nil && len(rep.Reconcilers) > 0
}

// printGitOps renders the advisory GITOPS DRIFT section (opt-in --drift): one
// line per reconciler, one per kind, and the objects that are not converged.
//
// Advisory, exactly like OPERATORS: it never sets hasAttention, never changes the
// cluster verdict, and takes no part in the all-clear suppression. Metadata and
// state only — no CR spec content, no condition message, and no revision that has
// not been through gitops.ShortRevision.
func printGitOps(rep *gitops.Report, w io.Writer) error {
	if !gitopsRender(rep) {
		return nil
	}
	if _, err := fmt.Fprintf(w,
		"GITOPS DRIFT  (advisory — reconciler-reported; threshold %s; no repo URLs)\n",
		rep.Threshold); err != nil {
		return err
	}
	for _, rc := range rep.Reconcilers {
		line := "  " + rc.Reconciler
		if len(rc.APIVersions) > 0 {
			line += " (" + strings.Join(rc.APIVersions, ", ") + ")"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
		for _, k := range rc.Kinds {
			switch {
			case k.Forbidden:
				if _, err := fmt.Fprintf(w, "    %-16slist forbidden — apply deploy/rbac-gitops.yaml\n", k.Kind); err != nil {
					return err
				}
				continue
			case k.Error != "":
				if _, err := fmt.Fprintf(w, "    %-16slist failed: %s\n", k.Kind, k.Error); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "    %-16s%s\n", k.Kind, driftSummary(k)); err != nil {
				return err
			}
			for _, d := range k.Drifted {
				name := d.Name
				if d.Namespace != "" {
					name = d.Namespace + "/" + d.Name
				}
				line := "      " + driftMarker(d.State) + " " + name
				if d.Detail != "" {
					line += "  " + d.Detail
				}
				if _, err := fmt.Fprintln(w, line); err != nil {
					return err
				}
			}
			if k.Truncated > 0 {
				if _, err := fmt.Fprintf(w, "      … +%d more\n", k.Truncated); err != nil {
					return err
				}
			}
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// driftMarker separates "a human must act" from "it is still converging". A
// suspended object is blocked and so carries ✗: the suspension may well be
// deliberate, but reconciliation has stopped either way.
func driftMarker(s gitops.State) string {
	switch s {
	case gitops.StateStale, gitops.StateBlocked:
		return "✗"
	default:
		return "·"
	}
}

// driftSummary renders one kind's counts in a fixed state order, omitting zeros.
// The order is a literal slice, never a range over the counts map, which would
// print differently on every run.
func driftSummary(k gitops.KindReport) string {
	order := []gitops.State{
		gitops.StateSynced, gitops.StatePending,
		gitops.StateStale, gitops.StateBlocked, gitops.StateUnknown,
	}
	var parts []string
	for _, s := range order {
		if n := k.Counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return strings.Join(parts, ", ")
}
```

Do **not** add a `hasGitOps` term to the all-clear suppression condition at
`internal/report/report.go:283`. The section is advisory and prints alongside the
all-clear line, exactly as `OPERATORS` does.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l internal/report/
go test ./internal/report/ -v
```

Expected: PASS, including the existing golden test — nothing set `Input.GitOps`, so
the golden output is unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): render the advisory GITOPS DRIFT section"
```

---

### Task 6: CLI wiring — `--drift`, `--drift-age`, and the shared fetch

**Files:**

- Modify: `main.go`
- Test: `main_test.go` (add to the existing file if one covers `envInt`; otherwise
  create `main_test.go`)

**Interfaces:**

- Consumes: `gitops.Adapters`, `gitops.Assess`, `report.Input.GitOps` from Tasks 1–5.
- Produces: the `--drift` and `--drift-age` flags, and `envDuration`.

- [ ] **Step 1: Write the failing test**

Add to `main_test.go`:

```go
func TestEnvDuration(t *testing.T) {
	const key = "KUBEAGENT_TEST_DRIFT_AGE"
	tests := []struct {
		name string
		set  string
		want time.Duration
	}{
		{"unset falls back", "", time.Hour},
		{"parses minutes", "30m", 30 * time.Minute},
		{"parses hours", "36h", 36 * time.Hour},
		{"garbage falls back", "soon", time.Hour},
		{"bare number falls back", "60", time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set == "" {
				os.Unsetenv(key)
			} else {
				t.Setenv(key, tt.set)
			}
			if got := envDuration(key, time.Hour); got != tt.want {
				t.Errorf("envDuration(%q) = %v, want %v", tt.set, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run TestEnvDuration -v
```

Expected: FAIL — `envDuration` is undefined.

- [ ] **Step 3: Implement**

Add the helper beside `envInt` in `main.go`:

```go
// envDuration returns the env var parsed as a Go duration ("30m", "2h"), else def.
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
```

Add the two flags immediately after `operatorsFlag` (`main.go:86`):

```go
	driftFlag := fs.Bool("drift", envBool("KUBEAGENT_DRIFT", false), "report GitOps convergence for Argo CD and Flux (advisory, needs deploy/rbac-gitops.yaml on a restricted context)")
	driftAge := fs.Duration("drift-age", envDuration("KUBEAGENT_DRIFT_AGE", time.Hour), "how long an object may differ from Git before --drift calls it stale (e.g. 30m, 2h)")
```

Replace the operator block (`main.go:187-199`) with:

```go
	// Operator custom resources and GitOps drift: opt-in, advisory, and built
	// lazily — a scan with neither flag constructs no dynamic client and issues no
	// discovery call.
	//
	// Both flags read the same three Argo CD/Flux kinds. operators.Adapters() is a
	// superset of gitops.Adapters(), so when both are set the cluster is listed
	// once and each assessor reads the same fetched objects.
	var operatorRep *operators.Report
	var gitopsRep *gitops.Report
	if *operatorsFlag || *driftFlag {
		dyn, disco, derr := cluster.NewDynamicClients(*kubeconfig, *contextName)
		if derr != nil {
			fmt.Fprintf(os.Stderr, "kubeagent: warning: %s unavailable: %v\n", enabledFlagNames(*operatorsFlag, *driftFlag), derr)
		} else {
			adapters := gitops.Adapters()
			if *operatorsFlag {
				adapters = operators.Adapters()
			}
			fetched := collect.OperatorResources(context.Background(), disco, dyn, adapters, namespace)
			if *operatorsFlag {
				rep := operators.Assess(fetched)
				operatorRep = &rep
			}
			if *driftFlag {
				rep := gitops.Assess(fetched, time.Now(), *driftAge)
				gitopsRep = &rep
			}
		}
	}
```

Add the small helper beside `envDuration`:

```go
// enabledFlagNames names the flags a shared failure affects, so a user running
// only one of them is not told the other is broken.
func enabledFlagNames(operators, drift bool) string {
	switch {
	case operators && drift:
		return "--operators/--drift"
	case operators:
		return "--operators"
	default:
		return "--drift"
	}
}
```

Set the report field immediately after `in.Operators = operatorRep`
(`main.go:265`):

```go
	in.GitOps = gitopsRep
```

Add the `gitops` import.

- [ ] **Step 4: Run the tests and check the flags by hand**

```bash
export PATH=$PATH:/usr/local/go/bin
gofmt -l main.go
go build ./... && go test ./...
go run . scan --help 2>&1 | grep -A2 -E '^\s+-drift'
```

Expected: all tests pass; `--drift` and `--drift-age` appear in the help with
`1h0m0s` as the default.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(scan): --drift and --drift-age, sharing one fetch with --operators"
```

---

### Task 7: RBAC manifest, golden fixture, and docs

**Files:**

- Create: `deploy/rbac-gitops.yaml`, `website/docs/features/gitops-drift.md`
- Modify: `internal/report/golden_test.go`,
  `internal/report/testdata/golden-scan.txt`, `website/mkdocs.yml`, `README.md`,
  `CHANGELOG.md`

**Interfaces:**

- Consumes: everything from Tasks 1–6.
- Produces: no Go API.

- [ ] **Step 1: Write `deploy/rbac-gitops.yaml`**

Modelled on `deploy/rbac-operators.yaml` — same flow-style sequences, same comment
voice, `list` and nothing else:

```yaml
# Opt-in add-on: grants list access to the three GitOps custom resources `scan
# --drift` reads. Apply alongside deploy/ for a restricted context; most human
# kubeconfigs already allow these. Without it, --drift still names which
# reconciler is installed (API discovery is open to every authenticated user)
# and marks each kind as forbidden — a useful answer, not an error.
#
# Its three rules are a subset of deploy/rbac-operators.yaml, so applying that
# file alone is enough to run both flags; this one exists so a drift-only user
# needs no grant on Longhorn volumes or CNPG clusters.
#
# Scan-only: the watch daemon does not read GitOps CRDs, so this is not wired
# into the Helm chart. list only — kubeagent never writes to a CRD.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubeagent-gitops
rules:
  - apiGroups: [argoproj.io]
    resources: [applications]
    verbs: [list]
  - apiGroups: [kustomize.toolkit.fluxcd.io]
    resources: [kustomizations]
    verbs: [list]
  - apiGroups: [helm.toolkit.fluxcd.io]
    resources: [helmreleases]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: kubeagent-gitops
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kubeagent-gitops
subjects:
  - kind: ServiceAccount
    name: kubeagent
    namespace: kubeagent
```

- [ ] **Step 2: Add a GitOps fixture to the golden test**

Read `internal/report/golden_test.go` and find where the `operators.Report` fixture
is built into the golden `Input`. Add a `gitops.Report` fixture in the same place,
using `driftFixture()`-shaped data but with **no** `Truncated` value and stable
content:

```go
	in.GitOps = &gitops.Report{
		Threshold: "1h",
		Reconcilers: []gitops.ReconcilerReport{
			{
				Reconciler:  "Argo CD",
				APIVersions: []string{"argoproj.io/v1alpha1"},
				Kinds: []gitops.KindReport{{
					Kind:       "Application",
					APIVersion: "argoproj.io/v1alpha1",
					Counts: map[gitops.State]int{
						gitops.StateSynced: 14, gitops.StatePending: 1, gitops.StateBlocked: 1,
					},
					Drifted: []gitops.Workload{
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "prod", Name: "payments",
							State: gitops.StateBlocked, Detail: "OutOfSync a1b2c3d, last synced 6d ago (auto-sync off)"},
						{Reconciler: "Argo CD", Kind: "Application", Namespace: "staging", Name: "web",
							State: gitops.StatePending, Detail: "OutOfSync 9f8e7d6, last synced 4m ago"},
					},
				}},
			},
			{
				Reconciler:  "Flux",
				APIVersions: []string{"kustomize.toolkit.fluxcd.io/v1", "helm.toolkit.fluxcd.io/v2"},
				Kinds: []gitops.KindReport{
					{
						Kind:       "Kustomization",
						APIVersion: "kustomize.toolkit.fluxcd.io/v1",
						Counts:     map[gitops.State]int{gitops.StateSynced: 9, gitops.StateStale: 1, gitops.StateBlocked: 1},
						Drifted: []gitops.Workload{
							{Reconciler: "Flux", Kind: "Kustomization", Namespace: "apps", Name: "web",
								State: gitops.StateBlocked, Detail: "suspended"},
							{Reconciler: "Flux", Kind: "Kustomization", Namespace: "flux-system", Name: "infra",
								State: gitops.StateStale, Detail: "attempted a1b2c3d, applied 9f8e7d6, not ready 3d: BuildFailed"},
						},
					},
					{Kind: "HelmRelease", APIVersion: "helm.toolkit.fluxcd.io/v2",
						Counts: map[gitops.State]int{gitops.StateSynced: 4}},
				},
			},
		},
	}
```

- [ ] **Step 3: Regenerate and inspect the golden file**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/report -run TestGoldenScanOutput -update
git diff internal/report/testdata/golden-scan.txt
```

Expected: the diff adds only the `GITOPS DRIFT` block after `OPERATORS`, and it
matches the section in the spec. Verify by eye that no revision longer than seven
characters and no URL appears.

```bash
go test ./internal/report/
```

Expected: PASS.

- [ ] **Step 4: Write the docs**

`website/docs/features/gitops-drift.md` — follow the structure of
`website/docs/features/operators.md` (read it first). It must cover: what the flag
answers and what it deliberately does not (no repo comparison); the five states with
the emphasis that `pending` is not a problem; the per-reconciler signal table from
the spec; `--drift-age`; the `KUBEAGENT_DRIFT`/`KUBEAGENT_DRIFT_AGE` env vars;
`deploy/rbac-gitops.yaml`; and an explicit statement that no repo URL, no `spec`
content, and no condition message is ever printed, with revisions reduced to a
7-character SHA or withheld.

Add the page to the `nav:` in `website/mkdocs.yml`, directly after the operators page.

`README.md` — add the two flags to the `scan` flag table, matching the existing rows'
wording.

`CHANGELOG.md` — under `## [Unreleased]` → `### Added`:

```markdown
- **GitOps drift (`scan --drift`, opt-in, advisory)** — a `GITOPS DRIFT` section
  answering whether the cluster is still converging on Git, for Argo CD
  `Application`s and Flux `Kustomization`s/`HelmRelease`s. Five states —
  `synced`, `pending` (differs but still converging), `stale` (differs past
  `--drift-age`, default `1h`), `blocked` (suspended, stalled, auto-sync off, or
  the last sync failed), and `unknown`. Never a finding, never affects the
  verdict or the exit code. Nothing is compared against a Git host: every signal
  is read from the reconciler's own status, and no repo URL, `spec` content, or
  condition message is ever printed — revisions are reduced to a 7-character SHA
  or withheld. Shares one fetch with `--operators` when both are set.
  `deploy/rbac-gitops.yaml` grants the `list`-only rights a restricted context
  needs.
```

- [ ] **Step 5: Verify the docs build and commit**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
(cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml)
```

Expected: "Documentation built", no `WARNING` lines naming the new page.

```bash
git add deploy/rbac-gitops.yaml website/ README.md CHANGELOG.md internal/report/
git commit -m "docs(gitops): drift feature page, RBAC manifest, and golden fixture"
```

---

### Task 8: Chaos scenario 17 — real Flux, real drift, no leaks

**Files:**

- Modify: `chaos/run.sh`, `chaos/README.md`

**Interfaces:**

- Consumes: the shipped `--drift` flag from Task 6.
- Produces: `scenario_17_gitops`, registered in `run_scenarios`.

Read `scenario_16_operators` in `chaos/run.sh` first and mirror its idiom exactly:
every `kubectl` pinned with `--context "$CTX"`, bounded waits rather than fixed
sleeps where a controller must come up, heredocs on every `apply -f -`, and a
`record` call naming the gate checks.

- [ ] **Step 1: Write the scenario**

Add before `run_scenarios`:

```bash
scenario_17_gitops() {   # real Flux -> --drift; a failing and a suspended Kustomization
  log "scenario 17: GitOps drift (--drift)"
  local ns=chaos-gitops
  local fluxurl="https://github.com/fluxcd/flux2/releases/download/v2.4.0/install.yaml"
  kubectl --context "$CTX" apply -f "$fluxurl" >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n flux-system rollout status deploy/source-controller --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" -n flux-system rollout status deploy/kustomize-controller --timeout=180s >/dev/null 2>&1 || true
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null

  # A GitRepository pointing at a host that cannot resolve: source-controller
  # fails fast with no outbound network and no dependency on a real repo, so the
  # Kustomization below settles on Ready=False within seconds. The token in the
  # URL and the distinctive path are the leak probe — neither may appear anywhere
  # in the report.
  local i
  for i in $(seq 6); do
    kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'GITREPO' && break
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: doomed
spec:
  interval: 30s
  url: https://chaosonlytoken@git.chaos.invalid/org/repo.git
  ref:
    branch: main
GITREPO
    sleep 5
  done

  kubectl --context "$CTX" -n "$ns" apply -f - >/dev/null 2>&1 <<'KS' || true
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: doomed
spec:
  interval: 30s
  path: ./overlays/chaosonlytoken
  prune: false
  sourceRef:
    kind: GitRepository
    name: doomed
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: parked
spec:
  suspend: true
  interval: 30s
  path: ./overlays/chaosonlytoken
  prune: false
  sourceRef:
    kind: GitRepository
    name: doomed
KS

  sleep 45
  local out body
  # --drift-age 10s so the 45s-old failure classifies as stale rather than as a
  # deploy that is still converging: this exercises the threshold, not just the
  # parser.
  out="$(scan --drift --drift-age 10s 2>&1 || true)"
  body="$(scan_body "$out")"
  {
    printf '%s\n' "$out"
    printf '\n--- gate checks ---\n'
    printf 'GITOPS DRIFT section:            %s\n' "$(printf '%s\n' "$body" | grep -m1 'GITOPS DRIFT' || true)"
    printf 'Kustomization line:              %s\n' "$(printf '%s\n' "$body" | grep -m1 'Kustomization' || true)"
    printf 'doomed enumerated:               %s\n' "$(printf '%s\n' "$body" | grep -c "$ns/doomed" || true)"
    printf 'parked enumerated as suspended:  %s\n' "$(printf '%s\n' "$body" | grep -cE "$ns/parked +suspended" || true)"
    printf 'repo URL or token in report:     %s\n' "$(printf '%s\n' "$body" | grep -cE 'chaosonlytoken|git\.chaos\.invalid' || true)"
    printf 'cluster verdict:                 %s\n' "$(printf '%s\n' "$body" | grep -m1 '^Cluster:' || true)"
  } | record "17. GitOps drift (--drift)" "detected: Flux Kustomization not ready + one suspended; no repo URL or token (0)"

  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
  kubectl --context "$CTX" delete -f "$fluxurl" --wait=false >/dev/null 2>&1 || true
}
```

Register it in `run_scenarios`, after `16_operators` and before `01_etcd`:

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 01_etcd)
```

- [ ] **Step 2: Verify the harness parses**

```bash
bash -n chaos/run.sh
```

Expected: no output. Then check no `apply -f -` in the new scenario lacks a heredoc
(the Task 9 defect from the previous slice):

```bash
grep -n 'apply -f -' chaos/run.sh | sed -n '1,99p'
```

Expected: every line either ends with a heredoc marker on the same line or is
immediately followed by one.

- [ ] **Step 3: Run the scenario against a live Kind cluster**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate --only 17
```

Expected: the results report records scenario 17 with `doomed` enumerated, `parked`
shown as `suspended`, and `0` for the repo-URL/token check. If `doomed` is enumerated
as `pending` rather than `stale`, the sleep is too short relative to `--drift-age` —
raise the sleep, do not raise the threshold past what the section is meant to prove.

**If the reason string differs from expectation, do not add a grep asserting on it.**
No gate check may depend on a Flux reason token; the checks above deliberately do not.

- [ ] **Step 4: Document the scenario**

Add scenario 17 to the scenario table in `chaos/README.md`, matching the existing
rows' wording, and note that it installs real Flux (pinned v2.4.0) and removes it
afterwards.

- [ ] **Step 5: Commit**

```bash
git add chaos/
git commit -m "test(chaos): scenario 17 — GitOps drift against real Flux"
```

---

## Self-Review

**Spec coverage.**

| Spec section | Task |
|---|---|
| `internal/gitops` package, `Assess`, shared fetch | 1, 4, 6 |
| `gitops.Adapters()`, discovery gate reuse | 1 |
| Argo signal table and ordering | 2 |
| Flux signal table, HelmRelease asymmetry | 3 |
| Five states, missing-timestamp discipline | 1 (`byAge`), 2, 3 |
| Revision redaction rule | 1 (`ShortRevision`), leak tests in 2, 3, 4 |
| Never reading `message`/`spec` strings | 1 (`condition`), leak tests in 2, 3, 4 |
| Report section, markers, counts order, cap, forbidden/error | 5 |
| JSON `gitops,omitempty` | 5 |
| `--drift`, `--drift-age`, env vars, lazy client | 6 |
| `deploy/rbac-gitops.yaml` | 7 |
| Golden fixture | 7 |
| Docs, README, CHANGELOG | 7 |
| Chaos scenario 17 | 8 |
| Advisory-only / no suppression change | 5 (explicit instruction) |

**Placeholders.** None: every step carries the code or the exact command.

**Type consistency.** `assessor` is
`func(map[string]any, time.Time, time.Duration) assessment` in Task 1 and every
assessor in Tasks 2–3 matches. `Assess(fetched []operators.Fetched, now time.Time,
threshold time.Duration) Report` is identical in Tasks 1, 4, and 6. `Report.Threshold`
is a `string` everywhere. `KindReport.Drifted` (not `Unhealthy`) is used consistently
in Tasks 1, 4, 5, and 7.

**Known ordering dependency.** Task 1 introduces `assess_stubs.go` so its package
compiles before Tasks 2–3 exist. Task 2 deletes one stub, Task 3 deletes the file. A
reviewer seeing the stub in Task 1's diff should expect it; seeing it survive past
Task 3 is a defect.
