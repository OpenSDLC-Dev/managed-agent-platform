package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}

const (
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
	pgForeignKeyViolation = "23503"
	pgNotNullViolation    = "23502"
)

// wantMigrations tracks the number of embedded migration files; bump it when
// a migration is added.
const wantMigrations = 16

func open(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSessionChain inserts the minimal FK chain (agent -> version snapshot ->
// environment -> session) so tests can exercise child tables.
func seedSessionChain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO agents (id, name, spec) VALUES ('agent_1', 'a', '{}')`,
		`INSERT INTO agent_versions (agent_id, version, name, spec) VALUES ('agent_1', 1, 'a', '{}')`,
		`INSERT INTO environments (id, name, kind, config) VALUES ('env_1', 'e', 'cloud', '{"type":"cloud"}')`,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_1', 'agent_1', 1, '{}', 'env_1')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

func TestOpenMigratesFreshDatabase(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()

	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'`)
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		tables = append(tables, name)
	}
	for _, want := range []string{
		"schema_migrations", "agents", "agent_versions", "environments",
		"sessions", "events", "work_items", "api_keys", "environment_keys",
		"skills", "skill_versions", "files", "vaults", "vault_credentials",
		"session_gate_tokens",
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %q missing after migration; have %v", want, tables)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != wantMigrations {
		t.Errorf("schema_migrations rows = %d, want %d", applied, wantMigrations)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != wantMigrations {
		t.Errorf("schema_migrations rows after re-run = %d, want %d", applied, wantMigrations)
	}
}

func TestConcurrentMigratorsDoNotConflict(t *testing.T) {
	// Several binaries (controlplane, brain, executor) may Open the same
	// database at startup simultaneously; the advisory lock must serialize
	// them onto one successful migration run.
	dsn := pgtest.FreshDB(t)
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool, err := store.Open(context.Background(), dsn)
			if err == nil {
				pool.Close()
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Open %d: %v", i, err)
		}
	}
}

func TestEventSeqUniquePerSession(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	insert := `INSERT INTO events (id, session_id, seq, type, payload)
	           VALUES ($1, 'sesn_1', $2, 'user.message', '{}')`
	if _, err := pool.Exec(ctx, insert, "sevt_1", 1); err != nil {
		t.Fatalf("first event: %v", err)
	}
	_, err := pool.Exec(ctx, insert, "sevt_2", 1)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		t.Errorf("duplicate (session_id, seq) => %v, want unique violation %s", err, pgUniqueViolation)
	}
	// The same seq in a different session must be fine.
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_2', 'agent_1', 1, '{}', 'env_1')`); err != nil {
		t.Fatalf("second session: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, session_id, seq, type, payload)
	                             VALUES ('sevt_3', 'sesn_2', 1, 'user.message', '{}')`); err != nil {
		t.Errorf("same seq in another session: %v", err)
	}
}

func TestEnumCheckConstraints(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	cases := []struct {
		name string
		q    string
	}{
		{"session status", `INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		                    VALUES ('sesn_bad', 'agent_1', 1, '{}', 'env_1', 'paused')`},
		{"environment kind", `INSERT INTO environments (id, name, kind, config) VALUES ('env_bad', 'e', 'hybrid', '{"type":"hybrid"}')`},
		{"environment kind/config disagreement", `INSERT INTO environments (id, name, kind, config)
		                                          VALUES ('env_bad2', 'e', 'self_hosted', '{"type":"cloud"}')`},
		{"environment config missing type", `INSERT INTO environments (id, name, kind, config)
		                                     VALUES ('env_bad3', 'e', 'cloud', '{}')`},
		{"work kind", `INSERT INTO work_items (id, environment_id, session_id, kind)
		               VALUES ('work_bad', 'env_1', 'sesn_1', 'shell_exec')`},
		{"work state", `INSERT INTO work_items (id, environment_id, session_id, kind, state)
		                VALUES ('work_bad2', 'env_1', 'sesn_1', 'tool_exec', 'running')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.q)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation {
				t.Errorf("invalid %s => %v, want check violation %s", tc.name, err, pgCheckViolation)
			}
		})
	}

	// Every legitimate enum value must be accepted — a typo in a CHECK list
	// for a value the suite never inserts would otherwise ship green.
	var valid []string
	for i, status := range []string{"idle", "running", "rescheduling", "terminated"} {
		valid = append(valid, fmt.Sprintf(
			`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
			 VALUES ('sesn_s%d', 'agent_1', 1, '{}', 'env_1', '%s')`, i, status))
	}
	// Distinct sessions per live state: only one queued/starting/active item
	// may exist per (session, kind) since 0003_work_dedup.
	for i, state := range []string{"queued", "starting", "active", "stopping", "stopped"} {
		valid = append(valid, fmt.Sprintf(
			`INSERT INTO work_items (id, environment_id, session_id, kind, state)
			 VALUES ('work_s%d', 'env_1', 'sesn_s%d', 'tool_exec', '%s')`, i, i%4, state))
	}
	valid = append(valid,
		`INSERT INTO work_items (id, environment_id, session_id, kind) VALUES ('work_mt', 'env_1', 'sesn_1', 'model_turn')`,
		`INSERT INTO environments (id, name, kind, config) VALUES ('env_sh', 'e2', 'self_hosted', '{"type":"self_hosted"}')`,
	)
	for _, q := range valid {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Errorf("valid insert rejected: %q: %v", q, err)
		}
	}
}

func TestEnvironmentConfigIsRequired(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	// The wire's environment config union always carries a type; a row
	// without a config cannot round-trip, so the column has no default.
	_, err := pool.Exec(context.Background(),
		`INSERT INTO environments (id, name, kind) VALUES ('env_nc', 'e', 'cloud')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgNotNullViolation {
		t.Errorf("environment without config => %v, want not-null violation %s", err, pgNotNullViolation)
	}
}

func TestSessionRequiresAgentVersionSnapshot(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	// (agent_id, agent_version) must point at a real immutable snapshot;
	// a dangling version would silently lose the audit trail.
	_, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_dangling', 'agent_1', 2, '{}', 'env_1')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgForeignKeyViolation {
		t.Errorf("session with dangling agent_version => %v, want FK violation %s", err, pgForeignKeyViolation)
	}
}

func TestWireRequiredTextColumnsNeverNull(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	// session.title and environment.description are required plain strings
	// on the wire and non-pointer strings in the domain; rows created
	// without them must read back as '', never NULL.
	var title, description string
	if err := pool.QueryRow(ctx, `SELECT title FROM sessions WHERE id = 'sesn_1'`).Scan(&title); err != nil {
		t.Errorf("scan sessions.title into string: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT description FROM environments WHERE id = 'env_1'`).Scan(&description); err != nil {
		t.Errorf("scan environments.description into string: %v", err)
	}
}

func TestWorkItemsSessionIndexExists(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	// work_items.session_id cascades on session delete; without an index
	// every session delete seq-scans the queue.
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'work_items' AND indexdef LIKE '%(session_id)%')`).Scan(&exists); err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	if !exists {
		t.Errorf("no index on work_items(session_id)")
	}
}

// TestKeyRotationMigrationRepairsExistingDuplicates: 0013's one-live indexes
// land on databases where the concurrent-mint race (#72) has already left a name
// or an environment holding several live credentials. Migrate runs every pending
// migration inside one transaction, so an unrepaired duplicate would fail the
// whole startup migration and take the deployment down rather than just skipping
// the index — 0013 must collapse the duplicates first, keeping the newest live
// row per slot and leaving other slots alone.
func TestKeyRotationMigrationRepairsExistingDuplicates(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	seedSessionChain(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO environments (id, name, kind, config) VALUES ('env_2', 'e2', 'cloud', '{"type":"cloud"}')`); err != nil {
		t.Fatalf("second environment: %v", err)
	}

	// Rewind to the pre-0013 schema, then stage the state the race produced.
	for _, q := range []string{
		`DROP INDEX api_keys_one_live`,
		`DROP INDEX environment_keys_one_live`,
		`DELETE FROM schema_migrations WHERE version = '0013_key_rotation_one_live.sql'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("rewind %q: %v", q, err)
		}
	}
	for i := 0; i < 3; i++ {
		created := fmt.Sprintf("2026-01-0%d", i+1)
		if _, err := pool.Exec(ctx,
			`INSERT INTO api_keys (id, name, key_hash, created_at) VALUES ($1, 'boot', $2, $3)`,
			fmt.Sprintf("apikey_dup%d", i), fmt.Sprintf("ak_hash_%d", i), created); err != nil {
			t.Fatalf("stage api_keys duplicate: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO environment_keys (id, environment_id, key_hash, created_at) VALUES ($1, 'env_1', $2, $3)`,
			fmt.Sprintf("envkey_dup%d", i), fmt.Sprintf("ek_hash_%d", i), created); err != nil {
			t.Fatalf("stage environment_keys duplicate: %v", err)
		}
	}
	// Untouched neighbours: the repair is per slot, not a global revoke.
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ('apikey_other', 'other', 'ak_other')`); err != nil {
		t.Fatalf("stage other name: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO environment_keys (id, environment_id, key_hash) VALUES ('envkey_other', 'env_2', 'ek_other')`); err != nil {
		t.Fatalf("stage other environment: %v", err)
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-applying 0013 over existing duplicates must repair them, not fail: %v", err)
	}

	// One live row per slot, and the survivor is the newest — the row a mint
	// would have left live had the race not happened.
	for _, tc := range []struct{ what, query, want string }{
		{"api_keys/boot", `SELECT id FROM api_keys WHERE name = 'boot' AND revoked_at IS NULL`, "apikey_dup2"},
		{"environment_keys/env_1", `SELECT id FROM environment_keys WHERE environment_id = 'env_1' AND revoked_at IS NULL`, "envkey_dup2"},
		{"api_keys/other", `SELECT id FROM api_keys WHERE name = 'other' AND revoked_at IS NULL`, "apikey_other"},
		{"environment_keys/env_2", `SELECT id FROM environment_keys WHERE environment_id = 'env_2' AND revoked_at IS NULL`, "envkey_other"},
	} {
		rows, err := pool.Query(ctx, tc.query)
		if err != nil {
			t.Fatalf("%s: %v", tc.what, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("%s scan: %v", tc.what, err)
			}
			ids = append(ids, id)
		}
		if len(ids) != 1 || ids[0] != tc.want {
			t.Errorf("%s live rows = %v, want exactly [%s]", tc.what, ids, tc.want)
		}
	}
}

func TestTenancyColumnsHaveSingleTenantDefaults(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	for _, table := range []string{"agents", "environments", "sessions"} {
		var org, wksp, proj string
		q := `SELECT org_id, workspace_id, project_id FROM ` + table + ` LIMIT 1`
		if err := pool.QueryRow(ctx, q).Scan(&org, &wksp, &proj); err != nil {
			t.Fatalf("%s tenancy columns: %v", table, err)
		}
		if org != "default" || wksp != "default" || proj != "default" {
			t.Errorf("%s tenancy defaults = (%s,%s,%s), want (default,default,default)", table, org, wksp, proj)
		}
	}
}

// rawPool connects without running migrations, for corrupting a database
// before pointing Migrate/Open at it.
func rawPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrateSurfacesFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("canceled context", func(t *testing.T) {
		pool := open(t, pgtest.FreshDB(t))
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := store.Migrate(canceled, pool); err == nil {
			t.Errorf("Migrate with canceled context must fail")
		}
	})

	t.Run("broken schema_migrations table", func(t *testing.T) {
		pool := rawPool(t, pgtest.FreshDB(t))
		if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (wrong text)`); err != nil {
			t.Fatalf("corrupt table: %v", err)
		}
		if err := store.Migrate(ctx, pool); err == nil {
			t.Errorf("Migrate over a schema_migrations table without a version column must fail")
		}
	})

	t.Run("conflicting object rolls back atomically", func(t *testing.T) {
		dsn := pgtest.FreshDB(t)
		pool := rawPool(t, dsn)
		if _, err := pool.Exec(ctx, `CREATE TABLE agents (id integer)`); err != nil {
			t.Fatalf("conflicting table: %v", err)
		}
		// Through Open, so its migration-error propagation is covered too.
		_, err := store.Open(ctx, dsn)
		if err == nil {
			t.Fatalf("Open over a conflicting agents table must fail")
		}
		if !strings.Contains(err.Error(), "0001_init.sql") {
			t.Errorf("error %q does not name the failing migration", err)
		}
		// Single-transaction guarantee: nothing else from the failed
		// migration may survive.
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'environments')`).Scan(&exists); err != nil {
			t.Fatalf("check tables: %v", err)
		}
		if exists {
			t.Errorf("environments table exists after failed migration; run was not atomic")
		}
	})

	t.Run("recording failure rolls back", func(t *testing.T) {
		pool := rawPool(t, pgtest.FreshDB(t))
		// version column present, but the insert violates applied_at's
		// NOT NULL because this variant has no default.
		if _, err := pool.Exec(ctx,
			`CREATE TABLE schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL)`); err != nil {
			t.Fatalf("variant table: %v", err)
		}
		if err := store.Migrate(ctx, pool); err == nil {
			t.Errorf("Migrate must fail when recording the version fails")
		}
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'agents')`).Scan(&exists); err != nil {
			t.Fatalf("check tables: %v", err)
		}
		if exists {
			t.Errorf("agents table exists after failed recording; run was not atomic")
		}
	})
}

// TestStrandedStopMigrationFinalizesExistingWindDowns: 0014 repairs the rows the
// pre-#25 state machine stranded. A graceful stop used to park a queued, starting
// or active item in stopping with nothing able to finish it, and the new code
// cannot rescue all of them on its own — a never-polled queued item carries no
// lease, so the poll's lapsed-lease finalizer never matches it. Every stopping
// row predating the migration is therefore finalized, and rows in other states
// are left exactly as they are.
func TestStrandedStopMigrationFinalizesExistingWindDowns(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	seedSessionChain(t, pool)

	if _, err := pool.Exec(ctx,
		`DELETE FROM schema_migrations WHERE version = '0014_finalize_stranded_stops.sql'`); err != nil {
		t.Fatalf("rewind 0014: %v", err)
	}
	// The two shapes the old state machine left behind — one with the lease its
	// holder claimed, one with none at all (a never-polled queued item) — plus a
	// live neighbour the repair must not touch.
	for _, q := range []string{
		`INSERT INTO work_items (id, environment_id, session_id, kind, state, stop_requested_at, lease_expires_at)
		 VALUES ('work_leased', 'env_1', 'sesn_1', 'tool_exec', 'stopping', now(), now() + interval '1 hour')`,
		`INSERT INTO work_items (id, environment_id, session_id, kind, state, stop_requested_at)
		 VALUES ('work_leaseless', 'env_1', 'sesn_1', 'tool_exec', 'stopping', now())`,
		`INSERT INTO work_items (id, environment_id, session_id, kind, state, lease_expires_at)
		 VALUES ('work_live', 'env_1', 'sesn_1', 'tool_exec', 'active', now() + interval '1 hour')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("stage %q: %v", q, err)
		}
	}

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("re-applying 0014 over stranded rows must repair them, not fail: %v", err)
	}

	for _, tc := range []struct {
		id, wantState string
		wantStopped   bool
		wantLease     bool
	}{
		{"work_leased", "stopped", true, false},
		{"work_leaseless", "stopped", true, false},
		{"work_live", "active", false, true}, // untouched: the repair is scoped to stopping
	} {
		var state string
		var stoppedAt, lease *time.Time
		if err := pool.QueryRow(ctx,
			`SELECT state, stopped_at, lease_expires_at FROM work_items WHERE id = $1`, tc.id).
			Scan(&state, &stoppedAt, &lease); err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		if state != tc.wantState || (stoppedAt != nil) != tc.wantStopped || (lease != nil) != tc.wantLease {
			t.Errorf("%s = state %q stopped_at set %v lease set %v, want %q / %v / %v",
				tc.id, state, stoppedAt != nil, lease != nil, tc.wantState, tc.wantStopped, tc.wantLease)
		}
	}
}

func TestOpenRejectsUnreachableDatabase(t *testing.T) {
	if _, err := store.Open(context.Background(), "postgres://nobody:x@127.0.0.1:1/nope?connect_timeout=1"); err == nil {
		t.Errorf("Open against a closed port must fail")
	}
	if _, err := store.Open(context.Background(), ":::not a dsn"); err == nil {
		t.Errorf("Open with a malformed DSN must fail")
	}
}
