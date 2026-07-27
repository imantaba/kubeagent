package capacity

import (
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// This is the chaos-proven bug. When the API server admits a Pod it copies each
// unset request from the corresponding limit, so a stored Pod carrying
// {"limits":{"memory":"256Mi"}} in its author's manifest is persisted as
// {"limits":{"memory":"256Mi"},"requests":{"memory":"256Mi"}}. RuleLimitNoRequest
// used to read Pods, so the shape it looked for (a limit with NO matching request)
// never occurred there — the rule fired only in hand-built test pods, never
// against a real cluster. The authored shape survives only in the Deployment's own
// pod template, which admission never rewrites.
//
// Before the fix this test fails with "want rule ... got []" — no row at all,
// exactly chaos scenario 18's finding.
func TestRuleLimitNoRequestFiresFromTemplateNotDefaultedPod(t *testing.T) {
	// The Pod as the API server actually stores it: request defaulted from limit.
	p := ownedBy(pod("prod", "cache-1", "worker1", "", ""), "ReplicaSet", "cache-abc")
	p.Spec.Containers = []corev1.Container{container("app", "", "256Mi", "", "256Mi")}
	rs := []appsv1.ReplicaSet{replicaSet("prod", "cache-abc", "cache")}

	// The Deployment's own template: the authored shape, limit only.
	deployments := []appsv1.Deployment{deployment("prod", "cache", container("app", "", "", "", "256Mi"))}
	templates := Templates(deployments, nil, nil, nil, nil)

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, rs, templates, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want exactly 1 row sourced from the template, got %+v", r.Owners)
	}
	if r.Owners[0].Kind != "Deployment" || r.Owners[0].Namespace != "prod" || r.Owners[0].Name != "cache" {
		t.Errorf("want Deployment/prod/cache, got %+v", r.Owners[0])
	}
	if !strings.Contains(r.Owners[0].Detail, "lim 256Mi") {
		t.Errorf("want the limit named, got %q", r.Owners[0].Detail)
	}
}

// Each of the five workload kinds Templates() reads reaches RuleLimitNoRequest
// carrying its own Kind in the row.
func TestTemplatesEachOwnerKindReachesRuleWithOwnKind(t *testing.T) {
	limitOnly := container("app", "", "", "", "256Mi")
	cases := []struct {
		name      string
		wantKind  string
		templates []OwnerTemplate
	}{
		{"Deployment", "Deployment",
			Templates([]appsv1.Deployment{deployment("prod", "d1", limitOnly)}, nil, nil, nil, nil)},
		{"StatefulSet", "StatefulSet",
			Templates(nil, []appsv1.StatefulSet{statefulSet("prod", "s1", limitOnly)}, nil, nil, nil)},
		{"DaemonSet", "DaemonSet",
			Templates(nil, nil, []appsv1.DaemonSet{daemonSet("prod", "ds1", limitOnly)}, nil, nil)},
		{"Job", "Job",
			Templates(nil, nil, nil, []batchv1.Job{job("prod", "j1", limitOnly)}, nil)},
		{"CronJob", "CronJob",
			Templates(nil, nil, nil, nil, []batchv1.CronJob{cronJob("prod", "cj1", limitOnly)})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, nil, nil, c.templates, nil, "")
			r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
			if len(r.Owners) != 1 || r.Owners[0].Kind != c.wantKind {
				t.Errorf("want 1 owner of Kind %s, got %+v", c.wantKind, r.Owners)
			}
		})
	}
}

// A Job's template is a copy of the CronJob's that created it, so counting both
// would report one authored mistake twice. Templates() skips a Job carrying a
// CronJob ownerReference; the CronJob itself is the source.
func TestTemplatesJobOwnedByCronJobYieldsOneRowFromCronJob(t *testing.T) {
	limitOnly := container("app", "", "", "", "256Mi")
	cj := cronJob("prod", "nightly", limitOnly)
	j := jobOwnedBy(job("prod", "nightly-28000000", limitOnly), "CronJob", "nightly")

	templates := Templates(nil, nil, nil, []batchv1.Job{j}, []batchv1.CronJob{cj})
	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, nil, nil, templates, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want exactly 1 row (from the CronJob, not the Job), got %+v", r.Owners)
	}
	if r.Owners[0].Kind != "CronJob" || r.Owners[0].Name != "nightly" {
		t.Errorf("want CronJob/prod/nightly, got %+v", r.Owners[0])
	}
}

// A bare Job (no CronJob owner) is still a source in its own right.
func TestTemplatesBareJobIsAStillASource(t *testing.T) {
	limitOnly := container("app", "", "", "", "256Mi")
	templates := Templates(nil, nil, nil, []batchv1.Job{job("batch", "backfill", limitOnly)}, nil)

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, nil, nil, templates, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 || r.Owners[0].Kind != "Job" || r.Owners[0].Name != "backfill" {
		t.Errorf("want Job/batch/backfill, got %+v", r.Owners)
	}
}

// Templates() is not given ReplicaSets at all — a Deployment's own template already
// carries the authored shape, so adding its ReplicaSets would report the same
// authored mistake twice under two owner identities. A Deployment scaled to several
// replicas (Deployment -> ReplicaSet -> Pods) still yields exactly one row.
func TestTemplatesDeploymentAndReplicaSetYieldOneRowNotTwo(t *testing.T) {
	deployments := []appsv1.Deployment{deployment("prod", "cache", container("app", "", "", "", "256Mi"))}
	templates := Templates(deployments, nil, nil, nil, nil)
	rs := []appsv1.ReplicaSet{replicaSet("prod", "cache-abc", "cache")}
	var pods []corev1.Pod
	for i := 0; i < 3; i++ {
		p := ownedBy(pod("prod", fmt.Sprintf("cache-%d", i), "worker1", "", ""), "ReplicaSet", "cache-abc")
		p.Spec.Containers = []corev1.Container{container("app", "", "256Mi", "", "256Mi")} // as stored, defaulted
		pods = append(pods, p)
	}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, rs, templates, nil, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want exactly 1 row for the Deployment, not one per ReplicaSet/Pod, got %+v", r.Owners)
	}
	if r.Owners[0].Kind != "Deployment" || r.Owners[0].Name != "cache" {
		t.Errorf("want Deployment/prod/cache, got %+v", r.Owners[0])
	}
}

// -n scopes the template enumeration the same way it scopes pods.
func TestRuleLimitNoRequestNamespaceScoping(t *testing.T) {
	deployments := []appsv1.Deployment{deployment("staging", "cache", container("app", "", "", "", "256Mi"))}
	templates := Templates(deployments, nil, nil, nil, nil)

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, nil, nil, templates, nil, "prod")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleLimitNoRequest {
				t.Errorf("want no row for a template outside the -n scope, got %+v", r.Owners)
			}
		}
	}
}

// A bare Pod (no controller, so no template exists) with a limit and no request
// produces no RuleLimitNoRequest row. This is a deliberate, documented consequence
// of reading templates rather than Pods — not an oversight: the API server has
// already defaulted the Pod's own spec, so the shape this rule looks for cannot
// exist there even for an ownerless Pod.
func TestRuleLimitNoRequestBarePodProducesNoRow(t *testing.T) {
	p := pod("prod", "loose", "worker1", "", "")
	p.Spec.Containers = []corev1.Container{container("app", "", "256Mi", "", "256Mi")} // defaulted, as stored

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, []corev1.Pod{p}, nil, nil, nil, "")

	if rep.RightSizing != nil {
		for _, r := range rep.RightSizing.Rules {
			if r.Name == RuleLimitNoRequest {
				t.Errorf("want no RuleLimitNoRequest row for a bare Pod with no template, got %+v", r.Owners)
			}
		}
	}
}

// attachSamples keys owners as Kind/Namespace/Name, and a template-derived key has
// that exact form for Deployment/StatefulSet/DaemonSet/Job rows — Deployment is
// exercised end-to-end in TestObservedForLimitNoRequestPairsWithFlaggedResource*
// (sample_test.go). A CronJob row is the one exception: its key is
// "CronJob/ns/name" while its pods roll up through the Job to "Job/ns/jobname", so
// no sample ever attaches. That is a known limitation (see attachSamples), not a
// bug — the row is still true and still renders, just without an Observed reading.
func TestSampleDoesNotAttachToCronJobRow(t *testing.T) {
	cj := cronJob("prod", "nightly", container("app", "", "", "", "256Mi"))
	templates := Templates(nil, nil, nil, nil, []batchv1.CronJob{cj})
	pods := []corev1.Pod{ownedBy(pod("prod", "nightly-28000000-abcde", "worker1", "", ""), "Job", "nightly-28000000")}
	usage := map[string]corev1.ResourceList{"prod/nightly-28000000-abcde": usageOf("50m", "64Mi")}

	rep := Assess([]corev1.Node{node("worker1", "4", "16Gi")}, pods, nil, templates, usage, "")

	r := ruleByName(t, rep.RightSizing, RuleLimitNoRequest)
	if len(r.Owners) != 1 {
		t.Fatalf("want 1 owner, got %+v", r.Owners)
	}
	if r.Owners[0].Observed != "" {
		t.Errorf("want no observed reading on a CronJob row (its pods roll up to Job, not CronJob), got %q",
			r.Owners[0].Observed)
	}
}
