package nodereserve

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a fake node with the given capacity and allocatable cpu/mem.
func node(name, capCPU, capMem, allocCPU, allocMem string, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(capCPU),
				corev1.ResourceMemory: resource.MustParse(capMem),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(allocCPU),
				corev1.ResourceMemory: resource.MustParse(allocMem),
			},
		},
	}
}

func find(r Report, name string) NodeReservation {
	for _, n := range r.Nodes {
		if n.Name == name {
			return n
		}
	}
	return NodeReservation{}
}

func TestAssess_WarnsWhenMemoryReservedZero(t *testing.T) {
	// allocatable mem == capacity mem -> nothing reserved -> warn.
	r := Assess([]corev1.Node{node("w1", "4", "16Gi", "4", "16Gi", nil)})
	n := find(r, "w1")
	if !n.Warning {
		t.Fatalf("want Warning=true when mem reserved is 0, got %+v", n)
	}
	if n.MemReserved != "0" {
		t.Errorf("want MemReserved %q, got %q", "0", n.MemReserved)
	}
	if n.CPUReserved != "0" {
		t.Errorf("want CPUReserved %q, got %q", "0", n.CPUReserved)
	}
	if r.WarnCount != 1 {
		t.Errorf("want WarnCount 1, got %d", r.WarnCount)
	}
}

func TestAssess_OKWhenMemoryReserved(t *testing.T) {
	// 800Mi mem reserved, cpu unset (0) -> not warned.
	r := Assess([]corev1.Node{node("w2", "4", "16Gi", "4", "15584Mi", nil)})
	n := find(r, "w2")
	if n.Warning {
		t.Errorf("want Warning=false when mem is reserved, got %+v", n)
	}
	if n.MemReserved != "800Mi" {
		t.Errorf("want MemReserved %q, got %q", "800Mi", n.MemReserved)
	}
	if n.CPUReserved != "0" {
		t.Errorf("want CPUReserved %q (cpu unset), got %q", "0", n.CPUReserved)
	}
	if r.WarnCount != 0 {
		t.Errorf("want WarnCount 0, got %d", r.WarnCount)
	}
}

func TestAssess_FormatsCPUAndMemReserved(t *testing.T) {
	// 200m cpu, 1Gi mem reserved.
	r := Assess([]corev1.Node{node("w3", "4", "16Gi", "3800m", "15Gi", nil)})
	n := find(r, "w3")
	if n.CPUReserved != "200m" {
		t.Errorf("want CPUReserved %q, got %q", "200m", n.CPUReserved)
	}
	if n.MemReserved != "1Gi" {
		t.Errorf("want MemReserved %q, got %q", "1Gi", n.MemReserved)
	}
	if n.Warning {
		t.Errorf("want Warning=false, got true")
	}
}

func TestAssess_ClampsNegativeDeltaToZero(t *testing.T) {
	// allocatable > capacity (pathological) -> clamp to 0, warn on mem.
	r := Assess([]corev1.Node{node("w4", "4", "16Gi", "5", "17Gi", nil)})
	n := find(r, "w4")
	if n.CPUReserved != "0" || n.MemReserved != "0" {
		t.Errorf("want reserved clamped to 0/0, got cpu=%q mem=%q", n.CPUReserved, n.MemReserved)
	}
	if !n.Warning {
		t.Errorf("want Warning=true when mem reserved clamps to 0")
	}
}

func TestAssess_RoleFromLabels(t *testing.T) {
	cp := node("m1", "4", "16Gi", "4", "16Gi", map[string]string{"node-role.kubernetes.io/control-plane": ""})
	wk := node("w1", "4", "16Gi", "4", "15Gi", nil)
	r := Assess([]corev1.Node{cp, wk})
	if got := find(r, "m1").Role; got != "control-plane" {
		t.Errorf("want Role control-plane, got %q", got)
	}
	if got := find(r, "w1").Role; got != "worker" {
		t.Errorf("want Role worker, got %q", got)
	}
}

// nodeEph builds a fake node that also reports ephemeral-storage capacity/allocatable.
func nodeEph(name, capCPU, capMem, capEph, allocCPU, allocMem, allocEph string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(capCPU),
				corev1.ResourceMemory:           resource.MustParse(capMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(capEph),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(allocCPU),
				corev1.ResourceMemory:           resource.MustParse(allocMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(allocEph),
			},
		},
	}
}

func TestAssess_FlagsAllThreeWhenNoneReserved(t *testing.T) {
	// capacity == allocatable for cpu/mem/ephemeral -> nothing reserved anywhere.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "4", "16Gi", "100Gi")})
	n := find(r, "n")
	if !n.Warning || !n.NoCPU || !n.NoEphemeral {
		t.Fatalf("want all three no-reserve flags set, got %+v", n)
	}
	if r.WarnCount != 1 || r.CPUNone != 1 || r.EphemeralNone != 1 || r.EphemeralReporting != 1 {
		t.Errorf("want counts 1/1/1 reporting 1, got warn=%d cpu=%d eph=%d reporting=%d",
			r.WarnCount, r.CPUNone, r.EphemeralNone, r.EphemeralReporting)
	}
}

func TestAssess_EphemeralOnlyUnreserved(t *testing.T) {
	// cpu + mem reserved, ephemeral not reserved.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "3800m", "15Gi", "100Gi")})
	n := find(r, "n")
	if n.Warning || n.NoCPU {
		t.Errorf("want mem/cpu reserved (no flags), got %+v", n)
	}
	if !n.NoEphemeral {
		t.Errorf("want NoEphemeral=true, got %+v", n)
	}
	if r.EphemeralNone != 1 || r.WarnCount != 0 || r.CPUNone != 0 {
		t.Errorf("want eph-none 1, warn 0, cpu-none 0; got %d/%d/%d", r.EphemeralNone, r.WarnCount, r.CPUNone)
	}
	if n.EphemeralReserved != "0" {
		t.Errorf("want EphemeralReserved %q, got %q", "0", n.EphemeralReserved)
	}
}

func TestAssess_AllThreeReserved(t *testing.T) {
	// 200m cpu, 1Gi mem, 2Gi ephemeral reserved -> no flags.
	r := Assess([]corev1.Node{nodeEph("n", "4", "16Gi", "100Gi", "3800m", "15Gi", "98Gi")})
	n := find(r, "n")
	if n.Warning || n.NoCPU || n.NoEphemeral {
		t.Errorf("want no flags when all reserved, got %+v", n)
	}
	if n.EphemeralReserved != "2Gi" {
		t.Errorf("want EphemeralReserved %q, got %q", "2Gi", n.EphemeralReserved)
	}
	if r.EphemeralReporting != 1 || r.EphemeralNone != 0 {
		t.Errorf("want reporting 1, eph-none 0; got %d/%d", r.EphemeralReporting, r.EphemeralNone)
	}
}

func TestAssess_EphemeralNotReported(t *testing.T) {
	// node() reports only cpu/mem -> ephemeral is "not reported".
	r := Assess([]corev1.Node{node("n", "4", "16Gi", "3800m", "15Gi", nil)})
	n := find(r, "n")
	if n.EphemeralReserved != "—" {
		t.Errorf("want EphemeralReserved %q when not reported, got %q", "—", n.EphemeralReserved)
	}
	if n.NoEphemeral {
		t.Errorf("want NoEphemeral=false when not reported, got true")
	}
	if r.EphemeralReporting != 0 || r.EphemeralNone != 0 {
		t.Errorf("want reporting 0, eph-none 0; got %d/%d", r.EphemeralReporting, r.EphemeralNone)
	}
}
