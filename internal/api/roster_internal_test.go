package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5"
)

// The 40P01 → 409 mapping, driven by a real deadlock rather than a stubbed
// error: two transactions each hold FOR UPDATE on one coordinator's row (as
// updateAgent does before it resolves) and then resolve a roster naming the
// other, so each waits for the share lock the other holds exclusively.
// Postgres aborts one after deadlock_timeout; that one must surface as the
// 409 rosterLockErr promises, and the survivor resolves.
func TestResolveRosterDeadlockIs409(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	ids := [2]string{domain.NewID(domain.PrefixAgent).String(), domain.NewID(domain.PrefixAgent).String()}
	for _, id := range ids {
		if _, err := pool.Exec(ctx,
			`INSERT INTO agents (id, name, version, spec) VALUES ($1, 'c', 1, '{"model":{"id":"m"}}')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ($1, 1, 'c', '{"model":{"id":"m"}}')`, id); err != nil {
			t.Fatal(err)
		}
	}
	var txs [2]pgx.Tx
	for i, id := range ids {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT version FROM agents WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		txs[i] = tx
	}
	errs := make(chan error, 2)
	for i := range ids {
		go func(i int) {
			roster := json.RawMessage(`{"type":"coordinator","agents":["` + ids[1-i] + `"]}`)
			_, err := resolveRoster(ctx, txs[i], roster, ids[i], 2)
			errs <- err
		}(i)
	}
	conflicts, resolved := 0, 0
	for range ids {
		err := <-errs
		var ae *apiError
		switch {
		case err == nil:
			resolved++
		case errors.As(err, &ae) && ae.status == http.StatusConflict:
			conflicts++
		default:
			t.Errorf("resolveRoster under deadlock: %v, want the 409 or success", err)
		}
	}
	if conflicts != 1 || resolved != 1 {
		t.Fatalf("got %d conflicts and %d resolutions, want exactly one of each", conflicts, resolved)
	}
}
