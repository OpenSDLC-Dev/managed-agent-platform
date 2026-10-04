package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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

// A set whose probe is too long for one exec — 130 mounts of 1 KB paths —
// is asked in batches, each within the bound, rather than refused unasked:
// any batch that says Absent is the answer, and a batch that ran and did not
// answer makes it unknown only where none said Absent. A path past the bound
// alone, refused before anything ran, asked nothing, and reads as Absent.
func TestProbePathsBatchesASetTooLongForOneExec(t *testing.T) {
	var paths []string
	for i := range 130 {
		paths = append(paths, fmt.Sprintf("/mnt/session/uploads/%03d/%s", i, strings.Repeat("p", 1000)))
	}
	missing := paths[117]
	run := func(answer func(cmd string) (sandbox.ExecResult, error)) (sandbox.Presence, []string) {
		var cmds []string
		sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			if err := sandbox.CheckCommand(req.Command); err != nil {
				return sandbox.ExecResult{}, err
			}
			cmds = append(cmds, req.Command)
			return answer(req.Command)
		}}
		return sandbox.ProbePaths(context.Background(), sb, paths...), cmds
	}

	got, cmds := run(func(string) (sandbox.ExecResult, error) { return sandbox.ExecResult{}, nil })
	if got != sandbox.Present || len(cmds) < 2 {
		t.Fatalf("all present = %v in %d execs, want Present in more than one", got, len(cmds))
	}
	asked := 0
	for _, cmd := range cmds {
		asked += strings.Count(cmd, "test -e ")
	}
	if asked != len(paths) {
		t.Errorf("the batches asked after %d paths, want all %d", asked, len(paths))
	}
	absentIn := func(cmd string) (sandbox.ExecResult, error) {
		if strings.Contains(cmd, missing) {
			return sandbox.ExecResult{ExitCode: 1}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	if got, _ := run(absentIn); got != sandbox.Absent {
		t.Errorf("one path absent = %v, want Absent", got)
	}
	flood := &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
	if got, _ := run(func(cmd string) (sandbox.ExecResult, error) {
		if !strings.Contains(cmd, missing) {
			return sandbox.ExecResult{}, flood
		}
		return absentIn(cmd)
	}); got != sandbox.Absent {
		t.Errorf("one batch absent, the others unanswered = %v, want Absent", got)
	}
	if got, _ := run(func(cmd string) (sandbox.ExecResult, error) {
		if strings.Contains(cmd, missing) {
			return sandbox.ExecResult{}, flood
		}
		return sandbox.ExecResult{}, nil
	}); got != sandbox.PresenceUnknown {
		t.Errorf("one batch unanswered, the others present = %v, want PresenceUnknown", got)
	}

	huge := "/" + strings.Repeat("h", sandbox.MaxCommandBytes)
	sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if err := sandbox.CheckCommand(req.Command); err != nil {
			return sandbox.ExecResult{}, err
		}
		return sandbox.ExecResult{}, nil
	}}
	if got := sandbox.ProbePaths(context.Background(), sb, "/w/a", huge); got != sandbox.Absent {
		t.Errorf("a path past the bound alone = %v, want Absent: refused unasked", got)
	}
}

// statOnly is a sandbox whose ReadFileStream alone is answered.
type statOnly struct {
	sandbox.Sandbox
	err error
}

func (s statOnly) ReadFileStream(_ context.Context, _ string, maxBytes int64) (io.ReadCloser, int64, error) {
	if maxBytes != 0 {
		return nil, 0, fmt.Errorf("stat read a cap of %d bytes, want 0", maxBytes)
	}
	if s.err != nil {
		return nil, 0, s.err
	}
	return io.NopCloser(strings.NewReader("")), 0, nil
}

// StatPresence reads a path capped at no bytes, and takes the read's refusal
// for what is there: a directory, no regular file, a file past the cap —
// present; nothing there, or a parent no directory — absent; anything else,
// the startup's flood among it, no answer.
func TestStatPresenceReadsTheRefusal(t *testing.T) {
	for _, c := range []struct {
		err  error
		want sandbox.Presence
	}{
		{nil, sandbox.Present},
		{fmt.Errorf("/w/.git: %w", sandbox.ErrIsDirectory), sandbox.Present},
		{fmt.Errorf("/w/.git: %w", sandbox.ErrFileTooLarge), sandbox.Present},
		{sandbox.ErrNotRegularFile, sandbox.Present},
		{fmt.Errorf("/w/.git: %w", sandbox.ErrFileNotExist), sandbox.Absent},
		{sandbox.ErrNotDirectory, sandbox.Absent},
		{&sandbox.StartupOutputError{What: "the read of /w/.git"}, sandbox.PresenceUnknown},
		{errors.New("exec: stream lost"), sandbox.PresenceUnknown},
	} {
		if got := sandbox.StatPresence(context.Background(), statOnly{err: c.err}, "/w/.git"); got != c.want {
			t.Errorf("StatPresence over %v = %v, want %v", c.err, got, c.want)
		}
	}
}
