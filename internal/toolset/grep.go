package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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
	multiline     bool
	// flags are the grep options every search pass shares: -i, and in a
	// line-mode content search -n and the context counts.
	flags []string
	// sel and prune are find's file test and the extra directory-prune test
	// that type and glob select with (grepglob.go).
	sel, prune []string
}

// query validates the input's twelve options and resolves their defaults —
// the recorded schema's, each named in its description.
func (in grepInput) query(root string) (grepQuery, string) {
	q := grepQuery{mode: in.OutputMode, lineNums: true, multiline: in.Multiline}
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
	var counts [6]int
	for i, f := range []struct {
		name string
		v    *float64
	}{{"-A", in.After}, {"-B", in.Before}, {"-C", in.C}, {"context", in.Context}, {"head_limit", in.HeadLimit}, {"offset", in.Offset}} {
		if f.v == nil {
			counts[i] = -1
			continue
		}
		if v := *f.v; v < 0 || v != math.Trunc(v) || v > math.MaxInt32 {
			return q, fmt.Sprintf("%s must be a whole number from 0 to %d, not %v", f.name, math.MaxInt32, v)
		}
		counts[i] = int(*f.v)
	}
	// context and -C are one option under two names; where both are given,
	// context wins. -A and -B then override it for their own side, as rg's do
	// whichever order the flags come in.
	base := 0
	for _, c := range []int{counts[2], counts[3]} {
		if c >= 0 {
			base = c
		}
	}
	q.after, q.before = base, base
	if counts[0] >= 0 {
		q.after = counts[0]
	}
	if counts[1] >= 0 {
		q.before = counts[1]
	}
	q.limit, q.skip = max(counts[4], 0), max(counts[5], 0)

	if in.IgnoreCase {
		q.flags = append(q.flags, "-i")
	}
	if q.mode == grepContent && !q.multiline {
		if q.lineNums {
			q.flags = append(q.flags, "-n")
		}
		// Only a nonzero count is passed: GNU grep given any context option,
		// even -A 0, separates every group with "--".
		if q.before > 0 {
			q.flags = append(q.flags, "-B", strconv.Itoa(q.before))
		}
		if q.after > 0 {
			q.flags = append(q.flags, "-A", strconv.Itoa(q.after))
		}
	}

	var types []string
	if in.Type != "" {
		var ok bool
		if types, ok = grepTypes[in.Type]; !ok {
			return q, fmt.Sprintf("unrecognized file type %q; type takes ripgrep's type names, such as go, py, js, ts, rust or java", in.Type)
		}
	}
	rules, err := compileGlobs(root, in.Glob)
	if err != nil {
		return q, err.Error()
	}
	q.sel, q.prune = selection(types, rules)
	return q, ""
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
// The files come from find — every regular file under the root but .git and
// node_modules, narrowed by type and glob — rather than from grep -r, whose
// --include matches a base name only and so cannot hold a glob like
// src/**/*.ts. A root that is a file is searched as given, whatever type and
// glob say, as rg searches a path it is handed. xargs runs grep over them in
// batches, through a wrapper that turns grep's "no match" exit into success,
// so that a nonzero xargs exit means some batch failed; "nothing matched" is
// the empty output, which the Go side answers. Every status that matters is
// read from PIPESTATUS, so the pager — which drains the rest of the stream
// rather than closing it early, so no writer upstream dies of SIGPIPE — can
// never mask a failure or fake one.
//
// multiline searches each file as one record (-z) with (?s) and (?m), which is
// rg -U --multiline-dotall's meaning: . crosses a newline and ^ and $ still
// match at each line. A file-list search needs nothing more; content and count
// need lines, which grep -z no longer has, so mlfile maps each match's byte
// offset onto the file's own line table (grep -b over every line) and prints
// or counts the lines it spans, the way rg -U does. -z also turns off grep's binary check,
// so texts drops binary files first.
const grepScript = `IFS=
root=__ROOT__
pat=__PAT__
mode=__MODE__
skip=__SKIP__
limit=__LIMIT__
multiline=__MULTILINE__
num=__NUM__
before=__BEFORE__
after=__AFTER__
flags=(__FLAGS__)
prune=(__PRUNE__)
sel=(__SEL__)
for t in find xargs; do
  command -v "$t" >/dev/null 2>&1 || { printf 'grep: %s not found in the sandbox image\n' "$t" >&2; exit 2; }
done
flavor=-P
grep -qP -- '' /dev/null 2>/dev/null
if [ "$?" -ge 2 ]; then flavor=-E; fi
if [ "$multiline" = 1 ]; then
  if [ "$flavor" != -P ]; then
    printf 'grep: multiline needs a grep with PCRE (-P), and this sandbox image has none\n' >&2; exit 2
  fi
  pat="(?sm)$pat"
  flags+=(-z)
fi
if [ ! -e "$root" ]; then printf 'grep: %s: No such file or directory\n' "$root" >&2; exit 2; fi
err=$(grep -q "$flavor" "${flags[@]}" -e "$pat" -- /dev/null 2>&1)
if [ "$?" -ge 2 ]; then printf '%s\n' "$err" >&2; exit 2; fi
fn=-h
if [ -d "$root" ]; then fn=-H; fi
files() {
  if [ "$fn" = -h ]; then printf '%s\0' "$root"; return; fi
  find -H "$root" -mindepth 1 -regextype posix-extended \
    -type d \( -name .git -o -name node_modules "${prune[@]}" \) -prune -o -type f \( "${sel[@]}" \) -print0
}
each() { xargs -0 -r bash -c 'grep "$@"; s=$?; if [ "$s" -eq 1 ]; then exit 0; fi; exit "$s"' grep "$@"; }
texts() { each -IlZ -e '' --; }
page() {
  if [ "$limit" -gt 0 ]; then tail -n "+$((skip + 1))" | { head -n "$limit"; cat >/dev/null; }
  else tail -n "+$((skip + 1))"; fi
}
mlfile() {
  local f=$1 rec s m t nl k=0 i j x=0 lo hi last=0 c p
  local -a tbl=() ma=() mb=()
  mapfile -t tbl < <(grep -ab -e '' -- "$f")
  while IFS= read -r -d '' rec; do
    s=${rec%%:*} m=${rec#*:}
    while (( k + 1 < ${#tbl[@]} )) && (( ${tbl[k+1]%%:*} <= s )); do k=$((k + 1)); done
    t=${m//[!$'\n']/} nl=${#t}
    if [[ $m == *$'\n' ]] && (( nl > 0 )); then nl=$((nl - 1)); fi
    if (( ${#ma[@]} )) && (( k + 1 <= mb[-1] )); then
      if (( k + 1 + nl > mb[-1] )); then mb[-1]=$((k + 1 + nl)); fi
    else
      ma+=("$((k + 1))") mb+=("$((k + 1 + nl))")
    fi
  done < <(grep -oab -P "${flags[@]}" -e "$pat" -- "$f")
  (( ${#ma[@]} )) || return 0
  if [ "$mode" = count ]; then
    if [ "$fn" = -H ]; then printf '%s:%d\n' "$f" "${#ma[@]}"; else printf '%d\n' "${#ma[@]}"; fi
    return 0
  fi
  for (( i = 0; i < ${#ma[@]}; i++ )); do
    lo=$((ma[i] - before)) hi=$((mb[i] + after))
    if (( lo <= last )); then lo=$((last + 1)); fi
    if (( lo < 1 )); then lo=1; fi
    if (( hi > ${#tbl[@]} )); then hi=${#tbl[@]}; fi
    if (( lo > hi )); then continue; fi
    if (( before + after > 0 && printed )) && (( last == 0 || lo > last + 1 )); then printf -- '--\n'; fi
    for (( j = lo; j <= hi; j++ )); do
      while (( x < ${#ma[@]} && mb[x] < j )); do x=$((x + 1)); done
      c=-
      if (( x < ${#ma[@]} && ma[x] <= j )); then c=:; fi
      p=
      if [ "$fn" = -H ]; then p=$f$c; fi
      if [ "$num" = 1 ]; then p=$p$j$c; fi
      printf '%s%s\n' "$p" "${tbl[j-1]#*:}"
    done
    last=$hi printed=1
  done
}
case $mode/$multiline in
  files_with_matches/0)
    files | each -l -I "$flavor" "${flags[@]}" -e "$pat" -- | page
    st=("${PIPESTATUS[@]:0:2}") ;;
  files_with_matches/1)
    files | texts | each -l -P "${flags[@]}" -e "$pat" -- | page
    st=("${PIPESTATUS[@]:0:3}") ;;
  count/0)
    files | each -c "$fn" -I "$flavor" "${flags[@]}" -e "$pat" -- | { grep -av -E '(^|:)0$'; true; } | page
    st=("${PIPESTATUS[@]:0:2}") ;;
  content/0)
    files | each "$fn" -I "$flavor" "${flags[@]}" -e "$pat" -- | page
    st=("${PIPESTATUS[@]:0:2}") ;;
  *)
    files | texts | each -lZ -P "${flags[@]}" -e "$pat" -- | while IFS= read -r -d '' f; do mlfile "$f"; done | page
    st=("${PIPESTATUS[@]:0:3}") ;;
esac
for s in "${st[@]}"; do
  if [ "$s" -ne 0 ]; then exit 2; fi
done
exit 0
`

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
	q, why := in.query(root)
	if why != "" {
		return failf("grep: %s", why)
	}
	// No absolute-pattern handling here: a grep pattern is a regex, not a path,
	// and one that happens to start with "/" must not be mistaken for an
	// absolute root and turned loose on the whole filesystem.
	res, err := r.Sandbox.Exec(ctx, sandbox.ExecRequest{Command: q.script(root, in.Pattern), Timeout: DefaultTimeout})
	if err != nil {
		return Result{}, err
	}
	switch {
	case res.TimedOut:
		return failf("grep: timed out after %s", DefaultTimeout)
	case res.ExitCode != 0:
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
func (q grepQuery) script(root, pattern string) string {
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
	return strings.NewReplacer(
		"__ROOT__", singleQuote(root),
		"__PAT__", singleQuote(pattern),
		"__MODE__", singleQuote(q.mode),
		"__SKIP__", strconv.Itoa(q.skip),
		"__LIMIT__", strconv.Itoa(q.limit),
		"__MULTILINE__", bit(q.multiline),
		"__NUM__", bit(q.lineNums),
		"__BEFORE__", strconv.Itoa(q.before),
		"__AFTER__", strconv.Itoa(q.after),
		"__FLAGS__", words(q.flags),
		"__PRUNE__", words(q.prune),
		"__SEL__", words(q.sel),
	).Replace(grepScript)
}
