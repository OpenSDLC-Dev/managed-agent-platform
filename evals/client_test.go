package evals

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The harness's REST client, hand-rolled over net/http against the public
// surface — no in-process shortcuts. Every call a trial makes goes through the
// same handler, auth and JSON an `ant` CLI would hit, because an eval that
// reached past the wire could not fail when the wire breaks.
//
// It speaks map[string]any rather than the domain structs on purpose: decoding
// into our own types would make a wire regression invisible (a renamed field
// would round-trip through the struct tag on both sides and the eval would
// still pass). Graders assert on the raw JSON the server actually sent.

// restTimeout bounds one non-streaming REST call. A call to the in-process
// server over a local Postgres is effectively instant, so this is not a
// performance budget but a liveness one: without it a control plane wedged on an
// insert would hang the trial until the 60-minute process timeout instead of
// failing it in bounded time. The SSE stream is deliberately not bound this way
// (a request timeout would kill the live tail mid-turn); awaitIdle's own
// deadline bounds the wait once the stream is connected.
const restTimeout = 2 * time.Minute

// do issues an authenticated request and returns the decoded JSON object,
// failing the test on anything that is not a 200 with a JSON body.
func (s *stack) do(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, path, err)
		}
		rd = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(context.Background(), restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.url+path, rd)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("x-api-key", evalKey)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s %s response: %v", method, path, err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d, body %s", method, path, res.StatusCode, raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s %s: response is not a JSON object: %s", method, path, raw)
	}
	return obj
}

// id pulls a resource id out of a create response.
func id(t *testing.T, obj map[string]any, what string) string {
	t.Helper()
	v, _ := obj["id"].(string)
	if v == "" {
		t.Fatalf("%s response carries no id: %v", what, obj)
	}
	return v
}

func (s *stack) createAgent(t *testing.T, body map[string]any) string {
	t.Helper()
	return id(t, s.do(t, http.MethodPost, "/v1/agents", body), "create agent")
}

// createEnvironment omits config, which the server defaults to
// {"type":"cloud"} — and cloud is not a preference here but a requirement:
// queue.Claim hands tool_exec work to a puller only when the environment is
// cloud, so a self_hosted environment would leave every tool call unclaimed and
// the trial would wait out its timeout with no executor ever waking.
func (s *stack) createEnvironment(t *testing.T, name string) string {
	t.Helper()
	return id(t, s.do(t, http.MethodPost, "/v1/environments",
		map[string]any{"name": name}), "create environment")
}

func (s *stack) createSession(t *testing.T, agentID, envID string) string {
	t.Helper()
	sessionID := id(t, s.do(t, http.MethodPost, "/v1/sessions",
		map[string]any{"agent": agentID, "environment_id": envID}), "create session")
	// Remember it for the teardown reap: the session's container (if the agent
	// provisions one) is removed as part of the stack's post-loop cleanup.
	s.mu.Lock()
	s.sessions = append(s.sessions, sessionID)
	s.mu.Unlock()
	return sessionID
}

func (s *stack) sendEvents(t *testing.T, sessionID string, evs ...map[string]any) {
	t.Helper()
	s.do(t, http.MethodPost, "/v1/sessions/"+sessionID+"/events", map[string]any{"events": evs})
}

func (s *stack) getSession(t *testing.T, sessionID string) map[string]any {
	t.Helper()
	return s.do(t, http.MethodGet, "/v1/sessions/"+sessionID, nil)
}

// listEvents reads the whole transcript, following next_page. The page size is
// the event list's documented maximum; a transcript longer than one page is
// normal for a multi-turn trial, and a grader that saw only the first page
// would quietly assert against a truncated history.
func (s *stack) listEvents(t *testing.T, sessionID string) []map[string]any {
	t.Helper()
	var all []map[string]any
	page := ""
	for {
		q := url.Values{"limit": {"1000"}}
		if page != "" {
			q.Set("page", page)
		}
		res := s.do(t, http.MethodGet,
			"/v1/sessions/"+sessionID+"/events?"+q.Encode(), nil)
		data, ok := res["data"].([]any)
		if !ok {
			t.Fatalf("event list has no data array: %v", res)
		}
		for _, e := range data {
			ev, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("event list entry is not an object: %v", e)
			}
			all = append(all, ev)
		}
		next, _ := res["next_page"].(string)
		if next == "" {
			return all
		}
		page = next
	}
}

// tryListEvents fetches a session's transcript best-effort, for the abort path
// where the fatal-on-error listEvents cannot run: it is called from a defer
// unwinding a t.Fatal, and a second t.Fatal there would nest another Goexit. It
// reads a single page and swallows every error — a partial transcript for a
// timed-out trial is worth more than none, and a fetch failure must not mask the
// abort that triggered it. One page suffices: an aborted trial is short by
// definition (it never reached idle), well under the list endpoint's maximum.
func (s *stack) tryListEvents(sessionID string) []map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), restTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.url+"/v1/sessions/"+sessionID+"/events?limit=1000", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", evalKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if json.NewDecoder(res.Body).Decode(&body) != nil {
		return nil
	}
	return body.Data
}

// userMessage is the wire shape the control plane validates for a turn's input:
// exactly {"type","content"}, content blocks of text/image/document.
func userMessage(text string) map[string]any {
	return map[string]any{
		"type":    "user.message",
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

// sseFrame is one decoded `event: <name>` + `data: <json>` frame.
type sseFrame struct {
	name string
	data map[string]any
}

// sseStream is a live tail of a session's event log.
type sseStream struct {
	cancel context.CancelFunc
	frames chan sseFrame
	err    chan error
}

// openStream subscribes before the caller posts anything. The order is
// load-bearing: the stream is a live tail with no cursor on the wire (the
// handler snapshots MAX(seq) at connect and sends only what commits after), so
// a stream opened after the user.message can miss the whole turn including the
// idle the trial is waiting for.
func (s *stack) openStream(t *testing.T, sessionID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.url+"/v1/sessions/"+sessionID+"/events/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("new stream request: %v", err)
	}
	req.Header.Set("x-api-key", evalKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		cancel()
		t.Fatalf("open stream: status %d, body %s", res.StatusCode, body)
	}
	if ct := res.Header.Get("content-type"); !strings.HasPrefix(ct, "text/event-stream") {
		res.Body.Close()
		cancel()
		t.Fatalf("stream content-type = %q, want text/event-stream", ct)
	}

	st := &sseStream{cancel: cancel, frames: make(chan sseFrame, 256), err: make(chan error, 1)}
	t.Cleanup(st.close)
	go func() {
		defer res.Body.Close()
		defer close(st.frames)
		sc := bufio.NewScanner(res.Body)
		// A tool_result carrying a command's whole stdout is far past
		// bufio's 64KiB default line cap, and a scan that stops there would
		// look like a silent stream rather than an oversized frame.
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
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
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			st.err <- err
		}
	}()
	return st
}

func (st *sseStream) close() { st.cancel() }

// awaitIdle consumes frames until the session goes idle, returning that
// session.status_idle event as the server framed it on the stream.
//
// The returned event is the stream's copy rather than a re-read from the list
// endpoint, so a G0 grader asserting "the idle was observed on SSE" is asserting
// on the thing itself: if the stream never delivered it, this returns an error
// instead.
func (st *sseStream) awaitIdle(timeout time.Duration) (map[string]any, error) {
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-st.frames:
			if !ok {
				select {
				case err := <-st.err:
					return nil, fmt.Errorf("stream failed while awaiting idle: %w", err)
				default:
					return nil, fmt.Errorf("stream closed before the session went idle")
				}
			}
			switch f.name {
			case "session.status_idle":
				return f.data, nil
			case "session.deleted":
				return nil, fmt.Errorf("session was deleted before it went idle")
			}
		case <-deadline:
			return nil, fmt.Errorf("no session.status_idle within %s", timeout)
		}
	}
}
