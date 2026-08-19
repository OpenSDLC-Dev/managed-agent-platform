package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/toolset"
	"github.com/jackc/pgx/v5"
)

// The session-thread resource (plan 35 decisions 1, 2, 12). Every session has
// a primary thread whose id derives from the session's (domain.PrimaryThreadID)
// and whose row stores no agent of its own — it renders from
// sessions.resolved_agent minus the roster, so a session update never leaves a
// stale duplicate; child threads (slice 3) hold their spawn-time snapshot. The
// primary's events are the session's: its list and stream serve the session
// view; a child's serve that child's own rows. Thread stats render the empty
// shape, the precedent session stats set (docs/DIVERGENCES.md).

// threadJSON is BetaManagedAgentsSessionThread.
type threadJSON struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"` // "session_thread"
	SessionID      string          `json:"session_id"`
	ParentThreadID *string         `json:"parent_thread_id"`
	Agent          threadAgentJSON `json:"agent"`
	Status         string          `json:"status"`
	Usage          usageJSON       `json:"usage"`
	Stats          threadStatsJSON `json:"stats"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	ArchivedAt     *time.Time      `json:"archived_at"`
}

type threadStatsJSON struct {
	ActiveSeconds   float64 `json:"active_seconds"`
	DurationSeconds float64 `json:"duration_seconds"`
	StartupSeconds  float64 `json:"startup_seconds"`
}

// threadRow carries one session_threads row, joined to its session's resolved
// agent for the primary's render.
type threadRow struct {
	id, sessionID        string
	parent               *string
	agentJSON            []byte // NULL on the primary
	agentName, status    string
	usageJSON            []byte
	createdAt, updatedAt time.Time
	archivedAt           *time.Time
	resolvedAgent        []byte
}

const threadColumns = `t.id, t.session_id, t.parent_thread_id, t.agent, t.agent_name, t.status, t.usage,
	t.created_at, t.updated_at, t.archived_at, s.resolved_agent`

func scanThread(row pgx.Row) (threadRow, error) {
	var r threadRow
	err := row.Scan(&r.id, &r.sessionID, &r.parent, &r.agentJSON, &r.agentName, &r.status, &r.usageJSON,
		&r.createdAt, &r.updatedAt, &r.archivedAt, &r.resolvedAgent)
	return r, err
}

func renderThread(r threadRow) (threadJSON, error) {
	var agent threadAgentJSON
	if r.agentJSON != nil {
		if err := json.Unmarshal(r.agentJSON, &agent); err != nil {
			return threadJSON{}, fmt.Errorf("decode stored thread agent: %w", err)
		}
	} else {
		// The primary: the session's resolved agent, read through.
		var resolved sessionAgentJSON
		if err := json.Unmarshal(r.resolvedAgent, &resolved); err != nil {
			return threadJSON{}, fmt.Errorf("decode stored resolved agent: %w", err)
		}
		agent = selfMember(resolved)
	}
	// The same echo rule as every agent surface: toolset configuration
	// resolved for the response.
	agent.Tools = toolset.MaterializeTools(agent.Tools)
	if agent.Tools == nil {
		agent.Tools = []json.RawMessage{}
	}
	var usage usageJSON
	if err := json.Unmarshal(r.usageJSON, &usage); err != nil {
		return threadJSON{}, fmt.Errorf("decode stored thread usage: %w", err)
	}
	return threadJSON{
		ID: r.id, Type: "session_thread", SessionID: r.sessionID, ParentThreadID: r.parent,
		Agent: agent, Status: r.status, Usage: usage, Stats: threadStatsJSON{},
		CreatedAt: r.createdAt.UTC(), UpdatedAt: r.updatedAt.UTC(), ArchivedAt: utcPtr(r.archivedAt),
	}, nil
}

// threadMaxLimit is the threads list's cap and default: the list param
// documents "Defaults to 1000" and no maximum; the bound is ours, as the
// session-events list's is.
const threadMaxLimit = 1000

// listThreads implements GET /v1/sessions/{id}/threads: creation order, the
// PageCursor envelope, forward-only (the param says so; a prev cursor is
// refused rather than honored).
func (s *server) listThreads(r *http.Request) (any, error) {
	ctx := r.Context()
	id := normalizeSessionID(r.PathValue("id"))
	if err := checkID(id, "session"); err != nil {
		return nil, err
	}
	page, err := parsePageWith(r.URL.Query(), threadMaxLimit, threadMaxLimit)
	if err != nil {
		return nil, err
	}
	if page.cur != nil && (page.cur.seqKeyed || page.cur.versioned || page.cur.dir != dirNext) {
		return nil, errInvalid("invalid page cursor")
	}
	if err := s.sessionExists(ctx, id); err != nil {
		return nil, err
	}
	query := `SELECT ` + threadColumns + ` FROM session_threads t JOIN sessions s ON s.id = t.session_id
	 WHERE t.session_id = $1`
	args := []any{id}
	if page.cur != nil {
		args = append(args, page.cur.t, page.cur.id)
		query += ` AND (t.created_at, t.id) > ($2, $3)`
	}
	args = append(args, page.limit+1)
	query += fmt.Sprintf(` ORDER BY t.created_at ASC, t.id ASC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	data := []any{}
	var last threadRow
	more := false
	for rows.Next() {
		row, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		if len(data) == page.limit {
			more = true
			break
		}
		rendered, err := renderThread(row)
		if err != nil {
			return nil, err
		}
		data = append(data, rendered)
		last = row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var next *string
	if more {
		c := encodeTimeCursor(dirNext, last.createdAt, last.id)
		next = &c
	}
	return pageJSON{Data: data, NextPage: next}, nil
}

// loadThread reads one thread of a session; a missing or foreign thread is a
// 404. forUpdate locks the row (and, through the join, nothing else).
func loadThread(ctx context.Context, db querier, sessionID, threadID string, forUpdate bool) (threadRow, error) {
	q := `SELECT ` + threadColumns + ` FROM session_threads t JOIN sessions s ON s.id = t.session_id
	 WHERE t.session_id = $1 AND t.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF t`
	}
	row, err := scanThread(db.QueryRow(ctx, q, sessionID, threadID))
	if errors.Is(err, pgx.ErrNoRows) {
		return threadRow{}, errNotFound("thread %s not found", threadID)
	}
	return row, err
}

// threadIDs reads and validates the {id}/{tid} path pair.
func threadIDs(r *http.Request) (sessionID, threadID string, err error) {
	sessionID = normalizeSessionID(r.PathValue("id"))
	if err := checkID(sessionID, "session"); err != nil {
		return "", "", err
	}
	threadID = r.PathValue("tid")
	if !domain.ID(threadID).HasPrefix(domain.PrefixSessionThread) || !domain.ID(threadID).Valid() {
		return "", "", errNotFound("thread %s not found", threadID)
	}
	return sessionID, threadID, nil
}

func (s *server) getThread(r *http.Request) (any, error) {
	sessionID, threadID, err := threadIDs(r)
	if err != nil {
		return nil, err
	}
	row, err := loadThread(r.Context(), s.pool, sessionID, threadID, false)
	if err != nil {
		return nil, err
	}
	return renderThread(row)
}

// archiveThread implements POST /v1/sessions/{id}/threads/{tid}/archive: an
// idle child thread is archived and terminated, with its
// session.thread_status_terminated on its own stream and the primary's
// (decision 12); archiving the primary — the session's own life — or a thread
// that is still running or rescheduling is refused (the reference's status
// code for the latter is unrecorded; 400 like the session's own
// archive-while-running). Archiving an archived thread is idempotent.
func (s *server) archiveThread(r *http.Request) (any, error) {
	ctx := r.Context()
	sessionID, threadID, err := threadIDs(r)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Session row first, thread row second — the order the session's own
	// archive takes, so the two cannot deadlock.
	if err := s.lockSession(ctx, tx, sessionID); err != nil {
		return nil, err
	}
	row, err := loadThread(ctx, tx, sessionID, threadID, true)
	if err != nil {
		return nil, err
	}
	if row.parent == nil {
		return nil, errInvalid("the primary thread cannot be archived; archive the session")
	}
	if row.archivedAt == nil {
		if row.status != string(domain.SessionIdle) {
			return nil, errInvalid("thread %s is %s; only an idle thread can be archived", threadID, row.status)
		}
		if row, err = terminateThread(ctx, tx, s.log, row); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return renderThread(row)
}

// lockSession takes the session row lock (404 when the session is gone).
func (s *server) lockSession(ctx context.Context, tx pgx.Tx, id string) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound("session %s not found", id)
	}
	return err
}

// terminateThread ends a live child thread: its unanswered tool calls — an
// idle thread parked on requires_action has them — are closed with error
// results the way an interrupt closes them, each on the surfaces its call was
// on; then archived_at is set, status moves to terminated, and the event goes
// on the child's stream cross-posted to the primary's. The session row is
// locked by the append; the caller holds the thread row.
func terminateThread(ctx context.Context, tx pgx.Tx, log *events.Log, row threadRow) (threadRow, error) {
	uses, err := events.UnansweredThreadToolUses(ctx, tx, domain.ID(row.sessionID), domain.ID(row.id))
	if err != nil {
		return row, err
	}
	batch, err := events.InterruptResults(uses)
	if err != nil {
		return row, err
	}
	for i := range batch {
		batch[i].ThreadID = domain.ID(row.id)
		batch[i].CrossPosted = uses[i].CrossPosted
	}
	if err := tx.QueryRow(ctx,
		`UPDATE session_threads SET status = 'terminated', archived_at = now(), updated_at = now()
		  WHERE id = $1 RETURNING status, archived_at, updated_at`, row.id).
		Scan(&row.status, &row.archivedAt, &row.updatedAt); err != nil {
		return row, err
	}
	payload, err := json.Marshal(map[string]any{"session_thread_id": row.id, "agent_name": row.agentName})
	if err != nil {
		return row, err
	}
	batch = append(batch, events.NewEvent{
		Type: domain.EventSessionThreadStatusTerminated, Payload: payload,
		ThreadID: domain.ID(row.id), CrossPosted: true,
	})
	_, err = log.AppendInTx(ctx, tx, domain.ID(row.sessionID), batch, events.AppendOptions{})
	return row, err
}

// terminateLiveChildren ends every live child of a session being archived
// (decision 12: a child terminates on the session's own end). Runs in the
// caller's transaction before the session is marked archived (the log refuses
// appends afterwards). Deletion has no rows to keep — deleteSession broadcasts
// the same event ephemerally instead.
func terminateLiveChildren(ctx context.Context, tx pgx.Tx, log *events.Log, sessionID string) error {
	children, err := liveChildThreads(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	for _, child := range children {
		if _, err := terminateThread(ctx, tx, log, child); err != nil {
			return err
		}
	}
	return nil
}

// liveChildThreads locks and returns a session's unarchived child threads in
// creation order. The session row is the caller's to lock first.
func liveChildThreads(ctx context.Context, tx pgx.Tx, sessionID string) ([]threadRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+threadColumns+` FROM session_threads t JOIN sessions s ON s.id = t.session_id
	 WHERE t.session_id = $1 AND t.parent_thread_id IS NOT NULL AND t.archived_at IS NULL
	 ORDER BY t.created_at, t.id FOR UPDATE OF t`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var children []threadRow
	for rows.Next() {
		row, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		children = append(children, row)
	}
	return children, rows.Err()
}

// threadScope is the events surface a thread serves (decision 2): the
// primary's is the session view; a child's is its own rows.
func threadScope(sessionID, threadID string) events.ListQuery {
	if domain.PrimaryThreadID(domain.ID(sessionID)).String() == threadID {
		return events.ListQuery{Scope: events.ScopeSession}
	}
	return events.ListQuery{Scope: events.ScopeThread, ThreadID: domain.ID(threadID)}
}

// listThreadEvents implements GET /v1/sessions/{id}/threads/{tid}/events:
// the thread's own view, ascending by seq, limit/page only — the params carry
// no order, types[] or created_at filters, so sending one is a 400 rather
// than a silent default.
func (s *server) listThreadEvents(r *http.Request) (any, error) {
	sessionID, threadID, err := threadIDs(r)
	if err != nil {
		return nil, err
	}
	if _, err := loadThread(r.Context(), s.pool, sessionID, threadID, false); err != nil {
		return nil, err
	}
	return s.listEvents(r, sessionID, threadScope(sessionID, threadID), false)
}

// streamThreadEvents implements GET /v1/sessions/{id}/threads/{tid}/stream.
func (s *server) streamThreadEvents(w http.ResponseWriter, r *http.Request) {
	sessionID, threadID, err := threadIDs(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if _, err := loadThread(r.Context(), s.pool, sessionID, threadID, false); err != nil {
		writeError(w, r, err)
		return
	}
	scope := threadScope(sessionID, threadID)
	s.streamEvents(w, r, sessionID, scope, s.broker.SubscribeThread(domain.ID(sessionID), scope.ThreadID))
}
