package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var sdkVersionShape = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// fixture writes a throwaway module tree and returns its root. The resolver is
// tested against source it controls before it is pointed at the real SDK,
// because a resolver that agrees with whatever the SDK happens to contain
// cannot be told apart from one that agrees with everything.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("betaagent.go", `package sdk

type BetaAgentNewParams struct {
	Name        string
	Description string
}

func (p BetaAgentNewParams) MarshalJSON() ([]byte, error) { return nil, nil }

type BetaManagedAgentsWebFetchToolConfig struct{ Enabled bool }

const DefaultTimeout = 30

func init() {}
func init() {}

type Base struct{ ID string }

type Outer struct {
	Base
	Name string
}

type Stopper interface{ Halt() error }

type Iface interface {
	Stop() error
	Stopper
}

const (
	AlphaConst = 1
	BetaConst  = 2
	GammaConst = 3
)

func (o Outer) MarshalJSON() ([]byte, error) { return nil, nil }

// Documented carries two lines of doc comment above it, which is the shape the
// registry cites when it names "the response doc".
type Documented struct{ A string }
`)
	// A test file. A package's documented surface is not what its tests declare,
	// and counting them would admit an anchor on a symbol nothing exports and make
	// a source-unique name look ambiguous.
	write("betaagent_test.go", `package sdk

func TestOnlyHelper() {}

type BetaManagedAgentsWebFetchToolConfig struct{ Enabled bool }
`)
	// A package no importer outside the module can reach, redeclaring a name
	// the public surface also uses. It is not the package the index links, so
	// counting it would make a unique public name read as ambiguous.
	write("internal/apierror/apierror.go", `package apierror

type BetaEnvironment struct{ Code int }

func Wrap() error { return nil }
`)
	// Unexported names in an importable package, which no link can name.
	write("betafile.go", `package sdk

type BetaFile struct{ ID string }

type betaFile struct{ ID string }

func helper() {}
`)
	// The SDK ships a generated API index beside its source. Plan 51 anchors
	// its citations on symbols, so the resolver has to see a file it cannot
	// parse. Lines 4-6 name symbols the way the real index does, by a link
	// fragment: one the fixture does not declare, one it does, and one into the
	// standard library under a name the module also declares (client.go's New).
	// The lines after them link the names the symbol tests ask about, each into
	// the package the test means, so that what those tests exercise is that
	// package's count and not the link.
	link := func(name string) string {
		return `- <a href="https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go#` + name + `">` +
			name + "</a>\n"
	}
	// A link names the package it points into, and only this module's go.mod
	// says which import paths those are.
	into := func(importPath, name string) string {
		return `- <a href="https://pkg.go.dev/` + importPath + `#` + name + `">` + name + "</a>\n"
	}
	write("go.mod", "module github.com/anthropics/anthropic-sdk-go\n\ngo 1.22\n")
	write("api.md", "# API\n\n- BetaAgentNewParams\n"+
		`- <a href="https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go#BetaEnvironmentWorkService.Stop">Stop</a>`+"\n"+
		`- <a href="https://pkg.go.dev/github.com/anthropics/anthropic-sdk-go#BetaAgentNewParams.MarshalJSON">MarshalJSON</a>`+"\n"+
		`- <a href="https://pkg.go.dev/errors#New">New</a>`+"\n"+
		link("BetaEnvironment")+link("NoSuchSymbolAnywhere")+link("TestOnlyHelper")+
		link("BetaManagedAgentsWebFetchToolConfig")+link("BetaManagedAgentsWebFetchToolConfig.Enabled")+
		link("Wrap")+link("BaseURL")+
		into("github.com/anthropics/anthropic-sdk-go/shared", "ErrorObject")+
		into("github.com/anthropics/anthropic-sdk-go/shared", "BetaEnvironmentNewParams")+
		into("github.com/anthropics/anthropic-sdk-golib/environments", "Lister")+
		into("github.com/anthropics/anthropic-sdk-go/lib/environments", "Poller.Wait"))
	// The SDK declares its shared types once, in `shared`, and aliases them at
	// its root; the index links them in `shared`.
	write("shared/shared.go", "package shared\n\ntype ErrorObject struct{ Message string }\n")
	write("aliases.go", "package sdk\n\nimport \"github.com/anthropics/anthropic-sdk-go/shared\"\n\n"+
		"type ErrorObject = shared.ErrorObject\n")
	write("client.go", "package sdk\n\nfunc New() {}\n")
	write("betaenvironment.go", `package sdk

type BetaEnvironment struct{ Description string }

type BetaEnvironmentNewParams struct{ Description string }
`)
	write("lib/environments/poller.go", `package environments

type Poller struct{}

func (p *Poller) Wait() error { return nil }

type Lister struct{}
`)
	return dir
}

// TestResolveSymbol pins what counts as a symbol anchor resolving. Both
// polarities matter: a positive anchor that stops resolving and a negative one
// that starts again are the two transitions this whole plan exists to surface,
// so "not found" has to be a first-class answer rather than an error.
func TestResolveSymbol(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		file, sym  string
		wantFound  bool
		wantUnique bool
	}{
		{"a top-level type", "betaagent.go", "BetaManagedAgentsWebFetchToolConfig", true, true},
		{"a method by its receiver", "betaagent.go", "BetaAgentNewParams.MarshalJSON", true, true},
		{"a field by its struct", "betaagent.go", "BetaAgentNewParams.Description", true, true},
		{"a constant", "betaagent.go", "DefaultTimeout", true, true},
		{"a pointer receiver, reached by a path", "lib/environments/poller.go", "Poller.Wait", true, true},
		{"the same file by its basename", "poller.go", "Poller.Wait", true, true},
		{"a field that repeats across structs is still unique per struct", "betaenvironment.go", "BetaEnvironment.Description", true, true},
		{"an unqualified name that repeats is not unique", "betaagent.go", "init", true, false},
		{"an interface method", "betaagent.go", "Iface.Stop", true, true},
		{"an embedded interface", "betaagent.go", "Iface.Stopper", true, true},
		{"an embedded struct field, named by what embeds it", "betaagent.go", "Outer.Base", true, true},
		{"a constant inside a grouped declaration", "betaagent.go", "BetaConst", true, true},
		// Promotion is deliberately not followed: `Outer.ID` is declared on
		// Base, and an anchor that named the promoting type would move the day
		// the embedding changed without the declaration changing at all.
		{"a promoted field is cited where it is declared", "betaagent.go", "Base.ID", true, true},
		{"and not through the type that promotes it", "betaagent.go", "Outer.ID", false, false},
		{"a symbol the module does not ship", "betaagent.go", "resolveSkillVersion", false, false},
		// A file the module ships but no parser can read is answered by the
		// packages it links: an api.md line documents a symbol declared elsewhere.
		{"a symbol behind a documentation file", "api.md", "BetaEnvironment", true, true},
		{"a symbol no file in the module declares", "api.md", "NoSuchSymbolAnywhere", false, false},
		// Test files are not the surface a generated API index documents.
		{"a symbol only a test declares is not behind api.md", "api.md", "TestOnlyHelper", false, false},
		// Linked twice at the root, as itself and by a field: one package,
		// counted once.
		{"and a test redeclaring one does not make it ambiguous", "api.md",
			"BetaManagedAgentsWebFetchToolConfig", true, true},
		// Nor is an `internal/` tree, which is another package than the one
		// the index links; and an unexported name is nothing a link can name.
		{"an internal redeclaration does not make a public name ambiguous", "api.md",
			"BetaEnvironment", true, true},
		{"an exported name only an internal tree declares is not behind api.md", "api.md",
			"Wrap", false, false},
		{"nor is an unexported type", "api.md", "betaFile", false, false},
		{"nor an unexported function", "api.md", "helper", false, false},
		{"nor an unexported field of an exported struct", "api.md", "BetaFile.id", false, false},
		// The document has to link the symbol as well. The module alone would
		// answer for any file that is not Go, whatever the file says.
		{"a symbol the module declares and the document never links", "api.md", "DefaultTimeout", false, false},
		{"a linked name that only begins with the symbol does not link it", "api.md", "Base", false, false},
		{"a member linked under its type links the type", "api.md", "BetaAgentNewParams", true, true},
		{"a method linked under its receiver links the bare name", "api.md", "MarshalJSON", true, false},
		// And the package the link names has to be the one that declares it:
		// a name is counted where the document says it lives, not wherever the
		// module happens to spell it.
		{"a link into a package below the root", "api.md", "Poller", true, true},
		{"a root alias does not make a type linked in its own package ambiguous", "api.md", "ErrorObject", true, true},
		{"a link into a package that does not declare the name", "api.md", "BetaEnvironmentNewParams", false, false},
		{"a link into a module whose path only begins with this one's", "api.md", "Lister", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, unique, err := r.Resolve(tc.file, tc.sym)
			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", tc.file, tc.sym, err)
			}
			if found != tc.wantFound || unique != tc.wantUnique {
				t.Errorf("Resolve(%q, %q) = found=%v unique=%v, want found=%v unique=%v",
					tc.file, tc.sym, found, unique, tc.wantFound, tc.wantUnique)
			}
		})
	}
}

// TestResolveAmbiguousBasename pins the one case a basename cannot answer: two
// files with the same name in different directories. Guessing between them
// would make a citation resolve against a file it does not mean.
func TestResolveAmbiguousBasename(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{"a/dup.go", "b/dup.go"} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n\ntype T struct{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Resolve("dup.go", "T"); err == nil {
		t.Error("Resolve on an ambiguous basename returned no error: a citation that " +
			"could mean either file must say which, not be guessed at")
	}
	if found, _, err := r.Resolve("a/dup.go", "T"); err != nil || !found {
		t.Errorf("Resolve on the qualified path = found=%v err=%v, want found=true", found, err)
	}
}

// TestPinnedModuleDirIsReachableOffline is the premise every resolving rung
// rests on: `make verify` runs `build` first, so the pinned module is already
// materialised and needs no network. If this stops holding, the rungs below it
// are not weaker — they are silently absent, so this asserts it directly.
func TestPinnedModuleDirIsReachableOffline(t *testing.T) {
	mod, err := Module(repoRoot(t), SDKModule)
	if err != nil {
		t.Fatalf("resolving %s offline: %v\n\nThe resolving rungs rest on the pinned "+
			"module being in the build graph and therefore already on disk. If this "+
			"fails in CI, `make verify` no longer materialises it before the tests run.",
			SDKModule, err)
	}
	if _, err := os.Stat(filepath.Join(mod.Dir, "go.mod")); err != nil {
		t.Errorf("%s has no go.mod: %v", mod.Dir, err)
	}
	if !sdkVersionShape.MatchString(mod.Version) {
		t.Errorf("go list reported version %q, which is not a tag this tool can compare "+
			"a stamp against", mod.Version)
	}
	t.Logf("pinned module %s at %s", mod.Version, mod.Dir)
}

// TestDeclsInCountsSpecsNotBlocks pins what a span reason is falsified against.
// A parenthesised const block is one GenDecl and three declarations, and calling
// a range over all three "one declaration" would contradict a
// `crosses-declarations` span that is true — leaving the migrator no symbol to
// name in its place. The SDK is generated enum blocks throughout.
func TestDeclsInCountsSpecsNotBlocks(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	decls, err := r.DeclsIn("betaagent.go", 32, 34)
	if err != nil {
		t.Fatalf("DeclsIn over the grouped const: %v", err)
	}
	if len(decls) != 3 {
		t.Errorf("DeclsIn(32-34) = %d declaration(s) %+v, want the three constants",
			len(decls), decls)
	}
	one, err := r.DeclsIn("betaagent.go", 32, 32)
	if err != nil {
		t.Fatalf("DeclsIn over one spec: %v", err)
	}
	if len(one) != 1 || one[0].Name != "AlphaConst" {
		t.Errorf("DeclsIn(32-32) = %+v, want just AlphaConst", one)
	}
}

// TestDeclsInAnswersRangesItCannotRead. Each of these used to return nil with no
// error, which the falsifier read as "nothing to contradict" — a span nobody
// could ever check, which is the hatch the closed reason set exists to close.
func TestDeclsInAnswersRangesItCannotRead(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if decls, err := r.DeclsIn("betaagent.go", 9000, 9001); err != nil || len(decls) != 0 {
		t.Errorf("DeclsIn past the end of the file = %+v, %v; want no declarations and no error",
			decls, err)
	}
	if _, err := r.DeclsIn("api.md", 1, 2); err == nil {
		t.Error("DeclsIn on a file no parser can read returned no error, so a `non-go` span " +
			"over it would be reported as checked")
	}
	if _, err := r.DeclsIn("nosuch.go", 1, 2); err == nil {
		t.Error("DeclsIn on a file the module does not ship returned no error")
	}
}

// TestAFileTheModuleDoesNotShipIsNotASymbolNotFound. Answered `false`, an
// `absent at` anchor on a misspelt or moved file could never be contradicted —
// nothing in a file that does not exist resolves — and a positive one would read
// as a vanished symbol when what went away was the file.
func TestAFileTheModuleDoesNotShipIsNotASymbolNotFound(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"nosuch.go", "lib/nosuch/poller.go"} {
		_, _, err := r.Resolve(file, "Anything")
		var gone NotShipped
		if !errors.As(err, &gone) || gone.File != file {
			t.Errorf("Resolve(%q) error = %v, want NotShipped naming the file", file, err)
		}
	}
}

// TestDeclsInStartsAtTheDocComment. A declaration begins where its
// documentation does: plan 51 measured 19 of 104 coordinates resolving only
// because a doc-comment line is attributed to what it documents, and the
// registry cites those sentences by name. Reading from the keyword instead
// would call every one of those ranges empty and tell the migrator the span had
// drifted off something.
func TestDeclsInStartsAtTheDocComment(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	decls, err := r.DeclsIn("betaagent.go", 39, 40)
	if err != nil {
		t.Fatalf("DeclsIn over a doc comment: %v", err)
	}
	if len(decls) != 1 || decls[0].Name != "Documented" {
		t.Fatalf("DeclsIn(39-40) = %+v, want the declaration those lines document", decls)
	}
	if decls[0].From != 39 {
		t.Errorf("the declaration starts at line %d, want 39 — its doc comment, not its "+
			"keyword", decls[0].From)
	}
}

// TestLineCountMeasuresTheFileTheModuleShips. A span whose upper bound is past
// the last line has drifted, and it can do so while still touching
// declarations — so the count is what separates "this range still encloses
// something" from "this range is partly off the end of the file".
func TestLineCountMeasuresTheFileTheModuleShips(t *testing.T) {
	r, err := NewResolver(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.LineCount("betafile.go")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("LineCount(betafile.go) = %d, want 7", n)
	}
	if _, err := r.LineCount("nosuch.go"); err == nil {
		t.Error("LineCount of a file the module does not ship returned no error, so a span " +
			"over it would be measured against nothing")
	}
}

// TestDeclsInGivesAGroupItsOwnDocumentation. A comment above `const (` documents
// the block, so a range over it — or over the line that opens the block —
// encloses what the block declares, the same way a doc comment belongs to the
// declaration under it. Only a spec's own lines used to count, which called a
// true `crosses-declarations` span over a documented group empty.
func TestDeclsInGivesAGroupItsOwnDocumentation(t *testing.T) {
	dir := t.TempDir()
	const body = "package p\n\n// Constants used by the API.\nconst (\n\tA = 1\n\tB = 2\n)\n\nconst C = 3\n"
	if err := os.WriteFile(filepath.Join(dir, "group.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := func(from, to int) string {
		t.Helper()
		decls, err := r.DeclsIn("group.go", from, to)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range decls {
			out = append(out, fmt.Sprintf("%s@%d", d.Name, d.From))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name     string
		from, to int
		want     string
	}{
		{"the group's doc comment encloses the group", 3, 3, "A@3,B@3"},
		{"so does the line that opens it", 4, 4, "A@3,B@3"},
		{"one spec's own line is that spec alone", 5, 5, "A@5"},
		{"a declaration outside the group is not in it", 9, 9, "C@9"},
	} {
		if got := names(tc.from, tc.to); got != tc.want {
			t.Errorf("%s: DeclsIn(%d-%d) = %s, want %s", tc.name, tc.from, tc.to, got, tc.want)
		}
	}
}

// TestDeclsInSkipsImports. An import names a package; counted as a declaration
// it had no name, and a range over the imports "enclosed" something no citation
// could anchor on.
func TestDeclsInSkipsImports(t *testing.T) {
	dir := t.TempDir()
	const src = "package p\n\nimport \"fmt\"\n\nimport (\n\t\"os\"\n)\n\nfunc F() { fmt.Println(os.Args) }\n"
	if err := os.WriteFile(filepath.Join(dir, "p.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := NewResolver(dir)
	if err != nil {
		t.Fatal(err)
	}
	if decls, err := r.DeclsIn("p.go", 3, 7); err != nil || len(decls) != 0 {
		t.Errorf("DeclsIn over the imports = %+v, %v; want no declarations", decls, err)
	}
	if decls, err := r.DeclsIn("p.go", 1, 9); err != nil || len(decls) != 1 || decls[0].Name != "F" {
		t.Errorf("DeclsIn over the file = %+v, %v; want only F", decls, err)
	}
}

// TestAWorkspaceModuleIsNamedAsOne. A go.work `use` puts a local tree where the
// pin was, and `go list -m` reports its directory with no version. That answer
// read as naming no directory at all, which sent whoever saw it looking for a
// missing module rather than at the workspace.
func TestAWorkspaceModuleIsNamedAsOne(t *testing.T) {
	root := t.TempDir()
	directive := "go " + goDirective(t, repoRoot(t)) + "\n"
	for rel, body := range map[string]string{
		"go.mod":     "module example.com/main\n\n" + directive,
		"dep/go.mod": "module example.com/dep\n\n" + directive,
		"go.work":    directive + "\nuse (\n\t.\n\t./dep\n)\n",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOWORK", filepath.Join(root, "go.work"))
	_, err := Module(root, "example.com/dep")
	if err == nil || !strings.Contains(err.Error(), "go.work") {
		t.Errorf("Module of a workspace module = %v, want an error naming go.work", err)
	}
}

// TestRequiresIsWhatGoModRequires. Rung 1 attributes a version to another
// project only through a module go.mod requires, direct or indirect, and down to
// any package inside one.
func TestRequiresIsWhatGoModRequires(t *testing.T) {
	root := t.TempDir()
	mod := "module example.com/main\n\ngo " + goDirective(t, repoRoot(t)) +
		"\n\nrequire (\n\texample.com/a v1.0.0\n\texample.com/b v0.2.0 // indirect\n)\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := Requires(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "example.com/a,example.com/b" {
		t.Errorf("Requires = %v, want both requirements", paths)
	}
	required := Required(paths)
	for name, want := range map[string]bool{
		"example.com/a": true, "example.com/a/pkg": true, "example.com/ab": false, "example.com": false,
		// A major version from the second on is a module of its own, and
		// requiring the first says nothing about it. A first-version directory
		// or a package merely named like one is still inside.
		"example.com/a/v2": false, "example.com/a/v12/pkg": false, "example.com/a/v1": true,
		"example.com/a/v0": true, "example.com/a/v2x": true, "example.com/a/pkg/v2": true,
	} {
		if got := required(name); got != want {
			t.Errorf("Required(%v)(%q) = %v, want %v", paths, name, got, want)
		}
	}
	if _, err := Requires(t.TempDir()); err == nil {
		t.Error("Requires where there is no go.mod returned no error, so every module path " +
			"would silently attribute nothing")
	} else if !strings.Contains(err.Error(), "go.mod") {
		t.Errorf("Requires where there is no go.mod = %v: what go wrote to stderr says why, and "+
			"the exit status alone does not", err)
	}
}

// TestADocumentationAnchorNeedsTheModulesName. A link is read as pointing into
// the module by its import path, and only go.mod says what that path is: a tree
// with no go.mod, or one naming no module, cannot answer, and answering "not
// linked" would read as a vanished symbol.
func TestADocumentationAnchorNeedsTheModulesName(t *testing.T) {
	for name, gomod := range map[string]string{
		"no go.mod":           "",
		"no module directive": "go 1.22\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"api.md": `<a href="https://pkg.go.dev/example.com/m#T">T</a>`,
				"t.go":   "package m\n\ntype T struct{}\n",
			}
			if gomod != "" {
				files["go.mod"] = gomod
			}
			for rel, body := range files {
				if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			r, err := NewResolver(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.Resolve("api.md", "T"); err == nil || !strings.Contains(err.Error(), "go.mod") {
				t.Errorf("Resolve = %v, want an error naming go.mod", err)
			}
			if _, err := r.NamesIn("api.md", 1, 1); err == nil {
				t.Error("NamesIn returned no error, so a `non-go` span would pass over a range it never read")
			}
		})
	}
}

// TestTrackedIsWhatGitTracks. Whether a coordinate is ours decides whether it
// is reported, and which Go files are read decides what rung 1 sees, so neither
// can depend on what happens to be lying on a developer's disk: an untracked
// file of the SDK's name would silence every citation of it, on that machine and
// no other.
func TestTrackedIsWhatGitTracks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tracked.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTree(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "betasession.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Tracked(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "tracked.go" {
		t.Errorf("Tracked = %v, want only the tracked file", files)
	}
	ours := InSet(files)
	if !ours("tracked.go") || !ours("./tracked.go") {
		t.Error("a tracked file is not ours")
	}
	if ours("betasession.go") {
		t.Error("an untracked file counts as ours, so what is on this disk decides the verdict")
	}
	if _, err := Tracked(t.TempDir()); err == nil {
		t.Error("Tracked outside a repository returned no error, so an empty tree would read " +
			"as a corpus with nothing in it")
	}
}
