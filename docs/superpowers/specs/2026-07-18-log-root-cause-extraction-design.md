# Log Root-Cause Extraction — Design

**Date:** 2026-07-18
**Status:** Approved

## Goal

Deepen kubeagent's "why" from the Kubernetes reason to the **application** reason.
Today a crashing pod reports `CrashLoopBackOff` and the container/restart evidence,
but the real cause — a panic, a `connection refused` to a dependency, a config parse
error — lives in the container's last log lines, which the user then has to fetch with
`kubectl logs --previous`. Opt-in `scan --logs` fetches the crashed container's
previous-instance logs, surfaces the failure line, and classifies it into a
plain-language cause, so the answer is right there in the scan.

## Scope

- **Scan-only.** This is a human troubleshooting aid, not a metric — it is **not** wired
  into the `watch` daemon (no gauge, no daemon/Helm change).
- **Read-only** (`pods/log` GET), **opt-in** (`--logs`, off by default), **deterministic**
  (the classifier is pure), and **offline** (no LLM needed; `--explain` is optional).
- Targets only the container-terminated crash findings — **CrashLoopBackOff**,
  **RestartLoop**, **OOMKilled** — which have `--previous` logs. ImagePullBackOff,
  Pending/Unschedulable, and VolumeAttachError are skipped (no container ran).

## Data source

The kubelet serves a container's logs via the API server:
`client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container, Previous: true,
TailLines})`. `Previous: true` returns the *last terminated* instance's logs — exactly the
crashed run we want. This needs the `pods/log` subresource `get` grant, which the base
read-only ClusterRole (`pods` get/list/watch) does **not** include.

## Components

### `internal/logscan` (new, pure)

```go
// Clue is the classified root cause from a container's crash logs.
type Clue struct {
	Signature string `json:"signature"` // matched signature name, "" if fallback
	Excerpt   string `json:"excerpt"`   // the single relevant log line, trimmed/truncated
	Cause     string `json:"cause"`     // plain-language cause
}

// Classify scans a container log's tail against an ordered signature library and
// returns the first match; if none match it falls back to the last non-empty line.
func Classify(log string) Clue
```

`Classify` splits `log` into lines, and for each signature (in priority order) returns the
first line that matches, with `Excerpt` = that line (trimmed, truncated to 200 runes) and
`Cause` = the signature's plain-language cause (some interpolate a captured group, e.g. the
`host:port`). If no signature matches, `Signature == ""`, `Excerpt` = the last non-empty
line, `Cause` = `"last output before exit (no known signature)"`. If `log` is empty/whitespace,
returns the zero `Clue` (no clue). Pure and read-only.

**Signature library** (ordered; each is a regexp → cause):

| Signature | Matches (case-insensitive) | Cause |
|-----------|----------------------------|-------|
| `panic` | `^panic:` or `goroutine \d+ \[running\]` | `application panic (code bug)` |
| `entrypoint` | `exec:` line: `exec: .*executable file not found` / `exec: .*no such file or directory` / `exec: .*permission denied` (container-start `exec:` wording only) | `bad command or entrypoint` |
| `conn-refused` | `dial tcp (\S+): connect: connection refused` | `cannot reach a dependency (<addr>) — connection refused` |
| `dns` | `no such host` / `server misbehaving` | `DNS resolution failed (<host> if captured)` |
| `oom-inproc` | `out of memory` / `cannot allocate memory` / `std::bad_alloc` | `ran out of memory in-process` |
| `config` | `yaml:` / `invalid character .* looking for` / `failed to parse` / `invalid config` | `configuration parse/validation error` |
| `addr-in-use` | `bind: address already in use` | `port already in use` |
| `auth` | `password authentication failed` / `access denied` / `401 Unauthorized` / `403 Forbidden` | `authentication/authorization failure to a dependency` |
| `perm-denied` | `permission denied` / `EACCES` (not already matched by `entrypoint`) | `permission denied — check securityContext / file permissions` |

Order matters and is the order above: the specific, `exec:`-anchored `entrypoint` signature
is checked **before** the generic `perm-denied`, so a container-start `exec: … permission
denied` classifies as a bad entrypoint while a bare runtime `permission denied` falls through
to `perm-denied`. `panic` is first so a panic body containing "no such file" isn't mis-matched.
Each regexp is anchored/specific enough to avoid cross-matches; the table tests below pin the
intended mapping one fake log at a time.

### `internal/collect`

```go
// PreviousLogs fetches the last-terminated instance's logs for one container, capped.
// Never returns an error (non-fatal, like NodeStats): returns ("", false) on any failure
// (no previous instance, forbidden, transport error).
func PreviousLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string) (string, bool)
```

Does `client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container,
Previous: true, TailLines: ptr(int64(25))}).DoRaw(ctx)`; on error returns `("", false)`.
The 25-line cap plus `logscan`'s per-line truncation bound the data read and displayed.
Thin, untested wrapper (mirrors `NodeStats`/`KubeletHealthz`).

### `internal/diagnose`

`Finding` gains three optional fields (additive; existing JSON unchanged):

```go
type Finding struct {
	// ... existing: Pod, Issue, Reason, Evidence, Resources ...
	Container  string `json:"container,omitempty"`  // set by crash detectors; which container to read logs for
	LogCause   string `json:"logCause,omitempty"`   // set by scan --logs enrichment
	LogExcerpt string `json:"logExcerpt,omitempty"` // set by scan --logs enrichment (text output only)
}
```

The **CrashLoop**, **RestartLoop**, and **OOMKilled** detectors set `Container: cs.Name`
(the crashing container they already identify). No other detector sets it. `LogCause` /
`LogExcerpt` are left empty by the pure detectors and filled by scan.

### `internal/scan`

`Options` gains `Logs bool`. After `diagnose.Run(...)` produces `findings`, when
`opts.Logs`, for each finding whose `Container != ""` (the crash types), call
`collect.PreviousLogs` then `logscan.Classify` and set the finding's `LogCause` =
`clue.Cause` and `LogExcerpt` = `clue.Excerpt` (skip when `PreviousLogs` returns
`ok == false` or the clue is zero). `Result` is unchanged in shape (findings already flow
through it). No `clusterhealth` change.

### `internal/report`

Under a crash finding that has a `LogExcerpt`, render (indented like the existing evidence
`↳` lines):

```
    logs (previous container):
      <LogExcerpt>
      → <LogCause>
```

Rendered only in text output; JSON carries `logCause`/`logExcerpt` via the `Finding` tags.
No verdict/attention impact — it annotates an existing finding.

### `internal/explain`

The explain input for a workload's findings includes `LogCause` (the derived label) when
present — **never** `LogExcerpt` (raw log text). The `--explain` privacy note is unchanged;
it still "never sends raw pod specs, pod IPs, environment variables, or secrets," because a
derived cause label like `"application panic (code bug)"` is not log text.

### `main.go` / RBAC

- `main.go`: `--logs` bool flag ("read each crashing container's previous logs and classify
  the failure — needs the `pods/log` grant"), into `scan.Options`; added to the scan usage
  string.
- RBAC: `--logs` reads `pods/log` with the user's own kubeconfig (most read contexts already
  allow it). For a restricted context, add `deploy/rbac-logs.yaml` granting `pods/log` `get`
  (mirrors `deploy/rbac-diskusage.yaml`), documented as the `--logs` add-on. The in-cluster
  daemon is unaffected (scan-only feature).

## Scope boundaries

- Read-only (a `GET` on `pods/log`); opt-in; scan-only; not a metric; not wired to `--fix`.
- `--explain` receives only the derived `LogCause`, never raw log text — privacy note
  unchanged.
- The displayed excerpt is a single, truncated line (the matched signature line or the last
  non-empty line), not a raw multi-line dump — bounding both noise and incidental secret
  exposure. kubeagent does no aggressive secret-redaction beyond showing one relevant line.
- v1 fetches `Previous` logs only (the crashed instance); it does not stream, tail live, or
  fetch current-instance logs.

## Testing

- `logscan.Classify` (pure) table tests: one fake log per signature asserting the expected
  `Signature`/`Cause` (and the captured `<addr>`/`<host>` interpolation for conn-refused/dns),
  plus a no-match log → fallback (`Signature == ""`, last-line excerpt), and empty → zero Clue.
- `diagnose`: the CrashLoop/RestartLoop/OOMKilled detectors set `Container` — assert it in
  their existing tests (extend, don't duplicate).
- `report`: a crash finding with `LogExcerpt`/`LogCause` renders the `logs (previous
  container):` block; a finding without them renders no such block.
- **Golden test:** add a `LogCause`/`LogExcerpt` to one crash finding in `goldenInput` so the
  new rendering is snapshotted in `testdata/golden-scan.txt`.
- `collect.PreviousLogs` is a thin untested wrapper (mirrors `NodeStats`).
- `explain`: the explain input includes `LogCause` and excludes `LogExcerpt` (assert both).

## Docs

- `CHANGELOG.md` (`## [Unreleased]` → `### Added`).
- `website/docs/features/diagnostics.md`: a "Crash log root-cause (opt-in)" subsection.
- `website/docs/quickstart.md`: the `--logs` flag in the flags list.
- `README.md`: one-line mention.
- `deploy/rbac-logs.yaml` (new) + a note in `deploy/README.md` / install docs on the
  `pods/log` add-on.

## Exact names to use verbatim

- Flag `--logs`; `scan.Options.Logs`; package `internal/logscan`; `logscan.Clue`
  {`Signature`,`Excerpt`,`Cause`}; `logscan.Classify(log string) Clue`;
  `collect.PreviousLogs`; `Finding.Container` / `Finding.LogCause` / `Finding.LogExcerpt`
  (JSON `container`/`logCause`/`logExcerpt`, all `omitempty`); report header
  `logs (previous container):`; fallback cause `last output before exit (no known signature)`;
  RBAC add-on `deploy/rbac-logs.yaml`.
