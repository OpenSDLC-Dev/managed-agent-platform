package toolset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
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

// grepQuery is a validated grepInput: rg's arguments, the directory rg runs
// in, the paging rg does not do itself, and the frame the search's script
// prints around what it says (newSearchFrame).
type grepQuery struct {
	args        []string
	cwd         string
	skip, limit int
	frame       sandbox.Frame
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
// names, searching root from cwd, the directory the script enters before rg
// runs, so the one memoryGlobs spells the baselines' glob from is the one rg
// matches it in. The rest is ours and fixed: --no-config, so an image's
// RIPGREP_CONFIG_PATH cannot change what a call means; --no-heading, so
// every line carries its file; --hidden with the version-control directories
// globbed out, as the reference's GrepTool searches (vcsDirs) — .gitignore
// and the other ignore files rg reads still apply; --sort=path, so an answer
// lists in one order run after run, and offset pages through that order
// whether or not the call that came before paged — which costs rg its
// parallel search, as sorting does (docs/DIVERGENCES.md); and, after the
// model's own glob so that one cannot bring them back, the memory sync's
// files (memoryGlobs). Every value the model chose is one argv word: the
// pattern after -e and the type and glob joined to their flag, so none of
// them can be read as an option whatever it begins with, and the path after
// "--".
func (in grepInput) query(root, cwd string) (grepQuery, string) {
	q := grepQuery{args: []string{"--no-config", "--no-heading", "--hidden", "--sort=path"}, cwd: cwd}
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
// That keeps a search that meets them in passing clean; it is not a boundary.
// A path that names them is searched — the marker file itself, the baselines'
// directory, or the same tree by another name, a symbolic link to /mnt/memory
// or /proc/self/root/mnt/memory — as is everything else in the model's own
// sandbox, which bash reads all the same.
//
// rg 15.2.0 matches a glob against each path as its walk yields it — the root
// as given, joined with what lies under it — less rg's working directory: the
// override matcher is rooted there, not at the search root, and strips the
// working directory's bytes from the front of a path that begins with them,
// and then one slash (the ignore crate's Gitignore::strip). A glob that begins
// with "/" must match all of what is left. So the glob naming the baselines is
// "/" and their path below cwd where cwd leads it — "/memory/.sync" from /mnt
// — and "/" and their absolute path where it does not: "//mnt/memory/.sync"
// from /workspace. rg takes its working directory from the kernel, so cwd is
// the Runner's workdir cleaned — no trailing slash, no ".." — and the script
// enters it before rg runs (grepQuery.script), whatever directory the exec
// started in. Only a workdir whose path runs through a symbolic link can still
// part the two, the kernel naming it by where the link leads: the tree named
// another way, as above. TestGrepLeavesOutTheMemorySyncState holds rg to the
// rest.
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
// below the 126 a shell takes for a command it could not run. A paged
// search's exit says what its page holds, in rg's own terms where they fit —
// 0 for a page with lines on it, 1 for one without, whether rg found none or
// an offset paged past them — and in the first two of these for rg's 2.
const (
	// exitErrorBesideEmptyPage: rg exited 2, an error, after printing lines
	// an offset paged away to the last — no matches on this page, beside the
	// error (grepAnswer).
	exitErrorBesideEmptyPage = 95
	// exitErrorBesideLines: rg exited 2 after printing lines, and the page
	// holds some of them — an error beside an answer, not an error alone.
	exitErrorBesideLines = 96
	// exitNoRipgrep: rg is not installed — or not runnable, or not this
	// build's — and the script's framed stdout is ripgrepMissing and the
	// machine.
	exitNoRipgrep = 97
	// exitStopped: the script stopped on a step of its own — the directory rg
	// runs in could not be entered, a paging tool is missing, or paging
	// failed — and its framed stderr says why.
	exitStopped = 98
)

const ripgrepMissing = "map-ripgrep-missing "

// openRipgrep is ripgrep.Open, a variable so a test can play a build that
// fetched no binaries.
var openRipgrep = ripgrep.Open

// pagerReader is the paging stage that says whether anything reached it: it
// passes its input through untouched, and exits 3 when there is none. Behind
// head it tells an exit 2 whose every line an offset cut away from one that
// printed nothing at all; at the end of the pipeline it tells an empty page
// from one that holds lines — a lone empty line among them. It reads the first
// character itself, which bash's read -n takes off a pipe a byte at a time,
// never past it, and writes it back: a newline, which read -n consumes and
// stores as nothing, as a newline. (read -n is bash 2's; the -N that would
// store the newline is bash 4.1's, past what the image contract asks.)
const pagerReader = `{ IFS= read -r -n 1 c || exit 3; if [ -n "$c" ]; then printf '%s' "$c"; else echo; fi; exec cat; }`

// script renders one search: the frame opened (Frame.Open), the
// directory rg runs in entered, rg checked, then run over the query's
// arguments and paged, and the frame closed as the script exits, whichever way
// it does (close_frame).
//
// The check runs on every call because the sandbox is the model's: it can
// delete the binary, replace it, or have lost it to a restore, and the check
// costs no round trip — it rides in the same exec as the search. It asks the
// binary for its version, which a missing file, one that cannot run, and one
// that is some other program all fail; it is not a defence against a model
// that plants a fake answering the right version, which would be the model
// tampering with its own sandbox, where bash already runs whatever it likes.
// A failed check reports the machine, so the caller can install rg and call
// again. The machine bash was built for names it (compiledMachine), read in
// the script's own process: no child prints it, so nothing the loader runs in
// a child — an `ENV LD_PRELOAD` library's constructor printing a banner — can
// corrupt it, and no environment sets it, as an image's `ENV` or a vault
// credential can set $HOSTTYPE. `uname -m` is asked only where that machine
// is none rg is shipped for — a 32-bit bash on a 64-bit kernel, whose kernel
// may run rg all the same — and the compiled machine stands again where the
// image ships no uname. That fallback is a child such a banner reaches inside
// the frame, as it reaches every child a platform script runs — a limit the
// image contract states rather than one the frame closes (sandbox.Frame). The
// report is the whole of the framed stdout (missingRipgrep).
//
// head_limit and offset page rg's output lines as the descriptions' "| tail
// -n +N | head -N" does: through head and tail in the sandbox, so a large
// answer is cut where it is made rather than carried out of the sandbox and
// thrown away, and head stops rg once it has its lines (rg exits 0 when the
// pipe closes under it). rg's stderr does not pass through either, so its
// errors survive the paging. pagerReader stands behind head when an offset
// skips lines and at the end of every paged pipeline, and the script folds
// what they saw into its exit: 1 for an empty page whatever rg's 0 or 1, 0
// for one with lines, and for rg's 2 the exit that says whether it printed
// any and whether the page kept them (exitErrorBesideLines,
// exitErrorBesideEmptyPage). So whether there are matches is the exit's
// answer, never the output's length. The line counts are summed in int64:
// each is up to 2³¹−1, and a 32-bit int (the worker's linux/arm build) would
// wrap.
func (q grepQuery) script() string {
	words := make([]string, len(q.args))
	for i, a := range q.args {
		words[i] = singleQuote(a)
	}
	var b strings.Builder
	b.WriteString(q.frame.Open())
	fmt.Fprintf(&b, `cd -- %[1]s >/dev/null || close_frame %[2]d
rg=%[3]s
v=
if [ -f "$rg" ] && [ -x "$rg" ]; then v=$("$rg" --version 2>/dev/null); fi
case $v in
%[4]s*) ;;
*) mt=%[7]s; m=${mt%%%%-*}
  case $m in x86_64|amd64|aarch64|arm64) ;; *) m=$(uname -m 2>/dev/null) || m=${mt%%%%-*} ;; esac
  printf '%[5]s%%s\n' "$m"; close_frame %[6]d ;;
esac
`, singleQuote(q.cwd), exitStopped,
		singleQuote(ripgrepPath()), singleQuote("ripgrep "+ripgrep.Pinned.Version+" "), ripgrepMissing, exitNoRipgrep,
		compiledMachine)
	var tools, stages []string
	if q.limit > 0 {
		tools = append(tools, "head")
		stages = append(stages, fmt.Sprintf("head -n %d", int64(q.skip)+int64(q.limit)))
	}
	if q.skip > 0 {
		tools = append(tools, "tail")
		stages = append(stages, pagerReader, fmt.Sprintf("tail -n +%d", int64(q.skip)+1))
	}
	if len(stages) == 0 {
		fmt.Fprintf(&b, "\"$rg\" %s\nclose_frame \"$?\"\n", strings.Join(words, " "))
		return b.String()
	}
	tools = append(tools, "cat")
	stages = append(stages, pagerReader)
	for _, tool := range tools {
		fmt.Fprintf(&b, "command -v %[1]s >/dev/null 2>&1 || { printf 'grep: head_limit and offset need %[1]s in the sandbox image\\n' >&2; close_frame %[2]d; }\n",
			tool, exitStopped)
	}
	fmt.Fprintf(&b, "\"$rg\" %s | %s\ns=(\"${PIPESTATUS[@]}\")\n", strings.Join(words, " "), strings.Join(stages, " | "))
	printed := 0 // the first reader: did rg print anything at all
	for i, stage := range stages {
		if stage == pagerReader {
			if printed == 0 {
				printed = i + 1
			}
			fmt.Fprintf(&b, "case ${s[%d]} in 0|3) ;; *) close_frame %d ;; esac\n", i+1, exitStopped)
			continue
		}
		fmt.Fprintf(&b, "[ \"${s[%d]}\" = 0 ] || close_frame %d\n", i+1, exitStopped)
	}
	page := len(stages) // the last reader: did the page keep any of it
	fmt.Fprintf(&b, `case ${s[0]} in
0|1) [ "${s[%[2]d]}" = 3 ] && close_frame 1; close_frame 0 ;;
2) [ "${s[%[1]d]}" = 3 ] && close_frame 2; [ "${s[%[2]d]}" = 3 ] && close_frame %[3]d; close_frame %[4]d ;;
*) close_frame "${s[0]}" ;;
esac
`, printed, page, exitErrorBesideEmptyPage, exitErrorBesideLines)
	return b.String()
}

// missingRipgrep reads a search's exit as the check's report that rg is not
// there: the machine, and true, when the script exited exitNoRipgrep and its
// framed stdout is the report and nothing else. What lies outside the frame —
// an image's banner, a forged report — is not read, and an exit 97 without
// the report is not the check's.
func missingRipgrep(res sandbox.ExecResult, f sandbox.Frame) (string, bool) {
	if res.ExitCode != exitNoRipgrep {
		return "", false
	}
	out, ok, _ := f.Cut(res.Stdout, res.StdoutTruncated)
	if !ok {
		return "", false
	}
	machine, report := strings.CutPrefix(out, ripgrepMissing)
	machine, whole := strings.CutSuffix(machine, "\n")
	if !report || !whole || strings.Contains(machine, "\n") {
		return "", false
	}
	return machine, true
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
// never what it names), the directory is made afresh, and the script enters
// it (enterRipgrepDir) and works from inside it, on names relative to it. A
// link swapped in after that cannot redirect the script: a relative name
// resolves from the directory the script stands in, not from the path. The
// install's own directory is made fresh — mkdir refuses a name already there
// — so the upload is aimed at one this install made; but the upload is the
// platform's write by absolute path, so a link a sandbox process swaps in at
// ripgrepDir or at that directory before it lands takes the binary wherever
// the link points inside the sandbox. That is no boundary: the model writes
// anywhere in its own sandbox itself. The install after it refuses all the
// same (installScript).
const prepareScript = `d=%[1]s
s=%[2]s
if [ -h "$d" ] || { [ -e "$d" ] && [ ! -d "$d" ]; }; then rm -f -- "$d" || exit 1; fi
mkdir -p -- "$d" || exit 1
` + enterRipgrepDir + `for o in ` + installPrefix + `*; do
  [ -d "$o" ] && [ ! -h "$o" ] || continue
  n=${o#` + installPrefix + `}
  n=${n%%%%-*}
  case $n in ''|*[!0-9]*) continue ;; esac
  [ "$n" -lt %[3]d ] && rm -rf -- "$o"
done
mkdir -- "$s" || exit 1
: > "$s/probe" && chmod 755 -- "$s/probe" || { rm -rf -- "$s"; exit 1; }
m=$("./$s/probe" 2>&1)
if [ $? != 0 ]; then rm -rf -- "$s"; printf '%%s\n' "$m" >&2; exit 2; fi
rm -f -- "$s/probe"
`

// enterRipgrepDir enters ripgrepDir ($d) and proves the directory the script
// then stands in is the one there and that no link: a script that goes on to
// name only what lies inside it, relatively, can then reach nothing outside
// it, whatever is swapped in at the path afterwards.
const enterRipgrepDir = `cd -- "$d" || exit 1
if [ -h "$d" ] || [ ! . -ef "$d" ]; then printf '%%s was replaced while ripgrep was being installed\n' "$d" >&2; exit 1; fi
`

// installScript lands the uploaded binary as rg and proves it runs, from
// inside ripgrepDir as prepareScript works (enterRipgrepDir), on names
// relative to it: the install's own directory, which must still be the
// directory prepare made rather than a link the model swapped in while the
// binary was carried in, and rg's own name. So the EXIT trap that removes the
// install's directory, and every step before it, acts inside ripgrepDir and
// nowhere else. It copies rather than renames: Docker writes the upload as
// root, so on an image that does not run as root only a copy the sandbox user
// makes is one it can mark executable. What sits at rg's path and is not a
// regular file — a directory or a link the model made there — is removed
// first, because mv would move the binary into it rather than over it; one
// that reappears before the move lands is reported, its stray copy removed.
// Exit 1 is a step that failed and said why; exit 2 is a binary in place that
// did not answer as this build's rg — a machine the kernel cannot run it on.
const installScript = `d=%[1]s
s=%[2]s
rg=%[3]s
` + enterRipgrepDir + `if [ -h "$s" ] || [ ! -d "$s" ]; then printf '%%s/%%s was replaced while ripgrep was being installed\n' "$d" "$s" >&2; exit 1; fi
trap 'rm -rf -- "$s"' EXIT
cat -- "$s/upload" > "$s/rg" && chmod 755 -- "$s/rg" || exit 1
if [ -h "$rg" ] || { [ -e "$rg" ] && [ ! -f "$rg" ]; }; then rm -rf -- "$rg" || exit 1; fi
mv -f -- "$s/rg" "$rg" || exit 1
if [ -d "$rg" ]; then
  rm -f -- "$rg/rg"
  printf '%%s/%%s became a directory while ripgrep was being installed\n' "$d" "$rg" >&2
  exit 1
fi
v=$("./$rg" --version 2>&1)
st=$?
case $st/$v in
0/%[4]s*) ;;
*) printf 'exit %%s: %%s\n' "$st" "$v" >&2; exit 2 ;;
esac
`

// compiledMachine is the shell word grep's script reads the sandbox's
// machine from (grepQuery.script): ${BASH_VERSINFO[5]}, the machine type
// compiled into bash — x86_64-pc-linux-gnu, aarch64-unknown-linux-gnu,
// x86_64-alpine-linux-musl — whose machine is what comes before its first
// '-'. bash sets the array itself as it starts and makes it read-only, so no
// environment variable reaches it, where the $HOSTTYPE and $MACHTYPE an
// image's `ENV` or a vault credential sets are taken over bash's own. It is a
// variable so that a test can play a bash built for a machine rg is not
// shipped for (export_test.go).
var compiledMachine = `"${BASH_VERSINFO[5]}"`

// linuxArch names the GOARCH of a sandbox's machine — the one its bash was
// built for, or `uname -m` (grepQuery.script) — or "" for a machine no binary
// is shipped for.
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
	name := fmt.Sprintf("%s%d-%s", installPrefix, now.Unix(), hex.EncodeToString(nonce[:]))

	res, err := sandbox.ExecScript(ctx, r.Sandbox, sandbox.ExecRequest{
		Command: fmt.Sprintf(prepareScript, singleQuote(ripgrepDir), singleQuote(name), now.Add(-staleInstall).Unix()),
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

	if err := r.Sandbox.WriteFileStream(ctx, ripgrepDir+"/"+name+"/upload", rg, size); err != nil {
		switch {
		case errors.Is(err, sandbox.ErrNotWritable):
			return fail("cannot install ripgrep under %s: %s", ripgrepDir, notWritableReason(err))
		case errors.Is(err, sandbox.ErrNotDirectory), errors.Is(err, sandbox.ErrIsDirectory), errors.Is(err, sandbox.ErrNotReplaceable):
			return fail("cannot install ripgrep under %s: %v", ripgrepDir, err)
		}
		return nil, err
	}
	res, err = sandbox.ExecScript(ctx, r.Sandbox, sandbox.ExecRequest{
		Command: fmt.Sprintf(installScript, singleQuote(ripgrepDir), singleQuote(name), singleQuote(path.Base(ripgrepPath())),
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
	// absolute root and turned loose on the whole filesystem. The root is clean
	// either way — resolve cleans a path, and the workdir is cleaned here — as
	// memoryGlobs compares it to MemorySyncDir by its path elements: "/mnt/" or
	// "/mnt/x/.." would otherwise not lead it.
	cwd := path.Clean(r.workdir())
	root := cwd
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	q, why := in.query(root, cwd)
	if why != "" {
		return failf("grep: %s", why)
	}
	// The script is one exec argument, and the pattern, path, type and glob
	// are all in it, the pattern again in rg's own argv: Exec refuses one
	// past sandbox.MaxCommandBytes before it runs, which is the model's to
	// shorten (searchExec). The install's scripts carry none of them.
	q.frame = newSearchFrame()
	script := q.script()
	for installed := false; ; installed = true {
		res, err := r.searchExec(ctx, script, "the pattern, path, type and glob")
		if err != nil {
			return Result{}, err
		}
		if res.TimedOut {
			return failf("grep: timed out after %s", DefaultTimeout)
		}
		machine, missing := missingRipgrep(res, q.frame)
		if !missing {
			return grepAnswer(res, q.frame)
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

// grepAnswer reads a search: rg's output, the script's framed stdout, and its
// messages, the framed stderr (Frame.Cut). Whether there are matches is
// the exit's to say (script): 0 is an answer, whatever its length — a lone
// empty line one too, which reads back as no text at all — and 1 is "no
// matches". An error beside an answer — one unreadable file among the
// matches, say — is still the answer, with rg's messages after it, as the
// reference's GrepTool keeps what rg found when it exits 2 (docs/
// DIVERGENCES.md): unpaged, an exit 2 after rg printed anything at all;
// paged, exitErrorBesideLines, and exitErrorBesideEmptyPage, whose answer is
// "no matches", as any page past the end is. An error with nothing printed,
// and any other exit, is a failure whose message is rg's own — or the
// script's, where it stopped on a step of its own (exitStopped). A message rg
// printed beside a successful answer, such as an ignore file it could not
// parse, follows it the same way rather than being lost.
//
// Output the script did not print to its end — no begin line on stdout, a
// shell that exited before the script ran or an image that printed past the
// output cap first, or no end line on a stdout the cap did not cut — is a
// failure carrying what the sandbox printed, never an answer. Each stream is
// read by its own cut (sandbox.ExecResult's per-stream flags): a stderr flood
// neither marks a whole answer as cut nor lets one without its end line
// through. A stderr without its frame is none of rg's and is left out: only
// the cap takes a begin line that the script printed first, and with it
// whatever came after. What the cap cut of the script's says so, where it
// cut: rg's messages the cap cut between their begin and end lines carry the
// notice after them, beside an answer and in a failure alike
// (messages); an answer the cap cut before its end line carries
// it in front, and so does a failure whose output it cut (searchFailure);
// and messages it took whole, begin line and all, are said after an answer
// and in front of a failure. A stream it cut only after its end line — an
// EXIT trap's flood — or inside the end line's nonce lost nothing of the
// script's, and says nothing.
func grepAnswer(res sandbox.ExecResult, f sandbox.Frame) (Result, error) {
	out, framed, short := f.Cut(res.Stdout, res.StdoutTruncated)
	if !framed {
		return unframed("grep", res)
	}
	msg, lost := messages(f, res)
	printed := out != ""
	// rg ends every line it prints; the last one's newline is not the answer's.
	out = strings.TrimSuffix(out, "\n")
	switch {
	case res.ExitCode == 0, res.ExitCode == exitErrorBesideLines, res.ExitCode == 2 && printed:
	case res.ExitCode == 1, res.ExitCode == exitErrorBesideEmptyPage:
		out = "no matches"
	default:
		return searchFailure("grep", out, msg, res.ExitCode, short || lost)
	}
	// The sandbox's own per-stream cap may already have cut the answer; the
	// marker must ride along, or a spill of it would read as the full result.
	// A cap that cut the stream only after its end line cut no answer.
	if short {
		out = truncationNotice + "\n" + out
	}
	// Messages the cap took whole, begin line and all, are said where they
	// would have followed: rg may have said something no answer shows.
	if lost {
		msg = truncationNotice
	}
	if msg != "" {
		out += "\n" + msg
	}
	return succeed(out)
}
