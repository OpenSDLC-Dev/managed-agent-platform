package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/mcp"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// The execution half of the mcp_exec driver. Discovery (mcpwork.go) fills the
// catalog so the brain can offer a server's tools; this answers the calls the
// model then makes against them.
//
// One item does one job. A pass with an unanswered call answers calls and does
// not discover: the turn is stopped on that call, while a listing is only ever
// wanted by a turn that has not started. Discovery is what a pass does when
// there is nothing outstanding.

// mcpToolUse is one MCP call this pass must answer: the tool-use event's id
// (which the result references), the server the call names, the tool, and the
// arguments the model produced. The server name lives on the call rather than on
// its answer — an agent.mcp_tool_result carries no mcp_server_name — so this is
// where the endpoint to dial is resolved from.
type mcpToolUse struct {
	id     domain.ID
	server string
	name   string
	input  json.RawMessage
}

// unansweredMCPToolUses returns the session's agent.mcp_tool_use events that no
// agent.mcp_tool_result answers, oldest first. Only the platform writes that
// result — a client may post neither shape, and no BYOC worker sees an MCP call
// — so unlike the sandbox six there is exactly one answering type to diff
// against, and a reclaim re-runs only what is still outstanding.
func (e *Executor) unansweredMCPToolUses(ctx context.Context, sid domain.ID) ([]mcpToolUse, error) {
	uses, err := e.log.List(ctx, sid, events.ListQuery{
		Types: []string{string(domain.EventAgentMCPToolUse)}})
	if err != nil {
		return nil, fmt.Errorf("list mcp tool uses: %w", err)
	}
	if len(uses) == 0 {
		return nil, nil
	}
	results, err := e.log.List(ctx, sid, events.ListQuery{
		Types: []string{string(domain.EventAgentMCPToolResult)}})
	if err != nil {
		return nil, fmt.Errorf("list mcp tool results: %w", err)
	}
	answered := make(map[string]bool, len(results))
	for _, r := range results {
		var ref struct {
			MCPToolUseID string `json:"mcp_tool_use_id"`
		}
		if err := json.Unmarshal(r.Body, &ref); err != nil {
			return nil, fmt.Errorf("mcp tool result %s: %w", r.ID, err)
		}
		answered[ref.MCPToolUseID] = true
	}

	var out []mcpToolUse
	for _, u := range uses {
		if answered[u.ID.String()] {
			continue
		}
		var body struct {
			Name          string          `json:"name"`
			MCPServerName string          `json:"mcp_server_name"`
			Input         json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u.Body, &body); err != nil {
			return nil, fmt.Errorf("mcp tool use %s: %w", u.ID, err)
		}
		out = append(out, mcpToolUse{
			id: u.ID, server: body.MCPServerName, name: body.Name, input: body.Input,
		})
	}
	return out, nil
}

// runMCPTools answers each outstanding call and returns the events to append:
// one agent.mcp_tool_result per call, plus a session.error for every call that
// never reached its server.
//
// Both, never one or the other. A transport failure is the model's business —
// it must see an answer or the turn cannot continue, and every later replay
// would carry a tool_use no result answers — and it is the platform's, which
// has a connection to heal and an operator to tell. A tool that ran and *failed*
// is only the model's: that is an ordinary result with is_error, and reporting
// it as a session error would cry wolf on a server that is working exactly as
// designed.
//
// A dead context (lease lost, shutdown) stops the pass and settles nothing, the
// same all-or-nothing the web driver and discovery use: the reclaim re-runs the
// calls this pass had not answered.
func (e *Executor) runMCPTools(ctx context.Context, cfg domain.EnvironmentConfig,
	declared []mcpServerRef, uses []mcpToolUse) ([]events.NewEvent, error, error) {

	endpoints := make(map[string]string, len(declared))
	for _, s := range declared {
		endpoints[s.Name] = s.URL
	}

	var out []events.NewEvent
	for _, u := range uses {
		start := time.Now()
		res, failure := e.runMCPTool(ctx, cfg, endpoints[u.server], u)
		toolset.RecordRun(ctx, "mcp__"+u.server+"__"+u.name, time.Since(start),
			toolset.Result{IsError: res.isError}, ctx.Err())
		if ctx.Err() != nil {
			return out, fmt.Errorf("mcp tool %s (%s): %w", u.name, u.id, ctx.Err()), nil
		}
		ev, err := mcpResultEvent(u.id, res)
		if err != nil {
			return out, nil, err
		}
		out = append(out, ev)
		if failure.message != "" {
			ev, err := mcpConnectionFailedEvent(u.server, failure)
			if err != nil {
				return out, nil, err
			}
			out = append(out, ev)
		}
	}
	return out, nil, nil
}

// answerMCPCalls runs this pass's calls and commits their answers, the follow-on
// work and the item's fate in one transaction — the same settlement shape the
// web driver and discovery use, and for the same reason: a pass that answered
// calls and then failed to schedule what comes next would leave the session
// waiting on a turn nothing enqueues.
//
// What comes next is the four-way chain, in the order the drivers can actually
// satisfy: another mcp_exec while any MCP call is still outstanding (this pass
// answers only what it found — a call the brain committed while it ran is one
// nothing else will answer), then the platform built-ins by the same web-first
// rule the other settlements use, then the brain when everything is answered.
// A turn whose only remaining calls are client-executed custom tools schedules
// nothing and waits for the client, exactly as the others do.
func (e *Executor) answerMCPCalls(ctx context.Context, item *queue.Item, sess sessionRun, calls []mcpToolUse) error {
	// Keep the lease across the calls: a tool that takes its time would
	// otherwise outlast a fixed TTL and lose the item mid-call.
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL)
	results, faultErr, runErr := e.runMCPTools(kctx, sess.envConfig, sess.mcpServers, calls)
	if kerr := keeper.Close(); kerr != nil {
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}
	if faultErr != nil {
		return faultErr
	}

	opts := events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			mcpPending, err := events.HasUnansweredMCPToolUse(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			// Chained by handing this very item back, not by enqueuing a second
			// one: Enqueue is keyed (session_id, kind) over the live states, so
			// a fresh mcp_exec raised while this one is still active would be
			// dropped on conflict and the session would wait on work nobody
			// queued. The other three arms name a different kind and enqueue.
			if mcpPending {
				return e.queue.Requeue(ctx, tx, item)
			}
			platformPending, err := events.UnansweredPlatformToolNames(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if len(platformPending) > 0 {
				kind := queue.ToolExec
				if slices.ContainsFunc(platformPending, toolset.IsWebTool) {
					kind = queue.WebExec
				}
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, kind); err != nil {
					return err
				}
				return e.queue.Complete(ctx, tx, item)
			}
			anyPending, err := events.HasUnansweredToolUse(ctx, tx, item.SessionID, nil)
			if err != nil {
				return err
			}
			if !anyPending {
				if _, err := e.queue.Enqueue(ctx, tx, item.EnvironmentID, item.SessionID, queue.ModelTurn); err != nil {
					return err
				}
			}
			return e.queue.Complete(ctx, tx, item)
		},
	}
	if _, err := e.log.AppendWith(ctx, item.SessionID, results, opts); err != nil {
		return fmt.Errorf("append mcp tool results: %w", err)
	}
	return nil
}

// mcpAnswer is one call's outcome in the shape the log wants: the blocks that
// become the result's content, and whether the model should read it as an error.
type mcpAnswer struct {
	blocks  []map[string]any
	isError bool
}

func mcpFailed(format string, args ...any) mcpAnswer {
	return mcpAnswer{blocks: []map[string]any{textBlock(fmt.Sprintf(format, args...))}, isError: true}
}

// mcpFailure is a transport-level failure worth a session.error: the message,
// already cut to scheme://host, and which of the wire's retry statuses it is. A
// zero value means no session.error — the call reached the server, or there was
// nothing to reach.
type mcpFailure struct {
	message string
	// terminal marks a failure no later turn can fix, which is the plan's
	// reading of the status union: `retrying` for a failure the next turn will
	// re-attempt, `terminal` for one it cannot. Only the egress refusal is
	// terminal here, because it is the only one this code can *know* is: a
	// connection error may be a server restarting as easily as a host that will
	// never resolve, and reporting a transient outage as terminal would tell a
	// client the session is done with a server that is back a minute later.
	terminal bool
}

// runMCPTool answers one call. The second return is a transport failure worth a
// session.error, zero when the call reached the server at all — including when
// the tool itself failed, which is a working server reporting a working failure.
//
// The connection is per call rather than per pass. A turn's MCP calls are few,
// a handshake is one round trip, and a connection each keeps one server's
// failure from touching another's call — the same reason discovery dials each
// server on its own.
func (e *Executor) runMCPTool(ctx context.Context, cfg domain.EnvironmentConfig,
	endpoint string, u mcpToolUse) (mcpAnswer, mcpFailure) {

	if endpoint == "" {
		// The agent stopped declaring this server between the turn that called
		// it and now — mcp_servers is mid-session-mutable. The model is told,
		// and nothing is dialled: there is no address to dial, and an agent edit
		// is not a connection that failed.
		return mcpFailed("MCP server %q is no longer configured on this agent.", u.server), mcpFailure{}
	}
	host, err := mcpEndpointHost(endpoint)
	if err != nil {
		return mcpFailed("MCP server %q has an unusable url.", u.server), mcpFailure{}
	}
	if !mcpEgressAllowed(cfg, host) {
		reason := egressRefusal(cfg, host)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, reason),
			mcpFailure{message: storableReason(reason, endpoint), terminal: true}
	}

	conn, err := mcp.Connect(ctx, mcp.Config{URL: endpoint, HTTPClient: e.mcpCallHTTP()})
	if err != nil {
		msg := storableReason(err.Error(), endpoint)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, msg), mcpFailure{message: msg}
	}
	defer conn.Close()

	res, err := conn.CallTool(ctx, u.name, u.input)
	if err != nil {
		msg := storableReason(err.Error(), endpoint)
		return mcpFailed("MCP tool %q on server %q could not be run: %s", u.name, u.server, msg),
			mcpFailure{message: msg}
	}
	return mcpAnswer{blocks: mcpResultBlocks(res.Content), isError: res.IsError}, mcpFailure{}
}

// mcpCallHTTP is the client tool calls go through: the executor's own when a
// test supplies one, else the guarded client whose request cap fits a tool
// rather than a handshake.
func (e *Executor) mcpCallHTTP() *http.Client {
	if e.mcpHTTP != nil {
		return e.mcpHTTP
	}
	return mcp.CallClient
}

// mcpResultBlocks renders a server's answer into the block types an
// agent.mcp_tool_result admits — text, image, document and search_result, and no
// others (verified against the SDK's event types). MCP has five content types of
// its own, so three of these mappings are decisions rather than translations,
// and all three are recorded in docs/DIVERGENCES.md:
//
//   - text becomes text, and an image becomes an image carrying the server's
//     bytes and media type. These are translations.
//   - An embedded resource becomes a document — the block type that exists to
//     carry a file — with the resource's URI as its title so the model can name
//     what it read. A text resource rides the wire's plain-text source, whose
//     media_type the schema fixes at "text/plain" (it is an enum of one), so a
//     resource declaring anything else keeps that fact in the document's context
//     rather than losing it. A blob rides the base64 source, whose media type is
//     free.
//   - A resource *link* carries no content at all, only a pointer, and becomes
//     text naming it. Not a url document source, which would have the model's
//     own side fetch an address this platform never vetted — the address guard
//     covers what the executor dials, not what a document source resolves.
//   - Audio has no block type on this wire, so it is described rather than
//     dropped: a model told a tool returned audio can say so, where a silently
//     shorter result reads as a tool that returned less than it did.
//
// A block whose type is none of MCP's five is dropped — the SDK's decoder admits
// two sampling-only types here that a tool result has no business carrying.
func mcpResultBlocks(content []mcp.Content) []map[string]any {
	out := make([]map[string]any, 0, len(content))
	for _, c := range content {
		switch c.Type {
		case "text":
			out = append(out, textBlock(c.Text))
		case "image":
			out = append(out, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": toolset.SanitizeText(c.MIMEType),
					"data":       base64.StdEncoding.EncodeToString(c.Data),
				},
			})
		case "resource":
			out = append(out, resourceBlock(c))
		case "resource_link":
			out = append(out, textBlock(fmt.Sprintf("The tool returned a link to a resource: %s (%s)",
				c.URI, mimeOrUnknown(c.MIMEType))))
		case "audio":
			out = append(out, textBlock(fmt.Sprintf(
				"The tool returned %d bytes of audio (%s), which cannot be shown here.",
				len(c.Data), mimeOrUnknown(c.MIMEType))))
		}
	}
	// A tool that answers with nothing at all still needs one block: the wire's
	// content is optional but an empty array is indistinguishable from a tool
	// that returned nothing, and a Messages endpoint rejects an empty text
	// block — which is what every later replay of this session would send.
	if len(out) == 0 {
		out = append(out, textBlock("The tool returned no content."))
	}
	return capMCPBlocks(out)
}

// capMCPBlocks holds one answer to the budget every tool result gets on this
// platform (toolset.MaxOutputBytes), for the two reasons that budget exists: it
// is the model's context, and a tool result is on the append-only event log
// forever. Neither MCP nor the reference bounds a tool's output, and this
// package's own ceiling is megabytes per response, so without this one server's
// answer would ride every later replay of the session.
//
// Blocks are charged their marshalled size — what actually lands — in the order
// the server sent them, and the first that does not fit ends the answer. A block
// is kept or dropped whole: a text block is already capped where it was built
// (textBlock), and cutting a base64 payload in half yields something that
// decodes to nothing, so the tool catalog's whole-or-nothing rule applies here
// too. What was dropped is said rather than left to look like a short answer.
//
// One block is exempt and only one: a leading text block, which has already been
// bounded on its own in textBlock. Keeping it admits a fixed overhead — the JSON
// around it and the truncation notice inside it — and not a server's bytes, and
// it is what makes the ordinary over-long answer a truncated answer rather than
// no answer at all. The exemption stops there. A leading *image* or base64
// document is bounded by nothing but the transport's megabytes, and there is no
// truncation to fall back on, so an over-budget one is dropped like any other:
// a result saying the image was too large is something a model can act on, where
// a base64 payload cut in half is not.
//
// The trailing notice rides on top of the budget rather than inside it: it is
// this platform's text about the server's, and it is a couple of hundred bytes.
// Spilling an over-budget answer to a file the model can read instead of
// truncating it is plan 29 slice 4b's, alongside the sandbox tools' spill.
func capMCPBlocks(blocks []map[string]any) []map[string]any {
	budget := toolset.MaxOutputBytes
	out := make([]map[string]any, 0, len(blocks))
	for i, b := range blocks {
		// Cannot fail: every block here is built from strings and byte slices.
		raw, _ := json.Marshal(b)
		exempt := i == 0 && b["type"] == "text"
		if len(raw) > budget && !exempt {
			return append(out, textBlock(fmt.Sprintf(
				"%d content block(s) of this answer were dropped: it is past the %d bytes "+
					"a tool result may put on this session's log.", len(blocks)-i, toolset.MaxOutputBytes)))
		}
		budget -= len(raw)
		out = append(out, b)
	}
	return out
}

func resourceBlock(c mcp.Content) map[string]any {
	title := toolset.SanitizeText(c.URI)
	block := map[string]any{"type": "document", "title": title}
	if c.Data != nil {
		block["source"] = map[string]any{
			"type":       "base64",
			"media_type": toolset.SanitizeText(c.MIMEType),
			"data":       base64.StdEncoding.EncodeToString(c.Data),
		}
		return block
	}
	block["source"] = map[string]any{
		"type":       "text",
		"media_type": "text/plain",
		"data":       toolset.CapOutput(toolset.SanitizeText(c.Text)),
	}
	// The schema's plain-text source admits exactly one media type, so a
	// resource declaring another is carried as context rather than dropped:
	// "text/markdown" changes how a model should read the same bytes.
	if mime := toolset.SanitizeText(c.MIMEType); mime != "" && mime != "text/plain" {
		block["context"] = "The source resource declares its media type as " + mime + "."
	}
	return block
}

// textBlock is every text block this driver writes, and the one place a single
// block is bounded: capped to what a tool result may return to a model, with the
// same truncation notice every built-in tool's output carries. Capping here
// rather than in capMCPBlocks is what keeps the ordinary answer — one huge text
// block — truncated rather than dropped whole.
func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": toolset.CapOutput(toolset.SanitizeText(s))}
}

func mimeOrUnknown(mime string) string {
	if m := toolset.SanitizeText(mime); m != "" {
		return m
	}
	return "media type unstated"
}

func mcpResultEvent(useID domain.ID, res mcpAnswer) (events.NewEvent, error) {
	payload, err := json.Marshal(map[string]any{
		"mcp_tool_use_id": useID.String(),
		"content":         res.blocks,
		"is_error":        res.isError,
	})
	if err != nil {
		return events.NewEvent{}, fmt.Errorf("marshal mcp tool result: %w", err)
	}
	return events.NewEvent{Type: domain.EventAgentMCPToolResult, Payload: payload}, nil
}

// mcpConnectionFailedEvent reports a server the platform could not reach. The
// message is already cut to scheme://host by the caller, for the reason a
// catalog row's is: an mcp_servers entry is customer-supplied and may carry a
// credential in its userinfo or its query.
//
// retry_status is a union variant — an object carrying a type, like every other
// retry status this platform emits, not the bare string the field's name invites.
// Which variant follows from what this platform will actually do next: every turn
// re-attempts a failed server, so an ordinary failure is `retrying` and heals by
// itself, while an egress refusal is `terminal` because no later turn changes the
// policy that refused it. Nothing disables a server for the rest of a session,
// and this does not invent that (docs/DIVERGENCES.md).
func mcpConnectionFailedEvent(server string, f mcpFailure) (events.NewEvent, error) {
	status := "retrying"
	if f.terminal {
		status = "terminal"
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":            "mcp_connection_failed_error",
			"mcp_server_name": server,
			"message":         f.message,
			"retry_status":    map[string]any{"type": status},
		},
	})
	if err != nil {
		return events.NewEvent{}, fmt.Errorf("marshal session error: %w", err)
	}
	return events.NewEvent{Type: domain.EventSessionError, Payload: payload}, nil
}
