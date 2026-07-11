// Package shell runs the built-in bash tool as a persistent per-session shell,
// on top of the sandbox's stateless Exec + file primitives — no new backend
// surface. cwd, exported variables, functions, aliases, and shell options
// survive across calls via a snapshot on the container's writable layer; the
// other five built-in tools use Exec/ReadFile/WriteFile directly and share
// nothing with the shell's state.
//
// Each call runs as its own exec process, so the sandbox's outside-the-container
// deadline applies to the command verbatim and cannot be forged from inside —
// the property a single always-running shell cannot keep, because with the
// command running AS the resident shell, foreground-versus-background is
// shell-internal state the command can rewrite.
//
// What this package does NOT do is decide whether a tool call may run twice. The
// snapshot lives in the sandbox, and a sandbox is cattle: its filesystem is
// agent-writable, and it can be reaped and re-provisioned under a retry, so it is
// neither a trustworthy nor a durable ledger. At-most-once belongs to the
// executor and the work queue, whose store is the event log.
//
// Divergences from a resident shell — all of them, each pinned by a test:
//   - The `jobs` table does not carry. A backgrounded process survives (it keeps
//     running, reachable by pid), but the next call is a new shell with an empty
//     job table.
//   - Plain (non-exported) variables do not carry; exported ones do. Nothing in
//     `declare` separates a user's plain variables from bash's own internals, so
//     the snapshot draws the line at `export`.
//   - Traps do not carry, and a command's EXIT trap fires at the end of that call
//     rather than at session end (there is no session-long shell to end). A
//     command that BOTH installs its own EXIT trap AND exits through it skips
//     that call's snapshot.
//   - A timed-out call's mutations are dropped. A resident shell would keep the
//     ones made before the kill; a SIGKILL leaves no chance to snapshot them, so
//     dropping them is the only behaviour available consistently on both the
//     killed path and the dodged-the-kill-and-overran path.
//   - The timeout bounds the whole call — restore, command, snapshot — not the
//     command alone. The bracket is milliseconds, but a very short timeout pays
//     for it.
//
// The snapshot is the agent's own shell state, not a security boundary: a command
// running as root in the container can rewrite or delete it, and only sabotages
// its own session by doing so. The guarantees that matter — the deadline, and
// at-most-once — are enforced outside the container, where it cannot reach them.
package shell

import (
	"context"
	_ "embed"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

//go:embed template.sh
var templateScript string

// stateRoot holds every session's shell state. It must live on the container's
// writable layer (not a tmpfs) so cwd/env survive a container restart; Destroy
// removes it with the container.
const stateRoot = "/var/lib/map-shell"

// Request is one bash tool call.
type Request struct {
	Command string        // user command bytes, run verbatim
	Restart bool          // reset the shell (cwd/env/functions/aliases/options) first
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

// Run executes one bash tool call against sb, carrying the shell's state in the
// container between calls. session scopes the state; id names this call's
// snapshot and its command file.
func Run(ctx context.Context, sb sandbox.Sandbox, session, id domain.ID, req Request) (Result, error) {
	state := stateRoot + "/" + session.String()
	var res Result

	if req.Restart {
		// Clearing the head pointer is the whole reset: the next call finds no
		// committed snapshot and starts from a fresh shell in the workdir. The
		// container's files are untouched, as the reference's restart leaves them.
		reset := "rm -f " + shellSingleQuote(state+"/head")
		if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: reset}); err != nil {
			return Result{}, err
		}
		res.Restarted = true
		if req.Command == "" {
			return res, nil
		}
	}

	if err := sb.WriteFile(ctx, state+"/cmd/"+id.String(), []byte(req.Command)); err != nil {
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
	res.Stdout, res.Stderr = er.Stdout, er.Stderr
	res.ExitCode, res.TimedOut, res.Truncated = er.ExitCode, er.TimedOut, er.Truncated

	// Commit this call's snapshot only if the call finished inside its deadline.
	// A timed-out call leaves its snapshot uncommitted, so its mutations are
	// dropped whether the watchdog's SIGKILL landed or the command dodged the
	// kill, overran, and exited on its own terms — where the EXIT trap does run.
	// It also means a command the sandbox abandoned cannot reach a later call by
	// writing its snapshot long after Exec stopped waiting: that write lands in an
	// id-scoped directory nothing will ever point at.
	if !res.TimedOut {
		if err := sb.WriteFile(ctx, state+"/head", []byte(id.String())); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}

// shellSingleQuote wraps s so bash treats it as a single literal word. Session
// and tool ids carry no metacharacters, but quoting keeps the substitution
// unforgeable regardless of what an id ever becomes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
