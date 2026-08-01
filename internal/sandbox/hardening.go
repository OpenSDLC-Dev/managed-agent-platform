package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
// Backends apply what their runtime can express, and only that: PidsLimit is
// Docker-only, because the Kubernetes Pod API carries no per-pod pids limit
// (it is the kubelet's `podPidsLimit` node setting). The asymmetry is recorded
// in docs/DIVERGENCES.md rather than faked here.
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
	// CapDrop names the Linux capabilities to drop, without the CAP_ prefix
	// ("NET_RAW"), or the single entry "ALL". Empty drops none. A gated
	// sandbox always drops NET_RAW/SETUID/SETGID on top of whatever this says:
	// those are what keep a tool from forging the gate's egress identity, so
	// they are not configurable away.
	CapDrop []string
	// ReadOnlyRootfs mounts the container's root filesystem read-only. The
	// provider mounts writable space over the workdir and /tmp when it is set
	// — the sandbox writes both — so this is a provision-time choice rather
	// than a runtime-layer one, but it still needs an image that tolerates a
	// read-only root elsewhere.
	ReadOnlyRootfs bool
	// RunAsUser overrides the image's default user with a numeric uid; nil
	// keeps the image's own USER. Numeric because that is what both backends
	// can express (a Kubernetes securityContext takes no user name). An image
	// whose workdir the uid cannot write needs ReadOnlyRootfs too, whose
	// writable mount is world-writable.
	RunAsUser *int64
}

// mandatoryGateCapDrop is what a gated sandbox drops whatever its Hardening
// says: without CAP_SETUID/CAP_SETGID a tool cannot become the gate's uid and
// match its owner-ACCEPT rule, and without CAP_NET_RAW it cannot AF_PACKET past
// the netfilter OUTPUT hook.
var mandatoryGateCapDrop = []string{"NET_RAW", "SETUID", "SETGID"}

// EffectiveCapDrop is the capability set a backend actually applies: the
// configured drops, plus the gate's mandatory three when the sandbox is half of
// a gate pair. It is one function so the two backends cannot drift on which
// drops are negotiable. The result is deduplicated and ordered so a create
// payload is stable; "ALL" absorbs everything.
func (h Hardening) EffectiveCapDrop(gated bool) []string {
	if !gated {
		return h.CapDrop
	}
	out := make([]string, 0, len(h.CapDrop)+len(mandatoryGateCapDrop))
	seen := map[string]bool{}
	for _, c := range append(append([]string{}, h.CapDrop...), mandatoryGateCapDrop...) {
		if c == "ALL" {
			return []string{"ALL"}
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// The environment variables HardeningFromEnv reads. They are shared by the two
// binaries that provision sandboxes (cmd/executor, cmd/worker), so they carry
// the SANDBOX_ prefix the backend selection already uses rather than either
// binary's own.
const (
	envPidsLimit      = "SANDBOX_PIDS_LIMIT"
	envCPUMillis      = "SANDBOX_CPU_MILLIS"
	envMemoryBytes    = "SANDBOX_MEMORY_BYTES"
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
	if h.MemoryBytes, err = envCount(envMemoryBytes, 0); err != nil {
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
		if perr != nil || uid < 0 {
			return Hardening{}, fmt.Errorf("%s=%q is not a uid", envRunAsUser, v)
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
	switch strings.TrimSpace(v) {
	case "":
		return DefaultCapDrop, nil
	case "none":
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		c := strings.ToUpper(strings.TrimSpace(part))
		c = strings.TrimPrefix(c, "CAP_")
		if c == "" || strings.ContainsFunc(c, func(r rune) bool {
			return !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_'
		}) {
			return nil, fmt.Errorf("%s=%q: %q is not a capability name", envCapDrop, v, part)
		}
		out = append(out, c)
	}
	return out, nil
}
