package api_test

import (
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// Observe authentication before the mux's redirect. Following it automatically
// would hide a first-hop sign-in behind the public target's eventual 401.
func ownedFirstHop(t *testing.T, base, path string, headers http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestPublicHumanCanonicalizationDoesNotSignIn(t *testing.T) {
	for _, mode := range []identity.Mode{identity.ModeOIDC, identity.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newLaneServerWith(t, func(c *identity.Config) {
				c.Mode = mode
				if mode == identity.ModeTrustedProxy {
					c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
				}
			})
			for _, state := range []string{"new", "existing", "deactivated"} {
				for _, tc := range []struct{ name, path, target string }{
					{"repeated_slash", "//v1/agents", "/v1/agents"},
					{"dot", "/./v1/agents", "/v1/agents"},
					{"private_parent", "/api/../v1/agents", "/v1/agents"},
					{"nested_parents", "/api//nested/../../v1/agents", "/v1/agents"},
					{"encoded_private_parent", "/%61pi/../v1/agents", "/v1/agents"},
					{"encoded_public", "/api/../%761/agents", "/%761/agents"},
					{"dual_events", "/./v1/sessions/sesn_nonexistent/events", "/v1/sessions/sesn_nonexistent/events"},
					{"dual_file", "//v1/files/file_nonexistent/content", "/v1/files/file_nonexistent/content"},
				} {
					t.Run(state+"/"+tc.name, func(t *testing.T) {
						claims := s.claims("platform-admins")
						claims["sub"] = rand.Text()
						headers := make(http.Header)
						setToken := func() {
							token := s.idp.Mint(t, claims)
							if mode == identity.ModeOIDC {
								headers.Set("Authorization", "Bearer "+token)
							} else {
								headers.Set("X-Goog-IAP-JWT-Assertion", token)
							}
						}
						setToken()
						if state != "new" {
							assertOwnedResponse(t, ownedFirstHop(t, s.url, consoleAPIKeysPath, headers), 200)
							status := "active"
							if state == "deactivated" {
								status = "deactivated"
							}
							_, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='user', role_source='console',
								email='old@example.test', display_name='Old Profile',
								last_seen_at='2026-01-01T00:00:00Z', status=$2 WHERE subject=$1`, claims["sub"], status)
							if err != nil {
								t.Fatal(err)
							}
						}
						claims["email"] = "new@example.test"
						claims["name"] = "New Profile"
						setToken()
						before := ownedDurableSnapshot(t, s.pool)
						for _, hop := range []struct{ name, path string }{{"first", tc.path}, {"public_target", tc.target}} {
							res := ownedFirstHop(t, s.url, hop.path, headers)
							location := res.Header.Get("Location")
							status, typ, body := laneRead(t, res)
							if after := ownedDurableSnapshot(t, s.pool); after != before {
								t.Errorf("%s hop changed persisted principals or keys: status=%d location=%q", hop.name, status, location)
							}
							if status != 401 || typ != "authentication_error" || location != "" {
								t.Errorf("%s hop: status=%d location=%q body=%s; want authentication 401 without redirect", hop.name, status, location, body)
							}
						}
					})
				}
			}
		})
	}
}

// Cleaning only the decoded URL would mistake encoded dots/slashes for routing
// separators. Cleaning alone would also revive human admission for raw /v1.
func TestHumanCanonicalizationBoundaryControls(t *testing.T) {
	for _, mode := range []identity.Mode{identity.ModeOIDC, identity.ModeTrustedProxy} {
		for _, tc := range []struct {
			name, path, location string
			want                 int
			private              bool
		}{
			{"raw_public_to_private", "/v1/.." + consoleAPIKeysPath, "", 401, false},
			{"encoded_public_to_private", "/%761/.." + consoleAPIKeysPath, "", 401, false},
			{"private_cleaning", "/other/.." + consoleAPIKeysPath, consoleAPIKeysPath, 307, true},
			{"private_repeated_slash", "/" + consoleAPIKeysPath, consoleAPIKeysPath, 307, true},
			{"encoded_private", "/%61pi/console/organizations/default/workspaces/default/api_keys", "", 200, true},
			{"encoded_dot_is_literal", "/%2e/v1/agents", "", 404, true},
			{"encoded_parent_is_literal", "/api/%2e%2e/v1/agents", "", 404, true},
			{"encoded_slash_is_literal", "/v1%2fapi/agents", "", 404, true},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				s := newLaneServerWith(t, func(c *identity.Config) {
					c.Mode = mode
					if mode == identity.ModeTrustedProxy {
						c.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
					}
				})
				headers := make(http.Header)
				if mode == identity.ModeOIDC {
					headers.Set("Authorization", "Bearer "+s.token("platform-admins"))
				} else {
					headers.Set("X-Goog-IAP-JWT-Assertion", s.token("platform-admins"))
				}
				before := ownedDurableSnapshot(t, s.pool)
				res := ownedFirstHop(t, s.url, tc.path, headers)
				if location := res.Header.Get("Location"); location != tc.location {
					t.Errorf("location=%q want=%q", location, tc.location)
				}
				assertOwnedResponse(t, res, tc.want)
				if changed := ownedDurableSnapshot(t, s.pool) != before; changed != tc.private {
					t.Fatalf("persisted principal changed=%t want=%t", changed, tc.private)
				}
				if tc.location != "" {
					assertOwnedResponse(t, ownedFirstHop(t, s.url, tc.location, headers), 200)
				}
			})
		}
	}
}

func TestOwnedKeyCanonicalizationBoundary(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := s.seedLocal("admin", "admin", "active")
	raw, _ := ownedKeyFixture(t, s.pool, admin, admin)
	cookie, session := s.issue(admin)
	before := ownedDurableSnapshot(t, s.pool)
	for _, tc := range []struct {
		name, path, location string
		want                 int
	}{
		{"public", "/v1/agents", "", 200},
		{"encoded_public", "/%761/agents", "", 200},
		{"public_cleaning", "/v1/unused/../agents", "/v1/agents", 307},
		{"public_repeated_slash", "/v1//agents", "/v1/agents", 307},
		{"public_encoded_parent_is_literal", "/v1/%2e%2e/api/unknown", "", 404},
		{"private", consoleAPIKeysPath, "", 401},
		{"encoded_private", "/%61pi/console/organizations/default/workspaces/default/api_keys", "", 401},
		{"public_to_private", "/v1/.." + consoleAPIKeysPath, "", 401},
		{"encoded_public_to_private", "/%761/.." + consoleAPIKeysPath, "", 401},
		{"public_to_encoded_private", "/v1/../%61pi/unknown", "", 401},
		{"private_to_public", "/api/../v1/agents", "", 401},
		{"leading_repeated_slash", "//v1/agents", "", 401},
		{"private_cleaning", "/other/.." + consoleAPIKeysPath, "", 401},
		{"encoded_slash_is_literal", "/v1%2fapi/agents", "", 401},
		{"encoded_dot_is_literal", "/%2e/v1/agents", "", 401},
		{"double_encoded_public", "/%25761/agents", "", 401},
	} {
		for _, header := range []string{"x-api-key", "Bearer"} {
			t.Run(tc.name+"/"+header, func(t *testing.T) {
				headers := nativeHeaders(cookie, session.CSRFToken)
				for k, v := range ownedHeaders(header, raw) {
					headers.Set(k, v)
				}
				res := ownedFirstHop(t, s.url, tc.path, headers)
				if location := res.Header.Get("Location"); location != tc.location {
					t.Errorf("location=%q want=%q", location, tc.location)
				}
				assertOwnedResponse(t, res, tc.want)
				if tc.location != "" {
					assertOwnedResponse(t, ownedFirstHop(t, s.url, tc.location, headers), 200)
				}
			})
		}
	}
	if after := ownedDurableSnapshot(t, s.pool); after != before {
		t.Fatal("owned-key boundary requests changed durable keys or principals")
	}
	assertOwnedResponse(t, ownedFirstHop(t, s.url, "/api/organizations/default/proxy/v1/agents", nativeHeaders(cookie, session.CSRFToken)), 200)
}
