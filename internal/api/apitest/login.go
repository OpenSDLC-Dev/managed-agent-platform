// Package apitest drives native setup and login for acceptance and eval stacks.
// It obtains management credentials through the same private HTTP API as Console.
package apitest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PasswordLogin configures the development login exercised by SetupKey.
func PasswordLogin(pool *pgxpool.Pool) api.Option {
	return api.WithLogin(login.New(pool, login.Config{
		Mode: login.ModePassword, Development: true, PublicOrigin: "http://localhost:3000",
	}), nil)
}

// SetupKey mirrors boot's setup-code mint, then redeems it, logs out and signs
// in again before issuing an owned key. It seeds no principals or credentials.
func SetupKey(t *testing.T, pool *pgxpool.Pool, baseURL string) string {
	t.Helper()
	code, err := api.MintPasswordSetupCode(context.Background(), pool)
	if err != nil || code == "" {
		t.Fatal("mint password setup code failed", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	post := func(path string, body any, csrf string) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:3000")
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var value map[string]any
		if res.StatusCode != http.StatusOK {
			t.Fatalf("native %s: status %d", path, res.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	credentials := map[string]string{"login_name": "acceptance-admin", "password": "local-test-password-69"}
	setup := post("/api/console/organizations/default/setup_code/redeem", map[string]string{
		"code": code, "login_name": credentials["login_name"], "password": credentials["password"],
	}, "")
	csrf, _ := setup["csrf_token"].(string)
	post("/api/auth/logout", map[string]any{}, csrf)
	session := post("/api/auth/login", credentials, "")
	csrf, _ = session["csrf_token"].(string)
	issued := post("/api/console/organizations/default/workspaces/default/api_keys", map[string]string{"name": "acceptance"}, csrf)
	owner, _ := issued["principal"].(map[string]any)
	key, _ := issued["raw_key"].(string)
	if key == "" || owner["type"] != "user_actor" || owner["user_id"] != session["id"] {
		t.Fatal("private issuance did not return a key owned by the signed-in admin")
	}
	post("/api/auth/logout", map[string]any{}, csrf)
	return key
}
