package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hashKey derives the stored form of an API key. Only this hash ever touches
// the database.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

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
// A CONTROLPLANE_API_KEY is operator-chosen and may be anything, so NOTHING in it
// may be assumed public — its leading run is not a prefix this platform knows, and
// reading it as one would render `secret-12345678` as `secret-123...5678`,
// fourteen of its fifteen characters. Such a value gets its last four characters
// only.
//
// Both paths refuse to produce a hint at all below a length floor, because "a
// masked value that is mostly the value" is worse than an empty column — and worse
// than it looks, since key_hash is an unsalted SHA-256 that a mostly-known
// plaintext makes trivially searchable offline.
//
// Slicing is by rune, not byte. A key is an arbitrary environment-variable string
// and may be non-ASCII; cutting mid-rune yields invalid UTF-8, which Postgres
// refuses on a text column — an EnsureAPIKey error at boot, i.e. a control plane
// that will not start because of the *hint*. A value that is not valid UTF-8 at
// all gets no hint for the same reason.
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

// EnsureAPIKey makes key the one live credential for the named logical key:
// it inserts (or reactivates) the hash and archives every other live key
// under the same name. That gives rotation-by-restart semantics — changing
// CONTROLPLANE_API_KEY and restarting cmd/controlplane retires the previous
// key instead of leaving it valid forever. All replicas must therefore share
// one key value per name; replicas booting with *different* values for one name
// race, and api_keys_one_live_unissued resolves that by failing the loser's
// transaction rather than leaving the name with two live credentials.
//
// It only ever writes rows with created_by NULL, which is what puts them under
// that index and marks them env-var-managed. A key issued over the console
// records its issuer and is deliberately outside the one-live rule (plan 32).
func EnsureAPIKey(ctx context.Context, pool *pgxpool.Pool, name, key string) error {
	hash := hashKey(key)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Archive before inserting: api_keys_one_live_unissued admits one active
	// unissued row per name and Postgres enforces it per statement, so
	// registering the replacement while the incumbent is still live would fail
	// every rotation.
	if _, err := tx.Exec(ctx,
		`UPDATE api_keys SET status = 'archived'
		 WHERE name = $1 AND key_hash <> $2 AND status = 'active' AND created_by IS NULL`,
		name, hash); err != nil {
		return err
	}
	// created_by and expires_at are RESET, not preserved, when an existing row is
	// adopted. The conflict path fires when the configured value already exists as
	// a row, and that row may have been issued over the console — carrying an
	// issuer and possibly an expiry. Leaving either in place would break both
	// halves of this function's contract: an issued row sits outside
	// api_keys_one_live_unissued, so the archive above would skip it on the next
	// rotation and leave two live credentials under one name; and an inherited
	// expiry would let EnsureAPIKey report success over a key authenticate then
	// refuses, i.e. a control plane that starts without a working bootstrap
	// credential. Naming a value in CONTROLPLANE_API_KEY makes it env-var-managed,
	// whatever it was before.
	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash, partial_key_hint) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (key_hash) DO UPDATE
		 SET status = 'active', name = EXCLUDED.name, partial_key_hint = EXCLUDED.partial_key_hint,
		     created_by = NULL, expires_at = NULL`,
		domain.NewID("apikey").String(), name, hash, partialKeyHint(key)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// authenticate resolves an x-api-key value to the key's row ID, or "" if the key
// is unknown, not active, or past its expiry.
//
// Expiry is evaluated here rather than swept: a key whose expires_at has passed
// stops authenticating the moment it passes, with no background job to be down.
// The comparison is against the database's clock, the same one that stamped
// created_at, so a control-plane replica with a skewed clock cannot extend or
// shorten a credential's life.
func authenticate(ctx context.Context, pool *pgxpool.Pool, key string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM api_keys
		 WHERE key_hash = $1 AND status = 'active'
		   AND (expires_at IS NULL OR expires_at > now())`,
		hashKey(key)).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return id, err
}

// requireAPIKey is the management-auth middleware: every /v1 route needs a
// valid, unrevoked x-api-key. The authenticated key's ID is stored in the
// request context as the audit principal (sessions.created_by).
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
		if key == "" {
			writeError(w, r, errAuth("missing x-api-key header"))
			return
		}
		principal, err := authenticate(r.Context(), pool, key)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if principal == "" {
			writeError(w, r, errAuth("invalid x-api-key"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal, principal)))
	})
}

// principalFrom is the audit answer to "who made this request" — the value
// sessions.created_by records. It resolves either lane's principal: the api key's
// name on the machine lane, and the human's `principal_` id on the identity lane
// (plan 31 slice 2, #56).
//
// Reading only ctxKeyPrincipal was the pre-plan-31 shape, when a machine key was
// the only thing that could reach a mutation. Left that way, the moment slice 3
// lets a human create a session the row would record NO creator at all — silently,
// since created_by is nullable and nothing checks it. The whole point of a stable
// `principal_` id is that an audit trail can name the human it belongs to.
//
// The machine lane wins when both are somehow set, matching dispatch: only one is
// ever populated today, and if that ever changed, the credential that
// authenticated the request is the machine one.
func principalFrom(ctx context.Context) string {
	if p, _ := ctx.Value(ctxKeyPrincipal).(string); p != "" {
		return p
	}
	if p, ok := identityFrom(ctx); ok {
		return p.ID
	}
	return ""
}
