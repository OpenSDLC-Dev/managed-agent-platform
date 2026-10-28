package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
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
		"CONSOLE_PUBLIC_ORIGIN", "DEVELOPMENT_MODE", "IDENTITY_OIDC_CLIENT_ID", "IDENTITY_OIDC_CLIENT_SECRET",
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

func TestStartupAdminMapInvalidatesOldSetupCodes(t *testing.T) {
	setupIdentityEnv(t, "oidc", "owners=admin")
	pool := pgtest.NewPool(t)
	code, err := api.MintSetupCode(context.Background(), pool, false)
	if err != nil || code == "" {
		t.Fatalf("could not mint the old configuration's code: %v", err)
	}
	t.Setenv("DATABASE_URL", pool.Config().ConnString())
	t.Setenv("CONTROLPLANE_API_KEY", "startup-test-key")
	t.Setenv("BRAIN_API_KEY", "")
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
		(SELECT count(*) FROM install_bootstrap)`).Scan(&codes, &flags); err != nil || codes != 0 || flags != 0 {
		t.Fatalf("admin-map startup left old setup codes or invented an admin: codes=%d flags=%d err=%v", codes, flags, err)
	}
	files, err := os.ReadDir(temp)
	if err != nil || len(files) != 0 || logs.Len() != 0 {
		t.Fatalf("admin-map startup delivered a code: entries=%d err=%v", len(files), err)
	}
}

// Invalid native settings must fail before setup mints or delivers anything.
// This starts run itself, not an independently assembled handler. Omitting
// WithLogin or constructing a JWT verifier in password mode must fail it.
func TestStartupNativeHTTP(t *testing.T) {
	setupIdentityEnv(t, "password", "")
	pool := pgtest.NewPool(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	origin := "http://" + addr
	temp := t.TempDir()
	for key, value := range map[string]string{"DATABASE_URL": pool.Config().ConnString(), "CONTROLPLANE_API_KEY": "native-startup-test-key", "BRAIN_API_KEY": "", "DEVELOPMENT_MODE": "true", "CONSOLE_PUBLIC_ORIGIN": origin, "CONTROLPLANE_ADDR": addr, "BLOB_BACKEND": "", "SECRETS_BACKEND": "", "DREAM_TICK_INTERVAL": "0", "TMPDIR": temp} {
		t.Setenv(key, value)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	// Wait on our own process inside the test. Cleanup verifies it really exits.
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown=%v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("control plane did not stop")
		}
	}()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(10 * time.Second)
	ready := false
	for !ready {
		res, err := client.Get(origin + "/api/auth/config")
		if err == nil {
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || !bytes.Contains(raw, []byte(`"mode":"password"`)) || !bytes.Contains(raw, []byte(`"native_login":true`)) {
				t.Fatalf("startup login wiring=%d %s", res.StatusCode, raw)
			}
			ready = true
			break
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("native startup did not listen")
		}
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 1 {
		t.Fatalf("startup delivery entries=%d err=%v", len(entries), err)
	}
	codeFile := filepath.Join(temp, entries[0].Name(), "setup-code")
	raw, err := os.ReadFile(codeFile)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"code": strings.TrimSpace(string(raw)), "login_name": "startupadmin", "password": "startup-test-password"})
	req, _ := http.NewRequest("POST", origin+"/api/console/organizations/default/setup_code/redeem", bytes.NewReader(body))
	req.Header.Set("Origin", origin)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !bytes.Contains(reply, []byte(`"role":"admin"`)) || len(res.Cookies()) != 1 {
		t.Fatalf("native startup setup=%d %s", res.StatusCode, reply)
	}
}

func TestStartupPasswordRestrictedDelivery(t *testing.T) {
	setupIdentityEnv(t, "password", "")
	t.Setenv("DEVELOPMENT_MODE", "true")
	t.Setenv("CONSOLE_PUBLIC_ORIGIN", "http://localhost:3000")
	t.Setenv("TMPDIR", t.TempDir())
	pool := pgtest.NewPool(t)
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	cleanup, err := deliverSetupCode(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("password startup omitted delivery")
	}
	defer cleanup()
	var entry struct {
		File    string `json:"setup_code_file"`
		Message string `json:"msg"`
	}
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entry.Message, "password") || strings.Contains(entry.Message, "signed-in") {
		t.Fatalf("password setup instruction requires prior sign-in: %q", entry.Message)
	}
	raw, err := os.ReadFile(entry.File)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(raw))) != 32 || strings.Contains(logs.String(), strings.TrimSpace(string(raw))) {
		t.Fatal("missing raw code or leaked code")
	}
	for path, mode := range map[string]os.FileMode{entry.File: 0600, filepath.Dir(entry.File): 0700} {
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("restricted delivery mode at %s: %v %v", path, info, err)
		}
	}
	cleanup()
	if _, err := os.Stat(entry.File); !os.IsNotExist(err) {
		t.Fatalf("owned file survived cleanup: %v", err)
	}
}

func TestStartupPasswordSetupEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, issuer, kind, role, status string
		mint                             bool
	}{
		{"external admin", "https://idp.example", "user", "admin", "active", true},
		{"inactive local admin", "urn:managed-agent-platform:local", "user", "admin", "deactivated", true},
		{"active local admin without password row", "urn:managed-agent-platform:local", "user", "admin", "active", false},
		{"local developer", "urn:managed-agent-platform:local", "user", "developer", "active", true},
		{"service admin", "urn:managed-agent-platform:local", "service_account", "admin", "active", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupIdentityEnv(t, "password", "")
			t.Setenv("DEVELOPMENT_MODE", "true")
			t.Setenv("CONSOLE_PUBLIC_ORIGIN", "http://localhost:3000")
			t.Setenv("TMPDIR", t.TempDir())
			pool := pgtest.NewPool(t)
			_, err := pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject,kind,role,status) VALUES ('principal_before',$1,'before',$2,$3,$4)`, tc.issuer, tc.kind, tc.role, tc.status)
			if err != nil {
				t.Fatal(err)
			}
			cleanup, err := deliverSetupCode(context.Background(), pool)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil || (cleanup != nil) != tc.mint {
				t.Fatalf("mint=%t want=%t err=%v", cleanup != nil, tc.mint, err)
			}
			if tc.mint {
				var purpose string
				if err := pool.QueryRow(context.Background(), `SELECT purpose FROM setup_codes`).Scan(&purpose); err != nil || purpose != "password" {
					t.Fatalf("purpose=%q err=%v", purpose, err)
				}
			}
		})
	}
}

func TestStartupNativeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, development, origin string
		valid                     bool
	}{
		{"password needs development", "", "https://console.example", false},
		{"password needs origin", "true", "", false},
		{"reject remote HTTP", "true", "http://console.example", false},
		{"reject origin path", "true", "https://console.example/", false},
		{"reject origin credentials", "true", "https://user:secret@console.example", false},
		{"reject default HTTP port", "true", "http://localhost:80", false},
		{"reject case and default HTTPS port", "true", "https://CONSOLE.example:443", false},
		{"canonical default HTTP", "true", "http://localhost", true},
		{"canonical nondefault HTTPS", "true", "https://console.example:8443", true},
		{"loopback development", "true", "http://localhost:3000", true},
		{"HTTPS development", "true", "https://console.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupIdentityEnv(t, "password", "")
			t.Setenv("DEVELOPMENT_MODE", tc.development)
			t.Setenv("CONSOLE_PUBLIC_ORIGIN", tc.origin)
			pool := pgtest.NewPool(t)
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			cleanup, err := deliverSetupCode(context.Background(), pool)
			if cleanup != nil {
				defer cleanup()
			}
			var codes int
			if qerr := pool.QueryRow(context.Background(), `SELECT count(*) FROM setup_codes`).Scan(&codes); qerr != nil {
				t.Fatal(qerr)
			}
			if tc.valid {
				if err != nil || cleanup == nil || codes != 1 {
					t.Fatalf("password delivery: codes=%d cleanup=%t err=%v", codes, cleanup != nil, err)
				}
			} else if err == nil || codes != 0 || cleanup != nil {
				t.Fatalf("bad settings minted a code: codes=%d err=%v", codes, err)
			}
		})
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
