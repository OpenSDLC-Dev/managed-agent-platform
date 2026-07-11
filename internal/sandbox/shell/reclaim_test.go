package shell_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/shell"
)

// fakeSandbox is an in-memory Sandbox for the reclaim state machine, which is a
// pure function of the checkpoint files and needs no container. Exec records
// the commands it was handed so a test can assert whether the command was run
// at all — the crux of at-most-once. The *Err fields inject backend faults so
// the error-propagation paths are exercised without a real outage.
type fakeSandbox struct {
	files      map[string]string
	execs      []string
	execResult sandbox.ExecResult
	execErr    error
	writeErr   error
	readErrFor map[string]error // path -> error ReadFile returns instead of the bytes
}

func (f *fakeSandbox) ID() string { return "fake" }

func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.execs = append(f.execs, req.Command)
	return f.execResult, f.execErr
}

func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	if e := f.readErrFor[path]; e != nil {
		return nil, e
	}
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, sandbox.ErrFileNotExist
}

func (f *fakeSandbox) WriteFile(_ context.Context, path string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

func (f *fakeSandbox) Destroy(_ context.Context) error { return nil }

func statePaths(session, id domain.ID) (result, cmd string) {
	state := "/var/lib/map-shell/" + session.String()
	return state + "/result/" + id.String(), state + "/cmd/" + id.String()
}

func TestReclaim(t *testing.T) {
	session := domain.NewID("sesn")

	// A completed call returns its recorded exit code and never runs again —
	// the at-most-once guarantee an executor crash-and-retry depends on.
	t.Run("CompletedCallReturnsRecordedCodeWithoutRerunning", func(t *testing.T) {
		id := domain.NewID("sevt")
		resultPath, _ := statePaths(session, id)
		sb := &fakeSandbox{files: map[string]string{resultPath: "7\n"}}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "rm -rf /"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.ExitCode != 7 {
			t.Errorf("ExitCode = %d, want 7 (the recorded code)", res.ExitCode)
		}
		if len(sb.execs) != 0 {
			t.Errorf("a completed call re-ran the command: execs=%v", sb.execs)
		}
	})

	// A started-but-unfinished call is not retried: the command may not be
	// idempotent, so the caller gets ErrInterrupted rather than a second run.
	t.Run("InflightCallIsNotRetried", func(t *testing.T) {
		id := domain.NewID("sevt")
		_, cmdPath := statePaths(session, id)
		sb := &fakeSandbox{files: map[string]string{cmdPath: "make deploy"}}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "make deploy"})
		if !errors.Is(err, shell.ErrInterrupted) {
			t.Fatalf("err = %v, want ErrInterrupted", err)
		}
		if len(sb.execs) != 0 {
			t.Errorf("an interrupted call was re-executed: execs=%v", sb.execs)
		}
	})

	// A fresh call delivers the command as a file, then execs the template.
	t.Run("FreshCallWritesCommandThenExecs", func(t *testing.T) {
		id := domain.NewID("sevt")
		_, cmdPath := statePaths(session, id)
		sb := &fakeSandbox{execResult: sandbox.ExecResult{Stdout: "hi", ExitCode: 0}}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "echo hi"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Stdout != "hi" {
			t.Errorf("Stdout = %q, want hi", res.Stdout)
		}
		if sb.files[cmdPath] != "echo hi" {
			t.Errorf("command file = %q, want the command bytes verbatim", sb.files[cmdPath])
		}
		if len(sb.execs) != 1 {
			t.Fatalf("execs = %d, want exactly one template run", len(sb.execs))
		}
		if !strings.Contains(sb.execs[0], id.String()) {
			t.Error("template was not substituted with the tool id")
		}
	})

	// A corrupt result marker is a hard error, not a silent zero exit.
	t.Run("CorruptResultMarkerErrors", func(t *testing.T) {
		id := domain.NewID("sevt")
		resultPath, _ := statePaths(session, id)
		sb := &fakeSandbox{files: map[string]string{resultPath: "not-a-number"}}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "x"})
		if err == nil || errors.Is(err, shell.ErrInterrupted) {
			t.Fatalf("err = %v, want a corrupt-marker error", err)
		}
		if !strings.Contains(err.Error(), "corrupt result marker") {
			t.Errorf("err = %v, want it to name the corrupt marker", err)
		}
	})

	// A result-read fault that is not ENOENT propagates rather than being read
	// as "no checkpoint yet" — a probe that can't see the marker must not re-run.
	t.Run("ResultReadFaultPropagates", func(t *testing.T) {
		id := domain.NewID("sevt")
		resultPath, _ := statePaths(session, id)
		boom := errors.New("backend down")
		sb := &fakeSandbox{readErrFor: map[string]error{resultPath: boom}}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "x"})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the backend fault", err)
		}
		if len(sb.execs) != 0 {
			t.Errorf("ran the command despite an unreadable marker: execs=%v", sb.execs)
		}
	})

	// The command-marker read has the same discipline: a non-ENOENT fault there
	// must propagate, not fall through to a re-run.
	t.Run("CommandReadFaultPropagates", func(t *testing.T) {
		id := domain.NewID("sevt")
		_, cmdPath := statePaths(session, id)
		boom := errors.New("backend down")
		sb := &fakeSandbox{readErrFor: map[string]error{cmdPath: boom}}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "x"})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the backend fault", err)
		}
		if len(sb.execs) != 0 {
			t.Errorf("ran the command despite an unreadable marker: execs=%v", sb.execs)
		}
	})

	// A WriteFile fault while delivering the command surfaces as an error, not a
	// run with an empty command file.
	t.Run("CommandWriteFaultPropagates", func(t *testing.T) {
		id := domain.NewID("sevt")
		boom := errors.New("disk full")
		sb := &fakeSandbox{writeErr: boom}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "x"})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the write fault", err)
		}
		if len(sb.execs) != 0 {
			t.Errorf("execed after a failed command delivery: execs=%v", sb.execs)
		}
	})

	// An Exec fault on the fresh run propagates as an error.
	t.Run("ExecFaultPropagates", func(t *testing.T) {
		id := domain.NewID("sevt")
		boom := errors.New("exec failed")
		sb := &fakeSandbox{execErr: boom}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "x"})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the exec fault", err)
		}
	})

	// Restart with no command resets the checkpoint and returns without a run.
	t.Run("RestartWithoutCommandResetsAndReturns", func(t *testing.T) {
		id := domain.NewID("sevt")
		sb := &fakeSandbox{}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Restart: true})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Restarted {
			t.Error("Restarted not reported")
		}
		if len(sb.execs) != 1 || !strings.Contains(sb.execs[0], "rm -f") {
			t.Fatalf("restart should run exactly one rm -f reset, got %v", sb.execs)
		}
	})

	// A reset fault on restart propagates before any command runs.
	t.Run("RestartResetFaultPropagates", func(t *testing.T) {
		id := domain.NewID("sevt")
		boom := errors.New("reset failed")
		sb := &fakeSandbox{execErr: boom}
		_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Restart: true, Command: "echo hi"})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the reset fault", err)
		}
		if len(sb.execs) != 1 {
			t.Errorf("execs = %d, want only the failed reset", len(sb.execs))
		}
	})

	// Restart with a command resets, then runs the command in one call.
	t.Run("RestartWithCommandResetsThenRuns", func(t *testing.T) {
		id := domain.NewID("sevt")
		sb := &fakeSandbox{}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Restart: true, Command: "echo hi"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Restarted {
			t.Error("Restarted not reported")
		}
		if len(sb.execs) != 2 {
			t.Fatalf("execs = %d, want reset then template", len(sb.execs))
		}
		if !strings.Contains(sb.execs[0], "rm -f") {
			t.Errorf("first exec = %q, want the reset", sb.execs[0])
		}
	})
}
