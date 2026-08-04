// Package gatetoken issues and authenticates the per-session bearer tokens a
// session's egress gate presents to the controlplane's internal gate-config
// endpoint (docs/plan/12_vaults-credentials.md slice 4). Only the hash is stored
// (the internal/api environment-key precedent); a token is valid for its
// session's life, so it is minted once when the gate is created and never
// rotated on the wall clock — a controlplane outage longer than a TTL therefore
// cannot be misread as a revocation.
package gatetoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TokenPrefix marks a session-gate token value. Internal-only (never on the /v1
// wire), like the apikey_/envkey_ credential values — deliberately NOT in
// domain/id.go knownPrefixes. The gtk_ prefix makes a leaked token
// secret-scanner-recognizable.
const TokenPrefix = "gtk_"

const tokenRandomBytes = 32 // 256 bits of entropy

// tokenEncoding is Crockford base32 (lowercased, no padding) — the same alphabet
// as domain IDs, so a token value carries no shell- or env-var-hostile
// characters (it travels to the gate container as GATE_TOKEN).
var tokenEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// Mint returns a fresh opaque token value. It panics only if the system CSPRNG
// fails, which is unrecoverable for a server that must mint credentials.
func Mint() string {
	b := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		panic("gatetoken: crypto/rand failed: " + err.Error())
	}
	return TokenPrefix + tokenEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// revokeSQL marks every live token for a session revoked. Ensure runs it
// before inserting a successor; Revoke runs it alone.
const revokeSQL = `UPDATE session_gate_tokens SET revoked_at = now()
	 WHERE session_id = $1 AND revoked_at IS NULL`

// Ensure makes token the one live gate token for sessionID: in one transaction
// it revokes every prior unrevoked token for the session and inserts the new
// hash. Re-minting on a replacement gate therefore invalidates the predecessor
// (revoke-on-re-mint), and the partial unique index keeps at most one live token
// per session. Only the hash is stored.
func Ensure(ctx context.Context, pool *pgxpool.Pool, sessionID, token string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, revokeSQL, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_gate_tokens (id, session_id, token_hash) VALUES ($1, $2, $3)`,
		domain.NewID("gatetok").String(), sessionID, hashToken(token)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Revoke marks sessionID's live gate token revoked without minting a successor
// — the gated→ungated teardown, where a provision dismantles the session's gate
// for good and Ensure's revoke-on-re-mint will never run (#197). Idempotent: a
// session with no live token is a no-op, so a provider that revokes before
// tearing the pair down can safely retry both if the teardown fails partway.
func Revoke(ctx context.Context, pool *pgxpool.Pool, sessionID string) error {
	_, err := pool.Exec(ctx, revokeSQL, sessionID)
	return err
}

// Authenticate resolves a gate token to the session it is scoped to, or "" if
// the token is unknown, revoked, or its session has been archived (fail-closed:
// an archived session's gate must stop being served). There is no wall-clock
// expiry — validity is the session's lifetime. A deleted session's token is
// cascade-removed and resolves to "" with no error.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	var sessionID string
	err := pool.QueryRow(ctx,
		`SELECT t.session_id FROM session_gate_tokens t
		    JOIN sessions s ON s.id = t.session_id
		  WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND s.archived_at IS NULL`,
		hashToken(token)).Scan(&sessionID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return sessionID, err
}
