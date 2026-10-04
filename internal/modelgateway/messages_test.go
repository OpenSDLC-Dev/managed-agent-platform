package modelgateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"
)

// The request goes upstream as the caller sent it but for model and the
// credential, and the answer comes back naming the alias.
func TestMessagesPassThrough(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("Hi there"))
	p := e.provider(up.URL, func(p *store.Provider) { p.Headers = map[string]string{"X-Vendor-Route": "a"} })
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()

	msg, err := e.client(key).Messages.New(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello()})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Model != "fast" || msg.Content[0].Text != "Hi there" {
		t.Errorf("message = %s %q", msg.Model, msg.Content[0].Text)
	}

	// An unknown field and an anthropic-beta header go upstream verbatim; the
	// caller's credential and the session header do not.
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[],"future_field":{"n":1.50}}`, map[string]string{
		"x-api-key": key, "anthropic-beta": "some-beta-2026", SessionHeaderForTest: "sesn_1", "Cookie": "c=1",
	})
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"model":"fast"`) {
		t.Fatalf("raw: %d %s", resp.StatusCode, b)
	}
	if rid := resp.Header.Get("request-id"); !strings.HasPrefix(rid, "req_") {
		t.Errorf("request-id = %q", rid)
	}
	calls := up.recorded()
	if len(calls) != 2 {
		t.Fatalf("%d upstream calls", len(calls))
	}
	c := calls[1]
	if c.Path != "/v1/messages" || c.Model != "deepseek-flash" || c.Key != "sk-upstream-1" {
		t.Errorf("upstream call: path %s model %s key %s", c.Path, c.Model, c.Key)
	}
	if string(c.Body["future_field"]) != `{"n":1.50}` {
		t.Errorf("unknown field = %s", c.Body["future_field"])
	}
	if c.Header.Get("anthropic-beta") != "some-beta-2026" || c.Header.Get("anthropic-version") != "2023-06-01" || c.Header.Get("X-Vendor-Route") != "a" {
		t.Errorf("forwarded headers: %v", c.Header)
	}
	for _, h := range []string{SessionHeaderForTest, "Authorization", "Cookie"} {
		if c.Header.Get(h) != "" {
			t.Errorf("%s went upstream", h)
		}
	}
	if calls[0].Header.Get("x-api-key") != "sk-upstream-1" {
		t.Errorf("the caller's key went upstream")
	}
}

const SessionHeaderForTest = modelgateway.SessionHeader

func TestMessagesStream(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("streamed"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()

	stream := e.client(key).Messages.NewStreaming(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello()})
	var acc anthropic.Message
	for stream.Next() {
		if err := acc.Accumulate(stream.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if acc.Model != "fast" || acc.Content[0].Text != "streamed" || acc.StopReason != "end_turn" {
		t.Errorf("accumulated = %s %q %s", acc.Model, acc.Content[0].Text, acc.StopReason)
	}

	// On the wire: every event whole, the comment and the ping kept, only
	// message_start's model changed.
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
	want := events("fast", "streamed")
	got := string(b)
	for i, ev := range want {
		if i == 0 {
			if !strings.Contains(got, `"model":"fast"`) || strings.Contains(got, "deepseek-flash") {
				t.Errorf("message_start not rewritten: %s", got)
			}
			continue
		}
		if !strings.Contains(got, ev) {
			t.Errorf("event %d missing or altered: %q", i, ev)
		}
	}
}

func TestCountTokens(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message(""))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()

	n, err := e.client(key).Messages.CountTokens(e.ctx, anthropic.MessageCountTokensParams{Model: "fast", Messages: hello()})
	if err != nil {
		t.Fatal(err)
	}
	c := up.recorded()[0]
	if n.InputTokens != 42 || c.Path != "/v1/messages/count_tokens" || c.Model != "deepseek-flash" {
		t.Errorf("count %d, upstream %s %s", n.InputTokens, c.Path, c.Model)
	}
	// Where the upstream has none, its 404 is the caller's answer.
	none := newFake(t, status(404, `{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`))
	e2 := newEnv(t)
	p2 := e2.provider(none.URL)
	e2.credential(p2, "sk-upstream-2", 1)
	e2.alias("fast", target(e2.deployment(p2, "m"), 0))
	key2 := e2.key(everyAlias)
	e2.start()
	_, err = e2.client(key2).Messages.CountTokens(e2.ctx, anthropic.MessageCountTokensParams{Model: "fast", Messages: hello()})
	var aerr *anthropic.Error
	if !errors.As(err, &aerr) || aerr.StatusCode != 404 || len(none.recorded()) != 1 {
		t.Errorf("count_tokens without an upstream: %v", err)
	}
}

// A failure the next attempt may cure moves to the next credential, then the
// next deployment; one it cannot is the caller's answer at once.
func TestRetryAndFallback(t *testing.T) {
	e := newEnv(t)
	var firstKeyCalls atomic.Int32
	primary := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		if c.Key == "sk-primary-a" {
			firstKeyCalls.Add(1)
			writeBody(w, 429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		writeBody(w, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})
	backup := newFake(t, message("from backup"))
	pp := e.provider(primary.URL)
	e.credential(pp, "sk-primary-a", 1000)
	e.credential(pp, "sk-primary-b", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(pp, "primary-model"), 0), target(e.deployment(bp, "backup-model"), 1))
	key := e.key(everyAlias)
	e.start()

	msg, err := e.client(key).Messages.New(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello()})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content[0].Text != "from backup" || len(primary.recorded()) != 2 || len(backup.recorded()) != 1 {
		t.Errorf("answer %q after %d primary and %d backup calls", msg.Content[0].Text, len(primary.recorded()), len(backup.recorded()))
	}
	if b := backup.recorded()[0]; b.Model != "backup-model" {
		t.Errorf("backup was sent %s", b.Model)
	}
}

func TestARefusalIsNotRetriedAndHasNoCredential(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"bad key sk-upstream-secret-1 in request"}}`))
	backup := newFake(t, message("never"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-secret-1", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if resp.StatusCode != 400 || strings.Contains(string(b), "sk-upstream-secret-1") || !strings.Contains(string(b), "bad key") {
		t.Errorf("relayed: %d %s", resp.StatusCode, b)
	}
	if len(backup.recorded()) != 0 {
		t.Error("a refusal was retried")
	}
}

// The attempt count is bounded, and the last failure is the answer, with the
// upstream's retry-after.
func TestAttemptsAreBounded(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Retry-After", "7")
		writeBody(w, 503, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	})
	p := e.provider(up.URL)
	for i := range 4 {
		e.credential(p, "sk-upstream-"+string(rune('a'+i))+"xyz", 1)
	}
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.MaxAttempts = 2 })

	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "7" || !strings.Contains(string(b), "down") {
		t.Errorf("answer: %d retry-after %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), b)
	}
	if n := len(up.recorded()); n != 2 {
		t.Errorf("%d attempts, want 2", n)
	}
}

func TestAnUnreachableUpstreamFallsBack(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()
	backup := newFake(t, message("alive"))
	p := e.provider(dead)
	e.credential(p, "sk-dead-1234", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0), target(e.deployment(bp, "m"), 1))
	e.alias("dead", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	if msg, err := e.client(key).Messages.New(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()}); err != nil || msg.Content[0].Text != "alive" {
		t.Errorf("fallback: %v", err)
	}
	resp, b := e.do("POST", "/v1/messages", `{"model":"dead","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 502 || typ != "api_error" || !strings.Contains(msg, "upstream request failed") {
		t.Errorf("no upstream: %d %s", resp.StatusCode, b)
	}
}

// A redirect fails the attempt and is never followed, so the credential
// never reaches its target.
func TestARedirectIsNotFollowed(t *testing.T) {
	e := newEnv(t)
	var reached atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer elsewhere.Close()
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		http.Redirect(w, r, elsewhere.URL+"/v1/messages", http.StatusTemporaryRedirect)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 502 || typ != "api_error" || !strings.Contains(msg, "redirect") || reached.Load() {
		t.Errorf("redirect: %d %s, target reached %t", resp.StatusCode, b, reached.Load())
	}
}

// A stream that ends before its first byte has not begun the caller's
// answer, so another attempt may still give one.
func TestAStreamThatDiesBeforeItsFirstByteIsRetried(t *testing.T) {
	e := newEnv(t)
	dying := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	backup := newFake(t, message("second"))
	p := e.provider(dying.URL)
	e.credential(p, "sk-dying-1", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start()

	stream := e.client(key).Messages.NewStreaming(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()})
	var acc anthropic.Message
	for stream.Next() {
		_ = acc.Accumulate(stream.Current())
	}
	if err := stream.Err(); err != nil || acc.Content[0].Text != "second" {
		t.Errorf("retried stream: %v, %v", acc.Content, err)
	}
}

// After the first byte a failure is the caller's, as an error event.
//
// The event the upstream was partway through is dropped: sent unfinished, it
// would merge with the error event into one malformed block.
func TestAStreamThatDiesMidwayEndsInAnErrorEvent(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, events(c.Model, "x")[0]+"event: content_block_start\ndata: {\"type\":\"content_bl")
		w.(http.Flusher).Flush()
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = buf.Flush()
			conn.Close()
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
	got := string(b)
	if !strings.Contains(got, "event: message_start") || strings.Contains(got, "content_bl") {
		t.Errorf("stream: %s", got)
	}
	_, data, ok := strings.Cut(got, "\n\nevent: error\ndata: ")
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if !ok || json.Unmarshal([]byte(strings.TrimSuffix(data, "\n\n")), &ev) != nil || ev.Type != "error" || ev.Error.Type != "api_error" ||
		ev.RequestID != resp.Header.Get("request-id") {
		t.Errorf("error event: %q", data)
	}
	if len(up.recorded()) != 1 {
		t.Error("a failure after the first byte was retried")
	}
}

// Once message_stop has passed the answer is whole: an upstream that then
// holds its connection until the stall guard trips ends the stream quietly.
func TestAFailureAfterMessageStopIsNotReported(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		streamText(w, c.Model, "done")
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := e.provider(up.URL, func(p *store.Provider) { p.StallTimeout = 150 * time.Millisecond })
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	stream := e.client(key).Messages.NewStreaming(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()})
	var acc anthropic.Message
	for stream.Next() {
		_ = acc.Accumulate(stream.Current())
	}
	if err := stream.Err(); err != nil || acc.Content[0].Text != "done" {
		t.Errorf("a whole answer ended in %v", err)
	}
}

// A refusal whose body breaks off is still a refusal: its status, not the
// broken read, says whether another attempt may cure it.
func TestARefusalWhoseBodyBreaksIsNotRetried(t *testing.T) {
	e := newEnv(t)
	broken := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) { breakOff(w, 400) })
	backup := newFake(t, message("never"))
	p := e.provider(broken.URL)
	e.credential(p, "sk-broken-1", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 400 || typ != "api_error" || !strings.Contains(msg, "broke off") {
		t.Errorf("broken refusal: %d %s", resp.StatusCode, b)
	}
	if len(backup.recorded()) != 0 {
		t.Error("a refusal was retried")
	}
}

// An answer whose body has begun is being paid for: a break past that point is
// the caller's 502, while one before any byte may still try elsewhere.
func TestABrokenAnswerIsRetriedOnlyBeforeItsFirstByte(t *testing.T) {
	e := newEnv(t)
	begun := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) { breakOff(w, 200) })
	empty := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
	backup := newFake(t, message("from backup"))
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	pb := e.provider(begun.URL)
	e.credential(pb, "sk-begun-1", 1)
	pe := e.provider(empty.URL)
	e.credential(pe, "sk-empty-1", 1)
	e.alias("begun", target(e.deployment(pb, "m"), 0), target(e.deployment(bp, "m"), 1))
	e.alias("empty", target(e.deployment(pe, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start()
	if resp, b := e.do("POST", "/v1/messages", `{"model":"begun","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key}); resp.StatusCode != 502 || len(backup.recorded()) != 0 {
		t.Errorf("begun: %d %s, backup called %d times", resp.StatusCode, b, len(backup.recorded()))
	}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"empty","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 || !strings.Contains(string(b), "from backup") {
		t.Errorf("empty: %d %s", resp.StatusCode, b)
	}
}

// breakOff answers status, promises a longer body than it sends, and drops
// the connection partway.
func breakOff(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"type":"error","err`)
	w.(http.Flusher).Flush()
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		conn.Close()
	}
}

// The credential is redacted from what the upstream decoded, so an escape the
// upstream's encoder chose cannot carry it past the match — in an error body
// or in an error event.
func TestAnEscapedCredentialIsRedacted(t *testing.T) {
	e := newEnv(t)
	const secret = `sk-up/stream"key-9`
	escaped := `sk-up\/stream\"key-9`
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		diag := `{"type":"error","error":{"type":"overloaded_error","message":"key ` + escaped + ` is overloaded"},"tried":["` + escaped + `",1.50]}`
		if _, plain := c.Body["plain"]; plain {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, "<p>key "+secret+" is overloaded</p>")
			return
		}
		if !stream {
			writeBody(w, 400, diag)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, events(c.Model, "x")[0]+"event: error\ndata: "+diag+"\n\n")
	})
	p := e.provider(up.URL)
	e.credential(p, secret, 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	for _, extra := range []string{`"stream":false`, `"stream":true`, `"plain":1`} {
		_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,`+extra+`,"messages":[]}`, map[string]string{"x-api-key": key})
		if got := string(b); strings.Contains(got, "sk-up") || !strings.Contains(got, "is overloaded") {
			t.Errorf("%s: %s", extra, got)
		}
	}
}

// An upstream body past the bound is answered by the gateway rather than
// relayed truncated: an error keeps its status, an answer is a 502.
func TestAnUpstreamBodyPastTheBoundIsNotRelayed(t *testing.T) {
	defer modelgateway.SetMaxResponseBody(64)()
	e := newEnv(t)
	long := strings.Repeat("x", 100)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		if c.Model == "refuses" {
			writeBody(w, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"`+long+`"}}`)
			return
		}
		writeBody(w, 200, `{"type":"message","model":"m","content":[{"type":"text","text":"`+long+`"}]}`)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("refuses", target(e.deployment(p, "refuses"), 0))
	e.alias("answers", target(e.deployment(p, "answers"), 0))
	key := e.key(everyAlias)
	e.start()
	for alias, want := range map[string]int{"refuses": 400, "answers": 502} {
		resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
		if typ, msg, _ := errorOf(t, b); resp.StatusCode != want || typ != "api_error" || !strings.Contains(msg, "bound of 64 bytes") {
			t.Errorf("%s: %d %s", alias, resp.StatusCode, b)
		}
	}
}

// A caller that stops reading but keeps its connection open is let go once a
// write to it stalls, and the upstream answer is still read to its end.
func TestACallerThatStopsReadingIsLetGo(t *testing.T) {
	defer modelgateway.SetWriteStall(200 * time.Millisecond)()
	e := newEnv(t)
	finished := make(chan bool, 1)
	chunk := "data: " + strings.Repeat("x", 512<<10) + "\n\n"
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for range 32 {
			if _, err := io.WriteString(w, chunk); err != nil {
				finished <- false
				return
			}
			w.(http.Flusher).Flush()
		}
		finished <- r.Context().Err() == nil
	})
	p := e.provider(up.URL, func(p *store.Provider) { p.StallTimeout = 5 * time.Second })
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	conn, err := net.Dial("tcp", strings.TrimPrefix(e.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: gw\r\nx-api-key: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", key, len(body), body)
	select {
	case ok := <-finished:
		if !ok {
			t.Error("the upstream answer was cut off behind a caller that stopped reading")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the upstream answer never finished")
	}
}

// The write bound is per write: a caller that keeps reading is never cut off,
// however long the stream runs past the bound.
func TestTheWriteBoundDoesNotCutALongStream(t *testing.T) {
	defer modelgateway.SetWriteStall(200 * time.Millisecond)()
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, ev := range events(c.Model, "slow") {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
	if got := string(b); !strings.Contains(got, "event: message_stop") || strings.Contains(got, "event: error") {
		t.Errorf("a long stream was cut: %s", got)
	}
}

// A stall is not retried, though another credential is there: the upstream
// that went quiet may still be generating, and charging for, its answer.
func TestAStalledUpstreamIsATimeout(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	defer close(release)
	silent := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, events(c.Model, "x")[0])
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := e.provider(silent.URL, func(p *store.Provider) { p.StallTimeout = 150 * time.Millisecond })
	e.credential(p, "sk-upstream-1", 1)
	e.credential(p, "sk-upstream-2", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if typ, _, _ := errorOf(t, b); resp.StatusCode != 504 || typ != "timeout_error" || len(silent.recorded()) != 1 {
		t.Errorf("before headers: %d %s after %d calls", resp.StatusCode, b, len(silent.recorded()))
	}
	_, b = e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
	if got := string(b); !strings.Contains(got, "event: error") || !strings.Contains(got, "timeout_error") {
		t.Errorf("mid-stream: %s", got)
	}
}

// A caller that leaves does not end the upstream answer: the gateway reads
// it to the end.
func TestACallerLeavingDoesNotEndTheUpstreamAnswer(t *testing.T) {
	e := newEnv(t)
	finished := make(chan bool, 1)
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		all := events(c.Model, "slow")
		for i, ev := range all {
			if i > 0 {
				time.Sleep(60 * time.Millisecond)
			}
			if _, err := io.WriteString(w, ev); err != nil {
				finished <- false
				return
			}
			w.(http.Flusher).Flush()
		}
		finished <- r.Context().Err() == nil
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()

	ctx, cancel := context.WithCancel(e.ctx)
	req, _ := http.NewRequestWithContext(ctx, "POST", e.url+"/v1/messages", strings.NewReader(`{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`))
	req.Header.Set("x-api-key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case ok := <-finished:
		if !ok {
			t.Error("the upstream answer was cut off when the caller left")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream answer never finished")
	}
}

func TestACredentialThatWillNotOpenMovesOn(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	if _, err := e.s.CreateCredential(e.ctx, store.Credential{ProviderID: p.ID, Ciphertext: []byte("not sealed"), KeyID: "k",
		Weight: 1000, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.alias("broken", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"broken","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 500 || typ != "api_error" || msg != "internal error" {
		t.Errorf("unopenable credential: %d %s", resp.StatusCode, b)
	}
	if len(up.recorded()) != 0 {
		t.Error("a call went out without a credential")
	}
}

func TestMessagesRefusals(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	e.alias("vectors", target(e.deployment(p, "e", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	off := e.deployment(p, "off", func(d *store.Deployment) { d.Enabled = false })
	e.alias("idle", target(off, 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key}
	cases := []struct {
		name, method, path, body string
		status                   int
		typ, msg                 string
	}{
		{"unknown model", "POST", "/v1/messages", `{"model":"nope","messages":[]}`, 404, "not_found_error", "nope"},
		{"embedding model", "POST", "/v1/messages", `{"model":"vectors","messages":[]}`, 400, "invalid_request_error", "serves embedding, not chat"},
		{"no upstream", "POST", "/v1/messages", `{"model":"idle","messages":[]}`, 503, "api_error", "no enabled upstream"},
		{"not JSON", "POST", "/v1/messages", `{`, 400, "invalid_request_error", "JSON"},
		{"no model", "POST", "/v1/messages", `{"messages":[]}`, 400, "invalid_request_error", "model"},
		{"too large", "POST", "/v1/messages", `{"model":"fast","pad":"` + strings.Repeat("a", 33<<20) + `"}`, 413, "request_too_large", "maximum"},
		{"wrong method", "GET", "/v1/messages", "", 405, "invalid_request_error", "POST"},
		{"unknown path", "POST", "/v1/complete", "{}", 404, "not_found_error", "/v1/complete"},
	}
	for _, c := range cases {
		resp, b := e.do(c.method, c.path, c.body, hdr)
		typ, msg, rid := errorOf(t, b)
		if resp.StatusCode != c.status || typ != c.typ || !strings.Contains(msg, c.msg) || rid != resp.Header.Get("request-id") {
			t.Errorf("%s: %d %s %q (request_id %q)", c.name, resp.StatusCode, typ, msg, rid)
		}
	}
	if len(up.recorded()) != 0 {
		t.Errorf("%d refused requests reached the upstream", len(up.recorded()))
	}
	// The /anthropic prefix serves the same routes.
	if resp, _ := e.do("POST", "/anthropic/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, hdr); resp.StatusCode != 200 {
		t.Errorf("prefixed messages: %d", resp.StatusCode)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	e := newEnv(t)
	e.start()
	for name, cfg := range map[string]modelgateway.Config{
		"no catalog":   {Keys: e.pool, Cipher: e.cipher, BootstrapKey: bootstrap},
		"no database":  {Cipher: e.cipher, BootstrapKey: bootstrap},
		"no cipher":    {Keys: e.pool, BootstrapKey: bootstrap},
		"no bootstrap": {Keys: e.pool, Cipher: e.cipher},
	} {
		if name != "no catalog" {
			cfg.Catalog = catalogFor(t, e)
		}
		if _, err := modelgateway.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A failure looking up the caller's key is a 500 that names nothing.
func TestAKeyLookupFailureIsInternal(t *testing.T) {
	e := newEnv(t)
	e.start(func(c *modelgateway.Config) { c.Keys = failingKeys{} })
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast"}`, map[string]string{"x-api-key": "sk-map-api01-x"})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 500 || typ != "api_error" || msg != "internal error" {
		t.Errorf("lookup failure: %d %s", resp.StatusCode, b)
	}
}

type failingKeys struct{}

func (failingKeys) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{} }

type failingRow struct{}

func (failingRow) Scan(...any) error { return errors.New("connection refused") }

// A caller that leaves before the gateway retries ends the request: no
// further attempt is made for an answer nobody will read. The upstream holds
// its failure until the caller has gone, so the retry is decided after the
// caller left whatever the backoff draws, and the test waits out the longest
// backoff there is before counting attempts.
func TestACallerLeavingDuringBackoffEndsTheRetries(t *testing.T) {
	e := newEnv(t)
	first := make(chan struct{}, 2)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		first <- struct{}{}
		time.Sleep(200 * time.Millisecond)
		writeBody(w, 503, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-a1", 1)
	e.credential(p, "sk-upstream-b1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.Backoff = time.Hour })

	ctx, cancel := context.WithCancel(e.ctx)
	req, _ := http.NewRequestWithContext(ctx, "POST", e.url+"/v1/messages", strings.NewReader(`{"model":"fast","max_tokens":8,"messages":[]}`))
	req.Header.Set("x-api-key", key)
	done := make(chan struct{})
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	<-first
	cancel()
	<-done
	time.Sleep(modelgateway.MaxBackoff + 500*time.Millisecond)
	if n := len(up.recorded()); n != 1 {
		t.Errorf("%d attempts after the caller left, want 1", n)
	}
}

// An answer that is not a JSON object passes as it came.
func TestAnAnswerThatIsNotAnObjectPassesUnchanged(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: [1]\n\nevent: message_stop\ndata: {}\n\n")
			return
		}
		writeBody(w, 200, `[1,2]`)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	if _, b := e.do("POST", "/v1/messages", `{"model":"fast"}`, map[string]string{"x-api-key": key}); string(b) != `[1,2]` {
		t.Errorf("whole: %s", b)
	}
	if _, b := e.do("POST", "/v1/messages", `{"model":"fast","stream":true}`, map[string]string{"x-api-key": key}); !strings.Contains(string(b), "data: [1]") {
		t.Errorf("stream: %s", b)
	}
}
