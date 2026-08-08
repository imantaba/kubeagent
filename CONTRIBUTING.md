# Contributing to kubeagent

Thanks for your interest. Bug reports, detector ideas, documentation fixes, and
pull requests are all welcome. Everyone taking part is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Before you start

- **Open an issue first for anything non-trivial.** A new detector, a new
  subcommand, or a change to output format is worth agreeing on before it is
  written. Typo fixes, doc corrections, and obvious bug fixes can go straight
  to a pull request.
- **Read [docs/design.md](docs/design.md).** kubeagent is a one-directional
  pipeline — `cluster` (connect) → `collect` (list) → `diagnose` (detect) →
  `report` (render) — and changes that cut across it usually want discussion.
- **Contributing a policy pack?** There is a documented route with its own
  admission criteria, enforced by `go test ./internal/policypack` — see
  [Contributing a pack](https://k8sproject.top/features/policy-packs/#contributing-a-pack).
- **Security problems do not go in issues.** See [SECURITY.md](SECURITY.md).

## Project invariants

These are not style preferences. A change that breaks one of them will not be
merged without an explicit decision under [GOVERNANCE.md](GOVERNANCE.md):

1. **Read-only by default.** `scan`, `watch`, `mcp`, `gate`, `tui`, `rbac` and
   `fleet` issue only `get`/`list`/`watch` against the cluster. Two opt-in
   flags write, and only those two: `scan --fix`, whose writes come from a
   fixed allowlist, refuse protected namespaces, require a per-action
   confirmation and re-verify afterwards; and `scan --rollback`, which undoes
   the most recent applied fix recorded in `--audit-log`. The two are mutually
   exclusive. One documented carve-out is not a write at all: `rbac check`
   creates `SelfSubjectAccessReview` objects — a virtual resource the API
   server evaluates and never persists, the same API `kubectl auth can-i` uses
   — which makes it the only POST outside remediation and changes no cluster
   state.
2. **No LLM call decides a write.** Remediation is chosen by deterministic
   code, and a read-only surface may never import `internal/remediate` or
   `internal/explain`. That list has grown well past the first two:
   `internal/mcp`, `internal/gate` (with `internal/findings`,
   `internal/sarif`, `internal/rolloutwait`), `internal/tui`,
   `internal/rbacprofile`, `internal/policy`, `internal/parallel`,
   `internal/fleet` and `internal/fleetfile` all carry the wall. Several
   packages go further and import nothing from kubeagent at all —
   `internal/jsonschema`, `internal/dashboard`, `internal/baseline`,
   `internal/glob`, `internal/knownissues` and `internal/policypack` — which
   makes the reach impossible by construction rather than by rule. Each wall
   is enforced by a test in its own package; adding a package means deciding
   which one it inherits.
3. **The diagnostic core works offline.** No API key is required for anything
   except the explicitly opt-in `--explain` and `--investigate` paths.
4. **No cluster identity in artifacts that travel** — the `gate` verdict, the
   SARIF output, and `--output html` carry no context name, API server URL, or
   kubeconfig path.

## Development

Go 1.26 or newer. On this project's usual environment Go lives outside the
default `PATH`:

```bash
export PATH=$PATH:/usr/local/go/bin

go build ./...          # build every package
go build -o kubeagent . # build the binary
go vet ./...
go test ./...
./kubeagent scan --kubeconfig ~/.kube/config
```

CI runs exactly `go vet ./...`, `go test ./...`, and `go build ./...`, plus the
DCO check. Run them locally before pushing.

### Layout

- `main.go` — only the `version` symbol the release workflow stamps with
  `-ldflags`. The CLI is a Cobra command tree in `internal/cli`, one file per
  command; flags are declared per command and never as persistent flags.
- `internal/cluster`, `internal/collect` — connecting and listing. These do
  I/O, and are tested with client-go's fake clientset.
- `internal/diagnose` and the per-concern packages beside it — detectors.
- `internal/report`, `internal/htmlreport`, `internal/sarif` — rendering.
- `docs/`, `website/docs/` — design notes and the user-facing documentation
  site.

### Testing

**Write the failing test first, watch it fail, then implement.** That is the
house style, and it is what review will ask about.

- **Detectors are pure functions.** Test them with fake pods and other fake
  objects (see the `helpers_test.go` files) — no cluster needed.
- **I/O packages** use client-go's fake clientset rather than a live cluster.
- **The golden output test** in `internal/report/golden_test.go` snapshots the
  full `scan` text output. When a report-format change is intentional,
  regenerate it:

  ```bash
  go test ./internal/report -run TestGoldenScanOutput -update
  ```

  Then refresh the README demo GIF and the quickstart example output in
  `website/docs/quickstart.md` in the same pull request.
- **Fuzzing.** Twelve native fuzz targets cover the parsers, the policy loader,
  the text sanitizer and the detector set (`FuzzDetectors`, `FuzzClassify`,
  `FuzzRedactURL`, `FuzzRedactError`, `FuzzParseResponses`, `FuzzParseReadyz`,
  `FuzzCertAssess`, `FuzzLoadPolicy`, `FuzzEvaluatePolicy`, `FuzzResolvePath`,
  `FuzzGlob`, `FuzzLine`). Their seed corpora
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
- A larger end-to-end suite that breaks pods on a disposable Kind cluster lives
  in [chaos/](chaos/) and is run before a release, not on every PR.

## Sign your commits (DCO)

kubeagent uses the
[Developer Certificate of Origin](https://developercertificate.org/). It is a
one-line statement that you wrote the contribution, or otherwise have the right
to submit it under this project's license. There is no CLA to sign.

Add the sign-off by committing with `-s`:

```bash
git commit -s -m "feat(diagnose): detect stuck finalizers"
```

which appends a trailer using your `user.name` and `user.email`:

```
Signed-off-by: Your Name <you@example.com>
```

Use your real name and a working email address. Every commit in a pull request
needs the trailer; CI checks this and will fail the PR if one is missing. To
fix a branch you already wrote:

```bash
git rebase --signoff main    # sign every commit on the branch
git push --force-with-lease
```

## Pull requests

- **Branch off `main`** and keep the branch focused on one change.
- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/):
  `feat(scope): …`, `fix(scope): …`, `docs: …`, `refactor: …`, `test: …`,
  `chore: …`. The subject is written in the imperative mood.
- **Update [CHANGELOG.md](CHANGELOG.md)** under `## [Unreleased]` for any
  user-visible change, in the style of the entries already there — say what
  changed for the operator, not which files moved.
- **Update the documentation** in `website/docs/` when behavior, flags, or
  output change.
- **Do not bump the version or tag a release** in a pull request; releases are
  cut by maintainers.

A maintainer will review your pull request. Expect questions about the
invariants above and about test coverage — they are not an objection to the
change, they are how a tool that runs against production clusters stays
trustworthy. Once approved and green, a maintainer merges it.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), and you certify the
[DCO](https://developercertificate.org/) with your sign-off.
