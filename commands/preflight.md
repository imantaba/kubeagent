---
description: Pre-deploy go/no-go check - kubeagent triage plus the drift and capacity advisory sections, with blind spots listed.
argument-hint: "[namespace]"
---

Run a pre-deploy readiness check. Namespace scope: $1 (empty means all namespaces).

Follow the `triaging-a-cluster` skill and the `reading-kubeagent-findings`
skill.

1. Preflight: confirm `kubeagent` is on PATH.
2. Call `kubeagent_triage`, passing `namespace` only if $1 is non-empty.
3. Call `kubeagent_advisory` once, with sections `drift` and `capacity`. These
   two are the pre-deploy questions: is live state already diverging from Git,
   and is there room to schedule what is about to land. Both are requested
   unconditionally here — unlike ordinary triage — because that is what makes
   this a gate.
4. Inspect any `critical` finding with `kubeagent_inspect`. Skip `high` and
   below unless a `critical` one points at them; this is a gate, not a full
   audit.

Report a single **GO** or **NO-GO**, then the reasoning:

- NO-GO if there is any `critical` finding, or if `drift` shows the target
  namespace already diverging from Git.
- GO with caveats if the only findings are `medium` or below.
- **Always** list the blind spots: every entry in `coverage.partial`, and
  `metricsServer` if it is not `available` — a capacity verdict without
  metrics-server is a guess, and say so.

Never treat a skipped check as a passing one. A gate that says GO because it did
not look is worse than no gate.

Do not remediate, and do not run the deployment.
