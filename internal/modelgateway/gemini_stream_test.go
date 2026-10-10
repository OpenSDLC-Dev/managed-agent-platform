package modelgateway_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/responses"
)

// geminiSSE streams chunks as Gemini does for alt=sse: one data line each,
// CRLF-framed, under contentType, and no [DONE] — the stream ends when the
// upstream closes it.
func geminiSSE(contentType string, chunks ...string) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(200)
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\r\n\r\n")
			w.(http.Flusher).Flush()
		}
	}
}

const (
	geminiThought = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Pondering","thought":true}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"},"responseId":"resp-s"}`
	geminiPiece   = `{"candidates":[{"content":{"role":"model","parts":[{"text":"Jupiter"}]}}],"usageMetadata":{"trafficType":"ON_DEMAND"},"responseId":"resp-s"}`
	geminiFinish  = `{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"dGV4dA=="}]},"finishReason":"STOP"}],"usageMetadata":` + geminiUsage + `,"responseId":"resp-s"}`
	geminiCalled  = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"get_time","args":{"tz":"UTC"}},"thoughtSignature":"c2lnLTE="}]}}],"responseId":"resp-s"}`
)

// stream reads a streamed Messages answer through anthropic-sdk-go.
func stream(t *testing.T, cl *anthropic.Client, p anthropic.MessageNewParams) anthropic.Message {
	t.Helper()
	s := cl.Messages.NewStreaming(context.Background(), p)
	var m anthropic.Message
	for s.Next() {
		if err := m.Accumulate(s.Current()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return m
}

// A streamed Messages request goes to the deployment's
// streamGenerateContent with alt=sse, and comes back as a Messages stream:
// the thought summaries leading as a thinking block under the deployment's
// provenance, the text after it, the usage from the last chunk, the ledger
// row the caller's — whatever Content-Type the upstream's stream names.
func TestAStreamedRequestReachesGemini(t *testing.T) {
	for _, contentType := range []string{"text/event-stream", "application/json"} {
		e := newEnv(t)
		f := newFake(t, geminiSSE(contentType, geminiThought, geminiPiece, geminiFinish))
		g := e.deployment(onGemini(e, f.URL), "gemini-3.8-flash")
		e.alias("g", target(g, 0))
		key := e.key(everyAlias)
		e.start()

		m := stream(t, e.client(key), anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: hello()})
		if m.ID != "resp-s" || m.Model != "g" || len(m.Content) != 2 || m.Content[0].Thinking != "Pondering" ||
			m.Content[0].Signature != "mapgw1."+g.ID+".gemini:" || m.Content[1].Text != "Jupiter" || m.StopReason != "end_turn" ||
			m.Usage.InputTokens != 6 || m.Usage.CacheReadInputTokens != 4 || m.Usage.OutputTokens != 8 {
			t.Errorf("%s: streamed answer %+v", contentType, m)
		}
		call := f.recorded()[0]
		if call.Path != "/v1beta/models/gemini-3.8-flash:streamGenerateContent" || call.Query != "alt=sse" ||
			call.Header.Get("x-goog-api-key") != "AIza-gemini-key1" || call.Body["stream"] != nil {
			t.Errorf("%s: upstream call %s?%s %s", contentType, call.Path, call.Query, call.Raw)
		}
		resp, b := e.do("POST", "/v1/messages", `{"model":"g","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
		if resp.StatusCode != 200 || resp.Header.Get(provider.ThinkingPrefixHeader) != "unchecked" || !strings.HasSuffix(string(b), "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
			t.Errorf("%s: %d %q\n%s", contentType, resp.StatusCode, resp.Header.Get(provider.ThinkingPrefixHeader), b)
		}
		for _, row := range e.ledger() {
			if row.Status != 200 || row.Tokens == nil || *row.Tokens != (store.Tokens{Input: 6, Output: 8, CacheRead: 4}) {
				t.Errorf("%s: ledger row %+v %+v", contentType, row, row.Tokens)
			}
		}
	}
}

// A streamed call's signature leads, as the whole answer's does, and goes
// back on the call when the tool loop returns it.
func TestAGeminiStreamCarriesItsSignature(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		if c.Query == "alt=sse" {
			geminiSSE("text/event-stream", geminiCalled, geminiFinish)(w, r, c)
			return
		}
		geminiText("noon it is")(w, r, c)
	})
	g := e.deployment(onGemini(e, f.URL), "gemini-3.8-flash")
	e.alias("g", target(g, 0))
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)
	tools := []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{Name: "get_time",
		InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"tz": map[string]any{"type": "string"}}}}}}
	first := stream(t, cl, anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: hello(), Tools: tools})
	if len(first.Content) != 2 || first.Content[0].Signature != "mapgw1."+g.ID+".gemini:c2lnLTE=" || first.Content[1].ID != "call_1" ||
		string(first.Content[1].Input) != `{"tz":"UTC"}` || first.StopReason != "tool_use" {
		t.Fatalf("first answer: %+v", first.Content)
	}
	loop := append(hello(), first.ToParam(), anthropic.NewUserMessage(anthropic.NewToolResultBlock("call_1", "noon", false)))
	if _, err := cl.Messages.New(context.Background(), anthropic.MessageNewParams{Model: "g", MaxTokens: 64, Messages: loop, Tools: tools}); err != nil {
		t.Fatal(err)
	}
	if sent := string(f.recorded()[1].Raw); !strings.Contains(sent, `"thoughtSignature":"c2lnLTE="`) {
		t.Errorf("the loop sent %s", sent)
	}
}

// A stream that opens with an error object answers as that error's response,
// its status the error's code, retried as one would be, a refused key the
// gateway's own failure; an error once the
// answer has begun, a stream the upstream closes before its finish, and a
// chunk the conversion cannot carry each end the stream with an error event.
func TestGeminiStreamErrors(t *testing.T) {
	const exhausted = `{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`
	e := newEnv(t)
	opens := newFake(t, geminiSSE("text/event-stream", exhausted))
	denied := newFake(t, geminiSSE("text/event-stream", `{"error":{"code":403,"message":"Permission denied","status":"PERMISSION_DENIED"}}`))
	broke := newFake(t, geminiSSE("text/event-stream", geminiPiece, exhausted))
	cut := newFake(t, geminiSSE("text/event-stream", geminiPiece))
	odd := newFake(t, geminiSSE("text/event-stream", geminiPiece, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"eA=="}}]}}]}`, geminiFinish))
	for name, f := range map[string]*fake{"opens": opens, "denied": denied, "broke": broke, "cut": cut, "odd": odd} {
		p := onGemini(e, f.URL)
		if name == "opens" || name == "denied" {
			e.credential(p, "AIza-gemini-key2", 1)
		}
		e.alias(name, target(e.deployment(p, "gemini-3.8-flash"), 0))
	}
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	body := `{"model":"%s","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

	resp, b := e.do("POST", "/v1/messages", strings.Replace(body, "%s", "opens", 1), hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusTooManyRequests || typ != "rate_limit_error" || msg != "Resource has been exhausted" {
		t.Errorf("opens: %d %s %s", resp.StatusCode, typ, msg)
	}
	if n := len(opens.recorded()); n != 2 {
		t.Errorf("opens was called %d times, want once per credential", n)
	}
	resp, b = e.do("POST", "/v1/messages", strings.Replace(body, "%s", "denied", 1), hdr)
	if typ, msg, _ := errorOf(t, b); resp.StatusCode != http.StatusBadGateway || typ != "api_error" || !strings.Contains(msg, "credential") {
		t.Errorf("denied: %d %s %s", resp.StatusCode, typ, msg)
	}
	if n := len(denied.recorded()); n != 2 {
		t.Errorf("denied was called %d times, want once per credential", n)
	}
	for name, want := range map[string][]string{
		"broke": {`"type":"rate_limit_error"`, `"message":"Resource has been exhausted"`},
		"cut":   {"the stream ended before its finish"},
		"odd":   {"inlineData has no Messages counterpart"},
	} {
		resp, b := e.do("POST", "/v1/messages", strings.Replace(body, "%s", name, 1), hdr)
		s := string(b)
		ok := resp.StatusCode == 200 && strings.Contains(s, `"text":"Jupiter"`) && strings.Contains(s, "event: error\n") && !strings.Contains(s, "message_stop")
		for _, w := range want {
			ok = ok && strings.Contains(s, w)
		}
		if !ok {
			t.Errorf("%s: %d\n%s", name, resp.StatusCode, s)
		}
	}
}

// A streamed Responses request reaches Gemini through both conversions.
func TestAStreamedResponsesRequestReachesGemini(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, geminiSSE("text/event-stream", geminiPiece, geminiFinish))
	e.alias("g", target(e.deployment(onGemini(e, f.URL), "gemini-3.8-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	types, text, last := streamResponse(t, e.oaClient(key, "/v1"), respParams("g"))
	if text != "Jupiter" || last.Type != "response.completed" || last.Response.Status != responses.ResponseStatusCompleted ||
		last.Response.Usage.OutputTokens != 8 {
		t.Errorf("events %v, text %q, last %+v", types, text, last)
	}
}
