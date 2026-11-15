package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestModelLimitSessionExpiryAfterWait(t *testing.T) {
	for _, lock := range []string{"advisory", "row"} {
		for _, expired := range []bool{false, true} {
			name := lock + "/live-control"
			if expired {
				name = lock + "/expires-during-wait"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				f := newGrantFixture(t, ctx)
				if _, err := f.e.pool.Exec(ctx, `INSERT INTO modelgateway.model_limits (model,rpm) VALUES ('fast',5)`); err != nil {
					t.Fatal(err)
				}
				body := newGrantBody(ctx)
				body.prefix = strings.NewReader(`{"requests_per_minute":null,"input_tokens_per_minute":null,"output_tokens_per_minute":null`)
				call := startGrantRequest(t, ctx, f.handler, "POST", "/admin/v1/rate_limits/fast", body, cookieAdminHeaders(f.issued))
				call.await(t, body.entered, "body before model limit lock")
				conn, observer := grantFenceConnection(t, ctx, f), grantFenceConnection(t, ctx, f)
				blocker, err := conn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				query, waiting := `SELECT pg_advisory_xact_lock(hashtextextended('modelgateway.model_limit/fast',69))`, "%pg_advisory_xact_lock%"
				if lock == "row" {
					query, waiting = `SELECT model FROM modelgateway.model_limits WHERE model='fast' FOR UPDATE`, "%INSERT INTO modelgateway.model_limits%"
				}
				if _, err := blocker.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
				var deadline time.Time
				if err := observer.QueryRow(ctx, `SELECT clock_timestamp()+interval '3 seconds'`).Scan(&deadline); err != nil {
					t.Fatal(err)
				}
				if expired {
					if _, err := observer.Exec(ctx, `UPDATE browser_sessions SET created_at=$2::timestamptz-interval '12 hours',expires_at=$2 WHERE principal_id=$1`, f.caller, deadline); err != nil {
						t.Fatal(err)
					}
				}
				body.unblock()
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				observed := false
				for {
					var blocked bool
					var at time.Time
					if err := observer.QueryRow(ctx, `SELECT clock_timestamp(),EXISTS(SELECT 1 FROM pg_stat_activity
						WHERE datname=current_database() AND state='active' AND wait_event_type='Lock'
						AND query ILIKE $2 AND $1=ANY(pg_blocking_pids(pid)))`, int(conn.PgConn().PID()), waiting).Scan(&at, &blocked); err != nil {
						t.Fatal(err)
					}
					if blocked && at.Before(deadline) {
						observed = true
					}
					if !at.Before(deadline) {
						if !observed || !blocked {
							t.Fatal("did not observe the model limit write waiting across expiry")
						}
						break
					}
					select {
					case <-call.done:
						t.Fatal("model limit request ended before the lock was released")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-tick.C:
					}
				}
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				rr := call.result(t)
				want, marker := http.StatusOK, ""
				if expired {
					want, marker = http.StatusUnauthorized, "1"
				}
				if rr.Code != want || rr.Header().Get("x-platform-signed-out") != marker {
					t.Errorf("model limit wait: status=%d marker=%q want=%d/%q", rr.Code, rr.Header().Get("x-platform-signed-out"), want, marker)
				}
				var rpm *int64
				if err := observer.QueryRow(ctx, `SELECT rpm FROM modelgateway.model_limits WHERE model='fast'`).Scan(&rpm); err != nil {
					t.Fatal(err)
				}
				if expired && (rpm == nil || *rpm != 5) || !expired && rpm != nil {
					t.Errorf("expiry=%v: unexpected persisted RPM %v", expired, rpm)
				}
			})
		}
	}
}
