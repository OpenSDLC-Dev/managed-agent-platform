package api_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

func ownedMemoryRequest(t *testing.T, s *tserver, method, path string, body any, headers map[string]string) map[string]any {
	t.Helper()
	status, result := readJSON(t, s.doRaw(method, path, body, headers))
	if status != 200 {
		t.Fatalf("%s %s: status=%d body=%v", method, path, status, result)
	}
	return result
}

func assertMemoryActor(t *testing.T, body map[string]any, field, kind, idField, id string) {
	t.Helper()
	want := map[string]any{"type": kind, idField: id}
	if !reflect.DeepEqual(body[field], want) {
		t.Fatalf("%s=%v want exactly %v", field, body[field], want)
	}
}

// Replacing the credential with its owner/issuer in either audit context or
// memoryActor must fail the persisted readbacks, for humans and service accounts.
func TestOwnedKeyMemoryActors(t *testing.T) {
	for _, kind := range []string{"user", "service_account"} {
		for _, header := range []string{"x-api-key", "Bearer"} {
			t.Run(kind+"/"+header, func(t *testing.T) {
				ns := newNativeServer(t, login.ModePassword)
				s := ns.tserver
				issuer := ownedPrincipalFixture(t, s.pool, "user", "admin")
				owner := ownedPrincipalFixture(t, s.pool, kind, "developer")
				admin := ownedPrincipalFixture(t, s.pool, kind, "admin")
				var raw, keyID, adminRaw, adminKeyID string
				if kind == "user" {
					raw, keyID = issueOwnedHTTP(t, ns, ownedCookie(t, ns, owner), "memory writer", nil)
					adminRaw, adminKeyID = issueOwnedHTTP(t, ns, ownedCookie(t, ns, admin), "memory admin", nil)
				} else {
					headers := ownedCookie(t, ns, issuer)
					ownerAlias, adminAlias := principal.PublicID(owner, kind), principal.PublicID(admin, kind)
					raw, keyID = issueOwnedHTTP(t, ns, headers, "memory service writer", &ownerAlias)
					adminRaw, adminKeyID = issueOwnedHTTP(t, ns, headers, "memory service admin", &adminAlias)
				}
				headers := ownedHeaders(header, raw)
				store := createMemoryStore(t, s, "owned actors")
				base := "/v1/memory_stores/" + store
				created := ownedMemoryRequest(t, s, "POST", base+"/memories", map[string]any{"path": "/owned.md", "content": "one"}, headers)
				versionPath := base + "/memory_versions/" + created["memory_version_id"].(string)
				version := ownedMemoryRequest(t, s, "GET", versionPath, nil, headers)
				assertMemoryActor(t, version, "created_by", "api_actor", "api_key_id", keyID)
				ownedMemoryRequest(t, s, "POST", base+"/memories/"+created["id"].(string), map[string]any{"content": "two"}, headers)
				assertOwnedResponse(t, s.doRaw("POST", versionPath+"/redact", nil, headers), 403)
				version = ownedMemoryRequest(t, s, "GET", versionPath, nil, headers)
				if version["redacted_at"] != nil || version["redacted_by"] != nil || version["content"] != "one" {
					t.Fatalf("developer redaction wrote state: %v", version)
				}
				ownedMemoryRequest(t, s, "POST", versionPath+"/redact", nil, ownedHeaders(header, adminRaw))
				version = ownedMemoryRequest(t, s, "GET", versionPath, nil, headers)
				assertMemoryActor(t, version, "created_by", "api_actor", "api_key_id", keyID)
				assertMemoryActor(t, version, "redacted_by", "api_actor", "api_key_id", adminKeyID)
				var createdBy, redactedBy []byte
				if err := s.pool.QueryRow(t.Context(), `SELECT created_by,redacted_by FROM memory_versions WHERE id=$1`, created["memory_version_id"]).Scan(&createdBy, &redactedBy); err != nil {
					t.Fatal(err)
				}
				persisted := map[string]any{}
				for field, raw := range map[string][]byte{"created_by": createdBy, "redacted_by": redactedBy} {
					var actor map[string]any
					if err := json.Unmarshal(raw, &actor); err != nil {
						t.Fatal(err)
					}
					persisted[field] = actor
				}
				assertMemoryActor(t, persisted, "created_by", "api_actor", "api_key_id", keyID)
				assertMemoryActor(t, persisted, "redacted_by", "api_actor", "api_key_id", adminKeyID)

				// principalFrom must remain the selected key's audit ID too.
				agent := createAgent(t, s, map[string]any{"name": "audit", "model": "m"})
				envID := selfHostedEnv(t, s, "audit")
				session := ownedMemoryRequest(t, s, "POST", "/v1/sessions", map[string]any{"agent": agent["id"], "environment_id": envID}, headers)
				var audit string
				if err := s.pool.QueryRow(t.Context(), `SELECT created_by FROM sessions WHERE id=$1`, session["id"]).Scan(&audit); err != nil {
					t.Fatal(err)
				}
				if audit != keyID {
					t.Fatalf("session audit=%q want selected key %q, not owner or issuer", audit, keyID)
				}
			})
		}
	}
	t.Run("cookie_actor", func(t *testing.T) {
		s := newNativeServer(t, login.ModePassword)
		owner := s.seedLocal("writer", "developer", "active")
		cookie, session := s.issue(owner)
		store := createMemoryStore(t, s.tserver, "cookie actor")
		path := "/api/organizations/default/proxy/v1/memory_stores/" + store
		res, raw := s.request("POST", path+"/memories", `{"path":"/cookie.md","content":"human"}`, nativeHeaders(cookie, session.CSRFToken))
		if res.StatusCode != 200 {
			t.Fatalf("cookie create=%d %s", res.StatusCode, raw)
		}
		var memory map[string]any
		if err := json.Unmarshal(raw, &memory); err != nil {
			t.Fatal(err)
		}
		_, version := s.do("GET", "/v1/memory_stores/"+store+"/memory_versions/"+memory["memory_version_id"].(string), nil)
		assertMemoryActor(t, version, "created_by", "user_actor", "user_id", owner)
	})
	t.Run("work_session_wins", func(t *testing.T) {
		s := newTestServer(t)
		_, envID, sessionID, storeID, envKey := storeWorker(t, s, "owned precedence")
		_, _, token := pollItem(t, s, envID, envKey)
		headers := asBearer(token)
		created := ownedMemoryRequest(t, s, "POST", "/v1/memory_stores/"+storeID+"/memories", map[string]any{"path": "/worker.md", "content": "session"}, headers)
		_, version := s.do("GET", "/v1/memory_stores/"+storeID+"/memory_versions/"+created["memory_version_id"].(string), nil)
		assertMemoryActor(t, version, "created_by", "session_actor", "session_id", sessionID)
	})
}

// Blanket omitempty would erase null controls and a zero-valued full version.
// Decode to maps: reading a missing field as nil alone cannot prove presence.
func TestMemoryVersionNullableControls(t *testing.T) {
	s := newTestServer(t)
	store := createMemoryStore(t, s, "nullable versions")
	base := "/v1/memory_stores/" + store
	headers := map[string]string{"x-api-key": testKey}
	created := createMemory(t, s, store, "/empty.md", "")
	versionPath := base + "/memory_versions/" + created["memory_version_id"].(string)
	check := func(t *testing.T, body map[string]any, want map[string]any) {
		t.Helper()
		for field, value := range want {
			got, present := body[field]
			if !present || got != value {
				t.Errorf("%s present=%v value=%v want present with %v", field, present, got, value)
			}
		}
	}
	const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	t.Run("full_empty", func(t *testing.T) {
		check(t, ownedMemoryRequest(t, s, "GET", versionPath+"?view=full", nil, headers), map[string]any{
			"path": "/empty.md", "content": "", "content_size_bytes": float64(0), "content_sha256": emptyDigest})
	})
	t.Run("basic", func(t *testing.T) {
		check(t, ownedMemoryRequest(t, s, "GET", versionPath+"?view=basic", nil, headers), map[string]any{
			"path": "/empty.md", "content": nil, "content_size_bytes": float64(0), "content_sha256": emptyDigest})
	})
	ownedMemoryRequest(t, s, "POST", base+"/memories/"+created["id"].(string), map[string]any{"content": "two"}, headers)
	t.Run("redacted", func(t *testing.T) {
		ownedMemoryRequest(t, s, "POST", versionPath+"/redact", nil, headers)
		check(t, ownedMemoryRequest(t, s, "GET", versionPath, nil, headers), map[string]any{
			"path": nil, "content": nil, "content_size_bytes": nil, "content_sha256": nil})
	})
	t.Run("deleted", func(t *testing.T) {
		assertOwnedResponse(t, s.doRaw(http.MethodDelete, base+"/memories/"+created["id"].(string), nil, headers), 200)
		var vid string
		if err := s.pool.QueryRow(t.Context(), `SELECT id FROM memory_versions WHERE memory_id=$1 AND operation='deleted'`, created["id"]).Scan(&vid); err != nil {
			t.Fatal(err)
		}
		check(t, ownedMemoryRequest(t, s, "GET", base+"/memory_versions/"+vid, nil, headers), map[string]any{
			"path": "/empty.md", "content": nil, "content_size_bytes": nil, "content_sha256": nil})
	})
}
