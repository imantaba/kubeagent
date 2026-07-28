# HTML report

`kubeagent scan --output html` writes one self-contained HTML document to
stdout: the artifact you attach to an incident ticket, paste into a pull
request, or mail to a colleague who has no cluster access.

```bash
kubeagent scan --output html > incident-4821.html
kubeagent scan -n prod --output html > prod.html
```

`--output` accepts `text`, `json`, or `html`. Anything else is rejected before
kubeagent touches the network:

```console
$ kubeagent scan --output bogus
kubeagent: unknown output format "bogus" (want text, json or html)
```

## What the document contains

- **A header** — kubeagent version, the namespace scope (or "all namespaces"),
  the generation timestamp, and the finding counts by severity.
- **Blind spots** — whatever kubeagent could not read, whenever there is any.
- **Findings** — every finding, highest severity first, with a severity filter.
- **Detail** — cluster health, the workload inventory, and the `--explain`
  narrative when you ran with `--explain`, each in a collapsed section so the
  findings stay at the top.

The opt-in advisory sections (`--capacity`, `--drift`, `--operators`,
`--certs`, and the rest) are not in the document yet. Their *findings* are —
`--output html` renders the same finding set the text report ranks — but their
detailed views only appear in `--output text` and `--output json`.

`--investigate` is in the same position. Its findings reach the document, but
its narrative — and the list of sources it consulted — do not; those still need
`--output text` or `--output json`.

## What the document deliberately does not contain

**No cluster identity.** No context name, no API server URL, no kubeconfig
path. A context name is not safe by default — in the wild they carry
`arn:aws:eks:eu-west-1:<account>:cluster/prod` or
`admin@prod-db.internal.corp` — and this file is meant to be forwarded.
Whoever shares it names the cluster in the ticket. This is the same rule
[`kubeagent gate`](ci-gate.md) follows for its verdict, so both shareable
artifacts behave identically.

**No JavaScript.** The severity filter is pure CSS. There is no external
stylesheet, font, or image either, so the file opens offline and renders
under a strict Content-Security-Policy — which is what artifact previews,
sandboxed mail clients, and corporate proxies enforce.

## Blind spots are not optional

If kubeagent could not list a resource — RBAC denied it, a CRD is absent, the
API timed out — the document says so in its own block, above the findings:

> kubeagent could not read the following, so the findings below are incomplete.

A shared report that silently omits what it could not see is the same
green-when-blind failure [CI gate mode](ci-gate.md) exists to prevent, and a
rendered document is easier to over-trust than an exit code.

## Escaping

Container termination messages, event reasons, and image-pull errors are
free-form strings the cluster controls, and they land verbatim in this
document. kubeagent renders it with Go's `html/template`, whose contextual
auto-escaping neutralizes them. A pod whose crash message contains markup
produces a report that displays that message as text.

## Notes

- `scan`'s exit code is unchanged: still `0` on an unhealthy cluster. Gating on
  health is [`kubeagent gate`](ci-gate.md).
- `gate` has no `--output html`. Its job is a verdict, not a document; its
  machine-readable surface is JSON and SARIF.
- Two runs against an unchanged cluster differ only in the header timestamp, so
  reports from the same incident diff cleanly.
- `--output html --fix` interleaves the remediation transcript after the
  document, exactly as `--output json --fix` does. Redirect the document to a
  file first, or run `--fix` separately.
