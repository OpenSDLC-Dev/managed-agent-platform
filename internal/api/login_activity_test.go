package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

func TestLocalLoginActivityAndRetention(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	id := s.seedLocal("recent-local", "developer", "active")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE principals SET created_at=now()-interval '181 days',last_seen_at=now()-interval '181 days',email='recent@example.test',display_name='Recently Active' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var before, after, seen time.Time
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	res, raw := s.request("POST", "/api/auth/login", fmt.Sprintf(`{"login_name":"recent-local","password":%q}`, localPassword), nativeHeaders("", ""))
	cookie, _ := sessionReply(t, res, raw)
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp(),last_seen_at FROM principals WHERE id=$1`, id).Scan(&after, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Before(before) || seen.After(after) {
		t.Errorf("successful local login last_seen=%v outside database bounds [%v,%v]", seen, before, after)
	}
	res, raw = s.request("GET", "/api/auth/session", "", nativeHeaders(cookie, ""))
	if res.StatusCode != 200 {
		t.Fatalf("usable cookie=%d %s", res.StatusCode, raw)
	}
	var readSeen time.Time
	if err := s.pool.QueryRow(ctx, `SELECT last_seen_at FROM principals WHERE id=$1`, id).Scan(&readSeen); err != nil || !readSeen.Equal(seen) {
		t.Fatalf("cookie read wrote login activity: %v err=%v", readSeen, err)
	}
	admin := seedMember(t, s, "retention-admin", "admin", "console")
	ac, as := s.issue(admin)
	expectManagement(t, s, "DELETE", membersPath+"/"+principal.PublicID(id, "user"), "", nativeHeaders(ac, as.CSRFToken), 200)
	tag, err := s.pool.Exec(ctx, `WITH scrubbed AS (
  UPDATE principals SET email='',display_name='',description=''
  WHERE status='deactivated' AND last_seen_at<now()-interval '180 days' RETURNING id
 ) DELETE FROM local_accounts USING scrubbed WHERE local_accounts.principal_id=scrubbed.id`)
	if err != nil {
		t.Fatal(err)
	}
	var email, name string
	var accounts int
	if err := s.pool.QueryRow(ctx, `SELECT email,display_name,(SELECT count(*) FROM local_accounts WHERE principal_id=$1) FROM principals WHERE id=$1`, id).Scan(&email, &name, &accounts); err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 0 || email != "recent@example.test" || name != "Recently Active" || accounts != 1 {
		t.Fatalf("retention erased recent login: deleted=%d email=%q name=%q accounts=%d", tag.RowsAffected(), email, name, accounts)
	}
}

func TestFailedLocalLoginDoesNotRecordActivity(t *testing.T) {
	for _, failure := range []string{"wrong-password", "inactive", "unknown", "issuance-failure"} {
		t.Run(failure, func(t *testing.T) {
			s := newNativeServer(t, login.ModePassword)
			id := s.seedLocal("failed-local", "developer", "active")
			ctx := context.Background()
			var original, seen time.Time
			if err := s.pool.QueryRow(ctx, `UPDATE principals SET last_seen_at=now()-interval '181 days' WHERE id=$1 RETURNING last_seen_at`, id).Scan(&original); err != nil {
				t.Fatal(err)
			}
			name, password, want := "failed-local", localPassword, 401
			switch failure {
			case "wrong-password":
				password += "wrong"
			case "inactive":
				want = 403
				if _, err := s.pool.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				name = "absent-local"
			case "issuance-failure":
				want = 500
				if _, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_session_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture rejects session'; END $$;
     CREATE TRIGGER reject_session_fixture BEFORE INSERT ON browser_sessions FOR EACH ROW EXECUTE FUNCTION reject_session_fixture()`); err != nil {
					t.Fatal(err)
				}
			}
			res, raw := s.request("POST", "/api/auth/login", fmt.Sprintf(`{"login_name":%q,"password":%q}`, name, password), nativeHeaders("", ""))
			if res.StatusCode != want || len(res.Cookies()) != 0 {
				t.Fatalf("failed login=%d %s cookies=%v", res.StatusCode, raw, res.Cookies())
			}
			var sessions int
			if err := s.pool.QueryRow(ctx, `SELECT last_seen_at,(SELECT count(*) FROM browser_sessions) FROM principals WHERE id=$1`, id).Scan(&seen, &sessions); err != nil || !seen.Equal(original) || sessions != 0 {
				t.Fatalf("failed login forged activity: seen=%v original=%v sessions=%d err=%v", seen, original, sessions, err)
			}
		})
	}
}
