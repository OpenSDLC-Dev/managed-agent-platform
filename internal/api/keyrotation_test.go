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
// calls succeeded, failing the test only if none did — a credential path where
// *every* caller fails is broken whatever its concurrency contract.
//
// How many *should* succeed is the caller's to assert, and the callers disagree
// on purpose. Both all-must-succeed cases are the majority now:
// TestConcurrentEnvironmentKeyIssuesAllSurvive (per-host keys — provisioning a
// fleet must not drop a host, which is exactly what migration 0021's retirement
// of environment_keys_one_live bought) and TestConcurrentSameValueAPIKeyMintsAllSucceed
// (replicas booting with one shared bootstrap value converge on one row). Only
// TestConcurrentAPIKeyMintsLeaveOneLiveKey still expects losers: distinct values
// under one name genuinely contend for a single live slot, so a loser must fail
// its own transaction rather than share it.
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

// TestConcurrentEnvironmentKeyIssuesAllSurvive is the environment-key half of
// #72's race, inverted by plan 30's model. Under rotate-on-mint, concurrent mints
// for one environment were a hazard — each revoked what it could see and none saw
// the others, leaving the queue with live credentials nobody knew about — and
// environment_keys_one_live resolved it by failing the losers. Per-host issuance
// has no shared slot to contend for: an operator provisioning a fleet in parallel
// must get every key they asked for, and every one of them must authenticate.
func TestConcurrentEnvironmentKeyIssuesAllSurvive(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	_, envID := pgtest.NewSession(t, pool, "self_hosted")

	// An incumbent first, so the racers issue alongside a live key rather than
	// into an empty environment — the exact shape that used to revoke it.
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
	// Distinct values, so no two hosts were handed the same credential.
	seen := map[string]bool{}
	for i, key := range keys {
		if key == "" || seen[key] {
			t.Fatalf("racer %d got a duplicate or empty key", i)
		}
		seen[key] = true
	}
}

// TestConcurrentAPIKeyMintsLeaveOneLiveKey is the same race on the management-key
// rotation path, which has the identical shape: concurrent EnsureAPIKey calls for
// one logical name must not leave two live x-api-key credentials under it.
func TestConcurrentAPIKeyMintsLeaveOneLiveKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	// Against a live incumbent, so the test also fails if the statements are
	// reordered back (see the environment-key twin).
	if err := api.EnsureAPIKey(ctx, pool, "boot", "ak-incumbent"); err != nil {
		t.Fatalf("seed incumbent: %v", err)
	}

	const racers = 8
	raceMints(t, racers, func(i int) error {
		return api.EnsureAPIKey(ctx, pool, "boot", fmt.Sprintf("ak-racer-%d", i))
	})

	if live := countLive(t, pool,
		"SELECT count(*) FROM api_keys WHERE name = $1 AND revoked_at IS NULL", "boot"); live != 1 {
		t.Fatalf("live api_keys after %d concurrent mints = %d, want 1", racers, live)
	}
}

// TestConcurrentSameValueAPIKeyMintsAllSucceed guards the production startup
// path: every controlplane replica calls EnsureAPIKey with the *same* bootstrap
// value (cmd/controlplane/main.go), and a returned error is fatal — the process
// refuses to start. Replicas booting together must therefore all succeed, which
// they do because the shared key_hash makes each racer's upsert converge on one
// row instead of adding a second live one. Nothing about the one-live index may
// turn a simultaneous rollout into a crash loop.
func TestConcurrentSameValueAPIKeyMintsAllSucceed(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	// A prior key under the same name, so the racers rotate rather than seed.
	if err := api.EnsureAPIKey(ctx, pool, "bootstrap", "ak-previous"); err != nil {
		t.Fatalf("seed previous key: %v", err)
	}

	const racers = 8
	succeeded := raceMints(t, racers, func(int) error {
		return api.EnsureAPIKey(ctx, pool, "bootstrap", "ak-shared")
	})
	if succeeded != racers {
		t.Errorf("same-value concurrent mints succeeded = %d/%d; a simultaneous rollout must not fail a replica", succeeded, racers)
	}
	if live := countLive(t, pool,
		"SELECT count(*) FROM api_keys WHERE name = $1 AND revoked_at IS NULL", "bootstrap"); live != 1 {
		t.Errorf("live api_keys = %d, want 1", live)
	}
}

// TestAPIKeyValueRepointsToAnotherName pins EnsureAPIKey's documented
// `name = EXCLUDED.name` behaviour now that the one-live index constrains it: a
// key value moved to a second logical name takes that name's live slot and frees
// the first, rather than leaving the value live under both.
func TestAPIKeyValueRepointsToAnotherName(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	if err := api.EnsureAPIKey(ctx, pool, "first", "ak-moving"); err != nil {
		t.Fatalf("mint under first: %v", err)
	}
	if err := api.EnsureAPIKey(ctx, pool, "second", "ak-incumbent"); err != nil {
		t.Fatalf("mint under second: %v", err)
	}
	if err := api.EnsureAPIKey(ctx, pool, "second", "ak-moving"); err != nil {
		t.Fatalf("re-point the value to second: %v", err)
	}

	if live := countLive(t, pool,
		"SELECT count(*) FROM api_keys WHERE name = $1 AND revoked_at IS NULL", "second"); live != 1 {
		t.Errorf("live keys under second = %d, want 1", live)
	}
	if live := countLive(t, pool,
		"SELECT count(*) FROM api_keys WHERE name = $1 AND revoked_at IS NULL", "first"); live != 0 {
		t.Errorf("live keys under first = %d, want 0 (the value moved)", live)
	}
}

// TestSchemaForbidsASecondLiveAPIKey pins the invariant in the schema rather than
// in the rotation code, so it holds against any writer — a hand-run statement, or
// a code path that forgets to revoke first. Racing transactions are how the
// duplicate arises; the index is what makes it unrepresentable. Only api_keys is
// covered here: migration 0021 retired the environment_keys half deliberately, and
// its inverse is pinned by TestSecondLiveEnvironmentKeyIsAccepted.
func TestSchemaForbidsASecondLiveAPIKey(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)

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
