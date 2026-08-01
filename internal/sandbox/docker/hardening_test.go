package docker

import (
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

func hardenedSpec(h sandbox.Hardening) sandbox.Spec {
	return sandbox.Spec{
		SessionID:  domain.ID("sesn_hard"),
		Image:      "img",
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  h,
	}
}

// The zero Hardening must leave the create payload byte-for-byte what it was
// before the field existed. Every field here is `omitempty`, so this is the
// guard that an unconfigured deployment's containers are unchanged — the
// contract rows can only see a hardened sandbox.
func TestSandboxConfigUnhardenedIsUnchanged(t *testing.T) {
	cfg := sandboxConfig(hardenedSpec(sandbox.Hardening{}), "/workspace", "")
	if cfg.User != "" {
		t.Errorf("User = %q, want the image's own", cfg.User)
	}
	h := cfg.HostConfig
	if h.PidsLimit != 0 || h.NanoCpus != 0 || h.Memory != 0 {
		t.Errorf("cgroup limits = %d/%d/%d, want none", h.PidsLimit, h.NanoCpus, h.Memory)
	}
	if h.CapDrop != nil || h.SecurityOpt != nil {
		t.Errorf("CapDrop/SecurityOpt = %v/%v, want none", h.CapDrop, h.SecurityOpt)
	}
	if h.ReadonlyRootfs || h.Mounts != nil {
		t.Errorf("ReadonlyRootfs/Mounts = %v/%v, want none", h.ReadonlyRootfs, h.Mounts)
	}
	if !h.Init {
		t.Error("Init = false; the zero Hardening must not disturb what was already set")
	}
}

func TestSandboxConfigAppliesHardening(t *testing.T) {
	uid := int64(10001)
	cfg := sandboxConfig(hardenedSpec(sandbox.Hardening{
		PidsLimit: 512, CPUMillis: 2000, MemoryBytes: 1 << 30,
		CapDrop: []string{"CHOWN"}, ReadOnlyRootfs: true, RunAsUser: &uid,
	}), "/workspace", "")

	h := cfg.HostConfig
	if h.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", h.PidsLimit)
	}
	// The daemon's unit is nanoCPUs; a millicore is a million of them, so two
	// CPUs is 2e9. Getting this wrong by 1000x is the whole risk of the field.
	if h.NanoCpus != 2_000_000_000 {
		t.Errorf("NanoCpus = %d, want 2000000000 for 2000 millicores", h.NanoCpus)
	}
	if h.Memory != 1<<30 {
		t.Errorf("Memory = %d, want %d", h.Memory, 1<<30)
	}
	if !slices.Equal(h.CapDrop, []string{"CHOWN"}) {
		t.Errorf("CapDrop = %v, want [CHOWN]", h.CapDrop)
	}
	// A drop without no-new-privileges is a drop a setuid binary hands back.
	if !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}) {
		t.Errorf("SecurityOpt = %v, want [no-new-privileges]", h.SecurityOpt)
	}
	if cfg.User != "10001" {
		t.Errorf("User = %q, want 10001", cfg.User)
	}
	if !h.ReadonlyRootfs {
		t.Error("ReadonlyRootfs = false")
	}
	want := []mount{{Type: "volume", Target: "/workspace"}, {Type: "volume", Target: "/tmp"}}
	if !slices.Equal(h.Mounts, want) {
		t.Errorf("Mounts = %+v, want %+v — anonymous volumes, since the daemon "+
			"refuses an archive PUT into a tmpfs on a read-only-rootfs container", h.Mounts, want)
	}
}

// The gate's three drops are not configurable away: they are what keeps a tool
// from becoming the gate's uid and matching its owner-ACCEPT rule.
func TestSandboxConfigGatedKeepsItsMandatoryDrops(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured []string
		want       []string
	}{
		{"nothing configured", nil, []string{"NET_RAW", "SETUID", "SETGID"}},
		{"a different drop configured", []string{"CHOWN"},
			[]string{"CHOWN", "NET_RAW", "SETUID", "SETGID"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sandboxConfig(hardenedSpec(sandbox.Hardening{CapDrop: tc.configured}), "/workspace", "gate1")
			if !slices.Equal(cfg.HostConfig.CapDrop, tc.want) {
				t.Errorf("CapDrop = %v, want %v", cfg.HostConfig.CapDrop, tc.want)
			}
			if !slices.Equal(cfg.HostConfig.SecurityOpt, []string{"no-new-privileges"}) {
				t.Errorf("SecurityOpt = %v, want [no-new-privileges]", cfg.HostConfig.SecurityOpt)
			}
		})
	}
}
