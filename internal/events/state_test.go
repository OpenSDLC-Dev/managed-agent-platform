package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func sessionStatus(t *testing.T, pool *pgxpool.Pool, id domain.ID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM sessions WHERE id = $1`, id.String()).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func TestAppendWithFlipsStatusAtomically(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	log := events.NewLog(pool)
	sessionID := newSession(t, pool)

	if got := sessionStatus(t, pool, sessionID); got != "idle" {
		t.Fatalf("fixture status = %q", got)
	}

	running := domain.SessionRunning
	out, err := log.AppendWith(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{SetStatus: &running})
	if err != nil {
		t.Fatalf("AppendWith: %v", err)
	}
	if len(out) != 1 || out[0].Type != domain.EventSessionStatusRunning {
		t.Fatalf("appended = %+v", out)
	}
	if got := sessionStatus(t, pool, sessionID); got != "running" {
		t.Errorf("status after flip = %q, want running", got)
	}
}

func TestAppendWithThenErrorRollsEverythingBack(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	log := events.NewLog(pool)
	sessionID := newSession(t, pool)

	running := domain.SessionRunning
	boom := errors.New("enqueue failed")
	_, err := log.AppendWith(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventSessionStatusRunning},
	}, events.AppendOptions{
		SetStatus: &running,
		Then: func(ctx context.Context, tx pgx.Tx) error {
			return boom
		},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("AppendWith error = %v, want the Then error", err)
	}
	if got := sessionStatus(t, pool, sessionID); got != "idle" {
		t.Errorf("status leaked out of a rolled-back append: %q", got)
	}
	evs, err := log.List(ctx, sessionID, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("events leaked out of a rolled-back append: %+v", evs)
	}
}

func TestAppendWithThenSeesTheAppend(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	log := events.NewLog(pool)
	sessionID := newSession(t, pool)

	var seen int
	_, err := log.AppendWith(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: json.RawMessage(`{"content":[]}`)},
	}, events.AppendOptions{
		Then: func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM events WHERE session_id = $1`, sessionID.String()).Scan(&seen)
		},
	})
	if err != nil {
		t.Fatalf("AppendWith: %v", err)
	}
	if seen != 1 {
		t.Errorf("Then saw %d events, want 1 (same transaction)", seen)
	}
}

func TestAppendWithAddUsageAccumulates(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	log := events.NewLog(pool)
	sessionID := newSession(t, pool)

	for range 2 {
		_, err := log.AppendWith(ctx, sessionID, []events.NewEvent{
			{Type: domain.EventSpanModelRequestStart},
		}, events.AppendOptions{
			AddUsage: &domain.ModelUsage{
				InputTokens: 100, OutputTokens: 7,
				CacheCreationInputTokens: 30, CacheReadInputTokens: 50,
			},
		})
		if err != nil {
			t.Fatalf("AppendWith: %v", err)
		}
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT usage FROM sessions WHERE id = $1`, sessionID.String()).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var usage domain.Usage
	if err := json.Unmarshal(raw, &usage); err != nil {
		t.Fatalf("stored usage %s: %v", raw, err)
	}
	want := domain.Usage{
		InputTokens: 200, OutputTokens: 14, CacheReadInputTokens: 100,
		CacheCreation: domain.CacheCreation{Ephemeral5m: 60},
	}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

func TestAppendWithMarkProcessedStampsOnlyConsumedInbound(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	log := events.NewLog(pool)
	sessionID := newSession(t, pool)

	first, err := log.Append(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: json.RawMessage(`{"content":[{"type":"text","text":"one"}]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ProcessedAt != nil {
		t.Fatal("inbound event stamped at append time")
	}
	second, err := log.Append(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventUserMessage, Payload: json.RawMessage(`{"content":[{"type":"text","text":"two"}]}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = log.AppendWith(ctx, sessionID, []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: json.RawMessage(`{"content":[]}`)},
	}, events.AppendOptions{MarkProcessedThrough: first[0].Seq})
	if err != nil {
		t.Fatalf("AppendWith: %v", err)
	}

	evs, err := log.List(ctx, sessionID, events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[domain.ID]domain.Event{}
	for _, ev := range evs {
		byID[ev.ID] = ev
	}
	if byID[first[0].ID].ProcessedAt == nil {
		t.Error("consumed inbound event not stamped")
	}
	if byID[second[0].ID].ProcessedAt != nil {
		t.Error("event past the watermark was stamped")
	}
}
