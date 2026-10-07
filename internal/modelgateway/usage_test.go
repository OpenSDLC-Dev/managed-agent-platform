package modelgateway_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// ledger reads every usage row, newest first.
func (e *env) ledger() []store.Usage {
	e.t.Helper()
	rows, _, err := e.s.ListUsage(e.ctx, store.UsageFilter{}, 0, 1000)
	e.must(err)
	return rows
}

// clearOfAMinute waits out the database's current minute when less than
// five seconds of it remain, so the admissions a test counts share one
// window.
func (e *env) clearOfAMinute() {
	var left float64
	e.must(e.pool.QueryRow(e.ctx, `SELECT extract(epoch FROM date_trunc('minute', now()) + interval '1 minute' - now())::float8`).Scan(&left))
	if left < 5 {
		time.Sleep(time.Duration(left*float64(time.Second)) + 100*time.Millisecond)
	}
}

// limitedKey issues a key granted every alias under the limits given.
func (e *env) limitedKey(rpm *int32, tpm *int64) string {
	e.t.Helper()
	key := e.key(everyAlias)
	_, err := e.s.PutKeyPolicy(e.ctx, store.KeyPolicy{APIKeyID: fmt.Sprintf("key_%d", e.keys), RPM: rpm, TPM: tpm})
	e.must(err)
	return key
}

func priced(v float64) *float64 { return &v }

// Each answered request is one row: the name the caller sent and the alias it
// matched, the deployment and credential that answered, the caller's session,
// the route, the status, the tokens the upstream reported costed at the
// deployment's prices, the latency, and for a stream the time its answer
// began. A count reports no tokens. The row's request id is the one the
// caller was given.
func TestTheLedgerRecordsEachAnsweredRequest(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	cred := e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "m", func(d *store.Deployment) {
		d.Prices = store.Prices{Input: priced(3), Output: priced(15)}
	})
	e.alias("fast", target(d, 0))
	e.alias("*", target(d, 0))
	key := e.key(everyAlias)
	e.start()

	hdr := map[string]string{"x-api-key": key, "X-MAP-Session-ID": "sesn_1"}
	whole, _ := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
	e.do("POST", "/v1/messages/count_tokens", `{"model":"claude-sonnet-x","messages":[]}`, map[string]string{"x-api-key": key})

	rows := e.ledger()
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3", len(rows))
	}
	count, stream, w := rows[0], rows[1], rows[2]
	want := store.Tokens{Input: 5, Output: 2}
	if w.RequestID != whole.Header.Get("request-id") || w.APIKeyID != "key_1" || w.Model != "fast" || w.Alias != "fast" ||
		w.DeploymentID != d.ID || w.CredentialID != cred.ID || w.SessionID != "sesn_1" || w.Protocol != "anthropic" ||
		w.Endpoint != "messages" || w.Status != 200 || w.ErrorType != "" || w.Tokens == nil || *w.Tokens != want ||
		w.Cost == nil || math.Abs(*w.Cost-0.000045) > 1e-12 || w.Latency <= 0 || w.TTFT != 0 {
		t.Errorf("the whole answer's row is %+v (tokens %v, cost %v)", w, w.Tokens, w.Cost)
	}
	if stream.Tokens == nil || *stream.Tokens != want || stream.TTFT <= 0 || stream.TTFT > stream.Latency || stream.SessionID != "" ||
		stream.Status != 200 || stream.ErrorType != "" {
		t.Errorf("the stream's row is %+v (tokens %v)", stream, stream.Tokens)
	}
	if count.Endpoint != "count_tokens" || count.Tokens != nil || count.Cost != nil || count.Model != "claude-sonnet-x" || count.Alias != "*" {
		t.Errorf("the count's row is %+v", count)
	}
}

// A stream's usage is message_start's counts, each one message_delta names
// laid over it, as the SDK accumulates them: MiniMax's start reports zeros
// and its delta every count; DeepSeek's report them all in both; Anthropic's
// delta may name its output alone, and a null names nothing.
func TestAStreamsUsageIsItsLastWord(t *testing.T) {
	for name, tc := range map[string]struct {
		start, delta string
		want         *store.Tokens
	}{
		"minimax":  {`{"input_tokens":0,"output_tokens":0}`, `{"input_tokens":1,"output_tokens":3,"cache_read_input_tokens":5859}`, &store.Tokens{Input: 1, Output: 3, CacheRead: 5859}},
		"deepseek": {`{"input_tokens":229,"cache_creation_input_tokens":0,"cache_read_input_tokens":5504,"output_tokens":0}`, `{"input_tokens":229,"cache_creation_input_tokens":0,"cache_read_input_tokens":5504,"output_tokens":64}`, &store.Tokens{Input: 229, Output: 64, CacheRead: 5504}},
		"output":   {`{"input_tokens":7,"cache_creation_input_tokens":3,"output_tokens":1}`, `{"output_tokens":9}`, &store.Tokens{Input: 7, Output: 9, CacheWrite: 3}},
		"null":     {`{"input_tokens":7,"output_tokens":1}`, `{"input_tokens":null,"output_tokens":4}`, &store.Tokens{Input: 7, Output: 4}},
		"none":     {`null`, `{"Output_Tokens":4}`, nil},
		"float":    {`{"input_tokens":7,"output_tokens":1}`, `{"output_tokens":4.5}`, &store.Tokens{Input: 7, Output: 1}},
		"late":     {`"x"`, `{"output_tokens":4}`, &store.Tokens{Output: 4}},
		"negative": {`{"input_tokens":7,"output_tokens":1}`, `{"output_tokens":-3}`, &store.Tokens{Input: 7, Output: 1}},
		"bound":    {`{"input_tokens":7,"output_tokens":1}`, `{"output_tokens":4294967296}`, &store.Tokens{Input: 7, Output: 1 << 32}},
		"past":     {`{"input_tokens":7,"output_tokens":1}`, `{"output_tokens":4294967297}`, &store.Tokens{Input: 7, Output: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"usage\":%s}}\n\n", c.Model, tc.start)
				_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":%s}\n\n", tc.delta)
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			})
			p := e.provider(up.URL)
			e.credential(p, "sk-upstream-1", 1)
			e.alias("fast", target(e.deployment(p, "m"), 0))
			key := e.key(everyAlias)
			e.start()
			e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key})
			got := e.ledger()[0].Tokens
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Errorf("tokens %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A request that fails is recorded with what its caller was given — the
// upstream's status and error type, or the type its status stands for when
// its body names none, or the gateway's own — at the last deployment tried.
// A stream that fails partway is a 200 whose error the caller read in an
// event, its tokens what had been reported.
func TestTheLedgerRecordsWhatTheCallerWasGiven(t *testing.T) {
	for name, tc := range map[string]struct {
		answer  func(http.ResponseWriter, *http.Request, fakeCall)
		stream  bool
		once    bool // not retried, so the first deployment is the last tried
		status  int
		errType string
		tokens  *store.Tokens
	}{
		"overloaded": {answer: status(529, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`), status: 529, errType: "overloaded_error"},
		"invalid":    {answer: status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`), once: true, status: 400, errType: "invalid_request_error"},
		"bare":       {answer: status(429, `<html>slow down</html>`), status: 429, errType: "rate_limit_error"},
		"unmapped":   {answer: status(418, `teapot`), once: true, status: 418, errType: "api_error"},
		"credential": {answer: status(401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`), status: 502, errType: "api_error"},
		"midway": {answer: func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, events(c.Model, "x")[0])
		}, stream: true, status: 200, errType: "api_error", tokens: &store.Tokens{Input: 5, Output: 1}},
		"error event": {answer: func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, events(c.Model, "x")[0])
			_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")
		}, stream: true, status: 200, errType: "overloaded_error", tokens: &store.Tokens{Input: 5, Output: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			first, last := newFake(t, tc.answer), newFake(t, tc.answer)
			p1, p2 := e.provider(first.URL), e.provider(last.URL)
			c1, c2 := e.credential(p1, "sk-upstream-1", 1), e.credential(p2, "sk-upstream-2", 1)
			d1, d2 := e.deployment(p1, "m1"), e.deployment(p2, "m2")
			e.alias("fast", target(d1, 0), target(d2, 1))
			key := e.key(everyAlias)
			e.start()
			body := `{"model":"fast","max_tokens":8,"messages":[]}`
			if tc.stream {
				body = `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`
			}
			resp, b := e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key})
			rows := e.ledger()
			if len(rows) != 1 {
				t.Fatalf("%d rows", len(rows))
			}
			u := rows[0]
			if resp.StatusCode != tc.status || u.Status != tc.status || u.ErrorType != tc.errType {
				t.Errorf("the caller got %d %s; the row says %d %q, want %d %q", resp.StatusCode, b, u.Status, u.ErrorType, tc.status, tc.errType)
			}
			dep, cred := d2.ID, c2.ID
			if tc.stream || tc.once {
				dep, cred = d1.ID, c1.ID
			}
			if u.DeploymentID != dep || u.CredentialID != cred {
				t.Errorf("the row names %s/%s, want the last tried, %s/%s", u.DeploymentID, u.CredentialID, dep, cred)
			}
			if (u.Tokens == nil) != (tc.tokens == nil) || u.Tokens != nil && *u.Tokens != *tc.tokens {
				t.Errorf("tokens %+v, want %+v", u.Tokens, tc.tokens)
			}
		})
	}
}

// A caller that leaves partway through a stream is recorded once the
// upstream's answer is read to its end, with every token it reported.
func TestACallerThatLeavesIsRecordedInFull(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i, ev := range events(c.Model, "slow") {
			if i > 0 {
				time.Sleep(40 * time.Millisecond)
			}
			_, _ = io.WriteString(w, strings.Replace(ev, `"output_tokens":2}`, `"output_tokens":50}`, 1))
			w.(http.Flusher).Flush()
		}
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
	deadline := time.Now().Add(10 * time.Second)
	for len(e.ledger()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	rows := e.ledger()
	if len(rows) != 1 || rows[0].Tokens == nil || rows[0].Tokens.Output != 50 || rows[0].Status != 200 {
		t.Fatalf("the ledger holds %+v", rows)
	}
}

// A ledger that cannot be written costs the caller nothing: the answer
// stands.
func TestALedgerFailureIsNotTheCallers(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	if _, err := e.pool.Exec(e.ctx, `ALTER TABLE modelgateway.usage RENAME TO usage_gone`); err != nil {
		t.Fatal(err)
	}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 || !strings.Contains(string(b), `"ok"`) {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

// A key's RPM refuses its requests past the limit with a 429
// rate_limit_error and a retry-after in whole seconds; the refusals reach no
// upstream and leave no row, and a count is a request like any other. Its
// TPM refuses once the minute's completed tokens reach it. A refusal the
// gateway makes before asking any upstream counts for nothing; a key with
// no limits writes no window.
func TestALimitedKeyIsRefusedPastItsLimit(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	rpm, tpm := int32(2), int64(10)
	byRequests, byTokens, free := e.limitedKey(&rpm, nil), e.limitedKey(nil, &tpm), e.key(everyAlias)
	// The control plane registers the bootstrap key's row; no policy names it.
	_, err := e.pool.Exec(e.ctx, `INSERT INTO api_keys (id, name, key_hash) VALUES ('key_boot', 'bootstrap', $1)`, apikey.Hash(bootstrap))
	e.must(err)
	e.start()
	e.clearOfAMinute()
	send := func(key, path, model string) (*http.Response, []byte) {
		return e.do("POST", path, `{"model":"`+model+`","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	}
	refused := func(resp *http.Response, b []byte, limit string) {
		t.Helper()
		typ, msg, _ := errorOf(t, b)
		ra, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if resp.StatusCode != 429 || typ != "rate_limit_error" || !strings.Contains(msg, limit+" per minute") || err != nil || ra < 1 || ra > 60 {
			t.Errorf("%d %s, retry-after %q; want a 429 naming %s", resp.StatusCode, b, resp.Header.Get("Retry-After"), limit)
		}
	}

	if resp, _ := send(byRequests, "/v1/messages", "nope"); resp.StatusCode != 404 {
		t.Fatalf("an unknown model: %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		if resp, b := send(byRequests, path, "fast"); resp.StatusCode != 200 {
			t.Fatalf("%s within the limit: %d %s", path, resp.StatusCode, b)
		}
	}
	resp, b := send(byRequests, "/v1/messages", "fast")
	refused(resp, b, "2 requests")
	if n := len(up.recorded()); n != 2 {
		t.Errorf("the upstream was called %d times, want the 2 admitted", n)
	}

	// Each answer reports 7 tokens: the first two are admitted under 10, the
	// third is not.
	for i := range 2 {
		if resp, b := send(byTokens, "/v1/messages", "fast"); resp.StatusCode != 200 {
			t.Fatalf("token request %d: %d %s", i, resp.StatusCode, b)
		}
	}
	resp, b = send(byTokens, "/v1/messages", "fast")
	refused(resp, b, "10 tokens")

	for range 3 {
		send(free, "/v1/messages", "fast")
		send(bootstrap, "/v1/messages", "fast")
	}
	if n := len(e.ledger()); n != 2+2+3+3 {
		t.Errorf("%d rows, want one per admitted request", n)
	}
	var windows int
	e.must(e.pool.QueryRow(e.ctx, `SELECT count(*) FROM modelgateway.rate_windows WHERE api_key_id NOT IN ('key_1', 'key_2')`).Scan(&windows))
	if windows != 0 {
		t.Errorf("unlimited keys wrote %d windows", windows)
	}
}

// A session id the ledger cannot hold — longer than its index takes, or not
// UTF-8 — is refused before the key's limits count it, rather than leave the
// request unrecorded; one at the bound is recorded.
func TestASessionIDTheLedgerCannotHoldIsRefused(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	rpm := int32(1)
	key := e.limitedKey(&rpm, nil)
	e.start()
	body := `{"model":"fast","max_tokens":8,"messages":[]}`
	for _, id := range []string{strings.Repeat("s", 257), "sesn_\xff"} {
		resp, b := e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key, "X-MAP-Session-ID": id})
		if typ, msg, _ := errorOf(t, b); resp.StatusCode != 400 || typ != "invalid_request_error" || !strings.Contains(msg, "X-MAP-Session-ID") {
			t.Errorf("%q: %d %s", id, resp.StatusCode, b)
		}
	}
	if n := len(up.recorded()); n != 0 {
		t.Errorf("the upstream was called %d times", n)
	}
	at := strings.Repeat("s", 256)
	if resp, b := e.do("POST", "/v1/messages", body, map[string]string{"x-api-key": key, "X-MAP-Session-ID": at}); resp.StatusCode != 200 {
		t.Fatalf("a session id at the bound: %d %s", resp.StatusCode, b)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].SessionID != at {
		t.Errorf("the ledger holds %+v", rows)
	}
}

// A ledger write slower than the write bound costs the answer nothing: what
// net/http writes once the handler returns gets a bound of its own.
func TestASlowLedgerDoesNotCutTheAnswer(t *testing.T) {
	defer modelgateway.SetWriteStall(300 * time.Millisecond)()
	e := newEnv(t)
	up := newFake(t, message("ok"))
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	tx, err := e.pool.Begin(e.ctx)
	e.must(err)
	_, err = tx.Exec(e.ctx, `LOCK TABLE modelgateway.usage IN ACCESS EXCLUSIVE MODE`)
	e.must(err)
	released := make(chan struct{})
	go func() { defer close(released); time.Sleep(time.Second); _ = tx.Rollback(e.ctx) }()
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	<-released
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"ok"`) {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if rows := e.ledger(); len(rows) != 1 {
		t.Errorf("the ledger holds %d rows, want the one recorded once the lock went", len(rows))
	}
}

// What the ledger takes from a caller or an upstream is made text Postgres
// holds, where either as sent would fail the row: a model name the wildcard
// admits is recorded without its NUL and cut to 256 bytes at a character's
// start, an upstream's error type is cut likewise, and one carrying a NUL — a lone NUL included,
// which would otherwise record no error at all — is read as none, so the
// status's type stands in.
func TestTheLedgerHoldsWhatItIsSent(t *testing.T) {
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch {
		case strings.Contains(string(c.Raw), "refuse"):
			writeBody(w, 400, `{"type":"error","error":{"type":"bad\u0000type","message":"no"}}`)
			return
		case strings.Contains(string(c.Raw), "long"):
			writeBody(w, 400, `{"type":"error","error":{"type":"`+strings.Repeat("e", 300)+`","message":"no"}}`)
			return
		case strings.Contains(string(c.Raw), "lone"):
			sse(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":3}}}\n\n",
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"\\u0000\",\"message\":\"no\"}}\n\n")
			return
		}
		message("ok")(w, r, c)
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("*", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"x\u0000`+strings.Repeat("é", 200)+`","max_tokens":8,"messages":[]}`, hdr); resp.StatusCode != 200 {
		t.Fatalf("a long name under the wildcard: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"metadata":{"user_id":"refuse"},"messages":[]}`, hdr); resp.StatusCode != 400 {
		t.Fatalf("a refusal: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"metadata":{"user_id":"lone"},"messages":[]}`, hdr); resp.StatusCode != 200 {
		t.Fatalf("a stream that fails: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"metadata":{"user_id":"long"},"messages":[]}`, hdr); resp.StatusCode != 400 {
		t.Fatalf("a long error type: %d %s", resp.StatusCode, b)
	}
	rows := e.ledger()
	if len(rows) != 4 {
		t.Fatalf("%d rows, want every request recorded", len(rows))
	}
	if want := "x" + strings.Repeat("é", 127); rows[3].Model != want {
		t.Errorf("model %q (%d bytes), want %q", rows[3].Model, len(rows[3].Model), want)
	}
	if rows[2].ErrorType != "invalid_request_error" {
		t.Errorf("a 400's error type %q, want invalid_request_error", rows[2].ErrorType)
	}
	if rows[1].ErrorType != "api_error" {
		t.Errorf("a stream's error type %q, want api_error", rows[1].ErrorType)
	}
	if want := strings.Repeat("e", 256); rows[0].ErrorType != want {
		t.Errorf("a long error type of %d bytes, want it cut to 256", len(rows[0].ErrorType))
	}
	var errs int64
	e.must(e.pool.QueryRow(e.ctx, `SELECT sum(errors) FROM modelgateway.usage_daily`).Scan(&errs))
	if errs != 3 {
		t.Errorf("the rollups count %d errors, want 3", errs)
	}
}

// A stream's time to first token runs to its first event that is not a
// keep-alive, even when keep-alives past what the gateway holds back began
// the relay before it, and no further.
func TestTimeToFirstTokenSkipsKeepAlives(t *testing.T) {
	e := newEnv(t)
	ping := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, strings.Repeat(ping, 70<<10/len(ping)))
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		for i, ev := range events("m", "ok") {
			_, _ = io.WriteString(w, ev)
			w.(http.Flusher).Flush()
			if i == 0 {
				time.Sleep(700 * time.Millisecond)
			}
		}
	})
	p := e.provider(up.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	if resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].TTFT < 300*time.Millisecond || rows[0].TTFT >= time.Second {
		t.Errorf("the ledger holds %+v, want one row whose time to first token is between 300ms and 1s", rows)
	}
}
