package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

// The organization member form can also edit its Default assignment.
func updateDefaultGrant(r *http.Request, tx pgx.Tx, id string, body map[string]json.RawMessage) error {
	value, set, null, err := stringField(body, "default_workspace_role")
	if err != nil || !set {
		return err
	}
	if _, both := body["playground_access"]; both {
		return errInvalid("specify either default_workspace_role or playground_access")
	}
	var grant *string
	if !null {
		grant = &value
	}
	var orgRole string
	if err := tx.QueryRow(r.Context(), `SELECT role FROM principals WHERE id=$1`, id).Scan(&orgRole); err != nil {
		return err
	}
	return setWorkspaceGrant(r.Context(), tx, workspace.Default, id, orgRole, grant)
}

func setWorkspaceGrant(ctx context.Context, tx pgx.Tx, space, id, orgRole string, grant *string) error {
	if orgRole == "admin" || orgRole == "billing" && grant != nil && *grant != string(workspace.Admin) {
		return errInvalid("this workspace role is inherited from the organization")
	}
	if grant == nil {
		tag, err := tx.Exec(ctx, `DELETE FROM workspace_memberships WHERE workspace_id=$1 AND principal_id=$2`, space, id)
		if err != nil {
			return err
		}
		// Work started under an inherited role has no explicit membership row to delete.
		if tag.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `SELECT revoke_workspace_runs($1,$2)`, id, space); err != nil {
				return err
			}
		}
	} else {
		role := workspace.Role(*grant)
		if role != workspace.User && !role.CanUseResources() {
			return errInvalid("unsupported workspace role")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_memberships(workspace_id,principal_id,role)
   VALUES($1,$2,$3) ON CONFLICT (workspace_id,principal_id) DO UPDATE SET role=EXCLUDED.role`, space, id, *grant); err != nil {
			return err
		}
	}
	if space == workspace.Default {
		_, err := tx.Exec(ctx, `UPDATE principals SET playground_access=$2 WHERE id=$1 AND playground_access IS DISTINCT FROM $2`, id, grant != nil && *grant == string(workspace.User))
		return err
	}
	return nil
}
