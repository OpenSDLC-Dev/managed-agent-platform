package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	// thread is the call's thread (empty for the primary) and crossPosted
	// whether it was cross-posted: the result answering it is written the
	// same way, and the endpoint it names is resolved from that thread's
	// agent (plan 35 decisions 2, 14).
	thread      domain.ID
	crossPosted bool
}

// runnableMCPToolUses returns the session's runnable agent.mcp_tool_use events
// — unanswered, and allowed or confirmed (plan 35 decision 5) — oldest first
// across every thread. Only the platform writes the answering
// agent.mcp_tool_result — a client may post neither shape, and no BYOC worker
// sees an MCP call — and a reclaim re-runs only what is still outstanding.
func (e *Executor) runnableMCPToolUses(ctx context.Context, sid domain.ID) ([]mcpToolUse, error) {
	uses, err := events.RunnableToolUses(ctx, e.pool, sid, domain.EventAgentMCPToolUse)
	if err != nil {
		return nil, fmt.Errorf("list mcp tool uses: %w", err)
	}
	var out []mcpToolUse
	for _, u := range uses {
		var body struct {
			Name          string          `json:"name"`
			MCPServerName string          `json:"mcp_server_name"`
			Input         json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(u.Payload, &body); err != nil {
			return nil, fmt.Errorf("mcp tool use %s: %w", u.ID, err)
		}
		out = append(out, mcpToolUse{
			id: u.ID, server: body.MCPServerName, name: body.Name, input: body.Input,
			thread: u.ThreadID, crossPosted: u.CrossPosted,
		})
	}
	return out, nil
}

// threadMCP is what one thread's MCP calls resolve against: the servers its
// agent declares (name → url) and the catalog listings it has (name → the
// url the listing was read at).
type threadMCP struct {
	declared map[string]string
	ready    map[string]string
}

// threadMCPFor resolves each thread named by calls to its declared servers
// and ready listings — the primary's from the session's resolved agent, a
// child's from its snapshot (decision 14).
func (e *Executor) threadMCPFor(ctx context.Context, sid domain.ID, primary []mcpServerRef, calls []mcpToolUse) (map[domain.ID]threadMCP, error) {
	out := map[domain.ID]threadMCP{}
	for _, c := range calls {
		if _, ok := out[c.thread]; ok {
			continue
		}
		declared, err := e.threadMCPServers(ctx, sid, c.thread, primary)
		if err != nil {
			return nil, err
		}
		ready, err := e.readyEndpoints(ctx, sid, c.thread)
		if err != nil {
			return nil, err
		}
		endpoints := make(map[string]string, len(declared))
		for _, s := range declared {
			endpoints[s.Name] = s.URL
		}
		out[c.thread] = threadMCP{declared: endpoints, ready: ready}
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
// A dead context stops the pass, and what happens then depends on which death it
// was. A lost lease or a shutdown settles nothing — the row is someone else's, or
// nothing may be trusted — and the reclaim re-runs the calls this pass had not
// answered. A *stall* is the exception this lane and the web driver share: the
// claimant still holds the item, and a call that returned ran in a third party's
// system, so its answer commits and only the rest is re-derived (#383).
// Discovery has no such exception: a listing is re-derivable, so it stays
// all-or-nothing whichever death it was.
func (e *Executor) runMCPTools(ctx context.Context, cfg domain.EnvironmentConfig,
	vaultIDs []string, threads map[domain.ID]threadMCP, spill *mcpSpiller,
	uses []mcpToolUse, progress func()) ([]events.NewEvent, error, error) {

	var out []events.NewEvent
	deadline := time.Now().Add(e.cfg.MCPPassTimeout)
	for i, u := range uses {
		// Per call, at the top of the iteration — the files.go rule: a call the
		// budget stopped short of still advances the pass (#383).
		progress()
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
		// Answered under a thread-scoped interrupt since the scan: skipped, and
		// cancelled on the keeper's beat if it happens mid-call (decision 9).
		if answered, err := events.Answered(ctx, e.pool, spill.sid, u.id); err != nil {
			return out, nil, fmt.Errorf("mcp tool %s (%s): answered check: %w", u.name, u.id, err)
		} else if answered {
			continue
		}
		res, failure := mcpAnswer{}, mcpFailure{}
		th := threads[u.thread]
		if endpoint, refusal := callEndpoint(th.declared, th.ready, u.server); refusal != "" {
			res = mcpFailed("%s", refusal)
		} else {
			var runErr error
			cctx, stop := e.answeredWatch(ctx, spill.sid, u.id, e.cfg.LeaseTTL/3)
			res, failure, runErr = e.runMCPTool(cctx, cfg, vaultIDs, endpoint, u)
			if stop() {
				continue
			}
			if runErr != nil {
				// A call this pass could not even attempt, for a reason that is
				// not the server's. If earlier calls in the pass were answered,
				// this leaves like a spent budget rather than like a fault: their
				// answers are committed and the item comes back for the rest,
				// because faulting would drop those answers on the floor and a
				// tool with a side effect would run a second time. With nothing
				// answered there is nothing to protect, so it faults — which
				// retries at the lease's pace rather than immediately, and is the
				// arm a persistently failing lookup settles into.
				if len(out) == 0 {
					return nil, runErr, nil
				}
				slog.ErrorContext(ctx, "executor: mcp pass cut short, its answers committed",
					"tool_use", u.id, "tool", u.name, "error", runErr)
				break
			}
		}
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
		// Spilled here rather than where the answer was rendered so that the
		// metric above measures the server and not this platform: a spill reaches
		// the sandbox endpoint and writes a file, and recording that as MCP
		// latency would page an operator about a server that answered promptly.
		//
		// The notice rides on top of the budget, as the dropped-block notice
		// does: it is this platform's line about the server's answer, and a
		// pointer to the spill file is worthless if the pointer is what gets cut.
		if notice := spill.write(ctx, u.id, res.content, res.lost); notice != "" {
			res.blocks = append(res.blocks, textBlock(notice))
		}
		ev, err := mcpResultEvent(u.id, res)
		if err != nil {
			return out, nil, err
		}
		ev.ThreadID, ev.CrossPosted = u.thread, u.crossPosted
		out = append(out, ev)
		if failure.message != "" {
			ev, err := mcpFailureEvent(u.server, failure)
			if err != nil {
				return out, nil, err
			}
			ev.ThreadID = u.thread
			out = append(out, ev)
		}
	}
	// The pass boundary: the report at the top of the loop covers every call but
	// the last one, whose answer would otherwise land inside the same silent
	// interval as the settlement behind it (#383).
	progress()
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
	// Progress is one call answered, not one pass finished. The pass is already
	// bounded whole by Config.MCPPassTimeout, which is well inside the stall
	// budget at both defaults (5m against 30m) — but the two knobs are
	// independently tunable, and a deployment that raises the pass budget past
	// the stall budget would otherwise have every pass cancelled at 30 minutes.
	// The settlement below keeps the calls that answered, so that converges
	// rather than looping — at the price of calling a third party's tool twice
	// for the one cut off in flight, every reclaim (#383).
	threads, err := e.threadMCPFor(ctx, item.SessionID, sess.mcpServers, calls)
	if err != nil {
		return err
	}
	kctx, keeper := e.queue.KeepLease(ctx, item, e.cfg.LeaseTTL, e.cfg.StallTimeout)
	spill := &mcpSpiller{exec: e, sid: item.SessionID, sess: sess}
	results, faultErr, runErr := e.runMCPTools(
		kctx, sess.envConfig, sess.vaultIDs, threads, spill, calls, keeper.Progress)
	kerr := keeper.Close()
	// A stall commits what answered, for the reason the tool_exec lane does: an
	// MCP call that returned ran in *someone else's system*, and discarding its
	// answer would have the reclaim call it a second time — a second write to a
	// third-party server, which is the one thing this settlement can prevent and
	// no retry can undo. Only the call the stall cut short and the ones behind it
	// are re-derived, and the Then below hands this very item back for them. A
	// lease genuinely lost still commits nothing.
	stalled := errors.Is(kerr, queue.ErrWorkStalled)
	if kerr != nil && !stalled {
		return fmt.Errorf("lease keeper: %w", kerr)
	}
	if runErr != nil {
		return runErr
	}
	if faultErr != nil && !stalled {
		return faultErr
	}
	if stalled && len(results) == 0 {
		// Nothing to commit, but the fault still names the call the stall cut
		// short — which with zero results is the *first* one, and the only place
		// its identity is written down.
		return fmt.Errorf("lease keeper: %w", stallFault(kerr, faultErr))
	}
	// Folded once, before the settlement, so every exit below carries both the
	// sentinel and the call the stall cut short — the failing-append path most
	// of all, being the one where nothing else records either (#383).
	if stalled {
		faultErr = stallFault(kerr, faultErr)
	}

	// The chain is settleDrain's: an MCP call still runnable hands this very
	// item back rather than enqueuing a second one (Enqueue is keyed over the
	// live states, so a fresh mcp_exec raised while this one is still active
	// would be dropped on conflict and the session would wait on work nobody
	// queued); the other kinds enqueue and complete; each thread whose calls
	// are all answered is woken.
	opts := events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			return e.settleDrain(ctx, tx, item, queue.MCPExec, false)
		},
	}
	if _, err := e.log.AppendWith(ctx, item.SessionID, results, opts); err != nil {
		// The stall rides along, as it does in the tool_exec lane: the answers
		// are lost and the item is left to the lease, and the append error alone
		// names neither the stall nor the call it cut short.
		if stalled {
			return fmt.Errorf("append mcp tool results: %w (keeper: %w)", err, faultErr)
		}
		return fmt.Errorf("append mcp tool results: %w", err)
	}
	// Committed, and still a stall: the item is settled but the run was cut
	// short, and the operator needs to know which of the two happened.
	//
	// Almost always the settlement above requeued, because a stall latched during
	// a *call* leaves that call unanswered — the ctx check after it returns runs
	// before the result is appended. One window escapes that: the answer's spill
	// to the sandbox sits between the two, so a tick landing during the last
	// call's spill latches a stall with every call answered, and the Then
	// completes the item. The error below is then a report and not a state — the
	// item is finished, nothing is requeued or billed twice — and the residue is
	// one "work item faulted" line about a pass that in fact finished. Accepted:
	// the alternative is a second flag threaded out of the Then to suppress a log
	// line, and a stall that reached that far is worth an operator's attention
	// either way (#383).
	if stalled {
		return fmt.Errorf("lease keeper: %w", faultErr)
	}
	return nil
}

// mcpAnswer is one call's outcome in the shape the log wants: the blocks that
// become the result's content, and whether the model should read it as an error.
type mcpAnswer struct {
	blocks  []map[string]any
	isError bool
	// content is the server's answer before this driver rendered or capped it,
	// and lost says the rendering did not carry all of it — a block truncated,
	// or a block the budget threw away. Both are carried out to the caller
	// rather than used here because the spill they feed runs outside the region
	// timed as the server's call (runMCPTools).
	content []mcp.Content
	lost    bool
}

func mcpFailed(format string, args ...any) mcpAnswer {
	return mcpAnswer{blocks: []map[string]any{textBlock(fmt.Sprintf(format, args...))}, isError: true}
}

// callEndpoint says where one outstanding call may be dialled, or — as the
// second return, in the words the model reads — why it may not be dialled at
// all. Nothing is dialled on a refusal: neither case is a connection that
// failed, so neither is worth a session.error either.
//
// Two things have to agree. The agent must still declare the server, because
// mcp_servers is one of the two mid-session-mutable fields and the turn that
// called this tool may be older than the spec. And the session's listing for it
// must have been read at that same url: the model was offered this tool because
// *that* endpoint published it, so dialling a new address would send a call
// built from one server's listing to a different server, under a name the second
// one never published — and whatever came back would be an answer to a question
// nobody asked. The patch that repoints a server already deletes the rows it
// invalidates, so ordinarily the listing is simply gone; this check does not
// depend on that delete having happened.
func callEndpoint(declared, ready map[string]string, server string) (string, string) {
	url := declared[server]
	if url == "" {
		return "", fmt.Sprintf("MCP server %q is no longer configured on this agent.", server)
	}
	if ready[server] != url {
		return "", fmt.Sprintf(
			"MCP server %q now points somewhere else than the server that offered this tool; "+
				"the call was not made.", server)
	}
	return url, ""
}

// runMCPTool answers one call at the endpoint callEndpoint cleared. The second
// return is a transport failure worth a session.error, empty when the call
// reached the server at all — including when the tool itself failed, which is a
// working server reporting a working failure.
//
// The connection is per call rather than per pass. A turn's MCP calls are few,
// a handshake is one round trip, and a connection each keeps one server's
// failure from touching another's call — the same reason discovery dials each
// server on its own.
func (e *Executor) runMCPTool(ctx context.Context, cfg domain.EnvironmentConfig,
	vaultIDs []string, endpoint string, u mcpToolUse) (mcpAnswer, mcpFailure, error) {

	host, err := mcpEndpointHost(endpoint)
	if err != nil {
		return mcpFailed("MCP server %q has an unusable url.", u.server), mcpFailure{}, nil
	}
	if !mcpEgressAllowed(cfg, host) {
		reason := egressRefusal(cfg, host)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, reason),
			mcpFailure{message: storableReason(reason, endpoint)}, nil
	}

	token, err := e.mcpBearer(ctx, vaultIDs, endpoint)
	if err != nil {
		if !credentialUnusable(err) {
			// The lookup failed, not the credential. Answering the call would
			// settle a transient failure permanently — the result event commits
			// and nothing re-runs it — and would tell the operator to look at a
			// credential that is fine. Faulting the item retries the whole call.
			return mcpAnswer{}, mcpFailure{}, fmt.Errorf("mcp credential for %q: %w", u.server, err)
		}
		msg := storableReason(err.Error(), endpoint)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, msg),
			mcpFailure{message: msg, authentication: true}, nil
	}

	conn, err := mcp.Connect(ctx, mcp.Config{
		URL: endpoint, HTTPClient: e.mcpCallHTTP(), BearerToken: token})
	if err != nil {
		msg := storableReason(err.Error(), endpoint, token)
		return mcpFailed("MCP server %q could not be reached: %s", u.server, msg),
			mcpFailure{message: msg, authentication: mcpAuthFailure(err)}, nil
	}
	defer conn.Close()

	res, err := conn.CallTool(ctx, u.name, u.input)
	if err != nil {
		msg := storableReason(err.Error(), endpoint, token)
		failure := mcpFailure{message: msg}
		switch {
		case mcpAuthFailure(err):
			// A credential the server refused, or one it required and this dial
			// did not carry. Either way the operator has something to fix, and
			// it is not the network.
			//
			// The two arms cannot both match today: mcp.Conn asks about the
			// refusal first and does not mark a refused call as answered, which
			// is where that precedence belongs — the client knows which of its
			// own markings applies. The order here agrees with it rather than
			// relying on it, and costs nothing; a refusal read as
			// ErrServerAnswered would tell the operator nothing at all.
			failure.authentication = true
		case errors.Is(err, mcp.ErrServerAnswered):
			// The server was reached and refused the call — an unknown tool, or
			// a request for input this platform cannot supply. The model is told
			// so it can stop calling that tool; the operator is not, because
			// mcp_connection_failed_error is the wire's word for a connection
			// that failed and there is no connection here to heal.
			failure = mcpFailure{}
		}
		return mcpFailed("MCP tool %q on server %q could not be run: %s", u.name, u.server, msg), failure, nil
	}
	// Before anything renders or spills it: both the result blocks and the spill
	// file are derived from this content, so scrubbing it once covers both.
	content := scrubContent(res.Content, token)
	blocks, lost := mcpResultBlocks(content)
	return mcpAnswer{blocks: blocks, isError: res.IsError, content: content, lost: lost}, mcpFailure{}, nil
}

// scrubContent replaces secrets in the three strings a server chooses inside a
// content block. Data is left alone: it is bytes rather than text — an image, an
// audio clip, a blob — and a substring replacement there would corrupt the
// payload without meaningfully covering a secret nothing reads back as text.
func scrubContent(content []mcp.Content, secrets ...string) []mcp.Content {
	if len(secrets) == 0 {
		return content
	}
	out := make([]mcp.Content, len(content))
	for i, c := range content {
		c.Text = scrubSecrets(c.Text, secrets...)
		c.MIMEType = scrubSecrets(c.MIMEType, secrets...)
		c.URI = scrubSecrets(c.URI, secrets...)
		out[i] = c
	}
	return out
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
func mcpResultBlocks(content []mcp.Content) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(content))
	truncated := false
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
			txt, cut := textBlockCut(c.Text)
			truncated = truncated || cut
			if txt["text"] != "" {
				out = append(out, txt)
			}
		case "image":
			out = append(out, imageBlock(c))
		case "resource":
			blk, cut := resourceBlockCut(c)
			truncated = truncated || cut
			out = append(out, blk)
		case "resource_link":
			out = append(out, textBlock(mcpLinkText(c)))
		case "audio":
			out = append(out, textBlock(mcpAudioText(c)))
		}
	}
	// A tool that answers with nothing at all still needs one block: the wire's
	// content is optional but an empty array is indistinguishable from a tool
	// that returned nothing, and a Messages endpoint rejects an empty text
	// block — which is what every later replay of this session would send.
	if len(out) == 0 {
		out = append(out, textBlock("The tool returned no content."))
	}
	capped, dropped := capMCPBlocks(out)
	return capped, truncated || dropped
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
// The spill notice that may follow it rides on top for the same reason
// (mcpspill.go): a pointer to the file holding the rest is worthless if the
// pointer is what gets cut. The two say different things and both can be true —
// this one counts blocks that left the answer, that one names where the answer's
// text went, and an image the budget dropped is in neither.
func capMCPBlocks(blocks []map[string]any) ([]map[string]any, bool) {
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
						"a tool result may put on this session's log.", len(blocks)-i, toolset.MaxOutputBytes))),
				true
		}
		budget -= len(raw)
		out = append(out, b)
	}
	return out, false
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
// resource rather than *from* it: the URI it is titled with, the media type its
// context names, and the same two where a block this wire cannot carry is
// described in a sentence instead — a resource link's address, an audio clip's
// or an image's media type. All are server-chosen and none is content, so a
// label past this is a label being used as a payload. It is small on purpose —
// the leading-block exemption in capMCPBlocks is only honest if everything
// textual in the block has been capped, and capping these at the answer budget
// would let one exempt block carry three of them: a described block is one text
// block, so an uncapped label there would take the whole budget under the
// exemption and the rest of the answer with it, with nothing over budget for the
// spill to catch. A URI longer than this is truncated in the title or the
// sentence a model reads; the resource itself is unaffected.
const maxResourceLabel = 2 << 10

// capLabel bounds one of those, cutting on a rune boundary. json.Marshal would
// coerce a rune split by a byte-wise cut to U+FFFD, which is a silent corruption
// where a visible marker is honest.
func capLabel(s string) string {
	s = toolset.SanitizeText(s)
	if len(s) <= maxResourceLabel {
		return s
	}
	return toolset.TruncateRunes(s, maxResourceLabel) + "[truncated]"
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
	blk, _ := resourceBlockCut(c)
	return blk
}

// resourceBlockCut is resourceBlock plus whether a text resource's body was
// capped on the way into its document source — the other half of what tells the
// spill an answer did not arrive whole.
func resourceBlockCut(c mcp.Content) (map[string]any, bool) {
	title := capLabel(c.URI)
	// A resource with no bytes either way — an empty file read over MCP is as
	// ordinary here as it is through the built-in read — would leave a document
	// whose source holds nothing. This platform keeps an empty payload off the
	// log (the empty text block above is the case the wire is known to reject),
	// and the address is what was worth saying about it anyway.
	if len(c.Data) == 0 && toolset.SanitizeText(c.Text) == "" {
		return textBlock(mcpEmptyResourceText(c)), false
	}
	block := map[string]any{"type": "document"}
	// The title is optional on the request side, so an absent one is a shape
	// the endpoint is known to take and an empty one is not. MCP requires a
	// resource to carry a URI, so only a server out of spec gets here.
	if title != "" {
		block["title"] = title
	}
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
			return block, false
		case imageMediaTypes[mime]:
			return imageBlock(c), false
		default:
			return textBlock(fmt.Sprintf(
				"The tool returned %d bytes of %s at %s, which cannot be shown here.",
				len(c.Data), mimeOrUnknown(c.MIMEType), title)), false
		}
	}
	clean := toolset.SanitizeText(c.Text)
	capped := toolset.CapOutput(clean)
	block["source"] = map[string]any{
		"type":       "text",
		"media_type": "text/plain",
		"data":       capped,
	}
	// The schema's plain-text source admits exactly one media type, so a
	// resource declaring another is carried as context rather than dropped:
	// "text/markdown" changes how a model should read the same bytes.
	if mime := capLabel(c.MIMEType); mime != "" && mime != "text/plain" {
		block["context"] = "The source resource declares its media type as " + mime + "."
	}
	return block, capped != clean
}

// textBlock is every text block this driver writes, and the one place a single
// block is bounded: capped to what a tool result may return to a model, with the
// same truncation notice every built-in tool's output carries. Capping here
// rather than in capMCPBlocks is what keeps the ordinary answer — one huge text
// block — truncated rather than dropped whole.
func textBlock(s string) map[string]any {
	blk, _ := textBlockCut(s)
	return blk
}

// textBlockCut is textBlock plus whether the cap took anything off, which is
// half of what tells the spill an answer did not arrive whole. CapOutput returns
// its input unchanged when it fits, so the comparison is exact rather than a
// length heuristic.
func textBlockCut(s string) (map[string]any, bool) {
	clean := toolset.SanitizeText(s)
	capped := toolset.CapOutput(clean)
	return map[string]any{"type": "text", "text": capped}, capped != clean
}

// mcpLinkText and mcpAudioText are the sentences a link and an audio block are
// described in — here and in the spill file both (mcpspill.go), so the file and
// the answer it stands in for never come to say different things about the same
// block.
func mcpLinkText(c mcp.Content) string {
	return fmt.Sprintf("The tool returned a link to a resource: %s (%s)",
		capLabel(c.URI), mimeOrUnknown(c.MIMEType))
}

func mcpEmptyResourceText(c mcp.Content) string {
	return fmt.Sprintf("The tool returned an empty resource: %s (%s)",
		capLabel(c.URI), mimeOrUnknown(c.MIMEType))
}

func mcpAudioText(c mcp.Content) string {
	return fmt.Sprintf("The tool returned %d bytes of audio (%s), which cannot be shown here.",
		len(c.Data), mimeOrUnknown(c.MIMEType))
}

// mimeOrUnknown is every media type this driver names in a sentence, and it caps
// as capLabel does and for the same reason: a media type is a label, and one
// that arrives as a payload would otherwise ride an exempt block for the whole
// budget.
func mimeOrUnknown(mime string) string {
	if m := capLabel(mime); m != "" {
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

// mcpFailure is what one call's dial left for the operator: the message a
// session.error carries, empty when there is nothing to heal, and which of the
// wire's two failures it was.
//
// The distinction is the reference's, and it is about cause rather than
// symptom: mcp_connection_failed_error is a server that "could not be reached
// (network error, timeout, or non-authentication HTTP failure)", while
// mcp_authentication_failed_error covers "the server rejected the credential
// from the attached vault, required authentication when no matching credential
// was configured, or an OAuth token refresh failed". The first two of those
// three arrive as the same 401 or 403 whether a token was sent or not, so one
// test answers both; the third is the credential this platform could not
// resolve at all, which never reaches the server and is an authentication
// failure all the same.
type mcpFailure struct {
	message        string
	authentication bool
}

// mcpFailureEvent reports a server the platform could not reach, or one that
// refused the session's credential. The message is already cut to scheme://host
// by the caller, for the reason a catalog row's is: an mcp_servers entry is
// customer-supplied and may carry a credential in its userinfo or its query.
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
// its own terms: every work cycle re-attempts a failed server. Plan 29's own reading of
// the union — terminal for a refusal — was measured against these three doc
// comments and dropped (docs/DIVERGENCES.md).
func mcpFailureEvent(server string, f mcpFailure) (events.NewEvent, error) {
	errType := "mcp_connection_failed_error"
	if f.authentication {
		errType = "mcp_authentication_failed_error"
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":            errType,
			"mcp_server_name": server,
			"message":         f.message,
			"retry_status":    map[string]any{"type": "retrying"},
		},
	})
	if err != nil {
		return events.NewEvent{}, fmt.Errorf("marshal session error: %w", err)
	}
	return events.NewEvent{Type: domain.EventSessionError, Payload: payload}, nil
}
