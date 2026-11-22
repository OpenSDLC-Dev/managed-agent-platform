package store_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

func TestDeploymentPriceMigrationKeepsUnlabelledRates(t *testing.T) {
	ctx := t.Context()
	pool := rawPool(t, pgtest.FreshDB(t))
	if err := store.MigrateThrough(ctx, pool, "0069_remove_bootstrap_api_keys.sql"); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO modelgateway.providers (id, name, profile, openai_base_url)
		VALUES ('provider', 'Existing account', 'deepseek', 'https://example.test');
		INSERT INTO modelgateway.deployments (id, provider_id, upstream_model, kind, price_input)
		VALUES ('before', 'provider', 'flash', 'chat', 0.25)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// A rolling upgrade can still have a writer that predates both new columns.
	_, err = pool.Exec(ctx, `INSERT INTO modelgateway.deployments (id, provider_id, upstream_model, kind, price_input)
		VALUES ('after', 'provider', 'another', 'chat', 0.25)`)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM modelgateway.deployments WHERE price_input = 0.25
		AND price_output IS NULL AND price_currency = '' AND price_note = ''`).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("unlabelled deployments = %d, %v", count, err)
	}
}
