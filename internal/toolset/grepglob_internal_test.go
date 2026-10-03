package toolset

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// compileGlob is held to the paths each glob must and must not match, by Go's
// regexp — which reads these POSIX EREs the same way, as they use nothing
// beyond groups, alternation, bracket expressions and stars. Each row is
// ripgrep 14.1.1's own answer, run as `rg -l -g GLOB` from the directory the
// paths are relative to (the anchor "/r/" stands for it).
func TestCompileGlobMatchesAsRipgrep(t *testing.T) {
	for _, tc := range []struct {
		glob     string
		match    []string
		mismatch []string
	}{
		{"*.go", []string{"/r/a.go", "/r/x/y/b.go", "/r/.h.go"}, []string{"/r/a.goo", "/r/ago", "/r/a.go/x"}},
		{"a?c", []string{"/r/abc", "/r/d/axc"}, []string{"/r/a/c", "/r/abbc"}},
		// The value is one glob: a space and a comma are part of it.
		{"foo bar.txt", []string{"/r/foo bar.txt", "/r/d/foo bar.txt"}, []string{"/r/foo", "/r/bar.txt"}},
		{"*.ts,*.js", []string{"/r/a.ts,b.js"}, []string{"/r/a.ts", "/r/a.js"}},
		// A trailing space is trimmed, as gitignore trims it; an escaped one
		// and a leading one are not.
		{"*.go ", []string{"/r/a.go"}, []string{"/r/a.go "}},
		{`a\ `, []string{"/r/a "}, []string{"/r/a"}},
		{" a", []string{"/r/ a"}, []string{"/r/a"}},
		// A slash anywhere anchors the glob at the working directory.
		{"src/*.go", []string{"/r/src/a.go"}, []string{"/r/src/x/a.go", "/r/y/src/a.go"}},
		{"/a.go", []string{"/r/a.go"}, []string{"/r/x/a.go"}},
		{"**/b/*.go", []string{"/r/b/a.go", "/r/x/y/b/a.go"}, []string{"/r/b/x/a.go"}},
		{"a/**/z", []string{"/r/a/z", "/r/a/b/c/z"}, []string{"/r/az", "/r/x/a/z"}},
		{"a/**/**/**/z", []string{"/r/a/z", "/r/a/b/c/z"}, []string{"/r/az"}},
		// a/** matches what is inside a, not a itself.
		{"a/**", []string{"/r/a/b", "/r/a/b/c"}, []string{"/r/a", "/r/ab"}},
		{"**", []string{"/r/a", "/r/a/b"}, nil},
		// ** inside a component is two stars.
		{"a**.go", []string{"/r/ab.go", "/r/x/a.go"}, []string{"/r/a/b.go"}},
		{"*.{ts,tsx}", []string{"/r/a.ts", "/r/a.tsx"}, []string{"/r/a.t", "/r/a.js"}},
		{"{src,lib}/**/*.go", []string{"/r/src/a.go", "/r/lib/x/a.go"}, []string{"/r/bin/a.go"}},
		// An empty alternative is dropped, as globset drops it; a stray } is
		// dropped too.
		{"{,a}.go", []string{"/r/a.go"}, []string{"/r/.go"}},
		{"a{}.go", []string{"/r/a.go"}, nil},
		{"a}.go", []string{"/r/a.go"}, []string{"/r/a}.go"}},
		{"[ab]x", []string{"/r/ax", "/r/bx"}, []string{"/r/cx"}},
		{"[!a]x", []string{"/r/bx"}, []string{"/r/ax"}},
		{"[^a]x", []string{"/r/bx"}, []string{"/r/ax"}},
		{"[!]a]x", []string{"/r/bx"}, []string{"/r/]x", "/r/ax"}},
		{"[]]x", []string{"/r/]x"}, []string{"/r/ax"}},
		{"[]-]x", []string{"/r/]x", "/r/-x"}, []string{"/r/ax"}},
		{"[a-]x", []string{"/r/ax", "/r/-x"}, []string{"/r/bx"}},
		{"[--/]x", []string{"/r/-x", "/r/.x"}, []string{"/r/ax"}},
		{"[[]x", []string{"/r/[x"}, []string{"/r/ax"}},
		{"[!^]x", []string{"/r/ax"}, []string{"/r/^x"}},
		{"[^-]x", []string{"/r/ax"}, []string{"/r/-x"}},
		// A class, negated or not, can match the separator: globset's can.
		{"sub[!x]b", []string{"/r/sub/b"}, []string{"/r/subxb"}},
		{"sub[/]b", []string{"/r/sub/b"}, nil},
		{`\*.go`, []string{"/r/*.go"}, []string{"/r/a.go"}},
		{`\!x`, []string{"/r/!x"}, []string{"/r/x"}},
		{"a+b(c)|$^.go", []string{"/r/a+b(c)|$^.go"}, []string{"/r/aab(c)|$^.go"}},
	} {
		rule, err := compileGlob(tc.glob, "/r/")
		if err != nil || rule == nil {
			t.Fatalf("compileGlob(%q) = %v, %v", tc.glob, rule, err)
		}
		re := regexp.MustCompile(rule.re)
		for _, p := range tc.match {
			if !re.MatchString(p) {
				t.Errorf("glob %q (%s) does not match %q", tc.glob, rule.re, p)
			}
		}
		for _, p := range tc.mismatch {
			if re.MatchString(p) {
				t.Errorf("glob %q (%s) matches %q", tc.glob, rule.re, p)
			}
		}
	}
}

// A class is a class of bytes, as globset's (?-u) regex makes it: [ö] holds
// ö's two bytes, and ? is one byte — find matches these under the C locale,
// which Go's regexp, reading UTF-8, cannot stand in for (TestGrepParameters
// runs them).
func TestCompileGlobIsBytewise(t *testing.T) {
	for glob, want := range map[string]string{"[ö]": "^/r/(/?|.*/)[\xb6\xc3]$", "?.go": `^/r/(/?|.*/)[^/]\.go$`} {
		if rule, err := compileGlob(glob, "/r/"); err != nil || rule.re != want {
			t.Errorf("compileGlob(%q) = %+v, %v, want re %q", glob, rule, err, want)
		}
	}
}

func TestCompileGlobLineRules(t *testing.T) {
	rule, err := compileGlob("!build/", "/")
	if err != nil || !rule.negate || !rule.dirOnly {
		t.Fatalf("!build/ = %+v, %v", rule, err)
	}
	// A root of "/" anchors without doubling its slash.
	if want := "^/(/?|.*/)build$"; rule.re != want {
		t.Errorf("re = %q, want %q", rule.re, want)
	}
	// "!" alone ignores everything (an empty glob gains **/, and ** is .*);
	// "/" alone matches nothing; a comment or a blank is no glob at all.
	for glob, want := range map[string]string{"!": "^/r/.*$", "/": "^/r/$"} {
		if rule, err := compileGlob(glob, "/r/"); err != nil || rule.re != want {
			t.Errorf("compileGlob(%q) = %+v, %v, want re %q", glob, rule, err, want)
		}
	}
	for _, glob := range []string{"", "  ", "#x", "# *.go"} {
		if rule, err := compileGlob(glob, "/r/"); err != nil || rule != nil {
			t.Errorf("compileGlob(%q) = %+v, %v, want no glob", glob, rule, err)
		}
	}
	// globset's own error messages, as rg prints them.
	for glob, want := range map[string]string{
		"[z-a]x":  `error parsing glob '[z-a]x': invalid range; 'z' > 'a'`,
		"[abc":    `error parsing glob '[abc': unclosed character class; missing ']'`,
		"[]":      `error parsing glob '[]': unclosed character class; missing ']'`,
		"[!]":     `error parsing glob '[!]': unclosed character class; missing ']'`,
		"{a,b":    `error parsing glob '{a,b': unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)`,
		"{a,{b}}": `error parsing glob '{a,{b}}': nested alternate groups are not allowed`,
		`a\`:      `error parsing glob 'a\': dangling '\'`,
	} {
		if _, err := compileGlob(glob, "/r/"); err == nil || err.Error() != want {
			t.Errorf("compileGlob(%q) error = %v, want %q", glob, err, want)
		}
	}
}

// globAnchor anchors at the workdir, rg's working directory, wherever the root
// lies under it, and nowhere for a root outside it. Its prefix is the
// workdir as find prints it, which is quoted where it has metacharacters.
func TestGlobAnchor(t *testing.T) {
	for _, tc := range []struct{ root, workdir, want string }{
		{"/workspace", "/workspace", "/workspace/"},
		{"/workspace/gp/code", "/workspace", "/workspace/"},
		{"/workspace2/x", "/workspace", ""},
		{"/etc", "/workspace", ""},
		{"/x", "/", "/"},
		{"/w.x/a+b", "/w.x/a+b", `/w\.x/a\+b/`},
		{"ws/-delete", "ws", `\./ws/`},
		{"ws", "ws", `\./ws/`},
		{"gp", ".", `\./`},
		{"/abs", "ws", ""},
	} {
		if got := globAnchor(tc.root, tc.workdir); got != tc.want {
			t.Errorf("globAnchor(%q, %q) = %q, want %q", tc.root, tc.workdir, got, tc.want)
		}
	}
	for p, want := range map[string]string{"/a": "/a", ".": ".", "-delete": "./-delete", "ws/x": "./ws/x"} {
		if got := findPath(p); got != want {
			t.Errorf("findPath(%q) = %q, want %q", p, got, want)
		}
	}
}

// selection's tests read as rg's override, type and hidden rules once find
// evaluates them; TestGrepParameters runs them against find itself. Here the
// shape is pinned.
func TestSelection(t *testing.T) {
	hidden := []string{"-name", ".*", "-o", "-name", "node_modules"}
	sel, prune := selection(nil, nil)
	if want := []string{"!", "-name", ".*"}; !slices.Equal(sel, want) || !slices.Equal(prune, hidden) {
		t.Errorf("no filter: sel %q prune %q", sel, prune)
	}
	sel, _ = selection([]string{"*.go", "*.mod"}, nil)
	if want := []string{"-name", "*.go", "-o", "-name", "*.mod"}; !slices.Equal(sel, want) {
		t.Errorf("type: sel %q, want %q", sel, want)
	}
	sel, prune = selection([]string{"*.go"}, &globRule{re: "P"})
	if want := []string{"-regex", "P"}; !slices.Equal(sel, want) {
		t.Errorf("positive glob: sel %q, want %q", sel, want)
	}
	if want := append([]string{"!", "-regex", "P", "("}, append(hidden, ")")...); !slices.Equal(prune, want) {
		t.Errorf("positive glob: prune %q, want %q", prune, want)
	}
	sel, prune = selection([]string{"*.go"}, &globRule{re: "N", negate: true})
	if want := []string{"!", "-regex", "N", "(", "-name", "*.go", ")"}; !slices.Equal(sel, want) {
		t.Errorf("negated glob: sel %q, want %q", sel, want)
	}
	if want := append([]string{"-regex", "N", "-o", "("}, append(hidden, ")")...); !slices.Equal(prune, want) {
		t.Errorf("negated glob: prune %q, want %q", prune, want)
	}
	// A directory-only glob decides directories alone: a positive one leaves
	// no file in, a negated one leaves the files to the hidden rule.
	if sel, _ = selection(nil, &globRule{re: "D", dirOnly: true}); !slices.Equal(sel, []string{"-false"}) {
		t.Errorf("dir-only positive: sel %q", sel)
	}
	if sel, _ = selection(nil, &globRule{re: "D", negate: true, dirOnly: true}); !slices.Equal(sel, []string{"!", "-name", ".*"}) {
		t.Errorf("dir-only negated: sel %q", sel)
	}
}

// bracket's members are placed where a bracket expression reads them as
// members, and the result matches exactly the bytes of the class.
func TestBracketMatchesItsBytes(t *testing.T) {
	for _, tc := range []struct {
		neg    bool
		ranges [][2]rune
	}{
		{false, [][2]rune{{']', ']'}}},
		{false, [][2]rune{{'^', '^'}}},
		{false, [][2]rune{{'^', '^'}, {'-', '-'}}},
		{true, [][2]rune{{'^', '^'}}},
		{false, [][2]rune{{'[', '['}, {':', ':'}}},
		{false, [][2]rune{{'!', ']'}}},
		{true, [][2]rune{{']', ']'}, {'-', '-'}, {'[', '['}, {'^', '^'}, {'a', 'z'}}},
		{false, [][2]rune{{'a', 'c'}, {'x', 'x'}, {'y', 'y'}}},
	} {
		got := bracket(tc.neg, tc.ranges)
		// A backslash in a POSIX bracket is a member; Go's regexp reads it as
		// an escape, so it is doubled for the check.
		goRE := got
		if strings.HasPrefix(goRE, "[") {
			goRE = strings.ReplaceAll(goRE, `\`, `\\`)
		}
		re := regexp.MustCompile("^" + goRE + "$")
		for b := 1; b < 128; b++ {
			in := false
			for _, r := range tc.ranges {
				in = in || (rune(b) >= r[0] && rune(b) <= r[1])
			}
			if re.MatchString(string(rune(b))) != (in != tc.neg) {
				t.Errorf("bracket(%v, %q) = %s, and %q is wrongly placed", tc.neg, tc.ranges, got, rune(b))
			}
		}
	}
}

func TestMultilinePatterns(t *testing.T) {
	// Leading start-of-pattern items stay first.
	ml, ok := multilinePatterns(`(*UCP)(*CRLF)foo\n`)
	if !ok || !strings.HasPrefix(ml.first, `(*UCP)(*CRLF)(?sm)foo\n`) || !strings.HasPrefix(ml.scan, `(*UCP)(*CRLF)(*NOTEMPTY)(?sm)`) {
		t.Errorf("start items: %+v", ml)
	}
	// A newline character becomes \n, inside a quote too; an open quote is
	// closed so the wrappers' groups close.
	for in, want := range map[string]string{
		"a\nb":        `a\nb`,
		"\\Qa\nb\\E.": `\Qa\E\n\Qb\E.`,
		"\\Qa.b":      `\Qa.b\E`,
		"a\\\nb":      `a\nb`,
		`a\\`:         `a\\`,
	} {
		if got := escapeNewlines(in); got != want {
			t.Errorf("escapeNewlines(%q) = %q, want %q", in, got, want)
		}
	}
	// rg reads a pattern that cannot match a newline line by line, and so
	// does the search: no multiline patterns.
	for pattern, want := range map[string]bool{
		"foo|bar": false, "x*": false, `\w+\d`: false, `\bfoo\b`: false, "[a-z]+": false, `\p{L}+`: false,
		`[^\n]+`: false, `a\.b`: false, `\Q.\E`: false,
		"fo.": true, `\s`: true, `a\nb`: true, "[^a]": true, "^$": true, "a$": true, `\Afoo`: true, `\z`: true,
		`\W`: true, `\D`: true, `[\s]`: true, `[\x00-\x7f]`: true, "[[:space:]]": true, `\p{Cc}`: true,
		`(a)\1`: true, `\x0a`: true, "a\nb": true, `\Qa` + "\n": true, "[\t-\r]": true,
	} {
		if _, got := multilinePatterns(pattern); got != want {
			t.Errorf("multiline search for %q = %v, want %v", pattern, got, want)
		}
	}
	// endCaret reads ^ as rg does at the very end, and leaves a ^ that is not
	// an anchor alone.
	for in, want := range map[string]string{
		"^a":      `(?:^|(?<=\n)\z)a`,
		`\^[^a]^`: `\^[^a](?:^|(?<=\n)\z)`,
		`(?^i)a`:  `(?^i)a`,
		`\Q^\E^`:  `\Q^\E(?:^|(?<=\n)\z)`,
		`(?#^)^`:  `(?#^)(?:^|(?<=\n)\z)`,
	} {
		if got := endCaret(in); got != want {
			t.Errorf("endCaret(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every sandbox tool's schema declares exactly the properties its tool
// decodes, so the unknown-property gate (unknownProperties, which reads the
// schema) and the decoder cannot drift apart: a property added to one alone
// fails here.
func TestSchemaPropertiesAreWhatEachToolDecodes(t *testing.T) {
	inputs := map[string]any{
		"bash": bashInput{}, "read": readInput{}, "write": writeInput{}, "edit": editInput{},
		"glob": searchInput{}, "grep": grepInput{},
	}
	for _, d := range definitions {
		if d.web {
			continue
		}
		input, ok := inputs[d.name]
		if !ok {
			t.Errorf("sandbox tool %q has no decoded input type registered here", d.name)
			continue
		}
		var tags []string
		typ := reflect.TypeOf(input)
		for i := 0; i < typ.NumField(); i++ {
			tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if tag == "" || tag == "-" && !strings.Contains(typ.Field(i).Tag.Get("json"), ",") {
				t.Errorf("%s: field %s carries no JSON name", d.name, typ.Field(i).Name)
				continue
			}
			tags = append(tags, tag)
		}
		var props []string
		for k := range d.props {
			props = append(props, k)
		}
		slices.Sort(tags)
		slices.Sort(props)
		if !slices.Equal(tags, props) {
			t.Errorf("%s: schema properties %q, decoded fields %q", d.name, props, tags)
		}
		delete(inputs, d.name)
	}
	for name := range inputs {
		t.Errorf("%s: no sandbox tool definition", name)
	}
}
