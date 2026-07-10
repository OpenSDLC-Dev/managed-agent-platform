// Package sandbox is the "hands" boundary: a disposable per-session container
// where the built-in toolset executes. Cattle, not pets — a sandbox dying is
// one tool-call error, never a lost session, because all durable state lives
// in the event log.
//
// The surface is deliberately small. Higher-level tools (glob, grep, edit) are
// pure functions of Exec and the file primitives below, so they live once in
// the toolset layer instead of being re-implemented by every backend.
//
// Divergence from the plan: there is no Attach. Provision is idempotent per
// session — it returns the session's existing sandbox when one is running —
// which is the only thing an executor ever needed Attach for, and it saves
// persisting a sandbox id nothing else would read.
package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// MaxOutputBytes caps what Exec keeps from each of stdout and stderr. It is a
// memory guard on the executor, not the tool-result limit: a command that
// writes a gigabyte must not be able to kill the process that ran it. The
// command still runs to completion — the excess is drained and discarded.
const MaxOutputBytes = 1 << 20

// MaxFileBytes caps ReadFile. The sandbox's filesystem is agent-controlled, so
// a read is an untrusted-length allocation: refuse rather than truncate, since
// a silently half-read file is worse than a failed tool call.
const MaxFileBytes = 4 << 20

var (
	// ErrNotFound reports that the sandbox is gone (destroyed, or reaped by
	// the host). The caller's tool call fails; the session does not.
	ErrNotFound = errors.New("sandbox: no such sandbox")
	// ErrFileNotExist reports a read of a path that does not exist.
	ErrFileNotExist = errors.New("sandbox: no such file")
	// ErrIsDirectory reports a file read of a directory.
	ErrIsDirectory = errors.New("sandbox: path is a directory")
	// ErrFileTooLarge reports a read of a file above MaxFileBytes.
	ErrFileTooLarge = errors.New("sandbox: file too large")
)

// Spec is what a session's sandbox is made of. Image is a platform deployment
// choice (the wire's environment config has no image field); Networking comes
// from the environment.
type Spec struct {
	SessionID  domain.ID
	Image      string
	Workdir    string
	Networking domain.Networking
}

// ExecRequest runs Command through /bin/bash -c inside the sandbox's workdir.
// A zero Timeout means "no limit", and then only the context bounds the call.
type ExecRequest struct {
	Command string
	Timeout time.Duration
}

// ExecResult is a finished command. TimedOut means the command outlived its
// deadline: the sandbox stopped it, or stopped waiting for it, or caught it
// running past the deadline and exiting on its own terms. TimedOut is the
// authoritative field — ExitCode may be the kill's code, or the code a command
// that dodged the kill chose for itself — and the output is whatever arrived.
// Truncated means output exceeded MaxOutputBytes and the tail was discarded.
//
// A backend must decide TimedOut where the sandboxed command cannot reach the
// decision. Anything inside the sandbox is the agent's to tamper with, so a
// deadline enforced only in there is a deadline the command can lift.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// Sandbox is one session's execution environment.
type Sandbox interface {
	// ID identifies the sandbox to the backend (a container id, a pod name).
	ID() string
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
	// ReadFile returns a file's bytes verbatim, binary included.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes data, creating parent directories and overwriting any
	// existing file.
	WriteFile(ctx context.Context, path string, data []byte) error
	// Destroy removes the sandbox. It is idempotent: destroying an already
	// destroyed sandbox is not an error.
	Destroy(ctx context.Context) error
}

// Provider makes sandboxes. Every backend passes the same contract suite
// (internal/sandbox/sandboxtest).
type Provider interface {
	// Provision returns the session's sandbox, creating it only if none is
	// running. Concurrent executors provisioning the same session converge on
	// one sandbox rather than racing to create two.
	Provision(ctx context.Context, spec Spec) (Sandbox, error)
}
