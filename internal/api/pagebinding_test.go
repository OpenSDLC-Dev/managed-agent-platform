package api_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

func TestPageTokensBindToTheirList(t *testing.T) {
	s := newTestServer(t)
	paths := map[string][]string{
		"agents":       {"/v1/agents"},
		"environments": {"/v1/environments"},
		"sessions":     {"/v1/sessions"},
		"memoryStores": {"/v1/memory_stores"},
		"vaults":       {"/v1/vaults"},
		"skills":       {"/v1/skills"},
	}
	add := func(name, path string) { paths[name] = append(paths[name], path) }
	var sessionIDs []string
	for i := range 2 {
		agentID, envID := fixture(t, s)
		if status, body := s.do(http.MethodPost, "/v1/agents/"+agentID, map[string]any{
			"version": 1, "name": fmt.Sprintf("cursor-agent-%d", i),
		}); status != http.StatusOK {
			t.Fatalf("create second agent version: %d %v", status, body)
		}
		sessionID := createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"].(string)
		sessionIDs = append(sessionIDs, sessionID)
		sendEvents(t, s, sessionID, userMessage("one"), userMessage("two"))
		storeID := createMemoryStore(t, s, fmt.Sprintf("cursor-store-%d", i))
		createMemory(t, s, storeID, "/a.md", "a")
		createMemory(t, s, storeID, "/b.md", "b")
		vaultID := createVault(t, s, fmt.Sprintf("cursor-vault-%d", i))
		createCredential(t, s, vaultID, envVarAuth("A"))
		createCredential(t, s, vaultID, envVarAuth("B"))
		skillID := s.createSkill(t)["id"].(string)
		ct, form := skillForm(t, nil, []upFile{{name: "financial-skill/SKILL.md", content: testSkillMD}})
		if status, body := s.doForm(http.MethodPost, "/v1/skills/"+skillID+"/versions", ct, form); status != http.StatusOK {
			t.Fatalf("create second skill version: %d %v", status, body)
		}

		add("agentVersions", "/v1/agents/"+agentID+"/versions")
		add("events", "/v1/sessions/"+sessionID+"/events")
		add("memories", "/v1/memory_stores/"+storeID+"/memories")
		add("memoryVersions", "/v1/memory_stores/"+storeID+"/memory_versions")
		add("credentials", "/v1/vaults/"+vaultID+"/credentials")
		add("skillVersions", "/v1/skills/"+skillID+"/versions")
	}

	type list struct {
		path, token string
		first       map[string]any
		rows        []map[string]any
	}
	lists := make(map[string][]list)
	for name, family := range paths {
		for _, path := range family {
			status, body := s.do(http.MethodGet, path+"?limit=1", nil)
			if status != http.StatusOK || len(listData(t, body)) != 1 {
				t.Fatalf("%s first page: %d %v", path, status, body)
			}
			first := listData(t, body)[0]
			cursor := wantCursor(t, body)
			status, body = s.do(http.MethodGet, path+"?limit=1&page="+url.QueryEscape(cursor), nil)
			if status != http.StatusOK || len(listData(t, body)) != 1 {
				t.Fatalf("%s continuation: %d %v", path, status, body)
			}
			next := listData(t, body)[0]
			if next["id"] == first["id"] && next["version"] == first["version"] {
				t.Fatalf("%s repeated the first row on continuation", path)
			}
			status, body = s.do(http.MethodGet, path, nil)
			if status != http.StatusOK {
				t.Fatalf("%s unpaged list: %d %v", path, status, body)
			}
			lists[name] = append(lists[name], list{path, cursor, first, listData(t, body)})
		}
	}

	// The 30 ordered top-level pairs and both directions of six pairs with
	// nested lists in the reference's 2026-10-31 cursor-parent-binding matrix.
	const (
		invalidCursor = "invalid page cursor"
		invalidToken  = "invalid page token"
		otherEndpoint = "page token is not valid for this endpoint"
		mismatch      = "page token does not match this request's parameters or API version; restart pagination from the first page"
		vaultError    = "page: invalid cursor"
		skillError    = "page is not a valid page token."
		envBase64     = "Invalid page token: invalid base64 encoding"
	)
	for _, tc := range []struct {
		source, target, message string
	}{
		{"agents", "environments", envBase64},
		{"agents", "sessions", otherEndpoint},
		{"agents", "memoryStores", mismatch},
		{"agents", "vaults", vaultError},
		{"agents", "skills", skillError},
		{"environments", "agents", invalidCursor},
		{"environments", "sessions", invalidCursor},
		{"environments", "memoryStores", invalidToken},
		{"environments", "vaults", vaultError},
		{"environments", "skills", skillError},
		{"sessions", "agents", "page token contains empty cursor"},
		{"sessions", "environments", envBase64},
		{"sessions", "memoryStores", mismatch},
		{"sessions", "vaults", vaultError},
		{"sessions", "skills", skillError},
		{"memoryStores", "agents", "unsupported page token version 2"},
		{"memoryStores", "environments", envBase64},
		{"memoryStores", "sessions", invalidCursor},
		{"memoryStores", "vaults", vaultError},
		{"memoryStores", "skills", skillError},
		{"vaults", "agents", invalidCursor},
		{"vaults", "environments", envBase64},
		{"vaults", "sessions", invalidCursor},
		{"vaults", "memoryStores", invalidToken},
		{"vaults", "skills", skillError},
		{"skills", "agents", invalidCursor},
		{"skills", "environments", "Invalid page token format: expected 'env_id|version'"},
		{"skills", "sessions", invalidCursor},
		{"skills", "memoryStores", invalidToken},
		{"skills", "vaults", vaultError},
		{"agents", "credentials", vaultError},
		{"credentials", "agents", invalidCursor},
		{"agents", "agentVersions", otherEndpoint},
		{"agentVersions", "agents", otherEndpoint},
		{"agents", "skillVersions", skillError},
		{"skillVersions", "agents", invalidCursor},
		{"agents", "memories", mismatch},
		{"memories", "agents", "unsupported page token version 10"},
		{"agents", "memoryVersions", invalidToken},
		{"memoryVersions", "agents", invalidCursor},
		{"agents", "events", otherEndpoint},
		{"events", "agents", otherEndpoint},
	} {
		t.Run(tc.source+"_to_"+tc.target, func(t *testing.T) {
			src, dst := lists[tc.source][0], lists[tc.target][0]
			status, body := s.do(http.MethodGet, dst.path+"?limit=1&page="+url.QueryEscape(src.token), nil)
			wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", tc.message)
		})
	}

	// Position-based nested lists can use a token from another parent,
	// provided that the rows still belong to the addressed parent. Demand
	// a nonempty page in at least one direction, as in the reference probes.
	for _, name := range []string{"agentVersions", "skillVersions", "credentials", "memories", "memoryVersions"} {
		t.Run("different_parent_"+name, func(t *testing.T) {
			total := 0
			for i, src := range lists[name] {
				dst := lists[name][1-i]
				status, body := s.do(http.MethodGet, dst.path+"?limit=2&page="+url.QueryEscape(src.token), nil)
				if status != http.StatusOK {
					t.Fatalf("%s token on %s: %d %v", src.path, dst.path, status, body)
				}
				for _, row := range listData(t, body) {
					total++
					if !slices.ContainsFunc(dst.rows, func(own map[string]any) bool { return reflect.DeepEqual(row, own) }) {
						t.Errorf("%s token on %s returned a row from a different parent: %v", src.path, dst.path, row)
					}
				}
			}
			if total == 0 {
				t.Error("both cross-parent continuations were empty")
			}
		})
	}
	t.Run("different_event_thread", func(t *testing.T) {
		for i, src := range lists["events"] {
			dst := lists["events"][1-i]
			status, body := s.do(http.MethodGet, dst.path+"?limit=1&page="+url.QueryEscape(src.token), nil)
			threadID := domain.PrimaryThreadID(domain.ID(sessionIDs[1-i]))
			wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("sessionstore GetEvents for thread %s: invalid_argument: cursor event %s does not belong to this thread",
					threadID, src.first["id"]))
		}
	})
	t.Run("session_and_thread_events_lists", func(t *testing.T) {
		session := lists["events"][0]
		thread := domain.PrimaryThreadID(domain.ID(sessionIDs[0]))
		path := "/v1/sessions/" + sessionIDs[0] + "/threads/" + thread.String() + "/events"
		status, body := s.do(http.MethodGet, path+"?limit=1&page="+url.QueryEscape(session.token), nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", invalidCursor)

		status, body = s.do(http.MethodGet, path+"?limit=1", nil)
		if status != http.StatusOK {
			t.Fatalf("thread first page: %d %v", status, body)
		}
		p := url.QueryEscape(wantCursor(t, body))
		status, body = s.do(http.MethodGet, session.path+"?limit=1&page="+p, nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", otherEndpoint)
	})
	t.Run("previous_format", func(t *testing.T) {
		p := base64.RawURLEncoding.EncodeToString([]byte("k1|n|t|1|agent_example"))
		status, body := s.do(http.MethodGet, "/v1/agents?page="+p, nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", invalidCursor)
	})
	t.Run("unknown_list", func(t *testing.T) {
		p := base64.RawURLEncoding.EncodeToString([]byte("k3|default|unknown|n|t|1|agent_example"))
		status, body := s.do(http.MethodGet, "/v1/agents?page="+p, nil)
		wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", invalidCursor)
	})
}
