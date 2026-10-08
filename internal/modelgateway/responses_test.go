package modelgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

// respParams is a Responses request for model saying hello, briefly.
func respParams(model string) responses.ResponseNewParams {
	return responses.ResponseNewParams{Model: model, Instructions: openai.String("be brief"), MaxOutputTokens: openai.Int(64),
		Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("hello")}}
}

// streamResponse reads a Responses stream through openai-go, and returns
// its events' types, its text deltas joined, and its last event.
func streamResponse(t *testing.T, cl openai.Client, p responses.ResponseNewParams) ([]string, string, responses.ResponseStreamEventUnion) {
	t.Helper()
	s := cl.Responses.NewStreaming(context.Background(), p)
	var types []string
	var text string
	var last responses.ResponseStreamEventUnion
	for s.Next() {
		last = s.Current()
		types = append(types, last.Type)
		if last.Type == "response.output_text.delta" {
			text += last.Delta
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return types, text, last
}

// A Responses request is served as the Messages request it converts to: the
// upstream is sent instructions as system, input as messages and
// max_output_tokens as max_tokens, and the caller is answered with a
// Response, whole and streamed, its model the alias. The ledger records it
// under its own endpoint, on the OpenAI protocol.
func TestAResponsesRequestIsServedAsMessages(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, message("Jupiter"))
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")

	r, err := cl.Responses.New(context.Background(), respParams("fast"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.ID, "resp_") || r.Model != "fast" || r.Status != "completed" || r.OutputText() != "Jupiter" ||
		r.Usage.InputTokens != 5 || r.Usage.OutputTokens != 2 || r.Instructions.OfString != "be brief" {
		t.Errorf("response: %+v", r)
	}
	call := f.recorded()[0]
	var body map[string]any
	_ = json.Unmarshal(call.Raw, &body)
	want := map[string]any{"model": "up-model", "max_tokens": float64(64), "system": "be brief",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}}}}
	if call.Path != "/v1/messages" || call.Key != "sk-upstream-1" || !reflect.DeepEqual(body, want) {
		t.Errorf("upstream call: %s %s", call.Path, call.Raw)
	}

	types, text, last := streamResponse(t, cl, respParams("fast"))
	if text != "Jupiter" || last.Type != "response.completed" || last.Response.OutputText() != "Jupiter" ||
		last.Response.Usage.OutputTokens != 2 || types[0] != "response.created" {
		t.Errorf("stream: %v %q %+v", types, text, last.Response)
	}
	for _, row := range e.ledger() {
		if row.Protocol != string(profile.OpenAI) || row.Endpoint != "responses" || row.Status != 200 ||
			row.Tokens == nil || row.Tokens.Input != 5 || row.Tokens.Output != 2 {
			t.Errorf("ledger row %+v %+v", row, row.Tokens)
		}
	}
}

// A credential usable on the OpenAI protocol alone is reached through two
// conversions — Responses to Messages to Chat Completions, and back — whole,
// and streamed in both vendors' styles.
func TestAResponsesRequestReachesAnOpenAIOnlyCredential(t *testing.T) {
	for _, style := range []chatStyle{deepseekStyle, minimaxStyle} {
		e := newEnv(t)
		f := newFake(t, chatAnswer("Jupiter", style))
		e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL+"/base"), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		cl := e.oaClient(key, "/v1")

		r, err := cl.Responses.New(context.Background(), respParams("fast"))
		if err != nil {
			t.Fatal(err)
		}
		if r.OutputText() != "Jupiter" || r.Status != "completed" || r.Usage.InputTokens != 10 || r.Usage.InputTokensDetails.CachedTokens != 4 {
			t.Errorf("style %d: response %+v", style, r)
		}
		if call := f.recorded()[0]; call.Path != "/base/chat/completions" {
			t.Errorf("style %d: upstream path %s", style, call.Path)
		}
		_, text, last := streamResponse(t, cl, respParams("fast"))
		if text != "Jupiter " || last.Type != "response.completed" || last.Response.Usage.OutputTokens != 3 {
			t.Errorf("style %d: stream %q %s %+v", style, text, last.Type, last.Response.Usage)
		}
	}
}

// A max_output_tokens left out is the alias's max_tokens as /v1/models
// answers it, or 8,192 where a target records none.
func TestAResponsesRequestsDefaultMaxTokens(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, message("ok"))
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("capped", target(e.deployment(p, "a", func(d *store.Deployment) { d.Capabilities.MaxTokens = 4096 }), 0))
	e.alias("open", target(e.deployment(p, "b"), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")
	for i, model := range []string{"capped", "open"} {
		p := responses.ResponseNewParams{Model: model, Input: respParams(model).Input}
		if _, err := cl.Responses.New(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		if got, want := string(f.recorded()[i].Body["max_tokens"]), []string{"4096", "8192"}[i]; got != want {
			t.Errorf("%s: max_tokens %s, want %s", model, got, want)
		}
	}
}

// thinkingThenCall answers with a signed thinking block and a tool call the
// first time, and with text once the call's result comes back.
func thinkingThenCall(w http.ResponseWriter, _ *http.Request, c fakeCall) {
	if strings.Contains(string(c.Raw), "tool_result") {
		writeBody(w, 200, fmt.Sprintf(`{"id":"msg_2","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"noon"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":1}}`, c.Model))
		return
	}
	writeBody(w, 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[
		{"type":"thinking","thinking":"use the tool","signature":"sig-up"},
		{"type":"tool_use","id":"toolu_1","name":"get_time","input":{"tz":"UTC"}}],"stop_reason":"tool_use","usage":{"input_tokens":8,"output_tokens":4}}`, c.Model))
}

// A tool loop keeps its thinking: the reasoning item a Response carries,
// sent back as input with the call's output, reaches the deployment that
// produced it as the thinking block it was, its own signature restored.
func TestAResponsesToolLoopKeepsItsThinking(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, thinkingThenCall)
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	d := e.deployment(p, "up-model")
	e.alias("fast", target(d, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")
	tool := responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: "get_time",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"tz": map[string]any{"type": "string"}}}}}
	first := respParams("fast")
	first.Tools = []responses.ToolUnionParam{tool}
	r, err := cl.Responses.New(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Output) != 2 || r.Output[0].Type != "reasoning" || r.Output[0].EncryptedContent != "mapgw1."+d.ID+".sig-up" ||
		r.Output[1].Type != "function_call" || r.Output[1].CallID != "toolu_1" || r.Output[1].Arguments.OfString != `{"tz":"UTC"}` {
		t.Fatalf("first answer: %s", r.RawJSON())
	}
	var input []responses.ResponseInputItemUnionParam
	input = append(input, responses.ResponseInputItemParamOfMessage("hello", responses.EasyInputMessageRoleUser))
	for _, o := range r.Output {
		input = append(input, asInput(t, o))
	}
	result := responses.ResponseInputItemParamOfFunctionCallOutput("12:00")
	result.OfFunctionCallOutput.CallID = openai.String("toolu_1")
	input = append(input, result)
	second := responses.ResponseNewParams{Model: "fast", Tools: first.Tools, Input: responses.ResponseNewParamsInputUnion{OfInputItemList: input}}
	r, err = cl.Responses.New(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if r.OutputText() != "noon" {
		t.Errorf("second answer: %s", r.RawJSON())
	}
	var sent struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(f.recorded()[1].Raw, &sent)
	if len(sent.Messages) != 3 || sent.Messages[1].Role != "assistant" || len(sent.Messages[1].Content) != 2 ||
		sent.Messages[1].Content[0]["type"] != "thinking" || sent.Messages[1].Content[0]["signature"] != "sig-up" ||
		sent.Messages[1].Content[0]["thinking"] != "use the tool" || sent.Messages[2].Content[0]["type"] != "tool_result" {
		t.Errorf("second upstream call: %s", f.recorded()[1].Raw)
	}
}

// asInput is an output item sent back as input, as a client replaying a
// stateless conversation does through openai-go's ToParam.
func asInput(t *testing.T, o responses.ResponseOutputItemUnion) responses.ResponseInputItemUnionParam {
	t.Helper()
	raw := []byte(o.RawJSON())
	switch o.Type {
	case "reasoning":
		var it responses.ResponseReasoningItem
		if err := json.Unmarshal(raw, &it); err != nil {
			t.Fatal(err)
		}
		p := it.ToParam()
		return responses.ResponseInputItemUnionParam{OfReasoning: &p}
	case "function_call":
		var it responses.ResponseFunctionToolCall
		if err := json.Unmarshal(raw, &it); err != nil {
			t.Fatal(err)
		}
		p := it.ToParam()
		return responses.ResponseInputItemUnionParam{OfFunctionCall: &p}
	case "message":
		var it responses.ResponseOutputMessage
		if err := json.Unmarshal(raw, &it); err != nil {
			t.Fatal(err)
		}
		p := it.ToParam()
		return responses.ResponseInputItemUnionParam{OfOutputMessage: &p}
	}
	t.Fatalf("no input for a %s item", o.Type)
	return responses.ResponseInputItemUnionParam{}
}

// Once a stream has failed in conversion the caller is written nothing more,
// keep-alives included, while the upstream is read on to its end.
func TestAFailedResponsesStreamWritesNothingMore(t *testing.T) {
	defer modelgateway.SetPingEvery(20 * time.Millisecond)()
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, call fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		ev := events(call.Model, "half")
		for _, s := range append(ev[:5:5], "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"web_search\",\"input\":{}}}\n\n") {
			_, _ = io.WriteString(w, s)
		}
		w.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		// The usage, then an error of the upstream's own, which must not
		// replace the failure the caller was given.
		for _, s := range append(ev[5:7:7], "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n") {
			_, _ = io.WriteString(w, s)
		}
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	_, b := e.do(http.MethodPost, "/v1/responses", `{"model":"fast","input":"hello","stream":true}`, map[string]string{"Authorization": "Bearer " + key})
	before, after, ok := strings.Cut(string(b), "event: response.failed\n")
	if !ok || strings.Contains(before, "event: ping") || strings.Count(after, "\n\n") != 1 {
		t.Errorf("stream %s", b)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Tokens == nil || rows[0].Tokens.Output != 2 || rows[0].ErrorType != "api_error" {
		t.Errorf("ledger %+v", rows)
	}
}

// oaError is the OpenAI error an SDK call failed with.
func oaError(t *testing.T, err error) *openai.Error {
	t.Helper()
	var apierr *openai.Error
	if !errors.As(err, &apierr) {
		t.Fatalf("not an OpenAI API error: %v", err)
	}
	return apierr
}

// A Responses caller is refused in OpenAI's envelope, by the fields it
// wrote: what a stateless gateway cannot do, a refusal of the Messages
// request it became, and an upstream's error.
func TestAResponsesRequestIsRefusedInItsOwnWords(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	chat := newFake(t, chatAnswer("x", deepseekStyle))
	e.alias("chat", target(e.deployment(onOpenAI(e, "deepseek", chat.URL), "deepseek-flash"), 0))
	e.alias("idle", target(e.deployment(p, "off", func(d *store.Deployment) { d.Enabled = false }), 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")

	// A Responses request is served on either protocol, so a model with no
	// upstream has none on any.
	_, err := cl.Responses.New(context.Background(), respParams("idle"))
	if a := oaError(t, err); a.StatusCode != 503 || a.Message != "model idle has no enabled upstream" {
		t.Errorf("no upstream: %d %s", a.StatusCode, a.Message)
	}

	stateful := respParams("fast")
	stateful.PreviousResponseID = openai.String("resp_1")
	_, err = cl.Responses.New(context.Background(), stateful)
	if a := oaError(t, err); a.StatusCode != 400 || a.Type != "invalid_request_error" || !strings.HasPrefix(a.Message, "previous_response_id: the gateway stores no response") {
		t.Errorf("previous_response_id: %d %s %s", a.StatusCode, a.Type, a.Message)
	}
	_, err = cl.Responses.New(context.Background(), respParams("fast"))
	if a := oaError(t, err); a.StatusCode != 400 || a.Type != "invalid_request_error" || a.Message != "prompt is too long" {
		t.Errorf("upstream refusal: %d %s %s", a.StatusCode, a.Type, a.Message)
	}
	// openai-go reads an Anthropic envelope's error member too, so the body
	// itself is checked: OpenAI's four fields, and nothing of Anthropic's.
	_, b := e.do(http.MethodPost, "/v1/responses", `{"model":"fast","input":"hello"}`, map[string]string{"Authorization": "Bearer " + key})
	if want := `{"error":{"code":null,"message":"prompt is too long","param":null,"type":"invalid_request_error"}}`; string(b) != want {
		t.Errorf("upstream refusal body %s, want %s", b, want)
	}
	structured := respParams("chat")
	structured.Text = responses.ResponseTextConfigParam{Format: responses.ResponseFormatTextConfigUnionParam{
		OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{Name: "t", Schema: map[string]any{"type": "object"}}}}
	_, err = cl.Responses.New(context.Background(), structured)
	if a := oaError(t, err); a.StatusCode != 400 || !strings.HasPrefix(a.Message, "text.format") {
		t.Errorf("a conversion's refusal: %d %s", a.StatusCode, a.Message)
	}
	serial := respParams("chat")
	serial.ParallelToolCalls = openai.Bool(false)
	serial.Tools = []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{Name: "get_time"}}}
	_, err = cl.Responses.New(context.Background(), serial)
	if a := oaError(t, err); a.StatusCode != 400 || a.Message != "parallel_tool_calls: every upstream of model chat ignores it" {
		t.Errorf("a vendor's refusal: %d %s", a.StatusCode, a.Message)
	}
	if n := len(chat.recorded()); n != 0 {
		t.Errorf("the refused request reached the upstream %d times", n)
	}
	// Offering no tools, it asks nothing a vendor could ignore.
	serial.Tools = nil
	if _, err := cl.Responses.New(context.Background(), serial); err != nil {
		t.Errorf("parallel_tool_calls false, no tools: %v", err)
	}
	for path, want := range map[string]string{
		"/v1/responses/resp_1":              "stateless",
		"/v1/responses/resp_1/input_items":  "stateless",
		"/openai/v1/responses/input_tokens": "/openai/v1/responses/input_tokens: not supported by the gateway's Responses API",
		"/v1/responses/compact":             "/v1/responses/compact: not supported by the gateway's Responses API",
	} {
		resp, b := e.do(http.MethodPost, path, "{}", map[string]string{"Authorization": "Bearer " + key})
		var env struct {
			Error struct{ Message, Type string } `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		if resp.StatusCode != 404 || env.Error.Type != "not_found_error" || !strings.Contains(env.Error.Message, want) {
			t.Errorf("%s: %d %s", path, resp.StatusCode, b)
		}
	}
	if resp, _ := e.do(http.MethodGet, "/v1/responses", "", map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 405 {
		t.Errorf("GET /v1/responses: %d", resp.StatusCode)
	}
}

// A whole answer carrying a block the conversion cannot carry is the
// gateway's 502, in OpenAI's envelope, naming the block with the call's key
// redacted, and is not retried once the upstream has answered.
func TestAnUnconvertibleResponsesAnswerIsA502(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		writeBody(w, 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"sk-upstream-1","id":"s"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2}}`, c.Model))
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do(http.MethodPost, "/v1/responses", `{"model":"fast","input":"hi"}`, map[string]string{"Authorization": "Bearer " + key})
	want := `{"error":{"code":null,"message":"upstream answer could not be converted: content[0]: a \"[redacted]\" block has no Responses counterpart","param":null,"type":"api_error"}}`
	if resp.StatusCode != http.StatusBadGateway || strings.TrimSpace(string(b)) != want || len(f.recorded()) != 1 {
		t.Errorf("%d %s after %d calls, want 502 %s", resp.StatusCode, b, len(f.recorded()), want)
	}
}

// A credential an escape would disguise — a quote in it — is redacted from a
// conversion failure that names a block the upstream sent, whole and
// streamed: the block's type is written as sent, not Go-quoted.
func TestAQuotedCredentialIsRedactedFromAResponsesFailure(t *testing.T) {
	const secret = `sk-up/stream"key-9`
	typ, _ := json.Marshal(secret)
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		if !stream {
			writeBody(w, 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":%s}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2}}`, c.Model, typ))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		last := fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":%s}}\n\n", typ)
		if strings.Contains(string(c.Raw), "a delta") {
			last = fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":%s}}\n\n", typ)
		}
		for _, s := range append(events(c.Model, "half")[:5:5], last) {
			_, _ = io.WriteString(w, s)
		}
	})
	p := e.provider(f.URL)
	e.credential(p, secret, 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	for _, c := range []struct {
		name, body string
	}{
		{"whole", `{"model":"fast","input":"hi"}`},
		{"a block", `{"model":"fast","input":"hi","stream":true}`},
		{"a delta", `{"model":"fast","input":"a delta","stream":true}`},
	} {
		_, b := e.do(http.MethodPost, "/v1/responses", c.body, map[string]string{"Authorization": "Bearer " + key})
		if strings.Contains(string(b), "key-9") || !strings.Contains(string(b), `[redacted]`) {
			t.Errorf("%s: %s", c.name, b)
		}
	}
}

// An event the upstream sent within the bound on one event, which the
// gateway's rewriting then grows past it — a signature, wrapped — fails the
// stream rather than going missing from it.
func TestAResponsesEventGrownPastTheBoundFails(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		const head, tail = "event: content_block_delta\ndata: " +
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"`, "\"}}\n\n"
		sig := strings.Repeat("s", upstream.MaxEvent-8-len(head)-len(tail))
		start := events(c.Model, "")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, s := range []string{start[0],
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n",
			head + sig + tail, start[5], start[6], start[7]} {
			_, _ = io.WriteString(w, s)
		}
	})
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	types, _, last := streamResponse(t, e.oaClient(key, "/v1"), respParams("fast"))
	if last.Type != "response.failed" || last.Response.Error.Message != "upstream stream could not be converted: "+upstream.ErrEventTooLarge.Error() {
		t.Errorf("stream %v %+v", types, last.Response.Error)
	}
}

// A stream whose content passes the gateway's bound on a whole answer fails
// at the event that passes it, the upstream read on for its usage.
func TestAResponsesStreamIsBounded(t *testing.T) {
	defer modelgateway.SetMaxResponseBody(3)()
	e := newEnv(t)
	f := newFake(t, message("half"))
	p := e.provider(f.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("fast", target(e.deployment(p, "up-model"), 0))
	key := e.key(everyAlias)
	e.start()
	types, text, last := streamResponse(t, e.oaClient(key, "/v1"), respParams("fast"))
	if text != "" || last.Type != "response.failed" || last.Response.Error.Message != "upstream stream could not be converted: the answer passes the gateway's bound of 3 bytes" {
		t.Errorf("stream %v %q %+v", types, text, last.Response.Error)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Tokens == nil || rows[0].Tokens.Output != 2 {
		t.Errorf("ledger %+v", rows)
	}
}

// On an OpenAI-only credential, the upstream's own error reaches a Responses
// caller as the upstream wrote it — its type, code and param, its message
// without the key — rather than the Anthropic type the Messages path makes
// of it: as an error response, and as the error a stream opens with. A body
// stating no type has the one its status gives, and MiniMax's, in
// Anthropic's envelope, keeps its own.
func TestAConvertedResponsesErrorIsTheUpstreams(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(http.ResponseWriter, *http.Request, fakeCall)
		status int
		want   string
	}{
		{"OpenAI's", status(400, `{"error":{"message":"bad sk-deepseek-key1 here","type":"tokens","param":"input","code":"context_length_exceeded"}}`),
			400, `{"error":{"code":"context_length_exceeded","message":"bad [redacted] here","param":"input","type":"tokens"}}`},
		{"a key anywhere", status(400, `{"error":{"message":"m","type":"sk-deepseek-key1","param":{"k":"sk-deepseek-key1"},"code":"sk-deepseek-key1"}}`),
			400, `{"error":{"code":"[redacted]","message":"m","param":{"k":"[redacted]"},"type":"[redacted]"}}`},
		{"an empty message", status(400, `{"error":{"message":"","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`),
			400, `{"error":{"code":"context_length_exceeded","message":"","param":"input","type":"invalid_request_error"}}`},
		{"no type", status(404, `{"error":{"message":"no such model"}}`),
			404, `{"error":{"code":null,"message":"no such model","param":null,"type":"not_found_error"}}`},
		{"Anthropic's", status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid tool_result content (2013)"}}`),
			400, `{"error":{"code":null,"message":"invalid tool_result content (2013)","param":null,"type":"invalid_request_error"}}`},
		{"a stream's first event", func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `data: {"error":{"message":"too long","type":"tokens","param":null,"code":"context_length_exceeded","http_code":"400"}}`+"\n\n")
		}, 400, `{"error":{"code":"context_length_exceeded","message":"too long","param":null,"type":"tokens"}}`},
	} {
		e := newEnv(t)
		f := newFake(t, c.answer)
		e.alias("chat", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
		key := e.key(everyAlias)
		e.start()
		resp, b := e.do(http.MethodPost, "/v1/responses", `{"model":"chat","input":"hi","stream":true}`, map[string]string{"Authorization": "Bearer " + key})
		if resp.StatusCode != c.status || string(b) != c.want {
			t.Errorf("%s: %d %s, want %d %s", c.name, resp.StatusCode, b, c.status, c.want)
		}
	}
}

// An upstream failing partway through a stream ends the caller's as
// response.failed, the error inside the Response, which openai-go reads as
// an event rather than a broken stream: an upstream's error event, a stream
// ending before message_stop, and a block the conversion cannot carry —
// after which the upstream is read on for the usage it reports.
func TestAResponsesStreamFailsAsAResponse(t *testing.T) {
	serverTool := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s\",\"name\":\"web_search\",\"input\":{}}}\n\n"
	for _, c := range []struct {
		name, tail, msg, errType string
		usage                    bool
	}{
		{"error event", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			"busy", "overloaded_error", false},
		{"cut off", "", "upstream stream failed: the stream ended before message_stop", "api_error", false},
		{"unconvertible", serverTool + strings.Join(events("m", "")[5:], ""),
			`upstream stream could not be converted: a "server_tool_use" block has no Responses counterpart`, "api_error", true},
		{"a key in a block", strings.Replace(serverTool, "server_tool_use", "sk-upstream-1", 1) + strings.Join(events("m", "")[5:], ""),
			`upstream stream could not be converted: a "[redacted]" block has no Responses counterpart`, "api_error", true},
	} {
		e := newEnv(t)
		f := newFake(t, func(w http.ResponseWriter, _ *http.Request, call fakeCall) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			for _, s := range append(events(call.Model, "half")[:5:5], c.tail) {
				_, _ = io.WriteString(w, s)
			}
		})
		p := e.provider(f.URL)
		e.credential(p, "sk-upstream-1", 1)
		e.alias("fast", target(e.deployment(p, "up-model"), 0))
		key := e.key(everyAlias)
		e.start()
		types, text, last := streamResponse(t, e.oaClient(key, "/v1"), respParams("fast"))
		if text != "half" || last.Type != "response.failed" || last.Response.Error.Code != "server_error" || last.Response.Error.Message != c.msg {
			t.Errorf("%s: stream %v %q %+v", c.name, types, text, last.Response)
		}
		rows := e.ledger()
		if len(rows) != 1 || rows[0].Endpoint != "responses" || rows[0].ErrorType != c.errType ||
			c.usage && (rows[0].Tokens == nil || rows[0].Tokens.Output != 2) {
			t.Errorf("%s: ledger %+v", c.name, rows)
		}
	}
}
