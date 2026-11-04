package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/jackc/pgx/v5/pgconn"
)

// A principal mutation's real table lock must stop password issuance before it
// locks expired sessions. This pins lock order without contriving a deadlock.
func TestPasswordLoginPrincipalBeforeExpiredSessionCleanup(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	id := s.seedLocal("lock-order", "developer", "active")
	other := seedMember(t, s, "unrelated-live", "user", "console")
	live, err := s.manager.Issue(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const expiredHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := s.pool.Exec(ctx, `INSERT INTO browser_sessions(token_hash,principal_id,created_at,expires_at)
		VALUES ($1,$2,clock_timestamp()-interval '2 hours',clock_timestamp()-interval '1 hour')`, expiredHash, id); err != nil {
		t.Fatal(err)
	}
	var originalSeen, before time.Time
	if err := s.pool.QueryRow(ctx, `UPDATE principals SET last_seen_at=clock_timestamp()-interval '181 days' WHERE id=$1 RETURNING last_seen_at,clock_timestamp()`, id).Scan(&originalSeen, &before); err != nil {
		t.Fatal(err)
	}
	var liveCreated, liveExpires time.Time
	if err := s.pool.QueryRow(ctx, `SELECT created_at,expires_at FROM browser_sessions WHERE principal_id=$1`, other).Scan(&liveCreated, &liveExpires); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	blockerPID := int32(blocker.Conn().PgConn().PID())
	type result struct {
		issued login.IssuedSession
		err    error
	}
	done := make(chan result, 1)
	go func() {
		issued, err := s.manager.IssuePasswordLogin(ctx, id)
		done <- result{issued, err}
	}()
	var reply result
	joined := false
	defer func() {
		// On every early exit, release the barrier and join the bounded issuer.
		blocker.Rollback(context.Background())
		cancel()
		if !joined {
			<-done
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var waitingPID int32
	var observeErr error
	for waitingPID == 0 && observeErr == nil && !joined {
		observeErr = s.pool.QueryRow(ctx, `SELECT coalesce((SELECT pid FROM pg_locks
			WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND relation='principals'::regclass AND mode='RowExclusiveLock' AND NOT granted
			AND $1::integer=ANY(pg_blocking_pids(pid)) LIMIT 1),0)`, blockerPID).Scan(&waitingPID)
		if waitingPID != 0 || observeErr != nil {
			break
		}
		select {
		case reply = <-done:
			joined = true
			observeErr = fmt.Errorf("issuance finished before its principal-lock wait: %v", reply.err)
		case <-ctx.Done():
			observeErr = ctx.Err()
		case <-ticker.C:
		}
	}
	var rowErr, probeReleaseErr error
	if observeErr == nil {
		probe, err := s.pool.Begin(ctx)
		if err != nil {
			rowErr = err
		} else {
			var locked string
			rowErr = probe.QueryRow(ctx, `SELECT token_hash FROM browser_sessions WHERE token_hash=$1 FOR UPDATE NOWAIT`, expiredHash).Scan(&locked)
			probeReleaseErr = probe.Rollback(context.Background())
		}
	}
	// C must release its row lock before A releases the principal barrier.
	releaseErr := blocker.Commit(ctx)
	if releaseErr != nil {
		blocker.Rollback(context.Background())
		cancel()
	}
	if !joined {
		reply = <-done
		joined = true
	}
	if observeErr != nil {
		t.Fatalf("observe principal wait: %v; issuer=%v", observeErr, reply.err)
	}
	t.Logf("issuer pid=%d waited for principals RowExclusiveLock, blocked by pid=%d", waitingPID, blockerPID)
	if rowErr != nil {
		var pgErr *pgconn.PgError
		if errors.As(rowErr, &pgErr) && pgErr.Code == "55P03" {
			t.Errorf("expired session was already locked while password issuance waited for principals: SQLSTATE %s", pgErr.Code)
		} else {
			t.Errorf("lock expired session while issuance waits: %v", rowErr)
		}
	}
	if probeReleaseErr != nil || releaseErr != nil {
		t.Fatalf("release probe=%v principal barrier=%v", probeReleaseErr, releaseErr)
	}
	if reply.err != nil || reply.issued.Token == "" {
		t.Fatalf("issuance after release: %v", reply.err)
	}
	request := httptest.NewRequest("GET", nativeOrigin+"/api/auth/session", nil)
	request.Header.Set("Cookie", "map_session="+reply.issued.Token)
	session, err := s.manager.Authenticate(ctx, request)
	if err != nil || session.PrincipalID != id {
		t.Errorf("new session is unusable: principal=%q err=%v", session.PrincipalID, err)
	}
	request.Header.Set("Cookie", "map_session="+live.Token)
	if session, err := s.manager.Authenticate(ctx, request); err != nil || session.PrincipalID != other {
		t.Errorf("unrelated live session is unusable: principal=%q err=%v", session.PrincipalID, err)
	}
	var expired, total int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM browser_sessions WHERE token_hash=$1),(SELECT count(*) FROM browser_sessions)`, expiredHash).Scan(&expired, &total); err != nil || expired != 0 || total != 2 {
		t.Errorf("session cleanup: expired=%d total=%d err=%v", expired, total, err)
	}
	var created, expires, after, seen time.Time
	if err := s.pool.QueryRow(ctx, `SELECT created_at,expires_at FROM browser_sessions WHERE principal_id=$1`, other).Scan(&created, &expires); err != nil || !created.Equal(liveCreated) || !expires.Equal(liveExpires) {
		t.Errorf("unrelated session changed: created=%v expires=%v err=%v", created, expires, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT clock_timestamp(),last_seen_at FROM principals WHERE id=$1`, id).Scan(&after, &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Equal(originalSeen) || seen.Before(before) || seen.After(after) {
		t.Errorf("last_seen=%v outside successful database-time bounds [%v,%v] (original %v)", seen, before, after, originalSeen)
	}
}

// Unlike the OIDC/proxy cases, password login writes the principal first.
// Deactivation must happen inside that blocking transaction, not behind it.
func testPasswordIssuanceDeactivationAtPrincipalBoundary(t *testing.T) {
	s := newNativeServer(t, login.ModePassword)
	id := s.seedLocal("raced", "user", "active")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var original time.Time
	if err := s.pool.QueryRow(ctx, `SELECT last_seen_at FROM principals WHERE id=$1`, id).Scan(&original); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	blockerPID := int32(blocker.Conn().PgConn().PID())
	type result struct {
		res *http.Response
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		body := fmt.Sprintf(`{"login_name":"raced","password":%q}`, localPassword)
		request, err := http.NewRequestWithContext(ctx, "POST", s.url+"/api/auth/login", strings.NewReader(body))
		if err != nil {
			done <- result{err: err}
			return
		}
		request.Header = nativeHeaders("", "")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- result{err: err}
			return
		}
		raw, err := io.ReadAll(response.Body)
		response.Body.Close()
		done <- result{response, raw, err}
	}()
	var reply result
	joined := false
	defer func() {
		blocker.Rollback(context.Background())
		if !joined {
			<-done
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	waiting := false
	var observeErr error
	for !waiting && observeErr == nil && !joined {
		observeErr = s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE database=(SELECT oid FROM pg_database WHERE datname=current_database())
			AND relation='principals'::regclass AND mode='RowExclusiveLock' AND NOT granted
			AND $1::integer=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
		if waiting || observeErr != nil {
			break
		}
		select {
		case reply = <-done:
			joined = true
			observeErr = fmt.Errorf("login finished before its principal-lock wait: %v response=%v", reply.err, reply.res)
		case <-ctx.Done():
			observeErr = ctx.Err()
		case <-ticker.C:
		}
	}
	var changed int64
	var deactivateErr error
	if observeErr == nil {
		tag, err := blocker.Exec(ctx, `UPDATE principals SET status='deactivated' WHERE id=$1`, id)
		changed, deactivateErr = tag.RowsAffected(), err
	}
	releaseErr := blocker.Commit(ctx)
	if releaseErr != nil {
		blocker.Rollback(context.Background())
	}
	if !joined {
		reply = <-done
		joined = true
	}
	if observeErr != nil || deactivateErr != nil || releaseErr != nil || changed != 1 {
		t.Fatalf("principal barrier: observe=%v deactivate=%v changed=%d release=%v; reply=%+v", observeErr, deactivateErr, changed, releaseErr, reply)
	}
	if reply.err != nil || reply.res == nil {
		t.Fatalf("deactivated issuance=%+v", reply)
	}
	if reply.res.StatusCode != 401 || reply.res.Header.Get("x-platform-signed-out") != "1" {
		t.Errorf("issuance race status=%d marker=%q body=%s", reply.res.StatusCode, reply.res.Header.Get("x-platform-signed-out"), reply.raw)
	}
	for _, cookie := range reply.res.Cookies() {
		if cookie.Name == "map_session" {
			t.Error("raced issuance set a session cookie")
		}
	}
	var sessions int
	var seen time.Time
	if err := s.pool.QueryRow(ctx, `SELECT last_seen_at,(SELECT count(*) FROM browser_sessions) FROM principals WHERE id=$1`, id).Scan(&seen, &sessions); err != nil || sessions != 0 || !seen.Equal(original) {
		t.Fatalf("deactivated issuance wrote activity/session: seen=%v original=%v sessions=%d err=%v", seen, original, sessions, err)
	}
}
