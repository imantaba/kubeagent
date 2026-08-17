# Changelog

All notable changes to kubeagent are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`scan` now discloses two things it used to skip silently, both under
  admission-webhook health.** A `clientConfig.url` backend on a `Fail`-policy
  webhook is not a `Service` kubeagent can check the endpoints of; `scan` now
  counts these and, when the count is non-zero, prints one NOTES line —
  `"N Fail-policy admission webhook(s) not checked: clientConfig.url
  backend"` — naming that they exist without guessing at their health. No
  `Issue` is added and no gauge moves; the count is carried on `scan.Result`
  and `report.Input` as untagged fields, so nothing about the published JSON
  changes and no `schemaVersion` moves. Separately, `-n <namespace>` already
  skipped the admission-webhook check entirely (for every namespace,
  including `kube-system`) without saying so; the `-n` header note now
  carries a second sentence — "cluster-wide checks skipped under -n:
  admission webhooks" — alongside the existing system-rollup caveat, joined
  by `\n` and rendered as its own line.

- **`kubeagent mcp`'s tool results now carry the classified log cause of a
  crashed container.** A finding that `scan --logs` enriched already carried
  its plain-language cause into the text and JSON reports, but the MCP view
  dropped it, so a client that flagged a `CrashLoopBackOff` could not see what
  the previous container's own output said about the exit. `logCause` is now a
  field on the finding the MCP server returns, `omitempty`, carrying the same
  derived one-line label — never the raw log text, which stays out of the tool
  result entirely. An MCP tool result is not one of kubeagent's versioned JSON
  documents, so no `schemaVersion` moves.

### Fixed

- **A webhook backed by an `ExternalName` Service was reported as having no
  ready endpoints.** `ExternalName` is a DNS CNAME and never carries an
  `EndpointSlice`, the same reasoning `internal/svchealth` already applies to
  its own check; `internal/webhookhealth.Assess` now mirrors that guard, so a
  found `ExternalName` backend produces no issue and does not suppress a
  co-occurring high-timeout finding. A backend that is missing outright is
  still reported, whatever its intended type.

- **A webhook with an empty `rules` list was flagged as if it intercepted
  requests.** A `Validating`/`MutatingWebhook` with no rules intercepts
  nothing, so a down or slow backend behind it can never have rejected or
  delayed a real request; `Assess` now skips it before either check. The
  package doc's "cluster-wide" claim is also narrowed: blast radius depends
  on rules and selectors this package does not read, so a flagged webhook's
  effect may be the whole cluster or a single namespace.

- **A webhook with both a down backend and a high `timeoutSeconds` reported
  only the backend problem, with the timeout silently dropped.** The
  precedence itself was correct — a dead backend dominates a report about
  latency — but the co-occurring timeout was invisible until the backend came
  back up. The backend issue's reason string now gains a suffix, `" (and
  timeoutSeconds %d ≥ %ds)"`, when the same webhook also clears the latency
  threshold. No `Issue` is added or removed; row count and both gauges are
  unchanged.

- **A pod sharing all three host namespaces was reported as sharing "the host
  network/PID/IPC namespace" — singular.** `internal/secscan`'s
  `HostNamespaces` finding now pluralizes the trailing noun on the count of
  shared namespaces: `"pod shares the host PID namespace"` for one,
  `"pod shares the host network/PID/IPC namespaces"` for more than one. The
  join order, the one-finding-per-pod shape, and the `HostNamespaces` check
  name are unchanged; `detail` carries no `enum` in the published scan
  schema, so this is a content change, not a shape change.

- **A `hostPath` volume mounted read-only everywhere was still reported as
  "writable host filesystem".** `internal/secscan`'s `HostPath` finding now
  walks the mounting containers' `VolumeMounts` for that volume name: when
  every mount sets `readOnly: true` the detail reads "(read-only host
  filesystem)"; a volume mounted read-only by one container and writable by
  another is writable — the union is the answer — and a `hostPath` volume no
  container mounts also renders "writable", the safe default when nothing
  constrains it. The finding still fires for every `hostPath` volume
  regardless of `readOnly` — PSS `baseline` forbids the volume, not the
  write — so this only changes the parenthetical, never whether the finding
  fires. No `schemaVersion` moves: `detail` is a free-form string.

- **`externalIPs` on an `ExternalName` Service was reported as an exposure it
  cannot cause.** `ExternalName` is a DNS CNAME, not a proxied Service — it
  carries no NodePort or LoadBalancer ingress, and the API server itself
  warns `spec.externalIPs is ignored when spec.type is "ExternalName"` on
  admission. `exposedService` now returns early for
  `ServiceTypeExternalName`, before the `LoadBalancer`/`NodePort` switch and
  before the `externalIPs` arm. `LoadBalancer`, `NodePort` and
  `externalIPs`-on-a-`ClusterIP` keep firing exactly as before; the tier
  summary's workload and finding counts drop by one only on a fixture that
  had an `ExternalName` false positive. No `schemaVersion` moves: `detail` is a
  free-form string and no `Finding` field changes shape.

- **A comment claimed `servicePorts`' empty-ports branch was unreachable
  against an API-server-validated cluster.** It is reachable: a headless
  Service (`clusterIP: None`) is exempt from the API server's ports-required
  rule, so a portless headless `ClusterIP` with `externalIPs` set is a valid
  object, and `exposedService`'s `externalIPs` arm brings it to
  `servicePorts`. The comment now says which shape reaches the branch and
  which shapes the API server refuses; `"no ports"` was always the honest
  answer for that Service and its behaviour is unchanged. A new test pins the
  case. No `schemaVersion` moves: no field changes shape and `detail` is a
  free-form string.

- **The four control-plane static pods in `kube-system` merged into one
  anonymous `Node` block in the SECURITY section.** A static (mirror) pod's
  controller owner is its `Node`, and `resolveWorkload` fell through to that
  owner's `Kind`/`Name` — so `etcd`, `kube-apiserver`, `kube-scheduler` and
  `kube-controller-manager` all attributed to the same node-named row.
  `resolveWorkload` now treats a `Node` controller owner the same as no
  owner at all: the pod attributes to itself. Static pod names are already
  qualified per component and per node by Kubernetes (`etcd-<node>`,
  `kube-apiserver-<node>`, ...), so the four components now separate into
  four attributable rows instead of one. No finding is added, removed or
  reworded — only the `kind` and `workload` fields on mirror-pod findings,
  and therefore the grouping. Every other owner kind is unaffected, and this
  is visible only under `-n kube-system`: a cluster-wide scan already drops
  system pods before `Assess` sees them. The workload column already named
  the node — a static pod's own name usually ends in it — so this changes
  which node-derived string is printed, not whether one is. No
  `schemaVersion` moves: `kind` carries no `enum` in the published scan
  schema.

- **A CronJob's pods churned through the SECURITY section as a new,
  unattributed `Job` row every tick.** A CronJob's Job children carry a
  ticked name (`report-29780988`, then `report-29780989`, ...), and
  `resolveWorkload` used to leave a Job-owned pod attributed to that Job
  directly, so each tick's findings looked like a new, unrelated workload.
  `secscan.Assess` now takes a fourth argument, `jobs []batchv1.Job`, and
  `resolveWorkload` folds a pod whose controller owner is a Job present in
  that slice, and whose Job's own controller owner is a CronJob, up to
  `CronJob/<name>` — mirroring the existing ReplicaSet-to-Deployment fold. A
  bare Job (or one absent from the slice) still resolves to `Job/<name>`:
  its name is stable and is the object the operator created, so there is
  nothing above it to fold to. No new cluster read and no RBAC change —
  Jobs are already collected unconditionally and carried on `scan`'s
  inventory inputs regardless of `--security`; the sole production call
  site now passes `inputs.Jobs` alongside the already-unfiltered
  `inputs.ReplicaSets`. No `schemaVersion` moves: `kind` and `workload` are
  free-form strings.

- **The `connection refused` log cause no longer repeats the address the
  container dialed.** The cause is now a fixed sentence with no submatch
  interpolated into it. A container that logged a failure to reach a URL
  carrying an embedded credential had that credential copied verbatim into
  `logCause`, a field every rendering of a finding carries — the text report,
  the JSON document, and now an MCP tool result. The raw line still reaches the
  log excerpt, sanitized and truncated, which is where an operator reads it. `logCause` is a free-form string in every published
  schema, so no `schemaVersion` moves.

- **A log body whose only content is the kubelet's own log-unavailable
  placeholder is now refused instead of classified.** `unable to retrieve
  container logs for <runtime>://<id>: …` is text the container runtime wrote,
  never the crashed container's own output, so classifying it reported the
  control plane's message as if it were the workload's behaviour — and a
  placeholder whose tail reads `permission denied` matched the `perm-denied`
  signature outright. Such a body now yields no cause and no excerpt at all. A
  body that mixes the placeholder with genuine output still classifies
  normally, on the genuine line, and an unanchored mid-line mention is left
  alone.

- **The fallback log cause now names the window it searched instead of claiming
  no signature exists.** It reads `last output before exit (no signature in the
  last 25 lines)`. kubeagent asks the API server for only the last 25 lines of
  the previous container, so a signature earlier in that container's output is
  invisible to the classifier rather than absent from the log — the old wording
  asserted the stronger claim. The 25 is now named by a comment at each of the
  two sites that depend on it, each pointing at the other, so the tail length
  and the sentence describing it cannot drift apart silently. `logCause` is a
  free-form string in every published schema, so no `schemaVersion` moves.

- **`scan --logs` no longer issues a previous-container log read that cannot
  succeed.** It probed any finding that named a container, including one still
  on its first attempt, which has no previous instance — a guaranteed `400`
  from the API server on every scan of a pod that had not yet restarted. The
  probe is now gated on the container having exited at least once (a restart,
  or a still-visible last termination), read from the pod objects the scan had
  already collected, so the gate costs a map build rather than a new cluster
  read. Both `ContainerStatuses` and `InitContainerStatuses` are searched,
  because an init-container finding names its container from the init slice
  that no other detector reads.

- **When two findings name the same container, the log block now goes to the
  finding that explains the exit.** A container that trips both
  `CrashLoopBackOff` and `OOMKilled` produced two findings and one log fetch,
  and the block landed on whichever finding reached the enrichment loop first.
  If a later finding's issue matches the container's own last termination
  reason, the block now moves to it. The fetch count is unchanged — still one
  per container — and the reason is read from the same two status slices the
  probe gate searches, for the same reason.

- **A `RolloutStuck` finding no longer carries one confidence level for two
  different kinds of evidence.** A Deployment's stuck rollout is read from a
  controller-set condition — Kubernetes itself asserting the state — while a
  StatefulSet's or DaemonSet's is inferred from comparing revision and ready
  counters across a grace period. `internal/rollouthealth` now sets
  `Confidence` on its own findings — `"high"` for the Deployment arm,
  `"medium"` for the StatefulSet and DaemonSet arms — and
  `confidence.Annotate` no longer stamps every finding unconditionally; it now
  fills `Confidence` only where a producer left it empty, so a producer that
  knows better than the issue string wins. A StatefulSet's or DaemonSet's
  stuck-rollout line in the text report now carries a trailing `[medium]` tag;
  the long-standing Deployment case is unchanged. A new `internal/diagnose`
  table test pins the confidence level for every kind `internal/knownissues`
  documents — all nineteen, the sixteen pod-detector kinds plus the three
  workload-level kinds — so a future kind cannot arrive without someone
  writing down which level it is and why. `confidence` carries no `enum` in
  any published schema, so a producer setting its own value moves no
  `schemaVersion`.

### Changed

- **`KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS` is now validated instead of silently
  clamped.** An unparseable or out-of-range value used to fall back to the
  default of 15 without warning; kubeagent now refuses to run, naming the
  variable, the offending value and the valid range. Valid values are 1–30,
  matching the API server's own cap on a webhook's `timeoutSeconds`.

- **Four finding-vocabulary groups are now CamelCase**, matching every other
  issue kind in the tree: `stuck terminating` → `StuckTerminating`; the PDB
  categories `unsatisfiable` → `PDBUnsatisfiable`, `stale` → `PDBStale`,
  `blocking` → `PDBBlocked`, `singleton` → `PDBSingleton`; the HPA categories
  `unable` → `HPAUnableToScale`, `metrics` → `HPAMetricsFailed`, `capped` →
  `HPACapped`; and the admission-webhook problems `missing-service` →
  `MissingService`, `no-endpoints` → `NoEndpoints`, `high-timeout` →
  `HighTimeout`. This changes the string content of `gate`'s JSON `issue`
  field, the watch daemon's `/issues` endpoint, and the MCP tool result's
  `reason` field — a consumer matching any of the old lowercase or hyphenated
  literals in those three surfaces breaks. No `schemaVersion` moves: none of
  the affected fields carry an `enum` in the published schemas, so this is a
  content change, not a shape change. A `pdbhealth.Issue`/`hpahealth.Issue`'s own
  `category` field, and therefore `scan --output json`'s `pdbIssues[].category`
  and `hpaIssues[].category`, stay lowercase — that field belongs to a typed
  document, not the finding vocabulary. The text renderer's `NoEndpoints:` and
  `HighTimeout:` lines replace the old `WebhookDown:`/`WebhookSlow:` labels it
  used to synthesize; its `PDBBlocked:` and `HPAStuck:` lines were already the
  target spelling and are unchanged.

- **`pdbhealth.Assess` now flags two PDB shapes that used to pass silently, and
  reworks three reason strings.** A PDB with neither `minAvailable` nor
  `maxUnavailable` set used to be skipped by the loop entirely; it is the
  strictest rule a PDB can express — the API server allows no voluntary
  eviction at all — and is now classified `unsatisfiable`. A PDB guarding a
  single-replica workload with no disruption headroom used to be treated as
  benign by falling outside every guard; it is now its own category,
  `singleton`, with a reason that names the trade-off ("often deliberate, but
  no voluntary eviction is possible"). This is separate from the CamelCase
  rename above: that entry covers the finding-vocabulary string `singleton` →
  `PDBSingleton` that the map already carried; this is `Assess` emitting the
  `singleton` category at all, for an input it used to pass over without
  flagging. Three existing reasons are reworded to say only what was measured:
  the `unsatisfiable` reason now names the over-ask count separately from the
  exact-cover count instead of using one string for both; the `blocking`
  reason now quotes the PDB's own status counters ("PDB status reports N/M
  guarded pods healthy") instead of asserting a fact about pod readiness
  kubeagent never measured; the `stale` reason drops its hedged "(stale?)" for
  "selector currently matches no pods", which is what the status actually
  shows. Separately, `scan`'s text-format summary line for these PDBs now
  reads "N PodDisruptionBudget issue(s)" instead of "N PodDisruptionBudget(s)
  blocking drains" — "blocking" was never a property every category had, and
  is less true now that there are two more categories that do not block
  anything until their shape changes. A consumer parsing that summary line for
  "blocking drains" breaks; the JSON `pdbIssues` array, its `category` values,
  and the text report's `PDBBlocked:` row label are unaffected. No
  `schemaVersion` moves: `pdbhealth.Issue`'s `category` and `reason` fields
  carry no `enum` in the published scan schema, so a new category value and
  reworded reason text are content changes, not shape changes.

- **`hpahealth.Assess` now distinguishes a disabled or ambiguous HPA from an
  ordinary metrics failure, and names the observed replica count when one is
  capped.** The `ScalingActive` arm used to collapse every `False` reason into
  category `metrics` with the prefix "can't fetch metrics"; an HPA whose
  scaling the upstream HPA controller reports disabled under `ScalingDisabled`,
  and one whose selector it reports overlapping another HPA's under
  `AmbiguousSelector`, were reported identically to one that simply could not
  read a metric. Those two reasons now classify as `disabled` ("scaling is
  disabled") and `ambiguous` ("two HPAs target the same pods"); every other
  `ScalingActive` reason keeps falling through to `metrics`, unchanged.
  Separately, the `ScalingLimited`/`capped` arm used to always render "pinned
  at maxReplicas N — desired exceeds the cap", even once the HPA's current
  replica count had already fallen back below the cap; it now reads
  `status.currentReplicas` and renders "at N of maxReplicas M — desired
  exceeds the cap" when current is below max, keeping the pinned-at-cap
  sentence when it is not. No new cluster read and no RBAC change:
  `status.currentReplicas` is already on the object `Assess` receives. The
  finding vocabulary grows from three HPA categories to five; `disabled` →
  `HPAScalingDisabled` and `ambiguous` → `HPAAmbiguousSelector` join the three
  duplicated `hpaCategoryToIssue` maps (`internal/findings`, `internal/mcp`,
  the watch daemon's `/issues`), so gate JSON, the MCP tool result and
  `/issues` all render the CamelCase spelling instead of falling back to the
  raw category. No `schemaVersion` moves: `hpahealth.Issue`'s `category` and
  `reason` fields carry no `enum` in the published scan schema, so a new
  category value and a reworded capped-arm reason are content changes, not
  shape changes. Both comparisons that decide the new behavior — the
  `ScalingActive` reason switch and the existing `TooManyReplicas` match — run
  on the raw condition `Reason`, unchanged from before.

- **`gate`'s text output for a cluster-scoped finding no longer carries a
  leading slash before the name.** The identity column built `"Kind
  %s/%s"` unconditionally from `Namespace` and `Name`; a finding with no
  namespace — a `ValidatingWebhookConfiguration`, for example — rendered as
  `"ValidatingWebhookConfiguration /vwc-missing"` instead of
  `"ValidatingWebhookConfiguration vwc-missing"`. `gate --output json` is
  unaffected (`namespace` still encodes as `""`), so no `schemaVersion`
  moves.

- **A webhook finding's name now carries the webhook alongside the
  configuration.** `gate`'s JSON `name`, SARIF's
  `artifactLocation.uri`, and the MCP tool result's `name` used to read the
  configuration name alone, so two Fail-policy webhooks at or above the
  timeout threshold in the same `ValidatingWebhookConfiguration` produced two
  findings indistinguishable from each other — same `kind`, same empty
  `namespace`, same `name`. All three now read `"config/webhook"`, matching
  the watch daemon's `/issues`, which already did. `gate`'s text output
  changes the same way. No `schemaVersion` moves: `name` is a free-form
  string with no `enum` and no pattern in the published schemas.

- **The SECURITY section's restricted aggregate read as if hardening
  coverage were total.** `"restricted (hardening gaps, near-universal): N
  across M workloads"` counted only the restricted-profile workloads in both
  the numerator and the denominator, so a section that also carried clean
  baseline/exposed workloads understated the population the aggregate speaks
  for. The line now reads `"N across M of T workloads"`, where `T` is every
  workload in the SECURITY section; the verbose view, which omits the
  aggregate entirely, is unchanged. No `schemaVersion` moves: this is
  `scan`'s text renderer only, and the aggregate line was never part of the
  JSON output.

## [1.16.1] - 2026-08-13

### Changed

- **The `Unschedulable` reason no longer guesses at a cause.** `PendingDetector`
  fires on `PodScheduled=False`/`Unschedulable` and reads nothing about *why* the
  scheduler refused the pod, but its reason offered three anyway: `No node can
  schedule this pod (resources, taints, or affinity)`. The scheduler's own
  message is already carried as evidence on the next line and names the real
  cause — including causes outside that list, such as an unbound
  PersistentVolumeClaim, where the parenthetical pointed away from the answer
  printed directly beneath it. The reason is now `No node can schedule this
  pod`. The issue kind, the evidence, the confidence and the guard are unchanged,
  so no `schemaVersion` moves, and `kubeagent known-issues Unschedulable` still
  enumerates the causes at length — which is where an enumeration belongs.

### Fixed

- **Four documentation claims narrowed to what the code does.** The root-cause
  section said a PVC-blocked pod has "no pod-level finding of its own"; it has
  one — `Unschedulable`, at high confidence — and the attribution contributes the
  storage *cause*, not the first finding. The attention-line rollup was shown
  only in its single-cause form (`3 ⇐ node worker-2`); the multi-cause form
  (`29 ⇐ 4 root causes`) is now documented beside it. The stale-`Lease` half of
  hard-down attribution was stated unconditionally; it follows
  `--node-heartbeat-threshold`, and `0` removes the check and every attribution
  depending on it, while the `NotReady` half is unaffected. And "Finding
  confidence" listed findings and hints together without saying that a root-cause
  attribution is not a finding, carries no `confidence` field in JSON, and gets
  its text-report `[medium]` from the kind of cause.

## [1.16.0] - 2026-08-12

### Added

- **New detector: `ContainerStartError`.** A main container the kubelet created
  and could not start now carries a finding instead of rendering as a failing
  workload with nothing under it. It fires on a closed set of five kubelet
  waiting reasons — `RunContainerError`, `CreateContainerError`,
  `PreStartHookError`, `PostStartHookError`, `StartError` — and quotes the
  reason and message verbatim as evidence, at `medium` confidence, because it
  reports that the container did not start without claiming to know why. The
  motivating case is a container OOM-killed *during startup*: the kubelet
  reports `RunContainerError` and records the OOM only in
  `lastState.terminated.Reason=StartError`, which `OOMKilled` — matching the
  reason `OOMKilled` — never sees. A brand-new pod gets a minute of grace
  (`containerStartDwell`, skipped once the container has restarted), so an
  ordinary startup is not reported as a failure. The image-family reasons
  (`InvalidImageName`, `ErrImageNeverPull`, `RegistryUnavailable`,
  `SignatureValidationFailed`) are deliberately not in the set: those are pull
  failures. Registered last in `DefaultDetectors`, which is report order only —
  `diagnose.Run` collects every detector's findings, so a catch-all shadows
  nothing. The known-issues vocabulary moves from fifteen kinds to sixteen; the
  issue kind is a string rather than an enum in every published schema, so no
  `schemaVersion` moves.

### Fixed

- **`VolumeMountError` could never fire.** The detector shipped in 1.15.0 reads
  the pod's `FailedMount` Warning events, and the scan pipeline collected only
  `FailedAttachVolume` events, so `facts.Events` never contained one to match. A
  pod stuck on a missing ConfigMap or Secret volume source still carried no
  diagnosis. The scan now lists `FailedMount` events alongside the attach ones.

- **A `KUBEAGENT_QUOTA_THRESHOLD` outside `(0, 1]` is refused out loud.**
  `KUBEAGENT_QUOTA_THRESHOLD=80` — the obvious way to write "warn me at 80%" —
  parsed as a float, failed the range check, and was silently replaced by the
  default, so the scan ran at 0.90 while the operator believed they had changed
  it. All four commands that read the variable (`scan`, `watch`, `gate`, `tui`)
  now write one line to stderr naming the variable, the value received and the
  threshold actually used, and continue at the default. stdout is untouched, so
  a `--output json` consumer sees byte-identical bytes. A set-but-empty value is
  left alone: `os.Getenv` cannot tell it from unset. The five literal `0.90`s
  behind the message collapse into one `scan.DefaultQuotaThreshold`, so the line
  cannot name a threshold the scan did not use.

- **The quota percentage is floored, not rounded up.** A ratio of 0.999
  rendered as `⚠ QuotaNearLimit: used 999m / hard 1 (100%)` — a line that
  reports the same quota as both full and not full. The percentage now truncates,
  so `100%` beside `QuotaExhausted` always means at or over the limit. The JSON
  `ratio` is unchanged and still carries the true value.

- **The quota summary line counts entries, not ResourceQuotas.** `7
  ResourceQuotas near/over quota` was the count of `(quota, resource)` pairs, so
  four ResourceQuota objects with several resources between them overstated how
  many objects were in trouble. The line now names quota entries.

### Changed

- **Documentation narrowed to what the code keeps**, from the diagnostics
  validation campaign: the ResourceQuota section's `exhausted` band is
  `used >= hard` (a quota narrowed below live usage reports over 100%) and its
  `near limit` band starts at the configured threshold rather than a hardcoded
  90%; an entry whose `hard` is zero is documented as skipped, because a zero
  quota is a prohibition rather than a capacity about to run out; both volume
  sections state that the evidence is the newest matching event still inside the
  API server's event TTL and that the guard against a resolved failure is the
  pod's own state rather than the event's age; and `CLAUDE.md`'s paraphrase of
  the MCP promise is narrowed from "path-free" to "free of kubeconfig paths and
  context names", with `mcp.md` gaining a note that a tool result carries API
  text and API text can name a filesystem path the kubelet chose. Nothing is
  deleted: every promise the code keeps is restated where a reader will find it.

## [1.15.0] - 2026-08-11

### Added

- **New detector: `VolumeMountError`.** A pod stuck at container creation
  because a volume cannot be *mounted* now carries a finding. Previously only
  `FailedAttachVolume` was matched (`VolumeAttachError`), so the most common
  mount failure — a ConfigMap or Secret named as a **volume source** that does
  not exist — produced no diagnosis anywhere: the kubelet raises
  `CreateContainerConfigError` only for a reference consumed through
  `env`/`envFrom`, and a volume that cannot be resolved never reaches container
  creation. The detector reads the pod's `FailedMount` Warning events, names the
  missing-object case specifically, and for every other mount failure claims
  only that the mount did not complete. `VolumeAttachDetector` is unchanged. The
  issue kind is a string rather than an enum in every published schema, so no
  `schemaVersion` moves.

- **Seven fuzz targets over the workload and cluster health packages.**
  `batchhealth`, `rollouthealth`, `createhealth`, `pvchealth`, `hpahealth`,
  `termhealth` and `clusterhealth` each gained a target that builds its own
  objects from hostile bytes and asserts that no invalid UTF-8, control
  character or formatting character reaches a finding — plus, per package, that
  the issue kind stays inside its own closed vocabulary and that the entry point
  is deterministic. These are the guard for the sanitize fixes below: a wrap
  removed by a later edit fails a campaign instead of shipping. The nightly
  matrix grows from fourteen pairs to twenty-one, and every seed corpus replays
  on a plain `go test`, so a regression fails a pull request without waiting for
  a campaign. Objects are drawn from the test-only `internal/fuzzgen`: a field
  the API server validates is drawn from the alphabet it enforces, because
  drawing it hostile would assert something false about production.

### Fixed

- **`kubeagent_inspect` returned an event's text unsanitized.** The `type`,
  `reason` and `message` of every event in a tool result were copied straight
  from the API object, and all three are free text the API server does not
  validate. This is the one place in the sweep where the bytes leave the
  operator's own terminal: a tool result is forwarded verbatim to a client that
  renders it however it likes, and JSON encoding is not sanitizing — it escapes
  an ESC byte and passes U+202E RIGHT-TO-LEFT OVERRIDE through unchanged. All
  three now go through `internal/safetext.Line`. Event ordering is unaffected:
  it already ran before this point, on the raw values. `internal/mcp`'s import
  wall is untouched — `safetext` is neither `remediate` nor `explain` — and
  sanitizing a string makes no model call.

- **Three condition messages reached a finding's reason unsanitized.** A
  HorizontalPodAutoscaler's `AbleToScale`/`ScalingActive` message, a Namespace's
  termination condition message, and a Node's `NodeReady` reason *and* message
  were read straight into the reason kubeagent prints. All four are free text
  the API server does not validate, so a control character, an ANSI escape or a
  right-to-left override placed in one of them travelled intact. A reason
  travels further than a terminal: it is carried in `gate`'s verdict JSON and in
  the SARIF a pipeline uploads. Each value now passes through
  `internal/safetext.Line` at the point it first becomes a kubeagent value, per
  the project's sanitize-at-ingress rule. Matching decisions are untouched and
  still run on the raw value — the HPA's `TooManyReplicas` comparison, the
  namespace condition's type, and the node condition's type and status.

  One visible consequence: a multi-line `NodeReady` message used to be cut at
  its first line, and now folds to one line with the breaks turned into spaces,
  which is what every other kubeagent surface does. The row stays bounded at 120
  characters, and the second line of a kubelet message is often where the cause
  is.

- **Five message sites carried the API's text into a finding unsanitized.** A
  failed Job's condition message (`batchhealth`), a
  Deployment's `ReplicaFailure` and `Progressing`/`ProgressDeadlineExceeded`
  condition messages (`rollouthealth`), a PersistentVolumeClaim's
  `ProvisioningFailed`/`FailedBinding` event message (`pvchealth`) and a
  `FailedCreate` event's message (`createhealth`) all reached a finding
  straight from the API object. Each now passes through
  `internal/safetext.Line` where it first becomes a kubeagent value.

  One of the five was also a *reason* site, which the original sweep had
  classified as evidence-only: `batchhealth`'s `humanReason` recognises two Job
  failure reasons by name and returns every other one verbatim, so an
  unrecognised reason was printed as kubeagent's own words. The `switch` is the
  matching decision and still reads the raw value; the default arm — the one
  that echoes — sanitizes.

  Two reasons are deliberately left as they were read, because neither is text
  from the cluster: `pvchealth` admits only the literals `ProvisioningFailed`
  and `FailedBinding`, and `createhealth`'s reason is one of six phrases the
  package writes itself, chosen by a classifier that reads the raw message and
  never echoes it.

- **A HorizontalPodAutoscaler's scale-target reference reached the report
  unsanitized.** An HPA issue names what the autoscaler targets, composed from
  `spec.scaleTargetRef`'s kind and name. Neither is a DNS name: the API server
  checks both with `IsPathSegmentName`, which refuses only `.`, `..`, and a
  value containing `/` or `%` — a control character, invalid UTF-8 and an
  unbounded length all pass it. The composed value is printed by the text
  renderer and carried in `scan`'s JSON document, which is written to a file and
  forwarded. Both halves now pass through `internal/safetext.Line`, separately
  rather than after joining, so a long kind cannot push the name out of one
  line's budget. This site was absent from the original sweep's table, which had
  treated an object reference as validated; the HPA's own namespace and name
  really are DNS-1123 labels and stay as they were read.

### Changed

- **A pod row now names what the pod is doing, the way `kubectl get pods` does.**
  The third column printed `status.phase`, so a pod whose container was in
  `CrashLoopBackOff` read `Running` and one that could not pull its image read
  `Pending` — an operator holding a kubeagent row beside a `kubectl` row saw two
  different words for the same pod and had to work out which tool was wrong.
  Neither was; they answer different questions. `inventory.PodRowFor` now
  computes a display value beside the raw phase — deleting is `Terminating`, a
  failed or waiting init container is `Init:<reason>`, a main container's
  waiting or terminated reason is that reason, and anything outside that rule
  falls through to the phase, which is what every row printed before. The rule
  is deliberately smaller than `kubectl`'s own column logic, which kubeagent has
  no reason to reimplement in full. A container's reasons are API text the API
  server does not validate, so they pass through `internal/safetext.Line` at
  that one site. `scan --output json` gains `state` beside `phase` on a pod row
  and moves to schema version **1.3**; `phase` is untouched and still carries
  one of the five phase words verbatim, so nothing reading it changes. The
  addition is additive — `state` is absent from `required`, and a document
  written without it still validates against the older schema.

- **`scan --output text` caps a finding's evidence line at 500 characters.**
  Nothing bounded the `↳` signal, and a container runtime repeats every layer of
  a failure — the back-off preamble, the rpc error, the unpack failure, the
  resolve failure, the bare image reference — so on a long registry path the
  line ran past the screen and took the alignment of the rows below it with it.
  A cut line now ends in `… (truncated)`, because a silently shortened error
  reads as the whole error. The cut is on characters rather than bytes, so a
  multi-byte character is never split. Real errors measure a few hundred
  characters and still arrive whole; the cap bites only on the pathological.
  `--output json` carries the full string — the cap is a terminal-layout
  decision, and the machine surface is where an operator goes for the complete
  cause. It narrows no claim: evidence quotes what the cluster said, and the
  finding's own reason is untouched.

- **A Deployment's first rollout no longer reports itself as a change.** A
  flagged Deployment still on revision 1 carried `changed: rollout to revision
  1, 4d ago` — but revision 1 is the Deployment's creation, so there was no
  prior state for it to be a change from, and indeed no image delta was ever
  printed beside it. It is now suppressed, which is what makes the line's
  presence meaningful. The gate is the revision number, not the survival of an
  older ReplicaSet: a Deployment at revision 6 whose predecessors have been
  garbage-collected still reports its rollout, without the delta. Suppression
  happens in `rollout.Annotate` rather than in a renderer, so `--output json`
  agrees — the `rollout` key is simply absent, as it already was for workloads
  that are not flagged Deployments and for rollouts older than the seven-day
  window. It is an optional key, so no `schemaVersion` moves.

- **`scan --output text` collapses findings that would print the same lines.**
  A Deployment whose replicas all fail the same way printed one identical pair
  of lines per pod — twenty of them on a twenty-replica Deployment — and the
  text renderer prints no pod name, so there was nothing to tell them apart
  with. They now render as one block with a count: `⚠ CrashLoopBackOff: …
  ×20`. A collapsed block can never print fewer lines than the findings it
  stands for: the key is the whole rendered block bar the `↳` signal line, so a
  `--suggest` command naming the pod or a per-container resources block keeps
  the findings apart, and every distinct signal inside a block is printed on
  its own line — which is what shows the restart counts when they differ. Text
  only: `--output json` still carries one finding per pod, each naming its own
  pod, and no count appears in it.

- **A CronJob whose last run failed no longer reports itself as `Idle`.** The
  status word is computed from the active-job count during assembly, before any
  Job has been judged, so a CronJob with a `JobFailed` finding printed
  `✗ shop/nightly-report  CronJob  Idle` — a true statement (the schedule is
  alive and nothing is running right now) that reads as "nothing to see"
  directly above the finding explaining that the most recent run failed. It now
  says `Last run failed`, set on the one branch that has actually looked at the
  newest owned Job. Not `Failed`, which is a standalone Job's word: a Job that
  failed is failed, a CronJob will fire again on schedule. A CronJob mid-run or
  with a clean newest run keeps the word assembly computed, and a Job's status
  is untouched. `status` is published as a bare string with no enum in every
  document that carries it, so no `schemaVersion` moves.

- **`known-issues RolloutStuck` no longer calls an emitted kind unknown.**
  `RolloutStuck`, `FailedCreate` and `JobFailed` come from `scan`'s workload
  passes rather than from the pod detector set the reference documents, so
  asking about one — a minute after a scan printed it — was answered with
  "unknown issue kind". They now get their own message: the kind is a
  **workload-level finding**, not one of the pod detectors this reference
  covers, and it is explained on the diagnostics page. A typo keeps the
  unknown-kind message verbatim, because that is the correct word for a typo,
  and the exit code is unchanged either way. No entry is added to the
  reference: those three stay undocumented there on purpose.
  `internal/knownissues` gains the three-name list — it imports nothing from
  kubeagent and nothing outside the standard library, and a `[]string` keeps
  both halves true. The list is closed by a new test that reads the `Issue:`
  literals the workload passes build and fails if they are not exactly those
  three, deriving the packages to walk from `scan.go`'s own `Annotate` call
  sites; the four existing closure tests are scoped to `internal/diagnose` and
  could not have caught a fourth.

- **`RolloutStuck` now covers a wedged StatefulSet and a wedged DaemonSet, not
  only a Deployment.** A StatefulSet and a DaemonSet publish no status
  conditions at all, so a scan could see one sitting at `0/3 Degraded` with no
  finding attached and nothing naming the cause. `internal/rollouthealth` gains
  an arm for each, reading the revision and replica counters `scan` already
  collects — a StatefulSet whose `updateRevision` differs from its
  `currentRevision` with `updatedReplicas` short of `spec.replicas`, one whose
  revisions match with `readyReplicas` short, and a DaemonSet whose
  `numberReady` is short of `desiredNumberScheduled`. Because a counter cannot
  distinguish "wedged" from "started ten seconds ago", both arms require the
  controller **and** every not-ready pod it owns to be older than 600 seconds —
  Kubernetes' own default `spec.progressDeadlineSeconds` on a Deployment, so
  the three kinds are equally patient. It is a package constant, not a flag.
  The existing `RolloutStuck` kind is reused, so no consumer filter, SARIF rule
  id or alert dedup key changes, and no `schemaVersion` moves; the reason string
  now names the workload's own kind and renders byte-identically for a
  Deployment. Evidence is only the counters that fired — never a pod, node,
  image or revision name. No new cluster read, no new RBAC.

- **The `RolloutStuck` next step no longer suggests `describe deployment`.** A
  finding carries no kind, and the kind is now one of three, so the suggested
  command was wrong two times in three. It is now
  `kubectl -n <ns> get events --field-selector involvedObject.name=<name>`,
  which is addressable by name alone and correct whichever kind fired. Still
  read-only, still never run by kubeagent.

- **`RolloutStuck` no longer flips on and off across scans of an unchanged
  crash-looping Deployment.** The suppression gate in `internal/rollouthealth`
  tested only whether the workload already carried a finding, which is a single
  momentary sample: a crash-looping container is in `Waiting` only between
  restart attempts, so `CrashLoopBackOff` matched on some scans and not others.
  Forty consecutive scans of one unchanged Deployment reported `RolloutStuck` 32
  times and `CrashLoopBackOff` 8 — and `issue` is what gate verdicts, SARIF
  results, alert dedup keys and the watch daemon's `/issues` are keyed on, so a
  CI gate could pass and fail alternately and an alert could re-fire as "new".
  The gate now also asks whether any pod in the workload has restarted at least
  `diagnose.RestartThreshold` times, a durable fact that holds across the whole
  restart cycle. The threshold is read from where `RestartLoopDetector` defines
  it rather than re-typed. No detector changed, no new cluster read, no new
  RBAC.

- **A `FailedCreate` caused by an admission webhook that could not be reached
  is now named as such.** `classifyCreateFailure` matched the literal
  `admission webhook`, which is what the API server writes when a webhook
  *denies* a request. A webhook whose backend is down, mis-served or
  cert-expired produces `failed calling webhook` instead, so the commonest
  real-world webhook outage — the one case where an operator most needs to be
  pointed at the webhook — classified as the vague default `pod creation is
  failing`. It now reads `an admission webhook could not be reached`, which
  claims only that the API server's call did not complete; kubeagent read one
  event message and does not know whether the backend is down, mis-served,
  cert-expired or merely slow. The denial arm keeps priority over it, and a
  message containing both phrasings is pinned by a test.
- **`FailedCreate` no longer hides behind a pod-level finding.** The check
  skipped any workload that already carried a finding, so a workload that was
  both quota-blocked and crash-looping reported whichever cause the scan
  happened to catch — `FailedCreate` between restarts, `CrashLoopBackOff` during
  the back-off, on the same unchanged workload seconds apart. Both findings now
  attach. The guard is replaced by a replica gap rather than removed: a workload
  is told its controller cannot create pods only while it is short of the pods
  it wants, so a `FailedCreate` event still sitting in the cluster after the
  quota was raised is not re-attached to a workload that has all its pods again.
  Omitted pods count toward the gap, so a truncated pod list is not mistaken for
  a creation failure. The identical clause in the rollout check is deliberately
  untouched — there it is the documented zero-redundancy rule, not this hazard.
- **`ProbeFailure` now names the heaviest failing probe, not the most recent
  one.** With a liveness and a readiness probe both failing, whichever event the
  kubelet wrote last decided the finding — so the same unchanged pod reported a
  readiness problem or a container-restart problem depending on the second it
  was scanned. The types are ranked by consequence instead (liveness, then
  startup, then readiness), inside a two-minute window ending at the newest
  `Unhealthy` event so a long-resolved liveness failure cannot outrank a
  readiness failure that is happening now. The window is anchored on that event
  rather than on the clock, so the detector stays a pure function of the objects
  it is handed. The evidence line now lists every probe type failing on the
  selected container — and only that container's, so a sidecar's liveness
  failure does not collect the app container's probe into its own line.
- **An `exec` probe failure now says what kind of probe it was.** The reason
  vocabulary is HTTP- and TCP-shaped and an `exec` probe's failure text is the
  command's own output, which is exactly the text that may not be forwarded, so
  the evidence line used to end after `probe failed` with no explanation of the
  silence. It now reads `exec probe, output withheld`, with the handler kind
  read from the pod spec's typed `exec`/`httpGet`/`tcpSocket`/`grpc` field
  rather than from any message. The command, the HTTP path and the port are
  still never read.
- **A `CrashLoopBackOff` finding now carries the last exit code, its reason and
  how long ago it happened.** It printed the restart count and nothing else, so
  a flapping pod gave up that detail roughly half the time: the fields are
  durable, but only `RestartLoop` read them, and `RestartLoop` fires only while
  the container is `Running`. An operator scanning the same unchanged pod twice
  got the exit code once. The enrichment applies the same durable conditions
  `RestartLoop` uses — at least three restarts, a recorded non-OOM error
  termination — so an OOM kill, a graceful exit and a container below the
  threshold all keep the plain wording. The issue kind, the reason sentence and
  the absent confidence tag are unchanged: this is evidence for the kubelet's
  own verdict, not a second opinion about it.
- **An init container blocked by a missing ConfigMap or Secret is now reported
  as `Init:CreateContainerConfigError`.** It was reported as
  `CreateContainerConfigError` — the main-container kind — because
  `ConfigErrorDetector` read the init container status slice as a fallback. The
  two are different diagnoses: an init failure blocks the whole pod, and nothing
  else in it has run. The finding now names the init container's position in the
  sequence (`init container "setup" (1/1): …`) the way every other init finding
  does, and `ConfigErrorDetector` reads main containers only, which makes
  `InitContainerDetector`'s no-overlap claim true again. The issue kind is a
  string rather than an enum in every published schema, so no `schemaVersion`
  moves.

### Fixed

- **An init container caught terminated with an error is now reported as
  crash-looping.** The detector matched only the kubelet's
  `Waiting`/`CrashLoopBackOff` window, so the same pod produced a finding or
  produced nothing depending on which moment the scan sampled. The terminated
  window carries a threshold the waiting one does not — at least one prior
  restart, the price of inferring a loop the kubelet has not declared — and a
  container that failed once and then succeeded is still not flagged.
- **A pod row's empty node and IP cells now carry a `—` placeholder.** An
  unscheduled pod has neither, and two empty cells collapsed into a run of
  spaces, so the age column read as if it were the node. Text renderer only;
  the JSON keeps the empty strings.
- **`diagnostics.md` said the image pull error is read "from the pod's
  conditions".** It is read from the container status's `waiting.message`;
  `status.conditions` is a different field and a reader who went looking there
  would not have found the text.

## [1.14.0] - 2026-08-08

### Added

- **Claude Code plugin.** kubeagent installs into Claude Code with
  `/plugin marketplace add imantaba/kubeagent`, wiring the existing
  `kubeagent mcp` server together with two skills and three commands
  (`/kubeagent:triage`, `/kubeagent:why`, `/kubeagent:preflight`). The skills
  are the point rather than the manifest: a model handed the four MCP tools
  with no instruction reads an absent `coverage` key as good news, which throws
  away both the coverage block and the least-privilege RBAC blind-spot
  reporting. Read-only throughout — no tool, skill, or command reaches `--fix`
  — and no new Go production code, so no import-graph invariant moves and none
  of the eight versioned JSON documents change. Tests pin the manifests against
  the chart's `appVersion`, against the real `mcp` flag parser, against the tool
  names `internal/mcp` actually registers, and against `bump-version.sh`. See
  [Claude Code plugin](website/docs/features/claude-plugin.md).

### Fixed

- **`kubeagent_inspect` resolves every object of the seven kinds it
  advertises.** It answered a lookup against the workload list the text report
  renders — a list built for display, not for lookup, which
  `inventory.Prioritize` filters — so objects the cluster plainly had answered
  `found: false`. A controller-owned pod is never a workload in its own right,
  yet `Pod/<name>` is exactly the identity `kubeagent_triage` emits for every
  critical finding, and the shipped skill tells a model to inspect it; on the
  cluster this was found against, that was six of six critical findings. A
  healthy-quiet workload is dropped outright, so inspecting a fully-ready
  Deployment failed too. That list also caps a Job's pod rows at three, so a
  Job's fourth pod could not be inspected either. A ReplicaSet failed for a
  cause of its own, and the widest: the snapshot's ReplicaSet collection was
  never read at all, so a ReplicaSet became a workload only as a side effect of
  a pod that resolved to one — which excluded every Deployment-owned
  ReplicaSet, the shape every rollout is made of, and every ReplicaSet scaled
  to zero. It now resolves from the snapshot directly, carrying its own pods
  and no `owner`. Existence now comes from the raw snapshot the scan already
  collected, so the fix costs **no new cluster read**. A pod answers as itself
  — `kind` `Pod`, its own phase, its one row, its own findings — with
  `desired` and `ready` absent rather than `0`, and a new `owner` field naming
  the controller so a caller can escalate without guessing the workload's name.
  `found: false` now means the object does not exist, and still returns its
  events. No
  `schemaVersion` moves (`kubeagent_inspect`'s result is not one of the eight
  versioned documents) and no import-graph invariant changes. See
  [MCP server](website/docs/features/mcp.md).

- **`kubeagent mcp` no longer exits when the kubeconfig marks no context as
  current.** Under `--allow-context-switch` — and only there — the server now
  starts without a default cluster instead of refusing to start. That flag
  registers `list_contexts`, the tool whose whole purpose is to pick a context,
  and it was unreachable in exactly the case it was built for. All four tools
  stay registered, a call naming a `context` works normally, and a call naming
  none is refused with a message pointing at `list_contexts`. Every other
  startup failure still exits with the same operator-facing error: a missing or
  unreadable kubeconfig, one naming zero contexts, an unreachable API server, a
  `--context` that does not resolve. No `schemaVersion` moves and no import-graph
  invariant changes. See [MCP server](website/docs/features/mcp.md).

- **The shipped `triaging-a-cluster` skill preflights capability, not presence.**
  `command -v kubeagent` passes on a binary built before `kubeagent mcp`
  existed, which is a plugin whose skills load and whose tools never appear.
  The skill now checks whether the `kubeagent_*` tools are in the model's own
  tool list — no subprocess — and shells out only to explain an empty one:
  nothing installed, a binary too old to serve MCP (`kubeagent mcp --help`
  fails), or a server that could not connect. The three commands defer to it
  rather than restating a check that is now three-way.

## [1.13.1] - 2026-08-08

### Added

- A documented route for contributing a policy pack, with admission criteria
  enforced by `go test ./internal/policypack` rather than by review. Three new
  checks cover the registry, the one layer nothing could see: every embedded
  `packs/*.yaml` must have a registry entry — one that does not would ship
  inside the binary while being invisible to `kubeagent policy packs`, to
  `--policy-pack` and to every other test — no two packs may share a name, a
  name must be usable as a `--policy-pack` value and fit the listing column,
  and a summary must be a single line the listing can align.
  Every check that can fail a contributor's pack is written down, so a failure
  is predictable before opening a pull request. Acceptance stays curatorial: the criteria are
  necessary, not sufficient. No pack ships in this release, no existing command
  line behaves differently, and nothing versioned moves.

### Fixed

- Five statements in `CONTRIBUTING.md` that no longer described the code: the
  claim that `main.go` parses flags with the standard library and uses no Cobra
  (the CLI has been a Cobra tree in `internal/cli` since v0.73.0), a read-only
  command list missing `rbac` and `fleet`, the claim that `--fix` is the single
  write path (`scan --rollback` also writes, and `rbac check` issues the one
  POST outside remediation), an import-wall list naming two packages where a
  dozen carry the wall, and a fuzz-target count of twelve against a tree
  holding fifteen.

## [1.13.0] - 2026-08-08

### Added

- A third curated policy pack, `cost`: sixteen rules over seven kinds —
  Deployment, StatefulSet, DaemonSet, CronJob, Job, HorizontalPodAutoscaler
  and PersistentVolumeClaim — covering resource requests and limits, CronJob
  history and active-deadline limits, Job retry limits, autoscaler ceilings
  and claim sizes. Run it with `kubeagent scan --policy-pack cost` or
  `kubeagent gate --policy-pack cost`, print it with `kubeagent policy packs
  --print cost`, and combine it with `--policy` or with
  `reliability`/`security`. Every rule is `info` — a cost finding is
  budget-dependent in a way a security finding is not — so adding the pack
  cannot fail a gate at the default `--fail-on critical` or at `--fail-on
  warning`. Opt-in: omitting `--policy-pack` renders the same bytes as
  before, and nothing versioned moves — `scan` stays at schema version 1.2
  and `gate` at 1.1.

## [1.12.0] - 2026-08-07

### Added

- A second curated policy pack, `security`: twenty-three rules over workload
  pod templates covering privileged containers, privilege escalation, running
  as root, writable root filesystems, added Linux capabilities, `hostPath`
  volumes, host ports, the three host namespaces, seccomp profiles and service
  account token mounting. Run it with `kubeagent scan --policy-pack security`
  or `kubeagent gate --policy-pack security`, print it with `kubeagent policy
  packs --print security`, and combine it with `--policy` or with the
  `reliability` pack. Four properties that are unsafe both when unset and when
  set wrong ship as a pair of rules — an `info` rule for the unset case and a
  `warning` rule for the explicit one — because every operator except `exists`
  and `notExists` skips an absent field, so one rule could only ever catch one
  of the two. No rule is `critical`, so adding the pack to a pipeline that
  passed yesterday cannot fail it today; `--fail-on warning` is the explicit
  act that makes it block. Nothing versioned moves: `scan` stays at schema
  version 1.2 and `gate` at 1.1, the evaluator is unchanged, and the pack needs
  no RBAC grant a plain `scan` did not already have.

## [1.11.0] - 2026-08-07

### Added

- `kubeagent fleet --fleet-file <path>` reads the clusters to sweep from a YAML
  file instead of a kubeconfig's contexts, so a fleet can span several
  kubeconfigs and each row can carry a name the operator chose. An entry names a
  `context`, optionally a `kubeconfig` path and optionally a `name`; selection
  comes from the file, and credentials still come from the kubeconfigs it points
  at. The format cannot express a credential — an entry has three string fields
  decoded strictly, so `server:`, `token:` and `certificate-authority-data:` are
  load errors. `--context` and `--all-contexts` are refused beside it;
  `--kubeconfig` becomes the fallback and `--match` filters the row identity.
  `fleet` moves to schema version 1.2 (added the optional `name` on a cluster
  summary and on an unreachable cluster, both `omitempty`), so a sweep selected
  from a kubeconfig encodes neither key, and every existing consumer is
  unaffected.

### Fixed

- `kubeagent fleet` could render two runs over the same fleet in different
  orders when several clusters shared a kubeconfig context name. Both sorts
  broke ties on the context name, which is not unique across kubeconfigs, and
  `sort.Slice` is not stable. Both now break on the row identity, which a
  duplicate-name load error keeps unique.

## [1.10.0] - 2026-08-07

### Added

- **`kubeagent fleet` cross-cluster correlation.** Under the per-cluster table,
  a sweep now names the issue kinds and the refused reads that appear in two or
  more of the judged clusters, most widespread first — the answer to "is this
  one problem or five" that a one-row-per-cluster view cannot give. It costs no
  new cluster read: both axes were already computed inside the sweep. A cluster
  counts once per signal however loud it is, the denominator is judged clusters
  rather than selected ones, and the correlation changes no verdict — every
  finding it counts was already counted in the cluster that produced it. The
  fleet JSON document gains an optional `shared` property and moves to schema
  version `1.1`; a sweep that found no correlation encodes no key, so every
  existing consumer is unaffected.

## [1.9.0] - 2026-08-07

### Added

- `kubeagent policy packs` lists the curated policy packs compiled into the
  binary, and `--print <name>` emits one as YAML to fork. It contacts nothing:
  no cluster, no kubeconfig, no network, and no model call.
- `scan --policy-pack <name>` and `gate --policy-pack <name>` evaluate a
  curated pack (repeatable, and combinable with `--policy`). The first pack,
  `reliability`, carries fourteen rules covering probes, resource requests and
  limits, replica counts, disruption-budget coverage and image tags. No rule in
  it is `critical`, so adding it to a pipeline cannot fail a build that passed
  yesterday; `--fail-on warning` makes them block.
- `internal/policypack` holds the packs and imports nothing from kubeagent and
  nothing outside the standard library, joining `internal/baseline`,
  `internal/glob` and `internal/knownissues`.

## [1.8.0] - 2026-08-06

### Added

- **`kubeagent known-issues [kind]` — an offline reference for every failure
  kind the detectors report.** With no argument it lists all thirteen kinds
  with a one-line summary; with a kind it prints that failure in full — what it
  means, its likely causes most common first, and read-only next steps whose
  object names are placeholders. No cluster, no kubeconfig, no network, and no
  flags at all. Separately: it makes no LLM call — the text is curated prose
  compiled into the binary, not generated.
- **The detector issue vocabulary is now machine-checked.** Four tests in
  `internal/diagnose` keep the reference and the detectors in step: a
  `go/parser` walk over every string literal reaching a finding's issue field, a
  fixture table driving all nine detectors to produce all thirteen kinds, a
  reverse check refusing an entry for a kind nothing emits, and a second parser
  walk over the two sites that build a kind from a runtime value, which reads
  the guards rather than the output so widening one fails the suite. That
  fourth walk understands a closed set of guard shapes and refuses every other
  shape by name rather than skipping it, so a guard rewritten out of its reach —
  or compared against a named constant rather than a string literal — fails too.
  Refusal is closed rather than best-effort: a value reaches that field only by
  naming it — every `Issue` occurrence must be a composite-literal key or an
  assignment's left side, inside a function declaration, and a second type
  declaring its own `Issue` field is refused as well — by not naming it, which is
  a positional literal or a second name for the type, or by bypassing syntax, for
  which the detectors' import set is pinned to six packages rather than filtered
  for `reflect` and `unsafe`, since `json.Unmarshal(payload, &f)` writes the
  field importing neither. The closure is over `internal/diagnose`'s own
  detectors, which is what the reference documents. A new detector that emits an
  undocumented kind fails the suite.

## [1.7.0] - 2026-08-06

### Added

- **`kubeagent fleet` — sweep many clusters in one read-only pass.** Selects
  kubeconfig contexts with `--context` (repeatable) or `--all-contexts` plus an
  optional `--match` glob, reads them through a bounded worker pool
  (`--workers`, default 8, `KUBEAGENT_FLEET_WORKERS`) with a per-cluster budget
  (`--cluster-timeout`, default 60s, `KUBEAGENT_FLEET_CLUSTER_TIMEOUT`), and
  prints one row per cluster worst first. Each cluster runs exactly the
  evaluation `kubeagent gate` runs, so the two can never disagree. Exit codes
  match `gate`'s, and `inconclusive` outranks `fail` for the same reason it
  does there. Separately: fleet makes no LLM call. `--output json` writes the
  eighth versioned document, `fleet` at schema 1.0; `scan` stays at 1.2 and
  `gate` at 1.1. The report names contexts and issue kinds — never a node,
  namespace, pod or workload.

### Changed

- `internal/policy`'s unexported glob matcher moved to `internal/glob`, a
  stdlib-only leaf `internal/policy` and `internal/cli` now share. Behaviour is
  unchanged: the table test, the blow-up test and `FuzzGlob` moved with it.

## [1.6.0] - 2026-08-06

### Added

- **A learned restart-rate baseline — "what's normal for *this* cluster".**
  `kubeagent baseline capture` prints a JSON document recording each workload's
  restart rate, normalised across its pods' observed lifetimes; `scan --baseline
  <file>` and `gate --baseline <file>` compare a later run against it and report
  workloads that restart much more than their own normal. A workload deviates
  only when both thresholds hold — at least `--baseline-factor` times its
  baseline rate (default 3.0) **and** at least `--baseline-floor` restarts/hour
  above it (default 0.5) — so a rise from 0.001 to 0.01 is not a 10× alarm.
  Pods younger than `--min-pod-age` (default 1h) are excluded from both sides of
  the rate.
- **The baseline document is the seventh versioned JSON document**, published at
  version 1.0 as `website/docs/schemas/baseline-v1.json` and printable with
  `kubeagent schema baseline`.
- `kubeagent baseline capture` is read-only toward the cluster (List calls only,
  all of them already in the `scan` RBAC profile) and makes no model call. It
  writes no file: the document goes to stdout, so an operator reviews it before
  deciding where it goes.

### Changed

- **`scan`'s JSON schema moves 1.1 → 1.2**, additively: `ScanReport` gains an
  optional `baseline` object, present only when `--baseline` was passed. Every
  existing consumer is unaffected. `gate`'s schema does not move — deviations
  are ordinary findings at the `info` level, so they land in the `failing` and
  `reported` arrays that already exist, and because `--fail-on` defaults to
  `critical` a deviation never fails a gate unless an operator asks for it with
  `--fail-on info`.
- The static `--expected-nodes` list is now called the **expected-node list**
  rather than the "expected-node baseline", so "baseline" names one thing.

## [1.5.0] - 2026-08-06

### Added

- **A second Kubernetes distribution in the nightly chaos matrix.**
  `chaos/run.sh --distro kind|k3s` selects which distribution the harness
  creates; `kind` remains the default, so every command line written before
  the flag existed is unchanged. The k3s path (via k3d) resolves a
  digest-pinned `rancher/k3s` image per supported minor from
  `chaos/versions.env`, the same way the kind path resolves a `kindest/node`
  one, and `.github/workflows/chaos-matrix.yml` gains one k3s cell at the
  newest supported minor alongside the existing per-minor kind cells.

### Changed

- **`node_exec`'s skip reason now names the control plane's shape alongside
  cluster ownership**: a k3s cluster the harness created is still refused,
  because an embedded datastore and a kubelet inside the single k3s process
  are not separately stoppable units. `worker_node` selects a node by its
  role label instead of a name pattern, so it holds on a k3d cluster as well
  as a kind one.
- **The `--fix` then `--rollback` audit-log round trip (scenario 9b) now
  records in the report which branch kubeagent took and dumps the audit
  log**, instead of discarding both runs' output — a failed round trip used
  to be indistinguishable from a refusal, a preflight denial, an error, or an
  empty audit log.

## [1.4.0] - 2026-08-05

### Added

- **A portability seam in the chaos harness.** `./chaos/run.sh --context <ctx>`
  runs the suite against a cluster the harness did not create. Each scenario
  declares the infrastructure it needs from a closed six-name vocabulary, and a
  scenario whose need is unmet is skipped with a named reason rather than run or
  silently dropped: six scenarios that write cluster-scoped objects and two that
  need shell access to a node container are refused outright on a cluster the
  harness does not own, and four more are gated on what the cluster turns out to
  have — a LoadBalancer provider, metrics-server, a NetworkPolicy-enforcing CNI,
  and a clean starting verdict. A preflight refuses to start unless the context
  connects, no `chaos-*` namespace already exists, and a namespace create/delete
  round trip succeeds; `--recreate`, `--teardown` and `--k8s-version` are refused
  rather than ignored; and leftover namespaces are swept at the end. The report
  names the platform (server version, node count, deduplicated OS image,
  container runtime and kubelet version) and never the cluster. This makes a
  cross-distribution answer obtainable by hand; gating a distribution in CI is a
  separate piece of work.

### Changed

- **The chaos harness's assertion summary now counts skipped scenarios.** The
  report gains a `- scenarios skipped: N` bullet and, when N is non-zero, a
  fenced list of each skip and its reason; the console line becomes
  `assertions: N run, M failed; K scenario(s) skipped`. It is reported
  unconditionally, including when it is zero. A skip is never a failure and never
  changes the exit code, which stays non-zero if and only if an assertion failed.

### Fixed

- **The chaos report no longer carries the kubeconfig context name.** The
  multi-cluster and MCP scenarios wrote it into the results file — as an
  asserted value that the assertion helper echoes on its passing branch, and
  in the MCP scenario's raw JSON-RPC response, which echoes it independent of
  the asserted value. Both now compare a harness-chosen alias or a derived
  indicator instead, proving exactly what they proved before. Four more
  scenarios could not take that route: the watch daemon labels its own log
  lines, its `/issues` roster and its Prometheus metric series with the
  context name, and a scenario that dumps that output into the report as
  evidence has no name of its own to substitute. Those are now caught at a
  single seam instead — every write to the report, from every scenario,
  passes through one filter that redacts node names and the context name
  together, in a single left-to-right pass over the raw bytes (never a
  regex over the context, since a real one can carry almost anything a
  kubeconfig accepts) rather than as two independently-ordered steps, which
  can each consume their own needle before the other's exact match ever
  runs. One case still slips a fragment through: a node name and the
  context name that overlap without either containing the other can leave
  the loser's non-overlapping tail in the clear, though never either's full
  literal text — see `chaos/README.md` for when. A scenario added later
  inherits the protection rather than having to remember it. A redaction
  that fails withholds the affected section instead of showing it
  unredacted, and never aborts the run. A context name is a credential and
  the results file is designed to be forwarded.

## [1.3.0] - 2026-08-05

### Added

- **PagerDuty as a fourth alert receiver (`kubeagent watch --alert-format
  pagerduty`).** The watch daemon posts [Events API v2](https://developer.pagerduty.com/docs/events-api-v2-overview)
  events directly, so being paged by kubeagent no longer means first deploying a
  Prometheus stack to reach PagerDuty through Alertmanager. A firing object is a
  `trigger` and a recovered one a `resolve`, both on a `dedup_key` derived from
  the object's identity — so a daemon restart re-triggers onto the open incident
  instead of opening a second one. The integration key is a credential and
  inherits the webhook URL's rule: it comes from `KUBEAGENT_ALERT_ROUTING_KEY`
  with no flag, because a flag would put it in the pod spec's args and in `ps`
  output, and it never reaches a log line, a metric label, an error message or a
  rendered manifest. `KUBEAGENT_ALERT_WEBHOOK` stays the endpoint for all four
  formats and becomes optional for this one, defaulting to PagerDuty's published
  URL. The Helm chart grows **no new values**: the existing
  `alerts.existingSecret` / `alerts.secretKey` pair feeds the routing key when
  `alerts.format` is `pagerduty`. Closes Theme E's last open receiver. See
  [watch mode](https://k8sproject.top/features/watch-mode/).

## [1.2.0] - 2026-08-05

### Added

- **In-cluster dashboard (`kubeagent watch --dashboard`).** A read-only HTML
  page at `/dashboard` on the daemon's metrics port: tracked incidents active
  and resolved with firing duration and time-to-resolution, per-cluster
  reachability, the aggregate counters, SLO burn when `--slo-target` is set, and
  on-incident explanations when `--explain` is set. It renders only state the
  daemon already tracks, so it performs no extra cluster read and needs no extra
  RBAC. Separately, no dashboard request makes a model call.
  Server-rendered with zero JavaScript, so
  `html/template`'s contextual escaping is the single escape boundary; the new
  `internal/dashboard` package imports nothing from kubeagent, enforced by a
  source-level test. The page is **unauthenticated**, exactly like `/metrics`
  and `/issues` on the same port — see
  [the docs](https://k8sproject.top/features/dashboard/) for the exposure
  posture. Enable with `--dashboard`, `KUBEAGENT_DASHBOARD=true`, or
  `--set dashboard.enabled=true`. Completes Theme G.

### Fixed

- **`--explain` no longer sends a pod's generated name to the model.** Every
  finding records the pod it was diagnosed on, and the deterministic kubectl
  command rendered beside it targeted that pod by name — so a prompt built for
  a pod-scoped issue carried `kubectl -n shop describe pod web-6b8d94f7c5-q2xzt`
  out of the cluster, on both `scan --explain` and the watch daemon's
  on-incident explanations. Pod rows were already withheld for exactly this
  reason; the command was a second route out that no test covered, because every
  fixture left the finding's pod field empty. The prompt now carries
  `kubectl -n shop describe pod <pod>` — same namespace, same verb, same
  container, placeholder for the generated name — and a finding diagnosed on the
  object itself (`RolloutStuck` names the Deployment) still renders its own name,
  which the prompt has already stated as the object that broke. Reports are
  unchanged: they run locally and keep the real command. Caught by the nightly
  chaos matrix, whose egress assertion fires only when the explained incident
  happens to be pod-scoped.

## [1.1.0] - 2026-08-02

### Added

- The Helm chart can name the cluster it runs in: `watch.clusterName` renders
  `--cluster-name`, setting the `cluster` label on every metric series and the
  `cluster` field in `/issues` and `/explanations`. Until now that flag was only
  reachable through the multi-cluster block, so a single-cluster install had no
  way to say what it was watching and every series read `cluster="local"` — a
  label that stops meaning anything the moment a second daemon's metrics reach
  the same Prometheus. `multicluster.localName` names the same thing and keeps
  working; `watch.clusterName` takes precedence, and leaving both empty renders
  exactly what it rendered before, byte for byte.
- Two chaos scenarios cover the two opt-in health probes that had no live
  coverage. Scenario 21 proves `--control-plane-health` really issues its
  `/readyz` request against a running apiserver, classifies it `ok`, and reports
  a ready control plane by saying nothing. Scenario 22 breaks DNS the way a
  liveness check cannot see — CoreDNS Ready, serving metrics, and answering
  every query `SERVFAIL` — and proves `--dns-health` names it. Scenario 20 now
  runs both flags under its least-privilege identity too: the CoreDNS
  `pods/proxy` refusal joins the three it already asserted, and `/readyz` — which
  a stock cluster grants to every authenticated identity — is asserted *not* to
  be named, so a read that succeeded is never reported as one kubeagent could not
  make.

### Changed

- Policy evaluation no longer builds a slot list proportional to the object it
  is checking. A path with a wildcard names one slot per list element, and a
  rule's verdict is decided by the first slot that violates, so the traversal
  now yields slots one at a time and stops there. Evaluating one rule against a
  Pod with 40 000 wildcard positions dropped from 40 224 allocations and 17 MB
  to none, and a cluster's worth of objects times a policy's worth of rules
  multiplied both. No verdict changes: the slots, their order, and the arity
  rule that an absent element still contributes an absent slot are all
  unchanged, and a differential fuzz run over 819 000 paths found no divergence
  from the previous resolver.
- The chaos harness side-loads Flux's controller images before scenario 17 runs,
  the same treatment Calico already gets. A Kind node has its own image store and
  the kubelet pulls serially, so on a cold cluster the six controllers queued up
  behind one another and the two the scenario depends on were still pulling after
  their rollout waits had timed out — the scenario then scanned a Flux that had
  never reconciled anything. Two assertions now state that dependency directly,
  so a Flux that fails to start says so instead of surfacing as a drift finding
  that did not appear. The suite runs 124 assertions, up from 122.
- `internal/safetext.Line` now bounds combining marks: a character keeps at most
  four, and a mark that opens a line or follows a space is dropped because it has
  no base character to sit on. Real text is unaffected — decomposed Vietnamese,
  Arabic, Devanagari, Hebrew and Thai stack at most three or four — but an
  unbounded stack in a container log can no longer paint over the terminal rows
  above and below its own line. A twelfth fuzz target, `FuzzLine`, asserts the
  bound and the sanitizer's idempotence on arbitrary bytes.

## [1.0.0] - 2026-08-01

**1.0 is a commitment, not a feature.** Nothing about how kubeagent behaves
changes at this version; what changes is that its surfaces are now covered by a
written contract, and that the Kubernetes versions it claims to support are ones
a nightly matrix actually proves. From here a MAJOR release is the only one that
may break a stable surface. See
[Compatibility and support](https://k8sproject.top/compatibility/) for what that
covers and — equally deliberately — what it does not.

Theme H, the production-contract track, is complete: supply-chain integrity
(0.68), least-privilege RBAC (0.69), fuzzed detectors (0.70), versioned JSON
schemas (0.71), bounded scan concurrency (0.72), the Cobra CLI (0.73), policy as
code (0.74), and now the cross-version chaos matrix and the contract itself.

### Added

- **The chaos harness is a gate, not a report.** Its checks are now
  machine-checked assertions (`chaos/assert.sh`: `expect_eq`, `expect_ge`,
  `expect_contains`, `expect_absent`) instead of prose an operator had to
  read closely. `./chaos/run.sh` now exits non-zero the moment any assertion
  fails, and ends with an `## Assertion summary` naming every failure and an
  `assertions: N run, M failed` line. All 20 scenarios carry an assertion
  except scenario 2 (expired certificates, out of scope on Kind), which runs
  no scan by design.
- **`chaos/run.sh --k8s-version <minor>`** pins the harness to a specific
  Kubernetes minor's digest-pinned kind node image, instead of letting kind
  pick its own. The supported minors and their images live in one place,
  `chaos/versions.env`; everything the harness derives from the cluster — its
  name, context, report path, CoreDNS backup — takes that minor's suffix, so
  two coexist on one machine without colliding. `bash
  chaos/version-selftest.sh` checks the resolver with no cluster and no
  docker. See `chaos/README.md`.
- **Nightly cross-version chaos matrix.** `.github/workflows/chaos-matrix.yml`
  runs the full 20-scenario suite once per Kubernetes minor kubeagent
  supports (v1.32, v1.33, v1.34 today), each on its own disposable kind
  cluster, `fail-fast` off so one minor's outage never cancels another's; a
  `workflow_dispatch` input reruns the same matrix, or a chosen subset, on
  demand. Every report is scanned for credential material before it is
  uploaded as an artifact — a flagged report fails the job and is never
  published, while a failed suite with a clean report still uploads, because
  a failed nightly is exactly the run that needs to stay diagnosable. No
  secret is granted: `ANTHROPIC_API_KEY` is never set, so the nightly gates
  kubeagent's deterministic core, not the `--explain` path. See
  `chaos/README.md` for what the matrix does and does not cover.
- **A written compatibility and support contract**
  ([website/docs/compatibility.md](https://k8sproject.top/compatibility/)).
  It names the surfaces that are stable within 1.x — the command line
  (including the single-dash long-flag shim that keeps pre-v0.73 invocations
  working), `gate`'s exit codes, the documented `KUBEAGENT_*` variables, the
  Helm chart's values, and the six JSON documents by reference to their own
  schema contract — and, just as deliberately, the ones that are not: the text
  report's wording, the HTML markup, every `internal/` package, and any
  model-generated prose. It states the supported Kubernetes window as an
  *evidenced* one — v1.32, v1.33 and v1.34 are listed because the nightly
  chaos matrix passes 105 assertions against each — and it sets a deprecation
  policy: one full MINOR of continued operation, a warning on stderr only so a
  JSON pipeline is never corrupted, removal no earlier than the next MAJOR.

### Fixed

- Log-derived network addresses (a Service's ClusterIP, a Pod's IP, an
  in-cluster hostname) no longer reach a model prompt. `internal/logscan`'s
  conn-refused signature deliberately captures the dial target for the
  operator's text report, but both prompt builders — the watch daemon's
  incident prompt and `scan --explain`'s inventory prompt — wrote that same
  string into the outgoing payload verbatim. A new `redact.Addresses` strips
  it at egress; the text report is unchanged. Found by the cross-version
  chaos matrix on a real runner: scenario 14's egress assertion went red on
  v1.32 and v1.33.

## [0.74.0] - 2026-08-01

### Added

- **Policy as code.** `kubeagent scan --policy FILE|DIR` and
  `kubeagent gate --policy FILE|DIR` evaluate organization-specific checks
  written in YAML, alongside the built-in detectors — "every production
  Deployment must be covered by a PodDisruptionBudget", "no image may come from
  a registry outside the allowlist" — without forking kubeagent. A rule names
  one kind and asserts one thing about it, from a closed set of ten operators
  and two relations. `kubeagent policy validate FILE…` checks a file with no
  cluster and no kubeconfig, so CI can reject a bad policy before a deploy.
  Evaluation is strictly read-only and adds no RBAC: the selectable kinds are
  exactly the kinds a plain scan already reads. Secrets are not selectable and
  a ConfigMap's `data` is not readable. A rule kubeagent could not evaluate is
  reported as **not evaluated** and fails a gate rather than passing quietly.
  See [Policy as code](https://k8sproject.top/features/policy/).

### Changed

- `scan`'s JSON document is schema version **1.1** (added `policy`) and
  `gate`'s is **1.1** (added `policyNotEvaluated`). Both additions are
  `omitempty`, so a run without `--policy` encodes neither and every existing
  consumer is unaffected.

## [0.73.0] - 2026-07-31

### Added

- `kubeagent completion bash|zsh|fish|powershell` prints a shell completion
  script, generated from the command tree so it stays correct as flags change.
  It contacts no cluster and reads no kubeconfig. Under krew, `kubectl`
  completion additionally needs a `kubectl_complete-kubeagent` shim — see
  [Shell completion](https://k8sproject.top/features/completion/).
- Per-command help: `kubeagent scan --help` now describes `scan`, not every
  command at once.

### Changed

- The CLI is built on Cobra, retiring the standard-library `flag` package v1
  shipped with. Every subcommand, every flag, every kubeagent error message and
  every exit code is unchanged, and a compatibility shim preserves the
  single-dash long-flag form (`-kubeconfig path`) that pflag would otherwise
  reject. Four details of the command line did change, all of them consequences
  of the flag library rather than choices about kubeagent's own behaviour:
  - `--n` is no longer accepted as a spelling of `--namespace`. `-n` and
    `--namespace` both work, as in every other kubectl-adjacent tool; the
    standard library accepted `--n` only because it had to declare the
    shorthand as a second full flag name.
  - An unrecognized flag is now reported in pflag's words
    (`unknown flag: --nonesuch`) rather than the standard library's
    (`flag provided but not defined: -nonesuch`), and the command no longer
    dumps its full flag list to stderr after the message. Exit codes are
    unchanged, including `gate`'s `4` for a usage error.
  - `kubeagent mcp` and `kubeagent tui` now reject a stray positional
    argument instead of silently ignoring it.
  - `<command> --help` exits 0. It used to exit 1 for every subcommand
    except `version`, and 4 for `gate`, because the standard-library flag
    set reported `--help` as a parse error. Asking for help is not an
    error, and no other exit code changed.

There is no `### Removed`. The four items above are the complete list of
command-line changes: each was found by a review of the migration rather than
chosen, each is recorded here rather than left for a user to discover, and none
of them touches a kubeagent-authored error string or an exit code.

## [0.72.0] - 2026-07-31

### Added

- `KUBEAGENT_SCAN_WORKERS` — how many of `scan`'s independent cluster reads may
  be in flight at once. 8 by default, clamped to 1..64. A value that does not
  parse is ignored rather than raised as an error. Under `kubeagent watch` the
  daemon runs one goroutine per cluster, so the effective cap across the process
  is this number times the number of clusters watched.
- `KUBEAGENT_QPS` and `KUBEAGENT_BURST` — restore a client-side request rate
  limit for anyone who needs one. Unset, kubeagent applies none.

### Changed

- `scan` now issues its independent cluster reads through a bounded worker pool
  instead of one at a time. Output is unchanged, byte for byte: blind spots and
  read failures are recorded by a sequential block that walks a fixed report
  order, so the rendered order can never depend on which read answered first.
- kubeagent no longer accepts client-go's default client-side rate limiter
  (5 QPS, burst 10 per API-group client). That default metered the scan against
  itself: measured on a three-node cluster, a scan with every add-on enabled
  took 6.01s with the limiter and 0.12s without, one read at a time in both,
  for byte-identical output. Load shedding is left to the API server's own
  Priority and Fairness, which knows what the server can take; `KUBEAGENT_QPS`
  restores a client-side limit. The pool is worth a further 2× on top (0.12s to
  0.06s), and nothing at all underneath the limiter — with the bucket back on,
  eight workers finish in the same 6.01s as one, which is why both changes
  shipped together.

## [0.71.0] - 2026-07-30

### Added

- **Versioned JSON output** — every machine-readable document now declares a
  `schemaVersion` (`scan`, `gate`, `rbac print`, `rbac check`, and the watch
  daemon's `/issues` and `/explanations`), and each surface's JSON Schema is
  generated from the Go types and published at
  `https://k8sproject.top/schemas/<name>-v1.json`. A drift test fails when a
  document's shape moves without a version bump, and says whether the change was
  additive or breaking. See
  [the JSON schema contract](https://k8sproject.top/features/json-schema/).
- **`kubeagent schema [name]`** — print the schema of any output document, or
  list them. Generated at runtime from the running binary's own types; needs no
  cluster and no kubeconfig.

### Changed

- **Breaking: `rbac print --output json` and `rbac check --output json` now emit
  an object, not a bare array.** An array root cannot carry a version field, so
  the rules moved under `rules` (alongside `roleName`) and the feature statuses
  under `features`, each beside a `schemaVersion`. Taken deliberately before
  1.0: an unversioned array root could never gain a version later without
  exactly this break. Recover the old shape with
  `jq '.rules'` or `jq '.features'`.

## [0.70.0] - 2026-07-30

### Added

- Native fuzzing across the detectors and the advisory parsers: seven `go test
  -fuzz` targets assert that no Kubernetes object or endpoint response can panic
  a scan, that the detector set stays pure and deterministic, and that no raw
  byte from the cluster reaches a terminal. Seed corpora replay on a plain `go
  test`, so a regression fails a pull request without any fuzzing budget; a real
  campaign runs nightly in `.github/workflows/fuzz.yml`. Objects come from the
  test-only `internal/fuzzgen`, which draws DNS-1123 alphabets for the fields the
  API server validates and hostile bytes for the fields it does not.
- `internal/safetext`: one sanitizer (`Line`) for text arriving from unvalidated
  API fields — bounds it to 512 runes and removes control characters, Unicode
  formatting characters (U+202E and friends, which `unicode.IsControl` does not
  catch) and invalid UTF-8.

### Fixed

- Text from fields the Kubernetes API server does not validate reached an
  operator's terminal unfiltered at nine ingress points: `waiting.Message` (in
  three detectors), `terminated.Reason`, `PodScheduled` and event messages, the
  container name parsed out of an event field path, a crashed container's log
  excerpt, and the dependency address spliced into a `connection refused` cause.
  The log tail is the one an unprivileged attacker controls outright, and it
  carried the same ANSI escapes kubeagent's own TUI uses to switch screens. All
  nine now pass through `safetext.Line`.
- `dnshealth`: `strconv.ParseFloat` accepts `"NaN"`, `"+Inf"` and `"-Inf"`
  without error, and converting a non-finite or out-of-range float to `int64` is
  implementation-defined — it yields `math.MinInt64` on amd64. A CoreDNS
  exporter reporting any of them turned a DNS response count negative and
  dragged the error ratio with it. Non-finite and negative samples are dropped,
  large ones clamped, and every accumulation saturates instead of wrapping.
- `controlplane`: the failing `/readyz` check names were tokens lifted from an
  HTTP body no schema constrains, printed unfiltered, with no count bound. They
  are now sanitized and capped at 20.
- `certhealth`: a certificate's `CommonName` and DNS SANs are chosen by whoever
  creates the `kubernetes.io/tls` Secret, and X.509 string types do not exclude
  control characters. Both are sanitized before they reach a report.
- `collect`: the kubelet `/healthz`, CoreDNS `/metrics` and apiserver `/readyz`
  reads handed an unbounded body straight to a parser. Parsed input is now capped
  at 1 MiB. This bounds the parsers, not the transfer: client-go's `Raw()`
  returns a body it has already read in full and gives no access to the
  underlying reader.

## [0.69.0] - 2026-07-29

### Added

- `kubeagent rbac print` and `kubeagent rbac check`: print the minimal ClusterRole a
  profile or feature list needs, and ask the API server whether the current identity may
  run it. `check` exits 1 when a feature is blocked, so CI can gate on it.
- Every RBAC manifest under `deploy/` and the chart's ClusterRole are now generated from a
  single feature table in `internal/rbacprofile`, with a golden test that fails on drift.
  Role names, resources and verbs are unchanged.

### Fixed

- A refused read behind a feature flag is now named as a blind spot in the scan report.
  `--disk-usage` and `--logs` discarded the error entirely, so a missing `nodes/proxy` or
  `pods/log` grant was invisible; `--certs`, `--kubelet-health`, `--dns-health`,
  `--control-plane-health` and the `--operators`/`--drift` advisories recorded it only
  inside their own section. A scan that could not see no longer looks like a scan that saw
  nothing wrong.
- A refused read is now reported in kubeagent's own words on every surface. The reason
  string previously carried the API server's own message, which interpolates the
  authorizer's error and so names the requesting identity — a ServiceAccount, an IAM ARN,
  an OIDC email, or arbitrary text under webhook authorization. That message reached the
  scan report, `kubeagent gate`, the TUI and the MCP tool results.
- `--logs` no longer claims a missing `pods/log` grant when the container simply has no
  previous run. A container that has never terminated — the normal case for
  `ImagePullBackOff` and `CreateContainerConfigError` — makes the API server answer `400`,
  not `403`, and that answer was being reported as a permission problem.

## [0.68.0] - 2026-07-29

### Changed

- **Pre-release tags no longer move `imantaba/kubeagent:latest`.** A SemVer
  pre-release (`v1.2.3-rc.1`) publishes as a GitHub pre-release and pushes only
  its own image tag, so an unpinned `docker pull` keeps resolving to the newest
  stable release. A tag that is not a SemVer release version now stops the
  release workflow instead of producing a release under a malformed name.
- **Relicensed to Apache-2.0.** The MIT `LICENSE` is replaced by the Apache
  License 2.0, with the `NOTICE` file restored. Apache-2.0 is the license
  foundations expect of a donated project, it grants an explicit patent
  licence that MIT does not, and relicensing is only cheap while the copyright
  sits with one author. Every dependency remains permissively licensed
  (Apache-2.0 / BSD / MIT), so nothing downstream changes.

### Added

- **Signed releases, SBOM and build provenance.** Every release now carries a
  keyless [cosign](https://docs.sigstore.dev/) signature over `SHA256SUMS` —
  which binds every archive by hash — plus a signed container image, an SPDX
  SBOM of the linux/amd64 binary, and SLSA build provenance for the archives
  and the image. Verification pins the release workflow's identity through
  Fulcio and Rekor, so there is no key to distribute or rotate. New page:
  [Verifying a release](https://k8sproject.top/verify/).
- **Byte-reproducible release archives.** The same tag rebuilt on another
  machine now produces the same `SHA256SUMS`: the Go build is trimmed of
  absolute paths, tar records a fixed mtime with numeric zero ownership and
  bytewise entry order, and gzip no longer stamps its header.
- **Project governance documents.** `GOVERNANCE.md` (single-maintainer
  decision making today, with an automatic switch to lazy consensus plus
  majority votes once there are three or more maintainers, and stated criteria
  for becoming one), `MAINTAINERS.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`
  (the CNCF Code of Conduct, adopted as this project's own text and enforced by
  the maintainers), and `ADOPTERS.md` — joining the `SECURITY.md` already in
  the tree.
- **Issue forms and a pull-request template.** Three issue forms (bug report,
  missed failure mode / new detector, feature request) and a PR template that
  asks a contributor to confirm the change keeps the project's invariants.
  Blank issues are off; the chooser links the private advisory channel first,
  so a vulnerability has somewhere to go that is not a public issue. The bug
  form requires an explicit "I redacted this" acknowledgement, because
  kubeagent output carries namespace, node, and image names.
- **DCO sign-off is now required and enforced.** Every commit needs a
  `Signed-off-by:` trailer matching its author (`git commit -s`); there is no
  CLA. A new `DCO` workflow runs `scripts/dco-check.sh` over each pull
  request's commits — a self-contained shell check with no third-party action
  in the path, runnable locally as `scripts/dco-check.sh main`.

## [0.67.0] - 2026-07-29

### Added

- **Interactive TUI** — `kubeagent tui` opens a full-screen, keyboard-driven
  browser over one scan: filter by severity (`1`/`2`/`0`), open a finding in
  full (`⏎`), read what kubeagent could not see (`b`), re-scan (`r`). It shows
  exactly what bare `kubeagent scan` shows, makes no LLM call on any path, and
  is read-only toward the cluster. Three flags — `--kubeconfig`, `--context`,
  `-n` — and no `--output`: a TUI is not redirectable, and piping it is refused
  before kubeagent touches the network. Documented at
  [Interactive TUI](https://k8sproject.top/features/tui/).

## [0.66.0] - 2026-07-28

### Added

- **Shareable HTML report** — `kubeagent scan --output html` renders one self-contained HTML document: header with version, namespace scope, timestamp and severity tally; a blind-spots block whenever a read failed; the full findings table with a pure-CSS severity filter; and collapsed detail sections for cluster health, the workload inventory, and the `--explain` narrative. The document carries no JavaScript and no external stylesheet, font, or image, so it opens offline and renders under a strict Content-Security-Policy — and it carries no cluster identity (no context name, no API server URL, no kubeconfig path), the same rule `kubeagent gate`'s verdict follows. Blind-spot reasons are classified rather than quoted — a Kubernetes denial interpolates the authorizer's own error, which names the user and under webhook authorization carries a third-party backend's text, so the document says "permission denied", "the cluster does not serve this resource type" or "the read failed" in kubeagent's words and leaves the exact message to `--output text` and `--output json`. Finding reasons are still quoted verbatim, because that quote is the diagnosis. `--output text` and `--output json` are byte-for-byte unchanged, and `scan`'s exit code is unchanged in both directions. New leaf package `internal/htmlreport`.

## [0.65.0] - 2026-07-28

### Added

- **CI/CD gate mode** — a new `kubeagent gate` subcommand with a stable exit-code
  contract (`0` pass, `1` fail, `2` inconclusive, `3` timeout, `4` usage) and a
  SARIF 2.1.0 renderer for GitHub code scanning. `gate` with no `--wait-for` is
  a pre-deploy sanity check; `gate --wait-for deployment/api -n prod` waits for
  that rollout to settle and then judges only findings attributable to it.
  "kubeagent could not see the cluster" gets its own exit code rather than
  passing quietly, and the escape hatch is explicit: `--allow-partial-read
  <resource>`, or `kubeagent gate || [ $? -eq 2 ]`. Read-only, and no LLM call
  on any gate path. See [CI/CD gate](website/docs/features/ci-gate.md).
- **`internal/findings`** — one ordered severity model (`info < warning <
  critical`) and one `scan.Result` flattener, consumed by the gate. The `mcp`,
  `watch`, and `report` surfaces keep their existing severity handling for now:
  migrating them would change the MCP tool payloads shipped in v0.63.0.

## [0.64.0] - 2026-07-28

### Added

- **`kubectl` plugin via krew** — kubeagent installs as a `kubectl` plugin with
  `kubectl krew install --manifest-url=https://github.com/imantaba/kubeagent/releases/latest/download/kubeagent.yaml`,
  after which `kubectl kubeagent scan` works anywhere `kubectl` does. The
  binary is unchanged: same detectors, same output, same read-only default.
  Usage, warning, and error text now name the command you actually typed, so a
  plugin user is no longer told to run `kubeagent`, which is not on their `PATH`.
  Not yet in the upstream krew-index, so `--manifest-url` is required.

### Changed

- **Releases now ship four platforms** — `linux/amd64`, `linux/arm64`,
  `darwin/amd64` and `darwin/arm64`, each a tarball with the binary, `README.md`
  and `LICENSE`, all listed in `SHA256SUMS`. The unversioned
  `kubeagent_linux_amd64.tar.gz` asset is unchanged, so the existing
  `releases/latest/download/…` quick-install keeps working. The krew manifest
  is rendered at release time from `krew/kubeagent.yaml.tmpl` with those
  checksums and attached as `kubeagent.yaml`. Windows is deliberately not
  published.

## [0.63.0] - 2026-07-28

### Added

- **MCP server (`kubeagent mcp`)** — serves kubeagent's deterministic, read-only
  diagnosis to AI agents over the Model Context Protocol on stdio. Four tools:
  `kubeagent_triage` (verdict plus findings), `kubeagent_inspect` (one workload,
  with its events), `kubeagent_advisory` (the opt-in operator, drift, capacity,
  security and certificate sections), and `list_contexts` (only when started
  with `--allow-context-switch`). Every result from the three diagnosis tools
  carries a `coverage` block naming what ran, what was skipped and why, and
  what was read only partially, so a model can tell "nothing is wrong" from
  "nothing was checked". No tool can reach `--fix`, and the server never calls
  an LLM.

### Changed

- Redaction (URL and error scrubbing) moved out of `internal/alert` into its
  own leaf package, `internal/redact`, so it can be shared without pulling in
  the alerting stack — used by both `internal/watch` and the new
  `internal/mcp`. CLI output is unchanged.
- The CLI's optional advisory sections (`--operators`, `--drift`, `--capacity`)
  now compute their degradations as a structured result in `internal/advisory`
  instead of writing warnings directly to stderr from `main.go`; `main.go`
  prints the same two warning sentences from that structured result. CLI
  output is unchanged.

## [0.62.0] - 2026-07-27

### Added

- **Capacity hints (`scan --capacity`, opt-in, advisory)** — a `CAPACITY`
  section carrying two sub-blocks: **Headroom** (free CPU/memory across the
  nodes that can actually take a pod right now, the single node with the
  largest free fit, the tightest node by requested ratio, and whether the
  cluster survives losing its biggest node) and **Right-sizing** (workloads
  shaped wrong on paper — no requests set, a limit with no matching request,
  or a request that can never be scheduled on any node). The limit-with-no-
  request rule reads a workload's own pod template (Deployment, StatefulSet,
  DaemonSet, Job, or CronJob), not the admitted Pod: the API server defaults
  a Pod's unset request from its limit before storing it, so only the
  template an author wrote still shows the authored shape. Two numbers this
  never produces: money (kubeagent has no price table; every figure is cores
  and GiB) and a peak (metrics-server keeps no history — one ~30s sample,
  nothing retained). A usage sample never puts a workload on the list; it
  only annotates a row a structural rule already raised, so a healthy
  workload with a tiny sample never appears. Node-loss placement uses
  first-fit-decreasing, which is one-sided sound: success is a constructive
  proof the requests fit, failure only ever reads `may not fit`, never `does
  not fit`. Advisory like `--operators` and `--drift`: never a `Finding`,
  never changes the verdict or the exit code, not wired into `watch`. Needs
  no new RBAC — nodes and pods are already read on every scan, and the one
  new call (`/apis/metrics.k8s.io/v1beta1/pods`) needs nothing beyond the
  existing node-metrics grant.

## [0.61.0] - 2026-07-27

### Added

- **GitOps drift (`scan --drift`, opt-in, advisory)** — a `GITOPS DRIFT` section
  answering whether the cluster is still converging on Git, for Argo CD
  `Application`s and Flux `Kustomization`s/`HelmRelease`s. Five states —
  `synced`, `pending` (differs but still converging), `stale` (differs past
  `--drift-age`, default `1h`), `blocked` (suspended, stalled, auto-sync off, or
  the last sync failed), and `unknown`. Never a finding, never affects the
  verdict or the exit code. Nothing is compared against a Git host: every signal
  is read from the reconciler's own status, and no repo URL, `spec` content, or
  condition message is ever printed — revisions are reduced to a 7-character SHA
  or withheld. Shares one fetch with `--operators` when both are set.
  `deploy/rbac-gitops.yaml` grants the `list`-only rights a restricted context
  needs.

## [0.60.0] - 2026-07-27

### Added

- **Operator health (`scan --operators`, opt-in, advisory)** — reports what
  cert-manager, CloudNativePG, Longhorn, Argo CD, Flux, and the Prometheus
  operator say about themselves, read through the dynamic client so kubeagent
  compiles against none of their Go APIs and needs no rebuild when you install
  one. API discovery is the installation signal: an operator whose group the
  API server does not serve is skipped with zero API calls, no error, and no
  report entry, so a default scan and a scan on a cluster running none of them
  both cost nothing.

  Ten resource kinds are assessed by two declarative rules, and every rule
  degrades to `unknown` rather than `unhealthy`: a renamed field or an unseen
  CRD version yields a missing signal, never a false outage. A suspended Flux
  reconciler reports `suspended`, not a stale `Ready=False`. Argo CD's sync
  status is deliberately not read — drift is a separate concern, and flagging
  it would make every pending deploy look like a failure.

  The report carries metadata and state only: namespace, name, kind, state, and
  the operator's own CamelCase condition reason. A CR's `spec`, its arbitrary
  `status` content, and a condition's free-text `message` never reach it — an
  Argo CD `Application` embeds a Git URL that can carry a token, a cert-manager
  `Issuer` references ACME account keys, a CNPG `Cluster` names backup
  credentials, and cert-manager writes ACME order URLs into condition messages.
  Counts stay exact while at most 20 unhealthy resources are listed per kind,
  the remainder reported rather than dropped.

  Read-only: `list` only, never `get`, `watch`, or any write. Advisory: the
  section never affects the cluster verdict. `deploy/rbac-operators.yaml` is
  the scan-only grant; without it the report still names which operators are
  installed and marks each kind forbidden.

## [0.59.0] - 2026-07-26

### Added

- **Multi-cluster hub.** `kubeagent watch --context prod-eu --context prod-us`
  runs one informer set per cluster inside a single process, behind one HTTP
  endpoint. `--context` is now repeatable, `--cluster-name` names the default
  cluster (the one watched with no `--context`), and `--include-local` adds it
  alongside the listed contexts. Every metric series carries a `cluster` label,
  `/issues` and `/explanations` carry a `cluster` field, `/issues` gains a
  `clusters` roster with each target's up/down state, and every alert names its
  cluster. A context missing from the kubeconfig is fatal at startup; a cluster
  that fails at runtime reports `kubeagent_cluster_up 0` and degrades on its own
  while the others keep reconciling. `/readyz` reports ready once every cluster
  has finished a first reconcile attempt and never flips on cluster health. The
  daemon remains strictly read-only toward every cluster: get/list/watch only.
  A failing cluster's error is redacted to `scheme://host` before it reaches the
  roster or a log line: a kubeconfig server URL can carry basic-auth userinfo or
  an auth-proxy token, and the `*url.Error` client-go returns would otherwise
  republish the whole request URL.
- The Helm chart gained `multicluster.*`: a kubeconfig mounted read-only from a
  Secret (never a `values.yaml` literal), one `--context` per listed entry, and
  the local cluster watched alongside them through its existing ServiceAccount.
  The chart's ClusterRole is unchanged and still covers the local cluster only —
  each remote credential must be read-only, which kubeagent cannot enforce from
  inside the pod.
- Chaos scenario 15 covers multi-cluster labelling, the cross-cluster merge, and
  per-cluster degradation with a deliberately dead third target.

### Changed

- Every per-cluster metric series now carries a `cluster` label, including in
  single-cluster operation, where it defaults to `local`. PromQL selectors match
  regardless of extra labels, so existing queries keep working; a recording rule
  that groups `by (...)` should add `cluster`. The alert and explanation series
  stay unlabelled — one sink and one budget per process, not per cluster.
- Alert payloads gained a cluster: a `cluster` field in the JSON format, a
  `cluster` label in the Alertmanager format, and a `prod-eu/Deployment/shop/web`
  object path in the Slack text.

## [0.58.1] - 2026-07-26

### Fixed

- The two `watch --explain` CLI wiring tests built a real clientset before
  reaching their stub, so they read whatever kubeconfig the machine happened to
  have. They passed locally and failed on a runner with no `~/.kube/config`,
  which broke the 0.58.0 release build — that tag was never published. Both now
  point at a dead kubeconfig fixture and are hermetic. No shipped behaviour
  changes; 0.58.1 is 0.58.0 with a green build.

## [0.58.0] - 2026-07-26

### Added

- `watch --explain`: opt-in, rate-limited on-incident explanations. When an
  object breaks, the daemon sends a second, model-written message a few seconds
  after the page — likely cause, how to confirm, and the deterministic fix.
  The alert itself still fires immediately and LLM-free; the explanation rides
  the same webhook sink, so retry, backoff and URL redaction all apply
  unchanged. New flags `--explain-cooldown` (default `1h`) and
  `--explain-budget` (default `20`/hour) bound the spend, a new
  `/explanations` endpoint serves the latest explanation per object, and five
  `kubeagent_explain_*` series make throttling visible. Works with a local
  OpenAI-compatible model via `KUBEAGENT_EXPLAIN_ENDPOINT`.
- Helm: `explain.*` values, with the API key and the endpoint URL both wired
  from a Secret via `secretKeyRef` — an endpoint URL is a credential too, since
  it can embed a token. The chart refuses to render if `explain.enabled` is
  true with no `explain.existingSecret` — nothing comes from `values.yaml`.

### Changed

- The watch daemon's package documentation no longer claims "no LLM". It stays
  strictly read-only toward the cluster: `--explain` adds no cluster read and no
  RBAC verb, because the model sees only findings the daemon had already
  collected.

## [0.57.0] - 2026-07-26

### Added

- `watch` SLO burn-rate tracking: an opt-in `--slo-target` (a percentage, e.g.
  `99.9`) turns on a time-weighted availability SLI — `good`/`total`
  workload-seconds over the unfiltered census, where good means not flagged,
  the same predicate the issue tracker uses — and a multi-window
  error-budget burn rate over it, following the Google SRE workbook's fast
  (1h, 14.4×) / slow (6h, 6×) pair. An alert fires only when both windows
  breach their threshold at once, gated on each window carrying at least 60%
  coverage: state is in-memory and resets on restart, so the gate keeps a
  just-started daemon from paging on its own warm-up. Five new Prometheus
  series render only when SLO tracking is on: `kubeagent_slo_availability_ratio`,
  `kubeagent_slo_burn_rate`, and `kubeagent_slo_window_coverage_ratio`, each
  split by `window="fast"`/`"slow"`, plus the unlabelled
  `kubeagent_slo_target_ratio` and `kubeagent_slo_error_budget_remaining_ratio`
  (the latter over the slow window). The burn alert (`SLO`/`error-budget`,
  issue `ErrorBudgetBurn`) reuses the existing alert sink — same bounded
  queue, retries, and URL redaction — rather than the
  per-object tracker, so it never appears in `/issues` or `kubeagent_issues_*`.
  Off unless `--slo-target` is set (Helm: `slo.enabled` / `slo.target`), and the
  daemon remains strictly read-only with no new RBAC.

### Fixed

- `watch` SLO burn-rate: the availability SLI was reading `good`/`total` off
  the **display list** — the workloads `inventory.Prioritize` had already
  filtered down to what `scan` prints for a human, which drops every healthy,
  quiet workload. On a healthy cluster that list is empty, so `total` was 0,
  `slo.Tracker.Observe` treated the reconcile as having nothing to record, and
  window coverage never left zero: the burn-rate alert could not fire at all,
  no matter how bad an outage got. A second bug in the same computation scored
  a workload "good" whenever it had no `Findings`, while the display list (and
  the issue tracker) key off `Flagged()` — so a workload that was
  under-replicated or `Failed` but hadn't yet produced a Finding *raised*
  measured availability instead of lowering it. `inventory.Prioritize` now
  also computes an unfiltered census (`Good`/`Total`) over every long-running
  workload before any display filtering, Job and CronJob excluded, with `Good`
  meaning not `Flagged()`; the watch daemon feeds that to the SLI instead. No
  change to `scan`'s output or its `--output json` contract. If you deployed
  the SLO burn-rate feature, its alert was silently inert until this fix —
  worth confirming your window-coverage series is now climbing rather than
  flatlined at zero.

## [0.56.0] - 2026-07-25

### Added

- `watch` alerting: the daemon can POST one alert per broken object to a webhook
  (`json`, `slack`, or `alertmanager` format), keyed on the object rather than the
  issue so an evolving failure never reports a recovery while the workload is
  still broken. The URL comes from `KUBEAGENT_ALERT_WEBHOOK` and is never logged
  beyond `scheme://host`; `--alert-format` and `--alert-repeat` tune the rest.
  Delivery is a bounded queue with three attempts and counted drops
  (`kubeagent_alerts_sent_total`, `kubeagent_alerts_dropped_total`,
  `kubeagent_alert_last_success_timestamp_seconds`). Alerting is off unless the
  environment variable is set, and the daemon remains strictly read-only toward
  the cluster.

## [0.55.0] - 2026-07-25

### Added

- **Stateful `watch`.** The daemon now tracks issue state across reconciles instead of
  re-deriving the whole picture every cycle: each `(kind, namespace, name, issue)` is
  followed through its lifecycle, and the daemon logs only the transitions — `NEW`,
  `RESOLVED` (with how long it fired), and `FLAPPING` (repeated firings within a window) —
  plus a per-reconcile summary line; a reconcile with no transitions logs nothing, so
  steady state stays quiet. Ten new Prometheus series expose the same state
  (`kubeagent_issues_active`, `kubeagent_issues_flapping`, `kubeagent_issues_new_total`,
  `kubeagent_issues_resolved_total`, `kubeagent_issues_flapping_total`,
  `kubeagent_issues_dropped_total`, `kubeagent_issue_resolution_seconds_sum` /
  `_count` for mean time to resolution, and the per-issue `kubeagent_issue_active` /
  `kubeagent_issue_age_seconds`), and a new read-only `/issues` JSON endpoint lists every
  active and recently-resolved issue with its full history. State is in-memory only: on
  restart, everything currently firing is reported as `NEW` once and every counter resets
  from zero. Fixed, unconfigurable defaults (500 tracked issues, 1h resolved retention,
  30m flap window, 3 firings to flap) — no new flags. The daemon remains strictly
  read-only; the tracker performs no I/O of its own.

## [0.54.0] - 2026-07-25

### Added

- **`--fix` rollback.** `kubeagent scan --rollback --audit-log <path>` reads the most
  recent applied remediation from the audit log and undoes it — rolling a Deployment
  forward to its pre-fix revision, or re-cordoning a node — through the same guard rails
  as any fix: curated preview diff, `[y/N]` confirmation, a content-based drift bond
  (the target revision must still exist and the workload's containers must still carry
  the images the fix left, so a change made since the fix is never clobbered), RBAC
  preflight, and an audit record with the new `rollback` disposition. The inverse is derived deterministically from structured
  `fromRevision`/`toRevision` fields now written into every audit record; records
  written before v0.54 are refused with a clear message rather than guessed at. One
  action per invocation; `--rollback` requires `--audit-log` and cannot be combined
  with `--fix`.

## [0.53.0] - 2026-07-24

### Added

- **`--fix` RBAC preflight.** Before each guarded write, kubeagent runs a
  `SelfSubjectAccessReview` to confirm the current credentials may perform it
  (`update` on the target deployment/node). A denial refuses up front — `skipped: you
  lack permission to update deployments in namespace "shop" (RBAC); no write attempted`
  — recorded with a new `preflight` audit disposition, instead of a mid-apply 403. An
  SSAR API failure fails closed (no write, `error`). Under `--dry-run` the check runs
  read-only and reports whether each fix would be permitted. Needs no extra RBAC —
  self-review is granted to all authenticated users.

## [0.52.0] - 2026-07-24

### Added

- **`--fix` audit log.** A new `--audit-log <path>` flag (with `--fix`) appends a
  durable, append-only JSON-Lines record of every remediation outcome — one line per
  action with its timestamp, target, previewed changes, and disposition
  (`dry-run` / `declined` / `applied` / `refused` / `error`). Secret-free by
  construction (only the previewed diff values and result detail are recorded); the
  file is opened `0o600` and append-only, and an unwritable path fails before any
  write. The accountability half of the remediation contract.

## [0.51.0] - 2026-07-24

### Added

- **`--fix` diff preview + preview→apply contract.** Every proposed fix now shows a
  curated `will change:` diff (revision, per-container images, a safe count of other
  template changes — never env values or template contents) computed at plan time,
  and `Apply` is bound to the preview: if the cluster drifted since (a new rollout,
  the target revision gone), it refuses with `state changed since preview` and makes
  no write. With `--output json`, the plan is included as `remediationPlan`
  (status `proposed`) — the foundation for the coming audit log. Plan and apply now
  share one target-selection rule (highest prior revision with a differing template).

## [0.50.0] - 2026-07-23

### Added

- **Agentic `--investigate`.** After a scan, an opt-in bounded tool-use loop lets the
  model make read-only follow-up reads — describe an object, list its events, hop to a
  related owner/node/PVC — to chase a root cause across the finding's resource graph,
  then emits an `Investigation` section (evidence trail + the grounded fix). Findings-
  scoped, capped (8 reads / 6 turns), no logs, structured-only egress, never writes.
  Anthropic-only (`ANTHROPIC_API_KEY`); supersedes `--explain`.

## [0.49.0] - 2026-07-23

### Added

- **Local-model `--explain`.** Set `KUBEAGENT_EXPLAIN_ENDPOINT` (an OpenAI-compatible
  `/chat/completions` URL — Ollama, vLLM, llama.cpp, LM Studio) and `--explain` runs
  against that local model: no `ANTHROPIC_API_KEY`, and nothing leaves the network.
  `--model`/`KUBEAGENT_MODEL` names the local model; `KUBEAGENT_EXPLAIN_API_KEY` is an
  optional bearer token. Theme-C (principled intelligence) — offline/local explain.

## [0.48.0] - 2026-07-23

### Changed

- **`--explain` now ranks and grounds remediation.** The explanation opens with a
  `Fix first:` ordered remediation list, and each per-issue Fix is anchored to
  kubeagent's deterministic, pre-reviewed `--suggest` command — the model ranks,
  sequences, and phrases, but never invents or substitutes a command. The
  Theme-C (principled intelligence) LLM-ranking layer over the deterministic
  `--suggest` core; the deterministic offline core is unchanged.

## [0.47.0] - 2026-07-23

### Added

- **Admission-webhook latency risk.** `scan` flags a Fail-policy admission webhook
  whose `timeoutSeconds` is at or above 15 (env `KUBEAGENT_WEBHOOK_TIMEOUT_SECONDS`,
  Helm `webhookLatency.timeoutThreshold`) — a latency landmine that blocks every
  intercepted create/update for up to that long, then rejects it. Rendered
  `WebhookSlow`; the daemon exposes `kubeagent_admission_webhook_latency_risks`.
  Read-only, always-on, advisory. Closes the Theme-B admission-webhook line.

## [0.46.0] - 2026-07-23

### Added

- **DNS / CoreDNS resolution health (`--dns-health`).** An opt-in probe of each
  CoreDNS pod's `:9153/metrics` flags an elevated SERVFAIL+REFUSED response ratio
  (default ≥ 5% over a 100-response floor; env `KUBEAGENT_DNS_SERVFAIL_RATIO`) —
  catching DNS that is up but failing to resolve, which the CoreDNS-pod health
  check misses. Read-only; needs the `pods/proxy` add-on grant; the daemon exposes
  `kubeagent_dns_servfail_ratio`. Second of the Theme-B control-plane closers.

## [0.45.0] - 2026-07-23

### Added

- **Control-plane / etcd health (`--control-plane-health`).** An opt-in probe of
  the apiserver `/readyz?verbose` endpoint flags an unhealthy control plane —
  naming the failing checks (etcd, admission/controller poststarthooks,
  informer-sync). Read-only; needs the `/readyz` add-on grant; the daemon exposes
  `kubeagent_control_plane_unhealthy`. First of the Theme-B control-plane closers.

## [0.44.0] - 2026-07-23

### Added

- **ResourceQuota near-exhaustion.** `scan` flags a namespace's ResourceQuota
  entry whose usage is at or over 90% of its hard limit (env
  `KUBEAGENT_QUOTA_THRESHOLD` to tune), labelled `exhausted` (blocking new
  objects now) or `near limit` — the proactive complement to the reactive
  `FailedCreate` detector. Read-only, always-on; the daemon exposes
  `kubeagent_resourcequota_issues`. Adds a `resourcequotas` read grant.

## [0.43.0] - 2026-07-22

### Added

- **Stuck-rollout detection (`RolloutStuck`).** `scan` flags a Deployment whose
  rollout has wedged — its `Progressing` condition is
  `ProgressDeadlineExceeded`, or it carries a `ReplicaFailure` condition — so
  the new pods are not becoming available. Surfaced only when no pod-level
  finding already explains the failure (zero redundancy). Read-only, always-on;
  no new flag, metric, or RBAC.

## [0.42.0] - 2026-07-22

### Added

- **`--suggest` next steps.** An opt-in flag prints a deterministic, reviewed
  next-step suggestion and a read-only `kubectl` investigation command under each
  pod finding (CrashLoopBackOff → check the previous logs, ImagePullBackOff →
  verify the tag/credentials, …). Offline (no API key), never LLM-decided, and
  read-only — it prints the command, it never runs it.

## [0.41.0] - 2026-07-22

### Added

- **Missing-config detection (`CreateContainerConfigError`).** `scan` now flags a
  container (main or init) that can't start because a referenced ConfigMap or
  Secret is missing, or a required key is absent — naming the object from the
  kubelet message. Previously such a workload showed only as degraded with no
  explaining finding. Read-only (no new flag or metric).

## [0.40.0] - 2026-07-22

### Added

- **PVC provisioning root cause.** The Pending-PVC check now names *why* a claim
  is stuck by correlating it against the cluster's StorageClasses and PVs — it
  references a StorageClass that does not exist, or (for a static claim) no
  available PersistentVolume matches its size and access modes — and flags these
  even when no `ProvisioningFailed` event is present (catching a PVC whose event
  has expired). Read-only; reuses collected objects (no new flag or metric).

## [0.39.0] - 2026-07-22

### Added

- **Ingress-route root cause.** A broken ingress route (`… has no ready
  endpoints (likely 502/503)`) now names *why* its backend Service is empty —
  the selector matches no pods, the matching pods are on a down node, or none
  are Ready — so the 502 is explained on the route itself. Read-only; reuses the
  Service endpoint-cause logic (no new flag or metric).

## [0.38.0] - 2026-07-22

### Added

- **Service-no-endpoints root cause.** For a broken Service with no ready
  endpoints, `scan` now names *why* — the selector matches no pods, the matching
  pods are on a down node, or they exist but none are Ready — by correlating the
  selector against the collected pods and node health. Read-only; enriches the
  existing service finding (no new flag or metric).

## [0.37.0] - 2026-07-22

### Added

- **Admission-webhook-failure detection.** `scan` flags a Validating/Mutating
  webhook whose `failurePolicy` is `Fail` and whose backing Service is missing
  or has no ready endpoints — it would reject every create/update it intercepts.
  Read-only, advisory, and cluster-wide only (skipped under `--namespace`); the
  daemon exposes `kubeagent_admission_webhooks_failing`. Adds a base
  `admissionregistration.k8s.io` read grant.

## [0.36.0] - 2026-07-21

### Added

- **HPA-can't-scale detection.** `scan` flags a HorizontalPodAutoscaler that is
  stuck — can't fetch metrics (broken autoscaling), can't scale because its
  target is missing or the scale subresource errors, or is pinned at
  `maxReplicas` while demand exceeds the cap — naming the target and the reason.
  Read-only and advisory; the daemon exposes `kubeagent_hpa_scaling_issues`.
  Adds a base `autoscaling/horizontalpodautoscalers` read grant.

## [0.35.0] - 2026-07-21

### Added

- **PodDisruptionBudget-blocked drains.** `scan` flags a PDB that will block a
  node drain — one that can never allow a voluntary eviction, a stale zero-pod
  selector, or a PDB blocking evictions on an already-degraded workload —
  naming the rule and the guarded-pod counts. Read-only and advisory; the
  daemon exposes `kubeagent_pdb_blocking_issues`. Adds a base
  `policy/poddisruptionbudgets` read grant.

## [0.34.0] - 2026-07-21

### Added

- **Stuck-terminating / finalizer-deadlock check.** `scan` flags a Namespace, Pod,
  or PVC wedged in `Terminating` past two minutes and names the blocker — a
  namespace finalizer/condition, a pod's finalizers (or "deletion not confirmed"
  when the node is gone), or `pvc-protection` cross-referenced to the pod still
  mounting the PVC. Read-only and advisory (never removes a finalizer, never
  changes the verdict); the daemon exposes `kubeagent_resources_stuck_terminating`.
  Adds a base `namespaces` read grant.

## [0.33.0] - 2026-07-21

### Added

- **Per-finding confidence score.** Every finding now carries a confidence level —
  high for a directly Kubernetes-asserted state, medium for a kubeagent heuristic
  (`RestartLoop`, `ProbeFailure`) or a statistical correlation (a shared-registry
  attribution). The text report tags only the non-high findings and hints
  (`⚠ RestartLoop [medium]`, `↳ likely caused by registry … [medium]`); JSON
  always carries `"confidence"`. Informational — it never changes priority or the
  cluster verdict. Read-only, always-on, no new RBAC.

## [0.32.0] - 2026-07-21

### Added

- **Certificate-expiry check (opt-in `--certs`).** Flags expired and soon-expiring
  TLS certificates from `kubernetes.io/tls` Secrets — parsing only the public
  certificate, never the key — in an advisory CERTIFICATES section with the
  Ingress routes each cert fronts (`--cert-warn-days`, default 30). Daemon parity
  via `KUBEAGENT_CERTS` + `kubeagent_certificates_expired`/`_expiring` gauges and
  a separate secrets RBAC add-on (`deploy/rbac-certs.yaml` / Helm
  `certs.enabled`); without the flag kubeagent makes no Secrets API calls.

## [0.31.0] - 2026-07-21

### Added

- **Failed-PVC root-cause attribution.** A workload whose pod mounts a
  PersistentVolumeClaim that cannot provision or bind (the v0.26.0 Pending-PVC
  check) is now attributed to that PVC — "↳ likely caused by PVC reports-data
  (ProvisioningFailed)" — connecting a pod stuck Pending/ContainerCreating, which
  has no pod-level finding of its own, to the storage cause kubeagent already
  reports. One affected workload is enough (the PVC is independently diagnosed);
  node attribution takes precedence. Read-only, always-on, no new RBAC.

## [0.30.0] - 2026-07-21

### Added

- **Shared-registry root-cause attribution.** When two or more workloads fail
  image pulls from the same registry host, `scan` names that registry as the
  shared root cause on each ("↳ likely caused by registry ghcr.io (2 workloads
  failing to pull)") — the registry-outage / expired-credentials / rate-limit
  incident. A lone pull failure is never blamed on the registry, and node
  attribution takes precedence. The attention-line rollup now reads
  "(M ⇐ K root causes)" when causes mix. Read-only, always-on, no new RBAC.

## [0.29.0] - 2026-07-21

### Added

- **Node-anchored root-cause attribution.** When a node is hard-down (NotReady, or
  its kubelet stops heartbeating), `scan` attributes each workload with a pod on it
  to that node — a hedged "↳ likely caused by node X (reason)" line plus a rollup
  on the attention line — collapsing a wall of disconnected findings toward the one
  real cause. Additive (the workload's own findings still show), read-only,
  always-on, no new RBAC. The first step of the root-cause correlation roadmap.

## [0.28.2] - 2026-07-20

### Changed

- **Watch gauges exclude parked endpoints.** `kubeagent_service_issues` and
  `kubeagent_ingress_route_issues` now count only real problems, excluding
  intentionally-empty (Expected/parked) Services and routes — a backend scaled to zero or
  annotated `kubeagent.io/expected-empty: "true"` no longer inflates the alert gauges,
  matching how the `scan` report already treats them.

## [0.28.1] - 2026-07-20

### Added

- **Quiet intentionally-empty endpoints.** An Ingress route whose backend Service is empty
  on purpose — the backing workload is scaled to zero (or between runs), or the Service is
  annotated `kubeagent.io/expected-empty: "true"` — is now shown as a parked route in NOTES
  instead of a 502/503 in NEEDS ATTENTION. The annotation also quiets the bare Service
  finding, covering operator-managed role-split Services (e.g. CloudNativePG `-ro` on a
  single-instance cluster) that kubeagent can't infer. Read-only, always-on, no new RBAC.

## [0.28.0] - 2026-07-20

### Added

- **"Can't create pods" (FailedCreate) check.** `scan` now flags a workload stuck below its
  desired replicas because its controller cannot create pods — a `ResourceQuota`,
  `LimitRange`, or admission webhook is rejecting them — naming the cause on the workload
  (e.g. "blocked by a ResourceQuota") with the admission message as evidence. Covers
  Deployments (via their ReplicaSet), StatefulSets, and DaemonSets. Read-only, always-on,
  no new RBAC.

- **`kubeagent_pvc_pending_issues` watch metric.** The `watch` daemon now exposes a
  Prometheus gauge for the count of PersistentVolumeClaims stuck Pending because
  provisioning/binding failed (the v0.26.0 Pending-PVC check), so operators can alert on
  it alongside the existing `kubeagent_*_issues` gauges.

## [0.27.0] - 2026-07-20

### Added

- **Job / CronJob failure check.** `scan` flags a failed Job (`BackoffLimitExceeded` /
  `DeadlineExceeded`) and a CronJob whose most-recent run failed, naming the cause on the
  workload. A failing CronJob is now shown by default (healthy ones stay hidden behind
  `--include-cron`). Read-only, always-on, no new RBAC.

## [0.26.0] - 2026-07-19

### Added

- **Pending-PVC provisioning check.** `scan` flags a PersistentVolumeClaim stuck
  `Pending` because provisioning/binding failed (`ProvisioningFailed` / `FailedBinding`
  events), naming the cause and rendering it in NEEDS ATTENTION (and JSON `pvcIssues`).
  Event-based like `VolumeAttachError`, so the normal `WaitForFirstConsumer` state is
  never flagged. Read-only, always-on, no new RBAC; advisory (does not change the verdict).

## [0.25.0] - 2026-07-19

### Added

- **InitContainer failure detector.** `scan` flags a pod blocked in its init phase —
  `Init:CrashLoopBackOff`, `Init:ImagePullBackOff` / `Init:ErrImagePull`, or
  `Init:OOMKilled` — reading `InitContainerStatuses` (which the main-container crash
  detectors don't look at) and naming which init container is failing, its position,
  and why. Read-only, always-on, no new RBAC.

## [0.24.0] - 2026-07-19

### Added

- **ProbeFailure detector.** `scan` flags a Running-but-not-Ready pod whose readiness,
  liveness, or startup probe keeps failing, reading the kubelet's `Unhealthy` events and
  naming the probe, container, and a plain-language reason (`HTTP 503`, `connection
  refused`, `timed out`, …). Complementary to `RestartLoop`/`CrashLoopBackOff`.
  Read-only, always-on, **no new RBAC**. The raw probe message (which may carry a pod IP
  or `exec` output) is never surfaced, so `--explain` stays privacy-preserving.

## [0.23.0] - 2026-07-18

### Added

- **Crash log root-cause (opt-in).** `scan --logs` reads each crashing container's
  previous-instance logs (`pods/log`) and classifies the failure into a plain-language
  cause — `application panic (code bug)`, `cannot reach a dependency (…) — connection
  refused`, `bad command or entrypoint`, etc. — shown under the finding as
  `logs (previous container): … → <cause>` and in JSON as `logCause`/`logExcerpt`. Only
  the crash findings (CrashLoopBackOff / RestartLoop / OOMKilled) are probed. Read-only,
  scan-only; needs the `pods/log` grant (`deploy/rbac-logs.yaml`). `--explain` receives
  only the derived cause, never raw log text.

## [0.22.0] - 2026-07-15

### Changed

- **Node-reservation reporting is clearer and multi-resource.** `scan` now reports
  the combined kube+system reservation for **memory, CPU, and ephemeral-storage**
  in a labeled per-resource `CONTEXT` block (replacing the cryptic
  `Nodes 0/2 reserve memory OK` line). Reserving no **ephemeral-storage** now
  raises a `NOTES` warning alongside the existing no-memory warning (both are
  node-destabilizers); CPU is informational. Still read-only and advisory; the
  `watch` daemon and `kubeagent_nodes_without_reservations` gauge are unchanged.

- **Relicensed to MIT.** Replaced the Apache-2.0 `LICENSE` with the MIT License
  and removed the Apache-specific `NOTICE` file.

## [0.21.1] - 2026-07-14

### Added

- **Apache 2.0 license.** Added a `LICENSE` (Apache-2.0) and `NOTICE` file, making
  the project's open-source terms explicit.

### Changed

- **README.** Added a hero section with badges (CI, Go Report Card, release,
  license), a highlights list, and a `go install` quick-start.
- **Release packaging.** The release workflow now also publishes an unversioned
  `kubeagent_linux_amd64.tar.gz` asset, so
  `releases/latest/download/kubeagent_linux_amd64.tar.gz` always resolves to the
  newest release.

## [0.21.0] - 2026-07-14

### Added

- **Kubelet health probe.** Opt-in `scan --kubelet-health` probes each node's
  kubelet `/healthz` through the `nodes/proxy` subresource and flags a kubelet
  that is reachable but reporting unhealthy — `✗ node worker-2 kubelet /healthz
  unhealthy: [-]pleg failed` — the "alive but sick" case the lease-heartbeat and
  NotReady checks miss. Shown in a `KUBELET HEALTH` section and JSON
  `kubeletHealth`, with the watch gauge `kubeagent_kubelet_unhealthy` (set
  `KUBEAGENT_KUBELET_HEALTH`). Read-only and **advisory** (does not change the
  cluster verdict); reuses the same `nodes/proxy` add-on as `--disk-usage` (no
  new RBAC).

## [0.20.0] - 2026-07-13

### Added

- **Expected-node baseline.** Opt-in `scan --expected-nodes node-a,node-b,…`
  declares the node names you expect; kubeagent flags each declared node that has
  **no `Node` object** in the cluster — `node node-b expected but absent
  from the cluster` — catching a node that never registered or dropped out. It
  degrades the cluster verdict, and the watch daemon exposes
  `kubeagent_nodes_expected_absent` (set `KUBEAGENT_EXPECTED_NODES`). A node that
  exists but is `NotReady` counts as present (its health is flagged elsewhere);
  extra/unexpected nodes are not flagged. Read-only; no new RBAC; best on
  clusters with stable node names.

## [0.19.0] - 2026-07-13

### Added

- **Node heartbeat freshness.** `scan` reads each node's `Lease`
  (`kube-node-lease`) and flags a **Ready** node whose kubelet has stopped
  heartbeating — `kubelet not heartbeating (lease Ns stale)` — catching a dark
  kubelet in the window *before* the control plane marks the node `NotReady`.
  It degrades the cluster verdict, is tunable via `--node-heartbeat-threshold`
  (default `40s`), and the watch daemon exposes
  `kubeagent_nodes_stale_heartbeat` (set `KUBEAGENT_NODE_HEARTBEAT_THRESHOLD`) so
  you can alert before a node goes down. Reads `leases` (a new read-only RBAC
  grant); on by default.

## [0.18.0] - 2026-07-12

### Added

- **Workload security posture.** Opt-in `scan --security` flags PSS-aligned
  hardening problems — privileged/over-privileged containers (privileged, host
  namespaces, hostPath, hostPort, dangerous added capabilities), insecure
  defaults (runs as root, privilege escalation allowed, capabilities not
  dropped), and exposed Services (NodePort/LoadBalancer/externalIPs) — in a
  dedicated `SECURITY` section and JSON `securityIssues`, each labelled
  `baseline`/`restricted`/`kubeagent`. The `SECURITY` section is signal-first:
  it opens with a one-line tier summary, shows the dangerous `baseline` and
  exposed-service findings in full per workload, and folds the near-universal
  `restricted` hardening gaps into a per-check aggregate; pass
  `--security-verbose` to list every finding per workload. JSON `securityIssues`
  always contains all findings regardless of the flag. Read-only and advisory
  (does not change the cluster verdict); needs no new RBAC; excludes system
  namespaces by default.

## [0.17.0] - 2026-07-11

### Added

- **Ingress route health.** `scan` now resolves each Ingress rule's backend
  Service and flags broken routes — the backend Service is missing (`NoService`),
  has no ready endpoints (`NoEndpoints`, the classic 502/503), or does not expose
  the referenced port (`PortNotExposed`) — in the NEEDS ATTENTION section and JSON
  `ingressIssues`, with the watch-daemon gauge `kubeagent_ingress_route_issues`.
  This turns "why is my ingress returning 502?" into a concrete cause. Reads
  Ingresses (a new read-only RBAC grant); advisory (does not change the cluster
  verdict).

## [0.16.0] - 2026-07-09

### Changed

- **Root cause for NotReady nodes and findings.** A `NotReady` node now names its
  cause — the `NodeReady` condition's reason and message (e.g.
  `NotReady: KubeletNotReady — container runtime network not ready: cni config
  uninitialized`) — instead of a bare `NotReady`. And the text scan now prints
  each finding's underlying signal (`Finding.Evidence`) beneath it, so a pending
  pod shows the scheduler's message (`0/5 nodes are available: 3 Insufficient
  memory, …`) without needing `--output json` or `--explain`. Read-only; the
  cluster verdict and JSON schema are unchanged.

## [0.15.0] - 2026-07-09

### Added

- **Disk-usage check (opt-in).** `scan --disk-usage` reads each node's kubelet
  `/stats/summary` (via the `nodes/proxy` subresource) and flags node
  filesystems and PVCs at or over a threshold (`--disk-threshold`, default
  `0.80`) in the NEEDS ATTENTION section and JSON `diskUsage` — an early warning
  before the kubelet's `DiskPressure` eviction signal. Off by default (adds no
  RBAC); enable the daemon with `KUBEAGENT_DISK_USAGE=true` and the
  `nodes/proxy` add-on (`deploy/rbac-diskusage.yaml` or Helm
  `diskUsage.enabled=true`), which also exposes `kubeagent_node_fs_usage_ratio`
  and `kubeagent_volumes_over_disk_threshold`. Read-only; advisory (does not
  change the cluster verdict).

## [0.14.0] - 2026-07-08

### Changed

- **Redesigned `scan` text output.** The human-readable output is now organized
  by severity into **NEEDS ATTENTION** (failing workloads, dead Services,
  credential warnings), **NOTES** (advisories — Delete-policy PVCs, expected-empty
  Services, hidden-workload counts), and **CONTEXT** (nodes/reservations,
  resources, platform), with a workload-scoped "Needs attention" line under the
  cluster verdict. All-OK node reservations collapse to one line, and
  Delete-policy PVCs show as a grouped summary — pass `--pvc-reclaim` for the full
  per-PVC list. `--output json` is unchanged, and `--fix` behavior is unchanged.

## [0.13.0] - 2026-07-08

### Added

- **PVC reclaim-policy check.** `scan` now lists Bound PersistentVolumeClaims
  whose bound PersistentVolume has `reclaimPolicy: Delete` — the data-loss-prone
  case where deleting the PVC or PV destroys the underlying storage. Shown as a
  "PVCs with reclaim policy Delete" section (text + JSON `pvcReclaim`) and, in
  the watch daemon, as the gauge `kubeagent_pvcs_reclaim_delete`. Reads PVCs and
  their bound PVs (two new read-only RBAC grants); advisory only (does not change
  the cluster verdict).

## [0.12.0] - 2026-07-08

### Added

- **Node reservation check.** `scan` now reports each node's aggregate kubelet
  reservation (`Capacity − Allocatable`, i.e. kube-reserved + system-reserved +
  eviction-hard combined) and warns when a node reserves **no memory** —
  a kubelet that can be OOM'd under pressure. Shown as a "Node reservations"
  section (text + JSON `nodeReserve`) and, in the watch daemon, as the gauge
  `kubeagent_nodes_without_reservations`. Read-only; no new RBAC. Advisory only
  (does not change the cluster verdict).

- **Helm chart.** The in-cluster watch daemon is now packaged as a Helm chart
  under `deploy/helm/kubeagent/`, alongside the raw manifests. It renders the
  identical read-only RBAC (`get`/`list`/`watch` only), deployment, and metrics
  Service, with image, replicas, watch cadence, metrics port, RBAC/ServiceAccount
  creation, resources, security context, and scheduling exposed as values.

## [0.11.0] - 2026-07-07

### Added

- **Restart-loop detection.** A new `RestartLoop` finding flags a container that
  keeps exiting with a non-OOM error and restarting (`RestartCount ≥ 3`, current
  run younger than 10 min) even though it is currently `Running` — a flapping pod
  the point-in-time detectors (`CrashLoopBackOff`/`OOMKilled`) miss. Durable
  (reads `RestartCount` + `lastState.Terminated`), so it appears in the scan,
  `--explain`, and `kubeagent_findings{issue="RestartLoop"}`. Read-only.

## [0.10.0] - 2026-07-06

### Added

- **Volume-attach detection.** A new `VolumeAttachError` finding flags a pod stuck
  at container creation because a volume cannot be attached (`FailedAttachVolume`
  Warning event) — most often a **Multi-Attach** error (a ReadWriteOnce volume
  still attached to another node). Detected by reading the pod's events (one cheap
  field-selected List; the watch daemon needs no events informer). Read-only; the
  daemon's RBAC gains `events` read.

## [0.9.0] - 2026-07-05

### Added

- **Daemon watch mode (`kubeagent watch`).** Run kubeagent in-cluster as a
  strictly read-only daemon: it watches the cluster via informers, re-runs the
  deterministic diagnosis on change (debounced) plus a heartbeat, and exposes the
  result as structured logs and hand-rolled Prometheus `/metrics` (with `/healthz`
  and `/readyz`). No cluster writes, no LLM calls, no new dependency. Read-only
  RBAC and Deployment manifests are in `deploy/`. (Multi-cluster, Kubernetes
  Events, `--explain`, and autonomous remediation are planned for later phases.)
- **Dockerfile.** A multi-stage build producing a small distroless, non-root
  image for running the daemon in-cluster (used by `deploy/deployment.yaml`).

## [0.8.0] - 2026-07-04

### Added

- **"What changed" rollout awareness.** A flagged Deployment now shows its most
  recent rollout when it is recent (within 7 days) — the revision, its age, and
  the image delta (`↳ changed: rollout to revision 6, 4d ago · image A → B`) — in
  text, JSON (`rollout`), and `--explain`. Deterministic and read-only (reuses
  the ReplicaSets already collected); factual, with no causal claim.

### Changed

- **`--fix` `RolloutUndo` is more conservative.** A Deployment rollback is now
  proposed only when the Deployment is **degraded** (fewer ready replicas than
  desired). A rollout stuck on `ImagePullBackOff` while its previous revision is
  still fully serving is left alone (the failure still shows in the scan and
  `--explain`; only the automatic rollback proposal is withheld).

## [0.7.0] - 2026-07-01

### Added

- **`--fix` remediation: `Uncordon`.** A second guard-railed action — an
  accidentally-cordoned node (`SchedulingDisabled`, no `NoExecute` taint) is made
  schedulable again after a per-action confirmation. Same rails as `RolloutUndo`
  (allowlist, apply-time precondition re-check, single write, never LLM-decided).

### Changed

- **Sharper `--explain`.** The `--explain` prompt now instructs a consistent,
  scannable structure (per issue: root cause → checks → fix; cluster/kube-system
  problems before workloads) and is grounded strictly in the scan's facts (told
  not to invent causes), reducing misattributed root causes. Still opt-in,
  read-only, structured-facts-only, and independent of `--fix`.

## [0.6.0] - 2026-07-01

### Added

- **`--fix` remediation (opt-in).** `scan --fix` proposes and, after a per-action
  `[y/N]` confirmation, applies safe reversible remediations (`--dry-run` to
  preview, `--yes` for non-interactive). v1 ships `RolloutUndo` (roll a Deployment
  with a failed image rollout back to its previous revision). Guard-railed:
  allowlist, protected namespaces, apply-time precondition re-check, re-verify;
  never LLM-decided. This is the first feature that can write to the cluster;
  default behavior remains read-only.

## [0.5.0] - 2026-06-30

No changes to the `kubeagent` binary since 0.4.0 — this release adds project
infrastructure (a documentation site and a pre-release chaos-test harness).

### Added

- **Documentation website.** A MkDocs + Material site (landing page, quickstart,
  per-feature docs, install, roadmap), published to GitHub Pages at
  [k8sproject.top](https://k8sproject.top) via a `pages.yml` workflow.
- **Pre-release chaos-test harness.** `chaos/run.sh` spins up a disposable Kind
  cluster (Calico CNI), injects the 10 most common production outages, runs
  `kubeagent scan` against each (adding `--explain` when `ANTHROPIC_API_KEY` is
  set), and writes a results report — a manual gate before each release, wired
  into the release checklist. See `chaos/README.md`.

## [0.4.0] - 2026-06-30

### Added

- **Service backing awareness.** A "no ready endpoints" Service issue is now
  annotated with its backing workload when that workload expects no pods — a
  CronJob/Job, or a DaemonSet/Deployment/StatefulSet scaled to 0 — so these stop
  reading as primary problems (text + JSON `expected`/`backing`). A
  Deployment/StatefulSet with replicas and no endpoints stays a primary issue.

### Fixed

- **Credential lint precision.** `--lint-secrets` no longer flags `*_FILE` env
  vars (which hold a path to a secret file, not the secret itself) or values that
  are dotted version numbers — removing two false-positive classes found in live
  use. Real secret values in `*_FILE`-named vars are still flagged.

## [0.3.0] - 2026-06-29

### Added

- **Connectivity diagnostics.** An unreachable or broken API server now yields an
  actionable diagnosis (down / timeout / TLS-cert / auth / DNS) with a `details:`
  line, instead of a raw transport error.
- **NetworkPolicy awareness.** A degraded workload with no detector finding is
  annotated with the NetworkPolicies selecting its pods (a root-cause hint), in
  text, JSON, and `--explain`.
- **Service / LoadBalancer health.** `scan` flags selector-based Services with no
  ready endpoints and LoadBalancer Services with no external address, in a new
  "Service issues" section (text + JSON) and in `--explain`.
- **Credential lint (opt-in).** `scan --lint-secrets` flags credentials stored in
  the clear (ConfigMap values, pod env literals) by location and pattern — never
  the value, and never sent to `--explain`.

## [0.2.0] - 2026-06-29

### Added

- **Resource context.** OOMKilled findings now show the killed container's CPU
  and memory requests + limits. `scan` also prints a cluster resource summary
  (CPU/memory: allocatable, reserved/requests, limits, and — when metrics-server
  is present — live usage) in text and JSON, and feeds it to `--explain` so the
  model can judge whether to raise a limit or scale out. Live usage is
  best-effort and degrades gracefully when metrics-server is absent.
- **Platform facts.** A second line under the cluster verdict naming the detected
  stack — CNI, ingress, storage provisioner(s), Kubernetes version + distribution,
  container runtime, and cloud — also in JSON (`platform`) and in `--explain`. No
  instance identifiers (e.g. raw `providerID`) are emitted.

### Fixed

- Completed/failed **bare pods** (e.g. a one-shot `kubectl run` pod in
  `Succeeded`/`Completed`) are no longer reported as `Degraded`. A pod-derived
  workload in a terminal phase is now treated like a finished Job (`Complete` /
  `Failed`) instead of being run through the ready/desired health model.
- The release `scan` now emits a warning when a metrics-server response is
  present but malformed (previously silently discarded).

## [0.1.0] - 2026-06-27

### Added

- Initial release. `kubeagent scan`: a read-only, prioritized cluster problem
  report — a P1 cluster-health verdict (nodes + `kube-system`) followed by P2
  workload/pod failures, in text or JSON.
- Deterministic detectors: CrashLoopBackOff, ImagePullBackOff/ErrImagePull,
  OOMKilled, Pending/Unschedulable.
- Workload inventory grouped by controller (Deployment / StatefulSet / DaemonSet /
  Job / CronJob / bare pod) with restart history; `--include-restarts` and
  `--include-cron` opt-ins.
- Optional `--explain` flag: a single Claude API call (official Go SDK)
  summarizing findings in plain English; the deterministic core stays usable
  offline. Model selectable via `--model` / `KUBEAGENT_MODEL`.
- `kubeagent version` subcommand (stamped at release time).
- CI (vet/test/build on push & PR) and a release workflow publishing a
  linux/amd64 tarball + `SHA256SUMS` to a GitHub Release.

[Unreleased]: https://github.com/imantaba/kubeagent/compare/v1.16.1...HEAD
[1.16.1]: https://github.com/imantaba/kubeagent/compare/v1.16.0...v1.16.1
[1.16.0]: https://github.com/imantaba/kubeagent/compare/v1.15.0...v1.16.0
[1.15.0]: https://github.com/imantaba/kubeagent/compare/v1.14.0...v1.15.0
[1.14.0]: https://github.com/imantaba/kubeagent/compare/v1.13.1...v1.14.0
[1.13.1]: https://github.com/imantaba/kubeagent/compare/v1.13.0...v1.13.1
[1.13.0]: https://github.com/imantaba/kubeagent/compare/v1.12.0...v1.13.0
[1.12.0]: https://github.com/imantaba/kubeagent/compare/v1.11.0...v1.12.0
[1.11.0]: https://github.com/imantaba/kubeagent/compare/v1.10.0...v1.11.0
[1.10.0]: https://github.com/imantaba/kubeagent/compare/v1.9.0...v1.10.0
[1.9.0]: https://github.com/imantaba/kubeagent/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/imantaba/kubeagent/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/imantaba/kubeagent/compare/v1.6.0...v1.7.0
[1.6.0]: https://github.com/imantaba/kubeagent/compare/v1.5.0...v1.6.0
[1.5.0]: https://github.com/imantaba/kubeagent/compare/v1.4.0...v1.5.0
[1.4.0]: https://github.com/imantaba/kubeagent/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/imantaba/kubeagent/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/imantaba/kubeagent/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/imantaba/kubeagent/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/imantaba/kubeagent/compare/v0.74.0...v1.0.0
[0.74.0]: https://github.com/imantaba/kubeagent/compare/v0.73.0...v0.74.0
[0.73.0]: https://github.com/imantaba/kubeagent/compare/v0.72.0...v0.73.0
[0.72.0]: https://github.com/imantaba/kubeagent/compare/v0.71.0...v0.72.0
[0.71.0]: https://github.com/imantaba/kubeagent/compare/v0.70.0...v0.71.0
[0.70.0]: https://github.com/imantaba/kubeagent/compare/v0.69.0...v0.70.0
[0.69.0]: https://github.com/imantaba/kubeagent/compare/v0.68.0...v0.69.0
[0.68.0]: https://github.com/imantaba/kubeagent/compare/v0.67.0...v0.68.0
[0.67.0]: https://github.com/imantaba/kubeagent/compare/v0.66.0...v0.67.0
[0.66.0]: https://github.com/imantaba/kubeagent/compare/v0.65.0...v0.66.0
[0.65.0]: https://github.com/imantaba/kubeagent/compare/v0.64.0...v0.65.0
[0.64.0]: https://github.com/imantaba/kubeagent/compare/v0.63.0...v0.64.0
[0.63.0]: https://github.com/imantaba/kubeagent/compare/v0.62.0...v0.63.0
[0.62.0]: https://github.com/imantaba/kubeagent/compare/v0.61.0...v0.62.0
[0.61.0]: https://github.com/imantaba/kubeagent/compare/v0.60.0...v0.61.0
[0.60.0]: https://github.com/imantaba/kubeagent/compare/v0.59.0...v0.60.0
[0.59.0]: https://github.com/imantaba/kubeagent/compare/v0.58.1...v0.59.0
[0.58.1]: https://github.com/imantaba/kubeagent/compare/v0.58.0...v0.58.1
[0.58.0]: https://github.com/imantaba/kubeagent/compare/v0.57.0...v0.58.0
[0.57.0]: https://github.com/imantaba/kubeagent/compare/v0.56.0...v0.57.0
[0.56.0]: https://github.com/imantaba/kubeagent/compare/v0.55.0...v0.56.0
[0.55.0]: https://github.com/imantaba/kubeagent/compare/v0.54.0...v0.55.0
[0.54.0]: https://github.com/imantaba/kubeagent/compare/v0.53.0...v0.54.0
[0.53.0]: https://github.com/imantaba/kubeagent/compare/v0.52.0...v0.53.0
[0.52.0]: https://github.com/imantaba/kubeagent/compare/v0.51.0...v0.52.0
[0.51.0]: https://github.com/imantaba/kubeagent/compare/v0.50.0...v0.51.0
[0.50.0]: https://github.com/imantaba/kubeagent/compare/v0.49.0...v0.50.0
[0.49.0]: https://github.com/imantaba/kubeagent/compare/v0.48.0...v0.49.0
[0.48.0]: https://github.com/imantaba/kubeagent/compare/v0.47.0...v0.48.0
[0.47.0]: https://github.com/imantaba/kubeagent/compare/v0.46.0...v0.47.0
[0.46.0]: https://github.com/imantaba/kubeagent/compare/v0.45.0...v0.46.0
[0.45.0]: https://github.com/imantaba/kubeagent/compare/v0.44.0...v0.45.0
[0.44.0]: https://github.com/imantaba/kubeagent/compare/v0.43.0...v0.44.0
[0.43.0]: https://github.com/imantaba/kubeagent/compare/v0.42.0...v0.43.0
[0.42.0]: https://github.com/imantaba/kubeagent/compare/v0.41.0...v0.42.0
[0.41.0]: https://github.com/imantaba/kubeagent/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/imantaba/kubeagent/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/imantaba/kubeagent/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/imantaba/kubeagent/compare/v0.37.0...v0.38.0
[0.37.0]: https://github.com/imantaba/kubeagent/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/imantaba/kubeagent/compare/v0.35.0...v0.36.0
[0.35.0]: https://github.com/imantaba/kubeagent/compare/v0.34.0...v0.35.0
[0.34.0]: https://github.com/imantaba/kubeagent/compare/v0.33.0...v0.34.0
[0.33.0]: https://github.com/imantaba/kubeagent/compare/v0.32.0...v0.33.0
[0.32.0]: https://github.com/imantaba/kubeagent/compare/v0.31.0...v0.32.0
[0.31.0]: https://github.com/imantaba/kubeagent/compare/v0.30.0...v0.31.0
[0.30.0]: https://github.com/imantaba/kubeagent/compare/v0.29.0...v0.30.0
[0.29.0]: https://github.com/imantaba/kubeagent/compare/v0.28.2...v0.29.0
[0.28.2]: https://github.com/imantaba/kubeagent/compare/v0.28.1...v0.28.2
[0.28.1]: https://github.com/imantaba/kubeagent/compare/v0.28.0...v0.28.1
[0.28.0]: https://github.com/imantaba/kubeagent/compare/v0.27.0...v0.28.0
[0.27.0]: https://github.com/imantaba/kubeagent/compare/v0.26.0...v0.27.0
[0.26.0]: https://github.com/imantaba/kubeagent/compare/v0.25.0...v0.26.0
[0.25.0]: https://github.com/imantaba/kubeagent/compare/v0.24.0...v0.25.0
[0.24.0]: https://github.com/imantaba/kubeagent/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/imantaba/kubeagent/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/imantaba/kubeagent/compare/v0.21.1...v0.22.0
[0.21.1]: https://github.com/imantaba/kubeagent/compare/v0.21.0...v0.21.1
[0.21.0]: https://github.com/imantaba/kubeagent/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/imantaba/kubeagent/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/imantaba/kubeagent/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/imantaba/kubeagent/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/imantaba/kubeagent/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/imantaba/kubeagent/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/imantaba/kubeagent/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/imantaba/kubeagent/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/imantaba/kubeagent/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/imantaba/kubeagent/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/imantaba/kubeagent/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/imantaba/kubeagent/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/imantaba/kubeagent/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/imantaba/kubeagent/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/imantaba/kubeagent/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/imantaba/kubeagent/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/imantaba/kubeagent/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/imantaba/kubeagent/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/imantaba/kubeagent/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/imantaba/kubeagent/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/imantaba/kubeagent/releases/tag/v0.1.0
