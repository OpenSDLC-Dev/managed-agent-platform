// Package login holds durable browser credentials and local-password primitives.
// It exposes no HTTP routes. Browser sessions read the current principal without
// replaying the IdP sign-in mapping or writing profile/last-seen fields.
//
// SQL stays here, its issuing package; store owns the schema. Argon2id comes
// from golang.org/x/crypto, already in the dependency graph, instead of a custom
// password derivation. Its parameters are fixed, bounded and process-limited.
package login

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	LocalIssuer          = "urn:managed-agent-platform:local"
	ServiceAccountIssuer = "urn:managed-agent-platform:service-account"
	sessionLifetime      = 12 * time.Hour
)

var (
	// ErrUnauthenticated covers missing, malformed, expired and revoked cookies.
	// A recognized deactivated person instead returns principal.ErrDeactivated.
	ErrUnauthenticated = errors.New("login: invalid or expired browser session")
	ErrCSRF            = errors.New("login: invalid browser origin or CSRF token")
)

// Manager shares credentials through PostgreSQL, not replica-local memory. It
// deliberately retains no OIDC client secret, even when constructed with one.
type Manager struct {
	pool   *pgxpool.Pool
	origin string
	secure bool
}

func New(pool *pgxpool.Pool, cfg Config) *Manager {
	m := &Manager{pool: pool, secure: true}
	if u, err := parseOrigin(cfg.PublicOrigin, cfg.Development); err == nil {
		m.origin = cfg.PublicOrigin
		m.secure = !loopbackHTTP(u)
	}
	return m
}
