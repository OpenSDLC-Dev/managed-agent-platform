package api

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// identityPrincipal is the authenticated human's stored audit ID and role.
// IdP sign-in applies the mapping through principal.Resolve; a browser cookie
// reads current authority without replaying that sign-in write.
type identityPrincipal struct {
	ID   string
	Role identity.Role
}

// identityFrom returns the authenticated human. API-key owners have a separate
// authority context, so an owned credential never becomes a cookie/IdP actor.
func identityFrom(ctx context.Context) (identityPrincipal, bool) {
	p, ok := ctx.Value(ctxKeyIdentity).(identityPrincipal)
	return p, ok
}

// requireRole checks owned-key authority or the authenticated browser session.
// Scoped worker credentials keep their own rules.
// RoleNone satisfies no principal-bound caller: an encoded work/gate segment
// can miss dispatch's machine predicate but still match ServeMux's route.
// That route's RoleNone must refuse before its handler checks work scope.
func requireRole(ctx context.Context, min identity.Role) error {
	var role identity.Role
	refusal := "this route is not available to SSO-authenticated callers"
	if key, ok := ownedKeyFrom(ctx); ok {
		role = key.Role
		refusal = "this route is not available to principal-owned API keys"
	} else if p, ok := identityFrom(ctx); ok {
		role = p.Role
	} else {
		return nil
	}
	if role.AtLeast(min) {
		return nil
	}
	if _, real := identity.ParseRole(string(min)); real {
		return errForbidden("this route requires the " + string(min) + " role")
	}
	return errForbidden(refusal)
}
