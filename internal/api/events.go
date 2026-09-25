package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

// platformExecuted names the built-in tools no client may answer with a
// user.tool_result: the web tools only this platform's web driver runs, and
// the six delegation tools the settlement answers in the commit that emits
// them (plan 35 decision 6). The second half is defence in depth — a
// delegation call is answered before any client can see it, so a forged
// answer is already refused as a duplicate — and it is worth having anyway,
// because the log is append-only and a forged child report is a report the
// coordinator would act on. It must never cover the sandbox six: a
// self_hosted worker answering those is the BYOC pull protocol.
func platformExecuted(name string) bool {
	return toolset.IsWebTool(name) || toolset.IsDelegationTool(name)
}

// wireHiddenTools are the calls no events list or stream of a session that
// delegates renders, nor the answers to them: the six delegation tools, of
// which the reference's lists show neither half on any surface (#675) — a
// spawn reads there as session.thread_created and the
// agent.thread_message_sent/_received pair. The rows stay in the log, where
// the thread's replay, the tool-result validation platformExecuted steers and
// the runnable classification read them.
var wireHiddenTools = toolset.AllDelegationTools()

// eventsView is what an events surface renders under, settled with its 404
// from the session row the request reads anyway.
type eventsView struct {
	// wide: the surface takes the self_hosted widening (plan 35 decision 13 i).
	wide bool
	// delegates: the session's snapshot carries a roster, so its surfaces hide
	// wireHiddenTools. Only there does the brain class the six names as
	// delegation calls — all six on every thread, the half a thread was never
	// offered included, so a call across the roles is hidden with its is_error
	// answer. On a single-agent session one of the names is an unknown tool
	// like any other (#567) and renders as one, so its lists skip the filter —
	// and the lookup it makes per tool result — outright.
	delegates bool
}

// apply sets the list-query fields the view decides.
func (v eventsView) apply(q *events.ListQuery) {
	q.ThreadToolCalls = v.wide
	if v.delegates {
		q.HideTools = wireHiddenTools
	}
}

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

	// user.tool_result is valid only under a worker's credential and only on
	// self_hosted environments, so the batch is validated against both.
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
	if err := requireNotDreamOwned(ctx, tx, id); err != nil {
		return nil, err
	}

	// A management credential's user.tool_result is the reference's 403, on
	// every session (#662); the transaction rolls back, so nothing lands.
	newEvents, err := events.NormalizeInbound(envKind, credentialFrom(ctx), rawEvents)
	if errors.Is(err, events.ErrEnvironmentCredentialRequired) {
		return nil, errForbidden(err.Error())
	}
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	// A tool result must answer an outstanding tool call. The log is
	// append-only: accepting a result with a wrong, unknown, or duplicate
	// reference would poison every future replay with a request the model
	// protocol rejects, permanently wedging the session — so bad references
	// are the client's 400, not the session's funeral. platformExecuted marks
	// the calls no client may answer, which closes the scan-to-commit
	// double-answer window on self_hosted (#222).
	if err := events.ValidateToolResults(ctx, tx, domain.ID(id), newEvents, platformExecuted); err != nil {
		return nil, errInvalid("%s", err)
	}
	// A confirmation must name a tool use still awaiting one; like a tool
	// result, a bad reference on the append-only log would wedge the resume.
	if err := events.ValidateToolConfirmations(ctx, tx, domain.ID(id), newEvents); err != nil {
		return nil, errInvalid("%s", err)
	}

	// Every inbound event addresses one thread (plan 35 decision 9): a
	// confirmation or result the thread of the call it answers (an explicit
	// session_thread_id must agree), an interrupt the thread it names or every
	// live thread, a message / define_outcome / system.message the primary.
	// Routed here, validated, before the triggers decide per thread.
	scoped, err := events.RouteInbound(ctx, tx, domain.ID(id), newEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}

	// Route input per thread, then settle its ordered tool flow under the same
	// session lock. Receipt prevents duplicates; processing controls blockers
	// and scheduling. Idle external waits and legacy running waits share this
	// path. The session's status is the fold over its live threads.
	type addressed struct{ interrupt, confirmation, toolResult bool }
	addr := map[domain.ID]*addressed{}
	at := func(tid domain.ID) *addressed {
		a := addr[tid]
		if a == nil {
			a = &addressed{}
			addr[tid] = a
		}
		return a
	}
	var hasUserMessage, hasInterrupt, interruptAll bool
	for i, ev := range newEvents {
		switch ev.Type {
		case domain.EventUserMessage:
			hasUserMessage = true
		case domain.EventUserToolResult, domain.EventUserCustomToolRes:
			at(ev.ThreadID).toolResult = true
		case domain.EventUserToolConfirm:
			at(ev.ThreadID).confirmation = true
		case domain.EventUserInterrupt:
			hasInterrupt = true
			if scoped[i] {
				at(ev.ThreadID).interrupt = true
			} else {
				interruptAll = true
			}
		}
	}
	threads, err := liveThreads(ctx, tx, id, status)
	if err != nil {
		return nil, err
	}
	if interruptAll {
		for _, th := range threads {
			at(th.id).interrupt = true
		}
	} else if hasInterrupt {
		// Interrupts that name every live thread one by one — the primary's
		// own id on a single-agent session, the documented echo of what a
		// client read off the status events — are the session-wide interrupt
		// spelled out: the same cancellation of every live item follows, so
		// the two requests the docs equate leave the work in one state.
		interruptAll = true
		for _, th := range threads {
			interruptAll = interruptAll && at(th.id).interrupt
		}
	}
	primaryStatus := status
	for _, th := range threads {
		if th.id == "" {
			primaryStatus = th.status
		}
	}
	// One active outcome at a time, and a file rubric must name a stored,
	// rubric-sized file — DB-backed like the tool-result cross-checks. An
	// interrupt in the same batch settles the active outcome first (its case
	// below), which is the documented way to chain a new outcome, so it
	// clears the stored-entry half of the check — but only when the interrupt
	// can actually settle (idle or running): the waiver shares the settling
	// case's own guard rather than silently depending on it. Outcomes belong
	// to the primary thread (decision 15), so only an interrupt that reaches
	// it settles one.
	interruptCanSettle := hasInterrupt && at("").interrupt &&
		(primaryStatus == string(domain.SessionIdle) || primaryStatus == string(domain.SessionRunning))
	if err := events.ValidateDefineOutcomes(ctx, tx, domain.ID(id), newEvents, interruptCanSettle); err != nil {
		return nil, errInvalid("%s", err)
	}
	defs, err := events.DefineOutcomes(newEvents)
	if err != nil {
		return nil, errInvalid("%s", err)
	}
	hasDefineOutcome := len(defs) > 0
	// The batch is laid out in processing order, not the order posted
	// (processingOrder), so the client's events are found again by id: minted
	// here rather than by the append.
	for i := range newEvents {
		if newEvents[i].ID == "" {
			newEvents[i].ID = domain.NewID(domain.PrefixEvent)
		}
	}
	// The platform's reaction, recorded by what processingOrder needs to place
	// it.
	layout := newSendLayout(newEvents)
	var opts events.AppendOptions
	// The status transitions this batch actually makes, in order, recorded once
	// the commit that made them lands. Usually one — but an interrupt that a
	// user.message in the same batch redirects moves the column twice
	// (running → idle → running), and SetStatus can only carry the final value.
	// TransitionThread reports each move it made to the session column.
	var moves []domain.SessionStatus
	moveTo := func(st *domain.SessionStatus) {
		if st != nil {
			moves = append(moves, *st)
			opts.SetStatus = st
		}
	}
	// wakeUnder moves one thread to running under the lock and records the
	// pair it emits as that thread's wake, which the input it wakes on follows.
	wakeUnder := func(t events.ThreadTransition) error {
		pair, moved, err := events.TransitionThread(ctx, tx, domain.ID(id), t)
		if err != nil {
			return err
		}
		layout.woke(t.ThreadID, pair)
		moveTo(moved)
		return nil
	}
	// Set when this batch clears a thread's last requires_action gate: the
	// seconds it waited on the human, measured in the database so both ends
	// read one clock, and recorded only after the resuming transaction commits.
	var approvalWaits []float64
	// The same-transaction work this batch schedules, run in order after the
	// append: cancels first (an interrupt frees the slot its redirect's own
	// model_turn needs, and takes the interrupted turn away from whoever was
	// running it), enqueues after.
	var cancels, thens []func(ctx context.Context, tx pgx.Tx) error
	enqueueTurn := func(tid domain.ID) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.queue.EnqueueThread(ctx, tx, envID, domain.ID(id), tid, queue.ModelTurn)
			return err
		}
	}
	enqueueExec := func(kind queue.Kind) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.queue.Enqueue(ctx, tx, envID, domain.ID(id), kind)
			return err
		}
	}
	// startWorkCycle is the primary's enqueueTurn where the turn begins a *new*
	// cycle rather than resuming a suspended one — an idle session woken by a
	// message or a new outcome, and the same pair redirecting a turn an
	// interrupt just ended.
	//
	// It re-attempts the MCP servers the last cycle could not reach, and this
	// delete is what makes it one. A failed catalog row is an answer rather than
	// an absence, so the brain runs its turn without that server instead of
	// suspending to re-dial an endpoint that just refused — which is right within
	// a cycle and wrong across them: the discovery driver runs only when a turn
	// suspends for a server with no row, so without this the first failure would
	// stand for the whole life of the session, however long ago it was and
	// whatever the operator has fixed since. Dropping the rows puts those servers
	// back in the state a turn suspends for, so the next turn re-dials them once,
	// and a server that is still down costs one dial per message rather than one
	// per turn. It is the cadence the reference documents — retry on the
	// session.status_idle → session.status_running transition — which is why the
	// two arms that make that transition on a new cycle are the two that call
	// this, and the resuming ones (a confirmation clearing the last gate, a tool
	// result completing the set) are not: their turn is already under way.
	startWorkCycle := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM mcp_catalogs WHERE session_id = $1 AND status = 'failed'`, id); err != nil {
			return err
		}
		return enqueueTurn("")(ctx, tx)
	}

	// Each denial is answered with an error result (the model protocol
	// requires every tool_use answered before the turn resumes; the denial
	// shape is an inference — see docs/DIVERGENCES.md), on the refused call's
	// thread; appended by the arm of that thread below.
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
	// Only the interrupt case ignores the thread's ask gate, because it answers
	// every outstanding call, gated or not — the set the gate would report is
	// exactly the set that case is about to clear.
	// Whether this batch stops the coordinator itself, read once before the loop
	// because the child arm below needs it and the loop must not decide it from
	// the order threads happen to come back in. It covers both spellings: a
	// session-wide interrupt reaches the primary through the expansion above,
	// and a thread-scoped one naming the primary keys straight to "".
	primaryInterrupted := at("").interrupt
	// The posted interrupt each interrupted thread's arm answers to: the first
	// received of those that reach it — the first that names the thread and
	// the first session-wide one, whichever came first. A later one finds the
	// thread already stopped, so what ending it writes belongs to the earlier.
	named, sessionWide := map[domain.ID]int{}, -1
	for i, ev := range newEvents {
		switch {
		case ev.Type != domain.EventUserInterrupt:
		case !scoped[i]:
			if sessionWide < 0 {
				sessionWide = i
			}
		default:
			if _, ok := named[ev.ThreadID]; !ok {
				named[ev.ThreadID] = i
			}
		}
	}
	interruptAt := func(tid domain.ID) int {
		if i, ok := named[tid]; ok && (sessionWide < 0 || i < sessionWide) {
			return i
		}
		return sessionWide
	}
	// The arms run in the order the send is processed, so the status events
	// their transitions emit read true where processingOrder lists them: the
	// primary's own arm first unless it is interrupted — its one move in the
	// batch is a message's wake, which a child's ending then finds already made
	// — then the interrupted threads in the order their interrupts were
	// received, then the rest, whose answers settle in Then. Were an
	// interrupted child's idle to run before a sibling's while listed after
	// it, a session.status_idle the second emits would precede the first
	// thread's idle in the list.
	armOrder := func(th threadState) int {
		switch {
		case at(th.id).interrupt:
			return interruptAt(th.id)
		case th.id == "":
			return -1
		}
		return len(newEvents)
	}
	slices.SortStableFunc(threads, func(a, b threadState) int { return cmp.Compare(armOrder(a), armOrder(b)) })
	for _, th := range threads {
		a, tid, status := at(th.id), th.id, th.status
		isPrimary := tid == ""
		switch {
		case a.interrupt:
			out, err := s.interruptThreadInTx(ctx, tx, interruptThreadIn{
				sessionID: domain.ID(id), threadID: tid, agentName: th.agentName,
				status: status, answered: events.ToolResultRefs(newEvents),
				all: interruptAll, primaryInterrupted: primaryInterrupted,
				resume: hasUserMessage || hasDefineOutcome,
			})
			if err != nil {
				return nil, err
			}
			layout.interrupted(interruptAt(tid), out)
			thens = append(thens, func(ctx context.Context, tx pgx.Tx) error {
				_, err := s.log.AdvanceThreadTools(ctx, tx, domain.ID(id), tid, platformExecuted)
				return err
			})
			for i := range out.moves {
				moveTo(&out.moves[i])
			}
			// Cancel first, then enqueue. A session-wide interrupt keeps
			// today's CancelSession exactly — every live item of every kind, so
			// the driver's own context is cancelled and the in-flight sandbox
			// command with it, and one cancel covers every thread the loop
			// interrupts; a thread-scoped one stops that thread's turn alone
			// and never the shared exec item a sibling's calls ride on, so
			// nothing cancels the driver and the answers written above are what
			// tells it: both drivers watch the call they are running and drop
			// it once answered (decision 9, #441).
			if out.cancelSession && len(cancels) == 0 {
				cancels = append(cancels, func(ctx context.Context, tx pgx.Tx) error {
					return s.queue.CancelSession(ctx, tx, domain.ID(id))
				})
			}
			if out.cancelThread {
				cancels = append(cancels, func(ctx context.Context, tx pgx.Tx) error {
					return s.queue.CancelThread(ctx, tx, domain.ID(id), tid)
				})
			}
			if out.wakeParent {
				thens = append(thens, enqueueTurn(""))
			}
			if out.resumed {
				thens = append(thens, startWorkCycle)
			}
			outcomeFlip = outcomeFlip || out.outcomeFlip
		case (a.confirmation || a.toolResult) && (status == string(domain.SessionIdle) || status == string(domain.SessionRunning)):
			thens = append(thens, func(ctx context.Context, tx pgx.Tx) error {

				pendingApproval, err := events.PendingThreadApprovals(ctx, tx, domain.ID(id), tid)
				if err != nil {
					return err
				}
				flow, err := s.log.AdvanceThreadTools(ctx, tx, domain.ID(id), tid, platformExecuted)
				if err != nil {
					return err
				}

				if pendingApproval {
					secs, err := events.ClearedApprovalWait(ctx, tx, domain.ID(id), tid)
					if err != nil {
						return err
					}
					if secs != nil {
						approvalWaits = append(approvalWaits, *secs)
					}
				}
				moved, err := s.log.SettleToolFlow(ctx, tx, domain.ID(id), tid, flow)
				if err != nil {
					return err
				}
				moveTo(moved)
				kind, err := execKindFor(ctx, tx, domain.ID(id), nil, nil)
				if err != nil {
					return err
				}
				if kind != "" {
					if err := enqueueExec(kind)(ctx, tx); err != nil {
						return err
					}
				}
				if !flow.Unsettled {
					return enqueueTurn(tid)(ctx, tx)
				}
				return nil
			})
		case isPrimary && (hasUserMessage || hasDefineOutcome) && status == string(domain.SessionIdle):
			// Messages remain queued behind any unprocessed tool call.
			flow, err := events.ThreadToolFlow(ctx, tx, domain.ID(id), tid, platformExecuted)
			if err != nil {
				return nil, err
			}
			if flow.Unsettled {
				break
			}

			if err := wakeUnder(events.ThreadTransition{Status: domain.SessionRunning}); err != nil {
				return nil, err
			}
			thens = append(thens, startWorkCycle)

		}
	}
	// A child-scoped interrupt is consumed right here, by its arm: the child's
	// turn it ends is the only turn that could ever stamp it, so it is stamped
	// processed on append — now the arms have run, so no earlier than the
	// results its arm synthesized ahead of it (#539; AppendInTx keeps a
	// batch's stamps from running backwards). A session-wide one is the
	// primary's next turn's to stamp, as before.
	now := time.Now().UTC()
	for i := range newEvents {
		if newEvents[i].Type == domain.EventUserInterrupt && newEvents[i].ThreadID != "" {
			newEvents[i].ProcessedAt = &now
		}
	}
	batch := layout.processingOrder()
	if interruptAll {
		batch = keepLastSessionIdle(batch)
	}
	if len(cancels)+len(thens) > 0 {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			for _, fn := range append(cancels, thens...) {
				if err := fn(ctx, tx); err != nil {
					return err
				}
			}
			return nil
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
	// The client's events, by the ids minted above: the batch is in processing
	// order, so they need not lead it.
	posted := make(map[domain.ID]*domain.Event, len(newEvents))
	for i := range appended {
		posted[appended[i].ID] = &appended[i]
	}
	// A tool reply may have been processed by Then, together with an earlier
	// queued reply. Echo the persisted timestamp rather than the pre-settlement
	// copy returned by the first append.
	//
	// An answer is consumed on receipt, and processingOrder lists it there, but
	// the settlement that stamps it runs in Then, after the append — later than
	// the rows this commit lists behind it. So an answer this commit consumed
	// takes a stamp inside its slot: no earlier than this commit's rows listed
	// ahead of it, no later than the first listed behind it, so the order and
	// the stamps say the same (#539). An answer still queued stays unstamped.
	//
	// One statement for the whole batch, whose RETURNING is the echo's reread:
	// the session's row lock is held, and a body can carry thousands of
	// answers. An answer's floor leaves the other answers out. Clamped, one
	// listed ahead of it could not raise the floor anyway — it is held at or
	// below its own ceiling, which counts this answer's stamp — and
	// unclamped, its stamp says only when its thread happened to settle. So
	// one pass over the commit's rows gives what clamping the answers one by
	// one, in list order, would.
	var answerIDs []string
	for _, ev := range newEvents {
		if ev.Type == domain.EventUserToolResult || ev.Type == domain.EventUserCustomToolRes || ev.Type == domain.EventUserToolConfirm {
			answerIDs = append(answerIDs, ev.ID.String())
		}
	}
	if len(answerIDs) > 0 {
		rows, err := tx.Query(ctx, `WITH answer AS (SELECT unnest($3::text[]) AS id),
		   slot AS (
		     SELECT e.id, e.processed_at, a.id IS NOT NULL AS is_answer,
		            MAX(e.processed_at) FILTER (WHERE a.id IS NULL)
		              OVER (ORDER BY e.seq ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS floor_at,
		            MIN(e.processed_at)
		              OVER (ORDER BY e.seq ROWS BETWEEN 1 FOLLOWING AND UNBOUNDED FOLLOWING) AS ceiling_at
		       FROM events e LEFT JOIN answer a ON a.id = e.id
		      WHERE e.session_id = $1 AND e.seq >= $2)
		 UPDATE events e SET processed_at = LEAST(GREATEST(slot.processed_at, slot.floor_at), slot.ceiling_at)
		   FROM slot
		  WHERE e.session_id = $1 AND e.id = slot.id AND slot.is_answer AND slot.processed_at IS NOT NULL
		 RETURNING e.id, e.processed_at`,
			id, appended[0].Seq, answerIDs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eid domain.ID
			var at *time.Time
			if err := rows.Scan(&eid, &at); err != nil {
				rows.Close()
				return nil, err
			}
			posted[eid].ProcessedAt = at
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
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
	for _, secs := range approvalWaits {
		events.RecordApprovalWait(ctx, secs)
	}

	// The response echoes the posted events only, in the order posted, not the
	// platform's state-machine reaction (which clients observe on the
	// stream/log, in processing order).
	data := make([]any, 0, len(newEvents))
	for _, ev := range newEvents {
		wire, err := eventWire(*posted[ev.ID], events.ScopeSession)
		if err != nil {
			return nil, err
		}
		data = append(data, wire)
	}
	return map[string]any{"data": data}, nil
}

// interruptThreadIn is one thread's slice of an interrupt: the thread itself
// and the four facts the arm reads from the send (or the runner) around it.
type interruptThreadIn struct {
	sessionID domain.ID
	threadID  domain.ID
	agentName string
	status    string
	// answered are the calls the same batch answers. A client may confirm and
	// post an outstanding result in one send, and that result is validated and
	// about to be appended — as good as answered.
	answered []string
	// all: every live thread of the session is interrupted, which is what
	// decides between cancelling the session's queued work and this thread's.
	all bool
	// primaryInterrupted: the coordinator is being interrupted in the same
	// batch, so a child ending here has nobody to tell.
	primaryInterrupted bool
	// resume: a user.message or user.define_outcome in the same batch redirects
	// the primary as soon as this interrupt has settled it.
	resume bool
}

// interruptThreadOut is what the arm leaves its caller to place: the events to
// append, by what processingOrder needs to place them, the status moves to
// record after the commit, and the work to schedule — cancels before enqueues,
// the order the caller keeps.
type interruptThreadOut struct {
	// settled is what the interrupt settles before it is itself processed:
	// the results it synthesizes for the calls it abandons and the ends of
	// the outcomes it interrupts (#539).
	settled []events.NewEvent
	// idled is this thread's idle pair, which follows the interrupt.
	idled []events.NewEvent
	// told is the notice a coordinator gets when this interrupt stopped its
	// child, with the coordinator's wake when the ending caused one.
	told *events.Delivery
	// resume is a redirect's running pair, which the waking input follows.
	resume        []events.NewEvent
	moves         []domain.SessionStatus
	cancelSession bool
	cancelThread  bool
	wakeParent    bool
	resumed       bool
	outcomeFlip   bool
}

// interruptThreadInTx is a send's interrupt arm for one thread: it settles the
// outstanding calls, tells and wakes a coordinator whose child stopped,
// transitions the thread, and reports the queue work to cancel and the outcomes
// to flip. It is a helper rather than a case body because the dream runner
// interrupts the session it owns from outside any request
// (interruptSessionInTx), where an appended event alone would stop nothing.
func (s *server) interruptThreadInTx(ctx context.Context, tx pgx.Tx, in interruptThreadIn) (interruptThreadOut, error) {
	var out interruptThreadOut
	isPrimary := in.threadID == ""
	// transition moves one thread under the caller's lock and keeps the pair it
	// emits in dst.
	transition := func(dst *[]events.NewEvent, t events.ThreadTransition) error {
		pair, moved, err := events.TransitionThread(ctx, tx, in.sessionID, t)
		if err != nil {
			return err
		}
		*dst = append(*dst, pair...)
		if moved != nil {
			out.moves = append(out.moves, *moved)
		}
		return nil
	}
	// Every tool call still outstanding on this thread is answered with
	// an error result. The model protocol requires every tool_use
	// answered before the conversation continues and the log is
	// append-only, so a call left abandoned would poison every future
	// replay — which is the dead end the interrupt exists to escape, not
	// one it may create. The batch's own results count as answered,
	// exactly as they do for the message trigger.
	abandoned, err := events.UnansweredThreadToolUses(ctx, tx, in.sessionID, in.threadID, in.answered)
	if err != nil {
		return out, err
	}
	// Only the two statuses a live thread rests at can be interrupted: nothing
	// leaves one at rescheduling across a commit, and terminated today is only
	// an archived child's. Neither should be settled from here if that changes:
	// terminated has ended and reviving it on the redirect below would make
	// this the one trigger that un-ends a session — the user.message case
	// guards against exactly that by requiring idle — while rescheduling would
	// need semantics no code has defined yet, and guessing them could leave the
	// column disagreeing with the log.
	interruptible := in.status == string(domain.SessionIdle) || in.status == string(domain.SessionRunning)
	// Nothing to stop: an idle thread with no outstanding call has no turn
	// to end, so the event is logged and settles no turn (a non-terminal
	// outcome still settles below — the flip does not depend on settling).
	// Emitting a status_idle for a thread that never left idle would
	// announce a transition that did not happen.
	flow, err := events.ThreadToolFlow(ctx, tx, in.sessionID, in.threadID, platformExecuted)
	if err != nil {
		return out, err
	}
	settling := interruptible && (in.status == string(domain.SessionRunning) || flow.Unsettled)
	if settling {
		results, err := events.InterruptResults(abandoned)
		if err != nil {
			return out, err
		}
		out.settled = append(out.settled, results...)
		// A child stopped mid-turn and the report it owed will never
		// come, so its coordinator is told (plan 35 decision 7) — and
		// woken when this was the last child it could have been
		// waiting on, the rule events.DeliverThreadEnded applies. The
		// wake runs before this thread's own idle below, so the
		// session never folds idle between the two. A
		// session-wide interrupt says nothing at all: it idles the
		// coordinator in this same loop, one notice per child would be
		// noise on a session the human just stopped, and a wake would
		// restart what the interrupt stopped.
		//
		// `all` is not that test on its own: it means every live
		// thread was named, so it is false the moment one idle sibling
		// goes unnamed — and a client that interrupts each *running*
		// thread has done exactly that. The wake below would then flip
		// the coordinator this same batch has already idled back to
		// running and queue it a fresh turn, restarting what the human
		// stopped. What the rule was always about is whether the
		// coordinator is still there to be told, so ask that.
		if !isPrimary && !in.all && !in.primaryInterrupted {
			notice, err := events.ThreadEnded(in.sessionID, in.threadID, in.agentName,
				fmt.Sprintf("[agent %s was interrupted]\n\nIt stopped mid-turn and will not report. "+
					"Send it new instructions, or archive it to free the slot.", in.agentName))
			if err != nil {
				return out, err
			}
			told, err := events.DeliverThreadEnded(ctx, tx, in.sessionID, in.threadID, notice)
			if err != nil {
				return out, err
			}
			out.told = &told
			if told.Moved != nil {
				out.moves = append(out.moves, *told.Moved)
			}
			out.wakeParent = told.Woke()
		}
		// end_turn, not a stop reason of its own: the reference documents an
		// interrupted turn as ending on the same stop reason as one that
		// finishes by itself, and the idle stop_reason union has no
		// interruption variant to carry (docs/DIVERGENCES.md). The thread
		// event is emitted whenever a turn ends — a stranded or gate-blocked
		// thread is already idle and its clients still need the new stop
		// reason — and so is the session's when it stays idle (Reemit);
		// the column only moves when the fold really changes.
		if err := transition(&out.idled, events.ThreadTransition{ThreadID: in.threadID, Status: domain.SessionIdle,
			Stop: &domain.StopReason{Type: domain.StopEndTurn}, Reemit: true}); err != nil {
			return out, err
		}
		// The queue work the caller cancels for this settlement: the
		// session's every live item, or this thread's turn alone.
		out.cancelSession, out.cancelThread = in.all, !in.all
	}
	if !isPrimary {
		return out, nil
	}
	// An active outcome settles with the turn — the docs mark it
	// interrupted "even if evaluation hadn't started yet", with an empty
	// outcome_evaluation_start_id when no start fired — freeing the
	// session for a new define_outcome, possibly one in this same batch
	// (the documented chaining pattern). The ends are settled by the
	// interrupt, so they are written before it, with its results.
	if interruptible {
		ends, flip, err := events.InterruptOutcomes(ctx, tx, in.sessionID)
		if err != nil {
			return out, err
		}
		out.settled = append(out.settled, ends...)
		out.outcomeFlip = flip
	}
	// The interrupt leaves nothing outstanding, so a user.message — or a
	// new user.define_outcome — in the same batch resumes exactly as it
	// would on any idle session: the documented way to steer a running
	// agent, or to chain outcomes, in one send.
	if in.resume && interruptible {
		if err := transition(&out.resume, events.ThreadTransition{Status: domain.SessionRunning}); err != nil {
			return out, err
		}
		out.resumed = true
	}
	return out, nil
}

// sendLayout records what one send writes, by what processingOrder needs to
// place it: the client's events as posted, what each posted interrupt's arms
// wrote around it, the running pair of each thread the send woke, and the rows
// it delivered.
type sendLayout struct {
	posted []events.NewEvent
	// settled and idled are keyed by the posted interrupt's index: what
	// settling it wrote ahead of it (the results it synthesized, the outcome
	// ends) and behind it (the idle pairs of the threads it ended).
	settled, idled map[int][]events.NewEvent
	// wakes are the running pairs, in the order their threads woke.
	wakes []threadWake
	// delivered are the rows delivered to a thread (an ending notice), each
	// with the index of the posted event whose processing delivered it.
	delivered []deliveredRow
}

type threadWake struct {
	thread domain.ID
	pair   []events.NewEvent
}

type deliveredRow struct {
	cause int
	row   events.NewEvent
}

func newSendLayout(posted []events.NewEvent) *sendLayout {
	return &sendLayout{posted: posted, settled: map[int][]events.NewEvent{}, idled: map[int][]events.NewEvent{}}
}

// interrupted records one thread's interrupt arm under the posted interrupt at
// index at.
func (l *sendLayout) interrupted(at int, out interruptThreadOut) {
	l.settled[at] = append(l.settled[at], out.settled...)
	l.idled[at] = append(l.idled[at], out.idled...)
	if out.told != nil {
		l.woke(out.told.Received.ThreadID, out.told.Wake)
		l.delivered = append(l.delivered, deliveredRow{cause: at, row: out.told.Received})
	}
	l.woke("", out.resume)
}

// woke records a thread's running pair; an empty pair is no wake.
func (l *sendLayout) woke(thread domain.ID, pair []events.NewEvent) {
	if len(pair) > 0 {
		l.wakes = append(l.wakes, threadWake{thread: thread, pair: pair})
	}
}

// processingOrder lays the send out in the order its events are processed
// within the commit — the order the reference lists them in (#793, #539;
// docs/plan/56_processing-order.md) — rather than the order they were posted.
// Every event goes where it is consumed, whatever its posted position:
//
//  1. What is consumed on receipt, in receipt order: the answers
//     (user.tool_confirmation, user.tool_result, user.custom_tool_result) and
//     the interrupts. Each interrupt is preceded by what settling it wrote —
//     the results it synthesized and the outcome ends — and followed by the
//     idle pairs of the threads it ended. An answer stays where it was
//     received, ahead of any result a later interrupt of the send synthesizes.
//  2. For each thread this commit woke, in the order woken: its running pair
//     (behind session.status_running when the fold moved), then the input its
//     woken turn consumes, in receipt order — the posted user.message,
//     user.define_outcome and system.message addressed to it, and every row
//     delivered to it in this commit, placed by the posted event whose
//     processing delivered it, whichever arm made the wake. So a notice
//     follows its coordinator's running event even when a message woke the
//     coordinator, or another child's ending did.
//  3. The input no wake in this commit is for, in receipt order: a message or
//     system.message to a primary already running, and a notice to a
//     coordinator this commit did not wake (it is running, parked on its
//     human, or still has a busy child). A later turn consumes it, so it
//     follows everything the send processes.
//
// A system.message is input, never a wake: it follows the running pair of a
// turn this send starts, and otherwise waits at the tail for the next one. A
// notice is input to the thread it names, placed wherever that thread's other
// input goes.
//
// One placement stays out of reach. A thread an answer resumes moves in Then,
// once the answer is on the log and its flow can settle, so its running pair
// follows the whole batch: an input posted beside the answer precedes that
// resume instead of following it.
//
// The POST echo keeps the posted order; only the log and the stream read this.
func (l *sendLayout) processingOrder() []events.NewEvent {
	type input struct {
		cause  int
		thread domain.ID
		ev     events.NewEvent
	}
	var out []events.NewEvent
	var inputs []input
	for i, ev := range l.posted {
		switch ev.Type {
		case domain.EventUserToolConfirm, domain.EventUserToolResult, domain.EventUserCustomToolRes:
			out = append(out, ev)
		case domain.EventUserInterrupt:
			out = append(append(append(out, l.settled[i]...), ev), l.idled[i]...)
		default:
			inputs = append(inputs, input{cause: i, thread: ev.ThreadID, ev: ev})
		}
	}
	for _, d := range l.delivered {
		inputs = append(inputs, input{cause: d.cause, thread: d.row.ThreadID, ev: d.row})
	}
	slices.SortStableFunc(inputs, func(a, b input) int { return cmp.Compare(a.cause, b.cause) })
	placed := make([]bool, len(inputs))
	for _, w := range l.wakes {
		out = append(out, w.pair...)
		for j, in := range inputs {
			if !placed[j] && in.thread == w.thread {
				out, placed[j] = append(out, in.ev), true
			}
		}
	}
	for j, in := range inputs {
		if !placed[j] {
			out = append(out, in.ev)
		}
	}
	return out
}

// keepLastSessionIdle drops all but the final session.status_idle of a batch. A
// session-wide interrupt ends its threads one by one, and each end re-idles the
// session with the fold of that moment; the session is told once, with the fold
// after the last of them. A single-agent session emits one either way.
func keepLastSessionIdle(batch []events.NewEvent) []events.NewEvent {
	lastIdle := -1
	for i, ev := range batch {
		if ev.Type == domain.EventSessionStatusIdle {
			lastIdle = i
		}
	}
	if lastIdle < 0 {
		return batch
	}
	kept := batch[:0:0]
	for i, ev := range batch {
		if ev.Type != domain.EventSessionStatusIdle || i == lastIdle {
			kept = append(kept, ev)
		}
	}
	return kept
}

// interruptSessionInTx interrupts every live thread of a session inside the
// caller's transaction, exactly as a session-wide user.interrupt posted to
// POST /v1/sessions/{id}/events does: the inbound event is appended, every
// outstanding call is answered, the threads idle, an active outcome flips to
// interrupted and the session's queued work is cancelled with the append.
//
// The dream runner calls it on the session a dream owns — from the cancel
// handler and from the tick (plan 41 §4.1) — where the handler is closed to it
// by requireNotDreamOwned, and where a bare event append would stop nothing.
// It never commits; the caller does. So it does not record the status metrics
// either — those observe a committed transition — and instead returns the
// moves it made, in the order the threads made them, for the caller to record
// after its own commit exactly as sendSessionEvents records its own.
//
// An archived session has nothing to interrupt: its threads have ended and its
// log is closed to appends. A session that is gone is the caller's 404.
func (s *server) interruptSessionInTx(ctx context.Context, tx pgx.Tx, sessionID string) ([]domain.SessionStatus, error) {
	var envKind, status string
	var archivedAt *time.Time
	err := tx.QueryRow(ctx,
		`SELECT e.kind, s.status, s.archived_at
		 FROM sessions s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $1 FOR UPDATE OF s`, sessionID).Scan(&envKind, &status, &archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", sessionID)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, nil
	}
	// The interrupt goes on the log as a client's would, so a reader sees why
	// the session stopped. It is threadless, which is the session-wide spelling
	// RouteInbound would leave untouched, and unstamped, as the handler leaves
	// a session-wide interrupt.
	interrupt, err := events.NormalizeInbound(envKind, events.ManagementCredential, []json.RawMessage{json.RawMessage(`{"type":"user.interrupt"}`)})
	if err != nil {
		return nil, err
	}
	threads, err := liveThreads(ctx, tx, sessionID, status)
	if err != nil {
		return nil, err
	}
	var opts events.AppendOptions
	var moves []domain.SessionStatus
	layout := newSendLayout(interrupt)
	var cancelSession, outcomeFlip bool
	for _, th := range threads {
		out, err := s.interruptThreadInTx(ctx, tx, interruptThreadIn{
			sessionID: domain.ID(sessionID), threadID: th.id, agentName: th.agentName,
			status: th.status, all: true, primaryInterrupted: true,
		})
		if err != nil {
			return nil, err
		}
		layout.interrupted(0, out)
		moves = append(moves, out.moves...)
		for i := range out.moves {
			opts.SetStatus = &out.moves[i]
		}
		cancelSession = cancelSession || out.cancelSession
		outcomeFlip = outcomeFlip || out.outcomeFlip
	}
	// In the order a client's session-wide interrupt is written (#539).
	batch := keepLastSessionIdle(layout.processingOrder())
	if cancelSession {
		opts.Then = func(ctx context.Context, tx pgx.Tx) error {
			return s.queue.CancelSession(ctx, tx, domain.ID(sessionID))
		}
	}
	if outcomeFlip {
		flip := events.FlipNonTerminalOutcomes(time.Now().UTC())
		opts.MutateOutcomes = func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
			return flip(evals)
		}
	}
	previousThen := opts.Then
	opts.Then = func(ctx context.Context, tx pgx.Tx) error {
		for _, th := range threads {
			if _, err := s.log.AdvanceThreadTools(ctx, tx, domain.ID(sessionID), th.id, platformExecuted); err != nil {
				return err
			}
		}
		if previousThen != nil {
			return previousThen(ctx, tx)
		}
		return nil
	}
	if _, err := s.log.AppendInTx(ctx, tx, domain.ID(sessionID), batch, opts); err != nil {
		return nil, err
	}
	return moves, nil
}

// postDreamStageInTx appends one stage's user.message on the primary thread of
// an idle session and enqueues the model turn it starts, inside the caller's
// transaction. It is the narrow recipe createSessionInTx runs for a create's
// initial events (sessions.go: NormalizeInbound → TransitionThread →
// AppendInTx with an Enqueue in Then), lifted for the dream runner's arm 9
// (plan 41 §4.1) — deliberately not a factoring of sendSessionEvents, which
// the runner cannot call at all: requireNotDreamOwned refuses every send to
// the session a dream owns, the runner's own included.
//
// Two things the create and the handler's idle-primary wake do are left out.
// Reemit is one: the rows here are idle rather than freshly inserted, so the
// move itself is what emits the pair. The other is startWorkCycle's
// mcp_catalogs delete, which re-dials the servers a previous cycle could not
// reach — the internal dream agent declares no MCP server, so there is never a
// row to drop.
//
// The arm has already established that the session is idle, so anything else
// found here is a bug rather than a state to handle. It is an error naming
// what was found, wrapping errDreamStageRefused so the arm can fail the dream
// on it rather than retry a state no tick will change. Like
// interruptSessionInTx it records no status metric — it returns the moves for
// whoever commits.
// errDreamStageRefused marks the two refusals below that no later tick can
// clear — an archived session, and an idle thread holding an unanswered
// tool_use — apart from both the database errors around them and the one
// refusal that does clear itself. The arm turns this into a failed dream
// naming the cause; without the distinction the tick would log and retry until
// DREAM_TIMEOUT, and the dream would settle as `timeout` with the real fault
// hours back in the log.
var errDreamStageRefused = errors.New("the pipeline session cannot take a stage")

func (s *server) postDreamStageInTx(ctx context.Context, tx pgx.Tx, sessionID, text string) ([]domain.SessionStatus, error) {
	var envKind, status, envID string
	var archivedAt *time.Time
	err := tx.QueryRow(ctx,
		`SELECT e.kind, s.status, s.archived_at, s.environment_id
		 FROM sessions s JOIN environments e ON e.id = s.environment_id
		 WHERE s.id = $1 FOR UPDATE OF s`, sessionID).Scan(&envKind, &status, &archivedAt, &envID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("session %s not found", sessionID)
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, fmt.Errorf("%w: session %s is archived", errDreamStageRefused, sessionID)
	}
	if status != string(domain.SessionIdle) {
		// Deliberately not errDreamStageRefused: this is the race the arm
		// cannot close — the status was idle when it read it — and the
		// session finishing its turn clears it. Rolling back and letting a
		// later tick see the idle session is the whole handling.
		return nil, fmt.Errorf("session %s is %s, not idle; no stage can be posted to it", sessionID, status)
	}
	msg := mustJSON(map[string]any{
		"type":    "user.message",
		"content": []any{map[string]any{"type": "text", "text": text}},
	})
	batch, err := events.NormalizeInbound(envKind, events.ManagementCredential, []json.RawMessage{json.RawMessage(msg)})
	if err != nil {
		return nil, err
	}
	// The guard the handler applies before waking an idle primary: a resumed
	// turn would replay an assistant tool_use that no tool_result answers, a
	// request the model protocol rejects. The handler logs it and leaves the
	// thread idle; here it is the arm's error, there being no client to post
	// the missing result.
	unanswered, err := events.HasUnansweredThreadToolUse(ctx, tx, domain.ID(sessionID), "", nil)
	if err != nil {
		return nil, err
	}
	if unanswered {
		return nil, fmt.Errorf("%w: session %s is idle with an unanswered tool_use", errDreamStageRefused, sessionID)
	}
	pair, moved, err := events.TransitionThread(ctx, tx, domain.ID(sessionID),
		events.ThreadTransition{Status: domain.SessionRunning})
	if err != nil {
		return nil, err
	}
	// TransitionThread writes sessions.status itself, so the append needs no
	// SetStatus; what the move is wanted for is the committer's own metric.
	var moves []domain.SessionStatus
	if moved != nil {
		moves = append(moves, *moved)
	}
	// The stage is the input the woken turn consumes, so it follows the pair
	// (processing order, #793).
	if _, err := s.log.AppendInTx(ctx, tx, domain.ID(sessionID), append(pair, batch...), events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.queue.Enqueue(ctx, tx, domain.ID(envID), domain.ID(sessionID), queue.ModelTurn)
			return err
		},
	}); err != nil {
		return nil, err
	}
	return moves, nil
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
// The session's view is the primary thread's (plan 35 decision 2): its own
// rows plus what child threads cross-post, and on a self_hosted environment
// their tool calls too, which is all a BYOC worker ever reads (decision 13 i).
func (s *server) listSessionEvents(r *http.Request) (any, error) {
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	return s.listEvents(r, id, events.ListQuery{Scope: events.ScopeSession}, true, func(ctx context.Context) (eventsView, error) {
		return s.sessionView(ctx, id)
	})
}

// listEvents renders one page of a session's log on the surface scope
// selects. filters admits the session list's order / types[] / created_at
// params; the thread lists carry none, so there they are refused rather than
// silently defaulted. resolve settles the 404 — after the params, so a bad
// request on a missing resource stays a 400, as every list here answers —
// and the view this surface renders under.
func (s *server) listEvents(r *http.Request, id string, query events.ListQuery, filters bool, resolve func(context.Context) (eventsView, error)) (any, error) {
	ctx := r.Context()
	q, err := queryValues(r)
	if err != nil {
		return nil, err
	}
	if !filters {
		for _, key := range []string{"order", "types", "types[]", "created_at[gt]", "created_at[gte]", "created_at[lt]", "created_at[lte]"} {
			if _, ok := q[key]; ok {
				return nil, errInvalid("%s is not supported on a thread's events", key)
			}
		}
	}

	page, err := parsePageMax(q, maxEventLimit)
	if err != nil {
		return nil, err
	}
	query.Limit = page.limit + 1
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		query.Desc = true
	default:
		return nil, errInvalid(`order must be "asc" or "desc"`)
	}
	if page.cur != nil {
		// A thread list mints only ascending cursors; a descending one came
		// from the session list and would walk the thread backwards past
		// the order refusal above.
		if !page.cur.seqKeyed || (!filters && page.cur.seqDesc) {
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

	view, err := resolve(ctx)
	if err != nil {
		return nil, err
	}
	view.apply(&query)
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
		wire, err := eventWire(ev, query.Scope)
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
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		writeError(w, r, err)
		return
	}
	s.streamEvents(w, r, id, events.ListQuery{Scope: events.ScopeSession}, func(ctx context.Context) (eventsView, error) {
		return s.sessionView(ctx, id)
	})
}

// streamEvents tails one surface of a session's log: scope selects the rows
// and, through the broker, the preview frames (a thread's frames reach only
// that thread's subscribers). resolve settles the 404 and the view — after
// the params, as listEvents does.
func (s *server) streamEvents(w http.ResponseWriter, r *http.Request, id string, scope events.ListQuery, resolve func(context.Context) (eventsView, error)) {
	ctx := r.Context()
	q := r.URL.Query()

	previews := make(map[string]bool)
	for _, v := range listParam(q, "event_deltas") {
		if !events.Previewable(domain.EventType(v)) {
			writeError(w, r, errInvalid(`event_deltas values must be "agent.message" or "agent.thinking"`))
			return
		}
		previews[v] = true
	}
	view, err := resolve(ctx)
	if err != nil {
		writeError(w, r, err)
		return
	}
	view.apply(&scope)
	sub := s.broker.SubscribeThread(domain.ID(id), scope.ThreadID)
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
			// The scope may hide the rows this wake announced (another
			// thread's), and they still move the tail: read the log's
			// high-water mark first — seq order is commit order, the session
			// lock serializes appends — and advance to it once the scoped
			// rows up to it are written, so a quiet surface never re-scans a
			// widening window.
			var upTo int64
			if err := s.pool.QueryRow(ctx,
				`SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1`, id).Scan(&upTo); err != nil {
				writeErrorFrame(w, flusher)
				return
			}
			wrote := 0
			for {
				// The page copies the subscriber's own surface rather than
				// rebuilding it, so a narrowing this stream must honour can
				// never be left behind on the list endpoint alone.
				page := scope
				page.AfterSeq, page.Limit = &lastSeq, sseWakeBatch
				evs, err := s.log.List(ctx, domain.ID(id), page)
				if err != nil {
					writeErrorFrame(w, flusher)
					return
				}
				for _, ev := range evs {
					wire, err := eventWire(ev, scope.Scope)
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
			if upTo > lastSeq {
				lastSeq = upTo
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

// threadAddressable are the event types whose nullable session_thread_id
// names the child thread they were cross-posted from (SDK: "When set, this
// event was cross-posted from a subagent's thread … Empty on the thread's own
// events") — the tool-use kinds and the inbound answers to them.
var threadAddressable = map[domain.EventType]bool{
	domain.EventAgentToolUse: true, domain.EventAgentMCPToolUse: true, domain.EventAgentCustomToolUse: true,
	domain.EventUserToolConfirm: true, domain.EventUserCustomToolRes: true, domain.EventUserToolResult: true,
	domain.EventUserInterrupt: true,
}

// eventWire renders a stored event onto the wire: the type-specific payload
// fields merged with the id/type/processed_at envelope. Payload bytes pass
// through untouched, so content blocks round-trip exactly — except
// session_thread_id, rendered per surface (plan 35 decision 2): a child's
// thread-addressable event seen through the session view names its thread —
// whether it got there by cross-posting or by decision 13's self_hosted
// widening; on the child's own surface the stored null is dropped.
//
// "Thread-addressable" is the whole of the qualifier, and it is the wire's
// rather than ours: the widening also carries the results answering a child's
// calls, and agent.tool_result declares no session_thread_id (absent at
// anthropic-sdk-go v1.66.0 — betasessionevent.go
// BetaManagedAgentsAgentToolResultEvent.SessionThreadID), so a widened one
// renders unnamed. A worker still correlates it, because the call it answers is
// named.
func eventWire(ev domain.Event, scope events.Scope) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(ev.Body, &out); err != nil {
		return nil, fmt.Errorf("event %s payload is corrupt: %w", ev.ID, err)
	}
	if out == nil {
		out = make(map[string]json.RawMessage)
	}
	// No recorded event carries session_thread_id or deny_message as a present
	// null: the reference omits them (#674). The writers store the null and the
	// log is append-only, so it is dropped here, for old rows and on every
	// surface alike — but only on the types the recordings show it omitted
	// from. A null anywhere else still renders, so a writer bug stays visible.
	if threadAddressable[ev.Type] && isNull(out["session_thread_id"]) {
		delete(out, "session_thread_id")
	}
	if ev.Type == domain.EventUserToolConfirm && isNull(out["deny_message"]) {
		delete(out, "deny_message")
	}
	if scope == events.ScopeSession && ev.ThreadID != "" && threadAddressable[ev.Type] {
		out["session_thread_id"], _ = json.Marshal(ev.ThreadID.String())
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

// sessionView reads a session surface's view — widened when the environment
// is self_hosted, the view rule's whole predicate (plan 35 decision 13 i), and
// filtered when the snapshot carries a roster — and resolves the same 404
// sessionExists does, so the session surfaces pay one round trip for all
// three. The kind is not on the session row, hence the join.
func (s *server) sessionView(ctx context.Context, id string) (eventsView, error) {
	var kind string
	var multiagent []byte
	err := s.pool.QueryRow(ctx,
		`SELECT e.kind, s.resolved_agent->'multiagent' FROM sessions s JOIN environments e ON e.id = s.environment_id WHERE s.id = $1`,
		id).Scan(&kind, &multiagent)
	if errors.Is(err, pgx.ErrNoRows) {
		return eventsView{}, errNotFound("session %s not found", id)
	}
	return eventsView{wide: kind == string(domain.EnvSelfHosted), delegates: hasRoster(multiagent)}, err
}

// hasRoster reports whether a session snapshot's multiagent is a non-empty
// roster: the brain's own test for a coordinator (internal/brain/mcptools.go
// hasRoster), spelled the same way, because the two must agree on which
// sessions delegate (eventsView.delegates). Decoded rather than measured for
// the reason that one gives — a single agent stores an explicit JSON null.
func hasRoster(multiagent []byte) bool {
	var p struct {
		Agents []json.RawMessage `json:"agents"`
	}
	return json.Unmarshal(multiagent, &p) == nil && len(p.Agents) > 0
}

// idsOf converts event id strings to domain ids for a stop reason.
func idsOf(ids []string) []domain.ID {
	out := make([]domain.ID, len(ids))
	for i, id := range ids {
		out[i] = domain.ID(id)
	}
	return out
}

// listParam collects a repeatable array query parameter in both wire
// spellings, key[]=v (bracket serialization) and key=v.
func listParam(q url.Values, key string) []string {
	var out []string
	out = append(out, q[key+"[]"]...)
	out = append(out, q[key]...)
	return out
}
