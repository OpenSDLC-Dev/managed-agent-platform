package toolset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// globLimit caps how many paths glob reports, newest first — the reference's
// limit. The walk itself is bounded only by the tool's timeout.
const globLimit = 200

type searchInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// searchBeginPrefix and searchEndPrefix open the two lines a search's script —
// glob's and grep's — prints around everything it says, on stdout and on
// stderr alike: the begin line before anything else, the end line as it
// exits. Their suffix is one 64-bit nonce per search (newSearchFrame), which
// no searched file holds but by a 2⁻⁶⁴ chance: rg prints a matched line bare
// from a single file with -n false, and such a line must never be read as the
// frame.
//
// The frame keeps out what reaches an exec's streams that the script did not
// print: an image's banner before it — what an `ENV BASH_ENV` file prints as
// the shell starts, say — and anything after it, such as an EXIT trap's. It
// keeps out what an image's startup prints, not what it changes; whether the
// platform's own scripts should run without it is #860. Nor is it a boundary
// against the sandbox's own processes: the script, nonce and all, is the
// exec's argv, which any process in the sandbox may read, and a model that
// forges a search's output from inside its own sandbox is tampering with what
// it alone reads.
const (
	searchBeginPrefix = "map-search-begin-"
	searchEndPrefix   = "map-search-end-"
)

// searchFrame is one search's begin and end lines.
type searchFrame struct{ begin, end string }

func newSearchFrame() searchFrame {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	n := hex.EncodeToString(nonce[:])
	return searchFrame{begin: searchBeginPrefix + n, end: searchEndPrefix + n}
}

// open is what a search's script begins with: close_frame, which prints the
// end line on both streams and exits with its argument — the one way the
// script exits — and then the begin line on both.
func (f searchFrame) open() string {
	return fmt.Sprintf(`close_frame() { printf '\n%%s\n' %[2]s; printf '\n%%s\n' %[2]s >&2; exit "$1"; }
printf '\n%%s\n' %[1]s; printf '\n%%s\n' %[1]s >&2
`, singleQuote(f.begin), singleQuote(f.end))
}

// cut returns what the script printed on one stream — what lies between the
// last begin line and the end line after it — with framed true, and short
// true where the sandbox's cap cut the stream before its end line. Each line
// is printed after a newline of its own, so it is a line however what came
// before it ended, and the newline before the end line is the frame's, not
// the script's. The last begin line, because whatever printed before the
// script — an image's banner — can print a line that looks like one, nonce
// and all, having read the script from the exec's argv; the script's own
// comes after it. That choice opens the other side as far as it closes this
// one: what prints after the script's end line — an EXIT trap an image's
// startup file set, which runs in the script's own shell and reads the nonce
// there — can print a begin line, an answer and an end line of its own, and
// that is what is read. Neither is a boundary the frame keeps: it keeps out
// what an image's startup prints by accident, not what a process in the
// sandbox forges on purpose (searchBeginPrefix, #860).
//
// A stream the sandbox's cap cut (truncated: that stream's own flag, never
// the other's) before its end line is short: all that follows the begin
// line is what there is, less any start of the end line the cap left at its
// tail. One the cap cut only after its end line — an EXIT trap's flood — is
// whole, and not short. A stream with no begin line, or a whole one with no
// end line after it, is not one the script printed to its end: "", false,
// false.
func (f searchFrame) cut(s string, truncated bool) (text string, framed, short bool) {
	t := "\n" + s
	begin := "\n" + f.begin + "\n"
	i := strings.LastIndex(t, begin)
	if i < 0 {
		return "", false, false
	}
	rest := t[i+len(begin):]
	end := "\n" + f.end + "\n"
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j], true, false
	}
	if !truncated {
		return "", false, false
	}
	for k := min(len(end)-1, len(rest)); k > 0; k-- {
		if strings.HasSuffix(rest, end[:k]) {
			return rest[:len(rest)-k], true, true
		}
	}
	return rest, true, true
}

// messages is what a search's script printed on stderr (cut), trimmed, and
// what the sandbox's cap took of it there. A stderr the cap cut between the
// begin and end lines keeps what came before the cut, with the truncation
// notice after it; one it cut only after the end line — an EXIT trap's flood
// — took none of the script's, and says nothing of the cap. One the cap cut
// before the begin line — what an image's startup printed filled the cap
// first — has lost all the script printed there: its messages are "", and
// lost tells the caller to say the cap cut them (searchFailure, grepAnswer).
// A stderr the cap did not cut and with no frame is none of the script's:
// "".
func (f searchFrame) messages(res sandbox.ExecResult) (msg string, lost bool) {
	msg, framed, short := f.cut(res.Stderr, res.StderrTruncated)
	msg = strings.TrimSpace(msg)
	if short {
		msg = strings.TrimSpace(msg + "\n" + truncationNotice)
	}
	return msg, res.StderrTruncated && !framed
}

// inputTooLong is Exec's refusal (sandbox.CommandTooLongError) of a search's
// command, which grew with the values the model sent — inputs names them — so
// the model can send less. Only searchExec makes one; Runner.dispatch answers
// it, and answers any other command too long as the platform's own.
type inputTooLong struct {
	inputs string
	bytes  int
}

func (e *inputTooLong) Error() string {
	return fmt.Sprintf("%s make a %d-byte command, over the %d bytes one exec argument can carry",
		e.inputs, e.bytes, sandbox.MaxCommandBytes)
}

// searchExec runs a search's script — glob's, grep's — under the tools'
// default deadline. The script carries inputs, the model's values, so Exec's
// refusal of it as too long is theirs (inputTooLong).
func (r Runner) searchExec(ctx context.Context, script, inputs string) (sandbox.ExecResult, error) {
	res, err := r.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: script, Timeout: DefaultTimeout})
	var tooLong *sandbox.CommandTooLongError
	if errors.As(err, &tooLong) {
		return res, &inputTooLong{inputs: inputs, bytes: tooLong.Bytes}
	}
	return res, err
}

// unframed is a search's answer to output its script did not print to its
// end (searchFrame.cut): a failure carrying what the sandbox printed, never an
// answer.
func unframed(tool string, res sandbox.ExecResult) (Result, error) {
	msg := fmt.Sprintf("%s: no answer reached the output whole (exit %d): the sandbox's shell exited, or filled the output cap, before the search finished", tool, res.ExitCode)
	if more := strings.TrimSpace(combine(res)); more != "" {
		msg += "\n" + more
	}
	return failf("%s", msg)
}

// globScript expands the pattern with bash's own globstar, which is where
// doublestar semantics already live: `**` spans directories, `*` does not cross
// a separator, and dotglob makes a leading dot ordinary — the same set the
// reference's hand-rolled matcher implements. Matches are then stamped with
// their mtimes and sorted newest first. globstar is bash 4.0's: a bash that
// refuses it stops the search with a message naming the version, rather than
// expanding `**` as `*`. The script runs inside a search frame
// (searchFrame.open), which only close_frame exits, so what it prints is read
// from between the frame's lines.
//
// The pattern is a variable, never a literal in the script, and IFS is empty so
// its value is not word-split: it is expanded exactly once, as a pathname
// pattern, and never as code.
//
// The whole pipeline is NUL-delimited, not newline-delimited, end to end: a
// filename may legally contain a newline, and stat's `%n` echoes it raw, so a
// newline-delimited record stream would let one matched file whose name carries
// `\n<digits> <path>` inject a second, fabricated record. `--printf … \0`
// terminates each record with a NUL (which no path can contain), `sort -z`
// sorts on that delimiter, and the Go side splits on it — so a match is exactly
// one record whatever its name.
//
// pipefail is on so a broken pipeline is a reported error, never a silent "no
// matches" — a masked failure would read to the model as "the directory is
// empty", which is worse than an error it can retry. The up-front command -v
// guard is the clearer message for the common case (an image missing a tool),
// but pipefail is the catch-all: a stat that does not understand --printf, or
// any mid-pipeline failure, still surfaces rather than being swallowed by the
// final sort's exit 0. The cost is that a match unlinked mid-listing by a
// concurrent background job (a rare per-file stat race) makes the whole call a
// retryable error rather than returning the survivors — the conservative
// direction, chosen because it never returns a silently wrong answer.
const globScript = `set -o pipefail
shopt -s globstar dotglob nullglob 2>/dev/null || { printf 'glob: needs bash 4.0 or later in the sandbox image, for ** (globstar); this one is bash %s\n' "$BASH_VERSION" >&2; close_frame 2; }
IFS=
for t in stat sort xargs; do
  command -v "$t" >/dev/null 2>&1 || { printf 'glob: %s not found in the sandbox image\n' "$t" >&2; close_frame 2; }
done
root=__ROOT__
prefix=__PREFIX__
pat=__PAT__
if [ ! -d "$root" ]; then printf 'glob: %s: no such directory\n' "$root" >&2; close_frame 2; fi
for f in "$prefix"$pat; do
  if [ -e "$f" ] || [ -L "$f" ]; then printf '%s\0' "$f"; fi
done | xargs -0 -r stat --printf '%.9Y %n\0' | sort -z -rn -k1,1
close_frame "$?"
`

func (r Runner) glob(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in searchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid glob input: %v", err)
	}
	if in.Pattern == "" {
		return failf("glob: pattern is required")
	}
	if res, bad := badField("glob", "pattern", in.Pattern); bad {
		return res, nil
	}
	if res, bad := badField("glob", "path", in.Path); bad {
		return res, nil
	}

	root := r.workdir()
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	// A relative pattern hangs off the search root; keeping the trailing slash in
	// the prefix rather than in root is what stops a root of "/" from producing
	// "//". An absolute pattern names its own root, so the search directory is
	// irrelevant to it — matching the reference, which sets root to "/". Leaving
	// root at the search directory would make an absolute pattern fail whenever
	// that directory happened to be absent.
	prefix := strings.TrimSuffix(root, "/") + "/"
	if path.IsAbs(in.Pattern) {
		root, prefix = "/", ""
	}

	frame := newSearchFrame()
	cmd := frame.open() + strings.NewReplacer(
		"__ROOT__", singleQuote(root),
		"__PREFIX__", singleQuote(prefix),
		"__PAT__", singleQuote(in.Pattern),
	).Replace(globScript)
	res, err := r.searchExec(ctx, cmd, "the pattern and path")
	if err != nil {
		return Result{}, err
	}
	if res.TimedOut {
		return failf("glob: timed out after %s", DefaultTimeout)
	}
	out, framed, short := frame.cut(res.Stdout, res.StdoutTruncated)
	if !framed {
		return unframed("glob", res)
	}
	if res.ExitCode != 0 {
		msg, lost := frame.messages(res)
		return searchFailure("glob", out, msg, res.ExitCode, short || lost)
	}

	// stat printed "<mtime> <path>\0" per match, newest first. Records split on
	// NUL (paths may contain newlines and spaces), and only a record its NUL
	// ends counts — what follows the last is nothing, or a record the output
	// cap cut; the mtime splits off on the first space.
	recs := strings.Split(out, "\x00")
	var paths []string
	for _, rec := range recs[:len(recs)-1] {
		_, p, ok := strings.Cut(rec, " ")
		if !ok {
			continue
		}
		paths = append(paths, p)
		if len(paths) == globLimit {
			break
		}
	}
	if len(paths) == 0 {
		return succeed("no matches")
	}
	return succeed(strings.Join(paths, "\n"))
}

// searchFailure hands the model what a search's script itself said — out, its
// framed stdout, and msg, its messages (searchFrame.messages): the bad regex,
// the missing directory — rather than a message of our own invention, and the
// exit code where it said nothing. Each stream's cut is said where it cut: a
// msg the cap cut carries its own notice after it, and cut — stdout cut, or
// the messages lost whole — puts one in front.
func searchFailure(tool, out, msg string, code int, cut bool) (Result, error) {
	failure := strings.TrimSpace(combine(sandbox.ExecResult{Stdout: out, Stderr: msg}))
	if failure == "" {
		failure = fmt.Sprintf("%s: failed with exit code %d", tool, code)
	}
	if cut {
		failure = truncationNotice + "\n" + failure
	}
	return failf("%s", failure)
}
