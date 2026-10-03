package toolset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/memsync"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// grep's three output modes, as the recorded schema's enum names them.
const (
	grepContent = "content"
	grepFiles   = "files_with_matches"
	grepCount   = "count"
)

// grepInput is the recorded reference's grep input (#827): the SDK's pattern
// and path, and the twelve properties the reference adds. The numbers are the
// schema's "number" — a float on the wire — and pointers where an absent
// value differs from zero.
type grepInput struct {
	Pattern    string   `json:"pattern"`
	Path       string   `json:"path"`
	Type       string   `json:"type"`
	Glob       string   `json:"glob"`
	OutputMode string   `json:"output_mode"`
	LineNums   *bool    `json:"-n"`
	After      *float64 `json:"-A"`
	Before     *float64 `json:"-B"`
	C          *float64 `json:"-C"`
	Context    *float64 `json:"context"`
	IgnoreCase bool     `json:"-i"`
	HeadLimit  *float64 `json:"head_limit"`
	Offset     *float64 `json:"offset"`
	Multiline  bool     `json:"multiline"`
}

// grepQuery is a validated grepInput: rg's arguments, the paging rg does not
// do itself, and the line that opens rg's part of the output (newGrepBegin).
type grepQuery struct {
	args        []string
	skip, limit int
	begin       string
}

// whole reads one of the schema's number options: a whole number from 0 to
// 2³¹−1, or -1 when it is absent.
func whole(name string, v *float64) (int, string) {
	if v == nil {
		return -1, ""
	}
	if *v < 0 || *v != math.Trunc(*v) || *v > math.MaxInt32 {
		return 0, fmt.Sprintf("%s must be a whole number from 0 to %d, not %s", name, math.MaxInt32, number(*v))
	}
	return int(*v), ""
}

// number spells a JSON number as a refusal quotes it: a whole one as its
// digits — 2147483648, where %v would print 2.147483648e+09 — up to where
// JavaScript too turns to an exponent, and any other in Go's shortest form
// (1.5, -0.25, 1e+21).
func number(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e21 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// vcsDirs are the version-control directories a search never enters, which
// is Claude Code's GrepTool's list: the recorded property descriptions are
// that tool's word for word, and it runs rg with --hidden and a `!dir` glob
// for each of these (docs/DIVERGENCES.md, the INFERRED grep entry).
var vcsDirs = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// query maps the input onto rg, flag for the flag each recorded description
// names, searching root from cwd, the directory rg runs in. The rest is ours
// and fixed: --no-config, so an image's RIPGREP_CONFIG_PATH cannot change what
// a call means; --no-heading, so every line carries its file; --hidden with
// the version-control directories globbed out, as the reference's GrepTool
// searches (vcsDirs) — .gitignore and the other ignore files rg reads still
// apply; --sort=path, so an answer lists in one order run after run, and
// offset pages through that order whether or not the call that came before
// paged — which costs rg its parallel search, as sorting does (docs/
// DIVERGENCES.md); and, after the model's own glob so that one cannot bring
// them back, the memory sync's files (memoryGlobs). Every value the model
// chose is one argv word: the pattern after -e and the type and glob joined to
// their flag, so none of them can be read as an option whatever it begins
// with, and the path after "--".
func (in grepInput) query(root, cwd string) (grepQuery, string) {
	q := grepQuery{args: []string{"--no-config", "--no-heading", "--hidden", "--sort=path"}}
	for _, d := range vcsDirs {
		q.args = append(q.args, "--glob=!"+d)
	}
	var why string
	if q.limit, why = whole("head_limit", in.HeadLimit); why != "" {
		return q, why
	}
	if q.skip, why = whole("offset", in.Offset); why != "" {
		return q, why
	}
	q.limit, q.skip = max(q.limit, 0), max(q.skip, 0)

	switch in.OutputMode {
	case "", grepFiles:
		q.args = append(q.args, "-l")
	case grepCount:
		q.args = append(q.args, "-c")
	case grepContent:
		if in.LineNums == nil || *in.LineNums {
			q.args = append(q.args, "-n")
		} else {
			q.args = append(q.args, "-N")
		}
		// The context counts "require output_mode: content, ignored
		// otherwise", as the recorded descriptions say — so outside content
		// mode they are not even read. context and -C are one option under two
		// names; where both are given, context wins. -A and -B go to rg as
		// they came, and rg lets each override -C for its own side whatever
		// the order.
		var counts [4]int
		for i, f := range []struct {
			name string
			v    *float64
		}{{"-A", in.After}, {"-B", in.Before}, {"-C", in.C}, {"context", in.Context}} {
			if counts[i], why = whole(f.name, f.v); why != "" {
				return q, why
			}
		}
		switch {
		case in.Context != nil:
			q.args = append(q.args, "-C", strconv.Itoa(counts[3]))
		case in.C != nil:
			q.args = append(q.args, "-C", strconv.Itoa(counts[2]))
		}
		if counts[0] >= 0 {
			q.args = append(q.args, "-A", strconv.Itoa(counts[0]))
		}
		if counts[1] >= 0 {
			q.args = append(q.args, "-B", strconv.Itoa(counts[1]))
		}
	default:
		return q, fmt.Sprintf("output_mode %q is not one of %q, %q, %q", in.OutputMode, grepContent, grepFiles, grepCount)
	}
	if in.IgnoreCase {
		q.args = append(q.args, "-i")
	}
	if in.Multiline {
		q.args = append(q.args, "-U", "--multiline-dotall")
	}
	if in.Type != "" {
		q.args = append(q.args, "--type="+in.Type)
	}
	if in.Glob != "" {
		q.args = append(q.args, "--glob="+in.Glob)
	}
	q.args = append(q.args, memoryGlobs(root, cwd)...)
	q.args = append(q.args, "-e", in.Pattern, "--", root)
	return q, ""
}

// memoryGlobs keeps the memory sync's own files out of a search. The
// baselines in MemorySyncDir and the marker at each store's root
// (memsync.MarkerName) are the platform's bookkeeping beside the memories, not
// memories, and --hidden would otherwise list them; the memory guidance's own
// grep line leaves them out for the same reason (internal/brain). The marker
// is left out by name, wherever it lies, as that line leaves it out. The
// baselines are one directory, left out where the walk reaches it from a root
// above it; a search rooted at it or inside it searches it, as rg searches a
// hidden directory it is handed.
//
// rg 15.2.0 matches a glob against each path as its walk yields it — the root
// as given, joined with what lies under it — less rg's working directory: the
// override matcher is rooted there, not at the search root, and strips the
// working directory's bytes from the front of a path that begins with them,
// and then one slash (the ignore crate's Gitignore::strip). A glob that begins
// with "/" must match all of what is left. So the glob naming the baselines is
// "/" and their path below cwd where cwd leads it — "/memory/.sync" from /mnt
// — and "/" and their absolute path where it does not: "//mnt/memory/.sync"
// from /workspace. TestGrepLeavesOutTheMemorySyncState holds rg to both.
func memoryGlobs(root, cwd string) []string {
	globs := []string{"--glob=!" + memsync.MarkerName}
	if root == MemorySyncDir || (root != "/" && !under(MemorySyncDir, root)) {
		return globs
	}
	rel := MemorySyncDir
	if rest, ok := strings.CutPrefix(MemorySyncDir, cwd); ok {
		rel = strings.TrimPrefix(rest, "/")
	}
	return append(globs, "--glob=!/"+rel)
}

// ripgrepDir is where grep installs rg in a sandbox: under /tmp, which is
// writable in every hardening shape on both backends (sandbox.WritablePaths —
// an emptyDir or a volume under a read-only root, neither mounted noexec) and
// outside the workdir, so the model's own searches and the checkpoint never
// meet it. A directory of the platform's own rather than /tmp itself: Docker
// lands the upload as root, and /tmp's sticky bit lets only a file's owner,
// the directory's, or root unlink or rename it, so a non-root sandbox user
// could never clear a root-owned upload out of /tmp. Each install instead
// works in a directory of its own under this one, which the sandbox user
// makes, and from a directory it may write that carries no sticky bit,
// removing the upload takes nothing more. The path is the platform's: what an
// install finds there that is not a directory — a link to somewhere else
// included — it removes, and it makes the directory afresh (prepareScript).
const ripgrepDir = "/tmp/.map-ripgrep"

// ripgrepPath is where this build's rg lives in a sandbox. The version is in
// the name, so a sandbox that outlives an upgrade gets the new one beside the
// old rather than running the old.
func ripgrepPath() string { return ripgrepDir + "/rg-" + ripgrep.Pinned.Version }

// The exit codes script claims for itself, outside rg's 0, 1 and 2 and
// below the 126 a shell takes for a command it could not run.
const (
	// exitErrorBesideLines: rg exited 2, an error, after printing lines that
	// paging may have cut away to the last — an offset past the end. That is
	// an error beside an answer, not an error alone (grepAnswer).
	exitErrorBesideLines = 96
	// exitNoRipgrep: rg is not installed — or not runnable, or not this
	// build's — and stdout's last line is ripgrepMissing and the machine.
	exitNoRipgrep = 97
	// exitPager: paging the output failed; stderr says why.
	exitPager = 98
)

const ripgrepMissing = "map-ripgrep-missing "

// grepBeginPrefix opens the line a search prints on stdout and on stderr
// immediately before rg runs (script). Its suffix is a nonce per search
// (newGrepBegin), so no file a model searches can hold the line: rg prints a
// matched line bare from a single file with -n false, and such a line must
// never be read as the frame.
const grepBeginPrefix = "map-grep-begin-"

func newGrepBegin() string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	return grepBeginPrefix + hex.EncodeToString(nonce[:])
}

// openRipgrep is ripgrep.Open, a variable so a test can play a build that
// fetched no binaries.
var openRipgrep = ripgrep.Open

// pagerReader is the paging stage behind head when an offset skips lines: it
// passes its input through untouched, and exits 3 when there is none — rg
// printed nothing — so the script can tell an exit 2 whose every line the
// offset cut away from one that printed nothing at all. It reads the first
// character itself, which bash's read -n takes off a pipe a byte at a time,
// never past it, and writes it back: a newline, which read -n consumes and
// stores as nothing, as a newline. (read -n is bash 2's; the -N that would
// store the newline is bash 4.1's, past what the image contract asks.)
const pagerReader = `{ IFS= read -r -n 1 c || exit 3; if [ -n "$c" ]; then printf '%s' "$c"; else echo; fi; exec cat; }`

// script renders one search: rg, checked first, then run over the query's
// arguments and paged.
//
// The check runs on every call because the sandbox is the model's: it can
// delete the binary, replace it, or have lost it to a restore, and the check
// costs no round trip — it rides in the same exec as the search. It asks the
// binary for its version, which a missing file, one that cannot run, and one
// that is some other program all fail; it is not a defence against a model
// that plants a fake answering the right version, which would be the model
// tampering with its own sandbox, where bash already runs whatever it likes.
// A failed check reports the machine, so the caller can install rg and call
// again; `uname -m` reports it, and bash's own $HOSTTYPE stands in where an
// image ships no uname. The report is the script's last line, printed after a
// newline of its own, and only the last line is read (missingRipgrep).
//
// What reaches stdout or stderr before rg runs is not rg's: an image's `ENV
// BASH_ENV` hook prints first, on either stream, possibly without ending its
// line. So immediately before rg runs the script prints the query's begin
// line on both, each after a newline of its own, and only what follows the
// last of them is read (grepAnswer) — the framing sandbox.bulkLeftBeginMarker
// argues.
//
// head_limit and offset page rg's output lines as the descriptions' "| tail
// -n +N | head -N" does: through head and tail in the sandbox, so a large
// answer is cut where it is made rather than carried out of the sandbox and
// thrown away, and head stops rg once it has its lines (rg exits 0 when the
// pipe closes under it). rg's stderr does not pass through either, so its
// errors survive the paging. An offset can cut away every line rg printed, so
// pagerReader stands before tail, and an exit 2 with lines behind it is
// exitErrorBesideLines. The line counts are summed in int64: each is up to
// 2³¹−1, and a 32-bit int (the worker's linux/arm build) would wrap.
func (q grepQuery) script() string {
	words := make([]string, len(q.args))
	for i, a := range q.args {
		words[i] = singleQuote(a)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `rg=%s
v=
if [ -f "$rg" ] && [ -x "$rg" ]; then v=$("$rg" --version 2>/dev/null); fi
case $v in
%s*) ;;
*) printf '\n%s%%s\n' "$(uname -m 2>/dev/null || printf '%%s' "$HOSTTYPE")"; exit %d ;;
esac
`, singleQuote(ripgrepPath()), singleQuote("ripgrep "+ripgrep.Pinned.Version+" "), ripgrepMissing, exitNoRipgrep)
	var tools, stages []string
	if q.limit > 0 {
		tools = append(tools, "head")
		stages = append(stages, fmt.Sprintf("head -n %d", int64(q.skip)+int64(q.limit)))
	}
	if q.skip > 0 {
		tools = append(tools, "cat", "tail")
		stages = append(stages, pagerReader, fmt.Sprintf("tail -n +%d", int64(q.skip)+1))
	}
	for _, tool := range tools {
		fmt.Fprintf(&b, "command -v %[1]s >/dev/null 2>&1 || { printf 'grep: head_limit and offset need %[1]s in the sandbox image\\n' >&2; exit %[2]d; }\n",
			tool, exitPager)
	}
	fmt.Fprintf(&b, "printf '\\n%%s\\n' %[1]s; printf '\\n%%s\\n' %[1]s >&2\n", singleQuote(q.begin))
	if len(stages) == 0 {
		fmt.Fprintf(&b, "exec \"$rg\" %s\n", strings.Join(words, " "))
		return b.String()
	}
	fmt.Fprintf(&b, "\"$rg\" %s | %s\ns=(\"${PIPESTATUS[@]}\")\n", strings.Join(words, " "), strings.Join(stages, " | "))
	reader := 0
	for i, stage := range stages {
		if stage == pagerReader {
			reader = i + 1
			fmt.Fprintf(&b, "case ${s[%d]} in 0|3) ;; *) exit %d ;; esac\n", reader, exitPager)
			continue
		}
		fmt.Fprintf(&b, "[ \"${s[%d]}\" = 0 ] || exit %d\n", i+1, exitPager)
	}
	if reader > 0 {
		fmt.Fprintf(&b, "[ \"${s[0]}\" = 2 ] && [ \"${s[%d]}\" = 0 ] && exit %d\n", reader, exitErrorBesideLines)
	}
	b.WriteString("exit \"${s[0]}\"\n")
	return b.String()
}

// missingRipgrep reads a search's exit as the check's report that rg is not
// there: the machine, and true, when the script exited exitNoRipgrep and the
// last line of its stdout is the report. Anything before that line — an
// image's banner — is not read, and an exit 97 without the report is not the
// check's.
func missingRipgrep(res sandbox.ExecResult) (string, bool) {
	if res.ExitCode != exitNoRipgrep {
		return "", false
	}
	out := strings.TrimRight(res.Stdout, "\n")
	return strings.CutPrefix(out[strings.LastIndexByte(out, '\n')+1:], ripgrepMissing)
}

// An install works in a directory of its own under ripgrepDir,
// installPrefix<unix seconds>-<nonce>: the probe, the upload, the backend's
// own temporary under it and the copy that becomes rg all land there, and the
// install removes it as it exits. One that never got to — a sandbox exec
// killed at its deadline, an executor that died mid-upload — leaves it
// behind, so every install first removes the ones older than staleInstall.
// The seconds are the platform's clock both times, never the sandbox's, so a
// sandbox clock that is wrong cannot sweep a live install away; staleInstall
// is far past any install's own deadline and covers the clock skew between
// executors. Nothing else in ripgrepDir is touched.
const (
	installPrefix = ".install-"
	staleInstall  = 10 * time.Minute
	// installTimeout bounds each of the install's two execs.
	installTimeout = time.Minute
)

// prepareScript makes the install's directory, after sweeping the stale ones,
// and proves the sandbox can execute a file there before a 5 MB binary is
// carried in: an empty file, which the kernel refuses with EACCES on a
// noexec mount and bash otherwise runs as an empty script, whatever
// interpreters the image has. Exit 1 is a step that failed and said why;
// exit 2 is a sandbox that will not execute a file under ripgrepDir. Either
// way the directory goes again.
//
// The sweep removes directories, so it must never reach past ripgrepDir. A
// link there — the model's, or an image's — would take it to wherever the
// link points, and a stale-looking name there would go; so what stands at
// ripgrepDir and is not a directory is removed (rm on a link removes the link,
// never what it names), the directory is made afresh, and the sweep runs from
// inside it, on names relative to it, once the directory the script stands in
// is shown to be the one at ripgrepDir and that to be no link. A link swapped
// in after that cannot redirect it: a relative name resolves from the
// directory the script stands in, not from the path.
const prepareScript = `d=%[1]s
s=%[2]s
if [ -h "$d" ] || { [ -e "$d" ] && [ ! -d "$d" ]; }; then rm -f -- "$d" || exit 1; fi
mkdir -p -- "$d" && cd -- "$d" || exit 1
if [ -h "$d" ] || [ ! . -ef "$d" ]; then printf '%%s was replaced while ripgrep was being installed\n' "$d" >&2; exit 1; fi
for o in ` + installPrefix + `*; do
  [ -d "$o" ] && [ ! -h "$o" ] || continue
  n=${o#` + installPrefix + `}
  n=${n%%%%-*}
  case $n in ''|*[!0-9]*) continue ;; esac
  [ "$n" -lt %[3]d ] && rm -rf -- "$o"
done
mkdir -- "$s" || exit 1
: > "$s/probe" && chmod 755 -- "$s/probe" || { rm -rf -- "$s"; exit 1; }
m=$("$s/probe" 2>&1)
if [ $? != 0 ]; then rm -rf -- "$s"; printf '%%s\n' "$m" >&2; exit 2; fi
rm -f -- "$s/probe"
`

// installScript lands the uploaded binary as rg and proves it runs. It copies
// rather than renames: Docker writes the upload as root, so on an image that
// does not run as root only a copy the sandbox user makes is one it can mark
// executable. What sits at rg's path and is not a regular file — a directory
// or a link the model made there — is removed first, because mv would move
// the binary into it rather than over it; one that reappears before the move
// lands is reported, its stray copy removed. Exit 1 is a step that failed and
// said why; exit 2 is a binary in place that did not answer as this build's
// rg — a machine the kernel cannot run it on.
const installScript = `s=%[1]s
rg=%[2]s
trap 'rm -rf -- "$s"' EXIT
cat -- "$s/upload" > "$s/rg" && chmod 755 -- "$s/rg" || exit 1
if [ -h "$rg" ] || { [ -e "$rg" ] && [ ! -f "$rg" ]; }; then rm -rf -- "$rg" || exit 1; fi
mv -f -- "$s/rg" "$rg" || exit 1
if [ -d "$rg" ]; then
  rm -f -- "$rg/rg"
  printf '%%s became a directory while ripgrep was being installed\n' "$rg" >&2
  exit 1
fi
v=$("$rg" --version 2>&1)
st=$?
case $st/$v in
0/%[3]s*) ;;
*) printf 'exit %%s: %%s\n' "$st" "$v" >&2; exit 2 ;;
esac
`

// linuxArch names the GOARCH of a sandbox's `uname -m`, or "" for a machine
// no binary is shipped for.
func linuxArch(machine string) string {
	switch machine {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return ""
}

// installRipgrep writes this build's rg for machine into the sandbox and
// checks it runs. A nil Result is success. A non-nil one is the tool error
// grep answers with instead, naming why rg cannot run here — there is no
// second implementation to fall back on (#827, an owner decision). An error is
// the sandbox itself failing.
func (r Runner) installRipgrep(ctx context.Context, machine string) (*Result, error) {
	fail := func(format string, a ...any) (*Result, error) {
		res, _ := failf("grep: "+format, a...)
		return &res, nil
	}
	arch := linuxArch(machine)
	if arch == "" {
		return fail("ripgrep is shipped for linux x86_64 and aarch64, and this sandbox is %q", machine)
	}
	rg, size, err := openRipgrep(arch)
	if err != nil {
		return fail("%v", err)
	}
	now := time.Now()
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	dir := fmt.Sprintf("%s/%s%d-%s", ripgrepDir, installPrefix, now.Unix(), hex.EncodeToString(nonce[:]))

	res, err := r.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Command: fmt.Sprintf(prepareScript, singleQuote(ripgrepDir), singleQuote(dir), now.Add(-staleInstall).Unix()),
		Timeout: installTimeout,
	})
	switch {
	case err != nil:
		return nil, err
	case res.TimedOut:
		return fail("installing ripgrep timed out")
	case res.ExitCode == 2:
		return fail("ripgrep cannot run in this sandbox: it refused to execute a file under %s (%s); grep needs /tmp to be writable and to allow executing files",
			ripgrepDir, strings.TrimSpace(res.Stderr))
	case res.ExitCode != 0:
		return fail("cannot install ripgrep under %s: %s", ripgrepDir, strings.TrimSpace(combine(res)))
	}

	if err := r.Sandbox.WriteFileStream(ctx, dir+"/upload", rg, size); err != nil {
		switch {
		case errors.Is(err, sandbox.ErrNotWritable):
			return fail("cannot install ripgrep under %s: %s", ripgrepDir, notWritableReason(err))
		case errors.Is(err, sandbox.ErrNotDirectory), errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrNotReplaceable):
			return fail("cannot install ripgrep under %s: %v", ripgrepDir, err)
		}
		return nil, err
	}
	res, err = r.Sandbox.Exec(ctx, sandbox.ExecRequest{
		Command: fmt.Sprintf(installScript, singleQuote(dir), singleQuote(ripgrepPath()),
			singleQuote("ripgrep "+ripgrep.Pinned.Version+" ")),
		Timeout: installTimeout,
	})
	switch {
	case err != nil:
		return nil, err
	case res.TimedOut:
		return fail("installing ripgrep timed out")
	case res.ExitCode == 2:
		return fail("ripgrep %s for %s was written to %s but does not run there (%s)",
			ripgrep.Pinned.Version, machine, ripgrepPath(), strings.TrimSpace(res.Stderr))
	case res.ExitCode != 0:
		return fail("cannot install ripgrep at %s: %s", ripgrepPath(), strings.TrimSpace(combine(res)))
	}
	return nil, nil
}

func (r Runner) grep(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in grepInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return failf("invalid grep input: %v", err)
	}
	if in.Pattern == "" {
		return failf("grep: pattern is required")
	}
	for _, f := range [][2]string{{"pattern", in.Pattern}, {"path", in.Path}, {"type", in.Type}, {"glob", in.Glob}} {
		if res, bad := badField("grep", f[0], f[1]); bad {
			return res, nil
		}
	}

	// No absolute-pattern handling here: a grep pattern is a regex, not a path,
	// and one that happens to start with "/" must not be mistaken for an
	// absolute root and turned loose on the whole filesystem.
	root := r.workdir()
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	q, why := in.query(root, r.workdir())
	if why != "" {
		return failf("grep: %s", why)
	}
	// The script is one exec argument, and the pattern, path, type and glob
	// are all in it, the pattern again in rg's own argv.
	q.begin = newGrepBegin()
	script := q.script()
	if len(script) > sandbox.MaxCommandBytes {
		return failf("grep: the pattern, path, type and glob make a %d-byte command, over the %d bytes one exec argument can carry; shorten them",
			len(script), sandbox.MaxCommandBytes)
	}
	for installed := false; ; installed = true {
		res, err := r.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: script, Timeout: DefaultTimeout})
		if err != nil {
			return Result{}, err
		}
		if res.TimedOut {
			return failf("grep: timed out after %s", DefaultTimeout)
		}
		machine, missing := missingRipgrep(res)
		if !missing {
			return grepAnswer(res, q.begin)
		}
		if installed {
			return failf("grep: ripgrep was installed at %s and was gone again before the search ran", ripgrepPath())
		}
		if bad, err := r.installRipgrep(ctx, machine); bad != nil || err != nil {
			if err != nil {
				return Result{}, err
			}
			return *bad, nil
		}
	}
}

// grepAnswer reads a search: rg's output, what follows the last begin line on
// stdout, and its messages, what follows it on stderr (script). rg exits 0
// for matches, 1 for none — or for a search head cut short, which rg may also
// report as 1 — and 2 for an error. An error beside an answer — one
// unreadable file among the matches, say — is still the answer, with rg's
// messages after it, as the reference's GrepTool keeps what rg found when it
// exits 2 (docs/DIVERGENCES.md), and so is an error beside lines an offset cut
// away (exitErrorBesideLines), whose answer is "no matches", as any page past
// the end is; an error with nothing printed, and any other exit, is a failure
// whose message is rg's own. A message rg printed beside a successful answer,
// such as an ignore file it could not parse, follows it the same way rather
// than being lost.
//
// Output with no begin line never reached rg. A script that stopped short of
// it — paging tools missing — says why itself; an exit that claims to be rg's
// without one — a shell that exited before the script ran, or an image that
// printed past the output cap first — is a failure, never an empty answer.
func grepAnswer(res sandbox.ExecResult, begin string) (Result, error) {
	out, framed := afterBegin(res.Stdout, begin)
	if !framed {
		switch res.ExitCode {
		case 0, 1, 2, exitErrorBesideLines:
			msg := fmt.Sprintf("grep: no answer from rg reached the output (exit %d): the sandbox's shell exited, or filled the output cap, before the search ran", res.ExitCode)
			if more := strings.TrimSpace(combine(res)); more != "" {
				msg += "\n" + more
			}
			return failf("%s", msg)
		}
		return searchFailure("grep", res)
	}
	msg, _ := afterBegin(res.Stderr, begin)
	msg = strings.TrimSpace(msg)
	out = strings.TrimRight(out, "\n")
	switch {
	case res.ExitCode == 0, res.ExitCode == 1, res.ExitCode == exitErrorBesideLines:
	case res.ExitCode == 2 && strings.TrimSpace(out) != "":
	default:
		return searchFailure("grep", sandbox.ExecResult{Stdout: out, Stderr: msg, ExitCode: res.ExitCode, Truncated: res.Truncated})
	}
	if out == "" {
		out = "no matches"
	}
	// The sandbox's own per-stream cap may already have cut this stream; the
	// marker must ride along, or a spill of it would read as the full result.
	if res.Truncated {
		out = truncationNotice + "\n" + out
	}
	if msg != "" {
		out += "\n" + msg
	}
	return succeed(out)
}

// afterBegin returns what follows the last line of s that is begin, and true;
// or s, and false, when no line is.
func afterBegin(s, begin string) (string, bool) {
	line := "\n" + begin + "\n"
	t := "\n" + s
	i := strings.LastIndex(t, line)
	if i < 0 {
		return s, false
	}
	return t[i+len(line):], true
}
