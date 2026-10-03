package store_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

// Every DELETE FROM files the platform runs owes what its rows name, and says
// whether it may take a session's file copy (#856).
//
// A copy names its source's object in object_key (0046), so a remover that
// returns ids and owes files/{id}, as every remover did before 0046, owes a
// key where nothing is stored and never the object a copy shares: deleting
// the last row naming it would leave the object stored and owed by nothing.
// And since 0047 nothing in the schema keeps a delete off a copy.
//
// What is read: every constant string expression in the non-test Go files of
// the packages under cmd/ and internal/, as the type checker folds it, so a
// statement split across concatenated literals or named constants is read
// whole (a constant declaration's value is read where the constant is used);
// and every migration after 0047, comments stripped. The rules:
//
//  1. A DELETE FROM names its table in the same constant. One whose table
//     comes at run time (fmt.Sprintf's %s, a variable appended) fails, to be
//     rewritten as a constant or reviewed and listed here.
//  2. A DELETE FROM files in Go ends `RETURNING ` + store.FileObjectKeySQL
//     (its value, whatever constant spells it) and returns nothing else.
//  3. Each DELETE FROM files either excludes copies (`source_file_id IS
//     NULL`), as the outputs harvest does, or is listed in reach with why it
//     may meet one. A migration's is listed always: no Go caller owes its
//     keys, so the entry says how it does.
//
// A remover written the pre-0046 way, `DELETE FROM files WHERE scope_type =
// 'session' AND scope_id = $1 RETURNING id`, breaks rules 2 and 3.
//
// What is not read: a statement whose DELETE and FROM are themselves pieced
// together at run time, and SQL that is neither a Go constant nor a
// migration, such as a file read at run time. TestFilesDeleteScanCatchesEachEvasion
// pins what is.
func TestEveryFilesDeleteOwesItsKeyAndDecidesAboutCopies(t *testing.T) {
	reach := map[string]int{
		"internal/api/files.go":         1, // deleteFile: the id may be a copy's
		"internal/api/sessions.go":      1, // deleteSession: the session's copies go with it
		"internal/api/fileretention.go": 1, // purgeExpiredFiles: a copy expires with its upload
		// enqueueDreamBlobs deletes by dream_id, which no copy carries
		// (internal/api's TestADreamsCloseLeavesASessionsCopyOfItsTranscript).
		"internal/api/dreamrunner.go": 1,
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scan := newDeleteScan()
	fset := token.NewFileSet()
	pkgs, imp := productionPackages(t, root, fset)
	for _, p := range pkgs {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		if err := scan.goPackage(fset, imp, p.ImportPath, files, root); err != nil {
			t.Fatalf("type-check %s: %v", p.ImportPath, err)
		}
	}
	dir := filepath.Join(root, "internal", "store", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() <= "0047_drop_file_copy_guard.sql" || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		scan.migration("internal/store/migrations/"+e.Name(), string(src))
	}
	for _, b := range scan.breaks {
		t.Error(b)
	}
	if !maps.Equal(scan.reach, reach) {
		t.Errorf("DELETE FROM files that may meet a copy, per file = %v, want %v.\n"+
			"A new or moved delete either excludes session copies or is listed here with why it may meet one.",
			scan.reach, reach)
	}
}

// Each way of writing a DELETE FROM files that a scan of lone literals would
// miss is read: pieces concatenated, a named constant, a table that comes at
// run time, and a migration after 0047. The two well-formed statements, one
// spelled through constants, break nothing, and only the one that leaves
// copies in reach counts.
func TestFilesDeleteScanCatchesEachEvasion(t *testing.T) {
	src := `package p

const key = "coalesce(object_key, 'files/' || id)"

const bare = "DELETE FROM files WHERE id = $1"

const prefix = "DELETE FROM files WHERE id = $1 RETURNING "

func sprintf(format string, args ...any) string { return format }

func removers(exec func(string), table string) {
	exec("DELETE FROM " + "files WHERE scope_type = 'session' AND scope_id = $1 RETURNING id") // concatenated
	exec(bare)                                                                                    // named
	exec(sprintf("DELETE FROM %s WHERE id = $1", table))                                          // run-time table
	exec("DELETE FROM " + table)                                                                  // run-time table
	exec(prefix + key)                                                                            // well formed, may meet a copy
	exec("DELETE FROM files WHERE id = $1 AND source_file_id IS NULL RETURNING " + key)          // well formed, excludes copies
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "/m/internal/p/p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	scan := newDeleteScan()
	if err := scan.goPackage(fset, nil, "p", []*ast.File{f}, "/m"); err != nil {
		t.Fatal(err)
	}
	scan.migration("internal/store/migrations/0048_later.sql",
		"-- DELETE FROM files in a comment is not a statement\n"+
			"/* nor DELETE FROM files here */\n"+
			"DELETE FROM files WHERE scope_type = 'session' AND source_file_id IS NULL;\n")

	var lines []int
	for _, b := range scan.breaks {
		var line int
		if _, err := fmt.Sscanf(b[strings.Index(b, ":")+1:], "%d", &line); err != nil {
			t.Fatalf("break without a line: %s", b)
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	// The concatenation, the named constant, and the two run-time tables.
	if want := []int{12, 13, 14, 15}; !slices.Equal(lines, want) {
		t.Errorf("breaks on lines %v, want %v:\n%s", lines, want, strings.Join(scan.breaks, "\n"))
	}
	// The concatenation, the named constant and prefix + key in Go; the
	// migration's, listed whatever its predicate.
	want := map[string]int{"internal/p/p.go": 3, "internal/store/migrations/0048_later.sql": 1}
	if !maps.Equal(scan.reach, want) {
		t.Errorf("reach = %v, want %v", scan.reach, want)
	}
}

var (
	deleteFromRE     = regexp.MustCompile(`(?i)\bdelete\s+from\b`)
	deleteTableRE    = regexp.MustCompile(`(?i)^\s+(?:only\s+)?("?[a-z_][a-z0-9_]*"?(?:\."?[a-z_][a-z0-9_]*"?)?)`)
	excludesCopiesRE = regexp.MustCompile(`(?i)\bsource_file_id\s+is\s+null\b`)
	returningRE      = regexp.MustCompile(`(?i)\breturning\b`)
	sqlCommentRE     = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)
	spaceRE          = regexp.MustCompile(`\s+`)
)

// deleteScan collects what the rules find: the breaks, each naming its site,
// and per file the DELETE FROM files that may meet a copy.
type deleteScan struct {
	breaks []string
	reach  map[string]int
}

func newDeleteScan() *deleteScan { return &deleteScan{reach: map[string]int{}} }

// statement holds one SQL text, found at site in file, to the rules; inGo
// says a Go caller runs it, which rule 2 asks of.
func (d *deleteScan) statement(file, site, sql string, inGo bool) {
	for _, loc := range deleteFromRE.FindAllStringIndex(sql, -1) {
		m := deleteTableRE.FindStringSubmatch(sql[loc[1]:])
		if m == nil {
			d.breaks = append(d.breaks, fmt.Sprintf(
				"%s: a DELETE FROM whose table is not in the constant; write it as one, or review it and list it:\n%s", site, sql))
			continue
		}
		if t := strings.ToLower(strings.ReplaceAll(m[1], `"`, "")); t != "files" && t != "public.files" {
			continue
		}
		if inGo {
			norm := strings.ToLower(strings.TrimSpace(spaceRE.ReplaceAllString(sql, " ")))
			key := strings.ToLower(spaceRE.ReplaceAllString("returning "+store.FileObjectKeySQL, " "))
			if len(returningRE.FindAllString(sql, -1)) != 1 || !strings.HasSuffix(norm, key) {
				d.breaks = append(d.breaks, fmt.Sprintf(
					"%s: a DELETE FROM files that does not end `RETURNING ` + store.FileObjectKeySQL, "+
						"so it cannot owe the object a copy shares:\n%s", site, sql))
			}
		}
		if !inGo || !excludesCopiesRE.MatchString(sql) {
			d.reach[file]++
		}
	}
}

// migration holds a migration's statements to the rules, its comments
// stripped.
func (d *deleteScan) migration(file, src string) {
	d.statement(file, file, sqlCommentRE.ReplaceAllString(src, " "), false)
}

// goPackage type-checks one package and holds each of its constant string
// expressions to the rules, read whole: the outermost one, never a piece of
// a larger one, and never a constant declaration's value, which is read
// where the constant is used. Paths are reported relative to root.
func (d *deleteScan) goPackage(fset *token.FileSet, imp types.Importer, path string, files []*ast.File, root string) error {
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if _, err := (&types.Config{Importer: imp}).Check(path, fset, files, info); err != nil {
		return err
	}
	type span struct{ pos, end token.Pos }
	within := func(e ast.Expr, s span) bool { return s.pos <= e.Pos() && e.End() <= s.end }
	// Strictly: two expressions over one span would each hide the other.
	inside := func(e ast.Expr, s span) bool { return within(e, s) && (s.pos != e.Pos() || s.end != e.End()) }
	var decls []span
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if g, ok := n.(*ast.GenDecl); ok && g.Tok == token.CONST {
				for _, spec := range g.Specs {
					for _, v := range spec.(*ast.ValueSpec).Values {
						decls = append(decls, span{v.Pos(), v.End()})
					}
				}
			}
			return true
		})
	}
	var found []ast.Expr
	for e, tv := range info.Types {
		if tv.Value != nil && tv.Value.Kind() == constant.String && deleteFromRE.MatchString(constant.StringVal(tv.Value)) {
			found = append(found, e)
		}
	}
	slices.SortFunc(found, func(a, b ast.Expr) int { return int(a.Pos() - b.Pos()) })
	for _, e := range found {
		if slices.ContainsFunc(decls, func(s span) bool { return within(e, s) }) ||
			slices.ContainsFunc(found, func(o ast.Expr) bool { return inside(e, span{o.Pos(), o.End()}) }) {
			continue
		}
		pos := fset.Position(e.Pos())
		rel, err := filepath.Rel(root, pos.Filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		d.statement(rel, fmt.Sprintf("%s:%d", rel, pos.Line), constant.StringVal(info.Types[e].Value), true)
	}
	return nil
}

// listedPackage is what go list says of a package.
type listedPackage struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
	DepOnly                 bool
	Error                   *struct{ Err string }
}

// productionPackages lists the packages under cmd/ and internal/, and an
// importer that reads every dependency's compiled export data, which go list
// -export builds, so each package type-checks from source without its
// dependencies being parsed.
func productionPackages(t *testing.T, root string, fset *token.FileSet) ([]listedPackage, types.Importer) {
	t.Helper()
	cmd := exec.Command("go", "list", "-e", "-export", "-deps",
		"-json=ImportPath,Dir,GoFiles,Export,DepOnly,Error", "./cmd/...", "./internal/...")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []listedPackage
	exports := map[string]string{}
	for dec := json.NewDecoder(strings.NewReader(string(out))); dec.More(); {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Error != nil {
			t.Fatalf("go list %s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly {
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("go list found no package under cmd/ or internal/")
	}
	return pkgs, importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
}
