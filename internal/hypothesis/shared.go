package hypothesis

import (
	"fmt"
	"sort"

	"github.com/imantaba/kubeagent/internal/inventory"
)

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

// MaxSharedLines caps the shared-cause lines. It equals local verdict
// mode's summary cap; a test in internal/investigate pins the two.
const MaxSharedLines = 4

// TruncationMarker marks a cut. It equals local verdict mode's marker; the
// same test pins the two.
const TruncationMarker = "[truncated by kubeagent]"

// Shared writes one line per group of two or more confirmed rows, largest
// first and then by key, at most MaxSharedLines lines plus the marker.
// With two or more confirmed rows and no group, it says so in one line.
// With fewer than two confirmed rows it writes nothing.
func Shared(results []Result) []string {
	type grp struct {
		key, text string
		n         int
	}
	var groups []*grp
	byKey := map[string]*grp{}
	confirmed := 0
	for _, r := range results {
		if !r.Decided || r.Outcome != Confirmed {
			continue
		}
		confirmed++
		if r.GroupKey == "" {
			continue
		}
		g, ok := byKey[r.GroupKey]
		if !ok {
			g = &grp{key: r.GroupKey, text: r.GroupText}
			byKey[r.GroupKey] = g
			groups = append(groups, g)
		}
		g.n++
	}
	if confirmed < 2 {
		return nil
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].n != groups[j].n {
			return groups[i].n > groups[j].n
		}
		return groups[i].key < groups[j].key
	})
	var lines []string
	for _, g := range groups {
		if g.n < 2 {
			continue
		}
		if len(lines) == MaxSharedLines {
			lines = append(lines, TruncationMarker)
			return lines
		}
		lines = append(lines, fmt.Sprintf("%d workloads share one upstream cause: %s", g.n, g.text))
	}
	if len(lines) == 0 {
		return []string{fmt.Sprintf("no shared cause among the %d workloads decided by rules", confirmed)}
	}
	return lines
}
