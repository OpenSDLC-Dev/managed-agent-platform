package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

type cookieContextKey struct{}

// cookieSessionFrom is deliberately separate from identityFrom. A machine key
// bypasses the old role check, and an IdP credential is not a browser session.
// New authenticated handlers must check this context at their own boundary.
func cookieSessionFrom(ctx context.Context) (login.Session, bool) {
	session, ok := ctx.Value(cookieContextKey{}).(login.Session)
	return session, ok
}

func requireCookieSession(r *http.Request) (login.Session, error) {
	session, ok := cookieSessionFrom(r.Context())
	if !ok {
		return login.Session{}, errAuth("a browser session cookie is required")
	}
	return session, nil
}

func (s *server) nativeLogin() bool { return s.login != nil && s.login.NativeEnabled() }

// privateCookiePath follows the mux's segment boundaries without cleaning the
// path. Encoded literal names are fine; encoded slashes, dot segments and paths
// that would redirect are not a way to move cookie authority onto /v1.
func privateCookiePath(r *http.Request) bool {
	parts := strings.Split(r.URL.EscapedPath(), "/")
	if len(parts) < 2 || parts[0] != "" {
		return false
	}
	for i := 1; i < len(parts); i++ {
		value, err := url.PathUnescape(parts[i])
		if err != nil || value == "" || value == "." || value == ".." || strings.Contains(value, "/") {
			return false
		}
		if i == 1 && value != "api" {
			return false
		}
	}
	return true
}

func legacyLoginCredentialOffered(r *http.Request, v *identity.Verifier) bool {
	if len(r.Header.Values("x-api-key")) > 0 || len(r.Header.Values("Authorization")) > 0 {
		return true
	}
	return v != nil && v.AssertionHeader() != "" && len(r.Header.Values(v.AssertionHeader())) > 0
}

func (s *server) requireCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proxy assertions may accompany a cookie at an IAP edge, but cannot replace
		// it. API keys and Authorization are explicitly refused on cookie-only routes.
		if !s.nativeLogin() || !privateCookiePath(r) || len(r.Header.Values("x-api-key")) > 0 || len(r.Header.Values("Authorization")) > 0 {
			writeError(w, r, errAuth("a browser session cookie is required"))
			return
		}
		session, err := s.login.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, login.ErrUnauthenticated) {
				w.Header().Set("x-platform-signed-out", "1")
				err = errAuth("invalid or expired browser session")
			}
			if errors.Is(err, principal.ErrDeactivated) {
				w.Header().Set("x-platform-signed-out", "1")
				err = errDeactivated
			}
			writeError(w, r, err)
			return
		}
		markVerified(r.Context())
		if s.login.CheckCSRF(r, session) != nil {
			writeError(w, r, errForbidden("invalid browser origin or CSRF token"))
			return
		}
		ctx := context.WithValue(r.Context(), cookieContextKey{}, session)
		ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: session.PrincipalID, Role: session.Role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Only the named method+path exceptions are anonymous. Neither a subtree nor
// the mux's cleaned redirect target is an authentication exception.
func (s *server) dispatchLoginAuth(next *http.ServeMux) http.Handler {
	legacy := dispatchAuth(s.pool, s.verifier, next)
	cookie := s.requireCookie(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := next.Handler(r)
		if strings.Contains(pattern, "/api/auth/") || strings.Contains(pattern, "setup_code/redeem") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		p := r.URL.EscapedPath()
		public := r.Method == http.MethodGet && p == "/api/auth/config"
		if s.nativeLogin() {
			switch s.login.Mode() {
			case login.ModeOIDC:
				public = public || r.Method == http.MethodGet && (p == "/api/auth/login" || p == "/api/auth/callback")
			case login.ModePassword, login.ModeTrustedProxy:
				public = public || r.Method == http.MethodPost && p == "/api/auth/login"
			}
			if s.login.Mode() == login.ModePassword && r.Method == http.MethodPost && p == "/api/console/organizations/default/setup_code/redeem" {
				public = true
			}
		}
		if public {
			next.ServeHTTP(w, r)
			return
		}
		cookieOnly := pattern == "GET /api/auth/session" || pattern == "/api/auth/session" || pattern == "POST /api/auth/logout" || pattern == "/api/auth/logout"
		if s.nativeLogin() && strings.HasSuffix(pattern, " "+consoleSetupCodePath) && s.login.Mode() != login.ModePassword {
			cookieOnly = true
		}
		// Use the matched mux pattern, including its method-less fallback, so
		// encoded literal segments cannot route a key to new administration.
		_, privatePattern, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			privatePattern = pattern
		}
		switch privatePattern {
		case consoleMembersPath, consoleMemberPath, consoleServiceAccountsPath, consoleServiceAccountPath, consoleServiceAccountArchivePath:
			cookieOnly = true
		}
		if cookieOnly {
			cookie.ServeHTTP(w, r)
			return
		}
		if s.nativeLogin() && privateCookiePath(r) && !legacyLoginCredentialOffered(r, s.verifier) {
			cookie.ServeHTTP(w, r)
			return
		}
		legacy.ServeHTTP(w, r)
	})
}
