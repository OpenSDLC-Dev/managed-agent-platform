package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// seccompPod builds a pod spec for one networking shape, gated or not, with no
// hardening asked for — the configuration that proves the profile is applied
// unconditionally rather than as one more opt-in knob.
func seccompPod(t *testing.T, net domain.NetworkingType, gate *sandbox.GateSpec) *corev1.Pod {
	t.Helper()
	return fakeProvider().podSpec("map-sesn-seccomp", "/workspace", sandbox.Spec{
		SessionID:  domain.ID("sesn_seccomp"),
		Image:      "img",
		Networking: domain.Networking{Type: net},
		Gate:       gate,
	}, "")
}

// TestPodSpecAlwaysSetsSeccompRuntimeDefault: without this the sandbox runs
// under the runtime's *unconfined* default with every syscall the kernel
// offers, while an ordinary Docker container gets the runtime's curated filter
// — so setting it on Kubernetes closes a gap between the two backends rather
// than opening a compatibility question. Deliberately not configurable: a knob
// nobody turns on could not carry the shared-responsibility row this moves onto
// the platform.
func TestPodSpecAlwaysSetsSeccompRuntimeDefault(t *testing.T) {
	for name, tc := range map[string]struct {
		net  domain.NetworkingType
		gate *sandbox.GateSpec
	}{
		"Unrestricted":  {domain.NetUnrestricted, nil},
		"Gated":         {domain.NetLimited, gateSpecFixture(&mintRecorder{})},
		"LimitedNoGate": {domain.NetLimited, nil}, // the netsetup init-container path
	} {
		t.Run(name, func(t *testing.T) {
			pod := seccompPod(t, tc.net, tc.gate)
			sc := pod.Spec.SecurityContext
			if sc == nil || sc.SeccompProfile == nil {
				t.Fatalf("pod has no pod-level seccompProfile: %+v", sc)
			}
			if sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
				t.Errorf("seccompProfile type = %q, want %q",
					sc.SeccompProfile.Type, corev1.SeccompProfileTypeRuntimeDefault)
			}
			// Pod-level covers every container in the pod, so the sidecar that
			// runs iptables and the init container that flushes routes are
			// filtered too — neither has a profile of its own to forget.
			if n := len(pod.Spec.InitContainers); tc.net == domain.NetLimited && n != 1 {
				t.Errorf("expected one init container on a limited pod, got %d", n)
			}
		})
	}
}

// TestSeccompStaysOffTheContainerSecurityContext pins the placement, not just
// the value. Container-level would break two tests that assert an unhardened
// pod's container securityContext is exactly nil — assertions that exist so a
// knob nobody set cannot quietly grow one — and the tempting fix would be to
// weaken them. Pod-level leaves them true, and is the only placement that
// reaches the gate sidecar and the netsetup init container without editing
// either.
func TestSeccompStaysOffTheContainerSecurityContext(t *testing.T) {
	pod := seccompPod(t, domain.NetUnrestricted, nil)
	if got := pod.Spec.Containers[0].SecurityContext; got != nil {
		t.Errorf("the sandbox container grew a securityContext: %+v", got)
	}
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.SeccompProfile != nil {
			t.Errorf("container %q carries its own seccompProfile; it belongs on the pod", c.Name)
		}
	}
}
