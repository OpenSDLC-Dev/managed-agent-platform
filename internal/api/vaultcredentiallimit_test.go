package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/gcpkms"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/gcpkms/gcpkmstest"
)

// A cipher that bounds plaintext size refuses credential material the API
// itself accepts — maxBodyBytes admits 4 MiB and no secret field is length-
// bounded — so a gcpkms deployment answers a request an openbao one would
// have stored. That is a real difference between two ciphers behind one
// interface, and plan 20's Decision 3 is explicit that it must reach the
// caller as the caller's error: "a test that accepted a generic 500 would
// bless exactly the behaviour this decision is trying to avoid."
//
// So this drives the real HTTP surface with a real gcpkms cipher rather than
// unit-testing the cipher in isolation, and it asserts the SAME request twice
// — stored under the local cipher, refused under gcpkms — because a refusal
// nobody can contrast with an acceptance proves only that something failed.
func TestOversizeCredentialSecretIsRefusedWithAFourHundred(t *testing.T) {
	// One byte past what Cloud KMS will seal. The sealed plaintext is the JSON
	// object built from the secret fields, so it is larger still — this is
	// comfortably over, and TestTheCeilingIsExactAndClassified pins the exact
	// boundary where it belongs, on the cipher.
	huge := strings.Repeat("k", gcpkms.MaxPlaintextBytes+1)
	body := func() map[string]any {
		return map[string]any{"auth": map[string]any{
			"type":         "environment_variable",
			"secret_name":  "BIG_TOKEN",
			"secret_value": huge,
			"networking":   map[string]any{"type": "unrestricted"},
		}}
	}

	t.Run("the local cipher stores it", func(t *testing.T) {
		s := newTestServer(t)
		vaultID := createVault(t, s, "under the local cipher")
		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", body())
		if status != http.StatusOK {
			t.Fatalf("POST credentials = %d, want 200; body %v", status, resp)
		}
	})

	t.Run("gcpkms refuses it, naming the limit", func(t *testing.T) {
		cipher, err := gcpkms.New(context.Background(), gcpkms.Config{
			KeyName: gcpkmstest.KeyName,
			Client:  gcpkmstest.NewClient(t),
		})
		if err != nil {
			t.Fatalf("gcpkms.New: %v", err)
		}
		s := newTestServerWithCipher(t, cipher)
		vaultID := createVault(t, s, "under gcpkms")

		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", body())
		if status != http.StatusBadRequest {
			t.Fatalf("POST credentials = %d, want 400 (a 500 here is the defect); body %v", status, resp)
		}
		errObj, _ := resp["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("error type = %v, want invalid_request_error", errObj["type"])
		}
		// The point of the classification is that the caller learns what to
		// shorten and by how much. A 400 saying "something went wrong" would
		// pass a status assertion and still be the defect.
		message, _ := errObj["message"].(string)
		for _, want := range []string{"65536", "Cloud KMS"} {
			if !strings.Contains(message, want) {
				t.Errorf("message %q does not mention %q", message, want)
			}
		}

		// And nothing was written: the seal happens before the transaction
		// opens, so a refusal must leave no row behind.
		var credentials int
		if err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM vault_credentials WHERE vault_id = $1`, vaultID).Scan(&credentials); err != nil {
			t.Fatalf("count credentials: %v", err)
		}
		if credentials != 0 {
			t.Errorf("a refused create left %d credential row(s) behind", credentials)
		}
	})

	// The update path re-seals too, and docs/DIVERGENCES.md claims BOTH answer
	// 400 — so both are driven. A create-only test would let the update path
	// regress to a 500 while the registry kept saying otherwise.
	t.Run("gcpkms refuses an oversize update too", func(t *testing.T) {
		cipher, err := gcpkms.New(context.Background(), gcpkms.Config{
			KeyName: gcpkmstest.KeyName,
			Client:  gcpkmstest.NewClient(t),
		})
		if err != nil {
			t.Fatalf("gcpkms.New: %v", err)
		}
		s := newTestServerWithCipher(t, cipher)
		vaultID := createVault(t, s, "update under gcpkms")

		// A credential small enough to store, then grown past the ceiling.
		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials", map[string]any{
			"auth": map[string]any{
				"type": "environment_variable", "secret_name": "TOKEN",
				"secret_value": "small", "networking": map[string]any{"type": "unrestricted"},
			},
		})
		if status != http.StatusOK {
			t.Fatalf("seed create = %d; body %v", status, resp)
		}
		credID, _ := resp["id"].(string)

		status, resp = s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+credID, map[string]any{
			"auth": map[string]any{"type": "environment_variable", "secret_value": huge},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("oversize update = %d, want 400 (a 500 here is the defect); body %v", status, resp)
		}
		errObj, _ := resp["error"].(map[string]any)
		message, _ := errObj["message"].(string)
		if !strings.Contains(message, "65536") {
			t.Errorf("message %q does not name the limit", message)
		}
	})
}
