// Package apikey is the platform API key check, shared by the two servers a
// key authenticates to: the control plane, and the model gateway that serves
// inference to the same keys (docs/plan/59_model-gateway.md). One check in one
// place means a key revoked or expired stops at the same instant on both.
package apikey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/textproto"

	"github.com/jackc/pgx/v5"
)

// Hash derives the stored form of an API key. Only this hash ever touches the
// database.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Querier is what Authenticate reads through: a pool or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Authenticate resolves a key to its row ID, or "" if the key is unknown, not
// active, or past its expiry.
//
// Expiry is evaluated here rather than swept: a key whose expires_at has passed
// stops authenticating the moment it passes, with no background job to be down.
// The comparison is against the database's clock, the same one that stamped
// created_at, so a replica with a skewed clock cannot extend or shorten a
// credential's life.
func Authenticate(ctx context.Context, db Querier, key string) (string, error) {
	var id string
	err := db.QueryRow(ctx,
		`SELECT id FROM api_keys
		 WHERE key_hash = $1 AND status = 'active'
		   AND (expires_at IS NULL OR expires_at > now())`,
		Hash(key)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CheckBrainKey says why brain cannot serve as the brain's key beside the
// bootstrap key boot, or returns nil when it can, as an empty brain key —
// none configured — always can. The control plane and the gateway both ask
// it, so the two refuse the same values. Its error reads after the variable's
// name.
func CheckBrainKey(boot, brain string) error {
	switch {
	case brain == "":
		return nil
	case brain == boot:
		// The control plane registers each key by its value under its own
		// name, so one value under both would move its row between them.
		return errors.New("must differ from the bootstrap key")
	case textproto.TrimString(brain) != brain:
		// HTTP trims a header value's surrounding whitespace, so such a key
		// arrives as another: the bootstrap key, if the two differ by
		// nothing else.
		return errors.New("has leading or trailing whitespace, which no request can carry")
	}
	return nil
}
