# kubeagent — Design: credential lint (gap-feature D)

**Status:** approved design (pre-implementation)
**Date:** 2026-06-29

## Goal

An opt-in `--lint-secrets` scan that flags credentials stored **in the clear** —
ConfigMap values and pod env `value:` literals — which anyone with `get` on those
objects can read (chaos test gap #10). It reports **location + pattern type only,
never the value**.

A credential inside a Secret is *correctly placed* and is not flagged; the leak
is a credential sitting somewhere a Secret should have been used.

## Decisions (from brainstorming)

- **Scan targets:** ConfigMap data and pod (init + regular) container env `value:`
  literals. Secrets are NOT scanned (a credential there is the right place).
- **Egress:** credential warnings are **local-only** — shown in text/JSON, **never**
  sent to the `--explain` model. (Values are never recorded regardless.)
- **Opt-in:** a `--lint-secrets` flag, default off. With it off, kubeagent does not
  list ConfigMaps, does not scan, and prints no section — output is identical to
  today.
- **Report API:** `report.PrintInventory` gains one `credentialWarnings`
  parameter (consistent with how summary/facts/serviceIssues were added). The
  `report.Input` struct refactor remains a separate, deferred cleanup.

## Invariants preserved

- **READ-ONLY:** one new List call (ConfigMaps), only when `--lint-secrets` is set.
  Never mutates the cluster.
- **No new Go module dependency.** Detection uses stdlib `regexp`/`strings`.
- **Sequential**, stdlib `flag`. Exit codes unchanged.
- **Namespace scope:** ConfigMaps are namespaced; the lint honors the scan's `-n`.
- **SECURITY — the cardinal rule:** a `credlint.Finding` has **no value field**.
  `Scan` never stores a matched value; `report` renders only
  namespace/name/kind/location/pattern; nothing about credential findings is ever
  passed to `--explain`. There is an explicit test that no finding field contains
  the secret value.

## Architecture

```text
collect (+ConfigMaps in scope, only when --lint-secrets)
      → credlint.Scan(configMaps, inputs.Pods)  ← new pure step (value-free findings)
      → report (text "Credential warnings" section + JSON)   [NOT explain]
```

## Component 1 — `internal/credlint` (pure)

```go
// Finding is one credential-in-the-clear warning. It deliberately carries NO
// value — only where the credential lives and what pattern matched.
type Finding struct {
    Namespace string `json:"namespace"`
    Name      string `json:"name"`     // ConfigMap or Pod name
    Kind      string `json:"kind"`     // "ConfigMap" | "Pod"
    Location  string `json:"location"` // ConfigMap data key, or "container/ENV_NAME"
    Pattern   string `json:"pattern"`  // what matched (never the value)
}

// Scan flags credential-shaped values in ConfigMap data and pod container env
// literals. Result sorted by (Namespace, Name, Location). It never records a
// matched value.
func Scan(configMaps []corev1.ConfigMap, pods []corev1.Pod) []Finding
```

Detection — a shared `classify(name, value string) (pattern string, ok bool)`
applied to each (key, value) in ConfigMap `.Data` and each literal pod env var:

1. **Value patterns** (regexp on the value; the *pattern name* is reported, the
   match is discarded):
   - AWS access key id — `AKIA[0-9A-Z]{16}` → "AWS access key".
   - Private key block — contains `-----BEGIN ` and `PRIVATE KEY-----` → "private key".
   - GitHub token — `ghp_[0-9A-Za-z]{36}` or `github_pat_[0-9A-Za-z_]{22,}` → "GitHub token".
   - JWT — `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+` → "JWT".
2. **Credential-like name with a literal value** — the name matches
   `(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`
   AND `looksLikeLiteralSecret(value)` → "credential-like name with a literal value".

`looksLikeLiteralSecret(value)` returns false (skips) for values that are empty,
purely numeric, `"true"`/`"false"` (case-insensitive), or a shell/template
reference (`$(…)`, `${…}`, starts with `$`). Otherwise true. (Value patterns in
rule 1 always fire regardless of name.)

Sources:
- **ConfigMaps:** each entry of `cm.Data` (skip `BinaryData` for v1). Location =
  the data key.
- **Pods:** for each container in `Spec.InitContainers` and `Spec.Containers`,
  each `Env` entry that has a non-empty `Value` and no `ValueFrom` (literals only).
  Location = `"<container>/<envName>"`.

A given (key/env, value) yields at most one finding (first matching rule wins:
value patterns before the name heuristic).

## Component 2 — collection (`internal/collect`)

```go
func ConfigMaps(ctx context.Context, client kubernetes.Interface, namespace string) ([]corev1.ConfigMap, error)
```

`client.CoreV1().ConfigMaps(namespace).List(...)`, wraps error, scoped to the
scan's `namespace`. Called only when `--lint-secrets` is set.

## Component 3 — surfacing (text + JSON; never explain)

### text (`internal/report`)

`PrintInventory` gains `credentialWarnings []credlint.Finding` (after
`serviceIssues`, before `explanation`). A section prints after the Service-issues
section, only when non-empty:

```text
Credential warnings (--lint-secrets):
  ⚠ default/app-config  ConfigMap[DB_PASSWORD]  credential-like name with a literal value
  ⚠ default/web  Pod[web/AWS_SECRET_ACCESS_KEY]  AWS access key
```

Line format: `  ⚠ <ns>/<name>  <Kind>[<Location>]  <Pattern>`. No value.

### json (`internal/report`)

`inventoryReport` gains `CredentialWarnings []credlint.Finding`
(`json:"credentialWarnings,omitempty"`).

### explain

Unchanged. Credential findings are never passed to `ExplainInventory` /
`buildInventoryPrompt`.

The all-clear "No issues found. ✅" line must also account for credential
warnings: it prints only when the cluster is Healthy AND there are no workloads
AND no service issues AND no credential warnings — otherwise the report would
claim "No issues found" directly above a credential-warnings section. So the
existing all-clear condition gains `&& len(credentialWarnings) == 0`.

## Component 4 — wiring (`main.go`)

Add `--lint-secrets` (`flag.Bool`, default false). After the existing collect/
assemble steps:

```go
var credWarnings []credlint.Finding
if *lintSecrets {
	cms, _ := collect.ConfigMaps(context.Background(), client, namespace)
	credWarnings = credlint.Scan(cms, inputs.Pods)
}
```

Pass `credWarnings` to `report.PrintInventory` (the explain call is unchanged).

## Testing (TDD)

- `credlint.Scan` — table tests: AWS key / private key / GitHub token / JWT in a
  ConfigMap value; credential-like key name + literal value; a pod env literal
  flagged with `container/ENV` location; `valueFrom` env skipped; numeric /
  `true` / `${VAR}` / empty values skipped; sort order; **a dedicated test that
  asserts no `Finding` field contains the secret value** (scan a known value, then
  check every finding's fields do not contain it).
- `collect.ConfigMaps` — fake clientset; namespace scoping.
- `report` — the Credential-warnings section renders when present and is absent
  when empty; JSON carries `credentialWarnings`; existing all-clear/section tests
  unaffected (pass `nil`).
- `main` — `--lint-secrets` parses; with it off, no ConfigMaps are listed and no
  section appears (assert via a fake clientset that `ConfigMaps` was not called, or
  that the output has no credential section).
- Egress: confirm `ExplainInventory`/`buildInventoryPrompt` are not given
  credential findings (no signature change; nothing to leak).

## Out of scope (explicit non-goals)

- Scanning Secrets (credentials there are correctly placed).
- High-entropy/statistical detection (false-positive prone) — only known patterns
  + the credential-like-name heuristic.
- Reporting or storing any secret value, ever; sending credential findings to the
  model.
- ConfigMap `BinaryData`, Helm-release secrets, or external secret stores.
- The `report.Input` struct refactor (separate deferred cleanup).
