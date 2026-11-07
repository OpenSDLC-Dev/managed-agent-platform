package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

const proxyOriginAgentBody = `{"name":"proxy-origin","model":"claude-opus-4-8"}`

func agentRowCounts(t *testing.T, s *tserver) [2]int {
	t.Helper()
	var counts [2]int
	if err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM agents),(SELECT count(*) FROM agent_versions)`).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	return counts
}

// Public agent creation refuses assertions even with a browser-simple body.
// The retained private human lane must still check ambient assertion origins.
func TestNativeProxyPublicManagementOrigin(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, raw := s.request("POST", "/api/auth/login", "", h)
	revoked, session := sessionReply(t, res, raw)
	var id string
	if err := s.pool.QueryRow(context.Background(), `UPDATE principals SET role='admin' WHERE issuer=$1 RETURNING id`, s.idp.Issuer()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	valid, _ := s.issue(id)
	res, raw = s.request("POST", "/api/auth/logout", "", nativeHeaders(revoked, session.CSRFToken))
	if res.StatusCode != 200 {
		t.Fatalf("logout=%d %s", res.StatusCode, raw)
	}
	for _, cookie := range []struct{ name, value string }{{"no-cookie", ""}, {"valid-cookie", valid}, {"revoked-cookie", revoked}} {
		for _, signals := range []struct {
			name string
			h    http.Header
		}{
			{"cross-site", http.Header{"Origin": {"https://attacker.example"}, "Sec-Fetch-Site": {"cross-site"}}},
			{"same-site-foreign", http.Header{"Origin": {"https://other.console.example"}, "Sec-Fetch-Site": {"same-site"}}},
			{"foreign", http.Header{"Origin": {"https://foreign.example"}}},
			{"null", http.Header{"Origin": {"null"}}},
			{"duplicate", http.Header{"Origin": {nativeOrigin, nativeOrigin}}},
			{"conflicting-metadata", http.Header{"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"cross-site"}}},
			{"duplicate-metadata", http.Header{"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"same-origin", "cross-site"}}},
			{"browser-without-origin", http.Header{"Sec-Fetch-Mode": {"navigate"}}},
		} {
			t.Run(cookie.name+"/"+signals.name, func(t *testing.T) {
				headers := signals.h.Clone()
				headers.Set("Content-Type", "text/plain")
				headers.Set("X-Goog-IAP-JWT-Assertion", token)
				if cookie.value != "" {
					headers.Set("Cookie", cookie.value)
				}
				before := agentRowCounts(t, s.tserver)
				res, raw := s.request("POST", "/v1/agents", proxyOriginAgentBody, headers)
				if res.StatusCode != 401 {
					t.Errorf("asserted public browser mutation=%d %s, want 401", res.StatusCode, raw)
				}
				if after := agentRowCounts(t, s.tserver); after != before {
					t.Errorf("refused browser mutation wrote agent/version rows: before=%v after=%v", before, after)
				}
				privateBefore := ownedDurableSnapshot(t, s.pool)
				res, raw = s.request("POST", consoleAPIKeysPath, `{"name":"must-not-issue"}`, headers)
				if res.StatusCode != 403 {
					t.Errorf("private ambient assertion origin=%d %s, want 403", res.StatusCode, raw)
				}
				if ownedDurableSnapshot(t, s.pool) != privateBefore {
					t.Error("refused private origin changed keys/principals")
				}
			})
		}
	}
}

// The origin policy belongs to the selected human lane, not every request that
// happens to carry an assertion. A single empty key is not an offered key.
func TestNativeProxyPublicCredentialPrecedence(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, raw := s.request("POST", "/api/auth/login", "", h)
	cookie, _ := sessionReply(t, res, raw)
	if _, err := s.pool.Exec(context.Background(), `UPDATE principals SET role='developer'`); err != nil {
		t.Fatal(err)
	}
	owner := ownedPrincipalFixture(t, s.pool, "user", "developer")
	owned, _ := ownedKeyFixture(t, s.pool, owner, owner)
	_, envID, _, _, worker := storeWorker(t, s.tserver, "proxy-origin")
	_, _, workToken := pollItem(t, s.tserver, envID, worker)
	if workToken == "" {
		t.Fatal("poll did not issue a sessions token")
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"same-origin", http.Header{"Origin": {nativeOrigin}, "Sec-Fetch-Site": {"same-origin"}}, 401},
		{"signalless", http.Header{}, 401},
		{"owned-bearer", http.Header{"Origin": {"https://foreign.example"}, "Authorization": {"Bearer " + owned}}, 200},
		{"valid-key", http.Header{"Origin": {"https://foreign.example"}, "X-Api-Key": {testKey}}, 200},
		{"invalid-key", http.Header{"Origin": {"https://foreign.example"}, "X-Api-Key": {"invalid-key"}}, 401},
		{"repeated-key", http.Header{"Origin": {"https://foreign.example"}, "X-Api-Key": {testKey, testKey}}, 401},
		{"empty-then-valid-key", http.Header{"Origin": {"https://foreign.example"}, "X-Api-Key": {"", testKey}}, 401},
		{"single-empty-key", http.Header{"Origin": {"https://foreign.example"}, "X-Api-Key": {""}}, 401},
		{"single-empty-key-same-origin", http.Header{"Origin": {nativeOrigin}, "X-Api-Key": {""}}, 401},
		{"inadmissible-worker", http.Header{"Origin": {"https://foreign.example"}, "Authorization": {"Bearer " + worker}}, 401},
		{"inadmissible-worker-signalless", http.Header{"Authorization": {"Bearer " + worker}}, 401},
		{"inadmissible-invalid-worker", http.Header{"Origin": {"https://foreign.example"}, "Authorization": {"Bearer sk-map-env01-invalid"}}, 401},
		{"inadmissible-work-token", http.Header{"Origin": {"https://foreign.example"}, "Authorization": {"Bearer " + workToken}}, 401},
		{"cookie-only", http.Header{"Origin": {nativeOrigin}, "Cookie": {cookie}}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := tc.headers.Clone()
			headers.Set("Content-Type", "text/plain")
			if tc.name != "cookie-only" {
				headers.Set("X-Goog-IAP-JWT-Assertion", token)
			}
			before := agentRowCounts(t, s.tserver)
			res, raw := s.request("POST", "/v1/agents", proxyOriginAgentBody, headers)
			if res.StatusCode != tc.want {
				t.Errorf("credential precedence=%d %s, want %d", res.StatusCode, raw, tc.want)
			}
			want := before
			if tc.want == 200 {
				want[0]++
				want[1]++
			}
			if after := agentRowCounts(t, s.tserver); after != want {
				t.Errorf("agent/version rows=%v, want %v", after, want)
			}
		})
	}
}

func TestNativeProxyOriginAdmittedWorkerPrecedence(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	token := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	_, envID, sessionID, _, worker := storeWorker(t, s.tserver, "proxy-workers")
	_, _, workToken := pollItem(t, s.tserver, envID, worker)
	if workToken == "" {
		t.Fatal("poll did not issue a sessions token")
	}
	body, err := json.Marshal(map[string]any{"events": []any{userMessage("machine lane with a foreign Origin")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bearer string
		keys         []string
		want         int
	}{
		{"environment-worker", worker, nil, 200},
		{"environment-worker-empty-key", worker, []string{""}, 200},
		{"invalid-environment-worker", "sk-map-env01-invalid", nil, 401},
		{"worker-invalid-management-key", worker, []string{"invalid-key"}, 401},
		{"work-token", workToken, nil, 200},
		{"work-token-empty-key", workToken, []string{""}, 200},
		{"work-token-invalid-management-key", workToken, []string{"invalid-key"}, 401},
		{"invalid-work-token", "wtk_invalid", nil, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Content-Type": {"text/plain"}, "Origin": {"https://foreign.example"}, "Sec-Fetch-Site": {"cross-site"},
				"Authorization": {"Bearer " + tc.bearer}, "X-Goog-Iap-Jwt-Assertion": {token}}
			if tc.keys != nil {
				headers["X-Api-Key"] = tc.keys
			}
			var before, after int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id=$1 AND type='user.message'`, sessionID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			res, raw := s.request("POST", "/v1/sessions/"+sessionID+"/events", string(body), headers)
			if res.StatusCode != tc.want {
				t.Errorf("worker precedence=%d %s, want %d", res.StatusCode, raw, tc.want)
			}
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE session_id=$1 AND type='user.message'`, sessionID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			want := before
			if tc.want == 200 {
				want++
			}
			if after != want {
				t.Errorf("worker mutation wrote %d events, want %d", after-before, want-before)
			}
		})
	}
}

func TestNativeDisabledProxyOriginLegacyCompatibility(t *testing.T) {
	s := newLaneServerWith(t, func(cfg *identity.Config) {
		cfg.Mode = identity.ModeTrustedProxy
		cfg.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
	})
	before := agentRowCounts(t, s.tserver)
	res := s.doRaw("POST", "/v1/agents", proxyOriginAgentBody, map[string]string{
		"Content-Type": "text/plain", "Origin": "https://foreign.example", "Sec-Fetch-Site": "cross-site",
		"X-Goog-IAP-JWT-Assertion": s.token("platform-devs"),
	})
	code, _, raw := laneRead(t, res)
	if code != 401 {
		t.Errorf("native-disabled legacy assertion=%d %s", code, raw)
	}
	if after := agentRowCounts(t, s.tserver); after != before {
		t.Errorf("public assertion changed agent/version rows: before=%v after=%v", before, after)
	}
	// Native-disabled legacy private assertions retain their old origin policy.
	assertOwnedResponse(t, s.doRaw("GET", consoleAPIKeysPath, nil, map[string]string{
		"Origin": "https://foreign.example", "Sec-Fetch-Site": "cross-site",
		"X-Goog-IAP-JWT-Assertion": s.token("platform-admins"),
	}), 200)
}
