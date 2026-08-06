package sandbox

import (
	"fmt"
	"math"
	"os"
	gopath "path"
	"slices"
	"strconv"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

// Hardening is the containment a provider applies to a session's sandbox when
// it creates one. The sandbox runs untrusted, model-directed commands, and the
// exec deadline cannot reclaim what it fails to kill — its kill is a process
// *group* kill, so a child that calls setsid outlives the deadline — which is
// why the cgroup limits here are the designed containment for an escaped
// process rather than a nicety (#65).
//
// The zero value applies nothing, so a provider behaves exactly as it did
// before this existed unless a caller asks for more. The platform's own
// defaults are resolved by HardeningFromEnv, which the executor and the BYOC
// worker call; they are deployment configuration, not a property of the type.
//
// Like Env and Networking, Hardening is bound when the sandbox is first
// created. Provision is idempotent and adopts a session's existing sandbox
// without re-applying a changed Hardening, so a caller that re-provisions a
// session must keep it stable.
//
// Backends apply what their runtime can express, and only that, and the gap
// runs both ways: PidsLimit is Docker-only, because the Kubernetes Pod API
// carries no per-pod pids limit (it is the kubelet's `podPidsLimit` node
// setting), while EphemeralStorageBytes is Kubernetes-only, because Docker's
// writable-layer quota is only as good as the daemon's storage driver. Each
// asymmetry is recorded in docs/DIVERGENCES.md rather than faked here, and the
// backend that cannot honour a configured value says so once rather than
// silently.
type Hardening struct {
	// PidsLimit caps the processes the sandbox may have alive at once. It is
	// the containment for a fork bomb and for the process pressure that would
	// stall the daemon probe the exec deadline uses to label an overrun. 0
	// leaves the runtime's default (unbounded).
	PidsLimit int64
	// CPUMillis caps CPU in thousandths of a core (2000 = two CPUs). A quota,
	// not a reservation: a backend that would otherwise turn a limit into a
	// scheduling reservation keeps the request small. 0 leaves it unbounded.
	CPUMillis int64
	// MemoryBytes caps the sandbox's memory. 0 leaves it unbounded — the
	// platform default, because an OOM kill in the middle of a task is a worse
	// failure than the throttling a CPU quota causes.
	MemoryBytes int64
	// EphemeralStorageBytes caps the sandbox's node-local disk — the container's
	// writable layer and every emptyDir the platform mounts over it. 0 leaves it
	// unbounded, the platform default.
	//
	// Kubernetes-only, the mirror image of PidsLimit: Docker's writable-layer
	// quota is only as good as the daemon's storage driver — some enforce it,
	// some refuse the option, and at least one accepts it and enforces nothing —
	// so that backend ignores this and says so once. The asymmetry is recorded
	// in docs/DIVERGENCES.md rather than faked.
	//
	// Its enforcement is unlike every other cap here, which is why it is opt-in
	// as MemoryBytes above is — a sharper version of that field's reason:
	// exceeding a memory limit kills the offending container, exceeding a CPU
	// limit throttles it, but exceeding this gets the whole pod **evicted** by
	// the kubelet. On this provider that surfaces mid-tool-call as a sandbox
	// that no longer exists. The limit makes the victim of node disk pressure
	// targeted and attributable instead of arbitrary; it does not make eviction
	// gentle.
	//
	// And it binds only where the kubelet can measure local ephemeral storage —
	// the node layouts Kubernetes supports for it. On any other layout the pod
	// takes the field and is never evicted for exceeding it, so this is a cap
	// whose effect is a property of the cluster's nodes as much as of the value.
	EphemeralStorageBytes int64
	// CapDrop names the Linux capabilities to drop, without the CAP_ prefix
	// ("NET_RAW"), or the single entry "ALL". Empty drops none. A gated
	// sandbox always drops NET_RAW/SETUID/SETGID on top of whatever this says:
	// those are what keep a tool from forging the gate's egress identity, so
	// they are not configurable away.
	CapDrop []string
	// ReadOnlyRootfs mounts the container's root filesystem read-only. The
	// provider mounts writable space over every path the platform itself writes
	// (WritablePaths) when it is set, so this is a provision-time choice rather
	// than a runtime-layer one — but it still needs an image that tolerates a
	// read-only root everywhere else. A session file resource created since #323
	// always resolves under the uploads root and so is covered; one stored before
	// it can still name a path outside the set and fail to materialize.
	ReadOnlyRootfs bool
	// RunAsUser overrides the image's default user with a numeric uid; nil
	// keeps the image's own USER. Numeric because that is what both backends
	// can express (a Kubernetes securityContext takes no user name).
	//
	// It does not make an image non-root-ready. ReadOnlyRootfs alongside it
	// makes the writable paths *exist* under a uid that could not create them
	// — which is what keeps the container's `mkdir -p <workdir>` entrypoint
	// alive — but only Kubernetes makes them *writable* by that uid (the
	// kubelet creates an emptyDir world-writable). On Docker a fresh anonymous
	// volume is root-owned 0755 unless the image ships the directory, so there
	// the image still decides. Shipping an image whose own USER is the uid
	// remains the reliable route; see docs/self-hosted-security.md §2.
	//
	// The one value both backends refuse is gaterun.DefaultGateUID for a gated
	// sandbox: a tool running as the gate's uid matches its owner-ACCEPT rule
	// and leaves the namespace unfiltered. Validate enforces it.
	RunAsUser *int64
}

// mandatoryGateCapDrop is what a gated sandbox drops whatever its Hardening
// says: without CAP_SETUID/CAP_SETGID a tool cannot become the gate's uid and
// match its owner-ACCEPT rule, and without CAP_NET_RAW it cannot AF_PACKET past
// the netfilter OUTPUT hook.
var mandatoryGateCapDrop = []string{"NET_RAW", "SETUID", "SETGID"}

// maxUID is the largest uid a sandbox may be given. uid_t is 32-bit and a
// value that does not fit truncates in the runtime's setuid — 2^32 becomes 0,
// which is root — so the cap sits inside a positive int32, matching
// gaterun's maxGateID. No real deployment needs an id above it.
const maxUID = 1<<31 - 1

// capDropAll is the runtime's wildcard, not a capability: both backends take it
// in a drop list to mean every capability, and it is the one entry the
// capability-name check has to let past.
const capDropAll = "ALL"

// EffectiveCapDrop is the capability set a backend actually applies: the
// configured drops, plus the gate's mandatory three when the sandbox is half of
// a gate pair. It is one function so the two backends cannot drift on which
// drops are negotiable. The result is deduplicated and ordered so a create
// payload is stable, "ALL" absorbs everything, and it is always a fresh slice —
// a backend must never be handed the caller's (or this package's default)
// backing array to store in a create payload.
func (h Hardening) EffectiveCapDrop(gated bool) []string {
	want := h.CapDrop
	if gated {
		want = append(append([]string{}, h.CapDrop...), mandatoryGateCapDrop...)
	}
	out := make([]string, 0, len(want))
	seen := map[string]bool{}
	for _, c := range want {
		if c == capDropAll {
			return []string{capDropAll}
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ShellStateRoot is where the persistent shell keeps every session's cwd/env
// state inside the sandbox (internal/sandbox/shell). It is named here because a
// read-only-rootfs sandbox has to mount writable space over it: without that,
// the first bash call fails to write its state and the tool faults rather than
// answering. It lives in this package rather than in shell because shell
// imports this one.
const ShellStateRoot = "/var/lib/map-shell"

// sessionResourceRoot is the mount point a session's file resources land under.
// The exact paths (`/mnt/session/uploads/…`) are resolved in internal/api, which
// this package must not import; the parent is what has to be writable. Since
// #323 every mount_path resolves under that root, so mounting the parent covers
// every resource created from then on, not just default-placed ones — a resource
// stored before #323 keeps its literal path and can still fall outside, which
// docs/self-hosted-security.md §4 states rather than guessing at here.
const sessionResourceRoot = "/mnt"

// WritablePaths are the mount points a read-only-rootfs sandbox still needs to
// write, in the order a backend should mount them. Both backends take the list
// from here so they cannot drift on it, and it is deduplicated: a deployment
// whose workdir is one of the fixed paths must not produce two mounts on the
// same target, which both runtimes reject. The workdir is cleaned first, so a
// trailing slash or a doubled separator is the same target here as it is to the
// kernel — otherwise `/tmp/` would slip past the dedupe and produce exactly the
// duplicate this exists to prevent.
func WritablePaths(workdir string) []string {
	if workdir == "" {
		workdir = DefaultWorkdir
	}
	out := make([]string, 0, 4)
	for _, p := range []string{gopath.Clean(workdir), "/tmp", ShellStateRoot, sessionResourceRoot} {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Validate rejects a Hardening that would quietly defeat something else the
// platform guarantees. Today that is one combination, and it fails closed at
// provision rather than at the first surprising egress: a **gated** sandbox may
// not run as the gate's own uid, because the gate's owner-match firewall ACCEPTs
// exactly that uid — every tool process would leave the namespace unfiltered,
// with allowed_hosts and vault substitution bypassed and nothing logged. The
// same hazard reached through the sandbox *image* is #196; this closes the half
// the platform itself now opens.
func (h Hardening) Validate(gated bool) error {
	if gated && h.RunAsUser != nil && *h.RunAsUser == gaterun.DefaultGateUID {
		return fmt.Errorf("sandbox: RunAsUser %d is the egress gate's own uid: "+
			"a sandbox running as it matches the gate's owner-match ACCEPT rule and would "+
			"bypass allowed_hosts entirely", *h.RunAsUser)
	}
	return nil
}

// The environment variables HardeningFromEnv reads. They are shared by the two
// binaries that provision sandboxes (cmd/executor, cmd/worker), so they carry
// the SANDBOX_ prefix the backend selection already uses rather than either
// binary's own.
const (
	envPidsLimit      = "SANDBOX_PIDS_LIMIT"
	envCPUMillis      = "SANDBOX_CPU_MILLIS"
	envMemoryBytes    = "SANDBOX_MEMORY_BYTES"
	envEphemeralBytes = "SANDBOX_EPHEMERAL_STORAGE_BYTES"
	envCapDrop        = "SANDBOX_CAP_DROP"
	envReadOnlyRootfs = "SANDBOX_READONLY_ROOTFS"
	envRunAsUser      = "SANDBOX_RUN_AS_USER"
)

// The platform's defaults, applied when the deployment sets nothing. They are
// chosen to be containment the first-class scenario — a general task agent on
// the default debian:stable-slim image, running as root — does not notice:
// 512 processes and two CPUs are generous, and the capability set is exactly
// the one a gated sandbox has always run under.
const (
	DefaultPidsLimit = 512
	DefaultCPUMillis = 2000
)

// DefaultCapDrop is the platform's default drop set: the three a gated sandbox
// already drops, extended to every sandbox. Nothing in the default image needs
// them — a tool that wants to change uid (apt's privilege drop, notably) warns
// and continues as root.
var DefaultCapDrop = []string{"NET_RAW", "SETUID", "SETGID"}

// HardeningFromEnv resolves the deployment's sandbox hardening. An unset or
// empty variable takes the default; an explicit 0 (or "none" for the capability
// set) turns that control off. A malformed value is an error rather than a
// silently dropped security control — a deployment that meant to cap the
// sandbox must not start believing it did.
func HardeningFromEnv() (Hardening, error) {
	var h Hardening
	var err error
	if h.PidsLimit, err = envCount(envPidsLimit, DefaultPidsLimit); err != nil {
		return Hardening{}, err
	}
	if h.CPUMillis, err = envCount(envCPUMillis, DefaultCPUMillis); err != nil {
		return Hardening{}, err
	}
	// A backend turns millicores into nanoCPUs by multiplying by a million, and
	// an int64 that wraps there lands on zero — which reads as "no limit", the
	// one answer a configured cap must never produce.
	if h.CPUMillis > math.MaxInt64/1_000_000 {
		return Hardening{}, fmt.Errorf("%s=%d is too large to express as a CPU limit", envCPUMillis, h.CPUMillis)
	}
	if h.MemoryBytes, err = envCount(envMemoryBytes, 0); err != nil {
		return Hardening{}, err
	}
	if h.EphemeralStorageBytes, err = envCount(envEphemeralBytes, 0); err != nil {
		return Hardening{}, err
	}
	if h.CapDrop, err = parseCapDrop(os.Getenv(envCapDrop)); err != nil {
		return Hardening{}, err
	}
	switch v := os.Getenv(envReadOnlyRootfs); v {
	case "", "false":
	case "true":
		h.ReadOnlyRootfs = true
	default:
		return Hardening{}, fmt.Errorf("%s=%q is not true or false", envReadOnlyRootfs, v)
	}
	if v := os.Getenv(envRunAsUser); v != "" {
		uid, perr := strconv.ParseInt(v, 10, 64)
		// Bounded, not merely non-negative: uid_t is 32-bit, so a larger value
		// truncates in the runtime's setuid and can land on 0 — an operator who
		// asked for an unprivileged sandbox would get a root one. The gate
		// refuses its own uid on the same grounds (gaterun.CheckGateID).
		if perr != nil || uid < 0 || uid > maxUID {
			return Hardening{}, fmt.Errorf("%s=%q is not a uid (0..%d)", envRunAsUser, v, maxUID)
		}
		h.RunAsUser = &uid
	}
	return h, nil
}

// envCount reads a non-negative count, falling back to def when unset.
func envCount(name string, def int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q is not a non-negative number", name, v)
	}
	return n, nil
}

// parseCapDrop reads the comma-separated capability list. "" takes the default
// set and the literal "none" drops nothing — the two must be distinguishable,
// since an operator who wants the runtime's full capability set back is asking
// for something quite different from one who set nothing. Entries are trimmed,
// upper-cased and stripped of a CAP_ prefix, because the two backends disagree
// about it: Docker accepts either spelling, a Kubernetes securityContext only
// the bare name.
func parseCapDrop(v string) ([]string, error) {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "":
		// A fresh slice: the default is exported, and handing its backing array
		// to a create payload would let one deployment's mutation reach another.
		return slices.Clone(DefaultCapDrop), nil
	case "NONE":
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		c := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(part)), "CAP_")
		if c != capDropAll && !isCapabilityName(c) {
			return nil, fmt.Errorf("%s=%q: %q is not a capability name", envCapDrop, v, part)
		}
		out = append(out, c)
	}
	return out, nil
}

// linuxCapabilities is every capability the kernel defines, bare (the CAP_
// prefix stripped), through CAP_CHECKPOINT_RESTORE — the last one added, in
// Linux 5.9. It is the set both runtimes accept, and the only way a typo can be
// caught where this configuration is read: "NET_RAWW" is shaped exactly like a
// capability name, so a character-class check passes it to the runtime, which
// rejects every container create instead of this deployment's startup. A kernel
// that grows a new capability needs a line here; refusing a name we cannot send
// is the safe direction, and the alternative is a deployment that starts
// cleanly and then cannot provision a single session.
var linuxCapabilities = map[string]bool{
	"AUDIT_CONTROL": true, "AUDIT_READ": true, "AUDIT_WRITE": true,
	"BLOCK_SUSPEND": true, "BPF": true, "CHECKPOINT_RESTORE": true,
	"CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true,
	"FOWNER": true, "FSETID": true, "IPC_LOCK": true, "IPC_OWNER": true,
	"KILL": true, "LEASE": true, "LINUX_IMMUTABLE": true,
	"MAC_ADMIN": true, "MAC_OVERRIDE": true, "MKNOD": true,
	"NET_ADMIN": true, "NET_BIND_SERVICE": true, "NET_BROADCAST": true,
	"NET_RAW": true, "PERFMON": true, "SETFCAP": true, "SETGID": true,
	"SETPCAP": true, "SETUID": true, "SYSLOG": true, "SYS_ADMIN": true,
	"SYS_BOOT": true, "SYS_CHROOT": true, "SYS_MODULE": true,
	"SYS_NICE": true, "SYS_PACCT": true, "SYS_PTRACE": true,
	"SYS_RAWIO": true, "SYS_RESOURCE": true, "SYS_TIME": true,
	"SYS_TTY_CONFIG": true, "WAKE_ALARM": true,
}

// isCapabilityName reports whether c is a Linux capability this platform can
// send to a runtime. c arrives upper-cased with any CAP_ prefix stripped, so a
// residual prefix ("CAP_CAP_NET_RAW") fails the lookup along with "0", "_" and
// every misspelling.
func isCapabilityName(c string) bool { return linuxCapabilities[c] }
