package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/jackc/pgx/v5"
)

// Session events: POST (send, batch), GET (list, cursor-paged), and the SSE
// stream. Wire shapes follow the reference SDK exactly — see the events
// package for the inbound contract and docs/DIVERGENCES.md for the documented v1
// divergences.

// sendSessionEvents implements POST /v1/sessions/{id}/events. The body is
// always a batch ({"events":[…]}); the response echoes the persisted events
// as {"data":[…]} with server-assigned ids.
func (s *server) sendSessionEvents(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))

	body, err := decodeObject(r)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownKeys(body, "events"); err != nil {
		return nil, err
	}
	rawEvents, err := rawList(body["events"], "events")
	if err != nil {
		return nil, err
	}
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}

	// The whole send is one transaction: the session row lock is taken up
	// front (FOR UPDATE OF s) so the state-machine decision — flip to
	// running? enqueue a turn? — is made against a status no concurrent
	// send can move underneath us, and commits atomically with the append.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// user.tool_result is only valid on self_hosted environments, so the
	// batch is validated against the session's environment kind.
	var envKind, status string
	var envID domain.ID
	err = tx.QueryRow(ctx,
		`SELECT e.kind, s.status, s.environment_id
		 FROM sessions s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $1 FOR UPDATE OF s`,
		id).Scan(&envKind, &status, &envID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	newEvents, err := events.NormalizeInbound(envKind, rawEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	// A tool result must answer an outstanding tool call. The log is
	// append-only: accepting a result with a wrong, unknown, or duplicate
	// reference would poison every future replay with a request the model
	// protocol rejects, permanently wedging the session — so bad references
	// are the client's 400, not the session's funeral.
	if err := events.ValidateToolResults(ctx, tx, domain.ID(id), newEvents); err != nil {
		return nil, errInvalid("%s", err)
	}
	// A confirmation must name a tool use still awaiting one; like a tool
	// result, a bad reference on the append-only log would wedge the resume.
	if err := events.ValidateToolConfirmations(ctx, tx, domain.ID(id), newEvents); err != nil {
		return nil, errInvalid("%s", err)
	}

	// State-machine triggers (the session's turn scheduler, per the plan's
	// "enqueue model-turn" arrow): a user.message wakes an idle session —
	// flip to running, say so on the log, queue a turn. A tool result while
	// running resumes the suspended turn — but only when it completes the
	// set: the model protocol requires every tool_use answered in the next
	// turn, so partial results of a parallel tool call keep waiting. The
	// session never left running, so no new status event. A tool confirmation
	// resolves a requires_action suspension, and an interrupt ends the turn in
	// progress — both in the cases below. Everything else only appends (a
	// user.message mid-turn is picked up by the brain's end-of-turn watermark
	// check).
	var hasUserMessage, hasToolResult, hasConfirmation, hasInterrupt bool
	for _, ev := range newEvents {
		switch ev.Type {
		case domain.EventUserMessage:
			hasUserMessage = true
		case domain.EventUserToolResult, domain.EventUserCustomToolRes:
			hasToolResult = true
		case domain.EventUserToolConfirm:
			hasConfirmation = true
		case domain.EventUserInterrupt:
			hasInterrupt = true
		}
	}
	batch := newEvents
	var opts events.AppendOptions
	// The status transitions this batch actually makes, in order, recorded once
	// the commit that made them lands. Usually one — but an interrupt that a
	// user.message in the same batch redirects moves the column twice
	// (running → idle → running), and SetStatus can only carry the final value.
	var moves []domain.SessionStatus
	setStatus := func(st domain.SessionStatus) {
		moves = append(moves, st)
		final := st
		opts.SetStatus = &final
	}
	// Set when this batch clears the last requires_action gate: the seconds the
	// session waited on the human, measured in the database so both ends read one
	// clock, and recorded only after the resuming transaction commits.
	var approvalWaitSeconds *float64
	enqueueTurn := func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.queue.Enqueue(ctx, tx, envID, domain.ID(id), queue.ModelTurn)
		return err
	}

	// The confirmation gate: the ask-gated tool uses this session is still
	// blocked on after applying this batch's confirmations. While it is
	// non-empty the session stays idle on requires_action — only a confirmation
	// that clears the LAST ask resumes it. A user.message (or any other input)
	// posted meanwhile appends and waits for the next replay: waking the turn
	// past an unresolved tool_use would replay a request the model protocol
	// rejects, and requires_action resolves only by confirmation
	// (BetaManagedAgentsSessionRequiresAction). Empty for a session with no
	// gated tools, so it costs the common path only one indexed query.
	askBlocking, err := events.UnconfirmedAskEvents(ctx, tx, domain.ID(id), events.ToolConfirmationRefs(newEvents))
	if err != nil {
		return nil, err
	}

	// Order matters twice here. The interrupt comes first because it ends the
	// turn in progress, so a batch carrying one settles on its terms whatever
	// else the batch says: a confirmation alongside it does not run its tool (the
	// user asked to stop), and a user.message alongside it is the documented
	// redirect, handled inside that case rather than by the message trigger.
	// Confirmation then comes before the message for the original reason: a batch
	// that mixes the two must resolve the gate and run the confirmed tools, not
	// wake the turn on the message past a tool the confirmation just cleared.
	//
	// Only the interrupt case ignores askBlocking, because it answers every
	// outstanding call, gated or not — the set askBlocking would report is
	// exactly the set that case is about to clear.
	switch {
	case hasInterrupt:
		// Every tool call still outstanding is answered with an error result.
		// The model protocol requires every tool_use answered before the
		// conversation continues and the log is append-only, so a call left
		// abandoned would poison every future replay — which is the dead end the
		// interrupt exists to escape, not one it may create. The batch's own
		// results count as answered, exactly as they do for the message trigger.
		abandoned, err := events.UnansweredToolUses(ctx, tx, domain.ID(id), events.ToolResultRefs(newEvents))
		if err != nil {
			return nil, err
		}
		// Only the two statuses v1 ever writes can be interrupted. Nothing sets
		// terminated or rescheduling today, and neither should be settled from
		// here if something one day does: terminated has ended and reviving it on
		// the redirect below would make this the one trigger that un-ends a
		// session — the user.message case guards against exactly that by
		// requiring idle — while rescheduling would need semantics no code has
		// defined yet, and guessing them could leave the column disagreeing with
		// the log.
		interruptible := status == string(domain.SessionIdle) || status == string(domain.SessionRunning)
		// Nothing to stop: an idle session with no outstanding call has no turn
		// to end, so the event is logged and settles nothing. Emitting a
		// session.status_idle for a session that never left idle would announce a
		// transition that did not happen.
		settling := interruptible && (status == string(domain.SessionRunning) || len(abandoned) > 0)
		if settling {
			results, err := interruptToolResults(abandoned)
			if err != nil {
				return nil, err
			}
			batch = append(batch, results...)
			// end_turn, not a stop reason of its own: the reference documents an
			// interrupted turn as ending on the same stop reason as one that
			// finishes by itself, and the idle stop_reason union has no
			// interruption variant to carry (docs/DIVERGENCES.md).
			stop, err := json.Marshal(map[string]any{"stop_reason": map[string]any{"type": "end_turn"}})
			if err != nil {
				return nil, err
			}
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: stop})
			// The event is emitted whenever a turn ends — a stranded or
			// gate-blocked session is already idle and its clients still need the
			// new stop reason — but the column only moves when the session was
			// really running. Writing idle over idle would count a transition
			// that did not happen, the way a running→running reclaim would.
			if status == string(domain.SessionRunning) {
				setStatus(domain.SessionIdle)
			}
		}
		// The interrupt leaves nothing outstanding, so a user.message in the same
		// batch resumes exactly as it would on any idle session — the documented
		// way to steer a running agent, in one send.
		if hasUserMessage && interruptible {
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusRunning})
			setStatus(domain.SessionRunning)
		}
		if settling || (hasUserMessage && interruptible) {
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				// Cancel first, then enqueue: the cancel frees the (session,
				// kind) slot the redirect's own model_turn needs, and takes the
				// interrupted turn away from whoever was running it.
				if settling {
					if err := s.queue.CancelSession(ctx, tx, domain.ID(id)); err != nil {
						return err
					}
				}
				if hasUserMessage {
					return enqueueTurn(ctx, tx)
				}
				return nil
			}
		}
	case hasConfirmation && status == string(domain.SessionIdle):
		// A requires_action suspension resolves. Each denial is answered with
		// an error result (the model protocol requires every tool_use answered
		// before the turn resumes; the denial shape is an inference — see
		// docs/DIVERGENCES.md). If confirmations remain outstanding, the session re-idles
		// with the shrunken blocking set; once the last ask is resolved it
		// resumes — running an executor for any still-unanswered allowed tool,
		// or the brain directly when every gated tool was denied.
		denyResults, deniedIDs, err := denyToolResults(newEvents)
		if err != nil {
			return nil, err
		}
		batch = append(batch, denyResults...)

		if len(askBlocking) > 0 {
			stop, err := json.Marshal(map[string]any{"stop_reason": map[string]any{
				"type": "requires_action", "event_ids": askBlocking,
			}})
			if err != nil {
				return nil, err
			}
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusIdle, Payload: stop})
			break
		}
		batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusRunning})
		setStatus(domain.SessionRunning)
		// How long the gate held: the elapsed since the suspension that raised it —
		// the most recent requires_action idle. Measured under the same row lock the
		// resume commits under, so it reads a consistent log, and in the database so
		// both ends read one clock.
		var secs float64
		err = tx.QueryRow(ctx,
			`SELECT EXTRACT(EPOCH FROM (clock_timestamp() - created_at))
			 FROM events
			 WHERE session_id = $1 AND type = $2
			   AND payload->'stop_reason'->>'type' = 'requires_action'
			 ORDER BY seq DESC LIMIT 1`,
			id, string(domain.EventSessionStatusIdle)).Scan(&secs)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			approvalWaitSeconds = &secs
		}
		// Resume the right work. The executor runs only platform built-ins, so a
		// tool_exec is enqueued only when an allowed one is still unanswered
		// (denials are already answered). If the only remaining unanswered tools
		// are client-executed custom tools, enqueue nothing — the client's
		// user.custom_tool_result resumes the turn (mirroring the non-ask suspend,
		// which never runs an executor for a custom-only turn). If every tool is
		// answered (all gated tools denied), resume the brain directly.
		platformPending, err := events.HasUnansweredPlatformToolUse(ctx, tx, domain.ID(id), deniedIDs)
		if err != nil {
			return nil, err
		}
		if platformPending {
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				_, err := s.queue.Enqueue(ctx, tx, envID, domain.ID(id), queue.ToolExec)
				return err
			}
			break
		}
		anyPending, err := events.HasUnansweredToolUse(ctx, tx, domain.ID(id), deniedIDs)
		if err != nil {
			return nil, err
		}
		if !anyPending {
			opts.Then = enqueueTurn
		}
	case hasUserMessage && status == string(domain.SessionIdle) && len(askBlocking) == 0:
		// Waking on a message must not step past an unanswered tool call, for
		// the reason the two enqueue sites above gate on the same check: the
		// resumed turn would replay an assistant tool_use that no tool_result
		// answers, a request the model protocol rejects. askBlocking catches
		// only the ask-gated ones, so an allow-policy tool needs this. The
		// batch's own results count as answered — this runs before the append,
		// and a client repairing a session posts the outstanding result and
		// its next message together — exactly as the two siblings pass theirs.
		//
		// With the brain classifying every tool-carrying turn as a suspension
		// (#181), an idle session should have nothing outstanding, so refusing
		// here means a log stranded before that fix. It is logged rather than
		// silent: the message appends unprocessed and the session stays idle,
		// which no later tool result revives (that trigger requires a running
		// session). The way out is a user.interrupt in the same batch or before
		// it — the case at the top of this switch answers the outstanding call
		// and hands the session back resumable (#68).
		unanswered, err := events.HasUnansweredToolUse(ctx, tx, domain.ID(id), events.ToolResultRefs(newEvents))
		if err != nil {
			return nil, err
		}
		if unanswered {
			slog.WarnContext(ctx, "user.message not resumed: session is idle with an unanswered tool_use",
				"session_id", id)
			break
		}
		batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusRunning})
		setStatus(domain.SessionRunning)
		opts.Then = enqueueTurn
	case hasToolResult && status == string(domain.SessionRunning):
		unanswered, err := events.HasUnansweredToolUse(ctx, tx, domain.ID(id), events.ToolResultRefs(newEvents))
		if err != nil {
			return nil, err
		}
		if !unanswered {
			opts.Then = enqueueTurn
		}
	}

	appended, err := s.log.AppendInTx(ctx, tx, domain.ID(id), batch, opts)
	switch {
	case errors.Is(err, events.ErrSessionNotFound):
		return nil, errNotFound("session %s not found", id)
	case errors.Is(err, events.ErrSessionArchived):
		return nil, errInvalid("session %s is archived and read-only", id)
	case err != nil:
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// After the commit: a status change that rolled back did not happen. These are
	// the production transitions — idle→running (a user.message waking a session,
	// a confirmation clearing the last gate) and running→idle (an interrupt
	// ending the turn), in the order the batch made them.
	for _, st := range moves {
		events.RecordSessionStatus(ctx, st)
	}
	if approvalWaitSeconds != nil {
		events.RecordApprovalWait(ctx, *approvalWaitSeconds)
	}

	// The response echoes the posted events only, not the platform's
	// state-machine reaction (which clients observe on the stream/log).
	data := make([]any, 0, len(newEvents))
	for _, ev := range appended[:len(newEvents)] {
		wire, err := eventWire(ev)
		if err != nil {
			return nil, err
		}
		data = append(data, wire)
	}
	return map[string]any{"data": data}, nil
}

// denyToolResults turns each denied user.tool_confirmation in a batch into the
// agent.tool_result that answers its gated tool use: an error result carrying
// the client's deny_message (or a default). The model protocol requires every
// tool_use answered before the turn resumes, so a denied tool must have a
// result, or the next replay is a request the model rejects. It also returns
// the answered (denied) tool-use ids.
//
// The denial's result shape is an inference: the reference documents the
// confirmation event, not the result a denial produces (see docs/DIVERGENCES.md).
func denyToolResults(evs []events.NewEvent) ([]events.NewEvent, []string, error) {
	var results []events.NewEvent
	var deniedIDs []string
	for _, ev := range evs {
		if ev.Type != domain.EventUserToolConfirm {
			continue
		}
		var c struct {
			Result      string `json:"result"`
			ToolUseID   string `json:"tool_use_id"`
			DenyMessage string `json:"deny_message"`
		}
		if err := json.Unmarshal(ev.Payload, &c); err != nil {
			return nil, nil, err
		}
		if c.Result != "deny" {
			continue
		}
		msg := c.DenyMessage
		if msg == "" {
			// Never an empty text block: a Messages endpoint rejects one, and
			// that request is what the brain replays on resume.
			msg = "The user declined this tool call."
		}
		payload, err := json.Marshal(map[string]any{
			"tool_use_id": c.ToolUseID,
			"content":     []map[string]any{{"type": "text", "text": msg}},
			"is_error":    true,
		})
		if err != nil {
			return nil, nil, err
		}
		results = append(results, events.NewEvent{Type: domain.EventAgentToolResult, Payload: payload})
		deniedIDs = append(deniedIDs, c.ToolUseID)
	}
	return results, deniedIDs, nil
}

// interruptAnswer maps a tool-use event type to the result event that answers it
// when a user.interrupt abandons the call, and to the field that names the call.
// The families have to match — a custom tool's result is a
// user.custom_tool_result keyed by custom_tool_use_id, an MCP tool's an
// agent.mcp_tool_result keyed by mcp_tool_use_id — for the reason the denial
// synthesis is confined to agent.tool_use (internal/events/toolflow.go): a
// result of the wrong shape is not the answer a client watching that tool's
// family is waiting for. thread marks the one shape whose wire object carries a
// session_thread_id; the two agent.* results have no such field (verified
// against the SDK's event types).
var interruptAnswer = map[domain.EventType]struct {
	result domain.EventType
	refKey string
	thread bool
}{
	domain.EventAgentToolUse:       {domain.EventAgentToolResult, "tool_use_id", false},
	domain.EventAgentCustomToolUse: {domain.EventUserCustomToolRes, "custom_tool_use_id", true},
	domain.EventAgentMCPToolUse:    {domain.EventAgentMCPToolResult, "mcp_tool_use_id", false},
}

// interruptResultText is what an abandoned call is answered with. Never an empty
// text block: a Messages endpoint rejects one, and that request is what every
// later replay of this session sends.
const interruptResultText = "The user interrupted this tool call before it returned a result."

// interruptToolResults answers each tool call a user.interrupt abandons with an
// error result. The turn cannot end without them: the model protocol requires
// every tool_use answered before the conversation continues, so an unanswered
// call on the append-only log would make every future replay a request the model
// rejects — the wedge the interrupt exists to undo.
//
// The results are stamped processed on the spot. The platform wrote them, and
// the one that lands under an inbound type would otherwise render with a null
// processed_at, indistinguishable from a client event still queued behind
// earlier ones, and would look to a settling brain like input to chain a turn on.
//
// That an interrupt answers the calls at all, and in this shape, is an inference:
// the reference documents the interrupt's stop reason, not what it writes for
// the calls it abandons (docs/DIVERGENCES.md).
func interruptToolResults(uses []events.ToolUseRef) ([]events.NewEvent, error) {
	now := time.Now().UTC()
	out := make([]events.NewEvent, 0, len(uses))
	for _, use := range uses {
		answer, ok := interruptAnswer[use.Type]
		if !ok {
			// Unreachable while this map and the events package's tool-use
			// taxonomy agree; the check is what keeps them agreeing, since a new
			// tool-use type with no answer here would strand exactly the call
			// this function exists to release.
			return nil, fmt.Errorf("no result event answers a %s tool use", use.Type)
		}
		fields := map[string]any{
			answer.refKey: use.ID,
			"content":     []map[string]any{{"type": "text", "text": interruptResultText}},
			"is_error":    true,
		}
		if answer.thread {
			fields["session_thread_id"] = nil
		}
		payload, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		out = append(out, events.NewEvent{Type: answer.result, Payload: payload, ProcessedAt: &now})
	}
	return out, nil
}

// listSessionEvents implements GET /v1/sessions/{id}/events with the
// PageCursor envelope {"data":[…],"next_page":…} (no prev_page on events).
func (s *server) listSessionEvents(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	q := r.URL.Query()

	page, err := parsePageMax(q, maxEventLimit)
	if err != nil {
		return nil, err
	}
	query := events.ListQuery{Limit: page.limit + 1}
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		query.Desc = true
	default:
		return nil, errInvalid(`order must be "asc" or "desc"`)
	}
	if page.cur != nil {
		if !page.cur.seqKeyed {
			return nil, errInvalid("invalid page cursor")
		}
		// The cursor binds the direction it was minted under, so a
		// follow-up that omits ?order= keeps walking the same way — and
		// one that contradicts it is an error, not a silent restart.
		if q.Get("order") != "" && query.Desc != page.cur.seqDesc {
			return nil, errInvalid("order does not match the page cursor")
		}
		query.Desc = page.cur.seqDesc
		query.AfterSeq = &page.cur.seq
	}
	types := listParam(q, "types")
	for _, ty := range types {
		// types[] is a free-form filter (an unknown-but-storable value filters to
		// empty, see the test), so only the unstorable byte is rejected — before it
		// binds into the type = ANY(...) text[] and fails as a 500. See #135.
		if !storableText(ty) {
			return nil, errInvalid(`types values must not contain U+0000 or invalid UTF-8`)
		}
	}
	query.Types = types
	for key, dst := range map[string]**time.Time{
		"created_at[gt]": &query.CreatedGT, "created_at[gte]": &query.CreatedGTE,
		"created_at[lt]": &query.CreatedLT, "created_at[lte]": &query.CreatedLTE,
	} {
		t, err := parseTimeParam(q, key)
		if err != nil {
			return nil, err
		}
		*dst = t
	}

	if err := s.sessionExists(ctx, id); err != nil {
		return nil, err
	}

	evs, err := s.log.List(ctx, domain.ID(id), query)
	if err != nil {
		return nil, err
	}
	more := len(evs) > page.limit
	if more {
		evs = evs[:page.limit]
	}
	data := make([]any, 0, len(evs))
	for _, ev := range evs {
		wire, err := eventWire(ev)
		if err != nil {
			return nil, err
		}
		data = append(data, wire)
	}
	var next *string
	if more {
		c := encodeSeqCursor(query.Desc, evs[len(evs)-1].Seq)
		next = &c
	}
	return pageJSON{Data: data, NextPage: next}, nil
}

// streamSessionEvents implements GET /v1/sessions/{id}/events/stream: a live
// SSE tail of the session's log from connect time (reconnecting clients seed
// history through the list endpoint — the wire has no stream cursor).
// Frames are `event: <type>` + `data: <json>`; the reference client drops
// frames without a recognized event name, so the name always mirrors the
// payload's type. Previews (event_start/event_delta) are only sent for the
// types opted into via ?event_deltas[].
func (s *server) streamSessionEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		writeError(w, r, err)
		return
	}
	q := r.URL.Query()

	previews := make(map[string]bool)
	for _, v := range listParam(q, "event_deltas") {
		if !events.Previewable(domain.EventType(v)) {
			writeError(w, r, errInvalid(`event_deltas values must be "agent.message" or "agent.thinking"`))
			return
		}
		previews[v] = true
	}
	if err := s.sessionExists(ctx, id); err != nil {
		writeError(w, r, err)
		return
	}

	sub := s.broker.Subscribe(domain.ID(id))
	defer sub.Close()
	if err := s.broker.Ready(ctx); err != nil {
		writeError(w, r, err)
		return
	}
	// Snapshot the tail position after LISTEN coverage is active: anything
	// committed later is guaranteed a wake, so nothing can fall in between.
	var lastSeq int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1`, id).Scan(&lastSeq); err != nil {
		writeError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("content-type", "text/event-stream; charset=utf-8")
	h.Set("cache-control", "no-cache")
	h.Del("content-length")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	// event_delta frames carry only event_id, so remember each previewed
	// event's type from its event_start until the buffered event lands.
	// Aborted previews never land, so the tracker is capped.
	started := previewTracker{types: make(map[string]string)}
	ping := time.NewTicker(ssePingInterval)
	defer ping.Stop()

	// processFrame forwards one broadcast frame per the subscriber's
	// preview opt-in; true means the stream is over.
	processFrame := func(raw json.RawMessage) (terminate bool) {
		var frame struct {
			Type    string `json:"type"`
			EventID string `json:"event_id"`
			Event   struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"event"`
		}
		if json.Unmarshal(raw, &frame) != nil {
			return false
		}
		switch frame.Type {
		case "event_start":
			started.add(frame.Event.ID, frame.Event.Type)
			if !previews[frame.Event.Type] {
				return false
			}
		case "event_delta":
			if !previews[started.types[frame.EventID]] {
				return false
			}
		}
		writeSSEFrame(w, frame.Type, raw)
		flusher.Flush()
		// The deleted session's row is gone; nothing further can arrive.
		return frame.Type == "session.deleted"
	}

	// drainFrames forwards every queued frame. The wake path runs it before
	// writing buffered events: preview frames were broadcast before their
	// event committed (same NOTIFY connection, delivery in order), so
	// draining first keeps event_start ahead of the event it previews —
	// a bare select would order the two channels randomly.
	drainFrames := func() (terminate bool) {
		for {
			select {
			case raw := <-sub.Frames():
				if processFrame(raw) {
					return true
				}
			default:
				return false
			}
		}
	}

	// sessionGone backstops the best-effort session.deleted broadcast: if
	// that frame was lost (broker reconnect gap, full buffer), the row's
	// absence is the durable signal, and the stream must still terminate.
	sessionGone := func() bool {
		err := s.sessionExists(ctx, id)
		var apiErr *apiError
		return errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound
	}
	endDeleted := func() {
		frame, _ := json.Marshal(map[string]any{
			"id":           domain.NewID("sevt").String(),
			"type":         "session.deleted",
			"processed_at": time.Now().UTC(),
		})
		writeSSEFrame(w, "session.deleted", frame)
		flusher.Flush()
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-sub.Wake():
			if drainFrames() {
				return
			}
			wrote := 0
			for {
				evs, err := s.log.List(ctx, domain.ID(id), events.ListQuery{AfterSeq: &lastSeq, Limit: sseWakeBatch})
				if err != nil {
					writeErrorFrame(w, flusher)
					return
				}
				for _, ev := range evs {
					wire, err := eventWire(ev)
					if err != nil {
						writeErrorFrame(w, flusher)
						return
					}
					writeSSEFrame(w, string(ev.Type), wire)
					lastSeq = ev.Seq
					started.remove(ev.ID.String())
				}
				flusher.Flush()
				wrote += len(evs)
				if len(evs) < sseWakeBatch {
					break
				}
			}
			// An empty wake can mean the log vanished with its session.
			if wrote == 0 && sessionGone() {
				endDeleted()
				return
			}

		case raw := <-sub.Frames():
			if processFrame(raw) {
				return
			}

		case <-ping.C:
			if sessionGone() {
				endDeleted()
				return
			}
			writeSSEFrame(w, "ping", []byte(`{"type":"ping"}`))
			flusher.Flush()
		}
	}
}

// sseWakeBatch bounds how much backlog one wake materializes in memory; the
// wake path loops until it drains.
const sseWakeBatch = 500

// writeErrorFrame surfaces a mid-stream server failure as the protocol's
// error frame, so clients can tell a broken tail from an orderly end.
func writeErrorFrame(w io.Writer, flusher http.Flusher) {
	writeSSEFrame(w, "error", []byte(`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))
	flusher.Flush()
}

// previewTracker maps in-flight preview event ids to their types, bounded
// because aborted previews never reconcile.
type previewTracker struct {
	types map[string]string
	order []string
}

const previewTrackerCap = 256

func (p *previewTracker) add(id, typ string) {
	if _, ok := p.types[id]; !ok {
		p.order = append(p.order, id)
		if len(p.order) > previewTrackerCap {
			delete(p.types, p.order[0])
			p.order = p.order[1:]
		}
	}
	p.types[id] = typ
}

func (p *previewTracker) remove(id string) {
	delete(p.types, id)
}

// ssePingInterval keeps idle streams alive through proxies. The reference
// client skips ping frames wholesale.
var ssePingInterval = 15 * time.Second

// writeSSEFrame emits one server-sent event. The event name is required: the
// reference decoder dispatches on it and silently drops unnamed frames.
func writeSSEFrame(w io.Writer, name string, data []byte) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
}

// eventWire renders a stored event onto the wire: the type-specific payload
// fields merged with the id/type/processed_at envelope. Payload bytes pass
// through untouched, so content blocks round-trip exactly.
func eventWire(ev domain.Event) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(ev.Body, &out); err != nil {
		return nil, fmt.Errorf("event %s payload is corrupt: %w", ev.ID, err)
	}
	if out == nil {
		out = make(map[string]json.RawMessage)
	}
	// Marshals of plain strings and database timestamps cannot fail.
	out["id"], _ = json.Marshal(ev.ID.String())
	out["type"], _ = json.Marshal(string(ev.Type))
	out["processed_at"] = json.RawMessage("null")
	if ev.ProcessedAt != nil {
		out["processed_at"], _ = json.Marshal(ev.ProcessedAt.UTC())
	}
	return json.Marshal(out)
}

// sessionExists resolves list/stream 404s (session_ already normalized).
func (s *server) sessionExists(ctx context.Context, id string) error {
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("session %s not found", id)
	}
	return err
}

// listParam collects a repeatable array query parameter in both wire
// spellings, key[]=v (bracket serialization) and key=v.
func listParam(q url.Values, key string) []string {
	var out []string
	out = append(out, q[key+"[]"]...)
	out = append(out, q[key]...)
	return out
}
