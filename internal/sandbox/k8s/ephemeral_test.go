package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// TestPodSpecAppliesEphemeralStorage: the cap reaches the pod as a limit *and*
// an equal request. Equal deliberately, and for the opposite reason CPU keeps
// its request small — the kubelet decides evictions against the request, so a
// limit the scheduler never reserved is a cap whose enforcement lands on
// whichever pod happens to be over its own request. Memory already takes this
// shape for the same reason.
func TestPodSpecAppliesEphemeralStorage(t *testing.T) {
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{EphemeralStorageBytes: 2 << 30})
	r := pod.Spec.Containers[0].Resources
	limit, ok := r.Limits[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatalf("no ephemeral-storage limit on the pod: %+v", r.Limits)
	}
	if got := limit.Value(); got != 2<<30 {
		t.Errorf("ephemeral-storage limit = %d, want %d", got, 2<<30)
	}
	req, ok := r.Requests[corev1.ResourceEphemeralStorage]
	if !ok {
		t.Fatalf("no ephemeral-storage request on the pod: %+v", r.Requests)
	}
	if got := req.Value(); got != 2<<30 {
		t.Errorf("ephemeral-storage request = %d, want it equal to the limit (%d)", got, 2<<30)
	}
}

// TestPodSpecEphemeralStorageAloneAllocatesTheResourceLists is the trap this
// backend's resourceRequirements sets for a third caller: the maps are built
// inside the CPU branch, and the memory branch has to guard for nil before
// writing. A deployment that turns the CPU cap off (SANDBOX_CPU_MILLIS=0 is
// documented as the way to do it) and asks only for a disk cap would otherwise
// assign into a nil map and panic every provision.
func TestPodSpecEphemeralStorageAloneAllocatesTheResourceLists(t *testing.T) {
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{
		CPUMillis: 0, MemoryBytes: 0, EphemeralStorageBytes: 1 << 30,
	})
	r := pod.Spec.Containers[0].Resources
	if _, ok := r.Limits[corev1.ResourceEphemeralStorage]; !ok {
		t.Errorf("an ephemeral-storage-only hardening produced no limit: %+v", r.Limits)
	}
	if _, ok := r.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Errorf("an ephemeral-storage-only hardening produced no request: %+v", r.Requests)
	}
	if _, ok := r.Limits[corev1.ResourceCPU]; ok {
		t.Error("a disabled CPU cap still produced a CPU limit")
	}
}

// TestPodSpecWithoutEphemeralStorageAsksForNothing keeps the knob opt-in: the
// zero value must leave the pod exactly as it was, since the enforcement action
// here is evicting the pod rather than refusing a write.
func TestPodSpecWithoutEphemeralStorageAsksForNothing(t *testing.T) {
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{CPUMillis: 500})
	r := pod.Spec.Containers[0].Resources
	if _, ok := r.Limits[corev1.ResourceEphemeralStorage]; ok {
		t.Errorf("an unset disk cap still produced a limit: %+v", r.Limits)
	}
	if _, ok := r.Requests[corev1.ResourceEphemeralStorage]; ok {
		t.Errorf("an unset disk cap still produced a request: %+v", r.Requests)
	}
}
