package sandboxtest

import (
	"regexp"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// probeEachLine is one line of sandbox.ProbeEach's script: a path, quoted
// whole, and the index the line prints when the path is not there.
var probeEachLine = regexp.MustCompile(`test -e '((?:[^']|'\\'')*)' \|\| echo ([0-9]+)\n`)

// probeEachScript is a script of those lines and nothing else.
var probeEachScript = regexp.MustCompile(`^(?:test -e '(?:[^']|'\\'')*' \|\| echo [0-9]+\n)+$`)

// ProbeEachPaths is the paths script — what a fake sandbox was handed, its
// frame taken off (Unwrap), as AnswerProbeEach takes it — asks about where it
// is a batch of sandbox.ProbeEach's, in the batch's order; ok is false where
// it is not one.
func ProbeEachPaths(script string) (paths []string, ok bool) {
	tests, ok := probeEachTests(script)
	for _, t := range tests {
		paths = append(paths, t.path)
	}
	return paths, ok
}

// AnswerProbeEach answers script — what a fake sandbox was handed, its frame
// taken off (Unwrap) — as a sandbox would where it is a batch of
// sandbox.ProbeEach's: the index of every path exists says is not there, a
// line each, ok true. Any other script is not one: ok false.
func AnswerProbeEach(script string, exists func(path string) bool) (res sandbox.ExecResult, ok bool) {
	tests, ok := probeEachTests(script)
	if !ok {
		return sandbox.ExecResult{}, false
	}
	var out strings.Builder
	for _, t := range tests {
		if !exists(t.path) {
			out.WriteString(t.index + "\n")
		}
	}
	return sandbox.ExecResult{Stdout: out.String()}, true
}

// probeEachTest is one line of a ProbeEach batch: the path it tests, unquoted,
// and the index it prints when the path is not there.
type probeEachTest struct{ path, index string }

// probeEachTests reads script's lines where it is a batch of
// sandbox.ProbeEach's; ok is false where it is not one.
func probeEachTests(script string) (tests []probeEachTest, ok bool) {
	if !probeEachScript.MatchString(script) {
		return nil, false
	}
	for _, m := range probeEachLine.FindAllStringSubmatch(script, -1) {
		tests = append(tests, probeEachTest{strings.ReplaceAll(m[1], `'\''`, "'"), m[2]})
	}
	return tests, true
}
