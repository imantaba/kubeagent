package fuzzgen

import (
	"bytes"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPodIsDeterministic(t *testing.T) {
	for _, in := range inputs {
		a, b := New(in).Pod(), New(in).Pod()
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("Pod() from the same %d bytes differed", len(in))
		}
	}
}

func TestPodNamesAreValid(t *testing.T) {
	for _, in := range inputs {
		c := New(in)
		for i := 0; i < 200; i++ {
			pod := c.Pod()
			for _, n := range []string{pod.Namespace, pod.Name} {
				if !dns1123.MatchString(n) {
					t.Fatalf("pod identity %q is not a DNS-1123 label", n)
				}
			}
			for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
				if !dns1123.MatchString(cs.Name) {
					t.Fatalf("container name %q is not a DNS-1123 label", cs.Name)
				}
			}
		}
	}
}

func TestPodSpecAndStatusAgreeOnContainerNames(t *testing.T) {
	// OOMKilledDetector looks a status container up in the spec to report its
	// limits. If the generator never made the two agree, that lookup would
	// always miss and the Resources path would never be fuzzed.
	c := New(bytes.Repeat([]byte("kubeagent"), 32))
	for i := 0; i < 200; i++ {
		pod := c.Pod()
		spec := map[string]bool{}
		for _, ctr := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
			spec[ctr.Name] = true
		}
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if !spec[cs.Name] {
				t.Fatalf("status container %q has no matching spec container", cs.Name)
			}
		}
	}
}

func TestPodReachesEveryDetectableState(t *testing.T) {
	// A generator that never emits CrashLoopBackOff would leave CrashLoopDetector
	// unfuzzed while the run still looked healthy. Assert the states the nine
	// detectors key on are all reachable.
	c := New(bytes.Repeat([]byte{0x00, 0x3f, 0x7f, 0xa5, 0xff, 0x11}, 64))
	seenWaiting := map[string]bool{}
	seenTerminated := map[string]bool{}
	var seenRunning, seenUnschedulable bool
	for i := 0; i < 20_000; i++ {
		pod := c.Pod()
		for _, cs := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
			if w := cs.State.Waiting; w != nil {
				seenWaiting[w.Reason] = true
			}
			if cs.State.Running != nil {
				seenRunning = true
			}
			for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
				if term != nil {
					seenTerminated[term.Reason] = true
				}
			}
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Reason == "Unschedulable" {
				seenUnschedulable = true
			}
		}
	}
	for _, r := range []string{"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "ContainerCreating"} {
		if !seenWaiting[r] {
			t.Errorf("no pod ever waited with reason %q", r)
		}
	}
	if !seenTerminated["OOMKilled"] {
		t.Error("no container was ever OOMKilled")
	}
	if !seenRunning {
		t.Error("no container was ever Running — RestartLoopDetector is unreachable")
	}
	if !seenUnschedulable {
		t.Error("no pod was ever Unschedulable — PendingDetector is unreachable")
	}
}

func TestEventsReachTheProbeAndAttachPaths(t *testing.T) {
	c := New(bytes.Repeat([]byte{0x00, 0x3f, 0x7f, 0xa5, 0xff, 0x11}, 64))
	seenReason := map[string]bool{}
	var seenRealFieldPath, seenProbeMessage bool
	for i := 0; i < 20_000; i++ {
		pod := c.Pod()
		for _, e := range c.Events(pod, 4) {
			seenReason[e.Reason] = true
			for _, ctr := range pod.Spec.Containers {
				if e.InvolvedObject.FieldPath == "spec.containers{"+ctr.Name+"}" {
					seenRealFieldPath = true
				}
			}
			if len(e.Message) > 8 && e.Message[:8] == "Readines" {
				seenProbeMessage = true
			}
		}
	}
	for _, r := range []string{"Unhealthy", "FailedAttachVolume"} {
		if !seenReason[r] {
			t.Errorf("no event ever had reason %q", r)
		}
	}
	if !seenRealFieldPath {
		t.Error("no event field path ever named a container the pod actually has")
	}
	if !seenProbeMessage {
		t.Error("no event ever carried a recognizable probe-failure message")
	}
}

func TestTLSSecretNeverCarriesAPrivateKey(t *testing.T) {
	// certhealth.Assess must never depend on tls.key, and this generator must
	// not tempt it to.
	c := New([]byte("seed"))
	for i := 0; i < 100; i++ {
		s := c.TLSSecret([]byte("not-a-cert"))
		if s.Type != corev1.SecretTypeTLS {
			t.Fatalf("secret type = %q, want %q", s.Type, corev1.SecretTypeTLS)
		}
		if _, ok := s.Data["tls.key"]; ok {
			t.Fatal("generated TLS secret carries a tls.key entry")
		}
		if got := string(s.Data["tls.crt"]); got != "not-a-cert" {
			t.Errorf("tls.crt = %q, want the crt passed in", got)
		}
	}
	if _, ok := c.TLSSecret(nil).Data["tls.crt"]; ok {
		t.Error("a nil crt should leave tls.crt absent, so the missing-tls.crt path is reachable")
	}
}
