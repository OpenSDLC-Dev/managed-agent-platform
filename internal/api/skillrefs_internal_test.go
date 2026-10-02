package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// TestTheAnthropicSkillCheckTakesNoLock pins the check as the read it is: the
// skill rows it resolved are not held while the create's transaction runs, so
// the FOR UPDATE a version upload, an import or a delete takes on the skill
// row proceeds — an agent create never waits on another transaction's
// archive upload, and a delete committing meanwhile leaves the agent a
// dangling reference that materialization skips (best-effort, registered).
func TestTheAnthropicSkillCheckTakesNoLock(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `SELECT 1 FROM skills WHERE id = 'alpha-notes' FOR UPDATE NOWAIT`); err != nil {
		t.Errorf("a version upload's FOR UPDATE beside the check = %v, want it taken", err)
	}
}
