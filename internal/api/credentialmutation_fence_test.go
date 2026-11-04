package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Blockers, observers and SQL withdrawals leave the application's pool free.
// A missing fence must be an observed withdrawal, not pool exhaustion.
func credentialFenceConnection(t *testing.T, ctx context.Context, s *nativeServer) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close credential fence connection: %v", err)
		}
	})
	return conn
}

func waitCredentialWrite(t *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID int, statement string, call *credentialRequest) int {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := observer.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
			AND query ILIKE $2 AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blockerPID, statement).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			t.Logf("credential backend %d running %q is blocked by target holder %d", pid, statement, blockerPID)
			return pid
		}
		select {
		case <-call.done:
			t.Fatalf("mutation ended before its real blocked SQL %q: status=%d body=%s", statement, call.rr.Code, call.rr.Body.String())
		case <-ctx.Done():
			t.Fatalf("did not observe credential SQL %q blocked by target holder %d", statement, blockerPID)
		case <-tick.C:
		}
	}
}

func observeCredentialWithdrawalFence(t *testing.T, ctx context.Context, observer *pgx.Conn, issuerPID, withdrawalPID int, statement string, done <-chan struct{}) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := observer.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
			AND ($2=0 OR pid=$2) AND query ILIKE $3 AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, issuerPID, withdrawalPID, statement).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			t.Logf("withdrawal backend %d running %q is blocked by actual credential issuer %d", pid, statement, issuerPID)
			return
		}
		select {
		case <-done:
			t.Errorf("withdrawal %q overtook the blocked credential write on backend %d", statement, issuerPID)
			return
		case <-ctx.Done():
			t.Fatalf("did not observe withdrawal %q blocked by credential issuer %d", statement, issuerPID)
		case <-tick.C:
		}
	}
}

// A post-body resample outside the write transaction cannot fence withdrawals.
// Session-only locking cannot protect against demotion or deactivation.
func TestNativeCredentialMutationCommitFence(t *testing.T) {
	for _, operation := range []string{"management-issue", "reactivate-org", "reactivate-workspace", "worker-issue"} {
		for _, withdrawal := range []string{"logout", "demote", "deactivate"} {
			t.Run(operation+"/"+withdrawal, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				s, h := newCredentialHandler(t)
				caller := seedMember(t, s, "fenced-issuer", "admin", "console")
				cookie, session := s.issue(caller)
				headers := nativeHeaders(cookie, session.CSRFToken)
				target := prepareCredentialTarget(t, ctx, s, h, caller, operation)
				blockerConn := credentialFenceConnection(t, ctx, s)
				observer := credentialFenceConnection(t, ctx, s)
				blocker, err := blockerConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					blocker.Rollback(ctx)
				})
				var blockerPID int
				statement := "%INSERT INTO api_keys%"
				switch operation {
				case "management-issue":
					// No target row exists yet; block the actual INSERT table.
					if _, err := blocker.Exec(ctx, `LOCK TABLE api_keys IN SHARE MODE`); err != nil {
						t.Fatal(err)
					}
					if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
						t.Fatal(err)
					}
				case "reactivate-org", "reactivate-workspace":
					// The route first locks this valid inactive target FOR UPDATE.
					statement = "%FROM api_keys%FOR UPDATE%"
					if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM api_keys WHERE id=$1 FOR UPDATE`, target.keyID).Scan(&blockerPID); err != nil {
						t.Fatal(err)
					}
				case "worker-issue":
					// The issuance transaction reads this environment FOR SHARE.
					statement = "%FROM environments%FOR SHARE%"
					if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM environments WHERE id=$1 FOR UPDATE`, target.envID).Scan(&blockerPID); err != nil {
						t.Fatal(err)
					}
				}
				// Only the credential target is held here, never the caller or session.
				body := newCredentialBody(ctx, target.body)
				issuer := startCredentialRequest(t, ctx, h, "POST", target.path, body, headers)
				issuer.await(t, body.entered, "partial body after admin admission")
				body.unblock()
				issuer.await(t, body.eof, "complete valid mutation body")
				issuerPID := waitCredentialWrite(t, ctx, observer, blockerPID, statement, issuer)

				var logout *credentialRequest
				var withdrawalDone chan struct{}
				var withdrawalErr error
				var withdrawalRows int64
				if withdrawal == "logout" {
					logout = startCredentialRequest(t, ctx, h, "POST", "/api/auth/logout", nil, headers)
					observeCredentialWithdrawalFence(t, ctx, observer, issuerPID, 0, "%DELETE FROM browser_sessions%", logout.done)
				} else {
					conn := credentialFenceConnection(t, ctx, s)
					withdrawalPID := int(conn.PgConn().PID())
					query := map[string]string{
						"demote":     `UPDATE principals SET role='developer' WHERE id=$1`,
						"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
					}[withdrawal]
					withdrawalCtx, withdrawalCancel := context.WithCancel(ctx)
					withdrawalDone = make(chan struct{})
					go func() {
						defer close(withdrawalDone)
						tag, err := conn.Exec(withdrawalCtx, query, caller)
						withdrawalRows, withdrawalErr = tag.RowsAffected(), err
					}()
					t.Cleanup(func() {
						withdrawalCancel()
						select {
						case <-withdrawalDone:
						case <-time.After(5 * time.Second):
							t.Error("principal withdrawal did not stop after cancellation")
						}
					})
					observeCredentialWithdrawalFence(t, ctx, observer, issuerPID, withdrawalPID, query, withdrawalDone)
				}
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				rr := issuer.result(t)
				if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
					t.Errorf("legitimate write-before-%s failed: status=%d body=%s", withdrawal, rr.Code, rr.Body.String())
				}
				if logout != nil {
					loggedOut := logout.result(t)
					if loggedOut.Code != 200 || strings.TrimSpace(loggedOut.Body.String()) != `{"ok":true}` {
						t.Errorf("fenced logout failed: status=%d body=%s", loggedOut.Code, loggedOut.Body.String())
					}
				} else {
					select {
					case <-withdrawalDone:
						if withdrawalErr != nil || withdrawalRows != 1 {
							t.Errorf("fenced %s affected %d rows: %v", withdrawal, withdrawalRows, withdrawalErr)
						}
					case <-ctx.Done():
						t.Fatalf("fenced %s did not finish: %v", withdrawal, ctx.Err())
					}
				}
				wantRole, wantStatus, wantSessions := "admin", "active", 1
				switch withdrawal {
				case "logout":
					wantSessions = 0
				case "demote":
					wantRole = "developer"
				case "deactivate":
					wantStatus = "deactivated"
				}
				var role, status string
				var sessions int
				if err := observer.QueryRow(ctx, `SELECT role,status,(SELECT count(*) FROM browser_sessions WHERE principal_id=$1) FROM principals WHERE id=$1`, caller).Scan(&role, &status, &sessions); err != nil || role != wantRole || status != wantStatus || sessions != wantSessions {
					t.Errorf("committed caller state: role=%q status=%q sessions=%d; want %q/%q/%d: %v", role, status, sessions, wantRole, wantStatus, wantSessions, err)
				}
				target.assertState(t, ctx, s, h, rr, true, caller)
			})
		}
	}
}
