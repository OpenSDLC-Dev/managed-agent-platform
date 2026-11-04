package api_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
)

func TestNativeSetupRechecksSessionAfterLocks(t *testing.T) {
	for _, withdrawal := range []string{"valid-control", "logout", "table-expiry", "session-row-expiry", "table-code-expiry", "session-row-code-expiry"} {
		t.Run(withdrawal, func(t *testing.T) {
			sessionRowWait := withdrawal == "session-row-expiry" || withdrawal == "session-row-code-expiry"
			codeExpiry := withdrawal == "table-code-expiry" || withdrawal == "session-row-code-expiry"
			s := newNativeServer(t, login.ModeOIDC)
			id := seedMember(t, s, "setup-candidate", "user", "console")
			cookie, session := s.issue(id)
			code, err := api.MintSetupCode(context.Background(), s.pool, false)
			if err != nil || code == "" {
				t.Fatalf("mint=%q %v", code, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if withdrawal == "table-expiry" || withdrawal == "session-row-expiry" {
				if _, err := s.pool.Exec(ctx, `UPDATE browser_sessions SET expires_at=clock_timestamp()+interval '2 seconds' WHERE principal_id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if codeExpiry {
				if _, err := s.pool.Exec(ctx, `UPDATE setup_codes SET expires_at=clock_timestamp()+interval '2 seconds' WHERE purpose='identity'`); err != nil {
					t.Fatal(err)
				}
			}
			blocker, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			var pid int
			if sessionRowWait {
				if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid() FROM browser_sessions WHERE principal_id=$1 FOR UPDATE`, id).Scan(&pid); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := blocker.Exec(ctx, `LOCK TABLE principals IN SHARE ROW EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
			}
			done := managementAsync(ctx, s, "POST", setupPath, fmt.Sprintf(`{"code":%q}`, code), nativeHeaders(cookie, session.CSRFToken))
			var early *managementReply
			if !sessionRowWait {
				observeManagementWait(t, s, ctx, 1, done)
				t.Log("observed principal-table lock wait")
			} else {
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
			observe:
				for {
					var waiting bool
					if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND position('FOR SHARE OF s' in query)>0 AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						t.Log("observed session-row lock wait")
						break
					}
					select {
					case reply := <-done:
						early = &reply
						t.Logf("setup finished without locking browser session: %d %s", reply.status, reply.raw)
						break observe
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-tick.C:
					}
				}
			}
			if codeExpiry {
				if early != nil {
					t.Fatal("setup finished before observed code-expiry lock wait")
				}
				var valid bool
				if err := s.pool.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM setup_codes WHERE purpose='identity'`).Scan(&valid); err != nil || !valid {
					t.Fatalf("setup code valid at lock wait=%v err=%v", valid, err)
				}
				if _, err := blocker.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(epoch FROM (expires_at-clock_timestamp())))+0.02) FROM setup_codes WHERE purpose='identity'`); err != nil {
					t.Fatal(err)
				}
				var expired, sessionValid bool
				if err := s.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp(),
					EXISTS(SELECT 1 FROM browser_sessions WHERE principal_id=$1 AND expires_at>clock_timestamp())
					FROM setup_codes WHERE purpose='identity'`, id).Scan(&expired, &sessionValid); err != nil || !expired || !sessionValid {
					t.Fatalf("setup code expired=%v browser session valid=%v err=%v", expired, sessionValid, err)
				}
				t.Log("confirmed setup code expired while browser session remains valid")
			}
			if withdrawal == "logout" {
				res, raw := s.request("POST", "/api/auth/logout", "", nativeHeaders(cookie, session.CSRFToken))
				if res.StatusCode != 200 {
					t.Fatalf("separate logout=%d %s", res.StatusCode, raw)
				}
			}
			if withdrawal == "table-expiry" || withdrawal == "session-row-expiry" {
				if _, err := blocker.Exec(ctx, `SELECT pg_sleep(GREATEST(0,EXTRACT(epoch FROM (expires_at-clock_timestamp())))+0.02) FROM browser_sessions WHERE principal_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				var expired bool
				if err := s.pool.QueryRow(ctx, `SELECT expires_at<=clock_timestamp() FROM browser_sessions WHERE principal_id=$1`, id).Scan(&expired); err != nil || !expired {
					t.Fatalf("database expiry=%v err=%v", expired, err)
				}
			}
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var reply managementReply
			if early != nil {
				reply = *early
			} else {
				reply = <-done
			}
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			var role string
			var codes int
			if err := s.pool.QueryRow(ctx, `SELECT role,(SELECT count(*) FROM setup_codes) FROM principals WHERE id=$1`, id).Scan(&role, &codes); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantRole, wantCodes, wantMarker := 401, "user", 1, "1"
			if withdrawal == "valid-control" {
				wantStatus, wantRole, wantCodes, wantMarker = 200, "admin", 0, ""
			} else if codeExpiry {
				wantStatus, wantRole, wantCodes, wantMarker = 400, "user", 1, ""
			}
			if reply.status != wantStatus || reply.header.Get("x-platform-signed-out") != wantMarker || role != wantRole || codes != wantCodes {
				t.Fatalf("post-lock setup=%d marker=%q role=%s codes=%d body=%s; want %d/%s/%d/%q", reply.status, reply.header.Get("x-platform-signed-out"), role, codes, reply.raw, wantStatus, wantRole, wantCodes, wantMarker)
			}
		})
	}
}
