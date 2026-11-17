// Package apikey supplies API key hashing and the database query interface.
// Public management and gateway inference check owned credentials together
// with the owner's current authority in their respective authentication paths.
package apikey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/jackc/pgx/v5"
)

// Hash derives the stored form of an API key. Only this hash ever touches the
// database.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Querier reads credential and owner authority through a pool or transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
