package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/secrets/local"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/vaultresolve"
)

// TestEnvVarCredentialDestinationBinding verifies plan 70 slice 1 (#1052):
// preserving existing secret material on an environment_variable credential
// update cannot expand where that secret is sent, while narrowing, equivalent
// host sets, metadata-only updates, and pre-existing credentials keep working.
func TestEnvVarCredentialDestinationBinding(t *testing.T) {
	cipher, err := local.New(local.Config{KeyID: "binding-test", Key: bytes.Repeat([]byte("b"), 32)})
	if err != nil {
		t.Fatalf("local cipher: %v", err)
	}
	s := newTestServerWithCipher(t, cipher)
	vaultID := createVault(t, s, "dest-binding")

	newLimited := func(name, secret string, hosts ...string) string {
		t.Helper()
		cred := createCredential(t, s, vaultID, map[string]any{
			"type":         "environment_variable",
			"secret_name":  name,
			"secret_value": secret,
			"networking":   map[string]any{"type": "limited", "allowed_hosts": hosts},
		})
		return cred["id"].(string)
	}

	readRow := func(id string) ([]byte, []byte) {
		t.Helper()
		var authDoc, ct []byte
		if err := s.pool.QueryRow(t.Context(),
			`SELECT auth, secret_ciphertext FROM vault_credentials WHERE id = $1`, id).
			Scan(&authDoc, &ct); err != nil {
			t.Fatalf("read credential %s: %v", id, err)
		}
		return authDoc, ct
	}

	t.Run("expansions without secret_value are refused and leave row unchanged", func(t *testing.T) {
		cases := []struct {
			name    string
			initial []string
			next    map[string]any
		}{
			{
				name:    "limited to unrestricted",
				initial: []string{"api.stripe.com"},
				next:    map[string]any{"type": "unrestricted"},
			},
			{
				name:    "adding an uncovered host",
				initial: []string{"api.stripe.com"},
				next:    map[string]any{"type": "limited", "allowed_hosts": []string{"api.stripe.com", "attacker.example.com"}},
			},
			{
				name:    "replacing with a different host",
				initial: []string{"api.stripe.com"},
				next:    map[string]any{"type": "limited", "allowed_hosts": []string{"attacker.example.com"}},
			},
			{
				name:    "widening exact host to wildcard",
				initial: []string{"api.example.com"},
				next:    map[string]any{"type": "limited", "allowed_hosts": []string{"*.example.com"}},
			},
			{
				name:    "widening narrow wildcard to parent wildcard",
				initial: []string{"*.sub.example.com"},
				next:    map[string]any{"type": "limited", "allowed_hosts": []string{"*.example.com"}},
			},
			{
				name:    "wildcard to apex host",
				initial: []string{"*.example.com"},
				next:    map[string]any{"type": "limited", "allowed_hosts": []string{"example.com"}},
			},
		}

		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				secretName := "EXPAND_" + string(rune('A'+i))
				id := newLimited(secretName, "top-secret-value", tc.initial...)
				beforeAuth, beforeCT := readRow(id)

				status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
					"auth": map[string]any{
						"type":       "environment_variable",
						"networking": tc.next,
					},
				})
				wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error",
					"auth.networking: expanding destinations requires supplying secret_value")

				afterAuth, afterCT := readRow(id)
				if !bytes.Equal(beforeAuth, afterAuth) || !bytes.Equal(beforeCT, afterCT) {
					t.Fatal("refused expansion mutated stored auth or ciphertext")
				}
			})
		}
	})

	t.Run("expansion succeeds when a new secret_value is supplied", func(t *testing.T) {
		id := newLimited("ROTATE_EXPAND", "old-secret", "api.stripe.com")
		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":         "environment_variable",
				"secret_value": "new-secret",
				"networking":   map[string]any{"type": "limited", "allowed_hosts": []string{"api.stripe.com", "hooks.stripe.com"}},
			},
		})
		if status != http.StatusOK {
			t.Fatalf("expand with secret_value: status %d (%v)", status, resp)
		}
		creds, err := vaultresolve.Credentials(t.Context(), s.pool, cipher, "sesn_test", []string{vaultID})
		if err != nil {
			t.Fatalf("resolve after rotate-expand: %v", err)
		}
		found := false
		for _, c := range creds {
			if c.CredentialID == id {
				found = true
				if c.Secret != "new-secret" || len(c.AllowedHosts) != 2 {
					t.Fatalf("resolved credential = %+v, want new-secret with 2 hosts", c)
				}
			}
		}
		if !found {
			t.Fatalf("credential %s missing from resolved set", id)
		}
	})

	t.Run("preserving, narrowing, injection_location, and metadata updates keep secret without secret_value", func(t *testing.T) {
		id := newLimited("NARROW_KEY", "kept-secret", "*.example.com", "extra.other.test")

		// Metadata and display_name only (no auth object at all).
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"display_name": "renamed",
			"metadata":     map[string]string{"owner": "sec"},
		}); status != http.StatusOK {
			t.Fatalf("metadata-only update: status %d (%v)", status, resp)
		}

		// Equivalent hosts with different casing/order + injection_location change.
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":               "environment_variable",
				"networking":         map[string]any{"type": "limited", "allowed_hosts": []string{"EXTRA.OTHER.TEST", "*.EXAMPLE.COM"}},
				"injection_location": map[string]any{"body": false},
			},
		}); status != http.StatusOK {
			t.Fatalf("equivalent networking update: status %d (%v)", status, resp)
		}

		// Narrow from wildcard + extra host to one exact subdomain.
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"api.example.com"}},
			},
		}); status != http.StatusOK {
			t.Fatalf("narrowing update: status %d (%v)", status, resp)
		}

		// Narrowing re-seals the tighter digest: re-expanding back to the old
		// wildcard without secret_value must now be refused.
		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"*.example.com"}},
			},
		})
		wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error",
			"auth.networking: expanding destinations requires supplying secret_value")

		// Narrowed credential still resolves cleanly with the original secret.
		creds, err := vaultresolve.Credentials(t.Context(), s.pool, cipher, "sesn_test", []string{vaultID})
		if err != nil {
			t.Fatalf("resolve narrowed: %v", err)
		}
		for _, c := range creds {
			if c.CredentialID == id && c.Secret != "kept-secret" {
				t.Fatalf("secret = %q, want kept-secret", c.Secret)
			}
		}
	})

	t.Run("unrestricted can preserve or narrow without secret_value, then cannot re-expand", func(t *testing.T) {
		cred := createCredential(t, s, vaultID, map[string]any{
			"type":         "environment_variable",
			"secret_name":  "UNREST_KEY",
			"secret_value": "unrest-secret",
			"networking":   map[string]any{"type": "unrestricted"},
		})
		id := cred["id"].(string)

		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "unrestricted"},
			},
		}); status != http.StatusOK {
			t.Fatalf("unrestricted unchanged: status %d (%v)", status, resp)
		}
		if status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"api.example.com"}},
			},
		}); status != http.StatusOK {
			t.Fatalf("unrestricted narrowed to limited: status %d (%v)", status, resp)
		}
		status, resp := s.do("POST", "/v1/vaults/"+vaultID+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "unrestricted"},
			},
		})
		wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error",
			"auth.networking: expanding destinations requires supplying secret_value")
	})

	t.Run("legacy credential without sealed binding digest still enforces no expansion", func(t *testing.T) {
		legacyVault := createVault(t, s, "legacy-vault")
		sealed, err := json.Marshal(map[string]string{"secret_value": "legacy-secret"})
		if err != nil {
			t.Fatal(err)
		}
		ct, keyID, err := cipher.Encrypt(t.Context(), sealed)
		if err != nil {
			t.Fatal(err)
		}
		credID := domain.NewID(domain.PrefixCredential).String()
		_, err = s.pool.Exec(t.Context(),
			`INSERT INTO vault_credentials
			   (id, vault_id, auth_type, auth, secret_ciphertext, secret_key_id, cred_key, metadata)
			 VALUES ($1, $2, 'environment_variable', $3::jsonb, $4, $5, 'name:LEGACY_KEY', '{}'::jsonb)`,
			credID, legacyVault,
			`{"type":"environment_variable","secret_name":"LEGACY_KEY","networking":{"type":"limited","allowed_hosts":["api.legacy.test"]},"injection_location":{"body":true,"header":true}}`,
			ct, keyID)
		if err != nil {
			t.Fatalf("insert legacy row: %v", err)
		}

		// Metadata-only update succeeds.
		if status, resp := s.do("POST", "/v1/vaults/"+legacyVault+"/credentials/"+credID, map[string]any{
			"display_name": "legacy-renamed",
			"metadata":     map[string]string{"k": "v"},
		}); status != http.StatusOK {
			t.Fatalf("legacy metadata update: status %d (%v)", status, resp)
		}

		// Expanding without secret_value is refused.
		status, resp := s.do("POST", "/v1/vaults/"+legacyVault+"/credentials/"+credID, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"api.legacy.test", "evil.test"}},
			},
		})
		wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error",
			"auth.networking: expanding destinations requires supplying secret_value")

		// Preserving destinations succeeds and upgrades the sealed binding digest.
		if status, resp := s.do("POST", "/v1/vaults/"+legacyVault+"/credentials/"+credID, map[string]any{
			"auth": map[string]any{
				"type":       "environment_variable",
				"networking": map[string]any{"type": "limited", "allowed_hosts": []string{"api.legacy.test"}},
			},
		}); status != http.StatusOK {
			t.Fatalf("legacy preserve update: status %d (%v)", status, resp)
		}

		// Verify the sealed JSON now holds approved_networking_sha256.
		var rawCT []byte
		var rawKeyID string
		if err := s.pool.QueryRow(t.Context(),
			`SELECT secret_ciphertext, secret_key_id FROM vault_credentials WHERE id = $1`, credID).
			Scan(&rawCT, &rawKeyID); err != nil {
			t.Fatal(err)
		}
		plain, err := cipher.Decrypt(t.Context(), rawCT, rawKeyID)
		if err != nil {
			t.Fatal(err)
		}
		var unpacked map[string]string
		if err := json.Unmarshal(plain, &unpacked); err != nil {
			t.Fatal(err)
		}
		wantDigest := egress.NetworkingBindingDigest(false, []string{"api.legacy.test"})
		if unpacked[egress.SealedApprovedNetworkingKey] != wantDigest {
			t.Fatalf("upgraded binding digest = %q, want %q", unpacked[egress.SealedApprovedNetworkingKey], wantDigest)
		}
	})

	t.Run("tampered auth column fails closed on update without secret_value and on resolve", func(t *testing.T) {
		tamperVault := createVault(t, s, "tamper-vault")
		cred := createCredential(t, s, tamperVault, map[string]any{
			"type":         "environment_variable",
			"secret_name":  "TAMPER_KEY",
			"secret_value": "real-secret",
			"networking":   map[string]any{"type": "limited", "allowed_hosts": []string{"api.good.test"}},
		})
		id := cred["id"].(string)

		// Simulate out-of-band DB tampering widening allowed_hosts.
		_, err := s.pool.Exec(t.Context(),
			`UPDATE vault_credentials
			 SET auth = jsonb_set(auth, '{networking}', '{"type":"limited","allowed_hosts":["api.good.test","evil.test"]}'::jsonb)
			 WHERE id = $1`, id)
		if err != nil {
			t.Fatalf("tamper row: %v", err)
		}

		// Update without secret_value must fail closed rather than adopting the tampered hosts.
		status, resp := s.do("POST", "/v1/vaults/"+tamperVault+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":               "environment_variable",
				"injection_location": map[string]any{"body": false},
			},
		})
		wantErrMsg(t, status, resp, http.StatusBadRequest, "invalid_request_error",
			"auth.networking: stored destination policy does not match sealed secret; supply secret_value to rebind")

		// Resolution for the gate also fails closed without leaking the secret.
		_, err = vaultresolve.Credentials(t.Context(), s.pool, cipher, "sesn_tamper", []string{tamperVault})
		if err == nil {
			t.Fatal("expected vaultresolve.Credentials to fail closed on tampered binding")
		}
		if strings.Contains(err.Error(), "real-secret") {
			t.Fatalf("resolve error leaked secret: %v", err)
		}

		// Supplying a new secret_value cleanly rebinds the credential.
		if status, resp := s.do("POST", "/v1/vaults/"+tamperVault+"/credentials/"+id, map[string]any{
			"auth": map[string]any{
				"type":         "environment_variable",
				"secret_value": "rebound-secret",
				"networking":   map[string]any{"type": "limited", "allowed_hosts": []string{"api.good.test"}},
			},
		}); status != http.StatusOK {
			t.Fatalf("rebind with new secret_value: status %d (%v)", status, resp)
		}
	})
}
