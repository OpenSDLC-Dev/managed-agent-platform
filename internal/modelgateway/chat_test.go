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
	oaoption "github.com/openai/openai-go/v3/option"
	"go.opentelemetry.io/otel/trace"
)

// onOpenAI is a provider of the named profile whose OpenAI endpoint is url,
// with one credential.
func onOpenAI(e *env, prof, url string) store.Provider {
	e.t.Helper()
	p := e.provider(url, func(p *store.Provider) {
		p.Name, p.Profile = prof, prof
		p.Endpoints = map[profile.Protocol]string{profile.OpenAI: url}
	})
	e.credential(p, "sk-"+prof+"-key1", 1)
	return p
}

// oaClient is an OpenAI SDK client of the gateway, with its own retries off.
func (e *env) oaClient(key string, base string) openai.Client {
	return openai.NewClient(oaoption.WithBaseURL(e.url+base), oaoption.WithAPIKey(key), oaoption.WithMaxRetries(0))
}

// chatUsage is the usage the fake reports: ten prompt tokens of which four
// were read from the cache, and three completion tokens.
const chatUsage = `{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":4}}`

// chunk is one Chat Completions stream chunk from model.
func chunk(model, content, finish string, usage string) string {
	f := `""`
	if finish != "" {
		f = fmt.Sprintf("%q", finish)
	}
	choices := fmt.Sprintf(`[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":%s}]`, content, f)
	if content == "" && finish == "" {
		choices = `[]`
	}
	if usage == "" {
		usage = "null"
	}
	return fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":%s,\"usage\":%s}\n\n", model, choices, usage)
}

// chatStyle is how a fake ends a stream: as DeepSeek does, usage on the
// last content chunk whether asked or not, then [DONE]; or as MiniMax-M3
// does, usage only when asked, in a chunk of its own with no choices, and no
// [DONE] at all (probed 2026-10-08).
type chatStyle int

const (
	deepseekStyle chatStyle = iota
	minimaxStyle
)

// chatAnswer answers Chat Completions from the model the call named, whole
// or streamed in the style given.
func chatAnswer(text string, style chatStyle) func(http.ResponseWriter, *http.Request, fakeCall) {
	return func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		var stream bool
		_ = json.Unmarshal(c.Body["stream"], &stream)
		if !stream {
			writeBody(w, 200, fmt.Sprintf(`{"id":"c1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, c.Model, text, chatUsage))
			return
		}
		var opts struct {
			IncludeUsage bool `json:"include_usage"`
		}
		_ = json.Unmarshal(c.Body["stream_options"], &opts)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		out := []string{": keep-alive\n\n", chunk(c.Model, text[:1], "", ""), chunk(c.Model, text[1:], "", "")}
		switch style {
		case deepseekStyle:
			out = append(out, chunk(c.Model, " ", "stop", chatUsage), "data: [DONE]\n\n")
		case minimaxStyle:
			out = append(out, chunk(c.Model, " ", "stop", ""))
			if opts.IncludeUsage {
				out = append(out, chunk(c.Model, "", "", chatUsage))
			}
		}
		for _, s := range out {
			_, _ = io.WriteString(w, s)
			w.(http.Flusher).Flush()
		}
	}
}

// dataLines is a stream's data lines, as a caller receives them.
func dataLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if d, ok := strings.CutPrefix(l, "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

// A Chat Completions request passes through to the deployment's OpenAI
// endpoint — its path the endpoint's own /chat/completions, the provider's
// key as a Bearer token, model the upstream id, every other field as sent —
// and the answer comes back with model the name the caller sent. The ledger
// takes the usage in the Messages API's meaning: input without the prompt
// tokens read from the cache.
func TestChatCompletionsPassThrough(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, chatAnswer("Jupiter", deepseekStyle))
	e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL+"/base"), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()

	for _, base := range []string{"/v1", "/openai/v1"} {
		cl := e.oaClient(key, base)
		m, err := cl.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "fast",
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Which planet is largest?")}, Temperature: openai.Float(0.5)})
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		if m.Model != "fast" || len(m.Choices) != 1 || m.Choices[0].Message.Content != "Jupiter" || m.Usage.PromptTokens != 10 {
			t.Errorf("%s: %+v", base, m)
		}
		calls := f.recorded()
		got := calls[len(calls)-1]
		if got.Path != "/base/chat/completions" || got.Model != "deepseek-flash" || got.Header.Get("Authorization") != "Bearer sk-deepseek-key1" ||
			got.Header.Get("X-Api-Key") != "" || string(got.Body["temperature"]) != "0.5" {
			t.Errorf("%s: upstream got %s %s, Authorization %q, body %s", base, got.Path, got.Model, got.Header.Get("Authorization"), got.Raw)
		}
	}

	// Unknown fields and the caller's headers: the body's go as sent, the
	// headers stay at the gateway.
	resp, b := e.do("POST", "/v1/chat/completions", `{"model":"fast","messages":[{"role":"user","content":"hi"}],"vendor_extra":{"a":[1,2]}}`,
		map[string]string{"Authorization": "Bearer " + key, "OpenAI-Organization": "org-x", "X-MAP-Session-ID": "s1", "anthropic-version": "2023-06-01"})
	calls := f.recorded()
	got := calls[len(calls)-1]
	if resp.StatusCode != 200 || string(got.Body["vendor_extra"]) != `{"a":[1,2]}` {
		t.Errorf("unknown field: %d %s; upstream got %s", resp.StatusCode, b, got.Raw)
	}
	for _, h := range []string{"OpenAI-Organization", "X-Map-Session-Id", "Anthropic-Version"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s went upstream: %q", h, v)
		}
	}

	rows := e.ledger()
	if len(rows) != 3 {
		t.Fatalf("%d ledger rows, want 3", len(rows))
	}
	for _, u := range rows {
		if u.Protocol != "openai" || u.Endpoint != "chat_completions" || u.Status != 200 || u.Tokens == nil ||
			*u.Tokens != (store.Tokens{Input: 6, Output: 3, CacheRead: 4}) {
			t.Errorf("ledger: %+v %+v", u, u.Tokens)
		}
	}
}

// A streamed answer relays chunk by chunk, each with the caller's model
// name, its keep-alives kept. The gateway asks for usage, so the ledger
// counts every stream, and keeps the chunk that carries it from a caller
// that did not ask: MiniMax answers usage only when asked, in a chunk with
// no choices, which a caller that did not ask may not expect. A stream that
// ends after its finish without [DONE], as MiniMax-M3's do, ends normally.
func TestChatCompletionsStreamed(t *testing.T) {
	e := newEnv(t)
	ds, mm := newFake(t, chatAnswer("Jupiter", deepseekStyle)), newFake(t, chatAnswer("Jupiter", minimaxStyle))
	e.alias("ds", target(e.deployment(onOpenAI(e, "deepseek", ds.URL), "deepseek-flash"), 0))
	e.alias("mm", target(e.deployment(onOpenAI(e, "minimax", mm.URL), "MiniMax-M3"), 0))
	key := e.key(everyAlias)
	e.start()

	for _, tc := range []struct {
		alias   string
		f       *fake
		options string // the caller's stream_options, "" for none
		usage   bool   // whether the caller sees usage
		done    bool   // whether the caller sees [DONE]
	}{
		{"ds", ds, "", true, true},
		{"mm", mm, "", false, false},
		{"mm", mm, `,"stream_options":{"include_usage":false}`, false, false},
		{"mm", mm, `,"stream_options":{"include_usage":true}`, true, false},
	} {
		name := tc.alias + tc.options
		resp, b := e.do("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]%s}`, tc.alias, tc.options),
			map[string]string{"Authorization": "Bearer " + key})
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(string(b), ": keep-alive") {
			t.Fatalf("%s: %d %s %s", name, resp.StatusCode, resp.Header.Get("Content-Type"), b)
		}
		calls := tc.f.recorded()
		var sent struct {
			IncludeUsage *bool `json:"include_usage"`
		}
		if err := json.Unmarshal(calls[len(calls)-1].Body["stream_options"], &sent); err != nil || sent.IncludeUsage == nil || !*sent.IncludeUsage {
			t.Errorf("%s: upstream was sent stream_options %s", name, calls[len(calls)-1].Body["stream_options"])
		}
		var text strings.Builder
		sawUsage, sawDone := false, false
		for _, d := range dataLines(b) {
			if d == "[DONE]" {
				sawDone = true
				continue
			}
			var c struct {
				Model   string            `json:"model"`
				Choices []json.RawMessage `json:"choices"`
				Usage   json.RawMessage   `json:"usage"`
			}
			if err := json.Unmarshal([]byte(d), &c); err != nil {
				t.Fatalf("%s: chunk %s: %v", name, d, err)
			}
			if c.Model != tc.alias {
				t.Errorf("%s: chunk model %q", name, c.Model)
			}
			if string(c.Usage) != "null" && len(c.Usage) > 0 {
				sawUsage = true
			}
			for _, ch := range c.Choices {
				var x struct {
					Delta struct{ Content string } `json:"delta"`
				}
				_ = json.Unmarshal(ch, &x)
				text.WriteString(x.Delta.Content)
			}
		}
		if text.String() != "Jupiter " || sawUsage != tc.usage || sawDone != tc.done {
			t.Errorf("%s: text %q, usage %v, [DONE] %v", name, text.String(), sawUsage, sawDone)
		}
	}
	for _, u := range e.ledger() {
		if u.Tokens == nil || *u.Tokens != (store.Tokens{Input: 6, Output: 3, CacheRead: 4}) || u.TTFT == 0 {
			t.Errorf("ledger: %+v %+v", u, u.Tokens)
		}
	}

	// The SDK assembles the stream.
	cl := e.oaClient(key, "/v1")
	for _, alias := range []string{"ds", "mm"} {
		s := cl.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{Model: alias,
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}, StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}})
		var acc openai.ChatCompletionAccumulator
		for s.Next() {
			acc.AddChunk(s.Current())
		}
		if err := s.Err(); err != nil || acc.Model != alias || len(acc.Choices) != 1 || acc.Choices[0].Message.Content != "Jupiter " || acc.Usage.CompletionTokens != 3 {
			t.Errorf("%s: %v %+v", alias, err, acc.ChatCompletion)
		}
	}
}

// A stream that breaks before its finish ends with an error the SDK reports,
// in OpenAI's stream error shape.
func TestAChatStreamBrokenOffEndsWithAnError(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chunk(c.Model, "Jup", "", ""))
	})
	e.alias("m", target(e.deployment(onOpenAI(e, "openai-generic", f.URL), "up"), 0))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer " + key})
	lines := dataLines(b)
	if resp.StatusCode != 200 || len(lines) != 2 || strings.Contains(string(b), "event:") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if typ, msg, ok := openAIEnvelope([]byte(lines[1])); !ok || typ != "api_error" || !strings.Contains(msg, "the stream ended before its finish") {
		t.Fatalf("the stream ended with %s", lines[1])
	}
	cl := e.oaClient(key, "/v1")
	s := cl.Chat.Completions.NewStreaming(context.Background(), openai.ChatCompletionNewParams{Model: "m",
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}})
	for s.Next() {
	}
	if s.Err() == nil || !strings.Contains(s.Err().Error(), "the stream ended before its finish") {
		t.Errorf("the SDK read %v", s.Err())
	}
	if rows := e.ledger(); len(rows) != 2 || rows[0].ErrorType != "api_error" {
		t.Errorf("ledger: %+v", rows)
	}
}

// The gateway's own refusals on an OpenAI route answer in OpenAI's error
// envelope, which the SDK reads; an upstream's error keeps its status and
// body, the call's credential removed from it; a retryable one moves on, and
// a vendor refusing the gateway's credential is a 502.
func TestChatErrors(t *testing.T) {
	e := newEnv(t)
	refusing := newFake(t, status(400, `{"error":{"message":"bad: sk-openai-generic-key1","type":"invalid_request_error","param":null,"code":"x"}}`))
	busy := newFake(t, status(429, `{"error":{"message":"slow down","type":"rate_limit_error"}}`))
	ok := newFake(t, chatAnswer("fine", deepseekStyle))
	locked := newFake(t, status(401, `{"error":{"message":"invalid key"}}`))
	e.alias("refusing", target(e.deployment(onOpenAI(e, "openai-generic", refusing.URL), "up"), 0))
	e.alias("busy", target(e.deployment(onOpenAI(e, "openai-generic", busy.URL), "up"), 0), target(e.deployment(onOpenAI(e, "openai-generic", ok.URL), "up"), 1))
	e.alias("locked", target(e.deployment(onOpenAI(e, "openai-generic", locked.URL), "up"), 0))
	e.alias("anthropic-only", target(e.deployment(onProfile(e, "anthropic-generic", ok.URL), "up"), 0))
	e.alias("vectors", target(e.deployment(onOpenAI(e, "openai-generic", ok.URL), "emb", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	key := e.key(everyAlias)
	narrow := e.key([]string{"busy"})
	e.start()

	ask := func(key, model string) error {
		cl := e.oaClient(key, "/v1")
		_, err := cl.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: model,
			Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}})
		return err
	}
	for _, tc := range []struct {
		key, model string
		status     int
		typ, msg   string
	}{
		{"sk-map-api01-nobody", "busy", 401, "authentication_error", "invalid x-api-key"},
		{key, "nope", 404, "not_found_error", "model: nope"},
		{narrow, "refusing", 403, "permission_error", "may not use model refusing"},
		{key, "vectors", 400, "invalid_request_error", "serves embedding, not chat"},
		{key, "anthropic-only", 503, "api_error", "no enabled upstream on the OpenAI protocol"},
		{key, "refusing", 400, "invalid_request_error", "bad: "},
		{key, "locked", 502, "api_error", "upstream refused the gateway's credential (HTTP 401)"},
	} {
		err := ask(tc.key, tc.model)
		var aerr *openai.Error
		if !errors.As(err, &aerr) || aerr.StatusCode != tc.status || aerr.Type != tc.typ || !strings.Contains(aerr.Message, tc.msg) {
			t.Errorf("%s: %v", tc.model, err)
			continue
		}
		if strings.Contains(aerr.RawJSON(), "sk-openai-generic-key1") {
			t.Errorf("%s: the credential reached the caller: %s", tc.model, aerr.RawJSON())
		}
	}
	if err := ask(key, "busy"); err != nil || len(busy.recorded()) != 1 || len(ok.recorded()) != 1 {
		t.Errorf("busy: %v; %d calls to the busy upstream and %d to the next", err, len(busy.recorded()), len(ok.recorded()))
	}
	// Errors a client sees before a model is named are OpenAI's too.
	resp, b := e.do("POST", "/v1/chat/completions", `{"messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
	if typ, _, ok := openAIEnvelope(b); resp.StatusCode != 400 || !ok || typ != "invalid_request_error" || resp.Header.Get("request-id") == "" {
		t.Errorf("no model: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.do("GET", "/v1/chat/completions", "", map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 405 {
		t.Errorf("GET: %d %s", resp.StatusCode, b)
	} else if _, _, ok := openAIEnvelope(b); !ok {
		t.Errorf("GET: %s", b)
	}
	if resp, b := e.do("POST", "/openai/v1/messages", `{}`, map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 404 {
		t.Errorf("/openai/v1/messages: %d %s", resp.StatusCode, b)
	} else if typ, _, ok := openAIEnvelope(b); !ok || typ != "not_found_error" {
		t.Errorf("/openai/v1/messages: %s", b)
	}
}

// What a vendor's OpenAI endpoint ignores although the answer depends on it
// goes to another deployment, or is refused naming the field: MiniMax's stop,
// its forcing tool_choice values and parallel_tool_calls false, and
// DeepSeek's parallel_tool_calls false (docs/HISTORY.md, 2026-10-08). The
// rest passes through.
func TestAChatRequestAVendorWouldIgnoreGoesElsewhere(t *testing.T) {
	const tools = `"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]`
	cases := []struct {
		prof, field, set string
		unset            []string
	}{
		{"minimax", "stop", `"stop":["END"]`, []string{`"stop":[]`, `"stop":null`, `"Stop":"END"`}},
		{"minimax", "stop", `"stop":"END"`, []string{`"stop":""`}},
		{"minimax", "tool_choice", tools + `,"tool_choice":"required"`, []string{tools + `,"tool_choice":"auto"`, tools + `,"tool_choice":"none"`, tools + `,"Tool_Choice":"required"`}},
		{"minimax", "tool_choice", tools + `,"tool_choice":{"type":"function","function":{"name":"t"}}`, []string{tools + `,"tool_choice":{"Type":"function","function":{"name":"t"}}`}},
		{"minimax", "tool_choice", tools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"t"}}]}}`,
			[]string{tools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`, tools + `,"tool_choice":{"type":"allowed_tools","allowed_tools":{"Mode":"required"}}`}},
		{"minimax", "parallel_tool_calls", tools + `,"parallel_tool_calls":false`, []string{tools + `,"parallel_tool_calls":true`, tools + `,"parallel_tool_calls":null`}},
		{"deepseek", "parallel_tool_calls", tools + `,"parallel_tool_calls":false`,
			[]string{tools + `,"parallel_tool_calls":true`, tools + `,"tool_choice":"required"`, `"stop":["END"]`}},
	}
	for _, tc := range cases {
		t.Run(tc.prof+" "+tc.set, func(t *testing.T) {
			e := newEnv(t)
			vendor, other := newFake(t, chatAnswer("vendor", deepseekStyle)), newFake(t, chatAnswer("other", deepseekStyle))
			p := onOpenAI(e, tc.prof, vendor.URL)
			d := e.deployment(p, "m")
			e.alias("mixed", target(d, 0), target(e.deployment(onOpenAI(e, "openai-generic", other.URL), "other-model"), 1))
			e.alias("alone", target(d, 0))
			key := e.key(everyAlias)
			e.start()
			try := func(model, extra string) (int, string, int, int) {
				nv, no := len(vendor.recorded()), len(other.recorded())
				resp, b := e.do("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],%s}`, model, extra),
					map[string]string{"Authorization": "Bearer " + key})
				return resp.StatusCode, string(b), len(vendor.recorded()) - nv, len(other.recorded()) - no
			}
			if s, b, nv, no := try("mixed", tc.set); s != 200 || nv != 0 || no != 1 {
				t.Errorf("mixed: %d %s, %d calls to %s and %d to the other", s, b, nv, tc.prof, no)
			}
			if s, b, nv, _ := try("alone", tc.set); s != 400 || nv != 0 || !strings.Contains(b, tc.field+": every upstream of model alone ignores it") ||
				strings.Contains(b, `"type":"error"`) {
				t.Errorf("alone: %d %s, %d calls", s, b, nv)
			}
			for _, extra := range tc.unset {
				if s, b, nv, _ := try("alone", extra); s != 200 || nv != 1 {
					t.Errorf("%s: %d %s, %d calls to %s", extra, s, b, nv, tc.prof)
				}
			}
		})
	}
}

// /v1/models answers in OpenAI's shape where the request carries no
// anthropic-version, and always under /openai: every alias the key may use,
// whatever its kind, but the wildcard.
func TestModelsInOpenAIsShape(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, chatAnswer("x", deepseekStyle))
	p := onOpenAI(e, "openai-generic", f.URL)
	e.alias("chat-a", target(e.deployment(p, "a"), 0))
	e.alias("chat-b", target(e.deployment(p, "b"), 0))
	e.alias("vectors", target(e.deployment(p, "emb", func(d *store.Deployment) { d.Kind = store.KindEmbedding }), 0))
	e.alias("*", target(e.deployment(p, "any"), 0))
	key := e.key([]string{"chat-a", "vectors", "*"})
	e.start()

	for _, base := range []string{"/v1", "/openai/v1"} {
		cl := e.oaClient(key, base)
		page, err := cl.Models.List(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", base, err)
		}
		var ids []string
		for _, m := range page.Data {
			ids = append(ids, m.ID)
			if m.Object != "model" || !m.JSON.Created.Valid() || !m.JSON.OwnedBy.Valid() || m.OwnedBy == "" {
				t.Errorf("%s: %s", base, m.RawJSON())
			}
		}
		if !reflect.DeepEqual(ids, []string{"chat-a", "vectors"}) {
			t.Errorf("%s: listed %v", base, ids)
		}
		if m, err := cl.Models.Get(context.Background(), "vectors"); err != nil || m.ID != "vectors" {
			t.Errorf("%s get: %v %+v", base, err, m)
		}
		for _, id := range []string{"chat-b", "*", "nope"} {
			var aerr *openai.Error
			if _, err := cl.Models.Get(context.Background(), id); !errors.As(err, &aerr) || aerr.StatusCode != 404 || aerr.Type != "not_found_error" {
				t.Errorf("%s get %s: %v", base, id, err)
			}
		}
	}
	// The Anthropic shape stays where it was asked for.
	if resp, b := e.do("GET", "/v1/models", "", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}); resp.StatusCode != 200 ||
		!strings.Contains(string(b), `"has_more"`) || strings.Contains(string(b), "vectors") {
		t.Errorf("anthropic shape: %d %s", resp.StatusCode, b)
	}
}

// An OpenAI usage object is read by its exact keys in the ledger's meaning:
// input without the cache reads, which DeepSeek also reports on a key of its
// own, read when the details are absent.
func TestChatUsageInTheLedgersMeaning(t *testing.T) {
	for _, c := range []struct {
		usage string
		want  *store.Tokens
	}{
		{`{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}`, &store.Tokens{Input: 6, Output: 3, CacheRead: 4}},
		{`{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":7,"prompt_cache_miss_tokens":3}`, &store.Tokens{Input: 3, Output: 3, CacheRead: 7}},
		{`{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"prompt_cache_hit_tokens":7}`, &store.Tokens{Input: 6, Output: 3, CacheRead: 4}},
		{`{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":null},"prompt_cache_hit_tokens":7}`, &store.Tokens{Input: 3, Output: 3, CacheRead: 7}},
		{`{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":40}}`, &store.Tokens{Output: 3, CacheRead: 10}},
		{`{"prompt_tokens":10,"completion_tokens":3}`, &store.Tokens{Input: 10, Output: 3}},
		{`{"completion_tokens":3}`, &store.Tokens{Output: 3}},
		{`{"Prompt_Tokens":10,"Completion_Tokens":3}`, nil},
		{`{"prompt_tokens":-1,"completion_tokens":1.5}`, nil},
		{`{"prompt_tokens":null,"completion_tokens":null}`, nil},
		{`{"total_tokens":13}`, nil},
		{`null`, nil},
		{``, nil},
	} {
		got := modelgateway.ChatUsageOf([]byte(c.usage))
		if (got == nil) != (c.want == nil) || got != nil && *got != *c.want {
			t.Errorf("%s: %+v, want %+v", c.usage, got, c.want)
		}
	}
}

// The gateway's ask for a stream's usage keeps the caller's other stream
// options, and a request that is not a stream gains none. Stream options it
// could not ask through — not an object, or a key that differs from
// stream_options or include_usage only in case — are refused before any
// upstream is asked.
func TestAChatStreamsOptionsAreKept(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, chatAnswer("Jupiter", minimaxStyle))
	e.alias("m", target(e.deployment(onOpenAI(e, "openai-generic", f.URL), "up"), 0))
	key := e.key(everyAlias)
	e.start()
	sent := func(body string) (json.RawMessage, bool) {
		e.do("POST", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + key})
		calls := f.recorded()
		v, ok := calls[len(calls)-1].Body["stream_options"]
		return v, ok
	}
	for opts, want := range map[string]string{
		`{"include_usage":false,"include_obfuscation":true}`: `{"include_obfuscation":true,"include_usage":true}`,
		`null`: `{"include_usage":true}`,
	} {
		if got, _ := sent(fmt.Sprintf(`{"model":"m","stream":true,"stream_options":%s,"messages":[{"role":"user","content":"hi"}]}`, opts)); string(got) != want {
			t.Errorf("%s was sent as %s, want %s", opts, got, want)
		}
	}
	if got, ok := sent(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`); ok {
		t.Errorf("a whole answer was asked with stream_options %s", got)
	}
	n := len(f.recorded())
	for _, set := range []string{`"stream_options":"all"`, `"stream_options":[]`, `"Stream_Options":{}`, `"ſtream_options":{"include_usage":false}`,
		`"stream_options":{"Include_Usage":false}`, `"stream_options":{"include_uſage":false}`} {
		resp, b := e.do("POST", "/v1/chat/completions", `{"model":"m","stream":true,`+set+`,"messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
		if typ, _, ok := openAIEnvelope(b); resp.StatusCode != 400 || !ok || typ != "invalid_request_error" {
			t.Errorf("%s: %d %s", set, resp.StatusCode, b)
		}
	}
	if len(f.recorded()) != n {
		t.Errorf("a refused request reached the upstream")
	}
}

// An upstream's error event in a stream reaches the caller with the call's
// credential removed, ends the stream there, and is recorded.
func TestAChatStreamsErrorEventIsRelayed(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, chunk(c.Model, "Jup", "", "")+
			"data: {\"error\":{\"message\":\"boom sk-openai-generic-key1\",\"type\":\"server_error\"}}\n\n"+chunk(c.Model, "iter", "stop", ""))
	})
	e.alias("m", target(e.deployment(onOpenAI(e, "openai-generic", f.URL), "up"), 0))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer " + key})
	lines := dataLines(b)
	if resp.StatusCode != 200 || len(lines) != 2 || !strings.Contains(lines[1], `"server_error"`) || strings.Contains(string(b), "sk-openai-generic-key1") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].ErrorType != "server_error" {
		t.Errorf("ledger: %+v", rows)
	}
}

// A [DONE] the upstream leaves unterminated at the end of its stream reaches
// the caller completed; a whole answer that is not JSON, or names no model,
// passes as it came.
func TestChatAnswersTheGatewayDoesNotRewrite(t *testing.T) {
	e := newEnv(t)
	whole := map[string]string{"text": "not json", "bare": `{"id":"c1", "choices":[]}`}
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		if b, ok := whole[c.Model]; ok {
			writeBody(w, 200, b)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Azure's first chunk carries its prompt filter results and no
		// choices; it is no usage chunk, so a caller that did not ask for
		// usage still gets it.
		_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"","choices":[],"prompt_filter_results":[]}`+"\n\n"+
			chunk(c.Model, "Jupiter", "stop", chatUsage)+"data: [DONE]")
	})
	p := onOpenAI(e, "openai-generic", f.URL)
	for _, m := range []string{"text", "bare", "stream"} {
		e.alias(m, target(e.deployment(p, m), 0))
	}
	key := e.key(everyAlias)
	e.start()
	_, b := e.do("POST", "/v1/chat/completions", `{"model":"stream","stream":true,"messages":[{"role":"user","content":"hi"}]}`, map[string]string{"Authorization": "Bearer " + key})
	if lines := dataLines(b); len(lines) != 3 || !strings.Contains(lines[0], "prompt_filter_results") || lines[2] != "[DONE]" || !strings.HasSuffix(string(b), "data: [DONE]\n\n") {
		t.Errorf("stream: %q", b)
	}
	for m, want := range whole {
		if resp, b := e.do("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, m),
			map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 200 || string(b) != want {
			t.Errorf("%s: %d %s", m, resp.StatusCode, b)
		}
	}
}

// A chat stream's relay ends at [DONE] or the upstream's error event: an
// upstream that holds its connection open after either does not hold the
// caller's response.
func TestAChatRelayEndsAtItsLastEvent(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		last := "data: [DONE]\n\n"
		if c.Model == "failed" {
			last = "data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n"
		}
		_, _ = io.WriteString(w, chunk(c.Model, "Jupiter", "", "")+last)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	p := onOpenAI(e, "openai-generic", f.URL)
	e.alias("done", target(e.deployment(p, "done"), 0))
	e.alias("failed", target(e.deployment(p, "failed"), 0))
	key := e.key(everyAlias)
	e.start()
	for _, alias := range []string{"done", "failed"} {
		start := time.Now()
		_, b := e.do("POST", "/v1/chat/completions", `{"model":"`+alias+`","stream":true,"messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
		if took := time.Since(start); took > 1500*time.Millisecond || len(dataLines(b)) != 2 {
			t.Errorf("%s: answered after %v: %s", alias, took, b)
		}
	}
}

// A chat completion is traced as the Messages API's are, its operation
// chat: the request span named by its route and counting the cache reads in
// its input tokens, as the conventions do, and the attempt's span by the
// operation and the upstream model.
func TestAChatCompletionIsTracedAsChat(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	f := newFake(t, chatAnswer("Jupiter", deepseekStyle))
	e.alias("fast", target(e.deployment(onOpenAI(e, "deepseek", f.URL), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start()
	if resp, b := e.do("POST", "/openai/v1/chat/completions", `{"model":"fast","messages":[]}`, map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	servers, clients := ended(t, rec, trace.SpanKindServer, 1), ended(t, rec, trace.SpanKindClient, 1)
	if len(servers) != 1 || len(clients) != 1 {
		t.Fatalf("%d server and %d client spans", len(servers), len(clients))
	}
	if s := servers[0]; s.Name() != "POST /v1/chat/completions" {
		t.Errorf("the request span is %q", s.Name())
	}
	for k, want := range map[string]string{"gen_ai.operation.name": "chat", "gen_ai.usage.input_tokens": "10", "gen_ai.usage.output_tokens": "3"} {
		if got, _ := attr(servers[0].Attributes(), k); got != want {
			t.Errorf("request span %s = %q, want %q", k, got, want)
		}
	}
	if c := clients[0]; c.Name() != "chat deepseek-flash" {
		t.Errorf("the attempt's span is %q", c.Name())
	}
	for k, want := range map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "deepseek"} {
		if got, _ := attr(clients[0].Attributes(), k); got != want {
			t.Errorf("attempt span %s = %q, want %q", k, got, want)
		}
	}
}

// openAIEnvelope reads b as OpenAI's error envelope exactly as the gateway
// writes it — the four fields openai-go's Error requires, param and code
// null, and nothing else — and returns its type and message.
func openAIEnvelope(b []byte) (typ, msg string, ok bool) {
	var env map[string]map[string]json.RawMessage
	if json.Unmarshal(b, &env) != nil || len(env) != 1 || len(env["error"]) != 4 ||
		string(env["error"]["param"]) != "null" || string(env["error"]["code"]) != "null" ||
		json.Unmarshal(env["error"]["type"], &typ) != nil || json.Unmarshal(env["error"]["message"], &msg) != nil {
		return "", "", false
	}
	return typ, msg, true
}

// A request's stream decides how its answer is relayed and its usage read,
// so a flag an upstream could read otherwise than the gateway does — a value
// that is not a boolean, or a key that differs from stream only in case,
// which Go's decoder reads as stream — is refused before any upstream is
// asked, on either protocol; null and false ask for a whole answer, and a
// count, which never streams, is not refused.
func TestAStreamFlagTheGatewayCannotReadIsRefused(t *testing.T) {
	e := newEnv(t)
	oa := newFake(t, chatAnswer("Jupiter", deepseekStyle))
	an := newFake(t, message("Jupiter"))
	e.alias("chat", target(e.deployment(onOpenAI(e, "openai-generic", oa.URL), "up"), 0))
	p := e.provider(an.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.alias("msgs", target(e.deployment(p, "up"), 0))
	key := e.key(everyAlias)
	e.start()
	for _, rt := range []struct {
		path, model string
		hdr         map[string]string
	}{
		{"/v1/chat/completions", "chat", map[string]string{"Authorization": "Bearer " + key}},
		{"/v1/messages", "msgs", map[string]string{"x-api-key": key}},
	} {
		for _, set := range []string{`"stream":1`, `"stream":"true"`, `"stream":{}`, `"Stream":true`, `"STREAM":false`, `"ſtream":true`} {
			resp, b := e.do("POST", rt.path, fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[],%s}`, rt.model, set), rt.hdr)
			if resp.StatusCode != 400 || !strings.Contains(string(b), "invalid_request_error") || !strings.Contains(strings.ToLower(string(b)), "stream") {
				t.Errorf("%s %s: %d %s", rt.path, set, resp.StatusCode, b)
			}
		}
		for _, set := range []string{`"stream":null`, `"stream":false`} {
			if resp, b := e.do("POST", rt.path, fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[],%s}`, rt.model, set), rt.hdr); resp.StatusCode != 200 ||
				!strings.Contains(string(b), "Jupiter") || strings.Contains(string(b), "data:") {
				t.Errorf("%s %s: %d %s", rt.path, set, resp.StatusCode, b)
			}
		}
	}
	if resp, b := e.do("POST", "/v1/messages/count_tokens", `{"model":"msgs","messages":[],"stream":1}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
		t.Errorf("count_tokens: %d %s", resp.StatusCode, b)
	}
	// stream_options is the Chat Completions route's alone to ask through.
	if resp, b := e.do("POST", "/v1/messages", `{"model":"msgs","max_tokens":8,"messages":[],"stream_options":"x","Stream_Options":{}}`, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
		t.Errorf("messages with stream_options: %d %s", resp.StatusCode, b)
	}
	if n, m := len(oa.recorded()), len(an.recorded()); n != 2 || m != 4 {
		t.Errorf("%d and %d upstream calls, want the 2 whole chat answers, and the 3 whole messages and a count", n, m)
	}
}

// An upstream's error in a chat stream ends it, whatever the event is named
// and whether or not its data parses, and no credential reaches the caller,
// escaped or not. An error that parses is relayed with the credential
// removed; data that is no JSON object — an error that does not parse, an
// empty error, a diagnostic — is replaced by the gateway's own error.
func TestAChatStreamsMalformedErrorIsStillRedacted(t *testing.T) {
	bad := map[string]string{
		"named-torn":   "event: error\ndata: {\"error\":{\"message\":\"sk-openai-generic\\u002dkey1\",\"type\":\"server_error\"}\n\n",
		"bare-torn":    "data: {\"error\":{\"message\":\"busy sk-openai-generic-key1\",\"type\":\"server_error\"}\n\n",
		"named-empty":  "event: error\ndata:\n\n",
		"named-done":   "event: error\ndata: [DONE]\n\n",
		"diagnostic":   "data: oops\ndata: sk-openai-generic-key1\n\n",
		"named-parses": "event: error\ndata: {\"message\":\"bad sk-openai-generic\\u002dkey1\"}\n\n",
		"empty":        "data:\n\n",
	}
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		sse(w, chunk(c.Model, "Jup", "", ""), bad[c.Model], chunk(c.Model, "iter", "stop", ""))
	})
	p := onOpenAI(e, "openai-generic", f.URL)
	for m := range bad {
		e.alias(m, target(e.deployment(p, m), 0))
	}
	key := e.key(everyAlias)
	e.start()
	for m := range bad {
		resp, b := e.do("POST", "/v1/chat/completions", `{"model":"`+m+`","stream":true,"messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
		lines := dataLines(b)
		if resp.StatusCode != 200 || len(lines) != 2 || strings.Contains(string(b), "key1") {
			t.Errorf("%s: %d %q", m, resp.StatusCode, b)
			continue
		}
		if typ, msg, ok := openAIEnvelope([]byte(lines[1])); m != "named-parses" && (!ok || typ != "api_error" || !strings.Contains(msg, "not a JSON object")) {
			t.Errorf("%s ended with %s", m, lines[1])
		}
	}
	rows := e.ledger()
	if len(rows) != len(bad) {
		t.Fatalf("%d ledger rows", len(rows))
	}
	for _, row := range rows {
		if row.ErrorType != "api_error" {
			t.Errorf("ledger: %+v", row)
		}
	}
}

// A chat stream may close on its last chunk without the blank line that
// ends it, and when that chunk is its first event it is still the answer,
// counted in the ledger, rather than the reason for another paid attempt.
func TestAChatStreamsOnlyChunkCutOffAtTheEndIsTheAnswer(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		sse(w, strings.TrimSuffix(chunk(c.Model, "Jupiter", "stop", chatUsage), "\n"))
	})
	next := newFake(t, chatAnswer("Saturn", deepseekStyle))
	e.alias("m", target(e.deployment(onOpenAI(e, "openai-generic", f.URL), "up"), 0),
		target(e.deployment(onOpenAI(e, "openai-generic", next.URL), "up"), 1))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
	if lines := dataLines(b); resp.StatusCode != 200 || len(lines) != 1 || !strings.Contains(lines[0], "Jupiter") || !strings.HasSuffix(string(b), "\n\n") {
		t.Fatalf("%d %q", resp.StatusCode, b)
	}
	if len(next.recorded()) != 0 {
		t.Errorf("the fallback was asked too")
	}
	if rows := e.ledger(); len(rows) != 1 || rows[0].Tokens == nil || *rows[0].Tokens != (store.Tokens{Input: 6, Output: 3, CacheRead: 4}) {
		t.Errorf("ledger: %+v", rows)
	}
}

// A chat stream that opens with an error has not begun the caller's answer,
// as a Messages stream that does has not (plan 59, "Retry and fallback
// happen before the first byte only"): it falls back like any refusal, and
// with nothing else to serve, the caller gets the error as the HTTP response
// it would have been — the gateway's own when the event is no JSON object.
func TestAChatStreamOpeningWithAnErrorFallsBack(t *testing.T) {
	opening := map[string]string{
		"quiet": "event: open\n\n" + chunk("quiet", "Jupiter", "stop", chatUsage) + "data: [DONE]\n\n",
		"named": "event: error\ndata: {\"error\":{\"message\":\"busy\",\"type\":\"server_error\",\"param\":null,\"code\":null}}\n\n",
		"bare":  "data: {\"error\":{\"message\":\"busy\",\"type\":\"server_error\",\"param\":null,\"code\":null}}\n\n",
		"torn":  "data: {\"error\":{\"message\":\"busy\"\n\n",
		// An OpenAI client dispatches a ping's data, and an empty data line,
		// as any chunk's: neither is a keep-alive here.
		"ping":   "event: ping\ndata: diagnostic sk-openai-generic-key1\n\n" + chunk("ping", "Jupiter", "stop", chatUsage),
		"pinged": "event: ping\ndata: {\"error\":{\"message\":\"busy\",\"type\":\"server_error\",\"param\":null,\"code\":null}}\n\n",
		"empty":  "data:\n\n" + chunk("empty", "Jupiter", "stop", chatUsage),
		// An event named error says so with no data in it.
		"dataless": "event: error\n\n" + chunk("dataless", "Jupiter", "stop", chatUsage),
	}
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) { sse(w, opening[c.Model]) })
	next := newFake(t, chatAnswer("Saturn", deepseekStyle))
	for m := range opening {
		e.alias(m, target(e.deployment(onOpenAI(e, "openai-generic", f.URL), m), 0),
			target(e.deployment(onOpenAI(e, "openai-generic", next.URL), "up"), 1))
		e.alias(m+"-alone", target(e.deployment(onOpenAI(e, "openai-generic", f.URL), m), 0))
	}
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"Authorization": "Bearer " + key}
	// An event opening the stream with no data, named other than error, is
	// no error: the answer begins there.
	if resp, b := e.do("POST", "/v1/chat/completions", `{"model":"quiet","stream":true,"messages":[]}`, hdr); resp.StatusCode != 200 || !strings.Contains(string(b), "Jupiter") {
		t.Errorf("quiet: %d %q", resp.StatusCode, b)
	}
	for _, m := range []string{"named", "bare", "torn", "ping", "pinged", "empty", "dataless"} {
		if resp, b := e.do("POST", "/v1/chat/completions", `{"model":"`+m+`","stream":true,"messages":[]}`, hdr); resp.StatusCode != 200 || !strings.Contains(string(b), `"content":"aturn"`) {
			t.Errorf("%s: %d %q", m, resp.StatusCode, b)
		}
		want := http.StatusInternalServerError
		if m == "torn" || m == "ping" || m == "empty" || m == "dataless" {
			want = http.StatusBadGateway
		}
		if resp, b := e.do("POST", "/v1/chat/completions", `{"model":"`+m+`-alone","stream":true,"messages":[]}`, hdr); resp.StatusCode != want ||
			!strings.Contains(string(b), `"error":{`) || strings.Contains(string(b), "data:") || strings.Contains(string(b), "key1") {
			t.Errorf("%s alone: %d %q", m, resp.StatusCode, b)
		}
	}
	if len(next.recorded()) != 7 {
		t.Errorf("%d calls to the fallback, want %d", len(next.recorded()), 7)
	}
}

// A stream asked for several choices (n) has said all it will once every
// choice has finished: one the upstream closes while another choice is still
// going is cut off, and ends with an error; one that closes after the last
// finish has ended, however it closed, a stall included.
func TestAChatStreamEndsWhenEveryChoiceHasFinished(t *testing.T) {
	second := func(model, content, finish string) string {
		f := "null"
		if finish != "" {
			f = fmt.Sprintf("%q", finish)
		}
		return fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":1,\"delta\":{\"content\":%q},\"finish_reason\":%s}]}\n\n", model, content, f)
	}
	choice := func(model, index, delta string) string {
		return fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":%s,\"delta\":%s,\"finish_reason\":null}]}\n\n", model, index, delta)
	}
	release := make(chan struct{})
	defer close(release)
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch c.Model {
		case "cut":
			sse(w, chunk(c.Model, "Jupiter", "stop", ""), second(c.Model, "Sat", ""))
			return
		case "none":
			sse(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"","choices":[],"prompt_filter_results":[]}`+"\n\n")
			return
		case "aliased":
			sse(w, chunk(c.Model, "A", "stop", ""), choice(c.Model, `1.5`, `{"content":"B"}`))
			return
		case "unindexed":
			sse(w, chunk(c.Model, "A", "stop", ""), choice(c.Model, `null`, `{"content":"B"}`))
			return
		case "unshaped":
			sse(w, chunk(c.Model, "A", "stop", ""), fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":\"x\"}\n\n", c.Model))
			return
		case "reopened":
			sse(w, chunk(c.Model, "A", "stop", ""), choice(c.Model, `0`, `{"content":"B"}`))
			return
		case "usage-unshaped":
			sse(w, chunk(c.Model, "A", "stop", ""), fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":\"x\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n", c.Model), "data: [DONE]\n\n")
			return
		case "choiceless":
			sse(w, chunk(c.Model, "A", "stop", ""), fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n", c.Model))
			return
		case "trailing":
			sse(w, chunk(c.Model, "A", "stop", ""), choice(c.Model, `0`, `{"role":"assistant","content":"","tool_calls":null,"annotations":[ ],"audio":{ }}`))
			return
		}
		sse(w, chunk(c.Model, "Jupiter", "stop", ""), second(c.Model, "Saturn", "stop"))
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	p := e.provider(f.URL, func(p *store.Provider) {
		p.Name, p.Profile = "openai-generic", "openai-generic"
		p.Endpoints = map[profile.Protocol]string{profile.OpenAI: f.URL}
		p.StallTimeout = 150 * time.Millisecond
	})
	e.credential(p, "sk-openai-generic-key1", 1)
	e.alias("cut", target(e.deployment(p, "cut"), 0))
	e.alias("held", target(e.deployment(p, "held"), 0))
	e.alias("none", target(e.deployment(p, "none"), 0))
	odd := map[string]string{"aliased": "could not read", "unindexed": "could not read", "unshaped": "could not read", "reopened": "before its finish", "trailing": ""}
	for m := range odd {
		e.alias(m, target(e.deployment(p, m), 0))
	}
	e.alias("usage-unshaped", target(e.deployment(p, "usage-unshaped"), 0))
	e.alias("choiceless", target(e.deployment(p, "choiceless"), 0))
	key := e.key(everyAlias)
	e.start()
	hdr := map[string]string{"Authorization": "Bearer " + key}
	_, b := e.do("POST", "/v1/chat/completions", `{"model":"cut","n":2,"stream":true,"messages":[]}`, hdr)
	if lines := dataLines(b); len(lines) != 3 {
		t.Errorf("cut: %q", b)
	} else if typ, msg, ok := openAIEnvelope([]byte(lines[2])); !ok || typ != "api_error" || !strings.Contains(msg, "before its finish") {
		t.Errorf("cut ended with %s", lines[2])
	}
	if _, b := e.do("POST", "/v1/chat/completions", `{"model":"held","n":2,"stream":true,"messages":[]}`, hdr); len(dataLines(b)) != 2 || strings.Contains(string(b), `"error"`) {
		t.Errorf("held: %q", b)
	}
	if rows := e.ledger(); len(rows) != 2 || rows[0].ErrorType != "" || rows[1].ErrorType != "api_error" {
		t.Errorf("ledger: %+v", rows)
	}
	// A stream that began no choice has not finished either.
	_, b = e.do("POST", "/v1/chat/completions", `{"model":"none","stream":true,"messages":[]}`, hdr)
	if lines := dataLines(b); len(lines) != 2 {
		t.Errorf("none: %q", b)
	} else if typ, _, ok := openAIEnvelope([]byte(lines[1])); !ok || typ != "api_error" {
		t.Errorf("none ended with %s", lines[1])
	}
	// A choice whose index is no whole number cannot be told from the
	// finished one, a choice that goes on after its finish has not finished,
	// and one that only repeats its role or empty fields after it has.
	for m, want := range odd {
		_, b := e.do("POST", "/v1/chat/completions", `{"model":"`+m+`","n":2,"stream":true,"messages":[]}`, hdr)
		lines := dataLines(b)
		switch {
		case want == "":
			if len(lines) != 2 || strings.Contains(string(b), `"error"`) {
				t.Errorf("%s: %q", m, b)
			}
		case len(lines) != 3:
			t.Errorf("%s: %q", m, b)
		default:
			if typ, msg, ok := openAIEnvelope([]byte(lines[2])); !ok || typ != "api_error" || !strings.Contains(msg, want) {
				t.Errorf("%s ended with %s", m, lines[2])
			}
		}
	}
	// A usage chunk whose choices the gateway cannot read is no usage chunk
	// it may withhold.
	if _, b := e.do("POST", "/v1/chat/completions", `{"model":"usage-unshaped","stream":true,"messages":[]}`, hdr); len(dataLines(b)) != 3 || strings.Contains(string(b), `"error"`) {
		t.Errorf("usage-unshaped: %q", b)
	}
	// A usage chunk with no choices at all is one, after which the stream
	// has ended without [DONE].
	if _, b := e.do("POST", "/v1/chat/completions", `{"model":"choiceless","stream":true,"messages":[]}`, hdr); len(dataLines(b)) != 1 || strings.Contains(string(b), `"error"`) {
		t.Errorf("choiceless: %q", b)
	}
}
