package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

// seedAlphaNotes writes an imported (non-prebuilt) anthropic skill with one
// version, 20260101, as its latest.
func seedAlphaNotes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
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
}

// checkRefs runs the check in a transaction of its own and returns its answer.
func checkRefs(t *testing.T, pool *pgxpool.Pool, raw string) error {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return checkAnthropicSkillRefs(ctx, tx, json.RawMessage(raw))
}

// TestTheAnthropicSkillCheckTakesNoLock pins the check as the read it is: the
// skill rows it resolved are not held while the create's transaction runs, so
// the FOR UPDATE a version upload, an import or a delete takes on the skill
// row proceeds — an agent create never waits on another transaction's
// archive upload, and a delete committing meanwhile leaves the agent a
// dangling reference that materialization skips (best-effort, registered).
func TestTheAnthropicSkillCheckTakesNoLock(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	seedAlphaNotes(t, pool)
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

// TestTheAnthropicSkillCheckBesideAVersionUpload is the case that retired the
// FOR SHARE lock: a version upload holds the skill row, has moved its
// latest_version to a version row of its own, and has not committed. A
// locking check waited behind it and then, rechecking the row it locked
// against its older snapshot, found the new latest_version without its
// version row and refused a valid `latest`. The read neither waits nor
// refuses, before the upload commits or after.
func TestTheAnthropicSkillCheckBesideAVersionUpload(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	seedAlphaNotes(t, pool)
	upload, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upload.Rollback(ctx) }()
	if _, err := upload.Exec(ctx, `SELECT 1 FROM skills WHERE id = 'alpha-notes' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Exec(ctx,
		`INSERT INTO skill_versions (id, skill_id, version, name, description, directory, sha256)
		 VALUES ($1, 'alpha-notes', '20260102', 'alpha-notes', 'd', 'alpha-notes', NULL)`,
		domain.NewID(domain.PrefixSkillVersion).String()); err != nil {
		t.Fatal(err)
	}
	if _, err := upload.Exec(ctx, `UPDATE skills SET latest_version = '20260102' WHERE id = 'alpha-notes'`); err != nil {
		t.Fatal(err)
	}

	const latest = `[{"type":"anthropic","skill_id":"alpha-notes","version":"latest"}]`
	done := make(chan error, 1)
	go func() { // off the test goroutine, so no t
		tx, err := pool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		done <- checkAnthropicSkillRefs(ctx, tx, json.RawMessage(latest))
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("check beside an uncommitted upload = %v, want accepted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check waited on the upload's lock")
	}
	if err := upload.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := checkRefs(t, pool, latest); err != nil {
		t.Errorf("check after the upload committed = %v, want accepted", err)
	}
}

// TestTheAnthropicSkillCheckRefusesAVersionOfNoForm pins the version refusal
// for a skill this catalog holds and is no prebuilt one: a version that is
// not latest, a number or an id is refused in #419's words without reaching
// a bind parameter — a NUL in it, which Postgres would refuse to bind, is
// the version sentence, not a fault.
func TestTheAnthropicSkillCheckRefusesAVersionOfNoForm(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedAlphaNotes(t, pool)
	for _, version := range []string{`v1.0`, `\u0000`, `skver_\u0000`} {
		err := checkRefs(t, pool, `[{"type":"anthropic","skill_id":"alpha-notes","version":"`+version+`"}]`)
		var ae *apiError
		if !errors.As(err, &ae) || ae.status != 400 {
			t.Errorf("version %q: err = %v, want the 400", version, err)
		}
	}
	if err := checkRefs(t, pool, `[{"type":"anthropic","skill_id":"alpha-notes","version":"20260101"}]`); err != nil {
		t.Errorf("the version it holds: %v", err)
	}
}
