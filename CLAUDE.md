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
- v1 CLI (`scan`) is **sequential** — no goroutines. The `watch` daemon
  (`internal/watch`) is the documented exception: it runs informers, a heartbeat
  ticker, and an HTTP server concurrently. It remains **strictly read-only**
  (get/list/watch only; no writes, no LLM).

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

- **v1** — deterministic scan + diagnose: CrashLoopBackOff,
  ImagePullBackOff/ErrImagePull, OOMKilled, Pending/Unschedulable.
- **v2** — optional `--explain` flag: a single Claude API call summarizing
  findings in plain English (the deterministic core stays usable offline).
