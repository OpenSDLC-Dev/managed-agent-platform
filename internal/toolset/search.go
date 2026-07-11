package toolset

import (
	"context"
	"encoding/json"
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

// globScript expands the pattern with bash's own globstar, which is where
// doublestar semantics already live: `**` spans directories, `*` does not cross
// a separator, and dotglob makes a leading dot ordinary — the same set the
// reference's hand-rolled matcher implements. Matches are then stamped with
// their mtimes and sorted newest first.
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
shopt -s globstar dotglob nullglob
IFS=
for t in stat sort xargs; do
  command -v "$t" >/dev/null 2>&1 || { printf 'glob: %s not found in the sandbox image\n' "$t" >&2; exit 2; }
done
root=__ROOT__
prefix=__PREFIX__
pat=__PAT__
if [ ! -d "$root" ]; then printf 'glob: %s: no such directory\n' "$root" >&2; exit 2; fi
for f in "$prefix"$pat; do
  if [ -e "$f" ] || [ -L "$f" ]; then printf '%s\0' "$f"; fi
done | xargs -0 -r stat --printf '%.9Y %n\0' | sort -z -rn -k1,1
`

// grepScript searches with the image's own grep: PCRE where it has it — a model
// writes \d and \b far more readily than their POSIX spellings — and ERE where
// it does not. The probe tells the two apart by exit code: a grep with PCRE
// finds nothing in /dev/null and exits 1, one without it rejects -P and exits 2.
const grepScript = `root=__ROOT__
pat=__PAT__
flavor=-P
grep -qP -- '' /dev/null 2>/dev/null
if [ "$?" -ge 2 ]; then flavor=-E; fi
grep -rnI "$flavor" --exclude-dir=.git --exclude-dir=node_modules -e "$pat" -- "$root"
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

	res, err := r.execScript(ctx, globScript, root, prefix, in.Pattern)
	if err != nil {
		return Result{}, err
	}
	switch {
	case res.TimedOut:
		return failf("glob: timed out after %s", DefaultTimeout)
	case res.ExitCode != 0:
		return searchFailure("glob", res)
	}

	// stat printed "<mtime> <path>\0" per match, newest first. Records split on
	// NUL (paths may contain newlines and spaces); the mtime splits off on the
	// first space.
	var paths []string
	for _, rec := range strings.Split(res.Stdout, "\x00") {
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

func (r Runner) grep(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in searchInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid grep input: %v", err)
	}
	if in.Pattern == "" {
		return failf("grep: pattern is required")
	}
	if res, bad := badField("grep", "pattern", in.Pattern); bad {
		return res, nil
	}
	if res, bad := badField("grep", "path", in.Path); bad {
		return res, nil
	}

	root := r.workdir()
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	// No absolute-pattern handling here: a grep pattern is a regex, not a path,
	// and one that happens to start with "/" must not be mistaken for an
	// absolute root and turned loose on the whole filesystem.
	res, err := r.execScript(ctx, grepScript, root, "", in.Pattern)
	if err != nil {
		return Result{}, err
	}
	switch {
	case res.TimedOut:
		return failf("grep: timed out after %s", DefaultTimeout)
	case res.ExitCode == 1:
		// grep's own "nothing matched". It is the answer, not a failure.
		return succeed("no matches")
	case res.ExitCode != 0:
		return searchFailure("grep", res)
	}
	out := strings.TrimRight(res.Stdout, "\n")
	if out == "" {
		return succeed("no matches")
	}
	return succeed(out)
}

// execScript renders a search script with the model's search root, prefix and
// pattern as data — single-quoted, never interpolated as code — and runs it.
// grep passes an empty prefix, which grepScript does not reference.
func (r Runner) execScript(ctx context.Context, script, root, prefix, pattern string) (sandbox.ExecResult, error) {
	cmd := strings.NewReplacer(
		"__ROOT__", singleQuote(root),
		"__PREFIX__", singleQuote(prefix),
		"__PAT__", singleQuote(pattern),
	).Replace(script)
	return r.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: cmd, Timeout: DefaultTimeout})
}

// searchFailure hands the model what the command itself said — the bad regex,
// the missing directory — rather than a message of our own invention.
func searchFailure(tool string, res sandbox.ExecResult) (Result, error) {
	msg := strings.TrimSpace(combine(res))
	if msg == "" {
		msg = fmt.Sprintf("%s: failed with exit code %d", tool, res.ExitCode)
	}
	return failf("%s", msg)
}
