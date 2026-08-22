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

// File is the registry this guard is written for. It is the only file in the
// repository using this pointer grammar: docs/HISTORY.md and
// docs/plan/04_work-stop-204.md each mention a `Tracked:` clause in prose
// ABOUT the registry, and widening the scope to every *.md would flag both.
const File = "docs/DIVERGENCES.md"

// pointerRe matches an entry's trailing italic pointer clause. It is anchored
// at end-of-line because the clause is always the last thing on its line, and
// [^*] because no clause contains an interior asterisk — both hold for all 113
// pointers today, and the shape rung fails loudly rather than silently
// skipping a line that breaks either.
var pointerRe = regexp.MustCompile(` \*(Tracked|Landed for):? ([^*]*)\*$`)

// lineRefRe is the second rot mechanism (#452): a hardcoded intra-file line
// number. One had drifted 77 lines before #453 replaced it with the entry's
// name. A name survives an insertion; a line number cannot.
var lineRefRe = regexp.MustCompile(`\(line \d+\)`)

// segmentRe reads a head segment: an issue number, optionally followed by a
// parenthetical saying what that issue means for THIS entry.
var segmentRe = regexp.MustCompile(`^#(\d+)\s*(\(.*\))?$`)

type segment struct {
	issue int
	paren string // "" when the segment names its issue and nothing else
}

// delivered reports whether the segment is provenance rather than a live
// tracker. The marker is the parenthetical's opening word, which is what the
// Format legend promises a reader.
func (s segment) delivered() bool {
	return strings.HasPrefix(strings.TrimPrefix(s.paren, "("), "delivered")
}

type pointer struct {
	kind string // "Tracked" or "Landed for"
	head []segment
	tail string
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
func parse(src string) ([]entry, string) {
	var out []entry
	section := ""
	inferred := ""
	for i, ln := range strings.Split(src, "\n") {
		switch {
		case strings.HasPrefix(ln, "## "):
			section = strings.TrimPrefix(ln, "## ")
			if strings.Contains(section, "INFERRED") {
				inferred = section
			}
			continue
		case !strings.HasPrefix(ln, "- **"):
			continue
		}
		e := entry{line: i + 1, section: section, title: title(ln)}
		if m := pointerRe.FindStringSubmatch(ln); m != nil {
			p, err := parsePointer(m[1], m[2])
			if err != "" {
				e.ptrErr = err
			} else {
				e.ptr = p
			}
		}
		out = append(out, e)
	}
	return out, inferred
}

func title(ln string) string {
	rest := strings.TrimPrefix(ln, "- **")
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
	p := &pointer{kind: kind}
	parts := splitTop(body, ';')
	if len(parts) > 1 {
		p.tail = strings.Join(parts[1:], ";")
	}
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
		m := segmentRe.FindStringSubmatch(seg)
		if m == nil {
			return nil, fmt.Sprintf("head segment %q is not `#N` or `#N (…)`", seg)
		}
		n, _ := strconv.Atoi(m[1])
		p.head = append(p.head, segment{issue: n, paren: m[2]})
	}
	if len(p.head) == 0 {
		return nil, "pointer names no issue"
	}
	return p, ""
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
				if shared[s.issue] > 1 && s.paren == "" {
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
					if open, known := state(s.issue); known && open {
						out = append(out, Finding{e.line, "provenance-closed", fmt.Sprintf(
							"%s: #%d is marked delivered but is open again — it is a live tracker now, not provenance", e.title, s.issue)})
					}
				}
			}
		}
		if e.section == inferred && live == 0 {
			out = append(out, Finding{e.line, "inferred-pointer", fmt.Sprintf(
				"%s: an entry in %q must name the open issue whose recording would settle it", e.title, inferred)})
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
