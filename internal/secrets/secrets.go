// Package secrets is the platform's encryption seam for vault credential
// material (docs/plan/12_vaults-credentials.md, D1): reversible encryption of
// small secret values behind the one interface every backend must satisfy
// (CLAUDE.md: backend variability lives behind an interface with one shared
// contract suite — internal/secrets/secretstest). Postgres stays the canonical
// store — callers persist the ciphertext and key id next to the resource row;
// this package only transforms bytes. The production backend is OpenBao's
// transit engine (internal/secrets/openbao); internal/secrets/gcpkms is the
// Cloud KMS backend a GCP deployment uses instead, and internal/secrets/local
// is the AES-256-GCM fallback for tests and minimal deployments.
//
// This package holds the interface and the errors backends wrap, so it imports
// none of them: selecting one from the environment is internal/secrets/backend,
// a sibling for the same structural reason internal/sandbox/backend is one.
package secrets

import (
	"context"
	"errors"
)

// ErrPlaintextTooLarge reports material a cipher cannot seal because its
// encryption service bounds the input. Not every backend has such a bound —
// local and openbao seal any size — so this is a contract for the ones that
// do: they wrap it, and a caller turning a Cipher failure into a response
// classifies it as the caller's error rather than the server's. Without that,
// the refusal reaches the client as a generic 500 and the useful message lives
// only in the server's log (docs/plan/20_gcp-deployment.md, Decision 3).
var ErrPlaintextTooLarge = errors.New("secrets: plaintext is too large for this cipher")

// Cipher encrypts and decrypts vault secret material. Implementations must
// bind each ciphertext to the key that produced it: Decrypt with a keyID the
// ciphertext was not encrypted under is an error, never a silent success.
type Cipher interface {
	// Encrypt seals plaintext (non-empty) and names the key that sealed it.
	// The ciphertext is opaque to callers; store it with the keyID and hand
	// both back to Decrypt.
	Encrypt(ctx context.Context, plaintext []byte) (ciphertext []byte, keyID string, err error)

	// Decrypt reverses Encrypt. Tampered, truncated, or foreign ciphertext —
	// or a keyID this cipher does not hold — is an error.
	Decrypt(ctx context.Context, ciphertext []byte, keyID string) ([]byte, error)
}
