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

// execWrapper runs the tool's command under a watchdog. Docker has no API to
// kill a running exec, so the kill has to come from inside the container: a
// command that ignores its deadline would otherwise hold a slot forever.
//
// $1 is the command, $2 the timeout in whole seconds ("0" = no limit). The
// watchdog marks before it kills, so exit 124 means "we killed it" and never
// "the command chose to exit 124" — the marker cannot exist unless the sleep
// elapsed with the command still running. Its stdio goes to /dev/null because
// it outlives the command it guards: a watchdog still holding the exec's pipes
// keeps the daemon's stream open for seconds after the command has exited, and
// every tool call pays that.
const execWrapper = `
mark="/tmp/.map-exec-timeout.$$"
/bin/bash -c "$1" &
pid=$!
if [ "$2" != "0" ]; then
  ( sleep "$2"; : > "$mark"; kill -9 "$pid" 2>/dev/null ) >/dev/null 2>&1 &
  guard=$!
fi
wait "$pid"
rc=$?
if [ -n "${guard:-}" ]; then kill -9 "$guard" 2>/dev/null; fi
if [ -e "$mark" ]; then rm -f "$mark"; exit 124; fi
exit "$rc"
`

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
		return &container{api: p.api, id: info.ID, workdir: workdir}, nil
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
			// The watchdog leaves short-lived orphans behind; an init process
			// reaps them instead of letting them pile up as zombies.
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
	return &container{api: p.api, id: id, workdir: workdir}, nil
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
	api     *apiClient
	id      string
	workdir string
}

func (c *container) ID() string { return c.id }

func (c *container) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	seconds := 0
	if req.Timeout > 0 {
		seconds = int(math.Ceil(req.Timeout.Seconds()))
	}
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

	start := time.Now()
	stream, err := c.api.execStart(ctx, execID)
	if err != nil {
		return sandbox.ExecResult{}, c.wrap(err)
	}
	stdout, stderr, truncated, err := demux(stream, sandbox.MaxOutputBytes)
	stream.Close()
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	elapsed := time.Since(start)

	code, err := c.exitCode(ctx, execID)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	return sandbox.ExecResult{
		Stdout:   string(stdout),
		Stderr:   string(stderr),
		ExitCode: code,
		// The wrapper reports 124 only from the watchdog, which cannot have
		// fired before the deadline. Both conditions together leave no way for
		// a command's own exit 124 to be misread as a timeout.
		TimedOut:  seconds > 0 && code == 124 && elapsed >= req.Timeout,
		Truncated: truncated,
	}, nil
}

// exitCode polls the finished exec. The stream closes when the process exits,
// but the daemon publishes the code a moment later.
func (c *container) exitCode(ctx context.Context, execID string) (int, error) {
	for attempt := 0; ; attempt++ {
		info, err := c.api.execInspect(ctx, execID)
		if err != nil {
			return 0, c.wrap(err)
		}
		if !info.Running {
			return info.ExitCode, nil
		}
		if attempt >= 100 {
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
		if statusIs(err, 404) && !noSuchContainer(err) {
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
	if statusIs(err, 404) && !noSuchContainer(err) {
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
	if noSuchContainer(err) {
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

func (c *container) Destroy(ctx context.Context) error {
	err := c.api.removeContainer(ctx, c.id)
	if statusIs(err, 404) {
		return nil
	}
	return err
}

// wrap turns "the container is gone" into the contract's sentinel; every other
// failure keeps the daemon's own message. The exec endpoints have exactly one
// way to 404 — no such container — so the status alone decides.
func (c *container) wrap(err error) error {
	if err == nil {
		return nil
	}
	if statusIs(err, 404) {
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
