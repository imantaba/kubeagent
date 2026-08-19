# JSON schema contract

kubeagent writes eight machine-readable JSON documents. Each one now declares a
`schemaVersion`, and each has a published [JSON Schema](https://json-schema.org/)
generated straight from the Go types that produce it. This page is the
contract: what is versioned, what a version number promises, how to pin to
one, and what is deliberately left unversioned because promising it would be
a lie.

## What is versioned

Six independent surfaces, eight documents:

| Document | Surface | Emitted by |
|---|---|---|
| `scan` | scan | `kubeagent scan --output json` |
| `gate` | gate | `kubeagent gate --output json` |
| `rbac-print` | rbac | `kubeagent rbac print --output json` |
| `rbac-check` | rbac | `kubeagent rbac check --output json` |
| `watch-issues` | watch | the watch daemon's `GET /issues` |
| `watch-explanations` | watch | the watch daemon's `GET /explanations` |
| `baseline` | baseline | `kubeagent baseline capture` |
| `fleet` | fleet | `kubeagent fleet --output json` |

`rbac-print` and `rbac-check` share the `rbac` surface's version: a consumer
that scripts one usually scripts both, so they move together.

### Deliberately out of scope

Not everything kubeagent writes carries a `schemaVersion`, and each omission
is a choice, not an oversight:

- **SARIF** (`kubeagent gate --output sarif`) — SARIF 2.1.0 is versioned by
  [OASIS](https://json.schemastore.org/sarif-2.1.0.json). Wrapping someone
  else's standard in kubeagent's own version number would misattribute it.
- **The Slack, Alertmanager and PagerDuty alert payloads** (`watch
  --alert-format slack|alertmanager|pagerduty`) — these are the receiver's
  shapes, not kubeagent's. Slack's incoming-webhook body, Alertmanager's
  `POST /api/v2/alerts` array and PagerDuty's Events API v2 event are
  contracts Slack, Prometheus and PagerDuty own; kubeagent only fills them in.
- **The `--fix` audit journal** (`--audit-log`) — a write-side record of
  remediation actions taken, appended to a file the operator names. It is
  evidence of what happened, not a read surface a script polls for shape.
- **`/metrics`** — Prometheus text exposition format, already versioned by
  Prometheus itself.
- **`/healthz` and `/readyz`** — plain text, not JSON.

## What MINOR and MAJOR mean

Every `schemaVersion` is `MAJOR.MINOR`. Every surface starts at `1.0`;
`gate` is at `1.1` today, having gained one optional field; `scan` is at
`1.8`, having gained nine; `fleet` is at `1.2`, having gained two
optional `name` properties — and **the schema version is not the kubeagent
release version** — a surface's number moves only when its own document's
shape moves, so a new `scan` field does not disturb a script reading the
`gate` document.

- **MINOR** — adds an *optional* field, or adds an enum value. A parser
  written against `1.0` still works against `1.8`: it just won't know about
  anything past `1.0`. The new field's own type may have required fields of
  its own — no document validated against `1.0` could ever reach them, since
  `1.0` never mentions that type at all.
- **MAJOR** — removes or renames a field, changes a field's type, makes an
  always-present field optional, makes an optional field always-present, or
  removes an enum value. Anything a `1.0` parser would choke on or silently
  misread.

## How to pin

Compare only the major component, and treat an unknown minor as compatible:

```bash
major=$(kubeagent scan --output json | jq -r '.schemaVersion | split(".")[0]')
[ "$major" = "1" ] || { echo "unsupported scan schema"; exit 1; }
```

## What is not promised

A `schemaVersion` promise is narrower than it might look. Not covered:

- **Property order.** Generated schemas render properties alphabetically;
  that is an artifact of how they're generated, not a contract.
- **Array element order** — unless a specific list is documented elsewhere as
  sorted.
- **The exact wording of any human-readable string.** `reason`, `summary`,
  `evidence`, and `explanation` fields are prose written for an operator to
  read, not data for a script to match on. They are free to be reworded in a
  MINOR release, or even a patch.
- **Anything under `explanation` or `investigation`.** That's model output.
  It has a shape, but its content is not deterministic and never will be.
- **Integer and float width.** `int`, `int8`…`int64`, and every `uint*` all
  render as `{"type":"integer"}`; `float32` and `float64` both render as
  `{"type":"number"}`. JSON Schema has no native width of its own, so
  widening or narrowing one of these — `int` to `int64`, say — produces no
  diff: it's invisible to the schema and to the drift test alike.

Matching on a `reason` string will break the day the wording changes. The
stable thing to match on is `issue`.

## A known casing inconsistency

`scan`'s `blindSpots` array and `gate`'s `inconclusive` array both hold the
same fact — a collector call that failed — but their entries are cased
differently. `scan`'s `scan.ReadFailure` has no JSON tags, so it publishes
`Resource` and `Reason`, capitalized; `gate`'s `gate.Blindspot` is tagged, so
it publishes `resource` and `reason`, lowercase, alongside `waived`. Both are
`required` in their respective schemas, exactly as they ship. Aligning the
two would rename a required property in one of them — a **MAJOR** change by
the drift classifier — so it is documented here rather than fixed, and is
parked as a candidate for the next MAJOR bump `scan` takes for an unrelated
reason. A consumer reading both documents must special-case the two keys
until then.

## The published schemas

Each surface's schema is published at a URL that carries only the **major**
version:

```text
https://k8sproject.top/schemas/scan-v1.json
https://k8sproject.top/schemas/gate-v1.json
https://k8sproject.top/schemas/rbac-print-v1.json
https://k8sproject.top/schemas/rbac-check-v1.json
https://k8sproject.top/schemas/watch-issues-v1.json
https://k8sproject.top/schemas/watch-explanations-v1.json
https://k8sproject.top/schemas/baseline-v1.json
https://k8sproject.top/schemas/fleet-v1.json
```

A MINOR bump edits the file at the same URL in place — a pinned URL never
moves for an additive change. A MAJOR bump publishes a new file beside the
old one (`scan-v2.json` alongside `scan-v1.json`); the old file stays put,
because a document already wired into someone's CI pipeline does not stop
existing just because kubeagent moved on.

## Validating a captured document offline

Any generally available JSON Schema validator works. For example, with
[`check-jsonschema`](https://check-jsonschema.readthedocs.io/), which accepts
`--schemafile` as either a local path or a URL:

```bash
pip install check-jsonschema

kubeagent scan --output json > captured.json

# local: the schema committed in this repo, or printed fresh by the binary —
# `kubeagent schema scan` writes the same bytes as the committed file
# (TestSchemaDrift in internal/schemadoc enforces that), so either works
# without a network call:
check-jsonschema --schemafile website/docs/schemas/scan-v1.json captured.json
kubeagent schema scan > scan-v1.json
check-jsonschema --schemafile scan-v1.json captured.json

# remote: the published URL, once this site has deployed the schema at it:
check-jsonschema --schemafile https://k8sproject.top/schemas/scan-v1.json captured.json
```

`additionalProperties` is deliberately left unset in every published schema
(JSON Schema's default: permitted). That is what makes the MINOR-bump
promise mechanical rather than aspirational: a document from a *newer* `1.x`
kubeagent, carrying a field `1.0`'s schema has never heard of, still
validates clean against the `1.0` schema — the extra field is simply
ignored, not rejected.

## The `kubeagent schema` command

```bash
kubeagent schema              # list the documents, their surfaces and versions
kubeagent schema scan         # print one to stdout
```

`kubeagent schema` needs no cluster and no kubeconfig — it doesn't connect to
anything. What it prints is generated at run time from the running binary's
own Go types, the same code path that wrote the committed files under
`website/docs/schemas/`, so there is no embedded copy that can drift from
what the binary actually emits.

## The `rbac` shape change

`kubeagent rbac print --output json` and `kubeagent rbac check --output
json` used to write a bare JSON array. As of this release, both write an
object instead — **this is a breaking change**, made deliberately before a
1.0 kubeagent release: an array has no place to put a `schemaVersion`, so an
unversioned array root could never gain a version later without exactly this
break.

**Before:**

```json
[
  { "apiGroup": "", "resources": ["pods"], "verbs": ["get", "list"] }
]
```

**After:**

```json
{
  "schemaVersion": "1.0",
  "roleName": "kubeagent",
  "rules": [
    { "apiGroup": "", "resources": ["pods"], "verbs": ["get", "list"] }
  ]
}
```

`rbac check --output json` moved the same way: the array of feature statuses
now lives under `features`, beside its own `schemaVersion`.

Anything that scripted the old array shape can recover it with one line:

```bash
kubeagent rbac print --output json | jq '.rules'      # the pre-0.71 array
kubeagent rbac check --output json | jq '.features'   # likewise
```
