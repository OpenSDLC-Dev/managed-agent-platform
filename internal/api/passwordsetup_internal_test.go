package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/pgtest"
)

func passwordSetupRequest(ctx context.Context, code string) *http.Request {
	r := httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"code":%q,"login_name":"freshadmin","password":"fixture-password-123"}`, code))).WithContext(ctx)
	r.SetPathValue("org", "default")
	return r
}

// A check made before the principal lock wait is reject-only. Changing a code
// or creating a local admin while it waits must defeat the pending redemption.
func TestPasswordSetupRechecksAfterLockWait(t *testing.T) {
	for _, tc := range []struct {
		name, change string
		want         int
	}{
		{"database clock expiry", `UPDATE setup_codes SET expires_at=clock_timestamp()`, 400},
		{"purpose switch", `UPDATE setup_codes SET purpose='identity'`, 400},
		{"spent code", `DELETE FROM setup_codes`, 400},
		{"active local admin without password row", `INSERT INTO principals(id,issuer,subject,role) VALUES ('principal_other','urn:managed-agent-platform:local','other','admin')`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			pgtest.DefaultRepeatableRead(t, pool)
			code, err := MintPasswordSetupCode(context.Background(), pool)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			writer, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(context.Background())
			if _, err := writer.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES ('principal_busy','https://idp.example','busy')`); err != nil {
				t.Fatal(err)
			}
			s := &server{pool: pool, login: login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: "https://console.example", Development: true})}
			done := make(chan setupResult, 1)
			go func() {
				value, err := s.createSetupLocalAdmin(passwordSetupRequest(ctx, code))
				done <- setupResult{value, err}
			}()
			waitSetupLock(t, ctx, pool, "principals", done)
			if _, err := writer.Exec(ctx, tc.change); err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			result := <-done
			var ae *apiError
			if !errors.As(result.err, &ae) || ae.status != tc.want {
				t.Fatalf("post-wait setup=%v/%v want=%d", result.value, result.err, tc.want)
			}
			var created, accounts int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM principals WHERE subject='freshadmin'),(SELECT count(*) FROM local_accounts)`).Scan(&created, &accounts); err != nil || created != 0 || accounts != 0 {
				t.Fatalf("rejected setup created rows=%d/%d err=%v", created, accounts, err)
			}
		})
	}
}

func TestPasswordSetupRejectPrecheckDoesNotWaitForPrincipalWriter(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err := writer.Exec(ctx, `INSERT INTO principals(id,issuer,subject) VALUES ('principal_busy','https://idp.example','busy')`); err != nil {
		t.Fatal(err)
	}
	s := &server{pool: pool, login: login.New(pool, login.Config{Mode: login.ModePassword, PublicOrigin: "https://console.example", Development: true})}
	_, err = s.createSetupLocalAdmin(passwordSetupRequest(ctx, "invalid"))
	var ae *apiError
	if !errors.As(err, &ae) || ae.status != 400 {
		t.Fatalf("cheap invalid-code precheck waited or changed response: %v", err)
	}
}
