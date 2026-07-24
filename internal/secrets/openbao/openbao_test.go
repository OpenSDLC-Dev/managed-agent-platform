package openbao_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/openbao"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/secretstest"
)

func TestMain(m *testing.M) { os.Exit(secretstest.Main(m)) }

func newCipher(t *testing.T) *openbao.Cipher {
	t.Helper()
	c, err := openbao.New(context.Background(), openbao.Config{
		Address: secretstest.Addr(t),
		Token:   secretstest.RootToken,
		Key:     secretstest.FreshKey(t),
	})
	if err != nil {
		t.Fatalf("openbao.New: %v", err)
	}
	return c
}

func TestContract(t *testing.T) {
	secretstest.Run(t, func(t *testing.T) secrets.Cipher { return newCipher(t) })
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []openbao.Config{
		{Token: "x", Key: "k"},
		{Address: "http://127.0.0.1:1", Key: "k"},
		{Address: "http://127.0.0.1:1", Token: "x"},
	} {
		if _, err := openbao.New(ctx, cfg); err == nil {
			t.Fatalf("New accepted incomplete config %+v", cfg)
		}
	}
}

func TestNewFailsFastOnUnreachableServer(t *testing.T) {
	_, err := openbao.New(context.Background(), openbao.Config{
		Address: "http://127.0.0.1:1", Token: "x", Key: "k",
	})
	if err == nil {
		t.Fatal("New reached an address nothing listens on")
	}
}

func TestNewFailsFastOnBadToken(t *testing.T) {
	_, err := openbao.New(context.Background(), openbao.Config{
		Address: secretstest.Addr(t), Token: "not-the-token", Key: secretstest.FreshKey(t),
	})
	if err == nil {
		t.Fatal("New accepted a token the server rejects")
	}
	if strings.Contains(err.Error(), "not-the-token") {
		t.Fatalf("error echoes the token: %v", err)
	}
}

// A hostile or misconfigured endpoint (a reverse proxy, a captive portal) may
// reflect the request's token into its error body; the client must scrub it
// before the text can reach a process error path or a log.
func TestServerErrorTextNeverEchoesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"errors":["invalid X-Vault-Token: %s"]}`, r.Header.Get("X-Vault-Token"))
	}))
	defer srv.Close()
	_, err := openbao.New(context.Background(), openbao.Config{
		Address: srv.URL, Token: "sekrit-token-value", Key: "k",
	})
	if err == nil {
		t.Fatal("New succeeded against an all-errors endpoint")
	}
	if strings.Contains(err.Error(), "sekrit-token-value") {
		t.Fatalf("error echoes the reflected token: %v", err)
	}
}

// Transit ciphertext is always "vault:vN:…"; anything else out of the endpoint
// is a broken proxy, and accepting it would persist data no decrypt can open.
func TestEncryptRejectsMalformedCiphertext(t *testing.T) {
	var ciphertext string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/encrypt/") {
			fmt.Fprintf(w, `{"data":{"ciphertext":%q}}`, ciphertext)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c, err := openbao.New(context.Background(), openbao.Config{
		Address: srv.URL, Token: "x", Key: "k",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ciphertext = range []string{"", "queued", "vault:verybad", "vault:v1", "vault:v:x"} {
		if _, _, err := c.Encrypt(context.Background(), []byte("v")); err == nil {
			t.Fatalf("Encrypt accepted malformed ciphertext %q", ciphertext)
		}
	}
	ciphertext = "vault:v1:AAAA"
	if _, _, err := c.Encrypt(context.Background(), []byte("v")); err != nil {
		t.Fatalf("Encrypt rejected well-formed ciphertext: %v", err)
	}
}

func TestNewIsIdempotentOnExistingKey(t *testing.T) {
	ctx := context.Background()
	key := secretstest.FreshKey(t)
	cfg := openbao.Config{Address: secretstest.Addr(t), Token: secretstest.RootToken, Key: key}
	a, err := openbao.New(ctx, cfg)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	ct, keyID, err := a.Encrypt(ctx, []byte("survives re-construction"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := openbao.New(ctx, cfg)
	if err != nil {
		t.Fatalf("second New on the same key: %v", err)
	}
	got, err := b.Decrypt(ctx, ct, keyID)
	if err != nil {
		t.Fatalf("Decrypt after re-construction: %v", err)
	}
	if string(got) != "survives re-construction" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestForeignCiphertextRejected(t *testing.T) {
	ctx := context.Background()
	a, b := newCipher(t), newCipher(t)
	ct, _, err := a.Encrypt(ctx, []byte("sealed under key A"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// b holds a different transit key; presenting A's ciphertext under B's
	// own key name reaches the server, which must refuse to decrypt it.
	if _, err := b.Decrypt(ctx, ct, keyOf(t, b)); err == nil {
		t.Fatal("key B decrypted ciphertext sealed under key A")
	}
}

// keyOf recovers the cipher's key name via a fresh encryption — the keyID it
// reports is the configured transit key.
func keyOf(t *testing.T, c *openbao.Cipher) string {
	t.Helper()
	_, keyID, err := c.Encrypt(context.Background(), []byte("probe"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return keyID
}

// FromEnv's openbao branch lives here rather than in the parent package's
// tests because it needs the live dev container this binary already runs.
func TestFromEnvOpenBao(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SECRETS_BACKEND", "openbao")
	t.Setenv("BAO_ADDR", secretstest.Addr(t))
	t.Setenv("BAO_TOKEN", secretstest.RootToken)
	t.Setenv("BAO_TRANSIT_KEY", secretstest.FreshKey(t))
	c, err := secrets.FromEnv(ctx)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	ct, keyID, err := c.Encrypt(ctx, []byte("wired from env"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := c.Decrypt(ctx, ct, keyID)
	if err != nil || string(got) != "wired from env" {
		t.Fatalf("round-trip: %q, %v", got, err)
	}

	t.Setenv("BAO_TOKEN", "")
	if _, err := secrets.FromEnv(ctx); err == nil {
		t.Fatal("FromEnv accepted openbao backend without BAO_TOKEN")
	}
}
