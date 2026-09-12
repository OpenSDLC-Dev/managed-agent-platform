package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// pinnedSDK is the SDK at whatever go.mod pins, and the facts the tests against
// it need, read out of its files' text.
//
// None of them is written down here. A symbol, a schema or a doc comment named in
// this file is what the next bump deletes, and a test that failed on a bump for
// its own premise would bring back the per-bump edit plan 51 exists to remove.
// They are read as text rather than through the resolver under test, so the
// premise cannot simply agree with what it checks.
type pinnedSDK struct {
	Mod
	roots  []string // the Go files at the module's root, tests aside, sorted
	file   string   // one of them
	method string   // a method that file declares, as Type.Method, declared once in the module
	absent string   // a name nothing in that file mentions
	linked string   // a Type.Method api.md links at the root, declared once, in a root file
	schema string   // a schema the SDK's spec carries
}

// methodDecl is a method declaration with an exported receiver and name.
var methodDecl = regexp.MustCompile(`(?m)^func \(\w+ \*?([A-Z]\w*)\) ([A-Z]\w*)\(`)

// indexLink is api.md's link to a method in the SDK's root package.
var indexLink = regexp.MustCompile(`pkg\.go\.dev/github\.com/anthropics/anthropic-sdk-go#([A-Z]\w*\.[A-Z]\w*)"`)

// pinned is the pinned SDK read once for every test that needs it: reading it
// walks the module and decompresses its spec.
var pinned struct {
	once sync.Once
	sdk  pinnedSDK
	err  error
}

func realSDK(t *testing.T) pinnedSDK {
	t.Helper()
	root := repoRoot(t)
	pinned.once.Do(func() { pinned.sdk, pinned.err = readSDK(root) })
	if pinned.err != nil {
		t.Fatal(pinned.err)
	}
	return pinned.sdk
}

func readSDK(root string) (pinnedSDK, error) {
	mod, err := Module(root, SDKModule)
	if err != nil {
		return pinnedSDK{}, fmt.Errorf("resolving %s offline: %w", SDKModule, err)
	}
	sdk := pinnedSDK{Mod: mod, absent: "sdkrefNeverDeclaredHere"}

	// Every exported method the importable, non-test source declares, counted,
	// so a subject is one whose name a citation could use without ambiguity.
	count := map[string]int{}
	atRoot := map[string]bool{}
	byFile := map[string][]string{}
	err = filepath.WalkDir(mod.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(mod.Dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); name == "internal" || name == "testdata" || name == "vendor" ||
				(strings.HasPrefix(name, ".") && rel != ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		root := !strings.Contains(rel, "/")
		if root {
			sdk.roots = append(sdk.roots, rel)
		}
		for _, m := range methodDecl.FindAllStringSubmatch(string(src), -1) {
			name := m[1] + "." + m[2]
			count[name]++
			if root {
				atRoot[name] = true
				if !strings.Contains(string(src), sdk.absent) {
					byFile[rel] = append(byFile[rel], name)
				}
			}
		}
		return nil
	})
	if err != nil {
		return pinnedSDK{}, fmt.Errorf("reading %s: %w", mod.Dir, err)
	}
	sort.Strings(sdk.roots)
	for _, f := range sdk.roots {
		if i := slices.IndexFunc(byFile[f], func(m string) bool { return count[m] == 1 }); i >= 0 {
			sdk.file, sdk.method = f, byFile[f][i]
			break
		}
	}
	if sdk.file == "" {
		return pinnedSDK{}, fmt.Errorf("%s at %s declares no method once at its root, so there is no subject", SDKModule, mod.Version)
	}

	index, err := os.ReadFile(filepath.Join(mod.Dir, "api.md"))
	if err != nil {
		return pinnedSDK{}, fmt.Errorf("plan 51 anchors citations on the SDK's api.md, and %s ships none: %w", mod.Version, err)
	}
	for _, m := range indexLink.FindAllStringSubmatch(string(index), -1) {
		if count[m[1]] == 1 && atRoot[m[1]] {
			sdk.linked = m[1]
			break
		}
	}
	if sdk.linked == "" {
		return pinnedSDK{}, fmt.Errorf("api.md at %s links no method its root declares once", mod.Version)
	}

	f, err := os.Open(filepath.Join(mod.Dir, SpecPath))
	if err != nil {
		return pinnedSDK{}, fmt.Errorf("the SDK at %s bundles no spec: %w", mod.Version, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return pinnedSDK{}, err
	}
	var doc struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		return pinnedSDK{}, fmt.Errorf("decoding %s: %w", SpecPath, err)
	}
	var schemas []string
	for name := range doc.Components.Schemas {
		if symbolRe.MatchString(name) && !strings.Contains(name, ".") {
			schemas = append(schemas, name)
		}
	}
	if len(schemas) == 0 {
		return pinnedSDK{}, fmt.Errorf("%s at %s carries no schema a path could name", SpecPath, mod.Version)
	}
	sort.Strings(schemas)
	sdk.schema = schemas[0]
	return sdk, nil
}

// TestTheRungsRunAgainstTheRealPin points the resolving rungs at the module the
// build graph actually holds, rather than at a fixture.
//
// The fixture tests prove the machinery; this proves the machinery is pointed at
// the SDK. Every subject is read from the pin, so the test says the same thing at
// the next tag as at this one.
func TestTheRungsRunAgainstTheRealPin(t *testing.T) {
	env, err := NewEnv(repoRoot(t))
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	sdk := realSDK(t)
	t.Logf("pin %s: %s %s, api.md %s, spec %s", env.Pin, sdk.file, sdk.method, sdk.linked, sdk.schema)
	head := func(form string) string { return form + " anthropic-sdk-go " + env.Pin + " — " }

	for _, tc := range []struct {
		name  string
		in    string
		rules []string
		why   string
	}{
		{
			name: "a symbol the SDK declares",
			in:   head("checked against") + sdk.file + " " + sdk.method,
			why:  "a method its file declares resolves at the tag it was read at",
		},
		{
			name:  "a symbol the file never declared, claimed present",
			in:    head("checked against") + sdk.file + " " + sdk.absent,
			rules: []string{"vanished-at-stamp"},
			why: "the file is still shipped, so only a symbol anchor can tell a deletion " +
				"from a line that merely moved — plan 51's central evidence",
		},
		{
			name: "the same symbol, claimed absent, which is the truth",
			in:   head("absent at") + sdk.file + " " + sdk.absent,
			why:  "the negative polarity is the form that makes a deletion recordable",
		},
		{
			name: "a method the API index links",
			in:   head("checked against") + "api.md " + sdk.linked,
			why: "plan 51 anchors its api.md citations on the symbols the index links, " +
				"whose coordinates drifted five times while the symbols never moved",
		},
		{
			name: "a schema path into the bundled spec",
			in:   head("checked against") + "spec components.schemas." + sdk.schema,
			why:  "the spec carries the constraints the generated Go types drop",
		},
		{
			name:  "a schema path the spec does not carry",
			in:    head("checked against") + "spec components.schemas." + sdk.absent,
			rules: []string{"vanished-at-stamp"},
			why:   "a spec citation is resolved, not assumed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseCitation(tc.in)
			if c == nil {
				t.Fatalf("ParseCitation(%q) = nil", tc.in)
			}
			var got []string
			for _, f := range env.Resolution([]Citation{*c}) {
				got = append(got, f.Rule)
			}
			if strings.Join(got, ",") != strings.Join(tc.rules, ",") {
				t.Errorf("Resolution(%q) rules = %v, want %v\n(%s)", tc.in, got, tc.rules, tc.why)
			}
		})
	}
}

// TestTheBumpReportRunsAgainstTheRealPin drives rung 3 over the real module the
// way a bump would: citations stamped at an older tag, resolved against the pin.
// The transition is constructed, not recorded — its symbol is one the pin's file
// never declared — because what this proves is that the report reads the pinned
// tree; the rules it applies are the fixture tests'. The stamp is the zero
// version because a symbol anchor's stamp is never opened, only compared with
// the pin, and no release of the SDK can come before it.
func TestTheBumpReportRunsAgainstTheRealPin(t *testing.T) {
	env, err := NewEnv(repoRoot(t))
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	sdk := realSDK(t)
	cs := citations(t,
		// Gone at the pin: exactly what a bump report is for.
		"checked against anthropic-sdk-go v0.0.0 — "+sdk.file+" "+sdk.absent,
		// Still there, so it should appear as lag and nothing else.
		"checked against anthropic-sdk-go v0.0.0 — "+sdk.file+" "+sdk.method,
	)
	rep := env.Bump(cs)

	if len(rep.Vanished) != 1 {
		t.Fatalf("Vanished = %v, want exactly the symbol the pin does not declare", rep.Vanished)
	}
	if !strings.Contains(rep.Vanished[0].Msg, sdk.absent) {
		t.Errorf("Vanished names %q, want %s", rep.Vanished[0].Msg, sdk.absent)
	}
	if len(rep.Lag) != 1 || !strings.Contains(rep.Lag[0], "v0.0.0") {
		t.Errorf("Lag = %v, want one group stamped v0.0.0 behind the pin", rep.Lag)
	}
	if rep.Read != 2 {
		t.Errorf("Read = %d, want 2: a report that does not say how much it read cannot "+
			"be told from one that read nothing", rep.Read)
	}
	t.Logf("bump report over two real citations:\n%s", rep)
}

// TestCachedModuleFindsThePinInTheRealCache exercises the production path
// behind decision 3's "reported when available". Every other test injects a
// stub cache, which is what lets the two branches be tested at all — and also
// what would let a wrong path spelling degrade to "not unpacked" forever,
// indistinguishable from a genuinely cold cache. The pin is the one version the
// cache is guaranteed to hold, because `make verify` builds before it runs.
func TestCachedModuleFindsThePinInTheRealCache(t *testing.T) {
	mod, err := Module(repoRoot(t), SDKModule)
	if err != nil {
		t.Fatalf("resolving %s offline: %v", SDKModule, err)
	}
	if mod.Replaced != "" {
		t.Skipf("%s is replaced by %s, so the build graph's directory is not a module "+
			"cache entry and this cannot be compared", SDKModule, mod.Replaced)
	}
	dir, err := CachedModule(SDKModule, mod.Version)
	if err != nil {
		t.Fatalf("CachedModule(%s, %s): %v\n\nThe pinned module is in the build graph, so "+
			"it is unpacked. A failure here means the cache path this builds is wrong, and "+
			"every older-tag lookup would report \"not unpacked\" whatever the cache held.",
			SDKModule, mod.Version, err)
	}
	if dir != mod.Dir {
		t.Errorf("CachedModule = %q, go list -m = %q: the two must name the same tree, or "+
			"rung 3 falsifies spans against a different one", dir, mod.Dir)
	}
}

// TestEscapeModulePath pins the module cache's case encoding against a path
// that actually needs it. This repository's own module path is the example the
// repo already documents: the owner's mixed case must survive exactly.
func TestEscapeModulePath(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/anthropics/anthropic-sdk-go":         "github.com/anthropics/anthropic-sdk-go",
		"github.com/go-jose/go-jose/v4":                  "github.com/go-jose/go-jose/v4",
		"github.com/OpenSDLC-Dev/managed-agent-platform": "github.com/!open!s!d!l!c-!dev/managed-agent-platform",
	} {
		if got := escapeModulePath(in); got != want {
			t.Errorf("escapeModulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// typeDecl is a type declared at column zero, outside any group.
var typeDecl = regexp.MustCompile(`^type ([A-Z]\w*) struct\b`)

// TestADocCommentBelongsToWhatItDocuments, against the real pin. Plan 51
// measured a share of its resolvable coordinates landing on a doc-comment line
// rather than on the declaration itself, and the registry cites those sentences
// by name. A span over one of them has to find the declaration it documents, or
// rung 3 would tell the migrator the range had drifted off something when it is
// pointing exactly where it meant to.
func TestADocCommentBelongsToWhatItDocuments(t *testing.T) {
	sdk := realSDK(t)
	r, err := NewResolver(sdk.Dir)
	if err != nil {
		t.Fatal(err)
	}
	// The doc comment is found by reading the text, not written down: a literal
	// line number or type name is what the next bump would move, and finding them
	// with the parser under test would agree with it rather than check it.
	for _, file := range sdk.roots {
		src, err := os.ReadFile(filepath.Join(sdk.Dir, file))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		for i, l := range lines {
			m := typeDecl.FindStringSubmatch(l)
			if m == nil || i < 2 || !strings.HasPrefix(lines[i-1], "//") || !strings.HasPrefix(lines[i-2], "//") {
				continue
			}
			from, to := i-1, i // 1-based: the two comment lines straight above the keyword
			decls, err := r.DeclsIn(file, from, to)
			if err != nil {
				t.Fatalf("DeclsIn over the doc comment: %v", err)
			}
			if len(decls) != 1 || decls[0].Name != m[1] {
				t.Fatalf("DeclsIn(%s:%d-%d) at %s = %+v, want %s — the declaration those "+
					"lines document", file, from, to, sdk.Version, decls, m[1])
			}
			return
		}
	}
	t.Fatalf("no root file of %s at %s has a type with a two-line doc comment, so this "+
		"cannot test attribution", SDKModule, sdk.Version)
}
