package apikey

import (
	"context"
	"errors"
)

// ErrOwnedPreset refuses to turn a principal's credential into preset authority.
// It carries neither the configured value nor its digest, so startup may log it.
var ErrOwnedPreset = errors.New("configured preset matches a principal-owned API key")

// CheckPresetOwnership checks every nonempty configured value, including owned
// keys that are inactive, archived or expired. Unknown and unowned values remain
// valid preset candidates. Both servers call it before exposing preset authority.
func CheckPresetOwnership(ctx context.Context, db Querier, keys ...string) error {
	hashes := make([]string, 0, len(keys))
	for _, key := range keys {
		if key != "" {
			hashes = append(hashes, Hash(key))
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	var owned bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys
		WHERE owner_principal_id IS NOT NULL AND coalesce(key_hash, owned_key_hash) = ANY($1))`, hashes).Scan(&owned); err != nil {
		return err
	}
	if owned {
		return ErrOwnedPreset
	}
	return nil
}
