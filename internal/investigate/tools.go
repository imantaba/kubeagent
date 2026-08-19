package investigate

// toolSpec is one read-only tool offered to the model (backend-agnostic; the
// Anthropic backend converts these to tool params). Properties is a JSON-schema
// "properties" object.
type toolSpec struct {
	Name        string
	Description string
	Properties  any
	Required    []string
}

// prop is a single JSON-schema string property with a description.
func prop(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// toolSpecs returns the fixed read-only allowlist. Nothing else is offered.
func toolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name:        "describe",
			Description: "Read structured status of one in-scope object (pod, deployment, replicaset, statefulset, daemonset, job, node, pvc, or service). Returns phase/conditions/container states — never logs, env, or secrets, and never an address kubeagent chose (pod/host/cluster IP); quoted API text may still contain a URL the cluster wrote.",
			Properties: map[string]any{
				"kind":      prop("one of: pod, deployment, replicaset, statefulset, daemonset, job, node, pvc, service"),
				"namespace": prop("the object's namespace (empty for a node)"),
				"name":      prop("the object's name"),
			},
			Required: []string{"kind", "namespace", "name"},
		},
		{
			Name:        "get_events",
			Description: "List recent events for one in-scope object by name.",
			Properties: map[string]any{
				"namespace": prop("the object's namespace"),
				"name":      prop("the object's name"),
			},
			Required: []string{"namespace", "name"},
		},
		{
			Name:        "get_related",
			Description: "From an in-scope pod, resolve a related object and bring it into scope: the owners its ownerReferences name, its node, its PersistentVolumeClaims, or the Services whose selectors match its labels.",
			Properties: map[string]any{
				"namespace": prop("the pod's namespace"),
				"name":      prop("the pod's name"),
				"relation":  prop("one of: owner, node, pvc, service"),
			},
			Required: []string{"namespace", "name", "relation"},
		},
		{
			Name:        "get_log_causes",
			Description: "Classify the previous-instance log tail (last 25 lines) of an in-scope pod's container into a plain-language crash cause. Returns only the classified cause string — never a raw log line.",
			Properties: map[string]any{
				"namespace": prop("the pod's namespace"),
				"pod":       prop("the pod's name"),
				"container": prop("the container name within the pod"),
			},
			Required: []string{"namespace", "pod", "container"},
		},
	}
}
