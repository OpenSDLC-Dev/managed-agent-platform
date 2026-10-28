package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestNativeLegacyCredentialPrecedence(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	cookie, _ := s.issue(s.seedLocal("legacycookie", "admin", "active"))
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	keys := "/api/console/organizations/default/workspaces/default/api_keys"
	for _, tc := range []struct {
		name, path string
		headers    http.Header
		want       int
	}{
		{"JWT remains on public", "/v1/agents", http.Header{"Authorization": {"Bearer " + token}}, 403},
		{"JWT outranks cookie on private", keys, http.Header{"Cookie": {cookie}, "Authorization": {"Bearer " + token}}, 403},
		{"JWT cannot reach session", "/api/%61uth/session", http.Header{"Authorization": {"Bearer " + token}}, 401},
		{"JWT beside cookie cannot reach session", "/api/auth/session", http.Header{"Cookie": {cookie}, "Authorization": {"Bearer " + token}}, 401},
		{"key preserves old authority", keys, http.Header{"Cookie": {cookie}, "X-Api-Key": {testKey}}, 200},
		{"public key not cookie", "/v1/agents", http.Header{"Cookie": {cookie}, "X-Api-Key": {testKey}}, 200},
		{"bad JWT cannot fall back", keys, http.Header{"Cookie": {cookie}, "Authorization": {"Bearer invalid.jwt.value"}}, 401},
		{"duplicate empty key cannot fall back", keys, http.Header{"Cookie": {cookie}, "X-Api-Key": {"", testKey}}, 401},
		{"empty Authorization cannot fall back", keys, http.Header{"Cookie": {cookie}, "Authorization": {""}}, 401},
		{"worker route refuses cookie", "/v1/environments/env_missing/work", http.Header{"Cookie": {cookie}}, 401},
		{"gate route refuses cookie", "/internal/v1/gate/config", http.Header{"Cookie": {cookie}}, 401},
		{"encoded config not anonymous", "/api/%61uth/config", nil, 401},
		{"cleaned config not anonymous", "/api/auth/../auth/config", nil, 401},
		{"encoded login not anonymous", "/api/%61uth/login", nil, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, raw := s.request("GET", tc.path, "", tc.headers)
			if res.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.want, raw)
			}
		})
	}
	// Stored authority is still used by the public verifier lane in slice 2.
	_, err := s.pool.Exec(context.Background(), `UPDATE principals SET role='developer' WHERE issuer=$1 AND subject='user-1'`, s.idp.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	res, raw := s.request("GET", "/v1/agents", "", http.Header{"Authorization": {"Bearer " + token}})
	if res.StatusCode != 200 {
		t.Fatalf("removed public JWT lane early: %d %s", res.StatusCode, raw)
	}
}

func TestNativeLegacyConfig(t *testing.T) {
	s := newLaneServer(t)
	res := s.doRaw("GET", "/api/auth/config", nil, nil)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var cfg map[string]any
	if res.StatusCode != 200 || json.Unmarshal(raw, &cfg) != nil || cfg["mode"] != "oidc" || cfg["native_login"] != false || len(cfg) != 2 {
		t.Fatalf("legacy config=%d %s", res.StatusCode, raw)
	}
}
