# Security Output Redesign — Design

**Date:** 2026-07-12
**Status:** Approved

## Goal

Make `scan --security` output readable on real clusters. Today the text
`SECURITY` section is a flat per-finding wall: a real hetzner-nova run produced
**374 findings across 121 workloads = 498 lines**, dominated by three
near-universal `restricted` checks (`RunAsRoot` ×106, `AllowPrivilegeEscalation`
×119, `CapabilitiesNotDropped` ×134 = 359 of 374) that bury the ~15 genuinely
dangerous `baseline` findings (privileged, hostPath, host namespaces, hostPort,
added capabilities).

Redesign the section to be **signal-first**: lead with a one-line summary, show
the workloads with `baseline`/`kubeagent` findings in full (the act-on-these),
and fold the near-universal `restricted` gaps into a compact aggregate — with a
`--security-verbose` flag to restore the full per-finding listing. The same run
becomes ~35 lines.

**Text-only.** JSON `securityIssues` is unchanged (always all findings).
Advisory unchanged (does not affect the cluster verdict / attention line).

## Scope

- Changed: `internal/report/report.go` (`printSecurityIssues` rewrite,
  `Input.SecurityVerbose`) and `main.go` (`--security-verbose` flag + usage
  string). Tests in `internal/report/report_test.go`.
- **Unchanged:** `internal/secscan` (findings are identical), `internal/scan`
  (`Options`/`Result`), and the JSON output. This is purely a rendering change.

## Tiers

Findings split into two display tiers by `Profile`:

- **Act-on-these** — `baseline` and `kubeagent` (ExposedService). Shown in full,
  per finding, grouped by workload.
- **Hardening gaps** — `restricted`. Near-universal; shown only as an aggregate
  by default; shown in full only with `--security-verbose`.

## Default layout

```text
SECURITY  (advisory — does not affect the cluster verdict)
  15 baseline · 359 restricted hardening gaps · 121 workloads

  ✗ cattle-monitoring-system/rancher-monitoring-prometheus-node-exporter  DaemonSet
      [baseline] HostNamespaces — pod shares the host network/PID namespace
      [baseline] HostPath — mounts hostPath /proc (writable host filesystem)
      [baseline] HostPath — mounts hostPath /sys (writable host filesystem)
      [baseline] HostPath — mounts hostPath / (writable host filesystem)
      [baseline] HostPort — container "node-exporter" binds host port 9796
  ✗ cattle-system/rancher  Deployment
      [baseline] HostPath — mounts hostPath /var/log/rancher-audit (writable host filesystem)
      … (act-on-these findings for the remaining flagged workloads)

  restricted (hardening gaps, near-universal): 359 across 118 workloads
    RunAsRoot ×106 · AllowPrivilegeEscalation ×119 · CapabilitiesNotDropped ×134
    → run with --security-verbose to list every finding per workload
```

Rules:

- A workload gets a **detail block only if it has ≥1 act-on-these finding**. Its
  own restricted findings are folded into the aggregate, not listed under it. A
  workload with *only* restricted findings appears **only** in the aggregate
  counts.
- Within a detail block, act-on-these findings render per-finding in the current
  format: `  [<profile>] <check> — <detail>` (two-space indent under the
  workload line). Restricted findings are omitted from the block.
- **Ordering:** detail blocks sorted worst-first — by descending count of
  act-on-these findings, then namespace, then workload. (Assess already sorts
  findings within a workload baseline→restricted→kubeagent; the report groups
  and re-orders the blocks.)
- **Summary header line** (second line): the non-zero tiers joined by ` · ` —
  `N baseline`, `N exposed service(s)` (the `kubeagent`/ExposedService count),
  `N restricted hardening gaps` — then `M workloads` (distinct workloads across
  all findings). Zero tiers are dropped.
- **Restricted aggregate block** (only when there is ≥1 restricted finding, and
  not verbose): the `restricted (hardening gaps, near-universal): N across M
  workloads` line, a per-check counts line (only checks with count > 0, in the
  canonical order `RunAsRoot`, `AllowPrivilegeEscalation`,
  `CapabilitiesNotDropped`, joined by ` · `), and the `--security-verbose` hint.
  `M` here is the count of distinct workloads that have ≥1 restricted finding.

The header summary line is the section's only recap — there is no separate
trailing `Security: …` line (the current one is removed as redundant). None of
this affects the cluster verdict or the "Needs attention" line.

## Verbose layout (`--security-verbose`)

Same summary header, then **every** finding for **every** flagged workload
(all profiles, current grouped-by-workload behavior with per-finding lines),
sorted worst-first. The restricted aggregate block is **omitted** (everything is
already listed).

## Edge cases

- **No findings:** `printSecurityIssues` early-returns (no header). The all-clear
  suppression already keys on `len(SecurityIssues) > 0`, unchanged.
- **Only restricted findings:** no detail blocks; header + aggregate block +
  trailing summary. (All-clear still suppressed — findings exist.)
- **Only act-on-these findings:** detail blocks + header + trailing summary; the
  restricted aggregate block is omitted (0 restricted).
- **`--security-verbose` without `--security`:** the security pass is not run
  (SecurityIssues is nil), so the flag has no effect — documented, mirroring
  `--disk-threshold` needing `--disk-usage`.

## Data flow

`main.go` declares `--security-verbose` (bool) and passes it as
`report.Input.SecurityVerbose`. `printInventoryText` calls
`printSecurityIssues(in.SecurityIssues, in.SecurityVerbose, w)`. The renderer
computes tiers and aggregates from the findings slice (a pure grouping over
`[]secscan.Finding`); no new fields on `secscan.Finding` or `scan.Result`. The
JSON branch (`inventoryReport.SecurityIssues`) is untouched.

## Testing

- **Default view:** findings mixing `baseline` + `restricted` across workloads →
  assert the baseline lines appear; the restricted **aggregate** line and
  per-check counts appear; individual restricted finding detail lines do **not**;
  the `--security-verbose` hint appears; a restricted-only workload has no detail
  block.
- **Verbose view:** same input with `SecurityVerbose: true` → individual
  restricted finding lines **do** appear; the aggregate block does **not**.
- **Only-restricted:** no detail blocks; aggregate present; all-clear suppressed.
- **Only-baseline:** detail blocks present; no aggregate block.
- **JSON:** `securityIssues` still contains the restricted findings regardless of
  the flag (unchanged behavior).
- **Ordering:** a workload with 2 act-on-these findings sorts before one with 1.

## Docs

- `CHANGELOG.md` (under the existing `## [Unreleased]`): note the redesigned
  signal-first `SECURITY` section and the `--security-verbose` flag.
- `website/docs/features/diagnostics.md`: update the Security posture subsection
  to describe the summary + act-on-these vs aggregated-restricted split and
  `--security-verbose`.

Exact names to use verbatim: flag `--security-verbose`; the profile/tier words
`baseline`/`restricted`/`kubeagent`; the aggregate label wording above.
