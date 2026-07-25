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
// calls succeeded, failing the test if none did. A mint that loses the race must
// fail its own transaction rather than share the credential slot, so some errors
// are expected — but a rotation path where *every* caller fails is broken too.
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

// TestConcurrentEnvironmentKeyMintsLeaveOneLiveKey: two EnsureEnvironmentKey
// calls for one environment race under READ COMMITTED. Each opens its own
// transaction, and neither's revoke step can see the other's uncommitted insert,
// so both revoke nothing and both commit — leaving the environment with two live
// Authorization: Bearer credentials driving the same work queue, the second one
// unknown to whoever minted the first.
//
// The invariant is one live key per environment at all times: a mint that cannot
// hold that slot must fail its transaction instead of sharing it.
func TestConcurrentEnvironmentKeyMintsLeaveOneLiveKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	_, envID := pgtest.NewSession(t, pool, "self_hosted")

	const racers = 8
	raceMints(t, racers, func(i int) error {
		return api.EnsureEnvironmentKey(ctx, pool, envID.String(), fmt.Sprintf("ek-racer-%d", i))
	})

	if live := countLive(t, pool,
		"SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL",
		envID.String()); live != 1 {
		t.Fatalf("live environment_keys after %d concurrent mints = %d, want 1", racers, live)
	}
}

// TestConcurrentAPIKeyMintsLeaveOneLiveKey is the same race on the management-key
// rotation path, which has the identical shape: concurrent EnsureAPIKey calls for
// one logical name must not leave two live x-api-key credentials under it.
func TestConcurrentAPIKeyMintsLeaveOneLiveKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	const racers = 8
	raceMints(t, racers, func(i int) error {
		return api.EnsureAPIKey(ctx, pool, "boot", fmt.Sprintf("ak-racer-%d", i))
	})

	if live := countLive(t, pool,
		"SELECT count(*) FROM api_keys WHERE name = $1 AND revoked_at IS NULL", "boot"); live != 1 {
		t.Fatalf("live api_keys after %d concurrent mints = %d, want 1", racers, live)
	}
}

// TestSchemaForbidsASecondLiveKey pins the invariant in the schema rather than in
// the rotation code, so it holds against any writer — a future operator issuance
// surface (#43), a hand-run statement, or a code path that forgets to revoke
// first. Racing transactions are how the duplicate arises; the index is what
// makes it unrepresentable.
func TestSchemaForbidsASecondLiveKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	_, envID := pgtest.NewSession(t, pool, "self_hosted")

	if err := api.EnsureEnvironmentKey(ctx, pool, envID.String(), "ek-first"); err != nil {
		t.Fatalf("EnsureEnvironmentKey: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ($1, $2, $3)`,
		"envkey_second", envID.String(), "second-hash"); err == nil {
		t.Error("a second live environment key was accepted; one environment must have one live credential")
	}

	if err := api.EnsureAPIKey(ctx, pool, "boot", "ak-first"); err != nil {
		t.Fatalf("EnsureAPIKey: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)`,
		"apikey_second", "boot", "second-hash"); err == nil {
		t.Error("a second live api key was accepted; one name must have one live credential")
	}

	// Revoked rows are not duplicates: the index is partial so rotation history
	// accumulates freely, and a second name keeps its own live slot.
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, revoked_at) VALUES ($1, $2, $3, now())`,
		"apikey_revoked", "boot", "third-hash"); err != nil {
		t.Errorf("a revoked row was rejected, so the index is not partial: %v", err)
	}
	if err := api.EnsureAPIKey(ctx, pool, "other", "ak-other"); err != nil {
		t.Errorf("a second logical name was refused its own live key: %v", err)
	}
}
