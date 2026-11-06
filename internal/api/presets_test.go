package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func presetRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var rows string
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(jsonb_agg(to_jsonb(k) ORDER BY id), '[]'::jsonb)::text FROM api_keys k`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func ownedPreset(t *testing.T, pool *pgxpool.Pool, name, status string, expired bool) string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO principals (id,issuer,subject,role) VALUES ('principal_owner','preset-test','owner','developer')`); err != nil {
		t.Fatal(err)
	}
	const raw = "synthetic-owned-preset-secret"
	if _, err := pool.Exec(ctx, `INSERT INTO api_keys (id,name,owner_principal_id,owned_key_hash,created_by,status,expires_at,partial_key_hint)
		VALUES ('apikey_owned',$1,'principal_owner',$2,'principal_owner',$3,
		CASE WHEN $4 THEN now() - interval '1 hour' ELSE now() + interval '1 hour' END,'...cret')`,
		name, apikey.Hash(raw), status, expired); err != nil {
		t.Fatal(err)
	}
	return raw
}

// Removing the owned preflight must yield a raw uniqueness error, not an
// operator-safe refusal. Neither that refusal nor old seed SQL may rotate away
// the incumbent or change any field of the owned row.
func TestOwnedPresetValuesAreNeverAdopted(t *testing.T) {
	for _, name := range []string{"bootstrap", "brain"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				pool := pgtest.NewPool(t)
				status := state
				if state == "expired" {
					status = "active"
				}
				raw := ownedPreset(t, pool, name, status, state == "expired")
				if err := api.EnsureAPIKey(context.Background(), pool, name, "synthetic-incumbent"); err != nil {
					t.Fatal(err)
				}
				before := presetRows(t, pool)
				err := api.EnsureAPIKey(context.Background(), pool, name, raw)
				var pgErr *pgconn.PgError
				if !errors.Is(err, apikey.ErrOwnedPreset) || errors.As(err, &pgErr) || !strings.Contains(err.Error(), "principal-owned API key") {
					t.Errorf("want an operator-safe principal-owned API key refusal, got %v", err)
				}
				if err != nil && (strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), apikey.Hash(raw))) {
					t.Error("preset error disclosed the offered credential or digest")
				}
				if presetRows(t, pool) != before {
					t.Error("owned refusal changed the owned row or incumbent")
				}
			})
		}
	}
}

func TestPresetPreparationChecksBothBeforeWriting(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	raw := ownedPreset(t, pool, "brain", "active", false)
	if _, err := pool.Exec(ctx, `CREATE FUNCTION forbid_preset_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'preset write reached before ownership check'; END $$;
		CREATE TRIGGER forbid_preset_write BEFORE INSERT OR UPDATE ON api_keys
		FOR EACH ROW EXECUTE FUNCTION forbid_preset_write()`); err != nil {
		t.Fatal(err)
	}
	if err := api.EnsureManagementPresets(ctx, pool, "new-bootstrap", raw); !errors.Is(err, apikey.ErrOwnedPreset) {
		t.Fatalf("brain ownership was not checked before bootstrap write: %v", err)
	}
}

func TestPresetPreparationRollsBackWrites(t *testing.T) {
	ctx := context.Background()
	for _, brain := range []string{"replacement-brain", ""} {
		t.Run("brain="+brain, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			if err := api.EnsureManagementPresets(ctx, pool, "old-bootstrap", "old-brain"); err != nil {
				t.Fatal(err)
			}
			before := presetRows(t, pool)
			// Fail after bootstrap's writes, for both brain rotation and retirement.
			if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_brain_write() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN IF NEW.name = 'brain' THEN RAISE EXCEPTION 'test brain write failure'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER reject_brain_write BEFORE INSERT OR UPDATE ON api_keys
				FOR EACH ROW EXECUTE FUNCTION reject_brain_write()`); err != nil {
				t.Fatal(err)
			}
			if err := api.EnsureManagementPresets(ctx, pool, "replacement-bootstrap", brain); err == nil || !strings.Contains(err.Error(), "test brain write failure") {
				t.Fatalf("missing injected brain failure: %v", err)
			}
			if presetRows(t, pool) != before {
				t.Error("failed brain write committed bootstrap changes")
			}
		})
	}
}

func TestPresetPreparationPreservesUnownedLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	pgtest.DefaultRepeatableRead(t, pool)
	// The install's default must not change this transaction's isolation level.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION require_preset_read_committed() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF current_setting('transaction_isolation') <> 'read committed' THEN
		RAISE EXCEPTION 'preset transaction must be read committed'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER require_preset_read_committed BEFORE INSERT OR UPDATE ON api_keys
		FOR EACH ROW EXECUTE FUNCTION require_preset_read_committed()`); err != nil {
		t.Fatal(err)
	}
	if err := api.EnsureManagementPresets(ctx, pool, "first-bootstrap", "first-brain"); err != nil {
		t.Fatal(err)
	}
	// Expired, archived legacy issued rows still support adoption. The storage
	// guard intentionally admits an unowned principal-issued row in this batch.
	for _, name := range []string{"bootstrap", "brain"} {
		err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO api_keys (id,name,key_hash,created_by,status,expires_at)
				VALUES ($1,$1,$2,'principal_legacy_issuer','archived',now() - interval '1 hour')`, "apikey_issued_"+name, apikey.Hash("adopted-"+name))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := api.EnsureManagementPresets(ctx, pool, "adopted-bootstrap", "adopted-brain"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bootstrap", "brain"} {
		var valid bool
		if err := pool.QueryRow(ctx, `SELECT name=$2 AND status='active' AND created_by IS NULL AND expires_at IS NULL
			FROM api_keys WHERE id=$1`, "apikey_issued_"+name, name).Scan(&valid); err != nil || !valid {
			t.Fatalf("unowned adoption changed: valid=%t err=%v", valid, err)
		}
		if id, err := apikey.Authenticate(ctx, pool, "adopted-"+name); err != nil || id != "apikey_issued_"+name {
			t.Fatalf("adopted key did not authenticate: id=%q err=%v", id, err)
		}
		if id, err := apikey.Authenticate(ctx, pool, "first-"+name); err != nil || id != "" {
			t.Fatalf("rotation left prior key usable: id=%q err=%v", id, err)
		}
	}
	if err := api.EnsureManagementPresets(ctx, pool, "adopted-bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	if id, err := apikey.Authenticate(ctx, pool, "adopted-brain"); err != nil || id != "" {
		t.Fatalf("unset brain still authenticates: id=%q err=%v", id, err)
	}
}

// This is a compatibility control for the already-installed database guard,
// not a second feature RED. Replay the complete old seed transaction, including
// its archive-before-upsert ordering and legacy key_hash conflict arbiter.
func TestLegacySeedCannotAliasAnOwnedDigest(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"bootstrap", "brain"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			t.Run(name+"/"+state, func(t *testing.T) {
				pool := pgtest.NewPool(t)
				status := state
				if state == "expired" {
					status = "active"
				}
				raw := ownedPreset(t, pool, name, status, state == "expired")
				if err := api.EnsureAPIKey(ctx, pool, name, "synthetic-incumbent"); err != nil {
					t.Fatal(err)
				}
				before := presetRows(t, pool)
				err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					var issuer *string
					var status string
					err := tx.QueryRow(ctx, `SELECT created_by, status FROM api_keys WHERE key_hash = $1`, apikey.Hash(raw)).Scan(&issuer, &status)
					if err != nil && !errors.Is(err, pgx.ErrNoRows) {
						return err
					}
					if _, err := tx.Exec(ctx, `UPDATE api_keys SET status = 'archived'
					WHERE name = $1 AND key_hash <> $2 AND status = 'active' AND created_by IS NULL`, name, apikey.Hash(raw)); err != nil {
						return err
					}
					_, err = tx.Exec(ctx, `INSERT INTO api_keys (id, name, key_hash, partial_key_hint) VALUES ($1, $2, $3, $4)
					ON CONFLICT (key_hash) DO UPDATE
					SET status = 'active', name = EXCLUDED.name, partial_key_hint = EXCLUDED.partial_key_hint,
					    created_by = NULL, expires_at = NULL`, "apikey_old_seed", name, apikey.Hash(raw), "...cret")
					return err
				})
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "api_keys_authentication_hash_key" {
					t.Fatalf("old seed did not hit the global digest arbiter: %v", err)
				}
				if presetRows(t, pool) != before {
					t.Error("old seed did not roll back its archive and insert together")
				}
			})
		}
	}
}
