package gcpkms_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/gcpkms"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/gcpkms/gcpkmstest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/secretstest"
)

func newCipher(t *testing.T) *gcpkms.Cipher {
	t.Helper()
	c, err := gcpkms.New(context.Background(), gcpkms.Config{
		KeyName: gcpkmstest.KeyName,
		Client:  gcpkmstest.NewClient(t),
	})
	if err != nil {
		t.Fatalf("gcpkms.New: %v", err)
	}
	return c
}

// The same suite every other backend passes. It is what makes "a cipher" mean
// one thing across three very different implementations.
func TestContract(t *testing.T) {
	secretstest.Run(t, func(t *testing.T) secrets.Cipher { return newCipher(t) })
}

// The ceiling is the whole reason this backend needed a sentinel, so it is
// pinned at the boundary rather than somewhere safely past it: the contract's
// own largest case is 64 KiB, which is Cloud KMS's limit exactly, with zero
// margin. One byte more must fail, and fail as the caller's error.
func TestTheCeilingIsExactAndClassified(t *testing.T) {
	ctx := context.Background()
	c := newCipher(t)

	at := bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytes)
	ct, keyID, err := c.Encrypt(ctx, at)
	if err != nil {
		t.Fatalf("Encrypt at the ceiling (%d bytes): %v", len(at), err)
	}
	got, err := c.Decrypt(ctx, ct, keyID)
	if err != nil {
		t.Fatalf("Decrypt at the ceiling: %v", err)
	}
	if !bytes.Equal(got, at) {
		t.Fatal("the ceiling-sized plaintext did not round-trip")
	}

	over := bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytes+1)
	_, _, err = c.Encrypt(ctx, over)
	if !errors.Is(err, secrets.ErrPlaintextTooLarge) {
		t.Fatalf("one byte over the ceiling = %v, want it to wrap ErrPlaintextTooLarge", err)
	}
	// The message is what a 4xx shows the caller, so it has to name the limit
	// rather than merely report that one was hit.
	for _, want := range []string{"65536", "65537"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %s", err, want)
		}
	}
}

// The marker is an escape hatch for envelope encryption (#242), and an escape
// hatch nobody exercises is a comment. These are the four ways it has to
// behave for a later v2 to be introducible without rewriting stored rows.
func TestTheCiphertextFormatMarker(t *testing.T) {
	ctx := context.Background()
	c := newCipher(t)
	ct, keyID, err := c.Encrypt(ctx, []byte("marked"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	const marker = "gcpkms:v1:"
	if !bytes.HasPrefix(ct, []byte(marker)) {
		t.Fatalf("ciphertext does not start with %q", marker)
	}
	raw := bytes.TrimPrefix(ct, []byte(marker))

	for _, tc := range []struct {
		name, wantIn string
		in           []byte
	}{
		// Raw KMS ciphertext — what a naive implementation would have stored.
		// Refused, so a row written by a marker-less build could never be
		// mistaken for a current one.
		{"unmarked", "format marker", raw},
		// The case the marker exists for: a v2 row reaching a v1 build is
		// named, not handed to KMS as if it were raw.
		{"a newer format version", "version 2", append([]byte("gcpkms:v2:"), raw...)},
		{"marker and nothing else", "nothing else", []byte(marker)},
		{"a version that is not a number", "not a number", []byte("gcpkms:vX:payload")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Decrypt(ctx, tc.in, keyID)
			if err == nil {
				t.Fatal("Decrypt accepted it")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// A key name that is well-formed but wrong is the expensive mistake: Encrypt
// accepts a CryptoKeyVersion where Decrypt does not, so a deployment
// configured with one would seal credentials it could never unseal.
func TestNewRejectsKeyNamesThatAreNotACryptoKey(t *testing.T) {
	for _, name := range []string{
		"",
		"test-key",
		"projects/p/locations/l/keyRings/r",
		"projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		"projects/p/locations/l/keyRings/r/cryptoKeys/",
		"/projects/p/locations/l/keyRings/r/cryptoKeys/k",
	} {
		if _, err := gcpkms.New(context.Background(), gcpkms.Config{
			KeyName: name, Client: gcpkmstest.NewClient(t),
		}); err == nil {
			t.Errorf("New accepted KeyName %q", name)
		}
	}
}

// A well-formed name the service will not serve — a typo, or an IAM binding
// that was never made — must stop the process at startup, not fail the first
// credential write hours later.
func TestNewProvesTheKeyIsUsable(t *testing.T) {
	_, err := gcpkms.New(context.Background(), gcpkms.Config{
		KeyName: "projects/somewhere-else/locations/global/keyRings/r/cryptoKeys/k",
		Client:  gcpkmstest.NewClient(t),
	})
	if err == nil {
		t.Fatal("New accepted a key the service does not hold")
	}
	if !strings.Contains(err.Error(), "probe encrypt") {
		t.Errorf("error %q does not say the startup probe failed", err)
	}
}

// Empty plaintext is not a size the ceiling covers, and sealing it would
// produce a credential row whose secrets decrypt to nothing.
func TestEncryptRejectsEmptyPlaintext(t *testing.T) {
	if _, _, err := newCipher(t).Encrypt(context.Background(), nil); err == nil {
		t.Fatal("Encrypt accepted empty plaintext")
	}
}

// The live tier: the same contract against real Cloud KMS. Skipped unless
// RUN_LIVE_KMS_TESTS is set, and a hard failure — never a skip — when it is
// set and GCPKMS_KEY_NAME is not.
func TestLiveContract(t *testing.T) {
	keyName := gcpkmstest.LiveKeyName(t)
	secretstest.Run(t, func(t *testing.T) secrets.Cipher {
		c, err := gcpkms.New(context.Background(), gcpkms.Config{KeyName: keyName})
		if err != nil {
			t.Fatalf("gcpkms.New against real Cloud KMS: %v", err)
		}
		return c
	})
}

// The ceiling measured against the real service, since a fake enforcing a
// constant proves only that the constant is consistent with itself.
func TestLiveCeiling(t *testing.T) {
	keyName := gcpkmstest.LiveKeyName(t)
	ctx := context.Background()
	c, err := gcpkms.New(ctx, gcpkms.Config{KeyName: keyName})
	if err != nil {
		t.Fatalf("gcpkms.New against real Cloud KMS: %v", err)
	}
	if _, _, err := c.Encrypt(ctx, bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytes)); err != nil {
		t.Errorf("real Cloud KMS refused %d bytes, which MaxPlaintextBytes says it accepts: %v",
			gcpkms.MaxPlaintextBytes, err)
	}
	if _, _, err := c.Encrypt(ctx, bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytes+1)); !errors.Is(err, secrets.ErrPlaintextTooLarge) {
		t.Errorf("one byte over = %v, want ErrPlaintextTooLarge", err)
	}
}
