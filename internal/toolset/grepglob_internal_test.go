package toolset

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestSplitGlobs(t *testing.T) {
	for in, want := range map[string][]string{
		"*.go":                {"*.go"},
		"*.{ts,tsx}":          {"*.{ts,tsx}"},
		"*.ts,*.tsx":          {"*.ts", "*.tsx"},
		" *.ts \t*.js ":       {"*.ts", "*.js"},
		"*.{ts,tsx},,*.js,":   {"*.{ts,tsx}", "*.js"},
		"src/**/*.go !*_x.go": {"src/**/*.go", "!*_x.go"},
		"":                    nil,
	} {
		if got := splitGlobs(in); !slices.Equal(got, want) {
			t.Errorf("splitGlobs(%q) = %q, want %q", in, got, want)
		}
	}
}

// globERE is held to the paths each glob must and must not match, by Go's
// regexp — which reads these POSIX EREs the same way, as they use nothing
// beyond groups, alternation, classes and stars.
func TestCompileGlobsMatch(t *testing.T) {
	for _, tc := range []struct {
		glob     string
		match    []string
		mismatch []string
	}{
		{"*.go", []string{"/r/a.go", "/r/x/y/b.go", "/r/.h.go"}, []string{"/r/a.goo", "/r/ago", "/r/a.go/x"}},
		{"a?c", []string{"/r/abc", "/r/d/axc"}, []string{"/r/a/c", "/r/abbc"}},
		{"src/*.go", []string{"/r/src/a.go"}, []string{"/r/src/x/a.go", "/r/y/src/a.go"}},
		{"/a.go", []string{"/r/a.go"}, []string{"/r/x/a.go"}},
		{"**/b/*.go", []string{"/r/b/a.go", "/r/x/y/b/a.go"}, []string{"/r/b/x/a.go"}},
		{"a/**/z", []string{"/r/a/z", "/r/a/b/c/z"}, []string{"/r/az", "/r/x/a/z"}},
		{"a/**/**/**/z", []string{"/r/a/z", "/r/a/b/c/z"}, []string{"/r/az"}},
		{"a/**", []string{"/r/a/b", "/r/a/b/c"}, []string{"/r/a", "/r/ab"}},
		{"**", []string{"/r/a", "/r/a/b"}, nil},
		{"a**.go", []string{"/r/ab.go", "/r/x/a.go"}, []string{"/r/a/b.go"}},
		{"*.{ts,tsx}", []string{"/r/a.ts", "/r/a.tsx"}, []string{"/r/a.t", "/r/a.js"}},
		{"{src,lib}/**/*.go", []string{"/r/src/a.go", "/r/lib/x/a.go"}, []string{"/r/bin/a.go"}},
		{"[ab]x", []string{"/r/ax", "/r/bx"}, []string{"/r/cx"}},
		{"[!a]x", []string{"/r/bx"}, []string{"/r/ax"}},
		{"[^a]x", []string{"/r/bx"}, []string{"/r/ax"}},
		{"[!]a]x", []string{"/r/bx"}, []string{"/r/]x", "/r/ax"}},
		{"[]]x", []string{"/r/]x"}, []string{"/r/ax"}},
		{`\*.go`, []string{"/r/*.go"}, []string{"/r/a.go"}},
		{"a+b(c)|$^.go", []string{"/r/a+b(c)|$^.go"}, []string{"/r/aab(c)|$^.go"}},
	} {
		rules, err := compileGlobs("/r", tc.glob)
		if err != nil || len(rules) != 1 {
			t.Fatalf("compileGlobs(%q) = %v, %v", tc.glob, rules, err)
		}
		re := regexp.MustCompile(rules[0].re)
		for _, p := range tc.match {
			if !re.MatchString(p) {
				t.Errorf("glob %q (%s) does not match %q", tc.glob, rules[0].re, p)
			}
		}
		for _, p := range tc.mismatch {
			if re.MatchString(p) {
				t.Errorf("glob %q (%s) matches %q", tc.glob, rules[0].re, p)
			}
		}
	}
	// A run of **/ compiles to one group.
	if rules, _ := compileGlobs("/r", "**/**/**/x"); strings.Count(rules[0].re, "(.*/)?") != 1 {
		t.Errorf("**/**/**/x = %s, want one (.*/)? group", rules[0].re)
	}
	// A negated class never matches the separator.
	rules, _ := compileGlobs("/r", "a[!x]b")
	if regexp.MustCompile(rules[0].re).MatchString("/r/a/b") {
		t.Errorf("a[!x]b matched across a slash: %s", rules[0].re)
	}
}

func TestCompileGlobsRootAndFlags(t *testing.T) {
	rules, err := compileGlobs("/", "!build/ *.go")
	if err != nil {
		t.Fatal(err)
	}
	if !rules[0].negate || !rules[0].dirOnly || rules[1].negate || rules[1].dirOnly {
		t.Fatalf("flags = %+v", rules)
	}
	// The root "/" anchors without doubling its slash, and a root's own
	// metacharacters are quoted.
	if want := "^/(.*/)?build$"; rules[0].re != want {
		t.Errorf("re = %q, want %q", rules[0].re, want)
	}
	rules, _ = compileGlobs("/w.x/a+b", "*.go")
	if !strings.HasPrefix(rules[0].re, `^/w\.x/a\+b/`) {
		t.Errorf("re = %q, want the root quoted", rules[0].re)
	}
	for glob, want := range map[string]string{
		"[abc": "unclosed [ class", "{a,b": "unclosed { alternation", "{a,{b}}": "nests",
		`a\`: "lone backslash", "!": "matches no path", "/": "matches no path",
	} {
		if _, err := compileGlobs("/r", glob); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("compileGlobs(%q) error = %v, want %q", glob, err, want)
		}
	}
}

// selection's tests read as rg's override rules once find evaluates them; the
// toolset tests run them against find itself. Here the shape is pinned.
func TestSelection(t *testing.T) {
	sel, prune := selection(nil, nil)
	if !slices.Equal(sel, []string{"-true"}) || prune != nil {
		t.Errorf("no filter: sel %q prune %q", sel, prune)
	}
	sel, _ = selection([]string{"*.go", "*.mod"}, nil)
	if want := []string{"-name", "*.go", "-o", "-name", "*.mod"}; !slices.Equal(sel, want) {
		t.Errorf("type: sel %q, want %q", sel, want)
	}
	pos, neg := globRule{re: "P"}, globRule{re: "N", negate: true}
	sel, prune = selection([]string{"*.go"}, []globRule{pos, neg})
	if want := []string{"!", "-regex", "N", "(", "-regex", "P", "-o", "(", "-false", ")", ")"}; !slices.Equal(sel, want) {
		t.Errorf("globs: sel %q, want %q", sel, want)
	}
	if want := []string{"-o", "(", "-regex", "N", "-o", "(", "!", "-regex", "P", "(", "-false", ")", ")", ")"}; !slices.Equal(prune, want) {
		t.Errorf("globs: prune %q, want %q", prune, want)
	}
	// A directory-only glob prunes but never selects a file; a negated glob
	// alone leaves the type in charge.
	sel, _ = selection([]string{"*.go"}, []globRule{{re: "D", negate: true, dirOnly: true}})
	if want := []string{"-name", "*.go"}; !slices.Equal(sel, want) {
		t.Errorf("dir-only: sel %q, want %q", sel, want)
	}
}
