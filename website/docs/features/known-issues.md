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
  CrashLoopBackOff             a container starts, exits, and is restarted on a widening backoff
  CreateContainerConfigError   the kubelet cannot build the container from its spec
  ErrImagePull                 the kubelet's attempt to pull the image failed
  ImagePullBackOff             repeated pull failures, now backing off between attempts
  Init:CrashLoopBackOff        an init container is crash-looping, so the pod never starts
  Init:ErrImagePull            an init container's image could not be pulled
  Init:ImagePullBackOff        an init container's image pull is backing off
  Init:OOMKilled               an init container was killed for exceeding its memory limit
  OOMKilled                    the kernel killed a container for exceeding its memory limit
  ProbeFailure                 a container is running but a probe keeps failing
  RestartLoop                  a container keeps exiting and restarting while still Running
  Unschedulable                no node can place the pod
  VolumeAttachError            a volume cannot be attached, so the container never starts

Print one:
  kubeagent known-issues <kind>
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

`kubeagent known-issues` documents exactly the thirteen kinds the
deterministic detector set can report, and the repository proves it rather
than asserting it. Three tests in `internal/diagnose` run on every `go test`:

- a `go/parser` walk over the detector sources, checking every string literal
  that reaches a finding's issue field;
- a fixture table that drives all nine detectors to produce all thirteen
  kinds and looks each one up in the registry — this is what covers the kinds
  composed at run time, which the parser cannot see;
- the reverse check, refusing an entry for a kind no detector emits.

Adding a detector that emits a new kind fails the build's tests until the kind
is documented. That is the point of the slice: the reference cannot drift from
the code.

## What a kind is

The `Kind` is the exact issue string a scan prints, copied verbatim rather
than restated more prettily, because it is the join between the two outputs.
Lookup is exact — no case folding, no fuzzy match, and no falling back from
`Init:OOMKilled` to `OOMKilled`:

```text
$ kubeagent known-issues oomkilled
kubeagent: unknown issue kind "oomkilled"; kubeagent documents the deterministic detector set (CrashLoopBackOff, CreateContainerConfigError, ErrImagePull, ImagePullBackOff, Init:CrashLoopBackOff, Init:ErrImagePull, Init:ImagePullBackOff, Init:OOMKilled, OOMKilled, ProbeFailure, RestartLoop, Unschedulable, VolumeAttachError). Other findings are explained at https://k8sproject.top/features/diagnostics/
```

An init container killed for memory blocks the pod from ever starting. That is
a different failure from the same reason on a main container, with different
causes and different next steps, so it is a different entry.

## What the entries may name

Every command line in a **What to check** section uses placeholders —
`<namespace>`, `<pod>`, `<container>`, `<node>`, `<name>` — never a real
object name. The only host that appears anywhere in the reference is the
project's own documentation site, in the per-entry link, and a test asserts
that no address or hostname reaches the prose.

## Not in this slice

Deliberately absent:

- **Kinds outside the detector set.** `NoEndpoints`, `RolloutStuck`,
  `JobFailed`, `FailedCreate` and the rest are real findings from the workload
  and cluster passes. They are not statically enumerable the way the detector
  set is, so documenting them here could not carry the same guarantee. They
  keep their prose in [Failure diagnostics](diagnostics.md).
- **A link from `scan` output to an entry.** `scan`'s rendering is unchanged.
- **JSON output.** This is a reference for a person, not a document to
  forward, so it adds no ninth [versioned document](json-schema.md).
- **Operator-supplied entries.** The registry ships with the binary; it is
  curated, not extensible at run time.
