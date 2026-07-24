// Package local is the AES-256-GCM secrets.Cipher for tests and minimal
// deployments: one 32-byte master key from configuration, no external
// service. Production deployments use internal/secrets/openbao; this backend
// keeps `make test` and bao-less installs working (docs/plan/12, D1).
package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// Config carries the master key and the identifier stored next to every
// ciphertext. A new KeyID with a new Key is how an operator rotates: old rows
// keep decrypting under the old pair for as long as it stays configured.
type Config struct {
	KeyID string
	Key   []byte // exactly 32 bytes
}

// Cipher is the local AES-256-GCM implementation of secrets.Cipher.
type Cipher struct {
	keyID string
	aead  cipher.AEAD
}

// New validates the config eagerly: a wrong-size key must fail at startup,
// not on the first credential write.
func New(cfg Config) (*Cipher, error) {
	if cfg.KeyID == "" {
		return nil, errors.New("local cipher: KeyID is required")
	}
	if len(cfg.Key) != 32 {
		return nil, fmt.Errorf("local cipher: key must be exactly 32 bytes, got %d", len(cfg.Key))
	}
	block, err := aes.NewCipher(cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("local cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("local cipher: %w", err)
	}
	return &Cipher{keyID: cfg.KeyID, aead: aead}, nil
}

// Encrypt seals plaintext under a fresh random nonce, with the key id as
// additional authenticated data so a ciphertext cannot be replayed under a
// different key id. Layout: nonce || sealed.
func (c *Cipher) Encrypt(_ context.Context, plaintext []byte) ([]byte, string, error) {
	if len(plaintext) == 0 {
		return nil, "", errors.New("local cipher: empty plaintext")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("local cipher: nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, []byte(c.keyID)), c.keyID, nil
}

// Decrypt reverses Encrypt. A keyID this cipher does not hold, or ciphertext
// that fails authentication, is an error.
func (c *Cipher) Decrypt(_ context.Context, ciphertext []byte, keyID string) ([]byte, error) {
	if keyID != c.keyID {
		return nil, fmt.Errorf("local cipher: key %q not held (holding %q)", keyID, c.keyID)
	}
	if len(ciphertext) <= c.aead.NonceSize() {
		return nil, errors.New("local cipher: ciphertext shorter than a nonce")
	}
	nonce, sealed := ciphertext[:c.aead.NonceSize()], ciphertext[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, sealed, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("local cipher: decrypt: %w", err)
	}
	return plaintext, nil
}
