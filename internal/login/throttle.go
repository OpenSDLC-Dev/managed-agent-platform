package login

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// EdgeClientIPHeader carries the client IP observed by the trusted app-edge
	// reverse proxy (Caddy {http.request.remote.host}).
	EdgeClientIPHeader = "X-Platform-Client-IP"
	// EdgeClientIPTokenHeader authenticates EdgeClientIPHeader as coming from
	// the configured app-edge proxy rather than a direct caller.
	EdgeClientIPTokenHeader = "X-Platform-Edge-Token"
)

// Throttle atomically counts both the client peer (20/5 min) and normalized
// login name (5/5 min) in PostgreSQL. A positive retryAfter means refusal; callers
// return 429 and round it up to whole seconds for Retry-After before hashing.
//
// Generic forwarded headers (X-Forwarded-For, Forwarded, X-Real-IP) are never
// read. EdgeClientIPHeader is trusted only when EdgeClientIPToken is configured
// and the request carries a single matching EdgeClientIPTokenHeader; direct
// requests continue to key on r.RemoteAddr.
func (m *Manager) Throttle(ctx context.Context, r *http.Request, loginName string) (retryAfter time.Duration, err error) {
	addr, err := m.throttlePeer(r)
	if err != nil {
		return 0, err
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
	if _, err := tx.Exec(ctx, `DELETE FROM login_throttle WHERE window_start <= statement_timestamp()-interval '5 minutes'`); err != nil {
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

func (m *Manager) throttlePeer(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, errors.New("login: invalid transport peer address")
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, errors.New("login: invalid transport peer address")
	}
	if m.edgeClientIPToken == "" {
		return peer.Unmap(), nil
	}
	tokens := r.Header.Values(EdgeClientIPTokenHeader)
	if len(tokens) != 1 || subtle.ConstantTimeCompare([]byte(tokens[0]), []byte(m.edgeClientIPToken)) != 1 {
		return peer.Unmap(), nil
	}
	ips := r.Header.Values(EdgeClientIPHeader)
	if len(ips) != 1 {
		return netip.Addr{}, errors.New("login: invalid trusted edge client address")
	}
	clientAddr, err := netip.ParseAddr(strings.TrimSpace(ips[0]))
	if err != nil || clientAddr.Zone() != "" {
		return netip.Addr{}, errors.New("login: invalid trusted edge client address")
	}
	return clientAddr.Unmap(), nil
}
