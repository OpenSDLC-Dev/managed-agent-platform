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
// turn has suspended for built-in tool calls, it runs every unanswered tool in
// the session's sandbox and posts a user.tool_result for each back through the
// session events API. It is the self_hosted twin of the platform executor's
// per-item processing, with two deployment differences: the transport is HTTP
// (the worker has no database), and the result event is user.tool_result, not
// agent.tool_result — the control plane resumes the brain when a result
// completes the outstanding set, so the worker never enqueues a turn itself.
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
	uses, err := unansweredToolUses(ctx, client, sessionID, report)
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
	return nil
}

// toolScanPageSize is how many events one page of the scan below requests. The
// walk reads the trailing turn only, which for a turn of N parallel tools is
// N+1 events on a fresh suspension (the uses plus the boundary result) and at
// most 2N+1 on a reclaim (their answered results are on the wire too) — so one
// page of 20 finishes in a single round trip up to N=19 fresh, N=9 reclaimed,
// and anything wider simply pages on. The size caps what a page over-reads,
// which is the cost that matters: tool inputs and results carry file contents.
// It is sent explicitly rather than left to the server's default: the bound is
// this worker's, and it holds against any wire-compatible control plane.
const toolScanPageSize = 20

// unansweredToolUses reads the session's event log over the wire and returns the
// agent.tool_use events still lacking a result, oldest first — the work this
// call must run. It mirrors the executor's diff exactly: an agent.tool_use is
// answered by either an agent.tool_result (a platform executor) or a
// user.tool_result (this worker), both referencing it by tool_use_id, so both
// count. This is the third expression of one rule — the canonical answered-set
// is events.HasUnansweredToolUse (the SQL the control plane resumes on) and
// executor.unansweredToolUses (the executor's DB-backed copy); the result types
// that answer are the shared domain constants below, so a new answering type
// must be added in all three. A drift here re-runs an answered tool every
// reclaim, re-posting a result the control plane's ValidateToolResults rejects.
//
// Events are parsed from each event's raw wire JSON into a minimal local shape
// rather than the SDK's typed event union: the union tracks the live API's tip
// and carries post-slice surface the worker has no need for, so decoding only
// the four fields this diff needs keeps a schema drift from breaking it.
//
// Cost: the worker has no database, so it cannot ask the executor's one EXISTS
// and there is no unanswered-only wire endpoint to ask instead. It bounds the
// read by walking newest-first and stopping at the trailing turn, rather than
// paging the session's whole tool history (#76). Two platform invariants make
// that exact for a turn that suspended on its tools: the brain commits a turn's
// tool_use events in ONE append (brain.commitTurn), and no later turn's uses
// reach the log until every outstanding one is answered — every enqueue of a
// model_turn that follows tool work is gated on events.HasUnansweredToolUse. So
// in this three-type stream the unanswered set is the newest contiguous run of
// tool uses, and the first result older than that run is the boundary —
// everything beyond it is answered. A result always outranks the use it answers
// (per-session seq is assigned under the session row lock, and a result may only
// reference a committed use), so walking down, every use meets its answer first.
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
func unansweredToolUses(ctx context.Context, client sdk.Client, sessionID string, progress func()) ([]toolUse, error) {
	iter := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: []string{
			string(domain.EventAgentToolUse),
			string(domain.EventAgentToolResult),
			string(domain.EventUserToolResult),
		},
		Order: sdk.BetaSessionEventListParamsOrderDesc,
		Limit: sdk.Int(toolScanPageSize),
	})
	answered := map[string]bool{}
	var out []toolUse
	var sawUse bool
scan:
	for iter.Next() {
		// Per event, because the walk auto-pages: a turn wide enough to span
		// several pages would otherwise spend every one of those round trips
		// inside a single silent step (#383).
		progress()
		var ev struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
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
			// an unanswered one. The filter guards the stray case, so the
			// six-tool Runner is never fed a name it must answer unknown-tool.
			// It still marks sawUse — the call belongs to the trailing turn's
			// run, and the boundary is about turns, not this worker's share.
			if toolset.IsWebTool(ev.Name) {
				continue
			}
			// Not "stop at the first answered use": a turn's tools can be
			// answered out of order — a denial's result lands at once while an
			// allowed sibling is still outstanding — so the whole run is read.
			if !answered[ev.ID] {
				out = append(out, toolUse{id: domain.ID(ev.ID), name: ev.Name, input: ev.Input})
			}
		case domain.EventAgentToolResult, domain.EventUserToolResult:
			if sawUse {
				break scan // older than the trailing turn: everything past here is answered
			}
			answered[ev.ToolUseID] = true
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("list session events: %w", err)
	}
	slices.Reverse(out) // the walk collected newest-first; run them in log order
	return out, nil
}

// postToolResult sends one user.tool_result answering a tool use. Empty tool
// output posts the reference runner's "(no output)" text block (v1.63.0),
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
		text = "(no output)"
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
