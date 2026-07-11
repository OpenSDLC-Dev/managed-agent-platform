// Package shell runs the built-in bash tool as a persistent per-session shell,
// on top of the sandbox's stateless Exec + file primitives — no new backend
// surface. cwd, exported variables, functions, options, and traps survive
// across calls via a checkpoint on the container's writable layer; the other
// five built-in tools use Exec/ReadFile/WriteFile directly and share nothing
// with the shell's state.
//
// Each call runs as its own exec process, so the sandbox's outside-the-container
// deadline applies to the command verbatim and cannot be forged from inside —
// the property a single always-running shell cannot keep. The one divergence
// from a resident shell: a backgrounded process survives (reachable by pid) but
// the jobs table does not carry across calls.
package shell

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

//go:embed template.sh
var templateScript string

// stateRoot holds every session's checkpoint. It must live on the container's
// writable layer (not a tmpfs) so cwd/env survive a container restart; Destroy
// removes it with the container.
const stateRoot = "/var/lib/map-shell"

// ErrInterrupted reports that a prior attempt at this call started but never
// recorded a result — the executor running it died mid-command. The command is
// not retried, because it may not be idempotent; the caller surfaces this as a
// failed tool result rather than risking a double run.
var ErrInterrupted = errors.New("shell: command interrupted and not retried")

// Request is one bash tool call.
type Request struct {
	Command string        // user command bytes, run verbatim
	Restart bool          // reset the shell (cwd/env/functions/options) first
	Timeout time.Duration // per-command; 0 means only the context bounds it
}

// Result mirrors sandbox.ExecResult with the restart flag the tool reports back.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
	Restarted bool
}

// Run executes one bash tool call against sb, persisting the shell's state in
// the container between calls. session scopes the checkpoint; id is the stable
// tool_use id that makes a reclaim after an executor crash at-most-once.
func Run(ctx context.Context, sb sandbox.Sandbox, session, id domain.ID, req Request) (Result, error) {
	state := stateRoot + "/" + session.String()
	var res Result

	if req.Restart {
		reset := fmt.Sprintf("rm -f %s/cwd %s/env %s/funcs %s/opts %s/traps",
			state, state, state, state, state)
		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: reset}); err != nil {
			return Result{}, err
		}
		res.Restarted = true
		if req.Command == "" {
			return res, nil
		}
	}

	resultPath := fmt.Sprintf("%s/result/%s", state, id)
	cmdPath := fmt.Sprintf("%s/cmd/%s", state, id)

	// Reclaim discipline, keyed to the stable id: a completed call returns its
	// recorded code without re-running (at-most-once); a started-but-unfinished
	// call is not retried.
	if b, err := sb.ReadFile(ctx, resultPath); err == nil {
		code, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
		if convErr != nil {
			return Result{}, fmt.Errorf("shell: corrupt result marker %q: %w", string(b), convErr)
		}
		res.ExitCode = code
		return res, nil
	} else if !errors.Is(err, sandbox.ErrFileNotExist) {
		return Result{}, err
	}
	if _, err := sb.ReadFile(ctx, cmdPath); err == nil {
		return Result{}, ErrInterrupted
	} else if !errors.Is(err, sandbox.ErrFileNotExist) {
		return Result{}, err
	}

	// Fresh run: deliver the command as a file, then exec the template.
	if err := sb.WriteFile(ctx, cmdPath, []byte(req.Command)); err != nil {
		return Result{}, err
	}
	script := strings.NewReplacer(
		"__STATE__", shellSingleQuote(state),
		"__ID__", shellSingleQuote(id.String()),
	).Replace(templateScript)

	er, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script, Timeout: req.Timeout})
	if err != nil {
		return Result{}, err
	}
	res.Stdout = er.Stdout
	res.Stderr = er.Stderr
	res.ExitCode = er.ExitCode
	res.TimedOut = er.TimedOut
	res.Truncated = er.Truncated
	return res, nil
}

// shellSingleQuote wraps s so bash treats it as a single literal word. Session
// and tool ids carry no metacharacters, but quoting keeps the substitution
// unforgeable regardless of what an id ever becomes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
