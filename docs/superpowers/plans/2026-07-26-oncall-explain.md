# Rate-limited on-incident `--explain` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the watch daemon sees an object break, send a model-written
explanation as a follow-up to the page — opt-in, bounded by an explicit call
budget, and without the daemon gaining a single new cluster read.

**Architecture:** A new pure `oncall.Throttle` (per-object cooldown + global
token bucket) and an `oncall.Explainer` (bounded queue, one worker) sit beside
the reconcile loop. `applyResult` calls `ex.Consider(...)`, which throttles and
enqueues without blocking; the worker calls the model and hands the result to the
existing `alert.Sink` via the same `enqueue` seam the SLO burn alert uses. The
explainer never receives a `kubernetes.Interface`, so it structurally cannot read
the cluster.

**Tech Stack:** Go 1.26, standard library `flag`, client-go fake clientset for
I/O tests, `net/http/httptest` for endpoint tests, Helm 3, bash + python3 for the
chaos harness.

**Spec:** [docs/superpowers/specs/2026-07-26-oncall-explain-design.md](../specs/2026-07-26-oncall-explain-design.md)

## Global Constraints

- **No `Co-Authored-By: Claude` trailer** on any commit, and no Claude / Claude Code / Anthropic attribution anywhere in code, comments, docs, or changelogs.
- **No secrets, credentials, private IPs, or internal hostnames** in any file — use `<PLACEHOLDER>`.
- **Never put an API key on a command line or in `values.yaml`.** Keys come from the environment; the chart wires them with `secretKeyRef`.
- **The webhook URL is a credential.** No log line, error, metric label, results file, rendered manifest, or doc example may carry more than `scheme://host`. `alert.RedactURL` is the only way a URL reaches a log.
- **The daemon stays strictly read-only toward the cluster:** get/list/watch only, no new informers, no new RBAC verbs, no change to the chart's Role.
- **`oncall.Explainer` must never accept a `kubernetes.Interface`** in its constructor or any method. This is the structural enforcement of the read-only invariant.
- **TDD:** write the failing test first, run it, watch it fail, then implement.
- Run `export PATH=$PATH:/usr/local/go/bin` before any Go command.
- Exact defaults: cooldown `1h`, budget `20` calls/hour (also the bucket capacity), job queue size `8`, per-call timeout `60s`, `Close()` grace `5s`, latest-explanation store capped at `100` entries with a `24h` max age.
- Metric names use the existing `kubeagent_<subsystem>_…` convention. There is no `watch_` segment in any current metric name. The five new series are exactly: `kubeagent_explain_allowed_total`, `kubeagent_explain_throttled_total`, `kubeagent_explain_failed_total`, `kubeagent_explain_dropped_total`, `kubeagent_explain_budget_remaining`.
- `docs/go-concepts.md` and `docs/testing/` are **gitignored**. Edit them when the task says to, but never `git add` them.

---

## File Structure

**Create:**

| File | Responsibility |
| --- | --- |
| `internal/oncall/throttle.go` | Pure cooldown + token-bucket admission decision. No I/O, no goroutines, no clock reads. |
| `internal/oncall/throttle_test.go` | Unit tests for the above with an explicit clock. |
| `internal/oncall/oncall.go` | `Explainer`: bounded job queue, one worker, latest-per-object store, counters. |
| `internal/oncall/oncall_test.go` | Unit tests with a fake model client. |
| `internal/explain/incident.go` | `IncidentSystemPrompt`, `BuildIncidentPrompt`, `(*Client).ExplainIncident`. |
| `internal/explain/incident_test.go` | Prompt-content tests, including the egress negative assertion. |
| `chaos/explain-stub.py` | Local OpenAI-compatible `/chat/completions` stub for chaos scenario 14. |

**Modify:**

| File | Change |
| --- | --- |
| `internal/alertstate/alertstate.go` | Add `ReasonExplanation` and `Notification.Text`. |
| `internal/alert/encode.go` | Render `Text` in all three formats. |
| `internal/explain/explain.go` | `summarizer` takes a system prompt; `ExplainInventory` passes `SystemPrompt`. |
| `internal/explain/local.go` | `openaiSummarizer.summarize` takes the system prompt. |
| `internal/watch/metrics.go` | `explainSnapshot`, `updateExplain`, five series, `/explanations`. |
| `internal/watch/watch.go` | Config fields, validation, explainer lifecycle + defer order, `Consider` wiring, package doc. |
| `main.go` | Four new `watch` flags, key/endpoint presence check, usage string, Config wiring. |
| `deploy/helm/kubeagent/values.yaml` | `explain:` block. |
| `deploy/helm/kubeagent/templates/deployment.yaml` | Args + `secretKeyRef` env. |
| `deploy/helm/kubeagent/Chart.yaml` | Minor version bump (templates changed). |
| `chaos/run.sh` | Scenario 14. |
| `website/docs/watch.md`, `website/docs/roadmap.md`, `CHANGELOG.md`, `docs/go-concepts.md` | Documentation. |

---

### Task 1: Notification carries explanation text

**Files:**

- Modify: `internal/alertstate/alertstate.go` (the `Reason` const block at :45-51, the `Notification` struct at :55-63)
- Modify: `internal/alert/encode.go`
- Test: `internal/alert/encode_test.go`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces: `alertstate.ReasonExplanation Reason = "explanation"` and the field
  `Notification.Text string`. Tasks 4, 5 and 9 depend on both.

- [ ] **Step 1: Write the failing tests**

Append to `internal/alert/encode_test.go`:

```go
func TestEncodeJSONCarriesExplanationText(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Text:        "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatJSON, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Reason string `json:"reason"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Reason != "explanation" {
		t.Errorf("reason = %q, want %q", got.Reason, "explanation")
	}
	if got.Text != n.Text {
		t.Errorf("text = %q, want %q", got.Text, n.Text)
	}
}

func TestEncodeJSONOmitsEmptyText(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
	}
	body, err := encode(FormatJSON, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), `"text"`) {
		t.Errorf("empty Text must be omitted, got %s", body)
	}
}

func TestEncodeSlackRendersExplanation(t *testing.T) {
	n := alertstate.Notification{
		Object: alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status: alertstate.StatusFiring,
		Reason: alertstate.ReasonExplanation,
		Issues: []string{"ImagePullBackOff"},
		Text:   "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatSlack, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.HasPrefix(got.Text, "*EXPLANATION* Deployment/shop/web") {
		t.Errorf("slack text = %q, want it to start with the EXPLANATION header", got.Text)
	}
	if !strings.Contains(got.Text, n.Text) {
		t.Errorf("slack text = %q, want it to contain the explanation", got.Text)
	}
}

func TestEncodeAlertmanagerAnnotatesExplanation(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
		Text:        "The image tag does not exist in the registry.",
	}
	body, err := encode(FormatAlertmanager, n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got []struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d alerts, want 1", len(got))
	}
	if got[0].Annotations["explanation"] != n.Text {
		t.Errorf("explanation annotation = %q, want %q", got[0].Annotations["explanation"], n.Text)
	}
}

// Model output is untrusted text. It must survive encoding as data — never as
// markup that could restructure the payload a receiver parses.
func TestEncodeEscapesHostileModelOutput(t *testing.T) {
	hostile := "\"}]} <script>alert(1)</script>\n*not a header*\x07"
	for _, f := range []Format{FormatJSON, FormatSlack, FormatAlertmanager} {
		n := alertstate.Notification{
			Object:      alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"},
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonExplanation,
			Issues:      []string{"ImagePullBackOff"},
			FiringSince: time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC),
			Text:        hostile,
		}
		body, err := encode(f, n)
		if err != nil {
			t.Fatalf("%s: encode: %v", f, err)
		}
		var any interface{}
		if err := json.Unmarshal(body, &any); err != nil {
			t.Errorf("%s: payload is not valid JSON after hostile text: %v", f, err)
		}
	}
}
```

Ensure `internal/alert/encode_test.go` imports `encoding/json`, `strings`,
`time`, and `github.com/imantaba/kubeagent/internal/alertstate`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/ -run 'Explanation|HostileModelOutput|OmitsEmptyText' -v
```

Expected: FAIL to compile — `undefined: alertstate.ReasonExplanation` and
`unknown field Text in struct literal`.

- [ ] **Step 3: Add the Reason and the field**

In `internal/alertstate/alertstate.go`, extend the `Reason` const block:

```go
const (
	ReasonNew         Reason = "new"         // object acquired its first issue
	ReasonChanged     Reason = "changed"     // issue set changed while firing
	ReasonRepeat      Reason = "repeat"      // periodic re-send, issue set unchanged
	ReasonResolved    Reason = "resolved"    // object has no active issues
	ReasonExplanation Reason = "explanation" // model-written follow-up for an object already firing
)
```

Add the field to `Notification`, after `Reason`:

```go
	// Text is explanation prose, set only when Reason is ReasonExplanation. It
	// is model-written and therefore untrusted: every encoder must emit it as
	// JSON-encoded data, never concatenated into markup.
	Text string
```

- [ ] **Step 4: Render Text in all three formats**

In `internal/alert/encode.go`, add to `jsonPayload` after `Issues`:

```go
	Text        string   `json:"text,omitempty"`
```

and in `encodeJSON`, set it in the struct literal:

```go
		Text:        n.Text,
```

In `encodeSlack`, add a branch as the first case:

```go
	var text string
	switch {
	case n.Reason == alertstate.ReasonExplanation:
		text = fmt.Sprintf("*EXPLANATION* %s\n%s", n.Object, n.Text)
	case n.Status == alertstate.StatusResolved:
		text = fmt.Sprintf("*RESOLVED* %s (fired for %s)", n.Object, n.ResolvedAt.Sub(n.FiringSince).Round(time.Second))
	default:
		text = fmt.Sprintf("*FIRING* %s\nissues: %s\nfiring since %s",
			n.Object, strings.Join(n.Issues, ", "), n.FiringSince.UTC().Format(time.RFC3339))
		if n.Flapping {
			text += " (flapping)"
		}
	}
```

In `encodeAlertmanager`, after the `Annotations` map is built:

```go
	if n.Text != "" {
		a.Annotations["explanation"] = n.Text
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/ ./internal/alertstate/ -v
```

Expected: PASS, including every pre-existing test in both packages.

- [ ] **Step 6: Commit**

```bash
git add internal/alertstate/alertstate.go internal/alert/encode.go internal/alert/encode_test.go
git commit -m "feat(alert): carry explanation prose on a notification

Adds ReasonExplanation and Notification.Text so the watch daemon can send a
model-written follow-up through the existing sink. All three encoders emit Text
as JSON-encoded data rather than markup: model output is untrusted, and a
receiver must not be able to have its payload restructured by what the model
chose to write."
```

---

### Task 2: The throttle

**Files:**

- Create: `internal/oncall/throttle.go`
- Test: `internal/oncall/throttle_test.go`

**Interfaces:**

- Consumes: nothing.
- Produces:

```go
func NewThrottle(cooldown time.Duration, budget int) *Throttle
func (t *Throttle) Allow(key string, now time.Time) bool
func (t *Throttle) Counters() (allowed, throttled int64)
func (t *Throttle) Remaining(now time.Time) float64
```

Task 4 consumes all four.

- [ ] **Step 1: Write the failing tests**

Create `internal/oncall/throttle_test.go`:

```go
package oncall

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

func TestCooldownBlocksARepeatAndReleasesAfterTheWindow(t *testing.T) {
	th := NewThrottle(time.Hour, 100)
	if !th.Allow("a", t0) {
		t.Fatal("first call must be allowed")
	}
	if th.Allow("a", t0.Add(59*time.Minute)) {
		t.Error("a repeat inside the cooldown must be blocked")
	}
	if !th.Allow("a", t0.Add(61*time.Minute)) {
		t.Error("a repeat past the cooldown must be allowed")
	}
}

func TestZeroCooldownAllowsImmediateRepeats(t *testing.T) {
	th := NewThrottle(0, 100)
	if !th.Allow("a", t0) || !th.Allow("a", t0) {
		t.Error("a zero cooldown must not block repeats")
	}
}

func TestBudgetAllowsABurstThenDenies(t *testing.T) {
	th := NewThrottle(0, 3)
	for i := 0; i < 3; i++ {
		if !th.Allow("a", t0) {
			t.Fatalf("call %d must be inside the burst capacity", i+1)
		}
	}
	if th.Allow("a", t0) {
		t.Error("the fourth call must exhaust the bucket")
	}
}

func TestBudgetRefillsContinuously(t *testing.T) {
	th := NewThrottle(0, 20) // 20/hour = one token every 3 minutes
	for i := 0; i < 20; i++ {
		th.Allow("a", t0)
	}
	if th.Allow("a", t0.Add(2*time.Minute)) {
		t.Error("two minutes must not yet have refilled a whole token")
	}
	if !th.Allow("a", t0.Add(4*time.Minute)) {
		t.Error("four minutes must have refilled a token")
	}
}

// The check order is the property: cooldown is evaluated first and costs
// nothing, so a cooldown-blocked object cannot spend budget another object
// needs. With capacity 2, objects a and b must both get through even though a
// was asked for twice.
func TestCooldownBlockedCallDoesNotConsumeBudget(t *testing.T) {
	th := NewThrottle(time.Hour, 2)
	if !th.Allow("a", t0) {
		t.Fatal("a must be allowed")
	}
	if th.Allow("a", t0.Add(time.Minute)) {
		t.Fatal("a must be cooldown-blocked")
	}
	if !th.Allow("b", t0.Add(2*time.Minute)) {
		t.Error("b must still have budget: the blocked repeat must not have spent a token")
	}
	if th.Allow("c", t0.Add(3*time.Minute)) {
		t.Error("c must be budget-denied: capacity was 2 and two calls were allowed")
	}
}

// A budget-denied object was never explained, so it must not be stamped with a
// cooldown — it stays eligible the moment budget returns.
func TestBudgetDeniedObjectStaysEligible(t *testing.T) {
	th := NewThrottle(30*time.Minute, 1)
	if !th.Allow("a", t0) {
		t.Fatal("a must be allowed")
	}
	if th.Allow("b", t0) {
		t.Fatal("b must be budget-denied")
	}
	if !th.Allow("b", t0.Add(time.Hour)) {
		t.Error("b was never explained, so it must be eligible once the bucket refills")
	}
}

func TestCountersTrackAllowedAndThrottled(t *testing.T) {
	th := NewThrottle(time.Hour, 1)
	th.Allow("a", t0)  // allowed
	th.Allow("a", t0)  // throttled: cooldown
	th.Allow("b", t0)  // throttled: budget
	allowed, throttled := th.Counters()
	if allowed != 1 || throttled != 2 {
		t.Errorf("counters = (%d, %d), want (1, 2)", allowed, throttled)
	}
}

func TestRemainingReportsProjectedTokensWithoutMutating(t *testing.T) {
	th := NewThrottle(0, 20)
	for i := 0; i < 20; i++ {
		th.Allow("a", t0)
	}
	if got := th.Remaining(t0); got != 0 {
		t.Errorf("Remaining right after exhaustion = %g, want 0", got)
	}
	if got := th.Remaining(t0.Add(30 * time.Minute)); got < 9.9 || got > 10.1 {
		t.Errorf("Remaining after 30m = %g, want about 10", got)
	}
	if got := th.Remaining(t0.Add(10 * time.Hour)); got != 20 {
		t.Errorf("Remaining must clamp to capacity, got %g", got)
	}
	// Remaining must not have refilled the bucket as a side effect.
	if got := th.Remaining(t0); got != 0 {
		t.Errorf("Remaining mutated the bucket: re-reading at t0 gave %g, want 0", got)
	}
}

// Only allowed calls are stamped, and stamps older than the cooldown are
// pruned, so the map cannot grow without bound in a long-lived daemon.
func TestStampMapIsPruned(t *testing.T) {
	th := NewThrottle(time.Hour, 100000)
	for i := 0; i < 500; i++ {
		th.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), t0.Add(time.Duration(i)*time.Second))
	}
	before := len(th.seen)
	th.Allow("zz", t0.Add(48*time.Hour))
	if len(th.seen) >= before {
		t.Errorf("stamps older than the cooldown must be pruned: %d before, %d after", before, len(th.seen))
	}
	if len(th.seen) != 1 {
		t.Errorf("every stamp predates the cooldown window, so only the newest must remain; got %d", len(th.seen))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/oncall/ -v
```

Expected: FAIL to build — `undefined: NewThrottle`.

- [ ] **Step 3: Implement the throttle**

Create `internal/oncall/throttle.go`:

```go
// Package oncall decides which broken objects earn a model-written explanation
// and delivers those explanations without ever touching the cluster. Nothing
// here holds a Kubernetes client: the caller passes findings that have already
// been collected, which is what keeps the watch daemon's read-only guarantee a
// property of the type signatures rather than a convention.
package oncall

import "time"

// Throttle decides which objects earn a model call. It is pure — no I/O, no
// goroutines, and no wall-clock reads: the caller passes now. A Throttle is not
// safe for concurrent use; the daemon touches it only from its reconcile loop.
//
// Two guards, for two different ways spend runs away. The per-object cooldown
// stops one flapping workload from being re-explained every reconcile. The
// global token bucket bounds a mass outage, where many distinct objects each
// legitimately clear their cooldown at once.
type Throttle struct {
	cooldown time.Duration
	capacity float64
	perSec   float64
	tokens   float64
	last     time.Time
	seen     map[string]time.Time

	allowed   int64
	throttled int64
}

// NewThrottle returns a throttle with a per-object cooldown and a budget in
// calls per hour. The budget is also the bucket's capacity, so a genuine mass
// outage gets its whole allowance immediately and then drips.
func NewThrottle(cooldown time.Duration, budget int) *Throttle {
	if budget < 1 {
		budget = 1
	}
	return &Throttle{
		cooldown: cooldown,
		capacity: float64(budget),
		perSec:   float64(budget) / 3600,
		tokens:   float64(budget),
		seen:     map[string]time.Time{},
	}
}

// Allow reports whether this object may be explained now, and records the
// decision. The cooldown is checked first because it costs nothing: were the
// budget checked first, an object that is about to be refused anyway would burn
// a token some other object needs.
func (t *Throttle) Allow(key string, now time.Time) bool {
	t.refill(now)
	t.prune(now)

	if stamped, ok := t.seen[key]; ok && now.Sub(stamped) < t.cooldown {
		t.throttled++
		return false
	}
	if t.tokens < 1 {
		t.throttled++
		return false
	}
	// Stamp only on success. A budget-denied object was never explained, so
	// stamping it would silence it twice for the same refusal.
	t.tokens--
	t.seen[key] = now
	t.allowed++
	return true
}

// Counters returns the process-lifetime decision counts.
func (t *Throttle) Counters() (allowed, throttled int64) {
	return t.allowed, t.throttled
}

// Remaining projects the tokens available at now without consuming or refilling
// anything, so a metrics read can never change an admission decision.
func (t *Throttle) Remaining(now time.Time) float64 {
	if t.last.IsZero() {
		return t.capacity
	}
	r := t.tokens + now.Sub(t.last).Seconds()*t.perSec
	if r > t.capacity {
		r = t.capacity
	}
	if r < 0 {
		r = 0
	}
	return r
}

func (t *Throttle) refill(now time.Time) {
	if t.last.IsZero() {
		t.last = now
		return
	}
	if elapsed := now.Sub(t.last).Seconds(); elapsed > 0 {
		t.tokens += elapsed * t.perSec
		if t.tokens > t.capacity {
			t.tokens = t.capacity
		}
	}
	t.last = now
}

// prune drops stamps that can no longer block anything. Only allowed calls are
// stamped and every stamp expires after the cooldown, so this bounds the map at
// roughly budget-rate x cooldown entries — about 20 at the defaults.
func (t *Throttle) prune(now time.Time) {
	for k, stamped := range t.seen {
		if now.Sub(stamped) >= t.cooldown {
			delete(t.seen, k)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/oncall/ -v
```

Expected: PASS, all nine tests.

- [ ] **Step 5: Commit**

```bash
git add internal/oncall/throttle.go internal/oncall/throttle_test.go
git commit -m "feat(oncall): per-object cooldown and hourly token bucket

Two guards for two different overspend modes: the cooldown stops a flapping
object being re-explained every reconcile, the bucket bounds a mass outage where
many distinct objects each clear their cooldown at once.

The cooldown is checked before the budget so a call that would be refused anyway
cannot burn a token another object needs, and only allowed calls are stamped so
a budget-denied object is not silenced twice for one refusal. Both properties
are asserted directly, because neither is visible from the return value alone."
```

---

### Task 3: The incident prompt

**Files:**

- Create: `internal/explain/incident.go`
- Modify: `internal/explain/explain.go` (the `summarizer` interface at :64-66, `ExplainInventory` at :95-108, `anthropicSummarizer.summarize`)
- Modify: `internal/explain/local.go` (`openaiSummarizer.summarize`)
- Modify: `internal/explain/explain_test.go` and any other test in the package with a fake summarizer
- Test: `internal/explain/incident_test.go`

**Interfaces:**

- Consumes: nothing from earlier tasks.
- Produces:

```go
const IncidentSystemPrompt = "…"

func BuildIncidentPrompt(object string, issues []string, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue) string

func (c *Client) ExplainIncident(ctx context.Context, prompt string) (string, error)
```

Task 4 consumes `BuildIncidentPrompt` and `ExplainIncident`. `object` is a
pre-rendered identity string such as `"Deployment/shop/web"` — this package
deliberately does not import `alertstate`.

- [ ] **Step 1: Write the failing tests**

Create `internal/explain/incident_test.go`:

```go
package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

func incidentWorkloads() []inventory.Workload {
	return []inventory.Workload{
		{
			Namespace: "shop", Name: "web", Kind: "Deployment",
			Desired: 3, Ready: 0, Status: "Degraded", Restarts: 4,
			Findings: []diagnose.Finding{{
				Issue: "ImagePullBackOff", Reason: "tag not found", Evidence: "manifest unknown",
			}},
			RootCause: "registry ghcr.example (12 workloads failing to pull)",
			Pods: []inventory.PodRow{{
				Name: "web-7d9f-abcde", Phase: "Pending", Ready: "0/1",
				Node: "worker-2", IP: "10.244.3.17", Image: "ghcr.example/web:missing",
			}},
		},
		{
			Namespace: "shop", Name: "cart", Kind: "Deployment",
			Desired: 2, Ready: 1, Status: "Degraded",
			Findings: []diagnose.Finding{{Issue: "CrashLoopBackOff", Reason: "exit 1", Evidence: "restarts 9"}},
		},
	}
}

func TestBuildIncidentPromptNamesTheTargetObject(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	if !strings.Contains(p, "Deployment/shop/web") {
		t.Errorf("prompt must name the object that broke:\n%s", p)
	}
	if !strings.Contains(p, "ImagePullBackOff") {
		t.Errorf("prompt must carry the triggering issues:\n%s", p)
	}
}

func TestBuildIncidentPromptCarriesClusterContext(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	if !strings.Contains(p, "shop/cart") {
		t.Errorf("prompt must include the other flagged workloads so the model can correlate:\n%s", p)
	}
	if !strings.Contains(p, "registry ghcr.example") {
		t.Errorf("prompt must include the root-cause attribution kubeagent already computed:\n%s", p)
	}
}

func TestBuildIncidentPromptIncludesDegradedClusterAndServiceIssues(t *testing.T) {
	cluster := clusterhealth.ClusterHealth{
		Verdict: "Degraded", NodesReady: 2, NodesTotal: 3,
		NodeIssues: []string{"worker-2 NotReady"},
	}
	svc := []svchealth.Issue{{
		Namespace: "shop", Name: "web", Type: "ClusterIP",
		Problem: "NoEndpoints", Detail: "0 ready endpoints",
	}}
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"}, cluster, incidentWorkloads(), svc)
	if !strings.Contains(p, "DEGRADED") || !strings.Contains(p, "worker-2 NotReady") {
		t.Errorf("prompt must include the degraded cluster verdict:\n%s", p)
	}
	if !strings.Contains(p, "NoEndpoints") || !strings.Contains(p, "0 ready endpoints") {
		t.Errorf("prompt must include the service issue's problem and detail:\n%s", p)
	}
}

// The egress guard. Pod rows carry pod names, node names and pod IPs. None of
// that is needed to explain a failure, so none of it may leave the cluster. A
// positive-only test would pass just as happily if the builder started
// serializing whole pod specs.
func TestBuildIncidentPromptDoesNotLeakPodDetail(t *testing.T) {
	p := BuildIncidentPrompt("Deployment/shop/web", []string{"ImagePullBackOff"},
		clusterhealth.ClusterHealth{Verdict: "Healthy"}, incidentWorkloads(), nil)
	for _, forbidden := range []string{"10.244.3.17", "web-7d9f-abcde", "worker-2"} {
		if strings.Contains(p, forbidden) {
			t.Errorf("prompt leaked %q, which no explanation needs:\n%s", forbidden, p)
		}
	}
}

type fakeIncidentSummarizer struct {
	system string
	prompt string
	out    string
	err    error
}

func (f *fakeIncidentSummarizer) summarize(_ context.Context, system, prompt string) (string, error) {
	f.system, f.prompt = system, prompt
	return f.out, f.err
}

func TestExplainIncidentUsesTheIncidentSystemPrompt(t *testing.T) {
	f := &fakeIncidentSummarizer{out: "  the registry tag is missing  "}
	c := &Client{s: f}
	got, err := c.ExplainIncident(context.Background(), "PROMPT")
	if err != nil {
		t.Fatalf("ExplainIncident: %v", err)
	}
	if got != "the registry tag is missing" {
		t.Errorf("output = %q, want it trimmed", got)
	}
	if f.system != IncidentSystemPrompt {
		t.Errorf("system prompt = %q, want IncidentSystemPrompt", f.system)
	}
	if f.prompt != "PROMPT" {
		t.Errorf("user prompt = %q, want %q", f.prompt, "PROMPT")
	}
}

func TestExplainIncidentRejectsEmptyOutput(t *testing.T) {
	c := &Client{s: &fakeIncidentSummarizer{out: "   \n  "}}
	if _, err := c.ExplainIncident(context.Background(), "PROMPT"); err == nil {
		t.Error("an empty explanation must be an error, not a delivered blank message")
	}
}

func TestExplainIncidentPropagatesModelErrors(t *testing.T) {
	c := &Client{s: &fakeIncidentSummarizer{err: errors.New("boom")}}
	if _, err := c.ExplainIncident(context.Background(), "PROMPT"); err == nil {
		t.Error("a model error must surface")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/explain/ -run Incident -v
```

Expected: FAIL to build — `undefined: BuildIncidentPrompt` and
`undefined: IncidentSystemPrompt`.

- [ ] **Step 3: Give the summarizer a system prompt**

In `internal/explain/explain.go`, change the interface:

```go
// summarizer turns a system prompt plus a user prompt into a single plain-text
// completion. The Anthropic-backed implementation lives in this package; tests
// use a fake. The system prompt is a parameter rather than a constant because
// a one-object incident follow-up and a whole-cluster scan summary want
// different instructions.
type summarizer interface {
	summarize(ctx context.Context, system, prompt string) (string, error)
}
```

Update the `ExplainInventory` call site:

```go
	out, err := c.s.summarize(ctx, SystemPrompt, BuildInventoryPrompt(cluster, summary, facts, serviceIssues, workloads))
```

Change `anthropicSummarizer.summarize` (a **value** receiver,
`func (a anthropicSummarizer) summarize`) to take
`(ctx context.Context, system, prompt string)` and use the `system` parameter
wherever it currently references `SystemPrompt`.

In `internal/explain/local.go`, change `openaiSummarizer.summarize` to
`(ctx context.Context, system, prompt string)` and use `system` in the message
list:

```go
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: prompt},
		},
```

Update every existing fake summarizer in the package's tests to the new
signature. Find them with:

```bash
grep -rn "summarize(" internal/explain/
```

- [ ] **Step 4: Write the incident prompt**

Create `internal/explain/incident.go`:

```go
// Incident explanations for the watch daemon: one object that just broke,
// rendered together with the cluster context the daemon already holds. Only
// structured fields are sent — never pod rows, pod IPs, node names, specs, or
// logs. The daemon performs no additional cluster reads to build this.
package explain

import (
	"context"
	"fmt"
	"strings"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/remediation"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

// IncidentSystemPrompt frames a single-object follow-up. It deliberately differs
// from SystemPrompt: a page that has already fired needs the cause of one
// object's failure, not a ranked remediation plan for the whole cluster.
const IncidentSystemPrompt = `You are a senior Kubernetes SRE. An alert has just fired for ONE object. Explain
why that object broke, using ONLY the facts provided — do not invent causes,
resources, or values that are not given.

Answer in at most 120 words, as three labelled lines:

Cause: the most likely cause of THIS object's failure, in one sentence. If the
facts show the same root cause affecting other objects, say so — a shared cause
is the most useful thing you can tell the person holding the pager.
Check: one or two read-only commands to confirm it (kubectl get/describe/logs).
Fix: use the provided deterministic, pre-reviewed command verbatim. Never
substitute or invent a different command.

No preamble, no restating the input, no generic advice. Prefer "likely" over
false certainty.`

// BuildIncidentPrompt renders the object that just broke plus the surrounding
// context. object is a pre-rendered identity such as "Deployment/shop/web".
//
// Only structured fields are sent. Workload.Pods is deliberately not rendered:
// pod names, node names and pod IPs explain nothing and are exactly the kind of
// detail that should not leave a cluster.
func BuildIncidentPrompt(object string, issues []string, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Alert just fired for %s.\n", object)
	if len(issues) > 0 {
		fmt.Fprintf(&b, "Its issues: %s\n", strings.Join(issues, ", "))
	}
	b.WriteString("\n")

	if cluster.Verdict == "Degraded" {
		fmt.Fprintf(&b, "Cluster health: DEGRADED — %d/%d nodes Ready.\n", cluster.NodesReady, cluster.NodesTotal)
		for _, iss := range cluster.NodeIssues {
			fmt.Fprintf(&b, "  node %s\n", iss)
		}
		for _, iss := range cluster.SystemIssues {
			fmt.Fprintf(&b, "  system %s\n", iss)
		}
		b.WriteString("\n")
	}

	if len(flagged) > 0 {
		b.WriteString("All workloads currently flagged (this object should be among them):\n")
		for _, w := range flagged {
			fmt.Fprintf(&b, "- %s/%s (%s): %d/%d ready, status %s, %d restarts\n",
				w.Namespace, w.Name, w.Kind, w.Ready, w.Desired, w.Status, w.Restarts)
			for _, f := range w.Findings {
				fmt.Fprintf(&b, "    issue: %s — %s (%s)\n", f.Issue, f.Reason, f.Evidence)
				if f.LogCause != "" {
					fmt.Fprintf(&b, "      log cause: %s\n", f.LogCause)
				}
				s := remediation.For(f)
				fmt.Fprintf(&b, "      suggested fix (deterministic, pre-reviewed — do not substitute): %s | run: %s\n", s.NextStep, s.Command)
			}
			if w.RootCause != "" {
				fmt.Fprintf(&b, "    root cause: %s\n", w.RootCause)
			}
			if len(w.NetworkPolicies) > 0 {
				fmt.Fprintf(&b, "    network policy: pods selected by %s (possible cause)\n", strings.Join(w.NetworkPolicies, ", "))
			}
			if w.Rollout != nil {
				fmt.Fprintf(&b, "    recent change: rolled out to revision %s %s", w.Rollout.Revision, w.Rollout.Since)
				if w.Rollout.NewImage != "" {
					fmt.Fprintf(&b, ", image %s → %s", w.Rollout.OldImage, w.Rollout.NewImage)
				}
				b.WriteString("\n")
			}
		}
		b.WriteString("\n")
	}

	if len(serviceIssues) > 0 {
		b.WriteString("Service issues:\n")
		for _, is := range serviceIssues {
			// Problem is the failure ("NoEndpoints"); Type is the service kind.
			// BuildInventoryPrompt renders only Type, which loses the failure —
			// an incident explanation needs both.
			fmt.Fprintf(&b, "  - %s/%s (%s, %s): %s\n", is.Namespace, is.Name, is.Type, is.Problem, is.Detail)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Explain why %s broke, in the required three-line form.", object)
	return b.String()
}

// ExplainIncident sends an already-built incident prompt. Building is separate
// from calling so the caller can render on its own goroutine and hand the worker
// a self-contained job.
func (c *Client) ExplainIncident(ctx context.Context, prompt string) (string, error) {
	out, err := c.s.summarize(ctx, IncidentSystemPrompt, prompt)
	if err != nil {
		return "", fmt.Errorf("explaining incident: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("explaining incident: model returned no text")
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/explain/ -v
```

Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 6: Commit**

```bash
git add internal/explain/incident.go internal/explain/incident_test.go internal/explain/explain.go internal/explain/local.go internal/explain/*_test.go
git commit -m "feat(explain): incident prompt for a single broken object

A page that has already fired needs the cause of one object's failure, not a
ranked plan for the whole cluster, so the incident prompt gets its own system
prompt and the summarizer interface takes the system prompt as a parameter.

The prompt carries the surrounding context the daemon already holds — the other
flagged workloads and the root-cause attribution — so the model can say 'one of
twelve failing to pull from the same registry' rather than re-deriving it. Pod
rows are deliberately not rendered: pod names, node names and pod IPs explain
nothing and should not leave a cluster. That is asserted as a negative test,
since a positive-only test would still pass if the builder started serializing
whole pod specs."
```

---

### Task 4: The explainer

**Files:**

- Create: `internal/oncall/oncall.go`
- Test: `internal/oncall/oncall_test.go`

**Interfaces:**

- Consumes: `alertstate.ReasonExplanation`, `alertstate.Notification.Text`
  (Task 1); `NewThrottle`, `Allow`, `Counters`, `Remaining` (Task 2);
  `explain.BuildIncidentPrompt`, `(*explain.Client).ExplainIncident` (Task 3).
- Produces:

```go
type Explanation struct {
	Kind, Namespace, Name string
	Issues                []string
	ExplainedAt           time.Time
	Model                 string
	Text                  string
}

type Stats struct {
	Allowed, Throttled, Failed, Dropped int64
	BudgetRemaining                     float64
}

type Config struct {
	Client   IncidentExplainer
	Model    string
	Cooldown time.Duration
	Budget   int
	Notify   func(alertstate.Notification)
	Timeout  time.Duration
}

type IncidentExplainer interface {
	ExplainIncident(ctx context.Context, prompt string) (string, error)
}

func New(cfg Config) *Explainer
func (e *Explainer) Start(ctx context.Context)
func (e *Explainer) Close()
func (e *Explainer) Consider(d watchstate.Delta, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue, now time.Time)
func (e *Explainer) Latest() []Explanation
func (e *Explainer) Stats(now time.Time) Stats
```

Tasks 5 and 6 consume `Explanation`, `Stats`, `Latest`, and the lifecycle
methods. **Every method must be a no-op on a nil `*Explainer`**, mirroring
`*alerter` in `internal/watch/watch.go`, so the reconcile loop needs no
conditional.

- [ ] **Step 1: Write the failing tests**

Create `internal/oncall/oncall_test.go`:

```go
package oncall

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

type fakeClient struct {
	mu      sync.Mutex
	calls   int
	out     string
	err     error
	release chan struct{} // when non-nil, each call blocks until it receives
}

func (f *fakeClient) ExplainIncident(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	f.calls++
	rel := f.release
	out, err := f.out, f.err
	f.mu.Unlock()
	if rel != nil {
		<-rel
	}
	return out, err
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recorder struct {
	mu   sync.Mutex
	sent []alertstate.Notification
}

func (r *recorder) notify(n alertstate.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
}

func (r *recorder) all() []alertstate.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alertstate.Notification(nil), r.sent...)
}

func newRecord(kind, ns, name, issue string, at time.Time) watchstate.Record {
	return watchstate.Record{
		Key:         watchstate.Key{Kind: kind, Namespace: ns, Name: name, Issue: issue},
		FirstSeen:   at,
		FiringSince: at,
		LastSeen:    at,
		Active:      true,
		Firings:     1,
	}
}

func flaggedWeb() []inventory.Workload {
	return []inventory.Workload{{
		Namespace: "shop", Name: "web", Kind: "Deployment",
		Desired: 3, Ready: 0, Status: "Degraded",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff", Reason: "tag not found"}},
	}}
}

// harness builds a started explainer and returns it with its fake collaborators.
func harness(t *testing.T, cfg Config) (*Explainer, *fakeClient, *recorder, context.CancelFunc) {
	t.Helper()
	fc, ok := cfg.Client.(*fakeClient)
	if !ok {
		fc = &fakeClient{out: "because the tag is missing"}
		cfg.Client = fc
	}
	rec := &recorder{}
	cfg.Notify = rec.notify
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	e := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	return e, fc, rec, cancel
}

// waitFor polls until cond is true or the deadline passes. The worker is a real
// goroutine, so the tests need a settling point rather than a fixed sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The daemon's first reconcile sees every pre-existing issue as New. Explaining
// them would spend the whole budget re-explaining problems nobody just caused —
// the same reason a cold daemon must not page.
func TestColdStartExplainsNothing(t *testing.T) {
	e, fc, rec, cancel := harness(t, Config{Cooldown: 0, Budget: 20})
	defer func() { cancel(); e.Close() }()

	d := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
		newRecord("Deployment", "shop", "cart", "CrashLoopBackOff", t0),
	}}
	e.Consider(d, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	time.Sleep(100 * time.Millisecond)
	if fc.callCount() != 0 {
		t.Errorf("cold start made %d calls, want 0", fc.callCount())
	}
	if len(rec.all()) != 0 {
		t.Errorf("cold start sent %d notifications, want 0", len(rec.all()))
	}
}

func TestOneObjectWithTwoNewIssuesGetsOneCall(t *testing.T) {
	e, fc, rec, cancel := harness(t, Config{Cooldown: time.Hour, Budget: 20})
	defer func() { cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0) // prime
	d := watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}
	e.Consider(d, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "one notification", func() bool { return len(rec.all()) == 1 })
	if fc.callCount() != 1 {
		t.Errorf("made %d calls for one object, want 1", fc.callCount())
	}
	n := rec.all()[0]
	if n.Reason != alertstate.ReasonExplanation {
		t.Errorf("reason = %q, want %q", n.Reason, alertstate.ReasonExplanation)
	}
	if n.Status != alertstate.StatusFiring {
		t.Errorf("status = %q, want %q", n.Status, alertstate.StatusFiring)
	}
	if n.Object != (alertstate.Object{Kind: "Deployment", Namespace: "shop", Name: "web"}) {
		t.Errorf("object = %v, want Deployment/shop/web", n.Object)
	}
	if n.Text == "" {
		t.Error("explanation notification must carry Text")
	}
	if len(n.Issues) != 2 || n.Issues[0] != "Degraded" || n.Issues[1] != "ImagePullBackOff" {
		t.Errorf("issues = %v, want both, sorted", n.Issues)
	}
	if n.FiringSince != t0 {
		t.Errorf("firingSince = %v, want the object's firing time %v", n.FiringSince, t0)
	}
}

func TestModelErrorCountsAsFailedAndSendsNothing(t *testing.T) {
	e, _, rec, cancel := harness(t, Config{
		Cooldown: time.Hour, Budget: 20, Client: &fakeClient{err: errors.New("boom")},
	})
	defer func() { cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider(watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "the failure to be counted", func() bool { return e.Stats(t0).Failed == 1 })
	if got := len(rec.all()); got != 0 {
		t.Errorf("sent %d notifications after a model error, want 0", got)
	}
	if got := len(e.Latest()); got != 0 {
		t.Errorf("stored %d explanations after a model error, want 0", got)
	}
}

func TestEmptyModelOutputIsAFailure(t *testing.T) {
	e, _, rec, cancel := harness(t, Config{
		Cooldown: time.Hour, Budget: 20, Client: &fakeClient{out: "   \n "},
	})
	defer func() { cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider(watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "ImagePullBackOff", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "the empty output to be counted as failed", func() bool { return e.Stats(t0).Failed == 1 })
	if got := len(rec.all()); got != 0 {
		t.Errorf("sent %d notifications for empty output, want 0", got)
	}
}

// Queue-full and policy-refused are different causes with different operator
// responses, so they must not share a counter.
func TestQueueFullCountsDroppedNotThrottled(t *testing.T) {
	release := make(chan struct{})
	fc := &fakeClient{out: "text", release: release}
	e, _, _, cancel := harness(t, Config{Cooldown: 0, Budget: 1000, Client: fc})
	defer func() { close(release); cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)

	var recs []watchstate.Record
	for i := 0; i < 40; i++ {
		recs = append(recs, newRecord("Deployment", "shop", "w"+string(rune('a'+i)), "Degraded", t0))
	}
	e.Consider(watchstate.Delta{New: recs}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	s := e.Stats(t0)
	if s.Dropped == 0 {
		t.Errorf("40 objects against a queue of %d with a blocked worker must drop some; stats %+v", queueSize, s)
	}
	if s.Throttled != 0 {
		t.Errorf("budget was ample, so nothing may be throttled; stats %+v", s)
	}
}

func TestThrottledObjectsNeverReachTheModel(t *testing.T) {
	e, fc, _, cancel := harness(t, Config{Cooldown: time.Hour, Budget: 1})
	defer func() { cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	e.Consider(watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
		newRecord("Deployment", "shop", "cart", "Degraded", t0),
	}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0)

	waitFor(t, "the allowed call", func() bool { return e.Stats(t0).Allowed == 1 })
	time.Sleep(100 * time.Millisecond)
	if fc.callCount() != 1 {
		t.Errorf("made %d model calls with a budget of 1, want 1", fc.callCount())
	}
	if got := e.Stats(t0).Throttled; got != 1 {
		t.Errorf("throttled = %d, want 1", got)
	}
}

func TestLatestEvictsAtTheCap(t *testing.T) {
	e, _, _, cancel := harness(t, Config{Cooldown: 0, Budget: 100000})
	defer func() { cancel(); e.Close() }()

	e.Consider(watchstate.Delta{}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	for i := 0; i < maxLatest+25; i++ {
		e.Consider(watchstate.Delta{New: []watchstate.Record{
			newRecord("Deployment", "shop", "w"+string(rune('a'+i%26))+string(rune('a'+i/26)), "Degraded", t0),
		}}, clusterhealth.ClusterHealth{}, flaggedWeb(), nil, t0.Add(time.Duration(i)*time.Second))
	}
	waitFor(t, "the store to fill", func() bool { return len(e.Latest()) >= maxLatest })
	time.Sleep(100 * time.Millisecond)
	if got := len(e.Latest()); got > maxLatest {
		t.Errorf("stored %d explanations, want at most %d", got, maxLatest)
	}
}

func TestNilExplainerIsANoOp(t *testing.T) {
	var e *Explainer
	e.Start(context.Background())
	e.Consider(watchstate.Delta{New: []watchstate.Record{
		newRecord("Deployment", "shop", "web", "Degraded", t0),
	}}, clusterhealth.ClusterHealth{}, nil, nil, t0)
	if got := e.Latest(); got != nil {
		t.Errorf("Latest on a nil explainer = %v, want nil", got)
	}
	if got := (e.Stats(t0)); got != (Stats{}) {
		t.Errorf("Stats on a nil explainer = %+v, want the zero value", got)
	}
	e.Close()
}

func TestCloseBeforeStartReturnsImmediately(t *testing.T) {
	e := New(Config{Client: &fakeClient{out: "x"}, Notify: func(alertstate.Notification) {}, Budget: 1})
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close on an unstarted explainer must not block")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/oncall/ -run 'ColdStart|OneObject|ModelError|EmptyModel|QueueFull|Throttled|LatestEvicts|NilExplainer|CloseBefore' -v
```

Expected: FAIL to build — `undefined: New`, `undefined: queueSize`,
`undefined: maxLatest`.

- [ ] **Step 3: Implement the explainer**

Create `internal/oncall/oncall.go`:

```go
package oncall

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/imantaba/kubeagent/internal/alertstate"
	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/explain"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/svchealth"
	"github.com/imantaba/kubeagent/internal/watchstate"
)

const (
	// queueSize bounds admitted-but-not-yet-run jobs. Small on purpose: a
	// backlog of stale explanations helps nobody, and the drop is counted.
	queueSize = 8
	// maxLatest caps the served explanation store. An unbounded map in a
	// process designed to run for months is not an option.
	maxLatest = 100
	// maxLatestAge drops explanations nobody will look at again.
	maxLatestAge = 24 * time.Hour
	// defaultTimeout bounds one model call.
	defaultTimeout = 60 * time.Second
	// closeGrace bounds how long Close waits for the worker, matching the
	// metrics server's shutdown grace in internal/watch.
	closeGrace = 5 * time.Second
)

// IncidentExplainer is the model call. *explain.Client satisfies it; tests use a
// fake. Note what is absent: nothing in this package accepts a Kubernetes
// client, so an explainer structurally cannot read the cluster.
type IncidentExplainer interface {
	ExplainIncident(ctx context.Context, prompt string) (string, error)
}

// Explanation is one delivered explanation, as served by /explanations.
type Explanation struct {
	Kind        string
	Namespace   string
	Name        string
	Issues      []string
	ExplainedAt time.Time
	Model       string
	Text        string
}

// Stats are process-lifetime counters plus the current budget reading.
type Stats struct {
	Allowed         int64
	Throttled       int64
	Failed          int64
	Dropped         int64
	BudgetRemaining float64
}

// Config configures an Explainer.
type Config struct {
	Client   IncidentExplainer
	Model    string
	Cooldown time.Duration
	Budget   int
	Notify   func(alertstate.Notification)
	Timeout  time.Duration // 0 takes defaultTimeout
}

// job is one self-contained unit of work. The prompt is rendered on the
// reconcile goroutine so nothing the worker touches can be mutated underneath
// it by the next evaluation.
type job struct {
	obj         alertstate.Object
	issues      []string
	firingSince time.Time
	prompt      string
}

// Explainer turns object transitions into model-written follow-up
// notifications, bounded by a throttle and a small job queue. A nil *Explainer
// is a valid, inert explainer: every method is a no-op, so the reconcile loop
// needs no conditional.
type Explainer struct {
	client  IncidentExplainer
	model   string
	notify  func(alertstate.Notification)
	timeout time.Duration
	th      *Throttle
	jobs    chan job
	done    chan struct{}

	// primed guards the cold start and is touched only by Consider, on the
	// reconcile goroutine. dropped likewise.
	primed  bool
	dropped int64

	mu      sync.Mutex
	started bool
	failed  int64
	latest  map[string]Explanation
}

// New builds an Explainer. Start must be called before it does any work.
func New(cfg Config) *Explainer {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Explainer{
		client:  cfg.Client,
		model:   cfg.Model,
		notify:  cfg.Notify,
		timeout: timeout,
		th:      NewThrottle(cfg.Cooldown, cfg.Budget),
		jobs:    make(chan job, queueSize),
		done:    make(chan struct{}),
		latest:  map[string]Explanation{},
	}
}

// Start launches the single worker goroutine, which runs until ctx is
// cancelled. After the first call, Start is a no-op.
func (e *Explainer) Start(ctx context.Context) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	go func() {
		defer close(e.done)
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-e.jobs:
				e.run(ctx, j)
			}
		}
	}()
}

// Close waits for the worker to exit, up to closeGrace. The caller cancels the
// context passed to Start first. Close on an explainer whose Start was never
// called returns immediately rather than blocking forever.
func (e *Explainer) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		return
	}
	select {
	case <-e.done:
	case <-time.After(closeGrace):
		log.Printf("kubeagent: explanation worker did not stop within %s", closeGrace)
	}
}

// Consider admits the objects that just broke. It runs on the reconcile
// goroutine and never blocks: the throttle is in-memory and the send is
// non-blocking.
//
// The trigger is watchstate.Delta.New deduplicated by object. That fires for an
// already-broken object acquiring an additional issue as well as for a genuine
// clean-to-flagged transition, which the per-object cooldown absorbs: a second
// finding minutes later is inside the cooldown, and a new failure mode an hour
// later is an escalation that deserves a fresh explanation.
func (e *Explainer) Consider(d watchstate.Delta, cluster clusterhealth.ClusterHealth,
	flagged []inventory.Workload, serviceIssues []svchealth.Issue, now time.Time) {
	if e == nil {
		return
	}
	// The first reconcile is the initial snapshot, not a set of transitions.
	// Explaining it would spend the whole budget on pre-existing problems every
	// time the daemon restarts.
	if !e.primed {
		e.primed = true
		return
	}

	for _, obj := range objectsFrom(d.New) {
		if !e.th.Allow(obj.key, now) {
			continue
		}
		j := job{
			obj:         obj.obj,
			issues:      obj.issues,
			firingSince: obj.firingSince,
			prompt:      explain.BuildIncidentPrompt(obj.obj.String(), obj.issues, cluster, flagged, serviceIssues),
		}
		select {
		case e.jobs <- j:
		default:
			// Admitted but not run: the token and the cooldown stamp are
			// already spent. Counted separately from a throttle refusal
			// because the cause and the operator's response differ — a full
			// queue means the endpoint is slow, not that policy said no.
			e.dropped++
			log.Printf("kubeagent: explanation queue full, dropped %s", obj.obj)
		}
	}
}

// Latest returns the stored explanations, newest first, pruned by age.
func (e *Explainer) Latest() []Explanation {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Explanation, 0, len(e.latest))
	for _, x := range e.latest {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExplainedAt.After(out[j].ExplainedAt) })
	return out
}

// Stats returns the counters and the current budget reading. It is called from
// the reconcile goroutine, alongside the other tracker snapshots.
func (e *Explainer) Stats(now time.Time) Stats {
	if e == nil {
		return Stats{}
	}
	allowed, throttled := e.th.Counters()
	e.mu.Lock()
	failed := e.failed
	e.mu.Unlock()
	return Stats{
		Allowed:         allowed,
		Throttled:       throttled,
		Failed:          failed,
		Dropped:         e.dropped,
		BudgetRemaining: e.th.Remaining(now),
	}
}

// run performs one model call and delivers the result.
func (e *Explainer) run(ctx context.Context, j job) {
	callCtx, cancel := context.WithTimeout(ctx, e.timeout)
	text, err := e.client.ExplainIncident(callCtx, j.prompt)
	cancel()
	if err == nil && strings.TrimSpace(text) == "" {
		// Defensive: the interface is the seam, so an implementation that
		// returns blank text without an error must not become a blank page.
		err = errEmpty
	}
	if err != nil {
		e.mu.Lock()
		e.failed++
		e.mu.Unlock()
		// The error may name the endpoint; log the object and the failure
		// class only, never the error's URL or any credential.
		log.Printf("kubeagent: explanation failed for %s", j.obj)
		return
	}
	text = strings.TrimSpace(text)

	now := time.Now()
	e.store(Explanation{
		Kind: j.obj.Kind, Namespace: j.obj.Namespace, Name: j.obj.Name,
		Issues: j.issues, ExplainedAt: now, Model: e.model, Text: text,
	})
	if e.notify != nil {
		e.notify(alertstate.Notification{
			Object:      j.obj,
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonExplanation,
			Issues:      j.issues,
			FiringSince: j.firingSince,
			Text:        text,
		})
	}
}

// store records the newest explanation for an object, pruning by age and
// evicting the oldest entry once the store is full.
func (e *Explainer) store(x Explanation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, old := range e.latest {
		if x.ExplainedAt.Sub(old.ExplainedAt) > maxLatestAge {
			delete(e.latest, k)
		}
	}
	key := x.Kind + "/" + x.Namespace + "/" + x.Name
	if _, replacing := e.latest[key]; !replacing {
		for len(e.latest) >= maxLatest {
			oldestKey, oldestAt := "", time.Time{}
			for k, old := range e.latest {
				if oldestAt.IsZero() || old.ExplainedAt.Before(oldestAt) {
					oldestKey, oldestAt = k, old.ExplainedAt
				}
			}
			delete(e.latest, oldestKey)
		}
	}
	e.latest[key] = x
}

// objectRef is one object's worth of a delta, with its issues collected.
type objectRef struct {
	key         string
	obj         alertstate.Object
	issues      []string
	firingSince time.Time
}

// objectsFrom folds per-issue records into one entry per object, in a stable
// order so a storm produces a deterministic admission sequence rather than one
// that depends on map iteration.
func objectsFrom(records []watchstate.Record) []objectRef {
	index := map[string]*objectRef{}
	var order []string
	for _, r := range records {
		key := r.Key.Kind + "/" + r.Key.Namespace + "/" + r.Key.Name
		ref, ok := index[key]
		if !ok {
			ref = &objectRef{
				key: key,
				obj: alertstate.Object{Kind: r.Key.Kind, Namespace: r.Key.Namespace, Name: r.Key.Name},
			}
			index[key] = ref
			order = append(order, key)
		}
		ref.issues = append(ref.issues, r.Key.Issue)
		if ref.firingSince.IsZero() || r.FiringSince.Before(ref.firingSince) {
			ref.firingSince = r.FiringSince
		}
	}
	out := make([]objectRef, 0, len(order))
	for _, key := range order {
		ref := index[key]
		sort.Strings(ref.issues)
		out = append(out, *ref)
	}
	return out
}
```

Add the sentinel near the constants:

```go
// errEmpty marks a model response that parsed but said nothing.
var errEmpty = errors.New("model returned no text")
```

and add `"errors"` to the import block.

- [ ] **Step 4: Run the tests to verify they pass, under the race detector**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/oncall/ -race -v
```

Expected: PASS, every test, with no race reports. The worker is a real
goroutine, so `-race` is required here rather than optional.

- [ ] **Step 5: Commit**

```bash
git add internal/oncall/oncall.go internal/oncall/oncall_test.go
git commit -m "feat(oncall): bounded-queue explainer with one worker

Mirrors the alert sink's shape: Consider runs on the reconcile goroutine and
never blocks, one worker performs the model call, and a full queue drops with
its own counter rather than sharing the throttle's. Nothing in the reconcile
path waits on the model.

The first Consider is treated as the initial snapshot and explains nothing: a
restarting daemon would otherwise spend its whole budget re-explaining problems
nobody just caused. Prompts are rendered at admission so a job carries no
reference to state the next evaluation will replace. A nil *Explainer is inert,
so the reconcile loop needs no conditional."
```

---

### Task 5: Metrics and the `/explanations` endpoint

**Files:**

- Modify: `internal/watch/metrics.go` (the `metrics` struct, `render`, `handler` at :330-355, the view types at :371-400)
- Test: `internal/watch/metrics_test.go`

**Interfaces:**

- Consumes: `oncall.Stats`, `oncall.Explanation` (Task 4).
- Produces:

```go
func (m *metrics) updateExplain(enabled bool, s oncall.Stats, latest []oncall.Explanation)
```

Task 6 calls it once per reconcile.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/metrics_test.go`:

```go
func TestExplainMetricsRenderWhenEnabled(t *testing.T) {
	m := newMetrics()
	m.updateExplain(true, oncall.Stats{
		Allowed: 3, Throttled: 30, Failed: 1, Dropped: 2, BudgetRemaining: 17.5,
	}, nil)
	out := m.render()
	for _, want := range []string{
		"kubeagent_explain_allowed_total 3",
		"kubeagent_explain_throttled_total 30",
		"kubeagent_explain_failed_total 1",
		"kubeagent_explain_dropped_total 2",
		"kubeagent_explain_budget_remaining 17.5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q", want)
		}
	}
}

func TestExplainMetricsAbsentWhenDisabled(t *testing.T) {
	m := newMetrics()
	if strings.Contains(m.render(), "kubeagent_explain_") {
		t.Error("no kubeagent_explain_ series may render when --explain is off")
	}
}

func TestExplanationsEndpointServesTheStore(t *testing.T) {
	m := newMetrics()
	at := time.Date(2026, 7, 26, 10, 4, 12, 0, time.UTC)
	m.updateExplain(true, oncall.Stats{Allowed: 1, Throttled: 4, Failed: 0, Dropped: 0},
		[]oncall.Explanation{{
			Kind: "Deployment", Namespace: "shop", Name: "web",
			Issues: []string{"ImagePullBackOff"}, ExplainedAt: at,
			Model: "test-model", Text: "the tag is missing",
		}})

	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explanations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Explanations []struct {
			Kind        string   `json:"kind"`
			Namespace   string   `json:"namespace"`
			Name        string   `json:"name"`
			Issues      []string `json:"issues"`
			ExplainedAt string   `json:"explainedAt"`
			Model       string   `json:"model"`
			Text        string   `json:"text"`
		} `json:"explanations"`
		Stats struct {
			AllowedTotal   int64 `json:"allowedTotal"`
			ThrottledTotal int64 `json:"throttledTotal"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if len(got.Explanations) != 1 {
		t.Fatalf("got %d explanations, want 1", len(got.Explanations))
	}
	e := got.Explanations[0]
	if e.Kind != "Deployment" || e.Namespace != "shop" || e.Name != "web" {
		t.Errorf("object = %s/%s/%s, want Deployment/shop/web", e.Kind, e.Namespace, e.Name)
	}
	if e.Text != "the tag is missing" || e.Model != "test-model" {
		t.Errorf("text/model = %q/%q", e.Text, e.Model)
	}
	if e.ExplainedAt != "2026-07-26T10:04:12Z" {
		t.Errorf("explainedAt = %q, want RFC3339 UTC", e.ExplainedAt)
	}
	if got.Stats.AllowedTotal != 1 || got.Stats.ThrottledTotal != 4 {
		t.Errorf("stats = %+v", got.Stats)
	}
}

func TestExplanationsEndpointIsEmptyWhenDisabled(t *testing.T) {
	m := newMetrics()
	rec := httptest.NewRecorder()
	m.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explanations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Explanations []interface{} `json:"explanations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Explanations) != 0 {
		t.Errorf("got %d explanations with --explain off, want 0", len(got.Explanations))
	}
}
```

Ensure the file imports `encoding/json`, `net/http`, `net/http/httptest`,
`strings`, `time`, and `github.com/imantaba/kubeagent/internal/oncall`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run Explain -v
```

Expected: FAIL to build — `m.updateExplain undefined`.

- [ ] **Step 3: Add the snapshot, the series, and the route**

In `internal/watch/metrics.go`, add the import
`"github.com/imantaba/kubeagent/internal/oncall"`, then add the snapshot type
next to `sloSnapshot`:

```go
// explainSnapshot is the explainer's state as of the last reconcile. Enabled is
// false when --explain was not set, in which case no explain series render at
// all — the absence of the series is how an operator sees the feature is off.
type explainSnapshot struct {
	Enabled bool
	Stats   oncall.Stats
	Latest  []oncall.Explanation
}
```

Add a field to the `metrics` struct beside the other snapshots:

```go
	explain explainSnapshot
```

Add the setter beside the other `update*` methods, taking the same lock they do:

```go
// updateExplain folds the explainer's state into the served snapshot.
func (m *metrics) updateExplain(enabled bool, s oncall.Stats, latest []oncall.Explanation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.explain = explainSnapshot{Enabled: enabled, Stats: s, Latest: latest}
}
```

In `render`, immediately before the `kubeagent_last_scan_timestamp_seconds`
gauge:

```go
	if m.explain.Enabled {
		counter("kubeagent_explain_allowed_total", "Incident explanations the throttle admitted since start", m.explain.Stats.Allowed)
		counter("kubeagent_explain_throttled_total", "Incident explanations refused by the cooldown or the hourly budget", m.explain.Stats.Throttled)
		counter("kubeagent_explain_failed_total", "Incident explanations whose model call errored or returned no text", m.explain.Stats.Failed)
		counter("kubeagent_explain_dropped_total", "Incident explanations admitted but dropped because the worker queue was full", m.explain.Stats.Dropped)
		gauge("kubeagent_explain_budget_remaining", "Model calls left in the hourly budget", m.explain.Stats.BudgetRemaining)
	}
```

Add the route in `handler`, after the `/issues` block:

```go
	mux.HandleFunc("/explanations", func(w http.ResponseWriter, _ *http.Request) {
		body, err := m.explanationsJSON()
		if err != nil {
			http.Error(w, "encoding explanations", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
```

and update the `handler` doc comment to
`// handler serves /metrics, /issues, /explanations, /healthz, and /readyz.`

Add the view types beside `issueView` and the encoder beside `issuesJSON`:

```go
// explanationView is one record as served by /explanations.
type explanationView struct {
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	ExplainedAt string   `json:"explainedAt"`
	Model       string   `json:"model"`
	Text        string   `json:"text"`
}

type explainStatsView struct {
	AllowedTotal    int64   `json:"allowedTotal"`
	ThrottledTotal  int64   `json:"throttledTotal"`
	FailedTotal     int64   `json:"failedTotal"`
	DroppedTotal    int64   `json:"droppedTotal"`
	BudgetRemaining float64 `json:"budgetRemaining"`
}

type explanationsView struct {
	Explanations []explanationView `json:"explanations"`
	Stats        explainStatsView  `json:"stats"`
}

// explanationsJSON renders the latest explanation per object. With --explain off
// the list is empty rather than the endpoint absent, so a probe gets a stable
// shape either way.
func (m *metrics) explanationsJSON() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := explanationsView{
		Explanations: []explanationView{},
		Stats: explainStatsView{
			AllowedTotal:    m.explain.Stats.Allowed,
			ThrottledTotal:  m.explain.Stats.Throttled,
			FailedTotal:     m.explain.Stats.Failed,
			DroppedTotal:    m.explain.Stats.Dropped,
			BudgetRemaining: m.explain.Stats.BudgetRemaining,
		},
	}
	for _, x := range m.explain.Latest {
		issues := x.Issues
		if issues == nil {
			issues = []string{}
		}
		out.Explanations = append(out.Explanations, explanationView{
			Kind:        x.Kind,
			Namespace:   x.Namespace,
			Name:        x.Name,
			Issues:      issues,
			ExplainedAt: x.ExplainedAt.UTC().Format(time.RFC3339),
			Model:       x.Model,
			Text:        x.Text,
		})
	}
	return json.Marshal(out)
}
```

`m.mu` is a `sync.RWMutex`: the `update*` setters take `Lock`, the JSON readers
take `RLock`, and `render` takes whichever the existing code takes.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -v
```

Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/watch/metrics.go internal/watch/metrics_test.go
git commit -m "feat(watch): explain metrics and the /explanations endpoint

Five series and a JSON endpoint mirroring /issues. Throttled and dropped are
separate counters because they mean different things to whoever is on call: one
says policy refused, the other says the endpoint is slow. budget_remaining is a
gauge so 'why did my incident go unexplained' is answerable from a dashboard
rather than from logs.

With --explain off no explain series render at all, which is how an operator
sees the feature is off; the endpoint still answers, with an empty list, so a
probe gets a stable shape either way."
```

---

### Task 6: Wire the explainer into the daemon

**Files:**

- Modify: `internal/watch/watch.go` (package doc at :1-4, `Config` at :25-49, `Run` at :52-180, `applyResult` at :256-277)
- Test: `internal/watch/watch_test.go`

**Interfaces:**

- Consumes: `oncall.New`, `Start`, `Close`, `Consider`, `Latest`, `Stats`
  (Task 4); `m.updateExplain` (Task 5); `explain.NewFromConfig` (existing).
- Produces: `Config` fields `Explain bool`, `ExplainModel string`,
  `ExplainEndpoint string`, `ExplainAPIKey string`, `ExplainCooldown time.Duration`,
  `ExplainBudget int`. Task 7 sets all six.

- [ ] **Step 1: Write the failing tests**

Append to `internal/watch/watch_test.go`:

```go
func TestValidateExplainRejectsBadBudgetAndCooldown(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"zero budget", Config{Explain: true, ExplainBudget: 0, ExplainCooldown: time.Hour}, "budget"},
		{"negative budget", Config{Explain: true, ExplainBudget: -1, ExplainCooldown: time.Hour}, "budget"},
		{"negative cooldown", Config{Explain: true, ExplainBudget: 20, ExplainCooldown: -time.Second}, "cooldown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExplain(tc.cfg)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateExplainAcceptsZeroCooldownAndIsSkippedWhenOff(t *testing.T) {
	if err := validateExplain(Config{Explain: true, ExplainBudget: 1, ExplainCooldown: 0}); err != nil {
		t.Errorf("a zero cooldown is legal (budget is then the only limit): %v", err)
	}
	if err := validateExplain(Config{Explain: false, ExplainBudget: 0, ExplainCooldown: -1}); err != nil {
		t.Errorf("validation must not run when --explain is off: %v", err)
	}
}

// The explainer produces for the alert sink, so it must be fully stopped before
// the sink is closed. alert.Sink never closes its queue channel, so a late
// Enqueue does not panic — it does something quieter and worse: the
// notification lands in a buffer whose sender has already returned, and is
// never delivered and never counted as a drop. Asserting "no panic" would
// therefore prove nothing; the order itself is the assertion.
func TestRunTeardownOrderStopsTheExplainerBeforeTheSink(t *testing.T) {
	var mu sync.Mutex
	var steps []string
	teardownOrder = func(step string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
	}
	defer func() { teardownOrder = nil }()

	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run must tear down immediately

	cfg := Config{
		MetricsAddr:     "127.0.0.1:0",
		Heartbeat:       time.Hour,
		Debounce:        time.Hour,
		AlertURL:        "http://127.0.0.1:1/hook",
		AlertFormat:     "json",
		AlertRepeat:     time.Hour,
		Explain:         true,
		ExplainEndpoint: "http://127.0.0.1:1/v1",
		ExplainModel:    "test-model",
		ExplainBudget:   20,
		ExplainCooldown: time.Hour,
	}
	if err := Run(ctx, fake.NewSimpleClientset(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), steps...)
	mu.Unlock()
	want := []string{"stopExplain", "explainerClose", "stopAlerts", "sinkClose"}
	if len(got) != len(want) {
		t.Fatalf("teardown steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("teardown steps = %v, want %v", got, want)
		}
	}
}
```

Ensure the file imports `context`, `strings`, `sync`, `time`, and
`k8s.io/client-go/kubernetes/fake`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run 'ValidateExplain|TeardownOrder' -v
```

Expected: FAIL to build — `undefined: validateExplain`, `undefined: teardownOrder`,
`unknown field Explain in struct literal`.

- [ ] **Step 3: Amend the package doc and the Config**

Replace the package comment at the top of `internal/watch/watch.go`:

```go
// Package watch runs kubeagent as an in-cluster, read-only daemon: it watches the
// cluster via informers, re-runs the deterministic evaluation on change (debounced)
// and on a heartbeat, and surfaces the result as structured logs and Prometheus
// metrics. No writes. The LLM is opt-in, off by default, and sees only findings
// the daemon has already collected — it triggers no additional cluster reads and
// needs no additional RBAC.
package watch
```

Add to `Config`, after `SLOTarget`:

```go
	Explain         bool          // opt-in on-incident explanations; off by default
	ExplainModel    string        // resolved model name
	ExplainEndpoint string        // OpenAI-compatible endpoint; empty selects Anthropic
	ExplainAPIKey   string        // bearer token for a local endpoint; ignored by Anthropic
	ExplainCooldown time.Duration // per-object minimum gap between explanations
	ExplainBudget   int           // model calls per hour, and the burst capacity
```

- [ ] **Step 4: Add the validation and the lifecycle**

Add beside `validateSLOTarget`:

```go
// validateExplain checks the explanation limits. A zero cooldown is legal and
// disables the per-object gap, leaving the budget as the only limit.
func validateExplain(cfg Config) error {
	if !cfg.Explain {
		return nil
	}
	if cfg.ExplainBudget < 1 {
		return fmt.Errorf("--explain-budget must be at least 1 call per hour, got %d", cfg.ExplainBudget)
	}
	if cfg.ExplainCooldown < 0 {
		return fmt.Errorf("--explain-cooldown cannot be negative, got %s", cfg.ExplainCooldown)
	}
	return nil
}

// newExplainer builds the explainer from the config, returning nil when
// --explain is off. It is handed no Kubernetes client: the explainer sees only
// findings the daemon has already collected.
func newExplainer(ctx context.Context, cfg Config, al *alerter) *oncall.Explainer {
	if !cfg.Explain {
		return nil
	}
	ex := oncall.New(oncall.Config{
		Client:   explain.NewFromConfig(cfg.ExplainModel, cfg.ExplainEndpoint, cfg.ExplainAPIKey),
		Model:    cfg.ExplainModel,
		Cooldown: cfg.ExplainCooldown,
		Budget:   cfg.ExplainBudget,
		Notify:   al.enqueue,
	})
	ex.Start(ctx)
	backend := "anthropic"
	if cfg.ExplainEndpoint != "" {
		// The endpoint may carry a token in its URL, so only scheme://host is
		// ever logged — the same rule the alert webhook follows.
		backend = alert.RedactURL(cfg.ExplainEndpoint)
	}
	log.Printf("kubeagent: on-incident explanations enabled (model=%s, backend=%s, cooldown=%s, budget=%d/h)",
		cfg.ExplainModel, backend, cfg.ExplainCooldown, cfg.ExplainBudget)
	return ex
}

// teardownOrder records Run's teardown steps when non-nil. Run's defer order is
// load-bearing — the explainer produces for the alert sink, so it must stop
// first — and the order is otherwise unobservable from outside the function.
var teardownOrder func(step string)

func noteTeardown(step string) {
	if teardownOrder != nil {
		teardownOrder(step)
	}
}
```

Add the imports `"github.com/imantaba/kubeagent/internal/explain"` and
`"github.com/imantaba/kubeagent/internal/oncall"`.

In `Run`, add the validation call next to the existing one:

```go
	if err := validateSLOTarget(cfg.SLOTarget); err != nil {
		return err
	}
	if err := validateExplain(cfg); err != nil {
		return err
	}
```

Change the two existing alert defers to record their step, and add the explainer
block immediately after them and before `m := newMetrics()`:

```go
	if al != nil {
		defer func() { noteTeardown("sinkClose"); al.sink.Close() }() // deferred first, so it runs last
	}
	defer func() { noteTeardown("stopAlerts"); stopAlerts() }()

	// The explainer is a producer for the alert sink, so it must be stopped
	// before the sink is. Defers run LIFO, so deferring these two after the
	// alert pair puts them first on the way out: stopExplain cancels any call
	// in flight, ex.Close waits for the worker, and only then does stopAlerts
	// let the sender drain and sink.Close return. Reversed, an explanation
	// finishing during shutdown would be enqueued into a sink whose sender had
	// already returned — never delivered, and never counted as a drop.
	explainCtx, stopExplain := context.WithCancel(ctx)
	ex := newExplainer(explainCtx, cfg, al)
	if ex != nil {
		defer func() { noteTeardown("explainerClose"); ex.Close() }()
	}
	defer func() { noteTeardown("stopExplain"); stopExplain() }()
```

- [ ] **Step 5: Call Consider from applyResult**

Add the `ex *oncall.Explainer` parameter to `applyResult`, after `al`:

```go
func applyResult(m *metrics, tr *watchstate.Tracker, al *alerter, ex *oncall.Explainer, sloTr *slo.Tracker, sloN *sloNotifier, res *scan.Result, dur time.Duration, now time.Time, err error) {
```

Inside it, after `al.notify(tr, now)`:

```go
	// The object alert has already been enqueued above, LLM-free. Only now is
	// the model considered, and only for objects the throttle admits.
	ex.Consider(d, res.Health, flaggedWorkloads(res), res.ServiceIssues, now)
```

and after `m.updateAlerts(al.stats())`:

```go
	m.updateExplain(ex != nil, ex.Stats(now), ex.Latest())
```

Add the helper to `internal/watch/issues.go`, beside `issueKeys`, which already
walks `res.Inventory.Workloads` with the same predicate:

```go
// flaggedWorkloads is the evaluation's flagged workloads, which is the cluster
// context an explanation gets. Same predicate the issue tracker uses.
func flaggedWorkloads(res *scan.Result) []inventory.Workload {
	var out []inventory.Workload
	for _, w := range res.Inventory.Workloads {
		if w.Flagged() {
			out = append(out, w)
		}
	}
	return out
}
```

adding the `"github.com/imantaba/kubeagent/internal/inventory"` import to
`internal/watch/issues.go` if it is not already there. Update the `reconcile`
closure's call:

```go
		applyResult(m, tr, al, ex, sloTr, sloN, &res, time.Since(start), time.Now(), err)
```

and every other `applyResult` call site, including in tests:

```bash
grep -rn "applyResult(" internal/watch/
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./internal/watch/ -race -v
```

Expected: PASS, including every pre-existing test.

- [ ] **Step 7: Commit**

```bash
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -m "feat(watch): wire on-incident explanations into the reconcile loop

The object alert still fires first and LLM-free; only then is Consider called,
and only for objects the throttle admits. The explainer is handed no Kubernetes
client, so the daemon gains no cluster read and no RBAC verb.

Run's defer chain grows two entries, and their position is load-bearing: the
explainer produces for the alert sink, so it must stop before the sink does.
alert.Sink never closes its queue, so getting this wrong would not panic — an
explanation finishing during shutdown would land in a buffer whose sender had
already returned, never delivered and never counted as a drop. The order is
otherwise unobservable from outside Run, so a test hook records it and a test
asserts it.

The package doc's 'no LLM' sentence is corrected rather than left to rot;
'strictly read-only toward the cluster' stays literally true."
```

---

### Task 7: CLI flags

**Files:**

- Modify: `main.go` (usage string at :64, `runWatch` at :311-406)
- Test: `main_test.go`

**Interfaces:**

- Consumes: the six `watch.Config` fields from Task 6;
  `explain.ResolveModel` (existing).
- Produces: the `watch` flags `--explain`, `--explain-cooldown`,
  `--explain-budget`, `--model`, and the env vars `KUBEAGENT_EXPLAIN`,
  `KUBEAGENT_EXPLAIN_COOLDOWN`, `KUBEAGENT_EXPLAIN_BUDGET`. Task 8 sets these
  from the chart.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go`:

```go
func TestRunWatchWiresExplainConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "<PLACEHOLDER>")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")
	t.Setenv("KUBEAGENT_MODEL", "")

	var got watch.Config
	orig := watchRun
	watchRun = func(_ context.Context, _ kubernetes.Interface, cfg watch.Config) error {
		got = cfg
		return nil
	}
	defer func() { watchRun = orig }()

	if err := runWatch([]string{"--explain", "--explain-cooldown", "30m", "--explain-budget", "5", "--model", "test-model"}); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	if !got.Explain {
		t.Error("Explain must be true")
	}
	if got.ExplainCooldown != 30*time.Minute {
		t.Errorf("cooldown = %s, want 30m", got.ExplainCooldown)
	}
	if got.ExplainBudget != 5 {
		t.Errorf("budget = %d, want 5", got.ExplainBudget)
	}
	if got.ExplainModel != "test-model" {
		t.Errorf("model = %q, want test-model", got.ExplainModel)
	}
}

func TestRunWatchDefaultsExplainOff(t *testing.T) {
	var got watch.Config
	orig := watchRun
	watchRun = func(_ context.Context, _ kubernetes.Interface, cfg watch.Config) error {
		got = cfg
		return nil
	}
	defer func() { watchRun = orig }()

	if err := runWatch(nil); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	if got.Explain {
		t.Error("--explain must be off by default")
	}
	if got.ExplainCooldown != time.Hour {
		t.Errorf("default cooldown = %s, want 1h", got.ExplainCooldown)
	}
	if got.ExplainBudget != 20 {
		t.Errorf("default budget = %d, want 20", got.ExplainBudget)
	}
}

// A config error must surface before the daemon starts, not after the metrics
// server is listening and a cache sync is underway.
func TestRunWatchExplainWithoutCredentialsFailsFast(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "")

	orig := watchRun
	watchRun = func(context.Context, kubernetes.Interface, watch.Config) error {
		t.Fatal("the daemon must not start without credentials")
		return nil
	}
	defer func() { watchRun = orig }()

	err := runWatch([]string{"--explain"})
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("error = %q, want it to name the missing credential", err)
	}
}

func TestRunWatchLocalEndpointNeedsAModelName(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("KUBEAGENT_EXPLAIN_ENDPOINT", "http://127.0.0.1:11434/v1")
	t.Setenv("KUBEAGENT_MODEL", "")

	orig := watchRun
	watchRun = func(context.Context, kubernetes.Interface, watch.Config) error {
		t.Fatal("the daemon must not start without a model name")
		return nil
	}
	defer func() { watchRun = orig }()

	if err := runWatch([]string{"--explain"}); err == nil {
		t.Fatal("want an error naming --model, got nil")
	}
}

func TestUsageMentionsTheExplainFlags(t *testing.T) {
	err := run(nil)
	if err == nil {
		t.Fatal("want the usage error")
	}
	for _, want := range []string{"--explain-cooldown", "--explain-budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("usage does not mention %s", want)
		}
	}
}
```

Match the existing tests' pattern for stubbing `watchRun` and for whatever the
top-level entry point is called; if `run` takes different arguments, adapt
`TestUsageMentionsTheExplainFlags` to however the existing tests trigger the
usage error.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test . -run 'Explain|Usage' -v
```

Expected: FAIL — unknown flag `--explain`, and the usage string does not mention
the new flags.

- [ ] **Step 3: Add the flags and the credential check**

In `runWatch`, after the `sloTarget` flag:

```go
	explainFlag := fs.Bool("explain", envBool("KUBEAGENT_EXPLAIN", false), "explain new incidents via one LLM call each (needs ANTHROPIC_API_KEY, or KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model)")
	explainCooldown := fs.Duration("explain-cooldown", envDur("KUBEAGENT_EXPLAIN_COOLDOWN", time.Hour), "minimum gap between explanations for the same object (0 = no per-object gap)")
	explainBudget := fs.Int("explain-budget", envInt("KUBEAGENT_EXPLAIN_BUDGET", 20), "model calls per hour, and the burst capacity")
	model := fs.String("model", "", "model for --explain (default: $KUBEAGENT_MODEL or claude-opus-4-8; the local model name when KUBEAGENT_EXPLAIN_ENDPOINT is set)")
```

After `fs.Parse` and beside the alert-URL block:

```go
	// --explain needs Anthropic, or a local OpenAI-compatible endpoint. Check
	// before connecting: a credential error must not surface as a daemon that
	// looks up and then silently never explains anything.
	explainEndpoint := os.Getenv("KUBEAGENT_EXPLAIN_ENDPOINT")
	var explainModel string
	if *explainFlag {
		if explainEndpoint == "" && os.Getenv("ANTHROPIC_API_KEY") == "" {
			return fmt.Errorf("--explain needs ANTHROPIC_API_KEY, or set KUBEAGENT_EXPLAIN_ENDPOINT for a local OpenAI-compatible model")
		}
		if explainEndpoint != "" {
			explainModel = firstNonEmpty(*model, os.Getenv("KUBEAGENT_MODEL")) // no Anthropic default for a local model
			if explainModel == "" {
				return fmt.Errorf("--explain with KUBEAGENT_EXPLAIN_ENDPOINT needs --model (or KUBEAGENT_MODEL) set to the local model name")
			}
		} else {
			explainModel = explain.ResolveModel(*model, os.Getenv("KUBEAGENT_MODEL"))
		}
	}
```

Add to the `watch.Config` literal, after `SLOTarget`:

```go
		Explain:                 *explainFlag,
		ExplainModel:            explainModel,
		ExplainEndpoint:         explainEndpoint,
		ExplainAPIKey:           os.Getenv("KUBEAGENT_EXPLAIN_API_KEY"),
		ExplainCooldown:         *explainCooldown,
		ExplainBudget:           *explainBudget,
```

- [ ] **Step 4: Update the usage string**

In the usage error at `main.go:64`, replace the `watch` clause

```text
| kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] |
```

with

```text
| kubeagent watch [--kubeconfig path] [--context name] [-n namespace] [--metrics-addr addr] [--heartbeat dur] [--debounce dur] [--alert-format json|slack|alertmanager] [--alert-repeat dur] [--slo-target pct] [--explain [--explain-cooldown dur] [--explain-budget n] [--model name]] |
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test . -v
```

Expected: PASS, including every pre-existing test.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat(cli): watch --explain, --explain-cooldown, --explain-budget, --model

Off by default. Missing credentials are a startup error rather than a daemon
that comes up healthy and silently never explains anything, and a local
endpoint without a model name is refused the same way scan --explain refuses it.

The API key is never a flag: it comes from the environment, so it stays out of
the pod spec's args and out of ps output — the same rule the alert webhook URL
already follows."
```

---

### Task 8: Helm chart

**Files:**

- Modify: `deploy/helm/kubeagent/values.yaml`
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml`
- Modify: `deploy/helm/kubeagent/Chart.yaml`
- Test: manual `helm lint` / `helm template` assertions in the steps below

**Interfaces:**

- Consumes: the flags and env vars from Task 7.
- Produces: the `explain:` values block.

- [ ] **Step 1: Write the failing check**

Run the assertions that must hold at the end of this task. They fail now:

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin
helm template x deploy/helm/kubeagent --set explain.enabled=true \
  --set explain.existingSecret=kubeagent-llm | grep -E '"--explain|ANTHROPIC_API_KEY'
```

Expected: no output — the chart does not know about `explain` yet.

- [ ] **Step 2: Add the values block**

Append to `deploy/helm/kubeagent/values.yaml`, following the style of the
existing `alerts:` block:

```yaml
# On-incident explanations. Off by default: this is the only feature that sends
# anything to a model endpoint. It performs no additional cluster reads and needs
# no additional RBAC — the model sees only findings the daemon already collected.
#
# The API key is NEVER a value here. A values file lands in Git, in
# `helm get values`, and in CI logs. Create a Secret and name it in
# existingSecret:
#
#   kubectl -n kubeagent create secret generic kubeagent-llm \
#     --from-literal=apiKey=<PLACEHOLDER>
explain:
  enabled: false
  # Model name. Required when endpoint is set; otherwise defaults to the
  # kubeagent default (claude-opus-4-8).
  model: ""
  # Minimum gap between explanations for the same object. 0 disables the
  # per-object gap, leaving budget as the only limit.
  cooldown: 1h
  # Model calls per hour, and the burst capacity.
  budget: 20
  # OpenAI-compatible endpoint for a local model, e.g.
  # "http://ollama.llm.svc.cluster.local:11434/v1". Empty selects Anthropic.
  endpoint: ""
  # Secret holding the API key. Required when enabled and endpoint is empty.
  existingSecret: ""
  # Key within that Secret. Read into ANTHROPIC_API_KEY when endpoint is empty,
  # and into KUBEAGENT_EXPLAIN_API_KEY when it is set.
  secretKey: apiKey
```

- [ ] **Step 3: Add the args and env to the deployment template**

In `deploy/helm/kubeagent/templates/deployment.yaml`, after the `slo` args
block and before `env:`:

```yaml
            {{- if .Values.explain.enabled }}
            {{- if and (not .Values.explain.endpoint) (not .Values.explain.existingSecret) }}
            {{- fail "explain.existingSecret is required when explain.enabled is true and explain.endpoint is empty (the API key must come from a Secret, never from values.yaml)" }}
            {{- end }}
            {{- if and .Values.explain.endpoint (not .Values.explain.model) }}
            {{- fail "explain.model is required when explain.endpoint is set (a local endpoint has no default model name)" }}
            {{- end }}
            - "--explain"
            - "--explain-cooldown={{ .Values.explain.cooldown }}"
            - "--explain-budget={{ .Values.explain.budget }}"
            {{- if .Values.explain.model }}
            - "--model={{ .Values.explain.model }}"
            {{- end }}
            {{- end }}
```

and inside `env:`, after the `alerts` block:

```yaml
            {{- if .Values.explain.enabled }}
            {{- if .Values.explain.endpoint }}
            - name: KUBEAGENT_EXPLAIN_ENDPOINT
              value: {{ .Values.explain.endpoint | quote }}
            {{- if .Values.explain.existingSecret }}
            - name: KUBEAGENT_EXPLAIN_API_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.explain.existingSecret | quote }}
                  key: {{ .Values.explain.secretKey | quote }}
            {{- end }}
            {{- else }}
            - name: ANTHROPIC_API_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.explain.existingSecret | quote }}
                  key: {{ .Values.explain.secretKey | quote }}
            {{- end }}
            {{- end }}
```

- [ ] **Step 4: Bump the chart version**

Chart templates and values changed, so this is a **minor** bump, not the patch
the release script would apply. In `deploy/helm/kubeagent/Chart.yaml`:

```yaml
version: 0.21.0
```

Leave `appVersion` alone — the release script owns it.

- [ ] **Step 5: Verify the chart**

```bash
export PATH=$PATH:$HOME/.local/bin:/usr/local/bin

# Lints clean.
helm lint deploy/helm/kubeagent

# Off by default: no explain flags, no key env.
helm template x deploy/helm/kubeagent | grep -cE '\-\-explain|ANTHROPIC_API_KEY' || echo "0 (correct)"

# Anthropic path: flags present, key from the Secret, never inline.
helm template x deploy/helm/kubeagent --set explain.enabled=true \
  --set explain.existingSecret=kubeagent-llm | grep -A4 -E '"--explain"|ANTHROPIC_API_KEY'

# Refused without a Secret.
helm template x deploy/helm/kubeagent --set explain.enabled=true 2>&1 | grep -q 'existingSecret is required' \
  && echo "correctly refused"

# Local endpoint without a model name is refused.
helm template x deploy/helm/kubeagent --set explain.enabled=true \
  --set explain.endpoint=http://127.0.0.1:11434/v1 2>&1 | grep -q 'explain.model is required' \
  && echo "correctly refused"
```

Expected: `helm lint` passes; the default render has no explain flags; the
Anthropic render shows `"--explain"`, `"--explain-cooldown=1h"`,
`"--explain-budget=20"` and an `ANTHROPIC_API_KEY` sourced via `secretKeyRef`
with **no literal key anywhere**; both `fail` guards trigger.

- [ ] **Step 6: Commit**

```bash
git add deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/helm/kubeagent/Chart.yaml
git commit -m "feat(helm): explain values, secretKeyRef key wiring, minor chart bump

The API key is never a value. A values file lands in Git, in helm get values,
and in CI logs, so the chart takes the name of a Secret the operator creates and
refuses to render without one. A local endpoint without a model name is refused
the same way, since there is no sensible default for someone else's model.

Templates and values changed, so the chart takes a minor bump rather than the
patch the release script applies by default."
```

---

### Task 9: Chaos scenario 14

**Files:**

- Create: `chaos/explain-stub.py`
- Modify: `chaos/run.sh`

**Interfaces:**

- Consumes: everything above.
- Produces: chaos scenario 14, run by `./chaos/run.sh --only 14`.

- [ ] **Step 1: Write the stub endpoint**

Create `chaos/explain-stub.py`, modelled on `chaos/alert-receiver.py`:

```python
#!/usr/bin/env python3
"""Minimal OpenAI-compatible /chat/completions stub for the chaos harness.

Lets scenario 14 exercise the full on-incident explain path end to end with no
API key anywhere in the shell. Every request is logged one-per-line to the given
file so the scenario can count the calls the daemon actually made.

Usage: explain-stub.py PORT LOGFILE
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1])
LOGFILE = sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
            user = next(
                (m.get("content", "") for m in reversed(body.get("messages", []))
                 if m.get("role") == "user"),
                "",
            )
        except (ValueError, AttributeError):
            user = ""
        with open(LOGFILE, "a", encoding="utf-8") as fh:
            fh.write(json.dumps({"path": self.path, "prompt": user}) + "\n")

        payload = json.dumps({
            "choices": [{"message": {
                "role": "assistant",
                "content": "Cause: the deployment references an image tag that does not exist.\n"
                           "Check: kubectl -n NS describe deploy/NAME\n"
                           "Fix: kubectl -n NS set image deploy/NAME container=<known-good-tag>",
            }}]
        }).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args):
        pass


HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
```

```bash
chmod +x chaos/explain-stub.py
```

- [ ] **Step 2: Add the scenario**

In `chaos/run.sh`, add `scenario_14` immediately after `scenario_12`'s
definition, following its structure exactly:

```bash
scenario_14() {
  log "scenario 14: on-incident explanations (budget, throttle, /explanations)"
  local ns=chaos-explain port=18090 aport=18091 sport=18092
  local wlog alerts calls wpid apid spid i expl

  wlog="$(mktemp)"; alerts="$(mktemp)"; calls="$(mktemp)"

  # A local receiver proves the notification path; a local OpenAI-compatible
  # stub proves the model path. No API key is involved anywhere in this
  # scenario — the endpoint is the only backend the daemon talks to.
  python3 chaos/alert-receiver.py "$aport" "$alerts" >/dev/null 2>&1 &
  apid=$!
  python3 chaos/explain-stub.py "$sport" "$calls" >/dev/null 2>&1 &
  spid=$!

  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  # Budget 1 with no per-object cooldown: two objects break at once, so exactly
  # one earns an explanation and the rest are throttled. That is the whole
  # point of the budget, asserted rather than assumed.
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  KUBEAGENT_EXPLAIN_ENDPOINT="http://127.0.0.1:$sport/v1" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format json --alert-repeat 1h \
    --explain --explain-budget 1 --explain-cooldown 0 --model chaos-stub >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done

  # The daemon is now primed on a healthy namespace, so what follows is a real
  # transition rather than a cold-start snapshot.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 40

  expl="$(curl -s "http://127.0.0.1:$port/explanations" 2>/dev/null || echo '<unreachable>')"
  local metrics
  metrics="$(curl -s "http://127.0.0.1:$port/metrics" 2>/dev/null || echo '')"

  kill "$wpid" >/dev/null 2>&1 || true; wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true; wait "$apid" >/dev/null 2>&1 || true
  kill "$spid" >/dev/null 2>&1 || true; wait "$spid" >/dev/null 2>&1 || true

  {
    echo '--- model calls the daemon actually made (one line per call) ---'
    printf 'calls: %s\n' "$(wc -l <"$calls" 2>/dev/null || echo 0)"
    echo
    echo '--- /explanations ---'
    printf '%s\n' "$expl" | python3 -m json.tool 2>/dev/null || printf '%s\n' "$expl"
    echo
    echo '--- explain metrics ---'
    { grep -E '^kubeagent_explain_' <<<"$metrics" || echo '<no explain series>'; }
    echo
    echo '--- explanation notifications delivered ---'
    { grep -c '"reason":"explanation"' "$alerts" 2>/dev/null || echo 0; } | sed 's/^/explanation notifications: /'
    { grep -c '"reason":"new"' "$alerts" 2>/dev/null || echo 0; } | sed 's/^/plain firing notifications: /'
    echo
    echo '--- egress check: no pod name, pod IP or node name in any prompt ---'
    { grep -cE '"prompt":[^\n]*(10\.[0-9]+\.[0-9]+\.[0-9]+|web-[0-9a-f]{6,}|kubeagent-chaos-worker)' "$calls" 2>/dev/null || true; } \
      | sed 's/^/prompts leaking pod or node detail: /'
    echo
    echo '--- endpoint redaction check (only scheme://host may appear in logs) ---'
    { grep -c "127.0.0.1:$sport/v1" "$wlog" || true; } | sed 's/^/log lines naming the endpoint path: /'
    echo
    echo '--- write-path check: the daemon issued no mutating calls ---'
    { grep -icE '\b(create|update|patch|delete)d?\b' "$wlog" || true; } | sed 's/^/log lines mentioning a write verb: /'
  } | record "14. On-incident explanations (budget 1, two objects break)" "expect: exactly 1 model call and exactly 1 explanation notification (reason=explanation) even though two objects break — Deployment/$ns/web and its Service — because --explain-budget 1 admits one and throttles the rest. kubeagent_explain_allowed_total must be 1 and kubeagent_explain_throttled_total at least 1; /explanations must carry one entry with non-empty text and model=chaos-stub, alongside the plain firing notifications which are unaffected. No prompt may contain a pod name, pod IP or node name, no log line may carry the endpoint's path, and no write verb may appear. This scenario uses a local stub endpoint, so it proves the transport, the throttle, the notification shape and the egress discipline — it does not exercise the Anthropic backend, which is covered by unit tests only."

  rm -f "$wlog" "$alerts" "$calls"
}
```

Register it in `run_scenarios` alongside the others, in numeric position, and
add it to the `--only` dispatch exactly the way scenario 12 is registered. Find
both with:

```bash
grep -n "scenario_12" chaos/run.sh
```

- [ ] **Step 3: Run the scenario**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
go build -o kubeagent .
./chaos/run.sh --only 14 --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/scratchpad/chaos14.md
```

Expected: the report shows `calls: 1`, one `"reason":"explanation"`
notification, `kubeagent_explain_allowed_total 1`,
`kubeagent_explain_throttled_total` at least 1, one `/explanations` entry with
`"model": "chaos-stub"` and non-empty `"text"`, and zeros for the leak,
redaction and write-verb checks.

**If any number disagrees with the expectation string, fix the code or the
expectation — never leave a string that claims something the run did not show.**
`record()` asserts nothing, so the expectation string is the only test.

- [ ] **Step 4: Commit**

```bash
git add chaos/explain-stub.py chaos/run.sh
git commit -m "test(chaos): scenario 14 exercises the explain path end to end

Runs the daemon against Kind with a local OpenAI-compatible stub, breaks two
objects with a budget of 1, and asserts exactly one model call, one explanation
notification, and a non-zero throttle count. No API key is involved anywhere.

The scenario also checks the egress discipline directly, against the prompts the
daemon actually sent: no pod name, pod IP or node name may appear in any of
them. The expectation string names what this does not cover — the Anthropic
backend, which unit tests reach and a stub cannot."
```

---

### Task 10: Documentation

**Files:**

- Modify: `website/docs/watch.md`
- Modify: `website/docs/roadmap.md`
- Modify: `CHANGELOG.md`
- Modify: `docs/go-concepts.md` (**gitignored — edit but never `git add`**)

**Interfaces:**

- Consumes: everything above.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Document the feature on the watch page**

Add a section to `website/docs/watch.md`, placed after the alerting section and
matching its heading level and style:

````markdown
## On-incident explanations (`--explain`)

Off by default. When enabled, an object that breaks gets a second, model-written
message a few seconds after its page: what likely caused it, how to confirm, and
the deterministic fix kubeagent already computed.

```bash
export ANTHROPIC_API_KEY=<PLACEHOLDER>
kubeagent watch --explain --explain-budget 20 --explain-cooldown 1h
```

The alert itself never waits on the model. It fires immediately and LLM-free,
exactly as it does without this flag; the explanation is enqueued separately
through the same webhook sink, referencing the same object, so it lands under
the original page.

### What the model sees

The object that broke, the other workloads currently flagged, the cluster
verdict, and the correlation hints kubeagent already computed — so it can say
"one of twelve workloads failing to pull from the same registry" rather than
guessing from one object in isolation.

It does **not** see pod specs, environment variables, ConfigMap or Secret
contents, pod names, pod IPs, node names, or logs. Enabling `--explain` adds no
cluster read and no RBAC verb: the daemon sends only findings it had already
collected.

### Cost control

Two limits, for two different ways spend runs away:

| Limit | Default | Guards against |
|---|---|---|
| `--explain-cooldown` | `1h` | one flapping object being re-explained every reconcile |
| `--explain-budget` | `20`/hour | a mass outage where many distinct objects break at once |

The budget is a token bucket whose capacity equals its hourly rate, so a real
mass outage gets its whole allowance at once and then drips. Over budget, the
call is skipped rather than queued — a stale explanation is worse than none —
and the skip is counted. A restart explains nothing from its first snapshot, so
a crash-looping daemon cannot spend its budget re-explaining pre-existing
problems.

Watch `kubeagent_explain_budget_remaining` to see why an incident went
unexplained.

### `/explanations`

The latest explanation per object, alongside the counters:

```bash
curl -s localhost:8080/explanations | jq .
```

This is model-written prose about your failures, served on the same
unauthenticated metrics port as `/issues`. Same sensitivity class as `/issues`,
but worth knowing before you enable it.

### With a local model

No data leaves your network:

```bash
export KUBEAGENT_EXPLAIN_ENDPOINT=http://ollama.llm.svc.cluster.local:11434/v1
kubeagent watch --explain --model llama3.1
```

### In the chart

The API key is never a chart value — a values file lands in Git, in
`helm get values`, and in CI logs. Create a Secret and name it:

```bash
kubectl -n kubeagent create secret generic kubeagent-llm --from-literal=apiKey=<PLACEHOLDER>
helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set explain.enabled=true --set explain.existingSecret=kubeagent-llm
```

The chart refuses to render if `explain.enabled` is true without a Secret.
````

- [ ] **Step 2: Move the slice to Shipped in the roadmap**

In `website/docs/roadmap.md`, remove slice 4 from the remaining Theme E slices
and add it under `## Shipped`, in the established style of the slice-3 entry
directly above it — one paragraph naming what shipped and the one design
decision worth remembering (the read-only invariant is enforced by the
explainer's type signature, which takes no Kubernetes client).

- [ ] **Step 3: Add the changelog entry**

Under `## [Unreleased]` in `CHANGELOG.md`:

```markdown
### Added

- `watch --explain`: opt-in, rate-limited on-incident explanations. When an
  object breaks, the daemon sends a second, model-written message a few seconds
  after the page — likely cause, how to confirm, and the deterministic fix.
  The alert itself still fires immediately and LLM-free; the explanation rides
  the same webhook sink, so retry, backoff and URL redaction all apply
  unchanged. New flags `--explain-cooldown` (default `1h`) and
  `--explain-budget` (default `20`/hour) bound the spend, a new
  `/explanations` endpoint serves the latest explanation per object, and five
  `kubeagent_explain_*` series make throttling visible. Works with a local
  OpenAI-compatible model via `KUBEAGENT_EXPLAIN_ENDPOINT`.
- Helm: `explain.*` values, with the API key wired from a Secret via
  `secretKeyRef`. The chart refuses to render if explanations are enabled
  without one.

### Changed

- The watch daemon's package documentation no longer claims "no LLM". It stays
  strictly read-only toward the cluster: `--explain` adds no cluster read and no
  RBAC verb, because the model sees only findings the daemon had already
  collected.
```

- [ ] **Step 4: Add the Go concepts entries**

Append two entries to `docs/go-concepts.md`, in the file's established style —
**a plain everyday example first, then the kubeagent example**:

1. **Token-bucket rate limiting.** Everyday example: a coffee shop loyalty card
   that grants one free coffee per hour and holds at most twenty stamps, so you
   can take twenty at once after a long absence but then only one per hour.
   Then: `oncall.Throttle`, where capacity equals the hourly rate so a real mass
   outage gets its whole allowance at once, and `Remaining` projects the count
   without consuming anything so reading a metric can never change a decision.

2. **`defer` runs LIFO, and that ordering can be load-bearing.** Everyday
   example: taking off a coat, then a jumper, then a shirt — you must reverse
   the order you put them on. Then: `watch.Run`, where the explainer produces
   for the alert sink, so it is deferred *after* the sink and therefore torn
   down *before* it. Note the failure this prevents is silent, not loud: the
   sink's queue channel is never closed, so a late send does not panic — the
   notification simply lands in a buffer nobody is reading any more.

**Do not `git add` this file — it is gitignored.**

- [ ] **Step 5: Verify the site builds**

```bash
export PATH=$PATH:/usr/local/go/bin
cd website && /tmp/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml; cd ..
```

Expected: "Documentation built", exit 0, and no `WARNING` lines naming
`watch.md` or `roadmap.md`. The red "Material for MkDocs 2.0" banner is
cosmetic.

- [ ] **Step 6: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -race
```

Expected: every package passes.

- [ ] **Step 7: Commit**

```bash
git add website/docs/watch.md website/docs/roadmap.md CHANGELOG.md
git commit -m "docs: on-incident explanations

Documents the flag, both rate limits, the /explanations endpoint, the local-model
path, and the Secret-based key wiring. States plainly what the model sees and
what it does not, and that /explanations puts model-written prose about your
failures on the same unauthenticated port as /issues — worth knowing before
enabling it rather than after."
```

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: the four decisions
to Tasks 4 and 3; read-only enforcement to Task 4's `IncidentExplainer`
interface and Task 6's `newExplainer` (no client parameter in either); the
package-doc amendment to Task 6; egress to Task 3's negative test and Task 9's
prompt check; untrusted model output to Task 1; the webhook-credential rule to
Task 6's `RedactURL` on the endpoint log line; opt-in and fail-fast to Tasks 6
and 7; `/explanations` exposure to Tasks 5 and 10; throttle semantics to Task 2;
configuration and Helm to Tasks 7 and 8; cold start, shutdown ordering, per-call
timeout and the failure table to Tasks 4 and 6; notification and endpoint shapes
to Tasks 1 and 5; every named test to Tasks 2, 3, 4, 5, 6, 7 and 9;
documentation to Task 10.

**Corrected from the spec while planning.** The spec said enqueueing onto a
closed sink is a panic. It is not: `alert.Sink` never closes its queue channel,
so a late `Enqueue` silently buffers a notification whose sender has already
returned. Same ordering requirement, quieter failure — which makes the test
assertion different, so Task 6 asserts the teardown order itself rather than the
absence of a panic. The spec has been amended.

**Type consistency.** `Explanation`, `Stats` and `Config` are defined once in
Task 4 and consumed with those exact field names in Tasks 5 and 6.
`updateExplain(enabled bool, s oncall.Stats, latest []oncall.Explanation)` is
declared in Task 5 and called with that signature in Task 6. The six
`watch.Config` fields are declared in Task 6 and set with those names in Task 7.
`BuildIncidentPrompt(object string, issues []string, cluster, flagged,
serviceIssues)` is declared in Task 3 and called with that signature in Task 4.
`ExplainIncident(ctx, prompt)` is declared in Task 3, abstracted as
`IncidentExplainer` in Task 4, and satisfied by `*explain.Client` in Task 6.

**Known ordering constraint for the executor.** Tasks 1–4 may be reviewed
independently but must land in order: Task 4 will not compile without Tasks 1,
2 and 3. Task 6 will not compile without Tasks 4 and 5.
