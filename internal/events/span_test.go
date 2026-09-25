package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// The span.* wire events and the OTel span must come from the same
// instrumentation point (CLAUDE.md principle 3): one StartModelRequest/End
// pair yields exactly one exported OTel span AND the start/end event pair,
// linked by model_request_start_id.
func TestModelRequestSameSourceEmission(t *testing.T) {
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newSession(t, pool)
	ctx := context.Background()

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(ctx) }()

	// Route the helper through the recording provider via the global, as
	// production wiring does.
	restore := swapTracerProvider(tp)
	defer restore()

	spanCtx, mr, err := log.StartModelRequest(ctx, sid, events.Backend{Provider: "anthropic", Model: "claude-x"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !oteltrace.SpanContextFromContext(spanCtx).IsValid() {
		t.Error("returned context carries no span")
	}
	// The end is two-phase: the wire event is rendered for the caller to
	// commit (the brain appends it atomically with the turn's settlement),
	// then Finish closes the OTel side with the commit's fate.
	speed := "standard"
	endEv, err := mr.EndEvent(true, domain.ModelUsage{
		InputTokens: 100, OutputTokens: 25, CacheReadInputTokens: 7, Speed: &speed,
	})
	if err != nil {
		t.Fatalf("end event: %v", err)
	}
	if _, err := log.Append(spanCtx, sid, []events.NewEvent{endEv}); err != nil {
		t.Fatalf("append end event: %v", err)
	}
	mr.Finish(ctx, true, nil)

	// Exactly one OTel span left the process.
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "model_request" {
		t.Errorf("span name = %q", spans[0].Name())
	}

	// And exactly the two wire events landed in the log, linked.
	list, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("log has %d events, want start+end", len(list))
	}
	start, end := list[0], list[1]
	if start.Type != domain.EventSpanModelRequestStart || end.Type != domain.EventSpanModelRequestEnd {
		t.Fatalf("event types = %s, %s", start.Type, end.Type)
	}
	if start.ProcessedAt == nil || end.ProcessedAt == nil {
		t.Error("span events must carry processed_at")
	}
	if mr.StartEventID() != start.ID {
		t.Errorf("StartEventID = %s, want %s", mr.StartEventID(), start.ID)
	}

	var payload struct {
		IsError             bool   `json:"is_error"`
		ModelRequestStartID string `json:"model_request_start_id"`
		ModelUsage          struct {
			InputTokens              int64   `json:"input_tokens"`
			OutputTokens             int64   `json:"output_tokens"`
			CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
			Speed                    *string `json:"speed"`
		} `json:"model_usage"`
	}
	if err := json.Unmarshal(end.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.IsError {
		t.Error("is_error not recorded")
	}
	if payload.ModelRequestStartID != start.ID.String() {
		t.Errorf("model_request_start_id = %q, want %q", payload.ModelRequestStartID, start.ID)
	}
	if payload.ModelUsage.InputTokens != 100 || payload.ModelUsage.OutputTokens != 25 ||
		payload.ModelUsage.CacheReadInputTokens != 7 || payload.ModelUsage.CacheCreationInputTokens != 0 {
		t.Errorf("model_usage = %+v", payload.ModelUsage)
	}
	if payload.ModelUsage.Speed == nil || *payload.ModelUsage.Speed != "standard" {
		t.Errorf("speed = %v", payload.ModelUsage.Speed)
	}

	// Failure path: a start against a dead session emits no span leak.
	if _, _, err := log.StartModelRequest(ctx, domain.NewID("sesn"), events.Backend{Provider: "anthropic", Model: "claude-x"}); err == nil {
		t.Error("start on unknown session should fail")
	}
}

// An input is processed when the request that consumes it starts, as the
// reference stamps it: 1 µs before that request's span.model_request_start
// (#793; every recorded consumption but one). The start's own commit stamps
// the thread's inputs below it that no earlier start stamped — and nothing
// else: not a row already stamped, not another thread's, not a row at or
// after the start.
func TestModelRequestStartStampsWhatItConsumes(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	child := domain.NewID(domain.PrefixSessionThread)
	earlier := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	appended, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("queued")},
		{Type: domain.EventUserMessage, Payload: text("already"), ProcessedAt: &earlier},
		{Type: domain.EventAgentThreadMessageReceived, Payload: text("report")},
		{Type: domain.EventUserMessage, ThreadID: child, Payload: text("the child's")},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, mr, err := log.StartModelRequestOn(ctx, sid, "", events.Backend{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("after")}}); err != nil {
		t.Fatal(err)
	}
	all, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[domain.ID]domain.Event{}
	var start domain.Event
	for _, ev := range all {
		byID[ev.ID] = ev
		if ev.Type == domain.EventSpanModelRequestStart {
			start = ev
		}
	}
	if start.Seq != mr.StartSeq() || start.ID != mr.StartEventID() {
		t.Fatalf("start row %s at %d, ModelRequest says %s at %d", start.ID, start.Seq, mr.StartEventID(), mr.StartSeq())
	}
	if start.ProcessedAt == nil {
		t.Fatal("the start carries no processed_at")
	}
	consumed := start.ProcessedAt.Add(-time.Microsecond)
	for i, want := range []*time.Time{&consumed, &earlier, &consumed, nil} {
		got := byID[appended[i].ID].ProcessedAt
		if (got == nil) != (want == nil) || (got != nil && !got.Equal(*want)) {
			t.Errorf("row %d processed_at = %v, want %v", i, got, want)
		}
	}
	for _, ev := range all {
		if ev.Seq > start.Seq && ev.ProcessedAt != nil {
			t.Errorf("row after the start (%s) stamped %v", ev.Type, ev.ProcessedAt)
		}
	}
}

// A start stamps only what its request reads: a user.message, a
// user.define_outcome, a system.message and a delivered
// agent.thread_message_received. An answer is its thread's ordered
// processor's to stamp, and a held one must stay null until that processor
// reaches it; an interrupt is no input a request reads, so a later request
// stamping it would date it to a request that never saw it (#793). The
// no-provider failure's stamp, MarkProcessedThrough, keeps the same rule.
func TestModelRequestStartStampsOnlyWhatARequestReads(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	appended, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("message")},
		{Type: domain.EventUserDefineOutcome, Payload: json.RawMessage(`{"description":"d"}`)},
		{Type: domain.EventSystemMessage, Payload: text("system")},
		{Type: domain.EventAgentThreadMessageReceived, Payload: text("report")},
		{Type: domain.EventUserInterrupt, Payload: json.RawMessage(`{}`)},
		{Type: domain.EventUserToolConfirm, Payload: json.RawMessage(`{"tool_use_id":"sevt_x","result":"allow"}`)},
		{Type: domain.EventUserToolResult, Payload: json.RawMessage(`{"tool_use_id":"sevt_x"}`)},
		{Type: domain.EventUserCustomToolRes, Payload: json.RawMessage(`{"custom_tool_use_id":"sevt_y"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	const read = 4 // the first four rows are what a request reads
	check := func(when string) {
		t.Helper()
		all, err := log.List(ctx, sid, events.ListQuery{})
		if err != nil {
			t.Fatal(err)
		}
		byID := map[domain.ID]domain.Event{}
		for _, ev := range all {
			byID[ev.ID] = ev
		}
		for i, a := range appended {
			got := byID[a.ID].ProcessedAt
			if stamped := got != nil; stamped != (i < read) {
				t.Errorf("%s: %s processed_at = %v, want stamped %v", when, a.Type, got, i < read)
			}
		}
	}
	if _, _, err := log.StartModelRequestOn(ctx, sid, "", events.Backend{}, nil); err != nil {
		t.Fatal(err)
	}
	check("after the start")
	if _, err := log.AppendWith(ctx, sid, nil, events.AppendOptions{
		MarkProcessedThrough: appended[len(appended)-1].Seq,
	}); err != nil {
		t.Fatal(err)
	}
	check("after MarkProcessedThrough")
}

// The stamp is taken after the session row lock, on the clock of the process
// writing the start — the brain's, which also stamps the request's reply and
// end — so an input that commits while the start waits on the lock is never
// dated before the start was free to run (#793). Everything asserted here is
// on this process's clock: created_at is the database's, and comparing across
// the two would measure their skew, not the order.
func TestModelRequestStartStampsAfterTheLock(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, sid.String()); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		_, _, err := log.StartModelRequestOn(ctx, sid, "", events.Backend{}, nil)
		started <- err
	}()
	// The start is blocked on the lock this transaction holds.
	for deadline := time.Now().Add(10 * time.Second); ; {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the start never waited on the session lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Long enough that a stamp taken when the start began, before the lock,
	// could not pass for one taken after it.
	time.Sleep(50 * time.Millisecond)
	if _, err := log.AppendInTx(ctx, tx, sid, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: text("raced")},
	}, events.AppendOptions{}); err != nil {
		t.Fatal(err)
	}
	released := time.Now()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	returned := time.Now()

	all, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Type != domain.EventUserMessage || all[1].Type != domain.EventSpanModelRequestStart {
		t.Fatalf("log = %v, want the message, then the start", all)
	}
	msg, start := all[0], all[1]
	if msg.ProcessedAt == nil || start.ProcessedAt == nil {
		t.Fatalf("message %v, start %v: want both stamped", msg.ProcessedAt, start.ProcessedAt)
	}
	if start.ProcessedAt.Before(released.Truncate(time.Microsecond)) || start.ProcessedAt.After(returned) {
		t.Errorf("start processed_at %v is outside [%v, %v], from the lock's release to the start's return",
			start.ProcessedAt, released, returned)
	}
	if want := start.ProcessedAt.Add(-time.Microsecond); !msg.ProcessedAt.Equal(want) {
		t.Errorf("message processed_at = %v, want %v", msg.ProcessedAt, want)
	}
}

// The start carries the claimant's lease proof in its own commit: a brain
// that lost its item must not tell a client its inputs were consumed, and a
// refused start writes neither the start nor a stamp.
func TestModelRequestStartThenFailingWritesNothing(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	log := events.NewLog(pool)
	sid := newThreadedSession(t, pool)
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: text("queued")}}); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("lease lost")
	_, _, err := log.StartModelRequestOn(ctx, sid, "", events.Backend{}, func(context.Context, pgx.Tx) error { return lost })
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the Then's", err)
	}
	all, err := log.List(ctx, sid, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ProcessedAt != nil {
		t.Errorf("log = %v, want the queued message alone, unstamped", all)
	}
}
