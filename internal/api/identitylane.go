package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// identityPrincipal is the verified human a request authenticated as: the
// principals row that records them, and the role their provider asserted THIS
// request. The role is carried, never stored — internal/store/migrations/
// 0022_principals.sql says why.
type identityPrincipal struct {
	ID   string
	Role identity.Role
}

// identityFrom returns the human a request authenticated as. ok is false on
// every machine lane — the management key, an environment key, a gate token —
// which is exactly what makes requireRole a no-op there.
func identityFrom(ctx context.Context) (identityPrincipal, bool) {
	p, ok := ctx.Value(ctxKeyIdentity).(identityPrincipal)
	return p, ok
}

// identityCredential returns the credential this deployment's mode expects, and
// whether it was present at all.
//
// The two modes never fall back to each other, and the asymmetry is deliberate.
// In oidc mode the credential is a Bearer with a JWT silhouette; a Bearer that
// is not JWT-shaped is left alone, because on the dual-auth paths that is how an
// environment key arrives. In trusted_proxy mode Bearer is ignored ENTIRELY and
// only the configured assertion header counts: the proxy is the only party that
// can set that header on a request reaching us, so accepting a Bearer as well
// would accept a credential the proxy never vouched for.
func identityCredential(r *http.Request, v *identity.Verifier) (token string, ok bool) {
	if v.Mode() == identity.ModeTrustedProxy {
		token = r.Header.Get(v.AssertionHeader())
		return token, token != ""
	}
	token, hasBearer := bearerToken(r)
	if !hasBearer || !identity.LooksLikeJWT(token) {
		return "", false
	}
	return token, true
}

// requireIdentity verifies the human credential, provisions the principal, and
// puts it on the context.
//
// Provisioning is on the AUTHENTICATION path on purpose: a verified token is
// already proof the human exists, so there is nothing to approve, and a request
// that could not record who made it should not proceed to act as them. A
// database failure here is a 500 rather than a 401 — the credential was good,
// and reporting an outage as an auth failure sends an operator hunting the IdP.
func requireIdentity(pool *pgxpool.Pool, v *identity.Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := identityCredential(r, v)
		if !ok {
			writeError(w, r, errAuth("missing credential"))
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
		principalID, err := upsertPrincipal(r.Context(), pool, id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyIdentity,
			identityPrincipal{ID: principalID, Role: id.Role})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireRole enforces a route's minimum role. It is the whole enforcement
// point, and it applies to the identity lane alone: a machine credential carries
// no role, so on those lanes it returns nil without looking at min.
//
// Default-deny falls out of Role.AtLeast rather than being coded here. AtLeast
// fails closed at both ends — a minimum that is not one of the three roles
// denies, and RoleNone satisfies nothing — and identity.RoleNone is not one of
// the three. So a route registered with RoleNone denies every human, and a human
// whose claims mapped to nothing denies everywhere. Neither case needs a branch,
// and neither can be forgotten. Slice 3 annotated every identity-reachable route
// per the plan's matrix; the registrations still at RoleNone are the machine
// lanes, where this function has already returned nil before min is read.
func requireRole(ctx context.Context, min identity.Role) error {
	p, onIdentityLane := identityFrom(ctx)
	if !onIdentityLane {
		return nil
	}
	if p.Role.AtLeast(min) {
		return nil
	}
	if _, real := identity.ParseRole(string(min)); real {
		return errForbidden("this route requires the " + string(min) + " role")
	}
	return errForbidden("this route is not available to SSO-authenticated callers")
}
