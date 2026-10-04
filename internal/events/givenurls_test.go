package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// An append indexes the URLs its events give web_fetch in its own
// transaction (#836): the session's index moves to the append's last seq on
// every append, and gains rows when an event gives a URL.
func TestAppendIndexesTheURLsItsEventsGive(t *testing.T) {
	pool := pgtest.NewPool(t)
	sid, _ := pgtest.NewSession(t, pool, "cloud")
	log := events.NewLog(pool)
	ctx := context.Background()
	state := func() (through, rows int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT i.indexed_through,
			(SELECT count(*) FROM session_given_urls g WHERE g.index_id = i.id)
			FROM session_given_url_indexes i WHERE i.session_id = $1`, sid.String()).Scan(&through, &rows); err != nil {
			t.Fatal(err)
		}
		return through, rows
	}
	msg, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": "read https://example.com/a."}}})
	if _, err := log.Append(ctx, sid, []events.NewEvent{{Type: domain.EventUserMessage, Payload: msg}}); err != nil {
		t.Fatal(err)
	}
	if through, rows := state(); through != 1 || rows != 2 {
		t.Errorf("after a message: indexed through %d with %d rows, want 1 and its 2 readings", through, rows)
	}
	thought, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": "https://example.com/b"}}})
	if _, err := log.Append(ctx, sid, []events.NewEvent{
		{Type: domain.EventAgentMessage, Payload: thought}, {Type: domain.EventAgentMessage, Payload: thought}}); err != nil {
		t.Fatal(err)
	}
	if through, rows := state(); through != 3 || rows != 2 {
		t.Errorf("after the agent's messages: indexed through %d with %d rows, want 3 and still 2", through, rows)
	}
}
