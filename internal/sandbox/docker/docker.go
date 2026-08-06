// Package docker is the v1 sandbox backend: one disposable container per
// session, driven over the Docker Engine API. The image must carry /bin/bash
// at that exact path (the plan's image contract) and a POSIX userland. A `stat`
// accepting `-c` (GNU or BusyBox) is wanted rather than required: the write path
// reads the target's mode with it, and an image without one still writes — it
// lands the file 0644, as every write did before #204. The k8s backend asks for
// more, and asks harder (internal/sandbox/k8s/client.go).
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	gopath "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// sessionLabel tags every container we own, so an operator can find and reap
// sandboxes this platform created without guessing from names.
const sessionLabel = "dev.opensdlc.managed-agent-platform.session-id"

// execWrapper kills the command when its deadline passes. Docker has no API to
// kill a running exec, so this has to happen inside the container. $1 is the
// command, $2 the timeout in whole seconds ("0" = no limit).
//
// The command runs via `exec`, so it *becomes* this process — the pid Docker
// reports for the exec is the command itself, not a shell wrapping it. That is
// what lets Exec judge the deadline from outside by watching one pid: there is
// no separate wrapper pid for the command to kill in order to look finished
// while it keeps running (it cannot kill itself and continue). `set -m` makes
// the command a process-group leader, so the watchdog's `kill -9 -"$self"`
// takes its children with it — a process group, not a tree, so a child that
// calls setsid still escapes and outlives the deadline (Exec's own bound, and
// the container's eventual teardown, are what bound that).
//
// The watchdog is best effort by construction, and the sandbox never trusts it.
// It is a process inside the container, where the command can find and kill it:
// nothing in here is out of the agent's reach. What it buys is
// that an honest command's runaway loop stops burning the sandbox's CPU the
// moment its deadline passes, and that the sandbox learns the real exit code.
// Whether a call actually timed out is decided outside the container, by Exec.
//
// The watchdog polls `kill -0 "$self"` — the wrapper's own pid, captured before
// the exec, which the exec keeps — rather than sleeping the whole deadline: an
// honest command that finishes early takes its watchdog with it within one
// poll, so no stray `sleep` piles up across a session's thousands of quick
// commands. ($$ is captured into a variable because bash freezes both $$ and
// $PPID at the parent's values inside a subshell, and $PPID there is the
// wrapper's parent, not the wrapper.) The final kill re-checks `kill -0
// "$self"` first: the command may have exited during the last `sleep`, and a
// blind `kill -9 -"$self"` would then signal whatever group has since been
// assigned that pid. The watchdog's own output is discarded; the command's
// stderr is the exec's, untouched, so a SIGKILL leaves no shell "Killed" line
// in the tool result to begin with.
const execWrapper = `
set -m
self=$$
if [ "$2" != "0" ]; then
  (
    n=0
    while [ "$n" -lt "$2" ]; do
      kill -0 "$self" 2>/dev/null || exit 0
      sleep 1
      n=$((n + 1))
    done
    kill -0 "$self" 2>/dev/null && kill -9 -"$self" 2>/dev/null
  ) >/dev/null 2>&1 &
fi
exec /bin/bash -c "$1"
`

// sigkillExit is what bash reports for a job killed by SIGKILL (128 + 9).
const sigkillExit = 137

const (
	// defaultKillGrace is how long Exec waits past a command's deadline for the
	// in-container watchdog to finish the kill. Past that it stops waiting and
	// reports the timeout on its own authority, so a command that disabled the
	// watchdog buys itself this much wall clock and no more.
	defaultKillGrace = 2 * time.Second
	// defaultOverrunSlop is how much of the measured time Exec charges to itself
	// rather than to the command: the daemon round trips and the poll interval
	// below both blur the moment a command exited. Beyond that, a command that
	// outlived its deadline and still chose its own exit code can only have
	// disabled the watchdog, and the answer is a timeout whatever the command
	// says. It must stay under killGrace, or Exec stops waiting before it could
	// ever notice.
	defaultOverrunSlop = 500 * time.Millisecond
	// defaultExitBudget bounds the wait for the daemon to publish an exit code
	// once the exec's output has closed. It is normally instant; the budget is
	// there so a daemon that never stops calling the exec "running" fails
	// loudly instead of hanging.
	defaultExitBudget = 5 * time.Second
	// defaultProbeLead is how far before the deadline Exec asks whether the
	// command is still alive. It has to be before, not at, the deadline: the
	// watchdog fires at the deadline, and a command already killed by it looks
	// exactly like one that was never there. The cost is that a command which
	// SIGKILLs itself within this much of its deadline reads as a timeout — and,
	// when the probe goes unanswered altogether, one that SIGKILLs itself any time
	// before the deadline and leaves the stream open past it (see alive).
	defaultProbeLead = 50 * time.Millisecond
)

// Config configures the backend. Host is a Docker daemon address
// (unix:///... or tcp://host:port); empty falls back to DOCKER_HOST and then
// to the well-known socket.
type Config struct {
	Host string
	// GateNetwork is the Docker network a session's egress-gate container joins
	// — the deploy network that carries real egress and reaches the control
	// plane. The sandbox does not join it; it shares the gate's netns and reaches
	// the world only through the gate's proxy. Empty defaults to "bridge". Unused
	// for sessions with no Spec.Gate (unrestricted, no vault credentials).
	GateNetwork string
	// GateTokenRevoker, when non-nil, is called by Reap before it removes a
	// session's containers, so a gate token never outlives its gate (#197). It
	// lives on the provider — not on a Spec — because Reap has no Spec: the
	// reaper works from a session id alone. The executor supplies the same
	// pool-backed implementation it puts on every Spec; the BYOC worker, which
	// has no database, leaves it nil and Reap skips revocation.
	GateTokenRevoker sandbox.GateTokenRevoker
}

// Provider provisions per-session containers.
type Provider struct {
	api         *apiClient
	gateNetwork string
	revoker     sandbox.GateTokenRevoker
	// cpus caches the daemon's CPU count, read on the first provision that needs
	// it (New deliberately contacts no daemon) and 0 until then. See daemonCPUs.
	cpus atomic.Int64
	// diskWarnOnce keeps warnUnenforceableEphemeralStorage to one line per
	// provider rather than one per provisioned sandbox.
	diskWarnOnce sync.Once
}

// warnUnenforceableEphemeralStorage says once that a configured disk cap is
// being ignored. Docker's writable-layer quota is only as good as the daemon's
// storage driver, and the daemons disagree: some enforce it, classic overlay2
// without XFS pquota refuses the option outright, and Docker Desktop's
// overlayfs accepts it and enforces nothing (measured on 29.6.2 — the container
// still sees the whole host filesystem). Passing it through blindly would
// therefore fail provisioning on one daemon and report a cap that does not
// exist on another; honouring it properly means reading the daemon's storage
// driver and branching on it, which this cap's plan scopes to Kubernetes. So
// this backend ignores it — the mirror of Kubernetes ignoring PidsLimit, and
// warned about for the same reason: an operator who capped a sandbox and
// silently got nothing believes it is capped.
func (p *Provider) warnUnenforceableEphemeralStorage(ctx context.Context, h sandbox.Hardening) {
	if h.EphemeralStorageBytes <= 0 {
		return
	}
	p.diskWarnOnce.Do(func() {
		slog.WarnContext(ctx, "docker: sandbox ephemeral storage limit is ignored on this backend; "+
			"whether a writable-layer quota is enforced depends on the daemon's storage driver, "+
			"so the platform does not report a cap it cannot vouch for — bound the disk at the host",
			"configured", h.EphemeralStorageBytes)
	})
}

func New(cfg Config) (*Provider, error) {
	api, err := newAPIClient(cfg.Host)
	if err != nil {
		return nil, err
	}
	gateNetwork := cfg.GateNetwork
	if gateNetwork == "" {
		gateNetwork = "bridge"
	}
	return &Provider{api: api, gateNetwork: gateNetwork, revoker: cfg.GateTokenRevoker}, nil
}

// Owned lists the distinct session ids of every container — running or stopped,
// sandboxes and gates alike — carrying this daemon's ownership label.
func (p *Provider) Owned(ctx context.Context) ([]domain.ID, error) {
	list, err := p.api.listContainers(ctx, sessionLabel)
	if err != nil {
		return nil, err
	}
	seen := make(map[domain.ID]struct{}, len(list))
	var out []domain.ID
	for _, c := range list {
		id := domain.ID(c.Labels[sessionLabel])
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// Reap destroys everything this daemon owns for the session: the sandbox
// container, its gate when the session is gated, and their anonymous volumes
// (removeContainer passes v=1). The gate token is revoked first when the
// provider has a revoker (#197 — and revoke-before-teardown keeps a partial
// failure retryable: a re-run re-revokes a no-op and finishes the removals).
// The sandbox goes before the gate, as Destroy orders it — the sandbox lives in
// the gate's network namespace — and every removal is attempted even when an
// earlier one fails, so one stuck container never strands the rest. A session
// owning nothing is a no-op.
func (p *Provider) Reap(ctx context.Context, sessionID domain.ID) error {
	if p.revoker != nil {
		if err := p.revoker.Revoke(ctx, sessionID); err != nil {
			return fmt.Errorf("docker: revoke gate token for session %s: %w", sessionID, err)
		}
	}
	list, err := p.api.listContainers(ctx, sessionLabel+"="+string(sessionID))
	if err != nil {
		return err
	}
	gate := "/" + gateName(sessionID)
	var errs error
	for pass := 0; pass < 2; pass++ {
		for _, c := range list {
			if (pass == 1) != slices.Contains(c.Names, gate) {
				continue
			}
			errs = errors.Join(errs, removeWaitingGone(ctx, p.api, c.ID))
		}
	}
	return errs
}

// reapRemoveTimeout bounds how long Reap waits for a removal another actor
// already started to finish — the Docker analogue of the K8s backend's
// reclaimTimeout-bounded wait for a pod to be gone.
const reapRemoveTimeout = 15 * time.Second

// removeWaitingGone removes a container for Reap. A 404 is success, and so —
// after waiting it out — is the daemon's 409 "removal ... is already in
// progress": that in-progress window, not the 404 one, is where a racing
// reaper on another executor (or a concurrent provision unwinding the same
// pair) actually lands, and it lasts the whole force-removal where the
// list-to-delete gap is milliseconds. Waiting keeps Reap's promise that the
// container is gone when it returns, where blanket-tolerating the 409 would
// not; concurrent reapers converge instead of surfacing each other as errors.
// Any other failure is the caller's.
func removeWaitingGone(ctx context.Context, api *apiClient, id string) error {
	err := api.removeContainer(ctx, id)
	if err == nil || statusIs(err, 404) {
		return nil
	}
	if !statusIs(err, 409) || !strings.Contains(err.Error(), "already in progress") {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, reapRemoveTimeout)
	defer cancel()
	for {
		_, ierr := api.inspectContainer(ctx, id)
		if statusIs(ierr, 404) {
			return nil
		}
		if ierr != nil && ctx.Err() == nil {
			return ierr
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker: removal of %s still in progress: %w", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func containerName(sessionID domain.ID) string { return "map-" + string(sessionID) }

// Provision returns the session's container, creating and starting it only if
// none exists. Two executors racing on the same session converge: the loser of
// the create race adopts the winner's container.
//
// When spec.Gate is set the sandbox is one half of a pair: the gate container is
// ensured first (it owns the network namespace the sandbox joins and enforces
// its egress), and only once it is healthy is the sandbox created inside its
// netns. If a fresh gate was created here and the sandbox half then fails, the
// gate is torn down rather than leaked.
func (p *Provider) Provision(ctx context.Context, spec sandbox.Spec) (sb sandbox.Sandbox, err error) {
	if spec.SessionID.IsZero() {
		return nil, errors.New("docker: provision needs a session id")
	}
	if spec.Image == "" {
		return nil, errors.New("docker: provision needs an image")
	}
	if err := sandbox.ValidateEnv(spec.Env); err != nil {
		return nil, err
	}
	if err := spec.Hardening.Validate(spec.Gate != nil); err != nil {
		return nil, err
	}
	p.warnUnenforceableEphemeralStorage(ctx, spec.Hardening)
	workdir := spec.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	name := containerName(spec.SessionID)

	var gateID string
	if spec.Gate != nil {
		id, created, gerr := p.ensureGate(ctx, spec)
		if gerr != nil {
			return nil, gerr
		}
		gateID = id
		if created {
			// The gate is live but the sandbox half is not yet. Any error return
			// below (a lost adopt, a failed create/start) leaves this gate an
			// orphan no one adopts, so tear it down. This is scoped to a gate this
			// call created; a rare concurrent-adopt race (another executor adopts
			// this gate and pairs a sandbox to it while this call then errors) can
			// still break that sandbox's netns, but it is fail-closed and
			// self-healing — the broken sandbox's next tool call faults, reclaims,
			// and re-provisions, and the NetworkMode-aware adoption below rebuilds
			// it against a fresh gate. The standalone teardown reaper (a separate
			// issue) is the race-safe long-term owner of orphan cleanup.
			defer func() {
				if err != nil {
					p.removeDetached(ctx, gateID)
				}
			}()
		}
	}

	info, ierr := p.api.inspectContainer(ctx, name)
	switch {
	case ierr == nil:
		if oerr := ours(info, spec.SessionID); oerr != nil {
			return nil, oerr
		}
		if pairedWithGate(info, gateID) {
			// Adopt: ungated, or already paired with this session's current gate —
			// but only a container created from the spec this call is asking for,
			// checked before it is started or anything runs in it.
			if aerr := adoptable(info, spec, workdir, gateID); aerr != nil {
				return nil, aerr
			}
			if !info.State.Running {
				if serr := p.api.startContainer(ctx, info.ID); serr != nil {
					return nil, serr
				}
			}
			return p.attach(info.ID, workdir, gateID), nil
		}
		// The sandbox's egress path no longer matches the session's shape: gated
		// but attached to a different (or since-removed) gate — a stale pairing (a
		// recreated gate, or a pre-gate `bridge` sandbox from before this session
		// was gated) — or ungated but still routed through a gate (the session's
		// gate opt-in was removed). Remove it and recreate it on the current
		// egress path rather than adopt one with the wrong pairing.
		if gateID == "" {
			// Gated→ungated: no replacement gate will re-mint, so revoke the
			// session's persisted token before dismantling the pair — it must not
			// outlive its gate (#197). Revoke comes first because it is
			// idempotent: a teardown that fails below retries both on the next
			// provision, whereas revoking after a completed teardown would lose
			// the trigger (this mismatch observation) if the revoke itself failed.
			if spec.GateTokenRevoker != nil {
				if rerr := spec.GateTokenRevoker.Revoke(ctx, spec.SessionID); rerr != nil {
					return nil, fmt.Errorf("docker: revoke gate token for session %s: %w", spec.SessionID, rerr)
				}
			}
		}
		if rerr := removeIgnoring404(ctx, p.api, info.ID); rerr != nil {
			return nil, rerr
		}
		if gateID == "" {
			// The gate half of the dismantled pair, found by its deterministic
			// name; 404 means it is already gone. Inspect-then-remove, and only a
			// gate carrying this session's ownership label (the `ours` check every
			// adoption path makes): the name is a convention, and force-removing
			// whatever holds it could take out a container the platform does not
			// own. A gate orphaned the other way (its sandbox gone but the pair
			// never re-provisioned ungated) is the standalone teardown reaper's to
			// collect — same owner as the orphan-gate race noted above.
			gi, gerr := p.api.inspectContainer(ctx, gateName(spec.SessionID))
			switch {
			case gerr == nil:
				if oerr := ours(gi, spec.SessionID); oerr != nil {
					return nil, oerr
				}
				if rerr := removeIgnoring404(ctx, p.api, gi.ID); rerr != nil {
					return nil, rerr
				}
			case !statusIs(gerr, 404):
				return nil, gerr
			}
		}
	case !statusIs(ierr, 404):
		return nil, ierr
	}

	cfg := sandboxConfig(spec, workdir, gateID, p.clampCPU(ctx, spec.Hardening.CPUMillis))
	id, cerr := p.api.createContainer(ctx, name, cfg)
	if statusIs(cerr, 404) { // the image is not on this host yet
		if perr := p.api.pullImage(ctx, spec.Image); perr != nil {
			return nil, perr
		}
		id, cerr = p.api.createContainer(ctx, name, cfg)
	}
	createdSandbox := cerr == nil
	if statusIs(cerr, 409) { // another executor created it first
		winner, werr := p.api.inspectContainer(ctx, name)
		if werr != nil {
			return nil, werr
		}
		if oerr := ours(winner, spec.SessionID); oerr != nil {
			return nil, oerr
		}
		if !pairedWithGate(winner, gateID) {
			// The race winner is not paired with this session's gate. Fail closed
			// rather than adopt a sandbox with the wrong egress path; the retry's
			// adoption above removes and rebuilds the stale winner.
			return nil, fmt.Errorf("docker: raced sandbox %s for session %s is not paired with gate %s",
				winner.ID, spec.SessionID, gateID)
		}
		if aerr := adoptable(winner, spec, workdir, gateID); aerr != nil {
			return nil, aerr
		}
		id, cerr = winner.ID, nil
	}
	if cerr != nil {
		return nil, cerr
	}
	if serr := p.api.startContainer(ctx, id); serr != nil {
		if createdSandbox {
			// Remove the sandbox we just created. Its immutable network mode
			// references a gate this call is about to tear down (the deferred gate
			// cleanup), so a stopped leftover would poison every later retry —
			// adopting it and failing to start against the vanished gate forever.
			p.removeDetached(ctx, id)
		}
		return nil, serr
	}
	return p.attach(id, workdir, gateID), nil
}

// pairedWithGate reports whether an existing sandbox's egress path matches the
// session's current shape: a gated session's sandbox must be networked through
// its current gate (never a stale gate reference or a pre-gate `bridge`
// sandbox), and an ungated session's (gateID == "") must not be gate-networked
// at all — a still-gated pair is dismantled and its token revoked, not adopted
// (#197).
func pairedWithGate(info containerInfo, gateID string) bool {
	if gateID == "" {
		return !strings.HasPrefix(info.HostConfig.NetworkMode, "container:")
	}
	return info.HostConfig.NetworkMode == "container:"+gateID
}

// sandboxConfig is the session container's create config. It applies
// spec.Hardening — cgroup limits, capability drops, a uid, a read-only root with
// writable volumes over the paths the sandbox writes — and, when gateID is set, joins the
// gate's network namespace (container:<gateID>) and points HTTP(S)_PROXY at the
// gate's loopback proxy, where egress is filtered and vault credentials are
// substituted. With no gate it networks directly per its Networking
// (unrestricted, or none — fail-closed — for limited).
//
// A gated sandbox always drops NET_RAW (no raw or AF_PACKET socket that would
// bypass the OUTPUT hook) and SETUID/SETGID (so even a root process cannot
// setuid to the gate's uid and match the owner-ACCEPT rule) whatever the
// Hardening says — sandbox.Hardening.EffectiveCapDrop owns that union, so the
// two backends cannot drift on it. no-new-privileges travels with any drop set:
// dropping a capability while leaving setuid-binary escalation available would
// hand it straight back.
// clampCPU bounds a CPU cap by the daemon's own CPU count. The daemon rejects a
// create whose NanoCpus exceeds the host's CPUs outright, so the platform's
// default of two would fail every provision on a one-CPU host — a per-session
// 400 rather than a startup error, and on a backend whose whole point is being
// easy to run locally. Clamping *down* to the only value the host can express is
// not a silent policy change: the operator asked for at least this much
// containment and gets the most the machine allows. A daemon that will not
// answer leaves the configured value alone, so a probe failure can never widen
// the cap.
func (p *Provider) clampCPU(ctx context.Context, millis int64) int64 {
	if millis <= 0 {
		return millis
	}
	cpus := p.daemonCPUs(ctx)
	if cpus <= 0 || millis <= cpus*1000 {
		return millis
	}
	slog.WarnContext(ctx, "docker: sandbox CPU cap exceeds the host's CPUs; clamping",
		"configured_millis", millis, "host_cpus", cpus)
	return cpus * 1000
}

// daemonCPUs asks the daemon for its CPU count once and caches the answer, or
// returns 0 for a daemon that would not answer. Only a *success* is cached: a
// failed probe is retried on the next provision, since caching it would let one
// transient error pin a host smaller than the cap into a rejected create per
// session until the executor restarts — the very failure clampCPU exists to
// prevent. Racing provisions may each probe once before the first answer lands,
// which costs a redundant /info and nothing else.
func (p *Provider) daemonCPUs(ctx context.Context) int64 {
	if n := p.cpus.Load(); n > 0 {
		return n
	}
	n, err := p.api.hostCPUs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "docker: could not read the daemon's CPU count; leaving the sandbox CPU cap unclamped", "err", err)
		return 0
	}
	p.cpus.Store(int64(n))
	return int64(n)
}

func sandboxConfig(spec sandbox.Spec, workdir, gateID string, cpuMillis int64) containerConfig {
	// Tools background processes and orphan them; an init process reaps them
	// instead of letting them pile up as zombies for the session's whole life.
	host := hostConfig{Init: true}
	env := spec.Env
	if gateID != "" {
		host.NetworkMode = "container:" + gateID
		env = withProxyEnv(env)
	} else {
		host.NetworkMode = networkMode(spec.Networking)
	}
	h := spec.Hardening
	if host.CapDrop = h.EffectiveCapDrop(gateID != ""); len(host.CapDrop) > 0 {
		host.SecurityOpt = []string{"no-new-privileges"}
	}
	host.PidsLimit = h.PidsLimit
	// The daemon takes CPU as nanoCPUs (its `--cpus`) and turns it into the
	// cgroup's cpu.max quota; a millicore is a million of them. cpuMillis is the
	// host-clamped value, and HardeningFromEnv has already refused one large
	// enough to wrap this multiplication.
	host.NanoCpus = cpuMillis * 1_000_000
	host.Memory = h.MemoryBytes
	if h.ReadOnlyRootfs {
		// Everything the platform itself writes inside the sandbox — see
		// sandbox.WritablePaths, which both backends share so neither can forget
		// one. The `mount` type says why these are volumes and not tmpfs.
		host.ReadonlyRootfs = true
		for _, path := range sandbox.WritablePaths(workdir) {
			host.Mounts = append(host.Mounts, mount{Type: "volume", Target: path})
		}
	}
	cfg := containerConfig{
		Image: spec.Image,
		Env:   envSlice(env),
		// Hold the container open and guarantee the workdir exists. Nothing
		// else runs here: every tool call is its own exec.
		Entrypoint: []string{"/bin/bash", "-c",
			"mkdir -p " + shellQuote(workdir) + " && while :; do sleep 3600; done"},
		Cmd:        []string{},
		WorkingDir: workdir,
		Labels:     map[string]string{sessionLabel: string(spec.SessionID)},
		HostConfig: host,
	}
	if h.RunAsUser != nil {
		cfg.User = strconv.FormatInt(*h.RunAsUser, 10)
	}
	return cfg
}

// withProxyEnv returns a copy of env with the sandbox's egress-proxy variables
// set to the gate's loopback proxy — upper and lower case, for the Go net/http
// and the curl conventions both. The platform's proxy vars win over any
// collision, but there is none to win: the executor has already dropped every
// vault credential whose secret_name is a reserved name (sandbox.ReservedEnvName,
// which includes these). The address is the gate's, so it lives here in the
// provider that runs the gate rather than leaking into the backend-agnostic
// executor.
func withProxyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+6)
	for k, v := range env {
		out[k] = v
	}
	proxyURL := "http://" + gaterun.DefaultProxyAddr
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		out[k] = proxyURL
	}
	// Force NO_PROXY empty, overriding anything the base image baked in (a
	// `NO_PROXY=*` or an internal-domain exclusion): a proxy-aware client that
	// honored it would bypass the loopback gate and hit the owner-match firewall's
	// DROP, failing even a policy-allowed request. Everything goes through the gate.
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		out[k] = ""
	}
	return out
}

// ours refuses a container that merely wears this session's name. The name is
// derived from the session id, so anything else on the daemon can hold it: a
// container left by an earlier deployment, or one that happens to collide.
// Adopting it would hand the agent's commands to a filesystem, an image, and —
// because a container's network mode is fixed when it is created — an egress
// policy that are not the ones this session asked for; a `limited` session must
// never inherit a `bridge` container's route out. The label answers only who
// created the container; whether its fixed-at-create config matches this call's
// spec is `adoptable`'s check (#29). This is not a trust boundary
// against a hostile daemon co-tenant: the label is world-readable and
// world-writable, and anyone with access to the daemon can forge it — but that
// actor already controls every sandbox on the host. It defends against the
// accidents, which are the realistic failure on a single-tenant daemon.
func ours(info containerInfo, sessionID domain.ID) error {
	if info.Config.Labels[sessionLabel] != string(sessionID) {
		return fmt.Errorf("docker: container %s is not this platform's sandbox for session %s",
			info.ID, sessionID)
	}
	return nil
}

// adoptable refuses an owned container whose fixed-at-create configuration is
// not what this call's spec asks for. `ours` answers "was this container
// created for this session?"; this answers "was it created from the spec being
// requested?" — a limited request must never ride a `bridge` container's route
// out, and a different image or workdir is a different sandbox, not this one
// found again. Both adoption paths (the existing container and the lost create
// race) make the same check, before the container is started or anything runs
// in it. A mismatch fails closed with sandbox.ErrSpecMismatch and removes
// nothing: the platform has no replacement lifecycle for a live session's
// sandbox, and deleting on a caller-supplied spec would hand a caller's bug the
// container's filesystem (#29). A gated container's network mode is not
// re-checked here — pairedWithGate has already pinned it to container:<gateID> —
// and Spec.Env and Spec.Hardening, equally fixed at create, are deliberately
// not compared: a changed Env silently keeps the created value by contract
// (sandboxtest's SpecEnvBoundAtProvision — the gate's placeholder stability
// rides on it), and hardening follows the same adopt-as-created rule.
func adoptable(info containerInfo, spec sandbox.Spec, workdir, gateID string) error {
	if gateID == "" {
		if want := networkMode(spec.Networking); info.HostConfig.NetworkMode != want {
			return fmt.Errorf("docker: container %s has network mode %q where session %s asked for %q: %w",
				info.ID, info.HostConfig.NetworkMode, spec.SessionID, want, sandbox.ErrSpecMismatch)
		}
	}
	if info.Config.Image != spec.Image {
		return fmt.Errorf("docker: container %s was created from image %q where session %s asked for %q: %w",
			info.ID, info.Config.Image, spec.SessionID, spec.Image, sandbox.ErrSpecMismatch)
	}
	if info.Config.WorkingDir != workdir {
		return fmt.Errorf("docker: container %s was created with workdir %q where session %s asked for %q: %w",
			info.ID, info.Config.WorkingDir, spec.SessionID, workdir, sandbox.ErrSpecMismatch)
	}
	return nil
}

// attach builds the sandbox handle. gateID is the session's egress-gate
// container when the sandbox is half of a pair, "" otherwise; Destroy uses it to
// tear the gate down alongside the sandbox.
func (p *Provider) attach(id, workdir, gateID string) *container {
	return &container{
		api: p.api, id: id, workdir: workdir, gateID: gateID,
		killGrace: defaultKillGrace, overrunSlop: defaultOverrunSlop,
		exitBudget: defaultExitBudget, probeLead: defaultProbeLead,
	}
}

// networkMode fails closed on the ungated path (no Spec.Gate — a deployment
// not opted into the egress gate): `limited` means "only AllowedHosts", which
// needs the gate to enforce, so without one a limited sandbox gets no network
// at all rather than silently unrestricted egress.
func networkMode(net domain.Networking) string {
	if net.Type == domain.NetLimited {
		return "none"
	}
	return "bridge"
}

// envSlice renders Spec.Env as the Docker API's KEY=value list, key-sorted so a
// given spec always produces the same container config. Returns nil for an empty
// map so the config omits Env entirely and the image's own environment stands.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

type container struct {
	api         *apiClient
	id          string
	workdir     string
	gateID      string // the session's egress-gate container, or "" when ungated
	killGrace   time.Duration
	overrunSlop time.Duration
	exitBudget  time.Duration
	probeLead   time.Duration
}

func (c *container) ID() string { return c.id }

// verdict is what the sandbox saw of a command's life from outside the
// container, at the only two instants that decide a timeout.
type verdict struct {
	// aliveAtDeadline: still running as the deadline arrived, so a SIGKILL that
	// follows is the watchdog's and not the command's own.
	aliveAtDeadline bool
	// overran: still running once the deadline and the sandbox's measurement
	// slop had both passed, so no exit code it later reports can be believed.
	overran bool
}

// probeDeadline answers those two questions and nothing else.
//
// It cannot use the exec's output stream, whose close a backgrounded straggler
// defers long past the command's death, nor the daemon's `Running` flag, which
// tracks that same stream. It asks whether the exec's process is in the
// container's process list — `ps` on the daemon's host, which the sandboxed
// command can neither reach nor forge.
//
// The two instants are watched independently, each on its own timer, because
// the overrun check is the guarantee and nothing may delay it: run in sequence,
// a first `top` that stalls on a slow daemon would still be waiting when the
// command overran and exited, and the stream's close would then cancel the wait
// before it ever reached the overrun instant — the overrun unmeasured, read as
// a clean finish.
//
// They run on two contexts. sleepCtx times the waits and is cancelled the moment
// the output stream closes: a probe still sleeping then never mattered, and the
// close is what unblocks it. confirmCtx times only the overrun `top` — a probe
// that has already reached the overrun instant and is mid-request. That request
// must not be cancelled by the stream closing: a command that overran and then
// exited during its own confirming probe would otherwise have the cancellation
// read as "process gone" and its overrun erased. confirmCtx outlives the stream
// close and dies only on Exec's own bound or the caller giving up.
//
// A probe whose *wait* is cut short answers false, and correctly: the stream
// cannot close while the process that owns it is alive, so a close before an
// instant is a command that had already finished by it. A probe cut short
// mid-request is a different question, and alive reads it against the deadline.
func (c *container) probeDeadline(sleepCtx, confirmCtx context.Context, pid int, deadline time.Duration, start time.Time) <-chan verdict {
	answer := make(chan verdict, 1)
	var atDeadline, overran bool
	var wg sync.WaitGroup
	wg.Add(2)
	// Alive as the deadline arrived, so a SIGKILL that follows is the watchdog's?
	go func() {
		defer wg.Done()
		if sleepUntil(sleepCtx, start.Add(deadline-c.probeLead)) {
			atDeadline = c.alive(sleepCtx, pid, start.Add(deadline))
		}
	}()
	// Still alive once the deadline and the slop had both passed? The guarantee,
	// so it keeps its own clock and never waits on the probe above.
	go func() {
		defer wg.Done()
		if sleepUntil(sleepCtx, start.Add(deadline+c.overrunSlop)) {
			overran = c.aliveOrTimedOut(confirmCtx, pid)
		}
	}()
	go func() {
		wg.Wait()
		answer <- verdict{aliveAtDeadline: atDeadline, overran: overran}
	}()
	return answer
}

// sleepUntil reports whether it got there.
func sleepUntil(ctx context.Context, t time.Time) bool {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// alive answers the pre-deadline probe. Its context is the one the stream close
// cancels, and what that cancellation is worth depends on when Exec sees it —
// its own host-side reading, needing nothing from inside the container. Seen
// before the deadline it settles the question: nothing that held the stream is
// left, so the command was gone by the deadline. Seen at or after it the close
// says nothing about the deadline, because the watchdog's punctual kill is itself
// what closes the stream, and so is a straggler outliving a command that finished
// early — an unanswered probe, which is the same thing as a daemon that would not
// answer, and takes the same fail-open answer below.
//
// Seen, not recorded: the cancellation is noticed a scheduling hop after the
// close, so this reads the later case for a close within a hop of the deadline —
// like the probe lead itself, a cost paid in the direction of the label rather
// than away from it. What it costs in full is a command that SIGKILLs itself
// before the deadline, leaves something backgrounded holding its stream past it,
// and gets no answer out of the daemon: that reads as a timeout. The overrun
// probe's own fail-open rule already did this to the same command a slop later;
// this brings it forward to the deadline, and only ever adds a timeout.
func (c *container) alive(ctx context.Context, pid int, deadlineAt time.Time) bool {
	for range 2 {
		alive, err := c.api.processAlive(ctx, c.id, pid)
		if err == nil {
			return alive
		}
		if ctx.Err() != nil {
			if time.Now().Before(deadlineAt) {
				// The stream closed before the deadline, so the process it was
				// holding is gone, and nobody is waiting on this answer.
				return false
			}
			break
		}
	}
	// The daemon would not say. Assume the command is still running: hiding an
	// overrun breaks the deadline's guarantee, while mislabelling one costs a
	// tool call.
	return true
}

// aliveOrTimedOut answers the overrun probe, which has already reached the
// overrun instant and only needs the daemon to confirm what it saw. Its context
// is not the one the stream close cancels, so — unlike alive — a cancellation
// here is Exec running out of its own bound, not the process finishing; that,
// and a daemon that will not answer, both read as still running. Erasing an
// overrun would break the guarantee; over-reporting one costs a tool call.
func (c *container) aliveOrTimedOut(ctx context.Context, pid int) bool {
	for range 2 {
		alive, err := c.api.processAlive(ctx, c.id, pid)
		if err == nil {
			return alive
		}
		if ctx.Err() != nil {
			break
		}
	}
	return true
}

// Exec waits at most Timeout + killGrace for the command itself; the daemon
// round trips around it (create the exec, collect its code) are bounded by ctx
// and exitBudget instead.
//
// The deadline is enforced twice, and only the second one is a guarantee. The
// watchdog inside the container does the killing, but it is a process the
// command can find and kill; Exec therefore stops waiting on its own clock, and
// treats any command that outlived its deadline as timed out no matter what
// exit code it chose. The one thing a command buys by killing its watchdog is
// overrunSlop of unnoticed overrun.
func (c *container) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	seconds := 0
	if req.Timeout > 0 {
		seconds = int(math.Ceil(req.Timeout.Seconds()))
	}
	// The watchdog can only sleep whole seconds, so its deadline — not the
	// caller's unrounded request — is the one a kill has to have arrived after.
	deadline := time.Duration(seconds) * time.Second

	execID, err := c.api.execCreate(ctx, c.id, execConfig{
		AttachStdout: true,
		AttachStderr: true,
		Cmd: []string{"/bin/bash", "-c", execWrapper,
			"map-exec", req.Command, strconv.Itoa(seconds)},
		WorkingDir: c.workdir,
	})
	if err != nil {
		return sandbox.ExecResult{}, c.wrap(err)
	}

	runCtx := ctx
	if seconds > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, deadline+c.killGrace)
		defer cancel()
	}

	// Start the clock before the request that starts the command, so the round
	// trip is charged to the sandbox and never shortens the command's measured
	// life.
	start := time.Now()
	stream, err := c.api.execStart(runCtx, execID)
	if err != nil {
		return sandbox.ExecResult{}, c.wrap(err)
	}
	defer stream.Close()

	probeCtx, stopProbing := context.WithCancel(runCtx)
	defer stopProbing()
	var probed <-chan verdict
	if seconds == 0 {
		// No deadline: nothing to hit, nothing to hide.
		none := make(chan verdict, 1)
		none <- verdict{}
		probed = none
	} else {
		pid, err := c.execPid(ctx, execID)
		if err != nil {
			return sandbox.ExecResult{}, c.wrap(err)
		}
		// probeCtx times the sleeps and dies when the stream closes; runCtx
		// carries the overrun confirmation, which the stream close must not
		// cancel, and which runCtx's own Timeout+killGrace still bounds.
		probed = c.probeDeadline(probeCtx, runCtx, pid, deadline, start)
	}

	// Drain in the background: a command blocks on a full pipe, so nothing may
	// wait on the command before reading what it wrote.
	type output struct {
		stdout, stderr []byte
		truncated      bool
		err            error
	}
	drained := make(chan output, 1)
	go func() {
		stdout, stderr, truncated, err := demux(stream, sandbox.MaxOutputBytes)
		drained <- output{stdout, stderr, truncated, err}
	}()

	var out output
	var abandoned bool
	select {
	case out = <-drained:
		if out.err != nil {
			return sandbox.ExecResult{}, out.err
		}
	case <-runCtx.Done():
		if ctx.Err() != nil {
			return sandbox.ExecResult{}, ctx.Err()
		}
		// Our own deadline, not the caller's. Stop reading and take what came.
		abandoned = true
		stream.Close()
		out = <-drained
	}

	// The command is over, or has been given up on. Either way both probes have
	// run or been overtaken by the stream closing, which cannot happen while the
	// process holding it is alive.
	stopProbing()
	v := <-probed

	if abandoned && v.overran {
		// The command outlived the watchdog that should have killed it — it can
		// kill that watchdog — so the timeout is called here instead. Its exit
		// code is not ours to collect: it has not exited. Whatever is still
		// running dies with the session's container.
		return sandbox.ExecResult{
			Stdout: string(out.stdout), Stderr: string(out.stderr),
			ExitCode: sigkillExit, TimedOut: true, Truncated: out.truncated,
		}, nil
	}

	// Inspect on the caller's context: runCtx is spent by definition on any
	// command that ran to its deadline.
	code, err := c.exitCode(ctx, execID)
	if err != nil {
		return sandbox.ExecResult{}, err
	}

	// Two ways a finished command can have hit its deadline.
	//
	// One: the watchdog killed it — SIGKILL, and the command was alive to receive
	// it, which a command cannot survive SIGKILL to fake. A command that SIGKILLs
	// itself is normally excluded by that same aliveness, the probe having found
	// it already gone. Two such commands are not excluded, and both read as the
	// watchdog's, erring toward the timeout label rather than away from it: one
	// that kills itself inside the probe's short lead, the deliberate cost of
	// sampling a lead ahead of the deadline; and one whose probe went unanswered,
	// where all that is left to read is an exec stream held open past the deadline
	// by something the command backgrounded, and that says nothing about the
	// command (see alive).
	//
	// Two: it was still running after the deadline and the slop, and exited
	// anyway, which on the honest path is impossible, because the watchdog would
	// have killed it first. (A command the kernel OOM-kills past its deadline
	// reads as a timeout. It hit a limit and produced nothing; the label is close
	// enough, and the alternative is to guess.)
	timedOut := (code == sigkillExit && v.aliveAtDeadline) || v.overran
	return sandbox.ExecResult{
		Stdout:    string(out.stdout),
		Stderr:    string(out.stderr),
		ExitCode:  code,
		TimedOut:  timedOut,
		Truncated: out.truncated,
	}, nil
}

// pollExec inspects the exec until ready is satisfied, giving up after
// exitBudget so a daemon that never reaches the state fails loudly rather than
// hanging; stuck is that give-up error. The daemon round trips here are bounded
// by exitBudget rather than by a command's deadline.
func (c *container) pollExec(ctx context.Context, execID string, ready func(execInfo) bool, stuck string) (execInfo, error) {
	giveUp := time.Now().Add(c.exitBudget)
	for {
		info, err := c.api.execInspect(ctx, execID)
		if err != nil {
			return execInfo{}, err
		}
		if ready(info) {
			return info, nil
		}
		if time.Now().After(giveUp) {
			return execInfo{}, errors.New(stuck)
		}
		select {
		case <-ctx.Done():
			return execInfo{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// execPid is the exec's process as the daemon's host numbers it. A zero pid
// would silently disarm the deadline probes — every later question about the
// command's life would answer "gone" — so it is retried, and then it is fatal.
func (c *container) execPid(ctx context.Context, execID string) (int, error) {
	info, err := c.pollExec(ctx, execID, func(i execInfo) bool { return i.Pid != 0 },
		fmt.Sprintf("docker: exec %s never reported a pid", execID))
	if err != nil {
		return 0, err
	}
	return info.Pid, nil
}

// exitCode polls the finished exec. The stream closes when the process exits,
// but the daemon publishes the code a moment later.
func (c *container) exitCode(ctx context.Context, execID string) (int, error) {
	info, err := c.pollExec(ctx, execID, func(i execInfo) bool { return !i.Running },
		fmt.Sprintf("docker: exec %s still running after its output closed", execID))
	if err != nil {
		return 0, c.wrap(err)
	}
	return info.ExitCode, nil
}

func (c *container) ReadFile(ctx context.Context, path string) ([]byte, error) {
	stream, entry, err := c.openFile(ctx, path, sandbox.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	data := make([]byte, entry.size)
	if _, err := io.ReadFull(entry.reader, data); err != nil {
		return nil, fmt.Errorf("docker: read %s: %w", path, err)
	}
	return data, nil
}

// ReadFileStream hands the archive entry's bytes through as they arrive from
// the daemon, so a harvest-scale file never buffers here; closing the returned
// reader closes the archive stream.
func (c *container) ReadFileStream(ctx context.Context, path string, maxBytes int64) (io.ReadCloser, int64, error) {
	stream, entry, err := c.openFile(ctx, path, maxBytes)
	if err != nil {
		return nil, 0, err
	}
	return struct {
		io.Reader
		io.Closer
	}{entry.reader, stream}, entry.size, nil
}

// fileEntry is an opened archive entry: the tar-framed reader positioned at
// the file's bytes, and the size its header declared.
type fileEntry struct {
	reader io.Reader
	size   int64
}

// openFile fetches path from the container's archive endpoint and classifies
// what came back, refusing anything but a regular file of at most maxBytes.
// On success the caller owns the returned stream and must close it; every
// failure path closes it here.
func (c *container) openFile(ctx context.Context, path string, maxBytes int64) (io.ReadCloser, fileEntry, error) {
	stream, err := c.api.getArchive(ctx, c.id, path)
	if err != nil {
		if statusIs(err, 404) && !containerGone(err) {
			return nil, fileEntry{}, fmt.Errorf("%s: %w", path, sandbox.ErrFileNotExist)
		}
		if !containerGone(err) {
			// The daemon reports a path blocked by a non-directory as a plain 500
			// carrying its own lstat message, so what the read hit is asked in the
			// sandbox rather than parsed out of that message. It is the read's
			// *parent* chain that is in question — the file itself being a regular
			// file is the normal case here — and only a read that already failed
			// asks, so nothing is spent on the path that works.
			if fault := c.pathFault(ctx, gopath.Dir(path)); fault != nil {
				return nil, fileEntry{}, fmt.Errorf("%s: %w", path, fault)
			}
		}
		return nil, fileEntry{}, c.wrap(err)
	}

	archive := tar.NewReader(stream)
	header, err := archive.Next()
	if err != nil {
		stream.Close()
		return nil, fileEntry{}, fmt.Errorf("docker: read %s: %w", path, err)
	}
	switch header.Typeflag {
	case tar.TypeDir:
		stream.Close()
		return nil, fileEntry{}, fmt.Errorf("%s: %w", path, sandbox.ErrIsDirectory)
	case tar.TypeReg:
	default:
		stream.Close()
		return nil, fileEntry{}, fmt.Errorf("%s is not a regular file: %w", path, sandbox.ErrNotRegularFile)
	}
	if header.Size > maxBytes {
		stream.Close()
		return nil, fileEntry{}, fmt.Errorf("%s is %d bytes: %w", path, header.Size, sandbox.ErrFileTooLarge)
	}
	return stream, fileEntry{reader: archive, size: header.Size}, nil
}

// WriteFile lands the bytes as a one-entry tar under a temporary name in the
// target's directory and renames them into place, so a transfer that fails part
// way through leaves the target as it was rather than truncated.
func (c *container) WriteFile(ctx context.Context, path string, data []byte) error {
	dir, tmp := gopath.Dir(path), sandbox.TempName()
	tarball, err := tarFile(tmp, data)
	if err != nil {
		return err
	}
	err = c.api.putArchive(ctx, c.id, dir, bytes.NewReader(tarball))
	if err != nil && !containerGone(err) {
		// The archive endpoint creates nothing and refuses an unusable extraction
		// point three ways: 404 for a directory that is missing, 400 for one that
		// is a non-directory, 500 for one whose own ancestor is. Making the
		// directory answers the first and classifies the rest, so all three go
		// through mkdirAll — and only the container's absence is ErrNotFound,
		// because calling a wrong path a missing sandbox would send the executor
		// looking for the wrong failure.
		if mkErr := c.mkdirAll(ctx, dir); mkErr != nil {
			// A put that was refused outright landed nothing, but one that died
			// mid-transfer left a piece of the entry behind; shed it on the way out
			// rather than let the directory keep it.
			c.discard(ctx, gopath.Join(dir, tmp))
			return mkErr
		}
		err = c.api.putArchive(ctx, c.id, dir, bytes.NewReader(tarball))
	}
	if containerGone(err) {
		return c.gone()
	}
	tmp = gopath.Join(dir, tmp)
	if err != nil {
		// Whatever of the entry landed is nobody's file; the stream path sheds its
		// residue the same way.
		c.discard(ctx, tmp)
		if cErr := c.unreplaceable(ctx, path); cErr != nil {
			return cErr
		}
		if cErr := c.notWritable(ctx, path); cErr != nil {
			return cErr
		}
		return err
	}
	return c.rename(ctx, tmp, path)
}

// WriteFileStream streams size bytes from src into the sandbox as a tar built on
// the fly over a pipe, so a large mount never fully buffers in the executor.
// Unlike WriteFile it cannot replay the body for the mkdir-and-retry dance (a
// one-shot object-storage reader is drained by the first attempt), so it
// pre-creates the parent directory instead — a mount write is rare enough that
// the extra exec is immaterial, and pre-creating is also what classifies a blocked
// path here. The tar writer enforces the byte count: a src yielding fewer or more
// than size bytes fails the archive rather than writing a truncated file, and
// because the bytes were landing under a temporary name, that failure takes the
// partial file with it instead of leaving it at the target.
func (c *container) WriteFileStream(ctx context.Context, path string, src io.Reader, size int64) error {
	dir := gopath.Dir(path)
	if err := c.mkdirAll(ctx, dir); err != nil {
		return err
	}
	tmp := gopath.Join(dir, sandbox.TempName())
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(streamTarFile(pw, gopath.Base(tmp), src, size)) }()
	err := c.api.putArchive(ctx, c.id, dir, pr)
	if containerGone(err) {
		return c.gone()
	}
	if err != nil {
		// However much of the payload landed, it is nobody's file. Leaving it
		// would cost a failed 500 MB mount 500 MB of the sandbox's disk.
		c.discard(ctx, tmp)
		if cErr := c.unreplaceable(ctx, path); cErr != nil {
			return cErr
		}
		if cErr := c.notWritable(ctx, path); cErr != nil {
			return cErr
		}
		return err
	}
	return c.rename(ctx, tmp, path)
}

// WriteFiles lands a whole batch for two execs rather than one per member: the
// members travel as a single archive the daemon extracts, and one exec then
// renames them all into place. That is what #206 asked for — a small write's cost
// here is mostly the exec, and a skill may hold ten thousand files.
//
// It is four steps, and the first two are what make it work on a sandbox that does
// not run as root. The bookkeeping is delivered first, into the workdir, which
// exists; an exec then makes the members' directories from it, INSIDE the sandbox,
// so they belong to whoever the sandbox runs as. Only then are the members
// delivered, and renamed. Letting the daemon make those directories instead would
// make them root's, and a non-root sandbox could rename nothing into them —
// measured: the whole batch fails there, where the file-at-a-time loop this
// replaces succeeded because its own `mkdir -p` ran inside the sandbox too.
//
// Whatever fails after the bookkeeping has landed, the batch sheds what it put in
// the sandbox on the way out; the caller's error is the one that matters, so the
// shedding's own failure is never reported over it.
func (c *container) WriteFiles(ctx context.Context, files []sandbox.FileWrite) error {
	if len(files) == 0 {
		return nil
	}
	b, err := sandbox.NewBulkWrite(c.workdir, files)
	if err != nil {
		return err
	}
	if err := c.putBulk(ctx, b.Bookkeeping); err != nil {
		if containerGone(err) {
			return c.gone()
		}
		c.discardBulk(ctx, b)
		return err
	}
	if err := c.prepareBulk(ctx, b); err != nil {
		c.discardBulk(ctx, b)
		return err
	}
	if err := c.putBulk(ctx, b.Members); err != nil {
		c.discardBulk(ctx, b)
		if containerGone(err) {
			return c.gone()
		}
		return err
	}
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.BulkRenameShell +
		fmt.Sprintf("__map_bulk_rename %s %s", shellQuote(b.Manifest), shellQuote(b.DirList))})
	if err != nil {
		// The bytes are landed and unnamed, and this exec is how they were to be
		// named; shed them rather than leave the sandbox carrying a payload nothing
		// will ever claim.
		c.discardBulk(ctx, b)
		return c.wrap(err)
	}
	if res.ExitCode == 0 {
		return nil
	}
	return b.Fault("docker", res.ExitCode, res.Stderr)
}

// putBulk streams one of the batch's archives to the daemon, which extracts it at
// the container's root. The tar is built on the fly over a pipe rather than
// buffered, as the streaming single write's is: the members are already in memory,
// and a second copy of a large skill is not worth having.
func (c *container) putBulk(ctx context.Context, archive func(io.Writer) error) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(archive(pw)) }()
	err := c.api.putArchive(ctx, c.id, "/", pr)
	pr.CloseWithError(err)
	return err
}

// prepareBulk makes the members' directories inside the sandbox, which is also
// where a path blocked by a non-directory is named.
func (c *container) prepareBulk(ctx context.Context, b *sandbox.BulkWrite) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.BulkPrepareShell +
		fmt.Sprintf("__map_bulk_prepare %s", shellQuote(b.DirList))})
	if err != nil {
		return c.wrap(err)
	}
	if res.ExitCode == 0 {
		return nil
	}
	return b.Fault("docker", res.ExitCode, res.Stderr)
}

// discardBulk sheds what a failed batch left in the sandbox. Its own failure is not
// worth reporting over the write's, exactly as the single write's discard is not.
func (c *container) discardBulk(ctx context.Context, b *sandbox.BulkWrite) {
	_, _ = c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.BulkDiscardShell +
		fmt.Sprintf("__map_bulk_discard %s %s", shellQuote(b.Manifest), shellQuote(b.DirList))})
}

// rename puts a landed temporary file at its target — the step that makes a write
// atomic — after refusing the targets a rename would quietly do the wrong thing
// with. `mv -f file dir` moves the file *into* the directory, so a target that is
// one is answered with the sentinel a read of a directory gets, and the temporary
// file is removed rather than left behind. The daemon's own extraction, given the
// same target, deletes the directory and puts the file where it was (#71). A
// target the shared __map_unreplaceable recognizes — a device node, a bind-mounted
// file — is refused the same way (#205): the `mv` there would fail against a mount
// point anyway (EBUSY), and for a device under a mounted /dev it never had a file
// to move — the daemon extracts the temporary file into the image's /dev on the
// overlay, where the tmpfs mounted over it keeps the container from ever seeing it,
// so the `mv` failed with "cannot stat" and the write read as the sandbox breaking.
//
// The move is also where the target's permission bits would otherwise be lost, so
// the shared __map_preserve_mode carries them onto the temporary file first — the
// k8s backend's write script calls the same function at the same point (#204).
func (c *container) rename(ctx context.Context, tmp, path string) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.PreserveModeShell + sandbox.UnreplaceableShell + fmt.Sprintf(
		"if [ -d %[2]s ]; then rm -f %[1]s; exit %[3]d; fi\n"+
			"if __map_unreplaceable %[2]s; then rm -f %[1]s; exit %[5]d; fi\n"+
			"__map_preserve_mode %[2]s %[1]s\n"+
			"mv -f %[1]s %[2]s || { rm -f %[1]s; exit 1; }\n"+
			// Asked again, because the first answer describes a moment that has
			// passed: something in the sandbox can make the target a directory in
			// between, and then the move puts the file *inside* it and exits 0 —
			// a reported success that wrote nothing where the caller asked.
			"if [ -d %[2]s ]; then rm -f %[2]s/%[4]s; exit %[3]d; fi",
		shellQuote(tmp), shellQuote(path), sandbox.ExitPathIsDirectory, gopath.Base(tmp),
		sandbox.ExitPathNotReplaceable)})
	if err != nil {
		// The bytes are landed and unnamed, and this exec is how they were to be
		// named; shed them rather than leave the sandbox carrying a payload nothing
		// will ever claim.
		c.discard(ctx, tmp)
		return err
	}
	switch res.ExitCode {
	case 0:
		return nil
	case sandbox.ExitPathIsDirectory:
		return fmt.Errorf("%s: %w", path, sandbox.ErrIsDirectory)
	case sandbox.ExitPathNotReplaceable:
		return fmt.Errorf("%s: %w", path, sandbox.ErrNotReplaceable)
	default:
		// The daemon extracts the archive as root, so a parent the sandbox
		// user cannot write takes the PUT and refuses the write only here, at
		// the move, which runs as that user (measured on non-root images:
		// TestWriteIntoARootOwnedParentOnANonRootImage). The same writability
		// question a refused PUT asks is asked again — and a parent that can
		// take a create keeps the raw error: that failure was the transfer's,
		// not the target's (plan 23, #306).
		if cErr := c.notWritable(ctx, path); cErr != nil {
			return cErr
		}
		return fmt.Errorf("docker: write %s: exit %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
}

// pathFault asks the sandbox whether a non-directory is what blocked path,
// returning ErrNotDirectory when it is and nil when it is not. Nil is also what a
// probe that could not run itself returns: the caller has a real failure to report
// already, and a guess about it would be worse than the daemon's own message.
func (c *container) pathFault(ctx context.Context, path string) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.PathFaultShell +
		fmt.Sprintf("__map_path_fault %s\nexit 0", shellQuote(path))})
	if err == nil && res.ExitCode == sandbox.ExitPathNotDirectory {
		return sandbox.ErrNotDirectory
	}
	return nil
}

// unreplaceable asks the sandbox whether the target is one a rename could never
// serve — a directory, a device node, a bind-mounted file — returning the
// matching sentinel and nil otherwise. The rename script asks the same questions
// itself, but a daemon that refuses the archive PUT outright (a read-only
// rootfs refuses every PUT) fails before that script can run, so a refused
// write asks them in an exec of their own — the classification must not depend
// on the PUT landing (#303). Nil is also what a probe that could not run itself
// returns, for pathFault's reason.
func (c *container) unreplaceable(ctx context.Context, path string) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.UnreplaceableShell + fmt.Sprintf(
		"if [ -d %[1]s ]; then exit %[2]d; fi\nif __map_unreplaceable %[1]s; then exit %[3]d; fi\nexit 0",
		shellQuote(path), sandbox.ExitPathIsDirectory, sandbox.ExitPathNotReplaceable)})
	if err != nil {
		return nil
	}
	switch res.ExitCode {
	case sandbox.ExitPathIsDirectory:
		return fmt.Errorf("%s: %w", path, sandbox.ErrIsDirectory)
	case sandbox.ExitPathNotReplaceable:
		return fmt.Errorf("%s: %w", path, sandbox.ErrNotReplaceable)
	}
	return nil
}

// notWritable asks the sandbox whether the target's directory can take a
// create at all, answering ErrNotWritable — with the shell's own strerror text
// as the reason — when it cannot, and nil when it can (the caller keeps the
// daemon's own error) or when the probe could not run, for pathFault's reason.
// It is the classify-on-refusal twin of the k8s write script's exit 20 (plan
// 23, #306): the daemon refuses the archive PUT before any script can say why,
// so a refused write onto a replaceable target attempts the same create the
// extraction would need in an exec of its own — with the agent's credentials,
// in the target's own directory, under a temporary name it removes on success.
// A failed rename asks it too: the daemon extracts as root, so a parent the
// sandbox user cannot write takes the PUT and refuses only the move.
func (c *container) notWritable(ctx context.Context, path string) error {
	probe := gopath.Join(gopath.Dir(path), sandbox.TempName())
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: fmt.Sprintf(
		"export LC_ALL=C\nmsg=$({ : > %[1]s; } 2>&1) || { printf '%%s' \"${msg##*: }\"; exit %[2]d; }\nrm -f %[1]s\nexit 0",
		shellQuote(probe), sandbox.ExitPathNotWritable)})
	if err != nil || res.ExitCode != sandbox.ExitPathNotWritable {
		return nil
	}
	return &sandbox.PathNotWritableError{Path: path, Reason: strings.TrimSpace(res.Stdout)}
}

// discard removes a temporary file a failed write left behind. Its own failure is
// not worth reporting over the write's: the caller already has the error that
// matters, and a sandbox that cannot delete the file is about to be thrown away.
func (c *container) discard(ctx context.Context, tmp string) {
	_, _ = c.Exec(ctx, sandbox.ExecRequest{Command: "rm -f " + shellQuote(tmp)})
}

// mkdirAll makes the directory a write needs, and is where a path blocked by a
// non-directory is named: `mkdir -p` fails there, and the shared shell says
// whether that is why (both backends embed it, so both answer alike).
func (c *container) mkdirAll(ctx context.Context, dir string) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: sandbox.PathFaultShell +
		fmt.Sprintf("export LC_ALL=C\nmkdir -p %[1]s || { __map_path_fault %[1]s; exit 1; }", shellQuote(dir))})
	if err != nil {
		return err
	}
	switch res.ExitCode {
	case 0:
		return nil
	case sandbox.ExitPathNotDirectory:
		return fmt.Errorf("%s: %w", dir, sandbox.ErrNotDirectory)
	default:
		// mkdir failed for a reason that is not a blocking file — a read-only
		// root, a root-owned parent, a full disk. Its own stderr names why,
		// and the strerror tail is the reason the classified refusal carries,
		// as the k8s write script's mkdir branch carries its own (plan 23,
		// #306); a mkdir that said nothing keeps the raw error.
		if reason := strerrorTail(res.Stderr); reason != "" {
			return &sandbox.PathNotWritableError{Path: dir, Reason: reason}
		}
		return fmt.Errorf("docker: mkdir -p %s: exit %d: %s", dir, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
}

// strerrorTail extracts the shell's strerror text — what follows the last
// ": " of stderr's first line — mirroring the write script's "${msg##*: }".
// Empty when there is no such tail to take.
func strerrorTail(stderr string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	if i := strings.LastIndex(line, ": "); i >= 0 {
		return strings.TrimSpace(line[i+2:])
	}
	return ""
}

// Destroy removes the sandbox and, when it is half of a pair, its egress gate.
// Any 404 is success, not just containerGone's: removal has one way to miss, and
// the container's absence is the outcome asked for. No path travels this
// endpoint, so no message can be spoofed into it. The gate is removed after the
// sandbox — the sandbox shares the gate's netns, so the gate must outlive it —
// and both removals are attempted even if the first errors, so a stuck sandbox
// never strands its gate. When both fail, both errors surface (errors.Join) so a
// stuck gate is never masked by the sandbox error.
func (c *container) Destroy(ctx context.Context) error {
	err := removeIgnoring404(ctx, c.api, c.id)
	if c.gateID != "" {
		err = errors.Join(err, removeIgnoring404(ctx, c.api, c.gateID))
	}
	return err
}

// removeIgnoring404 removes a container, treating its absence as success.
func removeIgnoring404(ctx context.Context, api *apiClient, id string) error {
	if err := api.removeContainer(ctx, id); err != nil && !statusIs(err, 404) {
		return err
	}
	return nil
}

// wrap turns "the container is gone" into the contract's sentinel; every other
// failure keeps the daemon's own message — including a 404 for a stale exec id,
// which is a lost exec, not a lost sandbox.
func (c *container) wrap(err error) error {
	if containerGone(err) {
		return c.gone()
	}
	return err
}

func (c *container) gone() error { return fmt.Errorf("%s: %w", c.id, sandbox.ErrNotFound) }

func tarFile(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := streamTarFile(&buf, name, bytes.NewReader(data), int64(len(data))); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// streamTarFile writes a single-entry tar (one regular file of exactly size
// bytes read from src) to w. The tar writer's own length accounting is the
// integrity check: Close fails if src was short, Write fails if it was long.
func streamTarFile(w io.Writer, name string, src io.Reader, size int64) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     size,
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	}); err != nil {
		return fmt.Errorf("docker: build archive: %w", err)
	}
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("docker: build archive: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("docker: build archive: %w", err)
	}
	return nil
}

// shellQuote makes a path a single, literal shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
