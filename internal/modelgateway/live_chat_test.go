package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/openai/openai-go/v3"
	oaoption "github.com/openai/openai-go/v3/option"
)

// TestLiveChatCompletions drives every named vendor's models through the
// gateway's Chat Completions passthrough with openai-go, each model's
// provider on the vendor's OpenAI endpoint behind a recording proxy:
//   - a text answer, whole and streamed, is the vendor's — its text, finish
//     and usage — under the name the caller sent, and the ledger counts its
//     usage in the Messages API's meaning, the cache reads apart;
//   - a stream whose caller did not ask for usage is counted all the same,
//     and shows the caller no chunk without choices;
//   - a tool call comes back, and its continuation, made with openai-go's
//     ToParam, is answered;
//   - each field the gateway refuses for the vendor (profile ChatIgnores) is
//     refused with no upstream call, and the vendor asked directly still
//     ignores it, which is the evidence the refusal rests on.
func TestLiveChatCompletions(t *testing.T) {
	vendors := namedVendors(t)
	e := newEnv(t)
	type route struct {
		v     liveVendor
		model string
		alias string // unlike the model's id, so its rewrite shows
		rec   *recorder
	}
	var routes []route
	for _, v := range vendors {
		host := openAIHost(v)
		if host == "" {
			t.Fatalf("%s: no OpenAI host in its profile for %s", v.name, v.base)
		}
		for _, model := range v.models {
			r := route{v: v, model: model, alias: "chat-" + model, rec: &recorder{}}
			proxy := recordingProxy(t, host, r.rec)
			p := e.provider(proxy, func(p *store.Provider) {
				p.Name, p.Profile, p.Endpoints = model, v.name, map[profile.Protocol]string{profile.OpenAI: proxy}
			})
			e.credential(p, v.keyEnv, 1)
			e.alias(r.alias, target(e.deployment(p, model), 0))
			routes = append(routes, r)
		}
	}
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")

	for _, r := range routes {
		t.Run(r.model, func(t *testing.T) {
			t.Parallel()
			ask := openai.ChatCompletionNewParams{Model: r.alias, MaxTokens: openai.Int(2048),
				Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("Which planet is the largest in the solar system? Answer in one word.")}}

			t.Run("whole", func(t *testing.T) {
				var resp *http.Response
				n := r.rec.count()
				m, err := cl.Chat.Completions.New(liveCtx(t), ask, oaoption.WithResponseInto(&resp))
				if err != nil {
					liveFatalf(t, "whole: %v", err)
				}
				_, raw := one(t, r.rec, n).answer()
				var up openai.ChatCompletion
				if err := json.Unmarshal(raw, &up); err != nil || len(up.Choices) != 1 || len(m.Choices) != 1 {
					liveFatalf(t, "whole: the vendor sent %s (%v); the gateway answered %s", masked(string(raw)), err, masked(m.RawJSON()))
				}
				if m.Model != r.alias || m.Choices[0].Message.Content != up.Choices[0].Message.Content ||
					m.Choices[0].FinishReason != up.Choices[0].FinishReason || m.Usage.RawJSON() != up.Usage.RawJSON() ||
					!strings.Contains(strings.ToLower(m.Choices[0].Message.Content), "jupiter") {
					liveFatalf(t, "whole: the gateway answered %s; the vendor sent %s", masked(m.RawJSON()), masked(string(raw)))
				}
				liveLedger(t, e, resp, up.Usage)
			})

			t.Run("streamed, usage not asked", func(t *testing.T) {
				var resp *http.Response
				n := r.rec.count()
				s := cl.Chat.Completions.NewStreaming(liveCtx(t), ask, oaoption.WithResponseInto(&resp))
				var acc openai.ChatCompletionAccumulator
				bare := 0
				for s.Next() {
					c := s.Current()
					if len(c.Choices) == 0 {
						bare++
					}
					acc.AddChunk(c)
				}
				if err := s.Err(); err != nil {
					liveFatalf(t, "streamed: %v", err)
				}
				x := one(t, r.rec, n)
				var sent struct {
					StreamOptions struct {
						IncludeUsage bool `json:"include_usage"`
					} `json:"stream_options"`
				}
				_, raw := x.answer()
				up := vendorChat(raw)
				if json.Unmarshal(x.sent, &sent) != nil || !sent.StreamOptions.IncludeUsage || bare != 0 || len(acc.Choices) != 1 || len(up.Choices) != 1 ||
					acc.Model != r.alias || acc.Choices[0].Message.Content != up.Choices[0].Message.Content || up.Usage.CompletionTokens == 0 {
					liveFatalf(t, "streamed: sent %s; the caller saw %d chunks without choices and %q from %q; the vendor sent %q, usage %s",
						masked(string(x.sent)), bare, masked(textOfChat(&acc.ChatCompletion)), acc.Model, masked(textOfChat(&up)), up.Usage.RawJSON())
				}
				liveLedger(t, e, resp, up.Usage)
			})

			t.Run("streamed, usage asked", func(t *testing.T) {
				p := ask
				p.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
				n := r.rec.count()
				s := cl.Chat.Completions.NewStreaming(liveCtx(t), p)
				var acc openai.ChatCompletionAccumulator
				for s.Next() {
					acc.AddChunk(s.Current())
				}
				_, raw := one(t, r.rec, n).answer()
				up := vendorChat(raw)
				if err := s.Err(); err != nil || acc.Usage.CompletionTokens == 0 || acc.Usage.PromptTokens != up.Usage.PromptTokens ||
					acc.Usage.CompletionTokens != up.Usage.CompletionTokens {
					liveFatalf(t, "streamed with usage: %v; the caller's usage %s, the vendor's %s", err, acc.Usage.RawJSON(), up.Usage.RawJSON())
				}
			})

			t.Run("tool round trip", func(t *testing.T) {
				p := openai.ChatCompletionNewParams{Model: r.alias, MaxTokens: openai.Int(2048), Tools: []openai.ChatCompletionToolUnionParam{liveChatTool},
					Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(liveAskText)}}
				var m *openai.ChatCompletion
				for try := 0; try < 3 && (m == nil || len(m.Choices) == 0 || len(m.Choices[0].Message.ToolCalls) == 0); try++ {
					var err error
					if m, err = cl.Chat.Completions.New(liveCtx(t), p); err != nil {
						liveFatalf(t, "tool call: %v", err)
					}
				}
				if len(m.Choices) == 0 || len(m.Choices[0].Message.ToolCalls) == 0 {
					liveFatalf(t, "three answers without a tool call, the last %s", masked(m.RawJSON()))
				}
				p.Messages = append(p.Messages, m.Choices[0].Message.ToParam())
				for _, tc := range m.Choices[0].Message.ToolCalls {
					p.Messages = append(p.Messages, openai.ToolMessage("2026-10-08T12:00:00Z", tc.ID))
				}
				next, err := cl.Chat.Completions.New(liveCtx(t), p)
				if err != nil || len(next.Choices) == 0 || next.Choices[0].Message.Content == "" {
					liveFatalf(t, "the continuation: %v %s", err, masked(next.RawJSON()))
				}
			})
		})
	}

	// The refusals, on each vendor's first model: the gateway sends nothing,
	// and the vendor asked directly still ignores what is refused.
	for _, v := range vendors {
		t.Run(v.name+" refusals", func(t *testing.T) {
			t.Parallel()
			model := v.models[0]
			for _, c := range chatRefusals(v.name) {
				var rec *recorder
				for _, r := range routes {
					if r.model == model {
						rec = r.rec
					}
				}
				n := rec.count()
				resp, b := e.do("POST", "/v1/chat/completions", string(c.body("chat-"+model)), map[string]string{"Authorization": "Bearer " + key})
				if resp.StatusCode != 400 || len(rec.since(n)) != 0 || !strings.Contains(string(b), c.field+": every upstream of model chat-"+model+" ignores it") {
					liveFatalf(t, "%s: %d %s, %d upstream calls", c.name, resp.StatusCode, b, len(rec.since(n)))
				}
				var seen []string
				ignored := false
				for try := 0; try < 3 && !ignored; try++ {
					m, err := askVendorChat(v, c.body(model))
					if err != nil {
						liveFatalf(t, "%s, asked directly: %v", c.name, err)
					}
					ignored = c.ignored(m)
					seen = append(seen, fmt.Sprintf("%s/%d calls/%q", m.Choices[0].FinishReason, len(m.Choices[0].Message.ToolCalls), abbreviate([]string{m.Choices[0].Message.Content})[0]))
				}
				if !ignored {
					liveErrorf(t, "%s, asked directly of %s: never ignored in %v; the vendor may now honor it, so its refusal in profile.go wants re-measuring", c.name, model, seen)
				}
			}
		})
	}
}

// openAIHost is the OpenAI host of v's profile in the region of v's base URL.
func openAIHost(v liveVendor) string {
	prof, _ := profile.Lookup(v.name)
	region, found := profile.Region(""), false
	for _, h := range prof.Hosts {
		if h.Protocol == profile.Anthropic && strings.TrimRight(h.BaseURL, "/") == strings.TrimRight(v.base, "/") {
			region, found = h.Region, true
		}
	}
	for _, h := range prof.Hosts {
		if found && h.Protocol == profile.OpenAI && h.Region == region {
			return h.BaseURL
		}
	}
	return ""
}

var liveChatTool = openai.ChatCompletionFunctionTool(openai.FunctionDefinitionParam{Name: "get_time",
	Description: openai.String("Returns the current time in UTC."), Parameters: openai.FunctionParameters{"type": "object", "properties": map[string]any{}}})

// chatRefusal is one field the gateway refuses for a vendor, the request
// that sets it, and how an answer shows the vendor ignored it.
type chatRefusal struct {
	name, field string
	body        func(model string) []byte
	ignored     func(m *openai.ChatCompletion) bool
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func chatRefusals(vendor string) []chatRefusal {
	timeTool := map[string]any{"type": "function", "function": map[string]any{"name": "get_time", "description": "Returns the current time in one timezone.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"timezone": map[string]any{"type": "string"}}, "required": []string{"timezone"}}}}
	request := func(prompt string, extra map[string]any) func(string) []byte {
		return func(model string) []byte {
			body := map[string]any{"model": model, "max_tokens": 2048, "messages": []map[string]any{{"role": "user", "content": prompt}}}
			for k, v := range extra {
				body[k] = v
			}
			b, _ := json.Marshal(body)
			return b
		}
	}
	finishedWithoutTool := func(m *openai.ChatCompletion) bool {
		return m.Choices[0].FinishReason == "stop" && len(m.Choices[0].Message.ToolCalls) == 0
	}
	parallel := chatRefusal{"parallel_tool_calls false", "parallel_tool_calls",
		request("What time is it right now in UTC and in Asia/Tokyo? Call the tool once for each timezone, both at once.",
			map[string]any{"tools": []any{timeTool}, "parallel_tool_calls": false}),
		func(m *openai.ChatCompletion) bool { return len(m.Choices[0].Message.ToolCalls) > 1 }}
	switch vendor {
	case "deepseek":
		return []chatRefusal{parallel}
	case "minimax":
		return []chatRefusal{parallel,
			{"stop", "stop", request("Count from 1 to 10, separated by spaces, digits only. No other text.", map[string]any{"stop": []string{" 5"}}),
				func(m *openai.ChatCompletion) bool {
					return strings.Contains(thinkBlock.ReplaceAllString(m.Choices[0].Message.Content, ""), " 6")
				}},
			{"tool_choice required", "tool_choice", request(noToolNeeded, map[string]any{"tools": []any{timeTool}, "tool_choice": "required"}), finishedWithoutTool},
			{"tool_choice naming a function", "tool_choice", request(noToolNeeded, map[string]any{"tools": []any{timeTool},
				"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_time"}}}), finishedWithoutTool},
		}
	}
	return nil
}

// askVendorChat sends body to v's OpenAI endpoint directly, without the
// gateway, with the key as a Bearer token, following no redirect, and
// returns an answer that names its finish.
func askVendorChat(v liveVendor, body []byte) (*openai.ChatCompletion, error) {
	req, err := http.NewRequest("POST", strings.TrimRight(openAIHost(v), "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.keyEnv)
	cl := &http.Client{Timeout: liveCallTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%d %s", resp.StatusCode, b)
	}
	var m openai.ChatCompletion
	if err := json.Unmarshal(b, &m); err != nil || len(m.Choices) == 0 || m.Choices[0].FinishReason == "" {
		return nil, fmt.Errorf("an answer with no finish (%v): %s", err, b)
	}
	return &m, nil
}

// vendorChat assembles a Chat Completions stream as the vendor sent it.
func vendorChat(raw []byte) openai.ChatCompletion {
	var acc openai.ChatCompletionAccumulator
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok || strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var c openai.ChatCompletionChunk
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &c) == nil {
			acc.AddChunk(c)
		}
	}
	return acc.ChatCompletion
}

// liveLedger finds the ledger row of the request resp answered and checks
// its tokens are the vendor's usage in the Messages API's meaning. The row
// is written as the answer ends, which a stream's client may not wait for:
// openai-go stops reading at [DONE].
func liveLedger(t *testing.T, e *env, resp *http.Response, u openai.CompletionUsage) {
	t.Helper()
	rid := resp.Header.Get("request-id")
	cached := u.PromptTokensDetails.CachedTokens
	want := store.Tokens{Input: u.PromptTokens - cached, Output: u.CompletionTokens, CacheRead: cached}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		for _, row := range e.ledger() {
			if row.RequestID == rid {
				if row.Protocol != "openai" || row.Endpoint != "chat_completions" || row.Tokens == nil || *row.Tokens != want {
					liveFatalf(t, "ledger %s: %+v %+v, want %+v", rid, row, row.Tokens, want)
				}
				return
			}
		}
	}
	liveFatalf(t, "no ledger row for %q", rid)
}

func textOfChat(m *openai.ChatCompletion) string {
	if m == nil || len(m.Choices) == 0 {
		return ""
	}
	return m.Choices[0].Message.Content
}
