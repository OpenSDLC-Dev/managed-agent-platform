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
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// defaultWorkdir is where tools run and relative paths resolve.
const defaultWorkdir = "/workspace"

// sessionLabel tags every container we own, so an operator can find and reap
// sandboxes this platform created without guessing from names.
const sessionLabel = "dev.opensdlc.managed-agent-platform.session-id"

// execWrapper kills the command when its deadline passes. Docker has no API to
// kill a running exec, so this has to happen inside the container. $1 is the
// command, $2 the timeout in whole seconds ("0" = no limit).
//
// The watchdog is best effort by construction, and the sandbox never trusts it.
// It runs beside the command, inside the container, where the command can find
// and kill it: nothing in here is out of the agent's reach. What it buys is
// that an honest command's runaway loop stops burning the sandbox's CPU the
// moment its deadline passes, and that the sandbox learns the real exit code.
// Whether a call actually timed out is decided outside the container, by Exec.
//
// `set -m` puts each job in its own process group, so the kill takes the
// command's children with it: killing the command's shell alone leaves them
// running, holding the exec's pipes open long after the deadline. It is a
// process group, not a process tree — a child that calls setsid escapes it —
// which is another reason Exec keeps its own bound.
//
// Only `wait` has its stderr silenced: bash announces a job it killed
// ("...Killed...") there, and that announcement would land in the tool result
// as if the command had printed it. Redirecting the whole wrapper instead would
// swallow its real failures — a fork that hits RLIMIT_NPROC, a missing shell.
const execWrapper = `
set -m
/bin/bash -c "$1" &
pid=$!
if [ "$2" != "0" ]; then
  ( sleep "$2"; kill -9 -"$pid" 2>/dev/null ) >/dev/null 2>&1 &
  guard=$!
fi
wait "$pid" 2>/dev/null
rc=$?
if [ -n "${guard:-}" ]; then kill -9 -"$guard" 2>/dev/null; fi
exit "$rc"
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
	// rather than to the command: draining the command's output happens after it
	// has exited. Beyond that, a command that outlived its deadline and still
	// chose its own exit code can only have disabled the watchdog, and the
	// answer is a timeout whatever the command says. It must stay under
	// killGrace, or Exec stops waiting before it could ever notice.
	defaultOverrunSlop = 500 * time.Millisecond
	// defaultExitBudget bounds the wait for the daemon to publish an exit code
	// once the exec's output has closed. It is normally instant; the budget is
	// there so a daemon that never stops calling the exec "running" fails
	// loudly instead of hanging.
	defaultExitBudget = 5 * time.Second
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
		workdir = defaultWorkdir
	}
	name := containerName(spec.SessionID)

	info, err := p.api.inspectContainer(ctx, name)
	switch {
	case err == nil:
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

func (p *Provider) attach(id, workdir string) *container {
	return &container{
		api: p.api, id: id, workdir: workdir,
		killGrace: defaultKillGrace, overrunSlop: defaultOverrunSlop, exitBudget: defaultExitBudget,
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
}

func (c *container) ID() string { return c.id }

// Exec waits at most Timeout + killGrace for the command itself; the daemon
// round trips around it (create the exec, collect its code) are bounded by ctx
// and exitBudget instead.
//
// The deadline is enforced twice, and only the second one is a guarantee. The
// watchdog inside the container does the killing, but it is a process sitting
// beside the command, so the command can kill it; Exec therefore stops waiting
// on its own clock, and treats any command that outlived its deadline as timed
// out no matter what exit code it chose. The one thing a command buys by
// killing its watchdog is overrunSlop of unnoticed overrun.
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

	stream, err := c.api.execStart(runCtx, execID)
	if err != nil {
		return sandbox.ExecResult{}, c.wrap(err)
	}
	// Time the command from the moment the daemon hands back its stream, which
	// is as close to "the command started" as the sandbox can stand.
	start := time.Now()
	stdout, stderr, truncated, err := demux(stream, sandbox.MaxOutputBytes)
	stream.Close()
	elapsed := time.Since(start)

	if err != nil {
		// Our own deadline, not the caller's. The watchdog did not finish the
		// job — a command can kill it, or leak a child into a new session that
		// holds these pipes — so the timeout is called here instead. Whatever
		// the command left running dies with the session's container.
		if seconds > 0 && runCtx.Err() != nil && ctx.Err() == nil {
			return sandbox.ExecResult{
				Stdout: string(stdout), Stderr: string(stderr),
				ExitCode: sigkillExit, TimedOut: true, Truncated: truncated,
			}, nil
		}
		return sandbox.ExecResult{}, err
	}

	// The exec finished on its own. Inspect on the caller's context: runCtx is
	// spent by definition on any command that ran to its deadline.
	code, err := c.exitCode(ctx, execID)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	// Two ways a finished command can have hit its deadline. The watchdog killed
	// it: SIGKILL, no earlier than the deadline — a command cannot survive
	// SIGKILL to fake that, and one that kills itself does so before the
	// watchdog could have fired. Or it outlived the deadline and exited anyway,
	// which on the honest path is impossible, because the watchdog would have
	// killed it first. (A command the kernel OOM-kills past its deadline reads
	// as a timeout. It hit a limit and produced nothing; the label is close
	// enough, and the alternative is to guess.)
	timedOut := seconds > 0 &&
		((code == sigkillExit && elapsed >= deadline) || elapsed >= deadline+c.overrunSlop)
	return sandbox.ExecResult{
		Stdout:    string(stdout),
		Stderr:    string(stderr),
		ExitCode:  code,
		TimedOut:  timedOut,
		Truncated: truncated,
	}, nil
}

// exitCode polls the finished exec. The stream closes when the process exits,
// but the daemon publishes the code a moment later.
func (c *container) exitCode(ctx context.Context, execID string) (int, error) {
	giveUp := time.Now().Add(c.exitBudget)
	for {
		info, err := c.api.execInspect(ctx, execID)
		if err != nil {
			return 0, c.wrap(err)
		}
		if !info.Running {
			return info.ExitCode, nil
		}
		if time.Now().After(giveUp) {
			return 0, fmt.Errorf("docker: exec %s still running after its output closed", execID)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
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
