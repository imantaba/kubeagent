# Security Policy

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Report it privately through GitHub:
[open a security advisory](https://github.com/imantaba/kubeagent/security/advisories/new).
The report is visible only to you and the maintainers, and it gives us a
private space to develop and test a fix, and to request a CVE where one is
warranted.

Please include:

- the kubeagent version (`kubeagent version`) and how it was installed;
- the affected subcommand (`scan`, `watch`, `mcp`, `gate`, `tui`) and flags;
- what an attacker gains, and what access they need to start;
- a reproduction — a manifest, a kubeconfig shape, or a transcript — with any
  real cluster identifiers redacted.

**Response targets:** acknowledgement within **three working days**, an initial
assessment within **seven days**, and a fix or a documented mitigation for a
confirmed high-severity issue within **30 days**. We will keep you updated if
an issue takes longer, and we will credit you in the advisory and the
[CHANGELOG](CHANGELOG.md) unless you ask us not to.

## Supported versions

kubeagent is pre-1.0 and ships fixes on the **latest released minor version**
only. There are no long-term-support branches yet; upgrade to the latest
release before reporting, and expect a fix to arrive as a new release rather
than as a patch to an older one.

## Verifying a release

Release archives are signed with [cosign](https://docs.sigstore.dev/) keyless
signing, ship an SPDX SBOM, carry SLSA build provenance, and are
byte-reproducible from the tagged source. There is no key to distribute:
verification pins the release workflow's identity. The commands are in
[Verifying a release](https://k8sproject.top/verify/).

## What counts as a vulnerability here

kubeagent reads production clusters and can be pointed at an LLM, so its
security boundary is mostly about what it *sends* and what it *writes*. The
following are security bugs, not merely defects:

- **Any write to the cluster that is not an explicitly confirmed `--fix`
  action.** kubeagent is read-only by default: `scan`, `watch`, `mcp`, `gate`,
  and `tui` issue only `get`/`list`/`watch`. A code path that creates, updates,
  patches, or deletes anything outside `--fix` is a vulnerability.
- **A `--fix` action escaping its guard rails** — acting outside the fixed
  allowlist, touching a protected namespace, skipping the per-action
  confirmation, or being selected by a model rather than by deterministic code.
- **Leaking cluster or workload secrets into an LLM prompt.** `--explain` and
  `--investigate` send findings, not pod specs, environment variables, or
  Secret contents. Anything that widens what leaves the machine is a
  vulnerability.
- **Leaking cluster identity into an artifact that is meant to travel** — the
  `gate` verdict, the SARIF output, and the `--output html` report deliberately
  carry no context name, API server URL, or kubeconfig path.
- **Credential exposure** — writing a kubeconfig, bearer token, or API key to
  logs, metrics, the `watch` daemon's HTTP endpoints, or an MCP tool result.
- **An unauthenticated write or an information disclosure through the `watch`
  daemon's HTTP surface** (`/metrics`, `/issues`), or through the MCP server's
  stdio protocol.

Out of scope: findings that require an attacker to already hold cluster-admin
or local shell access as the user running kubeagent; missing hardening that has
no exploit path; vulnerabilities in a dependency with no reachable path from
kubeagent code (report those upstream, and tell us so we can bump).

## Disclosure

We follow coordinated disclosure. A fix ships first, then the advisory is
published with the reporter's credit and, where applicable, a CVE. We ask
reporters to hold public details until the advisory is out, or 90 days from the
report, whichever comes first.
