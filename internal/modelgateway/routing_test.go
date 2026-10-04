package modelgateway_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/anthropics/anthropic-sdk-go"
)

// sse answers a stream of the given blocks.
func sse(w http.ResponseWriter, blocks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, b := range blocks {
		_, _ = io.WriteString(w, b)
		w.(http.Flusher).Flush()
	}
}

const overloadedEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

// A stream that opens with an error has not begun the caller's answer, so it
// falls back like any refusal, and when nothing serves, the caller gets the
// error as the HTTP response it would have been.
func TestAStreamThatOpensWithAnErrorFallsBack(t *testing.T) {
	e := newEnv(t)
	primary := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		switch c.Model {
		case "odd":
			sse(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"vendor_busy\",\"message\":\"busy\"}}\n\n")
			return
		case "conflict":
			sse(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"conflict_error\",\"message\":\"conflict\"}}\n\n")
			return
		}
		sse(w, ": ping\n\n", "event: ping\ndata: {\"type\":\"ping\"}\n\n", overloadedEvent)
	})
	backup := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		sse(w, append([]string{": warming\n\n"}, events(c.Model, "from backup")...)...)
	})
	pp := e.provider(primary.URL)
	e.credential(pp, "sk-primary-1", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(pp, "m"), 0), target(e.deployment(bp, "m"), 1))
	e.alias("alone", target(e.deployment(pp, "m"), 0))
	e.alias("odd", target(e.deployment(pp, "odd"), 0))
	e.alias("conflict", target(e.deployment(pp, "conflict"), 0))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.MaxAttempts = 1 })

	stream := e.client(key).Messages.NewStreaming(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()})
	var acc anthropic.Message
	for stream.Next() {
		_ = acc.Accumulate(stream.Current())
	}
	if err := stream.Err(); err != nil || acc.Content[0].Text != "from backup" {
		t.Errorf("fallback: %v %v", acc.Content, err)
	}
	// The keep-alive the answering upstream sent before its first event
	// reaches the caller.
	if _, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key}); !strings.HasPrefix(string(b), ": warming\n\n") {
		t.Errorf("held keep-alive: %q", b)
	}
	resp, b := e.do("POST", "/v1/messages", `{"model":"alone","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if typ, _, _ := errorOf(t, b); resp.StatusCode != 529 || typ != "overloaded_error" {
		t.Errorf("nothing serves: %d %s", resp.StatusCode, b)
	}
	// A type Anthropic does not define is a server error.
	resp, b = e.do("POST", "/v1/messages", `{"model":"odd","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if typ, _, _ := errorOf(t, b); resp.StatusCode != 500 || typ != "vendor_busy" {
		t.Errorf("unknown type: %d %s", resp.StatusCode, b)
	}
	// A type the SDK's enum lacks but Anthropic's errors page lists.
	resp, b = e.do("POST", "/v1/messages", `{"model":"conflict","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if typ, _, _ := errorOf(t, b); resp.StatusCode != 409 || typ != "conflict_error" {
		t.Errorf("conflict: %d %s", resp.StatusCode, b)
	}
}

// Keep-alives are held back only so far: past the bound the stream is
// committed to, so an upstream that pings without answering costs bounded
// memory, and a refusal after that reaches the caller as an error event.
func TestHeldKeepAlivesAreBounded(t *testing.T) {
	e := newEnv(t)
	pad := ": " + strings.Repeat("x", 1000) + "\n\n"
	primary := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		sse(w, strings.Repeat(pad, 70), overloadedEvent)
	})
	backup := newFake(t, message("from backup"))
	pp := e.provider(primary.URL)
	e.credential(pp, "sk-primary-1", 1)
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(pp, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if got := string(b); resp.StatusCode != 200 || !strings.HasPrefix(got, pad) || !strings.Contains(got, "event: error") || !strings.Contains(got, "overloaded_error") || len(backup.recorded()) != 0 {
		t.Errorf("%d, %d bytes, backup called %d times", resp.StatusCode, len(b), len(backup.recorded()))
	}
}

// A vendor refusing the gateway's own account is not the caller's
// authentication failing: another credential is tried, and when none serves
// the caller reads a 502 rather than a 401 about its own key.
func TestARefusedCredentialMovesOn(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		switch c.Key {
		case "sk-revoked-1":
			writeBody(w, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		case "sk-broke-1":
			writeBody(w, 402, `{"error":{"message":"Insufficient Balance"}}`)
		case "sk-forbidden-1":
			writeBody(w, 403, `{"type":"error","error":{"type":"permission_error","message":"no access"}}`)
		case "sk-streamauth-1":
			sse(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"bad key\"}}\n\n")
		default:
			message("ok")(w, nil, c)
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-revoked-1", 1000)
	e.credential(p, "sk-good-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	for _, k := range []string{"sk-broke-1", "sk-forbidden-1", "sk-streamauth-1"} {
		q := e.provider(up.URL, func(p *store.Provider) { p.Name = k })
		e.credential(q, k, 1)
		e.alias(k, target(e.deployment(q, "m"), 0))
	}
	key := e.key(everyAlias)
	e.start()
	if msg, err := e.client(key).Messages.New(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()}); err != nil || msg.Content[0].Text != "ok" {
		t.Errorf("moved on: %v", err)
	}
	for _, alias := range []string{"sk-broke-1", "sk-forbidden-1", "sk-streamauth-1"} {
		resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
		want := map[string]string{"sk-broke-1": "(HTTP 402)", "sk-forbidden-1": "(HTTP 403)", "sk-streamauth-1": "(authentication_error)"}[alias]
		if typ, msg, _ := errorOf(t, b); resp.StatusCode != 502 || typ != "api_error" || !strings.Contains(msg, "refused the gateway's credential "+want) {
			t.Errorf("%s: %d %s", alias, resp.StatusCode, b)
		}
	}
}

// The attempt budget is per deployment, so a deployment with more credentials
// than the budget cannot keep the request from its fallback.
func TestFallbackOutlastsAManyCredentialDeployment(t *testing.T) {
	e := newEnv(t)
	down := newFake(t, status(503, `{"type":"error","error":{"type":"api_error","message":"down"}}`))
	backup := newFake(t, message("from backup"))
	dp := e.provider(down.URL)
	for i := range 3 {
		e.credential(dp, fmt.Sprintf("sk-down-%d-xx", i), 1)
	}
	bp := e.provider(backup.URL)
	e.credential(bp, "sk-backup-1", 1)
	e.alias("fast", target(e.deployment(dp, "m"), 0), target(e.deployment(bp, "m"), 1))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.MaxAttempts = 2 })
	msg, err := e.client(key).Messages.New(e.ctx, anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()})
	if err != nil || msg.Content[0].Text != "from backup" || len(down.recorded()) != 2 {
		t.Errorf("answer %v after %d calls to the failing deployment: %v", msg, len(down.recorded()), err)
	}
}

// A stream that ends before message_stop is a broken answer, and says so; one
// whose upstream already reported its own error says nothing more.
func TestAStreamThatEndsEarlySaysSo(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		all := events(c.Model, "cut")
		if c.Model == "reported" {
			sse(w, all[0], overloadedEvent)
			return
		}
		sse(w, all[:5]...)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("cut", target(e.deployment(p, "cut"), 0))
	e.alias("reported", target(e.deployment(p, "reported"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/messages", `{"model":"cut","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if got := string(b); !strings.Contains(got, "event: error") || !strings.Contains(got, "ended before message_stop") {
		t.Errorf("cut: %s", got)
	}
	_, b = e.do("POST", "/v1/messages", `{"model":"reported","max_tokens":8,"stream":true}`, map[string]string{"x-api-key": key})
	if got := string(b); strings.Count(got, "event: error") != 1 || !strings.Contains(got, "overloaded_error") {
		t.Errorf("reported: %s", got)
	}
}

type countingCipher struct {
	secrets.Cipher
	opened atomic.Int32
}

func (c *countingCipher) Decrypt(ctx context.Context, ct []byte, keyID string) ([]byte, error) {
	c.opened.Add(1)
	return c.Cipher.Decrypt(ctx, ct, keyID)
}

// A credential is opened once, not on every model call.
func TestACredentialIsOpenedOnce(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	counting := &countingCipher{Cipher: e.cipher}
	e.start(func(c *modelgateway.Config) { c.Cipher = counting })
	for range 3 {
		if resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
	}
	if n := counting.opened.Load(); n != 1 {
		t.Errorf("opened %d times", n)
	}
}

// A deleted credential's opened key is dropped once a snapshot no longer
// holds the credential, and every other opened key is kept.
func TestADeletedCredentialsKeyIsDropped(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	a := e.credential(p, "sk-first-key-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	q := e.provider(up.URL, func(p *store.Provider) { p.Name = "other" })
	c := e.credential(q, "sk-other-key-1", 1)
	e.alias("other", target(e.deployment(q, "m"), 0))
	key := e.key(everyAlias)
	var cat *catalog.Catalog
	e.start(func(c *modelgateway.Config) { cat = c.Catalog })
	ctx, cancel := context.WithCancel(e.ctx)
	ran := make(chan struct{})
	go func() { defer close(ran); cat.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-ran })
	ask := func(alias string) string {
		before := len(up.recorded())
		if resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
			t.Fatalf("%d %s", resp.StatusCode, b)
		}
		return up.recorded()[before].Key
	}
	ask("fast")
	ask("other")
	if got, want := modelgateway.OpenedKeys(e.handler), sorted(a.ID, c.ID); !slices.Equal(got, want) {
		t.Fatalf("opened %v, want %v", got, want)
	}
	b := e.credential(p, "sk-second-key-1", 1)
	e.must(e.s.DeleteCredential(e.ctx, p.ID, a.ID))
	for deadline := time.Now().Add(10 * time.Second); ask("fast") != "sk-second-key-1"; {
		if time.Now().After(deadline) {
			t.Fatal("the deletion never reached the snapshot")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, want := modelgateway.OpenedKeys(e.handler), sorted(b.ID, c.ID); !slices.Equal(got, want) {
		t.Errorf("opened %v, want %v", got, want)
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}

// The write bound a response leaves on its connection is lifted, by net/http,
// before the connection's next request, so a kept-alive connection outlives
// it; and a small answer keeps its Content-Length.
func TestAKeptAliveConnectionOutlivesTheWriteBound(t *testing.T) {
	defer modelgateway.SetWriteStall(200 * time.Millisecond)()
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	client := &http.Client{Transport: &http.Transport{MaxConnsPerHost: 1}}
	var conns atomic.Int32
	post := func(body string) int {
		req, _ := http.NewRequest("POST", e.url+"/v1/messages", strings.NewReader(body))
		req.Header.Set("x-api-key", key)
		resp, err := client.Do(req.WithContext(withConnCount(e.ctx, &conns)))
		if err != nil {
			t.Fatalf("on the kept-alive connection: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.ContentLength < 0 {
			t.Errorf("%s: chunked, want a Content-Length", body)
		}
		return resp.StatusCode
	}
	if s := post(`{"model":"fast","max_tokens":8}`); s != 200 {
		t.Fatalf("first: %d", s)
	}
	time.Sleep(400 * time.Millisecond)
	if s := post(`{"model":"nope","max_tokens":8}`); s != 404 {
		t.Errorf("second: %d", s)
	}
	if conns.Load() != 1 {
		t.Errorf("%d connections, want the one kept alive", conns.Load())
	}
}

// A caller that half-closes its connection after its request is still
// reading: when the gateway gives up retrying, it writes the last failure
// rather than an empty 200.
func TestAHalfClosedCallerGetsItsAnswer(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		time.Sleep(100 * time.Millisecond)
		writeBody(w, 503, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-a1", 1)
	e.credential(p, "sk-upstream-b1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	conn, err := net.Dial("tcp", strings.TrimPrefix(e.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"model":"fast","max_tokens":8}`
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: gw\r\nx-api-key: %s\r\nContent-Length: %d\r\n\r\n%s", key, len(body), body)
	_ = conn.(*net.TCPConn).CloseWrite()
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(line, " 503 ") {
		t.Errorf("status line %q, %v", line, err)
	}
}

// An upstream's x-should-retry decides, as it does for Anthropic's SDKs, and
// reaches the caller; without one, a 408 or a 409 is worth another attempt.
func TestTheUpstreamsShouldRetryDecides(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		switch c.Model {
		case "never":
			w.Header().Set("X-Should-Retry", "false")
			writeBody(w, 500, `{"type":"error","error":{"type":"api_error","message":"deterministic"}}`)
		case "always":
			w.Header().Set("X-Should-Retry", "true")
			writeBody(w, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"try again"}}`)
		case "conflict":
			writeBody(w, 409, `{"type":"error","error":{"type":"api_error","message":"conflict"}}`)
		default:
			writeBody(w, 408, `{"type":"error","error":{"type":"timeout_error","message":"slow"}}`)
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-a1", 1)
	e.credential(p, "sk-upstream-b1", 1)
	for _, m := range []string{"never", "always", "timeout", "conflict"} {
		e.alias(m, target(e.deployment(p, m), 0))
	}
	key := e.key(everyAlias)
	e.start()
	for alias, want := range map[string]int{"never": 1, "always": 2, "timeout": 2, "conflict": 2} {
		before := len(up.recorded())
		resp, _ := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8}`, map[string]string{"x-api-key": key})
		if n := len(up.recorded()) - before; n != want {
			t.Errorf("%s: %d attempts, want %d", alias, n, want)
		}
		if alias == "never" && resp.Header.Get("X-Should-Retry") != "false" {
			t.Errorf("x-should-retry not relayed")
		}
	}
}

// withConnCount counts the new connections the requests under ctx open.
func withConnCount(ctx context.Context, n *atomic.Int32) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		if !i.Reused {
			n.Add(1)
		}
	}})
}
