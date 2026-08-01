package docker

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
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
	cfg := sandboxConfig(hardenedSpec(sandbox.Hardening{}), "/workspace", "", 0)
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
	}), "/workspace", "", 2000)

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
	// Every path the platform itself writes, not just the workdir: the shell's
	// state root is what a bash call needs, and a session file resource lands
	// under the resource mount root.
	var want []mount
	for _, path := range sandbox.WritablePaths("/workspace") {
		want = append(want, mount{Type: "volume", Target: path})
	}
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
			cfg := sandboxConfig(hardenedSpec(sandbox.Hardening{CapDrop: tc.configured}), "/workspace", "gate1", 0)
			if !slices.Equal(cfg.HostConfig.CapDrop, tc.want) {
				t.Errorf("CapDrop = %v, want %v", cfg.HostConfig.CapDrop, tc.want)
			}
			if !slices.Equal(cfg.HostConfig.SecurityOpt, []string{"no-new-privileges"}) {
				t.Errorf("SecurityOpt = %v, want [no-new-privileges]", cfg.HostConfig.SecurityOpt)
			}
		})
	}
}

// The daemon refuses a create whose NanoCpus exceeds the host's CPUs outright,
// so the platform's on-by-default two-CPU cap would fail *every* provision on a
// smaller host — a per-session 400, not a startup error. clampCPU bounds the cap
// by what the daemon says it has; a daemon that will not answer leaves the
// configured value alone, so a failed probe can never widen a cap.
func TestClampCPUToTheHostsCPUs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hostCPUs   string
		configured int64
		want       int64
	}{
		{"a cap the host can express is untouched", `{"NCPU":8}`, 2000, 2000},
		{"a cap above the host is clamped to it", `{"NCPU":1}`, 2000, 1000},
		{"no cap configured stays no cap", `{"NCPU":1}`, 0, 0},
		{"an unreadable daemon leaves the cap alone", "", 2000, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/info" {
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
				}
				if tc.hostCPUs == "" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				io.WriteString(w, tc.hostCPUs)
			})
			if got := p.clampCPU(context.Background(), tc.configured); got != tc.want {
				t.Errorf("clampCPU(%d) = %d, want %d", tc.configured, got, tc.want)
			}
		})
	}
}

// The daemon is asked once, not once per session: every tool call re-provisions.
func TestClampCPUAsksTheDaemonOnce(t *testing.T) {
	calls := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		io.WriteString(w, `{"NCPU":4}`)
	})
	for range 3 {
		p.clampCPU(context.Background(), 2000)
	}
	if calls != 1 {
		t.Errorf("daemon /info calls = %d, want 1", calls)
	}
}

// A daemon that would not answer is asked again. Caching the failure instead
// would let one transient error pin a host smaller than the cap into a rejected
// create per session until the executor restarts — which is the failure clampCPU
// exists to prevent, reintroduced by the cache in front of it.
func TestClampCPURetriesAFailedProbe(t *testing.T) {
	calls := 0
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, `{"NCPU":1}`)
	})
	if got := p.clampCPU(context.Background(), 2000); got != 2000 {
		t.Fatalf("first clampCPU = %d, want the configured 2000 left alone", got)
	}
	if got := p.clampCPU(context.Background(), 2000); got != 1000 {
		t.Errorf("clampCPU after the daemon recovered = %d, want it clamped to 1000", got)
	}
	if calls != 2 {
		t.Errorf("daemon /info calls = %d, want 2", calls)
	}
}

// A gated sandbox running as the gate's own uid would match the gate's
// owner-match ACCEPT rule and leave the namespace unfiltered. Provision refuses
// before it touches the daemon — a fail-closed check, not a warning.
func TestProvisionRefusesTheGatesUIDWhenGated(t *testing.T) {
	uid := int64(gaterun.DefaultGateUID)
	p := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the daemon was contacted at all: %s %s", r.Method, r.URL.Path)
	})
	_, err := p.Provision(context.Background(), sandbox.Spec{
		SessionID: domain.NewID("sesn"), Image: "img",
		Hardening: sandbox.Hardening{RunAsUser: &uid},
		Gate:      &sandbox.GateSpec{Image: "gate:1", ControlplaneURL: "http://cp"},
	})
	if err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("Provision = %v, want a refusal naming the gate", err)
	}
}
