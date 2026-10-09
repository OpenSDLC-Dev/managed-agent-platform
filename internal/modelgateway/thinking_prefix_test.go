package modelgateway_test

import (
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// named is a provider of profile prof at url, under its own name, with one
// credential allowed on every endpoint it has.
func named(e *env, name, prof string, endpoints map[profile.Protocol]string) store.Provider {
	e.t.Helper()
	p := e.provider("", func(p *store.Provider) { p.Name, p.Profile, p.Endpoints = name, prof, endpoints })
	e.credential(p, "sk-"+name+"-key1", 1)
	return p
}

// The gateway says, on a Messages answer, when the answer's thinking may go
// back under any prefix (docs/plan/61_thinking-replay-via-gateway.md): on a
// passthrough answer from a deployment whose vendor checks no prefix
// (DeepSeek), and on a converted answer, whose thinking the gateway signed
// itself, whatever its vendor — streamed and whole alike. The attempt that answered decides, not
// one that failed before it. Every other answer says nothing: another
// vendor's, an error, a count, a Chat Completions or Responses caller's. An
// upstream's own header of that name never reaches the caller.
func TestTheGatewaySaysWhenThinkingMayGoBackUnderAnyPrefix(t *testing.T) {
	e := newEnv(t)
	plain := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		w.Header().Set(provider.ThinkingPrefixHeader, "unchecked")
		either("plain")(w, r, c)
	})
	ds := newFake(t, either("Jupiter"))
	down := newFake(t, status(500, `{"type":"error","error":{"type":"api_error","message":"down"}}`))
	refuse := newFake(t, status(400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`))
	anth := func(url string) map[profile.Protocol]string {
		return map[profile.Protocol]string{profile.Anthropic: url}
	}

	deepseek := e.deployment(named(e, "ds", "deepseek",
		map[profile.Protocol]string{profile.Anthropic: ds.URL, profile.OpenAI: ds.URL}), "deepseek-flash")
	generic := e.deployment(named(e, "plain", "anthropic-generic", anth(plain.URL)), "m")
	e.alias("deepseek", target(deepseek, 0))
	e.alias("generic", target(generic, 0))
	e.alias("converted", target(e.deployment(named(e, "oa", "openai-generic",
		map[profile.Protocol]string{profile.OpenAI: ds.URL}), "any-model"), 0))
	e.alias("to-generic", target(e.deployment(named(e, "ds-down", "deepseek", anth(down.URL)), "deepseek-flash"), 0), target(generic, 1))
	e.alias("to-deepseek", target(e.deployment(named(e, "plain-down", "anthropic-generic", anth(down.URL)), "m"), 0), target(deepseek, 1))
	e.alias("refused", target(e.deployment(named(e, "ds-refuse", "deepseek", anth(refuse.URL)), "deepseek-flash"), 0))
	key := e.key(everyAlias)
	e.start(func(c *modelgateway.Config) { c.MaxAttempts = 1 })

	bearer := map[string]string{"Authorization": "Bearer " + key}
	for _, tc := range []struct {
		name, path, body string
		status           int
		unchecked        bool
	}{
		{"deepseek whole", "/v1/messages", `{"model":"deepseek","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 200, true},
		{"deepseek streamed", "/v1/messages", `{"model":"deepseek","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 200, true},
		{"converted whole", "/v1/messages", `{"model":"converted","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 200, true},
		{"converted streamed", "/v1/messages", `{"model":"converted","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 200, true},
		{"another vendor", "/v1/messages", `{"model":"generic","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 200, false},
		{"another vendor streamed", "/v1/messages", `{"model":"generic","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 200, false},
		{"fallback to another vendor", "/v1/messages", `{"model":"to-generic","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 200, false},
		{"fallback to deepseek", "/v1/messages", `{"model":"to-deepseek","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, 200, true},
		{"an error", "/v1/messages", `{"model":"refused","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, 400, false},
		{"a count", "/v1/messages/count_tokens", `{"model":"deepseek","messages":[{"role":"user","content":"hi"}]}`, 200, false},
		{"chat completions", "/v1/chat/completions", `{"model":"deepseek","messages":[{"role":"user","content":"hi"}]}`, 200, false},
		{"responses", "/v1/responses", `{"model":"deepseek","input":"hi"}`, 200, false},
	} {
		resp, b := e.do(http.MethodPost, tc.path, tc.body, bearer)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status %d, want %d: %s", tc.name, resp.StatusCode, tc.status, b)
			continue
		}
		got := resp.Header.Values(provider.ThinkingPrefixHeader)
		switch {
		case tc.unchecked && (len(got) != 1 || got[0] != "unchecked"):
			t.Errorf("%s: %s = %q, want unchecked", tc.name, provider.ThinkingPrefixHeader, got)
		case !tc.unchecked && len(got) != 0:
			t.Errorf("%s: %s = %q, want none", tc.name, provider.ThinkingPrefixHeader, got)
		}
	}
}
