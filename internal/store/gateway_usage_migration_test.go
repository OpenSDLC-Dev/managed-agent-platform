package store_test

import (
	"context"
	"testing"
	"time"

	gatewaystore "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestGatewayPrincipalUsagePreservesHistoryAndOldWriters(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.FreshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := store.MigrateThrough(ctx, pool, "0061_owned_api_key_issuer.sql"); err != nil {
		t.Fatal(err)
	}
	keyExec(t, pool, `INSERT INTO modelgateway.usage_daily (day,api_key_id,alias,deployment_id,requests,input_tokens)
		VALUES ((now() AT TIME ZONE 'UTC')::date,'key_old','fast','dep_1',7,70)`)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	_, err = conn.Conn().Prepare(ctx, "old_usage", `INSERT INTO modelgateway.usage
		(request_id,api_key_id,model,alias,deployment_id,credential_id,protocol,endpoint,status,latency_us,input_tokens)
		VALUES ('old','key_old','fast','fast','dep_1','cred_1','anthropic','messages',200,1,10)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "old_usage"); err != nil {
		t.Fatal(err)
	}
	s := gatewaystore.New(pool)
	_, err = s.RecordUsage(ctx, gatewaystore.Usage{RequestID: "new", APIKeyID: "key_owned", PrincipalID: "principal_owner",
		Model: "fast", Alias: "fast", DeploymentID: "dep_1", CredentialID: "cred_1", Protocol: "anthropic", Endpoint: "messages", Status: 200}, false)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC()
	days, more, err := s.ListDailyUsage(ctx, gatewaystore.UsageFilter{}, today, today, 10)
	if err != nil || more || len(days) != 2 {
		t.Fatalf("rollups: %+v %t %v", days, more, err)
	}
	if days[0].APIKeyID != "key_old" || days[0].PrincipalID != "" || days[0].Requests != 8 || days[0].Tokens.Input != 80 {
		t.Fatalf("swept history plus old writer: %+v", days[0])
	}
	if days[1].PrincipalID != "principal_owner" || days[1].APIKeyID != "" || days[1].Requests != 1 {
		t.Fatalf("new writer: %+v", days[1])
	}
	// Idempotent migration must not replay either source into the new rollup.
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT sum(requests) FROM modelgateway.usage_actor_daily`).Scan(&n); err != nil || n != 9 {
		t.Fatalf("repeated migration: requests=%d err=%v", n, err)
	}
}
