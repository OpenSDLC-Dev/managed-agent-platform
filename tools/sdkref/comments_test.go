package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commentTree writes a throwaway package whose comments carry the shapes this
// half of the corpus is actually written in, and returns its root.
func commentTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	const body = `package probe

// Wrapped is the case a line-at-a-time scanner cannot read: the head is on one
// physical line and the locator on the next, because a comment wraps at eighty
// columns like every other one in this repository. checked against
// anthropic-sdk-go v1.70.1 — betaagent.go BetaAgentNewParams
//
// This second paragraph is separated by a blank comment line and carries
// *a* *b* *c* *d* *e* *f* *g* *h* ahead of the
// coordinate: betasession.go:912.
func Wrapped() {}

/*
Block is the other comment syntax. It cites betaenvironment.go:368 with no
source named, which is how most of these are written.
*/
func Block() {}

// Ours points at a file this repository ships, so it is nobody's citation into
// an external source: internal/api/server.go:699.
//
// And a host and a port is not a coordinate at all: x.com:443.
func Ours() {}
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// gitTree makes dir a repository tracking everything already written into it,
// which is what Tracked reads: a tree's own files are ours only once git
// tracks them.
func gitTree(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func ourFiles(paths ...string) func(string) bool { return InSet(paths) }

// TestGoCommentsReadsAWrappedCitation is the reason comments are scanned by
// paragraph rather than by physical line. Scanned line by line, the citation
// below is a head with no locator followed by a locator with no head: two
// findings for one defect, or none. This asserts it is neither.
func TestGoCommentsReadsAWrappedCitation(t *testing.T) {
	root := commentTree(t)
	findings, citations, ours := GoComments(root, []string{"probe.go"}, ourFiles("internal/api/server.go"), nil)
	if len(citations) != 1 {
		t.Fatalf("citations = %+v, want the one wrapped citation read whole", citations)
	}
	if got := citations[0].Loc.Desc(); got != "betaagent.go BetaAgentNewParams" {
		t.Errorf("the wrapped citation resolved to %q", got)
	}

	var got []string
	for _, f := range findings {
		got = append(got, f.Rule)
	}
	if len(got) != 2 {
		t.Fatalf("findings = %v (%d), want exactly the two sourceless coordinates", got, len(got))
	}
	for _, f := range findings {
		if f.Rule != "bare-line" {
			t.Errorf("finding %s is not the bare coordinate this tree carries", f)
		}
		if strings.Contains(f.Msg, "server.go") || strings.Contains(f.Msg, "x.com") {
			t.Errorf("%s: neither our own file nor a host and port is a citation into a "+
				"source this grammar governs", f)
		}
	}
	// And our own coordinate is not dropped either: the report names it.
	if len(ours) != 1 || !strings.Contains(ours[0], "probe.go:") ||
		!strings.Contains(ours[0], "internal/api/server.go:699") {
		t.Errorf("ours = %v, want the one coordinate into this repository, named by where "+
			"it is written", ours)
	}
}

// TestACommentFindingNamesTheLineItsTextIsOn. A paragraph here runs to a dozen
// lines; reporting every finding at the paragraph's first line sends whoever
// migrates it to the wrong sentence, and the offset is silent because the
// message quotes text that really is in the file.
func TestACommentFindingNamesTheLineItsTextIsOn(t *testing.T) {
	root := commentTree(t)
	findings, citations, _ := GoComments(root, []string{"probe.go"}, ourFiles(), nil)
	// Emphasis is blanked rather than deleted precisely so these hold: an
	// offset into the joined paragraph still indexes the file's own bytes.
	want := map[string]int{
		"betasession.go:912":     10, // the second paragraph's third line
		"betaenvironment.go:368": 14, // the block comment's first line of text
	}
	for _, f := range findings {
		for text, line := range want {
			if strings.Contains(f.Msg, text) && f.Line != line {
				t.Errorf("%q reported at line %d, want %d — the line its text is on, not "+
					"the line its paragraph began on", text, f.Line, line)
			}
		}
	}
	// The citation's head begins on the paragraph's third line, not its first.
	if len(citations) == 1 && citations[0].Line != 5 {
		t.Errorf("the wrapped citation is reported at line %d, want 5 — where its head is "+
			"written", citations[0].Line)
	}
}

// TestACommentCitationsUnitIsItsParagraph. A disposition is written beside the
// anchor it acknowledges, and in a comment "beside" is the paragraph: two
// citations wrapped across its lines share one unit, the next paragraph of the
// same comment is another, and so is a second comment on the same line — which
// a unit named by its line number could not tell apart.
func TestACommentCitationsUnitIsItsParagraph(t *testing.T) {
	dir := t.TempDir()
	const body = `package probe

// A paragraph wrapping two citations: checked against anthropic-sdk-go v1.66.0
// — betaagent.go resolveSkillVersion, and absent at anthropic-sdk-go v1.70.1 —
// betaagent.go resolveSkillVersion.
//
// The next paragraph: absent at anthropic-sdk-go v1.70.1 — betaagent.go noSuchHelper.
var X = f(/* checked against anthropic-sdk-go v1.66.0 — betaagent.go A */ 1, /* absent at anthropic-sdk-go v1.70.1 — betaagent.go A */ 2)

/*
A block comment's first paragraph: absent at anthropic-sdk-go v1.70.1 — betaagent.go B.

And its second: absent at anthropic-sdk-go v1.70.1 — betaagent.go C.
*/
func f(...int) int { return 0 }
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cs, _ := GoComments(dir, []string{"probe.go"}, ourFiles(), nil)
	if len(cs) != 7 {
		t.Fatalf("citations = %+v, want seven", cs)
	}
	same := [][2]int{{0, 1}}
	different := [][2]int{{0, 2}, {1, 2}, {3, 4}, {2, 3}, {5, 6}, {4, 5}}
	for _, p := range same {
		if cs[p[0]].Unit != cs[p[1]].Unit {
			t.Errorf("%q and %q are in one paragraph, and in units %d and %d",
				cs[p[0]].Raw, cs[p[1]].Raw, cs[p[0]].Unit, cs[p[1]].Unit)
		}
	}
	for _, p := range different {
		if cs[p[0]].Unit == cs[p[1]].Unit {
			t.Errorf("%q and %q are in different paragraphs, and share unit %d",
				cs[p[0]].Raw, cs[p[1]].Raw, cs[p[0]].Unit)
		}
	}
}

// TestTheCorpusCarriesCommentCitationsToTheResolvingRungs. Rung 1 reading the
// comments is only half of it: if the citations it finds there never reach
// rungs 2 and 3, then after slice 3 migrates the comments every one of them is
// silently exempt from the gate, and nothing in the output says so.
func TestTheCorpusCarriesCommentCitationsToTheResolvingRungs(t *testing.T) {
	root := commentTree(t)
	doc := filepath.Join(root, "registry.md")
	// Both halves also carry another project's version, which only go.mod can
	// attribute: a half the corpus scanned without it would report the line.
	const elsewhere = "k8s.io/api v0.36.2 has no such field\n"
	for name, body := range map[string]string{
		"registry.md": elsewhere,
		"extra.go":    "// " + elsewhere + "package probe\n",
		"go.mod":      "module probe\n\ngo " + goDirective(t, repoRoot(t)) + "\n\nrequire k8s.io/api v0.36.2\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitTree(t, root)
	findings, citations, _, err := corpus(root, doc, "registry.md", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if f.File == "registry.md" || f.File == "extra.go" {
			t.Errorf("a version go.mod attributes to another module was reported: %s", f)
		}
	}
	if len(citations) != 1 || citations[0].File != "probe.go" {
		t.Fatalf("corpus citations = %+v, want the comment's one citation: the resolving "+
			"rungs judge what this returns and nothing else", citations)
	}
	_, without, _, err := corpus(root, doc, "registry.md", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(without) != 0 {
		t.Errorf("with comments off the corpus is %+v, want only the document's", without)
	}
}

// TestAContinuationInAParagraphThatNamesNoSource. Most comment paragraphs never
// spell a source name — that asymmetry is why the comment half sets
// AnyExternalCoordinate at all. A continuation there inherits its filename from
// the coordinate beside it, and a scanner that waited for a source token read
// the coordinate and passed over the continuation hanging off it.
func TestAContinuationInAParagraphThatNamesNoSource(t *testing.T) {
	s := Scanner{AnyExternalCoordinate: true}
	const line = "// The reference bounds it there (betasession.go:912, :915)."
	var quoted []string
	for _, f := range s.Line(line) {
		quoted = append(quoted, f.Msg)
	}
	if len(quoted) != 2 {
		t.Fatalf("Line reported %d findings, want the coordinate and the continuation: %v",
			len(quoted), quoted)
	}
	if !strings.Contains(quoted[1], `":915"`) {
		t.Errorf("the continuation was not reported: %s", quoted[1])
	}
	// And the registry half is unchanged: there the same shape is prose about
	// something else far more often than it is a citation.
	if got := (Scanner{}).Line(line); len(got) != 0 {
		t.Errorf("the registry scanner reported %v on a line naming no source", got)
	}
}

// TestACommentCitesTheFileBesideItByItsBasename. A comment that names a file
// of its own directory by basename means the file next to it, the way anyone
// reading one package does; resolving only from the repository root called every
// such coordinate a citation into someone else's source. And an absolute path is
// a place on a filesystem — a sandbox's workspace — which no module ships.
func TestACommentCitesTheFileBesideItByItsBasename(t *testing.T) {
	root := t.TempDir()
	const body = `package events

// The settlement orders these the way toolflow.go:278 does, and the agent
// writes its output under /workspace/src/util/helpers.go:3 — while the SDK's
// own reading is betasession.go:912.
func Probe() {}
`
	dir := filepath.Join(root, "internal", "events")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, _, ours := GoComments(root, []string{"internal/events/probe_test.go"},
		ourFiles("internal/events/toolflow.go"), nil)
	if len(findings) != 1 || !strings.Contains(findings[0].Msg, "betasession.go:912") {
		t.Errorf("findings = %v, want only the SDK's coordinate", findings)
	}
	if len(ours) != 1 || !strings.Contains(ours[0], "toolflow.go:278") {
		t.Errorf("ours = %v, want the sibling coordinate named as this repository's", ours)
	}
}

// TestATrackedFileTheTreeDeletedHoldsNoComment. git still lists a file removed
// without `git rm`, and there is no comment in it to read: the deletion is the
// change, and failing the gate as though the file would not parse misnames it.
func TestATrackedFileTheTreeDeletedHoldsNoComment(t *testing.T) {
	root := commentTree(t)
	findings, _, _ := GoComments(root, []string{"deleted.go", "probe.go"}, ourFiles(), nil)
	for _, f := range findings {
		if f.File != "probe.go" {
			t.Errorf("unexpected finding %s", f)
		}
	}
	if len(findings) == 0 {
		t.Errorf("probe.go was not read beside the deleted file")
	}
}

// TestAGoFileThatWillNotParseIsNamed. Every comment in such a file went unread,
// and a scan that skipped it in silence would report a clean corpus it had not
// looked at. The files beside it are still read.
func TestAGoFileThatWillNotParseIsNamed(t *testing.T) {
	root := commentTree(t)
	if err := os.WriteFile(filepath.Join(root, "broken.go"), []byte("package p\n\nfunc {\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, _, _ := GoComments(root, []string{"broken.go", "probe.go", "README.md"}, ourFiles(), nil)
	var unread, read int
	for _, f := range findings {
		switch {
		case f.Rule == "comments-unread" && f.File == "broken.go":
			unread++
		case f.File == "probe.go":
			read++
		default:
			t.Errorf("unexpected finding %s", f)
		}
	}
	if unread != 1 || read == 0 {
		t.Errorf("findings = %v, want broken.go named once and probe.go still read", findings)
	}
}
