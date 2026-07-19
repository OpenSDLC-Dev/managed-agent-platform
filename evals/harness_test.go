package evals

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// turnTimeout bounds one user.message → session.status_idle round trip. It is
// generous because the thing being waited on is a real model plus real
// containers, and a flaky-looking eval is worse than a slow one: the timeout
// exists to stop a hung run, not to measure the agent.
const turnTimeout = 5 * time.Minute

// maxConfirmRounds caps how many requires_action pauses one turn may raise before
// the harness gives up. Our permission tasks gate a single call, so one round is
// the norm and a partial re-idle adds a few more at most; a turn that keeps
// re-pausing past this — a model retrying a denied tool forever — would otherwise
// loop until the whole-suite timeout, turning a model misbehaviour into an opaque
// hang. The cap fails it fast, with a message that names the cause.
const maxConfirmRounds = 8

// Task is one eval: a prompt (or several) and what must be true afterwards.
// Fields left zero mean "the default agent" — no system prompt, the bare
// toolset, no seeds.
type Task struct {
	ID string
	// System is the agent's system prompt. {{NONCE}} is substituted.
	System string
	// Tools overrides the agent's toolset. Nil is the bare
	// agent_toolset_20260401, whose default policy runs every tool unattended; a
	// task that needs a gated tool (a permission round-trip) supplies the tools
	// array verbatim so the wire shape it exercises is the one under test.
	Tools []any
	// Seeds are files planted in the sandbox before the first turn. The harness
	// pre-provisions the session's container and writes them; the executor
	// adopts that same container when it runs the first tool. {{NONCE}} is
	// substituted into Path and Content.
	Seeds []Seed
	// Turns are the user messages, sent one at a time; each waits for the
	// session to go idle before the next is sent.
	Turns []Turn
	// Graders run after the last turn. The core pack (see grade_test.go) is
	// prepended to every task's list.
	Graders []Grader
}

// Seed is a file planted before turn 1. Path may be relative (resolved against
// the sandbox workdir) or absolute.
type Seed struct {
	Path    string
	Content string
}

// Turn is one user message. Both per-trial tokens ({{NONCE}} and {{RECALL}})
// are substituted into Message; see fill.
type Turn struct {
	Message string
	// OnAsk answers a confirmation pause. Nil means the turn expects none; a
	// gated turn supplies whether to allow or deny each gated tool call.
	OnAsk *Ask
}

// Ask is how a turn answers a requires_action pause. DenyMessage (with
// {{NONCE}} substituted) rides a denial back as the synthesized error result,
// and is ignored on an allow.
type Ask struct {
	Allow       bool
	DenyMessage string
}

// Trial is one execution of a Task: everything a grader is allowed to look at.
// It is deliberately raw — the transcript as the server framed it, the idles as
// the stream delivered them — so a grader asserts on the product's output and
// not on the harness's interpretation of it.
type Trial struct {
	Task  Task
	Nonce string
	// Recall is a second per-trial token, independent of Nonce. A task that
	// needs a value the model can only have from its replayed context states it
	// in one turn as {{RECALL}} and is asked for it in a later one; deriving it
	// from the nonce would not do, because the nonce is in every turn's prompt
	// and a model could spell the derivation without ever having seen the
	// earlier message.
	Recall    string
	SessionID string
	// Events is the whole transcript, read back through the list endpoint
	// after the last turn.
	Events []map[string]any
	// Idles is every session.status_idle observed on the SSE stream, in order,
	// one per turn. That they came off the stream rather than the list endpoint
	// is what makes SSE part of every trial's evidence.
	Idles []map[string]any
	// Elapsed is wall-clock across all turns, for the report. It is never
	// graded: a threshold on a real model's latency is a flake generator.
	Elapsed time.Duration

	stack *stack
}

// runTrial drives one task to completion and returns what happened. It fails
// the test only on harness or platform faults that make grading meaningless (a
// session that never goes idle, an API call that errors). Everything a grader
// can express — including the agent doing the wrong thing — is left to the
// graders, so a failure names the behavior rather than "the harness gave up".
//
// rec is the caller's record for this trial; runTrial stamps its Session id the
// moment the session exists so that a drive that fatals (a turn that never
// idles) still leaves the record pointing at the session to investigate.
func runTrial(t *testing.T, s *stack, task Task, rec *record) *Trial {
	t.Helper()
	nonce := newNonce(t)
	tr := &Trial{Task: task, Nonce: nonce, Recall: newNonce(t), stack: s}

	agentID := s.createAgent(t, agentBody(task, s.model, tr))
	envID := s.createEnvironment(t, "eval-"+task.ID)
	tr.SessionID = s.createSession(t, agentID, envID)
	rec.Session = tr.SessionID

	// Seeds are planted before the stream opens and the first turn is sent, so
	// the files are already there when the agent's first tool runs. Seeding
	// pre-provisions the container; the executor adopts that same one (see seed).
	if len(task.Seeds) > 0 {
		tr.seed(t, task.Seeds)
	}

	// The session's container is reaped at stack teardown, not here: a trial
	// that times out with its model call still in flight would, if reaped now,
	// have its tool result land on the still-running brain and the executor
	// re-provision a container after the reap — an orphan nothing then removes.
	// The stack reaps every session's container only after the loops are stopped,
	// when no re-provision is possible (see newStack).

	// Subscribe before the first message: the stream is a live tail with no
	// cursor, so anything committed before this call is not on it.
	stream := s.openStream(t, tr.SessionID)

	start := time.Now()
	for i, turn := range task.Turns {
		s.sendEvents(t, tr.SessionID, userMessage(tr.fill(turn.Message)))
		idle := s.driveToIdle(t, stream, turn, tr, i, len(task.Turns))
		tr.Idles = append(tr.Idles, idle)
		// An unresolved pause (driveToIdle returning a requires_action idle) leaves
		// the session wedged on a confirmation we won't give. Stop driving and let
		// the graders classify this idle, rather than post the next turn into a
		// stuck session where it would only time out.
		if stopReasonType(idle) == "requires_action" {
			break
		}
	}
	tr.Elapsed = time.Since(start)

	tr.Events = s.listEvents(t, tr.SessionID)
	return tr
}

// driveToIdle waits for one turn to settle, answering any confirmation pause on
// the way. A gated toolset suspends the session on a requires_action idle rather
// than ending the turn; the turn's OnAsk says whether to allow or deny each
// gated call, and the loop keeps confirming until an idle that is not a pause —
// the turn's real end, which is what it returns. That idle joins tr.Idles (one
// per turn, as the core pack expects); the intermediate requires_action idles are
// evidence the permission graders read back from the transcript.
//
// When it *cannot* keep driving — the turn set no OnAsk yet the session paused, or
// a model kept re-pausing past maxConfirmRounds, or the pause named no usable
// event id — it returns that requires_action idle rather than aborting. runTrial
// then stops driving and the graders classify it: the core pack's
// ends-with-end-turn (Either) reds a non-end_turn final idle, and a permission
// task's RequiresActionRaised (Platform) reds a malformed pause. A t.Fatalf here
// would instead misread a platform gating regression as a forgotten test OnAsk,
// or a re-pausing model as a platform abort, and skip P/M/E classing entirely.
// Only a stream that never produces an idle (the turn-wide deadline) is fatal.
func (s *stack) driveToIdle(t *testing.T, stream *sseStream, turn Turn, tr *Trial, turnIdx, turns int) map[string]any {
	t.Helper()
	// Every gated call answered so far this turn. The API re-idles after a
	// partial confirmation with only the *remaining* ids, so a requires_action
	// idle can list ids already confirmed — and confirming one twice is a 400.
	// Tracking them, and confirming each id exactly once, makes the loop correct
	// whether a turn gates one call or several (and however the intermediate
	// re-idles interleave on the stream).
	answered := map[string]bool{}
	rounds := 0
	// One deadline for the whole turn, not one per await: a model that re-pauses
	// repeatedly cannot stretch the turn to maxConfirmRounds × turnTimeout and slip
	// past the suite timeout before the round cap fires.
	deadline := time.Now().Add(turnTimeout)
	for {
		idle, err := stream.awaitIdle(time.Until(deadline))
		if err != nil {
			t.Fatalf("turn %d of %d: %v (session %s)", turnIdx+1, turns, err, tr.SessionID)
		}
		if stopReasonType(idle) != "requires_action" {
			return idle
		}
		// An unanswerable pause is graded, not aborted (see the doc comment).
		if turn.OnAsk == nil {
			return idle
		}
		if rounds++; rounds > maxConfirmRounds {
			return idle
		}
		stop, _ := idle["stop_reason"].(map[string]any)
		ids, _ := stop["event_ids"].([]any)
		// Confirm each not-yet-answered id, in one POST, referencing it by the
		// event id requires_action named (the id the API's blocking-set query
		// recognizes). A re-idle listing only already-answered ids adds nothing to
		// send, so the loop simply waits for the turn to resume. A pause carrying no
		// id or a malformed one is returned for RequiresActionRaised to class.
		if len(ids) == 0 {
			return idle
		}
		var confirmations []map[string]any
		for _, raw := range ids {
			eid, ok := raw.(string)
			if !ok || eid == "" {
				return idle
			}
			if answered[eid] {
				continue
			}
			answered[eid] = true
			confirmations = append(confirmations, toolConfirmation(eid, turn.OnAsk, tr))
		}
		if len(confirmations) > 0 {
			s.sendEvents(t, tr.SessionID, confirmations...)
		}
	}
}

// agentBody builds the create-agent request. The agent's model is set to
// MODEL_ID: the registry's single route sends MODEL_ID upstream whatever the
// agent says, so any other string here would be a fiction the transcript then
// records, naming a model the endpoint never saw.
func agentBody(task Task, model string, tr *Trial) map[string]any {
	tools := task.Tools
	if tools == nil {
		// The bare agent toolset, whose default permission policy runs every
		// tool unattended. A task that needs a gated toolset (a permission
		// round-trip) supplies task.Tools instead.
		tools = []any{map[string]any{"type": "agent_toolset_20260401"}}
	}
	body := map[string]any{
		"name":  "eval-" + task.ID,
		"model": model,
		"tools": tools,
	}
	if task.System != "" {
		body["system"] = tr.fill(task.System)
	}
	return body
}

// toolConfirmation answers one gated tool call. It references the call by the
// event id requires_action listed (the id the API's blocking-set query
// recognizes), and a denial carries a message the platform echoes back as the
// synthesized error result.
func toolConfirmation(toolUseID string, ask *Ask, tr *Trial) map[string]any {
	ev := map[string]any{"type": "user.tool_confirmation", "tool_use_id": toolUseID}
	if ask.Allow {
		ev["result"] = "allow"
		return ev
	}
	ev["result"] = "deny"
	if ask.DenyMessage != "" {
		// deny_message is rejected with an allow, so it is set only here.
		ev["deny_message"] = tr.fill(ask.DenyMessage)
	}
	return ev
}

// readFile reads a path out of the trial's sandbox for an [fs] grader.
//
// It provisions to get a handle, which for every task that reaches here is an
// adopt of the container the executor already made. A task whose agent never
// ran a tool has no container, and this would create an empty one — which is
// why "no container exists" is asserted with containerExists below rather than
// by a read that would destroy the evidence as it looked for it.
func (tr *Trial) readFile(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	ctx := context.Background()
	sb, err := tr.stack.sbx.Provision(ctx, sandbox.Spec{
		SessionID:  domain.ID(tr.SessionID),
		Image:      evalImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("adopt the sandbox to grade it: %v", err)
	}
	if !strings.HasPrefix(path, "/") {
		path = sandbox.DefaultWorkdir + "/" + path
	}
	return sb.ReadFile(ctx, path)
}

// seed plants a task's files before turn 1. Like readFile it provisions to get a
// handle, which here creates the session's container; the executor adopts that
// same container (by session label) when it runs the first tool, so the agent
// sees the seeded files. {{NONCE}} is substituted into every path and body so a
// seed can carry the per-trial token a grader later checks.
func (tr *Trial) seed(t *testing.T, seeds []Seed) {
	t.Helper()
	ctx := context.Background()
	sb, err := tr.stack.sbx.Provision(ctx, sandbox.Spec{
		SessionID:  domain.ID(tr.SessionID),
		Image:      evalImage,
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	if err != nil {
		t.Fatalf("provision the sandbox to seed it: %v", err)
	}
	for _, sd := range seeds {
		path := tr.fill(sd.Path)
		if !strings.HasPrefix(path, "/") {
			path = sandbox.DefaultWorkdir + "/" + path
		}
		if err := sb.WriteFile(ctx, path, []byte(tr.fill(sd.Content))); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
}

// containerName mirrors the docker provider's naming. It is duplicated rather
// than exported from there because a test asserting on a name the provider
// happens to hand it would assert nothing; this is the harness stating,
// independently, where it expects the container to be.
func containerName(sessionID string) string { return "map-" + sessionID }

// containerExists asks Docker directly. Going through the CLI rather than the
// sandbox provider is the point: Provision is the only entry the provider
// offers and it creates what it cannot find, so it can never answer this
// question — it would make the answer "yes" on its way to reporting it.
func containerExists(t *testing.T, sessionID string) bool {
	t.Helper()
	name := containerName(sessionID)
	out, err := exec.Command("docker", "ps", "--all",
		"--filter", "name=^"+name+"$", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("ask docker whether %s exists: %v", name, err)
	}
	return strings.TrimSpace(string(out)) == name
}

// reap removes the trial's container. Best effort by design: it runs from a
// cleanup, where the container may never have existed (a text-only task, an
// early failure), and where a docker hiccup must not turn a passing trial red.
//
// Only a run killed outright (SIGKILL of the test binary) skips these cleanups
// and leaks a container; `docker ps --filter name=map-sesn_` lists the orphans
// for a manual `docker rm`. There is no blanket sweep target, because a
// developer's local compose stack names its session containers the same way and
// a name filter cannot tell the two apart.
func reap(sessionID string) {
	_ = exec.Command("docker", "rm", "--force", "--volumes", containerName(sessionID)).Run()
}

// fill substitutes this trial's tokens into a prompt, a seed or an expectation.
// It is the ONE substituter: every string a task author writes goes through it,
// prompts and grader expectations alike, so a token can never be live on one
// side of a check and literal on the other. (An earlier revision of this file
// left the nonce on its own helper and taught only the graders that needed it
// about {{RECALL}}; the live suite caught the result immediately — the model
// said the code word back and the grader, still looking for the unsubstituted
// placeholder, red anyway.)
//
// Every trial gets a fresh {{NONCE}}, and it is what makes a final-message
// assertion mean anything: a task whose expected answer is a constant could be
// passed by a model that had seen the task before, by a cached response, or by a
// harness bug that replayed an earlier session. A random token demanded by this
// trial's prompt can only come from this trial's agent. {{RECALL}} is the second
// token, for the one task that needs a value the model can only have from its
// replayed context (see journalMultiturn).
//
// An unset Recall leaves {{RECALL}} standing rather than substituting the empty
// string, and that is deliberate: every consumer of a filled string is a
// substring check, and `strings.Contains(anything, "")` is true, so an empty
// substitution would green a recall assertion against any text at all. The
// placeholder left standing at least fails closed for a grader-only trial, where
// nothing put the literal in front of the model.
//
// It is a backstop, not the guarantee: runTrial sets both tokens unconditionally
// (a trial that reached a prompt has a real recall token), and if one ever did
// not, the literal would go out in the prompt and could come back in the reply.
// The guarantee is that the harness always fills both.
func (tr *Trial) fill(s string) string {
	out := strings.ReplaceAll(s, "{{NONCE}}", tr.Nonce)
	if tr.Recall == "" {
		return out
	}
	return strings.ReplaceAll(out, "{{RECALL}}", tr.Recall)
}

func newNonce(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate a nonce: %v", err)
	}
	return hex.EncodeToString(b[:])
}
