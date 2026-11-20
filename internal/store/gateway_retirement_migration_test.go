package store_test

import (
	"fmt"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

func TestGatewayPolicyRetirementPreservesHistoryAndModelBudgets(t *testing.T) {
	ctx := t.Context()
	pool := rawPool(t, pgtest.FreshDB(t))
	if err := store.MigrateThrough(ctx, pool, "0065_inference_tokens.sql"); err != nil {
		t.Fatal(err)
	}
	seedKeyPrincipals(t, pool)
	insertKeyFixture(t, pool, "apikey_legacy", "", "active")
	insertKeyFixture(t, pool, "apikey_owned", "principal_human", "active")
	keyExec(t, pool, `INSERT INTO modelgateway.key_policies(api_key_id,aliases,rpm,tpm)
		VALUES ('apikey_legacy',ARRAY['fast'],7,900)`)
	keyExec(t, pool, `INSERT INTO modelgateway.rate_windows(api_key_id,minute,requests,tokens)
		VALUES ('apikey_legacy','2026-01-01T00:00:00Z',3,88)`)
	keyExec(t, pool, `INSERT INTO modelgateway.model_limits(model,rpm,itpm,otpm) VALUES ('fast',20,1000,500)`)
	keyExec(t, pool, `INSERT INTO modelgateway.model_rate_windows(model,minute,requests,input_tokens,output_tokens)
		VALUES ('fast','2026-01-01T00:00:00Z',3,88,9)`)
	keyExec(t, pool, `INSERT INTO modelgateway.usage
		(request_id,api_key_id,principal_id,model,alias,deployment_id,credential_id,protocol,endpoint,status,latency_us,input_tokens)
		VALUES ('old','apikey_legacy','','fast','fast','dep_old','cred_old','anthropic','messages',200,1,10),
		('owned','apikey_owned','principal_human','fast','fast','dep_new','cred_new','anthropic','messages',200,1,20)`)
	keyExec(t, pool, `INSERT INTO modelgateway.usage_daily(day,api_key_id,alias,deployment_id,requests,input_tokens)
		VALUES ('2000-01-01','apikey_legacy','fast','dep_old',7,70)`)
	seed, _ := pgtest.NewSession(t, pool, "cloud")
	for _, creator := range []string{"apikey_legacy", "apikey_owned"} {
		keyExec(t, pool, `INSERT INTO sessions(id,agent_id,agent_version,resolved_agent,environment_id,created_by)
			SELECT $1,agent_id,agent_version,resolved_agent,environment_id,$2 FROM sessions WHERE id=$3`,
			"sesn_"+creator, creator, seed)
		keyExec(t, pool, `INSERT INTO deployments(id,name,agent_id,agent_version,environment_id,created_by)
			SELECT $1,$1,agent_id,agent_version,environment_id,$2 FROM sessions WHERE id=$3`,
			"depl_"+creator, creator, seed)
		keyExec(t, pool, `INSERT INTO dreams(id,status,inputs,input_memory_store_id,input_session_ids,model,output_behavior,created_by,session_id)
			VALUES ($1,'pending','[]','memstore_fixture','{}','{"id":"fixture"}','{"type":"create_new"}',$2,$3)`,
			"drm_"+creator, creator, "sesn_"+creator)
	}
	// These snapshots include audit IDs, credential hashes and immutable
	// initiators. The cleanup must not rewrite them or reset organization use.
	tables := []string{"api_keys", "principals", "sessions", "deployments", "dreams", "session_initiators",
		"modelgateway.usage", "modelgateway.usage_daily", "modelgateway.usage_actor_daily",
		"modelgateway.model_limits", "modelgateway.model_rate_windows"}
	snapshots := make(map[string]string, len(tables))
	for _, table := range tables {
		snapshots[table] = keySnapshot(t, pool, fmt.Sprintf(`SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)),'[]')::text FROM %s r`, table))
	}
	for pass := range 2 {
		// Compare the rows at the retirement migration's schema boundary.
		if err := store.MigrateThrough(ctx, pool, "0066_retire_gateway_key_policies.sql"); err != nil {
			t.Fatal(err)
		}
		var retired bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('modelgateway.key_policies') IS NULL
			AND to_regclass('modelgateway.rate_windows') IS NULL
			AND to_regprocedure('modelgateway.key_policy_require_unowned()') IS NULL`).Scan(&retired); err != nil || !retired {
			t.Fatalf("migration pass %d: retired schema remains: %t, %v", pass, retired, err)
		}
		for _, table := range tables {
			got := keySnapshot(t, pool, fmt.Sprintf(`SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)),'[]')::text FROM %s r`, table))
			if got != snapshots[table] {
				t.Errorf("migration pass %d changed %s", pass, table)
			}
		}
	}
}
