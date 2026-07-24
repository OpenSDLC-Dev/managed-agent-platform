// Package vaultresolve is read-time vault-credential resolution: it turns a
// session's attached vault_ids into the environment-variable bindings a sandbox
// is provisioned with. Resolution reads current rows every time it runs (no
// cache), so rotation and archive propagate without a session restart
// (docs/plan/12_vaults-credentials.md, D5).
//
// This slice resolves only the sandbox-visible half: each active
// environment_variable credential's secret_name paired with an opaque
// placeholder derived per (session, secret_name) (internal/egress) — stable
// across re-provision. The secret the placeholder stands for is
// read and substituted separately, at egress time in the per-session gate (a
// later slice) — never injected into the sandbox, which sees the placeholder
// alone.
package vaultresolve

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/jackc/pgx/v5"
)

// Querier is the read surface resolution needs — satisfied by a *pgxpool.Pool
// or a pgx.Tx. Resolution decrypts nothing, so it takes no cipher yet.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Binding is one resolved environment-variable credential: the sandbox-visible
// env var, injected at provision as SecretName=Placeholder. The placeholder is
// opaque and inert on its own — a request that carries it egresses the literal
// token until the gate substitutes the real secret.
type Binding struct {
	SecretName  string
	Placeholder string
}

// Bindings resolves the active environment_variable credentials of sessionID's
// attached vaults into placeholder bindings. When several attached vaults carry
// the same secret_name, the first vault in vaultIDs order wins (D5). An archived
// vault contributes nothing: archiving a vault archives and purges its
// credentials, so the archived_at filter already excludes them. Placeholders are
// derived per (session, secret_name), so resolution is fully deterministic — a
// re-provision or the egress gate recovers the exact tokens already injected.
func Bindings(ctx context.Context, q Querier, sessionID string, vaultIDs []string) ([]Binding, error) {
	if len(vaultIDs) == 0 {
		return nil, nil
	}
	// The vault's own archived_at is checked directly, not only the credential's:
	// an archived vault contributes nothing is a security guarantee, so it does
	// not rest solely on the archive cascade (archiving a vault archives+purges
	// its credentials) holding — a stale un-cascaded credential row is still
	// excluded here.
	rows, err := q.Query(ctx,
		`SELECT c.vault_id, c.auth FROM vault_credentials c
		    JOIN vaults v ON v.id = c.vault_id
		  WHERE c.vault_id = ANY($1) AND c.auth_type = 'environment_variable'
		    AND c.archived_at IS NULL AND v.archived_at IS NULL`,
		vaultIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group secret_names by vault so first-vault-wins can be applied in the
	// caller's vault_ids order, which the ANY(...) query does not preserve.
	byVault := map[string][]string{}
	for rows.Next() {
		var vaultID string
		var authDoc []byte
		if err := rows.Scan(&vaultID, &authDoc); err != nil {
			return nil, err
		}
		var doc struct {
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(authDoc, &doc); err != nil {
			return nil, err
		}
		if doc.SecretName != "" {
			byVault[vaultID] = append(byVault[vaultID], doc.SecretName)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	var out []Binding
	for _, vid := range vaultIDs {
		names := byVault[vid]
		sort.Strings(names)
		for _, name := range names {
			if _, dup := seen[name]; dup {
				continue // first attached vault with this secret_name already won
			}
			seen[name] = struct{}{}
			out = append(out, Binding{SecretName: name, Placeholder: egress.Placeholder(sessionID, name)})
		}
	}
	return out, nil
}
