package modelgateway_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// onProfile is a provider of the named profile at url, with one credential.
func onProfile(e *env, prof, url string) store.Provider {
	e.t.Helper()
	p := e.provider(url, func(p *store.Provider) { p.Name = prof; p.Profile = prof })
	e.credential(p, "sk-"+prof+"-key1", 1)
	return p
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
// (probed 2026-10-07) — is sent each one in a tool_result as text, rendered
// as the brain's flatten_search_results renders it. Every other block and
// field goes as sent — a block whose type key differs only in case, and a
// search_result the rendering cannot read, included — and a vendor that
// takes the block gets it untouched; a count_tokens request is edited as
// its /v1/messages twin.
func TestASearchResultReachesARefusingVendorAsText(t *testing.T) {
	const messages = `[
		{"role":"user","content":[{"type":"search_result","source":"https://example.com/top","title":"Top","content":[{"type":"text","text":"kept"}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[
			{"type":"search_result","source":"https://example.com/paris","title":"Paris","content":[{"type":"text","text":"Paris is the capital."}],"citations":{"enabled":true}},
			{"type":"text","text":"note"},
			{"type":"text","text":"x","Type":"search_result"},
			{"type":"search_result","source":"https://example.com/i","title":"I","content":[{"type":"image","source":{"type":"url","url":"https://example.com/i.png"}}]},
			{"type":"search_result","source":"https://example.com/n","title":5,"content":[{"type":"text","text":"n"}]}]},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"plain"}]}]`
	const flattened = `[
		{"role":"user","content":[{"type":"search_result","source":"https://example.com/top","title":"Top","content":[{"type":"text","text":"kept"}]}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[
			{"type":"text","text":"Paris (https://example.com/paris)\nParis is the capital.\n"},
			{"type":"text","text":"note"},
			{"type":"text","text":"x","Type":"search_result"},
			{"type":"search_result","source":"https://example.com/i","title":"I","content":[{"type":"image","source":{"type":"url","url":"https://example.com/i.png"}}]},
			{"type":"search_result","source":"https://example.com/n","title":5,"content":[{"type":"text","text":"n"}]}]},
			{"type":"tool_result","tool_use_id":"toolu_2","content":"plain"}]}]`
	e := newEnv(t)
	fakes := map[string]*fake{}
	for _, prof := range []string{"deepseek", "minimax", "anthropic-generic"} {
		fakes[prof] = newFake(t, message("ok"))
		e.alias(prof, target(e.deployment(onProfile(e, prof, fakes[prof].URL), "up"), 0))
	}
	key := e.key(everyAlias)
	e.start()

	for prof, want := range map[string]string{"deepseek": flattened, "minimax": flattened, "anthropic-generic": messages} {
		for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":%s}`, prof, messages)
			if resp, b := e.do("POST", path, body, map[string]string{"x-api-key": key}); resp.StatusCode != 200 {
				t.Fatalf("%s %s: %d %s", prof, path, resp.StatusCode, b)
			}
			calls := fakes[prof].recorded()
			got := calls[len(calls)-1]
			if got.Path != path || !reflect.DeepEqual(decoded(t, got.Body["messages"]), decoded(t, []byte(want))) {
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
		e.do("POST", "/v1/messages", fmt.Sprintf(`{"model":"deepseek","max_tokens":64,"messages":%s}`, msgs), map[string]string{"x-api-key": key})
		calls := fakes["deepseek"].recorded()
		if got := calls[len(calls)-1].Body["messages"]; !reflect.DeepEqual(decoded(t, got), decoded(t, []byte(want))) {
			t.Errorf("%s: deepseek was sent %s", name, got)
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

// A vendor that documents ignoring something bounding the answer — DeepSeek
// tool_choice's disable_parallel_tool_use, MiniMax stop_sequences — is never
// sent a request that sets it: another of the model's deployments serves
// it, and where there is none the gateway refuses it, count_tokens alike,
// naming the field. The field read by its exact key, or left unset, changes
// nothing.
func TestARequestAVendorWouldIgnoreGoesElsewhere(t *testing.T) {
	const tools = `"tools":[{"name":"t","input_schema":{"type":"object"}}]`
	cases := []struct {
		prof, field, set string
		unset            []string
	}{
		{"deepseek", "tool_choice.disable_parallel_tool_use", tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`,
			[]string{tools + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":false}`, tools + `,"tool_choice":{"type":"auto","Disable_Parallel_Tool_Use":true}`}},
		{"minimax", "stop_sequences", `"stop_sequences":["END"]`, []string{`"stop_sequences":[]`, `"Stop_Sequences":["END"]`}},
	}
	for _, tc := range cases {
		t.Run(tc.prof, func(t *testing.T) {
			e := newEnv(t)
			vendor, other := newFake(t, message("vendor")), newFake(t, message("other"))
			d := e.deployment(onProfile(e, tc.prof, vendor.URL), "up")
			e.alias("mixed", target(d, 0), target(e.deployment(onProfile(e, "anthropic-generic", other.URL), "other-model"), 1))
			e.alias("alone", target(d, 0))
			key := e.key(everyAlias)
			e.start()

			try := func(path, model, extra string) (int, string, int, int) {
				nv, no := len(vendor.recorded()), len(other.recorded())
				body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}],%s}`, model, extra)
				resp, b := e.do("POST", path, body, map[string]string{"x-api-key": key})
				return resp.StatusCode, string(b), len(vendor.recorded()) - nv, len(other.recorded()) - no
			}
			if s, b, nv, no := try("/v1/messages", "mixed", tc.set); s != 200 || nv != 0 || no != 1 {
				t.Errorf("mixed: %d %s, %d calls to %s and %d to the other", s, b, nv, tc.prof, no)
			}
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				s, b, nv, _ := try(path, "alone", tc.set)
				if s != 400 || nv != 0 || !strings.Contains(b, `"invalid_request_error"`) || !strings.Contains(b, tc.field) {
					t.Errorf("%s alone: %d %s, %d calls", path, s, b, nv)
				}
			}
			for _, extra := range tc.unset {
				if s, b, nv, _ := try("/v1/messages", "alone", extra); s != 200 || nv != 1 {
					t.Errorf("%s: %d %s, %d calls to %s", extra, s, b, nv, tc.prof)
				}
			}
		})
	}
}
