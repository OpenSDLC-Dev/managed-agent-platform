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

// frameNonceTrusted is how many of the nonce's digits a stream's cut tail has
// to carry before Cut takes it for the end line rather than for what the
// script printed: 32 bits' worth, where one digit would be a coincidence any
// sixteenth of the time output ends in the end line's constant part.
const frameNonceTrusted = 8

// frameLabel is what NewFrame and FrameOf accept as a label, so a command's
// frame can be read back out of it unambiguously.
var frameLabel = regexp.MustCompile(`^[a-z0-9]+$`)

// frameNonce is a nonce as NewFrame writes one.
var frameNonce = regexp.MustCompile(`^[0-9a-f]{16}$`)

// NewFrame is a frame for one run of a script: "map-<label>-begin-<nonce>"
// and "map-<label>-end-<nonce>", the nonce 64 bits from crypto/rand. The
// label names the script in what the sandbox printed, for whoever reads it,
// and must be lowercase letters and digits.
func NewFrame(label string) Frame {
	var nonce [frameNonceDigits / 2]byte
	_, _ = rand.Read(nonce[:])
	return FrameOf(label, hex.EncodeToString(nonce[:]))
}

// FrameOf is the frame a label and a nonce name — NewFrame's, rebuilt from
// what a framed command spells, for a fake sandbox to answer it
// (sandboxtest.Unwrap). It panics on a label NewFrame would refuse, or a
// nonce that is not 16 lowercase hex digits.
func FrameOf(label, nonce string) Frame {
	if !frameLabel.MatchString(label) {
		panic(fmt.Sprintf("sandbox: frame label %q is not lowercase letters and digits", label))
	}
	if !frameNonce.MatchString(nonce) {
		panic(fmt.Sprintf("sandbox: frame nonce %q is not %d lowercase hex digits", nonce, frameNonceDigits))
	}
	return Frame{begin: "map-" + label + "-begin-" + nonce, end: "map-" + label + "-end-" + nonce}
}

// Lines is the frame's begin and end lines as a stream carries them, each
// with the newline the script prints before it and the one after.
func (f Frame) Lines() (begin, end string) {
	return "\n" + f.begin + "\n", "\n" + f.end + "\n"
}

// Open is what a framed script begins with: close_frame, which prints the end
// line on both streams and exits with its argument — the one way the script
// may exit — then `set +e`, and then the begin line on both. Each line is
// printed after a newline of its own, so it is a line however what came before
// it ended. It is POSIX shell, so a script `sh -c` runs can open with it too.
//
// The script inherits the shell options an image's startup set, and errexit
// among them would end it at the first command that fails — rg finding no
// match — before it reached close_frame, so the frame would never close. A
// script that routes every exit through close_frame itself is written for the
// shell's defaults, so Open turns errexit off. A script that cannot route each
// of its exits through close_frame takes Wrap instead.
func (f Frame) Open() string {
	return fmt.Sprintf(`close_frame() { printf '\n%%s\n' '%[2]s'; printf '\n%%s\n' '%[2]s' >&2; exit "$1"; }
set +e
printf '\n%%s\n' '%[1]s'; printf '\n%%s\n' '%[1]s' >&2
`, f.begin, f.end)
}

// wrapClose is what Wrap puts after the script: the subshell's end, and the
// frame closed with the status the script left.
const wrapClose = "\n) || close_frame \"$?\"\nclose_frame 0\n"

// Wrap frames a whole script: Open, then the script in a subshell, then
// close_frame with the status the subshell left — so every way the script
// exits, an `exit` from inside one of its functions included, and an `exec`,
// which replaces the subshell alone, still closes the frame, and the exec
// exits as the script did. The subshell inherits the positional parameters,
// stdin and the functions and variables the shell has, and not its traps, so
// an EXIT trap an image's startup file set runs once, after the end line, as
// the outer shell exits.
//
// It inherits the shell's options as well, errexit among them where the
// startup set it. Open turns errexit off, and Wrap does not lean on that: the
// subshell is the left side of `||`, where bash and POSIX sh alike ignore
// errexit — for the subshell's own status and for every command inside it —
// so a script that fails a command runs on as written, and one that fails
// still reaches close_frame, whatever an image does to `set`. The script is
// parsed whole before it runs, as a subshell is, so it must not turn on a
// shell option that changes how bash parses — extglob — and use it in the
// same script.
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
// carries at least 8 of the nonce's 16 digits: the script prints the end line
// after all else it prints there, and a tail that carries that much of the
// nonce is that line's, which nothing the script prints holds but by a 2⁻³²
// chance. A shorter tail — "\n", the constant "\nmap-<label>-end-", or that
// and a digit or seven — could be the script's own, so a stream cut there is
// short. A stream with no begin line, or a whole one with no end line after
// it, is not one the script printed to its end: "", false, false.
func (f Frame) Cut(s string, truncated bool) (text string, framed, short bool) {
	return cutFrame(f, s, truncated, 0)
}

// CutBytes is Cut over bytes, for a stream that is a file's bytes, which it
// slices rather than copies.
func (f Frame) CutBytes(b []byte, truncated bool) (text []byte, framed, short bool) {
	return cutFrame(f, b, truncated, 0)
}

// CutBytesWithin is CutBytes for a stream whose output outside the frame is
// expected within room bytes on each side, and whose bytes between the lines
// can be tens of megabytes — a file read's, whose buffer keeps that room
// beside the file. The begin line is looked for in the stream's first room
// bytes and the end line in its last room bytes, so the file between them is
// not scanned for either: the last begin line there, and the first end line
// there. Only a stream with neither there — a startup that printed more than
// room before the frame, or a trap more after it — is searched whole, as
// CutBytes searches it. Beyond reading 50 MB in a few milliseconds rather than
// tens of them, it reads a file that holds the frame's own lines by chance,
// which CutBytes would cut at them, as the file it is.
func (f Frame) CutBytesWithin(b []byte, truncated bool, room int) (text []byte, framed, short bool) {
	return cutFrame(f, b, truncated, room)
}

// CutInBegin reports whether s ends partway through the frame's begin line —
// a stream that lost its tail while the begin line was on its way, which
// carries none of the script's output and no sign that anything else ended it.
// A stream ending in a newline is one, the begin line starting with its own;
// one ending in the whole begin line is not.
func (f Frame) CutInBegin(s string) bool {
	begin, _ := f.Lines()
	if strings.HasSuffix(s, begin) {
		return false
	}
	for k := min(len(begin)-1, len(s)); k > 0; k-- {
		if strings.HasSuffix(s, begin[:k]) {
			return true
		}
	}
	return false
}

// cutFrame is Cut, with room > 0 bounding where the lines are looked for
// first (CutBytesWithin).
func cutFrame[T string | []byte](f Frame, s T, truncated bool, room int) (T, bool, bool) {
	var none T
	begin, end := f.Lines()
	head := len(s)
	if room > 0 {
		head = min(len(s), room+len(begin))
	}
	i := lastIndex(s[:head], begin)
	if i < 0 && head < len(s) {
		i = lastIndex(s, begin)
	}
	var rest T
	switch {
	case i >= 0:
		rest = s[i+len(begin):]
	case hasPrefix(s, begin[1:]):
		// The begin line's own newline, with nothing before it to end.
		rest = s[len(begin)-1:]
	default:
		return none, false, false
	}
	j := -1
	if from := len(rest) - room - len(end); room > 0 && from > 0 {
		if k := index(rest[from:], end); k >= 0 {
			j = from + k
		}
	}
	if j < 0 {
		j = index(rest, end)
	}
	if j >= 0 {
		return rest[:j], true, false
	}
	if !truncated {
		return none, false, false
	}
	constant := len(end) - frameNonceDigits - len("\n")
	for k := min(len(end)-1, len(rest)); k > 0; k-- {
		if string(rest[len(rest)-k:]) == end[:k] {
			return rest[:len(rest)-k], true, k < constant+frameNonceTrusted
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
