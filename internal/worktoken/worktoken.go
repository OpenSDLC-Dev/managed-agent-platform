// Package worktoken issues and authenticates the per-item sessions tokens a
// BYOC worker presents for a work item whose session attaches a memory store
// (docs/plan/36_memory-stores.md decision 15). The reference's poll response
// carries the token inside the item's `secret` — URL-safe base64 of a JSON
// object whose sessions_token key the v1.66.0 reference worker reads
// (anthropic-sdk-go lib/environments/worker.go, sessionsTokenFromSecret) —
// and the worker then calls the item's heartbeat and stop, its session's read
// and events, the skill reads and the memory routes with the token as its
// Bearer, the environment key deleted from those requests.
//
// A token is minted once per claim, in the claim's own transaction, and
// stored hash-only. It carries neither an expiry nor a revocation column: it
// is valid while the item it names is live — the join conditions Authenticate
// runs — so a re-hand-out (a fresh work id, #62), a lapsed lease and a session
// archive each end it without an event, and a stop ends it a minute after
// it was requested: the reference worker flushes its unsynced memory writes once the
// control plane has reported the stop, on a context of its own bounded by 30
// seconds (lib/environments/memories.go Cleanup), and a token dead at that
// instant would lose them — a BYOC workdir is removed at the item's end, with
// no held sandbox to sync from later as a cloud session has. The value itself is
// gatetoken's mint under another prefix, so every internal bearer the
// platform issues shares one entropy and one alphabet.
package worktoken

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
)

// TokenPrefix marks a sessions token value. Internal-only, never an id on the
// /v1 wire — deliberately NOT in domain/id.go knownPrefixes, like gtk_ — and
// what the auth lane routes on: an environment key (sk-map-env01-…) and a JWT
// (two dots) can never carry it.
const TokenPrefix = "wtk_"

// DB is the statement surface Mint needs: a pgx.Tx, so the token row commits
// with the claim it belongs to, or a pool.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Principal is what a token authenticates — the item, its session, and the
// environment both belong to.
type Principal struct {
	WorkID        string
	SessionID     string
	EnvironmentID string
}

// Mint issues the token for workID's claim of sessionID's item and stores its
// hash on db. A later row for the same session needs no retry logic: the
// earlier one is dead by Authenticate's join conditions, its work id no
// longer a live item's.
func Mint(ctx context.Context, db DB, workID, sessionID string) (string, error) {
	token := gatetoken.MintWithPrefix(TokenPrefix)
	if _, err := db.Exec(ctx,
		`INSERT INTO work_session_tokens (id, work_id, session_id, token_hash) VALUES ($1, $2, $3, $4)`,
		domain.NewID("worktok").String(), workID, sessionID, gatetoken.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}

// Secret renders a token as the work item's `secret`: unpadded URL-safe base64
// of {"sessions_token": token}, the envelope the reference worker decodes
// (padded or not). The other keys the reference's envelope may carry — its
// ingress and source tokens, which the worker does not consume — are absent;
// #165's vault-credential bundle would be another key beside this one.
func Secret(token string) string {
	raw, _ := json.Marshal(map[string]string{"sessions_token": token})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Revoke deletes an item's tokens — the one delete the table sees. It is for
// the settlement of an abandoned wind-down (a stopping item whose lease
// lapsed): its minute may still be running when the platform presumes the
// worker dead and, in the same transaction, re-arms the session for another
// — and two workers must not write a session's memory at once.
func Revoke(ctx context.Context, db DB, workID string) error {
	_, err := db.Exec(ctx, `DELETE FROM work_session_tokens WHERE work_id = $1`, workID)
	return err
}

// Authenticate resolves a token to its principal, or the zero Principal when
// the token is unknown or no longer names a live item: the item's id must
// still be the one the token was minted for (a re-hand-out rewrites it), its
// lease unexpired while it runs, or its stop requested within the last minute
// once it is stopping or stopped (a graceful stop parks an active item in
// stopping with a lease the heartbeat no longer extends, so the minute counts
// from the request rather than from the lease or the stop's completion: the
// wind-down and the post-stop flush both ride it), and its session
// unarchived.
//
// The minute is sized to the reference worker: it learns of a stop at its
// next heartbeat (half the 30 s TTL), then flushes its unsynced memory writes
// in one pass bounded by MemoryFlushTimeout (30 s) — 45 s of the 60. Counted
// from the request, the settlement of an abandoned wind-down cannot restart
// it, and Revoke ends it there regardless.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, token string) (Principal, error) {
	var p Principal
	err := pool.QueryRow(ctx,
		`SELECT t.work_id, t.session_id, w.environment_id
		   FROM work_session_tokens t
		   JOIN work_items w ON w.id = t.work_id AND w.session_id = t.session_id
		   JOIN sessions s ON s.id = t.session_id
		  WHERE t.token_hash = $1
		    AND CASE WHEN w.state IN ('stopping', 'stopped')
		             THEN w.stop_requested_at > now() - interval '60 seconds'
		             ELSE w.lease_expires_at > now() END
		    AND s.archived_at IS NULL`,
		gatetoken.HashToken(token)).Scan(&p.WorkID, &p.SessionID, &p.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, nil
	}
	return p, err
}
