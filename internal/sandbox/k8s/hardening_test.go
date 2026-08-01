package k8s

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

func hardenedPod(t *testing.T, p *Provider, h sandbox.Hardening) *corev1.Pod {
	t.Helper()
	return p.podSpec("map-sesn-hard", "/workspace", sandbox.Spec{
		SessionID:  domain.ID("sesn_hard"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  h,
	}, "")
}

func TestPodSpecAppliesHardening(t *testing.T) {
	uid := int64(10001)
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{
		CPUMillis: 2000, MemoryBytes: 1 << 30,
		CapDrop: []string{"CHOWN"}, ReadOnlyRootfs: true, RunAsUser: &uid,
	})
	c := pod.Spec.Containers[0]

	if got := c.Resources.Limits.Cpu().MilliValue(); got != 2000 {
		t.Errorf("cpu limit = %dm, want 2000m", got)
	}
	// The limit is containment; the request must not become a two-CPU
	// reservation the scheduler has to honour on every sandbox pod.
	if got := c.Resources.Requests.Cpu().MilliValue(); got != cpuRequestFloorMillis {
		t.Errorf("cpu request = %dm, want the %dm floor", got, cpuRequestFloorMillis)
	}
	// Memory cannot be throttled, so request = limit is the right posture.
	if got, want := c.Resources.Limits.Memory().Value(), int64(1<<30); got != want {
		t.Errorf("memory limit = %d, want %d", got, want)
	}
	if got, want := c.Resources.Requests.Memory().Value(), int64(1<<30); got != want {
		t.Errorf("memory request = %d, want %d", got, want)
	}

	sc := c.SecurityContext
	if sc == nil {
		t.Fatal("hardened sandbox has no securityContext")
	}
	if sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"CHOWN"}) {
		t.Errorf("capabilities = %+v, want CHOWN dropped", sc.Capabilities)
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("a drop set without allowPrivilegeEscalation: false hands the capability back")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem is not set")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 10001 {
		t.Errorf("RunAsUser = %v, want 10001", sc.RunAsUser)
	}

	// A read-only root still has to leave the workdir and /tmp writable — /tmp
	// is where this backend keeps each exec's state file.
	mounted := map[string]bool{}
	for _, m := range c.VolumeMounts {
		mounted[m.MountPath] = true
	}
	for _, path := range sandbox.WritablePaths("/workspace") {
		if !mounted[path] {
			t.Errorf("%s is not mounted writable: %+v", path, c.VolumeMounts)
		}
	}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir == nil {
			t.Errorf("volume %s is not an emptyDir: %+v", v.Name, v.VolumeSource)
		}
	}
	if len(pod.Spec.Volumes) != len(sandbox.WritablePaths("/workspace")) {
		t.Errorf("volumes = %+v, want exactly the writable mounts", pod.Spec.Volumes)
	}
}

// A limit below the request floor must not produce a request above its own
// limit, which the API server rejects outright.
func TestPodSpecCPURequestNeverExceedsTheLimit(t *testing.T) {
	pod := hardenedPod(t, fakeProvider(), sandbox.Hardening{CPUMillis: 50})
	c := pod.Spec.Containers[0]
	if got := c.Resources.Requests.Cpu().MilliValue(); got != 50 {
		t.Errorf("cpu request = %dm for a 50m limit, want 50m", got)
	}
}

// The Kubernetes Pod API carries no per-pod pids limit — it is the kubelet's
// `podPidsLimit` node setting — so this backend ignores PidsLimit rather than
// inventing an approximation. The divergence is deliberate and recorded in
// docs/DIVERGENCES.md; this row is what keeps it deliberate.
func TestPodSpecIgnoresPidsLimit(t *testing.T) {
	plain := hardenedPod(t, fakeProvider(), sandbox.Hardening{})
	pids := hardenedPod(t, fakeProvider(), sandbox.Hardening{PidsLimit: 512})
	if pids.Spec.Containers[0].SecurityContext != nil {
		t.Errorf("a pids limit grew a securityContext: %+v", pids.Spec.Containers[0].SecurityContext)
	}
	if len(pids.Spec.Containers[0].Resources.Limits) != len(plain.Spec.Containers[0].Resources.Limits) {
		t.Errorf("a pids limit changed the pod's resources: %+v", pids.Spec.Containers[0].Resources)
	}
}

// The gate's three drops are not configurable away, exactly as on the Docker
// backend — sandbox.Hardening.EffectiveCapDrop is the one place that decides.
func TestPodSpecGatedKeepsItsMandatoryDrops(t *testing.T) {
	p := fakeProvider()
	pod := p.podSpec("map-sesn-gated", "/workspace", sandbox.Spec{
		SessionID:  domain.ID("sesn_gated"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetLimited},
		Hardening:  sandbox.Hardening{CapDrop: []string{"CHOWN"}},
		Gate:       gateSpecFixture(&mintRecorder{}),
	}, "gtk_unit_test_token")
	sc := pod.Spec.Containers[0].SecurityContext
	want := []corev1.Capability{"CHOWN", "NET_RAW", "SETUID", "SETGID"}
	if sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, want) {
		t.Errorf("gated capabilities = %+v, want %v", sc, want)
	}
}

// The RuntimeClass is a cluster-level choice on the provider's Config, not a
// per-session one: it is what makes a hardened runtime (gVisor's runsc, Kata)
// selectable, and what the Helm chart's RuntimeClass option was waiting on.
func TestPodSpecRuntimeClass(t *testing.T) {
	if rc := hardenedPod(t, fakeProvider(), sandbox.Hardening{}).Spec.RuntimeClassName; rc != nil {
		t.Errorf("RuntimeClassName = %q with none configured, want nil (the cluster default)", *rc)
	}
	p := fakeProvider()
	p.runtimeClass = "gvisor"
	rc := hardenedPod(t, p, sandbox.Hardening{}).Spec.RuntimeClassName
	if rc == nil || *rc != "gvisor" {
		t.Errorf("RuntimeClassName = %v, want gvisor", rc)
	}
}

// The same fail-closed refusal the Docker backend makes: a gated sandbox may not
// run as the gate's own uid, or the gate's owner-match ACCEPT rule admits every
// tool process and allowed_hosts is void.
func TestProvisionRefusesTheGatesUIDWhenGated(t *testing.T) {
	uid := int64(gaterun.DefaultGateUID)
	p := fakeProvider()
	_, err := p.Provision(context.Background(), sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: "img",
		Hardening: sandbox.Hardening{RunAsUser: &uid},
		Gate:      gateSpecFixture(&mintRecorder{}),
	})
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("Provision = %v, want a refusal naming the gate", err)
	}
}

// A volume name is derived from the mount path, so it must be pod-unique and
// DNS-1123 legal whatever the configured workdir looks like — the API server
// rejects the whole pod otherwise.
func TestVolumeNameIsLegalAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, path := range sandbox.WritablePaths("/srv/Agent_Work/1") {
		name := volumeName(path)
		if seen[name] {
			t.Errorf("volume name %q is not unique across %v", name, sandbox.WritablePaths("/srv/Agent_Work/1"))
		}
		seen[name] = true
		if name == "" || len(name) > 63 {
			t.Errorf("volume name %q for %s is not a legal length", name, path)
		}
		for _, r := range name {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				t.Errorf("volume name %q for %s carries %q, which DNS-1123 forbids", name, path, r)
			}
		}
	}
}
