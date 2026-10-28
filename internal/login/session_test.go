package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func loginPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.NewPool(t)
	_, err := pool.Exec(context.Background(), `INSERT INTO principals
		(id, issuer, subject, email, display_name, role, role_source, last_seen_at)
		VALUES ('principal_abc123', 'https://idp.example', 'person', 'ada@example.test', 'Ada', 'developer', 'idp', '2020-01-02T03:04:05Z')`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func browserRequest(method, token string) *http.Request {
	r := httptest.NewRequest(method, "https://console.example/api/auth/session", nil)
	r.RemoteAddr = "192.0.2.1:12345"
	if token != "" {
		r.AddCookie(&http.Cookie{Name: "map_session", Value: token})
	}
	return r
}

func TestSessionsUseCurrentPrincipal(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	manager := New(pool, Config{Mode: ModeOIDC, PublicOrigin: "https://console.example"})
	var before, after time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	issued, err := manager.Issue(ctx, "principal_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if issued.PrincipalID != "principal_abc123" || issued.ID != "user_abc123" || issued.Role != identity.RoleDeveloper || issued.RoleSource != "idp" || issued.Name != "Ada" || issued.Email != "ada@example.test" || issued.LoginName != "" || !issued.SignedIn {
		t.Fatalf("issued profile = %+v", issued.Session)
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(issued.Token)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("session token not canonical random 32 bytes: length %d, err %v", len(decoded), err)
	}
	var storedHash string
	var created, expires time.Time
	if err := pool.QueryRow(ctx, `SELECT token_hash, created_at, expires_at FROM browser_sessions WHERE principal_id = 'principal_abc123'`).Scan(&storedHash, &created, &expires); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(issued.Token))
	if storedHash != hex.EncodeToString(digest[:]) || storedHash == issued.Token {
		t.Fatal("session table must contain only SHA-256, not the bearer token")
	}
	if expires.Sub(created) != 12*time.Hour || created.Before(before) || created.After(after) || !issued.ExpiresAt.Equal(expires) {
		t.Fatalf("DB absolute lifetime: created %s, expires %s, DB bounds [%s, %s]", created, expires, before, after)
	}
	for _, role := range []string{"user", "admin", "developer"} {
		if _, err := pool.Exec(ctx, `UPDATE principals SET role = $1 WHERE id = 'principal_abc123'`, role); err != nil {
			t.Fatal(err)
		}
		session, err := manager.Authenticate(ctx, browserRequest("GET", issued.Token))
		if err != nil {
			t.Fatal(err)
		}
		if string(session.Role) != role || session.RoleSource != "idp" || session.Name != "Ada" || session.Email != "ada@example.test" {
			t.Fatalf("cookie replayed stale authority/profile: %+v", session)
		}
		if !session.ExpiresAt.Equal(expires) || session.CSRFToken == "" || session.CSRFToken != issued.CSRFToken {
			t.Fatal("session refreshed or lost its deterministic CSRF token")
		}
	}
	var source, email, name string
	var lastSeen time.Time
	if err := pool.QueryRow(ctx, `SELECT role_source, email, display_name, last_seen_at FROM principals WHERE id = 'principal_abc123'`).Scan(&source, &email, &name, &lastSeen); err != nil {
		t.Fatal(err)
	}
	if source != "idp" || email != "ada@example.test" || name != "Ada" || !lastSeen.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatal("cookie lookup wrote sign-in mapping/profile/last_seen")
	}
	if _, err := pool.Exec(ctx, `UPDATE principals SET status = 'deactivated' WHERE id = 'principal_abc123'`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(ctx, browserRequest("GET", issued.Token)); !errors.Is(err, principal.ErrDeactivated) {
		t.Fatalf("deactivated lookup = %v", err)
	}
	if _, err := manager.Issue(ctx, "principal_abc123"); err == nil {
		t.Fatal("issued cookie to inactive human")
	}
	if _, err := pool.Exec(ctx, `UPDATE principals SET status = 'active', kind = 'service_account', role = 'developer' WHERE id = 'principal_abc123'`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authenticate(ctx, browserRequest("GET", issued.Token)); err == nil {
		t.Fatal("human cookie admitted a service account")
	}
	if _, err := manager.Issue(ctx, "principal_abc123"); err == nil {
		t.Fatal("issued cookie to service account")
	}
	if _, err := manager.Issue(ctx, "principal_missing"); err == nil {
		t.Fatal("issued cookie to missing principal")
	}
}

func TestSessionsLocalSubjectAndSerialization(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO principals (id, issuer, subject, email, display_name)
		VALUES ('principal_def456', 'urn:managed-agent-platform:local', 'local.ada', 'ada@example.test', 'Local Ada')`); err != nil {
		t.Fatal(err)
	}
	manager := New(pool, Config{Mode: ModePassword, Development: true, PublicOrigin: "http://localhost:3000"})
	issued, err := manager.Issue(ctx, "principal_def456")
	if err != nil {
		t.Fatal(err)
	}
	if issued.LoginName != "local.ada" || issued.PrincipalID != "principal_def456" || issued.ID != "user_def456" {
		t.Fatalf("local identity linked by email or lost immutable subject: %+v", issued.Session)
	}
	encoded, err := json.Marshal(issued)
	if err != nil {
		t.Fatal(err)
	}
	xmlEncoded, err := xml.Marshal(issued)
	if err != nil {
		t.Fatal(err)
	}
	yamlEncoded, err := yaml.Marshal(issued)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("issued", "session", issued)
	for _, rendered := range []string{string(encoded), string(xmlEncoded), string(yamlEncoded), fmt.Sprint(issued), fmt.Sprintf("%+v", issued), fmt.Sprintf("%#v", issued), logs.String()} {
		if strings.Contains(rendered, issued.Token) {
			t.Fatal("raw session token serialized or formatted")
		}
	}
	var dto map[string]any
	if err := json.Unmarshal(encoded, &dto); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"principal_id", "issuer", "subject", "token", "password_hash"} {
		if _, ok := dto[field]; ok {
			t.Fatalf("private field %s serialized", field)
		}
	}
	for _, field := range []string{"signed_in", "id", "role", "role_source", "email", "name", "login_name", "expires_at", "csrf_token"} {
		if _, ok := dto[field]; !ok {
			t.Fatalf("session DTO lost %s", field)
		}
	}
}

func TestSessionRevocationAcrossManagers(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	otherPool, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherPool.Close)
	cfg := Config{PublicOrigin: "https://console.example"}
	first, second := New(pool, cfg), New(otherPool, cfg)
	issued, err := first.Issue(ctx, "principal_abc123")
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.Issue(ctx, "principal_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == other.Token {
		t.Fatal("two active sessions reused a bearer token")
	}
	r := browserRequest("POST", issued.Token)
	if _, err := second.Authenticate(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := second.Revoke(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Authenticate(ctx, r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked cookie still valid across managers: %v", err)
	}
	if err := second.Revoke(ctx, r); err != nil {
		t.Fatalf("revocation not idempotent: %v", err)
	}
	if _, err := first.Authenticate(ctx, browserRequest("GET", other.Token)); err != nil {
		t.Fatalf("revocation removed a different live session: %v", err)
	}
	revokedToken := issued.Token
	issued, err = first.Issue(ctx, "principal_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if issued.Token == revokedToken || issued.Token == other.Token {
		t.Fatal("new issuance reused an earlier session bearer")
	}
	if _, err := first.Authenticate(ctx, r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("new issuance resurrected a revoked cookie: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE browser_sessions SET created_at = clock_timestamp() - interval '13 hours', expires_at = clock_timestamp() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Authenticate(ctx, browserRequest("GET", issued.Token)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired cookie = %v", err)
	}
}

func TestSessionsPruneExpiredCredentialsAndAttempts(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	_, err := pool.Exec(ctx, `INSERT INTO browser_sessions (token_hash, principal_id, created_at, expires_at) VALUES
		(repeat('a',64), 'principal_abc123', clock_timestamp()-interval '13 hours', clock_timestamp()-interval '1 hour'),
		(repeat('b',64), 'principal_abc123', clock_timestamp(), clock_timestamp()+interval '12 hours');
		INSERT INTO browser_login_attempts (state_hash,binding_hash,nonce,code_verifier,created_at,expires_at) VALUES
		(repeat('c',64), repeat('d',64), 'old-nonce','old-verifier',clock_timestamp()-interval '11 minutes',clock_timestamp()-interval '1 minute'),
		(repeat('e',64), repeat('f',64), 'live-nonce','live-verifier',clock_timestamp(),clock_timestamp()+interval '10 minutes')`)
	if err != nil {
		t.Fatal(err)
	}
	manager := New(pool, Config{PublicOrigin: "https://console.example"})
	if _, err := manager.Issue(ctx, "principal_abc123"); err != nil {
		t.Fatal(err)
	}
	var sessions, attempts int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM browser_sessions), (SELECT count(*) FROM browser_login_attempts)`).Scan(&sessions, &attempts); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || attempts != 1 {
		t.Fatalf("issuance pruning removed live or retained expired credentials: %d sessions, %d attempts", sessions, attempts)
	}
}

func TestSessionsFailClosedOnStoreFailure(t *testing.T) {
	for _, fault := range []string{"closed pool", "missing sessions", "missing attempts"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			pool := loginPool(t)
			manager := New(pool, Config{PublicOrigin: "https://console.example"})
			issued, err := manager.Issue(ctx, "principal_abc123")
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "closed pool":
				pool.Close()
			case "missing sessions":
				if _, err := pool.Exec(ctx, `DROP TABLE browser_sessions`); err != nil {
					t.Fatal(err)
				}
			case "missing attempts":
				if _, err := pool.Exec(ctx, `DROP TABLE browser_login_attempts`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := manager.Issue(ctx, "principal_abc123"); err == nil {
				t.Fatal("store failure issued a session")
			}
			if fault != "missing attempts" {
				if _, err := manager.Authenticate(ctx, browserRequest("GET", issued.Token)); err == nil {
					t.Fatal("store failure authenticated a cookie")
				}
				if err := manager.Revoke(ctx, browserRequest("POST", issued.Token)); err == nil {
					t.Fatal("store failure claimed successful revocation")
				}
			}
			if fault == "closed pool" {
				if _, err := manager.Throttle(ctx, browserRequest("POST", ""), "local-user"); err == nil {
					t.Fatal("store failure bypassed password throttle")
				}
			}
		})
	}
}
