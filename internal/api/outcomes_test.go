package api_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// --- user.define_outcome over POST /v1/sessions/{id}/events (plan 21 slice 2) ---

func defineOutcome(description string, extra map[string]any) map[string]any {
	ev := map[string]any{
		"type":        "user.define_outcome",
		"description": description,
		"rubric":      map[string]any{"type": "text", "content": "# Rubric\n- complete"},
	}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

func sessionOutcomes(t *testing.T, s *tserver, sid string) []map[string]any {
	t.Helper()
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if status != http.StatusOK {
		t.Fatalf("get session: status %d body %v", status, res)
	}
	raw, ok := res["outcome_evaluations"].([]any)
	if !ok {
		t.Fatalf("outcome_evaluations missing or not an array: %v", res["outcome_evaluations"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestDefineOutcomeEchoShape(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	echo := sendEvents(t, s, sid, defineOutcome("Build a DCF model", nil))
	if len(echo) != 1 {
		t.Fatalf("echoed %d events, want 1", len(echo))
	}
	ev := echo[0]
	// The persisted event's wire shape, field for field
	// (BetaManagedAgentsUserDefineOutcomeEvent, anthropic-sdk-go v1.66.0).
	wantExactKeys(t, ev, "id", "type", "description", "rubric", "max_iterations",
		"outcome_id", "processed_at")
	if !strings.HasPrefix(ev["id"].(string), "sevt_") {
		t.Errorf("id = %v, want sevt_ prefix", ev["id"])
	}
	if !strings.HasPrefix(ev["outcome_id"].(string), "outc_") {
		t.Errorf("outcome_id = %v, want outc_ prefix", ev["outcome_id"])
	}
	if ev["max_iterations"] != float64(3) {
		t.Errorf("max_iterations = %v, want default 3", ev["max_iterations"])
	}
	// This platform's recorded processed_at divergence: echoed null, stamped
	// when the consuming turn settles.
	if ev["processed_at"] != nil {
		t.Errorf("processed_at = %v, want null on echo", ev["processed_at"])
	}
	rubric := ev["rubric"].(map[string]any)
	wantExactKeys(t, rubric, "type", "content")

	// The session projection: one entry, born pending, completed_at null.
	outs := sessionOutcomes(t, s, sid)
	if len(outs) != 1 {
		t.Fatalf("outcome_evaluations has %d entries, want 1", len(outs))
	}
	entry := outs[0]
	wantExactKeys(t, entry, "type", "outcome_id", "description", "explanation",
		"iteration", "result", "completed_at")
	if entry["type"] != "outcome_evaluation" || entry["result"] != "pending" {
		t.Errorf("entry = %v, want type outcome_evaluation result pending", entry)
	}
	if entry["outcome_id"] != ev["outcome_id"] {
		t.Errorf("entry outcome_id %v != echoed %v", entry["outcome_id"], ev["outcome_id"])
	}
	if entry["completed_at"] != nil {
		t.Errorf("completed_at = %v, want null before a terminal result", entry["completed_at"])
	}

	// The define_outcome wakes the idle session: running + a queued turn.
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid, nil)
	if status != http.StatusOK || res["status"] != "running" {
		t.Errorf("session status = %v, want running (the agent begins work on receipt)", res["status"])
	}
	var queued int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1 AND kind = 'model_turn'`, sid).Scan(&queued); err != nil {
		t.Fatalf("count work items: %v", err)
	}
	if queued != 1 {
		t.Errorf("model_turn work items = %d, want 1", queued)
	}
}

func TestDefineOutcomeSingleActive(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	sendEvents(t, s, sid, defineOutcome("first", nil))

	// A second define_outcome while the first is non-terminal is the
	// documented one-at-a-time rejection (shape ours, INFERRED).
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{defineOutcome("second", nil)}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "is still pending") {
		t.Errorf("stored-entry message = %q, want the still-pending wording (the stored branch, not the batch one)", msg)
	}

	// Two in one batch, on a FRESH session so the batch rule itself is pinned
	// (not shadowed by the stored-entry rule the case above exercises).
	sid2 := eventsFixture(t, s)
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid2+"/events",
		map[string]any{"events": []any{defineOutcome("a", nil), defineOutcome("b", nil)}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "send the next user.define_outcome") {
		t.Errorf("batch-rule message = %q, want the chaining guidance (the batch branch, not the stored-entry one)", msg)
	}
}

func TestDefineOutcomeTextRubricCap(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{defineOutcome("d", map[string]any{
			"rubric": map[string]any{"type": "text", "content": strings.Repeat("x", 262145)},
		})}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// Exactly at the cap (rune-counted) is accepted.
	sendEvents(t, s, sid, defineOutcome("d", map[string]any{
		"rubric": map[string]any{"type": "text", "content": strings.Repeat("界", 262144)},
	}))
}

func TestOutcomeEvaluationsOnListEndpoint(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	sendEvents(t, s, sid, defineOutcome("listed", nil))

	status, res := s.do(http.MethodGet, "/v1/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("list sessions: %d", status)
	}
	for _, sess := range listData(t, res) {
		if sess["id"] != sid {
			continue
		}
		outs, ok := sess["outcome_evaluations"].([]any)
		if !ok || len(outs) != 1 {
			t.Fatalf("list rendering outcome_evaluations = %v, want the one pending entry", sess["outcome_evaluations"])
		}
		return
	}
	t.Fatalf("session %s not in list", sid)
}

func TestInterruptSettlesOutcomeAndAllowsChaining(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	first := sendEvents(t, s, sid, defineOutcome("first", nil))[0]

	// The interrupt marks the active outcome interrupted — "even if
	// evaluation hadn't started yet" — with an empty start id, and the same
	// batch may chain the next outcome (the documented pattern).
	echo := sendEvents(t, s, sid,
		map[string]any{"type": "user.interrupt"},
		defineOutcome("second", nil))
	if len(echo) != 2 {
		t.Fatalf("echoed %d events, want 2", len(echo))
	}

	outs := sessionOutcomes(t, s, sid)
	if len(outs) != 2 {
		t.Fatalf("outcome_evaluations has %d entries, want 2", len(outs))
	}
	if outs[0]["result"] != "interrupted" {
		t.Errorf("first outcome result = %v, want interrupted", outs[0]["result"])
	}
	if outs[0]["completed_at"] == nil {
		t.Errorf("interrupted outcome completed_at is null, want a timestamp")
	}
	if outs[1]["result"] != "pending" {
		t.Errorf("chained outcome result = %v, want pending", outs[1]["result"])
	}

	// The log carries the terminal end event, referencing the outcome with an
	// empty outcome_evaluation_start_id (no evaluation cycle had started).
	status, res := s.do(http.MethodGet,
		"/v1/sessions/"+sid+"/events?types[]=span.outcome_evaluation_end", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d %v", status, res)
	}
	ends := listData(t, res)
	if len(ends) != 1 {
		t.Fatalf("span.outcome_evaluation_end events = %d, want 1", len(ends))
	}
	end := ends[0]
	if end["outcome_id"] != first["outcome_id"] {
		t.Errorf("end outcome_id = %v, want %v", end["outcome_id"], first["outcome_id"])
	}
	if end["outcome_evaluation_start_id"] != "" {
		t.Errorf("outcome_evaluation_start_id = %v, want empty string", end["outcome_evaluation_start_id"])
	}
	if end["result"] != "interrupted" {
		t.Errorf("end result = %v, want interrupted", end["result"])
	}
	if _, ok := end["usage"].(map[string]any); !ok {
		t.Errorf("end usage missing: %v", end)
	}
}

func TestInterruptMidGradingReferencesStart(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	echo := sendEvents(t, s, sid, defineOutcome("graded", nil))
	outcomeID := echo[0]["outcome_id"].(string)

	// Put the session mid-grading exactly as the brain's settleEndTurn
	// commits it: the span.outcome_evaluation_start and the entry's flip to
	// evaluating, one transaction.
	log := events.NewLog(s.pool)
	startEv, err := events.NewOutcomeStartEvent(domain.ID(outcomeID), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.AppendWith(context.Background(), domain.ID(sid), []events.NewEvent{startEv},
		events.AppendOptions{
			MutateOutcomes: func(evals []domain.OutcomeEvaluation) ([]domain.OutcomeEvaluation, error) {
				for i := range evals {
					if evals[i].OutcomeID == domain.ID(outcomeID) {
						evals[i].Result = domain.OutcomeResultEvaluating
					}
				}
				return evals, nil
			},
		}); err != nil {
		t.Fatalf("stage mid-grading state: %v", err)
	}

	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})

	// The interrupt's terminal end event references the committed start —
	// empty is only for a cycle that never started.
	status, res := s.do(http.MethodGet,
		"/v1/sessions/"+sid+"/events?types[]=span.outcome_evaluation_end", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d %v", status, res)
	}
	ends := listData(t, res)
	if len(ends) != 1 {
		t.Fatalf("span.outcome_evaluation_end events = %d, want 1", len(ends))
	}
	if got := ends[0]["outcome_evaluation_start_id"]; got != startEv.ID.String() {
		t.Errorf("outcome_evaluation_start_id = %v, want %s", got, startEv.ID)
	}
	if ends[0]["result"] != "interrupted" {
		t.Errorf("end result = %v, want interrupted", ends[0]["result"])
	}
	outs := sessionOutcomes(t, s, sid)
	if outs[0]["result"] != "interrupted" {
		t.Errorf("entry result = %v, want interrupted", outs[0]["result"])
	}
}

func TestDefineOutcomeFileRubric(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	up := s.uploadFile(t, "rubric.md", nil, "# DCF Rubric\n- five years")
	fileID := up["id"].(string)

	echo := sendEvents(t, s, sid, defineOutcome("Build it", map[string]any{
		"rubric": map[string]any{"type": "file", "file_id": fileID},
	}))
	outcomeID := echo[0]["outcome_id"].(string)

	// The rubric bytes are snapshotted to the outcome-owned key at
	// acceptance: deleting the source file cannot break replay or grading.
	rc, _, err := s.blobs.Get(context.Background(), events.RubricSnapshotKey(domain.ID(outcomeID)))
	if err != nil {
		t.Fatalf("rubric snapshot missing: %v", err)
	}
	snap, _ := io.ReadAll(rc)
	rc.Close()
	if string(snap) != "# DCF Rubric\n- five years" {
		t.Errorf("snapshot = %q, want the uploaded rubric bytes", snap)
	}
}

func TestDefineOutcomeFileRubricRejections(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	// Missing file.
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{defineOutcome("d", map[string]any{
			"rubric": map[string]any{"type": "file", "file_id": "file_0123456789abcdefghjkmnpq"},
		})}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// Oversized rubric file (past the 256 KiB cap).
	up := s.uploadFile(t, "big.md", nil, strings.Repeat("x", 256*1024+1))
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{defineOutcome("d", map[string]any{
			"rubric": map[string]any{"type": "file", "file_id": up["id"].(string)},
		})}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// Exactly at the byte cap is accepted.
	atCap := s.uploadFile(t, "cap.md", nil, strings.Repeat("x", 256*1024))
	sendEvents(t, s, sid, defineOutcome("d", map[string]any{
		"rubric": map[string]any{"type": "file", "file_id": atCap["id"].(string)},
	}))
}

// --- initial_events on POST /v1/sessions (absorbing #161) ---

func TestCreateSessionInitialEvents(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)

	res := createSession(t, s, map[string]any{
		"agent": agentID, "environment_id": envID,
		"initial_events": []any{
			userMessage("context first"),
			defineOutcome("Build a DCF model", map[string]any{"max_iterations": 5}),
		},
	})
	sid := res["id"].(string)

	// Born running with the turn already queued.
	if res["status"] != "running" {
		t.Errorf("created status = %v, want running", res["status"])
	}
	var queued int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE session_id = $1 AND kind = 'model_turn'`, sid).Scan(&queued); err != nil {
		t.Fatalf("count work items: %v", err)
	}
	if queued != 1 {
		t.Errorf("model_turn work items = %d, want 1", queued)
	}
	// The create response already carries the pending outcome entry.
	outs, _ := res["outcome_evaluations"].([]any)
	if len(outs) != 1 {
		t.Fatalf("create response outcome_evaluations = %v, want one entry", res["outcome_evaluations"])
	}
	entry := outs[0].(map[string]any)
	if entry["result"] != "pending" {
		t.Errorf("entry result = %v, want pending", entry["result"])
	}

	// The log holds the initial events in order, then the born-into status.
	status, list := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("list events: %d", status)
	}
	evs := listData(t, list)
	if len(evs) != 4 {
		t.Fatalf("log has %d events, want 4 (message, define_outcome, thread_status_running, status_running)", len(evs))
	}
	for i, wantType := range []string{"user.message", "user.define_outcome", "session.thread_status_running", "session.status_running"} {
		if evs[i]["type"] != wantType {
			t.Errorf("log[%d].type = %v, want %s", i, evs[i]["type"], wantType)
		}
	}
	if evs[1]["max_iterations"] != float64(5) {
		t.Errorf("define_outcome max_iterations = %v, want 5", evs[1]["max_iterations"])
	}
}

func TestCreateSessionInitialEventsRejections(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	base := map[string]any{"agent": agentID, "environment_id": envID}
	create := func(initial []any) (int, map[string]any) {
		body := map[string]any{"initial_events": initial}
		for k, v := range base {
			body[k] = v
		}
		return s.do(http.MethodPost, "/v1/sessions", body)
	}

	// Only the two documented types are accepted.
	status, res := create([]any{map[string]any{"type": "user.interrupt"}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// More than one define_outcome.
	status, res = create([]any{defineOutcome("a", nil), defineOutcome("b", nil)})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// A define_outcome without a rubric.
	status, res = create([]any{map[string]any{"type": "user.define_outcome", "description": "d"}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// More than 50 events.
	many := make([]any, 51)
	for i := range many {
		many[i] = userMessage("m")
	}
	status, res = create(many)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// More than 100 file-sourced document blocks across the list.
	blocks := make([]any, 101)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "document",
			"source": map[string]any{"type": "file", "file_id": "file_0123456789abcdefghjkmnpq"}}
	}
	status, res = create([]any{map[string]any{"type": "user.message", "content": blocks}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")

	// An empty list creates an ordinary idle session with no events.
	status, res = create([]any{})
	if status != http.StatusOK || res["status"] != "idle" {
		t.Fatalf("empty initial_events: status %d session status %v, want 200/idle", status, res["status"])
	}

	// The SDK content union's plain-string form is accepted and echoed.
	status, res = create([]any{map[string]any{"type": "user.message", "content": "plain string"}})
	if status != http.StatusOK || res["status"] != "running" {
		t.Fatalf("string-content initial event: status %d session %v, want 200/running", status, res["status"])
	}
}
