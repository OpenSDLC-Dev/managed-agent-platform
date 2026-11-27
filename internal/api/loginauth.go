package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/inferencetoken"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

type cookieContextKey struct{}

// cookieSessionFrom is separate from owned-key authority. Only a browser
// session can reach private routes, including credential mutations.
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

// beginCredentialMutation fences cookie-selected authority until the write commits.
// Callers read the body first.
func (s *server) beginCredentialMutation(r *http.Request) (pgx.Tx, error) {
	tx, _, err := s.beginWorkspaceCredentialMutation(r, identity.RoleAdmin)
	return tx, err
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

func (s *server) requireCookie(next *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proxy assertions may accompany a cookie at an IAP edge, but cannot replace
		// it. API keys and Authorization are explicitly refused on cookie-only routes.
		if !s.nativeLogin() || !privateCookiePath(r) || len(r.Header.Values("x-api-key")) > 0 || len(r.Header.Values("Authorization")) > 0 {
			writeError(w, r, errAuth("a browser session cookie is required"))
			return
		}
		_, pattern := next.Handler(r)
		_, route, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			route = pattern
		}
		if route == "/api/auth/logout" {
			r = r.Clone(r.Context())
			r.Header.Del("x-workspace-id")
		}
		session, err := s.login.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, login.ErrWorkspaceSelector) {
				err = errWorkspaceNotFound
			}
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
		hint, _ := workspace.PrivateSelector(r)
		if hint != "" && (session.WorkspaceID == "" || session.WorkspaceRole == "" && hint != workspace.Default) {
			writeError(w, r, errWorkspaceNotFound)
			return
		}
		ctx := context.WithValue(workspace.WithID(r.Context(), session.WorkspaceKey), cookieContextKey{}, session)
		role := session.Role
		if hint == "" && session.WorkspaceChoices > 1 && (route == consoleProxyPath || route == consoleProxyRootPath || route == consoleTokensPath || route == consoleRevokePath || route == consoleDeploymentInitiatorPath || route == consoleOrgAPIKeyPath) {
			writeError(w, r, errInvalid("select a workspace"))
			return
		}
		switch route {
		case "/api/auth/session", "/api/auth/workspaces", "/api/auth/logout":
			// Every active signed-in account can inspect its own session and leave it.
			role = identity.RoleUser
		case consoleProxyPath, consoleProxyRootPath, consoleAgentsOverviewPath, consoleTokensPath, consoleRevokePath:
			role = session.WorkspaceRole.ResourceRole()
		case consoleWorkspacesPath:
			// The directory lists the workspaces this person administers across tabs.
			// Creation retains the organization role.
			if r.Method == http.MethodGet && role != identity.RoleAdmin {
				var manages bool
				if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces w WHERE w.archived_at IS NULL AND workspace_role($1,w.id)='admin')`, session.PrincipalID).Scan(&manages); err != nil {
					writeError(w, r, err)
					return
				}
				if manages {
					role = identity.RoleAdmin
				}
			}
		case consoleWorkspacePath, consoleWorkspaceMembersPath, consoleWorkspaceMemberPath:
			role = session.WorkspaceRole.ResourceRole()
		case consoleAPIKeysPath, consoleOrgAPIKeyPath, consoleAPIKeyPath:
			role = keyManagerRole(session)
		}
		ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: session.PrincipalID, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Only the named method+path exceptions are anonymous. Neither a subtree nor
// the mux's cleaned redirect target is an authentication exception.
func (s *server) dispatchLoginAuth(next *http.ServeMux) http.Handler {
	resources := dispatchAuth(s.pool, next)
	cookie := s.requireCookie(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A delegated credential cannot select another lane, including an IAP
		// assertion or an accompanying cookie, nor follow a cleaning redirect.
		var tokens []string
		if apiKeyOffered(r) {
			if keys := r.Header.Values("x-api-key"); len(keys) == 1 && strings.HasPrefix(keys[0], inferencetoken.TokenPrefix) {
				tokens = keys
			}
		} else {
			// The control plane treats one empty x-api-key as absent. Its
			// sign-in exceptions must not accept an inference token;
			// none of those fields may conceal a live delegated credential.
			for _, value := range r.Header.Values("Authorization") {
				if token, ok := strings.CutPrefix(value, "Bearer "); ok && strings.HasPrefix(token, inferencetoken.TokenPrefix) {
					tokens = append(tokens, token)
				}
			}
		}
		if len(tokens) != 0 {
			p, err := inferencetoken.Authenticate(r.Context(), s.pool, tokens...)
			if err != nil {
				writeError(w, r, err)
				return
			}
			if p.WorkID != "" {
				writeError(w, r, errForbidden("this token permits inference only"))
				return
			}
		}
		_, pattern := next.Handler(r)
		if s.nativeLogin() && privateCookiePath(r) {
			w.Header().Set("Cache-Control", "no-store, private")
		}
		if strings.Contains(pattern, "/api/auth/") || strings.Contains(pattern, "setup_code/redeem") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
		}
		p := r.URL.EscapedPath()
		if p == cliAuthorizePath && (r.Method == http.MethodGet || r.Method == http.MethodPost) ||
			p == cliTokenPath && r.Method == http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
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
		cookieOnly := pattern == "GET /api/auth/workspaces" || pattern == "/api/auth/workspaces" || pattern == "GET /api/auth/session" || pattern == "/api/auth/session" || pattern == "POST /api/auth/logout" || pattern == "/api/auth/logout"
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
		case consoleMembersPath, consoleMemberPath, consoleServiceAccountsPath, consoleServiceAccountPath, consoleServiceAccountArchivePath, consoleDeploymentInitiatorPath, consoleAgentsOverviewPath, consoleProxyPath, consoleProxyRootPath:
			cookieOnly = true
		}
		if cookieOnly {
			cookie.ServeHTTP(w, r)
			return
		}
		if privateCookiePath(r) {
			cookie.ServeHTTP(w, r)
			return
		}
		resources.ServeHTTP(w, r)
	})
}
