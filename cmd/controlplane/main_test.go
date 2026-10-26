package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func TestStartupSetupCode(t *testing.T) {
	for _, tc := range []struct {
		name, mode, roleMap string
		mint                bool
	}{
		{"empty mode", "", "", false},
		{"disabled mode", "disabled", "", false},
		{"admin mapping", "oidc", "staff=developer,owners=admin", false},
		{"no mapping", "oidc", "", true},
		{"non-admin mapping", "oidc", "staff=developer", true},
		{"proxy mode", "trusted_proxy", "", true},
		{"proxy admin mapping", "trusted_proxy", "owners=admin", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IDENTITY_MODE", tc.mode)
			t.Setenv("IDENTITY_ROLE_MAP", tc.roleMap)
			for _, name := range []string{
				"IDENTITY_OIDC_ISSUER", "IDENTITY_OIDC_AUDIENCE", "IDENTITY_OIDC_JWKS_URL",
				"IDENTITY_PROXY_PRESET", "IDENTITY_PROXY_AUDIENCE", "IDENTITY_PROXY_HEADER",
				"IDENTITY_PROXY_ISSUER", "IDENTITY_PROXY_KEYS_URL", "IDENTITY_PROXY_ALGS",
				"IDENTITY_CLAIM_ROLES", "IDENTITY_CLAIM_EMAIL", "IDENTITY_CLAIM_NAME",
			} {
				t.Setenv(name, "")
			}
			if tc.mode == "trusted_proxy" {
				t.Setenv("IDENTITY_PROXY_PRESET", "gcp-iap")
				t.Setenv("IDENTITY_PROXY_AUDIENCE", "/projects/example/global/backendServices/test")
			} else {
				t.Setenv("IDENTITY_OIDC_ISSUER", "https://idp.example")
				t.Setenv("IDENTITY_OIDC_AUDIENCE", "console")
			}
			pool := pgtest.NewPool(t)
			t.Setenv("DATABASE_URL", pool.Config().ConnString())
			t.Setenv("CONTROLPLANE_API_KEY", "startup-test-key")
			t.Setenv("BRAIN_API_KEY", "")
			// Stop run after setup, before any backend or IdP network call.
			t.Setenv("BLOB_BACKEND", "stop-after-setup")
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			if err := run(context.Background()); err == nil || !strings.Contains(err.Error(), `unknown BLOB_BACKEND "stop-after-setup"`) {
				t.Fatalf("startup did not reach the stop point: %v", err)
			}
			var codes, flags int
			if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM setup_codes),
				(SELECT count(*) FROM install_bootstrap)`).Scan(&codes, &flags); err != nil {
				t.Fatal(err)
			}
			if !tc.mint {
				if codes != 0 || flags != 0 || logs.Len() != 0 {
					t.Fatalf("startup minted or logged with %s: codes=%d flags=%d log=%s", tc.name, codes, flags, logs.String())
				}
				return
			}
			var entry struct {
				Level     string `json:"level"`
				Message   string `json:"msg"`
				Code      string `json:"setup_code"`
				ExpiresIn string `json:"expires_in"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("setup log = %q: %v", logs.String(), err)
			}
			if codes != 1 || flags != 0 || entry.Level != "WARN" || entry.Code == "" || entry.ExpiresIn != "1h" ||
				entry.Message != "no admin exists yet: redeem this one-time setup code from a signed-in session now" {
				t.Fatalf("startup setup code: codes=%d flags=%d log=%s", codes, flags, logs.String())
			}
			sum := sha256.Sum256([]byte(entry.Code))
			var hash string
			if err := pool.QueryRow(context.Background(), `SELECT code_hash FROM setup_codes`).Scan(&hash); err != nil || hash != hex.EncodeToString(sum[:]) {
				t.Fatalf("logged code not backed by its hash: hash=%q err=%v", hash, err)
			}
		})
	}
}
