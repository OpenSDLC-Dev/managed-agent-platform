package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/openai"
)

// fakeServer speaks just enough of the OpenAI Chat Completions streaming
// protocol to prove the adapter sends a well-formed request and translates the
// stream faithfully. It captures the request and replays a scripted SSE body.
type fakeServer struct {
	t       *testing.T
	sse     []string // each becomes one `data: <s>` SSE frame; a final `data: [DONE]` is appended
	noDone  bool     // suppress the trailing `data: [DONE]` (simulates a cut-off stream)
	gotBody map[string]any
	gotHead http.Header
	status  int
	errBody string
	// echoAuth makes the error body quote the request's Authorization header
	// back, the way some gateways do on a 401 (see TestUpstreamError...).
	echoAuth bool
}

// testAPIKey is the credential start() configures the adapter with, so a test
// can assert an error never quotes it.
const testAPIKey = "sk-test-123"

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	f.gotHead = r.Header.Clone()
	if err := json.NewDecoder(r.Body).Decode(&f.gotBody); err != nil {
		f.t.Errorf("decode request body: %v", err)
	}
	if f.status != 0 {
		body := f.errBody
		if f.echoAuth {
			auth := r.Header.Get("Authorization")
			if auth == "" {
				// Without this the handler would echo "", the body would carry
				// no credential, and a leak assertion would pass vacuously.
				f.t.Fatal("echoAuth: request carried no Authorization header to echo")
			}
			body = `{"error":{"message":"rejected credential ` + auth + `","type":"invalid_request_error"}}`
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(body))
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	for _, data := range f.sse {
		_, _ = w.Write([]byte("data: " + data + "\n\n"))
	}
	if !f.noDone {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
}

func start(t *testing.T, f *fakeServer) provider.Provider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	p, err := openai.New(provider.Config{
		Protocol: "openai",
		Model:    "gpt-4o-mini",
		BaseURL:  srv.URL,
		APIKey:   testAPIKey,
		Headers:  map[string]string{"x-gateway-route": "llm-pool-7"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func collect(t *testing.T, s provider.Stream) []provider.Chunk {
	t.Helper()
	var chunks []provider.Chunk
	for s.Next() {
		chunks = append(chunks, s.Chunk())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	return chunks
}

// TestGenerateFullTurn drives a turn that streams text then a tool call, and
// asserts both the request the adapter produced (Anthropic -> OpenAI) and the
// chunks it translated back (OpenAI -> Anthropic-native).
func TestGenerateFullTurn(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":25,"completion_tokens":17,"total_tokens":42}}`,
	}}
	p := start(t, f)

	stream, err := p.Generate(context.Background(), provider.Request{
		System: "be terse",
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"run ls"}]`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"sure"},{"type":"tool_use","id":"call_prev","name":"bash","input":{"command":"pwd"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"call_prev","content":"/home"}]`)},
		},
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}`),
		},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)

	// --- the request that actually left the adapter ---
	if got := f.gotHead.Get("Authorization"); got != "Bearer sk-test-123" {
		t.Errorf("Authorization = %q, want Bearer sk-test-123", got)
	}
	if got := f.gotHead.Get("x-gateway-route"); got != "llm-pool-7" {
		t.Errorf("extra header lost: %q", got)
	}
	if f.gotBody["model"] != "gpt-4o-mini" {
		t.Errorf("model = %v", f.gotBody["model"])
	}
	if f.gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v", f.gotBody["max_tokens"])
	}
	if f.gotBody["stream"] != true {
		t.Errorf("stream = %v", f.gotBody["stream"])
	}
	if so, ok := f.gotBody["stream_options"].(map[string]any); !ok || so["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage:true", f.gotBody["stream_options"])
	}
	msgs, _ := f.gotBody["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (system + user + assistant + tool)", len(msgs))
	}
	// system prepended
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be terse" {
		t.Errorf("messages[0] = %v, want system/be terse", m0)
	}
	// user text
	m1 := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "run ls" {
		t.Errorf("messages[1] = %v, want user/run ls", m1)
	}
	// assistant text + tool_use -> content + tool_calls (arguments is a JSON string)
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "assistant" || m2["content"] != "sure" {
		t.Errorf("messages[2] role/content = %v", m2)
	}
	tcs, _ := m2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls len = %d, want 1", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["id"] != "call_prev" || tc["type"] != "function" || fn["name"] != "bash" {
		t.Errorf("tool_call = %v", tc)
	}
	if args, ok := fn["arguments"].(string); !ok || args != `{"command":"pwd"}` {
		t.Errorf("tool_call arguments = %v, want the object as a JSON string", fn["arguments"])
	}
	// tool_result -> tool role message keyed by tool_call_id
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_prev" || m3["content"] != "/home" {
		t.Errorf("messages[3] = %v, want tool/call_prev//home", m3)
	}
	// tools -> OpenAI function tools
	tools, _ := f.gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool0 := tools[0].(map[string]any)
	if tool0["type"] != "function" {
		t.Errorf("tool type = %v", tool0["type"])
	}
	tf := tool0["function"].(map[string]any)
	if tf["name"] != "bash" || tf["description"] != "run a command" {
		t.Errorf("tool function = %v", tf)
	}
	if _, ok := tf["parameters"].(map[string]any); !ok {
		t.Errorf("tool parameters missing (input_schema should map to parameters): %v", tf)
	}

	// --- the chunks the adapter translated back ---
	want := []provider.Chunk{
		{Kind: provider.KindTextDelta, Index: 0, Text: "Hel"},
		{Kind: provider.KindTextDelta, Index: 0, Text: "lo"},
	}
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d, want text x2 + tool_use + done", len(chunks))
	}
	for i, w := range want {
		if chunks[i].Kind != w.Kind || chunks[i].Text != w.Text {
			t.Errorf("chunk[%d] = %+v, want %+v", i, chunks[i], w)
		}
	}
	tu := chunks[2]
	if tu.Kind != provider.KindToolUse || tu.ToolUse == nil {
		t.Fatalf("chunk[2] = %+v, want tool_use", tu)
	}
	if tu.ToolUse.ID != "call_9" || tu.ToolUse.Name != "bash" || string(tu.ToolUse.Input) != `{"command":"ls"}` {
		t.Errorf("tool_use = %+v (input %s)", tu.ToolUse, tu.ToolUse.Input)
	}
	done := chunks[len(chunks)-1]
	if done.Kind != provider.KindDone || done.StopReason != "tool_use" {
		t.Errorf("done = %+v, want stop_reason tool_use (mapped from finish_reason tool_calls)", done)
	}
	if done.Usage == nil || done.Usage.InputTokens != 25 || done.Usage.OutputTokens != 17 {
		t.Errorf("done usage = %+v, want in=25 out=17", done.Usage)
	}
}

// For a turn that carried no tool calls, finish_reason maps onto the Anthropic
// vocabulary: only "length" is a truncation; everything else — including a
// "tool_calls" that produced no actual tool call — is a completed turn. (A real
// tool call forces tool_use regardless of finish_reason; see
// TestToolCallForcesToolUse.)
func TestFinishReasonMapping(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "end_turn", "content_filter": "end_turn"}
	for finish, wantStop := range cases {
		f := &fakeServer{sse: []string{
			`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		}}
		p := start(t, f)
		stream, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		chunks := collect(t, stream)
		done := chunks[len(chunks)-1]
		if done.Kind != provider.KindDone || done.StopReason != wantStop {
			t.Errorf("finish_reason %q -> %q, want %q", finish, done.StopReason, wantStop)
		}
	}
}

// The OpenAI-side twin of the anthropic adapter's start-only-usage contract
// (#128): a gateway that reports its reading on an early frame instead of the
// trailing include_usage frame must keep it — the frames that follow carry no
// usage object, and they must not zero what was already reported.
func TestGenerateKeepsUsageReportedBeforeTheFinalFrame(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}],"usage":{"prompt_tokens":31,"completion_tokens":64,"total_tokens":95}}`,
		`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	done := chunks[len(chunks)-1]
	if done.Kind != provider.KindDone || done.StopReason != "end_turn" {
		t.Fatalf("done = %+v", done)
	}
	if done.Usage == nil {
		t.Fatal("done carried no usage, but an early frame reported one")
	}
	if done.Usage.InputTokens != 31 || done.Usage.OutputTokens != 64 {
		t.Errorf("usage = %+v, want in=31 out=64 carried through from the early frame", *done.Usage)
	}
}

// String content (not a block array) must convert too.
func TestStringContentMessage(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"just a string"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	_ = collect(t, stream)
	m0 := f.gotBody["messages"].([]any)[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "just a string" {
		t.Errorf("string-content message = %v", m0)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := openai.New(provider.Config{Protocol: "openai", Model: "m"}); err == nil {
		t.Error("New without base_url should fail")
	}
	if _, err := openai.New(provider.Config{Protocol: "openai", BaseURL: "http://x"}); err == nil {
		t.Error("New without model should fail")
	}
}

func TestUpstreamError(t *testing.T) {
	f := &fakeServer{status: http.StatusUnauthorized, errBody: `{"error":{"message":"bad key","type":"invalid_request_error"}}`}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("Generate against a 401 should return an error")
	}
}

// A gateway that echoes the request's Authorization header into its own
// diagnostic body must not get the credential into the returned error: that
// error becomes a session.error event, which is append-only in Postgres and
// re-served to API clients on every read. The quoted body is what makes a
// misconfiguration diagnosable, so the rest of it must survive.
func TestUpstreamErrorNeverQuotesTheCredential(t *testing.T) {
	f := &fakeServer{status: http.StatusUnauthorized, echoAuth: true}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("Generate against a 401 should return an error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error quotes the credential back: %q", err)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "rejected credential") {
		t.Errorf("redaction destroyed the diagnostic: %q", err)
	}
}

// The error quotes only a bounded prefix of the body, so a credential sitting
// across that boundary would be cut in half and survive redaction as a
// still-revealing fragment.
func TestUpstreamErrorTruncationCannotSplitTheCredential(t *testing.T) {
	// Place the echoed key so that it straddles the 4096-byte quote budget with
	// most of it inside: reading exactly the budget would leave those leading
	// characters in the message, matching no registered secret.
	const budget = 4096
	const inside = 8
	head := `{"error":{"message":"pad `
	f := &fakeServer{
		status:  http.StatusUnauthorized,
		errBody: head + strings.Repeat("x", budget-len(head)-inside) + testAPIKey + `"}}`,
	}
	p := start(t, f)
	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("Generate against a 401 should return an error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error quotes the credential back: %q", err)
	}
	// A truncated key is still a leak: assert no leading run of it survives.
	for n := len(testAPIKey); n > 3; n-- {
		if strings.Contains(err.Error(), testAPIKey[:n]) {
			t.Errorf("error quotes a %d-character prefix of the credential: %q", n, err)
			break
		}
	}
}

// An unparsable base_url is quoted back by the parse error itself, so a
// credential in its userinfo leaks with no endpoint involved at all — and it is
// the one case the redactor cannot reach by parsing the URL.
func TestRequestConstructionErrorNeverQuotesBaseURLCredentials(t *testing.T) {
	const password = "pw-secret-999"
	p, err := openai.New(provider.Config{
		Protocol: "openai",
		Model:    "gpt-4o-mini",
		BaseURL:  "https://user:" + password + "@gw.internal/%zz",
		APIKey:   testAPIKey,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("an unparsable base_url should fail the request")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error quotes the base_url credential back: %q", err)
	}
}

// The same leak arrives under HTTP 200 through a mid-stream error frame — the
// path an operator is least likely to exercise, and unbounded in length.
func TestStreamErrorFrameNeverQuotesTheCredential(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"error":{"message":"upstream rejected Bearer ` + testAPIKey + ` for pool llm-pool-7"}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}
	err = stream.Err()
	if err == nil {
		t.Fatal("an error frame should fail the stream")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("stream error quotes the credential back: %q", err)
	}
	if !strings.Contains(err.Error(), "llm-pool-7") {
		t.Errorf("redaction destroyed the diagnostic: %q", err)
	}
}

// A stream cut off mid-turn — the body ends with neither a finish_reason nor
// the [DONE] terminator — is a truncated turn, not a silent success.
func TestTruncatedStreamFails(t *testing.T) {
	f := &fakeServer{noDone: true, sse: []string{
		`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var chunks []provider.Chunk
	for stream.Next() {
		chunks = append(chunks, stream.Chunk())
	}
	if stream.Err() == nil {
		t.Error("a stream ending before finish_reason must surface an error")
	}
}

// A completed turn drains its tail on Close so the connection can be pooled —
// but an endpoint that keeps writing past its own [DONE] would hold that drain
// forever, and the stall guard cannot end it, because the drain's own reads are
// what feed the guard. The bound has to be the byte limit, and Close must return
// (#121).
func TestCloseDoesNotDrainForeverPastDone(t *testing.T) {
	// Closed before the server is, so a regression fails this test in ten
	// seconds instead of hanging the whole binary: httptest.Server.Close waits
	// for a handler that would otherwise keep writing to a blocked drain.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
		// Past the terminator, forever — until the client hangs up.
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stop:
				return
			default:
			}
			if _, err := fmt.Fprint(w, ": "+strings.Repeat("k", 512)+"\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	p, err := openai.New(provider.Config{
		Protocol: "openai", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey,
		// Long on purpose: the drain must be bounded by its own limit, not by a
		// guard that this very drain keeps re-arming.
		StallTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for stream.Next() { // reaches [DONE] and completes
	}
	if stream.Err() != nil {
		t.Fatalf("stream error: %v", stream.Err())
	}
	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned — the tail drain is unbounded, and it holds the brain's turn (#121)")
	}
}

// An error body that stops mid-credential must not be quoted. Redaction matches
// whole secrets, so half an echoed key survives it — and this package's own
// truncation test defines any prefix longer than three characters as a leak. The
// stall guard is what makes this reachable: a read that used to hang now returns
// what it had (#121).
func TestUpstreamErrorBodyCutMidCredentialIsNotQuoted(t *testing.T) {
	// Everything but the last two characters: long enough that a surviving
	// prefix is unambiguously a leak, short enough that redaction cannot match.
	partial := testAPIKey[:len(testAPIKey)-2]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"rejected credential `+partial)
		fl.Flush()
		select { // never finishes the body
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	const budget = 300 * time.Millisecond
	p, err := openai.New(provider.Config{
		Protocol: "openai", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey, StallTimeout: budget,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type result struct{ err error }
	res := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		})
		res <- result{err}
	}()
	var genErr error
	select {
	case r := <-res:
		genErr = r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Generate never returned — a stalled error body is unbounded (#121)")
	}
	if genErr == nil {
		t.Fatal("Generate against a 401 should return an error")
	}
	if elapsed := time.Since(start); elapsed < budget {
		t.Errorf("Generate failed in %s, inside its own %s budget — something other than the stall bound ended it", elapsed, budget)
	}
	for n := len(partial); n > 3; n-- {
		if strings.Contains(genErr.Error(), partial[:n]) {
			t.Fatalf("error quotes a %d-character prefix of the credential: %q", n, genErr)
		}
	}
	// The status still reaches the operator — dropping the body must not drop
	// the diagnostic that explains a gateway misconfiguration.
	if !strings.Contains(genErr.Error(), "401") {
		t.Errorf("error = %q, want it to still report the status", genErr)
	}
}

// A responsive endpoint's error body must survive the stall bound. The guard is
// on silence, not duration, so an upstream that streams a long diagnostic in
// chunks — each one inside the budget, the whole longer than it — is not stalled
// and its explanation is the most useful thing the operator gets. The wrap that
// makes body bytes progress therefore has to happen before the status is
// examined, the way the anthropic adapter's middleware wraps every response
// (#121).
func TestSlowButResponsiveErrorBodyIsStillQuoted(t *testing.T) {
	const budget = 300 * time.Millisecond
	chunks := []string{`{"error":{"message":"upstream `, `is misconfigured, `, `not silent"}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		for _, c := range chunks {
			_, _ = fmt.Fprint(w, c)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(budget * 6 / 10): // inside the budget, never a stall
			}
		}
	}))
	t.Cleanup(srv.Close)
	p, err := openai.New(provider.Config{
		Protocol: "openai", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey, StallTimeout: budget,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	type result struct{ err error }
	res := make(chan result, 1)
	go func() {
		_, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		})
		res <- result{err}
	}()
	var genErr error
	select {
	case r := <-res:
		genErr = r.err
	case <-time.After(5 * time.Second):
		t.Fatal("Generate never returned")
	}
	if genErr == nil {
		t.Fatal("Generate against a 502 should return an error")
	}
	if want := strings.Join(chunks, ""); !strings.Contains(genErr.Error(), want) {
		t.Errorf("error = %q, want it to quote the whole body %q — the endpoint was slow, not silent", genErr, want)
	}
}
