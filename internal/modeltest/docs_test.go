package modeltest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// liveTierName matches one live-tier consent variable, e.g. RUN_LIVE_KMS_TESTS.
// It stops before a trailing '=' so the name is found in an environment line a
// test builds ("RUN_LIVE_WEB_TESTS=1") as readily as in the constant it is
// declared by.
var liveTierName = regexp.MustCompile(`RUN_LIVE_[A-Z0-9_]*[A-Z0-9]`)

// TestDocsListEveryLiveTierVariable holds README.md's tier table to the set of
// RUN_LIVE_* variables the tree actually has, in both directions.
//
// This package states the opt-in contract every paid tier restates: consent to
// spend money is a variable in the environment, and once given, missing
// configuration fails rather than skips. That contract is only as good as the
// table a reader consults to find out which variable buys which tier — and the
// documented set has drifted from the code three ways
// (docs/plan/34_doc-trim.md, "Pin the lists that drift, don't just correct
// them"). Each tier lives in its own support package, so no single import graph
// reaches all of them and nothing linked the six to the one table.
//
// The names are read from the tree's Go source as string literals rather than
// by grepping the text, because a variable named in a comment is prose and a
// variable named in a literal is a value the compiler carries: os.Getenv is
// given the name, never told about it. A test that *sets* one names it as a
// literal too, which is over-broad by the letter and exactly right in effect —
// a tier no code sets or reads is a tier that does not exist.
//
// RUN_EVALS is deliberately out of scope. It is not spelled RUN_LIVE_, and the
// difference is the point: the eval suite costs an order of magnitude more than
// the single-turn smoke, so it is opted into separately and not swept up by a
// pattern that matches the cheap tiers.
func TestDocsListEveryLiveTierVariable(t *testing.T) {
	root := repoRoot()

	inCode := map[string][]string{} // variable -> the files naming it, repo-relative
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Dot-directories hold no Go source of this module, and skipping
			// them keeps a worktree checked out under .claude/worktrees from
			// being scanned as if it were part of this tree.
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			for _, name := range liveTierName.FindAllString(lit.Value, -1) {
				if !slices.Contains(inCode[name], rel) {
					inCode[name] = append(inCode[name], rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s for live-tier variables: %v", root, err)
	}
	if len(inCode) == 0 {
		t.Fatalf("no RUN_LIVE_* variable found in any .go file under %s: this package "+
			"declares one itself, so an empty scan means the walk is broken, not that "+
			"the tiers are gone. A vacuous pass is the failure mode this check exists "+
			"to avoid.", root)
	}

	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	// Only table rows count. A tier named in passing in a paragraph is not the
	// table a reader scans to choose one, and it was prose that let the sets
	// diverge while every document still "mentioned" the variables.
	inTable := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		for _, name := range liveTierName.FindAllString(line, -1) {
			inTable[name] = true
		}
	}

	for _, name := range sortedKeys(inCode) {
		if inTable[name] {
			continue
		}
		t.Errorf("%s is a live tier this tree has (named in %s), but no row of README.md's "+
			"tier table mentions it. A paid tier missing from that table is one a "+
			"contributor cannot find, so it is never opted into and its credentials rot "+
			"unobserved. Add its row rather than removing this check.",
			name, strings.Join(inCode[name], ", "))
	}
	for name := range inTable {
		if _, ok := inCode[name]; ok {
			continue
		}
		t.Errorf("README.md's tier table offers %s, but no Go source in this tree carries "+
			"that string. Either the tier was removed and its row outlived it, or the row "+
			"misspells the variable — and a misspelled opt-in is one that silently never "+
			"opts in, which reads exactly like a tier that passed.", name)
	}
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
