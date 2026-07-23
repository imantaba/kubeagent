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
			Description: "Read structured status of one in-scope object (pod, deployment, replicaset, statefulset, daemonset, job, node, or pvc). Returns phase/conditions/container states — never logs, IPs, env, or secrets.",
			Properties: map[string]any{
				"kind":      prop("one of: pod, deployment, replicaset, statefulset, daemonset, job, node, pvc"),
				"namespace": prop("the object's namespace (empty for a node)"),
				"name":      prop("the object's name"),
			},
			Required: []string{"kind", "name"},
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
			Description: "From an in-scope pod, resolve a related object and bring it into scope: its owner (ReplicaSet/Deployment/Job), its node, or its PersistentVolumeClaims.",
			Properties: map[string]any{
				"namespace": prop("the pod's namespace"),
				"name":      prop("the pod's name"),
				"relation":  prop("one of: owner, node, pvc"),
			},
			Required: []string{"namespace", "name", "relation"},
		},
	}
}
