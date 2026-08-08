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
- **The CLI is a Cobra command tree in `internal/cli`**, one file per command;
  `main.go` holds only the `version` symbol the release workflow stamps with
  `-ldflags "-X main.version=<tag>"`. Flags are declared per command and never
  as persistent flags: `--kubeconfig` appears on eight commands, and three of the
  remaining ones deliberately do not accept it. pflag rejects the single-dash
  long-flag form the standard library accepted, so `internal/cli.Normalize`
  rewrites a leading `-longname` to `--longname` for names the target command
  registers — that shim is why command lines written against v0.72 and earlier
  keep working, and removing it is a breaking change. Every command sets
  `SilenceErrors` and `SilenceUsage`, so errors reach `Main`'s renderer and the
  exit codes stay kubeagent's own; validation lives in `RunE`, not in Cobra's
  `Args`/`MarkFlagsMutuallyExclusive` helpers, which would reword the messages.
- **`scan` runs its independent reads through a bounded worker pool**
  (`internal/parallel`, capped by `KUBEAGENT_SCAN_WORKERS`, 8 by default). The
  v1 "sequential, no goroutines" simplification is retired. Determinism is
  preserved by construction, not by discipline: no read closure touches shared
  state, each writes only its own destination, and a sequential block afterwards
  walks a fixed report order — so the rendered bytes are never a function of
  which read answered first. `internal/parallel` must never import
  `internal/remediate` or `internal/explain`. `internal/watch` is no longer the
  only documented long-lived-process exception: the `watch` daemon
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
  `internal/policy` (the `--policy` evaluator) is a sixth case and the most
  constrained: it is **pure** — no client, no context, no I/O beyond the bytes
  it is handed — and must never import `internal/remediate`, `internal/explain`,
  `internal/investigate`, `internal/report`, `internal/scan` or
  `internal/findings`. `internal/findings` imports `internal/scan`, which
  imports `internal/policy`, so the last three are a cycle rather than a
  preference; that is why `policy` defines its own `Level` type. A policy can
  never write to the cluster: there is no `--fix` path from a rule
  (see [website/docs/features/policy.md](website/docs/features/policy.md)).
  A policy can never require a grant beyond `core`: the kinds a rule may select
  are exactly the kinds `rbacprofile.coreRules` already grants, pinned by
  `TestSelectableKindsMatchesRBACProfileCore`, so `--policy` changes no RBAC manifest.
  `Secret` is not a selectable kind and a `ConfigMap` path beginning `data` or
  `binaryData` is a load error — a violation carries its evidence into a report
  designed to be forwarded.
  `internal/dashboard` (the `watch --dashboard` renderer) is a seventh case and
  the strictest: it **imports nothing from kubeagent at all** — only `embed`,
  `html/template`, `io`, `time`, `sort`, `strings` and `fmt` — which puts it in
  the same class as `internal/jsonschema` and makes reaching
  `internal/remediate` or `internal/explain` impossible by construction rather
  than by rule. `internal/dashboard/imports_test.go` enforces that, and a second
  test in the same file asserts that `template.HTML`, `template.JS`,
  `template.URL` and their four siblings appear nowhere, so contextual
  auto-escaping is the package's single escape boundary. It holds no client and
  no context, issues no cluster call and makes no model call — two separate
  promises. The page it renders is HTML, not one of the eight versioned JSON
  documents, so no `schemaVersion` moves
  (see [website/docs/features/dashboard.md](website/docs/features/dashboard.md)).
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
  `internal/glob` joins the same stdlib-only list: a two-metacharacter
  matcher (`*` any run including `/`, `?` one byte, everything else literal)
  with exactly two callers, `internal/policy` and `internal/cli`.
  `internal/glob/imports_test.go` enforces both halves — no kubeagent import
  and stdlib-only — the same pattern `internal/baseline/imports_test.go`
  established.
  `internal/knownissues` (the `known-issues` reference) joins the same
  stdlib-only list: the curated entry per issue kind the detector set can
  emit, as a Go slice literal — no data file, no parser, no dependency.
  `internal/knownissues/imports_test.go` enforces both halves, on
  `internal/baseline/imports_test.go`'s pattern. It holds no client and no
  context, issues no cluster call and makes no model call — two separate
  promises. The completeness check cannot live inside a package that imports
  nothing, so it lives in `internal/diagnose/knownissues_test.go`, where both
  the registry and the detectors are in scope: a `go/parser` walk over every
  `Issue:` literal, a fixture table driving all nine detectors to all
  thirteen kinds, a reverse check, and a second parser walk that reads the
  *guards* on the two sites that build a kind from a runtime value. The
  vocabulary is closed at thirteen because both apparently-dynamic sites —
  `imagepull.go` and `initcontainer.go` — are guarded to two reasons each,
  and that fourth test is what makes widening either guard fail the suite
  instead of quietly admitting a fourteenth kind. It understands a closed
  set of guard and value shapes — an `==` or `switch` against a string
  literal, and an `Issue` that is a `.Reason` field or a literal prefix
  added to one — and **refuses** every other shape by name rather than
  skipping it, so a guard rewritten out of its reach or compared against a
  named constant fails the suite too; widening the set is a deliberate edit
  to that test. Refusal is closed rather than best-effort: a value reaches
  that field only by naming it (every `Issue` occurrence must be a
  composite-literal key or an assignment's left side, inside a function
  declaration — a read is refused too, and so is a second type declaring an
  `Issue` field of its own), by not naming it (a positional literal, and a
  second name for the type, `type f = Finding`), or by bypassing syntax —
  which is why the detectors' import set is **pinned** to six packages
  rather than filtered for `reflect` and `unsafe`, since
  `json.Unmarshal(payload, &f)` writes the field importing neither. The
  closure is over `internal/diagnose` only, and deliberately: `scan`'s
  workload passes build their own `diagnose.Finding`s carrying
  `RolloutStuck`, `FailedCreate` and `JobFailed`, kinds the reference does
  not document. Each of those three was added after a shape slipped
  through with all four tests green
  (see [website/docs/features/known-issues.md](website/docs/features/known-issues.md)).
  `internal/policypack` joins the same stdlib-only list: the curated YAML rule
  packs a `--policy-pack` flag evaluates, embedded via `go:embed` — no parser,
  no dependency beyond `embed` and `sort`.
  `internal/policypack/imports_test.go` enforces both halves — no kubeagent
  import and stdlib-only — the same pattern `internal/baseline/imports_test.go`
  established. It holds no client and no context, issues no cluster call and
  makes no model call — two separate promises, and neither implies the other.
  It cannot parse its own YAML — a stdlib-only package has no YAML decoder —
  which is why `Pack` stores no rule count; `kubeagent policy packs` counts a
  pack by loading it through `internal/policy.Load` and reporting the length
  (see [website/docs/features/policy-packs.md](website/docs/features/policy-packs.md)).
  `internal/fleet` (the `kubeagent fleet` sweep) is a ninth case, and like
  `gate` it is one-shot, not long-lived: it runs once and exits. It is
  **read-only toward every cluster it sweeps** — `get`/`list` only, the exact
  calls the per-cluster `gate` evaluation it reuses already makes against
  that one context — and no `--fix` path. Separately: it makes **no LLM
  call**. It must never import `internal/remediate` or `internal/explain`.
  Its report names a row identity — the operator's own name for a cluster
  when the selection source gave one, the kubeconfig context otherwise —
  plus issue kinds and the API resource names of refused reads — never a
  node, namespace, pod or workload name, and never a blind spot's `Reason`,
  which is a redacted error string rather than a bounded vocabulary
  (see [website/docs/features/fleet.md](website/docs/features/fleet.md)).
  `internal/fleetfile` (the `--fleet-file` decoder) is a tenth case and takes
  `internal/fleet`'s wall plus one `internal/fleet` cannot carry: it must never
  import `internal/remediate` or `internal/explain`, and it must never import
  `k8s.io/client-go` or `internal/cluster` either, which makes "holds no client"
  structural rather than stated in a package that holds kubeconfig paths.
  `internal/fleetfile/imports_test.go` enforces both halves. It is not in the
  stdlib-only class — it imports `sigs.k8s.io/yaml`, already a direct dependency,
  and `internal/safetext`. It is pure: no client, no context, no I/O beyond the
  bytes it is handed. The file it decodes names clusters and cannot carry a
  credential: an `Entry` has three string fields decoded with
  `yaml.UnmarshalStrict`, so `server:`, `token:` and `certificate-authority-data:`
  are load errors rather than ignored keys.
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
- **`internal/jsonschema` imports nothing from kubeagent** — it is the schema
  generator, importable by every surface package including the ones that may not
  import `internal/remediate` or `internal/explain`. `internal/schemadoc` is the
  opposite case and deliberately so: it imports the six surface packages to
  name the eight document roots, so it transitively reaches `remediate` and
  `explain`. That is allowed — the invariants constrain what those packages
  import, not who imports them — and only `main.go` and `schemadoc`'s own tests
  import it. It holds no client and no context and makes no call.
- **The eight JSON documents are a versioned contract.** Changing a field name, a
  type, or an enum value in `report.ScanReport`, `gate.Verdict`,
  `rbacprofile.RulesDocument`, `rbacprofile.CheckDocument`,
  `watch.IssuesReport`, `watch.ExplanationsReport`, `baseline.Document` or
  `fleet.Report` means
  bumping the surface's version in `internal/jsonschema` and regenerating with
  `go test ./internal/schemadoc -run TestSchemaDrift -update`. The drift test
  says whether the change was additive (MINOR) or breaking (MAJOR). `scan` is
  at schema version **1.2** (added `policy`, then `baseline`, both
  `omitempty`), `gate` is at **1.1** (added `policyNotEvaluated`, `omitempty`),
  and `fleet` is at **1.2** (added `shared` at 1.1, then `name` at 1.2, both
  `omitempty`) — all three additive; `baseline` enters at **1.0**. A run
  without `--policy` or `--baseline` encodes none of those keys, a sweep that
  correlates nothing encodes no `shared` key, and a sweep selected from a
  kubeconfig writes no `name` key either, because a row identity that equals
  its context is not written — every existing consumer is unaffected.

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
- **Theme G is complete:** the MCP server
  (`kubeagent mcp`), documented in
  [website/docs/features/mcp.md](website/docs/features/mcp.md); the `kubectl`
  krew plugin (`krew/kubeagent.yaml.tmpl` +
  `scripts/render-krew-manifest.sh`, rendered at release time and never
  committed); CI/CD gate mode (`kubeagent gate`), documented in
  [website/docs/features/ci-gate.md](website/docs/features/ci-gate.md); the
  shareable HTML report (`scan --output html`), documented in
  [website/docs/features/html-report.md](website/docs/features/html-report.md);
  and the interactive TUI (`kubeagent tui`), documented in
  [website/docs/features/tui.md](website/docs/features/tui.md); and the
  in-cluster dashboard (`watch --dashboard`, v1.2.0), documented in
  [website/docs/features/dashboard.md](website/docs/features/dashboard.md),
  which closed the theme.
  A third distribution surface now sits alongside the MCP server and the krew
  plugin: kubeagent installs into Claude Code as a plugin
  (`.claude-plugin/plugin.json` + `marketplace.json`, with user-facing skills
  under `skills/` and commands under `commands/`), documented in
  [website/docs/features/claude-plugin.md](website/docs/features/claude-plugin.md).
  It ships no Go production code and is **read-only**: no tool, skill, or
  command reaches `--fix`. Note the two skills directories — `.claude/skills/`
  is dev-facing (it holds `release` and `update-demo-gif`); root-level
  `skills/` is what the plugin ships to users. Claude Code
  auto-discovers only the former. `plugin_manifest_test.go` and
  `internal/cli/plugin_flags_test.go` fail the build when the manifests or the
  shipped skill text drift from the flags and tool names kubeagent registers.
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
  ([website/docs/features/rbac.md](website/docs/features/rbac.md)). Slice 3 —
  fuzzed detectors — has shipped (v0.70.0): seven `go test -fuzz` targets assert
  that no Kubernetes object or endpoint response can panic a scan, that the
  detector set stays pure and deterministic, and that no raw byte from the
  cluster reaches a terminal. Objects come from the test-only
  `internal/fuzzgen`, which draws DNS-1123 alphabets for the fields the API
  server validates and hostile bytes for the fields it does not; seed corpora
  replay on a plain `go test`, and a real campaign runs nightly in
  `.github/workflows/fuzz.yml`. The campaign closed nine unsanitized ingress
  points, a non-finite-float integer overflow in the DNS health parser, an
  unbounded `/readyz` check list, and three uncapped proxied reads. Slice 4 —
  versioned JSON schema — has shipped (v0.71.0): `scan`, `gate`, `rbac print`,
  `rbac check`, and the watch daemon's `/issues` and `/explanations` all
  declare a `schemaVersion`; `internal/jsonschema` generates each surface's
  schema by reflection over its Go types and `internal/schemadoc` publishes it
  under `website/docs/schemas/`, `kubeagent schema [name]` prints any of them
  from the running binary with no cluster and no kubeconfig, and
  `TestSchemaDrift` fails a shape change that moves without a version bump,
  naming it additive or breaking
  ([website/docs/features/json-schema.md](website/docs/features/json-schema.md)).
  Slice 5 — bounded scan concurrency — has shipped (v0.72.0): `scan`'s independent reads
  run through a fixed worker pool (`internal/parallel`, 8 workers by default,
  `KUBEAGENT_SCAN_WORKERS`), and client-go's 5 QPS client-side rate limiter is
  off by default (`KUBEAGENT_QPS`/`KUBEAGENT_BURST` restore it). Output is
  byte-identical: the reads write only their own destinations and a single
  sequential pass afterwards renders in a fixed order
  ([website/docs/features/tuning.md](website/docs/features/tuning.md)).
  Slice 6 — the Cobra CLI — has shipped (v0.73.0): `internal/cli` replaces
  main.go's hand-rolled dispatch over the standard-library `flag` package,
  one file per command; `Normalize` rewrites the single-dash long-flag
  spelling pflag would reject, so command lines written against v0.72 and
  earlier keep working; and `kubeagent completion bash|zsh|fish|powershell`
  generates a completion script from the command tree
  ([website/docs/features/completion.md](website/docs/features/completion.md)).
  Slice 7 — policy as code — has shipped (v0.74.0): `scan --policy` and
  `gate --policy` evaluate operator-written YAML rules, `kubeagent policy
  validate` checks a file with no cluster, and a rule that could not be
  evaluated fails a gate instead of passing quietly.
  Slice 8 — the cross-version chaos matrix — and slice 9 — the written
  production contract — have shipped (v1.0.0), and **Theme H is complete**:
  `chaos/run.sh` is a gate rather than a report (134 machine-checked assertions;
  a failure lets the remaining scenarios run and surfaces in the exit code at the
  end), `--k8s-version <minor>` pins it to a
  digest-pinned kind node image from `chaos/versions.env`,
  `.github/workflows/chaos-matrix.yml` runs the full suite nightly once per
  supported minor, and now also runs one k3s cell at the newest supported
  minor — `--distro kind|k3s` selects which distribution the harness creates —
  and
  [website/docs/compatibility.md](website/docs/compatibility.md) writes down
  which surfaces are stable within 1.x, which are deliberately not, and what
  deprecating one costs. From 1.0 onward a MAJOR release is the only one that
  may break a stable surface.
- **Post-1.0 — the chaos harness's portability seam has shipped (v1.4.0):**
  `./chaos/run.sh --context <ctx>` runs the suite against a cluster the harness
  did not create. Each scenario declares what it needs from a closed six-name
  capability vocabulary, and twelve `requires` guards decide what runs; nine
  scenarios skip on a foreign cluster, each naming its reason in the assertion
  summary, which counts skips separately so a partial run can never be mistaken
  for a full one. `--recreate`, `--teardown` and `--k8s-version` are refused
  rather than ignored, a preflight checks the context connects and that a
  namespace round trip works before anything is touched, and leftover
  `chaos-*` namespaces are swept at the end. The harness is deliberately **not**
  read-only toward the cluster — it injects outages — which is exactly why
  pointing it at someone's real cluster is guard-railed. Portable mode also
  treats that cluster's identity as a credential: node names and the kubeconfig
  context name are redacted from the results file at a single seam every report
  write passes through (see `chaos/README.md` for the one documented residual),
  and a section that cannot be redacted is withheld rather than shown. This
  makes a cross-distribution answer obtainable by hand, and the follow-up
  slice — gating a second distribution in CI — has since shipped (v1.5.0):
  `--distro kind|k3s` selects which distribution the harness creates, and the
  nightly matrix now runs a k3s cell at the newest supported minor alongside
  the per-minor kind cells.
- **Post-1.0 — anomaly/baseline learning, slice 1 has shipped (v1.6.0):** a
  learned restart rate, the first answer to "what's normal for *this* cluster".
  `kubeagent baseline capture` prints a per-workload rate to **stdout** — it
  writes no file, so the only file write in the repository is still
  `scan --audit-log` — and `scan --baseline` / `gate --baseline` compare a later
  run against it. A workload deviates only when it clears **both** thresholds,
  `--baseline-factor` (3.0) and `--baseline-floor` (0.5/hour), so a rise from
  0.001 to 0.01 is not a 10× alarm. A learned rate is a heuristic, not a
  detector, so a deviation is a finding at `findings.Info` and never fails a
  gate at the default `--fail-on critical`; `internal/inventory.PodOwners` is
  the one implementation of the pod-to-workload rule, extracted so a baseline
  can see the workloads `Prioritize` drops and the Job pods `Assemble`
  truncates (see
  [website/docs/features/baseline.md](website/docs/features/baseline.md)).
- **Post-1.0 — fleet-scale, slice 1 has shipped (v1.7.0):** `kubeagent fleet`
  sweeps every selected kubeconfig context in bounded parallel and prints one
  row per cluster, worst first. The per-cluster pipeline is exactly `gate`'s —
  `scan.Evaluate` then the pure `gate.Decide` — so a sweep and a single-cluster
  `gate` can never disagree about the same cluster, and fleet adds no diagnosis
  of its own. Selection is `--context` (repeatable) or `--all-contexts` plus an
  optional `--match` glob; `--workers` (8) bounds concurrency and
  `--cluster-timeout` (60s) bounds each cluster, and a non-positive budget is
  refused rather than read as "no deadline", because the worker pool returns
  only once every worker has. `inconclusive` outranks `fail` for the same
  reason it does in `gate`: one unreachable cluster must not hide behind
  another cluster's failure. A client that cannot be built is fatal at exit 4
  — `cluster.NewClient` does no network I/O, so a failure there is a
  configuration defect, never a reachability event — while a cluster that is
  merely gone lands in `unreachable` with a reason from a fixed two-entry
  vocabulary, never an `err.Error()`
  (see [website/docs/features/fleet.md](website/docs/features/fleet.md)).
- **Post-1.0 — the known-issues knowledge base, slice 1 has shipped (v1.8.0):**
  `kubeagent known-issues [kind]` prints kubeagent's own reference for the
  thirteen kinds `diagnose.DefaultDetectors` can emit, from a curated Go
  slice literal in `internal/knownissues` — no cluster, no kubeconfig, no
  network, no flags, and no model call. The vocabulary is closed, and four
  tests in `internal/diagnose` keep it closed: they fail the suite if a
  detector emits a kind the reference does not document, if the reference
  documents a kind no detector emits, or if either of the two runtime-valued
  `Issue:` sites has its guard widened to admit a new reason — or rewritten
  into a shape the fourth test cannot read, which it refuses rather than
  skips. That refusal is closed rather than best-effort: a kind reaches a
  finding only by naming the field, by not naming it, or by bypassing syntax,
  and the fourth test checks all three
  (see [website/docs/features/known-issues.md](website/docs/features/known-issues.md)).
- **Post-1.0 — curated policy packs, slice 1 has shipped (v1.9.0):** the second half
  of the known-issues item's "curated community detector library" ambition
  now has a first form — not new Go detector code, but `kubeagent policy
  packs`: a kubeagent-curated `reliability` pack of fourteen rules, since
  slice 2 (v1.12.0) a `security` pack of twenty-three rules over workload pod
  templates, and since slice 3 (v1.13.0) a `cost` pack of sixteen rules over seven
  kinds — Deployment, StatefulSet, DaemonSet, CronJob, Job,
  HorizontalPodAutoscaler and PersistentVolumeClaim — every one `info`. All
  three are compiled into `internal/policypack` and evaluated by the existing
  `--policy` engine via `scan --policy-pack`/`gate --policy-pack`.
  The `security` pack pairs an `info` "field unset" rule with a `warning`
  "field set wrong" rule for the four properties that are unsafe either way,
  because every operator except `exists` and `notExists` skips an absent
  slot; where absence is the safe default a single value rule ships. The
  `cost` pack ships no paired rules at all: the same skip means a threshold's
  absence is already the safe value, and the one `exists` question it might
  have duplicated — an unset CPU or memory request on a Deployment — is
  already `reliability`'s, so `cost` asks it only for StatefulSet, DaemonSet
  and CronJob. Separately, kubeagent has no prices — no billing data, no
  instance types, no node cost, no cloud API — so `cost` names shapes that
  usually cost money and claims nothing more. It is
  opt-in — omitting the
  flag renders the same bytes as before — moves no `schemaVersion` (`scan`
  stays 1.2, `gate` stays 1.1), and ships no `critical` rule, so adding it to
  a pipeline that passed yesterday cannot fail it today
  (see [website/docs/features/policy-packs.md](website/docs/features/policy-packs.md)).
  Slice 4 has since shipped (v1.13.1), and **curated policy packs are
  complete**: there
  is now a documented route for a pack written outside kubeagent, with the
  admission criteria machine-checked at the layer nothing else can see.
  `policy.Load` validates every rule and the generic pack tests validate every
  *registered* pack, but neither can see the registry — `Load` is handed bytes
  and never learns where they came from, and every generic test iterates
  `All()`, so anything absent from `All()` is invisible to all of them.
  `internal/policypack/registry_test.go` closes it: every embedded
  `packs/*.yaml` must have a registry entry, no two entries may share a name or
  a file, a name must match `^[a-z0-9]+(-[a-z0-9]+)*$` and fit the `%-14s`
  listing column in `internal/cli/policy.go`, and a summary must be one line
  with no leading or trailing whitespace, no trailing period and no leading
  capital. Acceptance stays curatorial —
  the criteria are necessary, not sufficient, and a pack that ships is
  kubeagent's curation whoever wrote it; attribution lives in the pack's header
  comment, which `--print` emits, and there is no author field, because a
  two-tier listing would tell an operator to trust some shipped rules less than
  others. The slice adds **no production Go code**: the registry gains no entry
  and no field, and no pack ships. Loading a pack into an installed binary
  without a kubeagent release remains deliberately absent, and no outside pack
  has yet come through the route
  (see [website/docs/features/policy-packs.md](website/docs/features/policy-packs.md)).
- **Post-1.0 — fleet-scale, slice 2 has shipped (v1.10.0):** cross-cluster
  correlation. Under the per-cluster table, `kubeagent fleet` now names the
  issue kinds and the refused reads present in two or more of the **judged**
  clusters, most widespread first — the answer to "is this one problem or
  five" that a one-row-per-cluster view cannot give. It costs no new cluster
  read: `correlate` is a pure fold over values the sweep had already computed
  in memory. Separately: it makes **no LLM call**. Both axes come from
  bounded vocabularies — `findings.Finding.Issue` and
  `gate.Blindspot.Resource` — and a `Blindspot`'s `Reason` is never read,
  because it is a redacted error string rather than a bounded one, which is
  what keeps the report's promise to name no node, namespace, pod or workload
  intact. Evidence is a **set**: a kind hitting four hundred pods in one
  cluster is still one cluster, so a single noisy cluster cannot manufacture a
  fleet-wide signal. The denominator is the count of clusters judged, never
  selected — an unreachable cluster produced no verdict and could not have
  contributed a signal — and the header word "judged" is constant, never
  conditional. An empty section is omitted entirely, heading included. It
  changes no verdict: `decide` is untouched, so a sweep still cannot disagree
  with a single-cluster `gate` about the same cluster. `fleet` moves to schema
  version **1.1** (added `shared`, `omitempty`), and a sweep that correlates
  nothing encodes no key
  (see [website/docs/features/fleet.md](website/docs/features/fleet.md)).
  Slice 3 has since shipped (v1.11.0), and **fleet-scale is complete**:
  `kubeagent fleet --fleet-file <path>` selects the clusters to sweep from a
  YAML file instead of a kubeconfig's contexts, so a fleet can span several
  kubeconfigs and each row can carry a name the operator chose.
  `internal/fleetfile` decodes it under `internal/fleet`'s wall plus one
  `internal/fleet` itself cannot carry: neither `k8s.io/client-go` nor
  `internal/cluster`, so holding no client is structural rather than stated. Selection comes from
  the file; credentials still come from the kubeconfigs it points at, and
  the format cannot express one — an entry has three string fields decoded
  strictly, so `server:`, `token:` and `certificate-authority-data:` are
  load errors. `fleet` moves to schema version **1.2** (added the optional
  `name` on a cluster summary and on an unreachable cluster, both
  `omitempty`).
  The remaining post-1.0 work is other baseline dimensions.
