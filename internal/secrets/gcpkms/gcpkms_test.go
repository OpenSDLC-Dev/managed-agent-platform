package gcpkms_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/wrapperspb"

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

	at := bytes.Repeat([]byte("a"), c.MaxPlaintext())
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

	over := bytes.Repeat([]byte("a"), c.MaxPlaintext()+1)
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
	if c.MaxPlaintext() != gcpkms.MaxPlaintextBytes {
		t.Errorf("a software key resolved to a %d-byte ceiling, want %d",
			c.MaxPlaintext(), gcpkms.MaxPlaintextBytes)
	}
}

// The ceiling is a property of the KEY, not of Cloud KMS: an HSM key version
// bounds plaintext at 8 KiB, and a resource name does not say which you have.
// A backend that assumed the software ceiling would pass every test above and
// still hand a caller the uninformative 500 this slice exists to remove — the
// refusal would come from the service as a bare InvalidArgument, which carries
// no sentinel to classify. So the ceiling is read from the key at startup, and
// this pins that.
func TestAnHSMKeyGetsTheSmallerCeiling(t *testing.T) {
	ctx := context.Background()
	c, err := gcpkms.New(ctx, gcpkms.Config{
		KeyName: gcpkmstest.KeyName,
		Client:  gcpkmstest.NewHSMClient(t),
	})
	if err != nil {
		t.Fatalf("gcpkms.New against an HSM key: %v", err)
	}
	if c.MaxPlaintext() != gcpkms.MaxPlaintextBytesHSM {
		t.Fatalf("an HSM key resolved to a %d-byte ceiling, want %d",
			c.MaxPlaintext(), gcpkms.MaxPlaintextBytesHSM)
	}
	if _, _, err := c.Encrypt(ctx, bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytesHSM)); err != nil {
		t.Fatalf("Encrypt at the HSM ceiling: %v", err)
	}
	// The size the software ceiling would have waved through. It must be the
	// classified refusal, not the service's InvalidArgument.
	_, _, err = c.Encrypt(ctx, bytes.Repeat([]byte("a"), gcpkms.MaxPlaintextBytesHSM+1))
	if !errors.Is(err, secrets.ErrPlaintextTooLarge) {
		t.Fatalf("one byte over the HSM ceiling = %v, want it to wrap ErrPlaintextTooLarge", err)
	}
	if !strings.Contains(err.Error(), "8192") {
		t.Errorf("refusal %q does not name the HSM limit", err)
	}
}

// The response-integrity guards can only be reached from a service that answers
// wrongly, so without a fault-injection seam every one of them could be deleted
// with the suite still green. Each row is a response shape that must not be
// stored: the empty-ciphertext one is the sharp case, because an absent
// checksum reads as 0 and CRC32C("") is also 0, so it would compare EQUAL and
// be stored as a marker with nothing behind it — a credential that can never be
// unsealed.
func TestAMisbehavingServiceIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, wantIn string
		mangle       func(*kmspb.EncryptResponse)
	}{
		{"empty ciphertext with no checksum", "no ciphertext", func(r *kmspb.EncryptResponse) {
			r.Ciphertext = nil
			r.CiphertextCrc32C = nil
		}},
		{"no ciphertext checksum", "no ciphertext checksum", func(r *kmspb.EncryptResponse) {
			r.CiphertextCrc32C = nil
		}},
		{"wrong ciphertext checksum", "checksum mismatch", func(r *kmspb.EncryptResponse) {
			r.CiphertextCrc32C = wrapperspb.Int64(r.GetCiphertextCrc32C().GetValue() ^ 1)
		}},
		{"request checksum not verified", "did not verify", func(r *kmspb.EncryptResponse) {
			r.VerifiedPlaintextCrc32C = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := gcpkmstest.NewClientFor(t, &mangling{
				KeyManagementServiceServer: gcpkmstest.NewServer(t, kmspb.ProtectionLevel_SOFTWARE),
				encrypt:                    tc.mangle,
			})
			// The startup probe seals too, so a service this broken cannot even
			// produce a cipher — which is the right moment to find out.
			_, err := gcpkms.New(ctx, gcpkms.Config{KeyName: gcpkmstest.KeyName, Client: client})
			if err == nil {
				t.Fatal("New accepted a service returning a corrupt response")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// The decrypt half of the same contract, reached the same way.
func TestAMisbehavingDecryptIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, wantIn string
		mangle       func(*kmspb.DecryptResponse)
	}{
		{"no plaintext checksum", "no plaintext checksum", func(r *kmspb.DecryptResponse) {
			r.PlaintextCrc32C = nil
		}},
		{"wrong plaintext checksum", "checksum mismatch", func(r *kmspb.DecryptResponse) {
			r.PlaintextCrc32C = wrapperspb.Int64(r.GetPlaintextCrc32C().GetValue() ^ 1)
		}},
		{"no plaintext at all", "no plaintext", func(r *kmspb.DecryptResponse) {
			r.Plaintext = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := gcpkmstest.NewClientFor(t, &mangling{
				KeyManagementServiceServer: gcpkmstest.NewServer(t, kmspb.ProtectionLevel_SOFTWARE),
				decrypt:                    tc.mangle,
			})
			c, err := gcpkms.New(ctx, gcpkms.Config{KeyName: gcpkmstest.KeyName, Client: client})
			if err != nil {
				t.Fatalf("gcpkms.New: %v", err)
			}
			ct, keyID, err := c.Encrypt(ctx, []byte("round trip me"))
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			_, err = c.Decrypt(ctx, ct, keyID)
			if err == nil {
				t.Fatal("Decrypt accepted a corrupt response")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// mangling wraps the fake and corrupts one field of its answer, so the cipher's
// own response checks are the only thing standing between the corruption and a
// stored credential.
type mangling struct {
	kmspb.KeyManagementServiceServer
	encrypt func(*kmspb.EncryptResponse)
	decrypt func(*kmspb.DecryptResponse)
}

func (m *mangling) Encrypt(ctx context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	resp, err := m.KeyManagementServiceServer.Encrypt(ctx, req)
	if err != nil || m.encrypt == nil {
		return resp, err
	}
	m.encrypt(resp)
	return resp, nil
}

func (m *mangling) Decrypt(ctx context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	resp, err := m.KeyManagementServiceServer.Decrypt(ctx, req)
	if err != nil || m.decrypt == nil {
		return resp, err
	}
	m.decrypt(resp)
	return resp, nil
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
		{"a version that is not a number", "canonical", []byte("gcpkms:vX:payload")},
		// Canonical digits and still not a version: the overflow branch, which
		// the canonical-form check deliberately does not subsume.
		{"a version too large for an int", "not a number", []byte("gcpkms:v99999999999999999999:payload")},
		// A v1 row truncated inside its own marker: the prefix matched and there
		// is no terminator, the one unmark branch a partial write would reach.
		{"prefix with no terminator", "no version", []byte("gcpkms:v")},
		// Non-canonical digits this package could never have written. The marker
		// exists to say unambiguously which format wrote the bytes, so a
		// spelling it does not emit is not silently read as v1.
		{"a non-canonical version", "canonical", []byte("gcpkms:v01:payload")},
		{"a signed version", "canonical", []byte("gcpkms:v+1:payload")},
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
	// Only the acceptance half reaches the service: the refusal is the cipher's
	// own guard, which fires before any network call. So this proves real KMS
	// accepts the ceiling we resolved for the configured key — it cannot detect
	// the service raising its limit above ours.
	if _, _, err := c.Encrypt(ctx, bytes.Repeat([]byte("a"), c.MaxPlaintext())); err != nil {
		t.Errorf("real Cloud KMS refused %d bytes, which this key's ceiling says it accepts: %v",
			c.MaxPlaintext(), err)
	}
	if _, _, err := c.Encrypt(ctx, bytes.Repeat([]byte("a"), c.MaxPlaintext()+1)); !errors.Is(err, secrets.ErrPlaintextTooLarge) {
		t.Errorf("one byte over = %v, want ErrPlaintextTooLarge", err)
	}
}
