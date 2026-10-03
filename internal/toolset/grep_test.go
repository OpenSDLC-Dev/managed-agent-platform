package toolset_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/ripgrep"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/docker"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox/k8s"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
)

// muslImage is a sandbox image with no glibc at all — Alpine's musl and
// busybox, and the /bin/bash the image contract asks for — which a
// dynamically linked rg could not run in. It is the image the store suites'
// Postgres fixture already pulls.
const muslImage = "postgres:16-alpine"

// grepFixture writes the tree the parameter tests search, through bash so one
// call lays out the whole tree — in a subshell, so the persistent shell keeps
// no cd or option of it. It needs nothing beyond a busybox userland.
func grepFixture(t *testing.T, r toolset.Runner) {
	t.Helper()
	ok(t, r, "bash", `{"command":"(set -e; mkdir -p gp/code/util gp/code/web gp/many gp/sp gp/hid/.hd gp/repo/.git gp/repo/vendor && cd gp && `+
		`printf '1\\n2\\n3\\nX\\n5\\n6\\n7\\nX\\n9\\n' > ctx.txt && `+
		`printf 'Needle\\nNEEDLE\\nneedle\\n' > case.txt && `+
		`printf 'one\\nfoo start\\nmiddle\\nend bar\\nfive\\nsix foo x bar\\nseven\\n' > ml.txt && `+
		`printf 'foo\\nbar\\0baz\\nfoo\\n' > bin.dat && `+
		`for f in code/main.go code/main_test.go code/util/util.go code/readme.md `+
		`code/web/app.ts code/web/app.tsx code/web/app.js; do echo 'x needle' > $f; done && `+
		`for i in 1 2 3 4 5; do echo needle > many/f$i.txt; done && `+
		`echo needle > 'sp/foo bar.txt' && echo needle > sp/foo && echo needle > 'sp/a.ts,b.js' && echo needle > sp/a.ts && `+
		`for f in a.txt .h.go .hd/x.txt; do echo needle > hid/$f; done && `+
		`printf 'vendor/\\n*.log\\n' > repo/.gitignore && echo needle > repo/vendor/dep.go && echo needle > repo/run.log && echo needle > repo/main.go && `+
		`i=0; while [ $i -lt 20000 ]; do i=$((i+1)); echo $i; done > seq.txt)"}`)
}

func exactly(t *testing.T, r toolset.Runner, input, want string) {
	t.Helper()
	if got := ok(t, r, "grep", input); got != want {
		t.Fatalf("grep(%s) =\n%s\nwant\n%s", input, got, want)
	}
}

// under joins names onto a directory, one per line: a file list as rg prints
// it, sorted by path.
func under(dir string, names ...string) string {
	for i, n := range names {
		names[i] = dir + n
	}
	return strings.Join(names, "\n")
}

// TestGrepParameters pins how each of the twelve properties the recorded
// reference adds to grep (#827) reaches ripgrep, by the flag its description
// names, and the shape of what comes back. The answers are rg's own; what is
// ours is the mapping, the paging and the text around them.
func TestGrepParameters(t *testing.T) {
	r := runner(t)
	grepFixture(t, r)
	const code = "/workspace/gp/code/"

	t.Run("output_mode", func(t *testing.T) {
		// files_with_matches is the default: rg -l, sorted by path.
		exactly(t, r, `{"pattern":"needle","path":"gp/code"}`,
			under(code, "main.go", "main_test.go", "readme.md", "util/util.go", "web/app.js", "web/app.ts", "web/app.tsx"))
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"files_with_matches"}`, code+"util/util.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/code/util","output_mode":"content"}`, code+"util/util.go:1:x needle")
		// count is rg -c: per file, leaving out the files with none.
		exactly(t, r, `{"pattern":"^X$","path":"gp","output_mode":"count"}`, "/workspace/gp/ctx.txt:2")
		// A single file is searched without its name on the line, as rg
		// prints it, and a count of nothing is no answer at all.
		exactly(t, r, `{"pattern":"X","path":"gp/ctx.txt","output_mode":"count"}`, "2")
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

	// Context lines take "-" where matches take ":", and "--" separates
	// groups that are not adjacent.
	t.Run("-A, -B, -C and context", func(t *testing.T) {
		const base = `{"pattern":"X","path":"gp/ctx.txt","output_mode":"content",`
		exactly(t, r, base+`"-A":1}`, "4:X\n5-5\n--\n8:X\n9-9")
		exactly(t, r, base+`"-B":1}`, "3-3\n4:X\n--\n7-7\n8:X")
		exactly(t, r, base+`"-C":1}`, "3-3\n4:X\n5-5\n--\n7-7\n8:X\n9-9")
		exactly(t, r, base+`"context":1}`, "3-3\n4:X\n5-5\n--\n7-7\n8:X\n9-9")
		// -A and -B override the context for their side, as rg's do.
		exactly(t, r, base+`"-C":1,"-A":3}`, "3-3\n4:X\n5-5\n6-6\n7-7\n8:X\n9-9")
		exactly(t, r, base+`"-B":0,"context":1}`, "4:X\n5-5\n--\n8:X\n9-9")
		// context and -C are one option; given both, context wins.
		exactly(t, r, base+`"-C":0,"context":2}`, "2-2\n3-3\n4:X\n5-5\n6-6\n7-7\n8:X\n9-9")
		exactly(t, r, base+`"-C":0}`, "4:X\n8:X")
		// Groups in different files are separated as groups in one are.
		exactly(t, r, `{"pattern":"X|foo start","path":"gp","glob":"{ctx,ml}.txt","output_mode":"content","-A":1}`,
			"/workspace/gp/ctx.txt:4:X\n/workspace/gp/ctx.txt-5-5\n--\n/workspace/gp/ctx.txt:8:X\n/workspace/gp/ctx.txt-9-9\n--\n"+
				"/workspace/gp/ml.txt:2:foo start\n/workspace/gp/ml.txt-3-middle")
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

		// Paging a file list: the order is the path's, so the pages are
		// disjoint and together are the list, call after call.
		var all []string
		for _, page := range []string{`"head_limit":2`, `"head_limit":2,"offset":2`, `"offset":4`} {
			all = append(all, ok(t, r, "grep", `{"pattern":"needle","path":"gp/many",`+page+`}`))
		}
		if want := under("/workspace/gp/many/", "f1.txt", "f2.txt", "f3.txt", "f4.txt", "f5.txt"); strings.Join(all, "\n") != want {
			t.Fatalf("three pages =\n%s\nwant\n%s", strings.Join(all, "\n"), want)
		}
		exactly(t, r, `{"pattern":"needle","path":"gp/many","output_mode":"count","head_limit":3}`,
			"/workspace/gp/many/f1.txt:1\n/workspace/gp/many/f2.txt:1\n/workspace/gp/many/f3.txt:1")
		// head stops rg once its page is full, and a page deep into a large
		// answer is cut in the sandbox, never carried out whole.
		exactly(t, r, `{"pattern":"^1","path":"gp/seq.txt","output_mode":"content","head_limit":2,"offset":11109}`,
			"19998:19998\n19999:19999")
		fails(t, r, "grep", base+`,"head_limit":-2}`, "head_limit must be a whole number")
		fails(t, r, "grep", base+`,"offset":1e12}`, "offset must be a whole number")
	})

	t.Run("type", func(t *testing.T) {
		exactly(t, r, `{"pattern":"needle","path":"gp/code","type":"go"}`, under(code, "main.go", "main_test.go", "util/util.go"))
		exactly(t, r, `{"pattern":"needle","path":"gp/code","type":"ts"}`, under(code, "web/app.ts", "web/app.tsx"))
		fails(t, r, "grep", `{"pattern":"needle","path":"gp/code","type":"golang"}`, "unrecognized file type: golang")
		// A file named outright is searched whatever its type, as rg does.
		exactly(t, r, `{"pattern":"needle","path":"gp/code/readme.md","type":"go"}`, code+"readme.md")
	})

	t.Run("glob", func(t *testing.T) {
		exactly(t, r, `{"pattern":"needle","path":"gp/code","glob":"*.go"}`, under(code, "main.go", "main_test.go", "util/util.go"))
		exactly(t, r, `{"pattern":"needle","path":"gp/code","glob":"*.{ts,tsx}"}`, under(code, "web/app.ts", "web/app.tsx"))
		exactly(t, r, `{"pattern":"needle","path":"gp/code","glob":"!*_test.go","type":"go"}`, under(code, "main.go", "util/util.go"))
		// The value is one glob, whole: a space or a comma outside braces is
		// part of it, as it is to rg --glob.
		exactly(t, r, `{"pattern":"needle","path":"gp/sp","glob":"foo bar.txt"}`, "/workspace/gp/sp/foo bar.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/sp","glob":"*.ts,*.js"}`, "/workspace/gp/sp/a.ts,b.js")
		// globset's errors, in rg's words.
		fails(t, r, "grep", `{"pattern":"needle","glob":"[abc"}`, "unclosed character class")
	})

	// rg's own walk: hidden files and directories below the root are skipped,
	// and inside a git repository so is what its .gitignore names. A glob
	// does not bring a hidden file back; a path that names one does.
	t.Run("hidden and ignored files", func(t *testing.T) {
		exactly(t, r, `{"pattern":"needle","path":"gp/hid"}`, "/workspace/gp/hid/a.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/hid/.hd"}`, "/workspace/gp/hid/.hd/x.txt")
		exactly(t, r, `{"pattern":"needle","path":"gp/repo"}`, "/workspace/gp/repo/main.go")
		exactly(t, r, `{"pattern":"needle","path":"gp/repo/run.log"}`, "/workspace/gp/repo/run.log")
	})

	t.Run("binary files", func(t *testing.T) {
		// Over a directory a binary file is skipped; named outright it is
		// searched, and its content answer is rg's notice.
		exactly(t, r, `{"pattern":"baz","path":"gp","output_mode":"content"}`, "no matches")
		exactly(t, r, `{"pattern":"foo","path":"gp/bin.dat","output_mode":"content"}`, `binary file matches (found "\0" byte around offset 7)`)
		exactly(t, r, `{"pattern":"foo","path":"gp/bin.dat","output_mode":"count"}`, "2")
	})

	t.Run("multiline", func(t *testing.T) {
		const base = `{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"content"`
		exactly(t, r, base+`}`, "6:six foo x bar")
		exactly(t, r, base+`,"multiline":true}`, "2:foo start\n3:middle\n4:end bar\n6:six foo x bar")
		exactly(t, r, `{"pattern":"foo.*?bar","path":"gp/ml.txt","output_mode":"count","multiline":true}`, "2")
		// Without it a newline in the pattern is rg's refusal.
		fails(t, r, "grep", `{"pattern":"middle\nend","path":"gp/ml.txt"}`, `the literal "\n" is not allowed in a regex`)
		exactly(t, r, `{"pattern":"middle\nend","path":"gp/ml.txt","output_mode":"content","multiline":true}`, "3:middle\n4:end bar")
	})

	t.Run("failures are rg's, in its words", func(t *testing.T) {
		fails(t, r, "grep", `{"pattern":"[unclosed","path":"gp"}`, "regex parse error")
		fails(t, r, "grep", `{"pattern":"needle","path":"gp/absent"}`, "No such file or directory")
	})

	// Every value the model chose reaches rg as one argv word: nothing it
	// carries is run by the shell, and nothing it begins with makes it an
	// option.
	t.Run("the model's values are data, not code or options", func(t *testing.T) {
		for _, v := range []string{"$(touch /tmp/pwned-a)", "`touch /tmp/pwned-b`", "';touch /tmp/pwned-c;'", "x\ntouch /tmp/pwned-d"} {
			q := strings.ReplaceAll(strings.ReplaceAll(v, "\n", `\n`), "`", "\\u0060")
			call(t, r, "grep", `{"pattern":"`+q+`","path":"gp"}`)
			call(t, r, "grep", `{"pattern":"needle","path":"gp","glob":"`+q+`"}`)
			call(t, r, "grep", `{"pattern":"needle","path":"gp","type":"`+q+`"}`)
			call(t, r, "grep", `{"pattern":"needle","path":"`+q+`"}`)
		}
		if out := ok(t, r, "bash", `{"command":"ls /tmp/pwned-* 2>/dev/null; echo checked"}`); strings.TrimSpace(out) != "checked" {
			t.Fatalf("a grep value was executed: %q", out)
		}
		ok(t, r, "bash", `{"command":"mkdir -p gp/dash && printf -- '--version\\n-x\\n' > gp/dash/-f.txt"}`)
		exactly(t, r, `{"pattern":"--version","path":"gp/dash","output_mode":"content"}`, "/workspace/gp/dash/-f.txt:1:--version")
		exactly(t, r, `{"pattern":"-x","path":"gp/dash/-f.txt","output_mode":"content"}`, "2:-x")
		exactly(t, r, `{"pattern":"x","path":"gp/dash","glob":"--files"}`, "no matches")
		fails(t, r, "grep", `{"pattern":"x","path":"gp/dash","type":"--help"}`, "unrecognized file type: --help")
	})
}

// installedRipgrep reports the mode and size of the rg grep installed, or ""
// when there is none.
func installedRipgrep(t *testing.T, r toolset.Runner) string {
	t.Helper()
	return strings.TrimSpace(ok(t, r, "bash", `{"command":"stat -c '%a %s' `+toolset.RipgrepPath()+` 2>/dev/null; true"}`))
}

// ripgrepSize is the size of this build's rg for the sandbox's machine.
func ripgrepSize(t *testing.T, r toolset.Runner) int64 {
	t.Helper()
	arch := toolset.LinuxArch(strings.TrimSpace(ok(t, r, "bash", `{"command":"uname -m"}`)))
	_, size, err := ripgrep.Open(arch)
	if err != nil {
		t.Fatalf("ripgrep.Open(%s): %v", arch, err)
	}
	return size
}

// timed runs one grep and says how long it took.
func timed(t *testing.T, r toolset.Runner, input string) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	got := ok(t, r, "grep", input)
	return got, time.Since(start)
}

// The first grep in a sandbox installs rg and the search runs; after that the
// binary is checked on every call, and one the model removed or replaced is
// written again rather than trusted. A machine nothing is shipped for is a
// tool error naming it.
func TestGrepInstallsRipgrepInTheSandbox(t *testing.T) {
	r := runner(t)
	ok(t, r, "bash", `{"command":"mkdir -p lc && echo needle > lc/a.txt"}`)
	const in = `{"pattern":"needle","path":"lc","output_mode":"content"}`
	const want = "/workspace/lc/a.txt:1:needle"
	if got := installedRipgrep(t, r); got != "" {
		t.Fatalf("a fresh sandbox already holds rg: %s", got)
	}
	got, first := timed(t, r, in)
	if got != want {
		t.Fatalf("first grep = %q, want %q", got, want)
	}
	if got, want := installedRipgrep(t, r), fmt.Sprintf("755 %d", ripgrepSize(t, r)); got != want {
		t.Fatalf("installed rg = %q, want %q (mode and size)", got, want)
	}
	_, warm := timed(t, r, in)
	t.Logf("docker %s: first grep (installs rg) %v, the next %v", testImage, first.Round(time.Millisecond), warm.Round(time.Millisecond))
	if out := ok(t, r, "bash", `{"command":"ls -A /tmp/.map-ripgrep"}`); strings.TrimSpace(out) != "rg-"+ripgrep.Pinned.Version {
		t.Errorf("/tmp/.map-ripgrep holds %q; an upload or a temporary was left behind", out)
	}

	for _, tamper := range []string{
		"rm -f " + toolset.RipgrepPath(),
		"printf '#!/bin/sh\\necho tampered\\n' > " + toolset.RipgrepPath(),
		"chmod 644 " + toolset.RipgrepPath(),
	} {
		ok(t, r, "bash", `{"command":"`+tamper+`"}`)
		if got := ok(t, r, "grep", in); got != want {
			t.Fatalf("grep after %q = %q, want %q", tamper, got, want)
		}
		if got, want := installedRipgrep(t, r), fmt.Sprintf("755 %d", ripgrepSize(t, r)); got != want {
			t.Fatalf("after %q rg = %q, want it written again (%s)", tamper, got, want)
		}
	}

	// A machine no binary is shipped for: the check reports what uname says.
	ok(t, r, "bash", `{"command":"printf '#!/bin/sh\\necho riscv64\\n' > /usr/local/bin/uname && chmod +x /usr/local/bin/uname && rm -f `+toolset.RipgrepPath()+`"}`)
	fails(t, r, "grep", in, `ripgrep is shipped for linux x86_64 and aarch64, and this sandbox is "riscv64"`)
	ok(t, r, "bash", `{"command":"rm -f /usr/local/bin/uname"}`)
}

// Calls that race to install on a fresh sandbox each land a whole binary —
// every upload has its own name and every move is atomic — and leave nothing
// but the one rg behind.
func TestGrepInstallsUnderConcurrentCalls(t *testing.T) {
	r := runner(t)
	ok(t, r, "bash", `{"command":"mkdir -p cc && echo needle > cc/a.txt"}`)
	const n = 4
	errs := make(chan string, n)
	for range n {
		go func() {
			res, err := r.Run(context.Background(), domain.NewID("sevt"), "grep", []byte(`{"pattern":"needle","path":"cc"}`))
			switch {
			case err != nil:
				errs <- err.Error()
			case res.IsError || res.Content != "/workspace/cc/a.txt":
				errs <- res.Content
			default:
				errs <- ""
			}
		}()
	}
	for range n {
		if e := <-errs; e != "" {
			t.Errorf("a concurrent grep failed: %s", e)
		}
	}
	if out := ok(t, r, "bash", `{"command":"ls -A /tmp/.map-ripgrep"}`); strings.TrimSpace(out) != "rg-"+ripgrep.Pinned.Version {
		t.Errorf("/tmp/.map-ripgrep holds %q after the race", out)
	}
	if got, want := installedRipgrep(t, r), fmt.Sprintf("755 %d", ripgrepSize(t, r)); got != want {
		t.Errorf("installed rg = %q, want %q", got, want)
	}
}

// rg is static, so the same binary answers the same search on an image with
// no glibc at all — musl and busybox — as on Debian.
func TestGrepRunsOnAMuslBusyboxImage(t *testing.T) {
	alpine := runnerFor(t, muslImage, sandbox.Hardening{})
	debian := runner(t)
	if out := ok(t, alpine, "bash", `{"command":"ls -l /bin/ls; ldd --version 2>&1 | head -n1"}`); !strings.Contains(out, "busybox") || !strings.Contains(out, "musl") {
		t.Fatalf("%s is not a musl/busybox image: %q", muslImage, out)
	}
	grepFixture(t, alpine)
	grepFixture(t, debian)
	sameAnswers(t, alpine, debian, "alpine", "debian")
}

// sameAnswers holds two sandboxes to one answer per search, across every mode
// and the options that change rg's flags.
func sameAnswers(t *testing.T, a, b toolset.Runner, an, bn string) {
	t.Helper()
	for in, wantErr := range map[string]bool{
		`{"pattern":"needle","path":"gp/code","glob":"!*_test.go","type":"go"}`:                                  false,
		`{"pattern":"needle","path":"gp/code","type":"ts","output_mode":"count"}`:                                false,
		`{"pattern":"x","path":"gp/ctx.txt","output_mode":"content","-i":true,"-C":1,"head_limit":5,"offset":1}`: false,
		`{"pattern":"foo.*?bar","path":"gp","output_mode":"content","multiline":true,"-A":1}`:                    false,
		`{"pattern":"needle","path":"gp/many","output_mode":"count","head_limit":2}`:                             false,
		`{"pattern":"1","path":"gp/seq.txt","output_mode":"content","head_limit":2}`:                             false,
		`{"pattern":"needle","path":"gp/repo"}`:                                                                  false,
		`{"pattern":"[unclosed","path":"gp"}`:                                                                    true,
	} {
		x, y := call(t, a, "grep", in), call(t, b, "grep", in)
		if y.IsError != wantErr || y.Content == "no matches" {
			t.Fatalf("grep(%s) on %s = %+v, a fixture that no longer exercises the search", in, bn, y)
		}
		if x.IsError != y.IsError || x.Content != y.Content {
			t.Errorf("grep(%s) differs:\n%s (is_error=%v): %q\n%s (is_error=%v): %q", in, an, x.IsError, x.Content, bn, y.IsError, y.Content)
		}
	}
}

// runnerFor is runner with an image and hardening of the test's choosing.
func runnerFor(t *testing.T, image string, h sandbox.Hardening) toolset.Runner {
	t.Helper()
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("toolset tests require Docker: %v", err)
	}
	sb, err := provider.Provision(context.Background(), sandbox.Spec{
		SessionID:  domain.NewID("sesn"),
		Image:      image,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
		Hardening:  h,
	})
	if err != nil {
		t.Fatalf("provision %s: %v", image, err)
	}
	t.Cleanup(func() {
		if err := sb.Destroy(context.Background()); err != nil {
			t.Errorf("destroy: %v", err)
		}
	})
	return toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
}

// On an image that does not run as root, Docker still writes the upload as
// root; the install copies it, as the sandbox user, into a file that user can
// make executable. A read-only root moves /tmp onto a volume, which rg runs
// from all the same, and a root that has dropped every capability still owns
// what it makes. The fixture goes in with the write tool, because bash's
// state directory is a root-owned volume to a non-root user under a
// read-only root on Docker (docs/self-hosted-security.md §2).
func TestGrepInstallsRipgrepWhereTheSandboxIsNotRoot(t *testing.T) {
	const image = "map-grep-nonroot-test:latest"
	build := exec.Command("docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader("FROM debian:stable-slim\nRUN useradd -m app && mkdir -p /workspace && chown app:app /workspace\nUSER app\n")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the non-root image: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		name, image string
		h           sandbox.Hardening
	}{
		{"non-root", image, sandbox.Hardening{}},
		{"non-root, read-only root", image, sandbox.Hardening{ReadOnlyRootfs: true}},
		{"root without capabilities", testImage, sandbox.Hardening{CapDrop: []string{"ALL"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runnerFor(t, tc.image, tc.h)
			ok(t, r, "write", `{"file_path":"nr/a.txt","content":"needle\n"}`)
			exactly(t, r, `{"pattern":"needle","path":"nr","output_mode":"count"}`, "/workspace/nr/a.txt:1")
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A build made without `make ripgrep` compiles, and its grep says why it
// cannot search rather than searching some other way.
func TestGrepWithoutAnEmbeddedRipgrep(t *testing.T) {
	r := runner(t)
	toolset.WithoutRipgrep(t)
	fails(t, r, "grep", `{"pattern":"x"}`, "grep: this build carries no ripgrep (it was built without `make ripgrep`)")
	if got := installedRipgrep(t, r); got != "" {
		t.Errorf("a build without rg installed something: %s", got)
	}
}

// attached starts a container the Docker provider will take for a session's
// sandbox — its name and ownership label — with docker run arguments of the
// test's own, which Provision has no knob for, and hands back the provider's
// handle to it.
func attached(t *testing.T, args ...string) toolset.Runner {
	t.Helper()
	sid := domain.NewID("sesn")
	name := "map-" + string(sid)
	run := append([]string{"run", "-d", "--name", name,
		"--label", "dev.opensdlc.managed-agent-platform.session-id=" + string(sid), "-w", "/workspace"}, args...)
	run = append(run, testImage, "/bin/bash", "-c", "while :; do sleep 3600; done")
	if out, err := exec.Command("docker", run...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	provider, err := docker.New(docker.Config{})
	if err != nil {
		t.Fatalf("toolset tests require Docker: %v", err)
	}
	sb, err := provider.Attach(context.Background(), sid)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	return toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
}

// tmpVolume is a /tmp backed by a tmpfs the daemon mounts as a volume — the
// archive endpoint every write uses reaches a volume, where it cannot reach a
// --tmpfs mount — with the mount options given.
func tmpVolume(opts string) []string {
	return []string{"--mount", `type=volume,dst=/tmp,volume-driver=local,volume-opt=type=tmpfs,volume-opt=device=tmpfs,"volume-opt=o=` + opts + `"`}
}

// Where rg cannot be installed or cannot run, grep is a tool error that says
// which: a /tmp mounted noexec, and a /tmp the sandbox user cannot write.
func TestGrepSaysWhyRipgrepCannotRun(t *testing.T) {
	t.Run("noexec", func(t *testing.T) {
		r := attached(t, tmpVolume("noexec,mode=1777")...)
		msg := fails(t, r, "grep", `{"pattern":"x"}`, "was written to "+toolset.RipgrepPath()+" but does not run there")
		if !strings.Contains(msg, "Permission denied") || !strings.Contains(msg, "allow executing files") {
			t.Errorf("error = %q, want the shell's refusal and what grep needs", msg)
		}
	})
	t.Run("unwritable", func(t *testing.T) {
		r := attached(t, append(tmpVolume("mode=0755"), "--user", "65534")...)
		fails(t, r, "grep", `{"pattern":"x"}`, "grep: cannot install ripgrep under /tmp/.map-ripgrep: permission denied")
	})
}

// TestGrepRunsTheSameInAKubernetesPod installs and runs rg in a pod — over
// the k8s backend's exec, whose stdin carries the binary, into the emptyDir a
// read-only root mounts at /tmp — on the Debian and the musl image, installs
// it again once it is removed, and holds each answer to Docker's. A missing cluster is a
// hard failure, as with the k8s contract test; point it at a local one with
// MAP_K8S_CONTEXT.
func TestGrepRunsTheSameInAKubernetesPod(t *testing.T) {
	provider, err := k8s.New(k8s.Config{
		Context:   os.Getenv("MAP_K8S_CONTEXT"),
		Namespace: os.Getenv("MAP_K8S_NAMESPACE"),
	})
	if err != nil {
		t.Fatalf("this test requires a Kubernetes cluster: %v", err)
	}
	docker := runner(t)
	grepFixture(t, docker)
	for _, image := range []string{testImage, muslImage} {
		t.Run(image, func(t *testing.T) {
			sb, err := provider.Provision(context.Background(), sandbox.Spec{
				SessionID:  domain.NewID("sesn"),
				Image:      image,
				Networking: domain.Networking{Type: domain.NetUnrestricted},
				Hardening:  sandbox.Hardening{ReadOnlyRootfs: true},
			})
			if err != nil {
				t.Fatalf("provision: %v", err)
			}
			t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
			r := toolset.Runner{Sandbox: sb, Session: domain.NewID("sesn")}
			ok(t, r, "bash", `{"command":"mkdir -p lc && echo needle > lc/a.txt"}`)
			const in = `{"pattern":"needle","path":"lc","output_mode":"content"}`
			const want = "/workspace/lc/a.txt:1:needle"
			got, first := timed(t, r, in)
			if got != want {
				t.Fatalf("first grep = %q, want %q", got, want)
			}
			_, warm := timed(t, r, in)
			t.Logf("kind %s: first grep (installs rg) %v, the next %v", image, first.Round(time.Millisecond), warm.Round(time.Millisecond))
			if got, want := installedRipgrep(t, r), fmt.Sprintf("755 %d", ripgrepSize(t, r)); got != want {
				t.Fatalf("installed rg = %q, want %q", got, want)
			}
			ok(t, r, "bash", `{"command":"rm -f `+toolset.RipgrepPath()+`"}`)
			if got := ok(t, r, "grep", in); got != want {
				t.Fatalf("grep after the binary was removed = %q, want %q", got, want)
			}
			grepFixture(t, r)
			sameAnswers(t, r, docker, "k8s", "docker")
		})
	}
}

// scripted answers each Exec with the next result the test queued, and
// refuses the upload with streamErr when one is set, so the install's fault
// paths a real sandbox will not produce on demand can be pinned.
type scripted struct {
	*fakeSandbox
	results   []sandbox.ExecResult
	streamErr error
	uploaded  map[string]int64
}

func (s *scripted) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	_, _ = s.fakeSandbox.Exec(ctx, req)
	if len(s.results) == 0 {
		return sandbox.ExecResult{}, sandbox.ErrNotFound
	}
	res := s.results[0]
	s.results = s.results[1:]
	return res, nil
}

func (s *scripted) WriteFileStream(_ context.Context, path string, src io.Reader, size int64) error {
	if s.streamErr != nil {
		return s.streamErr
	}
	n, err := io.Copy(io.Discard, src)
	if s.uploaded == nil {
		s.uploaded = map[string]int64{}
	}
	s.uploaded[path] = n
	if err != nil || n != size {
		return fmt.Errorf("short upload: %d of %d: %v", n, size, err)
	}
	return nil
}

func TestGrepInstallFaults(t *testing.T) {
	missing := func(machine string) sandbox.ExecResult {
		return sandbox.ExecResult{ExitCode: 97, Stdout: "map-ripgrep-missing " + machine + "\n"}
	}
	found := sandbox.ExecResult{Stdout: "/workspace/a.txt\n"}
	for _, tc := range []struct {
		name      string
		results   []sandbox.ExecResult
		streamErr error
		want      string // a tool error mentioning this, or "" for found's answer
		fault     error  // or a backend fault
	}{
		{name: "an x86_64 sandbox gets the amd64 binary", results: []sandbox.ExecResult{missing("x86_64"), {}, found}},
		{name: "an upload the path refuses", results: []sandbox.ExecResult{missing("aarch64")},
			streamErr: fmt.Errorf("x: %w", sandbox.ErrNotDirectory), want: "cannot install ripgrep under /tmp/.map-ripgrep: x: sandbox: path is not a directory"},
		{name: "an upload the sandbox fails", results: []sandbox.ExecResult{missing("aarch64")},
			streamErr: sandbox.ErrNotFound, fault: sandbox.ErrNotFound},
		{name: "an install that timed out", results: []sandbox.ExecResult{missing("aarch64"), {TimedOut: true}},
			want: "installing ripgrep timed out"},
		{name: "an install step that failed", results: []sandbox.ExecResult{missing("aarch64"), {ExitCode: 1, Stderr: "cat: write error: No space left on device\n"}},
			want: "cannot install ripgrep at /tmp/.map-ripgrep/rg-" + ripgrep.Pinned.Version + ": cat: write error: No space left on device"},
		{name: "an install whose exec failed", results: []sandbox.ExecResult{missing("aarch64")}, fault: sandbox.ErrNotFound},
		{name: "a binary gone again before the search", results: []sandbox.ExecResult{missing("aarch64"), {}, missing("aarch64")},
			want: "was gone again before the search ran"},
		// Exit 97 without the marker is not the check's: it is answered as
		// any other failure.
		{name: "an exit 97 that is not the check's", results: []sandbox.ExecResult{{ExitCode: 97, Stderr: "boom\n"}}, want: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := &scripted{fakeSandbox: &fakeSandbox{}, results: tc.results, streamErr: tc.streamErr}
			res, err := run(t, sb, "grep", `{"pattern":"x"}`)
			switch {
			case tc.fault != nil:
				if !errors.Is(err, tc.fault) {
					t.Fatalf("grep = %+v, %v; want the fault %v", res, err, tc.fault)
				}
			case tc.want != "":
				if err != nil || !res.IsError || !strings.Contains(res.Content, tc.want) {
					t.Fatalf("grep = %+v, %v; want a tool error mentioning %q", res, err, tc.want)
				}
			default:
				if err != nil || res.IsError || res.Content != "/workspace/a.txt" {
					t.Fatalf("grep = %+v, %v; want the search's answer", res, err)
				}
				_, size, _ := ripgrep.Open("amd64")
				for path, n := range sb.uploaded {
					if !strings.HasPrefix(path, "/tmp/.map-ripgrep/.upload-") || n != size {
						t.Errorf("uploaded %d bytes to %s, want the amd64 binary's %d", n, path, size)
					}
				}
				if len(sb.uploaded) != 1 {
					t.Errorf("uploads = %v, want one", sb.uploaded)
				}
			}
		})
	}
}

// What rg prints beside an answer — a warning about an ignore file it could
// not read — follows the answer rather than being dropped, and an answer the
// sandbox's own cap cut says so.
func TestGrepKeepsWhatRipgrepSaidBesideTheAnswer(t *testing.T) {
	sb := &fakeSandbox{exec: sandbox.ExecResult{Stdout: "/workspace/a.txt\n", Stderr: "rg: ./.gitignore: line 1: error parsing glob\n", Truncated: true}}
	res, err := run(t, sb, "grep", `{"pattern":"x"}`)
	if err != nil || res.IsError || res.Content != "[output truncated]\n/workspace/a.txt\nrg: ./.gitignore: line 1: error parsing glob" {
		t.Fatalf("grep = %+v, %v", res, err)
	}
}

func TestLinuxArch(t *testing.T) {
	for machine, want := range map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
		"armv7l": "", "i686": "", "riscv64": "", "s390x": "", "": ""} {
		if got := toolset.LinuxArch(machine); got != want {
			t.Errorf("LinuxArch(%q) = %q, want %q", machine, got, want)
		}
	}
}
