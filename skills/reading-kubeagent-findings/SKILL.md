---
name: reading-kubeagent-findings
description: Use when reading a result from any kubeagent MCP tool - explains the coverage block, the difference between a skipped check and a passing one, and why severity and confidence are independent axes.
---

# Reading a kubeagent result

Every field in a kubeagent result is computed by a detector. None of it is
generated prose. Read it precisely and it will not mislead you; read it loosely
and it will.

This skill exists to prevent one specific mistake: **reading an absent key as
good news.**

## Read coverage first

`kubeagent_triage`, `kubeagent_inspect`, and `kubeagent_advisory` each return a
`coverage` object. It exists so you can tell "nothing is wrong" from "nothing
was checked". Read it before the findings.

### checksSkipped is not "passed"

`checksRun` names the checks that executed. `checksSkipped` names the ones that
did not, each with a reason. A skipped check produced no finding because it never
ran.

Seven checks are skipped on **every** `kubeagent_triage` call, and they fall
into two groups. A server started **without** `--logs` skips an eighth,
`log-tails`. The shipped Claude Code plugin passes `--logs`, so seven is the
count for a plugin install and eight for a hand-configured server that omits
the flag.

Five are not reachable through the MCP server at all — kubelet health,
control-plane health, DNS health, credential lint, and disk usage. Only the
CLI's `--kubelet-health`, `--control-plane-health`, `--dns-health`,
`--lint-secrets`, and `--disk-usage` flags run them.

So a triage result is never grounds for saying "DNS is fine". If the user asks
about DNS, tell them the tool did not check and give them the CLI command:

```bash
kubeagent scan --dns-health
```

The other two — the security and certificate sections — are skipped by triage
but *are* reachable: call `kubeagent_advisory` with the section you need.

### partial is a blind spot

`partial` names a resource kubeagent tried to list and could not — most often an
RBAC refusal. An empty section under a `partial` entry means **unknown**, not
**clean**.

Report it as a blind spot, and name the resource. "No NetworkPolicy problems
found" is wrong when NetworkPolicies are in `partial`. "kubeagent could not read
NetworkPolicies, so that is unchecked" is right.

`partial` covers the reads kubeagent can do without. A refusal on one it cannot
— pods, or the workload kinds it diagnoses from — fails the whole call instead,
and `kubeagent_triage` returns an error naming the resource and the verb. That
is not a cluster problem to report as a finding. Tell the user their credentials
are too narrow and give them the profile:

```bash
kubeagent rbac print --profile scan
```

### metricsServer: "not-checked" means never looked

`coverage.metricsServer` is the literal string `"not-checked"` until a call
requests capacity data — that is, until `kubeagent_advisory` runs with section
`capacity`. Only then does it become `"available"` or `"absent"`.

Reading `"not-checked"` as "no metrics problem" silently misses a cluster with no
metrics-server installed at all.

## severity and confidence are independent

`severity` is how bad it would be: `critical` when a detector matched a concrete
failure mode, `warning` when a health check flagged something that needs a
human. Those are the only two values. `confidence` is how sure kubeagent is:
`high` when the state is one Kubernetes itself asserts, `medium` when it is a
kubeagent heuristic. The two vocabularies do not overlap — there is no `high`
severity and no `critical` confidence.

Only `critical` findings carry a `confidence` at all — it comes from the
detector that matched. A `warning` has no `confidence` key, and its absence says
nothing about how sure kubeagent is. Do not read a missing `confidence` as a low
one.

A `critical` finding carrying `medium` confidence is a **lead to verify**, not a
conclusion to report. Escalate it with `kubeagent_inspect` and read the object's
events before you tell the user their production database is failing.

## verdict is derived, not separate

`verdict` is `healthy` or `degraded`, computed from the findings. It is a summary
of them, not an additional independent judgement. Do not report a verdict that
contradicts the findings list you are also showing.

## Quote findings; do not strengthen them

`reason` and `detail` are what the detector concluded. Quote them or paraphrase
them faithfully. Do not upgrade "container repeatedly crashes after starting"
into "the application is broken" — kubeagent observed the former and did not
claim the latter.

`remediationHint` is kubeagent's suggestion — the same deterministic, reviewed
next step that `kubeagent scan --suggest` prints. Anything else you suggest is
yours, and say so plainly.
