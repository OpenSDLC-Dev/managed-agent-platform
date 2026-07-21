package worker

import (
	"context"
	"encoding/json"
	"fmt"

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
// against an already-answered session (a redundant reclaim) is a cheap couple of
// reads with nothing to run.
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
// the three fields this diff needs keeps a schema drift from breaking it.
//
// Cost: with no unanswered-only wire endpoint, this pages the session's full
// agent.tool_use and result history on every call to find the outstanding few —
// O(history) per tool batch, where the executor's DB does it in one EXISTS. The
// outstanding set is always the last suspended turn's, so a future optimization
// could read newest-first and stop early; the simple full scan is correct and is
// what this first cut ships.
func unansweredToolUses(ctx context.Context, client sdk.Client, sessionID string) ([]toolUse, error) {
	uses, err := listRawEvents(ctx, client, sessionID, string(domain.EventAgentToolUse))
	if err != nil {
		return nil, fmt.Errorf("list tool uses: %w", err)
	}
	results, err := listRawEvents(ctx, client, sessionID,
		string(domain.EventAgentToolResult), string(domain.EventUserToolResult))
	if err != nil {
		return nil, fmt.Errorf("list tool results: %w", err)
	}

	answered := make(map[string]bool, len(results))
	for _, r := range results {
		var ref struct {
			ToolUseID string `json:"tool_use_id"`
		}
		if err := json.Unmarshal(r, &ref); err != nil {
			return nil, fmt.Errorf("parse tool result: %w", err)
		}
		answered[ref.ToolUseID] = true
	}

	var out []toolUse
	for _, u := range uses {
		var body struct {
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u, &body); err != nil {
			return nil, fmt.Errorf("parse tool use: %w", err)
		}
		if answered[body.ID] {
			continue
		}
		out = append(out, toolUse{id: domain.ID(body.ID), name: body.Name, input: body.Input})
	}
	return out, nil
}

// listRawEvents returns the raw wire JSON of every event of the given types for
// the session, oldest first (the API's default order), following pagination to
// completion. Reading the raw bytes lets the caller decode only the fields it
// needs; see unansweredToolUses.
func listRawEvents(ctx context.Context, client sdk.Client, sessionID string, types ...string) ([]json.RawMessage, error) {
	iter := client.Beta.Sessions.Events.ListAutoPaging(ctx, sessionID, sdk.BetaSessionEventListParams{
		Types: types,
	})
	var out []json.RawMessage
	for iter.Next() {
		ev := iter.Current()
		out = append(out, json.RawMessage(ev.RawJSON()))
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
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
