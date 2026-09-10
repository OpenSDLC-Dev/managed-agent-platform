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

// Mint returns a fresh opaque gate token value.
func Mint() string { return MintWithPrefix(TokenPrefix) }

// MintWithPrefix returns a fresh opaque token value under prefix — 32 random
// bytes in the encoding above. It is the one mint for every internal bearer
// the platform issues (the gate's gtk_, the work item's wtk_ in
// internal/worktoken), so their entropy and alphabet cannot drift. It panics
// only if the system CSPRNG fails, which is unrecoverable for a server that
// must mint credentials.
func MintWithPrefix(prefix string) string {
	b := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		panic("gatetoken: crypto/rand failed: " + err.Error())
	}
	return prefix + tokenEncoding.EncodeToString(b)
}

// HashToken is the digest stored in place of a token — sha256, hex — shared
// by every table that keeps one, for MintWithPrefix's reason.
func HashToken(token string) string {
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
// per session. Only the hash is stored. It takes the session row before either
// of those, the order a session delete takes and load-bearing against it (#313)
// — the body says why.
func Ensure(ctx context.Context, pool *pgxpool.Pool, sessionID, token string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The session row first, then its token rows — the order a session delete
	// takes: it holds the session (internal/api, requireNotRunning's FOR UPDATE)
	// and then needs the token rows, because migration 0012 cascades the delete
	// into them. Revoking first and inserting second took those in the opposite
	// order, so a delete racing a re-mint closed a cycle and Postgres aborted one
	// side of it (#313).
	//
	// KEY SHARE is the mode the insert's foreign key check takes below, so on the
	// path that reaches the insert this acquires the same tuple and relation
	// locks the transaction would have acquired anyway — earlier, and before the
	// token rows rather than after. Two Ensures still do not block each other on
	// it: a statement about what this costs, not a claim that they are otherwise
	// ordered.
	//
	// It is deliberately not read for existence — the foreign key
	// remains the one authority on whether the session is there, and a second
	// answer here could only disagree with it.
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM sessions WHERE id = $1 FOR KEY SHARE`, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, revokeSQL, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO session_gate_tokens (id, session_id, token_hash) VALUES ($1, $2, $3)`,
		domain.NewID("gatetok").String(), sessionID, HashToken(token)); err != nil {
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
		HashToken(token)).Scan(&sessionID)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return sessionID, err
}
