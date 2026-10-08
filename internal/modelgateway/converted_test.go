package modelgateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
)

// onBoth is a provider of the named profile with both endpoints at url, the
// Anthropic one under /anthropic, and one credential allowed on protos —
// both when none are named.
func onBoth(e *env, prof, url string, protos ...profile.Protocol) store.Provider {
	e.t.Helper()
	p := e.provider(url, func(p *store.Provider) {
		p.Name, p.Profile = prof, prof
		p.Endpoints = map[profile.Protocol]string{profile.Anthropic: url + "/anthropic", profile.OpenAI: url}
	})
	ct, kid, err := e.cipher.Encrypt(e.ctx, []byte("sk-"+prof+"-key1"))
	e.must(err)
	_, err = e.s.CreateCredential(e.ctx, store.Credential{ProviderID: p.ID, Ciphertext: ct, KeyID: kid,
		LastFour: "key1", Weight: 1, Enabled: true, Protocols: protos})
	e.must(err)
	return p
}

// either answers a Messages call at its Anthropic endpoint as message does,
// and a Chat Completions call as chatAnswer does.
func either(text string) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch {
		case strings.HasSuffix(c.Path, "/chat/completions"):
			chatAnswer(text, deepseekStyle)(w, r, c)
		case strings.HasSuffix(c.Path, "/count_tokens"):
			writeBody(w, 200, `{"input_tokens":42}`)
		default:
			message(text)(w, r, c)
		}
	}
}

// A Messages request whose deployment's provider has only an OpenAI
// endpoint is converted: the vendor is sent a Chat Completions request at
// its /chat/completions, with the provider's key as a Bearer and none of the
// caller's anthropic-* headers, and the caller is answered in Messages, its
// model the alias, its usage the ledger's reading — whole, and streamed in
// both vendors' styles, one ending at [DONE] and one, MiniMax-M3's, with no
// [DONE] at all. The ledger row is the caller's: the Messages protocol.
func TestAMessagesRequestIsConverted(t *testing.T) {
	for _, style := range []chatStyle{deepseekStyle, minimaxStyle} {
		e := newEnv(t)
		f := newFake(t, chatAnswer("Jupiter", style))
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL+"/base"), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		cl := e.client(key)

		m, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "fast", MaxTokens: 64,
			System: []anthropic.TextBlockParam{{Text: "be brief"}}, Messages: hello()})
		if err != nil {
			t.Fatal(err)
		}
		if m.Model != "fast" || len(m.Content) != 1 || m.Content[0].Text != "Jupiter" || m.StopReason != "end_turn" ||
			m.Usage.InputTokens != 6 || m.Usage.CacheReadInputTokens != 4 || m.Usage.OutputTokens != 3 {
			t.Errorf("whole answer: %+v", m)
		}
		call := f.recorded()[0]
		want := map[string]any{"model": "deepseek-flash", "max_tokens": float64(64),
			"messages": []any{map[string]any{"role": "system", "content": "be brief"}, map[string]any{"role": "user", "content": "hello"}}}
		var body map[string]any
		_ = json.Unmarshal(call.Raw, &body)
		if call.Path != "/base/chat/completions" || call.Header.Get("Authorization") != "Bearer sk-deepseek-key1" ||
			call.Header.Get("Anthropic-Version") != "" || call.Key != "" || !reflect.DeepEqual(body, want) {
			t.Errorf("upstream call: %s %v %s", call.Path, call.Header, call.Raw)
		}

		s := cl.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello()})
		var acc anthropic.Message
		for s.Next() {
			if err := acc.Accumulate(s.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Err(); err != nil {
			t.Fatalf("style %d: %v", style, err)
		}
		if acc.Model != "fast" || len(acc.Content) != 1 || acc.Content[0].Text != "Jupiter " || acc.StopReason != "end_turn" ||
			acc.Usage.InputTokens != 6 || acc.Usage.CacheReadInputTokens != 4 || acc.Usage.OutputTokens != 3 {
			t.Errorf("style %d: streamed answer: %+v", style, acc)
		}
		if opts := string(f.recorded()[1].Body["stream_options"]); opts != `{"include_usage":true}` {
			t.Errorf("style %d: stream_options %s", style, opts)
		}
		for _, row := range e.ledger() {
			if row.Protocol != string(profile.Anthropic) || row.Endpoint != "messages" || row.Status != 200 ||
				row.Tokens == nil || *row.Tokens != (store.Tokens{Input: 6, Output: 3, CacheRead: 4}) {
				t.Errorf("style %d: ledger row %+v %+v", style, row, row.Tokens)
			}
		}
	}
}

// A credential the provider allows on both protocols passes through; one
// allowed on the OpenAI protocol alone converts.
func TestTheCallersProtocolIsPreferred(t *testing.T) {
	for _, c := range []struct {
		protos []profile.Protocol
		path   string
	}{
		{nil, "/anthropic/v1/messages"},
		{[]profile.Protocol{profile.OpenAI}, "/chat/completions"},
	} {
		e := newEnv(t)
		f := newFake(t, either("hi"))
		e.alias("fast", target(e.deployment(onBoth(e, "deepseek", f.URL, c.protos...), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		if _, err := e.client(key).Messages.New(context.Background(), anthropic.MessageNewParams{Model: "fast", MaxTokens: 8, Messages: hello()}); err != nil {
			t.Fatal(err)
		}
		if got := f.recorded()[0].Path; got != c.path {
			t.Errorf("credential on %v reached %s, want %s", c.protos, got, c.path)
		}
	}
}

// A count has no Chat Completions counterpart: an alias whose upstreams all
// convert answers 404, as a vendor serving no count does, and a client falls
// back to estimating; one with an upstream that passes through counts there.
func TestACountIsNeverConverted(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, either("hi"))
	e.alias("converts", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	e.alias("mixed",
		target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0),
		target(e.deployment(onBoth(e, "minimax", f.URL), "MiniMax-M3"), 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	resp, b := e.do("POST", "/v1/messages/count_tokens", `{"model":"converts","messages":[{"role":"user","content":"hi"}]}`, hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 404 || typ != "not_found_error" || !strings.Contains(msg, "counts tokens") {
		t.Errorf("all converting: %d %s", resp.StatusCode, b)
	}
	resp, b = e.do("POST", "/v1/messages/count_tokens", `{"model":"mixed","messages":[{"role":"user","content":"hi"}]}`, hdr)
	if resp.StatusCode != 200 || string(b) != `{"input_tokens":42}` {
		t.Errorf("mixed: %d %s", resp.StatusCode, b)
	}
	for _, c := range f.recorded() {
		if c.Path != "/anthropic/v1/messages/count_tokens" {
			t.Errorf("a count reached %s", c.Path)
		}
	}
}

// A request the conversion cannot carry goes to the upstreams that pass
// through, and is refused, naming what it cannot carry, where none does.
func TestARequestTheConversionCannotCarry(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, either("hi"))
	e.alias("converts", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	e.alias("falls", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0),
		target(e.deployment(onBoth(e, "minimax", f.URL), "MiniMax-M3"), 1))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	doc := `"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}]}]`
	resp, b := e.do("POST", "/v1/messages", `{"model":"converts","max_tokens":8,`+doc+`}`, hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != 400 || typ != "invalid_request_error" || !strings.HasPrefix(msg, `messages[0].content[0]: a "document" block`) {
		t.Errorf("all converting: %d %s", resp.StatusCode, b)
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d upstream calls for a request refused", n)
	}
	resp, b = e.do("POST", "/v1/messages", `{"model":"falls","max_tokens":8,`+doc+`}`, hdr)
	if calls := f.recorded(); resp.StatusCode != 200 || len(calls) != 1 || calls[0].Path != "/anthropic/v1/messages" {
		t.Errorf("with a passthrough fallback: %d %s, calls %v", resp.StatusCode, b, calls)
	}
}

// What a vendor ignores is judged on the request as converted, for the
// vendor's OpenAI endpoint, and named as the caller wrote it.
func TestAConvertedRequestAVendorWouldIgnore(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, either("hi"))
	e.alias("ds", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	e.alias("m2", target(e.deployment(onOpenAI(e, "minimax", f.URL), "MiniMax-M2.7"), 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	for _, c := range []struct{ model, fields, want string }{
		{"ds", `"tools":[{"name":"t","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`, "tool_choice.disable_parallel_tool_use"},
		{"m2", `"stop_sequences":["x"]`, "stop_sequences"},
		{"m2", `"thinking":{"type":"disabled"}`, "thinking.type"},
	} {
		resp, b := e.do("POST", "/v1/messages", `{"model":"`+c.model+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}],`+c.fields+`}`, hdr)
		if _, msg, _ := errorOf(t, b); resp.StatusCode != 400 || !strings.HasPrefix(msg, c.want+": every upstream") {
			t.Errorf("%s %s: %d %s", c.model, c.fields, resp.StatusCode, b)
		}
	}
	if n := len(f.recorded()); n != 0 {
		t.Errorf("%d upstream calls for requests refused", n)
	}
}

// reasoning answers a Chat Completions call with reasoning, a text and a
// tool call, whole or streamed in DeepSeek's style.
func reasoning(w http.ResponseWriter, _ *http.Request, c fakeCall) {
	var stream bool
	_ = json.Unmarshal(c.Body["stream"], &stream)
	if !stream {
		writeBody(w, 200, fmt.Sprintf(`{"id":"c1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"I should check","content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}]},"finish_reason":"tool_calls"}],"usage":%s}`, c.Model, chatUsage))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, d := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"I should "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"check"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"checking"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_time","arguments":"{\"tz\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"UTC\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":` + chatUsage + `}`,
		`[DONE]`,
	} {
		_, _ = io.WriteString(w, "data: "+d+"\n\n")
		w.(http.Flusher).Flush()
	}
}

// A converted answer's reasoning reaches the caller as a thinking block,
// signed with its deployment's provenance and nothing more; sent back, it
// returns to that deployment as the assistant message's reasoning_content,
// which DeepSeek requires in a tool loop, and to no other deployment.
func TestConvertedReasoningReturnsToItsProducer(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, reasoning)
	ds := e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash")
	other := e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-v4-pro")
	e.alias("fast", target(ds, 0))
	e.alias("other", target(other, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "get_time", InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"tz": map[string]any{"type": "string"}}}}}}

	whole, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello(), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	s := cl.Messages.NewStreaming(context.Background(), anthropic.MessageNewParams{Model: "fast", MaxTokens: 64, Messages: hello(), Tools: tools})
	var streamed anthropic.Message
	for s.Next() {
		if err := streamed.Accumulate(s.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]anthropic.Message{"whole": *whole, "streamed": streamed} {
		if len(m.Content) != 3 || m.Content[0].Type != "thinking" || m.Content[0].Thinking != "I should check" ||
			m.Content[0].Signature != "mapgw1."+ds.ID+"." || m.Content[1].Text != "checking" ||
			m.Content[2].Name != "get_time" || string(m.Content[2].Input) != `{"tz":"UTC"}` || m.StopReason != "tool_use" {
			t.Errorf("%s: %+v", name, m.Content)
		}
	}

	loop := append(hello(), whole.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock("call_1", "noon", false)))
	for _, model := range []string{"fast", "other"} {
		if _, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: model, MaxTokens: 64, Messages: loop, Tools: tools}); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.recorded()
	for i, want := range map[int]string{2: `"I should check"`, 3: ``} {
		var sent struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(calls[i].Raw, &sent)
		if got := string(sent.Messages[1]["reasoning_content"]); got != want || string(sent.Messages[1]["role"]) != `"assistant"` {
			t.Errorf("call %d to %s: assistant turn %v, reasoning_content %s, want %s", i, calls[i].Model, sent.Messages[1], got, want)
		}
	}
}

// An upstream's error reaches a Messages caller in Anthropic's envelope —
// its message, its type where Anthropic has it and else the status's, the
// gateway's request_id — with the call's credentials removed; MiniMax's,
// already in that envelope on its OpenAI endpoint, keeps its own. A stream
// that opens with an error answers as that error's response.
func TestConvertedErrors(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  func(http.ResponseWriter, *http.Request, fakeCall)
		status  int
		typ     string
		message string
	}{
		{"an OpenAI error", status(400, `{"error":{"message":"bad sk-deepseek-key1 here","type":"invalid_request_error","param":null,"code":null}}`),
			400, "invalid_request_error", "bad [redacted] here"},
		{"a type Anthropic lacks", status(422, `{"error":{"message":"unprocessable","type":"unprocessable_entity"}}`), 422, "invalid_request_error", "unprocessable"},
		{"Anthropic's envelope", status(404, `{"type":"error","error":{"type":"not_found_error","message":"no such model"},"request_id":"up_1"}`),
			404, "not_found_error", "no such model"},
		{"a stream opening with an error", func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"error":{"message":"slow down","type":"rate_limit_error"}}`+"\n\n")
		}, 429, "rate_limit_error", "slow down"},
	} {
		e := newEnv(t)
		f := newFake(t, c.answer)
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start(func(cfg *modelgateway.Config) { cfg.MaxAttempts = 1 })
		resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		typ, msg, rid := errorOf(t, b)
		if resp.StatusCode != c.status || typ != c.typ ||
			!strings.Contains(msg, c.message) || rid == "" || rid != resp.Header.Get("request-id") || strings.Contains(string(b), "key1") {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, b)
		}
	}
}

// A converted stream that the upstream breaks off before its finish ends
// with an error event; one that pauses longer than pingEvery holds the
// caller with pings, and keep-alives become pings too.
func TestAConvertedStreamsEnds(t *testing.T) {
	defer modelgateway.SetPingEvery(50 * time.Millisecond)()
	for _, c := range []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"broken off", []string{`{"choices":[{"index":0,"delta":{"content":"par"}}]}`},
			[]string{"message_start", "content_block_start", "content_block_delta", "error"}},
		{"pings", []string{": keep-alive", `{"choices":[{"index":0,"delta":{"content":"h"}}]}`, "pause",
			`{"choices":[{"index":0,"delta":{"content":"i"},"finish_reason":"stop"}]}`, "[DONE]"},
			[]string{"ping", "message_start", "content_block_start", "content_block_delta", "ping", "content_block_delta",
				"content_block_stop", "message_delta", "message_stop"}},
		{"an error partway", []string{`{"choices":[{"index":0,"delta":{"content":"par"}}]}`, `{"error":{"message":"overloaded","type":"overloaded_error"}}`},
			[]string{"message_start", "content_block_start", "content_block_delta", "error"}},
		{"no JSON object partway", []string{`{"choices":[{"index":0,"delta":{"content":"par"}}]}`, `oops`},
			[]string{"message_start", "content_block_start", "content_block_delta", "error"}},
		{"a chunk that cannot be carried", []string{`{"choices":[{"index":0,"delta":{"content":"par"}}]}`, `{"choices":[{"index":1,"delta":{"content":"x"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, "[DONE]"},
			[]string{"message_start", "content_block_start", "content_block_delta", "error"}},
	} {
		e := newEnv(t)
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ch := range c.chunks {
				switch {
				case ch == "pause":
					time.Sleep(200 * time.Millisecond)
				case strings.HasPrefix(ch, ":"):
					_, _ = io.WriteString(w, ch+"\n\n")
				default:
					_, _ = io.WriteString(w, "data: "+ch+"\n\n")
				}
				w.(http.Flusher).Flush()
			}
		})
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		// Consecutive pings are one here: how many the pause holds is the
		// clock's to say.
		var names []string
		for _, l := range strings.Split(string(b), "\n") {
			if n, ok := strings.CutPrefix(l, "event: "); ok && (len(names) == 0 || n != "ping" || names[len(names)-1] != "ping") {
				names = append(names, n)
			}
		}
		if !reflect.DeepEqual(names, c.want) {
			t.Errorf("%s: events %v, want %v\n%s", c.name, names, c.want, b)
		}
		if c.want[len(c.want)-1] != "error" {
			continue
		}
		last := dataLines(b)[len(dataLines(b))-1]
		typ, _, rid := errorOf(t, []byte(last))
		if rows := e.ledger(); rid == "" || len(rows) != 1 || rows[0].ErrorType != typ {
			t.Errorf("%s: error event %s, ledger %+v", c.name, last, rows)
		}
	}
}

// An upstream answer the conversion cannot carry — here a tool call whose
// arguments are not a JSON object — is the gateway's 502, not another paid
// attempt, and the ledger still records the tokens it cost.
func TestAConvertedAnswerThatCannotBeCarried(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 200, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]},"finish_reason":"tool_calls"}],"usage":`+chatUsage+`}`)
	})
	p := onOpenAI(e, "deepseek", f.URL)
	e.credential(p, "sk-deepseek-key2", 1) // where a retry would go
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	typ, msg, _ := errorOf(t, b)
	rows := e.ledger()
	if resp.StatusCode != 502 || typ != "api_error" || !strings.Contains(msg, "arguments are not a JSON object") || len(f.recorded()) != 1 ||
		len(rows) != 1 || rows[0].Tokens == nil || *rows[0].Tokens != (store.Tokens{Input: 6, Output: 3, CacheRead: 4}) {
		t.Errorf("%d %s, %d calls, ledger %+v", resp.StatusCode, b, len(f.recorded()), rows)
	}
}

// An upstream error whose type is not Anthropic's takes the type Anthropic
// gives its status; one of Anthropic's is kept whatever the status, and an
// error already in Anthropic's envelope, as MiniMax's are, is kept as it is.
func TestAConvertedErrorTakesItsStatussType(t *testing.T) {
	type answer struct {
		code int
		body string
	}
	vendor := func(code int) answer { return answer{code, `{"error":{"message":"no","type":"vendor_specific"}}`} }
	for a, want := range map[answer]string{vendor(400): "invalid_request_error", vendor(404): "not_found_error",
		vendor(409): "conflict_error", vendor(413): "request_too_large", vendor(418): "invalid_request_error",
		vendor(429): "rate_limit_error", vendor(500): "api_error", vendor(503): "api_error", vendor(504): "timeout_error",
		vendor(529): "overloaded_error",
		{503, `{"error":{"message":"busy","type":"overloaded_error"}}`}:                                 "overloaded_error",
		{400, `{"type":"error","error":{"type":"bad_request_error","message":"no","http_code":"400"}}`}: "bad_request_error",
	} {
		code := a.code
		e := newEnv(t)
		f := newFake(t, status(code, a.body))
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start(func(cfg *modelgateway.Config) { cfg.MaxAttempts = 1 })
		resp, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		if typ, _, _ := errorOf(t, b); resp.StatusCode != code || typ != want {
			t.Errorf("%d: %d %s, want type %s", code, resp.StatusCode, b, want)
		}
	}
}

// A deployment reached through two credentials, one on each protocol, gets
// back on each only the thinking that protocol produced: a converted
// answer's reasoning, whose signature wraps nothing, as reasoning_content on
// a conversion attempt and never to the vendor's Anthropic endpoint, which
// cannot verify it; a signed block to that endpoint alone. An upstream
// refusing thinking it was not sent is not retried in strip mode.
func TestConvertedReasoningStaysOnItsProtocol(t *testing.T) {
	history := func(dep string) string {
		return fmt.Sprintf(`[{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"from chat","signature":"mapgw1.%[1]s."},{"type":"text","text":"one"}]},
			{"role":"user","content":"next"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"from messages","signature":"mapgw1.%[1]s.sig"},{"type":"text","text":"two"}]},
			{"role":"user","content":"go"}]`, dep)
	}
	send := func(e *env, f *fake, d store.Deployment) []map[string]json.RawMessage {
		key := e.key(everyAlias)
		e.start()
		resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"messages":`+history(d.ID)+`}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		calls := f.recorded()
		if resp.StatusCode != 200 || len(calls) != 1 {
			t.Fatalf("%d %s, %d calls", resp.StatusCode, b, len(calls))
		}
		var sent struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(calls[0].Raw, &sent)
		return sent.Messages
	}

	e := newEnv(t)
	f := newFake(t, either("ok"))
	d := e.deployment(onBoth(e, "deepseek", f.URL, profile.Anthropic), "deepseek-flash")
	e.alias("m", target(d, 0))
	got := send(e, f, d)
	if len(got) != 5 || string(got[1]["content"]) != `[{"type":"text","text":"one"}]` ||
		string(got[3]["content"]) != `[{"signature":"sig","thinking":"from messages","type":"thinking"},{"type":"text","text":"two"}]` {
		t.Errorf("passthrough: sent %s", encodeJSON(got))
	}

	e = newEnv(t)
	f = newFake(t, either("ok"))
	d = e.deployment(onBoth(e, "deepseek", f.URL, profile.OpenAI), "deepseek-flash")
	e.alias("m", target(d, 0))
	got = send(e, f, d)
	if len(got) != 5 || string(got[1]["reasoning_content"]) != `"from chat"` || got[3]["reasoning_content"] != nil {
		t.Errorf("conversion: sent %s", encodeJSON(got))
	}

	e = newEnv(t)
	f = newFake(t, status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid signature in thinking block"}}`))
	d = e.deployment(onBoth(e, "deepseek", f.URL, profile.Anthropic), "deepseek-flash")
	e.alias("m", target(d, 0))
	key := e.key(everyAlias)
	e.start()
	only := fmt.Sprintf(`[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"from chat","signature":"mapgw1.%s."},{"type":"text","text":"one"}]},{"role":"user","content":"go"}]`, d.ID)
	resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"messages":`+only+`}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if resp.StatusCode != 400 || len(f.recorded()) != 1 {
		t.Errorf("a refusal with no thinking sent: %d %s, %d calls", resp.StatusCode, b, len(f.recorded()))
	}
}

func encodeJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// An error partway through a converted stream keeps a type of Anthropic's,
// a credential's included, and otherwise takes the type of the status it
// states, as MiniMax's envelope does in http_code.
func TestAConvertedStreamErrorsType(t *testing.T) {
	for chunk, want := range map[string]string{
		`{"error":{"message":"no","type":"permission_error"}}`:                  "permission_error",
		`{"error":{"message":"slow down","type":"vendor_x","http_code":"429"}}`: "rate_limit_error",
		`{"error":{"message":"broke","type":"vendor_x"}}`:                       "api_error",
		`{"error":{"message":"no","type":"vendor_x","http_code":"401"}}`:        "authentication_error",
		`{"error":{"message":"no","type":"vendor_x","http_code":"402"}}`:        "billing_error",
		`{"error":{"message":"no","type":"vendor_x","http_code":"403"}}`:        "permission_error",
	} {
		e := newEnv(t)
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n"+"data: "+chunk+"\n\n")
		})
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		lines := dataLines(b)
		if typ, _, _ := errorOf(t, []byte(lines[len(lines)-1])); typ != want {
			t.Errorf("%s: the stream ended %s, want type %s", chunk, lines[len(lines)-1], want)
		}
	}
}

// A converted stream pings only while it is idle: an upstream writing more
// often than pingEvery gets none between its events.
func TestAConvertedStreamPingsOnlyWhenIdle(t *testing.T) {
	defer modelgateway.SetPingEvery(400 * time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 40; i++ {
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	})
	e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if n := strings.Count(string(b), "event: ping"); n != 0 || !strings.Contains(string(b), "event: message_stop") {
		t.Errorf("%d pings in a stream that was never idle:\n%s", n, b)
	}
}

// Each vendor is sent thinking in its own words on its OpenAI endpoint:
// MiniMax thinks adaptively when asked to think at all, and DeepSeek, Zhipu
// and Moonshot take adaptive as enabled.
func TestAConvertedRequestsThinkingIsTheVendors(t *testing.T) {
	for _, c := range []struct{ prof, asked, sent string }{
		{"minimax", `{"type":"enabled","budget_tokens":1024}`, `{"type":"adaptive"}`},
		{"minimax", `{"type":"adaptive"}`, `{"type":"adaptive"}`},
		{"deepseek", `{"type":"adaptive"}`, `{"type":"enabled"}`},
		{"deepseek", `{"type":"disabled"}`, `{"type":"disabled"}`},
		{"zhipu", `{"type":"disabled"}`, `{"type":"disabled"}`},
		{"moonshot", `{"type":"adaptive"}`, `{"type":"enabled"}`},
	} {
		e := newEnv(t)
		f := newFake(t, chatAnswer("ok", deepseekStyle))
		e.alias("m", target(e.deployment(onOpenAI(e, c.prof, f.URL), "model-x"), 0))
		key := e.key(everyAlias)
		e.start()
		resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":2048,"thinking":`+c.asked+`,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		calls := f.recorded()
		if resp.StatusCode != 200 || len(calls) != 1 || string(calls[0].Body["thinking"]) != c.sent {
			t.Errorf("%s asked %s: %d %s, sent %s", c.prof, c.asked, resp.StatusCode, b, encodeJSON(calls))
		}
	}
}

// A converted stream's own pings are not the upstream's sign of life: an
// upstream gone silent is cut off at its provider's stall budget all the
// same.
func TestAConvertedStreamStallsThroughItsPings(t *testing.T) {
	defer modelgateway.SetPingEvery(50 * time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	p := e.provider(f.URL, func(p *store.Provider) {
		p.Name, p.Profile, p.StallTimeout = "deepseek", "deepseek", 300*time.Millisecond
		p.Endpoints = map[profile.Protocol]string{profile.OpenAI: f.URL}
	})
	e.credential(p, "sk-deepseek-key1", 1)
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	start := time.Now()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	lines := dataLines(b)
	typ, _, _ := errorOf(t, []byte(lines[len(lines)-1]))
	if took := time.Since(start); typ != "timeout_error" || took > 2*time.Second || !strings.Contains(string(b), "event: ping") {
		t.Errorf("after %s the stream ended %s:\n%s", took, lines[len(lines)-1], b)
	}
}

// A chunk the conversion cannot carry ends the stream for the caller, with
// nothing after its error, while the upstream, still charging, is read on
// for the usage it reports after its finish, which the ledger records.
func TestAnUnconvertibleStreamIsStillCounted(t *testing.T) {
	defer modelgateway.SetPingEvery(20 * time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, ch := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"f","arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			"[DONE]",
		} {
			if i == 3 { // long enough for idle pings, were any still written
				time.Sleep(150 * time.Millisecond)
			}
			_, _ = io.WriteString(w, "data: "+ch+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	body := strings.TrimSpace(string(b))
	last := body[strings.LastIndex(body, "event: "):]
	var rows []store.Usage
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if rows = e.ledger(); len(rows) == 1 {
			break
		}
	}
	if !strings.HasPrefix(last, "event: error") || strings.Count(body, "event: error") != 1 || !strings.Contains(last, "went back to tool call 0") ||
		len(rows) != 1 || rows[0].ErrorType != "api_error" || rows[0].Tokens == nil || *rows[0].Tokens != (store.Tokens{Input: 10, Output: 5}) {
		t.Errorf("stream:\n%s\nledger %+v", b, rows)
	}
}

// A drain that stalls after a chunk the conversion could not carry leaves
// the ledger the error the caller was given.
func TestAStalledDrainKeepsTheCallersError(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n"+
			`data: {"choices":[{"index":1,"delta":{"content":"x"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	p := e.provider(f.URL, func(p *store.Provider) {
		p.Name, p.Profile, p.StallTimeout = "deepseek", "deepseek", 200*time.Millisecond
		p.Endpoints = map[profile.Protocol]string{profile.OpenAI: f.URL}
	})
	e.credential(p, "sk-deepseek-key1", 1)
	e.alias("fast", target(e.deployment(p, "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	var rows []store.Usage
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if rows = e.ledger(); len(rows) == 1 {
			break
		}
	}
	if len(rows) != 1 || rows[0].ErrorType != "api_error" || !strings.Contains(string(b), "could not be converted") {
		t.Errorf("stream:\n%s\nledger %+v", b, rows)
	}
}

// A drain ends at an upstream's error, keep-alives after it or not.
func TestADrainEndsAtTheUpstreamsError(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":1,"delta":{"content":"x"}}]}`+"\n\n"+
			`data: {"error":{"type":"api_error","message":"generation failed"}}`+"\n\n")
		w.(http.Flusher).Flush()
		for i := 0; i < 60; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			_, _ = io.WriteString(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
	})
	e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	start := time.Now()
	_, b := e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	var rows []store.Usage
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if rows = e.ledger(); len(rows) == 1 {
			break
		}
	}
	if took := time.Since(start); took > 1500*time.Millisecond || len(rows) != 1 || rows[0].ErrorType != "api_error" {
		t.Errorf("after %s, stream:\n%s\nledger %+v", took, b, rows)
	}
}

// The newest thinking goes first to the deployment that produced it, on the
// protocol that produced it, the only one it goes back on: a converted
// answer's reasoning to the credential that converts, a signed block to the
// one that passes through, whichever the weights favor.
func TestThinkingGoesFirstToItsProtocol(t *testing.T) {
	for _, c := range []struct {
		sig   string // the signature after the deployment id
		heavy []profile.Protocol
		want  string
	}{
		{"", nil, "/chat/completions"},
		{"sig", []profile.Protocol{profile.OpenAI}, "/anthropic/v1/messages"},
	} {
		e := newEnv(t)
		f := newFake(t, either("ok"))
		p := e.provider(f.URL, func(p *store.Provider) {
			p.Name, p.Profile = "deepseek", "deepseek"
			p.Endpoints = map[profile.Protocol]string{profile.Anthropic: f.URL + "/anthropic", profile.OpenAI: f.URL}
		})
		for i, protos := range [][]profile.Protocol{nil, {profile.OpenAI}} {
			weight := 1
			if slices.Equal(protos, c.heavy) {
				weight = 1000
			}
			ct, kid, err := e.cipher.Encrypt(e.ctx, []byte(fmt.Sprintf("sk-deepseek-key%d", i)))
			e.must(err)
			_, err = e.s.CreateCredential(e.ctx, store.Credential{ProviderID: p.ID, Ciphertext: ct, KeyID: kid,
				LastFour: fmt.Sprintf("key%d", i), Weight: weight, Enabled: true, Protocols: protos})
			e.must(err)
		}
		d := e.deployment(p, "deepseek-flash")
		e.alias("m", target(d, 0))
		key := e.key(everyAlias)
		e.start()
		history := fmt.Sprintf(`[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"mapgw1.%s.%s"},{"type":"text","text":"one"}]},{"role":"user","content":"go"}]`, d.ID, c.sig)
		resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":8,"messages":`+history+`}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		if calls := f.recorded(); resp.StatusCode != 200 || len(calls) != 1 || calls[0].Path != c.want {
			t.Errorf("signature %q: %d %s, calls %v, want %s", c.sig, resp.StatusCode, b, calls, c.want)
		}
	}
}
