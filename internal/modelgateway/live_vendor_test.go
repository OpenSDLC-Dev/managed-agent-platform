package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
)

// TestLiveVendorBehavior asks each named vendor's first model, through the
// gateway, what the live tier leaves to evidence (docs/plan/59_model-gateway.md,
// "Vendor behavior"; the findings are in docs/HISTORY.md), and holds the
// gateway to what that evidence decided:
//   - count_tokens reaches the vendor's own route and its count comes back
//     unchanged;
//   - a search_result replayed in a tool result reaches the vendor flattened
//     to text, which the model then answers from;
//   - the cache usage fields come back as the vendor reported them, a
//     repeated prompt reading from the cache;
//   - tool_choice auto and none pass through, none honored by a model asked to
//     call the tool, while a choice forcing a tool call that the vendor
//     ignores (any on both, tool on MiniMax) is refused with no upstream call,
//     and DeepSeek's tool is honored with thinking disabled and refused by
//     DeepSeek with it on;
//   - MiniMax's CN key on its international host is a credential the vendor
//     refuses, which the gateway answers as its own failure.
func TestLiveVendorBehavior(t *testing.T) {
	vendors := namedVendors(t)
	e := newEnv(t)
	type route struct {
		v     liveVendor
		alias string
		rec   *recorder
	}
	var routes []route
	var intl *recorder
	for _, v := range vendors {
		model := v.models[0]
		rec := &recorder{}
		p := e.provider(recordingProxy(t, v.base, rec), func(p *store.Provider) { p.Name, p.Profile = model, v.name })
		e.credential(p, v.keyEnv, 1)
		e.alias(model, target(e.deployment(p, model), 0))
		routes = append(routes, route{v, model, rec})
		if v.name == "minimax" && strings.Contains(v.base, "minimax.cn") {
			intl = &recorder{}
			ip := e.provider(recordingProxy(t, "https://api.minimax.io/anthropic", intl), func(p *store.Provider) { p.Name, p.Profile = "minimax-intl", v.name })
			e.credential(ip, v.keyEnv, 1)
			e.alias("minimax-intl", target(e.deployment(ip, model), 0))
		}
	}
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	for _, r := range routes {
		t.Run(r.v.name, func(t *testing.T) {
			t.Parallel()
			model := anthropic.Model(r.alias)

			t.Run("count_tokens", func(t *testing.T) {
				n := r.rec.count()
				ct, err := cl.Messages.CountTokens(liveCtx(t), anthropic.MessageCountTokensParams{Model: model, Messages: []anthropic.MessageParam{liveAsk()}})
				x := one(t, r.rec, n)
				status, raw := x.answer()
				var up struct {
					InputTokens int64 `json:"input_tokens"`
				}
				_ = json.Unmarshal(raw, &up)
				if err != nil || status != 200 || ct.InputTokens <= 0 || ct.InputTokens != up.InputTokens || !strings.HasSuffix(x.path, "/v1/messages/count_tokens") {
					liveFatalf(t, "count_tokens: %v, %+v; the vendor answered %d %s at %s", err, ct, status, raw, x.path)
				}
				t.Logf("count_tokens: %d input tokens", ct.InputTokens)
			})

			t.Run("search_result", func(t *testing.T) {
				search := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "search",
					Description: anthropic.String("Searches the project's knowledge base."),
					InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"query": map[string]any{"type": "string"}}, Required: []string{"query"}}}}
				ask := anthropic.NewUserMessage(anthropic.NewTextBlock("What is the project's code word? Search the knowledge base for it, then answer with the code word alone."))
				p := anthropic.MessageNewParams{Model: model, MaxTokens: 2048, Thinking: r.v.thinking, Tools: []anthropic.ToolUnionParam{search},
					Messages: []anthropic.MessageParam{ask}}
				// Whether the model chooses to search is not what this checks, so
				// it is asked up to three times.
				var m1 *anthropic.Message
				var results []anthropic.ContentBlockParamUnion
				for try := 0; try < 3 && len(results) == 0; try++ {
					var err error
					if m1, err = cl.Messages.New(liveCtx(t), p); err != nil {
						liveFatalf(t, "the search turn: %v", err)
					}
					for _, b := range m1.Content {
						if b.Type == "tool_use" {
							results = append(results, anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{ToolUseID: b.ID,
								Content: []anthropic.ToolResultBlockParamContentUnion{{OfSearchResult: &anthropic.SearchResultBlockParam{
									Source: "https://kb.example/code-word", Title: "The code word",
									Content: []anthropic.TextBlockParam{{Text: "The project's code word is PELICAN-42."}}}}}}})
						}
					}
				}
				if len(results) == 0 {
					liveFatalf(t, "the model answered three times without searching (stop %q)", m1.StopReason)
				}
				p.Messages = append(p.Messages, m1.ToParam(), anthropic.NewUserMessage(results...))
				n := r.rec.count()
				m2, err := cl.Messages.New(liveCtx(t), p)
				sent := one(t, r.rec, n).sent
				if bytes.Contains(sent, []byte(`"search_result"`)) || !bytes.Contains(sent, []byte("PELICAN-42")) || !bytes.Contains(sent, []byte("https://kb.example/code-word")) {
					liveFatalf(t, "the vendor was not sent the search result flattened to text: %s", abbreviate([]string{string(sent)}))
				}
				if err != nil || !strings.Contains(strings.ToUpper(textOf(m2)), "PELICAN-42") {
					liveFatalf(t, "the replay: %v; answered %q", err, masked(textOf(m2)))
				}
			})

			t.Run("cache", func(t *testing.T) {
				sys := []anthropic.TextBlockParam{{Text: "Answer in one word.\n" + strings.Repeat("This sentence pads the prompt for a prompt cache check. ", 300),
					CacheControl: anthropic.NewCacheControlEphemeralParam()}}
				var read int64
				for i := 0; i < 4 && (i < 2 || read == 0); i++ {
					n := r.rec.count()
					m, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: model, MaxTokens: 512, System: sys,
						Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say ok."))}})
					if err != nil {
						liveFatalf(t, "call %d: %v", i, err)
					}
					_, raw := one(t, r.rec, n).answer()
					var up anthropic.Message
					_ = json.Unmarshal(raw, &up)
					u, v := m.Usage, up.Usage
					if !v.JSON.CacheCreationInputTokens.Valid() || !v.JSON.CacheReadInputTokens.Valid() ||
						u.InputTokens != v.InputTokens || u.OutputTokens != v.OutputTokens ||
						u.CacheCreationInputTokens != v.CacheCreationInputTokens || u.CacheReadInputTokens != v.CacheReadInputTokens {
						liveFatalf(t, "call %d: the SDK read %d/%d/%d/%d, the vendor reported %s", i,
							u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, v.RawJSON())
					}
					if i > 0 {
						read = u.CacheReadInputTokens
					}
					t.Logf("cache call %d: input %d, cache write %d, cache read %d", i, u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
				}
				if read == 0 {
					liveFatalf(t, "three repeats of a %d-character prompt read nothing from the cache", len(sys[0].Text))
				}
			})

			t.Run("tool_choice", func(t *testing.T) {
				getTime := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: "get_time",
					Description: anthropic.String("Returns the current time."), InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}}}}
				ask := func(choice anthropic.ToolChoiceUnionParam, thinking anthropic.ThinkingConfigParamUnion) (*anthropic.Message, error, []*exchange) {
					n := r.rec.count()
					m, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: model, MaxTokens: 2048, Tools: []anthropic.ToolUnionParam{getTime},
						ToolChoice: choice, Thinking: thinking, Messages: []anthropic.MessageParam{liveAsk()}})
					return m, err, r.rec.since(n)
				}
				status := func(err error) int {
					var apiErr *anthropic.Error
					if errors.As(err, &apiErr) {
						return apiErr.StatusCode
					}
					return 0
				}
				none := anthropic.ThinkingConfigParamUnion{}
				for _, c := range []struct {
					name   string
					choice anthropic.ToolChoiceUnionParam
				}{{"auto", anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}}, {"none", anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}}} {
					m, err, xs := ask(c.choice, none)
					if err != nil || len(xs) != 1 || (c.name == "none" && hasToolUse(m)) {
						liveErrorf(t, "tool_choice %s: %v, %d upstream calls, tool called %v", c.name, err, len(xs), hasToolUse(m))
					}
				}
				refused := []string{"any"}
				if r.v.name == "minimax" {
					refused = append(refused, "tool")
				}
				for _, name := range refused {
					choice := anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
					if name == "tool" {
						choice = anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "get_time"}}
					}
					_, err, xs := ask(choice, none)
					if status(err) != 400 || len(xs) != 0 || !strings.Contains(err.Error(), "tool_choice.type") {
						liveErrorf(t, "tool_choice %s: %v, %d upstream calls; want the gateway's 400 naming tool_choice.type, and none", name, err, len(xs))
					}
				}
				if r.v.name == "deepseek" {
					tool := anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "get_time"}}
					m, err, xs := ask(tool, anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}})
					if err != nil || len(xs) != 1 || !hasToolUse(m) {
						liveErrorf(t, "tool_choice tool, thinking disabled: %v, %d upstream calls; want get_time called", err, len(xs))
					}
					_, err, xs = ask(tool, none)
					if len(xs) != 1 {
						liveFatalf(t, "tool_choice tool, thinking on: %d upstream calls, want 1", len(xs))
					}
					if vs, _ := xs[0].answer(); status(err) != 400 || vs != 400 {
						liveErrorf(t, "tool_choice tool, thinking on: %v; the vendor answered %d; want DeepSeek's own 400 relayed", err, vs)
					}
				}
			})

			if r.v.name == "minimax" && intl != nil {
				t.Run("cn key on the international host", func(t *testing.T) {
					n := intl.count()
					_, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: "minimax-intl", MaxTokens: 64,
						Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say ok."))}})
					vs, _ := one(t, intl, n).answer()
					var apiErr *anthropic.Error
					if vs != 401 || !errors.As(err, &apiErr) || apiErr.StatusCode != 502 {
						liveErrorf(t, "the CN key on the international host: the vendor answered %d, the gateway %v; want 401, then the gateway's 502", vs, err)
					}
				})
			}
		})
	}
}

func hasToolUse(m *anthropic.Message) bool {
	if m == nil {
		return false
	}
	for _, c := range m.Content {
		if c.Type == "tool_use" {
			return true
		}
	}
	return false
}
