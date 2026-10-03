package toolset_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// grepFixture writes the tree the parameter tests search, through bash so one
// call lays out the whole tree — in a subshell, so the persistent shell keeps
// no cd or option of it.
func grepFixture(t *testing.T, r toolset.Runner) {
	t.Helper()
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gp/code/util gp/code/web gp/many && cd gp && `+
		`printf '1\\n2\\n3\\nX\\n5\\n6\\n7\\nX\\n9\\n' > ctx.txt && `+
		`printf 'Needle\\nNEEDLE\\nneedle\\n' > case.txt && `+
		`printf 'one\\nfoo start\\nmiddle\\nend bar\\nfive\\nsix foo x bar\\nseven\\n' > ml.txt && `+
		`printf 'foo\\nbar\\0' > mlbin.dat && `+
		`for f in code/main.go code/main_test.go code/util/util.go code/readme.md code/mainXgo `+
		`code/web/app.ts code/web/app.tsx code/web/app.js; do echo 'x needle' > $f; done && `+
		`for i in 1 2 3 4 5; do echo needle > many/f$i.txt; done)"}`)
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
// default sandbox image's own GNU grep and find.
func TestGrepParameters(t *testing.T) {
	r := runner(t)
	grepFixture(t, r)
	const code = "/workspace/gp/code/"

	t.Run("output_mode", func(t *testing.T) {
		in := `{"pattern":"needle","path":"gp/code/util"}`
		exactly(t, r, in, code+"util/util.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"files_with_matches"}`, code+"util/util.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"content"}`, code+"util/util.go:1:x needle")
		// count is per file and leaves out the files with none.
		exactly(t, r, `{"pattern":"^X$","path":"gp","output_mode":"count"}`, "/workspace/gp/ctx.txt:2")
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
		// Ignored outside content mode.
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","-C":2}`, "/workspace/gp/ctx.txt")
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
			// Several globs, split at commas outside braces and at spaces.
			{"gp/code", "*.ts,*.js", []string{"web/app.ts", "web/app.js"}},
			{"gp/code", "*.ts *.js", []string{"web/app.ts", "web/app.js"}},
			// A slash anchors the glob at the search root; ** spans directories.
			{"gp/code", "util/*.go", []string{"util/util.go"}},
			{"gp/code", "/main.go", []string{"main.go"}},
			{"gp/code", "**/util/*.go", []string{"util/util.go"}},
			{"gp", "code/**/*.go", []string{"main.go", "main_test.go", "util/util.go"}},
			// The last glob a path matches decides it, rg's override order.
			{"gp/code", "*.go !*_test.go", []string{"main.go", "util/util.go"}},
			{"gp/code", "!*_test.go *.go", []string{"main.go", "main_test.go", "util/util.go"}},
			// A negated glob alone excludes, and prunes a directory it names.
			{"gp/code", "!util !web", []string{"main.go", "main_test.go", "readme.md", "mainXgo"}},
			{"gp/code", "[!m]*.go", []string{"util/util.go"}},
		} {
			in := `{"pattern":"needle","path":"` + tc.path + `","glob":"` + tc.glob + `"}`
			var want []string
			for _, w := range tc.want {
				want = append(want, code+w)
			}
			sameSet(t, in, ok(t, r, "grep", in), want...)
		}
		// An anchored glob is anchored: gp has no util directory of its own.
		exactly(t, r, `{"pattern":"needle","path":"gp","glob":"util/*.go"}`, "no matches")
		// A glob decides over the type when it names files positively; a
		// negated one narrows what the type admits.
		in := `{"pattern":"needle","path":"gp/code","type":"go","glob":"!*_test.go"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"main.go", code+"util/util.go")
		in = `{"pattern":"needle","path":"gp/code","type":"go","glob":"*.md"}`
		sameSet(t, in, ok(t, r, "grep", in), code+"readme.md")
		fails(t, r, "grep", `{"pattern":"needle","glob":"[abc"}`, "unclosed [ class")
		fails(t, r, "grep", `{"pattern":"needle","glob":"{a,{b}}"}`, "nests")
		fails(t, r, "grep", `{"pattern":"needle","glob":"!"}`, "matches no path")
		// A file named outright is searched whatever the glob says.
		exactly(t, r, `{"pattern":"needle","path":"gp/code/readme.md","glob":"*.go"}`, code+"readme.md")
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
		// count counts the matches, a match spanning lines once.
		exactly(t, r, `{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"count","multiline":true}`, "2")
		exactly(t, r, `{"pattern":"FOO.*?BAR","path":"gp/ml.txt","output_mode":"count","multiline":true,"-i":true}`, "2")
		// Two matches on one line count once, as rg -U -c counts them.
		exactly(t, r, `{"pattern":"o","path":"gp/ml.txt","output_mode":"count","multiline":true}`, "3")
		// A file list pages like any other.
		if got := ok(t, r, "grep", `{"pattern":"needle","path":"gp/many","multiline":true,"head_limit":2}`); len(strings.Split(got, "\n")) != 2 {
			t.Fatalf("multiline file list, head_limit 2 = %q", got)
		}
		// Over a directory each line carries its file, and a binary file —
		// which grep's -z mode would otherwise read as text — is skipped.
		exactly(t, r, `{"pattern":"foo.*?bar","path":"gp","multiline":true}`, "/workspace/gp/ml.txt")
		exactly(t, r, `{"pattern":"start\\nmiddle","path":"gp","output_mode":"content","multiline":true}`,
			"/workspace/gp/ml.txt:2:foo start\n/workspace/gp/ml.txt:3:middle")
		exactly(t, r, `{"pattern":"start\\nmiddle","path":"gp","output_mode":"count","multiline":true}`, "/workspace/gp/ml.txt:1")
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
		`{"pattern":"needle","path":"gp/code","glob":"*.go !*_test.go"}`:                                         false,
		`{"pattern":"needle","path":"gp/code","type":"ts","output_mode":"count"}`:                                false,
		`{"pattern":"x","path":"gp/ctx.txt","output_mode":"content","-i":true,"-C":1,"head_limit":5,"offset":1}`: false,
		`{"pattern":"foo.*?bar","path":"gp","output_mode":"content","multiline":true,"-A":1}`:                    false,
		`{"pattern":"[unclosed","path":"gp"}`:                                                                    true,
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
