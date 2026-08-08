package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imantaba/kubeagent/internal/baseline"
	"github.com/imantaba/kubeagent/internal/inventory"
)

// startedPod is a pod that started `age` before `now` with `restarts` restarts
// across one container.
func startedPod(ns, name string, owners []metav1.OwnerReference, restarts int32, started metav1.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, OwnerReferences: owners},
		Status: corev1.PodStatus{
			StartTime:         &started,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: restarts}},
		},
	}
}

func TestPodSamplesResolvesWorkloadAndAge(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	twoHoursAgo := metav1.NewTime(now.Add(-2 * time.Hour))

	in := inventory.Inputs{
		Pods: []corev1.Pod{
			startedPod("prod", "api-abc-1", ctrlRefCLI("ReplicaSet", "api-abc"), 3, twoHoursAgo),
			// No StartTime: it has never run, so it has observed no container
			// runtime and must not contribute pod-seconds.
			{ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "pending"}},
		},
		ReplicaSets: []appsv1.ReplicaSet{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "api-abc",
				OwnerReferences: ctrlRefCLI("Deployment", "api")},
		}},
	}

	got := podSamples(in, now)
	if len(got) != 1 {
		t.Fatalf("podSamples returned %d samples, want 1: %+v", len(got), got)
	}
	want := baseline.PodSample{Kind: "Deployment", Namespace: "prod", Name: "api", Restarts: 3, AgeSeconds: 7200}
	if got[0] != want {
		t.Errorf("sample = %+v, want %+v", got[0], want)
	}
}

func TestPodSamplesSumsRestartsAcrossContainers(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	p := startedPod("prod", "multi", nil, 0, metav1.NewTime(now.Add(-time.Hour)))
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "app", RestartCount: 2},
		{Name: "sidecar", RestartCount: 5},
	}
	got := podSamples(inventory.Inputs{Pods: []corev1.Pod{p}}, now)
	if len(got) != 1 || got[0].Restarts != 7 {
		t.Errorf("samples = %+v, want one sample with 7 restarts", got)
	}
}

func TestRunBaselineCaptureRendersADocument(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	in := inventory.Inputs{Pods: []corev1.Pod{
		startedPod("prod", "cache-0", ctrlRefCLI("StatefulSet", "cache"), 4, metav1.NewTime(now.Add(-2*time.Hour))),
	}}

	var buf bytes.Buffer
	if err := renderBaselineCapture(in, time.Hour, now, &buf); err != nil {
		t.Fatalf("renderBaselineCapture: %v", err)
	}
	var doc baseline.Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("output is not the baseline document: %v\n%s", err, buf.String())
	}
	if doc.SchemaVersion != baseline.SchemaVersion || doc.CapturedAt != "2026-01-02T12:00:00Z" {
		t.Errorf("header = %+v, want schemaVersion %q at 2026-01-02T12:00:00Z", doc, baseline.SchemaVersion)
	}
	if len(doc.Workloads) != 1 || doc.Workloads[0].Kind != "StatefulSet" || doc.Workloads[0].RestartsPerHour != 2 {
		t.Errorf("workloads = %+v, want one StatefulSet at 2 restarts/hour", doc.Workloads)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("output does not end in a newline")
	}
}

func TestBaselineReportRoundTripsThroughLoad(t *testing.T) {
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	in := inventory.Inputs{Pods: []corev1.Pod{
		startedPod("prod", "api-0", ctrlRefCLI("StatefulSet", "api"), 2, metav1.NewTime(now.Add(-2*time.Hour))),
	}}
	var buf bytes.Buffer
	if err := renderBaselineCapture(in, time.Hour, now, &buf); err != nil {
		t.Fatalf("renderBaselineCapture: %v", err)
	}
	doc, err := baseline.Load(buf.Bytes())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// The same cluster right after capture must show no deviation at all.
	rep := baselineReport(&doc, 0, 0, in, now)
	if rep == nil {
		t.Fatal("baselineReport returned nil for a loaded document")
	}
	if len(rep.Deviations) != 0 || rep.Compared != 1 {
		t.Errorf("report = %+v, want 1 compared and no deviations", rep)
	}
}

func TestLoadBaselineIsNilWithoutTheFlag(t *testing.T) {
	doc, err := loadBaseline("")
	if doc != nil || err != nil {
		t.Errorf("loadBaseline(\"\") = %v, %v; want nil, nil", doc, err)
	}
}

func TestLoadBaselineReportsAnUnreadableFile(t *testing.T) {
	if _, err := loadBaseline("/nonexistent/baseline.json"); err == nil {
		t.Error("loadBaseline accepted a path that does not exist")
	}
}

// ctrlRefCLI mirrors internal/inventory's test helper; internal/cli cannot
// reach that package's test scope.
func ctrlRefCLI(kind, name string) []metav1.OwnerReference {
	ctrl := true
	return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &ctrl}}
}
