package api_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// countLive reports how many unrevoked rows the query finds, failing the test on
// a query error so callers read as assertions.
func countLive(t *testing.T, pool *pgxpool.Pool, query string, arg any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, arg).Scan(&n); err != nil {
		t.Fatalf("count live keys: %v", err)
	}
	return n
}

// raceMints runs mint concurrently from a standing start and returns how many
// calls succeeded. A fleet's per-host environment keys must all survive.
func raceMints(t *testing.T, racers int, mint func(i int) error) int {
	t.Helper()
	start := make(chan struct{})
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = mint(i)
		}()
	}
	close(start)
	wg.Wait()

	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded == 0 {
		t.Fatalf("every concurrent mint failed: %v", errs)
	}
	return succeeded
}

// Per-host issuance has no shared slot to contend for. An operator provisioning
// a fleet in parallel must get every key, without revoking the incumbent.
func TestConcurrentEnvironmentKeyIssuesAllSurvive(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	_, envID := pgtest.NewSession(t, pool, "self_hosted")
	if _, err := api.IssueEnvironmentKey(ctx, pool, envID.String(), "incumbent"); err != nil {
		t.Fatalf("seed incumbent: %v", err)
	}

	const racers = 8
	keys := make([]string, racers)
	succeeded := raceMints(t, racers, func(i int) error {
		key, err := api.IssueEnvironmentKey(ctx, pool, envID.String(), fmt.Sprintf("host-%d", i))
		keys[i] = key
		return err
	})
	if succeeded != racers {
		t.Errorf("concurrent issues succeeded = %d/%d; provisioning a fleet must not drop a host", succeeded, racers)
	}
	if live := countLive(t, pool,
		"SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL",
		envID.String()); live != racers+1 {
		t.Fatalf("live environment_keys after %d concurrent issues = %d, want %d", racers, live, racers+1)
	}
	seen := map[string]bool{}
	for i, key := range keys {
		if key == "" || seen[key] {
			t.Fatalf("racer %d got a duplicate or empty key", i)
		}
		seen[key] = true
	}
}
