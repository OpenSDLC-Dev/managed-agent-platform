package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
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
//     repeated prompt reading from the cache, and input_tokens, cache write
//     and cache read add up to the same prompt on every call, as the Messages
//     API's do;
//   - tool_choice auto and none reach the vendor exactly as asked; a choice
//     forcing a tool call that the vendor ignores (any on both, tool on
//     MiniMax) is refused with no upstream call, and the vendor asked
//     directly still ignores it, which is the evidence the refusal rests
//     on; DeepSeek's tool is honored with thinking disabled and refused by
//     DeepSeek with it on;
//   - a MiniMax key on its other region's host is a credential the vendor
//     refuses, which the gateway answers as its own failure.
func TestLiveVendorBehavior(t *testing.T) {
	vendors := chatVendors(t)
	e := newEnv(t)
	type route struct {
		v     liveVendor
		alias string
		rec   *recorder
		other *recorder // the key on its other region's host, where there is one
	}
	var routes []route
	for _, v := range vendors {
		r := route{v: v, alias: v.models[0], rec: &recorder{}}
		p := e.provider(recordingProxy(t, v.base, r.rec), func(p *store.Provider) { p.Name, p.Profile = r.alias, v.name })
		e.credential(p, v.keyEnv, 1)
		e.alias(r.alias, target(e.deployment(p, r.alias), 0))
		if host := otherRegion(v); host != "" {
			r.other = &recorder{}
			op := e.provider(recordingProxy(t, host, r.other), func(p *store.Provider) { p.Name, p.Profile = r.alias+"-other-region", v.name })
			e.credential(op, v.keyEnv, 1)
			e.alias(r.alias+"-other-region", target(e.deployment(op, r.alias), 0))
		}
		routes = append(routes, r)
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
				xs := r.rec.since(n)
				if err != nil || len(xs) != 1 {
					liveFatalf(t, "count_tokens: %v, %d upstream calls, want one", err, len(xs))
				}
				status, raw := xs[0].answer()
				var up struct {
					InputTokens int64 `json:"input_tokens"`
				}
				_ = json.Unmarshal(raw, &up)
				if status != 200 || ct.InputTokens <= 0 || ct.InputTokens != up.InputTokens || !strings.HasSuffix(xs[0].path, "/v1/messages/count_tokens") {
					liveFatalf(t, "count_tokens: %+v; the vendor answered %d %s at %s", ct, status, raw, xs[0].path)
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
				// Whether the model searches, and how it words its answer, are
				// its own, so each turn is asked up to three times.
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
				var m2 *anthropic.Message
				for try := 0; try < 3 && !strings.Contains(strings.ToUpper(textOf(m2)), "PELICAN"); try++ {
					n := r.rec.count()
					var err error
					m2, err = cl.Messages.New(liveCtx(t), p)
					xs := r.rec.since(n)
					if err != nil || len(xs) != 1 {
						liveFatalf(t, "the replay: %v, %d upstream calls, want one", err, len(xs))
					}
					if sent := xs[0].sent; bytes.Contains(sent, []byte(`"search_result"`)) || !bytes.Contains(sent, []byte("PELICAN-42")) ||
						!bytes.Contains(sent, []byte("https://kb.example/code-word")) {
						liveFatalf(t, "the vendor was not sent the search result flattened to text: %s", abbreviate([]string{string(sent)}))
					}
				}
				if !strings.Contains(strings.ToUpper(textOf(m2)), "PELICAN") {
					liveFatalf(t, "three replays answered without the code word: %q (stop %q)", masked(textOf(m2)), m2.StopReason)
				}
			})

			t.Run("cache", func(t *testing.T) {
				// A prompt no earlier run sent, so the first call reads nothing
				// from the cache and a later one reads what this run wrote.
				sys := []anthropic.TextBlockParam{{Text: fmt.Sprintf("Run %d. Answer in one word.\n", time.Now().UnixNano()) +
					strings.Repeat("This sentence pads the prompt for a prompt cache check. ", 300),
					CacheControl: anthropic.NewCacheControlEphemeralParam()}}
				// A vendor builds its cache within seconds, best effort, and
				// MiniMax's hits come and go from one call to the next
				// (docs/HISTORY.md), so the calls go on, two seconds apart, until
				// one reads other than the first, when the sum below can tell the
				// meanings apart.
				var first, prompt int64
				changed, reread := false, false
				for i := 0; i < 6 && !changed; i++ {
					if i > 0 {
						time.Sleep(2 * time.Second)
					}
					n := r.rec.count()
					m, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: model, MaxTokens: 512, System: sys,
						Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say ok."))}})
					if err != nil {
						liveFatalf(t, "call %d: %v", i, err)
					}
					_, raw := one(t, r.rec, n).answer()
					up, _ := vendorAnswer(raw)
					u, v := m.Usage, up.Usage
					if !v.JSON.CacheCreationInputTokens.Valid() || !v.JSON.CacheReadInputTokens.Valid() ||
						!u.JSON.CacheCreationInputTokens.Valid() || !u.JSON.CacheReadInputTokens.Valid() || !sameUsage(u, v) {
						liveFatalf(t, "call %d: the gateway answered usage %s, the vendor reported %s", i, u.RawJSON(), v.RawJSON())
					}
					// In the Messages API's meaning, which the usage ledger prices,
					// input_tokens counts only what was neither read from the cache
					// nor written to it, so the three add up to the same prompt on
					// every call; input_tokens that counted the reads too would grow
					// the sum as the reads change.
					if sum := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; i == 0 {
						first, prompt = u.CacheReadInputTokens, sum
					} else if sum != prompt {
						liveFatalf(t, "call %d: input, cache write and cache read add up to %d, against %d on the first call", i, sum, prompt)
					} else {
						changed, reread = u.CacheReadInputTokens != first, reread || u.CacheReadInputTokens > 0
					}
					t.Logf("cache call %d: input %d, cache write %d, cache read %d", i, u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens)
				}
				if !reread {
					liveFatalf(t, "no repeat of a %d-character prompt read from the cache", len(sys[0].Text))
				}
				if !changed {
					t.Logf("every call read %d tokens from the cache, so the sums could not tell input_tokens' meanings apart", first)
				}
			})

			t.Run("tool_choice", func(t *testing.T) {
				ask := func(choice anthropic.ToolChoiceUnionParam, thinking anthropic.ThinkingConfigParamUnion, prompt string) (*anthropic.Message, []*exchange, error) {
					n := r.rec.count()
					m, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: model, MaxTokens: 2048, Tools: []anthropic.ToolUnionParam{liveTool},
						ToolChoice: choice, Thinking: thinking, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}})
					return m, r.rec.since(n), err
				}
				statusOf := func(err error) int {
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
					m, xs, err := ask(c.choice, none, liveAskText)
					if err != nil || len(xs) != 1 {
						liveErrorf(t, "tool_choice %s: %v, %d upstream calls, want one", c.name, err, len(xs))
						continue
					}
					var sent struct {
						ToolChoice json.RawMessage `json:"tool_choice"`
					}
					_ = json.Unmarshal(xs[0].sent, &sent)
					if want := `{"type":"` + c.name + `"}`; string(sent.ToolChoice) != want {
						liveErrorf(t, "tool_choice %s: the vendor was sent %s, want %s", c.name, sent.ToolChoice, want)
					}
					// MiniMax-M3 honors none, but not always (docs/HISTORY.md):
					// the vendor's lapse, not the gateway's.
					if c.name == "none" && hasToolUse(m) {
						t.Logf("tool_choice none: the model called the tool anyway")
					}
				}
				for _, name := range forcedIgnored(r.v.name) {
					_, xs, err := ask(forcing(name), none, noToolNeeded)
					if statusOf(err) != 400 || len(xs) != 0 || !strings.Contains(err.Error(), "tool_choice.type") {
						liveErrorf(t, "tool_choice %s: %v, %d upstream calls; want the gateway's 400 naming tool_choice.type, and none", name, err, len(xs))
					}
				}
				if r.v.name == "deepseek" {
					// A question needing no tool, so a call proves the choice forced it.
					m, xs, err := ask(forcing("tool"), anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}, noToolNeeded)
					if err != nil || len(xs) != 1 || !hasToolUse(m) {
						liveErrorf(t, "tool_choice tool, thinking disabled: %v, %d upstream calls; want get_time called", err, len(xs))
					}
					_, xs, err = ask(forcing("tool"), none, noToolNeeded)
					if len(xs) != 1 {
						liveFatalf(t, "tool_choice tool, thinking on: %v, %d upstream calls, want 1", err, len(xs))
					}
					if vs, _ := xs[0].answer(); statusOf(err) != 400 || vs != 400 {
						liveErrorf(t, "tool_choice tool, thinking on: %v; the vendor answered %d; want DeepSeek's own 400 relayed", err, vs)
					}
				}
			})

			t.Run("the refused choices asked directly", func(t *testing.T) {
				// The refusals rest on the vendor ignoring these choices. Asked
				// directly, a question needing no tool is answered in full without
				// a call while a choice is ignored, and never once it is honored;
				// a vendor that starts honoring one leaves its refusal stale. An
				// answer cut short proves neither, so up to three are asked for.
				for _, name := range forcedIgnored(r.v.name) {
					ignored := false
					var stops []string
					for try := 0; try < 3 && !ignored; try++ {
						m, err := askVendor(r.v, r.alias, forcing(name), noToolNeeded)
						if err != nil {
							liveFatalf(t, "tool_choice %s, asked directly: %v", name, err)
						}
						ignored = m.StopReason == anthropic.StopReasonEndTurn && !hasToolUse(m)
						stops = append(stops, fmt.Sprintf("%s/tool=%v", m.StopReason, hasToolUse(m)))
					}
					if !ignored {
						liveErrorf(t, "asked directly with tool_choice %s, %s never answered in full without the tool (%v): the vendor may now honor it, so its refusal in profile.go wants re-measuring", name, r.alias, stops)
					}
				}
			})

			if r.v.name == "minimax" {
				t.Run("a key on its other region's host", func(t *testing.T) {
					if r.other == nil {
						t.Skipf("%s is neither of the profile's MiniMax hosts, so there is no other region to try", r.v.base)
					}
					n := r.other.count()
					_, err := cl.Messages.New(liveCtx(t), anthropic.MessageNewParams{Model: model + "-other-region", MaxTokens: 64,
						Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Say ok."))}})
					xs := r.other.since(n)
					if len(xs) != 1 {
						liveFatalf(t, "the other region: %v, %d upstream calls, want one", err, len(xs))
					}
					vs, _ := xs[0].answer()
					var apiErr *anthropic.Error
					if vs != 401 || !errors.As(err, &apiErr) || apiErr.StatusCode != 502 {
						liveErrorf(t, "the key on its other region's host: the vendor answered %d, the gateway %v; want 401, then the gateway's 502", vs, err)
					}
				})
			}
		})
	}
}

// noToolNeeded is a question no offered tool helps with: a tool call made
// to it was forced.
const noToolNeeded = "Hello! Tell me a fun fact about the number seven, in one sentence."

// forcedIgnored is the forcing tool_choice values the vendor's profile
// refuses, because the vendor ignores them.
func forcedIgnored(vendor string) []string {
	if vendor == "minimax" {
		return []string{"any", "tool"}
	}
	return []string{"any"}
}

// forcing is the tool_choice forcing a call by name: any, or tool (get_time).
func forcing(name string) anthropic.ToolChoiceUnionParam {
	if name == "tool" {
		return anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: "get_time"}}
	}
	return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}
}

// askVendor asks v's model directly, without the gateway, with the tier's
// tool offered under choice and the key sent as v's profile sends it, and
// returns the answer. It follows no redirect, which would carry the key to
// another host, as the gateway's own client follows none.
func askVendor(v liveVendor, model string, choice anthropic.ToolChoiceUnionParam, prompt string) (*anthropic.Message, error) {
	body, err := json.Marshal(anthropic.MessageNewParams{Model: anthropic.Model(model), MaxTokens: 2048, Tools: []anthropic.ToolUnionParam{liveTool},
		ToolChoice: choice, Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(v.base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	if prof, _ := profile.Lookup(v.name); prof.BearerAuth {
		req.Header.Set("Authorization", "Bearer "+v.keyEnv)
	} else {
		req.Header.Set("X-Api-Key", v.keyEnv)
	}
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
	var m anthropic.Message
	if err := json.Unmarshal(b, &m); err != nil || m.StopReason == "" {
		return nil, fmt.Errorf("an answer with no stop reason (%v): %s", err, b)
	}
	return &m, nil
}

// otherRegion is the Anthropic host of v's profile in the region v's base URL
// is not, for MiniMax, whose key works in its own region only; empty for
// another vendor, or for a base URL that is neither region's host.
func otherRegion(v liveVendor) string {
	if v.name != "minimax" {
		return ""
	}
	prof, _ := profile.Lookup(v.name)
	mine := profile.Region("")
	for _, h := range prof.Hosts {
		if h.Protocol == profile.Anthropic && strings.TrimRight(h.BaseURL, "/") == strings.TrimRight(v.base, "/") {
			mine = h.Region
		}
	}
	for _, h := range prof.Hosts {
		if mine != "" && h.Protocol == profile.Anthropic && h.Region != mine {
			return h.BaseURL
		}
	}
	return ""
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
