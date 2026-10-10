package modelgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"
)

// onGemini is a provider of the gemini profile with its endpoint at url's
// /v1beta, and one credential, which defaults to the Gemini protocol.
func onGemini(e *env, url string) store.Provider {
	e.t.Helper()
	p := e.provider(url, func(p *store.Provider) {
		p.Name, p.Profile = "gemini", "gemini"
		p.Endpoints = map[profile.Protocol]string{profile.Gemini: url + "/v1beta"}
	})
	e.credential(p, "AIza-gemini-key1", 1)
	return p
}

// geminiUsage is the usage the fake reports: ten prompt tokens of which four
// were cached, three candidate tokens and five thought tokens.
const geminiUsage = `{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":5,"cachedContentTokenCount":4,"totalTokenCount":18}`

// geminiText answers text, its signature on its part as Gemini sets it.
func geminiText(text string) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 200, `{"candidates":[{"content":{"role":"model","parts":[{"text":`+encodeJSON(text)+`,"thoughtSignature":"dGV4dA=="}]},`+
			`"finishReason":"STOP"}],"usageMetadata":`+geminiUsage+`,"responseId":"resp-1"}`)
	}
}

// geminiLoop calls get_time, signed, on a request's first turn, and answers
// text once a tool result comes back.
func geminiLoop(w http.ResponseWriter, r *http.Request, c fakeCall) {
	var contents []json.RawMessage
	_ = json.Unmarshal(c.Body["contents"], &contents)
	if len(contents) > 1 {
		geminiText("noon it is")(w, r, c)
		return
	}
	writeBody(w, 200, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"get_time","args":{"tz":"UTC"}},`+
		`"thoughtSignature":"c2lnLTE="}]},"finishReason":"STOP"}],"usageMetadata":`+geminiUsage+`}`)
}

// A Messages request on an alias whose credential speaks only Gemini goes to
// the deployment's generateContent, the model in the path, the key as
// x-goog-api-key and none of the caller's headers, converted; the answer
// comes back in Messages, its usage the ledger's reading, its thinking
// declared prefix-unchecked; the ledger row is the caller's.
func TestAMessagesRequestReachesGemini(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, geminiText("Jupiter"))
	e.alias("g", target(e.deployment(onGemini(e, f.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()

	m, err := e.client(key).Messages.New(context.Background(), anthropic.MessageNewParams{Model: "g", MaxTokens: 64,
		System: []anthropic.TextBlockParam{{Text: "be brief"}}, Messages: hello()})
	if err != nil {
		t.Fatal(err)
	}
	if m.Model != "g" || m.ID != "resp-1" || len(m.Content) != 1 || m.Content[0].Text != "Jupiter" || m.StopReason != "end_turn" ||
		m.Usage.InputTokens != 6 || m.Usage.CacheReadInputTokens != 4 || m.Usage.OutputTokens != 8 {
		t.Errorf("answer: %+v", m)
	}
	call := f.recorded()[0]
	var body any
	_ = json.Unmarshal(call.Raw, &body)
	want := decodedJSON(t, `{"systemInstruction":{"parts":[{"text":"be brief"}]},"contents":[{"role":"user","parts":[{"text":"hello"}]}],
		"generationConfig":{"maxOutputTokens":64}}`)
	if call.Path != "/v1beta/models/gemini-3.8-flash:generateContent" || call.Header.Get("x-goog-api-key") != "AIza-gemini-key1" ||
		call.Key != "" || call.Header.Get("Authorization") != "" || call.Header.Get("Anthropic-Version") != "" || !reflect.DeepEqual(body, want) {
		t.Errorf("upstream call: %s %v %s", call.Path, call.Header, call.Raw)
	}

	resp, _ := e.do("POST", "/v1/messages", `{"model":"g","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if resp.StatusCode != 200 || resp.Header.Get(provider.ThinkingPrefixHeader) != "unchecked" {
		t.Errorf("status %d, %s %q", resp.StatusCode, provider.ThinkingPrefixHeader, resp.Header.Get(provider.ThinkingPrefixHeader))
	}
	for _, row := range e.ledger() {
		if row.Protocol != string(profile.Anthropic) || row.Endpoint != "messages" || row.Status != 200 ||
			row.Tokens == nil || *row.Tokens != (store.Tokens{Input: 6, Output: 8, CacheRead: 4}) {
			t.Errorf("ledger row %+v %+v", row, row.Tokens)
		}
	}
}

// The model goes in the path escaped, under models/ or the collection its
// name opens with, as genai reads a name, so a deployment's model id can add no
// query and no path of its own.
func TestAGeminiModelIsEscapedInThePath(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, geminiText("ok"))
	paths := map[string]string{
		"gemini/x?alt=sse":        "/v1beta/models/gemini%2Fx%3Falt=sse:generateContent",
		"models/gemini-3.8-flash": "/v1beta/models/gemini-3.8-flash:generateContent",
		"tunedModels/mine":        "/v1beta/tunedModels/mine:generateContent",
		"models/../x":             "/v1beta/models/..%2Fx:generateContent",
	}
	p := onGemini(e, f.URL)
	for model := range paths {
		e.alias(model, target(e.deployment(p, model), 0))
	}
	key := e.key(everyAlias)
	e.start()
	for model, want := range paths {
		if _, err := e.client(key).Messages.New(context.Background(), anthropic.MessageNewParams{Model: model, MaxTokens: 8, Messages: hello()}); err != nil {
			t.Fatal(err)
		}
		calls := f.recorded()
		if call := calls[len(calls)-1]; call.EscapedPath != want || call.Query != "" {
			t.Errorf("%s: upstream path %q, query %q, want %q", model, call.EscapedPath, call.Query, want)
		}
	}
}

// A Gemini answer's call leads with a thinking block carrying its signature
// under the deployment's provenance; sent back in a tool loop, the signature
// returns to that deployment on its call, and another deployment gets the
// sentinel in its place.
func TestAGeminiToolLoopCarriesItsSignature(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, geminiLoop)
	g := e.deployment(onGemini(e, f.URL), "gemini-3.8-flash")
	other := e.deployment(onGemini(e, f.URL), "gemini-3.8-pro")
	e.alias("g", target(g, 0))
	e.alias("other", target(other, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "get_time",
		InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"tz": map[string]any{"type": "string"}}}}}}

	first, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: hello(), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Content) != 2 || first.Content[0].Type != "thinking" || first.Content[0].Thinking != "" ||
		first.Content[0].Signature != "mapgw1."+g.ID+".gemini:c2lnLTE=" || first.Content[1].ID != "call_1" ||
		string(first.Content[1].Input) != `{"tz":"UTC"}` || first.StopReason != "tool_use" {
		t.Fatalf("first answer: %+v", first.Content)
	}
	loop := append(hello(), first.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock("call_1", "noon", false)))
	for _, model := range []string{"g", "other"} {
		m, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: model, MaxTokens: 64, Messages: loop, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Content) != 1 || m.Content[0].Text != "noon it is" {
			t.Errorf("%s: %+v", model, m.Content)
		}
	}
	for i, want := range map[int]string{1: "c2lnLTE=", 2: "skip_thought_signature_validator"} {
		var sent struct {
			Contents []struct {
				Role  string                       `json:"role"`
				Parts []map[string]json.RawMessage `json:"parts"`
			} `json:"contents"`
		}
		call := f.recorded()[i]
		_ = json.Unmarshal(call.Raw, &sent)
		if len(sent.Contents) != 3 || sent.Contents[1].Role != "model" || len(sent.Contents[1].Parts) != 1 ||
			string(sent.Contents[1].Parts[0]["thoughtSignature"]) != `"`+want+`"` ||
			string(sent.Contents[2].Parts[0]["functionResponse"]) != `{"id":"call_1","name":"get_time","response":{"output":"noon"}}` {
			t.Errorf("call %d to %s: %s", i, call.Path, call.Raw)
		}
	}
}

// A Gemini-only alias answers no Chat Completions request and no count; a
// request Gemini cannot carry, or one that streams, is refused naming why,
// and goes to another target that can serve it.
func TestWhatAGeminiAttemptCannotServe(t *testing.T) {
	e := newEnv(t)
	gf := newFake(t, geminiText("from gemini"))
	af := newFake(t, message("from anthropic"))
	gd := e.deployment(onGemini(e, gf.URL), "gemini-3.8-flash")
	ap := e.provider(af.URL)
	e.credential(ap, "sk-anthropic-key1", 1)
	e.alias("g", target(gd, 0))
	e.alias("mix", target(gd, 0), target(e.deployment(ap, "claude"), 1))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	ctx := context.Background()

	var apiErr *openai.Error
	oc := e.oaClient(key, "/v1")
	_, err := oc.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{Model: "g",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}})
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("chat completions: %v", err)
	}
	var aErr *anthropic.Error
	if _, err := cl.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{Model: "g", Messages: hello()}); !errors.As(err, &aErr) || aErr.StatusCode != http.StatusNotFound {
		t.Errorf("count_tokens: %v", err)
	}

	doc := `{"model":"%s","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}]}]}`
	streamed := `{"model":"%s","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	for body, field := range map[string]string{doc: "messages[0].content[0]", streamed: "stream"} {
		resp, b := e.do("POST", "/v1/messages", strings.Replace(body, "%s", "g", 1), hdr)
		if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadRequest || typ != "invalid_request_error" || !strings.HasPrefix(msg, field) {
			t.Errorf("%s: %d %s %s", field, resp.StatusCode, typ, msg)
		}
		if resp, b := e.do("POST", "/v1/messages", strings.Replace(body, "%s", "mix", 1), hdr); resp.StatusCode != 200 || !strings.Contains(string(b), "from anthropic") {
			t.Errorf("%s on mix: %d %s", field, resp.StatusCode, b)
		}
	}
	if m, err := cl.Messages.New(ctx, anthropic.MessageNewParams{Model: "mix", MaxTokens: 8, Messages: hello()}); err != nil || m.Content[0].Text != "from gemini" {
		t.Errorf("mix: %+v, %v", m, err)
	}
	if n := len(gf.recorded()); n != 1 {
		t.Errorf("gemini was called %d times, want once", n)
	}
}

// Gemini refusing the gateway's key — its 402 for an empty prepaid balance
// among them, and its 400 for a key that is not valid (measured 2026-10-10) —
// is the gateway's own failure: each credential is tried, and
// the caller is answered 502 api_error. Any other error reaches the caller in
// Anthropic's envelope, its message Gemini's and its type the status's.
func TestGeminiErrors(t *testing.T) {
	e := newEnv(t)
	depleted := newFake(t, status(402, `{"error":{"code":402,"message":"Your prepayment credits are depleted.","status":"RESOURCE_EXHAUSTED"}}`))
	bad := newFake(t, status(400, `{"error":{"code":400,"message":"* GenerateContentRequest.contents: contents is not specified","status":"INVALID_ARGUMENT"}}`))
	revoked := newFake(t, status(400, `{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"API_KEY_INVALID","domain":"googleapis.com","metadata":{"service":"generativelanguage.googleapis.com"}}]}}`))
	p := onGemini(e, depleted.URL)
	e.credential(p, "AIza-gemini-key2", 1)
	e.alias("depleted", target(e.deployment(p, "gemini-3.8-flash"), 0))
	rp := onGemini(e, revoked.URL)
	e.credential(rp, "AIza-gemini-key2", 1)
	e.alias("revoked", target(e.deployment(rp, "gemini-3.8-flash"), 0))
	e.alias("bad", target(e.deployment(onGemini(e, bad.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}

	resp, b := e.do("POST", "/v1/messages", `{"model":"depleted","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadGateway || typ != "api_error" || !strings.Contains(msg, "credential") {
		t.Errorf("depleted: %d %s %s", resp.StatusCode, typ, msg)
	}
	if n := len(depleted.recorded()); n != 2 {
		t.Errorf("depleted was called %d times, want once per credential", n)
	}
	resp, b = e.do("POST", "/v1/messages", `{"model":"revoked","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadGateway || typ != "api_error" || !strings.Contains(msg, "credential") {
		t.Errorf("revoked: %d %s %s", resp.StatusCode, typ, msg)
	}
	if n := len(revoked.recorded()); n != 2 {
		t.Errorf("revoked was called %d times, want once per credential", n)
	}
	resp, b = e.do("POST", "/v1/messages", `{"model":"bad","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, hdr)
	if typ, msg, rid := errorOf(t, b); resp.StatusCode != http.StatusBadRequest || typ != "invalid_request_error" ||
		msg != "* GenerateContentRequest.contents: contents is not specified" || rid == "" {
		t.Errorf("bad: %d %s %s", resp.StatusCode, typ, msg)
	}
}

// An answer Gemini ended for a reason Messages has none for is the gateway's
// 502, naming it; its tokens are counted all the same.
func TestAGeminiAnswerThatCannotBeCarried(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, status(200, `{"candidates":[{"content":{"parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL","finishMessage":"bad call"}],"usageMetadata":`+geminiUsage+`}`))
	e.alias("g", target(e.deployment(onGemini(e, f.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"g","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadGateway || typ != "api_error" || !strings.Contains(msg, "MALFORMED_FUNCTION_CALL") {
		t.Errorf("%d %s %s", resp.StatusCode, typ, msg)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Tokens == nil || *rows[0].Tokens != (store.Tokens{Input: 6, Output: 8, CacheRead: 4}) {
		t.Errorf("ledger: %+v", rows)
	}
}

// A Responses request reaches Gemini through both conversions: Responses to
// Messages to generateContent, and back.
func TestAResponsesRequestReachesGemini(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, geminiText("Jupiter"))
	e.alias("g", target(e.deployment(onGemini(e, f.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	oc := e.oaClient(key, "/v1")
	r, err := oc.Responses.New(context.Background(), respParams("g"))
	if err != nil {
		t.Fatal(err)
	}
	if r.OutputText() != "Jupiter" || r.Status != "completed" || r.Usage.InputTokens != 10 || r.Usage.InputTokensDetails.CachedTokens != 4 || r.Usage.OutputTokens != 8 {
		t.Errorf("response %+v", r)
	}
	if call := f.recorded()[0]; call.Path != "/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Errorf("upstream path %s", call.Path)
	}
}

func decodedJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

// A block goes back only to the deployment that produced it, and only on the
// protocol that did: a Gemini signature to no Anthropic or OpenAI attempt,
// and theirs to no Gemini one.
func TestProvenanceKeepsEachProtocolsThinking(t *testing.T) {
	for _, tc := range []struct {
		value string
		up    profile.Protocol
		want  bool
	}{
		{"gemini:c2ln", profile.Gemini, true},
		{"gemini:c2ln", profile.Anthropic, false},
		{"gemini:c2ln", profile.OpenAI, false},
		{"EqQBCkgIBRABGAIiQL", profile.Anthropic, true},
		{"EqQBCkgIBRABGAIiQL", profile.Gemini, false},
		{"", profile.OpenAI, true},
		{"", profile.Gemini, false},
	} {
		if got := modelgateway.KeptFor("dep_1", tc.value, "dep_1", tc.up); got != tc.want {
			t.Errorf("%q on %s: kept %v, want %v", tc.value, tc.up, got, tc.want)
		}
	}
	if modelgateway.KeptFor("dep_1", "gemini:c2ln", "dep_2", profile.Gemini) {
		t.Error("a Gemini block went to another deployment")
	}
}

// Gemini refusing a call's signature is a thinking refusal: the request goes
// once more without its thinking, the sentinel in the signature's place.
func TestAGeminiSignatureRefusalIsStrippedOnce(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		if strings.Contains(string(c.Raw), `"thoughtSignature":"c2lnLTE="`) {
			writeBody(w, 400, `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts","status":"INVALID_ARGUMENT"}}`)
			return
		}
		geminiLoop(w, r, c)
	})
	e.alias("g", target(e.deployment(onGemini(e, f.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "get_time",
		InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"tz": map[string]any{"type": "string"}}}}}}
	first, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: hello(), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	loop := append(hello(), first.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock("call_1", "noon", false)))
	m, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: loop, Tools: tools})
	if err != nil || len(m.Content) != 1 || m.Content[0].Text != "noon it is" {
		t.Fatalf("%+v, %v", m, err)
	}
	calls := f.recorded()
	if len(calls) != 3 || !strings.Contains(string(calls[2].Raw), `"thoughtSignature":"skip_thought_signature_validator"`) {
		t.Errorf("%d calls, the last %s", len(calls), calls[len(calls)-1].Raw)
	}
}

// When every attempt's conversion refuses a request, the refusal answered is
// the same one whatever order the draw put the attempts in.
func TestBothConversionsRefusingAnswerOneWay(t *testing.T) {
	e := newEnv(t)
	gf := newFake(t, geminiText("unused"))
	of := newFake(t, status(500, `{}`))
	e.alias("both", target(e.deployment(onGemini(e, gf.URL), "gemini-3.8-flash"), 0),
		target(e.deployment(onOpenAI(e, "openai-generic", of.URL), "gpt"), 0))
	key := e.key(everyAlias)
	e.start()
	doc := `{"model":"both","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}]}]}`
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	for range 12 {
		resp, b := e.do("POST", "/v1/messages", doc, hdr)
		if _, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadRequest || !strings.Contains(msg, "Chat Completions") {
			t.Fatalf("%d %s", resp.StatusCode, msg)
		}
	}
}
