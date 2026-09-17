package main

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// Rung 1 — shape. Syntax, plus two facts about this repository that no source's
// tag can move — which files git tracks, and which modules go.mod requires — so
// it is always decidable, and therefore the only rung that can speak about a
// citation stamped at a tag nobody here can open.
//
// The hard half is not judging a citation but *finding* one. A guard that only
// inspected text already written in this grammar would report nothing about a
// corpus written in the old one, which is the corpus it exists to migrate — a
// clean bill of health it did not earn. So the unit is a candidate: any clause
// that reaches for the grammar by naming a source this tool governs, by carrying
// a coordinate into a file nothing here ships, or by carrying a version at all.
// What conforms is consumed; what is left over is reported. A clause that does
// none of those — a file and a symbol named in passing, with no source, no tag
// and no line — is not a candidate, and plan 51's limits say why: telling one
// into the SDK from a partial path into this repository would need a guess.
//
// The last of those is what keeps the rung from being keyed on spelling. Each
// named rule below recognises one way the corpus writes a citation, and a
// detector built only from them is silent about every way nobody listed — which
// is how the plan's own opening example, a claim "at the pinned" tag with no
// source named, went unseen. So
// a version no rule consumed is a finding unless the text says whose it is, the
// same fail-closed reading the steering-document guard in
// internal/domain/docs_test.go gives the same sentences.
//
// A candidate reports once. An unmigrated citation is undated *and* its locator
// is a line number, but it has one defect — it has not been migrated — and
// listing both would make slice 2's diff look twice the size it is.

var (
	// datedHead opens a conforming citation. The locator runs from the end of
	// the head to the next clause boundary.
	datedHead = regexp.MustCompile(`\b` + formAlt + `\s+(` +
		strings.Join(sources, "|") + `)\s+(v\d+\.\d+\.\d+)\s+—\s+`)
	// undatedHead is a source and a tag: the shape the corpus is written in
	// today. Whether a temporal form stands in front of it is asked separately,
	// because the two defects need different edits. A match that is only the tail
	// of a longer name is not a head; see standalone.
	undatedHead = regexp.MustCompile(`(` + strings.Join(sources, "|") + `)\s+(v\d+\.\d+\.\d+)`)
	// majorTag is a source and the major number of a version, past the major
	// version a module path carries. Named after a form
	// with nothing following it, it is a dated claim no other pattern here can
	// see, since every version they read has two numbers at least; a fuller
	// version it cuts short is a head the patterns around it read the same way.
	// With no form it is how prose names a module's major version, as
	// `go-jose/v4` does, and dates nothing.
	majorTag = regexp.MustCompile(`(` + strings.Join(sources, "|") + `)(?:/v\d+)?\s+([vV]\d+)\b`)
	// negated is a negation standing straight before a temporal form. It turns the
	// claim into its opposite, and every one of the grammar's three is a positive
	// statement about a tag, so a negated head is not a citation however well the
	// rest of it parses. Only the word in front is read — a negation further back
	// in the sentence is prose this does not try to parse.
	negated = regexp.MustCompile("(?i)(?:\\bnot|\\bnever|\\bno longer|n['’]t)(?:\\s+(?:been|yet|ever))?[\\s`_\\[(]*$")
	// formBefore matches a temporal form immediately preceding a head, past the
	// quoting the corpus wraps a source and tag in: a form followed by a
	// backticked source and tag is dated, and the steering-document guard reads
	// it so.
	formBefore = regexp.MustCompile(`\b` + formAlt + "[\\s`_\\[(]+$")
	// datedBefore is a temporal form ending right before a version, with at most
	// one word between them — the source, however it is spelled, possessive
	// included. It is the steering-document guard's datingMarker, kept to the
	// same reading on purpose: two gates that disagreed about whether one
	// sentence is dated would each be right about half the corpus. The leading
	// word boundary keeps it off a word that merely ends in a form: "unchecked
	// against" dates nothing.
	datedBefore = regexp.MustCompile(`\b` + formAlt +
		"[\\s`_\\[(]+(?:([\\w.@/-]+)(?i:'s|’s)?[\\s`_\\[(]+)?$")
	// anyTag is every version the text carries, two-component ones included — a
	// bump outdates a version written without its patch number as surely as one
	// written with it. Whatever no rule above it consumed is a candidate unless it
	// is attributed elsewhere.
	anyTag = regexp.MustCompile(`\b[vV]\d+\.\d+(?:\.\d+)?\b`)
	// nameBefore is the word a version is written straight after, past quoting,
	// a possessive, or the `@` of `module@version`. It is how the text attributes
	// a version to a project: `k8s.io/api v0.36.2`,
	// `cloud.google.com/go/storage@v1.56.0`.
	nameBefore = regexp.MustCompile("([\\w./-]+)(?:['’][sS])?(?:@|[\\s`_\\[(]+)$")
	// nameChar is a character a name is spelt with; wordChar is one a word is.
	nameChar = regexp.MustCompile(`^[\w./-]$`)
	wordChar = regexp.MustCompile(`^[\w.-]$`)
	// untaggedHead is a governed source followed straight by a file, with no
	// tag between them. It names everything a citation needs
	// except the one thing a bump asks about,
	// and it is the commonest way this corpus cites the SDK in passing — with no
	// tag and, often, no line number, it matches no other rule here at all.
	untaggedHead = regexp.MustCompile(`(` + strings.Join(sources, "|") +
		`)\s+([\w./-]+\.(?:go|md|ya?ml|json(?:\.gz)?))\b`)
	// untaggedMention is a governed source — by name, possessive, quoted, or
	// with the rest of an import path after it — and the word written after it.
	// It is the same claim as untaggedHead without the file — a method named
	// after the source's possessive, a package path in parentheses after the
	// source — and matched nothing else here, so a bump could falsify it with no
	// rung saying so. Whether the word, or the path, names anything is mentions'
	// to judge, since this pattern cannot tell a symbol from prose.
	untaggedMention = regexp.MustCompile("(" + strings.Join(sources, "|") +
		")((?:/[\\w.-]*\\w)*)(?:`?(?:['’][sS])?[\\s`(]+([A-Za-z_][\\w./-]*\\w))?")
	// forgeRoutes are the pages a repository link reaches that are not its
	// tree: what follows one of them after a source's path is a number or a
	// title, never a package.
	forgeRoutes = []string{"pull", "issues", "discussions", "releases"}
	// packagePath is a path of packages, which symbolLike admits beside a
	// symbol; majorVersion is a module's major version, which names no package.
	packagePath  = regexp.MustCompile(`^[a-z][\w.-]*(?:/[\w.-]+)+$`)
	majorVersion = regexp.MustCompile(`^[vV]\d+$`)
	// specEpithet is the other way the corpus names a source: not by module but
	// by what it is. The registry writes a tag and then the kind of document,
	// which names the SDK's bundled copy at that tag and rots exactly like a
	// coordinate — but carries no source token, so the head above cannot see
	// it. These are the spec citations slice 2 migrates to a schema path, and a
	// rung that could not see them would report the migration complete while
	// every one of them remained.
	specEpithet = regexp.MustCompile(`(v\d+\.\d+\.\d+)\s+(OpenAPI spec|bundled spec)`)
	// untaggedSpec is the same epithet with no tag at all, a schema property
	// quoted after it — which is to the spec what untaggedHead is to a module: a
	// source and something inside it, and nothing to say which release. There is
	// no version token for the sweep below to catch.
	untaggedSpec = regexp.MustCompile(`\b(?:OpenAPI|bundled) spec\b`)
	// specFile is the spec the SDK bundles, named by its file, which only the
	// SDK ships: a mention with no head in front of it is an untagged SDK
	// citation however little else the sentence says. It is not a head, because
	// a conforming citation's locator may name the same file and must not be
	// cut there.
	specFile = regexp.MustCompile(regexp.QuoteMeta(path.Base(SpecPath)))
	// coordinate is a file and a line: the unmigrated locator. The extension
	// set is closed, and that is what keeps this off text that merely looks
	// like a coordinate — the registry documents a Kubernetes service address
	// and quotes files out of harness references, and a comment writes a host
	// and a port. Go source, the SDK's generated API index and the reference's
	// OpenAPI document are the file kinds a governed source actually ships.
	coordinate = regexp.MustCompile(`[\w./-]+\.(?:go|md|ya?ml|json(?:\.gz)?):\d+(?:-\d+)?`)
	// locatorLike is what a locator names: a file of a kind a governed source
	// ships, or a schema path. A dated claim carrying one has a locator that did
	// not parse, rather than none. An em dash is not enough, because prose
	// writes one as often as a citation does.
	locatorLike = regexp.MustCompile(`[\w./-]+\.(?:go|md|ya?ml|json(?:\.gz)?)\b|\bspec\s+[A-Za-z_]\w*(?:\.\w+){2,}`)
	// continuation is a coordinate with its filename left to the prose: `and
	// :859-860`. Each is masked today by the undated clause enclosing it, so
	// the moment slice 2 rewrites that head they are orphaned, and a scanner
	// that could not see them would call the migration finished. Requiring a
	// space, a slash or an open parenthesis in front is what keeps this off
	// `10:30` and off a coordinate's own colon.
	continuation = regexp.MustCompile(`[ (/](:\d+(?:-\d+)?)\b`)
	// reach marks a line as reaching for this grammar at all: one of the
	// governed source names, or the reference's OpenAPI document. That last one
	// is in no module and no resolver can open it, so it never reaches a
	// failing rung — but plan 51 measures its coordinates and slice 2 migrates
	// them to a schema path against the SDK's bundled copy, so rung 1 has to be
	// able to see them go.
	reach = regexp.MustCompile(strings.Join(sources, "|") + `|anthropic-openapi\.ya?ml`)
	// singleLine and unreasonedSpan classify a locator that did not parse.
	singleLine     = regexp.MustCompile(`^[\w./-]+:\d+$`)
	unreasonedSpan = regexp.MustCompile(`^[\w./-]+:\d+-\d+$`)
)

// The advice a finding carries, where more than one rule gives the same edit.
const (
	adviseUndated = "%q names a source and a tag with no temporal form: write `since`, " +
		"`checked against` or `absent at` before it, so a bump cannot falsify it"
	adviseLocatorMissing = "%q is stamped but gives no locator: add `— <file> <symbol>`, so " +
		"a bump has something to resolve rather than a sentence to re-read"
	adviseHeadMalformed = "%q is dated against a governed source, but its head is not the " +
		"grammar's: write `<form> <source> <tag> — <locator>` exactly, the source by its " +
		"grammar name, the tag as `vMAJOR.MINOR.PATCH` with a lower-case `v`, no leading zero " +
		"and no suffix, nothing quoted between the form and the tag, and an em dash before " +
		"the locator"
	adviseNegated = "%q negates a temporal form, and each of the grammar's three is a " +
		"positive claim about a tag: write what was checked (`checked against`) or what is " +
		"gone (`absent at`), or drop the version from a sentence that checked nothing"
	adviseUntagged = "%q names a source and a file but no tag, so no bump can be told whether " +
		"it moved: write `checked against <source> <tag> — <file> <symbol>`"
	adviseMentionUntagged = "%q names something in a source but no tag, so no bump can be " +
		"told whether it moved: cite it beside the mention as `(checked against <source> <tag> " +
		"— <file> <symbol>)` naming that symbol, or — where a citation beside it already " +
		"does — drop the source's name"
	adviseSpecUntagged = "%q names an OpenAPI document but no tag, so no bump can be told " +
		"whether it moved: write `checked against " + SpecSource + " <tag> — spec <schema path>`"
	adviseSpecUndated = "%q names the SDK's bundled spec at a tag with no temporal form: write " +
		"`since`, `checked against` or `absent at` before it, so a bump cannot falsify it"
	adviseSpecUnnamed = "%q dates a claim against the SDK's bundled spec by what it is rather " +
		"than whose it is: write `<form> " + SpecSource + " <tag> — spec <schema path>`"
)

// elsewhere is the edit for a version that is not a governed source's, which
// the advice for a version with no source has to offer as well as the edit for
// one that is.
const elsewhere = "if it is a module go.mod requires, write that module path in front of " +
	"it, the way go.mod does; if it is a version of anything else — a server, a tool — " +
	"write it without the `v`, which no gate here reads as a module version"

var (
	adviseSourceUnnamed = "%q dates a claim against a tag without saying whose tag it is: " +
		"if it is a tag of " + strings.Join(sources, ", ") + ", name the source and give a " +
		"locator, so a bump knows whether this one is its business; " + elsewhere
	adviseBareTag = "%q is a version with neither a temporal form nor a source: if it is a " +
		"tag of " + strings.Join(sources, ", ") + ", write `<" + strings.Join(forms, "|") +
		"> <source> <tag> — <locator>`; " + elsewhere
)

// datedRule names the defect in a claim dated against a governed source whose
// head is written the grammar's way as far as its source — the form, a space,
// the source's name — and that is not a citation, whose clause runs from the tag
// to end. There are two edits. A claim with nothing to resolve needs a locator;
// one whose head is misspelt after all — a leading zero or a pre-release suffix
// on the tag, a hyphen for the em dash — has its locator already, and telling
// its author to add one sends them to the wrong half of the clause.
func datedRule(text string, tag []int, end int) (string, string) {
	suffix := tag[1] < len(text) && strings.ContainsRune("-+", rune(text[tag[1]]))
	if !tagRe.MatchString(text[tag[0]:tag[1]]) || suffix || locatorLike.MatchString(text[tag[1]:end]) {
		return "head-malformed", adviseHeadMalformed
	}
	return "locator-missing", adviseLocatorMissing
}

// standalone reports whether a source name found at i is a name of its own
// rather than the tail of a longer one. The SDK's module path ends in its grammar
// name, and a head read inside `github.com/anthropics/anthropic-sdk-go` is half
// a name, with the form in front of the whole of it where there is one — so the
// sweep, which reads a name whole, is what classifies it.
func standalone(text string, i int) bool { return i == 0 || !nameChar.MatchString(text[i-1:i]) }

// names reports whether a source name found at text[i:j] names that source. It
// does not when it is the tail of a longer word — `mongo-sdk` ends in
// `go-sdk` — or the last element of a module go.mod requires for another
// project. Unlike standalone it admits a slash in front, since the patterns
// that ask it read a source ending an import path as that source; j may run on
// past the name, over the package path a mention carries.
func names(text string, i, j int, requires func(string) bool) bool {
	if i > 0 && wordChar.MatchString(text[i-1:i]) {
		return false
	}
	return !attributedElsewhere(text[nameStart(text, i):j], requires)
}

// nameStart is where the name ending at or running through i begins: a module
// path in front of a source's name is part of it, and so is the scheme of a
// link it is written as.
func nameStart(text string, i int) int {
	for i > 0 && (nameChar.MatchString(text[i-1:i]) || strings.HasPrefix(text[i-1:], "://")) {
		i--
	}
	return i
}

// reaches reports whether a line reaches for this grammar: reach, matched where
// it names a source.
func reaches(text string, requires func(string) bool) bool {
	for _, at := range reach.FindAllStringIndex(text, -1) {
		if names(text, at[0], at[1], requires) {
			return true
		}
	}
	return false
}

// Scanner runs rung 1.
//
// Its state is the two questions pure syntax cannot answer. The first is
// whether a bare coordinate names a file in *this* repository: the registry
// cites a line of the SDK's `internal/apierror/apierror.go` and a line of our
// own `internal/api/server.go` in one clause, and no prefix tells them apart,
// because the SDK has an `internal/` too. Only the tree can say, and a
// coordinate that is ours cites no external tag and is not this grammar's.
type Scanner struct {
	// InRepo reports whether a path names a file this repository ships. A
	// scanner without it answers pure syntax and treats no coordinate as ours,
	// which is what the unit tests want and what `Shape` below gives them.
	InRepo func(path string) bool

	// AnyExternalCoordinate treats a coordinate into a file this repository
	// does not ship as a candidate even where nothing on the line names a
	// source. It is set for Go comments and not for the registry, and the
	// asymmetry is in the text rather than in the rule: a comment beside our
	// own code that names a line of `betasession.go` can be citing nothing but an
	// external source, while the registry's prose also quotes examples,
	// harness references and operator addresses that merely look alike. Most
	// comment coordinates sit in paragraphs that never spell a source name, so
	// a scanner that waited for one would leave most of this half unread — and
	// would then report the migration finished.
	AnyExternalCoordinate bool

	// Requires reports whether go.mod requires the module a name belongs to,
	// and is the second question: a version written straight after such a name
	// is that module's, not a governed source's. A scanner without it attributes
	// no version to any module, which is the fail-closed reading and the one
	// `Shape` gives the unit tests.
	Requires func(name string) bool
}

// ShapeAll runs rung 1 over a whole document, numbering each finding by line.
func ShapeAll(src string) []Finding { return Scanner{}.All(src) }

// Shape runs rung 1 over one line.
func Shape(line string) []Finding { return Scanner{}.Line(line) }

// All runs rung 1 over a whole document, numbering each finding by line.
func (s Scanner) All(src string) []Finding {
	all, _ := s.Document(src, "")
	return all
}

// Document runs rung 1 over a whole document, naming each finding by the file
// and line it is on. It also returns the coordinates into this repository it
// read, each as `file:line coordinate`: no rung checks those — git holds their
// history, not a tag — and plan 51 has the bump report name them rather than
// let "not checked" mean "not mentioned".
func (s Scanner) Document(src, name string) ([]Finding, []string) {
	var all []Finding
	var ours []string
	for i, line := range strings.Split(src, "\n") {
		found, own := s.scan(line)
		for _, f := range found {
			f.File, f.Line = name, i+1
			all = append(all, f)
		}
		for _, o := range own {
			ours = append(ours, fmt.Sprintf("%s:%d %s", name, i+1, o.Msg))
		}
	}
	return all, ours
}

// Line runs rung 1 over one line. Each finding carries the offset it was found
// at, so a caller scanning joined text — a wrapped comment paragraph — can put
// it back on the physical line it came from.
func (s Scanner) Line(line string) []Finding {
	found, _ := s.scan(line)
	return found
}

// scan is Line, plus the coordinates into this repository the line holds, each
// carrying its offset and, as its Msg, the coordinate itself.
func (s Scanner) scan(line string) ([]Finding, []Finding) {
	// Markdown emphasis is never part of a coordinate or a symbol in this
	// corpus, and blanking it here keeps every pattern below from having to
	// know about the registry's `*Evidence: …*` wrapper. It is replaced rather
	// than deleted so that every offset below still indexes the caller's own
	// text — which is what lets a wrapped comment paragraph put a finding back
	// on the physical line it came from.
	text := blankEmphasis(line)
	bounds := headStarts(text)
	consumed := make([]bool, len(text))
	var findings []Finding

	// dated names the defect in a claim whose temporal form starts at form, and
	// where its quote starts. A negation in front of the form is a defect of its
	// own whatever follows, and the quote begins with it.
	dated := func(form int, rule, advice string) (int, string, string) {
		if n := negated.FindStringIndex(text[:form]); n != nil {
			return n[0], "negated-form", adviseNegated
		}
		return form, rule, advice
	}

	// A conforming citation is consumed whole; one that opens correctly and
	// then fails on its locator is classified by the locator's own shape,
	// because that is the part slice 2 has to rewrite.
	for _, at := range datedHead.FindAllStringSubmatchIndex(text, -1) {
		loc, end, atHead := clause(text, at[1], nextHead(bounds, at[1]))
		whole := trimConnective(text[at[0]:end], atHead)
		from, rule, advice := dated(at[0], "head-malformed", adviseHeadMalformed)
		switch {
		case rule == "negated-form" || !tagRe.MatchString(text[at[6]:at[7]]):
			findings = append(findings, Finding{at: from, Rule: rule,
				Msg: fmt.Sprintf(advice, strings.TrimSpace(text[from:at[0]]+whole))})
		case ParseCitation(whole) == nil:
			f := classifyLocator(loc, whole, strings.ToLower(text[at[2]:at[3]]), text[at[4]:at[5]])
			f.at = at[0]
			findings = append(findings, f)
		}
		mark(consumed, from, end)
	}

	// emit reports the clause that runs from `from` past `after`, and consumes
	// it. report does the same for a match nothing has consumed yet.
	emit := func(from, after int, rule, msg string) {
		_, end, atHead := clause(text, after, nextHead(bounds, after))
		quote := strings.TrimSpace(text[from:end])
		if atHead {
			// Cut at the next citation, the quote ends on whatever led into it
			// — a parenthesis, a connective — which is no part of this claim.
			quote = trimConnective(strings.TrimSuffix(quote, "("), true)
		}
		findings = append(findings, Finding{
			at:   from,
			Rule: rule,
			Msg:  fmt.Sprintf(msg, quote),
		})
		mark(consumed, from, end)
	}
	report := func(at []int, rule, msg string) {
		if !anyConsumed(consumed, at[0], at[1]) {
			emit(at[0], at[1], rule, msg)
		}
	}
	clauseEnd := func(after int) int {
		_, end, _ := clause(text, after, nextHead(bounds, after))
		return end
	}

	// A source and a tag with no em dash and no locator. If a temporal form
	// stands in front of it the claim is already dated, and what it lacks is a
	// locator or a well-formed head, which are different edits — and calling it
	// undated would also contradict the steering-document guard, which reads the
	// same phrasing as dated.
	for _, at := range undatedHead.FindAllStringSubmatchIndex(text, -1) {
		if !standalone(text, at[0]) || anyConsumed(consumed, at[0], at[1]) {
			continue
		}
		m := formBefore.FindStringSubmatchIndex(text[:at[0]])
		if m == nil {
			emit(at[0], at[1], "undated", adviseUndated)
			continue
		}
		rule, advice := datedRule(text, at[4:6], clauseEnd(at[1]))
		if strings.TrimSpace(text[m[3]:at[0]]) != "" {
			// Quoting between the form and the source is a head written wrong,
			// with a locator or without one: the edit is the same either way, and
			// asking for a locator first would only send the author back again.
			rule, advice = "head-malformed", adviseHeadMalformed
		}
		from, rule, advice := dated(m[2], rule, advice)
		emit(from, at[1], rule, advice)
	}
	for _, at := range majorTag.FindAllStringSubmatchIndex(text, -1) {
		// The pattern reads only the source's own name, so a module path or a
		// link in front of it puts the form before the whole of it.
		start := nameStart(text, at[0])
		if name := text[start:at[3]]; !governed(name) || attributedElsewhere(name, s.Requires) {
			continue
		}
		m := formBefore.FindStringSubmatchIndex(text[:start])
		if m == nil || anyConsumed(consumed, at[0], at[1]) {
			continue
		}
		from, rule, advice := dated(m[2], "head-malformed", adviseHeadMalformed)
		emit(from, at[1], rule, advice)
	}
	// A tag and the kind of document, which names no source. Dated, it is a
	// claim whose head names the spec by what it is.
	for _, at := range specEpithet.FindAllStringIndex(text, -1) {
		if anyConsumed(consumed, at[0], at[1]) {
			continue
		}
		m := datedBefore.FindStringSubmatchIndex(text[:at[0]])
		if m == nil {
			emit(at[0], at[1], "undated", adviseSpecUndated)
			continue
		}
		rule, advice := "source-unnamed", adviseSpecUnnamed
		if m[4] >= 0 && governed(text[m[4]:m[5]]) {
			rule, advice = "head-malformed", adviseHeadMalformed
		}
		from, rule, advice := dated(m[2], rule, advice)
		emit(from, at[1], rule, advice)
	}
	for _, at := range untaggedHead.FindAllStringSubmatchIndex(text, -1) {
		if !names(text, at[0], at[3], s.Requires) {
			continue
		}
		// The SDK's spec is a file the source ships, and the edit it needs is a
		// schema path rather than a symbol.
		advice := adviseUntagged
		if SpecLocator(text[at[4]:at[5]]) {
			advice = adviseSpecUntagged
		}
		report(at, "untagged", advice)
	}
	for _, re := range []*regexp.Regexp{untaggedSpec, specFile} {
		for _, at := range re.FindAllStringIndex(text, -1) {
			report(at, "untagged", adviseSpecUntagged)
		}
	}
	// After the spec: `OpenAPI` reads as a symbol, and the edit an OpenAPI
	// document needs is a schema path.
	for _, m := range mentions(text, s.Requires) {
		_, end, _ := clause(text, m.to, nextHead(bounds, m.to))
		switch {
		case s.ownsTag(text[m.to:end]):
			// The claim has a tag, so "no tag" is the wrong edit: the sweep
			// below classifies the version and says which one it needs.
		case citedBy(text, end, bounds, m):
			mark(consumed, m.from, m.to)
		default:
			report([]int{m.from, m.to}, "untagged", adviseMentionUntagged)
		}
	}

	// Last, every version nothing above consumed. Each is classified by what
	// stands in front of it, so the advice is the edit that version needs — and a
	// version the text attributes to a project this grammar does not govern is
	// consumed silently, because it is somebody else's citation and not a gap in
	// this one.
	//
	// Whether a version is spoken for is asked of its own bytes only. The text a
	// finding quotes may start earlier — at the form, or at the source's name —
	// and that start can sit inside a clause already reported, when the clause
	// before it ended at the full stop of an abbreviation such as "e.g.";
	// asking the question of the whole quote dropped the version with no
	// finding at all.
	for _, at := range anyTag.FindAllStringIndex(text, -1) {
		if anyConsumed(consumed, at[0], at[1]) {
			continue
		}
		before := text[:at[0]]
		name, named := "", -1
		if m := nameBefore.FindStringSubmatchIndex(before); m != nil {
			name, named = before[m[2]:m[3]], m[2]
			if attributedElsewhere(name, s.Requires) {
				mark(consumed, at[0], at[1])
				continue
			}
		}
		from, rule, advice := at[0], "bare-tag", adviseBareTag
		if m := datedBefore.FindStringSubmatchIndex(before); m != nil {
			rule, advice = "source-unnamed", adviseSourceUnnamed
			if m[4] >= 0 && governed(before[m[4]:m[5]]) {
				// Every head written the grammar's way was read above, so a
				// governed source dated here is written some other way: quoted, as
				// a possessive, by its module path, or with a misspelt tag.
				rule, advice = "head-malformed", adviseHeadMalformed
			}
			from, rule, advice = dated(m[2], rule, advice)
		} else if governed(name) {
			from, rule, advice = named, "undated", adviseUndated
		}
		emit(from, at[1], rule, advice)
	}

	// What is left is a bare coordinate. In the registry it counts only where
	// the line reaches for this grammar; in a Go comment any coordinate into a
	// file nothing here ships is one. Either way the reach is judged over the
	// whole line rather than the coordinate's own clause, and that is
	// deliberate: the registry names a source once and then hangs continuations
	// off it across several commas, so clause-scoping would lose the very
	// continuations this rule exists to catch.
	var ours []Finding
	if s.AnyExternalCoordinate || reaches(text, s.Requires) {
		var bare []Finding
		bare, ours = s.bare(text, consumed)
		findings = append(findings, bare...)
	}
	return findings, ours
}

// bare reports the coordinates left over once every citation clause has been
// consumed, and returns separately the ones that point into this repository.
// Full coordinates go first, so a coordinate's own colon is already spoken for
// by the time the bare continuations are swept.
func (s Scanner) bare(text string, consumed []bool) ([]Finding, []Finding) {
	var findings, ours []Finding
	report := func(from, to int, what, advice string) {
		if anyConsumed(consumed, from, to) {
			return
		}
		mark(consumed, from, to)
		findings = append(findings, Finding{at: from, Rule: "bare-line", Msg: fmt.Sprintf(
			"%q %s, so nothing can check it and a bump cannot be told whether it moved: %s",
			text[from:to], what, advice)})
	}
	var external bool
	for _, at := range coordinate.FindAllStringIndex(text, -1) {
		// An absolute path names a place on some filesystem — a sandbox's
		// workspace, a developer's disk — and no governed source ships one: a
		// citation into a module is relative to that module.
		if text[at[0]] == '/' {
			mark(consumed, at[0], at[1])
			continue
		}
		if path, _, _ := strings.Cut(text[at[0]:at[1]], ":"); s.InRepo != nil && s.InRepo(path) {
			if !anyConsumed(consumed, at[0], at[1]) {
				ours = append(ours, Finding{at: at[0], Msg: text[at[0]:at[1]]})
			}
			mark(consumed, at[0], at[1])
			continue
		}
		external = true
		report(at[0], at[1], "inherits its tag from the prose around it",
			"give it its own stamp and a symbol")
	}
	// A continuation is only meaningful where a coordinate could have set the
	// filename it inherits: a line naming a governed source, or — in a Go
	// comment, where the paragraph usually names no source at all — a
	// coordinate into a file this repository does not ship, sitting on the same
	// line. With neither, a lone `:12` is far more likely to be a port.
	if !reaches(text, s.Requires) && !(s.AnyExternalCoordinate && external) {
		return findings, ours
	}
	for _, at := range continuation.FindAllStringSubmatchIndex(text, -1) {
		report(at[2], at[3], "inherits both its file and its tag from the prose around it",
			"give it its own stamp, its own file, and a symbol")
	}
	return findings, ours
}

// headStarts collects where every citation head begins, sorted. A clause runs
// to the next one at the latest: two citations written side by side on one line
// would otherwise be read as a single malformed one by rung 1 and as the second
// one alone by Citations, and the two halves of one scan would disagree about
// where a citation ends.
func headStarts(text string) []int {
	var out []int
	for _, re := range []*regexp.Regexp{datedHead, undatedHead, specEpithet, untaggedHead, untaggedSpec} {
		for _, at := range re.FindAllStringIndex(text, -1) {
			out = append(out, at[0])
		}
	}
	for _, m := range mentions(text, nil) {
		out = append(out, m.from)
	}
	sort.Ints(out)
	return out
}

// mention is a governed source named beside something inside it: from the
// source's name to the end of what it names.
type mention struct {
	from, to int
	source   string
	name     string // the symbol or package path named
}

// mentions returns every untaggedMention that names something. A source named
// by its module path is still that source, so unlike a head a mention need not
// stand alone, and neither a major version nor the source's own name repeated in
// that path — `go-jose/go-jose/v4` — is a package. The scan resumes after a
// source's own name rather than after its match, because the word a match took
// may be a source's name itself.
func mentions(text string, requires func(string) bool) []mention {
	var out []mention
	for pos := 0; pos < len(text); {
		at := untaggedMention.FindStringSubmatchIndex(text[pos:])
		if at == nil {
			break
		}
		for i := range at {
			if at[i] >= 0 {
				at[i] += pos
			}
		}
		pos = at[3]
		if !names(text, at[2], at[5], requires) {
			continue
		}
		source, pkg := text[at[2]:at[3]], ""
		segs := strings.Split(strings.TrimPrefix(text[at[4]:at[5]], "/"), "/")
		if slices.Contains(forgeRoutes, segs[0]) {
			// A link to the source's pull requests or issues, which name no
			// package of it.
			segs = nil
		}
		for _, seg := range segs {
			if seg != "" && !majorVersion.MatchString(seg) && !slices.Contains(sources, seg) {
				pkg = path.Join(pkg, seg)
			}
		}
		switch {
		case at[6] >= 0 && symbolLike(text[at[6]:at[7]]):
			out = append(out, mention{at[0], at[7], source, text[at[6]:at[7]]})
			pos = at[7]
		case pkg != "":
			// A package of the source, named by its import path.
			out = append(out, mention{at[0], at[5], source, pkg})
			pos = at[5]
		}
	}
	return out
}

// symbolLike reports whether a word written after a source's name names
// something inside it: a path of packages, or a symbol the grammar's own
// locator would admit that is spelt in mixed case, or in capitals with a digit.
// A version is none of those, and is left to the sweep, which says which edit
// it needs; a word in lower case is prose, and one in capitals alone is
// emphasis. The judgment is spelling, so it has limits both ways — a
// capitalised word opening a clause reads as a symbol, and a one-word package
// in lower case reads as prose — and they are the price of seeing a claim with
// no file at all without opening the module, which this rung never does.
func symbolLike(word string) bool {
	if packagePath.MatchString(word) {
		return true
	}
	if !symbolRe.MatchString(word) || majorVersion.MatchString(word) {
		return false
	}
	upper := strings.ContainsFunc(word, unicode.IsUpper)
	return upper && strings.ContainsFunc(word, func(r rune) bool { return unicode.IsLower(r) || unicode.IsDigit(r) })
}

// citedBy reports whether the citation opening at `at` dates mention m: it
// cites m's source, and its locator names what m names. A qualified name is
// named by itself, or by a symbol whose first part is its last part — the way a
// package-qualified name is cited, `jwt.Expected` by `Expected.Time`, though
// spelling cannot tell a package from a type — and never by another type's
// member of the same name: not `JSONWebKey.UnmarshalJSON` by
// `OtherType.UnmarshalJSON`. A bare name or a package path is named by a
// part of a symbol, of the file's path, or of a schema path. A citation of
// another symbol would leave this one free to vanish, since rung 2 resolves
// only what a citation names.
func citedBy(text string, at int, bounds []int, m mention) bool {
	// ParseCitation reads a citation only from its first byte, so a head
	// further on parses nothing here.
	h := datedHead.FindStringIndex(text[at:])
	if h == nil {
		return false
	}
	_, end, atHead := clause(text, at+h[1], nextHead(bounds, at+h[1]))
	c := ParseCitation(trimConnective(text[at:end], atHead))
	if c == nil || c.Source != m.source {
		return false
	}
	name := m.name[strings.LastIndexAny(m.name, "./")+1:]
	if strings.Contains(m.name, ".") && !strings.Contains(m.name, "/") {
		for _, s := range c.Loc.Symbols {
			if s == m.name || strings.Split(s, ".")[0] == name {
				return true
			}
		}
		return false
	}
	parts := strings.FieldsFunc(strings.TrimSuffix(c.Loc.File, path.Ext(c.Loc.File))+"."+c.Loc.Path,
		func(r rune) bool { return r == '.' || r == '/' })
	for _, s := range c.Loc.Symbols {
		parts = append(parts, strings.Split(s, ".")...)
	}
	return slices.Contains(parts, name)
}

// ownsTag reports whether text carries a version this grammar governs: one the
// text does not give another project, which the sweep would report.
func (s Scanner) ownsTag(text string) bool {
	for _, at := range anyTag.FindAllStringIndex(text, -1) {
		m := nameBefore.FindStringSubmatchIndex(text[:at[0]])
		if m == nil || !attributedElsewhere(text[m[2]:m[3]], s.Requires) {
			return true
		}
	}
	return false
}

func nextHead(starts []int, after int) int {
	for _, s := range starts {
		if s > after {
			return s
		}
	}
	return -1
}

// classifyLocator names the defect in a locator that opened as a citation and
// then did not parse, under the form and source it was written with. Each
// branch is a different edit for whoever migrates it.
func classifyLocator(loc, whole, form, source string) Finding {
	head, reason, hasReason := cutSpanReason(loc)
	// Not a span, so the file is whatever the locator opens with.
	file, _, _ := strings.Cut(loc, " ")
	if hasReason {
		// A span's file runs to its colon, or to the space written where the
		// colon belongs.
		file, _, _ = strings.Cut(head, ":")
		file, _, _ = strings.Cut(file, " ")
	}
	switch {
	case form == "absent at" && (hasReason || unreasonedSpan.MatchString(loc)):
		// Asked before whether the span has a reason, because none would make
		// it legal: telling the author to add one would only send them back.
		return Finding{Rule: "span-absent", Msg: fmt.Sprintf(
			"%q claims a range of lines is absent, which nothing can contradict: a range "+
				"that encloses nothing reads the same whether its declaration left or was "+
				"never there, so an `absent at` claim takes a symbol or a schema path",
			strings.TrimSpace(whole))}
	case file == "spec" && source != SpecSource:
		return Finding{Rule: "spec-not-bundled", Msg: fmt.Sprintf(
			"%q gives a schema path into %s, which bundles no spec: only %s does, so a "+
				"claim about %s anchors on a symbol", strings.TrimSpace(whole), source, SpecSource, source)}
	case hasReason && SpecLocator(file):
		return Finding{Rule: "span-on-spec", Msg: fmt.Sprintf(
			"%q spans lines of an OpenAPI document, which is a tree: every cited line has "+
				"an enclosing node, so a spec citation takes a schema path and no span "+
				"reason earns the fallback there", strings.TrimSpace(whole))}
	case !hasReason && SpecLocator(file):
		return Finding{Rule: "symbol-on-spec", Msg: fmt.Sprintf(
			"%q names an OpenAPI document and then a symbol: a spec has no Go "+
				"declarations to anchor on, so a spec citation takes `spec <schema path>`",
			strings.TrimSpace(whole))}
	case hasReason && !spanReasons[reason]:
		return Finding{Rule: "span-reason-unknown", Msg: fmt.Sprintf(
			"%q gives the reason %q, which is not in the closed set (%s): a reason no "+
				"parser can check would let the span keep its line numbers forever",
			strings.TrimSpace(whole), reason, strings.Join(sortedReasons(), ", "))}
	case hasReason && spanRe.MatchString(head) && !reasonFits(file, reason):
		return impossibleReason(strings.TrimSpace(whole), file, reason)
	case unreasonedSpan.MatchString(loc):
		return Finding{Rule: "span-unreasoned", Msg: fmt.Sprintf(
			"%q falls back to a line span without saying why: the fallback is earned, "+
				"so add `(span: <reason>)` from the closed set", strings.TrimSpace(whole))}
	case singleLine.MatchString(loc):
		return Finding{Rule: "bare-line", Msg: fmt.Sprintf(
			"%q locates a single line, which names no declaration a parser can follow "+
				"across a bump: name the symbol instead", strings.TrimSpace(whole))}
	}
	return Finding{Rule: "locator-unreadable", Msg: fmt.Sprintf(
		"%q is stamped but its locator is neither a symbol, a reasoned span, nor a "+
			"schema path", strings.TrimSpace(whole))}
}

// impossibleReason is the finding for a span reason the file's own kind
// contradicts, worded for the way it does.
func impossibleReason(quote, file, reason string) Finding {
	msg := fmt.Sprintf("%q gives %q over %s, which no Go parser reads, so there is no "+
		"declaration for that reason to be about: name a symbol the file documents instead",
		quote, reason, file)
	switch {
	case reason == "non-go" && strings.HasSuffix(file, ".go"):
		msg = fmt.Sprintf("%q says %s has no structure a parser can read, and it is Go: name "+
			"the symbol, or give the reason a Go range can truthfully have", quote, file)
	case reason == "non-go":
		msg = fmt.Sprintf("%q says %s has no structure a parser can read, and a file of that "+
			"kind is data a parser reads: `non-go` is for prose alone, so name what the file "+
			"declares, or leave a claim about it out of this grammar", quote, file)
	}
	return Finding{Rule: "span-reason-impossible", Msg: msg}
}

func sortedReasons() []string {
	// The set is small and fixed; naming it in the plan's own order reads
	// better in a message than map iteration would.
	return []string{"crosses-declarations", "no-unique-name", "non-go"}
}

// clause returns the text from at to the end of the clause it opens, that end
// offset, and whether it stopped at the next citation's head rather than at
// punctuation. A locator ends at a comma, a semicolon, a sentence stop, or the
// start of the next citation — but not at a comma inside a parenthetical,
// because the corpus writes `BetaDeploymentService (New, Update, List)` and
// cutting at the first comma there reports half a symbol as the whole citation.
// A closing parenthesis that opened before the locator did ends it, since the
// locator is inside someone else's aside and the rest of that aside is not part
// of the citation.
//
// limit is the offset of the next citation head, or -1 for none.
func clause(text string, at, limit int) (string, int, bool) {
	end := len(text)
	atHead := limit >= 0 && limit < end
	if atHead {
		end = limit
	}
	depth := 0
	for i := at; i < end; i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return trimClause(text[at:i]), i, false
			}
			depth--
		case ',', ';':
			if depth == 0 {
				return trimClause(text[at:i]), i, false
			}
		case '.':
			// A sentence stop, not the dot inside `Type.Member` or `file.go`.
			if depth == 0 && (i+1 >= len(text) || text[i+1] == ' ' || text[i+1] == '\n') {
				return trimClause(text[at:i]), i, false
			}
		}
	}
	return trimClause(text[at:end]), end, atHead
}

// trimClause drops the punctuation a clause may end on. It never touches a
// word: the locator's last token is a symbol, and this corpus is not the place
// to decide that a declaration cannot be called what it is called.
func trimClause(s string) string {
	s = strings.TrimSpace(s)
	for _, tail := range []string{" —", ";", ",", "—"} {
		if strings.HasSuffix(s, tail) {
			return strings.TrimSpace(strings.TrimSuffix(s, tail))
		}
	}
	return s
}

// trimConnective additionally drops the word that joined this clause to
// whatever follows, and is used only where the cut was made at the next
// citation's head. Two citations written side by side are separated there,
// which leaves the `and` between them dangling on the end of the first — text
// belonging to neither, and enough to make the first one unparseable. On any
// other cut the last word is the locator's own, so removing it there would
// silently truncate a symbol named `and` or `or`.
func trimConnective(s string, atHead bool) string {
	s = trimClause(s)
	if !atHead {
		return s
	}
	for _, tail := range []string{" and", " or"} {
		if strings.HasSuffix(s, tail) {
			return strings.TrimSpace(strings.TrimSuffix(s, tail))
		}
	}
	return s
}

func blankEmphasis(s string) string { return strings.ReplaceAll(s, "*", " ") }

func mark(consumed []bool, from, to int) {
	for i := from; i < to && i < len(consumed); i++ {
		consumed[i] = true
	}
}

func anyConsumed(consumed []bool, from, to int) bool {
	for i := from; i < to && i < len(consumed); i++ {
		if consumed[i] {
			return true
		}
	}
	return false
}

// Citations returns every conforming citation in a document, with its line.
//
// It shares datedHead and clause with Shape on purpose: the two halves of one
// scan must agree about where a citation begins and ends, or a clause could be
// reported as malformed by rung 1 and resolved as well-formed by rung 2.
func Citations(src string) []Citation {
	var out []Citation
	for i, line := range strings.Split(src, "\n") {
		for _, c := range CitationsIn(line) {
			c.Line, c.Unit = i+1, i+1
			out = append(out, c)
		}
	}
	return out
}

// CitationsIn returns every conforming citation in one piece of text, each
// carrying the offset it starts at.
func CitationsIn(line string) []Citation {
	text := blankEmphasis(line)
	bounds := headStarts(text)
	var out []Citation
	for _, at := range datedHead.FindAllStringIndex(text, -1) {
		if negated.MatchString(text[:at[0]]) {
			continue // a negated form claims nothing, and rung 1 says so
		}
		_, end, atHead := clause(text, at[1], nextHead(bounds, at[1]))
		if c := ParseCitation(trimConnective(text[at[0]:end], atHead)); c != nil {
			c.at = at[0]
			out = append(out, *c)
		}
	}
	return out
}
