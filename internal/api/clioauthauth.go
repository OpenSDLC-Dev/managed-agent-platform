package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5/pgxpool"
)

type cliOAuthContextKey struct{}

func cliBearer(r *http.Request) bool {
	for _, value := range r.Header.Values("Authorization") {
		if strings.HasPrefix(value, "Bearer "+clioauth.AccessPrefix) {
			return true
		}
	}
	return false
}

func requireCLIOAuth(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if len(r.Header.Values("Authorization")) != 1 || !ok {
			writeError(w, r, errAuth("invalid OAuth token"))
			return
		}
		authority, err := clioauth.Authenticate(r.Context(), pool, token)
		if errors.Is(err, clioauth.ErrInvalidGrant) {
			err = errAuth("invalid OAuth token")
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		if err := cliResourceScope(r, authority); err != nil {
			writeError(w, r, err)
			return
		}
		markVerified(r.Context())
		ctx := context.WithValue(workspace.WithID(r.Context(), authority.WorkspaceKey), cliOAuthContextKey{}, authority)
		role := authority.WorkspaceRole.ResourceRole()
		if cliAuditPath(r) {
			role = authority.Role
		}
		ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: authority.PrincipalID, Role: role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func cliAuditPath(r *http.Request) bool {
	return r.URL.Path == cliWorkspacesPath || r.URL.Path == adminAPIKeysPath || strings.HasPrefix(r.URL.Path, adminAPIKeysPath+"/")
}

func cliResourceScope(r *http.Request, a clioauth.Authority) error {
	if !workspace.Matches(r.Header.Values("anthropic-workspace-id"), a.WorkspaceID) {
		return errForbidden("workspace is not served by this platform")
	}
	if cliAuditPath(r) {
		if !a.Has(clioauth.OrgAdmin) || a.Role != identity.RoleAdmin {
			return errForbidden("Authentication method not allowed for this endpoint.")
		}
		return nil
	}
	if a.Has(clioauth.Developer) {
		return nil
	}
	switch {
	case r.URL.Path == "/v1/files" || strings.HasPrefix(r.URL.Path, "/v1/files/"):
		return errForbidden("OAuth token does not meet scope requirement any_of(org:file_upload, user:developer, user:file_upload, user:managed_agents, workspace:developer)")
	case r.URL.Path == "/v1/skills" || strings.HasPrefix(r.URL.Path, "/v1/skills/"):
		return errForbidden("OAuth token does not meet scope requirement any_of(org:skills, user:developer, user:managed_agents, user:skills, workspace:developer, workspace:skills)")
	default:
		return errAuth("OAuth token lacks a scope this endpoint accepts")
	}
}

// The public role gate also admits owned admin keys. Organization audits require
// a consented CLI grant, including when the router matches an escaped path.
func cliAdminRead(fn func(*http.Request) (any, error)) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		a, _ := r.Context().Value(cliOAuthContextKey{}).(clioauth.Authority)
		if !a.Has(clioauth.OrgAdmin) || a.Role != identity.RoleAdmin {
			return nil, errForbidden("Authentication method not allowed for this endpoint.")
		}
		return fn(r)
	}
}

func (s *server) cliWorkspaces(r *http.Request) (any, error) {
	// The legacy CLI list omits Default. Registry-backed audit listing and
	// its Default inclusion option belong to the client rollout in Plan 42.
	return struct {
		Data    []any   `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}{Data: []any{}}, nil
}
