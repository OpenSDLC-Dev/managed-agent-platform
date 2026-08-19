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

// The thread substrate in the log (plan 35 slice 2): StatusChange pairs, the
// agent_name completed under the session lock, the per-thread scopes, the
// primary row's status and usage mirror, and the broker's per-thread frames.

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

func TestStatusChangeIsAPrimaryThreadPair(t *testing.T) {
	sid := domain.ID("sesn_0123456789abcdefghjkmnpqrs")
	primary := "sthr_0123456789abcdefghjkmnpqrs"
	stop := &domain.StopReason{Type: domain.StopRequiresAction, EventIDs: []domain.ID{"sevt_a"}}
	cases := []struct {
		status domain.SessionStatus
		stop   *domain.StopReason
		want   []domain.EventType
	}{
		{domain.SessionRunning, nil, []domain.EventType{domain.EventSessionThreadStatusRunning, domain.EventSessionStatusRunning}},
		{domain.SessionIdle, stop, []domain.EventType{domain.EventSessionThreadStatusIdle, domain.EventSessionStatusIdle}},
		{domain.SessionRescheduling, nil, []domain.EventType{domain.EventSessionThreadStatusRescheduled, domain.EventSessionStatusRescheduled}},
		// The primary never terminates: the session's end is its own event only.
		{domain.SessionTerminated, nil, []domain.EventType{domain.EventSessionStatusTerminated}},
	}
	for _, c := range cases {
		got := events.StatusChange(sid, c.status, c.stop)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d events, want %v", c.status, len(got), c.want)
		}
		for i, ev := range got {
			if ev.Type != c.want[i] {
				t.Errorf("%s[%d] = %s, want %s", c.status, i, ev.Type, c.want[i])
			}
			if ev.ThreadID != "" || ev.CrossPosted {
				t.Errorf("%s: the pair is the session's own rows, got thread %q cross_posted %v", c.status, ev.ThreadID, ev.CrossPosted)
			}
			var p map[string]any
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			isThread := i == 0 && len(got) == 2
			if tid, has := p["session_thread_id"]; has != isThread || (isThread && tid != primary) {
				t.Errorf("%s: payload %s, session_thread_id wanted on the thread event only (%s)", ev.Type, ev.Payload, primary)
			}
			if _, has := p["agent_name"]; has {
				t.Errorf("%s: agent_name is completed at append, not built here: %s", ev.Type, ev.Payload)
			}
			if _, has := p["stop_reason"]; has != (c.stop != nil) {
				t.Errorf("%s: stop_reason presence = %v in %s", ev.Type, has, ev.Payload)
			}
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("an unknown status did not panic")
		}
	}()
	events.StatusChange(sid, domain.SessionStatus("bogus"), nil)
}

// AppendInTx completes agent_name on the primary thread's status events from
// the session's resolved agent, leaves session.thread_created alone (it names
// the child's agent — its emitter's job), and leaves child-thread rows
// untouched; the primary session_threads row follows SetStatus and AddUsage.
func TestAppendCompletesAgentNameAndMirrorsThePrimaryRow(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	primary := domain.PrimaryThreadID(sid)
	child := domain.NewID(domain.PrefixSessionThread)

	running := domain.SessionRunning
	batch := append(events.StatusChange(sid, running, nil),
		events.NewEvent{Type: domain.EventSessionThreadCreated,
			Payload: []byte(`{"session_thread_id":"` + child.String() + `"}`)},
		events.NewEvent{Type: domain.EventSessionThreadStatusRunning, ThreadID: child, CrossPosted: true,
			Payload: []byte(`{"session_thread_id":"` + child.String() + `"}`)},
	)
	got, err := log.AppendWith(ctx, sid, batch, events.AppendOptions{
		SetStatus: &running,
		AddUsage:  &domain.ModelUsage{InputTokens: 5, OutputTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	name := func(i int) any {
		var p map[string]any
		_ = json.Unmarshal(got[i].Body, &p)
		return p["agent_name"]
	}
	if name(0) != "named" || name(1) != nil {
		t.Errorf("primary pair agent_name = %v / %v, want named / absent", name(0), name(1))
	}
	if name(2) != nil {
		t.Errorf("session.thread_created got an agent_name completed (%v); it names the child, so its emitter supplies it", name(2))
	}

	if name(3) != nil || got[3].ThreadID != child {
		t.Errorf("child-thread row = %s thread %q, want untouched payload on thread %s", got[3].Body, got[3].ThreadID, child)
	}
	if got[0].ThreadID != "" || got[0].Seq != 1 || got[3].Seq != 4 {
		t.Errorf("the batch shares the session's one sequence: %+v", got)
	}

	var status string
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT status, usage FROM session_threads WHERE id = $1`, primary.String()).Scan(&status, &raw); err != nil {
		t.Fatal(err)
	}
	var usage domain.Usage
	_ = json.Unmarshal(raw, &usage)
	if status != "running" || usage.InputTokens != 5 || usage.OutputTokens != 2 {
		t.Errorf("primary row = %s %s, want running with the session's usage", status, raw)
	}
	var sessionRaw []byte
	_ = pool.QueryRow(ctx, `SELECT usage FROM sessions WHERE id = $1`, sid.String()).Scan(&sessionRaw)
	if string(sessionRaw) != string(raw) {
		t.Errorf("primary usage %s != session usage %s", raw, sessionRaw)
	}
	// A session without a primary row (the legacy shape before the migration
	// ran, or a fixture) still appends: the mirror is an UPDATE of nothing.
	bare := newSession(t, pool)
	if _, err := log.AppendWith(ctx, bare, events.StatusChange(bare, running, nil), events.AppendOptions{SetStatus: &running}); err != nil {
		t.Fatalf("append on a session without a thread row: %v", err)
	}
}

// List scopes: ScopeAll is every row; ScopeSession is the primary's rows
// plus what children cross-post; ScopeThread is one child's rows.
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
