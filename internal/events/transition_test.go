package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The status fold (plan 35 decision 4): TransitionThread moves one thread,
// folds the session over its live threads, and emits the thread event first
// and the session event second — only when the folded value changed, or the
// caller forced the two value-independent emissions.

// transition runs one TransitionThread under the session lock in its own
// transaction, appends what it returns, and reports the event types appended
// in order, the status the session column moved to, and the stored payloads.
func transition(t *testing.T, pool *pgxpool.Pool, log *events.Log, sid domain.ID, tr events.ThreadTransition) ([]domain.Event, *domain.SessionStatus) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		t.Fatal(err)
	}
	batch, moved, err := events.TransitionThread(ctx, tx, sid, tr)
	if err != nil {
		t.Fatalf("TransitionThread(%+v): %v", tr, err)
	}
	var appended []domain.Event
	if len(batch) > 0 {
		if appended, err = log.AppendInTx(ctx, tx, sid, batch, events.AppendOptions{SetStatus: moved}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return appended, moved
}

func types(evs []domain.Event) []domain.EventType {
	out := make([]domain.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func sameTypes(got []domain.Event, want ...domain.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].Type != want[i] {
			return false
		}
	}
	return true
}

func threadStatus(t *testing.T, pool *pgxpool.Pool, tid domain.ID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM session_threads WHERE id = $1`, tid.String()).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func bodyOf(t *testing.T, ev domain.Event) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(ev.Body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// The single-thread reduction: every move of the primary is a move of the
// session, so every transition is the pair — thread event first, named with
// the session's agent, then the session event — and the column follows.
func TestTransitionThreadSingleThreadIsThePair(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	primary := domain.PrimaryThreadID(sid)
	running, idle := domain.SessionRunning, domain.SessionIdle
	stop := &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{"sevt_a"}}

	got, moved := transition(t, pool, log, sid, events.ThreadTransition{Status: running})
	if !sameTypes(got, domain.EventSessionThreadStatusRunning, domain.EventSessionStatusRunning) {
		t.Fatalf("idle→running = %v, want the thread/session pair", types(got))
	}
	if moved == nil || *moved != running || sessionStatus(t, pool, sid) != "running" || threadStatus(t, pool, primary) != "running" {
		t.Errorf("moved = %v, session %s, thread %s; want running everywhere", moved, sessionStatus(t, pool, sid), threadStatus(t, pool, primary))
	}
	p := bodyOf(t, got[0])
	if p["session_thread_id"] != primary.String() || p["agent_name"] != "named" || got[0].ThreadID != "" {
		t.Errorf("primary thread event = %s on thread %q, want the primary id and the session's agent name on the session's own row", got[0].Body, got[0].ThreadID)
	}
	if _, has := bodyOf(t, got[1])["session_thread_id"]; has {
		t.Errorf("session event carries a session_thread_id: %s", got[1].Body)
	}

	got, moved = transition(t, pool, log, sid, events.ThreadTransition{Status: idle, Stop: stop})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle, domain.EventSessionStatusIdle) || moved == nil || *moved != idle {
		t.Fatalf("running→idle = %v moved %v", types(got), moved)
	}
	for i := range got {
		if sr, _ := bodyOf(t, got[i])["stop_reason"].(map[string]any); sr["type"] != "requires_action" {
			t.Errorf("event %d stop_reason = %v, want requires_action on both", i, bodyOf(t, got[i])["stop_reason"])
		}
	}

	// A re-idle with a new stop reason is no value change: nothing moves and,
	// unasked, only the thread speaks; with Reemit — the interrupt's re-emit, the
	// confirmation's shrunken ask set — the session event is emitted too.
	stop2 := &domain.StopReason{Type: domain.StopEndTurn}
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{Status: idle, Stop: stop2})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle) || moved != nil {
		t.Errorf("unforced idle→idle = %v moved %v, want the thread event alone and no move", types(got), moved)
	}
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{Status: idle, Stop: stop2, Reemit: true})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle, domain.EventSessionStatusIdle) || moved != nil {
		t.Errorf("forced idle→idle = %v moved %v, want the pair and no move", types(got), moved)
	}
	if sr, _ := bodyOf(t, got[1])["stop_reason"].(map[string]any); sr["type"] != "end_turn" {
		t.Errorf("forced re-idle session stop_reason = %v, want end_turn", bodyOf(t, got[1])["stop_reason"])
	}

	// The reclaim pair on a running session: rescheduled then running, both
	// forced, the column back where it was.
	transition(t, pool, log, sid, events.ThreadTransition{Status: running})
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	_, _ = tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String())
	a, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{Status: domain.SessionRescheduling, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{Status: running, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := log.AppendInTx(ctx, tx, sid, append(a, b...), events.AppendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !sameTypes(appended, domain.EventSessionThreadStatusRescheduled, domain.EventSessionStatusRescheduled,
		domain.EventSessionThreadStatusRunning, domain.EventSessionStatusRunning) || sessionStatus(t, pool, sid) != "running" {
		t.Errorf("reclaim pair = %v, session %s", types(appended), sessionStatus(t, pool, sid))
	}

	// The primary never terminates on the thread surface: the session's end
	// is its own event only.
	got, _ = transition(t, pool, log, sid, events.ThreadTransition{Status: domain.SessionTerminated})
	if !sameTypes(got, domain.EventSessionStatusTerminated) || sessionStatus(t, pool, sid) != "terminated" {
		t.Errorf("terminated = %v session %s, want the session event alone", types(got), sessionStatus(t, pool, sid))
	}
	if _, _, err := events.TransitionThread(ctx, nil, sid, events.ThreadTransition{Status: "bogus"}); err == nil {
		t.Error("an unknown status was accepted")
	}
}

// A session from before the thread resource — no session_threads row at all —
// still transitions: the fold is the transition itself.
func TestTransitionThreadWithoutThreadRows(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	got, moved := transition(t, pool, log, sid, events.ThreadTransition{Status: domain.SessionRunning})
	if !sameTypes(got, domain.EventSessionThreadStatusRunning, domain.EventSessionStatusRunning) || moved == nil || sessionStatus(t, pool, sid) != "running" {
		t.Errorf("bare session: %v moved %v status %s", types(got), moved, sessionStatus(t, pool, sid))
	}
	ctx := context.Background()
	tx, _ := pool.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err := events.TransitionThread(ctx, tx, sid, events.ThreadTransition{ThreadID: "sthr_missing", Status: domain.SessionRunning}); err == nil {
		t.Error("a child thread that does not exist was moved")
	}
}

// The rollup over child threads: a child's own move emits its cross-posted
// thread event and a session event only when the fold changes; idle stop
// reasons pick requires_action over end_turn with the ask ids unioned in log
// order; a child's termination leaves the session where it was.
func TestTransitionThreadFoldsOverChildren(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	a := pgtest.NewChildThread(t, pool, sid)
	b := pgtest.NewChildThread(t, pool, sid)
	running, idle := domain.SessionRunning, domain.SessionIdle
	endTurn := &domain.StopReason{Type: domain.StopEndTurn}

	// Primary running, session running.
	transition(t, pool, log, sid, events.ThreadTransition{Status: running})
	// A child spawned running: thread event only, cross-posted and named.
	got, moved := transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: running})
	if !sameTypes(got, domain.EventSessionThreadStatusRunning) || moved != nil {
		t.Fatalf("child running under a running primary = %v moved %v, want its thread event alone", types(got), moved)
	}
	if got[0].ThreadID != a || bodyOf(t, got[0])["agent_name"] != "worker" || bodyOf(t, got[0])["session_thread_id"] != a.String() {
		t.Errorf("child thread event = %s on %q, want cross-posted from %s naming worker", got[0].Body, got[0].ThreadID, a)
	}
	var crossPosted bool
	_ = pool.QueryRow(ctx, `SELECT cross_posted FROM events WHERE id = $1`, got[0].ID.String()).Scan(&crossPosted)
	if !crossPosted {
		t.Error("a child's status event is not cross-posted to the session view")
	}
	transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: running})

	// The coordinator's W1 park: primary idle while children run — the
	// session stays running and says nothing.
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{Status: idle, Stop: endTurn})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle) || moved != nil || sessionStatus(t, pool, sid) != "running" {
		t.Errorf("primary park under running children = %v moved %v session %s, want thread event only, still running", types(got), moved, sessionStatus(t, pool, sid))
	}
	// A child idles on end_turn: not the session's quiescence while its
	// sibling runs.
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: idle, Stop: endTurn})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle) || moved != nil {
		t.Errorf("child idle under a running sibling = %v moved %v", types(got), moved)
	}
	// Quiescence: the last running thread parks on a human — the session idles
	// once, requires_action outranking the others' end_turn.
	askB := &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{"sevt_b1"}}
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: idle, Stop: askB})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle, domain.EventSessionStatusIdle) || moved == nil || *moved != idle {
		t.Fatalf("quiescence = %v moved %v, want the pair and a move to idle", types(got), moved)
	}
	if sr, _ := bodyOf(t, got[1])["stop_reason"].(map[string]any); sr["type"] != "requires_action" {
		t.Errorf("session idle stop_reason = %v, want requires_action (≻ end_turn)", bodyOf(t, got[1])["stop_reason"])
	}

	// A second asker: the session re-idles (Reemit, the payload-only re-idle)
	// with the union of both threads' asks in log order. Seed the ask events
	// so the union can be ordered by seq: a's ask is appended after b's.
	ids, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventAgentToolUse, ThreadID: b, CrossPosted: true, Payload: []byte(`{"tool_use_id":"x","name":"bash","input":{},"evaluated_permission":"ask"}`)},
		{Type: domain.EventAgentToolUse, ThreadID: a, CrossPosted: true, Payload: []byte(`{"tool_use_id":"y","name":"bash","input":{},"evaluated_permission":"ask"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: idle, Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{ids[0].ID}}})
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: idle, Reemit: true,
		Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{ids[1].ID}}})
	if !sameTypes(got, domain.EventSessionThreadStatusIdle, domain.EventSessionStatusIdle) || moved != nil {
		t.Fatalf("forced re-idle = %v moved %v", types(got), moved)
	}
	sr, _ := bodyOf(t, got[1])["stop_reason"].(map[string]any)
	union, _ := sr["event_ids"].([]any)
	if len(union) != 2 || union[0] != ids[0].ID.String() || union[1] != ids[1].ID.String() {
		t.Errorf("session event_ids = %v, want the seq-ordered union [%s %s]", union, ids[0].ID, ids[1].ID)
	}
	// The moving thread's own asks may not be on the log yet — the brain
	// transitions before it appends the batch that carries them, under
	// pre-minted ids — and they close the union, which is their place once
	// appended.
	minted := domain.NewID("sevt")
	got, _ = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: idle, Reemit: true,
		Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{minted}}})
	sr, _ = bodyOf(t, got[1])["stop_reason"].(map[string]any)
	union, _ = sr["event_ids"].([]any)
	if len(union) != 2 || union[0] != ids[0].ID.String() || union[1] != minted.String() {
		t.Errorf("session event_ids with an ask not yet on the log = %v, want [%s %s]", union, ids[0].ID, minted)
	}
	transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: idle, Reemit: true,
		Stop: &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{ids[1].ID}}})
	// The preview agrees with the fold, without writing.
	st, stop, err := events.PreviewTransition(ctx, pool, sid, events.ThreadTransition{ThreadID: a, Status: idle, Stop: endTurn})
	if err != nil || st != idle || stop == nil || stop.Type != domain.StopRequiresAction || len(stop.EventIDs) != 1 {
		t.Errorf("preview(a end_turn) = %s %+v err %v, want idle requires_action with b's one ask", st, stop, err)
	}
	if threadStatus(t, pool, a) != "idle" {
		t.Error("the preview wrote the thread row")
	}
	// A child resumes while the other still asks: the session runs (the
	// confirmation-for-A-while-B-asks shape), and A's later end_turn folds
	// back to b's requires_action.
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: running})
	if !sameTypes(got, domain.EventSessionThreadStatusRunning, domain.EventSessionStatusRunning) || moved == nil || *moved != running {
		t.Errorf("child resume on an idle session = %v moved %v", types(got), moved)
	}
	got, _ = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: idle, Stop: endTurn})
	if sr, _ := bodyOf(t, got[1])["stop_reason"].(map[string]any); sr["type"] != "requires_action" {
		t.Errorf("a's end_turn beside b's ask: session stop_reason = %v, want requires_action", sr)
	}

	// A child's termination: its cross-posted terminated event, the session
	// where it was (b still asks).
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: a, Status: domain.SessionTerminated})
	if !sameTypes(got, domain.EventSessionThreadStatusTerminated) || moved != nil || sessionStatus(t, pool, sid) != "idle" {
		t.Errorf("child terminated = %v moved %v session %s, want its event alone and the session untouched", types(got), moved, sessionStatus(t, pool, sid))
	}
	if threadStatus(t, pool, a) != "terminated" {
		t.Error("terminated child row not written")
	}
	// ...and with a terminated (excluded) child, b's resume makes the session
	// run and b's end_turn makes it idle end_turn — the primary parked on
	// end_turn and a gone.
	transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: running})
	got, moved = transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: idle, Stop: endTurn})
	if moved == nil || *moved != idle {
		t.Fatalf("final quiescence moved %v", moved)
	}
	if sr, _ := bodyOf(t, got[1])["stop_reason"].(map[string]any); sr["type"] != "end_turn" {
		t.Errorf("final idle stop_reason = %v, want end_turn", sr)
	}

	// A rescheduling child outranks idle siblings below running.
	transition(t, pool, log, sid, events.ThreadTransition{ThreadID: b, Status: domain.SessionRescheduling})
	if sessionStatus(t, pool, sid) != "rescheduling" {
		t.Errorf("session = %s with a rescheduling child, want rescheduling", sessionStatus(t, pool, sid))
	}
}

// AppendInTx folds usage into the turn's thread row (the primary without a
// ThreadID) and no longer mirrors status: the fold owns the status column.
func TestAppendFoldsUsagePerThread(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	primary := domain.PrimaryThreadID(sid)
	child := pgtest.NewChildThread(t, pool, sid)

	usage := func(tid domain.ID) domain.Usage {
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT usage FROM session_threads WHERE id = $1`, tid.String()).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var u domain.Usage
		_ = json.Unmarshal(raw, &u)
		return u
	}
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{AddUsage: &domain.ModelUsage{InputTokens: 5, OutputTokens: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{ThreadID: child, AddUsage: &domain.ModelUsage{InputTokens: 1, OutputTokens: 1}}); err != nil {
		t.Fatal(err)
	}
	if p, c := usage(primary), usage(child); p.InputTokens != 5 || p.OutputTokens != 2 || c.InputTokens != 1 || c.OutputTokens != 1 {
		t.Errorf("primary %+v child %+v, want 5/2 and 1/1", p, c)
	}
	var sessionRaw []byte
	_ = pool.QueryRow(ctx, `SELECT usage FROM sessions WHERE id = $1`, sid.String()).Scan(&sessionRaw)
	var total domain.Usage
	_ = json.Unmarshal(sessionRaw, &total)
	if total.InputTokens != 6 || total.OutputTokens != 3 {
		t.Errorf("session usage %+v, want the sum 6/3", total)
	}
	running := domain.SessionRunning
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{SetStatus: &running}); err != nil {
		t.Fatal(err)
	}
	if threadStatus(t, pool, primary) != "idle" {
		t.Error("SetStatus still mirrors onto the primary thread row; the fold owns it now")
	}
	// The agent_name completion for the primary's thread events stays.
	got, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventSessionThreadStatusIdle, Payload: []byte(`{"session_thread_id":"` + primary.String() + `"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if bodyOf(t, got[0])["agent_name"] != "named" {
		t.Errorf("primary thread event agent_name = %v, want named", bodyOf(t, got[0])["agent_name"])
	}
	// A bare session (no thread row) appends usage without complaint.
	bare := newSession(t, pool)
	if _, err := log.AppendWith(ctx, bare, nil, events.AppendOptions{AddUsage: &domain.ModelUsage{InputTokens: 1}}); err != nil {
		t.Fatalf("append usage on a session without a thread row: %v", err)
	}
}

// The thread-scoped watermark never stamps a sibling's queued input.
func TestMarkProcessedThroughIsThreadScoped(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	child := pgtest.NewChildThread(t, pool, sid)
	got, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("to the primary")},
		{Type: domain.EventUserToolConfirm, ThreadID: child, Payload: []byte(`{"result":"allow","tool_use_id":"sevt_x","deny_message":null,"session_thread_id":null}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{MarkProcessedThrough: got[1].Seq}); err != nil {
		t.Fatal(err)
	}
	processed := func(id domain.ID) bool {
		var p *string
		_ = pool.QueryRow(ctx, `SELECT processed_at::text FROM events WHERE id = $1`, id.String()).Scan(&p)
		return p != nil
	}
	if !processed(got[0].ID) || processed(got[1].ID) {
		t.Errorf("primary watermark: primary processed %v child processed %v, want true/false", processed(got[0].ID), processed(got[1].ID))
	}
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{ThreadID: child, MarkProcessedThrough: got[1].Seq}); err != nil {
		t.Fatal(err)
	}
	if !processed(got[1].ID) {
		t.Error("child watermark did not stamp the child's own input")
	}
}
