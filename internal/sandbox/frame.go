package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Frame is the begin and end lines one run of a platform script prints around
// everything it says, on stdout and on stderr alike: the begin line right
// before the script's own work, the end line as it exits, whichever way it
// does. Their suffix is one 64-bit nonce per run (NewFrame), which nothing
// the script prints holds but by a 2⁻⁶⁴ chance — a matched line rg prints
// bare, a file's own bytes — so such output is never read as the frame.
//
// It is how a platform script reads only its own output where an image's
// startup shares the streams (#860, after #827 framed glob's and grep's).
// `bash -c` sources an `ENV BASH_ENV` file before it runs anything, and that
// file can print — on either stream, ending its line or not — leave the
// directory the exec started in, and set an EXIT trap that prints after the
// script is done. The frame keeps out what reaches the streams before the
// begin line and after the end line. It keeps out what the startup prints, not
// what it changes: the startup file still runs, as it runs for every exec, so
// a framed script names every path whole rather than depend on the directory
// the exec started in, and a function an image exports under a command's name
// still answers — and prints — for that command when the script calls it. Nor
// does it keep out what a child the script runs prints between the lines — a
// library an `ENV LD_PRELOAD` names printing a banner from its constructor in
// every process it loads into — which the image contract forbids
// (docs/self-hosted-security.md). And it is no boundary against the sandbox's
// own processes: the script, nonce and all, is the exec's argv, which any
// process in the sandbox may read, and one that forges a frame is tampering
// with what a sandbox it can already write tells the platform about itself.
type Frame struct{ begin, end string }

// frameNonceDigits is the length of a frame's nonce in hex: 64 bits.
const frameNonceDigits = 16

// frameLabel is what NewFrame accepts as a label, so Unwrap can read one back
// out of a command unambiguously.
var frameLabel = regexp.MustCompile(`^[a-z0-9]+$`)

// NewFrame is a frame for one run of a script: "map-<label>-begin-<nonce>"
// and "map-<label>-end-<nonce>", the nonce 64 bits from crypto/rand. The
// label names the script in what the sandbox printed, for whoever reads it,
// and must be lowercase letters and digits.
func NewFrame(label string) Frame {
	if !frameLabel.MatchString(label) {
		panic(fmt.Sprintf("sandbox: frame label %q is not lowercase letters and digits", label))
	}
	var nonce [frameNonceDigits / 2]byte
	_, _ = rand.Read(nonce[:])
	return frameOf(label, hex.EncodeToString(nonce[:]))
}

func frameOf(label, nonce string) Frame {
	return Frame{begin: "map-" + label + "-begin-" + nonce, end: "map-" + label + "-end-" + nonce}
}

// Open is what a framed script begins with: close_frame, which prints the end
// line on both streams and exits with its argument — the one way the script
// may exit — and then the begin line on both. Each line is printed after a
// newline of its own, so it is a line however what came before it ended. It
// is POSIX shell, so a script `sh -c` runs can open with it too. A script
// that cannot route each of its exits through close_frame takes Wrap instead.
func (f Frame) Open() string {
	return fmt.Sprintf(`close_frame() { printf '\n%%s\n' '%[2]s'; printf '\n%%s\n' '%[2]s' >&2; exit "$1"; }
printf '\n%%s\n' '%[1]s'; printf '\n%%s\n' '%[1]s' >&2
`, f.begin, f.end)
}

// wrapClose is what Wrap puts after the script: the subshell's end, and the
// frame closed with the status the script left.
const wrapClose = "\n)\nclose_frame \"$?\"\n"

// Wrap frames a whole script: Open, then the script in a subshell, then
// close_frame with the status the subshell left — so every way the script
// exits, an `exit` from inside one of its functions included, still closes
// the frame, and the exec exits as the script did. The subshell inherits the
// positional parameters, stdin and the functions and variables the shell has,
// and not its traps, so an EXIT trap an image's startup file set runs once,
// after the end line, as the outer shell exits. The script is parsed whole
// before it runs, as a subshell is, so it must not turn on a shell option that
// changes how bash parses — extglob — and use it in the same script.
func (f Frame) Wrap(script string) string {
	return f.Open() + "(\n" + script + wrapClose
}

// Cut returns what the script printed on one stream — what lies between the
// last begin line and the end line after it — with framed true, and short
// true where the sandbox's cap cut the stream before its end line. The newline
// before the end line is the frame's, not the script's. The last begin line,
// because whatever printed before the script — an image's startup — can print
// a line that looks like one, nonce and all, having read the script from the
// exec's argv; the script's own comes after it. That choice opens the other
// side as far as it closes this one: what prints after the script's end line
// — an EXIT trap the startup file set, which runs in the script's own shell
// and reads the nonce there — can print a begin line, an answer and an end
// line of its own, and that is what is read. Neither is a boundary the frame
// keeps: it keeps out what an image's startup prints by accident, not what a
// process in the sandbox forges on purpose (Frame).
//
// A stream the sandbox's cap cut (truncated: that stream's own flag, never the
// other's) before its end line is short: all that follows the begin line is
// what there is, less any start of the end line the cap left at its tail. One
// the cap cut only after its end line — an EXIT trap's flood — is whole, and
// not short; so is one it cut inside the end line, where what it left of it
// reaches into the nonce: the script prints the end line after all else it
// prints there, and a tail that carries the nonce is that line's, which
// nothing the script prints holds but by chance. A tail of the end line's
// constant part alone — "\n", or "\nmap-search-e" — could be the script's
// own, so a stream cut there is short. A stream with no begin line, or a whole
// one with no end line after it, is not one the script printed to its end:
// "", false, false.
func (f Frame) Cut(s string, truncated bool) (text string, framed, short bool) {
	return cutFrame(f, s, truncated)
}

// CutBytes is Cut over bytes, for a stream that is a file's bytes, which it
// slices rather than copies.
func (f Frame) CutBytes(b []byte, truncated bool) (text []byte, framed, short bool) {
	return cutFrame(f, b, truncated)
}

func cutFrame[T string | []byte](f Frame, s T, truncated bool) (T, bool, bool) {
	var none T
	begin := "\n" + f.begin + "\n"
	var rest T
	switch i := lastIndex(s, begin); {
	case i >= 0:
		rest = s[i+len(begin):]
	case hasPrefix(s, begin[1:]):
		// The begin line's own newline, with nothing before it to end.
		rest = s[len(begin)-1:]
	default:
		return none, false, false
	}
	end := "\n" + f.end + "\n"
	if j := index(rest, end); j >= 0 {
		return rest[:j], true, false
	}
	if !truncated {
		return none, false, false
	}
	constant := len(end) - frameNonceDigits - len("\n")
	for k := min(len(end)-1, len(rest)); k > 0; k-- {
		if string(rest[len(rest)-k:]) == end[:k] {
			return rest[:len(rest)-k], true, k <= constant
		}
	}
	return rest, true, true
}

func lastIndex[T string | []byte](s T, sep string) int {
	if b, ok := any(s).([]byte); ok {
		return bytes.LastIndex(b, []byte(sep))
	}
	return strings.LastIndex(string(s), sep)
}

func index[T string | []byte](s T, sep string) int {
	if b, ok := any(s).([]byte); ok {
		return bytes.Index(b, []byte(sep))
	}
	return strings.Index(string(s), sep)
}

func hasPrefix[T string | []byte](s T, prefix string) bool {
	return len(s) >= len(prefix) && string(s[:len(prefix)]) == prefix
}

// Unframe is res as its framed script printed it: each stream cut to what lies
// between the frame's lines (Cut), and each stream's truncation flag saying
// whether the cap took any of what the script printed there — cut it short,
// or, on a stderr with no begin line that the cap did cut, took all of it,
// the startup's flood having filled the cap first. A stderr with no frame
// holds none of the script's and is "". ok is whether stdout was framed: the
// script printed its begin line there and either its end line or as much as
// the cap kept. Where it is false the script's answer did not reach the
// output, and res comes back as Exec reported it, for the caller to say what
// the sandbox printed instead; what the caller makes of it is its own — each
// caller's failure handling says.
func (f Frame) Unframe(res ExecResult) (ExecResult, bool) {
	out, framed, outShort := f.Cut(res.Stdout, res.StdoutTruncated)
	if !framed {
		return res, false
	}
	msg, msgFramed, msgShort := f.Cut(res.Stderr, res.StderrTruncated)
	res.Stdout, res.StdoutTruncated = out, outShort
	res.Stderr, res.StderrTruncated = msg, msgShort || (res.StderrTruncated && !msgFramed)
	return res, framed
}

// ExecFramed runs req's command in sb framed (Wrap), under a frame of its own
// labelled label, and answers what the script printed (Unframe): the result,
// and whether its stdout was framed. Exec's own error is returned as Exec
// returned it.
func ExecFramed(ctx context.Context, sb Sandbox, label string, req ExecRequest) (ExecResult, bool, error) {
	f := NewFrame(label)
	req.Command = f.Wrap(req.Command)
	res, err := sb.Exec(ctx, req)
	if err != nil {
		return res, false, err
	}
	res, framed := f.Unframe(res)
	return res, framed, nil
}

// wrapped matches the head of a command Wrap made, for Unwrap: its end line,
// whose label and nonce are the frame's.
var wrapped = regexp.MustCompile(`^close_frame\(\) \{ printf '\\n%s\\n' 'map-([a-z0-9]+)-end-([0-9a-f]{16})'; `)

// Unwrap undoes Wrap: the script a framed command carries, and its frame. It
// is for a fake sandbox, which answers the script as a sandbox would and
// frames its answer (Framed) as the script's run would have.
func Unwrap(command string) (script string, f Frame, ok bool) {
	m := wrapped.FindStringSubmatch(command)
	if m == nil {
		return "", Frame{}, false
	}
	f = frameOf(m[1], m[2])
	head := f.Open() + "(\n"
	if !strings.HasPrefix(command, head) || !strings.HasSuffix(command, wrapClose) || len(command) < len(head)+len(wrapClose) {
		return "", Frame{}, false
	}
	return command[len(head) : len(command)-len(wrapClose)], f, true
}

// Framed is res as a framed script's run prints it: each stream between the
// frame's begin and end lines — but for a stream res marks truncated, which
// the cap cut inside what the script printed, so it keeps its begin line and
// loses its end line with the rest. Like Unwrap it is for fakes.
func (f Frame) Framed(res ExecResult) ExecResult {
	frame := func(s string, truncated bool) string {
		s = "\n" + f.begin + "\n" + s
		if !truncated {
			s += "\n" + f.end + "\n"
		}
		return s
	}
	res.Stdout, res.Stderr = frame(res.Stdout, res.StdoutTruncated), frame(res.Stderr, res.StderrTruncated)
	return res
}
