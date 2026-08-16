package pvchealth

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/fuzzgen"
	"github.com/imantaba/kubeagent/internal/safetext"
)

// The claims, volumes and events are built here rather than in internal/fuzzgen
// because this is their only caller. fuzzgen owns the shapes several targets
// share and the cursor primitives every target draws from.

// fuzzBase is the instant timestamps are drawn around. Fixed, because a fuzz
// target that reads the wall clock is not reproducible.
var fuzzBase = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// fuzzPVCs draws the four inputs Assess takes. A StorageClass name and an access
// mode are API-validated, so they are drawn from their own alphabets; an event's
// message is not validated, so it is drawn from hostile bytes.
func fuzzPVCs(c *fuzzgen.Cursor) ([]corev1.PersistentVolumeClaim, []corev1.Event, []storagev1.StorageClass, []corev1.PersistentVolume) {
	ns := c.Name(20)
	scName := c.Name(20)
	sizes := []string{"1Gi", "10Gi", "500Mi", "0"}
	modes := []string{"ReadWriteOnce", "ReadOnlyMany", "ReadWriteMany"}

	var pvcs []corev1.PersistentVolumeClaim
	names := make([]string, 0, 3)
	for i := 0; i < c.IntN(4); i++ {
		name := c.Name(20)
		names = append(names, name)
		pvc := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.PersistentVolumeClaimPhase(c.Pick([]string{"Pending", "Bound", "Lost"})),
			},
		}
		// Three shapes matter to structuralCause: an unset class, the empty
		// string (a static claim), and a named one that may or may not exist.
		switch c.IntN(3) {
		case 0:
		case 1:
			empty := ""
			pvc.Spec.StorageClassName = &empty
		default:
			named := scName
			pvc.Spec.StorageClassName = &named
		}
		pvc.Spec.Resources.Requests = corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(c.Pick(sizes)),
		}
		for k := 0; k < c.IntN(3); k++ {
			pvc.Spec.AccessModes = append(pvc.Spec.AccessModes, corev1.PersistentVolumeAccessMode(c.Pick(modes)))
		}
		pvcs = append(pvcs, pvc)
	}
	if len(names) == 0 {
		names = append(names, c.Name(20))
	}

	var evs []corev1.Event
	for i := 0; i < c.IntN(4); i++ {
		evs = append(evs, corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: ns, Name: c.Name(20)},
			Reason:        c.Pick([]string{"ProvisioningFailed", "FailedBinding", "ExternalProvisioning"}),
			Message:       c.Hostile(128),
			LastTimestamp: c.Time(fuzzBase),
			InvolvedObject: corev1.ObjectReference{
				Kind:      c.Pick([]string{"PersistentVolumeClaim", "Pod"}),
				Namespace: ns,
				Name:      c.Pick(names),
			},
		})
	}

	var scs []storagev1.StorageClass
	if c.Bool() {
		scs = append(scs, storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: scName}})
	}

	var pvs []corev1.PersistentVolume
	for i := 0; i < c.IntN(3); i++ {
		pv := corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: c.Name(20)},
			Status: corev1.PersistentVolumeStatus{
				Phase: corev1.PersistentVolumePhase(c.Pick([]string{"Available", "Bound", "Released"})),
			},
		}
		pv.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(c.Pick(sizes))}
		for k := 0; k < c.IntN(3); k++ {
			pv.Spec.AccessModes = append(pv.Spec.AccessModes, corev1.PersistentVolumeAccessMode(c.Pick(modes)))
		}
		if c.Bool() {
			pv.Spec.StorageClassName = scName
		}
		pvs = append(pvs, pv)
	}
	return pvcs, evs, scs, pvs
}

// FuzzPVCAssess feeds hostile ProvisioningFailed/FailedBinding event text to a
// PVC issue's detail. The reason is asserted against the five literals this
// package writes reachable from this generator, because it is deliberately not
// sanitized: two of them come from an event, and the filter that admits them is
// what bounds the field. PVSelectorMismatch is a sixth literal the package can
// return, but fuzzPVCs never sets a claim's selector or a PV's labels, so it is
// unreachable here.
func FuzzPVCAssess(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte("\x1b[2J\x1b[H"))
	f.Add([]byte("\xff\xfe\xfd"))
	f.Add([]byte("‮​"))

	reasons := map[string]bool{
		"ProvisioningFailed": true, "FailedBinding": true,
		"MissingStorageClass": true, "NoMatchingPV": true,
		"ProvisionerNotResponding": true,
	}

	f.Fuzz(func(t *testing.T, params []byte) {
		pvcs, evs, scs, pvs := fuzzPVCs(fuzzgen.New(params))
		got := Assess(pvcs, evs, scs, pvs, 10*time.Minute, fuzzBase)

		for _, iss := range got {
			fuzzgen.AssertSafe(t, "issue.detail", iss.Detail)
			fuzzgen.AssertSafe(t, "issue.reason", iss.Reason)
			fuzzgen.AssertSafe(t, "issue.storageClass", iss.StorageClass)
			if !reasons[iss.Reason] {
				t.Errorf("Reason = %q, want one of this package's reachable literals", iss.Reason)
			}
			// The detail on the event path is exactly one sanitized value, and
			// on the structural path a phrase composed from bounded parts —
			// either way it must fit inside one line's budget.
			fuzzgen.AssertBounded(t, "issue.detail", iss.Detail, safetext.MaxLine)
		}

		again := Assess(pvcs, evs, scs, pvs, 10*time.Minute, fuzzBase)
		if !reflect.DeepEqual(got, again) {
			t.Errorf("Assess is not deterministic")
		}
	})
}
