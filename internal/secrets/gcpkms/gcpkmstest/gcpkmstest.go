// Package gcpkmstest is test support for the gcpkms cipher: an in-process
// fake Cloud KMS gRPC server, so the shared secrets contract runs hermetically
// and `make test` never touches GCP or spends money, plus the opt-in gate for
// the live tier that calls the real service.
//
// The fake is a fake, not a stub: it seals with real AES-256-GCM and enforces
// the ceiling the real API enforces, because the contract suite asserts
// properties a stub cannot honour — fresh encryptions must differ, tampering
// and truncation must be detected, and a foreign key must be refused. A fake
// that returned its input would pass none of them.
//
// The .env handling mirrors internal/webtool/webtooltest rather than importing
// it, for the reason that package gives for not importing internal/modeltest:
// each tier's contract is scoped to its own keys, and widening one for
// another's would trade two small parsers for one leaky contract. Same rules,
// restated: consent to spend money is the RUN_LIVE_KMS_TESTS environment
// variable, never the file; the environment always wins over the file, an
// empty environment value included; only the one KMS key is ever read from the
// file; not opted in, the file is never opened. Opted in but unconfigured
// FAILS rather than skips. Production code must never import this package.
package gcpkmstest

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// KeyName is the CryptoKey resource name the fake serves. It is a
// well-formed name so the cipher's own validation is exercised rather than
// bypassed.
const KeyName = "projects/map-test/locations/global/keyRings/test-ring/cryptoKeys/test-key"

// The real service's two raw-Encrypt ceilings, enforced here so a test that
// removed the cipher's own guard would still see the refusal — from the far
// side, as an InvalidArgument, which is exactly the unhelpful error the guard
// exists to replace. Which one binds depends on the key's protection level,
// and the fake models that too: an HSM key is where a single hard-coded 64 KiB
// ceiling turns the cipher's 4xx back into a 500, so the fake has to be able to
// be one.
const (
	maxPlaintextBytes    = 65536
	maxPlaintextBytesHSM = 8192
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// NewClient starts a fake KMS server serving KeyName as a SOFTWARE key and
// returns a client wired to it. Both are torn down when the test ends.
func NewClient(t *testing.T) *kms.KeyManagementClient {
	t.Helper()
	return NewClientFor(t, NewServer(t, kmspb.ProtectionLevel_SOFTWARE))
}

// NewHSMClient is NewClient against an HSM-protected key, whose plaintext
// ceiling is 8 KiB rather than 64 KiB. It exists because that difference is
// invisible in a resource name: a cipher that assumed the software ceiling
// would pass every test against NewClient and still hand callers an
// uninformative 500 on the first credential larger than 8 KiB.
func NewHSMClient(t *testing.T) *kms.KeyManagementClient {
	t.Helper()
	return NewClientFor(t, NewServer(t, kmspb.ProtectionLevel_HSM))
}

// NewServer builds the fake serving KeyName at the given protection level. It
// is exported so a test can wrap it — the response-integrity guards can only be
// reached from a server that answers wrongly on purpose, and without a seam for
// that, deleting any of them leaves the suite green.
func NewServer(t *testing.T, level kmspb.ProtectionLevel) kmspb.KeyManagementServiceServer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate fake KMS key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return &fake{aead: aead, level: level}
}

// NewClientFor serves srv over an in-process gRPC connection and returns a KMS
// client wired to it. Both are torn down when the test ends.
func NewClientFor(t *testing.T, srv kmspb.KeyManagementServiceServer) *kms.KeyManagementClient {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	kmspb.RegisterKeyManagementServiceServer(server, srv)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial fake KMS: %v", err)
	}
	client, err := kms.NewKeyManagementClient(context.Background(), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatalf("kms.NewKeyManagementClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// fake implements the two KeyManagementService methods this backend calls.
// Every other method stays Unimplemented, so a backend that started calling
// one would fail loudly rather than silently pass.
type fake struct {
	kmspb.UnimplementedKeyManagementServiceServer
	aead  cipher.AEAD
	level kmspb.ProtectionLevel
}

func (f *fake) Encrypt(_ context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	if req.GetName() != KeyName {
		return nil, status.Errorf(codes.NotFound, "key %q not found", req.GetName())
	}
	max := maxPlaintextBytes
	if f.level == kmspb.ProtectionLevel_HSM {
		max = maxPlaintextBytesHSM
	}
	if len(req.GetPlaintext()) > max {
		return nil, status.Errorf(codes.InvalidArgument,
			"plaintext too large: max_expected_size:%d", max)
	}
	// The real service VERIFIES the checksum rather than noting its presence,
	// and rejects a mismatch. Modelled, because a client that computed the CRC
	// wrongly would otherwise be green here and fail on every real call.
	if req.GetPlaintextCrc32C() != nil &&
		req.GetPlaintextCrc32C().GetValue() != int64(crc32.Checksum(req.GetPlaintext(), castagnoli)) {
		return nil, status.Error(codes.InvalidArgument, "plaintext checksum mismatch")
	}
	nonce := make([]byte, f.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, status.Errorf(codes.Internal, "nonce: %v", err)
	}
	ct := f.aead.Seal(nonce, nonce, req.GetPlaintext(), nil)
	return &kmspb.EncryptResponse{
		// The real service answers with the CryptoKeyVersion it used, not the
		// CryptoKey it was asked for. Mirrored so a backend that compared the
		// two would fail here rather than in production.
		Name:                    KeyName + "/cryptoKeyVersions/1",
		Ciphertext:              ct,
		CiphertextCrc32C:        wrapperspb.Int64(int64(crc32.Checksum(ct, castagnoli))),
		VerifiedPlaintextCrc32C: req.GetPlaintextCrc32C() != nil,
		ProtectionLevel:         f.level,
	}, nil
}

func (f *fake) Decrypt(_ context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if req.GetName() != KeyName {
		return nil, status.Errorf(codes.NotFound, "key %q not found", req.GetName())
	}
	ct := req.GetCiphertext()
	if req.GetCiphertextCrc32C() != nil &&
		req.GetCiphertextCrc32C().GetValue() != int64(crc32.Checksum(ct, castagnoli)) {
		return nil, status.Error(codes.InvalidArgument, "ciphertext checksum mismatch")
	}
	if len(ct) < f.aead.NonceSize() {
		return nil, status.Error(codes.InvalidArgument, "ciphertext is too short")
	}
	pt, err := f.aead.Open(nil, ct[:f.aead.NonceSize()], ct[f.aead.NonceSize():], nil)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "decryption failed: the ciphertext is invalid")
	}
	return &kmspb.DecryptResponse{
		Plaintext:       pt,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32.Checksum(pt, castagnoli))),
		UsedPrimary:     true,
		ProtectionLevel: f.level,
	}, nil
}

// LiveEnv is the consent variable for the tier that calls real Cloud KMS. It
// is read from the environment only — never from .env, so consent can never
// come from disk.
const LiveEnv = "RUN_LIVE_KMS_TESTS"

// KeyNameEnv names the CryptoKey the live tier uses, read from the environment
// or the repo-root .env. Exactly this one key ever reaches the file. It is not
// a credential — authentication is Application Default Credentials — and it
// rides .env only because that is where this repo keeps live-tier
// configuration.
const KeyNameEnv = "GCPKMS_KEY_NAME"

// LiveKeyName gates a live test and returns the CryptoKey resource name. Not
// opted in: the test skips and the file is never opened. Opted in with the key
// name missing: the test FAILS — a tier that skips itself when its
// configuration rots is not a safety net.
func LiveKeyName(t *testing.T) string {
	t.Helper()
	name, skip, err := liveKeyName(resolve)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// liveKeyName is the rule LiveKeyName applies, split out for its own tests.
func liveKeyName(getenv func(string) string) (name, skip string, err error) {
	if getenv(LiveEnv) == "" {
		return "", fmt.Sprintf("%s is not set: skipping the live KMS tier (Cloud KMS is not called)", LiveEnv), nil
	}
	name = getenv(KeyNameEnv)
	if name == "" {
		return "", "", fmt.Errorf("%s opted into the live KMS tier but %s is unset: "+
			"set it in the environment or the repo-root .env", LiveEnv, KeyNameEnv)
	}
	return name, "", nil
}

// resolve reads one key for the production gate.
func resolve(key string) string { return lookup(os.LookupEnv, dotEnv, key) }

// lookup resolves a key from the environment, falling back to the repo-root
// .env for the KMS key name only. The environment always wins — including when
// it sets the key to the empty string, which is an answer and not an
// invitation for the file to supply one.
func lookup(lookupEnv func(string) (string, bool), file func() map[string]string, key string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	if key != KeyNameEnv {
		return ""
	}
	return file()[key]
}

// dotEnv parses the repo-root .env once, on first use. The values stay here
// rather than being pushed into the process environment: an os.Setenv would
// outlive the test that triggered it. A missing file is not an error — the
// environment may carry everything.
var dotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(repoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDotEnv(f)
})

// repoRoot derives the checkout root from this file's compile-time path, so a
// worktree reaches its own .env rather than the main checkout's.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..")
}

func parseDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.TrimSpace(key) != KeyNameEnv {
			continue
		}
		out[KeyNameEnv] = parseValue(value)
	}
	return out
}

// parseValue takes the value side of one .env line, exactly as modeltest and
// webtooltest read the same file: a quoted value is whatever the quotes
// enclose, so a '#' inside them is content; an unquoted one runs to a '#' that
// follows whitespace and keeps one that does not.
func parseValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if i := commentStart(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func commentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
