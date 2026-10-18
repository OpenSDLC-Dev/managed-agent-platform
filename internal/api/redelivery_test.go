package api_test

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
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
	code, _, body := laneRead(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs}, headers))
	return code, strings.TrimSpace(body)
}

// eventList is the session's log as its events list renders it, ids and
// processed_at included.
func eventList(t *testing.T, s *tserver, sessionID string) []map[string]any {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sessionID+"/events?limit=1000", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d %v", status, res)
	}
	return listData(t, res)
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
	before, status, work := eventList(t, s, sessionID), s.sessionStatus(sessionID), workRows(t, s, sessionID)
	if code, body := sendRaw(t, s, headers, sessionID, evs...); code != http.StatusOK || body != `{"data":[]}` {
		t.Fatalf(`send = %d %s, want 200 {"data":[]}`, code, body)
	}
	if got := eventList(t, s, sessionID); !reflect.DeepEqual(got, before) {
		t.Errorf("a no-op send changed the log:\n%v\nwas\n%v", got, before)
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
// reading of it (docs/DIVERGENCES.md, INFERRED): the repeat is dropped, with a
// system.message that trails it, and the rest of the send is processed, a
// refusal still naming its event by the index the client sent.
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

	// A system.message may only follow a message or a result, so the one
	// trailing a repeat goes with it: the retried batch is a no-op rather than
	// a system.message on its own, which is refused when a send carries it
	// alone.
	t.Run("a system.message goes with the repeat it follows", func(t *testing.T) {
		s := newTestServer(t)
		sid := eventsFixture(t, s)
		callID := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse,
			`{"name":"record_marker","input":{},"session_thread_id":null}`)
		note := map[string]any{"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "note"}}}
		sendEvents(t, s, sid, customResult(callID), note)
		endTurn(t, s, sid)

		sendIsANoOp(t, s, map[string]string{"x-api-key": testKey}, sid, customResult(callID), note)
		if n := countEventType(t, s, sid, "system.message"); n != 1 {
			t.Errorf("system.message on the log = %d, want the first send's only", n)
		}
	})

	// The rest keeps the thread it addressed: an interrupt naming one child
	// stops that child, not the session, when a dropped repeat came first.
	t.Run("a thread-scoped interrupt stays scoped", func(t *testing.T) {
		s := newTestServer(t)
		sid := eventsFixture(t, s)
		primary := domain.PrimaryThreadID(domain.ID(sid)).String()
		setThread(t, s, primary, "running", "")
		child := insertChild(t, s, sid, "running")
		pgtest.SetSessionStatus(t, s.pool, domain.ID(sid), "running")
		callID := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, customCall)
		appendOn(t, s, sid, "", false, domain.EventUserCustomToolRes,
			`{"custom_tool_use_id":"`+callID+`","content":[{"type":"text","text":"done"}]}`)

		echo := sendEvents(t, s, sid, customResult(callID),
			map[string]any{"type": "user.interrupt", "session_thread_id": child})
		if len(echo) != 1 || echo[0]["type"] != "user.interrupt" {
			t.Fatalf("echo = %v, want the interrupt alone", echo)
		}
		if got := s.threadStatus(t, child); got != "idle" {
			t.Errorf("child = %q, want idle: the interrupt named it", got)
		}
		if got := s.threadStatus(t, primary); got != "running" {
			t.Errorf("primary = %q, want running: the interrupt named only the child", got)
		}
	})
}

// A repeat to a child's call is dropped like any other: the child it would
// have answered is not woken.
func TestARedeliveredResultToAChildsCall(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	child := insertChild(t, s, sid, "idle")
	callID := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	appendOn(t, s, sid, domain.ID(child), true, domain.EventUserCustomToolRes,
		`{"custom_tool_use_id":"`+callID+`","content":[{"type":"text","text":"done"}]}`)
	setThread(t, s, child, "idle", `{"type":"end_turn"}`)

	sendIsANoOp(t, s, map[string]string{"x-api-key": testKey}, sid, customResult(callID))
	if got := s.threadStatus(t, child); got != "idle" {
		t.Errorf("child = %q, want idle", got)
	}
}

// Two first answers racing for one call: the session row lock orders them,
// so one lands and the other finds it there and is dropped. Both are 200, and
// the call has one answer.
func TestTwoConcurrentResultsForOneCall(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	callID := appendGatedToolUse(t, s, sid, domain.EventAgentCustomToolUse,
		`{"name":"record_marker","input":{},"session_thread_id":null}`)

	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
				map[string]any{"events": []any{customResult(callID)}})
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("concurrent result = %d, want 200", code)
		}
	}
	if n := countEventType(t, s, sid, "user.custom_tool_result"); n != 1 {
		t.Errorf("results on the log = %d, want 1", n)
	}
	if n := s.liveWork(sid, queue.ModelTurn); n != 1 {
		t.Errorf("live model_turn items = %d, want the one resume", n)
	}
}
