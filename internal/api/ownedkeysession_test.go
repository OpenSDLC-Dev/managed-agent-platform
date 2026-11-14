package api_test

import (
	"context"
	"testing"
	"time"
)

// The session SELECT can evaluate expiry before waiting for its row lock. A
// lock-only holder must not let that stale result authorize a credential write.
func TestOwnedKeySessionExpiryAfterWait(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "live-control"
		if expired {
			name = "expires-during-session-wait"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			s, h, caller, headers := ownedMutationCaller(t, "cookie", "developer")
			before := ownedSnapshot(t, s)
			body := newCredentialBody(ctx, `{"name":"session-wait"}`)
			call := startCredentialRequest(t, ctx, h, "POST", consoleAPIKeysPath, body, headers)
			call.await(t, body.entered, "body before session lock")
			blockerConn, observer := credentialFenceConnection(t, ctx, s), credentialFenceConnection(t, ctx, s)
			var deadline time.Time
			if err := observer.QueryRow(ctx, `SELECT clock_timestamp()+interval '3 seconds'`).Scan(&deadline); err != nil {
				t.Fatal(err)
			}
			if expired {
				if _, err := observer.Exec(ctx, `UPDATE browser_sessions SET created_at=$2::timestamptz-interval '12 hours',expires_at=$2 WHERE principal_id=$1`, caller, deadline); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := blockerConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT token_hash FROM browser_sessions WHERE principal_id=$1 FOR UPDATE`, caller); err != nil {
				t.Fatal(err)
			}
			body.unblock()
			pid := waitCredentialWrite(t, ctx, observer, int(blockerConn.PgConn().PID()), "%browser_sessions%FOR SHARE%", call)
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var at time.Time
				var blocked bool
				if err := observer.QueryRow(ctx, `SELECT clock_timestamp(),$1=ANY(pg_blocking_pids($2))`, int(blockerConn.PgConn().PID()), pid).Scan(&at, &blocked); err != nil {
					t.Fatal(err)
				}
				if !blocked {
					t.Fatal("session lock wait ended before clock crossing")
				}
				if !at.Before(deadline) {
					t.Logf("SESSION_EXPIRY_LOCK_PROOF issuer=%d blocker=%d deadline=%s crossed=%s", pid, blockerConn.PgConn().PID(), deadline.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano))
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-tick.C:
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			rr := call.result(t)
			want, marker := 200, ""
			if expired {
				want, marker = 401, "1"
			}
			if rr.Code != want || rr.Header().Get("x-platform-signed-out") != marker {
				t.Errorf("session-wait status=%d marker=%q want=%d/%q", rr.Code, rr.Header().Get("x-platform-signed-out"), want, marker)
			}
			if expired && before != ownedSnapshot(t, s) {
				t.Error("expired session left a durable credential")
			}
		})
	}
}

// Invalid update bodies cannot bypass the post-upload authority check, and the
// selected caller must not learn whether a formerly visible row still exists.
func TestOwnedKeyBadBodyRechecksAuthority(t *testing.T) {
	for _, path := range []func(string) string{consoleOrgAPIKey, consoleAPIKey} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		s, h, caller, headers := ownedMutationCaller(t, "cookie", "developer")
		_, key := issueOwnedHTTP(t, s, headers, "bad body authority", nil)
		before := ownedSnapshot(t, s)
		body := newCredentialBody(ctx, `{"unknown":1}`)
		call := startCredentialRequest(t, ctx, h, "POST", path(key), body, headers)
		call.await(t, body.entered, "admitted invalid-body upload")
		if _, err := s.pool.Exec(ctx, `UPDATE principals SET role='user' WHERE id=$1`, caller); err != nil {
			t.Fatal(err)
		}
		body.unblock()
		rr := call.result(t)
		if rr.Code != 403 {
			t.Errorf("invalid body disclosed after role withdrawal: %d", rr.Code)
		}
		if before != ownedSnapshot(t, s) {
			t.Error("invalid body changed key metadata")
		}
		cancel()
	}
}
