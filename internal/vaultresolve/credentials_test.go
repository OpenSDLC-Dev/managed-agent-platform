package vaultresolve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testCipher is a deterministic local AES-GCM cipher for sealing fixtures.
func testCipher(t *testing.T) *local.Cipher {
	t.Helper()
	c, err := local.New(local.Config{KeyID: "test-key", Key: bytes.Repeat([]byte("k"), 32)})
	if err != nil {
		t.Fatalf("local cipher: %v", err)
	}
	return c
}

// newSealedEnvCred inserts an ACTIVE environment_variable credential whose
// secret_value is sealed through the cipher — the realistic fixture the decrypt
// path must round-trip. networking is a raw JSON fragment
// (`{"type":"unrestricted"}` or `{"type":"limited","allowed_hosts":[…]}`).
func newSealedEnvCred(t *testing.T, pool *pgxpool.Pool, cipher secrets.Cipher,
	vaultID, secretName, secretValue, networking string, header, body bool) string {
	t.Helper()
	sealed, err := json.Marshal(map[string]string{"secret_value": secretValue})
	if err != nil {
		t.Fatalf("marshal sealed: %v", err)
	}
	ct, keyID, err := cipher.Encrypt(context.Background(), sealed)
	if err != nil {
		t.Fatalf("seal secret: %v", err)
	}
	return insertEnvCred(t, pool, vaultID, secretName, networking, header, body, ct, keyID)
}

// insertEnvCred inserts an active environment_variable credential with the given
// sealed material verbatim, so a test can hand it a tampered or foreign
// ciphertext. cred_key is namespaced like the API writes it.
func insertEnvCred(t *testing.T, pool *pgxpool.Pool, vaultID, secretName, networking string,
	header, body bool, ciphertext []byte, keyID string) string {
	t.Helper()
	id := domain.NewID("vcrd").String()
	auth := fmt.Sprintf(
		`{"type":"environment_variable","secret_name":%q,"networking":%s,`+
			`"injection_location":{"body":%t,"header":%t}}`,
		secretName, networking, body, header)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
		 VALUES ($1, $2, 'environment_variable', $3::jsonb, $4, $5, $6)`,
		id, vaultID, auth, ciphertext, keyID, "name:"+secretName); err != nil {
		t.Fatalf("insert env cred: %v", err)
	}
	return id
}

func credByName(creds []vaultresolve.Credential) map[string]vaultresolve.Credential {
	m := make(map[string]vaultresolve.Credential, len(creds))
	for _, c := range creds {
		m[c.SecretName] = c
	}
	return m
}

func TestCredentialsResolvesAndDecrypts(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)
	sess := domain.NewID("sesn").String()

	v1 := newVault(t, pool, false)
	v2 := newVault(t, pool, false)
	v3 := newVault(t, pool, true) // archived vault

	apiV1 := newSealedEnvCred(t, pool, cipher, v1, "API_KEY", "secret-api-v1",
		`{"type":"limited","allowed_hosts":["api.example.com","*.svc.example.com"]}`, true, false)
	newSealedEnvCred(t, pool, cipher, v1, "DB_URL", "secret-db-url", `{"type":"unrestricted"}`, true, true)
	newSealedEnvCred(t, pool, cipher, v2, "API_KEY", "secret-api-v2", `{"type":"unrestricted"}`, true, true) // loser
	newSealedEnvCred(t, pool, cipher, v2, "V2_ONLY", "secret-v2only", `{"type":"unrestricted"}`, true, true)

	// Rows that must contribute nothing:
	newEnvCred(t, pool, v1, "OLD_KEY", true)                                                              // archived credential
	newEnvCred(t, pool, v1, "PURGED", false)                                                              // active but NULL ciphertext (purge anomaly): skipped, fail-closed
	newMCPCred(t, pool, v1)                                                                               // non-env-var
	newSealedEnvCred(t, pool, cipher, v3, "STALE", "secret-stale", `{"type":"unrestricted"}`, true, true) // in an archived vault

	// v3 is passed but archived, so STALE's exclusion exercises the v.archived_at
	// guard (not merely ANY($1) skipping an unlisted vault).
	got, err := vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v1, v2, v3})
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, len(got))
	for i, c := range got {
		names[i] = c.SecretName
	}
	sort.Strings(names)
	if want := []string{"API_KEY", "DB_URL", "V2_ONLY"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("secret_names = %v, want %v (archived/purged/non-env/archived-vault must be absent)", names, want)
	}

	by := credByName(got)

	api := by["API_KEY"]
	if api.Secret != "secret-api-v1" {
		t.Errorf("API_KEY secret = %q, want v1's value (first vault wins)", api.Secret)
	}
	if api.CredentialID != apiV1 {
		t.Errorf("API_KEY credential_id = %q, want %q", api.CredentialID, apiV1)
	}
	if api.Unrestricted {
		t.Error("API_KEY is limited, want Unrestricted=false")
	}
	if strings.Join(api.AllowedHosts, ",") != "api.example.com,*.svc.example.com" {
		t.Errorf("API_KEY allowed_hosts = %v", api.AllowedHosts)
	}
	if !api.Header || api.Body {
		t.Errorf("API_KEY injection = header:%t body:%t, want header-only", api.Header, api.Body)
	}
	if api.Placeholder != egress.Placeholder(sess, "API_KEY") {
		t.Errorf("API_KEY placeholder = %q, want the deterministic egress token", api.Placeholder)
	}

	db := by["DB_URL"]
	if !db.Unrestricted {
		t.Error("DB_URL is unrestricted, want Unrestricted=true")
	}
	if len(db.AllowedHosts) != 0 {
		t.Errorf("DB_URL allowed_hosts = %v, want empty for unrestricted", db.AllowedHosts)
	}
	if !db.Header || !db.Body {
		t.Errorf("DB_URL injection = header:%t body:%t, want both", db.Header, db.Body)
	}
}

func TestCredentialsBindingsParity(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)
	sess := domain.NewID("sesn").String()

	v1 := newVault(t, pool, false)
	v2 := newVault(t, pool, false)
	newSealedEnvCred(t, pool, cipher, v1, "API_KEY", "secret-api-v1",
		`{"type":"limited","allowed_hosts":["api.example.com"]}`, true, false)
	newSealedEnvCred(t, pool, cipher, v1, "DB_URL", "secret-db", `{"type":"unrestricted"}`, true, true)
	newSealedEnvCred(t, pool, cipher, v2, "API_KEY", "secret-api-v2", `{"type":"unrestricted"}`, true, true)
	newSealedEnvCred(t, pool, cipher, v2, "V2_ONLY", "secret-v2", `{"type":"unrestricted"}`, true, true)

	// The load-bearing invariant: the sandbox env-var half (Bindings) and the
	// gate substitution half (Credentials) resolve the SAME winner per
	// secret_name, in the same order, whatever the vault order — so a collided
	// secret_name's placeholder always stands for the one value the sandbox saw.
	apiSecretOf := map[string]string{v1: "secret-api-v1", v2: "secret-api-v2"}
	for _, order := range [][]string{{v1, v2}, {v2, v1}} {
		bindings, err := vaultresolve.Bindings(ctx, pool, sess, order)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := vaultresolve.Credentials(ctx, pool, cipher, sess, order)
		if err != nil {
			t.Fatal(err)
		}
		if len(bindings) != len(creds) {
			t.Fatalf("order %v: %d bindings vs %d creds", order, len(bindings), len(creds))
		}
		for i := range bindings {
			if bindings[i].SecretName != creds[i].SecretName {
				t.Errorf("order %v idx %d: secret_name %q vs %q", order, i, bindings[i].SecretName, creds[i].SecretName)
			}
			if bindings[i].Placeholder != creds[i].Placeholder {
				t.Errorf("order %v idx %d: placeholder drift %q vs %q", order, i, bindings[i].Placeholder, creds[i].Placeholder)
			}
		}
		// Names+placeholders match for either winning vault (they derive from
		// secret_name), so pin the identity of the winner too: Credentials must
		// decrypt the FIRST vault's colliding API_KEY, not the loser's.
		if got := credByName(creds)["API_KEY"].Secret; got != apiSecretOf[order[0]] {
			t.Errorf("order %v: API_KEY resolved to %q, want the first vault's value %q", order, got, apiSecretOf[order[0]])
		}
	}
}

func TestCredentialsDecryptFailureFailsClosed(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)
	sess := domain.NewID("sesn").String()

	const canary = "top-secret-leak-canary"

	t.Run("tampered ciphertext fails the whole call", func(t *testing.T) {
		v := newVault(t, pool, false)
		sealed, _ := json.Marshal(map[string]string{"secret_value": canary})
		ct, keyID, err := cipher.Encrypt(ctx, sealed)
		if err != nil {
			t.Fatal(err)
		}
		ct[len(ct)-1] ^= 0xff // flip a byte: GCM auth now fails
		id := insertEnvCred(t, pool, v, "TAMPER", `{"type":"unrestricted"}`, true, true, ct, keyID)

		_, err = vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v})
		if err == nil {
			t.Fatal("expected an error on tampered ciphertext, got nil")
		}
		// Exact match, not Contains: the decrypt error must be the id-only message
		// with NOTHING appended, so a regression re-wrapping the cipher error (%w)
		// fails here.
		if got := err.Error(); got != "vaultresolve: cannot decrypt credential "+id {
			t.Errorf("decrypt error = %q, want the id-only message", got)
		}
	})

	t.Run("sealed doc without secret_value errors", func(t *testing.T) {
		v := newVault(t, pool, false)
		sealed, _ := json.Marshal(map[string]string{}) // no secret_value
		ct, keyID, err := cipher.Encrypt(ctx, sealed)
		if err != nil {
			t.Fatal(err)
		}
		id := insertEnvCred(t, pool, v, "EMPTY", `{"type":"unrestricted"}`, true, true, ct, keyID)

		_, err = vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v})
		if err == nil {
			t.Fatal("expected an error for a sealed doc missing secret_value, got nil")
		}
		if !strings.Contains(err.Error(), id) {
			t.Errorf("error must name the credential id %q: %v", id, err)
		}
	})

	t.Run("decrypted payload that is not a JSON object errors", func(t *testing.T) {
		v := newVault(t, pool, false)
		ct, keyID, err := cipher.Encrypt(ctx, []byte("not-json-at-all")) // decrypts, but is not the sealed object
		if err != nil {
			t.Fatal(err)
		}
		id := insertEnvCred(t, pool, v, "NOTJSON", `{"type":"unrestricted"}`, true, true, ct, keyID)

		_, err = vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v})
		if err == nil {
			t.Fatal("expected an error for a non-JSON sealed payload, got nil")
		}
		// Exact match: this is the one path where the decrypted plaintext reaches a
		// json error. A regression wrapping it with %w would append the offending
		// byte of plaintext (json.SyntaxError) and fail this assertion.
		if got := err.Error(); got != "vaultresolve: malformed sealed secret for credential "+id {
			t.Errorf("sealed-parse error = %q, want the id-only message", got)
		}
	})

	t.Run("ciphertext present but NULL key id fails closed", func(t *testing.T) {
		// A half-purged row (ciphertext set, key id NULL) is not reachable through
		// the write path; if it occurs it must fail loudly, not resolve.
		v := newVault(t, pool, false)
		sealed, _ := json.Marshal(map[string]string{"secret_value": canary})
		ct, _, err := cipher.Encrypt(ctx, sealed)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.NewID("vcrd").String()
		if _, err := pool.Exec(ctx,
			`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key)
			 VALUES ($1, $2, 'environment_variable',
			         '{"type":"environment_variable","secret_name":"NOKEY","networking":{"type":"unrestricted"},"injection_location":{"body":true,"header":true}}'::jsonb,
			         $3, NULL, 'name:NOKEY')`,
			id, v, ct); err != nil {
			t.Fatalf("insert null-key cred: %v", err)
		}

		_, err = vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v})
		if err == nil {
			t.Fatal("expected an error for a ciphertext with a NULL key id, got nil")
		}
	})

	t.Run("a corrupt secret_name surfaces as an error", func(t *testing.T) {
		// winnersFor's parse failure propagates through Credentials, not silently
		// dropped — the gate half mirrors Bindings' corrupt-auth behavior.
		v := newVault(t, pool, false)
		id := domain.NewID("vcrd").String()
		if _, err := pool.Exec(ctx,
			`INSERT INTO vault_credentials (id, vault_id, auth_type, auth, cred_key)
			 VALUES ($1, $2, 'environment_variable', '{"secret_name":123}'::jsonb, 'name:corrupt')`,
			id, v); err != nil {
			t.Fatalf("insert corrupt cred: %v", err)
		}
		if _, err := vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v}); err == nil {
			t.Fatal("expected an error resolving a corrupt secret_name, got nil")
		}
	})
}

// leakyCipher is a deliberately hostile secrets.Cipher whose Decrypt error
// embeds secret material — the failure mode Credentials must not propagate.
type leakyCipher struct{ marker string }

func (l leakyCipher) Encrypt(context.Context, []byte) ([]byte, string, error) {
	return []byte("ct"), "leaky-key", nil
}

func (l leakyCipher) Decrypt(context.Context, []byte, string) ([]byte, error) {
	return nil, fmt.Errorf("cipher backend failure, recovered plaintext=%s", l.marker)
}

func TestCredentialsErrorNeverEchoesCipherError(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess := domain.NewID("sesn").String()
	const marker = "SECRET-INSIDE-CIPHER-ERROR"

	// Any non-NULL ciphertext reaches Decrypt; the stub ignores it and returns a
	// secret-bearing error. Credentials must surface neither the cipher's message
	// nor the marker — leak-safety is a property of Credentials, not of the cipher
	// (this fails if the decrypt error is wrapped with %w).
	v := newVault(t, pool, false)
	id := insertEnvCred(t, pool, v, "API_KEY", `{"type":"unrestricted"}`, true, true, []byte("dummy-ct"), "leaky-key")

	_, err := vaultresolve.Credentials(ctx, pool, leakyCipher{marker: marker}, sess, []string{v})
	if err == nil {
		t.Fatal("expected an error from a failing cipher, got nil")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("resolution error echoed the cipher error's contents (leak): %v", err)
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("resolution error must still name the credential id %q: %v", id, err)
	}
}

func TestCredentialsCorruptNetworkingErrors(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	cipher := testCipher(t)
	sess := domain.NewID("sesn").String()

	// An auth document whose networking is a string, not an object (only
	// reachable by out-of-band corruption — the API always writes an object),
	// surfaces as a resolution error rather than a silently dropped credential.
	// The secret is sealed so the row is not skipped before the parse.
	v := newVault(t, pool, false)
	id := newSealedEnvCred(t, pool, cipher, v, "CORRUPT", "secret", `"not-an-object"`, true, true)

	_, err := vaultresolve.Credentials(ctx, pool, cipher, sess, []string{v})
	if err == nil {
		t.Fatal("expected an error for a corrupt networking document, got nil")
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("error must name the credential id %q: %v", id, err)
	}
}

// countingQuerier counts Query calls so the empty-input short-circuit can be
// asserted without a database.
type countingQuerier struct{ calls int }

func (c *countingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	c.calls++
	return nil, fmt.Errorf("countingQuerier.Query should not have been called")
}

func TestCredentialsNilCipher(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	sess := domain.NewID("sesn").String()

	// Winners exist + nil cipher → error: a vault-attached session on a
	// cipher-less deployment is a misconfiguration, not a silent no-op.
	v := newVault(t, pool, false)
	newSealedEnvCred(t, pool, testCipher(t), v, "API_KEY", "secret", `{"type":"unrestricted"}`, true, true)
	if _, err := vaultresolve.Credentials(ctx, pool, nil, sess, []string{v}); err == nil {
		t.Fatal("expected an error resolving with a nil cipher and live winners, got nil")
	}

	// Empty vaultIDs + nil cipher → (nil, nil) with no query issued at all.
	cq := &countingQuerier{}
	got, err := vaultresolve.Credentials(ctx, cq, nil, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil for no attached vaults", got)
	}
	if cq.calls != 0 {
		t.Fatalf("querier called %d times for empty vaultIDs, want 0", cq.calls)
	}
}
