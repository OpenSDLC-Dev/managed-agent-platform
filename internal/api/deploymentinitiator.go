package api

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

const consoleDeploymentInitiatorPath = "/api/console/organizations/{org}/deployments/{id}/initiator"

// Private administration only. These fields never enter a /v1 deployment DTO.
type deploymentInitiatorJSON struct {
	PrincipalID *string `json:"principal_id"`
	APIKeyID    *string `json:"api_key_id"`
}

func readDeploymentInitiator(r *http.Request, db principal.Querier) (deploymentInitiatorJSON, error) {
	var out deploymentInitiatorJSON
	if err := checkID(r.PathValue("id"), "deployment"); err != nil {
		return out, err
	}
	var kind string
	err := db.QueryRow(r.Context(), `SELECT d.initiator_principal_id,d.initiator_api_key_id,coalesce(p.kind,'user')
		FROM deployments d LEFT JOIN principals p ON p.id=d.initiator_principal_id
		WHERE d.id=$1 AND d.archived_at IS NULL`, r.PathValue("id")).Scan(&out.PrincipalID, &out.APIKeyID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errNotFound("deployment not found")
	}
	if err == nil && out.PrincipalID != nil {
		public := principal.PublicID(*out.PrincipalID, kind)
		out.PrincipalID = &public
	}
	return out, err
}

func (s *server) getDeploymentInitiator(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	return readDeploymentInitiator(r, s.pool)
}

func (s *server) setDeploymentInitiator(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	if err := checkID(r.PathValue("id"), "deployment"); err != nil {
		return nil, err
	}
	obj, err := managementBody(r, "principal_id")
	if err != nil {
		return nil, err
	}
	public, err := requiredString(obj, "principal_id")
	if err != nil {
		return nil, err
	}
	id, kind, ok := principal.ParsePublicID(public)
	if !ok || kind != "service_account" {
		return nil, errInvalid("principal_id must be a svac_... service account ID")
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	ctx := r.Context()
	defer tx.Rollback(ctx)
	if err := setDeploymentLockWait(ctx, tx); err != nil {
		return nil, err
	}
	// Principal first, then deployment: the membership setting and scheduling
	// snapshot cannot interleave. Session initiators are never rewritten.
	var deploymentID string
	err = tx.QueryRow(ctx, `SELECT id FROM deployments WHERE id=$1 AND archived_at IS NULL FOR UPDATE`, r.PathValue("id")).Scan(&deploymentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	// The deployment lock can outlast the browser session. Membership and
	// logout are fenced by beginPrincipalMutation; the database clock is not.
	var live bool
	if err := tx.QueryRow(ctx, `SELECT $1::timestamptz > clock_timestamp()`, actor.ExpiresAt).Scan(&live); err != nil {
		return nil, err
	}
	if !live {
		return nil, &apiErrorWithHeaders{err: errAuth("invalid or expired browser session"), headers: map[string]string{"x-platform-signed-out": "1"}}
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM principals WHERE id=$1 AND kind='service_account' AND org_id='default'`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("service account not found")
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, errInvalid("service account must be active")
	}
	if _, err := tx.Exec(ctx, `UPDATE deployments SET initiator_principal_id=$1,initiator_api_key_id=NULL,updated_at=now() WHERE id=$2`, id, deploymentID); err != nil {
		return nil, err
	}
	out, err := readDeploymentInitiator(r, tx)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit(ctx)
}
