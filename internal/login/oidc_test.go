package login

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

func oidcRequest(binding string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "https://console.example/api/auth/callback", nil)
	if binding != "" {
		r.AddCookie(&http.Cookie{Name: "map_login", Value: binding})
	}
	return r
}

// Storing plaintext state/binding, reusing random values, or using process memory
// instead of the DB must break this cross-manager attempt contract.
func TestOIDCCodeFlow(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	cfg := Config{Mode: ModeOIDC, PublicOrigin: "https://console.example"}
	first, second := New(pool, cfg), New(other, cfg)
	var before, after time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	state, nonce, verifier, binding, err := first.BeginOIDC(ctx, oidcRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, value := range []string{state, nonce, verifier, binding} {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
		if err != nil || len(decoded) != 32 || seen[value] {
			t.Fatal("attempt values must be independent canonical 32-byte random values")
		}
		seen[value] = true
	}
	var stateHash, bindingHash, storedNonce, storedVerifier string
	var created, expires time.Time
	if err := pool.QueryRow(ctx, `SELECT state_hash,binding_hash,nonce,code_verifier,created_at,expires_at FROM browser_login_attempts`).Scan(&stateHash, &bindingHash, &storedNonce, &storedVerifier, &created, &expires); err != nil {
		t.Fatal(err)
	}
	s, b := sha256.Sum256([]byte(state)), sha256.Sum256([]byte(binding))
	if stateHash != hex.EncodeToString(s[:]) || bindingHash != hex.EncodeToString(b[:]) || storedNonce != nonce || storedVerifier != verifier {
		t.Fatal("attempt persistence lost values or stored raw browser credentials")
	}
	if created.Before(before) || created.After(after) || expires.Sub(created) != 10*time.Minute {
		t.Fatalf("attempt did not get DB-clock ten-minute expiry: %v/%v", created, expires)
	}
	gotNonce, gotVerifier, err := second.ConsumeOIDC(ctx, oidcRequest(binding), state)
	if err != nil || gotNonce != nonce || gotVerifier != verifier {
		t.Fatalf("replica consumption lost nonce or PKCE verifier: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM browser_login_attempts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("consumed attempt remains: count %d, err %v", count, err)
	}
}

func TestOIDCRejectsReplayAndWrongBrowser(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	cfg := Config{Mode: ModeOIDC, PublicOrigin: "https://console.example"}
	first, second := New(pool, cfg), New(other, cfg)
	state, nonce, verifier, binding, err := first.BeginOIDC(ctx, oidcRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	otherState, _, _, otherBinding, err := second.BeginOIDC(ctx, oidcRequest(binding))
	if err != nil {
		t.Fatal(err)
	}
	if state == otherState || binding == otherBinding {
		t.Fatal("new attempt reused browser credentials")
	}
	for _, tc := range []struct{ name, state, cookie string }{
		{"missing cookie", state, ""},
		{"wrong browser", state, otherBinding},
		{"wrong state", otherState, binding},
		{"malformed state", "not-canonical", binding},
		{"malformed cookie", state, "not-canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, v, err := second.ConsumeOIDC(ctx, oidcRequest(tc.cookie), tc.state)
			if !errors.Is(err, ErrUnauthenticated) || n != "" || v != "" {
				t.Fatalf("unbound attempt returned credentials: %v", err)
			}
		})
	}
	for _, header := range []string{
		"map_login=" + binding + "; map_login=" + binding,
		"map_login=" + binding + "; map_login",
		"map_login=" + binding + "; map_login =" + binding,
		`map_login="` + binding + `"`,
		"map_login=" + binding + "=",
	} {
		r := oidcRequest("")
		r.Header.Set("Cookie", header)
		if n, v, err := first.ConsumeOIDC(ctx, r, state); !errors.Is(err, ErrUnauthenticated) || n != "" || v != "" {
			t.Fatal("ambiguous/noncanonical pending cookie consumed attempt")
		}
	}
	var wg sync.WaitGroup
	type result struct {
		nonce, verifier string
		err             error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, manager := range []*Manager{first, second} {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			<-start
			n, v, err := m.ConsumeOIDC(ctx, oidcRequest(binding), state)
			results <- result{n, v, err}
		}(manager)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for r := range results {
		if r.err == nil {
			successes++
			if r.nonce != nonce || r.verifier != verifier {
				t.Fatal("consumed wrong attempt")
			}
		} else if !errors.Is(r.err, ErrUnauthenticated) || r.nonce != "" || r.verifier != "" {
			t.Fatalf("replay produced unexpected result: %v", r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("one-time attempt had %d successful concurrent consumers", successes)
	}
	if _, _, err := first.ConsumeOIDC(ctx, oidcRequest(binding), state); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replay accepted: %v", err)
	}
	if _, _, err := second.ConsumeOIDC(ctx, oidcRequest(otherBinding), otherState); err != nil {
		t.Fatalf("failed/replayed attempts consumed an unrelated attempt: %v", err)
	}
}

// An expiry predicate evaluated before a blocked DELETE/SELECT is insufficient.
// This test observes a real lock wait while still live, then releases it only
// after DB time passes the already-committed expiry, without changing the row.
func TestOIDCExpiryAfterRowLockWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := pgtest.NewPool(t)
	manager := New(pool, Config{PublicOrigin: "https://console.example"})
	state, _, _, binding, err := manager.BeginOIDC(ctx, oidcRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	if err := pool.QueryRow(ctx, `UPDATE browser_login_attempts SET created_at=clock_timestamp()-interval '9 minutes', expires_at=clock_timestamp()+interval '2 seconds' WHERE state_hash=$1 RETURNING expires_at`, hashCredential(state)).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	lock, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	var blocker int
	if err := lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM browser_login_attempts WHERE state_hash=$1 FOR UPDATE`, hashCredential(state)).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	var n, v string
	var consumeErr error
	done := make(chan struct{})
	go func() { n, v, consumeErr = manager.ConsumeOIDC(ctx, oidcRequest(binding), state); close(done) }()
	defer func() { _ = lock.Rollback(context.Background()); cancel(); <-done }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked, live bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))), clock_timestamp()<$2`, blocker, expires).Scan(&blocked, &live)
		if err != nil {
			t.Fatal(err)
		}
		if !live {
			t.Fatal("attempt expired before the test observed a lock wait")
		}
		if blocked {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := lock.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(epoch FROM ($1::timestamptz-clock_timestamp())))+0.02)`, expires); err != nil {
		t.Fatal(err)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if !errors.Is(consumeErr, ErrUnauthenticated) || n != "" || v != "" {
		t.Fatalf("attempt expired while waiting yet returned credentials: %v", consumeErr)
	}
}

func TestOIDCExpiredAttemptsAndCancellation(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	manager := New(pool, Config{PublicOrigin: "https://console.example"})
	state, _, _, binding, err := manager.BeginOIDC(ctx, oidcRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE browser_login_attempts SET created_at=clock_timestamp()-interval '11 minutes',expires_at=clock_timestamp()-interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if n, v, err := manager.ConsumeOIDC(ctx, oidcRequest(binding), state); !errors.Is(err, ErrUnauthenticated) || n != "" || v != "" {
		t.Fatalf("expired attempt consumed: %v", err)
	}
	liveState, _, _, liveBinding, err := manager.BeginOIDC(ctx, oidcRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM browser_login_attempts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("begin did not prune expired attempts: %d, %v", count, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if n, v, err := manager.ConsumeOIDC(cancelled, oidcRequest(liveBinding), liveState); err == nil || n != "" || v != "" {
		t.Fatal("cancelled consumption returned credentials")
	}
	if _, _, err := manager.ConsumeOIDC(ctx, oidcRequest(liveBinding), liveState); err != nil {
		t.Fatalf("cancelled request spent the attempt: %v", err)
	}
	pool.Close()
	if s, n, v, b, err := manager.BeginOIDC(ctx, oidcRequest("")); err == nil || s != "" || n != "" || v != "" || b != "" {
		t.Fatal("failed store returned new attempt credentials")
	}
	if n, v, err := manager.ConsumeOIDC(ctx, oidcRequest(liveBinding), liveState); err == nil || n != "" || v != "" {
		t.Fatal("failed store returned consumed credentials")
	}
}

func TestOIDCPendingCookie(t *testing.T) {
	for _, tc := range []struct {
		origin              string
		development, secure bool
	}{
		{"https://console.example", false, true},
		{"http://localhost:3000", true, false},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			manager := New(nil, Config{PublicOrigin: tc.origin, Development: tc.development})
			recorder := httptest.NewRecorder()
			manager.SetOIDCCookie(recorder, strings.Repeat("A", 43))
			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatal("missing/duplicate pending cookie")
			}
			cookie := cookies[0]
			if cookie.Name != "map_login" || cookie.Value != strings.Repeat("A", 43) || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || cookie.Secure != tc.secure || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
				t.Fatalf("pending cookie attributes = %+v", cookie)
			}
			recorder = httptest.NewRecorder()
			manager.ClearOIDCCookie(recorder)
			cookie = recorder.Result().Cookies()[0]
			if cookie.Name != "map_login" || cookie.Value != "" || cookie.MaxAge != -1 || !cookie.Expires.Before(time.Now()) || cookie.Path != "/" || cookie.Domain != "" || !cookie.HttpOnly || cookie.Secure != tc.secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("pending cookie clear attributes = %+v", cookie)
			}
		})
	}
}
