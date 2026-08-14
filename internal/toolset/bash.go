package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/shell"
)

// bashInput is the wire's BetaManagedAgentsAgentToolset20260401BashInput: every
// field optional, command omitted only when restart is true.
type bashInput struct {
	Command   string `json:"command"`
	Restart   bool   `json:"restart"`
	TimeoutMs int64  `json:"timeout_ms"`
}

func (r Runner) bash(ctx context.Context, id domain.ID, raw json.RawMessage) (Result, error) {
	var in bashInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid bash input: %v", err)
	}
	if in.Command == "" && !in.Restart {
		return failf("bash: command is required")
	}

	timeout := DefaultTimeout
	if in.TimeoutMs > 0 {
		// Clamp before scaling: a model-supplied millisecond count large enough
		// to overflow a Duration would otherwise come out negative — an instant
		// deadline instead of a long one.
		if ms := in.TimeoutMs; ms > MaxTimeout.Milliseconds() {
			timeout = MaxTimeout
		} else {
			timeout = time.Duration(ms) * time.Millisecond
		}
	}

	res, err := shell.Run(ctx, r.Sandbox, r.Session, id, shell.Request{
		Command: in.Command, Restart: in.Restart, Timeout: timeout,
	})
	if err != nil {
		return Result{}, err
	}
	if in.Command == "" {
		return succeed("bash session restarted")
	}

	// Sanitized here, not just in dispatch: the failure arms below cap through
	// capWithTrailer first, and NUL bytes must not spend the budget that a
	// command's real output (its stderr above all) needs to survive.
	out := SanitizeText(combine(sandbox.ExecResult{
		Stdout: res.Stdout, Stderr: res.Stderr, Truncated: res.Truncated,
	}))
	// The failure arms below spill for the same reason: they cap before
	// dispatch, so by the time dispatch's own spill hook runs the tail is
	// already gone. The success arm falls through to dispatch uncapped and
	// spills there — never here, or the same call would write its file twice.
	// The status trailer is capped WITH the output rather than after it, so a
	// command whose output overruns MaxOutputBytes does not lose the one line
	// that says whether it failed.
	switch {
	case res.TimedOut:
		// No exit code: on a timeout the sandbox's TimedOut is the authoritative
		// field and the code may be the kill's, or one a command that dodged the
		// kill picked for itself. The dropped state is worth saying — the next
		// call resumes from the last completed one, not from this command's
		// mutations.
		trailer := fmt.Sprintf("\ncommand timed out after %s; this call's shell state changes were dropped", timeout)
		return Result{Content: capWithTrailer(out, trailer, r.spill(ctx, id, out)), IsError: true}, nil
	case res.ExitCode != 0:
		return Result{Content: capWithTrailer(out, fmt.Sprintf("\nexit code: %d", res.ExitCode), r.spill(ctx, id, out)), IsError: true}, nil
	}
	return succeed(out)
}
