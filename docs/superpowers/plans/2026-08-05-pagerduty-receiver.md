# PagerDuty Events API v2 Receiver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `pagerduty` as a fourth `--alert-format` for the watch daemon's
alert sink, so an operator can be paged by kubeagent without first deploying a
Prometheus stack.

**Architecture:** One new encoder in `internal/alert` behind the existing
`Format` dispatch, one new env-only credential (`KUBEAGENT_ALERT_ROUTING_KEY`),
and a defaulted-but-overridable endpoint. The `Sink` — its queue, retry,
backoff, counters and URL redaction — is unchanged: a PagerDuty event is just
another JSON body posted to another URL. The Helm chart gains no values, only a
conditional that points the existing alert Secret at the new variable name.

**Tech Stack:** Go 1.26 (standard library only — `crypto/sha256`,
`encoding/hex`, `encoding/json`, `unicode/utf8`), Cobra/pflag for the CLI, Helm
3 templates, bash for the chaos harness, MkDocs Material for the site.

**Requirements:** `docs/superpowers/specs/2026-08-05-pagerduty-receiver-design.md`
(committed `ad239cb`). It records five settled decisions with the alternatives
each closes off. **Reopening one is a defect**, not an improvement.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every commit needs a `Signed-off-by` trailer matching its author** — use
  `git commit -s`. `main` enforces DCO. Verify with
  `bash scripts/dco-check.sh main HEAD`.
- **No `Co-Authored-By: Claude` trailer and no AI attribution anywhere** —
  commits, code, comments, docs, changelog, PR text. Every commit is authored
  solely by the human.
- **Work stays on branch `pagerduty-receiver`.** Never commit to `main`.
- **NO NEW DEPENDENCY.** `go.mod` and `go.sum` must not change.
  `crypto/sha256`, `encoding/hex`, `encoding/json` and `unicode/utf8` are all
  standard library.
- **`internal/alert` must never import `internal/remediate` or
  `internal/explain`.**
- **Read-only toward the cluster:** alerting adds one egress destination and
  issues no cluster call. **Separately — a second, distinct promise — nothing on
  this path makes a model call.** Never blur the two into one sentence in a
  comment or a doc, and never derive one from the other.
- **The routing key must never reach a log line, a metric label, an error
  message, or a rendered manifest.** `post` must keep discarding the response
  body (`io.Copy(io.Discard, resp.Body)` at `internal/alert/sink.go:229`); do
  not read 4xx bodies to improve a message.
- **Nothing kubeagent emits may carry more than `scheme://host`.**
- **The six versioned JSON documents do not move.** An alert body is not one of
  them: they are `report.ScanReport`, `gate.Verdict`,
  `rbacprofile.RulesDocument`, `rbacprofile.CheckDocument`, `watch.IssuesReport`
  and `watch.ExplanationsReport`. No `schemaVersion` bump, no
  `internal/jsonschema` change, no schema regeneration.
- **`internal/report/testdata/golden-scan.txt` stays byte-identical.** Do NOT
  regenerate the demo GIF or `website/docs/quickstart.md`.
- **`internal/rbacprofile`'s `Feature` table and every generated RBAC manifest
  stay untouched** — this reads no new resource kind.
- **Untrusted API text is sanitized at ingress via `internal/safetext`, never at
  a renderer or an encoder.** The encoder escapes for JSON, which
  `encoding/json` does; it must not become a second sanitization site.
- **TDD:** write the failing test first, run it, watch it fail, then implement.
- **`go test` runs with `-p 2`, never `-short`.** CI's `go test -race ./...`
  must stay green.
- **No secrets, credentials, private IPs or internal hostnames anywhere** —
  including test fixtures, the fuzz seed corpus, chart values examples and every
  doc example. RFC 5737 addresses (`192.0.2.0/24`, `198.51.100.0/24`,
  `203.0.113.0/24`), RFC 2606 domains (`example.com`, `example.org`,
  `example.net`), and the `<ROUTING_KEY>` / `<WEBHOOK_URL>` placeholders. **The
  one fixture routing key is the literal `not-a-real-routing-key`.** A fixture
  *named* like a credential is a defect even when its value is fake.
- **Never expose API keys to the shell.** The chaos harness runs with
  `ANTHROPIC_API_KEY` unset; it must not set, reference, export or pass it.
- Go lives at `/usr/local/go/bin` — `export PATH=$PATH:/usr/local/go/bin`.
  Helm and kind live at `$HOME/.local/bin`.

---

## File Structure

**Modified:**

| File | Responsibility after this change |
|---|---|
| `internal/alert/encode.go` | `FormatPagerDuty`; the three PagerDuty payload structs; `dedupKey`; `pdSummary`; `encodePagerDuty`; `validateRoutingKey`; `encode` grows a routing-key parameter |
| `internal/alert/url.go` | `DefaultURL(Format)`; `resolveURL` defaults an empty URL and fills `/v2/enqueue` |
| `internal/alert/sink.go` | `Config.RoutingKey`; `Sink.routingKey`; the format switch in `New`; the `encode` call in `deliver` |
| `internal/watch/watch.go` | `Config.AlertRoutingKey`; the enable gate in `newAlerter`; the startup log's resolved endpoint |
| `internal/cli/watch.go` | reads `KUBEAGENT_ALERT_ROUTING_KEY`; enables on either credential; `--alert-format` help text; two warnings |
| `internal/cli/root.go` | the usage string's `--alert-format` value list |
| `deploy/helm/kubeagent/values.yaml` | comments only — no new keys |
| `deploy/helm/kubeagent/templates/deployment.yaml` | one conditional on the env-var name |
| `deploy/helm/kubeagent/Chart.yaml` | MINOR bump, `0.25.1` → `0.26.0` |
| `chaos/run.sh` | scenario 23 |
| `.github/workflows/fuzz.yml` | one new `(package, target)` matrix pair |

**Created:**

| File | Responsibility |
|---|---|
| `internal/alert/fuzz_test.go` | `FuzzEncodePagerDuty` — the package's first fuzz target |

**Test files modified:** `internal/alert/encode_test.go`,
`internal/alert/url_test.go`, `internal/alert/sink_test.go`,
`internal/watch/watch_test.go`, `internal/cli/cli_test.go`.

**Docs modified:** `website/docs/features/watch-mode.md`,
`website/docs/compatibility.md`, `website/docs/roadmap.md`, `chaos/README.md`,
`deploy/README.md`, `CHANGELOG.md`, `CLAUDE.md`.

---

## Task 1: The PagerDuty encoder

**Files:**
- Modify: `internal/alert/encode.go` (the `Format` const block at 15-19, `encode`
  at 22-33, then append the new code at the end of the file)
- Modify: `internal/alert/sink.go:178` (the one non-test `encode` call site)
- Test: `internal/alert/encode_test.go`

**Interfaces:**
- Consumes: `alertstate.Notification{Object, Status, Issues, FiringSince,
  ResolvedAt, Flapping, Reason, Text}`; `alertstate.Object.String()` which
  renders `local/Deployment/shop/web`, or `local/Node/worker-2` when `Namespace`
  is empty; `alertstate.StatusFiring` / `StatusResolved`; `alertstate.ReasonNew`
  / `ReasonChanged` / `ReasonRepeat` / `ReasonResolved` / `ReasonExplanation`.
- Produces:
  - `const FormatPagerDuty Format = "pagerduty"`
  - `func encode(f Format, routingKey string, n alertstate.Notification) ([]byte, error)`
    — the signature every later task calls
  - `func encodePagerDuty(routingKey string, n alertstate.Notification) ([]byte, error)`
  - `func dedupKey(o alertstate.Object) string`
  - `func pdSummary(n alertstate.Notification) string`
  - types `pdEvent`, `pdPayload`, `pdDetails`
  - `const pdMaxDedupKey = 255`, `pdMaxSummary = 1024`, `pdSeverity = "error"`

**Note on the intermediate state:** this task adds `FormatPagerDuty` to the
const block but does **not** add it to `New`'s format switch in `sink.go` (Task
3 does). So after this task the format is unreachable through the `Sink`, and
`deliver` passes an empty routing key. That is deliberate and correct — the
sink gains the field in Task 3. Do not "fix" it here.

- [ ] **Step 1: Write the failing encoder-table tests**

Add these eight cases to the `tests` slice inside `TestEncode` in
`internal/alert/encode_test.go`, after the existing alertmanager cases. Note the
existing table's field is `format`, and its call site (line 78) becomes
`encode(tc.format, "not-a-real-routing-key", tc.notif)` in Step 3 — write the
cases now against that value.

```go
		{
			name:   "pagerduty firing new",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"new","flapping":false}}}`,
		},
		{
			name:   "pagerduty firing changed lists every issue in the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonChanged,
				Issues:      []string{"ErrImagePull", "ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ErrImagePull, ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ErrImagePull","ImagePullBackOff"],"reason":"changed","flapping":false}}}`,
		},
		{
			name:   "pagerduty repeat is a trigger on the same dedup key",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonRepeat,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"repeat","flapping":false}}}`,
		},
		{
			name:   "pagerduty explanation carries the prose in custom_details, never in the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonExplanation,
				Issues:      []string{"ImagePullBackOff"},
				FiringSince: at,
				Text:        "The image tag does not exist in the registry.",
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: ImagePullBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["ImagePullBackOff"],"reason":"explanation","flapping":false,"explanation":"The image tag does not exist in the registry."}}}`,
		},
		{
			name:   "pagerduty resolved carries only the three required fields",
			format: FormatPagerDuty,
			notif:  resolvedNotif,
			want:   `{"routing_key":"not-a-real-routing-key","event_action":"resolve","dedup_key":"local/Deployment/shop/web"}`,
		},
		{
			name:   "pagerduty flapping is a detail, not part of the summary",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonChanged,
				Issues:      []string{"CrashLoopBackOff"},
				FiringSince: at,
				Flapping:    true,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Deployment/shop/web","payload":{"summary":"local/Deployment/shop/web: CrashLoopBackOff","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Deployment","namespace":"shop","name":"web","issues":["CrashLoopBackOff"],"reason":"changed","flapping":true}}}`,
		},
		{
			name:   "pagerduty cluster-scoped object omits the namespace",
			format: FormatPagerDuty,
			notif: alertstate.Notification{
				Object:      alertstate.Object{Cluster: "local", Kind: "Node", Name: "worker-2"},
				Status:      alertstate.StatusFiring,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{"NodeNotReady"},
				FiringSince: at,
			},
			want: `{"routing_key":"not-a-real-routing-key","event_action":"trigger","dedup_key":"local/Node/worker-2","payload":{"summary":"local/Node/worker-2: NodeNotReady","source":"local","severity":"error","timestamp":"2026-07-25T10:04:11Z","custom_details":{"cluster":"local","kind":"Node","name":"worker-2","issues":["NodeNotReady"],"reason":"new","flapping":false}}}`,
		},
```

Then add this standalone test at the end of the file. It is the over-long
dedup-key case, which does not fit the table because its expected value is
computed:

```go
// A Kubernetes name may be 253 characters on its own, so the
// cluster/kind/namespace/name concatenation can legally exceed PagerDuty's
// 255-character dedup_key cap. A flat truncation would silently merge two
// distinct objects onto one incident — one outage swallowing another, which is
// exactly the failure the per-object rollup exists to prevent. The digest
// suffix keeps two over-long neighbours apart.
func TestEncodePagerDuty_OverLongDedupKeyStaysDistinct(t *testing.T) {
	long := strings.Repeat("a", 250)
	mk := func(name string) string {
		body, err := encode(FormatPagerDuty, "not-a-real-routing-key", alertstate.Notification{
			Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: name},
			Status:      alertstate.StatusFiring,
			Reason:      alertstate.ReasonNew,
			Issues:      []string{"ImagePullBackOff"},
			FiringSince: at,
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var got struct {
			DedupKey string `json:"dedup_key"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		return got.DedupKey
	}

	a, b := mk(long+"one"), mk(long+"two")
	if len(a) > 255 {
		t.Errorf("dedup_key is %d bytes, want at most 255", len(a))
	}
	if a == b {
		t.Errorf("two distinct objects collapsed onto one dedup_key: %q", a)
	}
	if !strings.HasPrefix(a, "local/Deployment/shop/"+strings.Repeat("a", 20)) {
		t.Errorf("dedup_key lost its readable prefix: %q", a)
	}
	if a != mk(long+"one") {
		t.Errorf("dedup_key is not deterministic")
	}
}
```

And this determinism test:

```go
// The same notification must encode byte-identically twice. Map iteration order
// is the classic way this breaks; the PagerDuty payload uses structs precisely
// so it cannot.
func TestEncodePagerDuty_IsDeterministic(t *testing.T) {
	n := alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonExplanation,
		Issues:      []string{"ErrImagePull", "ImagePullBackOff"},
		FiringSince: at,
		Flapping:    true,
		Text:        "The image tag does not exist in the registry.",
	}
	first, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("encode is not deterministic:\n%s\n%s", first, again)
		}
	}
}
```

- [ ] **Step 2: Update every existing `encode` call site to the new signature**

`encode` grows a routing-key parameter, so the existing calls must move with
it or the package will not compile. In `internal/alert/encode_test.go` change:

- line 78: `got, err := encode(tc.format, tc.notif)` →
  `got, err := encode(tc.format, "not-a-real-routing-key", tc.notif)`
- lines 98, 116, 127, 141, 168, 185, 212, 231, 251: insert `""` as the second
  argument, e.g. `encode(FormatSlack, "", n)` — these formats ignore the key.
- line 288 (inside `TestEncodeEscapesHostileModelOutput`):
  `body, err := encode(f, "not-a-real-routing-key", n)`, and extend that test's
  format list to `[]Format{FormatJSON, FormatSlack, FormatAlertmanager, FormatPagerDuty}`.

In `internal/alert/sink.go` line 178 change `encode(s.format, n)` to
`encode(s.format, "", n)`. Task 3 replaces that `""` with `s.routingKey`.

Also add `"encoding/json"` and `"strings"` to `encode_test.go`'s imports if
they are not already there (both are).

- [ ] **Step 3: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/ -run 'TestEncode' -p 2
```

Expected: FAIL — `undefined: FormatPagerDuty` at build time.

- [ ] **Step 4: Add `FormatPagerDuty` and the dispatch**

In `internal/alert/encode.go`, extend the const block:

```go
const (
	FormatJSON         Format = "json"
	FormatSlack        Format = "slack"
	FormatAlertmanager Format = "alertmanager"
	FormatPagerDuty    Format = "pagerduty"
)
```

Replace `encode` (lines 21-33) with:

```go
// encode renders one notification in the configured format. routingKey is the
// PagerDuty integration key and is ignored by every other format: PagerDuty is
// the one receiver that authenticates in the request body rather than with the
// URL itself.
func encode(f Format, routingKey string, n alertstate.Notification) ([]byte, error) {
	switch f {
	case FormatJSON:
		return encodeJSON(n)
	case FormatSlack:
		return encodeSlack(n)
	case FormatAlertmanager:
		return encodeAlertmanager(n)
	case FormatPagerDuty:
		return encodePagerDuty(routingKey, n)
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, alertmanager, or pagerduty)", f)
	}
}
```

- [ ] **Step 5: Add the PagerDuty types and encoder**

Append to `internal/alert/encode.go`:

```go
// PagerDuty Events API v2 constants.
const (
	// pdSeverity is a constant because kubeagent has no severity model on
	// diagnose.Finding. "error" is the same default Alertmanager's own PagerDuty
	// notifier picks, and it is the honest middle: kubeagent knows something is
	// broken, not how badly.
	pdSeverity = "error"

	// pdMaxDedupKey is PagerDuty's documented dedup_key cap. It is applied in
	// bytes, which satisfies both readings of "255 characters": a string of at
	// most 255 bytes is also at most 255 characters, whatever the encoding.
	pdMaxDedupKey = 255
	// pdDedupKeyPrefix leaves room for the "/" separator and the 8-hex digest.
	pdDedupKeyPrefix = pdMaxDedupKey - 1 - 8

	// pdMaxSummary matches what Alertmanager's PagerDuty notifier allows.
	pdMaxSummary = 1024
)

// pdEvent is one PagerDuty Events API v2 event. Payload is a pointer so a
// resolve can omit it entirely: PagerDuty requires only routing_key,
// event_action and dedup_key to close an incident, and it computes the incident
// duration itself — anything kubeagent added would be a second copy free to
// disagree with the first.
type pdEvent struct {
	RoutingKey  string     `json:"routing_key"`
	EventAction string     `json:"event_action"`
	DedupKey    string     `json:"dedup_key"`
	Payload     *pdPayload `json:"payload,omitempty"`
}

type pdPayload struct {
	Summary       string    `json:"summary"`
	Source        string    `json:"source"`
	Severity      string    `json:"severity"`
	Timestamp     string    `json:"timestamp"`
	CustomDetails pdDetails `json:"custom_details"`
}

// pdDetails is a struct rather than a map[string]any so the shape is documented
// by the type and cannot pick up a stray key, and so the encoded field order is
// fixed — a map would iterate differently on every call.
type pdDetails struct {
	Cluster     string   `json:"cluster"`
	Kind        string   `json:"kind"`
	Namespace   string   `json:"namespace,omitempty"`
	Name        string   `json:"name"`
	Issues      []string `json:"issues"`
	Reason      string   `json:"reason"`
	Flapping    bool     `json:"flapping"`
	Explanation string   `json:"explanation,omitempty"`
}

// encodePagerDuty renders one Events API v2 event. An explanation is a trigger
// on the same dedup_key rather than a new event kind: the object is still
// firing, and an explanation is additional detail about one state rather than a
// transition to another.
func encodePagerDuty(routingKey string, n alertstate.Notification) ([]byte, error) {
	e := pdEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		DedupKey:    dedupKey(n.Object),
	}
	if n.Status == alertstate.StatusResolved {
		e.EventAction = "resolve"
		return json.Marshal(e)
	}
	issues := n.Issues
	if issues == nil {
		issues = []string{}
	}
	e.Payload = &pdPayload{
		Summary:  pdSummary(n),
		Source:   n.Object.Cluster,
		Severity: pdSeverity,
		// FiringSince, not the current wall clock: the alert is stamped when the
		// object actually broke rather than when the daemon noticed.
		Timestamp: n.FiringSince.UTC().Format(time.RFC3339),
		CustomDetails: pdDetails{
			Cluster:     n.Object.Cluster,
			Kind:        n.Object.Kind,
			Namespace:   n.Object.Namespace,
			Name:        n.Object.Name,
			Issues:      issues,
			Reason:      string(n.Reason),
			Flapping:    n.Flapping,
			Explanation: n.Text,
		},
	}
	return json.Marshal(e)
}

// dedupKey identifies the incident. It is Object.String() — derived from
// identity, not from state, so it survives a daemon restart and the restart's
// re-trigger lands on the open alert instead of opening a second one. An
// over-long key keeps a readable prefix and appends a digest of the whole
// string, so two objects that share the first 246 bytes still get two
// incidents.
func dedupKey(o alertstate.Object) string {
	s := o.String()
	if len(s) <= pdMaxDedupKey {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return trimBytes(s, pdDedupKeyPrefix) + "/" + hex.EncodeToString(sum[:4])
}

// pdSummary is the line a human reads in a push notification at 3am, so it is
// the object and its issues and nothing else. Model-written prose never enters
// it: n.Text can run to paragraphs and travels in custom_details instead.
func pdSummary(n alertstate.Notification) string {
	s := n.Object.String()
	if len(n.Issues) > 0 {
		s += ": " + strings.Join(n.Issues, ", ")
	}
	return trimRunes(s, pdMaxSummary)
}

// trimBytes cuts s to at most n bytes without splitting a rune. Cutting one in
// half would leave a byte that json.Marshal replaces with U+FFFD — three bytes
// where one was cut, which is mojibake in an operator-facing incident key.
func trimBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// trimRunes cuts s to at most n runes.
func trimRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
```

Extend the import block at the top of `encode.go` to:

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/imantaba/kubeagent/internal/alertstate"
)
```

- [ ] **Step 6: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Expected: PASS, every package.

- [ ] **Step 7: Confirm `go.mod` and `go.sum` did not move**

```bash
git status --short go.mod go.sum
```

Expected: no output.

- [ ] **Step 8: Commit**

```bash
git add internal/alert/encode.go internal/alert/encode_test.go internal/alert/sink.go
git commit -s -m "alert: encode the PagerDuty Events API v2 event

A fourth Format renders a trigger for a firing object and a resolve for a
recovered one, both on a dedup_key derived from the object's identity so a
daemon restart re-triggers onto the open incident rather than opening a second.
An over-long key keeps a readable prefix and appends a digest of the whole
string: a flat truncation would merge two distinct objects onto one incident.

The format is not yet reachable through the Sink — New still rejects it."
```

---

## Task 2: Routing-key validation and endpoint resolution

**Files:**
- Modify: `internal/alert/encode.go` (append `validateRoutingKey`)
- Modify: `internal/alert/url.go` (add `DefaultURL`; extend `resolveURL`)
- Test: `internal/alert/url_test.go`

**Interfaces:**
- Consumes: `FormatPagerDuty` from Task 1.
- Produces:
  - `func validateRoutingKey(key string) error`
  - `func DefaultURL(f Format) string` — exported; `internal/watch` (Task 4)
    calls it so the startup log never prints an empty endpoint
  - `const pagerDutyEventsURL = "https://events.pagerduty.com/v2/enqueue"`
  - `resolveURL(raw, f)` now substitutes `DefaultURL(f)` for an empty `raw` and
    fills `/v2/enqueue` for `FormatPagerDuty`

- [ ] **Step 1: Write the failing tests**

Add these four rows to the `tests` table in `TestResolveURL` in
`internal/alert/url_test.go`:

```go
		{"pagerduty empty URL takes the published endpoint", "", FormatPagerDuty, "https://events.pagerduty.com/v2/enqueue"},
		{"pagerduty bare host gains the enqueue path", "https://events.eu.example.com", FormatPagerDuty, "https://events.eu.example.com/v2/enqueue"},
		{"pagerduty root path gains the enqueue path", "https://events.eu.example.com/", FormatPagerDuty, "https://events.eu.example.com/v2/enqueue"},
		{"pagerduty explicit path is respected", "http://192.0.2.10:8080/capture", FormatPagerDuty, "http://192.0.2.10:8080/capture"},
```

Add these two tests at the end of `internal/alert/url_test.go`:

```go
// Every other format's URL is the operator's own receiver and cannot be
// guessed, so only pagerduty has a default. A json install with no
// KUBEAGENT_ALERT_WEBHOOK must still be an error, not a silent post to nowhere.
func TestDefaultURL(t *testing.T) {
	if got := DefaultURL(FormatPagerDuty); got != "https://events.pagerduty.com/v2/enqueue" {
		t.Errorf("DefaultURL(pagerduty) = %q", got)
	}
	for _, f := range []Format{FormatJSON, FormatSlack, FormatAlertmanager, Format("teletype")} {
		if got := DefaultURL(f); got != "" {
			t.Errorf("DefaultURL(%s) = %q, want empty", f, got)
		}
	}
	if _, err := resolveURL("", FormatJSON); err == nil {
		t.Error("resolveURL(\"\", json) must error: there is no default receiver to fall back to")
	}
}

// The routing key is a credential. A validation error names the variable the
// operator must fix and never echoes what they set it to.
func TestValidateRoutingKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"a plain token is accepted", "not-a-real-routing-key", false},
		{"empty is rejected", "", true},
		{"a trailing newline is rejected", "not-a-real-routing-key\n", true},
		{"an embedded space is rejected", "not a real routing key", true},
		{"a pasted multi-line blob is rejected", "not-a-real\nrouting-key", true},
		{"a control byte is rejected", "not-a-real-routing-key\x07", true},
		{"a tab is rejected", "\tnot-a-real-routing-key", true},
		{"a non-ASCII byte is rejected", "not-a-real-routing-k\xffy", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRoutingKey(tc.key)
			if tc.wantErr != (err != nil) {
				t.Fatalf("validateRoutingKey(%q) error = %v, wantErr %v", tc.key, err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "KUBEAGENT_ALERT_ROUTING_KEY") {
				t.Errorf("error does not name the variable to fix: %v", err)
			}
			if tc.key != "" && strings.Contains(err.Error(), tc.key) {
				t.Errorf("error echoes the routing key: %v", err)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/ -run 'TestResolveURL|TestDefaultURL|TestValidateRoutingKey' -p 2
```

Expected: FAIL — `undefined: DefaultURL` and `undefined: validateRoutingKey` at
build time.

- [ ] **Step 3: Add `DefaultURL` and extend `resolveURL`**

In `internal/alert/url.go`, add above `resolveURL`:

```go
// pagerDutyEventsURL is PagerDuty's published Events API v2 endpoint. It is a
// default rather than a hardcoded destination: an operator on a non-default
// service region, behind an egress proxy, or pointing at a test double sets
// KUBEAGENT_ALERT_WEBHOOK and this is not consulted.
const pagerDutyEventsURL = "https://events.pagerduty.com/v2/enqueue"

// DefaultURL is the endpoint a format uses when the operator configured none.
// Only pagerduty has one, because it publishes a single fixed events endpoint
// while every other format's URL is the operator's own receiver. Exported
// because internal/watch logs the resolved endpoint at startup and must not
// print an empty string.
func DefaultURL(f Format) string {
	if f == FormatPagerDuty {
		return pagerDutyEventsURL
	}
	return ""
}
```

Then replace the body of `resolveURL` (keeping its existing doc comment, with
the first sentence updated):

```go
// resolveURL validates the destination and fills in the path the format expects
// when the URL carries none: /api/v2/alerts for alertmanager, /v2/enqueue for
// pagerduty. An empty URL takes the format's default, which only pagerduty has.
// Its errors never echo the input: url.Parse's own error text embeds the URL, so
// it is deliberately not wrapped.
func resolveURL(raw string, f Format) (string, error) {
	if raw == "" {
		raw = DefaultURL(f)
	}
	if raw == "" {
		return "", errors.New("alerting needs KUBEAGENT_ALERT_WEBHOOK set to the receiver URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("alert webhook URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("alert webhook URL must use http or https")
	}
	if u.Host == "" {
		return "", errors.New("alert webhook URL has no host")
	}
	if f == FormatAlertmanager && (u.Path == "" || u.Path == "/") {
		u.Path = "/api/v2/alerts"
	}
	if f == FormatPagerDuty && (u.Path == "" || u.Path == "/") {
		u.Path = "/v2/enqueue"
	}
	return u.String(), nil
}
```

- [ ] **Step 4: Add `validateRoutingKey`**

Append to `internal/alert/encode.go`:

```go
// validateRoutingKey rejects a value that cannot be an integration key: empty,
// or carrying a space or any byte outside printable ASCII — which is what a
// Secret with a trailing newline or a pasted multi-line blob looks like.
// Catching it at startup beats catching it at the first HTTP 400. The error
// names the variable to fix and never echoes the value.
//
// The length is deliberately not checked against PagerDuty's 32 characters:
// pinning an upstream length is a hostage to fortune, and it would force every
// fixture to be a 32-character string that reads like a real key.
func validateRoutingKey(key string) error {
	if key == "" {
		return errors.New("the pagerduty alert format needs KUBEAGENT_ALERT_ROUTING_KEY set to the integration key")
	}
	for i := 0; i < len(key); i++ {
		if b := key[i]; b <= ' ' || b > '~' {
			return errors.New("KUBEAGENT_ALERT_ROUTING_KEY must be one token of printable ASCII with no spaces (a trailing newline from a Secret is the usual cause)")
		}
	}
	return nil
}
```

Add `"errors"` to `encode.go`'s import block.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Expected: PASS. `TestResolveURL_ErrorsNeverEchoTheURL` already passes `""` with
`FormatJSON` and requires an error — the new empty-URL branch still returns one,
and its `in != ""` guard skips the echo check, so that test needs no change.

- [ ] **Step 6: Commit**

```bash
git add internal/alert/encode.go internal/alert/url.go internal/alert/url_test.go
git commit -s -m "alert: validate the routing key and resolve the PagerDuty endpoint

The routing key must be one token of printable ASCII; a Secret with a trailing
newline is a configuration mistake worth catching at startup rather than at the
first HTTP 400. The error names the variable and never echoes the value.

KUBEAGENT_ALERT_WEBHOOK stays the destination for all four formats and becomes
optional for pagerduty, defaulting to the published events endpoint. A URL with
no path gains /v2/enqueue the way alertmanager's gains /api/v2/alerts, so an
operator on a non-default region supplies a host and nothing more."
```

---

## Task 3: Wire the routing key through the sink

**Files:**
- Modify: `internal/alert/sink.go` (`Config` at 30-36, `Sink` at 52-63,
  `DefaultRepeat` at 65-74 — doc comment only, `New` at 76-102, `deliver` at
  177-178)
- Test: `internal/alert/sink_test.go`

**Interfaces:**
- Consumes: `FormatPagerDuty`, `encode(f, routingKey, n)` (Task 1);
  `validateRoutingKey`, `DefaultURL`, `resolveURL` (Task 2).
- Produces: `alert.Config{URL, Format, Repeat, RoutingKey}` — Task 4 builds this
  literal.

**`DefaultRepeat` needs no new branch.** It already returns `4 * time.Hour` for
every format but alertmanager, which is exactly what the spec requires for
pagerduty. Add the format to its test and one clause to its doc comment; do not
add an `if`. Likewise, **no `maxAlertmanagerRepeat`-style ceiling is added** for
pagerduty: that ceiling exists because Alertmanager expires an alert
`resolve_timeout` after the last POST, and PagerDuty does not expire alerts.

- [ ] **Step 1: Write the failing tests**

Add these four rows to `TestNew_Validation`'s table in
`internal/alert/sink_test.go`:

```go
		{"pagerduty with a routing key and no URL", Config{Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key", Repeat: 4 * time.Hour}, false},
		{"pagerduty with an explicit endpoint", Config{URL: "http://192.0.2.10:8080/capture", Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key", Repeat: 4 * time.Hour}, false},
		{"pagerduty without a routing key", Config{URL: "http://192.0.2.10:8080/capture", Format: FormatPagerDuty, Repeat: 4 * time.Hour}, true},
		{"pagerduty with a whitespace-bearing routing key", Config{Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key\n", Repeat: 4 * time.Hour}, true},
		{"pagerduty ignores the alertmanager cadence ceiling", Config{Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key", Repeat: 24 * time.Hour}, false},
```

The table's third field is `wantErr`, so the first two rows assert **no error**
and the middle two assert one. Read the existing rows to confirm the field name
before pasting.

Extend `TestDefaultRepeat` to cover the new format:

```go
	for _, f := range []Format{FormatJSON, FormatSlack, FormatPagerDuty} {
		if got := DefaultRepeat(f); got != 4*time.Hour {
			t.Errorf("DefaultRepeat(%s) = %s, want 4h0m0s", f, got)
		}
	}
```

Then add this test at the end of `internal/alert/sink_test.go`. It is the
delivery test: the key reaches the body, 202 is a success, and nothing logged
carries the key or more than `scheme://host`.

```go
// PagerDuty authenticates in the request body, and answers 202 rather than 200.
// This asserts the whole path end to end against a real HTTP server: the key
// arrives, the 2xx counts, and the log the daemon would print carries neither
// the credential nor the endpoint's path.
func TestSink_PagerDutyDeliversTheRoutingKeyInTheBodyOnly(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted) // 202, what PagerDuty returns
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	s, err := New(Config{URL: srv.URL + "/capture", Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key", Repeat: 4 * time.Hour}, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Enqueue(alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: at,
	})
	waitFor(t, "one firing delivery", func() bool { return s.Stats().FiringOK == 1 })

	mu.Lock()
	got := bodies
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d bodies, want 1", len(got))
	}
	if !strings.Contains(got[0], `"routing_key":"not-a-real-routing-key"`) {
		t.Errorf("the routing key did not reach the body: %s", got[0])
	}
	if !strings.Contains(got[0], `"event_action":"trigger"`) {
		t.Errorf("body is not a trigger: %s", got[0])
	}
	// A 202 must count as a success, not as a retryable failure.
	if s.Stats().FiringFailed != 0 {
		t.Errorf("a 202 was counted as a failure: %+v", s.Stats())
	}
	if strings.Contains(logBuf.String(), "not-a-real-routing-key") {
		t.Errorf("the routing key reached a log line: %s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "/capture") {
		t.Errorf("the endpoint path reached a log line: %s", logBuf.String())
	}
}

// A bad routing key is a 400, and a wrong credential does not fix itself.
func TestSink_PagerDutyBadRoutingKeyIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	s, err := New(Config{URL: srv.URL + "/capture", Format: FormatPagerDuty, RoutingKey: "not-a-real-routing-key", Repeat: 4 * time.Hour}, srv.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Enqueue(alertstate.Notification{
		Object:      alertstate.Object{Cluster: "local", Kind: "Deployment", Namespace: "shop", Name: "web"},
		Status:      alertstate.StatusFiring,
		Reason:      alertstate.ReasonNew,
		Issues:      []string{"ImagePullBackOff"},
		FiringSince: at,
	})
	waitFor(t, "one failed firing delivery", func() bool { return s.Stats().FiringFailed == 1 })

	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Errorf("got %d attempts, want 1 — a 4xx will not fix itself", n)
	}
	if strings.Contains(logBuf.String(), "not-a-real-routing-key") {
		t.Errorf("the routing key reached a log line: %s", logBuf.String())
	}
}
```

Check `internal/alert/sink_test.go`'s import block and add whatever these need
that is missing: `bytes`, `log`, `os`, `io`, `net/http`, `net/http/httptest`,
`strings`, `sync`, `context`, `time`, and
`github.com/imantaba/kubeagent/internal/alertstate`. `at` is the package-level
fixture already declared in `encode_test.go`; `waitFor` is the helper at
`sink_test.go:16`.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert/ -run 'TestNew_Validation|TestDefaultRepeat|TestSink_PagerDuty' -p 2
```

Expected: FAIL — `unknown field RoutingKey in struct literal of type Config` at
build time.

- [ ] **Step 3: Add the field and the validation**

In `internal/alert/sink.go`, extend `Config`:

```go
// Config configures the sink. Repeat is used only to validate the cadence
// against the chosen format; the re-send itself is driven by alertstate.
// RoutingKey is the PagerDuty integration key and is required by — and used by
// — that format alone: PagerDuty authenticates in the request body, where every
// other receiver authenticates with the URL itself.
type Config struct {
	URL        string
	Format     Format
	Repeat     time.Duration
	RoutingKey string
}
```

Add the field to `Sink`, beside `format`:

```go
type Sink struct {
	url         string
	format      Format
	routingKey  string
	client      *http.Client
	queue       chan alertstate.Notification
	done        chan struct{}
	backoffBase time.Duration

	mu      sync.Mutex
	started bool
	stats   Stats
}
```

Extend `DefaultRepeat`'s doc comment (the function body does not change):

```go
// DefaultRepeat is the re-send interval for a format when the operator did not
// choose one. Alertmanager needs a short cadence because it expires an alert
// resolve_timeout after the last POST; json, slack and pagerduty are
// notification channels where a chatty default is alert fatigue. PagerDuty needs
// no ceiling to match maxAlertmanagerRepeat: it does not expire alerts, so a
// slow cadence produces no false recovery to guard against.
```

In `New`, extend the format switch and add the validation:

```go
	switch cfg.Format {
	case FormatJSON, FormatSlack, FormatAlertmanager, FormatPagerDuty:
	default:
		return nil, fmt.Errorf("unknown alert format %q (want json, slack, alertmanager, or pagerduty)", cfg.Format)
	}
	if cfg.Format == FormatPagerDuty {
		if err := validateRoutingKey(cfg.RoutingKey); err != nil {
			return nil, err
		}
	}
```

and set the field in the returned struct literal:

```go
	return &Sink{
		url:         u,
		format:      cfg.Format,
		routingKey:  cfg.RoutingKey,
		client:      c,
		queue:       make(chan alertstate.Notification, queueSize),
		done:        make(chan struct{}),
		backoffBase: defaultBackoff,
	}, nil
```

- [ ] **Step 4: Pass the key to the encoder**

In `deliver` (line 178), change `encode(s.format, "", n)` to:

```go
	body, err := encode(s.format, s.routingKey, n)
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
go test -race ./internal/alert
```

Expected: PASS both.

- [ ] **Step 6: Commit**

```bash
git add internal/alert/sink.go internal/alert/sink_test.go
git commit -s -m "alert: accept the pagerduty format on the sink

New validates the routing key when the format needs one and rejects the format
otherwise, so a missing credential fails at startup rather than at the first
HTTP 400. The delivery path itself is unchanged and that is the point: 202 hits
the existing status < 300 branch, 400 hits status < 500 and is not retried, and
the response body stays discarded — reading a 4xx body to improve a log line
would open a new untrusted-text ingress in the one package whose job is not
leaking things.

DefaultRepeat needs no new branch: 4h is already what every format but
alertmanager returns, and PagerDuty does not expire alerts, so it takes no
cadence ceiling."
```

---

## Task 4: The watch daemon's config and enable gate

**Files:**
- Modify: `internal/watch/watch.go` (`Config.AlertFormat` comment at line 47,
  a new field after line 48, `newAlerter` at 266-280)
- Test: `internal/watch/watch_test.go`

**Interfaces:**
- Consumes: `alert.Config{URL, Format, Repeat, RoutingKey}` (Task 3);
  `alert.DefaultURL(Format)` and `alert.FormatPagerDuty` (Tasks 1-2).
- Produces: `watch.Config.AlertRoutingKey string` — Task 5 sets it.

**The enable gate is the substance of this task.** Today alerting is off unless
`AlertURL` is set. A PagerDuty install may legitimately set no URL at all, so
the gate becomes: the webhook URL, **or** a routing key under the one format
that uses one. A routing key set under `json` leaves alerting off, which is what
makes Task 5's "ignored" warning literally true.

- [ ] **Step 1: Write the failing tests**

Add to `internal/watch/watch_test.go`:

```go
// A PagerDuty install authenticates with the routing key and may set no webhook
// URL at all, so the enable gate cannot be "AlertURL is set" any more. It is
// still off by default, and a routing key under a format that does not use one
// leaves it off rather than starting a sink that would post nowhere.
func TestNewAlerter_EnableGate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantOn  bool
		wantErr bool
	}{
		{"nothing configured", Config{AlertFormat: "json"}, false, false},
		{"webhook only", Config{AlertURL: "http://192.0.2.10:8080/hook", AlertFormat: "json"}, true, false},
		{"pagerduty routing key only", Config{AlertFormat: "pagerduty", AlertRoutingKey: "not-a-real-routing-key"}, true, false},
		{"routing key under json stays off", Config{AlertFormat: "json", AlertRoutingKey: "not-a-real-routing-key"}, false, false},
		{"pagerduty with a URL but no routing key fails loudly", Config{AlertURL: "http://192.0.2.10:8080/hook", AlertFormat: "pagerduty"}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			al, err := newAlerter(ctx, tc.cfg)
			if tc.wantErr != (err != nil) {
				t.Fatalf("newAlerter error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), "not-a-real-routing-key") {
					t.Errorf("the error echoes the routing key: %v", err)
				}
				return
			}
			if tc.wantOn != (al != nil) {
				t.Fatalf("alerting on = %v, want %v", al != nil, tc.wantOn)
			}
			if al != nil {
				cancel()
				al.sink.Close()
			}
		})
	}
}

// The startup line names the endpoint the sink actually resolved. A pagerduty
// install that set no URL must not see "endpoint=" with nothing after it — and
// the routing key must not appear at all.
func TestNewAlerter_StartupLogNamesTheResolvedEndpoint(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	al, err := newAlerter(ctx, Config{AlertFormat: "pagerduty", AlertRoutingKey: "not-a-real-routing-key", AlertRepeat: 4 * time.Hour})
	if err != nil {
		t.Fatalf("newAlerter: %v", err)
	}
	cancel()
	al.sink.Close()

	line := buf.String()
	if !strings.Contains(line, "endpoint=https://events.pagerduty.com") {
		t.Errorf("startup line does not name the resolved endpoint: %q", line)
	}
	if strings.Contains(line, "/v2/enqueue") {
		t.Errorf("startup line carries more than scheme://host: %q", line)
	}
	if strings.Contains(line, "not-a-real-routing-key") {
		t.Errorf("startup line carries the routing key: %q", line)
	}
}
```

Add `bytes`, `log`, `os` and `strings` to `watch_test.go`'s imports if missing.

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/watch/ -run 'TestNewAlerter' -p 2
```

Expected: FAIL — `unknown field AlertRoutingKey in struct literal of type
Config` at build time.

- [ ] **Step 3: Add the field**

In `internal/watch/watch.go`, replace lines 46-48 with:

```go
	AlertURL                string        // the receiver URL; for pagerduty it is optional and defaults to the published endpoint
	AlertFormat             string        // "json" | "slack" | "alertmanager" | "pagerduty"
	AlertRoutingKey         string        // PagerDuty integration key; used by that format alone
	AlertRepeat             time.Duration // re-send interval for still-firing alerts
```

- [ ] **Step 4: Rewrite `newAlerter`**

Replace `newAlerter` (lines 266-280) with:

```go
// newAlerter builds the alerter from the config, returning nil when no
// credential is configured. Every format but pagerduty authenticates with the
// webhook URL; pagerduty authenticates with the routing key and defaults its
// endpoint, so either one turns alerting on. A routing key set under another
// format is not a credential for that format and leaves alerting off. Both are
// credentials: only scheme://host is ever logged, and the key is never logged
// at all.
func newAlerter(ctx context.Context, cfg Config) (*alerter, error) {
	format := alert.Format(cfg.AlertFormat)
	if cfg.AlertURL == "" && !(format == alert.FormatPagerDuty && cfg.AlertRoutingKey != "") {
		return nil, nil
	}
	sink, err := alert.New(alert.Config{
		URL:        cfg.AlertURL,
		Format:     format,
		Repeat:     cfg.AlertRepeat,
		RoutingKey: cfg.AlertRoutingKey,
	}, nil)
	if err != nil {
		return nil, err
	}
	sink.Start(ctx)
	endpoint := cfg.AlertURL
	if endpoint == "" {
		endpoint = alert.DefaultURL(format)
	}
	log.Printf("kubeagent: alerting enabled (format=%s, repeat=%s, endpoint=%s)", format, cfg.AlertRepeat, redact.URL(endpoint))
	return &alerter{sink: sink}, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
go test -race ./internal/watch
```

Expected: PASS both. `TestAlerter_NilIsDisabled` and
`TestRun_RejectsBadAlertConfigBeforeStartingAnything` must still pass unchanged.

- [ ] **Step 6: Commit**

```bash
git add internal/watch/watch.go internal/watch/watch_test.go
git commit -s -m "watch: enable alerting on either credential

A PagerDuty install authenticates with the routing key and may configure no
webhook URL at all, so the enable gate is no longer 'AlertURL is set'. Alerting
is still off by default, and a routing key set under a format that does not use
one leaves it off rather than starting a sink that would post nowhere.

The startup line names the endpoint the sink resolved rather than the raw
config, so a pagerduty install that set no URL no longer prints 'endpoint=' with
nothing after it. It stays redacted to scheme://host, and the routing key never
reaches it."
```

---

## Task 5: The CLI

**Files:**
- Modify: `internal/cli/watch.go` (`--alert-format` help at line 111;
  `runWatchOpts` at 141-152; the `watch.Config` literal at 212-243)
- Modify: `internal/cli/root.go:91` (the usage string)
- Test: `internal/cli/cli_test.go`

**Interfaces:**
- Consumes: `watch.Config.AlertRoutingKey` (Task 4).
- Produces: nothing later tasks call. This is the last Go task on the path.

**There is no `internal/cli/watch_test.go`** — watch's CLI tests live in
`internal/cli/cli_test.go` and `internal/cli/surface_test.go`.
`KUBEAGENT_ALERT_ROUTING_KEY` is **not** a flag default, so
`surface_test.go`'s env-key list at lines 112-118 does **not** change.

- [ ] **Step 1: Write the failing tests**

Add to `internal/cli/cli_test.go`. The file already has the pattern for
capturing stderr — reuse whatever helper the neighbouring warning tests at lines
450-510 use, and follow their shape exactly.

```go
// The routing key is a credential and inherits the webhook URL's rule: no flag,
// because a flag would put it in the pod spec's args and in `ps` output.
func TestRunWatch_NoRoutingKeyFlagExists(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	err := runWatch([]string{"--alert-routing-key", "not-a-real-routing-key", "--kubeconfig", bad})
	if err == nil {
		t.Fatal("expected no --alert-routing-key flag to exist")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected an unknown-flag error, got: %v", err)
	}
}

// A routing key under a format that does not use one is a configuration
// mistake, and a silent ignore is how it stays one.
func TestRunWatch_WarnsWhenTheRoutingKeyIsSetUnderAnotherFormat(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "json", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_ROUTING_KEY is set but --alert-format is json") {
		t.Fatalf("expected the wrong-format warning on stderr, got: %q", stderr)
	}
	if strings.Contains(stderr, "not-a-real-routing-key") {
		t.Fatalf("the warning echoes the routing key: %q", stderr)
	}
}

// A routing key with --alert-format pagerduty turns alerting on with no webhook
// URL set, so the "alerting is off" warning must not fire.
func TestRunWatch_RoutingKeyAloneEnablesPagerDuty(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "pagerduty", "--kubeconfig", bad})
	})
	if strings.Contains(stderr, "alert-* flags ignored") {
		t.Fatalf("unexpected alerting-is-off warning with a routing key set: %q", stderr)
	}
	if strings.Contains(stderr, "not-a-real-routing-key") {
		t.Fatalf("stderr carries the routing key: %q", stderr)
	}
}

// With neither credential set, the existing warning still fires and now names
// both.
func TestRunWatch_WarnsWhenNeitherCredentialIsSet(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "")
	bad := filepath.Join(t.TempDir(), "nonexistent-kubeconfig")
	stderr := captureStderr(t, func() {
		_ = runWatch([]string{"--alert-format", "pagerduty", "--kubeconfig", bad})
	})
	if !strings.Contains(stderr, "--alert-* flags ignored") {
		t.Fatalf("expected the ignored-alert-flags warning on stderr, got: %q", stderr)
	}
	if !strings.Contains(stderr, "KUBEAGENT_ALERT_ROUTING_KEY") {
		t.Fatalf("the warning does not name the pagerduty credential: %q", stderr)
	}
}
```

`captureStderr(t, func())` already exists at `internal/cli/cli_test.go:373` —
`runWatch` prints to `os.Stderr` directly rather than through an injectable
writer, which is what that helper is for. Use it; do not add a second one.

`internal/cli/cli_test.go:723` has a second helper worth using here —
`captureWatchConfig(t, args) (watch.Config, error)`, which returns the
`watch.Config` the CLI built without starting a daemon. It asserts the wiring
directly rather than by inference, so add this test too:

```go
// The env var must reach watch.Config, not merely fail to warn.
func TestRunWatch_RoutingKeyReachesTheConfig(t *testing.T) {
	t.Setenv("KUBEAGENT_ALERT_WEBHOOK", "")
	t.Setenv("KUBEAGENT_ALERT_ROUTING_KEY", "not-a-real-routing-key")
	cfg, err := captureWatchConfig(t, []string{"--alert-format", "pagerduty"})
	if err != nil {
		t.Fatalf("captureWatchConfig: %v", err)
	}
	if cfg.AlertRoutingKey != "not-a-real-routing-key" {
		t.Errorf("AlertRoutingKey = %q, want the value from the environment", cfg.AlertRoutingKey)
	}
	if cfg.AlertURL != "" {
		t.Errorf("AlertURL = %q, want empty — pagerduty defaults its endpoint in the sink", cfg.AlertURL)
	}
	// 4h is DefaultRepeat's answer for every format but alertmanager.
	if cfg.AlertRepeat != 4*time.Hour {
		t.Errorf("AlertRepeat = %s, want 4h0m0s", cfg.AlertRepeat)
	}
}
```

Read `captureWatchConfig`'s signature before using it — if it takes or returns
something different from the line above, follow the file, not this plan.

Also update the existing usage assertion at `internal/cli/cli_test.go:515`:

```go
	if !strings.Contains(err.Error(), "[--alert-format json|slack|alertmanager|pagerduty] [--alert-repeat dur]") {
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/cli/ -run 'TestRunWatch' -p 2
```

Expected: FAIL — the new warnings are not printed and the usage string still
reads `json|slack|alertmanager`.

- [ ] **Step 3: Read the routing key and rework the warnings**

In `internal/cli/watch.go`, replace lines 142-152 with:

```go
	// The webhook URL and the PagerDuty routing key are both credentials — a
	// Slack incoming-webhook URL is a bearer token in URL form, and a routing
	// key is one outright — so they come from the environment only, never a
	// flag, which would put them in the pod spec's args and in `ps` output.
	alertURL := os.Getenv("KUBEAGENT_ALERT_WEBHOOK")
	routingKey := os.Getenv("KUBEAGENT_ALERT_ROUTING_KEY")
	repeat := o.alertRepeat
	if repeat == 0 {
		repeat = alert.DefaultRepeat(alert.Format(o.alertFormat))
	}
	// Alerting is on when the format's own credential is present: the URL for
	// every format, or the routing key for pagerduty, which defaults its
	// endpoint.
	alerting := alertURL != "" || (o.alertFormat == string(alert.FormatPagerDuty) && routingKey != "")
	if !alerting && (o.alertFormat != "json" || o.alertRepeat != 0) {
		warnf(os.Stderr, "--alert-* flags ignored: neither KUBEAGENT_ALERT_WEBHOOK nor KUBEAGENT_ALERT_ROUTING_KEY (with --alert-format pagerduty) is set, so alerting is off")
	}
	if routingKey != "" && o.alertFormat != string(alert.FormatPagerDuty) {
		warnf(os.Stderr, "KUBEAGENT_ALERT_ROUTING_KEY is set but --alert-format is %s: the routing key is used by the pagerduty format alone", o.alertFormat)
	}
```

In the `watch.Config` literal, add the field beside `AlertFormat`:

```go
		AlertURL:                alertURL,
		AlertFormat:             o.alertFormat,
		AlertRoutingKey:         routingKey,
		AlertRepeat:             repeat,
```

- [ ] **Step 4: Update the flag help and the usage string**

`internal/cli/watch.go:111`:

```go
	f.StringVar(&o.alertFormat, "alert-format", envOr("KUBEAGENT_ALERT_FORMAT", "json"), "alert payload format: json, slack, alertmanager, or pagerduty")
```

`internal/cli/root.go:91` — inside the long usage string, change the single
substring `[--alert-format json|slack|alertmanager]` to
`[--alert-format json|slack|alertmanager|pagerduty]`. Change nothing else in
that line.

- [ ] **Step 5: Run the tests to verify they pass**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
go test -race ./internal/alert ./internal/watch ./internal/cli
```

Expected: PASS both.

- [ ] **Step 6: Confirm the golden scan output did not move**

```bash
git status --short internal/report/testdata/golden-scan.txt
```

Expected: no output.

- [ ] **Step 7: Commit**

```bash
git add internal/cli/watch.go internal/cli/root.go internal/cli/cli_test.go
git commit -s -m "cli: accept --alert-format pagerduty and read the routing key

KUBEAGENT_ALERT_ROUTING_KEY is read from the environment with no flag, for the
same reason KUBEAGENT_ALERT_WEBHOOK has none: a flag would put the credential in
the pod spec's args and in \`ps\` output.

Two warnings rather than one silent ignore. The existing 'alerting is off'
warning now names both credentials, and setting a routing key under a format
that does not use one says so instead of dropping it quietly. Neither echoes the
value."
```

---

## Task 6: The fuzz target

**Files:**
- Create: `internal/alert/fuzz_test.go`
- Modify: `.github/workflows/fuzz.yml` (the matrix `include:` list, lines 29-54)

**Interfaces:**
- Consumes: `encode`, `FormatPagerDuty`, `pdMaxDedupKey` (Task 1);
  `alertstate.Notification`.
- Produces: `FuzzEncodePagerDuty` — nothing calls it but the toolchain.

`internal/alert` has no fuzz target today, so this is the package's first.
**Seed corpora in this repo are `f.Add(...)` calls in code**, not
`testdata/fuzz/` directories — match `internal/redact/fuzz_test.go` and
`internal/dashboard/fuzz_test.go`. The workflow enumerates its
`(package, target)` pairs by hand, so a target that is not added there never
runs a real campaign.

- [ ] **Step 1: Write the fuzz target**

Create `internal/alert/fuzz_test.go`:

```go
package alert

import (
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/imantaba/kubeagent/internal/alertstate"
)

// FuzzEncodePagerDuty asserts the three properties the receiver depends on, for
// any object name and any explanation prose: the encoder never panics, it always
// produces valid JSON, and dedup_key never exceeds PagerDuty's cap. The third is
// the one worth fuzzing — a Kubernetes name may be 253 characters on its own, so
// the cluster/kind/namespace/name concatenation reaches the cap with entirely
// legal input.
//
// The count is in characters, not bytes: json.Marshal replaces an invalid UTF-8
// byte with U+FFFD, so the marshalled key can be longer in bytes than what
// dedupKey returned, but never longer in characters.
func FuzzEncodePagerDuty(f *testing.F) {
	f.Add("local", "Deployment", "shop", "web", "ImagePullBackOff", "")
	f.Add("prod-eu", "Node", "", "worker-2", "NodeNotReady", "The kubelet stopped reporting.")
	f.Add("local", "Deployment", "shop", "web", "CrashLoopBackOff", "\"}]} <script>alert(1)</script>\n*not a header*\x07")
	f.Add("", "", "", "", "", "")
	f.Add("\xff\xfe", "Deployment", "shop", "\xc3", "ImagePullBackOff", "\xff")
	f.Add("local", "Deployment", "shop", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ImagePullBackOff", "")

	f.Fuzz(func(t *testing.T, cluster, kind, namespace, name, issue, text string) {
		for _, status := range []alertstate.Status{alertstate.StatusFiring, alertstate.StatusResolved} {
			n := alertstate.Notification{
				Object:      alertstate.Object{Cluster: cluster, Kind: kind, Namespace: namespace, Name: name},
				Status:      status,
				Reason:      alertstate.ReasonNew,
				Issues:      []string{issue},
				FiringSince: time.Date(2026, 8, 5, 10, 4, 11, 0, time.UTC),
				Text:        text,
			}
			body, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
			if err != nil {
				t.Fatalf("encode returned an error: %v", err)
			}
			var got struct {
				DedupKey string `json:"dedup_key"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("encoder produced invalid JSON: %v", err)
			}
			if c := utf8.RuneCountInString(got.DedupKey); c > pdMaxDedupKey {
				t.Errorf("dedup_key is %d characters, want at most %d", c, pdMaxDedupKey)
			}
			// Determinism: the same notification must encode byte-identically.
			again, err := encode(FormatPagerDuty, "not-a-real-routing-key", n)
			if err != nil {
				t.Fatalf("encode returned an error on the second call: %v", err)
			}
			if string(again) != string(body) {
				t.Errorf("encode is not deterministic")
			}
		}
	})
}
```

- [ ] **Step 2: Run the seed corpus and a short campaign**

```bash
export PATH=$PATH:/usr/local/go/bin
go test ./internal/alert -run '^$' -fuzz '^FuzzEncodePagerDuty$' -fuzztime 60s
```

Expected: the seeds replay, then `elapsed: 60s` with no new interesting inputs
and no crashers. If it finds one, fix the encoder — a crasher is a real defect,
not a reason to weaken the assertion.

- [ ] **Step 3: Check that no crasher corpus was left behind**

```bash
git status --short internal/alert/
```

Expected: only `fuzz_test.go` as untracked. A `testdata/fuzz/` directory means
the campaign found something — fix it before continuing. `go test ./... -p 2`
also replays the seeds on every run.

- [ ] **Step 4: Register the target in the nightly matrix**

In `.github/workflows/fuzz.yml`, append to the matrix `include:` list, after the
`internal/dashboard` pair:

```yaml
          - package: ./internal/alert
            target: FuzzEncodePagerDuty
```

- [ ] **Step 5: Verify the workflow still parses**

```bash
python3 -c "import yaml,sys; d=yaml.safe_load(open('.github/workflows/fuzz.yml')); print(len(d['jobs']['fuzz']['strategy']['matrix']['include']), 'matrix pairs')"
```

Expected: `14 matrix pairs`.

- [ ] **Step 6: Run the full suite**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/alert/fuzz_test.go .github/workflows/fuzz.yml
git commit -s -m "alert: fuzz the PagerDuty encoder

internal/alert's first fuzz target. It asserts what a receiver depends on for
any object name and any explanation prose: the encoder never panics, always
produces valid JSON, is deterministic, and never emits a dedup_key past
PagerDuty's 255-character cap. That last one is reachable with entirely legal
input — a Kubernetes name may be 253 characters on its own.

The count is in characters rather than bytes because json.Marshal replaces an
invalid UTF-8 byte with U+FFFD, which is longer in bytes and not in characters.

The nightly workflow enumerates its (package, target) pairs by hand, so the pair
is registered there or the campaign never runs."
```

---

## Task 7: The Helm chart

**Files:**
- Modify: `deploy/helm/kubeagent/values.yaml` (the `alerts:` block, lines 92-104
  — comments only)
- Modify: `deploy/helm/kubeagent/templates/deployment.yaml` (the env block at
  lines 141-147)
- Modify: `deploy/helm/kubeagent/Chart.yaml` (`version: 0.25.1` → `0.26.0`)

**Interfaces:**
- Consumes: `KUBEAGENT_ALERT_ROUTING_KEY` (Task 5) and `--alert-format
  pagerduty` (Task 5).
- Produces: nothing later tasks call.

**Zero new values.** `alerts.format` accepts `pagerduty`; the template points
the existing `alerts.existingSecret` / `alerts.secretKey` pair at
`KUBEAGENT_ALERT_ROUTING_KEY` instead of `KUBEAGENT_ALERT_WEBHOOK` when the
format is `pagerduty`. Templates **and** values both move, so the chart takes a
**MINOR** bump — `0.25.1` → `0.26.0` — which overrides
`scripts/bump-version.sh`'s patch bump at release time.

- [ ] **Step 1: Capture the current default render, to prove it does not move**

```bash
export PATH=$PATH:$HOME/.local/bin
helm template x deploy/helm/kubeagent > /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chart-before.yaml
wc -l /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chart-before.yaml
```

- [ ] **Step 2: Update the `alerts:` comments in `values.yaml`**

Replace lines 92-104 of `deploy/helm/kubeagent/values.yaml` with:

```yaml
alerts:
  # Route watch transitions to an outbound receiver (one alert per broken
  # object). The credential is never set here: point existingSecret at a Secret
  # that already holds it. The daemon stays read-only toward the cluster.
  enabled: false
  # json | slack | alertmanager | pagerduty
  format: json
  # Re-send interval for still-firing alerts. Empty = the format default
  # (4h, or 60s for alertmanager, which expires alerts after resolve_timeout).
  repeat: ""
  # Name of an existing Secret holding the credential. Required when enabled.
  # For json, slack and alertmanager that is the webhook URL, delivered as
  # KUBEAGENT_ALERT_WEBHOOK. For pagerduty it is the Events API v2 integration
  # key, delivered as KUBEAGENT_ALERT_ROUTING_KEY; the endpoint defaults to
  # PagerDuty's published one and needs no value here.
  existingSecret: ""
  secretKey: webhook-url
```

- [ ] **Step 3: Add the conditional to `deployment.yaml`**

Replace the env block at lines 141-147 with:

```yaml
            {{- if .Values.alerts.enabled }}
            {{- /*
              PagerDuty authenticates with an integration key in the request
              body, where every other receiver authenticates with the URL
              itself. Same Secret shape either way — only the variable name
              changes, so an operator adds no new values to switch.
            */}}
            - name: {{ if eq .Values.alerts.format "pagerduty" }}KUBEAGENT_ALERT_ROUTING_KEY{{ else }}KUBEAGENT_ALERT_WEBHOOK{{ end }}
              valueFrom:
                secretKeyRef:
                  name: {{ required "alerts.existingSecret is required when alerts.enabled is true" .Values.alerts.existingSecret | quote }}
                  key: {{ .Values.alerts.secretKey | quote }}
            {{- end }}
```

- [ ] **Step 4: Bump the chart version**

In `deploy/helm/kubeagent/Chart.yaml`, change `version: 0.25.1` to
`version: 0.26.0`. Leave `appVersion: "v1.2.0"` alone — the release skill moves
it.

- [ ] **Step 5: Lint and render both paths**

```bash
export PATH=$PATH:$HOME/.local/bin
helm lint deploy/helm/kubeagent

# The default render must be byte-identical to before this task.
helm template x deploy/helm/kubeagent > /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chart-after.yaml
diff /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chart-{before,after}.yaml && echo "DEFAULT RENDER UNCHANGED"

# A slack install still gets the webhook variable.
helm template x deploy/helm/kubeagent \
  --set alerts.enabled=true --set alerts.format=slack \
  --set alerts.existingSecret=kubeagent-alerts \
  | grep -A5 'KUBEAGENT_ALERT_WEBHOOK'

# A pagerduty install gets the routing-key variable from the same Secret shape.
helm template x deploy/helm/kubeagent \
  --set alerts.enabled=true --set alerts.format=pagerduty \
  --set alerts.existingSecret=kubeagent-pagerduty \
  --set alerts.secretKey=routing-key \
  | grep -A5 'KUBEAGENT_ALERT_ROUTING_KEY'

# The required-value guard still fires when the Secret is not named.
helm template x deploy/helm/kubeagent \
  --set alerts.enabled=true --set alerts.format=pagerduty 2>&1 \
  | grep -c 'alerts.existingSecret is required'
```

Expected: `helm lint` reports `1 chart(s) linted, 0 chart(s) failed`;
`DEFAULT RENDER UNCHANGED`; the slack render shows `KUBEAGENT_ALERT_WEBHOOK` and
the pagerduty render shows `KUBEAGENT_ALERT_ROUTING_KEY`, each with
`secretKeyRef`; the last command prints `1`.

- [ ] **Step 6: Confirm no rendered manifest carries a credential**

```bash
export PATH=$PATH:$HOME/.local/bin
helm template x deploy/helm/kubeagent \
  --set alerts.enabled=true --set alerts.format=pagerduty \
  --set alerts.existingSecret=kubeagent-pagerduty \
  --set alerts.secretKey=routing-key \
  | grep -ci 'routing_key\|not-a-real'
```

Expected: `0` — the manifest names a Secret and a key, never a value.

- [ ] **Step 7: Commit**

```bash
git add deploy/helm/kubeagent/values.yaml deploy/helm/kubeagent/templates/deployment.yaml deploy/helm/kubeagent/Chart.yaml
git commit -s -m "chart: wire the alert Secret to the routing key for pagerduty

Zero new values. The same existingSecret/secretKey pair now feeds
KUBEAGENT_ALERT_ROUTING_KEY when the format is pagerduty and
KUBEAGENT_ALERT_WEBHOOK otherwise, so switching receivers is a one-line change
and the credential still never appears in values.yaml or in Helm's release
history.

Templates and values both moved, so the chart takes a MINOR bump."
```

---

## Task 8: Chaos scenario 23

**Files:**
- Modify: `chaos/run.sh` (the `--only` comment at line 26; a new
  `scenario_23_pagerduty` function after `scenario_22_dnshealth`; the `all=(…)`
  array in `run_scenarios` at line 1868)
- Modify: `chaos/README.md` (line 46, 110, 115, 148, 182, 191-192, and a new
  row 23 in the scenario table)
- Modify: `website/docs/compatibility.md` (lines 121 and 123)
- Modify: `website/docs/roadmap.md` (lines 516 and 530)
- Modify: `CLAUDE.md` (line 264)

**Interfaces:**
- Consumes: `--alert-format pagerduty` and `KUBEAGENT_ALERT_ROUTING_KEY`
  (Task 5); `chaos/assert.sh`'s `expect_eq` / `expect_ge` / `expect_contains`;
  `chaos/alert-receiver.py`, which appends each POSTed body to a file one JSON
  document per line and answers **200** (not 202 — the scenario must not depend
  on the status code).
- Produces: nothing later tasks call.

**Harness rules that are not negotiable:**
- **No helper may return non-zero.** The harness runs under `set -euo pipefail`;
  a failing assertion must let the remaining scenarios run and surface at the end
  in the exit code.
- **`scenario_01_etcd` stays LAST** in `run_scenarios`'s array. Scenario 23 goes
  immediately before it.
- **`|| true` on every `grep`-terminated capture** — grep exits 1 on zero
  matches, and a bare `var=$(...)` that fails aborts the whole scenario before
  its namespace cleanup runs.
- **Count, never quote, when asserting a credential is absent.** `assert.sh`
  embeds its needle in the PASS/FAIL line, so an `expect_absent` on the routing
  key would write the key into the report on every passing run — the exact leak
  the assertion exists to rule out. Scenario 12's `dash_webhook` capture at
  `chaos/run.sh:687` is the model.
- The port pair must not collide: scenario 12 uses 18080/18081, 13 uses
  18082/18083. Scenario 23 uses **18096/18097** — verify with
  `grep -n '180[0-9][0-9]' chaos/run.sh` before writing and pick the next free
  pair if that one is taken.

**Live verification:** no kind cluster is currently up on this machine (the
v1.2.0 gate's cluster was deleted). Either create one with
`./chaos/run.sh --only 23 --recreate` (~10 minutes, cluster + Calico + one
scenario) or defer to the slice's final full gate and **say so explicitly in the
task report**. `bash -n chaos/run.sh` is the cheap syntax gate either way.

- [ ] **Step 1: Add the scenario, with its assertions written to fail first**

Insert this function into `chaos/run.sh` immediately after
`scenario_22_dnshealth`'s closing brace:

```bash
scenario_23_pagerduty() {   # PagerDuty receiver: trigger on outage, resolve on repair, one dedup_key, no key in the log
  log "scenario 23: PagerDuty alert format (trigger/resolve on one dedup_key)"
  local ns=chaos-pagerduty port=18096 aport=18097 wlog wpid i events apid
  wlog="$(mktemp)"
  events="$(mktemp)"
  # The receiver stands in for events.pagerduty.com. KUBEAGENT_ALERT_WEBHOOK
  # overrides the default endpoint, which is what makes this format testable at
  # all without reaching PagerDuty.
  python3 chaos/alert-receiver.py "$aport" "$events" >/dev/null 2>&1 &
  apid=$!
  kubectl --context "$CTX" create ns "$ns" --dry-run=client -o yaml | kubectl --context "$CTX" apply -f - >/dev/null
  kubectl --context "$CTX" -n "$ns" apply -f chaos/manifests/app.yaml >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=90s >/dev/null 2>&1 || true

  # The routing key is a credential and has no flag: it comes from the
  # environment, exactly like the webhook URL beside it. This value is a fixture,
  # not a key — no PagerDuty account is contacted by this scenario.
  KUBEAGENT_ALERT_WEBHOOK="http://127.0.0.1:$aport" \
  KUBEAGENT_ALERT_ROUTING_KEY="not-a-real-routing-key" \
  ./kubeagent watch --context "$CTX" -n "$ns" --metrics-addr "127.0.0.1:$port" \
    --heartbeat 10s --debounce 2s --alert-format pagerduty --alert-repeat 1h >"$wlog" 2>&1 &
  wpid=$!
  for i in $(seq 40); do
    curl -sf "http://127.0.0.1:$port/readyz" >/dev/null 2>&1 && break
    sleep 1
  done

  # Same outage as scenario 9: a bad image, with the old replicas taken down by
  # the rollout so Ready < Desired.
  kubectl --context "$CTX" -n "$ns" patch deploy web --type=strategic \
    -p '{"spec":{"strategy":{"rollingUpdate":{"maxUnavailable":"100%"}}}}' >/dev/null
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:does-not-exist-9999 >/dev/null
  sleep 30

  # Repair and let the tracker observe the issue clear.
  kubectl --context "$CTX" -n "$ns" set image deploy/web web=nginx:1.27-alpine >/dev/null
  kubectl --context "$CTX" -n "$ns" rollout status deploy web --timeout=120s >/dev/null 2>&1 || true
  sleep 30

  kill "$wpid" >/dev/null 2>&1 || true
  wait "$wpid" >/dev/null 2>&1 || true
  kill "$apid" >/dev/null 2>&1 || true
  wait "$apid" >/dev/null 2>&1 || true

  # Every capture is `|| true` guarded: grep exits 1 on zero matches, and under
  # `set -euo pipefail` a bare assignment that fails would abort the scenario
  # before the namespace cleanup at the bottom ever ran.
  local trigger_n resolve_n event_n keyed_n web_dedup_n resolve_dedup key_in_log
  trigger_n="$(grep -c '"event_action":"trigger"' "$events" 2>/dev/null || true)"
  resolve_n="$(grep -c '"event_action":"resolve"' "$events" 2>/dev/null || true)"
  event_n="$(grep -c '"event_action"' "$events" 2>/dev/null || true)"
  # The routing key must be in every body. Counting both sides and comparing two
  # numbers keeps the key itself out of the assertion label.
  keyed_n="$(grep -cF -- '"routing_key":"not-a-real-routing-key"' "$events" 2>/dev/null || true)"
  # The property the whole format rests on: one object, one incident key, across
  # its trigger and its resolve. Not a count of all keys — how many other objects
  # break alongside the Deployment depends on timing, and asserting that number
  # would be asserting the scheduler rather than the encoder.
  web_dedup_n="$(grep -o "\"dedup_key\":\"[^\"]*/$ns/web\"" "$events" 2>/dev/null | sort -u | wc -l || true)"
  resolve_dedup="$(grep '"event_action":"resolve"' "$events" 2>/dev/null | grep -o '"dedup_key":"[^"]*"' | sort -u || true)"
  # Count, never quote: assert.sh embeds its needle in the PASS/FAIL line, so an
  # expect_absent here would write the routing key into the report on every
  # passing run — the leak this assertion exists to rule out.
  key_in_log="$(grep -cF -- "not-a-real-routing-key" "$wlog" || true)"

  {
    echo '--- PagerDuty events delivered to the receiver ---'
    { grep -o '"event_action":"[a-z]*","dedup_key":"[^"]*"' "$events" || echo '<no events delivered>'; }
    echo
    printf 'trigger events: %s\n' "$trigger_n"
    printf 'resolve events: %s\n' "$resolve_n"
    printf 'events carrying a routing key: %s of %s\n' "$keyed_n" "$event_n"
    printf 'distinct dedup keys for %s/web: %s\n' "$ns" "$web_dedup_n"
    echo
    echo '--- routing-key redaction check (the daemon log must never carry it) ---'
    printf 'daemon log lines carrying the routing key: %s\n' "$key_in_log"
    echo
    echo '--- assertions ---'
    expect_ge       "trigger events delivered"                        "$trigger_n"   1
    expect_ge       "resolve events delivered after the repair"       "$resolve_n"   1
    expect_eq       "every delivered event carries the routing key"   "$keyed_n"     "$event_n"
    expect_eq       "the Deployment fires on exactly one dedup key"   "$web_dedup_n" 1
    expect_contains "the resolve carries the Deployment's dedup key"  "$resolve_dedup" "$ns/web"
    expect_eq       "daemon log carries no routing key"               "$key_in_log"  0
  } | record "23. PagerDuty receiver (trigger on outage, resolve on repair, one dedup_key per object)" "expect: the daemon posts Events API v2 bodies to a local stand-in for events.pagerduty.com — a trigger while the Deployment is broken, a resolve after the repair, and both on the same identity-derived dedup_key, which is what makes a daemon restart re-trigger onto the open incident instead of opening a second one. The routing key travels in the request body only: it is in every delivered event and in no line of the daemon's log."

  rm -f "$wlog" "$events"
  kubectl --context "$CTX" delete ns "$ns" --wait=true --timeout=120s >/dev/null 2>&1 || true
}
```

Register it in `run_scenarios` — scenario 23 goes **before** `01_etcd`, which
stays last:

```bash
  local all=(02_certs 03_diskfull 04_networkpolicy 05_coredns 06_lb 07_oom 08_nsdelete 09_rollout 10_credleak 11_kubelet 12_watch 13_slo 14 15_multicluster 16_operators 17_gitops 18_capacity 19_mcp 20_rbac 21_controlplane 22_dnshealth 23_pagerduty 01_etcd)
```

And update the `--only` normalization comment at line 26:

```bash
# Normalize a numeric --only to the zero-padded form used in scenario keys (01..23).
```

- [ ] **Step 2: Syntax-check the harness**

```bash
bash -n chaos/run.sh && echo "SYNTAX OK"
grep -c 'scenario_23_pagerduty' chaos/run.sh
grep -n '180[0-9][0-9]' chaos/run.sh | grep -c '18096\|18097'
```

Expected: `SYNTAX OK`; `2` (the definition and the array entry); `2` (the two
new ports, appearing once each in the new function's `local` line — adjust if
the grep shape differs, the point is that neither port is used by another
scenario).

- [ ] **Step 3: Run the scenario against a cluster**

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --only 23 --recreate --out /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chaos-23.md
```

Expected: exit `0` and `assertions: 6 run, 0 failed`. If no cluster can be
created in this environment, skip this step, run Step 2 only, and **state in the
task report that scenario 23 has not been executed live and is deferred to the
slice's full gate**. Do not claim it passed.

- [ ] **Step 4: Confirm the report carries no credential**

```bash
grep -c 'not-a-real-routing-key' /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chaos-23.md
```

Expected: `0`. Skip if Step 3 was skipped.

- [ ] **Step 5: Move the assertion count 128 → 134 and the scenario count 22 → 23**

The scenario runs **six** `expect_*` calls, so the suite moves from 128 to
**134**. The spec budgeted four assertions and named them; this scenario ships
six, because two properties the spec folded into its prose are worth asserting
separately — that the routing key is in *every* delivered body, and that the
Deployment fires on exactly one dedup key rather than merely that a resolve
mentions it. More coverage than the spec asked for is not a reopened decision;
fewer would be.

If Step 3 ran live, the summary line is the authority — confirm the number
before editing eleven files against it:

```bash
grep 'assertions:' /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/chaos-23.md || true
```

Expected: `assertions: 6 run, 0 failed`. Update that count, and the scenario
count, in all five documents:

| File | Line | Change |
|---|---|---|
| `CLAUDE.md` | 264 | `128 machine-checked assertions` → `134 machine-checked assertions` |
| `chaos/README.md` | 46 | `# run a single scenario (1..22)` → `(1..23)` |
| `chaos/README.md` | 110 | `the baseline plus all 22 scenarios below` → `all 23 scenarios below` |
| `chaos/README.md` | 115 | `on three kind-hosted minors under twenty-two` → `under twenty-three` |
| `chaos/README.md` | 148 | `Each cell runs 128 assertions.` → `runs 134 assertions.` |
| `chaos/README.md` | 182 | `The baseline and all 22 scenarios` → `all 23 scenarios` |
| `chaos/README.md` | 191-192 | `Twenty-two specific injected outages passing … on those twenty-two` → `Twenty-three … on those twenty-three` |
| `website/docs/compatibility.md` | 121 | `the full 22-scenario chaos suite` → `23-scenario` |
| `website/docs/compatibility.md` | 123 | `with 128 machine-checked assertions per cell` → `134` |
| `website/docs/roadmap.md` | 516 | `runs the full 22-scenario suite` → `23-scenario` |
| `website/docs/roadmap.md` | 530 | `the nightly matrix passes 128 assertions against each` → `134 assertions` |

Read each line before editing it — the line numbers above are from the tree at
plan time and a neighbouring edit may have shifted them.

Add a row 23 to `chaos/README.md`'s scenario table, after row 22:

```markdown
| 23 | PagerDuty receiver | run `kubeagent watch --alert-format pagerduty` with the routing key in the environment and the endpoint pointed at a local stand-in for `events.pagerduty.com`, then inject and repair the bad-image outage | an Events API v2 `trigger` while the Deployment is broken and a `resolve` after the repair, both on the same identity-derived `dedup_key` — the property that makes a daemon restart re-trigger onto the open incident instead of opening a second one — and the routing key, which travels in the request body only, in **no** line of the daemon's log |
```

- [ ] **Step 6: Verify the counts are consistent everywhere**

```bash
grep -rn '\b128\b' CLAUDE.md chaos/README.md website/docs/compatibility.md website/docs/roadmap.md
grep -rn '22 scenario\|1\.\.22\|all 22\|twenty-two' chaos/README.md website/docs/compatibility.md website/docs/roadmap.md
```

Expected: no output from either — every stale count is gone.

- [ ] **Step 7: Commit**

```bash
git add chaos/run.sh chaos/README.md website/docs/compatibility.md website/docs/roadmap.md CLAUDE.md
git commit -s -m "chaos: scenario 23 asserts the PagerDuty receiver end to end

A daemon in pagerduty format, its endpoint pointed at a local stand-in for
events.pagerduty.com, must deliver a trigger while the Deployment is broken and
a resolve after the repair — both on the same identity-derived dedup_key, which
is the property the whole format rests on.

The fifth assertion could not be written any other way: the routing key must
appear in no line of a live process's stderr, which is a statement about a
running daemon rather than a function's return value. It counts rather than
quotes, because assert.sh embeds its needle in the PASS/FAIL line and an absence
check would print what it just proved absent.

Twenty-three scenarios, 134 assertions."
```

---

## Task 9: Documentation

**Files:**
- Modify: `website/docs/features/watch-mode.md` (the flag table at 286-289; the
  credential paragraph at 275-284; a new subsection under `### Payloads`; the
  `### No severity` section at 334)
- Modify: `website/docs/roadmap.md` (Theme E's bullet, line 440)
- Modify: `deploy/README.md` (the "Alerting (opt-in)" section at 174-194)
- Modify: `CHANGELOG.md` (`## [Unreleased]`, a new `### Added`)

**Interfaces:**
- Consumes: everything above. This task documents; it changes no code.
- Produces: nothing.

`website/docs/compatibility.md` needs **no** new promise beyond Task 8's
assertion count: a flag accepting a new value is additive under the stable-CLI
rules already written there.

- [ ] **Step 1: Update the flag table and the credential paragraph**

In `website/docs/features/watch-mode.md`, replace lines 275-289 with:

````markdown
Enable it by setting the credential in the environment:

```bash
export KUBEAGENT_ALERT_WEBHOOK=<WEBHOOK_URL>
kubeagent watch --alert-format slack
```

There is no `--alert-webhook` flag on purpose: a Slack incoming-webhook URL is a
bearer token in URL form, and a flag would put it in the pod spec's args and in
`ps` output. Only `scheme://host` is ever logged.

`pagerduty` authenticates differently — with an Events API v2 integration key in
the request **body**, not with the URL — so it reads
`KUBEAGENT_ALERT_ROUTING_KEY`, which has no flag for the same reason:

```bash
export KUBEAGENT_ALERT_ROUTING_KEY=<ROUTING_KEY>
kubeagent watch --alert-format pagerduty
```

The routing key never reaches a log line, a metric label, an error message or a
rendered manifest. `KUBEAGENT_ALERT_WEBHOOK` is optional for this format and
defaults to PagerDuty's published endpoint; set it to point at a non-default
service region or an egress proxy, and a URL with no path gains `/v2/enqueue`.

| Flag | Default | Meaning |
|------|---------|---------|
| `--alert-format` | `json` | `json`, `slack`, `alertmanager`, or `pagerduty` |
| `--alert-repeat` | `4h`, or `60s` for `alertmanager` | Re-send interval for still-firing alerts |
````

- [ ] **Step 2: Add the PagerDuty payload subsection**

In the same file, insert after the `alertmanager` paragraph that ends "A bare
host URL gets `/api/v2/alerts` appended." and its two-line `resolve_timeout`
note (currently lines 323-332), before `### No severity`:

````markdown
`pagerduty` — a [PagerDuty Events API v2](https://developer.pagerduty.com/docs/events-api-v2-overview)
event:

```json
{
  "routing_key": "<ROUTING_KEY>",
  "event_action": "trigger",
  "dedup_key": "local/Deployment/shop/web",
  "payload": {
    "summary": "local/Deployment/shop/web: ImagePullBackOff",
    "source": "local",
    "severity": "error",
    "timestamp": "2026-08-05T10:04:11Z",
    "custom_details": {
      "cluster": "local",
      "kind": "Deployment",
      "namespace": "shop",
      "name": "web",
      "issues": ["ImagePullBackOff"],
      "reason": "new",
      "flapping": false
    }
  }
}
```

`dedup_key` is the object's identity — the same string the `slack` format
already sends. Deriving it from identity rather than state is what makes it
survive a daemon restart: the restart re-triggers whatever is still broken, and
PagerDuty folds that onto the open incident instead of opening a second one. A
key past PagerDuty's 255-character cap keeps a readable prefix and gains a short
digest, so two objects that share a long prefix still get two incidents.

`timestamp` is when the **object** broke, not when the daemon noticed — the same
`firingSince` the `json` format carries.

An **explanation** (`--explain`) is a `trigger` on the same `dedup_key` with the
prose in `custom_details.explanation`, not a new event kind: the object is still
firing, and an explanation is more detail about one state rather than a
transition to another. The daemon's `/explanations` endpoint and the dashboard
remain the authoritative place to read that prose.

A **resolve** carries only `routing_key`, `event_action` and `dedup_key`.
PagerDuty computes the incident duration itself, so anything kubeagent added
would be a second copy free to disagree with the first.

`--alert-repeat` defaults to `4h` here, as it does for `json` and `slack`, and
takes no ceiling: PagerDuty does not expire alerts, so a slow cadence produces no
false recovery to guard against.
````

- [ ] **Step 3: Amend the `### No severity` section**

Its opening sentence stops being true the moment this format sends
`severity: error`. Replace the section heading and its first paragraph (lines
334-337) with:

```markdown
### Severity

kubeagent has no severity model: `diagnose.Finding` carries no rank, and no
payload derives one. The `pagerduty` format sends the constant `error` because
the Events API requires the field — it is the same default Alertmanager's own
PagerDuty notifier picks, and it is the honest answer: kubeagent knows something
is broken, not how badly. Every other format sends no severity at all.

Route on what is actually known, and derive severity in Alertmanager if you want
it:
```

Leave the YAML block and everything after it unchanged. Then check whether any
other page links to the old `#no-severity` anchor:

```bash
grep -rn 'no-severity' website/ mkdocs.yml 2>/dev/null || echo "no inbound links"
```

If there are inbound links, update them to `#severity`.

- [ ] **Step 4: Close the roadmap's open item**

In `website/docs/roadmap.md`, in Theme E's bullet (around line 440), change:

```
  webhook alerting (JSON / Slack / Alertmanager
  shipped; PagerDuty remains an open receiver), SLO burn-rate signals
```

to:

```
  webhook alerting (JSON / Slack / Alertmanager /
  PagerDuty, all shipped), SLO burn-rate signals
```

- [ ] **Step 5: Update `deploy/README.md`**

Replace the "Alerting (opt-in)" body (lines 176-194) with:

````markdown
The daemon can POST one alert per broken object to a receiver — generic JSON, a
Slack incoming webhook, Alertmanager's `/api/v2/alerts`, or PagerDuty's Events
API v2. It stays read-only toward the cluster; the receiver is its only other
egress.

The credential is read from the environment and never passed as a flag. For
JSON, Slack and Alertmanager that is the webhook URL:

```bash
kubectl -n kubeagent create secret generic kubeagent-alerts \
  --from-literal=webhook-url=<WEBHOOK_URL>

helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set alerts.enabled=true \
  --set alerts.format=slack \
  --set alerts.existingSecret=kubeagent-alerts
```

For PagerDuty it is the integration key, in the same Secret shape — the chart
grows no new values, and the endpoint defaults to PagerDuty's published one:

```bash
kubectl -n kubeagent create secret generic kubeagent-pagerduty \
  --from-literal=routing-key=<ROUTING_KEY>

helm upgrade --install kubeagent deploy/helm/kubeagent \
  --set alerts.enabled=true \
  --set alerts.format=pagerduty \
  --set alerts.existingSecret=kubeagent-pagerduty \
  --set alerts.secretKey=routing-key
```

Only `scheme://host` is ever logged, and the routing key is never logged at all.
See the [watch mode docs](https://k8sproject.top/features/watch-mode/) for the
payload shapes and the Alertmanager cadence rule.
````

- [ ] **Step 6: Add the CHANGELOG entry**

Under `## [Unreleased]` in `CHANGELOG.md`, add:

```markdown
### Added

- **PagerDuty as a fourth alert receiver (`kubeagent watch --alert-format
  pagerduty`).** The watch daemon posts [Events API v2](https://developer.pagerduty.com/docs/events-api-v2-overview)
  events directly, so being paged by kubeagent no longer means first deploying a
  Prometheus stack to reach PagerDuty through Alertmanager. A firing object is a
  `trigger` and a recovered one a `resolve`, both on a `dedup_key` derived from
  the object's identity — so a daemon restart re-triggers onto the open incident
  instead of opening a second one. The integration key is a credential and
  inherits the webhook URL's rule: it comes from `KUBEAGENT_ALERT_ROUTING_KEY`
  with no flag, because a flag would put it in the pod spec's args and in `ps`
  output, and it never reaches a log line, a metric label, an error message or a
  rendered manifest. `KUBEAGENT_ALERT_WEBHOOK` stays the endpoint for all four
  formats and becomes optional for this one, defaulting to PagerDuty's published
  URL. The Helm chart grows **no new values**: the existing
  `alerts.existingSecret` / `alerts.secretKey` pair feeds the routing key when
  `alerts.format` is `pagerduty`. Closes Theme E's last open receiver. See
  [watch mode](https://k8sproject.top/features/watch-mode/).
```

- [ ] **Step 7: Build the site strictly**

```bash
cd website && /tmp/claude-1000/-home-ubuntu-git-kubeagent/7d266e27-cc80-4715-920c-e608368180cc/scratchpad/mkdocs-venv/bin/mkdocs build --strict -f mkdocs.yml; cd ..
```

Expected: `Documentation built in …` and exit `0`, with **no `WARNING` lines
naming the pages this task touched**. The red "Material for MkDocs 2.0" banner
is cosmetic — judge by the exit code and the absence of page warnings.

- [ ] **Step 8: Scan every changed doc for a leaked credential**

```bash
git diff --name-only main...HEAD | xargs grep -nEi 'routing_key.*[a-z0-9]{20}|10\.[0-9]+\.[0-9]+\.[0-9]+|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.' 2>/dev/null || echo "CLEAN"
```

Expected: `CLEAN`, or matches only in `chaos/run.sh`'s `127.0.0.1` loopback
lines, which are fine.

- [ ] **Step 9: Full suite, then commit**

```bash
export PATH=$PATH:/usr/local/go/bin
go build ./... && go test ./... -p 2
git add website/docs/features/watch-mode.md website/docs/roadmap.md deploy/README.md CHANGELOG.md
git commit -s -m "docs: the PagerDuty receiver

The 'no severity' section becomes 'Severity': kubeagent still has no severity
model, and the pagerduty format still does not derive one, but it sends the
constant the Events API requires — so the old claim that no payload carries one
stopped being true and is amended rather than appended to.

Theme E's last open receiver is closed."
```

- [ ] **Step 10: Verify the DCO trailer on every commit in the branch**

```bash
bash scripts/dco-check.sh main HEAD
git log --oneline main..HEAD
git log main..HEAD --format='%h %s%n%(trailers:key=Signed-off-by)' | head -40
```

Expected: the DCO check passes; nine commits; every one carries exactly one
`Signed-off-by: imantaba <itn.taba@gmail.com>` and no `Co-Authored-By` line.

---

## After the last task

The slice's gate is the **full chaos run**, not a smoke test: chart templates
and the watch daemon both changed. Run it **once**, at the end, not per task:

```bash
export PATH=$PATH:/usr/local/go/bin:$HOME/.local/bin
unset ANTHROPIC_API_KEY
./chaos/run.sh --recreate
```

It takes roughly 35-40 minutes. A zero exit means all 134 assertions passed; a
non-zero exit names the failures on the console and in the report's
`## Assertion summary`. Then `superpowers:finishing-a-development-branch`, then
the `release` skill for **v1.3.0** — remembering that
`scripts/bump-version.sh` will patch-bump the chart from `0.26.0` to `0.26.1`,
which is correct at that point and should be left alone.
