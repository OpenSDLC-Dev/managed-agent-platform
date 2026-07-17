package evals

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Grading is deliberately code-based: every assertion below is a deterministic
// function of the transcript, the sandbox filesystem, or the session status. No
// LLM judge. A judge would be a second model whose own drift is indistinguishable
// from the drift this suite exists to detect, and none of these tasks needs one —
// they are engineered so that correct behavior leaves a mechanical trace.
//
// The trick that makes that possible is the nonce (see subst): prompts demand an
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

// FileLines asserts a sandbox file's exact lines, in order. Each want entry may
// carry {{NONCE}}.
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
			raw, err := tr.readFile(t, path)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			got := splitLines(string(raw))
			w := make([]string, len(want))
			for i, s := range want {
				w[i] = subst(s, tr.Nonce)
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
			want := subst(sub, tr.Nonce)
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

// ToolResultContains asserts some non-error tool_result carries sub.
//
// It is how a task proves a value came from the sandbox rather than from the
// model's imagination: the nonce it looks for exists only in a file the agent
// had to actually read.
func ToolResultContains(sub string, class Class) Grader {
	return Grader{
		Name:  "tool-result-contains:" + sub,
		Class: class,
		Check: func(_ *testing.T, tr *Trial) error {
			want := subst(sub, tr.Nonce)
			for _, ev := range eventsOfType(tr, "agent.tool_result") {
				if isErrorResult(ev) {
					continue
				}
				if strings.Contains(textOf(ev), want) {
					return nil
				}
			}
			return fmt.Errorf("no successful tool_result contains %q", want)
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
				if !isErrorResult(ev) {
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
				if containsAll(cmd, markers) {
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
				if containsAll(cmd, markers) {
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

// ContainerAbsent asserts no sandbox was ever made for the session — the
// executor must not provision on a session whose agent called no tools.
func ContainerAbsent(class Class) Grader {
	return Grader{
		Name:  "container-absent",
		Class: class,
		Check: func(t *testing.T, tr *Trial) error {
			if containerExists(t, tr.SessionID) {
				return fmt.Errorf("container %s exists for a session that ran no tools",
					containerName(tr.SessionID))
			}
			return nil
		},
	}
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

func isErrorResult(ev map[string]any) bool {
	b, _ := ev["is_error"].(bool)
	return b
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
