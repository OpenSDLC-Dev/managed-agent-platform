package admin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Leave the application pool available to issuers and withdrawals. Independent
// connections observe the real PostgreSQL blocker chain without supplying it.
func grantFenceConnection(t *testing.T, ctx context.Context, f *grantFixture) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, f.e.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close grant fence connection: %v", err)
		}
	})
	return conn
}

func waitGrantWrite(t *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID int, call *grantRequest) int {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := observer.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
			AND query ILIKE '%INSERT INTO modelgateway.key_policies%' AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blockerPID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			t.Logf("grant UPSERT backend %d is blocked by target-policy holder %d", pid, blockerPID)
			return pid
		}
		select {
		case <-call.done:
			t.Fatalf("grant ended before its real blocked UPSERT: status=%d body=%s", call.rr.Code, call.rr.Body.String())
		case <-ctx.Done():
			t.Fatal("did not observe the grant UPSERT blocked by the target policy holder")
		case <-tick.C:
		}
	}
}

func observeGrantWithdrawalFence(t *testing.T, ctx context.Context, observer *pgx.Conn, issuerPID, withdrawalPID int, statement string, done <-chan struct{}) {
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
			t.Logf("withdrawal backend %d running %q is blocked by actual grant issuer %d", pid, statement, issuerPID)
			return
		}
		select {
		case <-done:
			t.Errorf("withdrawal %q overtook the blocked grant write on backend %d", statement, issuerPID)
			return
		case <-ctx.Done():
			t.Fatalf("did not observe withdrawal %q blocked by grant issuer %d", statement, issuerPID)
		case <-tick.C:
		}
	}
}

// A resample outside the grant transaction cannot fence any withdrawal. A
// session-only lock must not pass the principal demotion/deactivation controls.
func TestGatewayCredentialMutationCommitFence(t *testing.T) {
	for _, withdrawal := range []string{"logout", "demote", "deactivate"} {
		t.Run(withdrawal, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			f := newGrantFixture(t, ctx)
			target := prepareGrantTarget(t, ctx, f, "limited")
			blockerConn := grantFenceConnection(t, ctx, f)
			observer := grantFenceConnection(t, ctx, f)
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
			// Lock only the target grant, never the caller or its browser session.
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM modelgateway.key_policies WHERE api_key_id=$1 FOR UPDATE`, target.id).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			body := newGrantBody(ctx)
			headers := cookieAdminHeaders(f.issued)
			issuer := startGrantRequest(t, ctx, f.handler, "POST", "/admin/v1/key_policies/"+target.id, body, headers)
			issuer.await(t, body.entered, "partial body after admin admission")
			body.unblock()
			issuer.await(t, body.eof, "complete valid grant body")
			issuerPID := waitGrantWrite(t, ctx, observer, blockerPID, issuer)

			var logout *grantRequest
			var withdrawalDone chan struct{}
			var withdrawalErr error
			var withdrawalRows int64
			if withdrawal == "logout" {
				logout = startGrantRequest(t, ctx, f.logout, "POST", "/api/auth/logout", nil, headers)
				observeGrantWithdrawalFence(t, ctx, observer, issuerPID, 0, "%DELETE FROM browser_sessions%", logout.done)
			} else {
				conn := grantFenceConnection(t, ctx, f)
				withdrawalPID := int(conn.PgConn().PID())
				query := map[string]string{
					"demote":     `UPDATE principals SET role='developer' WHERE id=$1`,
					"deactivate": `UPDATE principals SET status='deactivated' WHERE id=$1`,
				}[withdrawal]
				withdrawalCtx, withdrawalCancel := context.WithCancel(ctx)
				withdrawalDone = make(chan struct{})
				go func() {
					defer close(withdrawalDone)
					tag, err := conn.Exec(withdrawalCtx, query, f.caller)
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
				observeGrantWithdrawalFence(t, ctx, observer, issuerPID, withdrawalPID, query, withdrawalDone)
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			rr := issuer.result(t)
			if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
				t.Errorf("legitimate grant-before-%s failed: status=%d body=%s", withdrawal, rr.Code, rr.Body.String())
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
			if err := observer.QueryRow(ctx, `SELECT role,status,(SELECT count(*) FROM browser_sessions WHERE principal_id=$1) FROM principals WHERE id=$1`, f.caller).Scan(&role, &status, &sessions); err != nil || role != wantRole || status != wantStatus || sessions != wantSessions {
				t.Errorf("committed caller state: role=%q status=%q sessions=%d; want %q/%q/%d: %v", role, status, sessions, wantRole, wantStatus, wantSessions, err)
			}
			target.assertState(t, ctx, f, rr, true)
		})
	}
}
