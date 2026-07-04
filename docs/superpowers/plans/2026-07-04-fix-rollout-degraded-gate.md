# `--fix` RolloutUndo degraded-only gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--fix` propose a `RolloutUndo` only when the Deployment is actually degraded (`Ready < Desired`), so a stuck-but-serving rollout (previous revision still serving) is left alone.

**Architecture:** One condition added to the pure `Plan()` function in `internal/remediate/remediate.go`, unit-tested with fake workloads. Then docs (README, CHANGELOG, website) and the chaos `--fix` acceptance recipe are updated to match.

**Tech Stack:** Go 1.26. No new dependency — `Ready`/`Desired` are already fields on `inventory.Workload` passed into `Plan`.

## Global Constraints

- **READ-ONLY by default.** This change only makes `--fix` propose *less*; no new write path, no change to `Apply`, guard rails unchanged (allowlist, protected namespaces, apply-time re-check, per-action confirm, never LLM-decided).
- **Allowlist unchanged** — `{RolloutUndo, Uncordon}`. Uncordon is untouched.
- `--fix` stays fully decoupled from `--explain`. No new Go module dependency.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: degraded-only gate in `Plan()` + tests

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: `inventory.Workload.Ready` / `.Desired` (ints, already present).
- Produces: `RolloutUndo` actions only for Deployments where `Ready < Desired`.

- [ ] **Step 1: Make the test fixtures degraded, and write the new failing test**

In `internal/remediate/remediate_test.go`, update the `dep` helper so the
Deployment it builds is degraded (so the existing "proposes" tests still trigger
once the gate is added). Change:

```go
func dep(ns, name string, issue string) inventory.Workload {
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment"}
	if issue != "" {
		w.Findings = []diagnose.Finding{{Pod: ns + "/" + name + "-x", Issue: issue}}
	}
	return w
}
```

to:

```go
func dep(ns, name string, issue string) inventory.Workload {
	// Ready 0 < Desired 1 => degraded, so a RolloutUndo is warranted.
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment", Desired: 1, Ready: 0}
	if issue != "" {
		w.Findings = []diagnose.Finding{{Pod: ns + "/" + name + "-x", Issue: issue}}
	}
	return w
}
```

Then add this new test (a still-serving stuck rollout — the the-importer case):

```go
func TestPlan_SkipsAvailableDeployment(t *testing.T) {
	w := dep("shop", "web", "ImagePullBackOff")
	w.Ready, w.Desired = 1, 1 // previous revision still serving — not degraded
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	if got := Plan([]inventory.Workload{w}, rss, nil); len(got) != 0 {
		t.Fatalf("available deployment (Ready==Desired) -> no rollback, got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify the new one fails**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -run TestPlan`
Expected: `TestPlan_SkipsAvailableDeployment` FAILS (Plan still proposes a rollback
for a `1/1` Deployment); the other `TestPlan_*` tests PASS (the degraded `dep`
fixture still triggers them, since the gate isn't added yet).

- [ ] **Step 3: Add the degraded gate to `Plan()`**

In `internal/remediate/remediate.go`, in the Deployment loop, add the availability
check. Change:

```go
		if !hasImagePullFinding(w) {
			continue
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
```

to:

```go
		if !hasImagePullFinding(w) {
			continue
		}
		if w.Ready >= w.Desired {
			continue // still meeting its replica target (e.g. previous revision serving) — not an outage
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
```

- [ ] **Step 4: Run tests to verify all pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -v && go build ./... && go vet ./internal/remediate/ && gofmt -l internal/remediate/`
Expected: all remediate tests PASS (including the new `TestPlan_SkipsAvailableDeployment`
and every existing `TestPlan_*`/`TestApply_*`), build succeeds, vet clean, gofmt
prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediate/
git commit -m "fix(remediate): only propose RolloutUndo for a degraded Deployment (Ready < Desired)"
```

---

### Task 2: docs + chaos acceptance recipe

**Files:**
- Modify: `README.md`, `CHANGELOG.md`, `website/docs/features/remediation.md`, `chaos/README.md`

- [ ] **Step 1: `README.md` — qualify the RolloutUndo bullet**

In the `### Remediation (--fix, opt-in)` section, change the `RolloutUndo` bullet:

```markdown
- **`RolloutUndo`** — when a Deployment's newest rollout can't pull its image
  (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it back to
  that revision (a single, reversible `Deployment` update via client-go).
```

to:

```markdown
- **`RolloutUndo`** — when a Deployment is **degraded** (fewer ready replicas than
  desired) because its newest rollout can't pull its image
  (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it back to
  that revision (a single, reversible `Deployment` update via client-go). A rollout
  that is stuck but still serving on its previous revision is left alone.
```

- [ ] **Step 2: `CHANGELOG.md` — add an `[Unreleased] → Changed` entry**

Insert a new `## [Unreleased]` section with a `### Changed` block directly above
the existing `## [0.7.0] - 2026-07-01` line:

```markdown
## [Unreleased]

### Changed

- **`--fix` `RolloutUndo` is more conservative.** A Deployment rollback is now
  proposed only when the Deployment is **degraded** (fewer ready replicas than
  desired). A rollout stuck on `ImagePullBackOff` while its previous revision is
  still fully serving is left alone (the failure still shows in the scan and
  `--explain`; only the automatic rollback proposal is withheld).

## [0.7.0] - 2026-07-01
```

- [ ] **Step 3: `website/docs/features/remediation.md` — qualify the RolloutUndo row**

In the actions table, change the `RolloutUndo` row:

```markdown
| `RolloutUndo` | a Deployment's newest rollout cannot pull its image and a prior revision exists | rolls the Deployment back to its previous revision | `kubectl -n <ns> rollout undo deployment/<name>` |
```

to:

```markdown
| `RolloutUndo` | a Deployment is **degraded** (Ready < Desired) because its newest rollout cannot pull its image, and a prior revision exists | rolls the Deployment back to its previous revision | `kubectl -n <ns> rollout undo deployment/<name>` |
```

Then, immediately after the table (before the `An accidental cordon…` sentence),
add a note:

```markdown
A rollout that is stuck on `ImagePullBackOff` but whose previous revision is still
serving (`Ready == Desired`) is **not** rolled back — the app is not down, so the
image is left for you to fix forward.
```

- [ ] **Step 4: `chaos/README.md` — force a degraded rollout in the `--fix` acceptance recipe**

In the `### Validating --fix (remediation)` section, change the `RolloutUndo`
recipe:

```bash
kubectl --context kind-kubeagent-chaos -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos -n chaos-rollout rollout status deploy/web
```

to:

```bash
# Force a degraded rollout: no surge + allow an old pod down, so the failing new
# pod replaces a serving one (Ready < Desired) — which is what --fix now requires
# before proposing a rollback.
kubectl --context kind-kubeagent-chaos -n chaos-rollout patch deploy/web \
  -p '{"spec":{"strategy":{"rollingUpdate":{"maxSurge":0,"maxUnavailable":1}}}}'
kubectl --context kind-kubeagent-chaos -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos -n chaos-rollout rollout status deploy/web
```

And change the sentence after it:

```markdown
kubeagent should propose and apply a `RolloutUndo`, and the Deployment should
return to a healthy image.
```

to:

```markdown
kubeagent should propose and apply a `RolloutUndo` (the Deployment is degraded —
the new pod can't pull and replaced a serving one), and the Deployment should
return to a healthy image.
```

- [ ] **Step 5: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok` (no code changed in this task).

```bash
git add README.md CHANGELOG.md website/docs/features/remediation.md chaos/README.md
git commit -m "docs(fix): RolloutUndo only for degraded Deployments; degrade the chaos acceptance rollout"
```

---

## Self-Review

**Spec coverage:**
- Degraded-only gate (`Ready < Desired`) in `Plan()` → Task 1. ✓
- Fixture update + new still-serving skip test → Task 1. ✓
- README / CHANGELOG / website qualification → Task 2. ✓
- Chaos acceptance recipe forces a degraded rollout → Task 2. ✓
- Read-only / allowlist / Uncordon untouched / no new dep (Global Constraints) → only a `continue` added to `Plan`; no `Apply`/allowlist/module changes. ✓

**Placeholder scan:** none — every step has complete code/text.

**Type/name consistency:** `w.Ready`/`w.Desired` (ints on `inventory.Workload`),
the `dep` helper's degraded default (`Desired: 1, Ready: 0`), and the new
`TestPlan_SkipsAvailableDeployment` overriding to `1, 1` are consistent across the
task and its tests. The added guard `if w.Ready >= w.Desired { continue }` sits
after `hasImagePullFinding` and before `previousRevision`, matching the design's
condition order.
