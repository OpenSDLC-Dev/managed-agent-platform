package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// Session status is a fold over thread statuses, and thread status is
// authoritative (plan 35 decision 4): a transition moves one thread's row and
// re-derives the session's — terminated iff the primary is; else running iff
// any live thread is; else rescheduling iff any is; else idle, its stop
// reason a precedence pick over the idle threads' (requires_action ≻
// retries_exhausted ≻ end_turn, event_ids the seq-ordered union) — in the
// same transaction, under the same session row lock. It is recorded as a
// pair (decision 12): the thread's own session.thread_status_* first, the
// session's session.status_* second — the fact, then the rollup, the order
// the reference's own sequences show. A single-thread session reduces to the
// pre-thread behavior exactly: every thread move is a session move, so every
// pair is emitted; that reduction is the regression gate. Every session has a
// primary thread, so every session.status_* emission goes through
// TransitionThread; a history from before the thread resource existed holds
// no thread events (append-only; nothing backfills them).

// primaryStatusEvents are the thread events AppendInTx completes with the
// session's agent name when they carry no thread (the primary's own status
// changes). session.thread_created is deliberately not here: it is written
// to the parent's stream naming the *child's* agent, so its emitter supplies
// agent_name itself.
var primaryStatusEvents = map[domain.EventType]bool{
	domain.EventSessionThreadStatusRunning:     true,
	domain.EventSessionThreadStatusIdle:        true,
	domain.EventSessionThreadStatusRescheduled: true,
	domain.EventSessionThreadStatusTerminated:  true,
}

// threadStatusOf maps a thread status to the thread-status event that
// announces it. The primary thread never terminates (decision 12 — only
// _running is asserted for every session, and the webhooks page carves the
// primary's end out), so terminated is a child's event alone, added below.
var threadStatusOf = map[domain.SessionStatus]domain.EventType{
	domain.SessionRunning:      domain.EventSessionThreadStatusRunning,
	domain.SessionIdle:         domain.EventSessionThreadStatusIdle,
	domain.SessionRescheduling: domain.EventSessionThreadStatusRescheduled,
}

var sessionStatusOf = map[domain.SessionStatus]domain.EventType{
	domain.SessionRunning:      domain.EventSessionStatusRunning,
	domain.SessionIdle:         domain.EventSessionStatusIdle,
	domain.SessionRescheduling: domain.EventSessionStatusRescheduled,
	domain.SessionTerminated:   domain.EventSessionStatusTerminated,
}

// ThreadTransition is one thread's status move.
type ThreadTransition struct {
	ThreadID domain.ID // the moving thread; empty for the primary
	Status   domain.SessionStatus
	Stop     *domain.StopReason // the idle stop reason; nil for the other statuses
	// Reemit and Force are the two value-independent session emissions every
	// session keeps. Reemit is the payload-only re-idle — the interrupt's
	// idle→idle re-emit, a confirmation that shrinks the ask set: the session
	// event is emitted when the fold is unchanged *and equal to* the thread's
	// new status, so a child's re-idle under a running sibling stays a thread
	// event alone. Force is the reclaim's rescheduled+running pair on a
	// running session: emitted whatever the fold says, carrying the thread's
	// status. Callers set nothing else.
	Reemit, Force bool
}

// TransitionThread moves one thread's status and folds the session's over its
// live threads', in the caller's transaction under the session row lock (the
// API's trigger, the brain's settlements and the thread archive all hold it).
// It writes the thread row (status, stop_reason) and sessions.status, and
// returns the events to append — the thread's own first (a child's is
// cross-posted to the session view and names its agent; the primary's is
// completed with the session's agent name by AppendInTx), the session's
// second when the folded value changed or Force — and the status the session
// column moved to, nil when it did not: what the caller's post-commit metric
// counts, so a re-idle or a reclaim pair never inflates it.
func TransitionThread(ctx context.Context, tx pgx.Tx, sessionID domain.ID, t ThreadTransition) ([]NewEvent, *domain.SessionStatus, error) {
	if _, ok := sessionStatusOf[t.Status]; !ok {
		return nil, nil, fmt.Errorf("events: no status event for session status %q", t.Status)
	}
	tid := t.ThreadID
	if tid == "" {
		tid = domain.PrimaryThreadID(sessionID)
	}
	var stopJSON []byte
	if t.Status == domain.SessionIdle && t.Stop != nil {
		stopJSON = mustJSON(t.Stop)
	}
	var agentName string
	err := tx.QueryRow(ctx,
		`UPDATE session_threads SET status = $2, stop_reason = $3, updated_at = now()
		  WHERE id = $1 AND session_id = $4 RETURNING agent_name`,
		tid.String(), string(t.Status), stopJSON, sessionID.String()).Scan(&agentName)
	rowFound := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("move thread %s: %w", tid, err)
	}
	if !rowFound && t.ThreadID != "" {
		return nil, nil, fmt.Errorf("thread %s not found in session %s", tid, sessionID)
	}

	var out []NewEvent
	threadType, emitThread := threadStatusOf[t.Status]
	if t.Status == domain.SessionTerminated && t.ThreadID != "" {
		threadType, emitThread = domain.EventSessionThreadStatusTerminated, true
	}
	if emitThread {
		payload := map[string]any{"session_thread_id": tid.String()}
		if t.ThreadID != "" {
			payload["agent_name"] = agentName
		}
		if stopJSON != nil {
			payload["stop_reason"] = t.Stop
		}
		out = append(out, NewEvent{Type: threadType, Payload: mustJSON(payload),
			ThreadID: t.ThreadID, CrossPosted: t.ThreadID != ""})
	}

	folded, foldedStop, err := foldSession(ctx, tx, sessionID, t, rowFound)
	if err != nil {
		return nil, nil, err
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, sessionID.String()).Scan(&current); err != nil {
		return nil, nil, fmt.Errorf("fold session %s: %w", sessionID, err)
	}
	changed := string(folded) != current
	var moved *domain.SessionStatus
	if changed {
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET status = $2, updated_at = now() WHERE id = $1`,
			sessionID.String(), string(folded)); err != nil {
			return nil, nil, err
		}
		moved = &folded
	}
	if changed || t.Force || (t.Reemit && folded == t.Status) {
		// Unchanged: the re-emit carries the folded stop reason (the union);
		// the forced reclaim pair carries the thread's own status.
		emit, stop := folded, foldedStop
		if !changed && folded != t.Status {
			emit, stop = t.Status, t.Stop
		}
		payload := map[string]any{}
		if emit == domain.SessionIdle && stop != nil {
			payload["stop_reason"] = stop
		}
		out = append(out, NewEvent{Type: sessionStatusOf[emit], Payload: mustJSON(payload)})
	}
	return out, moved, nil
}

// AppendTransition is the self-committing form for callers with no other
// decision to make under the lock (the brain's reclaim, test harnesses): one
// transaction that locks the session, runs the transitions in order, appends
// evs followed by what they emitted, with opts' side effects, and — after
// the commit, never before — counts the session's net move, so a reclaim's
// rescheduled+running pair on a running session counts nothing.
func (l *Log) AppendTransition(ctx context.Context, sessionID domain.ID, evs []NewEvent, transitions []ThreadTransition, opts AppendOptions) ([]domain.Event, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before string
	err = tx.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1 FOR UPDATE`, sessionID.String()).Scan(&before)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	batch := append([]NewEvent(nil), evs...)
	opts.SetStatus = nil
	for _, t := range transitions {
		out, _, err := TransitionThread(ctx, tx, sessionID, t)
		if err != nil {
			return nil, err
		}
		batch = append(batch, out...)
	}
	var after string
	if err := tx.QueryRow(ctx, `SELECT status FROM sessions WHERE id = $1`, sessionID.String()).Scan(&after); err != nil {
		return nil, err
	}
	if after != before {
		net := domain.SessionStatus(after)
		opts.SetStatus = &net
	}
	appended, err := l.AppendInTx(ctx, tx, sessionID, batch, opts)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if opts.SetStatus != nil {
		RecordSessionStatus(ctx, *opts.SetStatus)
	}
	return appended, nil
}

// PreviewTransition is TransitionThread's fold without its writes: the
// session status (and idle stop reason) the transition would leave, for the
// settlement that decides before it moves — the outcome intercept fires only
// when a thread's end_turn is the session's quiescence (decision 15).
func PreviewTransition(ctx context.Context, q Querier, sessionID domain.ID, t ThreadTransition) (domain.SessionStatus, *domain.StopReason, error) {
	return foldSession(ctx, q, sessionID, t, true)
}

// stopRank orders the idle stop reasons the fold picks among: the ask-over-cap
// pair is documented, end_turn's rank is inferred (docs/DIVERGENCES.md).
func stopRank(t domain.StopReasonType) int {
	switch t {
	case domain.StopRequiresAction:
		return 3
	case domain.StopRetriesExhausted:
		return 2
	case domain.StopEndTurn:
		return 1
	}
	return 0
}

// foldSession derives the session's status from its live threads', with t
// standing in for its own thread's row (so the preview needs no write and the
// transition needs no re-read). A session with no thread rows at all — a
// fixture from before the resource existed — folds to the transition itself,
// the single-thread reduction.
func foldSession(ctx context.Context, q Querier, sessionID domain.ID, t ThreadTransition, rowFound bool) (domain.SessionStatus, *domain.StopReason, error) {
	if t.ThreadID == "" && t.Status == domain.SessionTerminated {
		return domain.SessionTerminated, nil, nil
	}
	tid := t.ThreadID
	if tid == "" {
		tid = domain.PrimaryThreadID(sessionID)
	}
	rows, err := q.Query(ctx,
		`SELECT id, status, stop_reason FROM session_threads
		  WHERE session_id = $1 AND archived_at IS NULL AND status <> 'terminated'
		  ORDER BY created_at, id`, sessionID.String())
	if err != nil {
		return "", nil, fmt.Errorf("fold session %s: %w", sessionID, err)
	}
	defer rows.Close()
	type thread struct {
		status domain.SessionStatus
		stop   *domain.StopReason
	}
	var threads []thread
	for rows.Next() {
		var id, status string
		var stopJSON []byte
		if err := rows.Scan(&id, &status, &stopJSON); err != nil {
			return "", nil, err
		}
		th := thread{status: domain.SessionStatus(status)}
		if domain.ID(id) == tid {
			// The moving thread: its new status stands in for the row (the
			// transition wrote it already; the preview has not).
			th = thread{status: t.Status, stop: t.Stop}
		} else if len(stopJSON) > 0 {
			th.stop = new(domain.StopReason)
			if err := json.Unmarshal(stopJSON, th.stop); err != nil {
				return "", nil, fmt.Errorf("thread %s stop_reason: %w", id, err)
			}
		}
		if th.status == domain.SessionTerminated {
			continue
		}
		threads = append(threads, th)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if !rowFound || len(threads) == 0 {
		return t.Status, t.Stop, nil
	}

	var anyResched bool
	var pick *domain.StopReason
	var askIDs []domain.ID
	var askThreads int
	for _, th := range threads {
		switch th.status {
		case domain.SessionRunning:
			return domain.SessionRunning, nil, nil
		case domain.SessionRescheduling:
			anyResched = true
		case domain.SessionIdle:
			if th.stop == nil {
				continue
			}
			if pick == nil || stopRank(th.stop.Type) > stopRank(pick.Type) {
				pick = th.stop
			}
			if th.stop.Type == domain.StopRequiresAction {
				askIDs = append(askIDs, th.stop.EventIDs...)
				askThreads++
			}
		}
	}
	if anyResched {
		return domain.SessionRescheduling, nil, nil
	}
	if pick != nil && pick.Type == domain.StopRequiresAction {
		ids := askIDs
		if askThreads > 1 {
			// The union across threads, in log order — one query, only when
			// more than one thread contributes (a single list is already
			// ordered by its emitter).
			if ids, err = idsInSeqOrder(ctx, q, sessionID, askIDs); err != nil {
				return "", nil, err
			}
		}
		pick = &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: ids}
	}
	return domain.SessionIdle, pick, nil
}

// idsInSeqOrder returns ids ordered by their events' seq. Ids not yet on the
// log — the moving thread's own asks, minted in memory and appended after the
// transition in the same commit — come last, in the order given: they land
// above everything committed, so that is their log order too.
func idsInSeqOrder(ctx context.Context, q Querier, sessionID domain.ID, ids []domain.ID) ([]domain.ID, error) {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := q.Query(ctx,
		`SELECT id FROM events WHERE session_id = $1 AND id = ANY($2) ORDER BY seq`,
		sessionID.String(), strs)
	if err != nil {
		return nil, fmt.Errorf("order ask events: %w", err)
	}
	defer rows.Close()
	out := make([]domain.ID, 0, len(ids))
	seen := make(map[domain.ID]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, domain.ID(id))
		seen[domain.ID(id)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// withAgentName sets agent_name on a thread status payload that lacks it.
func withAgentName(payload json.RawMessage, name string) (json.RawMessage, error) {
	obj, err := asObject(payload, "thread event payload")
	if err != nil {
		return nil, err
	}
	if _, ok := obj["agent_name"]; ok {
		return payload, nil
	}
	obj["agent_name"] = mustJSON(name)
	return json.Marshal(obj)
}

// nullableID binds an optional id: NULL when empty.
func nullableID(id domain.ID) *string {
	if id == "" {
		return nil
	}
	s := id.String()
	return &s
}

// NullableThread binds a thread id as the thread_id column holds it: NULL for
// the primary (plan 35 decision 2) — the one convention every package that
// queries by thread shares.
func NullableThread(threadID domain.ID) *string { return nullableID(threadID) }
