package api_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// A result for a call that already has one is the reference's no-op (#58):
// the recorded sends answered 200 `{"data":[]}` and appended nothing, for a
// byte-identical re-delivery and for a changed result under the same id
// alike, while the session ran and once it was idle again
// (managed-agents-wire-recordings 2026-10-18-duplicate-tool-result, setup.json
// idx 5–6 and 11–15).

// sendRaw posts a batch under headers and returns the status and the body as
// sent, so an empty data array is told apart from a missing or null one.
func sendRaw(t *testing.T, s *tserver, headers map[string]string, sessionID string, evs ...map[string]any) (int, string) {
	t.Helper()
	res := s.doRaw(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs}, headers)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return res.StatusCode, strings.TrimSpace(string(raw))
}

// workRows counts every work item the session has ever had, in any state: a
// no-op enqueues nothing, live or not.
func workRows(t *testing.T, s *tserver, sessionID string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count work: %v", err)
	}
	return n
}

// sendIsANoOp posts a batch, asserts the recorded answer, and that the send
// left the log, the status and the work queue as they were.
func sendIsANoOp(t *testing.T, s *tserver, headers map[string]string, sessionID string, evs ...map[string]any) {
	t.Helper()
	before, status, work := s.eventTypes(sessionID), s.sessionStatus(sessionID), workRows(t, s, sessionID)
	if code, body := sendRaw(t, s, headers, sessionID, evs...); code != http.StatusOK || body != `{"data":[]}` {
		t.Fatalf(`send = %d %s, want 200 {"data":[]}`, code, body)
	}
	if got := s.eventTypes(sessionID); !slices.Equal(got, before) {
		t.Errorf("a no-op send changed the log: %v, was %v", got, before)
	}
	if got := s.sessionStatus(sessionID); got != status {
		t.Errorf("a no-op send moved the status to %q from %q", got, status)
	}
	if n := workRows(t, s, sessionID); n != work {
		t.Errorf("a no-op send added %d work items", n-work)
	}
}

// resultTexts lists the text of every result of typ on the session's log.
func resultTexts(t *testing.T, s *tserver, sessionID, typ string) []string {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d %v", status, res)
	}
	var texts []string
	for _, ev := range listData(t, res) {
		if ev["type"] != typ {
			continue
		}
		blocks, _ := ev["content"].([]any)
		for _, b := range blocks {
			if m, _ := b.(map[string]any); m != nil {
				texts = append(texts, m["text"].(string))
			}
		}
	}
	return texts
}

// endTurn finishes the turn a result resumed and rests the session idle, so
// that what a later send does to it is its own doing.
func endTurn(t *testing.T, s *tserver, sessionID string) {
	t.Helper()
	ctx := context.Background()
	q := queue.New(s.pool)
	item, err := q.Claim(ctx, queue.ModelTurn, time.Minute)
	if err != nil || item == nil {
		t.Fatalf("claim the resumed turn: %+v %v", item, err)
	}
	if err := q.Complete(ctx, s.pool, item); err != nil {
		t.Fatal(err)
	}
	pgtest.SetSessionStatus(t, s.pool, domain.ID(sessionID), "idle")
}

func TestARedeliveredToolResultIsANoOp(t *testing.T) {
	for _, lane := range []struct {
		name    string
		session func(t *testing.T, s *tserver) (string, map[string]string)
		use     domain.EventType
		call    string
		typ     string
		key     string
	}{
		// The recording's custom lane posted under a Console session; a
		// management key is this platform's equivalent.
		{"custom", func(t *testing.T, s *tserver) (string, map[string]string) {
			return eventsFixture(t, s), map[string]string{"x-api-key": testKey}
		}, domain.EventAgentCustomToolUse, `{"name":"record_marker","input":{},"session_thread_id":null}`,
			"user.custom_tool_result", "custom_tool_use_id"},
		// The builtin lane posted under an environment Bearer, a worker's.
		{"builtin", func(t *testing.T, s *tserver) (string, map[string]string) {
			sid := selfHostedSession(t, s)
			return sid, workerAuth(t, s, sid)
		}, domain.EventAgentToolUse, `{"name":"bash","input":{},"evaluated_permission":"allow","session_thread_id":null}`,
			"user.tool_result", "tool_use_id"},
	} {
		t.Run(lane.name, func(t *testing.T) {
			s := newTestServer(t)
			sid, auth := lane.session(t, s)
			callID := appendGatedToolUse(t, s, sid, lane.use, lane.call)
			result := func(text string) map[string]any {
				return map[string]any{"type": lane.typ, lane.key: callID, "is_error": false,
					"content": []any{map[string]any{"type": "text", "text": text}}}
			}

			echo := sendEventsAs(t, s, auth, sid, result("first"))
			if len(echo) != 1 || echo[0]["type"] != lane.typ {
				t.Fatalf("first result echo = %v, want the one result", echo)
			}
			// While the turn it resumed is under way: an identical repeat, then
			// a changed one.
			sendIsANoOp(t, s, auth, sid, result("first"))
			sendIsANoOp(t, s, auth, sid, result("changed"))
			// Once that turn is over: a repeat must not wake the session.
			endTurn(t, s, sid)
			sendIsANoOp(t, s, auth, sid, result("first"))

			if got := resultTexts(t, s, sid, lane.typ); !slices.Equal(got, []string{"first"}) {
				t.Errorf("results on the log = %v, want only the first", got)
			}

			// An archived session refuses before any result is read, the
			// repeat included.
			if st, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); st != http.StatusOK {
				t.Fatalf("archive: %d", st)
			}
			if code, body := sendRaw(t, s, auth, sid, result("first")); code != http.StatusBadRequest {
				t.Errorf("repeat on an archived session = %d %s, want 400", code, body)
			}
		})
	}
}

// TestARedeliveredResultBesideOtherEvents: the recording sent each result
// alone, so a batch that carries one beside other events is this platform's
// reading of it (docs/DIVERGENCES.md, INFERRED): the repeat is dropped and the
// rest of the send is processed as if it had come alone.
func TestARedeliveredResultBesideOtherEvents(t *testing.T) {
	t.Run("the rest of the send is processed", func(t *testing.T) {
		s := newTestServer(t)
		sid := eventsFixture(t, s)
		callID := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse,
			`{"name":"record_marker","input":{},"session_thread_id":null}`)
		sendEvents(t, s, sid, customResult(callID))
		endTurn(t, s, sid)

		echo := sendEvents(t, s, sid, customResult(callID), userMessage("next"))
		if len(echo) != 1 || echo[0]["type"] != "user.message" {
			t.Fatalf("echo = %v, want the message alone", echo)
		}
		if n := countEventType(t, s, sid, "user.custom_tool_result"); n != 1 {
			t.Errorf("results on the log = %d, want 1", n)
		}
		if got := s.sessionStatus(sid); got != "running" {
			t.Errorf("status = %q, want running: the message wakes the session", got)
		}
		if n := s.liveWork(sid, queue.ModelTurn); n != 1 {
			t.Errorf("live model_turn items = %d, want 1", n)
		}
	})

	// A refusal raised after the drop names the event by its place in the
	// batch as sent, not in what was left of it.
	t.Run("a refusal keeps the client's index", func(t *testing.T) {
		s := newTestServer(t)
		sid := eventsFixture(t, s)
		answered := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"a","input":{},"session_thread_id":null}`)
		awaited := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse, `{"name":"b","input":{},"session_thread_id":null}`)
		sendEvents(t, s, sid, customResult(answered))

		sendRefusedWhileAwaiting(t, s, sid, whileAwaiting("user.message", 1, awaited),
			customResult(answered), userMessage("hello"))
	})
}
