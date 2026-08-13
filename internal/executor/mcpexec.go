package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"
	"unicode/utf8"

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
	deadline := time.Now().Add(e.cfg.MCPPassTimeout)
	for i, u := range uses {
		start := time.Now()
		// The budget is checked between calls and never cancels one: a call cut
		// off mid-flight would be answered as though the server had failed, and
		// a tool with a side effect would have run without an answer on the log
		// saying so. So the pass overruns by at most one call, and always makes
		// progress — the first call is past this check with the clock at zero.
		// What it leaves unanswered keeps the item (answerMCPCalls requeues it),
		// and the calls it did make are committed, so nothing runs twice.
		if i > 0 && start.After(deadline) {
			break
		}
		res, failure := e.runMCPTool(ctx, cfg, endpoints[u.server], u)
		// One label value for every MCP call, rather than the call's own name.
		// The metric's tool name has until now been drawn from a fixed set of
		// eight, and a server's name and its tools' names are both third-party
		// and unbounded: an agent that declares generated server names, or a
		// server offering a large catalog, multiplies time series without limit
		// in whatever backend collects them. Which tool ran is on the event log,
		// where it costs storage rather than cardinality.
		toolset.RecordRun(ctx, mcpToolMetricName, time.Since(start),
			toolset.Result{IsError: res.isError}, ctx.Err())
		if ctx.Err() != nil {
			return out, fmt.Errorf("mcp tool %s (%s): %w", u.name, u.id, ctx.Err()), nil
		}
		ev, err := mcpResultEvent(u.id, res)
		if err != nil {
			return out, nil, err
		}
		out = append(out, ev)
		if failure != "" {
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

// runMCPTool answers one call. The second return is a transport failure worth a
// session.error, empty when the call reached the server at all — including when
// the tool itself failed, which is a working server reporting a working failure.
//
// The connection is per call rather than per pass. A turn's MCP calls are few,
// a handshake is one round trip, and a connection each keeps one server's
// failure from touching another's call — the same reason discovery dials each
// server on its own.
func (e *Executor) runMCPTool(ctx context.Context, cfg domain.EnvironmentConfig,
	endpoint string, u mcpToolUse) (mcpAnswer, string) {

	if endpoint == "" {
		// The agent stopped declaring this server between the turn that called
		// it and now — mcp_servers is mid-session-mutable. The model is told,
		// and nothing is dialled: there is no address to dial, and an agent edit
		// is not a connection that failed.
		return mcpFailed("MCP server %q is no longer configured on this agent.", u.server), ""
	}
	host, err := mcpEndpointHost(endpoint)
	if err != nil {
		return mcpFailed("MCP server %q has an unusable url.", u.server), ""
	}
	if !mcpEgressAllowed(cfg, host) {
		reason := egressRefusal(cfg, host)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, reason),
			storableReason(reason, endpoint)
	}

	conn, err := mcp.Connect(ctx, mcp.Config{URL: endpoint, HTTPClient: e.mcpCallHTTP()})
	if err != nil {
		msg := storableReason(err.Error(), endpoint)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, msg), msg
	}
	defer conn.Close()

	res, err := conn.CallTool(ctx, u.name, u.input)
	if err != nil {
		msg := storableReason(err.Error(), endpoint)
		failure := msg
		if errors.Is(err, mcp.ErrServerAnswered) {
			// The server was reached and refused the call — an unknown tool, or
			// a request for input this platform cannot supply. The model is told
			// so it can stop calling that tool; the operator is not, because
			// mcp_connection_failed_error is the wire's word for a connection
			// that failed and there is no connection here to heal.
			failure = ""
		}
		return mcpFailed("MCP tool %q on server %q could not be run: %s", u.name, u.server, msg), failure
	}
	return mcpAnswer{blocks: mcpResultBlocks(res.Content), isError: res.IsError}, ""
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
			// An explicit empty text block is a shape a server may legally send
			// and a Messages endpoint rejects, and replay puts this content
			// array into a Messages tool_result unchanged — so one on this
			// append-only log fails every later turn of the session, not just
			// this one. Dropping it cannot lose an answer: there is nothing in
			// it, and a result left with no blocks at all picks up the
			// no-content block below.
			if txt := textBlock(c.Text); txt["text"] != "" {
				out = append(out, txt)
			}
		case "image":
			out = append(out, imageBlock(c))
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
// One block is exempt: the first this driver has already capped every string
// inside — a text block, or a document whose source is text, the body through
// toolset.CapOutput and the two labels a document carries about its resource
// through capLabel. Keeping it admits a fixed overhead and not a server's bytes:
// the JSON around it, the truncation notices inside it, the labels' own small
// ceiling, and whatever JSON escaping costs, which is bounded but not one-to-one
// — a control byte the NUL strip leaves alone marshals as six. What the
// exemption buys is that the ordinary over-long answer arrives *truncated*
// rather than not at all, which is the whole difference for the shapes a model
// can still read.
//
// It is the first *capped* block and not the first block, because those are not
// the same answer: a server that sends a thumbnail and then its report would,
// under a positional rule, have the report vanish for standing second — the one
// shape the exemption exists to keep readable, lost to something in front of it.
//
// It stops at those two. A leading image or base64 document is bounded by
// nothing but the transport's megabytes and cannot be truncated — half a base64
// payload decodes to nothing — so an over-budget one is dropped like any other,
// and a result saying the image was too large is something a model can act on
// where a broken payload is not.
//
// The trailing notice rides on top of the budget rather than inside it: it is
// this platform's text about the server's, and it is a couple of hundred bytes.
// Spilling an over-budget answer to a file the model can read instead of
// truncating it is plan 29 slice 4b's, alongside the sandbox tools' spill.
func capMCPBlocks(blocks []map[string]any) []map[string]any {
	budget := toolset.MaxOutputBytes
	exempted := false
	out := make([]map[string]any, 0, len(blocks))
	for i, b := range blocks {
		// Cannot fail: every block here is built from strings and byte slices.
		raw, _ := json.Marshal(b)
		if len(raw) > budget && alreadyCapped(b) && !exempted {
			exempted = true
			out = append(out, b)
			budget -= len(raw)
			continue
		}
		if len(raw) > budget {
			return append(out, textBlock(fmt.Sprintf(
				"%d content block(s) of this answer were dropped: it is past the %d bytes "+
					"a tool result may put on this session's log.", len(blocks)-i, toolset.MaxOutputBytes)))
		}
		budget -= len(raw)
		out = append(out, b)
	}
	return out
}

// alreadyCapped reports whether the text this block carries has been through
// toolset.CapOutput where the block was built — which is what makes exempting it
// bounded rather than open-ended. Without the document arm, the cap
// resourceBlock applies to a text resource could never reach the log: a capped
// body plus its wrapper always exceeds the budget, so every such block was
// dropped whole and a 150 KB text file reached the model as nothing at all.
func alreadyCapped(b map[string]any) bool {
	if b["type"] == "text" {
		return true
	}
	src, ok := b["source"].(map[string]any)
	return ok && src["type"] == "text"
}

// maxResourceLabel bounds the two strings a document block carries *about* its
// resource rather than *from* it: the URI it is titled with, and the media type
// its context names. Both are server-chosen and neither is content, so a label
// past this is a label being used as a payload. It is small on purpose — the
// leading-block exemption in capMCPBlocks is only honest if everything textual
// in the block has been capped, and capping these at the answer budget would let
// one exempt block carry three of them. A URI longer than this is truncated in
// the title a model reads; the resource itself is unaffected.
const maxResourceLabel = 2 << 10

// capLabel bounds one of those, cutting on a rune boundary. json.Marshal would
// coerce a rune split by a byte-wise cut to U+FFFD, which is a silent corruption
// where a visible marker is honest.
func capLabel(s string) string {
	s = toolset.SanitizeText(s)
	if len(s) <= maxResourceLabel {
		return s
	}
	cut := maxResourceLabel
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "[truncated]"
}

// What a block may declare is the intersection of two schemas, not the one this
// event is written against. The session event takes a free string for an image
// source's media type and for a document's; the brain replays a result's content
// array into a Messages tool_result *unchanged* (brain/replay.go's
// toolResultBlock), and there the image source's media type is an enum of four
// and a base64 document's is the single constant application/pdf. A block the
// platform can store but not send again fails not this turn but every later one,
// on an append-only log — so what the log may hold is what both schemas admit.
var imageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

const pdfMediaType = "application/pdf"

// mcpToolMetricName is what every MCP call is counted under. See the call site
// for why it is one value rather than the tool's own name.
const mcpToolMetricName = "mcp"

// imageBlock carries image bytes when the wire can carry them and describes them
// when it cannot. A server's mimeType is neither trusted into the enum nor
// guessed at from the bytes: what a model is looking at is the server's to
// declare, and MCP requires it to (mimeType is required on image content), so a
// block without one — or with no bytes, or with a type this wire has no slot for
// — is described the way audio is rather than sent with a field the endpoint
// will reject.
func imageBlock(c mcp.Content) map[string]any {
	mime := capLabel(c.MIMEType)
	if len(c.Data) == 0 || !imageMediaTypes[mime] {
		return textBlock(fmt.Sprintf(
			"The tool returned %d bytes of image data (%s), which cannot be shown here.",
			len(c.Data), mimeOrUnknown(c.MIMEType)))
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mime,
			"data":       base64.StdEncoding.EncodeToString(c.Data),
		},
	}
}

func resourceBlock(c mcp.Content) map[string]any {
	title := capLabel(c.URI)
	// A resource with no bytes either way — an empty file read over MCP is as
	// ordinary here as it is through the built-in read — would leave a document
	// whose source holds nothing. This platform keeps an empty payload off the
	// log (the empty text block above is the case the wire is known to reject),
	// and the address is what was worth saying about it anyway.
	if len(c.Data) == 0 && toolset.SanitizeText(c.Text) == "" {
		return textBlock(fmt.Sprintf("The tool returned an empty resource: %s (%s)",
			title, mimeOrUnknown(c.MIMEType)))
	}
	block := map[string]any{"type": "document", "title": title}
	if len(c.Data) > 0 {
		// A blob rides whichever block type can carry its bytes, and the two
		// that can are narrow: a base64 document source is application/pdf and
		// nothing else, and an image source is one of four. A resource that is
		// neither is described — with its address, which is the part worth
		// keeping — rather than sent under a media type the endpoint rejects.
		mime := capLabel(c.MIMEType)
		switch {
		case mime == pdfMediaType:
			block["source"] = map[string]any{
				"type":       "base64",
				"media_type": pdfMediaType,
				"data":       base64.StdEncoding.EncodeToString(c.Data),
			}
			return block
		case imageMediaTypes[mime]:
			return imageBlock(c)
		default:
			return textBlock(fmt.Sprintf(
				"The tool returned %d bytes of %s at %s, which cannot be shown here.",
				len(c.Data), mimeOrUnknown(c.MIMEType), title))
		}
	}
	block["source"] = map[string]any{
		"type":       "text",
		"media_type": "text/plain",
		"data":       toolset.CapOutput(toolset.SanitizeText(c.Text)),
	}
	// The schema's plain-text source admits exactly one media type, so a
	// resource declaring another is carried as context rather than dropped:
	// "text/markdown" changes how a model should read the same bytes.
	if mime := capLabel(c.MIMEType); mime != "" && mime != "text/plain" {
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
// retry status this platform emits, not the bare string the field's name invites
// — and it is always `retrying`, including for an egress refusal that no later
// turn can fix. The union is about the *session*, not about whether a retry
// would help: the SDK documents `terminal` as "The session encountered a
// terminal error and will transition to `terminated` state", and this platform's
// session does no such thing — the call is answered is_error and the turn
// carries on. `exhausted` is likewise the brain's ("this turn is dead; queued
// inputs are flushed"), not a tool call's. So `retrying` is the only variant that
// does not assert something this platform will then contradict, and it is true on
// its own terms: every turn re-attempts a failed server. Plan 29's own reading of
// the union — terminal for a refusal — was measured against these three doc
// comments and dropped (docs/DIVERGENCES.md).
func mcpConnectionFailedEvent(server, message string) (events.NewEvent, error) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":            "mcp_connection_failed_error",
			"mcp_server_name": server,
			"message":         message,
			"retry_status":    map[string]any{"type": "retrying"},
		},
	})
	if err != nil {
		return events.NewEvent{}, fmt.Errorf("marshal session error: %w", err)
	}
	return events.NewEvent{Type: domain.EventSessionError, Payload: payload}, nil
}
