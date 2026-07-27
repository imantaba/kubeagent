package gitops

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assessArgo reads an Argo CD Application's convergence.
//
// Argo publishes drift directly — status.sync.status is OutOfSync or it is not —
// but it does NOT publish how long an Application has been out of sync. No such
// timestamp exists. The only honest anchor is status.operationState.finishedAt,
// when the last sync operation finished, so the detail reads "last synced 6d ago"
// and never "drifted for 6d".
func assessArgo(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	switch nestedString(obj, "status", "sync", "status") {
	case "Synced":
		return assessment{State: StateSynced}
	case "OutOfSync":
		// fall through
	default:
		return assessment{State: StateUnknown, Detail: "sync state unreported"}
	}

	head := "OutOfSync " + ShortRevision(nestedString(obj, "status", "sync", "revision"))

	// Checked only under OutOfSync: an Application that recovered from a failed
	// operation and is now Synced must not be flagged for its history.
	if phase := nestedString(obj, "status", "operationState", "phase"); phase == "Failed" || phase == "Error" {
		return assessment{State: StateBlocked, Detail: head + ", last sync failed"}
	}

	age, known := ageOf(parseTime(nestedString(obj, "status", "operationState", "finishedAt")), now)
	when := "age unknown"
	if known {
		when = "last synced " + shortAge(age) + " ago"
	}
	if !hasAutoSync(obj) {
		return assessment{State: StateBlocked, Detail: head + ", " + when + " (auto-sync off)"}
	}
	return assessment{State: byAge(age, known, threshold), Detail: head + ", " + when}
}

// hasAutoSync reports whether spec.syncPolicy.automated is present, which decides
// whether the Application can converge without a human.
//
// Reading a bool-shaped decision out of spec is established precedent —
// internal/operators reads spec.suspend the same way. Rendering a spec string is
// not, and this package renders none.
func hasAutoSync(obj map[string]any) bool {
	_, found, err := unstructured.NestedMap(obj, "spec", "syncPolicy", "automated")
	return err == nil && found
}
