package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	sdk "github.com/anthropics/anthropic-sdk-go"
)

// ToolExecConfig is the sandbox shape a worker provisions for a session's tools.
// A self_hosted environment's wire config carries no image (the sandbox image is
// a deployment choice, not part of the domain model), so Image and Workdir come
// from the worker's own configuration — mirroring the platform executor's
// Config. Networking is the session's egress policy, read from the session's
// environment and threaded in by the caller.
type ToolExecConfig struct {
	Image      string
	Workdir    string
	Networking domain.Networking
	// Hardening is the containment the worker's sandboxes are created with, the
	// BYOC twin of the platform executor's (#65). cmd/worker resolves it from
	// the same environment variables the executor reads, so a customer-hosted
	// sandbox is capped the same way a platform-managed one is.
	Hardening sandbox.Hardening
	// Coordinator is set when the session's snapshot carries a multiagent
	// roster, which the caller reads with its liveness gate. It is the whole of
	// what this thread-unaware driver knows about threads (plan 35 decision 13
	// iv): it widens the scan to the log's start and makes the run re-scan
	// until nothing is left, because concurrent threads can commit calls while
	// this item is live and their enqueue is a no-op against it.
	Coordinator bool
	// Progress, when set, is called each time the run finishes a step — the
	// sandbox provisioned, the skills or files materialized, a tool answered.
	// The lease loop watches it to tell a long run from a wedged one (#383);
	// a caller that runs the driver directly leaves it nil.
	Progress func()
}

// toolUse is one unanswered agent.tool_use the worker must run: the tool-use
// event's id (which the result references and which scopes the bash shell's
// per-call state), the tool name, and its input.
type toolUse struct {
	id    domain.ID
	name  string
	input json.RawMessage
}

// RunSessionTools is the BYOC worker's tool-exec driver: given a session whose
// turn has suspended for built-in tool calls, it runs every runnable tool call
// in the session's sandbox and posts a user.tool_result for each back through
// the session events API. It is the self_hosted twin of the platform executor's
// per-item processing, with two deployment differences: the transport is HTTP
// (the worker has no database), and the result event is user.tool_result, not
// agent.tool_result — the control plane resumes the brain when a result
// completes the outstanding set, so the worker never enqueues a turn itself.
//
// cfg.Coordinator makes the run multi-pass: on a session whose threads run
// concurrently, one pass is not the item's whole work, because a sibling
// thread's calls can land while this pass runs and its enqueue is a no-op
// against the live item. So the found set is answered and the scan repeated
// until one comes back empty, over the sandbox the first pass provisioned. On a
// single-agent session the driver keeps its single pass exactly, the window
// there being closed by the session's own serialization.
//
// Results are posted per tool as each completes, so a backend fault partway
// through leaves the tools that did run answered on the log; a reclaiming pass
// re-derives only the still-unanswered ones. This matches the executor's
// partial-commit-on-fault: a tool-level failure (missing file, nonzero exit)
// still yields a result the model must see, and only a backend fault (sandbox
// gone) stops the set with the rest left for the reclaim.
//
// The sandbox is provisioned only when there is unanswered work, so a call
// against an already-answered session (a redundant reclaim) is one bounded read
// with nothing to run.
//
// Session liveness is the caller's gate, not this driver's. The platform
// executor refuses to run a stale session's tools by loading its status under
// the session row lock (executor.sessionForRun) before provisioning — but it
// does so in its per-item orchestration, not in its runTools core, which this
// driver is the analog of. The BYOC caller (the lease loop, PR C2b) owns the
// same session load: it must read the session (for the same reason it must load
// the egress policy this cfg.Networking carries) and skip a session that is not
// running or is archived, mirroring sessionForRun. The control plane is only a
// partial backstop here — a post to an archived session is refused (400), but a
// post to a merely not-running one appends without resuming — so the complete
// gate belongs in the caller, not in a reliance on the append being rejected.
func RunSessionTools(ctx context.Context, client sdk.Client, provider sandbox.Provider, sessionID string, cfg ToolExecConfig) error {
	// Called at each step below, so the caller's stall guard measures the run's
	// silence rather than its length (#383). Nil-safe: a caller that does not
	// watch the run passes none. Named report, not progress — the package's
	// progress type is the tracker this ends up feeding.
	report := cfg.Progress
	if report == nil {
		report = func() {}
	}
	// Before the scan, not after the caller: whatever the caller did last — the
	// session liveness read, a wire round trip of its own — ends here, so it does
	// not share a silent interval with the paging scan below. The budget's floor
	// covers neither (#383).
	report()
	uses, err := unansweredToolUses(ctx, client, sessionID, cfg.Coordinator, report)
	if err != nil {
		return err
	}
	if len(uses) == 0 {
		return nil
	}
	// The scan pages over the wire, and provisioning below can pull a cold
	// image: two steps the budget must clear one at a time, not together.
	report()
	sb, err := provider.Provision(ctx, sandbox.Spec{
		SessionID:  domain.ID(sessionID),
		Image:      cfg.Image,
		Workdir:    cfg.Workdir,
		Networking: cfg.Networking,
		Hardening:  cfg.Hardening,
	})
	if err != nil {
		return fmt.Errorf("provision sandbox: %w", err)
	}
	report()
	if err := SetupSkills(ctx, client, sessionID, sb, cfg.Workdir, report); err != nil {
		return err
	}
	report()
	if err := SetupFiles(ctx, client, sessionID, sb, cfg.Workdir, report); err != nil {
		return err
	}
	report()
	runner := toolset.Runner{Sandbox: sb, Session: domain.ID(sessionID), Workdir: cfg.Workdir}
	for {
		for _, u := range uses {
			res, err := runner.Run(ctx, u.id, u.name, u.input)
			if err != nil {
				// Backend fault: stop here. The results posted so far stay answered;
				// this tool and any after it are re-derived on a reclaiming pass.
				return fmt.Errorf("tool %s (%s): %w", u.name, u.id, err)
			}
			// The run and the post are two steps, and reporting only after both puts
			// them in one silent interval — which the floor does not cover. A `bash`
			// call may legitimately take toolset.MaxTimeout, and posting its result is
			// a wire round trip to the control plane with no bound of its own, so the
			// pair can outlast a budget neither half comes close to. The run would then
			// be cancelled *after* its side effects had happened and *before* its
			// result was posted, and the reclaim would run the same command again
			// (#383).
			report()
			if err := postToolResult(ctx, client, sessionID, u.id, res); err != nil {
				return err
			}
			report()
		}
		// A single-agent session's window cannot open: its next turn cannot start
		// before this set's last result lands, so one pass is the whole item and
		// re-scanning would only cost a round trip.
		if !cfg.Coordinator {
			return nil
		}
		// A coordinator's threads run concurrently, so a sibling may have
		// committed calls while the pass above ran — and its enqueue was a no-op
		// against this live item, leaving nobody else to serve them. Re-scan
		// until a pass comes back empty. It terminates on progress: every
		// non-empty pass answers every call it returned, and the calls this
		// driver never answers (web, ask-gated, delegation) are filtered out of
		// the found set rather than returned unanswered. The sandbox, skills and
		// files stay one-shot above — wire-expensive, idempotent and
		// session-scoped, so nothing a later pass finds can change them.
		uses, err = unansweredToolUses(ctx, client, sessionID, true, report)
		if err != nil {
			return err
		}
		if len(uses) == 0 {
			return nil
		}
		// The scan's own boundary, so its last page does not share a silent
		// interval with the first tool run of the next pass (#383).
		report()
	}
}

// toolScanPageSize is how many events one page of the scan below requests. A
// single-agent walk reads the trailing turn only, which for a turn of N
// parallel tools is N+1 events on a fresh suspension (the uses plus the
// boundary result) and at most 2N+1 on a reclaim (their answered results are on
// the wire too) — so one page of 20 finishes in a single round trip up to N=19
// fresh, N=9 reclaimed, and anything wider simply pages on. A coordinator's
// walk has no boundary and pages to the log's start; the size is what keeps
// that walk's cost linear in pages rather than in over-read bytes. Capping the
// over-read is the cost that matters either way: tool inputs and results carry
// file contents. It is sent explicitly rather than left to the server's
// default: the bound is this worker's, and it holds against any
// wire-compatible control plane.
const toolScanPageSize = 20

// unansweredToolUses reads the session's event log over the wire and returns the
// tool calls this worker must run, oldest first: the agent.tool_use events that
// no result answers and no human still owes a verdict on. It mirrors the
// executor's diff exactly. Answered means either an agent.tool_result (a
// platform executor) or a user.tool_result (this worker), both referencing the
// use by tool_use_id, so both count. Runnable narrows that to the calls the
// platform may run now (plan 35 decision 5): evaluated_permission allow — a
// call carrying none, from before the stamp existed, counts as allowed — or
// ask with an allow user.tool_confirmation recorded for it. That narrowing is
// not a multiagent nicety, and so is not narrowed to a coordinator's sessions
// the way the boundary below is: one turn can suspend on two asks and be
// released one at a time on any session at all, and running the sibling a human
// has not answered executes a command nobody approved. This is the third
// expression of one rule — the canonical set is events.HasUnansweredToolUse
// (the SQL the control plane resumes on) and events.RunnableToolUses (which
// every platform driver drains through); the types that answer and the one
// that releases are the shared domain constants below, so a new one must be
// added in all three. A drift here re-runs an answered tool every reclaim,
// re-posting a result the control plane's ValidateToolResults rejects.
//
// Events are parsed from each event's raw wire JSON into a minimal local shape
// rather than the SDK's typed event union: the union tracks the live API's tip
// and carries post-slice surface the worker has no need for, so decoding only
// the six fields this diff needs keeps a schema drift from breaking it.
//
// Cost: the worker has no database, so it cannot ask the executor's one EXISTS
// and there is no unanswered-only wire endpoint to ask instead. On a
// single-agent session it bounds the read by walking newest-first and stopping
// at the trailing turn, rather than paging the session's whole tool history
// (#76). Two platform invariants make that exact for a turn that suspended on
// its tools: the brain commits a turn's tool_use events in ONE append
// (brain.commitTurn), and no later turn's uses reach the log until every
// outstanding one is answered — every enqueue of a model_turn that follows tool
// work is gated on events.HasUnansweredToolUse. So in this stream the unanswered
// set is the newest contiguous run of tool uses, and the first result older than
// that run is the boundary — everything beyond it is answered. A result always
// outranks the use it answers (per-session seq is assigned under the session row
// lock, and a result may only reference a committed use), so walking down, every
// use meets its answer first — and a confirmation likewise outranks the call it
// releases, so the walk has the verdict before it reads the call.
//
// coordinator drops that boundary, because both of its premises hold per thread
// rather than per session (plan 35 decision 13 iv): one thread's turn can
// suspend while a sibling's calls are still outstanding, so a runnable call can
// sit arbitrarily far down the log with an answered pair above it, and no stop
// condition short of the log's start finds it. A coordinator session therefore
// pays a full walk per claimed item — what the reference pays per attach, and
// the price of a thread-unaware worker serving child threads at all. The calls
// it walks are the session view's, which on a self_hosted environment carries
// every child thread's (decision 13 i).
//
// One path used to strand a use outside that run — a turn whose stop reason was
// not tool_use committed its tool_use events but enqueued no tool_exec, and the
// API's user.message-on-idle resume was the one enqueue after tool work not
// gated on the unanswered set — and no bounded scan can find an arbitrarily old
// stranded use, so this scan and the executor's DB-side diff disagreed on that
// shape alone. Both halves are closed (#181): the brain suspends on tool blocks
// whatever stop reason arrives with them, and that resume now gates like the
// others. On any log the current code can produce, the bound is exact and the
// two diffs agree; a log a pre-#181 binary already stranded is the residue
// neither reaches.
func unansweredToolUses(ctx context.Context, client sdk.Client, sessionID string, coordinator bool, progress func()) ([]toolUse, error) {
	iter := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: []string{
			string(domain.EventAgentToolUse),
			string(domain.EventAgentToolResult),
			string(domain.EventUserToolResult),
			string(domain.EventUserToolConfirm),
		},
		Order: sdk.BetaSessionEventListParamsOrderDesc,
		Limit: sdk.Int(toolScanPageSize),
	})
	answered := map[string]bool{}
	allowed := map[string]bool{}
	var out []toolUse
	var sawUse bool
scan:
	for iter.Next() {
		// Per event, because the walk auto-pages: a turn wide enough to span
		// several pages would otherwise spend every one of those round trips
		// inside a single silent step (#383).
		progress()
		var ev struct {
			ID                  string          `json:"id"`
			Type                string          `json:"type"`
			Name                string          `json:"name"`
			Input               json.RawMessage `json:"input"`
			ToolUseID           string          `json:"tool_use_id"`
			EvaluatedPermission string          `json:"evaluated_permission"`
			Result              string          `json:"result"`
		}
		if err := json.Unmarshal([]byte(iter.Current().RawJSON()), &ev); err != nil {
			return nil, fmt.Errorf("parse session event: %w", err)
		}
		switch domain.EventType(ev.Type) {
		case domain.EventAgentToolUse:
			sawUse = true
			// A web call (web_fetch/web_search) is never this worker's: it runs
			// in the platform executor's web driver for every environment kind,
			// and the enqueue hold-back keeps a polled item from coexisting with
			// an unanswered one. A delegation call is never anyone's to run: the
			// settlement that emitted it answers it in the same commit. Both
			// filters guard the stray case, so the six-tool Runner is never fed
			// a name it must answer unknown-tool — an answer the control plane
			// would then refuse, faulting the item into a reclaim loop. They
			// still mark sawUse — the call belongs to the trailing turn's run,
			// and the boundary is about turns, not this worker's share.
			if toolset.IsWebTool(ev.Name) || toolset.IsDelegationTool(ev.Name) {
				continue
			}
			// The runnable narrowing, spelled in Go: an absent verdict reads as
			// allow (the SQL's COALESCE), and an ask waits for a human's allow.
			// A deny is skipped too — the platform synthesizes its error result,
			// and running it meanwhile would execute what the policy refused.
			if ev.EvaluatedPermission != "" && ev.EvaluatedPermission != "allow" && !allowed[ev.ID] {
				continue
			}
			// Not "stop at the first answered use": a turn's tools can be
			// answered out of order — a denial's result lands at once while an
			// allowed sibling is still outstanding — so the whole run is read.
			if !answered[ev.ID] {
				out = append(out, toolUse{id: domain.ID(ev.ID), name: ev.Name, input: ev.Input})
			}
		case domain.EventAgentToolResult, domain.EventUserToolResult:
			if sawUse && !coordinator {
				break scan // older than the trailing turn: everything past here is answered
			}
			answered[ev.ToolUseID] = true
		case domain.EventUserToolConfirm:
			// Never touches sawUse or the boundary: a verdict is not a turn's
			// end, and one recorded above a trailing run belongs to it.
			if ev.Result == "allow" {
				allowed[ev.ToolUseID] = true
			}
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	slices.Reverse(out) // the walk collected newest-first; run them in log order
	return out, nil
}

// postToolResult sends one user.tool_result answering a tool use. Empty tool
// output posts the reference runner's toolset.NoOutput text block (v1.63.1),
// never an empty one: the Sessions API rejects an empty text block, and so
// does the Messages endpoint the brain's replay hands the content to.
// is_error is carried through so the model sees a tool-level failure as an
// error result.
func postToolResult(ctx context.Context, client sdk.Client, sessionID string, useID domain.ID, res toolset.Result) error {
	ev := sdk.BetaManagedAgentsEventParamsOfUserToolResult(useID.String())
	// The convenience constructor sets only tool_use_id; the wire requires the
	// event's type discriminator, which the union marshaler does not fill in.
	ev.OfUserToolResult.Type = sdk.BetaManagedAgentsUserToolResultEventParamsTypeUserToolResult
	ev.OfUserToolResult.IsError = sdk.Bool(res.IsError)
	text := res.Content
	if text == "" {
		text = toolset.NoOutput
	}
	ev.OfUserToolResult.Content = []sdk.BetaManagedAgentsUserToolResultEventParamsContentUnion{{
		OfText: &sdk.BetaManagedAgentsTextBlockParam{
			Text: text,
			Type: sdk.BetaManagedAgentsTextBlockTypeText,
		},
	}}
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, sdk.BetaSessionEventSendParams{
		Events: []sdk.BetaManagedAgentsEventParamsUnion{ev},
	})
	if err != nil {
		return fmt.Errorf("post tool result for %s: %w", useID, err)
	}
	return nil
}
