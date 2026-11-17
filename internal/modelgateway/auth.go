package modelgateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/inferencetoken"
	"github.com/jackc/pgx/v5"
)

// caller keeps credential attribution and the principal authorizing inference.
type caller struct {
	keyID       string // its api_keys id
	principalID string
	sessionID   string // delegated calls use the claim's session, never a caller header
	delegated   bool
}

// authenticate checks a delegated claim or an owned key with its current owner.
// x-api-key precedes Bearer; repeated fields are ambiguous and refused.
func (h *handler) authenticate(r *http.Request) (caller, *apiError) {
	keys := r.Header.Values("x-api-key")
	if len(keys) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return caller{}, unauthenticated("a credential header is repeated")
	}
	key := ""
	if len(keys) == 1 {
		key = keys[0]
	} else if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key = tok
	}
	if key == "" {
		return caller{}, unauthenticated("x-api-key header is required")
	}
	if strings.HasPrefix(key, inferencetoken.TokenPrefix) {
		p, err := inferencetoken.Authenticate(r.Context(), h.cfg.Keys, key)
		if err != nil {
			return caller{}, internal(r, "inference token lookup failed", err)
		}
		if p.WorkID != "" {
			return caller{principalID: p.PrincipalID, sessionID: p.SessionID, delegated: true}, nil
		}
		return caller{}, unauthenticated("invalid inference token")
	}
	// Read credential and current owner authority in one database snapshot.
	var c caller
	var role string
	err := h.cfg.Keys.QueryRow(r.Context(), `SELECT k.id, p.id, p.role
		FROM api_keys k JOIN principals p ON p.id=k.owner_principal_id
		WHERE k.owned_key_hash=$1 AND k.status='active'
		  AND (k.expires_at IS NULL OR k.expires_at > clock_timestamp())
		  AND p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'`,
		apikey.Hash(key)).Scan(&c.keyID, &c.principalID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return caller{}, unauthenticated("invalid x-api-key")
	}
	if err != nil {
		return caller{}, internal(r, "api key lookup failed", err)
	}
	if role != "developer" && role != "admin" {
		return caller{}, forbidden("calling models requires the developer role")
	}
	return c, nil
}
