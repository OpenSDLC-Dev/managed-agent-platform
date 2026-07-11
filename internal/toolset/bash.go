package toolset

import (
	"context"
	"encoding/json"
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

	out := combine(sandbox.ExecResult{
		Stdout: res.Stdout, Stderr: res.Stderr, Truncated: res.Truncated,
	})
	switch {
	case res.TimedOut:
		// No exit code: on a timeout the sandbox's TimedOut is the authoritative
		// field and the code may be the kill's, or one a command that dodged the
		// kill picked for itself. The dropped state is worth saying — the next
		// call resumes from the last completed one, not from this command's
		// mutations.
		return failf("%s\ncommand timed out after %s; this call's shell state changes were dropped", out, timeout)
	case res.ExitCode != 0:
		return failf("%s\nexit code: %d", out, res.ExitCode)
	}
	return succeed(out)
}
