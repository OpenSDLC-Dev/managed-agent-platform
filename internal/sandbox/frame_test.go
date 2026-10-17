package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// CutInBegin answers whether a stream ends partway into the begin line, far
// enough to reach frameNonceTrusted digits of its nonce — the stream lost
// while the line was on its way, after whatever banner came first — and only
// that: a stream whose begin line arrived whole, or that ends on anything else
// — a newline alone, the end of any line, the line's first bytes, which a
// banner can end in by itself, among them — does not.
func TestFrameCutInBegin(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, _ := f.Lines()
	// Up to the nonce, and 8 of its 16 digits.
	least := len(begin) - 1 - 16 + 8
	for s, want := range map[string]bool{
		"":                          false,
		"banner":                    false,
		"banner\n":                  false,
		"\n":                        false,
		"banner" + begin[:1]:        false,
		"banner" + begin[:2]:        false,
		"banner" + begin[:10]:       false,
		"banner" + begin[:least-1]:  false,
		"banner" + begin[:least]:    true,
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
			_, end := f.Lines()
			if !strings.HasPrefix(raw.Stdout, "welcome to the image ") || !strings.HasSuffix(raw.Stdout, end) ||
				!strings.HasPrefix(raw.Stderr, "stderr banner ") || !strings.HasSuffix(raw.Stderr, end) {
				t.Errorf("raw = %+v; want the startup file's banner before the frame, and its EXIT trap never run (close_frame clears it)", raw)
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

// An image's startup file can turn on settings that change what a glob
// matches, or its order (sandboxtest.GlobHook); turn on privileged mode, in
// which bash 5 keeps the ignore list of a GLOBIGNORE set before it, and
// dotglob with it; and set an ERR trap, which set +e leaves on. The platform's
// scripts are written for bash's defaults: the preamble puts each back,
// whatever the bash has, and says nothing as it does, so a command the script
// lets fail and a glob in it run as on any image. The glob lists the test's
// directory, which holds the startup file alone.
func TestScriptPreambleUndoesAStartupsGlobOptions(t *testing.T) {
	const probe = `false
printf '%s\n' "$-" "${GLOBIGNORE-unset}" "${GLOBSORT-unset}" *
for o in dotglob extglob failglob globstar nocaseglob nullglob; do
  if shopt -q "$o" 2>/dev/null; then echo "$o on"; fi
done
`
	for _, hook := range []string{
		sandboxtest.GlobHook,
		"GLOBIGNORE='*'\nshopt -s dotglob\nset -p\n",
		sandboxtest.GlobHook + "trap 'exit 42' ERR\n",
	} {
		res := runHooked(t, hook, "/bin/bash", sandbox.Script(probe), "")
		flags, rest, _ := strings.Cut(res.Stdout, "\n")
		if strings.ContainsAny(flags, "fp") || rest != "unset\nunset\nhook.sh\n" || res.Stderr != "" || res.ExitCode != 0 {
			t.Errorf("under %q: %+v; want no f or p in $-, GLOBIGNORE and GLOBSORT unset, the glob matching hook.sh, "+
				"every glob option off, and no stderr", hook, res)
		}
	}
}

// Open and Wrap are POSIX shell, so a script `sh -c` runs is framed the same
// way. sh reads no BASH_ENV, so nothing prints around it here. A script the
// preamble alone opens (Script) runs there too, and as the frame does not
// hide what the preamble prints, the check reads its stderr whole: the
// preamble's bash-only lines pass over without a word.
func TestFrameWrapIsPOSIXShell(t *testing.T) {
	f := sandbox.NewFrame("test")
	raw := run(t, "/bin/sh", f.Wrap(`if [ -e "$1" ]; then echo P; exit 0; fi; echo M`), "", "/")
	if res, ok := f.Unframe(raw); !ok || res.Stdout != "P\n" || res.ExitCode != 0 {
		t.Errorf("sh: Unframe = %+v, %v; want P", res, ok)
	}
	if raw := run(t, "/bin/sh", sandbox.Script("echo P"), ""); raw.Stdout != "P\n" || raw.Stderr != "" || raw.ExitCode != 0 {
		t.Errorf("sh: Script = %+v; want P, and nothing on stderr", raw)
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

// A framed script whose stdout carries no frame gave no answer, and
// Unanswered says why. The cap cut stdout before the frame: the startup's
// flood (StartupOutputError). The begin line on either stream, or a stream
// that ends partway through it, shows the script began and its output was lost
// on the way (ErrOutputLost). Neither shows the image's startup ended the shell
// first, with a status of its own choosing (StartupExitError), which a real
// bash under a startup file that exits shows last.
func TestFrameUnansweredSaysWhyNoAnswerCame(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, _ := f.Lines()
	other, _ := sandbox.NewFrame("test").Lines()
	for _, tc := range []struct {
		name string
		res  sandbox.ExecResult
		want error
	}{
		{"nothing, and a status", sandbox.ExecResult{ExitCode: 16}, sandbox.ErrUnframedStatus},
		{"a banner on each stream, and a status", sandbox.ExecResult{Stdout: "welcome", Stderr: "warning\n", ExitCode: 1}, sandbox.ErrUnframedStatus},
		{"a banner, and a zero", sandbox.ExecResult{Stdout: "welcome\n"}, sandbox.ErrUnframedStatus},
		{"another frame's begin line", sandbox.ExecResult{Stdout: other, Stderr: other, ExitCode: 15}, sandbox.ErrUnframedStatus},
		{"stdout's begin line, and no end line", sandbox.ExecResult{Stdout: "welcome" + begin + "part"}, sandbox.ErrOutputLost},
		{"stderr's begin line, and stdout lost", sandbox.ExecResult{Stderr: "warning" + begin, ExitCode: 16}, sandbox.ErrOutputLost},
		{"stdout ending inside its begin line", sandbox.ExecResult{Stdout: "welcome" + begin[:len(begin)-3]}, sandbox.ErrOutputLost},
		{"stderr ending inside its begin line", sandbox.ExecResult{Stderr: begin[:len(begin)-2], ExitCode: 1}, sandbox.ErrOutputLost},
		{"a banner ending as a begin line starts", sandbox.ExecResult{Stdout: "welcome\nm", ExitCode: 10}, sandbox.ErrUnframedStatus},
		{"the command, echoed", sandbox.ExecResult{Stderr: "blocked: " + f.Wrap("echo ran") + "\n", ExitCode: 126}, sandbox.ErrUnframedStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := f.Unframe(tc.res); ok {
				t.Fatalf("Unframe(%+v) answered", tc.res)
			}
			err := f.Unanswered("the probe of /w", tc.res)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "the probe of /w") {
				t.Errorf("Unanswered = %v, want %v naming what was asked", err, tc.want)
			}
			var exit *sandbox.StartupExitError
			if isExit := errors.As(err, &exit); isExit != (tc.want == sandbox.ErrUnframedStatus) ||
				isExit && (exit.Status != tc.res.ExitCode || exit.What != "the probe of /w" || exit.MayHaveRun) {
				t.Errorf("Unanswered = %#v, want the startup's exit with status %d only where neither stream began", err, tc.res.ExitCode)
			}
		})
	}
	var flood *sandbox.StartupOutputError
	if err := f.Unanswered("the probe of /w", sandbox.ExecResult{Stdout: "welcome" + begin[:5], StdoutTruncated: true, Stderr: begin, ExitCode: 16}); !errors.As(err, &flood) ||
		flood.What != "the probe of /w" || errors.Is(err, sandbox.ErrUnframedStatus) {
		t.Errorf("Unanswered over a stdout the cap cut = %v, want the startup's flood", err)
	}
	if err := f.Unanswered("the probe of /w", sandbox.ExecResult{Stderr: strings.Repeat("y\n", 64), StderrTruncated: true, ExitCode: 16}); !errors.As(err, &flood) ||
		errors.Is(err, sandbox.ErrUnframedStatus) {
		t.Errorf("Unanswered over a stderr the cap cut before any begin line = %v, want the startup's flood", err)
	}
	for _, status := range []int{0, 1, 16} {
		raw := runHooked(t, sandboxtest.BannerHook+"exit "+strconv.Itoa(status)+"\n", "/bin/bash", f.Wrap("echo ran"), "")
		if _, ok := f.Unframe(raw); ok {
			t.Fatalf("a startup that exits %d: the script answered: %+v", status, raw)
		}
		var exit *sandbox.StartupExitError
		if err := f.Unanswered("the probe of /w", raw); !errors.As(err, &exit) || exit.Status != status {
			t.Errorf("a startup that exits %d: Unanswered = %v over %+v, want the startup's exit", status, err, raw)
		}
	}
}

// Began is whether a stream shows the script began: its begin line, or an
// end partway through it, which was on its way when the stream was lost.
// Another frame's begin line is not this script's, and a newline alone is no
// part of one.
func TestFrameBegan(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, end := f.Lines()
	other, _ := sandbox.NewFrame("test").Lines()
	for s, want := range map[string]bool{
		"": false, "welcome\n": false, other: false, "\n": false, end: false,
		begin: true, "welcome" + begin + "part": true, begin[1:]: true, "welcome" + begin[:len(begin)-1]: true,
		// The line's first bytes, which a banner can end in by itself.
		begin[:4]: false, "welcome\nm": false,
		// The command a startup echoes spells the line's words, never the line.
		f.Wrap("echo ran"): false, "blocked: " + f.Wrap("echo ran") + "\n": false,
	} {
		if got := f.Began(s); got != want {
			t.Errorf("Began(%q) = %v, want %v", s, got, want)
		}
	}
}

// UnframeStatus answers a status where stdout carries the whole frame, as
// Unframe does, and where stdout carries none, or a frame the cap cut before
// its end line — the cap cut it, the transport lost it, a startup silenced it,
// a shell was killed after the begin line — through a stderr that carries the
// whole frame. Nowhere else: not on a stderr whose frame is missing or cut
// short.
func TestFrameUnframeStatus(t *testing.T) {
	f := sandbox.NewFrame("test")
	begin, end := f.Lines()
	flood := strings.Repeat("y\n", 64)
	for _, c := range []struct {
		name string
		res  sandbox.ExecResult
		ok   bool
		want sandbox.ExecResult
	}{
		{"stdout framed", sandbox.ExecResult{Stdout: "banner" + begin + "why" + end, Stderr: begin + "msg" + end, ExitCode: 20},
			true, sandbox.ExecResult{Stdout: "why", Stderr: "msg", ExitCode: 20}},
		{"stdout flooded, stderr framed", sandbox.ExecResult{Stdout: flood, StdoutTruncated: true, Stderr: "warn" + begin + "msg" + end + "trap", ExitCode: 16},
			true, sandbox.ExecResult{Stdout: "", StdoutTruncated: true, Stderr: "msg", ExitCode: 16}},
		{"stdout flooded, stderr bare", sandbox.ExecResult{Stdout: flood, StdoutTruncated: true, Stderr: "warn", ExitCode: 0}, false, sandbox.ExecResult{}},
		{"stdout flooded, stderr began and lost its end", sandbox.ExecResult{Stdout: flood, StdoutTruncated: true, Stderr: begin + "msg", ExitCode: 0}, false, sandbox.ExecResult{}},
		{"both flooded, stderr's end cut short", sandbox.ExecResult{Stdout: flood, StdoutTruncated: true, Stderr: begin + "msg" + end[:3], StderrTruncated: true, ExitCode: 0}, false, sandbox.ExecResult{}},
		{"stdout whole and unframed, stderr framed", sandbox.ExecResult{Stdout: "banner", Stderr: begin + "msg" + end, ExitCode: 15},
			true, sandbox.ExecResult{Stdout: "", Stderr: "msg", ExitCode: 15}},
		{"stdout lost, stderr framed", sandbox.ExecResult{Stderr: begin + end, ExitCode: 0}, true, sandbox.ExecResult{}},
		{"stdout cut only past its end line", sandbox.ExecResult{Stdout: begin + "why" + end + flood, StdoutTruncated: true, Stderr: begin + end, ExitCode: 3},
			true, sandbox.ExecResult{Stdout: "why", ExitCode: 3}},
		// A shell killed after the begin line, its status its traps' choice.
		{"stdout's frame cut short, stderr began only", sandbox.ExecResult{Stdout: flood + begin + "wh", StdoutTruncated: true, Stderr: begin, ExitCode: 0},
			false, sandbox.ExecResult{}},
		{"stdout's frame cut short, stderr framed", sandbox.ExecResult{Stdout: flood + begin + "wh", StdoutTruncated: true, Stderr: begin + "msg" + end, ExitCode: 0},
			true, sandbox.ExecResult{Stdout: "wh", StdoutTruncated: true, Stderr: "msg", ExitCode: 0}},
	} {
		got, ok := f.UnframeStatus(c.res)
		if ok != c.ok || (ok && got != c.want) || (!ok && got != c.res) {
			t.Errorf("%s: UnframeStatus = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// ExecStatus runs the script framed under a frame of its own, keeps the rest
// of the request, and answers the result only where stdout carries the frame:
// its status is then the script's own. Elsewhere it answers why no answer came
// (Unanswered), and Exec's own error as Exec returned it — and a run its
// deadline stopped with no error, for the caller to answer the timeout before
// any status: read inside its frame where the frame arrived whole, and
// elsewhere as what followed its begin line, nothing where it never began, so
// no banner and no frame line comes back.
func TestExecStatusAnswersOnlyAFramedStatus(t *testing.T) {
	var got sandbox.ExecRequest
	sb := execOnly{exec: func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		got = req
		script, f, ok := sandboxtest.Unwrap(req.Command)
		if !ok || script != "exit 16" {
			t.Fatalf("Exec got %q; want exit 16 wrapped", req.Command)
		}
		res := sandboxtest.Framed(f, sandbox.ExecResult{Stdout: "why\n", ExitCode: 16})
		res.Stdout = "banner" + res.Stdout + "trap"
		return res, nil
	}}
	res, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "exit 16", Timeout: time.Second})
	if err != nil || res != (sandbox.ExecResult{Stdout: "why\n", ExitCode: 16}) || got.Timeout != time.Second ||
		!strings.Contains(got.Command, "'map-probe-begin-") {
		t.Errorf("ExecStatus = %+v, %v (request %+v)", res, err, got)
	}
	sb.exec = func(sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: "welcome", ExitCode: 16}, nil
	}
	var exit *sandbox.StartupExitError
	if _, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "x"}); !errors.As(err, &exit) ||
		exit.Status != 16 || exit.What != "the probe of /w" {
		t.Errorf("ExecStatus over unframed output = %v; want the startup's exit", err)
	}
	sb.exec = func(sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: "partial"}, sandbox.ErrNotFound
	}
	if res, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "x"}); !errors.Is(err, sandbox.ErrNotFound) ||
		res.Stdout != "partial" {
		t.Errorf("ExecStatus over a failed Exec = %+v, %v; want Exec's own answer", res, err)
	}
	var killed sandbox.ExecResult
	sb.exec = func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		_, f, _ := sandboxtest.Unwrap(req.Command)
		begin, _ := f.Lines()
		killed = sandbox.ExecResult{Stdout: "welcome" + begin + "partial", Stderr: begin, ExitCode: 137, TimedOut: true}
		return killed, nil
	}
	if res, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "x", Timeout: time.Second}); err != nil ||
		res != (sandbox.ExecResult{Stdout: "partial", ExitCode: 137, TimedOut: true}) {
		t.Errorf("ExecStatus over a run its deadline stopped after its begin line = %+v, %v (Exec gave %+v); want what followed the begin line", res, err, killed)
	}
	sb.exec = func(sandbox.ExecRequest) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Stdout: "welcome", ExitCode: 137, TimedOut: true}, nil
	}
	if res, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "x", Timeout: time.Second}); err != nil ||
		res != (sandbox.ExecResult{ExitCode: 137, TimedOut: true}) {
		t.Errorf("ExecStatus over a run its deadline stopped before its begin line = %+v, %v; want no output: the banner is not the script's", res, err)
	}
	sb.exec = func(req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		_, f, _ := sandboxtest.Unwrap(req.Command)
		res := sandboxtest.Framed(f, sandbox.ExecResult{Stdout: "why\n", ExitCode: 3})
		res.TimedOut = true
		return res, nil
	}
	if res, err := sandbox.ExecStatus(context.Background(), sb, "probe", "the probe of /w", sandbox.ExecRequest{Command: "x", Timeout: time.Second}); err != nil ||
		res != (sandbox.ExecResult{Stdout: "why\n", ExitCode: 3, TimedOut: true}) {
		t.Errorf("ExecStatus over a run that closed its frame past its deadline = %+v, %v; want what it printed inside the frame", res, err)
	}
}

// StartupExitError names what got no answer, the status the startup left,
// and whether the command had run; errors.Is matches it to ErrUnframedStatus,
// so a caller that already refuses an unframed status (StatPresence) refuses
// it too.
func TestStartupExitErrorSaysWhoseTheStatusWas(t *testing.T) {
	err := error(&sandbox.StartupExitError{What: "the read of /w/a", Status: 16})
	if msg := err.Error(); !strings.Contains(msg, "the read of /w/a") || !strings.Contains(msg, "16") || !strings.Contains(msg, "BASH_ENV") ||
		strings.Contains(msg, "had run") || !errors.Is(err, sandbox.ErrUnframedStatus) || errors.Is(err, sandbox.ErrFileNotExist) {
		t.Errorf("StartupExitError = %q", msg)
	}
	if msg := (&sandbox.StartupExitError{What: "the command's exit record", MayHaveRun: true}).Error(); !strings.Contains(msg, "may have run") {
		t.Errorf("StartupExitError with MayHaveRun = %q, want it to say the command may have run", msg)
	}
	if msg := (&sandbox.StartupExitError{What: "the write of /w/a", Status: 137}).Error(); !strings.Contains(msg, "status 137") ||
		!strings.Contains(msg, "killed by signal 9") || !strings.Contains(msg, "OOM killer") || !strings.Contains(msg, "ended by that file") {
		t.Errorf("StartupExitError of 137 = %q, want the status, a signal named — the startup's or a kill from outside — and the startup's own exit", msg)
	}
	for _, err := range []error{&sandbox.StartupExitError{}, &sandbox.StartupOutputError{}, fmt.Errorf("x: %w", &sandbox.StartupOutputError{})} {
		if !errors.Is(err, sandbox.ErrImageStartup) {
			t.Errorf("%T does not match ErrImageStartup", err)
		}
	}
	if errors.Is(&sandbox.StartupOutputError{}, sandbox.ErrUnframedStatus) {
		t.Error("a StartupOutputError matches ErrUnframedStatus; a flood says nothing of a status")
	}
}

// An image's startup file that traps EXIT with an `exit` of its own would
// replace the status a framed script left, after its frame closed; close_frame
// clears the trap, so the status, and what the script printed, are the
// script's, whether the subshell exits by `exit` or a script that routes its
// exits through close_frame itself does (#942).
func TestFrameClosesPastAnImagesExitTrap(t *testing.T) {
	const hook = `trap "printf 'bye'; printf 'bye' >&2; exit 0" EXIT` + "\n"
	f := sandbox.NewFrame("test")
	for name, command := range map[string]string{
		"wrapped": f.Wrap("echo why; exit 16"),
		"opened":  f.Open() + "echo why; close_frame 16\n",
	} {
		raw := runHooked(t, hook, "/bin/bash", command, "")
		res, ok := f.Unframe(raw)
		if !ok || res.ExitCode != 16 || res.Stdout != "why\n" || strings.Contains(raw.Stdout+raw.Stderr, "bye") {
			t.Errorf("%s: a script that exits 16 under a startup's EXIT trap = %+v (raw %+v), want 16 and the trap never run", name, res, raw)
		}
	}
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
