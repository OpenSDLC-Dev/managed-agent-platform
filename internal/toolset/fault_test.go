package toolset_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// fakeSandbox answers with whatever the test scripted, so the fault paths a real
// daemon will not produce on demand can be pinned.
type fakeSandbox struct {
	exec     sandbox.ExecResult
	execErr  error
	files    map[string]string
	readErr  error
	writeErr error
	commands []string
	timeouts []time.Duration
	reads    []string
	writes   []string
}

func (f *fakeSandbox) ID() string { return "fake" }

func (f *fakeSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	f.commands = append(f.commands, req.Command)
	f.timeouts = append(f.timeouts, req.Timeout)
	if f.execErr != nil {
		return sandbox.ExecResult{}, f.execErr
	}
	return f.exec, nil
}

func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[path]
	if !ok {
		return nil, sandbox.ErrFileNotExist
	}
	return []byte(data), nil
}

func (f *fakeSandbox) WriteFile(_ context.Context, path string, data []byte) error {
	f.writes = append(f.writes, path)
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = string(data)
	return nil
}

func (f *fakeSandbox) WriteFileStream(ctx context.Context, path string, src io.Reader, size int64) error {
	data, err := io.ReadAll(io.LimitReader(src, size))
	if err != nil {
		return err
	}
	return f.WriteFile(ctx, path, data)
}

// WriteFiles is the batch every backend lands for a fixed couple of execs
// however many members it carries; the fake keeps the same observable semantics
// by writing the members in order and stopping at the first failure.
func (f *fakeSandbox) WriteFiles(ctx context.Context, files []sandbox.FileWrite) error {
	for _, w := range files {
		if err := f.WriteFile(ctx, w.Path, w.Data); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSandbox) Destroy(_ context.Context) error { return nil }

func run(t *testing.T, sb sandbox.Sandbox, name, input string) (toolset.Result, error) {
	t.Helper()
	r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	return r.Run(context.Background(), domain.NewID("sevt"), name, json.RawMessage(input))
}

// A backend fault is the executor's problem, not the model's: it comes back as
// an error, never as a tool result the model would try to reason about.
func TestBackendFaultIsNotAToolError(t *testing.T) {
	boom := errors.New("dial unix /var/run/docker.sock: connection refused")

	cases := []struct {
		name  string
		sb    *fakeSandbox
		tool  string
		input string
	}{
		{"bash exec", &fakeSandbox{execErr: boom}, "bash", `{"command":"echo hi"}`},
		{"glob exec", &fakeSandbox{execErr: boom}, "glob", `{"pattern":"*.go"}`},
		{"grep exec", &fakeSandbox{execErr: boom}, "grep", `{"pattern":"x"}`},
		{"read", &fakeSandbox{readErr: boom}, "read", `{"file_path":"a.txt"}`},
		{"write", &fakeSandbox{writeErr: boom}, "write", `{"file_path":"a.txt","content":"x"}`},
		{"edit read", &fakeSandbox{readErr: boom}, "edit", `{"file_path":"a.txt","old_string":"a","new_string":"b"}`},
		{"edit write", &fakeSandbox{
			files:    map[string]string{"/workspace/a.txt": "a"},
			writeErr: boom,
		}, "edit", `{"file_path":"a.txt","old_string":"a","new_string":"b"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := run(t, tc.sb, tc.tool, tc.input)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the backend fault", err)
			}
		})
	}
}

// A destroyed sandbox is a backend fault too — the session survives, this tool
// call does not, and the executor decides what happens next.
func TestDestroyedSandboxIsABackendFault(t *testing.T) {
	sb := &fakeSandbox{execErr: sandbox.ErrNotFound}
	if _, err := run(t, sb, "bash", `{"command":"echo hi"}`); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("err = %v, want sandbox.ErrNotFound", err)
	}
}

// The file sentinels are the model's to see: a missing file, a directory, a path
// blocked by a file, an oversize read are all things the model can recover from by
// trying something else, so they are tool results and not errors.
func TestFileSentinelsAreToolErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"missing", sandbox.ErrFileNotExist, "no such file"},
		{"directory", sandbox.ErrIsDirectory, "not a regular file"},
		{"non-regular", sandbox.ErrNotRegularFile, "not a regular file"},
		{"not a directory", sandbox.ErrNotDirectory, "not a directory"},
		{"too large", sandbox.ErrFileTooLarge, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, tool := range []struct{ name, input string }{
				{"read", `{"file_path":"a.txt"}`},
				{"edit", `{"file_path":"a.txt","old_string":"a","new_string":"b"}`},
			} {
				res, err := run(t, &fakeSandbox{readErr: tc.err}, tool.name, tool.input)
				if err != nil {
					t.Fatalf("%s: err = %v, want a tool result", tool.name, err)
				}
				if !res.IsError || !strings.Contains(res.Content, tc.want) {
					t.Fatalf("%s: result = %+v, want an error result mentioning %q", tool.name, res, tc.want)
				}
			}

			// The write side answers the same way. Two of these sentinels reach the
			// toolset only from a write — a path blocked by a file, and a target that
			// is a directory — and a write left unclassified is the one the executor
			// retries into a doomed loop (#71).
			res, err := run(t, &fakeSandbox{writeErr: tc.err}, "write", `{"file_path":"a.txt","content":"x"}`)
			if err != nil {
				t.Fatalf("write: err = %v, want a tool result", err)
			}
			if !res.IsError || !strings.Contains(res.Content, tc.want) {
				t.Fatalf("write: result = %+v, want an error result mentioning %q", res, tc.want)
			}

			// And through `edit`, whose write follows a read that worked: the file it
			// found is not the file it can replace.
			sb := &fakeSandbox{files: map[string]string{sandbox.DefaultWorkdir + "/a.txt": "a"}, writeErr: tc.err}
			res, err = run(t, sb, "edit", `{"file_path":"a.txt","old_string":"a","new_string":"b"}`)
			if err != nil {
				t.Fatalf("edit: err = %v, want a tool result", err)
			}
			if !res.IsError || !strings.Contains(res.Content, tc.want) {
				t.Fatalf("edit: result = %+v, want an error result mentioning %q", res, tc.want)
			}
		})
	}
}

// Relative tool paths resolve against the sandbox's workdir, and the default
// workdir is the sandbox's own.
func TestPathsResolveAgainstTheWorkdir(t *testing.T) {
	sb := &fakeSandbox{}
	r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	if _, err := r.Run(context.Background(), domain.NewID("sevt"), "write",
		json.RawMessage(`{"file_path":"sub/a.txt","content":"x"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(sb.writes) != 1 || sb.writes[0] != sandbox.DefaultWorkdir+"/sub/a.txt" {
		t.Fatalf("writes = %v, want the workdir-rooted path", sb.writes)
	}

	custom := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn"), Workdir: "/srv/app"}
	if _, err := custom.Run(context.Background(), domain.NewID("sevt"), "read",
		json.RawMessage(`{"file_path":"a.txt"}`)); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := sb.reads[len(sb.reads)-1]; got != "/srv/app/a.txt" {
		t.Fatalf("read %q, want it rooted at the configured workdir", got)
	}
}

// Every tool call carries a deadline into the sandbox: the one the model chose,
// clamped to MaxTimeout, or the package default. A model-chosen timeout is a
// lease the executor has to keep alive, so none of these may be unbounded — and
// a millisecond count big enough to overflow a Duration must not come out as an
// instant deadline.
func TestEveryCallIsDeadlined(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  time.Duration
	}{
		{"bash default", "bash", `{"command":"echo hi"}`, toolset.DefaultTimeout},
		{"bash honours timeout_ms", "bash", `{"command":"echo hi","timeout_ms":1500}`, 1500 * time.Millisecond},
		{"bash clamps a long timeout", "bash", `{"command":"echo hi","timeout_ms":86400000}`, toolset.MaxTimeout},
		{"bash clamps an overflowing timeout", "bash",
			`{"command":"echo hi","timeout_ms":9223372036854775807}`, toolset.MaxTimeout},
		{"bash ignores a negative timeout", "bash", `{"command":"echo hi","timeout_ms":-1}`, toolset.DefaultTimeout},
		{"glob", "glob", `{"pattern":"*.go"}`, toolset.DefaultTimeout},
		{"grep", "grep", `{"pattern":"x"}`, toolset.DefaultTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sb := &fakeSandbox{}
			if _, err := run(t, sb, tc.tool, tc.input); err != nil {
				t.Fatalf("%s: %v", tc.tool, err)
			}
			if len(sb.timeouts) != 1 || sb.timeouts[0] != tc.want {
				t.Fatalf("deadline = %v, want %v", sb.timeouts, tc.want)
			}
		})
	}
}

// A NUL byte in a model-supplied path or pattern is the model's malformed
// input, not the sandbox failing: it is a tool error, and it never reaches the
// sandbox (where a tar header or a truncated command would misclassify it as a
// backend fault). Pinned with a fake sandbox that would return a Go error if
// the byte got through.
func TestNULInAPathIsAToolError(t *testing.T) {
	boom := errors.New("archive/tar: header field contains a NUL byte")
	// json.Marshal escapes the NUL properly, so the JSON is valid and the tool's
	// own Unmarshal succeeds — the byte reaches badField, not the JSON decoder.
	bad := "a\x00b"
	mk := func(m map[string]any) string {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		tool, field, input string
	}{
		{"read", "file_path", mk(map[string]any{"file_path": bad})},
		{"write", "file_path", mk(map[string]any{"file_path": bad, "content": "x"})},
		{"edit", "file_path", mk(map[string]any{"file_path": bad, "old_string": "a", "new_string": "b"})},
		{"glob", "pattern", mk(map[string]any{"pattern": bad})},
		{"glob", "path", mk(map[string]any{"pattern": "*", "path": bad})},
		{"grep", "pattern", mk(map[string]any{"pattern": bad})},
		{"grep", "path", mk(map[string]any{"pattern": "x", "path": bad})},
	}
	for _, tc := range cases {
		t.Run(tc.tool+" "+tc.field, func(t *testing.T) {
			sb := &fakeSandbox{execErr: boom, writeErr: boom, readErr: boom}
			res, err := run(t, sb, tc.tool, tc.input)
			if err != nil {
				t.Fatalf("err = %v, want a tool result — the NUL must be caught before the sandbox", err)
			}
			if !res.IsError || !strings.Contains(res.Content, tc.field+" must not contain a NUL byte") {
				t.Fatalf("result = %+v, want a NUL tool error on %s", res, tc.field)
			}
			if len(sb.commands)+len(sb.writes)+len(sb.reads) != 0 {
				t.Fatalf("the bad input reached the sandbox: commands=%v writes=%v reads=%v",
					sb.commands, sb.writes, sb.reads)
			}
		})
	}
}

// An absolute glob pattern is rooted at "/", so a non-existent path argument is
// not turned into its search root (which would make it fail the directory
// check). Pinned against the rendered script rather than a container.
func TestAbsoluteGlobPatternIgnoresPath(t *testing.T) {
	sb := &fakeSandbox{}
	if _, err := run(t, sb, "glob", `{"pattern":"/etc/*.conf","path":"does-not-exist"}`); err != nil {
		t.Fatalf("glob: %v", err)
	}
	script := sb.commands[0]
	if !strings.Contains(script, "root='/'") {
		t.Fatalf("script does not root an absolute pattern at /:\n%s", script)
	}
	if strings.Contains(script, "does-not-exist") {
		t.Fatalf("the path argument leaked into an absolute pattern's script:\n%s", script)
	}
}

// A grep regex that happens to start with "/" is a regex, not an absolute root:
// it must still search the workdir, never the whole filesystem.
func TestGrepPatternStartingWithSlashSearchesTheWorkdir(t *testing.T) {
	sb := &fakeSandbox{}
	if _, err := run(t, sb, "grep", `{"pattern":"/usr/local"}`); err != nil {
		t.Fatalf("grep: %v", err)
	}
	script := sb.commands[0]
	if !strings.Contains(script, "root='"+sandbox.DefaultWorkdir+"'") {
		t.Fatalf("grep did not root at the workdir:\n%s", script)
	}
}

// A search that outran its deadline is an error result, not an empty one: "no
// matches" from a command that never finished would be a lie.
func TestSearchTimeoutIsAnErrorResult(t *testing.T) {
	for _, tool := range []struct{ name, input string }{
		{"glob", `{"pattern":"**/*.go"}`},
		{"grep", `{"pattern":"x"}`},
	} {
		sb := &fakeSandbox{exec: sandbox.ExecResult{TimedOut: true, ExitCode: 137}}
		res, err := run(t, sb, tool.name, tool.input)
		if err != nil {
			t.Fatalf("%s: %v", tool.name, err)
		}
		if !res.IsError || !strings.Contains(res.Content, "timed out") {
			t.Fatalf("%s: result = %+v, want a timeout error result", tool.name, res)
		}
	}
}

// A failed search hands back what the command itself said; a silent failure
// still names the tool and its exit code rather than reading as an empty result.
func TestSearchFailure(t *testing.T) {
	sb := &fakeSandbox{exec: sandbox.ExecResult{ExitCode: 2, Stderr: "grep: unmatched [\n"}}
	res, _ := run(t, sb, "grep", `{"pattern":"[","path":"x"}`)
	if !res.IsError || !strings.Contains(res.Content, "unmatched [") {
		t.Fatalf("result = %+v, want the command's own message", res)
	}

	silent := &fakeSandbox{exec: sandbox.ExecResult{ExitCode: 9}}
	res, _ = run(t, silent, "glob", `{"pattern":"*"}`)
	if !res.IsError || !strings.Contains(res.Content, "exit code 9") {
		t.Fatalf("result = %+v, want the exit code", res)
	}
}

// glob reports at most globLimit paths, newest first — the sort is the shell's,
// so the cut is simply the head of what it printed.
func TestGlobLimit(t *testing.T) {
	var stdout strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&stdout, "1700000000.%09d /workspace/f%d.go\x00", i, i)
	}
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: stdout.String()}}
	res, err := run(t, sb, "glob", `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	lines := strings.Split(res.Content, "\n")
	if len(lines) != 200 {
		t.Fatalf("glob returned %d paths, want 200", len(lines))
	}
	if lines[0] != "/workspace/f0.go" {
		t.Fatalf("first path = %q, want the shell's own order preserved", lines[0])
	}
}

// An absolute pattern names its own root, so it is not hung off the search root.
func TestAbsoluteGlobPattern(t *testing.T) {
	sb := &fakeSandbox{}
	if _, err := run(t, sb, "glob", `{"pattern":"/etc/*.conf"}`); err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(sb.commands[0], "prefix=''") {
		t.Fatalf("script keeps a prefix for an absolute pattern:\n%s", sb.commands[0])
	}
}

// The tool result is what goes on the event log forever, so it is capped — and
// the cut backs off to a rune boundary rather than splitting a character.
func TestOutputIsCapped(t *testing.T) {
	// 3-byte runes: the cap is not a multiple of 3, so a raw byte slice
	// would split a rune — a 2-byte fixture against the even cap never
	// could, and proved nothing. writeErr keeps this fixture on the
	// plain-truncation path; the spill preview's own cut is pinned by
	// TestOversizedOutputSpillsToTheSandbox with the same rune width.
	long := strings.Repeat("€", toolset.MaxOutputBytes/2)
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: long, ExitCode: 0}, writeErr: errors.New("no spill")}
	res, err := run(t, sb, "grep", `{"pattern":"x"}`)
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.HasSuffix(res.Content, "\n[output truncated]") {
		t.Fatalf("content does not report the truncation")
	}
	body := strings.TrimSuffix(res.Content, "\n[output truncated]")
	if len(body) > toolset.MaxOutputBytes {
		t.Fatalf("content is %d bytes, want at most %d", len(body), toolset.MaxOutputBytes)
	}
	if !utf8.ValidString(body) {
		t.Fatal("the cut split a rune")
	}
}

// bash's status trailer is the load-bearing signal — did the command fail — and
// must survive truncation of a huge output, not be lopped off the end of it.
func TestBashStatusTrailerSurvivesTruncation(t *testing.T) {
	huge := strings.Repeat("x", toolset.MaxOutputBytes+50_000)

	// Both failure arms spill before capping — the spill file must hold the
	// WHOLE output, not the capped preview, and the notice rides between the
	// truncated body and the status trailer. Explicit ids so the file's
	// content can be asserted, not just its existence.
	bash := func(t *testing.T, sb *fakeSandbox, input string) (toolset.Result, string) {
		t.Helper()
		r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
		id := domain.NewID("sevt")
		res, err := r.Run(context.Background(), id, "bash", json.RawMessage(input))
		if err != nil {
			t.Fatalf("bash: %v", err)
		}
		return res, "/tmp/tool_outputs/" + id.String() + ".txt"
	}

	t.Run("exit code", func(t *testing.T) {
		sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: huge, ExitCode: 7}}
		res, spillPath := bash(t, sb, `{"command":"big; exit 7"}`)
		if !res.IsError || !strings.HasSuffix(res.Content, "\nexit code: 7") {
			t.Fatalf("content tail = %q, want the exit code to survive", tail(res.Content))
		}
		if len(res.Content) > toolset.MaxOutputBytes {
			t.Fatalf("content is %d bytes, want it within the cap", len(res.Content))
		}
		if !strings.Contains(res.Content, "[output truncated; full output written to "+spillPath+"]") {
			t.Fatal("content does not report the truncation and the spill file")
		}
		if sb.files[spillPath] != huge {
			t.Fatalf("spill file holds %d bytes, want the full %d", len(sb.files[spillPath]), len(huge))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: huge, TimedOut: true}}
		res, spillPath := bash(t, sb, `{"command":"big","timeout_ms":500}`)
		if !res.IsError || !strings.HasSuffix(res.Content, "state changes were dropped") {
			t.Fatalf("content tail = %q, want the timeout notice to survive", tail(res.Content))
		}
		if len(res.Content) > toolset.MaxOutputBytes {
			t.Fatalf("content is %d bytes, want it within the cap", len(res.Content))
		}
		if !strings.Contains(res.Content, "[output truncated; full output written to "+spillPath+"]") {
			t.Fatal("content does not report the truncation and the spill file")
		}
		if sb.files[spillPath] != huge {
			t.Fatalf("spill file holds %d bytes, want the full %d", len(sb.files[spillPath]), len(huge))
		}
	})
}

func tail(s string) string {
	if len(s) > 60 {
		return "…" + s[len(s)-60:]
	}
	return s
}

// A sandbox-level truncation says so, and stderr follows stdout whole rather
// than being run onto the end of its last line.
func TestCombine(t *testing.T) {
	sb := &fakeSandbox{exec: sandbox.ExecResult{
		Stdout: "out", Stderr: "err", Truncated: true, ExitCode: 1,
	}}
	res, err := run(t, sb, "bash", `{"command":"x"}`)
	if err != nil {
		t.Fatalf("bash: %v", err)
	}
	want := "[output truncated]\nout\nerr\nexit code: 1"
	if res.Content != want {
		t.Fatalf("content = %q, want %q", res.Content, want)
	}
}

// Malformed input is the model's mistake, not the sandbox's fault: every tool
// reports it as an error result it can correct.
func TestMalformedInput(t *testing.T) {
	for _, tc := range []struct{ tool, input, want string }{
		{"bash", `{"command":42}`, "invalid bash input"},
		{"read", `{"file_path":42}`, "invalid read input"},
		{"write", `{"file_path":42}`, "invalid write input"},
		{"edit", `{"file_path":42}`, "invalid edit input"},
		{"glob", `{"pattern":42}`, "invalid glob input"},
		{"grep", `{"pattern":42}`, "invalid grep input"},
	} {
		res, err := run(t, &fakeSandbox{}, tc.tool, tc.input)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		if !res.IsError || !strings.Contains(res.Content, tc.want) {
			t.Fatalf("%s: result = %+v, want %q", tc.tool, res, tc.want)
		}
	}
}

// grep and glob never let a model-chosen pattern reach bash as code.
func TestSearchPatternsAreQuoted(t *testing.T) {
	sb := &fakeSandbox{}
	r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	for _, tc := range []struct{ tool, input string }{
		{"glob", `{"pattern":"'; touch /pwned; '"}`},
		{"grep", `{"pattern":"'; touch /pwned; '"}`},
	} {
		if _, err := r.Run(context.Background(), domain.NewID("sevt"), tc.tool,
			json.RawMessage(tc.input)); err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		script := sb.commands[len(sb.commands)-1]
		if strings.Contains(script, "; touch /pwned; ") && !strings.Contains(script, `'\''`) {
			t.Fatalf("%s: the pattern reached the script unquoted:\n%s", tc.tool, script)
		}
	}
}

// The spill (#226, docs/plan/18_spill-oversized-output.md): an output past
// MaxOutputBytes lands whole in a sandbox file and the preview names it. The
// fixture is 3-byte runes so the cap falls mid-rune — the preview's own cut
// must back off to a boundary, a property the fallback path pins separately.
func TestOversizedOutputSpillsToTheSandbox(t *testing.T) {
	full := strings.Repeat("€", 40_000) // 120000 bytes; 102400 % 3 != 0
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: full}}
	r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	id := domain.NewID("sevt")
	res, err := r.Run(context.Background(), id, "grep", json.RawMessage(`{"pattern":"x"}`))
	if err != nil || res.IsError {
		t.Fatalf("grep: err=%v content=%s", err, tail(res.Content))
	}
	spillPath := "/tmp/tool_outputs/" + id.String() + ".txt"
	if sb.files[spillPath] != full {
		t.Errorf("spill file holds %d bytes, want the full %d", len(sb.files[spillPath]), len(full))
	}
	wantSuffix := "\n[output truncated; full output written to " + spillPath + "]"
	if !strings.HasSuffix(res.Content, wantSuffix) {
		t.Fatalf("content tail = %q, want it to end with %q", tail(res.Content), wantSuffix)
	}
	body := strings.TrimSuffix(res.Content, wantSuffix)
	if len(body) > toolset.MaxOutputBytes || !strings.HasPrefix(full, body) {
		t.Errorf("preview body is %d bytes, want the output's own head within the cap", len(body))
	}
	if !utf8.ValidString(body) {
		t.Error("the spill preview's cut split a rune")
	}
}

func TestOutputWithinTheCapDoesNotSpill(t *testing.T) {
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: "hello\n"}}
	res, err := run(t, sb, "grep", `{"pattern":"h"}`)
	if err != nil || res.Content != "hello" {
		t.Fatalf("grep: err=%v content=%q", err, res.Content)
	}
	for _, w := range sb.writes {
		if strings.HasPrefix(w, "/tmp/tool_outputs/") {
			t.Errorf("an under-cap output wrote a spill file: %s", w)
		}
	}
}

// A successful grep whose output hit the sandbox's own per-stream Exec cap
// arrives with ExecResult.Truncated set, and the marker must survive into the
// content — or the spill notice's "full output" would vouch for a result the
// sandbox itself already cut.
func TestExecTruncatedGrepCarriesTheUpstreamMarker(t *testing.T) {
	full := strings.Repeat("z", toolset.MaxOutputBytes+64)
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: full, Truncated: true}}
	res, err := run(t, sb, "grep", `{"pattern":"z"}`)
	if err != nil || res.IsError {
		t.Fatalf("grep: err=%v", err)
	}
	if !strings.HasPrefix(res.Content, "[output truncated]\n") {
		t.Errorf("content head = %.40q, want the upstream truncation marker first", res.Content)
	}
}

// read is exempt: its full content already sits in the sandbox at the very
// path the model just named, and a spilled copy would chain — every read of
// a spill file would mint another under a fresh id, with no fixed point. An
// oversized read truncates plainly; view_range and bash slicing reach the
// rest.
func TestReadNeverSpills(t *testing.T) {
	full := strings.Repeat("x", toolset.MaxOutputBytes+64)
	sb := &fakeSandbox{files: map[string]string{"/big.txt": full}}
	res, err := run(t, sb, "read", `{"file_path":"/big.txt"}`)
	if err != nil || res.IsError {
		t.Fatalf("read: err=%v", err)
	}
	if !strings.HasSuffix(res.Content, "\n[output truncated]") || strings.Contains(res.Content, "/tmp/tool_outputs/") {
		t.Errorf("content tail = %q, want plain truncation naming no spill file", tail(res.Content))
	}
	for _, w := range sb.writes {
		if strings.HasPrefix(w, "/tmp/tool_outputs/") {
			t.Errorf("read spilled a copy: %s", w)
		}
	}
}

// A failed spill write must cost the call nothing: the result caps exactly as
// it did before the spill existed — and the attempt must really have been
// made, or this fixture would also pass with the hook deleted.
func TestSpillWriteFailureFallsBackToPlainTruncation(t *testing.T) {
	full := strings.Repeat("y", toolset.MaxOutputBytes+64)
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: full}, writeErr: errors.New("disk full")}
	res, err := run(t, sb, "grep", `{"pattern":"y"}`)
	if err != nil || res.IsError {
		t.Fatalf("grep: err=%v content=%s", err, tail(res.Content))
	}
	attempted := false
	for _, w := range sb.writes {
		if strings.HasPrefix(w, "/tmp/tool_outputs/") {
			attempted = true
		}
	}
	if !attempted {
		t.Error("no spill write was attempted")
	}
	if !strings.HasSuffix(res.Content, "\n[output truncated]") {
		t.Errorf("content tail = %q, want the plain truncation trailer", tail(res.Content))
	}
	if strings.Contains(res.Content, "/tmp/tool_outputs/") {
		t.Errorf("a failed spill still named a file: %s", tail(res.Content))
	}
}
