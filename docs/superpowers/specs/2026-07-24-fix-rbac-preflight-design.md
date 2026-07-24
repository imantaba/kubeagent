# `--fix` RBAC preflight (design)

**Status:** approved · **Date:** 2026-07-24 · **Type:** write-path hardening (Theme D,
trustworthy remediation — slice 3: RBAC preflight)

## Problem

`--fix` today discovers a permission problem the hard way: it runs the whole preview,
takes the operator's `y`, calls `Update`, and only then gets a `403 Forbidden` from the
API server — a confusing mid-apply failure that looks like an error, not a "you don't
have rights to do this." A production remediation contract should confirm the operator
can actually perform the write **before** attempting it, and refuse cleanly with a
plain-language reason.

This slice adds a `SelfSubjectAccessReview` (SSAR) preflight to each guarded write:
before `Apply` mutates anything, it asks the API server "can *I* update this
deployment / node?" and refuses up front if not.

## Scope decisions (locked)

| Decision | Choice |
|----------|--------|
| Placement | The SSAR **gate runs inside `remediate.Apply`** (single enforcement point — nothing writes without it), via an exported `remediate.Preflight` that the dry-run path also calls |
| Denial disposition | A **new `preflight` audit disposition** (distinct from drift `refused` and mid-write `error`) |
| Dry-run + errors | Under `--dry-run` the SSAR **runs read-only and reports** would-I-be-allowed; an SSAR **API failure fails closed** → `error` disposition, no write |

## Architecture

`remediate` gains the preflight function and calls it as the last gate before each
write. `Result` gains a `PreflightDenied` flag. `main.go`/`runFixes` renders the new
disposition and, on the dry-run path, calls `Preflight` to report. `internal/audit` is
unchanged — it simply carries the new disposition string.

### 1. `remediate` — the preflight gate

```go
// Preflight asks the API server whether the current credentials may perform the write
// this Action implies (verb=update on its resource/namespace/name), via a
// SelfSubjectAccessReview. Returns (allowed, humanReason, err):
//   - err != nil        → the SSAR call itself failed (treat as fail-closed: do not write)
//   - allowed == true   → the write is permitted
//   - allowed == false  → not permitted; reason is a plain-language explanation
func Preflight(ctx context.Context, client kubernetes.Interface, a Action) (bool, string, error)
```

Attribute mapping (verb is always `update`):

| Action.Kind | group | resource | namespace | name |
|-------------|-------|----------|-----------|------|
| RolloutUndo | `apps` | `deployments` | `a.Namespace` | `a.Name` |
| Uncordon | `` (core) | `nodes` | `` (cluster-scoped) | `a.Name` |

Implementation:

```go
attrs := authorizationv1.ResourceAttributes{Verb: "update", Group: group, Resource: resource, Namespace: ns, Name: a.Name}
ssar := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &attrs}}
resp, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
if err != nil { return false, "", err }
if resp.Status.Allowed { return true, "", nil }
return false, fmt.Sprintf("you lack permission to update %s in namespace %q (RBAC)", resource, ns), nil
// nodes (cluster-scoped): "you lack permission to update nodes (RBAC)"
```

`Result` gains:

```go
	PreflightDenied bool // the RBAC preflight refused this action; Applied false, Err nil, no write
```

Gate placement in `applyRolloutUndo` / `applyUncordon` — **after** the state
preconditions (drift bond / no-target / uncordon precondition) and **immediately
before** the `Update`:

```go
	allowed, reason, err := Preflight(ctx, client, a)
	if err != nil {
		res.Err = fmt.Errorf("permission preflight failed: %w", err) // fail closed, no write
		return res
	}
	if !allowed {
		res.PreflightDenied = true
		res.Detail = reason + "; no write attempted"
		return res
	}
	// ... existing Update ...
```

Rationale for ordering: if the state already drifted, the action is moot regardless of
permission, so the more informative drift `refused` wins; permission is the final gate
before the write.

### 2. `main.go` / `runFixes`

- **Real apply path** — add the `PreflightDenied` case (Err-first ordering preserved):

```go
	switch {
	case res.Err != nil:
		fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		logAudit(a, "error", res.Err.Error())
	case res.Applied:
		fmt.Fprintf(w, "  applied: %s\n", res.Detail)
		logAudit(a, "applied", res.Detail)
	case res.PreflightDenied:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit(a, "preflight", res.Detail)
	default:
		fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		logAudit(a, "refused", res.Detail)
	}
```

- **Dry-run path** — call `Preflight` and report (still never writes; disposition stays
  `dry-run`, the permission finding goes in the audit `detail`):

```go
	if dryRun {
		allowed, reason, err := remediate.Preflight(ctx, client, a)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  (dry-run: not applied; permission check errored: %v)\n", err)
			logAudit(a, "dry-run", "permission check errored: "+err.Error())
		case allowed:
			fmt.Fprintln(w, "  (dry-run: not applied; you have permission to apply this)")
			logAudit(a, "dry-run", "permission: allowed")
		default:
			fmt.Fprintf(w, "  (dry-run: not applied; would be blocked — %s)\n", reason)
			logAudit(a, "dry-run", reason)
		}
		continue
	}
```

`runFixes` already receives the `client` — no signature change.

### 3. `internal/audit` — unchanged

`Record.Disposition` is a free string; the new `preflight` value needs no code change.
The documented vocabulary becomes `dry-run | declined | applied | refused | preflight |
error`.

## RBAC / chart — no change

`SelfSubjectAccessReview` is **self-review**: Kubernetes grants `create
selfsubjectaccessreviews` (authorization.k8s.io) to **all authenticated users** through
the built-in `system:basic-user` ClusterRole (bound to `system:authenticated`). `--fix`
runs with the **operator's own kubeconfig** — the in-cluster daemon never fixes — so no
ClusterRole grant is required and the Helm chart's ClusterRole is untouched → **chart
PATCH**. If an operator's principal somehow lacks even self-review, the SSAR `Create`
errors → the fail-closed `error` disposition (no write), which is the correct outcome.

## Global constraints

- **Writes only get stricter.** The preflight can only *prevent* writes (deny or
  SSAR-error → no write); it never enables a write that wasn't already going to happen.
  All prior guard rails (allowlist, protected namespaces, `[y/N]`, `--dry-run`/`--yes`,
  slice-1 drift bond, slice-2 audit) are unchanged.
- **Fail closed.** An SSAR API error means "could not confirm permission" → do not
  write; surface as `error`.
- **No secrets in output/audit.** The preflight reason names only verb/resource/
  namespace — never specs, env, or secrets.
- **No new dependency** (authorizationv1 + client-go are already in the module). **No
  RBAC/Helm change** (chart PATCH). **Golden snapshot unchanged** (no report change).
- **No `Co-Authored-By: Claude` trailer.** **TDD.** gofmt-clean.

## Out of scope (YAGNI)

`SubjectAccessReview` for arbitrary users/impersonation; caching SSAR results; a
`--skip-preflight` escape hatch; preflighting reads or the `get`/`list` calls Apply
already makes; `SelfSubjectRulesReview`; non-`update` verbs.

## Testing

- **`Preflight` (fake clientset + a `create`/`selfsubjectaccessreviews` reactor):**
  - RolloutUndo builds `ResourceAttributes{update, apps, deployments, ns, name}`;
    Uncordon builds `{update, "", nodes, "", name}` — assert by capturing the created
    SSAR in the reactor.
  - reactor returns `Allowed:true` → `(true, "", nil)`; `Allowed:false` → `(false,
    reason, nil)` with the reason naming the resource+namespace; reactor returns an
    error → `(false, "", err)`.
- **`Apply` integration (fake clientset):**
  - denied SSAR → `PreflightDenied` true, `Applied` false, `Err` nil, and **zero
    update calls** (assert via `cli.Actions()`); drift/no-target still short-circuit
    *before* preflight (a denied SSAR on an already-drifted action still reports the
    drift `refused`, not `preflight`).
  - SSAR error → `res.Err` set, no write.
  - allowed SSAR → applies exactly as before.
- **Existing "successful apply" tests must grant permission.** With the gate in `Apply`,
  the fake clientset's default SSAR returns `Allowed:false`, so every test that expects
  a write now needs an allow-SSAR reactor. Add a test helper (e.g. `allowFix(cli)` that
  `PrependReactor("create","selfsubjectaccessreviews", …Allowed:true)`) and apply it to:
  `TestApply_RollsBackToPreviousTemplate`, `TestApply_MatchingPreviewApplies`,
  `TestApply_Uncordon`, and (in `main_test.go`) `TestRunFixes_YesApplies`,
  `TestRunFixes_UncordonYesApplies`, `TestRunFixes_AuditRecordsApplied`,
  `TestRunFixes_AuditRecordsError`. (Refusal/drift/no-target/precondition tests
  short-circuit before preflight and need no reactor.)
- **`runFixes`:** real apply with a denied reactor → one `preflight` audit record and a
  `skipped:` line; dry-run with allowed / denied / erroring reactors → the three
  reported dry-run lines and dry-run audit records carrying the permission finding.
- **Live gate:** full chaos suite — the fix scenarios run under a normal kubeconfig
  (self-review allowed) and apply as before; the preflight adds one read per action.

## Release

- **Gate:** touches the `--fix` write path (Apply gate, `Result.PreflightDenied`,
  runFixes) → **FULL CHAOS GATE** (`./chaos/run.sh --recreate`).
- **Version:** minor **v0.52.0 → v0.53.0**.
- **Chart:** **PATCH** — no RBAC/Helm/template change (SSAR is self-review, allowed for
  all authenticated users).

## Files touched

- **Modify:** `internal/remediate/remediate.go` (+ test) — `Preflight`, the attribute
  mapping, `Result.PreflightDenied`, the gate in both `apply*` funcs, an `allowFix`
  test helper.
- **Modify:** `main.go` (+ `main_test.go`) — the `preflight` disposition case and the
  dry-run permission report; update the existing successful-apply tests with the
  allow-SSAR reactor.
- **Docs:** `website/docs/features/remediation.md`, `README.md`, `CHANGELOG.md`,
  `website/docs/roadmap.md` (Theme-D slice-3 shipped bullet).
