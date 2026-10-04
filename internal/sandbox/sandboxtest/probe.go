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

// AnswerProbeEach answers script — what a fake sandbox was handed, its frame
// taken off (Unwrap) — as a sandbox would where it is a batch of
// sandbox.ProbeEach's: the index of every path exists says is not there, a
// line each, ok true. Any other script is not one: ok false.
func AnswerProbeEach(script string, exists func(path string) bool) (res sandbox.ExecResult, ok bool) {
	if !probeEachScript.MatchString(script) {
		return sandbox.ExecResult{}, false
	}
	var out strings.Builder
	for _, m := range probeEachLine.FindAllStringSubmatch(script, -1) {
		if !exists(strings.ReplaceAll(m[1], `'\''`, "'")) {
			out.WriteString(m[2] + "\n")
		}
	}
	return sandbox.ExecResult{Stdout: out.String()}, true
}
