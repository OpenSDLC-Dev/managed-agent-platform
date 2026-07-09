package store_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

const (
	pgUniqueViolation = "23505"
	pgCheckViolation  = "23514"
)

func open(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSessionChain inserts the minimal FK chain (agent -> environment ->
// session) so tests can exercise child tables.
func seedSessionChain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO agents (id, name, spec) VALUES ('agent_1', 'a', '{}')`,
		`INSERT INTO environments (id, name, kind) VALUES ('env_1', 'e', 'cloud')`,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id)
		 VALUES ('sesn_1', 'agent_1', 1, '{}', 'env_1')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
}

func TestOpenMigratesFreshDatabase(t *testing.T) {
	pool := open(t, freshDB(t))
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
	} {
		if !slices.Contains(tables, want) {
			t.Errorf("table %q missing after migration; have %v", want, tables)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", applied)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := open(t, freshDB(t))
	ctx := context.Background()

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Errorf("schema_migrations rows after re-run = %d, want 1", applied)
	}
}

func TestConcurrentMigratorsDoNotConflict(t *testing.T) {
	// Several binaries (controlplane, brain, executor) may Open the same
	// database at startup simultaneously; the advisory lock must serialize
	// them onto one successful migration run.
	dsn := freshDB(t)
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
	pool := open(t, freshDB(t))
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
	pool := open(t, freshDB(t))
	ctx := context.Background()
	seedSessionChain(t, pool)

	cases := []struct {
		name string
		q    string
	}{
		{"session status", `INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		                    VALUES ('sesn_bad', 'agent_1', 1, '{}', 'env_1', 'paused')`},
		{"environment kind", `INSERT INTO environments (id, name, kind) VALUES ('env_bad', 'e', 'hybrid')`},
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

	// The legitimate enum values must all be accepted.
	valid := []string{
		`INSERT INTO work_items (id, environment_id, session_id, kind) VALUES ('work_1', 'env_1', 'sesn_1', 'model_turn')`,
		`INSERT INTO work_items (id, environment_id, session_id, kind, state) VALUES ('work_2', 'env_1', 'sesn_1', 'tool_exec', 'active')`,
		`INSERT INTO environments (id, name, kind) VALUES ('env_sh', 'e2', 'self_hosted')`,
		`INSERT INTO sessions (id, agent_id, agent_version, resolved_agent, environment_id, status)
		 VALUES ('sesn_r', 'agent_1', 1, '{}', 'env_1', 'rescheduling')`,
	}
	for _, q := range valid {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Errorf("valid insert rejected: %q: %v", q, err)
		}
	}
}

func TestTenancyColumnsHaveSingleTenantDefaults(t *testing.T) {
	pool := open(t, freshDB(t))
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
		pool := open(t, freshDB(t))
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := store.Migrate(canceled, pool); err == nil {
			t.Errorf("Migrate with canceled context must fail")
		}
	})

	t.Run("broken schema_migrations table", func(t *testing.T) {
		pool := rawPool(t, freshDB(t))
		if _, err := pool.Exec(ctx, `CREATE TABLE schema_migrations (wrong text)`); err != nil {
			t.Fatalf("corrupt table: %v", err)
		}
		if err := store.Migrate(ctx, pool); err == nil {
			t.Errorf("Migrate over a schema_migrations table without a version column must fail")
		}
	})

	t.Run("conflicting object rolls back atomically", func(t *testing.T) {
		pool := rawPool(t, freshDB(t))
		if _, err := pool.Exec(ctx, `CREATE TABLE agents (id integer)`); err != nil {
			t.Fatalf("conflicting table: %v", err)
		}
		err := store.Migrate(ctx, pool)
		if err == nil {
			t.Fatalf("Migrate over a conflicting agents table must fail")
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
		pool := rawPool(t, freshDB(t))
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

func TestOpenFailsWhenMigrationFails(t *testing.T) {
	ctx := context.Background()
	dsn := freshDB(t)
	pool := rawPool(t, dsn)
	if _, err := pool.Exec(ctx, `CREATE TABLE agents (id integer)`); err != nil {
		t.Fatalf("conflicting table: %v", err)
	}
	if _, err := store.Open(ctx, dsn); err == nil {
		t.Errorf("Open must surface a failed migration")
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
