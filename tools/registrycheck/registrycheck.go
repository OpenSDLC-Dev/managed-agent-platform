// Package main implements registrycheck, the guard over docs/DIVERGENCES.md's
// pointers (#452).
//
// `Tracked: #N` is a present-tense claim that work is outstanding. It is
// written once and falsified later, elsewhere, by an event the file cannot
// see — someone closing #N. Nothing in `make verify` reads GitHub, so that
// falsification was silent and unbounded, which is how 65 of 111 pointers came
// to be wrong before anyone noticed (#445). This tool re-derives the fact
// instead of asserting it.
//
// It runs in two halves, because only one of them needs the network:
//
//   - The shape rungs are offline and free, so they run inside the merge gate —
//     not through this binary but through the package's own test, which calls
//     Check on the real file the way internal/modeltest/docs_test.go holds
//     README's tier table to the tree.
//   - The state rungs ask GitHub whether each referenced issue is open or
//     closed. `make verify` is offline and credential-free by design, so they
//     cannot join it; `make registry-check` runs them, and
//     .github/workflows/registry.yml runs that on a schedule, where a red run
//     is the whole notification mechanism.
//
// The parser exists because the two jobs a pointer does are not separable by
// eye. A clause reads
//
//	*Tracked: #78 (what this entry still leaves open); landed for #45.*
//
// where the head names the LIVE tracker — the issue that will settle the entry,
// which must therefore be open — and everything after the first top-level
// semicolon is PROVENANCE, past tense and permanently true, whose issue may
// perfectly well be open (an entry can land for one issue while another still
// owns a piece of it). A head segment whose parenthetical opens with
// "delivered" is provenance too. Splitting on a naive strings.Split(";") or
// treating every "#N" alike produces false positives on both counts, which is
// why the rules below work on parsed segments rather than on the raw line.
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Finding is one violated rule at one line. Rule is the stable name a failure
// message leads with, so a red run says which invariant broke before it says
// where.
type Finding struct {
	Line int
	Rule string
	Msg  string
}

func (f Finding) String() string {
	return fmt.Sprintf("%d: [%s] %s", f.Line, f.Rule, f.Msg)
}

// Referenced lists every issue a live-or-provenance head segment names, so the
// state lookup can fill in anything the repository listing did not carry.
func Referenced(src string) []int {
	seen := map[int]bool{}
	var out []int
	entries, _ := parse(src)
	for _, e := range entries {
		if e.ptr == nil {
			continue
		}
		for _, s := range e.ptr.head {
			if !seen[s.issue] {
				seen[s.issue] = true
				out = append(out, s.issue)
			}
		}
	}
	return out
}

// File is the registry this guard is written for, and the only file in the
// repository that uses this pointer grammar — several others mention a
// `Tracked:` clause in prose ABOUT the registry, none writes one. The scope is
// narrow because the grammar is this file's convention rather than the repo's:
// run against any other markdown, every rung that keys on a section heading
// would report the absence of a section that file was never meant to have.
const File = "docs/DIVERGENCES.md"

// pointerRe matches an entry's trailing italic pointer clause. It is anchored
// at end-of-line because the clause is always the last thing on its line, and
// [^*] because no clause contains an interior asterisk — both hold for all 114
// pointers today.
//
// A clause that breaks either assumption must not be skipped in silence, which
// is what a non-match alone would mean: the entry would carry no pointer as far
// as every later rung is concerned, and a closed tracker inside it would never
// be looked up. pointerOpenRe is therefore the wider net — anything that opens
// like a clause — and a line it catches that pointerRe does not is a
// pointer-shape finding.
var pointerRe = regexp.MustCompile(` \*(Tracked:|Landed for:?) ([^*]*)\*$`)

var pointerOpenRe = regexp.MustCompile(`(?i) \*(tracked|landed for):?`)

// lineRefRe is the second rot mechanism (#452): a hardcoded intra-file
// cross-reference by line number. One had drifted 77 lines before #453 replaced
// it with the entry's name. A name survives an insertion; a line number cannot.
// Singular and plural both, because "(lines 69-71)" rots the same way.
//
// A `file.go:285` citation into another file is deliberately NOT this: it names
// code the reader is being sent to read, and this file's insertions cannot move
// it. Those churn on their own schedule and are the cited file's problem.
var lineRefRe = regexp.MustCompile(`\(lines? \d+`)

// entryRe is any list bullet opening in bold. It is deliberately wider than the
// `- **` the file actually uses: an entry written `* **X**`, or indented, would
// otherwise not be an entry at all, and every rung would skip it in silence.
var entryRe = regexp.MustCompile(`^\s*[-*+] \*\*`)

// issueRe reads the number a head segment opens with. What may follow it is
// one balanced parenthetical and nothing else, which readSegment enforces by
// scanning rather than by regexp: `(\(.*\))?` is greedy from the first "(" to
// the last ")", so "#900 (a) — but see #901 (b)" parsed as ONE segment whose
// parenthetical swallowed #901, and #901 was never state-checked.
var issueRe = regexp.MustCompile(`^#(\d+)`)

// readSegment parses "#N", optionally followed by one balanced parenthetical,
// and rejects anything else — including a second issue outside that
// parenthetical, which is the shape that used to disappear.
func readSegment(s string) (segment, string) {
	m := issueRe.FindStringSubmatch(s)
	if m == nil {
		return segment{}, fmt.Sprintf("head segment %q does not open with `#N`", s)
	}
	n, _ := strconv.Atoi(m[1])
	rest := strings.TrimSpace(s[len(m[0]):])
	if rest == "" {
		return segment{issue: n}, ""
	}
	if !strings.HasPrefix(rest, "(") {
		return segment{}, fmt.Sprintf("head segment %q has text after #%d that is not a parenthetical", s, n)
	}
	depth := 0
	for i, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 {
			if i != len(rest)-1 {
				return segment{}, fmt.Sprintf(
					"head segment %q continues after its parenthetical closes — anything there, a second `#N` above all, is never checked", s)
			}
			return segment{issue: n, paren: rest}, ""
		}
	}
	return segment{}, fmt.Sprintf("head segment %q has an unclosed parenthetical", s)
}

type segment struct {
	issue int
	paren string // "" when the segment names its issue and nothing else
}

// delivered reports whether the segment is provenance rather than a live
// tracker. The marker is the parenthetical's opening word, which is what the
// Format legend promises a reader.
// The file writes provenance exactly two ways, "(delivered)" and
// "(delivered; …)", so the word has to be followed by one of those two
// delimiters. A prefix test alone read "(delivered-ish)" as provenance and
// skipped the state check on a live tracker, which is a false negative; the
// same test read "(Delivered)" as live, which is a false positive.
func (s segment) delivered() bool {
	const word = "delivered"
	rest := strings.TrimPrefix(s.paren, "(")
	if len(rest) < len(word) || !strings.EqualFold(rest[:len(word)], word) {
		return false
	}
	after := rest[len(word):]
	return after == "" || after[0] == ')' || after[0] == ';'
}

// says reports whether the parenthetical carries anything. The rung can only
// check that a clause exists, never that it is honest — but "()" satisfies a
// bare presence test while saying strictly nothing, and that much is checkable.
func (s segment) says() bool {
	return strings.ContainsFunc(strings.Trim(s.paren, "()"), unicode.IsLetter)
}

type pointer struct {
	kind string // "Tracked" or "Landed for"
	head []segment
	// There is no tail field on purpose. Everything past the first top-level
	// semicolon is provenance, and provenance is never state-checked — so the
	// parser splits it off to find the head and then has no further use for it.
}

type entry struct {
	line    int
	section string
	title   string
	ptr     *pointer // nil when the entry carries no pointer clause at all
	ptrErr  string   // why the clause could not be parsed, when it could not
}

// parse splits the document into entries. It is deliberately tolerant of
// everything except the pointer grammar: prose, headings and blank lines are
// skipped, and only a line opening `- **` is an entry.
func parse(src string) ([]entry, map[string]bool) {
	var out []entry
	section := ""
	// A set, not the last one seen: holding a single string means a second
	// INFERRED heading would silently stop the rung applying to the entries
	// under the first, which is the shape of defect this tool exists to catch.
	inferred := map[string]bool{}
	for i, ln := range strings.Split(src, "\n") {
		switch {
		case strings.HasPrefix(ln, "## "):
			section = strings.TrimPrefix(ln, "## ")
			if strings.Contains(section, "INFERRED") {
				inferred[section] = true
			}
			continue
		case !entryRe.MatchString(ln):
			continue
		}
		e := entry{line: i + 1, section: section, title: title(ln)}
		if m := pointerRe.FindStringSubmatch(ln); m != nil {
			p, err := parsePointer(strings.TrimSuffix(m[1], ":"), m[2])
			if err != "" {
				e.ptrErr = err
			} else {
				e.ptr = p
			}
		} else if pointerOpenRe.MatchString(ln) {
			e.ptrErr = "clause opens like a pointer but does not parse as one, so nothing in it would be " +
				"checked — it must read `*Tracked: …*` (the colon is optional only on the oldest " +
				"`Landed for` clauses) and close with `*` at the end of the line, with no `*` inside it"
		}
		out = append(out, e)
	}
	return out, inferred
}

// title reads the bolded name a finding leads with. It strips whatever bullet
// entryRe matched rather than a fixed "- **", so an entry written some other way
// is named in the finding instead of being reported against the bullet itself.
func title(ln string) string {
	rest := ln
	if loc := entryRe.FindStringIndex(ln); loc != nil {
		rest = ln[loc[1]:]
	}
	if i := strings.Index(rest, "**"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func parsePointer(kind, body string) (*pointer, string) {
	if !strings.HasSuffix(body, ".") {
		return nil, "pointer clause does not end with a full stop"
	}
	body = strings.TrimSuffix(body, ".")
	// splitTop splits at paren depth 0, so an unbalanced clause is not a
	// cosmetic complaint: a stray `)` drives the depth negative and every
	// separator after it stops being top-level, folding the remaining segments
	// into the one before them where nothing state-checks them.
	if !balanced(body) {
		return nil, "pointer clause has unbalanced parentheses, which would hide the segments after the imbalance"
	}
	p := &pointer{kind: kind}
	parts := splitTop(body, ';')
	// A `Landed for` clause is provenance whole, and need not name an issue at
	// all — one cites a plan file instead. Nothing about it can go stale, so it
	// has no head to check.
	if kind == "Landed for" {
		return p, ""
	}
	for _, raw := range splitTop(parts[0], ',') {
		seg := strings.TrimSpace(raw)
		if seg == "" {
			continue
		}
		s, err := readSegment(seg)
		if err != "" {
			return nil, err
		}
		p.head = append(p.head, s)
	}
	if len(p.head) == 0 {
		return nil, "pointer names no issue"
	}
	return p, ""
}

// balanced reports whether every parenthesis in the clause closes, and none
// closes before it opens.
func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth < 0 {
			return false
		}
	}
	return depth == 0
}

// splitTop splits on a separator that is not inside parentheses. Both
// separators occur inside parentheticals — "(delivered; files with plan 08,
// repos with plan 25)" has one of each — so a depth-blind split truncates the
// head and loses the segments after it.
func splitTop(s string, sep rune) []string {
	var out []string
	depth, cur := 0, strings.Builder{}
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
		if r == sep && depth == 0 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	return append(out, cur.String())
}

// Check runs every rung the given inputs allow. state answers whether an issue
// number is open; pass nil to run the offline rungs alone. A number state does
// not know is reported rather than assumed — an unknown issue is exactly the
// case where guessing would hide the defect.
func Check(src string, state func(int) (open bool, known bool)) []Finding {
	entries, inferred := parse(src)
	var out []Finding

	// The INFERRED rung finds its section by name. Renaming the heading would
	// therefore switch the rung off rather than fail it, and a rung that can
	// disappear without saying so is the defect this whole tool is about.
	if len(inferred) == 0 {
		out = append(out, Finding{1, "inferred-section",
			"no `## …(INFERRED)…` heading, so the rule that an inference must name the tracker whose recording would settle it has nothing to run against"})
	}

	// How many distinct entries name each issue as a live tracker. An issue
	// named by more than one cannot, by itself, say what any single entry
	// leaves open — which is the whole complaint behind the parenthetical rule.
	// Counted per entry rather than per segment, so one entry naming an issue
	// twice does not make it look shared with itself.
	shared := map[int]int{}
	for _, e := range entries {
		if e.ptr == nil {
			continue
		}
		seen := map[int]bool{}
		for _, s := range e.ptr.head {
			if !s.delivered() && !seen[s.issue] {
				seen[s.issue] = true
				shared[s.issue]++
			}
		}
	}

	for _, e := range entries {
		if e.ptrErr != "" {
			out = append(out, Finding{e.line, "pointer-shape", fmt.Sprintf("%s: %s", e.title, e.ptrErr)})
			continue
		}
		live := 0
		if e.ptr != nil {
			for _, s := range e.ptr.head {
				if s.delivered() {
					continue
				}
				live++
				if shared[s.issue] > 1 && !s.says() {
					out = append(out, Finding{e.line, "shared-parenthetical", fmt.Sprintf(
						"%s: #%d is the live tracker of %d entries, so naming it alone says nothing about this one — add a parenthetical naming what this entry still leaves open",
						e.title, s.issue, shared[s.issue])})
				}
				if state != nil {
					open, known := state(s.issue)
					switch {
					case !known:
						out = append(out, Finding{e.line, "unknown-issue", fmt.Sprintf(
							"%s: #%d is named as the live tracker but GitHub does not know it", e.title, s.issue)})
					case !open:
						out = append(out, Finding{e.line, "live-tracker-open", fmt.Sprintf(
							"%s: #%d is closed, so it can no longer settle this entry — demote it to provenance (`(delivered)`, or a trailing `landed for #%d` clause) and name the issue that will",
							e.title, s.issue, s.issue)})
					}
				}
			}
			if state != nil {
				for _, s := range e.ptr.head {
					if !s.delivered() {
						continue
					}
					open, known := state(s.issue)
					switch {
					case !known:
						out = append(out, Finding{e.line, "unknown-issue", fmt.Sprintf(
							"%s: #%d is cited as delivered but GitHub does not know it", e.title, s.issue)})
					case open:
						out = append(out, Finding{e.line, "provenance-closed", fmt.Sprintf(
							"%s: #%d is cited as `(delivered)` but is open, so a reader cannot tell whether the entry is settled — "+
								"if the issue as a whole is still outstanding while this entry's part of it landed, say so with a trailing `landed for #%d` clause instead",
							e.title, s.issue, s.issue)})
					}
				}
			}
		}
		if inferred[e.section] && live == 0 {
			out = append(out, Finding{e.line, "inferred-pointer", fmt.Sprintf(
				"%s: an entry in %q must name the open issue whose recording would settle it", e.title, e.section)})
		}
	}

	for i, ln := range strings.Split(src, "\n") {
		if m := lineRefRe.FindString(ln); m != "" {
			out = append(out, Finding{i + 1, "line-reference", fmt.Sprintf(
				"%q is a cross-reference that any insertion above it falsifies — name the entry instead", m)})
		}
	}
	return out
}
