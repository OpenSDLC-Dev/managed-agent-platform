package store

import (
	"context"
	"errors"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/jackc/pgx/v5"
)

const credentialCols = `id, provider_id, kind, ciphertext, key_id, last_four, protocols, weight, enabled, created_at, updated_at`

func scanCredential(row pgx.Row) (Credential, error) {
	var (
		c      Credential
		protos []string
	)
	if err := row.Scan(&c.ID, &c.ProviderID, &c.Kind, &c.Ciphertext, &c.KeyID, &c.LastFour, &protos, &c.Weight, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Credential{}, err
	}
	for _, p := range protos {
		c.Protocols = append(c.Protocols, profile.Protocol(p))
	}
	return c, nil
}

func credentialNotFound(id string) error {
	return fail(ErrNotFound, "credential %q does not exist", id)
}

// lockProvider reads a provider and holds it against deletion until the
// transaction ends.
func lockProvider(ctx context.Context, tx pgx.Tx, id string) (Provider, error) {
	p, err := scanProvider(tx.QueryRow(ctx, `SELECT `+providerCols+` FROM modelgateway.providers WHERE id = $1 FOR SHARE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, providerNotFound(id)
	}
	return p, err
}

// credentialProtocols is want, or every protocol p has an endpoint for when
// want is nil, refusing a protocol p has none for. An empty, non-nil want is
// refused rather than read as nil: it cannot mean both "none" and "all".
func credentialProtocols(p Provider, want []profile.Protocol) ([]profile.Protocol, error) {
	if want != nil && len(want) == 0 {
		return nil, fail(ErrInvalid, "protocols: an empty list names no protocol; leave it out for every protocol the provider has an endpoint for")
	}
	if want == nil {
		for proto := range p.Endpoints {
			want = append(want, proto)
		}
	}
	for _, proto := range want {
		if _, ok := p.Endpoints[proto]; !ok {
			return nil, fail(ErrInvalid, "provider %q has no %s endpoint, so its keys cannot be used on %s", p.ID, proto, proto)
		}
	}
	return sortedProtocols(want), nil
}

// CreateCredential inserts c under a new id. A missing provider is not found,
// since a credential lives under its provider's path.
func (s *Store) CreateCredential(ctx context.Context, c Credential) (Credential, error) {
	if c.Kind == "" {
		c.Kind = "api_key"
	}
	var out Credential
	err := s.write(ctx, func(tx pgx.Tx) error {
		p, err := lockProvider(ctx, tx, c.ProviderID)
		if err != nil {
			return err
		}
		protos, err := credentialProtocols(p, c.Protocols)
		if err != nil {
			return err
		}
		out, err = scanCredential(tx.QueryRow(ctx,
			`INSERT INTO modelgateway.credentials (id, provider_id, kind, ciphertext, key_id, last_four, protocols, weight, enabled)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+credentialCols,
			newID("gwcred"), c.ProviderID, c.Kind, c.Ciphertext, c.KeyID, c.LastFour, protocolStrings(protos), c.Weight, c.Enabled))
		return err
	})
	return out, err
}

// GetCredential reads one of a provider's credentials.
func (s *Store) GetCredential(ctx context.Context, providerID, id string) (Credential, error) {
	c, err := scanCredential(s.pool.QueryRow(ctx,
		`SELECT `+credentialCols+` FROM modelgateway.credentials WHERE id = $1 AND provider_id = $2`, id, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, credentialNotFound(id)
	}
	return c, err
}

// ListCredentials reads a provider's credentials, oldest first.
func (s *Store) ListCredentials(ctx context.Context, providerID string) ([]Credential, error) {
	return listCredentials(ctx, s.pool, `WHERE provider_id = $1`, providerID)
}

func listCredentials(ctx context.Context, q querier, where string, args ...any) ([]Credential, error) {
	rows, err := q.Query(ctx, `SELECT `+credentialCols+` FROM modelgateway.credentials `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCredential applies fn to the credential and writes back its
// protocols, weight and enabled flag. The key itself never changes: rotating
// one is adding the new key and deleting the old.
func (s *Store) UpdateCredential(ctx context.Context, providerID, id string, fn func(*Credential) error) (Credential, error) {
	var out Credential
	err := s.write(ctx, func(tx pgx.Tx) error {
		// The provider first, as DeleteProvider takes it before the
		// credentials it cascades to: one lock order, no deadlock.
		p, err := lockProvider(ctx, tx, providerID)
		if errors.Is(err, ErrNotFound) {
			return credentialNotFound(id)
		}
		if err != nil {
			return err
		}
		c, err := scanCredential(tx.QueryRow(ctx,
			`SELECT `+credentialCols+` FROM modelgateway.credentials WHERE id = $1 AND provider_id = $2 FOR UPDATE`, id, providerID))
		if errors.Is(err, pgx.ErrNoRows) {
			return credentialNotFound(id)
		}
		if err != nil {
			return err
		}
		next := c
		next.Protocols = slices.Clone(c.Protocols)
		if err := fn(&next); err != nil {
			return err
		}
		protos, err := credentialProtocols(p, next.Protocols)
		if err != nil {
			return err
		}
		out, err = scanCredential(tx.QueryRow(ctx,
			`UPDATE modelgateway.credentials SET protocols = $2, weight = $3, enabled = $4, updated_at = now()
			 WHERE id = $1 RETURNING `+credentialCols,
			id, protocolStrings(protos), next.Weight, next.Enabled))
		return err
	})
	return out, err
}

// DeleteCredential removes one of a provider's credentials.
func (s *Store) DeleteCredential(ctx context.Context, providerID, id string) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM modelgateway.credentials WHERE id = $1 AND provider_id = $2`, id, providerID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return credentialNotFound(id)
		}
		return nil
	})
}
