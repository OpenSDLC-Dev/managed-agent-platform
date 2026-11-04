package login

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func throttleRequest(peer string) *http.Request {
	r := httptest.NewRequest("POST", "https://console.example/api/auth/login", nil)
	r.RemoteAddr = peer
	return r
}

func TestThrottleIsSharedAndAtomic(t *testing.T) {
	ctx := context.Background()
	pool := loginPool(t)
	otherPool, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherPool.Close)
	managers := []*Manager{New(pool, Config{}), New(otherPool, Config{})}
	for _, tc := range []struct {
		name           string
		count, allowed int
		peer, login    func(int) string
	}{
		{"peer IP", 24, 20, func(int) string { return "192.0.2.50:1234" }, func(i int) string { return fmt.Sprintf("user-%d", i) }},
		{"normalized login", 12, 5, func(i int) string { return fmt.Sprintf("198.51.100.%d:1234", i+1) }, func(i int) string {
			if i%2 == 0 {
				return "  AlIcE  "
			}
			return "alice"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `DELETE FROM login_throttle`); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			results := make(chan time.Duration, tc.count)
			failures := make(chan error, tc.count)
			var wg sync.WaitGroup
			for i := 0; i < tc.count; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					retry, err := managers[i%2].Throttle(ctx, throttleRequest(tc.peer(i)), tc.login(i))
					if err != nil {
						failures <- err
					}
					results <- retry
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			close(failures)
			for err := range failures {
				t.Errorf("concurrent throttle: %v", err)
			}
			allowed := 0
			for retry := range results {
				if retry == 0 {
					allowed++
				} else if retry <= 0 || retry > 5*time.Minute {
					t.Errorf("Retry-After = %s", retry)
				}
			}
			if allowed != tc.allowed {
				t.Fatalf("allowed %d concurrent attempts, want %d", allowed, tc.allowed)
			}
		})
	}
	t.Run("window uses DB time and prunes", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM login_throttle`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if retry, err := managers[0].Throttle(ctx, throttleRequest("203.0.113.10:1234"), "window-user"); err != nil || retry != 0 {
				t.Fatalf("attempt %d: retry %s, err %v", i, retry, err)
			}
		}
		if retry, err := managers[1].Throttle(ctx, throttleRequest("203.0.113.10:9999"), "window-user"); err != nil || retry <= 0 {
			t.Fatalf("blocked attempt: retry %s, err %v", retry, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE login_throttle SET window_start = clock_timestamp()-interval '6 minutes'; INSERT INTO login_throttle (bucket_hash,window_start,attempts) VALUES (repeat('0',64),clock_timestamp()-interval '6 minutes',1)`); err != nil {
			t.Fatal(err)
		}
		if retry, err := managers[1].Throttle(ctx, throttleRequest("203.0.113.10:5555"), "window-user"); err != nil || retry != 0 {
			t.Fatalf("new window blocked: retry %s, err %v", retry, err)
		}
		var count, maximum int
		if err := pool.QueryRow(ctx, `SELECT count(*), max(attempts) FROM login_throttle`).Scan(&count, &maximum); err != nil {
			t.Fatal(err)
		}
		if count != 2 || maximum != 1 {
			t.Fatalf("expired throttle rows not pruned/reset: rows %d, max attempts %d", count, maximum)
		}
	})
	t.Run("forwarded addresses cannot split peer bucket", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM login_throttle`); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 21; i++ {
			r := throttleRequest("192.0.2.100:1234")
			r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
			r.Header.Set("Forwarded", fmt.Sprintf("for=198.51.100.%d", i))
			r.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", i))
			retry, err := managers[i%2].Throttle(ctx, r, fmt.Sprintf("other-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			if (retry == 0) != (i < 20) {
				t.Fatalf("forwarded headers changed peer threshold at attempt %d: %s", i+1, retry)
			}
		}
		var rawRows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM login_throttle WHERE bucket_hash !~ '^[0-9a-f]{64}$'`).Scan(&rawRows); err != nil {
			t.Fatal(err)
		}
		if rawRows != 0 {
			t.Fatal("peer/login values stored instead of SHA-256 bucket keys")
		}
	})
	t.Run("both counters roll back on partial failure", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM login_throttle`); err != nil {
			t.Fatal(err)
		}
		r := throttleRequest("192.0.2.200:1234")
		if _, err := managers[0].Throttle(ctx, r, "rollback-user"); err != nil {
			t.Fatal(err)
		}
		var second string
		if err := pool.QueryRow(ctx, `SELECT bucket_hash FROM login_throttle ORDER BY bucket_hash DESC LIMIT 1`).Scan(&second); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM login_throttle; ALTER TABLE login_throttle ADD CONSTRAINT fixture_reject_second CHECK (bucket_hash <> '`+second+`')`); err != nil {
			t.Fatal(err)
		}
		if _, err := managers[1].Throttle(ctx, r, "rollback-user"); err == nil {
			t.Fatal("injected second-counter failure was ignored")
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM login_throttle`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("first throttle counter committed despite second-counter failure: %d rows", count)
		}
		if _, err := pool.Exec(ctx, `ALTER TABLE login_throttle DROP CONSTRAINT fixture_reject_second`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unparseable peer fails closed", func(t *testing.T) {
		r := throttleRequest("not-an-ip:1234")
		r.Header.Set("X-Forwarded-For", "192.0.2.1")
		if _, err := managers[0].Throttle(ctx, r, "alice"); err == nil {
			t.Fatal("invalid peer fell back to forwarded header")
		}
	})
}
