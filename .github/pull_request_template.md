<!--
Thanks for the pull request. The checklist is short on purpose — every item is
something a reviewer would otherwise have to ask about.

Security fixes do not start here: see SECURITY.md.
-->

## What this changes

<!-- One or two sentences on the behavior that changes, from the operator's point
of view. "Renames X to Y" is a diff summary; "scan no longer reports a healthy
CronJob as failed" is a change. -->

## Why

<!-- The problem this solves. Link the issue it closes: "Closes #123". -->

## How it was verified

<!-- What you ran, and what it showed. Paste the relevant output, redacted. If
you tested against a real cluster, say which kind. -->

## Checklist

- [ ] Commits are signed off (`git commit -s`) — see [CONTRIBUTING.md](../CONTRIBUTING.md)
- [ ] `go vet ./...`, `go test ./...`, and `go build ./...` pass
- [ ] Tests were written first, and the new test fails without the change
- [ ] `CHANGELOG.md` updated under `## [Unreleased]` (any user-visible change)
- [ ] Documentation in `website/docs/` updated (behavior, flags, or output changed)
- [ ] Golden output regenerated deliberately, if the report format changed
      (`go test ./internal/report -run TestGoldenScanOutput -update`)

## Invariants

Confirm the change keeps these true, or say plainly which one it touches and why:

- [ ] Read-only toward the cluster — `get`/`list`/`watch` only, outside the
      guard-railed `--fix` path
- [ ] No LLM call decides a write, and `internal/mcp` and `internal/gate` still
      import neither `internal/remediate` nor `internal/explain`
- [ ] The diagnostic core still works with no API key
- [ ] No cluster identity added to an artifact meant to travel (the `gate`
      verdict, SARIF, `--output html`)
