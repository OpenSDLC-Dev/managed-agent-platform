package events

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
)

// RouteInbound resolves the thread each inbound event addresses (plan 35
// decision 9) and checks the client's explicit session_thread_id against it,
// after the batch has passed ValidateToolResults and ValidateToolConfirmations
// (so every reference names a tool use in this session). A confirmation or a
// result is written on the thread of the tool use it answers — cross-posted
// when the call was, so the answer shows on the same surfaces — and a claim
// that names another thread is the client's 400. An interrupt's claim names
// the one thread it ends: it must be a live thread of this session; the
// primary's own id means the primary. user.message, user.define_outcome and
// system.message carry no thread and address the primary (ThreadID stays
// empty). A child-scoped interrupt is cross-posted: the client sent it through
// the session, so the session view shows it with the thread named, the
// child's own view with null — as the answers to a cross-posted call are
// (INFERRED, docs/DIVERGENCES.md). On return each event's ThreadID and
// CrossPosted are what the append stores; the returned slice marks, per
// event, whether the client named a thread at all — what tells a
// thread-scoped interrupt (the primary's own id included) from a session-wide
// one once both carry an empty ThreadID.
func RouteInbound(ctx context.Context, q Querier, sessionID domain.ID, evs []NewEvent) ([]bool, error) {
	primary := domain.PrimaryThreadID(sessionID)
	scoped := make([]bool, len(evs))
	for i := range evs {
		ev := &evs[i]
		claim := ev.ThreadID
		scoped[i] = claim != ""
		switch ev.Type {
		case domain.EventUserToolConfirm, domain.EventUserToolResult, domain.EventUserCustomToolRes:
			refKey := resultRefKey(ev.Type)
			if refKey == "" {
				refKey = "tool_use_id"
			}
			ref, err := payloadString(ev.Payload, refKey)
			if err != nil {
				return nil, fmt.Errorf("events[%d]: %w", i, err)
			}
			var thread string
			var crossPosted bool
			err = q.QueryRow(ctx,
				`SELECT COALESCE(thread_id, ''), cross_posted FROM events WHERE session_id = $1 AND id = $2`,
				sessionID.String(), ref).Scan(&thread, &crossPosted)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("events[%d]: %s %q does not name a tool use in this session", i, refKey, ref)
			}
			if err != nil {
				return nil, fmt.Errorf("route inbound event: %w", err)
			}
			owner := domain.ID(thread)
			if claim != "" {
				want := owner
				if want == "" {
					want = primary
				}
				if claim != want {
					return nil, fmt.Errorf("events[%d]: session_thread_id %q does not match the thread of tool use %q (%s)", i, claim, ref, want)
				}
			}
			ev.ThreadID, ev.CrossPosted = owner, crossPosted
		case domain.EventUserInterrupt:
			if claim == "" {
				continue
			}
			if claim == primary {
				ev.ThreadID = ""
				continue
			}
			var archivedAt *time.Time
			err := q.QueryRow(ctx,
				`SELECT archived_at FROM session_threads WHERE id = $1 AND session_id = $2`,
				claim.String(), sessionID.String()).Scan(&archivedAt)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("events[%d]: session_thread_id %q does not name a thread in this session", i, claim)
			}
			if err != nil {
				return nil, fmt.Errorf("route inbound event: %w", err)
			}
			if archivedAt != nil {
				return nil, fmt.Errorf("events[%d]: thread %s is archived", i, claim)
			}
			ev.CrossPosted = true
		}
	}
	return scoped, nil
}
