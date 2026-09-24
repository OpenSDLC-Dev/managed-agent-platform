package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// doQuery issues a request whose query string is raw, byte for byte: it is set
// on req.URL.RawQuery after the request is built, so nothing between the test
// and the server re-escapes or repairs it.
func (s *tserver) doQuery(method, path, raw string) (int, map[string]any) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.url+path, nil)
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	req.URL.RawQuery = raw
	req.Header.Set("x-api-key", testKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	var obj map[string]any
	_ = json.NewDecoder(res.Body).Decode(&obj)
	return res.StatusCode, obj
}

// malformedQueries are the three ways url.Values loses a pair it was sent, each
// built around param=value so that the pair lost is the one under test: an
// escape that does not decode, a pair holding a bare ";" (dropped since Go 1.17
// stopped splitting on it), and a query past Go's 10,000-pair ceiling, which
// loses every pair at once.
func malformedQueries(param, value string) map[string]string {
	return map[string]string{
		"invalid escape":        param + "=" + value + "%zz",
		"semicolon":             param + "=" + value + ";x=1",
		"over the pair ceiling": param + "=" + value + strings.Repeat("&x=1", 10000),
	}
}

// wantMalformedQuery asserts the refusal every such query draws: a 400
// invalid_request_error whose message says the query string did not parse.
func wantMalformedQuery(t *testing.T, label string, status int, body map[string]any) {
	t.Helper()
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	inner, _ := body["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.HasPrefix(msg, "malformed query string: ") {
		t.Errorf("%s: message = %q, want the malformed query string refusal", label, msg)
	}
}

// A handler whose query parameters narrow what it lists or select what it
// serves refuses a query string that does not parse, rather than answering as
// if the lost parameter had never been sent: a list that lost its filter
// answers with everything, and an agent read that lost its version answers
// with the latest one, both as a 200. Each path's control request, the same
// parameter well-formed, is not a 400, so the refusal is the query's doing.
func TestMalformedQueryStringIsRefused(t *testing.T) {
	s := newTestServer(t)
	agent := createAgent(t, s, map[string]any{"name": "q", "model": "claude-opus-4-8"})["id"].(string)
	store := createMemoryStore(t, s, "queries")
	session := domain.NewID(domain.PrefixSession).String()
	thread := domain.NewID(domain.PrefixSessionThread).String()
	for _, c := range []struct{ path, param, value string }{
		{"/v1/agents/" + agent, "version", "1"},
		{"/v1/agents", "created_at[gte]", "2026-01-01T00:00:00Z"},
		{"/v1/sessions", "agent_id", agent},
		{"/v1/sessions/" + session + "/events", "types[]", "user.message"},
		// The thread lists share the session list's handler, so they share
		// its refusal although their own parameters only page.
		{"/v1/sessions/" + session + "/threads/" + thread + "/events", "limit", "1"},
		{"/v1/deployments", "agent_id", agent},
		{"/v1/deployment_runs", "deployment_id", domain.NewID(domain.PrefixDeployment).String()},
		{"/v1/dreams", "statuses[]", "completed"},
		{"/v1/skills", "source", "custom"},
		{"/v1/files", "scope_id", session},
		{"/v1/memory_stores", "created_at[gte]", "2026-01-01T00:00:00Z"},
		{"/v1/memory_stores/" + store + "/memories", "path_prefix", "/a/"},
		{"/v1/memory_stores/" + store + "/memory_versions", "memory_id", domain.NewID(domain.PrefixMemory).String()},
	} {
		t.Run(c.path+"?"+c.param, func(t *testing.T) {
			if status, body := s.doQuery(http.MethodGet, c.path, c.param+"="+c.value); status == http.StatusBadRequest {
				t.Fatalf("well-formed: status %d (%v)", status, body)
			}
			for name, raw := range malformedQueries(c.param, c.value) {
				status, body := s.doQuery(http.MethodGet, c.path, raw)
				if status != http.StatusBadRequest {
					t.Errorf("%s: status %d, want 400", name, status)
					continue
				}
				wantMalformedQuery(t, name, status, body)
			}
		})
	}
}

// The loss that matters most: a memory delete whose expected_content_sha256
// went missing read as no precondition at all, and deleted unconditionally. A
// well-formed digest that does not match is a 409, so the digest here would
// refuse the delete whichever way it arrived — unless it is lost.
func TestMalformedQueryStringKeepsTheDeletePrecondition(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "guarded")
	id := createMemory(t, s, store, "/kept.md", "bytes")["id"].(string)
	path := "/v1/memory_stores/" + store + "/memories/" + id

	for name, raw := range malformedQueries("expected_content_sha256", digest("other")) {
		status, body := s.doQuery(http.MethodDelete, path, raw)
		if after, got := s.do(http.MethodGet, path, nil); after != http.StatusOK || got["content"] != "bytes" {
			t.Fatalf("%s: the delete answered %d (%v), and the memory after it: status %d (%v)",
				name, status, body, after, got)
		}
		wantMalformedQuery(t, name, status, body)
	}
}
