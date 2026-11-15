package modelgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/jackc/pgx/v5"
)

// caller keeps the key's audit identity, its owner, and any legacy policy.
type caller struct {
	keyID       string // its api_keys id
	principalID string // empty for legacy unowned keys
	policy      store.KeyPolicy
}

// may reports whether the caller's grant covers the configured alias name.
func (c caller) may(alias string) bool {
	return c.policy.Aliases == nil || slices.Contains(c.policy.Aliases, alias)
}

// authenticate checks the key and its current owner, or its legacy grant.
// The key rides x-api-key, as an Anthropic SDK sends it, or a Bearer, as an
// OpenAI SDK does; x-api-key wins when both are present. A repeated header is
// refused as ambiguous, as the control plane refuses it. Every key, the
// bootstrap and brain keys included, authenticates by the control plane's
// rule — an active, unexpired row — so a key the platform has archived or let
// expire calls nothing here either; those two keys' values only spare them a
// policy. A policy belongs to a row, so one written for either key lapses when
// the key rotates to a value never registered before, which is a new row; a
// value registered before gets its old row back, and that row's policy.
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
	// Read credential and current owner authority in one database snapshot.
	// Leave apikey.Authenticate legacy-only: private receivers still use it.
	var c caller
	var role string
	err := h.cfg.Keys.QueryRow(r.Context(), `SELECT k.id, coalesce(k.owner_principal_id,''), coalesce(p.role,'')
		FROM api_keys k LEFT JOIN principals p ON p.id=k.owner_principal_id
		WHERE coalesce(k.key_hash,k.owned_key_hash)=$1 AND k.status='active'
		  AND (k.expires_at IS NULL OR k.expires_at > clock_timestamp())
		  AND (k.owner_principal_id IS NULL OR
		       (p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'))`,
		apikey.Hash(key)).Scan(&c.keyID, &c.principalID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return caller{}, unauthenticated("invalid x-api-key")
	}
	if err != nil {
		return caller{}, internal(r, "api key lookup failed", err)
	}
	if c.principalID != "" {
		if role != "developer" && role != "admin" {
			return caller{}, forbidden("calling models requires the developer role")
		}
		return c, nil
	}
	if p, ok := h.cfg.Catalog.Snapshot().KeyPolicy(c.keyID); ok {
		c.policy = p
		return c, nil
	}
	if !h.knownByValue(key) {
		return caller{}, forbidden("this API key has no model grant; an administrator grants one in the console")
	}
	return c, nil
}

// knownByValue reports whether key is the bootstrap key or the brain's. It
// compares digests, so each comparison is constant-time in the key's length
// as well as its bytes.
func (h *handler) knownByValue(key string) bool {
	sum := sha256.Sum256([]byte(key))
	known := 0
	for _, k := range h.known {
		known |= subtle.ConstantTimeCompare(sum[:], k[:])
	}
	return known == 1
}
