package admin_test

import (
	"context"

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
