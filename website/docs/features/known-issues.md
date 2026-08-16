# Known issues reference

A `scan` names what is wrong: `CrashLoopBackOff`, `Init:OOMKilled`,
`VolumeAttachError`. `kubeagent known-issues` says what those names mean —
what the failure actually is, what usually causes it, and what to read next —
without a cluster, a kubeconfig, or a network connection.

```bash
kubeagent known-issues            # every documented kind, one line each
kubeagent known-issues OOMKilled  # one kind in full
```

```text
$ kubeagent known-issues
Failure kinds kubeagent's pod and workload detectors report:
  ContainerStartError          the container was created but could not be started
  CrashLoopBackOff             a container starts, exits, and is restarted on a widening backoff
  CreateContainerConfigError   the kubelet cannot build the container from its spec
  ErrImagePull                 the kubelet's attempt to pull the image failed
  ImagePullBackOff             repeated pull failures, now backing off between attempts
  Init:CrashLoopBackOff        an init container is crash-looping, so the pod never starts
  Init:CreateContainerConfigError the kubelet cannot build an init container from its spec
  Init:ErrImagePull            an init container's image could not be pulled
  Init:ImagePullBackOff        an init container's image pull is backing off
  Init:OOMKilled               an init container was killed for exceeding its memory limit
  OOMKilled                    the kernel killed a container for exceeding its memory limit
  ProbeFailure                 a container is running but a probe keeps failing
  RestartLoop                  a container keeps exiting and restarting while still Running
  Unschedulable                no node can place the pod
  VolumeAttachError            a volume cannot be attached, so the container never starts
  VolumeMountError             a volume cannot be mounted, so the container never starts

Print one:
  kubeagent known-issues <kind>

The kubeagent watch daemon additionally reports cluster-level and certificate
issue kinds that this reference does not document.
```

```text
$ kubeagent known-issues OOMKilled
OOMKilled
  The kernel killed a container for exceeding its memory limit.

Likely causes
  - The limit is lower than the workload's real steady-state usage.
  - A leak: usage climbs until the limit is reached, then repeats on a
    cycle.
  - A runtime heap sized above the container limit, so the runtime never
    reclaims before the kernel intervenes.

What to check
  - kubectl -n <namespace> describe pod <pod> — lastState.terminated.exitCode 137
  - kubectl -n <namespace> top pod <pod> — usage against the configured limit
  - The container's own memory tuning against resources.limits.memory

  https://k8sproject.top/features/diagnostics/#oomkilled
```

## Guarantees

`kubeagent known-issues` **touches no cluster at all.** It is not merely
read-only: there is no client, no context, and no kubeconfig on this path —
the command takes no flags, not even `--kubeconfig`.

Separately and additionally, it **makes no LLM call.** The text is curated
prose compiled into the binary. Nothing is generated at run time and nothing
is sent anywhere. Those are two different promises and neither implies the
other — `--explain` is the model path, and this is not a smaller version of
it.

## The vocabulary is closed

`kubeagent known-issues` documents exactly the sixteen kinds the
deterministic detector set can report, and the repository checks that rather
than asserting it. Four tests in `internal/diagnose` run on every `go test`:

- a `go/parser` walk over the detector sources, checking every string literal
  that reaches a finding's issue field;
- a fixture table that drives all eleven detectors to produce all sixteen
  kinds and looks each one up in the registry — this is what covers the kinds
  composed at run time, which the parser cannot see;
- the reverse check, refusing an entry for a kind no detector emits;
- a second parser walk for the two sites that build a kind from a runtime
  value rather than a literal. It reads the *guards* instead of the output:
  every string those functions test a `.Reason` field against is composed
  with the site's prefix and looked up. Widening a guard therefore fails the
  suite immediately, which the fixture table alone would not — a fixture only
  covers the path someone remembered to write.

That fourth walk understands a deliberately small set of shapes — an `==`
comparison against a `.Reason` field, or a `switch` on one, in both cases
against a string literal; a bare `.Reason` as the kind, or a literal prefix
added to one — and **refuses** anything else rather than ignoring it. A guard
rewritten into a shape it cannot read fails the suite by name, exactly as
widening one does, and so does a guard that compares against a named constant
instead of a literal: reading half a guard and reporting only that half would be
the quieter kind of wrong.

Refusing is only worth something if nothing can slip past unnoticed, so the
question is also asked from the other side. A value reaches that field in one of
three ways, and each has its own check:

- **the field is named** — every occurrence of `Issue` in the package must sit
  where the walk reads it, as a key in a composite literal or the left side of
  an assignment, inside a function declaration whose guards it can see. Anything
  else is named and refused, a plain read included, and so is a second type
  declaring an `Issue` field of its own — Go converts between structs whose
  fields match, so that one could be built positionally and converted;
- **the field is not named** — a finding written positionally has no `Issue`
  token for any of that to match, so a positional literal is refused outright,
  as is a second name for the type itself, `type f = Finding`, which would give
  one a type name the check does not recognise;
- **syntax is bypassed** — an import can write the field without the writer
  naming it. `reflect` and `unsafe` are the obvious two, but
  `json.Unmarshal(payload, &f)` does it as readily and imports neither, and so
  would the next decoder anyone reached for. So the detectors' import set is
  **pinned** rather than filtered: six packages, and a seventh fails the test
  until someone widens the list on purpose.

That third check earns its keep twice over these sources: a detector that
imported something to hand it back a ready-made finding would be reaching past
the walk as surely as a decoder does, and the pin refuses that import too. What
a pinned import set cannot do is constrain a package that imports
`internal/diagnose` and builds a finding of its own. Nothing here tries to.

Within `internal/diagnose` there is no fourth way. Outside it there is — that
last shape is not an evasion but the ordinary way `scan`'s workload passes
report, and it is why the boundary below is drawn where it is. The closure is
over the pod-level detectors, which is what the reference documents; it was
never a claim about every finding kubeagent can print.

Each of those checks was added after a shape slipped through: a `switch` that
admitted a fourteenth kind, a kind assigned on the line after the finding was
built, a finding written with no field names, a second struct of the same shape
converted to one, a decoder. Every one of them left all four tests green, which
is precisely the failure mode — a walk that skips what it does not understand
reports nothing, and three sibling tests agree with it.

Adding a detector that emits a new kind fails the build's tests until the kind
is documented. That is the point of the slice: the reference cannot drift from
the code.

The honest boundary: these tests cover `internal/diagnose`, the pod-level
detectors. Other packages report their own findings — `FailedCreate`,
`RolloutStuck`, `JobFailed` — and those are deliberately outside this
vocabulary, with their prose on the [Failure diagnostics](diagnostics.md)
page.

Those three are named, though, so that asking about one gets an honest answer
rather than "unknown" — see [Asking about a workload-level
kind](#asking-about-a-workload-level-kind) below. That second list is closed
the same way, and by the same kind of test: a fifth check reads the
`Issue:` literals the workload passes build and fails if they are not exactly
the three names. It cannot live beside the other four — they are scoped to
`internal/diagnose` on purpose — so it lives where those passes are in scope.

## What a kind is

The `Kind` is the exact issue string a scan prints, copied verbatim rather
than restated more prettily, because it is the join between the two outputs.
Lookup is exact — no case folding, no fuzzy match, and no falling back from
`Init:OOMKilled` to `OOMKilled`:

```text
$ kubeagent known-issues oomkilled
kubeagent: unknown issue kind "oomkilled"; kubeagent documents the deterministic detector set (ContainerStartError, CrashLoopBackOff, CreateContainerConfigError, ErrImagePull, ImagePullBackOff, Init:CrashLoopBackOff, Init:CreateContainerConfigError, Init:ErrImagePull, Init:ImagePullBackOff, Init:OOMKilled, OOMKilled, ProbeFailure, RestartLoop, Unschedulable, VolumeAttachError, VolumeMountError). Other findings are explained at https://k8sproject.top/features/diagnostics/
```

An init container killed for memory blocks the pod from ever starting. That is
a different failure from the same reason on a main container, with different
causes and different next steps, so it is a different entry.

## Asking about a workload-level kind

`RolloutStuck`, `FailedCreate` and `JobFailed` have no entry here, and they are
not typos either — kubeagent may have printed one a second earlier. They get
their own answer:

```text
$ kubeagent known-issues RolloutStuck
kubeagent: "RolloutStuck" is a workload-level finding, not one of the pod detectors this reference covers; it is explained at https://k8sproject.top/features/diagnostics/
```

The exit code does not change: naming one of the three is still not a lookup
that found an entry, so it exits non-zero exactly as an unknown argument does.
Only the wording changes, and only for those three. A typo keeps the message
above verbatim, because **unknown** is the correct word for a typo and blurring
the two cases into one softer message would lose that.

## What the entries may name

Every command line in a **What to check** section uses placeholders —
`<namespace>`, `<pod>`, `<container>`, `<node>`, `<name>` — never a real
object name. The only host that appears anywhere in the reference is the
project's own documentation site, in the per-entry link, and a test asserts
that no address or hostname reaches the prose.

## Not in this slice

Deliberately absent:

- **Entries for kinds outside the detector set.** `NoEndpoints`,
  `RolloutStuck`, `JobFailed`, `FailedCreate` and the rest are real findings
  from the workload and cluster passes, and none has an entry here. They keep
  their prose in [Failure diagnostics](diagnostics.md). The three workload
  kinds are at least named, so asking about one is [answered
  honestly](#asking-about-a-workload-level-kind) rather than called unknown;
  the cluster-pass kinds are not, because they are not statically enumerable
  the way the other two lists are, so a list of them could not carry the same
  guarantee.
- **The watch daemon's own issue kinds.** `kubeagent watch` additionally
  reports cluster-level and certificate issue kinds of its own — an
  unhealthy control plane, a degraded DNS check, an expired, expiring or
  invalid certificate among them — that this reference does not document
  and does not name, unlike the three workload kinds above. The daemon's
  vocabulary is a candidate for its own closure test, in its own slice; for
  now the listing discloses the gap rather than leaving it unsaid.
- **A link from `scan` output to an entry.** `scan`'s rendering is unchanged.
- **JSON output.** This is a reference for a person, not a document to
  forward, so it adds no ninth [versioned document](json-schema.md).
- **Operator-supplied entries.** The registry ships with the binary; it is
  curated, not extensible at run time.
