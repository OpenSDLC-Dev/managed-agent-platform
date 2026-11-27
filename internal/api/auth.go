package api

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IssuedKeyPrefix marks a management key this platform minted, beside plan 30's
// `sk-map-env01-` for worker credentials. It is public by construction: it is the
// same for every key, so showing it identifies the *kind* of credential without
// revealing anything about a particular one.
const IssuedKeyPrefix = "sk-map-api01-"

// partialKeyHint is the masked form a listing shows. It is computed from the
// plaintext because it cannot be recovered from the hash, and it is the only part
// of a key that outlives issuance.
//
// Two rules, because there are two kinds of value here and only one has a format
// this platform knows.
//
// A key we minted carries IssuedKeyPrefix, which is public, so the hint may show
// it in full and then three characters, an ellipsis, and the final four —
// mirroring the reference's `sk-ant-api03-R2D...igAA`. The split is taken at a
// fixed offset from the *known* prefix, never at the last separator in the value:
// the body is base64url (envkeys.go mints with base64.RawURLEncoding), whose
// alphabet includes `-`, so a last-separator rule would move the split to wherever
// a random dash happened to land and could leave a key almost entirely published.
//
// Historical opaque values expose only their final four runes. Both formats
// require enough hidden entropy, and invalid UTF-8 produces no hint.
func partialKeyHint(key string) string {
	const lead, tail = 3, 4
	// An issued key's body must hide at least as much as it shows; an opaque one,
	// where nothing is public, at least three times as much. Minted bodies are 43
	// base64url characters, so the issued floor is slack rather than a constraint —
	// which is the point: the rule stays safe by construction, not by luck.
	const minIssuedBody, minOpaque = 2 * (lead + tail), 4 * tail
	if !utf8.ValidString(key) {
		return ""
	}
	if strings.HasPrefix(key, IssuedKeyPrefix) {
		body := []rune(key[len(IssuedKeyPrefix):])
		if len(body) < minIssuedBody {
			return ""
		}
		return IssuedKeyPrefix + string(body[:lead]) + "..." + string(body[len(body)-tail:])
	}
	r := []rune(key)
	if len(r) < minOpaque {
		return ""
	}
	return "..." + string(r[len(r)-tail:])
}

// requireAPIKey admits owned keys only on public /v1 in either header.
// The selected key's ID remains the audit principal (sessions.created_by).
func requireAPIKey(pool *pgxpool.Pool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A repeated field is refused before the value is read. HTTP allows one,
		// no real client sends one, and Header.Get would silently pick the first —
		// so without this the answer to "which key authenticated?" would depend on
		// header order, and it would differ from the answer apiKeyOffered gives
		// when choosing the lane. One rule in both places: a duplicate credential
		// is ambiguous, and ambiguous is a 401.
		if len(r.Header.Values("x-api-key")) > 1 {
			writeError(w, r, errAuth("multiple x-api-key headers"))
			return
		}
		key := r.Header.Get("x-api-key")
		requestedPublic, routedPublic := publicAPIPaths(r)
		public := requestedPublic && routedPublic
		ownedBearer := key == "" && public && issuedAPIKeyBearer(r)
		if ownedBearer {
			if len(r.Header.Values("Authorization")) > 1 {
				writeError(w, r, errAuth("multiple Authorization headers"))
				return
			}
			key, _ = bearerToken(r)
		}
		if key == "" {
			// The reference's words for a request offering no key (2026-09-19
			// self-hosted-docker worker-network idx 0; #540), and its
			// `x-should-retry: false` (#842).
			writeError(w, r, noRetry(errAuth("x-api-key header is required")))
			return
		}
		if !public {
			writeError(w, r, errAuth("a browser session cookie is required"))
			return
		}
		authority, err := authenticateManagementKey(r.Context(), pool, key)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if authority.KeyID == "" {
			writeError(w, r, errAuth("invalid x-api-key"))
			return
		}
		if !workspace.Matches(r.Header.Values("anthropic-workspace-id"), authority.WorkspacePublicID) {
			writeError(w, r, errForbidden("workspace is not served by this platform"))
			return
		}
		markVerified(r.Context())
		ctx := context.WithValue(workspace.WithID(r.Context(), authority.WorkspaceID), ctxKeyPrincipal, authority.KeyID)
		ctx = context.WithValue(ctx, ownedKeyContextKey{}, authority)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFrom is audit attribution: the selected owned credential row on the
// public lane, or the human principal on the cookie lane.
func principalFrom(ctx context.Context) string {
	if p, _ := ctx.Value(ctxKeyPrincipal).(string); p != "" {
		return p
	}
	if p, ok := identityFrom(ctx); ok {
		return p.ID
	}
	return ""
}
