package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

func nativeCallbackFixture(t *testing.T, s *nativeServer, observe func(*http.Request)) (string, http.Header) {
	t.Helper()
	res, _ := s.request("GET", "/api/auth/login", "", nil)
	u, err := url.Parse(res.Header.Get("Location"))
	if err != nil || res.StatusCode != 303 {
		t.Fatalf("callback start=%d err=%v", res.StatusCode, err)
	}
	q := u.Query()
	var binding string
	for _, cookie := range res.Cookies() {
		if cookie.Name == "map_login" {
			binding = cookie.Name + "=" + cookie.Value
		}
	}
	if binding == "" {
		t.Fatal("no pending browser binding")
	}
	s.idp.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		if r.ParseForm() != nil {
			http.Error(w, "invalid form", 400)
			return
		}
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("code") != "fixture-code" || base64.RawURLEncoding.EncodeToString(challenge[:]) != q.Get("code_challenge") || r.Form.Get("redirect_uri") != nativeOrigin+"/api/auth/callback" {
			http.Error(w, "invalid code flow", 400)
			return
		}
		claims := s.idp.Claims(laneAudience, time.Now())
		claims["nonce"] = q.Get("nonce")
		json.NewEncoder(w).Encode(map[string]string{"id_token": s.idp.Mint(t, claims)})
	})
	return "/api/auth/callback?state=" + url.QueryEscape(q.Get("state")) + "&code=fixture-code", http.Header{"Cookie": {binding}}
}

func TestNativeOIDCTraceContext(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(old); provider.Shutdown(context.Background()) })
	s := newNativeServer(t, login.ModeOIDC)
	headers := make(chan http.Header, 1)
	path, h := nativeCallbackFixture(t, s, func(r *http.Request) { headers <- r.Header.Clone() })
	const incoming = "00-0102030405060708090a0b0c0d0e0f10-1112131415161718-01"
	h.Set("traceparent", incoming)
	h.Set("tracestate", "caller=fixture")
	res, raw := s.request("GET", path, "", h)
	if res.StatusCode != 303 {
		t.Fatalf("traced callback=%d %s", res.StatusCode, raw)
	}
	outgoing := <-headers
	sc := trace.SpanContextFromContext(telemetry.Extract(context.Background(), map[string]string{"traceparent": outgoing.Get("traceparent"), "tracestate": outgoing.Get("tracestate")}))
	var server trace.SpanContext
	for _, span := range recorder.Ended() {
		if span.Name() == "GET /api/auth/callback" && span.SpanKind() == trace.SpanKindServer {
			server = span.SpanContext()
		}
	}
	if !server.IsValid() || !sc.IsValid() || sc.TraceID() != server.TraceID() || sc.SpanID() != server.SpanID() || sc.TraceID().String() != "0102030405060708090a0b0c0d0e0f10" || sc.SpanID().String() == "1112131415161718" || !sc.IsSampled() || sc.TraceState().String() != "caller=fixture" {
		t.Fatalf("IdP did not receive the actual server span: outbound=%q/%q server=%v", outgoing.Get("traceparent"), outgoing.Get("tracestate"), server)
	}
}

func TestNativeUncanonicalOriginConfig(t *testing.T) {
	for _, origin := range []string{"http://localhost:80", "https://CONSOLE.example:443"} {
		t.Run(origin, func(t *testing.T) {
			pool := newPoolWithKey(t)
			manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: origin, Development: true})
			srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil, api.WithLogin(manager, nil)))
			defer srv.Close()
			s := &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool}, manager: manager}
			res, raw := s.request("GET", "/api/auth/config", "", nil)
			if res.StatusCode != 200 || !strings.Contains(string(raw), `"native_login":false`) {
				t.Fatalf("unusable origin advertised native login: status=%d body=%s", res.StatusCode, raw)
			}
		})
	}
}

func TestNativeBadPasswordDoesNotSignOut(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	s.seedLocal("existing", "user", "active")
	res, raw := s.request("POST", "/api/auth/login", `{"login_name":"existing","password":"incorrect-fixture-password"}`, nativeHeaders("", ""))
	if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "" || len(res.Cookies()) != 0 {
		t.Fatalf("bad password changed browser state: %d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
	}
}

func TestNativeDeactivatedLoginMarker(t *testing.T) {
	for _, mode := range []login.Mode{login.ModePassword, login.ModeOIDC, login.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newNativeServer(t, mode)
			method, path, body, h := "POST", "/api/auth/login", "", nativeHeaders("", "")
			if mode == login.ModePassword {
				s.seedLocal("removed", "user", "deactivated")
				body = fmt.Sprintf(`{"login_name":"removed","password":%q}`, localPassword)
			} else {
				_, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,status) VALUES ('principal_removed',$1,'user-1','deactivated')`, s.idp.Issuer())
				if err != nil {
					t.Fatal(err)
				}
				if mode == login.ModeOIDC {
					method = "GET"
					path, h = nativeCallbackFixture(t, s, nil)
				} else {
					h.Set("X-Goog-IAP-JWT-Assertion", s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now())))
				}
			}
			res, raw := s.request(method, path, body, h)
			if res.StatusCode != 403 || res.Header.Get("x-platform-signed-out") != "1" {
				t.Fatalf("removed login status=%d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
			}
			for _, cookie := range res.Cookies() {
				if cookie.Name == "map_session" {
					t.Fatal("removed login set a session cookie")
				}
			}
			var sessions int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM browser_sessions`).Scan(&sessions); err != nil || sessions != 0 {
				t.Fatalf("removed login stored %d sessions: %v", sessions, err)
			}
		})
	}
}

// The barrier is the real session-table lock. The test observes Issue waiting
// before deactivating, rather than racing a sleep against Argon2 or HTTP.
func TestNativeLoginIssuanceDeactivationRace(t *testing.T) {
	for _, mode := range []login.Mode{login.ModePassword, login.ModeOIDC, login.ModeTrustedProxy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newNativeServer(t, mode)
			method, path, body, h := "POST", "/api/auth/login", "", nativeHeaders("", "")
			if mode == login.ModePassword {
				s.seedLocal("raced", "user", "active")
				body = fmt.Sprintf(`{"login_name":"raced","password":%q}`, localPassword)
			} else if mode == login.ModeOIDC {
				method = "GET"
				path, h = nativeCallbackFixture(t, s, nil)
			} else {
				h.Set("X-Goog-IAP-JWT-Assertion", s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now())))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			blocker, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `LOCK TABLE browser_sessions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			type result struct {
				res *http.Response
				raw []byte
				err error
			}
			done := make(chan result, 1)
			go func() {
				request, err := http.NewRequestWithContext(ctx, method, s.url+path, strings.NewReader(body))
				if err != nil {
					done <- result{err: err}
					return
				}
				request.Header = h.Clone()
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				response, err := client.Do(request)
				if err != nil {
					done <- result{err: err}
					return
				}
				raw, err := io.ReadAll(response.Body)
				response.Body.Close()
				done <- result{response, raw, err}
			}()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			waiting := false
			for !waiting {
				if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND relation='browser_sessions'::regclass AND NOT granted)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case early := <-done:
					t.Fatalf("login finished before observed issuance wait: %v status=%v body=%s", early.err, early.res, early.raw)
				case <-ctx.Done():
					t.Fatal("did not observe issuance wait")
				case <-ticker.C:
				}
			}
			changed, err := s.pool.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE kind='user' AND status='active'`)
			if err != nil || changed.RowsAffected() != 1 {
				t.Fatalf("barrier deactivated %d principals: %v", changed.RowsAffected(), err)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			reply := <-done
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			if reply.res.StatusCode != 401 || reply.res.Header.Get("x-platform-signed-out") != "1" {
				t.Fatalf("issuance race status=%d marker=%q body=%s", reply.res.StatusCode, reply.res.Header.Get("x-platform-signed-out"), reply.raw)
			}
			for _, cookie := range reply.res.Cookies() {
				if cookie.Name == "map_session" {
					t.Fatal("raced issuance set a session cookie")
				}
			}
			var sessions int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM browser_sessions`).Scan(&sessions); err != nil || sessions != 0 {
				t.Fatalf("raced issuance stored %d sessions: %v", sessions, err)
			}
		})
	}
}

// This is the approved parser lifecycle, not a failure that needs fixing.
// Ambiguous queries clear the browser binding but leave the server attempt.
func TestNativeCallbackQueryValidationRetainsAttempt(t *testing.T) {
	for _, suffix := range []string{"&code=duplicate", "&unrelated=%ZZ", "&state=duplicate", "&error=one&error=two"} {
		t.Run(suffix, func(t *testing.T) {
			s := newNativeServer(t, login.ModeOIDC)
			path, h := nativeCallbackFixture(t, s, nil)
			res, raw := s.request("GET", path+suffix, "", h)
			if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "" {
				t.Fatalf("ambiguous callback=%d marker=%q body=%s", res.StatusCode, res.Header.Get("x-platform-signed-out"), raw)
			}
			cleared := false
			for _, cookie := range res.Cookies() {
				if cookie.Name == "map_session" {
					t.Fatal("ambiguous callback issued session")
				}
				if cookie.Name == "map_login" && cookie.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("invalid callback did not clear pending browser cookie")
			}
			var attempts, sessions int
			if err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM browser_login_attempts),(SELECT count(*) FROM browser_sessions)`).Scan(&attempts, &sessions); err != nil || attempts != 1 || sessions != 0 {
				t.Fatalf("parser refusal changed durable lifecycle: attempts=%d sessions=%d err=%v", attempts, sessions, err)
			}
			// Deliberately restore the original binding; this is not what a browser
			// does after accepting the clearing Set-Cookie response.
			res, raw = s.request("GET", path, "", h)
			if res.StatusCode != 303 || res.Header.Get("Location") != "/" {
				t.Fatalf("retained-binding retry=%d %s", res.StatusCode, raw)
			}
			var issued bool
			for _, cookie := range res.Cookies() {
				if cookie.Name == "map_session" {
					issued = true
				}
			}
			if !issued {
				t.Fatal("valid retained-binding retry did not issue session")
			}
		})
	}
}
