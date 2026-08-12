package api

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// upsertPrincipal records that a verified human was seen and returns their
// principal id, provisioning the row on first sight (#56, plan 31).
//
// It is a single statement because it is on the authentication path of every
// human request: a read-then-write would double the round trips and race two
// concurrent logins into a UNIQUE violation. ON CONFLICT (issuer, subject) is
// the same guarded-upsert shape session_checkpoints and worker_polls already
// use, and the conflict target is exactly the UNIQUE constraint 0022 declares.
//
// RETURNING id is what makes it one trip: on insert it returns the id just
// minted, on conflict the id the row already had — so a returning human keeps
// the id their earlier sessions were stamped with, which is the whole point of
// created_by surviving as an audit trail.
//
// email and display_name are REFRESHED from the token rather than written once.
// The provider owns them; a human who changes their display name at the IdP
// should not read as their old name in an audit trail forever. last_seen_at
// likewise moves on every request, because it is what an operator's retention
// DELETE selects on.
//
// No role is written. Roles live in the token and are re-read per request, so
// this table can never become a second, stale authority beside the provider —
// see 0022_principals.sql.
func upsertPrincipal(ctx context.Context, pool *pgxpool.Pool, id identity.Identity) (string, error) {
	var principalID string
	err := pool.QueryRow(ctx, `
		INSERT INTO principals (id, issuer, subject, email, display_name)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (issuer, subject) DO UPDATE
		SET email = EXCLUDED.email,
		    display_name = EXCLUDED.display_name,
		    last_seen_at = now()
		RETURNING id`,
		domain.NewID(domain.PrefixPrincipal).String(),
		id.Issuer, id.Subject, id.Email, id.DisplayName,
	).Scan(&principalID)
	if err != nil {
		return "", err
	}
	return principalID, nil
}
