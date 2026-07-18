package report

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imantaba/kubeagent/internal/clusterhealth"
	"github.com/imantaba/kubeagent/internal/diagnose"
	"github.com/imantaba/kubeagent/internal/ingresshealth"
	"github.com/imantaba/kubeagent/internal/inventory"
	"github.com/imantaba/kubeagent/internal/nodehealth"
	"github.com/imantaba/kubeagent/internal/nodereserve"
	"github.com/imantaba/kubeagent/internal/pvcreclaim"
	"github.com/imantaba/kubeagent/internal/secscan"
	"github.com/imantaba/kubeagent/internal/svchealth"
)

var update = flag.Bool("update", false, "rewrite golden files")

// goldenNow is the fixed clock for the snapshot; every fixture timestamp precedes it.
var goldenNow = time.Date(2026, 7, 18, 15, 4, 5, 0, time.UTC)

const goldenPath = "testdata/golden-scan.txt"

// goldenInput builds one Input exercising every rendered section, so the golden is a
// broad snapshot of the whole report. All values are fixed literals.
func goldenInput(now time.Time) Input {
	return Input{
		Now: now,
		Cluster: clusterhealth.ClusterHealth{
			Verdict: "Degraded", NodesTotal: 4, NodesReady: 2,
			NodesStaleHeartbeat: 1, NodesExpectedAbsent: 1,
			NodeIssues: []string{
				"worker-2 NotReady: KubeletNotReady — container runtime is down",
				"worker-3 SchedulingDisabled",
				"worker-1 kubelet not heartbeating (lease 95s stale)",
				"db-01 expected but absent from the cluster",
			},
			SystemIssues: []string{"kube-system/coredns Degraded 1/2"},
		},
		Result:             inventory.Result{Workloads: goldenWorkloads()},
		Resources:          sampleSummary(),
		Platform:           sampleFacts(),
		CredentialWarnings: sampleCredWarnings(),
		// sampleServiceIssues() are "real" (NEEDS ATTENTION); the appended Expected issue
		// exercises the NOTES "•" expected-service path too.
		ServiceIssues: append(sampleServiceIssues(), svchealth.Issue{
			Namespace: "shop", Name: "internal-metrics", Type: "ClusterIP",
			Problem: "NoEndpoints", Detail: "no ready endpoints", Expected: true,
		}),
		IngressIssues: []ingresshealth.RouteIssue{{
			Namespace: "shop", Ingress: "storefront", Host: "shop.example.com", Path: "/",
			Service: "payments", Port: "80", Problem: "NoEndpoints",
			Detail: "backend Service payments:80 has no ready endpoints (likely 502/503)",
		}},
		SecurityIssues: goldenSecurity(),
		NodeReserve: &nodereserve.Report{
			WarnCount: 2, EphemeralNone: 2, CPUNone: 2, EphemeralReporting: 2,
			Nodes: []nodereserve.NodeReservation{
				{Name: "worker-1", CPUReserved: "0", MemReserved: "0", EphemeralReserved: "0", Warning: true, NoEphemeral: true, NoCPU: true},
				{Name: "worker-2", CPUReserved: "0", MemReserved: "0", EphemeralReserved: "0", Warning: true, NoEphemeral: true, NoCPU: true},
			},
		},
		PVCReclaim: &pvcreclaim.Report{Count: 1, PVCs: []pvcreclaim.PVCReclaim{
			{Namespace: "shop", Name: "cache-data", PV: "pvc-abc123", StorageClass: "standard", Capacity: "128Mi"},
		}},
		KubeletHealth: &nodehealth.Report{Probed: 3, Unhealthy: []nodehealth.Issue{{Node: "worker-2", Detail: "[-]syncloop failed"}}},
	}
}

// goldenWorkloads covers every failure mode the report renders: CrashLoopBackOff,
// ImagePullBackOff, OOMKilled(+CrashLoop), Pending/Unschedulable, RestartLoop, and
// VolumeAttachError. Timestamps precede goldenNow so ages are fixed.
func goldenWorkloads() []inventory.Workload {
	r := "2025-12-25T00:00:00Z" // ~8d before goldenNow
	return []inventory.Workload{
		{Namespace: "shop", Name: "web", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Restarts: 8, LastRestart: r, Image: "busybox:1.36",
			Pods:     []inventory.PodRow{{Name: "web-5b8-2wplt", Phase: "Running", Ready: "0/1", Restarts: 8, LastRestart: r, Node: "worker-1", IP: "10.244.2.2", Age: "20d", Image: "busybox:1.36"}},
			Findings: []diagnose.Finding{{Pod: "shop/web", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting", Evidence: `container "web", restartCount=8`, Container: "web", LogExcerpt: "panic: runtime error: invalid memory address", LogCause: "application panic (code bug)"}}},
		{Namespace: "shop", Name: "api", Kind: "Deployment", Desired: 2, Ready: 0, Status: "Degraded",
			Image:    "nginx:9.9.9-nope",
			Pods:     []inventory.PodRow{{Name: "api-864-dxtdh", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-1", IP: "10.244.2.4", Age: "6d", Image: "nginx:9.9.9-nope"}},
			Findings: []diagnose.Finding{{Pod: "shop/api", Issue: "ImagePullBackOff", Reason: "Bad image reference or registry authentication", Evidence: `container "api": Back-off pulling image "nginx:9.9.9-nope": not found`}}},
		{Namespace: "shop", Name: "billing-worker", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Degraded",
			Restarts: 6, LastRestart: r, Image: "polinux/stress",
			Pods: []inventory.PodRow{{Name: "billing-7c7-vbgd7", Phase: "Running", Ready: "0/1", Restarts: 6, LastRestart: r, Node: "worker-2", IP: "10.244.1.2", Age: "18d", Image: "polinux/stress"}},
			Findings: []diagnose.Finding{
				{Pod: "shop/billing-worker", Issue: "CrashLoopBackOff", Reason: "Container repeatedly crashes after starting", Evidence: `container "worker", restartCount=6`},
				{Pod: "shop/billing-worker", Issue: "OOMKilled", Reason: "Container exceeded its memory limit and was killed", Evidence: `container "worker", exitCode=137`,
					Resources: &diagnose.ContainerResources{Container: "worker", CPURequest: "", CPULimit: "", MemRequest: "32Mi", MemLimit: "64Mi"}},
			}},
		{Namespace: "shop", Name: "report-cron", Kind: "Deployment", Desired: 1, Ready: 0, Status: "Pending",
			Image:    "busybox:1.36",
			Pods:     []inventory.PodRow{{Name: "report-cron-767-xghsp", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "", IP: "", Age: "20d", Image: "busybox:1.36"}},
			Findings: []diagnose.Finding{{Pod: "shop/report-cron", Issue: "Unschedulable", Reason: "No node can schedule this pod (resources, taints, or affinity)", Evidence: "0/4 nodes are available: 1 node(s) had untolerated taint, 3 Insufficient cpu"}}},
		{Namespace: "shop", Name: "cache", Kind: "Deployment", Desired: 1, Ready: 1, Status: "Running",
			Restarts: 5, LastRestart: r, Image: "redis:7-alpine",
			Pods:     []inventory.PodRow{{Name: "cache-6d9-abcde", Phase: "Running", Ready: "1/1", Restarts: 5, LastRestart: r, Node: "worker-3", IP: "10.244.3.7", Age: "12d", Image: "redis:7-alpine"}},
			Findings: []diagnose.Finding{{Pod: "shop/cache", Issue: "RestartLoop", Reason: "Container keeps exiting with a non-OOM error and restarting", Evidence: `container "cache", restartCount=5 (still flapping)`}}},
		{Namespace: "shop", Name: "data", Kind: "StatefulSet", Desired: 1, Ready: 0, Status: "Degraded",
			Image:    "postgres:16",
			Pods:     []inventory.PodRow{{Name: "data-0", Phase: "Pending", Ready: "0/1", Restarts: 0, Node: "worker-2", IP: "", Age: "9d", Image: "postgres:16"}},
			Findings: []diagnose.Finding{{Pod: "shop/data-0", Issue: "VolumeAttachError", Reason: "A ReadWriteOnce volume is still attached to another node (Multi-Attach)", Evidence: "Multi-Attach error for volume \"pvc-data-0\": volume is already used by pod(s) on node worker-1"}}},
	}
}

// goldenSecurity renders the full SECURITY section (non-verbose): baseline (Privileged,
// HostPath), an exposed Service, and enough restricted gaps across workloads for the
// "restricted (hardening gaps, near-universal)" aggregate.
func goldenSecurity() []secscan.Finding {
	restricted := func(ns, wl, container, check string) secscan.Finding {
		return secscan.Finding{Namespace: ns, Workload: wl, Kind: "Deployment", Container: container, Profile: "restricted", Check: check, Detail: check + " gap"}
	}
	fs := []secscan.Finding{
		{Namespace: "shop", Workload: "legacy-agent", Kind: "Deployment", Container: "agent", Profile: "baseline", Check: "Privileged", Detail: `container "agent" runs privileged (full host access)`},
		{Namespace: "shop", Workload: "legacy-agent", Kind: "Deployment", Profile: "baseline", Check: "HostPath", Detail: "mounts hostPath /var/run (writable host filesystem)"},
		{Namespace: "shop", Workload: "payments", Kind: "Service", Profile: "kubeagent", Check: "ExposedService", Detail: "type NodePort exposes port(s) 80 externally"},
	}
	for _, wl := range []string{"web", "api", "billing-worker", "cache", "data", "legacy-agent"} {
		fs = append(fs,
			restricted("shop", wl, wl, "RunAsRoot"),
			restricted("shop", wl, wl, "AllowPrivilegeEscalation"),
			restricted("shop", wl, wl, "CapabilitiesNotDropped"),
		)
	}
	return fs
}

func TestGoldenScanOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintInventory(goldenInput(goldenNow), "text", &buf); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("scan text output changed:\n%s\n\n"+
			"If this change is intended, run:\n"+
			"  go test ./internal/report -run TestGoldenScanOutput -update\n"+
			"then refresh docs/kubeagent-demo.gif (the update-demo-gif skill) and the "+
			"quickstart example output in website/docs/quickstart.md.",
			firstDiff(string(want), string(got)))
	}
}

// firstDiff returns the first differing line, for a readable failure message.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  want: %q\n  got:  %q", i+1, wl, gl)
		}
	}
	return "(files differ only in trailing content)"
}

// TestGoldenInputCoversAllSections guards against the fixture silently losing a section,
// which would leave the golden a partial snapshot.
func TestGoldenInputCoversAllSections(t *testing.T) {
	in := goldenInput(goldenNow)
	if in.Cluster.Verdict == "" || len(in.Result.Workloads) < 6 || in.Resources == nil ||
		in.Platform == nil || len(in.ServiceIssues) == 0 || len(in.CredentialWarnings) == 0 ||
		len(in.IngressIssues) == 0 || len(in.SecurityIssues) == 0 || in.NodeReserve == nil ||
		in.PVCReclaim == nil || in.KubeletHealth == nil {
		t.Fatal("goldenInput must populate every section so the golden stays comprehensive")
	}
	// Guard the *distinct* failure modes too, so a fixture regression can't drop one
	// (e.g. a second CrashLoop replacing VolumeAttachError) while still counting six.
	modes := map[string]bool{}
	for _, wl := range in.Result.Workloads {
		for _, f := range wl.Findings {
			modes[f.Issue] = true
		}
	}
	if len(modes) < 6 {
		t.Fatalf("goldenInput must exercise at least 6 distinct failure modes, got %d: %v", len(modes), modes)
	}
}
