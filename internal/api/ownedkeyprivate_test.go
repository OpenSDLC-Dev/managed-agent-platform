package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// An ignored legacy Bearer must not displace the private assertion or become
// machine authority. The assertion supplies the current role and owned-row view.
func TestPrivateLegacyBearerKeepsAssertionAuthority(t *testing.T) {
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
	})
	headers := make(http.Header)
	headers.Set("X-Goog-IAP-JWT-Assertion", s.token("platform-admins"))
	assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers), 200)
	var human string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM principals WHERE issuer=$1 AND subject='user-1'`, s.idp.Issuer()).Scan(&human); err != nil {
		t.Fatal(err)
	}
	legacy, _, err := api.IssueManagementKey(t.Context(), s.pool, "legacy companion", nil, "apikey_companion_issuer")
	if err != nil {
		t.Fatal(err)
	}
	headers.Set("Authorization", "Bearer "+legacy)
	t.Run("admin_assertion_with_unowned_bearer", func(t *testing.T) {
		assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers), 200)
	})
	t.Run("user_assertion_cannot_borrow_machine_authority", func(t *testing.T) {
		h := headers.Clone()
		h.Set("X-Goog-IAP-JWT-Assertion", s.token("platform-read"))
		status, typ, body := laneRead(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h))
		if status != 403 || typ != "permission_error" || laneMessage(t, body) != "this route requires the developer role" {
			t.Fatalf("status=%d error=%q body=%s; want human developer-role 403", status, typ, body)
		}
	})
	for _, state := range []string{"active", "inactive", "expired", "archived"} {
		t.Run(state, func(t *testing.T) {
			raw, row, err := api.IssueManagementKey(t.Context(), s.pool, "legacy "+state, nil, "apikey_companion_issuer")
			if err != nil {
				t.Fatal(err)
			}
			status := state
			if state == "expired" {
				status = "active"
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2,
				expires_at=CASE WHEN $3 THEN clock_timestamp()-interval '1 second' END WHERE id=$1`, row.ID, status, state == "expired"); err != nil {
				t.Fatal(err)
			}
			h := headers.Clone()
			h.Set("Authorization", "Bearer "+raw)
			for _, tc := range []struct {
				name, assertion string
				want            int
			}{
				{"admin", s.token("platform-admins"), 200},
				{"developer", s.token("platform-devs"), 200},
				{"user", s.token("platform-read"), 403},
				{"bad_assertion", "not-a-valid-assertion", 401},
				{"no_assertion", "", 401},
			} {
				t.Run(tc.name, func(t *testing.T) {
					h.Set("X-Goog-IAP-JWT-Assertion", tc.assertion)
					res := ownedFirstHop(t, s.url, consoleAPIKeysPath, h)
					if tc.name == "developer" {
						status, _, body := laneRead(t, res)
						if status != 200 {
							t.Fatalf("developer list status=%d", status)
						}
						var rows []map[string]any
						if json.Unmarshal([]byte(body), &rows) != nil {
							t.Fatal("invalid key list")
						}
						for _, key := range rows {
							owner, ok := key["principal"].(map[string]any)
							if !ok || owner["user_id"] != principal.PublicID(human, "user") || key["can_manage"] != true {
								t.Fatal("companion key bypassed true-owner list filter")
							}
						}
						// Workspace remains an admin-only control; bypassing the human role
						// would return its 404 instead of 403.
						assertOwnedResponse(t, ownedFirstHop(t, s.url, "/api/console/organizations/default/workspaces/default", h), 403)
					} else {
						assertOwnedResponse(t, res, tc.want)
					}
				})
			}
			// Classification ignores lifecycle only for the companion Bearer. The
			// same value selected as x-api-key must still pass key authentication.
			t.Run("selected_x_key", func(t *testing.T) {
				h.Set("X-Goog-IAP-JWT-Assertion", s.token("platform-admins"))
				h.Set("x-api-key", raw)
				want := 401
				if state == "active" {
					want = 200
				}
				assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), want)
			})
			t.Run("human_audit", func(t *testing.T) {
				res := s.doRaw("POST", consoleAPIKeysPath, map[string]any{"name": "human audit " + state}, map[string]string{
					"Authorization": "Bearer " + raw, "X-Goog-IAP-JWT-Assertion": s.token("platform-admins"),
				})
				status, _, body := laneRead(t, res)
				if status != 200 {
					t.Fatalf("human issuance: status=%d body=%s", status, body)
				}
				var created struct {
					ID        string                    `json:"id"`
					CreatedBy struct{ ID, Type string } `json:"created_by"`
				}
				if err := json.Unmarshal([]byte(body), &created); err != nil {
					t.Fatal(err)
				}
				var creator string
				if err := s.pool.QueryRow(t.Context(), `SELECT created_by FROM api_keys WHERE id=$1`, created.ID).Scan(&creator); err != nil {
					t.Fatal(err)
				}
				if creator != human || created.CreatedBy.ID != principal.PublicID(human, "user") || created.CreatedBy.Type != "user" {
					t.Fatalf("creator=%q response_actor=%+v want human %q, never companion key %q", creator, created.CreatedBy, human, row.ID)
				}
			})
		})
	}
	t.Run("deactivated_human", func(t *testing.T) {
		if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, human); err != nil {
			t.Fatal(err)
		}
		assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers), 403)
	})
}

// Use the same verified human for the native cookie and private assertion, with
// no IdP mapping to undo the stored-role changes exercised below.
func privateAssertionServer(t *testing.T) (*nativeServer, http.Header, string) {
	t.Helper()
	s := newNativeServer(t, login.ModeTrustedProxy)
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now())))
	assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), 403)
	var human string
	if err := s.pool.QueryRow(t.Context(), `UPDATE principals SET role='admin' WHERE issuer=$1 AND subject='user-1' RETURNING id`, s.idp.Issuer()).Scan(&human); err != nil {
		t.Fatal(err)
	}
	cookie, session := s.issue(human)
	h.Set("Cookie", cookie)
	h.Set("X-CSRF-Token", session.CSRFToken)
	assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), 200)
	return s, h, human
}

// An active-only lookup would let an expired or archived owned Bearer fall
// through to a valid assertion. Both credential kinds stay private-negative.
func TestOwnedKeyPrivateAssertionIsolation(t *testing.T) {
	s, headers, human := privateAssertionServer(t)
	for _, kind := range []string{"user", "service_account"} {
		owner := ownedPrincipalFixture(t, s.pool, kind, "admin")
		for _, state := range []string{"active", "inactive", "expired", "archived"} {
			raw, id := ownedKeyFixture(t, s.pool, owner, human)
			status := state
			if state == "expired" {
				status = "active"
			}
			if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2,
				expires_at=CASE WHEN $3 THEN clock_timestamp()-interval '1 second' END WHERE id=$1`, id, status, state == "expired"); err != nil {
				t.Fatal(err)
			}
			before := ownedDurableSnapshot(t, s.pool)
			for _, path := range []string{consoleAPIKeysPath, consoleAPIKey(id), consoleOrgAPIKey(id),
				"/api/console/organizations/default/members", "/api/console/organizations/default/service_accounts",
				"/api/organizations/default/proxy/v1/agents", "/api/unknown"} {
				for _, prefix := range []string{"/api/", "/%61pi/"} {
					path := strings.Replace(path, "/api/", prefix, 1)
					for _, method := range []string{"GET", "POST", "PUT"} {
						for _, header := range []string{"x-api-key", "Bearer"} {
							t.Run(kind+"/"+state+"/"+method+path+"/"+header, func(t *testing.T) {
								h := headers.Clone()
								for k, v := range ownedHeaders(header, raw) {
									h.Set(k, v)
								}
								res, body := s.request(method, path, `{"name":"must not persist","status":"inactive"}`, h)
								if res.StatusCode != 401 || laneMessage(t, string(body)) == "" {
									t.Fatalf("status=%d body=%s want credential refusal 401", res.StatusCode, body)
								}
							})
						}
					}
				}
			}
			if ownedDurableSnapshot(t, s.pool) != before {
				t.Errorf("%s/%s owned private refusal changed durable keys or principals", kind, state)
			}
		}
	}
}

func TestPrivateLegacyBearerSelection(t *testing.T) {
	s, headers, human := privateAssertionServer(t)
	legacy, _, err := api.IssueManagementKey(t.Context(), s.pool, "legacy companion", nil, "apikey_companion_issuer")
	if err != nil {
		t.Fatal(err)
	}
	owned, _ := ownedKeyFixture(t, s.pool, human, human)
	for _, tc := range []struct {
		name      string
		xkeys     []string
		auth      []string
		assertion bool
		want      int
	}{
		{"legacy_companion", nil, []string{"Bearer " + legacy}, true, 200},
		{"single_empty_x", []string{""}, []string{"Bearer " + legacy}, true, 200},
		{"invalid_x", []string{"wrong"}, []string{"Bearer " + legacy}, true, 401},
		{"repeated_x", []string{"", legacy}, []string{"Bearer " + legacy}, true, 401},
		{"repeated_empty_x", []string{"", ""}, []string{"Bearer " + legacy}, true, 401},
		{"unknown_issued", nil, []string{"Bearer " + api.IssuedKeyPrefix + "unknown"}, true, 401},
		{"owned_cannot_borrow_same_issuer", nil, []string{"Bearer " + owned}, true, 401},
		{"repeated_legacy_owned", nil, []string{"Bearer " + legacy, "Bearer " + owned}, true, 401},
		{"repeated_owned_legacy", nil, []string{"Bearer " + owned, "Bearer " + legacy}, true, 401},
		{"repeated_legacy", nil, []string{"Bearer " + legacy, "Bearer " + legacy}, true, 401},
		{"cookie_cannot_replace_assertion", nil, []string{"Bearer " + legacy}, false, 401},
		{"selected_legacy_x_wins", []string{legacy}, []string{"Bearer " + owned}, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := headers.Clone()
			if !tc.assertion {
				h.Del("X-Goog-IAP-JWT-Assertion")
			}
			for _, value := range tc.xkeys {
				h.Add("x-api-key", value)
			}
			for _, value := range tc.auth {
				h.Add("Authorization", value)
			}
			before := ownedDurableSnapshot(t, s.pool)
			assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, h), tc.want)
			if tc.want == 401 && ownedDurableSnapshot(t, s.pool) != before {
				t.Fatal("refused selected credential fell back to human sign-in")
			}
		})
	}
	headers.Set("Authorization", "Bearer "+legacy)
	t.Run("method_fallback", func(t *testing.T) {
		res, body := s.request("PUT", consoleAPIKeysPath, "", headers)
		if res.StatusCode != 405 {
			t.Fatalf("status=%d body=%s want method fallback 405", res.StatusCode, body)
		}
	})
	t.Run("cross_origin_assertion", func(t *testing.T) {
		h := headers.Clone()
		h.Set("Origin", "https://foreign.example")
		before := ownedDurableSnapshot(t, s.pool)
		res, body := s.request("POST", consoleAPIKeysPath, `{"name":"must not persist"}`, h)
		if res.StatusCode != 403 || laneMessage(t, string(body)) != "invalid browser origin" {
			t.Fatalf("status=%d body=%s want origin refusal", res.StatusCode, body)
		}
		if ownedDurableSnapshot(t, s.pool) != before {
			t.Fatal("cross-origin assertion changed durable state")
		}
	})
	t.Run("stored_role_is_selected", func(t *testing.T) {
		if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='user' WHERE id=$1`, human); err != nil {
			t.Fatal(err)
		}
		status, typ, body := laneRead(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers))
		if status != 403 || typ != "permission_error" || laneMessage(t, body) != "this route requires the developer role" {
			t.Fatalf("status=%d error=%q body=%s want current human role refusal", status, typ, body)
		}
	})
}

func TestPrivateLegacyBearerLookupFailure(t *testing.T) {
	s, headers, _ := privateAssertionServer(t)
	legacy, _, err := api.IssueManagementKey(t.Context(), s.pool, "legacy companion", nil, "apikey_companion_issuer")
	if err != nil {
		t.Fatal(err)
	}
	headers.Set("Authorization", "Bearer "+legacy)
	s.pool.Close()
	assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers), 500)
}

// Neither the raw nor the mux-cleaned public boundary may inherit the private
// legacy exception. Encoded dots and slashes remain literal route segments.
func TestPrivateLegacyBearerCanonicalization(t *testing.T) {
	s := newLaneServerWith(t, func(c *identity.Config) {
		c.Mode = identity.ModeTrustedProxy
		c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
	})
	legacy, _, err := api.IssueManagementKey(t.Context(), s.pool, "legacy companion", nil, "apikey_companion_issuer")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, location string
		want                 int
	}{
		{"private", consoleAPIKeysPath, "", 200},
		{"encoded_private", "/%61pi/console/organizations/default/workspaces/default/api_keys", "", 200},
		{"private_cleaning", "/other/.." + consoleAPIKeysPath, consoleAPIKeysPath, 307},
		{"private_repeated_slash", "/" + consoleAPIKeysPath, consoleAPIKeysPath, 307},
		{"unknown_private", "/api/unknown", "", 404},
		{"encoded_dot", "/%2e/v1/agents", "", 404},
		{"encoded_parent", "/api/%2e%2e/v1/agents", "", 404},
		{"encoded_slash", "/v1%2fapi/agents", "", 404},
		{"raw_public_to_private", "/v1/.." + consoleAPIKeysPath, "", 401},
		{"encoded_public_to_private", "/%761/.." + consoleAPIKeysPath, "", 401},
		{"private_to_public", "/api/../v1/agents", "", 401},
		{"private_to_encoded_public", "/api/../%761/agents", "", 401},
		{"public", "/v1/agents", "", 401},
		{"encoded_public", "/%761/agents", "", 401},
		{"public_repeated_slash", "//v1/agents", "", 401},
		{"dual_events", "/v1/sessions/sesn_nonexistent/events", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := s.claims("platform-admins")
			claims["sub"] = tc.name // A rejected boundary must not provision this subject.
			headers := http.Header{"Authorization": {"Bearer " + legacy}, "X-Goog-IAP-JWT-Assertion": {s.idp.Mint(t, claims)}}
			before := ownedDurableSnapshot(t, s.pool)
			res := ownedFirstHop(t, s.url, tc.path, headers)
			if location := res.Header.Get("Location"); location != tc.location {
				t.Errorf("location=%q want=%q", location, tc.location)
			}
			assertOwnedResponse(t, res, tc.want)
			if tc.want == 401 && ownedDurableSnapshot(t, s.pool) != before {
				t.Fatal("public boundary provisioned or refreshed a human")
			}
			if tc.location != "" {
				assertOwnedResponse(t, ownedFirstHop(t, s.url, tc.location, headers), 200)
			}
		})
	}
}
