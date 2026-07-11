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

// fakeSandbox is an in-memory Sandbox. The commit rule — point `head` at this
// call's snapshot only when the call finished inside its deadline — is a pure
// function of what Exec reported, so it needs no container. The *Err fields
// inject backend faults so the error paths are exercised without a real outage.
type fakeSandbox struct {
	files      map[string]string
	execs      []string
	execResult sandbox.ExecResult
	execErr    error
	writeErr   error
}

func (f *fakeSandbox) ID() string { return "fake" }

func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.execs = append(f.execs, req.Command)
	return f.execResult, f.execErr
}

func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
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

func statePaths(session, id domain.ID) (head, cmd string) {
	state := "/var/lib/map-shell/" + session.String()
	return state + "/head", state + "/cmd/" + id.String()
}

func TestCommit(t *testing.T) {
	session := domain.NewID("sesn")

	// The command reaches the container as a file, byte for byte, and the
	// template it execs carries the substituted (and quoted) ids.
	t.Run("CommandIsDeliveredAsAFileAndTheTemplateExeced", func(t *testing.T) {
		id := domain.NewID("sevt")
		_, cmdPath := statePaths(session, id)
		sb := &fakeSandbox{execResult: sandbox.ExecResult{Stdout: "hi"}}
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
		if !strings.Contains(sb.execs[0], "'"+id.String()+"'") {
			t.Error("template was not substituted with the quoted tool id")
		}
	})

	// A call that finished inside its deadline commits its snapshot.
	t.Run("CleanExitCommitsTheSnapshot", func(t *testing.T) {
		id := domain.NewID("sevt")
		headPath, _ := statePaths(session, id)
		sb := &fakeSandbox{execResult: sandbox.ExecResult{ExitCode: 3}}
		if _, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "exit 3"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if sb.files[headPath] != id.String() {
			t.Errorf("head = %q, want %q — a non-zero exit is still a finished call", sb.files[headPath], id)
		}
	})

	// A timed-out call does not: its mutations are dropped, whether the SIGKILL
	// landed or the command dodged it, overran, and ran its EXIT trap anyway.
	t.Run("TimedOutCallDoesNotCommitTheSnapshot", func(t *testing.T) {
		id := domain.NewID("sevt")
		headPath, _ := statePaths(session, id)
		sb := &fakeSandbox{execResult: sandbox.ExecResult{ExitCode: 0, TimedOut: true}}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "sleep 300"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.TimedOut {
			t.Fatal("TimedOut not reported")
		}
		if _, ok := sb.files[headPath]; ok {
			t.Error("a timed-out call committed its snapshot — its mutations must be dropped")
		}
	})

	// Restart clears the head pointer, which is the whole reset.
	t.Run("RestartClearsTheHeadPointer", func(t *testing.T) {
		id := domain.NewID("sevt")
		headPath, _ := statePaths(session, id)
		sb := &fakeSandbox{}
		res, err := shell.Run(context.Background(), sb, session, id, shell.Request{Restart: true})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Restarted {
			t.Error("Restarted not reported")
		}
		if len(sb.execs) != 1 {
			t.Fatalf("execs = %v, want exactly the reset", sb.execs)
		}
		if !strings.Contains(sb.execs[0], "rm -f '"+headPath+"'") {
			t.Errorf("reset = %q, want it to remove the quoted head path", sb.execs[0])
		}
	})

	// Restart with a command resets, then runs the command in the same call.
	t.Run("RestartWithCommandResetsThenRuns", func(t *testing.T) {
		id := domain.NewID("sevt")
		headPath, _ := statePaths(session, id)
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
		if sb.files[headPath] != id.String() {
			t.Error("the restarted call's own snapshot was not committed")
		}
	})

	// Every backend fault propagates rather than being swallowed.
	t.Run("BackendFaultsPropagate", func(t *testing.T) {
		boom := errors.New("backend down")

		t.Run("Reset", func(t *testing.T) {
			sb := &fakeSandbox{execErr: boom}
			_, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
				shell.Request{Restart: true, Command: "x"})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the reset fault", err)
			}
			if len(sb.execs) != 1 {
				t.Errorf("execs = %d, want only the failed reset", len(sb.execs))
			}
		})

		t.Run("CommandDelivery", func(t *testing.T) {
			sb := &fakeSandbox{writeErr: boom}
			_, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
				shell.Request{Command: "x"})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the write fault", err)
			}
			if len(sb.execs) != 0 {
				t.Errorf("execed after a failed command delivery: %v", sb.execs)
			}
		})

		t.Run("Exec", func(t *testing.T) {
			sb := &fakeSandbox{execErr: boom}
			_, err := shell.Run(context.Background(), sb, session, domain.NewID("sevt"),
				shell.Request{Command: "x"})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the exec fault", err)
			}
		})
	})
}

// A head pointer that cannot be written is a broken container, not a silent
// loss of continuity: the call fails rather than reporting a result whose state
// the next call will not see.
func TestCommitFaultFailsTheCall(t *testing.T) {
	session, id := domain.NewID("sesn"), domain.NewID("sevt")
	boom := errors.New("disk full")
	sb := &failOnHead{err: boom, headSuffix: "/head"}
	_, err := shell.Run(context.Background(), sb, session, id, shell.Request{Command: "echo hi"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the commit fault", err)
	}
}

// failOnHead writes everything but the head pointer.
type failOnHead struct {
	fakeSandbox
	err        error
	headSuffix string
}

func (f *failOnHead) WriteFile(ctx context.Context, path string, data []byte) error {
	if strings.HasSuffix(path, f.headSuffix) {
		return f.err
	}
	return f.fakeSandbox.WriteFile(ctx, path, data)
}
