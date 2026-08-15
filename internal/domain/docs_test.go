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
// line — `id.go`, `idAlphabet` and `Valid()` all sit in the ARCHITECTURE.md row
// this test reads, and none of them is a prefix.
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
// than keeping an anchor nothing satisfies.
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

	for _, doc := range []struct{ path, anchor string }{
		{"CLAUDE.md", "**ID prefixes**:"},
		{filepath.Join("docs", "ARCHITECTURE.md"), "`ID` with wire-compatible prefixes"},
	} {
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

// repoRoot derives the checkout root from this file's compile-time path, so the
// check reads the documents of the tree it was compiled from — a worktree's own
// CLAUDE.md, not the main checkout's — and needs no working directory, network
// or fixture to do it.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}
