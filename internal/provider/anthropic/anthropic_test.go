package anthropic_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
)

// fakeServer speaks just enough Anthropic Messages protocol to prove the
// adapter sends the right request and translates the stream faithfully.
type fakeServer struct {
	t       *testing.T
	sse     []string // data payloads, event name = payload's type field
	gotBody map[string]any
	gotHead http.Header
	status  int
	// echoKey makes the error body quote the request's x-api-key header back,
	// the way some gateways do (see TestGenerateUpstreamErrorNeverQuotes...).
	echoKey bool
}

// testAPIKey is the credential start() configures the adapter with, so a test
// can assert an error never quotes it.
const testAPIKey = "test-key-123"

func (f *fakeServer) handler(w http.ResponseWriter, r *http.Request) {
	f.t.Helper()
	if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	f.gotHead = r.Header.Clone()
	if err := json.NewDecoder(r.Body).Decode(&f.gotBody); err != nil {
		f.t.Errorf("decode request body: %v", err)
	}
	if f.status != 0 {
		msg := "bad key"
		if f.echoKey {
			// Both auth headers are echoed: userinfo in base_url reaches the
			// endpoint as Authorization: Basic, not as x-api-key.
			key := strings.TrimSpace(r.Header.Get("x-api-key") + " " + r.Header.Get("Authorization"))
			if key == "" {
				// Without this the handler would echo "", the body would carry
				// no credential, and a leak assertion would pass vacuously.
				f.t.Fatal("echoKey: request carried no credential header to echo")
			}
			// The body must stay valid JSON: the SDK only keeps a response body
			// it could parse, so an HTML echo would never reach the error text
			// and the assertion would pass for the wrong reason.
			msg = "rejected credential " + key
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"authentication_error","message":%q}}`, msg)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	for _, data := range f.sse {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(data), &m)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, data)
	}
}

func start(t *testing.T, f *fakeServer) provider.Provider {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	p, err := anthropic.New(provider.Config{
		Protocol: "anthropic",
		Model:    "upstream-model",
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

func TestGenerateFullTurn(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":17}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)

	stream, err := p.Generate(context.Background(), provider.Request{
		System: "be terse",
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"run ls"}]`)},
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

	// The request that actually left the adapter.
	if f.gotHead.Get("x-api-key") != "test-key-123" {
		t.Errorf("x-api-key = %q", f.gotHead.Get("x-api-key"))
	}
	if f.gotHead.Get("anthropic-version") == "" {
		t.Error("anthropic-version header missing")
	}
	if f.gotHead.Get("x-gateway-route") != "llm-pool-7" {
		t.Errorf("extra header lost: %q", f.gotHead.Get("x-gateway-route"))
	}
	if f.gotBody["model"] != "upstream-model" {
		t.Errorf("model = %v", f.gotBody["model"])
	}
	if f.gotBody["max_tokens"] != float64(512) {
		t.Errorf("max_tokens = %v", f.gotBody["max_tokens"])
	}
	if f.gotBody["stream"] != true {
		t.Errorf("stream = %v", f.gotBody["stream"])
	}
	system := f.gotBody["system"].([]any)[0].(map[string]any)
	if system["text"] != "be terse" || system["type"] != "text" {
		t.Errorf("system = %v", system)
	}
	msg := f.gotBody["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("message role = %v", msg["role"])
	}
	if msg["content"].([]any)[0].(map[string]any)["text"] != "run ls" {
		t.Errorf("message content did not round-trip: %v", msg["content"])
	}
	tool := f.gotBody["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "bash" || tool["input_schema"].(map[string]any)["type"] != "object" {
		t.Errorf("tool definition did not round-trip: %v", tool)
	}

	// The chunk translation.
	want := []struct {
		kind provider.ChunkKind
		text string
	}{
		{provider.KindThinkingDelta, "pondering"},
		{provider.KindTextDelta, "Hel"},
		{provider.KindTextDelta, "lo"},
		{provider.KindToolUse, ""},
		{provider.KindDone, ""},
	}
	if len(chunks) != len(want) {
		t.Fatalf("got %d chunks (%+v), want %d", len(chunks), chunks, len(want))
	}
	for i, w := range want {
		if chunks[i].Kind != w.kind {
			t.Errorf("chunk %d kind = %s, want %s", i, chunks[i].Kind, w.kind)
		}
		if w.text != "" && chunks[i].Text != w.text {
			t.Errorf("chunk %d text = %q, want %q", i, chunks[i].Text, w.text)
		}
	}
	if chunks[1].Index != 1 || chunks[0].Index != 0 {
		t.Errorf("delta indexes = %d,%d", chunks[0].Index, chunks[1].Index)
	}

	tu := chunks[3].ToolUse
	if tu == nil || tu.ID != "toolu_9" || tu.Name != "bash" {
		t.Fatalf("tool use = %+v", tu)
	}
	var input map[string]any
	if err := json.Unmarshal(tu.Input, &input); err != nil || input["command"] != "ls" {
		t.Errorf("accumulated tool input = %s (%v)", tu.Input, err)
	}

	done := chunks[4]
	if done.StopReason != "tool_use" {
		t.Errorf("stop reason = %q", done.StopReason)
	}
	u := done.Usage
	if u == nil || u.InputTokens != 25 || u.OutputTokens != 17 ||
		u.CacheCreationInputTokens != 2 || u.CacheReadInputTokens != 3 {
		t.Errorf("usage = %+v", u)
	}
}

func TestGenerateEmptyToolInputAndStringContent(t *testing.T) {
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"noop","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)

	// String-form content is valid Anthropic shorthand and passes through
	// verbatim — the adapter never rewrites it.
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"do nothing"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	if len(chunks) != 2 || chunks[0].Kind != provider.KindToolUse {
		t.Fatalf("chunks = %+v", chunks)
	}
	if string(chunks[0].ToolUse.Input) != "{}" {
		t.Errorf("empty tool input = %s, want {}", chunks[0].ToolUse.Input)
	}
	// Default max_tokens applied (the wire field is required).
	if f.gotBody["max_tokens"] == nil || f.gotBody["max_tokens"] == float64(0) {
		t.Errorf("max_tokens defaulted to %v", f.gotBody["max_tokens"])
	}
	content := f.gotBody["messages"].([]any)[0].(map[string]any)["content"]
	if content != "do nothing" {
		t.Errorf("string content should pass through verbatim, got %v", content)
	}
}

func TestGenerateVerbatimPassthrough(t *testing.T) {
	// Fields and tool types unknown to the pinned SDK version must survive
	// to the wire byte-preserved: round-tripping through the SDK's typed
	// variants would silently drop anything the SDK doesn't model yet.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_7","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)

	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi","future_block_field":{"nested":1}}]`)},
		},
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"bash","input_schema":{"type":"object"},"future_tool_field":"kept"}`),
			json.RawMessage(`{"type":"web_search_20990101","name":"search","max_uses":3}`),
		},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	collect(t, stream)

	block := f.gotBody["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if nested, _ := block["future_block_field"].(map[string]any); nested == nil || nested["nested"] != float64(1) {
		t.Errorf("unknown content-block field dropped: %v", block)
	}
	tools := f.gotBody["tools"].([]any)
	if tools[0].(map[string]any)["future_tool_field"] != "kept" {
		t.Errorf("unknown tool field dropped: %v", tools[0])
	}
	if tools[1].(map[string]any)["type"] != "web_search_20990101" || tools[1].(map[string]any)["max_uses"] != float64(3) {
		t.Errorf("unknown tool type mangled: %v", tools[1])
	}
}

func TestGenerateToolInputOnStartBlock(t *testing.T) {
	// Non-Anthropic gateways may deliver the complete tool input on the
	// start block with no input_json_delta frames at all.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_3","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_5","name":"read","input":{"path":"/tmp/x"}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"read it"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	if len(chunks) != 2 || chunks[0].Kind != provider.KindToolUse {
		t.Fatalf("chunks = %+v", chunks)
	}
	var input map[string]any
	if err := json.Unmarshal(chunks[0].ToolUse.Input, &input); err != nil || input["path"] != "/tmp/x" {
		t.Errorf("start-block tool input dropped: %s", chunks[0].ToolUse.Input)
	}
}

func TestGenerateUpstreamError(t *testing.T) {
	f := &fakeServer{status: http.StatusUnauthorized}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		// Some SDK versions defer the error to the first stream read.
		if stream.Next() {
			t.Fatal("stream yielded a chunk from a 401 response")
		}
		err = stream.Err()
	}
	if err == nil {
		t.Fatal("401 upstream produced no error")
	}
}

// generateErr runs one turn and returns whichever error surfaced — the SDK may
// report an upstream failure from Generate or defer it to the first read.
func generateErr(t *testing.T, p provider.Provider) error {
	t.Helper()
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Next() {
	}
	return stream.Err()
}

// A gateway that echoes the request's x-api-key into its own diagnostic body
// must not get the credential into the returned error: that error becomes a
// session.error event, which is append-only in Postgres and re-served to API
// clients on every read. The rest of the body must survive — it is what makes
// a misconfiguration diagnosable.
func TestGenerateUpstreamErrorNeverQuotesTheCredential(t *testing.T) {
	p := start(t, &fakeServer{status: http.StatusUnauthorized, echoKey: true})
	err := generateErr(t, p)
	if err == nil {
		t.Fatal("401 upstream produced no error")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("error quotes the credential back: %q", err)
	}
	if !strings.Contains(err.Error(), "rejected credential") {
		t.Errorf("redaction destroyed the diagnostic: %q", err)
	}
}

// The same leak arrives under HTTP 200 through a mid-stream error event. It
// surfaces from Err() after Next(), not from Generate, so a fix applied only
// where Generate returns would leave this path live.
func TestStreamErrorEventNeverQuotesTheCredential(t *testing.T) {
	p := start(t, &fakeServer{sse: []string{
		`{"type":"error","error":{"type":"overloaded_error","message":"key ` + testAPIKey + ` exhausted in pool llm-pool-7"}}`,
	}})
	err := generateErr(t, p)
	if err == nil {
		t.Fatal("an error event should fail the stream")
	}
	if strings.Contains(err.Error(), testAPIKey) {
		t.Errorf("stream error quotes the credential back: %q", err)
	}
	if !strings.Contains(err.Error(), "llm-pool-7") {
		t.Errorf("redaction destroyed the diagnostic: %q", err)
	}
}

// A credential in base_url's userinfo needs no cooperation from the endpoint
// at all: the SDK formats the request URL into every API error with String(),
// not Redacted().
//
// The passwords are chosen for how they *render*, which is the whole
// difficulty: url.Parse stores a password decoded while String() re-encodes it,
// so a password made only of URL-safe characters is the one case where the two
// forms coincide — testing just that would pass while every password needing an
// escape leaked.
func TestErrorNeverQuotesBaseURLCredentials(t *testing.T) {
	passwords := []struct {
		name     string
		password string
	}{
		{"url-safe, renders unchanged", "pw-secret-456"},
		{"needs escaping, renders encoded", "p@ss-w0rd"},
		{"partly escaped, renders in a third form", "a/b+c=d"},
	}
	for _, tc := range passwords {
		f := &fakeServer{t: t, status: http.StatusInternalServerError}
		srv := httptest.NewServer(http.HandlerFunc(f.handler))
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatalf("%s: parse server URL: %v", tc.name, err)
		}
		u.User = url.UserPassword("gateway-user", tc.password)

		p, err := anthropic.New(provider.Config{
			Protocol: "anthropic",
			Model:    "upstream-model",
			BaseURL:  u.String(),
			APIKey:   testAPIKey,
		})
		if err != nil {
			t.Fatalf("%s: New: %v", tc.name, err)
		}
		err = generateErr(t, p)
		if err == nil {
			t.Fatalf("%s: 500 upstream produced no error", tc.name)
		}
		// Both renderings must be gone: the error carries the encoded form, but
		// a body echo would carry the decoded one.
		if strings.Contains(err.Error(), tc.password) {
			t.Errorf("%s: error quotes the base_url credential back: %q", tc.name, err)
		}
		if encoded := strings.TrimPrefix(u.User.String(), "gateway-user:"); strings.Contains(err.Error(), encoded) {
			t.Errorf("%s: error quotes the encoded base_url credential back: %q", tc.name, err)
		}
		if !strings.Contains(err.Error(), "gateway-user") {
			t.Errorf("%s: redaction destroyed the diagnostic: %q", tc.name, err)
		}
		srv.Close()
	}
}

// net/http derives an Authorization: Basic header from base_url's userinfo
// whenever the request carries none — always here, since the Anthropic protocol
// authenticates with x-api-key. So an endpoint that echoes its auth header back
// quotes the credential base64-encoded, in none of the forms the URL renders.
func TestErrorNeverQuotesBaseURLCredentialsAsBasicAuth(t *testing.T) {
	const user, password = "gw-user", "pw-secret-999"
	f := &fakeServer{t: t, status: http.StatusUnauthorized, echoKey: true}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	u.User = url.UserPassword(user, password)

	p, err := anthropic.New(provider.Config{
		Protocol: "anthropic",
		Model:    "upstream-model",
		BaseURL:  u.String(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = generateErr(t, p)
	if err == nil {
		t.Fatal("401 upstream produced no error")
	}
	basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	if !strings.Contains(f.gotHead.Get("Authorization"), basic) {
		t.Fatalf("precondition: request did not carry basic auth, got %q", f.gotHead.Get("Authorization"))
	}
	if strings.Contains(err.Error(), basic) {
		t.Errorf("error quotes the base64 credential back: %q", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error quotes the credential back: %q", err)
	}
}

func TestGenerateRejectsInvalidRequestJSON(t *testing.T) {
	// Verbatim passthrough still fails fast on structurally invalid JSON —
	// with the index in the error, before anything reaches the endpoint.
	p := start(t, &fakeServer{})

	_, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`{broken`)}},
	})
	if err == nil || !strings.Contains(err.Error(), "messages[0]") {
		t.Errorf("invalid message content error = %v, want messages[0] context", err)
	}

	_, err = p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools:    []json.RawMessage{json.RawMessage(`{"name":"ok","input_schema":{}}`), json.RawMessage(`{broken`)},
	})
	if err == nil || !strings.Contains(err.Error(), "tools[1]") {
		t.Errorf("invalid tool error = %v, want tools[1] context", err)
	}
}

func TestGenerateParallelToolUseInputsStayIntact(t *testing.T) {
	// Two tool_use blocks in one turn (parallel tool use): the first
	// emitted ToolUse.Input must survive the second block's accumulation —
	// the accumulator's backing array is reused between blocks.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_5","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls -la\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b","name":"bash","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"go"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream) // full drain BEFORE inspecting inputs
	if len(chunks) != 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if got := string(chunks[0].ToolUse.Input); got != `{"command":"ls -la"}` {
		t.Errorf("first tool input corrupted after drain: %s", got)
	}
	if got := string(chunks[1].ToolUse.Input); got != `{"command":"pwd"}` {
		t.Errorf("second tool input = %s", got)
	}
}

func TestGenerateNeverSendsAmbientCredentials(t *testing.T) {
	// The SDK autoloads ANTHROPIC_* env credentials by default; a provider
	// pointed at a third-party gateway must never forward the operator's
	// real Anthropic token.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-secret-never-send")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key-never-send")

	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_6","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	collect(t, stream)

	if got := f.gotHead.Get("authorization"); got != "" {
		t.Errorf("ambient bearer token leaked to the endpoint: %q", got)
	}
	if got := f.gotHead.Get("x-api-key"); got != "test-key-123" {
		t.Errorf("x-api-key = %q, want only the configured key", got)
	}
}

func TestGenerateMalformedStreams(t *testing.T) {
	base := `{"type":"message_start","message":{"id":"msg_7","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`
	cases := []struct {
		name string
		sse  []string
	}{
		{"overlapping tool blocks", []string{base,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"bash","input":{}}}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b","name":"bash","input":{}}}`,
		}},
		{"tool block never closed", []string{base,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"bash","input":{}}}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		}},
		{"missing message_delta", []string{base,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_stop"}`,
		}},
	}
	for _, tc := range cases {
		f := &fakeServer{sse: tc.sse}
		p := start(t, f)
		stream, err := p.Generate(context.Background(), provider.Request{
			Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"go"`)}},
		})
		if err != nil {
			t.Fatalf("%s: Generate: %v", tc.name, err)
		}
		var sawDone bool
		for stream.Next() {
			if stream.Chunk().Kind == provider.KindDone {
				sawDone = true
			}
		}
		if sawDone || stream.Err() == nil {
			t.Errorf("%s: done=%v err=%v — malformed streams must fail loudly", tc.name, sawDone, stream.Err())
		}
	}
}

func TestGenerateUsageSurvivesSparseDeltas(t *testing.T) {
	// A second message_delta without a usage object must not zero the
	// counters an earlier frame reported.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_8","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":42,"cache_read_input_tokens":9}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{}}`,
		`{"type":"message_stop"}`,
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
	if done.Usage.OutputTokens != 42 || done.Usage.CacheReadInputTokens != 9 || done.Usage.InputTokens != 11 {
		t.Errorf("usage zeroed by sparse delta: %+v", done.Usage)
	}
}

func TestGenerateStartBlockInputPreservesBigIntegers(t *testing.T) {
	// The start-block seed passes through as raw bytes: re-encoding a
	// decoded value would round 9007199254740993 through float64.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_c","name":"close_issue","input":{"issue_id":9007199254740993}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"close it"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	chunks := collect(t, stream)
	if !strings.Contains(string(chunks[0].ToolUse.Input), "9007199254740993") {
		t.Errorf("big integer mangled in seed passthrough: %s", chunks[0].ToolUse.Input)
	}
}

func TestGenerateTruncatedStream(t *testing.T) {
	// A stream that drains without message_stop is a truncated turn and
	// must surface as an error, never as a quiet success.
	f := &fakeServer{sse: []string{
		`{"type":"message_start","message":{"id":"msg_4","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut o"}}`,
	}}
	p := start(t, f)
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var sawDone bool
	for stream.Next() {
		if stream.Chunk().Kind == provider.KindDone {
			sawDone = true
		}
	}
	if sawDone {
		t.Error("truncated stream produced a done chunk")
	}
	if stream.Err() == nil {
		t.Error("truncated stream must surface an error")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := anthropic.New(provider.Config{Model: "m"}); err == nil {
		t.Error("missing base_url should error — no silent api.anthropic.com fallback")
	}
	if _, err := anthropic.New(provider.Config{BaseURL: "http://x"}); err == nil {
		t.Error("missing model should error")
	}
}
