package modelgateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/anthropics/anthropic-sdk-go"
)

// onProfile is a provider of the named profile at url, with one credential.
func onProfile(e *env, prof, url string) store.Provider {
	e.t.Helper()
	p := e.provider(url, func(p *store.Provider) { p.Name = prof; p.Profile = prof })
	e.credential(p, "sk-"+prof+"-key1", 1)
	return p
}

// holdsSearchResult reports whether any object in raw has the type
// search_result, as a vendor decoding the body finds it.
func holdsSearchResult(raw json.RawMessage) bool {
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if t["type"] == "search_result" {
				return true
			}
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		case []any:
			for _, x := range t {
				if walk(x) {
					return true
				}
			}
		}
		return false
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return walk(v)
}

func decoded(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

// A vendor that refuses search_result blocks — DeepSeek's and MiniMax's
// Anthropic endpoints both do, in a tool_result and at the top level alike
// (probed 2026-10-07), as their fakes do here — is sent each one in a
// tool_result as text, in the rendering the brain's flatten_search_results
// uses, from its fields' exact keys and keeping its cache_control. Every
// other block and field goes as sent, for the vendor to refuse — a block
// whose type key differs only in case, and a search_result the rendering
// cannot read, included — and a vendor that takes the block gets it
// untouched; a count_tokens request is edited as its /v1/messages twin.
func TestASearchResultReachesARefusingVendorAsText(t *testing.T) {
	const messages = `[
		{"role":"user","content":[{"type":"search_result","source":"https://example.com/top","title":"Top","content":[{"type":"text","text":"kept"}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[
			{"type":"search_result","source":"https://example.com/paris","title":"Paris","content":[{"type":"text","text":"Paris is the capital."}],"citations":{"enabled":true},"cache_control":{"type":"ephemeral"}},
			{"type":"search_result","Source":"https://example.com/other","source":"https://example.com/k","title":"K","content":[{"type":"text","text":"original","Text":"replacement"},{"type":"text","Type":"image","text":"typed"}]},
			{"type":"text","text":"note"},
			{"type":"text","text":"x","Type":"search_result"},
			{"type":"search_result","source":"https://example.com/i","title":"I","content":[{"type":"image","source":{"type":"url","url":"https://example.com/i.png"}}]},
			{"type":"search_result","source":"https://example.com/n","title":5,"content":[{"type":"text","text":"n"}]}]},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"plain"}]}]`
	const flattened = `[
		{"role":"user","content":[{"type":"search_result","source":"https://example.com/top","title":"Top","content":[{"type":"text","text":"kept"}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[
			{"type":"text","text":"Paris (https://example.com/paris)\nParis is the capital.\n","cache_control":{"type":"ephemeral"}},
			{"type":"text","text":"K (https://example.com/k)\noriginal\ntyped\n"},
			{"type":"text","text":"note"},
			{"type":"text","text":"x","Type":"search_result"},
			{"type":"search_result","source":"https://example.com/i","title":"I","content":[{"type":"image","source":{"type":"url","url":"https://example.com/i.png"}}]},
			{"type":"search_result","source":"https://example.com/n","title":5,"content":[{"type":"text","text":"n"}]}]},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"plain"}]}]`
	refusals := map[string]struct {
		status int
		body   string
	}{
		"deepseek": {422, `{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[0].content: unknown variant ` +
			"`search_result`" + `","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`},
		"minimax": {400, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid params, invalid tool_result content (2013)"}}`},
	}
	e := newEnv(t)
	fakes := map[string]*fake{}
	for _, prof := range []string{"deepseek", "minimax", "anthropic-generic"} {
		answer, refusal := message("ok"), refusals[prof]
		fakes[prof] = newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
			if refusal.status != 0 && holdsSearchResult(c.Body["messages"]) {
				writeBody(w, refusal.status, refusal.body)
				return
			}
			answer(w, r, c)
		})
		e.alias(prof, target(e.deployment(onProfile(e, prof, fakes[prof].URL), "up"), 0))
	}
	key := e.key(everyAlias)
	e.start()

	// The fixture's top-level and unreadable blocks go as sent, so the
	// vendors refuse it, in their own words; what they were sent is the point.
	for prof, want := range map[string]struct {
		status   int
		messages string
	}{"deepseek": {422, flattened}, "minimax": {400, flattened}, "anthropic-generic": {200, messages}} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":%s}`, prof, messages)
			if resp, b := e.do("POST", path, body, map[string]string{"x-api-key": key}); resp.StatusCode != want.status {
				t.Fatalf("%s %s: %d %s, want %d", prof, path, resp.StatusCode, b, want.status)
			}
			calls := fakes[prof].recorded()
			got := calls[len(calls)-1]
			if got.Path != path || !reflect.DeepEqual(decoded(t, got.Body["messages"]), decoded(t, []byte(want.messages))) {
				t.Errorf("%s %s was sent %s", prof, path, got.Body["messages"])
			}
		}
	}

	// A type spelled through an escape is read as the vendor reads it, and
	// the edit follows thinking provenance, which removes the unwrapped block.
	for name, msgs := range map[string]string{
		"an escaped type": `[{"role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"search\u005fresult","source":"https://example.com/t","title":"T","content":[{"type":"text","text":"x"}]}]}]}]`,
		"thinking ahead": `[{"role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"unwrapped"},{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"search_result","source":"https://example.com/t","title":"T","content":[{"type":"text","text":"x"}]}]}]}]`,
	} {
		const want = `[{"role":"user","content":"q"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"T (https://example.com/t)\nx\n"}]}]}]`
		resp, b := e.do("POST", "/v1/messages", fmt.Sprintf(`{"model":"deepseek","max_tokens":64,"messages":%s}`, msgs), map[string]string{"x-api-key": key})
		calls := fakes["deepseek"].recorded()
		got := calls[len(calls)-1].Body["messages"]
		if resp.StatusCode != 200 || !reflect.DeepEqual(decoded(t, got), decoded(t, []byte(want))) {
			t.Errorf("%s: %d %s; deepseek was sent %s", name, resp.StatusCode, b, got)
		}
		// The SDK reads the flattened block as the text block it is.
		var sdk []anthropic.MessageParam
		if err := json.Unmarshal(got, &sdk); err != nil || len(sdk) != 3 || len(sdk[2].Content) != 1 || sdk[2].Content[0].OfToolResult == nil ||
			len(sdk[2].Content[0].OfToolResult.Content) != 1 || sdk[2].Content[0].OfToolResult.Content[0].OfText == nil ||
			sdk[2].Content[0].OfToolResult.Content[0].OfText.Text != "T (https://example.com/t)\nx\n" {
			t.Errorf("%s: the SDK reads %s as %+v (%v)", name, got, sdk, err)
		}
	}

	// A request with no messages, for the upstream to refuse, gains none.
	e.do("POST", "/v1/messages", `{"model":"deepseek","max_tokens":64}`, map[string]string{"x-api-key": key})
	calls := fakes["deepseek"].recorded()
	if m, ok := calls[len(calls)-1].Body["messages"]; ok {
		t.Errorf("a request with no messages was sent messages %s", m)
	}
}

// Each profile sends the provider's key in the header its vendor
// documents, and in that one alone.
func TestEachProfileSendsTheKeyAsItsVendorDocuments(t *testing.T) {
	bearer := map[string]bool{"deepseek": false, "minimax": true, "zhipu": false, "moonshot": true, "anthropic-generic": false}
	e := newEnv(t)
	f := newFake(t, message("ok"))
	for prof := range bearer {
		e.alias(prof, target(e.deployment(onProfile(e, prof, f.URL), "up"), 0))
	}
	key := e.key(everyAlias)
	e.start()

	for prof, b := range bearer {
		e.do("POST", "/v1/messages", fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, prof),
			map[string]string{"x-api-key": key})
		calls := f.recorded()
		h, secret := calls[len(calls)-1].Header, "sk-"+prof+"-key1"
		want := map[string]string{"X-Api-Key": secret, "Authorization": ""}
		if b {
			want = map[string]string{"X-Api-Key": "", "Authorization": "Bearer " + secret}
		}
		for k, v := range want {
			if got := h.Get(k); got != v {
				t.Errorf("%s: %s %q, want %q", prof, k, got, v)
			}
		}
	}
}

// A vendor that ignores something bounding the answer — DeepSeek tool_choice's
// disable_parallel_tool_use and its type any, MiniMax stop_sequences, its
// tool_choice types any and tool and its disable_parallel_tool_use, and
// thinking disabled on MiniMax's M2.x — is
// never sent a request that sets it:
// another of the model's deployments serves it, and where there is none the
// gateway refuses it, naming the field. A count is routed alike, but made
// where every deployment would ignore the field, which leaves it unchanged.
// The field read by its exact key, or left unset, changes nothing.
func TestARequestAVendorWouldIgnoreGoesElsewhere(t *testing.T) {
	const tools = `"tools":[{"name":"t","input_schema":{"type":"object"}}]`
	cases := []struct {
		prof, model, field, set string
		unset                   []string
	}{
		{"deepseek", "deepseek-flash", "tool_choice.disable_parallel_tool_use", tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
			[]string{tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":false}`, tools + `,"tool_choice":{"type":"auto","Disable_Parallel_Tool_Use":true}`}},
		{"deepseek", "deepseek-v4-pro", "tool_choice.type", tools + `,"tool_choice":{"type":"any"}`,
			[]string{tools + `,"tool_choice":{"type":"tool","name":"t"}`, tools + `,"tool_choice":{"type":"auto"}`, tools + `,"tool_choice":{"Type":"any"}`}},
		{"minimax", "MiniMax-M3", "stop_sequences", `"stop_sequences":["END"]`, []string{`"stop_sequences":[]`, `"Stop_Sequences":["END"]`}},
		{"minimax", "MiniMax-M3.1-Flash-Preview", "tool_choice.type", tools + `,"tool_choice":{"type":"tool","name":"t"}`,
			[]string{tools + `,"tool_choice":{"type":"auto"}`, tools + `,"tool_choice":{"type":"none"}`, tools + `,"tool_choice":{"Type":"tool","name":"t"}`}},
		{"minimax", "MiniMax-M3", "tool_choice.type", tools + `,"tool_choice":{"type":"any"}`,
			[]string{tools + `,"tool_choice":{"type":"auto"}`, tools + `,"tool_choice":{"Type":"any"}`}},
		{"minimax", "MiniMax-M3.1-Flash-Preview", "tool_choice.disable_parallel_tool_use", tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
			[]string{tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":false}`, tools + `,"tool_choice":{"type":"auto","Disable_Parallel_Tool_Use":true}`}},
		{"minimax", "MiniMax-M2.7", "thinking.type", `"thinking":{"type":"disabled"}`,
			[]string{`"thinking":{"type":"adaptive"}`, `"thinking":{"Type":"disabled"}`, `"Thinking":{"type":"disabled"}`}},
	}
	for _, tc := range cases {
		t.Run(tc.model+" "+tc.field, func(t *testing.T) {
			e := newEnv(t)
			vendor, other := newFake(t, message("vendor")), newFake(t, message("other"))
			p := onProfile(e, tc.prof, vendor.URL)
			d := e.deployment(p, tc.model)
			e.alias("mixed", target(d, 0), target(e.deployment(onProfile(e, "anthropic-generic", other.URL), "other-model"), 1))
			e.alias("alone", target(d, 0), target(e.deployment(p, tc.model+"-b"), 1))
			key := e.key(everyAlias)
			e.start()

			try := func(path, model, extra string) (int, string, int, int) {
				nv, no := len(vendor.recorded()), len(other.recorded())
				body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}],%s}`, model, extra)
				resp, b := e.do("POST", path, body, map[string]string{"x-api-key": key})
				return resp.StatusCode, string(b), len(vendor.recorded()) - nv, len(other.recorded()) - no
			}
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				if s, b, nv, no := try(path, "mixed", tc.set); s != 200 || nv != 0 || no != 1 {
					t.Errorf("%s mixed: %d %s, %d calls to %s and %d to the other", path, s, b, nv, tc.prof, no)
				}
			}
			s, b, nv, _ := try("/v1/messages", "alone", tc.set)
			if want := tc.field + ": every upstream of model alone ignores it"; s != 400 || nv != 0 ||
				!strings.Contains(b, `"invalid_request_error"`) || !strings.Contains(b, want) {
				t.Errorf("alone: %d %s, %d calls", s, b, nv)
			}
			if s, b, nv, _ := try("/v1/messages/count_tokens", "alone", tc.set); s != 200 || nv != 1 {
				t.Errorf("a count, alone: %d %s, %d calls", s, b, nv)
			}
			for _, extra := range tc.unset {
				if s, b, nv, _ := try("/v1/messages", "alone", extra); s != 200 || nv != 1 {
					t.Errorf("%s: %d %s, %d calls to %s", extra, s, b, nv, tc.prof)
				}
			}
		})
	}

	// Deployments each dropped for a field of its own: the refusal names
	// both, in a stable order, and says no more of either than is true. MiniMax-M3 honors
	// thinking disabled.
	e := newEnv(t)
	mm, ds := newFake(t, message("minimax")), newFake(t, message("deepseek"))
	pm := onProfile(e, "minimax", mm.URL)
	e.alias("both", target(e.deployment(onProfile(e, "deepseek", ds.URL), "d"), 0), target(e.deployment(pm, "m"), 1))
	e.alias("m3", target(e.deployment(pm, "MiniMax-M3"), 0))
	key := e.key(everyAlias)
	e.start()
	if resp, b := e.do("POST", "/v1/messages", `{"model":"m3","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`,
		map[string]string{"x-api-key": key}); resp.StatusCode != 200 || len(mm.recorded()) != 1 {
		t.Errorf("MiniMax-M3 with thinking disabled: %d %s", resp.StatusCode, b)
	}
	resp, b := e.do("POST", "/v1/messages", `{"model":"both","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop_sequences":["END"],`+
		cases[0].set+`}`, map[string]string{"x-api-key": key})
	if want := "every upstream of model both ignores one of stop_sequences, tool_choice.disable_parallel_tool_use"; resp.StatusCode != 400 ||
		!strings.Contains(string(b), want) || len(mm.recorded())+len(ds.recorded()) != 1 {
		t.Errorf("both: %d %s, want %q", resp.StatusCode, b, want)
	}
}

// What the gateway re-encodes goes on the wire as JSON without Go's HTML
// escapes, both ways: a request whose history the gateway filters, and an
// answer it rewrites, keep <, > and & as the caller and the upstream wrote
// them, at their length.
func TestTheGatewayWritesNoHTMLEscapes(t *testing.T) {
	e := newEnv(t)
	f := newFake(t, func(w http.ResponseWriter, _ *http.Request, c fakeCall) {
		writeBody(w, 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"x < y && y > z"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":2}}`, c.Model))
	})
	e.alias("m", target(e.deployment(onProfile(e, "deepseek", f.URL), "up"), 0))
	key := e.key(everyAlias)
	e.start()

	resp, b := e.do("POST", "/v1/messages", `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"is a < b & c > d?"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"unwrapped"},{"type":"text","text":"maybe"}]},
		{"role":"user","content":"why <now>?"}]}`, map[string]string{"x-api-key": key})
	sent := string(f.recorded()[0].Raw)
	if resp.StatusCode != 200 || !strings.Contains(sent, `is a < b & c > d?`) || !strings.Contains(sent, `why <now>?`) || strings.Contains(sent, "unwrapped") {
		t.Errorf("the upstream was sent %s", sent)
	}
	if !strings.Contains(string(b), `x < y && y > z`) {
		t.Errorf("the caller was answered %s", b)
	}
}
