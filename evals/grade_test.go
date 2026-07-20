package evals

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// Grading is deliberately code-based: every assertion below is a deterministic
// function of the transcript, the sandbox filesystem, or the session status. No
// LLM judge. A judge would be a second model whose own drift is indistinguishable
// from the drift this suite exists to detect, and none of these tasks needs one —
// they are engineered so that correct behavior leaves a mechanical trace.
//
// The trick that makes that possible is the nonce (see fill): prompts demand an
// exact token that only this trial's agent could know, so an exact-match check
// tests the agent rather than the grader's generosity.

// Class says whose bug a failure is, and therefore what a red run means.
//
// The distinction is the whole point of the suite. A grader that cannot say
// whether the platform broke or the model merely wandered produces a signal
// nobody can act on: every red run becomes "probably the model" and the one that
// was a real regression is dismissed with the rest.
type Class string

const (
	// Platform: the product is wrong. The model could not have caused this and
	// no rerun can fix it. A P failure is a bug.
	Platform Class = "P"
	// Model: the platform did its job and the model did not do as asked. Worth
	// seeing — a prompt to sharpen, or genuine model drift — but not a product
	// defect.
	Model Class = "M"
	// Either: the evidence does not separate the two. Triage by hand.
	Either Class = "E"
)

// Grader is one assertion about a finished trial. Check returns nil to pass and
// an error describing the actual observed state to fail — the message is read by
// someone who was not watching the run, so it names what it saw, not just what
// it wanted.
type Grader struct {
	Name  string
	Class Class
	Check func(t *testing.T, tr *Trial) error
}

// corePack is prepended to every task: the invariants that must hold no matter
// what the task asked for. They are what turns each task into a platform test
// rather than a prompt test — a task can be about anything and still assert that
// the event log, the queue, the stream and the accounting all behaved.
func corePack(task Task) []Grader {
	return []Grader{{
		Name:  "idle-observed-on-stream",
		Class: Platform,
		Check: func(_ *testing.T, tr *Trial) error {
			// runTrial cannot reach grading without an idle per turn, so this
			// pins the fact rather than discovering it: SSE delivered every
			// one. Were the stream to go quiet, the trial would die at its
			// turn timeout and this grader would never run — which is why the
			// timeout message names the session.
			if len(tr.Idles) != len(task.Turns) {
				return fmt.Errorf("saw %d idle frames on the stream for %d turns",
					len(tr.Idles), len(task.Turns))
			}
			return nil
		},
	}, {
		Name:  "ends-with-end-turn",
		Class: Either,
		// Either, not Platform: a stop_reason of max_tokens is the model
		// running long, while a missing or malformed one is ours. The
		// transcript separates them; the class cannot.
		Check: func(_ *testing.T, tr *Trial) error {
			if len(tr.Idles) == 0 {
				return fmt.Errorf("no idle observed, so there is no final stop_reason to check")
			}
			last := tr.Idles[len(tr.Idles)-1]
			if got := stopReasonType(last); got != "end_turn" {
				return fmt.Errorf("final idle stop_reason.type = %q, want end_turn (stop_reason %v)",
					got, last["stop_reason"])
			}
			return nil
		},
	}, {
		Name:  "no-session-error",
		Class: Platform,
		Check: func(_ *testing.T, tr *Trial) error {
			if errs := eventsOfType(tr, "session.error"); len(errs) > 0 {
				return fmt.Errorf("%d session.error event(s), first: %v", len(errs), errs[0])
			}
			return nil
		},
	}, {
		Name:  "tool-results-joined",
		Class: Platform,
		// The executor's contract in one line: every intent the brain emitted
		// got exactly one answer. Zero means a tool call was dropped and the
		// session would wedge on resume; two would double-feed the model.
		Check: func(_ *testing.T, tr *Trial) error {
			answers := map[string]int{}
			for _, ev := range eventsOfType(tr, "agent.tool_result") {
				id, _ := ev["tool_use_id"].(string)
				answers[id]++
			}
			for _, use := range eventsOfType(tr, "agent.tool_use") {
				id, _ := use["id"].(string)
				// A missing id is a wire regression, not a join to check: left as
				// the empty string it would match a tool_result whose tool_use_id
				// was also dropped, and the pair would pass vacuously. Reject it
				// as the malformed event it is.
				if id == "" {
					return fmt.Errorf("agent.tool_use (%v) has no id", use["name"])
				}
				if n := answers[id]; n != 1 {
					return fmt.Errorf("tool_use %s (%v) has %d tool_results, want exactly 1",
						id, use["name"], n)
				}
				delete(answers, id)
			}
			for id, n := range answers {
				return fmt.Errorf("%d tool_result(s) for tool_use %s, which is not on the log", n, id)
			}
			return nil
		},
	}, {
		Name:  "usage-accounted",
		Class: Platform,
		// Token accounting is what a self-hosting operator bills and capacity-plans
		// against, so "the agent worked but the numbers were empty" is a real
		// defect and not a cosmetic one.
		Check: func(_ *testing.T, tr *Trial) error {
			ends := eventsOfType(tr, "span.model_request_end")
			if len(ends) == 0 {
				return fmt.Errorf("no span.model_request_end events: the turn never reached the model")
			}
			for _, ev := range ends {
				in, out, ok := modelUsage(ev)
				if !ok {
					return fmt.Errorf("span.model_request_end %v carries no model_usage", ev["id"])
				}
				if in <= 0 || out <= 0 {
					return fmt.Errorf("model_usage input=%v output=%v, want both above zero", in, out)
				}
			}
			return nil
		},
	}, {
		Name:  "session-status-idle",
		Class: Platform,
		// The status the REST surface reports, not the one the stream implied:
		// the two are written by different paths and a divergence is exactly the
		// kind of bug a client would hit and a stream-only assertion would miss.
		Check: func(t *testing.T, tr *Trial) error {
			got := tr.stack.getSession(t, tr.SessionID)["status"]
			if got != "idle" {
				return fmt.Errorf("GET session status = %v, want idle", got)
			}
			return nil
		},
	}}
}

// FileLines asserts a sandbox file's exact lines, in order. The path and each
// want entry may carry the trial's tokens — the path because Seed substitutes
// into its own, and a grader reading the literal-braced name while the seed
// wrote the filled one would red the platform for a file that is exactly where
// it should be.
//
// Lines rather than bytes, because the one byte these tasks cannot pin is the
// trailing newline: a model that prints line by line emits one and a model that
// joins on "\n" does not, and both did the task. Everything else stays exact —
// extra lines, reordering, stray prose and leading whitespace all fail — so this
// forgives the convention without forgiving the content. Tasks that genuinely
// need byte-equality (an edit that must preserve a file verbatim) want a
// different grader.
func FileLines(path string, want []string, class Class) Grader {
	return Grader{
		Name:  "file-lines:" + path,
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			raw, err := tr.readFile(t, tr.fill(path))
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			got := splitLines(string(raw))
			w := make([]string, len(want))
			for i, s := range want {
				w[i] = tr.fill(s)
			}
			if !slices.Equal(got, w) {
				return fmt.Errorf("%s has %d line(s) %q, want %d line(s) %q",
					path, len(got), got, len(w), w)
			}
			return nil
		},
	}
}

// splitLines splits a text file into lines, ignoring a trailing newline. An
// empty file is zero lines, not one empty line.
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// FinalMessageHas asserts the agent's last message contains sub (after nonce
// substitution). Only ever used for a token the prompt explicitly demanded —
// never for incidental prose, which is the model's to word however it likes.
func FinalMessageHas(sub string, class Class) Grader {
	return Grader{
		Name:  "final-message-has:" + sub,
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			want := tr.fill(sub)
			got := finalMessage(tr)
			if !strings.Contains(got, want) {
				return fmt.Errorf("final agent.message = %q, want it to contain %q", got, want)
			}
			return nil
		},
	}
}

// ToolUseAtLeast asserts the agent called a tool at least n times. An empty name
// counts every tool.
//
// A floor rather than an exact count on purpose: how many commands a model needs
// is its business, and pinning it exactly would fail the eval on a model that did
// the job more efficiently. It counts tool_use events, not round trips — the
// brain can emit several in one turn and the executor answers them as a batch, so
// n≥2 means "the model reached for a tool more than once", not "the loop
// suspended and resumed twice". The core pack's tool-results-joined grader is
// what actually proves the loop closed.
func ToolUseAtLeast(name string, n int, class Class) Grader {
	label := name
	if label == "" {
		label = "any"
	}
	return Grader{
		Name:  fmt.Sprintf("tool-use-at-least:%s:%d", label, n),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			got := countToolUse(tr, name)
			if got < n {
				return fmt.Errorf("%d %s tool call(s), want at least %d", got, label, n)
			}
			return nil
		},
	}
}

// NoToolUse asserts the agent ran no tools at all — the negative half of the
// suite. Without it, a platform that ran tools nobody asked for would look
// identical to one that behaved.
func NoToolUse(class Class) Grader {
	return Grader{
		Name:  "no-tool-use",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if uses := eventsOfType(tr, "agent.tool_use"); len(uses) > 0 {
				names := make([]string, 0, len(uses))
				for _, u := range uses {
					names = append(names, fmt.Sprint(u["name"]))
				}
				return fmt.Errorf("%d tool call(s) on a text-only task: %s",
					len(uses), strings.Join(names, ", "))
			}
			return nil
		},
	}
}

// ToolResultOK asserts at least one tool call actually succeeded — the guard
// against a trial that "used tools" by failing at them n times and then
// narrating a plausible answer.
func ToolResultOK(class Class) Grader {
	return Grader{
		Name:  "tool-result-ok",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			results := eventsOfType(tr, "agent.tool_result")
			for _, ev := range results {
				if okResult(ev) {
					return nil
				}
			}
			return fmt.Errorf("none of the %d tool_result(s) succeeded", len(results))
		},
	}
}

// SeparateBashCalls asserts that no single bash command contains all of markers,
// forcing a task whose point is cross-call state to spread its work over separate
// calls. Without it a model could pack an export and the command that depends on
// it into one bash call, satisfying the file and result graders while the
// persistence they exist to test never ran across a call boundary. Doing it in
// one call is the model ignoring the instruction, so a violation is a Model
// finding — and one that must fail the trial, or a broken snapshot could pass.
func SeparateBashCalls(markers ...string) Grader {
	return Grader{
		Name:  "separate-bash-calls",
		Class: Model,
		Check: func(_ *testing.T, tr *Trial) error {
			for _, cmd := range bashCommands(tr) {
				if containsAll(cmd, tr.fillAll(markers)) {
					return fmt.Errorf("one bash call did all of %v in a single command (%q); "+
						"the task needs them in separate calls to exercise cross-call state", markers, cmd)
				}
			}
			return nil
		},
	}
}

// BashCommandWith asserts that some single bash command contains all of markers
// — the positive counterpart to SeparateBashCalls. Where that forbids packing
// steps together, this requires a step to actually happen in bash. shell-state
// uses it to pin that the file was written by a bash call that read the persisted
// variable ("$MARK" and the path in one command), not by the write tool or a
// literal the model carried from a prior call's output: those would satisfy the
// file check without any bash call ever consuming the variable across a call
// boundary, and the persistence would go untested. Following the instruction is
// the model's job, so a miss is a Model finding.
func BashCommandWith(markers ...string) Grader {
	return Grader{
		Name:  "bash-command-with",
		Class: Model,
		Check: func(_ *testing.T, tr *Trial) error {
			for _, cmd := range bashCommands(tr) {
				if containsAll(cmd, tr.fillAll(markers)) {
					return nil
				}
			}
			return fmt.Errorf("no single bash command contained all of %v; "+
				"the task needs the file written by a bash call that used the variable", markers)
		},
	}
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// bashCommands returns the command of each bash tool_use, in order. A bash
// tool_use carries its input as {"command": "..."}.
func bashCommands(tr *Trial) []string {
	var cmds []string
	for _, ev := range eventsOfType(tr, "agent.tool_use") {
		if ev["name"] != "bash" {
			continue
		}
		input, _ := ev["input"].(map[string]any)
		if cmd, _ := input["command"].(string); cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// ContainerAbsent asserts no sandbox was made for a session whose agent called
// no tools — the executor must never provision without a tool_exec.
//
// The check is conditional on zero agent.tool_use, and that is what keeps it a
// clean Platform signal. A model that disobeys a no-tools task and calls bash
// makes the executor provision a container as the correct consequence of a real
// tool call; flagging that as a platform bug would blame us for the model's
// choice. NoToolUse already reports the disobedience, as the Model miss it is,
// so here a container is only a platform fault when nothing asked for one.
func ContainerAbsent(class Class) Grader {
	return Grader{
		Name:  "container-absent",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			if len(eventsOfType(tr, "agent.tool_use")) > 0 {
				return nil
			}
			if containerExists(t, tr.SessionID) {
				return fmt.Errorf("container %s exists for a session that ran no tools",
					containerName(tr.SessionID))
			}
			return nil
		},
	}
}

// FileEquals asserts a sandbox file's exact bytes, {{NONCE}} substituted. Where
// FileLines forgives the trailing newline, this forgives nothing: it is for a
// task whose point is that a file was changed surgically or left untouched, so
// every byte is the assertion.
func FileEquals(path, content string, class Class) Grader {
	return Grader{
		Name:  "file-equals:" + path,
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			raw, err := tr.readFile(t, tr.fill(path))
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			want := tr.fill(content)
			if string(raw) != want {
				return fmt.Errorf("%s = %q, want exactly %q", path, string(raw), want)
			}
			return nil
		},
	}
}

// FileMatches asserts a sandbox file matches a regexp. The trailing newline is
// trimmed first so an anchored pattern (^…$) can pin the whole content without
// spelling the newline. {{NONCE}} is substituted into the pattern before compile.
func FileMatches(path, pattern string, class Class) Grader {
	return Grader{
		Name:  "file-matches:" + path,
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			raw, err := tr.readFile(t, tr.fill(path))
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			re, err := regexp.Compile(tr.fill(pattern))
			if err != nil {
				return fmt.Errorf("bad pattern %q: %w", pattern, err)
			}
			got := strings.TrimRight(string(raw), "\n")
			if !re.MatchString(got) {
				return fmt.Errorf("%s = %q, want it to match %q", path, got, re)
			}
			return nil
		},
	}
}

// ToolNotUsed asserts the agent never called a named tool — the guard for a task
// that must be done with one tool and not another (an edit, not a whole-file
// rewrite). Choosing the wrong tool is usually the model's to answer for, so its
// uses are Model.
func ToolNotUsed(name string, class Class) Grader {
	return Grader{
		Name:  "tool-not-used:" + name,
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if n := countToolUse(tr, name); n > 0 {
				return fmt.Errorf("%s was called %d time(s), want never", name, n)
			}
			return nil
		},
	}
}

// ToolCallResult correlates a tool call to its own result. It finds a call to
// the named tool whose input carries inputSub (nonce substituted), then checks
// that call's joined result: its is_error against wantErr, and its content
// against contentSub (a plain substring, nonce substituted; "" skips it). Tying
// the input to the result is what keeps a task honest — an independent "some bash
// ran the path" plus "some error result said exit 1" can be two unrelated calls,
// which this cannot: it grades the result of the call it found.
//
// Class Either: no matching call is the model not doing as asked, a matching call
// with the wrong result is the platform's tool misbehaving, and the transcript
// cannot separate them.
func ToolCallResult(name, inputSub string, wantErr bool, contentSub string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("tool-call-result:%s:%s", name, inputSub),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			wantIn := tr.fill(inputSub)
			wantContent := tr.fill(contentSub)
			for _, use := range eventsOfType(tr, "agent.tool_use") {
				if use["name"] != name || !strings.Contains(inputJSON(use), wantIn) {
					continue
				}
				id, _ := use["id"].(string)
				res := resultFor(tr, id)
				if res == nil {
					return fmt.Errorf("%s call with %q has no tool_result", name, wantIn)
				}
				gotErr, present := isErrorFlag(res)
				if !present {
					return fmt.Errorf("%s call with %q: result carries no is_error flag (%q)",
						name, wantIn, textOf(res))
				}
				if gotErr != wantErr {
					return fmt.Errorf("%s call with %q: result is_error=%v, want %v (%q)",
						name, wantIn, gotErr, wantErr, textOf(res))
				}
				if wantContent != "" && !strings.Contains(textOf(res), wantContent) {
					return fmt.Errorf("%s call with %q: result %q lacks %q",
						name, wantIn, textOf(res), wantContent)
				}
				return nil
			}
			return fmt.Errorf("no %s call whose input contains %q", name, wantIn)
		},
	}
}

// ToolCalledWith asserts the model called name with every marker in its input.
// It is the Model half of a P/M pair: it owns "the model never made the call",
// which is what lets the Platform-class CallResult beside it stay vacuous on
// that miss instead of blaming the platform for it.
//
// Markers are matched against the decoded input (see inputText), so a marker may
// carry a redirect, a quote or a newline — the things a bash command is made of.
func ToolCalledWith(name string, markers []string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("tool-called-with:%s:%s", name, strings.Join(markers, "|")),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if len(toolCallsWith(tr, name, markers)) > 0 {
				return nil
			}
			return fmt.Errorf("no %s call whose input carries all of %q", name, markers)
		},
	}
}

// CallResult grades the result of a call the model actually made: among the
// calls to name whose input carries every marker (an empty marker list means any
// call to that tool), at least one must have a joined result whose is_error
// matches wantErr and whose content carries sub ("" skips the content check).
//
// It is VACUOUS when no such call exists, and that is the whole point: paired
// with a Model-class ToolCalledWith (or a tool-use floor) that owns the miss,
// the only way it reds is a call that happened and a result that came back
// wrong — so it can be Platform without firing on model non-compliance. Where
// ToolCallResult requires the call and therefore folds the two failures into
// Either, this splits them.
//
// One satisfying call is enough rather than all of them, because how many times
// a model reaches for a tool is its business: a first attempt with a different
// pattern, a retry after a typo. The claim is that the platform demonstrated the
// behavior on the call that asked for it.
func CallResult(name string, markers []string, wantErr bool, sub string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("call-result:%s:%s", name, strings.Join(markers, "|")),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			uses := toolCallsWith(tr, name, markers)
			if len(uses) == 0 {
				return nil
			}
			want := tr.fill(sub)
			var last error
			graded := 0
			for _, use := range uses {
				id, _ := use["id"].(string)
				res := resultFor(tr, id)
				if res == nil {
					// The call never came back. That gap is ToolResultOK's to
					// report; from here the call is simply not gradeable, so
					// move on rather than letting it excuse the siblings.
					continue
				}
				graded++
				gotErr, present := isErrorFlag(res)
				if !present {
					// Terminal, not "try the next call": a later well-formed result
					// says nothing about this one, and letting a retry erase a
					// dropped wire field is the vacuous pass the flag check exists
					// to close.
					return fmt.Errorf("call %s: result carries no is_error flag (%q)", id, textOf(res))
				}
				switch {
				case gotErr != wantErr:
					last = fmt.Errorf("call %s: result is_error=%v, want %v (%q)",
						id, gotErr, wantErr, textOf(res))
				case want != "" && !strings.Contains(textOf(res), want):
					last = fmt.Errorf("call %s: result %q lacks %q", id, textOf(res), want)
				default:
					return nil
				}
			}
			if graded == 0 {
				return nil
			}
			return fmt.Errorf("none of the %d %s call(s) carrying %q produced the expected result; last: %w",
				len(uses), name, markers, last)
		},
	}
}

// GlobPathList asserts every successful glob result opens the way the tool's
// contract says it does: an absolute path, or its own "no matches".
//
// It is the half of glob's output that does not depend on which pattern the
// model chose, which is what makes it a clean Platform check — a result whose
// records are mangled (the mtime prefix leaking through, a NUL split gone
// wrong, a relative path) reds here whatever the model asked for. Which paths
// come back does depend on the pattern, so "the seeded file is among them" is a
// separate, Either-class CallResult.
//
// Vacuous when the model never called glob: the task's tool-use floor owns that.
func GlobPathList(class Class) Grader {
	return Grader{
		Name:  "glob-path-list",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			for _, use := range eventsOfType(tr, "agent.tool_use") {
				if use["name"] != "glob" {
					continue
				}
				id, _ := use["id"].(string)
				res := resultFor(tr, id)
				if res == nil {
					// The core pack's tool-results-joined grader owns the missing
					// answer; there is no list to shape-check here.
					continue
				}
				gotErr, present := isErrorFlag(res)
				if !present {
					return fmt.Errorf("glob call %s: result carries no is_error flag (%q)", id, textOf(res))
				}
				if gotErr {
					// A failed glob is the model's pattern or a real tool error;
					// either way there is no path list to shape-check.
					continue
				}
				out := textOf(res)
				if out == "no matches" {
					continue
				}
				// An empty success is not an empty list — glob says "no matches"
				// for that. Nothing here is the shape of a dropped content block,
				// and a grader that walked zero lines would accept it silently.
				lines := splitLines(out)
				if len(lines) == 0 {
					return fmt.Errorf("glob call %s succeeded with no content, want a path list or %q",
						id, "no matches")
				}
				// The first record only, deliberately. Every regression this can
				// see shows up there — a leaked `<mtime> ` stat prefix, a relative
				// path, an error string returned as a success — and the tool's own
				// contract forbids asserting more: search.go is NUL-delimited end
				// to end precisely because a filename may legally contain a
				// newline, so a later "line" can be the tail of a legitimate path
				// and a per-line check would red the platform for output that is
				// exactly right.
				if !strings.HasPrefix(lines[0], "/") {
					return fmt.Errorf("glob call %s returned %q, whose first record %q is not an absolute path",
						id, out, lines[0])
				}
			}
			return nil
		},
	}
}

// NotInToolTraffic asserts a token never crossed the sandbox boundary — it
// appears in no tool input and in no tool result.
//
// journal-multiturn uses it to keep its recall token honest: the token proves
// event replay only for as long as the filesystem is not a second way to know
// it, so a model that writes it down (or reads it back) must red rather than
// quietly turn a replay test into a persistence test.
func NotInToolTraffic(sub string, class Class) Grader {
	return Grader{
		Name:  "not-in-tool-traffic:" + sub,
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			want := tr.fill(sub)
			for _, ev := range eventsOfType(tr, "agent.tool_use") {
				// The encoded input as well as the decoded values: this grader is
				// the one that must find the token wherever it is, including a
				// place inputText does not look (an object key, a non-string
				// value). Everywhere else the decoded form is what a marker should
				// match; here breadth wins over precision.
				if strings.Contains(inputText(ev), want) || strings.Contains(inputJSON(ev), want) {
					return fmt.Errorf("%v tool call carries %q in its input, so the token is on disk, "+
						"not only in the replayed context", ev["name"], want)
				}
			}
			for _, ev := range eventsOfType(tr, "agent.tool_result") {
				if strings.Contains(textOf(ev), want) {
					return fmt.Errorf("a tool_result carries %q, so the token came back out of the sandbox", want)
				}
			}
			return nil
		},
	}
}

// OnlyIf makes a grader vacuous unless every premise holds — the mechanism
// behind a Platform-class claim that is only the platform's *given* that the
// model did as it was asked.
//
// CallResult builds its own premise in (the call it grades must have happened),
// which covers a claim about that call's result. This is for the other shape: a
// claim about an artifact, whose premise is some earlier call the grader does
// not otherwise look at. shell-state is the case — mark.txt holds the nonce only
// if the model exported it and then wrote it with the variable, and a trial that
// skipped either step is a Model miss, not a broken shell snapshot.
//
// The grader keeps its name and class, so a gated check reports exactly as it
// did before; what changes is when it speaks.
func OnlyIf(g Grader, premises ...func(*Trial) bool) Grader {
	return Grader{
		Name:  g.Name,
		Class: g.Class,
		Check: func(t *testing.T, tr *Trial) error {
			for _, holds := range premises {
				if !holds(tr) {
					return nil
				}
			}
			return g.Check(t, tr)
		},
	}
}

// calledWith is OnlyIf's premise: the model called name with every marker in its
// input. It is ToolCalledWith's condition over the same finder, deliberately —
// pair the two on the same markers and the Platform grader goes vacuous exactly
// when the Model grader beside it reds, so the two can never disagree about
// whether the instructed call happened. A premise that matched a narrower set
// than the Model grader it is paired with would open a window where neither
// fires, which is the one thing a gate must not do.
func calledWith(name string, markers ...string) func(*Trial) bool {
	return func(tr *Trial) bool { return len(toolCallsWith(tr, name, markers)) > 0 }
}

// toolCallsWith returns every agent.tool_use of the named tool whose decoded
// input carries all markers, in log order. An empty marker list matches every
// call to that tool.
func toolCallsWith(tr *Trial, name string, markers []string) []map[string]any {
	want := tr.fillAll(markers)
	var out []map[string]any
	for _, use := range eventsOfType(tr, "agent.tool_use") {
		if use["name"] != name {
			continue
		}
		if containsAll(inputText(use), want) {
			out = append(out, use)
		}
	}
	return out
}

// inputText joins the string values of a tool_use's input, one per line, in key
// order.
//
// It matches a marker against what the model actually wrote, where inputJSON
// matches the encoding of it — and json.Marshal HTML-escapes <, > and & and
// spells a newline \n, so a marker carrying a redirect or a heredoc could never
// match there. A newline between values keeps a single-line marker from
// straddling two fields and matching a pair of them that no one wrote together.
func inputText(ev map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			b.WriteString(t)
			b.WriteByte('\n')
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(t[k])
			}
		}
	}
	walk(ev["input"])
	return b.String()
}

// toolUseByID returns the agent.tool_use with this event id, or nil.
func toolUseByID(tr *Trial, id string) map[string]any {
	for _, use := range eventsOfType(tr, "agent.tool_use") {
		if use["id"] == id {
			return use
		}
	}
	return nil
}

// EventAfterUserMessage asserts an event of evType appears after the nth
// user.message on the log — proof that a later turn actually did something, not
// that turn one did all the work. journal-multiturn uses it to require the second
// turn to touch the sandbox, which a span-count over the whole transcript (turn
// one alone emits two model-request spans) cannot.
func EventAfterUserMessage(evType string, nth int, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("event-after-user-message:%s:%d", evType, nth),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			seen := 0
			for i, ev := range tr.Events {
				if ev["type"] != "user.message" {
					continue
				}
				if seen++; seen < nth {
					continue
				}
				for _, later := range tr.Events[i+1:] {
					if later["type"] == evType {
						return nil
					}
				}
				return fmt.Errorf("no %s after user.message #%d", evType, nth)
			}
			return fmt.Errorf("fewer than %d user.message events", nth)
		},
	}
}

// EventCountAtLeast asserts the transcript holds at least n events of a type. A
// floor, not an exact count: enough to know a resumed session re-invoked the
// model (≥2 model-request spans) or that both turns were recorded, without
// pinning a number a longer transcript would exceed.
func EventCountAtLeast(evType string, n int, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("event-count-at-least:%s:%d", evType, n),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if got := len(eventsOfType(tr, evType)); got < n {
				return fmt.Errorf("%d %s event(s), want at least %d", got, evType, n)
			}
			return nil
		},
	}
}

// RequiresActionRaised asserts the session paused for confirmation — a
// session.status_idle whose stop_reason is requires_action and names the events
// awaiting a decision. It is the platform half of the permission bridge: the gate
// stopped a tool before it ran.
//
// It fires only once a gated tool was actually called. A turn with no tool_use
// had nothing to gate, and a Model-class grader (the task's ToolCalledWith) owns
// "the model never called the tool" — so a Platform failure here means one thing:
// a gated call the bridge failed to suspend.
func RequiresActionRaised(class Class) Grader {
	return Grader{
		Name:  "requires-action-raised",
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if len(eventsOfType(tr, "agent.tool_use")) == 0 {
				return nil
			}
			for _, ev := range eventsOfType(tr, "session.status_idle") {
				stop, _ := ev["stop_reason"].(map[string]any)
				if stop["type"] != "requires_action" {
					continue
				}
				ids, _ := stop["event_ids"].([]any)
				if len(ids) == 0 {
					return fmt.Errorf("requires_action idle carried no event_ids")
				}
				for _, raw := range ids {
					if s, ok := raw.(string); !ok || s == "" {
						return fmt.Errorf("requires_action idle carried a non-string or empty event id %v", raw)
					}
				}
				return nil
			}
			return fmt.Errorf("no session.status_idle with stop_reason requires_action")
		},
	}
}

// EvaluatedPermissionAsk asserts a call to the named tool was recorded as gated —
// evaluated_permission "ask" on the agent.tool_use. It pins that the tool the
// task gated is the one the platform stopped.
//
// Like RequiresActionRaised it fires only when the tool was actually called: no
// call means nothing to check (a Model grader owns the skip), and a Platform
// failure means the tool ran without the "ask" the gate should have stamped.
//
// Every call is checked, not just the first. The toolset gates by name, so a
// second call to the same tool that came back unstamped is the same defect as a
// first one — and a gate that only held for the opening call is exactly the
// shape a first-call-only assertion would miss.
func EvaluatedPermissionAsk(name string, class Class) Grader {
	return Grader{
		Name:  "evaluated-permission-ask:" + name,
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			for _, ev := range eventsOfType(tr, "agent.tool_use") {
				if ev["name"] != name {
					continue
				}
				if ev["evaluated_permission"] != "ask" {
					return fmt.Errorf("%s tool_use %v evaluated_permission = %v, want ask",
						name, ev["id"], ev["evaluated_permission"])
				}
			}
			return nil
		},
	}
}

// ConfirmedResult is the join issue #99 asked for: the call the task means
// (name plus markers in its decoded input), to *its* confirmation, to *that*
// call's result — which must carry is_error == wantErr and contain contentSub
// ("" skips the content check), sequenced after the confirmation. Reading
// position in the log is sound: the list endpoint returns events in commit
// order. Correlating from the confirmation forward instead, as this once did,
// grades whichever call the bridge happened to stop rather than the one the task
// gated.
//
// One confirmed call satisfying the claim is enough, matching CallResult. An
// earlier revision required every confirmed matching call to satisfy it, and
// that was a false-red generator on a paid live suite: the toolset gates every
// tool, so a model that writes the file and then *verifies* it with a second
// command carrying the same marker gets a second confirmed result, and a
// verification that exits non-zero would have failed the trial with the platform
// behaving perfectly.
//
// It is vacuous when nothing was confirmed, when the model never made the call
// the task described, and when no matching call was confirmed. The last one is
// the subtle case, and it is safe only because sibling graders own that window
// task by task — a general claim that "EvaluatedPermissionAsk covers it" is not
// true, since that grader checks the permission *stamp* and not that a
// suspension happened. In perm-allow the gated call must produce its file
// (FileLines) and carry the stamp; in perm-deny the seeded file must be
// *unchanged* (FileLines, Platform), so an unconfirmed append that actually runs
// reds there, and the stamp is checked too. Blaming the platform here instead
// would misread the harness giving up on a model that re-pauses past
// maxConfirmRounds (harness_test.go) as a bridge fault.
//
// The one thing it asserts about confirmations it does not grade: each must name
// an agent.tool_use that is on the log. The harness confirms whatever event id
// requires_action listed (see driveToIdle), so a gate that named some other
// event arrives here as a confirmation pointing at nothing — and correlating
// forward could never see it, because the platform would answer the id it was
// handed and look consistent.
func ConfirmedResult(name string, markers []string, wantErr bool, contentSub string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("confirmed-result:%s:%s", name, strings.Join(markers, "|")),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			// Where each confirmed call's confirmation sits, and the check that it
			// names a real call. First occurrence wins: the harness confirms an id
			// once, and grading a duplicate against its own later index would
			// demand a second result nobody promised.
			confirmedAt := map[string]int{}
			for i, ev := range tr.Events {
				if ev["type"] != "user.tool_confirmation" {
					continue
				}
				tid, _ := ev["tool_use_id"].(string)
				if tid == "" {
					return fmt.Errorf("user.tool_confirmation carries no tool_use_id")
				}
				if toolUseByID(tr, tid) == nil {
					return fmt.Errorf("user.tool_confirmation names %s, which is no agent.tool_use on the log: "+
						"the gate asked about an event that is not the call it stopped", tid)
				}
				if _, seen := confirmedAt[tid]; !seen {
					confirmedAt[tid] = i
				}
			}
			if len(confirmedAt) == 0 {
				return nil
			}

			wantContent := tr.fill(contentSub)
			var last error
			for _, use := range toolCallsWith(tr, name, markers) {
				id, _ := use["id"].(string)
				at, ok := confirmedAt[id]
				if !ok {
					continue
				}
				res := resultAfter(tr, at, id)
				if res == nil {
					last = fmt.Errorf("no agent.tool_result for confirmed %s after its confirmation", id)
					continue
				}
				gotErr, present := isErrorFlag(res)
				if !present {
					// Terminal, like CallResult's: a later well-formed result says
					// nothing about this one, and letting a retry erase a dropped
					// wire field is the vacuous pass the flag check exists to close.
					return fmt.Errorf("result for confirmed %s carries no is_error flag (content %q)",
						id, textOf(res))
				}
				switch {
				case gotErr != wantErr:
					last = fmt.Errorf("result for confirmed %s: is_error=%v, want %v (content %q)",
						id, gotErr, wantErr, textOf(res))
				case wantContent != "" && !strings.Contains(textOf(res), wantContent):
					last = fmt.Errorf("result for confirmed %s does not contain %q (got %q)",
						id, wantContent, textOf(res))
				default:
					return nil
				}
			}
			return last
		},
	}
}

// resultAfter returns the first agent.tool_result for a tool_use id committed
// after position at, or nil.
func resultAfter(tr *Trial, at int, id string) map[string]any {
	for _, ev := range tr.Events[at+1:] {
		if ev["type"] == "agent.tool_result" && ev["tool_use_id"] == id {
			return ev
		}
	}
	return nil
}

// ReadRangeRequested asserts the model asked to read exactly line..line of path
// via view_range — the model half of the view-range task, reading the line the
// prompt named. A miss is the model not following the instruction, so it is Model.
func ReadRangeRequested(path string, line int, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("read-range-requested:%s:%d", path, line),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			if readRangeUse(tr, path, line) != nil {
				return nil
			}
			return fmt.Errorf("no read requested %s view_range [%d,%d]", path, line, line)
		},
	}
}

// ReadRangeBytes asserts that IF such a read was made, it returned the line's
// bytes verbatim — the off-by-one guard for view_range slicing. A matching read
// that returns the neighbouring line or a stray newline is unambiguously the
// platform's slicer, which is why this is Platform. The "no such read" case is
// the model's and belongs to ReadRangeRequested, so this passes when no matching
// read exists rather than blaming the platform for a line the model never read.
func ReadRangeBytes(path string, line int, want string, class Class) Grader {
	return Grader{
		Name:  fmt.Sprintf("read-range-bytes:%s:%d", path, line),
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			use := readRangeUse(tr, path, line)
			if use == nil {
				return nil
			}
			id, _ := use["id"].(string)
			res := resultFor(tr, id)
			if res == nil {
				return fmt.Errorf("read of %s line %d has no tool_result", path, line)
			}
			if !okResult(res) {
				return fmt.Errorf("read of %s line %d errored or malformed: %q", path, line, textOf(res))
			}
			if got, wantText := textOf(res), tr.fill(want); got != wantText {
				return fmt.Errorf("read %s view_range [%d,%d] returned %q, want %q",
					path, line, line, got, wantText)
			}
			return nil
		},
	}
}

// readRangeUse returns the first read tool_use requesting exactly [line, line] of
// path, or nil. The file_path must be either the workspace-relative path or its
// canonical absolute form under the sandbox workdir — a loose suffix match would
// accept a wrong-root read of "/tmp/poem.txt" and then blame the platform when its
// bytes fail ReadRangeBytes, misclassing the model's wrong path as a slicer bug.
func readRangeUse(tr *Trial, path string, line int) map[string]any {
	abs := sandbox.DefaultWorkdir + "/" + path
	for _, use := range eventsOfType(tr, "agent.tool_use") {
		if use["name"] != "read" {
			continue
		}
		input, _ := use["input"].(map[string]any)
		if p, _ := input["file_path"].(string); p != path && p != abs {
			continue
		}
		if viewRangeIs(input["view_range"], line) {
			return use
		}
	}
	return nil
}

// --- transcript accessors -------------------------------------------------
//
// All of these read the raw wire JSON. A missing or reshaped field surfaces as
// a grader failure naming what it saw, which is the outcome we want: the eval
// noticing that the wire changed.

func eventsOfType(tr *Trial, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range tr.Events {
		if ev["type"] == typ {
			out = append(out, ev)
		}
	}
	return out
}

func stopReasonType(idle map[string]any) string {
	stop, _ := idle["stop_reason"].(map[string]any)
	s, _ := stop["type"].(string)
	return s
}

// finalMessage concatenates the text blocks of the last agent.message.
func finalMessage(tr *Trial) string {
	msgs := eventsOfType(tr, "agent.message")
	if len(msgs) == 0 {
		return ""
	}
	return textOf(msgs[len(msgs)-1])
}

// textOf joins an event's text content blocks. Non-text blocks are skipped
// rather than rendered: a grader looking for a demanded token has nothing to say
// about an image.
func textOf(ev map[string]any) string {
	blocks, _ := ev["content"].([]any)
	var b strings.Builder
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] != "text" {
			continue
		}
		s, _ := block["text"].(string)
		b.WriteString(s)
	}
	return b.String()
}

// isErrorFlag returns a tool_result's is_error and whether it was present. The
// platform always stamps the flag, so a missing one is a malformed result: an
// assertion that pins is_error in *either* direction must reject the absence
// rather than read it as a zero-value false, which is how a dropped-flag wire
// regression would otherwise sail through the wantErr==false direction of a
// correlated grader.
func isErrorFlag(ev map[string]any) (isErr, present bool) {
	b, ok := ev["is_error"].(bool)
	return b, ok
}

// okResult reports whether a tool_result is an explicit success — is_error
// present and false. A missing or non-boolean is_error is not a success: the
// platform always stamps the flag, so its absence is a malformed result a
// success-requiring grader must reject rather than read as a zero-value false.
func okResult(ev map[string]any) bool {
	isErr, present := isErrorFlag(ev)
	return present && !isErr
}

// modelUsage pulls the token counts off a span.model_request_end event. in is
// the total input across the fresh and cached counters; ok is false only when
// the event carries no model_usage object at all — the shape the usage-accounted
// grader rejects and sumTokens skips. One spelling of the wire access, so the
// grader and the report cannot drift on a field rename.
//
// Summing the cached counters into in is what keeps a valid fully-cached turn
// from reading as broken: the OpenAI adapter splits cached tokens out, so such a
// turn has input_tokens 0 while cache_read_input_tokens carries the real count.
// A spend report wants the total too.
func modelUsage(ev map[string]any) (in, out float64, ok bool) {
	usage, ok := ev["model_usage"].(map[string]any)
	if !ok {
		return 0, 0, false
	}
	fresh, _ := usage["input_tokens"].(float64)
	cacheRead, _ := usage["cache_read_input_tokens"].(float64)
	cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
	out, _ = usage["output_tokens"].(float64)
	return fresh + cacheRead + cacheCreate, out, true
}

func countToolUse(tr *Trial, name string) int {
	n := 0
	for _, ev := range eventsOfType(tr, "agent.tool_use") {
		if name == "" || ev["name"] == name {
			n++
		}
	}
	return n
}

// inputJSON re-encodes a tool_use's input object. A grader matching a substring
// of the input works on the JSON rather than reaching for one field, so it does
// not have to know which key (command, file_path) a given tool carries the token
// in.
func inputJSON(ev map[string]any) string {
	b, err := json.Marshal(ev["input"])
	if err != nil {
		return ""
	}
	return string(b)
}

// resultFor returns the agent.tool_result joined to a tool_use id, or nil. The
// core pack's tool-results-joined grader proves the join is one-to-one; this only
// fetches it.
func resultFor(tr *Trial, id string) map[string]any {
	if id == "" {
		return nil
	}
	for _, ev := range eventsOfType(tr, "agent.tool_result") {
		if ev["tool_use_id"] == id {
			return ev
		}
	}
	return nil
}

// viewRangeIs reports whether a decoded view_range input is exactly [line, line].
// The wire carries the bounds as JSON numbers, so they arrive as float64.
func viewRangeIs(v any, line int) bool {
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		return false
	}
	start, ok1 := arr[0].(float64)
	end, ok2 := arr[1].(float64)
	// Exact equality, not int() truncation: a range like [57.5, 57.5] is not a
	// request for line 57 and must not be graded as one.
	return ok1 && ok2 && start == float64(line) && end == float64(line)
}
