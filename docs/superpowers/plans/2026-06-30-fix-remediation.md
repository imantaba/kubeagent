# `--fix` Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in, guard-railed `--fix` mode whose one v1 remediation rolls back a Deployment whose newest rollout can't pull its image.

**Architecture:** A new pure `internal/remediate` package plans allowlisted `Action`s from diagnosed workloads and applies the single one (`RolloutUndo`) via client-go. `main.go` gains `--fix`/`--dry-run`/`--yes`, printing proposals after the report and confirming per action. No remediation is LLM-decided.

**Tech Stack:** Go 1.26, stdlib `flag`/`bufio`, `k8s.io/api/apps/v1`, `k8s.io/apimachinery` (`equality`, `meta/v1`), client-go (+ fake clientset for tests).

## Global Constraints

- **Invariant amendment:** kubeagent is **READ-ONLY by default**; the only cluster writes are guard-railed remediations behind the opt-in `--fix` flag, confirmed per action, from a fixed allowlist. Without `--fix`, behavior is byte-identical to today, and no `remediate` write path runs.
- **Allowlist:** the only `Action.Kind` is `"RolloutUndo"`. Anything else is never planned and is a no-op error in `Apply`.
- **Protected namespaces:** never plan or apply in `kube-system`, `kube-public`, `kube-node-lease`.
- **Execution is client-go only** (no kubectl runtime dependency); the `kubectl …` string is audit/display only.
- **No new Go module dependency** (apps/v1, apimachinery equality/meta, client-go fake are already present).
- **No LLM in the fix path:** nothing about actions is sent to `--explain`.
- Go at `/usr/local/go/bin` (`export PATH=$PATH:/usr/local/go/bin`).

---

### Task 1: `internal/remediate` — types, guards, pure `Plan`

**Files:**
- Create: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Produces: `Action` struct; `Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet) []Action`; unexported helpers `ownedBy`, `revFromAnnotations`, and `protectedNamespaces` reused by Task 2.

- [ ] **Step 1: Write the failing tests**

Create `internal/remediate/remediate_test.go`:

```go
package remediate

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/inventory"
)

func dep(ns, name string, issue string) inventory.Workload {
	w := inventory.Workload{Namespace: ns, Name: name, Kind: "Deployment"}
	if issue != "" {
		w.Findings = []diagnose.Finding{{Pod: ns + "/" + name + "-x", Issue: issue}}
	}
	return w
}

func rs(ns, name, owner, revision string) appsv1.ReplicaSet {
	return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations:     map[string]string{"deployment.kubernetes.io/revision": revision},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: owner}},
	}}
}

func TestPlan_ProposesRolloutUndo(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	got := Plan(wls, rss)
	if len(got) != 1 || got[0].Kind != "RolloutUndo" || got[0].Namespace != "shop" || got[0].Name != "web" {
		t.Fatalf("want one RolloutUndo for shop/web, got %+v", got)
	}
}

func TestPlan_SkipsWithoutImagePullFinding(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1"), rs("shop", "web-2", "web", "2")}
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("no finding -> no action, got %+v", got)
	}
}

func TestPlan_SkipsWithoutPriorRevision(t *testing.T) {
	wls := []inventory.Workload{dep("shop", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("shop", "web-1", "web", "1")} // only one revision
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("no prior revision -> no action, got %+v", got)
	}
}

func TestPlan_SkipsProtectedNamespace(t *testing.T) {
	wls := []inventory.Workload{dep("kube-system", "web", "ImagePullBackOff")}
	rss := []appsv1.ReplicaSet{rs("kube-system", "web-1", "web", "1"), rs("kube-system", "web-2", "web", "2")}
	if got := Plan(wls, rss); len(got) != 0 {
		t.Fatalf("protected namespace -> no action, got %+v", got)
	}
}

func TestPlan_SkipsNonDeployment(t *testing.T) {
	w := inventory.Workload{Namespace: "shop", Name: "ss", Kind: "StatefulSet",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}
	if got := Plan([]inventory.Workload{w}, nil); len(got) != 0 {
		t.Fatalf("non-Deployment -> no action, got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `undefined: Plan` / package has no Go files.

- [ ] **Step 3: Write the implementation**

Create `internal/remediate/remediate.go`:

```go
// Package remediate plans and applies safe, reversible, opt-in fixes for problems
// kubeagent detects. Planning is pure; applying performs a single guarded write
// via client-go. No remediation is ever decided by an LLM.
package remediate

import (
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/imantaba/kubeagent/internal/inventory"
)

const revisionAnno = "deployment.kubernetes.io/revision"

// protectedNamespaces are never targeted by a remediation.
var protectedNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// Action is one proposed, allowlisted remediation. Never free-form; never LLM-decided.
type Action struct {
	Kind              string // "RolloutUndo" (the only kind in v1)
	Namespace         string
	Name              string // workload name (a Deployment in v1)
	Summary           string // one-line human description
	Reason            string // why it's proposed
	KubectlEquivalent string // shown for audit only; NOT how it executes
}

// Plan returns the safe, allowlisted, precondition-satisfied remediations for the
// diagnosed workloads. Pure: reads only, mutates nothing.
func Plan(workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet) []Action {
	var actions []Action
	for _, w := range workloads {
		if w.Kind != "Deployment" || protectedNamespaces[w.Namespace] {
			continue
		}
		if !hasImagePullFinding(w) {
			continue
		}
		prev := previousRevision(w.Namespace, w.Name, replicaSets)
		if prev == "" {
			continue
		}
		actions = append(actions, Action{
			Kind:              "RolloutUndo",
			Namespace:         w.Namespace,
			Name:              w.Name,
			Summary:           "roll back to the previous revision",
			Reason:            "newest rollout cannot pull its image; a prior revision (" + prev + ") exists",
			KubectlEquivalent: "kubectl -n " + w.Namespace + " rollout undo deployment/" + w.Name,
		})
	}
	return actions
}

func hasImagePullFinding(w inventory.Workload) bool {
	for _, f := range w.Findings {
		if f.Issue == "ImagePullBackOff" || f.Issue == "ErrImagePull" {
			return true
		}
	}
	return false
}

// previousRevision returns the revision just below the current (max) one, among the
// ReplicaSets owned by the named Deployment in the namespace, or "" if there is no
// prior revision to roll back to.
func previousRevision(namespace, deployment string, replicaSets []appsv1.ReplicaSet) string {
	var revs []int
	for _, rs := range replicaSets {
		if rs.Namespace == namespace && ownedBy(rs, deployment) {
			if r := revFromAnnotations(rs.Annotations); r > 0 {
				revs = append(revs, r)
			}
		}
	}
	if len(revs) < 2 {
		return ""
	}
	sort.Sort(sort.Reverse(sort.IntSlice(revs)))
	return strconv.Itoa(revs[1])
}

func ownedBy(rs appsv1.ReplicaSet, deployment string) bool {
	for _, o := range rs.OwnerReferences {
		if o.Kind == "Deployment" && o.Name == deployment {
			return true
		}
	}
	return false
}

func revFromAnnotations(anno map[string]string) int {
	if v, ok := anno[revisionAnno]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -v && go vet ./internal/remediate/ && gofmt -l internal/remediate/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediate/
git commit -m "feat(remediate): Action + pure Plan (RolloutUndo for failed image rollout)"
```

---

### Task 2: `remediate.Apply` — guarded client-go executor

**Files:**
- Modify: `internal/remediate/remediate.go`
- Test: `internal/remediate/remediate_test.go`

**Interfaces:**
- Consumes: `Action`, `ownedBy`, `revFromAnnotations`, `protectedNamespaces` (Task 1).
- Produces: `Result` struct; `Apply(ctx context.Context, client kubernetes.Interface, a Action) Result`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/remediate/remediate_test.go` (add imports `context`, `corev1 "k8s.io/api/core/v1"`, `"k8s.io/client-go/kubernetes/fake"`):

```go
func depObj(ns, name, image, curRev string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name,
		Annotations: map[string]string{"deployment.kubernetes.io/revision": curRev},
	}}
	d.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
	return d
}

func rsWithImage(ns, name, owner, rev, image string) appsv1.ReplicaSet {
	r := rs(ns, name, owner, rev)
	r.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": owner, "pod-template-hash": "h" + rev}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
	}
	return r
}

func TestApply_RollsBackToPreviousTemplate(t *testing.T) {
	// current rev 2 = broken image; rev 1 = good image
	cur := depObj("shop", "web", "nginx:does-not-exist", "2")
	good := rsWithImage("shop", "web-1", "web", "1", "nginx:1.27")
	broken := rsWithImage("shop", "web-2", "web", "2", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &good, &broken)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if !res.Applied || res.Err != nil {
		t.Fatalf("expected applied, got %+v", res)
	}
	out, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got := out.Spec.Template.Spec.Containers[0].Image; got != "nginx:1.27" {
		t.Errorf("image not rolled back: got %q", got)
	}
	if _, ok := out.Spec.Template.Labels["pod-template-hash"]; ok {
		t.Errorf("pod-template-hash must be cleared on the Deployment template")
	}
}

func TestApply_NoTargetWhenOnlyCurrentRevision(t *testing.T) {
	cur := depObj("shop", "web", "nginx:does-not-exist", "1")
	only := rsWithImage("shop", "web-1", "web", "1", "nginx:does-not-exist")
	cli := fake.NewSimpleClientset(cur, &only)
	res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "shop", Name: "web"})
	if res.Applied || res.Err != nil {
		t.Fatalf("expected a no-write skip, got %+v", res)
	}
}

func TestApply_RejectsUnknownKindAndProtectedNs(t *testing.T) {
	cli := fake.NewSimpleClientset()
	if res := Apply(context.Background(), cli, Action{Kind: "Nuke", Namespace: "shop", Name: "web"}); res.Err == nil {
		t.Error("unknown kind must error")
	}
	if res := Apply(context.Background(), cli, Action{Kind: "RolloutUndo", Namespace: "kube-system", Name: "x"}); res.Err == nil {
		t.Error("protected namespace must error")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/`
Expected: FAIL — `undefined: Apply`.

- [ ] **Step 3: Write the implementation**

Append to `internal/remediate/remediate.go` (add imports: `context`, `fmt`, `corev1 "k8s.io/api/core/v1"`, `apiequality "k8s.io/apimachinery/pkg/api/equality"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"k8s.io/client-go/kubernetes"`):

```go
// Result records what Apply did, for the audit line.
type Result struct {
	Action  Action
	Applied bool
	Detail  string
	Err     error
}

// Apply performs the action's single guarded write via client-go and reports it.
func Apply(ctx context.Context, client kubernetes.Interface, a Action) Result {
	res := Result{Action: a}
	if a.Kind != "RolloutUndo" {
		res.Err = fmt.Errorf("unknown action kind %q", a.Kind)
		return res
	}
	if protectedNamespaces[a.Namespace] {
		res.Err = fmt.Errorf("refusing to act in protected namespace %q", a.Namespace)
		return res
	}
	dep, err := client.AppsV1().Deployments(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		res.Err = fmt.Errorf("get deployment: %w", err)
		return res
	}
	rsList, err := client.AppsV1().ReplicaSets(a.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		res.Err = fmt.Errorf("list replicasets: %w", err)
		return res
	}
	target := pickTarget(dep, rsList.Items)
	if target == nil {
		res.Detail = "no differing prior revision to roll back to (state changed); no write made"
		return res
	}
	// Roll back: restore the target revision's pod template. The controller manages
	// the pod-template-hash label, so drop it from the Deployment spec.
	tpl := *target.Spec.Template.DeepCopy()
	delete(tpl.Labels, "pod-template-hash")
	dep.Spec.Template = tpl
	if _, err := client.AppsV1().Deployments(a.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		res.Err = fmt.Errorf("update deployment: %w", err)
		return res
	}
	res.Applied = true
	res.Detail = fmt.Sprintf("rolled back %s/%s to revision %d (pod template restored)",
		a.Namespace, a.Name, revFromAnnotations(target.Annotations))
	return res
}

// pickTarget returns the owned ReplicaSet with the highest revision strictly below
// the Deployment's current revision whose pod template differs from the current
// one. nil if none.
func pickTarget(dep *appsv1.Deployment, replicaSets []appsv1.ReplicaSet) *appsv1.ReplicaSet {
	curRev := revFromAnnotations(dep.Annotations)
	var best *appsv1.ReplicaSet
	for i := range replicaSets {
		rs := &replicaSets[i]
		if rs.Namespace != dep.Namespace || !ownedBy(*rs, dep.Name) {
			continue
		}
		r := revFromAnnotations(rs.Annotations)
		if curRev > 0 && r >= curRev {
			continue
		}
		if templatesEqual(rs.Spec.Template, dep.Spec.Template) {
			continue
		}
		if best == nil || r > revFromAnnotations(best.Annotations) {
			best = rs
		}
	}
	return best
}

func templatesEqual(a, b corev1.PodTemplateSpec) bool {
	ac, bc := a.DeepCopy(), b.DeepCopy()
	delete(ac.Labels, "pod-template-hash")
	delete(bc.Labels, "pod-template-hash")
	return apiequality.Semantic.DeepEqual(ac, bc)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `export PATH=$PATH:/usr/local/go/bin && go test ./internal/remediate/ -v && go vet ./internal/remediate/ && gofmt -l internal/remediate/`
Expected: tests PASS, vet clean, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/remediate/
git commit -m "feat(remediate): Apply rollout-undo via client-go (guarded, re-verified)"
```

---

### Task 3: wire `--fix` into `main.go`

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `remediate.Plan`, `remediate.Apply`, `remediate.Action`, `remediate.Result`; `inputs.ReplicaSets`; the existing `report.PrintInventory` call.

- [ ] **Step 1: Write the failing tests**

Append to `main_test.go` (it imports `os`, `path/filepath`, `strings`, `testing`; add `"bytes"`, `"context"`, `appsv1 "k8s.io/api/apps/v1"`, `corev1 "k8s.io/api/core/v1"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"k8s.io/client-go/kubernetes/fake"`, `"github.com/imantaba/kubeagent/internal/diagnose"`, `"github.com/imantaba/kubeagent/internal/inventory"`):

```go
func TestRun_FixFlagsAccepted(t *testing.T) {
	// --fix/--dry-run/--yes must be defined flags: this fails on output-format
	// validation (before any cluster call), proving they parsed.
	err := run([]string{"scan", "--fix", "--dry-run", "--yes", "--output", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown output format") {
		t.Fatalf("expected output-format error (flags accepted), got: %v", err)
	}
}

func fixWorkload() []inventory.Workload {
	return []inventory.Workload{{Namespace: "shop", Name: "web", Kind: "Deployment",
		Findings: []diagnose.Finding{{Issue: "ImagePullBackOff"}}}}
}
func fixRS() []appsv1.ReplicaSet {
	mk := func(name, rev, img string) appsv1.ReplicaSet {
		r := appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": rev},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web"}}}}
		r.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: img}}}}
		return r
	}
	return []appsv1.ReplicaSet{mk("web-1", "1", "nginx:1.27"), mk("web-2", "2", "nginx:bad")}
}

func TestRunFixes_DryRunWritesNothing(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	cli := fake.NewSimpleClientset(d)
	var out bytes.Buffer
	runFixes(context.Background(), cli, fixWorkload(), fixRS(), true /*dryRun*/, false, &out, strings.NewReader(""))
	for _, a := range cli.Actions() {
		if a.GetVerb() == "update" {
			t.Fatalf("dry-run must not write; saw %s", a.GetVerb())
		}
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected a dry-run notice, got: %s", out.String())
	}
}

func TestRunFixes_YesApplies(t *testing.T) {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}}}
	d.Spec.Template = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:bad"}}}}
	rss := fixRS()
	cli := fake.NewSimpleClientset(d, &rss[0], &rss[1])
	var out bytes.Buffer
	runFixes(context.Background(), cli, fixWorkload(), rss, false, true /*assumeYes*/, &out, strings.NewReader(""))
	got, _ := cli.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	if got.Spec.Template.Spec.Containers[0].Image != "nginx:1.27" {
		t.Errorf("expected rollback to nginx:1.27, got %q", got.Spec.Template.Spec.Containers[0].Image)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRun_FixFlagsAccepted|TestRunFixes'`
Expected: FAIL — `undefined: runFixes` / `flag provided but not defined: -fix`.

- [ ] **Step 3: Wire `main.go`**

Add imports `bufio`, `io`, `appsv1 "k8s.io/api/apps/v1"`, `"k8s.io/client-go/kubernetes"`, and `"github.com/imantaba/kubeagent/internal/remediate"` (keep the existing `strings`/`fmt`/`context`/`os`/`inventory` imports).

Register the flags after `lintSecrets` (main.go:57):

```go
	fix := fs.Bool("fix", false, "propose and (after confirmation) apply safe, reversible remediations (opt-in writes)")
	dryRun := fs.Bool("dry-run", false, "with --fix: print proposed remediations only; never prompt or write")
	assumeYes := fs.Bool("yes", false, "with --fix: apply all proposed remediations without prompting")
```

Add `[--fix [--dry-run|--yes]]` to the usage string (after `[--lint-secrets]`).

Replace the final report return:

```go
	return report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, explanation, *output, os.Stdout)
```

with:

```go
	if err := report.PrintInventory(health, result, &summary, &facts, serviceIssues, credWarnings, explanation, *output, os.Stdout); err != nil {
		return err
	}
	if *fix {
		runFixes(context.Background(), client, result.Workloads, inputs.ReplicaSets, *dryRun, *assumeYes, os.Stdout, os.Stdin)
	}
	return nil
```

Add the helper functions (anywhere at file scope in `main.go`):

```go
// runFixes proposes the planned remediations and, unless --dry-run, applies each
// after a [y/N] confirmation (or unconditionally with --yes). Writes are guarded
// inside remediate.Apply.
func runFixes(ctx context.Context, client kubernetes.Interface, workloads []inventory.Workload, replicaSets []appsv1.ReplicaSet, dryRun, assumeYes bool, w io.Writer, in io.Reader) {
	actions := remediate.Plan(workloads, replicaSets)
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nNo automatic remediations available.")
		return
	}
	reader := bufio.NewReader(in)
	for _, a := range actions {
		fmt.Fprintf(w, "\nProposed fix: %s/%s (Deployment) — %s\n  reason: %s\n  kubectl equivalent: %s\n",
			a.Namespace, a.Name, a.Summary, a.Reason, a.KubectlEquivalent)
		if dryRun {
			fmt.Fprintln(w, "  (dry-run: not applied)")
			continue
		}
		if !assumeYes {
			fmt.Fprint(w, "  Apply? [y/N] ")
			line, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(line)) != "y" {
				fmt.Fprintln(w, "  skipped.")
				continue
			}
		}
		res := remediate.Apply(ctx, client, a)
		switch {
		case res.Err != nil:
			fmt.Fprintf(w, "  ERROR: %v\n", res.Err)
		case res.Applied:
			fmt.Fprintf(w, "  applied: %s\n", res.Detail)
		default:
			fmt.Fprintf(w, "  skipped: %s\n", res.Detail)
		}
	}
}
```

- [ ] **Step 4: Run tests + build**

Run: `export PATH=$PATH:/usr/local/go/bin && go test . -run 'TestRun_FixFlagsAccepted|TestRunFixes' -v && go build ./... && go vet ./... && go test ./... && gofmt -l main.go main_test.go`
Expected: the new tests PASS, build succeeds, vet clean, all packages `ok`, gofmt prints nothing.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: --fix mode (propose, confirm, apply remediations) wired into scan"
```

---

### Task 4: docs — invariant amendment, README, CHANGELOG, chaos acceptance

**Files:**
- Modify: `CLAUDE.md`, `README.md`, `CHANGELOG.md`, `chaos/README.md`

- [ ] **Step 1: Amend the READ-ONLY invariant in `CLAUDE.md`**

Replace the bullet:

```markdown
- **READ-ONLY.** Only `List`/`Get`-style calls. Never create, update, patch, or
  delete cluster resources.
```

with:

```markdown
- **READ-ONLY by default.** Only `List`/`Get`-style calls, EXCEPT the opt-in
  `--fix` remediation flag, whose writes are guard-railed (fixed allowlist,
  protected namespaces, per-action confirmation, re-verify) and never
  LLM-decided. Without `--fix`, kubeagent never creates, updates, patches, or
  deletes anything.
```

- [ ] **Step 2: Update `README.md`**

In the intro, change `and operates **read-only**.` to `and operates **read-only by default** (an opt-in \`--fix\` flag can apply safe, reversible remediations — see below).` Then add a section before `## Install`:

```markdown
### Remediation (--fix, opt-in)

By default kubeagent only reads. `scan --fix` additionally proposes safe,
reversible remediations for what it finds and applies each one **only after you
confirm** (`Apply? [y/N]`, default No). Writes are guard-railed: a fixed allowlist
of actions, never in protected namespaces (`kube-system`, `kube-public`,
`kube-node-lease`), preconditions re-checked against live state, and the result
re-verified. Nothing about remediations is sent to `--explain`.

```bash
./kubeagent scan --fix             # propose + confirm each fix
./kubeagent scan --fix --dry-run   # show proposals only; never prompt or write
./kubeagent scan --fix --yes       # apply all proposals without prompting
```

**v1 remediation:** `RolloutUndo` — when a Deployment's newest rollout can't pull
its image (`ImagePullBackOff`/`ErrImagePull`) and a prior revision exists, roll it
back to that revision (a single, reversible `Deployment` update via client-go).
```

- [ ] **Step 3: Update `CHANGELOG.md`**

Under `## [Unreleased]`, add:

```markdown
### Added

- **`--fix` remediation (opt-in).** `scan --fix` proposes and, after a per-action
  `[y/N]` confirmation, applies safe reversible remediations (`--dry-run` to
  preview, `--yes` for non-interactive). v1 ships `RolloutUndo` (roll a Deployment
  with a failed image rollout back to its previous revision). Guard-railed:
  allowlist, protected namespaces, apply-time precondition re-check, re-verify;
  never LLM-decided. This is the first feature that can write to the cluster;
  default behavior remains read-only.
```

- [ ] **Step 4: Add a `--fix` acceptance note to `chaos/README.md`**

After the scenarios table, add:

```markdown
### Validating `--fix` (remediation)

Scenario 9 (faulty rollout) is the acceptance test for `--fix`. After a run leaves
it injected, roll it back and confirm recovery:

```bash
kubectl --context kind-kubeagent-chaos -n chaos-rollout set image deploy/web web=nginx:does-not-exist-9999
./kubeagent scan --context kind-kubeagent-chaos --fix --yes
kubectl --context kind-kubeagent-chaos -n chaos-rollout rollout status deploy/web
```

kubeagent should propose and apply a `RolloutUndo`, and the Deployment should
return to a healthy image.
```

- [ ] **Step 5: Build + commit**

Run: `export PATH=$PATH:/usr/local/go/bin && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok`.

```bash
git add CLAUDE.md README.md CHANGELOG.md chaos/README.md
git commit -m "docs: --fix remediation (invariant amendment, README, changelog, chaos acceptance)"
```

---

## Self-Review

**Spec coverage:**
- Invariant amendment → Task 4 (CLAUDE.md/README). ✓
- `internal/remediate` pure `Plan` + types + guards → Task 1. ✓
- `Apply` client-go executor (rollback mechanics, protected-ns + allowlist + apply-time precondition + re-verify detail) → Task 2. ✓
- CLI `--fix`/`--dry-run`/`--yes`, per-action confirm, dry-run-wins, proposals after report → Task 3. ✓
- No LLM in fix path (explain untouched) → Task 3 wiring doesn't touch the explain call. ✓
- Live acceptance (chaos scenario 9) → Task 4 chaos/README note (controller validates live on finish). ✓

**Placeholder scan:** none — complete code in every step.

**Type/name consistency:** `Action{Kind,Namespace,Name,Summary,Reason,KubectlEquivalent}`, `Result{Action,Applied,Detail,Err}`, `Plan(workloads, replicaSets)`, `Apply(ctx, client, action)`, helpers `ownedBy`/`revFromAnnotations`/`protectedNamespaces`/`pickTarget`/`templatesEqual`, and `runFixes(ctx, client, workloads, replicaSets, dryRun, assumeYes, w, in)` are used identically across Tasks 1–3. The revision annotation `deployment.kubernetes.io/revision` and the allowlisted kind `"RolloutUndo"` match across plan/apply/tests.
```
