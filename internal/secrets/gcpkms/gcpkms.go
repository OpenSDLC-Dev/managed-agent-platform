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

// The two raw-Encrypt ceilings Cloud KMS applies, and the reason the limit is
// not a constant: which one binds is a property of the KEY, not of this code.
//
// Getting that wrong is not cosmetic. A plaintext between the two bounds on an
// HSM key would pass a 64 KiB guard, be refused by the service as a bare
// InvalidArgument, and reach the caller as exactly the generic 500 this
// backend's size classification exists to prevent — so the ceiling is read from
// the key at startup (see New) rather than assumed.
const (
	// MaxPlaintextBytes is the ceiling for a SOFTWARE, EXTERNAL or EXTERNAL_VPC
	// key version. Measured against the real service (plan 20's ground truth,
	// 2026-08-01): 65536 accepted, 65537 refused with `max_expected_size:65536`.
	MaxPlaintextBytes = 65536

	// MaxPlaintextBytesHSM is the ceiling for an HSM key version, where the
	// service bounds plaintext *plus* additional authenticated data at 8 KiB.
	// This backend sends no AAD, so the whole budget is plaintext.
	MaxPlaintextBytesHSM = 8192
)

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

	// maxPlaintext is the ceiling this key actually enforces, resolved from the
	// protection level the startup probe reported. A key whose primary version
	// later changes protection level needs a restart to be re-read — a bounded
	// staleness, and the alternative is asking the service its level on every
	// seal.
	maxPlaintext int
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
	// The ceiling starts at the smaller of the two, so a probe that somehow
	// fails to establish it can only ever be over-strict. Over-strict costs a
	// caller a 4xx naming a lower number than the service would have enforced;
	// over-permissive costs them the uninformative 500 this whole path exists
	// to remove. Only one of those is recoverable by the person reading it.
	c := &Cipher{keyName: cfg.KeyName, client: client, maxPlaintext: MaxPlaintextBytesHSM}
	_, level, err := c.seal(ctx, []byte("map-gcpkms-startup-probe"))
	if err != nil {
		return nil, fmt.Errorf("gcpkms cipher: probe encrypt with %s: %w", c.keyName, err)
	}
	// The probe answers with the CryptoKeyVersion it used and that version's
	// protection level, so the ceiling comes from the key itself rather than
	// from an assumption about how the operator created it.
	if level != kmspb.ProtectionLevel_HSM {
		c.maxPlaintext = MaxPlaintextBytes
	}
	return c, nil
}

// MaxPlaintext reports the plaintext ceiling this cipher's key enforces —
// MaxPlaintextBytes, or MaxPlaintextBytesHSM for an HSM-protected key.
func (c *Cipher) MaxPlaintext() int { return c.maxPlaintext }

// Encrypt seals plaintext under the configured CryptoKey. The keyID is that
// key's resource name.
func (c *Cipher) Encrypt(ctx context.Context, plaintext []byte) ([]byte, string, error) {
	if len(plaintext) == 0 {
		return nil, "", errors.New("gcpkms cipher: empty plaintext")
	}
	if len(plaintext) > c.maxPlaintext {
		// The sentinel, not the KMS client's InvalidArgument: the API turns
		// this into a 4xx naming the limit, and an unclassified failure would
		// leave the useful message in the server's log and nowhere else.
		//
		// The usual "gcpkms cipher:" prefix is deliberately absent — this is
		// the one error here that reaches an API caller verbatim, and the
		// sentinel already carries a package prefix of its own.
		return nil, "", fmt.Errorf("%w: %d bytes, and Cloud KMS's raw Encrypt accepts at most %d for this key",
			secrets.ErrPlaintextTooLarge, len(plaintext), c.maxPlaintext)
	}
	sealed, _, err := c.seal(ctx, plaintext)
	if err != nil {
		return nil, "", fmt.Errorf("gcpkms cipher: encrypt: %w", err)
	}
	return sealed, c.keyName, nil
}

// seal is Encrypt's body without the size guard, so New's startup probe
// exercises the same call path it will use in production. It also returns the
// protection level of the key version the service used, which is what New reads
// the plaintext ceiling from.
func (c *Cipher) seal(ctx context.Context, plaintext []byte) ([]byte, kmspb.ProtectionLevel, error) {
	resp, err := c.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:            c.keyName,
		Plaintext:       plaintext,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(plaintext, castagnoli))),
	})
	if err != nil {
		return nil, 0, err
	}
	// KMS's integrity contract, both directions. A false VerifiedPlaintextCrc32C
	// means the service did not receive the checksum we sent — so it cannot say
	// the plaintext arrived intact — and a ciphertext checksum mismatch means
	// the response did not survive the trip. Either way, storing the result
	// would turn a transport fault into a credential that never decrypts, which
	// is the one failure the vault feature cannot recover from.
	if !resp.GetVerifiedPlaintextCrc32C() {
		return nil, 0, errors.New("the service did not verify the request checksum")
	}
	// Emptiness and absence are checked before the comparison, not left to it.
	// GetValue() answers 0 for an absent wrapper and CRC32C("") is also 0, so an
	// empty ciphertext with no checksum would compare EQUAL and be stored — as a
	// marker with nothing after it, a credential that can never be unsealed.
	// That is precisely the failure the paragraph above claims to prevent, so it
	// is refused explicitly rather than by arithmetic coincidence.
	if len(resp.GetCiphertext()) == 0 {
		return nil, 0, errors.New("the service returned no ciphertext")
	}
	if resp.GetCiphertextCrc32C() == nil {
		return nil, 0, errors.New("the response carried no ciphertext checksum")
	}
	if resp.GetCiphertextCrc32C().GetValue() != int64(crc32.Checksum(resp.GetCiphertext(), castagnoli)) {
		return nil, 0, errors.New("response checksum mismatch")
	}
	return append([]byte(fmt.Sprintf("%s%d:", markerPrefix, formatVersion)), resp.GetCiphertext()...),
		resp.GetProtectionLevel(), nil
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
	// Absence checked separately, for seal's reason: an absent wrapper answers 0
	// and would otherwise be compared against a real checksum, passing whenever
	// that checksum happened to be 0.
	if resp.GetPlaintextCrc32C() == nil {
		return nil, errors.New("gcpkms cipher: decrypt: the response carried no plaintext checksum")
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
	// Canonical digits only. strconv.Atoi would read "+1" and "01" as 1, so a
	// marker this package could never have written would decode as v1 — and the
	// marker's whole job is to say unambiguously which format wrote the bytes.
	if !canonicalDigits(digits) {
		return nil, fmt.Errorf("ciphertext's format version %q is not a canonical number", digits)
	}
	// Canonical and still unusable: a run of digits long enough to overflow an
	// int. Atoi is what catches that, which is why the check above does not
	// replace it.
	version, err := strconv.Atoi(string(digits))
	if err != nil {
		return nil, fmt.Errorf("ciphertext's format version %q is not a number this build can read", digits)
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

// canonicalDigits reports whether b is a decimal integer written the one way
// this package writes one: digits only, no sign, and no leading zero.
func canonicalDigits(b []byte) bool {
	if len(b) == 0 || (len(b) > 1 && b[0] == '0') {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
