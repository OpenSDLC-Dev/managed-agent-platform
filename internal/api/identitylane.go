package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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
		scope, err := identityScope(r, pool, v, id)
		if err != nil {
			writeError(w, r, err)
			return
		}
		// Stamped before anything writes, so both headers ride a 200 and a
		// role denial alike. ctxKeyBootstrapKey is deliberately not set: a
		// human is never the env-var-managed key.
		stampScope(w, scope)
		ctx := context.WithValue(r.Context(), ctxKeyIdentity,
			identityPrincipal{ID: principalID, Role: id.Role})
		ctx = context.WithValue(ctx, ctxKeyScope, scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// The identity lane's two tenancy refusals, both 403s (plan 42 §6.2).
const (
	// identityWorkspacesUnconfigured is decision 4's fail-closed arm. It names
	// the two variables because a deployment that grew a second workspace has a
	// configuration to write, and no other message would say which.
	identityWorkspacesUnconfigured = "this deployment has more than one live workspace and neither " +
		"IDENTITY_CLAIM_WORKSPACES nor IDENTITY_WORKSPACE_MAP is configured, so an identity's workspace cannot be resolved"
	// identityNoWorkspace is the no-authority refusal: authenticated, and a
	// member of nothing this deployment still runs. It is the membership twin of
	// a human whose claims mapped to no role, and takes the same shape — 403,
	// naming what is missing and never which workspaces exist.
	identityNoWorkspace = "this identity is a member of no live workspace in this deployment"
)

// identityScope resolves the workspace a human's request runs in (plan 42 §6.2).
//
// Membership comes from the claims the verifier already resolved; the registry
// decides which of them a deployment still runs, because archive is a tombstone
// and an archived workspace resolves no credential. The header then NARROWS,
// under the one rule selectWorkspace holds for every lane — so a workspace
// outside the identity's live set answers the same 404 whether it is foreign,
// archived, or never existed.
//
// The unconfigured deployment is the whole compatibility story: no claim, one
// live workspace, and every SSO installation that predates this slice keeps
// working untouched. Two live workspaces and no configuration is the arm that
// must refuse rather than guess.
func identityScope(r *http.Request, pool *pgxpool.Pool, v *identity.Verifier, id identity.Identity) (domain.Scope, error) {
	if !v.WorkspacesConfigured() {
		live, err := allLiveWorkspaceScopes(r.Context(), pool)
		if err != nil {
			return domain.Scope{}, err
		}
		// Zero folds in with "more than one" rather than taking a third arm: the
		// default workspace cannot be archived (§6.9), so a deployment with no
		// live workspace is unreachable, and a branch nothing exercises is worse
		// than a refusal that covers it.
		if len(live) != 1 {
			return domain.Scope{}, errForbidden(identityWorkspacesUnconfigured)
		}
		return selectWorkspace(r, live)
	}
	if len(id.Workspaces) == 0 {
		return domain.Scope{}, errForbidden(identityNoWorkspace)
	}
	covered, err := liveWorkspaceScopesAmong(r.Context(), pool, id.Workspaces)
	if err != nil {
		return domain.Scope{}, err
	}
	if len(covered) == 0 {
		return domain.Scope{}, errForbidden(identityNoWorkspace)
	}
	return selectWorkspace(r, covered)
}

// allLiveWorkspaceScopes reads every live workspace as a scope — the
// unconfigured deployment's case, where the COUNT is the decision.
//
// LIMIT 2 because that is the whole question: identityScope needs to know
// whether there is exactly one live workspace and which, and every answer past
// the second is the same answer.
//
// It is a separate function from liveWorkspaceScopesAmong rather than a nil
// argument to one. The fail-open reading ("no ids means every workspace") and
// the fail-closed one ("no ids means nothing") are one guard apart, and
// mappedWorkspaces returns exactly nil for an identity that is a member of
// nothing — so a single function taking a possibly-empty slice puts the
// dangerous input one missing check away from the dangerous branch.
func allLiveWorkspaceScopes(ctx context.Context, pool *pgxpool.Pool) ([]domain.Scope, error) {
	rows, err := pool.Query(ctx,
		`SELECT org_id, id FROM workspaces WHERE archived_at IS NULL ORDER BY id LIMIT 2`)
	if err != nil {
		return nil, err
	}
	return scanWorkspaceScopes(rows)
}

// liveWorkspaceScopesAmong selects the members of an identity's set that are
// still live. An empty set is answered without a query at all: it can only ever
// mean "a member of nothing", never "everything".
func liveWorkspaceScopesAmong(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]domain.Scope, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT org_id, id FROM workspaces WHERE archived_at IS NULL AND id = ANY ($1) ORDER BY id`, ids)
	if err != nil {
		return nil, err
	}
	return scanWorkspaceScopes(rows)
}

// scanWorkspaceScopes projects workspace rows to scopes. Org rides along from
// the row and the project is the frozen literal, because project has no
// registry of its own (migration 0034's header says so).
func scanWorkspaceScopes(rows pgx.Rows) ([]domain.Scope, error) {
	defer rows.Close()
	var out []domain.Scope
	for rows.Next() {
		s := domain.Scope{ProjectID: "default"}
		if err := rows.Scan(&s.OrgID, &s.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
// lanes. Usually this function has already returned nil for those, min unread —
// but not always, and the exception is the reason they keep RoleNone rather than
// nothing: dispatchAuth classifies the escaped path while ServeMux matches the
// decoded one, so a percent-encoded spelling of a work or gate route arrives HERE,
// on the identity lane, with a real principal. RoleNone is what turns it away.
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
