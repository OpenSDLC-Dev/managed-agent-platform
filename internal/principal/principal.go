// Package principal resolves a verified person to their principals row and
// the role stored on it (docs/plan/69_principal-bound-auth.md decision 4).
//
// It is a package of its own because two identity lanes apply the same rule:
// the control plane's (internal/api), and the model gateway admin API's for
// its operator token. Two copies of the statement would be two copies of who
// may do what, free to drift apart.
package principal

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
)

// Principal is the row a verified person resolved to: its stable id, and the
// role stored on it, which is the role that authorizes the request.
type Principal struct {
	ID   string
	Role identity.Role
}

// ErrDeactivated is Resolve's answer for a person who was removed. Removal
// lasts: the row stays, and the person is refused rather than re-created.
var ErrDeactivated = errors.New("principal: deactivated")

// Querier is the one method Resolve needs, as a *pgxpool.Pool has it.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Resolve records that a verified person was seen and returns their principal,
// creating the row at their first sign-in. Every verified request counts as a
// sign-in, so the IdP's mapping is applied here each time.
//
// It is a single statement because it is on the authentication path of every
// request a person makes: a read-then-write would double the round trips and
// race two concurrent first sign-ins into a UNIQUE violation. ON CONFLICT
// (issuer, subject) targets exactly the UNIQUE constraint 0022 declares.
//
// RETURNING is what makes it one trip: on insert it returns the id just
// minted, on conflict the id the row already had. So a returning person keeps
// the id their earlier sessions were stamped with, which is the whole point of
// created_by surviving as an audit trail. It returns the role the same way:
// the stored role after this sign-in's rule, never the token's.
//
// email and display_name are refreshed from the token rather than written
// once. The provider owns them; a person who changes their display name at the
// IdP should not read as their old name in an audit trail forever.
// last_seen_at moves on every request too.
//
// The role rule. EXCLUDED holds this sign-in's answer: the mapped role and
// 'idp' when the mapping matched, or user and 'console' when it did not
// (id.Role is identity.RoleNone, passed as NULL). It replaces the stored role
// when this sign-in or the one that set the stored role came from the
// mapping. So a mapped person has the IdP's role, a person the mapping
// stopped matching goes back to user, and only a role the console set
// survives a sign-in the mapping does not match.
//
// A deactivated row fails the conflict update's WHERE, so the statement
// neither inserts nor updates and returns no row: the person is refused, and
// their row keeps the email, display name and last_seen_at it had. Removal
// wins over the mapping, since the WHERE refuses the row whatever this
// sign-in mapped to.
func Resolve(ctx context.Context, db Querier, id identity.Identity) (Principal, error) {
	var mapped *string
	if id.Role != identity.RoleNone {
		r := string(id.Role)
		mapped = &r
	}
	var p Principal
	err := db.QueryRow(ctx, `
		INSERT INTO principals (id, issuer, subject, email, display_name, role, role_source)
		VALUES ($1, $2, $3, $4, $5, coalesce($6::text, 'user'),
		        CASE WHEN $6::text IS NULL THEN 'console' ELSE 'idp' END)
		ON CONFLICT (issuer, subject) DO UPDATE
		SET email = EXCLUDED.email,
		    display_name = EXCLUDED.display_name,
		    last_seen_at = now(),
		    role = CASE WHEN EXCLUDED.role_source = 'idp' OR principals.role_source = 'idp'
		                THEN EXCLUDED.role ELSE principals.role END,
		    role_source = EXCLUDED.role_source
		WHERE principals.status = 'active'
		RETURNING id, role`,
		domain.NewID(domain.PrefixPrincipal).String(),
		id.Issuer, id.Subject, id.Email, id.DisplayName, mapped,
	).Scan(&p.ID, &p.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrDeactivated
	}
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}
