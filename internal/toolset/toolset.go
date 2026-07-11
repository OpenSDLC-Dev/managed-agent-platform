// Package toolset is the built-in agent_toolset_20260401: the six tools the
// platform executes for the model — bash, read, write, edit, glob, grep — run
// inside the session's sandbox.
//
// Two halves. Tools turns an agent's toolset entry into the definitions the
// model is handed (name, description, input schema); Runner.Run executes one
// call of a named tool against a sandbox. Nothing here talks to the event log
// or the work queue: what a tool call means for the session is the executor's,
// and this package only knows how to run one.
//
// The reference implementation of these six is anthropic-sdk-go's
// tools/agenttoolset, which runs them on the host and therefore has to confine
// the file tools to a workdir and warn that bash cannot be confined at all.
// Here the container IS the confinement, and bash runs in it like everything
// else, so the file tools resolve relative paths against the workdir and
// otherwise let a path be a path: a model that wants /etc can read it with
// bash regardless, and a lexical check that bash ignores is theatre, not a
// boundary.
//
// Divergences from that reference, all deliberate:
//   - No workdir confinement (above). Absolute paths and absolute glob
//     patterns are accepted.
//   - grep shells out to GNU grep inside the sandbox (PCRE where the image's
//     grep has it, POSIX ERE otherwise) rather than preferring ripgrep and
//     falling back to a Go walker. One implementation, one behaviour, and no
//     dependence on what the image happens to ship beyond the /bin/bash the
//     sandbox already requires.
//   - The tools carry no state between calls except bash's, which is the
//     shell package's snapshot; there is no per-runner session object to close.
package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

const (
	// MaxOutputBytes caps what a tool call returns to the model. The sandbox
	// caps a command's output an order of magnitude higher (that cap is a
	// memory guard on the executor); this one is the model's context budget,
	// and it is the tool result that goes on the event log forever.
	MaxOutputBytes = 100 << 10

	// DefaultTimeout bounds a tool call the model did not time itself, and
	// MaxTimeout bounds the one it did. A model-chosen timeout is a lease the
	// executor has to keep alive, so it cannot be unbounded.
	DefaultTimeout = 2 * time.Minute
	MaxTimeout     = 10 * time.Minute

	truncationNotice = "[output truncated]"
)

// Result is one tool call as the model sees it. IsError marks a tool-level
// failure — a missing file, a bad regex, a nonzero exit — which the model reads
// and can recover from. A backend fault (the sandbox is gone, the daemon is
// unreachable) is never a Result: it comes back from Run as an error, and what
// happens to the tool call then is the executor's decision, not the model's.
type Result struct {
	Content string
	IsError bool
}

// Runner executes built-in tool calls inside one session's sandbox.
type Runner struct {
	Sandbox sandbox.Sandbox
	// Session scopes the bash shell's state in the container.
	Session domain.ID
	// Workdir is where relative tool paths resolve. Empty means the sandbox's
	// own default, which is where its Exec already runs.
	Workdir string
}

// Run executes the named built-in tool. id names this call — the tool-use
// event's id — and scopes the bash shell's per-call files.
func (r Runner) Run(ctx context.Context, id domain.ID, name string, input json.RawMessage) (Result, error) {
	var (
		res Result
		err error
	)
	switch name {
	case "bash":
		res, err = r.bash(ctx, id, input)
	case "read":
		res, err = r.read(ctx, input)
	case "write":
		res, err = r.write(ctx, input)
	case "edit":
		res, err = r.edit(ctx, input)
	case "glob":
		res, err = r.glob(ctx, input)
	case "grep":
		res, err = r.grep(ctx, input)
	default:
		// Not a backend fault: the model asked for something this platform does
		// not run (web_fetch and web_search are named in the wire's tool-config
		// enum and are deferred). Telling it so lets it try something else.
		return failf("unknown tool %q", name)
	}
	if err != nil {
		return Result{}, err
	}
	res.Content = capOutput(res.Content)
	return res, nil
}

// workdir is the root relative tool paths resolve against.
func (r Runner) workdir() string {
	if r.Workdir == "" {
		return sandbox.DefaultWorkdir
	}
	return r.Workdir
}

// resolve roots a model-supplied path. Slash paths, not filepath: the sandbox is
// a Linux container whatever the executor runs on.
func (r Runner) resolve(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	return path.Join(r.workdir(), p)
}

// succeed and failf are the two Result shapes; both return a nil error, because
// a tool that ran and failed is not a backend fault.
func succeed(content string) (Result, error) { return Result{Content: content}, nil }

func failf(format string, a ...any) (Result, error) {
	return Result{Content: fmt.Sprintf(format, a...), IsError: true}, nil
}

// capOutput trims content to MaxOutputBytes, backing off to a rune boundary so
// a split multi-byte character does not reach the event log as a replacement
// character.
func capOutput(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	cut := s[:MaxOutputBytes]
	for i := 0; i < utf8.UTFMax && len(cut) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut + "\n" + truncationNotice
}

// combine folds a command's two streams into the one text block a tool result
// carries. Interleaving is lost — the sandbox captures the streams separately —
// so stderr follows stdout whole.
func combine(res sandbox.ExecResult) string {
	out := res.Stdout
	if res.Stderr != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += res.Stderr
	}
	if res.Truncated {
		out = truncationNotice + "\n" + out
	}
	return out
}

// singleQuote wraps s as one bash single-quoted word, so a model-supplied path
// or pattern reaches the command as data and can never be read as code. Inside
// single quotes bash expands nothing; the only escape is the quote itself.
func singleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
