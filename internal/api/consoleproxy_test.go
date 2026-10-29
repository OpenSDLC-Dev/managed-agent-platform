package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

const privateProxy = "/api/organizations/default/proxy/v1"

func newPrivateServer(t *testing.T) *nativeServer {
	t.Helper()
	pool := newPoolWithKey(t)
	blobs := blobtest.Mem()
	manager := login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: nativeOrigin, Development: true})
	srv := httptest.NewServer(api.NewHandler(pool, blobs, nil, nil, api.WithLogin(manager, nil)))
	t.Cleanup(srv.Close)
	return &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool, blobs: blobs}, manager: manager}
}

// Missing cookie provenance, a weaker read floor, or re-parsing the body/query breaks this contract.
func TestPrivateResourceProxy(t *testing.T) {
	s := newPrivateServer(t)
	admin := seedMember(t, s, "admin", "admin", "console")
	developer := seedMember(t, s, "developer", "developer", "console")
	user := seedMember(t, s, "user", "user", "console")
	adminCookie, adminSession := s.issue(admin)
	devCookie, devSession := s.issue(developer)
	userCookie, _ := s.issue(user)

	t.Run("resource-role-floors", func(t *testing.T) {
		for _, resource := range []string{"agents", "environments", "sessions", "vaults", "skills", "files", "memory_stores", "dreams", "deployments", "deployment_runs"} {
			for _, caller := range []struct {
				name, cookie string
				want         int
			}{{"admin", adminCookie, 200}, {"developer", devCookie, 200}, {"user", userCookie, 403}} {
				t.Run(resource+"/"+caller.name, func(t *testing.T) {
					res, raw := s.request("GET", privateProxy+"/"+resource, "", nativeHeaders(caller.cookie, ""))
					if res.StatusCode != caller.want || res.Header.Get("x-platform-signed-out") != "" {
						t.Fatalf("status=%d want=%d body=%s marker=%q", res.StatusCode, caller.want, raw, res.Header.Get("x-platform-signed-out"))
					}
				})
			}
		}
		// Redaction keeps its admin floor. An absent store/version is irrelevant to a developer refusal.
		res, raw := s.request("POST", privateProxy+"/memory_stores/memstore_missing/memory_versions/memver_missing/redact", "{}", nativeHeaders(devCookie, devSession.CSRFToken))
		if res.StatusCode != 403 || !strings.Contains(string(raw), "admin role") {
			t.Fatalf("redaction floor=%d %s", res.StatusCode, raw)
		}
		// Key management is not relaxed as a side effect of resource forwarding.
		expectManagement(t, s, "GET", "/api/console/organizations/default/workspaces/default/api_keys", "", nativeHeaders(devCookie, ""), 403)
	})

	t.Run("cookie-only-including-method-fallback", func(t *testing.T) {
		for name, h := range map[string]http.Header{
			"none": nil, "key": {"X-Api-Key": {testKey}},
			"bearer":                     {"Authorization": {"Bearer fixture.token.value"}},
			"cookie-key":                 {"Cookie": {adminCookie}, "X-Api-Key": {testKey}},
			"cookie-empty-key":           {"Cookie": {adminCookie}, "X-Api-Key": {""}},
			"cookie-bearer":              {"Cookie": {adminCookie}, "Authorization": {"Bearer invalid"}},
			"cookie-empty-authorization": {"Cookie": {adminCookie}, "Authorization": {""}},
			"duplicate-cookie":           {"Cookie": {adminCookie + "; " + adminCookie}},
		} {
			for _, path := range []string{privateProxy + "/agents", "/%61pi/%6frganizations/%64efault/%70roxy/%761/%61gents"} {
				for _, method := range []string{"GET", "PUT"} {
					t.Run(name+method+path, func(t *testing.T) {
						res, raw := s.request(method, path, "{}", h)
						if res.StatusCode != 401 {
							t.Fatalf("non-cookie admission=%d %s", res.StatusCode, raw)
						}
					})
				}
			}
		}
		expectManagement(t, s, "GET", "/%61pi/%6frganizations/%64efault/%70roxy/%761/%61gents", "", nativeHeaders(devCookie, ""), 200)
		expectManagement(t, s, "PUT", privateProxy+"/agents", "{}", nativeHeaders(devCookie, devSession.CSRFToken), 405)
		expectManagement(t, s, "DELETE", privateProxy+"/agents/agent_missing", "", nativeHeaders(devCookie, devSession.CSRFToken), 404)
		expectManagement(t, s, "GET", "/api/organizations/11111111-1111-4111-8111-111111111111/proxy/v1/agents", "", nativeHeaders(adminCookie, ""), 401)
		expectManagement(t, s, "GET", "/api/organizations/foreign/proxy/v1/agents", "", nativeHeaders(adminCookie, ""), 400)
	})

	t.Run("csrf-body-query-and-actor", func(t *testing.T) {
		aid, eid := fixture(t, s.tserver)
		body := fmt.Sprintf("{\n \"agent\":%q, \"environment_id\":%q, \"title\":\"raw + %% body\", \"metadata\":{\"query\":\"a+b%%2Fc\"}}", aid, eid)
		for name, mutate := range map[string]func(http.Header){
			"no-origin":        func(h http.Header) { h.Del("Origin") },
			"alias-origin":     func(h http.Header) { h.Set("Origin", nativeOrigin+":443") },
			"foreign-origin":   func(h http.Header) { h.Set("Origin", "https://other.example") },
			"duplicate-origin": func(h http.Header) { h.Add("Origin", nativeOrigin) },
			"no-csrf":          func(h http.Header) { h.Del("X-CSRF-Token") },
			"wrong-csrf":       func(h http.Header) { h.Set("X-CSRF-Token", strings.Repeat("x", 43)) },
			"duplicate-csrf":   func(h http.Header) { h.Add("X-CSRF-Token", devSession.CSRFToken) },
			"cross-site":       func(h http.Header) { h.Set("Sec-Fetch-Site", "cross-site") },
		} {
			t.Run(name, func(t *testing.T) {
				h := nativeHeaders(devCookie, devSession.CSRFToken)
				mutate(h)
				expectManagement(t, s, "POST", privateProxy+"/sessions", body, h, 403)
			})
		}
		raw := expectManagement(t, s, "POST", privateProxy+"/sessions?beta=true&extra=a%2Bb+%2Fc", body, nativeHeaders(devCookie, devSession.CSRFToken), 200)
		var created struct{ ID string }
		if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
			t.Fatalf("session=%s err=%v", raw, err)
		}
		var actor, title, metadata string
		if err := s.pool.QueryRow(context.Background(), `SELECT created_by,title,metadata->>'query' FROM sessions WHERE id=$1`, created.ID).Scan(&actor, &title, &metadata); err != nil || actor != developer || title != "raw + % body" || metadata != "a+b%2Fc" {
			t.Fatalf("forwarded actor/body=%q/%q/%q err=%v", actor, title, metadata, err)
		}
		// A dropped or normalized query changes the resource handler's validation.
		expectManagement(t, s, "GET", privateProxy+"/agents?limit=0", "", nativeHeaders(devCookie, ""), 400)
		expectManagement(t, s, "GET", privateProxy+"/sessions/"+created.ID+"/events/stream?event_deltas%5B%5D=user.message", "", nativeHeaders(devCookie, ""), 400)
	})

	t.Run("multipart-and-binary-headers", func(t *testing.T) {
		content := "\x00\xff\r\nraw + % bytes"
		ct, body := fileForm(t, "raw.bin", nil, content)
		h := nativeHeaders(adminCookie, adminSession.CSRFToken)
		h.Set("Content-Type", ct)
		raw := expectManagement(t, s, "POST", privateProxy+"/files?beta=true", body, h, 200)
		var uploaded struct{ ID string }
		if err := json.Unmarshal(raw, &uploaded); err != nil || uploaded.ID == "" {
			t.Fatalf("upload=%s err=%v", raw, err)
		}
		stored, _, err := s.blobs.Get(context.Background(), "files/"+uploaded.ID)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		_, err = got.ReadFrom(stored)
		stored.Close()
		if err != nil || got.String() != content {
			t.Fatalf("multipart bytes=%q err=%v", got.String(), err)
		}
		expectManagement(t, s, "GET", privateProxy+"/files/"+uploaded.ID+"/content", "", nativeHeaders(adminCookie, ""), 400)
		// A tool-produced file is downloadable. The proxy must not switch to the worker bypass.
		id := domain.NewID(domain.PrefixFile).String()
		if err := s.blobs.Put(context.Background(), "files/"+id, strings.NewReader(content), int64(len(content)), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO files(id,filename,mime_type,size_bytes,downloadable) VALUES ($1,'raw.bin','application/octet-stream',$2,true)`, id, len(content)); err != nil {
			t.Fatal(err)
		}
		res, downloaded := s.request("GET", privateProxy+"/files/"+id+"/content", "", nativeHeaders(devCookie, ""))
		if res.StatusCode != 200 || string(downloaded) != content || res.Header.Get("Content-Type") != "application/octet-stream" || res.Header.Get("Content-Length") != fmt.Sprint(len(content)) || res.Header.Get("Content-Disposition") != `attachment; filename="raw.bin"` {
			t.Fatalf("download=%d %q headers=%v", res.StatusCode, downloaded, res.Header)
		}
	})

	t.Run("store-outage-is-not-sign-out", func(t *testing.T) {
		if _, err := s.pool.Exec(context.Background(), `ALTER TABLE browser_sessions RENAME TO unavailable_sessions`); err != nil {
			t.Fatal(err)
		}
		res, raw := s.request("GET", privateProxy+"/agents", "", nativeHeaders(adminCookie, ""))
		if res.StatusCode != 500 || res.Header.Get("x-platform-signed-out") != "" {
			t.Fatalf("store failure=%d %s marker=%q", res.StatusCode, raw, res.Header.Get("x-platform-signed-out"))
		}
		if _, err := s.pool.Exec(context.Background(), `ALTER TABLE unavailable_sessions RENAME TO browser_sessions`); err != nil {
			t.Fatal(err)
		}
	})
}

// A canonical private prefix cannot turn cookie authority into a machine credential.
func TestPrivateProxyCannotEnterMachineLanes(t *testing.T) {
	s := newPrivateServer(t)
	id := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(id)
	for _, path := range []string{
		"/environments/env_missing/work", "/environments/env_missing/work/poll",
		"/environments/env_missing/%77ork", "/environments/env_missing/%77ork/stats",
		"/messages", "/messages/count_tokens", "/chat/completions", "/responses", "/models", "/embeddings", "/rerank",
		"/internal/v1/gate/config", "/gate/config", "/api/oauth/organizations/default/environments/env_missing/tokens",
	} {
		t.Run("bounded"+path, func(t *testing.T) {
			res, raw := s.request("GET", privateProxy+path, "", nativeHeaders(cookie, ""))
			if res.StatusCode != 404 {
				t.Fatalf("non-resource target=%d %s; want bounded 404", res.StatusCode, raw)
			}
		})
	}
	for _, path := range []string{
		privateProxy + "/environments/env_missing%2fwork", privateProxy + "/environments/env_missing%2Fwork/poll",
		privateProxy + "/environments%2fenv_missing/work", privateProxy + "/agents/..", privateProxy + "/%2e%2e/agents",
		privateProxy + "/agents/./versions", privateProxy + "//agents", privateProxy + "/agents%2f", privateProxy + "/agents/%2e",
		"/api/organizations/default/proxy%2fv1/agents", "/api/organizations/default/proxy/v1%2fagents",
	} {
		t.Run("unambiguous"+path, func(t *testing.T) {
			for _, h := range []http.Header{nativeHeaders(cookie, ""), {"X-Api-Key": {testKey}}, {"Cookie": {cookie}, "X-Api-Key": {testKey}}} {
				res, raw := s.request("GET", path, "", h)
				if res.StatusCode < 400 || res.Header.Get("Location") != "" {
					t.Fatalf("ambiguous path admitted or redirected=%d %s headers=%v", res.StatusCode, raw, res.Header)
				}
			}
		})
	}
	for _, path := range []string{
		"/v1/agents", "/%761/agents", "/v1/sessions/sesn_missing/events", "/v1/sessions/sesn_missing/events/stream",
		"/v1/environments/env_missing/work", "/v1/environments/env_missing/%77ork",
		"/v1/skills/skill_missing/versions/latest/content", "/v1/files/file_missing/content",
		"/internal/v1/gate/config", "/internal/v1/gate/%63onfig", "/v1/messages", "/v1/models",
	} {
		t.Run("raw"+path, func(t *testing.T) {
			res, raw := s.request("GET", path, "", nativeHeaders(cookie, session.CSRFToken))
			if res.StatusCode != 401 {
				t.Fatalf("raw cookie authority=%d %s", res.StatusCode, raw)
			}
		})
	}
	for _, mode := range []login.Mode{login.ModeDisabled, login.ModeOIDC} {
		t.Run("no-native-"+string(mode), func(t *testing.T) {
			pool := newPoolWithKey(t)
			manager := login.New(pool, login.Config{Mode: mode})
			srv := httptest.NewServer(api.NewHandler(pool, nil, nil, nil, api.WithLogin(manager, nil)))
			defer srv.Close()
			legacy := &nativeServer{tserver: &tserver{t: t, url: srv.URL, pool: pool}, manager: manager}
			expectManagement(t, legacy, "GET", privateProxy+"/agents", "", http.Header{"X-Api-Key": {testKey}}, 401)
			expectManagement(t, legacy, "GET", "/v1/agents", "", http.Header{"X-Api-Key": {testKey}}, 200)
		})
	}
}

// Forwarding a signed edge assertion must not perform another sign-in or restore stale authority/profile.
func TestPrivateProxyDoesNotReplayIAPSignIn(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	claims := s.idp.Claims(laneAudience, time.Now())
	claims["roles"], claims["email"], claims["name"] = []any{"admins"}, "stale@example.test", "Stale profile"
	id := domain.NewID(domain.PrefixPrincipal).String()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,role,role_source,email,display_name,last_seen_at) VALUES ($1,$2,$3,'developer','idp','current@example.test','Current profile','2026-01-01T00:00:00Z')`, id, s.idp.Issuer(), claims["sub"]); err != nil {
		t.Fatal(err)
	}
	cookie, _ := s.issue(id)
	assertion := s.idp.Mint(t, claims)
	for _, offered := range []string{assertion, "not-even-a-valid-assertion"} {
		h := nativeHeaders(cookie, "")
		h.Set("X-Goog-IAP-JWT-Assertion", offered)
		expectManagement(t, s, "GET", privateProxy+"/agents", "", h, 200)
	}
	expectManagement(t, s, "GET", privateProxy+"/agents", "", http.Header{"X-Goog-Iap-Jwt-Assertion": {assertion}}, 401)
	var role, email, name string
	var seen time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT role,email,display_name,last_seen_at FROM principals WHERE id=$1`, id).Scan(&role, &email, &name, &seen); err != nil || role != "developer" || email != "current@example.test" || name != "Current profile" || !seen.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("cookie replayed sign-in: role/profile/seen=%s/%s/%s/%v err=%v", role, email, name, seen, err)
	}
}
