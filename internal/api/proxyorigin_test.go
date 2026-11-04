package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// A proxy may authenticate and attach an assertion to a browser's simple form.
// The native private boundary must reject it before the real revoke handler.
func TestNativeProxyAssertionBrowserMutation(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, raw := s.request("POST", "/api/auth/login", "", h)
	cookie, session := sessionReply(t, res, raw)
	if _, err := s.pool.Exec(context.Background(), `UPDATE principals SET role='admin'`); err != nil {
		t.Fatal(err)
	}
	_, env := pgtest.NewSession(t, s.pool, "self_hosted")
	target := func() (string, string) {
		t.Helper()
		if _, err := api.IssueEnvironmentKey(context.Background(), s.pool, env.String(), "browser-target"); err != nil {
			t.Fatal(err)
		}
		keys, _, err := api.ListEnvironmentKeys(context.Background(), s.pool, env.String(), 100, 0)
		if err != nil || len(keys) == 0 {
			t.Fatalf("keys=%v err=%v", keys, err)
		}
		return consoleRevoke(env.String(), keys[0].ID), keys[0].ID
	}
	res, raw = s.request("POST", "/api/auth/logout", "", nativeHeaders(cookie, session.CSRFToken))
	if res.StatusCode != 200 {
		t.Fatalf("logout=%d %s", res.StatusCode, raw)
	}
	var principalID string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM principals WHERE issuer=$1`, s.idp.Issuer()).Scan(&principalID); err != nil {
		t.Fatal(err)
	}
	validCookie, _ := s.issue(principalID)
	for _, offeredCookie := range []string{"", validCookie, cookie} {
		for name, signals := range map[string]http.Header{
			"cross-site":             {"Origin": {"https://attacker.example"}, "Sec-Fetch-Site": {"cross-site"}},
			"same-site-cross-origin": {"Origin": {"https://other.console.example"}, "Sec-Fetch-Site": {"same-site"}},
			"foreign":                {"Origin": {"https://foreign.example"}},
			"null":                   {"Origin": {"null"}},
			"duplicate":              {"Origin": {nativeOrigin, nativeOrigin}},
			"conflicting-metadata":   {"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"cross-site"}},
			"duplicate-metadata":     {"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"same-origin", "cross-site"}},
			"missing-origin":         {"Sec-Fetch-Mode": {"navigate"}},
			"default-port":           {"Origin": {nativeOrigin + ":443"}},
		} {
			t.Run(name+"/"+offeredCookie[:min(len(offeredCookie), 12)], func(t *testing.T) {
				path, id := target()
				headers := signals.Clone()
				headers.Set("Content-Type", "application/x-www-form-urlencoded")
				headers.Set("X-Goog-IAP-JWT-Assertion", token)
				if offeredCookie != "" {
					headers.Set("Cookie", offeredCookie)
				}
				res, raw := s.request("POST", path, "x=y", headers)
				if res.StatusCode != 403 {
					t.Errorf("asserted browser form=%d %s, want 403", res.StatusCode, raw)
				}
				var revoked bool
				if err := s.pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM environment_keys WHERE id=$1`, id).Scan(&revoked); err != nil || revoked {
					t.Fatalf("revoke side effect=%v err=%v", revoked, err)
				}
			})
		}
	}
	for name, headers := range map[string]http.Header{
		"same-origin":      {"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"same-origin"}, "X-Goog-Iap-Jwt-Assertion": {token}},
		"nonbrowser":       {"X-Goog-Iap-Jwt-Assertion": {token}},
		"explicit-machine": {"Origin": {"https://attacker.example"}, "X-Goog-Iap-Jwt-Assertion": {token}, "X-Api-Key": {testKey}},
	} {
		t.Run(name, func(t *testing.T) {
			path, _ := target()
			res, raw := s.request("POST", path, "x=y", headers)
			if res.StatusCode != 204 {
				t.Fatalf("compatible lane=%d %s", res.StatusCode, raw)
			}
		})
	}
	path, _ := target()
	h = nativeHeaders(validCookie, "")
	h.Set("X-Api-Key", "invalid-key")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, raw = s.request("POST", path, "", h)
	if res.StatusCode != 401 {
		t.Fatalf("invalid key fell back=%d %s", res.StatusCode, raw)
	}
	res, raw = s.request("GET", "/v1/agents", "", http.Header{"Cookie": {validCookie}})
	if res.StatusCode != 401 {
		t.Fatalf("public cookie=%d %s", res.StatusCode, raw)
	}
	res, raw = s.request("GET", membersPath, "", http.Header{"X-Goog-Iap-Jwt-Assertion": {token}})
	if res.StatusCode != 401 {
		t.Fatalf("new route assertion=%d %s", res.StatusCode, raw)
	}
}
