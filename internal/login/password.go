package login

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// One fixed policy keeps attacker-controlled PHC parameters from choosing an
// allocation size or work factor. Changes need an explicit migration policy.
const passwordPHCParameters = "m=65536,t=3,p=1"
const dummyPasswordHash = "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

var argonSlots = make(chan struct{}, 8)

func validPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024 && utf8.ValidString(password) && !strings.ContainsRune(password, 0)
}

// HashPassword applies the fixed Argon2id v19 policy with a fresh 16-byte salt.
// The returned PHC string carries no password. Callers throttle before hashing.
func HashPassword(ctx context.Context, password string) (string, error) {
	if !validPassword(password) {
		return "", errors.New("login: password must be 12–1024 UTF-8 bytes without NUL")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return "", err
	}
	return "$argon2id$v=19$" + passwordPHCParameters + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword fails closed on malformed PHC, before allocating Argon2 memory.
// Pass an empty encoded value for an unknown login: it performs the same bounded
// dummy derivation and comparison, but can never authenticate that account.
// A mismatch returns false, nil; canceled queue admission returns the context error.
func VerifyPassword(ctx context.Context, encoded, password string) (bool, error) {
	if !validPassword(password) {
		return false, nil
	}
	unknown := encoded == ""
	if unknown {
		encoded = dummyPasswordHash
	}
	if len(encoded) > 128 {
		return false, nil
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != passwordPHCParameters || len(parts[4]) != 22 || len(parts[5]) != 43 {
		return false, nil
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false, nil
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		return false, nil
	}
	key, err := passwordKey(ctx, password, salt)
	if err != nil {
		return false, err
	}
	matches := subtle.ConstantTimeCompare(key, expected) == 1
	return matches && !unknown, nil
}

// Both hashing and verification, including unknown-account work, share eight
// process-wide slots. Cancellation withdraws queued work, not an in-flight
// Argon2 derivation. Neither a stored PHC nor a caller can change this bound.
func passwordKey(ctx context.Context, password string, salt []byte) ([]byte, error) {
	select {
	case argonSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-argonSlots }()
	// Acquisition and cancellation may both be ready. Do not derive if the
	// select chose a slot for a request that was already canceled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32), nil
}
