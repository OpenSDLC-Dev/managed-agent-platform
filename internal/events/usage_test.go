package events_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// usageT0 anchors the stamps these tests hand AppendWith, so every interval
// they assert is exact.
var usageT0 = time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time {
	t := usageT0.Add(d)
	return &t
}

func status(typ domain.EventType, stamp *time.Time) events.NewEvent {
	return events.NewEvent{Type: typ, ProcessedAt: stamp}
}

func idleAt(stamp *time.Time) events.NewEvent {
	return events.NewEvent{Type: domain.EventSessionStatusIdle, ProcessedAt: stamp,
		Payload: json.RawMessage(`{"stop_reason":{"type":"end_turn"}}`)}
}

func mustAppend(t *testing.T, log *events.Log, sid domain.ID, evs []events.NewEvent, opts events.AppendOptions) []domain.Event {
	t.Helper()
	got, err := log.AppendWith(context.Background(), sid, evs, opts)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return got
}

// snapshot decodes a session.usage event's body.
func snapshot(t *testing.T, ev domain.Event) map[string]any {
	t.Helper()
	if ev.Type != domain.EventSessionUsage {
		t.Fatalf("event %d is %s, want session.usage", ev.Seq, ev.Type)
	}
	var body map[string]any
	if err := json.Unmarshal(ev.Body, &body); err != nil {
		t.Fatalf("decode session.usage: %v", err)
	}
	return body
}

func activeSeconds(t *testing.T, ev domain.Event) float64 {
	t.Helper()
	usage, _ := snapshot(t, ev)["usage"].(map[string]any)
	v, ok := usage["active_seconds"].(float64)
	if !ok {
		t.Fatalf("session.usage %s carries no numeric active_seconds", ev.Body)
	}
	return v
}

func storedActive(t *testing.T, pool *pgxpool.Pool, sid domain.ID) (float64, *time.Time) {
	t.Helper()
	var (
		active float64
		since  *time.Time
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT active_seconds, running_since FROM sessions WHERE id = $1`, sid.String()).Scan(&active, &since); err != nil {
		t.Fatalf("read active_seconds: %v", err)
	}
	return active, since
}

// The reference writes a session.usage immediately before every
// session.status_idle, after the thread's own idle (recorded in
// managed-agents-wire-recordings; plan 68). The snapshot holds the session's
// token totals including the turn this append settles, and the time the
// session spent running, and nothing this platform cannot measure: no budget,
// list_cost or server_tool_use, and no thread id.
func TestUsageSnapshotPrecedesTheSessionIdle(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	if _, err := pool.Exec(context.Background(), `UPDATE sessions SET usage = $2 WHERE id = $1`, sid.String(),
		`{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":4,"ephemeral_5m_input_tokens":7}}`); err != nil {
		t.Fatal(err)
	}

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(0))}, events.AppendOptions{})
	// Every row stamped: an unstamped one would take the clock's now, and the
	// batch's stamps may not run backwards past it.
	got := mustAppend(t, log, sid, []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: text("done"), ProcessedAt: at(6 * time.Second)},
		{Type: domain.EventSessionThreadStatusIdle, ProcessedAt: at(6242 * time.Millisecond),
			Payload: json.RawMessage(`{"stop_reason":{"type":"end_turn"}}`)},
		idleAt(at(6242 * time.Millisecond)),
	}, events.AppendOptions{AddUsage: &domain.ModelUsage{
		InputTokens: 20, OutputTokens: 3, CacheReadInputTokens: 1, CacheCreationInputTokens: 2}})

	want := []domain.EventType{domain.EventAgentMessage, domain.EventSessionThreadStatusIdle,
		domain.EventSessionUsage, domain.EventSessionStatusIdle}
	if !slices.Equal(types(got), want) {
		t.Fatalf("batch = %v, want %v", types(got), want)
	}
	usageEv, idle := got[2], got[3]
	if usageEv.ThreadID != "" {
		t.Errorf("session.usage thread = %q, want the session's own row", usageEv.ThreadID)
	}
	if usageEv.ProcessedAt == nil || idle.ProcessedAt == nil || !usageEv.ProcessedAt.Equal(*idle.ProcessedAt) {
		t.Errorf("session.usage processed_at = %v, want the idle's %v", usageEv.ProcessedAt, idle.ProcessedAt)
	}
	body := snapshot(t, usageEv)
	if keys := slices.Sorted(maps.Keys(body)); !slices.Equal(keys, []string{"usage"}) {
		t.Errorf("session.usage keys = %v, want [usage] (no budget for a session without one)", keys)
	}
	var usage map[string]any
	if err := json.Unmarshal(mustMarshal(t, body["usage"]), &usage); err != nil {
		t.Fatal(err)
	}
	wantUsage := map[string]any{
		"input_tokens": 120.0, "output_tokens": 13.0, "cache_read_input_tokens": 6.0,
		"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 4.0, "ephemeral_5m_input_tokens": 9.0},
		"active_seconds": 6.242,
	}
	if string(mustMarshal(t, usage)) != string(mustMarshal(t, wantUsage)) {
		t.Errorf("session.usage usage = %s, want %s", mustMarshal(t, usage), mustMarshal(t, wantUsage))
	}

	// The persisted row is what a list reads back.
	listed, err := log.List(context.Background(), sid, events.ListQuery{Types: []string{string(domain.EventSessionUsage)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != usageEv.ID ||
		string(mustMarshal(t, snapshot(t, listed[0]))) != string(mustMarshal(t, body)) {
		t.Errorf("listed session.usage = %v, want the appended one", listed)
	}
	if active, since := storedActive(t, pool, sid); active != 6.242 || since != nil {
		t.Errorf("stored active_seconds = %v running_since = %v, want 6.242 and none open", active, since)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Only the session's own idle gets a snapshot: a thread's idle, the session's
// other status events and an idle-free batch get none.
func TestUsageSnapshotOnlyBeforeASessionIdle(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)

	got := mustAppend(t, log, sid, []events.NewEvent{
		status(domain.EventSessionStatusRunning, nil),
		{Type: domain.EventSessionThreadStatusIdle, Payload: json.RawMessage(`{"stop_reason":{"type":"end_turn"}}`)},
		status(domain.EventSessionStatusRescheduled, nil),
		{Type: domain.EventSessionError, Payload: json.RawMessage(`{"error":{"type":"unknown_error"}}`)},
		status(domain.EventSessionStatusTerminated, nil),
	}, events.AppendOptions{})
	if slices.Contains(types(got), domain.EventSessionUsage) {
		t.Errorf("batch without a session idle = %v, want no session.usage", types(got))
	}
}

// active_seconds is the time the session spent running, counted from its own
// status events: running opens an interval, and idle, rescheduled or
// terminated closes it. A running inside an open interval restarts it, an
// idle with nothing open adds nothing, and an interval two hosts' clocks make
// negative adds zero.
func TestUsageActiveSecondsCountsRunningIntervals(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	s := time.Second

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(0))}, events.AppendOptions{})
	got := mustAppend(t, log, sid, []events.NewEvent{idleAt(at(2 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 2 {
		t.Errorf("first idle active_seconds = %v, want 2", v)
	}

	mustAppend(t, log, sid, []events.NewEvent{
		status(domain.EventSessionStatusRunning, at(10*s)),
		status(domain.EventSessionStatusRescheduled, at(11*s)),
	}, events.AppendOptions{})
	mustAppend(t, log, sid, []events.NewEvent{
		status(domain.EventSessionStatusRunning, at(12*s)),
		status(domain.EventSessionStatusRunning, at(12*s+500*time.Millisecond)),
	}, events.AppendOptions{})
	got = mustAppend(t, log, sid, []events.NewEvent{idleAt(at(15 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 5.5 {
		t.Errorf("second idle active_seconds = %v, want 5.5 (2 + 1 rescheduled + 2.5 after the restart)", v)
	}

	got = mustAppend(t, log, sid, []events.NewEvent{idleAt(at(20 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 5.5 {
		t.Errorf("re-idle active_seconds = %v, want 5.5 (no interval open)", v)
	}

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(30*s))}, events.AppendOptions{})
	got = mustAppend(t, log, sid, []events.NewEvent{idleAt(at(29 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 5.5 {
		t.Errorf("skewed idle active_seconds = %v, want 5.5 (a negative interval adds zero)", v)
	}

	// One batch that runs and idles, to the millisecond.
	got = mustAppend(t, log, sid, []events.NewEvent{
		status(domain.EventSessionStatusRunning, at(40*s)),
		idleAt(at(41*s + 234567*time.Microsecond)),
	}, events.AppendOptions{})
	if v := activeSeconds(t, got[1]); v != 6.735 {
		t.Errorf("one-batch idle active_seconds = %v, want 6.735", v)
	}

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(50*s))}, events.AppendOptions{})
	active, since := storedActive(t, pool, sid)
	if active < 6.734 || active > 6.735 || since == nil || !since.Equal(*at(50 * s)) {
		t.Errorf("stored = %v since %v, want 6.734567 and an interval open at %v", active, since, *at(50 * s))
	}
}

// legacyStatus writes a session-level status event the way a binary from
// before migration 0055 does: into the log, leaving the clock's columns as
// they were.
func legacyStatus(t *testing.T, pool *pgxpool.Pool, sid domain.ID, typ domain.EventType, stamp *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO events (id, session_id, seq, type, payload, processed_at)
		 SELECT $1, $2, COALESCE(MAX(seq), 0) + 1, $3, '{}', $4 FROM events WHERE session_id = $2`,
		domain.NewID(domain.PrefixEvent).String(), sid.String(), string(typ), *stamp); err != nil {
		t.Fatalf("legacy %s: %v", typ, err)
	}
}

// A status event the clock's columns never saw is reconciled at the next
// status event AppendInTx writes, from the log's last one: a close counts at
// its own stamp, so an idle after it does not count the gap, and an open
// opens at its own. An unseen close and reopen restarts the clock at the
// reopen, which loses the time before that close and no more.
func TestUsageClockReconcilesWithTheLog(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	s := time.Second

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(0))}, events.AppendOptions{})
	legacyStatus(t, pool, sid, domain.EventSessionStatusIdle, at(3*s))
	got := mustAppend(t, log, sid, []events.NewEvent{idleAt(at(100 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 3 {
		t.Errorf("re-idle after an unseen close: active_seconds = %v, want 3, not the 100 s to the re-idle", v)
	}

	legacyStatus(t, pool, sid, domain.EventSessionStatusRunning, at(200*s))
	got = mustAppend(t, log, sid, []events.NewEvent{idleAt(at(204 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 7 {
		t.Errorf("idle after an unseen open: active_seconds = %v, want 7 (3 + 4)", v)
	}

	mustAppend(t, log, sid, []events.NewEvent{status(domain.EventSessionStatusRunning, at(300*s))}, events.AppendOptions{})
	legacyStatus(t, pool, sid, domain.EventSessionStatusIdle, at(301*s))
	legacyStatus(t, pool, sid, domain.EventSessionStatusRunning, at(310*s))
	got = mustAppend(t, log, sid, []events.NewEvent{idleAt(at(315 * s))}, events.AppendOptions{})
	if v := activeSeconds(t, got[0]); v != 12 {
		t.Errorf("idle after an unseen close and reopen: active_seconds = %v, want 12 (7 + 5)", v)
	}
	if active, since := storedActive(t, pool, sid); active != 12 || since != nil {
		t.Errorf("stored = %v since %v, want 12 and nothing open", active, since)
	}
}

// A turn that suspends emits its idle from AppendOptions.Then, inside the
// same transaction: the nested append's snapshot sees the outer append's
// usage and the interval it opened.
func TestUsageSnapshotInANestedAppend(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)

	var nested []domain.Event
	mustAppend(t, log, sid, []events.NewEvent{
		status(domain.EventSessionStatusRunning, at(0)),
		{Type: domain.EventAgentMessage, Payload: text("calling")},
	}, events.AppendOptions{
		AddUsage: &domain.ModelUsage{InputTokens: 9, OutputTokens: 4},
		Then: func(ctx context.Context, tx pgx.Tx) error {
			var err error
			nested, err = log.AppendInTx(ctx, tx, sid, []events.NewEvent{idleAt(at(3 * time.Second))}, events.AppendOptions{})
			return err
		},
	})
	if len(nested) != 2 {
		t.Fatalf("nested batch = %v, want session.usage then session.status_idle", types(nested))
	}
	usage, _ := snapshot(t, nested[0])["usage"].(map[string]any)
	if usage["input_tokens"] != 9.0 || usage["output_tokens"] != 4.0 || usage["active_seconds"] != 3.0 {
		t.Errorf("nested snapshot usage = %v, want 9 in, 4 out, 3 s", usage)
	}
}
