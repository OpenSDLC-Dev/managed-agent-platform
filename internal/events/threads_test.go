package events_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The thread substrate in the log (plan 35): the per-thread scopes and the
// broker's per-thread frames; the status fold is transition_test.go.

// newThreadedSession is newSession with a named resolved agent and the
// primary session_threads row the control plane writes at create.
func newThreadedSession(t *testing.T, pool *pgxpool.Pool) domain.ID {
	t.Helper()
	sid := newSession(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sessions SET resolved_agent = '{"name":"named"}' WHERE id = $1`, sid.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO session_threads (id, session_id, agent_name, status) VALUES ($1, $2, 'named', 'idle')`,
		domain.PrimaryThreadID(sid).String(), sid.String()); err != nil {
		t.Fatal(err)
	}
	return sid
}

// List scopes: ScopeAll is every row; ScopeSession is the primary's rows
// plus what children cross-post; ScopeThread is one thread's own rows — a
// child's, or the primary's with no ThreadID.
func TestListScopes(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	a, b := domain.NewID(domain.PrefixSessionThread), domain.NewID(domain.PrefixSessionThread)

	if _, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("primary")},
		{Type: domain.EventAgentMessage, ThreadID: a, Payload: text("a-private")},
		{Type: domain.EventAgentToolUse, ThreadID: a, CrossPosted: true, Payload: []byte(`{"tool_use_id":"t","name":"bash","input":{}}`)},
		{Type: domain.EventAgentMessage, ThreadID: b, Payload: text("b-private")},
		{Type: domain.EventAgentMessage, CrossPosted: true, Payload: text("primary-flagged")},
	}); err != nil {
		t.Fatal(err)
	}
	seqs := func(q events.ListQuery) []int64 {
		evs, err := log.List(ctx, sid, q)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int64, 0, len(evs))
		for _, ev := range evs {
			out = append(out, ev.Seq)
		}
		return out
	}
	one := int64(1)
	cases := []struct {
		name string
		q    events.ListQuery
		want []int64
	}{
		{"all", events.ListQuery{}, []int64{1, 2, 3, 4, 5}},
		{"session", events.ListQuery{Scope: events.ScopeSession}, []int64{1, 3, 5}},
		{"thread a", events.ListQuery{Scope: events.ScopeThread, ThreadID: a}, []int64{2, 3}},
		{"thread b", events.ListQuery{Scope: events.ScopeThread, ThreadID: b}, []int64{4}},
		// The primary's own rows — what its turn replays: never a child's
		// cross-post (a sibling's ask is not this conversation's tool_use).
		{"primary own", events.ListQuery{Scope: events.ScopeThread}, []int64{1, 5}},
		{"session after 1", events.ListQuery{Scope: events.ScopeSession, AfterSeq: &one, Limit: 1}, []int64{3}},
		{"session typed", events.ListQuery{Scope: events.ScopeSession, Types: []string{"agent.message"}}, []int64{5}},
	}
	for _, c := range cases {
		got := seqs(c.q)
		if len(got) != len(c.want) {
			t.Errorf("%s: seqs %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: seqs %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
	evs, _ := log.List(ctx, sid, events.ListQuery{Scope: events.ScopeThread, ThreadID: a})
	if evs[0].ThreadID != a {
		t.Errorf("listed thread_id = %q, want %s", evs[0].ThreadID, a)
	}
}

// The broker hands a frame to the subscribers of the thread named in its
// envelope — the session stream gets unaddressed frames, a child's stream its
// own — and session.deleted to everyone.
func TestSubscribeThreadFiltersFrames(t *testing.T) {
	pool := pgtest.NewPool(t)
	broker := events.NewBroker(pool)
	sid := newSession(t, pool)
	child := domain.NewID(domain.PrefixSessionThread)

	session := subscribeReady(t, broker, sid)
	own := broker.SubscribeThread(sid, child)
	t.Cleanup(own.Close)
	other := broker.SubscribeThread(sid, domain.NewID(domain.PrefixSessionThread))
	t.Cleanup(other.Close)

	ctx := context.Background()
	notify := func(envelope string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `SELECT pg_notify('map_session_frames', $1)`, envelope); err != nil {
			t.Fatal(err)
		}
	}
	sess := `"session_id":"` + sid.String() + `"`
	notify(`{` + sess + `,"frame":{"type":"agent.message","preview":true}}`)
	notify(`{` + sess + `,"thread_id":"` + child.String() + `","frame":{"type":"content_delta","delta":{"type":"text_delta","text":"x"}}}`)
	notify(`{` + sess + `,"frame":{"type":"session.deleted","id":"sevt_x"}}`)

	if f := waitFrame(t, session); f["type"] != "agent.message" {
		t.Errorf("session stream first frame = %v, want the unaddressed preview", f["type"])
	}
	if f := waitFrame(t, session); f["type"] != "session.deleted" {
		t.Errorf("session stream second frame = %v, want session.deleted", f["type"])
	}
	if f := waitFrame(t, own); f["type"] != "content_delta" {
		t.Errorf("child stream first frame = %v, want its delta", f["type"])
	}
	if f := waitFrame(t, own); f["type"] != "session.deleted" {
		t.Errorf("child stream second frame = %v, want session.deleted", f["type"])
	}
	if f := waitFrame(t, other); f["type"] != "session.deleted" {
		t.Errorf("other child's only frame = %v, want session.deleted", f["type"])
	}
	for name, sub := range map[string]*events.Subscription{"session": session, "own": own, "other": other} {
		select {
		case raw := <-sub.Frames():
			t.Errorf("%s stream received an extra frame %s", name, raw)
		default:
		}
	}
}
