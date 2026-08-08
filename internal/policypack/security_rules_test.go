package policypack_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/imantaba/kubeagent/internal/policy"
)

// TestSecurityPackKindDistribution pins the pack's scope decision: the full
// rule set runs against Deployment, and only the two host-escape claims spill
// over to the kinds where they bite as hard. It is not a cross-product, and it
// deliberately never selects Pod or Job — a controller-owned pod repeats its
// workload's violation once per replica, and a report naming one Deployment is
// actionable in a way that a report naming forty pods is not.
func TestSecurityPackKindDistribution(t *testing.T) {
	byKind := map[string]int{}
	for _, r := range loadPack(t, "security") {
		byKind[r.Match.Kind]++
	}

	want := map[string]int{
		"Deployment":  18,
		"StatefulSet": 2,
		"DaemonSet":   2,
		"CronJob":     1,
	}
	for kind, n := range want {
		if byKind[kind] != n {
			t.Errorf("%d rules select %s, want %d", byKind[kind], kind, n)
		}
	}
	for kind := range byKind {
		if _, ok := want[kind]; !ok {
			t.Errorf("the pack selects %s, which is not one of the four workload kinds it is scoped to", kind)
		}
	}

	total := 0
	for _, n := range byKind {
		total += n
	}
	if total != 23 {
		t.Errorf("the pack holds %d rules, want 23", total)
	}
}

// hardenedContainer satisfies every container-level rule in the security pack.
// Each case below starts from it and changes or removes exactly the one thing
// its rule is about, so a case can only fail for its own reason.
//
// capabilities carries drop but no add: an absent add[*] is one absent slot,
// which notIn skips — which is the correct satisfying side, since the rule is
// about a capability that was added, not about one that was not dropped.
func hardenedContainer() map[string]any {
	return map[string]any{
		"name":  "app",
		"image": fixtureImage,
		"ports": []any{map[string]any{"containerPort": int64(8080)}},
		"securityContext": map[string]any{
			"privileged":               false,
			"allowPrivilegeEscalation": false,
			"runAsNonRoot":             true,
			"runAsUser":                int64(1000),
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
		},
	}
}

// containerMinus returns a hardened container with one field removed, walking
// the path so a nested field can be removed too:
// containerMinus(t, "securityContext", "runAsNonRoot").
func containerMinus(t *testing.T, path ...string) map[string]any {
	t.Helper()
	c := hardenedContainer()
	m := c
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("containerMinus(%v): %q is not a map", path, k)
		}
		m = next
	}
	delete(m, path[len(path)-1])
	return c
}

// containerWithSecurityContext returns a hardened container with one
// securityContext field set explicitly to v. Setting it explicitly is the
// whole point: every operator except exists and notExists SKIPS an absent
// slot, so a fixture that merely omitted the field would make the rule say
// nothing, which is not a pass.
func containerWithSecurityContext(t *testing.T, key string, v any) map[string]any {
	t.Helper()
	c := hardenedContainer()
	sc, ok := c["securityContext"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no securityContext map")
	}
	sc[key] = v
	return c
}

// containerWithAddedCapability returns a hardened container that adds one
// Linux capability.
func containerWithAddedCapability(t *testing.T, name string) map[string]any {
	t.Helper()
	c := hardenedContainer()
	sc, ok := c["securityContext"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no securityContext map")
	}
	caps, ok := sc["capabilities"].(map[string]any)
	if !ok {
		t.Fatal("hardenedContainer has no capabilities map")
	}
	caps["add"] = []any{name}
	return c
}

// containerWithHostPort returns a hardened container whose port is bound on
// the node itself.
func containerWithHostPort() map[string]any {
	c := hardenedContainer()
	c["ports"] = []any{map[string]any{"containerPort": int64(8080), "hostPort": int64(8080)}}
	return c
}

// hardenedPodSpec satisfies every pod-level rule in the security pack. It
// carries a volume deliberately: a spec with no volumes at all would satisfy
// the hostPath rule vacuously, so the satisfying side proves that an ordinary
// volume passes rather than that no volume was looked at.
func hardenedPodSpec(c map[string]any) map[string]any {
	return map[string]any{
		"serviceAccountName":           "app",
		"automountServiceAccountToken": false,
		"hostNetwork":                  false,
		"hostPID":                      false,
		"hostIPC":                      false,
		"securityContext": map[string]any{
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"volumes":    []any{map[string]any{"name": "cache", "emptyDir": map[string]any{}}},
		"containers": []any{c},
	}
}

// podSpecWith returns a hardened pod spec with one top-level field set
// explicitly to v.
func podSpecWith(c map[string]any, key string, v any) map[string]any {
	spec := hardenedPodSpec(c)
	spec[key] = v
	return spec
}

// podSpecWithout returns a hardened pod spec with one top-level field removed.
func podSpecWithout(c map[string]any, key string) map[string]any {
	spec := hardenedPodSpec(c)
	delete(spec, key)
	return spec
}

// podSpecWithSeccomp returns a hardened pod spec whose seccomp profile type is
// set explicitly to v.
func podSpecWithSeccomp(c map[string]any, v string) map[string]any {
	spec := hardenedPodSpec(c)
	spec["securityContext"] = map[string]any{"seccompProfile": map[string]any{"type": v}}
	return spec
}

// podSpecWithHostPathVolume returns a hardened pod spec carrying a second
// volume that mounts a path from the node.
func podSpecWithHostPathVolume(c map[string]any) map[string]any {
	spec := hardenedPodSpec(c)
	spec["volumes"] = []any{
		map[string]any{"name": "cache", "emptyDir": map[string]any{}},
		map[string]any{"name": "host-root", "hostPath": map[string]any{"path": "/"}},
	}
	return spec
}

// hardenedWorkload builds a Deployment, StatefulSet or DaemonSet around one
// pod spec.
func hardenedWorkload(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"replicas": int64(2),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
				"spec":     spec,
			},
		},
	}}
}

// hardenedDeployment is the shorthand the eighteen Deployment cases use.
func hardenedDeployment(name string, spec map[string]any) *unstructured.Unstructured {
	return hardenedWorkload("Deployment", name, spec)
}

// hardenedCronJob wraps a pod spec in the batch/v1 shape, whose template lives
// one level deeper than a Deployment's: spec.jobTemplate.spec.template.spec.
func hardenedCronJob(name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata":   map[string]any{"name": name, "namespace": fixtureNamespace},
		"spec": map[string]any{
			"schedule": "*/5 * * * *",
			"jobTemplate": map[string]any{
				"spec": map[string]any{
					"template": map[string]any{
						"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
						"spec":     spec,
					},
				},
			},
		},
	}}
}

// evaluateOne runs exactly one rule against exactly one object and returns its
// violations, failing the test if anything was unreadable.
func evaluateOne(t *testing.T, r policy.Rule, kind string, obj *unstructured.Unstructured) []policy.Violation {
	t.Helper()
	objects := map[string][]*unstructured.Unstructured{kind: {obj}}
	violations, notEvaluated := policy.Evaluate([]policy.Rule{r}, policy.InputsFrom(objects, nil))
	if len(notEvaluated) != 0 {
		t.Fatalf("nothing was unreadable, got %#v", notEvaluated)
	}
	return violations
}

// packRule finds one rule in a pack by id.
func packRule(t *testing.T, rules []policy.Rule, id string) policy.Rule {
	t.Helper()
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("the pack has no rule %q", id)
	return policy.Rule{}
}

// TestEverySecurityRuleFiresAndPasses drives each rule through the real
// evaluator, alone, against an object that must violate it and one that must
// not. A rule with a typo'd path or the wrong operator loads cleanly and
// checks nothing; this is what catches that.
func TestEverySecurityRuleFiresAndPasses(t *testing.T) {
	rules := loadPack(t, "security")

	cases := []ruleCase{
		{
			id:         "security.deploy-privileged",
			kind:       "Deployment",
			violating:  hardenedDeployment("privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-privilege-escalation-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("escalation-unset", hardenedPodSpec(containerMinus(t, "securityContext", "allowPrivilegeEscalation"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-privilege-escalation",
			kind:       "Deployment",
			violating:  hardenedDeployment("escalates", hardenedPodSpec(containerWithSecurityContext(t, "allowPrivilegeEscalation", true))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-run-as-non-root-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("non-root-unset", hardenedPodSpec(containerMinus(t, "securityContext", "runAsNonRoot"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-run-as-non-root",
			kind:       "Deployment",
			violating:  hardenedDeployment("may-be-root", hardenedPodSpec(containerWithSecurityContext(t, "runAsNonRoot", false))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-run-as-root-uid",
			kind: "Deployment",
			// runAsUser is set explicitly: gt SKIPS an absent field, so a
			// fixture that omitted it would make the rule pass for the wrong
			// reason.
			violating:  hardenedDeployment("uid-zero", hardenedPodSpec(containerWithSecurityContext(t, "runAsUser", int64(0)))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-read-only-root-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("read-only-unset", hardenedPodSpec(containerMinus(t, "securityContext", "readOnlyRootFilesystem"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-read-only-root",
			kind:       "Deployment",
			violating:  hardenedDeployment("writable-root", hardenedPodSpec(containerWithSecurityContext(t, "readOnlyRootFilesystem", false))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-added-capabilities",
			kind: "Deployment",
			// The satisfying side adds a capability outside the blocked list, so
			// it proves the comparison ran rather than that the slot was absent.
			violating:  hardenedDeployment("sys-admin", hardenedPodSpec(containerWithAddedCapability(t, "SYS_ADMIN"))),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(containerWithAddedCapability(t, "CHOWN"))),
		},
		{
			id:   "security.deploy-host-path-volume",
			kind: "Deployment",
			// The satisfying side carries an emptyDir volume, so it proves an
			// ordinary volume passes rather than that no volume was looked at.
			violating:  hardenedDeployment("host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:   "security.deploy-host-port",
			kind: "Deployment",
			// The satisfying side declares a containerPort, so it proves a
			// port without hostPort passes rather than that no port existed.
			violating:  hardenedDeployment("node-port", hardenedPodSpec(containerWithHostPort())),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-network",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-network", podSpecWith(hardenedContainer(), "hostNetwork", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-pid",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-pid", podSpecWith(hardenedContainer(), "hostPID", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-host-ipc",
			kind:       "Deployment",
			violating:  hardenedDeployment("host-ipc", podSpecWith(hardenedContainer(), "hostIPC", true)),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-seccomp-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("seccomp-unset", podSpecWithout(hardenedContainer(), "securityContext")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-seccomp-unconfined",
			kind:       "Deployment",
			violating:  hardenedDeployment("unconfined", podSpecWithSeccomp(hardenedContainer(), "Unconfined")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-service-account-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("default-sa", podSpecWithout(hardenedContainer(), "serviceAccountName")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.deploy-automount-token-unset",
			kind:       "Deployment",
			violating:  hardenedDeployment("automount-unset", podSpecWithout(hardenedContainer(), "automountServiceAccountToken")),
			satisfying: hardenedDeployment("ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.statefulset-privileged",
			kind:       "StatefulSet",
			violating:  hardenedWorkload("StatefulSet", "privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedWorkload("StatefulSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.statefulset-host-path-volume",
			kind:       "StatefulSet",
			violating:  hardenedWorkload("StatefulSet", "host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedWorkload("StatefulSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.daemonset-privileged",
			kind:       "DaemonSet",
			violating:  hardenedWorkload("DaemonSet", "privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedWorkload("DaemonSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.daemonset-host-path-volume",
			kind:       "DaemonSet",
			violating:  hardenedWorkload("DaemonSet", "host-root", podSpecWithHostPathVolume(hardenedContainer())),
			satisfying: hardenedWorkload("DaemonSet", "ok", hardenedPodSpec(hardenedContainer())),
		},
		{
			id:         "security.cronjob-privileged",
			kind:       "CronJob",
			violating:  hardenedCronJob("privileged", hardenedPodSpec(containerWithSecurityContext(t, "privileged", true))),
			satisfying: hardenedCronJob("ok", hardenedPodSpec(hardenedContainer())),
		},
	}

	if len(cases) != len(rules) {
		t.Fatalf("%d cases for %d rules — every rule must be proved from both sides", len(cases), len(rules))
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			r := packRule(t, rules, tc.id)

			violations := evaluateOne(t, r, tc.kind, tc.violating)
			if len(violations) != 1 {
				t.Fatalf("violating object produced %d violations, want 1", len(violations))
			}
			if violations[0].RuleID != tc.id {
				t.Errorf("violation is from rule %q, want %q", violations[0].RuleID, tc.id)
			}
			if violations[0].Level == policy.LevelCritical {
				t.Errorf("violation is critical — no pack rule may be")
			}

			if violations := evaluateOne(t, r, tc.kind, tc.satisfying); len(violations) != 0 {
				t.Errorf("satisfying object produced %d violations, want 0: %#v", len(violations), violations)
			}
		})
	}
}

// TestPairedRulesDivideTheWork pins the pack's pairing principle. Four
// properties are unsafe when absent AND unsafe when set to the wrong value,
// and no single rule can catch both: exists says nothing about a bad value,
// and notIn SKIPS an absent slot. So each ships as a pair, and each half must
// cover exactly the case the other cannot.
//
// Asserting both directions is the point. A single-direction test would still
// pass if someone collapsed a pair into one rule, which is precisely the
// change this exists to fail.
func TestPairedRulesDivideTheWork(t *testing.T) {
	rules := loadPack(t, "security")

	cases := []struct {
		property string
		unsetID  string
		valueID  string
		// unset is missing the field entirely.
		unset *unstructured.Unstructured
		// bad sets the field explicitly to the unsafe value.
		bad *unstructured.Unstructured
	}{
		{
			property: "allowPrivilegeEscalation",
			unsetID:  "security.deploy-privilege-escalation-unset",
			valueID:  "security.deploy-privilege-escalation",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "allowPrivilegeEscalation"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "allowPrivilegeEscalation", true))),
		},
		{
			property: "runAsNonRoot",
			unsetID:  "security.deploy-run-as-non-root-unset",
			valueID:  "security.deploy-run-as-non-root",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "runAsNonRoot"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "runAsNonRoot", false))),
		},
		{
			property: "readOnlyRootFilesystem",
			unsetID:  "security.deploy-read-only-root-unset",
			valueID:  "security.deploy-read-only-root",
			unset:    hardenedDeployment("unset", hardenedPodSpec(containerMinus(t, "securityContext", "readOnlyRootFilesystem"))),
			bad:      hardenedDeployment("bad", hardenedPodSpec(containerWithSecurityContext(t, "readOnlyRootFilesystem", false))),
		},
		{
			property: "seccompProfile",
			unsetID:  "security.deploy-seccomp-unset",
			valueID:  "security.deploy-seccomp-unconfined",
			unset:    hardenedDeployment("unset", podSpecWithout(hardenedContainer(), "securityContext")),
			bad:      hardenedDeployment("bad", podSpecWithSeccomp(hardenedContainer(), "Unconfined")),
		},
	}

	if len(cases) != 4 {
		t.Fatalf("%d paired properties, want 4", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.property, func(t *testing.T) {
			unsetRule := packRule(t, rules, tc.unsetID)
			valueRule := packRule(t, rules, tc.valueID)

			// The unset object: only the exists half may fire.
			if got := evaluateOne(t, unsetRule, "Deployment", tc.unset); len(got) != 1 {
				t.Errorf("%s produced %d violations on an object missing %s, want 1", tc.unsetID, len(got), tc.property)
			}
			if got := evaluateOne(t, valueRule, "Deployment", tc.unset); len(got) != 0 {
				t.Errorf("%s produced %d violations on an object missing %s, want 0 — a value operator must skip an absent slot", tc.valueID, len(got), tc.property)
			}

			// The explicitly-bad object: only the value half may fire.
			if got := evaluateOne(t, valueRule, "Deployment", tc.bad); len(got) != 1 {
				t.Errorf("%s produced %d violations on an object setting %s to the unsafe value, want 1", tc.valueID, len(got), tc.property)
			}
			if got := evaluateOne(t, unsetRule, "Deployment", tc.bad); len(got) != 0 {
				t.Errorf("%s produced %d violations on an object that DOES set %s, want 0", tc.unsetID, len(got), tc.property)
			}

			// The levels are part of the principle, not decoration: the unset
			// half is advisory, the explicit-bad half is a warning.
			if unsetRule.Level != policy.LevelInfo {
				t.Errorf("%s is %q, want info", tc.unsetID, unsetRule.Level)
			}
			if valueRule.Level != policy.LevelWarning {
				t.Errorf("%s is %q, want warning", tc.valueID, valueRule.Level)
			}
		})
	}
}
