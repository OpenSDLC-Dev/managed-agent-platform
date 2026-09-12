package domain

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// prefixToken matches one prefix as the documents write it: a backticked
// lowercase word ending in an underscore, e.g. `sesn_`. The trailing underscore
// is what separates a prefix from every other backticked identifier on the same
// line, and a pinned line is dense with them: CLAUDE.md carries `knownPrefixes`
// and `internal/domain/id.go` beside the prefixes themselves, and none of those
// is one.
var prefixToken = regexp.MustCompile("`([a-z]+)_`")

// TestDocsEnumerateTheWirePrefixSet holds the documents that spell the
// resource-id prefix set out in full to knownPrefixes, the set every /v1 path
// actually accepts as an id shape.
//
// It exists because the documented set has drifted from the code twice
// (docs/plan/34_doc-trim.md, "Pin the lists that drift, don't just correct
// them"): a prefix is added here for a new resource, and the documents that
// enumerate the whole set go on describing the previous one. Nothing linked
// them, so nothing failed, and correcting them by hand only resets the clock.
//
// The set comes from knownPrefixes itself, not from a copy pinned in this file,
// because a copy would be a third home for the same fact and a third thing to
// drift. What is pinned here is *where* the lists are — the line of each
// document, located by an anchor phrase. That is deliberate: most mentions of a
// prefix in this repo are not a list, and a test that fired on every mention
// would be turned off. docs/ARCHITECTURE.md's wire-compatibility paragraph names
// five prefixes and then an ellipsis, and is right to; an elided list claims
// nothing about completeness and so cannot go stale, which is why it is not
// pinned. A document that stops enumerating the set loses its entry here rather
// than keeping an anchor nothing satisfies — which is what became of
// ARCHITECTURE.md's own entry when plan 34 slice 3 replaced its per-file tables,
// the id.go row among them, with a map. CLAUDE.md is the enumeration now.
//
// One document is enough to catch the drift. The list below carries a tripwire
// for the case where it stops being one: an empty list makes the loop a no-op
// and the test a pass, so it fatals instead. Nothing at runtime can reach that
// branch — the list is a literal — and it is not trying to; it is a note to
// whoever deletes the last entry, at the moment they do it, that removing an
// entry and re-anchoring it are different acts.
//
// The comparison runs in both directions. An omission is the drift that has
// already happened; a prefix the document adds matters just as much, because
// envkey_, principal_ and apikey_ are held out of knownPrefixes on purpose (see
// id.go for why), so a document that lists one of them among the wire prefixes
// tells a reader that /v1 accepts a shape it rejects.
func TestDocsEnumerateTheWirePrefixSet(t *testing.T) {
	want := make([]string, 0, len(knownPrefixes))
	for p := range knownPrefixes {
		want = append(want, p)
	}
	slices.Sort(want)

	docs := []struct{ path, anchor string }{
		{"CLAUDE.md", "**ID prefixes**:"},
	}
	if len(docs) == 0 {
		t.Fatal("no document is pinned to knownPrefixes: the last entry was removed rather " +
			"than re-anchored, so this check reads nothing and passes. Either a document " +
			"still enumerates the set and belongs here, or none does and the set is " +
			"documented nowhere — both need fixing, neither is a pass.")
	}
	for _, doc := range docs {
		b, err := os.ReadFile(filepath.Join(repoRoot(), doc.path))
		if err != nil {
			t.Errorf("read %s: %v", doc.path, err)
			continue
		}
		var carrying []string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, doc.anchor) {
				carrying = append(carrying, line)
			}
		}
		if len(carrying) != 1 {
			t.Errorf("%s has %d lines containing %q, want exactly 1: this test pins the "+
				"prefix list that line carries, and cannot tell which of several is it. "+
				"If the list moved, re-anchor it here; if the document no longer "+
				"enumerates the set, drop its entry from this test.",
				doc.path, len(carrying), doc.anchor)
			continue
		}

		got := make([]string, 0, len(want))
		for _, m := range prefixToken.FindAllStringSubmatch(carrying[0], -1) {
			if !slices.Contains(got, m[1]) {
				got = append(got, m[1])
			}
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			continue
		}

		var omitted, invented []string
		for _, p := range want {
			if !slices.Contains(got, p) {
				omitted = append(omitted, p+"_")
			}
		}
		for _, p := range got {
			if !knownPrefixes[p] {
				invented = append(invented, p+"_")
			}
		}
		t.Errorf("%s enumerates the wire ID-prefix set on its line containing %q, and that "+
			"list disagrees with internal/domain/id.go's knownPrefixes.\n"+
			"  the document omits:            %s\n"+
			"  the document names, code does not accept: %s\n"+
			"  document: %v\n"+
			"      code: %v\n"+
			"knownPrefixes is what a /v1 path accepts as an id shape, so it is the set the "+
			"document has to mirror. Correct the document, not this test — and if a prefix "+
			"is genuinely private (never on the /v1 wire), it belongs out of knownPrefixes "+
			"and out of the list alike.",
			doc.path, doc.anchor, orNone(omitted), orNone(invented), got, want)
	}
}

// orNone renders an empty difference as a word rather than "[]", so a failure
// message reads as a sentence about the one direction that actually broke.
func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, " ")
}

// repoRoot finds the checkout root — the directory holding go.mod — so the
// check reads the documents of the tree it was compiled from (a worktree's own
// CLAUDE.md, not the main checkout's) with no working directory, network or
// fixture needed.
//
// It starts from this file's compile-time path and walks up. Under -trimpath
// that path is module-relative rather than absolute, so the walk starts from
// the test's working directory instead — which `go test` sets to the package
// directory, the same place. Releases build with -trimpath (Makefile,
// Dockerfile) and tests do not, but GOFLAGS carries it into either, and the
// cost of not handling it is a red build blamed on documentation drift.
func repoRoot() string {
	dir := ""
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		dir = filepath.Dir(file)
	} else if wd, err := os.Getwd(); err == nil {
		dir = wd
	}
	for dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// sdkVersionLiteral matches a module version as a document would write one.
// Both cases of the `v`, because a sentence can start with one, and two or three
// components, because dropping the patch is the cheapest way past a guard like
// this and `v1.70` still reads as the pin. A version with no `v` at all is not
// matched: Go module versions carry one by universal convention, while bare
// decimal triples in these documents are far more often a Go release, an image
// tag or a section number, and turning those red would cost more than the
// spelling buys. TestUndatedVersionsRule pins that tradeoff both ways.
var sdkVersionLiteral = regexp.MustCompile(`\b[vV]\d+\.\d+(?:\.\d+)?\b`)

// datingMarker matches the text that may sit before a version and make naming it
// legal: docs/plan/51_sdk-reference-binding.md's closed set of temporal markers,
// then an optional source, and then whatever markdown the house style puts
// around a tag. A marked literal is a claim about that tag and stays true after a
// bump; an unmarked one restates whatever go.mod pins today, and does not.
var datingMarker = regexp.MustCompile("(?i)\\b(?:since|checked against|absent at)" +
	"[\\s`*_\\[(]+(?:[\\w.@/-]+(?:'s|’s)?[\\s`*_\\[(]+)?$")

// sdkEntry matches one module-and-version line, whatever block it sits in.
var sdkEntry = regexp.MustCompile(`^github\.com/anthropics/anthropic-sdk-go(?:/v\d+)? +(v\S+)$`)

// modVerbs are the go.mod directives that open a block or carry an entry inline.
var modVerbs = map[string]bool{
	"module": true, "go": true, "toolchain": true, "godebug": true, "tool": true,
	"ignore": true, "require": true, "exclude": true, "replace": true, "retract": true,
}

// whitespaceRun collapses a document to one line so an anchor phrase survives a
// reflow that rewraps it.
var whitespaceRun = regexp.MustCompile(`\s+`)

// pinnedSDK returns the anthropic-sdk-go version go.mod requires, or "".
//
// It walks the file rather than matching one line, because the module path can
// appear in an `exclude`, `replace` or `retract` block too, and only the
// `require` entry is the pin. A commented-out entry answers for nothing, and a
// file with CRLF endings is read the same as one without.
func pinnedSDK(gomod string) string {
	block := ""
	for _, raw := range strings.Split(gomod, "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case line == ")":
			block = ""
			continue
		}
		if head, rest, ok := strings.Cut(line, " "); ok && modVerbs[head] {
			if strings.TrimSpace(rest) == "(" {
				block = head
				continue
			}
			if m := sdkEntry.FindStringSubmatch(strings.TrimSpace(rest)); head == "require" && m != nil {
				return m[1]
			}
			continue
		}
		if block == "require" {
			if m := sdkEntry.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
	}
	return ""
}

// undatedVersions returns the version literals on one line that nothing dates.
// When markersDate is false the line may carry no version at all, dated or not.
func undatedVersions(line string, markersDate bool) []string {
	var bare []string
	for _, at := range sdkVersionLiteral.FindAllStringIndex(line, -1) {
		if markersDate && datingMarker.MatchString(line[:at[0]]) {
			continue
		}
		bare = append(bare, line[at[0]:at[1]])
	}
	return bare
}

// TestSteeringDocsDoNotPinTheSDKVersion holds the documents that tell a reader
// which anthropic-sdk-go version to judge the wire against to the one place that
// actually knows: go.mod.
//
// It exists because correcting these by hand only resets the clock, the same
// lesson docs/plan/34_doc-trim.md drew for the prefix lists above. #593 moved the
// pin v1.66.0 → v1.70.1 and swept neither document; #667 corrected
// docs/REFERENCE_PROJECTS.md's copy by hand and nothing reached .claude/, so the
// file steering the verifier's wire-compatibility rung went on naming a version
// this repository had stopped building against until #724. Nothing linked the
// copies to the pin, so nothing failed.
//
// Both halves of the check are written to fail closed against wording and
// layout, because the failure that matters is the one nobody sees. The anchor is
// the positive clause each document must still carry, matched against the whole
// document with its whitespace collapsed, so deleting the instruction fails here
// rather than passing over a file that now names no authority at all — which is
// worse than a stale literal, not better — while a reflow that rewraps the
// sentence does not. It is the clause and not a noun phrase from it because
// "do not judge against the SDK version `go.mod` pins" contains the phrase and
// means the opposite. And a version is a finding unless something dates it,
// rather than a finding only when it sits beside a phrase this test recognises:
// a rule keyed on the wording of the defect is disarmed by rewording it, or by a
// reflow that puts the phrase and the version on different lines.
//
// The two documents get different rules, because they are different kinds of
// document. verifier.md is pure instruction — it has no occasion to name a tag at
// all, dated or not, since an instruction to judge against v1.66.0 is wrong the
// moment the pin moves however carefully it is dated. REFERENCE_PROJECTS.md also
// records history, where naming a tag is right; there the rule is that the tag
// must be dated, so the sentence is about that tag rather than about the pin.
func TestSteeringDocsDoNotPinTheSDKVersion(t *testing.T) {
	root := repoRoot()
	if root == "" {
		t.Fatal("repo root not found")
	}

	// The tripwire: the fact must have exactly one home, and this is it. The pin
	// is read for the log line rather than compared against anything — comparing
	// would legalise a literal that happens to equal today's pin, which is the
	// half of #724 that had not gone wrong yet.
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	pin := pinnedSDK(string(gomod))
	if pin == "" {
		t.Fatal("go.mod requires no anthropic-sdk-go: either the dependency is gone, in " +
			"which case these documents pin nothing and this test goes with it, or the " +
			"entry changed shape and pinnedSDK has to learn the new one. Both need a " +
			"person; neither is a pass.")
	}
	t.Logf("go.mod pins anthropic-sdk-go %s", pin)

	for _, doc := range []struct {
		path string
		// anchor is the sentence that sends a reader to go.mod, which the
		// document must still carry somewhere.
		anchor string
		// dated reports whether a temporal marker makes naming a tag legal here.
		dated bool
	}{
		{
			path:   ".claude/agents/verifier.md",
			anchor: "Judge against the SDK version `go.mod` pins — read it there",
		},
		{
			path:   "docs/REFERENCE_PROJECTS.md",
			anchor: "Wire-compat is judged against the SDK version pinned in `go.mod`",
			dated:  true,
		},
	} {
		body, err := os.ReadFile(filepath.Join(root, doc.path))
		if err != nil {
			t.Errorf("%s: %v", doc.path, err)
			continue
		}

		if !strings.Contains(whitespaceRun.ReplaceAllString(string(body), " "), doc.anchor) {
			t.Errorf("%s no longer contains %q: this test pins the sentence that sends a "+
				"reader to go.mod for the SDK version, and a document naming no authority "+
				"at all is worse than one naming a stale tag, not better. If the sentence "+
				"was reworded, re-anchor it here; if the document no longer steers that "+
				"judgment, drop its entry from this test.", doc.path, doc.anchor)
		}

		for i, line := range strings.Split(string(body), "\n") {
			bare := undatedVersions(line, doc.dated)
			if len(bare) == 0 {
				continue
			}
			why := "this document is an operational instruction, so it has no occasion to " +
				"name a tag at all: say that go.mod pins the version and let a reader read " +
				"the value there"
			if doc.dated {
				why = "date it — `since`, `checked against` or `absent at` immediately " +
					"before the source and tag, per docs/plan/51_sdk-reference-binding.md " +
					"— or say that go.mod pins the version and let a reader read the value " +
					"there"
			}
			t.Errorf("%s:%d names %s, which nothing dates; %s, so this cannot rot the way "+
				"#724 did:\n\t%s", doc.path, i+1, strings.Join(bare, " and "), why,
				strings.TrimSpace(line))
		}
	}
}

// TestUndatedVersionsRule pins what the rule above counts as an undated version.
//
// It exists because the documents that rule scans are clean by construction, so
// running it over them proves only that it matched nothing — which is exactly
// what every hole in it also looks like. The cases below are the shapes #724 took
// and the shapes review found it could take next, in both polarities: what must
// be caught, and what must stay legal so the rule is not turned off instead.
func TestUndatedVersionsRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		line  string
		dated bool
		want  []string
	}{
		{"the defect as it stood", "Judge against the SDK version pinned in `go.mod` (v1.66.0) — post-pin", false, []string{"v1.66.0"}},
		{"the same defect while the value still happens to be right", "judged against the SDK version pinned in `go.mod` (v1.70.1) —", true, []string{"v1.70.1"}},
		{"a reflow that leaves the version on its own line", "(v1.70.1) — and because the pin moves, a registry entry", true, []string{"v1.70.1"}},
		{"the claim reworded past any phrase list", "judged against `go.mod`'s pin, currently v1.70.1.", true, []string{"v1.70.1"}},
		{"the patch component dropped", "the version `go.mod` pins (v1.70)", true, []string{"v1.70"}},
		{"a capitalised v, as a sentence would start", "V1.66.0 is what the pin was", true, []string{"V1.66.0"}},
		{"every version on a line, not just the first", "the pin moved v1.66.0 → v1.70.1", true, []string{"v1.66.0", "v1.70.1"}},
		{"a word that merely contains a marker does not date one", "the shape is unchecked against v1.63.0", true, []string{"v1.63.0"}},
		{"a dated tag, which is what keeps history legal", "*Evidence: checked against anthropic-sdk-go v1.63.0 betaagent.go:484-570", true, nil},
		{"a dated tag in the house style, backticked", "checked against `anthropic-sdk-go v1.63.0`", true, nil},
		{"a dated tag emphasised, or linked", "**since anthropic-sdk-go v1.59.0** and [absent at v1.58.0](x)", true, nil},
		{"a dated tag from a source that is not the SDK", "checked against go-jose v4.1.4 for the JWKS cache", true, nil},
		{"each tag needs its own marker, not one for the list", "checked against go-jose v4.1.4 and anthropic-cli v0.5.0", true, []string{"v0.5.0"}},
		{"a dated tag with a possessive source", "since anthropic-sdk-go's v1.63.0 the field is typed", true, nil},
		{"a dated tag without the module name", "the field is absent at v1.59.0 and arrives later", true, nil},
		{"a dated tag where no tag may be named at all", "checked against anthropic-sdk-go v1.63.0", false, []string{"v1.63.0"}},
		{"prose that only looks numeric", "the gate is 90.21% of statements, some 1.5 times the packages", true, nil},
		{"a bare decimal triple is deliberately not a version here", "the Go version is 1.26.0 and the chart is 0.12.0", true, nil},
		{"a file coordinate is not a version", "betaagent.go:484-570 and api.md:884", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := undatedVersions(tc.line, tc.dated); !slices.Equal(got, tc.want) {
				t.Errorf("undatedVersions(%q, %v) = %v, want %v", tc.line, tc.dated, got, tc.want)
			}
		})
	}
}

// TestPinnedSDKReadsTheRequireEntry pins which go.mod line answers for the pin.
//
// The premise guard above is a `t.Fatal`, so a shape this misreads does not
// weaken the check, it switches the check off — and a shape it reads from the
// wrong block reports a version the build never used. Both are silent, which is
// why the shapes are asserted here rather than reasoned about in a comment.
func TestPinnedSDKReadsTheRequireEntry(t *testing.T) {
	const block = "module example.com/m\n\ngo 1.26.0\n\nrequire (\n\tgithub.com/anthropics/anthropic-sdk-go v1.70.1\n\tgithub.com/other/thing v2.0.0\n)\n"
	for _, tc := range []struct{ name, gomod, want string }{
		{"the require block this repository uses", block, "v1.70.1"},
		{"the same file with CRLF endings", strings.ReplaceAll(block, "\n", "\r\n"), "v1.70.1"},
		{"a single-line require", "module example.com/m\n\nrequire github.com/anthropics/anthropic-sdk-go v1.70.1\n", "v1.70.1"},
		{"an indirect marker", "require (\n\tgithub.com/anthropics/anthropic-sdk-go v1.70.1 // indirect\n)\n", "v1.70.1"},
		{"a major-version suffix", "require (\n\tgithub.com/anthropics/anthropic-sdk-go/v2 v2.0.1\n)\n", "v2.0.1"},
		{"a commented-out entry answers for nothing", "require (\n\t// github.com/anthropics/anthropic-sdk-go v1.70.1\n)\n", ""},
		{"an exclude block is not the pin", "exclude (\n\tgithub.com/anthropics/anthropic-sdk-go v1.69.0\n)\n", ""},
		{"a retract block is not the pin", "retract (\n\tgithub.com/anthropics/anthropic-sdk-go v1.69.0\n)\n", ""},
		{"a replace block is not the pin", "replace (\n\tgithub.com/anthropics/anthropic-sdk-go v1.60.0 => ../fork\n)\n", ""},
		{"a single-line replace is not the pin", "replace github.com/anthropics/anthropic-sdk-go v1.60.0 => ../fork\n", ""},
		{"the require wins over an exclude naming the same module", "exclude (\n\tgithub.com/anthropics/anthropic-sdk-go v1.69.0\n)\n" + block, "v1.70.1"},
		{"a renamed dependency answers for nothing", strings.ReplaceAll(block, "anthropic-sdk-go", "renamed-sdk-go"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedSDK(tc.gomod); got != tc.want {
				t.Errorf("pinnedSDK() = %q, want %q", got, tc.want)
			}
		})
	}
}
