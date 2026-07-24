package local_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/secretstest"
)

func testKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func TestContract(t *testing.T) {
	secretstest.Run(t, func(t *testing.T) secrets.Cipher {
		c, err := local.New(local.Config{KeyID: "local-1", Key: testKey()})
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		return c
	})
}

func TestNewValidation(t *testing.T) {
	if _, err := local.New(local.Config{KeyID: "", Key: testKey()}); err == nil {
		t.Fatal("New accepted an empty KeyID")
	}
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := local.New(local.Config{KeyID: "k", Key: bytes.Repeat([]byte{1}, n)}); err == nil {
			t.Fatalf("New accepted a %d-byte key", n)
		} else if !strings.Contains(err.Error(), "32 bytes") {
			t.Fatalf("key-size error does not name the requirement: %v", err)
		}
	}
}

func TestEmptyPlaintextRejected(t *testing.T) {
	c, err := local.New(local.Config{KeyID: "local-1", Key: testKey()})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	if _, _, err := c.Encrypt(context.Background(), nil); err == nil {
		t.Fatal("Encrypt accepted empty plaintext")
	}
}

// A ciphertext sealed under one key id must not decrypt under another cipher
// holding the same key bytes but a different id: the id is authenticated data,
// so re-labelling a key without re-encrypting is caught rather than silently
// accepted.
func TestKeyIDIsAuthenticated(t *testing.T) {
	ctx := context.Background()
	a, err := local.New(local.Config{KeyID: "local-1", Key: testKey()})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	b, err := local.New(local.Config{KeyID: "local-2", Key: testKey()})
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	ct, _, err := a.Encrypt(ctx, []byte("bound to local-1"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := b.Decrypt(ctx, ct, "local-2"); err == nil {
		t.Fatal("ciphertext sealed under key id local-1 decrypted under local-2")
	}
}
