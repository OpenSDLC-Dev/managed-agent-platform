package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

// A historical unowned key remains an audit record, never current authority.
// Private routes require the browser session even when a valid IdP assertion
// accompanies the request. The same assertion still works at sign-in.
func TestNoPresetAdmission(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, body := s.request("POST", "/api/auth/login", "", h)
	cookie, session := sessionReply(t, res, body)
	if _, err := s.pool.Exec(t.Context(), `UPDATE principals SET role='admin'`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/agents", consoleAPIKeysPath, "/api/%63onsole/organizations/default/workspaces/default/api_keys"} {
		for name, headers := range map[string]http.Header{
			"unowned-key": {"X-Api-Key": {testKey}},
			"assertion":   {"X-Goog-IAP-JWT-Assertion": {token}},
			"idp-bearer":  {"Authorization": {"Bearer " + token}},
		} {
			t.Run(name+path, func(t *testing.T) {
				res, body := s.request("GET", path, "", headers)
				if res.StatusCode != http.StatusUnauthorized {
					t.Fatalf("retired credential admitted: status=%d body=%s", res.StatusCode, body)
				}
			})
		}
	}
	headers := nativeHeaders(cookie, session.CSRFToken)
	key, _ := issueOwnedHTTP(t, s, headers, "current owned key", nil)
	for _, headers := range []http.Header{{"X-Api-Key": {key}}, {"Authorization": {"Bearer " + key}}} {
		res, body := s.request("GET", "/v1/agents", "", headers)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("owned public key refused: status=%d body=%s", res.StatusCode, body)
		}
		res, body = s.request("GET", consoleAPIKeysPath, "", headers)
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("private key admitted: status=%d body=%s", res.StatusCode, body)
		}
	}
	res, body = s.request("GET", consoleAPIKeysPath, "", headers)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("private cookie refused: status=%d body=%s", res.StatusCode, body)
	}
}
