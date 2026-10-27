package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

func TestConsoleSetupCodePath(t *testing.T) {
	if consoleSetupCodePath != "/api/console/organizations/{org}/setup_code/redeem" {
		t.Fatalf("setup code path = %q", consoleSetupCodePath)
	}
}

type setupResult struct {
	value any
	err   error
}

// The identity context predates the competing write, as it can in an HTTP
// request between Resolve and redemption. The transaction must not trust it.
func setupRequest(ctx context.Context, code string) *http.Request {
	ctx = context.WithValue(ctx, ctxKeyIdentity, identityPrincipal{ID: "principal_caller", Role: identity.RoleUser})
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"code":"`+code+`"}`)).WithContext(ctx)
	r.SetPathValue("org", "default")
	return r
}

func startSetupRedemption(ctx context.Context, pool *pgxpool.Pool, code string) <-chan setupResult {
	done := make(chan setupResult, 1)
	go func() {
		value, err := (&server{pool: pool}).redeemSetupCode(setupRequest(ctx, code))
		done <- setupResult{value, err}
	}()
	return done
}

func waitSetupLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, done <-chan setupResult) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND relation = $1::regclass AND NOT granted)`, table).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case result := <-done:
			t.Fatalf("operation finished before waiting on %s: %v / %v", table, result.value, result.err)
		case <-ctx.Done():
			t.Fatalf("operation never waited on %s: %v", table, ctx.Err())
		case <-tick.C:
		}
	}
}

func setupCallerPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool := pgtest.NewPool(t)
	if _, err := pool.Exec(context.Background(), `INSERT INTO principals (id, issuer, subject)
		VALUES ('principal_caller', 'https://idp.example', 'caller')`); err != nil {
		t.Fatal(err)
	}
	code, err := MintSetupCode(context.Background(), pool, false)
	if err != nil || code == "" {
		t.Fatalf("mint = %q, %v", code, err)
	}
	return pool, code
}

type unusedSetupBody struct{ read bool }

func (b *unusedSetupBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("closed setup must not read the body")
}
func (*unusedSetupBody) Close() error { return nil }

func TestClosedSetupDoesNotBlockPrincipalWriters(t *testing.T) {
	pool, _ := setupCallerPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE principals SET role = 'admin' WHERE id = 'principal_caller'`); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	// An unrelated sign-in holds ROW EXCLUSIVE, which must not make a
	// permanently closed redemption wait for a first-admin table lock.
	if _, err := writer.Exec(ctx, `INSERT INTO principals (id, issuer, subject)
		VALUES ('principal_busy', 'https://idp.example', 'busy')`); err != nil {
		t.Fatal(err)
	}
	r := setupRequest(ctx, "irrelevant")
	body := &unusedSetupBody{}
	r.Body = body
	w := httptest.NewRecorder()
	s := &server{pool: pool}
	noStore(s.handle(identity.RoleUser, s.redeemSetupCode))(w, r)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "an admin already exists") {
		t.Fatalf("closed redemption waited for principal writes: status=%d body=%s", w.Code, w.Body.String())
	}
	if body.read {
		t.Error("closed redemption read an irrelevant client body")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" {
		t.Error("closed redemption lost its no-store headers")
	}
}

func TestInvalidSetupCodeDoesNotWaitForPrincipalWriters(t *testing.T) {
	for _, tc := range []struct {
		name, body, message string
		expired             bool
	}{
		{"unknown code", `{"code":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`, "invalid or expired setup code", false},
		{"expired code", "", "invalid or expired setup code", true},
		{"malformed body", `{"code":`, "request body must be a JSON object", false},
		{"missing code", `{}`, "code is required", false},
		{"empty code", `{"code":""}`, "code is required", false},
		{"null code", `{"code":null}`, "code is required", false},
		{"non-string code", `{"code":7}`, "code must be a string", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, code := setupCallerPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			body := tc.body
			if tc.expired {
				if _, err := pool.Exec(ctx, `UPDATE setup_codes SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
					t.Fatal(err)
				}
				body = `{"code":"` + code + `"}`
			}
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			// This unrelated sign-in holds ROW EXCLUSIVE. Rejected requests
			// must finish without queuing a conflicting first-admin table lock.
			if _, err := writer.Exec(ctx, `INSERT INTO principals (id, issuer, subject)
				VALUES ('principal_busy', 'https://idp.example', 'busy')`); err != nil {
				t.Fatal(err)
			}
			r := setupRequest(ctx, "")
			r.Body = io.NopCloser(strings.NewReader(body))
			w := httptest.NewRecorder()
			s := &server{pool: pool}
			done := make(chan struct{})
			go func() {
				defer close(done)
				noStore(s.handle(identity.RoleUser, s.redeemSetupCode))(w, r)
			}()
			defer func() {
				cancel()
				_ = writer.Rollback(context.Background())
				<-done
			}()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
		wait:
			for {
				select {
				case <-done:
					break wait
				case <-ctx.Done():
					t.Fatalf("invalid setup request did not finish: %v", ctx.Err())
				case <-tick.C:
				}
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
					WHERE database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND relation = 'principals'::regclass AND NOT granted)`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					t.Fatal("invalid setup request queued a conflicting principals lock instead of returning 400")
				}
			}
			if w.Code != 400 || !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("invalid redemption = %d / %s, want 400 / %q", w.Code, w.Body.String(), tc.message)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" {
				t.Error("invalid redemption lost its no-store headers")
			}
		})
	}
}

func TestAnAdminAppearingWhileReadingTheSetupBodyTakesPriority(t *testing.T) {
	for _, body := range []string{`{"code":"wrong"}`, `{"code":`, `{}`, `{"code":7}`} {
		t.Run(body, func(t *testing.T) {
			pool, _ := setupCallerPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			stop := context.AfterFunc(ctx, func() {
				_ = reader.CloseWithError(ctx.Err())
				_ = writer.CloseWithError(ctx.Err())
			})
			defer stop()
			r := setupRequest(ctx, "")
			r.Body = reader
			w := httptest.NewRecorder()
			s := &server{pool: pool}
			done := make(chan struct{})
			go func() {
				defer close(done)
				noStore(s.handle(identity.RoleUser, s.redeemSetupCode))(w, r)
			}()
			defer func() {
				cancel()
				_ = reader.Close()
				_ = writer.Close()
				<-done
			}()
			// Write returns only once the handler is reading the body, after
			// its initial bootstrap check. Withhold EOF until the admin commits.
			if _, err := writer.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO principals (id, issuer, subject, role)
				VALUES ('principal_first', 'https://idp.example', 'first', 'admin')`); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			<-done
			if w.Code != 409 || !strings.Contains(w.Body.String(), "an admin already exists") {
				t.Fatalf("body validation overtook bootstrap: status=%d body=%s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Pragma") != "no-cache" {
				t.Error("closed redemption lost its no-store headers")
			}
		})
	}
}

func TestSetupCodeRechecksValidityAfterWaitingForLocks(t *testing.T) {
	for _, tc := range []struct{ name, change string }{
		{"expiry", `UPDATE setup_codes SET expires_at = clock_timestamp() - interval '1 second'`},
		{"changed hash", `UPDATE setup_codes SET code_hash = 'invalidated-' || code_hash`},
		{"removed hash", `DELETE FROM setup_codes`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, code := setupCallerPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			if _, err := writer.Exec(ctx, `INSERT INTO principals (id, issuer, subject)
				VALUES ('principal_busy', 'https://idp.example', 'busy')`); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Exec(ctx, tc.change); err != nil {
				t.Fatal(err)
			}
			// The unlocked read sees the old unexpired hash. Only the locked
			// recheck can see the invalidation committed after this lock wait.
			done := startSetupRedemption(ctx, pool, code)
			waitSetupLock(t, ctx, pool, "principals", done)
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			result := <-done
			var ae *apiError
			if !errors.As(result.err, &ae) || ae.status != 400 || ae.message != "invalid or expired setup code" {
				t.Fatalf("stale code redemption = %v / %v, want 400", result.value, result.err)
			}
			var role string
			var bootstrapped bool
			if err := pool.QueryRow(ctx, `SELECT role, EXISTS (SELECT 1 FROM install_bootstrap)
				FROM principals WHERE id = 'principal_caller'`).Scan(&role, &bootstrapped); err != nil || role != "user" || bootstrapped {
				t.Fatalf("invalid code promoted caller: role=%s bootstrapped=%v err=%v", role, bootstrapped, err)
			}
		})
	}
}

func TestSetupCodeRechecksTheCallerUnderLock(t *testing.T) {
	for _, tc := range []struct {
		name, change, message string
		status                int
	}{
		{"deactivation", "status = 'deactivated'", "only an active user principal can redeem a setup code", 403},
		{"mapping", "role = 'developer', role_source = 'idp'", "your role comes from the identity provider's mapping", 409},
		{"service account", "kind = 'service_account', role = 'developer'", "only an active user principal can redeem a setup code", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, code := setupCallerPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			if _, err := writer.Exec(ctx, `UPDATE principals SET `+tc.change+` WHERE id = 'principal_caller'`); err != nil {
				t.Fatal(err)
			}
			done := startSetupRedemption(ctx, pool, code)
			waitSetupLock(t, ctx, pool, "principals", done)
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			result := <-done
			var ae *apiError
			if !errors.As(result.err, &ae) || ae.status != tc.status || ae.message != tc.message {
				t.Fatalf("stale caller redemption = %v / %v, want %d / %q", result.value, result.err, tc.status, tc.message)
			}
			var c, f int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM setup_codes),
				(SELECT count(*) FROM install_bootstrap)`).Scan(&c, &f); err != nil || c != 1 || f != 0 {
				t.Fatalf("refusal consumed code or bootstrapped: codes=%d flags=%d err=%v", c, f, err)
			}
		})
	}
}

func TestAnInFlightMappedAdminClosesSetup(t *testing.T) {
	pool, code := setupCallerPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := principal.Resolve(ctx, writer, identity.Identity{Issuer: "https://idp.example", Subject: "mapped-admin", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	done := startSetupRedemption(ctx, pool, code)
	waitSetupLock(t, ctx, pool, "principals", done)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	result := <-done
	var ae *apiError
	if !errors.As(result.err, &ae) || ae.status != 409 || ae.message != "an admin already exists" {
		t.Fatalf("redemption raced mapped admin = %v / %v, want 409", result.value, result.err)
	}
	var admins int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM principals WHERE role = 'admin'`).Scan(&admins); err != nil || admins != 1 {
		t.Fatalf("admins = %d, %v; want only the mapped admin", admins, err)
	}
}

func TestMintSetupCodeStartsItsTTLAfterWaitingForLocks(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(ctx, `INSERT INTO principals (id, issuer, subject)
		VALUES ('principal_busy', 'https://idp.example', 'busy')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan setupResult, 1)
	go func() {
		code, err := MintSetupCode(ctx, pool, false)
		done <- setupResult{code, err}
	}()
	waitSetupLock(t, ctx, pool, "principals", done)
	var barrier time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&barrier); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.err != nil || result.value == "" {
		t.Fatalf("mint after lock release failed: %v", result.err)
	}
	var created, expires time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at, expires_at FROM setup_codes`).Scan(&created, &expires); err != nil {
		t.Fatal(err)
	}
	if created.Before(barrier) || expires.Before(barrier.Add(time.Hour)) || expires.Sub(created) != time.Hour {
		t.Fatalf("lock waiting consumed setup-code TTL: created=%v expires=%v released-after=%v", created, expires, barrier)
	}
}

func TestMintSetupCodeWaitsForAnInFlightAdmin(t *testing.T) {
	pool, _ := setupCallerPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := principal.Resolve(ctx, writer, identity.Identity{Issuer: "https://idp.example", Subject: "mapped-admin", Role: identity.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	done := make(chan setupResult, 1)
	go func() {
		code, err := MintSetupCode(ctx, pool, false)
		done <- setupResult{code, err}
	}()
	waitSetupLock(t, ctx, pool, "principals", done)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result := <-done; result.err != nil || result.value != "" {
		t.Fatalf("mint overtook the first admin: %v / %v", result.value, result.err)
	}
}

func TestSetupLocksPrincipalsBeforeCodesAndMapping(t *testing.T) {
	pool, code := setupCallerPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(ctx, `LOCK TABLE setup_codes IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	redeem := startSetupRedemption(ctx, pool, code)
	waitSetupLock(t, ctx, pool, "setup_codes", redeem)
	mapped := make(chan setupResult, 1)
	go func() {
		p, err := principal.Resolve(ctx, pool, identity.Identity{Issuer: "https://idp.example", Subject: "mapped-admin", Role: identity.RoleAdmin})
		mapped <- setupResult{p, err}
	}()
	waitSetupLock(t, ctx, pool, "principals", mapped)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result := <-redeem; result.err != nil {
		t.Fatalf("redemption did not win before mapping: %v", result.err)
	}
	if result := <-mapped; result.err != nil {
		t.Fatalf("mapping failed after redemption (lock inversion): %v", result.err)
	}
	var codes, flags int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM setup_codes),
		(SELECT count(*) FROM install_bootstrap)`).Scan(&codes, &flags); err != nil || codes != 0 || flags != 1 {
		t.Fatalf("after serialized redemption and mapping: codes=%d flags=%d err=%v", codes, flags, err)
	}
}
