package login

import (
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
func HashPassword(password string) (string, error) {
	if !validPassword(password) {
		return "", errors.New("login: password must be 12–1024 UTF-8 bytes without NUL")
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	key := passwordKey(password, salt)
	return "$argon2id$v=19$" + passwordPHCParameters + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

// VerifyPassword fails closed on malformed PHC, before allocating Argon2 memory.
// Pass an empty encoded value for an unknown login: it performs the same bounded
// dummy derivation and comparison, but can never authenticate that account.
func VerifyPassword(encoded, password string) bool {
	if !validPassword(password) {
		return false
	}
	unknown := encoded == ""
	if unknown {
		encoded = dummyPasswordHash
	}
	if len(encoded) > 128 {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != passwordPHCParameters || len(parts[4]) != 22 || len(parts[5]) != 43 {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		return false
	}
	key := passwordKey(password, salt)
	matches := subtle.ConstantTimeCompare(key, expected) == 1
	return matches && !unknown
}

// Both hashing and verification, including unknown-account work, share eight
// process-wide slots. Neither a stored PHC nor a caller can change this bound.
func passwordKey(password string, salt []byte) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	return argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
}
