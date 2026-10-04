package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/sandboxtest"
)

// Cut reads what a framed script printed on one stream — what lies between
// the last begin line and the end line after it — whatever came before or
// after: an image's banner, ending its line or not, a begin line it forged
// from the exec's argv, an EXIT trap's words. A stream with no begin line, or
// a whole one with no end line, is not one the script printed to its end. A
// stream the cap cut before its end line is short, keeping what came before
// the cut less any start of the end line; one cut at least 8 digits into the
// end line's nonce, or only after the end line, lost nothing of the script's —
// fewer digits than that could be the script's own output ending as the end
// line begins by chance, one digit in sixteen.
func TestFrameCutReadsOnlyWhatTheScriptPrinted(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, end := f.Lines()
	constant := end[:len(end)-17] // "\nmap-test-end-": before the nonce
	nonce := end[len(constant) : len(end)-1]
	for _, tc := range []struct {
		name          string
		s             string
		truncated     bool
		text          string
		framed, short bool
	}{
		{"whole", begin + "a\nb\n" + end, false, "a\nb\n", true, false},
		{"banner and trap around it", "welcome to the image " + begin + "a\n" + end + "exit banner ", false, "a\n", true, false},
		{"a begin line with no newline before it", begin[1:] + "a" + end, false, "a", true, false},
		{"a forged begin line before the script's", "x" + begin + "forged\n" + begin + "a\n" + end, false, "a\n", true, false},
		{"an answer that is one empty line", begin + "\n" + end, false, "\n", true, false},
		{"an answer of nothing", begin + end, false, "", true, false},
		{"the first end line after the last begin", begin + "a" + end + "b" + end, false, "a", true, false},
		{"no begin line", "banner banner", false, "", false, false},
		{"no begin line, the cap cut", "banner banner", true, "", false, false},
		{"an end line with no begin before it", end + "a", false, "", false, false},
		{"whole, with no end line", begin + "a\n", false, "", false, false},
		{"whole, with half an end line", begin + "a\n" + end[:len(end)/2], false, "", false, false},
		{"cut before its end line", begin + "a\nb", true, "a\nb", true, true},
		{"cut inside the end line's constant part", begin + "a\n" + constant[:5], true, "a\n", true, true},
		{"cut on the end line's newline alone", begin + "a\n" + "\n", true, "a\n", true, true},
		{"cut at the end line's last constant byte", begin + "a\n" + constant, true, "a\n", true, true},
		{"cut 12 digits into the end line's nonce", begin + "a\n" + end[:len(end)-5], true, "a\n", true, false},
		{"cut 8 digits into the end line's nonce", begin + "a\n" + constant + nonce[:8], true, "a\n", true, false},
		{"cut 7 digits into the end line's nonce", begin + "a\n" + constant + nonce[:7], true, "a\n", true, true},
		// Output of the script's own that happens to end in the end line's
		// constant part and the nonce's first digit, the cap cutting there,
		// is not taken for the end line.
		{"cut one digit into what looks like the end line", begin + "a\n" + constant + nonce[:1], true, "a\n", true, true},
		{"cut before the end line's last newline", begin + "a\n" + end[:len(end)-1], true, "a\n", true, false},
		{"cut only after the end line", begin + "a\n" + end + "trap flood", true, "a\n", true, false},
		{"cut right after the begin line", begin, true, "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, framed, short := f.Cut(tc.s, tc.truncated)
			if text != tc.text || framed != tc.framed || short != tc.short {
				t.Errorf("Cut(%q, %v) = %q, %v, %v; want %q, %v, %v", tc.s, tc.truncated, text, framed, short, tc.text, tc.framed, tc.short)
			}
			b, bFramed, bShort := f.CutBytes([]byte(tc.s), tc.truncated)
			if string(b) != tc.text || bFramed != tc.framed || bShort != tc.short {
				t.Errorf("CutBytes(%q, %v) = %q, %v, %v; want Cut's %q, %v, %v", tc.s, tc.truncated, b, bFramed, bShort, tc.text, tc.framed, tc.short)
			}
			// A window that covers each case's output around the frame
			// answers as the whole-stream search does.
			for _, room := range []int{1 << 10} {
				w, wFramed, wShort := f.CutBytesWithin([]byte(tc.s), tc.truncated, room)
				if string(w) != tc.text || wFramed != tc.framed || wShort != tc.short {
					t.Errorf("CutBytesWithin(%q, %v, %d) = %q, %v, %v; want Cut's %q, %v, %v", tc.s, tc.truncated, room, w, wFramed, wShort, tc.text, tc.framed, tc.short)
				}
			}
		})
	}
	// Another run's frame is not this one's.
	other := sandbox.NewFrame("test")
	if _, framed, _ := other.Cut(begin+"a\n"+end, false); framed {
		t.Error("a frame read another run's lines as its own")
	}
	// CutBytes slices the stream it is given rather than copying it.
	s := []byte(begin + "file bytes" + end)
	b, _, _ := f.CutBytes(s, false)
	if len(b) == 0 || &b[0] != &s[len(begin)] {
		t.Error("CutBytes copied the stream")
	}
}

// CutBytesWithin looks for the begin line only in the stream's first room
// bytes and for the end line in its last room bytes, so a file between them
// is not scanned — and a file that holds the frame's own lines by chance is
// read as the file it is, where the whole-stream search would cut it there.
// Output around the frame past the room — a startup's banner longer than it,
// a trap's flood — sends the search over the whole stream, as CutBytes does.
func TestFrameCutBytesWithinLooksNearTheEnds(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, end := f.Lines()
	const room = 64
	file := strings.Repeat("x", 4*room) + begin + "middle" + end + strings.Repeat("y", 4*room)
	for _, tc := range []struct {
		name, s, text string
		truncated     bool
		framed, short bool
	}{
		{"a file holding the frame's lines", "banner" + begin + file + end + "trap", file, false, true, false},
		{"a banner past the room", strings.Repeat("b", 2*room) + begin + "f" + end, "f", false, true, false},
		{"a trap past the room", begin + "f" + end + strings.Repeat("t", 2*room), "f", false, true, false},
		{"a trap flood past the room the cap cut", begin + "f" + end + strings.Repeat("t", 2*room), "f", true, true, false},
		{"no end line, the cap cut", begin + strings.Repeat("f", 4*room), strings.Repeat("f", 4*room), true, true, true},
		{"no begin line", strings.Repeat("b", 4*room), "", false, false, false},
	} {
		got, framed, short := f.CutBytesWithin([]byte(tc.s), tc.truncated, room)
		if string(got) != tc.text || framed != tc.framed || short != tc.short {
			t.Errorf("%s: CutBytesWithin = %d bytes, %v, %v; want %d bytes, %v, %v", tc.name, len(got), framed, short, len(tc.text), tc.framed, tc.short)
		}
	}
	// The whole-stream search cuts that file at the lines it holds.
	if got, _, _ := f.CutBytes([]byte("banner"+begin+file+end+"trap"), false); string(got) == file {
		t.Error("CutBytes read a file holding the frame's lines whole; the test no longer shows what the window buys")
	}
}

// CutInBegin answers whether a stream ends partway into the begin line — the
// stream lost while the line was on its way, after whatever banner came first —
// and only that: a stream whose begin line arrived whole, or that ends on
// anything else — a newline alone, the end of any line, among them — does not.
func TestFrameCutInBegin(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, _ := f.Lines()
	for s, want := range map[string]bool{
		"":                          false,
		"banner":                    false,
		"banner\n":                  false,
		"\n":                        false,
		"banner" + begin[:1]:        false,
		"banner" + begin[:2]:        true,
		"banner" + begin[:10]:       true,
		begin[:len(begin)-1]:        true,
		"banner" + begin:            false,
		"banner" + begin + "K 0":    false,
		"banner" + begin[:10] + "x": false,
		// Another frame's begin line, cut short.
		"\nmap-test-begin-0123456789abcd": false,
	} {
		if got := f.CutInBegin(s); got != want {
			t.Errorf("CutInBegin(%q) = %v, want %v", s, got, want)
		}
	}
}

// FrameOf rebuilds the frame a label and a nonce name, which is the frame
// NewFrame made with them; Lines spells its two lines as a stream carries
// them. A label or a nonce NewFrame would not have made is refused.
func TestFrameOfRebuildsAFrame(t *testing.T) {
	f := sandbox.NewFrame("probe")
	begin, end := f.Lines()
	nonce := strings.TrimSuffix(strings.TrimPrefix(begin, "\nmap-probe-begin-"), "\n")
	if g := sandbox.FrameOf("probe", nonce); g != f {
		t.Errorf("FrameOf(probe, %s) = %v, want %v", nonce, g, f)
	}
	if end != "\nmap-probe-end-"+nonce+"\n" {
		t.Errorf("Lines = %q, %q", begin, end)
	}
	for _, c := range [][2]string{{"Probe", nonce}, {"probe", nonce[:15]}, {"probe", strings.ToUpper(nonce) + "x"}, {"probe", "0123456789abcdeg"}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("FrameOf(%q, %q) made a frame", c[0], c[1])
				}
			}()
			sandbox.FrameOf(c[0], c[1])
		}()
	}
}

// Unframe cuts both streams of a result, each by its own flag: what the cap
// took of the script's output is said per stream, a stderr whose begin line
// the cap took — a startup flood filled it first — has lost all the script
// printed there, and ok is stdout's frame alone. A result whose stdout is not
// framed comes back as it was.
func TestFrameUnframeCutsEachStreamByItsOwnFlag(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, end := f.Lines()
	for _, tc := range []struct {
		name string
		in   sandbox.ExecResult
		want sandbox.ExecResult
		ok   bool
	}{
		{"whole", sandbox.ExecResult{Stdout: "b" + begin + "out" + end + "t", Stderr: "e" + begin + "err" + end + "t", ExitCode: 3},
			sandbox.ExecResult{Stdout: "out", Stderr: "err", ExitCode: 3}, true},
		{"floods after the end lines", sandbox.ExecResult{Stdout: begin + "out" + end + "x", Stderr: begin + end + "y",
			StdoutTruncated: true, StderrTruncated: true}, sandbox.ExecResult{Stdout: "out"}, true},
		{"stdout cut short", sandbox.ExecResult{Stdout: begin + "ou", Stderr: begin + end, StdoutTruncated: true},
			sandbox.ExecResult{Stdout: "ou", StdoutTruncated: true}, true},
		{"stderr cut short", sandbox.ExecResult{Stdout: begin + end, Stderr: begin + "er", StderrTruncated: true},
			sandbox.ExecResult{Stderr: "er", StderrTruncated: true}, true},
		{"stderr flooded before its begin line", sandbox.ExecResult{Stdout: begin + "out" + end, Stderr: "flood", StderrTruncated: true},
			sandbox.ExecResult{Stdout: "out", StderrTruncated: true}, true},
		{"stderr with no frame, whole", sandbox.ExecResult{Stdout: begin + "out" + end, Stderr: "banner"},
			sandbox.ExecResult{Stdout: "out"}, true},
		// Unframed, the result is Exec's own, for the caller to say what the
		// sandbox printed instead.
		{"no begin line on stdout", sandbox.ExecResult{Stdout: "banner", Stderr: "e" + begin + "err" + end, ExitCode: 127, TimedOut: true},
			sandbox.ExecResult{Stdout: "banner", Stderr: "e" + begin + "err" + end, ExitCode: 127, TimedOut: true}, false},
		{"whole stdout with no end line", sandbox.ExecResult{Stdout: begin + "out", StderrTruncated: true},
			sandbox.ExecResult{Stdout: begin + "out", StderrTruncated: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := f.Unframe(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("Unframe(%+v) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// run runs a framed command under a shell the way a sandbox's exec does —
// `<shell> -c <command> <label> <args…>` — with a BASH_ENV file that prints,
// moves and traps (sandboxtest.BannerHook), and the result as Exec would
// report it.
func run(t *testing.T, shell, command, stdin string, args ...string) sandbox.ExecResult {
	t.Helper()
	return runHooked(t, sandboxtest.BannerHook, shell, command, stdin, args...)
}

// runHooked is run with hook as the BASH_ENV file.
func runHooked(t *testing.T, hook, shell, command, stdin string, args ...string) sandbox.ExecResult {
	t.Helper()
	dir := t.TempDir()
	hookFile := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(hookFile, []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, append([]string{"-c", command, "map-test"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BASH_ENV="+hookFile)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	code := 0
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", shell, err)
	}
	return sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code}
}

// A wrapped script, run by a bash whose startup file prints on both streams
// without ending its lines, leaves the directory and sets an EXIT trap that
// prints after it, answers exactly what the script printed, on each stream,
// with the script's own exit status — however the script exits: falling off
// its end, an `exit`, an `exit` from inside one of its functions, an `exec`,
// which replaces the subshell the script runs in and not the shell that
// closes the frame. It gets its
// positional parameters and its stdin, and it does not depend on the
// directory the exec started in. The banner is still there around the frame,
// and the trap still runs, once, after it: the frame keeps the startup's
// output out, not the startup.
func TestFrameWrapAnswersThroughAStartupFile(t *testing.T) {
	for _, tc := range []struct {
		name, script, stdin string
		args                []string
		stdout, stderr      string
		code                int
	}{
		{"falls off its end", "printf 'a b\\n'; printf 'oops' >&2", "", nil, "a b\n", "oops", 0},
		{"exits", "echo one; exit 3; echo two", "", nil, "one\n", "", 3},
		{"exits from a function", "f() { echo in; exit 4; }\nf\necho after", "", nil, "in\n", "", 4},
		{"reads its positional parameters", `printf '%s|' "$0" "$1" "$2"`, "", []string{"x y", "z"}, "map-test|x y|z|", "", 0},
		{"reads its stdin", "cat; echo", "piped", nil, "piped\n", "", 0},
		{"ends on a comment", "echo ok # no newline after", "", nil, "ok\n", "", 0},
		{"prints NULs and no newline", `printf 'a\0b\0'`, "", nil, "a\x00b\x00", "", 0},
		{"fails a command", "false", "", nil, "", "", 1},
		{"execs", "echo before; exec printf 'after'", "", nil, "before\nafter", "", 0},
		{"execs a command that fails", "exec sh -c 'echo gone; exit 5'", "", nil, "gone\n", "", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sandbox.NewFrame("test")
			raw := run(t, "/bin/bash", f.Wrap(tc.script), tc.stdin, tc.args...)
			res, ok := f.Unframe(raw)
			if !ok || res.Stdout != tc.stdout || res.Stderr != tc.stderr || res.ExitCode != tc.code || res.Truncated() {
				t.Errorf("Unframe = %+v, %v; want stdout %q, stderr %q, exit %d\nraw: %+v", res, ok, tc.stdout, tc.stderr, tc.code, raw)
			}
			if !strings.HasPrefix(raw.Stdout, "welcome to the image ") || !strings.HasSuffix(raw.Stdout, "exit banner ") ||
				!strings.HasPrefix(raw.Stderr, "stderr banner ") || !strings.HasSuffix(raw.Stderr, "exit stderr ") ||
				strings.Count(raw.Stdout, "exit banner") != 1 {
				t.Errorf("raw = %+v; want the startup file's banner before the frame and its trap's, once, after it", raw)
			}
		})
	}
	// The startup file's `cd /` reaches the script, which is why a framed
	// script names every path it needs whole.
	f := sandbox.NewFrame("test")
	if res, _ := f.Unframe(run(t, "/bin/bash", f.Wrap("pwd"), "")); res.Stdout != "/\n" {
		t.Errorf("pwd = %q, want the startup file's /", res.Stdout)
	}
}

// An image's startup file can turn errexit on, which the script inherits:
// the frame still closes — and with the script's own status — whether the
// script fails a command along the way and goes on, as it was written to, or
// ends on one, under bash and POSIX sh alike. A script that opens its own
// frame (Open) and exits through close_frame runs past a failing command as
// well. sh reads no BASH_ENV, so its errexit is turned on ahead of the
// command, as it would be for a shell an image left that way.
func TestFrameClosesUnderErrexit(t *testing.T) {
	const errexit = "set -e\n"
	for _, tc := range []struct {
		name, script string
		stdout       string
		code         int
		open         bool
	}{
		{"fails a command and goes on", "false\necho after", "after\n", 0, false},
		{"ends on a failing command", "echo before\nfalse", "before\n", 1, false},
		{"exits non-zero", "echo before\nexit 4", "before\n", 4, false},
		{"opens its own frame and exits through it", "false\necho after\nclose_frame 6", "after\n", 6, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, shell := range []string{"/bin/bash", "/bin/sh"} {
				f := sandbox.NewFrame("test")
				cmd := f.Wrap(tc.script)
				if tc.open {
					cmd = f.Open() + tc.script
				}
				var raw sandbox.ExecResult
				if shell == "/bin/bash" {
					raw = runHooked(t, errexit+sandboxtest.BannerHook, shell, cmd, "")
				} else {
					raw = run(t, shell, errexit+cmd, "")
				}
				if res, ok := f.Unframe(raw); !ok || res.Stdout != tc.stdout || res.ExitCode != tc.code {
					t.Errorf("%s: Unframe = %+v, %v; want stdout %q, exit %d\nraw: %+v", shell, res, ok, tc.stdout, tc.code, raw)
				}
			}
		})
	}
}

// Open and Wrap are POSIX shell, so a script `sh -c` runs is framed the same
// way. sh reads no BASH_ENV, so nothing prints around it here.
func TestFrameWrapIsPOSIXShell(t *testing.T) {
	f := sandbox.NewFrame("test")
	raw := run(t, "/bin/sh", f.Wrap(`if [ -e "$1" ]; then echo P; exit 0; fi; echo M`), "", "/")
	if res, ok := f.Unframe(raw); !ok || res.Stdout != "P\n" || res.ExitCode != 0 {
		t.Errorf("sh: Unframe = %+v, %v; want P", res, ok)
	}
}

// sandboxtest.Unwrap reads back the script and frame Wrap put in a command,
// so a fake sandbox answers the script and frames its answer
// (sandboxtest.Framed) as a run of it would print it. Anything else — a bare script, one only opened, one cut
// short — is no wrapped command.
func TestFrameUnwrapAndFramedRoundTrip(t *testing.T) {
	f := sandbox.NewFrame("memsync")
	const script = "[ -d '/mnt/m' ] || exit 0\nfind . -print0"
	got, g, ok := sandboxtest.Unwrap(f.Wrap(script))
	if !ok || got != script || g != f {
		t.Fatalf("Unwrap = %q, %v, %v; want the script and the frame back", got, g, ok)
	}
	for _, want := range []sandbox.ExecResult{
		{Stdout: "out", Stderr: "err", ExitCode: 2},
		// A stream marked truncated was cut inside the script's output, and
		// comes back cut short.
		{Stdout: "ou", StdoutTruncated: true},
		{Stdout: "out", Stderr: "er", StderrTruncated: true},
	} {
		if back, ok := f.Unframe(sandboxtest.Framed(g, want)); !ok || back != want {
			t.Errorf("Unframe(Framed(%+v)) = %+v, %v", want, back, ok)
		}
	}
	for _, cmd := range []string{script, f.Open() + script, f.Wrap(script)[:len(f.Wrap(script))-3], "x" + f.Wrap(script)} {
		if _, _, ok := sandboxtest.Unwrap(cmd); ok {
			t.Errorf("Unwrap(%q) read a wrapped command", cmd)
		}
	}
}

// Every frame carries a nonce of its own, 64 bits from crypto/rand: output
// that holds one run's begin or end line cannot frame the next, which a
// constant, a counter or a clock would let it — the first two because a later
// run repeats an earlier one's frame or most of it, a clock because its high
// digits do not move between calls. The label is lowercase letters and digits,
// so a command's frame reads back unambiguously.
func TestNewFrameNoncesAreRandomAndLabelsPlain(t *testing.T) {
	const frames = 64
	seen := map[string]bool{}
	var nonces []string
	for range frames {
		begin, end := sandbox.NewFrame("test").Lines()
		nonce := strings.TrimSuffix(strings.TrimPrefix(begin, "\nmap-test-begin-"), "\n")
		if end != "\nmap-test-end-"+nonce+"\n" {
			t.Fatalf("end line %q does not carry the begin line's nonce %s", end, nonce)
		}
		if seen[nonce] {
			t.Fatalf("two frames with the nonce %s", nonce)
		}
		seen[nonce] = true
		nonces = append(nonces, nonce)
	}
	for i := range 16 {
		digits := map[byte]bool{}
		for _, n := range nonces {
			digits[n[i]] = true
		}
		if len(digits) < 2 {
			t.Errorf("hex digit %d of the nonce is %q in all %d frames; it is not random", i, nonces[0][i], frames)
		}
	}
	for _, label := range []string{"", "Search", "a-b", "a b", "a'b"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewFrame(%q) took a label that is not lowercase letters and digits", label)
				}
			}()
			sandbox.NewFrame(label)
		}()
	}
}

// execOnly is a sandbox whose Exec alone is answered.
type execOnly struct {
	sandbox.Sandbox
	exec func(sandbox.ExecRequest) (sandbox.ExecResult, error)
}

func (s execOnly) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return s.exec(req)
}

// ExecFramed wraps the command under a frame of its own, keeps the rest of
// the request, and answers what the script printed — or Exec's own error as
// Exec returned it.
func TestExecFramedRunsTheScriptFramedAndAnswersItsOutput(t *testing.T) {
	var got sandbox.ExecRequest
	sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		got = req
		script, f, ok := sandboxtest.Unwrap(req.Command)
		if !ok || script != "echo hi" {
			t.Fatalf("Exec got %q; want echo hi wrapped", req.Command)
		}
		res := sandboxtest.Framed(f, sandbox.ExecResult{Stdout: "hi\n", ExitCode: 5})
		res.Stdout = "banner" + res.Stdout + "trap"
		return res, nil
	}}
	res, framed, err := sandbox.ExecFramed(context.Background(), sb, "probe", sandbox.ExecRequest{Command: "echo hi", Timeout: time.Second})
	if err != nil || !framed || res != (sandbox.ExecResult{Stdout: "hi\n", ExitCode: 5}) || got.Timeout != time.Second ||
		!strings.Contains(got.Command, "'map-probe-begin-") {
		t.Errorf("ExecFramed = %+v, %v, %v (request %+v)", res, framed, err, got)
	}
	sb.exec = func(sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: "partial"}, sandbox.ErrNotFound
	}
	if res, framed, err := sandbox.ExecFramed(context.Background(), sb, "probe", sandbox.ExecRequest{Command: "x"}); !errors.Is(err, sandbox.ErrNotFound) ||
		framed || res.Stdout != "partial" {
		t.Errorf("ExecFramed over a failed Exec = %+v, %v, %v; want Exec's own answer", res, framed, err)
	}
	sb.exec = func(sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: "banner"}, nil
	}
	if _, framed, err := sandbox.ExecFramed(context.Background(), sb, "probe", sandbox.ExecRequest{Command: "x"}); err != nil || framed {
		t.Errorf("ExecFramed over unframed output = %v, %v; want unframed and no error", framed, err)
	}
}
