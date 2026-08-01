package sandbox_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
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
	// A literal, not sandbox.DefaultCapDrop: comparing the result to the very
	// slice it was built from is an assertion that cannot fail.
	if want := []string{"NET_RAW", "SETUID", "SETGID"}; !slices.Equal(h.CapDrop, want) {
		t.Errorf("CapDrop = %v, want %v", h.CapDrop, want)
	}
	// And it must be a copy: DefaultCapDrop is exported, so handing its backing
	// array out would let one deployment's mutation reach every later one.
	h.CapDrop[0] = "MUTATED"
	if sandbox.DefaultCapDrop[0] != "NET_RAW" {
		t.Errorf("mutating the parsed set changed DefaultCapDrop to %v", sandbox.DefaultCapDrop)
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

// "none" turns the drops off, in whatever case the operator wrote it — upper-cased
// it would otherwise become a capability literally named NONE and fail every
// container create instead of doing what it says.
func TestHardeningFromEnvNoneIsCaseInsensitive(t *testing.T) {
	for _, spelling := range []string{"none", "NONE", "None"} {
		t.Run(spelling, func(t *testing.T) {
			setHardeningEnv(t, map[string]string{"SANDBOX_CAP_DROP": spelling})
			h, err := sandbox.HardeningFromEnv()
			if err != nil || h.CapDrop != nil {
				t.Errorf("HardeningFromEnv() = %v, %v; want no drops", h.CapDrop, err)
			}
		})
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
		// A character-class check alone admits these, and they reach the runtime
		// as capability names — failing every container create instead of this
		// deployment's startup.
		{"SANDBOX_CAP_DROP", "0"},
		{"SANDBOX_CAP_DROP", "_"},
		{"SANDBOX_CAP_DROP", "123"},
		{"SANDBOX_CAP_DROP", "CAP_CAP_NET_RAW"},
		// Shaped exactly like a capability name, so only the kernel's own set
		// can catch it here rather than at every container create.
		{"SANDBOX_CAP_DROP", "NET_RAWW"},
		{"SANDBOX_CAP_DROP", "NET_RAW,SYS_ADMINN"},
		// Large enough to wrap the millicores-to-nanoCPUs multiplication a
		// backend does, which lands on zero and reads as "no limit".
		{"SANDBOX_CPU_MILLIS", "9223372036854775"},
		{"SANDBOX_READONLY_ROOTFS", "yes"},
		{"SANDBOX_RUN_AS_USER", "nobody"},
		{"SANDBOX_RUN_AS_USER", "-1"},
		// uid_t is 32-bit: a larger value truncates in the runtime's setuid and
		// can land on 0, handing a root sandbox to an operator who asked for an
		// unprivileged one. 2^32 is that exact case.
		{"SANDBOX_RUN_AS_USER", "4294967296"},
		{"SANDBOX_RUN_AS_USER", "2147483648"},
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
		{"ungated normalises too", []string{"CHOWN", "CHOWN", "ALL"}, false, []string{"ALL"}},
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

// The ungated result must not alias the caller's slice either: a backend stores
// it in a create payload, and a caller that reused its own Hardening would then
// be editing a pending request.
func TestEffectiveCapDropDoesNotAliasTheCaller(t *testing.T) {
	configured := []string{"CHOWN"}
	got := sandbox.Hardening{CapDrop: configured}.EffectiveCapDrop(false)
	got[0] = "MUTATED"
	if configured[0] != "CHOWN" {
		t.Errorf("the caller's CapDrop became %v", configured)
	}
}

// Every path the platform writes inside a sandbox has to be in one list, or a
// read-only-rootfs deployment breaks somewhere nobody tested — the persistent
// shell's state root is the one that cost a review round.
func TestWritablePaths(t *testing.T) {
	paths := sandbox.WritablePaths("/workspace")
	for _, want := range []string{"/workspace", "/tmp", sandbox.ShellStateRoot, "/mnt"} {
		if !slices.Contains(paths, want) {
			t.Errorf("WritablePaths = %v, missing %s", paths, want)
		}
	}
	// A workdir that collides with a fixed path must not be mounted twice: both
	// runtimes reject duplicate mount targets, so the sandbox would not start.
	for _, workdir := range []string{"/tmp", sandbox.ShellStateRoot, "/mnt"} {
		got := sandbox.WritablePaths(workdir)
		seen := map[string]bool{}
		for _, p := range got {
			if seen[p] {
				t.Errorf("WritablePaths(%q) = %v, contains %s twice", workdir, got, p)
			}
			seen[p] = true
		}
	}
	// An empty workdir is the same default the providers resolve.
	if got := sandbox.WritablePaths(""); !slices.Contains(got, sandbox.DefaultWorkdir) {
		t.Errorf("WritablePaths(\"\") = %v, want the default workdir", got)
	}
}

// A gated sandbox running as the gate's own uid matches the owner-match ACCEPT
// rule, so every tool process leaves the namespace unfiltered with allowed_hosts
// bypassed and nothing logged. Provision must refuse rather than start it.
func TestValidateRefusesTheGatesOwnUID(t *testing.T) {
	gateUID, other := int64(gaterun.DefaultGateUID), int64(65534)
	for _, tc := range []struct {
		name    string
		uid     *int64
		gated   bool
		wantErr bool
	}{
		{"the gate's uid on a gated sandbox", &gateUID, true, true},
		{"the gate's uid without a gate is only a uid", &gateUID, false, false},
		{"another uid on a gated sandbox", &other, true, false},
		{"no uid at all", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sandbox.Hardening{RunAsUser: tc.uid}.Validate(tc.gated)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%v) = %v, wantErr %v", tc.gated, err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "gate") {
				t.Errorf("error %q does not say why the uid is refused", err)
			}
		})
	}
}

// Every capability the platform drops by default, and the wildcard, must
// survive the name check — a set that rejected its own defaults would fail
// every startup rather than only the typos it exists to catch.
func TestCapDropAcceptsTheDefaultsAndTheWildcard(t *testing.T) {
	for _, v := range append(append([]string{}, sandbox.DefaultCapDrop...), "ALL", "SYS_ADMIN", "CHOWN", "BPF", "CHECKPOINT_RESTORE") {
		setHardeningEnv(t, map[string]string{"SANDBOX_CAP_DROP": v})
		h, err := sandbox.HardeningFromEnv()
		if err != nil {
			t.Errorf("SANDBOX_CAP_DROP=%q: %v", v, err)
			continue
		}
		if len(h.CapDrop) != 1 || h.CapDrop[0] != v {
			t.Errorf("SANDBOX_CAP_DROP=%q parsed to %v", v, h.CapDrop)
		}
	}
}

// A workdir the operator wrote with a trailing slash is the same mount target
// as one without, and mounting both fails the create on either backend. Exact
// string dedupe cannot see that; cleaning the path first can.
func TestWritablePathsNormalizesBeforeDeduplicating(t *testing.T) {
	for _, workdir := range []string{"/tmp/", "/tmp", "//tmp", "/tmp/."} {
		got := sandbox.WritablePaths(workdir)
		if len(got) != 3 {
			t.Errorf("WritablePaths(%q) = %v, want the workdir folded into the fixed paths", workdir, got)
		}
		if !slices.Contains(got, "/tmp") {
			t.Errorf("WritablePaths(%q) = %v, want it to contain /tmp", workdir, got)
		}
	}
	if got := sandbox.WritablePaths("/srv/work/"); !slices.Contains(got, "/srv/work") {
		t.Errorf("WritablePaths(%q) = %v, want the cleaned workdir", "/srv/work/", got)
	}
}

// The largest uid that survives a 32-bit uid_t is still accepted: the bound
// exists to catch truncation, not to narrow what a deployment may choose.
func TestRunAsUserAcceptsTheLargestSafeUID(t *testing.T) {
	for _, v := range []string{"0", "1", "65534", "2147483647"} {
		setHardeningEnv(t, map[string]string{"SANDBOX_RUN_AS_USER": v})
		h, err := sandbox.HardeningFromEnv()
		if err != nil || h.RunAsUser == nil || strconv.FormatInt(*h.RunAsUser, 10) != v {
			t.Errorf("SANDBOX_RUN_AS_USER=%q parsed to %v, %v", v, h.RunAsUser, err)
		}
	}
}
