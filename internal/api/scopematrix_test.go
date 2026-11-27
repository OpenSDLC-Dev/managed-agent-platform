package api_test

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	scopeCreate     = regexp.MustCompile(`(?is)\bcreate\s+table\s+([\w.]+)\s*\(([^;]+)`)
	scopeAlter      = regexp.MustCompile(`(?is)\balter\s+table\s+([\w.]+)\s+add\s+(?:column\s+)?workspace_id\b`)
	scopeReference  = regexp.MustCompile(`(?i)\breferences\s+([\w.]+)\s*\(`)
	scopeRelation   = regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+((?:"[\w$]+"|[\w$]+)(?:\s*\.\s*(?:"[\w$]+"|[\w$]+))?)`)
	scopeBinding    = regexp.MustCompile(`(?i)\bworkspace_id\s*=\s*\$[1-9]|\binsert\s+into\s+[\w.]+\s*\([^)]*\bworkspace_id\b`)
	scopeDynamic    = regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+(?:<\?>|%s)`)
	scopeDDLComment = regexp.MustCompile(`(?m)--.*$`)
	scopeStatement  = regexp.MustCompile(`(?i)^(?:select|insert|update|delete|with)\b`)
)

// Derive the roots and children from the database schema. A new table or package
// enters the guard as soon as its first SQL statement lands.
func resourceRelations(t *testing.T, root string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "internal/store/migrations/*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("read migrations: %v", err)
	}
	scoped := map[string]bool{}
	parents := map[string][]string{}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sql := strings.ToLower(scopeDDLComment.ReplaceAllString(string(body), ""))
		for _, m := range scopeCreate.FindAllStringSubmatch(sql, -1) {
			if strings.Contains(m[2], "workspace_id") {
				scoped[m[1]] = true
			}
			for _, ref := range scopeReference.FindAllStringSubmatch(m[2], -1) {
				parents[m[1]] = append(parents[m[1]], ref[1])
			}
		}
		for _, m := range scopeAlter.FindAllStringSubmatch(sql, -1) {
			scoped[m[1]] = true
		}
	}
	// Principals and browser login sessions belong to the organization. Audit
	// attribution via principals does not turn an organization table into a resource.
	delete(scoped, "principals")
	for changed := true; changed; {
		changed = false
		for child, refs := range parents {
			for _, parent := range refs {
				if scoped[parent] && !scoped[child] {
					scoped[child] = true
					changed = true
				}
			}
		}
	}
	return scoped
}

func scopeTableNames(sql string, tables map[string]bool) []string {
	var names []string
	for _, m := range scopeRelation.FindAllStringSubmatch(sql, -1) {
		parts := strings.Split(m[1], ".")
		for i, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, `"`) {
				parts[i] = strings.Trim(part, `"`)
			} else {
				parts[i] = strings.ToLower(part)
			}
		}
		name := strings.TrimPrefix(strings.Join(parts, "."), "public.")
		if tables[name] && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

func TestWorkspaceSQLIdentifiers(t *testing.T) {
	tables := map[string]bool{"agents": true, "modelgateway.usage": true}
	for _, query := range []string{"SELECT count(*) FROM AGENTS", "SELECT count(*) FROM Agents", "SELECT count(*) FROM MODELGATEWAY.Usage", `SELECT * FROM public."agents"`, `SELECT * FROM "modelgateway".usage`} {
		if len(scopeTableNames(query, tables)) != 1 {
			t.Fatalf("resource table missing from inventory for %s", query)
		}
	}
}

func TestWorkspaceSQLScope(t *testing.T) {
	root := filepath.Join("..", "..")
	tables := resourceRelations(t, root)
	exceptions := map[string]bool{}
	f, err := os.Open("testdata/workspace_sql.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		line := lines.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 || cols[2] == "" {
			t.Fatalf("invalid scope exception %q", line)
		}
		for _, digest := range strings.Fields(cols[1]) {
			exceptions[cols[0]+" "+digest] = false
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	var missing []string
	check := func(site, sql string) {
		sql = strings.Join(strings.Fields(sql), " ")
		names := scopeTableNames(sql, tables)
		if scopeStatement.MatchString(sql) && scopeDynamic.MatchString(sql) {
			names = append(names, "dynamic table")
		}
		if len(names) == 0 || scopeBinding.MatchString(sql) {
			return
		}
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(sql)))[:16]
		key := site + " " + digest
		if _, ok := exceptions[key]; ok {
			exceptions[key] = true
			return
		}
		missing = append(missing, fmt.Sprintf("%s\t%s\t%s\t%s", site, digest, strings.Join(names, ","), sql))
	}
	for _, tree := range []string{"internal", "cmd"} {
		err = filepath.WalkDir(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "pgtest" || d.Name() == "apitest" || d.Name() == "migrations" {
					return filepath.SkipDir
				}
				return nil
			}
			site, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if strings.HasSuffix(path, ".sql") {
				sql, err := os.ReadFile(path)
				if err == nil {
					check(site, string(sql))
				}
				return err
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			node, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range node.Decls {
				where := site
				if fn, ok := decl.(*ast.FuncDecl); ok {
					where += "." + fn.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch v := n.(type) {
					case *ast.BinaryExpr:
						if v.Op == token.ADD {
							if sql, ok := scopeSQLText(v); ok {
								check(where, sql)
								return false
							}
						}
					case *ast.BasicLit:
						if v.Kind == token.STRING {
							sql, _ := strconv.Unquote(v.Value)
							check(where, sql)
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(missing)
	for _, line := range missing {
		t.Errorf("unclassified workspace SQL: %s", line)
	}
	for key, seen := range exceptions {
		if !seen {
			t.Errorf("stale workspace SQL exception: %s", key)
		}
	}
}

// Keep a concatenated SQL statement together, including the fragments around a
// shared projection. A literal that changes a reviewed exception changes its key.
func scopeSQLText(e ast.Expr) (string, bool) {
	switch n := e.(type) {
	case *ast.BasicLit:
		if n.Kind == token.STRING {
			v, err := strconv.Unquote(n.Value)
			return v, err == nil
		}
	case *ast.BinaryExpr:
		if n.Op == token.ADD {
			left, l := scopeSQLText(n.X)
			right, r := scopeSQLText(n.Y)
			return left + right, l || r
		}
	}
	return "<?>", false
}
