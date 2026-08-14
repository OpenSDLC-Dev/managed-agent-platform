// Package vaultresolve is read-time vault-credential resolution: it turns a
// session's attached vault_ids into the environment-variable bindings a sandbox
// is provisioned with. Resolution reads current rows every time it runs (no
// cache), so rotation and archive propagate without a session restart
// (docs/plan/12_vaults-credentials.md, D5).
//
// Two resolutions share one selection rule (winnersFor, first-vault-wins), so
// they can never disagree on which credential a secret_name resolves to.
// Bindings yields the sandbox-visible half: each active environment_variable
// credential's secret_name paired with an opaque placeholder derived per
// (session, secret_name) (internal/egress) — stable across re-provision.
// Credentials yields the gate half: the same winners with their secrets
// decrypted, which the per-session egress gate substitutes back for the
// placeholder at egress time. The sandbox only ever sees the placeholder; the
// secret is never injected into it.
package vaultresolve

import (
	"context"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the read surface resolution needs — satisfied by a *pgxpool.Pool
// or a pgx.Tx. Resolution decrypts nothing, so it takes no cipher yet.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// DB is Querier plus the one write resolution performs: storing an mcp_oauth
// credential's rotated tokens after a dial-time refresh (see MCPCredentialFor).
// The two are separate because the environment-variable resolutions genuinely
// only read, and widening their parameter would say otherwise.
type DB interface {
	Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
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
	winners, err := winnersFor(ctx, q, vaultIDs)
	if err != nil {
		return nil, err
	}
	var out []Binding
	for _, w := range winners {
		out = append(out, Binding{SecretName: w.secretName, Placeholder: egress.Placeholder(sessionID, w.secretName)})
	}
	return out, nil
}
