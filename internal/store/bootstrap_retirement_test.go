package store_test

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

func TestRetireBootstrapKeyPreservesIssuedNamesAndAudit(t *testing.T) {
	ctx := t.Context()
	pool := rawPool(t, pgtest.FreshDB(t))
	if err := store.MigrateThrough(ctx, pool, "0068_playground_access.sql"); err != nil {
		t.Fatal(err)
	}
	seedKeyPrincipals(t, pool)
	keyExec(t, pool, `INSERT INTO api_keys (id, name, key_hash, status, created_by, workspace_id) VALUES
		('seed', 'bootstrap', 'h1', 'active', NULL, 'default'),
		('rotated', 'bootstrap', 'h2', 'archived', NULL, 'default'),
		('legacy_issued', 'bootstrap', 'h3', 'archived', 'apikey_issuer', 'default'),
		('other_workspace', 'bootstrap', 'h4', 'archived', NULL, 'another'),
		('other_preset', 'brain', 'h5', 'active', NULL, 'default')`)
	keyExec(t, pool, `INSERT INTO api_keys (id, name, owned_key_hash, owner_principal_id, created_by)
		VALUES ('user_key', 'bootstrap', 'h6', 'principal_human', 'principal_human')`)
	keyExec(t, pool, `INSERT INTO modelgateway.usage
		(request_id, api_key_id, model, alias, deployment_id, credential_id, protocol, endpoint, status, latency_us)
		VALUES ('prior_request', 'rotated', 'm', 'm', 'dep', 'cred', 'anthropic', 'messages', 200, 1)`)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := keySnapshot(t, pool, `SELECT string_agg(id, ',' ORDER BY id) FROM api_keys`); got != "legacy_issued,other_preset,other_workspace,user_key" {
		t.Errorf("surviving key IDs: %s", got)
	}
	if got := keySnapshot(t, pool, `SELECT api_key_id FROM modelgateway.usage WHERE request_id='prior_request'`); got != "rotated" {
		t.Errorf("historical usage records key ID %q", got)
	}
}
