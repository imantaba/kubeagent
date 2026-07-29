# Interactive TUI

`kubeagent tui` is a full-screen, keyboard-driven browser over one scan. It is
for the moment a scan produces more findings than fit on a screen and you want
to filter to the criticals, read one in full, and check what kubeagent could not
see — without re-running the command four times.

```bash
kubeagent tui
kubeagent tui -n shop
```

It shows **exactly what bare `kubeagent scan` shows**: the same default detector
set, the same findings, in the same order. Not a subset, not a superset.

## Keys

| Key | Does |
|-----|------|
| `↑` `k` / `↓` `j` | move |
| `g` / `G` | first / last finding |
| `⏎` `→` `l` | open the selected finding |
| `esc` `←` `h` | back to the list |
| `1` / `2` / `0` `a` | critical only / warning and above / everything |
| `b` | blind spots — what kubeagent could not read |
| `r` | re-scan |
| `?` | the key map, and the coverage note |
| `q` `Ctrl-C` | quit |

Anything else is ignored.

## Flags

Three, matching their `scan` spellings and defaults exactly:

| Flag | Default | Meaning |
|------|---------|---------|
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | path to kubeconfig |
| `--context` | current-context | kubeconfig context to use |
| `-n` / `--namespace` | all namespaces | namespace to browse |

There is no `--output`: a TUI seizes the terminal and is not redirectable, so it
is not an output format. Piping or redirecting it is refused before kubeagent
touches the network:

```console
$ kubeagent tui > out.txt
kubeagent: tui needs an interactive terminal; use 'kubeagent scan' for pipes and files
```

## What it deliberately does not do

**No LLM call, on any path.** `--explain` and `--investigate` are not accepted
here. This is a stronger claim than read-only and worth stating on its own: a
re-scan key that silently re-billed an API call every time you pressed it would
be a trap.

**No `--fix`.** Remediation stays where its guard rails are — the fixed
allowlist, the protected namespaces, the per-action confirmation and the
re-verify all live in `kubeagent scan --fix`. A keystroke is the wrong gesture
for a cluster write.

**No live refresh.** The screen is one snapshot, and `r` is how it changes.
Continuous watching is [`kubeagent watch`](watch-mode.md), which is built for it.

**No opt-in advisories.** `--security`, `--certs`, `--capacity`, `--drift`,
`--operators` and the rest are not run here. The help screen says so.

**No cluster identity in the chrome.** No context name and no kubeconfig path in
any frame. A failed re-scan's message appears in the footer with the same
`internal/redact` treatment the rest of the CLI applies — path, query and
userinfo stripped, `scheme://host` kept.

## Blind spots

`b` opens what kubeagent could not read, and the header announces the count
whenever there is one, so a partial scan never looks like a clean one.

Unlike the [HTML report](html-report.md), the TUI shows those reasons
**verbatim**. The HTML report classifies them because it is written to be
forwarded and an authorization error embeds the username. A TUI frame is on your
own screen and is never captured, so classifying would throw away the detail
that tells you which credential to fix.

## Notes

- Read-only toward the cluster: `get` and `list` only, not even `watch`.
- `NO_COLOR` is honoured. Under it the frame renders without any colour.
- The terminal is restored on every exit — quit, `Ctrl-C`, `SIGTERM`, `SIGHUP`,
  and a panic. kubeagent runs on the alternate screen, so quitting gives you
  back the scrollback you had before it started.
- `Ctrl-Z` is ignored. Raw mode turns off the terminal's signal generation, and
  resuming into a terminal whose mode no longer matches would leave you with an
  unusable shell.
- Below 40×10 the frame says so rather than drawing a broken table.
