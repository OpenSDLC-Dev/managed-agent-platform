package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

const deploymentCols = `id, provider_id, upstream_model, kind, display_name, capabilities,
	price_input, price_output, price_cache_write, price_cache_read, price_currency, price_note, enabled, created_at, updated_at`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.ProviderID, &d.UpstreamModel, &d.Kind, &d.DisplayName, &d.Capabilities,
		&d.Prices.Input, &d.Prices.Output, &d.Prices.CacheWrite, &d.Prices.CacheRead, &d.PriceCurrency, &d.PriceNote, &d.Enabled, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

func deploymentNotFound(id string) error {
	return fail(ErrNotFound, "deployment %q does not exist", id)
}

// CreateDeployment inserts d under a new id. A missing provider is invalid
// input rather than not found: the deployment is what the call names.
func (s *Store) CreateDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	var out Deployment
	err := s.write(ctx, func(tx pgx.Tx) error {
		if _, err := lockProvider(ctx, tx, d.ProviderID); errors.Is(err, ErrNotFound) {
			return fail(ErrInvalid, "provider %q does not exist", d.ProviderID)
		} else if err != nil {
			return err
		}
		var err error
		out, err = scanDeployment(tx.QueryRow(ctx,
			`INSERT INTO modelgateway.deployments (id, provider_id, upstream_model, kind, display_name, capabilities,
			   price_input, price_output, price_cache_write, price_cache_read, price_currency, price_note, enabled)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING `+deploymentCols,
			newID("gwdep"), d.ProviderID, d.UpstreamModel, d.Kind, d.DisplayName, d.Capabilities,
			d.Prices.Input, d.Prices.Output, d.Prices.CacheWrite, d.Prices.CacheRead, d.PriceCurrency, d.PriceNote, d.Enabled))
		return err
	})
	return out, err
}

// GetDeployment reads one deployment.
func (s *Store) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	d, err := scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentCols+` FROM modelgateway.deployments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, deploymentNotFound(id)
	}
	return d, err
}

// ListDeployments reads every deployment, oldest first.
func (s *Store) ListDeployments(ctx context.Context) ([]Deployment, error) {
	return listDeployments(ctx, s.pool)
}

func listDeployments(ctx context.Context, q querier) ([]Deployment, error) {
	rows, err := q.Query(ctx, `SELECT `+deploymentCols+` FROM modelgateway.deployments ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeploymentPatch replaces only the fields the caller supplies. A present
// Prices replaces all four rates, including any the caller clears.
type DeploymentPatch struct {
	DisplayName   *string
	Capabilities  *Capabilities
	Prices        *Prices
	PriceCurrency *string
	PriceNote     *string
	Enabled       *bool
}

func (s *Store) UpdateDeployment(ctx context.Context, id string, patch DeploymentPatch) (Deployment, error) {
	var out Deployment
	err := s.write(ctx, func(tx pgx.Tx) error {
		stmt := `UPDATE modelgateway.deployments SET display_name = coalesce($2, display_name),
			capabilities = coalesce($3, capabilities), price_currency = coalesce($4, price_currency),
			price_note = coalesce($5, price_note), enabled = coalesce($6, enabled), updated_at = now()`
		args := []any{id, patch.DisplayName, patch.Capabilities, patch.PriceCurrency, patch.PriceNote, patch.Enabled}
		// Unspecified NUMERIC prices must not pass through the API's float64 values.
		if rates := patch.Prices; rates != nil {
			stmt += `, price_input = $7, price_output = $8, price_cache_write = $9, price_cache_read = $10`
			args = append(args, rates.Input, rates.Output, rates.CacheWrite, rates.CacheRead)
		}
		var err error
		out, err = scanDeployment(tx.QueryRow(ctx, stmt+` WHERE id = $1 RETURNING `+deploymentCols, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return deploymentNotFound(id)
		}
		return err
	})
	return out, err
}

// DeleteDeployment removes a deployment no alias routes to.
func (s *Store) DeleteDeployment(ctx context.Context, id string) error {
	err := s.write(ctx, func(tx pgx.Tx) error {
		aliases, err := collectStrings(ctx, tx, `SELECT alias FROM modelgateway.alias_targets WHERE deployment_id = $1 ORDER BY alias`, id)
		if err != nil {
			return err
		}
		if len(aliases) > 0 {
			return fail(ErrConflict, "deployment %q is a target of aliases (%s); remove it from them first", id, strings.Join(aliases, ", "))
		}
		tag, err := tx.Exec(ctx, `DELETE FROM modelgateway.deployments WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return deploymentNotFound(id)
		}
		return nil
	})
	if isForeignKeyViolation(err) {
		return fail(ErrConflict, "deployment %q became an alias target; remove it from the alias first", id)
	}
	return err
}
