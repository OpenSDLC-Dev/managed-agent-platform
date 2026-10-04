package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// ProbePaths tests every path in one scripted exec, each quoted whole, and
// answers only what the exec said: Present on exit 0, Absent on exit 1, and
// PresenceUnknown for anything else — an error, the startup's among them, a
// timeout, another exit — which a caller must not take for Absent (#860).
func TestProbePathsAnswersOnlyWhatTheExecSaid(t *testing.T) {
	var got string
	answer := func(res sandbox.ExecResult, err error) sandbox.Sandbox {
		return execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			got = req.Command
			return res, err
		}}
	}
	for _, c := range []struct {
		name string
		res  sandbox.ExecResult
		err  error
		want sandbox.Presence
	}{
		{"exit 0", sandbox.ExecResult{}, nil, sandbox.Present},
		{"exit 1", sandbox.ExecResult{ExitCode: 1}, nil, sandbox.Absent},
		{"another exit", sandbox.ExecResult{ExitCode: 127}, nil, sandbox.PresenceUnknown},
		{"timed out", sandbox.ExecResult{ExitCode: 1, TimedOut: true}, nil, sandbox.PresenceUnknown},
		{"the startup's flood", sandbox.ExecResult{}, &sandbox.StartupOutputError{What: "x", Ran: true}, sandbox.PresenceUnknown},
		{"the sandbox gone", sandbox.ExecResult{}, sandbox.ErrNotFound, sandbox.PresenceUnknown},
	} {
		if p := sandbox.ProbePaths(context.Background(), answer(c.res, c.err), "/w/a", "/w/it's"); p != c.want {
			t.Errorf("%s: ProbePaths = %v, want %v", c.name, p, c.want)
		}
	}
	if want := sandbox.Script(`test -e '/w/a' && test -e '/w/it'\''s' && true`); got != want {
		t.Errorf("command = %q, want %q", got, want)
	}
}

// ReadAnswered tells a read's answer about a path — its bytes, or a path
// sentinel — from an error that says nothing of it.
func TestReadAnswered(t *testing.T) {
	for _, err := range []error{nil, sandbox.ErrFileNotExist, sandbox.ErrIsDirectory, sandbox.ErrNotRegularFile,
		sandbox.ErrNotDirectory, fmt.Errorf("/x: %w", sandbox.ErrFileTooLarge)} {
		if !sandbox.ReadAnswered(err) {
			t.Errorf("ReadAnswered(%v) = false, want true", err)
		}
	}
	for _, err := range []error{&sandbox.StartupOutputError{What: "the read of /x"}, sandbox.ErrNotFound, errors.New("exec: connection reset")} {
		if sandbox.ReadAnswered(err) {
			t.Errorf("ReadAnswered(%v) = true, want false", err)
		}
	}
}
