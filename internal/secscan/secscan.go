// Package secscan flags high-signal, Pod Security Standards-aligned workload
// security-posture problems — privileged/over-privileged containers, insecure
// container defaults, and exposed Services. It is a curated subset of PSS
// (baseline + restricted), not a conformance implementation. Pure and
// read-only: the caller supplies the pods, services, and replicasets.
package secscan

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	profileBaseline   = "baseline"
	profileRestricted = "restricted"
	profileKubeagent  = "kubeagent"
)

// Finding is one security-posture problem attributed to a workload (or Service).
type Finding struct {
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Kind      string `json:"kind"`
	Container string `json:"container,omitempty"`
	Profile   string `json:"profile"`
	Check     string `json:"check"`
	Detail    string `json:"detail"`
}

// Assess flags PSS-aligned security-posture problems in the given pods and
// services. replicaSets is used only to fold a Deployment's pods up to the
// Deployment for display. Pure; the caller supplies already-namespace-filtered
// inputs.
func Assess(pods []corev1.Pod, services []corev1.Service, replicaSets []appsv1.ReplicaSet) []Finding {
	rsByKey := make(map[string]appsv1.ReplicaSet, len(replicaSets))
	for _, rs := range replicaSets {
		rsByKey[rs.Namespace+"/"+rs.Name] = rs
	}
	seen := make(map[string]bool)
	var out []Finding
	add := func(f Finding) {
		key := strings.Join([]string{f.Namespace, f.Kind, f.Workload, f.Container, f.Check, f.Detail}, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	}
	for _, pod := range pods {
		wl := resolveWorkload(pod, rsByKey)
		for _, f := range podFindings(pod, wl) {
			add(f)
		}
	}
	for _, svc := range services {
		if f, ok := exposedService(svc); ok {
			add(f)
		}
	}
	sortFindings(out)
	return out
}

// workloadRef is a pod's display owner.
type workloadRef struct{ Kind, Name string }

// resolveWorkload maps a pod to its top-level workload: its controlling owner,
// folded up one level when that owner is a ReplicaSet (→ its Deployment).
func resolveWorkload(pod corev1.Pod, rsByKey map[string]appsv1.ReplicaSet) workloadRef {
	owner := controllerOf(pod.OwnerReferences)
	if owner == nil {
		return workloadRef{Kind: "Pod", Name: pod.Name}
	}
	if owner.Kind == "ReplicaSet" {
		if rs, ok := rsByKey[pod.Namespace+"/"+owner.Name]; ok {
			if d := controllerOf(rs.OwnerReferences); d != nil && d.Kind == "Deployment" {
				return workloadRef{Kind: "Deployment", Name: d.Name}
			}
		}
		return workloadRef{Kind: "ReplicaSet", Name: owner.Name}
	}
	return workloadRef{Kind: owner.Kind, Name: owner.Name}
}

func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// podFindings returns every posture finding for one pod.
func podFindings(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	out = append(out, baselinePodChecks(pod, wl)...)
	out = append(out, containerChecks(pod, wl)...)
	out = append(out, restrictedChecks(pod, wl)...)
	return out
}

// baselinePodChecks covers pod-level baseline controls.
func baselinePodChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	if ns := hostNamespaces(pod); ns != "" {
		out = append(out, finding(pod, wl, profileBaseline, "HostNamespaces", "", "pod shares the host "+ns))
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			out = append(out, finding(pod, wl, profileBaseline, "HostPath", "",
				fmt.Sprintf("mounts hostPath %s (writable host filesystem)", v.HostPath.Path)))
		}
	}
	return out
}

// containerChecks covers per-container baseline controls.
func containerChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	for _, c := range allContainers(pod) {
		if isPrivileged(c) {
			out = append(out, finding(pod, wl, profileBaseline, "Privileged", c.Name,
				fmt.Sprintf("container %q runs privileged (full host access)", c.Name)))
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				out = append(out, finding(pod, wl, profileBaseline, "HostPort", c.Name,
					fmt.Sprintf("container %q binds host port %d", c.Name, p.HostPort)))
				break
			}
		}
		if caps := dangerousAddedCaps(c); len(caps) > 0 {
			out = append(out, finding(pod, wl, profileBaseline, "AddedCapability", c.Name,
				fmt.Sprintf("container %q adds capability %s", c.Name, strings.Join(caps, ", "))))
		}
	}
	return out
}

// restrictedChecks covers per-container restricted controls, evaluated on
// effective settings (a container's securityContext overrides the pod's).
func restrictedChecks(pod corev1.Pod, wl workloadRef) []Finding {
	var out []Finding
	for _, c := range allContainers(pod) {
		if !guaranteedNonRoot(pod, c) {
			out = append(out, finding(pod, wl, profileRestricted, "RunAsRoot", c.Name,
				fmt.Sprintf("container %q is not guaranteed to run as non-root", c.Name)))
		}
		if !escalationDisabled(c) {
			out = append(out, finding(pod, wl, profileRestricted, "AllowPrivilegeEscalation", c.Name,
				fmt.Sprintf("container %q allows privilege escalation (allowPrivilegeEscalation not false)", c.Name)))
		}
		if !dropsAll(c) {
			out = append(out, finding(pod, wl, profileRestricted, "CapabilitiesNotDropped", c.Name,
				fmt.Sprintf("container %q does not drop all capabilities", c.Name)))
		}
	}
	return out
}

func guaranteedNonRoot(pod corev1.Pod, c corev1.Container) bool {
	if nr := effectiveRunAsNonRoot(pod, c); nr != nil && *nr {
		return true
	}
	uid := effectiveRunAsUser(pod, c)
	return uid != nil && *uid > 0
}

func effectiveRunAsNonRoot(pod corev1.Pod, c corev1.Container) *bool {
	if c.SecurityContext != nil && c.SecurityContext.RunAsNonRoot != nil {
		return c.SecurityContext.RunAsNonRoot
	}
	if pod.Spec.SecurityContext != nil {
		return pod.Spec.SecurityContext.RunAsNonRoot
	}
	return nil
}

func effectiveRunAsUser(pod corev1.Pod, c corev1.Container) *int64 {
	if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
		return c.SecurityContext.RunAsUser
	}
	if pod.Spec.SecurityContext != nil {
		return pod.Spec.SecurityContext.RunAsUser
	}
	return nil
}

func escalationDisabled(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.AllowPrivilegeEscalation != nil && !*c.SecurityContext.AllowPrivilegeEscalation
}

func dropsAll(c corev1.Container) bool {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return false
	}
	for _, cap := range c.SecurityContext.Capabilities.Drop {
		if cap == "ALL" {
			return true
		}
	}
	return false
}

// exposedService flags Services reachable from outside the cluster.
func exposedService(svc corev1.Service) (Finding, bool) {
	var reason string
	switch {
	case svc.Spec.Type == corev1.ServiceTypeLoadBalancer:
		reason = "type LoadBalancer"
	case svc.Spec.Type == corev1.ServiceTypeNodePort:
		reason = "type NodePort"
	case len(svc.Spec.ExternalIPs) > 0:
		reason = "externalIPs set"
	default:
		return Finding{}, false
	}
	return Finding{
		Namespace: svc.Namespace, Workload: svc.Name, Kind: "Service",
		Profile: profileKubeagent, Check: "ExposedService",
		Detail: fmt.Sprintf("%s exposes %s externally", reason, servicePorts(svc)),
	}, true
}

func servicePorts(svc corev1.Service) string {
	var ps []string
	for _, p := range svc.Spec.Ports {
		ps = append(ps, strconv.Itoa(int(p.Port)))
	}
	if len(ps) == 0 {
		return "no ports"
	}
	return "port(s) " + strings.Join(ps, ",")
}

// baselineAllowedCap is the only capability the baseline profile permits adding.
const baselineAllowedCap = "NET_BIND_SERVICE"

func dangerousAddedCaps(c corev1.Container) []string {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return nil
	}
	var bad []string
	for _, cap := range c.SecurityContext.Capabilities.Add {
		if string(cap) != baselineAllowedCap {
			bad = append(bad, string(cap))
		}
	}
	return bad
}

func isPrivileged(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged
}

// finding builds a Finding attributed to the pod's workload.
func finding(pod corev1.Pod, wl workloadRef, profile, check, container, detail string) Finding {
	return Finding{
		Namespace: pod.Namespace, Workload: wl.Name, Kind: wl.Kind,
		Container: container, Profile: profile, Check: check, Detail: detail,
	}
}

// allContainers returns the pod's init + regular containers.
func allContainers(pod corev1.Pod) []corev1.Container {
	return append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
}

// hostNamespaces returns a human phrase for the shared host namespaces, or "".
func hostNamespaces(pod corev1.Pod) string {
	var s []string
	if pod.Spec.HostNetwork {
		s = append(s, "network")
	}
	if pod.Spec.HostPID {
		s = append(s, "PID")
	}
	if pod.Spec.HostIPC {
		s = append(s, "IPC")
	}
	if len(s) == 0 {
		return ""
	}
	return strings.Join(s, "/") + " namespace"
}

// sortFindings orders most-dangerous first, then namespace/workload/container/check.
func sortFindings(fs []Finding) {
	rank := map[string]int{profileBaseline: 0, profileRestricted: 1, profileKubeagent: 2}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if rank[a.Profile] != rank[b.Profile] {
			return rank[a.Profile] < rank[b.Profile]
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Container != b.Container {
			return a.Container < b.Container
		}
		return a.Check < b.Check
	})
}
