package store

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
)

const keyPolicyCols = `api_key_id, aliases, rpm, tpm, created_at, updated_at`

func scanKeyPolicy(row pgx.Row) (KeyPolicy, error) {
	var k KeyPolicy
	err := row.Scan(&k.APIKeyID, &k.Aliases, &k.RPM, &k.TPM, &k.CreatedAt, &k.UpdatedAt)
	return k, err
}

func keyPolicyNotFound(id string) error {
	return fail(ErrNotFound, "api key %q has no model grant", id)
}

// PutKeyPolicy writes k whole, creating it or replacing the key's grant. The
// key must exist in api_keys, which the platform owns, and every alias the
// grant names must exist.
func (s *Store) PutKeyPolicy(ctx context.Context, k KeyPolicy) (KeyPolicy, error) {
	var out KeyPolicy
	err := s.write(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys WHERE id = $1)`, k.APIKeyID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fail(ErrNotFound, "api key %q does not exist", k.APIKeyID)
		}
		if k.Aliases != nil {
			found, err := collectStrings(ctx, tx, `SELECT name FROM modelgateway.aliases WHERE name = ANY($1) FOR SHARE`, k.Aliases)
			if err != nil {
				return err
			}
			for _, name := range k.Aliases {
				if !slices.Contains(found, name) {
					return fail(ErrInvalid, "alias %q does not exist", name)
				}
			}
		}
		var err error
		out, err = scanKeyPolicy(tx.QueryRow(ctx,
			`INSERT INTO modelgateway.key_policies (api_key_id, aliases, rpm, tpm) VALUES ($1, $2, $3, $4)
			 ON CONFLICT (api_key_id) DO UPDATE
			   SET aliases = EXCLUDED.aliases, rpm = EXCLUDED.rpm, tpm = EXCLUDED.tpm, updated_at = now()
			 RETURNING `+keyPolicyCols,
			k.APIKeyID, k.Aliases, k.RPM, k.TPM))
		return err
	})
	if isForeignKeyViolation(err) {
		return KeyPolicy{}, fail(ErrNotFound, "api key %q does not exist", k.APIKeyID)
	}
	return out, err
}

// GetKeyPolicy reads one key's grant.
func (s *Store) GetKeyPolicy(ctx context.Context, apiKeyID string) (KeyPolicy, error) {
	k, err := scanKeyPolicy(s.pool.QueryRow(ctx, `SELECT `+keyPolicyCols+` FROM modelgateway.key_policies WHERE api_key_id = $1`, apiKeyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyPolicy{}, keyPolicyNotFound(apiKeyID)
	}
	return k, err
}

// ListKeyPolicies reads every grant, oldest first.
func (s *Store) ListKeyPolicies(ctx context.Context) ([]KeyPolicy, error) {
	return listKeyPolicies(ctx, s.pool)
}

func listKeyPolicies(ctx context.Context, q querier) ([]KeyPolicy, error) {
	rows, err := q.Query(ctx, `SELECT `+keyPolicyCols+` FROM modelgateway.key_policies ORDER BY created_at, api_key_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyPolicy
	for rows.Next() {
		k, err := scanKeyPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteKeyPolicy revokes a key's grant.
func (s *Store) DeleteKeyPolicy(ctx context.Context, apiKeyID string) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM modelgateway.key_policies WHERE api_key_id = $1`, apiKeyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return keyPolicyNotFound(apiKeyID)
		}
		return nil
	})
}

// Load reads every row in one repeatable-read snapshot, so a catalog never
// sees half of a write.
func (s *Store) Load(ctx context.Context) (Config, error) {
	var cfg Config
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		if cfg.Providers, err = listProviders(ctx, tx); err != nil {
			return err
		}
		if cfg.Credentials, err = listCredentials(ctx, tx, ""); err != nil {
			return err
		}
		if cfg.Deployments, err = listDeployments(ctx, tx); err != nil {
			return err
		}
		if cfg.Aliases, err = listAliases(ctx, tx); err != nil {
			return err
		}
		cfg.KeyPolicies, err = listKeyPolicies(ctx, tx)
		return err
	})
	return cfg, err
}
