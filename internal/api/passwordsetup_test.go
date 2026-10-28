package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestPasswordSetupPreservesTombstones(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	id := s.seedLocal("removed", "admin", "deactivated")
	code, err := api.MintPasswordSetupCode(context.Background(), s.pool)
	if err != nil || code == "" {
		t.Fatalf("inactive local admin suppressed development setup: %v", err)
	}
	body := func(name string) string {
		return fmt.Sprintf(`{"code":%q,"login_name":%q,"password":%q}`, code, name, localPassword)
	}
	res, raw := s.request("POST", setupPath, body("removed"), nativeHeaders("", ""))
	if res.StatusCode != 409 {
		t.Fatalf("setup revived a tombstone=%d %s", res.StatusCode, raw)
	}
	var status string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM principals WHERE id=$1`, id).Scan(&status); err != nil || status != "deactivated" {
		t.Fatalf("lost tombstone=%s err=%v", status, err)
	}
	res, raw = s.request("POST", setupPath, body("replacement"), nativeHeaders("", ""))
	_, session := sessionReply(t, res, raw)
	if session.LoginName != "replacement" || session.ID == "user_"+strings.TrimPrefix(id, "principal_") {
		t.Fatalf("setup linked the old person=%s", raw)
	}
}

func TestPasswordSetupExpiredAndThrottled(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	code, err := api.MintPasswordSetupCode(context.Background(), s.pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `UPDATE setup_codes SET expires_at=clock_timestamp()-interval '1 second'`)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"code":%q,"login_name":"setupname","password":%q}`, code, localPassword)
	for i := 0; i < 6; i++ {
		res, raw := s.request("POST", setupPath, body, nativeHeaders("", ""))
		want := 400
		if i == 5 {
			want = 429
		}
		if res.StatusCode != want {
			t.Fatalf("setup attempt %d=%d want=%d body=%s", i+1, res.StatusCode, want, raw)
		}
		if i == 5 && res.Header.Get("Retry-After") == "" {
			t.Fatal("setup throttle lacks retry bound")
		}
	}
	var people, accounts int
	if err := s.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM principals),(SELECT count(*) FROM local_accounts)`).Scan(&people, &accounts); err != nil || people != 0 || accounts != 0 {
		t.Fatalf("failed setup wrote credentials: %d/%d err=%v", people, accounts, err)
	}
}

func TestNativeIdentitySetupRefusesPasswordPurpose(t *testing.T) {
	s := newNativeServer(t, login.ModeOIDC)
	code, err := api.MintPasswordSetupCode(context.Background(), s.pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO principals(id,issuer,subject) VALUES ('principal_person',$1,'person')`, s.idp.Issuer())
	if err != nil {
		t.Fatal(err)
	}
	cookie, session := s.issue("principal_person")
	res, raw := s.request("POST", setupPath, fmt.Sprintf(`{"code":%q}`, code), nativeHeaders(cookie, session.CSRFToken))
	if res.StatusCode != 400 || !strings.Contains(string(raw), "invalid or expired setup code") {
		t.Fatalf("identity setup accepted password-purpose code=%d %s", res.StatusCode, raw)
	}
}
