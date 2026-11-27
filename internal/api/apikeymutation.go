package api

import (
	"errors"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

// beginWorkspaceCredentialMutation holds current human authority through the key write.
// Read the complete body first. Principal -> browser session -> key is the
// shared lock order with principal removal, which archives owned keys.
func (s *server) beginWorkspaceCredentialMutation(r *http.Request, min identity.Role) (pgx.Tx, identityPrincipal, error) {
	if _, err := requireCookieSession(r); err != nil {
		return nil, identityPrincipal{}, err
	}
	ctx := r.Context()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, identityPrincipal{}, err
	}
	fail := func(err error) (pgx.Tx, identityPrincipal, error) {
		tx.Rollback(ctx)
		return nil, identityPrincipal{}, err
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return fail(err)
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT true FROM workspaces WHERE id=$1 AND archived_at IS NULL FOR SHARE`, workspace.ID(ctx)).Scan(&active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fail(errForbidden("workspace access was withdrawn"))
		}
		return fail(err)
	}
	session, err := s.login.AuthenticateInTx(ctx, tx, r)
	if err != nil {
		switch {
		case errors.Is(err, login.ErrUnauthenticated):
			err = &apiErrorWithHeaders{err: errAuth("invalid or expired browser session"), headers: map[string]string{"x-platform-signed-out": "1"}}
		case errors.Is(err, principal.ErrDeactivated):
			err = &apiErrorWithHeaders{err: errDeactivated, headers: map[string]string{"x-platform-signed-out": "1"}}
		}
		return fail(err)
	}
	caller := identityPrincipal{ID: session.PrincipalID, Role: keyManagerRole(session)}
	if !caller.Role.AtLeast(min) {
		return fail(errForbidden("this route requires the " + string(min) + " role"))
	}
	return tx, caller, nil
}

func keyManagerRole(session login.Session) identity.Role {
	return session.WorkspaceRole.ResourceRole()
}

// Lifecycle guards are independent of row permission. Inactive, expired and
// archived owned rows remain visible; a preset is visible to admins but immutable.
func canManageAPIKey(k ManagementKey, caller identityPrincipal) bool {
	if k.CreatedBy == nil {
		return false
	}
	return caller.Role == identity.RoleAdmin ||
		caller.Role == identity.RoleDeveloper && k.OwnerPrincipalID != nil && *k.OwnerPrincipalID == caller.ID
}

// resolveAPIKeyOwner runs only inside the principal-first mutation fence. A
// public alias asserts a kind; the stored row must confirm it before assignment.
func resolveAPIKeyOwner(r *http.Request, tx pgx.Tx, caller identityPrincipal, alias *string) (*string, error) {
	if alias == nil {
		return &caller.ID, requireKeyOwnerGrant(r, tx, caller.ID)
	}
	if _, err := requireCookieSession(r); err != nil {
		return nil, err
	}
	notFound := withDetails(errNotFound("principal %s not found", *alias), userFacingDetails)
	id, claimedKind, ok := principal.ParsePublicID(*alias)
	if !ok {
		return nil, notFound
	}
	var kind, status, org string
	var issuerRole identity.Role
	err := tx.QueryRow(r.Context(), `SELECT owner.kind,owner.status,owner.org_id,issuer.role
		FROM principals owner JOIN principals issuer ON issuer.id=$2 WHERE owner.id=$1`, id, caller.ID).
		Scan(&kind, &status, &org, &issuerRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if kind != claimedKind || status != "active" || org != reservedOrganization {
		return nil, notFound
	}
	if id == caller.ID {
		return &id, requireKeyOwnerGrant(r, tx, id)
	}
	if kind != "service_account" {
		return nil, errForbidden("API keys cannot be created for another user")
	}
	if issuerRole != identity.RoleAdmin {
		return nil, errForbidden("an organization admin browser session is required to assign a service account")
	}
	return &id, requireKeyOwnerGrant(r, tx, id)
}

func requireKeyOwnerGrant(r *http.Request, tx pgx.Tx, ownerID string) error {
	var allowed bool
	if err := tx.QueryRow(r.Context(), `SELECT coalesce(workspace_role($1,$2) IN ('admin','developer','limited_developer'),false)`, ownerID, workspace.ID(r.Context())).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return errForbidden("the key owner requires developer access in this workspace")
	}
	return nil
}
