// Package main implements sdkref, the guard over how this repository cites the
// Anthropic SDK and the other sources it reads at a tag (#722, plan 51).
//
// A citation into an external source does two jobs at once, and they have
// opposite requirements under a pin bump. It makes a *temporal claim* — this has
// been true since vX, this was verified at vX, this is gone as of vX — and it
// gives a *locator* inside that source. Written as one clause — "at the pinned"
// tag, then a file and a line number — a bump appears to invalidate
// every one of them, so the bump is either done wholesale (which turns true
// `since` sentences false and produces a green diff proving nothing was
// re-checked) or not at all. docs/plan/51_sdk-reference-binding.md separates the
// axes; this tool holds the separation.
//
// Three rungs, and only two of them can fail the gate:
//
//   - Shape is syntax, plus which files git tracks and which modules go.mod
//     requires, and always decidable, so it runs on every citation.
//   - Resolution runs only for a citation stamped at the version go.mod pins,
//     because that is the one tag the gate is guaranteed to have: `make verify`
//     begins with `build`, so the module is already materialised and
//     `go list -m` finds it with GOPROXY=off. The registry cites tags no cold
//     runner holds, and reaching for them would make the gate need the network.
//   - The report resolves every anchor it can against the pin whatever the
//     citation's stamp, and never fails the gate. At gate time a symbol that has
//     vanished is not yet a defect — the entry may describe a version where it
//     existed — so this rung is judgment, not obligation. A gate that reddened until fifty
//     claims were re-verified would recreate the thing the plan removes. It is
//     also the only rung that may read a tag other than the pin, and only ever
//     one a module cache already holds: available, never required.
//
// Both halves of the corpus go through the same scanner: the registry named by
// File, and the citations written in Go comments, which GoComments reads.
//
// The two failing rungs fail `make verify` through this package's own test,
// which runs -fail over both halves. The report's one obligation is on the pull
// request that brings a transition — moving a pin, or editing a citation — where
// .github/workflows/sdk-bump.yml runs -report and fails while one awaits the
// disposition docs/REFERENCE_PROJECTS.md describes.
package main

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// File is the registry, the larger half of the corpus. The other half is the Go
// comments, reached through the files git tracks rather than by a constant,
// because there is no list of them that could not fall behind the packages.
const File = "docs/DIVERGENCES.md"

// Finding is one violated rule at one place. Rule is the stable name a failure
// message leads with, so a report says which invariant broke before it says
// where. File names the document or Go file the finding is in, and is empty
// only for text scanned with no name, which is what lets the registry and the
// comments share one output.
type Finding struct {
	File string
	Line int
	Rule string
	Msg  string

	// at is the byte offset the finding was seen at within the text scanned.
	// A caller that joined several physical lines into one — a wrapped comment
	// paragraph — uses it to report the line the text is actually on, rather
	// than the line the paragraph began on.
	at int
}

func (f Finding) String() string {
	switch {
	case f.File != "" && f.Line != 0:
		return fmt.Sprintf("%s:%d: [%s] %s", f.File, f.Line, f.Rule, f.Msg)
	case f.File != "":
		return fmt.Sprintf("%s: [%s] %s", f.File, f.Rule, f.Msg)
	case f.Line != 0:
		return fmt.Sprintf("%d: [%s] %s", f.Line, f.Rule, f.Msg)
	}
	return fmt.Sprintf("[%s] %s", f.Rule, f.Msg)
}

// Citation is one reference into an external source: a temporal claim about a
// tag, and a locator inside that source at that tag.
type Citation struct {
	File string
	Line int
	// Unit names the text the citation was read from, within File: its registry
	// line's number, or the byte offset its comment paragraph begins at — a line
	// can hold two comments. A disposition is written beside the anchor it
	// acknowledges, and this is what "beside" means.
	Unit   int
	at     int    // byte offset within the text scanned; see Finding.at
	Form   string // since | checked against | absent at
	Source string
	Tag    string
	Loc    Locator
	Raw    string
}

// Positive reports whether the citation claims the thing it locates exists at
// its tag. `absent at` is the one form that claims the opposite, and a negative
// claim that silently starts resolving again is as wrong as a positive one that
// stops — which is why polarity travels with the citation rather than being
// assumed by the caller.
func (c Citation) Positive() bool { return c.Form != "absent at" }

// Locator is where inside the source the citation points.
type Locator struct {
	Kind    string   // symbol | span | schema
	File    string   // symbol, span: a file the source ships; schema: the literal `spec`
	Symbols []string // symbol: one or more of Name, Type.Member
	Path    string   // schema: a dotted path into the bundled document
	From    int      // span
	To      int      // span
	Reason  string   // span
}

// names are what the locator says is, or is not, in the source: its symbols, or
// its schema path. A span names lines, which no tag but its own can judge.
func (l Locator) names() []string {
	switch l.Kind {
	case "symbol":
		return l.Symbols
	case "schema":
		return []string{l.Path}
	}
	return nil
}

// Desc renders the locator the way the citation wrote it, for messages.
func (l Locator) Desc() string {
	switch l.Kind {
	case "schema":
		return l.File + " " + l.Path
	case "span":
		return fmt.Sprintf("%s:%d-%d (span: %s)", l.File, l.From, l.To, l.Reason)
	}
	return l.File + " " + strings.Join(l.Symbols, " and ")
}

// forms are the three temporal claims, longest first so that "checked against"
// is not read as a bare source name.
var forms = []string{"checked against", "absent at", "since"}

// sources are the citation targets this grammar governs. Everything else the
// registry cites — public documentation by page and fetch date, recordings by
// archive path, this repository's own files — is a real citation and is not
// this tool's; forcing it into a tag-and-symbol grammar would be a category
// error.
//
// The reference's `anthropic-openapi.yml` is not here either, and for a
// different reason: plan 51 counts its coordinates and slice 2 migrates them,
// but into a schema path against the SDK's *bundled* copy — so the citation
// that results names `anthropic-sdk-go`, and `anthropic-openapi.yml` is only
// the spelling rung 1 has to recognise on the way out. That recognition lives
// in the scanner's `reach`, not in this list.
var sources = []string{"anthropic-sdk-go", "anthropic-cli", "go-jose", "go-sdk"}

// spanReasons is the closed set. Every member is a property a parser can check,
// which is the whole point: a reason no parser could contradict would let a
// span keep its line numbers forever, which is the ceremony the set exists to
// prevent. There is deliberately no reason meaning "this one is different".
var spanReasons = map[string]bool{
	"crosses-declarations": true, // the range covers more than one declaration
	"no-unique-name":       true, // the enclosing declaration's name repeats in the file
	"non-go":               true, // the source is prose, with no machine-readable structure at all
}

// prose is the kind of file `non-go` can be true of. Plan 51 keeps that reason
// deliberately narrow: a JSON or YAML file is as machine-readable as the spec,
// and admitting it over one would let a line span there keep its numbers
// forever, with nothing any rung could contradict.
func prose(file string) bool {
	ext := path.Ext(file)
	return ext == ".md" || ext == ".txt"
}

// reasonFits reports whether a span reason can be true of a file of this kind,
// which needs no source to decide: `non-go` claims a parser has nothing to read,
// so only prose can have it, and a declaration reason is about declarations,
// which only Go has.
func reasonFits(file, reason string) bool {
	if reason == "non-go" {
		return prose(file)
	}
	return strings.HasSuffix(file, ".go")
}

var (
	// tagRe is a release as a module version writes it. A number with a leading
	// zero is not one, and admitting it would be worse than refusing it: rung 2
	// compares a stamp with the pin as text, so a stamp spelling the pin's
	// numbers with a zero in front would never be the pin and never be judged.
	tagRe    = regexp.MustCompile(`^v(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$`)
	symbolRe = regexp.MustCompile(`^[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)?$`)
	schemaRe = regexp.MustCompile(`^[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*){2,}$`)
	spanRe   = regexp.MustCompile(`^([\w./-]+):(\d+)-(\d+)$`)
	// fileRe is what may stand where a locator names a file. It is deliberately
	// not `.go`-only: plan 51 anchors two citations on `api.md`, whose lines
	// document Go symbols the module declares elsewhere, and refusing the shape
	// would leave those two with no legal form to migrate into.
	fileRe = regexp.MustCompile(`^[\w./-]+\.[A-Za-z][\w]*$`)
	// citationRe splits a conforming citation into its four parts. The em dash
	// is the separator because the corpus already writes evidence clauses with
	// one, and because a hyphen would collide with a line span.
	citationRe = regexp.MustCompile(`^` + formAlt + `\s+(` +
		strings.Join(sources, "|") + `)\s+(\S+)\s+—\s+(.+)$`)
)

// formAlt matches a temporal form in any case, as one capturing group. The
// grammar's words are lower case, but a citation that opens a sentence is
// capitalised, and the registry writes one that way — a pattern that read only
// the lower-case spelling would be silent about it.
var formAlt = `((?i:` + strings.Join(forms, "|") + `))`

// majorSuffix is the `/vN` a module path carries from its second major version.
var majorSuffix = regexp.MustCompile(`/v\d+$`)

// governed reports whether a name written in front of a tag is a source this
// grammar governs — by its grammar name, alone or ending a path, which is how
// anthropic-cli's module path names a checkout no go.mod requires, or by the
// module path go.mod knows it by.
func governed(name string) bool {
	return slices.Contains(sources, path.Base(name)) || governedModule(name)
}

// governedModule reports whether a name is the module path go.mod knows a
// governed source by, with or without the major-version suffix and down to any
// package in it.
func governedModule(name string) bool {
	for _, mod := range modules {
		base := majorSuffix.ReplaceAllString(mod, "")
		if name == base || strings.HasPrefix(name, base+"/") {
			return true
		}
	}
	return false
}

// attributedElsewhere reports whether a name written in front of a tag says the
// tag belongs to a project this grammar does not govern: a module go.mod
// requires that is not a governed source's module.
//
// A name that only looks like a module path attributes nothing. A documentation
// host, or a pkg.go.dev link into the SDK itself, is shaped like one, and read
// as one it carried a governed tag away without a finding. Only go.mod can tell
// a module from a string with a dot in it. And a required module is its whole
// path: one whose last element merely spells a source's name, as many a
// project's `go-sdk` does, is somebody else's.
//
// A link names a module by the path after its host, so each path the name ends
// in is asked, longest first: the first that is a governed source's module
// keeps the tag, and the first go.mod requires gives it away.
func attributedElsewhere(name string, requires func(string) bool) bool {
	if requires == nil {
		return false
	}
	for {
		if governedModule(name) {
			return false
		}
		if requires(name) {
			return true
		}
		var more bool
		if _, name, more = strings.Cut(name, "/"); !more {
			return false
		}
	}
}

// Required is the predicate over the module paths go.mod requires: whether a
// name is one of them, or a package inside one.
//
// A path that goes on with a major version is neither. `storage/v2` is a module
// of its own, which go.mod requiring `storage` says nothing about.
func Required(paths []string) func(string) bool {
	return func(name string) bool {
		for _, p := range paths {
			rest, ok := strings.CutPrefix(name, p)
			if ok && (rest == "" || strings.HasPrefix(rest, "/") && !majorElement.MatchString(rest[1:])) {
				return true
			}
		}
		return false
	}
}

// majorElement is a path element that names a major version from the second on,
// which the element after a module path can only be if it is another module.
var majorElement = regexp.MustCompile(`^v(?:[2-9]|[1-9]\d+)(?:/|$)`)

// ParseCitation reads one clause and returns the citation it makes, or nil if
// the clause does not make one in this grammar. It is deliberately strict: a
// near-miss is not a citation, so that rung 1 sees it as an unmigrated
// candidate rather than silently accepting a shape the resolver cannot use.
func ParseCitation(s string) *Citation {
	m := citationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || !tagRe.MatchString(m[3]) {
		return nil
	}
	loc, ok := parseLocator(strings.TrimSpace(m[4]))
	if !ok {
		return nil
	}
	form := strings.ToLower(m[1])
	// A span is a positive locator only. What falsifies one is what its range
	// encloses, and "these lines are absent" has no such answer: a range that
	// holds nothing at a bump reads the same whether the thing left or was never
	// there, so no rung could ever flip it.
	if form == "absent at" && loc.Kind == "span" {
		return nil
	}
	// A schema path names a node of the spec the source bundles, and only one
	// source bundles one. Parsed for another, it would reach a resolver that
	// could only answer it about the SDK's.
	if loc.Kind == "schema" && m[2] != SpecSource {
		return nil
	}
	return &Citation{Form: form, Source: m[2], Tag: m[3], Loc: loc, Raw: strings.TrimSpace(s)}
}

// SpecLocator reports whether a locator's file names an OpenAPI document rather
// than source: the `spec` token the schema form uses, the path the SDK bundles
// its copy at, or the reference's own YAML, which the registry cites by name.
// Every spelling has to be recognised, or the refusal below is keyed on a token
// the corpus does not write — the registry names the bundled one by the path
// SpecPath holds, and the reference's by its YAML file's name.
func SpecLocator(file string) bool {
	base := path.Base(file)
	return file == "spec" || base == path.Base(SpecPath) ||
		base == "anthropic-openapi.yml" || base == "anthropic-openapi.yaml"
}

// parseLocator reads the three locator forms. The order matters: a span is
// recognised by its colon before a symbol is tried, so a file-and-range never
// reads as a symbol whose name happens to contain a colon.
func parseLocator(s string) (Locator, bool) {
	if head, reason, ok := cutSpanReason(s); ok {
		m := spanRe.FindStringSubmatch(head)
		// The reason is part of the grammar, not a note beside it: a span whose
		// reason is outside the closed set has not earned the fallback. Admitting
		// one here would let rung 1 consume it as conforming and report nothing.
		//
		// And a reason the file's kind contradicts is refused too, since saying
		// so needs no source at all. That is also what refuses every span over a
		// spec, whatever reason it gives: a spec is neither Go nor prose. Refusing
		// only `non-go` there would close one door and leave the next one open —
		// `crosses-declarations` is just as irrefutable over a document no Go
		// parser reads, and would let the spec citations keep their line numbers
		// forever, the ceremony this closed set exists to prevent.
		if m == nil || !spanReasons[reason] || !reasonFits(m[1], reason) {
			return Locator{}, false
		}
		// A range no file could hold is not a locator. Discarding the
		// conversion error would let `:99999999999999999999-1` parse as a span,
		// and nothing downstream can contradict a range that cannot exist.
		from, err1 := strconv.Atoi(m[2])
		to, err2 := strconv.Atoi(m[3])
		if err1 != nil || err2 != nil || from < 1 || to < from {
			return Locator{}, false
		}
		return Locator{Kind: "span", File: m[1], From: from, To: to, Reason: reason}, true
	}
	file, rest, ok := strings.Cut(s, " ")
	if !ok {
		return Locator{}, false
	}
	rest = strings.TrimSpace(rest)
	if file == "spec" {
		if !schemaRe.MatchString(rest) {
			return Locator{}, false
		}
		return Locator{Kind: "schema", File: file, Path: rest}, true
	}
	// A spec named by its path is still a spec. The symbol arm below would
	// otherwise take it, and rung 2 would answer the symbol from the module's Go
	// declarations — resolving a claim about a schema against a type that merely
	// shares its name, and passing it.
	if !fileRe.MatchString(file) || SpecLocator(file) {
		return Locator{}, false
	}
	// One citation may anchor on more than one symbol — plan 51's own live
	// example names two tool-config types in a single clause — but only joined
	// by `and`: a comma is where rung 1 cuts a clause, so a comma-joined list
	// would be half-read by the scanner and whole-read by the parser.
	var symbols []string
	for _, sym := range strings.Split(rest, " and ") {
		sym = strings.TrimSpace(sym)
		if !symbolRe.MatchString(sym) {
			return Locator{}, false
		}
		symbols = append(symbols, sym)
	}
	return Locator{Kind: "symbol", File: file, Symbols: symbols}, true
}

var spanSuffixRe = regexp.MustCompile(`^(.*?)\s*\(span:\s*([\w-]+)\)$`)

// cutSpanReason splits a span locator from its reason clause.
func cutSpanReason(s string) (head, reason string, ok bool) {
	m := spanSuffixRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return strings.TrimSpace(m[1]), m[2], true
}
