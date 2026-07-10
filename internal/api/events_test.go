package api_test

import (
	"bufio"
	"context"
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

func sendEvents(t *testing.T, s *tserver, sessionID string, evs ...map[string]any) []map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs})
	if status != http.StatusOK {
		t.Fatalf("send events: status %d, body %v", status, res)
	}
	return listData(t, res)
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

	echo := sendEvents(t, s, sid,
		map[string]any{"type": "user.interrupt"},
		map[string]any{"type": "user.tool_confirmation", "result": "deny",
			"tool_use_id": "sevt_tu1", "deny_message": "too risky"},
		map[string]any{"type": "user.custom_tool_result", "custom_tool_use_id": "sevt_ct1",
			"content": []any{map[string]any{"type": "text", "text": "ok"}}, "is_error": false},
		map[string]any{"type": "user.tool_result", "tool_use_id": "sevt_tu2"},
		map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "hi"}}},
		map[string]any{"type": "system.message", "content": []any{map[string]any{"type": "text", "text": "note"}}},
	)
	if len(echo) != 6 {
		t.Fatalf("echoed %d events, want 6", len(echo))
	}

	interrupt := echo[0]
	wantExactKeys(t, interrupt, "id", "type", "processed_at", "session_thread_id")
	if interrupt["session_thread_id"] != nil {
		t.Errorf("session_thread_id = %v, want null", interrupt["session_thread_id"])
	}

	confirm := echo[1]
	wantExactKeys(t, confirm, "id", "type", "result", "tool_use_id", "deny_message", "processed_at", "session_thread_id")
	if confirm["result"] != "deny" || confirm["deny_message"] != "too risky" || confirm["tool_use_id"] != "sevt_tu1" {
		t.Errorf("tool_confirmation echo = %v", confirm)
	}

	custom := echo[2]
	wantExactKeys(t, custom, "id", "type", "custom_tool_use_id", "content", "is_error", "processed_at", "session_thread_id")
	if custom["is_error"] != false || custom["custom_tool_use_id"] != "sevt_ct1" {
		t.Errorf("custom_tool_result echo = %v", custom)
	}

	toolRes := echo[3]
	wantExactKeys(t, toolRes, "id", "type", "tool_use_id", "content", "is_error", "processed_at", "session_thread_id")
	if toolRes["content"] != nil || toolRes["is_error"] != nil {
		t.Errorf("omitted content/is_error should render null: %v", toolRes)
	}

	system := echo[5]
	wantExactKeys(t, system, "id", "type", "content", "processed_at")
	if system["type"] != "system.message" {
		t.Errorf("system echo = %v", system)
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

	// search_result is a tool-result-only block; its source is a plain URL string.
	echo = sendEvents(t, s, sid, map[string]any{
		"type": "user.custom_tool_result", "custom_tool_use_id": "sevt_x",
		"content": []any{map[string]any{
			"type": "search_result", "source": "https://example.com", "title": "hit",
			"content": []any{map[string]any{"type": "text", "text": "body"}}}},
	})
	sr := echo[0]["content"].([]any)[0].(map[string]any)
	if sr["source"] != "https://example.com" || sr["title"] != "hit" {
		t.Errorf("search_result did not round-trip: %v", sr)
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
		{"stream-only type", map[string]any{"events": []any{map[string]any{"type": "event_delta"}}}, "stream-only"},
		{"define_outcome", map[string]any{"events": []any{map[string]any{"type": "user.define_outcome",
			"description": "d", "rubric": map[string]any{}}}}, "not supported in v1"},
		{"unknown event field", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": txt, "contents": txt}}}, "unknown field"},
		{"message without content", map[string]any{"events": []any{map[string]any{"type": "user.message"}}}, "content is required"},
		{"content not array", map[string]any{"events": []any{map[string]any{"type": "user.message",
			"content": "hi"}}}, "array of content blocks"},
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
		{"tool_result on cloud env", map[string]any{"events": []any{map[string]any{
			"type": "user.tool_result", "tool_use_id": "sevt_1"}}}, "self_hosted"},
		{"thread id rejected", map[string]any{"events": []any{map[string]any{
			"type": "user.interrupt", "session_thread_id": "sthr_1"}}}, "threads are deferred"},
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

func TestSendToolResultOnSelfHosted(t *testing.T) {
	s := newTestServer(t)
	sid := selfHostedSession(t, s)
	echo := sendEvents(t, s, sid, map[string]any{
		"type": "user.tool_result", "tool_use_id": "sevt_t1", "is_error": true,
		"content": []any{map[string]any{"type": "text", "text": "exit 1"}},
	})
	if echo[0]["is_error"] != true {
		t.Errorf("is_error = %v", echo[0]["is_error"])
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

	// Default: chronological asc, everything, next_page null.
	status, res := s.do(http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("list: %d %v", status, res)
	}
	all := listData(t, res)
	if len(all) != 6 {
		t.Fatalf("listed %d, want 6", len(all))
	}
	if all[0]["content"].([]any)[0].(map[string]any)["text"] != "m0" {
		t.Errorf("default order is not chronological: first = %v", all[0])
	}
	if np := nextPage(t, res); np != "" {
		t.Errorf("next_page = %q, want null", np)
	}

	// Cursor walk at limit=2: pages of 2/2/2, opaque next_page in between.
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
	if len(walked) != 6 {
		t.Errorf("cursor walk saw %d events, want 6", len(walked))
	}
	for i, ev := range all {
		if walked[i] != ev["id"].(string) {
			t.Errorf("walk[%d] = %s, want %s", i, walked[i], ev["id"])
		}
	}

	// desc reverses.
	_, res = s.do(http.MethodGet, path+"?order=desc", nil)
	desc := listData(t, res)
	if desc[0]["id"] != all[5]["id"] {
		t.Errorf("desc first = %v, want %v", desc[0]["id"], all[5]["id"])
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

	// created_at range: everything strictly after the third event.
	mid := all[2]["id"].(string)
	var midCreated string
	err := s.pool.QueryRow(context.Background(),
		`SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM events WHERE id = $1`,
		mid).Scan(&midCreated)
	if err != nil {
		t.Fatal(err)
	}
	_, res = s.do(http.MethodGet, path+"?created_at[gt]="+midCreated, nil)
	if got := listData(t, res); len(got) != 3 {
		t.Errorf("created_at[gt] mid returned %d, want 3", len(got))
	}
	_, res = s.do(http.MethodGet, path+"?created_at[lte]="+midCreated, nil)
	if got := listData(t, res); len(got) != 3 {
		t.Errorf("created_at[lte] mid returned %d, want 3", len(got))
	}

	// Validation.
	status, res = s.do(http.MethodGet, path+"?order=upside_down", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?limit=0", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?page=@@@", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, path+"?created_at[gte]=yesterday", nil)
	wantErr(t, status, res, http.StatusBadRequest, "invalid_request_error")
	status, res = s.do(http.MethodGet, "/v1/sessions/sesn_missing/events", nil)
	wantErr(t, status, res, http.StatusNotFound, "not_found_error")

	// A time-keyed cursor from another list is rejected, not misread.
	status, res = s.do(http.MethodGet, "/v1/sessions?limit=1", nil)
	if status != http.StatusOK {
		t.Fatal("sessions list failed")
	}
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

	// Batches arrive in order.
	sendEvents(t, s, sid, userMessage("after-2"), map[string]any{"type": "user.interrupt"})
	if f := st.next(t); f.name != "user.message" {
		t.Errorf("frame 2 = %q", f.name)
	}
	if f := st.next(t); f.name != "user.interrupt" {
		t.Errorf("frame 3 = %q", f.name)
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
