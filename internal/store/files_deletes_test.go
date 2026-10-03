package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every DELETE FROM files in production code owes what its rows name, and
// says whether it may take a session's file copy (#856).
//
// A copy names its source's object in object_key (0046), so a remover that
// returns ids and owes files/{id}, as every remover did before 0046, owes a
// key where nothing is stored and never the object a copy shares: deleting
// the last row naming it would leave the object stored and owed by nothing.
// So each statement ends `RETURNING ` + store.FileObjectKeySQL.
//
// Since 0047 nothing in the schema keeps a delete off a copy, so each
// statement either excludes copies (`source_file_id IS NULL`), as the outputs
// harvest does, or is listed in reach below with why it may meet one. A
// remover written the pre-0046 way, `DELETE FROM files WHERE scope_type =
// 'session' AND scope_id = $1 RETURNING id`, fails both rules.
//
// Go source only: no migration since 0047 deletes from files.
func TestEveryFilesDeleteOwesItsKeyAndDecidesAboutCopies(t *testing.T) {
	reach := map[string]int{
		"internal/api/files.go":         1, // deleteFile: the id may be a copy's
		"internal/api/sessions.go":      1, // deleteSession: the session's copies go with it
		"internal/api/fileretention.go": 1, // purgeExpiredFiles: a copy expires with its upload
		// enqueueDreamBlobs deletes by dream_id, which no copy carries
		// (internal/api's TestADreamsCloseLeavesASessionsCopyOfItsTranscript).
		"internal/api/dreamrunner.go": 1,
	}
	deleteRE := regexp.MustCompile(`(?i)\bdelete\s+from\s+files\b`)
	excludesCopiesRE := regexp.MustCompile(`(?i)\bsource_file_id\s+is\s+null\b`)
	returningRE := regexp.MustCompile(`(?i)\breturning\b`)

	root := filepath.Join("..", "..")
	got := map[string]int{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			deletes := func(lit *ast.BasicLit) (string, bool) {
				if lit.Kind != token.STRING {
					return "", false
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					s = lit.Value
				}
				return s, deleteRE.MatchString(s)
			}
			// The literals that end RETURNING and are followed by the key.
			owing := map[*ast.BasicLit]bool{}
			ast.Inspect(f, func(n ast.Node) bool {
				b, ok := n.(*ast.BinaryExpr)
				if !ok || b.Op != token.ADD {
					return true
				}
				lit, ok := b.X.(*ast.BasicLit)
				if !ok {
					return true
				}
				s, ok := deletes(lit)
				if !ok {
					return true
				}
				trimmed := strings.TrimSpace(s)
				if isFileObjectKeySQL(b.Y) && len(returningRE.FindAllString(s, -1)) == 1 &&
					strings.EqualFold(trimmed[max(0, len(trimmed)-len("returning")):], "returning") {
					owing[lit] = true
				}
				return true
			})
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok {
					return true
				}
				s, ok := deletes(lit)
				if !ok {
					return true
				}
				if !owing[lit] {
					t.Errorf("%s: a DELETE FROM files that does not end `RETURNING ` + store.FileObjectKeySQL, "+
						"so it cannot owe the object a copy shares:\n%s", fset.Position(lit.Pos()), s)
				}
				if !excludesCopiesRE.MatchString(s) {
					got[rel]++
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if !maps.Equal(got, reach) {
		t.Errorf("DELETE FROM files without `source_file_id IS NULL`, per file = %v, want %v.\n"+
			"A new or moved delete either excludes session copies or is listed here with why it may meet one.", got, reach)
	}
}

// isFileObjectKeySQL reports whether e names store.FileObjectKeySQL, qualified
// or, inside package store, bare.
func isFileObjectKeySQL(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		return ok && pkg.Name == "store" && e.Sel.Name == "FileObjectKeySQL"
	case *ast.Ident:
		return e.Name == "FileObjectKeySQL"
	}
	return false
}
