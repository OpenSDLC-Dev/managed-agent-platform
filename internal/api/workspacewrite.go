package api

import (
	"context"
	"net/http"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

type writeRoleKey struct{}

// The dispatcher's admin minimum strengthens any Console-specific minimum.
func withWriteFloor(r *http.Request, min identity.Role) *http.Request {
	if min == identity.RoleAdmin {
		return r.WithContext(context.WithValue(r.Context(), writeRoleKey{}, workspace.Admin))
	}
	return r
}

// Recheck the authenticated subject inside the write transaction. The registry
// lock orders this commit against archival and membership edits. Machine lanes
// already bind their authority to a single environment or work item.
func lockResourceWrite(ctx context.Context, tx pgx.Tx) error {
	var subject, keyID string
	if key, ok := ownedKeyFrom(ctx); ok {
		subject, keyID = key.OwnerID, key.KeyID
	} else if principal, ok := identityFrom(ctx); ok {
		subject = principal.ID
	}
	min, _ := ctx.Value(writeRoleKey{}).(workspace.Role)
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT lock_workspace_write($1,$2,$3,$4)`, workspace.ID(ctx), subject, keyID, min).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return errForbidden("workspace access was withdrawn")
	}
	return nil
}

func (s *server) beginResourceWrite(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err == nil {
		if err = lockResourceWrite(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
		}
	}
	return tx, err
}
