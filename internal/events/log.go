// Package events implements the append-only session event log — the single
// source of truth for session state — plus its live fan-out: per-session seq
// allocation, list queries, a Postgres LISTEN/NOTIFY broker for SSE
// subscribers, ephemeral event_start/event_delta preview frames, and the
// span.* events emitted from the same instrumentation point as OTel spans.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notification channels shared by every control-plane replica. Payloads carry
// the session id (and for frames the frame itself); LISTEN is server-wide, so
// one listening connection per process serves every subscriber.
const (
	channelEvents = "map_session_events"
	channelFrames = "map_session_frames"
)

// Sentinel errors the API layer maps onto wire error envelopes.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionArchived = errors.New("session is archived")
)

// NewEvent is one event to append. Payload holds the normalized type-specific
// wire fields only — never id/type/processed_at, which live on the envelope.
type NewEvent struct {
	ID          domain.ID // optional; generated when empty (previews pre-allocate)
	Type        domain.EventType
	Payload     json.RawMessage
	ProcessedAt *time.Time // nil = queued, awaiting in-order processing
}

// Log is the append-only event store over the shared pool.
type Log struct {
	pool *pgxpool.Pool
}

func NewLog(pool *pgxpool.Pool) *Log { return &Log{pool: pool} }

// Append durably appends events to one session's log in order, allocating
// the per-session seq under the session row lock (concurrent appends to the
// same session serialize; different sessions don't contend), and notifies
// stream subscribers on commit.
func (l *Log) Append(ctx context.Context, sessionID domain.ID, evs []NewEvent) ([]domain.Event, error) {
	if len(evs) == 0 {
		return nil, errors.New("append requires at least one event")
	}
	for _, ev := range evs {
		if !ev.Type.Persisted() {
			return nil, fmt.Errorf("event type %q is stream-only and cannot be persisted", ev.Type)
		}
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var archivedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT archived_at FROM sessions WHERE id = $1 FOR UPDATE`, sessionID.String()).Scan(&archivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if archivedAt != nil {
		return nil, ErrSessionArchived
	}

	var seq int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = $1`, sessionID.String()).Scan(&seq); err != nil {
		return nil, err
	}

	// One multi-row INSERT: the session row lock is held for a single round
	// trip however large the batch. created_at is clock_timestamp() — real
	// time AFTER the lock was acquired — never the column default now(),
	// which Postgres freezes at BEGIN and would let a transaction that
	// waited on the lock write a higher seq with an earlier timestamp,
	// breaking the seq/created_at agreement the list filters rely on.
	var (
		sb   strings.Builder
		args []any
		out  = make([]domain.Event, 0, len(evs))
	)
	sb.WriteString(`INSERT INTO events (id, session_id, seq, type, payload, processed_at, created_at) VALUES `)
	for i, ev := range evs {
		seq++
		id := ev.ID
		if id == "" {
			id = domain.NewID("sevt")
		}
		payload := ev.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		n := len(args)
		fmt.Fprintf(&sb, "($%d, $%d, $%d, $%d, $%d, $%d, clock_timestamp())", n+1, n+2, n+3, n+4, n+5, n+6)
		args = append(args, id.String(), sessionID.String(), seq, string(ev.Type), payload, utcOrNil(ev.ProcessedAt))
		out = append(out, domain.Event{
			ID:          id,
			SessionID:   sessionID,
			Seq:         seq,
			Type:        ev.Type,
			Body:        payload,
			ProcessedAt: utcOrNil(ev.ProcessedAt),
		})
	}
	sb.WriteString(` RETURNING created_at`)
	rows, err := tx.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	for i := 0; rows.Next(); i++ {
		var createdAt time.Time
		if err := rows.Scan(&createdAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[i].CreatedAt = createdAt.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// NOTIFY fires on commit, so subscribers only ever wake for committed
	// rows. The payload is just a pointer — subscribers re-read the log.
	if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`,
		channelEvents, `{"session_id":"`+sessionID.String()+`"}`); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// ListQuery narrows and pages a session's event log. AfterSeq is a keyset
// position (exclusive) in the direction of the sort; seq order and
// created_at order agree because appends serialize per session.
type ListQuery struct {
	Types                                        []string
	CreatedGT, CreatedGTE, CreatedLT, CreatedLTE *time.Time
	AfterSeq                                     *int64
	Desc                                         bool
	Limit                                        int // 0 = unlimited
}

// List returns events for one session in seq order. It does not check that
// the session exists — callers that need 404 semantics check first.
func (l *Log) List(ctx context.Context, sessionID domain.ID, q ListQuery) ([]domain.Event, error) {
	var sb strings.Builder
	sb.WriteString(`SELECT id, seq, type, payload, processed_at, created_at FROM events WHERE session_id = $1`)
	args := []any{sessionID.String()}
	add := func(clause string, v any) {
		args = append(args, v)
		sb.WriteString(" AND " + clause + "$" + strconv.Itoa(len(args)))
	}
	if len(q.Types) > 0 {
		add("type = ANY(", q.Types)
		sb.WriteString(")")
	}
	if q.CreatedGT != nil {
		add("created_at > ", *q.CreatedGT)
	}
	if q.CreatedGTE != nil {
		add("created_at >= ", *q.CreatedGTE)
	}
	if q.CreatedLT != nil {
		add("created_at < ", *q.CreatedLT)
	}
	if q.CreatedLTE != nil {
		add("created_at <= ", *q.CreatedLTE)
	}
	if q.AfterSeq != nil {
		if q.Desc {
			add("seq < ", *q.AfterSeq)
		} else {
			add("seq > ", *q.AfterSeq)
		}
	}
	if q.Desc {
		sb.WriteString(" ORDER BY seq DESC")
	} else {
		sb.WriteString(" ORDER BY seq ASC")
	}
	if q.Limit > 0 {
		args = append(args, q.Limit)
		sb.WriteString(" LIMIT $" + strconv.Itoa(len(args)))
	}

	rows, err := l.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Event
	for rows.Next() {
		var (
			ev          domain.Event
			id, typ     string
			processedAt *time.Time
		)
		if err := rows.Scan(&id, &ev.Seq, &typ, &ev.Body, &processedAt, &ev.CreatedAt); err != nil {
			return nil, err
		}
		ev.ID = domain.ID(id)
		ev.SessionID = sessionID
		ev.Type = domain.EventType(typ)
		ev.ProcessedAt = utcOrNil(processedAt)
		ev.CreatedAt = ev.CreatedAt.UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// publishFrame broadcasts an ephemeral frame (previews, session.deleted) to
// live stream subscribers. Frames are never persisted and never replayed.
func (l *Log) publishFrame(ctx context.Context, sessionID domain.ID, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(map[string]json.RawMessage{
		"session_id": json.RawMessage(`"` + sessionID.String() + `"`),
		"frame":      raw,
	})
	if err != nil {
		return err
	}
	_, err = l.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channelFrames, string(envelope))
	return err
}

// PublishEventFrame broadcasts a fully-rendered wire event object (e.g. the
// session.deleted event, whose row cannot outlive the session) to live
// stream subscribers without persisting it.
func (l *Log) PublishEventFrame(ctx context.Context, sessionID domain.ID, event map[string]any) error {
	return l.publishFrame(ctx, sessionID, event)
}

func utcOrNil(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
