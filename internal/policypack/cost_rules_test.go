package policypack_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/policy"
)

// TestCostPackKindDistribution pins the pack's scope decision: three workload
// kinds carry resource rules, CronJob carries five because it has both a pod
// template and its own retention knobs, and three more kinds are here because
// each one is a direct claim on spend — a Job's retry budget, an autoscaler's
// ceiling, a claim's size. It also pins the level split, which is the pack's
// central promise: every rule is info, so no --fail-on above info can be
// failed by adding this pack to a pipeline.
func TestCostPackKindDistribution(t *testing.T) {
	rules := loadPack(t, "cost")

	byKind := map[string]int{}
	byLevel := map[policy.Level]int{}
	for _, r := range rules {
		byKind[r.Match.Kind]++
		byLevel[r.Level]++
	}

	wantKind := map[string]int{
		"Deployment":              3,
		"StatefulSet":             2,
		"DaemonSet":               3,
		"CronJob":                 5,
		"Job":                     1,
		"HorizontalPodAutoscaler": 1,
		"PersistentVolumeClaim":   1,
	}
	for kind, n := range wantKind {
		if byKind[kind] != n {
			t.Errorf("%d rules select %s, want %d", byKind[kind], kind, n)
		}
	}
	for kind := range byKind {
		if _, ok := wantKind[kind]; !ok {
			t.Errorf("the pack selects %s, which is not one of the seven kinds it is scoped to", kind)
		}
	}

	if len(rules) != 16 {
		t.Errorf("the pack holds %d rules, want 16", len(rules))
	}

	if byLevel[policy.LevelInfo] != 16 {
		t.Errorf("%d rules are info, want 16", byLevel[policy.LevelInfo])
	}
	if n := byLevel[policy.LevelWarning] + byLevel[policy.LevelCritical]; n != 0 {
		t.Errorf("%d rules are above info — the cost pack must not be able to fail a gate above --fail-on info", n)
	}
}

// sizedContainer satisfies every container-level rule in the cost pack: it
// requests a modest amount of CPU and memory and bounds its local disk. Each
// case below starts from it and changes or removes exactly the one thing its
// rule is about, so a case can only fail for its own reason.
func sizedContainer() map[string]any {
	return map[string]any{
		"name":  "app",
		"image": fixtureImage,
		"resources": map[string]any{
			"limits":   map[string]any{"memory": "512Mi", "ephemeral-storage": "1Gi"},
			"requests": map[string]any{"cpu": "100m", "memory": "256Mi"},
		},
	}
}

// containerWithoutResource returns a sized container with one resources entry
// removed: containerWithoutResource(t, "requests", "cpu").
func containerWithoutResource(t *testing.T, group, key string) map[string]any {
	t.Helper()
	c := sizedContainer()
	res, ok := c["resources"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources map")
	}
	g, ok := res[group].(map[string]any)
	if !ok {
		t.Fatalf("the sized container has no resources.%s map", group)
	}
	delete(g, key)
	return c
}

// containerRequesting returns a sized container asking for a different amount
// of one resource. Setting the field explicitly is the whole point: every
// operator except exists and notExists SKIPS an absent slot, so a fixture that
// merely omitted the field would make a threshold rule say nothing, which is
// not a pass.
func containerRequesting(t *testing.T, key, value string) map[string]any {
	t.Helper()
	c := sizedContainer()
	res, ok := c["resources"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources map")
	}
	g, ok := res["requests"].(map[string]any)
	if !ok {
		t.Fatal("the sized container has no resources.requests map")
	}
	g[key] = value
	return c
}

// cronJobKnobs is the four things the five CronJob rules read. goodCronJobSpec
// fills every one with a safe value and each case changes exactly one; a nil
// history or deadline leaves that field ABSENT, which is what the
// activeDeadlineSeconds case needs and what every threshold case must avoid.
type cronJobKnobs struct {
	container             map[string]any
	successfulHistory     any
	failedHistory         any
	activeDeadlineSeconds any
}

func goodCronJobSpec() cronJobKnobs {
	return cronJobKnobs{
		container:             sizedContainer(),
		successfulHistory:     int64(3),
		failedHistory:         int64(1),
		activeDeadlineSeconds: int64(600),
	}
}

// cronJobFrom wraps the knobs in the batch/v1 shape, whose pod template lives
// one level deeper than a Deployment's: spec.jobTemplate.spec.template.spec.
func cronJobFrom(name string, k cronJobKnobs) *unstructured.Unstructured {
	jobSpec := map[string]any{
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
			"spec":     map[string]any{"containers": []any{k.container}},
		},
	}
	if k.activeDeadlineSeconds != nil {
		jobSpec["activeDeadlineSeconds"] = k.activeDeadlineSeconds
	}
	spec := map[string]any{
		"schedule":    "*/5 * * * *",
		"jobTemplate": map[string]any{"spec": jobSpec},
	}
	if k.successfulHistory != nil {
		spec["successfulJobsHistoryLimit"] = k.successfulHistory
	}
	if k.failedHistory != nil {
		spec["failedJobsHistoryLimit"] = k.failedHistory
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec":       spec,
	}}
}

func goodCronJob(name string) *unstructured.Unstructured {
	return cronJobFrom(name, goodCronJobSpec())
}

func cronJobWithContainer(name string, c map[string]any) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.container = c
	return cronJobFrom(name, k)
}

func cronJobWithHistory(name string, successful, failed any) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.successfulHistory, k.failedHistory = successful, failed
	return cronJobFrom(name, k)
}

func cronJobWithoutDeadline(name string) *unstructured.Unstructured {
	k := goodCronJobSpec()
	k.activeDeadlineSeconds = nil
	return cronJobFrom(name, k)
}

// costJob builds a batch/v1 Job with an explicit backoffLimit. The field is
// always set: absent, lte would skip, and the API's own default of six is
// already within the threshold anyway.
func costJob(name string, backoffLimit int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"backoffLimit": backoffLimit,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     map[string]any{"containers": []any{sizedContainer()}},
			},
		},
	}}
}

// hpa builds an autoscaling/v2 HorizontalPodAutoscaler with an explicit
// ceiling. maxReplicas is required by the API, so it is never absent in
// practice and never absent here.
func hpa(name string, maxReplicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling/v2",
		"kind":       "HorizontalPodAutoscaler",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"minReplicas":    int64(2),
			"maxReplicas":    maxReplicas,
			"scaleTargetRef": map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": "web"},
		},
	}}
}

// claim builds a PersistentVolumeClaim with an explicit request. rules_test.go
// already has a pvc helper for the reliability pack's storage-class rule; that
// one sets no size, which is the one field this pack reads.
func claim(name, storage string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": "standard",
			"resources":        map[string]any{"requests": map[string]any{"storage": storage}},
		},
	}}
}

// TestEveryCostRuleFiresAndPasses drives each rule through the real evaluator,
// alone, against an object that must violate it and one that must not. A rule
// with a typo'd path or the wrong operator loads cleanly and checks nothing;
// this is what catches that.
func TestEveryCostRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "cost")

	cases := []ruleCase{
		{
			id:         "cost.deploy-ephemeral-storage-limit",
			kind:       "Deployment",
			violating:  deployment("unbounded-disk", containerWithoutResource(t, "limits", "ephemeral-storage")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.statefulset-cpu-request",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-cpu", containerWithoutResource(t, "requests", "cpu"), 2),
			satisfying: workload("StatefulSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.statefulset-memory-request",
			kind:       "StatefulSet",
			violating:  workload("StatefulSet", "no-memory", containerWithoutResource(t, "requests", "memory"), 2),
			satisfying: workload("StatefulSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-cpu-request",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-cpu", containerWithoutResource(t, "requests", "cpu"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-memory-request",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "no-memory", containerWithoutResource(t, "requests", "memory"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.daemonset-ephemeral-storage-limit",
			kind:       "DaemonSet",
			violating:  workload("DaemonSet", "unbounded-disk", containerWithoutResource(t, "limits", "ephemeral-storage"), 2),
			satisfying: workload("DaemonSet", "sized", sizedContainer(), 2),
		},
		{
			id:         "cost.cronjob-cpu-request",
			kind:       "CronJob",
			violating:  cronJobWithContainer("no-cpu", containerWithoutResource(t, "requests", "cpu")),
			satisfying: goodCronJob("sized"),
		},
		{
			id:         "cost.cronjob-memory-request",
			kind:       "CronJob",
			violating:  cronJobWithContainer("no-memory", containerWithoutResource(t, "requests", "memory")),
			satisfying: goodCronJob("sized"),
		},
		{
			id:         "cost.cronjob-active-deadline",
			kind:       "CronJob",
			violating:  cronJobWithoutDeadline("no-deadline"),
			satisfying: goodCronJob("bounded"),
		},
		{
			id:         "cost.deploy-large-cpu-request",
			kind:       "Deployment",
			violating:  deployment("big-cpu", containerRequesting(t, "cpu", "16")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.deploy-large-memory-request",
			kind:       "Deployment",
			violating:  deployment("big-memory", containerRequesting(t, "memory", "64Gi")),
			satisfying: deployment("sized", sizedContainer()),
		},
		{
			id:         "cost.cronjob-successful-history",
			kind:       "CronJob",
			violating:  cronJobWithHistory("many-successes", int64(50), int64(1)),
			satisfying: goodCronJob("few-successes"),
		},
		{
			id:         "cost.cronjob-failed-history",
			kind:       "CronJob",
			violating:  cronJobWithHistory("many-failures", int64(3), int64(50)),
			satisfying: goodCronJob("few-failures"),
		},
		{
			id:         "cost.job-backoff-limit",
			kind:       "Job",
			violating:  costJob("many-retries", 50),
			satisfying: costJob("few-retries", 6),
		},
		{
			id:         "cost.hpa-max-replicas",
			kind:       "HorizontalPodAutoscaler",
			violating:  hpa("wide", 200),
			satisfying: hpa("narrow", 10),
		},
		{
			id:         "cost.pvc-large-storage",
			kind:       "PersistentVolumeClaim",
			violating:  claim("big", "4Ti"),
			satisfying: claim("small", "20Gi"),
		},
	}

	if len(cases) != 16 {
		t.Fatalf("%d cases, want one per rule in a sixteen-rule pack", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r := packRule(t, rules, tc.id)

			if got := evaluateOne(t, r, tc.kind, tc.violating); len(got) != 1 {
				t.Errorf("%s produced %d violations on the violating object, want 1", tc.id, len(got))
			}
			if got := evaluateOne(t, r, tc.kind, tc.satisfying); len(got) != 0 {
				t.Errorf("%s produced %d violations on the satisfying object, want 0", tc.id, len(got))
			}
		})
	}
}

// TestCostThresholdsCompareQuantitiesNotStrings machine-checks the fact the
// whole threshold half of this pack rests on: internal/policy's compareNumeric
// falls through ParseInt and ParseFloat to resource.ParseQuantity, so a
// threshold compares quantities rather than bytes.
//
// Both cases below are chosen because a lexical comparison gets them WRONG in
// opposite directions, so neither can pass by accident:
//
//	"16" sorts BEFORE "8", so a lexical lte would let sixteen CPUs through
//	"512Mi" sorts AFTER "32Gi", so a lexical lte would accuse half a gibibyte
//
// cost.hpa-max-replicas is a third instance of the same property — "200" sorts
// before "50" — and its own case above already covers it.
func TestCostThresholdsCompareQuantitiesNotStrings(t *testing.T) {
	rules := loadPack(t, "cost")

	cpu := packRule(t, rules, "cost.deploy-large-cpu-request")
	if got := evaluateOne(t, cpu, "Deployment", deployment("sixteen", containerRequesting(t, "cpu", "16"))); len(got) != 1 {
		t.Errorf(`a request of 16 CPUs produced %d violations of a "lte 8" rule, want 1 — the threshold is comparing bytes, not quantities`, len(got))
	}

	memory := packRule(t, rules, "cost.deploy-large-memory-request")
	if got := evaluateOne(t, memory, "Deployment", deployment("half-a-gibibyte", containerRequesting(t, "memory", "512Mi"))); len(got) != 0 {
		t.Errorf(`a request of 512Mi produced %d violations of a "lte 32Gi" rule, want 0 — the threshold is comparing bytes, not quantities`, len(got))
	}
}

// TestCostShipsNoPairedRules pins the decision that the cost pack needs none of
// the security pack's exists/value pairs, and pins the reason: every operator
// except exists and notExists skips an absent slot, so a threshold fires only
// on a value someone actually wrote. A workload that sets no CPU request is
// never accused of setting a large one, and where the API defines a default —
// three successful runs kept, one failed run kept, six retries — absence is
// already the safe value.
//
// It is the inverse of the security pack's TestPairedRulesDivideTheWork. A
// later edit that introduces a pair here has to delete this test, which makes
// it a decision rather than a drift.
func TestCostShipsNoPairedRules(t *testing.T) {
	type slot struct{ kind, path string }
	seen := map[slot]string{}

	for _, r := range loadPack(t, "cost") {
		s := slot{kind: r.Match.Kind, path: r.Assert.Path}
		if first, ok := seen[s]; ok {
			t.Errorf("%s and %s both assert %s on %s — the cost pack ships no paired rules", first, r.ID, r.Assert.Path, r.Match.Kind)
			continue
		}
		seen[s] = r.ID
	}

	// The pack uses exactly two operators. A third would be a new claim shape
	// that this test's reasoning has not been checked against.
	for _, r := range loadPack(t, "cost") {
		if r.Assert.Op != policy.OpExists && r.Assert.Op != policy.OpLte {
			t.Errorf("rule %q uses %q — the cost pack asserts only exists and lte", r.ID, r.Assert.Op)
		}
	}
}
