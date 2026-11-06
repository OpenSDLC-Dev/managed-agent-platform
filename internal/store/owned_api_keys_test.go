package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/store"
)

// This is the pre-0060 authentication query, deliberately not an effective-hash
// lookup. A replica can retain its prepared statement while another migrates.
const legacyKeyLookup = `SELECT id FROM api_keys
 WHERE key_hash = $1 AND status = 'active'
   AND (expires_at IS NULL OR expires_at > now())`

func requireOwnedKeyColumns(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'api_keys'
		  AND column_name IN ('owner_principal_id', 'key_hash', 'owned_key_hash')
		  AND data_type = 'text' AND is_nullable = 'YES'`).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("owned-key schema contract: found %d nullable text credential columns, want 3", count)
	}
}

func keyExec(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("key fixture SQL: %v", err)
	}
}

func requireKeyPGError(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Fatalf("operation error = %v, want SQLSTATE %s constraint %q", err, code, constraint)
	}
}

func keySnapshot(t *testing.T, db apikey.Querier, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(context.Background(), query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func seedKeyPrincipals(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	keyExec(t, pool, `INSERT INTO principals (id, issuer, subject, kind, role) VALUES
		('principal_human', 'https://idp.example', 'human', 'user', 'developer'),
		('principal_other', 'https://idp.example', 'other', 'user', 'developer'),
		('principal_sa', 'local-service-account', 'sa', 'service_account', 'developer')`)
}

func TestOwnedKeyDigestMigration(t *testing.T) {
	ctx := context.Background()
	pool := rawPool(t, pgtest.FreshDB(t))
	if err := store.MigrateThrough(ctx, pool, "0059_principal_management.sql"); err != nil {
		t.Fatal(err)
	}
	seedKeyPrincipals(t, pool)
	type credential struct {
		id, value string
		active    bool
	}
	var keys []credential
	for i, issuer := range []any{nil, "apikey_issuer", "principal_human"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			id := fmt.Sprintf("apikey_legacy_%d_%s", i, state)
			value := "fixture-" + id
			status := state
			var expiry any
			if state == "expired" {
				status, expiry = "active", "2000-01-01T00:00:00Z"
			} else if state == "inactive" {
				expiry = "2100-01-01T00:00:00Z"
			}
			keyExec(t, pool, `INSERT INTO api_keys
				(id, name, key_hash, status, expires_at, created_by, partial_key_hint)
				VALUES ($1, $1, $2, $3, $4, $5, 'fixture-hint')`, id, apikey.Hash(value), status, expiry, issuer)
			keyExec(t, pool, `INSERT INTO modelgateway.key_policies (api_key_id, aliases, rpm, tpm)
				VALUES ($1, ARRAY['fixture-model'], 7, 900)`, id)
			keys = append(keys, credential{id, value, state == "active"})
		}
	}
	const keyRows = `SELECT jsonb_agg(to_jsonb(k) - 'owner_principal_id' - 'owned_key_hash' ORDER BY id)::text FROM api_keys k`
	const policies = `SELECT jsonb_agg(to_jsonb(p) ORDER BY api_key_id)::text FROM modelgateway.key_policies p`
	beforeKeys, beforePolicies := keySnapshot(t, pool, keyRows), keySnapshot(t, pool, policies)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Conn().Prepare(ctx, "legacy_authentication", legacyKeyLookup); err != nil {
		t.Fatal(err)
	}
	checkAuthentication := func(t *testing.T) {
		t.Helper()
		for _, key := range keys {
			want := ""
			if key.active {
				want = key.id
			}
			var prepared string
			err := conn.QueryRow(ctx, "legacy_authentication", apikey.Hash(key.value)).Scan(&prepared)
			if (want == "" && !errors.Is(err, pgx.ErrNoRows)) || (want != "" && (err != nil || prepared != want)) {
				t.Errorf("prepared lookup %s = %q, %v; want %q", key.id, prepared, err, want)
			}
			if got, err := apikey.Authenticate(ctx, pool, key.value); err != nil || got != want {
				t.Errorf("ordinary lookup %s = %q, %v; want %q", key.id, got, err, want)
			}
		}
	}
	checkAuthentication(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	requireOwnedKeyColumns(t, pool)
	if got := keySnapshot(t, pool, keyRows); got != beforeKeys {
		t.Errorf("migration changed legacy credential bytes: got %s, want %s", got, beforeKeys)
	}
	if got := keySnapshot(t, pool, policies); got != beforePolicies {
		t.Errorf("migration changed legacy policies: got %s, want %s", got, beforePolicies)
	}
	var owned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE owner_principal_id IS NOT NULL OR owned_key_hash IS NOT NULL`).Scan(&owned); err != nil || owned != 0 {
		t.Fatalf("migration backfilled %d owned rows, err %v; want none", owned, err)
	}
	checkAuthentication(t)
	// The statement prepared on 0059 must also refuse newly inserted owned keys.
	for _, owner := range []string{"principal_human", "principal_sa"} {
		id := "apikey_owned_" + owner
		value := "fixture-" + id
		keyExec(t, pool, `INSERT INTO api_keys (id, name, owner_principal_id, owned_key_hash, created_by)
			VALUES ($1, $1, $2, $3, 'principal_human')`, id, owner, apikey.Hash(value))
		keys = append(keys, credential{id, value, false})
	}
	checkAuthentication(t)
}

func TestKeyCredentialShapeRejectsMalformedInserts(t *testing.T) {
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	const insert = `INSERT INTO api_keys (id, name, owner_principal_id, key_hash, owned_key_hash, created_by)
		VALUES ($1, $1, $2, $3, $4, $5)`
	for _, tc := range []struct {
		name                         string
		owner, legacy, owned, issuer any
	}{
		{"owned-plus-legacy-hash", "principal_human", "legacy", nil, "principal_human"},
		{"both-hashes", "principal_human", "legacy", "owned", "principal_human"},
		{"ownerless-owned-hash", nil, nil, "owned", "principal_human"},
		{"ownerless-both-hashes", nil, "legacy", "owned", "principal_human"},
		{"neither-hash", nil, nil, nil, nil},
		{"owned-neither-hash", "principal_human", nil, nil, "principal_human"},
		{"owned-null-issuer", "principal_human", nil, "owned", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, insert, "apikey_"+tc.name, tc.owner, tc.legacy, tc.owned, tc.issuer)
			requireKeyPGError(t, err, pgCheckViolation, "api_keys_credential_shape")
		})
	}
	t.Run("missing-owner", func(t *testing.T) {
		_, err := pool.Exec(context.Background(), insert, "apikey_missing", "principal_missing", nil, "missing-owner", "principal_human")
		requireKeyPGError(t, err, pgForeignKeyViolation, "api_keys_owner_principal_id_fkey")
	})
	// Human-issued unowned INSERTs stay valid until the replacement HTTP issuer
	// exists. This preparatory batch must not break today's issuance path.
	for i, issuer := range []any{nil, "apikey_issuer", "principal_human"} {
		t.Run(fmt.Sprintf("valid-unowned-%d", i), func(t *testing.T) {
			id := fmt.Sprintf("apikey_valid_%d", i)
			keyExec(t, pool, insert, id, nil, "fixture-"+id, nil, issuer)
		})
	}
	for _, owner := range []string{"principal_human", "principal_sa"} {
		t.Run("valid-owned-"+owner, func(t *testing.T) {
			keyExec(t, pool, insert, "apikey_"+owner, owner, nil, "fixture-"+owner, "principal_human")
		})
	}
}

// state is a display state: expired remains an active row with a past expiry.
func insertKeyFixture(t *testing.T, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id, owner, state string) {
	t.Helper()
	var ownerID, legacyHash, ownedHash, expiry any
	if owner == "" {
		legacyHash = apikey.Hash("fixture-" + id)
	} else {
		ownerID, ownedHash = owner, apikey.Hash("fixture-"+id)
	}
	if state == "expired" {
		state, expiry = "active", "2000-01-01T00:00:00Z"
	}
	keyExec(t, db, `INSERT INTO api_keys
		(id,name,owner_principal_id,key_hash,owned_key_hash,created_by,status,expires_at)
		VALUES ($1,$1,$2,$3,$4,'principal_human',$5,$6)`, id, ownerID, legacyHash, ownedHash, state, expiry)
}

func TestOwnedKeysAreInvisibleToLegacyReaders(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	for _, owner := range []string{"", "principal_human", "principal_sa"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			t.Run(owner+"/"+state, func(t *testing.T) {
				id := "apikey_" + owner + "_" + state
				insertKeyFixture(t, pool, id, owner, state)
				want := ""
				if owner == "" && state == "active" {
					want = id
				}
				if got, err := apikey.Authenticate(ctx, pool, "fixture-"+id); err != nil || got != want {
					t.Errorf("legacy Authenticate = %q, %v; want %q", got, err, want)
				}
				var got string
				err := pool.QueryRow(ctx, legacyKeyLookup, apikey.Hash("fixture-"+id)).Scan(&got)
				if (want == "" && !errors.Is(err, pgx.ErrNoRows)) || (want != "" && (err != nil || got != want)) {
					t.Errorf("legacy SQL = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
}

func TestKeyDigestLanesShareUniqueness(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	for _, state := range []string{"active", "inactive", "archived", "expired"} {
		for _, ownedFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owned-first-%t", state, ownedFirst), func(t *testing.T) {
				id := fmt.Sprintf("apikey_%s_%t", state, ownedFirst)
				owner := ""
				if ownedFirst {
					owner = "principal_human"
				}
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				insertKeyFixture(t, tx, id, owner, state)
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				other, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Rollback(ctx)
				query := `INSERT INTO api_keys (id,name,key_hash) VALUES ($1,$1,$2)`
				if !ownedFirst {
					query = `INSERT INTO api_keys (id,name,owner_principal_id,owned_key_hash,created_by)
						VALUES ($1,$1,'principal_sa',$2,'principal_human')`
				}
				_, err = other.Exec(ctx, query, id+"_duplicate", apikey.Hash("fixture-"+id))
				requireKeyPGError(t, err, pgUniqueViolation, "api_keys_authentication_hash_key")
			})
		}
	}
	// The legacy ON CONFLICT(key_hash) arbiter must survive the new index.
	keyExec(t, pool, `INSERT INTO api_keys (id,name,key_hash) VALUES ('apikey_seed','seed','seed-hash')
		ON CONFLICT(key_hash) DO UPDATE SET name=EXCLUDED.name`)
	keyExec(t, pool, `INSERT INTO api_keys (id,name,key_hash) VALUES ('apikey_seed_again','renamed','seed-hash')
		ON CONFLICT(key_hash) DO UPDATE SET name=EXCLUDED.name`)
}

func TestKeyCredentialIdentityCannotChange(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	insertKeyFixture(t, pool, "apikey_owned", "principal_human", "active")
	insertKeyFixture(t, pool, "apikey_legacy", "", "active")
	const snapshot = `SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM api_keys k`
	before := keySnapshot(t, pool, snapshot)
	for _, tc := range []struct{ name, sql string }{
		{"owner-transfer", `UPDATE api_keys SET owner_principal_id='principal_sa' WHERE id='apikey_owned'`},
		{"owned-digest-rewrite", `UPDATE api_keys SET owned_key_hash='different' WHERE id='apikey_owned'`},
		{"legacy-digest-rewrite", `UPDATE api_keys SET key_hash='different' WHERE id='apikey_legacy'`},
		{"owned-to-unowned", `UPDATE api_keys SET key_hash=owned_key_hash,owned_key_hash=NULL,owner_principal_id=NULL WHERE id='apikey_owned'`},
		{"unowned-to-owned", `UPDATE api_keys SET owned_key_hash=key_hash,key_hash=NULL,owner_principal_id='principal_human' WHERE id='apikey_legacy'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, tc.sql)
			_ = tx.Rollback(ctx)
			requireKeyPGError(t, err, pgCheckViolation, "api_keys_credential_immutable")
			if got := keySnapshot(t, pool, snapshot); got != before {
				t.Errorf("rejected identity change altered credential bytes: got %s, want %s", got, before)
			}
		})
	}
	keyExec(t, pool, `UPDATE api_keys SET name='renamed',status='inactive',
		owner_principal_id=owner_principal_id,key_hash=key_hash,owned_key_hash=owned_key_hash`)
	if got := keySnapshot(t, pool, `SELECT count(*)::text FROM api_keys WHERE name='renamed' AND status='inactive'`); got != "2" {
		t.Errorf("mutable name/status controls updated %s rows, want 2", got)
	}
}

// The SQL and explicit isolation/lock order are the existing member and service
// account removal paths. A future owned issuer must use this same fence.
func deactivateKeyOwner(t *testing.T, pool *pgxpool.Pool, id string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	keyExec(t, tx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`)
	keyExec(t, tx, `UPDATE principals SET status='deactivated',deactivated_at=clock_timestamp(),deactivated_by=$2,updated_at=clock_timestamp(),updated_by=$2 WHERE id=$1`, id, "principal_other")
	return tx
}

func TestPrincipalDeactivationArchivesOnlyOwnedKeys(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	for _, owner := range []string{"principal_human", "principal_sa", "principal_other", ""} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			insertKeyFixture(t, pool, "apikey_"+owner+"_"+state, owner, state)
		}
	}
	keyExec(t, pool, `INSERT INTO environments (id,name,kind,config) VALUES ('env_owned_test','owned test','cloud','{"type":"cloud"}')`)
	keyExec(t, pool, `INSERT INTO environment_keys (id,environment_id,key_hash) VALUES ('envkey_control','env_owned_test','environment-hash')`)
	const rows = `SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM api_keys k`
	const principals = `SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM principals p`
	const envRows = `SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM environment_keys k`
	before, beforePrincipals, beforeEnv := keySnapshot(t, pool, rows), keySnapshot(t, pool, principals), keySnapshot(t, pool, envRows)
	checkArchived := func(t *testing.T, db apikey.Querier, owner string) {
		t.Helper()
		if got := keySnapshot(t, db, `SELECT count(*)::text FROM api_keys WHERE owner_principal_id=$1 AND status='archived'`, owner); got != "4" {
			t.Errorf("deactivation archived %s keys for %s, want 4 (active, inactive, archived, expired)", got, owner)
		}
	}
	rollback := deactivateKeyOwner(t, pool, "principal_human")
	checkArchived(t, rollback, "principal_human")
	if got := keySnapshot(t, pool, rows); got != before {
		t.Error("another connection observed key archival before removal committed")
	}
	if err := rollback.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if keySnapshot(t, pool, rows) != before || keySnapshot(t, pool, principals) != beforePrincipals {
		t.Error("removal rollback did not restore principal and key bytes")
	}
	const unaffected = `SELECT jsonb_agg(to_jsonb(k) ORDER BY id)::text FROM api_keys k WHERE owner_principal_id IS DISTINCT FROM $1`
	otherKeys := keySnapshot(t, pool, unaffected, "principal_human")
	removeHuman := deactivateKeyOwner(t, pool, "principal_human")
	if err := removeHuman.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	checkArchived(t, pool, "principal_human")
	if got := keySnapshot(t, pool, unaffected, "principal_human"); got != otherKeys {
		t.Error("removing issuer changed other owners' or unowned keys")
	}
	keyExec(t, pool, `UPDATE principals SET status='active' WHERE id='principal_human'`)
	checkArchived(t, pool, "principal_human")
	otherKeys = keySnapshot(t, pool, unaffected, "principal_sa")
	removeSA := deactivateKeyOwner(t, pool, "principal_sa")
	checkArchived(t, removeSA, "principal_sa")
	if err := removeSA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	keyExec(t, pool, `UPDATE principals SET status='active' WHERE id='principal_sa'`)
	checkArchived(t, pool, "principal_sa")
	if keySnapshot(t, pool, unaffected, "principal_sa") != otherKeys || keySnapshot(t, pool, envRows) != beforeEnv {
		t.Error("service-account removal or restoration changed unrelated credentials")
	}
}

func TestOwnedKeysCannotReceiveLegacyPolicies(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	insertKeyFixture(t, pool, "apikey_legacy", "", "active")
	insertKeyFixture(t, pool, "apikey_other_legacy", "", "inactive")
	keyExec(t, pool, `INSERT INTO modelgateway.key_policies (api_key_id,aliases,rpm) VALUES ('apikey_legacy',ARRAY['old'],5)`)
	keyExec(t, pool, `INSERT INTO modelgateway.key_policies (api_key_id,aliases,rpm) VALUES ('apikey_legacy',ARRAY['replacement'],6)
		ON CONFLICT(api_key_id) DO UPDATE SET aliases=EXCLUDED.aliases,rpm=EXCLUDED.rpm`)
	keyExec(t, pool, `UPDATE modelgateway.key_policies SET api_key_id='apikey_other_legacy',tpm=100`)
	if got := keySnapshot(t, pool, `SELECT aliases[1] || ':' || rpm || ':' || tpm FROM modelgateway.key_policies WHERE api_key_id='apikey_other_legacy'`); got != "replacement:6:100" {
		t.Fatalf("unowned policy replacement/target-change control = %q", got)
	}
	const policies = `SELECT jsonb_agg(to_jsonb(p) ORDER BY api_key_id)::text FROM modelgateway.key_policies p`
	before := keySnapshot(t, pool, policies)
	for _, owner := range []string{"principal_human", "principal_sa"} {
		for _, state := range []string{"active", "inactive", "archived", "expired"} {
			id := "apikey_" + owner + "_" + state
			insertKeyFixture(t, pool, id, owner, state)
			for _, tc := range []struct{ name, sql string }{
				{"insert", `INSERT INTO modelgateway.key_policies (api_key_id) VALUES ($1)`},
				{"upsert", `INSERT INTO modelgateway.key_policies (api_key_id,aliases) VALUES ($1,ARRAY['new']) ON CONFLICT(api_key_id) DO UPDATE SET aliases=EXCLUDED.aliases`},
				{"target-change", `UPDATE modelgateway.key_policies SET api_key_id=$1 WHERE api_key_id='apikey_other_legacy'`},
			} {
				t.Run(owner+"/"+state+"/"+tc.name, func(t *testing.T) {
					tx, err := pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(ctx, tc.sql, id)
					_ = tx.Rollback(ctx)
					requireKeyPGError(t, err, pgCheckViolation, "modelgateway_key_policy_unowned")
					if keySnapshot(t, pool, policies) != before {
						t.Error("refused owned policy write changed policy bytes")
					}
				})
			}
		}
	}
	t.Run("missing-target", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO modelgateway.key_policies (api_key_id) VALUES ('apikey_missing')`)
		requireKeyPGError(t, err, pgForeignKeyViolation, "modelgateway_key_policy_target")
	})
}

func TestAnUncommittedOwnedPolicyTargetIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := open(t, pgtest.FreshDB(t))
	requireOwnedKeyColumns(t, pool)
	seedKeyPrincipals(t, pool)
	issuer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer issuer.Rollback(ctx)
	insertKeyFixture(t, issuer, "apikey_pending", "principal_sa", "active")
	// Acquire explicitly: the assertion must run on another backend, while the
	// target INSERT remains uncommitted. The deadline bounds a mistaken FK wait.
	writer, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	if writer.Conn().PgConn().PID() == issuer.Conn().PgConn().PID() {
		t.Fatal("policy writer must use a separate backend")
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = writer.Exec(bounded, `INSERT INTO modelgateway.key_policies (api_key_id) VALUES ('apikey_pending')`)
	// A generic FK failure would not prove the BEFORE guard saw and refused the
	// invisible target. Its own constraint name distinguishes that contract.
	requireKeyPGError(t, err, pgForeignKeyViolation, "modelgateway_key_policy_target")
	if err := issuer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO modelgateway.key_policies (api_key_id) VALUES ('apikey_pending')`)
	requireKeyPGError(t, err, pgCheckViolation, "modelgateway_key_policy_unowned")
	if got := keySnapshot(t, pool, `SELECT count(*)::text FROM modelgateway.key_policies`); got != "0" {
		t.Errorf("owned target acquired %s policies, want none", got)
	}
}
