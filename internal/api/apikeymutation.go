package api

import (
	"errors"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
)

// beginAPIKeyMutation holds current human authority through the key write.
// Read the complete body first. Principal -> browser session -> key is the
// shared lock order with principal removal, which archives owned keys.
func (s *server) beginAPIKeyMutation(r *http.Request) (pgx.Tx, identityPrincipal, error) {
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
	caller := identityPrincipal{ID: session.PrincipalID, Role: session.Role}
	if !caller.Role.AtLeast(identity.RoleDeveloper) {
		return fail(errForbidden("this route requires the developer role"))
	}
	return tx, caller, nil
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
		return &caller.ID, nil
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
	err := tx.QueryRow(r.Context(), `SELECT kind,status,org_id FROM principals WHERE id=$1`, id).Scan(&kind, &status, &org)
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
		return &id, nil
	}
	if kind != "service_account" {
		return nil, errForbidden("API keys cannot be created for another user")
	}
	if caller.Role != identity.RoleAdmin {
		return nil, errForbidden("an admin browser session is required to assign a service account")
	}
	return &id, nil
}
