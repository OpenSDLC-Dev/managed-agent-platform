package api_test

import (
	"net/http"
	"testing"
)

func TestAdminAPIKeyAuditAfterConsoleDeletion(t *testing.T) {
	const collection = "/v1/organizations/api_keys"
	s := newTestServer(t)
	token := issueCLITokens(t, s, "org:admin")["access_token"].(string)
	admin := map[string]string{"Authorization": "Bearer " + token}
	created := issueAPIKey(t, s, map[string]any{"name": "audit lifecycle"})
	id, _ := created["id"].(string)
	raw, _ := created["raw_key"].(string)
	if id == "" || raw == "" {
		t.Fatal("Console issuance did not return a key ID and credential")
	}
	authStatus := func() int {
		res := s.doRaw(http.MethodGet, "/v1/agents", nil, map[string]string{"x-api-key": raw})
		res.Body.Close()
		return res.StatusCode
	}
	if status := authStatus(); status != http.StatusOK {
		t.Fatalf("newly issued key authentication: HTTP %d", status)
	}

	for _, path := range []string{collection, collection + "/" + id} {
		res := s.doRaw(http.MethodPost, path, map[string]any{"name": "public write"}, admin)
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("public POST %s: HTTP %d", path, res.StatusCode)
		}
	}
	status, _ := s.doCookie(http.MethodPost, consoleOrgAPIKey(id), map[string]any{"status": "archived"})
	if status != http.StatusOK {
		t.Fatalf("Console deletion: HTTP %d", status)
	}
	if status := authStatus(); status != http.StatusUnauthorized {
		t.Fatalf("archived key authentication: HTTP %d", status)
	}

	status, list := s.decode(s.doRaw(http.MethodGet, collection+"?status=archived", nil, admin))
	rows, _ := list["data"].([]any)
	if status != http.StatusOK || len(rows) != 1 {
		t.Fatalf("archived list: HTTP %d, %d keys", status, len(rows))
	}
	listed, _ := rows[0].(map[string]any)
	status, detail := s.decode(s.doRaw(http.MethodGet, collection+"/"+id, nil, admin))
	if status != http.StatusOK {
		t.Fatalf("retrieve archived key: HTTP %d", status)
	}
	for _, key := range []map[string]any{listed, detail} {
		if key["id"] != id || key["status"] != "archived" || key["name"] != "audit lifecycle" {
			t.Error("admin read did not return the original archived key")
		}
	}
}
