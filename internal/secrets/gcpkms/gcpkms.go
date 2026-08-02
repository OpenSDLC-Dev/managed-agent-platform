// Package gcpkms is a secrets.Cipher backed by Cloud KMS's raw Encrypt and
// Decrypt: the key material never leaves the service, and the platform stores
// only ciphertext (docs/plan/20_gcp-deployment.md, Decision 3). It is what
// lets a GCP deployment drop the bundled OpenBao — KMS needs a key resource
// name, which is not a secret, plus Workload Identity, so there is no static
// token to distribute or rotate.
//
// Two properties are worth reading before choosing this backend.
//
// # A ceiling
//
// KMS's raw Encrypt bounds plaintext at MaxPlaintextBytes; OpenBao's transit
// engine has no such bound. So this backend cannot serve every input the vault
// credential API accepts, and that is handled as the real difference between
// two ciphers behind one interface that it is: the refusal is
// secrets.ErrPlaintextTooLarge rather than a raw InvalidArgument from the KMS
// client, the API classifies it as the caller's error and answers with a 4xx
// naming the limit, and docs/DIVERGENCES.md records it. Envelope encryption
// removes the ceiling entirely and is deferred to #242.
//
// # A format marker
//
// Ciphertext is "gcpkms:v1:" followed by exactly what KMS returned. The
// version is what makes #242 introducible later without rewriting stored rows:
// an envelope format becomes v2, v1 rows keep decrypting, and a v2 ciphertext
// reaching an older build is refused by name instead of being handed to KMS as
// if it were raw. That marker is the one piece of the escape hatch that is
// cheap now and expensive retrofitted, which is why it ships in the first
// commit rather than with the feature that needs it.
package gcpkms

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strconv"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
)

// MaxPlaintextBytes is Cloud KMS's raw-Encrypt ceiling. Measured against the
// real service (plan 20's ground truth, 2026-08-01): 65536 bytes is accepted,
// 65537 is refused with `max_expected_size:65536`.
const MaxPlaintextBytes = 65536

// markerPrefix and formatVersion compose the ciphertext marker. Split rather
// than written as one string so Decrypt can tell "not our ciphertext at all"
// from "our ciphertext, a format this build predates".
const (
	markerPrefix  = "gcpkms:v"
	formatVersion = 1
)

// keyNameRE matches a CryptoKey resource name and nothing else — notably not a
// CryptoKeyVersion. Encrypt would accept a version name and Decrypt would then
// refuse it, so a deployment configured with one would seal credentials it
// could never unseal.
var keyNameRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)

// castagnoli is the CRC32C polynomial KMS's integrity fields use.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Config names the key. Authentication is Application Default Credentials —
// Workload Identity on GKE — so no credential appears here.
type Config struct {
	// KeyName is the full CryptoKey resource name
	// (projects/P/locations/L/keyRings/R/cryptoKeys/K). It doubles as the
	// keyID stored beside each ciphertext.
	KeyName string

	// Client overrides the ADC-authenticated client New would build. Tests
	// point it at an in-process fake; the caller owns closing it.
	Client *kms.KeyManagementClient
}

// Cipher is the KMS-backed implementation of secrets.Cipher.
type Cipher struct {
	keyName string
	client  *kms.KeyManagementClient
}

// New validates the key name eagerly and proves the key is usable before
// returning, so a typo or a missing IAM binding fails at startup rather than
// on the first credential write (openbao.New's key-ensure, for the same
// reason).
//
// The proof is a throwaway Encrypt, deliberately. GetCryptoKey would be the
// obvious probe and needs cloudkms.cryptoKeys.get, which
// roles/cloudkms.cryptoKeyEncrypterDecrypter — the only role this backend
// otherwise requires — does not carry. Probing with the privilege the runtime
// path already needs keeps the deploy guide's IAM grant at exactly
// encrypt/decrypt (plan 20, Decision 11's least-privilege discipline).
func New(ctx context.Context, cfg Config) (*Cipher, error) {
	if !keyNameRE.MatchString(cfg.KeyName) {
		return nil, fmt.Errorf("gcpkms cipher: KeyName %q is not a CryptoKey resource name "+
			"(projects/P/locations/L/keyRings/R/cryptoKeys/K)", cfg.KeyName)
	}
	client := cfg.Client
	if client == nil {
		var err error
		// No credential option: Application Default Credentials, which on GKE
		// is the Workload Identity binding. A deployment without one fails
		// here, naming what is missing.
		client, err = kms.NewKeyManagementClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("gcpkms cipher: build client (Application Default Credentials): %w", err)
		}
	}
	c := &Cipher{keyName: cfg.KeyName, client: client}
	if _, err := c.seal(ctx, []byte("map-gcpkms-startup-probe")); err != nil {
		return nil, fmt.Errorf("gcpkms cipher: probe encrypt with %s: %w", c.keyName, err)
	}
	return c, nil
}

// Encrypt seals plaintext under the configured CryptoKey. The keyID is that
// key's resource name.
func (c *Cipher) Encrypt(ctx context.Context, plaintext []byte) ([]byte, string, error) {
	if len(plaintext) == 0 {
		return nil, "", errors.New("gcpkms cipher: empty plaintext")
	}
	if len(plaintext) > MaxPlaintextBytes {
		// The sentinel, not the KMS client's InvalidArgument: the API turns
		// this into a 4xx naming the limit, and an unclassified failure would
		// leave the useful message in the server's log and nowhere else.
		//
		// The usual "gcpkms cipher:" prefix is deliberately absent — this is
		// the one error here that reaches an API caller verbatim, and the
		// sentinel already carries a package prefix of its own.
		return nil, "", fmt.Errorf("%w: %d bytes, and Cloud KMS's raw Encrypt accepts at most %d",
			secrets.ErrPlaintextTooLarge, len(plaintext), MaxPlaintextBytes)
	}
	sealed, err := c.seal(ctx, plaintext)
	if err != nil {
		return nil, "", fmt.Errorf("gcpkms cipher: encrypt: %w", err)
	}
	return sealed, c.keyName, nil
}

// seal is Encrypt's body without the size guard, so New's startup probe
// exercises the same call path it will use in production.
func (c *Cipher) seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	resp, err := c.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:            c.keyName,
		Plaintext:       plaintext,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(plaintext, castagnoli))),
	})
	if err != nil {
		return nil, err
	}
	// KMS's integrity contract, both directions. A false VerifiedPlaintextCrc32C
	// means the service did not receive the checksum we sent — so it cannot say
	// the plaintext arrived intact — and a ciphertext checksum mismatch means
	// the response did not survive the trip. Either way, storing the result
	// would turn a transport fault into a credential that never decrypts, which
	// is the one failure the vault feature cannot recover from.
	if !resp.GetVerifiedPlaintextCrc32C() {
		return nil, errors.New("the service did not verify the request checksum")
	}
	if resp.GetCiphertextCrc32C().GetValue() != int64(crc32.Checksum(resp.GetCiphertext(), castagnoli)) {
		return nil, errors.New("response checksum mismatch")
	}
	return append([]byte(fmt.Sprintf("%s%d:", markerPrefix, formatVersion)), resp.GetCiphertext()...), nil
}

// Decrypt reverses Encrypt. A keyID other than the configured key name is
// refused locally — this cipher holds exactly one key.
func (c *Cipher) Decrypt(ctx context.Context, ciphertext []byte, keyID string) ([]byte, error) {
	if keyID != c.keyName {
		return nil, fmt.Errorf("gcpkms cipher: key %q not held (holding %q)", keyID, c.keyName)
	}
	payload, err := unmark(ciphertext)
	if err != nil {
		return nil, fmt.Errorf("gcpkms cipher: %w", err)
	}
	resp, err := c.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:             c.keyName,
		Ciphertext:       payload,
		CiphertextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(payload, castagnoli))),
	})
	if err != nil {
		return nil, fmt.Errorf("gcpkms cipher: decrypt: %w", err)
	}
	plaintext := resp.GetPlaintext()
	if len(plaintext) == 0 {
		return nil, errors.New("gcpkms cipher: decrypt returned no plaintext")
	}
	if resp.GetPlaintextCrc32C().GetValue() != int64(crc32.Checksum(plaintext, castagnoli)) {
		return nil, errors.New("gcpkms cipher: decrypt: response checksum mismatch")
	}
	return plaintext, nil
}

// unmark strips the format marker and reports what a marker this build does
// not understand actually is, rather than handing its payload to KMS.
func unmark(ciphertext []byte) ([]byte, error) {
	rest, ok := bytes.CutPrefix(ciphertext, []byte(markerPrefix))
	if !ok {
		return nil, fmt.Errorf("ciphertext does not carry the %q format marker", markerPrefix)
	}
	digits, payload, ok := bytes.Cut(rest, []byte(":"))
	if !ok || len(digits) == 0 {
		return nil, errors.New("ciphertext's format marker carries no version")
	}
	version, err := strconv.Atoi(string(digits))
	if err != nil {
		return nil, fmt.Errorf("ciphertext's format version %q is not a number", digits)
	}
	if version != formatVersion {
		return nil, fmt.Errorf("ciphertext is format version %d; this build understands version %d "+
			"(a newer version is written by a newer build — upgrade rather than downgrade)", version, formatVersion)
	}
	if len(payload) == 0 {
		return nil, errors.New("ciphertext carries a format marker and nothing else")
	}
	return payload, nil
}
