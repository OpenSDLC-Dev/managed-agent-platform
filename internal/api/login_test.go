package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity/identitytest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

const nativeOrigin = "https://console.example"
const setupPath = "/api/console/organizations/default/setup_code/redeem"
const localPassword = "test-only-password-123"

type nativeServer struct {
	*tserver
	manager *login.Manager
	idp     *identitytest.IdP
}

func newNativeServer(t *testing.T, mode login.Mode) *nativeServer {
	t.Helper()
	pool := newPoolWithKey(t)
	m := login.New(pool, login.Config{Mode: mode, PublicOrigin: nativeOrigin, Development: true})
	var v *identity.Verifier
	var rp *identity.RelyingParty
	var idp *identitytest.IdP
	if mode == login.ModeOIDC || mode == login.ModeTrustedProxy {
		idp = identitytest.NewIdP(t)
		cfg := identity.Config{Mode: identity.Mode(mode), Issuer: idp.Issuer(), Audience: laneAudience, JWKSURL: idp.JWKSURL(), HTTPClient: idp.Client()}
		if mode == login.ModeTrustedProxy {
			cfg.AssertionHeader = "X-Goog-IAP-JWT-Assertion"
		}
		var err error
		v, err = identity.New(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if mode == login.ModeOIDC {
			rp, err = identity.NewRelyingParty(context.Background(), cfg, laneAudience, "fixture-client-secret", nativeOrigin+"/api/auth/callback")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, v, api.WithLogin(m, rp)))
	t.Cleanup(srv.Close)
	return &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool}, manager: m, idp: idp}
}

func (s *nativeServer) request(method, path, body string, headers http.Header) (*http.Response, []byte) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.url+path, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		s.t.Fatal(err)
	}
	return res, raw
}

func nativeHeaders(cookie, csrf string) http.Header {
	h := http.Header{"Origin": {nativeOrigin}, "Content-Type": {"application/json"}}
	if cookie != "" {
		h.Set("Cookie", cookie)
	}
	if csrf != "" {
		h.Set("X-CSRF-Token", csrf)
	}
	return h
}

func sessionReply(t *testing.T, res *http.Response, raw []byte) (string, login.Session) {
	t.Helper()
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, raw)
	}
	var session login.Session
	if err := json.Unmarshal(raw, &session); err != nil || !session.SignedIn || !strings.HasPrefix(session.ID, "user_") || session.CSRFToken == "" {
		t.Fatalf("invalid session DTO=%s err=%v", raw, err)
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == "map_session" {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 43200 {
				t.Fatalf("cookie attributes=%+v", c)
			}
			if strings.Contains(string(raw), c.Value) {
				t.Fatal("raw session cookie in DTO")
			}
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie")
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" {
		t.Fatal("auth response is cacheable")
	}
	return cookie, session
}

func (s *nativeServer) seedLocal(name, role, status string) string {
	s.t.Helper()
	id := domain.NewID(domain.PrefixPrincipal).String()
	hash, err := login.HashPassword(localPassword)
	if err != nil {
		s.t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role,status) VALUES ($1,$2,$3,$4,$5);`, id, login.LocalIssuer, name, role, status)
	if err != nil {
		s.t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO local_accounts(principal_id,password_hash) VALUES ($1,$2)`, id, hash)
	if err != nil {
		s.t.Fatal(err)
	}
	return id
}

func (s *nativeServer) issue(id string) (string, login.Session) {
	s.t.Helper()
	issued, err := s.manager.Issue(context.Background(), id)
	if err != nil {
		s.t.Fatal(err)
	}
	return "map_session=" + issued.Token, issued.Session
}

// Missing config admission or an auth-subtree bypass must fail this matrix.
func TestNativeLoginRouteMatrix(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		method, path string
		key          bool
		want         int
	}{
		{"GET", "/api/auth/config", false, 200},
		{"GET", "/api/auth/session", true, 401},
		{"GET", "/api/%61uth/session", true, 401},
		{"POST", "/api/auth/logout", true, 401},
		{"GET", "/api/auth/unknown", false, 401},
		{"POST", "/api/auth/config", false, 401},
		{"GET", "/api/auth/login", false, 401},
		{"GET", "/api/auth/callback", false, 401},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			headers := map[string]string{}
			if tc.key {
				headers["x-api-key"] = testKey
			}
			res := s.doRaw(tc.method, tc.path, nil, headers)
			defer res.Body.Close()
			raw, _ := io.ReadAll(res.Body)
			if res.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.want, raw)
			}
			if tc.path == "/api/auth/config" && tc.method == "GET" {
				var cfg map[string]any
				if json.Unmarshal(raw, &cfg) != nil || cfg["mode"] != "disabled" || cfg["native_login"] != false || len(cfg) != 2 {
					t.Fatalf("config=%s", raw)
				}
				if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" {
					t.Fatal("auth config is cacheable")
				}
			}
		})
	}
	for _, mode := range []login.Mode{login.ModePassword, login.ModeOIDC, login.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newNativeServer(t, mode)
			res, raw := s.request("GET", "/api/auth/config", "", nil)
			var cfg map[string]any
			if res.StatusCode != 200 || json.Unmarshal(raw, &cfg) != nil || cfg["mode"] != string(mode) || cfg["native_login"] != true || len(cfg) != 2 {
				t.Fatalf("config status=%d body=%s", res.StatusCode, raw)
			}
			id := s.seedLocal("matrix", "admin", "active")
			cookie, session := s.issue(id)
			for _, tc := range []struct {
				method, path string
				headers      http.Header
				want         int
			}{
				{"GET", "/api/auth/session", nativeHeaders(cookie, ""), 200},
				{"GET", "/api/%61uth/session", nativeHeaders(cookie, ""), 200},
				{"GET", "/%61pi/auth/session", nativeHeaders(cookie, ""), 200},
				{"GET", "/api/auth/session", http.Header{"X-Api-Key": {testKey}}, 401},
				{"GET", "/api/%61uth/session", http.Header{"X-Api-Key": {testKey}}, 401},
				{"GET", "/api/auth/session", http.Header{"Cookie": {cookie}, "X-Api-Key": {testKey}}, 401},
				{"GET", "/api/auth/session", http.Header{"Cookie": {cookie}, "Authorization": {"Bearer junk"}}, 401},
				{"GET", "/api/auth/session", http.Header{"Cookie": {cookie + "; " + cookie}}, 401},
				{"POST", "/api/auth/logout", nativeHeaders(cookie, ""), 403},
				{"POST", "/api/auth/logout", http.Header{"X-Api-Key": {testKey}}, 401},
				{"GET", "/api/auth/unknown", nil, 401},
				{"GET", "/api/auth/unknown", nativeHeaders(cookie, ""), 404},
				{"POST", "/api/auth/config", nil, 401},
				{"GET", "/v1/agents", nativeHeaders(cookie, ""), 401},
				{"GET", "/%761/agents", nativeHeaders(cookie, ""), 401},
				{"GET", "/api/../v1/agents", nativeHeaders(cookie, ""), 401},
				{"GET", "/api/%2e%2e/v1/agents", nativeHeaders(cookie, ""), 401},
				{"GET", "/api//auth/session", nativeHeaders(cookie, ""), 401},
				{"GET", "/api/console/organizations/default/workspaces/default/api_keys", nativeHeaders(cookie, ""), 200},
				{"GET", "/api/console/organizations/default/workspaces/default/api_keys", http.Header{"Cookie": {cookie}, "X-Api-Key": {"bad"}}, 401},
				{"GET", "/api/console/organizations/default/workspaces/default/api_keys", http.Header{"Cookie": {cookie}, "Authorization": {"Bearer junk"}}, 401},
				{"POST", "/api/console/organizations/default/workspaces/default/api_keys", nativeHeaders(cookie, ""), 403},
			} {
				t.Run(tc.method+tc.path+fmt.Sprint(tc.want), func(t *testing.T) {
					res, raw := s.request(tc.method, tc.path, "{}", tc.headers)
					if res.StatusCode != tc.want {
						t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.want, raw)
					}
				})
			}
			res, raw = s.request("POST", "/api/auth/logout", "", nativeHeaders(cookie, session.CSRFToken))
			if res.StatusCode != 200 || strings.TrimSpace(string(raw)) != `{"ok":true}` {
				t.Fatalf("logout=%d %s", res.StatusCode, raw)
			}
			cleared := false
			for _, c := range res.Cookies() {
				if c.Name == "map_session" && c.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("logout did not expire cookie")
			}
			res, _ = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
			if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "1" {
				t.Fatalf("revoked session=%d marker=%q", res.StatusCode, res.Header.Get("x-platform-signed-out"))
			}
		})
	}
}

func TestNativePasswordRequiresDevelopment(t *testing.T) {
	pool := newPoolWithKey(t)
	manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin})
	srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil, api.WithLogin(manager, nil)))
	defer srv.Close()
	s := &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool}, manager: manager}
	res, raw := s.request("GET", "/api/auth/config", "", nil)
	if res.StatusCode != 200 || !strings.Contains(string(raw), `"native_login":false`) {
		t.Fatalf("undeclared development exposed native password login: %d %s", res.StatusCode, raw)
	}
	res, raw = s.request("POST", "/api/auth/login", `{"login_name":"ada","password":"`+localPassword+`"}`, nativeHeaders("", ""))
	if res.StatusCode != 401 {
		t.Fatalf("undeclared development admitted password POST: %d %s", res.StatusCode, raw)
	}
}

func TestNativePasswordLogin(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	id := s.seedLocal("ada", "user", "active")
	body := `{"login_name":"ada","password":"` + localPassword + `"}`
	res, raw := s.request("POST", "/api/auth/login", body, nativeHeaders("", ""))
	cookie, session := sessionReply(t, res, raw)
	if session.LoginName != "ada" || session.Role != identity.RoleUser {
		t.Fatalf("session=%+v", session)
	}
	res, _ = s.request("GET", "/api/console/organizations/default/workspaces/default/api_keys", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "" {
		t.Fatalf("user resource=%d", res.StatusCode)
	}
	_, err := s.pool.Exec(context.Background(), `UPDATE principals SET role='admin' WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	res, raw = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	var current login.Session
	json.Unmarshal(raw, &current)
	if res.StatusCode != 200 || current.Role != identity.RoleAdmin || len(res.Cookies()) != 0 {
		t.Fatalf("same-cookie promotion=%d %s", res.StatusCode, raw)
	}
	_, err = s.pool.Exec(context.Background(), `UPDATE principals SET status='deactivated' WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	res, _ = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "1" {
		t.Fatalf("deactivation=%d", res.StatusCode)
	}
}

func TestNativePasswordValidationAndThrottle(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	for _, tc := range []struct {
		name, body string
		headers    http.Header
		want       int
	}{
		{"missing origin", `{}`, http.Header{"Content-Type": {"application/json"}}, 403},
		{"wrong origin", `{}`, http.Header{"Origin": {"https://evil.example"}, "Content-Type": {"application/json"}}, 403},
		{"non JSON", `{}`, http.Header{"Origin": {nativeOrigin}, "Content-Type": {"text/plain"}}, 400},
		{"duplicate content type", `{}`, http.Header{"Origin": {nativeOrigin}, "Content-Type": {"application/json", "application/json"}}, 400},
		{"cross site", `{}`, http.Header{"Origin": {nativeOrigin}, "Content-Type": {"application/json"}, "Sec-Fetch-Site": {"cross-site"}}, 403},
		{"uppercase name", `{"login_name":"Ada","password":"` + localPassword + `"}`, nativeHeaders("", ""), 400},
		{"short password", `{"login_name":"ada","password":"short"}`, nativeHeaders("", ""), 400},
		{"array", `[]`, nativeHeaders("", ""), 400},
		{"oversize", `{"pad":"` + strings.Repeat("x", (4<<20)+1) + `"}`, nativeHeaders("", ""), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, raw := s.request("POST", "/api/auth/login", tc.body, tc.headers)
			if res.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.want, raw)
			}
		})
	}
	for i := 0; i < 6; i++ {
		res, raw := s.request("POST", "/api/auth/login", `{"login_name":"unknown","password":"`+localPassword+`"}`, nativeHeaders("", ""))
		want := 401
		if i == 5 {
			want = 429
		}
		if res.StatusCode != want {
			t.Fatalf("attempt %d status=%d want=%d body=%s", i+1, res.StatusCode, want, raw)
		}
		if i == 5 && res.Header.Get("Retry-After") == "" {
			t.Fatal("throttle has no retry bound")
		}
	}
}

func TestCookieLookupDoesNotApplyMapping(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	_, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,email,display_name,role,role_source,last_seen_at)
 VALUES ('principal_cookie',$1,'person','ada@example.test','Ada','developer','idp','2020-01-02T03:04:05Z')`, s.idp.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := s.issue("principal_cookie")
	res, raw := s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 200 {
		t.Fatalf("session=%d %s", res.StatusCode, raw)
	}
	var role, source, email, name string
	var seen time.Time
	err = s.pool.QueryRow(context.Background(), `SELECT role,role_source,email,display_name,last_seen_at FROM principals WHERE id='principal_cookie'`).Scan(&role, &source, &email, &name, &seen)
	if err != nil || role != "developer" || source != "idp" || email != "ada@example.test" || name != "Ada" || seen.UTC().Format(time.RFC3339) != "2020-01-02T03:04:05Z" {
		t.Fatalf("cookie changed IdP state: %s %s %s %s %s err=%v", role, source, email, name, seen, err)
	}
}

func TestNativeOIDCLogin(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	res, _ := s.request("GET", "/api/auth/login?return_to=https://evil.example", "", nil)
	if res.StatusCode != 303 {
		t.Fatalf("start=%d", res.StatusCode)
	}
	location, err := url.Parse(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := location.Query()
	var binding string
	for _, c := range res.Cookies() {
		if c.Name == "map_login" {
			binding = c.Name + "=" + c.Value
		}
	}
	if binding == "" || q.Get("state") == "" || q.Get("nonce") == "" {
		t.Fatal("start missing browser-bound state")
	}
	s.idp.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "fixture-auth-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || r.Form.Get("redirect_uri") != nativeOrigin+"/api/auth/callback" {
			http.Error(w, "bad PKCE", 400)
			return
		}
		claims := s.idp.Claims(laneAudience, time.Now())
		claims["nonce"] = q.Get("nonce")
		claims["email"] = "ada@example.test"
		json.NewEncoder(w).Encode(map[string]any{"id_token": s.idp.Mint(t, claims)})
	})
	callback := "/api/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=fixture-auth-code"
	bad, _ := s.request("GET", callback, "", nil)
	if bad.StatusCode != 401 {
		t.Fatalf("callback without binding=%d", bad.StatusCode)
	}
	res, raw := s.request("GET", callback, "", http.Header{"Cookie": {binding}})
	if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
		t.Fatalf("callback=%d %s location=%s", res.StatusCode, raw, res.Header.Get("Location"))
	}
	var cookie string
	for _, c := range res.Cookies() {
		if c.Name == "map_session" {
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatal("callback no session")
	}
	res, raw = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 200 || !strings.Contains(string(raw), "ada@example.test") {
		t.Fatalf("OIDC session=%d %s", res.StatusCode, raw)
	}
	replay, _ := s.request("GET", callback, "", http.Header{"Cookie": {binding}})
	if replay.StatusCode != 401 {
		t.Fatalf("replay=%d", replay.StatusCode)
	}
	for _, secret := range []string{"fixture-auth-code", "fixture-client-secret", strings.TrimPrefix(cookie, "map_session="), q.Get("state"), q.Get("nonce")} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("credential in logs")
		}
	}
}

func TestNativeIAPLogin(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	claims := s.idp.Claims(laneAudience, time.Now())
	claims["email"] = "iap@example.test"
	token := s.idp.Mint(t, claims)
	h := nativeHeaders("", "")
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, raw := s.request("POST", "/api/auth/login", "", h)
	cookie, session := sessionReply(t, res, raw)
	if session.Email != "iap@example.test" {
		t.Fatalf("IAP session=%s", raw)
	}
	for _, path := range []string{"/api/auth/session", "/api/%61uth/session", setupPath} {
		method := "GET"
		body := ""
		if path == setupPath {
			method = "POST"
			body = `{"code":"none"}`
		}
		res, _ = s.request(method, path, body, h)
		if res.StatusCode != 401 {
			t.Fatalf("assertion without cookie on %s=%d", path, res.StatusCode)
		}
	}
	h = nativeHeaders(cookie, session.CSRFToken)
	h.Set("X-Goog-IAP-JWT-Assertion", token)
	res, _ = s.request("GET", "/api/auth/session", "", h)
	if res.StatusCode != 200 {
		t.Fatalf("IAP edge plus cookie=%d", res.StatusCode)
	}
}

func TestNativeSetupAdmission(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	code, err := api.MintSetupCode(context.Background(), s.pool, false)
	if err != nil {
		t.Fatal(err)
	}
	claims := s.idp.Claims(laneAudience, time.Now())
	token := s.idp.Mint(t, claims)
	for _, path := range []string{setupPath, "/api/%63onsole/organizations/default/setup_code/redeem"} {
		for _, h := range []http.Header{{"X-Api-Key": {testKey}}, {"Authorization": {"Bearer " + token}}} {
			h.Set("Origin", nativeOrigin)
			h.Set("Content-Type", "application/json")
			res, _ := s.request("POST", path, `{"code":"`+code+`"}`, h)
			if res.StatusCode != 401 {
				t.Fatalf("non-cookie setup %s=%d", path, res.StatusCode)
			}
		}
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject) VALUES ('principal_setup',$1,'setup')`, s.idp.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	cookie, session := s.issue("principal_setup")
	res, _ := s.request("POST", setupPath, `{"code":"`+code+`"}`, nativeHeaders(cookie, ""))
	if res.StatusCode != 403 {
		t.Fatalf("setup without CSRF=%d", res.StatusCode)
	}
	res, raw := s.request("POST", setupPath, `{"code":"`+code+`"}`, nativeHeaders(cookie, session.CSRFToken))
	if res.StatusCode != 200 || !strings.Contains(string(raw), `"id":"user_setup"`) || !strings.Contains(string(raw), `"role":"admin"`) || len(res.Cookies()) != 0 {
		t.Fatalf("native promotion=%d %s", res.StatusCode, raw)
	}
	res, raw = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 200 || !strings.Contains(string(raw), `"role":"admin"`) {
		t.Fatalf("same-cookie role=%d %s", res.StatusCode, raw)
	}
}

func TestPasswordSetupCreatesOnlyOneAdmin(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	codes := make([]string, 2)
	for i := range codes {
		var err error
		codes[i], err = api.MintPasswordSetupCode(context.Background(), s.pool)
		if err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		res *http.Response
		raw []byte
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, code := range codes {
		wg.Add(1)
		go func(i int, code string) {
			defer wg.Done()
			<-start
			res, raw := s.request("POST", setupPath, fmt.Sprintf(`{"code":%q,"login_name":"admin%d","password":%q}`, code, i, localPassword), nativeHeaders("", ""))
			results <- result{res, raw}
		}(i, code)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for reply := range results {
		if reply.res.StatusCode == 200 {
			_, session := sessionReply(t, reply.res, reply.raw)
			if session.Role != identity.RoleAdmin {
				t.Fatal("setup did not create admin")
			}
			success++
		} else if reply.res.StatusCode != 409 {
			t.Fatalf("race loser=%d %s", reply.res.StatusCode, reply.raw)
		}
	}
	var admins, accounts, left int
	err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM principals WHERE role='admin'),(SELECT count(*) FROM local_accounts),(SELECT count(*) FROM setup_codes)`).Scan(&admins, &accounts, &left)
	if err != nil || success != 1 || admins != 1 || accounts != 1 || left != 0 {
		t.Fatalf("success=%d admins=%d accounts=%d codes=%d err=%v", success, admins, accounts, left, err)
	}
}

func TestPasswordSetupAfterOtherMode(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	old, err := api.MintSetupCode(context.Background(), s.pool, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_external','https://idp.example','external','admin')`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := api.MintPasswordSetupCode(context.Background(), s.pool)
	if err != nil || code == "" {
		t.Fatalf("mode switch mint=%q err=%v", code, err)
	}
	body := func(c string) string {
		return fmt.Sprintf(`{"code":%q,"login_name":"newadmin","password":%q}`, c, localPassword)
	}
	res, raw := s.request("POST", setupPath, body(old), nativeHeaders("", ""))
	if res.StatusCode != 400 {
		t.Fatalf("identity-purpose code accepted in password mode=%d %s", res.StatusCode, raw)
	}
	res, raw = s.request("POST", setupPath, body(code), nativeHeaders("", ""))
	_, session := sessionReply(t, res, raw)
	if session.Role != identity.RoleAdmin {
		t.Fatal("password setup role")
	}
}
