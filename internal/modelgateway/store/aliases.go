package store

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

const aliasCols = `name, display_name, kind, created_at, updated_at`

func scanAlias(row pgx.Row) (Alias, error) {
	var a Alias
	err := row.Scan(&a.Name, &a.DisplayName, &a.Kind, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

func aliasNotFound(name string) error { return fail(ErrNotFound, "alias %q does not exist", name) }

// targetsKind checks targets against the deployments they name, holding those
// against deletion, and returns the one kind they share.
func targetsKind(ctx context.Context, tx pgx.Tx, targets []Target) (Kind, error) {
	if len(targets) == 0 {
		return "", fail(ErrInvalid, "an alias needs at least one target")
	}
	ids := make([]string, 0, len(targets))
	for _, t := range targets {
		if slices.Contains(ids, t.DeploymentID) {
			return "", fail(ErrInvalid, "deployment %q is a target twice", t.DeploymentID)
		}
		if t.Weight <= 0 || t.Priority < 0 {
			return "", fail(ErrInvalid, "target %q needs a positive weight and a priority of zero or more", t.DeploymentID)
		}
		ids = append(ids, t.DeploymentID)
	}
	rows, err := tx.Query(ctx, `SELECT id, kind FROM modelgateway.deployments WHERE id = ANY($1) FOR SHARE`, ids)
	if err != nil {
		return "", err
	}
	kinds := map[string]Kind{}
	for rows.Next() {
		var (
			id   string
			kind Kind
		)
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return "", err
		}
		kinds[id] = kind
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	var kind Kind
	for _, id := range ids {
		k, ok := kinds[id]
		switch {
		case !ok:
			return "", fail(ErrInvalid, "deployment %q does not exist", id)
		case kind == "":
			kind = k
		case k != kind:
			return "", fail(ErrInvalid, "targets mix kinds %s and %s; an alias has one kind", kind, k)
		}
	}
	return kind, nil
}

func writeTargets(ctx context.Context, tx pgx.Tx, name string, targets []Target) error {
	if _, err := tx.Exec(ctx, `DELETE FROM modelgateway.alias_targets WHERE alias = $1`, name); err != nil {
		return err
	}
	for _, t := range targets {
		if _, err := tx.Exec(ctx,
			`INSERT INTO modelgateway.alias_targets (alias, deployment_id, priority, weight) VALUES ($1, $2, $3, $4)`,
			name, t.DeploymentID, t.Priority, t.Weight); err != nil {
			return err
		}
	}
	return nil
}

// CreateAlias inserts a. Its kind is its targets'; an embedding alias takes
// exactly one, since an index built through it must stay in one vector space.
func (s *Store) CreateAlias(ctx context.Context, a Alias) (Alias, error) {
	var out Alias
	err := s.write(ctx, func(tx pgx.Tx) error {
		kind, err := targetsKind(ctx, tx, a.Targets)
		if err != nil {
			return err
		}
		if kind == KindEmbedding && len(a.Targets) != 1 {
			return fail(ErrInvalid, "an embedding alias has exactly one deployment: an index built through it would mix vector spaces")
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO modelgateway.aliases (name, display_name, kind) VALUES ($1, $2, $3) ON CONFLICT (name) DO NOTHING`,
			a.Name, a.DisplayName, kind)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fail(ErrConflict, "alias %q already exists", a.Name)
		}
		if err := writeTargets(ctx, tx, a.Name, a.Targets); err != nil {
			return err
		}
		out, err = getAlias(ctx, tx, a.Name, "")
		return err
	})
	return out, err
}

// GetAlias reads one alias with its targets, in one snapshot so the two
// never come from different versions of the alias.
func (s *Store) GetAlias(ctx context.Context, name string) (Alias, error) {
	var out Alias
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = getAlias(ctx, tx, name, "")
		return err
	})
	return out, err
}

func getAlias(ctx context.Context, q querier, name, lock string) (Alias, error) {
	a, err := scanAlias(q.QueryRow(ctx, `SELECT `+aliasCols+` FROM modelgateway.aliases WHERE name = $1 `+lock, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Alias{}, aliasNotFound(name)
	}
	if err != nil {
		return Alias{}, err
	}
	targets, err := loadTargets(ctx, q, `WHERE alias = $1`, name)
	a.Targets = targets[name]
	return a, err
}

// loadTargets groups the matching targets by alias, each alias's in priority
// order.
func loadTargets(ctx context.Context, q querier, where string, args ...any) (map[string][]Target, error) {
	rows, err := q.Query(ctx,
		`SELECT alias, deployment_id, priority, weight FROM modelgateway.alias_targets `+where+` ORDER BY alias, priority, deployment_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Target{}
	for rows.Next() {
		var (
			alias string
			t     Target
		)
		if err := rows.Scan(&alias, &t.DeploymentID, &t.Priority, &t.Weight); err != nil {
			return nil, err
		}
		out[alias] = append(out[alias], t)
	}
	return out, rows.Err()
}

// ListAliases reads every alias with its targets, by name, in one snapshot.
func (s *Store) ListAliases(ctx context.Context) ([]Alias, error) {
	var out []Alias
	err := s.read(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = listAliases(ctx, tx)
		return err
	})
	return out, err
}

func listAliases(ctx context.Context, q querier) ([]Alias, error) {
	rows, err := q.Query(ctx, `SELECT `+aliasCols+` FROM modelgateway.aliases ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []Alias
	for rows.Next() {
		a, err := scanAlias(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	targets, err := loadTargets(ctx, q, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Targets = targets[out[i].Name]
	}
	return out, nil
}

// UpdateAlias applies fn to the alias and writes back its display name and
// targets, which keep the alias's kind; an embedding alias keeps its one
// deployment.
func (s *Store) UpdateAlias(ctx context.Context, name string, fn func(*Alias) error) (Alias, error) {
	var out Alias
	err := s.write(ctx, func(tx pgx.Tx) error {
		a, err := getAlias(ctx, tx, name, "FOR UPDATE")
		if err != nil {
			return err
		}
		next := a
		next.Targets = slices.Clone(a.Targets)
		if err := fn(&next); err != nil {
			return err
		}
		kind, err := targetsKind(ctx, tx, next.Targets)
		if err != nil {
			return err
		}
		if kind != a.Kind {
			return fail(ErrInvalid, "alias %q routes to deployments of kind %s; a target of kind %s cannot join it", name, a.Kind, kind)
		}
		if a.Kind == KindEmbedding && (len(next.Targets) != 1 || next.Targets[0].DeploymentID != a.Targets[0].DeploymentID) {
			return fail(ErrInvalid, "an embedding alias's deployment is fixed at creation; a new embedding model is a new alias")
		}
		if _, err := tx.Exec(ctx,
			`UPDATE modelgateway.aliases SET display_name = $2, updated_at = now() WHERE name = $1`, name, next.DisplayName); err != nil {
			return err
		}
		if err := writeTargets(ctx, tx, name, next.Targets); err != nil {
			return err
		}
		out, err = getAlias(ctx, tx, name, "")
		return err
	})
	return out, err
}

// DeleteAlias removes an alias and its targets. An alias a key policy grants
// is a conflict: the grant would outlive it and pass to the next alias of its
// name. The alias is locked first, so a grant PutKeyPolicy is writing either
// commits before the check sees it or finds the alias gone.
func (s *Store) DeleteAlias(ctx context.Context, name string) error {
	return s.write(ctx, func(tx pgx.Tx) error {
		var found string
		err := tx.QueryRow(ctx, `SELECT name FROM modelgateway.aliases WHERE name = $1 FOR UPDATE`, name).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return aliasNotFound(name)
		}
		if err != nil {
			return err
		}
		keys, err := collectStrings(ctx, tx,
			`SELECT api_key_id FROM modelgateway.key_policies WHERE $1 = ANY(aliases) ORDER BY api_key_id`, name)
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			return fail(ErrConflict, "alias %q is granted to api keys (%s); remove it from their policies first", name, strings.Join(keys, ", "))
		}
		_, err = tx.Exec(ctx, `DELETE FROM modelgateway.aliases WHERE name = $1`, name)
		return err
	})
}
