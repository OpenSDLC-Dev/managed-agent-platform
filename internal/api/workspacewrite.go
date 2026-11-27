package api

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
)

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
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT lock_workspace_write($1,$2,$3)`, workspace.ID(ctx), subject, keyID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return errForbidden("workspace access was withdrawn")
	}
	return nil
}

func (s *server) beginResourceWrite(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err == nil {
		if err = lockResourceWrite(ctx, tx); err != nil {
			_ = tx.Rollback(ctx)
		}
	}
	return tx, err
}
