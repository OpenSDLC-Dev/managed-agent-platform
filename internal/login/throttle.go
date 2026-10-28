package login

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Throttle atomically counts both the transport peer (20/5 min) and normalized
// login name (5/5 min) in PostgreSQL. A positive retryAfter means refusal; callers
// return 429 and round it up to whole seconds for Retry-After before hashing.
// Forwarded headers are never a source of transport-peer identity.
func (m *Manager) Throttle(ctx context.Context, r *http.Request, loginName string) (retryAfter time.Duration, err error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return 0, errors.New("login: invalid transport peer address")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return 0, errors.New("login: invalid transport peer address")
	}
	buckets := []struct {
		hash  string
		limit int
	}{
		{hashCredential("login-peer\x00" + addr.Unmap().String()), 20},
		{hashCredential("login-name\x00" + strings.ToLower(strings.TrimSpace(loginName))), 5},
	}
	// Stable lock order prevents two requests sharing different buckets from
	// deadlocking when they update both counters in one transaction.
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].hash < buckets[j].hash })
	tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM login_throttle WHERE window_start <= clock_timestamp()-interval '5 minutes'`); err != nil {
		return 0, err
	}
	for _, bucket := range buckets {
		var attempts int
		var seconds float64
		err := tx.QueryRow(ctx, `
			INSERT INTO login_throttle (bucket_hash,window_start,attempts)
			VALUES ($1,clock_timestamp(),1)
			ON CONFLICT (bucket_hash) DO UPDATE
			SET (window_start,attempts) = (
				SELECT CASE WHEN login_throttle.window_start+interval '5 minutes'<=stamp.at THEN stamp.at ELSE login_throttle.window_start END,
				       CASE WHEN login_throttle.window_start+interval '5 minutes'<=stamp.at THEN 1 ELSE login_throttle.attempts+1 END
				FROM (SELECT clock_timestamp() AS at) stamp
			)
			RETURNING attempts,extract(epoch FROM (window_start+interval '5 minutes'-clock_timestamp()))`, bucket.hash).Scan(&attempts, &seconds)
		if err != nil {
			return 0, err
		}
		if attempts > bucket.limit {
			retry := time.Duration(seconds * float64(time.Second))
			if retry > retryAfter {
				retryAfter = retry
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return retryAfter, nil
}
