package api_test

import (
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ownedKeyFixture inserts the immutable 0060 credential shape. Issuer and owner
// are independent inputs; this is not an issuance API or an ownership backfill.
func ownedKeyFixture(t *testing.T, pool *pgxpool.Pool, ownerID, issuerID string) (raw, keyID string) {
	t.Helper()
	raw = api.IssuedKeyPrefix + rand.Text()
	keyID = domain.NewID(domain.PrefixAPIKey).String()
	_, err := pool.Exec(t.Context(), `INSERT INTO api_keys
		(id,name,owned_key_hash,owner_principal_id,created_by)
		VALUES ($1,'owned fixture',$2,$3,$4)`, keyID, apikey.Hash(raw), ownerID, issuerID)
	if err != nil {
		t.Fatal(err)
	}
	return raw, keyID
}

func ownedPrincipalFixture(t *testing.T, pool *pgxpool.Pool, kind, role string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixPrincipal).String()
	_, err := pool.Exec(t.Context(), `INSERT INTO principals(id,issuer,subject,kind,role)
		VALUES ($1,'owned-key-fixture',$1,$2,$3)`, id, kind, role)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ownedHeaders(header, raw string) map[string]string {
	if header == "Bearer" {
		return asBearer(raw)
	}
	return map[string]string{"x-api-key": raw}
}

func assertOwnedResponse(t *testing.T, res *http.Response, want int) {
	t.Helper()
	status, typ, body := laneRead(t, res)
	if status != want {
		t.Fatalf("status=%d want=%d body=%s", status, want, body)
	}
	if want == 401 && typ != "authentication_error" || want == 403 && typ != "permission_error" {
		t.Fatalf("status=%d error=%q body=%s", status, typ, body)
	}
}

// Missing owner lookup, cached mint-time authority, or treating issuer as owner
// must fail these requests against the real database and HTTP dispatcher.
func TestOwnedKeyPublicAdmission(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	issuer := s.seedLocal("issuer", "admin", "active")
	for _, kind := range []string{"user", "service_account"} {
		for _, role := range []string{"developer", "admin"} {
			owner := ownedPrincipalFixture(t, s.pool, kind, role)
			raw, _ := ownedKeyFixture(t, s.pool, owner, issuer)
			for _, header := range []string{"x-api-key", "Bearer"} {
				t.Run(kind+"/"+role+"/"+header, func(t *testing.T) {
					assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders(header, raw)), 200)
				})
			}
		}
	}
	t.Run("current_role", func(t *testing.T) {
		owner := s.seedLocal("demoted", "developer", "active")
		raw, _ := ownedKeyFixture(t, s.pool, owner, issuer)
		for _, change := range []struct {
			role string
			want int
		}{{"developer", 200}, {"user", 403}, {"admin", 200}} {
			if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role=$2 WHERE id=$1`, owner, change.role); err != nil {
				t.Fatal(err)
			}
			for _, header := range []string{"x-api-key", "Bearer"} {
				t.Run(change.role+"/"+header, func(t *testing.T) {
					assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders(header, raw)), change.want)
				})
			}
		}
	})
	for _, state := range []string{"inactive", "archived", "expired", "deactivated_owner", "foreign_owner"} {
		t.Run(state, func(t *testing.T) {
			owner := ownedPrincipalFixture(t, s.pool, "user", "admin")
			// Change the owner before inserting, so the deactivation trigger cannot
			// make the active-key/removed-owner case pass by archiving the key.
			switch state {
			case "deactivated_owner":
				_, err := s.pool.Exec(t.Context(), `UPDATE principals SET status='deactivated' WHERE id=$1`, owner)
				if err != nil {
					t.Fatal(err)
				}
			case "foreign_owner":
				_, err := s.pool.Exec(t.Context(), `UPDATE principals SET org_id='other' WHERE id=$1`, owner)
				if err != nil {
					t.Fatal(err)
				}
			}
			raw, keyID := ownedKeyFixture(t, s.pool, owner, issuer)
			if state == "inactive" || state == "archived" {
				if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET status=$2 WHERE id=$1`, keyID, state); err != nil {
					t.Fatal(err)
				}
			}
			if state == "expired" {
				if _, err := s.pool.Exec(t.Context(), `UPDATE api_keys SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, keyID); err != nil {
					t.Fatal(err)
				}
			}
			for _, header := range []string{"x-api-key", "Bearer"} {
				assertOwnedResponse(t, s.doRaw("GET", "/v1/agents", nil, ownedHeaders(header, raw)), 401)
			}
		})
	}
}

// These paths exercise every dispatch arm without starting an unbounded stream.
var ownedDispatchPaths = []string{
	"/v1/agents", "/v1/sessions/sesn_nonexistent",
	"/v1/sessions/sesn_nonexistent/events", "/v1/sessions/sesn_nonexistent/events/stream",
	"/v1/skills/skill_nonexistent/versions", "/v1/skills/skill_nonexistent/versions/1",
	"/v1/skills/skill_nonexistent/versions/1/content", "/v1/files/file_nonexistent/content",
}

func TestOwnedKeyCredentialDispatch(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	admin := s.seedLocal("admin", "admin", "active")
	cookie, session := s.issue(admin)
	user := s.seedLocal("user", "user", "active")
	userKey, _ := ownedKeyFixture(t, s.pool, user, admin)
	adminKey, _ := ownedKeyFixture(t, s.pool, admin, admin)
	legacy, _, err := api.IssueManagementKey(t.Context(), s.pool, "legacy issued", nil, admin)
	if err != nil {
		t.Fatal(err)
	}
	// Native verifier uses the real clock, not the lane fixture's frozen clock.
	claims := s.idp.Claims(laneAudience, time.Now())
	assertion := s.idp.Mint(t, claims)
	for _, path := range ownedDispatchPaths {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				xkeys   []string
				auth    []string
				want    int
				message string
			}{
				{"owned_x_wins", []string{userKey}, []string{"Bearer " + adminKey}, 403, requiresDeveloper},
				{"invalid_x_wins", []string{"wrong"}, []string{"Bearer " + adminKey}, 401, "invalid x-api-key"},
				{"repeated_x", []string{"", adminKey}, []string{"Bearer " + adminKey}, 401, "multiple x-api-key headers"},
				{"repeated_empty_x", []string{"", ""}, []string{"Bearer " + adminKey}, 401, "multiple x-api-key headers"},
				{"empty_x_allows_bearer", []string{""}, []string{"Bearer " + userKey}, 403, requiresDeveloper},
				{"owned_bearer_role", nil, []string{"Bearer " + userKey}, 403, requiresDeveloper},
				{"legacy_bearer", nil, []string{"Bearer " + legacy}, 401, "invalid x-api-key"},
				{"unknown_issued_bearer", nil, []string{"Bearer " + api.IssuedKeyPrefix + "unknown"}, 401, "invalid x-api-key"},
				{"duplicate_authorization", nil, []string{"Bearer " + adminKey, "Bearer " + adminKey}, 401, "multiple Authorization headers"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					h := nativeHeaders(cookie, session.CSRFToken)
					h.Set("X-Goog-IAP-JWT-Assertion", assertion)
					for _, value := range tc.xkeys {
						h.Add("x-api-key", value)
					}
					for _, value := range tc.auth {
						h.Add("Authorization", value)
					}
					res, raw := s.request("GET", path, "", h)
					if res.StatusCode != tc.want || laneMessage(t, string(raw)) != tc.message {
						t.Fatalf("status=%d body=%s want=%d %q", res.StatusCode, raw, tc.want, tc.message)
					}
				})
			}
		})
	}
}

func TestOwnedKeyPrivateIsolation(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := s.seedLocal("admin", "admin", "active")
	cookie, session := s.issue(admin)
	raw, keyID := ownedKeyFixture(t, s.pool, admin, admin)
	service := ownedPrincipalFixture(t, s.pool, "service_account", "admin")
	serviceRaw, _ := ownedKeyFixture(t, s.pool, service, admin)
	paths := []string{consoleAPIKeysPath, consoleAPIKey(keyID), consoleOrgAPIKey(keyID),
		"/api/console/organizations/default/members", "/api/console/organizations/default/service_accounts",
		"/api/organizations/default/proxy/v1/agents", "/api/unknown"}
	before := ownedDurableSnapshot(t, s.pool)
	for kind, raw := range map[string]string{"user": raw, "service_account": serviceRaw} {
		for _, path := range paths {
			for _, prefix := range []string{"/api/", "/%61pi/"} {
				path := strings.Replace(path, "/api/", prefix, 1)
				for _, method := range []string{"GET", "POST", "PUT"} {
					for _, header := range []string{"x-api-key", "Bearer"} {
						t.Run(kind+"/"+method+path+"/"+header, func(t *testing.T) {
							h := nativeHeaders(cookie, session.CSRFToken)
							for k, v := range ownedHeaders(header, raw) {
								h.Set(k, v)
							}
							res, body := s.request(method, path, `{"name":"must not persist","status":"inactive"}`, h)
							if res.StatusCode != 401 {
								t.Fatalf("status=%d body=%s want 401", res.StatusCode, body)
							}
						})
					}
				}
			}
		}
	}
	if after := ownedDurableSnapshot(t, s.pool); after != before {
		t.Fatal("refused private key requests changed durable keys or principals")
	}
	res, body := s.request("GET", "/v1/agents", "", nativeHeaders(cookie, session.CSRFToken))
	if res.StatusCode != 401 {
		t.Fatalf("raw cookie on public route: status=%d body=%s", res.StatusCode, body)
	}
}

// Full row snapshots catch profile/role/last_seen changes as well as provisioning.
func ownedDurableSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
		'principals',(SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM principals p),
		'keys',(SELECT jsonb_agg(to_jsonb(k) ORDER BY id) FROM api_keys k))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestPublicHumanCredentialsDoNotSignIn(t *testing.T) {
	for _, mode := range []identity.Mode{identity.ModeOIDC, identity.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newLaneServerWith(t, func(c *identity.Config) {
				c.Mode = mode
				if mode == identity.ModeTrustedProxy {
					c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
				}
			})
			send := func(path, token string) *http.Response {
				if mode == identity.ModeOIDC {
					return s.bearer("GET", path, token, nil)
				}
				return s.doRaw("GET", path, nil, map[string]string{"X-Goog-IAP-JWT-Assertion": token})
			}
			for _, state := range []string{"new", "existing", "deactivated"} {
				if state == "existing" {
					assertOwnedResponse(t, send(consoleAPIKeysPath, s.token("platform-admins")), 200)
					setPrincipal(t, s, `role='user', role_source='console', last_seen_at='2026-01-01T00:00:00Z'`)
				}
				if state == "deactivated" {
					setPrincipal(t, s, `status='deactivated'`)
				}
				before := ownedDurableSnapshot(t, s.pool)
				claims := s.claims("platform-admins")
				claims["name"] = "Must Not Refresh"
				claims["email"] = "must-not-refresh@example.test"
				token := s.idp.Mint(t, claims)
				for _, path := range append(append([]string{}, ownedDispatchPaths...), "/%761/agents", "/v1/sessions/sesn_nonexistent/%65vents", "/v1/environments/env_nonexistent/%77ork") {
					t.Run(state+path, func(t *testing.T) { assertOwnedResponse(t, send(path, token), 401) })
				}
				if after := ownedDurableSnapshot(t, s.pool); after != before {
					t.Errorf("%s public credential changed principals or keys", state)
				}
			}
		})
	}
}
