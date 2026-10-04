package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
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

// ProbeEach lists, in one exec that a real bash runs under an image's startup
// file that prints, moves and traps (run), which of the paths are not there:
// each path's answer in order — quoted whole, a quote or a newline in it
// included, a directory there as much as a file, a path under a directory
// that is not. No paths asks nothing.
func TestProbeEachListsWhatIsNotThere(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "it's", "new\nline"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := []string{dir + "/a", dir + "/gone", dir + "/it's", dir + "/it's gone", dir + "/new\nline", dir + "/sub", dir + "/sub/none"}
	execs := 0
	sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		execs++
		return run(t, "bash", req.Command, ""), nil
	}}
	got := sandbox.ProbeEach(context.Background(), sb, paths...)
	want := []sandbox.Presence{sandbox.Present, sandbox.Absent, sandbox.Present, sandbox.Absent, sandbox.Present, sandbox.Present, sandbox.Absent}
	if !slices.Equal(got, want) || execs != 1 {
		t.Errorf("ProbeEach = %v in %d execs, want %v in one", got, execs, want)
	}
	if got := sandbox.ProbeEach(context.Background(), sb); len(got) != 0 || execs != 1 {
		t.Errorf("ProbeEach of nothing = %v after %d execs, want nothing asked", got, execs)
	}
}

// ProbeEach answers only what its batch listed. An answer it cannot read —
// an exec that failed, the startup's flood among them; output the frame did
// not carry whole; a timeout; an exit other than 0; a line that is not an
// index of the batch's — is no answer about any path of the batch, which a
// caller must not take for Absent (#860).
func TestProbeEachTakesAnUnreadableAnswerForNone(t *testing.T) {
	answer := func(res sandbox.ExecResult, framed bool, err error) sandbox.Sandbox {
		return execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			_, f, ok := sandboxtest.Unwrap(req.Command)
			if !ok {
				t.Fatalf("an unframed probe: %q", req.Command)
			}
			if framed {
				res = sandboxtest.Framed(f, res)
			}
			return res, err
		}}
	}
	none := []sandbox.Presence{sandbox.PresenceUnknown, sandbox.PresenceUnknown}
	for _, c := range []struct {
		name   string
		res    sandbox.ExecResult
		framed bool
		err    error
		want   []sandbox.Presence
	}{
		{"b listed", sandbox.ExecResult{Stdout: "1\n"}, true, nil, []sandbox.Presence{sandbox.Present, sandbox.Absent}},
		{"none listed", sandbox.ExecResult{}, true, nil, []sandbox.Presence{sandbox.Present, sandbox.Present}},
		{"both listed", sandbox.ExecResult{Stdout: "0\n1\n"}, true, nil, []sandbox.Presence{sandbox.Absent, sandbox.Absent}},
		{"the startup's flood", sandbox.ExecResult{}, false, &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}, none},
		{"the sandbox gone", sandbox.ExecResult{}, false, sandbox.ErrNotFound, none},
		{"no frame", sandbox.ExecResult{Stdout: "1\n"}, false, nil, none},
		{"cut short", sandbox.ExecResult{Stdout: "1\n", StdoutTruncated: true}, true, nil, none},
		{"timed out", sandbox.ExecResult{Stdout: "1\n", TimedOut: true}, true, nil, none},
		{"another exit", sandbox.ExecResult{Stdout: "1\n", ExitCode: 2}, true, nil, none},
		{"a line that is no index", sandbox.ExecResult{Stdout: "banner\n1\n"}, true, nil, none},
		{"an index past the batch", sandbox.ExecResult{Stdout: "2\n"}, true, nil, none},
		{"no newline after the last", sandbox.ExecResult{Stdout: "1"}, true, nil, none},
	} {
		if got := sandbox.ProbeEach(context.Background(), answer(c.res, c.framed, c.err), "/w/a", "/w/b"); !slices.Equal(got, c.want) {
			t.Errorf("%s: ProbeEach = %v, want %v", c.name, got, c.want)
		}
	}
}

// 5,000 mounts of 1 KB paths, the directory holding them all removed: each
// path is asked once, in as few execs as the bound on one command allows
// (MaxCommandBytes) — at about 1 KB a path, 44 — each within it; never an
// exec per path, or a search that halves the set, which took some 10,000
// execs and outran the stall budget. A batch that does not answer leaves its
// own paths unknown, and no other batch's.
func TestProbeEachBoundsItsExecs(t *testing.T) {
	var paths []string
	for i := range 5000 {
		paths = append(paths, fmt.Sprintf("/mnt/session/uploads/%04d/%s", i, strings.Repeat("p", 1000)))
	}
	var cmds []string
	sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if err := sandbox.CheckCommand(req.Command); err != nil {
			return sandbox.ExecResult{}, err
		}
		cmds = append(cmds, req.Command)
		if len(cmds) == 2 {
			return sandbox.ExecResult{}, &sandbox.StartupOutputError{What: "the command's exit record", Ran: true}
		}
		script, f, ok := sandboxtest.Unwrap(req.Command)
		res, isProbe := sandboxtest.AnswerProbeEach(script, func(string) bool { return false })
		if !ok || !isProbe {
			t.Fatalf("not a probe: %q", req.Command)
		}
		return sandboxtest.Framed(f, res), nil
	}}
	got := sandbox.ProbeEach(context.Background(), sb, paths...)
	if len(cmds) > 44 {
		t.Errorf("ProbeEach asked %d paths in %d execs, want at most 44", len(paths), len(cmds))
	}
	asked := 0
	for _, cmd := range cmds {
		asked += strings.Count(cmd, "test -e ")
	}
	if asked != len(paths) {
		t.Fatalf("the batches asked after %d paths, want all %d", asked, len(paths))
	}
	unanswered := strings.Count(cmds[1], "test -e ")
	var counts [3]int
	for _, p := range got {
		counts[p]++
	}
	if counts[sandbox.PresenceUnknown] != unanswered || counts[sandbox.Absent] != len(paths)-unanswered {
		t.Errorf("answers = %d unknown, %d absent; want the unanswered batch's %d unknown and the rest absent",
			counts[sandbox.PresenceUnknown], counts[sandbox.Absent], unanswered)
	}
}
