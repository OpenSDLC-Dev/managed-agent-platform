package api

import (
	"encoding/json"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

// Called inside the existing organization-admin principal mutation fence.
// A missing field preserves the grant; an explicit null withdraws it and
// retires any grandfathered runs, even when the member had no explicit grant.
func updateDefaultGrant(r *http.Request, tx pgx.Tx, id string, body map[string]json.RawMessage) error {
	value, set, null, err := stringField(body, "default_workspace_role")
	if err != nil || !set {
		return err
	}
	if _, both := body["playground_access"]; both {
		return errInvalid("specify either default_workspace_role or playground_access")
	}
	ctx := r.Context()
	var orgRole string
	if err := tx.QueryRow(ctx, `SELECT role FROM principals WHERE id=$1`, id).Scan(&orgRole); err != nil {
		return err
	}
	if orgRole == "admin" || orgRole == "billing" && !null && value != "admin" {
		return errInvalid("this workspace role is inherited from the organization")
	}
	if null {
		tag, err := tx.Exec(ctx, `DELETE FROM workspace_memberships WHERE workspace_id='default' AND principal_id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `SELECT revoke_workspace_runs($1,'default')`, id); err != nil {
				return err
			}
		}
	} else {
		role := workspace.Role(value)
		if role != workspace.User && !role.CanUseResources() {
			return errInvalid("unsupported default_workspace_role")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_memberships(workspace_id,principal_id,role)
			VALUES ('default',$1,$2) ON CONFLICT (workspace_id,principal_id) DO UPDATE SET role=EXCLUDED.role`, id, value); err != nil {
			return err
		}
	}
	// Mirror the legacy checkbox without removing a stronger explicit grant.
	_, err = tx.Exec(ctx, `UPDATE principals SET playground_access=$2
		WHERE id=$1 AND playground_access IS DISTINCT FROM $2`, id, value == string(workspace.User))
	return err
}
