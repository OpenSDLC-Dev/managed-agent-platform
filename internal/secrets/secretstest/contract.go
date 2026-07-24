package secretstest

import (
	"bytes"
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// Run is the shared contract suite every secrets.Cipher backend must pass.
// newCipher is called once per subtest so backends isolate fixtures (a fresh
// transit key per subtest for OpenBao).
func Run(t *testing.T, newCipher func(t *testing.T) secrets.Cipher) {
	ctx := context.Background()

	t.Run("RoundTrip", func(t *testing.T) {
		c := newCipher(t)
		for _, plaintext := range [][]byte{
			[]byte("s"),
			[]byte("ntn_a-typical-api-key-value"),
			[]byte("binary\x00\xff\xfe payload"),
			bytes.Repeat([]byte("0123456789abcdef"), 4096), // 64 KiB
		} {
			ct, keyID, err := c.Encrypt(ctx, plaintext)
			if err != nil {
				t.Fatalf("Encrypt(%d bytes): %v", len(plaintext), err)
			}
			if keyID == "" {
				t.Fatal("Encrypt returned an empty keyID")
			}
			got, err := c.Decrypt(ctx, ct, keyID)
			if err != nil {
				t.Fatalf("Decrypt(%d bytes): %v", len(plaintext), err)
			}
			if !bytes.Equal(got, plaintext) {
				t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(plaintext))
			}
		}
	})

	t.Run("CiphertextHidesPlaintext", func(t *testing.T) {
		c := newCipher(t)
		plaintext := []byte("zd_live_super-secret-token")
		ct, _, err := c.Encrypt(ctx, plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if bytes.Contains(ct, plaintext) {
			t.Fatal("ciphertext contains the plaintext verbatim")
		}
	})

	t.Run("FreshEncryptionsDiffer", func(t *testing.T) {
		c := newCipher(t)
		plaintext := []byte("same input twice")
		a, keyA, err := c.Encrypt(ctx, plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		b, keyB, err := c.Encrypt(ctx, plaintext)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if bytes.Equal(a, b) {
			t.Fatal("two encryptions of the same plaintext produced identical ciphertext")
		}
		if keyA != keyB {
			t.Fatalf("keyID drifted between encryptions: %q vs %q", keyA, keyB)
		}
	})

	t.Run("TamperDetected", func(t *testing.T) {
		c := newCipher(t)
		ct, keyID, err := c.Encrypt(ctx, []byte("integrity matters"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		tampered := bytes.Clone(ct)
		tampered[len(tampered)-1] ^= 0x01
		if _, err := c.Decrypt(ctx, tampered, keyID); err == nil {
			t.Fatal("Decrypt accepted tampered ciphertext")
		}
	})

	t.Run("TruncatedCiphertextRejected", func(t *testing.T) {
		c := newCipher(t)
		ct, keyID, err := c.Encrypt(ctx, []byte("short and sweet"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		for _, n := range []int{0, 1, len(ct) / 2} {
			if _, err := c.Decrypt(ctx, ct[:n], keyID); err == nil {
				t.Fatalf("Decrypt accepted ciphertext truncated to %d bytes", n)
			}
		}
	})

	t.Run("UnknownKeyIDRejected", func(t *testing.T) {
		c := newCipher(t)
		ct, _, err := c.Encrypt(ctx, []byte("bound to its key"))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if _, err := c.Decrypt(ctx, ct, "not-a-key-this-cipher-holds"); err == nil {
			t.Fatal("Decrypt accepted a keyID the cipher does not hold")
		}
	})
}
