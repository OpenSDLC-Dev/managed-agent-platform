package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"strings"
)

// The other half of the corpus. Go comments beside the code cite the SDK the
// same way the registry does and rot the same way — a comment that names a line
// in `betasession.go` is wrong the moment the SDK inserts one above it.
//
// Two things make the comments harder to read than the registry. A wrapped
// comment splits a citation across two physical lines, so a line-at-a-time
// scanner would see a head with no locator on one and a locator with no head on
// the next: two findings for one defect, or none. So a comment is scanned by
// paragraph, ended by a blank comment line — the same rule gofmt and go/doc
// already read them by — and each finding is then put back on the physical line
// its text was actually on, because a paragraph here runs to a dozen lines and
// a coordinate reported at the top of one sends a reader to the wrong sentence.
//
// And a comment rarely spells the source out. It writes "the SDK", or nothing
// at all, so the registry's rule — look for a coordinate only where the text
// names a governed source — would leave most of this half unread. Here the
// discriminator is the tree instead: a coordinate into a file this repository
// does not ship can be citing nothing but somewhere else.

// GoComments runs rung 1 over the comments of every Go file in files — paths
// relative to root, as Tracked lists them — with the two predicates Scanner
// takes, returning what it found, the conforming citations for the resolving
// rungs, and the coordinates into this repository it read.
//
// The files are what git tracks rather than what a walk of the disk finds, for
// the reason Tracked gives: an untracked file would otherwise decide the
// verdict on one machine. And a file that will not parse is a finding, not a
// skip. Every comment in it went unread, and a rung that said nothing about that
// would report a clean corpus it had not looked at.
func GoComments(root string, files []string, inRepo, requires func(string) bool) ([]Finding, []Citation, []string) {
	var findings []Finding
	var citations []Citation
	var ours []string
	for _, rel := range files {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil,
			parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			findings = append(findings, Finding{File: rel, Rule: "comments-unread", Msg: fmt.Sprintf(
				"this file could not be parsed, so no citation in its comments was read: %v", err)})
			continue
		}
		// A comment names a file beside it by its basename, the way a Go
		// programmer reads one package: a coordinate into a file of the same
		// directory means that file. Resolving only from the root called every
		// such coordinate someone else's.
		dir := path.Dir(rel)
		scanner := Scanner{AnyExternalCoordinate: true, Requires: requires, InRepo: func(p string) bool {
			return inRepo != nil && (inRepo(p) || inRepo(path.Join(dir, p)))
		}}
		for _, cg := range f.Comments {
			for _, par := range paragraphs(fset, cg) {
				found, own := scanner.scan(par.text)
				for _, fd := range found {
					fd.File, fd.Line = rel, par.lineAt(fd.at)
					findings = append(findings, fd)
				}
				for _, o := range own {
					ours = append(ours, fmt.Sprintf("%s:%d %s", rel, par.lineAt(o.at), o.Msg))
				}
				for _, c := range CitationsIn(par.text) {
					c.File, c.Line = rel, par.lineAt(c.at)
					citations = append(citations, c)
				}
			}
		}
	}
	return findings, citations, ours
}

// para is one comment paragraph: its text with the comment markers and the
// wrapping removed, and enough bookkeeping to say which physical line any
// offset in that text came from.
type para struct {
	text   string
	starts []int // byte offset in text where each physical line's text begins
	lines  []int // that line's number in the file
}

// lineAt maps an offset in the joined text back to the line it was written on.
func (p para) lineAt(off int) int {
	line := 0
	for i, start := range p.starts {
		if start > off {
			break
		}
		line = p.lines[i]
	}
	return line
}

func paragraphs(fset *token.FileSet, cg *ast.CommentGroup) []para {
	var out []para
	var cur para
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			cur.text = b.String()
			out = append(out, cur)
		}
		cur, b = para{}, strings.Builder{}
	}
	add := func(at int, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			flush()
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		cur.starts = append(cur.starts, b.Len())
		cur.lines = append(cur.lines, at)
		b.WriteString(text)
	}
	for _, c := range cg.List {
		at := fset.Position(c.Pos()).Line
		if strings.HasPrefix(c.Text, "//") {
			add(at, strings.TrimPrefix(c.Text, "//"))
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(c.Text, "/*"), "*/")
		for i, l := range strings.Split(body, "\n") {
			add(at+i, strings.TrimPrefix(strings.TrimSpace(l), "*"))
		}
	}
	flush()
	return out
}
