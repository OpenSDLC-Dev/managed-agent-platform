package toolset_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// grepFixture writes the tree the parameter tests search, through bash so one
// call lays out the whole tree — in a subshell, so the persistent shell keeps
// no cd or option of it.
func grepFixture(t *testing.T, r toolset.Runner) {
	t.Helper()
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gp/code/util gp/code/web gp/many gp/sp gp/z gp/hid/.hd gp/hid/.git gp/hid/node_modules gp/loc && cd gp && `+
		`printf '1\\n2\\n3\\nX\\n5\\n6\\n7\\nX\\n9\\n' > ctx.txt && `+
		`printf 'Needle\\nNEEDLE\\nneedle\\n' > case.txt && `+
		`printf 'one\\nfoo start\\nmiddle\\nend bar\\nfive\\nsix foo x bar\\nseven\\n' > ml.txt && `+
		`printf 'foo\\nbar\\0' > mlbin.dat && `+
		`printf 'foo\\nbar\\0baz\\nfoo\\n' > bin.dat && `+
		`for f in code/main.go code/main_test.go code/util/util.go code/readme.md code/mainXgo `+
		`code/web/app.ts code/web/app.tsx code/web/app.js; do echo 'x needle' > $f; done && `+
		`for i in 1 2 3 4 5; do echo needle > many/f$i.txt; done && `+
		`echo needle > 'sp/foo bar.txt' && echo needle > sp/foo && echo needle > 'sp/a.ts,b.js' && echo needle > sp/a.ts && `+
		`printf 'foo foo\\nbar\\n' > z/f.txt && printf 'a\\n\\nb\\n\\n' > z/e.txt && printf 'abc' > z/eof.txt && `+
		`for f in a.txt .h.go .env .hd/x.txt .git/z.txt node_modules/m.txt; do echo needle > hid/$f; done && `+
		`echo needle > loc/a.go && echo needle > loc/ö.go && seq 1 200000 > seq.txt)"}`)
}

// lines splits a multi-file result and sorts it: find walks a directory in its
// own order, which no caller may rely on.
func lines(s string) []string {
	out := strings.Split(s, "\n")
	slices.Sort(out)
	return out
}

func sameSet(t *testing.T, input, got string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if g := lines(got); !slices.Equal(g, want) {
		t.Fatalf("grep(%s) =\n%s\nwant (in any order)\n%s", input, strings.Join(g, "\n"), strings.Join(want, "\n"))
	}
}

func exactly(t *testing.T, r toolset.Runner, input, want string) {
	t.Helper()
	if got := ok(t, r, "grep", input); got != want {
		t.Fatalf("grep(%s) =\n%s\nwant\n%s", input, got, want)
	}
}

// TestGrepParameters pins each of the twelve properties the recorded reference
// adds to grep (#827), with the meaning its schema gives them, against the
// default sandbox image's own GNU grep and find. Every expected answer is
// ripgrep 14.1.1's for the same search (rg run from /workspace, the workdir).
func TestGrepParameters(t *testing.T) {
	r := runner(t)
	grepFixture(t, r)
	const code = "/workspace/gp/code/"

	t.Run("output_mode", func(t *testing.T) {
		in := `{"pattern":"needle","path":"gp/code/util"}`
		exactly(t, r, in, code+"util/util.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"files_with_matches"}`, code+"util/util.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"content"}`, code+"util/util.go:1:x needle")
		// count is per file and leaves out the files with none — a single
		// file's zero too.
		exactly(t, r, `{"pattern":"^X$","path":"gp","output_mode":"count"}`, "/workspace/gp/ctx.txt:2")
		exactly(t, r, `{"pattern":"zzz","path":"gp/case.txt","output_mode":"count"}`, "no matches")
		fails(t, r, "grep", `{"pattern":"needle","output_mode":"lines"}`, "output_mode")
	})

	t.Run("-n", func(t *testing.T) {
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"content","-n":true}`, code+"util/util.go:1:x needle")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"content","-n":false}`, code+"util/util.go:x needle")
		// Ignored outside content mode, as its description says.
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","-n":false}`, code+"util/util.go")
	})

	t.Run("-i", func(t *testing.T) {
		exactly(t, r, `{"pattern":"needle","path":"gp/case.txt","output_mode":"count"}`, "1")
		exactly(t, r, `{"pattern":"needle","path":"gp/case.txt","output_mode":"count","-i":true}`, "3")
		exactly(t, r, `{"pattern":"needle","path":"gp/case.txt","output_mode":"content","-i":false}`, "3:needle")
	})

	// A single file is searched without a file name on its lines, as rg and
	// grep both print it; context lines take "-" where matches take ":", and
	// "--" separates groups that are not adjacent.
	t.Run("-A, -B, -C and context", func(t *testing.T) {
		const base = `{"pattern":"X","path":"gp/ctx.txt","output_mode":"content",`
		exactly(t, r, base+`"-A":1}`, "4:X\n5-5\n--\n8:X\n9-9")
		exactly(t, r, base+`"-B":1}`, "3-3\n4:X\n--\n7-7\n8:X")
		exactly(t, r, base+`"-C":1}`, "3-3\n4:X\n5-5\n--\n7-7\n8:X\n9-9")
		exactly(t, r, base+`"context":1}`, "3-3\n4:X\n5-5\n--\n7-7\n8:X\n9-9")
		// -A and -B override the context for their side, whichever order.
		exactly(t, r, base+`"-C":1,"-A":3}`, "3-3\n4:X\n5-5\n6-6\n7-7\n8:X\n9-9")
		exactly(t, r, base+`"-B":0,"context":1}`, "4:X\n5-5\n--\n8:X\n9-9")
		// context and -C are one option; given both, context wins.
		exactly(t, r, base+`"-C":0,"context":2}`, "2-2\n3-3\n4:X\n5-5\n6-6\n7-7\n8:X\n9-9")
		// A zero count prints no group separators.
		exactly(t, r, base+`"-C":0}`, "4:X\n8:X")
		// Groups in different files are separated as groups in one are: three
		// groups, two separators, none before the first.
		got := ok(t, r, "grep", `{"pattern":"X|foo start","path":"gp","glob":"{ctx,ml}.txt","output_mode":"content","-A":1}`)
		if strings.Count(got, "\n--\n") != 2 || strings.HasPrefix(got, "--") || len(strings.Split(got, "\n")) != 8 {
			t.Fatalf("context over two files = %q", got)
		}
		// Ignored outside content mode — not even validated, as the recorded
		// descriptions say ("Requires output_mode: content, ignored otherwise").
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","-C":2}`, "/workspace/gp/ctx.txt")
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","output_mode":"count","-A":-1,"context":1.5}`, "2")
		fails(t, r, "grep", base+`"-A":-1}`, "-A must be a whole number")
		fails(t, r, "grep", base+`"context":1.5}`, "context must be a whole number")
		fails(t, r, "grep", base+`"-B":"2"}`, "invalid grep input")
	})

	t.Run("head_limit and offset", func(t *testing.T) {
		const base = `{"pattern":".","path":"gp/ctx.txt","output_mode":"content"`
		exactly(t, r, base+`}`, "1:1\n2:2\n3:3\n4:X\n5:5\n6:6\n7:7\n8:X\n9:9")
		exactly(t, r, base+`,"head_limit":0}`, "1:1\n2:2\n3:3\n4:X\n5:5\n6:6\n7:7\n8:X\n9:9")
		exactly(t, r, base+`,"head_limit":3}`, "1:1\n2:2\n3:3")
		exactly(t, r, base+`,"offset":7}`, "8:X\n9:9")
		exactly(t, r, base+`,"head_limit":2,"offset":3}`, "4:X\n5:5")
		exactly(t, r, base+`,"offset":9}`, "no matches")
		// Context lines and separators are lines like any other: "| head -N".
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","output_mode":"content","-A":1,"head_limit":3}`, "4:X\n5-5\n--")
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","output_mode":"content","-A":1,"head_limit":2,"offset":2}`, "--\n8:X")

		// Paging a file list: the pages are disjoint and together are the list.
		var all []string
		for _, page := range []string{`"head_limit":2`, `"head_limit":2,"offset":2`, `"offset":4`} {
			got := strings.Split(ok(t, r, "grep", `{"pattern":"needle","path":"gp/many",`+page+`}`), "\n")
			all = append(all, got...)
		}
		sameSet(t, "three pages", strings.Join(all, "\n"), "/workspace/gp/many/f1.txt", "/workspace/gp/many/f2.txt",
			"/workspace/gp/many/f3.txt", "/workspace/gp/many/f4.txt", "/workspace/gp/many/f5.txt")
		if got := ok(t, r, "grep", `{"pattern":"needle","path":"gp/many","output_mode":"count","head_limit":3}`); len(strings.Split(got, "\n")) != 3 ||
			strings.Count(got, ":1") != 3 {
			t.Fatalf("count head_limit 3 = %q, want three file:1 entries", got)
		}
		fails(t, r, "grep", base+`,"head_limit":-2}`, "head_limit must be a whole number")
		fails(t, r, "grep", base+`,"offset":1e12}`, "offset must be a whole number")
	})

	t.Run("type", func(t *testing.T) {
		in := `{"pattern":"needle","path":"gp/code","type":"go"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"main.go", code+"main_test.go", code+"util/util.go")
		in = `{"pattern":"needle","path":"gp/code","type":"ts"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"web/app.ts", code+"web/app.tsx")
		fails(t, r, "grep", `{"pattern":"needle","path":"gp/code","type":"golang"}`, `unrecognized file type "golang"`)
		// A file named outright is searched whatever its type, as rg does.
		exactly(t, r, `{"pattern":"needle","path":"gp/code/readme.md","type":"go"}`, code+"readme.md")
	})

	t.Run("glob", func(t *testing.T) {
		for _, tc := range []struct {
			path, glob string
			want       []string
		}{
			// No slash: a base name at any depth. The dot is literal.
			{"gp/code", "*.go", []string{"main.go", "main_test.go", "util/util.go"}},
			{"gp/code", "main.go", []string{"main.go"}},
			{"gp/code", "*.{ts,tsx}", []string{"web/app.ts", "web/app.tsx"}},
			// A slash anchors the glob at the workdir — rg's working directory
			// — not at the search root; ** spans directories.
			{"gp/code", "gp/code/util/*.go", []string{"util/util.go"}},
			{"gp/code", "/gp/code/main.go", []string{"main.go"}},
			{"gp/code", "**/util/*.go", []string{"util/util.go"}},
			{"gp", "gp/code/**/*.go", []string{"main.go", "main_test.go", "util/util.go"}},
			// A negated glob excludes, and prunes a directory it names.
			{"gp/code", "!*_test.go", []string{"main.go", "util/util.go", "readme.md", "mainXgo", "web/app.ts", "web/app.tsx", "web/app.js"}},
			{"gp/code", "!{util,web}", []string{"main.go", "main_test.go", "readme.md", "mainXgo"}},
			{"gp/code", "[!m]*.go", []string{"util/util.go"}},
		} {
			in := `{"pattern":"needle","path":"` + tc.path + `","glob":"` + tc.glob + `"}`
			var want []string
			for _, w := range tc.want {
				want = append(want, code+w)
			}
			sameSet(t, in, ok(t, r, "grep", in), want...)
		}
		// The value is one glob, whole: a space or a comma outside braces is
		// part of it, as it is to rg --glob.
		exactly(t, r, `{"pattern":"needle","path":"gp/sp","glob":"foo bar.txt"}`, "/workspace/gp/sp/foo bar.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/sp","glob":"*.ts,*.js"}`, "/workspace/gp/sp/a.ts,b.js")
		// An anchored glob is anchored at the workdir: there is no util
		// directory there.
		exactly(t, r, `{"pattern":"needle","path":"gp/code","glob":"util/*.go"}`, "no matches")
		// A glob decides over the type when it names files positively; a
		// negated one narrows what the type admits.
		in := `{"pattern":"needle","path":"gp/code","type":"go","glob":"!*_test.go"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"main.go", code+"util/util.go")
		in = `{"pattern":"needle","path":"gp/code","type":"go","glob":"*.md"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"readme.md")
		// globset's errors, in its words.
		fails(t, r, "grep", `{"pattern":"needle","glob":"[abc"}`, "unclosed character class")
		fails(t, r, "grep", `{"pattern":"needle","glob":"{a,{b}}"}`, "nested alternate groups are not allowed")
		fails(t, r, "grep", `{"pattern":"needle","glob":"[z-a]x"}`, "error parsing glob '[z-a]x': invalid range; 'z' > 'a'")
		// "!" alone ignores every file, as rg reads it.
		exactly(t, r, `{"pattern":"needle","path":"gp/code","glob":"!"}`, "no matches")
		// A file named outright is searched whatever the glob says.
		exactly(t, r, `{"pattern":"needle","path":"gp/code/readme.md","glob":"*.go"}`, code+"readme.md")
		// ? and a class match a byte, as rg's globs do: ö is two.
		exactly(t, r, `{"pattern":"needle","path":"gp/loc","glob":"?.go"}`, "/workspace/gp/loc/a.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/loc","glob":"??.go"}`, "/workspace/gp/loc/ö.go")
	})

	// rg skips hidden files and directories below the root unless a glob or a
	// type admits them, and searches a hidden path it is handed. It honours
	// .gitignore, which this does not; node_modules is pruned in its stead.
	t.Run("hidden files", func(t *testing.T) {
		const hid = "/workspace/gp/hid/"
		exactly(t, r, `{"pattern":"needle","path":"gp/hid"}`, hid+"a.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid","glob":"*.go"}`, hid+".h.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid","type":"go"}`, hid+".h.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid","glob":"!*.go"}`, hid+"a.txt")
		in := `{"pattern":"needle","path":"gp/hid","glob":"*"}`
		sameSet(t, in, ok(t, r, "grep", in), hid+"a.txt", hid+".h.go", hid+".env", hid+".hd/x.txt", hid+".git/z.txt", hid+"node_modules/m.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid/.hd"}`, hid+".hd/x.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid/.env"}`, hid+".env")
	})

	t.Run("binary files", func(t *testing.T) {
		// Over a directory a binary file is skipped; named outright it is
		// searched, and its content answer is rg's notice.
		exactly(t, r, `{"pattern":"baz","path":"gp","output_mode":"content"}`, "no matches")
		exactly(t, r, `{"pattern":"foo","path":"gp/bin.dat","output_mode":"content"}`, `binary file matches (found "\0" byte around offset 7)`)
		exactly(t, r, `{"pattern":"foo","path":"gp/bin.dat","output_mode":"count"}`, "2")
		exactly(t, r, `{"pattern":"foo","path":"gp/bin.dat"}`, "/workspace/gp/bin.dat")
		exactly(t, r, `{"pattern":"zzz","path":"gp/bin.dat","output_mode":"content"}`, "no matches")
		exactly(t, r, `{"pattern":"o\\nb","path":"gp/bin.dat","output_mode":"content","multiline":true}`, `binary file matches (found "\0" byte around offset 7)`)
	})

	t.Run("multiline", func(t *testing.T) {
		const base = `{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"content"`
		// Without it a match stays on one line.
		exactly(t, r, base+`}`, "6:six foo x bar")
		// With it . crosses newlines, and every line a match spans prints.
		exactly(t, r, base+`,"multiline":true}`, "2:foo start\n3:middle\n4:end bar\n6:six foo x bar")
		exactly(t, r, base+`,"multiline":true,"-n":false}`, "foo start\nmiddle\nend bar\nsix foo x bar")
		exactly(t, r, base+`,"multiline":true,"-A":1}`, "2:foo start\n3:middle\n4:end bar\n5-five\n6:six foo x bar\n7-seven")
		exactly(t, r, base+`,"multiline":true,"-B":1,"head_limit":2,"offset":1}`, "2:foo start\n3:middle")
		// ^ and $ still match at every line, and a match's closing newline
		// does not pull in the line after it.
		exactly(t, r, `{"pattern":"^middle\\nend","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "3:middle\n4:end bar")
		exactly(t, r, `{"pattern":"bar\\n","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "4:end bar\n6:six foo x bar")
		// A newline character in the pattern is a newline, as rg -U reads it;
		// without multiline it is rg's refusal.
		exactly(t, r, `{"pattern":"middle\nend","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "3:middle\n4:end bar")
		exactly(t, r, `{"pattern":"\\Qmiddle\nend\\E","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "3:middle\n4:end bar")
		fails(t, r, "grep", `{"pattern":"middle\nend","path":"gp/ml.txt"}`, `the literal "\n" is not allowed in a regex`)
		// A leading start-of-pattern item stays first.
		exactly(t, r, `{"pattern":"(*UCP)middle\\n","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "3:middle")
		// count is rg -U -c: the matches, so two on one line are two...
		exactly(t, r, `{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"count","multiline":true}`, "2")
		exactly(t, r, `{"pattern":"FOO.*?BAR","path":"gp/ml.txt","output_mode":"count","multiline":true,"-i":true}`, "2")
		exactly(t, r, `{"pattern":"fo.","path":"gp/z/f.txt","output_mode":"count","multiline":true}`, "2")
		// ...but a pattern that cannot match a newline is searched line by
		// line, as rg searches it, and counts lines.
		exactly(t, r, `{"pattern":"o","path":"gp/ml.txt","output_mode":"count","multiline":true}`, "3")
		exactly(t, r, `{"pattern":"foo|bar","path":"gp/z/f.txt","output_mode":"count","multiline":true}`, "2")
		// An empty match counts and prints as rg's do, in every mode alike...
		exactly(t, r, `{"pattern":"^\\s*$","path":"gp/z/e.txt","output_mode":"content","multiline":true}`, "2:\n4:")
		exactly(t, r, `{"pattern":"^\\s*$","path":"gp/z/e.txt","output_mode":"count","multiline":true}`, "2")
		exactly(t, r, `{"pattern":"^\\s*$","path":"gp/z","multiline":true}`, "/workspace/gp/z/e.txt")
		exactly(t, r, `{"pattern":"\\n*","path":"gp/z/e.txt","output_mode":"count","multiline":true}`, "3")
		exactly(t, r, `{"pattern":"\\n*","path":"gp/z/e.txt","output_mode":"content","multiline":true}`, "1:a\n2:\n3:b\n4:")
		exactly(t, r, `{"pattern":"x\\n|","path":"gp/z/f.txt","output_mode":"count","multiline":true}`, "12")
		// ...save one at the very end of a file: rg prints the last line it
		// ends, and neither counts nor lists the file for it.
		exactly(t, r, `{"pattern":"foo\\n|\\z","path":"gp/z/eof.txt","output_mode":"content","multiline":true}`, "1:abc")
		exactly(t, r, `{"pattern":"foo\\n|\\z","path":"gp/z/eof.txt","output_mode":"count","multiline":true}`, "no matches")
		exactly(t, r, `{"pattern":"foo\\n|\\z","path":"gp/z","multiline":true}`, "/workspace/gp/z/f.txt")
		exactly(t, r, `{"pattern":"foo\\n|\\z","path":"gp/z/f.txt","output_mode":"content","multiline":true,"-B":1}`, "1:foo foo\n2-bar")
		exactly(t, r, `{"pattern":"^$","path":"gp/z/f.txt","output_mode":"content","multiline":true,"-B":1}`, "2-bar")
		// A file list pages like any other.
		if got := ok(t, r, "grep", `{"pattern":"needle\\n","path":"gp/many","multiline":true,"head_limit":2}`); len(strings.Split(got, "\n")) != 2 {
			t.Fatalf("multiline file list, head_limit 2 = %q", got)
		}
		// Over a directory each line carries its file, and a binary file —
		// which grep's -z mode would otherwise read as text — is skipped in
		// every mode: mlbin.dat holds foo\nbar too.
		in := `{"pattern":"foo.*?bar","path":"gp","multiline":true}`
		sameSet(t, in, ok(t, r, "grep", in), "/workspace/gp/ml.txt", "/workspace/gp/z/f.txt")
		exactly(t, r, `{"pattern":"start\\nmiddle","path":"gp","output_mode":"content","multiline":true}`,
			"/workspace/gp/ml.txt:2:foo start\n/workspace/gp/ml.txt:3:middle")
		exactly(t, r, `{"pattern":"start\\nmiddle","path":"gp","output_mode":"count","multiline":true}`, "/workspace/gp/ml.txt:1")
		exactly(t, r, `{"pattern":"^foo\\nbar","path":"gp","output_mode":"content","multiline":true}`, "no matches")
		exactly(t, r, `{"pattern":"^foo\\nbar","path":"gp","output_mode":"count","multiline":true}`, "no matches")
	})

	// Every new value reaches the sandbox as data. A type is looked up, never
	// passed; a glob becomes a quoted regex; neither is ever run.
	t.Run("type and glob are data, not code", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"needle","type":"go; touch /tmp/pwned-type"}`, "unrecognized file type")
		ok(t, r, "grep", `{"pattern":"needle","path":"gp","glob":"$(touch /tmp/pwned-glob)*.go"}`)
		ok(t, r, "grep", `{"pattern":"needle","path":"gp","glob":"';touch /tmp/pwned-quote;'"}`)
		if out := ok(t, r, "bash", `{"command":"ls /tmp/pwned-* 2>/dev/null; echo checked"}`); strings.TrimSpace(out) != "checked" {
			t.Fatalf("a grep property was executed: %q", out)
		}
	})
}

// Groups printed by different xargs batches — each its own grep — are
// separated as grep separates them within one, and paging counts those
// separators too. 1,500 files with 200-character names take three batches,
// and the fifteen that match fall across them.
func TestGrepContextAcrossBatches(t *testing.T) {
	r := runner(t)
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gb && cd gb && n=$(printf '%0200d' 0) && for i in $(seq 1 1500); do `+
		`if [ $((i % 100)) = 0 ]; then printf 'X\\ny\\n'; else printf 'y\\n'; fi > f${i}_$n; done)"}`)
	for _, ml := range []string{"false", "true"} {
		pattern := "X"
		if ml == "true" {
			pattern = `X\\n`
		}
		got := ok(t, r, "grep", `{"pattern":"`+pattern+`","path":"gb","output_mode":"content","-A":1,"multiline":`+ml+`}`)
		out := strings.Split(got, "\n")
		if len(out) != 3*15-1 || out[0] == "--" || strings.Count(got, "\n--\n") != 14 {
			t.Fatalf("multiline=%s: %d lines, first %q, %d separators; want 44 lines, 14 separators between them",
				ml, len(out), out[0], strings.Count(got, "\n--\n"))
		}
		page := ok(t, r, "grep", `{"pattern":"`+pattern+`","path":"gb","output_mode":"content","-A":1,"multiline":`+ml+`,"offset":3,"head_limit":3}`)
		if want := strings.Join(out[3:6], "\n"); page != want {
			t.Fatalf("multiline=%s: page = %q, want %q", ml, page, want)
		}
	}
}

// head_limit stops the search once the page is full, rather than reading
// every file to throw the rest away: with every file matching, a search for
// the first line runs a batch's grep, or two where the first finished before
// the pager could say so, where the whole search runs six. A grep earlier on
// the PATH logs each run, and a batch's names its files.
func TestGrepHeadLimitStopsTheSearch(t *testing.T) {
	r := runner(t)
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gh && cd gh && n=$(printf '%0200d' 0) && for i in $(seq 1 3000); do echo needle > f${i}_$n; done && `+
		`printf '#!/bin/bash\\necho \"$*\" >> /tmp/greps\\nexec /usr/bin/grep \"$@\"\\n' > /usr/local/bin/grep && chmod +x /usr/local/bin/grep)"}`)
	runs := func(input string) int {
		t.Helper()
		ok(t, r, "bash", `{"command":"rm -f /tmp/greps"}`)
		ok(t, r, "grep", input)
		n, err := strconv.Atoi(strings.TrimSpace(ok(t, r, "bash", `{"command":"grep -c gh/f /tmp/greps"}`)))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, mode := range []string{"files_with_matches", "count", "content"} {
		all := runs(`{"pattern":"needle","path":"gh","output_mode":"` + mode + `"}`)
		first := runs(`{"pattern":"needle","path":"gh","output_mode":"` + mode + `","head_limit":1}`)
		if all < 5 || first > 2 {
			t.Errorf("%s: %d batches for the whole search, %d for head_limit 1; want at least 5 and at most 2", mode, all, first)
		}
	}
	// With one file matching — the first find lists, so the first batch's —
	// the batch that finds it writes nothing more, so nothing upstream dies
	// of SIGPIPE; the pager's word stops the next batch instead, so at most
	// one more runs rather than all the rest.
	match := strings.TrimSpace(ok(t, r, "bash", `{"command":"f=$(find /workspace/gh -type f | head -n1) && echo haystack > \"$f\" && echo \"$f\""}`))
	ok(t, r, "bash", `{"command":"rm -f /tmp/greps"}`)
	exactly(t, r, `{"pattern":"haystack","path":"gh","head_limit":1}`, match)
	if n := runs(`{"pattern":"haystack","path":"gh","head_limit":1}`); n > 2 {
		t.Errorf("%d batches ran for a page the first batch filled", n)
	}
}

// A failure anywhere in the search is a tool error, never a short answer or
// "no matches": find's, grep's on a file, and the regex engine's on a
// pattern too costly for it — whose match before the failure must not pass
// for the answer. Without CAP_DAC_OVERRIDE root cannot read a mode-000
// directory or file, so this sandbox drops it.
func TestGrepSurfacesFailures(t *testing.T) {
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("toolset tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  sandbox.Hardening{CapDrop: []string{"DAC_OVERRIDE", "DAC_READ_SEARCH"}},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gf/dir/locked gf/file && echo needle > gf/dir/a.txt && echo needle > gf/dir/locked/b.txt && `+
		`chmod 000 gf/dir/locked && echo needle > gf/file/a.txt && echo needle > gf/file/b.txt && chmod 000 gf/file/b.txt && `+
		`mkdir -p gf/bt && printf 'xb\\n%s d\\n' $(head -c 30000 /dev/zero | tr '\\0' a) > gf/bt/bt.txt)"}`)

	for _, mode := range []string{"files_with_matches", "count", "content"} {
		fails(t, r, "grep", `{"pattern":"needle","path":"gf/dir","output_mode":"`+mode+`"}`, "Permission denied")
		fails(t, r, "grep", `{"pattern":"needle","path":"gf/dir","output_mode":"`+mode+`","head_limit":5}`, "Permission denied")
		fails(t, r, "grep", `{"pattern":"needle","path":"gf/file","output_mode":"`+mode+`"}`, "Permission denied")
		fails(t, r, "grep", `{"pattern":"needle\\n","path":"gf/dir","output_mode":"`+mode+`","multiline":true}`, "Permission denied")
	}
	for _, mode := range []string{"count", "content"} {
		for _, pattern := range []string{`xb|(?:(?:a|a)+)+c`, `xb\\n|(?:(?:a|a)+)+c`} {
			for _, path := range []string{"gf/bt/bt.txt", "gf/bt"} {
				in := `{"pattern":"` + pattern + `","path":"` + path + `","output_mode":"` + mode + `","multiline":true}`
				msg := fails(t, r, "grep", in, "exceeded PCRE's backtracking limit")
				if strings.Contains(msg, "xargs:") {
					t.Errorf("grep(%s) error carries xargs's own line: %q", in, msg)
				}
			}
		}
	}
}

// A relative workdir makes every path relative, and a root beginning with "-"
// must reach find as a path, never as an expression: "-delete" read as one
// would delete the tree it was meant to search.
func TestGrepRelativeWorkdir(t *testing.T) {
	base := runner(t)
	ok(t, base, "bash", `{"command":"mkdir -p -- -delete/sub && echo needle > -delete/sub/f.txt && echo needle > -delete/g.go"}`)
	r := toolset.Runner{Sandbox: base.Sandbox, Session: base.Session, Workdir: "-delete"}
	in := `{"pattern":"needle"}`
	sameSet(t, in, ok(t, r, "grep", in), "./-delete/g.go", "./-delete/sub/f.txt")
	exactly(t, r, `{"pattern":"needle","output_mode":"content","glob":"sub/*"}`, "./-delete/sub/f.txt:1:needle")
	exactly(t, r, `{"pattern":"needle","path":"sub","output_mode":"count"}`, "./-delete/sub/f.txt:1")
	if got := strings.TrimSpace(ok(t, base, "bash", `{"command":"find ./-delete -type f | sort"}`)); got != "./-delete/g.go\n./-delete/sub/f.txt" {
		t.Fatalf("the tree after the search = %q", got)
	}
}

// TestGrepRunsTheSameInAKubernetesPod runs one call of each mode on the
// Kubernetes backend and holds its answer to the Docker backend's: the search
// is one bash script either way, handed to /bin/bash -c by both backends' exec
// wrapper. A missing cluster is a hard failure, as with the k8s contract test;
// point it at a local one with MAP_K8S_CONTEXT.
func TestGrepRunsTheSameInAKubernetesPod(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      testImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	pod := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
	docker := runner(t)
	grepFixture(t, pod)
	grepFixture(t, docker)

	for in, wantErr := range map[string]bool{
		`{"pattern":"needle","path":"gp/code","glob":"!*_test.go","type":"go"}`:                                  false,
		`{"pattern":"needle","path":"gp/code","type":"ts","output_mode":"count"}`:                                false,
		`{"pattern":"x","path":"gp/ctx.txt","output_mode":"content","-i":true,"-C":1,"head_limit":5,"offset":1}`: false,
		`{"pattern":"foo.*?bar","path":"gp","output_mode":"content","multiline":true,"-A":1}`:                    false,
		`{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"count","multiline":true,"head_limit":1}`:       false,
		`{"pattern":"needle","path":"gp/many","output_mode":"count"}`:                                            false,
		`{"pattern":"needle","path":"gp/hid","glob":"*"}`:                                                        false,
		// Cut off by the pager, so every stage upstream dies of SIGPIPE: the
		// pod's processes must take it as Docker's do, not as a failed write.
		`{"pattern":"1","path":"gp/seq.txt","output_mode":"content","head_limit":2}`:                     false,
		`{"pattern":"1\\n","path":"gp/seq.txt","output_mode":"content","multiline":true,"head_limit":2}`: false,
		`{"pattern":"1","path":"gp","output_mode":"count","head_limit":1,"glob":"seq.txt"}`:              false,
		`{"pattern":"[unclosed","path":"gp"}`:                                                            true,
	} {
		p, d := call(t, pod, "grep", in), call(t, docker, "grep", in)
		if d.IsError != wantErr || d.Content == "no matches" {
			t.Fatalf("grep(%s) on docker = %+v, a fixture that no longer exercises the search", in, d)
		}
		if p.IsError != d.IsError || !slices.Equal(lines(p.Content), lines(d.Content)) {
			t.Errorf("grep(%s) differs:\nk8s    (is_error=%v): %q\ndocker (is_error=%v): %q", in, p.IsError, p.Content, d.IsError, d.Content)
		}
	}
}
