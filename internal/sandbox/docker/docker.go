// Package docker is the v1 sandbox backend: one disposable container per
// session, driven over the Docker Engine API. The image must carry /bin/bash
// at that exact path (the plan's image contract) and a POSIX userland.
package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	gopath "path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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
	// SIGKILLs itself within this much of its deadline reads as a timeout.
	defaultProbeLead = 50 * time.Millisecond
)

// Config configures the backend. Host is a Docker daemon address
// (unix:///... or tcp://host:port); empty falls back to DOCKER_HOST and then
// to the well-known socket.
type Config struct {
	Host string
}

// Provider provisions per-session containers.
type Provider struct {
	api *apiClient
}

func New(cfg Config) (*Provider, error) {
	api, err := newAPIClient(cfg.Host)
	if err != nil {
		return nil, err
	}
	return &Provider{api: api}, nil
}

func containerName(sessionID domain.ID) string { return "map-" + string(sessionID) }

// Provision returns the session's container, creating and starting it only if
// none exists. Two executors racing on the same session converge: the loser of
// the create race adopts the winner's container.
func (p *Provider) Provision(ctx context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	if spec.SessionID.IsZero() {
		return nil, errors.New("docker: provision needs a session id")
	}
	if spec.Image == "" {
		return nil, errors.New("docker: provision needs an image")
	}
	workdir := spec.Workdir
	if workdir == "" {
		workdir = sandbox.DefaultWorkdir
	}
	name := containerName(spec.SessionID)

	info, err := p.api.inspectContainer(ctx, name)
	switch {
	case err == nil:
		if err := ours(info, spec.SessionID); err != nil {
			return nil, err
		}
		if !info.State.Running {
			if err := p.api.startContainer(ctx, info.ID); err != nil {
				return nil, err
			}
		}
		return p.attach(info.ID, workdir), nil
	case !statusIs(err, 404):
		return nil, err
	}

	cfg := containerConfig{
		Image: spec.Image,
		// Hold the container open and guarantee the workdir exists. Nothing
		// else runs here: every tool call is its own exec.
		Entrypoint: []string{"/bin/bash", "-c",
			"mkdir -p " + shellQuote(workdir) + " && while :; do sleep 3600; done"},
		Cmd:        []string{},
		WorkingDir: workdir,
		Labels:     map[string]string{sessionLabel: string(spec.SessionID)},
		HostConfig: hostConfig{
			NetworkMode: networkMode(spec.Networking),
			// Tools background processes and orphan them; an init process reaps
			// them instead of letting them pile up as zombies for the session's
			// whole lifetime.
			Init: true,
		},
	}

	id, err := p.api.createContainer(ctx, name, cfg)
	if statusIs(err, 404) { // the image is not on this host yet
		if err := p.api.pullImage(ctx, spec.Image); err != nil {
			return nil, err
		}
		id, err = p.api.createContainer(ctx, name, cfg)
	}
	if statusIs(err, 409) { // another executor created it first
		info, ierr := p.api.inspectContainer(ctx, name)
		if ierr != nil {
			return nil, ierr
		}
		if ierr := ours(info, spec.SessionID); ierr != nil {
			return nil, ierr
		}
		id, err = info.ID, nil
	}
	if err != nil {
		return nil, err
	}
	if err := p.api.startContainer(ctx, id); err != nil {
		return nil, err
	}
	return p.attach(id, workdir), nil
}

// ours refuses a container that merely wears this session's name. The name is
// derived from the session id, so anything else on the daemon can hold it: a
// container left by an earlier deployment, or one that happens to collide.
// Adopting it would hand the agent's commands to a filesystem, an image, and —
// because a container's network mode is fixed when it is created — an egress
// policy that are not the ones this session asked for; a `limited` session must
// never inherit a `bridge` container's route out. This is not a trust boundary
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

func (p *Provider) attach(id, workdir string) *container {
	return &container{
		api: p.api, id: id, workdir: workdir,
		killGrace: defaultKillGrace, overrunSlop: defaultOverrunSlop,
		exitBudget: defaultExitBudget, probeLead: defaultProbeLead,
	}
}

// networkMode fails closed. `limited` means "only AllowedHosts", which needs
// the egress proxy the plan reserves; until it exists, a limited sandbox gets
// no network at all rather than silently unrestricted egress.
func networkMode(net domain.Networking) string {
	if net.Type == domain.NetLimited {
		return "none"
	}
	return "bridge"
}

type container struct {
	api         *apiClient
	id          string
	workdir     string
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
// A probe whose wait is cut short answers false, and correctly: the stream
// cannot close while the process that owns it is alive, so a close before an
// instant is a command that had already finished by it.
func (c *container) probeDeadline(sleepCtx, confirmCtx context.Context, pid int, deadline time.Duration, start time.Time) <-chan verdict {
	answer := make(chan verdict, 1)
	var atDeadline, overran bool
	var wg sync.WaitGroup
	wg.Add(2)
	// Alive as the deadline arrived, so a SIGKILL that follows is the watchdog's?
	go func() {
		defer wg.Done()
		if sleepUntil(sleepCtx, start.Add(deadline-c.probeLead)) {
			atDeadline = c.alive(sleepCtx, pid)
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
// cancels, and it reads that cancellation as "process gone": the stream cannot
// close while the process holding it is alive, so a close before the deadline
// is a command that finished early, not one still running.
func (c *container) alive(ctx context.Context, pid int) bool {
	for range 2 {
		alive, err := c.api.processAlive(ctx, c.id, pid)
		if err == nil {
			return alive
		}
		if ctx.Err() != nil {
			// The stream closed, so the process it was holding is gone, and
			// nobody is waiting on this answer any more.
			return false
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

	// Two ways a finished command can have hit its deadline. The watchdog killed
	// it: SIGKILL, and the command was alive to receive it — a command cannot
	// survive SIGKILL to fake that, and one that kills itself before the
	// pre-deadline probe was already gone when we looked. (A self-SIGKILL inside
	// the probe's short lead reads as the watchdog's: the deliberate cost of
	// sampling a lead ahead of the deadline, and it errs toward a timeout.) Or it
	// was still running after the deadline and the
	// slop, and exited anyway, which on the honest path is impossible, because
	// the watchdog would have killed it first. (A command the kernel OOM-kills
	// past its deadline reads as a timeout. It hit a limit and produced nothing;
	// the label is close enough, and the alternative is to guess.)
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
	stream, err := c.api.getArchive(ctx, c.id, path)
	if err != nil {
		if statusIs(err, 404) && !containerGone(err) {
			return nil, fmt.Errorf("%s: %w", path, sandbox.ErrFileNotExist)
		}
		return nil, c.wrap(err)
	}
	defer stream.Close()

	archive := tar.NewReader(stream)
	header, err := archive.Next()
	if err != nil {
		return nil, fmt.Errorf("docker: read %s: %w", path, err)
	}
	switch header.Typeflag {
	case tar.TypeDir:
		return nil, fmt.Errorf("%s: %w", path, sandbox.ErrIsDirectory)
	case tar.TypeReg:
	default:
		return nil, fmt.Errorf("docker: %s is not a regular file", path)
	}
	if header.Size > sandbox.MaxFileBytes {
		return nil, fmt.Errorf("%s is %d bytes: %w", path, header.Size, sandbox.ErrFileTooLarge)
	}
	data := make([]byte, header.Size)
	if _, err := io.ReadFull(archive, data); err != nil {
		return nil, fmt.Errorf("docker: read %s: %w", path, err)
	}
	return data, nil
}

func (c *container) WriteFile(ctx context.Context, path string, data []byte) error {
	tarball, err := tarFile(gopath.Base(path), data)
	if err != nil {
		return err
	}
	dir := gopath.Dir(path)
	err = c.api.putArchive(ctx, c.id, dir, tarball)
	if statusIs(err, 404) && !containerGone(err) {
		// The archive endpoint does not create parents; only a missing
		// directory can produce this 404, so make it and try once more.
		if mkErr := c.mkdirAll(ctx, dir); mkErr != nil {
			return mkErr
		}
		err = c.api.putArchive(ctx, c.id, dir, tarball)
	}
	// Only the container's absence is ErrNotFound here — the other 404 means
	// the path is wrong, and calling that a missing sandbox would send the
	// executor looking for the wrong failure.
	if containerGone(err) {
		return c.gone()
	}
	return err
}

func (c *container) mkdirAll(ctx context.Context, dir string) error {
	res, err := c.Exec(ctx, sandbox.ExecRequest{Command: "mkdir -p " + shellQuote(dir)})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("docker: mkdir -p %s: exit %d: %s", dir, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Destroy takes any 404 as success, not just containerGone's: removal has one
// way to miss, and the container's absence is the outcome asked for. No path
// travels this endpoint, so no message can be spoofed into it.
func (c *container) Destroy(ctx context.Context) error {
	err := c.api.removeContainer(ctx, c.id)
	if statusIs(err, 404) {
		return nil
	}
	return err
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
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("docker: build archive: %w", err)
	}
	if _, err := tw.Write(data); err != nil {
		return nil, fmt.Errorf("docker: build archive: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("docker: build archive: %w", err)
	}
	return buf.Bytes(), nil
}

// shellQuote makes a path a single, literal shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
