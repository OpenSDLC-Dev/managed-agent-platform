package api_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// --- helpers ---

func eventsFixture(t *testing.T, s *tserver) (sessionID string) {
	t.Helper()
	agentID, envID := fixture(t, s)
	res := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})
	return res["id"].(string)
}

func selfHostedSession(t *testing.T, s *tserver) string {
	t.Helper()
	a := createAgent(t, s, map[string]any{"name": "sh-agent", "model": "claude-opus-4-8"})
	e := createEnvironment(t, s, map[string]any{"name": "sh-env", "config": map[string]any{"type": "self_hosted"}})
	res := createSession(t, s, map[string]any{"agent": a["id"], "environment_id": e["id"]})
	return res["id"].(string)
}

func userMessage(text string) map[string]any {
	return map[string]any{
		"type":    "user.message",
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

// sendEvents posts a batch under the management key and returns the echo.
func sendEvents(t *testing.T, s *tserver, sessionID string, evs ...map[string]any) []map[string]any {
	t.Helper()
	return sendEventsAs(t, s, map[string]string{"x-api-key": testKey}, sessionID, evs...)
}

// sendEventsAs is sendEvents under the given credential headers.
func sendEventsAs(t *testing.T, s *tserver, headers map[string]string, sessionID string, evs ...map[string]any) []map[string]any {
	t.Helper()
	status, res := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs}, headers))
	if status != http.StatusOK {
		t.Fatalf("send events: status %d, body %v", status, res)
	}
	return listData(t, res)
}

// workerAuth is the credential a session's BYOC worker posts under: an
// environment key for the session's own environment, as a Bearer, issued on
// the session's first call and reused after. A user.tool_result is admitted
// under environment credentials alone (#662).
func workerAuth(t *testing.T, s *tserver, sessionID string) map[string]string {
	t.Helper()
	if key, ok := s.workerKeys[sessionID]; ok {
		return asBearer(key)
	}
	var envID string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT environment_id FROM sessions WHERE id = $1`, sessionID).Scan(&envID); err != nil {
		t.Fatalf("session %s's environment: %v", sessionID, err)
	}
	if s.workerKeys == nil {
		s.workerKeys = map[string]string{}
	}
	s.workerKeys[sessionID] = issueKey(t, s.pool, envID, "worker")
	return asBearer(s.workerKeys[sessionID])
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantExactKeys(t *testing.T, obj map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := keysOf(obj)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("wire object keys = %v, want %v", got, want)
	}
}

// --- POST /v1/sessions/{id}/events ---

func TestSendUserMessageEchoShape(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	echo := sendEvents(t, s, sid, userMessage("hello"))
	if len(echo) != 1 {
		t.Fatalf("echoed %d events, want 1", len(echo))
	}
	ev := echo[0]
	// Field-exact: BetaManagedAgentsUserMessageEvent has id, content, type,
	// processed_at — and no session_thread_id.
	wantExactKeys(t, ev, "id", "type", "content", "processed_at")
	if !strings.HasPrefix(ev["id"].(string), "sevt_") {
		t.Errorf("id = %v, want sevt_ prefix", ev["id"])
	}
	if ev["type"] != "user.message" {
		t.Errorf("type = %v", ev["type"])
	}
	if ev["processed_at"] != nil {
		t.Errorf("processed_at = %v, want null (not yet processed)", ev["processed_at"])
	}
	content := ev["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello" {
		t.Errorf("content block = %v", block)
	}
}

func TestSendEchoShapesPerType(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	customID := appendToolUse(t, s, sid, domain.EventAgentCustomToolUse)
	toolID := appendToolUse(t, s, sid, domain.EventAgentToolUse)
	riskyID := appendToolUseWithPerm(t, s, sid, "risky", "ask")
	safeID := appendToolUseWithPerm(t, s, sid, "safe", "ask")

	// Under the worker's credential, since the batch carries a user.tool_result.
	echo := sendEventsAs(t, s, workerAuth(t, s, sid), sid,
		map[string]any{"type": "user.interrupt"},
		map[string]any{"type": "user.tool_confirmation", "result": "deny",
			"tool_use_id": riskyID, "deny_message": "too risky"},
		map[string]any{"type": "user.tool_confirmation", "result": "allow", "tool_use_id": safeID},
		map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": customID,
			"content": []any{map[string]any{"type": "text", "text": "ok"}}, "is_error": false},
		map[string]any{"type": "user.tool_result", "tool_use_id": toolID},
		map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
		map[string]any{"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "note"}}},
	)
	if len(echo) != 7 {
		t.Fatalf("echoed %d events, want 7", len(echo))
	}

	// A null session_thread_id or deny_message is omitted, never rendered
	// null (#674); a value, where there is one, still renders. processed_at
	// renders null while unprocessed (docs/DIVERGENCES.md, "POST/history
	// processed_at").
	interrupt := echo[0]
	wantExactKeys(t, interrupt, "id", "type", "processed_at")

	confirm := echo[1]
	wantExactKeys(t, confirm, "id", "type", "result", "tool_use_id", "deny_message", "processed_at")
	if confirm["result"] != "deny" || confirm["deny_message"] != "too risky" || confirm["tool_use_id"] != riskyID {
		t.Errorf("tool_confirmation echo = %v", confirm)
	}

	allow := echo[2]
	wantExactKeys(t, allow, "id", "type", "result", "tool_use_id", "processed_at")
	if allow["result"] != "allow" || allow["tool_use_id"] != safeID {
		t.Errorf("allow tool_confirmation echo = %v", allow)
	}

	custom := echo[3]
	wantExactKeys(t, custom, "id", "type", "custom_tool_use_id", "content", "is_error", "processed_at")
	if custom["is_error"] != false || custom["custom_tool_use_id"] != customID || custom["processed_at"] == nil {
		t.Errorf("custom_tool_result echo = %v", custom)
	}

	toolRes := echo[4]
	wantExactKeys(t, toolRes, "id", "type", "tool_use_id", "content", "is_error", "processed_at")
	if toolRes["content"] != nil || toolRes["is_error"] != nil {
		t.Errorf("omitted content/is_error should render null: %v", toolRes)
	}

	system := echo[6]
	wantExactKeys(t, system, "id", "type", "content", "processed_at")
	if system["type"] != "system.message" {
		t.Errorf("system echo = %v", system)
	}

	// The list renders deny_message by the same rule: kept when given,
	// omitted when null — never a present null anywhere in the recordings.
	_, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	seen := 0
	for _, ev := range listData(t, res) {
		switch ev["id"] {
		case confirm["id"]:
			seen++
			if ev["deny_message"] != "too risky" {
				t.Errorf("listed deny = %v, want its deny_message", ev)
			}
		case allow["id"]:
			seen++
			if _, ok := ev["deny_message"]; ok {
				t.Errorf("listed allow = %v, want no deny_message", ev)
			}
		}
	}
	if seen != 2 {
		t.Errorf("listed %d of the two confirmations", seen)
	}
}

func TestSendContentBlockKinds(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	echo := sendEvents(t, s, sid, map[string]any{
		"type": "user.message",
		"content": []any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "aGk="}},
			map[string]any{"type": "document", "source": map[string]any{
				"type": "url", "url": "https://example.com/doc.pdf"}, "title": "spec"},
		},
	})
	content := echo[0]["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content has %d blocks, want 3", len(content))
	}
	img := content[1].(map[string]any)["source"].(map[string]any)
	if img["media_type"] != "image/png" || img["data"] != "aGk=" {
		t.Errorf("image block did not round-trip: %v", img)
	}
	doc := content[2].(map[string]any)
	if doc["title"] != "spec" {
		t.Errorf("document title lost: %v", doc)
	}

	// search_result is a tool-result-only block; its source is a plain URL
	// string, and citations.enabled is required on the wire.
	echo = sendEvents(t, s, sid, map[string]any{
		"type": "user.custom_tool_result", "custom_tool_use_id": appendToolUse(t, s, sid, domain.EventAgentCustomToolUse),
		"content": []any{map[string]any{
			"type": "search_result", "source": "https://example.com", "title": "hit",
			"citations": map[string]any{"enabled": false},
			"content":   []any{map[string]any{"type": "text", "text": "body"}}}},
	})
	sr := echo[0]["content"].([]any)[0].(map[string]any)
	if sr["source"] != "https://example.com" || sr["title"] != "hit" {
		t.Errorf("search_result did not round-trip: %v", sr)
	}
	if cit, _ := sr["citations"].(map[string]any); cit == nil || cit["enabled"] != false {
		t.Errorf("citations did not round-trip: %v", sr["citations"])
	}
}

func TestSendValidationSweep(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	path := "/v1/sessions/" + sid + "/events"

	txt := []any{map[string]any{"type": "text", "text": "x"}}
	cases := []struct {
		name string
		body any
		frag string
	}{
		{"missing events", map[string]any{}, "events"},
		{"events not array", map[string]any{"events": "nope"}, "array"},
		{"empty events", map[string]any{"events": []any{}}, "at least one"},
		{"unknown top-level key", map[string]any{"events": []any{userMessage("x")}, "stream": true}, "stream"},
		{"event missing type", map[string]any{"events": []any{map[string]any{"content": txt}}}, "type is required"},
		{"unknown type", map[string]any{"events": []any{map[string]any{"type": "user.bogus"}}}, "unknown event type"},
		{"platform type", map[string]any{"events": []any{map[string]any{"type": "agent.message"}}}, "emitted by the platform"},
		// The two MCP shapes by name. Both were already refused — platformEmitted
		// has listed them since the types existed — so these rows pin standing
		// behavior rather than anything this change introduced. They are named
		// now because the gate acts on one of them: a client able to post an
		// agent.mcp_tool_use carrying evaluated_permission "ask" could park its
		// own session on a call nothing runs, and one able to post an
		// agent.mcp_tool_result could answer a call only the platform may answer.
		{"platform MCP tool use", map[string]any{"events": []any{map[string]any{"type": "agent.mcp_tool_use"}}}, "emitted by the platform"},
		{"platform MCP tool result", map[string]any{"events": []any{map[string]any{"type": "agent.mcp_tool_result"}}}, "emitted by the platform"},
		{"stream-only type", map[string]any{"events": []any{map[string]any{"type": "event_delta"}}}, "stream-only"},
		{"define_outcome empty rubric", map[string]any{"events": []any{map[string]any{"type": "user.define_outcome",
			"description": "d", "rubric": map[string]any{}}}}, "rubric type is required"},
		{"define_outcome no rubric", map[string]any{"events": []any{map[string]any{"type": "user.define_outcome",
			"description": "d"}}}, "rubric is required"},
		{"define_outcome empty description", map[string]any{"events": []any{map[string]any{"type": "user.define_outcome",
			"description": "", "rubric": map[string]any{"type": "text", "content": "r"}}}}, "description must not be empty"},
		{"define_outcome bad max_iterations", map[string]any{"events": []any{map[string]any{"type": "user.define_outcome",
			"description": "d", "rubric": map[string]any{"type": "text", "content": "r"}, "max_iterations": 21}}}, "between 1 and 20"},
		{"unknown event field", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": txt, "contents": txt}}}, "unknown field"},
		{"message without content", map[string]any{"events": []any{map[string]any{"type": "user.message"}}}, "content is required"},
		{"content not array or string", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": 42}}}, "array of content blocks"},
		{"bad block type", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "search_result"}}}}}, "not allowed here"},
		{"text block without text", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "text"}}}}}, "text"},
		{"image without source", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "image"}}}}}, "source is required"},
		{"image bad source kind", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "text",
				"data": "x", "media_type": "text/plain"}}}}}}, "not allowed here"},
		{"base64 without data", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64",
				"media_type": "image/png"}}}}}}, "data is required"},
		{"confirmation without result", map[string]any{"events": []any{map[string]any{
			"type": "user.tool_confirmation", "tool_use_id": "sevt_1"}}}, "result is required"},
		{"confirmation bad result", map[string]any{"events": []any{map[string]any{
			"type": "user.tool_confirmation", "tool_use_id": "sevt_1", "result": "maybe"}}}, `"allow" or "deny"`},
		{"confirmation without tool_use_id", map[string]any{"events": []any{map[string]any{
			"type": "user.tool_confirmation", "result": "allow"}}}, "tool_use_id is required"},
		{"deny_message with allow", map[string]any{"events": []any{map[string]any{
			"type": "user.tool_confirmation", "result": "allow", "tool_use_id": "sevt_1",
			"deny_message": "no"}}}, `only allowed when result is "deny"`},
		{"custom result without id", map[string]any{"events": []any{map[string]any{
			"type": "user.custom_tool_result"}}}, "custom_tool_use_id is required"},
		{"is_error not bool", map[string]any{"events": []any{map[string]any{
			"type": "user.custom_tool_result", "custom_tool_use_id": "sevt_1",
			"is_error": "yes"}}}, "boolean"},
		{"thread id rejected", map[string]any{"events": []any{map[string]any{
			"type": "user.interrupt", "session_thread_id": "sthr_1"}}}, "does not name a thread in this session"},
		{"NUL in text", map[string]any{"events": []any{userMessage("a\x00b")}}, "U+0000"},
		{"system.message alone", map[string]any{"events": []any{map[string]any{
			"type": "system.message", "content": txt}}}, "immediately follow"},
		{"system.message not last", map[string]any{"events": []any{
			map[string]any{"type": "system.message", "content": txt}, userMessage("x")}}, "final event"},
		{"system.message non-text block", map[string]any{"events": []any{userMessage("x"),
			map[string]any{"type": "system.message", "content": []any{map[string]any{"type": "image",
				"source": map[string]any{"type": "url", "url": "https://x"}}}}}}, "not allowed here"},
		{"two system.messages", map[string]any{"events": []any{userMessage("x"),
			map[string]any{"type": "system.message", "content": txt},
			map[string]any{"type": "system.message", "content": txt}}}, "final event"},
	}
	for _, tc := range cases {
		status, res := s.do(http.MethodPost, path, tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %v)", tc.name, status, res)
			continue
		}
		wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
		inner, _ := res["error"].(map[string]any)
		if msg, _ := inner["message"].(string); !strings.Contains(msg, tc.frag) {
			t.Errorf("%s: message %q does not mention %q", tc.name, msg, tc.frag)
		}
	}

	// An invalid batch is atomic: nothing from it may land in the log.
	status, res := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK || len(listData(t, res)) != 0 {
		t.Errorf("failed batches must append nothing; log has %v", res)
	}
}

// The BYOC pull protocol: a self_hosted session's worker answers a sandbox
// call under its environment key.
func TestSendToolResultOnSelfHosted(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	echo := sendEventsAs(t, s, workerAuth(t, s, sid), sid, map[string]any{
		"type": "user.tool_result", "tool_use_id": appendToolUse(t, s, sid, domain.EventAgentToolUse), "is_error": true,
		"content": []any{map[string]any{"type": "text", "text": "exit 1"}},
	})
	if echo[0]["is_error"] != true {
		t.Errorf("is_error = %v", echo[0]["is_error"])
	}
}

// toolResultRefusal is the 403 a management credential's user.tool_result
// draws, at the batch index of the first one.
func toolResultRefusal(index int) string {
	return fmt.Sprintf("events[%d]: `user.tool_result` may only be sent with environment credentials "+
		"(the self-hosted worker's environment key or its sessions token); "+
		"an API key or Console session cannot post this event type", index)
}

// TestToolResultAdmissionIsByCredential pins who may post a user.tool_result
// (#662): the reference decides by the credential that signed the request,
// not by the session's environment kind. A management credential is refused
// 403 permission_error — the recorded answer to a Console session on a
// self_hosted session (2026-09-02 batch2.json,
// sessW.send.user.tool_result.console-auth) — and a tool_result anywhere in a
// batch refuses all of it, so nothing lands. The session's own environment
// key is admitted (sessW.send.user.tool_result.env-key); another
// environment's key never reaches the session at all.
func TestToolResultAdmissionIsByCredential(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	useID := appendToolUse(t, s, sid, domain.EventAgentToolUse)
	path := "/v1/sessions/" + sid + "/events"
	result := map[string]any{"type": "user.tool_result", "tool_use_id": useID,
		"content": []any{map[string]any{"type": "text", "text": "exit 0"}}}
	mgmt := map[string]string{"x-api-key": testKey}
	before := s.eventTypes(sid)
	wasStatus := s.sessionStatus(sid)
	untouched := func(after string) {
		t.Helper()
		if got := s.eventTypes(sid); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Errorf("after %s the log is %v, want it unchanged at %v", after, got, before)
		}
		if got := s.sessionStatus(sid); got != wasStatus {
			t.Errorf("after %s the status is %q, want it unchanged at %q", after, got, wasStatus)
		}
		if n := s.liveWork(sid, queue.ModelTurn); n != 0 {
			t.Errorf("after %s live model_turn = %d, want 0", after, n)
		}
	}

	st, body := readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{result}}, mgmt))
	wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", toolResultRefusal(0))
	untouched("a management key's tool_result")

	// Behind a message the whole batch is refused, the message included —
	// which would otherwise have resumed the turn, the result answering the
	// one outstanding call.
	st, body = readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{userMessage("carry on"), result}}, mgmt))
	wantErrMsg(t, st, body, http.StatusForbidden, "permission_error", toolResultRefusal(1))
	untouched("a mixed batch under a management key")

	// A live key for another environment reaches no session outside it: the
	// not-found an absent session gets, before the handler runs.
	_, _, otherKey := selfHostedWorker(t, s, "elsewhere")
	st, body = readJSON(t, s.doRaw(http.MethodPost, path, map[string]any{"events": []any{result}}, asBearer(otherKey)))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")
	untouched("another environment's key")

	// The management key keeps every other event type: the gate is the event
	// type's, not a lane-wide refusal.
	sendEvents(t, s, sid, userMessage("still here"))

	echo := sendEventsAs(t, s, workerAuth(t, s, sid), sid, result)
	if echo[0]["type"] != "user.tool_result" || echo[0]["tool_use_id"] != useID {
		t.Errorf("the worker's result echoed as %v", echo[0])
	}
}

// TestToolResultRefusalOrder pins where the credential rule sits: in the
// batch's normalization, after the session is read, so a management caller's
// result to an absent session is the 404 and to an archived one the archived
// 400, and within a batch the first event that fails decides the answer. The
// reference's order in all three is unobserved (docs/DIVERGENCES.md).
func TestToolResultRefusalOrder(t *testing.T) {
	s := newTestServer(t)
	mgmt := map[string]string{"x-api-key": testKey}
	result := map[string]any{"type": "user.tool_result", "tool_use_id": "sevt_00000000000000000000000000"}

	st, body := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+domain.NewID("sesn").String()+"/events",
		map[string]any{"events": []any{result}}, mgmt))
	wantErr(t, st, body, http.StatusNotFound, "not_found_error")

	archived := selfHostedSession(t, s)
	if st, _ := s.do(http.MethodPost, "/v1/sessions/"+archived+"/archive", nil); st != http.StatusOK {
		t.Fatalf("archive: %d", st)
	}
	st, body = readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+archived+"/events",
		map[string]any{"events": []any{result}}, mgmt))
	wantErr(t, st, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "archived") {
		t.Errorf("message %q, want the archived refusal", msg)
	}

	live := selfHostedSession(t, s)
	st, body = readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+live+"/events",
		map[string]any{"events": []any{map[string]any{"type": "user.bogus"}, result}}, mgmt))
	wantErr(t, st, body, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := body["error"].(map[string]any)["message"].(string); !strings.HasPrefix(msg, "events[0]: unknown event type") {
		t.Errorf("message %q, want events[0]'s own refusal", msg)
	}
	if got := s.eventTypes(live); len(got) != 0 {
		t.Errorf("the refused batch left %v on the log", got)
	}
}

// TestToolResultTypeSpellingsUnderAManagementKey posts raw bodies — which a
// marshalled map could not carry: a duplicate key, a chosen escape — spelling
// the type every way a second parse could read differently from the one that
// decides it. Under a management key none may land a user.tool_result: a
// spelling that decodes to user.tool_result draws the credential's 403, and
// the rest the 400 an unknown, absent or ill-typed type, or a stray field,
// gets (#662).
func TestToolResultTypeSpellingsUnderAManagementKey(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	useID := appendToolUse(t, s, sid, domain.EventAgentToolUse)
	path := "/v1/sessions/" + sid + "/events"
	mgmt := map[string]string{"x-api-key": testKey}
	rest := fmt.Sprintf(`"tool_use_id":%q,"content":[{"type":"text","text":"forged"}]`, useID)
	before := s.eventTypes(sid)

	const forbidden, invalid = http.StatusForbidden, http.StatusBadRequest
	for _, tc := range []struct {
		name  string
		event string
		want  int
	}{
		{"duplicate type, tool_result last", `{"type":"user.message","type":"user.tool_result",` + rest + `}`, forbidden},
		{"duplicate type, tool_result first", `{"type":"user.tool_result","type":"user.message",` + rest + `}`, invalid},
		{"escaped key", `{"\u0074ype":"user.tool_result",` + rest + `}`, forbidden},
		{"escaped key after a plain one", `{"type":"user.message","\u0074ype":"user.tool_result",` + rest + `}`, forbidden},
		{"escaped key before a plain one", `{"\u0074ype":"user.tool_result","type":"user.message",` + rest + `}`, invalid},
		{"escaped dot in the value", `{"type":"user\u002etool_result",` + rest + `}`, forbidden},
		{"every letter escaped", `{"type":"\u0075\u0073\u0065\u0072\u002e\u0074\u006f\u006f\u006c\u005f\u0072\u0065\u0073\u0075\u006c\u0074",` + rest + `}`, forbidden},
		{"whitespace around the colon", "{\"type\"\t:\n \"user.tool_result\"," + rest + "}", forbidden},
		{"Type beside type", `{"type":"user.message","Type":"user.tool_result",` + rest + `}`, invalid},
		{"Type alone", `{"Type":"user.tool_result",` + rest + `}`, invalid},
		{"TYPE alone", `{"TYPE":"user.tool_result",` + rest + `}`, invalid},
		{"a space in the key", `{"type ":"user.tool_result",` + rest + `}`, invalid},
		{"null type", `{"type":null,` + rest + `}`, invalid},
		{"numeric type", `{"type":7,` + rest + `}`, invalid},
		{"array type", `{"type":["user.tool_result"],` + rest + `}`, invalid},
		{"object type", `{"type":{"type":"user.tool_result"},` + rest + `}`, invalid},
		{"upper-case value", `{"type":"USER.TOOL_RESULT",` + rest + `}`, invalid},
		{"mixed-case value", `{"type":"User.Tool_Result",` + rest + `}`, invalid},
		{"leading space", `{"type":" user.tool_result",` + rest + `}`, invalid},
		{"trailing space", `{"type":"user.tool_result ",` + rest + `}`, invalid},
		{"escaped trailing space", `{"type":"user.tool_result\u0020",` + rest + `}`, invalid},
		{"escaped NUL suffix", `{"type":"user.tool_result\u0000",` + rest + `}`, invalid},
	} {
		st, body := readJSON(t, s.doRaw(http.MethodPost, path, `{"events":[`+tc.event+`]}`, mgmt))
		inner, _ := body["error"].(map[string]any)
		wantType := map[int]string{forbidden: "permission_error", invalid: "invalid_request_error"}[tc.want]
		if st != tc.want || inner["type"] != wantType {
			t.Errorf("%s: %d %v, want %d %s", tc.name, st, body, tc.want, wantType)
		}
	}
	if got := s.eventTypes(sid); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("the log is %v after every spelling was refused, want it unchanged at %v", got, before)
	}
}

// TestToolResultOnACloudSession: the credential gate holds on a cloud session
// too — the reference refused a Console session's result there with the same
// 403 (2026-09-02 batch2.json, sessT.send.user.tool_result.for-platform-call).
// What it answers an environment credential on a cloud session is unobserved,
// because every recorded cloud probe was refused by that gate first, so the
// environment-kind refusal this platform keeps behind it is an inference
// (docs/DIVERGENCES.md). The console issues no key for a cloud environment;
// this one is seeded directly.
func TestToolResultOnACloudSession(t *testing.T) {
	s := newTestServer(t)
	agentID, envID := fixture(t, s)
	sid := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
	useID := appendToolUse(t, s, sid, domain.EventAgentToolUse)
	path := "/v1/sessions/" + sid + "/events"
	body := map[string]any{"events": []any{map[string]any{"type": "user.tool_result", "tool_use_id": useID,
		"content": []any{map[string]any{"type": "text", "text": "client says hi"}}}}}
	before := s.eventTypes(sid)

	st, res := readJSON(t, s.doRaw(http.MethodPost, path, body, map[string]string{"x-api-key": testKey}))
	wantErrMsg(t, st, res, http.StatusForbidden, "permission_error", toolResultRefusal(0))

	st, res = readJSON(t, s.doRaw(http.MethodPost, path, body, asBearer(issueKey(t, s.pool, envID, "cloud"))))
	wantErr(t, st, res, http.StatusBadRequest, "invalid_request_error")
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "self_hosted") {
		t.Errorf("message %q must name the environment kind that admits a tool result", msg)
	}
	if got := s.eventTypes(sid); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Errorf("the log is %v after two refusals, want it unchanged at %v", got, before)
	}
}

// TestSessionsTokenPostsAToolResult: the per-item sessions token is an
// environment credential here, because the reference worker sends its tool
// results under it whenever the item carries one (checked against
// anthropic-sdk-go v1.70.1 — worker.go EnvironmentWorker.handleItem) — so it
// is admitted, where the 2026-09-02 recording saw the reference refuse a
// decoded sessions_token (sessW.send.user.tool_result.sessions_token).
// Registered in docs/DIVERGENCES.md.
func TestSessionsTokenPostsAToolResult(t *testing.T) {
	s := newTestServer(t)
	_, envID, sid, _, key := storeWorker(t, s, "result")
	workID, _, token := pollItem(t, s, envID, key)
	if token == "" {
		t.Fatal("no sessions token on the poll")
	}
	if st := status(t, s, http.MethodPost, "/v1/environments/"+envID+"/work/"+workID+"/ack", nil, asBearer(key)); st != http.StatusOK {
		t.Fatalf("ack = %d", st)
	}
	useID := appendToolUse(t, s, sid, domain.EventAgentToolUse)
	echo := sendEventsAs(t, s, asBearer(token), sid, map[string]any{"type": "user.tool_result", "tool_use_id": useID,
		"content": []any{map[string]any{"type": "text", "text": "ok"}}})
	if echo[0]["tool_use_id"] != useID {
		t.Errorf("the token's result echoed as %v", echo[0])
	}
}

// Web calls are platform-executed on both environment kinds, so a client
// user.tool_result may never answer one — even while the executor's web pass
// is still running it, which is the double-answer window #222 closes. The
// name-scoped rejection must not touch sandbox calls: answering those via
// user.tool_result is the BYOC pull protocol (TestSendToolResultOnSelfHosted).
// Posted under the worker's own key: a management key is refused by the
// credential gate before the reference is read, so under environment
// credentials this arm is the only guard.
func TestSendToolResultForWebCallRejected(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	webID := appendToolUseWithPerm(t, s, sid, "web_fetch", "allow")
	status, res := readJSON(t, s.doRaw(http.MethodPost, "/v1/sessions/"+sid+"/events", map[string]any{
		"events": []any{map[string]any{"type": "user.tool_result", "tool_use_id": webID,
			"content": []any{map[string]any{"type": "text", "text": "forged"}}}}}, workerAuth(t, s, sid)))
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	inner, _ := res["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.Contains(msg, "platform-executed") || !strings.Contains(msg, "web_fetch") {
		t.Errorf("message %q must name the platform-executed web call", msg)
	}
}

func TestSendSessionStateErrors(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	status, res := s.do(http.MethodPost, "/v1/sessions/sesn_missing/events",
		map[string]any{"events": []any{userMessage("x")}})
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")

	if st, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/archive", nil); st != http.StatusOK {
		t.Fatalf("archive: %d", st)
	}
	status, res = s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{userMessage("x")}})
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

func TestSendAcceptsSessionPrefix(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	alias := "session_" + strings.TrimPrefix(sid, "sesn_")
	echo := sendEvents(t, s, alias, userMessage("via alias"))
	if echo[0]["type"] != "user.message" {
		t.Errorf("echo = %v", echo[0])
	}
}

// --- GET /v1/sessions/{id}/events ---

func TestListEventsPagingAndFilters(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	path := "/v1/sessions/" + sid + "/events"

	for i := 0; i < 5; i++ {
		sendEvents(t, s, sid, userMessage(fmt.Sprintf("m%d", i)))
	}
	sendEvents(t, s, sid, map[string]any{"type": "user.interrupt"})
	// Four platform events sit on top of the six posted ones: the first
	// user.message woke the idle session, and the interrupt ended the turn that
	// woke, each a primary-thread + session status pair — m0,
	// session.status_running, session.thread_status_running, m1..m4,
	// user.interrupt, session.thread_status_idle, session.status_idle.

	// Default: chronological asc, everything, next_page null.
	status, res := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, res)
	}
	all := listData(t, res)
	if len(all) != 10 {
		t.Fatalf("listed %d, want 10", len(all))
	}
	// The list pages on seq, which is monotonic per session, so nothing above
	// or in the walk below needs the clock. The created_at comparators further
	// down do: they take a boundary off one event and count both sides, which
	// holds only while created_at rises with seq across the settlements that
	// wrote these ten. Each row takes its own clock_timestamp() as it is
	// inserted, so what would break that is the same backwards step of the
	// database clock as everywhere else here (#411), answered the same way
	// (#561).
	//
	// Stamping costs the emitter's own guarantee no coverage: that created_at
	// never runs backwards against seq is pinned where it is engineered, by
	// TestAppendConcurrentSeqIntegrity in internal/events, under lock
	// contention this test does not produce.
	seqOrder := make([]string, 0, len(all))
	for _, ev := range all {
		seqOrder = append(seqOrder, ev["id"].(string))
	}
	stampCreatedAt(t, s, "events", seqOrder...)
	if all[0]["content"].([]any)[0].(map[string]any)["text"] != "m0" {
		t.Errorf("default order is not chronological: first = %v", all[0])
	}
	if np := nextPage(t, res); np != "" {
		t.Errorf("next_page = %q, want null", np)
	}

	// Cursor walk at limit=2: pages of 2/2/2/2/2, opaque next_page in between.
	var walked []string
	page := ""
	for pages := 0; ; pages++ {
		url := path + "?limit=2"
		if page != "" {
			url += "&page=" + page
		}
		status, res := s.do(http.MethodGet, url, nil)
		if status != http.StatusOK {
			t.Fatalf("walk: %d", status)
		}
		for _, ev := range listData(t, res) {
			walked = append(walked, ev["id"].(string))
		}
		if page = nextPage(t, res); page == "" {
			break
		}
		if pages > 5 {
			t.Fatal("cursor walk did not terminate")
		}
	}
	if len(walked) != 10 {
		t.Errorf("cursor walk saw %d events, want 10", len(walked))
	}
	for i, ev := range all {
		if walked[i] != ev["id"].(string) {
			t.Errorf("walk[%d] = %s, want %s", i, walked[i], ev["id"])
		}
	}

	// desc reverses.
	_, res = s.do(http.MethodGet, path+"?order=desc", nil)
	desc := listData(t, res)
	if desc[0]["id"] != all[9]["id"] {
		t.Errorf("desc first = %v, want %v", desc[0]["id"], all[9]["id"])
	}

	// types filter, both spellings.
	for _, qs := range []string{"?types[]=user.interrupt", "?types=user.interrupt"} {
		_, res = s.do(http.MethodGet, path+qs, nil)
		if got := listData(t, res); len(got) != 1 || got[0]["type"] != "user.interrupt" {
			t.Errorf("types filter %s returned %v", qs, got)
		}
	}
	// Unknown type values filter to empty, not error.
	_, res = s.do(http.MethodGet, path+"?types[]=user.bogus", nil)
	if got := listData(t, res); len(got) != 0 {
		t.Errorf("bogus type filter returned %v", got)
	}

	// created_at range: everything strictly after the fourth event.
	mid := all[3]["id"].(string)
	var midCreated string
	err := s.pool.QueryRow(context.Background(),
		`SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM events WHERE id = $1`,
		mid).Scan(&midCreated)
	if err != nil {
		t.Fatal(err)
	}
	_, res = s.do(http.MethodGet, path+"?created_at[gt]="+midCreated, nil)
	if got := listData(t, res); len(got) != 6 {
		t.Errorf("created_at[gt] mid returned %d, want 6", len(got))
	}
	_, res = s.do(http.MethodGet, path+"?created_at[lte]="+midCreated, nil)
	if got := listData(t, res); len(got) != 4 {
		t.Errorf("created_at[lte] mid returned %d, want 4", len(got))
	}

	// Validation.
	status, res = s.do(http.MethodGet, path+"?order=upside_down", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?limit=0", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	// The session-events list accepts limit up to 1000 — the reference worker's
	// SessionToolRunner reconciles by listing with limit=1000 (checked against
	// anthropic-sdk-go v1.70.1 — betasessiontoolrunner.go
	// SessionToolRunner.reconcile) — unlike the other lists, which cap at 100.
	// 1001 is rejected (our compatible upper bound; see maxEventLimit).
	status, res = s.do(http.MethodGet, path+"?limit=1000", nil)
	if status != http.StatusOK {
		t.Errorf("limit=1000 on events: status %d, want 200: %s", status, res)
	}
	status, res = s.do(http.MethodGet, path+"?limit=1001", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?page=@@@", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?created_at[gte]=yesterday", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, "/v1/sessions/sesn_missing/events", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")

	// A time-keyed cursor (the resource lists' kind) is rejected, not
	// misread as a seq position.
	timeCursor := base64.RawURLEncoding.EncodeToString([]byte("k1|n|t|1752000000000000000|agent_x"))
	status, res = s.do(http.MethodGet, path+"?page="+timeCursor, nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// --- GET /v1/sessions/{id}/events/stream ---

// sseStream reads frames off a live stream in the background.
type sseStream struct {
	cancel context.CancelFunc
	frames chan sseFrame
}

type sseFrame struct {
	name string
	data map[string]any
}

func (s *tserver) stream(t *testing.T, path string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		cancel()
		t.Fatalf("stream status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		cancel()
		t.Fatalf("content-type = %q", ct)
	}

	st := &sseStream{cancel: cancel, frames: make(chan sseFrame, 64)}
	t.Cleanup(st.close)
	go func() {
		defer res.Body.Close()
		defer close(st.frames)
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		var name string
		var data strings.Builder
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				var obj map[string]any
				if json.Unmarshal([]byte(data.String()), &obj) == nil {
					st.frames <- sseFrame{name: name, data: obj}
				}
				name, data = "", strings.Builder{}
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data.WriteString(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return st
}

func (st *sseStream) close() { st.cancel() }

// next returns the next non-ping frame.
func (st *sseStream) next(t *testing.T) sseFrame {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				t.Fatal("stream closed while waiting for a frame")
			}
			if f.name == "ping" {
				continue
			}
			return f
		case <-deadline:
			t.Fatal("no frame within timeout")
		}
	}
}

// expectNone asserts no non-ping frame is buffered.
func (st *sseStream) expectNone(t *testing.T) {
	t.Helper()
	select {
	case f, ok := <-st.frames:
		if ok && f.name != "ping" {
			t.Errorf("unexpected frame %q: %v", f.name, f.data)
		}
	default:
	}
}

// expectClosed asserts the server ends the stream.
func (st *sseStream) expectClosed(t *testing.T) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-st.frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not close")
		}
	}
}

func TestStreamLiveTail(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	// History from before the connection is NOT replayed on the stream —
	// clients seed history via list (the wire has no stream cursor).
	sendEvents(t, s, sid, userMessage("before"))

	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")
	echo := sendEvents(t, s, sid, userMessage("after-1"))
	f := st.next(t)
	if f.name != "user.message" || f.data["type"] != "user.message" {
		t.Fatalf("frame name/type = %q/%v", f.name, f.data["type"])
	}
	if f.data["id"] != echo[0]["id"] {
		t.Errorf("streamed id %v, want %v (the pre-connect event must not replay)", f.data["id"], echo[0]["id"])
	}
	if text := f.data["content"].([]any)[0].(map[string]any)["text"]; text != "after-1" {
		t.Errorf("streamed text = %v", text)
	}

	// Batches arrive in order, the platform's reaction to them included: the
	// interrupt ends the running turn and the message in the same batch starts
	// the next one, so both status pairs — the idle one thread-first, the
	// running one session-first — follow the two posted events.
	sendEvents(t, s, sid, userMessage("after-2"), map[string]any{"type": "user.interrupt"})
	for i, want := range []string{"user.message", "user.interrupt",
		"session.thread_status_idle", "session.status_idle",
		"session.status_running", "session.thread_status_running"} {
		if f := st.next(t); f.name != want {
			t.Errorf("frame %d = %q, want %q", i+2, f.name, want)
		}
	}
	st.expectNone(t)
}

func TestStreamValidation(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)

	status, res := s.do(http.MethodGet, "/v1/sessions/sesn_missing/events/stream", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")

	status, res = s.do(http.MethodGet, "/v1/sessions/"+sid+"/events/stream?event_deltas[]=user.message", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

func TestStreamPreviewDeltas(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	log := events.NewLog(s.pool)
	ctx := context.Background()

	// One subscriber opted into agent.message previews, one not.
	optIn := s.stream(t, "/v1/sessions/"+sid+"/events/stream?event_deltas[]=agent.message&event_deltas[]=agent.thinking")
	plain := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	// The brain-side preview flow (slice 5 will drive this in production).
	preview, err := log.StartPreview(ctx, sid2id(t, sid), domain.EventAgentMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := preview.Delta(ctx, 0, "hel"); err != nil {
		t.Fatal(err)
	}
	if err := preview.Delta(ctx, 0, "lo"); err != nil {
		t.Fatal(err)
	}

	start := optIn.next(t)
	if start.name != "event_start" || start.data["type"] != "event_start" {
		t.Fatalf("first preview frame = %q %v", start.name, start.data)
	}
	pv := start.data["event"].(map[string]any)
	if pv["type"] != "agent.message" || pv["id"] != preview.EventID().String() {
		t.Errorf("event_start preview = %v", pv)
	}
	for _, want := range []string{"hel", "lo"} {
		f := optIn.next(t)
		if f.name != "event_delta" {
			t.Fatalf("frame = %q", f.name)
		}
		d := f.data["delta"].(map[string]any)
		if d["type"] != "content_delta" {
			t.Errorf("delta type = %v, want content_delta", d["type"])
		}
		if got := d["content"].(map[string]any)["text"]; got != want {
			t.Errorf("delta text = %v, want %q", got, want)
		}
		if f.data["event_id"] != preview.EventID().String() {
			t.Errorf("event_id = %v", f.data["event_id"])
		}
	}

	// The buffered event supersedes the previews under the same id, and
	// reaches BOTH subscribers.
	if _, err := log.Append(ctx, sid2id(t, sid), []events.NewEvent{{
		ID: preview.EventID(), Type: domain.EventAgentMessage,
		Payload: json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`),
	}}); err != nil {
		t.Fatal(err)
	}
	buffered := optIn.next(t)
	if buffered.name != "agent.message" || buffered.data["id"] != preview.EventID().String() {
		t.Errorf("buffered frame = %q id %v", buffered.name, buffered.data["id"])
	}
	// The non-opted subscriber saw no previews — its FIRST frame is the
	// buffered event itself.
	pf := plain.next(t)
	if pf.name != "agent.message" || pf.data["id"] != preview.EventID().String() {
		t.Errorf("plain subscriber first frame = %q %v", pf.name, pf.data)
	}
	plain.expectNone(t)

	// Previews never persist: the log holds exactly the buffered event.
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	if status != http.StatusOK || len(listData(t, res)) != 1 {
		t.Errorf("log should hold only the buffered event: %v", res)
	}

	// agent.thinking is start-only: opted-in subscribers get event_start,
	// then the buffered thinking event with the same id.
	thinking, err := log.StartPreview(ctx, sid2id(t, sid), domain.EventAgentThinking)
	if err != nil {
		t.Fatal(err)
	}
	ts := optIn.next(t)
	if ts.name != "event_start" || ts.data["event"].(map[string]any)["type"] != "agent.thinking" {
		t.Fatalf("thinking start = %v", ts.data)
	}
	if _, err := log.Append(ctx, sid2id(t, sid), []events.NewEvent{{
		ID: thinking.EventID(), Type: domain.EventAgentThinking,
	}}); err != nil {
		t.Fatal(err)
	}
	tb := optIn.next(t)
	if tb.name != "agent.thinking" || tb.data["id"] != thinking.EventID().String() {
		t.Errorf("buffered thinking = %q %v", tb.name, tb.data)
	}
}

func TestStreamSessionDeletedTerminates(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	if status, _ := s.do(http.MethodDelete, "/v1/sessions/"+sid, nil); status != http.StatusOK {
		t.Fatalf("delete: %d", status)
	}
	f := st.next(t)
	if f.name != "session.deleted" || f.data["type"] != "session.deleted" {
		t.Fatalf("frame = %q %v", f.name, f.data)
	}
	if id, _ := f.data["id"].(string); !strings.HasPrefix(id, "sevt_") {
		t.Errorf("session.deleted id = %v", f.data["id"])
	}
	if _, ok := f.data["processed_at"].(string); !ok {
		t.Errorf("session.deleted processed_at = %v", f.data["processed_at"])
	}
	st.expectClosed(t)
}

// sid2id converts the wire session id string to a domain ID.
func sid2id(t *testing.T, sid string) domain.ID {
	t.Helper()
	if !strings.HasPrefix(sid, "sesn_") {
		t.Fatalf("unexpected session id %q", sid)
	}
	return domain.ID(sid)
}

func TestListEventsDescCursorKeepsDirection(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	path := "/v1/sessions/" + sid + "/events"
	for i := 0; i < 5; i++ {
		sendEvents(t, s, sid, userMessage(fmt.Sprintf("d%d", i)))
	}

	status, res := s.do(http.MethodGet, path+"?order=desc&limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("desc page 1: %d", status)
	}
	page1 := listData(t, res)
	cur := nextPage(t, res)
	if cur == "" {
		t.Fatal("desc page 1 has no next_page")
	}

	// Following next_page WITHOUT re-passing order must keep walking desc.
	status, res = s.do(http.MethodGet, path+"?limit=2&page="+cur, nil)
	if status != http.StatusOK {
		t.Fatalf("desc page 2: %d", status)
	}
	page2 := listData(t, res)
	texts := func(evs []map[string]any) (out []string) {
		for _, ev := range evs {
			out = append(out, ev["content"].([]any)[0].(map[string]any)["text"].(string))
		}
		return
	}
	got := append(texts(page1), texts(page2)...)
	want := []string{"d4", "d3", "d2", "d1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("desc walk = %v, want %v", got, want)
		}
	}

	// An explicitly contradicting order is an error, not a silent restart.
	status, res = s.do(http.MethodGet, path+"?order=asc&page="+cur, nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

func TestStreamErrorFrameOnCorruptRow(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	// A corrupt row slipped in behind the API's back (no NOTIFY)…
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO events (id, session_id, seq, type, payload) VALUES ($1, $2, 1, 'user.message', '[1]')`,
		domain.NewID("sevt").String(), sid); err != nil {
		t.Fatal(err)
	}
	// …and the next real append wakes the stream into rendering it.
	status, _ := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events",
		map[string]any{"events": []any{userMessage("wake")}})
	if status != http.StatusOK {
		t.Fatalf("send: %d", status)
	}

	f := st.next(t)
	if f.name != "error" || f.data["type"] != "error" {
		t.Fatalf("frame = %q %v, want the protocol error frame", f.name, f.data)
	}
	inner, _ := f.data["error"].(map[string]any)
	if inner == nil || inner["type"] != "api_error" {
		t.Errorf("error frame body = %v", f.data)
	}
	st.expectClosed(t)
}

func TestStreamDeletionBackstopViaPing(t *testing.T) {
	restore := api.SetPingIntervalForTest(50 * time.Millisecond)
	defer restore()

	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	// Delete the session row directly — simulating a lost session.deleted
	// broadcast (another replica's NOTIFY missed during a reconnect gap).
	// The ping-time existence check must still terminate the stream.
	if _, err := s.pool.Exec(context.Background(),
		`DELETE FROM sessions WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	f := st.next(t)
	if f.name != "session.deleted" || f.data["type"] != "session.deleted" {
		t.Fatalf("frame = %q %v", f.name, f.data)
	}
	st.expectClosed(t)
}

func TestStreamPingKeepalive(t *testing.T) {
	restore := api.SetPingIntervalForTest(30 * time.Millisecond)
	defer restore()

	s := newTestServer(t)
	sid := eventsFixture(t, s)
	st := s.stream(t, "/v1/sessions/"+sid+"/events/stream")

	// The reference decoder skips ping frames; ours must carry the event
	// name "ping" so it recognizes them.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				t.Fatal("stream closed before a ping")
			}
			if f.name == "ping" {
				if f.data["type"] != "ping" {
					t.Errorf("ping data = %v", f.data)
				}
				return
			}
		case <-deadline:
			t.Fatal("no ping within timeout")
		}
	}
}

func TestSendMalformedBody(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sid+"/events", `{"events":`)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
}

// A stored null is dropped only where the recordings show the key omitted
// (#674): session_thread_id on the thread-addressable types, deny_message on
// user.tool_confirmation. Anywhere else it renders as stored, so a writer
// storing a null the SDK forbids — session_thread_id is required on the
// session.thread_* events — shows on the wire instead of being hidden.
func TestNullOmissionIsScopedToTheEvidencedTypes(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	appended, err := events.NewLog(s.pool).Append(context.Background(), domain.ID(sid), []events.NewEvent{
		{Type: domain.EventAgentToolUse, Payload: []byte(`{"name":"bash","input":{},"session_thread_id":null}`)},
		{Type: domain.EventUserToolConfirm,
			Payload: []byte(`{"result":"allow","tool_use_id":"sevt_x","deny_message":null,"session_thread_id":null}`)},
		{Type: domain.EventSessionThreadStatusIdle,
			Payload: []byte(`{"session_thread_id":null,"stop_reason":{"type":"end_turn"}}`)},
		{Type: domain.EventAgentMessage, Payload: []byte(`{"content":[],"deny_message":null}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := eventsByID(t, s, "/v1/sessions/"+sid+"/events")
	for _, tc := range []struct {
		id, key string
		kept    bool
	}{
		{appended[0].ID.String(), "session_thread_id", false},
		{appended[1].ID.String(), "session_thread_id", false},
		{appended[1].ID.String(), "deny_message", false},
		{appended[2].ID.String(), "session_thread_id", true},
		{appended[3].ID.String(), "deny_message", true},
	} {
		ev := listed[tc.id]
		if ev == nil {
			t.Fatalf("event %s missing from the list", tc.id)
		}
		v, ok := ev[tc.key]
		if ok != tc.kept || v != nil {
			t.Errorf("%s %s: present=%v value=%v, want present=%v and null", ev["type"], tc.key, ok, v, tc.kept)
		}
	}
}

func TestEventsCorruptRowRendering(t *testing.T) {
	s := newTestServer(t)
	sid := eventsFixture(t, s)
	echo := sendEvents(t, s, sid, userMessage("fine"))
	ctx := context.Background()

	// A null payload still renders (envelope only)…
	if _, err := s.pool.Exec(ctx, `UPDATE events SET payload = 'null' WHERE id = $1`,
		echo[0]["id"]); err != nil {
		t.Fatal(err)
	}
	status, res := s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	if status != http.StatusOK {
		t.Fatalf("null payload list: %d %v", status, res)
	}
	got := listData(t, res)[0]
	wantExactKeys(t, got, "id", "type", "processed_at")

	// …but a non-object payload is a defect and 500s.
	if _, err := s.pool.Exec(ctx, `UPDATE events SET payload = '[1,2]' WHERE id = $1`,
		echo[0]["id"]); err != nil {
		t.Fatal(err)
	}
	status, res = s.do(http.MethodGet, "/v1/sessions/"+sid+"/events", nil)
	wantErr(t, status, res, http.StatusInternalServerError, "api_error")
}
