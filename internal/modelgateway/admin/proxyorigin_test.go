package admin_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestNativeProxyAssertionBrowserMutation(t *testing.T) {
	e, m := newCookieAdmin(t, login.ModeTrustedProxy, cookieOrigin, true)
	token := e.token("admins")
	assertion := http.Header{"X-Goog-Iap-Jwt-Assertion": {token}}
	status, _, raw := cookieAdminRequest(t, e, "GET", "/admin/v1/profiles", "", assertion)
	if status != 200 {
		t.Fatalf("sign in=%d %s", status, raw)
	}
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM principals`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	valid, err := m.Issue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := m.Issue(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", cookieOrigin+"/api/auth/logout", nil)
	req.Header.Set("Cookie", "map_session="+revoked.Token)
	if err := m.Revoke(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// No vendor call: this real handler only stores a provider.
	body := `{"name":"form-provider","profile":"deepseek","endpoints":{"anthropic":"https://api.deepseek.com/anthropic"}}`
	for _, cookie := range []string{"", "map_session=" + valid.Token, "map_session=" + revoked.Token} {
		for name, signals := range map[string]http.Header{
			"cross-site":             {"Origin": {"https://attacker.example"}, "Sec-Fetch-Site": {"cross-site"}},
			"same-site-cross-origin": {"Origin": {"https://other.console.example"}, "Sec-Fetch-Site": {"same-site"}},
			"null":                   {"Origin": {"null"}},
			"duplicate":              {"Origin": {cookieOrigin, cookieOrigin}},
			"conflicting-metadata":   {"Origin": {cookieOrigin}, "Sec-Fetch-Site": {"cross-site"}},
			"duplicate-metadata":     {"Origin": {cookieOrigin}, "Sec-Fetch-Site": {"same-origin", "cross-site"}},
			"missing-origin":         {"Sec-Fetch-Mode": {"navigate"}},
			"default-port":           {"Origin": {cookieOrigin + ":443"}},
		} {
			t.Run(name+"/"+cookie[:min(len(cookie), 12)], func(t *testing.T) {
				headers := signals.Clone()
				headers.Set("X-Goog-IAP-JWT-Assertion", token)
				headers.Set("Content-Type", "text/plain")
				if cookie != "" {
					headers.Set("Cookie", cookie)
				}
				status, _, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/providers", body, headers)
				if status != 403 {
					t.Errorf("asserted browser form=%d %s, want 403", status, raw)
				}
				var count int
				if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM modelgateway.providers`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("provider side effect=%d err=%v", count, err)
				}
			})
		}
	}
	for name, h := range map[string]http.Header{
		"same-origin":      {"Origin": {cookieOrigin}, "Sec-Fetch-Site": {"same-origin"}, "X-Goog-Iap-Jwt-Assertion": {token}},
		"nonbrowser":       {"X-Goog-Iap-Jwt-Assertion": {token}},
		"explicit-machine": {"Origin": {"https://attacker.example"}, "X-Goog-Iap-Jwt-Assertion": {token}, "X-Api-Key": {bootstrap}},
	} {
		t.Run(name, func(t *testing.T) {
			status, _, raw := cookieAdminRequest(t, e, "POST", "/admin/v1/providers", body, h)
			if status != 200 {
				t.Fatalf("compatible lane=%d %s", status, raw)
			}
			if _, err := e.pool.Exec(context.Background(), `DELETE FROM modelgateway.providers`); err != nil {
				t.Fatal(err)
			}
		})
	}
	h := cookieAdminHeaders(valid)
	h.Set("X-Api-Key", "invalid")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	status, _, raw = cookieAdminRequest(t, e, "POST", "/admin/v1/providers", body, h)
	if status != 401 {
		t.Fatalf("invalid key fell back=%d %s", status, raw)
	}
}
