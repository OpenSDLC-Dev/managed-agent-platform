package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func setupIdentityEnv(t *testing.T, mode, roleMap string) {
	t.Helper()
	t.Setenv("IDENTITY_MODE", mode)
	t.Setenv("IDENTITY_ROLE_MAP", roleMap)
	for _, name := range []string{
		"IDENTITY_OIDC_ISSUER", "IDENTITY_OIDC_AUDIENCE", "IDENTITY_OIDC_JWKS_URL",
		"IDENTITY_PROXY_PRESET", "IDENTITY_PROXY_AUDIENCE", "IDENTITY_PROXY_HEADER",
		"IDENTITY_PROXY_ISSUER", "IDENTITY_PROXY_KEYS_URL", "IDENTITY_PROXY_ALGS",
		"IDENTITY_CLAIM_ROLES", "IDENTITY_CLAIM_EMAIL", "IDENTITY_CLAIM_NAME",
	} {
		t.Setenv(name, "")
	}
	if mode == "trusted_proxy" {
		t.Setenv("IDENTITY_PROXY_PRESET", "gcp-iap")
		t.Setenv("IDENTITY_PROXY_AUDIENCE", "/projects/example/global/backendServices/test")
	} else {
		t.Setenv("IDENTITY_OIDC_ISSUER", "https://idp.example")
		t.Setenv("IDENTITY_OIDC_AUDIENCE", "console")
	}
}

func TestSetupCodeFileDelivery(t *testing.T) {
	setupIdentityEnv(t, "oidc", "")
	pool := pgtest.NewPool(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	neighbor := filepath.Join(temp, "keep")
	if err := os.WriteFile(neighbor, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	cleanup, err := deliverSetupCode(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("delivery returned no cleanup")
	}
	t.Cleanup(cleanup)
	var entry struct {
		File      string `json:"setup_code_file"`
		ExpiresIn string `json:"expires_in"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil || entry.File == "" || entry.ExpiresIn != "1h" {
		t.Fatalf("delivery did not log its path and expiry: %v", err)
	}
	info, err := os.Lstat(entry.File)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("delivery file mode = %v, want regular 0600", info.Mode())
	}
	dir := filepath.Dir(entry.File)
	info, err = os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 || filepath.Dir(dir) != temp {
		t.Fatalf("delivery directory is not private 0700 under the process temp directory: %v", err)
	}
	raw, err := os.ReadFile(entry.File)
	if err != nil {
		t.Fatal(err)
	}
	code := strings.TrimSuffix(string(raw), "\n")
	if len(code) != 32 {
		t.Fatal("delivery file does not hold the minted code")
	}
	if strings.Contains(logs.String(), code) || strings.Contains(logs.String(), `"setup_code":`) {
		t.Fatal("raw setup code leaked to ordinary logs")
	}
	sum := sha256.Sum256([]byte(code))
	var hash string
	if err := pool.QueryRow(context.Background(), `SELECT code_hash FROM setup_codes`).Scan(&hash); err != nil || hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("delivered code does not match the stored hash: %v", err)
	}
	cleanup()
	for _, path := range []string{entry.File, dir} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orderly cleanup left delivery resources: %v", err)
		}
	}
	if kept, err := os.ReadFile(neighbor); err != nil || string(kept) != "unchanged" {
		t.Fatalf("cleanup changed a resource it did not create: %v", err)
	}
}

func TestSetupCodeDeliveryFailure(t *testing.T) {
	setupIdentityEnv(t, "oidc", "")
	pool := pgtest.NewPool(t)
	parent := t.TempDir()
	blocked := filepath.Join(parent, "regular-file")
	if err := os.WriteFile(blocked, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	// A regular-file parent fails even when a root test process bypasses chmod.
	t.Setenv("TMPDIR", blocked)
	t.Setenv("DATABASE_URL", pool.Config().ConnString())
	t.Setenv("CONTROLPLANE_API_KEY", "startup-test-key")
	t.Setenv("BRAIN_API_KEY", "")
	t.Setenv("BLOB_BACKEND", "stop-after-setup")
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	err := run(context.Background())
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("startup did not fail on local delivery: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatal("failed delivery logged before a successful write and close")
	}
	const code = "TEST-SETUP-SECRET-MUST-NOT-BE-LOGGED"
	_, _, err = writeSetupCode(code)
	if err == nil || strings.Contains(err.Error(), code) || strings.Contains(logs.String(), code) {
		t.Fatal("delivery error is missing or exposes the raw code")
	}
	var codes int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM setup_codes`).Scan(&codes); err != nil || codes != 1 {
		t.Fatalf("startup did not mint before local delivery failed: codes=%d err=%v", codes, err)
	}
	files, err := os.ReadDir(parent)
	if err != nil || len(files) != 1 || files[0].Name() != "regular-file" {
		t.Fatalf("failed delivery created extra resources: entries=%d err=%v", len(files), err)
	}
	if kept, err := os.ReadFile(blocked); err != nil || string(kept) != "unchanged" {
		t.Fatalf("failed delivery changed its pre-existing destination: %v", err)
	}
}

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
			setupIdentityEnv(t, tc.mode, tc.roleMap)
			pool := pgtest.NewPool(t)
			t.Setenv("DATABASE_URL", pool.Config().ConnString())
			t.Setenv("CONTROLPLANE_API_KEY", "startup-test-key")
			t.Setenv("BRAIN_API_KEY", "")
			// Stop run after setup, before any backend or IdP network call.
			t.Setenv("BLOB_BACKEND", "stop-after-setup")
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
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
			files, err := os.ReadDir(temp)
			if err != nil || len(files) != 0 {
				t.Fatalf("startup left delivery resources behind: entries=%d err=%v", len(files), err)
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
				File      string `json:"setup_code_file"`
				ExpiresIn string `json:"expires_in"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("decode setup log: %v", err)
			}
			if entry.Code != "" {
				t.Fatal("raw setup code is present in ordinary startup logs")
			}
			if codes != 1 || flags != 0 || entry.Level != "WARN" || entry.File == "" || entry.ExpiresIn != "1h" ||
				!strings.Contains(entry.Message, "read") || !strings.Contains(entry.Message, "file") {
				t.Fatalf("startup delivery: codes=%d flags=%d; missing path, retrieval instruction or expiry", codes, flags)
			}
			if _, err := os.Stat(entry.File); !os.IsNotExist(err) {
				t.Fatalf("startup failure did not remove the delivery file: %v", err)
			}
		})
	}
}
