package login

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// Session is the private session DTO plus the internal audit actor. It contains
// current authority, not the role or profile at the time the cookie was issued.
type Session struct {
	PrincipalID string        `json:"-"`
	SignedIn    bool          `json:"signed_in"`
	ID          string        `json:"id"`
	Role        identity.Role `json:"role"`
	RoleSource  string        `json:"role_source"`
	Email       string        `json:"email"`
	Name        string        `json:"name"`
	LoginName   string        `json:"login_name,omitempty"`
	ExpiresAt   time.Time     `json:"expires_at"`
	CSRFToken   string        `json:"csrf_token"`
}

// IssuedSession carries the bearer only as far as SetCookie. Serializers and
// method-interceptable fmt/log rendering exclude it; see Format for fmt's limits.
type IssuedSession struct {
	Session
	Token string `json:"-" yaml:"-" xml:"-"`
}

func (s IssuedSession) String() string       { return fmt.Sprintf("%+v", s.Session) }
func (s IssuedSession) GoString() string     { return s.String() }
func (s IssuedSession) LogValue() slog.Value { return slog.AnyValue(s.Session) }

// Format redacts method-interceptable fmt verbs on values and pointers. fmt
// bypasses methods for %p on a struct value and for unexported nested value
// fields; those paths are not covered. %p on a pointer prints only its address.
func (s IssuedSession) Format(state fmt.State, _ rune) { io.WriteString(state, s.String()) }

// Issue stores only SHA-256 of a fresh 32-byte bearer. The database starts the
// absolute 12-hour lifetime. Issuance also prunes expired sessions and attempts.
func (m *Manager) Issue(ctx context.Context, principalID string) (IssuedSession, error) {
	raw := make([]byte, 32)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return IssuedSession{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM browser_sessions WHERE expires_at <= clock_timestamp();
		DELETE FROM browser_login_attempts WHERE expires_at <= clock_timestamp()`); err != nil {
		return IssuedSession{}, err
	}
	session, err := scanSession(tx.QueryRow(ctx, `
		WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at), issued AS (
			INSERT INTO browser_sessions (token_hash,principal_id,created_at,expires_at)
			SELECT $1,p.id,stamp.at,stamp.at+interval '12 hours'
			FROM principals p CROSS JOIN stamp
			WHERE p.id=$2 AND p.kind='user' AND p.status='active'
			RETURNING principal_id,expires_at
		)
		SELECT p.id,p.kind,p.status,p.role,p.role_source,p.email,p.display_name,
		       CASE WHEN p.issuer=$3 THEN p.subject ELSE '' END,issued.expires_at
		FROM issued JOIN principals p ON p.id=issued.principal_id`, hashCredential(token), principalID, LocalIssuer))
	if err != nil {
		return IssuedSession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedSession{}, err
	}
	session.CSRFToken = csrfToken(token)
	return IssuedSession{Session: session, Token: token}, nil
}

// Authenticate is read-only. In particular it must never call principal.Resolve:
// a cookie is not an IdP sign-in and cannot replay its mapping/profile writes.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (Session, error) {
	token, err := sessionToken(r)
	if err != nil {
		return Session{}, err
	}
	session, err := scanSession(m.pool.QueryRow(ctx, `
		SELECT p.id,p.kind,p.status,p.role,p.role_source,p.email,p.display_name,
		       CASE WHEN p.issuer=$2 THEN p.subject ELSE '' END,s.expires_at
		FROM browser_sessions s JOIN principals p ON p.id=s.principal_id
		WHERE s.token_hash=$1 AND s.expires_at>clock_timestamp()`, hashCredential(token), LocalIssuer))
	if err != nil {
		return Session{}, err
	}
	session.CSRFToken = csrfToken(token)
	return session, nil
}

// Revoke is idempotent for a canonical cookie, including an already expired one.
// HTTP admission and CSRF checks belong to the later logout handler.
func (m *Manager) Revoke(ctx context.Context, r *http.Request) error {
	token, err := sessionToken(r)
	if err != nil {
		return err
	}
	_, err = m.pool.Exec(ctx, `DELETE FROM browser_sessions WHERE token_hash=$1`, hashCredential(token))
	return err
}

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	var kind, status string
	err := row.Scan(&s.PrincipalID, &kind, &status, &s.Role, &s.RoleSource, &s.Email, &s.Name, &s.LoginName, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Session{}, err
	}
	if kind != "user" {
		return Session{}, ErrUnauthenticated
	}
	if status != "active" {
		return Session{}, principal.ErrDeactivated
	}
	s.ID = principal.PublicID(s.PrincipalID, kind)
	if s.ID == "" {
		return Session{}, ErrUnauthenticated
	}
	s.SignedIn = true
	return s, nil
}

func hashCredential(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func csrfToken(token string) string {
	sum := sha256.Sum256([]byte("managed-agent-platform:browser-csrf:v1\x00" + token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
