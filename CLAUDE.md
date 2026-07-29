# kubeagent — Project Notes for Claude

A read-only Kubernetes troubleshooting CLI written in Go. This is **also a
Go-learning project** for a developer who is new to Go (comes from Python, but
prefers Go explained from scratch — see "Learning companion" below).

## Build, test, run

- Go lives at `/usr/local/go/bin` — put it on PATH: `export PATH=$PATH:/usr/local/go/bin`
- Module: `github.com/imantaba/kubeagent` (Go 1.26)
- Build: `go build ./...`  (binary: `go build -o kubeagent .`)
- Test:  `go test ./...`
- Run:   `./kubeagent scan [--kubeconfig path] [--output text|json]`
- Or as a `kubectl` plugin (krew): `kubectl kubeagent scan …` — same binary,
  same flags. `invocationName` in `main.go` reads `argv[0]` so usage and error
  text name whichever spelling the user typed.

## Architecture

One-directional pipeline, one focused package per stage:

```
cluster (connect) → collect (list pods) → diagnose (Detector interface) → report (text/JSON)
```

Full design in [docs/design.md](docs/design.md); task-by-task build plan in
[docs/plan-v1.md](docs/plan-v1.md).

## Invariants (do not break)

- **READ-ONLY by default.** Only `List`/`Get`-style calls, EXCEPT the opt-in
  `--fix` remediation flag, whose writes are guard-railed (fixed allowlist,
  protected namespaces, per-action confirmation, re-verify) and never
  LLM-decided. Without `--fix`, kubeagent never creates, updates, patches, or
  deletes anything.
- v1 uses the **standard-library `flag`** package only — no Cobra yet.
- v1 CLI (`scan`) is **sequential** — no goroutines. `internal/watch` is no
  longer the only documented long-lived-process exception: the `watch` daemon
  runs informers, a heartbeat ticker, and an HTTP server concurrently, and
  `kubeagent mcp` (`internal/mcp`) is a second long-lived server, serving MCP
  tool calls over stdio for as long as the client stays connected. Both remain
  **strictly read-only toward the cluster** (get/list/watch only; no writes)
  and make **no LLM calls**. `internal/mcp` must never import
  `internal/remediate` or `internal/explain` — there is no code path from the
  MCP server into a write or into a model call. One deliberate carve-out:
  `kubeagent mcp`'s eager startup connection check exits with an error naming
  the kubeconfig path and context on stderr — the operator's channel, read
  before the process ever starts serving — while the protocol stream and
  every tool result stay path-free (see
  [website/docs/features/mcp.md](website/docs/features/mcp.md)). `kubeagent
  gate` (`internal/gate`, `internal/findings`, `internal/sarif`,
  `internal/rolloutwait`) is a third case, though it is not long-lived: it
  runs once and exits. It too is **read-only toward the cluster** (`get`/`list`
  only), makes **no LLM calls**, and must never import `internal/remediate` or
  `internal/explain` (see
  [website/docs/features/ci-gate.md](website/docs/features/ci-gate.md)).
  `kubeagent tui` (`internal/tui`) is a fourth case, a long-lived interactive
  process alongside the watch daemon and the MCP server, not a one-shot run
  like `gate`. It is **strictly read-only toward the cluster** (`get`/`list`
  only, not even `watch`), makes **no LLM calls**, and must never import
  `internal/remediate`, `internal/explain`, `internal/investigate`, or
  `internal/report` (see [website/docs/features/tui.md](website/docs/features/tui.md)).
  `kubeagent rbac` (`internal/rbacprofile`) is a fifth case: a one-shot, read-only command
  that makes **no LLM calls** and must never import `internal/remediate` or
  `internal/explain`. Its `check` verb creates `SelfSubjectAccessReview` objects — a virtual
  resource the API server evaluates and never persists, the same API `kubectl auth can-i`
  uses. It is the sole non-`--fix` path in kubeagent that issues a POST, and it changes no
  cluster state.
  `internal/htmlreport` (the `scan --output html` renderer) is a different case
  and is deliberately allowed to reuse `report.Input`, which transitively pulls
  in `internal/remediate`. The rule above is about capability, not the
  dependency graph: `Render` takes an `io.Writer` and a value, holds no client
  and no context, and never reads `RemediationPlan`. It must still never import
  `internal/remediate`, `internal/explain` or `internal/investigate` directly.

## Commit conventions

- **Do NOT add a `Co-Authored-By: Claude` trailer** (or any Claude / Claude Code
  attribution) to commits. This overrides the default Claude Code behavior of
  appending a co-author trailer. Every commit is authored solely by the human;
  no AI assistant should appear as a contributor to this repository.

## Testing style

- Detectors are pure functions: unit-test with **fake pods** (`helpers_test.go`),
  no cluster needed.
- I/O packages (`cluster`, `collect`) use client-go's **fake clientset**.
- **TDD:** write the failing test first, watch it fail, then implement.
- **Golden output test:** `internal/report/golden_test.go` snapshots the full `scan`
  text output against `testdata/golden-scan.txt`. When a report-format change is
  intentional, regenerate it with
  `go test ./internal/report -run TestGoldenScanOutput -update`, then refresh the README
  demo GIF (the `update-demo-gif` skill) and the quickstart example output
  (`website/docs/quickstart.md`).

## Learning companion

- [docs/go-concepts.md](docs/go-concepts.md) is a running Go cheat-sheet. When a
  task introduces a **new** Go concept (JSON, `context.Context`, goroutines,
  etc.), append an entry in the established style: **a plain everyday example
  first, then the kubeagent example.**
- **No Python comparisons** — the author is learning Go on its own terms.
- One simple example per concept is enough; don't pile on.

## Roadmap

- **v1 (shipped)** — deterministic scan + diagnose: CrashLoopBackOff,
  ImagePullBackOff/ErrImagePull, OOMKilled, Pending/Unschedulable.
- **v2 (shipped)** — optional `--explain` flag: a single Claude API call summarizing
  findings in plain English (the deterministic core stays usable offline).
- **Now shipping (0.2x)** — a broad detector suite (probes, init containers,
  Job/CronJob, FailedCreate, Pending-PVC, …), the read-only `watch` daemon, and
  guard-railed `--fix`. See the CHANGELOG for per-release detail.
- **The living forward roadmap** — principles, themed tracks, and milestone
  releases (root-cause correlation → principled `--explain`/`--fix` → continuous
  ops → operator/ecosystem coverage → MCP server & `kubectl` plugin → v1.0
  production contract) lives in
  [website/docs/roadmap.md](website/docs/roadmap.md). Update it when a milestone
  ships or the plan shifts.
- **Theme G slices 1, 2, 3, 4a and 4b have shipped:** the MCP server
  (`kubeagent mcp`), documented in
  [website/docs/features/mcp.md](website/docs/features/mcp.md); the `kubectl`
  krew plugin (`krew/kubeagent.yaml.tmpl` +
  `scripts/render-krew-manifest.sh`, rendered at release time and never
  committed); CI/CD gate mode (`kubeagent gate`), documented in
  [website/docs/features/ci-gate.md](website/docs/features/ci-gate.md); the
  shareable HTML report (`scan --output html`), documented in
  [website/docs/features/html-report.md](website/docs/features/html-report.md);
  and the interactive TUI (`kubeagent tui`), documented in
  [website/docs/features/tui.md](website/docs/features/tui.md). The rest of
  Theme G — an optional in-cluster dashboard — remains ahead.
- **Theme H slice 1 has shipped (v0.68.0):** supply-chain integrity for
  releases — byte-reproducible archives (`scripts/build-release-archives.sh`,
  regression-tested by `release_archives_test.go`), keyless cosign signatures
  over `SHA256SUMS` and over the image digest, SPDX SBOMs and SLSA build
  provenance attested for both, and the pre-release guard in
  `scripts/release-vars.sh` that keeps an `-rc` tag off `:latest`. A verifier
  follows [website/docs/verify.md](website/docs/verify.md). Slice 2 —
  per-feature least-privilege RBAC — has shipped (v0.69.0): one `Feature` table in
  `internal/rbacprofile` generates every RBAC manifest and the chart
  ClusterRole, `kubeagent rbac print`/`check` report what each feature costs
  and whether an identity may run it, and a refused read is now named as a
  blind spot instead of rendering an empty section
  ([website/docs/features/rbac.md](website/docs/features/rbac.md)). The rest of
  Theme H — fuzzed detectors and the v1.0 production contract — remains ahead.
