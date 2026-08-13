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

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
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
	var sessionArchivedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT e.kind, s.status, s.environment_id, s.archived_at
		 FROM sessions s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $1 FOR UPDATE OF s`,
		id).Scan(&envKind, &status, &envID, &sessionArchivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	// Checked before any side effect (the rubric snapshot writes a blob), not
	// only at append time: a rejected send must leave nothing behind.
	if sessionArchivedAt != nil {
		return nil, errInvalid("session %s is archived and read-only", id)
	}

	newEvents, err := events.NormalizeInbound(envKind, rawEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	// A tool result must answer an outstanding tool call. The log is
	// append-only: accepting a result with a wrong, unknown, or duplicate
	// reference would poison every future replay with a request the model
	// protocol rejects, permanently wedging the session — so bad references
	// are the client's 400, not the session's funeral. IsWebTool marks the
	// calls only the platform's web driver answers, which closes the
	// scan-to-commit double-answer window on self_hosted (#222).
	if err := events.ValidateToolResults(ctx, tx, domain.ID(id), newEvents, toolset.IsWebTool); err != nil {
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
	// One active outcome at a time, and a file rubric must name a stored,
	// rubric-sized file — DB-backed like the tool-result cross-checks. An
	// interrupt in the same batch settles the active outcome first (its case
	// below), which is the documented way to chain a new outcome, so it
	// clears the stored-entry half of the check — but only when the interrupt
	// can actually settle (idle or running): the waiver shares the settling
	// case's own guard rather than silently depending on it.
	interruptCanSettle := hasInterrupt &&
		(status == string(domain.SessionIdle) || status == string(domain.SessionRunning))
	if err := events.ValidateDefineOutcomes(ctx, tx, domain.ID(id), newEvents, interruptCanSettle); err != nil {
		return nil, errInvalid("%s", err)
	}
	defs, err := events.DefineOutcomes(newEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	hasDefineOutcome := len(defs) > 0
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
	// Set by the interrupt case when a non-terminal outcome entry must flip to
	// interrupted; consumed by the MutateOutcomes composition after the switch.
	var outcomeFlip bool

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
		// to end, so the event is logged and settles no turn (a non-terminal
		// outcome still settles below — the flip does not depend on settling). Emitting a
		// session.status_idle for a session that never left idle would announce a
		// transition that did not happen.
		settling := interruptible && (status == string(domain.SessionRunning) || len(abandoned) > 0)
		if settling {
			results, err := events.InterruptResults(abandoned)
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
		// An active outcome settles with the turn — the docs mark it
		// interrupted "even if evaluation hadn't started yet", with an empty
		// outcome_evaluation_start_id when no start fired — freeing the
		// session for a new define_outcome, possibly one in this same batch
		// (the documented chaining pattern).
		if interruptible {
			ends, flip, err := events.InterruptOutcomes(ctx, tx, domain.ID(id))
			if err != nil {
				return nil, err
			}
			batch = append(batch, ends...)
			outcomeFlip = flip
		}
		// The interrupt leaves nothing outstanding, so a user.message — or a
		// new user.define_outcome — in the same batch resumes exactly as it
		// would on any idle session: the documented way to steer a running
		// agent, or to chain outcomes, in one send.
		if (hasUserMessage || hasDefineOutcome) && interruptible {
			batch = append(batch, events.NewEvent{Type: domain.EventSessionStatusRunning})
			setStatus(domain.SessionRunning)
		}
		if settling || ((hasUserMessage || hasDefineOutcome) && interruptible) {
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				// Cancel first, then enqueue: the cancel frees the (session,
				// kind) slot the redirect's own model_turn needs, and takes the
				// interrupted turn away from whoever was running it.
				if settling {
					if err := s.queue.CancelSession(ctx, tx, domain.ID(id)); err != nil {
						return err
					}
				}
				if hasUserMessage || hasDefineOutcome {
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
		denyResults, deniedIDs, err := events.DenialResults(ctx, tx, domain.ID(id), newEvents)
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
		// Resume the right work. The executor runs only platform built-ins, so
		// their work item is enqueued only when an allowed one is still
		// unanswered (denials are already answered) — as web_exec when any of
		// them is a web tool, else tool_exec, the same web-first choice the
		// brain's settlement makes and for the same reason: a tool_exec is
		// visible to a BYOC worker, which implements only the six sandbox tools
		// and must not see the log while a web call is outstanding. If the only
		// remaining unanswered tools are client-executed custom tools, enqueue
		// nothing — the client's user.custom_tool_result resumes the turn
		// (mirroring the non-ask suspend, which never runs an executor for a
		// custom-only turn). If every tool is answered (all gated tools
		// denied), resume the brain directly.
		//
		// This site knows nothing about MCP yet, and an allowed MCP call would
		// resume into no work at all: UnansweredPlatformToolNames counts only
		// agent.tool_use, so it schedules nothing, while HasUnansweredToolUse
		// below counts the MCP call and so declines to wake the brain either.
		// The session is left running rather than idle, which is the worse of
		// the two — a running session also refuses archive and delete — and only
		// a user.interrupt gets out of it.
		//
		// Unreachable today: the brain stamps evaluated_permission inside its
		// agent.tool_use branch alone, classify() resolves no name to the MCP
		// event type, and a client may not post one, so no MCP call can be
		// gated. But the trigger is the first agent.mcp_tool_use *emitted*, not
		// the first one gated — an ungated MCP call reaches the same two
		// queries by the paths that settle a turn — so the mcp_exec arm of the
		// four-way settlement is owed to that emission, wherever it lands.
		//
		// Landing both in one commit is necessary and not sufficient: the brain
		// and the control plane are separate deployments, so a rollout runs a
		// new brain against an old control plane (and a rollback the reverse).
		// A brain that emits an MCP call while some control-plane replica still
		// runs this version strands whatever that call was meant to resume. The
		// scheduling side has to be deployable first — either released ahead of
		// the producer, or written so an unrecognized outstanding call resumes
		// something rather than nothing.
		// Answered: the calls this batch denies, and the calls its own results
		// answer. A client may confirm and post an outstanding result in one
		// send, and that result is validated and about to be appended — as good
		// as answered, which is why the two sibling arms of this switch pass
		// ToolResultRefs too. Counting only the denials reads such a call as
		// outstanding, and both decisions below then go wrong in opposite
		// directions: a platform call answered here would run an executor pass
		// with nothing to do, and a client-executed one would leave the turn
		// unresumed — committed running, everything answered, nothing queued,
		// and no later trigger, since the tool-result trigger fires on a
		// subsequent send the client has no reason to make.
		answered := append(deniedIDs, events.ToolResultRefs(newEvents)...)
		platformPending, err := events.UnansweredPlatformToolNames(ctx, tx, domain.ID(id), answered)
		if err != nil {
			return nil, err
		}
		if len(platformPending) > 0 {
			kind := queue.ToolExec
			for _, name := range platformPending {
				if toolset.IsWebTool(name) {
					kind = queue.WebExec
					break
				}
			}
			opts.Then = func(ctx context.Context, tx pgx.Tx) error {
				_, err := s.queue.Enqueue(ctx, tx, envID, domain.ID(id), kind)
				return err
			}
			break
		}
		anyPending, err := events.HasUnansweredToolUse(ctx, tx, domain.ID(id), answered)
		if err != nil {
			return nil, err
		}
		if !anyPending {
			opts.Then = enqueueTurn
		}
	case (hasUserMessage || hasDefineOutcome) && status == string(domain.SessionIdle) && len(askBlocking) == 0:
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
			slog.WarnContext(ctx, "wake event not resumed: session is idle with an unanswered tool_use",
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

	// The outcome projection moves with the events that change it, under the
	// same lock: an interrupt flips every non-terminal entry to interrupted;
	// an accepted define_outcome appends its entry, born pending.
	if outcomeFlip || hasDefineOutcome {
		flip := events.FlipNonTerminalOutcomes(time.Now().UTC())
		opts.MutateOutcomes = func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
			if outcomeFlip {
				var err error
				if evals, err = flip(evals); err != nil {
					return nil, err
				}
			}
			for _, d := range defs {
				evals = append(evals, events.NewOutcomeEntry(d))
			}
			return evals, nil
		}
	}
	if err := s.snapshotRubrics(ctx, defs); err != nil {
		return nil, err
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

// snapshotRubrics copies each file rubric's bytes to an outcome-owned blob
// key at acceptance, so deleting the source file mid-outcome cannot break
// replay or grading. A snapshot orphaned by a failed commit is harmless —
// keyed by an outcome id that never came to exist.
func (s *server) snapshotRubrics(ctx context.Context, defs []events.DefineOutcome) error {
	for _, d := range defs {
		if d.RubricType != "file" {
			continue
		}
		if s.blobs == nil {
			return errInvalid("file rubrics require the files surface, which this deployment does not configure")
		}
		rc, size, err := s.blobs.Get(ctx, blob.FilesKey(d.RubricFileID))
		if err != nil {
			return fmt.Errorf("read rubric file %s: %w", d.RubricFileID, err)
		}
		err = s.blobs.Put(ctx, events.RubricSnapshotKey(d.OutcomeID), rc, size, "application/octet-stream")
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("snapshot rubric for %s: %w", d.OutcomeID, err)
		}
	}
	return nil
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
