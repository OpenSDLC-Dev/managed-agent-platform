package admin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Leave all application pool connections available to the issuer and logout.
// An independently connected observer proves the real PostgreSQL blocker chain.
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
			WHERE datname=current_database() AND wait_event_type='Lock'
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

func observeGrantLogoutFence(t *testing.T, ctx context.Context, observer *pgx.Conn, issuerPID int, logout *grantRequest) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := observer.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query ILIKE '%DELETE FROM browser_sessions%' AND $1=ANY(pg_blocking_pids(pid)))`, issuerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			t.Logf("logout DELETE is blocked by actual grant issuer backend %d", issuerPID)
			return
		}
		select {
		case <-logout.done:
			t.Errorf("logout overtook the blocked grant write: status=%d body=%s", logout.rr.Code, logout.rr.Body.String())
			return
		case <-ctx.Done():
			t.Fatal("did not observe logout blocked by the grant issuer")
		case <-tick.C:
		}
	}
}

// A post-body role/session resample outside the grant transaction is not a
// fence: logout must wait for this already-authorized blocked UPSERT to commit.
func TestGatewayCredentialMutationCommitFence(t *testing.T) {
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
	// Lock only the target grant. Taking a caller/session lock here would
	// manufacture the very authority fence the handler must supply itself.
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
	logout := startGrantRequest(t, ctx, f.logout, "POST", "/api/auth/logout", nil, headers)
	observeGrantLogoutFence(t, ctx, observer, issuerPID, logout)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rr := issuer.result(t)
	if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
		t.Errorf("legitimate grant-before-logout failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	loggedOut := logout.result(t)
	if loggedOut.Code != 200 || strings.TrimSpace(loggedOut.Body.String()) != `{"ok":true}` {
		t.Errorf("fenced logout failed: status=%d body=%s", loggedOut.Code, loggedOut.Body.String())
	}
	var sessions int
	if err := observer.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE principal_id=$1`, f.caller).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("fenced logout left %d browser sessions: %v", sessions, err)
	}
	target.assertState(t, ctx, f, rr, true)
}
