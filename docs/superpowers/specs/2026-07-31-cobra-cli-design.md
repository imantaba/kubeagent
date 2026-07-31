# Cobra CLI — design

Theme H sub-project 5 of 7 on the road to v1.0. It retires the second and last
of the two v1 simplifications the roadmap named: the hand-rolled
standard-library `flag` CLI.

The first — a strictly sequential scan — was retired in v0.72.0. This one is
different in character. Bounded concurrency changed how kubeagent talks to a
cluster; this changes nothing about the cluster at all. It changes only the
program's front door. That makes it lower risk in one sense and higher risk in
another: the CLI is the entry point to every feature kubeagent has, so a
regression here is a regression everywhere at once.

## The problem

`main.go` is 1286 lines. It dispatches subcommands with a chain of
`if len(args) > 0 && args[0] == "…"` tests, builds seven `flag.FlagSet`s
carrying 86 flag definitions between them, and documents the whole surface in a
single 1900-character `usage:` string that no one reads to the end.

```go
if len(args) > 0 && args[0] == "watch" { return runWatch(args[1:]) }
if len(args) > 0 && args[0] == "mcp"   { return runMCP(args[1:]) }
…
if len(args) == 0 || args[0] != "scan" {
    return fmt.Errorf("usage: %[1]s scan [--kubeconfig path] [--context name] …", invokedAs)
}
```

Three things follow from that shape:

- **No shell completion.** kubeagent ships as a `kubectl` krew plugin, where tab
  completion is the norm for every neighbouring plugin. Writing a completion
  script by hand for 86 flags across 8 commands is not a serious proposal.
- **No per-command help.** `kubeagent gate --help` prints the same wall of text
  as `kubeagent scan --help`, because there is only one wall.
- **The dispatch chain is where new commands go wrong.** Adding a subcommand
  means remembering to add it above the `args[0] != "scan"` fallback, and
  nothing enforces that.

None of this is broken. All of it is the accumulated cost of a decision made
deliberately at v1 — and v1.0 is where the roadmap says to pay it off.

## Goals

- Every command line valid at v0.72.0 stays valid, with the same behaviour, the
  same stderr, and the same exit code.
- `kubeagent completion bash|zsh|fish|powershell` works, and works under the
  `kubectl kubeagent` spelling too.
- Per-command `--help` replaces the single usage string.
- `main.go` stops being the place the whole CLI lives.

## Non-goals

- New flags, or new commands other than `completion`.
- Rewording help text beyond what Cobra's format imposes.
- Any change to `--fix`, to what a scan reads, or to any rendered report byte.
- Any JSON shape change. No `schemaVersion` bump anywhere.
- Cobra's config-file, viper, or persistent-flag-inheritance machinery. Flags
  are declared per command exactly as they are today; `--kubeconfig` appearing
  on six commands stays six declarations, because two of the six
  (`rbac print`, `version`) deliberately do not accept it and a persistent flag
  would silently give it to them.

## Architecture

### The package

A new `internal/cli` package holds the whole command tree. `main.go` becomes:

```go
package main

import (
	"os"

	"github.com/imantaba/kubeagent/internal/cli"
)

// version is the build version, overridden at release time via
// -ldflags "-X main.version=<tag>".
var version = "dev"

func main() { os.Exit(cli.Main(version)) }
```

The `-ldflags "-X main.version=…"` target stays `main.version`, so
`.github/workflows/release.yml` and `scripts/build-release-archives.sh` need no
change. The value is passed into the package rather than duplicated there.

Files, one per command, following the package's existing internal layout
convention:

| File | Holds |
|---|---|
| `root.go` | `Main`, `Run`, the root command, `invocationName`, `invokedAs`, `warnf`, `exitError`, `exitCodeFor` |
| `normalize.go` | the single-dash argv shim |
| `scan.go` | `scan` and its 34 flag declarations, plus the render / `--fix` / `--rollback` tail |
| `watch.go` | `watch`, `contextList`, `buildTargets` |
| `gate.go` | `gate`, `stringList`, `scopeTo`, `gateScanOptions` |
| `mcp.go` | `mcp` |
| `tui.go` | `tui`, `tuiScanOptions` |
| `rbac.go` | `rbac`, `rbac print`, `rbac check`, `splitFeatureList`, `selectedFeatures`, `selectedRules` |
| `schema.go` | `schema` |
| `version.go` | `version`, `versionLine` |
| `completion.go` | `completion` |
| `helpers.go` | `envOr`, `envDur`, `envBool`, `envFloat`, `envInt`, `envDuration`, `splitCSV`, `firstNonEmpty` |
| `fix.go` | `runFixes`, `runRollback` |

`internal/cli` imports every surface package, which is fine: the invariants in
CLAUDE.md constrain what `internal/mcp`, `internal/gate`, `internal/tui`,
`internal/rbacprofile`, `internal/parallel`, `internal/safetext` and
`internal/jsonschema` may import, not who imports them. `internal/cli` is
imported by `main.go` alone.

### The entry point

```go
// Run executes one command line and returns its error, exactly as main.go's
// run() does today. Tests call this.
func Run(args []string) error

// Main wraps Run with the process-level concerns: rendering the error to
// stderr and choosing the exit status. version comes from main, so the
// release workflow's -ldflags target stays main.version.
func Main(version string) int
```

`invokedAs` stays a package-level variable in `internal/cli`, initialised from
`os.Args[0]` exactly as it is today, and still overridable by tests. It is not
threaded through `Main`, because `warnf` and the error renderer both read it
from package scope and always have.

`Run` keeps `run`'s signature — `args` is `os.Args[1:]`, with the subcommand at
`args[0]`. That is what makes the 129 tests in `main_test.go` a package move
rather than a rewrite: only the call site name and the package clause change.

### The root command

```go
root := &cobra.Command{
	Use:           "kubeagent",
	Short:         "Read-only Kubernetes troubleshooting",
	SilenceErrors: true,
	SilenceUsage:  true,
	Annotations: map[string]string{
		cobra.CommandDisplayNameAnnotation: invokedAs,
	},
}
```

`SilenceErrors` and `SilenceUsage` are load-bearing, not stylistic. Without
them Cobra prints `Error: <msg>` itself and dumps the command's usage block
after every failure — two changes to stderr on every error path in the program.
With them Cobra returns the error up to `Main`, which renders it the way
`main()` renders it today.

Both are set on the root and inherited, but each command sets them explicitly
too: inheritance in Cobra happens at execution time via `ExecuteC`, and a test
that runs a subcommand directly would not get them.

## The `invokedAs` problem

krew installs the binary as `~/.krew/bin/kubectl-kubeagent`, and `kubectl`
execs it under that name. `invocationName(argv[0])` reads the basename and
returns `"kubectl kubeagent"` for that spelling and `"kubeagent"` otherwise;
every usage line, error and warning is prefixed with the result, so a plugin
user is never told about a `kubeagent` binary that is not on their PATH.

Cobra derives the command path from the root's `Use` field, and `Name()` takes
everything before the first space — so `Use: "kubectl kubeagent"` would produce
a root named `kubectl` and a scan whose path renders `kubectl scan`. Wrong in a
way that is worse than the status quo.

Cobra ≥ 1.8.0 added `cobra.CommandDisplayNameAnnotation` for precisely this
case: `CommandPath()` returns the annotation when the root carries one, so
`kubectl kubeagent scan` renders correctly in help output, in error text, and
in the generated completion scripts. The implementer verifies the constant
exists in the resolved Cobra version before relying on it; if it does not, the
fallback is a root usage template that pipes `.CommandPath` through a template
function, which is more code for the same result.

`invokedAs` remains a package-level variable that tests override, unchanged.

## The single-dash shim

This is the one place where a straight framework swap would break a working
command line.

The standard library's `flag` package treats `-flag` and `--flag` as the same
thing. pflag does not: a single dash introduces shorthands, so `-kubeconfig`
parses as the cluster `-k -u -b -e -c …` and fails. Every one of these works
today and would stop working:

```
kubeagent scan -kubeconfig ~/.kube/config -output json
kubeagent gate -fail-on warning -timeout 5m
kubeagent watch -metrics-addr 127.0.0.1:9090
```

Nothing in this repository's docs, scripts, tests or chaos suite uses that
form — it was checked. But it has worked in every kubeagent release ever cut,
and this sub-project's whole premise is that v1.0 freezes the CLI contract
rather than breaking it on the way in.

So `internal/cli/normalize.go`:

```go
// Normalize rewrites a leading single-dash long flag to the double-dash form
// pflag requires, preserving the standard library's parsing for command lines
// written against every kubeagent release before v0.73.
//
// It rewrites only names the target command actually registers as long flags.
// An unregistered -xyz is left alone so pflag produces its own error rather
// than kubeagent silently inventing a flag. Registered shorthands (-n, -h) are
// left alone. Everything after a bare -- is left alone.
func Normalize(args []string, isLongFlag func(string) bool) []string
```

Cases it handles, each a test:

| Input | Output | Why |
|---|---|---|
| `-kubeconfig path` | `--kubeconfig path` | registered long name |
| `-kubeconfig=path` | `--kubeconfig=path` | `=` form, split on the first `=` |
| `-n default` | `-n default` | registered shorthand, untouched |
| `-h` | `-h` | shorthand |
| `--kubeconfig path` | unchanged | already correct |
| `-xyz` | unchanged | not registered — pflag reports it |
| `-` | unchanged | a bare dash is a value, not a flag |
| `--` and everything after | unchanged | terminator |
| `path/-with-dash` | unchanged | not a flag, no leading dash |

The shim runs once per command, after the command has been resolved and before
`Execute` parses — `isLongFlag` closes over that command's own `*pflag.FlagSet`
via `Lookup(name) != nil`. Consulting the real flag set is what makes the shim
safe: it can never rewrite something the command does not know about.

Its effect on `--` handling is nil by construction: it stops rewriting at the
first bare `--` and copies the rest through.

## Flags that are not plain values

Two flags are repeatable, and both have a trap in the obvious translation.

`watch --context` is a `contextList` today: it appends, does not split on
commas, and rejects an empty value with the exact message
`--context cannot be empty`. `gate --allow-partial-read` is a `stringList`:
appends, no splitting, no validation.

pflag offers two array types and only one is correct. `StringSlice` splits its
input on commas, so `--context a,b` would become two contexts where today it is
one context literally named `a,b` — legal in a kubeconfig, if unusual.
**`StringArray` is the correct translation for both**, and the empty-value
check for `--context` moves into the command's `RunE` so its message survives
verbatim.

## Error handling

Every error string and every exit code is frozen. Three rules produce that:

1. **`SilenceErrors` + `SilenceUsage`, and `Main` renders.** The existing
   `main()` body moves into `Main` unchanged: `fmt.Fprintf(os.Stderr, "%s: %v\n",
   invokedAs, err)`, skipped when the error is an `*exitError` with an empty
   `msg`, then `os.Exit(exitCodeFor(err))`.

2. **Validation stays in `RunE`, not in Cobra's validators.** It is tempting to
   replace

   ```go
   if *rollback && *fix { return fmt.Errorf("--rollback and --fix are mutually exclusive") }
   ```

   with `cmd.MarkFlagsMutuallyExclusive("rollback", "fix")`. Do not: Cobra
   words that error itself, in its own format. Every check that exists after
   `fs.Parse` today moves into the `RunE` body in the same order, producing the
   same string.

3. **The fallback usage error survives.** Today an unknown or missing
   subcommand returns the long `usage: …` error, which `main()` prints to
   stderr with exit 1. Cobra's default for an unknown subcommand is its own
   `unknown command "x" for "kubeagent"` on stderr. The root's `RunE` returns
   the existing usage error when called with no subcommand, and
   `root.SetFlagErrorFunc` plus an unknown-command check preserve the current
   text and exit status.

`gate`'s five-code contract (`gate.CodeUsage`, `gate.CodeInconclusive`, and the
verdict codes) is carried entirely by `exitError`, which is unchanged, so it
needs nothing beyond rule 1.

`--help` currently returns `flag.ErrHelp` from the inner flag sets, which
`main()` prints and exits 0 on. Under Cobra, `--help` prints help to stdout and
returns nil. Exit status is 0 either way. The tests asserting
`errors.Is(err, flag.ErrHelp)` are testing the mechanism rather than the
behaviour and are rewritten to assert the observable one: help text on stdout,
exit 0, nothing on stderr.

## `completion`

```
kubeagent completion bash|zsh|fish|powershell
```

Cobra generates all four. The command takes exactly one argument, validated
with `cobra.ExactValidArgs(1)` and `ValidArgs`, writes the script to stdout,
and touches nothing else. It builds no client, reads no kubeconfig, makes no
LLM call, and needs no cluster — which also means it needs no RBAC row in
`internal/rbacprofile`.

Under krew, `kubectl` completion for a plugin needs a `kubectl_complete-kubeagent`
executable on PATH. The docs page documents the two-line shim; the krew
manifest is not changed, because krew installs one binary and the shim is a
user-side step.

The generated scripts embed the command path, so the
`CommandDisplayNameAnnotation` work above is what makes the `kubectl kubeagent`
spelling complete correctly.

## Testing

The migration's safety net is written **before** the migration, against the
current stdlib implementation, and must pass **unchanged** afterwards. This is
the technique that carried sub-project 4: a determinism test that only passes
once concurrency is enabled is a test written to the implementation. The same
holds here — a CLI test that only passes once Cobra lands proves nothing about
compatibility.

Three new tables, all in `main_test.go` first and moving to
`internal/cli/cli_test.go` with everything else:

- **Command surface.** Every subcommand × every flag it declares, each with a
  non-default value, asserting the value reaches the command's options struct.
  A dropped flag becomes a failing case rather than a silently defaulted scan.
  This is the table that catches the mechanical transcription errors that a
  86-flag migration invites.
- **Error strings.** Every validation path that returns an error today, keyed
  by the command line that triggers it, asserting the exact message and the
  exit code `exitCodeFor` derives from it.
- **`invokedAs`.** Both spellings, across an error path, a warning path, and —
  after the migration — a help path and a completion script.

Plus, new and Cobra-specific:

- `Normalize` unit tests, one per row of the table above.
- A test that every command in the tree sets `SilenceErrors` and `SilenceUsage`,
  walked with `root.Commands()` recursively, so a command added later cannot
  quietly reintroduce Cobra's error format.
- A test that `completion` emits a non-empty script for each of the four shells
  and that the script names the invoked spelling.

Unchanged and still binding: `internal/report/testdata/golden-scan.txt` stays
byte-identical, `go test` runs with `-p 2` and never `-short`, and CI's
`go test -race ./...` must stay green.

## Supply chain

Theme H slice 1 shipped keyless cosign signatures, SPDX SBOMs and SLSA
provenance for every release. Adding a direct dependency is therefore a
supply-chain event, and worth stating plainly rather than assuming:

| Module | Status now | Status after |
|---|---|---|
| `github.com/spf13/cobra` | absent | direct require |
| `github.com/spf13/pflag` | indirect (via client-go) | direct require |
| `github.com/inconshreveable/mousetrap` | absent | indirect (cobra, Windows-only) |

One genuinely new module in the build graph, plus one promotion of a module
already compiled into every kubeagent binary shipped since v1. Both `cobra` and
`pflag` are Kubernetes-ecosystem staples already present in the transitive
graph of anything that links client-go; `kubectl` itself is built on them.

The SBOM is generated from the built artifact, so it picks the new modules up
with no workflow change. No vendoring: this repository has never vendored, and
`go.sum` already pins every module by hash.

Pin `github.com/spf13/cobra v1.10.2` — the latest release as of this spec, and
comfortably past the v1.8.0 that introduced `CommandDisplayNameAnnotation`. Add
it with `go get github.com/spf13/cobra@v1.10.2`, not `@latest`, so the plan and
the resulting `go.mod` agree. `pflag` stays at the v1.0.9 already resolved for
client-go unless cobra requires higher, in which case the `go get` output
records the bump.

## Documentation

- `CLAUDE.md` — the invariant "v1 uses the standard-library `flag` package only
  — no Cobra yet" is replaced by a statement of what the CLI is now: a Cobra
  command tree in `internal/cli`, with the single-dash shim named as the reason
  older command lines keep working, and the note that flags are declared per
  command rather than inherited.
- `website/docs/roadmap.md` — Theme H slice 6 marked shipped.
- `CHANGELOG.md` — Added: `completion`, per-command help. Changed: the CLI is
  built on Cobra. Explicitly **not** a Breaking entry, which is the point.
- `website/docs/features/completion.md` — new page: the four shells, the
  `kubectl kubeagent` shim, and where each shell wants the file.
- `website/mkdocs.yml` — nav entry for it.
- `docs/go-concepts.md` — an entry on struct-literal configuration and method
  values, the two Go idioms a reader meets for the first time in a Cobra
  command tree. Plain example first, then the kubeagent one. (Gitignored —
  edited on disk, never staged.)

## What does not change

The rendered output of every command. The six JSON documents and their
`schemaVersion` values. What a scan reads from a cluster and in what order.
`--fix`'s guard rails. The RBAC feature table. The krew manifest template. The
release workflow. `main.version` as the ldflags target.

## Global constraints

Copied verbatim into the implementation plan:

- Every commit carries a `Signed-off-by` trailer matching its author
  (`git commit -s`) — `main` enforces DCO.
- No `Co-Authored-By` trailer and no AI attribution anywhere: commits, PR text,
  code, comments, docs, changelog.
- Read-only toward the cluster: `get`/`list` only, no writes outside `--fix`.
  `completion` touches no cluster at all.
- Detectors stay pure functions.
- `internal/report/testdata/golden-scan.txt` stays byte-identical.
- `go test` runs with `-p 2` and never `-short`.
- No `schemaVersion` bump — this sub-project changes no JSON shape.
- Blocked reasons stay kubeagent's own words, never the API server's.
- No secrets, credentials, private IPs or internal hostnames anywhere,
  including help text and test tables: RFC 5737 IPs, RFC 2606 domains.
- URLs are credentials — no log line, error, or generated script carries more
  than `scheme://host`.
- Kubeconfig paths are credentials.
- `go.mod` / `go.sum` change is expected here and only here: `cobra` added,
  `pflag` promoted. No other dependency.

## Risks

**The 86-flag transcription.** The single largest failure mode is a flag whose
default, type, or destination is mistyped in the move — `--disk-threshold`
defaulting to 0 instead of 0.80, `--cert-warn-days` bound to the wrong field.
Nothing about the program fails loudly when that happens; a scan just reports
something subtly wrong. The command-surface table exists for this and is worth
more than the rest of the test plan combined. It is written first, against the
current implementation, so it is a real check rather than a transcription of
the same mistake.

**Cobra's help on stderr vs stdout.** Cobra writes help to stdout for `--help`
and to stderr for a usage error. The current code writes the usage error to
stderr and `flag.ErrHelp`-triggered help to stderr as well. The tests assert
which stream carries what, so any drift is caught rather than discovered by a
user piping `--help` into a pager.

**A command that forgets `SilenceUsage`.** Cobra's default is loud, and
inheritance only applies through `ExecuteC`. The recursive tree-walking test
covers today's commands and every command added later.

**Scope creep into "while we're here".** A Cobra tree makes persistent flags,
flag groups, required-flag marking, and command aliases all one line away.
Every one of them changes behaviour. None are in scope.

## Gate

Lightweight real-cluster smoke, per the release skill: this sub-project changes
no cluster interaction, no RBAC, no `nodes/proxy` read, no Helm template. A
two-node Kind cluster, `scan` with a representative flag set, `gate`, `rbac
check`, `tui` started and quit, `mcp` handshaked over stdio, and `completion`
for all four shells — the point being command-surface coverage rather than
outage coverage.

The full chaos suite is not required by the gate rule, but the branch's own
determinism claim is cheap to check there: the same `scan` invocation against
the still-running chaos cluster before and after the migration must produce
byte-identical output.
