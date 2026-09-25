package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// storedInterrupts reads the processed_at of every user.interrupt on the log,
// in list order.
func storedInterrupts(t *testing.T, s *tserver, sid string) []*time.Time {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT processed_at FROM events WHERE session_id = $1 AND type = 'user.interrupt' ORDER BY seq`, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*time.Time
	for rows.Next() {
		var at *time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		out = append(out, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// An interrupt is consumed on receipt, whatever it reaches, and the reference
// stamps it there: after the results it synthesizes, before the idle pair it
// causes, with no request between (2026-09-02/batch2.json
// `sessT.events.after-outcome` idx 35-37). No request reads an interrupt, so
// no request's start will ever stamp one (#793): its own send stamps it, in
// the commit whose list it is laid out in, and the commit's stamps still run
// forward through that list.
func TestEveryInterruptIsStampedAtReceipt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope func(sid, child string) map[string]any
	}{
		{"session-wide", func(string, string) map[string]any { return map[string]any{"type": "user.interrupt"} }},
		{"primary-scoped", func(sid, _ string) map[string]any {
			return map[string]any{"type": "user.interrupt",
				"session_thread_id": domain.PrimaryThreadID(domain.ID(sid)).String()}
		}},
		{"child-scoped", func(_, child string) map[string]any {
			return map[string]any{"type": "user.interrupt", "session_thread_id": child}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "running", "")
			appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
			child := insertChild(t, s, sid, "running")
			appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, allowBashCall)
			runningSession(t, s, sid)
			seq := lastSeq(t, s, sid)

			echo := sendEvents(t, s, sid, tc.scope(sid, child))

			if echo[0]["processed_at"] == nil {
				t.Errorf("interrupt echoed unprocessed: %v", echo[0])
			}
			if got := storedInterrupts(t, s, sid); len(got) != 1 || got[0] == nil {
				t.Errorf("stored interrupts = %v, want one, stamped", got)
			}
			stampsRunForward(t, s, sid, seq)
		})
	}
}

// An interrupt with nothing to stop still reached the session and was
// consumed there: it writes nothing else, and is stamped all the same.
func TestAnInterruptWithNothingToStopIsStamped(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	echo := sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

	if echo[0]["processed_at"] == nil {
		t.Errorf("interrupt echoed unprocessed: %v", echo[0])
	}
	if got := storedInterrupts(t, s, sid); len(got) != 1 || got[0] == nil {
		t.Errorf("stored interrupts = %v, want one, stamped", got)
	}
}

// The dream runner's interrupt of the session it owns is written as a
// client's session-wide one is, stamp included.
func TestTheDreamRunnersInterruptIsStampedAtReceipt(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "running", "")
	appendOn(t, s, sid, "", false, domain.EventAgentToolUse, allowBashCall)
	runningSession(t, s, sid)
	seq := lastSeq(t, s, sid)

	if err := api.InterruptSessionForTest(context.Background(), s.pool, sid); err != nil {
		t.Fatal(err)
	}

	if got := storedInterrupts(t, s, sid); len(got) != 1 || got[0] == nil {
		t.Errorf("stored interrupts = %v, want one, stamped", got)
	}
	stampsRunForward(t, s, sid, seq)
}

// storedAnswer reads one answer's processed_at, and how many results answer
// the call it names.
func storedAnswer(t *testing.T, s *tserver, sid, answerID, callID string) (*time.Time, int) {
	t.Helper()
	var at *time.Time
	var results int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT processed_at,
		        (SELECT count(*) FROM events r WHERE r.session_id = $1
		           AND r.type IN ('agent.tool_result','user.tool_result','user.custom_tool_result','agent.mcp_tool_result')
		           AND COALESCE(r.payload->>'tool_use_id', r.payload->>'custom_tool_use_id', r.payload->>'mcp_tool_use_id') = $3)
		   FROM events WHERE session_id = $1 AND id = $2`, sid, answerID, callID).Scan(&at, &results); err != nil {
		t.Fatal(err)
	}
	return at, results
}

// A turn made a custom call A, then an ask-gated call B. The client answered
// B while A still waited, so the ordered walk held B's answer unprocessed
// behind A. A later interrupt settles the thread, and nothing may be left
// unprocessed behind it: the ordered walk reaches only calls whose result is
// still unprocessed, so whatever the interrupt answers it answers for good,
// and an answer held for such a call is consumed in the interrupt's commit.
// The settlement of the turn after used to stamp it; since #793 nothing
// after the interrupt would. A held result is the other shape: its call is
// answered already, so the interrupt leaves it to the result, which the walk
// then reaches once A is answered.
func TestAnInterruptLeavesNoHeldAnswerUnprocessed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selfHosted bool
		second     string // the payload of call B
		answer     func(callB string) map[string]any
		byWorker   bool // the answer is the self_hosted worker's
	}{
		{"an allow held behind a custom call", false, askBashCall,
			func(b string) map[string]any { return confirm(b, "allow", nil) }, false},
		{"a denial held behind a custom call", false, askBashCall,
			func(b string) map[string]any { return confirm(b, "deny", nil) }, false},
		{"a custom result held behind a custom call", false, customCall, customResult, false},
		{"a worker's tool result held behind a custom call", true, allowBashCall,
			func(b string) map[string]any {
				return map[string]any{"type": "user.tool_result", "tool_use_id": b,
					"content": []any{map[string]any{"type": "text", "text": "ran"}}}
			}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			sid := eventsFixture(t, s)
			if tc.selfHosted {
				sid = selfHostedSession(t, s)
			}
			primary := domain.PrimaryThreadID(domain.ID(sid)).String()
			a := appendOn(t, s, sid, "", false, domain.EventAgentCustomToolUse, customCall)
			typ := domain.EventAgentToolUse
			if tc.second == customCall {
				typ = domain.EventAgentCustomToolUse
			}
			b := appendOn(t, s, sid, "", false, typ, tc.second)
			setThread(t, s, primary, "idle", `{"type":"requires_action","event_ids":["`+a+`","`+b+`"]}`)

			headers := map[string]string{"x-api-key": testKey}
			if tc.byWorker {
				headers = workerAuth(t, s, sid)
			}
			held := sendEventsAs(t, s, headers, sid, tc.answer(b))[0]["id"].(string)
			if at, _ := storedAnswer(t, s, sid, held, b); at != nil {
				t.Fatalf("the answer to B was processed at %v while A still waited", at)
			}

			sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

			at, results := storedAnswer(t, s, sid, held, b)
			if at == nil {
				t.Errorf("the answer to B is still unprocessed after the interrupt settled its thread")
			}
			if results != 1 {
				t.Errorf("B carries %d results, want 1", results)
			}
			if _, resultsA := storedAnswer(t, s, sid, a, a); resultsA != 1 {
				t.Errorf("A carries %d results, want the interrupt's one", resultsA)
			}
		})
	}
}

// Archiving a child closes its open calls the way an interrupt does, so an
// answer held behind one of them is consumed by the archive's commit too:
// the thread is ended, and no walk of it reaches the call again.
func TestArchivingAChildLeavesNoHeldAnswerUnprocessed(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	setThread(t, s, domain.PrimaryThreadID(domain.ID(sid)).String(), "idle", `{"type":"end_turn"}`)
	child := insertChild(t, s, sid, "idle")
	a := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentCustomToolUse, customCall)
	b := appendOn(t, s, sid, domain.ID(child), true, domain.EventAgentToolUse, askBashCall)
	setThread(t, s, child, "idle", `{"type":"requires_action","event_ids":["`+a+`","`+b+`"]}`)
	held := sendEvents(t, s, sid, confirm(b, "allow", nil))[0]["id"].(string)
	if at, _ := storedAnswer(t, s, sid, held, b); at != nil {
		t.Fatalf("the confirmation of B was processed at %v while A still waited", at)
	}

	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sid+"/threads/"+child+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d, body %v", status, body)
	}

	if at, results := storedAnswer(t, s, sid, held, b); at == nil || results != 1 {
		t.Errorf("confirmation of B processed at %v with %d results on B; want it stamped, B answered once", at, results)
	}
}
