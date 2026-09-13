package hypothesis

import "github.com/imantaba/kubeagent/internal/inventory"

// group names what a confirmed cause points at, so that two workloads
// confirmed on the same upstream object share one line. The text is the
// candidate's own cause, except for a PVC whose reason is about the class
// rather than the claim — then the fresh claim's storage class is the key,
// and only when its name is plain enough to print.
func group(w inventory.Workload, h inventory.Hypothesis, reads Reads) (key, text string) {
	switch h.Kind {
	case "node":
		return "node/" + h.Object, h.Cause
	case "registry":
		return "registry/" + h.Object, h.Cause
	case "pvc":
		reason := parenthesized(h.Cause)
		if reason == "ProvisionerNotResponding" || reason == "MissingStorageClass" {
			if pvc := reads.PVCs[w.Namespace+"/"+h.Object]; pvc != nil && pvc.Spec.StorageClassName != nil &&
				*pvc.Spec.StorageClassName != "" && plainName(*pvc.Spec.StorageClassName) {
				class := *pvc.Spec.StorageClassName
				return "storageclass/" + class + "/" + reason, "storage class " + class + " (" + reason + ")"
			}
		}
		return "pvc/" + w.Namespace + "/" + h.Object, h.Cause
	}
	return "", ""
}

// plainName reports whether s is non-empty and uses only [a-z0-9.-] — the
// shape a storage class name has when it is safe to print verbatim.
func plainName(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return false
		}
	}
	return s != ""
}
