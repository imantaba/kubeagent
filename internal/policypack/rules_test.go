package policypack_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/policy"
)

// The fixtures below use one namespace and one label set throughout. Neither
// names anything real: a rule is about a shape, not about infrastructure.
const (
	fixtureNamespace = "app"
	fixtureImage     = "registry.example.com/team/app:1.0"
)

// goodContainer satisfies every container-level rule in the pack. Each case
// below starts from it and removes or changes exactly the one thing its rule
// is about, so a case can only fail for its own reason.
func goodContainer() map[string]any {
	return map[string]any{
		"name":           "app",
		"image":          fixtureImage,
		"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": int64(8080)}},
		"livenessProbe":  map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": int64(8080)}},
		"resources": map[string]any{
			"limits":   map[string]any{"memory": "512Mi"},
			"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
		},
	}
}

// containerWithout returns a good container with one field removed. The path
// is walked so a nested field can be removed too, which is what the resource
// rules need: containerWithout("resources", "limits", "memory").
func containerWithout(t *testing.T, path ...string) map[string]any {
	t.Helper()
	c := goodContainer()
	m := c
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("containerWithout(%v): %q is not a map", path, k)
		}
		m = next
	}
	delete(m, path[len(path)-1])
	return c
}

// containerWithImage returns a good container with a different image.
func containerWithImage(image string) map[string]any {
	c := goodContainer()
	c["image"] = image
	return c
}

// workload builds a Deployment, StatefulSet or DaemonSet around one container.
// replicas is set by default so the replica rule's `gte` has something to
// compare — an absent field makes every operator except exists SKIP, and a
// fixture that made the rule skip would prove nothing.
func workload(kind, name string, c map[string]any, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     map[string]any{"containers": []any{c}},
			},
		},
	}}
}

func deployment(name string, c map[string]any) *unstructured.Unstructured {
	return workload("Deployment", name, c, 2)
}

// pdb builds a PodDisruptionBudget selecting the pod template labels workload
// stamps. coveredByPDB reads spec.selector.matchLabels and compares against
// spec.template.metadata.labels, in the same namespace.
func pdb(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "PodDisruptionBudget",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
		},
	}}
}

func cronJob(name, concurrencyPolicy string) *unstructured.Unstructured {
	spec := map[string]any{"schedule": "*/5 * * * *"}
	if concurrencyPolicy != "" {
		spec["concurrencyPolicy"] = concurrencyPolicy
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

func pvc(name, storageClass string) *unstructured.Unstructured {
	spec := map[string]any{"accessModes": []any{"ReadWriteOnce"}}
	if storageClass != "" {
		spec["storageClassName"] = storageClass
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

// ruleCase is one rule proved from both sides.
type ruleCase struct {
	id   string
	kind string // the Objects key the rule selects
	// violating must produce exactly one violation of this rule.
	violating *unstructured.Unstructured
	// satisfying must produce none.
	satisfying *unstructured.Unstructured
	// support is extra objects added to the SATISFYING run only, keyed by
	// kind. Only the PDB rule needs it: the point of that case is that the
	// budget is absent in one run and present in the other.
	support map[string][]*unstructured.Unstructured
}

// TestEveryReliabilityRuleFiresAndPasses drives each rule through the real
// evaluator, alone, against an object that must violate it and one that must
// not. A rule with a typo'd path or the wrong operator loads cleanly and
// checks nothing; this is what catches that.
func TestEveryReliabilityRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "reliability")
	byID := map[string]policy.Rule{}
	for _, r := range rules {
		byID[r.ID] = r
	}

	cases := []ruleCase{
		{
			id:         "reliability.deploy-readiness-probe",
			kind:       "Deployment",
			violating:  deployment("no-readiness", containerWithout(t, "readinessProbe")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-liveness-probe",
			kind:       "Deployment",
			violating:  deployment("no-liveness", containerWithout(t, "livenessProbe")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.statefulset-readiness-probe",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-readiness", containerWithout(t, "readinessProbe"), 2),
			satisfying: workload("StatefulSet", "ok", goodContainer(), 2),
		},
		{
			id:         "reliability.daemonset-readiness-probe",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-readiness", containerWithout(t, "readinessProbe"), 1),
			satisfying: workload("DaemonSet", "ok", goodContainer(), 1),
		},
		{
			id:         "reliability.deploy-memory-limit",
			kind:       "Deployment",
			violating:  deployment("no-mem-limit", containerWithout(t, "resources", "limits", "memory")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.statefulset-memory-limit",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-mem-limit", containerWithout(t, "resources", "limits", "memory"), 2),
			satisfying: workload("StatefulSet", "ok", goodContainer(), 2),
		},
		{
			id:         "reliability.deploy-cpu-request",
			kind:       "Deployment",
			violating:  deployment("no-cpu-request", containerWithout(t, "resources", "requests", "cpu")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-memory-request",
			kind:       "Deployment",
			violating:  deployment("no-mem-request", containerWithout(t, "resources", "requests", "memory")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:         "reliability.deploy-image-not-latest",
			kind:       "Deployment",
			violating:  deployment("floating", containerWithImage("registry.example.com/team/app:latest")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:   "reliability.deploy-image-tagged",
			kind: "Deployment",
			// No colon at all, so no tag and no digest.
			violating:  deployment("untagged", containerWithImage("registry.example.com/team/app")),
			satisfying: deployment("ok", goodContainer()),
		},
		{
			id:   "reliability.deploy-replicas-min-two",
			kind: "Deployment",
			// replicas is set explicitly: gte SKIPS an absent field, so a
			// fixture that omitted it would make the rule pass for the wrong
			// reason.
			violating:  workload("Deployment", "single", goodContainer(), 1),
			satisfying: workload("Deployment", "paired", goodContainer(), 2),
		},
		{
			id:         "reliability.deploy-pdb",
			kind:       "Deployment",
			violating:  deployment("unbudgeted", goodContainer()),
			satisfying: deployment("budgeted", goodContainer()),
			support: map[string][]*unstructured.Unstructured{
				"PodDisruptionBudget": {pdb("web")},
			},
		},
		{
			id:         "reliability.cronjob-concurrency-policy",
			kind:       "CronJob",
			violating:  cronJob("piles-up", "Allow"),
			satisfying: cronJob("serialized", "Forbid"),
		},
		{
			id:         "reliability.pvc-storage-class",
			kind:       "PersistentVolumeClaim",
			violating:  pvc("classless", ""),
			satisfying: pvc("fast", "standard"),
		},
	}

	if len(cases) != len(rules) {
		t.Fatalf("%d cases for %d rules — every rule must be proved from both sides", len(cases), len(rules))
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r, ok := byID[tc.id]
			if !ok {
				t.Fatalf("the pack has no rule %q", tc.id)
			}
			only := []policy.Rule{r}

			// The violating side.
			objects := map[string][]*unstructured.Unstructured{tc.kind: {tc.violating}}
			violations, notEvaluated := policy.Evaluate(only, policy.InputsFrom(objects, nil))
			if len(notEvaluated) != 0 {
				t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
			}
			if len(violations) != 1 {
				t.Fatalf("violating object produced %d violations, want 1", len(violations))
			}
			if violations[0].RuleID != tc.id {
				t.Errorf("violation is from rule %q, want %q", violations[0].RuleID, tc.id)
			}
			if violations[0].Level == policy.LevelCritical {
				t.Errorf("violation is critical — no pack rule may be")
			}

			// The satisfying side.
			objects = map[string][]*unstructured.Unstructured{tc.kind: {tc.satisfying}}
			for k, v := range tc.support {
				objects[k] = append(objects[k], v...)
			}
			violations, notEvaluated = policy.Evaluate(only, policy.InputsFrom(objects, nil))
			if len(notEvaluated) != 0 {
				t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
			}
			if len(violations) != 0 {
				t.Errorf("satisfying object produced %d violations, want 0: %#v", len(violations), violations)
			}
		})
	}
}

// TestWildcardRequiresEverySlot pins the semantic the container rules depend
// on: a wildcard produces one slot per element and every slot must satisfy the
// assertion, so a Deployment where only ONE of two containers sets a memory
// limit violates. Collapsing to "a value was found somewhere" would silently
// pass it, and every probe and resource rule in the pack would become
// decorative.
func TestWildcardRequiresEverySlot(t *testing.T) {
	rules := loadPack(t, "reliability")
	var r policy.Rule
	for _, candidate := range rules {
		if candidate.ID == "reliability.deploy-memory-limit" {
			r = candidate
		}
	}
	if r.ID == "" {
		t.Fatal("the pack has no reliability.deploy-memory-limit rule")
	}

	mixed := deployment("mixed", goodContainer())
	containers := []any{goodContainer(), containerWithout(t, "resources", "limits", "memory")}
	containers[1].(map[string]any)["name"] = "sidecar"
	spec := mixed.Object["spec"].(map[string]any)
	template := spec["template"].(map[string]any)
	template["spec"].(map[string]any)["containers"] = containers

	objects := map[string][]*unstructured.Unstructured{"Deployment": {mixed}}
	violations, _ := policy.Evaluate([]policy.Rule{r}, policy.InputsFrom(objects, nil))
	if len(violations) != 1 {
		t.Fatalf("a Deployment where one of two containers sets a memory limit produced %d violations, want 1", len(violations))
	}
}
