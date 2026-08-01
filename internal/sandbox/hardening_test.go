package sandbox_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// hardeningEnv are every variable HardeningFromEnv reads. Each test clears them
// all before setting its own, so a variable leaking in from the developer's
// shell cannot quietly change what a row measures.
var hardeningEnv = []string{
	"SANDBOX_PIDS_LIMIT", "SANDBOX_CPU_MILLIS", "SANDBOX_MEMORY_BYTES",
	"SANDBOX_CAP_DROP", "SANDBOX_READONLY_ROOTFS", "SANDBOX_RUN_AS_USER",
}

func setHardeningEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, name := range hardeningEnv {
		t.Setenv(name, kv[name])
	}
}

// An unconfigured deployment still gets containment: that is the whole point of
// #65, so the defaults are asserted by value rather than "something non-zero".
func TestHardeningFromEnvDefaults(t *testing.T) {
	setHardeningEnv(t, nil)
	h, err := sandbox.HardeningFromEnv()
	if err != nil {
		t.Fatalf("HardeningFromEnv() = %v", err)
	}
	if h.PidsLimit != sandbox.DefaultPidsLimit || h.CPUMillis != sandbox.DefaultCPUMillis {
		t.Errorf("pids/cpu = %d/%d, want %d/%d",
			h.PidsLimit, h.CPUMillis, sandbox.DefaultPidsLimit, sandbox.DefaultCPUMillis)
	}
	if !slices.Equal(h.CapDrop, sandbox.DefaultCapDrop) {
		t.Errorf("CapDrop = %v, want %v", h.CapDrop, sandbox.DefaultCapDrop)
	}
	// Memory, read-only rootfs and the uid are opt-in: defaulting them on would
	// OOM-kill or break a task on the platform's own default image.
	if h.MemoryBytes != 0 || h.ReadOnlyRootfs || h.RunAsUser != nil {
		t.Errorf("opt-in knobs = %d/%v/%v, want off", h.MemoryBytes, h.ReadOnlyRootfs, h.RunAsUser)
	}
}

func TestHardeningFromEnvOverrides(t *testing.T) {
	setHardeningEnv(t, map[string]string{
		"SANDBOX_PIDS_LIMIT":      "64",
		"SANDBOX_CPU_MILLIS":      "500",
		"SANDBOX_MEMORY_BYTES":    "1073741824",
		"SANDBOX_CAP_DROP":        "ALL",
		"SANDBOX_READONLY_ROOTFS": "true",
		"SANDBOX_RUN_AS_USER":     "65534",
	})
	h, err := sandbox.HardeningFromEnv()
	if err != nil {
		t.Fatalf("HardeningFromEnv() = %v", err)
	}
	if h.PidsLimit != 64 || h.CPUMillis != 500 || h.MemoryBytes != 1<<30 {
		t.Errorf("limits = %d/%d/%d, want 64/500/%d", h.PidsLimit, h.CPUMillis, h.MemoryBytes, 1<<30)
	}
	if !slices.Equal(h.CapDrop, []string{"ALL"}) {
		t.Errorf("CapDrop = %v, want [ALL]", h.CapDrop)
	}
	if !h.ReadOnlyRootfs {
		t.Error("ReadOnlyRootfs = false, want true")
	}
	if h.RunAsUser == nil || *h.RunAsUser != 65534 {
		t.Errorf("RunAsUser = %v, want 65534", h.RunAsUser)
	}
}

// An explicit 0 is not "unset": an operator turning a control off must be able
// to, and must not silently get the default back.
func TestHardeningFromEnvZeroDisables(t *testing.T) {
	setHardeningEnv(t, map[string]string{
		"SANDBOX_PIDS_LIMIT": "0",
		"SANDBOX_CPU_MILLIS": "0",
		"SANDBOX_CAP_DROP":   "none",
	})
	h, err := sandbox.HardeningFromEnv()
	if err != nil {
		t.Fatalf("HardeningFromEnv() = %v", err)
	}
	if h.PidsLimit != 0 || h.CPUMillis != 0 || h.CapDrop != nil {
		t.Errorf("disabled hardening = %+v, want every control off", h)
	}
}

// Capability names are normalised because the backends disagree: Docker takes
// either spelling, a Kubernetes securityContext only the bare name.
func TestHardeningFromEnvNormalisesCapabilityNames(t *testing.T) {
	setHardeningEnv(t, map[string]string{"SANDBOX_CAP_DROP": " cap_net_raw , SYS_ADMIN "})
	h, err := sandbox.HardeningFromEnv()
	if err != nil {
		t.Fatalf("HardeningFromEnv() = %v", err)
	}
	if want := []string{"NET_RAW", "SYS_ADMIN"}; !slices.Equal(h.CapDrop, want) {
		t.Errorf("CapDrop = %v, want %v", h.CapDrop, want)
	}
}

// A malformed value fails startup. Falling back to the default would leave a
// deployment believing it had capped a sandbox it had not.
func TestHardeningFromEnvRejectsMalformedValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"SANDBOX_PIDS_LIMIT", "many"},
		{"SANDBOX_PIDS_LIMIT", "-1"},
		{"SANDBOX_CPU_MILLIS", "2.5"},
		{"SANDBOX_MEMORY_BYTES", "1GiB"},
		{"SANDBOX_CAP_DROP", "NET_RAW,"},
		{"SANDBOX_CAP_DROP", "NET RAW"},
		{"SANDBOX_READONLY_ROOTFS", "yes"},
		{"SANDBOX_RUN_AS_USER", "nobody"},
		{"SANDBOX_RUN_AS_USER", "-1"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			setHardeningEnv(t, map[string]string{tc.name: tc.value})
			h, err := sandbox.HardeningFromEnv()
			if err == nil {
				t.Fatalf("HardeningFromEnv() = %+v, nil; want an error", h)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Errorf("error %q does not name the variable %s", err, tc.name)
			}
		})
	}
}

func TestEffectiveCapDrop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured []string
		gated      bool
		want       []string
	}{
		{"ungated passes the configured set through", []string{"CHOWN"}, false, []string{"CHOWN"}},
		{"ungated drops nothing when nothing is configured", nil, false, nil},
		// The gate's three are what make its owner-match firewall hold, so they
		// are added even to a sandbox configured to drop nothing at all.
		{"gated adds the mandatory three", nil, true, []string{"NET_RAW", "SETUID", "SETGID"}},
		{"gated does not duplicate an overlapping entry", []string{"SETUID", "CHOWN"}, true,
			[]string{"SETUID", "CHOWN", "NET_RAW", "SETGID"}},
		{"ALL absorbs the mandatory three", []string{"ALL"}, true, []string{"ALL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.Hardening{CapDrop: tc.configured}.EffectiveCapDrop(tc.gated)
			if !slices.Equal(got, tc.want) {
				t.Errorf("EffectiveCapDrop(%v) = %v, want %v", tc.gated, got, tc.want)
			}
		})
	}
}
