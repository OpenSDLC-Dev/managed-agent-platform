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
// pipefail is on and every path handed to stat is one the shell just saw exist,
// so a stat that fails means the image is missing a tool rather than a file
// having gone — the call reports it instead of quietly answering "no matches".
const globScript = `set -o pipefail
shopt -s globstar dotglob nullglob
IFS=
root=__ROOT__
prefix=__PREFIX__
pat=__PAT__
if [ ! -d "$root" ]; then printf 'glob: %s: no such directory\n' "$root" >&2; exit 2; fi
for f in "$prefix"$pat; do
  if [ -e "$f" ] || [ -L "$f" ]; then printf '%s\0' "$f"; fi
done | xargs -0 -r stat -c '%.9Y %n' | sort -rn -k1,1
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
	res, err := r.runSearch(ctx, globScript, in)
	if err != nil {
		return Result{}, err
	}
	switch {
	case res.TimedOut:
		return failf("glob: timed out after %s", DefaultTimeout)
	case res.ExitCode != 0:
		return searchFailure("glob", res)
	}

	// stat printed "<mtime> <path>" a line at a time, newest first. The split is
	// on the first space only — a path may contain them.
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		_, p, ok := strings.Cut(line, " ")
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
	res, err := r.runSearch(ctx, grepScript, in)
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

// runSearch renders a search script with the model's pattern and search root as
// data — single-quoted, never interpolated as code — and runs it.
func (r Runner) runSearch(ctx context.Context, script string, in searchInput) (sandbox.ExecResult, error) {
	root := r.workdir()
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	// An absolute pattern names its own root; a relative one hangs off the
	// search root. Keeping the trailing slash in the prefix rather than in root
	// is what stops a root of "/" from producing "//".
	prefix := strings.TrimSuffix(root, "/") + "/"
	if path.IsAbs(in.Pattern) {
		prefix = ""
	}
	cmd := strings.NewReplacer(
		"__ROOT__", singleQuote(root),
		"__PREFIX__", singleQuote(prefix),
		"__PAT__", singleQuote(in.Pattern),
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
