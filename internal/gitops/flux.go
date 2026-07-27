package gitops

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// assessKustomization reads a Flux Kustomization's convergence.
func assessKustomization(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessFlux(obj, now, threshold, true)
}

// assessHelmRelease reads a Flux HelmRelease's convergence, minus the revision
// comparison: helm.toolkit.fluxcd.io/v2 has no status.lastAppliedRevision (it
// existed in v2beta1 and was removed), so there is no honest attempted-vs-applied
// signal to read. Inventing one would flag every healthy release.
func assessHelmRelease(obj map[string]any, now time.Time, threshold time.Duration) assessment {
	return assessFlux(obj, now, threshold, false)
}

// assessFlux reads the convergence both Flux kinds share.
//
// Flux never says "OutOfSync" — it reapplies continuously — so its drift signal
// is indirect: a suspended object has stopped reconciling, a stalled one has
// given up, and a Ready=False one is failing to land what Git says. The age
// anchor for all three is the Ready condition's lastTransitionTime, which is how
// long the object has held its current reconciliation state.
func assessFlux(obj map[string]any, now time.Time, threshold time.Duration, checkRevisions bool) assessment {
	if v, found, err := unstructured.NestedBool(obj, "spec", "suspend"); err == nil && found && v {
		return assessment{State: StateBlocked, Detail: "suspended"}
	}
	if status, reason, _, found := condition(obj, "Stalled"); found && status == "True" {
		return assessment{State: StateBlocked, Detail: withReason("stalled", reason)}
	}

	readyStatus, readyReason, changed, readyFound := condition(obj, "Ready")
	age, known := ageOf(changed, now)
	revisions := ""
	if checkRevisions {
		revisions = revisionPair(obj)
	}

	if readyFound && readyStatus == "False" {
		detail := withReason("not ready"+forAge(age, known), readyReason)
		if revisions != "" {
			detail = revisions + ", " + detail
		}
		return assessment{State: byAge(age, known, threshold), Detail: detail}
	}
	if revisions != "" {
		return assessment{State: byAge(age, known, threshold), Detail: revisions + ", unchanged" + forAge(age, known)}
	}
	if readyFound && readyStatus == "True" {
		return assessment{State: StateSynced}
	}
	return assessment{State: StateUnknown, Detail: "Ready not reported"}
}

// revisionPair renders the attempted/applied pair when the newest revision has
// not landed, and "" when there is nothing to say. Both values pass through
// ShortRevision: Flux writes them as "<ref>@sha1:<hash>", and <ref> is arbitrary
// user text.
func revisionPair(obj map[string]any) string {
	attempted := nestedString(obj, "status", "lastAttemptedRevision")
	applied := nestedString(obj, "status", "lastAppliedRevision")
	if attempted == "" || attempted == applied {
		return ""
	}
	shown := "none"
	if applied != "" {
		shown = ShortRevision(applied)
	}
	return "attempted " + ShortRevision(attempted) + ", applied " + shown
}

// forAge appends the age anchor, or says plainly that there is none. An unknown
// age reads as unknown; it is never silently treated as zero or as stale.
func forAge(age time.Duration, known bool) string {
	if !known {
		return ", age unknown"
	}
	return " " + shortAge(age)
}

// withReason appends the reconciler's own condition reason — a CamelCase token by
// API convention. The condition message is never appended: it is arbitrary prose
// that routinely quotes the repository URL.
func withReason(head, reason string) string {
	if reason == "" {
		return head
	}
	return head + ": " + reason
}
