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
		for _, s := range ev[5:] {
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
	if rows := e.ledger(); len(rows) != 1 || rows[0].Tokens == nil || rows[0].Tokens.Output != 2 {
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
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")

	stateful := respParams("fast")
	stateful.PreviousResponseID = openai.String("resp_1")
	_, err := cl.Responses.New(context.Background(), stateful)
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
	_, err = cl.Responses.New(context.Background(), serial)
	if a := oaError(t, err); a.StatusCode != 400 || a.Message != "parallel_tool_calls: every upstream of model chat ignores it" {
		t.Errorf("a vendor's refusal: %d %s", a.StatusCode, a.Message)
	}
	if n := len(chat.recorded()); n != 0 {
		t.Errorf("the refused request reached the upstream %d times", n)
	}
	for _, path := range []string{"/v1/responses/resp_1", "/v1/responses/resp_1/input_items", "/openai/v1/responses/input_tokens"} {
		resp, b := e.do(http.MethodGet, path, "", map[string]string{"Authorization": "Bearer " + key})
		var env struct {
			Error struct{ Message, Type string } `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		if resp.StatusCode != 404 || env.Error.Type != "not_found_error" || !strings.Contains(env.Error.Message, "stateless") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, b)
		}
	}
	if resp, _ := e.do(http.MethodGet, "/v1/responses", "", map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 405 {
		t.Errorf("GET /v1/responses: %d", resp.StatusCode)
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
