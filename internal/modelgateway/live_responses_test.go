package modelgateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/openai/openai-go/v3"
	oaoption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// TestLiveResponses drives every named vendor's chat models through the
// gateway's Responses API with the OpenAI Go SDK, each model on two routes
// behind a recording proxy: its vendor's Anthropic endpoint, which the
// Messages request a Responses one converts to passes through to, and its
// OpenAI endpoint alone, which that Messages request reaches converted once
// more, to Chat Completions:
//   - a text answer, whole and streamed, is the vendor's, item for block —
//     its thinking a reasoning item, its text a message — under the alias,
//     its usage the vendor's in the Responses API's meaning, which the
//     ledger records under the responses endpoint, on the OpenAI protocol;
//   - a tool call, reasoning asked for by an effort, comes back as a
//     function_call item beside the reasoning items, and the continuation,
//     made by sending the output back as input with the SDK's ToParam, is
//     answered, the vendor having been sent its own thinking back: its
//     signatures on its Anthropic endpoint, unwrapped from the reasoning
//     items' encrypted_content, and its reasoning_content on its OpenAI one.
func TestLiveResponses(t *testing.T) {
	vendors := chatVendors(t)
	e := newEnv(t)
	type route struct {
		v      liveVendor
		model  string
		alias  string
		dep    string
		openAI bool // the route's provider has the vendor's OpenAI endpoint alone
		rec    *recorder
	}
	var routes []route
	for _, v := range vendors {
		host := openAIHost(v)
		if host == "" {
			t.Fatalf("%s: no OpenAI host in its profile for %s", v.name, v.base)
		}
		for _, model := range v.models {
			for _, openAI := range []bool{false, true} {
				r := route{v: v, model: model, alias: "resp-" + model, openAI: openAI, rec: &recorder{}}
				base := v.base
				if openAI {
					r.alias, base = "resp-oa-"+model, host
				}
				proxy := recordingProxy(t, base, r.rec)
				p := e.provider(proxy, func(p *store.Provider) {
					p.Name, p.Profile = r.alias, v.name
					if openAI {
						p.Endpoints = map[profile.Protocol]string{profile.OpenAI: proxy}
					}
				})
				e.credential(p, v.keyEnv, 1)
				d := e.deployment(p, model)
				r.dep = d.ID
				e.alias(r.alias, target(d, 0))
				routes = append(routes, r)
			}
		}
	}
	key := e.key(everyAlias)
	e.start()
	cl := e.oaClient(key, "/v1")

	for _, r := range routes {
		t.Run(r.alias, func(t *testing.T) {
			t.Parallel()
			// vendor is what the vendor answered in an exchange, as the
			// gateway must answer it: its blocks as summary writes them, its
			// stop as a Response's status, and its usage.
			vendor := func(x *exchange, stream bool) (string, string, store.Tokens) {
				_, raw := x.answer()
				if r.openAI {
					c := vendorConverted(raw, stream)
					return c.text, responseStatusOf(c.stop), c.usage
				}
				m, _ := vendorAnswer(raw)
				u := m.Usage
				return summary(m), responseStatusOf(string(m.StopReason)),
					store.Tokens{Input: u.InputTokens, Output: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}
			}

			for _, stream := range []bool{false, true} {
				name := map[bool]string{false: "whole", true: "streamed"}[stream]
				t.Run(name, func(t *testing.T) {
					p := responses.ResponseNewParams{Model: r.alias, MaxOutputTokens: openai.Int(2048),
						Input: responses.ResponseNewParamsInputUnion{OfString: openai.String("Which planet is the largest in the solar system? Answer in one word.")}}
					// Whether the vendor's answer names Jupiter is the model's
					// choice, so up to three answers are asked for.
					for try := 1; ; try++ {
						var resp *http.Response
						n := r.rec.count()
						got, err := liveRespond(cl, p, stream, oaoption.WithResponseInto(&resp))
						if err != nil {
							liveFatalf(t, "%v", err)
						}
						x := one(t, r.rec, n)
						text, status, usage := vendor(x, stream)
						if got.Model != r.alias || respSummary(got) != text || string(got.Status) != status || !sameResponseUsage(got.Usage, usage) {
							liveFatalf(t, "the gateway answered %s (%s, usage %s); the vendor sent %s (%s, usage %+v)",
								masked(respSummary(got)), got.Status, got.Usage.RawJSON(), masked(text), status, usage)
						}
						if rid := resp.Header.Get("request-id"); got.ID != "resp_"+strings.TrimPrefix(rid, "req_") {
							liveFatalf(t, "response id %s for request %s", got.ID, rid)
						}
						responsesLedger(t, e, resp, usage)
						if strings.Contains(strings.ToLower(got.OutputText()), "jupiter") {
							t.Logf("%s %s: %s", r.alias, name, itemKinds(got))
							break
						}
						if try == 3 {
							liveFatalf(t, "three answers without Jupiter, the last %s", masked(respSummary(got)))
						}
					}
				})
			}

			t.Run("tool round trip", func(t *testing.T) {
				tool := responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{Name: "get_time",
					Description: openai.String("Returns the current time."), Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}}
				ask := responses.ResponseInputItemParamOfMessage(liveAskText, responses.EasyInputMessageRoleUser)
				p := responses.ResponseNewParams{Model: r.alias, MaxOutputTokens: openai.Int(2048), Tools: []responses.ToolUnionParam{tool},
					Reasoning: shared.ReasoningParam{Effort: shared.ReasoningEffortHigh},
					Input:     responses.ResponseNewParamsInputUnion{OfInputItemList: []responses.ResponseInputItemUnionParam{ask}}}
				// On its Anthropic endpoint a model that thinks must return
				// reasoning, within three asks, as the Messages tier's round
				// trip requires; adaptive thinking is the model's choice.
				thinks := !r.openAI && !slices.Contains(r.v.quiet, r.model)
				done := func(got *responses.Response) bool {
					return got != nil && hasCall(got) && (!thinks || hasItem(got, "reasoning"))
				}
				var got *responses.Response
				var x *exchange
				for try := 0; try < 3 && !done(got); try++ {
					n := r.rec.count()
					var err error
					if got, err = liveRespond(cl, p, try%2 == 0); err != nil {
						liveFatalf(t, "tool call: %v", err)
					}
					x = one(t, r.rec, n)
				}
				if !done(got) {
					liveFatalf(t, "three answers without a function call, or a model that thinks without reasoning; the last %s", masked(respSummary(got)))
				}
				// On the Anthropic endpoint, each reasoning item's
				// encrypted_content is the vendor's signature, wrapped as the
				// Messages API's would be.
				var sentBack []string
				if !r.openAI {
					_, raw := x.answer()
					vend, interleaved := vendorThinking(raw)
					var returned []string
					returned, sentBack = expected(vend, r.dep)
					var enc []string
					for _, o := range got.Output {
						if o.Type == "reasoning" {
							enc = append(enc, o.EncryptedContent)
						}
					}
					if !interleaved && !slices.Equal(enc, returned) {
						liveFatalf(t, "encrypted_content %v; the vendor sent %v", abbreviate(enc), abbreviate(vend))
					}
				}
				// The output goes back as input, every call's output after it.
				input := []responses.ResponseInputItemUnionParam{ask}
				var results []responses.ResponseInputItemUnionParam
				var reasoning string
				for _, o := range got.Output {
					input = append(input, asInput(t, o))
					switch o.Type {
					case "function_call":
						result := responses.ResponseInputItemParamOfFunctionCallOutput("2026-10-08T12:00:00Z")
						result.OfFunctionCallOutput.CallID = openai.String(o.CallID)
						results = append(results, result)
					case "reasoning":
						for _, s := range o.Summary {
							reasoning += s.Text
						}
					}
				}
				p.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: append(input, results...)}
				n := r.rec.count()
				next, err := liveRespond(cl, p, false)
				if err != nil {
					liveFatalf(t, "the continuation: %v", err)
				}
				if next.OutputText() == "" && !hasCall(next) {
					liveFatalf(t, "the continuation: %s", masked(respSummary(next)))
				}
				sent := one(t, r.rec, n).sent
				if r.openAI {
					var body struct {
						Messages []map[string]json.RawMessage `json:"messages"`
					}
					_ = json.Unmarshal(sent, &body)
					var back string
					if len(body.Messages) > 1 {
						_ = json.Unmarshal(body.Messages[1]["reasoning_content"], &back)
					}
					if back != reasoning {
						liveFatalf(t, "the continuation sent the vendor reasoning %q; it had answered %q", abbreviate([]string{back}), abbreviate([]string{reasoning}))
					}
				} else if back := sentThinking(sent); !slices.Equal(back, sentBack) {
					liveFatalf(t, "the continuation sent the vendor %v; want %v", abbreviate(back), abbreviate(sentBack))
				}
				t.Logf("%s tool call: %s; reasoning %d bytes", r.alias, itemKinds(got), len(reasoning))
			})
		})
	}
}

// liveRespond sends p to the gateway's Responses API, whole or streamed,
// and returns the Response: a stream's is its last event's, which must be
// response.completed or response.incomplete, its text the deltas joined.
func liveRespond(cl openai.Client, p responses.ResponseNewParams, stream bool, opts ...oaoption.RequestOption) (*responses.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), liveCallTimeout)
	defer cancel()
	if !stream {
		return cl.Responses.New(ctx, p, opts...)
	}
	s := cl.Responses.NewStreaming(ctx, p, opts...)
	defer s.Close()
	var last responses.ResponseStreamEventUnion
	var text string
	for s.Next() {
		last = s.Current()
		if last.Type == "response.output_text.delta" {
			text += last.Delta
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if last.Type != "response.completed" && last.Type != "response.incomplete" {
		return nil, fmt.Errorf("the stream ended with %q: %s", last.Type, last.RawJSON())
	}
	r := last.Response
	if got := r.OutputText(); got != text {
		return nil, fmt.Errorf("the stream's deltas say %q, its last event %q", text, got)
	}
	return &r, nil
}

// respSummary is a Response's output as summary writes a message's blocks:
// a reasoning item with a summary is a thinking block, one without a
// redacted_thinking block.
func respSummary(r *responses.Response) string {
	var parts []string
	for _, o := range r.Output {
		switch o.Type {
		case "reasoning":
			if len(o.Summary) == 0 {
				parts = append(parts, "redacted_thinking")
				continue
			}
			var b strings.Builder
			for _, s := range o.Summary {
				b.WriteString(s.Text)
			}
			parts = append(parts, "thinking("+b.String()+")")
		case "message":
			for _, c := range o.Content {
				parts = append(parts, "text("+c.Text+")")
			}
		case "function_call":
			parts = append(parts, "tool_use("+o.Name+" "+o.Arguments.OfString+")")
		default:
			parts = append(parts, o.Type)
		}
	}
	return strings.Join(parts, " ")
}

// responseStatusOf is the status a Response has for a Messages stop reason.
func responseStatusOf[S ~string](stop S) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded", "refusal":
		return "incomplete"
	}
	return "completed"
}

// sameResponseUsage reports whether a Response's usage is the vendor's,
// read in the ledger's meaning: input_tokens counts the cache too.
func sameResponseUsage(u responses.ResponseUsage, t store.Tokens) bool {
	return u.InputTokens == t.Input+t.CacheRead+t.CacheWrite && u.InputTokensDetails.CachedTokens == t.CacheRead &&
		u.OutputTokens == t.Output && u.TotalTokens == u.InputTokens+u.OutputTokens
}

// responsesLedger checks a Responses request's ledger row: the responses
// endpoint, the OpenAI protocol, and the vendor's usage.
func responsesLedger(t *testing.T, e *env, resp *http.Response, want store.Tokens) {
	t.Helper()
	rid := resp.Header.Get("request-id")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		for _, row := range e.ledger() {
			if row.RequestID == rid {
				if row.Protocol != "openai" || row.Endpoint != "responses" || row.Tokens == nil || *row.Tokens != want {
					liveFatalf(t, "ledger %s: %+v %+v, want %+v", rid, row, row.Tokens, want)
				}
				return
			}
		}
	}
	liveFatalf(t, "no ledger row for %q", rid)
}

func hasCall(r *responses.Response) bool { return hasItem(r, "function_call") }

func hasItem(r *responses.Response, typ string) bool {
	return slices.ContainsFunc(r.Output, func(o responses.ResponseOutputItemUnion) bool { return o.Type == typ })
}

func itemKinds(r *responses.Response) string {
	var ks []string
	for _, o := range r.Output {
		ks = append(ks, o.Type)
	}
	return strings.Join(ks, ",")
}
