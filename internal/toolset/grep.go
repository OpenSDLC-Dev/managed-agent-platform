package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

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

// grepQuery is a validated grepInput, in the terms the script takes.
type grepQuery struct {
	mode          string
	lineNums      bool
	before, after int
	skip, limit   int
	ignoreCase    bool
	// pattern is the line-oriented search's; ml, when multiline is set, the
	// multiline search's (greppattern.go).
	pattern   string
	multiline bool
	ml        multiline
	// sel and prune are find's file test and directory-prune test, from the
	// type and the glob (grepglob.go).
	sel, prune []string
}

// whole reads one of the schema's number options: a whole number from 0 to
// 2³¹−1, or -1 when it is absent.
func whole(name string, v *float64) (int, string) {
	if v == nil {
		return -1, ""
	}
	if *v < 0 || *v != math.Trunc(*v) || *v > math.MaxInt32 {
		return 0, fmt.Sprintf("%s must be a whole number from 0 to %d, not %v", name, math.MaxInt32, *v)
	}
	return int(*v), ""
}

// query validates the input's twelve options and resolves their defaults —
// the recorded schema's, each named in its description. anchor is where an
// anchored glob is anchored (globAnchor).
func (in grepInput) query(anchor string) (grepQuery, string) {
	q := grepQuery{mode: in.OutputMode, lineNums: true, ignoreCase: in.IgnoreCase, pattern: in.Pattern}
	switch q.mode {
	case "":
		q.mode = grepFiles
	case grepContent, grepFiles, grepCount:
	default:
		return q, fmt.Sprintf("output_mode %q is not one of %q, %q, %q", in.OutputMode, grepContent, grepFiles, grepCount)
	}
	if in.LineNums != nil {
		q.lineNums = *in.LineNums
	}
	var why string
	if q.limit, why = whole("head_limit", in.HeadLimit); why != "" {
		return q, why
	}
	if q.skip, why = whole("offset", in.Offset); why != "" {
		return q, why
	}
	q.limit, q.skip = max(q.limit, 0), max(q.skip, 0)
	// The context counts "require output_mode: content, ignored otherwise",
	// as the recorded descriptions say — so outside content mode they are not
	// even read.
	if q.mode == grepContent {
		var counts [4]int
		for i, f := range []struct {
			name string
			v    *float64
		}{{"-A", in.After}, {"-B", in.Before}, {"-C", in.C}, {"context", in.Context}} {
			if counts[i], why = whole(f.name, f.v); why != "" {
				return q, why
			}
		}
		// context and -C are one option under two names; where both are
		// given, context wins. -A and -B then override it for their own
		// side, as rg's do whichever order the flags come in.
		base := max(counts[2], 0)
		if counts[3] >= 0 {
			base = counts[3]
		}
		q.after, q.before = base, base
		if counts[0] >= 0 {
			q.after = counts[0]
		}
		if counts[1] >= 0 {
			q.before = counts[1]
		}
	}

	if in.Multiline {
		q.ml, q.multiline = multilinePatterns(in.Pattern)
	} else if strings.Contains(in.Pattern, "\n") {
		// rg's own refusal, which grep -P would answer with a stranger one and
		// grep -E would not give at all, reading the lines as two patterns.
		return q, `the literal "\n" is not allowed in a regex; set multiline to match across lines`
	}

	var types []string
	if in.Type != "" {
		var ok bool
		if types, ok = grepTypes[in.Type]; !ok {
			return q, fmt.Sprintf("unrecognized file type %q; type takes ripgrep's type names, such as go, py, js, ts, rust or java", in.Type)
		}
	}
	rule, err := compileGlob(in.Glob, anchor)
	if err != nil {
		return q, err.Error()
	}
	q.sel, q.prune = selection(types, rule)
	return q, ""
}

// findPath is how a resolved path reaches find, grep and awk: an absolute one
// as it is, and a relative one — a relative workdir makes them so — behind
// "./", so that a name beginning with "-" is never read as an option or as a
// find expression.
func findPath(p string) string {
	if path.IsAbs(p) || p == "." {
		return p
	}
	return "./" + p
}

// globAnchor is the ERE for the prefix rg strips from a path before matching
// a glob against it — its working directory, which here is the workdir — or ""
// for a root outside the workdir, whose paths rg matches as walked.
func globAnchor(root, workdir string) string {
	under := root == workdir || strings.HasPrefix(root, strings.TrimSuffix(workdir, "/")+"/") ||
		(workdir == "." && !path.IsAbs(root))
	if !under {
		return ""
	}
	return regexp.QuoteMeta(strings.TrimSuffix(findPath(workdir), "/")) + "/"
}

// grepScript is one search, over the image's own grep: PCRE where it has it — a
// model writes \d and \b far more readily than their POSIX spellings — and ERE
// where it does not. The probe tells the two apart by exit code: a grep with
// PCRE finds nothing in /dev/null and exits 1, one without it rejects -P and
// exits 2.
//
// The model's pattern, path, glob and type reach it only as the values of
// variables and array elements, each single-quoted by the renderer, and from
// there only as argv: nothing it carries is ever read as code.
//
// The files come from find, under the C locale so that ? and a class match a
// byte, as rg's globs do: every regular file under the root that is not
// hidden, outside node_modules, and admitted by type and glob, with rg's
// precedence (grepglob.go's selection). A root that is a file is searched as
// given, whatever type and glob say, as rg searches a path it is handed — and
// a binary one is searched too, its content answer rg's one-line notice.
// xargs runs grep over the files in batches, through a wrapper that turns
// grep's "no match" exit into success.
//
// head_limit stops the work, not just the output. The pager (page) exits once
// it has its lines and removes the flag file MAPSTOP names; every stage
// upstream then dies of SIGPIPE at its next write — grep writes line-buffered
// — or, writing nothing more, finds the flag gone before its next batch or
// file. The wrapper turns its grep's SIGPIPE into an exit of 255, on which
// xargs stops at once and exits 124, while any other failure kills the
// wrapper and xargs exits 125. So every stage's status is read from
// PIPESTATUS and must be 0, 141 (SIGPIPE) or xargs's 124; anything else fails
// the call, never reaching the model as a short answer or as "no matches".
//
// Context output separates its groups with "--" across files as within one;
// grep does that within a batch, and wrapsep puts one before every batch's
// output, the first of which the pager drops.
//
// multiline runs rg -U's search where the pattern can match a newline
// (greppattern.go), each file read as one record (-z). A file list needs
// nothing more; content runs grep -o over each matching file with scan, whose
// records include its empty matches, and grepAwk prints the lines they touch
// as rg -U's searcher and printer do; count runs first beside it, whose
// records lack the empty ones, so grepAwk can tell them apart. Each stream
// carries its grep's exit status and message as its last record, so the
// merge fails when a grep did. The streams are process substitutions holding
// none of the exec's stdio: one cut off by the pager runs on, unwaited, only
// to its next write. mawk reads a pipe a block at a time unless -W
// interactive, which content uses when paging, so that the page is not
// waiting on a block. -z also turns off grep's binary check, so texts drops
// binary files first.
const grepScript = `IFS=
root=__ROOT__
pat=__PAT__
mlfirst=__MLFIRST__
mlany=__MLANY__
mllisted=__MLLISTED__
mlscan=__MLSCAN__
mlend=__MLEND__
mlawk=__MLAWK__
mode=__MODE__
ml=__ML__
skip=__SKIP__
limit=__LIMIT__
num=__NUM__
before=__BEFORE__
after=__AFTER__
ci=(__CI__)
prune=(__PRUNE__)
sel=(__SEL__)
need=(find xargs head tail tr wc)
if [ "$ml" = 1 ] && [ "$mode" != files_with_matches ]; then need+=(awk); fi
for t in "${need[@]}"; do
  command -v "$t" >/dev/null 2>&1 || { printf 'grep: %s not found in the sandbox image\n' "$t" >&2; exit 2; }
done
flavor=-P
grep -qP -- '' /dev/null 2>/dev/null
if [ "$?" -ge 2 ]; then flavor=-E; fi
if [ "$ml" = 1 ] && [ "$flavor" != -P ]; then
  printf 'grep: multiline needs a grep with PCRE (-P), and this sandbox image has none\n' >&2; exit 2
fi
if [ ! -e "$root" ]; then printf 'grep: %s: No such file or directory\n' "$root" >&2; exit 2; fi
if [ "$ml" = 1 ]; then
  err=$(grep -qzP "${ci[@]}" -e "$mlfirst" -- /dev/null 2>&1)
else
  err=$(grep -q "$flavor" "${ci[@]}" -e "$pat" -- /dev/null 2>&1)
fi
if [ "$?" -ge 2 ]; then printf '%s\n' "$err" >&2; exit 2; fi
fn=-h bin=-a
if [ -d "$root" ]; then fn=-H bin=-I; fi
sep=0
if [ "$mode" = content ] && [ "$fn" = -H ] && [ "$((before + after))" -gt 0 ]; then sep=1; fi
hd=(cat) tl=(cat) lb=() aw=(awk) stop=
if [ "$limit" -gt 0 ]; then
  hd=(page) lb=(--line-buffered)
  stop=$(mktemp 2>/dev/null) || stop=
  trap '[ -z "$stop" ] || rm -f -- "$stop"' EXIT
  case $(awk -W version 2>&1 </dev/null) in mawk*) aw=(awk -W interactive) ;; esac
fi
if [ "$((sep + skip))" -gt 0 ]; then tl=(tail -n "+$((sep + skip + 1))"); fi
export MAPSTOP=$stop
page() {
  head -n "$((sep + skip + limit))"
  local s=$?
  [ -z "$stop" ] || rm -f -- "$stop"
  return "$s"
}
stopped() { [ -n "$MAPSTOP" ] && [ ! -e "$MAPSTOP" ]; }
wrap='if [ -n "$MAPSTOP" ] && [ ! -e "$MAPSTOP" ]; then exit 255; fi
grep "$@"; s=$?
case $s in 0|1) exit 0 ;; 141) exit 255 ;; esac
kill -TERM "$$"; exit 2'
wrapsep='if [ -n "$MAPSTOP" ] && [ ! -e "$MAPSTOP" ]; then exit 255; fi
grep "$@" | { IFS= read -r l || exit 0; printf -- "--\n%s\n" "$l" || exit 2; exec cat; }
set -- "${PIPESTATUS[@]}"
case $1 in 0|1|141) ;; *) kill -TERM "$$"; exit 2 ;; esac
case $2 in 0|141) ;; *) kill -TERM "$$"; exit 2 ;; esac
if [ "$1" = 141 ] || [ "$2" = 141 ]; then exit 255; fi
exit 0'
each() { xargs -0 -r bash -c "$wrap" grep "$@"; }
eachsep() { xargs -0 -r bash -c "$wrapsep" grep "$@"; }
files() {
  if [ "$fn" = -h ]; then printf '%s\0' "$root"; return; fi
  LC_ALL=C find -H "$root" -mindepth 1 -regextype posix-extended \
    -type d \( "${prune[@]}" \) -prune -o -type f \( "${sel[@]}" \) -print0
}
texts() {
  if [ "$fn" = -h ]; then cat; else each -IlZ -e '' --; fi
}
nonzero() {
  grep "${lb[@]}" -av -E '(^|:)0$'
  local s=$?
  if [ "$s" -le 1 ]; then return 0; fi
  return "$s"
}
pipeok() {
  for s in "$@"; do
    case $s in 0|141) ;; *) return 1 ;; esac
  done
}
binary() {
  local has at
  has=$(LC_ALL=C tr -dc '\000' < "$root" | head -c1 | wc -c; pipeok "${PIPESTATUS[@]}") || return 2
  if [ "$has" -eq 0 ]; then return 1; fi
  at=$(LC_ALL=C tr '\000\n' '\n\000' < "$root" | head -n1 | wc -c; pipeok "${PIPESTATUS[@]}") || return 2
  printf '%d\n' "$((at - 1))"
}
stream() {
  exec </dev/null 2>/dev/null
  { err=$(grep -obzaP "${ci[@]}" -e "$1" -- "$f" 2>&1 >&3); printf 'E%d:%s\0' "$?" "$err"; } 3>&1 |
    LC_ALL=C tr '\n\000' '\001\n'
}
mlfile() {
  local kind=$1 f=$2 pre= eofm=0 size=0 last s
  if [ "$fn" = -H ]; then pre=$f; fi
  if [ "$kind" = content ]; then
    last=$(tail -c1 -- "$f" | wc -l; exit "${PIPESTATUS[0]}") || return 2
    if [ "$last" -eq 0 ] || [ "$before" -gt 0 ]; then
      grep -qzaP "${ci[@]}" -e "$mlend" -- "$f"
      s=$?
      case $s in 0) eofm=1 ;; 1) ;; *) return 2 ;; esac
    fi
    size=$(wc -c < "$f") || return 2
  fi
  if [ "$kind" = count ]; then
    F=$f PRE=$pre K=count LC_ALL=C awk "$mlawk" 3< <(stream "$mlfirst") < <(stream "$mlscan")
  else
    F=$f PRE=$pre K=content NUM=$num BEFORE=$before AFTER=$after SEP=$sep EOFM=$eofm SIZE=$size \
      LIMIT=$limit LC_ALL=C "${aw[@]}" "$mlawk" < <(stream "$mlscan")
  fi
}
mlloop() {
  local f s
  while IFS= read -r -d '' f; do
    if stopped; then return 141; fi
    mlfile "$1" "$f"
    s=$?
    if [ "$s" -ne 0 ]; then return "$s"; fi
  done
}
if [ "$fn" = -h ] && [ "$mode" = content ]; then
  at=$(binary)
  s=$?
  if [ "$s" -ge 2 ]; then exit 2; fi
  if [ "$s" -eq 0 ]; then
    if [ "$ml" = 1 ]; then
      grep -qzaP "${ci[@]}" -e "$mlfirst" -- "$root"
    else
      grep -qa "$flavor" "${ci[@]}" -e "$pat" -- "$root"
    fi
    s=$?
    if [ "$s" -ge 2 ]; then exit 2; fi
    if [ "$s" -eq 0 ]; then
      printf 'binary file matches (found "\\0" byte around offset %d)\n' "$at" | "${hd[@]}" | "${tl[@]}"
    fi
    exit 0
  fi
fi
ctx=()
if [ "$num" = 1 ]; then ctx+=(-n); fi
if [ "$before" -gt 0 ]; then ctx+=(-B "$before"); fi
if [ "$after" -gt 0 ]; then ctx+=(-A "$after"); fi
case $mode/$ml in
  files_with_matches/0)
    files | each "${lb[@]}" -l "$bin" "$flavor" "${ci[@]}" -e "$pat" -- | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
  count/0)
    files | each "${lb[@]}" -c "$fn" "$bin" "$flavor" "${ci[@]}" -e "$pat" -- | nonzero | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
  content/0)
    if [ "$sep" = 1 ]; then e=eachsep; else e=each; fi
    files | "$e" "${lb[@]}" "$fn" "$bin" "$flavor" "${ci[@]}" "${ctx[@]}" -e "$pat" -- | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
  files_with_matches/1)
    files | texts | each "${lb[@]}" -l "$bin" -zP "${ci[@]}" -e "$mllisted" -- | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
  count/1)
    files | texts | each "${lb[@]}" -lZ "$bin" -zP "${ci[@]}" -e "$mllisted" -- | mlloop count | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
  *)
    files | texts | each "${lb[@]}" -lZ "$bin" -zP "${ci[@]}" -e "$mlany" -- | mlloop content | "${hd[@]}" | "${tl[@]}"
    st=("${PIPESTATUS[@]}") ;;
esac
for s in "${st[@]}"; do
  case $s in 0|124|141) ;; *) exit 2 ;; esac
done
exit 0
`

// grepAwk is rg -U's answer for one file, from scan's records on its stdin:
// "offset:text", in order, each text's newlines turned to \001 so a record
// is a line. For count, first's records come beside them on fd 3, and a scan
// record that first lacks is an empty match; for content the two need not be
// told apart, an empty match at a character touching the line that character
// is on. Its memory is bounded by the context asked for, not by the file: it
// reads the file's lines alongside and keeps only the last -B of them.
//
// count is rg -U -c: every match, except an empty one just where the match
// before it ended (grep-matcher's find_iter skips those). content prints every
// line a match touches, with rg's markers and "--" between groups that do not
// touch; an empty match at the very end of the file prints the last line when
// the file has no final newline, and only its before-context when it has, as
// rg's searcher does. The line holding byte x is the one whose span [ls, le)
// contains it.
const grepAwk = `
function die() { exit 2 }
function trailer(r,   m) {
  if (r ~ /^E[01]:/) return
  m = substr(r, index(r, ":") + 1); gsub(/\001/, "\n", m)
  if (m != "") print m > "/dev/stderr"
  die()
}
function lost() { print "grep: a match stream ended early" > "/dev/stderr"; die() }
function next1(   r, n) {
  if (end1) return -1
  n = (getline r < G1)
  if (n <= 0) lost()
  if (substr(r, 1, 1) == "E") { trailer(r); end1 = 1; return -1 }
  return substr(r, 1, index(r, ":") - 1) + 0
}
function readln(   n) {
  n = (getline cur < F)
  if (n < 0) { print "grep: " F ": cannot read" > "/dev/stderr"; die() }
  if (n == 0) return 0
  ln++; ls = le; le = ls + length(cur) + 1; done = 0
  return 1
}
function show(l, c, t,   p) {
  if ((B > 0 || A > 0) && ((shown > 0 && l > shown + 1) || (shown == 0 && SEP))) print "--"
  p = ""
  if (PRE != "") p = PRE c
  if (NUM) p = p l c
  print p t
  if (LIMIT) fflush()
  shown = l
}
function pass() {
  if (ln <= aft) show(ln, "-", cur)
  else if (B > 0) { keep[ln] = cur; delete keep[ln - B] }
}
function seek(x) {
  while (ln == 0 || le <= x) {
    if (ln > 0 && !done) pass()
    if (!readln()) return 0
  }
  return 1
}
function event(s, t,   j) {
  if (!seek(s)) {
    for (j = ln + 1 - B; j <= ln; j++) if (j > shown && (j in keep)) show(j, "-", keep[j])
    return
  }
  if (!done) {
    if (gb == 0 || ln > gb + 1)
      for (j = ln - B; j < ln; j++) if (j > shown && (j in keep)) show(j, "-", keep[j])
    show(ln, ":", cur); done = 1
  }
  while (le <= t) {
    if (!readln()) break
    show(ln, ":", cur); done = 1
  }
  gb = ln; aft = ln + A
}
BEGIN {
  F = ENVIRON["F"]; G1 = "/dev/fd/3"; K = ENVIRON["K"]; PRE = ENVIRON["PRE"]
  NUM = ENVIRON["NUM"] + 0; B = ENVIRON["BEFORE"] + 0; A = ENVIRON["AFTER"] + 0
  SEP = ENVIRON["SEP"] + 0; EOFM = ENVIRON["EOFM"] + 0; SIZE = ENVIRON["SIZE"] + 0
  LIMIT = ENVIRON["LIMIT"] + 0
  if (K == "count") n1 = next1()
  while (1) {
    if ((getline r) <= 0) lost()
    if (substr(r, 1, 1) == "E") { trailer(r); break }
    i = index(r, ":"); s = substr(r, 1, i - 1) + 0; e = s + length(r) - i; last = e
    if (K != "count") { event(s, e - 1); continue }
    while (n1 >= 0 && n1 < s) n1 = next1()
    ne = (n1 == s)
    if (ne) n1 = next1()
    if (ne || !(pne && s == pend)) cnt++
    pne = ne; pend = ne ? e : s
  }
  if (K == "count") {
    while (n1 >= 0) n1 = next1()
    if (cnt > 0) { if (PRE != "") print PRE ":" cnt; else print cnt }
    exit 0
  }
  if (EOFM && last < SIZE) event(SIZE, SIZE)
  while (ln < aft && readln()) pass()
  exit 0
}
`

// xargsKill is the line xargs adds when the wrapper kills itself over a
// failed grep; the grep's own message says what failed.
var xargsKill = regexp.MustCompile(`(?m)^xargs: bash: terminated by signal 15\n?`)

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

	root := r.workdir()
	if in.Path != "" {
		root = r.resolve(in.Path)
	}
	q, why := in.query(globAnchor(root, r.workdir()))
	if why != "" {
		return failf("grep: %s", why)
	}
	// No absolute-pattern handling here: a grep pattern is a regex, not a path,
	// and one that happens to start with "/" must not be mistaken for an
	// absolute root and turned loose on the whole filesystem.
	res, err := r.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: q.script(findPath(root)), Timeout: DefaultTimeout})
	if err != nil {
		return Result{}, err
	}
	switch {
	case res.TimedOut:
		return failf("grep: timed out after %s", DefaultTimeout)
	case res.ExitCode != 0:
		res.Stderr = xargsKill.ReplaceAllString(res.Stderr, "")
		return searchFailure("grep", res)
	}
	out := strings.TrimRight(res.Stdout, "\n")
	if out == "" {
		return succeed("no matches")
	}
	// The sandbox's own per-stream cap may already have cut this stream; the
	// marker must ride along, or a spill of it would read as the full result.
	if res.Truncated {
		out = truncationNotice + "\n" + out
	}
	return succeed(out)
}

// script renders grepScript for one search. Every string is single-quoted, so
// the script carries the model's input as data; every number is one query
// validated.
func (q grepQuery) script(root string) string {
	words := func(ws []string) string {
		quoted := make([]string, len(ws))
		for i, w := range ws {
			quoted[i] = singleQuote(w)
		}
		return strings.Join(quoted, " ")
	}
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	var ci []string
	if q.ignoreCase {
		ci = []string{"-i"}
	}
	return strings.NewReplacer(
		"__ROOT__", singleQuote(root),
		"__PAT__", singleQuote(q.pattern),
		"__MLFIRST__", singleQuote(q.ml.first),
		"__MLANY__", singleQuote(q.ml.anywhere),
		"__MLLISTED__", singleQuote(q.ml.listed),
		"__MLSCAN__", singleQuote(q.ml.scan),
		"__MLEND__", singleQuote(q.ml.atEnd),
		"__MLAWK__", singleQuote(grepAwk),
		"__MODE__", singleQuote(q.mode),
		"__ML__", bit(q.multiline),
		"__SKIP__", strconv.Itoa(q.skip),
		"__LIMIT__", strconv.Itoa(q.limit),
		"__NUM__", bit(q.lineNums),
		"__BEFORE__", strconv.Itoa(q.before),
		"__AFTER__", strconv.Itoa(q.after),
		"__CI__", words(ci),
		"__PRUNE__", words(q.prune),
		"__SEL__", words(q.sel),
	).Replace(grepScript)
}
