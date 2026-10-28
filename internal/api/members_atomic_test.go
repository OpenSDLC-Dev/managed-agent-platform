package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

// A credential-store failure must roll back the principal insert, not reserve
// a login name with no usable password. The failure is a real PostgreSQL trigger.
func TestLocalMemberCredentialFailureRollsBack(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	if _, err := s.pool.Exec(context.Background(), `CREATE FUNCTION reject_local_credential() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture credential write failed'; END $$;
		CREATE TRIGGER reject_local_credential BEFORE INSERT ON local_accounts FOR EACH ROW EXECUTE FUNCTION reject_local_credential()`); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"login_name":"atomic","password":%q,"role":"user"}`, localPassword)
	expectManagement(t, s, "POST", membersPath, body, h, 500)
	var count int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM principals WHERE issuer=$1 AND subject='atomic'`, login.LocalIssuer).Scan(&count); err != nil || count != 0 {
		t.Fatalf("credential failure left %d principal rows: %v", count, err)
	}
	if _, err := s.pool.Exec(context.Background(), `DROP TRIGGER reject_local_credential ON local_accounts`); err != nil {
		t.Fatal(err)
	}
	expectManagement(t, s, "POST", membersPath, body, h, 200)
}

// Invalid bodies must be rejected without waiting for the principal table.
func TestMembersValidationBeforeLocks(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	admin := seedMember(t, s, "admin", "admin", "console")
	cookie, session := s.issue(admin)
	h := nativeHeaders(cookie, session.CSRFToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lockPrincipalMutations(t, s, ctx)
	for _, tc := range []struct{ path, body string }{
		{membersPath, `{"login_name":"invalid","password":"short","role":"user"}`},
		{membersPath + "/" + principal.PublicID(admin, "user"), `{"role":"viewer"}`},
		{serviceAccountsPath, `{"name":"bad name"}`},
		{serviceAccountsPath + "/" + principal.PublicID(admin, "service_account"), `{"organization_role":"user"}`},
	} {
		reply := <-managementAsync(ctx, s, "POST", tc.path, tc.body, h)
		if reply.err != nil || reply.status != 400 {
			t.Fatalf("invalid request waited on lock: %+v body=%s", reply, reply.raw)
		}
	}
}
