package api_test

import (
	"context"
	"fmt"
	"strings"
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

	// Race against a live incumbent, not an empty slot: every racer then has a
	// row to revoke, which is what makes the test fail if the two statements are
	// ever reordered back to insert-then-revoke (the insert would meet the live
	// incumbent and trip the index on the very first rotation).
	if err := api.EnsureEnvironmentKey(ctx, pool, envID.String(), "ek-incumbent"); err != nil {
		t.Fatalf("seed incumbent: %v", err)
	}

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

// TestCrossEnvironmentReuseLeavesTheIncumbentAlone pins the rollback the revoke-
// before-insert order made load-bearing: the rejected call has already revoked
// the target environment's live key by the time it learns the value belongs to
// another environment, so only the transaction rollback keeps that environment's
// worker credential alive. The existing cross-environment test gives the target
// environment no key, so it cannot see this.
func TestCrossEnvironmentReuseLeavesTheIncumbentAlone(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	_, envA := pgtest.NewSession(t, pool, "self_hosted")
	_, envB := pgtest.NewSession(t, pool, "self_hosted")

	// A has *rotated away* from shared-value: the value is bound to A but retired,
	// and A holds a different live key. That is the corner where an unguarded
	// conflict action would un-revoke A's retired row, hand A a second live key,
	// and fail on environment_keys_one_live — losing the descriptive rejection to
	// a raw constraint error. One ordinary rotation leaves exactly this state.
	if err := api.EnsureEnvironmentKey(ctx, pool, envA.String(), "shared-value"); err != nil {
		t.Fatalf("mint for A: %v", err)
	}
	if err := api.EnsureEnvironmentKey(ctx, pool, envA.String(), "ek-a-current"); err != nil {
		t.Fatalf("rotate A: %v", err)
	}
	if err := api.EnsureEnvironmentKey(ctx, pool, envB.String(), "ek-b-incumbent"); err != nil {
		t.Fatalf("mint for B: %v", err)
	}
	var before string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL`,
		envB.String()).Scan(&before); err != nil {
		t.Fatalf("read B's incumbent: %v", err)
	}

	if err := api.EnsureEnvironmentKey(ctx, pool, envB.String(), "shared-value"); err == nil {
		t.Fatal("re-minting env A's key value for env B was accepted")
	} else if !strings.Contains(err.Error(), "already bound to a different environment") {
		t.Errorf("rejection surfaced as %v, want the bound-to-a-different-environment error", err)
	}

	// A's retired value must stay retired: the rejected mint may not resurrect it.
	var revoked bool
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at IS NOT NULL FROM environment_keys WHERE key_hash <> '' AND environment_id = $1
		   AND id <> (SELECT id FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL)`,
		envA.String()).Scan(&revoked); err != nil {
		t.Fatalf("read A's retired row: %v", err)
	}
	if !revoked {
		t.Error("env A's retired key was un-revoked by a rejected cross-environment mint")
	}

	var after string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL`,
		envB.String()).Scan(&after); err != nil {
		t.Fatalf("B has no live key after a rejected mint: %v", err)
	}
	if after != before {
		t.Errorf("B's live key changed from %s to %s across a rejected mint", before, after)
	}
	if live := countLive(t, pool,
		"SELECT count(*) FROM environment_keys WHERE environment_id = $1 AND revoked_at IS NULL",
		envA.String()); live != 1 {
		t.Errorf("env A live keys = %d, want 1 (the rejected mint must not disturb A either)", live)
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
