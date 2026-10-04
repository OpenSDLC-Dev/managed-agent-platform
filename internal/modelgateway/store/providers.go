package store

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/jackc/pgx/v5"
)

const providerCols = `id, name, profile, anthropic_base_url, openai_base_url, headers, stall_timeout_ms, enabled, created_at, updated_at`

func scanProvider(row pgx.Row) (Provider, error) {
	var (
		p         Provider
		anth, oai *string
		stall     *int32
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Profile, &anth, &oai, &p.Headers, &stall, &p.Enabled, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return Provider{}, err
	}
	p.Endpoints = map[profile.Protocol]string{}
	if anth != nil {
		p.Endpoints[profile.Anthropic] = *anth
	}
	if oai != nil {
		p.Endpoints[profile.OpenAI] = *oai
	}
	if stall != nil {
		p.StallTimeout = time.Duration(*stall) * time.Millisecond
	}
	return p, nil
}

func providerNotFound(id string) error { return fail(ErrNotFound, "provider %q does not exist", id) }

func endpoint(p Provider, proto profile.Protocol) *string {
	if u, ok := p.Endpoints[proto]; ok {
		return &u
	}
	return nil
}

func headersParam(h map[string]string) map[string]string {
	if h == nil {
		return map[string]string{}
	}
	return h
}

func stallParam(d time.Duration) *int32 {
	if d == 0 {
		return nil
	}
	ms := int32(d / time.Millisecond)
	return &ms
}

// CreateProvider inserts p under a new id.
func (s *Store) CreateProvider(ctx context.Context, p Provider) (Provider, error) {
	if len(p.Endpoints) == 0 {
		return Provider{}, fail(ErrInvalid, "a provider needs at least one endpoint")
	}
	for proto := range p.Endpoints {
		if proto != profile.Anthropic && proto != profile.OpenAI {
			return Provider{}, fail(ErrInvalid, "unknown protocol %q", proto)
		}
	}
	var out Provider
	err := s.write(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = scanProvider(tx.QueryRow(ctx,
			`INSERT INTO modelgateway.providers (id, name, profile, anthropic_base_url, openai_base_url, headers, stall_timeout_ms, enabled)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+providerCols,
			newID("gwprov"), p.Name, p.Profile, endpoint(p, profile.Anthropic), endpoint(p, profile.OpenAI),
			headersParam(p.Headers), stallParam(p.StallTimeout), p.Enabled))
		return err
	})
	return out, err
}

// GetProvider reads one provider.
func (s *Store) GetProvider(ctx context.Context, id string) (Provider, error) {
	p, err := scanProvider(s.pool.QueryRow(ctx, `SELECT `+providerCols+` FROM modelgateway.providers WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Provider{}, providerNotFound(id)
	}
	return p, err
}

// ListProviders reads every provider, oldest first.
func (s *Store) ListProviders(ctx context.Context) ([]Provider, error) {
	return listProviders(ctx, s.pool)
}

func listProviders(ctx context.Context, q querier) ([]Provider, error) {
	rows, err := q.Query(ctx, `SELECT `+providerCols+` FROM modelgateway.providers ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateProvider applies fn to the provider and writes back its name, headers,
// stall timeout and enabled flag.
func (s *Store) UpdateProvider(ctx context.Context, id string, fn func(*Provider) error) (Provider, error) {
	var out Provider
	err := s.write(ctx, func(tx pgx.Tx) error {
		p, err := scanProvider(tx.QueryRow(ctx, `SELECT `+providerCols+` FROM modelgateway.providers WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return providerNotFound(id)
		}
		if err != nil {
			return err
		}
		next := p
		next.Endpoints, next.Headers = maps.Clone(p.Endpoints), maps.Clone(p.Headers)
		if err := fn(&next); err != nil {
			return err
		}
		out, err = scanProvider(tx.QueryRow(ctx,
			`UPDATE modelgateway.providers SET name = $2, headers = $3, stall_timeout_ms = $4, enabled = $5, updated_at = now()
			 WHERE id = $1 RETURNING `+providerCols,
			id, next.Name, headersParam(next.Headers), stallParam(next.StallTimeout), next.Enabled))
		return err
	})
	return out, err
}

// DeleteProvider removes a provider and its credentials. A provider with
// deployments is a conflict: its deployments go first.
func (s *Store) DeleteProvider(ctx context.Context, id string) error {
	err := s.write(ctx, func(tx pgx.Tx) error {
		deps, err := collectStrings(ctx, tx, `SELECT id FROM modelgateway.deployments WHERE provider_id = $1 ORDER BY id`, id)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return fail(ErrConflict, "provider %q has deployments (%s); delete them first", id, strings.Join(deps, ", "))
		}
		tag, err := tx.Exec(ctx, `DELETE FROM modelgateway.providers WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return providerNotFound(id)
		}
		return nil
	})
	if isForeignKeyViolation(err) {
		return fail(ErrConflict, "provider %q gained a deployment; delete it first", id)
	}
	return err
}

func collectStrings(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
