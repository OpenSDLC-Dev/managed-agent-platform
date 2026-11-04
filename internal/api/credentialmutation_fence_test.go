package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The blocker and observer do not consume the application's pool. A missing
// fence must be an observed logout, not exhaustion of the pool's connections.
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

func waitCredentialWrite(t *testing.T, ctx context.Context, observer *pgx.Conn, blockerPID int, call *credentialRequest) int {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int
		if err := observer.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			AND query ILIKE '%INSERT INTO api_keys%' AND $1=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blockerPID).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			t.Logf("management-key INSERT backend %d is blocked by target-table holder %d", pid, blockerPID)
			return pid
		}
		select {
		case <-call.done:
			t.Fatalf("issuance ended before its real blocked INSERT: status=%d body=%s", call.rr.Code, call.rr.Body.String())
		case <-ctx.Done():
			t.Fatal("did not observe the management-key INSERT blocked by the target holder")
		case <-tick.C:
		}
	}
}

func observeCredentialLogoutFence(t *testing.T, ctx context.Context, observer *pgx.Conn, issuerPID int, logout *credentialRequest) {
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
			t.Logf("logout DELETE is blocked by actual credential issuer backend %d", issuerPID)
			return
		}
		select {
		case <-logout.done:
			t.Errorf("logout overtook the blocked credential write: status=%d body=%s", logout.rr.Code, logout.rr.Body.String())
			return
		case <-ctx.Done():
			t.Fatal("did not observe logout blocked by the credential issuer")
		case <-tick.C:
		}
	}
}

// A post-body read-only resample still loses authority before INSERT commits.
// Retaining the principal/session guard in that write's transaction fences logout.
func TestNativeCredentialMutationCommitFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	s, h := newCredentialHandler(t)
	caller := seedMember(t, s, "fenced-issuer", "admin", "console")
	cookie, session := s.issue(caller)
	headers := nativeHeaders(cookie, session.CSRFToken)
	target := prepareCredentialTarget(t, ctx, s, h, caller, "management-issue")
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
	// Issuance has no target row yet. SHARE locks its actual credential table,
	// blocking INSERT's RowExclusiveLock while allowing authentication reads.
	if _, err := blocker.Exec(ctx, `LOCK TABLE api_keys IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	body := newCredentialBody(ctx, target.body)
	issuer := startCredentialRequest(t, ctx, h, "POST", target.path, body, headers)
	issuer.await(t, body.entered, "partial body after admin admission")
	body.unblock()
	issuer.await(t, body.eof, "complete valid issuance body")
	issuerPID := waitCredentialWrite(t, ctx, observer, blockerPID, issuer)
	logout := startCredentialRequest(t, ctx, h, "POST", "/api/auth/logout", nil, headers)
	observeCredentialLogoutFence(t, ctx, observer, issuerPID, logout)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rr := issuer.result(t)
	if rr.Code != 200 || rr.Header().Get("x-platform-signed-out") != "" {
		t.Errorf("legitimate write-before-logout failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	loggedOut := logout.result(t)
	if loggedOut.Code != 200 || strings.TrimSpace(loggedOut.Body.String()) != `{"ok":true}` {
		t.Errorf("fenced logout failed: status=%d body=%s", loggedOut.Code, loggedOut.Body.String())
	}
	var sessions int
	if err := observer.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE principal_id=$1`, caller).Scan(&sessions); err != nil || sessions != 0 {
		t.Errorf("fenced logout left %d browser sessions: %v", sessions, err)
	}
	target.assertState(t, ctx, s, h, rr, true, caller)
}
