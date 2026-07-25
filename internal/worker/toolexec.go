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
	uses, err := unansweredToolUses(ctx, client, sessionID)
	if err != nil {
		return err
	}
	if len(uses) == 0 {
		return nil
	}
	sb, err := provider.Provision(ctx, sandbox.Spec{
		SessionID:  domain.ID(sessionID),
		Image:      cfg.Image,
		Workdir:    cfg.Workdir,
		Networking: cfg.Networking,
	})
	if err != nil {
		return fmt.Errorf("provision sandbox: %w", err)
	}
	if err := SetupSkills(ctx, client, sessionID, sb, cfg.Workdir); err != nil {
		return err
	}
	if err := SetupFiles(ctx, client, sessionID, sb, cfg.Workdir); err != nil {
		return err
	}
	runner := toolset.Runner{Sandbox: sb, Session: domain.ID(sessionID), Workdir: cfg.Workdir}
	for _, u := range uses {
		res, err := runner.Run(ctx, u.id, u.name, u.input)
		if err != nil {
			// Backend fault: stop here. The results posted so far stay answered;
			// this tool and any after it are re-derived on a reclaiming pass.
			return fmt.Errorf("tool %s (%s): %w", u.name, u.id, err)
		}
		if err := postToolResult(ctx, client, sessionID, u.id, res); err != nil {
			return err
		}
	}
	return nil
}

// toolScanPageSize is how many events one page of the scan below requests. The
// walk needs the trailing turn only — its tool uses plus whatever already
// answers them, at most 2N+1 events for a turn of N parallel tools — so one page
// covers any realistic batch in a single round trip while capping what a page
// over-reads (tool inputs and results carry file contents). It is sent
// explicitly rather than left to the server's default: the bound is this
// worker's, and it holds against any wire-compatible control plane.
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
// that exact: the brain commits a turn's tool_use events in ONE append
// (brain.commitTurn), and no later turn's uses reach the log until every
// outstanding one is answered (events.HasUnansweredToolUse gates the resume).
// So in this three-type stream the unanswered set is always the newest
// contiguous run of tool uses, and the first result older than that run is the
// boundary — everything beyond it is answered. A result always outranks the use
// it answers (per-session seq is assigned under the session row lock, and a
// result may only reference a committed use), so walking down, every use meets
// its answer first.
func unansweredToolUses(ctx context.Context, client sdk.Client, sessionID string) ([]toolUse, error) {
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
// output posts no content blocks (the SDK omits the empty field): the control
// plane stores that as null content, which the brain's replay renders as a
// tool_result block with no content field — valid for the Messages API, where an
// empty text block is not. is_error is carried through so the model sees a
// tool-level failure as an error result.
func postToolResult(ctx context.Context, client sdk.Client, sessionID string, useID domain.ID, res toolset.Result) error {
	ev := sdk.BetaManagedAgentsEventParamsOfUserToolResult(useID.String())
	// The convenience constructor sets only tool_use_id; the wire requires the
	// event's type discriminator, which the union marshaler does not fill in.
	ev.OfUserToolResult.Type = sdk.BetaManagedAgentsUserToolResultEventParamsTypeUserToolResult
	ev.OfUserToolResult.IsError = sdk.Bool(res.IsError)
	if res.Content != "" {
		ev.OfUserToolResult.Content = []sdk.BetaManagedAgentsUserToolResultEventParamsContentUnion{{
			OfText: &sdk.BetaManagedAgentsTextBlockParam{
				Text: res.Content,
				Type: sdk.BetaManagedAgentsTextBlockTypeText,
			},
		}}
	}
	_, err := client.Beta.Sessions.Events.Send(ctx, sessionID, sdk.BetaSessionEventSendParams{
		Events: []sdk.BetaManagedAgentsEventParamsUnion{ev},
	})
	if err != nil {
		return fmt.Errorf("post tool result for %s: %w", useID, err)
	}
	return nil
}
