// Package inferencetoken issues the brain's per-claim delegated inference
// credential (plan 69). It carries workspace:inference as its only scope and
// brain as its actor. The subject is a member, or a legacy initiator until
// slice 7. Hashes live apart from API keys; no management lookup accepts them.
package inferencetoken

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gatetoken"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TokenPrefix distinguishes this internal bearer from API, work and gate keys.
const TokenPrefix = "itk_"

// Credential selects the candidate under the gateway's credential precedence.
// A prefix is only a hint: an operator may have chosen it for a legacy key.
// Authenticate must succeed before a receiver treats it as delegated authority.
func Credential(r *http.Request) string {
	keys, auth := r.Header.Values("x-api-key"), r.Header.Values("Authorization")
	if len(keys) > 1 || len(auth) > 1 {
		return ""
	}
	var token string
	if len(keys) == 1 {
		token = keys[0]
	} else {
		token, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		return ""
	}
	return token
}

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type writer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Principal is the authority of one authenticated model-turn claim. Exactly
// one of PrincipalID and APIKeyID is set; a legacy key stands for itself.
type Principal struct {
	PrincipalID string
	APIKeyID    string
	SessionID   string
	WorkID      string
}

// Mint commits with the claim whose work-item lock the caller holds. The
// caller checks membership before calling. No session or principal FK may
// be added here: those locks would reverse the queue's lock order.
func Mint(ctx context.Context, db writer, workID string, generation int64, principalID, apiKeyID *string) (string, error) {
	token := gatetoken.MintWithPrefix(TokenPrefix)
	_, err := db.Exec(ctx, `INSERT INTO inference_tokens(work_id,claim_generation,token_hash,principal_id,api_key_id)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT(work_id) DO UPDATE SET
		claim_generation=excluded.claim_generation,token_hash=excluded.token_hash,
		principal_id=excluded.principal_id,api_key_id=excluded.api_key_id`,
		workID, generation, gatetoken.HashToken(token), principalID, apiKeyID)
	if err != nil {
		return "", err
	}
	return token, nil
}

// Authenticate checks the lease, claim generation, session and membership in
// one snapshot. Role is deliberately absent: demotion does not stop work
// started while the initiator could create it. Legacy key state is ignored.
func Authenticate(ctx context.Context, db Querier, token string) (Principal, error) {
	var p Principal
	err := db.QueryRow(ctx, `SELECT coalesce(t.principal_id,''),coalesce(t.api_key_id,''),w.session_id,w.id
		FROM inference_tokens t
		JOIN work_items w ON w.id=t.work_id AND w.inference_generation=t.claim_generation
		JOIN sessions s ON s.id=w.session_id
		LEFT JOIN principals p ON p.id=t.principal_id
		WHERE t.token_hash=$1 AND w.kind='model_turn' AND w.state='active'
		AND w.lease_expires_at>clock_timestamp() AND s.archived_at IS NULL
		AND (t.api_key_id IS NOT NULL OR
		     (p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'))`,
		gatetoken.HashToken(token)).Scan(&p.PrincipalID, &p.APIKeyID, &p.SessionID, &p.WorkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, nil
	}
	return p, err
}
