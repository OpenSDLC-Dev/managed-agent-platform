package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/admin"
)

const cookieOrigin = "https://console.example"

func newCookieAdmin(t *testing.T, mode login.Mode, origin string, withIdentity bool) (*env, *login.Manager) {
	t.Helper()
	e := newEnv(t, withIdentity)
	m := login.New(e.pool, login.Config{Mode: mode, PublicOrigin: origin, Development: true})
	cfg := admin.Config{Store: e.store, Cipher: e.cipher, BootstrapKey: bootstrap, Prober: e.prober, Login: m}
	if withIdentity {
		ic := identity.Config{Mode: identity.ModeOIDC, Issuer: e.idp.Issuer(), Audience: audience, JWKSURL: e.idp.JWKSURL(),
			RoleMap: map[string]identity.Role{"admins": identity.RoleAdmin, "devs": identity.RoleDeveloper}, HTTPClient: e.idp.Client(), Now: e.clock.Now}
		if mode == login.ModeTrustedProxy {
			ic.Mode, ic.AssertionHeader = identity.ModeTrustedProxy, "X-Goog-IAP-JWT-Assertion"
		}
		var err error
		cfg.Verifier, err = identity.New(context.Background(), ic)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Principals = e.pool
	}
	h, err := admin.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.url = srv.URL
	return e, m
}

func cookieAdminRequest(t *testing.T, e *env, method, path, body string, headers http.Header) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, string(raw)
}

func cookieAdminHeaders(issued login.IssuedSession) http.Header {
	return http.Header{"Cookie": {"map_session=" + issued.Token}, "Origin": {cookieOrigin}, "X-Csrf-Token": {issued.CSRFToken}}
}

// A cached login role, sign-in mapping/profile replay, missing CSRF or wrong sign-out marker fails this.
func TestGatewayCookieUsesCurrentRole(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeOIDC, cookieOrigin, true)
	e.ok(e.do("GET", "/admin/v1/profiles", nil, map[string]string{"Authorization": "Bearer " + e.token("admins")}), "legacy sign-in")
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM principals`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	issued, err := m.Issue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	h := cookieAdminHeaders(issued)
	for _, cookie := range []string{"", "map_session=not-canonical", h.Get("Cookie") + "; " + h.Get("Cookie"), `map_session="` + issued.Token + `"`} {
		status, header, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", http.Header{"Cookie": {cookie}})
		if status != 401 || header.Get("x-platform-signed-out") != "1" {
			t.Fatalf("invalid browser session=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
		}
	}
	e.setPrincipal(`role='developer',role_source='console',email='current@example.test',display_name='Current profile',last_seen_at='2026-01-01T00:00:00Z'`)
	for _, path := range []string{"/admin/v1/profiles", "/admin/v1/usage/daily", "/admin/v1/usage/requests", "/admin/%761/%70rofiles"} {
		status, header, raw := cookieAdminRequest(t, e, "GET", path, "", h)
		if status != 200 || header.Get("x-platform-signed-out") != "" {
			t.Fatalf("current developer read=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
		}
	}
	body := `{"name":"cookie-provider","profile":"deepseek","endpoints":{"anthropic":"https://api.deepseek.com/anthropic"}}`
	status, header, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/providers", body, h)
	if status != 403 || !strings.Contains(raw, "admin") || header.Get("x-platform-signed-out") != "" {
		t.Fatalf("developer write=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
	}
	var role, source, email, name string
	var seen time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT role,role_source,email,display_name,last_seen_at FROM principals WHERE id=$1`, id).Scan(&role, &source, &email, &name, &seen); err != nil || role != "developer" || source != "console" || email != "current@example.test" || name != "Current profile" || !seen.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cookie replayed profile/mapping: %s/%s/%s/%s/%v err=%v", role, source, email, name, seen, err)
	}
	e.setPrincipal(`role='user'`)
	for _, path := range []string{"/admin/v1/profiles", "/admin/v1/nothing"} {
		status, header, raw := cookieAdminRequest(t, e, "GET", path, "", h)
		if status != 403 || !strings.Contains(raw, "developer") || header.Get("x-platform-signed-out") != "" {
			t.Fatalf("user read=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
		}
	}
	e.setPrincipal(`role='admin'`)
	for name, mutate := range map[string]func(http.Header){
		"missing-origin":      func(h http.Header) { h.Del("Origin") },
		"foreign-origin":      func(h http.Header) { h.Set("Origin", "https://other.example") },
		"noncanonical-origin": func(h http.Header) { h.Set("Origin", cookieOrigin+":443") },
		"duplicate-origin":    func(h http.Header) { h.Add("Origin", cookieOrigin) },
		"missing-csrf":        func(h http.Header) { h.Del("X-CSRF-Token") },
		"wrong-csrf":          func(h http.Header) { h.Set("X-CSRF-Token", strings.Repeat("x", 43)) },
		"duplicate-csrf":      func(h http.Header) { h.Add("X-CSRF-Token", issued.CSRFToken) },
		"cross-site":          func(h http.Header) { h.Set("Sec-Fetch-Site", "cross-site") },
	} {
		t.Run(name, func(t *testing.T) {
			for _, method := range []string{"POST", "DELETE"} {
				offered := h.Clone()
				mutate(offered)
				status, header, raw := cookieAdminRequest(t, e, method, "/admin/v1/providers", body, offered)
				if status != 403 || header.Get("x-platform-signed-out") != "" {
					t.Fatalf("CSRF=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
				}
			}
		})
	}
	status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/providers", body, h)
	if status != 200 {
		t.Fatalf("admin write=%d %s", status, raw)
	}
	for _, tc := range []struct {
		name, query string
		want        int
		marker      string
	}{
		{"deactivate", `UPDATE principals SET status='deactivated' WHERE id=$1`, 403, "1"},
		{"expire", `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second' WHERE principal_id=$1`, 401, "1"},
		{"revoke", `DELETE FROM browser_sessions WHERE principal_id=$1`, 401, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.setPrincipal(`status='active'`)
			current, err := m.Issue(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.pool.Exec(context.Background(), tc.query, id); err != nil {
				t.Fatal(err)
			}
			status, header, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", cookieAdminHeaders(current))
			if status != tc.want || header.Get("x-platform-signed-out") != tc.marker {
				t.Fatalf("withdrawal=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
			}
		})
	}
	e.setPrincipal(`status='active'`)
	current, err := m.Issue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `ALTER TABLE browser_sessions RENAME TO unavailable_sessions`); err != nil {
		t.Fatal(err)
	}
	status, header, raw = cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", cookieAdminHeaders(current))
	if status != 500 || header.Get("x-platform-signed-out") != "" {
		t.Fatalf("store outage=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
	}
}

// Offered legacy credentials retain precedence and never fall back, including empty/repeated fields.
func TestGatewayCookieLegacyPrecedence(t *testing.T) {
	for _, mode := range []login.Mode{login.ModeOIDC, login.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			e, m := newCookieAdmin(t, mode, cookieOrigin, true)
			if _, err := e.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_cookie','https://cookie.example','cookie','admin')`); err != nil {
				t.Fatal(err)
			}
			issued, err := m.Issue(context.Background(), "principal_cookie")
			if err != nil {
				t.Fatal(err)
			}
			valid := cookieAdminHeaders(issued)
			for name, credential := range map[string]http.Header{
				"invalid-key": {"X-Api-Key": {"wrong"}}, "empty-key": {"X-Api-Key": {""}},
				"duplicate-key":  {"X-Api-Key": {bootstrap, bootstrap}},
				"invalid-bearer": {"Authorization": {"Bearer invalid"}}, "empty-authorization": {"Authorization": {""}},
				"wrong-scheme":            {"Authorization": {"Basic invalid"}},
				"duplicate-authorization": {"Authorization": {"Bearer " + bootstrap, "Bearer " + bootstrap}},
			} {
				t.Run(name, func(t *testing.T) {
					h := valid.Clone()
					for k, v := range credential {
						h[k] = v
					}
					status, header, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
					if status != 401 || header.Get("x-platform-signed-out") != "" {
						t.Fatalf("invalid offered legacy credential fell back=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
					}
				})
			}
			for _, token := range []string{"", "invalid.jwt.value"} {
				if mode != login.ModeTrustedProxy {
					continue
				}
				h := valid.Clone()
				h.Set("X-Goog-IAP-JWT-Assertion", token)
				status, header, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
				if status != 401 || header.Get("x-platform-signed-out") != "" {
					t.Fatalf("offered assertion fallback=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
				}
			}
			// The legacy operator token, not the admin cookie, determines the role.
			h := valid.Clone()
			field, token := "Authorization", "Bearer "+e.token("devs")
			if mode == login.ModeTrustedProxy {
				field, token = "X-Goog-IAP-JWT-Assertion", e.token("devs")
			}
			h.Set(field, token)
			status, header, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/providers", `{}`, h)
			if status != 403 || header.Get("x-platform-signed-out") != "" {
				t.Fatalf("operator precedence=%d %s marker=%q", status, raw, header.Get("x-platform-signed-out"))
			}
			for _, field := range []string{"X-Api-Key", "Authorization"} {
				h := http.Header{"Cookie": {"map_session=not-canonical"}}
				value := bootstrap
				if field == "Authorization" {
					value = "Bearer " + value
				}
				h.Set(field, value)
				status, _, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", h)
				if status != 200 {
					t.Fatalf("bootstrap precedence=%d %s", status, raw)
				}
			}
		})
	}
}

func TestGatewayCookieNeedsNativeLogin(t *testing.T) {
	for _, tc := range []struct {
		mode   login.Mode
		origin string
		legacy bool
	}{{login.ModeDisabled, cookieOrigin, false}, {login.ModeOIDC, "", true}, {login.ModePassword, cookieOrigin, false}} {
		t.Run(string(tc.mode)+"/"+tc.origin, func(t *testing.T) {
			e, m := newCookieAdmin(t, tc.mode, tc.origin, tc.legacy)
			if _, err := e.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_reader','https://control.example','control','developer')`); err != nil {
				t.Fatal(err)
			}
			issued, err := m.Issue(context.Background(), "principal_reader")
			if err != nil {
				t.Fatal(err)
			}
			want := 401
			if tc.mode == login.ModePassword {
				want = 200
			}
			status, _, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", cookieAdminHeaders(issued))
			if status != want {
				t.Fatalf("native gate=%d %s want=%d", status, raw, want)
			}
			e.ok(e.admin("GET", "/admin/v1/profiles", nil), "legacy bootstrap remains")
			if tc.legacy {
				e.ok(e.do("GET", "/admin/v1/profiles", nil, map[string]string{"Authorization": "Bearer " + e.token("admins")}), "verifier-only operator remains")
			}
		})
	}
}
