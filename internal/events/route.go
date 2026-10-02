package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// ThreadNotFoundError refuses an interrupt whose well-formed session_thread_id
// names no thread of this session. The API answers it 404 not_found_error in
// the reference's words, which carry no "events[i]: " prefix (2026-09-02
// batch2 #333 `sessK.send.interrupt.thread-of-other-session`; #841). The
// recording named another session's thread; an id naming no thread anywhere
// misses the same lookup and is answered alike (INFERRED, docs/DIVERGENCES.md).
type ThreadNotFoundError struct{ ID domain.ID }

func (e *ThreadNotFoundError) Error() string { return "Thread not found: " + e.ID.String() }

// Refusal is the client's error in a send batch the log-reading checks refuse
// — RouteInbound, ValidateToolResults, ValidateToolConfirmations and
// ValidateDefineOutcomes: the API answers it 400. Every other error they
// return, a *ThreadNotFoundError and a *NoPendingConfirmationError aside, is a
// fault reading the log — a database error, a cancelled context — and none of
// it is the client's to read.
type Refusal struct{ msg string }

func (e *Refusal) Error() string { return e.msg }

func refuse(format string, args ...any) error {
	return &Refusal{fmt.Sprintf(format, args...)}
}

// RouteInbound resolves the thread each inbound event addresses (plan 35
// decision 9), after the batch has passed ValidateToolResults and
// ValidateToolConfirmations (so every reference names a tool use in this
// session). A confirmation or a result is written on the thread of the tool
// use it answers — cross-posted when the call was, so the answer shows on the
// same surfaces — whatever thread the client named (answerClaim drops that
// claim). An interrupt's claim names the one thread it ends: it must be a live
// thread of this session (a *ThreadNotFoundError when it names none); the
// primary's own id means the primary. user.message, user.define_outcome and
// system.message carry no thread and address the primary (ThreadID stays
// empty). A child-scoped interrupt is cross-posted: the client sent it through
// the session, so the session view shows it with the thread named, the
// child's own view with null — as the answers to a cross-posted call are
// (INFERRED, docs/DIVERGENCES.md). On return each event's ThreadID and
// CrossPosted are what the append stores; the returned slice marks the
// interrupts that named a thread — what tells a thread-scoped interrupt (the
// primary's own id included) from a session-wide one once both carry an empty
// ThreadID.
func RouteInbound(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent) ([]bool, error) {
	primary := domain.PrimaryThreadID(sessionID)
	scoped := make([]bool, len(evs))
	for i := range evs {
		ev := &evs[i]
		switch ev.Type {
		case domain.EventUserToolConfirm, domain.EventUserToolResult, domain.EventUserCustomToolRes:
			refKey := resultRefKey(ev.Type)
			if refKey == "" {
				refKey = "tool_use_id"
			}
			ref, err := payloadString(ev.Payload, refKey)
			if err != nil {
				return nil, refuse("events[%d]: %v", i, err)
			}
			var thread string
			var crossPosted bool
			err = q.QueryRow(ctx,
				`SELECT COALESCE(thread_id, ''), cross_posted FROM events WHERE session_id = $1 AND id = $2`,
				sessionID.String(), ref).Scan(&thread, &crossPosted)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, refuse("events[%d]: %s %q does not name a tool use in this session", i, refKey, ref)
			}
			if err != nil {
				return nil, fmt.Errorf("route inbound event: %w", err)
			}
			ev.ThreadID, ev.CrossPosted = domain.ID(thread), crossPosted
		case domain.EventUserInterrupt:
			claim := ev.ThreadID
			if claim == "" {
				continue
			}
			scoped[i] = true
			if claim == primary {
				ev.ThreadID = ""
				continue
			}
			var archivedAt *time.Time
			var status string
			err := q.QueryRow(ctx,
				`SELECT archived_at, status FROM session_threads WHERE id = $1 AND session_id = $2`,
				claim.String(), sessionID.String()).Scan(&archivedAt, &status)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, &ThreadNotFoundError{ID: claim}
			}
			if err != nil {
				return nil, fmt.Errorf("route inbound event: %w", err)
			}
			if archivedAt != nil {
				return nil, refuse("events[%d]: thread %s is archived", i, claim)
			}
			// Not live even before archived_at lands: termination and archiving
			// travel together today, but the claim's rule is the fold's.
			if status == string(domain.SessionTerminated) {
				return nil, refuse("events[%d]: thread %s is terminated", i, claim)
			}
			ev.CrossPosted = true
		}
	}
	return scoped, nil
}
