package toolset_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

const testImage = "debian:stable-slim"

// runner gives the whole suite one real container. Each subtest works under its
// own directory beneath the workdir, and bash subtests take a fresh session so
// they never inherit another's shell state. A missing daemon is a hard failure,
// as with the other suites — skipping would hollow out the coverage gate.
func runner(t *testing.T) toolset.Runner {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("toolset tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	return toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
}

// call runs one tool and fails the test on an infrastructure error — the tests
// below are about what the model sees, which is the Result.
func call(t *testing.T, r toolset.Runner, name, input string) toolset.Result {
	t.Helper()
	res, err := r.Run(context.Background(), domain.NewID("sevt"), name, json.RawMessage(input))
	if err != nil {
		t.Fatalf("%s(%s): %v", name, input, err)
	}
	return res
}

// ok asserts a successful tool call and returns its content.
func ok(t *testing.T, r toolset.Runner, name, input string) string {
	t.Helper()
	res := call(t, r, name, input)
	if res.IsError {
		t.Fatalf("%s(%s) is an error result: %s", name, input, res.Content)
	}
	return res.Content
}

// fails asserts an error result whose content mentions want.
func fails(t *testing.T, r toolset.Runner, name, input, want string) string {
	t.Helper()
	res := call(t, r, name, input)
	if !res.IsError {
		t.Fatalf("%s(%s) succeeded, want an error result: %s", name, input, res.Content)
	}
	if !strings.Contains(res.Content, want) {
		t.Fatalf("%s(%s) error = %q, want it to mention %q", name, input, res.Content, want)
	}
	return res.Content
}

func TestBash(t *testing.T) {
	r := runner(t)

	t.Run("runs a command and captures stdout", func(t *testing.T) {
		got := ok(t, r, "bash", `{"command":"echo hello"}`)
		if strings.TrimSpace(got) != "hello" {
			t.Fatalf("content = %q, want hello", got)
		}
	})

	t.Run("captures stderr", func(t *testing.T) {
		got := ok(t, r, "bash", `{"command":"echo oops >&2"}`)
		if !strings.Contains(got, "oops") {
			t.Fatalf("content = %q, want it to carry stderr", got)
		}
	})

	t.Run("state persists across calls", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp && export MARKER=carried"}`)
		if got := ok(t, r, "bash", `{"command":"pwd; echo $MARKER"}`); !strings.Contains(got, "/tmp") ||
			!strings.Contains(got, "carried") {
			t.Fatalf("content = %q, want the shell's cwd and export to have carried", got)
		}
	})

	t.Run("restart resets the shell", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp"}`)
		if got := ok(t, r, "bash", `{"restart":true}`); !strings.Contains(got, "restarted") {
			t.Fatalf("restart content = %q, want it to report the restart", got)
		}
		if got := ok(t, r, "bash", `{"command":"pwd"}`); !strings.Contains(got, "/workspace") {
			t.Fatalf("after restart pwd = %q, want the workdir", got)
		}
	})

	t.Run("restart with a command resets and then runs it", func(t *testing.T) {
		r := r
		r.Session = domain.NewID("sesn")
		ok(t, r, "bash", `{"command":"cd /tmp"}`)
		if got := ok(t, r, "bash", `{"restart":true,"command":"pwd"}`); !strings.Contains(got, "/workspace") {
			t.Fatalf("content = %q, want the command to run in the reset shell", got)
		}
	})

	t.Run("a nonzero exit is an error result carrying the code", func(t *testing.T) {
		got := fails(t, r, "bash", `{"command":"echo partial; exit 3"}`, "exit code: 3")
		if !strings.Contains(got, "partial") {
			t.Fatalf("content = %q, want the output the command did produce", got)
		}
	})

	t.Run("a timeout is an error result and does not report an exit code", func(t *testing.T) {
		got := fails(t, r, "bash", `{"command":"sleep 30","timeout_ms":500}`, "timed out")
		if strings.Contains(got, "exit code") {
			t.Fatalf("content = %q, want no exit code on a timeout (TimedOut is the authoritative field)", got)
		}
	})

	t.Run("a command is required", func(t *testing.T) {
		fails(t, r, "bash", `{}`, "command is required")
	})

	t.Run("malformed input is an error result", func(t *testing.T) {
		fails(t, r, "bash", `{"command":42}`, "invalid bash input")
	})
}

func TestReadWriteEdit(t *testing.T) {
	r := runner(t)

	t.Run("write creates parent directories and reports the byte count", func(t *testing.T) {
		got := ok(t, r, "write", `{"file_path":"rw/deep/a.txt","content":"one\ntwo\nthree\n"}`)
		if got != "wrote 14 bytes to rw/deep/a.txt" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("read returns the file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt"}`); got != "one\ntwo\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("an absolute path inside the workdir reads the same file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"/workspace/rw/deep/a.txt"}`); got != "one\ntwo\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("view_range slices 1-indexed inclusive lines", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2,3]}`); got != "two\nthree" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a view_range end of 0 means to end of file", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2,0]}`); got != "two\nthree\n" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a start line past the end reads empty", func(t *testing.T) {
		if got := ok(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[99,100]}`); got != "" {
			t.Fatalf("content = %q, want empty", got)
		}
	})

	t.Run("an inverted view_range is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[3,1]}`, "before start line")
	})

	t.Run("a malformed view_range is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/deep/a.txt","view_range":[2]}`, "view_range")
	})

	t.Run("a missing file is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/nope.txt"}`, "no such file")
	})

	t.Run("a directory is an error result", func(t *testing.T) {
		fails(t, r, "read", `{"file_path":"rw/deep"}`, "not a regular file")
	})

	// A non-regular file (a FIFO) is the model's path mistake, not the sandbox
	// failing — it is a tool error it can recover from, never a backend fault.
	t.Run("a non-regular file is an error result", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkfifo /workspace/rw/fifo"}`)
		fails(t, r, "read", `{"file_path":"rw/fifo"}`, "not a regular file")
		fails(t, r, "edit", `{"file_path":"rw/fifo","old_string":"a","new_string":"b"}`, "not a regular file")
	})

	t.Run("file_path is required", func(t *testing.T) {
		fails(t, r, "read", `{}`, "file_path is required")
		fails(t, r, "write", `{"content":"x"}`, "file_path is required")
		fails(t, r, "edit", `{"old_string":"a","new_string":"b"}`, "file_path is required")
	})

	t.Run("edit replaces a unique occurrence", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/e.txt","content":"alpha beta gamma\n"}`)
		if got := ok(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"beta","new_string":"BETA"}`); got !=
			"edited rw/e.txt (1 replacement(s))" {
			t.Fatalf("content = %q", got)
		}
		if got := ok(t, r, "read", `{"file_path":"rw/e.txt"}`); got != "alpha BETA gamma\n" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("edit requires a unique match unless replace_all", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/m.txt","content":"x x x\n"}`)
		fails(t, r, "edit", `{"file_path":"rw/m.txt","old_string":"x","new_string":"y"}`, "must be unique")
		if got := ok(t, r, "edit",
			`{"file_path":"rw/m.txt","old_string":"x","new_string":"y","replace_all":true}`); got !=
			"edited rw/m.txt (3 replacement(s))" {
			t.Fatalf("content = %q", got)
		}
		if got := ok(t, r, "read", `{"file_path":"rw/m.txt"}`); got != "y y y\n" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("edit reports an old_string it cannot find", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"absent","new_string":"x"}`, "not found")
	})

	t.Run("edit requires a non-empty old_string", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/e.txt","old_string":"","new_string":"x"}`, "old_string is required")
	})

	t.Run("edit of a missing file is an error result", func(t *testing.T) {
		fails(t, r, "edit", `{"file_path":"rw/nope.txt","old_string":"a","new_string":"b"}`, "no such file")
	})

	t.Run("write overwrites", func(t *testing.T) {
		ok(t, r, "write", `{"file_path":"rw/o.txt","content":"first"}`)
		ok(t, r, "write", `{"file_path":"rw/o.txt","content":"second"}`)
		if got := ok(t, r, "read", `{"file_path":"rw/o.txt"}`); got != "second" {
			t.Fatalf("file = %q", got)
		}
	})

	t.Run("bash sees what the file tools wrote", func(t *testing.T) {
		if got := ok(t, r, "bash", `{"command":"cat /workspace/rw/o.txt"}`); !strings.Contains(got, "second") {
			t.Fatalf("bash saw %q", got)
		}
	})
}

func TestGlob(t *testing.T) {
	r := runner(t)
	// Distinct, ascending mtimes: newest-first ordering is part of the contract,
	// and stat's nanosecond precision would otherwise tie same-second writes.
	ok(t, r, "bash", `{"command":"mkdir -p g/sub && `+
		`touch -d '2020-01-01' g/old.go && touch -d '2021-01-01' g/sub/mid.go && `+
		`touch -d '2022-01-01' g/new.go && touch -d '2023-01-01' g/note.txt && `+
		`touch -d '2024-01-01' g/.hidden.go"}`)

	t.Run("doublestar matches at any depth, newest first", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"g/**/*.go"}`)
		want := "/workspace/g/.hidden.go\n/workspace/g/new.go\n/workspace/g/sub/mid.go\n/workspace/g/old.go"
		if got != want {
			t.Fatalf("content =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("a single star does not cross a directory separator", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"g/*.go"}`)
		if strings.Contains(got, "sub/mid.go") {
			t.Fatalf("content = %q, want no nested match", got)
		}
		if !strings.Contains(got, "g/new.go") {
			t.Fatalf("content = %q, want the top-level match", got)
		}
	})

	t.Run("path scopes the search root", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"**/*.go","path":"g/sub"}`)
		if got != "/workspace/g/sub/mid.go" {
			t.Fatalf("content = %q", got)
		}
	})

	// An absolute pattern names its own root, so a path argument — even one that
	// does not exist — is irrelevant to it (the reference sets root to "/").
	t.Run("an absolute pattern ignores the path root", func(t *testing.T) {
		got := ok(t, r, "glob", `{"pattern":"/workspace/g/*.go","path":"does-not-exist"}`)
		if !strings.Contains(got, "/workspace/g/new.go") {
			t.Fatalf("content = %q, want the absolute pattern's matches", got)
		}
	})

	// stat's %n echoes a filename raw, newlines included; the whole pipeline is
	// NUL-delimited so a name carrying a fake stat record stays one record. The
	// matched file comes back with its newline intact, and the second line that
	// looks like a stat record ("<mtime> FAKE") is never split off into its own
	// path — the mark of the bug this pins.
	t.Run("a newline in a matched name cannot fabricate a path", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p g4 && touch $'g4/real\n9999999999 FAKE'"}`)
		got := ok(t, r, "glob", `{"pattern":"g4/*"}`)
		if got != "/workspace/g4/real\n9999999999 FAKE" {
			t.Fatalf("content = %q, want the one real path with its newline intact", got)
		}
	})

	t.Run("no matches is not an error", func(t *testing.T) {
		if got := ok(t, r, "glob", `{"pattern":"g/**/*.rs"}`); got != "no matches" {
			t.Fatalf("content = %q", got)
		}
	})

	// The pattern is one word, whatever it contains: the script empties IFS, so
	// a space in it is a character in a filename and not a field separator.
	t.Run("a pattern may contain a space", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p 'g3' && touch 'g3/two words.go'"}`)
		if got := ok(t, r, "glob", `{"pattern":"g3/two w*.go"}`); got != "/workspace/g3/two words.go" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a missing search root is an error result", func(t *testing.T) {
		fails(t, r, "glob", `{"pattern":"*","path":"g/absent"}`, "no such")
	})

	t.Run("pattern is required", func(t *testing.T) {
		fails(t, r, "glob", `{}`, "pattern is required")
	})

	// The pattern reaches bash as the value of a variable, and bash does not
	// rescan an expansion's result for command substitution — it only globs it.
	// This pins that: the payload matches no file and is never run.
	t.Run("a pattern with shell metacharacters is data, not code", func(t *testing.T) {
		if got := ok(t, r, "glob", `{"pattern":"$(touch /tmp/pwned)/*.go"}`); got != "no matches" {
			t.Fatalf("content = %q, want no matches", got)
		}
		if out := ok(t, r, "bash", `{"command":"test -e /tmp/pwned && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the glob pattern was executed: %q", out)
		}
	})

	// A file whose own name carries a metacharacter is still just a file.
	t.Run("a metacharacter in a matched name is not re-expanded", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p g2 && touch 'g2/$(touch evil).go'"}`)
		if got := ok(t, r, "glob", `{"pattern":"g2/*.go"}`); !strings.Contains(got, "$(touch evil).go") {
			t.Fatalf("content = %q, want the literally-named file", got)
		}
		if out := ok(t, r, "bash", `{"command":"test -e evil && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the matched name was executed: %q", out)
		}
	})
}

func TestGrep(t *testing.T) {
	r := runner(t)
	ok(t, r, "write", `{"file_path":"gr/a.txt","content":"alpha\nneedle 42\nomega\n"}`)
	ok(t, r, "write", `{"file_path":"gr/b.txt","content":"nothing here\n"}`)

	t.Run("matches carry path, line number and text", func(t *testing.T) {
		got := ok(t, r, "grep", `{"pattern":"needle","path":"gr"}`)
		if got != "/workspace/gr/a.txt:2:needle 42" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("a perl character class works", func(t *testing.T) {
		got := ok(t, r, "grep", `{"pattern":"needle \\d+","path":"gr"}`)
		if !strings.Contains(got, "needle 42") {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("no matches is not an error", func(t *testing.T) {
		if got := ok(t, r, "grep", `{"pattern":"absent","path":"gr"}`); got != "no matches" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("an invalid regex is an error result", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"[unclosed","path":"gr"}`, "grep")
	})

	t.Run("a missing search root is an error result", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"x","path":"gr/absent"}`, "grep")
	})

	t.Run("pattern is required", func(t *testing.T) {
		fails(t, r, "grep", `{}`, "pattern is required")
	})

	t.Run("binary files and vendored trees are skipped", func(t *testing.T) {
		ok(t, r, "bash", `{"command":"mkdir -p gr/node_modules && printf 'needle\\0bin' > gr/bin.dat && `+
			`echo needle > gr/node_modules/dep.txt"}`)
		got := ok(t, r, "grep", `{"pattern":"needle","path":"gr"}`)
		if strings.Contains(got, "bin.dat") || strings.Contains(got, "node_modules") {
			t.Fatalf("content = %q, want binary and node_modules skipped", got)
		}
	})

	t.Run("a pattern with shell metacharacters is data, not code", func(t *testing.T) {
		ok(t, r, "grep", `{"pattern":"$(touch /tmp/pwned2)","path":"gr"}`)
		if out := ok(t, r, "bash", `{"command":"test -e /tmp/pwned2 && echo INJECTED || echo clean"}`); !strings.Contains(out, "clean") {
			t.Fatalf("the grep pattern was executed: %q", out)
		}
	})

	t.Run("output is capped", func(t *testing.T) {
		ok(t, r, "bash", fmt.Sprintf(`{"command":"mkdir -p big && for i in $(seq 1 %d); do echo needle-line-with-some-padding-$i; done > big/f.txt"}`, 20000))
		got := ok(t, r, "grep", `{"pattern":"needle","path":"big"}`)
		if len(got) > toolset.MaxOutputBytes+len("\n[output truncated]") {
			t.Fatalf("content is %d bytes, want it capped at %d", len(got), toolset.MaxOutputBytes)
		}
		if !strings.HasSuffix(got, "[output truncated]") {
			t.Fatalf("content does not report the truncation: %q", got[max(0, len(got)-80):])
		}
	})
}

func TestUnknownTool(t *testing.T) {
	r := runner(t)
	fails(t, r, "web_search", `{"query":"x"}`, "unknown tool")
}
