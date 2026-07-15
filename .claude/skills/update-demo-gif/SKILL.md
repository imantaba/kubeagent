---
name: update-demo-gif
description: Regenerate the kubeagent scan demo GIF shown on the README (docs/kubeagent-demo.gif). Spins up a throwaway Kind cluster, injects the standard broken workloads (CrashLoopBackOff, ImagePullBackOff, OOMKilled, and a dead Service), verifies a real `kubeagent scan` shows them, records the terminal with VHS via docs/kubeagent-demo.tape, and checks the result. Use this whenever the user wants to update, refresh, re-record, or fix the demo GIF / README animation / terminal recording — especially after any change to `scan`'s output makes the current GIF stale — even if they just say "redo the gif", "the demo looks old", or "re-record the scan".
---

# Updating the kubeagent demo GIF

The README hero image (`docs/kubeagent-demo.gif`, referenced from `README.md`) is a
terminal recording of `kubeagent scan` against a deliberately-broken cluster. It goes
stale whenever `scan`'s output changes, so this skill regenerates it reproducibly.

**Three committed artifacts define the demo — keep them in sync:**

- `docs/kubeagent-demo.tape` — the [VHS](https://github.com/charmbracelet/vhs) script
  that drives the recording (typing, timing, theme). It runs `kubeagent scan` and writes
  `docs/kubeagent-demo.gif`.
- `.claude/skills/update-demo-gif/assets/faults.yaml` — the broken workloads to inject.
- `.claude/skills/update-demo-gif/scripts/verify-findings.sh` — the pre-record check
  that the cluster is actually showing every failure before we record.

## Why a lightweight Kind cluster (not the chaos harness)

The `chaos/` harness reverts each fault after scanning, so it never leaves a persistently
broken cluster to record — and its Calico CNI is slow and flaky. For the GIF we want a
fast, reliable cluster with the failures left *in place*, so this uses a plain 2-node
Kind cluster (default kindnet CNI, ready in seconds) with `faults.yaml` applied and never
reverted. The failure *types* mirror what the chaos scenarios inject.

## Prerequisites

`vhs` (with `ttyd` + `ffmpeg`), `kind`, `kubectl`, `docker`, and `go` must be on PATH.
If `vhs` is missing, install it — on this machine Homebrew pulls it with its deps:
`brew install vhs` (installs `vhs`, `ttyd`, `ffmpeg` together). Confirm with `vhs --version`.

## Steps

Run everything from the repo root. `$SKILL` below is this skill's directory
(`.claude/skills/update-demo-gif`).

### 1. Build kubeagent and put it on PATH

The tape's `Require kubeagent` needs the binary discoverable by name:

```bash
export PATH=$PATH:/usr/local/go/bin
go build -o kubeagent . && cp kubeagent ~/.local/bin/kubeagent   # ~/.local/bin is on PATH
kubeagent version
```

### 2. Create the throwaway cluster

```bash
export PATH=$PATH:$HOME/.local/bin
kind create cluster --name kubeagent-demo --wait 90s \
  --config <(printf 'kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n  - role: control-plane\n  - role: worker\n')
```

### 3. Inject the faults and make the demo cluster the current context

The tape runs a bare `kubeagent scan` (no `--context`) so the recording looks clean, so
the demo cluster must be the **current** context:

```bash
kubectl --context kind-kubeagent-demo apply -f "$SKILL/assets/faults.yaml"
kubectl config use-context kind-kubeagent-demo
```

### 4. Verify the failures are live — the pre-record test

Kubernetes needs ~30–60s to reach CrashLoopBackOff / ImagePullBackOff / OOMKilled. Do
**not** record until they're all present, or the GIF captures a half-started cluster. The
bundled script polls a real scan until every expected finding shows up (or fails):

```bash
bash "$SKILL/scripts/verify-findings.sh" kind-kubeagent-demo ./kubeagent
```

Expect `OK — all expected findings present`. If it times out, read its scan dump — usually
an image is still pulling; wait and re-run. Only proceed on success.

### 5. Record

```bash
export PATH=$HOME/.local/bin:/home/linuxbrew/.linuxbrew/bin:/usr/local/go/bin:$PATH
vhs docs/kubeagent-demo.tape        # writes docs/kubeagent-demo.gif
```

### 6. Check the result before committing

A GIF that scrolled, clipped the output, or ballooned in size is worse than the old one.
Inspect the **last** frame (the full scan output) and the file size:

```bash
ffmpeg -y -sseof -0.3 -i docs/kubeagent-demo.gif -frames:v 1 /tmp/demo-lastframe.png 2>/dev/null
du -h docs/kubeagent-demo.gif        # expect a few hundred KB, not multiple MB
```

Open `/tmp/demo-lastframe.png` (or Read it) and confirm: the verdict line is at the top,
the NEEDS ATTENTION findings and the final prompt are both visible (nothing scrolled off),
and the glyphs (`✗ ⚠ ↳ · —`) render — no missing-glyph boxes. If the output no longer fits,
adjust `Height`/`Width`/`FontSize` in `docs/kubeagent-demo.tape` and re-record (step 5).

kubeagent's output is intentionally monochrome (no ANSI color — like `kubectl`); the tape
adds only a subtle green `$` prompt. Don't try to add per-line color; that would misrepresent
the tool.

### 7. Tear down and commit

```bash
kind delete cluster --name kubeagent-demo
```

`README.md` already references `docs/kubeagent-demo.gif`, so no README edit is needed unless
you moved the file. Commit the refreshed GIF (and the tape if you tuned it):

```bash
git add docs/kubeagent-demo.gif docs/kubeagent-demo.tape
git commit -m "docs: refresh kubeagent scan demo GIF"
```

## Adjusting what the demo shows

- **Different failures:** edit `assets/faults.yaml`, and update the `want=(...)` finding
  list in `scripts/verify-findings.sh` to match, so the pre-record test still guards the
  right things.
- **Look / pacing:** edit `docs/kubeagent-demo.tape` (`Set Theme`, `FontSize`, `Width`,
  `Height`, `TypingSpeed`, the trailing `Sleep` that holds on the final frame).
