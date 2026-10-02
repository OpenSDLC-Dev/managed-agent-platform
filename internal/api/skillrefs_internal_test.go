package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestTheAnthropicSkillCheckHoldsWhatItResolved pins the lock the check takes:
// the skill rows it resolved stay FOR SHARE until the create's transaction
// ends, so the FOR UPDATE a skill or version delete takes first waits rather
// than slipping between the check and the agent's insert.
func TestTheAnthropicSkillCheckHoldsWhatItResolved(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO skills (id, source, display_title, latest_version) VALUES ('alpha-notes', 'anthropic', 'alpha-notes', '20260101')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ($1, 'alpha-notes', '20260101', 'alpha-notes', 'd', 'alpha-notes', NULL)`,
		domain.NewID(domain.PrefixSkillVersion).String()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := checkAnthropicSkillRefs(ctx, tx, json.RawMessage(`[{"type":"anthropic","skill_id":"alpha-notes"}]`)); err != nil {
		t.Fatalf("check: %v", err)
	}
	_, err = pool.Exec(ctx, `SELECT 1 FROM skills WHERE id = 'alpha-notes' FOR UPDATE NOWAIT`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("a delete's FOR UPDATE beside the check = %v, want lock_not_available", err)
	}
}
