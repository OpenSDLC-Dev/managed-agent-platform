package modelgateway_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestLiveConversion drives every named vendor's chat models through the
// gateway's conversion path with the Anthropic Go SDK, each model's
// provider on the vendor's OpenAI endpoint alone, behind a recording proxy:
//   - a text answer, whole and streamed, is the vendor's Chat Completions
//     answer in Messages — its reasoning as a thinking block, its text, its
//     finish as the stop reason — under the alias, its usage the ledger's
//     reading of the vendor's, which the ledger records under the Messages
//     protocol;
//   - a tool call comes back as a tool_use block, and its continuation, made
//     with the SDK's ToParam, is answered, the vendor having been sent its
//     own reasoning back as the assistant message's reasoning_content, which
//     DeepSeek refuses a tool loop without.
func TestLiveConversion(t *testing.T) {
	vendors := chatVendors(t)
	e := newEnv(t)
	type route struct {
		v     liveVendor
		model string
		alias string
		rec   *recorder
	}
	var routes []route
	for _, v := range vendors {
		host := openAIHost(v)
		if host == "" {
			t.Fatalf("%s: no OpenAI host in its profile for %s", v.name, v.base)
		}
		for _, model := range v.models {
			r := route{v: v, model: model, alias: "conv-" + model, rec: &recorder{}}
			proxy := recordingProxy(t, host, r.rec)
			p := e.provider(proxy, func(p *store.Provider) {
				p.Name, p.Profile, p.Endpoints = r.alias, v.name, map[profile.Protocol]string{profile.OpenAI: proxy}
			})
			e.credential(p, v.keyEnv, 1)
			e.alias(r.alias, target(e.deployment(p, model), 0))
			routes = append(routes, r)
		}
	}
	key := e.key(everyAlias)
	e.start()
	cl := e.client(key)

	for _, r := range routes {
		t.Run(r.model, func(t *testing.T) {
			t.Parallel()
			ask := anthropic.MessageNewParams{Model: r.alias, MaxTokens: 2048, Thinking: r.v.thinking,
				Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Which planet is the largest in the solar system? Answer in one word."))}}
			for _, stream := range []bool{false, true} {
				name := map[bool]string{false: "whole", true: "streamed"}[stream]
				t.Run(name, func(t *testing.T) {
					var resp *http.Response
					n := r.rec.count()
					m, err := liveSend(cl, ask, stream, option.WithResponseInto(&resp))
					if err != nil {
						liveFatalf(t, "%v", err)
					}
					x := one(t, r.rec, n)
					_, raw := x.answer()
					want := vendorConverted(raw, stream)
					if m.Model != r.alias || summary(*m) != want.text || m.StopReason != want.stop || m.Usage.InputTokens != want.usage.Input ||
						m.Usage.OutputTokens != want.usage.Output || m.Usage.CacheReadInputTokens != want.usage.CacheRead ||
						!strings.Contains(strings.ToLower(textOf(m)), "jupiter") {
						liveFatalf(t, "the gateway answered %s %s %+v; the vendor sent %s", masked(summary(*m)), m.StopReason, m.Usage, masked(string(raw)))
					}
					var sent map[string]json.RawMessage
					_ = json.Unmarshal(x.sent, &sent)
					if x.path != "/chat/completions" && !strings.HasSuffix(x.path, "/chat/completions") || string(sent["model"]) != `"`+r.model+`"` {
						liveFatalf(t, "sent %s to %s", masked(string(x.sent)), x.path)
					}
					convertedLedger(t, e, resp, want.usage)
					t.Logf("%s %s: %s, <think> in its text: %v", r.model, name, kindsOf(m), strings.Contains(textOf(m), "<think>"))
				})
			}

			t.Run("tool round trip", func(t *testing.T) {
				p := anthropic.MessageNewParams{Model: r.alias, MaxTokens: 2048, Thinking: r.v.thinking, Tools: []anthropic.ToolUnionParam{liveTool},
					Messages: []anthropic.MessageParam{liveAsk()}}
				var m *anthropic.Message
				for try := 0; try < 3 && (m == nil || !hasToolUse(m)); try++ {
					var err error
					if m, err = liveSend(cl, p, try%2 == 1); err != nil {
						liveFatalf(t, "tool call: %v", err)
					}
				}
				if !hasToolUse(m) {
					liveFatalf(t, "three answers without a tool call, the last %s", masked(summary(*m)))
				}
				p.Messages = append(p.Messages, m.ToParam())
				var results []anthropic.ContentBlockParamUnion
				var reasoning string
				for _, b := range m.Content {
					switch b.Type {
					case "tool_use":
						results = append(results, anthropic.NewToolResultBlock(b.ID, "2026-10-08T12:00:00Z", false))
					case "thinking":
						reasoning += b.Thinking
					}
				}
				p.Messages = append(p.Messages, anthropic.NewUserMessage(results...))
				n := r.rec.count()
				next, err := liveSend(cl, p, false)
				if err != nil {
					liveFatalf(t, "the continuation: %v", err)
				}
				if textOf(next) == "" {
					liveFatalf(t, "the continuation: %s", masked(summary(*next)))
				}
				var sent struct {
					Messages []map[string]json.RawMessage `json:"messages"`
				}
				_ = json.Unmarshal(one(t, r.rec, n).sent, &sent)
				var got string
				if len(sent.Messages) > 1 {
					_ = json.Unmarshal(sent.Messages[1]["reasoning_content"], &got)
				}
				if got != reasoning {
					liveFatalf(t, "the continuation sent the vendor reasoning %q; it had answered %q", abbreviate([]string{got}), abbreviate([]string{reasoning}))
				}
				t.Logf("%s tool call: %s, <think> in its text: %v", r.model, kindsOf(m), strings.Contains(textOf(m), "<think>"))
			})
		})
	}
}

// converted is what a vendor's Chat Completions answer is, as the gateway
// converts it.
type converted struct {
	text  string // summary's line for the answer
	stop  anthropic.StopReason
	usage store.Tokens
}

// vendorConverted reads a recorded Chat Completions answer, whole or
// streamed, and converts it as the gateway must have.
func vendorConverted(raw []byte, stream bool) converted {
	var b []byte
	if stream {
		s := convert.NewStream("", "", func() string { return "sig" })
		var events []byte
		var usage json.RawMessage
		for _, line := range strings.Split(string(raw), "\n") {
			data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
			if !ok || strings.TrimSpace(data) == "[DONE]" {
				continue
			}
			var obj map[string]json.RawMessage
			if json.Unmarshal([]byte(data), &obj) == nil && len(obj["usage"]) > 0 && string(obj["usage"]) != "null" {
				usage = obj["usage"]
			}
			out, _ := s.Chunk([]byte(strings.TrimSpace(data)))
			events = append(events, out...)
		}
		events = append(events, s.End()...)
		var m anthropic.Message
		for _, ev := range strings.Split(strings.TrimSuffix(string(events), "\n\n"), "\n\n") {
			if _, data, ok := strings.Cut(ev, "\ndata: "); ok {
				var u anthropic.MessageStreamEventUnion
				if json.Unmarshal([]byte(data), &u) == nil {
					_ = m.Accumulate(u)
				}
			}
		}
		return converted{summary(m), m.StopReason, chatTokens(usage)}
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	u := chatTokens(obj["usage"])
	b, _ = convert.Answer(raw, "", "", &convert.Usage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead}, func() string { return "sig" })
	var m anthropic.Message
	_ = json.Unmarshal(b, &m)
	return converted{summary(m), m.StopReason, u}
}

// chatTokens reads a Chat Completions usage in the ledger's meaning.
func chatTokens(raw json.RawMessage) store.Tokens {
	var u struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		Details    struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		Hit *int64 `json:"prompt_cache_hit_tokens"`
	}
	_ = json.Unmarshal(raw, &u)
	cached := u.Details.Cached
	if cached == 0 && u.Hit != nil {
		cached = *u.Hit
	}
	return store.Tokens{Input: u.Prompt - cached, Output: u.Completion, CacheRead: cached}
}

// summary is an answer's blocks as one comparable line, its signatures
// left out, which name the deployment.
func summary(m anthropic.Message) string {
	var parts []string
	for _, b := range m.Content {
		switch b.Type {
		case "thinking":
			parts = append(parts, "thinking("+b.Thinking+")")
		case "text":
			parts = append(parts, "text("+b.Text+")")
		case "tool_use":
			parts = append(parts, "tool_use("+b.Name+" "+string(b.Input)+")")
		default:
			parts = append(parts, b.Type)
		}
	}
	return strings.Join(parts, " ")
}

// convertedLedger checks the ledger row of a converted request: the Messages
// protocol and endpoint, and the vendor's usage in the ledger's meaning.
func convertedLedger(t *testing.T, e *env, resp *http.Response, want store.Tokens) {
	t.Helper()
	rid := resp.Header.Get("request-id")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		for _, row := range e.ledger() {
			if row.RequestID == rid {
				if row.Protocol != "anthropic" || row.Endpoint != "messages" || row.Tokens == nil || *row.Tokens != want {
					liveFatalf(t, "ledger %s: %+v %+v, want %+v", rid, row, row.Tokens, want)
				}
				return
			}
		}
	}
	liveFatalf(t, "no ledger row for %s", rid)
}
