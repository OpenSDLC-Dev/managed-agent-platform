// Package toolset is the built-in agent_toolset_20260401: the six tools the
// platform executes for the model — bash, read, write, edit, glob, grep — run
// inside the session's sandbox.
//
// It also holds a second, unrelated six: the delegation tools of a multi-agent
// session (delegation.go). Only their names, descriptions and schemas live here.
// Runner must never be handed one — they touch no sandbox, and the brain's
// settlement answers them itself (plan 35 decision 6); IsDelegationTool is the
// predicate every driver consults to keep them out.
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
//   - write and edit preserve the permission bits of an existing regular file they
//     replace, where the reference writes a fixed 0644 (its atomicWriteFile chmods
//     the temporary file to that constant before renaming). Where nothing is
//     carried over — a new file, a symlink, a docker sandbox whose user cannot
//     chmod the temporary file (#209) — 0644 is what lands here too. The Claude Code harness preserves
//     them, and the workflow that decided it is ordinary — `chmod +x` a script in
//     bash, edit it, run it (#204). The rename that makes the write atomic is the
//     sandbox backends'; both carry the mode over (internal/sandbox/filefault.go).
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
	// SearchResults, when non-nil, is the structured content of a web_search
	// answer: the tool_result carries these search_result blocks instead of a
	// text block (an empty non-nil slice is an empty content array — a search
	// with no hits). Only the executor's web driver sets it; nil keeps today's
	// text shape byte-identical.
	SearchResults []domain.SearchResultBlock
	IsError       bool
}

// NoOutput is the text block posted for a Result whose Content is empty — a
// tool that succeeded silently. It is the reference runner's placeholder
// (its wording since v1.63.1: "The Sessions API rejects empty text blocks; a tool that succeeds
// silently must still produce a postable result"), and one constant because
// the executor and the BYOC worker must post the same block for the same
// empty read.
const NoOutput = "(no output)"

// Runner executes built-in tool calls inside one session's sandbox.
type Runner struct {
	Sandbox sandbox.Sandbox
	// Session scopes the bash shell's state in the container.
	Session domain.ID
	// Workdir is where relative tool paths resolve. Empty means the sandbox's
	// own default, which is where its Exec already runs.
	Workdir string
	// MemoryRoots are the session's mounted memory stores and ReadOnlyRoots
	// the ones attached read_only (plan 36 decision 12). write and edit
	// refuse a path inside a read-only root, and any path under /mnt/memory
	// that is inside no root — the tree is the stores' and their baselines',
	// and a file written beside them would belong to nothing. bash is not
	// confined: the sync pulls from a read-only store and never pushes to it,
	// so what bash writes there is overwritten by the store's next change and
	// never reaches the store.
	MemoryRoots   []string
	ReadOnlyRoots []string
}

// Run executes the named built-in tool. id names this call — the tool-use
// event's id — and scopes the bash shell's per-call files.
//
// Every tool call the platform runs arrives here, from the cloud executor and
// the BYOC worker alike, so this is the one place the tool-execution metric can
// be recorded once and mean the same thing at both deployment points.
func (r Runner) Run(ctx context.Context, id domain.ID, name string, input json.RawMessage) (res Result, err error) {
	start := time.Now()
	defer func() { RecordRun(ctx, name, time.Since(start), res, err) }()
	return r.dispatch(ctx, id, name, input)
}

func (r Runner) dispatch(ctx context.Context, id domain.ID, name string, input json.RawMessage) (Result, error) {
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
		// Not a backend fault: the model asked for something this Runner does
		// not run. The web tools never arrive here — the executor routes them
		// to its own web driver and every sandbox-tool scan filters them out
		// (IsWebTool) — so a name landing on this arm is one the platform does
		// not recognise at all. Telling the model so lets it try something else.
		return failf("unknown tool %q", name)
	}
	if err != nil {
		return Result{}, err
	}
	res.Content = SanitizeText(res.Content)
	// read never spills: its full content already sits in the sandbox at the
	// very path the model just named, so a spill would only copy a file to a
	// second file — and reading a spill file back would then mint another
	// copy under a fresh id on every attempt, a chain with no fixed point.
	// An oversized read truncates plainly; view_range and bash slicing reach
	// the rest. Every other tool's output is ephemeral — the one place the
	// spill earns its keep.
	if name != "read" {
		if notice := r.spill(ctx, id, res.Content); notice != "" {
			res.Content = TruncateRunes(res.Content, MaxOutputBytes) + "\n" + notice
			return res, nil
		}
	}
	res.Content = CapOutput(res.Content)
	return res, nil
}

// spillDir is where an oversized output's full bytes land in the sandbox —
// outside the workdir, so a project-relative glob or grep never matches a
// spill file. The convention is ours: the reference documents the spill but
// neither its path nor its preview shape (docs/DIVERGENCES.md, #226).
const spillDir = "/tmp/tool_outputs"

// spill writes an output past MaxOutputBytes to the sandbox whole and returns
// the truncation notice naming the file — or "" when the output fits, or when
// the write fails: the caller then truncates exactly as before, so the spill
// is an enhancement, never a new failure mode for the call. The web tools
// never reach it — their driver runs with no sandbox at all, a deliberate
// divergence (web content is re-fetchable; a command's output is not).
func (r Runner) spill(ctx context.Context, id domain.ID, full string) string {
	if len(full) <= MaxOutputBytes {
		return ""
	}
	path, err := SpillFile(ctx, r.Sandbox, id, full)
	if err != nil {
		return ""
	}
	return "[output truncated; full output written to " + path + "]"
}

// SpillFile writes one call's oversized output to the sandbox and returns the
// path, or the error the sandbox refused the write with. It is the half of spill
// that decides *where*, without the budget test or the notice.
//
// It hands back the error rather than a bool because the sandbox classifies the
// refusal (ErrNotFound, ErrNotDirectory, ErrNotWritable) and that classification
// is the only account of why no file exists — this package logs nothing, by
// design, so a caller that runs on a shared process is where it can be said.
//
// Exported for the executor's MCP driver (plan 29 slice 4c), which spills to the
// same directory under the same id-per-call convention — so a model that has
// learned where its truncated output goes is right whichever tool produced it —
// but says something different about it. The two differ where they must and
// nowhere else: an MCP answer spills its *text*, so it cannot borrow a sentence
// promising the full output, and it spills on a trigger of its own — whether the
// rendering lost anything, which a length test cannot answer for an answer made
// of blocks — so it cannot borrow the budget test either.
func SpillFile(ctx context.Context, sb sandbox.Sandbox, id domain.ID, full string) (string, error) {
	path := spillDir + "/" + id.String() + ".txt"
	if err := sb.WriteFile(ctx, path, []byte(full)); err != nil {
		return "", err
	}
	return path, nil
}

// SanitizeText strips NUL bytes from tool output. Postgres's jsonb cannot
// store \u0000 inside a string value, so a NUL anywhere in a result — one
// byte of /dev/zero on stdout is enough — would fault the event append, and a
// faulted work item reclaim-loops, re-running the same command into the same
// failure. Sanitized before CapOutput so the log budget is spent on bytes
// that survive. Exported for the executor's web driver, which produces
// results outside this Runner (the same reason CapOutput is exported).
func SanitizeText(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
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

// MemoryMountRoot is where memory stores mount (plan 36 decision 8), the tree
// the roots above carve up; the executor mounts under it, and the API keeps
// repository mounts out of it.
const MemoryMountRoot = "/mnt/memory"

// unwritable says why a resolved path may not be written by the file tools —
// the read-only store it is inside, or the reserved tree it is loose in — or
// "" when it may. display is the path the model used, for the message; the
// read-only wording is the reference toolset's own (plan 36 decision 12).
// The check is lexical, like resolve: a symlink the agent planted can lead a
// path out of its root, as `bash` can write anywhere anyway — the store
// behind a read-only mount is protected by the sync's pull-only mode, and
// this is the clear answer the file tools owe a model that tries.
func (r Runner) unwritable(display, resolved string) string {
	for _, root := range r.ReadOnlyRoots {
		if under(resolved, root) {
			return fmt.Sprintf("%s is inside read-only directory %s", display, root)
		}
	}
	if !under(resolved, MemoryMountRoot) {
		return ""
	}
	for _, root := range r.MemoryRoots {
		if under(resolved, root) {
			return ""
		}
	}
	return fmt.Sprintf("%s is under %s, which holds only mounted memory stores; nothing is mounted at that path", display, MemoryMountRoot)
}

func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// succeed and failf are the two Result shapes; both return a nil error, because
// a tool that ran and failed is not a backend fault.
func succeed(content string) (Result, error) { return Result{Content: content}, nil }

func failf(format string, a ...any) (Result, error) {
	return Result{Content: fmt.Sprintf(format, a...), IsError: true}, nil
}

// badField reports a model-supplied path or pattern that carries a NUL byte,
// as a tool error. NUL is invalid in every Unix path and json.Unmarshal accepts
// it; left unchecked it reaches the sandbox as a tar header (which archive/tar
// rejects) or a truncated command — surfacing as a backend fault for what is
// really the model's own malformed input, and so misrouting the two failure
// kinds Run is careful to keep apart. It returns the error result and true when
// the field is bad.
func badField(tool, field, value string) (Result, bool) {
	if strings.IndexByte(value, 0) >= 0 {
		return Result{Content: fmt.Sprintf("%s: %s must not contain a NUL byte", tool, field), IsError: true}, true
	}
	return Result{}, false
}

// TruncateRunes returns s cut to at most n bytes, backing off to a rune
// boundary so a split multi-byte character never reaches the event log as a
// replacement character.
//
// Exported for the reason CapOutput is: other packages cut strings against
// budgets of their own — the executor's MCP driver caps a resource label, the
// brain caps a name its tool notes quote — and every hand-rolled cut is another
// chance to land mid-rune, where json.Marshal coerces the tail to U+FFFD and the
// corruption is silent.
func TruncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for i := 0; i < utf8.UTFMax && len(cut) > 0; i++ {
		if r, size := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut
}

// CapOutput trims content to MaxOutputBytes, marking the truncation. Exported
// for the same reason RecordRun is: the executor's web driver produces tool
// results outside this Runner and must honor the SAME log budget — one cap,
// one meaning, whatever process ran the tool.
func CapOutput(s string) string {
	if len(s) <= MaxOutputBytes {
		return s
	}
	return TruncateRunes(s, MaxOutputBytes) + "\n" + truncationNotice
}

// capWithTrailer caps body so that body + trailer still fits MaxOutputBytes with
// the trailer whole. bash's exit-code and timeout lines are the load-bearing
// signal — whether the command failed — and must survive truncation of a huge
// output, which they would not if the trailer were appended and the join then
// capped from the end. The result is already within the cap, so Run's own
// CapOutput leaves it untouched. spillNotice, when non-empty, replaces the
// plain truncation marker — a spilled body's preview names its file.
func capWithTrailer(body, trailer, spillNotice string) string {
	if len(body)+len(trailer) <= MaxOutputBytes {
		return body + trailer
	}
	notice := "\n" + truncationNotice
	if spillNotice != "" {
		notice = "\n" + spillNotice
	}
	budget := MaxOutputBytes - len(trailer) - len(notice)
	if budget < 0 {
		budget = 0
	}
	return TruncateRunes(body, budget) + notice + trailer
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
