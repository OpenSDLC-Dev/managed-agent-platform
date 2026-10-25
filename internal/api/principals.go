package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// errDeactivated refuses a person who was removed from the organization. It is
// a 403, not a 401: the credential is genuine, and the refusal is of the
// account, so telling them so sends them to an admin rather than to the IdP.
var errDeactivated = errForbidden("this account has been removed from the organization")

// resolvePrincipal is principal.Resolve on the control plane's identity lane:
// it records that a verified human was seen, creating their row at first
// sight (#56, plan 31), and returns the role stored on it (plan 69).
func resolvePrincipal(ctx context.Context, pool *pgxpool.Pool, id identity.Identity) (identityPrincipal, error) {
	p, err := principal.Resolve(ctx, pool, id)
	if errors.Is(err, principal.ErrDeactivated) {
		return identityPrincipal{}, errDeactivated
	}
	if err != nil {
		return identityPrincipal{}, err
	}
	return identityPrincipal{ID: p.ID, Role: p.Role}, nil
}
