package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Upgrading a real pre-purpose schema must neither turn old identity codes
// into password authority nor change their hash or database expiry.
func TestSetupCodePurposeUpgrade(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.MigrateThrough(ctx, pool, "0057_browser_login.sql"); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := pool.QueryRow(ctx, `INSERT INTO setup_codes(code_hash,expires_at) VALUES (repeat('a',64),clock_timestamp()+interval '1 hour') RETURNING expires_at`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var purpose, hash string
	var after time.Time
	if err := pool.QueryRow(ctx, `SELECT purpose,code_hash,expires_at FROM setup_codes`).Scan(&purpose, &hash, &after); err != nil {
		t.Fatal(err)
	}
	if purpose != "identity" || len(hash) != 64 || hash != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || !after.Equal(before) {
		t.Fatalf("upgrade changed setup authority: purpose=%q hash=%q expires=%v want=%v", purpose, hash, after, before)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO setup_codes(code_hash,expires_at,purpose) VALUES (repeat('b',64),clock_timestamp()+interval '1 hour','password')`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE setup_codes SET purpose=NULL`, `UPDATE setup_codes SET purpose='other'`} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("schema admitted invalid purpose: %s", q)
		}
	}
}
