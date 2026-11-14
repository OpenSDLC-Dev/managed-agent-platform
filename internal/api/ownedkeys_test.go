package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func issueOwnedHTTP(t *testing.T, s *nativeServer, headers http.Header, name string, principalID *string) (raw, keyID string) {
	t.Helper()
	body := map[string]any{"name": name}
	if principalID != nil {
		body["principal_id"] = *principalID
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	reply := expectManagement(t, s, "POST", consoleAPIKeysPath, string(encoded), headers, 200)
	var key struct {
		Raw string `json:"raw_key"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(reply, &key); err != nil || key.Raw == "" || key.ID == "" {
		t.Fatalf("missing issued credential: %v", err)
	}
	return key.Raw, key.ID
}

func ownedCookie(t *testing.T, s *nativeServer, id string) http.Header {
	t.Helper()
	cookie, session := s.issue(id)
	return nativeHeaders(cookie, session.CSRFToken)
}

func ownedObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func ownedSnapshot(t *testing.T, s *nativeServer) string {
	t.Helper()
	var snapshot string
	if err := s.pool.QueryRow(t.Context(), `SELECT coalesce(jsonb_agg(to_jsonb(k) ORDER BY id),'[]'::jsonb)::text FROM api_keys k`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// A null owner, an owner inferred from the creator, or a list-only field on a
// mutation response breaks this contract. The SQL assertion checks both lanes.
func TestOwnedKeyIssuanceDTO(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	for _, role := range []string{"developer", "admin"} {
		caller := seedMember(t, s, role, role, "console")
		headers := ownedCookie(t, s, caller)
		self := principal.PublicID(caller, "user")
		for _, assignment := range []string{"omitted", "null", "self", "service"} {
			if assignment == "service" && role != "admin" {
				continue
			}
			t.Run(role+"/"+assignment, func(t *testing.T) {
				owner, kind := caller, "user"
				body := map[string]any{"name": "owned DTO"}
				switch assignment {
				case "null":
					body["principal_id"] = nil
				case "self":
					body["principal_id"] = self
				case "service":
					owner, kind = ownedPrincipalFixture(t, s.pool, "service_account", "developer"), "service_account"
					body["principal_id"] = principal.PublicID(owner, kind)
				}
				encoded, _ := json.Marshal(body)
				reply := expectManagement(t, s, "POST", consoleAPIKeysPath, string(encoded), headers, 200)
				obj := ownedObject(t, reply)
				expectedOwner := map[string]any{"type": "user_actor", "user_id": self}
				if kind == "service_account" {
					expectedOwner = map[string]any{"type": "service_account_actor", "service_account_id": principal.PublicID(owner, kind)}
				}
				creator := map[string]any{"type": "user", "id": self}
				assertDTO := func(obj map[string]any, secret bool) {
					t.Helper()
					if !reflect.DeepEqual(obj["principal"], expectedOwner) || !reflect.DeepEqual(obj["created_by"], creator) {
						t.Errorf("owner/creator=%v/%v want %v/%v", obj["principal"], obj["created_by"], expectedOwner, creator)
					}
					if v, ok := obj["workspace_id"]; !ok || v != nil {
						t.Error("workspace must remain null")
					}
					for _, field := range []string{"scope", "can_manage"} {
						if _, ok := obj[field]; ok {
							t.Errorf("unexpected mutation field %s", field)
						}
					}
					_, present := obj["raw_key"]
					if present != secret {
						t.Errorf("raw key presence=%v want %v", present, secret)
					}
				}
				assertDTO(obj, true)
				id, raw := obj["id"].(string), obj["raw_key"].(string)
				var stored bool
				if err := s.pool.QueryRow(t.Context(), `SELECT owner_principal_id=$2 AND created_by=$3 AND key_hash IS NULL AND owned_key_hash=$4 AND position($5 in to_jsonb(k)::text)=0 FROM api_keys k WHERE id=$1`, id, owner, caller, apikey.Hash(raw), raw).Scan(&stored); err != nil || !stored {
					t.Fatalf("owned digest/identity/plaintext invariant=%v err=%v", stored, err)
				}
				updated := ownedObject(t, expectManagement(t, s, "POST", consoleOrgAPIKey(id), `{"name":"renamed"}`, headers, 200))
				assertDTO(updated, false)
				listed := expectManagement(t, s, "GET", consoleAPIKeysPath, "", headers, 200)
				var rows []map[string]any
				if err := json.Unmarshal(listed, &rows); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row["id"] == id {
						found = true
						if row["can_manage"] != true {
							t.Error("owned non-preset row is not manageable")
						}
						delete(row, "can_manage")
						assertDTO(row, false)
					}
				}
				if !found {
					t.Fatal("issued key absent from list")
				}
			})
		}
	}
}

// Wrong-kind aliases, foreign/deactivated owners and credential fallback must
// never create a row. Malformed shapes keep their established 400/404 split.
func TestOwnedKeyAssignment(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "assign-admin", "admin", "console")
	developer := seedMember(t, s, "assign-dev", "developer", "console")
	other := seedMember(t, s, "assign-other", "developer", "console")
	sa := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
	foreign := ownedPrincipalFixture(t, s.pool, "service_account", "admin")
	removed := ownedPrincipalFixture(t, s.pool, "service_account", "admin")
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET org_id='other' WHERE id=$1;`, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, removed); err != nil {
		t.Fatal(err)
	}
	for _, caller := range []string{admin, developer} {
		headers := ownedCookie(t, s, caller)
		for _, tc := range []struct {
			name  string
			value any
			want  int
		}{
			{"number", 3, 400}, {"prefix", "principal_bad", 400}, {"unstorable", "user_bad\x00", 400},
			{"undecodable", "svac_missing", 404}, {"nonexistent", principal.PublicID(domain.NewID(domain.PrefixPrincipal).String(), "user"), 404},
			{"wrong-kind", principal.PublicID(sa, "user"), 404},
			{"foreign", principal.PublicID(foreign, "service_account"), 404}, {"deactivated", principal.PublicID(removed, "service_account"), 404},
			{"other-human", principal.PublicID(other, "user"), 403},
		} {
			t.Run(caller+"/"+tc.name, func(t *testing.T) {
				before := ownedSnapshot(t, s)
				body, _ := json.Marshal(map[string]any{"name": "refused", "principal_id": tc.value})
				expectManagement(t, s, "POST", consoleAPIKeysPath, string(body), headers, tc.want)
				if after := ownedSnapshot(t, s); before != after {
					t.Fatal("refusal changed key rows")
				}
			})
		}
	}
	t.Run("developer-service", func(t *testing.T) {
		before := ownedSnapshot(t, s)
		expectManagement(t, s, "POST", consoleAPIKeysPath, fmt.Sprintf(`{"name":"refused","principal_id":%q}`, principal.PublicID(sa, "service_account")), ownedCookie(t, s, developer), 403)
		if before != ownedSnapshot(t, s) {
			t.Fatal("developer assigned service owner")
		}
	})
	t.Run("selected-machine", func(t *testing.T) {
		h := ownedCookie(t, s, admin)
		h.Set("X-Api-Key", testKey)
		before := ownedSnapshot(t, s)
		expectManagement(t, s, "POST", consoleAPIKeysPath, fmt.Sprintf(`{"name":"no fallback","principal_id":%q}`, principal.PublicID(admin, "user")), h, 401)
		if before != ownedSnapshot(t, s) {
			t.Fatal("cookie fallback minted key")
		}
		for _, body := range []string{`{"name":"legacy"}`, `{"name":"legacy-null","principal_id":null}`} {
			obj := ownedObject(t, expectManagement(t, s, "POST", consoleAPIKeysPath, body, h, 200))
			if obj["principal"] != nil {
				t.Fatal("selected machine gained an owner")
			}
		}
	})
	for _, mode := range []login.Mode{login.ModeOIDC, login.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			humanServer := newNativeServer(t, mode)
			h := nativeHeaders("", "")
			token := humanServer.idp.Mint(t, humanServer.idp.Claims(laneAudience, time.Now()))
			if mode == login.ModeOIDC {
				h.Set("Authorization", "Bearer "+token)
			} else {
				h.Set("X-Goog-IAP-JWT-Assertion", token)
			}
			expectManagement(t, humanServer, "GET", consoleAPIKeysPath, "", h, 403)
			var caller string
			if err := humanServer.pool.QueryRow(t.Context(), `UPDATE principals SET role='developer' WHERE subject='user-1' RETURNING id`).Scan(&caller); err != nil {
				t.Fatal(err)
			}
			cookie, session := humanServer.issue(caller)
			h.Set("Cookie", cookie)
			h.Set("X-CSRF-Token", session.CSRFToken)
			before := ownedSnapshot(t, humanServer)
			expectManagement(t, humanServer, "POST", consoleAPIKeysPath, fmt.Sprintf(`{"name":"explicit","principal_id":%q}`, session.ID), h, 401)
			if before != ownedSnapshot(t, humanServer) {
				t.Fatal("legacy human named owner with companion cookie")
			}
			for _, body := range []string{`{"name":"implicit"}`, `{"name":"implicit-null","principal_id":null}`} {
				obj := ownedObject(t, expectManagement(t, humanServer, "POST", consoleAPIKeysPath, body, h, 200))
				if !reflect.DeepEqual(obj["principal"], map[string]any{"type": "user_actor", "user_id": session.ID}) {
					t.Errorf("legacy human owner=%v", obj["principal"])
				}
			}
		})
	}
}

// A genuine pre-cutover human-issued unowned row. Never disable the INSERT
// guard or turn creator into owner to manufacture this historical fixture.
func preIssuerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// The migrate-through seam is store-test-only. Apply the actual old SQL to a
	// fresh disposable database here, then let store.Migrate perform the cutover.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `CREATE TABLE schema_migrations(version text PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	names, err := filepath.Glob("../store/migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range names {
		name := filepath.Base(path)
		if name > "0060_owned_api_keys.sql" {
			break
		}
		sql, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), string(sql)); err != nil {
			t.Fatalf("historical schema %s: %v", name, err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO schema_migrations(version) VALUES ($1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func historicalKeyServer(t *testing.T) (*nativeServer, string, string) {
	t.Helper()
	pool := preIssuerPool(t)
	s := &nativeServer{tserver: &tserver{t: t, pool: pool}, manager: login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})}
	developer := seedMember(t, s, "historical-issuer", "developer", "console")
	_, old, err := api.IssueManagementKey(t.Context(), pool, "legacy-human", nil, developer)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := api.EnsureAPIKey(t.Context(), pool, "test", testKey); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil, api.WithLogin(s.manager, nil)))
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s, developer, old.ID
}

func TestOwnedKeyRowPermissions(t *testing.T) {
	s, developer, legacy := historicalKeyServer(t)
	admin := seedMember(t, s, "row-admin", "admin", "console")
	other := seedMember(t, s, "row-other", "developer", "console")
	devHeaders, adminHeaders := ownedCookie(t, s, developer), ownedCookie(t, s, admin)
	// Direct storage deliberately separates owner from creator; permission must
	// follow the owner even for historical/adversarial rows.
	_, owned := ownedKeyFixture(t, s.pool, developer, admin)
	_, cross := ownedKeyFixture(t, s.pool, other, developer)
	expected := map[string]bool{owned: true}
	for _, status := range []string{"inactive", "archived", "expired"} {
		_, id := ownedKeyFixture(t, s.pool, developer, admin)
		expected[id] = true
		if status == "expired" {
			_, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET expires_at='2000-01-01' WHERE id=$1`, id)
			if err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2 WHERE id=$1`, id, status); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("true-owner-list", func(t *testing.T) {
		var rows []map[string]any
		if err := json.Unmarshal(expectManagement(t, s, "GET", consoleAPIKeysPath, "", devHeaders, 200), &rows); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, row := range rows {
			id := row["id"].(string)
			got[id] = true
			if row["can_manage"] != true {
				t.Errorf("own row %s not manageable", id)
			}
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("developer visibility=%v want true owners %v", got, expected)
		}
	})
	t.Run("admin-list-hints", func(t *testing.T) {
		var rows []map[string]any
		if err := json.Unmarshal(expectManagement(t, s, "GET", consoleAPIKeysPath, "", adminHeaders, 200), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(expected)+3 {
			t.Fatalf("admin omitted rows: %d", len(rows))
		}
		for _, row := range rows {
			want := row["created_by"] != nil
			if row["can_manage"] != want {
				t.Errorf("row %v hint=%v want %v", row["id"], row["can_manage"], want)
			}
		}
	})
	for _, path := range []func(string) string{consoleOrgAPIKey, consoleAPIKey} {
		for _, id := range []string{cross, legacy, "apikey_missing"} {
			for _, body := range []string{`{}`, `{"unknown":1}`, `{"name":"stolen"}`, `{"status":"inactive"}`} {
				t.Run(path(id)+body, func(t *testing.T) {
					before := ownedSnapshot(t, s)
					raw := expectManagement(t, s, "POST", path(id), body, devHeaders, 404)
					obj := ownedObject(t, raw)
					if errMessage(obj) != fmt.Sprintf("API Key `%s` not found.", id) {
						t.Fatalf("visibility refusal=%s", raw)
					}
					if before != ownedSnapshot(t, s) {
						t.Fatal("invisible row mutated")
					}
				})
			}
		}
		expectManagement(t, s, "POST", path(owned), `{"unknown":1}`, devHeaders, 400)
		expectManagement(t, s, "POST", path(owned), `{"name":"mine"}`, devHeaders, 200)
	}
}

func TestOwnedKeyLifecycle(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "lifecycle-admin", "admin", "console")
	h := ownedCookie(t, s, admin)
	_, id := issueOwnedHTTP(t, s, h, "lifecycle", nil)
	for _, status := range []string{"inactive", "active", "archived"} {
		expectManagement(t, s, "POST", consoleOrgAPIKey(id), fmt.Sprintf(`{"status":%q}`, status), h, 200)
	}
	for _, patch := range []string{`{}`, `{"name":"rename"}`, `{"status":"active"}`, `{"status":"inactive"}`, `{"status":"archived"}`} {
		expectManagement(t, s, "POST", consoleOrgAPIKey(id), patch, h, 400)
	}
	expired := ownedObject(t, expectManagement(t, s, "POST", consoleAPIKeysPath, `{"name":"born expired","expires_at":"2000-01-01T00:00:00Z"}`, h, 200))
	if expired["status"] != "expired" {
		t.Fatal("past-expiry create not already expired")
	}
	id = expired["id"].(string)
	for _, patch := range []string{`{}`, `{"name":"rename"}`, `{"status":"active"}`, `{"status":"inactive"}`, `{"status":"archived","name":"rename"}`} {
		expectManagement(t, s, "POST", consoleOrgAPIKey(id), patch, h, 400)
	}
	expectManagement(t, s, "POST", consoleOrgAPIKey(id), `{"status":"archived"}`, h, 200)
	_, personal := issueOwnedHTTP(t, s, h, "personal", nil)
	sa := ownedPrincipalFixture(t, s.pool, "service_account", "developer")
	alias := principal.PublicID(sa, "service_account")
	_, service := issueOwnedHTTP(t, s, h, "service", &alias)
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='active' WHERE id=$1`, admin); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{personal: "archived", service: "active"} {
		var status string
		if err := s.pool.QueryRow(t.Context(), `SELECT status FROM api_keys WHERE id=$1`, id).Scan(&status); err != nil || status != want {
			t.Errorf("owner removal/restoration status=%s want=%s err=%v", status, want, err)
		}
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, sa); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.pool.QueryRow(t.Context(), `SELECT status FROM api_keys WHERE id=$1`, service).Scan(&status); err != nil || status != "archived" {
		t.Errorf("SA archive status=%s err=%v", status, err)
	}
}
