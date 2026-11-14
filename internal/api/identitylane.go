package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
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

// requireIdentity verifies the human credential, resolves the principal, and
// puts it on the context.
//
// Provisioning is on the AUTHENTICATION path on purpose: a verified token is
// already proof the human exists, so there is nothing to approve, and a request
// that could not record who made it should not proceed to act as them. A
// removed person is refused here too, with errDeactivated's 403, before any
// route sees them. A database failure here is a 500 rather than a 401 — the
// credential was good, and reporting an outage as an auth failure sends an
// operator hunting the IdP.
func requireIdentity(pool *pgxpool.Pool, v *identity.Verifier, manager *login.Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := v.Credential(r)
		if !ok {
			writeError(w, r, errAuth("missing credential"))
			return
		}
		// Dispatch has selected the human lane. An accompanying worker Bearer
		// cannot exempt its ambient assertion on the retained private surface.
		if manager != nil && manager.NativeEnabled() && v.Mode() == identity.ModeTrustedProxy && manager.CheckProxyOrigin(r) != nil {
			writeError(w, r, errForbidden("invalid browser origin"))
			return
		}
		id, err := v.Verify(r.Context(), token)
		if err != nil {
			// Reason() carries the detail; Error() is the constant string every
			// rejection shares, so the response tells a caller nothing about
			// which check failed. The operator's log gets the rest.
			var reason string
			var ie *identity.Error
			if errors.As(err, &ie) {
				reason = ie.Reason()
			}
			slog.InfoContext(r.Context(), "identity: credential rejected",
				"request_id", requestIDFrom(r.Context()), "reason", reason)
			writeError(w, r, errAuth(err.Error()))
			return
		}
		markVerified(r.Context())
		p, err := resolvePrincipal(r.Context(), pool, id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyIdentity, p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole checks owned-key authority first, then the retained human lane.
// Legacy unowned keys and scoped machine credentials keep their own rules.
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
