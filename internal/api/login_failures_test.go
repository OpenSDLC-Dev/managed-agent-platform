package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestNativeOIDCCallbackFailureAndReplay(t *testing.T) {
	for _, failure := range []string{"nonce", "provider", "deactivated"} {
		t.Run(failure, func(t *testing.T) {
			s := newNativeServer(t, login.ModeOIDC)
			res, _ := s.request("GET", "/api/auth/login", "", nil)
			u, err := url.Parse(res.Header.Get("Location"))
			if err != nil || res.StatusCode != 303 {
				t.Fatalf("start=%d err=%v", res.StatusCode, err)
			}
			var cookie string
			for _, c := range res.Cookies() {
				if c.Name == "map_login" {
					cookie = c.Name + "=" + c.Value
				}
			}
			if failure == "deactivated" {
				_, err = s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,status) VALUES ('principal_removed',$1,'user-1','deactivated')`, s.idp.Issuer())
				if err != nil {
					t.Fatal(err)
				}
			}
			var exchanges atomic.Int32
			const secret = "FIXTURE-SECRET-IN-PROVIDER-ERROR"
			s.idp.SetTokenHandler(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				if failure == "provider" {
					http.Error(w, secret+" access_token=fixture", 400)
					return
				}
				claims := s.idp.Claims(laneAudience, time.Now())
				claims["nonce"] = u.Query().Get("nonce")
				if failure == "nonce" {
					claims["nonce"] = "wrong-nonce"
				}
				json.NewEncoder(w).Encode(map[string]string{"id_token": s.idp.Mint(t, claims)})
			})
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			path := "/api/auth/callback?state=" + url.QueryEscape(u.Query().Get("state")) + "&code=" + secret
			res, raw := s.request("GET", path, "", http.Header{"Cookie": {cookie}})
			want := 401
			if failure == "deactivated" {
				want = 403
			}
			marker := ""
			if failure == "deactivated" {
				marker = "1"
			}
			if res.StatusCode != want || res.Header.Get("x-platform-signed-out") != marker || strings.Contains(string(raw), secret) || strings.Contains(logs.String(), secret) {
				t.Fatalf("callback failure=%d want=%d marker=%q want=%q body=%s logs=%s", res.StatusCode, want, res.Header.Get("x-platform-signed-out"), marker, raw, logs.String())
			}
			for _, c := range res.Cookies() {
				if c.Name == "map_session" {
					t.Fatal("failed callback issued a session")
				}
			}
			res, _ = s.request("GET", path, "", http.Header{"Cookie": {cookie}})
			if res.StatusCode != 401 || exchanges.Load() != 1 {
				t.Fatalf("failed attempt replayed: status=%d exchanges=%d", res.StatusCode, exchanges.Load())
			}
			var sessions int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM browser_sessions`).Scan(&sessions); err != nil || sessions != 0 {
				t.Fatalf("failed callback sessions=%d err=%v", sessions, err)
			}
		})
	}
}

func TestNativeIAPRejectsInvalidAssertions(t *testing.T) {
	s := newNativeServer(t, login.ModeTrustedProxy)
	valid := s.idp.Mint(t, s.idp.Claims(laneAudience, time.Now()))
	wrong := s.idp.Mint(t, s.idp.Claims("wrong-audience", time.Now()))
	for _, values := range [][]string{nil, {wrong}, {"invalid.jwt.value"}, {valid, valid}} {
		h := nativeHeaders("", "")
		if values != nil {
			h["X-Goog-Iap-Jwt-Assertion"] = values
		}
		res, raw := s.request("POST", "/api/auth/login", "", h)
		if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "" || len(res.Cookies()) != 0 {
			t.Fatalf("invalid assertion=%d %s", res.StatusCode, raw)
		}
	}
}

func TestNativeExpiredCookieAndStoreFailure(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	cookie, _ := s.issue(s.seedLocal("expiry", "admin", "active"))
	_, err := s.pool.Exec(context.Background(), `UPDATE browser_sessions SET created_at=clock_timestamp()-interval '13 hours',expires_at=clock_timestamp()-interval '1 second'`)
	if err != nil {
		t.Fatal(err)
	}
	res, raw := s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 401 || res.Header.Get("x-platform-signed-out") != "1" {
		t.Fatalf("expired HTTP session=%d %s", res.StatusCode, raw)
	}
	s.pool.Close()
	res, raw = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 500 {
		t.Fatalf("database failure admitted or misclassified cookie=%d %s", res.StatusCode, raw)
	}
}
