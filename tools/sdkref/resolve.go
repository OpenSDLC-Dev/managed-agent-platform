package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SDKModule is the module the registry cites most, and the one the resolving
// rungs are guaranteed to find: it is in the build graph, so `make verify` has
// already materialised it by the time any test runs.
const SDKModule = "github.com/anthropics/anthropic-sdk-go"

// Mod is a module as the build graph has it: where it is on disk, and which
// version that is. The two travel together because every resolving rung needs
// both — the directory to read, and the version to compare a stamp against.
type Mod struct {
	Dir     string
	Version string
	// Replaced names what the directory actually holds when a `replace` or a
	// `go.work` points the pinned version somewhere else. Judging citations
	// against a fork while reporting the pinned tag would be the quietest
	// possible wrong answer, so it is carried rather than dropped.
	Replaced string
}

// Module resolves a module of the build graph with the network refused.
//
// GOPROXY=off is not a precaution, it is the assertion: if the module were not
// already present, this would have to download it, and a gate that downloads is
// a gate that fails on a runner with no network rather than on a defect. The
// pinned version is the only one this can answer for, which is exactly why the
// failing rungs stop at the pin.
func Module(repoRoot, module string) (Mod, error) {
	const format = "{{.Dir}}\t{{.Version}}\t{{with .Replace}}{{.Path}}@{{.Version}}{{end}}"
	cmd := exec.Command("go", "list", "-m", "-f", format, module)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(), "GOPROXY=off")
	out, err := cmd.Output()
	if err != nil {
		// "the module is not here" and "go itself did not run" are different
		// answers, and a caller that files both under "unreachable" cannot tell
		// a cold cache from a broken toolchain.
		return Mod{}, fmt.Errorf("go list -m %s (GOPROXY=off): %s", module, failure(err))
	}
	// Only the line's end is trimmed: the format always writes three fields, and
	// trimming spaces as well ate the tabs of the two that can be empty.
	fields := strings.SplitN(strings.TrimRight(string(out), "\r\n"), "\t", 3)
	if len(fields) < 3 || fields[0] == "" {
		return Mod{}, fmt.Errorf("go list -m %s reported %q, which names no directory",
			module, strings.TrimSpace(string(out)))
	}
	if fields[1] == "" {
		// A module with a directory and no version is a workspace module: a
		// go.work `use` has put a local tree where the pin was, and that tree has
		// no tag for a citation to be judged at.
		return Mod{}, fmt.Errorf("go list -m %s names %s with no version: a go.work `use` "+
			"stands in for the pin, so there is no tag to judge a citation against",
			module, fields[0])
	}
	return Mod{Dir: fields[0], Version: fields[1], Replaced: fields[2]}, nil
}

// failure is what a command that failed says about why: what it wrote to stderr
// when it wrote anything, which names the cause, and otherwise the error, which
// says only that it did not run or did not succeed.
func failure(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}

// Requires lists the module paths go.mod requires, direct and indirect, which is
// what lets rung 1 read `k8s.io/api v0.36.2` as another project's version. It
// asks go rather than parsing the file, and `go mod edit` reads go.mod alone: no
// module graph, and no network.
func Requires(root string) ([]string, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit -json in %s: %s", root, failure(err))
	}
	var mod struct{ Require []struct{ Path string } }
	if err := json.Unmarshal(out, &mod); err != nil {
		return nil, fmt.Errorf("go mod edit -json in %s: %w", root, err)
	}
	paths := make([]string, 0, len(mod.Require))
	for _, r := range mod.Require {
		paths = append(paths, r.Path)
	}
	return paths, nil
}

// CachedModule finds a module version in the local module cache without asking
// the network for it. It is what makes plan 51's "older tags are reported when
// available and never required" land: rung 3 falsifies a span against its own
// stamped tag when the cache happens to hold it, and says so when it does not.
func CachedModule(modulePath, version string) (string, error) {
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMODCACHE: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("go env GOMODCACHE is empty")
	}
	dir := filepath.Join(root, filepath.FromSlash(escapeModulePath(modulePath))+"@"+version)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s@%s is not unpacked in the module cache", modulePath, version)
	}
	return dir, nil
}

// escapeModulePath applies the module cache's case encoding: an upper-case
// letter becomes `!` and its lower-case form, so two module paths differing
// only in case cannot collide on a case-insensitive filesystem.
func escapeModulePath(p string) string {
	var b strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Resolver answers whether a symbol anchor points at something the module still
// declares. It indexes the tree once: a citation names a file by basename far
// more often than by path, and the same basename in two directories has to be
// an error rather than a guess.
type Resolver struct {
	dir      string
	byPath   map[string]bool     // module-relative path -> shipped
	byBase   map[string][]string // basename -> module-relative paths
	goFiles  []string
	declsFor map[string]map[string]int
	pkgDecls map[string]map[string]int // package directory -> the names it exports, built on demand
	content  map[string][]byte         // module-relative path -> the file, read on demand
	linked   map[string][]link         // documentation file -> every symbol it links into the module
}

// NewResolver indexes the files under dir. Every file is indexed for existence,
// not only the Go ones: plan 51 anchors two citations on the SDK's generated
// `api.md`, and a resolver that could not see the file would call a live
// citation stale.
func NewResolver(dir string) (*Resolver, error) {
	r := &Resolver{
		dir:      dir,
		byPath:   map[string]bool{},
		byBase:   map[string][]string{},
		declsFor: map[string]map[string]int{},
		pkgDecls: map[string]map[string]int{},
		content:  map[string][]byte{},
		linked:   map[string][]link{},
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored or test-data trees are not the module's own surface and
			// would make a basename ambiguous for no reason.
			if name := d.Name(); name == "vendor" || name == "testdata" || (strings.HasPrefix(name, ".") && name != ".") {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		r.byPath[rel] = true
		r.byBase[d.Name()] = append(r.byBase[d.Name()], rel)
		if strings.HasSuffix(d.Name(), ".go") {
			r.goFiles = append(r.goFiles, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, paths := range r.byBase {
		sort.Strings(paths)
	}
	sort.Strings(r.goFiles)
	return r, nil
}

// Resolve reports whether file declares symbol, and whether that name is unique
// within the file.
//
// A symbol not found is an answer, not an error: an `absent at` citation is
// asserting exactly that, and the report's job is to notice when either polarity
// flips. A file not found is an error, a NotShipped: "the file is gone" and "the
// symbol is gone from the file" send a reader to different edits, and answering
// both with `false` would let an `absent at` claim on a misspelt path pass for
// ever, since nothing in a file that does not exist can resolve. An ambiguous
// basename is an error too, because guessing there would resolve a citation
// against a file it does not mean.
//
// A citation on a file that is not Go is answered by the document and the
// packages it links together. An `api.md` line documents a symbol declared
// elsewhere in the module, which is plan 51's reason for anchoring those
// citations on the symbol at all — so the document has to link the symbol, the
// way the SDK's index links every one it documents, and the package the link
// names has to declare it. The module alone would certify `README.md
// BetaSessionService` for a README that never names it, and would count the
// SDK's root aliases of its `shared` types as second declarations of names the
// index links in `shared` alone.
func (r *Resolver) Resolve(file, symbol string) (found, unique bool, err error) {
	rel, err := r.locate(file)
	if err != nil {
		return false, false, err
	}
	if !strings.HasSuffix(rel, ".go") {
		links, err := r.links(rel)
		if err != nil {
			return false, false, err
		}
		n, counted := 0, map[string]bool{}
		for _, l := range links {
			if counted[l.pkg] || !(l.name == symbol || strings.HasPrefix(l.name, symbol+".") ||
				strings.HasSuffix(l.name, "."+symbol)) {
				continue
			}
			counted[l.pkg] = true
			n += r.packageDeclares(l.pkg, symbol)
		}
		return n > 0, n == 1, nil
	}
	decls, err := r.declarations(rel)
	if err != nil {
		return false, false, err
	}
	n := decls[symbol]
	return n > 0, n == 1, nil
}

// NotShipped is the error for a file the module does not have.
type NotShipped struct{ File string }

func (e NotShipped) Error() string { return fmt.Sprintf("the module ships no %s", e.File) }

func (r *Resolver) locate(file string) (string, error) {
	file = strings.TrimPrefix(filepath.ToSlash(file), "./")
	if r.byPath[file] {
		return file, nil
	}
	if strings.Contains(file, "/") {
		// Only a bare basename can be ambiguous; a path either matches or names
		// nothing the module has.
		return "", NotShipped{file}
	}
	switch paths := r.byBase[file]; len(paths) {
	case 0:
		return "", NotShipped{file}
	case 1:
		return paths[0], nil
	default:
		return "", fmt.Errorf("%q names %d files in the module (%s): cite it by its "+
			"path, since a guess would resolve against a file the citation does not mean",
			file, len(paths), strings.Join(paths, ", "))
	}
}

// packageDeclares counts a name across one package of the module, for the
// citations whose file is documentation rather than source. It is built on
// demand, one package at a time, because it parses every file in the package,
// which a corpus with no such citation should not pay for.
//
// What counts is what the package's non-test files declare. A generated API
// index documents no test, so counting one would make a unique public name read
// as ambiguous because a test happens to share it. Unexported names need no such
// care: a link's fragment is exported in every part, so no anchor a document
// links can ask about one.
func (r *Resolver) packageDeclares(pkg, symbol string) int {
	decls, ok := r.pkgDecls[pkg]
	if !ok {
		decls = map[string]int{}
		for _, rel := range r.goFiles {
			if path.Dir(rel) != pkg || strings.HasSuffix(rel, "_test.go") {
				continue
			}
			found, err := r.declarations(rel)
			if err != nil {
				continue // an unparseable file in the dependency is not this tool's to report
			}
			for name, n := range found {
				decls[name] += n
			}
		}
		r.pkgDecls[pkg] = decls
	}
	return decls[symbol]
}

// documentedSymbol is how a documentation file names a Go symbol: as a link to
// it on the page of the package that declares it,
// `pkg.go.dev/<import path>#<name>`. The SDK's `api.md` writes every symbol that
// way, so a link is structure a parser can read, where a capitalised word in
// prose is not — and the import path says which package to ask, which a name
// alone cannot. A standard-library link such as `pkg.go.dev/context#Context`
// names no package of the module, and is left out by that.
var documentedSymbol = regexp.MustCompile(`pkg\.go\.dev/([\w./-]+)#([A-Z]\w*(?:\.[A-Z]\w*)?)\b`)

// moduleDirective is the line of a go.mod that names its module.
var moduleDirective = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?\s*$`)

// link is one symbol a documentation file links, with the directory of the
// package the link names, relative to the module.
type link struct{ pkg, name string }

// linksIn reads the links in a piece of documentation that point into this
// module. What the module is called is its go.mod's answer, so a tree with none
// is an error rather than a document that links nothing.
func (r *Resolver) linksIn(text string) ([]link, error) {
	mod, err := r.read("go.mod")
	if err != nil {
		return nil, fmt.Errorf("no go.mod names the module, so no link can be told to point into it: %w", err)
	}
	m := moduleDirective.FindSubmatch(mod)
	if m == nil {
		return nil, fmt.Errorf("go.mod has no module directive, so no link can be told to point into it")
	}
	var out []link
	for _, l := range documentedSymbol.FindAllStringSubmatch(text, -1) {
		// The module's own path, or a package below it. An import path that does
		// not begin with the module's is left whole by the trim, and so begins
		// with a domain rather than a slash.
		rest := strings.TrimPrefix(l[1], string(m[1]))
		if rest != "" && !strings.HasPrefix(rest, "/") {
			continue
		}
		out = append(out, link{pkg: path.Clean("./" + rest), name: l[2]})
	}
	return out, nil
}

// links is every link a documentation file makes into the module, read once.
func (r *Resolver) links(rel string) ([]link, error) {
	if l, ok := r.linked[rel]; ok {
		return l, nil
	}
	b, err := r.read(rel)
	if err != nil {
		return nil, err
	}
	l, err := r.linksIn(string(b))
	if err != nil {
		return nil, err
	}
	r.linked[rel] = l
	return l, nil
}

// read returns a file of the module, read once: the SDK's index is large, and
// every citation anchored on it would otherwise read it again.
func (r *Resolver) read(rel string) ([]byte, error) {
	if b, ok := r.content[rel]; ok {
		return b, nil
	}
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	r.content[rel] = b
	return b, nil
}

// NamesIn returns the symbols that lines from-to of a documentation file link,
// where the package each link names declares it. It is what makes `non-go`
// falsifiable over such a file: the reason claims there is no structure to name,
// and a range naming a declared symbol has one — which is why plan 51 anchors
// the `api.md` citations on their symbols rather than letting them keep a span.
func (r *Resolver) NamesIn(file string, from, to int) ([]string, error) {
	rel, err := r.locate(file)
	if err != nil {
		return nil, err
	}
	b, err := r.read(rel)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if from < 1 || to > len(lines) || from > to {
		return nil, fmt.Errorf("%s has no lines %d-%d", rel, from, to)
	}
	links, err := r.linksIn(strings.Join(lines[from-1:to], "\n"))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range links {
		if !seen[l.name] && r.packageDeclares(l.pkg, l.name) > 0 {
			seen[l.name] = true
			out = append(out, l.name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// LineCount is how many lines the module's copy of a file has, which is what
// makes "this range runs off the end" checkable. A span whose upper bound is
// past the last line has drifted whatever reason it gave, and a rung that only
// asked which declarations the range still touches would pass it in silence.
func (r *Resolver) LineCount(file string) (int, error) {
	rel, err := r.locate(file)
	if err != nil {
		return 0, err
	}
	b, err := r.read(rel)
	if err != nil {
		return 0, err
	}
	n := bytes.Count(b, []byte("\n"))
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		n++ // a last line with no terminator is still a line
	}
	return n, nil
}

// declarations counts every name a file declares, qualified the way a citation
// writes it: a method by its receiver, a field by its struct, an interface
// method by its interface, an embedded member by the type that embeds it. The
// count is what answers uniqueness — `no-unique-name` is a span reason
// precisely because generated union registration puts many `init` functions in
// one file.
func (r *Resolver) declarations(rel string) (map[string]int, error) {
	if d, ok := r.declsFor[rel]; ok {
		return d, nil
	}
	f, _, err := r.parse(rel)
	if err != nil {
		return nil, err
	}
	return r.count(rel, f), nil
}

// count is declarations over a file already parsed, so DeclsIn, which needs the
// syntax tree as well, does not parse the file a second time to get it.
func (r *Resolver) count(rel string, f *ast.File) map[string]int {
	if d, ok := r.declsFor[rel]; ok {
		return d
	}
	decls := map[string]int{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			decls[d.Name.Name]++
			if recv := receiverOf(d); recv != "" {
				decls[recv+"."+d.Name.Name]++
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					decls[s.Name.Name]++
					for _, member := range members(s.Type) {
						decls[s.Name.Name+"."+member]++
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						decls[n.Name]++
					}
				}
			}
		}
	}
	r.declsFor[rel] = decls
	return decls
}

func (r *Resolver) parse(rel string) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	// ParseComments, because a declaration's extent has to include the doc
	// comment above it — see DeclsIn. Without it every doc-comment line looks
	// like it belongs to no declaration at all.
	f, err := parser.ParseFile(fset, filepath.Join(r.dir, filepath.FromSlash(rel)), nil,
		parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	return f, fset, nil
}

// members names what a type declaration exposes under its own name: struct
// fields including the embedded ones, and interface methods including embedded
// interfaces. The SDK's params and response structs embed heavily, so a
// resolver that read only named fields would report a live `Params.Body` anchor
// as vanished.
func members(t ast.Expr) []string {
	var fields *ast.FieldList
	switch t := t.(type) {
	case *ast.StructType:
		fields = t.Fields
	case *ast.InterfaceType:
		fields = t.Methods
	}
	if fields == nil {
		return nil
	}
	var out []string
	for _, fld := range fields.List {
		if len(fld.Names) == 0 {
			if name := embeddedName(fld.Type); name != "" {
				out = append(out, name)
			}
			continue
		}
		for _, n := range fld.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// embeddedName is the name an embedded field or interface is reached by.
func embeddedName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return embeddedName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return embeddedName(t.X)
	case *ast.IndexListExpr:
		return embeddedName(t.X)
	}
	return ""
}

func receiverOf(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return ""
	}
	return embeddedName(d.Recv.List[0].Type)
}

// Decl is one declaration and the lines it spans.
type Decl struct {
	Name        string
	From, To    int
	NameRepeats bool
}

// DeclsIn returns the declarations whose extent intersects the line range,
// which is what makes a span reason falsifiable: `crosses-declarations` is
// false when the range touches one, and `no-unique-name` is false when that
// one's name appears once in the file.
//
// A parenthesised `const (…)` counts as one declaration per spec, not one per
// block. The SDK is full of generated enum blocks, and calling a range over
// three of them "one declaration" would falsify a `crosses-declarations` span
// that is true and leave the migrator no symbol to name instead.
func (r *Resolver) DeclsIn(file string, from, to int) ([]Decl, error) {
	rel, err := r.locate(file)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(rel, ".go") {
		return nil, fmt.Errorf("%s is not Go, so no parser can say what its lines enclose", rel)
	}
	f, fset, err := r.parse(rel)
	if err != nil {
		return nil, err
	}
	decls := r.count(rel, f)
	var out []Decl
	// A declaration begins at its doc comment, not at its keyword. The registry
	// cites documentation sentences constantly — a line of `betaenvironment.go`
	// glossed as "the response doc" — and plan 51's measurement found a share of
	// its coordinates resolving only because a doc-comment line is attributed to
	// what it documents. Reading from Pos() instead would call every one of those
	// ranges empty and tell the migrator it had drifted off something.
	add := func(name string, node ast.Node, doc *ast.CommentGroup) {
		lo, hi := fset.Position(node.Pos()).Line, fset.Position(node.End()).Line
		if doc != nil {
			lo = fset.Position(doc.Pos()).Line
		}
		if hi < from || lo > to {
			return
		}
		out = append(out, Decl{Name: name, From: lo, To: hi, NameRepeats: decls[name] > 1})
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if recv := receiverOf(d); recv != "" {
				name = recv + "." + name
			}
			add(name, d, d.Doc)
		case *ast.GenDecl:
			// An import names a package, not a declaration of this file: a range
			// over the import block would otherwise "enclose" nameless
			// declarations, and a span reason would be judged against them.
			if d.Tok == token.IMPORT {
				continue
			}
			if d.Lparen == token.NoPos {
				add(declName(d), d, d.Doc)
				continue
			}
			// A block's own documentation, and the line that opens it, speak
			// for everything in it: a comment above `const (` documents the
			// group. So a range over those lines encloses every spec, and each
			// spec's extent begins where the group's does.
			head := fset.Position(d.Pos()).Line
			if d.Doc != nil {
				head = fset.Position(d.Doc.Pos()).Line
			}
			opens := fset.Position(d.Lparen).Line
			headHit := !(opens < from || head > to)
			for _, spec := range d.Specs {
				n := len(out)
				add(specName(spec), spec, specDoc(spec))
				if len(out) > n {
					if headHit {
						out[n].From = head
					}
					continue
				}
				if headHit {
					hi := fset.Position(spec.End()).Line
					name := specName(spec)
					out = append(out, Decl{Name: name, From: head, To: hi, NameRepeats: decls[name] > 1})
				}
			}
		}
	}
	return out, nil
}

func declName(d *ast.GenDecl) string {
	for _, spec := range d.Specs {
		if name := specName(spec); name != "" {
			return name
		}
	}
	return ""
}

func specDoc(spec ast.Spec) *ast.CommentGroup {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Doc
	case *ast.ValueSpec:
		return s.Doc
	}
	return nil
}

func specName(spec ast.Spec) string {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name.Name
	case *ast.ValueSpec:
		if len(s.Names) > 0 {
			return s.Names[0].Name
		}
	}
	return ""
}

// Tracked lists the files this repository ships, relative to root: both the Go
// files whose comments are half the corpus, and the set InSet makes into the
// predicate Scanner needs — whether a coordinate names a file of ours, and is
// therefore out of the grammar's scope.
//
// A prefix cannot answer that. The registry cites the SDK's
// `internal/apierror/apierror.go` and our own `internal/api/server.go` in the
// same clause, so the only thing that separates them is which tree holds them.
//
// "Ships" means tracked by git, not present on disk. A walk of the working tree
// let whatever a developer had lying around decide the verdict: an untracked
// `betasession.go` at the root would have silenced every comment citing the
// SDK's file of that name, on that machine and no other. So this needs git and a
// clone, which every place the gate runs has — CI checks the repository out
// with git — and a source archive does not; that fails here, loudly, rather
// than reading an empty tree as a clean one.
func Tracked(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %s", root, failure(err))
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// InSet is the predicate over a list of repository paths.
func InSet(paths []string) func(string) bool {
	have := map[string]bool{}
	for _, p := range paths {
		have[p] = true
	}
	return func(path string) bool { return have[strings.TrimPrefix(path, "./")] }
}
