package login

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	oidcCookieName = "map_login"
	oidcLifetime   = 10 * time.Minute
)

// BeginOIDC stores one browser-bound login attempt for any replica to consume.
// Each start returns a fresh pending-cookie binding. An older attempt cannot be
// completed with that new binding. State and binding persist only as hashes;
// nonce and PKCE verifier expire with the attempt.
func (m *Manager) BeginOIDC(ctx context.Context, _ *http.Request) (state, nonce, verifier, binding string, err error) {
	values := make([]string, 4)
	for i := range values {
		raw := make([]byte, 32)
		rand.Read(raw)
		values[i] = base64.RawURLEncoding.EncodeToString(raw)
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", "", "", "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM browser_login_attempts WHERE expires_at<=clock_timestamp()`); err != nil {
		return "", "", "", "", err
	}
	if _, err := tx.Exec(ctx, `
		WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at)
		INSERT INTO browser_login_attempts (state_hash,binding_hash,nonce,code_verifier,created_at,expires_at)
		SELECT $1,$2,$3,$4,at,at+interval '10 minutes' FROM stamp`,
		hashCredential(values[0]), hashCredential(values[3]), values[1], values[2]); err != nil {
		return "", "", "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", "", "", err
	}
	return values[0], values[1], values[2], values[3], nil
}

// ConsumeOIDC atomically spends the state only for its original browser. Lock
// first, then compare expiry with DB time in a second statement: a predicate in
// the locking statement can be evaluated before an arbitrarily long lock wait.
// No token exchange belongs in this transaction; a failed exchange still spends
// the attempt and requires a fresh start.
func (m *Manager) ConsumeOIDC(ctx context.Context, r *http.Request, state string) (nonce, verifier string, err error) {
	binding, err := cookieToken(r, oidcCookieName)
	if err != nil || !canonicalToken(state) {
		return "", "", ErrUnauthenticated
	}
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var locked int
	err = tx.QueryRow(ctx, `SELECT 1 FROM browser_login_attempts
		WHERE state_hash=$1 AND binding_hash=$2 FOR UPDATE`, hashCredential(state), hashCredential(binding)).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUnauthenticated
	}
	if err != nil {
		return "", "", err
	}
	err = tx.QueryRow(ctx, `DELETE FROM browser_login_attempts
		WHERE state_hash=$1 AND expires_at>clock_timestamp() RETURNING nonce,code_verifier`, hashCredential(state)).Scan(&nonce, &verifier)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrUnauthenticated
	}
	if err != nil {
		return "", "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return nonce, verifier, nil
}

func (m *Manager) SetOIDCCookie(w http.ResponseWriter, binding string) {
	cookie := m.cookie(binding)
	cookie.Name = oidcCookieName
	cookie.MaxAge = int(oidcLifetime / time.Second)
	cookie.Expires = time.Now().UTC().Add(oidcLifetime)
	http.SetCookie(w, cookie)
}

func (m *Manager) ClearOIDCCookie(w http.ResponseWriter) {
	cookie := m.cookie("")
	cookie.Name = oidcCookieName
	cookie.Expires = time.Unix(1, 0).UTC()
	cookie.MaxAge = -1
	http.SetCookie(w, cookie)
}
