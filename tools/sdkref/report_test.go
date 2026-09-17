package main

import (
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env builds an Env over the fixture module, so the resolving rungs are tested
// against source this test controls. The pin is whatever the fixture says it is;
// what matters is that a stamp equal to it takes the failing path and a stamp
// unequal to it does not. The module cache is stubbed empty by default: whether
// an older tag can be opened must be the test's decision and never the disk's.
func env(t *testing.T, pin string) *Env {
	t.Helper()
	return envWithCache(t, pin, nil)
}

func envWithCache(t *testing.T, pin string, cached map[string]string) *Env {
	t.Helper()
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return &Env{
		Pin:         pin,
		resolvers:   map[string]*Resolver{"anthropic-sdk-go": r},
		versions:    map[string]string{"anthropic-sdk-go": pin},
		unreachable: map[string]string{},
		older:       map[string]*Resolver{},
		olderErr:    map[string]string{},
		cache: func(path, version string) (string, error) {
			if dir, ok := cached[version]; ok {
				return dir, nil
			}
			return "", fmt.Errorf("%s@%s is not unpacked in the module cache", path, version)
		},
	}
}

// olderFixture is a second module tree standing in for a tag the cache happens
// to hold. It is deliberately not the pin's shape, so a span falsified against
// it can only have been read there.
func olderFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := "package sdk\n\ntype OnlyOneThing struct {\n\tA string\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "betaagent.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestResolutionPolarity pins rung 2. The negative polarity is the half that is
// easy to leave out and impossible to notice missing: an `absent at` claim that
// silently starts resolving again looks exactly like a clean run.
func TestResolutionPolarity(t *testing.T) {
	const pin = "v1.70.1"
	for _, tc := range []struct {
		name  string
		in    string
		rules []string
	}{
		{
			name: "a positive claim that resolves is clean",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams",
		},
		{
			name:  "a positive claim that does not resolve",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion",
			rules: []string{"vanished-at-stamp"},
		},
		{
			name: "a negative claim that does not resolve is clean",
			in:   "absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion",
		},
		{
			name:  "a negative claim that resolves again",
			in:    "absent at anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams",
			rules: []string{"returned-at-stamp"},
		},
		{
			name:  "a symbol the file declares twice",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go init",
			rules: []string{"ambiguous-symbol"},
		},
		{
			name: "a stamp that is not the pin is rung 3's, not rung 2's",
			in:   "checked against anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion",
		},
		{
			name: "both symbols of a two-symbol anchor resolve",
			in: "checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams " +
				"and BetaManagedAgentsWebFetchToolConfig",
		},
		{
			name: "one gone in a two-symbol anchor is the citation being wrong",
			in: "checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams " +
				"and resolveSkillVersion",
			rules: []string{"vanished-at-stamp"},
		},
		{
			name:  "a schema path against a source that bundles no spec",
			in:    "checked against anthropic-sdk-go v1.70.1 — spec components.schemas.BetaSession",
			rules: []string{"unresolvable"},
		},
		{
			// A negative claim over two symbols says neither is there, so one
			// that resolves falsifies it. Read with the positive quantifier, the
			// other one's absence hid it.
			name: "one back in a two-symbol absent-at anchor is the citation being wrong",
			in: "absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion " +
				"and BetaAgentNewParams",
			rules: []string{"returned-at-stamp"},
		},
		{
			name: "both absent in a two-symbol absent-at anchor is clean",
			in:   "absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion and noSuchHelper",
		},
		{
			// Whichever of the two declarations resolved, the name is there.
			name:  "an absent-at anchor on a name declared twice is back, not ambiguous",
			in:    "absent at anthropic-sdk-go v1.70.1 — betaagent.go init",
			rules: []string{"returned-at-stamp"},
		},
		{
			name:  "a positive anchor on a file the module does not ship",
			in:    "checked against anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams",
			rules: []string{"vanished-at-stamp"},
		},
		{
			// Nothing in a missing file resolves, so this could never be
			// contradicted — which is not the same as being true.
			name:  "an absent-at anchor on a file the module does not ship",
			in:    "absent at anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams",
			rules: []string{"unresolvable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseCitation(tc.in)
			if c == nil {
				t.Fatalf("ParseCitation(%q) = nil; the case cannot test rung 2", tc.in)
			}
			var got []string
			for _, f := range env(t, pin).Resolution([]Citation{*c}) {
				got = append(got, f.Rule)
			}
			if strings.Join(got, ",") != strings.Join(tc.rules, ",") {
				t.Errorf("Resolution(%q) rules = %v, want %v", tc.in, got, tc.rules)
			}
		})
	}
}

// TestAmbiguityBehindADocumentationFileNamesThePackages. A non-Go anchor is
// answered by the packages the document links it into, so a message blaming the
// file the citation wrote would send the migrator to a file that declares the
// symbol nowhere — and would offer a span fallback that `DeclsIn` refuses
// outright.
func TestAmbiguityBehindADocumentationFileNamesThePackages(t *testing.T) {
	// MarshalJSON is declared twice in the fixture's source, on two receivers.
	c := ParseCitation("checked against anthropic-sdk-go v1.70.1 — api.md MarshalJSON")
	if c == nil {
		t.Fatal("the case cannot run: the clause did not parse")
	}
	got := env(t, "v1.70.1").Resolution([]Citation{*c})
	if len(got) != 1 || got[0].Rule != "ambiguous-symbol" {
		t.Fatalf("Resolution = %v, want the ambiguity reported", got)
	}
	if strings.Contains(got[0].Msg, "api.md declares") {
		t.Errorf("the message blames api.md, which declares nothing: %s", got[0].Msg)
	}
	if !strings.Contains(got[0].Msg, "the packages api.md links it into declare") {
		t.Errorf("the message does not name where the count came from: %s", got[0].Msg)
	}
	if strings.Contains(got[0].Msg, "no-unique-name") {
		t.Errorf("the message offers a span fallback, which no parser can read on a "+
			"documentation file: %s", got[0].Msg)
	}
}

// TestSpanReasonFalsification pins the half of rung 1 that needs the source. A
// reason nothing can contradict is a way out of the migration, so each member of
// the closed set is checked against what the parser actually finds — and so is
// every way a span can turn out to be uncheckable, because a rung that says
// nothing about input it never read is a clean bill of health it did not earn.
func TestSpanReasonFalsification(t *testing.T) {
	const pin = "v1.70.1"
	for _, tc := range []struct {
		name  string
		in    string
		rules []string
	}{
		{
			name: "crosses-declarations over a range that does",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-13 (span: crosses-declarations)",
		},
		{
			name:  "crosses-declarations over a range that does not",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: crosses-declarations)",
			rules: []string{"span-reason-false"},
		},
		{
			// A file no parser reads can only truthfully give `non-go`, and over
			// lines that name nothing the module declares, giving it is enough.
			name: "non-go over a file that is not Go",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:1-2 (span: non-go)",
		},
		{
			// The index names this symbol by a link fragment, as the real one
			// names all of them. A span over it has a symbol to anchor on.
			name:  "non-go over documentation lines that name a declared symbol",
			in:    "checked against anthropic-sdk-go v1.70.1 — api.md:5-5 (span: non-go)",
			rules: []string{"span-reason-false"},
		},
		{
			// A capitalised word in prose is not structure a parser can read,
			// even when the module declares a symbol of that name.
			name: "non-go over a name written as prose rather than linked",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:3-3 (span: non-go)",
		},
		{
			// The link is structure, but to a symbol the module does not
			// declare there: nothing the migrator could anchor on instead.
			name: "non-go over a link to a symbol the module does not declare",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:4-4 (span: non-go)",
		},
		{
			// The fixture declares a `New`, but `errors#New` is the standard
			// library's: a link without a module's domain is not the module's.
			name: "non-go over a link into the standard library",
			in:   "checked against anthropic-sdk-go v1.70.1 — api.md:6-6 (span: non-go)",
		},
		{
			name:  "non-go over a range past the end of that file",
			in:    "checked against anthropic-sdk-go v1.70.1 — api.md:3-99 (span: non-go)",
			rules: []string{"span-past-eof"},
		},
		{
			name: "crosses-declarations over a grouped const block, which it does",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:32-34 (span: crosses-declarations)",
		},
		{
			name: "no-unique-name over a repeated name",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:14-14 (span: no-unique-name)",
		},
		{
			name:  "no-unique-name over a name declared once",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: no-unique-name)",
			rules: []string{"span-reason-false"},
		},
		{
			name: "no-unique-name over a method, which its receiver makes unique",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:8-8 (span: no-unique-name)",
			// MarshalJSON repeats across the SDK but BetaAgentNewParams.MarshalJSON
			// does not, and the qualified name is the one a citation would write.
			rules: []string{"span-reason-false"},
		},
		{
			name:  "a range wholly past the end of the file",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go:9000-9001 (span: crosses-declarations)",
			rules: []string{"span-past-eof"},
		},
		{
			// Inside the file, and still enclosing nothing: the blank line
			// between the package clause and the first declaration.
			name:  "a range inside the file that encloses no declaration",
			in:    "checked against anthropic-sdk-go v1.70.1 — betaagent.go:2-2 (span: crosses-declarations)",
			rules: []string{"span-encloses-nothing"},
		},
		{
			// Not "could not be checked": at its own tag the file is gone, which
			// contradicts the span as surely as a false reason does.
			name:  "a span on a file the module does not ship",
			in:    "checked against anthropic-sdk-go v1.70.1 — nosuch.go:1-2 (span: crosses-declarations)",
			rules: []string{"span-not-shipped"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseCitation(tc.in)
			if c == nil {
				t.Fatalf("ParseCitation(%q) = nil; the case cannot test the falsifier", tc.in)
			}
			var got []string
			for _, f := range env(t, pin).Resolution([]Citation{*c}) {
				got = append(got, f.Rule)
			}
			if strings.Join(got, ",") != strings.Join(tc.rules, ",") {
				t.Errorf("Resolution(%q) rules = %v, want %v", tc.in, got, tc.rules)
			}
		})
	}
}

// TestBumpReport pins rung 3: the two transitions, whatever the stamp, and the
// sections that must be printed even when empty. A section that disappears when
// it has nothing to say cannot be told from one that never ran.
func TestBumpReport(t *testing.T) {
	const pin = "v1.70.1"
	cs := citations(t,
		// stamped behind the pin, and gone there: the transition the plan exists for
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion",
		// stamped behind the pin and still present: lag only
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams",
		// absent at an old tag, and still absent from the file it names
		"absent at anthropic-sdk-go v1.66.0 — betaagent.go BetaEnvironment",
		// absent at an old tag, back at the pin
		"absent at anthropic-sdk-go v1.66.0 — betaenvironment.go BetaEnvironment",
		// a checkout no module carries
		"checked against anthropic-cli v0.5.0 — main.go run",
	)
	rep := env(t, pin).Bump(cs)

	if len(rep.Vanished) != 1 || !strings.Contains(rep.Vanished[0].Msg, "resolveSkillVersion") {
		t.Errorf("Vanished = %v, want the one anchor gone at the pin", rep.Vanished)
	}
	if len(rep.Returned) != 1 || !strings.Contains(rep.Returned[0].Msg, "betaenvironment.go") {
		// BetaEnvironment lives in betaenvironment.go, not betaagent.go, so the
		// betaagent.go claim is still true — which is the point of naming the file.
		t.Errorf("Returned = %v, want only the betaenvironment.go anchor", rep.Returned)
	}
	if len(rep.Lag) != 1 || !strings.Contains(rep.Lag[0], "v1.66.0") {
		t.Errorf("Lag = %v, want one group behind the pin", rep.Lag)
	}
	if len(rep.Uncheckable) == 0 {
		t.Fatal("Uncheckable is empty, but an anthropic-cli citation cannot be resolved: " +
			"a rung that emits nothing for input it never read is a clean bill of health " +
			"it did not earn")
	}
	// And a listing has to name what it covers. "1 citation was not checked"
	// leaves a reader with nothing to open, which is the same silence one step
	// further on.
	if got := rep.Uncheckable[0].Anchors; len(got) != 1 || !strings.Contains(got[0], "anthropic-cli") {
		t.Errorf("Uncheckable[0].Anchors = %v, want the citation itself named", got)
	}
	if !strings.Contains(rep.Uncheckable[0].String(), "anthropic-cli v0.5.0") {
		t.Errorf("the rendered entry does not name its anchor: %s", rep.Uncheckable[0])
	}
	out := rep.String()
	// Plan 51 specifies three lists and, under them, what went unchecked; the
	// transitions are split by whether a citation has dispositioned them.
	for _, section := range []string{
		"transitions awaiting a disposition",
		"transitions already dispositioned",
		"line spans the sources contradict",
		"stamps behind the pin",
		"not checked, and why",
	} {
		if strings.Count(out, "\n"+section) != 1 {
			t.Errorf("report does not carry the %q section exactly once:\n%s", section, out)
		}
	}
	// Both polarities of a transition are one list, and both reach it.
	if at := strings.Index(out, "transitions awaiting a disposition"); at < 0 ||
		!strings.Contains(out[at:], "resolveSkillVersion") ||
		!strings.Contains(out[at:], "betaenvironment.go BetaEnvironment") {
		t.Errorf("the transitions list does not carry both polarities:\n%s", out)
	}
	if n := strings.Count(out, "\n\n"); n != 5 {
		t.Errorf("report has %d sections, want five:\n%s", n, out)
	}
	if !strings.Contains(out, "none") {
		t.Errorf("an empty section printed no placeholder, so it cannot be told from a "+
			"section that never ran:\n%s", out)
	}
}

// TestBumpReportAnswersOnlyWhatThePinCan. Each of these used to reach a
// transition or pass in silence, and each is a question the pin cannot settle:
// a stamp from after it, a name it declares twice, a claim about a file it does
// not have.
func TestBumpReportAnswersOnlyWhatThePinCan(t *testing.T) {
	const pin = "v1.70.1"
	rep := env(t, pin).Bump(citations(t,
		// Written against a newer checkout: a symbol added after the pin reads
		// there as vanished.
		"checked against anthropic-sdk-go v1.71.0 — betaagent.go resolveSkillVersion",
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go init",
		"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams",
		"absent at anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams",
		"absent at anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion and BetaAgentNewParams",
	))
	if len(rep.Vanished) != 1 || !strings.Contains(rep.Vanished[0].Msg, "ships no nosuch.go") {
		t.Errorf("Vanished = %v, want only the positive anchor whose file is gone: the stamp "+
			"ahead of the pin is not a transition", rep.Vanished)
	}
	if len(rep.Returned) != 1 || !strings.Contains(rep.Returned[0].Msg, "and BetaAgentNewParams") {
		t.Errorf("Returned = %v, want the two-symbol absent-at anchor one of whose symbols is back",
			rep.Returned)
	}
	for _, why := range []string{"ahead of the pin", "more than once", "does not ship"} {
		var n int
		for _, u := range rep.Uncheckable {
			if strings.Contains(u.Why, why) {
				n += len(u.Anchors)
			}
		}
		if n != 1 {
			t.Errorf("Uncheckable carries %d anchor(s) for %q, want 1: %v", n, why, rep.Uncheckable)
		}
	}
	if strings.Contains(strings.Join(rep.Lag, "\n"), "v1.71.0") {
		t.Errorf("Lag = %v: a stamp ahead of the pin is not behind it", rep.Lag)
	}
}

// TestNewer. The pin is whatever go.mod holds, which may be a pre-release or a
// pseudo-version, and the release with the same numbers comes after either.
func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		tag, pin string
		want     bool
	}{
		{"v1.71.0", "v1.70.1", true},
		{"v1.66.0", "v1.70.1", false},
		{"v1.100.0", "v1.99.9", true}, // numeric, not lexical
		{"v2.0.0", "v1.70.1", true},
		{"v1.70.2", "v1.70.2-0.20260901000000-abcdef012345", true},
		{"v1.70.1", "v1.70.2-0.20260901000000-abcdef012345", false},
		{"v1.70.10", "v1.70.9", true}, // a longer number is a larger one
		// Too large for any integer, and still the later release: converted, it
		// read as zero and was counted as lag.
		{"v99999999999999999999.0.0", "v1.70.1", true},
		{"v1.70.1", "v99999999999999999999.0.0", false},
	} {
		if got := newer(tc.tag, tc.pin); got != tc.want {
			t.Errorf("newer(%s, %s) = %v, want %v", tc.tag, tc.pin, got, tc.want)
		}
	}
}

// TestAnOlderSpanIsFalsifiedWhereTheCacheHoldsIt is decision 3's "reported when
// available" in both directions. A span's numbers mean nothing at the pin, so
// the only place its reason can be contradicted is the tag it was written for —
// and the only honest thing to do when that tag is not on the machine is to say
// so, since a gate that read whatever a developer's disk happened to carry would
// pass or fail by accident.
func TestAnOlderSpanIsFalsifiedWhereTheCacheHoldsIt(t *testing.T) {
	const span = "checked against anthropic-sdk-go v1.66.0 — betaagent.go:3-5 (span: crosses-declarations)"
	cs := citations(t, span)

	t.Run("available", func(t *testing.T) {
		e := envWithCache(t, "v1.70.1", map[string]string{"v1.66.0": olderFixture(t)})
		rep := e.Bump(cs)
		if len(rep.Contradicted) != 1 || !strings.Contains(rep.Contradicted[0].Msg, "it covers 1") {
			t.Fatalf("Contradicted = %v, want the span reason falsified at v1.66.0, where "+
				"the range covers a single declaration", rep.Contradicted)
		}
		if len(rep.Uncheckable) != 0 {
			t.Errorf("Uncheckable = %v, want none: the tag was available", rep.Uncheckable)
		}
	})

	t.Run("not available", func(t *testing.T) {
		rep := env(t, "v1.70.1").Bump(cs)
		if len(rep.Contradicted) != 0 {
			t.Errorf("Contradicted = %v, want none: nothing could read v1.66.0", rep.Contradicted)
		}
		if len(rep.Uncheckable) != 1 || !strings.Contains(rep.Uncheckable[0].Why, "v1.66.0") {
			t.Errorf("Uncheckable = %v, want the span named as unread rather than passed over",
				rep.Uncheckable)
		}
	})

	t.Run("lag counts a span nobody could read", func(t *testing.T) {
		rep := env(t, "v1.70.1").Bump(cs)
		if len(rep.Lag) != 1 {
			t.Errorf("Lag = %v, want the stamp counted: how far the corpus has drifted is a "+
				"property of the stamp, not of whether the tag could be opened", rep.Lag)
		}
	})
}

// TestAPinStampedSpanReachesTheReport. Rung 2 falsifies it, but the report is
// read on its own after a bump, and a span that appeared in no section of it
// would be invisible to the person doing the dispositioning.
func TestAPinStampedSpanReachesTheReport(t *testing.T) {
	cs := citations(t,
		"checked against anthropic-sdk-go v1.70.1 — betaagent.go:3-6 (span: crosses-declarations)")
	rep := env(t, "v1.70.1").Bump(cs)
	if len(rep.Contradicted) != 1 {
		t.Errorf("Contradicted = %v, want the pin-stamped span whose reason is false",
			rep.Contradicted)
	}
}

// citations parses one citation per line, each in its own unit, so no two of
// them are read as written beside each other.
func citations(t *testing.T, lines ...string) []Citation {
	t.Helper()
	var out []Citation
	for i, l := range lines {
		c := ParseCitation(l)
		if c == nil {
			t.Fatalf("ParseCitation(%q) = nil", l)
		}
		c.Unit = i + 1
		out = append(out, *c)
	}
	return out
}

// TestCitationsAgreeWithShape holds the two halves of one scan to the same
// answer. If Shape reported a clause that Citations also parsed, one of them is
// wrong about where the citation is — and the corpus would be judged twice, by
// two rungs that disagree.
func TestCitationsAgreeWithShape(t *testing.T) {
	const src = `- **entry** — *Evidence: checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams; anthropic-sdk-go v1.66.0 poller.go:492-518.*`
	shape := ShapeAll(src)
	cites := Citations(src, nil)
	if len(cites) != 1 {
		t.Fatalf("Citations = %d, want the one conforming clause: %+v", len(cites), cites)
	}
	if len(shape) != 1 || shape[0].Rule != "undated" {
		t.Fatalf("ShapeAll = %v, want exactly the unmigrated clause", shape)
	}
	if cites[0].Loc.Symbols[0] != "BetaAgentNewParams" {
		t.Errorf("the conforming citation resolved to %q", cites[0].Loc.Desc())
	}
}

// TestCitationsBoundAClauseWhereShapeDoes. A required module ending in a
// source's name bounds no clause in rung 1, so it must bound none in Citations:
// written as a citation's file, it would otherwise pass rung 1 as conforming and
// never reach rung 2.
func TestCitationsBoundAClauseWhereShapeDoes(t *testing.T) {
	const src = "checked against go-jose v4.1.4 — example.com/acme/go-sdk/x.go Foo"
	if shape := (Scanner{Requires: testRequires}).Line(src); len(shape) != 0 {
		t.Fatalf("rung 1 = %v, want the citation to conform", shape)
	}
	if cites := CitationsIn(src, testRequires); len(cites) != 1 {
		t.Errorf("CitationsIn = %+v, want the one citation rung 1 passed", cites)
	}
}

// TestARegistryCitationsUnitIsItsLine. The registry writes one entry per line,
// so two citations on a line share a unit and a citation on the next does not.
func TestARegistryCitationsUnitIsItsLine(t *testing.T) {
	cs := Citations("prose\n"+
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go A and absent at anthropic-sdk-go v1.70.1 — betaagent.go A\n"+
		"absent at anthropic-sdk-go v1.70.1 — betaagent.go B", nil)
	var got []int
	for _, c := range cs {
		got = append(got, c.Unit)
	}
	if fmt.Sprint(got) != "[2 2 3]" {
		t.Errorf("units = %v, want [2 2 3]", got)
	}
}

// TestANegatedCitationReachesNoRung. Rung 1 reports a negated form, and the
// rungs below must not also judge what follows it as the positive claim it is
// not: resolved, it would pass exactly when the negation says it should fail.
func TestANegatedCitationReachesNoRung(t *testing.T) {
	const src = "*Evidence: not checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams.*"
	if cites := Citations(src, nil); len(cites) != 0 {
		t.Errorf("Citations = %+v, want none: a negated form claims nothing", cites)
	}
	if shape := ShapeAll(src); len(shape) != 1 || shape[0].Rule != "negated-form" {
		t.Errorf("ShapeAll = %v, want the negated form reported", shape)
	}
}

// TestASpanReasonIsFalsifiedOverEveryDeclarationItCovers. `no-unique-name`
// claims there was no name to anchor on. The grammar joins several symbols with
// `and`, so a range covering more than one declaration is still nameable —
// checking only the single-declaration case let every wider span through, and a
// span that reports nothing is a span nobody will ever migrate.
func TestASpanReasonIsFalsifiedOverEveryDeclarationItCovers(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string // substrings the one finding must carry, or nil for none
	}{
		{
			// Fixture lines 17-22 hold `Base` and `Outer`, each declared once.
			name: "two declarations, both nameable",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:17-22 (span: no-unique-name)",
			want: []string{"span-reason-false", `"Base"`, `"Outer"`},
		},
		{
			// Fixture lines 14-15 hold two `init` functions.
			name: "two declarations sharing one name is the reason being true",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:14-15 (span: no-unique-name)",
		},
		{
			// The range still touches `Documented`, so counting declarations
			// alone would pass it.
			name: "a range that runs off the end of the file",
			in:   "checked against anthropic-sdk-go v1.70.1 — betaagent.go:39-99 (span: crosses-declarations)",
			want: []string{"span-past-eof", "the file has 41"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := env(t, "v1.70.1").Resolution(citations(t, tc.in))
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("Resolution = %v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("Resolution = %v, want one finding", got)
			}
			line := got[0].Rule + " " + got[0].Msg
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("finding %q does not carry %q", line, w)
				}
			}
		})
	}
}

// TestACitedSourceThisRunCannotOpenIsNamed. Rung 2 can only judge a tag it can
// read, so it skips the citations of an unresolvable source — and a caller that
// looked at findings alone would read that skip as a pass over the whole corpus.
func TestACitedSourceThisRunCannotOpenIsNamed(t *testing.T) {
	e := env(t, "v1.70.1")
	e.unreachable["go-jose"] = "go list -m: not a known dependency"
	cs := citations(t,
		"checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams",
		"checked against go-jose v4.0.5 — jwk.go JSONWebKey",
	)

	if got := e.Resolution(cs); len(got) != 0 {
		t.Fatalf("Resolution = %v, want none: the go-jose citation is skipped, which is "+
			"exactly why the skip has to be reported elsewhere", got)
	}
	why := e.Unresolvable(cs)
	if len(why) != 1 || !strings.Contains(why[0], "go-jose") {
		t.Fatalf("Unresolvable = %v, want go-jose named", why)
	}
	rep := e.Bump(cs)
	var named bool
	for _, u := range rep.Uncheckable {
		if strings.Contains(u.Why, "go-jose") && len(u.Anchors) == 1 {
			named = true
		}
	}
	if !named {
		t.Errorf("Uncheckable = %v, want the go-jose citation listed with its anchor",
			rep.Uncheckable)
	}
}

// TestAnUnreadableSpecIsNamed. The SDK can resolve while the spec it bundles
// cannot be read — an SDK that stopped shipping it — and then every schema
// anchor goes unchecked and reaches no transition, which an exit code that
// counted transitions alone would call clean.
func TestAnUnreadableSpecIsNamed(t *testing.T) {
	schema := citations(t, "checked against anthropic-sdk-go v1.66.0 — spec components.schemas.BetaSession")
	symbol := citations(t, "checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams")
	e := env(t, "v1.70.1")
	e.spec = NewSpec(t.TempDir())
	if got := e.Unresolvable(schema); len(got) != 1 || !strings.Contains(got[0], "bundled spec") {
		t.Errorf("Unresolvable = %v, want the unreadable spec named", got)
	}
	if got := e.Unresolvable(symbol); len(got) != 0 {
		t.Errorf("Unresolvable = %v, want none: nothing cited reads the spec", got)
	}
	e.spec = nil
	if got := e.Unresolvable(schema); len(got) != 1 || !strings.Contains(got[0], "bundled spec") {
		t.Errorf("Unresolvable = %v, want the unopened spec named", got)
	}
}

// TestAReplacedSourceIsNamed. Rung 2 judges a replaced module against the tree
// the replacement names while quoting the tag, so a -fail that passed on its
// findings would certify a fork.
func TestAReplacedSourceIsNamed(t *testing.T) {
	e := env(t, "v1.70.1")
	e.replaced = map[string]string{"anthropic-sdk-go": "anthropic-sdk-go v1.70.1 is replaced by ../fork"}
	cs := citations(t, "checked against anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams")
	if got := e.Unresolvable(cs); len(got) != 1 || !strings.Contains(got[0], "replaced by ../fork") {
		t.Errorf("Unresolvable = %v, want the replacement named", got)
	}
	if got := e.Unresolvable(nil); len(got) != 0 {
		t.Errorf("Unresolvable over no citation = %v, want none: only a cited source changes "+
			"what the verdict means", got)
	}
}

// TestTheReportNamesOurOwnCoordinates. Plan 51 checks this repository's own
// coordinates at no rung — git holds their history — and has the report name
// them under what went unchecked, so that "not checked" is a list rather than a
// silence.
func TestTheReportNamesOurOwnCoordinates(t *testing.T) {
	rep := env(t, "v1.70.1").Bump(nil)
	rep.NameOurs([]string{"internal/events/probe.go:3 toolflow.go:278"})
	if len(rep.Uncheckable) != 1 || len(rep.Uncheckable[0].Anchors) != 1 {
		t.Fatalf("Uncheckable = %v, want our one coordinate listed", rep.Uncheckable)
	}
	if out := rep.String(); !strings.Contains(out, "toolflow.go:278") {
		t.Errorf("the rendered report does not name it:\n%s", out)
	}
	before := len(rep.Uncheckable)
	rep.NameOurs(nil)
	if len(rep.Uncheckable) != before {
		t.Errorf("naming no coordinates added an empty entry: %v", rep.Uncheckable)
	}
}

// TestTheReportNamesEachSourcesOwnPin. The report covers every module the
// grammar governs, and go-jose's pin is not the SDK's: a transition message that
// quoted the SDK's version for a go-jose anchor would send a reader to the wrong
// tag.
func TestTheReportNamesEachSourcesOwnPin(t *testing.T) {
	e := env(t, "v1.70.1")
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	e.resolvers["go-jose"], e.versions["go-jose"] = r, "v4.1.4"
	rep := e.Bump(citations(t,
		"checked against go-jose v4.0.0 — betaagent.go NoSuchKey",
		"checked against go-jose v4.0.0 — nosuch.go Key",
		"absent at go-jose v4.0.0 — betaagent.go BetaAgentNewParams",
	))
	if len(rep.Vanished) != 2 || len(rep.Returned) != 1 {
		t.Fatalf("Vanished = %v, Returned = %v, want two and one", rep.Vanished, rep.Returned)
	}
	for _, f := range append(rep.Vanished, rep.Returned...) {
		if !strings.Contains(f.Msg, "pin v4.1.4") || strings.Contains(f.Msg, "v1.70.1") {
			t.Errorf("a go-jose transition does not name go-jose's pin: %s", f.Msg)
		}
	}
	head, _, _ := strings.Cut(rep.String(), "\n")
	if !strings.Contains(head, "anthropic-sdk-go v1.70.1") || !strings.Contains(head, "go-jose v4.1.4") {
		t.Errorf("the report's header does not name every pin it resolved against: %s", head)
	}
}

// TestASpanTheReportCannotReadIsListedByWhy. A span over a file its own tag
// does not ship is contradicted, not unchecked; and the spans that genuinely
// could not be read are grouped by what went wrong, since a reason quoting each
// citation made every one of them a group of its own.
func TestASpanTheReportCannotReadIsListedByWhy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package p\n\nfunc {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := envWithCache(t, "v1.70.1", map[string]string{"v1.66.0": olderFixture(t)})
	e.resolvers["anthropic-sdk-go"] = r
	rep := e.Bump(citations(t,
		"checked against anthropic-sdk-go v1.66.0 — nosuch.go:1-2 (span: crosses-declarations)",
		"checked against anthropic-sdk-go v1.70.1 — broken.go:1-2 (span: crosses-declarations)",
		"checked against anthropic-sdk-go v1.70.1 — broken.go:2-3 (span: crosses-declarations)",
	))
	if len(rep.Contradicted) != 1 || rep.Contradicted[0].Rule != "span-not-shipped" {
		t.Errorf("Contradicted = %v, want the span whose file its tag does not ship", rep.Contradicted)
	}
	if len(rep.Uncheckable) != 1 || len(rep.Uncheckable[0].Anchors) != 2 {
		t.Errorf("Uncheckable = %v, want the two unparseable spans under one reason", rep.Uncheckable)
	}
}

// TestASpanOverGoThatWillNotParseIsUncheckable. The file exists and the range
// fits, so everything before the parser passes — and a rung that went quiet when
// the parser then failed would hand back a clean bill for a span nobody read.
func TestASpanOverGoThatWillNotParseIsUncheckable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package p\n\nfunc {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := env(t, "v1.70.1")
	e.resolvers["anthropic-sdk-go"] = r
	got := e.Resolution(citations(t,
		"checked against anthropic-sdk-go v1.70.1 — broken.go:1-2 (span: crosses-declarations)"))
	if len(got) != 1 || got[0].Rule != "span-uncheckable" {
		t.Errorf("Resolution = %v, want the span named as unreadable", got)
	}
}

// document reads lines the way the registry is read, one unit per line, so a
// test can put two anchors beside each other or apart.
func document(t *testing.T, lines ...string) []Citation {
	t.Helper()
	cs := Citations(strings.Join(lines, "\n"), nil)
	for i := range cs {
		cs[i].File = "registry.md"
	}
	return cs
}

// TestATransitionIsDispositionedBesideIt is plan 51's disposition. A bump
// fails on a transition nobody has read, and reading one is written down beside
// it: an anchor gone at the pin takes an `absent at` on what it lost, at a tag
// after its own stamp and no later than the pin. Each case below is one way
// something that is not that edit could pass for it.
func TestATransitionIsDispositionedBesideIt(t *testing.T) {
	const pin = "v1.70.1"
	const gone = "checked against anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion"
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool // dispositioned
	}{
		{
			name:  "absent at the pin, beside it",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"},
			want:  true,
		},
		{
			// When it went is a finer claim than when the bump was taken, and
			// just as true at every later pin.
			name:  "absent at a tag between the stamp and the pin",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.68.0 — betaagent.go resolveSkillVersion"},
			want:  true,
		},
		{
			name:  "nothing beside it",
			lines: []string{gone},
		},
		{
			name:  "absent at the stamp's own tag contradicts the anchor rather than dispositioning it",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion"},
		},
		{
			name:  "absent at a tag before the stamp",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.60.0 — betaagent.go resolveSkillVersion"},
		},
		{
			// It describes a bump that has not happened.
			name:  "absent at a tag ahead of the pin",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.71.0 — betaagent.go resolveSkillVersion"},
		},
		{
			name: "absent at the pin, in another unit",
			lines: []string{gone,
				"absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"},
		},
		{
			name:  "absent at the pin, in another file",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.70.1 — betafile.go resolveSkillVersion"},
		},
		{
			name:  "absent at the pin, naming another symbol",
			lines: []string{gone + " and absent at anthropic-sdk-go v1.70.1 — betaagent.go noSuchHelper"},
		},
		{
			name:  "absent from another source",
			lines: []string{gone + " and absent at go-jose v1.68.0 — betaagent.go resolveSkillVersion"},
		},
		{
			// A positive anchor claims every symbol it names, so only the one
			// that went has anything to acknowledge.
			name: "only what went needs acknowledging",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams " +
				"and resolveSkillVersion and absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"},
			want: true,
		},
		{
			name: "everything that went needs acknowledging, not only the last",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — betaagent.go noSuchHelper " +
				"and resolveSkillVersion and absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"},
		},
		{
			name: "everything that went needs acknowledging, not only the first",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — betaagent.go noSuchHelper " +
				"and resolveSkillVersion and absent at anthropic-sdk-go v1.70.1 — betaagent.go noSuchHelper"},
		},
		{
			name: "one absent-at over both",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — betaagent.go noSuchHelper " +
				"and resolveSkillVersion and absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion " +
				"and noSuchHelper"},
			want: true,
		},
		{
			name: "a file the pin does not ship",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams " +
				"and absent at anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams"},
			want: true,
		},
		{
			// Stamped at the pin, it is simply wrong, and rung 2 says so.
			name: "an anchor stamped at the pin has no later tag to be acknowledged at",
			lines: []string{"checked against anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion " +
				"and absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := env(t, pin).Bump(document(t, tc.lines...))
			if len(rep.Returned) != 0 {
				t.Fatalf("Returned = %v, want none: this case is about a vanished anchor", rep.Returned)
			}
			switch {
			case tc.want && (len(rep.Dispositioned) != 1 || len(rep.Vanished) != 0):
				t.Errorf("Vanished = %v, Dispositioned = %v, want the transition dispositioned",
					rep.Vanished, rep.Dispositioned)
			case !tc.want && (len(rep.Dispositioned) != 0 || len(rep.Vanished) != 1):
				t.Errorf("Vanished = %v, Dispositioned = %v, want the transition awaiting a disposition",
					rep.Vanished, rep.Dispositioned)
			}
		})
	}
}

// TestAPreReleasePinIsDispositionedAtTheReleaseItPrecedes. The grammar names
// release tags only, so under a pseudo-version pin no tag after the stamp was
// no later than the pin, and a transition there could never be dispositioned.
func TestAPreReleasePinIsDispositionedAtTheReleaseItPrecedes(t *testing.T) {
	const pin = "v1.70.2-0.20260901000000-abcdef012345"
	const gone = "checked against anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion"
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"v1.70.2", true},
		{"v1.70.3", false},
	} {
		rep := env(t, pin).Bump(document(t,
			gone+" and absent at anthropic-sdk-go "+tc.tag+" — betaagent.go resolveSkillVersion"))
		if got := len(rep.Dispositioned) == 1 && len(rep.Vanished) == 0; got != tc.want {
			t.Errorf("absent at %s: Vanished = %v, Dispositioned = %v, want dispositioned = %v",
				tc.tag, rep.Vanished, rep.Dispositioned, tc.want)
		}
	}
	rep := env(t, pin).Bump(document(t, gone))
	if len(rep.Vanished) != 1 || !strings.Contains(rep.Vanished[0].Msg, "`absent at anthropic-sdk-go v1.70.2 — ") {
		t.Errorf("Vanished = %v, want the line naming the release the pin precedes", rep.Vanished)
	}
}

// TestTheLineAwaitingADispositionNamesOnlyWhatIsOpen. A line naming what an
// `absent at` beside it already names would stamp that symbol's absence twice.
// And an anchor stamped at the pin has no bump to acknowledge — the line the
// tool would refuse is not the line to suggest.
func TestTheLineAwaitingADispositionNamesOnlyWhatIsOpen(t *testing.T) {
	rep := env(t, "v1.70.1").Bump(document(t,
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion and noSuchHelper "+
			"and absent at anthropic-sdk-go v1.68.0 — betaagent.go resolveSkillVersion",
		"checked against anthropic-sdk-go v1.70.1 — betaagent.go noSuchHelper"))
	if len(rep.Vanished) != 2 {
		t.Fatalf("Vanished = %v, want both anchors", rep.Vanished)
	}
	if msg := rep.Vanished[0].Msg; !strings.Contains(msg, "— betaagent.go noSuchHelper`") {
		t.Errorf("Vanished[0] = %s, want the line naming noSuchHelper alone", rep.Vanished[0])
	}
	if msg := rep.Vanished[1].Msg; strings.Contains(msg, "`absent at") ||
		!strings.Contains(msg, "the claim is what is wrong") {
		t.Errorf("Vanished[1] = %s, want no disposition offered for an anchor stamped at the pin",
			rep.Vanished[1])
	}
}

// TestADeletedFilesDispositionIsNotListedUnchecked. Rung 2 judges an `absent
// at` stamped at the pin on a file the pin does not ship, so the report has
// nothing to add — listing it as uncheckable would grow that list by one line
// for every file a bump deletes, burying what really went unread.
func TestADeletedFilesDispositionIsNotListedUnchecked(t *testing.T) {
	rep := env(t, "v1.70.1").Bump(document(t,
		"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams "+
			"and absent at anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams"))
	if len(rep.Dispositioned) != 1 || len(rep.Uncheckable) != 0 {
		t.Errorf("Dispositioned = %v, Uncheckable = %v, want the transition dispositioned and "+
			"nothing unchecked", rep.Dispositioned, rep.Uncheckable)
	}
}

// TestADispositionIsInTheAnchorsOwnDocument. Units are numbered by line, so the
// registry's first line and a comment paragraph opening on a Go file's first
// line share a number and nothing else.
func TestADispositionIsInTheAnchorsOwnDocument(t *testing.T) {
	cs := document(t, "checked against anthropic-sdk-go v1.66.0 — betaagent.go resolveSkillVersion")
	elsewhere := document(t, "absent at anthropic-sdk-go v1.70.1 — betaagent.go resolveSkillVersion")
	elsewhere[0].File = "internal/probe/probe.go"
	rep := env(t, "v1.70.1").Bump(append(cs, elsewhere...))
	if len(rep.Vanished) != 1 || len(rep.Dispositioned) != 0 {
		t.Errorf("Vanished = %v, Dispositioned = %v, want the transition awaiting a disposition: "+
			"the `absent at` is in another file", rep.Vanished, rep.Dispositioned)
	}
}

// TestASchemaTransitionIsDispositionedBesideIt. A schema path vanishes like a
// symbol does, and is acknowledged by naming the same path.
func TestASchemaTransitionIsDispositionedBesideIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, SpecPath))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(`{"components":{"schemas":{"BetaSession":{}}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	const gone = "checked against anthropic-sdk-go v1.66.0 — spec components.schemas.BetaGone"
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"the same path": {gone + " and absent at anthropic-sdk-go v1.70.1 — spec components.schemas.BetaGone", true},
		"another path":  {gone + " and absent at anthropic-sdk-go v1.70.1 — spec components.schemas.BetaOther", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := env(t, "v1.70.1")
			e.spec = NewSpec(dir)
			rep := e.Bump(document(t, tc.line))
			if got := len(rep.Dispositioned) == 1 && len(rep.Vanished) == 0; got != tc.want {
				t.Errorf("Vanished = %v, Dispositioned = %v, want dispositioned = %v",
					rep.Vanished, rep.Dispositioned, tc.want)
			}
		})
	}
}

// TestAReturnedAnchorIsNeverDispositioned. Plan 51's disposition for an
// `absent at` anchor that resolves again is to drop the clause, so nothing
// written beside it can stand in for that — not even an anchor saying it is
// back.
func TestAReturnedAnchorIsNeverDispositioned(t *testing.T) {
	rep := env(t, "v1.70.1").Bump(document(t,
		"absent at anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams and checked against "+
			"anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams"))
	if len(rep.Returned) != 1 || len(rep.Dispositioned) != 0 {
		t.Fatalf("Returned = %v, Dispositioned = %v, want the returned anchor awaiting its "+
			"disposition", rep.Returned, rep.Dispositioned)
	}
	if !strings.Contains(rep.Returned[0].Msg, "drop the `absent at` clause") {
		t.Errorf("the transition does not say what disposes of it: %s", rep.Returned[0])
	}
	if n := rep.Undispositioned(); n != 1 {
		t.Errorf("Undispositioned() = %d, want 1", n)
	}
}

// TestAnUndispositionedTransitionNamesItsDisposition. The report is read on
// the pull request that moves the pin, by someone who has to write the line; a
// transition that only said "gone" would send them to the plan to learn its
// grammar. The clause names what went, and only that.
func TestAnUndispositionedTransitionNamesItsDisposition(t *testing.T) {
	rep := env(t, "v1.70.1").Bump(document(t,
		"checked against anthropic-sdk-go v1.66.0 — betaagent.go BetaAgentNewParams and noSuchHelper "+
			"and resolveSkillVersion",
		"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams"))
	if len(rep.Vanished) != 2 {
		t.Fatalf("Vanished = %v, want both anchors", rep.Vanished)
	}
	for i, want := range []string{
		"`absent at anthropic-sdk-go v1.70.1 — betaagent.go noSuchHelper and resolveSkillVersion`",
		"`absent at anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams`",
	} {
		if !strings.Contains(rep.Vanished[i].Msg, want) {
			t.Errorf("Vanished[%d] = %s, want it to name %s", i, rep.Vanished[i], want)
		}
	}
	if n := rep.Undispositioned(); n != 2 {
		t.Errorf("Undispositioned() = %d, want 2", n)
	}
}

// TestADeletedFilesDispositionPassesRungTwo. Rung 2 refuses an `absent at` on a
// file the pin does not ship, because nothing in a missing file resolves and so
// nothing could contradict it. That made a deleted file the one transition no
// line could disposition: the clause the bump asks for is the clause the gate
// refused. The positive anchor beside it, stamped earlier on the same file and
// symbol, is what says the file was there, so the pair passes — and the
// `absent at` passes with nothing else.
func TestADeletedFilesDispositionPassesRungTwo(t *testing.T) {
	const absent = "absent at anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams"
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool // passes
	}{
		{
			name:  "beside the anchor it acknowledges",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams and " + absent},
			want:  true,
		},
		{
			name:  "alone",
			lines: []string{absent},
		},
		{
			name:  "in another unit",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams", absent},
		},
		{
			name:  "beside an anchor on another symbol",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentList and " + absent},
		},
		{
			name:  "beside an anchor in another file",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — other.go BetaAgentNewParams and " + absent},
		},
		{
			// The positive anchor at the same tag is itself a claim rung 2
			// refuses, so it cannot vouch for the other.
			name:  "beside an anchor stamped at the same tag",
			lines: []string{"checked against anthropic-sdk-go v1.70.1 — nosuch.go BetaAgentNewParams and " + absent},
		},
		{
			name:  "beside another absent-at",
			lines: []string{"absent at anthropic-sdk-go v1.66.0 — nosuch.go BetaAgentNewParams and " + absent},
		},
		{
			name: "every symbol it names beside an anchor",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go A and B and " +
				"absent at anthropic-sdk-go v1.70.1 — nosuch.go A and B"},
			want: true,
		},
		{
			name: "one symbol it names beside no anchor",
			lines: []string{"checked against anthropic-sdk-go v1.66.0 — nosuch.go A and " +
				"absent at anthropic-sdk-go v1.70.1 — nosuch.go A and B"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refused []Finding
			for _, f := range env(t, "v1.70.1").Resolution(document(t, tc.lines...)) {
				if f.Rule == "unresolvable" {
					refused = append(refused, f)
				}
			}
			if tc.want && len(refused) != 0 {
				t.Errorf("Resolution = %v, want the disposition of a deleted file accepted", refused)
			}
			if !tc.want && len(refused) != 1 {
				t.Errorf("Resolution = %v, want the `absent at` on a missing file refused", refused)
			}
		})
	}
}
