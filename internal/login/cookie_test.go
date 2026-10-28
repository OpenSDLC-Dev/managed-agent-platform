package login

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestCookieAndCSRFBoundaries(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	manager := New(pool, Config{Mode: ModeOIDC, PublicOrigin: "https://console.example"})
	issued, err := manager.Issue(ctx, "principal_abc123")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("managed-agent-platform:browser-csrf:v1\x00" + issued.Token))
	if issued.CSRFToken != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatal("CSRF must be domain-separated SHA-256 of the session cookie")
	}
	w := httptest.NewRecorder()
	manager.SetCookie(w, issued)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "map_session" || cookie.Value != issued.Token || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || !cookie.Expires.Equal(issued.ExpiresAt.Truncate(1e9)) {
		t.Fatal("unsafe session cookie attributes")
	}
	for _, tc := range []struct {
		origin      string
		development bool
		secure      bool
	}{
		{"http://localhost:3000", true, false}, {"http://127.0.0.1:3000", true, false}, {"http://[::1]:3000", true, false},
		{"https://console.example", true, true}, {"http://localhost:3000", false, true}, {"http://not-local.example", true, true},
	} {
		m := New(pool, Config{PublicOrigin: tc.origin, Development: tc.development})
		w := httptest.NewRecorder()
		m.SetCookie(w, issued)
		if got := w.Result().Cookies()[0].Secure; got != tc.secure {
			t.Errorf("Secure for %q (dev %v) = %v", tc.origin, tc.development, got)
		}
		w = httptest.NewRecorder()
		m.ClearCookie(w)
		cleared := w.Result().Cookies()[0]
		if cleared.Name != "map_session" || cleared.Value != "" || cleared.MaxAge >= 0 || cleared.Path != "/" || cleared.Domain != "" || cleared.Secure != tc.secure || !cleared.HttpOnly || cleared.SameSite != http.SameSiteLaxMode {
			t.Fatalf("clear cookie changes scope or fails expiry: %+v", cleared)
		}
	}
	valid := browserRequest("POST", issued.Token)
	valid.Header.Set("Origin", "https://console.example")
	valid.Header.Set("X-CSRF-Token", issued.CSRFToken)
	valid.Header.Set("Sec-Fetch-Site", "same-origin")
	if err := manager.CheckCSRF(valid, issued.Session); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"missing origin", func(r *http.Request) { r.Header.Del("Origin") }},
		{"wrong origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{"origin slash", func(r *http.Request) { r.Header.Set("Origin", "https://console.example/") }},
		{"duplicate origin", func(r *http.Request) { r.Header.Add("Origin", "https://console.example") }},
		{"missing csrf", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"wrong csrf", func(r *http.Request) { r.Header.Set("X-CSRF-Token", strings.Repeat("a", 43)) }},
		{"duplicate csrf", func(r *http.Request) { r.Header.Add("X-CSRF-Token", issued.CSRFToken) }},
		{"cross site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{"duplicate fetch metadata", func(r *http.Request) { r.Header.Add("Sec-Fetch-Site", "same-origin") }},
		{"forged forwarded origin", func(r *http.Request) {
			r.Header.Set("Origin", "http://evil.example")
			r.Header.Set("Forwarded", "proto=https;host=console.example")
			r.Header.Set("X-Forwarded-Host", "console.example")
			r.Header.Set("X-Forwarded-Proto", "https")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := valid.Clone(ctx)
			tc.change(r)
			if err := manager.CheckCSRF(r, issued.Session); err == nil {
				t.Fatal("accepted unsafe browser request")
			}
		})
	}
	for _, method := range []string{"GET", "HEAD", "OPTIONS"} {
		if err := manager.CheckCSRF(browserRequest(method, issued.Token), issued.Session); err != nil {
			t.Errorf("safe method %s: %v", method, err)
		}
	}
	for i, cookieHeaders := range [][]string{
		nil, {"map_session="}, {"map_session=" + issued.Token + "="}, {"map_session=\"" + issued.Token + "\""},
		{"map_session=" + issued.Token + "; map_session=" + issued.Token}, {"map_session=" + issued.Token, "map_session=" + issued.Token},
		{"map_session=bad; map_session=" + issued.Token}, {"map_session; map_session=" + issued.Token}, {"map_session =" + issued.Token},
		{"map_session=" + issued.Token + " "}, {"map_session=" + issued.Token[:42] + "B"}, {"map_session=%41" + issued.Token[1:]},
	} {
		t.Run("invalid cookie "+strconv.Itoa(i), func(t *testing.T) {
			r := browserRequest("POST", "")
			for _, h := range cookieHeaders {
				r.Header.Add("Cookie", h)
			}
			if _, err := manager.Authenticate(ctx, r); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("noncanonical cookie accepted: %v", err)
			}
			if err := manager.Revoke(ctx, r); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("noncanonical logout accepted: %v", err)
			}
		})
	}
	if _, err := manager.Authenticate(ctx, browserRequest("GET", issued.Token)); err != nil {
		t.Fatalf("bad-cookie logout revoked a valid session: %v", err)
	}
	noOriginManager := New(pool, Config{})
	if err := noOriginManager.CheckOrigin(valid); err == nil {
		t.Fatal("origin check admitted an unconfigured browser origin")
	}
}
